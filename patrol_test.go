package main

// CG-5 drain 内巡逻测试:两条独立信号(procgroup 存活 + 日志心跳)判卡死。
//
// 三条验收测试对应卡描述:
//   A. 真僵态复现:执行器不退不写日志 → 心跳超时判卡 → cancel 整组击杀 → 子进程真死
//   B. 反例注入:伪心跳(procgroup 死透 + 日志假装继续增长)不得骗过 patrol
//      —— 教训:procgroup 存活是唯一的授权凭证,心跳只是辅助信号
//   C. 启动窗口保护:任务刚进 activeIDs 但 invoke 未 register,patrol 不误杀

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// patrolShort 临时短化三个 patrol 参数;返回还原函数供 defer 调用。
func patrolShort() func() {
	origHB, origPG, origCD := patrolHeartbeatTimeout, patrolPGGrace, patrolEventCooldown
	patrolHeartbeatTimeout = 30 * time.Millisecond
	patrolPGGrace = 20 * time.Millisecond
	patrolEventCooldown = 0
	return func() {
		patrolHeartbeatTimeout = origHB
		patrolPGGrace = origPG
		patrolEventCooldown = origCD
	}
}

// A. 真僵态:sh -c "sleep 60" 不写日志 → 心跳超时命中 → cancel(真实 killProcGroup) → sleep 死。
// 【为什么用真进程】要证明 patrol → cancel → killProcGroup 完整链路可达;mock cancel 只能验证
// 决策(能不能判卡)不能验证收尾(能不能真杀掉)。
// 【杀的突变】把 patrolOnce 里的 cancel() 调用删掉 → sleep 60 秒不会死,测试红。
func TestPatrolFlagsSilentlyHungTaskAndKillsProcgroup(t *testing.T) {
	defer patrolShort()()

	root := testRoot(t)
	taskID := "patrol-silent"
	// setupProcGroup 会挂 cmd.Cancel(超时整组击杀),Go 契约要求配套 CommandContext。
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "sleep 60")
	setupProcGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	registerTaskInvoke(taskID, pid)
	// cmd.Wait 反证进程被真正回收(不再 zombie);跑在 goroutine 里,主体在 patrol 触发击杀后 select 等它收。
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	defer func() {
		_ = killProcGroup(pid) // 兜底:若测试逻辑提前失败,别留 sleep 挂着
		unregisterTaskInvoke(taskID, pid)
	}()

	activeIDs := map[string]bool{taskID: true}
	activeCancels := map[string]func(){
		taskID: func() { _ = killProcGroup(pid) }, // 真实击杀,验证完整链路
	}
	states := map[string]*patrolState{}

	// 首轮:sleep 60 活着,pgSeenAlive=true,lastLogGrow=now。
	patrolOnce(root, activeIDs, activeCancels, states, time.Now())
	if !states[taskID].pgSeenAlive {
		t.Fatal("前提:首轮应见 procgroup 存活(sleep 60 正在跑,anyTaskProcAlive 应返回 true)")
	}

	// 等心跳超时(sleep 不写日志)。
	time.Sleep(40 * time.Millisecond)

	// 第二轮:noHeartbeat 触发 → 击杀。
	patrolOnce(root, activeIDs, activeCancels, states, time.Now())

	// cmd.Wait 返回即证明 sleep 被真正杀掉并回收(POSIX zombie 未 waitpid 前 processAlive 恒真,
	// 直接查 processAlive 会假阳性;以 cmd.Wait 收尾时刻为准更严)。
	select {
	case <-waitDone:
		// 好:进程被杀并被 wait 收尾
	case <-time.After(2 * time.Second):
		t.Fatal("patrol 命中卡死应经 cancel 整组击杀,cmd.Wait 应在 2s 内返回(sleep 60 已被杀)")
	}

	// evStalled 事件应有落笔(先记事件后击杀的审计凭据)。
	events, _, err := loadTaskEvents(root, taskID)
	if err != nil {
		t.Fatalf("加载事件失败: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Type == evStalled {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("patrol 判卡死应先记 evStalled 事件(先记事件后击杀:即便击杀失败也留审计凭据)")
	}
}

