package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func grokBuildTestConfig(t *testing.T, bin string) *Config {
	t.Helper()
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".grok"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	cfg := defaultConfig("")
	cfg.DefaultRunner = "codex"
	cfg.CodexBin = "/usr/bin/true"
	cfg.GrokBuildBin = bin
	cfg.KimiCLIBin = "/usr/bin/true"
	cfg.KimiCLIOpus = &KimiCLIOpusRoute{Enabled: true, Model: "kimi-code/k3", Effort: "max"}
	cfg.GrokBuild = &GrokBuildRoute{
		Enabled:               true,
		Model:                 "grok-4.6",
		Effort:                "xhigh",
		LimitFallbackMin:      180,
		KimiOpusFallback:      true,
		FableClaudeFallback:   true,
		FableFirstPrinciples:  true,
		CodexFallbackModel:    "gpt-5.6-sol",
		CodexFallbackEffort:   "xhigh",
		ReviewCodexModel:      "gpt-5.6-sol",
		ReviewCodexEffort:     "max",
		OpusAdversarialReview: true,
		TierRoutes: map[string]GrokTierRoute{
			"opus_backend": {Effort: "xhigh", CodexFallbackModel: "gpt-5.6-sol", CodexFallbackEffort: "max"},
			"sonnet":       {Effort: "high", CodexFallbackModel: "gpt-5.6-luna", CodexFallbackEffort: "max"},
			"haiku":        {Effort: "high", CodexFallbackModel: "gpt-5.6-luna", CodexFallbackEffort: "xhigh"},
		},
	}
	cfg.StepTimeoutMin = 1
	cfg.CooldownMarginSec = 0
	return cfg
}

func TestGrokLifecycleProbeFailsBeforeProviderProcess(t *testing.T) {
	bin, productCalls := fakeGrokBuildCounted(t, `{"type":"end","stopReason":"end_turn"}`, "", 0)
	cfg := grokBuildTestConfig(t, bin)
	stateDir := filepath.Join(os.Getenv("HOME"), ".grok")
	if err := os.Remove(stateDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateDir, []byte("not-a-directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	task := &Task{ID: "grok-lifecycle-denied", Type: typeSequence, Dir: t.TempDir(), PreferRunner: grokBuildRunnerName}
	root := admitDirectInvoke(t, "", task)
	task.LastProviderPreflight = &ProviderPreflightReadback{Runner: grokBuildRunnerName, State: providerReady}
	if _, _, err := invokeGrokBuild(context.Background(), root, cfg, task, "harmless prompt"); err == nil ||
		!strings.Contains(err.Error(), "lifecycle-state directory identity invalid") {
		t.Fatalf("expected lifecycle-state denial before provider, got %v", err)
	}
	if n := countProductCalls(t, productCalls); n != 0 {
		t.Fatalf("provider process started despite lifecycle-state denial: %d", n)
	}
}

func TestGrokLifecycleProbeRejectsRelativeHomeBeforeProviderProcess(t *testing.T) {
	bin, productCalls := fakeGrokBuildCounted(t, `{"type":"end","stopReason":"end_turn"}`, "", 0)
	cfg := grokBuildTestConfig(t, bin)
	t.Setenv("HOME", "relative-home")
	task := &Task{ID: "grok-relative-home", Type: typeSequence, Dir: t.TempDir(), PreferRunner: grokBuildRunnerName}
	root := admitDirectInvoke(t, "", task)
	task.LastProviderPreflight = &ProviderPreflightReadback{Runner: grokBuildRunnerName, State: providerReady}
	if _, _, err := invokeGrokBuild(context.Background(), root, cfg, task, "harmless prompt"); err == nil ||
		!strings.Contains(err.Error(), "home must be an absolute path") {
		t.Fatalf("expected relative HOME denial before provider, got %v", err)
	}
	if n := countProductCalls(t, productCalls); n != 0 {
		t.Fatalf("provider process started with relative HOME: %d", n)
	}
}

func TestGrokLifecycleProbePassesExactHomeToProvider(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "grok")
	homeDump := filepath.Join(dir, "home.txt")
	payload := `{"type":"text","data":"GROK_OK"}` + "\n" +
		`{"type":"end","stopReason":"end_turn","sessionId":"session-grok","num_turns":1}`
	script := "#!/bin/sh\n" +
		"printf '%s' \"$HOME\" > " + shSingleQuote(homeDump) + "\n" +
		"printf '%s\\n' " + shSingleQuote(payload) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := grokBuildTestConfig(t, bin)
	wantHome := os.Getenv("HOME")
	if !filepath.IsAbs(wantHome) {
		t.Fatalf("test HOME must be absolute: %q", wantHome)
	}
	task := &Task{ID: "grok-bound-home", Type: typeSequence, Dir: t.TempDir(), PreferRunner: grokBuildRunnerName}
	root := admitDirectInvoke(t, "", task)
	task.LastProviderPreflight = &ProviderPreflightReadback{Runner: grokBuildRunnerName, State: providerReady}
	if _, _, err := invokeGrokBuild(context.Background(), root, cfg, task, "harmless prompt"); err != nil {
		t.Fatal(err)
	}
	gotHome, err := os.ReadFile(homeDump)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotHome) != wantHome {
		t.Fatalf("provider HOME=%q want exact probe HOME %q", gotHome, wantHome)
	}
	entries, err := os.ReadDir(filepath.Join(wantHome, ".grok"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("lifecycle probe residue: %+v", entries)
	}
}

func TestGrokTierRoutesPinEffortFallbackAndOpusReview(t *testing.T) {
	cfg := grokBuildTestConfig(t, "/usr/bin/true")
	for _, tc := range []struct {
		name           string
		model          string
		routeClass     string
		wantReason     string
		wantEffort     string
		wantFallback   string
		wantFallbackEF string
		wantReview     bool
	}{
		{name: "backend opus", model: "opus", routeClass: routeClassBackend, wantReason: routeReasonGrokOpusBackend, wantEffort: "xhigh", wantFallback: "gpt-5.6-sol", wantFallbackEF: "max", wantReview: true},
		{name: "sonnet", model: "sonnet", wantReason: routeReasonGrokSonnet, wantEffort: "high", wantFallback: "gpt-5.6-luna", wantFallbackEF: "max"},
		{name: "haiku", model: "haiku", wantReason: routeReasonGrokHaiku, wantEffort: "high", wantFallback: "gpt-5.6-luna", wantFallbackEF: "xhigh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := &Task{Type: typeSequence, Model: tc.model, RouteClass: tc.routeClass, PreferRunner: "codex", Prompts: []string{"p"}}
			if !grokBuildAutoRouteApplies(cfg, task) {
				t.Fatal("expected Grok auto route")
			}
			if !pinGrokBuildAutoRoute(cfg, task) {
				t.Fatal("expected Grok route to pin")
			}
			if task.PreferRunner != grokBuildRunnerName || task.GrokModel != "grok-4.6" ||
				task.GrokEffort != tc.wantEffort || task.RouteReason != tc.wantReason || task.ReviewAfter != tc.wantReview {
				t.Fatalf("unexpected primary route: %+v", task)
			}
			if task.OwnerRouteName == "" || task.OwnerRouteLeg != 1 {
				t.Fatalf("generic compatibility route did not preserve its R2 resolver snapshot: %+v", task)
			}
			pinGrokBuildCodexFallback(cfg, task)
			if task.PreferRunner != "codex" || task.CodexModel != tc.wantFallback ||
				task.Effort != tc.wantFallbackEF || !task.EffortExplicit {
				t.Fatalf("unexpected tier fallback: %+v", task)
			}
		})
	}

	ownerCfg := policyTestConfig()
	nonBackendOpus := &Task{Type: typeSequence, Model: "opus", RouteClass: routeClassGeneral, RiskClass: riskClassOrdinary, PreferRunner: "codex", FreshSteps: true, Prompts: []string{"p"}}
	route, ok := resolveOwnerRoute(ownerCfg, nonBackendOpus)
	if !ok || route.Name != "opus_non_backend" || !pinOwnerPrimaryRoute(nonBackendOpus, route) {
		t.Fatalf("non-backend Opus must resolve through the unified Owner route: ok=%v route=%+v task=%+v", ok, route, nonBackendOpus)
	}
	if nonBackendOpus.PreferRunner != grokBuildRunnerName || nonBackendOpus.GrokModel != "grok-4.6" ||
		nonBackendOpus.GrokEffort != "xhigh" || nonBackendOpus.RouteReason != routeReasonGrokOpusGeneral {
		t.Fatalf("non-backend Opus must start on Grok 4.6/xhigh: %+v", nonBackendOpus)
	}
}

