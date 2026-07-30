package main

import "fmt"

// ---- 卡级投入产出分档（stakes → 复核深度查表）----
//
// BD-44 承 2026-07-31 委托人指示（吸纳 PerlicaOptimize BD-27 执行档位 / L1.9 槽位查表思想）。
//
// 【要解决什么】"这张卡值不值得配一轮对抗复审、值不值得抬思考档"是投入产出判断。
// 以前它只存在于派卡人的纪律里：忘了加 -review-after，一张高风险卡就裸奔进主干；
// 顺手加上 -review-after，一张改错别字的卡也要烧一轮 fable 复审。把它做成一张三档查表，
// 派卡人只需回答"这卡多重要"，深度由表决定。
//
// 【为什么入队即钉】查表**只在 add 时执行一次**，结果固化到卡面（Task.ReviewAfter / Task.Effort）。
// 运行期一律不再回查 config——否则改一次 stakes_policy，队列里所有在跑/在等的卡的复核深度都会
// 静默变化，而卡面看不出任何差别（与 XFrozenEngine "入队即钉引擎身份" 同一纪律：防漂移）。
// Task.Stakes 只作审计留档，不是运行期判据。

const (
	stakesLow    = "low"
	stakesNormal = "normal"
	stakesHigh   = "high"
)

// StakesRule.Review 的取值域。
const (
	stakesReviewFollow = "follow" // 跟随 -review-after 的显式指定（空值同义）
	stakesReviewOn     = "on"     // 强制配对抗复审
	stakesReviewOff    = "off"    // 强制不配
)

var validStakes = map[string]bool{stakesLow: true, stakesNormal: true, stakesHigh: true}

// defaultStakesPolicy 是内置查表。委托人指定的三档语义：
//
//	low    → 不配复审（改错别字/加注释一类，复审收益低于成本）
//	normal → 跟随 -review-after 显式指定（保持既有默认行为，不改变任何存量派卡习惯）
//	high   → 强制配复审 + 思考档地板抬到 high
func defaultStakesPolicy() map[string]StakesRule {
	return map[string]StakesRule{
		stakesLow:    {Review: stakesReviewOff},
		stakesNormal: {Review: stakesReviewFollow},
		stakesHigh:   {Review: stakesReviewOn, DefaultEffort: "high"},
	}
}

// stakesRule 取该档位的规则：用户 config 里有就用用户的，没有回落内置默认。
// 【为什么要双层回落】loadConfig 从 defaultConfig 起手再 Unmarshal，用户表按键合并，
// 正常情况下三档都在。但用户可能显式写 `"stakes_policy": {"high": {...}}` 之后又手动删了
// 内置项、或从旧版 config 迁移——这时缺档必须有兜底，而不是让 add 拿到零值静默不查表。
func stakesRule(cfg *Config, stakes string) StakesRule {
	if cfg != nil {
		if r, ok := cfg.StakesPolicy[stakes]; ok {
			return r
		}
	}
	if r, ok := defaultStakesPolicy()[stakes]; ok {
		return r
	}
	return StakesRule{Review: stakesReviewFollow}
}

// effortRank 把思考等级映射成可比较的序（与 claude --effort / codex model_reasoning_effort 同序）。
// 未知/空 = 0，永远低于任何显式档位——所以"没设 effort"的卡会被地板抬起来。
func effortRank(effort string) int {
	switch effort {
	case "low":
		return 1
	case "medium":
		return 2
	case "high":
		return 3
	case "xhigh":
		return 4
	case "max":
		return 5
	}
	return 0
}

// applyStakes 查表并把结果固化到卡面。只在 add 入队时调用一次（见文件头【为什么入队即钉】）。
//
// explicitEffort 表示命令行显式给了 -effort：显式意图恒优先于查表的"缺省抬档"，
// 否则 `-stakes high -effort low`（人明确要省这一刀）会被表默默改写，命令行就不再可信。
//
// 查表值非法（review 写错、default_effort 不是合法档位）时**报错而非静默忽略**：add 是交互命令，
// 人当场就能看到；静默忽略会让一张写错的表长期以为在生效（护栏静默失效是最坏的失败模式）。
func applyStakes(t *Task, cfg *Config, stakes string, explicitEffort bool) error {
	if stakes == "" {
		stakes = stakesNormal
	}
	if !validStakes[stakes] {
		return fmt.Errorf("未知 stakes %q（可选: %s/%s/%s）", stakes, stakesLow, stakesNormal, stakesHigh)
	}
	t.Stakes = stakes

	r := stakesRule(cfg, stakes)
	switch r.Review {
	case stakesReviewOn:
		t.ReviewAfter = true
	case stakesReviewOff:
		t.ReviewAfter = false
	case stakesReviewFollow, "":
		// 不干预：保留 -review-after 的显式取值。
	default:
		return fmt.Errorf("config.stakes_policy.%s.review=%q 非法（可选: %s/%s/%s）",
			stakes, r.Review, stakesReviewOn, stakesReviewOff, stakesReviewFollow)
	}

	if r.DefaultEffort != "" {
		if !validEfforts[r.DefaultEffort] {
			return fmt.Errorf("config.stakes_policy.%s.default_effort=%q 非法（可选: low/medium/high/xhigh/max）",
				stakes, r.DefaultEffort)
		}
		// 只抬不降：类型默认已给到更高档（如 max）的卡不被这张表拉低。
		if !explicitEffort && effortRank(t.Effort) < effortRank(r.DefaultEffort) {
			t.Effort = r.DefaultEffort
		}
	}
	return nil
}
