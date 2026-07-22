package main

// events.go —— per-task 事件账本（CG-2）。
//
// 【为什么必须存在】
// 看板此前的活动流是拿 task.Status 反推的一句话——同一张 UpdatedAt 覆盖前只留最后一次状态。
// 于是"这张卡三分钟前是 running，两分钟前撞了 limit_paused，一分钟前又续回 running"这类
// 真实历史在盘上被压平成"当前是 running"，与看板宣称的"诚实性第一"直接冲突。事件账本把每次
// 状态迁移**原子追加**到 events.jsonl，看板改读事件流后有了真历史；且事件缺口显式标注，绝不
// 靠状态反推伪造"完整历史"。
//
// 【为什么用 O_APPEND + fsync】
// JSONL 追加是 append-only 语义的最小承载：POSIX 下 O_APPEND 保证 write 的定位与写入是同一
// 原子操作，kill -9 落到写中途最多留一行"半截 JSON"残尾——读的时候按行 json.Unmarshal 失败
// 就丢弃，已落的历史事件不受影响。每条事件都 Sync：事件是审计凭证，进程崩溃不能丢已"宣称写下"
// 的事件（一次 fsync 成本可忽略，事件不密集）。
//
// 【为什么 seq 用运行时算而非维护外部计数器】
// 外部计数器要额外的写点和锁，与"不新增写者"矛盾；每次 append 前扫一遍现有事件算下一个 seq，
// 卡内事件不会上千，成本可控——简单胜过优化。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// TaskEvent 是一条状态迁移事件。
// Seq: 卡内单调递增，用于事件缺口检测（读者见 seq 跳号即在活动流插入"事件缺口"）。
// Type: 见下方枚举——严格与状态机迁移一一对应。
// Actor: 谁触发的（cli:add / cli:release / runner / runner:review 等）；崩溃后能溯源。
// Status/Step: 迁移后的状态快照（便于事件流阅读时快速理解上下文）。
// Detail: 迁移特定的附加信息（限额恢复时间、重试次数、错误摘要、下游派生的卡 ID 等）。
type TaskEvent struct {
	Seq    int64          `json:"seq"`
	TS     string         `json:"ts"`
	Type   string         `json:"type"`
	Actor  string         `json:"actor,omitempty"`
	Status string         `json:"status,omitempty"`
	Step   int            `json:"step,omitempty"`
	Detail map[string]any `json:"detail,omitempty"`
}

// 事件类型枚举——必须与验收清单"入队/派发/步成功/limit_paused/held/retry/canceled/终态/closeout"逐一对应。
const (
	evQueued      = "queued"       // 首次入队（新卡落盘），或 held→queued（release）
	evDispatched  = "dispatched"   // tick 派上跑（进入 running）
	evStepOK      = "step_ok"      // 单步执行成功（Step 推进）
	evLimitPaused = "limit_paused" // 撞限额挂起（claude / 本机 codex / 远端账号 三种）
	evHeld        = "held"         // 挂起（人工 / 超轮限升级 / emit 挂起）
	evRetry       = "retry"        // 退回排队重试（错误后 backoff）
	evCanceled    = "canceled"     // 取消
	evDone        = "done"         // 完成
	evFailed      = "failed"       // 失败（超 attempts / 交叉链断裂 / C 契约违规 / reconcile 判孤儿）
	evCloseout    = "closeout"     // 完成后派生动作（下游审核/修复/交叉B或C/emit/收口）已入队
)

// 缺口标记：不是被写入的事件，而是活动流读者见 seq 跳号时插入的显式披露。
// 保留在事件类型枚举里的原因：让活动流序列化字段一致（前端拿到 Type=event_gap 明确渲染红条）。
const evGap = "event_gap"

func eventsDir(root string) string            { return filepath.Join(root, "events") }
func archivedEventsDir(root string) string    { return filepath.Join(root, "archive", "events") }
func eventsPath(root, id string) string       { return filepath.Join(eventsDir(root), id+".jsonl") }
func archivedEventsPath(root, id string) string {
	return filepath.Join(archivedEventsDir(root), id+".jsonl")
}

// eventsPathAnywhere 返回该任务的事件流位置（活动/归档二处，任一存在优先返回）。
// 活动流可能读到已被 clean 归档的卡——保留 archive/events/ 检索路径以免历史断线。
func eventsPathAnywhere(root, id string) string {
	live := eventsPath(root, id)
	if _, err := os.Stat(live); err == nil {
		return live
	}
	return archivedEventsPath(root, id)
}

// eventMuByTask 是每任务的进程内互斥锁——挡同进程内 goroutine 的并发 emit。
// 【为什么进程内锁不可省】staleLock 有 bootstrap 竞态:文件锁"创建 lockfile"与"写 PID"两步之间,
// 若第二个 goroutine 在 A 写 PID 前读 lockfile 得空内容,Unmarshal 失败被判 stale 强夺——两个
// goroutine 同时进入临界区,seq 计算错位撞车。文件锁只挡跨进程,进程内的强序由 sync.Mutex 兜底,
// 组合起来才既挡跨进程又挡同进程 goroutine,seq 单调契约才不漏。
var eventMuByTask sync.Map // map[string]*sync.Mutex