// B. 反例注入:procgroup 死透 + 日志假装继续增长(测试脚本每几十 ms echo >>log) 不得骗过 patrol。
// 【为什么这么重要】若把心跳当独立授权凭证,任何写日志的守护脚本都能让 patrol 误判"还活着",
// 卡死侦测形同虚设。设计上 procgroup 是唯一的授权凭证,心跳仅辅助——本测试直击该规则。
// 【杀的突变】把 patrolOnce 里 pgDeadTooLong 的判据 || 到 noHeartbeat 依赖 alive 之类的条件 →
// 日志新增刷 lastLogGrow 就绕过 → 测试红。
func TestPatrolFakeHeartbeatCannotDefeatPatrol(t *testing.T) {
	defer patrolShort()()

	root := testRoot(t)
	taskID := "patrol-fake-hb"
	logPath := taskLogPath(root, taskID)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	activeIDs := map[string]bool{taskID: true}
	cancelled := false
	activeCancels := map[string]func(){
		taskID: func() { cancelled = true },
	}
	states := map[string]*patrolState{}

	// 首轮建 state。procgroup 未 register → alive=false,pgSeenAlive 初值 false。
	patrolOnce(root, activeIDs, activeCancels, states, time.Now())

	// 手工置 pgSeenAlive=true(模拟"曾观察到活过") + pgDeadSince=30ms 前(死超 pgGrace=20ms)。
	states[taskID].pgSeenAlive = true
	states[taskID].pgDeadSince = time.Now().Add(-30 * time.Millisecond)

	// 伪心跳:测试脚本追加日志(冒充执行器还在写输出)。
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("still writing\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	// 第二轮:日志新增会刷新 lastLogGrow 让 noHeartbeat 假,但 pgDeadTooLong 应仍触发。
	patrolOnce(root, activeIDs, activeCancels, states, time.Now())
	if !cancelled {
		t.Fatal("伪心跳骗过 patrol:procgroup 死透仍应判卡死(心跳是辅助信号,不是授权凭证)")
	}
}

// C. 启动窗口保护:任务刚进 activeIDs 但 invoke 尚未 cmd.Start/register,patrol 反复见 alive=false
// 也不能误判卡死。等待 invoke 就绪的窗口(runTask 步骤间隙、invoke→invoke 切换)属正常。
// 【杀的突变】去掉 patrolState.pgSeenAlive 前置守卫,直接用 pgDeadSince 计时 → 启动即误杀,测试红。
func TestPatrolStartupGraceProtectsUninvokedTask(t *testing.T) {
	defer patrolShort()()
	// 本测试只验 procgroup 通道的启动保护,禁用 heartbeat 通道以隔离变量。
	origHB := patrolHeartbeatTimeout
	patrolHeartbeatTimeout = 24 * time.Hour
	defer func() { patrolHeartbeatTimeout = origHB }()

	root := testRoot(t)
	taskID := "patrol-startup"
	// 无日志、无 procgroup 注册,模拟"任务已 activeIDs 但 invoke 尚未 cmd.Start"。
	activeIDs := map[string]bool{taskID: true}
	cancelled := false
	activeCancels := map[string]func(){
		taskID: func() { cancelled = true },
	}
	states := map[string]*patrolState{}

	// 多轮 patrol,累计时长 > pgGrace(20ms),应不触发。
	for i := 0; i < 5; i++ {
		patrolOnce(root, activeIDs, activeCancels, states, time.Now())
		time.Sleep(10 * time.Millisecond)
	}
	if cancelled {
		t.Fatal("procgroup 从未观察到活过时不得误杀(启动窗口保护:排除假阳性)")
	}
}
