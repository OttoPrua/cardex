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
	if got := buildProjectGoal(nil, "", fixedTime()); got != nil {
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
	pg := buildProjectGoal(ov, "", fixedTime())
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
	pg := buildProjectGoal(ov, "", fixedTime())
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
	if pg := buildProjectGoal(ov, "", fixedTime()); pg.LandedPercent == nil {
		t.Fatalf("前置：fixture 在场时应能合成，pg=%+v", pg)
	}
	// 删除 fixture 文件——模拟机械检查未落盘 / 被清理的场景
	if err := os.Remove(fx); err != nil {
		t.Fatal(err)
	}
	pg := buildProjectGoal(ov, "", fixedTime())
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
	pg := buildProjectGoal(ov, "", fixedTime())
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
	pg = buildProjectGoal(ov, "", fixedTime())
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
	pg := buildProjectGoal(ov, "", fixedTime())
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
	pg := buildProjectGoal(ov, "", fixedTime())
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
	pg := buildProjectGoal(ov, "", fixedTime())
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
	pg := buildProjectGoal(ov, "", fixedTime())
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

// ================== Round-1/2/3 加固：按类闭合的红线 case ==================
//
// P1-1（round-3 加固）：evidence.path 强制绝对路径——相对路径无论解析到 CWD 还是 boardRoot,
//   都存在「同名文件静默兜底」的兜底路径，与 fail-honest 冲突。
// P1-2: manual done_percent 与 evidence pct 都要卡 [0, 100]；负值成对相消也要挡（num<0/den<=0）。
// P1-3: evidence 存在即独占，失败/超龄一律 insufficient，绝不回退人工值。
// P1-4: board.json 解析错误必须落错 + 透出 OverviewResp.BoardOverrideError。

// ---- P1-1（round-3）: evidence.path 强制绝对路径 ----

// TestGoalEvidenceRelativePathRejected 相对路径必须直接 insufficient——
// 即使 boardRoot 下确有同名文件也不行，因为「同名文件静默兜底」正是 fail-honest 要防的。
// 强证据：把 fixture 写到 boardRoot 下能被找到的位置，evidence.path 仍用相对串——
// 若代码回归到 filepath.Join(boardRoot, ev.Path) 会读到这份并算出 42.9%，测试报红。
func TestGoalEvidenceRelativePathRejected(t *testing.T) {
	boardRoot := t.TempDir()
	sub := filepath.Join(boardRoot, "subdir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	fx := filepath.Join(sub, "gates.json")
	writeJSONFile(t, fx, map[string]any{
		"gate_counts": map[string]any{"pass": 9, "blocked": 12},
	})
	ov := &boardOverrideGoal{
		Milestones: []boardOverrideMilestone{
			{
				ID: "M4", Title: "gates", Weight: 1,
				Evidence: &boardOverrideEvidence{
					Path:        "subdir/gates.json", // 相对路径——即使 boardRoot 下能找到也要拒
					Numerator:   "gate_counts.pass",
					Denominator: []string{"gate_counts.pass", "gate_counts.blocked"},
					MaxAgeHours: 24,
				},
			},
		},
	}
	pg := buildProjectGoal(ov, boardRoot, fixedTime())
	m := pg.Milestones[0]
	if !m.Insufficient {
		t.Fatalf("相对路径即使 boardRoot 下存在也必须 insufficient，got=%+v", m)
	}
	if m.DonePercent != nil {
		t.Fatalf("绝不能读出 42.9%%（相对路径静默兜底回归），got %v", *m.DonePercent)
	}
	if !strings.Contains(m.InsufficientReason, "绝对路径") {
		t.Fatalf("原因应指明须绝对路径，got %q", m.InsufficientReason)
	}
}

// TestGoalEvidenceRelativePathDoesNotWalkCWD 相对路径必须不走进程 CWD——
// 若代码回归到 os.Stat(相对路径) 会命中 CWD 里的同名文件，测试报红。
// round-3 后：相对路径直接拒绝，CWD 和 boardRoot 都不参与解析。
func TestGoalEvidenceRelativePathDoesNotWalkCWD(t *testing.T) {
	cwdDir := t.TempDir()
	fake := filepath.Join(cwdDir, "gates.json")
	writeJSONFile(t, fake, map[string]any{
		"gate_counts": map[string]any{"pass": 999, "blocked": 0},
	})
	oldCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwdDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCWD) })
	boardRoot := t.TempDir()
	ov := &boardOverrideGoal{
		Milestones: []boardOverrideMilestone{
			{
				ID: "M4", Title: "gates", Weight: 1,
				Evidence: &boardOverrideEvidence{
					Path:        "gates.json", // 相对
					Numerator:   "gate_counts.pass",
					Denominator: []string{"gate_counts.pass", "gate_counts.blocked"},
					MaxAgeHours: 24,
				},
			},
		},
	}
	pg := buildProjectGoal(ov, boardRoot, fixedTime())
	m := pg.Milestones[0]
	if !m.Insufficient {
		t.Fatalf("相对路径必须 insufficient（不得走 CWD 里的 %s），got=%+v", fake, m)
	}
	if m.DonePercent != nil {
		t.Fatalf("绝不能从 CWD 里读到 999/999 之类值，got done_percent=%v", *m.DonePercent)
	}
	if !strings.Contains(m.InsufficientReason, "绝对路径") {
		t.Fatalf("原因应指明须绝对路径，got %q", m.InsufficientReason)
	}
}

