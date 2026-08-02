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

// TestEscalationHeldSeparatesShellAndChainCost：超轮限升级路径。升级卡是刚出生的壳卡（零用量是
// 真实的，落显式标记），被审链撞墙前的开销另记 chain_cost_total —— 两组键分开，谁也不冒充谁。
//
// 【为什么必须分开】复盘按卡求和取 cost_total。若把链账写进升级卡的 cost_total，同一笔开销会被
// 实现卡与升级卡各记一次，总额凭空翻倍；若只写链账不写标记，壳卡又变成"成本不明"的噪声。
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
	handleReviewVerdict(root, cfg, rv, reviewReport, nil)

	var esc *Task
	for _, x := range listQueued(t, root) {
		if strings.Contains(x.Title, "超轮限") {
			esc = x
		}
	}
	if esc == nil {
		t.Fatal("R4 超轮限应挂升级卡")
	}
	events, _, err := loadTaskEvents(root, esc.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type != evHeld {
			continue
		}
		if _, unavailable := assertCostTelemetry(t, ev); !unavailable {
			t.Errorf("升级壳卡从未执行，其自身 cost 应为显式 unavailable: %+v", ev.Detail)
		}
		if chain, _ := ev.Detail["chain_cost_total"].(float64); chain != 2.5 {
			t.Errorf("chain_cost_total = %v, 应为被审链累计的 2.5", chain)
		}
		if turns, _ := ev.Detail["chain_turns_total"].(float64); turns != 9 {
			t.Errorf("chain_turns_total = %v, 应为 9", turns)
		}
		return
	}
	t.Fatalf("升级卡应有 held 事件: %v", eventTypes(events))
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
