package main

// tombstones.go —— per-task 幂等墓碑（CG-4）。
//
// 【为什么必须存在】
// 三处"副作用注入"在崩溃/进程重启窗口里可能被反复执行:
//   1) limit_paused 恢复:runTask 顶部若 MidStep=true+SessionID 非空,会向同一 claude 会话发
//      cfg.ResumePrompt("继续。上一条指令因为用量限额被中断...")续跑上一步。
//   2) mid_step 续跑:与 1) 同一代码路径,区别在触发原因是崩溃中断而非限额。
//   3) cross 链 reconcile:tick 每 5 分钟一轮扫孤儿 A/B 卡,若发现"done+无后继"就 saveTask(failed)
//      +emit failed 事件。crash 落在 saveTask 与 emit 之间会漏事件;落在两个 tick 之间会重复裁决。
// 无护栏时,一个"跑到一半崩溃"的任务,tick 每轮都会在其残尾里再撞一次注入——把一个残尾放大成 N 次
// 重发 prompt(烧 token)/N 次 failed 事件(诚实审计流被自己污染)。CG-4 借鉴 AgentBridge 的 per-pending
// 幂等墓碑:每次注入前先写"我准备干这个"的墓碑,注入成功再改成"我干完了"的终稿;tick 见终稿即跳过,
// 见 pending 达上限也跳过——把"至多一次"从注入侧防御拉进到跨进程重启也守得住。
//
// 【为什么单 JSON 而非 JSONL】
// 事件账本(events.go)用 JSONL 因为需要"每次追加原子、崩溃只影响残尾"的语义;墓碑是"当前状态"而非
// "历史流",单 JSON 一次 atomicWrite(tmp→rename)即可原子替换整份账本,不需要行级追加语义。
// 每张任务卡的墓碑条目数少于 6(注入点有限)——JSON 打整份也不吃亏,且 upsert/reset 语义更干净。
//
// 【为什么 bound=2 而非 1】
// 验收明写"崩溃窗口注入 ... mock 计数断言总注入次数 ≤ 2;若无上限,测试报红。"
// bound=1 意味着第一次尝试若崩溃在"prompt 已发送、final 未落盘"处,重启后 pending(attempt=1) 即卡死
// bound,永远不会有第二次尝试——业务永远推不动。bound=2 允许"崩一次+重试一次"共 2 次注入,足以在多数
// 崩溃场景下推进,同时用"至多 2 次"挡住无限循环的崩溃风暴。是"必须重试至少一次"与"不能重试无限次"
// 的最小可行折衷。
//
// 【为什么 reset-at-entry 只在 Status!=running 时触发】
// 同一步(step)可能因合法用量限额被多次挂起→重派→再挂起(sol 档限额撞得频繁时):
//   - 若不 reset,每次重派都在同一 kind 上 +1,第三轮就会因 bound=2 被跳过,合法业务卡死。
//   - 若无脑 reset,崩溃循环中重启就会把 pending 清掉,bound 保护失效——同一崩溃能无限重试。
// 观察点:tick 派 runTask 时,任务盘上状态若是 queued/limit_paused,则本次是"编排层认可的新一轮尝试";
// 若是 running,则是"上一轮 runTask 中途崩溃遗留",不该给它免费重置。以 Status!=running 作为 "fresh
// entry" 判据,恰好卡住这个区别。
//
// 【为什么损坏字节按无墓碑披露而非静默跳过或 crash】
// 验收第三条明写"反例注入:损坏字节 → 按无墓碑处理并日志披露,不 crash、不静默跳步"。
//   - crash:极端不友好,一张卡的墓碑被外部误写就把整个 tick 拉倒。
//   - 静默跳过:等于"因为读不出墓碑,假装它是 final"——反而永远不再注入,业务卡死更隐蔽。
//   - 披露 + 视作无墓碑 + 下次写入自然覆盖:允许业务推进 + 用 stderr 让运维知晓 + 下一次成功 append 后
//     墓碑重归有效状态。这是"错误暴露最大化 + 副作用可控"的诚实姿态。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// tombstoneMaxAttempts 是同一 kind 上允许的"pending→inject"尝试次数上限(含成功后落 final 的那次)。
// 详见文件头【为什么 bound=2】。
const tombstoneMaxAttempts = 2

const (
	tombstonePhasePending = "pending"
	tombstonePhaseFinal   = "final"
)

