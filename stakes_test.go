package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDefaultStakesPolicyTable 逐格钉死内置查表。
// 【突变致死】这是本功能的**规格**本身：改错任何一格（low 变成配复审、high 不配复审、
// high 的地板从 high 掉到 medium、normal 从 follow 变成 on/off）都会在这里报红。
// 用逐格字面量而不是"遍历表看长度"——后者对值的变化完全不敏感。
func TestDefaultStakesPolicyTable(t *testing.T) {
	p := defaultStakesPolicy()
	if len(p) != 3 {
		t.Fatalf("内置查表应恰好三档(low/normal/high), got %d 档: %+v", len(p), p)
	}
	want := map[string]StakesRule{
		stakesLow:    {Review: stakesReviewOff},
		stakesNormal: {Review: stakesReviewFollow},
		stakesHigh:   {Review: stakesReviewOn, DefaultEffort: "high"},
	}
	for k, w := range want {
		got, ok := p[k]
		if !ok {
			t.Fatalf("内置查表缺档位 %q", k)
		}
		if got.Review != w.Review {
			t.Errorf("stakes_policy[%s].review = %q, 应为 %q", k, got.Review, w.Review)
		}
		if got.DefaultEffort != w.DefaultEffort {
			t.Errorf("stakes_policy[%s].default_effort = %q, 应为 %q", k, got.DefaultEffort, w.DefaultEffort)
		}
	}
	// defaultConfig 必须真的把这张表挂上去——否则 add 会走 stakesRule 的兜底路径，
	// 表看着对、生产里却没生效。
	cfg := defaultConfig("claude")
	if len(cfg.StakesPolicy) != 3 {
		t.Fatalf("defaultConfig 未挂载 stakes_policy: %+v", cfg.StakesPolicy)
	}
	if cfg.StakesPolicy[stakesHigh].Review != stakesReviewOn {
		t.Errorf("defaultConfig.stakes_policy.high.review = %q, 应为 %q",
			cfg.StakesPolicy[stakesHigh].Review, stakesReviewOn)
	}
}

// TestApplyStakesLookup 覆盖三档 × 显式意图的组合。
// 【突变致死】每个子用例断言的是"查表落到卡面的确切结果"，任何一格查表值被改都会红。
func TestApplyStakesLookup(t *testing.T) {
	cfg := defaultConfig("claude")

	cases := []struct {
		name           string
		stakes         string
		inReviewAfter  bool // 相当于命令行是否给了 -review-after
		inEffort       string
		explicitEffort bool
		wantStakes     string
		wantReview     bool
		wantEffort     string
	}{
		{"low: 强制不配复审(即便显式 -review-after)", stakesLow, true, "", false, stakesLow, false, ""},
		{"low: 没给 -review-after 也不配", stakesLow, false, "", false, stakesLow, false, ""},
		{"low: 不抬思考档", stakesLow, false, "medium", false, stakesLow, false, "medium"},
		{"normal: 跟随显式 -review-after=true", stakesNormal, true, "", false, stakesNormal, true, ""},
		{"normal: 跟随显式 -review-after=false", stakesNormal, false, "", false, stakesNormal, false, ""},
		{"normal: 不抬思考档", stakesNormal, false, "", false, stakesNormal, false, ""},
		{"high: 强制配复审(没给 -review-after 也配)", stakesHigh, false, "", false, stakesHigh, true, "high"},
		{"high: 缺省抬到 high", stakesHigh, false, "low", false, stakesHigh, true, "high"},
		{"high: 只抬不降(类型默认 max 保留)", stakesHigh, false, "max", false, stakesHigh, true, "max"},
		{"high: 显式 -effort 恒优先(不被地板改写)", stakesHigh, false, "low", true, stakesHigh, true, "low"},
		{"缺省档位=normal", "", true, "", false, stakesNormal, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			task := &Task{ReviewAfter: c.inReviewAfter, Effort: c.inEffort}
			if err := applyStakes(task, cfg, c.stakes, c.explicitEffort); err != nil {
				t.Fatalf("applyStakes 出错: %v", err)
			}
			if task.Stakes != c.wantStakes {
				t.Errorf("Stakes = %q, 应为 %q", task.Stakes, c.wantStakes)
			}
			if task.ReviewAfter != c.wantReview {
				t.Errorf("ReviewAfter = %v, 应为 %v", task.ReviewAfter, c.wantReview)
			}
			if task.Effort != c.wantEffort {
				t.Errorf("Effort = %q, 应为 %q", task.Effort, c.wantEffort)
			}
		})
	}
}

