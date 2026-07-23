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
	"runtime"
	"strconv"
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
