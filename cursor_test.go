package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func cursorTestConfig() *Config {
	cfg := defaultConfig("/usr/bin/true")
	cfg.DefaultRunner = "codex"
	cfg.CodexBin = "/usr/bin/true"
	cfg.CodexModel = "gpt-5.6-sol"
	cfg.CodexReasoning = "xhigh"
	cfg.GrokBuildBin = "/usr/bin/true"
	cfg.GrokBuild = &GrokBuildRoute{Enabled: true, Model: "grok-4.6", Effort: "xhigh"}
	cfg.CursorBin = "/usr/bin/true"
	cfg.CursorModel = "cursor-grok-4.6-xhigh"
	cfg.CursorFable = &CursorFableRoute{
		Enabled: true, Model: "claude-fable-5-thinking-max", LimitFallbackMin: 180,
		FallbackProfile: "fable-dual",
	}
	cfg.CrossProfiles = map[string]CrossProfile{
		"fable-dual": {
			A:     CrossEngine{Kind: grokBuildRunnerName, Model: "grok-4.6", Effort: "xhigh", Label: "grok-4.6/xhigh"},
			B:     CrossEngine{Kind: "codex", Effort: "ultra", Label: "gpt-5.6-sol/ultra"},
			Merge: &CrossEngine{Kind: "codex", Effort: "max", Label: "gpt-5.6-sol/max first-principles merge"},
		},
	}
	return cfg
}

func verifiedCursorFallbackAuth(t *testing.T) fallbackAuthorization {
	t.Helper()
	auth, err := authorizePolicyFallback(safeFixtureProof(t))
	if err != nil {
		t.Fatal(err)
	}
	return auth
}

func fakeCursorAgent(t *testing.T, stdout string, stderr string, exit int) (string, string) {
	t.Helper()
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "cursor-agent")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + argsPath + "\"\n"
	if stdout != "" {
		script += "printf '%s\\n' '" + strings.ReplaceAll(stdout, "'", "'\\''") + "'\n"
	}
	if stderr != "" {
		script += "printf '%s\\n' '" + strings.ReplaceAll(stderr, "'", "'\\''") + "' >&2\n"
	}
	script += "exit " + string(rune('0'+exit)) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsPath
}

func TestParseCursorJSONLRequiresSemanticCompletion(t *testing.T) {
	metaOnly := parseCursorJSONL(`{"type":"system","subtype":"init","session_id":"meta-session","model":"Claude Fable 5"}`)
	if metaOnly == nil || !metaOnly.IsError || !strings.Contains(metaOnly.Result, "终局") {
		t.Fatalf("system init metadata must not count as completion: %+v", metaOnly)
	}

	raw := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"cursor-session","model":"Cursor Grok 4.6 Extra High"}`,
		`{"type":"thinking","subtype":"completed","session_id":"cursor-session"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"CURSOR_OK"}]},"session_id":"cursor-session"}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"CURSOR_OK","session_id":"cursor-session","duration_ms":321,"usage":{"inputTokens":10,"outputTokens":4,"cacheReadTokens":3,"cacheWriteTokens":2}}`,
	}, "\n")
	res := parseCursorJSONL(raw)
	if res == nil || res.IsError || res.Result != "CURSOR_OK" || res.SessionID != "cursor-session" || res.NumTurns != 1 {
		t.Fatalf("unexpected cursor result: %+v", res)
	}
	if res.Usage == nil || res.Usage.InputTokens != 10 || res.Usage.OutputTokens != 4 ||
		res.Usage.CacheReadInputTokens != 3 || res.Usage.CacheCreationInputTokens != 2 {
		t.Fatalf("unexpected cursor usage: %+v", res.Usage)
	}
}

func TestParseCursorJSONLCountsToolResultInUserEnvelope(t *testing.T) {
	res := parseCursorJSONL(strings.Join([]string{
		`{"type":"user","message":{"content":[{"type":"tool_result","text":"done"}]}}`,
		`{"type":"error","error":"connection reset"}`,
	}, "\n"))
	if res.ToolEvents != 1 || !res.ObservationComplete {
		t.Fatalf("tool_result must block a presemantic fallback even in a user envelope: %+v", res)
	}
}

func TestParseCursorJSONLUnknownUserEnvelopeFailsProofClosed(t *testing.T) {
	res := parseCursorJSONL(strings.Join([]string{
		`{"type":"user","message":"unrecognized envelope"}`,
		`{"type":"error","error":"connection reset"}`,
	}, "\n"))
	if res.ObservationComplete {
		t.Fatalf("an unparsed user envelope must not be treated as proof of zero tool events: %+v", res)
	}
}

func TestInvokeCursorUsesReadOnlyAskAndNeverAutoReview(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"cursor-session"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"CURSOR_OK"}]},"session_id":"cursor-session"}`,
		`{"type":"result","subtype":"success","result":"CURSOR_OK","session_id":"cursor-session"}`,
	}, "\n")
	bin, argsPath := fakeCursorAgent(t, stream, "", 0)
	cfg := cursorTestConfig()
	cfg.CursorBin = bin
	task := &Task{ID: "cursor-invoke", Type: typeReview, Dir: t.TempDir(), CursorModel: "cursor-grok-4.6-xhigh"}
	res, _, err := invokeCursor(context.Background(), cfg, task, "review this")
	if err != nil || res == nil || res.IsError || res.Result != "CURSOR_OK" {
		t.Fatalf("cursor invoke failed: res=%+v err=%v", res, err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	got := "\n" + string(args) + "\n"
	for _, want := range []string{
		"\n--print\n", "\n--trust\n", "\n--output-format\nstream-json\n",
		"\n--model\ncursor-grok-4.6-xhigh\n", "\n--workspace\n" + task.Dir + "\n",
		"\n--sandbox\nenabled\n", "\n--mode\nask\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cursor argv missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"--auto-review", "--force", "--yolo"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("cursor argv must not enable %s:\n%s", forbidden, got)
		}
	}
}

func TestValidateCursorFableUsesOnlyProvenEfforts(t *testing.T) {
	cfg := cursorTestConfig()
	if err := validateCursor(cfg); err != nil {
		t.Fatalf("proven Cursor route should validate: %v", err)
	}
	bad := cursorTestConfig()
	bad.CrossProfiles["fable-dual"] = CrossProfile{
		A:     bad.CrossProfiles["fable-dual"].A,
		B:     bad.CrossProfiles["fable-dual"].B,
		Merge: &CrossEngine{Kind: "codex", Effort: "xhigh"},
	}
	if err := validateCursor(bad); err == nil || !strings.Contains(err.Error(), "Sol/max") {
		t.Fatalf("non-max Sol merger must fail fast, got %v", err)
	}
}

func TestCursorFableFallbackBuildsGrokSolAndIndependentMerge(t *testing.T) {
	root := testRoot(t)
	cfg := cursorTestConfig()
	task := newTask(root, cfg, typeCoordinate, "hard decision", t.TempDir(), []string{"decide from first principles"}, 9)
	task.Model = "fable"
	task.PreferRunner = "codex"
	if err := prepareCursorFableFallback(root, cfg, task, "cursor quota exhausted", fallbackQuota, fallbackAuthorization{}); err == nil {
		t.Fatal("Cursor next-writer chain must reject a missing post-invocation authorization")
	}
	if err := prepareCursorFableFallback(root, cfg, task, "cursor quota exhausted", fallbackQuota, verifiedCursorFallbackAuth(t)); err != nil {
		t.Fatal(err)
	}
	if task.XRole != "A" || task.PreferRunner != grokBuildRunnerName || task.GrokModel != "grok-4.6" || task.GrokEffort != "xhigh" {
		t.Fatalf("A must be Grok/xhigh: %+v", task)
	}
	if task.XEngineB == nil || task.XEngineB.PreferRunner != "codex" || task.XEngineB.CodexModel != "gpt-5.6-sol" || task.XEngineB.Effort != "ultra" {
		t.Fatalf("B must be independent Sol/ultra: %+v", task.XEngineB)
	}
	if task.XEngineC == nil || task.XEngineC.PreferRunner != "codex" || task.XEngineC.CodexModel != "gpt-5.6-sol" || task.XEngineC.Effort != "max" {
		t.Fatalf("C must be a distinct Sol/max first-principles merger: %+v", task.XEngineC)
	}
	if task.ReviewAfter || task.FableFirstPrinciplesReview {
		t.Fatalf("fallback cross chain must not spawn a second review loop: %+v", task)
	}

	if err := writeCrossPeer(root, task.XKey, "A answer"); err != nil {
		t.Fatal(err)
	}
	b := newTask(root, cfg, typeCrossCheck, "交叉B[fable-dual]: hard decision", task.Dir, []string{"solo"}, task.Priority)
	b.XRole, b.XKey, b.XProfile, b.XTask = "B", task.XKey, task.XProfile, task.XTask
	b.XEngineB, b.XEngineC = task.XEngineB, task.XEngineC
	applyFrozenEngine(b, task.XEngineB)
	handleCrossStage(root, cfg, b, &claudeResult{Result: "B answer"}, nil)
	tasks, err := loadTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	var c *Task
	for _, candidate := range tasks {
		if candidate.XKey == task.XKey && candidate.XRole == "C" {
			c = candidate
			break
		}
	}
	if c == nil || c.PreferRunner != "codex" || c.XCodexModel != "gpt-5.6-sol" || c.Effort != "max" {
		t.Fatalf("C must use the separately frozen merger: %+v", c)
	}
	if !strings.Contains(c.Prompts[0], "A answer") || !strings.Contains(c.Prompts[0], "B answer") {
		t.Fatalf("merge prompt must contain both independent answers: %q", c.Prompts[0])
	}
}

