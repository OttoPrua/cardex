package main

// boardweight.go —— 「工时进度」口径：按**工作量**而不是卡数算完成占比，并给出预估完成时刻。
// 委托人 2026-08-03 追加："一卡不同预估完成时间在百分比贡献中根据时间统计，方便让预估进度
// 更有参考价值。"
//
// 【为什么卡数口径会误导】实测本账本 1622 张 done 卡：按类型的中位 turns 是
// sequence 57 / design-review 34 / coordinate 8 / progress-pull 2——**相差 28 倍**；
// 多步卡中位 112，是单步卡（41）的 2.7 倍。于是"10 张卡完成 7 张 = 70%"这句话，在剩下 3 张
// 全是 sequence 大卡时严重高估，在剩下 3 张全是 progress-pull 时又严重低估。按工作量加权
// 才回答得了"还剩多少活"。
//
// 【权重用 turns 而不是时长】卡上**没有**执行时长字段（boardeta.go 文件头已论证：
// updated_at−created_at 大半是排队等待，拿它当耗时会离谱高估）。盘上真正被测量、且与
// "这张卡有多少活"强相关的量只有 turns（回合数）与 cost。turns 覆盖面更广（cost 对
// codex/引擎卡恒为 0），故取 turns 作**工作量代理**。它不是时长，所以：
//   - 百分比按"工作量占比"表述，不谎称"时间占比"；
//   - 换算成完成时刻时，用同一批样本实测的「每单位工作量耗多少墙钟分钟」，
//     这个换算率天然吸收了并行度、限额冷却与红线时段（与 paceModel 同一思路）。
//
// 【已知数据缺口，必须披露】codex / 远端 / 引擎卡不回报 turns（实测 24% 的 done 卡 turns=0）。
// 这些卡按同类型中位补一个预测值——补出来的是估计不是实测，方向未知（既可能高估也可能低估），
// basis 里写明占比。样本不足时整个口径回落卡数口径并说明，绝不给一个来历不明的百分比。

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// ProjectWeighted 是项目的工时口径读数。Available=false 时前端必须回落卡数口径并显示 Basis。
type ProjectWeighted struct {
	Available bool `json:"available"`
	// Percent 是工作量完成占比（done 权重 / 总权重，总权重含预估余量卡）。
	Percent float64 `json:"percent"`
	// DoneWeight / TotalWeight 单位见 Unit（当前恒为 "turns"）。
	DoneWeight  float64 `json:"done_weight"`
	TotalWeight float64 `json:"total_weight"`
	Unit        string  `json:"unit"`
	// MeasuredRatio 是 done 卡里**有实测 turns** 的比例（0..1）。低于 1 说明有卡是补的估计值。
	MeasuredRatio float64 `json:"measured_ratio"`
	// FinishAt / RemainMinutes 是按实测换算率推的完成时刻；换算率不可得时为 nil
	// （与 BoardETA 同纪律：数据不足序列化成 null，不给 0——0 会被画成"马上就完"）。
	FinishAt      *string  `json:"finish_at"`
	RemainMinutes *float64 `json:"remain_minutes"`
	Basis         string   `json:"basis"`
}

// weightMinSamples 是建权重表所需的最小实测样本数。低于它就没有"同类历史"可言，
// 整个口径回落卡数——宁可说数据不足，也不拿三五张卡的中位数去给几百张卡定权重。
const weightMinSamples = 12

// weightKey 是权重表的分桶键。type 与"是否多步"是**盘上事实**（不是猜的），
// 且实测这两个维度上的 turns 差异最大（类型间 28 倍、步数间 2.7 倍）。
// 不按 kind/model 再细分：桶一细样本就稀，稀桶的中位数比粗桶的更不可信。
type weightKey struct {
	typ   string
	multi bool
}

func weightKeyOf(t *Task) weightKey {
	return weightKey{typ: t.Type, multi: len(t.Prompts) > 1}
}

// weightTable 是「同类卡的典型工作量」查表，由本项目的实测 done 卡建成。
type weightTable struct {
	byKey    map[weightKey]float64
	overall  float64 // 全局中位，供无样本的桶回落
	measured int     // 参与建表的实测卡数
	total    int     // 考察过的 done 卡数（含无实测的）
}