// TestGoalEvidenceRelativePathRejectedWhenBoardRootEmpty boardRoot 为空 + 相对路径 →
// 依然 insufficient（round-3 起相对路径策略与 boardRoot 是否为空无关）。
func TestGoalEvidenceRelativePathRejectedWhenBoardRootEmpty(t *testing.T) {
	ov := &boardOverrideGoal{
		Milestones: []boardOverrideMilestone{
			{
				ID: "M4", Title: "gates", Weight: 1,
				Evidence: &boardOverrideEvidence{
					Path:        "gates.json",
					Numerator:   "gate_counts.pass",
					Denominator: []string{"gate_counts.pass", "gate_counts.blocked"},
					MaxAgeHours: 24,
				},
			},
		},
	}
	pg := buildProjectGoal(ov, "", fixedTime())
	m := pg.Milestones[0]
	if !m.Insufficient {
		t.Fatal("相对路径必须 insufficient（无论 boardRoot 是否为空）")
	}
	if !strings.Contains(m.InsufficientReason, "绝对路径") {
		t.Fatalf("原因应指明须绝对路径，got %q", m.InsufficientReason)
	}
}

// TestGoalEvidenceAbsolutePathAccepted 绝对路径的 happy path 必须仍然成功——
// 承重契约的另一半：strict abs-only 不能把合法配置误杀。
func TestGoalEvidenceAbsolutePathAccepted(t *testing.T) {
	dir := t.TempDir() // t.TempDir 返回绝对路径
	fx := filepath.Join(dir, "gates.json")
	writeJSONFile(t, fx, map[string]any{
		"gate_counts": map[string]any{"pass": 9, "blocked": 12},
	})
	ov := &boardOverrideGoal{
		Milestones: []boardOverrideMilestone{
			{
				ID: "M4", Title: "gates", Weight: 1,
				Evidence: &boardOverrideEvidence{
					Path:        fx, // 绝对路径
					Numerator:   "gate_counts.pass",
					Denominator: []string{"gate_counts.pass", "gate_counts.blocked"},
					MaxAgeHours: 24,
				},
			},
		},
	}
	pg := buildProjectGoal(ov, "", fixedTime()) // boardRoot 空也应能读绝对路径
	m := pg.Milestones[0]
	if m.Insufficient || m.DonePercent == nil {
		t.Fatalf("绝对路径必须能读，got=%+v", m)
	}
	if *m.DonePercent < 42.8 || *m.DonePercent > 43.0 {
		t.Fatalf("done_percent 应 ≈42.9，got=%v", *m.DonePercent)
	}
}

// ---- P1-2: percent 越界拒绝 ----

// TestGoalManualDonePercentNegativeRejected 人工值负数必须拒绝（教训：round1 的 int64
// 截断对 -50 会算出 -49.9，前端把"-49.9%"当权威渲染；类似地 250 展示 250%）。
func TestGoalManualDonePercentNegativeRejected(t *testing.T) {
	ov := &boardOverrideGoal{
		Milestones: []boardOverrideMilestone{
			{ID: "M1", Title: "越界负", Weight: 1, DonePercent: floatPtr(-50)},
		},
	}
	pg := buildProjectGoal(ov, "", fixedTime())
	m := pg.Milestones[0]
	if !m.Insufficient || m.DonePercent != nil {
		t.Fatalf("done_percent=-50 必须 insufficient 且 DonePercent==nil，got=%+v", m)
	}
	if !strings.Contains(m.InsufficientReason, "越界") {
		t.Fatalf("原因应含'越界'，got %q", m.InsufficientReason)
	}
	// 越界唯一里程碑 → 整块 landed_percent 必为 nil（不得画负数百分数）
	if pg.LandedPercent != nil {
		t.Fatalf("唯一里程碑越界时 landed_percent 必为 nil，got=%v", *pg.LandedPercent)
	}
}

func TestGoalManualDonePercentAbove100Rejected(t *testing.T) {
	ov := &boardOverrideGoal{
		Milestones: []boardOverrideMilestone{
			{ID: "M1", Title: "越界超", Weight: 1, DonePercent: floatPtr(250)},
		},
	}
	pg := buildProjectGoal(ov, "", fixedTime())
	m := pg.Milestones[0]
	if !m.Insufficient || m.DonePercent != nil {
		t.Fatalf("done_percent=250 必须 insufficient 且 DonePercent==nil，got=%+v", m)
	}
	if pg.LandedPercent != nil {
		t.Fatalf("唯一里程碑越界时 landed_percent 必为 nil，got=%v", *pg.LandedPercent)
	}
}

