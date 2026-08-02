package main

// 「含预估余量」进度口径（boardestimate.go）回归测试。
// 这一类的事故形态是**编造读数**：估算值没有 basis、样本不足硬给系数、做完的项目显示幻影
// 余量、分桶与总条对不上账——每一条都违反看板"数据不足显式披露"的底线，逐个立靶。
// 派生耦合模型的核心性质（委托人 2026-08-02 修正轮点名）另立靶：完成不派生的卡必须让
// 预估总量缩减（趋向完成），派生瀑布提前入账。

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

func asReview(t *Task)  { t.ReviewOf = "t-impl" }
func asFix(t *Task)     { t.FixRound = 1 }
func asCrossB(t *Task)  { t.XRole = "B" }
func asEmitted(t *Task) { t.EmittedBy = "t-parent" }

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

// couplingFixture 是派生耦合主路径的基准场景：
// 5 张 done 根卡 + 3 张 done 复审 + 1 张 queued 复审 + 2 张 queued 根卡
// → 现存 11、完成 8、系统派生 4、在途 3；k=0.5，R = round(3×1) = 3，est = 14。
func couplingFixture() []*Task {
	var ts []*Task
	for i := 0; i < 5; i++ {
		ts = append(ts, estCard(statusDone))
	}
	ts = append(ts,
		estCard(statusDone, asReview), estCard(statusDone, asReview), estCard(statusDone, asReview),
		estCard(statusQueued, asReview),
		estCard(statusQueued), estCard(statusQueued),
	)
	return ts
}

// 派生耦合主路径：k = 系统派生/完成，R = 在途 × k/(1−k)（等比级数，派生的派生一次前置）。
func TestEstimateSpawnCoupling(t *testing.T) {
	e := buildProjectEstimate(couplingFixture(), 0)
	if e.Source != "spawn_coupling" {
		t.Fatalf("应走派生耦合估算: %+v", e)
	}
	if e.EstimatedTotal != 14 || e.EstimatedRemaining != 3 {
		t.Fatalf("11 现存/8 完成/4 派生/3 在途 → k=0.5、R=3、总量 14: %+v", e)
	}
	for _, want := range []string{"k=0.50", "派生的派生", "自校准", "不预测", "emitted_by"} {
		if !strings.Contains(e.Basis, want) {
			t.Errorf("basis 缺关键口径披露 %q: %q", want, e.Basis)
		}
	}
}

// 【委托人修正轮核心性质】完成一张不派生的卡，预估总量必须缩减——"每次更新更接近完成"。
// 完成一张派生类卡（其自身也是别人派生出来的）同样不得抬升预估：它的量早已在 R 里。
func TestEstimateConvergesTowardCompletion(t *testing.T) {
	before := buildProjectEstimate(couplingFixture(), 0)

	// 场景一：queued 根卡完成（不派生新卡）。
	ts := couplingFixture()
	ts[9].Status = statusDone // 一张 queued 根卡 → done
	after := buildProjectEstimate(ts, 0)
	if after.EstimatedTotal >= before.EstimatedTotal {
		t.Fatalf("完成不派生的卡应让预估总量缩减: before=%d after=%d", before.EstimatedTotal, after.EstimatedTotal)
	}

	// 场景二：queued 复审卡完成。
	ts = couplingFixture()
	ts[8].Status = statusDone // queued 复审 → done
	after = buildProjectEstimate(ts, 0)
	if after.EstimatedTotal > before.EstimatedTotal {
		t.Fatalf("完成派生类卡不得抬升预估: before=%d after=%d", before.EstimatedTotal, after.EstimatedTotal)
	}
}

// 扩张期（k≥1，如装配浪进行中）：等比级数不收敛，按 k 上限给收敛下限并在 basis 明说——
// 吐无穷大或假装精确都是编造。emitted_by 谱系标计入派生人口（emit 产出是派生主力）。
func TestEstimateExpansionClampDisclosed(t *testing.T) {
	var ts []*Task
	ts = append(ts, estCard(statusDone), estCard(statusDone)) // 2 张 done 协调根
	for i := 0; i < 8; i++ {
		ts = append(ts, estCard(statusDone, asEmitted)) // 8 张 done emit 产出
	}
	for i := 0; i < 3; i++ {
		ts = append(ts, estCard(statusQueued, asEmitted)) // 3 张在途 emit 产出
	}
	// 完成 10、派生 11 → k=1.1 ≥ 0.85 → 按 0.85 计：R = round(3×0.85/0.15) = 17。
	e := buildProjectEstimate(ts, 0)
	if e.Source != "spawn_coupling" || e.EstimatedRemaining != 17 {
		t.Fatalf("扩张期钳制: %+v", e)
	}
	if !strings.Contains(e.Basis, "扩张期") || !strings.Contains(e.Basis, "下限") {
		t.Fatalf("扩张期身份与下限性质必须披露: %q", e.Basis)
	}
}

