package main

// 「含预估余量」进度口径（boardestimate.go）回归测试。
// 这一类的事故形态是**编造读数**：估算值没有 basis、样本不足硬给系数、做完的项目显示幻影
// 余量——每一条都违反看板"数据不足显式披露"的底线，逐个立靶。

import (
	"strings"
	"testing"
)

func estCard(status string, mut ...func(*Task)) *Task {
	t := &Task{Status: status, Prompts: []string{"p"}}
	for _, m := range mut {
		m(t)
	}
	return t
}

func asReview(t *Task) { t.ReviewOf = "t-impl" }
func asFix(t *Task)    { t.FixRound = 1 }
func asCrossB(t *Task) { t.XRole = "B" }

// 计划锚点恒压过机械估算；被现存量超出时按现存计并提示更新——过期计划当分母是负进度幻觉。
func TestEstimatePlannedAnchor(t *testing.T) {
	ts := []*Task{estCard(statusDone), estCard(statusQueued)}
	e := buildProjectEstimate(ts, 10)
	if e.Source != "planned" || e.EstimatedTotal != 10 || e.EstimatedRemaining != 8 {
		t.Fatalf("计划锚点: %+v", e)
	}
	if !strings.Contains(e.Basis, "planned_total_cards") {
		t.Fatalf("basis 应指明锚点来源与校准点: %q", e.Basis)
	}
	// 现存 12 > 计划 10：分母按现存，余量 0，basis 明说计划过期。
	var many []*Task
	for i := 0; i < 12; i++ {
		many = append(many, estCard(statusQueued))
	}
	e = buildProjectEstimate(many, 10)
	if e.EstimatedTotal != 12 || e.EstimatedRemaining != 0 || !strings.Contains(e.Basis, "超出") {
		t.Fatalf("计划被超出应按现存计并披露: %+v", e)
	}
}

// 无在途卡：余量必须为 0——做完的项目显示幻影余量就是编造。
func TestEstimateSettledProjectHasNoPhantomRemaining(t *testing.T) {
	ts := []*Task{estCard(statusDone), estCard(statusDone, asReview), estCard(statusFailed)}
	e := buildProjectEstimate(ts, 0)
	if e.Source != "settled" || e.EstimatedRemaining != 0 || e.EstimatedTotal != 3 {
		t.Fatalf("终态项目: %+v", e)
	}
}

// 样本不足：显式回落现存卡数并说明，绝不硬给系数。
func TestEstimateInsufficientSampleFallsBackDisclosed(t *testing.T) {
	ts := []*Task{estCard(statusQueued), estCard(statusDone), estCard(statusDone, asReview)}
	e := buildProjectEstimate(ts, 0)
	if e.Source != "insufficient" || e.EstimatedTotal != 3 || e.EstimatedRemaining != 0 {
		t.Fatalf("样本不足: %+v", e)
	}
	if !strings.Contains(e.Basis, "样本不足") {
		t.Fatalf("必须显式披露样本不足: %q", e.Basis)
	}
}

// 膨胀率主路径：6 根卡（2 未完结）+ 6 衍生卡 → 系数 2.0，余量 = 2×(2−1)=2。
// 衍生卡三种身份（复审/修复轮/交叉 B 腿）都必须被认出来，漏认会低估系数。
func TestEstimateSpawnFactor(t *testing.T) {
	var ts []*Task
	for i := 0; i < 4; i++ {
		ts = append(ts, estCard(statusDone)) // 完结根卡
	}
	ts = append(ts, estCard(statusQueued), estCard(statusRunning)) // 未完结根卡 ×2
	ts = append(ts,
		estCard(statusDone, asReview), estCard(statusDone, asReview),
		estCard(statusDone, asFix), estCard(statusDone, asFix),
		estCard(statusDone, asCrossB), estCard(statusQueued, asReview),
	)
	e := buildProjectEstimate(ts, 0)
	if e.Source != "spawn_factor" {
		t.Fatalf("应走膨胀率估算: %+v", e)
	}
	if e.EstimatedTotal != 14 || e.EstimatedRemaining != 2 {
		t.Fatalf("12 卡/6 根 → 系数 2.0，未完结根 2 → 余量 2、总量 14: %+v", e)
	}
	for _, want := range []string{"系数", "根卡", "自校准", "不预测新立项"} {
		if !strings.Contains(e.Basis, want) {
			t.Errorf("basis 缺关键口径披露 %q: %q", want, e.Basis)
		}
	}
}

// 公式自有界：spawn = 未完结根卡×(系数−1) ≤ 现存−根卡，预估总量恒 < 2×现存——
// 离群系数（修复链疯长过的项目）不需要额外封顶就撑不爆预估轴。钉住这条数学事实：
// 若有人把公式改成"根卡×系数"这类会超界的形态，此测试红。
func TestEstimateSpawnBounded(t *testing.T) {
	var ts []*Task
	for i := 0; i < 5; i++ {
		ts = append(ts, estCard(statusQueued)) // 全部未完结根卡
	}
	for i := 0; i < 45; i++ {
		ts = append(ts, estCard(statusDone, asFix)) // 系数 50/5=10（极端离群）
	}
	e := buildProjectEstimate(ts, 0)
	if want := 95; e.EstimatedTotal != want { // 50 + 5×(10−1) = 95
		t.Fatalf("极端系数下 est=%d want %d", e.EstimatedTotal, want)
	}
	if e.EstimatedTotal >= 2*50 {
		t.Fatalf("预估总量必须 < 2×现存（公式自有界被破坏）: %d", e.EstimatedTotal)
	}
}

// 已取消卡两侧都不计：既不进现存分母，也不进系数样本。
func TestEstimateExcludesCanceled(t *testing.T) {
	ts := []*Task{estCard(statusDone), estCard(statusCanceled), estCard(statusCanceled, asReview)}
	e := buildProjectEstimate(ts, 0)
	if e.EstimatedTotal != 1 {
		t.Fatalf("取消卡不进任何口径: %+v", e)
	}
}