// TestGoalManualDonePercentBoundaryAccepts 边界值 0 与 100 必须被接受。
func TestGoalManualDonePercentBoundaryAccepts(t *testing.T) {
	ov := &boardOverrideGoal{
		Milestones: []boardOverrideMilestone{
			{ID: "M1", Title: "0", Weight: 1, DonePercent: floatPtr(0)},
			{ID: "M2", Title: "100", Weight: 1, DonePercent: floatPtr(100)},
		},
	}
	pg := buildProjectGoal(ov, "", fixedTime())
	if pg.Milestones[0].Insufficient || pg.Milestones[1].Insufficient {
		t.Fatalf("边界值 0 / 100 必须接受，got=%+v", pg.Milestones)
	}
	if pg.LandedPercent == nil || *pg.LandedPercent < 49.9 || *pg.LandedPercent > 50.1 {
		t.Fatalf("合成应 (0+100)/2=50，got=%v", pg.LandedPercent)
	}
}

// TestGoalEvidencePercentAbove100Rejected pointer 配错让 evidence 折算超 100%
// （num=30, den=[10] → 300%）必须拒绝，不得直出"300%"。
func TestGoalEvidencePercentAbove100Rejected(t *testing.T) {
	dir := t.TempDir()
	fx := filepath.Join(dir, "bad.json")
	writeJSONFile(t, fx, map[string]any{
		"num": 30, "den": 10,
	})
	ov := &boardOverrideGoal{
		Milestones: []boardOverrideMilestone{
			{
				ID: "M4", Title: "gates", Weight: 1,
				Evidence: &boardOverrideEvidence{
					Path:        fx,
					Numerator:   "num",
					Denominator: []string{"den"},
					MaxAgeHours: 24,
				},
			},
		},
	}
	pg := buildProjectGoal(ov, "", fixedTime())
	m := pg.Milestones[0]
	if !m.Insufficient || m.DonePercent != nil {
		t.Fatalf("30/10=300%% 必须 insufficient，got=%+v", m)
	}
	if !strings.Contains(m.InsufficientReason, "越界") {
		t.Fatalf("原因应含'越界'，got %q", m.InsufficientReason)
	}
}

// TestGoalEvidencePercentJustAt100Accepted num=den 时因浮点尾巴可能算成 100.0000001，
// 但语义就是 100%，必须接受不误杀。
func TestGoalEvidencePercentJustAt100Accepted(t *testing.T) {
	dir := t.TempDir()
	fx := filepath.Join(dir, "full.json")
	writeJSONFile(t, fx, map[string]any{"num": 3, "den": 3})
	ov := &boardOverrideGoal{
		Milestones: []boardOverrideMilestone{
			{
				ID: "M4", Title: "gates", Weight: 1,
				Evidence: &boardOverrideEvidence{
					Path:        fx,
					Numerator:   "num",
					Denominator: []string{"den"},
					MaxAgeHours: 24,
				},
			},
		},
	}
	pg := buildProjectGoal(ov, "", fixedTime())
	m := pg.Milestones[0]
	if m.Insufficient || m.DonePercent == nil {
		t.Fatalf("num=den 应折算 100%%，got=%+v", m)
	}
	if *m.DonePercent < 99.9 || *m.DonePercent > 100.1 {
		t.Fatalf("done_percent 应 ≈100，got %v", *m.DonePercent)
	}
}

// TestGoalEvidencePercentNegativeRejected 兄弟位点：分子为负（或分母为负）时算出 pct<0，
// 必须拒绝——与 manual done_percent<0 对称。若代码回归"只拒 >100 不拒 <0"，测试报红。
// 教训：负百分比是"坏配置产出的编造读数"的一种，前端渲染 -30% 比"数据不足"糟糕得多。
func TestGoalEvidencePercentNegativeRejected(t *testing.T) {
	dir := t.TempDir()
	fx := filepath.Join(dir, "bad-neg.json")
	// num=-3, den=10 → -30%
	writeJSONFile(t, fx, map[string]any{"num": -3, "den": 10})
	ov := &boardOverrideGoal{
		Milestones: []boardOverrideMilestone{
			{
				ID: "M4", Title: "gates", Weight: 1,
				Evidence: &boardOverrideEvidence{
					Path:        fx,
					Numerator:   "num",
					Denominator: []string{"den"},
					MaxAgeHours: 24,
				},
			},
		},
	}
	pg := buildProjectGoal(ov, "", fixedTime())
	m := pg.Milestones[0]
	if !m.Insufficient || m.DonePercent != nil {
		t.Fatalf("num=-3/den=10 → -30%% 必须 insufficient，got=%+v", m)
	}
	if !strings.Contains(m.InsufficientReason, "负值") {
		t.Fatalf("原因应指明是负值，got %q", m.InsufficientReason)
	}
	// 唯一里程碑越界时 landed_percent 必为 nil
	if pg.LandedPercent != nil {
		t.Fatalf("唯一里程碑 pct<0 越界时 landed_percent 必为 nil，got=%v", *pg.LandedPercent)
	}
}

// ---- P1-1 兄弟位点（round-3）：符号相消攻击面 ----
//
// 阐释：单挡 pct<0 会被「分子/分母都是负 → pct 相消为正」绕过；单挡 den==0 会被 den<0 绕过。
// 三条 case 各杀一种同构缺陷：num<0 单负、den<0 单负（含 num=0/-0 陷阱）、num&den 双负相消。

