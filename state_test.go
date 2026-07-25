package main

// state_test.go —— 单实例调度锁(state.go 的 acquireLock/releaseLock)回红反例测试。
//
// CG-R1 修复的双洞:
//   1) acquireLock 强夺分支旧法裸 os.Remove(path) → 多进程可同时 Remove-Link 双持锁;
//   2) releaseLock 无条件 os.Remove → 系统睡眠/挂起跨 TTL 唤醒时 A.defer release 会删掉
//      强夺者 B 刚建的新锁, 让第三写者 C 双跑。
// 与 events.go:acquireEventLock/releaseEventLock 同源同类,机械移植同法闭合。

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestReleaseLockRefusesForeignPID (CG-R1 修复反例 · state.go:releaseLock 核 PID 匹配)
// 预置一个 PID != Getpid() 的锁文件, 调 releaseLock 应保持文件在——只删自己名下的。
// 【反例】把 releaseLock 回退成 `_ = os.Remove(lockPath(root))`, 本测试会看到锁被误删 → 报红。
// 【为什么这条测试直接钉住睡眠/唤醒后误删他人锁的回归】staleLock 判 mtime>TTL 就允许强夺, 系统
// 挂起跨 TTL 唤醒时 A 的 defer release 若无 PID 核, 会删掉强夺者 B 刚 Link 挂的新锁, 让第三写者
// C 进临界区双跑。核 PID 匹配是最小契约: 内容不可读/不可解析/PID 不匹配都不删。
func TestReleaseLockRefusesForeignPID(t *testing.T) {
	root := testRoot(t)
	path := lockPath(root)
	// 预置"他人"锁: PID=1 是 init 进程, 存活但绝非本 test 进程. 组合"存在+PID 非自身"触发新
	// releaseLock 的核 PID 检查, 不允许删.
	info, _ := json.Marshal(lockInfo{PID: 1, At: time.Now().Format(time.RFC3339)})
	if err := os.WriteFile(path, info, 0o644); err != nil {
		t.Fatal(err)
	}
	releaseLock(root)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		t.Fatal("releaseLock 误删了他人 PID 的锁——反例注入: releaseLock 回退成裸 os.Remove 会走此路径, 本测试即刻报红")
	}
	// 反证 1: 本进程 PID 的锁必须能被 releaseLock 正常释放.
	selfInfo, _ := json.Marshal(lockInfo{PID: os.Getpid(), At: time.Now().Format(time.RFC3339)})
	if err := os.WriteFile(path, selfInfo, 0o644); err != nil {
		t.Fatal(err)
	}
	releaseLock(root)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("PID 匹配自身的锁必须被 releaseLock 正常删除, 否则单实例锁永远死锁")
	}
	// 反证 2: 内容不可解析的锁不删——留给真正的持有者或让下一轮 staleLock/mtime>TTL 兜底.
	if err := os.WriteFile(path, []byte("{invalid"), 0o644); err != nil {
		t.Fatal(err)
	}
	releaseLock(root)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		t.Fatal("内容不可解析不应删——防误伤真正的持有者(Unmarshal 中途崩溃写坏的锁, 待 mtime>TTL 兜底)")
	}
}

