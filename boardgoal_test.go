package main

// boardgoal_test.go —— CG-8 目标锚定进度验收。
//
// 逐项对应验收清单：
//   [快照/齐全]  TestGoalFullManualSnapshot        —— 全人工里程碑，合成值+源+as_of+逐条
//   [快照/缺失]  TestBuildSnapshotOmitsGoalWhenNotConfigured —— goal 缺失 → JSON 里无 goal 键
//   [快照/在场]  TestBuildSnapshotIncludesGoalWhenConfigured —— goal 在场 → JSON 里有 goal 键
//   [evidence]   TestGoalEvidencePercentComputed   —— gate_counts pass/blocked → 42.9
//   [evidence]   TestGoalEvidenceFileMissing…      —— 文件删 → 里程碑不足 + goal.Partial
//   [反例①]     TestGoalEvidenceStringFieldRefuses —— 同名字段是字符串 → 里程碑不足
//   [反例②]     TestGoalWeightSumZeroInsufficient —— 权重和 0 → 整块不足，无百分比
//   [stale]      TestGoalEvidenceStaleMarks…       —— 超龄 → stale=true 必现
//   [负权重]     TestGoalNegativeWeightInsufficient —— 负权重 → 整块不足
//   [回归]       TestProgressPercentUnchangedByGoal —— 卡片进度语义不因 goal 改变
//
// 反例注入不是"多写一个 case"——它是承重防线：
//   ①"看起来像数值的字符串"若被贴心解析成数字，落地进度就是编造；
//   ②权重和 0 若被"回退到均权"或"直接给 0%"，都是替用户造读数。
// 两条防线必须自动化守住，不能依赖代码 review 的记忆。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- 测试辅助 ----

func fixedTime() time.Time {
	return time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
}

