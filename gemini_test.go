package main

// gemini_test.go —— Gemini CLI 备用执行器的行为钉桩（设计规格
// docs/2026-08-03-gemini-executor-design.md）。测试策略与 codex 同族：假 gemini 脚本
// （argv/stdin 捕获 + 注入各类真实错误文案），不跑真 CLI。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ---- 模型解析 ----

func TestResolveGeminiModelSlotFallback(t *testing.T) {
	cfg := defaultConfig("")

	// 优先序 1：交叉冻结恒最高。
	m, note := resolveGeminiModel(cfg, &Task{XGeminiModel: "frozen-pro", GeminiModel: "flash", Model: "sonnet"})
	if m != "frozen-pro" || note != "" {
		t.Fatalf("XGeminiModel 应恒最高: got %q note %q", m, note)
	}
	// 优先序 2：卡级钉定。
	m, _ = resolveGeminiModel(cfg, &Task{GeminiModel: "flash-lite", Model: "opus"})
	if m != "flash-lite" {
		t.Fatalf("卡级 GeminiModel 应优先于槽映射: got %q", m)
	}
	// 优先序 3：内置槽映射（fable/opus→pro、sonnet→flash、haiku→flash-lite）。
	for model, want := range map[string]string{
		"claude-fable-5": "pro", "fable": "pro", "opus": "pro",
		"claude-sonnet-5": "flash", "sonnet": "flash", "haiku": "flash-lite",
	} {
		if m, note = resolveGeminiModel(cfg, &Task{Model: model}); m != want || note != "" {
			t.Fatalf("槽映射 %s → %s（note 应空），got %q note %q", model, want, m, note)
		}
	}
	// 缺槽向下档回落且必须披露。
	cfg.GeminiModels = map[string]string{"haiku": "flash-lite"}
	if m, note = resolveGeminiModel(cfg, &Task{Model: "sonnet"}); m != "flash-lite" || note == "" {
		t.Fatalf("sonnet 槽缺失应回落 haiku 档并披露: got %q note %q", m, note)
	}
	// 整表覆写语义：非空表即整表生效，全缺槽落 gemini_model。
	cfg.GeminiModels = map[string]string{"fable": "pro"}
	cfg.GeminiModel = "flash"
	if m, note = resolveGeminiModel(cfg, &Task{Model: "haiku"}); m != "flash" || note == "" {
		t.Fatalf("槽全缺应落 gemini_model 并披露: got %q note %q", m, note)
	}
	// 终兜底：连 gemini_model 都没有 → 内置 pro，永不落空（空=auto 路由，已否决）。
	cfg.GeminiModels = nil
	cfg.GeminiModel = ""
	if m, note = resolveGeminiModel(cfg, &Task{}); m != "pro" || note == "" {
		t.Fatalf("终兜底应为内置 pro 且披露: got %q note %q", m, note)
	}
}

func TestValidateGeminiRejectsBadConfigs(t *testing.T) {
	cfg := defaultConfig("")
	cfg.GeminiModels = map[string]string{"turbo": "pro"}
	if err := validateGemini(cfg); err == nil {
		t.Fatal("gemini_models 非档位键应载入即拒")
	}
	cfg.GeminiModels = map[string]string{"opus": "pro"}
	cfg.GeminiApprovalMode = "sudo"
	if err := validateGemini(cfg); err == nil {
		t.Fatal("非法 gemini_approval_mode 应载入即拒")
	}
	cfg.GeminiApprovalMode = "auto_edit"
	if err := validateGemini(cfg); err != nil {
		t.Fatalf("合法配置不应报错: %v", err)
	}
	// "gemini" 是引擎名保留字（与 Runner 标签语义冲突）。
	e := defaultConfig("")
	e.Engines = map[string]EngineProfile{"gemini": {BaseURL: "https://x"}}
	if err := validateEngines(e); err == nil {
		t.Fatal("引擎名 gemini 应被保留字拒绝")
	}
	// 新派发面已退休 Gemini；历史配置字段仍可读取，但不得继续把它放进 fallback_order。
	f := defaultConfig("")
	f.FallbackOrder = []string{"codex", "gemini"}
	if err := validateEngines(f); err == nil {
		t.Fatal("fallback_order 的 gemini 退休项必须拒绝")
	}
}