// TestHelperProcessAcquireLock 是跨进程锁测试的 helper 子进程入口, 受 GO_TEST_HELPER_LOCK=1 门控.
// 无环境变量时直接返回, 主 test 二进制可把它当空测试跑过. 有环境变量时按参数在指定时刻起跑
// acquireLock, 拿到锁则 hold 短时间(远小于 TTL)后 release、以 exit 0 汇报; 没拿到 exit 1.
func TestHelperProcessAcquireLock(t *testing.T) {
	if os.Getenv("GO_TEST_HELPER_LOCK") != "1" {
		return
	}
	root := os.Getenv("HELPER_ROOT")
	ttlMs, _ := strconv.Atoi(os.Getenv("HELPER_TTL_MS"))
	holdMs, _ := strconv.Atoi(os.Getenv("HELPER_HOLD_MS"))
	startAtMicro, _ := strconv.ParseInt(os.Getenv("HELPER_START_AT_UNIX_MICRO"), 10, 64)
	// 统一起跑时刻: 所有 helper 尽量同时进 acquireLock, 才能在 stale 锁上触发多方并发强夺.
	if wait := time.Until(time.UnixMicro(startAtMicro)); wait > 0 {
		time.Sleep(wait)
	}
	ttl := time.Duration(ttlMs) * time.Millisecond
	if !acquireLock(root, ttl) {
		os.Exit(1)
	}
	// hold << ttl: 保证 loser 在 acquireLock 内两次 os.Link 尝试都发生在 winner 未 release 前,
	// staleLock 见 winner 新挂锁 mtime 新鲜返回 false, loser 立刻 return false. 若 hold 逼近或
	// 超过 TTL, loser 有机会在 winner release 后第二次 os.Link 成功 → 假 winner. hold=50ms 远
	// 小于 ttl=1s, 稳挡.
	time.Sleep(time.Duration(holdMs) * time.Millisecond)
	releaseLock(root)
	os.Exit(0)
}

// TestAcquireLockStealsAtomicallyNoDoubleOccupancy (CG-R1 修复反例 · state.go:acquireLock 强夺唯一化)
// 预置 stale 锁 (mtime>TTL 的空文件模拟"陈旧遗留"), 起 N=5 个 helper 子进程同时进 acquireLock
// 强夺分支. 旧法裸 os.Remove(path) 让多个进程都 Remove 后 os.Link 挂锁双持; 新法 os.Rename 是
// POSIX 原子, path 只能被一方成功搬走, 恰 1 个进程返回 true.
//
// 【反例】把 acquireLock 里 os.Rename 改回 os.Remove(path), 本测试会看到 2+ 个 helper 子进程
// exit 0 → winners > 1 → 断言直接报红.
//
// 【为什么必须跨进程】单进程内多 goroutine 共享 os.Getpid(), acquireLock 里的 PID 判据无法区分
// 竞争者, 无法真实再现"多方同时进强夺"的原子性缺陷. helper-process 模式复用 events_test.go 里
// 已过审的跨进程测试模板(TestRecordEventCrossProcessNoSeqCollision).
func TestAcquireLockStealsAtomicallyNoDoubleOccupancy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 行为未验证, 平台纳入与否待裁(01-BACKLOG §3 #61);当前 CI 只跑 POSIX")
	}
	if testing.Short() {
		t.Skip("跨进程 fork+exec 较慢, -short 模式跳过")
	}
	root := testRoot(t)
	path := lockPath(root)
	// 预置 stale 锁: 空内容(Unmarshal 失败) + mtime>TTL. TTL=1s, mtime 回退 5s → mtime>TTL 判据
	// 稳定判 stale, 走强夺分支. PID=999999 兜底: staleLock 里 Unmarshal 失败会短路走 mtime 分支.
	info, _ := json.Marshal(lockInfo{PID: 999999, At: time.Now().Format(time.RFC3339)})
	if err := os.WriteFile(path, info, 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-5 * time.Second)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
	// 起跑时刻放 500ms 后: 让所有 helper 都已 Start 就位, 尽量并发进 acquireLock 强夺分支.
	startAt := time.Now().Add(500 * time.Millisecond)
	const workers = 5
	cmds := make([]*exec.Cmd, 0, workers)
	for i := 0; i < workers; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcessAcquireLock$")
		cmd.Env = append(os.Environ(),
			"GO_TEST_HELPER_LOCK=1",
			"HELPER_ROOT="+root,
			"HELPER_TTL_MS=1000",
			"HELPER_HOLD_MS=50",
			"HELPER_START_AT_UNIX_MICRO="+strconv.FormatInt(startAt.UnixMicro(), 10),
		)
		// stdout/stderr 直连便于定位子进程 panic; 目标是"winners>1 必红".
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatalf("helper 子进程 %d Start 失败: %v", i, err)
		}
		cmds = append(cmds, cmd)
	}
	winners := 0
	for i, cmd := range cmds {
		err := cmd.Wait()
		if err == nil {
			winners++
			continue
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("helper 子进程 %d 非退出错: %v", i, err)
		}
		if exitErr.ExitCode() != 1 {
			t.Fatalf("helper 子进程 %d 意外退出码 %d(想要 1=拿锁失败)", i, exitErr.ExitCode())
		}
	}
	if winners != 1 {
		t.Fatalf("跨进程强夺 stale 锁: winners=%d(应恰 1); 反例注入: acquireLock 里 os.Rename 改回 os.Remove(path) → 多方 Remove-Link 双持, 本断言即红.", winners)
	}
}