// TestGoalEvidenceDoubleNegativeSneakThroughRejected 反例：fixture 里
// {pass:-9, blocked:-2}，num=-9/den=-11=+81.8%，看起来是合理读数。round-3 前的代码
// 只挡 pct<0/den==0，两负相消的正数直接入账；round-3 起 num<0/den<=0 各自单挡，此路封死。
func TestGoalEvidenceDoubleNegativeSneakThroughRejected(t *testing.T) {
	dir := t.TempDir()
	fx := filepath.Join(dir, "double-neg.json")
	writeJSONFile(t, fx, map[string]any{
		"gate_counts": map[string]any{"pass": -9, "blocked": -2},
	})
	ov := &boardOverrideGoal{
		Milestones: []boardOverrideMilestone{
			{
				ID: "M4", Title: "gates", Weight: 1,
				Evidence: &boardOverrideEvidence{
					Path:        fx,
					Numerator:   "gate_counts.pass",
					Denominator: []string{"gate_counts.pass", "gate_counts.blocked"},
					MaxAgeHours: 24,
				},
			},
		},
	}
	pg := buildProjectGoal(ov, "", fixedTime())
	m := pg.Milestones[0]
	if !m.Insufficient || m.DonePercent != nil {
		t.Fatalf("双负相消攻击 {pass:-9,blocked:-2} 必须 insufficient（不得算出 +81.8%%），got=%+v", m)
	}
	// 具体拒因应指向 num<0 或 den<=0（负值），不允许「pct 为负」这种被相消绕过的托词
	if !strings.Contains(m.InsufficientReason, "负值") && !strings.Contains(m.InsufficientReason, "≤ 0") {
		t.Fatalf("原因应指明 numerator 负 或 denominator ≤ 0，got %q", m.InsufficientReason)
	}
	if pg.LandedPercent != nil {
		t.Fatalf("双负相消攻击不得渗出 landed_percent=+81.8%%，got=%v", *pg.LandedPercent)
	}
}

// TestGoalEvidenceZeroNumeratorNegativeDenominatorRejected 反例：{pass:0, blocked:-5}，
// den=-5（不是 0，绕过 den==0）；num=0/den=-5 → -0.0（Go 里 -0.0 == 0.0，绕过 pct<0）。
// round-3 前会渗出 0% 读数；round-3 起 den<=0 单挡此路。
func TestGoalEvidenceZeroNumeratorNegativeDenominatorRejected(t *testing.T) {
	dir := t.TempDir()
	fx := filepath.Join(dir, "zero-num-neg-den.json")
	writeJSONFile(t, fx, map[string]any{
		"gate_counts": map[string]any{"pass": 0, "blocked": -5},
	})
	ov := &boardOverrideGoal{
		Milestones: []boardOverrideMilestone{
			{
				ID: "M4", Title: "gates", Weight: 1,
				Evidence: &boardOverrideEvidence{
					Path:        fx,
					Numerator:   "gate_counts.pass",
					Denominator: []string{"gate_counts.pass", "gate_counts.blocked"},
					MaxAgeHours: 24,
				},
			},
		},
	}
	pg := buildProjectGoal(ov, "", fixedTime())
	m := pg.Milestones[0]
	if !m.Insufficient || m.DonePercent != nil {
		t.Fatalf("num=0/den=-5 (-0 相消) 必须 insufficient，绝不得静默 0%%，got=%+v", m)
	}
	if !strings.Contains(m.InsufficientReason, "≤ 0") {
		t.Fatalf("原因应指明 denominator ≤ 0，got %q", m.InsufficientReason)
	}
}

// TestGoalEvidenceNegativeNumeratorRejected 反例：num<0 单独一路（den>0 时 pct 就是负数，
// pct<0 兜底也能拦；但按分类闭合，num<0 应在自己的检查点上就报出）。
// 强证据：insufficient_reason 必须指向 numerator，而不是 pct。
func TestGoalEvidenceNegativeNumeratorRejected(t *testing.T) {
	dir := t.TempDir()
	fx := filepath.Join(dir, "neg-num.json")
	writeJSONFile(t, fx, map[string]any{
		"gate_counts": map[string]any{"pass": -3, "blocked": 10},
	})
	ov := &boardOverrideGoal{
		Milestones: []boardOverrideMilestone{
			{
				ID: "M4", Title: "gates", Weight: 1,
				Evidence: &boardOverrideEvidence{
					Path:        fx,
					Numerator:   "gate_counts.pass",
					Denominator: []string{"gate_counts.pass", "gate_counts.blocked"},
					MaxAgeHours: 24,
				},
			},
		},
	}
	pg := buildProjectGoal(ov, "", fixedTime())
	m := pg.Milestones[0]
	if !m.Insufficient || m.DonePercent != nil {
		t.Fatalf("num<0 必须 insufficient，got=%+v", m)
	}
	if !strings.Contains(m.InsufficientReason, "numerator") {
		t.Fatalf("原因应指向 numerator（分类闭合的检查点），got %q", m.InsufficientReason)
	}
}

