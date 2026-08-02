package main

// 「工时进度」口径（boardweight.go）回归测试。
// 事故形态与预估口径同类：**编造读数**——没有实测样本却给出百分比、把预测值当实测、
// 缺口不披露、完成时刻凭空生成。逐条立靶。

import (
	"strings"
	"testing"
	"time"
)

func wcard(status, typ string, turns int, doneAt time.Time, steps int) *Task {
	prompts := make([]string, steps)
	for i := range prompts {
		prompts[i] = "p"
	}
	t := &Task{ID: "w" + status + typ + doneAt.Format("150405.000"), Type: typ,
		Status: status, Prompts: prompts, TurnsUsed: turns}
	if !doneAt.IsZero() {
		t.UpdatedAt = doneAt.Format(time.RFC3339)
	}
	return t
}

// 样本不足时整个口径不可用：给 available=false + 说明，绝不吐一个没有基准的百分比。
func TestWeightedUnavailableBelowSampleFloor(t *testing.T) {
	now := time.Now()
	var ts []*Task
	for i := 0; i < weightMinSamples-1; i++ {
		ts = append(ts, wcard(statusDone, typeSequence, 40, now.Add(-time.Duration(i)*time.Hour), 1))
	}
	w := buildProjectWeighted(ts, nil, now)
	if w.Available {
		t.Fatalf("实测样本 %d < %d 应判不可用: %+v", weightMinSamples-1, weightMinSamples, w)
	}
	if !strings.Contains(w.Basis, "回落卡数口径") {
		t.Errorf("必须说明回落去向: %q", w.Basis)
	}
	if w.Percent != 0 || w.FinishAt != nil {
		t.Errorf("不可用时不得给读数: percent=%v finish=%v", w.Percent, w.FinishAt)
	}
}

// 权重表按 (type, 是否多步) 分桶取中位；桶内样本 <3 回落全局中位。
// 中位而非均值：turns 重尾右偏（实测中位 41、均值 53、max 447），均值会让预测值系统性偏大。
func TestWeightTableMedianByTypeAndSteps(t *testing.T) {
	now := time.Now()
	var ts []*Task
	// sequence 单步：10、20、30、1000（离群）→ 中位 25，均值 265
	for i, v := range []int{10, 20, 30, 1000} {
		ts = append(ts, wcard(statusDone, typeSequence, v, now.Add(-time.Duration(i)*time.Hour), 1))
	}
	// design-review 单步：4、6、8 → 中位 6
	for i, v := range []int{4, 6, 8} {
		ts = append(ts, wcard(statusDone, typeReview, v, now.Add(-time.Duration(10+i)*time.Hour), 1))
	}
	// coordinate 只有 2 张（桶内 <3）→ 该桶不建，回落全局中位
	for i, v := range []int{100, 200} {
		ts = append(ts, wcard(statusDone, typeCoordinate, v, now.Add(-time.Duration(20+i)*time.Hour), 1))
	}
	for i := 0; i < 6; i++ { // 补够 weightMinSamples
		ts = append(ts, wcard(statusDone, typeSequence, 25, now.Add(-time.Duration(30+i)*time.Hour), 1))
	}
	tbl := buildWeightTable(ts)
	if !tbl.ok() {
		t.Fatalf("样本足够时应可用: measured=%d", tbl.measured)
	}
	if got := tbl.byKey[weightKey{typ: typeSequence}]; got != 25 {
		t.Errorf("sequence 单步中位应为 25（离群 1000 不得把它拽高）, got %v", got)
	}
	if got := tbl.byKey[weightKey{typ: typeReview}]; got != 6 {
		t.Errorf("design-review 中位应为 6, got %v", got)
	}
	if _, ok := tbl.byKey[weightKey{typ: typeCoordinate}]; ok {
		t.Error("桶内样本 <3 不该建桶（稀桶中位比粗桶更不可信）")
	}
	// 未跑的 coordinate 卡回落全局中位而不是 0——权重 0 等于把它当不存在。
	pending := &Task{Type: typeCoordinate, Status: statusQueued, Prompts: []string{"p"}}
	if got := tbl.weightOf(pending); got != tbl.overall || got <= 0 {
		t.Errorf("无桶样本应回落全局中位 %v, got %v", tbl.overall, got)
	}
	// done 卡有实测就用实测，不用预测值。
	measured := wcard(statusDone, typeSequence, 999, now, 1)
	if got := tbl.weightOf(measured); got != 999 {
		t.Errorf("done 卡应优先用实测 turns, got %v", got)
	}
}

