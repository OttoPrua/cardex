package main

// stakes 分档修复轮限（retro-77 建议一，2026-08-02 监控 session 终裁采纳）的回归测试。
//
// 【场景来源】retro-77 样本：10 张高 effort 规格对齐类卡有 9 张撞在全局上限 3 上进人裁壳，
// 事后均判"壳清、工作在新链继续"——上限对该类卡偏紧。high 档因此放宽到 4，low/normal 不动。
//
// 【突变致死设计】本文件的断言全部钉**具体轮数**而非"比全局大"：把 defaultStakesPolicy 里
// high 档的 4 改成 3 或 5，或把 low/normal 从 0（跟随全局）改成任何非 0 值，都必须报红。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStakesDefaultMaxFixRoundsPinned 钉死内置表的分档轮限取值本身。
// 【突变致死】defaultStakesPolicy 的 high 档 MaxFixRounds 改成 3/5 → 红；
// low/normal 补上任何非 0 值 → 红（它们必须留 0 = 跟随全局，否则改全局配置对低档位失效）。
func TestStakesDefaultMaxFixRoundsPinned(t *testing.T) {
	p := defaultStakesPolicy()
	if got := p[stakesHigh].MaxFixRounds; got != 4 {
		t.Errorf("high 档内置 max_fix_rounds = %d, 应为 4（retro-77 终裁：高档位放宽一轮）", got)
	}
	for _, tier := range []string{stakesLow, stakesNormal} {
		if got := p[tier].MaxFixRounds; got != 0 {
			t.Errorf("%s 档内置 max_fix_rounds = %d, 应为 0（跟随全局；非 0 会让全局配置对该档静默失效）", tier, got)
		}
	}
}

// TestApplyStakesPinsMaxFixRounds 钉住 add 路径把**解析后的绝对轮数**固化到卡面。
// 【突变致死】applyStakes 改成只在 r.MaxFixRounds>0 时写卡面（低档位留 0 当哨兵）→ 全局改配置
// 会让在队低档卡的上限静默漂移，本测试的 wantGlobalDrift 子例报红。
func TestApplyStakesPinsMaxFixRounds(t *testing.T) {
	cases := []struct {
		name      string
		globalMax int
		stakes    string
		want      int
	}{
		{"high 档取分档值 4（不是全局 3）", 0, stakesHigh, 4},
		{"normal 档跟随全局默认 3", 0, stakesNormal, 3},
		{"low 档跟随全局默认 3", 0, stakesLow, 3},
		{"normal 档跟随显式全局值", 7, stakesNormal, 7},
		{"high 档的分档值压过全局值", 7, stakesHigh, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := defaultConfig("claude")
			cfg.MaxFixRounds = c.globalMax
			task := &Task{}
			if err := applyStakes(task, cfg, c.stakes, false); err != nil {
				t.Fatalf("applyStakes: %v", err)
			}
			if task.MaxFixRounds != c.want {
				t.Errorf("卡面 max_fix_rounds = %d, 应为 %d", task.MaxFixRounds, c.want)
			}
		})
	}
}

