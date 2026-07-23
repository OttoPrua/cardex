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
	"sync"
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
// 【为什么临界区只覆盖账本读写、不覆盖 inject 长回调】
// resume 侧 inject 会调 invokeClaude/invokeCodex/invokeRemoteClaude 派 LLM 子进程,单次可达数分钟。
// 若持墓碑锁横穿整个 inject, 同卡 CLI 侧的 resetTombstoneKind(retry/release)会被卡到锁 TTL(5s)超
// 后被 stale 强夺,得不偿失。分两阶段:pending 写完即释锁,inject 无锁跑;final 前再取锁重读账本。
//
// 【为什么 final 回写要"entry-gone/nonce-mismatch 即放弃"】
// CG-4 R2 P1-1 审查证伪场景:CLI 侧 resetTombstoneKind(main.go retry/release)与 tick 的
// injectAtMostOnce 首次并发读-改-写同一墓碑账本. 无该防御下,自动化 ops 监听 saveTask(failed) 即
// claudego retry, reset 落在 pending 与 final 之间(inject 内 emitTaskEvent 取事件锁+fsync 隔出数十
// ms 窗口), final 回写重读发现条目已被并发 reset 删除后会走"attempt<newAttempt 分支以 attempt=
// newAttempt 重建条目",把 reset 静默覆盖——retry 承诺的"清墓碑重新起 bound=2 自动再裁决"被作废.
// 视作并发 reset 胜出=不重建 final:业务已完成一次注入, bound 从零起算, 这才是 retry 语义的正确终态.
// nonce 校验是同一防御的第二重: 若 reset 后另一次 injectAtMostOnce 又写了 pending, nonce 与我们本次
// 不同, 我们的 final 不能覆盖别人的 pending(会丢别人的记录).
//
// 【为什么两层锁: sync.Mutex + tmp+os.Link 文件锁】
// 与 events.go acquireEventLock 同源同构缺陷: 文件锁内容里带 PID, 同进程 goroutine pid 相同,
// 不上 mutex 则后来者会读到"自己进程的 PID"→ 误以为是自己持锁 → 破坏"每任务墓碑严格串行"契约.
// 组合: sync.Mutex 挡同进程 goroutine + 文件锁挡跨进程 = 每任务墓碑串行的完整闭环.
func injectAtMostOnce(root, id, kind string, inject func() error) (skipped, corrupted bool, err error) {
	if id == "" || kind == "" {
		// 空 id/kind 是调用侧兜底不该出现,不阻断业务。
		return false, false, nil
	}
	// 阶段 1: 临界区读账本 + 决策 + 写 pending. 锁只覆盖账本读写窗口, 不覆盖 inject 长回调.
	mu := tombstoneLockForTask(id)
	mu.Lock()
	release, lockErr := acquireTombstoneLock(root, id)
	if lockErr != nil {
		mu.Unlock()
		return false, false, lockErr
	}
	journal, corrupted, err := readTombstoneJournal(root, id)
	if err != nil {
		release()
		mu.Unlock()
		return false, false, err
	}
	if corrupted {
		fmt.Fprintf(os.Stderr, "警告: 墓碑损坏 %s(kind=%s),按无墓碑处理(空账本已就位,下次写入自然覆盖)\n", id, kind)
	}
	if entry, ok := journal.Entries[kind]; ok {
		if entry.Phase == tombstonePhaseFinal {
			release()
			mu.Unlock()
			return true, corrupted, nil
		}
		if entry.Attempt >= tombstoneMaxAttempts {
			fmt.Fprintf(os.Stderr, "警告: 墓碑至多一次已耗尽 %s(kind=%s,attempts=%d),不再注入\n", id, kind, entry.Attempt)
			release()
			mu.Unlock()
			return true, corrupted, nil
		}
	}
	prev := journal.Entries[kind]
	newAttempt := prev.Attempt + 1
	now := time.Now()
	pendingNonce := now.UnixNano()
	journal.Entries[kind] = Tombstone{
		Kind:    kind,
		Attempt: newAttempt,
		Phase:   tombstonePhasePending,
		Nonce:   pendingNonce,
		TS:      now.Format(time.RFC3339Nano),
	}
	if writeErr := writeTombstoneJournal(root, id, journal); writeErr != nil {
		release()
		mu.Unlock()
		return false, corrupted, writeErr
	}
	release()
	mu.Unlock()

	// 阶段 2: inject 无锁跑. 若 CLI 侧 resetTombstoneKind 在此窗口执行,它会拿到锁、清掉本条 kind
	// 条目——阶段 3 重读若发现条目已不存在或 nonce 不匹配,则视作 reset 胜出, 放弃 final 重建.
	if injErr := inject(); injErr != nil {
		return false, corrupted, injErr
	}

	// 阶段 3: 再取锁, 认领并升级为 final.
	mu.Lock()
	defer mu.Unlock()
	release2, lockErr := acquireTombstoneLock(root, id)
	if lockErr != nil {
		return false, corrupted, lockErr
	}
	defer release2()
	journal, _, err = readTombstoneJournal(root, id)
	if err != nil {
		return false, corrupted, err
	}
	existing, exists := journal.Entries[kind]
	if !exists {
		// 并发 reset 已删除本 kind 条目——reset 胜出. 详见函数头【为什么 final 回写要"entry-gone"即放弃】.
		return false, corrupted, nil
	}
	if existing.Nonce != pendingNonce {
		// 别人已推进本 kind 到新一轮 attempt (reset+新 injectAtMostOnce 或跨进程后来者):
		// 不覆盖他们的记录, 我们的成功由他们那一轮的 pending/final 承接.
		return false, corrupted, nil
	}
	// 认领: 本 kind 条目正是我们写的 pending, 升级为 final.
	existing.Phase = tombstonePhaseFinal
	existing.Nonce = time.Now().UnixNano()
	existing.TS = time.Now().Format(time.RFC3339Nano)
	journal.Entries[kind] = existing
	if writeErr := writeTombstoneJournal(root, id, journal); writeErr != nil {
		return false, corrupted, writeErr
	}
	return false, corrupted, nil
}

