package main

// boardkind.go — 任务「工作性质」维度（kind）：设计 / 落地 / 修复 / 审核 / 协调。
//
// 【为什么要有这一维】看板原来只有一条项目级进度条，分母是全部卡、分子是 done 卡。
// 一个真实项目里审核卡与修复卡合起来常占七成以上（实测某项目 430 张 design-review
// 对 800 张 sequence），而这两类卡的生命周期短、完成率天然高——于是「95% 完成」的
// 观感之下，真正的落地卡可能才走了四成。单一进度条不是算错，是**把三种性质完全不同
// 的活按张数等权平均**，读数方向性地偏乐观，正是"对后期工作过分低估"的来源。
//
// 拆分之后每类各报各的完成占比，总条仍在（口径不变），但读者一眼能看到
// 「审核 96% / 落地 41%」这种真实分布。
//
// 【分类纪律】
//  1. **结构信号优先，关键词垫底**。review_of / fix_round / type 是盘上的确定事实，
//     零歧义；标题关键词是启发式，只在结构信号全不命中时才用。每张卡都带 kind_source
//     如实交代这次判定是靠什么得出的（review_of / fix_round / type / title / override / default）。
//  2. **判不出的落到「落地」，不落「未分类」**。这与 phase 的"未分阶段"取舍相反，
//     是刻意的：本卡要防的失真方向是"低估剩余工作量"，把归不了类的活算成待落地的活
//     是往保守一侧偏；单独开一个"未分类"桶反而会让落地桶显得比实际更空。
//     kind_source="default" 已把这件事说清楚，不存在伪装成确定判定的问题。
//  3. **顺序即语义**。review 必须先于 fix 判：审核卡会继承被审卡的 fix_round，
//     「审核: 修复R3: …」若先判 fix 就会把审核卡计进修复桶。

import (
	"regexp"
	"strconv"
	"strings"
)

// 任务性质（kind）取值。进 JSON 契约（TaskBrief.kind / Project.kinds[].key），不得擅改。
const (
	kindDesign = "design"
	kindImpl   = "impl"
	kindFix    = "fix"
	kindReview = "review"
	kindCoord  = "coord"
)

// kindOrder 是展示顺序：按「设计 → 落地 → 修复 → 审核 → 协调」的工作流自然序，
// 而不是按张数排——张数排序会让审核桶常年霸占第一行，读者反而找不到落地这一行。
var kindOrder = []string{kindDesign, kindImpl, kindFix, kindReview, kindCoord}

var kindLabel = map[string]string{
	kindDesign: "设计",
	kindImpl:   "落地",
	kindFix:    "修复",
	kindReview: "审核",
	kindCoord:  "协调",
}

// validKind 供 board.json 的 kind_rules 校验用：写错的 kind 值必须被拒 + 披露，
// 不能静默当成某个默认值——那等于替用户改分类。
func validKind(k string) bool {
	_, ok := kindLabel[k]
	return ok
}

var (
	// kindReviewPrefixRe 是审核类卡的标题包装前缀。允许前置若干 [批注] 块——
	// 实测有 "[超轮限R4·需人裁] 审核: …" 这类先批注再包装的写法。
	kindReviewPrefixRe = regexp.MustCompile(`^\s*(?:\[[^\]]*\]\s*)*(?:对抗)?(?:审核|复审|复核|终审|裁决)\s*(?:[:：]|\s)`)
	// kindFixPrefixRe 是自动修复卡的轮次前缀（修复R1: / backfill修复: ）。
	// 必须带冒号：不带冒号的"修复"在正文里出现得太多（"…P0 修复方案"），会误吃实现卡。
	kindFixPrefixRe = regexp.MustCompile(`^\s*(?:\[[^\]]*\]\s*)*(?:backfill)?修复(?:R\d+)?\s*[:：]`)
	// kindCoordPrefixRe 是收口/进度这类记账卡的前缀（closeout 卡由 runner 自动派发，
	// 标题恒为 "收口: <根标题>"）。它们既不是设计也不是落地，计进落地桶会虚增落地分母。
	kindCoordPrefixRe = regexp.MustCompile(`^\s*(?:\[[^\]]*\]\s*)*(?:收口|进度|统筹|协调)\s*[:：]`)
)

