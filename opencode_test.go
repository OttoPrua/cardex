package main

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

func TestResolveOpenCodeModel(t *testing.T) {
	cfg := defaultConfig("claude")
	cfg.OpenCodeModel = "opencode-go/gpt-5.6-luna"
	cfg.OpenCodeModels = map[string]string{"sonnet": "opencode-go/glm-5.2"}
	if got := resolveOpenCodeModel(cfg, &Task{Model: "sonnet"}); got != "opencode-go/glm-5.2" {
		t.Fatalf("tier model = %q", got)
	}
	if got := resolveOpenCodeModel(cfg, &Task{OpenCodeModel: "opencode-go/qwen3.7-max"}); got != "opencode-go/qwen3.7-max" {
		t.Fatalf("task pin = %q", got)
	}
}

func TestParseOpenCodeJSONL(t *testing.T) {
	raw := `{"type":"text","sessionID":"ses-1","part":{"text":"OK","time":{"start":10,"end":25}}}` + "\n" +
		`{"type":"step_finish","sessionID":"ses-1","part":{"tokens":{"input":7,"output":5},"cost":0.25}}`
	res := parseOpenCodeJSONL(raw)
	if res.Result != "OK" || res.SessionID != "ses-1" || res.NumTurns != 1 || res.TotalCostUSD != 0.25 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Usage == nil || res.Usage.InputTokens != 7 || res.Usage.OutputTokens != 5 {
		t.Fatalf("unexpected usage: %+v", res.Usage)
	}
}

func openCodeNightTestConfig(bin string) *Config {
	cfg := defaultConfig("")
	cfg.DefaultRunner = "codex"
	cfg.CodexBin = "/usr/bin/true"
	cfg.OpenCodeBin = bin
	cfg.StepTimeoutMin = 1
	cfg.CooldownMarginSec = 0
	cfg.OpenCodeNightOpus = &OpenCodeNightRoute{
		Enabled: true, StartHour: 1, EndHour: 4, Timezone: "Asia/Shanghai",
		Model: "opencode-go/kimi-k3", Variant: "max", LimitFallbackMin: 180,
	}
	return cfg
}

func TestOpenCodeNightOpusWindowAndGuards(t *testing.T) {
	root := testRoot(t)
	cfg := openCodeNightTestConfig("/usr/bin/true")
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	task := &Task{Model: "opus", PreferRunner: "codex", Dir: t.TempDir(), Prompts: []string{"p"}}
	cases := []struct {
		name string
		hour int
		min  int
		want bool
	}{
		{"窗前", 0, 59, false},
		{"起点包含", 1, 0, true},
		{"窗内", 3, 59, true},
		{"终点排除", 4, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 8, 7, tc.hour, tc.min, 0, 0, loc)
			if got := openCodeNightOpusEligible(root, cfg, task, now); got != tc.want {
				t.Fatalf("eligible=%v want %v", got, tc.want)
			}
		})
	}

	now := time.Date(2026, 8, 7, 2, 0, 0, 0, loc)
	for name, mutate := range map[string]func(*Task){
		"Fable 不改道":           func(x *Task) { x.Model = "fable" },
		"Sonnet 不改道":          func(x *Task) { x.Model = "sonnet" },
		"远端不改道":               func(x *Task) { x.RemoteHost = "qmthost" },
		"显式 Codex 模型不改道":      func(x *Task) { x.CodexModel = "gpt-5.6-sol" },
		"已有会话不改道":             func(x *Task) { x.SessionID = "session" },
		"显式 Codex runner 不改道": func(x *Task) { x.RunnerExplicit = true },
		"限额接力后不弹回":            func(x *Task) { x.RouteReason = routeReasonOpenCodeLimitFallback },
	} {
		t.Run(name, func(t *testing.T) {
			probe := *task
			mutate(&probe)
			if openCodeNightOpusEligible(root, cfg, &probe, now) {
				t.Fatal("护栏任务不应夜间改道")
			}
		})
	}
	ownerCfg := *cfg
	ownerCfg.OwnerRoutingEnforced = true
	if openCodeNightOpusEligible(root, &ownerCfg, task, now) {
		t.Fatal("Owner 强制路由下必须完全禁用旧 OpenCode 夜间自动分支")
	}

	setEngineCooldown(root, openCodeCooldownName, now.Add(time.Hour).Unix(), "test")
	if openCodeNightOpusEligible(root, cfg, task, now) {
		t.Fatal("OpenCode 车道冷却时应直接选 Codex")
	}
}

