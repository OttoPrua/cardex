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
	"math"
	"os"
	"path/filepath"
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
//
// root 参数保留是历史约定（round-1 曾用它给相对 evidence.path 挂 boardRoot），
// round-3 起 evidence.path **强制要求绝对路径**——相对路径无论解析到 CWD 还是 boardRoot，
// 都存在「同名文件静默读错」的兜底路径（boardRoot 里存在同名脚手架/临时文件时零告警读错），
// 与本卡 fail-honest 纪律冲突。root 目前只作为占位保留，供未来扩展。
func buildProjectGoal(ov *boardOverrideGoal, root string, now time.Time) *ProjectGoal {
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
		gm := buildGoalMilestone(m, root, ov.AsOf, now)
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
	// 【R2·P1-1 尾巴】合成前非有限数守护——放在 totalWeight/validWeightSum 阈值
	// 判断**之前**是关键:极端权重(math.MaxFloat64 相乘溢出、NaN 权重)会让
	// weightedDone/validWeightSum 变成 Inf 或 NaN;NaN 会绕过 m.Weight<0 判断
	// (NaN<0 恒为 false)、绕过 totalWeight<=0 短路(NaN<=0 恒为 false)、绕过
	// validWeightSum>0 门(NaN>0 恒为 false)——三重比较全 false 让代码不进任何分支,
	// LandedPercent 保持 nil 但 Insufficient 也不置位,goal_source 仍标 manual/mixed
	// 虚报"入账"。round1 再把 Inf 转成任意 int64(Go 规范:超出 int64 值域的浮点
	// 转换是"实现相关",macOS/amd64 上 +Inf → 0,负数场景可能是 int64.min)。
	// 三条比较关口全用 math.IsNaN + math.IsInf 单独判定,任何非有限数直接整块 insufficient。
	// (反例:reason 字符串刻意避免出现 "NaN"/"Inf" 字面量,防止未来 JSON 契约测试
	// 用 strings.Contains 误命中——契约挡的是"数值渗出",不是"文字提及"。)
	if math.IsNaN(totalWeight) || math.IsInf(totalWeight, 0) ||
		math.IsNaN(validWeightSum) || math.IsInf(validWeightSum, 0) ||
		math.IsNaN(weightedDone) || math.IsInf(weightedDone, 0) {
		pg.Insufficient = true
		pg.InsufficientReason = "landed_percent 合成溢出(权重求和为非有限数)"
		pg.GoalSource = "insufficient"
		return pg
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
		if math.IsNaN(v) || math.IsInf(v, 0) {
			// belt-and-suspenders:上面已挡输入非有限数,理论上除法后不会再出;
			// 兜底让防线在未来 round1 换算改动下仍自证——比只加注释再回归稳。
			pg.Insufficient = true
			pg.InsufficientReason = "landed_percent 合成结果非有限数"
			pg.GoalSource = "insufficient"
			return pg
		}
		pg.LandedPercent = &v
	}
	if insufficientCount > 0 {
		// 合成值仅基于可用里程碑时须明示 partial（若全部都不足，LandedPercent 就是 nil）。
		pg.Partial = true
	}

	// 【R2·P1-3】goal_source 按**实际入账来源**打标,而非配置形态。
	// 反例(老逻辑按 ov.Milestones[].Evidence 是否配置打标):evidence 文件全失效降级
	// 时,实际入账全是 manual,却仍标 "mixed@as_of" 或 "evidence"——向用户虚报数据来源,
	// 违反 fail-honest 纪律。修法:遍历 pg.Milestones,按 Source 前缀 + !Insufficient
	// 分类;"配了 evidence 但一条都没入账"追加 +degraded 后缀披露降级。
	evidenceLanded := false // 至少一条 evidence source 真取到数值(!Insufficient)
	manualLanded := false   // 至少一条 manual 值成功入账
	for _, m := range pg.Milestones {
		if m.Insufficient || m.DonePercent == nil {
			continue
		}
		// 按 Source 前缀分类:evidence@... vs manual(@as_of)
		switch {
		case strings.HasPrefix(m.Source, "evidence@"):
			evidenceLanded = true
		case strings.HasPrefix(m.Source, "manual"):
			manualLanded = true
		}
	}
	switch {
	case evidenceLanded && manualLanded:
		pg.GoalSource = withAsOf("mixed", ov.AsOf)
	case evidenceLanded:
		pg.GoalSource = "evidence"
	case manualLanded:
		if hasEvidence {
			// 配了 evidence 但一条都没入账 → 披露"降级到 manual"。
			// 前端直接显示字符串,委托人一眼看出机械口径失效、当前读数来自人工估算。
			pg.GoalSource = withAsOf("manual+degraded", ov.AsOf)
		} else {
			pg.GoalSource = withAsOf("manual", ov.AsOf)
		}
	default:
		// 无任何有效入账(所有 milestone 都 insufficient)——理论上此时 validWeightSum=0
		// 且 LandedPercent=nil,标 insufficient 让前端不显示百分数,避免虚报 evidence/manual。
		pg.GoalSource = "insufficient"
	}
	return pg
}

