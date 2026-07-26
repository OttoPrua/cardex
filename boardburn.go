package main

// boardburn.go — 额度燃尽视图。
//
// 三个数据源，都在本机，都只读：
//  1. CodexBar 的 claude.json —— claude 侧各账号 session/weekly/opus 窗口的百分比时间序列；
//  2. CodexBar 的 usage-history.jsonl —— codex 侧 primary(5h)/secondary(周) 的百分比时间序列
//     （config.usage_feed 就指向它，budget.go 的红线判定也读这个文件）；
//  3. ~/.claude/projects/*/*.jsonl transcript —— 每条 assistant 消息的绝对 token 用量。
//
// 诚实性要点（这一屏最容易骗人，所以规矩最严）：
//   - 燃烧速率只在**当前窗口周期内**拟合。跨过一次重置去算斜率会得到负数或垃圾值，
//     所以按 resetsAt 分组，只取与最新样本同属一个重置边界的点（带容差，见 burnResetTolerance）。
//   - 只有一个样本点时**没有**速率可言 → burn_rate / exhaust_at 一律 null，verdict = "数据不足"。
//     实测多数账号窗口就只有 1 个样本，这个分支是常态而非边角。
//   - resetsAt 已经过去 = 这条样本描述的窗口早就翻篇了，其 used_percent 不再代表现状 → 标 stale。
//   - **判定不可用要发生在估算之前**。先算速率再判「数据不足」、却不回收已写入的指针，
//     等于一边说数据不足一边给出 30.3%/小时和一个 12 天前的耗尽时刻。见 buildBurnSource 的顺序。
//   - 可空字段（resets_at / minutes_to_reset / burn_rate_pct_per_hour / exhaust_at）**都**可能是 null。
//     样本缺 resetsAt 是常态（限额重置后写入的 0% 样本就没有），此时不许编一个重置时刻，
//     消费方必须按可空处理——注意这四个字段的 null 与 verdict / stale 无关，
//     一条完全新鲜、有速率、verdict="充裕" 的源同样可能没有 resets_at。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---- 对外 JSON 结构（字段名严格对齐契约）----

type BurnPoint struct {
	T           string  `json:"t"`
	UsedPercent float64 `json:"used_percent"`
}

// BurnSource 是一个「账号 × 窗口」的额度视图。
//
// 五个指针字段（ResetsAt / MinutesToReset / BurnRatePctPerH / ExhaustAt）都可能是 null，
// 这是刻意的，见文件头诚实性要点。两类原因要分清：
//   - BurnRatePctPerH / ExhaustAt 为 null = 这批样本算不出可信的估算；
//   - ResetsAt / MinutesToReset 为 null = 源数据里就没有重置时刻（不是估算失败）。
//     后者与 verdict、stale **都不相关**，别拿 verdict 当护栏。
type BurnSource struct {
	ID            string  `json:"id"`
	Provider      string  `json:"provider"`
	AccountKey    string  `json:"account_key"`
	AccountLabel  string  `json:"account_label"`
	Window        string  `json:"window"`
	WindowLabel   string  `json:"window_label"`
	WindowMinutes int     `json:"window_minutes"`
	UsedPercent   float64 `json:"used_percent"`
	// RemainingPercent = 100 − UsedPercent，钳在 [0,100]。
	//
	// 为什么后端算而不是让前端减：额度这一屏的**主读数是"还剩多少"**——
	// "还能干多久"才是用户要做的决定，"已经烧了多少"是过程量。让每个消费方各自做减法，
	// 迟早有一处忘了钳位（源数据出现 >100 时会算出负剩余）或忘了处理缺样本，
	// 于是同一份数据在两个地方显示成两个数。口径只有一处、只算一次。
	// used_percent **保留不动**：它是 CodexBar 的原始读数，改口径会连坐所有历史消费方。
	RemainingPercent float64     `json:"remaining_percent"`
	CapturedAt       string      `json:"captured_at"`
	ResetsAt         *string     `json:"resets_at"`
	MinutesToReset   *float64    `json:"minutes_to_reset"`
	Stale            bool        `json:"stale"`
	Series           []BurnPoint `json:"series"`
	BurnRatePctPerH  *float64    `json:"burn_rate_pct_per_hour"`
	ExhaustAt        *string     `json:"exhaust_at"`
	ExhaustBefore    bool        `json:"exhaust_before_reset"`
	Verdict          string      `json:"verdict"`
}

type TokenSeriesPoint struct {
	T       string             `json:"t"`
	ByModel map[string]float64 `json:"by_model"`
}