// ---- 审批模式（plan 只读是唯一硬护栏）----

func TestGeminiApprovalModePlanForcedOnNonSequence(t *testing.T) {
	cfg := defaultConfig("")
	for _, typ := range []string{typeReview, typeCoordinate, typeAssembly, typeProgressPull, typeCrossCheck} {
		if got := geminiApprovalModeFor(cfg, &Task{Type: typ}); got != "plan" {
			t.Fatalf("非 sequence 卡 %s 应强制 plan, got %q", typ, got)
		}
	}
	if got := geminiApprovalModeFor(cfg, &Task{Type: typeSequence}); got != "yolo" {
		t.Fatalf("sequence 默认应 yolo, got %q", got)
	}
	cfg.GeminiApprovalMode = "auto_edit"
	if got := geminiApprovalModeFor(cfg, &Task{Type: typeSequence}); got != "auto_edit" {
		t.Fatalf("sequence 应吃 config 覆写, got %q", got)
	}
	if got := geminiApprovalModeFor(cfg, &Task{Type: typeReview}); got != "plan" {
		t.Fatalf("复审卡不受 config 覆写影响，恒 plan, got %q", got)
	}
}

// ---- 限额 / 认证判据 ----

func TestGeminiSuspendKindDailyAuthTransient(t *testing.T) {
	mk := func(errMsg string) (*claudeResult, string) {
		return &claudeResult{IsError: true, Subtype: "gemini_error", Result: errMsg}, "\n" + errMsg
	}
	// 官方终止态文案（googleQuotaErrors.ts）→ limit。
	res, combined := mk("Error: You have exhausted your daily quota on this model.")
	if k := geminiSuspendKind(res, combined); k != "limit" {
		t.Fatalf("每日配额耗尽应判 limit, got %q", k)
	}
	res, combined = mk(`status 429: quotaId "GenerateRequestsPerDayPerProjectPerModel-FreeTier", limit: 0`)
	if k := geminiSuspendKind(res, combined); k != "limit" {
		t.Fatalf("PerDay quotaId 应判 limit, got %q", k)
	}
	// 认证/资格错误（2026-08-03 实测形态）→ auth。
	res, combined = mk("Error authenticating: IneligibleTierError: This client is no longer supported for Gemini Code Assist for individuals. reasonCode: UNSUPPORTED_CLIENT")
	if k := geminiSuspendKind(res, combined); k != "auth" {
		t.Fatalf("IneligibleTierError 应判 auth, got %q", k)
	}
	res, combined = mk("400 INVALID_ARGUMENT: API key not valid. Please pass a valid API key.")
	if k := geminiSuspendKind(res, combined); k != "auth" {
		t.Fatalf("API key 无效应判 auth, got %q", k)
	}
	// 每分钟限流/裸 429 → 不挂车道（留给 transientRe 退避重试）。
	res, combined = mk("429 RESOURCE_EXHAUSTED: Quota exceeded for metric generate_requests_per_minute. Please retry in 27s")
	if k := geminiSuspendKind(res, combined); k != "" {
		t.Fatalf("每分钟限流不应挂车道, got %q", k)
	}
	if !transientRe.MatchString("429 RESOURCE_EXHAUSTED: rate limit") {
		t.Fatal("每分钟限流应由 transientRe 承接退避")
	}
	// 成功结果携带限额字面量（自审本仓 prose）→ 恒不挂。
	ok := &claudeResult{IsError: false, Result: "本仓处理 daily quota 措辞的正则是 geminiDailyRe"}
	if k := geminiSuspendKind(ok, "{\"response\":\"...daily quota...\"}\n"); k != "" {
		t.Fatalf("成功结果的 prose 不得触发挂起, got %q", k)
	}
	// transcript 来源的 Result 不进扫描面。
	tr := &claudeResult{IsError: true, Result: "复审引用: You have exhausted your daily quota", ResultFromTranscript: true}
	if k := geminiSuspendKind(tr, "{\"response\":\"x\"}\n干净的 stderr"); k != "" {
		t.Fatalf("transcript 来源的 Result 不得触发挂起, got %q", k)
	}
}