// TestGoalEvidenceNegativeDenominatorSumRejected 反例：den<0（分母求和为负），num>0 且合理。
// pct 结果是负数（num/负=负），pct<0 兜底也能拦；但按分类闭合，den<=0 应在自己的检查点上报出。
func TestGoalEvidenceNegativeDenominatorSumRejected(t *testing.T) {
	dir := t.TempDir()
	fx := filepath.Join(dir, "neg-den.json")
	// den = a + b = 3 + (-8) = -5
	writeJSONFile(t, fx, map[string]any{"num": 2, "a": 3, "b": -8})
	ov := &boardOverrideGoal{
		Milestones: []boardOverrideMilestone{
			{
				ID: "M4", Title: "gates", Weight: 1,
				Evidence: &boardOverrideEvidence{
					Path:        fx,
					Numerator:   "num",
					Denominator: []string{"a", "b"},
					MaxAgeHours: 24,
				},
			},
		},
	}
	pg := buildProjectGoal(ov, "", fixedTime())
	m := pg.Milestones[0]
	if !m.Insufficient || m.DonePercent != nil {
		t.Fatalf("den 求和 <0 必须 insufficient，got=%+v", m)
	}
	if !strings.Contains(m.InsufficientReason, "denominator") || !strings.Contains(m.InsufficientReason, "≤ 0") {
		t.Fatalf("原因应指向 denominator ≤ 0，got %q", m.InsufficientReason)
	}
}

// TestGoalEvidencePercentZeroAccepted 边界值 0%（num=0/den>0）必须接受——
// 这是"刚开工"的合法值。若代码把 0 也拒了就是误杀 fail-honest 的合法零。
func TestGoalEvidencePercentZeroAccepted(t *testing.T) {
	dir := t.TempDir()
	fx := filepath.Join(dir, "zero.json")
	writeJSONFile(t, fx, map[string]any{"num": 0, "den": 10})
	ov := &boardOverrideGoal{
		Milestones: []boardOverrideMilestone{
			{
				ID: "M4", Title: "gates", Weight: 1,
				Evidence: &boardOverrideEvidence{
					Path:        fx,
					Numerator:   "num",
					Denominator: []string{"den"},
					MaxAgeHours: 24,
				},
			},
		},
	}
	pg := buildProjectGoal(ov, "", fixedTime())
	m := pg.Milestones[0]
	if m.Insufficient || m.DonePercent == nil {
		t.Fatalf("num=0/den=10 应折算为 0%% 且接受（合法零），got=%+v", m)
	}
	if *m.DonePercent < -0.01 || *m.DonePercent > 0.01 {
		t.Fatalf("done_percent 应为 0，got %v", *m.DonePercent)
	}
}

// TestGoalLandedPercentBoundedByInputs 合成层（validWeightSum > 0 分支）产物必须在 [0,100]。
// 前置约束：每个 done_percent 已被两个入口卡在 [0,100]，权重 >=0；因此合成必落在 [0,100]。
// 这条测试用双端极值输入锁死"合成层不会因未来的 round1/权重换算改动而漂移出界"。
func TestGoalLandedPercentBoundedByInputs(t *testing.T) {
	ov := &boardOverrideGoal{
		Milestones: []boardOverrideMilestone{
			{ID: "M1", Title: "low", Weight: 1, DonePercent: floatPtr(0)},
			{ID: "M2", Title: "high", Weight: 1, DonePercent: floatPtr(100)},
			{ID: "M3", Title: "mid", Weight: 2, DonePercent: floatPtr(50)},
		},
	}
	pg := buildProjectGoal(ov, "", fixedTime())
	if pg.LandedPercent == nil {
		t.Fatal("合法输入必须能合成 landed_percent")
	}
	v := *pg.LandedPercent
	if v < 0 || v > 100 {
		t.Fatalf("合成层出界（承重契约破坏）：landed_percent=%v 不在 [0,100]", v)
	}
	// 也断言均权合成正确：(0+100+50*2)/4 = 50
	if v < 49.9 || v > 50.1 {
		t.Fatalf("合成算错，应 ~50，got %v", v)
	}
}

// ---- P1-2 兄弟位点：max_age_hours < 0 也是"配错但沉默生效"，拒绝 ----

func TestGoalEvidenceNegativeMaxAgeRejected(t *testing.T) {
	dir := t.TempDir()
	fx := filepath.Join(dir, "gates.json")
	writeJSONFile(t, fx, map[string]any{
		"gate_counts": map[string]any{"pass": 9, "blocked": 12},
	})
	ov := &boardOverrideGoal{
		Milestones: []boardOverrideMilestone{
			{
				ID: "M4", Title: "gates", Weight: 1,
				Evidence: &boardOverrideEvidence{
					Path:        fx,
					Numerator:   "gate_counts.pass",
					Denominator: []string{"gate_counts.pass", "gate_counts.blocked"},
					MaxAgeHours: -1, // 配错
				},
			},
		},
	}
	pg := buildProjectGoal(ov, "", fixedTime())
	m := pg.Milestones[0]
	if !m.Insufficient {
		t.Fatal("max_age_hours 为负必须 insufficient，绝不能兜底成'永不过期'")
	}
	if !strings.Contains(m.InsufficientReason, "max_age_hours") {
		t.Fatalf("原因应指明 max_age_hours 字段，got %q", m.InsufficientReason)
	}
}

// ---- P1-3: evidence 存在即独占，不回退人工值 ----