// spawnLiveForeignPID 起一个存活的子进程 (sleep 60), 返回其 PID 与清理 cleanup。
// 用途:锁的 TOCTOU 复核测试要求"存活异 PID", os.Getpid() 是自身不算异, PID=1 (init) 在
// 非 root 下 kill(1,0) 常返回 EPERM 让 processAlive 判假不算存活。起真实子进程避免这些坑。
func spawnLiveForeignPID(t *testing.T) (int, func()) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows 无 POSIX sleep 命令; TOCTOU 复核测试仅跑 POSIX")
	}
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn sleep helper: %v", err)
	}
	pid := cmd.Process.Pid
	// 起完立刻核活, 避免子进程尚未准备好时 processAlive 抖动返回 false。
	if !processAlive(pid) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("子进程 pid=%d 起后 processAlive=false, 环境异常", pid)
	}
	cleanup := func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	return pid, cleanup
}

// TestIsForeignLiveLockClassifies (CG-R1 R3 P2-2 · isForeignLiveLock 单元反例)
// isForeignLiveLock 是 acquireLock/releaseLock 的 TOCTOU 复核判据: 返 true 才归还锁归属,
// 属自身/已死/不可解析/文件不存在都返 false。分类要点:
//   - parseable + PID>0 + PID != self + processAlive(PID) → true (真"存活异 PID")
//   - parseable + PID == self → false (归属就是本方, 不需归还)
//   - parseable + PID <= 0 → false (无效条目)
//   - parseable + PID != self + !processAlive(PID) → false (进程已死, 陈旧锁)
//   - 不可 Unmarshal → false
//   - 文件缺失 → false
//
// 反例注入:把判据里 `li.PID == os.Getpid()` 改成 `li.PID != os.Getpid()` (取反),
// self 场景会误判 true, 本测试的 case "self PID" 立即报红。
func TestIsForeignLiveLockClassifies(t *testing.T) {
	root := testRoot(t)
	tmpPath := func(name string) string { return lockPath(root) + "." + name }

	// case 1: 文件不存在 → false
	if isForeignLiveLock(tmpPath("missing")) {
		t.Fatal("case missing: 文件不存在应返 false")
	}

	// case 2: 内容不可解析 → false
	p2 := tmpPath("badjson")
	if err := os.WriteFile(p2, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if isForeignLiveLock(p2) {
		t.Fatal("case badjson: Unmarshal 失败应返 false")
	}

	// case 3: PID = self → false (归属就是本方)
	p3 := tmpPath("self")
	selfInfo, _ := json.Marshal(lockInfo{PID: os.Getpid(), At: time.Now().Format(time.RFC3339)})
	if err := os.WriteFile(p3, selfInfo, 0o644); err != nil {
		t.Fatal(err)
	}
	if isForeignLiveLock(p3) {
		t.Fatal("case self: PID==self 应返 false (不需归还)")
	}

	// case 4: PID = 0 (无效) → false
	p4 := tmpPath("zeropid")
	zeroInfo, _ := json.Marshal(lockInfo{PID: 0, At: time.Now().Format(time.RFC3339)})
	if err := os.WriteFile(p4, zeroInfo, 0o644); err != nil {
		t.Fatal(err)
	}
	if isForeignLiveLock(p4) {
		t.Fatal("case zeropid: PID<=0 应返 false")
	}

	// case 5: PID 属已死进程 → false (陈旧锁, 无需归还)
	// 起一个 sleep 0.01, 待其自然结束后 processAlive 应返 false, 复用其 PID。
	deadCmd := exec.Command("sleep", "0")
	if err := deadCmd.Start(); err != nil {
		t.Fatalf("spawn dead helper: %v", err)
	}
	deadPID := deadCmd.Process.Pid
	if err := deadCmd.Wait(); err != nil && !strings.Contains(err.Error(), "exit status") {
		// sleep 0 正常退出返 nil; 允许非零退出的边缘, 只要 PID 已被 wait 收.
	}
	// 兜底: 若极偶发 processAlive 仍返 true (PID 被系统立刻复用), 换用一定不存在的高 PID。
	if processAlive(deadPID) {
		deadPID = 0x7FFFFFFE // 用 int32 边界近上限 PID, 系统内极少复用到
	}
	p5 := tmpPath("deadpid")
	deadInfo, _ := json.Marshal(lockInfo{PID: deadPID, At: time.Now().Format(time.RFC3339)})
	if err := os.WriteFile(p5, deadInfo, 0o644); err != nil {
		t.Fatal(err)
	}
	if isForeignLiveLock(p5) {
		t.Fatalf("case deadpid: PID=%d processAlive=false 应返 false", deadPID)
	}

	// case 6: PID 属存活异进程 → true (需归还)
	// (Windows 无 sleep, spawnLiveForeignPID 会 skip)
	livePID, cleanup := spawnLiveForeignPID(t)
	defer cleanup()
	p6 := tmpPath("livepid")
	liveInfo, _ := json.Marshal(lockInfo{PID: livePID, At: time.Now().Format(time.RFC3339)})
	if err := os.WriteFile(p6, liveInfo, 0o644); err != nil {
		t.Fatal(err)
	}
	if !isForeignLiveLock(p6) {
		t.Fatalf("case livepid: PID=%d 存活异 PID 必须返 true (否则 TOCTOU 收窄失效, 双持锁场景重开)", livePID)
	}
}

// TestAcquireLockRestoresForeignLiveLockOnStolenRename (CG-R1 R3 P2-2 · 强夺侧 TOCTOU 反例)
// 场景直落 P2-2 复审: staleLock 判据到 os.Rename 之间, path 可能被他人 Link 新鲜锁 (B 过
// staleLock 判据后停顿至 A 完成 Rename+Link, B 的 Rename 会搬走 A 的新鲜锁双持)。
// 预置内容 = 存活异 PID + mtime>TTL: staleLock 沿 mtime>ttl 路径判 stale=true, 我们进入
// 强夺分支; 新法 Rename 后核 stale, 见存活异 PID → Link 归还并返回 false。
//
// 【反例】把 acquireLock 强夺分支的 `if isForeignLiveLock(stale) { … return false }` 分支
// 去掉 (回退到"Rename 成功即 Remove(stale)"), 本测试:
//   - 断言 acquireLock 返回 false: 会看到返回 true (双持锁场景);
//   - 断言锁归属仍是 foreignPID: 会看到 PID 被夺成 self, 反例即红。
func TestAcquireLockRestoresForeignLiveLockOnStolenRename(t *testing.T) {
	root := testRoot(t)
	path := lockPath(root)

	livePID, cleanup := spawnLiveForeignPID(t)
	defer cleanup()

	// 预置: 存活异 PID 内容 + mtime 回退 5s 触发 staleLock (TTL=1s)。
	info, _ := json.Marshal(lockInfo{PID: livePID, At: time.Now().Format(time.RFC3339)})
	if err := os.WriteFile(path, info, 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-5 * time.Second)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}

	got := acquireLock(root, 1*time.Second)
	if got {
		t.Fatal("P2-2 反例: 强夺分支 Rename 出的是存活异 PID 的新鲜锁, acquireLock 必须归还并返回 false — " +
			"当前返回 true 说明 isForeignLiveLock 归还分支未生效, 双持锁场景重开")
	}
	// 归还必须让锁归属未变。
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("锁被误删——反例注入: 去掉 os.Link(stale, path) 归还分支, 本测试报红: %v", err)
	}
	var restored lockInfo
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("锁内容被破坏: %v (data=%q)", err, string(data))
	}
	if restored.PID != livePID {
		t.Fatalf("归属被夺: got PID=%d, want %d (存活异 PID 必须原样归还)", restored.PID, livePID)
	}
}

