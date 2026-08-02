package main

// boardspend.go — 队列任务消耗视图：按时间窗口看「哪些模型、哪些卡烧掉了多少」。
//
// 【与 transcript 曲线的分工】燃尽页的 token 曲线扫 ~/.claude/projects，两条曲线
// 现在共用同一组时间窗口标签页，但它们**不是同一份账**，永远不该被读成同一个数：
//   - transcript 曲线：绝对 token 吞吐，**不分账号也不分来源**——那个目录里混着人在
//     Claude Code 里手敲的交互会话。窗口拉长要真金白银地扫盘（实测 24h≈104MB /
//     7d≈419MB / 30d≈1.06GB），撞上字节预算闸就会截断，故它必须自报 truncated。
//   - 本文件：**队列口径的花费**，一行是一张卡，交互会话根本不在里面；随卡长期留存，
//     任何窗口都零额外扫描。
// "最近 24 小时只看得到一个模型"当初就是这个分工没说清造成的误读——那 24 小时里
// 恰好只有 opus 在跑，而且多半来自交互会话，不是队列。
//
// 本文件的源是**任务卡自己的账**。每张卡跑完时 runner 会把 claude CLI 回报的
// total_cost_usd / num_turns 写回卡上（见 runner.go），这份账：
//   - 随卡长期留存（含 archive/），要看多久就能看多久，零额外扫描；
//   - 天然是队列口径——一张卡就是一次派发，交互会话根本不在里面；
//   - 快照已经把全部卡读进内存了，这里只是再过一遍，不碰磁盘。
//
// 【两条必须说出口的边界】
//   1. `cost_usd` 是 claude CLI 回报的 **API 等价成本**。订阅制下它不是实际扣款，
//      只是"这些活按 API 价该值多少钱"。当成账单看会得出一个吓人且错误的数字。
//   2. **codex / 远端 codex 卡不回报花费**，它们的 cost 恒为 0。实测 1423 张卡里
//      448 张没有花费数据。不把这个缺口的张数说出来，用户会把"codex 那半边没花钱"
//      当成事实——而那半边烧的是另一套额度。

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// spendTopN 是「按项目」表回吐的条数上限。
// 项目数量级远小于卡（实测十来个），这个闸基本不会触发；
// 留着是防某天目录归并出几百个假项目时把响应撑爆。触发时由 TopTruncated 披露。
const spendTopN = 40

// ModelSpend 是一个模型在窗口内的合计。
type ModelSpend struct {
	Model string `json:"model"`
	Tier  string `json:"tier"`
	// Tasks 只数**有花费数据**的卡：把 cost=0 的 codex 卡也算进来，
	// 会让"平均每卡花费"凭空腰斩。
	Tasks     int     `json:"tasks"`
	CostUSD   float64 `json:"cost_usd"`
	TurnsUsed int     `json:"turns_used"`
}

// ProjectSpend 是一个项目在窗口内的消耗。
//
// 【为什么是项目维度而不是逐卡】一个 30 天窗口里有近千张卡，逐卡表只能截到前几十行，
// 而那几十行往往是同一个项目的连续修复链——看完并不知道"钱花在哪条线上"。
// 项目是委托人实际做取舍的粒度（哪条线该停、哪条线该加码），也天然是稳定的聚合键。
type ProjectSpend struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Tasks 是窗口内该项目的卡数；Priced 是其中有花费数据的。两个都给，
	// 否则"这个项目只花了 $3"分不清是活少还是花费没记上（codex 侧不回报）。
	Tasks     int     `json:"tasks"`
	Priced    int     `json:"priced"`
	CostUSD   float64 `json:"cost_usd"`
	TurnsUsed int     `json:"turns_used"`
	// TopModel 是该项目里花得最多的模型，回答"这条线的钱主要烧在哪个档位上"。
	TopModel     string  `json:"top_model"`
	TopModelTier string  `json:"top_model_tier"`
	TopModelCost float64 `json:"top_model_cost_usd"`
}

// TaskSpend 是一个时间窗口内的队列消耗视图。
type TaskSpend struct {
	Range      string `json:"range"`
	RangeLabel string `json:"range_label"`
	// Since 是窗口起点；range=all 时为空串（表示"有史以来"，不是"从 0 时刻起"）。
	Since string `json:"since"`
	// Tasks 是窗口内的卡数；Priced/Unpriced 把"有没有花费数据"摊开——
	// 只给一个合计金额会让人以为那就是全部活的成本。
	Tasks     int            `json:"tasks"`
	Priced    int            `json:"priced"`
	Unpriced  int            `json:"unpriced"`
	CostUSD   float64        `json:"cost_usd"`
	TurnsUsed int            `json:"turns_used"`
	ByModel   []ModelSpend   `json:"by_model"`
	ByProject []ProjectSpend `json:"by_project"`
	ProjectsN int            `json:"projects_n"`
	// TopTruncated 表示按项目表被 spendTopN 截断了（前端必须说出来）。
	TopTruncated bool `json:"top_truncated"`
	// Basis 是口径披露，前端原样呈现。
	Basis string `json:"basis"`
}

// spendRange 是一个可选窗口。Hours<=0 表示"全部历史"。
type spendRange struct {
	Key   string
	Label string
	Hours int
}

// spendRanges 是前端标签页的取值。次序即标签次序：从最近到最远，最后是全量。
var spendRanges = []spendRange{
	{Key: "24h", Label: "最近 24 小时", Hours: 24},
	{Key: "7d", Label: "最近 7 天", Hours: 24 * 7},
	{Key: "30d", Label: "最近 30 天", Hours: 24 * 30},
	{Key: "all", Label: "全部历史", Hours: 0},
}

// resolveSpendRange 把查询串映射成窗口。未知取值一律回落 24h（第一个），
// 不报错也不猜——这是个展示窗口，写错一个参数不该让整页 500。
func resolveSpendRange(key string) spendRange {
	for _, r := range spendRanges {
		if r.Key == key {
			return r
		}
	}
	return spendRanges[0]
}

// buildTaskSpend 从快照里的任务卡汇总窗口内的消耗。
//
// 时间维度用 **UpdatedAt** 而不是 CreatedAt：花费是在卡跑完那一刻产生并写回的，
// 按创建时间归档会把一张上周入队、今天才跑完的卡算进上周——那笔钱是今天花的。
func buildTaskSpend(cfg *Config, snap *boardSnapshot, key string, now time.Time) TaskSpend {
	r := resolveSpendRange(key)
	out := TaskSpend{
		Range: r.Key, RangeLabel: r.Label,
		ByModel: []ModelSpend{}, ByProject: []ProjectSpend{},
	}
	var cut time.Time
	if r.Hours > 0 {
		cut = now.Add(-time.Duration(r.Hours) * time.Hour)
		out.Since = cut.Format(time.RFC3339)
	}

	// task id → 项目。快照只有 项目→卡 的方向，按卡聚合要反着查。
	type projRef struct{ id, name string }
	projOf := make(map[string]projRef, len(snap.byID))
	for _, p := range snap.Projects {
		for _, t := range snap.projTasks[p.ID] {
			projOf[t.ID] = projRef{p.ID, p.Name}
		}
	}

	byModel := map[string]*ModelSpend{}
	byProj := map[string]*ProjectSpend{}
	// projModel 记每个项目内部的模型花费，用来挑 TopModel。
	projModel := map[string]map[string]float64{}

	for _, t := range snap.byID {
		if r.Hours > 0 {
			ts, ok := parseRFC3339(t.UpdatedAt)
			// 时间戳损坏/缺失的卡：窗口视图里只能丢掉（归不了档），
			// 但 all 窗口一定收得进来，不至于整张卡从所有视图消失。
			if !ok || ts.Before(cut) {
				continue
			}
		}
		out.Tasks++

		ref := projOf[t.ID]
		if ref.id == "" {
			ref = projRef{"(未归入项目)", "(未归入项目)"}
		}
		ps := byProj[ref.id]
		if ps == nil {
			ps = &ProjectSpend{ID: ref.id, Name: ref.name}
			byProj[ref.id] = ps
			projModel[ref.id] = map[string]float64{}
		}
		// 卡数在有没有花费之前就记：一个项目派了 80 张 codex 卡却显示"0 张"，
		// 会被读成这条线没在动。
		ps.Tasks++

		// 订阅引擎卡一律归 Unpriced：claude CLI 报的 total_cost_usd 是按 Anthropic 价目
		// 对供应商模型算的（未知模型多为 0，偶有别名误计价），与真实订阅成本口径不可比——
		// 混进 Priced 会污染"平均每卡花费"。与文件头 codex 卡 cost=0 的披露纪律同源。
		if t.CostUSD <= 0 || taskEngineName(cfg, t) != "" {
			out.Unpriced++
			continue
		}
		out.Priced++
		out.CostUSD += t.CostUSD
		out.TurnsUsed += t.TurnsUsed

		model, _ := effectiveModel(cfg, t)
		if model == "" {
			model = "(账号默认)"
		}
		m := byModel[model]
		if m == nil {
			m = &ModelSpend{Model: model, Tier: modelTier(cfg, model)}
			byModel[model] = m
		}
		m.Tasks++
		m.CostUSD += t.CostUSD
		m.TurnsUsed += t.TurnsUsed

		ps.Priced++
		ps.CostUSD += t.CostUSD
		ps.TurnsUsed += t.TurnsUsed
		projModel[ref.id][model] += t.CostUSD
	}
	out.CostUSD = round2(out.CostUSD)

	for _, m := range byModel {
		m.CostUSD = round2(m.CostUSD)
		out.ByModel = append(out.ByModel, *m)
	}
	for id, ps := range byProj {
		ps.CostUSD = round2(ps.CostUSD)
		// 挑该项目花得最多的模型；同额时按名字定序，免得自动刷新时来回跳。
		best, bestCost := "", -1.0
		for m, c := range projModel[id] {
			if c > bestCost || (c == bestCost && m < best) {
				best, bestCost = m, c
			}
		}
		if best != "" {
			ps.TopModel, ps.TopModelTier, ps.TopModelCost = best, modelTier(cfg, best), round2(bestCost)
		}
		out.ByProject = append(out.ByProject, *ps)
	}
	out.ProjectsN = len(out.ByProject)
	// 花得多的排前面；同额时按模型名，保证同一份数据每次的次序一致
	// （map 遍历顺序随机，不定序的话每 30 秒自动刷新表格行都会跳一次）。
	sort.Slice(out.ByModel, func(i, j int) bool {
		if out.ByModel[i].CostUSD != out.ByModel[j].CostUSD {
			return out.ByModel[i].CostUSD > out.ByModel[j].CostUSD
		}
		return out.ByModel[i].Model < out.ByModel[j].Model
	})
	// 花得多的排前面；同额时按 id，保证同一份数据每次次序一致
	// （map 遍历顺序随机，不定序的话每 30 秒自动刷新表格行都会跳一次）。
	sort.Slice(out.ByProject, func(i, j int) bool {
		if out.ByProject[i].CostUSD != out.ByProject[j].CostUSD {
			return out.ByProject[i].CostUSD > out.ByProject[j].CostUSD
		}
		return out.ByProject[i].ID < out.ByProject[j].ID
	})
	if len(out.ByProject) > spendTopN {
		out.ByProject, out.TopTruncated = out.ByProject[:spendTopN], true
	}
	out.Basis = spendBasis(&out)
	return out
}

// spendBasis 生成口径披露串。三件事必须说：钱是等价成本不是扣款、
// 有多少张卡没有花费数据、这份账与 transcript 曲线不是一回事。
func spendBasis(s *TaskSpend) string {
	var b strings.Builder
	b.WriteString("花费取自任务卡上 runner 回写的 cost_usd（claude CLI 的 total_cost_usd）——")
	b.WriteString("订阅制下这是 **API 等价成本**，不是实际扣款金额。")
	if s.Unpriced > 0 {
		b.WriteString(" 窗口内 ")
		b.WriteString(strconv.Itoa(s.Tasks))
		b.WriteString(" 张卡中有 ")
		b.WriteString(strconv.Itoa(s.Unpriced))
		b.WriteString(" 张没有花费数据（codex / 远端 codex 不回报花费，未跑或已取消的卡同样为空），")
		b.WriteString("它们烧的是另一套额度，**未计入**上面的合计。")
	}
	b.WriteString(" 时间按卡的 updated_at（跑完那一刻）归档；")
	b.WriteString("本表是**队列口径**，不含在 Claude Code 里手敲的交互会话——")
	b.WriteString("那部分只在上面的 transcript 曲线里出现。")
	return b.String()
}

// round2 保留两位小数——花费是钱，round1 会把 $0.04 和 $0.05 抹成同一个数。
// 只用于非负金额（cost_usd 恒 ≥0），故不处理负数的进位方向。
func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }
