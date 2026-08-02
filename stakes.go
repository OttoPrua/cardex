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
	// 跟随 -review-after 的显式指定。注意：**空串不再与它同义**——空串表示"该字段没写"，
	// 由 stakesRule 回落成内置表同档位的值（见 stakesRule 的【为什么档内还要逐字段回落】）。
	stakesReviewFollow = "follow"
	stakesReviewOn     = "on"     // 强制配对抗复审
	stakesReviewOff    = "off"    // 强制不配
)

var validStakes = map[string]bool{stakesLow: true, stakesNormal: true, stakesHigh: true}

// defaultMaxFixRounds 是全局兜底的修复轮次上限（config.max_fix_rounds 为 0 时取它）。
const defaultMaxFixRounds = 3

// defaultStakesPolicy 是内置查表。委托人指定的三档语义：
//
//	low    → 不配复审（改错别字/加注释一类，复审收益低于成本）
//	normal → 跟随 -review-after 显式指定（保持既有默认行为，不改变任何存量派卡习惯）
//	high   → 强制配复审 + 思考档地板抬到 high + 修复轮限放到 4
//
// 【为什么只有 high 抬轮限】retro-77（2026-08-02，监控 session 终裁采纳）样本：10 张高 effort
// 规格对齐类卡有 9 张撞在全局上限 3 上进人裁壳，事后均判"壳清、工作在新链继续"——对这类卡而言
// 上限 3 不是护栏而是噪声源，它没拦住任何打转，只是把同一件事换条链重跑。低/普通档保持跟随
// 全局（0 = 不覆盖）：低价值卡在实现层打转三轮就该停，多给一轮只是多烧一轮额度。
func defaultStakesPolicy() map[string]StakesRule {
	return map[string]StakesRule{
		stakesLow:    {Review: stakesReviewOff},
		stakesNormal: {Review: stakesReviewFollow},
		stakesHigh:   {Review: stakesReviewOn, DefaultEffort: "high", MaxFixRounds: 4},
	}
}

// globalMaxFixRounds 取全局兜底轮限（config 未配则 defaultMaxFixRounds）。
func globalMaxFixRounds(cfg *Config) int {
	if cfg != nil && cfg.MaxFixRounds > 0 {
		return cfg.MaxFixRounds
	}
	return defaultMaxFixRounds
}

// taskMaxFixRounds 取一张卡实际适用的轮限：**优先卡面钉死的值**，为 0 才回落全局。
//
// 【为什么以卡面为准】与 ReviewAfter/Effort 同一条"入队即钉"纪律（见文件头）：轮限在 add 时按
// stakes 查表定死，运行期不再回查 config——否则改一次 stakes_policy 或 max_fix_rounds，队列里
// 所有在跑/在等的修复链会静默换上限，而"这张卡还能修几轮"在卡面上看不出任何差别。
// 回落全局这一支服务两类卡：本改动之前入队的存量卡，以及不经 add 创建的派生卡（其轮限由
// handleReviewVerdict 沿修复链继承，见 runner.go）。
func taskMaxFixRounds(t *Task, cfg *Config) int {
	if t != nil && t.MaxFixRounds > 0 {
		return t.MaxFixRounds
	}
	return globalMaxFixRounds(cfg)
}

// stakesRule 取该档位的规则：**缺档回落整条内置默认，档内留空的字段逐个回落内置同档位的值**。
//
// 【为什么缺档要回落】loadConfig 从 defaultConfig 起手再 Unmarshal，用户表按键合并，
// 正常情况下三档都在。但用户可能显式写 `"stakes_policy": {"high": {...}}` 之后又手动删了
// 内置项、或从旧版 config 迁移、或写成 `"stakes_policy": null`（JSON null 把整张 map 打成 nil）——
// 这时缺档必须有兜底，而不是让 add 拿到零值静默不查表。
//
// 【为什么档内还要逐字段回落】JSON 的合并粒度是**键**：用户只写
// `{"stakes_policy":{"high":{"default_effort":"xhigh"}}}`（只想抬思考地板）时，整条 high 规则被替换成
// `{Review:"", DefaultEffort:"xhigh"}`——Review 空串按 follow 处理，于是**所有 -stakes high 的卡静默
// 失去强制复审**，账面无任何报错（结构合法、语义被截断，绕过了"查表值写错即报错"那条防线）。
// 护栏静默失效是本功能定义的最坏失败模式，所以留空 = 继承内置值，**要 follow 必须显式写 "follow"**
// （空串与 "follow" 在此处是两种不同的意图，不再同义）。
// 靶：TestStakesRuleFieldLevelFallback / TestLoadConfigPartialHighTierKeepsForcedReview。
//
// 【已知表达力缺口】`default_effort` 同理留空 = 继承，因此"高档位但不要思考地板"在 high 档无法表达
// （没有代表"无地板"的合法档位字面量）；需要时只能显式写一个更低的档位。`max_fix_rounds` 同理：
// 0 = 继承，所以"high 档跟随全局轮限"不能靠留空表达，只能把全局那个数显式写进 high 档。
// 两处登记在案，未做语法扩展。
func stakesRule(cfg *Config, stakes string) StakesRule {
	base, ok := defaultStakesPolicy()[stakes]
	if !ok {
		base = StakesRule{Review: stakesReviewFollow}
	}
	if cfg == nil {
		return base
	}
	r, ok := cfg.StakesPolicy[stakes]
	if !ok {
		return base
	}
	if r.Review == "" {
		r.Review = base.Review
	}
	if r.DefaultEffort == "" {
		r.DefaultEffort = base.DefaultEffort
	}
	if r.MaxFixRounds == 0 {
		r.MaxFixRounds = base.MaxFixRounds
	}
	return r
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
		// 空串这一支在现有取值域下取不到（stakesRule 已把留空字段回落成内置表的值，
		// 而内置三档的 Review 都非空）——留着是防御性兜底，不是活路径。
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

	// 修复轮限固化到卡面：**钉的是解析后的绝对轮数**，不是"0=跟随全局"这种延后判定的哨兵值。
	// 否则一张 -stakes low 的卡入队时 max_fix_rounds 留 0，跑到第 3 轮时人改了全局 max_fix_rounds，
	// 它的上限就静默变了——这正是入队即钉要防的漂移。钉绝对值后，卡面上直接看得见"还能修几轮"。
	if r.MaxFixRounds < 0 {
		return fmt.Errorf("config.stakes_policy.%s.max_fix_rounds=%d 非法（须 >0；0/不写 = 跟随全局 max_fix_rounds）",
			stakes, r.MaxFixRounds)
	}
	if r.MaxFixRounds > 0 {
		t.MaxFixRounds = r.MaxFixRounds
	} else {
		t.MaxFixRounds = globalMaxFixRounds(cfg)
	}
	return nil
}