// kindCoordTypes 是「协调/记账」性质的任务类型。这些卡不产出被审对象，
// 也不推进落地，单列一桶避免污染落地进度。
var kindCoordTypes = map[string]bool{
	typeCoordinate:   true, // 分工协调：产出任务分工并入队
	typeProgressPull: true, // 进度回收：--resume 拉结构化进度报告
	typeAssembly:     true, // 提示词装配
	"batch":          true, // 批量入队卡
}

// kindDesignHints 是「设计/调研」性质的标题关键词。
//
// 这份清单是**启发式**，故意保持短而具体：宁可漏判（落到「落地」桶，读数偏保守）
// 也不要误判（把落地卡算成设计卡会让落地桶虚空，读数偏乐观——正是本卡要根治的方向）。
// 命中后 kind_source="title"，前端可据此让用户知道这条判定的可信度低于结构信号。
// 拉丁词按小写全串匹配，中文词按原样子串匹配。
var kindDesignHints = []string{
	"设计", "方案", "规划", "调研", "选型", "架构", "评估", "蓝图", "草案", "立项", "盘点",
	"design", "spec", "rfc", "roadmap", "proposal", "blueprint", "research",
}

// boardKindRule 是 board.json 里的人工分类规则（projects.<id>.kind_rules）。
// 启发式再怎么调都会有判错的卡，给一个精确的人工出口比继续堆关键词更诚实。
type boardKindRule struct {
	// Match 是标题子串（大小写不敏感）或任务 ID 全串。
	Match string
	// Kind 必须是 kindLabel 的键之一，否则整条规则被拒 + 披露。
	Kind string
}

// kindMark 是一次分类的完整结果：判成什么 + 靠什么判的。
// 两者必须成对流转——只带 kind 不带 source 的话，「盘上确定事实」与「关键词猜的」
// 在界面上长得一模一样，那是把猜测伪装成事实。
type kindMark struct {
	Kind   string
	Source string
}

// deriveTaskKind 判定一张卡的工作性质。
//
// 判定顺序即优先级，改动顺序前先读文件头第 3 条纪律：
//  1. 人工规则（board.json kind_rules，首条命中即用）
//  2. 审核：x_role=C / review_of / type=design-review / 标题审核前缀
//  3. 修复：fix_round>0 / 标题修复前缀
//  4. 协调：coordinate / progress-pull / prompt-assembly / batch 类型，或收口/进度前缀
//  5. 设计：标题关键词
//  6. 兜底：落地
func deriveTaskKind(t *Task, rules []boardKindRule) kindMark {
	title := strings.TrimSpace(t.Title)
	lower := strings.ToLower(title)

	for _, r := range rules {
		if r.Match == "" {
			continue
		}
		if t.ID == r.Match || strings.Contains(lower, strings.ToLower(r.Match)) {
			return kindMark{Kind: r.Kind, Source: "override"}
		}
	}

	// ---- 审核（必须先于修复判：审核卡继承被审卡的 fix_round）----
	switch {
	case t.XRole == "C":
		// 交叉验证链的 C 卡是"引擎乙拿甲结论对抗式查漏"，性质就是审核。
		return kindMark{Kind: kindReview, Source: "x_role"}
	case t.ReviewOf != "":
		return kindMark{Kind: kindReview, Source: "review_of"}
	case t.Type == typeReview:
		return kindMark{Kind: kindReview, Source: "type"}
	case kindReviewPrefixRe.MatchString(title):
		return kindMark{Kind: kindReview, Source: "title"}
	}

	// ---- 修复 ----
	if t.FixRound > 0 {
		return kindMark{Kind: kindFix, Source: "fix_round"}
	}
	if kindFixPrefixRe.MatchString(title) {
		return kindMark{Kind: kindFix, Source: "title"}
	}

	// ---- 协调 / 记账 ----
	if kindCoordTypes[t.Type] {
		return kindMark{Kind: kindCoord, Source: "type"}
	}
	if kindCoordPrefixRe.MatchString(title) {
		return kindMark{Kind: kindCoord, Source: "title"}
	}

	// ---- 设计（启发式，垫底）----
	for _, hint := range kindDesignHints {
		if strings.Contains(lower, hint) {
			return kindMark{Kind: kindDesign, Source: "title"}
		}
	}

	return kindMark{Kind: kindImpl, Source: "default"}
}