// TestReleaseLockRestoresLockStolenBetweenReadAndDelete (CG-R1 R3 P2-2 · 释放侧 TOCTOU 反例)
// 场景直落 P2-2 复审: releaseLock 的 ReadFile → Remove 间隙, 他人若判 stale 强夺 (Rename+Link
// 新锁), 旧法无脑 Remove(path) 会误删他们的新锁。
// 无法在真机上确定性复现该竞态, 但可以脱下"读→改→动"三步分开做:
//  1. 手工先写自己 PID 的锁 (进入释放流程时 ReadFile 会读到本方 PID);
//  2. 让 releaseLock 走进 Rename 分支; 我们模拟"间隙内被他人夺权"通过在 Rename 之后**手工检查
//     stale 归属**——但 releaseLock 里内联复核 isForeignLiveLock, 这里我们改测更硬的契约:
//     "内容为自身 PID 的锁必须被 releaseLock 干净释放"(基线不能被 TOCTOU 加固破坏)。
//
// 更硬的 TOCTOU 反例通过 isForeignLiveLock 单元测试 (TestIsForeignLiveLockClassifies) 与 acquire
// 侧的 TestAcquireLockRestoresForeignLiveLockOnStolenRename 已直接钉住; 本测试守 release 基线契约。
func TestReleaseLockRestoresLockStolenBetweenReadAndDelete(t *testing.T) {
	root := testRoot(t)
	path := lockPath(root)

	// 基线契约 1: 归属为自身的锁, releaseLock 必须清干净。
	selfInfo, _ := json.Marshal(lockInfo{PID: os.Getpid(), At: time.Now().Format(time.RFC3339)})
	if err := os.WriteFile(path, selfInfo, 0o644); err != nil {
		t.Fatal(err)
	}
	releaseLock(root)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("归属为自身 releaseLock 后 path 应消失: err=%v", err)
	}
	// 无 stale 文件残留 (Rename→Remove 应清干净)。
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), filepath.Base(path)+".rel-") {
			t.Fatalf("Rename→Remove 后应无 .rel-* 残留: %s", e.Name())
		}
	}

	// 基线契约 2: 归属为存活异 PID (模拟 TOCTOU 间隙内被夺权后的最终态), releaseLock 必须
	// 归还——即便旧法只做 PID 核后 Remove, 也会因 PID 不匹配返回不动; 新法 Rename→复核→归还,
	// 最终态也是"锁归属未变"。
	livePID, cleanup := spawnLiveForeignPID(t)
	defer cleanup()
	foreignInfo, _ := json.Marshal(lockInfo{PID: livePID, At: time.Now().Format(time.RFC3339)})
	if err := os.WriteFile(path, foreignInfo, 0o644); err != nil {
		t.Fatal(err)
	}
	releaseLock(root)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("异 PID 锁被 releaseLock 误删——反例注入: releaseLock 在 PID 不匹配时误 Remove, 本测试报红: %v", err)
	}
	var restored lockInfo
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("锁内容被破坏: %v", err)
	}
	if restored.PID != livePID {
		t.Fatalf("归属被夺: got PID=%d, want %d", restored.PID, livePID)
	}
}