// 主路径：进度按工作量算而不是卡数——这正是本口径存在的理由。
// 场景刻意构造成"卡数看着快完了、工作量还剩一大半"。
func TestWeightedProgressDiffersFromCardCount(t *testing.T) {
	now := time.Now()
	var ts []*Task
	// 12 张小卡（progress-pull 2 turns）已完成
	for i := 0; i < 12; i++ {
		ts = append(ts, wcard(statusDone, typeProgressPull, 2, now.Add(-time.Duration(i+1)*time.Hour), 1))
	}
	// 4 张大卡（sequence）在途——按卡数 12/16=75%，按工作量应远低于此
	for i := 0; i < 4; i++ {
		ts = append(ts, &Task{ID: "big" + string(rune('a'+i)), Type: typeSequence,
			Status: statusQueued, Prompts: []string{"p"}})
	}
	// 给 sequence 桶喂实测样本（60 turns/张）
	for i := 0; i < 4; i++ {
		ts = append(ts, wcard(statusDone, typeSequence, 60, now.Add(-time.Duration(20+i)*time.Hour), 1))
	}
	w := buildProjectWeighted(ts, nil, now)
	if !w.Available {
		t.Fatalf("样本足够应可用: %s", w.Basis)
	}
	// done = 12×2 + 4×60 = 264；pending = 4×60 = 240；total = 504 → 52.4%
	if w.DoneWeight != 264 || w.TotalWeight != 504 {
		t.Fatalf("权重口径: done=%v total=%v, want 264/504", w.DoneWeight, w.TotalWeight)
	}
	cardPct := 16.0 / 20.0 * 100 // 卡数口径 80%
	if w.Percent >= cardPct {
		t.Errorf("剩余全是大卡时工时口径必须低于卡数口径: 工时 %v%% vs 卡数 %v%%", w.Percent, cardPct)
	}
	// 状态拆分（供进度条分段着色）：各段之和 ≡ Stats.Total ≡ 不含余量的总权重。
	if w.Stats.Done != 264 || w.Stats.Queued != 240 || w.Stats.Total != 504 {
		t.Fatalf("状态拆分: %+v", w.Stats)
	}
	sum := w.Stats.Queued + w.Stats.Running + w.Stats.LimitPaused + w.Stats.Held +
		w.Stats.Failed + w.Stats.Done
	if sum != w.Stats.Total {
		t.Errorf("各状态段之和必须等于 Stats.Total（否则条画不满/画溢出）: %v vs %v", sum, w.Stats.Total)
	}
}

// 取消卡不进任何状态段，也不进 Total——与卡数口径"分母排除已取消"一致。
func TestWeightStatsExcludesCanceled(t *testing.T) {
	var s WeightStats
	s.add(statusDone, 10)
	s.add(statusCanceled, 999)
	s.add(statusQueued, 5)
	if s.Canceled != 0 || s.Total != 15 {
		t.Fatalf("取消卡不得计入: %+v", s)
	}
}

// 预估余量单列 SpawnWeight（画条尾幽灵段），不混进任何状态段——混进去就会被读成真实卡的活。
func TestWeightedSpawnWeightSeparateFromStats(t *testing.T) {
	now := time.Now()
	var ts []*Task
	for i := 0; i < 12; i++ {
		ts = append(ts, wcard(statusDone, typeSequence, 50, now.Add(-time.Duration(i+1)*time.Hour), 1))
	}
	ts = append(ts, &Task{ID: "p1", Type: typeSequence, Status: statusQueued, Prompts: []string{"p"}})
	w := buildProjectWeighted(ts, &ProjectEstimate{EstimatedRemaining: 2}, now)
	if w.SpawnWeight != 100 { // 2 × 在途均值 50
		t.Fatalf("SpawnWeight 应为 100, got %v", w.SpawnWeight)
	}
	if w.Stats.Total != 650 { // 12×50 + 1×50，不含余量
		t.Fatalf("Stats.Total 只算已存在的卡: %v", w.Stats.Total)
	}
	if w.TotalWeight != w.Stats.Total+w.SpawnWeight {
		t.Errorf("总权重应 = 已存在 + 余量: %v vs %v+%v", w.TotalWeight, w.Stats.Total, w.SpawnWeight)
	}
}

// 预估余量按在途卡的平均权重折进总分母（余量卡与在途卡同源）。
func TestWeightedFoldsEstimateRemaining(t *testing.T) {
	now := time.Now()
	var ts []*Task
	for i := 0; i < 12; i++ {
		ts = append(ts, wcard(statusDone, typeSequence, 50, now.Add(-time.Duration(i+1)*time.Hour), 1))
	}
	ts = append(ts, &Task{ID: "p1", Type: typeSequence, Status: statusQueued, Prompts: []string{"p"}})

	base := buildProjectWeighted(ts, nil, now)
	withEst := buildProjectWeighted(ts, &ProjectEstimate{EstimatedRemaining: 3}, now)
	if withEst.TotalWeight <= base.TotalWeight {
		t.Fatalf("预估余量应抬高总权重: base=%v est=%v", base.TotalWeight, withEst.TotalWeight)
	}
	// 在途卡平均权重 50（sequence 中位）→ 3 张余量 = 150
	if got := withEst.TotalWeight - base.TotalWeight; got != 150 {
		t.Errorf("余量折算应为 3×50=150, got %v", got)
	}
	if withEst.DoneWeight != base.DoneWeight {
		t.Error("余量不得进分子")
	}
}