// TokenSeries 是 transcript 里的绝对 token 曲线。
//
// points[].by_model 是**四类 token 等权相加**的原始吞吐量。同一份响应里的
// queue_spend.weighted_tokens 走的却是 budget.go 的额度口径（cache_read 按 1 折再乘模型权重）。
// 两个都叫「token」的数字并排放着、口径差一个数量级（实测 cache_read 占 94.5%，
// 等权口径比真正新处理的 token 大 18 倍、比额度口径大 6.7 倍），
// 不把拆分和折算值一并发出去，消费方根本无从在界面上说清这件事。
// 于是 ByComponent / WeightedTotal / Basis 三个字段是**口径披露**，不是可选装饰。
type TokenSeries struct {
	// Range / Since 交代这条曲线covers 的窗口——它跟着消耗视图的时间标签页走。
	Range         string             `json:"range"`
	Since         string             `json:"since"`
	BucketMinutes int                `json:"bucket_minutes"`
	Models        []string           `json:"models"`
	Points        []TokenSeriesPoint `json:"points"`
	// Truncated / FilesMatched / FilesScanned / BytesScanned 是**扫描完整性**披露。
	// 长窗口下 transcript 体量会撞上字节预算闸（实测 30 天 1.06 GB）；
	// 一条少了后半段的曲线与"那段时间没跑活"在图上长得一模一样，
	// 静默截断就是造读数，所以截没截、扫了多少必须一起发出去。
	Truncated    bool  `json:"truncated"`
	FilesMatched int   `json:"files_matched"`
	FilesScanned int   `json:"files_scanned"`
	BytesScanned int64 `json:"bytes_scanned"`
	// ByComponent 是窗口内四类 token 的合计：input / output / cache_creation / cache_read。
	ByComponent map[string]float64 `json:"by_component"`
	// WeightedTotal 是同一批样本按 budget.go 权重折算后的额度口径合计，
	// 与 queue_spend.weighted_tokens 可比（但样本范围不同：这里是全部 transcript，那里只有队列账本）。
	WeightedTotal float64 `json:"weighted_total"`
	// Basis 用人话交代 by_model 的口径，供前端原样呈现。
	Basis string `json:"basis"`
}

type QueueSpend struct {
	WindowHours    int                `json:"window_hours"`
	WeightedTokens float64            `json:"weighted_tokens"`
	ByModel        map[string]float64 `json:"by_model"`
}

type QuotaSummary struct {
	ClaudeSession  *BurnSource `json:"claude_session"`
	ClaudeWeekly   *BurnSource `json:"claude_weekly"`
	CodexPrimary   *BurnSource `json:"codex_primary"`
	CodexSecondary *BurnSource `json:"codex_secondary"`
}

type BurnResp struct {
	GeneratedAt string       `json:"generated_at"`
	Sources     []BurnSource `json:"sources"`
	TokenSeries TokenSeries  `json:"token_series"`
	QueueSpend  QueueSpend   `json:"queue_spend"`
	// TaskSpend 是按时间窗口的队列消耗（见 boardspend.go）。它**不进 burnCache**：
	// 窗口由请求参数决定，而缓存里那份 transcript 扫描与窗口无关，
	// 混在一起会让每个窗口各占一份昂贵的缓存。由 handleBurn 逐请求填。
	TaskSpend TaskSpend `json:"task_spend"`
}

// ---- 源文件定位 ----

// claudeHistoryPath 是 CodexBar 存 claude 侧百分比历史的位置。
// 没有配置项指向它（config.usage_feed 只管 codex 那个 jsonl），故按约定路径找；
// 找不到就当这一侧没数据，不报错——看板不该因为没装 CodexBar 就整页挂掉。
func claudeHistoryPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support",
		"com.steipete.codexbar", "history", "claude.json")
}

func codexFeedPath(cfg *Config) string {
	if cfg != nil && cfg.UsageFeed != "" {
		return cfg.UsageFeed
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "CodexBar", "usage-history.jsonl")
}

func transcriptRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// ---- claude.json ----

type claudeHistEntry struct {
	CapturedAt  string  `json:"capturedAt"`
	ResetsAt    string  `json:"resetsAt"`
	UsedPercent float64 `json:"usedPercent"`
}

type claudeHistWindow struct {
	Name          string            `json:"name"`
	WindowMinutes int               `json:"windowMinutes"`
	Entries       []claudeHistEntry `json:"entries"`
}

type claudeHistFile struct {
	Accounts            map[string][]claudeHistWindow `json:"accounts"`
	PreferredAccountKey string                        `json:"preferredAccountKey"`
}

// ---- 通用：把一串样本变成 BurnSource ----

const (
	maxSeriesPoints = 240 // 单个窗口最多回吐的样本点，防前端被巨长序列拖垮
	burnMinSpanMin  = 5.0 // 拟合斜率所需的最小时间跨度（分钟）
	burnLookbackDay = 14  // 只展示最近这些天有过样本的窗口

	// burnResetTolerance 是「两个 resetsAt 指向同一个重置边界」的容差。
	// CodexBar 写同一个边界时会在 20:59:59Z 与 21:00:00Z 之间抖动 1 秒，
	// 按字符串精确比对会把同一周期的样本切碎（实测 38 条 claude 源里 15 条被切，
	// 其中一条本来有 3 个点、6%→25%，被切到只剩 1 点后谎报「数据不足」）。
	// 取 90 秒：远大于观测到的抖动，又远小于最短的真实周期（5 小时）。
	burnResetTolerance = 90 * time.Second

	// burnMinRatePctPerH 是「算得上在燃烧」的速率下限。
	// 平坦序列的最小二乘分子是两个近似相等的大数相减，本该精确抵消为 0，
	// 但 Go 在 arm64 上把 n*sxy-sx*sy 融合成 FMSUB（n*sxy 从不单独舍入），
	// 留下约 1e-15 的正残差。只用 `rate > 0` 当闸门，它就会放行并推出公元 2318 年。
	burnMinRatePctPerH = 0.05

	// burnMaxHorizonH 是外推的地平线上限。对 5 小时/一周的窗口来说，
	// 超过 30 天的「预计耗尽时刻」不含任何信息，只会显得精确。
	burnMaxHorizonH = 30 * 24

	// burnResetDropPct 是「用量回落多少算跨过一次重置」的阈值。
	// 同一周期内用量单调不减，明显回落只可能是重置。
	burnResetDropPct = 0.5
)

type rawSample struct {
	at       time.Time
	pct      float64
	resetsAt string
}

// remainingPercent 把「已用%」翻成「剩余%」并钳到 [0,100]。
//
// 钳位不是防御性冗余：CodexBar 的样本实测出现过 100.0 以上（窗口刚重置前的最后一笔），
// 不钳会算出负剩余，前端画进度条时会得到负宽度（渲染成 0，看着像"刚好耗尽"，
// 其实是"已超"）；同样，负的 used 会算出 >100 的剩余，读成"比满额还多"。
// 两个方向都必须夹住——这是读数，不是装饰。
func remainingPercent(used float64) float64 {
	r := 100 - used
	if r < 0 {
		return 0
	}
	if r > 100 {
		return 100
	}
	return round1(r)
}

// buildBurnSource 把某个窗口的原始样本序列压成 BurnSource。
// 关键约束：只用**当前窗口周期**（与最新样本同一个 resetsAt）内的点算速率。
func buildBurnSource(id, provider, acctKey, acctLabel, window, windowLabel string,
	windowMinutes int, samples []rawSample, now time.Time, maxAge time.Duration) *BurnSource {

	if len(samples) == 0 {
		return nil
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].at.Before(samples[j].at) })
	latest := samples[len(samples)-1]

	src := &BurnSource{
		ID: id, Provider: provider, AccountKey: acctKey, AccountLabel: acctLabel,
		Window: window, WindowLabel: windowLabel, WindowMinutes: windowMinutes,
		UsedPercent:      latest.pct,
		RemainingPercent: remainingPercent(latest.pct),
		CapturedAt:       latest.at.UTC().Format(time.RFC3339),
	}

	period := currentPeriod(samples, windowMinutes)
	if len(period) > maxSeriesPoints {
		period = period[len(period)-maxSeriesPoints:]
	}
	for _, s := range period {
		src.Series = append(src.Series, BurnPoint{
			T: s.at.UTC().Format(time.RFC3339), UsedPercent: s.pct,
		})
	}

	// 重置时间
	resetPassed := false
	if latest.resetsAt != "" {
		if rt, ok := parseRFC3339(latest.resetsAt); ok {
			rs := rt.UTC().Format(time.RFC3339)
			src.ResetsAt = &rs
			mins := round1(rt.Sub(now).Minutes())
			if mins < 0 {
				// 重置时刻已过：这条样本描述的窗口早翻篇了
				resetPassed = true
				zero := 0.0
				src.MinutesToReset = &zero
			} else {
				src.MinutesToReset = &mins
			}
		}
	}
	src.Stale = resetPassed || now.Sub(latest.at) > maxAge

	// 样本比它所描述的窗口还老 = 这个读数根本不可能代表当前窗口的现状。
	// 5 小时窗口配一条 14 小时前的样本，说「充裕」是纯粹的编造。
	//
	// 这个判定必须**在估算之前**做完：先算速率、再判数据不足、却不回收已经写进去的
	// 指针，就会得到「verdict=数据不足 + rate=30.3 + exhaust_at=12 天前」这种自相矛盾的响应。
	// 不可用就根本不算，null 是唯一诚实的输出。
	staleBeyondWindow := windowMinutes > 0 &&
		now.Sub(latest.at) > time.Duration(windowMinutes)*time.Minute
	unusable := resetPassed || staleBeyondWindow

	// 燃烧速率：当前周期内最小二乘拟合（%/小时）。
	// 少于 2 个点、或跨度太短 → 没有可言的速率，保持 null。
	if !unusable && len(period) >= 2 {
		spanMin := period[len(period)-1].at.Sub(period[0].at).Minutes()
		if spanMin >= burnMinSpanMin {
			if rate, ok := leastSquaresSlopePerHour(period); ok {
				if rate < 0 {
					// 同一周期内用量不可能变少。负斜率是「样本仍跨了周期」的证据
					// （currentPeriod 的边界检测没兜住的残余情形），不是一个能报出去的速率。
					unusable = true
				} else {
					r := round1(rate)
					src.BurnRatePctPerH = &r
					src.ExhaustAt, src.ExhaustBefore = projectExhaust(latest, rate, src.ResetsAt, now)
				}
			}
		}
	}

	src.Verdict = burnVerdict(src, unusable)
	return src
}

// currentPeriod 圈定「与最新样本同属一个窗口周期」的样本。
//
// 两条路径：
//  1. 最新样本带 resetsAt：按重置边界分组，用 burnResetTolerance 容差比对**解析后的时刻**。
//     不能拿原始字符串当键——CodexBar 的边界会抖动 1 秒，精确串比会把一个周期切成两半。
//  2. 最新样本没有 resetsAt（opus 窗口、或限额重置后写入的 0% 样本）：退化成
//     「最近一个窗口长度内」。但这个时间窗和真实周期没有任何对齐关系，
//     直接用会把重置前后的样本拌进同一次拟合，得到负速率——正是文件头写明要防的情况。
//     所以往回走时自己找重置边界，遇到就地截断。
func currentPeriod(samples []rawSample, windowMinutes int) []rawSample {
	latest := samples[len(samples)-1]

	if rt, ok := parseRFC3339(latest.resetsAt); ok {
		var period []rawSample
		for _, s := range samples {
			if st, ok2 := parseRFC3339(s.resetsAt); ok2 && sameResetBoundary(st, rt) {
				period = append(period, s)
			}
		}
		return period
	}

	cut := latest.at.Add(-time.Duration(windowMinutes) * time.Minute)
	start := len(samples) - 1
	var refReset time.Time
	haveRef := false
	for i := len(samples) - 1; i >= 0; i-- {
		s := samples[i]
		if s.at.Before(cut) {
			break
		}
		// 证据一：出现了与参考边界不同的 resetsAt —— 已经跨到上一个周期了。
		if st, ok := parseRFC3339(s.resetsAt); ok {
			if haveRef && !sameResetBoundary(st, refReset) {
				break
			}
			refReset, haveRef = st, true
		}
		// 证据二：往回看用量反而更高 —— 周期内用量单调不减，回落只可能是重置。
		if s.pct > samples[start].pct+burnResetDropPct {
			break
		}
		start = i
	}
	return samples[start:]
}

func sameResetBoundary(a, b time.Time) bool {
	d := a.Sub(b)
	if d < 0 {
		d = -d
	}
	return d <= burnResetTolerance
}

// projectExhaust 按当前速率外推「打到 100% 的时刻」。三道闸，缺一不可：
//
//  1. 速率下限 burnMinRatePctPerH：平坦序列的浮点残差也是正数，
//     只判 rate>0 会外推出公元 2318 年，而展示用的 round1(rate) 同时显示成 0——
//     同一个对象里「速率为 0」和「292 年后耗尽」并存。
//  2. 地平线上限 burnMaxHorizonH：超出就不给，别用一个精确到秒的时间戳伪装确定性。
//  3. 不给过去的时刻：结果早于 now 说明按这个速率早该烧完了，
//     正确表达是速率已失效，而不是一个已经过去的耗尽时间。
//
// 锚点用 latest.at 而非 now，两者代数等价：从 now 出发要先把用量推进到 now
// （pct + rate·Δ），再除以 rate，结果正是 latest.at + (100-pct)/rate。
func projectExhaust(latest rawSample, rate float64, resetsAt *string, now time.Time) (*string, bool) {
	if rate < burnMinRatePctPerH || latest.pct >= 100 {
		return nil, false
	}
	hours := (100 - latest.pct) / rate
	if hours > burnMaxHorizonH {
		return nil, false
	}
	ex := latest.at.Add(time.Duration(hours * float64(time.Hour)))
	if ex.Before(now) {
		return nil, false
	}
	before := false
	if resetsAt != nil {
		if rt, ok := parseRFC3339(*resetsAt); ok {
			before = ex.Before(rt)
		}
	}
	exs := ex.UTC().Format(time.RFC3339)
	return &exs, before
}

// leastSquaresSlopePerHour 对 (时间, 百分比) 做最小二乘，返回 %/小时。
func leastSquaresSlopePerHour(pts []rawSample) (float64, bool) {
	n := float64(len(pts))
	t0 := pts[0].at
	var sx, sy, sxy, sxx float64
	for _, p := range pts {
		x := p.at.Sub(t0).Hours()
		y := p.pct
		sx += x
		sy += y
		sxy += x * y
		sxx += x * x
	}
	den := n*sxx - sx*sx
	if math.Abs(den) < 1e-9 {
		return 0, false
	}
	return (n*sxy - sx*sy) / den, true
}

// burnVerdict 给一句人话结论。数据不足时**必须**说数据不足，不许猜。
// unusable 表示这条样本已无法描述当前窗口（重置已过 / 样本比窗口还老）。
func burnVerdict(s *BurnSource, unusable bool) string {
	if unusable {
		// 注意顺序：不可用的样本连「已耗尽」都不能断言——
		// 100% 是上一个窗口的事，那个窗口早就重置了。
		return "数据不足"
	}
	if s.UsedPercent >= 100 {
		return "已耗尽"
	}
	if s.BurnRatePctPerH == nil {
		return "数据不足"
	}
	if s.ExhaustBefore {
		return "将在重置前烧完"
	}
	// 按当前速率推到重置时刻的预计用量，超过 80% 算偏紧。
	if s.MinutesToReset != nil {
		projected := s.UsedPercent + (*s.BurnRatePctPerH)*(*s.MinutesToReset/60)
		if projected >= 80 {
			return "偏紧"
		}
		return "充裕"
	}
	if s.UsedPercent >= 80 {
		return "偏紧"
	}
	return "充裕"
}

func shortAcct(key string) string {
	k := key
	if i := strings.LastIndex(k, ":"); i >= 0 {
		k = k[i+1:]
	}
	if len(k) > 8 {
		k = k[:8]
	}
	return k
}

func claudeWindowLabel(name string) string {
	switch name {
	case "session":
		return "5 小时窗口"
	case "weekly":
		return "周窗口"
	case "opus":
		return "Opus 周窗口"
	}
	return name
}

func codexWindowLabel(kind string) string {
	switch kind {
	case "primary":
		return "5 小时窗口"
	case "secondary":
		return "周窗口"
	}
	return kind
}

// accountLabeler 按「最近有样本」的顺序给账号编 A/B/C，首选账号恒为 A 并标注（主）。
//
// 序号按 provider 各自独立编，所以**必须**带 provider 前缀：两侧都从 A 起编，
// codex 的「账号 A」与 claude 的「账号 A（主）」是两个毫不相干的账号，
// 而排序键（新鲜度优先）又几乎保证它们并排出现——用户会读成同一个账号的两个数字对不上。
type accountLabeler struct {
	prefix string
	label  map[string]string
}

func newAccountLabeler(prefix string, latest map[string]time.Time, preferred string) *accountLabeler {
	keys := make([]string, 0, len(latest))
	for k := range latest {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i] == preferred {
			return true
		}
		if keys[j] == preferred {
			return false
		}
		if !latest[keys[i]].Equal(latest[keys[j]]) {
			return latest[keys[i]].After(latest[keys[j]])
		}
		return keys[i] < keys[j]
	})
	l := &accountLabeler{prefix: prefix, label: map[string]string{}}
	for i, k := range keys {
		name := prefix + "账号 " + string(rune('A'+i%26))
		if k == preferred {
			name += "（主）"
		}
		l.label[k] = name
	}
	return l
}

func (l *accountLabeler) get(k string) string {
	if v, ok := l.label[k]; ok {
		return v
	}
	return l.prefix + "账号 " + shortAcct(k)
}

// ---- claude 侧 sources ----

func claudeBurnSources(now time.Time, maxAge time.Duration) []BurnSource {
	path := claudeHistoryPath()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var f claudeHistFile
	if json.Unmarshal(data, &f) != nil {
		return nil
	}

	latestPerAcct := map[string]time.Time{}
	for acct, wins := range f.Accounts {
		for _, w := range wins {
			for _, e := range w.Entries {
				if at, ok := parseRFC3339(e.CapturedAt); ok {
					if cur, seen := latestPerAcct[acct]; !seen || at.After(cur) {
						latestPerAcct[acct] = at
					}
				}
			}
		}
	}
	labeler := newAccountLabeler("Claude ", latestPerAcct, f.PreferredAccountKey)

	cut := now.AddDate(0, 0, -burnLookbackDay)
	var out []BurnSource
	for acct, wins := range f.Accounts {
		// 陈年账号（14 天内没样本）不进列表——除非它是首选账号，那张卡用户总要看到。
		if lt, ok := latestPerAcct[acct]; !ok || (lt.Before(cut) && acct != f.PreferredAccountKey) {
			continue
		}
		for _, w := range wins {
			var samples []rawSample
			for _, e := range w.Entries {
				at, ok := parseRFC3339(e.CapturedAt)
				if !ok {
					continue
				}
				samples = append(samples, rawSample{at: at, pct: e.UsedPercent, resetsAt: e.ResetsAt})
			}
			src := buildBurnSource(
				"claude:"+w.Name+":"+shortAcct(acct), "claude", acct, labeler.get(acct),
				w.Name, claudeWindowLabel(w.Name), w.WindowMinutes, samples, now, maxAge)
			if src != nil {
				out = append(out, *src)
			}
		}
	}
	return out
}

// ---- codex 侧 sources ----

// codexFeedLine 复用 budget.go 里 feedSample 的同一份文件格式，
// 但看板要按账号+窗口分组做时间序列，故单独解析（多取 accountKey 字段）。
type codexFeedLine struct {
	AccountKey    string  `json:"accountKey"`
	Provider      string  `json:"provider"`
	SampledAt     string  `json:"sampledAt"`
	ResetsAt      string  `json:"resetsAt"`
	UsedPercent   float64 `json:"usedPercent"`
	WindowKind    string  `json:"windowKind"`
	WindowMinutes int     `json:"windowMinutes"`
}

func codexBurnSources(cfg *Config, now time.Time, maxAge time.Duration) []BurnSource {
	path := codexFeedPath(cfg)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	// 只看尾部 8MB：这个 jsonl 只增不减，早期样本对「当前窗口」毫无意义。
	if len(data) > 8*1024*1024 {
		data = data[len(data)-8*1024*1024:]
	}
	type key struct {
		acct, kind string
		winMin     int
	}
	groups := map[key][]rawSample{}
	latestPerAcct := map[string]time.Time{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var r codexFeedLine
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		at, ok := parseRFC3339(r.SampledAt)
		if !ok {
			continue
		}
		k := key{r.AccountKey, r.WindowKind, r.WindowMinutes}
		groups[k] = append(groups[k], rawSample{at: at, pct: r.UsedPercent, resetsAt: r.ResetsAt})
		if cur, seen := latestPerAcct[r.AccountKey]; !seen || at.After(cur) {
			latestPerAcct[r.AccountKey] = at
		}
	}
	labeler := newAccountLabeler("Codex ", latestPerAcct, "")
	var out []BurnSource
	for k, samples := range groups {
		src := buildBurnSource(
			"codex:"+k.kind+":"+shortAcct(k.acct), "codex", k.acct, labeler.get(k.acct),
			k.kind, codexWindowLabel(k.kind), k.winMin, samples, now, maxAge)
		if src != nil {
			out = append(out, *src)
		}
	}
	return out
}

// ---- transcript 绝对 token 曲线 ----

// tokenUsageMark / tokenAssistantMark 是逐行预筛用的字面量。提到包级是为了
// 不在每行调用里重新分配（每行两次 []byte(...) 转换，1 GB 的量级下相当可观）。
var (
	tokenUsageMark     = []byte(`"usage"`)
	tokenAssistantMark = []byte(`"assistant"`)
)

// tokenScanPlan 是一个窗口下的扫描参数：回看多久、多少分钟一桶、最多读多少字节。
//
// 【三者必须一起定】桶大小要让点数落在可画的范围（曲线画 2000 个点既卡又没信息）；
// 字节预算要够覆盖该窗口的真实体量，否则扫一半就停——而"扫了一半的全月"是**造读数**，
// 比不给这个窗口糟得多。实测体量：24h≈104MB / 7d≈419MB / 30d≈1.06GB。
// 预算取实测量的 2 倍余量：transcript 会随使用增长，卡得太紧会在某天悄悄开始截断。
type tokenScanPlan struct {
	LookbackHours int
	BucketMinutes int
	ByteBudget    int64
}

const mib = int64(1024 * 1024)

// tokenScanPlanFor 把消耗视图的窗口映射成扫描参数。
// range=all 对 transcript 没有"全部"可言（那个目录可能存着一年的历史，且没有上限），
// 故按 90 天封顶并在 basis 里说明——给一个跑不完的窗口不如给一个说得清的。
func tokenScanPlanFor(key string) tokenScanPlan {
	switch key {
	case "7d":
		return tokenScanPlan{LookbackHours: 24 * 7, BucketMinutes: 60, ByteBudget: 1024 * mib}
	case "30d":
		return tokenScanPlan{LookbackHours: 24 * 30, BucketMinutes: 360, ByteBudget: 2560 * mib}
	case "all":
		return tokenScanPlan{LookbackHours: 24 * 90, BucketMinutes: 720, ByteBudget: 4096 * mib}
	default: // 24h
		return tokenScanPlan{LookbackHours: 24, BucketMinutes: 15, ByteBudget: 512 * mib}
	}
}

type transcriptLine struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Message   struct {
		Model string `json:"model"`
		Usage *struct {
			InputTokens              float64 `json:"input_tokens"`
			OutputTokens             float64 `json:"output_tokens"`
			CacheReadInputTokens     float64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens float64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// tokenAgg 是扫描 transcript 时的累加器。除了按桶/模型分的曲线，
// 还必须同时累计四类 token 的拆分与额度口径折算值——见 TokenSeries 的口径披露说明。
type tokenAgg struct {
	cfg       *Config
	buckets   map[time.Time]map[string]float64
	models    map[string]bool
	component map[string]float64
	weighted  float64
}

// buildTokenSeries 扫 transcript 得到按模型分的绝对 token 曲线。
// 这是**不分账号**的绝对用量（与百分比源互补：百分比看还剩多少，token 看烧了多少）。
func buildTokenSeries(cfg *Config, now time.Time, rangeKey string) TokenSeries {
	plan := tokenScanPlanFor(rangeKey)
	ts := TokenSeries{
		Range: resolveSpendRange(rangeKey).Key, BucketMinutes: plan.BucketMinutes,
		Models: []string{}, Points: []TokenSeriesPoint{},
	}
	root := transcriptRoot()
	if root == "" {
		return ts
	}
	cut := now.Add(-time.Duration(plan.LookbackHours) * time.Hour)
	ts.Since = cut.UTC().Format(time.RFC3339)

	var files []string
	// 只挑最近改动过的文件：这个窗口的曲线不可能来自窗口之前就没再写过的会话。
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 单个目录读不了就跳过，不让整棵树的遍历失败
		}
		if d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().Before(cut) {
			return nil
		}
		files = append(files, p)
		return nil
	})
	ts.FilesMatched = len(files)

	agg := &tokenAgg{
		cfg:       cfg,
		buckets:   map[time.Time]map[string]float64{},
		models:    map[string]bool{},
		component: map[string]float64{},
	}
	var budget = plan.ByteBudget
	for _, p := range files {
		if budget <= 0 {
			// 撞上预算闸 = 这条曲线**不完整**。必须记下来往外发：
			// 一条少了后半段的曲线看起来和"那段时间没跑活"一模一样，静默截断就是造读数。
			ts.Truncated = true
			break
		}
		ts.FilesScanned++
		// 【CG-R3b R1 类闭合】p 来自 ~/.claude/projects 的目录遍历——那是 claude CLI 自己写的目录,
		// 属 ClaudeGo 控制域之外:WalkDir 的 d.IsDir() 对 symlink 恒为 false,一条名为 *.jsonl 的
		// symlink→FIFO 就能让 os.Open 永久阻塞,把 board 的燃尽采样 goroutine 占死(web handler
		// 再不返回)。openRegularFileNoBlock 保证 open 不挂、拿到的必是普通文件。
		f, _, err := openRegularFileNoBlock(p)
		if err != nil {
			continue
		}
		rd := bufio.NewReaderSize(f, 256*1024)
		for {
			// 用 ReadBytes 而非 Scanner：单行可能远超 Scanner 默认 64KB 上限
			// （transcript 里有把整个文件内容塞进一条消息的行）。
			line, err := rd.ReadBytes('\n')
			budget -= int64(len(line))
			ts.BytesScanned += int64(len(line))
			if len(line) > 0 {
				accumulateTokenLine(line, cut, plan.BucketMinutes, agg)
			}
			if err != nil || budget <= 0 {
				// budget<=0 时这个文件也只读了一半——同样是截断，同样要披露。
				if budget <= 0 {
					ts.Truncated = true
				}
				break
			}
		}
		f.Close()
	}

	for m := range agg.models {
		ts.Models = append(ts.Models, m)
	}
	sort.Strings(ts.Models)
	keys := make([]time.Time, 0, len(agg.buckets))
	for k := range agg.buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Before(keys[j]) })
	for _, k := range keys {
		ts.Points = append(ts.Points, TokenSeriesPoint{
			T: k.UTC().Format(time.RFC3339), ByModel: agg.buckets[k],
		})
	}

	for _, c := range []string{"input", "output", "cache_creation", "cache_read"} {
		agg.component[c] = round1(agg.component[c])
	}
	ts.ByComponent = agg.component
	ts.WeightedTotal = round1(agg.weighted)
	ts.Basis = tokenSeriesBasis(agg, &ts)
	return ts
}

// tokenSeriesBasis 如实交代 by_model 的口径，并在缓存读取占比高时明说
// 「这个数不等同于额度消耗」——它与 queue_spend.weighted_tokens 差一个数量级。
func tokenSeriesBasis(agg *tokenAgg, ts *TokenSeries) string {
	// 截断先说：一条不完整的曲线，后面所有比例数字都只覆盖读到的那部分。
	var pre string
	if ts.Truncated {
		pre = fmt.Sprintf("⚠ 本次扫描撞上字节预算上限，只读了 %d/%d 个 transcript 文件（%.1f GB）——"+
			"下面的曲线与数字**不完整**，只覆盖读到的那部分。缩短窗口可得到完整读数。",
			ts.FilesScanned, ts.FilesMatched, float64(ts.BytesScanned)/float64(1024*1024*1024))
	}
	if ts.Range == "all" {
		pre += "窗口 all 对 transcript 按 90 天封顶（那个目录没有上限，" +
			"给一个跑不完的窗口不如给一个说得清的）。"
	}
	total := 0.0
	for _, v := range agg.component {
		total += v
	}
	if total <= 0 {
		return pre + "这个窗口内没有可用的 transcript 用量样本。"
	}
	fresh := agg.component["input"] + agg.component["output"] + agg.component["cache_creation"]
	b := fmt.Sprintf(
		"by_model 是 input+output+cache_creation+cache_read **四项等权相加**的原始吞吐量，"+
			"其中缓存读取占 %.1f%%（%.0f / %.0f）。缓存读取的实际额度成本远低于全价："+
			"按 budget.go 同一套权重（cache_read 计 0.1）折算后为 %.0f，"+
			"真正新处理的 token（不含缓存读取）为 %.0f。",
		agg.component["cache_read"]/total*100, agg.component["cache_read"], total,
		agg.weighted, fresh)
	if agg.component["cache_read"] > 0 && fresh > 0 {
		b += fmt.Sprintf("等权口径相当于新处理量的 %.1f 倍，**不可**与 queue_spend.weighted_tokens 直接相比。",
			total/fresh)
	}
	return pre + b
}

func accumulateTokenLine(line []byte, cut time.Time, bucketMin int, agg *tokenAgg) {
	// 廉价预筛：绝大多数行不是带 usage 的 assistant 消息，先用子串排掉再谈 JSON 解析。
	// 用 bytes.Contains 而不是 strings.Contains(string(line), …)——后者会给**每一行**
	// 复制一份字符串。24 小时窗口是 104 MB 还看不出来，30 天窗口要扫 1.0 GB，
	// 那就是凭空多分配 1 GB、多走一遍 GC。
	if !bytes.Contains(line, tokenUsageMark) || !bytes.Contains(line, tokenAssistantMark) {
		return
	}
	var r transcriptLine
	if json.Unmarshal(line, &r) != nil {
		return
	}
	if r.Type != "assistant" || r.Message.Usage == nil {
		return
	}
	model := r.Message.Model
	// <synthetic> 是本地合成消息（如中断提示），不是真实模型调用，计入会虚增用量。
	if model == "" || model == "<synthetic>" {
		return
	}
	at, ok := parseRFC3339(r.Timestamp)
	if !ok || at.Before(cut) {
		return
	}
	u := r.Message.Usage
	total := u.InputTokens + u.OutputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	if total <= 0 {
		return
	}
	b := at.UTC().Truncate(time.Duration(bucketMin) * time.Minute)
	if agg.buckets[b] == nil {
		agg.buckets[b] = map[string]float64{}
	}
	agg.buckets[b][model] += total
	agg.models[model] = true

	agg.component["input"] += u.InputTokens
	agg.component["output"] += u.OutputTokens
	agg.component["cache_creation"] += u.CacheCreationInputTokens
	agg.component["cache_read"] += u.CacheReadInputTokens
	agg.weighted += weightedTokens(agg.cfg, model, &usageInfo{
		InputTokens:              int(u.InputTokens),
		CacheCreationInputTokens: int(u.CacheCreationInputTokens),
		CacheReadInputTokens:     int(u.CacheReadInputTokens),
		OutputTokens:             int(u.OutputTokens),
	})
}

// ---- 组装 ----

// burnCache 给燃尽视图单独做缓存：transcript 扫描是整个看板最贵的操作
// （按 mtime 过滤后仍有数十 MB），TTL 比任务快照长。
// burnCache 按**窗口**分别缓存。窗口一进 transcript 扫描的成本模型就变了：
// 24h 读 104 MB、30d 读 1.06 GB，共用一个缓存槽会让每次换标签页都重扫一遍最贵的那个。
// 每个窗口一格，各自计时。
type burnCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]*burnCacheEntry
}

type burnCacheEntry struct {
	at   time.Time
	resp *BurnResp
}

func (c *burnCache) get(root string, cfg *Config, now time.Time, rangeKey string) *BurnResp {
	key := resolveSpendRange(rangeKey).Key
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]*burnCacheEntry{}
	}
	if e := c.entries[key]; e != nil && now.Sub(e.at) < c.ttl {
		return e.resp
	}
	r := buildBurn(root, cfg, now, key)
	c.entries[key] = &burnCacheEntry{at: now, resp: r}
	return r
}

func buildBurn(root string, cfg *Config, now time.Time, rangeKey string) *BurnResp {
	maxAgeMin := 90
	if cfg != nil && cfg.UsageFeedMaxAgeMin > 0 {
		maxAgeMin = cfg.UsageFeedMaxAgeMin
	}
	maxAge := time.Duration(maxAgeMin) * time.Minute

	sources := append(claudeBurnSources(now, maxAge), codexBurnSources(cfg, now, maxAge)...)
	// 新鲜的排前面；同鲜度下用量高的排前面（用户最关心快烧完的那条）。
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].Stale != sources[j].Stale {
			return !sources[i].Stale
		}
		if sources[i].CapturedAt != sources[j].CapturedAt {
			return sources[i].CapturedAt > sources[j].CapturedAt
		}
		return sources[i].UsedPercent > sources[j].UsedPercent
	})

	spent, byModel := queueWindowSpent(root, now)
	if byModel == nil {
		byModel = map[string]float64{}
	}
	return &BurnResp{
		GeneratedAt: now.Format(time.RFC3339),
		Sources:     sources,
		TokenSeries: buildTokenSeries(cfg, now, rangeKey),
		QueueSpend: QueueSpend{
			WindowHours: windowHours, WeightedTokens: round1(spent), ByModel: byModel,
		},
	}
}

// quotaSummary 从 sources 里挑出顶部条要用的四条。
// 挑选规则：优先「非陈旧 + 最新」的那条；全陈旧时退而取最新的一条（并保留其 stale 标记，
// 让前端能明确显示「这数据老了」，而不是假装没有数据）。
func quotaSummary(sources []BurnSource) QuotaSummary {
	pick := func(provider, window string) *BurnSource {
		var best *BurnSource
		for i := range sources {
			s := &sources[i]
			if s.Provider != provider || s.Window != window {
				continue
			}
			if best == nil ||
				(best.Stale && !s.Stale) ||
				(best.Stale == s.Stale && s.CapturedAt > best.CapturedAt) {
				best = s
			}
		}
		return best
	}
	return QuotaSummary{
		ClaudeSession:  pick("claude", "session"),
		ClaudeWeekly:   pick("claude", "weekly"),
		CodexPrimary:   pick("codex", "primary"),
		CodexSecondary: pick("codex", "secondary"),
	}
}
