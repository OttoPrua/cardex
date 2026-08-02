package main

// 终态事件成本遥测（retro-77 建议二，2026-08-02 监控 session 终裁采纳）的回归测试。
//
// 【场景来源】retro-77 样本：10 张卡有 9 张的事件账本查不到 cost_usd/turns——正常完成路径写了，
// 但 cancel / 超轮限升级 / 分类器直判 failed|held 这些**提前退出**路径只写 reason 就收工。
// 复盘按事件账本算账，于是这些卡的开销在报表里彻底消失，且消失了多少无从得知。
//
// 【纪律】终态事件二选一，没有第三种：带 cost_total+turns_total，或带 cost_unavailable 显式标记。
// 静默缺字段是本功能定义的最坏失败模式——它让不完整的统计看起来完整。

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// terminalEventTypes 是必须携带成本遥测的事件类型集合。
var terminalEventTypes = map[string]bool{evDone: true, evFailed: true, evCanceled: true, evHeld: true}

// assertCostTelemetry 断言一条事件满足"要么有数、要么有显式标记"，并返回它报的累计成本。
// unavailable 为 true 时表示走的是显式标记分支。
func assertCostTelemetry(t *testing.T, ev TaskEvent) (cost float64, unavailable bool) {
	t.Helper()
	_, hasCost := ev.Detail[evDetailCostTotal]
	_, hasTurns := ev.Detail[evDetailTurnsTotal]
	unavail, _ := ev.Detail[evDetailCostUnavailable].(bool)

	if unavail {
		if hasCost || hasTurns {
			t.Errorf("%s/%s 同时带 cost_unavailable 与成本数值，口径自相矛盾: %+v", ev.Type, ev.Actor, ev.Detail)
		}
		if reason, _ := ev.Detail[evDetailCostUnavailReason].(string); reason == "" {
			t.Errorf("%s/%s 带 cost_unavailable 却无 reason —— 复盘无从分列成因: %+v", ev.Type, ev.Actor, ev.Detail)
		}
		return 0, true
	}
	if !hasCost || !hasTurns {
		t.Fatalf("终态事件 %s/%s 既无 cost_total/turns_total 也无 cost_unavailable 标记 —— "+
			"静默缺字段让不完整的统计看起来完整: %+v", ev.Type, ev.Actor, ev.Detail)
	}
	c, _ := ev.Detail[evDetailCostTotal].(float64)
	return c, false
}

// ---- 单元层：helper 本身的两条分支 ----

func TestWithCostTelemetryBranches(t *testing.T) {
	t.Run("有用量: 落累计值", func(t *testing.T) {
		d := withCostTelemetry(map[string]any{"reason": "x"}, &Task{TurnsUsed: 3, CostUSD: 0.42})
		if d[evDetailCostTotal] != 0.42 || d[evDetailTurnsTotal] != 3 {
			t.Fatalf("累计用量未落盘: %+v", d)
		}
		if _, ok := d[evDetailCostUnavailable]; ok {
			t.Errorf("有数据不该带 unavailable 标记: %+v", d)
		}
		if d["reason"] != "x" {
			t.Errorf("原有 detail 字段被覆盖: %+v", d)
		}
	})

	t.Run("零用量: 落显式标记而非静默省字段", func(t *testing.T) {
		d := withCostTelemetry(nil, &Task{})
		if unavail, _ := d[evDetailCostUnavailable].(bool); !unavail {
			t.Fatalf("零用量必须落 cost_unavailable 显式标记: %+v", d)
		}
		if d[evDetailCostUnavailReason] != costUnavailNoUsage {
			t.Errorf("缺 reason，复盘无从分列: %+v", d)
		}
		if _, ok := d[evDetailCostTotal]; ok {
			t.Errorf("零用量不该伪造 cost_total=0（与'确实花了0'不可区分）: %+v", d)
		}
	})

	t.Run("nil 卡: 兜底也走显式标记", func(t *testing.T) {
		if unavail, _ := withCostTelemetry(nil, nil)[evDetailCostUnavailable].(bool); !unavail {
			t.Error("nil 任务应落显式标记")
		}
	})
}

