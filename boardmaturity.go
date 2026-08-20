package main

// boardmaturity.go — 证据驱动的结构成熟度进度。
//
// 这层只回答「设计目标离可验收实现还有多远」，不回答「Cardex 已派出的卡完成了多少」。
// 后者继续由 ProgressPercent 原样承担。成熟度数据来自版本化 JSON 合同；看板只读、校验、
// 重算，绝不从 done 卡数、代码量、提交量、测试数量或运行进程推导晋级。
//
// v1/v2 固定六档计分：
//   设计/待开发=0；遗留待迁移=1；开发/复审中=1；隔离候选=2；
//   主线/集成已有=3；Live/有界 Canary=4。
// fresh review 可以让状态回退。迁移 ledger 的负分是合法信息，不会被钳成 0。
// v2 额外要求每个能力切片声明一个稳定的主要工作性质（设计/落地/修复/审核/协调）。
// kind 只负责拆分展示，绝不改变成熟度状态或分值。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	maturitySchemaV1 = "cardex-maturity-progress/v1"
	maturitySchemaV2 = "cardex-maturity-progress/v2"
)

var maturityStateScores = map[string]int{
	"设计/待开发":         0,
	"遗留待迁移":          1,
	"开发/复审中":         1,
	"隔离候选":           2,
	"主线/集成已有":        3,
	"Live/有界 Canary": 4,
}

var requiredMaturityEvidenceLayers = []string{"design", "cardex", "git_test", "runtime"}

// boardMaturitySource 是 board.json 单项目的只读合同指针。
// Project 为空时按看板自动推导的项目名匹配；Path 必须是绝对路径。
type boardMaturitySource struct {
	Path        string  `json:"path"`
	Project     string  `json:"project,omitempty"`
	MaxAgeHours float64 `json:"max_age_hours,omitempty"`
}

type maturityDenominator struct {
	Version          string `json:"version"`
	CapabilitySlices int    `json:"capability_slices"`
	MaxPoints        int    `json:"max_points"`
	WeightedByEffort bool   `json:"weighted_by_effort"`
}

type maturityEvidence struct {
	Layer      string `json:"layer"`
	ObservedAt string `json:"observed_at"`
	Source     string `json:"source"`
	Ref        string `json:"ref,omitempty"`
	Summary    string `json:"summary"`
}

type maturityETAContract struct {
	Target               string   `json:"target"`
	MinDays              *float64 `json:"min_days,omitempty"`
	MaxDays              *float64 `json:"max_days,omitempty"`
	ObservationFloorDays *float64 `json:"observation_floor_days,omitempty"`
	OpenEnded            bool     `json:"open_ended,omitempty"`
	Confidence           string   `json:"confidence"`
	Basis                string   `json:"basis"`
	AcceptanceGate       string   `json:"acceptance_gate"`
}

type maturityContractSlice struct {
	ID             string             `json:"id"`
	Order          int                `json:"order"`
	Phase          string             `json:"phase"`
	Goal           string             `json:"goal,omitempty"`
	Kind           string             `json:"kind,omitempty"`
	State          string             `json:"state"`
	EffortWeight   *float64           `json:"effort_weight,omitempty"`
	Evidence       []maturityEvidence `json:"evidence,omitempty"`
	AcceptanceGate string             `json:"acceptance_gate"`
	UserAccepted   bool               `json:"user_accepted,omitempty"`
}

type maturityContractProject struct {
	Project   string                  `json:"project"`
	ProjectID string                  `json:"project_id,omitempty"`
	ETA       *maturityETAContract    `json:"eta,omitempty"`
	Slices    []maturityContractSlice `json:"slices"`
}

type maturitySnapshotProject struct {
	Project   string `json:"project"`
	Points    int    `json:"points"`
	MaxPoints int    `json:"max_points"`
}