func TestGrokOpusAdversarialReviewPinsIndependentSolMax(t *testing.T) {
	cfg := grokBuildTestConfig(t, "/usr/bin/true")
	parent := &Task{Type: typeSequence, Model: "opus", GrokModel: "grok-4.6", ReviewAfter: true, SolMaxAdversarialReview: true}
	review := &Task{Type: typeReview, Model: "claude-opus-5", PreferRunner: "codex"}
	// The obligation and identity are frozen on the parent. A later in-memory config drift must not
	// silently weaken the already-owed independent review (loadConfig rejects this in production).
	cfg.GrokBuild.ReviewCodexModel = "gpt-5.6-luna"
	cfg.GrokBuild.ReviewCodexEffort = "medium"
	pinGrokOpusAdversarialReview(cfg, parent, review)
	if review.PreferRunner != "codex" || review.CodexModel != "gpt-5.6-sol" ||
		review.Effort != "max" || !review.EffortExplicit {
		t.Fatalf("Grok Opus review must be an independent Sol/max card: %+v", review)
	}
}

func TestGrokOpusCompletionSpawnsIndependentSolMaxAdversarialReview(t *testing.T) {
	root := testRoot(t)
	cfg := grokBuildTestConfig(t, "/usr/bin/true")
	parent := newTask(root, cfg, typeSequence, "backend implementation", t.TempDir(), []string{"implement"}, 9)
	parent.Status = statusDone
	parent.Model = "opus"
	parent.RouteClass = routeClassBackend
	parent.Runner = grokBuildRunnerName
	parent.GrokModel = "grok-4.6"
	parent.GrokEffort = "xhigh"
	parent.ReviewAfter = true
	parent.SolMaxAdversarialReview = true
	if err := saveTask(root, parent); err != nil {
		t.Fatal(err)
	}
	postComplete(root, cfg, parent, &claudeResult{Result: "done"}, nil)
	tasks, err := loadBoardTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	var review *Task
	for _, task := range tasks {
		if task.ReviewOf == parent.ID {
			review = task
			break
		}
	}
	if review == nil || review.Type != typeReview || review.PreferRunner != "codex" ||
		review.CodexModel != "gpt-5.6-sol" || review.Effort != "max" || !review.EffortExplicit || review.ReviewAfter {
		t.Fatalf("missing independent adversarial Sol/max review: %+v", review)
	}
}

func TestRunTaskGrokTierLimitQueuesMatchingCodexFallback(t *testing.T) {
	for _, tc := range []struct {
		name       string
		model      string
		routeClass string
		wantModel  string
		wantEffort string
		wantReason string
	}{
		{name: "backend opus", model: "opus", routeClass: routeClassBackend, wantModel: "gpt-5.6-sol", wantEffort: "max", wantReason: routeReasonGrokToSolPending},
		{name: "sonnet", model: "sonnet", wantModel: "gpt-5.6-luna", wantEffort: "max", wantReason: routeReasonGrokSonnetToLunaPending},
		{name: "haiku", model: "haiku", wantModel: "gpt-5.6-luna", wantEffort: "xhigh", wantReason: routeReasonGrokHaikuToLunaPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := testRoot(t)
			bin, _, _ := fakeGrokBuild(t, `{"type":"error","message":"HTTP 429: usage limit reached"}`, "", 1)
			cfg := grokBuildTestConfig(t, bin)
			task := newTask(root, cfg, typeSequence, tc.name, t.TempDir(), []string{"p"}, 1)
			task.Model = tc.model
			task.RouteClass = tc.routeClass
			if !pinGrokBuildAutoRoute(cfg, task) {
				t.Fatal("failed to pin tier route")
			}
			if err := saveTask(root, task); err != nil {
				t.Fatal(err)
			}
			if err := runTaskVia(context.Background(), root, cfg, task, grokBuildRunnerName); err != nil {
				t.Fatal(err)
			}
			got, err := loadTask(root, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != statusQueued || got.PreferRunner != "codex" || got.CodexModel != tc.wantModel ||
				got.Effort != tc.wantEffort || !got.EffortExplicit || got.RouteReason != tc.wantReason {
				t.Fatalf("wrong tier fallback: %+v", got)
			}
		})
	}
}

func TestBoardShowsCurrentGrokTierRoutes(t *testing.T) {
	cfg := grokBuildTestConfig(t, "/usr/bin/true")
	cases := []struct {
		model      string
		routeClass string
		wantEffort string
		wantRoute  string
	}{
		{model: "sonnet", wantEffort: "high", wantRoute: "Grok Build 4.6/high →（仅安全失败）Codex GPT-5.6 Luna/max"},
		{model: "haiku", wantEffort: "high", wantRoute: "Grok Build 4.6/high →（仅安全失败）Codex GPT-5.6 Luna/xhigh"},
		{model: "opus", routeClass: routeClassBackend, wantEffort: "xhigh", wantRoute: "Grok Build 4.6/xhigh →（仅安全失败）Codex GPT-5.6 Sol/max；实现完成后另派 Codex GPT-5.6 Sol/max 独立对抗复审"},
	}
	for _, tc := range cases {
		task := &Task{Type: typeSequence, Status: statusQueued, Model: tc.model, RouteClass: tc.routeClass, PreferRunner: "codex", Prompts: []string{"p"}}
		brief := toBrief(cfg, task, time.Now())
		if brief.Runner != grokBuildRunnerName || brief.Model != "grok-4.6" || brief.Effort != tc.wantEffort || brief.ModelRoute != tc.wantRoute {
			t.Fatalf("unexpected board route for %s/%s: %+v", tc.model, tc.routeClass, brief)
		}
	}

	nonBackend := &Task{Type: typeSequence, Status: statusQueued, Model: "opus", RouteClass: routeClassGeneral, PreferRunner: "codex", Prompts: []string{"p"}}
	brief := toBrief(cfg, nonBackend, time.Now())
	const wantOpusRoute = "Grok Build 4.6/xhigh →（仅安全失败）Kimi CLI K3/max →（仅安全失败）Codex GPT-5.6 Sol/xhigh；进入 Grok 路径且完成实现后另派 Codex GPT-5.6 Sol/max 独立对抗复审"
	if brief.Runner != grokBuildRunnerName || brief.Model != "grok-4.6" || brief.Effort != "xhigh" || brief.ModelRoute != wantOpusRoute {
		t.Fatalf("unexpected non-backend Opus board route: %+v", brief)
	}
}

func fakeGrokBuild(t *testing.T, payload, stderr string, exitCode int) (bin, argsDump, promptDump string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "grok")
	argsDump = filepath.Join(dir, "args.dump")
	promptDump = filepath.Join(dir, "prompt.dump")
	payloadPath := filepath.Join(dir, "result.jsonl")
	if err := os.WriteFile(payloadPath, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$arg\" = 'models' ]; then\n" +
		"    printf '%s\\n' \"$@\" > " + shSingleQuote(argsDump+".probe") + "\n" +
		"    printf '%s\\n' 'You are logged in with grok.com.' 'Available models:' '  * grok-4.6 (default)'\n" +
		"    exit 0\n" +
		"  fi\n" +
		"done\n" +
		"printf '%s\\n' \"$@\" > " + shSingleQuote(argsDump) + "\n" +
		"prev=''\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$prev\" = '--prompt-file' ]; then cp \"$arg\" " + shSingleQuote(promptDump) + "; fi\n" +
		"  prev=\"$arg\"\n" +
		"done\n" +
		"cat " + shSingleQuote(payloadPath) + "\n"
	if stderr != "" {
		script += "printf '%s\\n' " + shSingleQuote(stderr) + " >&2\n"
	}
	script += "exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsDump, promptDump
}

func fakeGrokBuildExpiredAuth(t *testing.T) (bin, probeCalls, productCalls string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "grok")
	probeCalls = filepath.Join(dir, "probe.calls")
	productCalls = filepath.Join(dir, "product.calls")
	script := "#!/bin/sh\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$arg\" = 'models' ]; then\n" +
		"    printf 'probe\\n' >> " + shSingleQuote(probeCalls) + "\n" +
		"    printf '%s\\n' " + shSingleQuote(grokOIDCNoAuthContextClosedMetadata) + " >&2\n" +
		"    exit 1\n" +
		"  fi\n" +
		"done\n" +
		"printf 'product\\n' >> " + shSingleQuote(productCalls) + "\n" +
		"printf '%s\\n' '{\"type\":\"text\",\"data\":\"SHOULD_NOT_RUN\"}' '{\"type\":\"end\",\"stopReason\":\"end_turn\",\"sessionId\":\"unexpected\",\"num_turns\":1}'\n" +
		"exit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, probeCalls, productCalls
}

func TestParseGrokBuildStreamingJSON(t *testing.T) {
	raw := `{"type":"available_commands","tools":[]}` + "\n" +
		`{"type":"thought","data":"reasoning is metadata"}` + "\n" +
		`{"type":"text","data":"GROK_"}` + "\n" +
		`{"type":"tool_call","toolCallId":"call-1","toolName":"read_file","status":"completed"}` + "\n" +
		`{"type":"text","data":"OK"}` + "\n" +
		`{"type":"end","stopReason":"end_turn","sessionId":"session-grok","num_turns":2,"total_cost_usd":0.125,"duration_ms":321,"usage":{"input_tokens":10,"cache_read_input_tokens":3,"cache_creation_input_tokens":2,"output_tokens":4}}`
	res := parseGrokBuildJSONL(raw)
	if res == nil || res.IsError || res.Result != "GROK_OK" || res.SessionID != "session-grok" ||
		res.NumTurns != 2 || res.TotalCostUSD != 0.125 || res.DurationMS != 321 {
		t.Fatalf("unexpected Grok result: %+v", res)
	}
	if res.Usage == nil || res.Usage.InputTokens != 10 || res.Usage.CacheReadInputTokens != 3 ||
		res.Usage.CacheCreationInputTokens != 2 || res.Usage.OutputTokens != 4 {
		t.Fatalf("unexpected Grok usage: %+v", res.Usage)
	}
}

func grokBuildArgIndex(argv []string, flag string) int {
	for i, arg := range argv {
		if arg == flag {
			return i
		}
	}
	return -1
}