func withAsOf(prefix, asOf string) string {
	if asOf == "" {
		return prefix
	}
	return prefix + "@" + asOf
}

// buildGoalMilestone 单个里程碑折算：evidence **存在即独占**——一旦配了 evidence 就
// 只按 evidence 取数，失败 / 超龄 / pointer 取不到数值一律 insufficient，
// **绝不回退到 m.DonePercent**（人工值）：读数含义漂移是造假的一种。
// 无 evidence 时才用人工 done_percent；两者都无 → 数据不足。
// **不做插值、不回退旧值**。
//
// 人工 done_percent 强制在 [0, 100]：round1(int64 截断) 对 -50 会算出 -49.9，
// 落地进度作为决策读数出现负值/超 100 都是造读数——语义上就是坏配置，标数据不足。
func buildGoalMilestone(m boardOverrideMilestone, root, asOf string, now time.Time) GoalMilestone {
	gm := GoalMilestone{
		ID:     m.ID,
		Title:  m.Title,
		Weight: m.Weight,
		Basis:  m.Basis,
	}
	if m.Evidence != nil {
		v, src, stale, reason := readEvidencePercent(m.Evidence, root, now)
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
		v := *m.DonePercent
		if v < 0 || v > 100 {
			// 越界的人工值一律拒绝。允许 -50 或 250 通过就会在合成值里出现负数或
			// 200%——前端把这数字当权威展示，比"不显示"糟糕得多。
			gm.Insufficient = true
			gm.InsufficientReason = fmt.Sprintf("done_percent 越界 (%v，须在 [0,100])", v)
			gm.Source = "insufficient"
			return gm
		}
		rounded := round1(v)
		gm.DonePercent = &rounded
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
// evidence.path 强制要求绝对路径（round-3 加固）：相对路径无论解析到进程 CWD 还是
// board.json 所在目录，都存在「同名文件静默读错」的兜底路径——
//   - CWD：launchd 从 / 起、shell 从项目目录起，同一配置读数「时有时无」；
//   - boardRoot：默认数据根里若存在同名脚手架/临时文件也会零告警读错。
// 两者都把「配错的路径」伪装成"数据不足"或"读到值"，无诊断可查。绝对路径是唯一
// 能让读数出处一目了然的选项——若配错就当场 insufficient 报出来。
// boardRoot 参数保留是为了 API 稳定性（其它调用点已透传），但内部不再回退到它。
//
// 反例注入①的关键：extractNumber 严格要求最终值是 JSON 数值（float64）。
// 若 fixture 里同名字段是字符串 "9/21"，钻取会在类型断言处失败，直接返回 not-ok；
// 绝不尝试把字符串 parse 成数字——那种"贴心"回退等于替用户凭空造读数。
func readEvidencePercent(ev *boardOverrideEvidence, boardRoot string, now time.Time) (float64, string, bool, string) {
	_ = boardRoot // 保留形参供 API 稳定；round-3 起相对路径直接拒绝，不再回退到 boardRoot。
	if ev.Path == "" {
		return 0, "insufficient", false, "evidence.path 为空"
	}
	// 绝对路径守卫：非绝对一律 insufficient。filepath.IsAbs 是跨平台权威判断
	// （Windows 上 D:/foo 是绝对而 foo/bar 不是；Unix 上以 / 开头才算绝对）。
	// 允许相对路径 = 允许「同名文件静默兜底」，与 fail-honest 冲突。
	if !filepath.IsAbs(ev.Path) {
		return 0, "insufficient", false, fmt.Sprintf("evidence.path 必须是绝对路径 (got %q)", ev.Path)
	}
	resolved := ev.Path
	src := "evidence@" + resolved
	if ev.MaxAgeHours < 0 {
		// 负 max_age_hours 是明显的配置错误。0 意为"不限"（已有语义），负数若被
		// 兜底成"不限"就把「配错的超龄门」变成永远不生效——同 P1-4 的隐匿降级同类，
		// 一律 insufficient。
		return 0, src, false, fmt.Sprintf("evidence.max_age_hours 为负值 (%v)", ev.MaxAgeHours)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return 0, src, false, "evidence 文件不存在或不可读: " + err.Error()
	}
	mtime := info.ModTime()
	src = "evidence@" + resolved + "@" + mtime.UTC().Format(time.RFC3339)
	if ev.MaxAgeHours > 0 {
		age := now.Sub(mtime)
		maxAge := time.Duration(ev.MaxAgeHours * float64(time.Hour))
		if age > maxAge {
			return 0, src, true, fmt.Sprintf("evidence 已超龄 (age=%s > max=%s)", age.Round(time.Second), maxAge.Round(time.Second))
		}
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return 0, src, false, "读 evidence 文件失败: " + err.Error()
	}
	var jroot any
	if err := json.Unmarshal(data, &jroot); err != nil {
		return 0, src, false, "evidence JSON 解析失败: " + err.Error()
	}
	num, ok := extractNumber(jroot, ev.Numerator)
	if !ok {
		return 0, src, false, fmt.Sprintf("evidence numerator %q 取不到数值", ev.Numerator)
	}
	if num < 0 {
		// 分子为负 = 坏配置（fixture 里 pass=-9 或计数字段被误写成偏移量）。
		// 单挡这一层是必需的：若同时 den<0 则 pct=num/den 会算出正数，绕过下方 pct<0 检查；
		// 举例 {pass:-9, blocked:-2} 折算 -9/-11*100=81.8% 会被伪装成合理读数。
		return 0, src, false, fmt.Sprintf("evidence numerator 为负值 (%v)", num)
	}
	if len(ev.Denominator) == 0 {
		return 0, src, false, "evidence denominator 未配置"
	}
	den := 0.0
	for _, p := range ev.Denominator {
		v, ok := extractNumber(jroot, p)
		if !ok {
			return 0, src, false, fmt.Sprintf("evidence denominator %q 取不到数值", p)
		}
		if v < 0 {
			// 【R2·P1-1】分量级符号相消守护:sum-level 的 den<=0 挡不住「一正一负相加得正」——
			// 反例:fixture {pass:5, blocked:10, adjustment:-3} 配 denominator:[pass,blocked,adjustment]
			// → den=5+10-3=12>0、num=5≥0、pct=41.7%∈[0,100]——三闸全过零告警渗出错读数(真值 33.3%)。
			// 威胁模型与 num<0 单挡对称(计数字段被误配成 offset、pointer 指错到 delta 字段等),
			// 与 boardgoal.go:340 注释及 README「防线放在 num 与 den 各自绝对值上」的按类闭合声称一致——
			// den 是分量求和,「各自绝对值」必须逐分量守护,不是仅守和。
			return 0, src, false, fmt.Sprintf("evidence denominator %q 为负值 (%v)", p, v)
		}
		den += v
	}
	if den <= 0 {
		// 除零 + 全零分母守护。落地进度是决策读数，NaN/Inf 一旦渗出会污染合成值。
		// 单分量负值已在循环内单挡(上方 v<0)——本闸门专治「所有分量非负但求和为 0」(如 {a:0,b:0}),
		// 与 num=0/den=0 = NaN 一起兜底。历史上 den<0 曾靠此关口挡(-0.0 == 0.0 陷阱),
		// 现改为分量级 + 求和级双层防线,「符号相消」类攻击面完备闭合。
		return 0, src, false, fmt.Sprintf("evidence denominator 求和 ≤ 0 (%v)", den)
	}
	pct := num / den * 100
	if pct < 0 {
		// belt-and-suspenders：正常路径 num>=0 & den>0 时 pct 不会<0，但保留兜底
		// 让防线在任何未来重构下都自证——比只加注释再回归稳。
		return 0, src, false, fmt.Sprintf("evidence 折算为负值 (num=%v den=%v)", num, den)
	}
	// 上界越界同样是坏数据：pointer 里配错导致 num=30, den=[10] 会算出 300%。
	// 前端把这数字当权威渲染，与"数据不足"相比是更糟糕的污染。
	// 用小容差吸收浮点尾巴（0.1+0.2 会算成 0.30000000000000004 之类的经典漂移），
	// 只有真实"分子>分母"的配置错误才会明显越界。
	const upperTol = 1e-6
	if pct > 100+upperTol {
		return 0, src, false, fmt.Sprintf("evidence 折算越界 (%.4f%%，num=%v den=%v)", pct, num, den)
	}
	if pct > 100 {
		pct = 100
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