type maturitySnapshotRef struct {
	At        string                    `json:"at"`
	Points    int                       `json:"points"`
	MaxPoints int                       `json:"max_points"`
	Projects  []maturitySnapshotProject `json:"projects,omitempty"`
}

type maturityTransition struct {
	At                      string             `json:"at"`
	Project                 string             `json:"project"`
	SliceID                 string             `json:"slice_id"`
	Phase                   string             `json:"phase"`
	PreviousState           string             `json:"previous_state"`
	CurrentState            string             `json:"current_state"`
	Trigger                 string             `json:"trigger"`
	Evidence                []maturityEvidence `json:"evidence"`
	ExactSHA                string             `json:"exact_sha,omitempty"`
	ReviewVerdict           string             `json:"review_verdict,omitempty"`
	LiteralMainMerge        bool               `json:"literal_main_merge,omitempty"`
	LivePromotion           bool               `json:"live_promotion,omitempty"`
	UserAcceptancePromotion bool               `json:"user_acceptance_promotion,omitempty"`
}

type maturityActivity struct {
	CardexTotal   int `json:"cardex_total"`
	CardexDone    int `json:"cardex_done"`
	CardexFailed  int `json:"cardex_failed"`
	CardexHeld    int `json:"cardex_held"`
	CardexRunning int `json:"cardex_running"`
}

type maturityContract struct {
	SchemaVersion     string                    `json:"schema_version"`
	SnapshotAt        string                    `json:"snapshot_at"`
	Denominator       maturityDenominator       `json:"denominator"`
	StateScores       map[string]int            `json:"state_scores"`
	ReferenceSnapshot *maturitySnapshotRef      `json:"reference_snapshot,omitempty"`
	PreviousSync      *maturitySnapshotRef      `json:"previous_sync,omitempty"`
	Projects          []maturityContractProject `json:"projects"`
	MigrationLedger   []maturityTransition      `json:"migration_ledger"`
	EvidenceLayers    []maturityEvidence        `json:"evidence_layers"`
	Activity          *maturityActivity         `json:"activity,omitempty"`
}

// maturityCalcProject 是一次合同物化的内部中间态；放在包级仅为让快照引用校验复用。
type maturityCalcProject struct {
	in      *maturityContractProject
	points  int
	max     int
	work    float64
	workMax float64
	counts  map[string]int
	kinds   map[string]*maturityCalcKind
	slices  []MaturitySlice
}

type maturityCalcKind struct {
	points  int
	max     int
	work    float64
	workMax float64
	slices  int
}

// 下列类型是对前端的稳定展示契约；所有分数都由上面的状态重算，不信任输入聚合值。
type MaturitySlice struct {
	ID             string             `json:"id"`
	Order          int                `json:"order"`
	Phase          string             `json:"phase"`
	Goal           string             `json:"goal,omitempty"`
	Kind           string             `json:"kind,omitempty"`
	State          string             `json:"state"`
	Score          int                `json:"score"`
	MaxPoints      int                `json:"max_points"`
	Percent        float64            `json:"percent"`
	EffortWeight   *float64           `json:"effort_weight,omitempty"`
	Evidence       []maturityEvidence `json:"evidence,omitempty"`
	AcceptanceGate string             `json:"acceptance_gate"`
	UserAccepted   bool               `json:"user_accepted"`
}

type MaturityAggregate struct {
	Points    int     `json:"points"`
	MaxPoints int     `json:"max_points"`
	Percent   float64 `json:"percent"`
}

// MaturityKindProgress 与实发进度的 Project.kinds[] 使用同一组 key/label，
// 但分子分母来自能力切片成熟度分，而不是卡片状态。
type MaturityKindProgress struct {
	Key           string  `json:"key"`
	Label         string  `json:"label"`
	SliceCount    int     `json:"slice_count"`
	Points        int     `json:"points"`
	MaxPoints     int     `json:"max_points"`
	Percent       float64 `json:"percent"`
	WorkPoints    float64 `json:"work_points"`
	WorkMaxPoints float64 `json:"work_max_points"`
	WorkPercent   float64 `json:"work_percent"`
}