// 谱系标的四种系统派生身份都必须被认出来，漏认会低估 k（预估失真方向=余量偏小）。
func TestSystemSpawnedCardIdentities(t *testing.T) {
	for name, mut := range map[string]func(*Task){
		"复审卡": asReview, "修复轮卡": asFix, "交叉B腿": asCrossB, "emit谱系标": asEmitted,
	} {
		if !systemSpawnedCard(estCard(statusQueued, mut)) {
			t.Errorf("%s 应判为系统派生", name)
		}
	}
	if systemSpawnedCard(estCard(statusQueued)) {
		t.Error("裸根卡不是系统派生（人工立项外生）")
	}
}

// emit 产出必须落 emitted_by 谱系标（此前父指针只在事件 detail，卡面无谱系 → k 被低估）。
func TestEnqueueEmittedStampsLineage(t *testing.T) {
	root := testRoot(t)
	cfg := defaultConfig("claude")
	parent := newTask(root, cfg, typeAssembly, "装配父", t.TempDir(), []string{"p"}, 1)
	ids, err := enqueueEmitted(root, cfg, parent, "```json\n{\"tasks\":[{\"title\":\"子卡\",\"prompt\":\"做\"}]}\n```")
	if err != nil || len(ids) != 1 {
		t.Fatalf("emit 入队失败: ids=%v err=%v", ids, err)
	}
	child, err := loadTask(root, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if child.EmittedBy != parent.ID {
		t.Fatalf("emit 子卡应带 emitted_by=%s, got %q", parent.ID, child.EmittedBy)
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

// ---- 分桶分摊（委托人修正轮：分桶也要能切换）----

// 余量按历史派生构成分摊；最大余数法凑整；Σ 桶余量 ≡ 项目余量（分桶与总条对不上账
// 会被读成看板算错）；无派生历史的桶分 0（前端回落现有卡口径）。
func TestAnnotateKindEstimatesDistribution(t *testing.T) {
	// 历史派生构成：修复×2、审核×1；设计桶无派生历史。
	ts := []*Task{
		estCard(statusDone),           // impl 根
		estCard(statusDone, asFix),    // fix 派生
		estCard(statusQueued, asFix),  // fix 派生
		estCard(statusDone, asReview), // review 派生
		estCard(statusQueued),         // design 根（在途）
	}
	kindOf := map[string]kindMark{}
	keys := []string{kindImpl, kindFix, kindFix, kindReview, kindDesign}
	for i, task := range ts {
		task.ID = "t" + string(rune('a'+i))
		kindOf[task.ID] = kindMark{Kind: keys[i]}
	}
	kinds := []KindProgress{
		{Key: kindDesign, Stats: boardStats{Total: 1}},
		{Key: kindImpl, Stats: boardStats{Total: 1, Done: 1}},
		{Key: kindFix, Stats: boardStats{Total: 2, Done: 1}},
		{Key: kindReview, Stats: boardStats{Total: 1, Done: 1}},
	}
	annotateKindEstimates(kinds, ts, kindOf, 3)
	sum := 0
	byKey := map[string]KindProgress{}
	for _, k := range kinds {
		sum += k.EstimatedRemaining
		byKey[k.Key] = k
	}
	if sum != 3 {
		t.Fatalf("Σ 桶余量必须 ≡ 项目余量 3, got %d (%+v)", sum, kinds)
	}
	// 派生构成 fix:review = 2:1 → 3 张余量分 2/1。
	if byKey[kindFix].EstimatedRemaining != 2 || byKey[kindFix].EstimatedTotal != 4 {
		t.Errorf("fix 桶应分 2 张（est_total=4）: %+v", byKey[kindFix])
	}
	if byKey[kindReview].EstimatedRemaining != 1 || byKey[kindReview].EstimatedTotal != 2 {
		t.Errorf("review 桶应分 1 张（est_total=2）: %+v", byKey[kindReview])
	}
	if byKey[kindDesign].EstimatedRemaining != 0 || byKey[kindDesign].EstimatedTotal != 0 {
		t.Errorf("无派生历史的桶应保持 0（前端回落卡口径）: %+v", byKey[kindDesign])
	}
	// 余量 0：整个标注是 no-op。
	kinds2 := []KindProgress{{Key: kindImpl, Stats: boardStats{Total: 1}}}
	annotateKindEstimates(kinds2, ts, kindOf, 0)
	if kinds2[0].EstimatedTotal != 0 {
		t.Fatalf("余量 0 不应标注: %+v", kinds2[0])
	}
}
