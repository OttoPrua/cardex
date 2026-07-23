package main

// CG-5 drain 内巡逻测试:两条信号组合判卡死(单条不足)。
//
// 【CG-5 R2 修订】把心跳独立触发降级为 procgroup 死透后的确认信号,原因见 patrol.go 头注释与
// P0-1 修法(runner.go:95-97 stdout 收进内存 buffer,单步执行期任务日志天然零增长,独立触发会
// 永久取消归档健康 opus 重卡)。
//
// 五条验收测试(A/B/C 为原 CG-5 卡验收,D/E 为 R2 P0-1 回归反例):
//   A. 真僵态复现:执行器进程组死透后 patrol 判卡 → cancel 整组击杀 → 子进程真死并被回收
//   B. 反例注入:伪心跳(procgroup 死透 + 日志假装继续增长)不得骗过 patrol
//      —— 教训:procgroup 存活是唯一的授权凭证,心跳只是辅助信号
//   C. 启动窗口保护:任务刚进 activeIDs 但 invoke 未 register,patrol 不误杀
//   D. R2 P0-1 回归反例:procgroup 活着但心跳超阈值(模拟 opus 重卡单步无日志增长)——绝不触发,
//      否则 60min 单步的健康重卡被误杀成永久归档。
//   E. R2 P0-1 保底:心跳只在 procgroup 也死透时确认为 reason,配置里若把心跳阈值调低成毫秒级,
//      仅 alive 场景仍无触发——防"某人重新把心跳当独立触发"回退。

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

// A. 真僵态:sh -c "sleep 60" 起后立刻杀掉 → 进程组死透 → 过 pgGrace → pgDeadTooLong 触发 → cancel
// (真实 killProcGroup) 幂等验证完整链路。
// 【R2 场景改法】旧测试让 sleep 60 存活 + 心跳超时触发——但按 R2 P0-1 修法,心跳单独不触发,那种
// 场景是"活但静默",要交给 invoke 的 StepTimeout 兜底。现在改成 procgroup 真的死透(用 killProcGroup
// 打死 sleep 后调 cmd.Wait 收尸,anyTaskProcAlive 转 false)→ 走 pgDeadTooLong 通道,同样验证 patrol
// → cancel → killProcGroup 链路可达且能真收尾(cancel 是幂等的第二拳)。
// 【杀的突变】把 patrolOnce 里的 cancel() 调用删掉 → 未 emit evStalled/cancelled 计数为 0,测试红。
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
	defer unregisterTaskInvoke(taskID, pid)

	activeIDs := map[string]bool{taskID: true}
	cancelHits := 0
	activeCancels := map[string]func(){
		taskID: func() { cancelHits++; _ = killProcGroup(pid) }, // 真实击杀(幂等),验证完整链路
	}
	states := map[string]*patrolState{}

	// 首轮:sleep 60 活着,pgSeenAlive=true。
	patrolOnce(root, activeIDs, activeCancels, states, time.Now())
	if !states[taskID].pgSeenAlive {
		t.Fatal("前提:首轮应见 procgroup 存活(sleep 60 正在跑,anyTaskProcAlive 应返回 true)")
	}

	// 主动打死进程组并等收尸——模拟"执行器进程组真挂死"(实际生产里挂死会是执行器 wait 阻塞
	// 但进程已消失,此处直接把 sleep 杀掉等价还原该状态)。
	_ = killProcGroup(pid)
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("前提:killProcGroup 后 sleep 应被回收,cmd.Wait 应 2s 内返回")
	}
	// unregisterTaskInvoke 由 defer 保底,这里模拟 runTask goroutine 卡住没跑到 unregister——
	// taskPG 里 pid 残留但 processAlive 已假(死进程被 wait 收后 kill(pid,0) 报 ESRCH),
	// anyTaskProcAlive 假 → pgSeenAlive && !alive → pgDeadSince 计时启动。

	// 第二轮:观察到 procgroup 死透,pgDeadSince=now。
	patrolOnce(root, activeIDs, activeCancels, states, time.Now())
	if states[taskID].pgDeadSince.IsZero() {
		t.Fatal("第二轮应观察到 procgroup 死透并启动 pgDeadSince 计时")
	}

	// 等过 pgGrace(20ms)。
	time.Sleep(30 * time.Millisecond)

	// 第三轮:pgDeadTooLong 触发 → cancel(第二拳幂等)+ emit evStalled。
	patrolOnce(root, activeIDs, activeCancels, states, time.Now())
	if cancelHits == 0 {
		t.Fatal("procgroup 死透超 pgGrace 后应触发 cancel(patrol → cancelRun → killProcGroup 完整链路)")
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

// D. R2 P0-1 回归反例:procgroup 存活但心跳超阈值(模拟 opus 重卡单步 30+ 分钟无日志增长)。
// 【为什么这是 P0】旧判据把 noHeartbeat 当独立触发,踩了系统契约的雷:runner.go:95-97 stdout 收进
// 内存 buffer,logBlock 只在 invoke 返回后追加,单步执行期任务日志天然零增长;step_timeout_min
// 默认 60min → 心跳阈值 30min 独立触发就会永久取消归档健康 opus 重卡。修法:心跳必须叠加 procgroup
// 死透。此测试模拟"活但静默"场景,任何轮次都不得触发。
// 【杀的突变】把 patrol.go 里 noHeartbeat 判据里的 && pgDead 摘掉(回退到 R1 独立触发)→ 本测试红。
func TestPatrolDoesNotKillHealthyLongStepWithoutHeartbeat(t *testing.T) {
	defer patrolShort()()
	// 心跳阈值刻意压到 5ms,heartbeat 通道被"触发"到极限;procgroup 通道靠 sleep 60 常活兜住。
	origHB := patrolHeartbeatTimeout
	patrolHeartbeatTimeout = 5 * time.Millisecond
	defer func() { patrolHeartbeatTimeout = origHB }()

	root := testRoot(t)
	taskID := "patrol-healthy-long-step"
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "sleep 60")
	setupProcGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	registerTaskInvoke(taskID, pid)
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	defer func() {
		_ = killProcGroup(pid)
		unregisterTaskInvoke(taskID, pid)
		<-waitDone
	}()

	activeIDs := map[string]bool{taskID: true}
	cancelHits := 0
	activeCancels := map[string]func(){
		taskID: func() { cancelHits++ },
	}
	states := map[string]*patrolState{}

	// 首轮建 state;pgSeenAlive 应为 true(sleep 活着)。
	patrolOnce(root, activeIDs, activeCancels, states, time.Now())
	if !states[taskID].pgSeenAlive {
		t.Fatal("前提:首轮应见 procgroup 存活(sleep 60 正在跑)")
	}

	// 反复 patrol 到时长远超心跳阈值(5ms×N=100ms > 5ms)。日志文件不存在→lastLogGrow 始终=firstSeen,
	// noHeartbeat 判据(单点)已到期,但因 procgroup 活着(pgDead=false),叠加判据必假,不得触发。
	for i := 0; i < 20; i++ {
		patrolOnce(root, activeIDs, activeCancels, states, time.Now())
		time.Sleep(5 * time.Millisecond)
	}
	if cancelHits != 0 {
		t.Fatal("procgroup 活但日志静默是健康 opus 重卡常态,patrol 绝不得触发;活但静默交给 step_timeout 兜底")
	}

	// evStalled 事件也不得有落笔——事件账本被这类误判污染同样是 P0。
	events, _, _ := loadTaskEvents(root, taskID)
	for _, e := range events {
		if e.Type == evStalled {
			t.Fatalf("procgroup 活但日志静默不得落 evStalled 事件(污染活动流,让人误以为真出过卡死)")
		}
	}
}

