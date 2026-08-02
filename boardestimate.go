package main

// boardestimate.go —— 项目进度的「含预估余量」口径（委托人 2026-08-02 追加；同日修正轮换用
// 派生耦合模型并把余量分摊进分桶）。
//
// 现状进度条的分母是**现有卡**：一个项目派了 12 张卡完成 9 张就显示 75%，但 cardex 的工作
// 模式会持续派生新卡（review_after 复审、修复轮、emit/装配产出），"75%" 常常高估完成度。
// 本文件给出第二个口径：把**预估还会派生的卡数**并入分母，让轴线更接近"这条线到头还有多远"。
//
// 【为什么是派生耦合几何模型，不是"根卡×膨胀率"】第一版用 未完结根卡×(系数−1)，它只看
// 当前根卡的直接衍生，看不见"派生的派生"（装配/协调卡一完成就 emit 一批新卡，每张又各自
// 长出审核/修复链）——预估总量随每波派生落地持续扩大，恰是委托人点名要修的观感（预估应该
// 提前把量估足，之后每次更新趋向完成，而不是分母越走越远）。派生耦合模型改问一个可测的
// 历史量：**每完成 1 张卡，平均派生出 k 张新卡**。要清完当前 A 张在途卡，未来还会诞生
// kA + k²A + …… = A·k/(1−k) 张（等比级数一次收敛）——把整条派生瀑布**前置**进预估。
// 由此得到委托人要的趋势性质：完成一张不派生的卡 → k 与 A 双降 → 预估总量缩减；完成一张
// 会派生的卡 → 新卡落地时它的量早已在 R 里，总量基本不动。**不承诺严格单调**：一个从没
// 发生过派生浪的年轻项目第一波 emit 仍会抬升预估（没有历史就没有预知）——那是真实信息，
// 靠假装单调把它抹平就是造读数；那种场景的正解是 planned_total_cards 计划锚点。
//
// 三条纪律（与看板"数据不足显式披露，绝不编造估算"同源——预估不是编造，前提是口径全披露）：
//  1. **零额度**：估算是纯本地推导，随每次快照即时重算——派生数据算在读侧，天然自校准，
//     不需要定时任务，也不烧任何模型调用。
//  2. **人工计划锚点优先**：board.json `projects.<id>.planned_total_cards` 声明的阶段性
//     计划量恒压过机械估算——阶段计划达成/调整时人工更新它，就是校准 hook。
//  3. **basis 必带**：估算值必须随口径说明（系数/样本/锚点来源/已知偏差方向）一起吐给前端，
//     样本不足时显式回落现存卡数并说明原因，绝不给一个来历不明的百分比。

import (
	"fmt"
	"math"
	"sort"
)

// ProjectEstimate 是一个项目的「含预估余量」口径。字段全量吐出（不 omitempty）：
// 前端切到预估口径时需要完整信息，Source/Basis 决定如何披露。
type ProjectEstimate struct {
	// EstimatedTotal 预估最终总卡数（≥ 现存非取消卡数）。预估口径的进度分母。
	EstimatedTotal int `json:"estimated_total"`
	// EstimatedRemaining 预估还会派生的卡数（EstimatedTotal − 现存非取消卡数）。
	EstimatedRemaining int `json:"estimated_remaining"`
	// Source 估算来源："planned"（board.json 计划锚点）/ "spawn_coupling"（派生耦合模型）/
	// "insufficient"（样本不足，回落现存卡数）/ "settled"（无在途卡，无余量可估）。
	Source string `json:"source"`
	// Basis 人话口径说明，前端原样呈现（悬停）。
	Basis string `json:"basis"`
}

// estimateMinCompletions / estimateMinSpawned 是启用派生耦合估算的最小样本量：完成数太少时
// 一两张卡的个体差异就能把 k 打飞，宁可显式说"样本不足"。
const (
	estimateMinCompletions = 5
	estimateMinSpawned     = 3
)

// estimateMaxCoupling 是 k 的收敛上限。k≥1 表示项目处于扩张期（每完成 1 张平均新增 ≥1 张），
// 等比级数不收敛、"最终总量"在数学上无定义——此时按 0.85 给出**收敛下限**并在 basis 里
// 明说扩张期身份，比吐一个无穷大或假装精确都诚实。
const estimateMaxCoupling = 0.85

// systemSpawnedCard 判定"系统派生卡"：复审卡（ReviewOf）、修复轮卡（FixRound>0）、交叉链
// 派生腿（XRole B/C）、以及带谱系标的 emit 产出/收口卡/升级卡（EmittedBy，2026-08-02 起
// 新卡生效）。其余视为人工立项（外生，不参与派生系数，也不被预测——"不预测新立项"）。
func systemSpawnedCard(t *Task) bool {
	return t != nil && (t.ReviewOf != "" || t.FixRound > 0 || t.XRole == "B" || t.XRole == "C" || t.EmittedBy != "")
}