func TestResolveOpenCodeNightRunModelAndVariant(t *testing.T) {
	cfg := openCodeNightTestConfig("/usr/bin/true")
	auto := &Task{Model: "opus", PreferRunner: "codex", Effort: "xhigh"}
	if got := resolveOpenCodeRunModel(cfg, auto); got != "opencode-go/kimi-k3" {
		t.Fatalf("night model=%q", got)
	}
	if got := resolveOpenCodeRunVariant(cfg, auto); got != "max" {
		t.Fatalf("night variant=%q", got)
	}
	explicit := &Task{Model: "sonnet", PreferRunner: "opencode", OpenCodeModel: "opencode-go/glm-5.2", Effort: "high"}
	if got := resolveOpenCodeRunModel(cfg, explicit); got != "opencode-go/glm-5.2" {
		t.Fatalf("explicit model=%q", got)
	}
	if got := resolveOpenCodeRunVariant(cfg, explicit); got != "high" {
		t.Fatalf("explicit variant=%q", got)
	}
}

func TestPinnedKimiK3CanStartOutsideNightWindow(t *testing.T) {
	root := testRoot(t)
	cfg := openCodeNightTestConfig("/usr/bin/true")
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	kimi := &Task{PreferRunner: "opencode", OpenCodeModel: "opencode-go/kimi-k3"}
	if !openCodePinnedReady(root, cfg, kimi, time.Date(2026, 8, 7, 12, 0, 0, 0, loc)) {
		t.Fatal("显式 Kimi K3 是人工钉定，白天也应可运行")
	}
	if !openCodePinnedReady(root, cfg, kimi, time.Date(2026, 8, 7, 2, 0, 0, 0, loc)) {
		t.Fatal("显式 Kimi K3 在夜间也应可运行")
	}
	glm := &Task{PreferRunner: "opencode", OpenCodeModel: "opencode-go/glm-5.2"}
	if !openCodePinnedReady(root, cfg, glm, time.Date(2026, 8, 7, 12, 0, 0, 0, loc)) {
		t.Fatal("其他显式 OpenCode 模型不应被 Kimi 夜间窗口误挡")
	}
}

func TestOpenCodeLimitDetectorUsesOnlyFailedOutput(t *testing.T) {
	errJSON := `{"type":"error","sessionID":"s","error":{"name":"APIError","message":"HTTP 429: usage limit reached"}}`
	res := parseOpenCodeJSONL(errJSON)
	if !isLimitHitOpenCode(res, errJSON) {
		t.Fatal("OpenCode 429 应识别为车道限额")
	}
	success := &claudeResult{Result: "reviewed rate limit handling", IsError: false}
	if isLimitHitOpenCode(success, `{"type":"text","part":{"text":"rate limit"}}`) {
		t.Fatal("成功终稿讨论 rate limit 不得误挂车道")
	}
}

func fakeOpenCode(t *testing.T, payload string, exitCode int) (bin, argsDump string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "opencode")
	argsDump = filepath.Join(dir, "args.dump")
	payloadPath := filepath.Join(dir, "result.jsonl")
	if err := os.WriteFile(payloadPath, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + shSingleQuote(argsDump) + "\n" +
		"cat " + shSingleQuote(payloadPath) + "\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsDump
}