func TestCursorFableFallbackAcceptsOnlyConfirmedQuota(t *testing.T) {
	for _, kind := range []fallbackFailureKind{
		fallbackTransport, fallbackStreamIncomplete, fallbackSemanticStall,
		fallbackInvalidTerminal, fallbackExecutionEnv,
	} {
		t.Run(string(kind), func(t *testing.T) {
			root := testRoot(t)
			cfg := cursorTestConfig()
			task := newTask(root, cfg, typeCoordinate, "hard decision", t.TempDir(), []string{"decide"}, 9)
			task.Model = "fable"
			task.PreferRunner = "codex"
			if err := prepareCursorFableFallback(root, cfg, task, string(kind), kind, verifiedCursorFallbackAuth(t)); err == nil ||
				!strings.Contains(err.Error(), "quota-only") {
				t.Fatalf("non-quota Fable terminal must remain held: %v", err)
			}
		})
	}
	root := testRoot(t)
	cfg := cursorTestConfig()
	task := newTask(root, cfg, typeCoordinate, "hard decision", t.TempDir(), []string{"decide"}, 9)
	task.Model = "fable"
	task.PreferRunner = "codex"
	if err := prepareCursorFableFallback(root, cfg, task, "confirmed quota", fallbackQuota, verifiedCursorFallbackAuth(t)); err != nil {
		t.Fatalf("confirmed eligible quota must create the bounded answer/merge chain: %v", err)
	}
}

func TestBoardShowsCursorFablePrimaryAndCompleteFallbackRoute(t *testing.T) {
	root := testRoot(t)
	cfg := cursorTestConfig()
	task := newTask(root, cfg, typeCoordinate, "hard decision", t.TempDir(), []string{"decide from first principles"}, 9)
	task.Model = "fable"
	task.PreferRunner = "codex"

	brief := toBrief(cfg, task, time.Now())
	const wantRoute = "Cursor Fable 5 Thinking Max → [仅确认 eligible quota-limit 后: Grok Build 4.6/xhigh 独立作答 → Codex GPT-5.6 Sol/ultra 独立作答] → Codex GPT-5.6 Sol/max 第一性合并（不预设原方案正确）"
	if brief.Runner != cursorRunnerName || brief.Model != "claude-fable-5-thinking-max" || brief.Effort != "max" {
		t.Fatalf("queued Fable card must show its effective Cursor primary: %+v", brief)
	}
	if brief.ModelRoute != wantRoute {
		t.Fatalf("queued Fable card route=%q, want %q", brief.ModelRoute, wantRoute)
	}

	if err := prepareCursorFableFallback(root, cfg, task, "cursor quota exhausted", fallbackQuota, verifiedCursorFallbackAuth(t)); err != nil {
		t.Fatal(err)
	}
	brief = toBrief(cfg, task, time.Now())
	if brief.Model != "grok-4.6" || brief.Effort != "xhigh" {
		t.Fatalf("fallback A card must continue showing the current effective leg: %+v", brief)
	}
	if brief.ModelRoute != wantRoute {
		t.Fatalf("fallback chain must retain the complete route, got %q", brief.ModelRoute)
	}
	app, err := boardWeb.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(app), "model_route") || !strings.Contains(string(app), "完整模型链") {
		t.Fatal("board UI must visibly consume the complete model route")
	}
}

func TestBoardShowsUnsupportedFableAsPolicyWaitNotGenericCodex(t *testing.T) {
	for name, fixture := range map[string]struct {
		cfg  *Config
		task *Task
	}{
		"multi-step": {cfg: cursorTestConfig(), task: &Task{Type: typeSequence, Model: "fable", PreferRunner: "codex", FreshSteps: true,
			Status: statusQueued, Prompts: []string{"p1", "p2"}}},
		"route disabled": {cfg: func() *Config { c := cursorTestConfig(); c.CursorFable.Enabled = false; return c }(),
			task: &Task{Type: typeSequence, Model: "fable", PreferRunner: "codex", FreshSteps: true,
				Status: statusQueued, Prompts: []string{"p"}}},
	} {
		t.Run(name, func(t *testing.T) {
			brief := toBrief(fixture.cfg, fixture.task, time.Now())
			if brief.Runner != cursorRunnerName || brief.Model != "claude-fable-5-thinking-max" ||
				brief.RouteReason != routeReasonCursorFablePolicyWait || !strings.Contains(brief.ModelRoute, "政策等待") {
				t.Fatalf("unsupported explicit Fable must read back as a fail-closed owner-policy wait: %+v", brief)
			}
		})
	}
}