// buildWeightTable 从 done 卡的实测 turns 建权重表。取中位数而非均值：turns 是重尾右偏
// （实测中位 41、均值 53、最大 447），均值会被少数超长卡拽高，让所有预测值系统性偏大。
func buildWeightTable(ts []*Task) *weightTable {
	samples := map[weightKey][]float64{}
	var all []float64
	total := 0
	for _, t := range ts {
		if t.Status != statusDone {
			continue
		}
		total++
		if t.TurnsUsed <= 0 {
			continue // codex/远端/引擎卡不回报 turns，见文件头的缺口披露
		}
		w := float64(t.TurnsUsed)
		samples[weightKeyOf(t)] = append(samples[weightKeyOf(t)], w)
		all = append(all, w)
	}
	tbl := &weightTable{byKey: map[weightKey]float64{}, measured: len(all), total: total}
	if len(all) == 0 {
		return tbl
	}
	sort.Float64s(all)
	tbl.overall = quantileSorted(all, 0.5)
	for k, v := range samples {
		if len(v) < 3 {
			continue // 桶内样本太少，用全局中位更稳
		}
		sort.Float64s(v)
		tbl.byKey[k] = quantileSorted(v, 0.5)
	}
	return tbl
}

func (w *weightTable) ok() bool { return w != nil && w.measured >= weightMinSamples && w.overall > 0 }

// weightOf 返回一张卡的工作量权重：done 卡有实测就用实测，否则按同类中位预测。
func (w *weightTable) weightOf(t *Task) float64 {
	if t.Status == statusDone && t.TurnsUsed > 0 {
		return float64(t.TurnsUsed)
	}
	if v, ok := w.byKey[weightKeyOf(t)]; ok {
		return v
	}
	return w.overall
}

// buildProjectWeighted 算工时口径。est 是卡数口径的预估余量（boardestimate.go），
// 余量卡按**在途卡的平均权重**折算——它们是"现有工作还会派生出来的卡"，与在途卡同源。
func buildProjectWeighted(ts []*Task, est *ProjectEstimate, now time.Time) *ProjectWeighted {
	tbl := buildWeightTable(ts)
	if !tbl.ok() {
		return &ProjectWeighted{Available: false, Unit: "turns", Basis: fmt.Sprintf(
			"工时口径数据不足：本项目仅 %d 张完成卡带实测 turns（需 ≥%d），"+
				"无法建立同类工作量基准，已回落卡数口径。", tbl.measured, weightMinSamples)}
	}

	doneW, totalW, pendingW := 0.0, 0.0, 0.0
	pendingN := 0
	for _, t := range ts {
		if t.Status == statusCanceled {
			continue // 与 progressPercent 同口径：取消卡不进分母
		}
		w := tbl.weightOf(t)
		totalW += w
		if t.Status == statusDone {
			doneW += w
			continue
		}
		if !t.terminal() {
			pendingW += w
			pendingN++
		}
	}
	// 预估余量卡：按在途卡的平均权重折算（无在途卡时用全局中位）。
	spawnW := 0.0
	if est != nil && est.EstimatedRemaining > 0 {
		avg := tbl.overall
		if pendingN > 0 {
			avg = pendingW / float64(pendingN)
		}
		spawnW = float64(est.EstimatedRemaining) * avg
		totalW += spawnW
	}
	if totalW <= 0 {
		return &ProjectWeighted{Available: false, Unit: "turns",
			Basis: "工时口径不可用：本项目总工作量为 0（没有可计权的卡）。"}
	}

	out := &ProjectWeighted{
		Available: true, Unit: "turns",
		Percent:       round1(doneW / totalW * 100),
		DoneWeight:    math.Round(doneW),
		TotalWeight:   math.Round(totalW),
		MeasuredRatio: round2(float64(tbl.measured) / math.Max(1, float64(tbl.total))),
	}

	// 换算率：每单位工作量耗多少墙钟分钟。与 paceModel 同一思路——用实测的
	// 「样本窗口跨度 / 窗口内完成的工作量」，天然吸收并行度、限额冷却与红线时段。
	if rate, span, wsum, lookbackH := weightThroughput(ts, now); rate > 0 {
		remain := (totalW - doneW) * rate
		fin := now.Add(time.Duration(remain) * time.Minute).Format(time.RFC3339)
		out.RemainMinutes = &[]float64{round1(remain)}[0]
		out.FinishAt = &fin
		out.Basis = fmt.Sprintf(
			"工时口径：按 turns（回合数）作工作量代理，本项目 done 权重 %.0f / 总权重 %.0f"+
				"（总权重含预估余量 %.0f）。换算率取最近 %d 天实测：%.1f 小时内结掉 %.0f 单位工作量"+
				" → 每单位 %.1f 分钟，剩余 %.0f 单位 ≈ %s。"+
				"%s turns 不是时长而是工作量代理；换算率已吸收并行度/冷却/红线。",
			doneW, totalW, spawnW, lookbackH/24, span, wsum, rate, totalW-doneW,
			fmtDurationMinutes(remain), tbl.gapNote())
		return out
	}
	out.Basis = fmt.Sprintf(
		"工时口径：按 turns（回合数）作工作量代理，本项目 done 权重 %.0f / 总权重 %.0f"+
			"（总权重含预估余量 %.0f）。**完成时刻不可得**：样本窗口内没有足够的完成工作量可推算换算率，"+
			"故只给占比不给时间。%s",
		doneW, totalW, spawnW, tbl.gapNote())
	return out
}