// Tombstone 是一条注入墓碑。
// Kind 是注入点的标识(如 "resume:3" / "reconcile:cross"),同一 kind 全生命周期共享 bound 计数。
// Attempt 从 1 起,达到 tombstoneMaxAttempts 后禁止再注入(除非 reset)。
// Phase 只有 "pending"(注入前落笔)和 "final"(注入成功后落笔)两态;phase=final 恒挡住后续注入。
// Nonce 单调,取 UnixNano;主要用于人工审计时看清楚"每次尝试的时间戳单调递增",没有业务判据用途。
// TS 是 Nonce 的人可读格式。
type Tombstone struct {
	Kind    string `json:"kind"`
	Attempt int    `json:"attempt"`
	Phase   string `json:"phase"`
	Nonce   int64  `json:"nonce"`
	TS      string `json:"ts"`
}

// tombstoneJournal 是一张卡内所有 kind 墓碑的合集。Version 预留升级钩子。
type tombstoneJournal struct {
	Version int                  `json:"version"`
	Entries map[string]Tombstone `json:"entries"`
}

func tombstonesDir(root string) string         { return filepath.Join(root, "tombstones") }
func archivedTombstonesDir(root string) string { return filepath.Join(root, "archive", "tombstones") }
func tombstonePath(root, id string) string     { return filepath.Join(tombstonesDir(root), id+".json") }
func archivedTombstonePath(root, id string) string {
	return filepath.Join(archivedTombstonesDir(root), id+".json")
}

// resumeKind 返回 limit_paused/mid_step 恢复注入点的 kind。同一步的多次合法重派共享一个 kind
// (由 runTask 顶部的 reset 清空 bound)。
func resumeKind(step int) string { return fmt.Sprintf("resume:%d", step) }

// reconcileCrossKind 返回 cross 链孤儿裁决注入点的 kind。全卡生命周期只此一个。
func reconcileCrossKind() string { return "reconcile:cross" }

// readTombstoneJournal 读并解析墓碑账本。返回:
//   - journal: 解析成功时的实体;文件不存在或损坏时返回空账本(version=1,entries 空 map)。
//   - corrupted: 文件存在但 json.Unmarshal 失败(损坏字节/半截写入),caller 需自行披露。
//   - err: 仅非 NotExist 的 IO 错误。文件不存在不算错(还没写过第一次注入,正常情况)。
func readTombstoneJournal(root, id string) (tombstoneJournal, bool, error) {
	empty := tombstoneJournal{Version: 1, Entries: map[string]Tombstone{}}
	data, err := os.ReadFile(tombstonePath(root, id))
	if err != nil {
		if os.IsNotExist(err) {
			return empty, false, nil
		}
		return empty, false, err
	}
	var parsed tombstoneJournal
	if json.Unmarshal(data, &parsed) != nil {
		// 损坏字节:返回空账本 + corrupted=true,caller 决定披露与后续写入策略。
		// 不在这里 fmt.Fprintf,是让 caller 能带上业务上下文(taskID + kind)一起打日志。
		return empty, true, nil
	}
	if parsed.Entries == nil {
		parsed.Entries = map[string]Tombstone{}
	}
	if parsed.Version == 0 {
		parsed.Version = 1
	}
	return parsed, false, nil
}

// writeTombstoneJournal 原子写整份账本。atomicWrite(tmp→rename)保证 kill -9 在中途最多留一份未 rename
// 的 .tmp 而不污染主账本;若 rename 前崩溃,下次读还是旧账本(旧终稿保留)。
func writeTombstoneJournal(root, id string, j tombstoneJournal) error {
	if err := os.MkdirAll(tombstonesDir(root), 0o755); err != nil {
		return err
	}
	if j.Entries == nil {
		j.Entries = map[string]Tombstone{}
	}
	if j.Version == 0 {
		j.Version = 1
	}
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(tombstonePath(root, id), append(data, '\n'))
}

// injectAtMostOnce 是"至多一次注入"的通用护栏。语义:
//  1. 读账本。若该 kind 已 phase=final → 返回 skipped=true,不调 inject。
//  2. 若该 kind pending 且 Attempt≥bound → 返回 skipped=true(崩溃风暴上限),stderr 警告。
//  3. 否则:写 pending(Attempt+1),调 inject。
//  4. inject 返回 nil → 写 final(标记成功)。inject 返回 err → 保留 pending(留给下次尝试),透传 err。
//
// 返回:
//   - skipped:注入被跳过(已 final 或已耗尽 bound)。caller 据此决定业务行为(如挂 held 升级)。
//   - corrupted:账本文件损坏,已按无墓碑处理并 stderr 披露。caller 可加业务日志。
//   - err:账本 IO 错误或 inject 透传错误。inject 错误时 pending 已落盘,重试次数已+1。
//
// 【为什么 pending 先落盘再 inject,而不是 inject 后再一次性写 final】
// 若 inject 先执行,崩溃在 inject 结束到 pending/final 之间的窗口会让下次重启时"账本看不出这次尝试
// 已发生"→bound 保护形同虚设,同一注入无限重跑。pending 先落盘,即使 inject 未开始就崩溃,重启后账本
// 也知道"曾试过一次",bound 计数是诚实的。
//
// 【为什么 final 写入前要再读一次账本】
// 防御性:虽然本工程单 lock 单进程写墓碑,但如果未来放宽并发(如 cli 命令与 tick 同时改同一张卡的
// 不同 kind),不重读会把并发写入的其他 kind 覆盖成旧值。重读只多一次 read,防坑成本低。
func injectAtMostOnce(root, id, kind string, inject func() error) (skipped, corrupted bool, err error) {
	if id == "" || kind == "" {
		// 空 id/kind 是调用侧兜底不该出现,不阻断业务。
		return false, false, nil
	}
	journal, corrupted, err := readTombstoneJournal(root, id)
	if err != nil {
		return false, false, err
	}
	if corrupted {
		fmt.Fprintf(os.Stderr, "警告: 墓碑损坏 %s(kind=%s),按无墓碑处理(空账本已就位,下次写入自然覆盖)\n", id, kind)
	}
	if entry, ok := journal.Entries[kind]; ok {
		if entry.Phase == tombstonePhaseFinal {
			return true, corrupted, nil
		}
		if entry.Attempt >= tombstoneMaxAttempts {
			fmt.Fprintf(os.Stderr, "警告: 墓碑至多一次已耗尽 %s(kind=%s,attempts=%d),不再注入\n", id, kind, entry.Attempt)
			return true, corrupted, nil
		}
	}
	prev := journal.Entries[kind]
	newAttempt := prev.Attempt + 1
	now := time.Now()
	journal.Entries[kind] = Tombstone{
		Kind:    kind,
		Attempt: newAttempt,
		Phase:   tombstonePhasePending,
		Nonce:   now.UnixNano(),
		TS:      now.Format(time.RFC3339Nano),
	}
	if err := writeTombstoneJournal(root, id, journal); err != nil {
		return false, corrupted, err
	}
	if injErr := inject(); injErr != nil {
		return false, corrupted, injErr
	}
	// 再读一遍,只改本 kind,不覆盖并发写入的其他 kind。详见函数头【为什么 final 写入前要再读一次账本】。
	journal, _, err = readTombstoneJournal(root, id)
	if err != nil {
		return false, corrupted, err
	}
	final := journal.Entries[kind]
	final.Kind = kind
	if final.Attempt < newAttempt {
		final.Attempt = newAttempt
	}
	final.Phase = tombstonePhaseFinal
	final.Nonce = time.Now().UnixNano()
	final.TS = time.Now().Format(time.RFC3339Nano)
	journal.Entries[kind] = final
	if err := writeTombstoneJournal(root, id, journal); err != nil {
		return false, corrupted, err
	}
	return false, corrupted, nil
}

// resetTombstoneKind 清除单一 kind 的墓碑条目。runTask 顶部对 "resume:<step>" 调它——同一步的合法
// 多次重派(如反复撞限额)每次都要在新一轮 bound 上重新计数;详见文件头【为什么 reset-at-entry ...】。
// 其他 kind 不受影响(reconcile:cross 与 resume 无关,不该被误清)。
// 若清后账本为空则删除整个文件,避免空文件残留触发下次读的 corruption 误判(空字节 json.Unmarshal 会失败)。
func resetTombstoneKind(root, id, kind string) error {
	if id == "" || kind == "" {
		return nil
	}
	journal, _, err := readTombstoneJournal(root, id)
	if err != nil {
		return err
	}
	if _, exists := journal.Entries[kind]; !exists {
		return nil
	}
	delete(journal.Entries, kind)
	if len(journal.Entries) == 0 {
		if rmErr := os.Remove(tombstonePath(root, id)); rmErr != nil && !os.IsNotExist(rmErr) {
			return rmErr
		}
		return nil
	}
	return writeTombstoneJournal(root, id, journal)
}

// archiveTaskTombstones 随 archiveTask 把墓碑账本移到 archive/tombstones/。事件账本已在 archiveTaskEvents
// 做同样的事——两者形成"卡+事件+墓碑"三件套的完整归档,让审计后能拿到"这张卡从生到死每一次注入都试过几次"的全景。
// 文件不存在(旧卡从未触发过注入)不算错。
func archiveTaskTombstones(root, id string) error {
	src := tombstonePath(root, id)
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(archivedTombstonesDir(root), 0o755); err != nil {
		return err
	}
	return os.Rename(src, archivedTombstonePath(root, id))
}
