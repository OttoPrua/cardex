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
			A: CrossEngine{Kind: grokBuildRunnerName, Model: "grok-4.6", Effort: "xhigh", Label: "grok-4.6/xhigh"},
			B: CrossEngine{Kind: "codex", Effort: "ultra", Label: "gpt-5.6-sol/ultra adversarial merger"},
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

func TestInvokeCursorFinalOwnerFableSequenceIsReadOnlyAsk(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"FABLE_OK"}]}}`,
		`{"type":"result","subtype":"success","result":"FABLE_OK"}`,
	}, "\n")
	bin, argsPath := fakeCursorAgent(t, stream, "", 0)
	cfg := policyTestConfig()
	cfg.CursorBin = bin
	task := &Task{
		ID: "fable-read-only", Type: typeSequence, Dir: t.TempDir(), Model: "fable",
		CursorModel: "claude-fable-5-thinking-max", OwnerRouteName: "fable_explicit",
	}
	res, _, err := invokeCursor(context.Background(), cfg, task, "decide without writing")
	if err != nil || res == nil || res.IsError {
		t.Fatalf("Fable Cursor invoke failed: res=%+v err=%v", res, err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	got := "\n" + string(args) + "\n"
	if !strings.Contains(got, "\n--mode\nask\n") {
		t.Fatalf("Final Owner Fable sequence was not forced read-only at invocation:\n%s", got)
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
	if err := validateCursor(bad); err == nil || !strings.Contains(err.Error(), "第三 Sol/max") {
		t.Fatalf("any third Sol/max merger must fail fast, got %v", err)
	}
}

func TestCursorFableFallbackBuildsGrokAnswerAndSingleSolTerminalMerge(t *testing.T) {
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
		t.Fatalf("B must be the Sol/ultra adversarial reviewer-merger: %+v", task.XEngineB)
	}
	if task.XEngineC != nil {
		t.Fatalf("Fable must have no third Sol/max engine: %+v", task.XEngineC)
	}
	if task.ReviewAfter || task.FableFirstPrinciplesReview {
		t.Fatalf("fallback cross chain must not spawn a second review loop: %+v", task)
	}

	handleCrossStage(root, cfg, task, &claudeResult{Result: "Grok answer"}, nil)
	tasks, err := loadTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	var c *Task
	for _, candidate := range tasks {
		if candidate.XKey == task.XKey && candidate.XRole == "B" {
			t.Fatalf("Fable must not create a blind independent Sol B: %+v", candidate)
		}
		if candidate.XKey == task.XKey && candidate.XRole == "C" {
			c = candidate
			break
		}
	}
	if c == nil || c.PreferRunner != "codex" || c.XCodexModel != "gpt-5.6-sol" || c.Effort != "ultra" ||
		!c.FableReviewerMerger || c.AutomaticSolCalls != 1 || c.ReviewAfter {
		t.Fatalf("C must be the sole fresh Sol/ultra terminal reviewer-merger: %+v", c)
	}
	if !strings.Contains(c.Prompts[0], "decide from first principles") || !strings.Contains(c.Prompts[0], "Grok answer") {
		t.Fatalf("merge prompt must contain original problem and Grok answer: %q", c.Prompts[0])
	}
}

func TestCursorFableFallbackAcceptsOnlyQuotaOrEligiblePresemantic(t *testing.T) {
	for _, kind := range []fallbackFailureKind{fallbackSemanticStall, fallbackInvalidTerminal} {
		t.Run(string(kind), func(t *testing.T) {
			root := testRoot(t)
			cfg := cursorTestConfig()
			task := newTask(root, cfg, typeCoordinate, "hard decision", t.TempDir(), []string{"decide"}, 9)
			task.Model = "fable"
			task.PreferRunner = "codex"
			if err := prepareCursorFableFallback(root, cfg, task, string(kind), kind, verifiedCursorFallbackAuth(t)); err == nil ||
				!strings.Contains(err.Error(), "proven presemantic") {
				t.Fatalf("semantic or acceptance failure must remain held: %v", err)
			}
		})
	}
	for _, kind := range []fallbackFailureKind{fallbackQuota, fallbackTransport, fallbackStreamIncomplete, fallbackExecutionEnv} {
		root := testRoot(t)
		cfg := cursorTestConfig()
		task := newTask(root, cfg, typeCoordinate, "hard decision", t.TempDir(), []string{"decide"}, 9)
		task.Model = "fable"
		task.PreferRunner = "codex"
		if err := prepareCursorFableFallback(root, cfg, task, string(kind), kind, verifiedCursorFallbackAuth(t)); err != nil {
			t.Fatalf("%s must create the bounded answer/terminal-merge chain: %v", kind, err)
		}
	}
}

func TestBoardShowsCursorFablePrimaryAndCompleteFallbackRoute(t *testing.T) {
	root := testRoot(t)
	cfg := cursorTestConfig()
	task := newTask(root, cfg, typeCoordinate, "hard decision", t.TempDir(), []string{"decide from first principles"}, 9)
	task.Model = "fable"
	task.PreferRunner = "codex"

	brief := toBrief(cfg, task, time.Now())
	const wantChain = "Grok answer → Sol/ultra adversarial merge → terminal"
	if brief.Runner != cursorRunnerName || brief.Model != "claude-fable-5-thinking-max" || brief.Effort != "max" {
		t.Fatalf("queued Fable card must show its effective Cursor primary: %+v", brief)
	}
	if !strings.Contains(brief.ModelRoute, wantChain) || strings.Contains(brief.ModelRoute, "Sol/max") {
		t.Fatalf("queued Fable card route=%q, want terminal single-Sol chain", brief.ModelRoute)
	}

	if err := prepareCursorFableFallback(root, cfg, task, "cursor quota exhausted", fallbackQuota, verifiedCursorFallbackAuth(t)); err != nil {
		t.Fatal(err)
	}
	brief = toBrief(cfg, task, time.Now())
	if brief.Model != "grok-4.6" || brief.Effort != "xhigh" {
		t.Fatalf("fallback A card must continue showing the current effective leg: %+v", brief)
	}
	if !strings.Contains(brief.ModelRoute, wantChain) || strings.Contains(brief.ModelRoute, "Sol/max") {
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
		got.XEngineB == nil || got.XEngineC != nil || got.Step != 0 || got.SessionID != "" {
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

func TestRunTaskCursorFableSemanticStallStaysHeldWithoutFallback(t *testing.T) {
	bin, _ := fakeCursorAgent(t,
		`{"type":"system","subtype":"init","session_id":"presemantic","model":"Claude Fable 5 300K Max"}`,
		"semantic timeout", 1)
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
		t.Fatalf("semantic Fable failure must hold the original card without answer/merge legs: %+v", got)
	}
	if cd := loadEngineCooldown(root, cursorCooldownName); cd != nil {
		t.Fatalf("semantic Fable failure must not create a quota cooldown: %+v", cd)
	}
	events, _, err := loadTaskEvents(root, task.ID)
	if err != nil || len(events) == 0 {
		t.Fatalf("load hold receipt: events=%d err=%v", len(events), err)
	}
	last := events[len(events)-1]
	if last.Type != evHeld || last.Detail["reason"] != "fable_ineligible_failure_held" ||
		last.Detail["failure_kind"] != string(fallbackSemanticStall) {
		t.Fatalf("semantic hold receipt incomplete: %+v", last)
	}
}
