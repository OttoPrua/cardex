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

func kimiCLITestConfig(t *testing.T, bin string) *Config {
	t.Helper()
	cfg := defaultConfig("")
	cfg.DefaultRunner = "codex"
	cfg.CodexBin = "/usr/bin/true"
	cfg.KimiCLIBin = bin
	cfg.KimiCLIHome = t.TempDir()
	if err := os.WriteFile(filepath.Join(cfg.KimiCLIHome, "config.toml"), []byte("default_model = \"kimi-code/k3\"\n\n[providers.\"managed:kimi-code\"]\ntype = \"kimi\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(cfg.KimiCLIHome, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.KimiCLIModel = "kimi-code/k3"
	cfg.KimiCLIEffort = "max"
	cfg.StepTimeoutMin = 1
	cfg.CooldownMarginSec = 0
	cfg.KimiCLIOpus = &KimiCLIOpusRoute{
		Enabled: true, ExcludeBackend: true, Model: "kimi-code/k3", Effort: "max", LimitFallbackMin: 180,
	}
	cfg.GrokBuildBin = "/usr/bin/true"
	cfg.GrokBuild = &GrokBuildRoute{
		Enabled: true, Model: "grok-4.6", Effort: "xhigh", KimiOpusFallback: true,
		CodexFallbackModel: "gpt-5.6-sol", CodexFallbackEffort: "xhigh",
		ReviewCodexModel: "gpt-5.6-sol", ReviewCodexEffort: "max", OpusAdversarialReview: true,
		TierRoutes: map[string]GrokTierRoute{
			"opus_backend": {Effort: "xhigh", CodexFallbackModel: "gpt-5.6-sol", CodexFallbackEffort: "max"},
			"sonnet":       {Effort: "high", CodexFallbackModel: "gpt-5.6-luna", CodexFallbackEffort: "max"},
			"haiku":        {Effort: "high", CodexFallbackModel: "gpt-5.6-luna", CodexFallbackEffort: "xhigh"},
		},
	}
	return cfg
}

func TestParseKimiCLIJSONL(t *testing.T) {
	raw := `{"role":"meta","type":"system.version","version":"0.35.0"}` + "\n" +
		`{"role":"assistant","content":"OK"}` + "\n" +
		`{"role":"meta","type":"session.resume_hint","session_id":"session-1"}`
	res := parseKimiCLIJSONL(raw)
	if res.Result != "OK" || res.SessionID != "session-1" || res.NumTurns != 1 || res.IsError {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestParseKimiCLIJSONLUnknownMetadataFailsProofClosed(t *testing.T) {
	res := parseKimiCLIJSONL(`{"role":"meta","type":"model.started"}`)
	if res.ObservationComplete {
		t.Fatalf("unknown metadata must not be assumed presemantic: %+v", res)
	}
}

func TestKimiCLIOpusSecondLegAvailabilityAndBackendExclusion(t *testing.T) {
	root := testRoot(t)
	cfg := kimiCLITestConfig(t, "/usr/bin/true")
	for _, hour := range []int{0, 3, 12, 23} {
		task := &Task{Model: "opus", PreferRunner: "codex", Type: typeSequence, Dir: t.TempDir(), Prompts: []string{"实现前端页面"}}
		now := time.Date(2026, 8, 13, hour, 0, 0, 0, time.Local)
		if !kimiCLIOpusEligible(root, cfg, task, now) {
			t.Fatalf("%02d:00 非后端 Opus 的 Kimi 第二腿应可用", hour)
		}
	}

	for name, task := range map[string]*Task{
		"显式后端": {Model: "opus", PreferRunner: "codex", Type: typeSequence, RouteClass: routeClassBackend, Prompts: []string{"实现功能"}},
		"存量词项": {Model: "opus", PreferRunner: "codex", Type: typeSequence, Prompts: []string{"实现数据库迁移和 API endpoint"}},
		"人工消歧": {Model: "opus", PreferRunner: "codex", Type: typeSequence, RouteClass: routeClassGeneral, Prompts: []string{"评估后端方案但只改文档"}},
	} {
		t.Run(name, func(t *testing.T) {
			excluded := kimiCLIBackendExcluded(cfg, task)
			if name == "人工消歧" && excluded {
				t.Fatal("route_class=general 必须覆盖文本误命中")
			}
			if name != "人工消歧" && !excluded {
				t.Fatal("后端开发必须排除 Kimi 自动路由")
			}
		})
	}

	sonnet := &Task{Model: "sonnet", PreferRunner: "codex", Type: typeSequence, Prompts: []string{"实现前端"}}
	if kimiCLIOpusEligible(root, cfg, sonnet, time.Now()) {
		t.Fatal("Sonnet 不得进入 Opus Kimi 路由")
	}
}

func fakeKimiCLI(t *testing.T, payload string, exitCode int) (bin, argsDump, envDump string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "kimi")
	argsDump = filepath.Join(dir, "args.dump")
	envDump = filepath.Join(dir, "env.dump")
	payloadPath := filepath.Join(dir, "result.jsonl")
	if err := os.WriteFile(payloadPath, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + shSingleQuote(argsDump) + "\n" +
		"printf '%s\\n' \"$KIMI_MODEL_THINKING_EFFORT\" \"$KIMI_CODE_NO_AUTO_UPDATE\" \"$KIMI_CODE_HOME\" > " + shSingleQuote(envDump) + "\n" +
		"cat " + shSingleQuote(payloadPath) + "\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsDump, envDump
}

func TestInvokeKimiCLIUsesK3MaxAndVersionCompatibleFlags(t *testing.T) {
	payload := `{"role":"meta","type":"system.version","version":"0.35.0"}` + "\n" +
		`{"role":"assistant","content":"OK"}` + "\n" +
		`{"role":"meta","type":"session.resume_hint","session_id":"session-ok"}`
	bin, argsDump, envDump := fakeKimiCLI(t, payload, 0)
	cfg := kimiCLITestConfig(t, bin)
	task := &Task{ID: "kimi-k3-max", Model: "opus", PreferRunner: "codex", Effort: "xhigh", Type: typeSequence, Dir: t.TempDir()}
	root := testRoot(t)
	res, combined, err := invokeKimiCLI(context.Background(), root, cfg, task, "p")
	if err != nil || res == nil || res.Result != "OK" || res.SessionID != "session-ok" {
		t.Fatalf("invoke failed: res=%+v err=%v", res, err)
	}
	if strings.Contains(combined, "COLD-START TRANSPORT RETRY") {
		t.Fatalf("0.35 semantic stream must not be replayed:\n%s", combined)
	}
	args, _ := os.ReadFile(argsDump)
	got := string(args)
	for _, want := range []string{"--model\nkimi-code/k3\n", "--prompt\np\n", "--output-format\nstream-json\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Kimi CLI argv 缺 %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "--auto\n") || strings.Contains(got, "--yolo\n") || strings.Contains(got, "--plan\n") {
		t.Fatalf("Kimi CLI 0.35.0 禁止 --prompt 与 --auto/--yolo/--plan 同用:\n%s", got)
	}
	env, _ := os.ReadFile(envDump)
	envLines := strings.Split(strings.TrimSpace(string(env)), "\n")
	executeHome := filepath.Join(root, "kimi-cli-home", "execute")
	if len(envLines) != 3 || envLines[0] != "max" || envLines[1] != "1" || envLines[2] != executeHome {
		t.Fatalf("K3 max 必须通过官方环境变量注入, env=%q", env)
	}
	runtimeConfig, err := os.ReadFile(filepath.Join(executeHome, "config.toml"))
	if err != nil || !strings.Contains(string(runtimeConfig), `default_permission_mode = "auto"`) ||
		!strings.Contains(string(runtimeConfig), `default_plan_mode = false`) {
		t.Fatalf("Cardex 隔离配置必须启用 prompt 模式自动权限: err=%v config=%q", err, runtimeConfig)
	}
}

func TestInvokeKimiCLIReviewUsesIsolatedConfiguredPlanMode(t *testing.T) {
	payload := `{"role":"assistant","content":"REVIEW_OK"}`
	bin, argsDump, envDump := fakeKimiCLI(t, payload, 0)
	cfg := kimiCLITestConfig(t, bin)
	task := &Task{ID: "kimi-review", Model: "opus", Type: "review", Dir: t.TempDir()}
	root := testRoot(t)
	res, _, err := invokeKimiCLI(context.Background(), root, cfg, task, "review")
	if err != nil || res == nil || res.Result != "REVIEW_OK" {
		t.Fatalf("review invoke failed: res=%+v err=%v", res, err)
	}
	args, _ := os.ReadFile(argsDump)
	if got := string(args); strings.Contains(got, "--plan\n") || strings.Contains(got, "--auto\n") || strings.Contains(got, "--yolo\n") {
		t.Fatalf("prompt mode must not receive incompatible permission flags:\n%s", got)
	}
	reviewHome := filepath.Join(root, "kimi-cli-home", "review")
	env, _ := os.ReadFile(envDump)
	if !strings.HasSuffix(strings.TrimSpace(string(env)), reviewHome) {
		t.Fatalf("review must use its isolated runtime home: %q", env)
	}
	runtimeConfig, err := os.ReadFile(filepath.Join(reviewHome, "config.toml"))
	if err != nil || !strings.Contains(string(runtimeConfig), `default_permission_mode = "auto"`) ||
		!strings.Contains(string(runtimeConfig), `default_plan_mode = true`) {
		t.Fatalf("review runtime must enable configured plan mode: err=%v config=%q", err, runtimeConfig)
	}
	executeConfig := filepath.Join(root, "kimi-cli-home", "execute", "config.toml")
	if _, err := os.Stat(executeConfig); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("review must not create or mutate the execute runtime: %v", err)
	}
}

func TestInvokeKimiCLI0361RetriesMetadataOnlyColdStart(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "kimi")
	countPath := filepath.Join(dir, "count")
	script := "#!/bin/sh\n" +
		"count=0\n" +
		"if [ -f " + shSingleQuote(countPath) + " ]; then count=$(cat " + shSingleQuote(countPath) + "); fi\n" +
		"count=$((count + 1))\n" +
		"printf '%s' \"$count\" > " + shSingleQuote(countPath) + "\n" +
		"printf '%s\\n' '{\"role\":\"meta\",\"type\":\"system.version\",\"version\":\"0.36.1\"}'\n" +
		"if [ \"$count\" -eq 1 ]; then exit 1; fi\n" +
		"printf '%s\\n' '{\"role\":\"assistant\",\"content\":\"KIMI_0361_OK\"}'\n" +
		"printf '%s\\n' '{\"role\":\"meta\",\"type\":\"session.resume_hint\",\"session_id\":\"session-0361\"}'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := kimiCLITestConfig(t, bin)
	task := &Task{ID: "kimi-0361-cold-start", Model: "opus", Type: typeSequence, Dir: t.TempDir()}
	res, combined, err := invokeKimiCLI(context.Background(), testRoot(t), cfg, task, "read only")
	if err != nil || res == nil || res.Result != "KIMI_0361_OK" || res.SessionID != "session-0361" {
		t.Fatalf("0.36.1 metadata-only cold start should restart transport once: res=%+v err=%v\n%s", res, err, combined)
	}
	count, err := os.ReadFile(countPath)
	if err != nil || string(count) != "2" {
		t.Fatalf("expected exactly two transport starts, count=%q err=%v", count, err)
	}
}

func TestValidateKimiCLIOpenFileLimit(t *testing.T) {
	if err := validateKimiCLIOpenFileLimit(256); err == nil ||
		!strings.Contains(err.Error(), "EMFILE") || !strings.Contains(err.Error(), "65536") {
		t.Fatalf("low launchd ceiling must fail with actionable EMFILE diagnosis: %v", err)
	}
	if err := validateKimiCLIOpenFileLimit(65536); err != nil {
		t.Fatalf("recommended ceiling must pass: %v", err)
	}
}

func TestKimiCLIEMFILEStderrOverridesVersionMetadata(t *testing.T) {
	stdout := `{"role":"meta","type":"system.version","version":"0.36.1"}`
	stderr := "[unexpected] Error: EMFILE: too many open files, watch\n"
	runErr := errors.New("exit status 1")
	res := parseKimiCLIJSONL(stdout)
	preserveKimiCLIProcessError(res, stderr, runErr)
	got := errorSummary(res, stdout+"\n"+stderr, runErr)
	if !strings.Contains(got, "EMFILE: too many open files, watch") {
		t.Fatalf("last_error must surface concrete stderr root cause, got %q", got)
	}
	if strings.Contains(got, "system.version") {
		t.Fatalf("system.version is metadata and must not mask root cause, got %q", got)
	}
}

func TestInvokeKimiCLI0361PreservesEMFILEStderr(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "kimi")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' '{\"role\":\"meta\",\"type\":\"system.version\",\"version\":\"0.36.1\"}'\n" +
		"printf '%s\\n' '[unexpected] Error: EMFILE: too many open files, watch' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := kimiCLITestConfig(t, bin)
	task := &Task{ID: "kimi-0361-emfile", Model: "opus", Type: typeSequence, Dir: dir}
	res, combined, runErr := invokeKimiCLI(context.Background(), testRoot(t), cfg, task, "read only")
	if runErr == nil || res == nil || !res.IsError ||
		!strings.Contains(errorSummary(res, combined, runErr), "EMFILE: too many open files, watch") {
		t.Fatalf("EMFILE stderr must survive version metadata: res=%+v err=%v\n%s", res, runErr, combined)
	}
	if strings.Contains(combined, "COLD-START TRANSPORT RETRY") {
		t.Fatalf("concrete stderr must not trigger metadata-only retry:\n%s", combined)
	}
}

func TestRunTaskKimiCLILimitQueuesSolFallback(t *testing.T) {
	root := testRoot(t)
	payload := `{"role":"meta","type":"error","content":"HTTP 429: usage limit reached"}`
	bin, _, _ := fakeKimiCLI(t, payload, 1)
	cfg := kimiCLITestConfig(t, bin)
	task := newTask(root, cfg, typeSequence, "kimi limit", t.TempDir(), []string{"p"}, 1)
	task.Model = "opus"
	task.RouteClass = routeClassGeneral
	route, ok := resolveOwnerRoute(cfg, task)
	if !ok || !pinOwnerPrimaryRoute(task, route) {
		t.Fatal("无法钉定 Grok 第一腿")
	}
	task.Runner = grokBuildRunnerName
	if err := queuePolicyFallback(cfg, task, fallbackTransport, v3ProofAuthorization()); err != nil {
		t.Fatal(err)
	}
	task.Runner = kimiCLIRunnerName
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	if err := runTaskVia(context.Background(), root, cfg, task, kimiCLIRunnerName); err != nil {
		t.Fatal(err)
	}
	got, err := loadTask(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusQueued || got.RouteReason != routeReasonKimiToSolPending ||
		got.PreferRunner != "codex" || got.CodexModel != "gpt-5.6-sol" ||
		got.Effort != "xhigh" || !got.EffortExplicit || got.OwnerRouteLeg != 3 ||
		got.SessionID != "" || got.Attempts != 0 {
		t.Fatalf("Kimi 安全限额应串行排队 Sol/xhigh 第三腿: %+v", got)
	}
	cd := loadEngineCooldown(root, kimiCLICooldownName)
	if cd == nil || !cd.active(time.Now()) || cd.UntilEpoch < time.Now().Add(179*time.Minute).Unix() {
		t.Fatalf("应写独立 Kimi CLI 车道冷却约 180 分钟: %+v", cd)
	}
	if _, err := os.Stat(cooldownPath(root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Kimi CLI 限额不得写 Claude 全局冷却: %v", err)
	}
	events := readAllEventsRaw(t, root, task.ID)
	last := events[len(events)-1]
	if last.Type != evRetry || last.Actor != "runner:kimi-cli" ||
		last.Detail["fallback_runner"] != "codex" || last.Detail["fallback_model"] != "gpt-5.6-sol" ||
		last.Detail["fallback_reasoning"] != "xhigh" ||
		last.Detail["workspace_fingerprint_before"] == "" ||
		last.Detail["workspace_fingerprint_before"] != last.Detail["workspace_fingerprint_after"] {
		t.Fatalf("应留下 Kimi CLI→Sol/xhigh 三证接力事件: %+v", last)
	}
}

func TestKimiCLIBoardShowsActualModelEffortAndRoute(t *testing.T) {
	cfg := kimiCLITestConfig(t, "/usr/bin/true")
	now := time.Now()
	queued := &Task{ID: "queued", Status: statusQueued, PreferRunner: "codex", Model: "opus", Effort: "xhigh", Type: typeSequence, Prompts: []string{"前端实现"}}
	brief := toBrief(cfg, queued, now)
	if brief.Runner != grokBuildRunnerName || brief.RunnerSource != "route_policy" ||
		brief.Model != "grok-4.6" || brief.ModelSource != "grok_model" ||
		brief.Effort != "xhigh" || brief.EffortSource != "grok_effort" || brief.RouteReason != routeReasonGrokOpusGeneral ||
		!strings.Contains(brief.ModelRoute, "Grok Build 4.6/xhigh →（仅安全失败）Kimi CLI K3/max") {
		t.Fatalf("未派发 Opus/general 卡必须显示 Grok→Kimi 开头的全路由: %+v", brief)
	}

	backend := &Task{ID: "backend", Status: statusQueued, PreferRunner: "codex", Model: "opus", Effort: "xhigh", Type: typeSequence, RouteClass: routeClassBackend, Prompts: []string{"后端实现"}}
	brief = toBrief(cfg, backend, now)
	if brief.Runner != grokBuildRunnerName || brief.Model != "grok-4.6" || brief.Effort != "xhigh" || brief.RouteReason != routeReasonGrokOpusBackend {
		t.Fatalf("后端 Opus 必须显示 Grok/xhigh 主腿: %+v", brief)
	}
}

func TestValidateKimiCLIConfig(t *testing.T) {
	cfg := kimiCLITestConfig(t, "/usr/bin/true")
	if err := validateKimiCLI(cfg); err != nil {
		t.Fatalf("合法 Kimi CLI 策略被拒: %v", err)
	}
	missingGrok := kimiCLITestConfig(t, "/usr/bin/true")
	missingGrok.GrokBuild = nil
	if err := validateKimiCLI(missingGrok); err == nil {
		t.Fatal("Kimi 自动路由缺 Grok 下一腿必须 fail-fast")
	}
	cfg.KimiCLIOpus.Effort = "ultra"
	if err := validateKimiCLI(cfg); err == nil {
		t.Fatal("非法 Kimi effort 必须 fail-fast")
	}
}

func TestKimiCLIRealCanary(t *testing.T) {
	if os.Getenv("CARDEX_KIMI_REAL_CANARY") != "1" {
		t.Skip("set CARDEX_KIMI_REAL_CANARY=1 for the authenticated integration canary")
	}
	bin := strings.TrimSpace(os.Getenv("CARDEX_KIMI_BIN"))
	home := strings.TrimSpace(os.Getenv("CARDEX_KIMI_HOME"))
	if bin == "" || home == "" {
		t.Fatal("CARDEX_KIMI_BIN and CARDEX_KIMI_HOME are required")
	}
	cfg := defaultConfig("")
	cfg.KimiCLIBin = bin
	cfg.KimiCLIHome = home
	cfg.KimiCLIModel = "kimi-code/k3"
	cfg.KimiCLIEffort = "max"
	cfg.StepTimeoutMin = 2
	task := &Task{
		ID: "kimi-real-canary", Type: typeSequence, Dir: t.TempDir(), Model: "opus",
		Prompts: []string{"run pwd"}, PreferRunner: "codex",
	}
	res, _, err := invokeKimiCLI(context.Background(), testRoot(t), cfg, task,
		"这是 Cardex Kimi CLI 原生执行器测试。运行 pwd，不做任何写入，然后只回复 KIMI_CARDEX_NATIVE_OK。")
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !strings.Contains(res.Result, "KIMI_CARDEX_NATIVE_OK") {
		t.Fatalf("unexpected Kimi result: %+v", res)
	}
}

func TestKimiCLIRealReviewCanary(t *testing.T) {
	if os.Getenv("CARDEX_KIMI_REAL_REVIEW_CANARY") != "1" {
		t.Skip("set CARDEX_KIMI_REAL_REVIEW_CANARY=1 for the authenticated review canary")
	}
	bin := strings.TrimSpace(os.Getenv("CARDEX_KIMI_BIN"))
	home := strings.TrimSpace(os.Getenv("CARDEX_KIMI_HOME"))
	if bin == "" || home == "" {
		t.Fatal("CARDEX_KIMI_BIN and CARDEX_KIMI_HOME are required")
	}
	cfg := defaultConfig("")
	cfg.KimiCLIBin = bin
	cfg.KimiCLIHome = home
	cfg.KimiCLIModel = "kimi-code/k3"
	cfg.KimiCLIEffort = "max"
	cfg.StepTimeoutMin = 2
	task := &Task{
		ID: "kimi-real-review-canary", Type: "review", Dir: t.TempDir(), Model: "opus",
		Prompts: []string{"review pwd"}, PreferRunner: "codex",
	}
	root := testRoot(t)
	res, combined, err := invokeKimiCLI(context.Background(), root, cfg, task,
		"这是 Cardex Kimi CLI 只读复盘执行器测试。运行 pwd，不做任何写入，然后只回复 KIMI_CARDEX_REVIEW_OK。")
	if err != nil {
		t.Fatalf("review canary failed: %v\n%s", err, combined)
	}
	if res == nil || !strings.Contains(res.Result, "KIMI_CARDEX_REVIEW_OK") {
		t.Fatalf("unexpected Kimi review result: %+v", res)
	}
	runtimeConfig, err := os.ReadFile(filepath.Join(root, "kimi-cli-home", "review", "config.toml"))
	if err != nil || !strings.Contains(string(runtimeConfig), `default_plan_mode = true`) {
		t.Fatalf("real review canary did not use configured plan mode: err=%v config=%q", err, runtimeConfig)
	}
}
