package main

// 多订阅引擎档案（engines.go）回归测试。
// 覆盖面按事故等级排：①限额判据与冷却分账（Kimi 撞限额挂住 claude 队列 = 最坏静默事故）；
// ②env 注入串味（A 引擎的调用打到 B 端点/花错账）；③降级链质量地板（复审被静默降档）；
// ④凭据泄露出口（cmd/doctor 只许引用形态）。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---- 配置校验 ----

func TestValidateEnginesRejectsBadConfigs(t *testing.T) {
	base := func() *Config {
		cfg := defaultConfig("claude")
		cfg.Engines = map[string]EngineProfile{}
		return cfg
	}
	cases := []struct {
		name string
		mut  func(*Config)
	}{
		{"保留字 codex", func(c *Config) { c.Engines["codex"] = EngineProfile{BaseURL: "https://x"} }},
		{"保留字 claude", func(c *Config) { c.Engines["claude"] = EngineProfile{BaseURL: "https://x"} }},
		{"保留字 remote", func(c *Config) { c.Engines["remote"] = EngineProfile{BaseURL: "https://x"} }},
		{"非法字符（要进文件名）", func(c *Config) { c.Engines["Kimi/CN"] = EngineProfile{BaseURL: "https://x"} }},
		{"base_url 为空", func(c *Config) { c.Engines["kimi"] = EngineProfile{} }},
		{"auth_var 不合法", func(c *Config) {
			c.Engines["kimi"] = EngineProfile{BaseURL: "https://x", AuthVar: "ANTHROPIC_TOKEN"}
		}},
		{"models 键不是档位别名", func(c *Config) {
			c.Engines["kimi"] = EngineProfile{BaseURL: "https://x", Models: map[string]string{"turbo": "k3"}}
		}},
		{"fallback_order 引用未配置引擎", func(c *Config) { c.FallbackOrder = []string{"codex", "ghost"} }},
	}
	for _, tc := range cases {
		cfg := base()
		tc.mut(cfg)
		if err := validateEngines(cfg); err == nil {
			t.Errorf("%s: 应校验失败，却通过了", tc.name)
		}
	}
	ok := base()
	ok.Engines["kimi"] = EngineProfile{BaseURL: "https://api.kimi.com/coding/", AuthEnv: "KIMI_API_KEY",
		AuthVar: "ANTHROPIC_API_KEY", Models: map[string]string{"opus": "k3"}}
	ok.FallbackOrder = []string{"codex", "kimi"}
	if err := validateEngines(ok); err != nil {
		t.Fatalf("合法配置不应报错: %v", err)
	}
}

// 内置预设必须全部能过校验——预设表改坏（typo 的 auth_var、非法档位键）要在测试层拦住，
// 不能等用户 engines add 之后 loadConfig 才炸。
func TestEnginePresetsAllValid(t *testing.T) {
	cfg := defaultConfig("claude")
	cfg.Engines = enginePresets()
	if err := validateEngines(cfg); err != nil {
		t.Fatalf("内置预设未过校验: %v", err)
	}
	for name, p := range enginePresets() {
		if p.AuthEnv == "" {
			t.Errorf("预设 %s 缺 auth_env（预设不许带明文凭据，必须走环境变量引用）", name)
		}
		if p.AuthValue != "" {
			t.Errorf("预设 %s 带 auth_value 明文——预设绝不许携带凭据", name)
		}
		if p.Tier == "" {
			t.Errorf("预设 %s 缺 tier 档位标注（统一分级表是委托人指定的交付物）", name)
		}
	}
}

// TestDefaultConfigShipsNoBuiltinEngines 是 engines 判"不命中覆写截断类"的依据（登记见
// configtables_test.go）：内置表不预置任何引擎条目——预设只经 `cardex engines add` 显式并入，
// 条目全部由用户 config 自持，没有"内置值被部分覆写截断"这回事。
// 【突变致死】哪天把 enginePresets 直接烘进 defaultConfig，这里红，逼人重做分类判定
// （那时部分覆写 engines.kimi 会把内置预设的 models/auth_var 打成零值）。
func TestDefaultConfigShipsNoBuiltinEngines(t *testing.T) {
	if n := len(defaultConfig("claude").Engines); n != 0 {
		t.Errorf("defaultConfig 现在预置了 %d 个 engines 条目: "+
			"engines 此前按'无内置值可被截断'判为不命中覆写截断类, 该判据已失效, 请重新判定并补字段级回落", n)
	}
}

// TestEngineProfileZeroFieldsFailClosed 钉住"档内缺字段全落保守默认或响错"的判据本身：
// 用户手写 engines 条目时漏掉可选字段，每个缺口的运行时语义都必须是保守侧——
// 静默放宽（如 auth_var 拼错静默用错凭据槽、缺认证静默改用本机订阅跑）才是本类事故。
func TestEngineProfileZeroFieldsFailClosed(t *testing.T) {
	// auth_var 缺省 → AUTH_TOKEN（合法枚举内的保守默认，非猜测）。
	if v := engineAuthVar(EngineProfile{}); v != "ANTHROPIC_AUTH_TOKEN" {
		t.Errorf("auth_var 缺省应落 ANTHROPIC_AUTH_TOKEN, got %q", v)
	}
	// 认证三口全缺 → 响错（invokeEngine 层归 auth 类挂 held），绝不静默继续。
	if _, err := resolveEngineAuth(EngineProfile{}); err == nil {
		t.Error("认证全缺必须响错，不得静默放行（那会让 claude CLI 静默用本机订阅跑、额度花错账）")
	}
	// limit_fallback_min 缺省 → 继承全局（TestEngineResetEpochFallbacks 钉行为）。
	// models/default_model 全缺 → 透传并披露（TestResolveEngineModelMapping 钉行为）。
}

