package main

// boardgoal.go — CG-8 「目标锚定进度」（landed progress）。
//
// 为什么加这层：现有的 progressPercent 只回答「派出的活干完了多少」
// （完成卡数 / (总卡数 - 已取消)），但不回答「离项目目标多远」。
// 委托人已在 board.json 的 desc 字段用一段静态快照文案顶着（2026-07-23），
// 本文件把这份人工评估机械化，同时保留人工兜底与 fail-honest 兜底。
//
// 两条纪律，从看板本体继承：
//   1) **只读**：evidence 只允许读文件，绝不执行命令，绝不写盘。看板挂在
//      生产队列数据上，任何写入都会污染真实队列；命令执行会打破「刷新一次页面
//      不产生副作用」的语义边界。落盘写数值的活由编排 session / 卡 完成，
//      看板只读它们的产出。
//   2) **fail-honest**：宁标「数据不足」也不编数字。历史教训——
//      终端回显造假（见 tool-output-reliability.md），闸门级结论必须交叉验证；
//      落地进度是决策级读数，任何插值/兜底/回退旧值都是造假，全部拒绝。

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// ---- 对外 JSON 结构（字段名进入契约，不得擅改）----

// ProjectGoal 是项目落地进度视图。**null 与零值语义不同**：
//   - LandedPercent = nil：数据不足，前端不得渲染百分数；
//   - LandedPercent 有值 + Partial=true：部分里程碑数据不足，合成值仅基于可用里程碑；
//   - Insufficient=true：整块无法合成（权重和 ≤ 0 或负权重），此时 LandedPercent 强制为 nil。
type ProjectGoal struct {
	Statement          string          `json:"statement"`
	AsOf               string          `json:"as_of,omitempty"`
	Milestones         []GoalMilestone `json:"milestones"`
	LandedPercent      *float64        `json:"landed_percent"`
	GoalSource         string          `json:"goal_source"`
	Insufficient       bool            `json:"insufficient,omitempty"`
	InsufficientReason string          `json:"insufficient_reason,omitempty"`
	Partial            bool            `json:"partial,omitempty"`
}

// GoalMilestone 是单个里程碑的展示态。DonePercent 与 ProjectGoal.LandedPercent 同理：
// nil 表示数据不足；Stale=true 表示 evidence 文件超龄，此时 DonePercent 必为 nil。
type GoalMilestone struct {
	ID                 string   `json:"id"`
	Title              string   `json:"title"`
	Weight             float64  `json:"weight"`
	DonePercent        *float64 `json:"done_percent"`
	Basis              string   `json:"basis,omitempty"`
	Source             string   `json:"source"`
	Insufficient       bool     `json:"insufficient,omitempty"`
	InsufficientReason string   `json:"insufficient_reason,omitempty"`
	Stale              bool     `json:"stale,omitempty"`
}

// ---- board.json 侧的输入结构（override）----

type boardOverrideGoal struct {
	Statement  string                    `json:"statement"`
	AsOf       string                    `json:"as_of"`
	Milestones []boardOverrideMilestone  `json:"milestones"`
}

type boardOverrideMilestone struct {
	ID          string                    `json:"id"`
	Title       string                    `json:"title"`
	Weight      float64                   `json:"weight"`
	// DonePercent 是**人工锚定**的百分比（0-100）。用指针以便区分「人工写了 0」
	// 与「没写这个字段」——json.Unmarshal 遇到缺失字段时保持 nil。
	DonePercent *float64                  `json:"done_percent,omitempty"`
	Basis       string                    `json:"basis,omitempty"`
	Evidence    *boardOverrideEvidence    `json:"evidence,omitempty"`
}

// boardOverrideEvidence 描述从落盘 JSON 自动取数的方式。
// Numerator 与 Denominator 都是「点分路径」，如 "gate_counts.pass"；
// Denominator 是路径**列表**，看板对每条路径的取数求和作为分母
// （典型用法：分子 pass，分母 [pass, blocked]，得到 pass/(pass+blocked)）。
//
// 为什么用两个独立字段而不是一个复合 pointer 字符串：
//   - 严格类型：分子必须是单一数值来源，分母是可加集合，语法上就区分开；
//   - 反例注入①防线：若 fixture 里 "gate_counts":"9/21" 是字符串，
//     "gate_counts.pass" 在字符串上无法钻取 → 直接判「数据不足」，
//     绝不会走「解析字符串成数字」这种编造路径。
type boardOverrideEvidence struct {
	Path        string   `json:"path"`
	Numerator   string   `json:"numerator"`
	Denominator []string `json:"denominator"`
	MaxAgeHours float64  `json:"max_age_hours"`
}

