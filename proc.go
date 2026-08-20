package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// rescueWaitDelay 把"进程成功退出但派生后台子进程握住 stdout 管道触发 WaitDelay"的
// exec.ErrWaitDelay 救回为 nil。cmd.Wait 因孙进程（ssh ControlPersist 后台 mux、rsync -e ssh、
// MCP/探针脚本等）吊住管道写端，等到 setupProcGroup 设的 WaitDelay(10s)后返回 ErrWaitDelay，
// 但进程本身已 exit 0（ProcessState.Success()）。不救则：runReviewSync 把成功的同步误判失败→
// divert 每轮被收掉、分流特性静默失效；本地执行器（invokeClaude/invokeCodex）把在手的有效结果
// 当失败重试。真正的超时/非零退出 ProcessState.Success()=false，不受本救援影响。
// 远端执行器（invokeRemoteClaude/invokeRemoteCodex）自有"有结果即成功"救援，不走此路。
func rescueWaitDelay(err error, cmd *exec.Cmd) error {
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		return nil
	}
	return err
}

// 在跑执行器的进程登记簿。执行器自成进程组（见 setupProcGroup）后不再随
// cardex 的前台进程组收到终端信号——Ctrl-C/SIGTERM 若不接管，cardex 一死
// 它们就孤儿化继续烧额度、占工作目录（与 cancel 不杀进程是同一类病）。
var (
	procMu          sync.Mutex
	procGroups      = map[int]bool{}
	killHandlerOnce sync.Once
)

// taskPGIDs 是"任务→当前活着的执行器 pid 集合"的映射,供 CG-5 巡逻查任务进程组存活。
// 【为什么单独于 procGroups】procGroups 只保存 pid 集合供信号处理器连坐击杀,不知道 pid 属于哪张
// 卡;巡逻需要"某张卡当前有没有活着的执行器"的问答,反向索引必须走独立结构。同一任务可能有多个
// pid(理论极少,但 setupProcGroup 支持多次 invoke——如同 runTask 循环内多步)。
var (
	taskPGMu sync.Mutex
	taskPG   = map[string]map[int]bool{}
	// taskPGResidue records every process group that remained alive after its direct executor
	// returned. A later attempt may prune a group only after the OS proves it dead; simply starting
	// another invocation must never erase a still-live writer from the fallback proof.
	taskPGResidue = map[string]map[int]bool{}
	// taskLeaseResidue complements process-group probing. A provider or shell can call setsid(2)
	// and leave the original PGID while a detached descendant still owns the task workspace. The
	// inherited lease pipe remains open across that ordinary detach, so fallback stays fail-closed
	// until every inheriting descendant exits. This is an accidental-residue guard, not a hostile
	// sandbox: a program that deliberately closes unknown file descriptors can opt out.
	taskLeaseResidue = map[string][]<-chan struct{}{}
)

// taskProcessLease is prepared by the platform-specific helper before cmd.Start. On Unix, done is
// closed only after EOF on an inherited pipe; on Windows the helper returns nil and the existing
// conservative processGroupAlive implementation keeps policy fallback fail-closed.
type taskProcessLease struct {
	done   <-chan struct{}
	commit func()
	abort  func()
}

func pruneTaskLeaseResidueLocked(taskID string) {
	leasing := taskLeaseResidue[taskID]
	if len(leasing) == 0 {
		return
	}
	alive := leasing[:0]
	for _, done := range leasing {
		select {
		case <-done:
			// EOF proves every ordinary inheritor has closed the lease.
		default:
			alive = append(alive, done)
		}
	}
	if len(alive) == 0 {
		delete(taskLeaseResidue, taskID)
		return
	}
	taskLeaseResidue[taskID] = alive
}

func resetTaskProcessResidue(taskID string) {
	if taskID == "" {
		return
	}
	taskPGMu.Lock()
	pruneTaskLeaseResidueLocked(taskID)
	pgids := make([]int, 0, len(taskPGResidue[taskID]))
	for pgid := range taskPGResidue[taskID] {
		pgids = append(pgids, pgid)
	}
	taskPGMu.Unlock()
	for _, pgid := range pgids {
		if processGroupAlive(pgid) {
			continue
		}
		taskPGMu.Lock()
		if groups := taskPGResidue[taskID]; groups != nil {
			delete(groups, pgid)
			if len(groups) == 0 {
				delete(taskPGResidue, taskID)
			}
		}
		taskPGMu.Unlock()
	}
}