// TestApplyStakesRejectsBadInput 非法输入必须报错，而不是静默按默认放行——
// 静默放行会让一张写错的表长期以为在生效（护栏静默失效是最坏的失败模式）。
func TestApplyStakesRejectsBadInput(t *testing.T) {
	cfg := defaultConfig("claude")

	if err := applyStakes(&Task{}, cfg, "HIGH", false); err == nil {
		t.Error("大小写不符的 stakes 应报错(取值域是精确的 low/normal/high)")
	}
	if err := applyStakes(&Task{}, cfg, "critical", false); err == nil {
		t.Error("未知 stakes 应报错")
	}

	bad := defaultConfig("claude")
	bad.StakesPolicy = map[string]StakesRule{stakesHigh: {Review: "yes"}}
	if err := applyStakes(&Task{}, bad, stakesHigh, false); err == nil {
		t.Error("stakes_policy.high.review 非法取值应报错")
	}
	bad2 := defaultConfig("claude")
	bad2.StakesPolicy = map[string]StakesRule{stakesHigh: {Review: stakesReviewOn, DefaultEffort: "ultra"}}
	if err := applyStakes(&Task{}, bad2, stakesHigh, false); err == nil {
		t.Error("stakes_policy.high.default_effort 非法档位应报错(ultra 是 codex 特档,非 claude --effort 档)")
	}
}

// TestStakesRuleFallback 用户表覆盖内置表；缺档回落内置默认（而不是拿到零值静默不查表）。
func TestStakesRuleFallback(t *testing.T) {
	cfg := defaultConfig("claude")
	cfg.StakesPolicy = map[string]StakesRule{
		stakesLow: {Review: stakesReviewOn}, // 用户把 low 改成"也要复审"
	}
	task := &Task{}
	if err := applyStakes(task, cfg, stakesLow, false); err != nil {
		t.Fatalf("applyStakes: %v", err)
	}
	if !task.ReviewAfter {
		t.Error("用户 stakes_policy 未覆盖内置表: low 已配 review=on 却没配复审")
	}
	// 用户表里没写 high → 回落内置默认（强制复审 + 抬 high）。
	task2 := &Task{}
	if err := applyStakes(task2, cfg, stakesHigh, false); err != nil {
		t.Fatalf("applyStakes: %v", err)
	}
	if !task2.ReviewAfter || task2.Effort != "high" {
		t.Errorf("缺档未回落内置默认: ReviewAfter=%v Effort=%q", task2.ReviewAfter, task2.Effort)
	}
}

// TestLoadConfigMergesStakesPolicy 用户 config.json 只写一档时，其余档位必须仍是内置默认——
// 这依赖 loadConfig "从 defaultConfig 起手再 Unmarshal" + Go 往非 nil map 按键合并的行为。
// 一旦有人把 StakesPolicy 改成先置 nil 再解析，这里会红。
func TestLoadConfigMergesStakesPolicy(t *testing.T) {
	root := t.TempDir()
	body := `{"claude_bin":"claude","stakes_policy":{"low":{"review":"on"}}}`
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(root)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.StakesPolicy[stakesLow].Review != stakesReviewOn {
		t.Errorf("用户档位未生效: low.review=%q", cfg.StakesPolicy[stakesLow].Review)
	}
	if cfg.StakesPolicy[stakesHigh].Review != stakesReviewOn ||
		cfg.StakesPolicy[stakesHigh].DefaultEffort != "high" {
		t.Errorf("未写的档位应保留内置默认, got high=%+v", cfg.StakesPolicy[stakesHigh])
	}
	if cfg.StakesPolicy[stakesNormal].Review != stakesReviewFollow {
		t.Errorf("未写的档位应保留内置默认, got normal=%+v", cfg.StakesPolicy[stakesNormal])
	}
}

// ---- 复审位质量地板：review/merge 卡恒不被 budget 改道 ----