// resetTombstoneKind 清除单一 kind 的墓碑条目。runTask 顶部对 "resume:<step>" 调它——同一步的合法
// 多次重派(如反复撞限额)每次都要在新一轮 bound 上重新计数;详见文件头【为什么 reset-at-entry ...】。
// 其他 kind 不受影响(reconcile:cross 与 resume 无关,不该被误清)。
// 若清后账本为空则删除整个文件,避免空文件残留触发下次读的 corruption 误判(空字节 json.Unmarshal 会失败)。
//
// 【为什么必须与 injectAtMostOnce 走同一把锁】
// 详见 injectAtMostOnce 函数头【为什么 final 回写要"entry-gone/nonce-mismatch 即放弃"】.
// 加锁与 final 回写侧的 entry-gone 判据形成防御纵深: 锁保证 reset 与 pending/final 写不交错,
// nonce 判据保证即便锁语义未来回归, final 也不会静默复活 reset.
func resetTombstoneKind(root, id, kind string) error {
	if id == "" || kind == "" {
		return nil
	}
	mu := tombstoneLockForTask(id)
	mu.Lock()
	defer mu.Unlock()
	release, err := acquireTombstoneLock(root, id)
	if err != nil {
		return err
	}
	defer release()
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
//
// 【为什么必须抢墓碑锁】与 archiveTaskEvents 同源同类缺陷: 不抢锁的裸 os.Rename 与并发
// injectAtMostOnce 竞态——injectAtMostOnce 阶段 3 已重读账本、准备写 final; archive 恰好在此时把
// src 移去 archived; 阶段 3 的 writeTombstoneJournal 会新建一个只有 final 一条的活动墓碑, 与归档文件
// 并列存在, 后续读走 tombstonePath 会看到"注入尝试历史被吞、只剩一条 final"的骗审现场. 加锁保证
// archive 与 injectAtMostOnce/resetTombstoneKind 严格串行.
func archiveTaskTombstones(root, id string) error {
	src := tombstonePath(root, id)
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	mu := tombstoneLockForTask(id)
	mu.Lock()
	defer mu.Unlock()
	release, err := acquireTombstoneLock(root, id)
	if err != nil {
		return err
	}
	defer release()
	// 抢锁期间 src 可能已被其他归档流搬走(clean 与 postComplete 并发 archive 都走这里).
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

// tombstoneMuByTask 是每任务的进程内互斥锁——挡同进程内 goroutine 的并发 injectAtMostOnce/
// resetTombstoneKind/archiveTaskTombstones. 与 events.lockForTask 严格同构:
// 【为什么进程内锁不可省】文件锁内容里带 PID, 同进程 goroutine pid 相同, 若不上 mutex, 后来者
// 会读到"自己进程的 PID"→ 误以为是自己持锁 → 破坏"每任务墓碑严格串行"契约. 组合 sync.Mutex +
// tmp+os.Link 文件锁 = 每任务墓碑串行的完整闭环.
var tombstoneMuByTask sync.Map // map[string]*sync.Mutex

func tombstoneLockForTask(taskID string) *sync.Mutex {
	if v, ok := tombstoneMuByTask.Load(taskID); ok {
		return v.(*sync.Mutex)
	}
	mu := &sync.Mutex{}
	actual, _ := tombstoneMuByTask.LoadOrStore(taskID, mu)
	return actual.(*sync.Mutex)
}

// tombstoneLockPath 每任务一把锁——不用全局锁是因为不同任务的写入无冲突, 全局锁会把所有 CLI/tick
// 串行化, 成本不值.
func tombstoneLockPath(root, taskID string) string {
	return tombstonePath(root, taskID) + ".lock"
}

// acquireTombstoneLock 抢占单任务墓碑写锁——语义与 events.go:acquireEventLock 严格同源:
// tmp 先写完 lockInfo → os.Link 原子挂名 → 若目标已在则按 staleEventLock(mtime>TTL 或 PID 已死)
// 判定强夺; 强夺用 os.Rename 独占, 消除"两方各以为夺权成功"的双持锁.
//
// 【为什么必须存在】详见 injectAtMostOnce 函数头【为什么 final 回写要"entry-gone/nonce-mismatch
// 即放弃"】: CG-4 R1 补的 CLI 侧 resetTombstoneKind 让 CLI 与 runner tick 首次并发读-改-写同一
// 墓碑账本, 无锁下 final 回写会静默复活被并发 reset 删除的条目, 作废本轮承诺的"retry 清墓碑重新起
// bound=2 自动再裁决"契约. 加锁串行化两者是根修.
//
// 【为什么复用 staleEventLock 判据】墓碑锁与事件锁的 stale 语义严格同构: 都是 per-task 文件锁,
// 都用 lockInfo 结构, TTL 都为 5s, 强夺允收边界都是"确定持有者已死"或"锁真的过期". 复用避免
// 重复实现同类判据造成语义漂移.
func acquireTombstoneLock(root, taskID string) (func(), error) {
	if err := os.MkdirAll(tombstonesDir(root), 0o755); err != nil {
		return nil, err
	}
	path := tombstoneLockPath(root, taskID)
	for i := 0; i < 1000; i++ {
		tmp := fmt.Sprintf("%s.acq-%d-%d", path, os.Getpid(), time.Now().UnixNano())
		info, _ := json.Marshal(lockInfo{PID: os.Getpid(), At: time.Now().Format(time.RFC3339)})
		if err := os.WriteFile(tmp, info, 0o644); err != nil {
			return nil, err
		}
		linkErr := os.Link(tmp, path)
		_ = os.Remove(tmp)
		if linkErr == nil {
			return func() { releaseTombstoneLock(path) }, nil
		}
		if !os.IsExist(linkErr) {
			return nil, linkErr
		}
		// 复用 events 锁的 stale 判据: 内容不可解析且 mtime<=TTL 一律等待不强夺, 只在(可解析&&!alive)
		// 或 mtime>TTL 判 stale. 保持防御纵深, 消除"空文件窗口→立即强夺"的 bootstrap 竞态.
		if staleEventLock(path, 5*time.Second) {
			stale := fmt.Sprintf("%s.stale-%d-%d", path, os.Getpid(), time.Now().UnixNano())
			if renameErr := os.Rename(path, stale); renameErr == nil {
				_ = os.Remove(stale)
			}
			continue
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil, fmt.Errorf("墓碑写锁抢占超时: %s", path)
}

// releaseTombstoneLock 只删属于本进程的锁文件: 核 PID 匹配才 Remove.
// 【为什么核 PID】与 releaseEventLock 同源: staleEventLock 对 mtime>TTL 的锁判 stale 强夺, 系统睡眠/
// 挂起跨 TTL 唤醒后原持有者的 defer release 无条件 Remove 会误删强夺者刚建的新锁, 让第三方进入临界
// 区连环撞. 读文件失败/解析失败/PID 不匹配都不删, 让下一轮 staleEventLock 兜底.
func releaseTombstoneLock(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var li lockInfo
	if json.Unmarshal(data, &li) != nil {
		return
	}
	if li.PID != os.Getpid() {
		return
	}
	_ = os.Remove(path)
}