func markTaskLeaseResidue(taskID string, done <-chan struct{}) {
	if taskID == "" || done == nil {
		return
	}
	taskPGMu.Lock()
	pruneTaskLeaseResidueLocked(taskID)
	taskLeaseResidue[taskID] = append(taskLeaseResidue[taskID], done)
	taskPGMu.Unlock()
	// Remove a completed lease even when this task never asks for another residue probe. Without this
	// waiter, closed channels remain in the global map until an unrelated later query for the same ID.
	go func() {
		<-done
		taskPGMu.Lock()
		leasing := taskLeaseResidue[taskID]
		kept := leasing[:0]
		for _, candidate := range leasing {
			if candidate != done {
				kept = append(kept, candidate)
			}
		}
		if len(kept) == 0 {
			delete(taskLeaseResidue, taskID)
		} else {
			taskLeaseResidue[taskID] = kept
		}
		taskPGMu.Unlock()
	}()
}

func markTaskProcessResidue(taskID string, pgid int) {
	if taskID == "" || pgid <= 0 {
		return
	}
	taskPGMu.Lock()
	groups := taskPGResidue[taskID]
	if groups == nil {
		groups = make(map[int]bool)
		taskPGResidue[taskID] = groups
	}
	groups[pgid] = true
	taskPGMu.Unlock()
}

func taskProcessResidue(taskID string) bool {
	if taskID == "" {
		return false
	}
	resetTaskProcessResidue(taskID)
	taskPGMu.Lock()
	defer taskPGMu.Unlock()
	pruneTaskLeaseResidueLocked(taskID)
	return len(taskPGResidue[taskID]) != 0 || len(taskLeaseResidue[taskID]) != 0
}

func registerTaskInvoke(taskID string, pid int) {
	if taskID == "" || pid <= 0 {
		return
	}
	taskPGMu.Lock()
	defer taskPGMu.Unlock()
	m, ok := taskPG[taskID]
	if !ok {
		m = make(map[int]bool)
		taskPG[taskID] = m
	}
	m[pid] = true
}

func unregisterTaskInvoke(taskID string, pid int) {
	if taskID == "" || pid <= 0 {
		return
	}
	taskPGMu.Lock()
	defer taskPGMu.Unlock()
	if m, ok := taskPG[taskID]; ok {
		delete(m, pid)
		if len(m) == 0 {
			delete(taskPG, taskID)
		}
	}
}

// anyTaskProcAlive 报告该任务当前有没有活着的执行器进程组。
// 【为什么按登记表 + processAlive 双查】仅看登记表存在,不足以判活:register 后进程若已死但
// unregister 尚未跑到(defer 未执行),表里会残留死 pid;仅看 processAlive 不查登记,无法回答"这张
// 卡的执行器"的定向问题(pid 属于任何卡都可能存在)。两者相与:登记为准找候选,processAlive 逐一
// 核实,伪存活(表里死 pid)不会骗过巡逻——反例注入教训:巡逻的"存活"是授权凭证,登记≠存活。
func anyTaskProcAlive(taskID string) bool {
	taskPGMu.Lock()
	pids := make([]int, 0, len(taskPG[taskID]))
	for pid := range taskPG[taskID] {
		pids = append(pids, pid)
	}
	taskPGMu.Unlock()
	for _, pid := range pids {
		if processAlive(pid) {
			return true
		}
	}
	return false
}

// runCmdRegistered 代替 cmd.Run：把子进程登记在册，供信号处理器连坐击杀。
func runCmdRegistered(cmd *exec.Cmd) error {
	return runCmdRegisteredHarvest(cmd, nil)
}

// runCmdRegisteredForTask 同 runCmdRegistered,额外把 pid 登记到 taskPG,供巡逻查任务进程组存活。
// 【为什么单独一支而非改 runCmdRegistered 签名】改现有签名会打断 runReviewSync/tests 等大量调用
// 点;新增函数只让 4 处 invoke(claude/codex/远端×2)接入巡逻,其它路径无需触巡逻不变。
func runCmdRegisteredForTask(cmd *exec.Cmd, taskID string) error {
	return runCmdRegisteredForTaskWorkspace(cmd, taskID, cmd.Dir)
}

