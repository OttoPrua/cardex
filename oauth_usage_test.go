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
// 返回:
//   - server;
//   - authPtr:最近一次请求的 Authorization 头(校验凭据确实被送过去,不是"HTTP 通了但没带凭据");
//   - countPtr:累计请求次数(供缓存复用测试断言"两次 read 只打一次端点"、
//     以及"凭据缺失时不该发起 HTTP"反例断言 *count==0)。
//
// 教训:handler 内跨 goroutine 的 t.Fatal 只会 goexit 触发 handler 那个 goroutine 的 FailNow,
// 主 goroutine 完全无感知——用 t.Errorf 报错+计数,让主 goroutine 用 count 断言实施硬校验。
func startMockOAuthServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *string, *int) {
	t.Helper()
	auth := ""
	count := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		auth = r.Header.Get("Authorization")
		if got := r.Header.Get("anthropic-beta"); got != "oauth-2025-04-20" {
			t.Errorf("缺 anthropic-beta 头，实得 %q", got)
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &auth, &count
}

// isolateHome 把测试进程的家目录环境变量整套指到 dir,
// 保证 os.UserHomeDir 兜底路径(macOS/Linux 用 HOME,Windows 用 USERPROFILE)不摸真实用户凭据。
// 教训:老测试只 t.Setenv("HOME") 在 Windows 上失效(UserHomeDir 读 USERPROFILE),
// 隔离形同虚设——TestOAuthUsageFailOpenOnMissingCreds 在 Windows 开发机上要么直接红要么静默兜底。
func isolateHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

// mustAssertBearer 断言 mock 端点确实收到了预期的 Bearer 凭据。
// 老验收清单里 authPtr 全部被 _ 丢弃——凭据是否送达从未被硬校验,
// 意味着"根本没带凭据/带错凭据"这类回归无人捕获。
func mustAssertBearer(t *testing.T, authPtr *string) {
	t.Helper()
	want := "Bearer sk-ant-oat01-fake-for-tests"
	if authPtr == nil || *authPtr != want {
		t.Fatalf("凭据未按预期送达端点:want=%q got=%q", want, ptrDeref(authPtr))
	}
}

func ptrDeref(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
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
// **注意**:老版此处 pct 传的是整数 90——利用了老 readPercentFields "num>1 视为百分比原样"的启发式;
// 新语义 utilization 严格 0-1 分数域,整数刻度已被拒绝。所以这里改用 utilization 的合法域(0.9/0.4)+
// 另留一条形态 A 走 used_percent(0-100 域直取)的场景,双保险覆盖两个字段路径。
func TestOAuthUsageBlocksAtRedline(t *testing.T) {
	dir := t.TempDir()
	creds := writeFakeCreds(t, dir)
	isolateHome(t, dir)
	resetOAuthUsageCache()
	t.Cleanup(resetOAuthUsageCache)

	pct := 90 // 起始 90%(CG-1b 实测百分域，整数形态),红线 85% → 应 block
	srv, authPtr, count := startMockOAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
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
	mustAssertBearer(t, authPtr) // 硬校验凭据实际送达端点
	firstCount := *count

	// 低于线样本：切回 40%(整数百分域) → 应放行。**必须显式清缓存**,否则 15min TTL 内命中旧样本。
	pct = 40
	resetOAuthUsageCache()
	blocked, _ = budgetBlocked(dir, cfg, time.Now())
	if blocked {
		t.Fatalf("端点报 40%% < 红线 85%% 应放行，实得 blocked=true")
	}
	if *count <= firstCount {
		t.Fatalf("清缓存后应重打端点,实得请求数未增(first=%d now=%d)", firstCount, *count)
	}
}

// 验收 2（反例注入）：body 缺 5h 字段但响应头带伪造限流数值 → 必须判"数据不足"（不 block）。
// 若实现取用了响应头数值，此测试报红。
func TestOAuthUsageIgnoresResponseHeaders(t *testing.T) {
	dir := t.TempDir()
	creds := writeFakeCreds(t, dir)
	isolateHome(t, dir)
	resetOAuthUsageCache()
	t.Cleanup(resetOAuthUsageCache)

	srv, authPtr, _ := startMockOAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		// 蓄意伪造：响应头带一堆"看起来像限流"的数字；body 里绝无 5h 窗口字段。
		w.Header().Set("X-Ratelimit-Utilization", "99")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h", "99")
		w.Header().Set("X-Used-Percent", "99")
		w.WriteHeader(http.StatusOK)
		// body 里只有 seven_day 相关字段，无 five_hour——按端点变更/字段缺失处理。
		_ = json.NewEncoder(w).Encode(map[string]any{
			"seven_day": map[string]any{"utilization": 0.05},
		})
	})
	cfg := baseCfg(t, srv.URL, creds, 85)

	blocked, _ := budgetBlocked(dir, cfg, time.Now())
	if blocked {
		t.Fatal("反例：响应头伪造 99%% 不该触发 block（只信 body，body 无 5h 字段=数据不足=fail-open）")
	}
	mustAssertBearer(t, authPtr) // 顺带核验凭据在这条路径也确实被送达

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
	isolateHome(t, dir)
	resetOAuthUsageCache()
	t.Cleanup(resetOAuthUsageCache)

	srv, authPtr, _ := startMockOAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server exploded", http.StatusInternalServerError)
	})
	cfg := baseCfg(t, srv.URL, creds, 85)

	blocked, _ := budgetBlocked(dir, cfg, time.Now())
	if blocked {
		t.Fatal("端点 500 应 fail-open，实得 blocked=true")
	}
	// 500 路径也发生了完整 HTTP 请求(先发头再收 5xx),同样必须硬校验凭据送达——
	// 这是 P1-4 同类闭合:实现若"看到未文档化端点降级为不带凭据的探针"或"漏发 Auth 头",
	// 生产端会 401→fail-open→第三源静默失效,而 5xx 路径没有 Bearer 断言的话回归无红灯。
	mustAssertBearer(t, authPtr)

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