// ---- 端到端：提前取消场景（本功能的承重验收）----

// fakeClaudeCancelOnNthCall 造一个假 claude：正常回成功结果，但在**第 n 次**被调用时先把盘上的
// 任务文件改成 canceled——模拟"人在第 n 步执行途中按下 cancel / tick 对账把 ctx 撤了"。
// 这样第 n-1 步的用量已经落到卡面，取消发生在其后：正是 retro-77 里丢账最多的那条路。
func fakeClaudeCancelOnNthCall(t *testing.T, root, taskID string, n int) string {
	t.Helper()
	dir := t.TempDir()
	counter := filepath.Join(dir, "calls")
	taskFile := filepath.Join(tasksDir(root), taskID+".json")
	script := "#!/bin/sh\n" +
		"n=$(cat " + shSingleQuote(counter) + " 2>/dev/null || echo 0)\n" +
		"n=$((n+1)); printf '%s' \"$n\" > " + shSingleQuote(counter) + "\n" +
		"if [ \"$n\" -eq " + strconv.Itoa(n) + " ]; then\n" +
		// 用 awk 而非 sed -i：-i 的就地语义在 BSD/GNU 之间不兼容（BSD 要求后备缀参数）。
		"  awk '{gsub(/\"status\": \"running\"/, \"\\\"status\\\": \\\"canceled\\\"\"); print}' " +
		shSingleQuote(taskFile) + " > " + shSingleQuote(taskFile+".tmp") + " && mv " +
		shSingleQuote(taskFile+".tmp") + " " + shSingleQuote(taskFile) + "\n" +
		"fi\n" +
		"cat <<'JSON_EOF'\n" + mkOKResultJSON("sess-cancel") + "\nJSON_EOF\n" +
		"exit 0\n"
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCanceledMidRunCarriesAccumulatedCost 是本功能的承重测试：一张卡跑完第 1 步（用量已落卡面）
// 后在第 2 步途中被取消，其 canceled 终态事件**必须**带上已烧掉的 cost/turns。
//
// 【突变致死】把 runner.go finalizeCanceled 的 withCostTelemetry(detail, t) 换回裸 detail → 报红。
func TestCanceledMidRunCarriesAccumulatedCost(t *testing.T) {
	root := testRoot(t)
	work := t.TempDir()
	// 先建卡拿到 ID，假 claude 才能定位任务文件。
	cfg := runTaskCfg(t, "placeholder")
	tk := newTask(root, cfg, typeSequence, "两步·第二步途中被取消", work, []string{"step-1", "step-2"}, 5)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	cfg.ClaudeBin = fakeClaudeCancelOnNthCall(t, root, tk.ID, 2)

	if err := runTask(context.Background(), root, cfg, tk, false); err != nil {
		t.Fatal(err)
	}

	// 取消卡会被归档，事件账本随之搬到 archive/events/。
	events, _, err := loadTaskEvents(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	var canceled *TaskEvent
	for i := range events {
		if events[i].Type == evCanceled {
			canceled = &events[i]
		}
	}
	if canceled == nil {
		t.Fatalf("提前取消应留一条 canceled 终态事件, got %v", eventTypes(events))
	}
	cost, unavailable := assertCostTelemetry(t, *canceled)
	if unavailable {
		t.Fatalf("第 1 步已烧掉 $0.01/1 turn，取消事件却报 cost_unavailable —— 这正是 retro-77 的丢账形态: %+v",
			canceled.Detail)
	}
	if cost <= 0 {
		t.Errorf("canceled 事件的 cost_total = %v, 应为第 1 步累计的 0.01", cost)
	}
	if turns, _ := canceled.Detail[evDetailTurnsTotal].(float64); turns != 1 {
		t.Errorf("canceled 事件的 turns_total = %v, 应为 1", turns)
	}
}

// TestCliCancelBeforeAnyStepMarksUnavailable：卡在产出任何一步之前被 cancel —— 零用量是**真实的**，
// 但仍必须落显式标记，不许静默省掉字段（否则复盘无法区分"没花钱"与"不知道花没花钱"）。
func TestCliCancelBeforeAnyStepMarksUnavailable(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	tk := newTask(root, cfg, typeSequence, "还没开跑就被取消", "/tmp", []string{"p"}, 5)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if err := cmdSetStatus([]string{"-root", root, tk.ID}, "cancel"); err != nil {
		t.Fatal(err)
	}
	events, _, err := loadTaskEvents(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	var canceled *TaskEvent
	for i := range events {
		if events[i].Type == evCanceled {
			canceled = &events[i]
		}
	}
	if canceled == nil {
		t.Fatalf("cli cancel 应留 canceled 事件, got %v", eventTypes(events))
	}
	if _, unavailable := assertCostTelemetry(t, *canceled); !unavailable {
		t.Errorf("零用量卡应落 cost_unavailable 显式标记: %+v", canceled.Detail)
	}
}

// TestCliCancelAfterSpendCarriesCost：cli:cancel 路径（人手动取消一张已烧过钱的卡）同样不许丢账。
// 【突变致死】把 main.go cmdSetStatus 的 withCostTelemetry 换回裸 map → 报红。
func TestCliCancelAfterSpendCarriesCost(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	tk := newTask(root, cfg, typeSequence, "烧过钱再被人取消", "/tmp", []string{"a", "b"}, 5)
	tk.Step = 1
	tk.TurnsUsed = 4
	tk.CostUSD = 1.25
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if err := cmdSetStatus([]string{"-root", root, tk.ID}, "cancel"); err != nil {
		t.Fatal(err)
	}
	events, _, err := loadTaskEvents(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type != evCanceled {
			continue
		}
		cost, unavailable := assertCostTelemetry(t, ev)
		if unavailable {
			t.Fatalf("卡面有 $1.25/4 turns，取消事件却报无数据: %+v", ev.Detail)
		}
		if cost != 1.25 {
			t.Errorf("cost_total = %v, 应为 1.25", cost)
		}
		return
	}
	t.Fatalf("未找到 canceled 事件: %v", eventTypes(events))
}

// findTaskByTitlePrefix 从盘上在队卡里取标题前缀匹配的那张（多张匹配即测试前提出错，直接 Fatal）。
func findTaskByTitlePrefix(t *testing.T, root, prefix string) *Task {
	t.Helper()
	var hit *Task
	for _, x := range listQueued(t, root) {
		if strings.HasPrefix(x.Title, prefix) {
			if hit != nil {
				t.Fatalf("标题前缀 %q 命中多张卡，测试前提不成立", prefix)
			}
			hit = x
		}
	}
	if hit == nil {
		t.Fatalf("未找到标题前缀为 %q 的卡", prefix)
	}
	return hit
}

// escalationHeldEvent 取升级卡的 held 事件（无则 Fatal）。
func escalationHeldEvent(t *testing.T, root, escID string) TaskEvent {
	t.Helper()
	events, _, err := loadTaskEvents(root, escID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type == evHeld {
			return ev
		}
	}
	t.Fatalf("升级卡应有 held 事件: %v", eventTypes(events))
	return TaskEvent{}
}

// TestEscalationHeldSeparatesShellAndChainCost：超轮限升级路径。升级卡是刚出生的壳卡（零用量是
// 真实的，落显式标记），链累计开销另记 chain_cost_total —— 两组键分开，谁也不冒充谁。
//
// 【为什么必须分开】复盘按卡求和取 cost_total。若把链账写进升级卡的 cost_total，同一笔开销会被
// 实现卡与升级卡各记一次，总额凭空翻倍；若只写链账不写标记，壳卡又变成"成本不明"的噪声。
//
// 本例是**单卡链**（实现卡 2.5 + 审核卡 0.4，无中间修复轮），只钉"壳账 ≠ 链账"这一分列语义；
// 多轮链的累加口径由 TestChainCostAccumulatesAcrossFixRounds 钉住。
func TestEscalationHeldSeparatesShellAndChainCost(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	impl := mkImplTask(t, root, cfg)
	impl.FixRound = 3 // 本次判定 R4 > 全局兜底 3 → 超轮限
	impl.CostUSD = 2.5
	impl.TurnsUsed = 9
	if err := saveTask(root, impl); err != nil {
		t.Fatal(err)
	}
	rv := mkReviewTask(t, root, cfg, impl)
	rv.CostUSD = 0.4 // 本轮审核卡自身开销也属于这条链
	rv.TurnsUsed = 2
	handleReviewVerdict(root, cfg, rv, reviewReport, nil)

	esc := findTaskByTitlePrefix(t, root, "[超轮限R4·需人裁]")
	ev := escalationHeldEvent(t, root, esc.ID)
	if _, unavailable := assertCostTelemetry(t, ev); !unavailable {
		t.Errorf("升级壳卡从未执行，其自身 cost 应为显式 unavailable: %+v", ev.Detail)
	}
	if chain, _ := ev.Detail["chain_cost_total"].(float64); chain != 2.9 {
		t.Errorf("chain_cost_total = %v, 应为实现卡 2.5 + 审核卡 0.4 = 2.9", chain)
	}
	if turns, _ := ev.Detail["chain_turns_total"].(float64); turns != 11 {
		t.Errorf("chain_turns_total = %v, 应为 9+2=11", turns)
	}
	// 链账同时钉进升级卡卡面：人裁后从升级卡续出的新一轮不该让链账从 0 重新起算。
	if esc.ChainCostUSD != 2.9 || esc.ChainTurnsUsed != 11 {
		t.Errorf("升级卡卡面链账 = %v/%v, 应为 2.9/11", esc.ChainCostUSD, esc.ChainTurnsUsed)
	}
}

// TestChainCostAccumulatesAcrossFixRounds 是 chain_cost_total "链累计"口径的承重靶。
//
// 【它证伪什么】复审 P1-1：落盘值曾直接取 orig.CostUSD，而 orig 只是**最后一轮**被审的修复卡；
// 修复卡由 newTask 全新建卡、CostUSD 从 0 起算，于是"链累计"实际只是残值。本例造一条三轮真链，
// 每一环的用量都不同且互不整除，任何"只取最后一环""漏掉审核卡""不继承链账"的实现都算不出终值。
//
// 链形状（上限钉 2，第 3 轮判定即超限）：
//
//	实现卡 3.00/10 → 审核R1 0.10/1 → 修复R1 2.00/7 → 审核R2 0.20/2 → 修复R2 0.50/3 → 审核R3 0.05/1
//	链累计 = 3.00+0.10+2.00+0.20+0.50+0.05 = 5.85 美元 / 10+1+7+2+3+1 = 24 turns
//
// 【突变致死】改成 orig.CostUSD → 0.50（红）；漏 t.CostUSD → 5.80（红）；
// 修复卡不继承 ChainCostUSD → 0.55（红）；漏 orig.CostUSD → 3.85（红）。
func TestChainCostAccumulatesAcrossFixRounds(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()

	impl := mkImplTask(t, root, cfg)
	impl.MaxFixRounds = 2 // 卡面钉死上限 2：第 3 轮判定即超限，链长可控
	impl.CostUSD = 3.00
	impl.TurnsUsed = 10
	if err := saveTask(root, impl); err != nil {
		t.Fatal(err)
	}

	// 一轮 = 派审核卡（带自身开销）→ 消费 verdict → 拿到下一张修复卡并给它记上自身开销。
	round := func(reviewed *Task, revCost float64, revTurns int, fixCost float64, fixTurns int, wantTitle string) *Task {
		t.Helper()
		rv := mkReviewTask(t, root, cfg, reviewed)
		rv.MaxFixRounds = reviewed.MaxFixRounds
		rv.CostUSD = revCost
		rv.TurnsUsed = revTurns
		handleReviewVerdict(root, cfg, rv, reviewReport, nil)
		fix := findTaskByTitlePrefix(t, root, wantTitle)
		fix.CostUSD = fixCost
		fix.TurnsUsed = fixTurns
		if err := saveTask(root, fix); err != nil {
			t.Fatal(err)
		}
		return fix
	}

	fix1 := round(impl, 0.10, 1, 2.00, 7, "修复R1: ")
	if fix1.ChainCostUSD != 3.10 || fix1.ChainTurnsUsed != 11 {
		t.Fatalf("修复R1 卡面链账 = %v/%v, 应为 实现卡 3.00 + 审核R1 0.10 = 3.10 / 11",
			fix1.ChainCostUSD, fix1.ChainTurnsUsed)
	}
	fix2 := round(fix1, 0.20, 2, 0.50, 3, "修复R2: ")
	if fix2.ChainCostUSD != 5.30 || fix2.ChainTurnsUsed != 20 {
		t.Fatalf("修复R2 卡面链账 = %v/%v, 应为 3.10+2.00+0.20 = 5.30 / 20",
			fix2.ChainCostUSD, fix2.ChainTurnsUsed)
	}

	// 第 3 轮：round=3 > MaxFixRounds=2 → 挂升级卡，链账落进 held 事件。
	rv3 := mkReviewTask(t, root, cfg, fix2)
	rv3.MaxFixRounds = fix2.MaxFixRounds
	rv3.CostUSD = 0.05
	rv3.TurnsUsed = 1
	handleReviewVerdict(root, cfg, rv3, reviewReport, nil)

	esc := findTaskByTitlePrefix(t, root, "[超轮限R3·需人裁]")
	ev := escalationHeldEvent(t, root, esc.ID)
	chain, _ := ev.Detail["chain_cost_total"].(float64)
	// 浮点累加：用容差比较，别把 IEEE754 尾差当契约违规。
	if diff := chain - 5.85; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("chain_cost_total = %v, 应为整条链累计 5.85（实现 3.00 + 三张审核 0.35 + 两张修复 2.50）", chain)
	}
	if turns, _ := ev.Detail["chain_turns_total"].(float64); turns != 24 {
		t.Errorf("chain_turns_total = %v, 应为 24", turns)
	}
	// 反向锚：升级壳卡自身仍是零用量（链账绝不冒充卡账）。
	if _, unavailable := assertCostTelemetry(t, ev); !unavailable {
		t.Errorf("升级壳卡自身应为显式 unavailable，链账不得冒充卡账: %+v", ev.Detail)
	}
}

// ---- held 提前退出点：本轮同类位点普查（复审 P1-2 + P2-1）----

// TestTombstoneExhaustedHeldCarriesAccumulatedCost 是复审 P1-2 的靶：一张已跑过步、卡面有累计
// cost/turns 的卡崩溃续跑，resume 墓碑 bound=2 耗尽 → runner.go 挂 held。这是 runner 内真实的
// 提前退出点，其 held 事件必须带上已烧掉的开销。
//
// 【突变致死】把 runner.go 该处的 withCostTelemetry(...) 换回裸 map → 报红（既无数值也无标记）。
func TestTombstoneExhaustedHeldCarriesAccumulatedCost(t *testing.T) {
	root := testRoot(t)
	cfg := runTaskCfg(t, fakeClaudeBin(t, mkOKResultJSON("sess-T"), "", 0))

	tk := newTask(root, cfg, typeSequence, "崩溃风暴撞墓碑上限", t.TempDir(), []string{"p1", "p2"}, 5)
	tk.Step = 1
	tk.MidStep = true
	tk.SessionID = "sess-T" // resuming=true
	// 盘上仍是 running：本次是"上一轮 runTask 中途崩溃遗留"，入口不 reset 墓碑（bound 保护生效）。
	tk.Status = statusRunning
	tk.CostUSD = 1.75 // 第 1 步已烧掉的真实开销
	tk.TurnsUsed = 6
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	// 预置一份 resume:1 已达 bound 的墓碑（模拟连撞两次崩溃）。
	crashed := errors.New("崩在注入途中")
	for i := 0; i < tombstoneMaxAttempts; i++ {
		_, _, _ = injectAtMostOnce(root, tk.ID, resumeKind(1), func() error { return crashed })
	}

	if err := runTask(context.Background(), root, cfg, tk, false); err != nil {
		t.Fatal(err)
	}
	if tk.Status != statusHeld {
		t.Fatalf("墓碑耗尽应升级 held, got %s", tk.Status)
	}
	events, _, err := loadTaskEvents(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type != evHeld || ev.Actor != "runner:tombstone" {
			continue
		}
		if ev.Detail["reason"] != "resume_tombstone_exhausted" {
			t.Errorf("原有 reason 字段被遥测覆盖: %+v", ev.Detail)
		}
		cost, unavailable := assertCostTelemetry(t, ev)
		if unavailable {
			t.Fatalf("卡面有 $1.75/6 turns，墓碑耗尽的 held 却报无数据 —— 这条崩溃链的开销在账本里蒸发了: %+v",
				ev.Detail)
		}
		if cost != 1.75 {
			t.Errorf("cost_total = %v, 应为卡面累计的 1.75", cost)
		}
		if turns, _ := ev.Detail[evDetailTurnsTotal].(float64); turns != 6 {
			t.Errorf("turns_total = %v, 应为 6", turns)
		}
		return
	}
	t.Fatalf("墓碑耗尽应留一条 runner:tombstone 的 held 事件: %v", eventTypes(events))
}

// TestCliHoldAfterSpendCarriesCost：cli:hold 此前落的是裸 nil detail。hold 只禁 running——一张
// limit_paused 且已烧过钱的卡可被人工 hold，不接遥测这笔开销就查不到（复审 P2-1 的同类位点）。
//
// 【突变致死】把 main.go cli:hold 的 withCostTelemetry(...) 换回 nil → 报红。
func TestCliHoldAfterSpendCarriesCost(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	tk := newTask(root, cfg, typeSequence, "撞限额后被人挂起", "/tmp", []string{"a", "b"}, 5)
	tk.Status = statusLimitPaused
	tk.Step = 1
	tk.CostUSD = 0.88
	tk.TurnsUsed = 3
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if err := cmdSetStatus([]string{"-root", root, tk.ID}, "hold"); err != nil {
		t.Fatal(err)
	}
	events, _, err := loadTaskEvents(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type != evHeld {
			continue
		}
		cost, unavailable := assertCostTelemetry(t, ev)
		if unavailable {
			t.Fatalf("卡面有 $0.88/3 turns，cli:hold 事件却报无数据: %+v", ev.Detail)
		}
		if cost != 0.88 {
			t.Errorf("cost_total = %v, 应为 0.88", cost)
		}
		return
	}
	t.Fatalf("hold 应留 held 事件: %v", eventTypes(events))
}

// TestAddHoldMarksUnavailable：add -hold 的新生卡零用量是**真实的**，但仍须落显式标记而非裸
// reason（同类位点普查，复审 P2-1）。
func TestAddHoldMarksUnavailable(t *testing.T) {
	root := testRoot(t)
	if err := saveConfig(root, defaultConfig("claude")); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	if err := cmdAdd([]string{"-root", root, "-dir", work, "-hold", "先挂着"}); err != nil {
		t.Fatal(err)
	}
	tasks := listQueued(t, root)
	if len(tasks) != 1 {
		t.Fatalf("应只有一张卡, got %d", len(tasks))
	}
	events, _, err := loadTaskEvents(root, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type != evHeld {
			continue
		}
		if _, unavailable := assertCostTelemetry(t, ev); !unavailable {
			t.Errorf("新生卡应落显式 cost_unavailable 标记: %+v", ev.Detail)
		}
		return
	}
	t.Fatalf("add -hold 应留 held 事件: %v", eventTypes(events))
}

// ---- 类闭合的机械靶：源码层普查所有终态事件 emit 点 ----

// TestEveryTerminalEmitSiteWrapsCostTelemetry 是"本仓每一个终态事件 emit 点都接了成本遥测"这条
// **全称声称**的挂靶测试（events.go / docs/guide.md 均按名引用它）。
//
// 【为什么用源码扫描而不是跑路径】行为测试只能覆盖它恰好跑到的那几条路径；本卡上一轮正是因此漏了
// runner:tombstone / cli:hold / cli:add / runner:emit 四处。这里直接解析本包所有非测试 .go 文件的
// AST，找出所有 emitTaskEvent(...) 调用，凡事件类型实参是 evDone/evFailed/evCanceled/evHeld 的，
// 其 detail 实参（第 7 个）必须是 withCostTelemetry(...) 调用——否则报红并点名文件:行号。
//
// 【它守什么】新增一个终态 emit 点却忘了接遥测 → 本测试立刻红，不必等复盘丢账才发现。
// 【它不守什么】它只认 emitTaskEvent 这一条写事件的通道（recordEvent 的唯一调用方就是它，见
// events.go），也不校验 withCostTelemetry 拿到的是不是"对的那张卡"——后者由上面各条行为测试负责。
func TestEveryTerminalEmitSiteWrapsCostTelemetry(t *testing.T) {
	const evTypeArg, detailArg = 2, 6 // emitTaskEvent(root, taskID, evType, actor, status, step, detail)

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("解析本包源码失败: %v", err)
	}
	terminal := map[string]bool{"evDone": true, "evFailed": true, "evCanceled": true, "evHeld": true}

	checked := 0
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				fn, ok := call.Fun.(*ast.Ident)
				if !ok || fn.Name != "emitTaskEvent" || len(call.Args) <= detailArg {
					return true
				}
				evIdent, ok := call.Args[evTypeArg].(*ast.Ident)
				if !ok || !terminal[evIdent.Name] {
					return true
				}
				checked++
				pos := fset.Position(call.Pos())
				wrapped := false
				if inner, ok := call.Args[detailArg].(*ast.CallExpr); ok {
					if id, ok := inner.Fun.(*ast.Ident); ok && id.Name == "withCostTelemetry" {
						wrapped = true
					}
				}
				if !wrapped {
					t.Errorf("%s:%d 的 %s 事件 detail 未包 withCostTelemetry —— "+
						"静默缺字段让不完整的统计看起来完整（终态事件二选一，没有第三种）",
						filepath.Base(pos.Filename), pos.Line, evIdent.Name)
				}
				return true
			})
		}
	}
	// 下界防"扫了个寂寞"：解析路径失效/参数序号写错会让 checked 归零而测试假绿。
	// 18 是本轮普查登记的位点数（词面表见本测试的扫描输出），只作下界，新增位点不必改这里。
	if checked < 18 {
		t.Fatalf("只扫到 %d 个终态 emit 点，远少于本轮登记的 18 个 —— 扫描逻辑失效，本测试已失去防线意义", checked)
	}
}