// ---- 折算主逻辑 ----

// buildProjectGoal 把 board.json 的 goal 覆盖块折算成对外 ProjectGoal。
// 返回 nil 表示「未配置目标」——前端契约要求此时完全不显示该区块。
// now 由调用方注入（便于测试与快照复现），evidence 的超龄判定基于文件 mtime。
func buildProjectGoal(ov *boardOverrideGoal, now time.Time) *ProjectGoal {
	if ov == nil {
		return nil
	}
	pg := &ProjectGoal{
		Statement:  ov.Statement,
		AsOf:       ov.AsOf,
		Milestones: make([]GoalMilestone, 0, len(ov.Milestones)),
	}

	// 第一遍：把每个 milestone 折算成对外态（含 stale/insufficient 判定）。
	// 单独一个 milestone 的失败不影响其它 milestone。
	hasEvidence := false
	for _, m := range ov.Milestones {
		gm := buildGoalMilestone(m, ov.AsOf, now)
		pg.Milestones = append(pg.Milestones, gm)
		if m.Evidence != nil {
			hasEvidence = true
		}
	}

	// 第二遍：整块权重体检 + 合成落地进度。
	// 任一 weight < 0 视为配置错误——落地进度是决策读数，允许负权重会得到
	// 一个「越推进越倒退」的鬼数字。整块判「数据不足」比编一个"合理"值更诚实。
	for _, m := range ov.Milestones {
		if m.Weight < 0 {
			pg.Insufficient = true
			pg.InsufficientReason = fmt.Sprintf("milestone %q 权重为负（%v）", m.ID, m.Weight)
			pg.GoalSource = "insufficient"
			return pg
		}
	}
	var totalWeight, validWeightSum, weightedDone float64
	validCount := 0
	insufficientCount := 0
	for i, m := range ov.Milestones {
		totalWeight += m.Weight
		if pg.Milestones[i].Insufficient {
			insufficientCount++
			continue
		}
		if pg.Milestones[i].DonePercent == nil {
			// 兜底：正常情况下 Insufficient=false 且 DonePercent 一定非 nil，
			// 走到这里说明 buildGoalMilestone 的两个字段状态不一致，视为 bug 数据不足。
			insufficientCount++
			continue
		}
		validWeightSum += m.Weight
		weightedDone += m.Weight * *pg.Milestones[i].DonePercent
		validCount++
	}
	if totalWeight <= 0 {
		// 反例注入②：milestones 权重和为 0（或全空）→ 整块「数据不足」，
		// 严禁显示 NaN/Inf/任何百分比。
		pg.Insufficient = true
		pg.InsufficientReason = "milestones 权重和为 0，无法合成"
		pg.GoalSource = "insufficient"
		return pg
	}

	if validWeightSum > 0 {
		v := round1(weightedDone / validWeightSum)
		pg.LandedPercent = &v
	}
	if insufficientCount > 0 {
		// 合成值仅基于可用里程碑时须明示 partial（若全部都不足，LandedPercent 就是 nil）。
		pg.Partial = true
	}

	// goal_source：全 manual 就写 manual@as_of；含 evidence 就写 mixed@as_of。
	// evidence 单独出现的场景（无手工里程碑）走 evidence 分支。
	hasManual := false
	for _, m := range ov.Milestones {
		if m.Evidence == nil {
			hasManual = true
			break
		}
	}
	switch {
	case hasEvidence && hasManual:
		pg.GoalSource = withAsOf("mixed", ov.AsOf)
	case hasEvidence:
		pg.GoalSource = "evidence"
	default:
		pg.GoalSource = withAsOf("manual", ov.AsOf)
	}
	return pg
}

func withAsOf(prefix, asOf string) string {
	if asOf == "" {
		return prefix
	}
	return prefix + "@" + asOf
}

