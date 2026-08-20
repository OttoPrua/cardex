package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func loadReferenceMaturityFixture(t *testing.T) maturityContract {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "perlica-progress-reference-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c maturityContract
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// 历史回归锚：这不是当前实时状态，而是确保插件永远不会重新滑回 Cardex done 率。
func TestPerlicaReferenceMaturityFixture(t *testing.T) {
	c := loadReferenceMaturityFixture(t)
	now := time.Date(2026, 8, 9, 23, 33, 0, 0, time.FixedZone("CST", 8*3600))
	want := map[string]struct {
		points, max int
		pct         float64
	}{
		"PerlicaHermes":   {31, 44, 70.45},
		"PerlicaOptimize": {10, 44, 22.73},
		"PerlicaAnywhere": {19, 72, 26.39},
		"PerlicaAnything": {36, 56, 64.29},
	}
	wantKinds := map[string]map[string][2]int{
		"PerlicaHermes": {
			kindDesign: {1, 4}, kindImpl: {15, 20}, kindFix: {3, 4}, kindReview: {7, 8}, kindCoord: {5, 8},
		},
		"PerlicaOptimize": {
			kindDesign: {3, 8}, kindImpl: {3, 12}, kindFix: {1, 4}, kindReview: {2, 12}, kindCoord: {1, 8},
		},
		"PerlicaAnywhere": {
			kindDesign: {2, 8}, kindImpl: {10, 40}, kindFix: {2, 8}, kindReview: {3, 12}, kindCoord: {2, 4},
		},
		"PerlicaAnything": {
			kindDesign: {3, 8}, kindImpl: {24, 32}, kindFix: {3, 4}, kindReview: {3, 4}, kindCoord: {3, 8},
		},
	}
	for project, w := range want {
		got, err := materializeMaturity(&c, project, now)
		if err != nil {
			t.Fatalf("%s: %v", project, err)
		}
		if got.Points != w.points || got.MaxPoints != w.max || got.Percent != w.pct {
			t.Errorf("%s 得分 got=%d/%d %.2f want=%d/%d %.2f",
				project, got.Points, got.MaxPoints, got.Percent, w.points, w.max, w.pct)
		}
		if got.Portfolio.Points != 96 || got.Portfolio.MaxPoints != 216 || got.Portfolio.Percent != 44.44 {
			t.Errorf("%s 全体系应为 96/216=44.44%%，got=%+v", project, got.Portfolio)
		}
		// fixture 明确未加权：工时档必须透明回退成熟度，而不是 turns/card done。
		if got.EffortWeighted || got.WorkPercent != got.Percent {
			t.Errorf("%s 未加权工时回退错误：weighted=%v work=%.2f maturity=%.2f",
				project, got.EffortWeighted, got.WorkPercent, got.Percent)
		}
		if got.Delta.NetPoints != 0 || got.Delta.NetDeltaPP != 0 {
			t.Errorf("reference snapshot 对自身净变化应为 0，got=%+v", got.Delta)
		}
		kindPoints, kindMax := 0, 0
		if len(got.Kinds) != len(kindOrder) {
			t.Fatalf("%s v2 应稳定输出五类成熟度，got=%+v", project, got.Kinds)
		}
		for i, k := range got.Kinds {
			if k.Key != kindOrder[i] {
				t.Fatalf("%s kind 顺序漂移：index=%d got=%s want=%s", project, i, k.Key, kindOrder[i])
			}
			kw, ok := wantKinds[project][k.Key]
			if !ok || k.Points != kw[0] || k.MaxPoints != kw[1] {
				t.Errorf("%s/%s 分类成熟度 got=%d/%d want=%d/%d", project, k.Key, k.Points, k.MaxPoints, kw[0], kw[1])
			}
			if k.WorkPoints != float64(k.Points) || k.WorkMaxPoints != float64(k.MaxPoints) || k.WorkPercent != k.Percent {
				t.Errorf("%s/%s 未加权工时分类必须与结构分类相同，got=%+v", project, k.Key, k)
			}
			kindPoints += k.Points
			kindMax += k.MaxPoints
		}
		if kindPoints != got.Points || kindMax != got.MaxPoints {
			t.Fatalf("%s 彩色完成段必须精确覆盖总成熟度：kinds=%d/%d total=%d/%d", project, kindPoints, kindMax, got.Points, got.MaxPoints)
		}
	}

	got, err := materializeMaturity(&c, "PerlicaOptimize", now)
	if err != nil {
		t.Fatal(err)
	}
	led := got.SnapshotLedger
	if led.PhaseRowsChanged != 7 || led.ForwardTransitions != 6 || led.EvidenceRegressions != 1 || led.NetPointGain != 5 {
		t.Fatalf("历史迁移应为 7 行/6 前进/1 回退/+5 点，got=%+v", led)
	}
	if led.LiteralMainMerges != 0 || led.LivePromotions != 0 || led.UserAcceptancePromotions != 0 {
		t.Fatalf("literal main/live/用户验收晋级必须均为 0，got=%+v", led)
	}
	if got.Activity == nil || got.Activity.CardexDone != 513 || got.Activity.CardexTotal != 545 {
		t.Fatalf("Cardex 活动诊断丢失：%+v", got.Activity)
	}
	doneRate := float64(got.Activity.CardexDone) / float64(got.Activity.CardexTotal) * 100
	if doneRate < 94.12 || doneRate > 94.14 {
		t.Fatalf("fixture 的禁用主结果应约为 94.13，got %.4f", doneRate)
	}
	if got.Portfolio.Percent == 94.13 || got.Percent == 94.13 {
		t.Fatal("成熟度主结果绝不能等于 Cardex done 率")
	}

	// fresh review 反例必须真的表现为候选→开发/复审的负一分，而非只计一条备注。
	foundRegression := false
	for _, tr := range got.Transitions {
		if tr.Trigger == "fresh_review" {
			foundRegression = tr.PreviousState == "隔离候选" && tr.CurrentState == "开发/复审中"
		}
	}
	if !foundRegression || got.ProjectLedger.EvidenceRegressions != 1 || got.ProjectLedger.NetPointGain != 0 {
		t.Fatalf("Optimize fresh review 回退未入账：project=%+v transitions=%+v", got.ProjectLedger, got.Transitions)
	}
}

func TestMaturityEffortWeightIsIndependentIndex(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	w3, w1 := 3.0, 1.0
	c := minimalMaturityContract(now)
	c.Denominator = maturityDenominator{Version: "weighted-v1", CapabilitySlices: 2, MaxPoints: 8, WeightedByEffort: true}
	c.Projects[0].Slices = []maturityContractSlice{
		{ID: "A", Order: 1, Phase: "大切片", Kind: kindImpl, State: "Live/有界 Canary", EffortWeight: &w3, AcceptanceGate: "已闭合"},
		{ID: "B", Order: 2, Phase: "小切片", Kind: kindDesign, State: "设计/待开发", EffortWeight: &w1, AcceptanceGate: "待开发"},
	}
	got, err := materializeMaturity(&c, "Demo", now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Percent != 50 { // 非加权：(4+0)/8
		t.Fatalf("结构成熟度应保持独立 50%%，got %.2f", got.Percent)
	}
	if !got.EffortWeighted || got.WorkPoints != 12 || got.WorkMaxPoints != 16 || got.WorkPercent != 75 {
		t.Fatalf("工时成熟度应为 12/16=75%%，got=%+v", got)
	}
}

func TestMaturityFreshReviewDowngradeIsAllowedAndReconciled(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	c := minimalMaturityContract(now)
	c.Denominator = maturityDenominator{Version: "rollback-v1", CapabilitySlices: 1, MaxPoints: 4}
	c.Projects[0].Slices = []maturityContractSlice{{
		ID: "R", Order: 1, Phase: "候选", Kind: kindReview, State: "开发/复审中", AcceptanceGate: "修 P1 后重审",
	}}
	c.PreviousSync = &maturitySnapshotRef{At: now.Add(-time.Hour).Format(time.RFC3339), Points: 2, MaxPoints: 4}
	c.MigrationLedger = []maturityTransition{{
		At: now.Format(time.RFC3339), Project: "Demo", SliceID: "R", Phase: "候选",
		PreviousState: "隔离候选", CurrentState: "开发/复审中", Trigger: "fresh_review",
		Evidence:      []maturityEvidence{{Layer: "git_test", ObservedAt: now.Format(time.RFC3339), Source: "review", Summary: "P1=1"}},
		ReviewVerdict: "FAIL_P1",
	}}
	got, err := materializeMaturity(&c, "Demo", now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Points != 1 || got.SnapshotLedger.NetPointGain != -1 || got.SnapshotLedger.EvidenceRegressions != 1 {
		t.Fatalf("fresh review 回退应从 2→1，got points=%d ledger=%+v", got.Points, got.SnapshotLedger)
	}
}

func TestMaturityV2RequiresStableKindButV1RemainsReadable(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	c := minimalMaturityContract(now)
	c.Projects[0].Slices[0].Kind = ""
	if _, err := materializeMaturity(&c, "Demo", now); err == nil {
		t.Fatal("v2 缺 kind 必须 fail-honest，不能按标题猜分类")
	}
	c.SchemaVersion = maturitySchemaV1
	got, err := materializeMaturity(&c, "Demo", now)
	if err != nil {
		t.Fatalf("v1 向后兼容失败：%v", err)
	}
	if len(got.Kinds) != 0 || got.Points != 0 || got.MaxPoints != 4 {
		t.Fatalf("v1 无 kind 时应保留成熟度但不编造分类，got=%+v", got)
	}
}

func TestBuildProjectMaturityRejectsStaleAndRelativeSource(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	if got := buildProjectMaturity(&boardMaturitySource{Path: "relative.json"}, "Demo", now); got == nil || got.Available {
		t.Fatalf("相对路径必须 fail-honest，got=%+v", got)
	}
	c := minimalMaturityContract(now.Add(-3 * time.Hour))
	dir := t.TempDir()
	p := filepath.Join(dir, "maturity.json")
	b, _ := json.Marshal(c)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	got := buildProjectMaturity(&boardMaturitySource{Path: p, MaxAgeHours: 2}, "Demo", now)
	if got == nil || got.Available || got.InsufficientReason == "" {
		t.Fatalf("超龄合同必须 available=false 并说明原因，got=%+v", got)
	}
}

func minimalMaturityContract(at time.Time) maturityContract {
	stamp := at.Format(time.RFC3339)
	return maturityContract{
		SchemaVersion: maturitySchemaV2,
		SnapshotAt:    stamp,
		Denominator: maturityDenominator{
			Version: "demo-v1", CapabilitySlices: 1, MaxPoints: 4,
		},
		StateScores: map[string]int{
			"设计/待开发": 0, "遗留待迁移": 1, "开发/复审中": 1,
			"隔离候选": 2, "主线/集成已有": 3, "Live/有界 Canary": 4,
		},
		Projects: []maturityContractProject{{
			Project: "Demo", ProjectID: "demo",
			Slices: []maturityContractSlice{{
				ID: "D-1", Order: 1, Phase: "Demo", Kind: kindDesign, State: "设计/待开发", AcceptanceGate: "待开发",
			}},
		}},
		EvidenceLayers: []maturityEvidence{
			{Layer: "design", ObservedAt: stamp, Source: "design", Summary: "目标"},
			{Layer: "cardex", ObservedAt: stamp, Source: "cardex", Summary: "活动"},
			{Layer: "git_test", ObservedAt: stamp, Source: "git", Summary: "代码测试"},
			{Layer: "runtime", ObservedAt: stamp, Source: "runtime", Summary: "只读运行态"},
		},
	}
}
