package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// windowHours 是 Claude 订阅限额窗口长度。账本用滑动窗口近似官方的固定窗口，
// 边界附近略保守，但不需要探测窗口起点。
const windowHours = 5

// usageRec 是一次 claude CLI 调用的额度消耗记录（本地账本 usage.json）。
type usageRec struct {
	At       int64   `json:"at"`
	TaskID   string  `json:"task"`
	Model    string  `json:"model"`
	Weighted float64 `json:"wtok"`
	// Engine 非空时这条记录是订阅引擎（config.engines）的调用：走该订阅自己的额度，
	// **不占 claude 的 5 小时红线预算**（queueWindowSpent 跳过），只作看板披露式计数。
	// 旧记录无此字段 = claude（omitempty 双向兼容）。
	Engine string `json:"engine,omitempty"`
}

func modelWeight(cfg *Config, model string) float64 {
	if w, ok := cfg.ModelWeights[model]; ok && w > 0 {
		return w
	}
	if w, ok := cfg.ModelWeights["default"]; ok && w > 0 {
		return w
	}
	return 1
}

// weightedTokens 把一次调用的 token 用量折算成加权额度。
// 近似规则：未命中缓存的输入与输出全价，缓存读取按 1 折计，再乘模型权重。
func weightedTokens(cfg *Config, model string, u *usageInfo) float64 {
	if u == nil {
		return 0
	}
	raw := float64(u.InputTokens+u.CacheCreationInputTokens+u.OutputTokens) + 0.1*float64(u.CacheReadInputTokens)
	return raw * modelWeight(cfg, model)
}

func loadUsage(root string) []usageRec {
	data, err := os.ReadFile(usagePath(root))
	if err != nil {
		return nil
	}
	var recs []usageRec
	if json.Unmarshal(data, &recs) != nil {
		return nil
	}
	return recs
}

// usageMu 保护账本的读-改-写：并行任务同时记账时不能互相覆盖。
var usageMu sync.Mutex

// appendUsage 记账并顺手把窗口外的旧记录剪掉（多留 1 小时算燃烧速率用）。
// 引擎执行（t.Runner=引擎名，runTaskVia 在 invoke 前已写好标签）打 Engine 标——引擎的
// wtok 是"该订阅的本地计数"而非 claude 额度，红线判定在 queueWindowSpent 侧按标过滤。
func appendUsage(root string, cfg *Config, t *Task, u *usageInfo) {
	usageMu.Lock()
	defer usageMu.Unlock()
	w := weightedTokens(cfg, t.Model, u)
	if w <= 0 {
		return
	}
	engine := ""
	if _, ok := cfg.Engines[t.Runner]; ok {
		engine = t.Runner // Runner 由 runTaskVia 按 via 写入，成员资格判定不误吃 codex/remote 标签
	}
	now := time.Now()
	keep := now.Add(-(windowHours + 1) * time.Hour).Unix()
	var recs []usageRec
	for _, r := range loadUsage(root) {
		if r.At >= keep {
			recs = append(recs, r)
		}
	}
	recs = append(recs, usageRec{At: now.Unix(), TaskID: t.ID, Model: t.Model, Weighted: w, Engine: engine})
	if data, err := json.Marshal(recs); err == nil {
		_ = atomicWrite(usagePath(root), append(data, '\n'))
	}
}

// queueWindowSpent 返回滑动窗口内队列消耗的加权 token 与各模型分布。
// **只统计 claude 记录**（Engine 空）：红线三通道管的是 claude 订阅的保底额度，引擎调用
// 走各自订阅的账，混进来会让红线早触发、白白把 claude 队列拦停。
func queueWindowSpent(root string, now time.Time) (float64, map[string]float64) {
	since := now.Add(-windowHours * time.Hour).Unix()
	total := 0.0
	byModel := map[string]float64{}
	for _, r := range loadUsage(root) {
		if r.At < since || r.Engine != "" {
			continue
		}
		total += r.Weighted
		m := r.Model
		if m == "" {
			m = "(默认)"
		}
		byModel[m] += r.Weighted
	}
	return total, byModel
}

// engineWindowSpent 返回滑动窗口内各引擎的本地账计数（看板披露式接入用）。
// 这是"cardex 自己派发的调用"的下限计数，不是订阅端的权威用量——各家无公开用量端点，
// 按项目纪律显式披露口径，绝不冒充燃尽数据。
func engineWindowSpent(root string, now time.Time) map[string]float64 {
	since := now.Add(-windowHours * time.Hour).Unix()
	byEngine := map[string]float64{}
	for _, r := range loadUsage(root) {
		if r.At < since || r.Engine == "" {
			continue
		}
		byEngine[r.Engine] += r.Weighted
	}
	return byEngine
}

// ---- 外部全局用量源（CodexBar usage-history.jsonl 格式）----

type feedSample struct {
	Provider      string `json:"provider"`
	SampledAt     string `json:"sampledAt"`
	ResetsAt      string `json:"resetsAt"`
	UsedPercent   int    `json:"usedPercent"`
	WindowMinutes int    `json:"windowMinutes"`
	WindowKind    string `json:"windowKind"`
}

// latestFeedSample 取用量源里最新的 claude 5 小时窗口样本。
func latestFeedSample(path string) (*feedSample, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// 只扫尾部 256KB，避免历史文件太大。
	if len(data) > 256*1024 {
		data = data[len(data)-256*1024:]
	}
	var best *feedSample
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, `"provider":"claude"`) {
			continue
		}
		var s feedSample
		if json.Unmarshal([]byte(line), &s) != nil || s.Provider != "claude" {
			continue
		}
		// 5 小时窗口：windowMinutes 300，或标记为 primary。
		if s.WindowMinutes != windowHours*60 && s.WindowKind != "primary" {
			continue
		}
		if best == nil || s.SampledAt > best.SampledAt {
			cp := s
			best = &cp
		}
	}
	if best == nil {
		return nil, fmt.Errorf("用量源里没有 claude 的 5 小时窗口样本")
	}
	return best, nil
}

// ---- 分时段红线 ----

func parseHHMM(s string) (int, bool) {
	var h, m int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// inDailyWindow 判断 now 是否落在每日 [from, to) 时段内；from > to 表示跨零点。
func inDailyWindow(now time.Time, from, to string) bool {
	f, ok1 := parseHHMM(from)
	t, ok2 := parseHHMM(to)
	if !ok1 || !ok2 || f == t {
		return false
	}
	cur := now.Hour()*60 + now.Minute()
	if f < t {
		return cur >= f && cur < t
	}
	return cur >= f || cur < t
}

// effectiveThresholds 返回此刻生效的红线阈值：时段内非零字段覆盖全局值。
// label 非空表示有时段命中（用于提示信息）。
func effectiveThresholds(cfg *Config, now time.Time) (qb int64, rp int, label string) {
	qb, rp = cfg.QueueBudgetTokens, cfg.RedlinePercent
	for _, w := range cfg.RedlineWindows {
		if inDailyWindow(now, w.From, w.To) {
			if w.QueueBudgetTokens > 0 {
				qb = w.QueueBudgetTokens
			}
			if w.RedlinePercent > 0 {
				rp = w.RedlinePercent
			}
			label = fmt.Sprintf("时段 %s-%s：", w.From, w.To)
		}
	}
	return qb, rp, label
}

// preWindowHold 判断当前是否处于某个红线时段的前置缓冲期：
// 时段开始前 RedlineLeadMin 分钟内不再起跑 claude 任务（跑起来的单步任务无法让位）。
func preWindowHold(cfg *Config, now time.Time) (bool, string) {
	if cfg.RedlineLeadMin <= 0 {
		return false, ""
	}
	cur := now.Hour()*60 + now.Minute()
	for _, w := range cfg.RedlineWindows {
		if w.RedlinePercent <= 0 && w.QueueBudgetTokens <= 0 {
			continue
		}
		f, ok := parseHHMM(w.From)
		if !ok {
			continue
		}
		start := (f - cfg.RedlineLeadMin + 1440) % 1440
		in := false
		if start < f {
			in = cur >= start && cur < f
		} else { // 缓冲跨零点
			in = cur >= start || cur < f
		}
		if in {
			return true, fmt.Sprintf("红线时段 %s-%s 前置缓冲（%d 分钟）：不再起跑 claude 任务，避免踩进预留窗口",
				w.From, w.To, cfg.RedlineLeadMin)
		}
	}
	return false, ""
}

// budgetBlocked 判定额度红线是否生效（true 则本轮不派发）。
// 三通道：本地队列预算封顶；外部用量源(usage_feed)与 oauth 端点(oauth_usage)按全局百分比停。
// 两个百分比源合并规则=**最保守值优先**（可用样本里 percent 最大者判线）——分歧不猜测，最坏假设兜住；
// 全部不可用 → fail-open 放行（沿用既有语义：数据不足不该锁队列）。
func budgetBlocked(root string, cfg *Config, now time.Time) (bool, string) {
	if hold, reason := preWindowHold(cfg, now); hold {
		return true, reason
	}
	qb, rp, label := effectiveThresholds(cfg, now)
	if qb > 0 {
		spent, _ := queueWindowSpent(root, now)
		if spent >= float64(qb) {
			return true, fmt.Sprintf("%s队列已用 %.0f/%d 加权 token（滑动 %dh 窗口），保底额度留给交互使用",
				label, spent, qb, windowHours)
		}
	}
	if rp <= 0 {
		return false, ""
	}
	reads := collectPercentReads(cfg, now)
	worst := worstAvailable(reads)
	if worst == nil || worst.Percent < rp {
		return false, ""
	}
	return true, fmt.Sprintf("%s全局 5h 窗口已用 %d%%（红线 %d%%，来源 %s%s）",
		label, worst.Percent, rp, worst.Source, worst.AgeSuffix)
}

// ---- 第三用量源：oauth/usage 端点 ----

// oauthUsageDefaultURL 是端点默认路径。**未文档化**——任何时刻 Anthropic 都可能改路径/格式。
// 因此实现里：所有异常一律按"数据不足"处理（返回 error 交由 fail-open 兜底），绝不 crash 或猜测。
const oauthUsageDefaultURL = "https://api.anthropic.com/api/oauth/usage"

// oauthCreds 是 ~/.claude/.credentials.json 的 accessToken 载体。字段名与 Claude Code 硬编码对齐。
// macOS 上凭据实际存 keychain（"Claude Code-credentials"），走 loadOAuthAccessToken 的 fallback。
type oauthCreds struct {
	ClaudeAI struct {
		AccessToken string `json:"accessToken"`
	} `json:"claudeAiOauth"`
}

// loadOAuthAccessToken 复用 Claude Code 的 OAuth accessToken。
// 优先顺序：
//  1. 配置 OAuthUsageCredsPath（测试/自定义部署用）——**硬隔离**:一旦显式指定,只读该源,不兜底;
//  2. ~/.claude/.credentials.json（Linux/Windows 明文存储）；
//  3. macOS keychain "Claude Code-credentials"（macOS 默认存储，明文文件不存在）。
//
// 取不到返回 ""——上层视为"凭据缺失"，fail-open 放行且日志披露。
// 教训:老版"OAuthUsageCredsPath 非空但读不到→fall through 到 ~/.claude"在 Windows 上让
// UserHomeDir 读 USERPROFILE 命中真实用户凭据,测试隔离形同虚设(反例:凭据缺失路径本该不发 HTTP,
// 却被兜底路径带出真凭据打到 mock),同时也剥夺了自定义部署"我明确指了别的路径,不要摸 ~/.claude"的
// 严格语义——硬隔离直接根修。
func loadOAuthAccessToken(cfg *Config) string {
	if cfg != nil && cfg.OAuthUsageCredsPath != "" {
		return readCredsFile(cfg.OAuthUsageCredsPath)
	}
	home, err := os.UserHomeDir()
	if err == nil {
		if tok := readCredsFile(filepath.Join(home, ".claude", ".credentials.json")); tok != "" {
			return tok
		}
	}
	if runtime.GOOS == "darwin" {
		if tok := readKeychainCreds(); tok != "" {
			return tok
		}
	}
	return ""
}

func readCredsFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	var c oauthCreds
	if json.Unmarshal(data, &c) != nil {
		return ""
	}
	return c.ClaudeAI.AccessToken
}

// readKeychainCreds 走 macOS `security` 工具读 "Claude Code-credentials" 项；-w 打印明文。
// 该项由 Claude Code 桌面端写入，用户已在系统里授权本二进制访问时才能读到；改名换了二进制路径 = 钥匙串 ACL 要重批（cutover 步骤）。
// 未授权/未登录 → 返回 ""，fail-open。
func readKeychainCreds() string {
	out, err := exec.Command("security", "find-generic-password", "-w", "-s", "Claude Code-credentials").Output()
	if err != nil {
		return ""
	}
	var c oauthCreds
	if json.Unmarshal(out, &c) != nil {
		return ""
	}
	return c.ClaudeAI.AccessToken
}

// oauthUsageSample 是 oauth/usage 端点的一次拉取结果。
// PercentOK=false 表示端点响应本身有（HTTP 200 + body），但里面缺 5h 字段或字段值歧义——按"数据不足"处理。
// 与"网络/凭据/HTTP 4xx-5xx 失败"分开，是为让 quota 命令能诚实披露到底哪一步断的。
// Reason 非空时携带具体不可信原因(例如"utilization=1 落在刻度歧义点"),供上层展示层露给委托人。
// SampledAt 是**首次抓取该样本的墙钟时刻**(不是每次 read 的 now)——这样 now-SampledAt 才是真实 age,
// oauth_usage_max_age_min 才有意义;老版 SampledAt=now 导致 age 恒为 0、配置形同虚设。
type oauthUsageSample struct {
	Percent   int
	PercentOK bool
	SampledAt time.Time
	Reason    string
}

// fetchOAuthUsage 直读端点，取 5h 窗口百分比。
// **只信 body**——响应头绝不参与判定（核验已推翻"响应头带 unified 限流数值"之说；
// 且响应头是最容易被中间层伪造/覆盖的信道，用它做闸门等于开天窗）。
// 任何 error → 数据不足语义；调用方按 fail-open 放行。
func fetchOAuthUsage(cfg *Config, now time.Time) (*oauthUsageSample, error) {
	tok := loadOAuthAccessToken(cfg)
	if tok == "" {
		return nil, fmt.Errorf("oauth 凭据缺失（未找到 ~/.claude/.credentials.json 或 keychain 项）")
	}
	url := cfg.OAuthUsageURL
	if url == "" {
		url = oauthUsageDefaultURL
	}
	timeout := cfg.OAuthUsageTimeoutSec
	if timeout <= 0 {
		timeout = 6
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", "cardex/"+version)
	client := &http.Client{Timeout: time.Duration(timeout) * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("端点返回 HTTP %d", resp.StatusCode)
	}
	// 64KB 上限：即便端点某日返回巨响应，也不允许它吃光内存把 tick 拖崩。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, err
	}
	return parseOAuthUsageBody(body, now)
}

// parseOAuthUsageBody 从 body 里挖 5h 窗口的百分比。
// 端点未文档化 → 用宽松+防御式解析：
//  - 尝试几种已观察到的字段路径（five_hour / fiveHour / windows[]）；
//  - 兼容 utilization / used_percent / percent 命名；
//  - 数值域按字段名硬分派、绝不自动归一（CG-1b）：utilization 认 0-100 域原样取整，
//    (0,1] 区间刻度歧义拒判；used_percent/usedPercent/percent 铁定 0-100 域原样取，详见 readPercentFields；
//  - **拿不到就返回 PercentOK=false**（"端点已变更/字段缺失/值歧义"=数据不足，不猜、不用 header 兜底）。
func parseOAuthUsageBody(body []byte, now time.Time) (*oauthUsageSample, error) {
	if !json.Valid(body) {
		return nil, fmt.Errorf("响应不是有效 JSON")
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	pct, ok, ambig := extractFiveHourPercent(raw)
	if !ok {
		return &oauthUsageSample{PercentOK: false, SampledAt: now, Reason: ambig}, nil
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return &oauthUsageSample{Percent: pct, PercentOK: true, SampledAt: now}, nil
}

// extractFiveHourPercent 深度容错——只挖已知形态；未知形态一律拒绝。
// 教训："只要看到任意百分比就用"是常见 bug 温床：端点某天新增字段 seven_day.utilization=95
// 会被误判成"5h 窗口 95%"锁死队列。所以只认明确指向 5h 的键。
// 返回 (percent, ok, ambig)——命中 5h 节点但值歧义时,整体拒绝并向上冒 ambig 披露原因,
// 不回退到别的形态(否则会给端点漂移开天窗:"这个字段可疑那就换个字段猜"是骗数据源)。
func extractFiveHourPercent(raw map[string]any) (int, bool, string) {
	// 形态 A：{"five_hour": {"utilization": 0.42}} / {"fiveHour": {"used_percent": 42}}
	for _, key := range []string{"five_hour", "fiveHour", "five_hour_window", "primary"} {
		if node, ok := raw[key].(map[string]any); ok {
			v, got, ambig := readPercentFields(node)
			if got {
				return v, true, ""
			}
			if ambig != "" {
				return 0, false, ambig
			}
		}
	}
	// 形态 B：{"windows": [{"name": "5h", ...}, ...]}
	if arr, ok := raw["windows"].([]any); ok {
		for _, item := range arr {
			node, ok := item.(map[string]any)
			if !ok {
				continue
			}
			name, _ := node["name"].(string)
			kind, _ := node["kind"].(string)
			mins, _ := node["window_minutes"].(float64)
			if !isFiveHourWindow(name, kind, int(mins)) {
				continue
			}
			v, got, ambig := readPercentFields(node)
			if got {
				return v, true, ""
			}
			if ambig != "" {
				return 0, false, ambig
			}
		}
	}
	return 0, false, ""
}

func isFiveHourWindow(name, kind string, mins int) bool {
	if mins == windowHours*60 {
		return true
	}
	n := strings.ToLower(strings.TrimSpace(name))
	k := strings.ToLower(strings.TrimSpace(kind))
	switch n {
	case "5h", "5_hour", "5-hour", "5hour", "five_hour", "primary":
		return true
	}
	return k == "primary" || k == "5h"
}

// readPercentFields 从节点里挖 utilization / used_percent / percent 字段。
// 返回 (percent, ok, ambig)——ok=true 表示读到可信百分比;ok=false 且 ambig 非空:字段命中但值歧义/域外,
// 上层应按"数据不足"披露 ambig 拒响应;ok=false 且 ambig 为空:字段全缺失(不构成异常,继续尝试兄弟节点)。
//
// 教训1(CG-1):老版做"0-1 视为分数×100、>1 视为百分比原样"的自动归一——在整数刻度崩塌:
//   * used_percent:0.8 真实语义 0.8%,老版判为分数×100→80% 假触线。
// 教训2(CG-1b):端点实测 utilization 是 0-100 百分比域（返回如 54），非 0-1 分数域；
//   (0,1] 区间存在刻度歧义（旧分数写法 vs 新百分写法），拒判为数据不足。
// 新语义：按字段名硬分派、拒绝任何自动归一，任一歧义值一律"数据不足"拒响应（fail-open）。
func readPercentFields(node map[string]any) (int, bool, string) {
	for _, key := range []string{"utilization", "used_percent", "usedPercent", "percent"} {
		v, ok := node[key]
		if !ok {
			continue
		}
		num, ok := toFloat(v)
		if !ok {
			continue
		}
		if key == "utilization" {
			// CG-1b:端点实测为 0-100 百分比域（如 54），按字段名硬分派，原样取整，永不 ×100。
			// 防双向归一护栏：(0,1] 区间刻度歧义——旧分数写法 ×100 或新百分写法原样均错，拒判。
			switch {
			case num < 0 || num > 100:
				return 0, false, fmt.Sprintf("utilization=%.4g 超出 0-100 百分比域", num)
			case num > 0 && num <= 1:
				return 0, false, fmt.Sprintf("utilization=%.4g 落在刻度歧义区间 (0,1]：旧分数域 ×100 或新百分域原样两判均错，拒判为数据不足", num)
			default:
				return int(num + 0.5), true, ""
			}
		}
		// used_percent/usedPercent/percent 铁定 0-100 百分比域,原样取整,永不 ×100
		if num < 0 || num > 100 {
			return 0, false, fmt.Sprintf("%s=%.4g 超出 0-100 百分比域", key, num)
		}
		return int(num + 0.5), true, ""
	}
	return 0, false, ""
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

// ---- 三源合并：取最保守值 ----

// percentRead 是"百分比通道"的一次读数（usage_feed / oauth_usage 各贡献一条）。
// Available=false 表示该源不可用（未配置/端点失败/凭据缺失/字段缺失/样本过期），
// worstAvailable 忽略这类读数——数据不足不该锁队列，也不该假装保守。
type percentRead struct {
	Source    string
	Available bool
	Percent   int
	Reason    string // 不可用原因披露文案（quota 命令展示用）
	AgeSuffix string // "，样本 3m 前" 之类
}

// collectPercentReads 采集所有百分比通道的当前读数（可用与不可用都保留，供 quota 展示）。
func collectPercentReads(cfg *Config, now time.Time) []percentRead {
	var out []percentRead
	out = append(out, readUsageFeedPercent(cfg, now))
	if cfg.OAuthUsage {
		out = append(out, readOAuthUsagePercent(cfg, now))
	}
	return out
}

func readUsageFeedPercent(cfg *Config, now time.Time) percentRead {
	r := percentRead{Source: "usage_feed"}
	if cfg.UsageFeed == "" {
		r.Reason = "未配置"
		return r
	}
	s, err := latestFeedSample(cfg.UsageFeed)
	if err != nil {
		r.Reason = err.Error()
		return r
	}
	at, perr := time.Parse(time.RFC3339, s.SampledAt)
	if perr != nil {
		r.Reason = "样本时间无法解析"
		return r
	}
	age := now.Sub(at)
	// 教训:老版 maxAge>0 时才 check,导致 usage_feed_max_age_min:0 语义反转成
	// "任意陈旧样本永远采信"——CodexBar 死在 99% 样本后队列被永久封锁
	// (正是 CG-1 动机里的失效模式:桌面端消耗看不见+样本冻死→队列锁死)。
	// 修法:maxAge<=0 归位默认 90 分钟,保证陈旧样本必过期→fail-open。
	maxAge := time.Duration(cfg.UsageFeedMaxAgeMin) * time.Minute
	if maxAge <= 0 {
		maxAge = 90 * time.Minute
	}
	if age > maxAge {
		r.Reason = fmt.Sprintf("样本已过期（%s 前）", age.Round(time.Minute))
		return r
	}
	r.Available = true
	r.Percent = s.UsedPercent
	r.AgeSuffix = fmt.Sprintf("，样本 %s 前", age.Round(time.Minute))
	return r
}

// oauthUsageCache 是进程级样本缓存:
//  1. 拒 tick 15s 循环每次都打端点(macOS 无凭据文件会每次落 keychain 弹窗,quota 命令并行调用同理);
//  2. 让 SampledAt/oauth_usage_max_age_min 真正有语义(过去 SampledAt=now→age 恒 0→配置形同虚设=P1-3 死配置)。
// 复用窗口=oauth_usage_max_age_min(0 用默认 15 分钟,与 quota 展示层的"新鲜度"感知一致)。
// 重抓失败时保留旧样本:让上层能披露"上一样本过期+重抓失败"而不是骤然回退到"完全无数据"。
type oauthUsageCacheState struct {
	mu        sync.Mutex
	sample    *oauthUsageSample
	fetchedAt time.Time
	lastErr   error
}

var oauthUsageCache oauthUsageCacheState

// resetOAuthUsageCache 仅供单元测试互不干扰用——生产代码不该调它。
func resetOAuthUsageCache() {
	oauthUsageCache.mu.Lock()
	defer oauthUsageCache.mu.Unlock()
	oauthUsageCache.sample = nil
	oauthUsageCache.fetchedAt = time.Time{}
	oauthUsageCache.lastErr = nil
}

// oauthUsageEffectiveMaxAge 是 max_age_min 的运行时值,0 用默认 15 分钟(fail-open 方向:"未配置"绝不"永远采信")。
func oauthUsageEffectiveMaxAge(cfg *Config) time.Duration {
	maxAge := time.Duration(cfg.OAuthUsageMaxAgeMin) * time.Minute
	if maxAge <= 0 {
		maxAge = 15 * time.Minute
	}
	return maxAge
}

// oauthUsageCachedRead 是读端口:进程级复用,过期才重抓;重抓失败保留旧样本供上层判过期。
// 返回 (sample, err)——err 非 nil 且 sample 非 nil 表示"缓存里有旧样本但本次刷新失败";
// err 非 nil 且 sample 为 nil 表示"从没抓成功过,现在也失败"(纯不可用)。
func oauthUsageCachedRead(cfg *Config, now time.Time) (*oauthUsageSample, error) {
	oauthUsageCache.mu.Lock()
	defer oauthUsageCache.mu.Unlock()
	maxAge := oauthUsageEffectiveMaxAge(cfg)
	if oauthUsageCache.sample != nil && now.Sub(oauthUsageCache.fetchedAt) < maxAge {
		return oauthUsageCache.sample, oauthUsageCache.lastErr
	}
	s, err := fetchOAuthUsage(cfg, now)
	if err != nil {
		oauthUsageCache.lastErr = err
		if oauthUsageCache.sample != nil {
			return oauthUsageCache.sample, err
		}
		return nil, err
	}
	oauthUsageCache.sample = s
	oauthUsageCache.fetchedAt = now
	oauthUsageCache.lastErr = nil
	return s, nil
}

func readOAuthUsagePercent(cfg *Config, now time.Time) percentRead {
	r := percentRead{Source: "oauth_usage"}
	sample, err := oauthUsageCachedRead(cfg, now)
	if sample == nil {
		// 从未成功抓过样本 + 本次抓取失败——纯不可用
		if err != nil {
			r.Reason = err.Error()
		} else {
			r.Reason = "尚无有效样本"
		}
		return r
	}
	if !sample.PercentOK {
		if sample.Reason != "" {
			r.Reason = sample.Reason
		} else {
			r.Reason = "响应缺 5h 窗口字段（端点可能已变更）"
		}
		return r
	}
	maxAge := oauthUsageEffectiveMaxAge(cfg)
	age := now.Sub(sample.SampledAt)
	if age > maxAge {
		// 缓存样本已过期(通常发生在"上次重抓失败,保留旧样本"路径)
		if err != nil {
			r.Reason = fmt.Sprintf("样本已过期（%s 前），重抓失败：%s", age.Round(time.Minute), err.Error())
		} else {
			r.Reason = fmt.Sprintf("样本已过期（%s 前）", age.Round(time.Minute))
		}
		return r
	}
	r.Available = true
	r.Percent = sample.Percent
	r.AgeSuffix = fmt.Sprintf("，样本 %s 前", age.Round(time.Minute))
	return r
}

// worstAvailable 返回可用读数里最保守（百分比最大）的那条。全不可用返回 nil。
// 语义："取最保守值"=最坏假设兜住分歧,而不是平均或投票——观测口径不一致时,只有极端值不会误放行。
func worstAvailable(reads []percentRead) *percentRead {
	var best *percentRead
	for i := range reads {
		r := &reads[i]
		if !r.Available {
			continue
		}
		if best == nil || r.Percent > best.Percent {
			best = r
		}
	}
	return best
}
