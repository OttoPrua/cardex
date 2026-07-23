package main

// patrol.go —— drain 内巡逻(CG-5)。
//
// 【为什么必须存在】
// 已知竞态"记录不修":review sync ~110s+ 完成时,孙进程吊住 stdout 管道让 120s deadline 先到,
// 成功同步被误判超时→分流每轮回退本机审;窗口极窄但真实存在,长期以"回退无害"带病运行。同时
// 执行器真挂死(永不退出、无输出、被外部 SIGSTOP 挂起、silently 死等)的场景只有 step_timeout
// (默认 60 分钟)兜底,期间目录锁与并行槽位死占。harvest 早收割处理"结果在手不退"(看得见的完
// 成态),patrol 处理"什么都看不见"的僵态——两轴正交,同样都用整组击杀但触发条件不同。
//
// 【为什么不新增守护进程】
// tick 的 drain 循环本身每 DrainRescanSec(默认 15s)重扫一次做取消对账,巡逻贴附在这一循环上零
// 新增常驻进程;整组击杀沿用 activeCancels 的机制,不引入第二套击杀路径。
//
// 【两条独立信号 + 反例注入教训】
//   1) 进程组存活:anyTaskProcAlive 查该任务当前有无活着的执行器 pid;
//   2) 心跳:任务日志文件 size 增长(执行器每步的 logBlock 会持续追加输出)。
// 反例注入:伪造心跳(测试脚本每 100ms 追加日志,但真正执行器已死)不得骗过巡逻——所以 procgroup
// 存活是**授权凭证**,心跳只是辅助信号。判据:
//   - procgroup 曾活过且现死透且死超 pgGrace(允许步骤间隙的 invoke 切换)→ 判卡死;
//   - 心跳超 heartbeatTimeout 无增长(执行器可能在长思考,但超阈值则疑挂)→ 判卡死。
// 心跳单独判据不足以证明存活——只能证明"有人在写日志",不能证明"执行器还在跑"。
//
// 【为什么触发时先记事件再击杀】
// 击杀路径可能挂(cancelRun 竞态、goroutine 卡死),事件账本先落让审计凭据留痕;patrol 记 evStalled
// 是"诊断观察",状态仍是 running,随后 finalizeCanceled 落 evCanceled 才是真状态迁移。分开落让
// 因果链清晰:dispatched → stalled(诊断) → canceled(收尾)。

import (
	"os"
	"time"
)

// 巡逻可调参数——测试用短值(patrol_test.go override),生产用保守值。
// heartbeatTimeout:允许任务日志多久没增长。太短会误杀正常长思考(claude opus 一次思考可能 5-10 分钟),
// 太长则真僵死拖长同目录锁占。取 30 分钟:半个默认 step_timeout(60 分钟),仍留 step_timeout 兜底。
// pgGrace:进程组"暂空"允许多久——runTask 步骤间隙(saveTask + emit 事件之类)、invoke → invoke 切换,
// 会有秒级窗口 procgroup 是空的。取 60s 覆盖极端慢机器上的多步切换。
// eventCooldown:同一任务重复记 evStalled 事件的最短间隔——巡逻每 15s 一轮,击杀信号若被处理不及时
// (goroutine 阻塞在 Wait 内),下一轮又会 stalled;cooldown 挡重复事件把账本刷成噪声。
var (
	patrolHeartbeatTimeout = 30 * time.Minute
	patrolPGGrace          = 60 * time.Second
	patrolEventCooldown    = 5 * time.Minute
)

// patrolState 是每任务的巡逻累积状态。生命周期 = drain 一轮排空(tick 函数内的 states map)。
// 【为什么 pgSeenAlive】任务刚进 activeIDs 时,invoke 可能还没跑到 cmd.Start,procgroup 空是正常;
// 若一上来就用 pgDeadSince 计时,可能在 60s 内误杀(pgGrace)一个 runTask 内部有慢初始化的任务。
// 只有观察到 procgroup 至少活过一次,后续再空才启动 pgDeadSince 计时——从"从未见过存活"排除
// 出可击杀集合,把假阳性压到只在真正的 running→死透 转换。
type patrolState struct {
	logSize     int64
	lastLogGrow time.Time
	pgSeenAlive bool
	pgDeadSince time.Time // 零值 = 当前 procgroup 存活 或 从未观察到过存活
	lastStallAt time.Time // 上次记 evStalled 事件的时间(cooldown 用)
	firstSeen   time.Time
}