func TestGeminiResetEpoch(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cfg := defaultConfig("")
	cfg.LimitFallbackMin = 30
	cfg.CooldownMarginSec = 0 // 余量语义由 parseResetEpoch 既有测试钉住，此处归零便于断言绝对值

	// retryDelay 形态（RetryInfo detail）优先。
	got := geminiResetEpoch("x", `"retryDelay":"27s"`, cfg, now)
	if got != now.Add(27*time.Second).Unix() {
		t.Fatalf("retryDelay 27s 应精确解析, got %d want %d", got, now.Add(27*time.Second).Unix())
	}
	got = geminiResetEpoch("x", "Please retry in 39.5s", cfg, now)
	if got != now.Add(time.Duration(39.5*float64(time.Second))).Unix() {
		t.Fatalf("retry in 39.5s 应解析, got %d", got)
	}
	// 无任何时间戳：回退抬到 ≥360min（每日配额无公开重置时刻，半小时一撞是纯浪费）。
	got = geminiResetEpoch("You have exhausted your daily quota on this model.", "同左", cfg, now)
	if got != now.Add(360*time.Minute).Unix() {
		t.Fatalf("每日配额回退应为 360min, got %d want %d", got, now.Add(360*time.Minute).Unix())
	}
	// 用户配了更保守的全局回退（>360）时尊重之。
	cfg.LimitFallbackMin = 720
	if got = geminiResetEpoch("daily quota", "同左", cfg, now); got != now.Add(720*time.Minute).Unix() {
		t.Fatalf("更大的 limit_fallback_min 应被尊重, got %d", got)
	}
}

// ---- 派发护栏 ----

func TestGeminiDivertOKGuards(t *testing.T) {
	root := testRoot(t)
	now := time.Now()
	cfg := defaultConfig("")
	cfg.GeminiBin = "/usr/bin/true"
	base := func() *Task {
		return &Task{ID: "g1", Type: typeSequence, Model: "sonnet", Prompts: []string{"p"}}
	}
	if !geminiDivertOK(root, cfg, base(), now) {
		t.Fatal("基线卡应可改道 gemini")
	}
	// 闸①：gemini_bin 缺失。
	nc := *cfg
	nc.GeminiBin = ""
	if geminiDivertOK(root, &nc, base(), now) {
		t.Fatal("gemini_bin 缺失不得改道")
	}
	// 闸②：codexEligible 形状（有会话的多步卡不可断）。
	withSession := base()
	withSession.SessionID = "sess"
	if geminiDivertOK(root, cfg, withSession, now) {
		t.Fatal("带会话的卡不得改道（会话断链）")
	}
	// 闸③：no_fallback_models 钉定模型。
	pinned := base()
	pinned.Model = "claude-fable-5"
	nf := *cfg
	nf.NoFallbackModels = []string{"claude-fable-5"}
	if geminiDivertOK(root, &nf, pinned, now) {
		t.Fatal("no_fallback 模型不得改道")
	}
	// 闸④：交叉卡引擎身份不偷换。
	cross := base()
	cross.Type = typeCrossCheck
	if geminiDivertOK(root, cfg, cross, now) {
		t.Fatal("交叉卡不得改道")
	}
	// 闸⑤：复审位质量地板。
	review := base()
	review.Type = typeReview
	if geminiDivertOK(root, cfg, review, now) {
		t.Fatal("复审位不得改道")
	}
	roleC := base()
	roleC.XRole = "C"
	if geminiDivertOK(root, cfg, roleC, now) {
		t.Fatal("交叉裁决 C 卡不得改道")
	}
	// 闸⑥：gemini 车道自己在冷却。
	setEngineCooldown(root, "gemini", now.Add(time.Hour).Unix(), "test")
	if geminiDivertOK(root, cfg, base(), now) {
		t.Fatal("车道冷却期不得改道")
	}
	clearEngineCooldown(root, "gemini")
}