func lockForTask(taskID string) *sync.Mutex {
	if v, ok := eventMuByTask.Load(taskID); ok {
		return v.(*sync.Mutex)
	}
	mu := &sync.Mutex{}
	actual, _ := eventMuByTask.LoadOrStore(taskID, mu)
	return actual.(*sync.Mutex)
}

// recordEvent 追加一条事件到该任务的 events.jsonl。
// 【纪律】写入路径必须与状态机的 saveTask 一一配对：状态机迁移后立即落一条事件。
// 若追加失败只打警告不阻断状态机——事件账本是"审计凭证"层，不该反向卡死主流程。
//
// 【为什么 nextSeq+append+fsync 必须放在跨进程 + 进程内双层锁下】state.go 的调度实例 .lock 只挡
// "多个 runner 同时开跑"，不挡 cli:cancel / cli:release / cli:hold 与 runner 并发写同一卡的事件。
// 若不加锁,两个写者各读到 max=N、各写 seq=N+1——两条事件同 seq,"卡内 seq 单调递增"契约破,
// 重复 seq 中删一条事件也不可检测(seq 序列仍显完整),"事件缺口显式披露"这道墙被绕过。O_APPEND
// 只保证 write 定位原子,不解决 read-compute-write 的组合竞态。用 O_EXCL 短自旋锁挡跨进程,再叠
// 一层进程内 sync.Mutex 挡 staleLock bootstrap 竞态(空 lockfile 被 unmarshal 判 stale 强夺)。
func recordEvent(root, taskID string, ev TaskEvent) error {
	if taskID == "" {
		return nil // 任务尚未分配 ID：极少见的兜底，静默跳过
	}
	if err := os.MkdirAll(eventsDir(root), 0o755); err != nil {
		return err
	}
	// 进程内锁在外层:保证同进程 goroutine 严格串行,不触发文件锁的 bootstrap 竞态。
	mu := lockForTask(taskID)
	mu.Lock()
	defer mu.Unlock()
	// 跨进程写锁：包 nextSeq+append+fsync 三步。锁文件位于 events/<id>.jsonl.lock，
	// TTL 短(5s)——单次追加几个 ms，若持锁进程死亡按陈旧锁强夺（processAlive 检活）。
	release, err := acquireEventLock(root, taskID)
	if err != nil {
		return err
	}
	defer release()
	// 定 seq：读现有事件算下一个。iterEvents 已过滤崩溃残尾，seq 计数不会被半截行污染。
	seq, err := nextSeq(root, taskID)
	if err != nil {
		return err
	}
	ev.Seq = seq
	if ev.TS == "" {
		// 用 RFC3339Nano 是为了让同一秒内多次迁移仍能按时间排序（如 dispatched→limit_paused
		// 可能在同一秒发生）；避免看板活动流把两条挤乱序。
		ev.TS = time.Now().Format(time.RFC3339Nano)
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	// O_APPEND：POSIX 保证追加定位与写入是同一原子操作；进程内多次调用互不截断——写者只会互相
	// 排后面，绝不会写进对方的字节中间。跨进程"读-算-写"竞态由上面的 acquireEventLock 挡住。
	// O_RDWR 而非 O_WRONLY：ensureTrailingNewline 要 ReadAt 检末字节；write-only 描述符 ReadAt 报
	// "bad file descriptor"（macOS/BSD 尤为严格），会让首次写入之后每次追加都失败。
	f, err := os.OpenFile(eventsPath(root, taskID), os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	// 防"上次崩溃残尾直接接尾"：若末尾不是 \n，先补一个 \n 把半截行封成独立坏行——
	// 读者按行 Unmarshal 时把它当损坏行丢弃，新事件保持独立成行，不会被"粘"进坏行。
	if err := ensureTrailingNewline(f); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	// fsync：事件是审计凭证，宣称已写就必须持久化。少量额外延迟换绝不丢事件的语义保障。
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// eventLockPath 每任务一把锁——不用全局锁是因为不同任务的写入互不干扰，全局锁会把 CLI 多命令
// 组合与 runner tick 串行化，成本不值。
func eventLockPath(root, taskID string) string {
	return eventsPath(root, taskID) + ".lock"
}

// acquireEventLock 用 O_CREATE|O_EXCL 抢占单任务事件写锁；持锁进程已死或锁超龄视为陈旧强夺。
// 复用 state.go:acquireLock 的语义但作用域为单任务：写事件的临界区极短(几毫秒)，TTL 5s 足够，
// 极端崩溃(kill -9 于 nextSeq..fsync 之间)按陈旧锁清除，不会永久卡死后续写入。
func acquireEventLock(root, taskID string) (func(), error) {
	path := eventLockPath(root, taskID)
	// 自旋：短临界区场景下自旋比 fsnotify/inotify 简单可靠。最多等 5s（1000×5ms），
	// 覆盖单个写者的完整临界区仍抢不到就报错让 emitTaskEvent 打警告——绝不静默漏事件。
	for i := 0; i < 1000; i++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			data, _ := json.Marshal(lockInfo{PID: os.Getpid(), At: time.Now().Format(time.RFC3339)})
			_, _ = f.Write(data)
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		if staleLock(path, 5*time.Second) {
			_ = os.Remove(path)
			continue
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil, fmt.Errorf("事件写锁抢占超时: %s", path)
}

// ensureTrailingNewline 若文件末尾不是 \n 就补一个。为空文件是 no-op。
// 【教训】早期原型没做这步：上一次崩溃残尾"{\"seq\":3,\"ty" 直接接下一条 "{\"seq\":4,...}\n"
// 会拼成 "{\"seq\":3,\"ty{\"seq\":4,...}\n" 一整行——读者 Unmarshal 失败，第四条事件被残尾"吞"。
// 补一个换行把残尾封成独立坏行，坏行只被丢弃，第四条完整独立。
func ensureTrailingNewline(f *os.File) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() == 0 {
		return nil
	}
	buf := make([]byte, 1)
	if _, err := f.ReadAt(buf, st.Size()-1); err != nil {
		return err
	}
	if buf[0] == '\n' {
		return nil
	}
	_, err = f.Write([]byte{'\n'})
	return err
}

// nextSeq 读该任务的事件流算下一个 seq。文件不存在返回 1。
func nextSeq(root, taskID string) (int64, error) {
	events, hadCorruption, err := readEvents(eventsPath(root, taskID))
	if err != nil {
		if os.IsNotExist(err) {
			return 1, nil
		}
		return 0, err
	}
	_ = hadCorruption // seq 不受损坏行影响：只看合法事件里的最大 seq。
	var max int64
	for _, ev := range events {
		if ev.Seq > max {
			max = ev.Seq
		}
	}
	return max + 1, nil
}

// readEvents 按行解析事件流。返回 (合法事件, 是否遇到崩溃残尾/损坏行, err)。
// 【为什么坏行只丢弃不修复】损坏行是崩溃留下的"半截 JSON"，不含可信信息；试图从半截推断
// 完整事件等于伪造历史，与"事件缺口显式标注"纪律直接冲突。丢弃 + 缺口披露是唯一诚实做法。
func readEvents(path string) ([]TaskEvent, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	// bufio.Scanner 默认 64KB 单行上限对事件流够用（单条事件极少超 4KB）。
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	out := make([]TaskEvent, 0, 32)
	hadCorruption := false
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev TaskEvent
		if json.Unmarshal(line, &ev) != nil {
			hadCorruption = true
			continue
		}
		if ev.Seq <= 0 || ev.Type == "" {
			// 必填字段缺失也算损坏（历史迁移期兼容：若真有旧格式无 seq 事件，改这里的策略）。
			hadCorruption = true
			continue
		}
		out = append(out, ev)
	}
	if err := scanner.Err(); err != nil {
		return out, hadCorruption, err
	}
	return out, hadCorruption, nil
}

// loadTaskEvents 是"从任一位置（活动/归档）读事件"的对外入口。文件不存在返回空列表且无错——
// 事件账本尚未生成的旧卡也能被活动流正确忽略（活动流禁止用状态反推补齐）。
func loadTaskEvents(root, taskID string) ([]TaskEvent, bool, error) {
	events, hadCorruption, err := readEvents(eventsPathAnywhere(root, taskID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return events, hadCorruption, nil
}

// archiveTaskEvents 随 archiveTask 把事件账本移到 archive/events/。事件账本不存在（旧卡/无事件）
// 不算错——archive 是"随卡"归档，卡有事件才有账本，反之空账本无需强行创建。
func archiveTaskEvents(root, taskID string) error {
	src := eventsPath(root, taskID)
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(archivedEventsDir(root), 0o755); err != nil {
		return err
	}
	return os.Rename(src, archivedEventsPath(root, taskID))
}

// emitTaskEvent 是 recordEvent 的便捷封装：状态机侧点用它记录事件，写入失败只打警告不阻断。
// 【为什么失败不阻断】事件账本是审计凭证层，绝不能反向让 saveTask 失败卡死主流程；出错走
// stderr 提示由 launchd 日志收拢——事件缺口在活动流里由 seq 检测自动可见。
func emitTaskEvent(root, taskID, evType, actor, status string, step int, detail map[string]any) {
	if taskID == "" || root == "" {
		return
	}
	ev := TaskEvent{Type: evType, Actor: actor, Status: status, Step: step, Detail: detail}
	if err := recordEvent(root, taskID, ev); err != nil {
		fmt.Fprintf(os.Stderr, "警告: 事件账本写入失败 %s(%s): %v\n", taskID, evType, err)
	}
}