func TestInvokeGrokBuildUses46XHighAndSandboxModes(t *testing.T) {
	for _, tc := range []struct {
		name            string
		typ             string
		skipPermissions bool
		sessionID       string
		sandbox         string
		permission      string
		wantNoPlan      bool
	}{
		{name: "implementation", typ: typeSequence, skipPermissions: false, sandbox: "workspace", permission: "auto", wantNoPlan: true},
		{name: "review", typ: typeReview, skipPermissions: false, sandbox: "read-only", permission: "plan", wantNoPlan: false},
		{name: "review skip-permissions", typ: typeReview, skipPermissions: true, sandbox: "workspace", permission: "auto", wantNoPlan: true},
		{name: "coordinate skip-permissions", typ: typeCoordinate, skipPermissions: true, sandbox: "workspace", permission: "auto", wantNoPlan: true},
		{name: "implementation resume", typ: typeSequence, sessionID: "session-grok", sandbox: "workspace", permission: "auto", wantNoPlan: true},
		{name: "crosscheck", typ: typeCrossCheck, skipPermissions: false, sandbox: "read-only", permission: "plan", wantNoPlan: false},
		{name: "crosscheck skip-permissions", typ: typeCrossCheck, skipPermissions: true, sandbox: "read-only", permission: "plan", wantNoPlan: false},
		{name: "crosscheck skip-permissions resume", typ: typeCrossCheck, skipPermissions: true, sessionID: "session-grok", sandbox: "read-only", permission: "plan", wantNoPlan: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := `{"type":"text","data":"GROK_OK"}` + "\n" +
				`{"type":"end","stopReason":"end_turn","sessionId":"session-grok","num_turns":1}`
			bin, argsDump, promptDump := fakeGrokBuild(t, payload, "", 0)
			cfg := grokBuildTestConfig(t, bin)
			task := &Task{
				ID: "grok-invoke", Type: tc.typ, Dir: t.TempDir(), PreferRunner: grokBuildRunnerName,
				SkipPermissions: tc.skipPermissions, SessionID: tc.sessionID,
			}
			if grokBuildWriteCapable(task) != tc.wantNoPlan {
				t.Fatalf("write-capable helper=%v want=%v type=%s skip=%v",
					grokBuildWriteCapable(task), tc.wantNoPlan, tc.typ, tc.skipPermissions)
			}
			root := admitDirectInvoke(t, "", task)
			res, _, err := invokeGrokBuild(context.Background(), root, cfg, task, "harmless prompt")
			if err != nil || res == nil || res.Result != "GROK_OK" || res.SessionID != "session-grok" {
				t.Fatalf("invoke failed: res=%+v err=%v", res, err)
			}
			argsRaw, _ := os.ReadFile(argsDump)
			args := string(argsRaw)
			argv := strings.Split(strings.TrimSuffix(args, "\n"), "\n")
			for _, want := range []string{
				"--no-auto-update\n",
				"--model\ngrok-4.6\n", "--reasoning-effort\nxhigh\n",
				"--output-format\nstreaming-json\n", "--sandbox\n" + tc.sandbox + "\n",
				"--permission-mode\n" + tc.permission + "\n", "--prompt-file\n",
				"--no-memory\n", "--no-subagents\n", "--disable-web-search\n", "--verbatim\n",
			} {
				if !strings.Contains(args, want) {
					t.Fatalf("Grok argv missing %q:\n%s", want, args)
				}
			}
			if hasNoPlan := grokBuildArgIndex(argv, "--no-plan") >= 0; hasNoPlan != tc.wantNoPlan {
				t.Fatalf("Grok argv --no-plan want=%v got=%v:\n%s", tc.wantNoPlan, hasNoPlan, args)
			}
			if strings.Contains(args, "harmless prompt") {
				t.Fatalf("prompt must not be exposed in process argv:\n%s", args)
			}
			gotPrompt, _ := os.ReadFile(promptDump)
			if string(gotPrompt) != "harmless prompt" {
				t.Fatalf("prompt-file mismatch: %q", gotPrompt)
			}
			if len(argv) < 2 || argv[len(argv)-2] != "--prompt-file" {
				t.Fatalf("--prompt-file must be the final flag/value pair:\n%s", args)
			}
			order := []string{"--no-memory", "--no-subagents", "--disable-web-search", "--verbatim"}
			if tc.wantNoPlan {
				order = append(order, "--no-plan")
			}
			if tc.sessionID != "" {
				order = append(order, "--resume")
			} else if grokBuildArgIndex(argv, "--resume") >= 0 {
				t.Fatalf("Grok argv must not include --resume without a session:\n%s", args)
			}
			order = append(order, "--prompt-file")
			prev := -1
			for _, flag := range order {
				i := grokBuildArgIndex(argv, flag)
				if i < 0 {
					t.Fatalf("Grok argv missing %q:\n%s", flag, args)
				}
				if i <= prev {
					t.Fatalf("Grok argv %q must follow previous safety/session flags:\n%s", flag, args)
				}
				prev = i
			}
			if tc.sessionID != "" {
				i := grokBuildArgIndex(argv, "--resume")
				if i+1 >= len(argv) || argv[i+1] != tc.sessionID {
					t.Fatalf("Grok argv --resume value mismatch:\n%s", args)
				}
			}
			probeArgsRaw, _ := os.ReadFile(argsDump + ".probe")
			probeArgs := string(probeArgsRaw)
			probeArgv := strings.Split(strings.TrimSuffix(probeArgs, "\n"), "\n")
			if len(probeArgv) != 2 || probeArgv[0] != "--no-auto-update" || probeArgv[1] != "models" {
				t.Fatalf("Grok auth models probe argv must be exactly --no-auto-update models:\n%s", probeArgs)
			}
			if grokBuildArgIndex(probeArgv, "--no-plan") >= 0 || grokBuildArgIndex(probeArgv, "--prompt-file") >= 0 ||
				grokBuildArgIndex(probeArgv, "--model") >= 0 || strings.Contains(probeArgs, "harmless prompt") {
				t.Fatalf("Grok auth models probe must not start a model session or pass --no-plan/prompt:\n%s", probeArgs)
			}
		})
	}
}