// TestQualityFloorNoDivertUnderCooldown 是委托人点名的负例：构造 claude 冷却场景，
// 断言 design-review 卡不被改道 codex，而同等条件的实现卡照常改道（证明场景本身确实"改道开着"，
// 否则一个恒 false 的谓词也能骗过测试）。
func TestQualityFloorNoDivertUnderCooldown(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	// claude 冷却中：这是触发 codex 改道的前提场景。
	setCooldown(root, now.Add(2*time.Hour).Unix(), "5h 限额用尽")
	if !loadCooldown(root).active(now) {
		t.Fatal("前置条件不成立: 冷却未生效, 后续断言就不是在测改道路径")
	}

	cfg := defaultConfig("claude")
	cfg.CodexBin = "/opt/homebrew/bin/codex"
	cfg.CodexFallback = true

	// 对照组：普通实现卡在冷却期应当改道 codex（否则本测试无区分力）。
	impl := &Task{Type: typeSequence, Prompts: []string{"实现 X"}, Model: "sonnet"}
	if !codexDivertOK(cfg, impl) {
		t.Fatal("对照组失效: 冷却期普通实现卡本应改道 codex")
	}

	// 被测：复审卡。**即便模型不在 no_fallback_models 里**也不得改道——
	// 按模型名的黑名单会被换模型绕过，这里按卡的角色兜底。
	review := &Task{Type: typeReview, Prompts: []string{"审这一版"}, Model: "sonnet"}
	if noFallback(cfg, review.Model) {
		t.Fatal("测试前提被破坏: sonnet 不该在 no_fallback_models 里, 否则测不出角色级护栏")
	}
	if codexDivertOK(cfg, review) {
		t.Error("design-review 卡在 claude 冷却期被改道 codex —— 复审位质量地板失效")
	}

	// 交叉验证合并/裁决卡（crosscheck-merge 模板，XRole=C）同属复审位。
	merge := &Task{Type: typeCrossCheck, XRole: "C", Prompts: []string{"合并甲乙结论"}, Model: "opus"}
	if codexDivertOK(cfg, merge) {
		t.Error("crosscheck-merge(XRole=C) 卡被改道 codex —— 复审位质量地板失效")
	}
	// 即便有人把合并卡的 type 改成非交叉类型（越过 typeCrossCheck 那条护栏），角色判据仍须挡住。
	mergeMislabeled := &Task{Type: typeSequence, XRole: "C", Prompts: []string{"合并甲乙结论"}, Model: "sonnet"}
	if codexDivertOK(cfg, mergeMislabeled) {
		t.Error("XRole=C 的卡仅靠 type 判据挡不住时被改道 —— 角色级地板未生效")
	}
}

// TestQualityFloorCard 直接钉住角色判据的取值域：多一个/少一个类型都会红。
func TestQualityFloorCard(t *testing.T) {
	floor := []*Task{
		{Type: typeReview},
		{XRole: "C"},
		{Type: typeCrossCheck, XRole: "C"},
	}
	for _, task := range floor {
		if !qualityFloorCard(task) {
			t.Errorf("应属复审位质量地板: type=%q x_role=%q", task.Type, task.XRole)
		}
	}
	notFloor := []*Task{
		{Type: typeSequence},
		{Type: typeAssembly},
		{Type: typeCoordinate},
		{Type: typeProgressPull},
		{Type: typeCrossCheck, XRole: "A"},
		{Type: typeCrossCheck, XRole: "B"},
	}
	for _, task := range notFloor {
		if qualityFloorCard(task) {
			t.Errorf("不应属复审位质量地板: type=%q x_role=%q", task.Type, task.XRole)
		}
	}
}

// TestCodexDivertOKGuards 其余改道前置条件不因新增地板而失守（回归护栏）。
func TestCodexDivertOKGuards(t *testing.T) {
	base := func() *Config {
		c := defaultConfig("claude")
		c.CodexBin = "codex"
		c.CodexFallback = true
		return c
	}
	ok := &Task{Type: typeSequence, Prompts: []string{"p"}, Model: "sonnet"}
	if !codexDivertOK(base(), ok) {
		t.Fatal("基线场景应允许改道")
	}
	off := base()
	off.CodexFallback = false
	if codexDivertOK(off, ok) {
		t.Error("codex_fallback=false 时不得改道")
	}
	nobin := base()
	nobin.CodexBin = ""
	if codexDivertOK(nobin, ok) {
		t.Error("codex_bin 为空时不得改道")
	}
	if codexDivertOK(base(), &Task{Type: typeSequence, Prompts: []string{"p"}, Model: "claude-fable-5"}) {
		t.Error("no_fallback_models 里的模型不得改道")
	}
	if codexDivertOK(base(), &Task{Type: typeSequence, Prompts: []string{"p"}, SessionID: "sess-1"}) {
		t.Error("带 claude 会话的卡不得改道(跨 CLI 无法延续上下文)")
	}
	if codexDivertOK(base(), &Task{Type: typeCrossCheck, XRole: "A", Prompts: []string{"p"}, Model: "opus"}) {
		t.Error("交叉验证卡不得改道(引擎身份就是交付物)")
	}
}