// buildGoalMilestone 单个里程碑折算：evidence 优先（有配置就试）；失败/超龄回退到
// 人工 done_percent；两者都无 → 数据不足。**不做插值、不回退旧值**。
func buildGoalMilestone(m boardOverrideMilestone, asOf string, now time.Time) GoalMilestone {
	gm := GoalMilestone{
		ID:     m.ID,
		Title:  m.Title,
		Weight: m.Weight,
		Basis:  m.Basis,
	}
	if m.Evidence != nil {
		v, src, stale, reason := readEvidencePercent(m.Evidence, now)
		if stale {
			// 超龄一定标 Stale=true——快照断言依赖这个字段可见。
			gm.Stale = true
			gm.Insufficient = true
			gm.InsufficientReason = reason
			gm.Source = src // 保留 evidence@mtime 供前端展示「哪份数据过期了」
			return gm
		}
		if reason != "" {
			// 文件缺失 / pointer 取不到数值 / 分母为 0 等——里程碑级数据不足。
			// 严禁回退到 m.DonePercent（人工值）：这会把「已过期的机械口径」
			// 悄悄换成「更旧的人工估算」，读数含义漂移。
			gm.Insufficient = true
			gm.InsufficientReason = reason
			gm.Source = src
			return gm
		}
		rounded := round1(v)
		gm.DonePercent = &rounded
		gm.Source = src
		return gm
	}
	if m.DonePercent != nil {
		v := round1(*m.DonePercent)
		gm.DonePercent = &v
		gm.Source = withAsOf("manual", asOf)
		return gm
	}
	gm.Insufficient = true
	gm.InsufficientReason = "既无 evidence 也无 done_percent"
	gm.Source = "insufficient"
	return gm
}

// readEvidencePercent 从 evidence.Path 指向的 JSON 里按 numerator/denominator 折算百分比。
// 返回：值(0-100)、source 标签、stale 标志、若非空则为「数据不足原因」。
//
// 反例注入①的关键：extractNumber 严格要求最终值是 JSON 数值（float64）。
// 若 fixture 里同名字段是字符串 "9/21"，钻取会在类型断言处失败，直接返回 not-ok；
// 绝不尝试把字符串 parse 成数字——那种"贴心"回退等于替用户凭空造读数。
func readEvidencePercent(ev *boardOverrideEvidence, now time.Time) (float64, string, bool, string) {
	src := "evidence@" + ev.Path
	if ev.Path == "" {
		return 0, "insufficient", false, "evidence.path 为空"
	}
	info, err := os.Stat(ev.Path)
	if err != nil {
		return 0, src, false, "evidence 文件不存在或不可读: " + err.Error()
	}
	mtime := info.ModTime()
	src = "evidence@" + ev.Path + "@" + mtime.UTC().Format(time.RFC3339)
	if ev.MaxAgeHours > 0 {
		age := now.Sub(mtime)
		maxAge := time.Duration(ev.MaxAgeHours * float64(time.Hour))
		if age > maxAge {
			return 0, src, true, fmt.Sprintf("evidence 已超龄 (age=%s > max=%s)", age.Round(time.Second), maxAge.Round(time.Second))
		}
	}
	data, err := os.ReadFile(ev.Path)
	if err != nil {
		return 0, src, false, "读 evidence 文件失败: " + err.Error()
	}
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return 0, src, false, "evidence JSON 解析失败: " + err.Error()
	}
	num, ok := extractNumber(root, ev.Numerator)
	if !ok {
		return 0, src, false, fmt.Sprintf("evidence numerator %q 取不到数值", ev.Numerator)
	}
	if len(ev.Denominator) == 0 {
		return 0, src, false, "evidence denominator 未配置"
	}
	den := 0.0
	for _, p := range ev.Denominator {
		v, ok := extractNumber(root, p)
		if !ok {
			return 0, src, false, fmt.Sprintf("evidence denominator %q 取不到数值", p)
		}
		den += v
	}
	if den == 0 {
		// 除零守护。落地进度是决策读数，NaN/Inf 一旦渗出会污染合成值。
		return 0, src, false, "evidence denominator 求和为 0"
	}
	pct := num / den * 100
	if pct < 0 {
		// 负百分比在语义上就是坏数据（分子/分母有一个是负），拒绝。
		return 0, src, false, fmt.Sprintf("evidence 折算为负值 (num=%v den=%v)", num, den)
	}
	return pct, src, false, ""
}

// extractNumber 按点分路径钻取 JSON，只在最终值为数值时返回 (v, true)。
// 严格性：中途遇到非 map 或键缺失即返回 (0, false)；终值是字符串/bool/null/数组
// 一律视为「取不到数值」，不做类型强转。**这是反例注入①的第一道防线。**
func extractNumber(root any, path string) (float64, bool) {
	if path == "" {
		return 0, false
	}
	cur := root
	for _, p := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return 0, false
		}
		v, ok := m[p]
		if !ok {
			return 0, false
		}
		cur = v
	}
	n, ok := cur.(float64)
	if !ok {
		return 0, false
	}
	return n, true
}