func TestRunTaskGrokExpiredAuthHoldsFirstCardAndOpensEngineCircuit(t *testing.T) {
	root := testRoot(t)
	bin, probeCalls, productCalls := fakeGrokBuildExpiredAuth(t)
	cfg := grokBuildTestConfig(t, bin)
	cfg.MaxAttempts = 3

	first := newTask(root, cfg, typeSequence, "grok expired auth first", t.TempDir(), []string{"p"}, 1)
	first.Model = "opus"
	first.RouteClass = routeClassBackend
	if !pinGrokBuildAutoRoute(cfg, first) {
		t.Fatal("failed to pin Grok backend route")
	}
	if err := saveTask(root, first); err != nil {
		t.Fatal(err)
	}
	if err := runTaskVia(context.Background(), root, cfg, first, grokBuildRunnerName); err != nil {
		t.Fatal(err)
	}
	gotFirst, err := loadTask(root, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotFirst.Status != statusHeld || gotFirst.Attempts != 0 || gotFirst.PreferRunner != grokBuildRunnerName ||
		gotFirst.RouteReason != routeReasonGrokOpusBackend || !strings.HasPrefix(gotFirst.LastError, "[auth]") {
		t.Fatalf("first Grok auth failure must hold without attempts/fallback: %+v", gotFirst)
	}
	cd := loadEngineCooldown(root, grokBuildCooldownName)
	if cd == nil || !cd.active(time.Now()) || !strings.HasPrefix(cd.Reason, "auth: ") {
		t.Fatalf("first Grok auth failure must open engine auth circuit: %+v", cd)
	}
	if _, err := os.Stat(productCalls); !os.IsNotExist(err) {
		t.Fatalf("auth preflight must prevent product invocation, stat err=%v", err)
	}

	second := newTask(root, cfg, typeSequence, "grok circuit follower", t.TempDir(), []string{"p"}, 1)
	second.Model = "opus"
	second.RouteClass = routeClassBackend
	if !pinGrokBuildAutoRoute(cfg, second) {
		t.Fatal("failed to pin second Grok backend route")
	}
	if err := saveTask(root, second); err != nil {
		t.Fatal(err)
	}
	if err := runTaskVia(context.Background(), root, cfg, second, grokBuildRunnerName); err != nil {
		t.Fatal(err)
	}
	gotSecond, err := loadTask(root, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotSecond.Status != statusQueued || gotSecond.Attempts != 0 || gotSecond.PreferRunner != grokBuildRunnerName ||
		gotSecond.RouteReason != routeReasonGrokOpusBackend {
		t.Fatalf("open Grok auth circuit must leave later card queued on same engine: %+v", gotSecond)
	}
	probeRaw, err := os.ReadFile(probeCalls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(probeRaw), "probe\n") != 1 {
		t.Fatalf("auth circuit must suppress repeated probes, calls=%q", probeRaw)
	}
}

func TestGrokBuildAuthProbeSingleFlightsConcurrentFailure(t *testing.T) {
	root := testRoot(t)
	bin, probeCalls, productCalls := fakeGrokBuildExpiredAuth(t)
	cfg := grokBuildTestConfig(t, bin)
	const workers = 8
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- ensureGrokBuildAuth(context.Background(), root, cfg, "grok-4.6")
		}()
	}
	wg.Wait()
	close(errs)
	circuits := 0
	for err := range errs {
		if !isGrokBuildAuthProbeError(err) {
			t.Fatalf("all workers must receive the same auth-class gate, got %v", err)
		}
		if isGrokBuildAuthCircuitError(err) {
			circuits++
		}
	}
	if circuits != workers-1 {
		t.Fatalf("exactly one worker should perform the real probe; circuit followers=%d want=%d", circuits, workers-1)
	}
	probeRaw, err := os.ReadFile(probeCalls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(probeRaw), "probe\n") != 1 {
		t.Fatalf("concurrent auth failure must execute one CLI probe, calls=%q", probeRaw)
	}
	if _, err := os.Stat(productCalls); !os.IsNotExist(err) {
		t.Fatalf("single-flight auth probe must not start a product process, stat err=%v", err)
	}
}

func TestRefreshGrokBuildAuthClearsOnlyAuthCircuit(t *testing.T) {
	root := testRoot(t)
	bin, _, _ := fakeGrokBuild(t, "", "", 0)
	cfg := grokBuildTestConfig(t, bin)
	now := time.Now()
	setEngineCooldown(root, grokBuildCooldownName, now.Add(time.Hour).Unix(), "auth: expired login")
	if err := refreshGrokBuildAuth(context.Background(), root, cfg, "grok-4.6"); err != nil {
		t.Fatal(err)
	}
	if cd := loadEngineCooldown(root, grokBuildCooldownName); cd != nil {
		t.Fatalf("successful live probe must clear the auth circuit: %+v", cd)
	}

	const quotaReason = "HTTP 429: usage limit reached"
	setEngineCooldown(root, grokBuildCooldownName, now.Add(time.Hour).Unix(), quotaReason)
	if err := refreshGrokBuildAuth(context.Background(), root, cfg, "grok-4.6"); err != nil {
		t.Fatal(err)
	}
	cd := loadEngineCooldown(root, grokBuildCooldownName)
	if cd == nil || cd.Reason != quotaReason || !cd.active(time.Now()) {
		t.Fatalf("auth refresh must preserve a quota cooldown: %+v", cd)
	}
}

func TestGrokBuildProbeDiagnosticRedactsSecrets(t *testing.T) {
	diagnostic := safeGrokBuildProbeDiagnostic("Authorization: Bearer super-secret-token", nil)
	if strings.Contains(diagnostic, "super-secret-token") || !strings.Contains(diagnostic, "<redacted>") {
		t.Fatalf("preflight diagnostic must not expose credentials: %q", diagnostic)
	}
}

func TestValidateGrokBuildRejectsUnsupportedMax(t *testing.T) {
	cfg := grokBuildTestConfig(t, "/usr/bin/true")
	if err := validateGrokBuild(cfg); err != nil {
		t.Fatalf("valid Grok config rejected: %v", err)
	}
	cfg.GrokBuild.Effort = "max"
	if err := validateGrokBuild(cfg); err == nil || !strings.Contains(err.Error(), "xhigh") {
		t.Fatalf("Grok 4.6 max must fail fast with xhigh guidance: %v", err)
	}
}