// runCmdRegisteredForTaskWorkspace binds the inherited writer lease to the authoritative product
// workspace even when the executable itself runs in a disposable review copy.
func runCmdRegisteredForTaskWorkspace(cmd *exec.Cmd, taskID, workspaceDir string) error {
	return runCmdRegisteredHarvestForTaskWorkspace(cmd, nil, taskID, workspaceDir)
}

// remoteHarvestPoll 早收割看门狗的轮询间隔；两拍（发现结果+一拍宽限）后仍不退即击杀。
// 包级变量而非常量：测试用毫秒级间隔驱动；生产最坏多等两拍（30s），对 150 分钟超时可忽略。
var remoteHarvestPoll = 15 * time.Second

// runCmdRegisteredHarvest 同 runCmdRegistered，外加可选的「结果在手早收割」看门狗。
// 远端命令常在结果已完整打回 stdout 后仍不退出——远端孙进程继承并吊住 ssh 管道，
// EOF 永不到来，Wait 干等到 step_timeout（实测挂满 150 分钟：完成品不被收割，
// 还占着同目录串行锁堵死后续队列）。resultInBuf 非 nil 时轮询之：连续两拍为真
// 且进程仍在 → 整组击杀让 Wait 立刻返回，上层「结果在手即成功」救援把击杀退出码洗白。
// 两拍宽限防结果行刚落缓冲、stdout 尾部仍在冲刷时误杀。
func runCmdRegisteredHarvest(cmd *exec.Cmd, resultInBuf func() bool) error {
	return runCmdRegisteredHarvestForTaskWorkspace(cmd, resultInBuf, "", cmd.Dir)
}

// runCmdRegisteredHarvestForTask 同 runCmdRegisteredHarvest,额外把 pid 登记到 taskPG(taskID 非空时)。
func runCmdRegisteredHarvestForTask(cmd *exec.Cmd, resultInBuf func() bool, taskID string) error {
	return runCmdRegisteredHarvestForTaskWorkspace(cmd, resultInBuf, taskID, cmd.Dir)
}

func runCmdRegisteredHarvestForTaskWorkspace(cmd *exec.Cmd, resultInBuf func() bool, taskID, workspaceDir string) error {
	lease, leaseErr := prepareTaskProcessLease(cmd, taskID, workspaceDir)
	if leaseErr != nil {
		return leaseErr
	}
	if err := cmd.Start(); err != nil {
		if lease != nil && lease.abort != nil {
			lease.abort()
		}
		return err
	}
	if lease != nil && lease.commit != nil {
		lease.commit()
	}
	pid := cmd.Process.Pid
	procMu.Lock()
	procGroups[pid] = true
	procMu.Unlock()
	registerTaskInvoke(taskID, pid)
	defer func() {
		procMu.Lock()
		delete(procGroups, pid)
		procMu.Unlock()
		unregisterTaskInvoke(taskID, pid)
	}()
	if resultInBuf != nil {
		poll := remoteHarvestPoll // 读一次到局部：看门狗 goroutine 内读全局会与测试 defer 改写并发竞态（-race）
		done := make(chan struct{})
		defer close(done)
		go func() {
			armed := false
			t := time.NewTicker(poll)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-t.C:
					if !resultInBuf() {
						continue
					}
					if armed {
						_ = killProcGroup(pid)
						return
					}
					armed = true
				}
			}
		}()
	}
	err := cmd.Wait()
	// Direct-child exit normally closes the lease immediately. Give the pipe reader a bounded
	// scheduling window; if it is still open, a descendant retained the execution lease and the
	// policy proof must block the next writer until EOF is observed later.
	if lease != nil && lease.done != nil {
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-lease.done:
			timer.Stop()
		case <-timer.C:
			markTaskLeaseResidue(taskID, lease.done)
		}
	}
	// Wait reaps the direct child. A surviving member of its process group is therefore writer/process
	// residue, even though the leader pid itself is gone; preserve that fact for the fallback proof.
	if taskID != "" && processGroupAlive(pid) {
		markTaskProcessResidue(taskID, pid)
	}
	return err
}

// syncBuffer 是给 exec.Cmd 输出用的并发安全缓冲：exec 的内部拷贝 goroutine 写，
// 早收割看门狗并发读（String），裸 bytes.Buffer 会数据竞争。
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// installKillHandler 让 Ctrl-C/SIGTERM 先击杀全部在册执行器进程组再退出。
func installKillHandler() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		procMu.Lock()
		for pid := range procGroups {
			_ = killProcGroup(pid)
		}
		procMu.Unlock()
		os.Exit(130)
	}()
}