func TestInvokeOpenCodeNightUsesKimiMax(t *testing.T) {
	payload := `{"type":"text","sessionID":"ses-ok","part":{"text":"OK"}}` + "\n" +
		`{"type":"step_finish","sessionID":"ses-ok","part":{"tokens":{"input":7,"output":5},"cost":0.25}}`
	bin, argsDump := fakeOpenCode(t, payload, 0)
	cfg := openCodeNightTestConfig(bin)
	task := &Task{ID: "oc-kimi-max", Model: "opus", PreferRunner: "codex", Effort: "xhigh", Dir: t.TempDir()}
	admitDirectInvoke(t, "", task)
	res, _, err := invokeOpenCode(context.Background(), cfg, task, "p")
	if err != nil || res == nil || res.Result != "OK" {
		t.Fatalf("invoke failed: res=%+v err=%v", res, err)
	}
	args, err := os.ReadFile(argsDump)
	if err != nil {
		t.Fatal(err)
	}
	got := string(args)
	if !strings.Contains(got, "opencode-go/kimi-k3\n") || !strings.Contains(got, "--variant\nmax\n") {
		t.Fatalf("夜间调用必须 Kimi K3/max, argv:\n%s", got)
	}
}

func TestRunTaskOpenCodeNightLimitQueuesCodexFallback(t *testing.T) {
	root := testRoot(t)
	payload := `{"type":"error","sessionID":"ses-limit","error":{"name":"APIError","message":"HTTP 429: usage limit reached"}}`
	bin, _ := fakeOpenCode(t, payload, 1)
	cfg := openCodeNightTestConfig(bin)
	task := newTask(root, cfg, typeReview, "night limit", t.TempDir(), []string{"p"}, 1)
	task.Model = "opus"
	task.Effort = "xhigh"
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	if err := runTaskVia(context.Background(), root, cfg, task, "opencode"); err != nil {
		t.Fatal(err)
	}
	got, err := loadTask(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusQueued || got.RouteReason != routeReasonOpenCodeLimitFallbackPending {
		t.Fatalf("夜间限额应立即排队转 Codex: status=%q route=%q err=%q", got.Status, got.RouteReason, got.LastError)
	}
	if got.Attempts != 0 || got.SessionID != "" || got.MidStep || got.ResumeAtEpoch != 0 {
		t.Fatalf("接力不得烧 attempts 或携带 OpenCode 会话: %+v", got)
	}
	cd := loadEngineCooldown(root, openCodeCooldownName)
	if cd == nil || !cd.active(time.Now()) || cd.UntilEpoch < time.Now().Add(179*time.Minute).Unix() {
		t.Fatalf("应写独立 OpenCode 车道冷却约 180 分钟: %+v", cd)
	}
	if _, err := os.Stat(cooldownPath(root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenCode 限额不得写 Claude 全局冷却: %v", err)
	}
	events := readAllEventsRaw(t, root, task.ID)
	last := events[len(events)-1]
	if last.Type != evRetry || last.Actor != "runner:opencode" || last.Detail["fallback_runner"] != "codex" {
		t.Fatalf("应留下 Kimi→Codex 接力事件: %+v", last)
	}
	if last.Detail["fallback_model"] != "gpt-5.6-sol" || last.Detail["fallback_reasoning"] != "xhigh" {
		t.Fatalf("接力必须 Sol/xhigh: %+v", last.Detail)
	}
}

func TestRunTaskPinnedKimiLimitPreservesExplicitProvider(t *testing.T) {
	root := testRoot(t)
	payload := `{"type":"error","error":{"message":"quota exceeded"}}`
	bin, _ := fakeOpenCode(t, payload, 1)
	cfg := openCodeNightTestConfig(bin)
	task := newTask(root, cfg, typeSequence, "pinned limit", t.TempDir(), []string{"p"}, 1)
	task.PreferRunner = "opencode"
	task.OpenCodeModel = "opencode-go/kimi-k3"
	task.Model = "sonnet" // 证明接力不误用原卡的 Luna 档位。
	task.Effort = "max"
	task.EffortExplicit = true
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	if err := runTaskVia(context.Background(), root, cfg, task, "opencode"); err != nil {
		t.Fatal(err)
	}
	got, err := loadTask(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusLimitPaused {
		t.Fatalf("显式 OpenCode/Kimi 限额后必须保留 provider 钉定并暂停: %+v", got)
	}
	if got.PreferRunner != "opencode" || got.OpenCodeModel != "opencode-go/kimi-k3" ||
		got.CodexModel != "" || got.Effort != "max" || !got.EffortExplicit {
		t.Fatalf("显式 provider 身份不得被额度分支改写: %+v", got)
	}
	if got.ResumeAtEpoch == 0 {
		t.Fatalf("显式 provider 应等待独立车道冷却: %+v", got)
	}
}

func TestOwnerRoutingEnforcedDisablesLegacyOpenCodeAutoFallback(t *testing.T) {
	root := testRoot(t)
	payload := `{"type":"error","error":{"message":"quota exceeded"}}`
	bin, _ := fakeOpenCode(t, payload, 1)
	cfg := openCodeNightTestConfig(bin)
	cfg.OwnerRoutingEnforced = true
	task := newTask(root, cfg, typeReview, "legacy auto route", t.TempDir(), []string{"p"}, 1)
	task.Model = "opus"
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	if err := runTaskVia(context.Background(), root, cfg, task, "opencode"); err != nil {
		t.Fatal(err)
	}
	got, err := loadTask(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusLimitPaused || got.PreferRunner != "codex" || got.RouteReason != routeReasonOpenCodeNightOpus {
		t.Fatalf("Owner 强制模式不得让旧 OpenCode 路径无证明接力: %+v", got)
	}
}

func TestRunTaskPinnedKimiNonLimitErrorDoesNotFallback(t *testing.T) {
	root := testRoot(t)
	payload := `{"type":"error","error":{"message":"temporary upstream failure"}}`
	bin, _ := fakeOpenCode(t, payload, 1)
	cfg := openCodeNightTestConfig(bin)
	task := newTask(root, cfg, typeSequence, "pinned non-limit", t.TempDir(), []string{"p"}, 1)
	task.PreferRunner = "opencode"
	task.OpenCodeModel = "opencode-go/kimi-k3"
	task.Effort = "max"
	task.EffortExplicit = true
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	if err := runTaskVia(context.Background(), root, cfg, task, "opencode"); err != nil {
		t.Fatal(err)
	}
	got, err := loadTask(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusQueued || got.PreferRunner != "opencode" || got.CodexModel != "" {
		t.Fatalf("非限额错误必须继续钉定 Kimi 并按普通退避重试: %+v", got)
	}
	if got.RouteReason != routeReasonOpenCodeExplicit || got.Attempts != 1 {
		t.Fatalf("非限额失败应保留人工路由并消耗一次普通重试: %+v", got)
	}
}

func TestOpenCodeLimitFallbackReasonSurvivesCodexRetries(t *testing.T) {
	root := testRoot(t)
	cfg := openCodeNightTestConfig("/usr/bin/true")
	cfg.CodexBin = fakeCodexUsageLimit(t)
	cfg.LimitFallbackMin = 1
	task := newTask(root, cfg, typeSequence, "fallback retry", t.TempDir(), []string{"p"}, 1)
	task.Model = "opus"
	task.RouteReason = routeReasonOpenCodeLimitFallbackPending
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := runTaskVia(context.Background(), root, cfg, task, "codex"); err != nil {
			t.Fatal(err)
		}
		got, err := loadTask(root, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.RouteReason != routeReasonOpenCodeLimitFallback {
			t.Fatalf("第 %d 次 Codex 尝试后接力原因丢失: %q", attempt+1, got.RouteReason)
		}
		got.Status = statusQueued
		got.ResumeAtEpoch = 0
		got.NotBeforeEpoch = 0
		if err := saveTask(root, got); err != nil {
			t.Fatal(err)
		}
		task = got
	}
}

func TestOpenCodeActualRouteTelemetryAndBoard(t *testing.T) {
	cfg := openCodeNightTestConfig("/usr/bin/true")
	task := &Task{
		Runner: "opencode", PreferRunner: "codex", Model: "opus", Effort: "xhigh",
		RouteReason: routeReasonOpenCodeNightOpus,
	}
	detail := dispatchEventDetail(cfg, task, false, false)
	if detail["opencode_model"] != "opencode-go/kimi-k3" || detail["opencode_variant"] != "max" ||
		detail["route_reason"] != routeReasonOpenCodeNightOpus {
		t.Fatalf("派发遥测未记录真实 Kimi 组合: %+v", detail)
	}
	if _, ok := detail["codex_model"]; ok {
		t.Fatalf("OpenCode 实跑不得伪造 Codex 模型: %+v", detail)
	}
	if model, source := effectiveModel(cfg, task); model != "opencode-go/kimi-k3" || source != "opencode_model" {
		t.Fatalf("看板模型=(%q,%q)", model, source)
	}
	if effort, source := effectiveEffort(cfg, task); effort != "max" || source != "opencode_variant" {
		t.Fatalf("看板思考档=(%q,%q)", effort, source)
	}
}

func TestKimiBoardBriefShowsActualPreferredAndFallbackRoutes(t *testing.T) {
	cfg := openCodeNightTestConfig("/usr/bin/true")
	now := time.Date(2026, 8, 7, 2, 30, 0, 0, time.FixedZone("CST", 8*60*60))

	running := &Task{
		ID: "k3-running", Status: statusRunning, Runner: "opencode", PreferRunner: "codex",
		Model: "opus", Effort: "xhigh", RouteReason: routeReasonOpenCodeNightOpus,
		Prompts: []string{"run"}, UpdatedAt: now.Add(-time.Minute).Format(time.RFC3339),
	}
	brief := toBrief(cfg, running, now)
	if brief.Runner != "opencode" || brief.RunnerSource != "actual" ||
		brief.Model != "opencode-go/kimi-k3" || brief.ModelSource != "opencode_model" ||
		brief.ModelTier != "高" || brief.Effort != "max" || brief.EffortSource != "opencode_variant" ||
		brief.RouteReason != routeReasonOpenCodeNightOpus {
		t.Fatalf("K3 实跑卡看板字段不完整: %+v", brief)
	}

	queuedExplicit := &Task{
		ID: "k3-queued", Status: statusQueued, PreferRunner: "opencode",
		OpenCodeModel: "opencode-go/kimi-k3", Model: "opus", Effort: "max", Prompts: []string{"run"},
	}
	brief = toBrief(cfg, queuedExplicit, now)
	if brief.Runner != "opencode" || brief.RunnerSource != "runner_pref" ||
		brief.Model != "opencode-go/kimi-k3" || brief.Effort != "max" {
		t.Fatalf("未派发的显式 K3 卡不得误显示成 Claude: %+v", brief)
	}

	fallback := &Task{
		ID: "k3-fallback", Status: statusQueued, Runner: "opencode", PreferRunner: "codex",
		Model: "opus", Effort: "xhigh", RouteReason: routeReasonOpenCodeLimitFallbackPending,
		Prompts: []string{"run"},
	}
	brief = toBrief(cfg, fallback, now)
	if brief.Runner != "codex" || brief.RunnerSource != "route_reason" ||
		brief.Model != resolveCodexModel(cfg, fallback) || brief.ModelSource != "codex_model" ||
		brief.Effort != "xhigh" || brief.EffortSource != "codex_reasoning" {
		t.Fatalf("K3 限额接力卡必须显示下一跳 Sol/xhigh，而不是旧 K3: %+v", brief)
	}
}

func TestKimiBoardFrontendConsumesRunnerVariantAndRoute(t *testing.T) {
	code := appJSCode(t)
	for _, want := range []string{
		"Kimi K3",
		"opencode_model: 'OpenCode 实际模型'",
		"opencode_variant: 'OpenCode variant'",
		"opencode_night_opus_preferred: '夜间 K3 优先'",
		"opencode_limit_fallback_pending: 'K3 限额，待转 Sol'",
		"runnerChip(t.runner)",
		"routeChip(t.route_reason)",
		"t.runner_source",
	} {
		if !strings.Contains(code, want) {
			t.Errorf("K3 看板前端缺少 %q", want)
		}
	}
	if strings.Count(code, "routeChip(t.route_reason)") < 2 {
		t.Fatal("总览任务行与项目任务卡都必须显示 K3 路由原因")
	}
}

func TestValidateOpenCodeNightRoute(t *testing.T) {
	cfg := openCodeNightTestConfig("/usr/bin/true")
	if err := validateOpenCode(cfg); err != nil {
		t.Fatalf("合法夜间策略被拒: %v", err)
	}
	cfg.OpenCodeNightOpus.Timezone = "Mars/Olympus"
	if err := validateOpenCode(cfg); err == nil {
		t.Fatal("非法时区必须 fail-fast")
	}
}