type MaturityETA struct {
	Target             string  `json:"target"`
	EarliestAt         *string `json:"earliest_at"`
	LatestAt           *string `json:"latest_at"`
	ObservationFloorAt *string `json:"observation_floor_at"`
	OpenEnded          bool    `json:"open_ended"`
	Confidence         string  `json:"confidence"`
	Basis              string  `json:"basis"`
	AcceptanceGate     string  `json:"acceptance_gate"`
}

type MaturityDelta struct {
	ReferenceAt     string  `json:"reference_at,omitempty"`
	ReferencePoints int     `json:"reference_points"`
	CurrentPoints   int     `json:"current_points"`
	NetPoints       int     `json:"net_points"`
	NetDeltaPP      float64 `json:"net_delta_pp"`
}

type MaturityLedgerSummary struct {
	PhaseRowsChanged         int `json:"phase_rows_changed"`
	ForwardTransitions       int `json:"forward_transitions"`
	EvidenceRegressions      int `json:"evidence_regressions"`
	NetPointGain             int `json:"net_point_gain"`
	LiteralMainMerges        int `json:"literal_main_merges"`
	LivePromotions           int `json:"live_promotions"`
	UserAcceptancePromotions int `json:"user_acceptance_promotions"`
}

type ProjectMaturity struct {
	Available          bool                   `json:"available"`
	InsufficientReason string                 `json:"insufficient_reason,omitempty"`
	Source             string                 `json:"source,omitempty"`
	SchemaVersion      string                 `json:"schema_version,omitempty"`
	DenominatorVersion string                 `json:"denominator_version,omitempty"`
	SnapshotAt         string                 `json:"snapshot_at,omitempty"`
	Project            string                 `json:"project,omitempty"`
	Points             int                    `json:"points"`
	MaxPoints          int                    `json:"max_points"`
	Percent            float64                `json:"percent"`
	StateCounts        map[string]int         `json:"state_counts,omitempty"`
	Kinds              []MaturityKindProgress `json:"kinds,omitempty"`
	Slices             []MaturitySlice        `json:"slices,omitempty"`
	EffortWeighted     bool                   `json:"effort_weighted"`
	WorkPoints         float64                `json:"work_points"`
	WorkMaxPoints      float64                `json:"work_max_points"`
	WorkPercent        float64                `json:"work_percent"`
	WorkBasis          string                 `json:"work_basis,omitempty"`
	ETA                *MaturityETA           `json:"eta,omitempty"`
	Delta              MaturityDelta          `json:"delta"`
	ProjectLedger      MaturityLedgerSummary  `json:"project_ledger"`
	SnapshotLedger     MaturityLedgerSummary  `json:"snapshot_ledger"`
	Portfolio          MaturityAggregate      `json:"portfolio"`
	EvidenceLayers     []maturityEvidence     `json:"evidence_layers,omitempty"`
	Activity           *maturityActivity      `json:"activity,omitempty"`
	Basis              string                 `json:"basis,omitempty"`
	Transitions        []maturityTransition   `json:"transitions,omitempty"`
}