func floatPtr(v float64) *float64 { return &v }

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// backdateFile 把文件 mtime 强制设成 hoursAgo 小时前，用于超龄断言。
func backdateFile(t *testing.T, path string, hoursAgo float64) {
	t.Helper()
	old := fixedTime().Add(-time.Duration(hoursAgo * float64(time.Hour)))
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

// ---- 单元层：buildProjectGoal 的六个承重分支 ----

func TestGoalNilOverrideReturnsNil(t *testing.T) {
	if got := buildProjectGoal(nil, fixedTime()); got != nil {
		t.Fatalf("nil override 必须返回 nil（前端契约：不显示该区块），got=%+v", got)
	}
}

func TestGoalFullManualSnapshot(t *testing.T) {
	ov := &boardOverrideGoal{
		Statement: "落地实际使用",
		AsOf:      "2026-07-23",
		Milestones: []boardOverrideMilestone{
			{ID: "M1", Title: "设计收口", Weight: 1, DonePercent: floatPtr(100), Basis: "REVIEW Go"},
			{ID: "M2", Title: "控制面离线", Weight: 2, DonePercent: floatPtr(50), Basis: "H2 波收尾"},
		},
	}
	pg := buildProjectGoal(ov, fixedTime())
	if pg == nil {
		t.Fatal("goal 齐全时不应返回 nil")
	}
	// 合成 = (1*100 + 2*50) / (1+2) = 200/3 ≈ 66.7
	if pg.LandedPercent == nil || *pg.LandedPercent < 66.6 || *pg.LandedPercent > 66.8 {
		t.Fatalf("landed_percent 应为 ~66.7，got=%v", pg.LandedPercent)
	}
	if pg.GoalSource != "manual@2026-07-23" {
		t.Fatalf("goal_source 应含 as_of，got=%q", pg.GoalSource)
	}
	if pg.AsOf != "2026-07-23" {
		t.Fatalf("as_of 未透出：%q", pg.AsOf)
	}
	if pg.Insufficient || pg.Partial {
		t.Fatalf("齐全场景不应 insufficient/partial，got insufficient=%v partial=%v", pg.Insufficient, pg.Partial)
	}
	if len(pg.Milestones) != 2 {
		t.Fatalf("里程碑必须逐条返回，got %d", len(pg.Milestones))
	}
	for _, m := range pg.Milestones {
		if m.Source != "manual@2026-07-23" {
			t.Fatalf("每条里程碑 source 应为 manual@as_of，got %q on %s", m.Source, m.ID)
		}
		if m.DonePercent == nil {
			t.Fatalf("里程碑 %s DonePercent 不应 nil", m.ID)
		}
	}

	// JSON 序列化断言：契约里 landed_percent 是数值而非 NaN/字符串。
	b, err := json.Marshal(pg)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"as_of":"2026-07-23"`) {
		t.Fatalf("JSON 里 as_of 缺失：%s", s)
	}
	if !strings.Contains(s, `"goal_source":"manual@2026-07-23"`) {
		t.Fatalf("JSON 里 goal_source 缺失：%s", s)
	}
	if strings.Contains(s, "NaN") || strings.Contains(s, "Infinity") {
		t.Fatalf("JSON 不得出现 NaN/Infinity：%s", s)
	}
}

func TestGoalEvidencePercentComputed(t *testing.T) {
	dir := t.TempDir()
	fx := filepath.Join(dir, "gates.json")
	// 命中路径：gate_counts.{pass,blocked}
	writeJSONFile(t, fx, map[string]any{
		"gate_counts": map[string]any{"pass": 9, "blocked": 12},
	})
	ov := &boardOverrideGoal{
		AsOf: "2026-07-23",
		Milestones: []boardOverrideMilestone{
			{
				ID: "M4", Title: "test-ready gates", Weight: 1,
				Evidence: &boardOverrideEvidence{
					Path:        fx,
					Numerator:   "gate_counts.pass",
					Denominator: []string{"gate_counts.pass", "gate_counts.blocked"},
					MaxAgeHours: 24,
				},
			},
		},
	}
	pg := buildProjectGoal(ov, fixedTime())
	if pg == nil || len(pg.Milestones) != 1 {
		t.Fatalf("evidence 折算失败: %+v", pg)
	}
	m := pg.Milestones[0]
	if m.DonePercent == nil || *m.DonePercent < 42.8 || *m.DonePercent > 43.0 {
		t.Fatalf("done_percent 应 ≈42.9，got=%v", m.DonePercent)
	}
	if !strings.HasPrefix(m.Source, "evidence@"+fx+"@") {
		t.Fatalf("source 应 evidence@<path>@<mtime>，got %q", m.Source)
	}
	if m.Insufficient || m.Stale {
		t.Fatalf("正常路径不应 stale/insufficient: %+v", m)
	}
	// 单里程碑覆盖时 goal_source 走 evidence 分支。
	if pg.GoalSource != "evidence" {
		t.Fatalf("单 evidence 里程碑时 goal_source 应为 evidence，got %q", pg.GoalSource)
	}
}

func TestGoalEvidenceFileMissingMarksInsufficientAndPartial(t *testing.T) {
	dir := t.TempDir()
	fx := filepath.Join(dir, "gates.json")
	writeJSONFile(t, fx, map[string]any{
		"gate_counts": map[string]any{"pass": 9, "blocked": 12},
	})
	ov := &boardOverrideGoal{
		AsOf: "2026-07-23",
		Milestones: []boardOverrideMilestone{
			{ID: "M2", Title: "控制面离线", Weight: 2, DonePercent: floatPtr(50)},
			{
				ID: "M4", Title: "gates", Weight: 1,
				Evidence: &boardOverrideEvidence{
					Path: fx, Numerator: "gate_counts.pass",
					Denominator: []string{"gate_counts.pass", "gate_counts.blocked"},
					MaxAgeHours: 24,
				},
			},
		},
	}
	// 先验证正常场景有 landed_percent
	if pg := buildProjectGoal(ov, fixedTime()); pg.LandedPercent == nil {
		t.Fatalf("前置：fixture 在场时应能合成，pg=%+v", pg)
	}
	// 删除 fixture 文件——模拟机械检查未落盘 / 被清理的场景
	if err := os.Remove(fx); err != nil {
		t.Fatal(err)
	}
	pg := buildProjectGoal(ov, fixedTime())
	if pg == nil || len(pg.Milestones) != 2 {
		t.Fatalf("goal 应仍在，milestones 应齐 2 条：%+v", pg)
	}
	if !pg.Partial {
		t.Fatal("部分里程碑数据不足时必须 partial=true")
	}
	// 合成值只基于可用里程碑（M2）：50%
	if pg.LandedPercent == nil || *pg.LandedPercent < 49.9 || *pg.LandedPercent > 50.1 {
		t.Fatalf("partial 合成应为 50 (=M2 单值)，got=%v", pg.LandedPercent)
	}
	// M4 应标 insufficient，且 done_percent==nil（禁止回退到旧值或人工值）
	var m4 GoalMilestone
	for _, m := range pg.Milestones {
		if m.ID == "M4" {
			m4 = m
			break
		}
	}
	if !m4.Insufficient || m4.DonePercent != nil {
		t.Fatalf("M4 文件缺失时应 insufficient 且 DonePercent==nil，got=%+v", m4)
	}
	// JSON 层面确认 done_percent 是 null（不是 0，也不是缺失）
	b, _ := json.Marshal(m4)
	if !strings.Contains(string(b), `"done_percent":null`) {
		t.Fatalf("done_percent 必须序列化为 null，got %s", string(b))
	}
}

// TestGoalEvidenceStringFieldRefuses —— 反例注入①。
// fixture 顶层 gate_counts 是**字符串** "9/21"（不是 map）；
// 若 pointer 解析器"贴心"把字符串 parse 成数字，测试报红。
func TestGoalEvidenceStringFieldRefuses(t *testing.T) {
	dir := t.TempDir()
	fx := filepath.Join(dir, "gates.json")
	writeJSONFile(t, fx, map[string]any{
		"gate_counts": "9/21", // 反例
	})
	ov := &boardOverrideGoal{
		Milestones: []boardOverrideMilestone{
			{
				ID: "M4", Title: "gates", Weight: 1,
				Evidence: &boardOverrideEvidence{
					Path: fx, Numerator: "gate_counts.pass",
					Denominator: []string{"gate_counts.pass", "gate_counts.blocked"},
					MaxAgeHours: 24,
				},
			},
		},
	}
	pg := buildProjectGoal(ov, fixedTime())
	m := pg.Milestones[0]
	if !m.Insufficient {
		t.Fatal("反例①：gate_counts 是字符串时必须判 insufficient")
	}
	if m.DonePercent != nil {
		t.Fatalf("反例①：绝不能解析出数值，got done_percent=%v", *m.DonePercent)
	}
	if pg.LandedPercent != nil {
		t.Fatalf("反例①：唯一里程碑 insufficient 时 landed_percent 必须为 nil，got=%v", *pg.LandedPercent)
	}
	if !pg.Partial {
		t.Fatal("反例①：全部里程碑不足时必须 partial=true 披露降级")
	}

	// 反例①备用形态：pointer 直指 "gate_counts"（终值即字符串）也要拒绝
	ov.Milestones[0].Evidence.Numerator = "gate_counts"
	ov.Milestones[0].Evidence.Denominator = []string{"gate_counts"}
	pg = buildProjectGoal(ov, fixedTime())
	if !pg.Milestones[0].Insufficient || pg.Milestones[0].DonePercent != nil {
		t.Fatal("反例①备用：终值是字符串时也必须 insufficient")
	}
}

// TestGoalWeightSumZeroInsufficient —— 反例注入②。
// 权重和 0 若被"回退均权"或"直接给 0%"都是造读数——必须整块标数据不足，
// 且 landed_percent 是 null（JSON 不得出现 NaN/Infinity 或任何百分比数字）。
func TestGoalWeightSumZeroInsufficient(t *testing.T) {
	ov := &boardOverrideGoal{
		AsOf: "2026-07-23",
		Milestones: []boardOverrideMilestone{
			{ID: "M1", Title: "全零权重 A", Weight: 0, DonePercent: floatPtr(100)},
			{ID: "M2", Title: "全零权重 B", Weight: 0, DonePercent: floatPtr(50)},
		},
	}
	pg := buildProjectGoal(ov, fixedTime())
	if !pg.Insufficient {
		t.Fatal("反例②：权重和 0 时必须整块 insufficient")
	}
	if pg.LandedPercent != nil {
		t.Fatalf("反例②：landed_percent 必须 nil，got=%v", *pg.LandedPercent)
	}
	b, _ := json.Marshal(pg)
	s := string(b)
	if strings.Contains(s, "NaN") || strings.Contains(s, "Infinity") {
		t.Fatalf("反例②：JSON 不得出现 NaN/Infinity：%s", s)
	}
	if !strings.Contains(s, `"landed_percent":null`) {
		t.Fatalf("反例②：landed_percent 必须序列化为 null，got %s", s)
	}
	if !strings.Contains(s, `"insufficient":true`) {
		t.Fatalf("反例②：insufficient 标志必现，got %s", s)
	}
}

func TestGoalNegativeWeightInsufficient(t *testing.T) {
	ov := &boardOverrideGoal{
		Milestones: []boardOverrideMilestone{
			{ID: "M1", Title: "正权重", Weight: 1, DonePercent: floatPtr(100)},
			{ID: "M2", Title: "负权重", Weight: -0.5, DonePercent: floatPtr(50)},
		},
	}
	pg := buildProjectGoal(ov, fixedTime())
	if !pg.Insufficient || pg.LandedPercent != nil {
		t.Fatalf("负权重时必须整块 insufficient 且 landed_percent==nil，got=%+v", pg)
	}
	if !strings.Contains(pg.InsufficientReason, "M2") {
		t.Fatalf("insufficient_reason 应指明肇事里程碑，got %q", pg.InsufficientReason)
	}
}

func TestGoalEvidenceStaleMarksStaleAndInsufficient(t *testing.T) {
	dir := t.TempDir()
	fx := filepath.Join(dir, "gates.json")
	writeJSONFile(t, fx, map[string]any{
		"gate_counts": map[string]any{"pass": 9, "blocked": 12},
	})
	backdateFile(t, fx, 48) // 48 小时前，超 24h 上限
	ov := &boardOverrideGoal{
		Milestones: []boardOverrideMilestone{
			{
				ID: "M4", Title: "gates", Weight: 1,
				Evidence: &boardOverrideEvidence{
					Path: fx, Numerator: "gate_counts.pass",
					Denominator: []string{"gate_counts.pass", "gate_counts.blocked"},
					MaxAgeHours: 24,
				},
			},
		},
	}
	pg := buildProjectGoal(ov, fixedTime())
	m := pg.Milestones[0]
	if !m.Stale {
		t.Fatal("超龄时 stale 必须为 true")
	}
	if !m.Insufficient || m.DonePercent != nil {
		t.Fatal("stale 里程碑必须同时 insufficient 且 DonePercent==nil")
	}
	// 快照断言：stale 字段必须在 JSON 里可见
	b, _ := json.Marshal(m)
	if !strings.Contains(string(b), `"stale":true`) {
		t.Fatalf("stale=true 必须序列化可见：%s", string(b))
	}
}

func TestGoalMixedManualAndEvidenceGoalSource(t *testing.T) {
	dir := t.TempDir()
	fx := filepath.Join(dir, "gates.json")
	writeJSONFile(t, fx, map[string]any{
		"gate_counts": map[string]any{"pass": 9, "blocked": 12},
	})
	ov := &boardOverrideGoal{
		AsOf: "2026-07-23",
		Milestones: []boardOverrideMilestone{
			{ID: "M1", Title: "设计", Weight: 1, DonePercent: floatPtr(100)},
			{
				ID: "M4", Title: "gates", Weight: 1,
				Evidence: &boardOverrideEvidence{
					Path: fx, Numerator: "gate_counts.pass",
					Denominator: []string{"gate_counts.pass", "gate_counts.blocked"},
					MaxAgeHours: 24,
				},
			},
		},
	}
	pg := buildProjectGoal(ov, fixedTime())
	if pg.GoalSource != "mixed@2026-07-23" {
		t.Fatalf("混合 manual+evidence 时 goal_source 应 mixed@as_of，got %q", pg.GoalSource)
	}
}

// ---- 集成层：走完整 buildSnapshot，确认 omitempty 与 JSON 契约 ----

// bootBoardRoot 构造一个含 config.json + 一张任务卡的 root，供 buildSnapshot 调用。
// 项目目录（/tmp/goalproj）走归并，最终生成一个 project.id="goalproj"。
func bootBoardRoot(t *testing.T, boardJSON string) string {
	t.Helper()
	root := testRoot(t)
	if err := saveConfig(root, defaultConfig("claude")); err != nil {
		t.Fatal(err)
	}
	tk := newTask(root, testCfg(), typeSequence, "落地进度", "/tmp/goalproj", []string{"跑一步"}, 5)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if boardJSON != "" {
		if err := os.WriteFile(filepath.Join(root, "board.json"), []byte(boardJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestBuildSnapshotOmitsGoalWhenNotConfigured(t *testing.T) {
	root := bootBoardRoot(t, "") // 无 board.json
	snap, err := buildSnapshot(root, fixedTime())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Projects) == 0 {
		t.Fatal("测试前置：应至少有一个 project")
	}
	p := snap.Projects[0]
	if p.Goal != nil {
		t.Fatalf("goal 缺失场景下 Project.Goal 必须为 nil，got %+v", p.Goal)
	}
	b, _ := json.Marshal(p)
	if strings.Contains(string(b), `"goal":`) {
		t.Fatalf("JSON 里不得出现 goal 键（前端契约：不显示该区块），got %s", string(b))
	}
}

func TestBuildSnapshotIncludesGoalWhenConfigured(t *testing.T) {
	boardJSON := `{
  "projects": {
    "goalproj": {
      "goal": {
        "statement": "落地实际使用",
        "as_of": "2026-07-23",
        "milestones": [
          {"id":"M1","title":"设计","weight":1,"done_percent":100,"basis":"REVIEW Go"},
          {"id":"M2","title":"控制面","weight":2,"done_percent":50,"basis":"H2 收尾"}
        ]
      }
    }
  }
}`
	root := bootBoardRoot(t, boardJSON)
	snap, err := buildSnapshot(root, fixedTime())
	if err != nil {
		t.Fatal(err)
	}
	var pj *Project
	for _, p := range snap.Projects {
		if p.ID == "goalproj" {
			pj = p
			break
		}
	}
	if pj == nil {
		t.Fatal("找不到 goalproj，快照项目：")
	}
	if pj.Goal == nil {
		t.Fatal("goal 齐全时 Project.Goal 必须非 nil")
	}
	if pj.Goal.AsOf != "2026-07-23" || pj.Goal.GoalSource != "manual@2026-07-23" {
		t.Fatalf("Goal 元数据错：%+v", pj.Goal)
	}
	if pj.Goal.LandedPercent == nil || *pj.Goal.LandedPercent < 66.6 || *pj.Goal.LandedPercent > 66.8 {
		t.Fatalf("landed_percent 应 ~66.7，got %v", pj.Goal.LandedPercent)
	}
	if len(pj.Goal.Milestones) != 2 {
		t.Fatalf("里程碑应逐条透出 2 条，got %d", len(pj.Goal.Milestones))
	}
	// JSON 契约断言
	b, _ := json.Marshal(pj)
	s := string(b)
	if !strings.Contains(s, `"goal":`) || !strings.Contains(s, `"goal_source":"manual@2026-07-23"`) {
		t.Fatalf("JSON 契约缺失：%s", s)
	}
}

// TestProgressPercentUnchangedByGoal —— 回归：progress_percent 是「卡片进度」，
// 不因 goal 的存在与否而改变。CG-8 明确「只加不改」。
func TestProgressPercentUnchangedByGoal(t *testing.T) {
	noGoal := bootBoardRoot(t, "")
	withGoal := bootBoardRoot(t, `{
  "projects": {
    "goalproj": {
      "goal": {
        "statement": "任意",
        "milestones":[{"id":"M1","title":"x","weight":1,"done_percent":50}]
      }
    }
  }
}`)
	snapA, err := buildSnapshot(noGoal, fixedTime())
	if err != nil {
		t.Fatal(err)
	}
	snapB, err := buildSnapshot(withGoal, fixedTime())
	if err != nil {
		t.Fatal(err)
	}
	if snapA.Projects[0].ProgressPercent != snapB.Projects[0].ProgressPercent {
		t.Fatalf("progress_percent 不应因 goal 变化：无 goal=%v 有 goal=%v",
			snapA.Projects[0].ProgressPercent, snapB.Projects[0].ProgressPercent)
	}
	if snapA.Projects[0].Stats != snapB.Projects[0].Stats {
		t.Fatalf("stats 不应因 goal 变化")
	}
}