// 凭据缺失 → fail-open + 端点绝不被调。
// 老版此处 handler 用 t.Fatal——跨 goroutine 只 goexit 该 handler 那个 goroutine 的 FailNow,
// 主 goroutine 完全无感知,加上 OAuthUsageCredsPath 老兜底 fall through 到 ~/.claude,
// 在 Windows 上直接摸真实凭据 → 隔离形同虚设。修法:硬隔离(代码侧)+ count==0 硬断言(测试侧)。
func TestOAuthUsageFailOpenOnMissingCreds(t *testing.T) {
	dir := t.TempDir()
	// 故意不写凭据文件；指向不存在的路径。
	missing := filepath.Join(dir, "does-not-exist.json")
	isolateHome(t, dir)
	resetOAuthUsageCache()
	t.Cleanup(resetOAuthUsageCache)

	srv, _, count := startMockOAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		// 用 Errorf(不用 Fatal)+计数,让主 goroutine 能明确失败;并给出回响防"静默通过测试"。
		t.Errorf("凭据缺失时不该发起 HTTP 请求(实收到 %s %s)", r.Method, r.URL.Path)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	})
	cfg := baseCfg(t, srv.URL, missing, 85)

	blocked, _ := budgetBlocked(dir, cfg, time.Now())
	if blocked {
		t.Fatal("凭据缺失应 fail-open")
	}
	if *count != 0 {
		t.Fatalf("凭据缺失时端点不应被调用,实得 %d 次(说明 loadOAuthAccessToken 兜底路径漏了真实凭据)", *count)
	}
	r := readOAuthUsagePercent(cfg, time.Now())
	if r.Available || r.Reason == "" {
		t.Fatalf("凭据缺失时应 Available=false + 明确披露，实得 %+v", r)
	}
	// 再读一次也不能触发 HTTP(缓存不该缓存"未成功抓过"状态导致下次也不发,反过来说本次没抓所以自然也没请求)。
	_ = readOAuthUsagePercent(cfg, time.Now())
	if *count != 0 {
		t.Fatalf("重复 read 仍不该发 HTTP,实得 %d 次", *count)
	}
}

// 验收 4：三源读数不同 → 按最保守值判线（可用样本里 percent 最大者）。
// 场景：queue 消耗仅 40%、usage_feed 报 60%、oauth_usage 报 82%，红线 80% → oauth 触线 block。
func TestBudgetBlockedTakesWorstOfThreeSources(t *testing.T) {
	dir := t.TempDir()
	creds := writeFakeCreds(t, dir)
	isolateHome(t, dir)
	resetOAuthUsageCache()
	t.Cleanup(resetOAuthUsageCache)

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

	// oauth_usage：mock 端点报 82%(走 used_percent 通道,0-100 域直取)。
	// 老版此处用 utilization:82(整数刻度)——新语义下 utilization 严格 0-1 分数域,
	// 82 已超出域被判"数据不足",反而不会 block,场景意图会失效。
	srv, authPtr, _ := startMockOAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"five_hour": map[string]any{"used_percent": 82},
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
	mustAssertBearer(t, authPtr) // 三源合并链路上凭据也必须送达

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
		{"扁平 five_hour + utilization 分数刻度歧义→拒判", `{"five_hour":{"utilization":0.42}}`, -1},
		{"扁平 five_hour + utilization 整数百分域→42%", `{"five_hour":{"utilization":42}}`, 42},
		{"扁平 fiveHour + used_percent 整数", `{"fiveHour":{"used_percent":73}}`, 73},
		{"windows[] 数组 + name=5h + utilization 分数刻度歧义→拒判", `{"windows":[{"name":"5h","utilization":0.55},{"name":"7d","utilization":0.1}]}`, -1},
		{"windows[] 数组 + name=5h + utilization 整数百分域", `{"windows":[{"name":"5h","utilization":55},{"name":"7d","utilization":10}]}`, 55},
		{"windows[] 数组 + window_minutes=300", `{"windows":[{"window_minutes":300,"used_percent":88}]}`, 88},
		{"只有 seven_day 无 5h", `{"seven_day":{"utilization":0.9}}`, -1},
		{"空 JSON 对象", `{}`, -1},
		{"格式奇葩：非法 JSON", `not json at all`, -1},
		// P1-3 端到端反例:审查报告字面要求的用例——used_percent=1 真实语义 1%,
		// 老版把 ≤1 一体 ×100 会读成 100% 假报"已用 100%"锁死全队列;新语义按字段名硬分派,
		// used_percent 铁定 0-100 域原样→期望 1。
		{"used_percent=1 边界真1%(不是刻度歧义)", `{"five_hour":{"used_percent":1}}`, 1},
		// utilization=0.01 落在 (0,1] 刻度歧义区间：旧分数 ×100=1% 或新百分 0.01% 均不可信，拒判。
		{"utilization=0.01 刻度歧义区间拒判", `{"five_hour":{"utilization":0.01}}`, -1},
		// utilization=1 同样在 (0,1] 歧义区间(1% vs 100% 不可分)→拒判。
		{"utilization=1 刻度歧义拒判", `{"five_hour":{"utilization":1}}`, -1},
		// utilization=1.5 在 (1,100] 百分域合法→2%（不再超出分数域拒判）。
		{"utilization=1.5 百分域合法→2%", `{"five_hour":{"utilization":1.5}}`, 2},
		// used_percent=101 超出 0-100 百分比域→数据不足,防"垃圾读数被 clamp 后当有效"。
		{"used_percent=101 超出百分比域拒判", `{"five_hour":{"used_percent":101}}`, -1},
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

// ---- P1 反例组:每一条闭合发现都自带能证伪的报红反例 ----

// TestReadPercentFieldsRejectsAmbiguousScales — P1-1 底层反例。
// 老版 readPercentFields 做"0-1 视为分数×100、>1 视为百分比原样"的自动归一,
// 在整数刻度崩塌:used_percent:0.8(真实 0.8%)→80% 假触线。
// CG-1b 追加:端点实测 utilization 是 0-100 百分比域；(0,1] 区间刻度歧义，拒判为数据不足。
// 新语义:按字段名硬分派、拒绝任何自动归一、任一歧义值一律"数据不足"拒响应。
func TestReadPercentFieldsRejectsAmbiguousScales(t *testing.T) {
	cases := []struct {
		name  string
		node  map[string]any
		want  int    // -1 表示应识别为歧义/不可信
		ambig string // 期望 ambig 关键词(空则不强求)
	}{
		{"utilization=1(整数)刻度歧义区间", map[string]any{"utilization": 1}, -1, "刻度歧义"},
		{"utilization=1.0(float)同样拒", map[string]any{"utilization": 1.0}, -1, "刻度歧义"},
		{"utilization=1.5 百分域合法值→2%", map[string]any{"utilization": 1.5}, 2, ""},
		{"utilization=-0.1 负值域外", map[string]any{"utilization": -0.1}, -1, "超出"},
		{"utilization=0.42 刻度歧义区间拒判", map[string]any{"utilization": 0.42}, -1, "刻度歧义"},
		{"utilization=0(边界零)→0%", map[string]any{"utilization": 0}, 0, ""},
		{"used_percent=0.8 老版会假触 80%,新语义原样→四舍五入 1%", map[string]any{"used_percent": 0.8}, 1, ""},
		{"used_percent=80 合法百分比", map[string]any{"used_percent": 80}, 80, ""},
		{"used_percent=101 超出百分比域", map[string]any{"used_percent": 101}, -1, "超出"},
		{"usedPercent camelCase 同规矩", map[string]any{"usedPercent": 0.5}, 1, ""}, // 0.5% → 四舍五入 1
		{"percent 别名同规矩(不 ×100)", map[string]any{"percent": 42}, 42, ""},
	}
	for _, c := range cases {
		pct, ok, ambig := readPercentFields(c.node)
		if c.want >= 0 {
			if !ok {
				t.Errorf("%s: 应 ok=true pct=%d, got !ok ambig=%q", c.name, c.want, ambig)
				continue
			}
			if pct != c.want {
				t.Errorf("%s: 期望 pct=%d, got %d", c.name, c.want, pct)
			}
			continue
		}
		if ok {
			t.Errorf("%s: 应识别为歧义/域外, got pct=%d", c.name, pct)
			continue
		}
		if c.ambig != "" && !strContains(ambig, c.ambig) {
			t.Errorf("%s: ambig 应含 %q,实得 %q", c.name, c.ambig, ambig)
		}
	}
}

// TestOAuthUsageFailOpenOnUtilizationOne — P1-1 高层反例(end-to-end)。
// 端点回 utilization:1(真实1%),红线设 5%——老版按分数上限 ×100→100% 触线锁死;
// 新语义按刻度歧义拒判→fail-open 放行且披露原因。
func TestOAuthUsageFailOpenOnUtilizationOne(t *testing.T) {
	dir := t.TempDir()
	creds := writeFakeCreds(t, dir)
	isolateHome(t, dir)
	resetOAuthUsageCache()
	t.Cleanup(resetOAuthUsageCache)

	srv, _, _ := startMockOAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"five_hour": map[string]any{"utilization": 1},
		})
	})
	cfg := baseCfg(t, srv.URL, creds, 5) // 红线故意设很低放大回归威力

	blocked, reason := budgetBlocked(dir, cfg, time.Now())
	if blocked {
		t.Fatalf("反例:utilization=1(刻度歧义)应 fail-open,实得 blocked=true reason=%q", reason)
	}
	r := readOAuthUsagePercent(cfg, time.Now())
	if r.Available {
		t.Fatalf("反例:utilization=1 时 Available 必须为 false,实得 %+v", r)
	}
	if !strContains(r.Reason, "歧义") {
		t.Fatalf("Reason 应披露刻度歧义,实得 %q", r.Reason)
	}
}

// TestOAuthUsageFailOpenOnUsedPercentSubOne — P1-1 高层反例(端到端第二类字段路径)。
// 端点回 used_percent:0.8(真实 0.8%),红线 5%——老版分数×100→80% 触线锁死;
// 新语义 used_percent 铁定 0-100 域原样→四舍五入 1%<5% 放行。
func TestOAuthUsageFailOpenOnUsedPercentSubOne(t *testing.T) {
	dir := t.TempDir()
	creds := writeFakeCreds(t, dir)
	isolateHome(t, dir)
	resetOAuthUsageCache()
	t.Cleanup(resetOAuthUsageCache)

	srv, _, _ := startMockOAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"five_hour": map[string]any{"used_percent": 0.8},
		})
	})
	cfg := baseCfg(t, srv.URL, creds, 5)

	blocked, reason := budgetBlocked(dir, cfg, time.Now())
	if blocked {
		t.Fatalf("反例:used_percent=0.8(真实 <1%%)不应触发 5%% 红线,实得 blocked=true reason=%q", reason)
	}
	r := readOAuthUsagePercent(cfg, time.Now())
	if !r.Available {
		t.Fatalf("低值样本应 Available=true,实得 %+v", r)
	}
	if r.Percent > 1 {
		t.Fatalf("used_percent=0.8 原样应四舍五入到 1(不 ×100),实得 %d", r.Percent)
	}
}

// TestUsageFeedFailOpenOnMaxAgeZero — P1-2 反例(usage_feed 侧)。
// 老版 maxAge>0 时才 check,导致 usage_feed_max_age_min:0 语义反转为"任意陈旧样本永远采信"——
// CodexBar 死在 99% 样本后队列被永久封锁(正是 CG-1 动机的失效模式)。
// 修复:maxAge<=0 归位默认 90 分钟,陈旧样本必过期→fail-open。
func TestUsageFeedFailOpenOnMaxAgeZero(t *testing.T) {
	dir := t.TempDir()
	feedPath := filepath.Join(dir, "usage-history.jsonl")
	// 样本 120min 前(> 默认 90min TTL);usedPercent=99(若采信必 block)。
	old := time.Now().UTC().Add(-120 * time.Minute)
	line, _ := json.Marshal(map[string]any{
		"provider":      "claude",
		"sampledAt":     old.Format(time.RFC3339),
		"resetsAt":      old.Add(5 * time.Hour).Format(time.RFC3339),
		"usedPercent":   99,
		"windowMinutes": 300,
		"windowKind":    "primary",
	})
	if err := os.WriteFile(feedPath, append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		RedlinePercent:     50,
		UsageFeed:          feedPath,
		UsageFeedMaxAgeMin: 0, // 反例键
	}
	blocked, reason := budgetBlocked(dir, cfg, time.Now())
	if blocked {
		t.Fatalf("反例:max_age_min=0 应归位默认 90min→陈旧样本过期→fail-open,实得 blocked=true reason=%q", reason)
	}
	r := readUsageFeedPercent(cfg, time.Now())
	if r.Available {
		t.Fatalf("陈旧样本应 Available=false,实得 %+v", r)
	}
	if !strContains(r.Reason, "已过期") {
		t.Fatalf("Reason 应披露样本已过期,实得 %q", r.Reason)
	}
}

// TestOAuthUsageCacheTTLLifecycle — P1-2 同类反例(oauth 侧) + P1-3 缓存反例(合并覆盖)。
// 覆盖三点:①max_age_min=0 归位默认 15min(不是"永远采信");②TTL 内 read 复用缓存,端点只打 1 次
// (防 tick 15s 循环打端点+macOS 弹 keychain);③超 TTL 触发重抓。
func TestOAuthUsageCacheTTLLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name         string
		maxAgeMin    int
		reuseAtMin   int
		refetchAtMin int
	}{
		{"max_age=0 归位默认 15min", 0, 10, 16},
		{"max_age=30 显式指定", 30, 25, 31},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			creds := writeFakeCreds(t, dir)
			isolateHome(t, dir)
			resetOAuthUsageCache()
			t.Cleanup(resetOAuthUsageCache)

			srv, _, count := startMockOAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"five_hour": map[string]any{"used_percent": 42},
				})
			})
			cfg := baseCfg(t, srv.URL, creds, 85)
			cfg.OAuthUsageMaxAgeMin = tc.maxAgeMin

			now := time.Now()
			r1 := readOAuthUsagePercent(cfg, now)
			if !r1.Available || r1.Percent != 42 {
				t.Fatalf("首次 read 应命中新样本 42%%, 实得 %+v", r1)
			}
			if *count != 1 {
				t.Fatalf("首次 read 应打端点一次,实得 %d", *count)
			}

			r2 := readOAuthUsagePercent(cfg, now.Add(time.Duration(tc.reuseAtMin)*time.Minute))
			if !r2.Available || r2.Percent != 42 {
				t.Fatalf("TTL 内复用缓存,应仍 42%%, 实得 %+v", r2)
			}
			if *count != 1 {
				t.Fatalf("TTL 内应复用缓存,实得端点被打 %d 次(若>1 则缓存被拒,P1-3 死配置回归)", *count)
			}

			r3 := readOAuthUsagePercent(cfg, now.Add(time.Duration(tc.refetchAtMin)*time.Minute))
			if !r3.Available || r3.Percent != 42 {
				t.Fatalf("超 TTL 重抓应拿到新样本,实得 %+v", r3)
			}
			if *count != 2 {
				t.Fatalf("超 TTL 应重抓端点,实得 %d 次(若=1 则 max_age=0 又被反转成永远采信=P1-2 回归)", *count)
			}
		})
	}
}

// TestOAuthUsageRefetchFailureKeepsStaleUntilExpiry — P1-3 反例(重抓失败保留旧样本)。
// 场景:缓存里已有好样本,过 TTL 后重抓失败——不应回退到"完全无数据",而应保留旧样本并把
// Reason 披露成"过期+重抓失败",让 quota 能诚实展示"曾拿到过 42%,现在端点炸了"。
func TestOAuthUsageRefetchFailureKeepsStaleUntilExpiry(t *testing.T) {
	dir := t.TempDir()
	creds := writeFakeCreds(t, dir)
	isolateHome(t, dir)
	resetOAuthUsageCache()
	t.Cleanup(resetOAuthUsageCache)

	fail := false
	srv, _, _ := startMockOAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"five_hour": map[string]any{"used_percent": 42},
		})
	})
	cfg := baseCfg(t, srv.URL, creds, 85)
	cfg.OAuthUsageMaxAgeMin = 15

	now := time.Now()
	r1 := readOAuthUsagePercent(cfg, now)
	if !r1.Available || r1.Percent != 42 {
		t.Fatalf("首抓应成功,实得 %+v", r1)
	}

	// 端点切故障,推进到 TTL 之外→缓存过期,重抓失败,保留旧样本但披露过期。
	fail = true
	r2 := readOAuthUsagePercent(cfg, now.Add(20*time.Minute))
	if r2.Available {
		t.Fatalf("过期+重抓失败应 Available=false,实得 %+v", r2)
	}
	if !containsAll(r2.Reason, "已过期", "重抓失败") {
		t.Fatalf("Reason 应披露过期与重抓失败两要素,实得 %q", r2.Reason)
	}
}

// TestLoadOAuthAccessTokenHardIsolatesWhenCredsPathSet — P1-4 反例(硬隔离)。
// 老版 OAuthUsageCredsPath 非空但读不到会 fall through 到 ~/.claude——
// Windows 上 UserHomeDir 读 USERPROFILE 命中真实用户凭据,测试隔离失效。
// 新语义:显式指定即硬信,读不到就返回 ''(不再兜底),自定义部署"我不要摸别的路径"的严格语义也回来了。
func TestLoadOAuthAccessTokenHardIsolatesWhenCredsPathSet(t *testing.T) {
	dir := t.TempDir()
	// 在 HOME/.claude/.credentials.json 塞一份"真实"凭据(硬隔离应绝不读它)。
	claudeDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	homeCreds := `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-REAL-should-NOT-be-touched"}}`
	if err := os.WriteFile(filepath.Join(claudeDir, ".credentials.json"), []byte(homeCreds), 0o600); err != nil {
		t.Fatal(err)
	}
	isolateHome(t, dir)

	// 显式指定不存在的 creds path — 硬隔离应返回 ''。
	cfg := &Config{OAuthUsageCredsPath: filepath.Join(dir, "does-not-exist.json")}
	if tok := loadOAuthAccessToken(cfg); tok != "" {
		t.Fatalf("硬隔离反例:cred path 非空但读不到,应返回 '',绝不 fall through 到 ~/.claude,实得 %q", tok)
	}

	// 自检:如果不指定 cred path,兜底 HOME 应确实能读到我们塞的假真实凭据——
	// 证明测试环境有效(即"真实凭据确实在那里,只是被硬隔离忽略了")。
	cfg2 := &Config{}
	if tok := loadOAuthAccessToken(cfg2); tok != "sk-ant-oat01-REAL-should-NOT-be-touched" {
		t.Fatalf("自检:未硬隔离时兜底 HOME 路径应命中,实得 %q", tok)
	}
}

// TestCG1bUtilizationPercentDomain — CG-1b 回归锚：utilization 字段实测为 0-100 百分比域。
//
// 正反双例证伪两侧归一方向：
//   - 正例 utilization=54 → 54%（不再"数据不足"）；
//   - 反例 utilization=0.54 旧分数形态 → 拒判（旧 ×100=54% 或新原样=0.54%，两判均错）。
//
// 另：从 testdata/oauth_usage_fixture.json 读脱敏实样 fixture 作额外回归锚（字段形状照真响应，数值已改）。
func TestCG1bUtilizationPercentDomain(t *testing.T) {
	now := time.Now()

	// 单元层（直接走 parseOAuthUsageBody）
	unitCases := []struct {
		name      string
		body      string
		wantPct   int
		wantOK    bool
		wantAmbig string
	}{
		// 正例：实测形态，直接判 54%
		{"utilization=54 百分域→54%", `{"five_hour":{"utilization":54}}`, 54, true, ""},
		// 反例①：旧分数形态，两判均错，拒判（双向防归一）
		{"utilization=0.54 刻度歧义拒判", `{"five_hour":{"utilization":0.54}}`, 0, false, "刻度歧义"},
	}
	for _, c := range unitCases {
		s, err := parseOAuthUsageBody([]byte(c.body), now)
		if c.wantOK {
			if err != nil {
				t.Errorf("%s: 不应 error，实得 %v", c.name, err)
				continue
			}
			if s == nil || !s.PercentOK || s.Percent != c.wantPct {
				t.Errorf("%s: 期望 %d%%，实得 %+v", c.name, c.wantPct, s)
			}
			continue
		}
		// 反例：应拒判（PercentOK=false 或 error）
		if err == nil && s != nil && s.PercentOK {
			t.Errorf("%s: 应拒判为数据不足，实得 percent=%d", c.name, s.Percent)
			continue
		}
		if c.wantAmbig != "" {
			reason := ""
			if s != nil {
				reason = s.Reason
			}
			if !strContains(reason, c.wantAmbig) {
				t.Errorf("%s: Reason 应含 %q，实得 %q", c.name, c.wantAmbig, reason)
			}
		}
	}

	// 脱敏 fixture 回归锚（testdata/oauth_usage_fixture.json）
	fixBody, err := os.ReadFile("testdata/oauth_usage_fixture.json")
	if err != nil {
		t.Fatalf("读 fixture 失败: %v", err)
	}
	fs, ferr := parseOAuthUsageBody(fixBody, now)
	if ferr != nil {
		t.Fatalf("fixture 解析出错: %v", ferr)
	}
	if fs == nil || !fs.PercentOK {
		t.Fatalf("fixture 应解析为有效百分比，实得 %+v", fs)
	}
	if fs.Percent < 0 || fs.Percent > 100 {
		t.Fatalf("fixture 百分比超出 [0,100] 域，实得 %d", fs.Percent)
	}
}

// mockOAuthDiscoveredShape 是保留一个手工快速探针——用来验证真实端点响应结构时可以临时启用。
// 生产测试不打真实网络。
var _ = fmt.Sprintf