// patrolOnce 对每个在跑任务跑一次巡逻检查。
// 【为什么用 cancelRun 而非直接调 killProcGroup】setupProcGroup 挂的 cmd.Cancel 已内含 killProcGroup,
// 走 cancel 路径能同步触发 runTask 的 ctx.Err() 检查 → finalizeCanceled → tick 的 doneMsg 回收槽位
// 与目录互斥,与人工 cancel 走同一收尾管线。patrol 若绕开这条路直接 killProcGroup,activeCancels
// 与 activeIDs/activeDirs 不会同步清理,反而破坏 tick 状态。
//
// 【为什么不改盘上 status 为 canceled】保持"卡死"与"取消"语义分离——卡死是执行器故障,取消是人工
// 决策。ctx canceled 后 finalizeCanceled 会走 canceled 归档路径(在这个实现里"疑似卡死"确实按
// canceled 归档收尾,是权衡:更细分的 stalled 归档态需要新增状态机迁移,超出本卡"最小内核"纪律);
// 事件账本的 evStalled → evCanceled 序列保留了完整因果链,人工审计能看出这次 canceled 的成因。
func patrolOnce(root string, activeIDs map[string]bool, activeCancels map[string]func(), states map[string]*patrolState, now time.Time) {
	for id := range activeIDs {
		st, ok := states[id]
		if !ok {
			st = &patrolState{firstSeen: now, lastLogGrow: now}
			states[id] = st
		}

		// 心跳:任务日志文件 size 是否增长(执行器每步 logBlock/logSection 持续追加输出)。
		if fi, err := os.Stat(taskLogPath(root, id)); err == nil {
			if fi.Size() > st.logSize {
				st.logSize = fi.Size()
				st.lastLogGrow = now
			}
		}

		// 进程组存活:登记表 + processAlive 双查(反例守卫见 anyTaskProcAlive 注释)。
		alive := anyTaskProcAlive(id)
		if alive {
			st.pgSeenAlive = true
			st.pgDeadSince = time.Time{}
		} else if st.pgSeenAlive {
			// 只有"曾活过 → 现死透"才启动 pgDeadSince 计时,防启动窗口误伤(见 patrolState 注释)。
			if st.pgDeadSince.IsZero() {
				st.pgDeadSince = now
			}
		}

		// 判据:两条互相独立,任意一条命中即判卡死。
		// procgroup 死过 pgGrace: 执行器进程组曾活过但现在死透且死超 60s——runTask 应当在 finalizeXxx
		// 收尾把任务从 activeIDs 拿走;若还在,说明 runTask 的 goroutine 卡在了什么地方(Wait 阻塞、
		// 事件锁抢占超时等),巡逻替它推一把。
		pgDeadTooLong := st.pgSeenAlive && !alive && !st.pgDeadSince.IsZero() && now.Sub(st.pgDeadSince) >= patrolPGGrace
		// 心跳超时:日志 size 长时间无增长。alive 与否都算——执行器进程活着但一动不动
		// (被 SIGSTOP 挂起/死锁),日志不会长,同样判卡死。
		noHeartbeat := now.Sub(st.lastLogGrow) >= patrolHeartbeatTimeout
		if !pgDeadTooLong && !noHeartbeat {
			continue
		}

		reason := "procgroup_dead"
		if noHeartbeat && !pgDeadTooLong {
			reason = "no_heartbeat"
		} else if noHeartbeat && pgDeadTooLong {
			reason = "procgroup_dead_and_no_heartbeat"
		}

		// 先记事件后击杀:事件是审计凭据,即便击杀失败也要有"我判定卡死于何时因何"的落笔。
		// cooldown 挡重复:巡逻 15s 一轮,击杀信号若被 goroutine 阻塞拖住,下一轮同判据又会命中,
		// 若不 cooldown 会把账本刷成噪声。
		if st.lastStallAt.IsZero() || now.Sub(st.lastStallAt) >= patrolEventCooldown {
			emitTaskEvent(root, id, evStalled, "runner:patrol", statusRunning, 0, map[string]any{
				"reason":        reason,
				"pg_dead_since": ifTimeStr(st.pgDeadSince),
				"last_log_grow": st.lastLogGrow.Format(time.RFC3339),
				"log_size":      st.logSize,
			})
			st.lastStallAt = now
		}

		// 整组击杀走 cancel 路径:cmd.Cancel = killProcGroup,runTask 的 ctx.Err() 检查见 canceled →
		// finalizeCanceled → archiveTask + emit evCanceled。tick 的 doneMsg 回收槽位/目录锁,与
		// 人工 cancel 同一收尾管线,不引入第二套击杀。cancelRun 幂等,重复调用无害。
		if cancel := activeCancels[id]; cancel != nil {
			cancel()
		}
	}

	// 清理已不在 activeIDs 的状态,防长跑 drain 里 map 无限增长(同一 drain 内会有很多任务来去)。
	for id := range states {
		if !activeIDs[id] {
			delete(states, id)
		}
	}
}

func ifTimeStr(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
