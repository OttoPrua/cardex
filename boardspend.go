package main

// boardspend.go — 队列任务消耗视图：按时间窗口看「哪些模型、哪些卡烧掉了多少」。
//
// 【为什么要有这一块，而不是把 transcript 曲线的窗口拉长】
// 燃尽页原有的 token 曲线扫的是 ~/.claude/projects 的 transcript，有两个硬约束：
//   1. **窗口拉不长**。实测 30 天的 transcript 有 1.0 GB（7 天 409 MB），
//      而扫描有 512 MB 的字节预算闸；拉到 30 天必然撞闸静默截断，
//      "看起来是全月、其实只读了一半"比不给这个窗口更糟。
//   2. **口径不是队列**。transcript 目录里混着人在 Claude Code 里手敲的交互会话，
//      它们与 claudego 派发的卡在同一批文件里，分不开。
// 于是"最近 24 小时只看得到一个模型"这件事既不是 bug 也不是数据丢了——
// 那 24 小时里恰好只有 opus 在跑（而且多半来自交互会话，不是队列）。
//
// 本文件换一个源：**任务卡自己的账**。每张卡跑完时 runner 会把 claude CLI 回报的
// total_cost_usd / num_turns 写回卡上（见 runner.go），这份账：
//   - 随卡长期留存（含 archive/），要看多久就能看多久，零额外扫描；
//   - 天然是队列口径——一张卡就是一次派发，交互会话根本不在里面；
//   - 快照已经把全部卡读进内存了，这里只是再过一遍，不碰磁盘。
//
// 【两条必须说出口的边界】
//   1. `cost_usd` 是 claude CLI 回报的 **API 等价成本**。订阅制下它不是实际扣款，
//      只是"这些活按 API 价该值多少钱"。当成账单看会得出一个吓人且错误的数字。
//   2. **codex / 远端 codex 卡不回报花费**，它们的 cost 恒为 0。实测 1423 张卡里
//      448 张没有花费数据。不把这个缺口的张数说出来，用户会把"codex 那半边没花钱"
//      当成事实——而那半边烧的是另一套额度。

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// spendTopN 是「逐卡消耗」表回吐的条数上限。
// 30 天窗口里有近千张卡，全吐既没人看得完，也会把响应撑大；
// 排在前面的才是"钱花在哪"的答案。差额由 Tasks/Priced 两个计数如实交代。
const spendTopN = 30

// ModelSpend 是一个模型在窗口内的合计。
type ModelSpend struct {
	Model string `json:"model"`
	Tier  string `json:"tier"`
	// Tasks 只数**有花费数据**的卡：把 cost=0 的 codex 卡也算进来，
	// 会让"平均每卡花费"凭空腰斩。
	Tasks     int     `json:"tasks"`
	CostUSD   float64 `json:"cost_usd"`
	TurnsUsed int     `json:"turns_used"`
}

// TaskSpendEntry 是逐卡消耗表的一行。
type TaskSpendEntry struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	Project   string  `json:"project"`
	Model     string  `json:"model"`
	ModelTier string  `json:"model_tier"`
	Status    string  `json:"status"`
	Kind      string  `json:"kind"`
	CostUSD   float64 `json:"cost_usd"`
	TurnsUsed int     `json:"turns_used"`
	UpdatedAt string  `json:"updated_at"`
}

// TaskSpend 是一个时间窗口内的队列消耗视图。
type TaskSpend struct {
	Range      string `json:"range"`
	RangeLabel string `json:"range_label"`
	// Since 是窗口起点；range=all 时为空串（表示"有史以来"，不是"从 0 时刻起"）。
	Since string `json:"since"`
	// Tasks 是窗口内的卡数；Priced/Unpriced 把"有没有花费数据"摊开——
	// 只给一个合计金额会让人以为那就是全部活的成本。
	Tasks     int              `json:"tasks"`
	Priced    int              `json:"priced"`
	Unpriced  int              `json:"unpriced"`
	CostUSD   float64          `json:"cost_usd"`
	TurnsUsed int              `json:"turns_used"`
	ByModel   []ModelSpend     `json:"by_model"`
	Top       []TaskSpendEntry `json:"top"`
	// TopTruncated 表示逐卡表被 spendTopN 截断了（前端必须说出来）。
	TopTruncated bool `json:"top_truncated"`
	// Basis 是口径披露，前端原样呈现。
	Basis string `json:"basis"`
}

// spendRange 是一个可选窗口。Hours<=0 表示"全部历史"。
type spendRange struct {
	Key   string
	Label string
	Hours int
}

// spendRanges 是前端标签页的取值。次序即标签次序：从最近到最远，最后是全量。
var spendRanges = []spendRange{
	{Key: "24h", Label: "最近 24 小时", Hours: 24},
	{Key: "7d", Label: "最近 7 天", Hours: 24 * 7},
	{Key: "30d", Label: "最近 30 天", Hours: 24 * 30},
	{Key: "all", Label: "全部历史", Hours: 0},
}

// resolveSpendRange 把查询串映射成窗口。未知取值一律回落 24h（第一个），
// 不报错也不猜——这是个展示窗口，写错一个参数不该让整页 500。
func resolveSpendRange(key string) spendRange {
	for _, r := range spendRanges {
		if r.Key == key {
			return r
		}
	}
	return spendRanges[0]
}