// ---- 全路径扫描：任何终态事件都不得静默缺字段 ----

// TestAllTerminalEventsCarryTelemetry 跑几条真实执行路径，对产出的**每一条**终态事件统一体检。
// 这是防回归的那道网：以后新增一条终态事件的 emit 点却忘了接遥测，只要它落在这些路径上就报红。
func TestAllTerminalEventsCarryTelemetry(t *testing.T) {
	paths := []struct {
		name  string
		build func(t *testing.T, root string) (*Config, *Task)
	}{
		{
			name: "顺利完成两步",
			build: func(t *testing.T, root string) (*Config, *Task) {
				cfg := runTaskCfg(t, fakeClaudeBin(t, mkOKResultJSON("s"), "", 0))
				tk := newTask(root, cfg, typeSequence, "ok", t.TempDir(), []string{"a", "b"}, 5)
				return cfg, tk
			},
		},
		{
			name: "重试耗尽转 failed",
			build: func(t *testing.T, root string) (*Config, *Task) {
				cfg := runTaskCfg(t, fakeClaudeBin(t, "", "boom", 1))
				cfg.MaxAttempts = 1
				tk := newTask(root, cfg, typeSequence, "fail", t.TempDir(), []string{"a"}, 5)
				return cfg, tk
			},
		},
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			root := testRoot(t)
			cfg, tk := p.build(t, root)
			if err := saveTask(root, tk); err != nil {
				t.Fatal(err)
			}
			if err := runTask(context.Background(), root, cfg, tk, false); err != nil {
				t.Fatal(err)
			}
			events, _, err := loadTaskEvents(root, tk.ID)
			if err != nil {
				t.Fatal(err)
			}
			seen := 0
			for _, ev := range events {
				if !terminalEventTypes[ev.Type] {
					continue
				}
				seen++
				assertCostTelemetry(t, ev)
			}
			if seen == 0 {
				t.Fatalf("该路径应产出至少一条终态事件: %v", eventTypes(events))
			}
		})
	}
}