// ---- 凭据解析 ----

func TestResolveEngineAuthPrecedence(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "k.key")
	if err := os.WriteFile(keyFile, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CARDEX_TEST_KEY", "env-secret")

	// env > file > value
	p := EngineProfile{AuthEnv: "CARDEX_TEST_KEY", AuthFile: keyFile, AuthValue: "literal"}
	if v, err := resolveEngineAuth(p); err != nil || v != "env-secret" {
		t.Fatalf("auth_env 应最优先: v=%q err=%v", v, err)
	}
	// env 名配置了但变量为空：报错而不是静默落文件——配置写了 auth_env 就是明确意图，
	// 静默换来源会让"密钥换错账号"不可见。
	t.Setenv("CARDEX_TEST_KEY", "")
	if _, err := resolveEngineAuth(p); err == nil {
		t.Fatal("auth_env 变量为空应报错，不得静默回落 auth_file")
	}
	if v, err := resolveEngineAuth(EngineProfile{AuthFile: keyFile}); err != nil || v != "file-secret" {
		t.Fatalf("auth_file 应去空白读出: v=%q err=%v", v, err)
	}
	if v, err := resolveEngineAuth(EngineProfile{AuthValue: "literal"}); err != nil || v != "literal" {
		t.Fatalf("auth_value 兜底: v=%q err=%v", v, err)
	}
	if _, err := resolveEngineAuth(EngineProfile{}); err == nil {
		t.Fatal("三者全缺应报错")
	}
}

// cmd/doctor 出口只许引用形态——断言 engineAuthRef 永不解析出密钥明文。
func TestEngineAuthRefNeverContainsSecret(t *testing.T) {
	t.Setenv("CARDEX_TEST_KEY", "env-secret")
	keyFile := filepath.Join(t.TempDir(), "k.key")
	if err := os.WriteFile(keyFile, []byte("file-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	refs := []string{
		engineAuthRef(EngineProfile{AuthEnv: "CARDEX_TEST_KEY"}),
		engineAuthRef(EngineProfile{AuthFile: keyFile}),
		engineAuthRef(EngineProfile{AuthValue: "value-secret"}),
	}
	for _, ref := range refs {
		for _, secret := range []string{"env-secret", "file-secret", "value-secret"} {
			if strings.Contains(ref, secret) {
				t.Fatalf("引用形态泄露了密钥明文: %q", ref)
			}
		}
	}
	if refs[0] != "$CARDEX_TEST_KEY" {
		t.Errorf("auth_env 引用形态应为 $VAR: %q", refs[0])
	}
	if !strings.Contains(refs[1], "cat ") {
		t.Errorf("auth_file 引用形态应为 $(cat ...): %q", refs[1])
	}
}

// ---- 模型解析 ----

func TestResolveEngineModelMapping(t *testing.T) {
	p := EngineProfile{
		Models:       map[string]string{"opus": "k3", "sonnet": "kimi-for-coding"},
		DefaultModel: "kimi-for-coding",
	}
	cases := []struct {
		in       string
		want     string
		wantNote bool
	}{
		{"", "kimi-for-coding", false},     // 空 → default_model
		{"opus", "k3", false},              // 档位命中
		{"claude-opus-4-8", "k3", false},   // 全名归一到档位
		{"claude-fable-5", "k3", true},     // fable 无映射 → 向下回落 opus，必须披露
		{"haiku", "kimi-for-coding", true}, // haiku 无映射 → default_model，披露
		{"k3-256k", "k3-256k", false},      // 供应商原生 ID 直通
		{"glm-5.2", "glm-5.2", false},      // 非本家模型名也直通（用户自担）
	}
	for _, c := range cases {
		got, note := resolveEngineModel(p, c.in)
		if got != c.want {
			t.Errorf("resolveEngineModel(%q): got %q, want %q", c.in, got, c.want)
		}
		if (note != "") != c.wantNote {
			t.Errorf("resolveEngineModel(%q): 披露 note=%q, wantNote=%v", c.in, note, c.wantNote)
		}
	}
	// 映射全空且无 default_model：档位别名原样透传（网关端映射），必须披露。
	bare := EngineProfile{}
	got, note := resolveEngineModel(bare, "sonnet")
	if got != "sonnet" || note == "" {
		t.Fatalf("无映射透传: got %q note %q", got, note)
	}
}

// ---- env 注入 ----

func TestBuildEngineEnvScrubsAndInjects(t *testing.T) {
	// 父进程环境里的全局 ANTHROPIC_*（用户 shell 常见）必须被剥掉——串味即打错端点/花错账。
	t.Setenv("ANTHROPIC_BASE_URL", "https://leak.example.com")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "leak-token")
	t.Setenv("ANTHROPIC_MODEL", "leak-model")
	t.Setenv("ANTHROPIC_DEFAULT_SONNET_MODEL", "leak-sonnet")
	t.Setenv("API_TIMEOUT_MS", "1")
	t.Setenv("CARDEX_TEST_KEY", "sk-test")

	cfg := defaultConfig("claude")
	cfg.ThinkingTokens = 12345
	p := EngineProfile{
		BaseURL:  "https://api.kimi.com/coding/",
		AuthEnv:  "CARDEX_TEST_KEY",
		AuthVar:  "ANTHROPIC_API_KEY",
		Models:   map[string]string{"opus": "k3", "sonnet": "kimi-for-coding", "haiku": "kimi-for-coding"},
		ExtraEnv: map[string]string{"API_TIMEOUT_MS": "3000000", "AAA_FIRST": "1"},
	}
	env, err := buildEngineEnv(cfg, p)
	if err != nil {
		t.Fatal(err)
	}
	get := func(key string) (string, int) {
		val, n := "", 0
		for _, kv := range env {
			if strings.HasPrefix(kv, key+"=") {
				val = kv[len(key)+1:]
				n++
			}
		}
		return val, n
	}
	for key, want := range map[string]string{
		"ANTHROPIC_BASE_URL":             "https://api.kimi.com/coding/",
		"ANTHROPIC_API_KEY":              "sk-test",
		"ANTHROPIC_DEFAULT_OPUS_MODEL":   "k3",
		"ANTHROPIC_DEFAULT_SONNET_MODEL": "kimi-for-coding",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":  "kimi-for-coding",
		"CLAUDE_CODE_SUBAGENT_MODEL":     "kimi-for-coding",
		"API_TIMEOUT_MS":                 "3000000",
		"MAX_THINKING_TOKENS":            "12345",
	} {
		if val, n := get(key); val != want || n != 1 {
			t.Errorf("%s: got %q（出现 %d 次）, want %q（恰 1 次）", key, val, n, want)
		}
	}
	// 剥净断言：继承的泄露值与未映射变量一律不在。
	if val, n := get("ANTHROPIC_AUTH_TOKEN"); n != 0 {
		t.Errorf("ANTHROPIC_AUTH_TOKEN 应被剥净（auth_var 是 API_KEY）: got %q", val)
	}
	if val, n := get("ANTHROPIC_MODEL"); n != 0 {
		t.Errorf("ANTHROPIC_MODEL 应被剥净: got %q", val)
	}
}

// invokeEngine 认证未就绪的错误必须落 auth 类（held 等人工），不许烧 attempts 空转——
// 凭据不会因重试自动出现。
func TestEngineAuthErrorClassifiesAsAuth(t *testing.T) {
	msg := "invalid api key: 引擎 kimi 认证未就绪——auth_env 指定的环境变量 KIMI_API_KEY 为空"
	if cls := classifyFailure(msg, "", nil, errors.New(msg)); cls != failureAuth {
		t.Fatalf("引擎认证错误应归 auth 类, got %s", cls)
	}
}

// ---- 限额判据 ----

func TestIsLimitHitEngineKimiPhrasings(t *testing.T) {
	mk := func(text string) *claudeResult {
		return &claudeResult{Type: "result", IsError: true, Result: text}
	}
	cases := []struct {
		name string
		res  *claudeResult
		want bool
	}{
		// Kimi 官方错误参考里的四种限额措辞（2026-08-02 核实）——全含 "usage limit"，limitRe 覆盖。
		{"Kimi 429 五小时窗", mk("API Error: 429 You've reached your usage limit for this period. Your quota will be refreshed in the next period."), true},
		{"Kimi 429 月度", mk("API Error: 429 You've reached kimi monthly usage limit for this billing cycle."), true},
		// 403 计费周期：**必须**判限额——否则 403 被失败分类吃成 permission→held，无人值守断档。
		{"Kimi 403 计费周期判限额而非权限", mk("API Error: 403 You've reached your usage limit for this billing cycle. Your quota will be refreshed in the next cycle."), true},
		// 通用配额措辞（GLM/MiniMax 未公开格式，engineQuotaRe 保守收网）。
		{"quota exceeded", mk("API Error: 429 quota exceeded for this plan"), true},
		{"中文额度不足", mk(`{"error":{"code":"1113","message":"余额不足，请充值后重试"}}`), true},
		// 瞬时拥堵不是限额：挂半小时冷却是把小病治成大病，留给 transientRe 退避。
		{"Kimi 引擎过载不判限额", mk("API Error: 429 The engine is currently overloaded, please try again later"), false},
		{"Kimi 并发超限不判限额", mk("API Error: 429 We're receiving too many requests at the moment. Please wait a moment and try again."), false},
		{"成功结果含限额字样不误判", &claudeResult{Type: "result", IsError: false, Result: "本工具围绕 usage limit 做调度"}, false},
		// transcript 来源的 prose 不参与扫描（与 isLimitHit 同一纪律）。
		{"transcript prose 不误判", &claudeResult{Type: "result", IsError: true, ResultFromTranscript: true,
			Result: "审查引用: quota exceeded 与 usage limit 措辞"}, false},
	}
	for _, c := range cases {
		if got := isLimitHitEngine(c.res, ""); got != c.want {
			t.Errorf("%s: isLimitHitEngine=%v, want %v", c.name, got, c.want)
		}
	}
}

func TestLimitHitForRunnerRoutesEngine(t *testing.T) {
	// 差异化输入：engineQuotaRe 独有措辞（limitRe 不含"配额已用尽"）——引擎判据 true，
	// claude/codex 判据 false，路由拿错立刻红。
	res := &claudeResult{Type: "result", IsError: true, Result: "配额已用尽"}
	if !limitHitForRunner("kimi", false, nil, res, "") {
		t.Fatal("via=引擎名应走 isLimitHitEngine（engineQuotaRe 命中）")
	}
	if limitHitForRunner("", false, nil, res, "") {
		t.Fatal("via=\"\"（claude）不应吃 engineQuotaRe 措辞")
	}
	if limitHitForRunner("codex", false, nil, res, "") {
		t.Fatal("via=codex 不应吃 engineQuotaRe 措辞")
	}
	// 远端组合不受 via 影响，仍走旧映射（remoteUsesClaude 决定）。
	if limitHitForRunner("kimi", true, &Task{Model: "opus"}, res, "") {
		t.Fatal("remote=true 时应沿用旧路由（远端 claude 判据），不走引擎判据")
	}
}

func TestEngineResetEpochFallbacks(t *testing.T) {
	cfg := &Config{LimitFallbackMin: 30, CooldownMarginSec: 90}
	now := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)

	// 档案级回退优先于全局。
	p := EngineProfile{LimitFallbackMin: 45}
	got := engineResetEpoch("You've reached your usage limit for this period.", "usage limit for this period", cfg, p, now)
	if want := now.Add(45*time.Minute).Unix() + 90; got != want {
		t.Fatalf("档案级回退: got %d want %d", got, want)
	}
	// 档案未配 → 继承全局。
	got = engineResetEpoch("usage limit", "usage limit", cfg, EngineProfile{}, now)
	if want := now.Add(30*time.Minute).Unix() + 90; got != want {
		t.Fatalf("继承全局回退: got %d want %d", got, want)
	}
	// 月度/计费周期措辞：抬到 ≥360min——月度耗尽半小时一撞是纯浪费。
	got = engineResetEpoch("You've reached kimi monthly usage limit for this billing cycle.",
		"monthly usage limit for this billing cycle", cfg, p, now)
	if want := now.Add(360*time.Minute).Unix() + 90; got != want {
		t.Fatalf("月度限额应抬到 360min: got %d want %d", got, want)
	}
	// 措辞里带精确重置时间戳时瀑布解析仍优先（不被回退盖掉）。
	epoch := now.Add(2 * time.Hour).Unix()
	got = engineResetEpoch("usage limit reached|"+strconv.FormatInt(epoch, 10), "usage limit", cfg, p, now)
	if got != epoch+90 {
		t.Fatalf("epoch 形态应优先: got %d want %d", got, epoch+90)
	}
}

// ---- 冷却分账 ----

func TestEngineCooldownRoundtrip(t *testing.T) {
	root := testRoot(t)
	now := time.Now()
	if loadEngineCooldown(root, "kimi").active(now) {
		t.Fatal("未设置时不应活跃")
	}
	setEngineCooldown(root, "kimi", now.Add(time.Hour).Unix(), "test")
	if !loadEngineCooldown(root, "kimi").active(now) {
		t.Fatal("设置后应活跃")
	}
	if loadEngineCooldown(root, "glm-cn").active(now) {
		t.Fatal("引擎冷却互不串账")
	}
	if loadCooldown(root).active(now) {
		t.Fatal("引擎冷却不得写 claude 全局冷却")
	}
	clearEngineCooldown(root, "kimi")
	if loadEngineCooldown(root, "kimi").active(now) {
		t.Fatal("清除后不应活跃")
	}
}

// ---- 派发护栏 ----

func TestEngineDivertGuards(t *testing.T) {
	root := testRoot(t)
	now := time.Now()
	cfg := defaultConfig("claude")
	cfg.Engines = map[string]EngineProfile{"kimi": {BaseURL: "https://api.kimi.com/coding/", AuthEnv: "K"}}

	ok := &Task{Type: typeSequence, Prompts: []string{"p"}, Model: "sonnet"}
	if !engineDivertOK(root, cfg, "kimi", ok, now) {
		t.Fatal("单步无会话普通卡应可改道")
	}
	cases := []struct {
		name string
		t    *Task
	}{
		{"带会话不可断", &Task{Prompts: []string{"p"}, SessionID: "s"}},
		{"多步非 fresh 不可断", &Task{Prompts: []string{"a", "b"}}},
		{"no_fallback 模型（fable 钉定）", &Task{Prompts: []string{"p"}, Model: "claude-fable-5"}},
		{"交叉卡引擎身份不偷换", &Task{Prompts: []string{"p"}, Type: typeCrossCheck}},
		{"复审位质量地板", &Task{Prompts: []string{"p"}, Type: typeReview}},
		{"交叉裁决位 XRole=C", &Task{Prompts: []string{"p"}, XRole: "C"}},
	}
	for _, c := range cases {
		if engineDivertOK(root, cfg, "kimi", c.t, now) {
			t.Errorf("%s: 不应允许改道", c.name)
		}
	}
	// 引擎自己在冷却：不可改道过去（撞了也是白撞，还刷脏事件账本）。
	setEngineCooldown(root, "kimi", now.Add(time.Hour).Unix(), "test")
	if engineDivertOK(root, cfg, "kimi", ok, now) {
		t.Fatal("引擎冷却中不应被选为改道出路")
	}
	// 档案不存在。
	if engineDivertOK(root, cfg, "ghost", ok, now) {
		t.Fatal("未配置引擎不应可改道")
	}
}

func TestPickDivertRunnerOrder(t *testing.T) {
	root := testRoot(t)
	now := time.Now()
	cfg := defaultConfig("claude")
	cfg.Engines = map[string]EngineProfile{
		"kimi":   {BaseURL: "https://api.kimi.com/coding/"},
		"glm-cn": {BaseURL: "https://open.bigmodel.cn/api/anthropic"},
	}
	task := &Task{Type: typeSequence, Prompts: []string{"p"}, Model: "sonnet"}

	// 默认链只有 codex，codex 不可用（无 bin）→ 无出路（与旧行为一致）。
	if via := pickDivertRunner(root, cfg, task, now); via != "" {
		t.Fatalf("默认链 codex 不可用应无出路, got %q", via)
	}
	// 链上引擎按序取第一个可用。
	cfg.FallbackOrder = []string{"codex", "kimi", "glm-cn"}
	if via := pickDivertRunner(root, cfg, task, now); via != "kimi" {
		t.Fatalf("codex 不可用应取 kimi, got %q", via)
	}
	// kimi 冷却中 → 顺延 glm-cn。
	setEngineCooldown(root, "kimi", now.Add(time.Hour).Unix(), "test")
	if via := pickDivertRunner(root, cfg, task, now); via != "glm-cn" {
		t.Fatalf("kimi 冷却应顺延 glm-cn, got %q", via)
	}
	// codex 可用时（bin+开关）按序优先。
	cfg.CodexBin = "/usr/bin/true"
	cfg.CodexFallback = true
	if via := pickDivertRunner(root, cfg, task, now); via != "codex" {
		t.Fatalf("codex 恢复后链首优先, got %q", via)
	}
	// 质量地板对整条链生效：复审卡在任何引擎上都不许降级。
	review := &Task{Type: typeReview, Prompts: []string{"p"}}
	if via := pickDivertRunner(root, cfg, review, now); via != "" {
		t.Fatalf("复审卡不许改道任何引擎, got %q", via)
	}
}

func TestPinnedEngineReady(t *testing.T) {
	root := testRoot(t)
	now := time.Now()
	cfg := defaultConfig("claude")
	cfg.Engines = map[string]EngineProfile{"kimi": {BaseURL: "https://api.kimi.com/coding/"}}
	if !pinnedEngineReady(root, cfg, "kimi", now) {
		t.Fatal("已配引擎无冷却应就绪")
	}
	if pinnedEngineReady(root, cfg, "ghost", now) {
		t.Fatal("未配引擎不就绪（绝不 fail-open 回 claude）")
	}
	setEngineCooldown(root, "kimi", now.Add(time.Hour).Unix(), "t")
	if pinnedEngineReady(root, cfg, "kimi", now) {
		t.Fatal("冷却中不就绪")
	}
}

// ---- 看板披露（P2）----

// 统一分级表落进 modelTier：档位来自 AA 智能指数 2026 快照（2026-08-02）的标准线判定，
// 见 engines.go 预设注释与 docs/guide.md。表改动（如 glm-5.2 升档）必须连这里一起改——
// 分级是委托人指定的交付物，不许静默漂移。无自定义表（默认配置）时行为=纯标准线。
func TestModelTierUnifiedStandard(t *testing.T) {
	cfg := defaultConfig("claude")
	cases := map[string]string{
		"k3": "高", "k3-256k": "高", "kimi-k3": "高", // AA 57.1，opus 档
		"glm-5.2": "中", "kimi-for-coding": "中", // 51.1 / 服务档
		"MiniMax-M3": "轻", "kimi-k2.7-code": "轻", "mimo-v2.5-pro": "轻",
		"glm-4.5-air": "轻", "glm-4.7:cloud": "轻", "deepseek-v4-flash": "轻", "qwen3.7-max": "轻",
		// 既有映射不许被新表挤掉。
		"claude-fable-5": "旗舰", "gpt-5.6-sol": "旗舰", "opus": "高", "sonnet": "中", "haiku": "轻",
	}
	for model, want := range cases {
		if got := modelTier(cfg, model); got != want {
			t.Errorf("modelTier(%q) = %q, want %q（统一标准线表漂移）", model, got, want)
		}
	}
}

// 自定义分级表（model_tiers）：无更强模型的机队按牌面定档——手里最强的模型就是自己的
// fable 档。自定义恒优先于内置标准线；前缀匹配盖住变体后缀；未列条目回落标准线。
func TestModelTiersCustomOverride(t *testing.T) {
	cfg := defaultConfig("claude")
	cfg.ModelTiers = map[string]string{
		"glm-5.2": "fable",  // GLM-only 机队：5.2 顶最强档（标准线里它是 sonnet 档）
		"glm-4.7": "sonnet", // 前缀键：应盖住 glm-4.7:cloud 变体
	}
	if got := modelTier(cfg, "glm-5.2"); got != "旗舰" {
		t.Fatalf("自定义表应优先于标准线: modelTier(glm-5.2)=%q, want 旗舰", got)
	}
	if got := modelTier(cfg, "GLM-5.2"); got != "旗舰" {
		t.Fatalf("匹配按小写归一: modelTier(GLM-5.2)=%q, want 旗舰", got)
	}
	if got := modelTier(cfg, "glm-4.7:cloud"); got != "中" {
		t.Fatalf("前缀键应盖住变体后缀: modelTier(glm-4.7:cloud)=%q, want 中", got)
	}
	// 精确条目优先于更短的前缀条目。
	cfg.ModelTiers["glm-4.7:cloud"] = "haiku"
	if got := modelTier(cfg, "glm-4.7:cloud"); got != "轻" {
		t.Fatalf("精确匹配应优先于前缀: got %q, want 轻", got)
	}
	// 未列条目回落内置标准线，不受自定义表存在影响。
	if got := modelTier(cfg, "k3"); got != "高" {
		t.Fatalf("未列条目应回落标准线: modelTier(k3)=%q, want 高", got)
	}
	if got := modelTier(cfg, "闻所未闻模型"); got != "未知" {
		t.Fatalf("两级都判不出显示未知: got %q", got)
	}
}

// model_tiers 坏值载入即拒（fail fast）——档位写错静默当"未知"会让自定义分级悄悄失效。
func TestModelTiersValidation(t *testing.T) {
	bad := []map[string]string{
		{"glm-5.2": "flagship"}, // 不是档位关键字
		{"GLM-5.2": "fable"},    // 键必须全小写（防影子条目）
		{"": "fable"},           // 空键
	}
	for i, mt := range bad {
		cfg := defaultConfig("claude")
		cfg.ModelTiers = mt
		if err := validateEngines(cfg); err == nil {
			t.Errorf("case %d (%v): 应校验失败", i, mt)
		}
	}
	ok := defaultConfig("claude")
	ok.ModelTiers = map[string]string{"glm-5.2": "fable", "minimax-m3": "sonnet"}
	if err := validateEngines(ok); err != nil {
		t.Fatalf("合法自定义表不应报错: %v", err)
	}
}

// 引擎展示档位推导：显式 tier 优先；缺省时从最高档映射模型经自定义表/标准线推导——
// 多模型订阅换映射即换档，不须手动同步 tier 字段。
func TestEngineDisplayTierDerived(t *testing.T) {
	cfg := defaultConfig("claude")
	// 显式 tier 恒优先。
	if got := engineDisplayTier(cfg, EngineProfile{Tier: "opus", Models: map[string]string{"opus": "glm-4.5-air"}}); got != "opus" {
		t.Fatalf("显式 tier 应优先: got %q", got)
	}
	// 缺省：从最高档映射模型推导（标准线：k3→opus）。
	if got := engineDisplayTier(cfg, EngineProfile{Models: map[string]string{"opus": "k3", "haiku": "kimi-for-coding"}}); got != "opus" {
		t.Fatalf("应从最高档映射模型推导: got %q", got)
	}
	// 自定义表参与推导：GLM-only 机队把 glm-5.2 定为 fable 后，引擎档位随之。
	cfg.ModelTiers = map[string]string{"glm-5.2": "fable"}
	if got := engineDisplayTier(cfg, EngineProfile{Models: map[string]string{"opus": "glm-5.2"}}); got != "fable" {
		t.Fatalf("自定义表应参与引擎档位推导: got %q", got)
	}
	// 全推不出返回空（展示层显示 "-"）。
	if got := engineDisplayTier(cfg, EngineProfile{}); got != "" {
		t.Fatalf("无据可推应返回空: got %q", got)
	}
}

// 引擎卡的看板模型还原：卡面 Model 是档位别名时，看板必须显示映射后的供应商模型——
// 展示与实际执行分叉是 TestEffectiveModelUsesMergedTypeDefault 同类的静默事故。
func TestEffectiveModelResolvesEngineMapping(t *testing.T) {
	cfg := defaultConfig("claude")
	cfg.Engines = map[string]EngineProfile{
		"kimi": {BaseURL: "https://api.kimi.com/coding/", Models: map[string]string{"sonnet": "kimi-for-coding"}},
	}
	model, source := effectiveModel(cfg, &Task{Model: "sonnet", PreferRunner: "kimi"})
	if model != "kimi-for-coding" || source != "engine:kimi" {
		t.Fatalf("钉定引擎卡应显示映射后模型: got (%q,%q)", model, source)
	}
	// 改道卡（Runner 记了引擎、无钉定）同样按引擎还原。
	model, source = effectiveModel(cfg, &Task{Model: "sonnet", Runner: "kimi"})
	if model != "kimi-for-coding" || source != "engine:kimi" {
		t.Fatalf("改道引擎卡应显示映射后模型: got (%q,%q)", model, source)
	}
	// 非引擎卡不受影响（回归护栏）。
	model, source = effectiveModel(cfg, &Task{Model: "sonnet"})
	if model != "sonnet" || source != "task" {
		t.Fatalf("普通 claude 卡不应被引擎路径吃掉: got (%q,%q)", model, source)
	}
	// Runner="remote:host" 不得被误认成引擎（成员资格判定，非字符串猜测）。
	model, source = effectiveModel(cfg, &Task{Model: "opus", Runner: "remote:winbox"})
	if source == "engine:kimi" {
		t.Fatalf("remote 标签不得误入引擎路径: got (%q,%q)", model, source)
	}
}

// 额度条披露行：冷却状态与本地账计数两个事实，无引擎配置时不占地。
func TestEngineQuotaRowsDisclosure(t *testing.T) {
	root := testRoot(t)
	now := time.Now()
	cfg := defaultConfig("claude")
	if rows := engineQuotaRows(root, cfg, now); rows != nil {
		t.Fatalf("无引擎配置时应返回 nil, got %v", rows)
	}
	cfg.Engines = map[string]EngineProfile{
		"kimi":   {BaseURL: "https://x", Tier: "opus"},
		"glm-cn": {BaseURL: "https://y", Tier: "sonnet"},
	}
	setEngineCooldown(root, "kimi", now.Add(time.Hour).Unix(), "test")
	rows := engineQuotaRows(root, cfg, now)
	if len(rows) != 2 {
		t.Fatalf("应有 2 行, got %d", len(rows))
	}
	// sortedEngineNames 保证顺序确定：glm-cn 在前。
	if rows[0].Name != "glm-cn" || rows[0].CoolingUntil != 0 {
		t.Errorf("glm-cn 行应就绪: %+v", rows[0])
	}
	if rows[1].Name != "kimi" || rows[1].CoolingUntil == 0 || rows[1].Tier != "opus" {
		t.Errorf("kimi 行应带冷却时刻与档位: %+v", rows[1])
	}
}

// ---- runTaskVia 集成（fake claude 二进制）----