// buildTaskSpend 从快照里的任务卡汇总窗口内的消耗。
//
// 时间维度用 **UpdatedAt** 而不是 CreatedAt：花费是在卡跑完那一刻产生并写回的，
// 按创建时间归档会把一张上周入队、今天才跑完的卡算进上周——那笔钱是今天花的。
func buildTaskSpend(cfg *Config, snap *boardSnapshot, key string, now time.Time) TaskSpend {
	r := resolveSpendRange(key)
	out := TaskSpend{
		Range: r.Key, RangeLabel: r.Label,
		ByModel: []ModelSpend{}, Top: []TaskSpendEntry{},
	}
	var cut time.Time
	if r.Hours > 0 {
		cut = now.Add(-time.Duration(r.Hours) * time.Hour)
		out.Since = cut.Format(time.RFC3339)
	}

	// task id → 项目名。快照只有 项目→卡 的方向，逐卡表要反着查。
	projOf := make(map[string]string, len(snap.byID))
	for _, p := range snap.Projects {
		for _, t := range snap.projTasks[p.ID] {
			projOf[t.ID] = p.Name
		}
	}

	byModel := map[string]*ModelSpend{}
	var rows []TaskSpendEntry
	for _, t := range snap.byID {
		if r.Hours > 0 {
			ts, ok := parseRFC3339(t.UpdatedAt)
			// 时间戳损坏/缺失的卡：窗口视图里只能丢掉（归不了档），
			// 但 all 窗口一定收得进来，不至于整张卡从所有视图消失。
			if !ok || ts.Before(cut) {
				continue
			}
		}
		out.Tasks++
		if t.CostUSD <= 0 {
			out.Unpriced++
			continue
		}
		out.Priced++
		out.CostUSD += t.CostUSD
		out.TurnsUsed += t.TurnsUsed

		model, _ := effectiveModel(cfg, t)
		if model == "" {
			model = "(账号默认)"
		}
		m := byModel[model]
		if m == nil {
			m = &ModelSpend{Model: model, Tier: modelTier(model)}
			byModel[model] = m
		}
		m.Tasks++
		m.CostUSD += t.CostUSD
		m.TurnsUsed += t.TurnsUsed

		rows = append(rows, TaskSpendEntry{
			ID: t.ID, Title: t.Title, Project: projOf[t.ID],
			Model: model, ModelTier: modelTier(model), Status: t.Status,
			Kind:      snap.kindOf[t.ID].Kind,
			CostUSD:   round2(t.CostUSD),
			TurnsUsed: t.TurnsUsed,
			UpdatedAt: t.UpdatedAt,
		})
	}
	out.CostUSD = round2(out.CostUSD)

	for _, m := range byModel {
		m.CostUSD = round2(m.CostUSD)
		out.ByModel = append(out.ByModel, *m)
	}
	// 花得多的排前面；同额时按模型名，保证同一份数据每次的次序一致
	// （map 遍历顺序随机，不定序的话每 30 秒自动刷新表格行都会跳一次）。
	sort.Slice(out.ByModel, func(i, j int) bool {
		if out.ByModel[i].CostUSD != out.ByModel[j].CostUSD {
			return out.ByModel[i].CostUSD > out.ByModel[j].CostUSD
		}
		return out.ByModel[i].Model < out.ByModel[j].Model
	})
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CostUSD != rows[j].CostUSD {
			return rows[i].CostUSD > rows[j].CostUSD
		}
		return rows[i].ID < rows[j].ID
	})
	if len(rows) > spendTopN {
		rows, out.TopTruncated = rows[:spendTopN], true
	}
	out.Top = rows
	out.Basis = spendBasis(&out)
	return out
}

// spendBasis 生成口径披露串。三件事必须说：钱是等价成本不是扣款、
// 有多少张卡没有花费数据、这份账与 transcript 曲线不是一回事。
func spendBasis(s *TaskSpend) string {
	var b strings.Builder
	b.WriteString("花费取自任务卡上 runner 回写的 cost_usd（claude CLI 的 total_cost_usd）——")
	b.WriteString("订阅制下这是 **API 等价成本**，不是实际扣款金额。")
	if s.Unpriced > 0 {
		b.WriteString(" 窗口内 ")
		b.WriteString(strconv.Itoa(s.Tasks))
		b.WriteString(" 张卡中有 ")
		b.WriteString(strconv.Itoa(s.Unpriced))
		b.WriteString(" 张没有花费数据（codex / 远端 codex 不回报花费，未跑或已取消的卡同样为空），")
		b.WriteString("它们烧的是另一套额度，**未计入**上面的合计。")
	}
	b.WriteString(" 时间按卡的 updated_at（跑完那一刻）归档；")
	b.WriteString("本表是**队列口径**，不含在 Claude Code 里手敲的交互会话——")
	b.WriteString("那部分只在上面的 transcript 曲线里出现。")
	return b.String()
}

// round2 保留两位小数——花费是钱，round1 会把 $0.04 和 $0.05 抹成同一个数。
// 只用于非负金额（cost_usd 恒 ≥0），故不处理负数的进位方向。
func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }
