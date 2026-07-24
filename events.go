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
	"sort"
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
	// CG-5:巡逻先记事件再击杀。evStalled 是"披露判定卡死于何时因何触发"的诊断事件,不代表状态迁移
	// (状态仍是 running);随后的 evCanceled(由 finalizeCanceled 落)才是"被巡逻杀了"的真状态迁移。
	// 两条事件的时间戳与 detail 组合出完整因果链:dispatched→stalled(诊断)→canceled(收尾)。
	evStalled = "stalled"
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

// eventMuByTask 是每任务的进程内互斥锁——挡同进程内 goroutine 的并发 emit。
// 【为什么进程内锁不可省】即便 acquireEventLock 已用 tmp+os.Link 消除跨进程 bootstrap 竞态,
// 文件锁仍属"进程级"资源(内容里带 PID), 同进程内多个 goroutine 的 pid 相同, 若不上 mutex,
// 后来者会读到"自己进程的 PID"→ 误以为是自己持锁 → 破坏"每任务事件写严格串行"契约.
// 组合:sync.Mutex 挡同进程 goroutine + tmp+Link 挡跨进程 = seq 单调递增无缝隙.
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
// 只保证 write 定位原子,不解决 read-compute-write 的组合竞态。tmp+os.Link 原子挂名的文件锁挡跨
// 进程, 内层 sync.Mutex 挡同进程 goroutine——两层缺一不可: 缺文件锁则 CLI 与 daemon 撞 seq,
// 缺 mutex 则同进程 goroutine 因共享 pid 会误信自己是持锁者.
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
	// 写路径由 pickEventsWritePath 决定:归档后补写走归档文件, 保持写点唯一, 让历史不断线.
	writePath := pickEventsWritePath(root, taskID)
	// 若目标是归档文件, 确保归档目录存在(recordEvent 头部只 MkdirAll 了活动目录).
	if writePath == archivedEventsPath(root, taskID) {
		if err := os.MkdirAll(archivedEventsDir(root), 0o755); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(writePath, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o644)
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

// acquireEventLock 抢占单任务事件写锁；持锁进程已死或锁超龄视为陈旧强夺。
// 【为什么用 tmp+os.Link 原子挂名而非 O_CREATE|O_EXCL 直接建空文件后写 PID】
// 旧法两步不原子:另一进程在"文件已建但 PID 未写"的空文件窗口读到空内容, staleLock(state.go:79)
// Unmarshal 失败即刻判 stale 强夺 → Remove 掉本进程正在写的锁 → 双持锁 seq 撞车. tmp 里先写完
// lockInfo, 再 os.Link(tmp,path) 把带完整内容的 inode 原子挂到目标名字上——path 一旦存在,
// 内容就是完整的,再也没有空文件窗口. 强夺权用 os.Rename(path→path.stale-*) 独占, 让多方竞
// 争强夺只能一方成功, 消除"Remove→create 交错双持锁"的第二重放大器.
// 释放前核 PID:TTL 强夺后原持有者的 defer release 无条件 Remove 会误删强夺者的新锁, 让第三写
// 者进入临界区连环撞 seq——releaseEventLock 读锁文件核 PID==自身才删.
func acquireEventLock(root, taskID string) (func(), error) {
	path := eventLockPath(root, taskID)
	// 自旋：短临界区场景下自旋比 fsnotify/inotify 简单可靠。最多等 5s（1000×5ms），
	// 覆盖单个写者的完整临界区仍抢不到就报错让 emitTaskEvent 打警告——绝不静默漏事件。
	for i := 0; i < 1000; i++ {
		// tmp 名带 pid+纳秒时钟避免同进程/跨进程重名; 与 path 同目录以让 os.Link 跨文件系统亦可
		// (POSIX 硬链接要求同 mount, 同目录必然满足).
		tmp := fmt.Sprintf("%s.acq-%d-%d", path, os.Getpid(), time.Now().UnixNano())
		info, _ := json.Marshal(lockInfo{PID: os.Getpid(), At: time.Now().Format(time.RFC3339)})
		if err := os.WriteFile(tmp, info, 0o644); err != nil {
			return nil, err
		}
		linkErr := os.Link(tmp, path)
		// tmp 无论 Link 成败都得清:成功后 tmp 已是 path 的第二个硬链接, 保留会让 release 侧无法
		// 从 path 名字断言 inode 独占; 失败保留 tmp 是漏文件.
		_ = os.Remove(tmp)
		if linkErr == nil {
			return func() { releaseEventLock(path) }, nil
		}
		if !os.IsExist(linkErr) {
			return nil, linkErr
		}
		// path 已被别人占. 用本地 staleEventLock:除 state.go:staleLock 的 mtime/PID 判据外多一条
		// "内容不可解析且 mtime<1s 不判 stale"的短暂空窗豁免——防"另一进程刚 WriteFile(tmp) 未及
		// Link"这种极窄窗口内被误判为 stale. 保持防御纵深, 即便日后有人把 tmp+Link 改回 O_EXCL,
		// 空文件窗口也不会被强夺.
		if staleEventLock(path, 5*time.Second) {
			// 强夺唯一化:os.Rename 是 POSIX 原子操作, path 只能被一方成功搬走. 失败者(路径已被他人
			// 搬走)本轮 sleep 后自然重试, 不会再 Remove 一次导致"两方各以为夺权成功".
			// 【CG-R1 R3 P2-2 收窄】staleEventLock 到 Rename 之间, path 可能被他人 Link 新鲜锁
			// (B 过 staleEventLock 判据后停顿至 A 完成 Rename+Link, B 的 Rename 会搬走 A 的新
			// 鲜锁双持). 核 stale 内容: 存活异 PID 即 os.Link 归还目标路径, 本轮 sleep+continue
			// 让 A 的锁不被误删。见 state.go:isForeignLiveLock 与 acquireLock 同类闭合。
			stale := fmt.Sprintf("%s.stale-%d-%d", path, os.Getpid(), time.Now().UnixNano())
			if renameErr := os.Rename(path, stale); renameErr == nil {
				if isForeignLiveLock(stale) {
					_ = os.Link(stale, path)
					_ = os.Remove(stale)
					time.Sleep(5 * time.Millisecond)
					continue
				}
				_ = os.Remove(stale)
			}
			continue
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil, fmt.Errorf("事件写锁抢占超时: %s", path)
}

// releaseEventLock 只删属于本进程的锁文件:核 PID 匹配才 Remove.
// 【为什么核 PID】staleEventLock(与 state.go:staleLock)对 mtime>5s 的锁直接判 stale 强夺——
// 系统睡眠/挂起跨 5s 唤醒后, 原持有者的 defer release 无条件 Remove 会删掉强夺者刚建的新锁,
// 让第三写者进临界区连环撞 seq. 读文件失败/解析失败/PID 不匹配都不删——留给真正的持有者
// (若真是我们的锁, Unmarshal 必成; 若不是, 让下一轮 staleEventLock 兜底).
//
// 【CG-R1 R3 P2-2 收窄:核 PID→Remove 间隙的 TOCTOU】ReadFile 到 Remove 之间, 他人若判 stale
// 强夺 (Rename+Link 新锁), 我们无脑 Remove(path) 会误删他们的新锁。改用 os.Rename 独占搬走
// 再核内容: 属自身 PID 才 Remove; 内容变了(存活异 PID) 则 Link 归还, 让归属复原。
// 与 state.go:releaseLock、tombstones.go:releaseTombstoneLock 同类闭合。
func releaseEventLock(path string) {
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
	stale := fmt.Sprintf("%s.rel-%d-%d", path, os.Getpid(), time.Now().UnixNano())
	if err := os.Rename(path, stale); err != nil {
		return
	}
	if isForeignLiveLock(stale) {
		_ = os.Link(stale, path)
	}
	_ = os.Remove(stale)
}

// staleEventLock 是事件锁本地的更保守判据:严格执行"仅当(可解析&&!processAlive)或 mtime>TTL 才判 stale".
// 【为什么内容空/不可解析且 mtime 未超 TTL 一律不判 stale】审查(CG-2 R2 concerns P1-1):即便 tmp+os.Link
// 已消除空文件窗口, 仍必须在 TTL 全窗口豁免"内容尚不完整"的锁——理由有二:
//   (1) 防御纵深:未来维护若把 tmp+Link 改回 O_EXCL, 空文件窗口重现, 只要 mtime<=TTL 就不强夺,
//       就能挡"读到空内容→立即强夺"的 bootstrap 竞态回归; 1s 阈值太窄, GC/swap/系统忙时
//       合法写者可能超 1s 才补上内容, 被误判 stale;
//   (2) 强夺允收边界收窄至"确定持有者已死"或"锁真的过期"两条:内容可解析但 PID 进程不在,
//       是明确的进程已死; mtime>TTL 是"任何持有者都不可能占这么久"的硬边界. 两条之外
//       (含内容尚不完整但 mtime<=TTL 的所有情况)一律等待, 不强夺.
// 反例注入:把此处的"mtime<=TTL 不 stale"改回 1s 阈值或直接判 stale,
// TestAcquireEventLockWaitsOnFreshEmptyLock 会报红——预置空锁+新鲜 mtime 场景会被误判强夺.
func staleEventLock(path string, ttl time.Duration) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return true
	}
	if time.Since(fi.ModTime()) > ttl {
		return true
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var li lockInfo
	if json.Unmarshal(data, &li) != nil || li.PID <= 0 {
		// 内容空/不可解析但 mtime 未超 TTL:视为合法写者尚未收尾的短暂空窗,一律等待不强夺.
		// 达到 TTL 仍不可解析才判 stale (走上面 mtime>ttl 分支, 这里不重复判).
		return false
	}
	return !processAlive(li.PID)
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

// nextSeq 读该任务事件流算下一个 seq. 活动+归档两处都扫,取合法事件最大 seq——
// 【为什么必须双读】archiveTaskEvents 会把 events.jsonl 搬去 archive/events/. 若只看活动路径,
// clean 与 postComplete 并发时(runReviewSync 最长 120s 的宽窗口内, closeout 补写在归档搬完后
// 发生), nextSeq 见活动路径不存在 → 返回 1 → 新建 seq=1 起算的活动文件. 完整历史被隐匿且
// 绕过 board.go:390-393 的头部缺口守卫(seq 从 1 起算, events[0].Seq>1 条件不满足). 双读取
// max 让归档后的补写继承旧 seq 序号, 缺口披露与"每状态迁移恰一条事件"验收在归档并发下仍成立.
// 文件不存在(两处都无)返回 1——首次入队场景.
func nextSeq(root, taskID string) (int64, error) {
	var maxSeq int64
	for _, path := range []string{eventsPath(root, taskID), archivedEventsPath(root, taskID)} {
		events, _, err := readEvents(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return 0, err
		}
		// 损坏行不影响 seq 推算:readEvents 已丢弃, 只看合法事件里的最大 seq.
		for _, ev := range events {
			if ev.Seq > maxSeq {
				maxSeq = ev.Seq
			}
		}
	}
	return maxSeq + 1, nil
}

// pickEventsWritePath 决定 recordEvent 应把新事件追加到哪个文件.
// 【为什么归档后写归档】archiveTaskEvents 已把活动搬去归档, 若 recordEvent 见活动缺失就新建
// seq=1 起算的活动, 会造成"两个文件, 各自 seq 从 1 起, 历史断线". 保持写点唯一 → 归档独在
// 时追加归档; 活动独在或两者并存时优先写活动(与既有 emit 流一致, 让活动 → 归档的归档流可控).
func pickEventsWritePath(root, taskID string) string {
	live := eventsPath(root, taskID)
	if _, err := os.Stat(live); err == nil {
		return live
	}
	arch := archivedEventsPath(root, taskID)
	if _, err := os.Stat(arch); err == nil {
		return arch
	}
	return live
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

// loadTaskEvents 是"从活动+归档合并读事件"的对外入口. 文件不存在返回空列表且无错——
// 事件账本尚未生成的旧卡也能被活动流正确忽略(活动流禁止用状态反推补齐).
// 【为什么合并双读】即便 pickEventsWritePath 已收敛写点(归档独在时写归档), 极窄迁移窗口内
// 仍可能两处都有事件(如 archive 前后夹了 emit). 合并 + 按 seq 去重, 让读侧看到完整历史,
// 避免"看板只见一处、seq 断线冒充完整"的死角. 单侧存在的常规场景仍返回该侧原样(不排序保序).
func loadTaskEvents(root, taskID string) ([]TaskEvent, bool, error) {
	live := eventsPath(root, taskID)
	arch := archivedEventsPath(root, taskID)
	liveEvents, liveCorrupt, liveErr := readEvents(live)
	if liveErr != nil && !os.IsNotExist(liveErr) {
		return nil, false, liveErr
	}
	archEvents, archCorrupt, archErr := readEvents(arch)
	if archErr != nil && !os.IsNotExist(archErr) {
		return nil, false, archErr
	}
	liveMissing := os.IsNotExist(liveErr)
	archMissing := os.IsNotExist(archErr)
	if liveMissing && archMissing {
		return nil, false, nil
	}
	if liveMissing {
		return archEvents, archCorrupt, nil
	}
	if archMissing {
		return liveEvents, liveCorrupt, nil
	}
	// 两处并存的极少见迁移窗口:按 seq 合并去重, 再按 seq 升序排,让读侧仍看到完整单调历史.
	merged := mergeEventsBySeq(archEvents, liveEvents)
	return merged, liveCorrupt || archCorrupt, nil
}

// mergeEventsBySeq 按 seq 合并两份事件流并去重, 结果按 seq 升序.
// 冲突时(同 seq 两处都有)保留归档侧——归档更接近"历史底本", 活动侧更可能是补写的近事件.
func mergeEventsBySeq(archEvents, liveEvents []TaskEvent) []TaskEvent {
	seen := make(map[int64]bool, len(archEvents)+len(liveEvents))
	out := make([]TaskEvent, 0, len(archEvents)+len(liveEvents))
	for _, ev := range archEvents {
		if seen[ev.Seq] {
			continue
		}
		seen[ev.Seq] = true
		out = append(out, ev)
	}
	for _, ev := range liveEvents {
		if seen[ev.Seq] {
			continue
		}
		seen[ev.Seq] = true
		out = append(out, ev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// archiveTaskEvents 随 archiveTask 把事件账本移到 archive/events/. 事件账本不存在(旧卡/无事件)
// 不算错——archive 是"随卡"归档, 卡有事件才有账本, 反之空账本无需强行创建.
// 【为什么必须抢事件锁】不抢锁的裸 os.Rename 与并发 recordEvent 竞态: recordEvent 已进临界区
// 读到 events 内容、计出 nextSeq, archive 恰好在此时 rename src → archived. recordEvent 的
// OpenFile(O_APPEND|O_CREATE) 会新建一个 seq 从 next 开始的活动文件, 造成"活动文件突然只有
// 若干后续事件, 历史整体隐匿"——头部缺口守卫失效. 用事件锁包住 rename, 让 recordEvent 要么
// 在 rename 前完成、要么在 rename 后看到"活动不存在但归档存在" → 走 pickEventsWritePath 追
// 加到归档文件, 保持写点唯一, 历史不断线.
func archiveTaskEvents(root, taskID string) error {
	src := eventsPath(root, taskID)
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	release, err := acquireEventLock(root, taskID)
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
	if err := os.MkdirAll(archivedEventsDir(root), 0o755); err != nil {
		return err
	}
	// 若归档路径已存在(极少见, 同 ID 归档文件曾被写过): 必须合并 src+dst 到 dst 并删除 src.
	// 【为什么必须合并而非跳过】跳过策略会让 src 遗留在活动路径 → 后续 emit 走 pickEventsWritePath
	// 见活动存在则继续写活动, 形成"活动/归档两处长期并存"的迁移态:
	//   (1) loadTaskEvents 合并读能兜住展示层, 但把 seq 冲突消解的复杂性推给读者;
	//   (2) 归档流的语义是"卡已到终态, 后续不再写", 保留活动路径违背意图;
	//   (3) 万一读者只走单一路径(如未来某个 CLI 工具直接读 archive/), 会漏活动侧事件.
	// 合并策略:读双侧事件按 seq 合并去重, atomicWrite 覆盖 dst, 删除 src——写点收敛回单一归档.
	// 覆盖旧归档不算"毁历史"因为合并已保留其全部事件(mergeEventsBySeq 冲突时保留归档侧).
	dst := archivedEventsPath(root, taskID)
	if _, err := os.Stat(dst); err == nil {
		return mergeIntoArchivedEvents(src, dst)
	}
	return os.Rename(src, dst)
}

// mergeIntoArchivedEvents 把 src 的事件合并进已存在的 dst, 然后删除 src.
// 【为什么先合并再删】不用简单 append:src/dst 各自可能有 seq 相同的事件(如极端情况下的双写残留),
// mergeEventsBySeq 会去重, append 会重复 seq 破坏"卡内 seq 单调递增"契约. 用 atomicWrite 保证
// 崩溃安全:合并写 tmp+Rename 挂 dst, 崩溃在合并中间不会污染旧 dst; 崩溃在 Remove(src) 前不会
// 丢事件(src 仍在, 下一次 archive 会再走一次合并——去重把重复消掉).
func mergeIntoArchivedEvents(src, dst string) error {
	srcEvents, _, srcErr := readEvents(src)
	if srcErr != nil && !os.IsNotExist(srcErr) {
		return srcErr
	}
	dstEvents, _, dstErr := readEvents(dst)
	if dstErr != nil && !os.IsNotExist(dstErr) {
		return dstErr
	}
	merged := mergeEventsBySeq(dstEvents, srcEvents)
	var buf bytes.Buffer
	for _, ev := range merged {
		data, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	if err := atomicWrite(dst, buf.Bytes()); err != nil {
		return err
	}
	return os.Remove(src)
}

// hasCanceledEvent 扫账本判断是否已有 evCanceled 事件——取消去重的**唯一**可靠真相源.
// 【为什么不用 diskCanceled】cmdSetStatus 的走位是"先 saveTask(cancel) 再 emit(canceled)":
// 若在两步之间崩溃/断电, 或 emit 因 best-effort 失败(锁超时/磁盘满), 盘上 status=canceled
// 但账本无事件. 用磁盘态代理"账本已有事件"会让 finalizeCanceled 与 runTask 入口守卫都跳过
// emit——取消事件永久缺失且无 seq 缺口可见(0 条与 2 条同样违背"恰一条", 但 0 条不可检测更
// 糟, 是对旧代码保证≥1 条的直接回归). 用账本本身作为去重键:盘上 canceled 但账本无事件时
// 必须补一条(detail 标 reason=backfill 溯源, 让审计能区分正规取消与补写).
func hasCanceledEvent(root, taskID string) bool {
	if taskID == "" {
		return false
	}
	events, _, err := loadTaskEvents(root, taskID)
	if err != nil || len(events) == 0 {
		return false
	}
	for _, ev := range events {
		if ev.Type == evCanceled {
			return true
		}
	}
	return false
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
