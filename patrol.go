package main

// patrol.go —— drain 内巡逻(CG-5)。
//
// 【为什么必须存在】
// 已知竞态"记录不修":review sync ~110s+ 完成时,孙进程吊住 stdout 管道让 120s deadline 先到,
// 成功同步被误判超时→分流每轮回退本机审;窗口极窄但真实存在,长期以"回退无害"带病运行。同时
// runTask 收尾 goroutine 若在 Wait/事件锁 等地方阻塞,任务已死但仍占 activeIDs → 目录锁与并行
// 槽位死占,step_timeout(默认 60 分钟)的 WithTimeout 只管击杀 invoke,不管收尾 goroutine 泄漏。
// harvest 早收割处理"结果在手不退"(看得见的完成态),patrol 处理"执行器已死但 runTask 未收尾"的
// 收尾僵态——两轴正交,同样都用整组击杀但触发条件不同。
// 【R2.2 起触发面严格 = pgDeadTooLong】"活但静默"(如 SIGSTOP 挂起、silently 死等、长步无输出)
// 交给 invoke 的 WithTimeout(StepTimeoutMin) 兜底,patrol 只处理 procgroup 已死透且死超 pgGrace 的
// 收尾僵态;心跳降为已死后的 reason 分类信号,不再单独触发。理由见判据处注释。
//
// 【为什么不新增守护进程】
// tick 的 drain 循环本身每 DrainRescanSec(默认 15s)重扫一次做取消对账,巡逻贴附在这一循环上零
// 新增常驻进程;整组击杀沿用 activeCancels 的机制,不引入第二套击杀路径。
//
// 【心跳降为 reason 分类信号——CG-5 R2.2 修的 P1】
// R2.1 把 noHeartbeat 触发条件叠加的是 pgDead(raw pgSeenAlive && !alive) 而非 pgDeadTooLong,
// 可绕过 pgGrace 保护窗提前开火:step_timeout≥70min 时步超时击杀的 WaitDelay 10s 窗口或步间
// invoke 切换 procgroup 短暂死透(<60s),patrol 15s 一轮约 2/3 概率落窗 → noHeartbeat 命中却
// pgDeadTooLong 未成立 → 立即 evStalled + cancel → finalizeCanceled 永久取消归档健康 opus 重卡。
// R2.2 修法:触发面严格 = pgDeadTooLong 单一条件——pgGrace 保护对心跳通道同样生效;心跳超阈值
// 只在 pgDeadTooLong 已成立时用于把事件从 procgroup_dead 升级为 procgroup_dead_and_no_heartbeat,
// 不再是独立触发。procgroup 存活始终是**授权凭证**——伪心跳(测试脚本刷日志但进程组已死)不能
// 骗过巡逻,反例仍报红。
// R2.1 的动机(单步执行期日志天然零增长会误杀健康 opus 重卡——runner.go:95-97 stdout 收进内存
// buffer,logBlock 只在 invoke 返回后追加)仍然成立,只是当时的实现留了漏洞。
//
// 【两条信号如何组合】
//   1) 进程组存活:anyTaskProcAlive 查该任务当前有无活着的执行器 pid(登记 + processAlive 双查)。
//   2) 心跳:任务日志文件 size 增长(执行器每步的 logBlock 会持续追加输出——只在多步任务的步骤边界
//      才有增长可查)。
// 唯一触发条件:pgDeadTooLong——曾活过 → 现死透 → 死超 pgGrace(允许步骤间隙 invoke 切换)。
// 心跳只在触发后区分 reason:staleness ≥ heartbeatTimeout 时报 procgroup_dead_and_no_heartbeat,
// 否则报 procgroup_dead。
// 心跳单独判据不足以证明存活——只能证明"有人在写日志",不能证明"执行器还在跑",且反过来"没人写
// 日志"也不能证明"执行器已死"(单步内内存 buffer 不刷盘)。
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
// heartbeatTimeout:允许任务日志多久没增长。CG-5 R2.2 起仅作 procgroup 死透后的 reason 分类信号,
// 不再独立触发击杀;默认 70min = step_timeout_min(60min 默认)+10min 余量。tick.go 入口按
// max(70min, cfg.StepTimeoutMin+10min) 提升——生产曾按 ~150min 步超时运行(proc.go:113/runner.go:268
// 注释证实),硬编码 70min 会让 step_timeout≥60min 的长步任务死透后一进 pgGrace 就被误归为
// no_heartbeat 类,审计视图把健康长步的正常收尾误染成"卡死"证据。伸缩后阈值恒 ≥ step_timeout+10min。
// pgGrace:进程组"暂空"允许多久——runTask 步骤间隙(saveTask + emit 事件之类)、invoke → invoke 切换,
// 会有秒级窗口 procgroup 是空的。取 60s 覆盖极端慢机器上的多步切换。是触发面的关键防线——
// 心跳通道也必须叠加 pgGrace(见 patrolOnce 判据处 R2.2 修正),否则 60s 内 pgDead 就能提前开火。
// eventCooldown:同一任务重复记 evStalled 事件的最短间隔——巡逻每 15s 一轮,击杀信号若被处理不及时
// (goroutine 阻塞在 Wait 内),下一轮又会 stalled;cooldown 挡重复事件把账本刷成噪声。
var (
	patrolHeartbeatTimeout = 70 * time.Minute // 生产:tick.go 入口按 max(70min, cfg.StepTimeoutMin+10min) 提升;测试用短值 override
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

		// 唯一触发面:pgDeadTooLong——执行器进程组曾活过但现在死透且死超 pgGrace——runTask 应当在
		// finalizeXxx 收尾把任务从 activeIDs 拿走;若还在,说明 runTask 的 goroutine 卡在了什么地方
		// (Wait 阻塞、事件锁抢占超时等),巡逻替它推一把。
		// 【CG-5 R2 P1 收紧】R2 R1 曾把 noHeartbeat 触发条件叠加的是 pgDead(raw pgSeenAlive && !alive),
		// 不是 pgDeadTooLong——可绕过 pgGrace 60s 保护窗提前开火:step_timeout≥70min 时步超时击杀的
		// WaitDelay 10s 窗口 或 步间 invoke 切换 procgroup 短暂死透(<60s),patrol 15s 一轮约 2/3 概率落
		// 窗 → noHeartbeat=true 而 pgDeadTooLong=false → 立即假 evStalled + cancel → runner.go ctx.Err()
		// → finalizeCanceled 把健康重卡永久取消归档;非默认配置(仓内 proc.go:113/runner.go:268 注释证实
		// 曾按 ~150min 步超时运行)下 P0-1 声称消灭的误杀走廊复活。修法:触发面严格 = pgDeadTooLong 单一
		// 条件(pgGrace 保护对心跳通道同样生效),心跳降为 procgroup 已死后的 reason 升级信号,不再是
		// 独立触发。"活但静默"场景交给 invoke 自带的 WithTimeout(StepTimeoutMin) 兜底,巡逻不插手。
		pgDeadTooLong := st.pgSeenAlive && !alive && !st.pgDeadSince.IsZero() && now.Sub(st.pgDeadSince) >= patrolPGGrace
		if !pgDeadTooLong {
			continue
		}

		// reason 分类:pgDeadTooLong 已成立(触发面),再看 heartbeat 是否也超阈值区分事件类型。
		// 【为什么心跳阈值仍要随 cfg.StepTimeoutMin 伸缩】阈值本身不再影响触发,只影响 reason 分类;但
		// 若阈值 < step_timeout,健康长步(单步内日志天然零增长——runner.go:95-97 stdout 收进内存 buffer)
		// 死透后一进 pgGrace 就被误归为 no_heartbeat 类,审计视图会把健康长步的正常收尾误染成"卡死"证据。
		// tick.go 在入口按 max(70min, StepTimeoutMin+10min) 提升,留 10min 余量兜底 step_timeout 内死于
		// pipe hold 的边角(此时 heartbeat 才是真信号)。
		noHeartbeat := now.Sub(st.lastLogGrow) >= patrolHeartbeatTimeout
		reason := "procgroup_dead"
		if noHeartbeat {
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

// updatePatrolHeartbeatTimeout 是 tick 入口调用的耦合 helper:让 patrolHeartbeatTimeout 与
// cfg.StepTimeoutMin 同步扩张。默认 60min 步超时 → 阈值 70min(留 10min 余量);150min 步超时 →
// 阈值 160min。取 max(70min, cfg+10min) 保底,不因 cfg 变小而降到 70min 以下——70min 是"心跳类
// reason 的最小可信区间",低于此值心跳分类会把正常长步的收尾误染成 no_heartbeat 疑云。
// 【为什么抽成 helper】tick 入口调用行简洁 + 单元测试可脱离 tick 完整环境直测(见 patrol_test.go
// 的 TestUpdatePatrolHeartbeatTimeoutScalesWithStepTimeoutMin)。
func updatePatrolHeartbeatTimeout(cfg *Config) {
	target := 70 * time.Minute
	if extended := time.Duration(cfg.StepTimeoutMin+10) * time.Minute; extended > target {
		target = extended
	}
	patrolHeartbeatTimeout = target
}