// buildProjectEstimate 计算项目的预估口径。planned 是 board.json 的计划锚点（0=未声明）。
func buildProjectEstimate(ts []*Task, planned int) *ProjectEstimate {
	existing := 0 // 非取消现存卡（与 progressPercent 的分母口径一致）
	active := 0
	completions := 0 // done+failed：链走到头的卡（派生系数的分母）
	spawned := 0     // 系统派生卡（派生系数的分子）
	for _, t := range ts {
		if t.Status == statusCanceled {
			continue
		}
		existing++
		switch t.Status {
		case statusDone, statusFailed:
			completions++
		default:
			active++
		}
		if systemSpawnedCard(t) {
			spawned++
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

	if completions < estimateMinCompletions || spawned < estimateMinSpawned {
		return &ProjectEstimate{EstimatedTotal: existing, EstimatedRemaining: 0, Source: "insufficient",
			Basis: fmt.Sprintf("样本不足（完成 %d<%d 或系统派生 %d<%d），暂回落现存卡数口径；"+
				"历史积累后自动启用派生耦合估算，或在 board.json 声明 planned_total_cards 作计划锚点。",
				completions, estimateMinCompletions, spawned, estimateMinSpawned)}
	}

	// 派生耦合：k = 系统派生卡 / 完成卡（本项目全史）。清完在途 A 张的全部后续派生
	// = A·k/(1−k)（等比级数，含派生的派生，一次前置）。
	k := float64(spawned) / float64(completions)
	kEff := k
	expansion := ""
	if kEff > estimateMaxCoupling {
		kEff = estimateMaxCoupling
		expansion = fmt.Sprintf("注意：k=%.2f≥%.2f 属扩张期（每完成 1 张平均新增 ≥1 张），"+
			"收敛型预估不适用，按 k=%.2f 给出**下限**。", k, estimateMaxCoupling, estimateMaxCoupling)
	}
	spawn := int(math.Round(float64(active) * kEff / (1 - kEff)))
	if spawn < 0 {
		spawn = 0
	}
	est := existing + spawn
	basis := fmt.Sprintf("派生耦合估算：历史每完成 1 张平均派生 k=%.2f 张（系统派生 %d / 完成 %d）；"+
		"在途 %d 张 × k/(1−k) ≈ 全链余量 ~%d 张——含派生的派生（等比级数一次前置），完成不派生的卡"+
		"会让预估总量缩减。%s口径：只估现有工作的派生瀑布，不预测人工新立项；谱系标（emitted_by）"+
		"2026-08-02 起新卡生效，存量 emit 产出缺标会低估 k（预估偏保守）。每次快照按最新历史重算"+
		"（自校准）；要更准的分母请在 board.json 声明 planned_total_cards。",
		k, spawned, completions, active, spawn, expansion)
	return &ProjectEstimate{EstimatedTotal: est, EstimatedRemaining: spawn,
		Source: "spawn_coupling", Basis: basis}
}

// annotateKindEstimates 把项目级预估余量按**历史派生构成**分摊进分桶（设计/落地/修复/审核/
// 协调），让分桶在预估口径下也有自己的分母（委托人 2026-08-02 修正轮点名）。
// 份额 = 各桶系统派生卡的历史占比（预估的余量本来就是"还会派生出来的卡"，用派生人口的
// 构成分它最对口）；无派生历史时回落现存构成。最大余数法分配，Σ 桶余量 ≡ 项目余量——
// 分桶与总条对不上账会被读成看板算错。分不到余量的桶两字段为 0（前端回落现有卡口径）。
func annotateKindEstimates(kinds []KindProgress, ts []*Task, kindOf map[string]kindMark, remaining int) {
	if remaining <= 0 || len(kinds) == 0 {
		return
	}
	spawnByKind := map[string]int{}
	existByKind := map[string]int{}
	spawnTotal, existTotal := 0, 0
	for _, t := range ts {
		if t.Status == statusCanceled {
			continue
		}
		key := kindOf[t.ID].Kind
		existByKind[key]++
		existTotal++
		if systemSpawnedCard(t) {
			spawnByKind[key]++
			spawnTotal++
		}
	}
	share, total := spawnByKind, spawnTotal
	if total == 0 {
		share, total = existByKind, existTotal
	}
	if total == 0 {
		return
	}
	// 最大余数法：先取整分配，再按小数部分从大到小补齐差额（并列按桶序，输出确定）。
	type frac struct {
		idx  int
		part float64
	}
	base := make([]int, len(kinds))
	sum := 0
	fracs := make([]frac, 0, len(kinds))
	for i, kp := range kinds {
		x := float64(remaining) * float64(share[kp.Key]) / float64(total)
		b := int(math.Floor(x))
		base[i] = b
		sum += b
		fracs = append(fracs, frac{idx: i, part: x - float64(b)})
	}
	sort.SliceStable(fracs, func(a, b int) bool { return fracs[a].part > fracs[b].part })
	for j := 0; sum < remaining && j < len(fracs); j++ {
		base[fracs[j].idx]++
		sum++
	}
	for i := range kinds {
		if base[i] <= 0 {
			continue
		}
		den := kinds[i].Stats.Total - kinds[i].Stats.Canceled
		kinds[i].EstimatedRemaining = base[i]
		kinds[i].EstimatedTotal = den + base[i]
	}
}