// TestStakesRuleMaxFixRoundsFieldLevelFallback 是字段级回落纪律在新字段上的复刻（承 CARDEX-5 修复轮 1）：
// 只覆写 high 档的一个字段，不得把同档其余字段打成零值。
//
// 【为什么这条必须有】JSON 合并粒度只到键。用户写 `{"high":{"max_fix_rounds":6}}`（只想再多给两轮）
// 时整条 high 规则被替换成 `{Review:"", DefaultEffort:"", MaxFixRounds:6}`——若不做字段级回落，
// **所有 -stakes high 的卡静默失去强制复审**，账面无任何报错。反向同理：只写 default_effort 会把
// 分档轮限打回 0（静默缩回全局 3）。护栏静默失效是本功能定义的最坏失败模式。
func TestStakesRuleMaxFixRoundsFieldLevelFallback(t *testing.T) {
	t.Run("只写 max_fix_rounds: review/effort 回落内置 high 档", func(t *testing.T) {
		cfg := defaultConfig("claude")
		cfg.StakesPolicy = map[string]StakesRule{stakesHigh: {MaxFixRounds: 6}}
		task := &Task{}
		if err := applyStakes(task, cfg, stakesHigh, false); err != nil {
			t.Fatalf("applyStakes: %v", err)
		}
		if !task.ReviewAfter {
			t.Error("只写 max_fix_rounds 后 high 档失去强制复审 —— 护栏静默解除")
		}
		if task.Effort != "high" {
			t.Errorf("思考档地板丢失: Effort = %q, 应为 high", task.Effort)
		}
		if task.MaxFixRounds != 6 {
			t.Errorf("用户写的 max_fix_rounds 未生效: %d, 应为 6", task.MaxFixRounds)
		}
	})

	t.Run("只写 default_effort: max_fix_rounds 回落内置 4 而非缩回全局 3", func(t *testing.T) {
		cfg := defaultConfig("claude")
		cfg.StakesPolicy = map[string]StakesRule{stakesHigh: {DefaultEffort: "xhigh"}}
		task := &Task{}
		if err := applyStakes(task, cfg, stakesHigh, false); err != nil {
			t.Fatalf("applyStakes: %v", err)
		}
		if task.MaxFixRounds != 4 {
			t.Errorf("分档轮限被部分覆写打掉: %d, 应回落内置 high 档的 4", task.MaxFixRounds)
		}
	})

	t.Run("整表 nil: 回落内置表", func(t *testing.T) {
		cfg := defaultConfig("claude")
		cfg.StakesPolicy = nil
		task := &Task{}
		if err := applyStakes(task, cfg, stakesHigh, false); err != nil {
			t.Fatalf("applyStakes: %v", err)
		}
		if task.MaxFixRounds != 4 {
			t.Errorf("stakes_policy=null 时分档轮限应回落内置 4, got %d", task.MaxFixRounds)
		}
	})
}

// TestApplyStakesRejectsNegativeMaxFixRounds：写错的表在 add 当场报错，不静默按默认放行。
func TestApplyStakesRejectsNegativeMaxFixRounds(t *testing.T) {
	cfg := defaultConfig("claude")
	cfg.StakesPolicy = map[string]StakesRule{stakesHigh: {Review: stakesReviewOn, MaxFixRounds: -1}}
	if err := applyStakes(&Task{}, cfg, stakesHigh, false); err == nil {
		t.Error("max_fix_rounds 为负应报错（静默放行 = 一张写错的表长期以为在生效）")
	}
}

// TestLoadConfigPartialHighTierKeepsWidenedRounds 走真实 config.json → loadConfig → add 的整条路。
// 单测 stakesRule 只证函数，这条证"用户真这么写"时分档轮限还在。
func TestLoadConfigPartialHighTierKeepsWidenedRounds(t *testing.T) {
	root := t.TempDir()
	body := `{"claude_bin":"claude","stakes_policy":{"high":{"default_effort":"xhigh"}}}`
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(root)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	// 前提：JSON 确实把 high 整条替换掉了（否则本测试测的不是这个洞）。
	if raw := cfg.StakesPolicy[stakesHigh]; raw.MaxFixRounds != 0 {
		t.Fatalf("前提不成立: 期望 JSON 把 high.max_fix_rounds 打成 0, got %d", raw.MaxFixRounds)
	}
	task := &Task{}
	if err := applyStakes(task, cfg, stakesHigh, false); err != nil {
		t.Fatalf("applyStakes: %v", err)
	}
	if task.MaxFixRounds != 4 {
		t.Errorf("部分覆写 high 档后分档轮限失效: %d, 应为 4", task.MaxFixRounds)
	}
}

// ---- 端到端：修复闭环真的按卡面轮限截断 ----