func TestPinnedGeminiReadyAndPickDivert(t *testing.T) {
	root := testRoot(t)
	now := time.Now()
	cfg := defaultConfig("")
	if pinnedGeminiReady(root, cfg, now) {
		t.Fatal("gemini_bin 未配时钉定不可派（跳过等待，绝不 fail-open）")
	}
	cfg.GeminiBin = "/usr/bin/true"
	if !pinnedGeminiReady(root, cfg, now) {
		t.Fatal("bin 已配且无冷却应可派")
	}
	setEngineCooldown(root, "gemini", now.Add(time.Hour).Unix(), "test")
	if pinnedGeminiReady(root, cfg, now) {
		t.Fatal("车道冷却中钉定应等待")
	}
	clearEngineCooldown(root, "gemini")

	// fallback_order 顺序语义：codex 不可用时轮到 gemini；都可用时 codex 优先。
	task := &Task{ID: "d1", Type: typeSequence, Model: "sonnet", Prompts: []string{"p"}}
	cfg.FallbackOrder = []string{"codex", "gemini"}
	cfg.CodexFallback = false // codex 闸①关
	if got := pickDivertRunner(root, cfg, task, now); got != "gemini" {
		t.Fatalf("codex 不可用应轮到 gemini, got %q", got)
	}
	cfg.CodexFallback = true
	cfg.CodexBin = "/usr/bin/true"
	if got := pickDivertRunner(root, cfg, task, now); got != "codex" {
		t.Fatalf("链序 codex 在前应先选 codex, got %q", got)
	}
}

// ---- JSON 输出解析 ----

func TestParseGeminiJSONAndStats(t *testing.T) {
	j := parseGeminiJSON([]byte(`{"response":"答复正文","stats":{"models":{"gemini-3.1-pro-preview":{"tokens":{"prompt":100,"candidates":50,"cached":20,"thoughts":30,"tool":5}}}}}`))
	if j == nil || j.Response != "答复正文" {
		t.Fatalf("response 解析失败: %+v", j)
	}
	u := geminiStatsUsage(j.Stats)
	if u == nil || u.InputTokens != 80 || u.CacheReadInputTokens != 20 || u.OutputTokens != 85 {
		t.Fatalf("stats 应按 prompt-cached/cached/candidates+thoughts+tool 折算, got %+v", u)
	}
	// 错误对象形态。
	j = parseGeminiJSON([]byte(`{"error":{"type":"QuotaError","message":"You have exhausted your daily quota on this model.","code":429}}`))
	if j == nil || j.Error == nil || !strings.Contains(j.Error.Message, "daily quota") {
		t.Fatalf("error 解析失败: %+v", j)
	}
	// stdout 前后有 Node 噪声时截取 JSON 体。
	j = parseGeminiJSON([]byte("Warning: color\n{\"response\":\"ok\"}\n"))
	if j == nil || j.Response != "ok" {
		t.Fatalf("噪声包裹的 JSON 应可解析: %+v", j)
	}
	if parseGeminiJSON([]byte("not json at all")) != nil {
		t.Fatal("非 JSON 应返回 nil")
	}
	if geminiStatsUsage([]byte(`{"weird":true}`)) != nil {
		t.Fatal("形状不符的 stats 应返回 nil（宁缺不编造）")
	}
}

// ---- 假 gemini 端到端（argv/stdin 契约 + 车道语义）----