func TestCursorQuotaAndPolicyGateAreErrorOnlySignals(t *testing.T) {
	success := &claudeResult{Result: "the prose says quota exceeded", IsError: false}
	if isLimitHitCursor(success, "") || isCursorDataPolicyGate(success, "") {
		t.Fatal("successful semantic prose must not trigger Cursor fallback")
	}
	failure := &claudeResult{IsError: true, Result: "HTTP 429: usage limit reached"}
	if !isLimitHitCursor(failure, "") {
		t.Fatal("Cursor quota error should be detected")
	}
	if !isCursorDataPolicyGate(nil, "ActionRequiredError: Review Data Policy") {
		t.Fatal("Cursor Fable data-policy gate should be detected separately")
	}
}

func TestRunTaskCursorFableQuotaAtomicallyEntersFallbackChain(t *testing.T) {
	bin, _ := fakeCursorAgent(t,
		`{"type":"system","subtype":"init","session_id":"presemantic","model":"Claude Fable 5 300K Max"}`,
		"HTTP 429: usage limit reached", 1)
	root := testRoot(t)
	cfg := cursorTestConfig()
	cfg.CursorBin = bin
	cfg.StepTimeoutMin = 1
	task := newTask(root, cfg, typeCoordinate, "hard decision", t.TempDir(), []string{"decide safely"}, 8)
	task.Model = "fable"
	task.PreferRunner = "codex"
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	if err := runTaskVia(context.Background(), root, cfg, task, cursorRunnerName); err != nil {
		t.Fatal(err)
	}
	got, err := findTask(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusQueued || got.XRole != "A" || got.PreferRunner != grokBuildRunnerName ||
		got.XEngineB == nil || got.XEngineC == nil || got.Step != 0 || got.SessionID != "" {
		t.Fatalf("quota fallback must persist one complete fresh cross chain: %+v", got)
	}
	if cd := loadEngineCooldown(root, cursorCooldownName); cd == nil || !cd.active(time.Now()) {
		t.Fatalf("real Cursor quota must cool only the Cursor lane: %+v", cd)
	}
	events, _, err := loadTaskEvents(root, task.ID)
	if err != nil || len(events) == 0 {
		t.Fatalf("load fallback receipt: events=%d err=%v", len(events), err)
	}
	last := events[len(events)-1]
	if last.Type != evRetry || last.Actor != "runner:cursor" ||
		last.Detail["workspace_fingerprint_before"] == "" ||
		last.Detail["workspace_fingerprint_before"] != last.Detail["workspace_fingerprint_after"] ||
		last.Detail["process_residue"] != false {
		t.Fatalf("Cursor fallback must persist the complete safety receipt: %+v", last)
	}
}

func TestRunTaskCursorFableNonQuotaStaysHeldWithoutFallback(t *testing.T) {
	bin, _ := fakeCursorAgent(t,
		`{"type":"system","subtype":"init","session_id":"presemantic","model":"Claude Fable 5 300K Max"}`,
		"connection reset by peer", 1)
	root := testRoot(t)
	cfg := cursorTestConfig()
	cfg.CursorBin = bin
	task := newTask(root, cfg, typeCoordinate, "hard decision", t.TempDir(), []string{"decide safely"}, 8)
	task.Model = "fable"
	task.PreferRunner = "codex"
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	if err := runTaskVia(context.Background(), root, cfg, task, cursorRunnerName); err != nil {
		t.Fatal(err)
	}
	got, err := findTask(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusHeld || got.XRole != "" || got.Attempts != 0 || got.PreferRunner != "codex" {
		t.Fatalf("non-quota Fable terminal must hold the original card without answer/merge legs: %+v", got)
	}
	if cd := loadEngineCooldown(root, cursorCooldownName); cd != nil {
		t.Fatalf("non-quota Fable terminal must not create a quota cooldown: %+v", cd)
	}
	events, _, err := loadTaskEvents(root, task.ID)
	if err != nil || len(events) == 0 {
		t.Fatalf("load hold receipt: events=%d err=%v", len(events), err)
	}
	last := events[len(events)-1]
	if last.Type != evHeld || last.Detail["reason"] != "fable_non_quota_held" ||
		last.Detail["failure_kind"] != string(fallbackTransport) {
		t.Fatalf("non-quota hold receipt incomplete: %+v", last)
	}
}