// E. R2 P0-1 保底:reason 分类必与"两信号叠加"契约一致——单纯 pgDeadTooLong 命中(心跳未过阈值)
// 只能是 procgroup_dead;noHeartbeat 命中必蕴含 pgDead,故分类应为 procgroup_dead_and_no_heartbeat。
// 【杀的突变】若 reason 分类退回 R1 的"no_heartbeat 独立"分支(即 heartbeat 单独触发时报 no_heartbeat),
// 本测试对 procgroup_dead 分类无法解释 → 红。
func TestPatrolReasonReflectsBothSignalsCombination(t *testing.T) {
	defer patrolShort()()
	// heartbeat 阈值刻意拉长,只让 pgDeadTooLong 命中,验 reason=procgroup_dead(非 no_heartbeat)。
	origHB := patrolHeartbeatTimeout
	patrolHeartbeatTimeout = 24 * time.Hour
	defer func() { patrolHeartbeatTimeout = origHB }()

	root := testRoot(t)
	taskID := "patrol-reason-classify"

	activeIDs := map[string]bool{taskID: true}
	activeCancels := map[string]func(){
		taskID: func() {},
	}
	states := map[string]*patrolState{}

	// 首轮建 state(procgroup 未 register → alive=false,pgSeenAlive 初值 false)。
	patrolOnce(root, activeIDs, activeCancels, states, time.Now())
	// 手工置 pgSeenAlive=true + pgDeadSince=30ms 前(死超 pgGrace=20ms)。
	states[taskID].pgSeenAlive = true
	states[taskID].pgDeadSince = time.Now().Add(-30 * time.Millisecond)
	// lastLogGrow 距今很近(heartbeat 未到 24h),noHeartbeat 假。

	patrolOnce(root, activeIDs, activeCancels, states, time.Now())

	events, _, err := loadTaskEvents(root, taskID)
	if err != nil {
		t.Fatalf("加载事件失败: %v", err)
	}
	var stall *TaskEvent
	for i := range events {
		if events[i].Type == evStalled {
			stall = &events[i]
			break
		}
	}
	if stall == nil {
		t.Fatal("pgDeadTooLong 应触发 evStalled")
	}
	reason, _ := stall.Detail["reason"].(string)
	if reason != "procgroup_dead" {
		t.Fatalf("单独 pgDeadTooLong 命中,reason 应为 procgroup_dead(不叠加心跳);got=%q", reason)
	}
}
