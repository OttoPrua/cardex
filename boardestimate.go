package main

// boardestimate.go —— 项目进度的「含预估余量」口径（委托人 2026-08-02 追加）。
//
// 现状进度条的分母是**现有卡**：一个项目派了 12 张卡完成 9 张就显示 75%，但 cardex 的工作
// 模式会持续派生新卡（review_after 复审、修复轮、emit/装配产出），"75%" 常常高估完成度。
// 本文件给出第二个口径：把**预估还会派生的卡数**并入分母，让轴线更接近"这条线到头还有多远"。
//
// 三条纪律（与看板"数据不足显式披露，绝不编造估算"同源——预估不是编造，前提是口径全披露）：
//  1. **零额度**：估算是纯本地推导（历史膨胀率），随每次快照即时重算——派生数据算在读侧，
//     天然自校准，不需要定时任务，也不烧任何模型调用。这就是"定期刷新校准"的实现方式：
//     每一次快照都是一次校准（历史窗口滚动更新系数）。
//  2. **人工计划锚点优先**：board.json `projects.<id>.planned_total_cards` 声明的阶段性计划量
//     恒压过机械估算——阶段计划达成/调整时人工更新它，就是委托人所说的 hook。不用固定时间
//     刷新：cardex 是事件驱动的 tick 模型，固定时间在无活动时空转、密集期又滞后。
//  3. **basis 必带**：估算值必须随口径说明（样本量/系数/锚点来源/已知偏差方向）一起吐给前端，
//     样本不足时显式回落现存卡数并说明原因，绝不给一个来历不明的百分比。

import (
	"fmt"
	"math"
)

// ProjectEstimate 是一个项目的「含预估余量」口径。字段全量吐出（不 omitempty）：
// 前端切到预估口径时需要完整信息，Source/Basis 决定如何披露。
type ProjectEstimate struct {
	// EstimatedTotal 预估最终总卡数（≥ 现存非取消卡数）。预估口径的进度分母。
	EstimatedTotal int `json:"estimated_total"`
	// EstimatedRemaining 预估还会派生的卡数（EstimatedTotal − 现存非取消卡数）。
	EstimatedRemaining int `json:"estimated_remaining"`
	// Source 估算来源："planned"（board.json 计划锚点）/ "spawn_factor"（历史膨胀率）/
	// "insufficient"（样本不足，回落现存卡数）/ "settled"（无在途卡，无余量可估）。
	Source string `json:"source"`
	// Basis 人话口径说明，前端原样呈现（悬停）。
	Basis string `json:"basis"`
}

// estimateMinRoots / estimateMinDerived 是启用膨胀率估算的最小样本量：根卡太少时一条链的
// 个体差异就能把系数打飞，宁可显式说"样本不足"。
const (
	estimateMinRoots   = 5
	estimateMinDerived = 3
)

// estimateRootCard 判定"根卡"（种子）：不是复审卡（ReviewOf）、不是修复轮卡（FixRound>0）、
// 不是交叉链的派生腿（B/C；A 是链的种子）。其余全算派生卡——它们是队列自我繁殖的部分，
// 也正是"现有口径低估余量"的来源。
func estimateRootCard(t *Task) bool {
	return t != nil && t.ReviewOf == "" && t.FixRound == 0 && t.XRole != "B" && t.XRole != "C"
}

// buildProjectEstimate 计算项目的预估口径。planned 是 board.json 的计划锚点（0=未声明）。
func buildProjectEstimate(ts []*Task, planned int) *ProjectEstimate {
	existing := 0 // 非取消现存卡（与 progressPercent 的分母口径一致）
	active := 0
	roots := 0
	unfinishedRoots := 0
	for _, t := range ts {
		if t.Status == statusCanceled {
			continue
		}
		existing++
		if !t.terminal() {
			active++
		}
		if estimateRootCard(t) {
			roots++
			if !t.terminal() {
				unfinishedRoots++
			}
		}
	}

	// 计划锚点优先：阶段性计划就是最好的分母，机械估算只是没有计划时的替补。
	if planned > 0 {
		est := planned
		basis := fmt.Sprintf("计划锚点：board.json 声明计划总量 %d 张（planned_total_cards），现存 %d 张。"+
			"阶段计划达成/调整时请更新该值——这是预估口径的人工校准点。", planned, existing)
		if existing > planned {
			est = existing
			basis = fmt.Sprintf("计划锚点已被超出：声明计划 %d 张 < 现存 %d 张，分母按现存计；"+
				"计划量已过期，建议更新 board.json 的 planned_total_cards。", planned, existing)
		}
		return &ProjectEstimate{EstimatedTotal: est, EstimatedRemaining: est - existing,
			Source: "planned", Basis: basis}
	}

	// 无在途卡：不存在会继续派生的种子，余量为零（做完的项目不该显示幻影余量）。
	if active == 0 {
		return &ProjectEstimate{EstimatedTotal: existing, EstimatedRemaining: 0, Source: "settled",
			Basis: "无在途卡（全部终态），无衍生余量可估；预估口径与现存卡数一致。"}
	}

	derived := existing - roots
	if roots < estimateMinRoots || derived < estimateMinDerived {
		return &ProjectEstimate{EstimatedTotal: existing, EstimatedRemaining: 0, Source: "insufficient",
			Basis: fmt.Sprintf("样本不足（根卡 %d<%d 或衍生卡 %d<%d），暂回落现存卡数口径；"+
				"历史积累后自动启用膨胀率估算，或在 board.json 声明 planned_total_cards 作计划锚点。",
				roots, estimateMinRoots, derived, estimateMinDerived)}
	}

	// 历史膨胀率：全史（本项目非取消卡）口径 卡数/根卡数。系数含在途未走完的链，
	// 天然偏小 → 估算偏保守（宁可少报余量也不虚增），方向随 basis 披露。
	// 【公式自有界，无需另设封顶】spawn = 未完结根卡 × (系数−1) ≤ 根卡 × (现存/根卡 − 1)
	// = 现存 − 根卡 < 现存，故预估总量恒 < 2×现存——离群系数不可能把预估轴撑到不可读
	// （靶：TestEstimateSpawnBounded）。
	factor := float64(existing) / float64(roots)
	spawn := int(math.Round(float64(unfinishedRoots) * (factor - 1)))
	if spawn < 0 {
		spawn = 0
	}
	est := existing + spawn
	return &ProjectEstimate{EstimatedTotal: est, EstimatedRemaining: spawn, Source: "spawn_factor",
		Basis: fmt.Sprintf("按历史膨胀率估算：本项目 %d 卡 / %d 根卡 → 系数 %.2f；"+
			"未完结根卡 %d × (系数−1) ≈ 还会派生 %d 张。口径：只估现有工作的衍生量"+
			"（复审/修复轮/emit 产出），不预测新立项；系数含在途链故偏保守。"+
			"每次快照按最新历史重算（自校准）；要更准的分母请在 board.json 声明 planned_total_cards。",
			existing, roots, factor, unfinishedRoots, spawn)}
}