// TestGoalEvidenceFailDoesNotFallbackToManual 当同一里程碑**同时**配了 evidence 和
// done_percent，evidence 文件缺失时**必须**判 insufficient，DonePercent==nil；
// 若代码回归到"evidence 失败回落到人工值"，会得到 DonePercent==80。这条测试**杀死**
// 未来任何"贴心回退"型改动。
func TestGoalEvidenceFailDoesNotFallbackToManual(t *testing.T) {
	dir := t.TempDir()
	fx := filepath.Join(dir, "does-not-exist.json")
	ov := &boardOverrideGoal{
		AsOf: "2026-07-23",
		Milestones: []boardOverrideMilestone{
			{
				ID: "M4", Title: "gates", Weight: 1,
				DonePercent: floatPtr(80), // ← 若代码回退到人工值就会读出 80
				Evidence: &boardOverrideEvidence{
					Path:        fx,
					Numerator:   "gate_counts.pass",
					Denominator: []string{"gate_counts.pass", "gate_counts.blocked"},
					MaxAgeHours: 24,
				},
			},
		},
	}
	pg := buildProjectGoal(ov, "", fixedTime())
	m := pg.Milestones[0]
	if !m.Insufficient {
		t.Fatal("evidence 文件缺失 + 有 done_percent 时必须 insufficient（禁止回退人工值）")
	}
	if m.DonePercent != nil {
		t.Fatalf("绝不得落回人工值 80，got done_percent=%v", *m.DonePercent)
	}
	// source 必须体现 evidence 通道失败——不能标 manual@... 让读者误以为读的是人工值
	if !strings.HasPrefix(m.Source, "evidence@") {
		t.Fatalf("source 必须仍是 evidence@... 通道，got %q", m.Source)
	}
}

// TestGoalEvidenceStaleDoesNotFallbackToManual 同上，超龄 evidence 也不得落回人工值。
// 教训：超龄意味着「机械口径已过期」，人工值往往更旧，静默换掉 = 读数含义漂移。
func TestGoalEvidenceStaleDoesNotFallbackToManual(t *testing.T) {
	dir := t.TempDir()
	fx := filepath.Join(dir, "gates.json")
	writeJSONFile(t, fx, map[string]any{
		"gate_counts": map[string]any{"pass": 9, "blocked": 12},
	})
	backdateFile(t, fx, 48) // 超 24h 上限
	ov := &boardOverrideGoal{
		Milestones: []boardOverrideMilestone{
			{
				ID: "M4", Title: "gates", Weight: 1,
				DonePercent: floatPtr(80),
				Evidence: &boardOverrideEvidence{
					Path:        fx,
					Numerator:   "gate_counts.pass",
					Denominator: []string{"gate_counts.pass", "gate_counts.blocked"},
					MaxAgeHours: 24,
				},
			},
		},
	}
	pg := buildProjectGoal(ov, "", fixedTime())
	m := pg.Milestones[0]
	if !m.Stale || !m.Insufficient {
		t.Fatalf("超龄 + 有 done_percent 时必须同时 stale + insufficient，got=%+v", m)
	}
	if m.DonePercent != nil {
		t.Fatalf("绝不得落回人工值 80，got %v", *m.DonePercent)
	}
}

// TestGoalEvidencePointerFailDoesNotFallbackToManual 补 pointer 取不到数值这条通道
// （不是文件缺失、不是超龄，是取数失败）——同样绝不回退。
func TestGoalEvidencePointerFailDoesNotFallbackToManual(t *testing.T) {
	dir := t.TempDir()
	fx := filepath.Join(dir, "gates.json")
	writeJSONFile(t, fx, map[string]any{
		"gate_counts": "9/21", // 反例①的字符串形态
	})
	ov := &boardOverrideGoal{
		Milestones: []boardOverrideMilestone{
			{
				ID: "M4", Title: "gates", Weight: 1,
				DonePercent: floatPtr(80),
				Evidence: &boardOverrideEvidence{
					Path:        fx,
					Numerator:   "gate_counts.pass",
					Denominator: []string{"gate_counts.pass", "gate_counts.blocked"},
					MaxAgeHours: 24,
				},
			},
		},
	}
	pg := buildProjectGoal(ov, "", fixedTime())
	m := pg.Milestones[0]
	if !m.Insufficient || m.DonePercent != nil {
		t.Fatalf("pointer 取不到数值时不得落回人工值，got=%+v", m)
	}
}

// ---- P1-4: board.json 解析错误必须落错 + 透出 ----

// TestLoadBoardOverrideReportsParseError 写坏 JSON（jsonc 注释）→ 返回空 override
// **且**错误串非空；调用方必须能看到这个错误。
func TestLoadBoardOverrideReportsParseError(t *testing.T) {
	root := t.TempDir()
	// jsonc 风格的注释——标准 JSON 不接受，encoding/json 会返回 syntax error
	bad := `{
  // 项目覆盖块
  "projects": {"foo": {"name": "Foo"}}
}`
	if err := os.WriteFile(filepath.Join(root, "board.json"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	ov, errMsg, errKind := loadBoardOverride(root)
	if ov == nil {
		t.Fatal("返回值 override 不应为 nil")
	}
	if len(ov.Projects) != 0 {
		t.Fatalf("解析失败时 projects 应为空，got=%+v", ov.Projects)
	}
	if errMsg == "" {
		t.Fatal("解析错误必须返回非空错误串（fail-honest 承重契约）")
	}
	if !strings.Contains(errMsg, "board.json") {
		t.Fatalf("错误串应指明是 board.json，got %q", errMsg)
	}
	// 【R3·P1-2】jsonc 注释=语法错,kind 必须是 syntax（整块丢）。
	if errKind != overrideErrKindSyntax {
		t.Fatalf("jsonc 注释应归类为 syntax，got kind=%q", errKind)
	}
}

// TestLoadBoardOverrideMissingIsNotError 文件不存在 = 未配置，不是错。
func TestLoadBoardOverrideMissingIsNotError(t *testing.T) {
	root := t.TempDir()
	ov, errMsg, errKind := loadBoardOverride(root)
	if ov == nil {
		t.Fatal("文件缺失时应返回空 override，不该 nil")
	}
	if errMsg != "" {
		t.Fatalf("文件不存在不该报错，got %q", errMsg)
	}
	if errKind != "" {
		t.Fatalf("文件不存在时 kind 应为空，got %q", errKind)
	}
}

// TestBuildSnapshotSurfacesBoardOverrideError 快照层必须把错误串挂到 BoardOverrideError，
// 供 handler 塞进 OverviewResp。
func TestBuildSnapshotSurfacesBoardOverrideError(t *testing.T) {
	root := bootBoardRoot(t, `{"projects": {`) // 断裂 JSON
	snap, err := buildSnapshot(root, fixedTime())
	if err != nil {
		t.Fatal(err)
	}
	if snap.BoardOverrideError == "" {
		t.Fatal("snap 必须携带解析错误信息，供 handler 透出前端")
	}
	if !strings.Contains(snap.BoardOverrideError, "board.json") {
		t.Fatalf("错误应指明来源为 board.json，got %q", snap.BoardOverrideError)
	}
	// 同时 projects 里不该有任何 goal（override 全部失效）
	for _, p := range snap.Projects {
		if p.Goal != nil {
			t.Fatalf("override 解析失败时不得有 goal 生效，proj=%s goal=%+v", p.ID, p.Goal)
		}
	}
}

// TestBuildSnapshotBoardOverrideOkNoError 正常 board.json → BoardOverrideError 为空，
// 契约字段 omitempty 序列化时消失。
func TestBuildSnapshotBoardOverrideOkNoError(t *testing.T) {
	root := bootBoardRoot(t, `{
  "projects": {"goalproj": {"name": "Ok"}}
}`)
	snap, err := buildSnapshot(root, fixedTime())
	if err != nil {
		t.Fatal(err)
	}
	if snap.BoardOverrideError != "" {
		t.Fatalf("正常场景 BoardOverrideError 必须为空，got %q", snap.BoardOverrideError)
	}
}

// ================== Round-2 加固：类型错部分保留（P1-1）==================
//
// 教训：CG-8 给 board.json 首次引入数值字段（weight/done_percent/max_age_hours），
// 委托人此前在 desc 里写自由文本，写 "weight":"1" / "done_percent":"50%" 是高概率手误。
// 若一处类型手误就把整块 override（含所有项目的 name/desc/phases/goal）连坐蒸发，
// 与"根本没配 override"外观完全相同——静默连坐 = 造读数，违反 fail-honest。
//
// Go encoding/json 语义：*UnmarshalTypeError 时 Unmarshal 已 skip 该字段并**继续填充**
// 剩余字段。因此类型错必须保留部分结果并显式披露；语法错才不得不整块丢弃。

// TestLoadBoardOverrideTypeErrorPreservesOtherFields 类型错时同一项目 name/desc 仍生效。
// **强证据**：milestone.weight 写成字符串（*json.UnmarshalTypeError 场景），
// 若代码回归"任何错就返空 override"，这条测试会红——因为 desc 覆盖不会生效。
func TestLoadBoardOverrideTypeErrorPreservesOtherFields(t *testing.T) {
	root := t.TempDir()
	// goal.milestones[0].weight 是字符串（高概率手误），触发 *json.UnmarshalTypeError
	bad := `{
  "projects": {
    "typo": {
      "name": "Typo",
      "desc": "有类型手误但 name/desc 仍要生效",
      "phases": {"exec": "开跑"},
      "goal": {
        "statement": "落地",
        "milestones": [
          {"id":"M1","title":"设计","weight":"1","done_percent":100}
        ]
      }
    },
    "ok": {"name": "Ok", "desc": "无手误项目"}
  }
}`
	if err := os.WriteFile(filepath.Join(root, "board.json"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	ov, errMsg, errKind := loadBoardOverride(root)
	if errMsg == "" {
		t.Fatal("类型错必须披露 errMsg，禁止静默降级")
	}
	if ov == nil {
		t.Fatal("类型错时 override 不应 nil")
	}
	// 【R3·P1-2】类型错必须归类为 type,让前端横幅写"部分覆盖仍生效,出错字段已跳过"；
	// 若被误归类 syntax,前端会写"全部失效"——与本用例断言的 name/desc/phases 仍生效矛盾,
	// 披露自身失实即 fail-honest 卡自破线。
	if errKind != overrideErrKindType {
		t.Fatalf("weight 类型错必须归类 type（fail-honest 披露必须诚实），got kind=%q", errKind)
	}
	// 关键断言：Go 语义在类型错后已 skip 该字段并继续填充——name/desc/phases 必须生效。
	po, ok := ov.Projects["typo"]
	if !ok {
		t.Fatal("类型错处项目本身应仍在 map 里（Go 语义：skip 出错字段但继续填充其它字段）")
	}
	if po.Name != "Typo" {
		t.Fatalf("类型错项目 name 覆盖必须仍生效，got %q", po.Name)
	}
	if po.Desc != "有类型手误但 name/desc 仍要生效" {
		t.Fatalf("类型错项目 desc 覆盖必须仍生效，got %q", po.Desc)
	}
	if po.Phases["exec"] != "开跑" {
		t.Fatalf("类型错项目 phases 覆盖必须仍生效，got %+v", po.Phases)
	}
	// 无手误的旁路项目必须完全生效
	po2, ok := ov.Projects["ok"]
	if !ok || po2.Name != "Ok" || po2.Desc != "无手误项目" {
		t.Fatalf("无手误旁路项目必须完整生效，got ok=%v %+v", ok, po2)
	}
}

// TestBuildSnapshotTypeErrorSurfacesErrorAndKeepsOverride 集成层：类型错也必须
// 挂 BoardOverrideError 供前端披露，同时其它字段仍生效（不连坐蒸发）。
func TestBuildSnapshotTypeErrorSurfacesErrorAndKeepsOverride(t *testing.T) {
	bad := `{
  "projects": {
    "goalproj": {
      "name": "有覆盖 Name",
      "desc": "有覆盖 Desc",
      "goal": {"statement":"落地","milestones":[{"id":"M1","title":"x","weight":"1","done_percent":50}]}
    }
  }
}`
	root := bootBoardRoot(t, bad)
	snap, err := buildSnapshot(root, fixedTime())
	if err != nil {
		t.Fatal(err)
	}
	// 披露必须落地
	if snap.BoardOverrideError == "" {
		t.Fatal("类型错必须挂 BoardOverrideError（fail-honest 承重契约）")
	}
	// 但 name/desc 覆盖仍要生效——不能连坐蒸发
	var pj *Project
	for _, p := range snap.Projects {
		if p.ID == "goalproj" {
			pj = p
			break
		}
	}
	if pj == nil {
		t.Fatal("找不到 goalproj")
	}
	if pj.Name != "有覆盖 Name" {
		t.Fatalf("类型错时项目 name 覆盖必须仍生效（不连坐），got %q", pj.Name)
	}
	if pj.Desc != "有覆盖 Desc" {
		t.Fatalf("类型错时项目 desc 覆盖必须仍生效（不连坐），got %q", pj.Desc)
	}
}

// TestLoadBoardOverrideSyntaxErrorDropsAll 语法错（如括号不闭合、抄了 jsonc 注释）
// 无法部分保留，必须整块丢弃 + 披露。这条测试锁死"语法错也保留部分"这种误改。
func TestLoadBoardOverrideSyntaxErrorDropsAll(t *testing.T) {
	root := t.TempDir()
	// 断裂 JSON——语法错，非类型错
	bad := `{"projects": {"foo": {"name":"Foo"`
	if err := os.WriteFile(filepath.Join(root, "board.json"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	ov, errMsg, errKind := loadBoardOverride(root)
	if errMsg == "" {
		t.Fatal("语法错必须披露 errMsg")
	}
	if ov == nil {
		t.Fatal("语法错时 override 不应 nil（应是空壳）")
	}
	if len(ov.Projects) != 0 {
		t.Fatalf("语法错必须整块丢，projects 应为空，got=%+v", ov.Projects)
	}
	// 【R3·P1-2】语法错必须归类 syntax（前端渲染"覆盖全部失效"）。
	if errKind != overrideErrKindSyntax {
		t.Fatalf("断裂 JSON 应归类 syntax，got kind=%q", errKind)
	}
}

// TestLoadBoardOverrideCommentErrorDropsAll 常见坑：委托人抄 README jsonc 示例——
// 单行注释 `//` 是 JSON 语法错。必须整块丢 + 披露，且提示串要含"注释/尾逗号"帮助自诊。
func TestLoadBoardOverrideCommentErrorDropsAll(t *testing.T) {
	root := t.TempDir()
	bad := `{
  // 这行注释是 jsonc，不是 JSON——常见抄示例的坑
  "projects": {"foo": {"name":"Foo"}}
}`
	if err := os.WriteFile(filepath.Join(root, "board.json"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	ov, errMsg, errKind := loadBoardOverride(root)
	if errMsg == "" {
		t.Fatal("含注释的 board.json 必须披露 errMsg")
	}
	if !strings.Contains(errMsg, "注释") {
		t.Fatalf("错误串应提示注释/尾逗号问题，got %q", errMsg)
	}
	if len(ov.Projects) != 0 {
		t.Fatalf("语法错必须整块丢弃，projects 应为空，got=%+v", ov.Projects)
	}
	// 【R3·P1-2】注释=语法错,kind=syntax。
	if errKind != overrideErrKindSyntax {
		t.Fatalf("注释 → 语法错,应归类 syntax，got kind=%q", errKind)
	}
}