// annotateKindWeights 给分桶补工时口径的分子分母，让分桶随口径同步切换（与
// annotateKindEstimates 同一纪律）。余量卡不摊进桶：卡数口径的余量已按历史派生构成分摊过，
// 这里再按工作量摊一次会让两套口径的桶分母讲不同的故事；工时桶只算**已存在的卡**，
// 与项目级总条的差额（预估余量那部分）由总条自己承担并在 basis 里写明。
func annotateKindWeights(kinds []KindProgress, ts []*Task, kindOf map[string]kindMark) {
	tbl := buildWeightTable(ts)
	if !tbl.ok() {
		return
	}
	type acc struct{ done, total float64 }
	byKind := map[string]*acc{}
	for _, t := range ts {
		if t.Status == statusCanceled {
			continue
		}
		k := kindOf[t.ID].Kind
		if byKind[k] == nil {
			byKind[k] = &acc{}
		}
		w := tbl.weightOf(t)
		byKind[k].total += w
		if t.Status == statusDone {
			byKind[k].done += w
		}
	}
	for i := range kinds {
		if a := byKind[kinds[i].Key]; a != nil && a.total > 0 {
			kinds[i].WeightedDone = math.Round(a.done)
			kinds[i].WeightedTotal = math.Round(a.total)
		}
	}
}

// gapNote 披露实测覆盖率缺口（codex/远端/引擎卡不回报 turns）。
func (w *weightTable) gapNote() string {
	if w.total == 0 || w.measured >= w.total {
		return ""
	}
	return fmt.Sprintf("已知缺口：%d/%d 张完成卡不回报 turns（codex/远端/引擎执行器），"+
		"这些卡按同类中位补估计值，偏差方向未知。", w.total-w.measured, w.total)
}

// weightThroughput 返回 (每单位工作量的分钟数, 样本跨度小时, 样本工作量, 采用的回看窗口小时)。
// 窗口与 paceModel 同源（先 7 天，不够放宽到 30 天），保证两处 ETA 讲的是同一段历史。
func weightThroughput(ts []*Task, now time.Time) (float64, float64, float64, int) {
	try := func(lookbackH int) (float64, float64, float64, bool) {
		cut := now.Add(-time.Duration(lookbackH) * time.Hour)
		var first, last time.Time
		wsum := 0.0
		n := 0
		for _, t := range ts {
			if t.Status != statusDone || t.TurnsUsed <= 0 {
				continue
			}
			at, ok := parseRFC3339(t.UpdatedAt)
			if !ok || at.Before(cut) || at.After(now.Add(time.Hour)) {
				continue
			}
			if first.IsZero() || at.Before(first) {
				first = at
			}
			if at.After(last) {
				last = at
			}
			wsum += float64(t.TurnsUsed)
			n++
		}
		if n < paceMinGaps+1 || wsum <= 0 {
			return 0, 0, 0, false
		}
		spanH := last.Sub(first).Hours()
		if spanH <= 0 {
			return 0, 0, 0, false
		}
		return spanH * 60 / wsum, spanH, wsum, true
	}
	if rate, span, wsum, ok := try(pacePrimaryLookbackH); ok {
		return rate, span, wsum, pacePrimaryLookbackH
	}
	if rate, span, wsum, ok := try(paceFallbackLookbackH); ok {
		return rate, span, wsum, paceFallbackLookbackH
	}
	return 0, 0, 0, 0
}

// fmtDurationMinutes 把分钟数写成人话（basis 里用）。
func fmtDurationMinutes(m float64) string {
	if m < 90 {
		return fmt.Sprintf("%.0f 分钟", m)
	}
	if h := m / 60; h < 48 {
		return fmt.Sprintf("%.1f 小时", h)
	}
	return fmt.Sprintf("%.1f 天", m/1440)
}