// fakeEngineClaude 写一个假 claude：把 env 与 argv 各 dump 一份，stdout 输出指定 JSON 文件内容。
func fakeEngineClaude(t *testing.T, resultJSON string, exitCode int) (bin, envDump, argsDump string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "claude")
	envDump = filepath.Join(dir, "env.dump")
	argsDump = filepath.Join(dir, "args.dump")
	payload := filepath.Join(dir, "result.json")
	if err := os.WriteFile(payload, []byte(resultJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"env > " + envDump + "\n" +
		"printf '%s\\n' \"$@\" > " + argsDump + "\n" +
		"cat " + payload + "\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, envDump, argsDump
}

const kimiLimitJSON = `{"type":"result","subtype":"error_during_execution","is_error":true,` +
	`"result":"API Error: 429 You've reached your usage limit for this period. Your quota will be refreshed in the next period.","num_turns":1}`

const engineOKJSON = `{"type":"result","subtype":"success","is_error":false,"result":"done",` +
	`"session_id":"sess-kimi-1","num_turns":2,"total_cost_usd":0.01,` +
	`"usage":{"input_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":50}}`

func engineTestConfig(t *testing.T, bin string) *Config {
	t.Helper()
	cfg := defaultConfig(bin)
	cfg.StepTimeoutMin = 1
	cfg.LimitFallbackMin = 30
	cfg.CooldownMarginSec = 0
	cfg.MaxAttempts = 3
	t.Setenv("CARDEX_TEST_ENGINE_KEY", "sk-test-123")
	cfg.Engines = map[string]EngineProfile{
		"kimi": {
			BaseURL: "https://api.kimi.com/coding/",
			AuthEnv: "CARDEX_TEST_ENGINE_KEY",
			AuthVar: "ANTHROPIC_API_KEY",
			Models:  map[string]string{"opus": "k3", "sonnet": "kimi-for-coding", "haiku": "kimi-for-coding"},
		},
	}
	return cfg
}

// 引擎限额：只写 cooldown-kimi.json，绝不写 claude 全局冷却；不烧 attempts；到点可重派。
func TestRunTaskEngineLimitPausesOwnCooldownOnly(t *testing.T) {
	root := testRoot(t)
	bin, _, _ := fakeEngineClaude(t, kimiLimitJSON, 1)
	cfg := engineTestConfig(t, bin)

	task := newTask(root, cfg, typeSequence, "kimi limit", t.TempDir(), []string{"p"}, 1)
	task.PreferRunner = "kimi"
	task.Model = "sonnet"
	task.Attempts = 1
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	if err := runTaskVia(context.Background(), root, cfg, task, "kimi"); err != nil {
		t.Fatal(err)
	}
	got, err := loadTask(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusLimitPaused {
		t.Fatalf("引擎限额应挂 limit_paused, got %q last_error=%q", got.Status, got.LastError)
	}
	if got.Attempts != 1 {
		t.Fatalf("限额不烧 attempts: got %d", got.Attempts)
	}
	if !strings.HasPrefix(got.LastError, "引擎 kimi 用量限额: ") {
		t.Fatalf("应记引擎专属限额原因, got %q", got.LastError)
	}
	if !loadEngineCooldown(root, "kimi").active(time.Now()) {
		t.Fatal("应写入 cooldown-kimi.json")
	}
	if _, err := os.Stat(cooldownPath(root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("引擎限额绝不写 claude 全局 cooldown, stat err=%v", err)
	}
	if !eligible(got, time.Unix(got.ResumeAtEpoch, 0)) {
		t.Fatal("resume_at 到点后应可重派")
	}
	if got.Runner != "kimi" {
		t.Fatalf("Runner 标签应为引擎名, got %q", got.Runner)
	}
}

// 引擎成功：账本打 engine 标（不占 claude 红线）、钉定卡回写会话、清自家冷却、不动 claude 冷却。
func TestRunTaskEngineSuccessLedgerSessionAndCooldown(t *testing.T) {
	root := testRoot(t)
	bin, envDump, argsDump := fakeEngineClaude(t, engineOKJSON, 0)
	cfg := engineTestConfig(t, bin)

	// 预置：kimi 自己的冷却（应被成功清掉）+ claude 全局冷却（引擎成功不得碰）。
	setEngineCooldown(root, "kimi", time.Now().Add(time.Hour).Unix(), "stale")
	setCooldown(root, time.Now().Add(time.Hour).Unix(), "claude 冷却中")

	task := newTask(root, cfg, typeSequence, "kimi ok", t.TempDir(), []string{"p"}, 1)
	task.PreferRunner = "kimi"
	task.Model = "sonnet"
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	if err := runTaskVia(context.Background(), root, cfg, task, "kimi"); err != nil {
		t.Fatal(err)
	}
	got, err := loadTask(root, task.ID)
	if err == nil && got.Status != statusDone {
		t.Fatalf("应完成, got %q last_error=%q", got.Status, got.LastError)
	}
	if err != nil {
		// 单步 done 后可能被 postComplete 留在 tasks/（无归档路径），loadTask 失败才查 archive。
		if got2, err2 := findTaskAnywhere(root, task.ID); err2 != nil || got2.Status != statusDone {
			t.Fatalf("应完成: %v / %+v", err2, got2)
		}
		got, _ = findTaskAnywhere(root, task.ID)
	}
	if got.SessionID != "sess-kimi-1" {
		t.Fatalf("钉定引擎卡应回写会话（引擎有会话语义）, got %q", got.SessionID)
	}
	if got.Runner != "kimi" {
		t.Fatalf("Runner=kimi, got %q", got.Runner)
	}
	// 账本：engine 标记录不进 claude 红线口径，进引擎披露口径。
	total, _ := queueWindowSpent(root, time.Now())
	if total != 0 {
		t.Fatalf("引擎调用不得占 claude 红线预算, queueWindowSpent=%v", total)
	}
	byEngine := engineWindowSpent(root, time.Now())
	if byEngine["kimi"] <= 0 {
		t.Fatalf("引擎本地账计数应有 kimi 记录, got %v", byEngine)
	}
	// 冷却分账：自家清掉、claude 的原样在。
	if loadEngineCooldown(root, "kimi").active(time.Now()) {
		t.Fatal("引擎成功应清自家冷却")
	}
	if !loadCooldown(root).active(time.Now()) {
		t.Fatal("引擎成功不得清 claude 全局冷却")
	}
	// env 注入落地断言（integration 面）。
	envBytes, err := os.ReadFile(envDump)
	if err != nil {
		t.Fatal(err)
	}
	envs := string(envBytes)
	for _, want := range []string{
		"ANTHROPIC_BASE_URL=https://api.kimi.com/coding/",
		"ANTHROPIC_API_KEY=sk-test-123",
		"ANTHROPIC_DEFAULT_OPUS_MODEL=k3",
	} {
		if !strings.Contains(envs, want+"\n") {
			t.Errorf("子进程环境缺 %s", want)
		}
	}
	argsBytes, err := os.ReadFile(argsDump)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(argsBytes), "kimi-for-coding") {
		t.Errorf("argv 应带映射后的 --model kimi-for-coding, got %q", string(argsBytes))
	}
}

// 改道（非钉定）引擎运行不得回写会话——否则冷却结束后卡回 claude 重试会 --resume 引擎会话，
// 跨引擎会话漂移。
func TestRunTaskEngineDivertedDoesNotAdoptSession(t *testing.T) {
	root := testRoot(t)
	bin, _, _ := fakeEngineClaude(t, engineOKJSON, 0)
	cfg := engineTestConfig(t, bin)

	task := newTask(root, cfg, typeSequence, "diverted", t.TempDir(), []string{"p"}, 1)
	task.Model = "sonnet" // PreferRunner 留空 = 改道卡
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	if err := runTaskVia(context.Background(), root, cfg, task, "kimi"); err != nil {
		t.Fatal(err)
	}
	got, err := findTaskAnywhere(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "" {
		t.Fatalf("改道卡不得回写引擎会话（防跨引擎 --resume 漂移）, got %q", got.SessionID)
	}
}
