package main

// CG-1 订阅用量端点直读（第三用量源）验收回归。
//
// 端点未文档化、可随时变更——所有异常路径必须按"数据不足"处理，绝不 crash、绝不静默锁队列。
// 四条验收对应可借鉴清单 §1：mock 端点覆盖派发/反例注入/fail-open/三源分歧四条主线。
// 反例注入的教训：曾有实现拿响应头凑数——正是这类"看起来聪明的兜底"给了伪造/漂移可乘之机；
// 本模块的铁律=**只信 body**。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeFakeCreds 写一份最小合法凭据（仅 accessToken 有值），供测试端点认证使用。
func writeFakeCreds(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, ".credentials.json")
	body := `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-fake-for-tests","refreshToken":"","expiresAt":0}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// startMockOAuthServer 起一个 mock oauth/usage 端点。
// - authHeader 记录最近一次请求的 Authorization 头（校验凭据确实被送过去）；
// - handler 由测试自定义（每条验收路径的响应形态不同）。
func startMockOAuthServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *string) {
	t.Helper()
	auth := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if got := r.Header.Get("anthropic-beta"); got != "oauth-2025-04-20" {
			t.Errorf("缺 anthropic-beta 头，实得 %q", got)
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &auth
}

// baseCfg 是四条验收共享的最小配置——启用 oauth_usage 且注入 mock 端点/凭据文件。
func baseCfg(t *testing.T, url, credsPath string, rp int) *Config {
	t.Helper()
	return &Config{
		RedlinePercent:       rp,
		UsageFeedMaxAgeMin:   90,
		OAuthUsage:           true,
		OAuthUsageURL:        url,
		OAuthUsageCredsPath:  credsPath,
		OAuthUsageMaxAgeMin:  15,
		OAuthUsageTimeoutSec: 3,
	}
}

// 验收 1：mock 端点返回 utilization ≥ redline_percent → tick 不派发；改注入低于线样本 → 恢复派发。
func TestOAuthUsageBlocksAtRedline(t *testing.T) {
	dir := t.TempDir()
	creds := writeFakeCreds(t, dir)

	pct := 90 // 起始 90%，红线 85% → 应 block
	srv, _ := startMockOAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"five_hour": map[string]any{"utilization": pct},
		})
	})
	cfg := baseCfg(t, srv.URL, creds, 85)

	blocked, reason := budgetBlocked(dir, cfg, time.Now())
	if !blocked {
		t.Fatalf("端点报 %d%% ≥ 红线 85%% 应 block，实得 blocked=false", pct)
	}
	if reason == "" {
		t.Fatal("block 时必须有 reason 披露")
	}

	// 低于线样本：切回 40% → 应放行。
	pct = 40
	blocked, _ = budgetBlocked(dir, cfg, time.Now())
	if blocked {
		t.Fatalf("端点报 40%% < 红线 85%% 应放行，实得 blocked=true")
	}
}

// 验收 2（反例注入）：body 缺 5h 字段但响应头带伪造限流数值 → 必须判"数据不足"（不 block）。
// 若实现取用了响应头数值，此测试报红。
func TestOAuthUsageIgnoresResponseHeaders(t *testing.T) {
	dir := t.TempDir()
	creds := writeFakeCreds(t, dir)

	srv, _ := startMockOAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		// 蓄意伪造：响应头带一堆"看起来像限流"的数字；body 里绝无 5h 窗口字段。
		w.Header().Set("X-Ratelimit-Utilization", "99")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h", "99")
		w.Header().Set("X-Used-Percent", "99")
		w.WriteHeader(http.StatusOK)
		// body 里只有 seven_day 相关字段，无 five_hour——按端点变更/字段缺失处理。
		_ = json.NewEncoder(w).Encode(map[string]any{
			"seven_day": map[string]any{"utilization": 5},
		})
	})
	cfg := baseCfg(t, srv.URL, creds, 85)

	blocked, _ := budgetBlocked(dir, cfg, time.Now())
	if blocked {
		t.Fatal("反例：响应头伪造 99%% 不该触发 block（只信 body，body 无 5h 字段=数据不足=fail-open）")
	}

	// 语义层面双保险：直接调 fetchOAuthUsage，PercentOK 必须为 false。
	sample, err := fetchOAuthUsage(cfg, time.Now())
	if err != nil {
		t.Fatalf("HTTP 200 且 body 合法 JSON 时不应报网络错，实得 %v", err)
	}
	if sample.PercentOK {
		t.Fatal("反例：body 缺 5h 字段时 PercentOK 必须为 false（若解析响应头凑数，此断言变红）")
	}
}

// 验收 3：mock 端点 500 / 凭据缺失 → 派发行为与无此源时逐一致（fail-open 回归）；quota 标注不可用。
func TestOAuthUsageFailOpenOn500(t *testing.T) {
	dir := t.TempDir()
	creds := writeFakeCreds(t, dir)

	srv, _ := startMockOAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server exploded", http.StatusInternalServerError)
	})
	cfg := baseCfg(t, srv.URL, creds, 85)

	blocked, _ := budgetBlocked(dir, cfg, time.Now())
	if blocked {
		t.Fatal("端点 500 应 fail-open，实得 blocked=true")
	}

	// 与"完全没配置 oauth_usage"的基线一致。
	baseCfg := *cfg
	baseCfg.OAuthUsage = false
	blocked2, _ := budgetBlocked(dir, &baseCfg, time.Now())
	if blocked != blocked2 {
		t.Fatalf("500 时行为应与无此源等价：oauth=on blocked=%v vs oauth=off blocked=%v", blocked, blocked2)
	}

	// quota 展示层：readOAuthUsagePercent 必须给出可读的 Reason。
	r := readOAuthUsagePercent(cfg, time.Now())
	if r.Available {
		t.Fatal("500 时 Available 必须为 false")
	}
	if r.Reason == "" {
		t.Fatal("500 时必须给出披露原因，供 quota 标注")
	}
}

func TestOAuthUsageFailOpenOnMissingCreds(t *testing.T) {
	dir := t.TempDir()
	// 故意不写凭据文件；指向不存在的路径。
	missing := filepath.Join(dir, "does-not-exist.json")
	srv, _ := startMockOAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("凭据缺失时不该发起 HTTP 请求")
	})
	cfg := baseCfg(t, srv.URL, missing, 85)
	// 关掉 macOS keychain 兜底路径：把 HOME 也指到临时目录，确保 ~/.claude/.credentials.json 不存在。
	t.Setenv("HOME", dir)

	blocked, _ := budgetBlocked(dir, cfg, time.Now())
	if blocked {
		t.Fatal("凭据缺失应 fail-open")
	}
	r := readOAuthUsagePercent(cfg, time.Now())
	if r.Available || r.Reason == "" {
		t.Fatalf("凭据缺失时应 Available=false + 明确披露，实得 %+v", r)
	}
}

// 验收 4：三源读数不同 → 按最保守值判线（可用样本里 percent 最大者）。
// 场景：queue 消耗仅 40%、usage_feed 报 60%、oauth_usage 报 82%，红线 80% → oauth 触线 block。
func TestBudgetBlockedTakesWorstOfThreeSources(t *testing.T) {
	dir := t.TempDir()
	creds := writeFakeCreds(t, dir)

	// usage_feed：写一份 CodexBar 格式的 jsonl，最新 claude 5h 窗口样本 60%。
	feedPath := filepath.Join(dir, "usage-history.jsonl")
	now := time.Now().UTC()
	feedLine, _ := json.Marshal(map[string]any{
		"provider":      "claude",
		"sampledAt":     now.Format(time.RFC3339),
		"resetsAt":      now.Add(3 * time.Hour).Format(time.RFC3339),
		"usedPercent":   60,
		"windowMinutes": 300,
		"windowKind":    "primary",
	})
	if err := os.WriteFile(feedPath, append(feedLine, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	// oauth_usage：mock 端点报 82%。
	srv, _ := startMockOAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"five_hour": map[string]any{"utilization": 82},
		})
	})

	cfg := baseCfg(t, srv.URL, creds, 80)
	cfg.UsageFeed = feedPath
	cfg.QueueBudgetTokens = 1_000_000 // 队列封顶 1M，消耗只有 40% ≈ 400K，队列通道不触线

	// 队列账本 40 万加权 token（sim 队列消耗，不参与百分比合并——是独立通道）。
	spent := usageRec{At: now.Unix(), TaskID: "sim", Model: "opus", Weighted: 400000}
	blob, _ := json.Marshal([]usageRec{spent})
	if err := os.WriteFile(usagePath(dir), blob, 0o644); err != nil {
		t.Fatal(err)
	}

	blocked, reason := budgetBlocked(dir, cfg, time.Now())
	if !blocked {
		t.Fatal("三源分歧 40/60/82，红线 80，应按最保守 82 判线 block")
	}
	// reason 必须点名"最保守"来源，防未来实现回退到"平均/投票/首命中"。
	if !containsAll(reason, "82", "oauth_usage") {
		t.Fatalf("reason 应披露 82%% 且来自 oauth_usage，实得 %q", reason)
	}

	// worstAvailable 直接断言合并逻辑：两个百分比源合并出 82（不是 60，也不是 71 平均）。
	reads := collectPercentReads(cfg, time.Now())
	worst := worstAvailable(reads)
	if worst == nil || worst.Percent != 82 || worst.Source != "oauth_usage" {
		t.Fatalf("worstAvailable 期望 oauth_usage/82，实得 %+v", worst)
	}
}

// ---- 端点响应形态兼容性（既然未文档化，多留几形态测试兜住格式漂移）----

func TestParseOAuthUsageBodyShapes(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		body string
		want int // 期望的 5h 百分比；-1 表示应识别为"数据不足"
	}{
		{"扁平 five_hour + utilization 分数", `{"five_hour":{"utilization":0.42}}`, 42},
		{"扁平 fiveHour + used_percent 整数", `{"fiveHour":{"used_percent":73}}`, 73},
		{"windows[] 数组 + name=5h", `{"windows":[{"name":"5h","utilization":0.55},{"name":"7d","utilization":0.1}]}`, 55},
		{"windows[] 数组 + window_minutes=300", `{"windows":[{"window_minutes":300,"used_percent":88}]}`, 88},
		{"只有 seven_day 无 5h", `{"seven_day":{"utilization":0.9}}`, -1},
		{"空 JSON 对象", `{}`, -1},
		{"格式奇葩：非法 JSON", `not json at all`, -1},
	}
	for _, c := range cases {
		s, err := parseOAuthUsageBody([]byte(c.body), now)
		if c.want < 0 {
			// 未识别形态：允许 err 或 PercentOK=false，二者都视为"数据不足"。
			if err == nil && s != nil && s.PercentOK {
				t.Errorf("%s: 应识别为数据不足，实得 percent=%d", c.name, s.Percent)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: 解析出错 %v", c.name, err)
			continue
		}
		if !s.PercentOK || s.Percent != c.want {
			t.Errorf("%s: 期望 %d%%，实得 %+v", c.name, c.want, s)
		}
	}
}

// containsAll 是一个便利函数：判定 s 是否包含所有 subs（顺序无关）。
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strContains(s, sub) {
			return false
		}
	}
	return true
}

// strContains 避免引入 strings 包依赖冲突；仅做逐字符匹配。
func strContains(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// mockOAuthDiscoveredShape 是保留一个手工快速探针——用来验证真实端点响应结构时可以临时启用。
// 生产测试不打真实网络。
var _ = fmt.Sprintf