func buildProjectMaturity(src *boardMaturitySource, boardProject string, now time.Time) *ProjectMaturity {
	if src == nil {
		return nil
	}
	fail := func(reason string) *ProjectMaturity {
		return &ProjectMaturity{Available: false, Source: src.Path, InsufficientReason: reason}
	}
	if src.Path == "" || !filepath.IsAbs(src.Path) {
		return fail(fmt.Sprintf("maturity.path 必须是绝对路径 (got %q)", src.Path))
	}
	if src.MaxAgeHours < 0 || math.IsNaN(src.MaxAgeHours) || math.IsInf(src.MaxAgeHours, 0) {
		return fail("maturity.max_age_hours 必须是有限非负数")
	}
	b, err := os.ReadFile(src.Path)
	if err != nil {
		return fail("读取成熟度合同失败: " + err.Error())
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c maturityContract
	if err := dec.Decode(&c); err != nil {
		return fail("成熟度合同 JSON/字段解析失败: " + err.Error())
	}
	wantProject := strings.TrimSpace(src.Project)
	if wantProject == "" {
		wantProject = boardProject
	}
	view, err := materializeMaturity(&c, wantProject, now)
	if err != nil {
		return fail(err.Error())
	}
	view.Source = src.Path
	if src.MaxAgeHours > 0 {
		at, _ := time.Parse(time.RFC3339, c.SnapshotAt)
		age := now.Sub(at)
		if age > time.Duration(src.MaxAgeHours*float64(time.Hour)) {
			return fail(fmt.Sprintf("成熟度快照已超龄 (snapshot_at=%s, age=%s > max=%s)",
				c.SnapshotAt, age.Round(time.Second), time.Duration(src.MaxAgeHours*float64(time.Hour)).Round(time.Second)))
		}
	}
	return view
}

func materializeMaturity(c *maturityContract, wantProject string, now time.Time) (*ProjectMaturity, error) {
	if c == nil {
		return nil, fmt.Errorf("成熟度合同为空")
	}
	if c.SchemaVersion != maturitySchemaV1 && c.SchemaVersion != maturitySchemaV2 {
		return nil, fmt.Errorf("不支持的成熟度 schema_version %q", c.SchemaVersion)
	}
	requireKinds := c.SchemaVersion == maturitySchemaV2
	snapshotAt, err := time.Parse(time.RFC3339, c.SnapshotAt)
	if err != nil {
		return nil, fmt.Errorf("snapshot_at 不是 RFC3339: %v", err)
	}
	if snapshotAt.After(now.Add(time.Hour)) {
		return nil, fmt.Errorf("snapshot_at 位于未来超过 1 小时: %s", c.SnapshotAt)
	}
	if err := validateStateScores(c.StateScores); err != nil {
		return nil, err
	}
	if c.Denominator.Version == "" || c.Denominator.CapabilitySlices <= 0 ||
		c.Denominator.MaxPoints != c.Denominator.CapabilitySlices*4 {
		return nil, fmt.Errorf("denominator 无效：version/slices/max_points 必须满足 max_points=slices*4")
	}
	if err := validateEvidenceLayers(c.EvidenceLayers); err != nil {
		return nil, err
	}

	calcs := map[string]*maturityCalcProject{}
	bySlice := map[string]*maturityContractSlice{}
	portfolioPoints, portfolioMax := 0, 0
	totalSlices := 0
	for i := range c.Projects {
		p := &c.Projects[i]
		if strings.TrimSpace(p.Project) == "" {
			return nil, fmt.Errorf("projects[%d].project 为空", i)
		}
		pk := strings.ToLower(strings.TrimSpace(p.Project))
		if _, dup := calcs[pk]; dup {
			return nil, fmt.Errorf("项目重复: %s", p.Project)
		}
		cp := &maturityCalcProject{in: p, counts: map[string]int{}, kinds: map[string]*maturityCalcKind{}}
		seenOrder := map[int]bool{}
		for j := range p.Slices {
			s := &p.Slices[j]
			if s.ID == "" || s.Phase == "" || s.AcceptanceGate == "" {
				return nil, fmt.Errorf("%s slices[%d] 缺 id/phase/acceptance_gate", p.Project, j)
			}
			if requireKinds && !validKind(s.Kind) {
				return nil, fmt.Errorf("%s/%s 的 kind=%q 无效；v2 必须是 %s",
					p.Project, s.ID, s.Kind, strings.Join(kindOrder, "/"))
			}
			if s.Kind != "" && !validKind(s.Kind) {
				return nil, fmt.Errorf("%s/%s 使用未知 kind %q", p.Project, s.ID, s.Kind)
			}
			if seenOrder[s.Order] {
				return nil, fmt.Errorf("%s 的 slice order %d 重复", p.Project, s.Order)
			}
			seenOrder[s.Order] = true
			key := pk + "\x00" + strings.ToLower(s.ID)
			if _, dup := bySlice[key]; dup {
				return nil, fmt.Errorf("%s slice id 重复: %s", p.Project, s.ID)
			}
			bySlice[key] = s
			score, ok := maturityStateScores[s.State]
			if !ok {
				return nil, fmt.Errorf("%s/%s 使用未知成熟度状态 %q", p.Project, s.ID, s.State)
			}
			if s.UserAccepted && score < 4 {
				return nil, fmt.Errorf("%s/%s USER_ACCEPTED=true 但状态未达到 Live/有界 Canary", p.Project, s.ID)
			}
			if err := validateSliceEvidence(p.Project, s); err != nil {
				return nil, err
			}
			cp.points += score
			cp.max += 4
			cp.counts[s.State]++
			ms := MaturitySlice{ID: s.ID, Order: s.Order, Phase: s.Phase, Goal: s.Goal,
				Kind: s.Kind, State: s.State, Score: score, MaxPoints: 4, Percent: float64(score) * 25,
				EffortWeight: s.EffortWeight, Evidence: s.Evidence,
				AcceptanceGate: s.AcceptanceGate, UserAccepted: s.UserAccepted}
			cp.slices = append(cp.slices, ms)
			if s.Kind != "" {
				ck := cp.kinds[s.Kind]
				if ck == nil {
					ck = &maturityCalcKind{}
					cp.kinds[s.Kind] = ck
				}
				ck.points += score
				ck.max += 4
				ck.slices++
			}
			if c.Denominator.WeightedByEffort {
				if s.EffortWeight == nil || *s.EffortWeight <= 0 || math.IsNaN(*s.EffortWeight) || math.IsInf(*s.EffortWeight, 0) {
					return nil, fmt.Errorf("%s/%s 缺有效 effort_weight（合同声明 weighted_by_effort=true）", p.Project, s.ID)
				}
				cp.work += float64(score) * *s.EffortWeight
				cp.workMax += 4 * *s.EffortWeight
				if s.Kind != "" {
					cp.kinds[s.Kind].work += float64(score) * *s.EffortWeight
					cp.kinds[s.Kind].workMax += 4 * *s.EffortWeight
				}
			}
		}
		sort.Slice(cp.slices, func(i, j int) bool { return cp.slices[i].Order < cp.slices[j].Order })
		calcs[pk] = cp
		portfolioPoints += cp.points
		portfolioMax += cp.max
		totalSlices += len(p.Slices)
	}
	if totalSlices != c.Denominator.CapabilitySlices || portfolioMax != c.Denominator.MaxPoints {
		return nil, fmt.Errorf("分母漂移：合同声明 %d slices/%d points，逐项重算为 %d/%d",
			c.Denominator.CapabilitySlices, c.Denominator.MaxPoints, totalSlices, portfolioMax)
	}
	if err := validateSnapshotRef("reference_snapshot", c.ReferenceSnapshot, calcs, portfolioMax); err != nil {
		return nil, err
	}
	if err := validateSnapshotRef("previous_sync", c.PreviousSync, calcs, portfolioMax); err != nil {
		return nil, err
	}

	globalLedger, projectLedgers, transitions, err := validateMaturityLedger(c, bySlice)
	if err != nil {
		return nil, err
	}
	if c.PreviousSync != nil {
		if c.PreviousSync.MaxPoints != portfolioMax || c.PreviousSync.Points+globalLedger.NetPointGain != portfolioPoints {
			return nil, fmt.Errorf("previous_sync 与 migration_ledger 对不上：%d %+d != %d",
				c.PreviousSync.Points, globalLedger.NetPointGain, portfolioPoints)
		}
	}

	wk := strings.ToLower(strings.TrimSpace(wantProject))
	var cp *maturityCalcProject
	for key, candidate := range calcs {
		if key == wk || strings.EqualFold(candidate.in.ProjectID, wantProject) {
			cp = candidate
			break
		}
	}
	if cp == nil {
		return nil, fmt.Errorf("成熟度合同中找不到项目 %q", wantProject)
	}

	v := &ProjectMaturity{
		Available: true, SchemaVersion: c.SchemaVersion, DenominatorVersion: c.Denominator.Version,
		SnapshotAt: c.SnapshotAt, Project: cp.in.Project,
		Points: cp.points, MaxPoints: cp.max, Percent: percent(cp.points, cp.max),
		StateCounts: cp.counts, Slices: cp.slices,
		EffortWeighted: c.Denominator.WeightedByEffort,
		ProjectLedger:  projectLedgers[strings.ToLower(cp.in.Project)], SnapshotLedger: globalLedger,
		Portfolio:      MaturityAggregate{Points: portfolioPoints, MaxPoints: portfolioMax, Percent: percent(portfolioPoints, portfolioMax)},
		EvidenceLayers: c.EvidenceLayers, Activity: c.Activity,
		Basis: "结构成熟度按版本化能力切片的 0–4 状态求和；五类 kind 只拆分已取得分，不改变状态或分值；Cardex done、代码量、提交量、测试数量与进程存在只作活动/诊断证据，不触发晋级。",
	}
	for _, key := range kindOrder {
		ck := cp.kinds[key]
		if ck == nil || ck.max == 0 {
			continue
		}
		mk := MaturityKindProgress{
			Key: key, Label: kindLabel[key], SliceCount: ck.slices,
			Points: ck.points, MaxPoints: ck.max, Percent: percent(ck.points, ck.max),
		}
		if c.Denominator.WeightedByEffort {
			mk.WorkPoints, mk.WorkMaxPoints = round1(ck.work), round1(ck.workMax)
			mk.WorkPercent = round1(ck.work / ck.workMax * 100)
		} else {
			mk.WorkPoints, mk.WorkMaxPoints = float64(ck.points), float64(ck.max)
			mk.WorkPercent = percent(ck.points, ck.max)
		}
		v.Kinds = append(v.Kinds, mk)
	}
	if c.Denominator.WeightedByEffort {
		v.WorkPoints, v.WorkMaxPoints = round1(cp.work), round1(cp.workMax)
		v.WorkPercent = round1(cp.work / cp.workMax * 100)
		v.WorkBasis = "合同提供了逐切片 effort_weight；工时进度为 effort_weight×成熟度分的独立指数，不回写结构成熟度历史。"
	} else {
		v.WorkPoints, v.WorkMaxPoints, v.WorkPercent = float64(cp.points), float64(cp.max), percent(cp.points, cp.max)
		v.WorkBasis = "合同未声明可靠工时权重（weighted_by_effort=false）；工时档透明回退到未加权结构成熟度，不使用 turns 或 Cardex done 率。"
	}
	eta, err := materializeMaturityETA(cp.in.ETA, snapshotAt)
	if err != nil {
		return nil, fmt.Errorf("%s ETA 无效: %w", cp.in.Project, err)
	}
	v.ETA = eta
	v.Delta = maturityDeltaForProject(c.ReferenceSnapshot, cp.in.Project, cp.points, cp.max)
	v.Transitions = transitions[strings.ToLower(cp.in.Project)]
	return v, nil
}

func validateStateScores(got map[string]int) error {
	if len(got) != len(maturityStateScores) {
		return fmt.Errorf("state_scores 必须完整且只能包含固定口径的 %d 个状态", len(maturityStateScores))
	}
	for state, score := range maturityStateScores {
		v, ok := got[state]
		if !ok || v != score {
			return fmt.Errorf("state_scores[%q]=%d (present=%v)，必须为 %d", state, v, ok, score)
		}
	}
	return nil
}

func validateSnapshotRef(label string, ref *maturitySnapshotRef, calcs map[string]*maturityCalcProject, portfolioMax int) error {
	if ref == nil {
		return nil
	}
	if _, err := time.Parse(time.RFC3339, ref.At); err != nil {
		return fmt.Errorf("%s.at 非 RFC3339", label)
	}
	if ref.MaxPoints != portfolioMax || ref.Points < 0 || ref.Points > ref.MaxPoints {
		return fmt.Errorf("%s 总分无效: %d/%d（当前固定分母 %d）", label, ref.Points, ref.MaxPoints, portfolioMax)
	}
	seen := map[string]bool{}
	for _, p := range ref.Projects {
		key := strings.ToLower(strings.TrimSpace(p.Project))
		cp := calcs[key]
		if cp == nil || seen[key] || p.MaxPoints != cp.max || p.Points < 0 || p.Points > p.MaxPoints {
			return fmt.Errorf("%s project 记录无效: %s %d/%d", label, p.Project, p.Points, p.MaxPoints)
		}
		seen[key] = true
	}
	return nil
}

func validateEvidenceLayers(evs []maturityEvidence) error {
	seen := map[string]bool{}
	for i, e := range evs {
		if e.Layer == "" || e.ObservedAt == "" || e.Source == "" || e.Summary == "" {
			return fmt.Errorf("evidence_layers[%d] 缺 layer/observed_at/source/summary", i)
		}
		if _, err := time.Parse(time.RFC3339, e.ObservedAt); err != nil {
			return fmt.Errorf("evidence_layers[%d].observed_at 非 RFC3339: %v", i, err)
		}
		seen[e.Layer] = true
	}
	for _, layer := range requiredMaturityEvidenceLayers {
		if !seen[layer] {
			return fmt.Errorf("成熟度合同缺少 %s 四层对账证据", layer)
		}
	}
	return nil
}

func validateSliceEvidence(project string, s *maturityContractSlice) error {
	for i, e := range s.Evidence {
		if e.Layer == "" || e.Source == "" || e.Summary == "" || e.ObservedAt == "" {
			return fmt.Errorf("%s/%s evidence[%d] 缺字段", project, s.ID, i)
		}
		if _, err := time.Parse(time.RFC3339, e.ObservedAt); err != nil {
			return fmt.Errorf("%s/%s evidence[%d].observed_at 非 RFC3339", project, s.ID, i)
		}
	}
	return nil
}

func validateMaturityLedger(c *maturityContract, bySlice map[string]*maturityContractSlice) (
	MaturityLedgerSummary, map[string]MaturityLedgerSummary, map[string][]maturityTransition, error) {
	var all MaturityLedgerSummary
	byProject := map[string]MaturityLedgerSummary{}
	transitions := map[string][]maturityTransition{}
	seen := map[string]bool{}
	for i, tr := range c.MigrationLedger {
		pk := strings.ToLower(strings.TrimSpace(tr.Project))
		key := pk + "\x00" + strings.ToLower(tr.SliceID)
		if seen[key] {
			return all, nil, nil, fmt.Errorf("migration_ledger v1 每个 slice 只允许一条迁移，重复 %s/%s", tr.Project, tr.SliceID)
		}
		seen[key] = true
		s := bySlice[key]
		if s == nil {
			return all, nil, nil, fmt.Errorf("migration_ledger[%d] 指向未知 slice %s/%s", i, tr.Project, tr.SliceID)
		}
		prev, okPrev := maturityStateScores[tr.PreviousState]
		cur, okCur := maturityStateScores[tr.CurrentState]
		if !okPrev || !okCur {
			return all, nil, nil, fmt.Errorf("migration_ledger[%d] 使用未知状态", i)
		}
		if tr.CurrentState != s.State {
			return all, nil, nil, fmt.Errorf("migration_ledger[%d] current_state=%q 与 slice 当前状态 %q 不一致", i, tr.CurrentState, s.State)
		}
		if tr.At == "" || tr.Trigger == "" || len(tr.Evidence) == 0 {
			return all, nil, nil, fmt.Errorf("migration_ledger[%d] 缺 at/trigger/evidence", i)
		}
		if _, err := time.Parse(time.RFC3339, tr.At); err != nil {
			return all, nil, nil, fmt.Errorf("migration_ledger[%d].at 非 RFC3339", i)
		}
		delta := cur - prev
		one := byProject[pk]
		all.PhaseRowsChanged++
		one.PhaseRowsChanged++
		all.NetPointGain += delta
		one.NetPointGain += delta
		if delta > 0 {
			all.ForwardTransitions++
			one.ForwardTransitions++
		} else if delta < 0 {
			all.EvidenceRegressions++
			one.EvidenceRegressions++
		}
		if tr.LiteralMainMerge {
			all.LiteralMainMerges++
			one.LiteralMainMerges++
		}
		if tr.LivePromotion {
			all.LivePromotions++
			one.LivePromotions++
		}
		if tr.UserAcceptancePromotion {
			all.UserAcceptancePromotions++
			one.UserAcceptancePromotions++
		}
		byProject[pk] = one
		transitions[pk] = append(transitions[pk], tr)
	}
	return all, byProject, transitions, nil
}

func materializeMaturityETA(in *maturityETAContract, at time.Time) (*MaturityETA, error) {
	if in == nil {
		return nil, nil
	}
	if in.Target == "" || in.Confidence == "" || in.Basis == "" || in.AcceptanceGate == "" {
		return nil, fmt.Errorf("缺 target/confidence/basis/acceptance_gate")
	}
	for name, v := range map[string]*float64{"min_days": in.MinDays, "max_days": in.MaxDays, "observation_floor_days": in.ObservationFloorDays} {
		if v != nil && (*v < 0 || *v > 36500 || math.IsNaN(*v) || math.IsInf(*v, 0)) {
			return nil, fmt.Errorf("%s 必须是有限非负数", name)
		}
	}
	if in.MinDays != nil && in.MaxDays != nil && *in.MaxDays < *in.MinDays {
		return nil, fmt.Errorf("max_days 小于 min_days")
	}
	out := &MaturityETA{Target: in.Target, OpenEnded: in.OpenEnded, Confidence: in.Confidence,
		Basis: in.Basis, AcceptanceGate: in.AcceptanceGate}
	if in.MinDays != nil {
		x := at.Add(time.Duration(*in.MinDays * float64(24*time.Hour))).Format(time.RFC3339)
		out.EarliestAt = &x
	}
	if in.MaxDays != nil {
		x := at.Add(time.Duration(*in.MaxDays * float64(24*time.Hour))).Format(time.RFC3339)
		out.LatestAt = &x
	}
	if in.ObservationFloorDays != nil {
		x := at.Add(time.Duration(*in.ObservationFloorDays * float64(24*time.Hour))).Format(time.RFC3339)
		out.ObservationFloorAt = &x
	}
	return out, nil
}

func maturityDeltaForProject(ref *maturitySnapshotRef, project string, current, max int) MaturityDelta {
	d := MaturityDelta{CurrentPoints: current}
	if ref == nil {
		return d
	}
	d.ReferenceAt = ref.At
	for _, p := range ref.Projects {
		if strings.EqualFold(p.Project, project) {
			d.ReferencePoints = p.Points
			d.NetPoints = current - p.Points
			d.NetDeltaPP = round1((float64(current)/float64(max) - float64(p.Points)/float64(p.MaxPoints)) * 100)
			return d
		}
	}
	return d
}

func percent(points, max int) float64 {
	if max <= 0 {
		return 0
	}
	return math.Round(float64(points)/float64(max)*10000) / 100
}