func TestRunTaskKimiLimitHoldsWithoutGlobalSolFallback(t *testing.T) {
	root := testRoot(t)
	kimiBin, _, _ := fakeKimiCLI(t, `{"role":"meta","type":"error","content":"HTTP 429: usage limit reached"}`, 1)
	cfg := kimiCLITestConfig(t, kimiBin)
	cfg.OwnerRoutingEnforced = true
	grokBin, _, _ := fakeGrokBuild(t, "", "", 0)
	cfg.GrokBuildBin = grokBin
	cfg.GrokBuild = grokBuildTestConfig(t, grokBin).GrokBuild
	task := newTask(root, cfg, typeSequence, "kimi to sol", t.TempDir(), []string{"p"}, 1)
	task.Model = "opus"
	task.RouteClass = routeClassGeneral
	route, ok := resolveOwnerRoute(cfg, task)
	if !ok || !pinOwnerPrimaryRoute(task, route) {
		t.Fatal("failed to pin general Opus primary")
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
	if got.Status != statusHeld || got.PreferRunner != kimiCLIRunnerName || got.CodexModel != "" ||
		got.RouteReason != routeReasonGrokToKimi || got.OwnerRouteLeg != 2 || got.SessionID != "" || got.Attempts != 0 ||
		!strings.Contains(got.LastError, "global Codex fallback disabled") {
		t.Fatalf("Kimi limit must hold on the second leg without a global Sol fallback: %+v", got)
	}
	if got.LastRouteAttempt == nil || got.LastRouteAttempt.ActualProvider != kimiCLIRunnerName ||
		got.LastRouteAttempt.ActualModel != "kimi-code/k3" || got.LastRouteAttempt.ActualEffort != "max" ||
		got.LastRouteAttempt.OwnerRouteLeg != 2 || got.LastRouteAttempt.FailureKind != string(fallbackQuota) {
		t.Fatalf("Kimi attempt identity/readback incomplete: %+v", got.LastRouteAttempt)
	}
}

func TestRunTaskGrokLimitQueuesKimiSecond(t *testing.T) {
	root := testRoot(t)
	bin, _, _ := fakeGrokBuild(t, `{"type":"error","message":"HTTP 429: usage limit reached"}`, "", 1)
	cfg := policyTestConfig()
	cfg.GrokBuildBin = bin
	task := newTask(root, cfg, typeSequence, "grok to kimi", t.TempDir(), []string{"p"}, 1)
	task.Model = "opus"
	task.RouteClass = routeClassGeneral
	route, ok := resolveOwnerRoute(cfg, task)
	if !ok || !pinOwnerPrimaryRoute(task, route) {
		t.Fatal("failed to pin general Opus primary")
	}
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	if err := runTaskVia(context.Background(), root, cfg, task, grokBuildRunnerName); err != nil {
		t.Fatal(err)
	}
	got, err := loadTask(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusQueued || got.PreferRunner != kimiCLIRunnerName || got.KimiModel != "kimi-code/k3" ||
		got.Effort != "max" || !got.EffortExplicit || got.RouteReason != routeReasonGrokToKimiPending || got.OwnerRouteLeg != 2 {
		t.Fatalf("Grok limit must queue Kimi K3/max as the second leg: %+v", got)
	}
	cd := loadEngineCooldown(root, grokBuildCooldownName)
	if cd == nil || !cd.active(time.Now()) {
		t.Fatalf("Grok limit must set an independent cooldown: %+v", cd)
	}
	events, _, err := loadTaskEvents(root, task.ID)
	if err != nil || len(events) == 0 {
		t.Fatalf("load Grok fallback receipt: events=%d err=%v", len(events), err)
	}
	last := events[len(events)-1]
	if last.Type != evRetry || last.Actor != "runner:grok-build" ||
		last.Detail["fallback_runner"] != kimiCLIRunnerName || last.Detail["fallback_model"] != "kimi-code/k3" ||
		last.Detail["fallback_reasoning"] != "max" ||
		last.Detail["requested_provider"] != grokBuildRunnerName || last.Detail["actual_provider"] != grokBuildRunnerName ||
		last.Detail["requested_model"] != "grok-4.6" || last.Detail["actual_model"] != "grok-4.6" ||
		last.Detail["requested_effort"] != "xhigh" || last.Detail["actual_effort"] != "xhigh" ||
		last.Detail["owner_route_name"] != "opus_non_backend" || last.Detail["owner_route_leg"] != float64(1) ||
		last.Detail["attempt"] != float64(0) || last.Detail["failure_kind"] != string(fallbackQuota) ||
		last.Detail["workspace_fingerprint_before"] == "" ||
		last.Detail["workspace_fingerprint_before"] != last.Detail["workspace_fingerprint_after"] ||
		last.Detail["process_residue"] != false {
		t.Fatalf("Grok fallback must persist the complete safety receipt: %+v", last)
	}
}

func TestLegacyClaudeFableLimitDoesNotCreateNewOwnerFallback(t *testing.T) {
	root := testRoot(t)
	claudeBin := fakeClaudeBin(t, "You've reached your usage limit", "", 1)
	grokBin, _, _ := fakeGrokBuild(t, "", "", 0)
	cfg := grokBuildTestConfig(t, grokBin)
	cfg.ClaudeBin = claudeBin
	task := newTask(root, cfg, typeCoordinate, "fable design", t.TempDir(), []string{"design"}, 1)
	task.Model = "claude-fable-5"
	task.PreferRunner = "claude"
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	if err := runTaskVia(context.Background(), root, cfg, task, ""); err != nil {
		t.Fatal(err)
	}
	got, err := loadTask(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusLimitPaused || got.PreferRunner != "claude" ||
		got.RouteReason != "" || got.FableFirstPrinciplesReview || got.GrokModel != "" {
		t.Fatalf("legacy Claude-Fable fields must not create a new owner fallback chain: %+v", got)
	}
	if _, err := os.Stat(cooldownPath(root)); err != nil {
		t.Fatalf("Claude limit must retain the global Claude cooldown: %v", err)
	}
}

func TestFableFallbackSpawnsExactlyOneFirstPrinciplesSolAudit(t *testing.T) {
	root := testRoot(t)
	cfg := grokBuildTestConfig(t, "/usr/bin/true")
	parent := newTask(root, cfg, typeCoordinate, "关键设计方案", t.TempDir(), []string{"设计"}, 1)
	parent.Status = statusDone
	parent.Project = "Cardex"
	parent.Model = "claude-fable-5"
	parent.Runner = grokBuildRunnerName
	parent.RouteReason = routeReasonFableToGrok
	parent.FableFirstPrinciplesReview = true
	if err := saveTask(root, parent); err != nil {
		t.Fatal(err)
	}
	result := "候选方案：沿用现有方向。"
	postComplete(root, cfg, parent, &claudeResult{Result: result}, nil)
	postComplete(root, cfg, parent, &claudeResult{Result: result}, nil)

	tasks, err := loadBoardTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	var audits []*Task
	for _, task := range tasks {
		if task.ReviewOf == parent.ID && task.AdvisoryReview {
			audits = append(audits, task)
		}
	}
	if len(audits) != 1 {
		t.Fatalf("expected exactly one first-principles audit, got %d: %+v", len(audits), audits)
	}
	audit := audits[0]
	if audit.PreferRunner != "codex" || audit.CodexModel != "gpt-5.6-sol" ||
		audit.Effort != "max" || !audit.EffortExplicit || audit.RemoteHost != "" || audit.ReviewAfter {
		t.Fatalf("audit must be local Sol/max advisory-only: %+v", audit)
	}
	prompt := strings.Join(audit.Prompts, "\n")
	for _, phrase := range []string{"第一性原理", "不预设", "从零重建", "盲点"} {
		if !strings.Contains(prompt, phrase) {
			t.Fatalf("audit prompt missing %q:\n%s", phrase, prompt)
		}
	}

	// Advisory audit output is evidence for the owner, not a verdict consumed by the
	// implementation -> review -> auto-fix loop.
	postComplete(root, cfg, audit, &claudeResult{Result: "```json\n{\"verdict\":\"block\",\"p1\":[\"alternative\"]}\n```"}, nil)
	tasks, err = loadBoardTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if strings.HasPrefix(task.Title, "修复R") {
			t.Fatalf("advisory audit must not enter auto-fix loop: %+v", task)
		}
	}
}

func TestGrokLimitDetectorIgnoresSuccessfulDiscussion(t *testing.T) {
	res := &claudeResult{Result: "The design discusses rate limit handling."}
	if isLimitHitGrokBuild(res, `{"type":"text","data":"rate limit handling"}`) {
		t.Fatal("successful prose must not be classified as Grok quota")
	}
	res = parseGrokBuildJSONL(`{"type":"error","message":"quota exhausted"}`)
	if !isLimitHitGrokBuild(res, `{"type":"error","message":"quota exhausted"}`) {
		t.Fatal("Grok error event must be classified as quota")
	}
	if got := grokBuildResetEpoch(grokBuildTestConfig(t, "/usr/bin/true"), res, "quota exhausted", time.Now()); got <= time.Now().Unix() {
		t.Fatalf("reset epoch must be in the future: %d", got)
	}
}

func TestGrokBuildBoardShowsActualModelEffortAndRelay(t *testing.T) {
	cfg := grokBuildTestConfig(t, "/usr/bin/true")
	toKimi := &Task{
		ID: "to-kimi", Status: statusQueued, Runner: grokBuildRunnerName, PreferRunner: kimiCLIRunnerName,
		Model: "opus", RouteClass: routeClassGeneral, KimiModel: "kimi-code/k3", Effort: "max", EffortExplicit: true,
		RouteReason: routeReasonGrokToKimiPending, OwnerRouteName: "opus_general", OwnerRouteLeg: 2,
	}
	brief := toBrief(cfg, toKimi, time.Now())
	if brief.Runner != kimiCLIRunnerName || brief.RunnerSource != "route_reason" ||
		brief.Model != "kimi-code/k3" || brief.ModelSource != "kimi_model" ||
		brief.Effort != "max" || brief.EffortSource != "kimi_effort" {
		t.Fatalf("Grok→Kimi relay must display its next actual runner/model: %+v", brief)
	}

	toSol := *toKimi
	toSol.Runner = kimiCLIRunnerName
	toSol.PreferRunner = "codex"
	toSol.KimiModel = ""
	toSol.CodexModel = "gpt-5.6-sol"
	toSol.Effort = "xhigh"
	toSol.EffortExplicit = true
	toSol.RouteReason = routeReasonKimiToSolPending
	toSol.OwnerRouteLeg = 3
	brief = toBrief(cfg, &toSol, time.Now())
	if brief.Runner != "codex" || brief.Model != "gpt-5.6-sol" || brief.Effort != "xhigh" {
		t.Fatalf("Kimi→Sol relay must display Sol/xhigh as next actual route: %+v", brief)
	}
}

func TestGrokNativeRunnerNameIsReservedFromEngineProfiles(t *testing.T) {
	if engineVia(grokBuildRunnerName) || engineVia("claude") {
		t.Fatal("native Grok and explicit Claude sentinels must not be mistaken for engine profiles")
	}
	reserved := defaultConfig("")
	reserved.Engines = map[string]EngineProfile{grokBuildRunnerName: {BaseURL: "https://example.invalid"}}
	if err := validateEngines(reserved); err == nil {
		t.Fatal("grok-build must be reserved from engine profile name collisions")
	}
}