// 实测覆盖缺口（codex/远端/引擎卡不回报 turns）必须披露，不能让补出来的估计值冒充实测。
func TestWeightedDisclosesMeasurementGap(t *testing.T) {
	now := time.Now()
	var ts []*Task
	for i := 0; i < 12; i++ {
		ts = append(ts, wcard(statusDone, typeSequence, 40, now.Add(-time.Duration(i+1)*time.Hour), 1))
	}
	for i := 0; i < 6; i++ { // codex 卡：done 但 turns=0
		ts = append(ts, wcard(statusDone, typeSequence, 0, now.Add(-time.Duration(20+i)*time.Hour), 1))
	}
	w := buildProjectWeighted(ts, nil, now)
	if !w.Available {
		t.Fatalf("应可用: %s", w.Basis)
	}
	if w.MeasuredRatio >= 1 || w.MeasuredRatio <= 0 {
		t.Fatalf("实测覆盖率应在 (0,1): %v", w.MeasuredRatio)
	}
	if !strings.Contains(w.Basis, "不回报 turns") || !strings.Contains(w.Basis, "偏差方向未知") {
		t.Errorf("缺口与其不确定性必须写进 basis: %q", w.Basis)
	}
	if !strings.Contains(w.Basis, "turns 不是时长") {
		t.Errorf("必须说明 turns 只是工作量代理、不是时长: %q", w.Basis)
	}
}

// 换算率不可得时只给占比不给时间——编一个完成时刻比不给更糟。
func TestWeightedNoFinishTimeWithoutThroughput(t *testing.T) {
	now := time.Now()
	var ts []*Task
	// 全部完成于同一时刻 → 跨度为 0，算不出换算率
	same := now.Add(-2 * time.Hour)
	for i := 0; i < 14; i++ {
		ts = append(ts, wcard(statusDone, typeSequence, 40, same, 1))
	}
	ts = append(ts, &Task{ID: "pend", Type: typeSequence, Status: statusQueued, Prompts: []string{"p"}})
	w := buildProjectWeighted(ts, nil, now)
	if !w.Available || w.Percent <= 0 {
		t.Fatalf("占比仍应给出: %+v", w)
	}
	if w.FinishAt != nil || w.RemainMinutes != nil {
		t.Fatalf("换算率不可得时不得给完成时刻: finish=%v remain=%v", w.FinishAt, w.RemainMinutes)
	}
	if !strings.Contains(w.Basis, "完成时刻不可得") {
		t.Errorf("必须说明为什么没有时间: %q", w.Basis)
	}
}

// 分桶工时：只计已存在的卡（余量由总条承担），取消卡不进分母。
func TestAnnotateKindWeights(t *testing.T) {
	now := time.Now()
	var ts []*Task
	mk := func(id, kind, status string, turns int) *Task {
		c := wcard(status, typeSequence, turns, now.Add(-time.Duration(len(ts)+1)*time.Hour), 1)
		c.ID = id
		return c
	}
	for i := 0; i < 12; i++ {
		ts = append(ts, mk("base"+string(rune('a'+i)), kindImpl, statusDone, 40))
	}
	fixDone := mk("fix1", kindFix, statusDone, 30)
	fixPend := &Task{ID: "fix2", Type: typeSequence, Status: statusQueued, Prompts: []string{"p"}}
	canceled := &Task{ID: "fix3", Type: typeSequence, Status: statusCanceled, Prompts: []string{"p"}}
	ts = append(ts, fixDone, fixPend, canceled)

	kindOf := map[string]kindMark{}
	for _, c := range ts {
		kindOf[c.ID] = kindMark{Kind: kindImpl}
	}
	for _, c := range []*Task{fixDone, fixPend, canceled} {
		kindOf[c.ID] = kindMark{Kind: kindFix}
	}
	kinds := []KindProgress{{Key: kindImpl}, {Key: kindFix}}
	annotateKindWeights(kinds, ts, kindOf)

	if kinds[0].WeightedTotal != 480 || kinds[0].WeightedDone != 480 { // 12×40
		t.Errorf("落地桶: %+v", kinds[0])
	}
	// 修复桶：done 30（实测）+ pending 40（sequence 中位）= 70；取消卡不计
	if kinds[1].WeightedTotal != 70 || kinds[1].WeightedDone != 30 {
		t.Errorf("修复桶应为 30/70（取消卡不进分母）: %+v", kinds[1])
	}
	// 分桶也要带状态拆分（工时口径下分桶条同样按状态分段着色）。
	ws := kinds[1].WeightStats
	if ws == nil || ws.Done != 30 || ws.Queued != 40 || ws.Total != 70 {
		t.Errorf("修复桶状态拆分: %+v", ws)
	}
}