// KindProgress 是一类工作性质的进度切片。字段名进 JSON 契约（Project.kinds[]）。
type KindProgress struct {
	Key             string     `json:"key"`
	Label           string     `json:"label"`
	Stats           boardStats `json:"stats"`
	ProgressPercent float64    `json:"progress_percent"`
	// ProgressPercent 与项目总条同口径（done/(total-canceled)），故两者可直接对读：
	// 总条 88% 而落地条 41% 就是"这个项目的乐观观感来自哪里"的直接答案。

	// EstimatedTotal / EstimatedRemaining 是「含预估余量」口径下本桶的分母与余量：
	// 项目级余量按历史派生构成分摊进桶（annotateKindEstimates），Σ 桶余量 ≡ 项目余量。
	// 0 = 本桶没分到余量（或整体处于卡口径），前端据此回落现有卡分母。
	EstimatedTotal     int `json:"estimated_total,omitempty"`
	EstimatedRemaining int `json:"estimated_remaining,omitempty"`
}

// buildKindProgress 把一批卡按性质分桶。
// 空桶不回吐——一个从没派过修复卡的项目不该显示"修复 0/0"，那是噪声不是信息。
func buildKindProgress(ts []*Task, kindOf map[string]kindMark) []KindProgress {
	byKind := map[string]*boardStats{}
	for _, t := range ts {
		k := kindOf[t.ID].Kind
		if k == "" {
			k = kindImpl // 理论上不会走到：kindOf 由同一批卡填充
		}
		if byKind[k] == nil {
			byKind[k] = &boardStats{}
		}
		byKind[k].add(t.Status)
	}
	out := make([]KindProgress, 0, len(kindOrder))
	for _, k := range kindOrder {
		s := byKind[k]
		if s == nil || s.Total == 0 {
			continue
		}
		out = append(out, KindProgress{
			Key:             k,
			Label:           kindLabel[k],
			Stats:           *s,
			ProgressPercent: progressPercent(s),
		})
	}
	return out
}

// parseKindRules 校验 board.json 里的 kind_rules，返回 (可用规则, 披露串)。
//
// 为什么坏规则要**逐条**拒而不是整块拒：kind_rules 是列表，一条写错就丢掉整个列表
// 会让另外九条正确规则一起静默失效；但被丢掉的那条必须出现在披露串里，
// 否则用户会对着一条"配了但没生效"的规则找半天——静默降级即造读数。
func parseKindRules(raw []boardOverrideKindRule) ([]boardKindRule, string) {
	var out []boardKindRule
	var bad []string
	for i, r := range raw {
		match := strings.TrimSpace(r.Match)
		kind := strings.TrimSpace(r.Kind)
		switch {
		case match == "":
			bad = append(bad, "第"+strconv.Itoa(i+1)+"条 match 为空")
		case !validKind(kind):
			bad = append(bad, "第"+strconv.Itoa(i+1)+"条 kind="+quoteShort(kind)+"不是合法取值")
		default:
			out = append(out, boardKindRule{Match: match, Kind: kind})
		}
	}
	if len(bad) == 0 {
		return out, ""
	}
	return out, "board.json kind_rules 有 " + strconv.Itoa(len(bad)) + " 条被跳过（其余仍生效）：" +
		strings.Join(bad, "；") + "；合法 kind 取值：" + strings.Join(kindOrder, " / ")
}

// quoteShort 给披露串里的用户输入加引号并截断，避免一条超长的手误值把告警刷爆。
func quoteShort(s string) string {
	r := []rune(s)
	if len(r) > 24 {
		s = string(r[:24]) + "…"
	}
	return `"` + s + `"`
}