// TestFixLoopHonorsPinnedRoundLimit 是本功能的承重测试：R4 这一轮，钉了 4 轮的高档卡必须继续派
// 修复卡，钉了 3 轮的普通卡必须挂升级卡。两个分支同时断言才能锁死"分档确实生效且只对高档生效"。
//
// 【突变致死】把 defaultStakesPolicy 的 high 档 MaxFixRounds 改成 3 → high 子例挂升级卡，报红；
// 改成 5 → normal 子例不受影响但 high 子例的 R5 边界（下一个子测试）报红；
// 把 runner 的 taskMaxFixRounds(orig,…) 换回 cfg.MaxFixRounds → high 子例报红。
func TestFixLoopHonorsPinnedRoundLimit(t *testing.T) {
	cases := []struct {
		name         string
		pinnedRounds int
		round        int // 被审卡已完成的轮次；本次判定为 round+1
		wantEscalate bool
	}{
		{"高档卡钉 4 轮: R4 仍派修复卡", 4, 3, false},
		{"高档卡钉 4 轮: R5 才挂升级卡", 4, 4, true},
		{"普通卡钉 3 轮: R4 挂升级卡", 3, 3, true},
		{"普通卡钉 3 轮: R3 仍派修复卡", 3, 2, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := testRoot(t)
			cfg := testCfg() // 全局 MaxFixRounds=0 → 兜底 3；卡面钉死值必须压过它
			impl := mkImplTask(t, root, cfg)
			impl.FixRound = c.round
			impl.MaxFixRounds = c.pinnedRounds
			if err := saveTask(root, impl); err != nil {
				t.Fatal(err)
			}
			rv := mkReviewTask(t, root, cfg, impl)
			rv.MaxFixRounds = c.pinnedRounds
			if err := saveTask(root, rv); err != nil {
				t.Fatal(err)
			}
			handleReviewVerdict(root, cfg, rv, reviewReport, nil)

			var esc, fix *Task
			for _, x := range listQueued(t, root) {
				switch {
				case strings.Contains(x.Title, "超轮限"):
					esc = x
				case strings.HasPrefix(x.Title, "修复R"):
					fix = x
				}
			}
			if c.wantEscalate {
				if esc == nil {
					t.Fatalf("R%d 超卡面轮限 %d，应挂升级卡", c.round+1, c.pinnedRounds)
				}
				if fix != nil {
					t.Fatalf("超轮限后不该再派修复卡: %s", fix.Title)
				}
				if esc.MaxFixRounds != c.pinnedRounds {
					t.Errorf("升级卡应继承轮限 %d, got %d", c.pinnedRounds, esc.MaxFixRounds)
				}
			} else {
				if fix == nil {
					t.Fatalf("R%d 未超卡面轮限 %d，应继续派修复卡", c.round+1, c.pinnedRounds)
				}
				if esc != nil {
					t.Fatalf("未超轮限却挂了升级卡: %s", esc.Title)
				}
				// 不继承的话，下一轮 handleReviewVerdict 读到 0 会静默缩回全局 3。
				if fix.MaxFixRounds != c.pinnedRounds {
					t.Errorf("修复卡应继承轮限 %d, got %d —— 不继承则高档卡只有第一轮享受放宽上限",
						c.pinnedRounds, fix.MaxFixRounds)
				}
			}
		})
	}
}

// TestFixLoopLegacyCardFallsBackToGlobal：本改动之前入队的存量卡（卡面 max_fix_rounds=0）
// 必须继续按全局值截断，不能因为读不到卡面值就变成"无上限"。
func TestFixLoopLegacyCardFallsBackToGlobal(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	cfg.MaxFixRounds = 2
	impl := mkImplTask(t, root, cfg)
	impl.FixRound = 2 // 本次判定为 R3 > 全局 2
	impl.MaxFixRounds = 0
	if err := saveTask(root, impl); err != nil {
		t.Fatal(err)
	}
	rv := mkReviewTask(t, root, cfg, impl)
	handleReviewVerdict(root, cfg, rv, reviewReport, nil)

	found := false
	for _, x := range listQueued(t, root) {
		if strings.Contains(x.Title, "超轮限") {
			found = true
		}
	}
	if !found {
		t.Error("存量卡（卡面轮限 0）应回落全局 max_fix_rounds 截断，而非无上限打转")
	}
}