func fakeGemini(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gemini")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInvokeGeminiArgvStdinAndSession(t *testing.T) {
	capDir := t.TempDir()
	t.Setenv("CAP_DIR", capDir)
	bin := fakeGemini(t, `printf '%s\n' "$@" > "$CAP_DIR/argv"
cat > "$CAP_DIR/stdin"
printf '%s' '{"response":"done ok","stats":{"models":{"m":{"tokens":{"prompt":10,"candidates":5,"cached":0,"thoughts":0,"tool":0}}}}}'
`)
	cfg := defaultConfig("")
	cfg.GeminiBin = bin
	cfg.StepTimeoutMin = 1
	task := &Task{ID: "argv-1", Type: typeSequence, Dir: t.TempDir(), Model: "claude-fable-5", Prompts: []string{"p"}}
	root := admitDirectInvoke(t, "", task)

	res, _, note, err := invokeGemini(context.Background(), root, cfg, task, "任务正文")
	if err != nil || res == nil || res.IsError {
		t.Fatalf("成功路径: err=%v res=%+v", err, res)
	}
	if res.Result != "done ok" || note != "" {
		t.Fatalf("response 应成为 Result: %q note=%q", res.Result, note)
	}
	argv, _ := os.ReadFile(filepath.Join(capDir, "argv"))
	got := strings.Split(strings.TrimSpace(string(argv)), "\n")
	joined := strings.Join(got, " ")
	for _, want := range []string{"-o json", "--approval-mode yolo", "--skip-trust", "-m pro", "--session-id "} {
		if !strings.Contains(joined+" ", want) {
			t.Fatalf("argv 缺 %q: %v", want, got)
		}
	}
	// fable 卡经槽映射跑 pro；会话 ID 是本进程生成的 UUIDv4 并随 res 上报。
	sid := ""
	for i, a := range got {
		if a == "--session-id" && i+1 < len(got) {
			sid = got[i+1]
		}
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(sid) {
		t.Fatalf("--session-id 应为 UUIDv4, got %q", sid)
	}
	if res.SessionID != sid {
		t.Fatalf("会话应随 res 上报: res=%q argv=%q", res.SessionID, sid)
	}
	stdin, _ := os.ReadFile(filepath.Join(capDir, "stdin"))
	if !strings.Contains(string(stdin), "任务正文") || !strings.Contains(string(stdin), "SUBAGENT") {
		t.Fatalf("prompt 应走 stdin 且带 subagent 前导, got %q", string(stdin))
	}
	if res.Usage == nil || res.Usage.InputTokens != 10 {
		t.Fatalf("stats 应折算进 Usage, got %+v", res.Usage)
	}

	// 已有会话 → --resume，不再生成新 --session-id。
	task.SessionID = "11111111-2222-4333-8444-555555555555"
	if _, _, _, err = invokeGemini(context.Background(), root, cfg, task, "续跑"); err != nil {
		t.Fatal(err)
	}
	argv, _ = os.ReadFile(filepath.Join(capDir, "argv"))
	if !strings.Contains(string(argv), "--resume\n11111111-2222-4333-8444-555555555555") {
		t.Fatalf("有会话应 --resume: %q", string(argv))
	}
	if strings.Contains(string(argv), "--session-id") {
		t.Fatalf("--resume 时不得再传 --session-id: %q", string(argv))
	}
	// 复审卡强制 plan。
	review := &Task{ID: "argv-2", Type: typeReview, Dir: t.TempDir(), Prompts: []string{"p"}}
	admitDirectInvoke(t, root, review)
	if _, _, _, err = invokeGemini(context.Background(), root, cfg, review, "审"); err != nil {
		t.Fatal(err)
	}
	argv, _ = os.ReadFile(filepath.Join(capDir, "argv"))
	if !strings.Contains(strings.ReplaceAll(string(argv), "\n", " "), "--approval-mode plan") {
		t.Fatalf("复审卡应强制 plan: %q", string(argv))
	}
}

func TestRunTaskGeminiDailyLimitFixtureIsRetiredBeforeInvocation(t *testing.T) {
	root := testRoot(t)
	cfg := defaultConfig("")
	cfg.GeminiBin = fakeGemini(t, `printf 'Error: You have exhausted your daily quota on this model.\n' >&2
exit 1
`)
	cfg.StepTimeoutMin = 1
	cfg.LimitFallbackMin = 30
	cfg.CooldownMarginSec = 0

	task := newTask(root, cfg, typeSequence, "gemini daily limit", t.TempDir(), []string{"p"}, 1)
	task.PreferRunner = "gemini"
	task.Attempts = 2
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	if err := runTaskVia(context.Background(), root, cfg, task, "gemini"); err != nil {
		t.Fatal(err)
	}
	got, err := loadTask(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusHeld || !strings.Contains(got.LastError, "retired") {
		t.Fatalf("历史 Gemini 卡必须在调用前 held, got %q (%s)", got.Status, got.LastError)
	}
	if got.Attempts != 2 {
		t.Fatalf("退休闸不得烧 attempts, got %d", got.Attempts)
	}
	cd := loadEngineCooldown(root, "gemini")
	if cd != nil && cd.active(time.Now()) {
		t.Fatalf("退休闸不得伪造 Gemini 冷却: %+v", cd)
	}
	if _, err := os.Stat(cooldownPath(root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("gemini 车道挂起不得写 claude 全局冷却, stat err=%v", err)
	}
}

func TestRunTaskGeminiAuthFixtureIsRetiredBeforeInvocation(t *testing.T) {
	root := testRoot(t)
	cfg := defaultConfig("")
	cfg.GeminiBin = fakeGemini(t, `printf 'YOLO mode is enabled. All tool calls will be automatically approved.\n' >&2
printf 'Error authenticating: IneligibleTierError: This client is no longer supported for Gemini Code Assist for individuals. reasonCode: UNSUPPORTED_CLIENT\n' >&2
exit 1
`)
	cfg.StepTimeoutMin = 1
	cfg.CooldownMarginSec = 0
	task := newTask(root, cfg, typeSequence, "gemini auth", t.TempDir(), []string{"p"}, 1)
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	if err := runTaskVia(context.Background(), root, cfg, task, "gemini"); err != nil {
		t.Fatal(err)
	}
	got, _ := loadTask(root, task.ID)
	if got.Status != statusHeld || !strings.Contains(got.LastError, "retired") || got.Attempts != 0 {
		t.Fatalf("历史 Gemini 卡必须在认证/模型调用前 held: %+v", got)
	}
	cd := loadEngineCooldown(root, "gemini")
	if cd != nil && cd.active(time.Now()) {
		t.Fatalf("未执行的退休卡不得写认证冷却: %+v", cd)
	}
}

func TestRunTaskGeminiSuccessFixtureCannotBypassRetirement(t *testing.T) {
	root := testRoot(t)
	cfg := defaultConfig("")
	cfg.GeminiBin = fakeGemini(t, `cat > /dev/null
printf '%s' '{"response":"完成","stats":{"models":{"m":{"tokens":{"prompt":7,"candidates":3,"cached":0,"thoughts":0,"tool":0}}}}}'
`)
	cfg.StepTimeoutMin = 1

	// 预置两个冷却：claude 全局 + gemini 车道。gemini 成功只准清自己的。
	claudeUntil := time.Now().Add(time.Hour).Unix()
	setCooldown(root, claudeUntil, "claude limit")
	setEngineCooldown(root, "gemini", time.Now().Add(time.Hour).Unix(), "stale")

	task := newTask(root, cfg, typeSequence, "gemini ok", t.TempDir(), []string{"p"}, 1)
	task.PreferRunner = "gemini"
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	if err := runTaskVia(context.Background(), root, cfg, task, "gemini"); err != nil {
		t.Fatal(err)
	}
	got, _ := loadTask(root, task.ID)
	if got.Status != statusHeld || !strings.Contains(got.LastError, "retired") || got.Attempts != 0 {
		t.Fatalf("即使 fixture 可成功，历史 Gemini 卡也必须在调用前 held: %+v", got)
	}
	if !loadEngineCooldown(root, "gemini").active(time.Now()) {
		t.Fatal("退休闸不得改写历史 Gemini 冷却")
	}
	if cd := loadCooldown(root); !cd.active(time.Now()) || cd.UntilEpoch != claudeUntil {
		t.Fatal("gemini 成功不得动 claude 全局冷却（账各归各）")
	}
	if recs := loadUsage(root); len(recs) != 0 {
		t.Fatalf("未调用的退休卡不得写 usage: %+v", recs)
	}
}

func TestRunTaskGeminiDivertDoesNotWriteBackSession(t *testing.T) {
	root := testRoot(t)
	cfg := defaultConfig("")
	cfg.GeminiBin = fakeGemini(t, `cat > /dev/null
printf '%s' '{"response":"完成"}'
`)
	cfg.StepTimeoutMin = 1
	// 改道卡：PreferRunner 空（claude 卡被 fallback_order 改道）。会话不得回写——
	// 冷却结束回 claude 时带 gemini 会话 = 跨引擎 --resume，引擎身份漂移。
	task := newTask(root, cfg, typeSequence, "divert", t.TempDir(), []string{"p"}, 1)
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	if err := runTaskVia(context.Background(), root, cfg, task, "gemini"); err != nil {
		t.Fatal(err)
	}
	got, _ := loadTask(root, task.ID)
	if got.SessionID != "" {
		t.Fatalf("改道卡不得回写 gemini 会话, got %q", got.SessionID)
	}
}

// ---- 历史交叉验证 kind 只保留解码/展示 ----

func TestApplyCrossEngineGemini(t *testing.T) {
	cfg := defaultConfig("")
	cfg.GeminiBin = "/usr/bin/true"
	cfg.GeminiModel = "pro"

	if err := applyCrossEngine(&Task{Prompts: []string{"x"}}, CrossEngine{Kind: "gemini"}, cfg); err == nil {
		t.Fatal("新 Gemini 交叉卡必须拒绝")
	}
	if _, err := freezeCrossEngine(CrossEngine{Kind: "gemini"}, cfg); err == nil {
		t.Fatal("新 Gemini 冻结规格必须拒绝")
	}
	// 历史 profile identity 仍可稳定显示，不能与 codex 串味。
	if crossEngineIdentity(CrossEngine{Kind: "gemini"}, cfg) == crossEngineIdentity(CrossEngine{Kind: "codex"}, cfg) {
		t.Fatal("gemini 与 codex 身份串不得相同")
	}
}

// ---- 看板档位（统一标准线 AA II v4.1 快照 2026-08-03）----

func TestModelTierGeminiStandardLine(t *testing.T) {
	cfg := defaultConfig("")
	for model, want := range map[string]string{
		"gemini-3.5-flash":       "sonnet", // AA 50
		"gemini-3.6-flash":       "sonnet", // AA 50
		"flash":                  "sonnet",
		"gemini-3.1-pro-preview": "haiku", // AA 46，haiku 档上沿（距 sonnet 下沿 47 差 1 分）
		"pro":                    "haiku",
		"gemini-2.5-pro":         "haiku", // AA 26
		"gemini-3.5-flash-lite":  "haiku", // AA 36
		"flash-lite":             "haiku",
		// 裸别名精确匹配纪律：不误吃他家 *-pro/*-flash。
		"mimo-v2.5-pro":     "haiku",
		"deepseek-v4-flash": "haiku",
	} {
		if got := modelTierKeyword(cfg, model); got != want {
			t.Fatalf("modelTierKeyword(%q) = %q, want %q", model, got, want)
		}
	}
	// model_tiers 自定义覆写恒优先（按牌面抬档出口）。
	cfg.ModelTiers = map[string]string{"gemini-3.1-pro-preview": "sonnet"}
	if got := modelTierKeyword(cfg, "gemini-3.1-pro-preview"); got != "sonnet" {
		t.Fatalf("model_tiers 覆写应恒优先, got %q", got)
	}
}

func TestEffectiveModelGeminiSide(t *testing.T) {
	cfg := defaultConfig("")
	m, src := effectiveModel(cfg, &Task{PreferRunner: "gemini", Model: "sonnet"})
	if m != "flash" || src != "gemini_model" {
		t.Fatalf("gemini 系应显示槽映射真跑模型, got %q/%q", m, src)
	}
	m, src = effectiveModel(cfg, &Task{Runner: "gemini", GeminiModel: "flash-lite"})
	if m != "flash-lite" || src != "gemini_model" {
		t.Fatalf("卡级钉定应直显, got %q/%q", m, src)
	}
}

// ---- emit 契约 ----

func TestEnqueueEmittedRunnerGeminiRejected(t *testing.T) {
	root := testRoot(t)
	cfg := defaultConfig("")
	parent := newTask(root, cfg, typeCoordinate, "父", t.TempDir(), []string{"p"}, 1)
	result := "```json\n{\"tasks\":[{\"title\":\"填充\",\"type\":\"sequence\",\"runner\":\"gemini\",\"gemini_model\":\"flash\",\"prompts\":[\"做事\"]}]}\n```"
	ids, err := enqueueEmitted(root, cfg, parent, result)
	if err == nil || len(ids) != 0 {
		t.Fatalf("emit 不得创建 Gemini 新卡: ids=%v err=%v", ids, err)
	}
}
