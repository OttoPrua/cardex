package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	grokTerminalFixtureSemanticText   = "UNIQUE_FIXTURE_TEXT_SEMANTIC_ABC_DO_NOT_LEAK"
	grokTerminalFixtureStderrSentinel = "STDERR_SENTINEL_DO_NOT_LEAK_XYZ"
	grokTerminalQuotaDiagnostic       = "quota exceeded"
	grokTerminalTransportDiagnostic   = "connection reset by peer"
	grokTerminalStallDiagnostic       = "semantic stall"
	grokTerminalAuthPermDiagnostic    = "permission denied"
	grokTerminalInvalidOptionDiag     = "invalid option --foo"
	grokTerminalMalformedPayload      = "not-json-malformed-stream"
	grokTerminalPrivateSession        = "session-private-do-not-leak"
)

// fakeGrokBuildCounted mirrors fakeGrokBuild but counts product invocations.
func fakeGrokBuildCounted(t *testing.T, payload, stderr string, exitCode int) (bin, productCalls string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "grok")
	productCalls = filepath.Join(dir, "product.calls")
	payloadPath := filepath.Join(dir, "result.jsonl")
	if err := os.WriteFile(payloadPath, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$arg\" = 'models' ]; then\n" +
		"    printf '%s\\n' 'You are logged in with grok.com.' 'Available models:' '  * grok-4.6 (default)'\n" +
		"    exit 0\n" +
		"  fi\n" +
		"done\n" +
		"printf 'product\\n' >> " + shSingleQuote(productCalls) + "\n" +
		"cat " + shSingleQuote(payloadPath) + "\n"
	if stderr != "" {
		script += "printf '%s\\n' " + shSingleQuote(stderr) + " >&2\n"
	}
	script += "exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, productCalls
}

func countProductCalls(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "product" {
			n++
		}
	}
	return n
}

func assertNoGrokTerminalLeak(t *testing.T, root string, taskID string, needles ...string) {
	t.Helper()
	taskData, err := os.ReadFile(taskPath(root, taskID))
	if err != nil {
		t.Fatal(err)
	}
	eventData, err := os.ReadFile(eventsPath(root, taskID))
	if err != nil {
		t.Fatal(err)
	}
	blob := string(taskData) + "\n" + string(eventData)
	for _, needle := range needles {
		if needle != "" && strings.Contains(blob, needle) {
			t.Fatalf("task/event JSON must not retain fixture/stderr material %q", needle)
		}
	}
}

func assertGrokTerminalUnknownHeld(t *testing.T, root string, task *Task, productCalls string, wantAttempts int, wantKind fallbackFailureKind, wantSem, wantModel, wantTool int, wantObsComplete bool, extraNeedles ...string) {
	t.Helper()
	got, err := loadTask(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusHeld {
		t.Fatalf("expected held, got status=%q task=%+v", got.Status, got)
	}
	if got.Attempts != wantAttempts {
		t.Fatalf("attempts must stay unchanged at %d, got %d", wantAttempts, got.Attempts)
	}
	if got.PreferRunner != grokBuildRunnerName || got.CodexModel != "" {
		t.Fatalf("must not fallback/requeue to another writer: %+v", got)
	}
	if got.ResumeAtEpoch != 0 || got.NotBeforeEpoch != 0 {
		t.Fatalf("must not schedule resume/backoff: %+v", got)
	}
	if cd := loadEngineCooldown(root, grokBuildCooldownName); cd != nil {
		t.Fatalf("must not write grok cooldown: %+v", cd)
	}
	if _, err := os.Stat(cooldownPath(root)); !os.IsNotExist(err) {
		t.Fatalf("must not write global cooldown, stat err=%v", err)
	}
	if got.LastRouteAttempt == nil {
		t.Fatal("missing LastRouteAttempt")
	}
	ra := got.LastRouteAttempt
	if ra.FailureKind != string(wantKind) || ra.FailureClass != "unknown_outcome" {
		t.Fatalf("exact failure kind/class mismatch: kind=%q class=%q want kind=%q class=unknown_outcome",
			ra.FailureKind, ra.FailureClass, wantKind)
	}
	if ra.SemanticEvents != wantSem || ra.ModelEvents != wantModel || ra.ToolEvents != wantTool {
		t.Fatalf("counters not retained: sem=%d model=%d tool=%d want %d/%d/%d",
			ra.SemanticEvents, ra.ModelEvents, ra.ToolEvents, wantSem, wantModel, wantTool)
	}
	if ra.ObservationOK != wantObsComplete {
		t.Fatalf("observation_complete=%v want %v", ra.ObservationOK, wantObsComplete)
	}
	if n := countProductCalls(t, productCalls); n != 1 {
		t.Fatalf("expected exactly one product invocation, got %d", n)
	}

	events, _, err := loadTaskEvents(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var held *TaskEvent
	for i := range events {
		ev := &events[i]
		if ev.Type == evRetry {
			t.Fatalf("must not emit retry after unknown Grok terminal: %+v", ev)
		}
		if ev.Type == evLimitPaused {
			t.Fatalf("must not emit limit_paused after unknown Grok terminal: %+v", ev)
		}
		if ev.Type == evHeld && ev.Actor == "runner:grok-build" {
			held = ev
		}
	}
	if held == nil {
		t.Fatalf("missing runner:grok-build held event in %+v", eventTypes(events))
	}
	if held.Detail["reason"] != "grok_terminal_unknown_outcome_held" {
		t.Fatalf("held reason: %+v", held.Detail)
	}
	if held.Detail["reason_class"] != "unknown_outcome" || held.Detail["failure_class"] != "unknown_outcome" {
		t.Fatalf("held reason_class/failure_class: %+v", held.Detail)
	}
	if held.Detail["failure_kind"] != string(wantKind) {
		t.Fatalf("held failure_kind: %+v", held.Detail)
	}
	if held.Detail["observation_complete"] != wantObsComplete {
		t.Fatalf("held observation_complete: %+v", held.Detail)
	}
	if intFromDetail(held.Detail["semantic_events"]) != wantSem ||
		intFromDetail(held.Detail["model_events"]) != wantModel ||
		intFromDetail(held.Detail["tool_events"]) != wantTool {
		t.Fatalf("held counters: %+v", held.Detail)
	}
	needles := append([]string{grokTerminalFixtureSemanticText, grokTerminalFixtureStderrSentinel}, extraNeedles...)
	assertNoGrokTerminalLeak(t, root, task.ID, needles...)
}

func intFromDetail(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return -1
	}
}

func TestGrokTerminalUnknownOutcomeDerivesKindFromSubtypeOnly(t *testing.T) {
	tests := []struct {
		name        string
		res         *claudeResult
		wantKind    fallbackFailureKind
		wantUnknown bool
	}{
		{name: "nil result", wantKind: "", wantUnknown: false},
		{name: "proved complete metadata-only stream incomplete", res: &claudeResult{
			Subtype: "grok_build_stream_incomplete", ObservationComplete: true,
		}, wantKind: fallbackStreamIncomplete, wantUnknown: false},
		{name: "semantic stream incomplete", res: &claudeResult{
			Subtype: "grok_build_stream_incomplete", ObservationComplete: true, SemanticEvents: 1, ModelEvents: 1,
		}, wantKind: fallbackStreamIncomplete, wantUnknown: true},
		{name: "tool invalid terminal", res: &claudeResult{
			Subtype: "grok_build_invalid_terminal", ModelEvents: 1, ToolEvents: 1,
		}, wantKind: fallbackInvalidTerminal, wantUnknown: true},
		{name: "incomplete observation zero counters", res: &claudeResult{
			Subtype: "grok_build_stream_incomplete", ObservationComplete: false,
		}, wantKind: fallbackStreamIncomplete, wantUnknown: true},
		{name: "nonzero turns are unknown", res: &claudeResult{
			Subtype: "grok_build_stream_incomplete", ObservationComplete: true, NumTurns: 1,
		}, wantKind: fallbackStreamIncomplete, wantUnknown: true},
		{name: "unrelated process error", res: &claudeResult{
			Subtype: "grok_build_process_error", ObservationComplete: true, SemanticEvents: 1,
		}, wantKind: "", wantUnknown: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotKind, unknown := grokTerminalUnknownOutcome(tc.res)
			if gotKind != tc.wantKind || unknown != tc.wantUnknown {
				t.Fatalf("got kind=%q unknown=%v, want kind=%q unknown=%v", gotKind, unknown, tc.wantKind, tc.wantUnknown)
			}
		})
	}
}

func TestRunTaskGrokSemanticMissingEndHoldsUnknownOutcome(t *testing.T) {
	root := testRoot(t)
	// Keep stderr empty so the semantic missing-end observation stays complete; the stderr
	// sentinel is covered by the invalid-terminal and malformed-stream cases below.
	bin, productCalls := fakeGrokBuildCounted(t, grokTerminalSemanticMissingEndPayload(), "", 1)
	cfg := policyTestConfig()
	cfg.GrokBuildBin = bin
	cfg.MaxAttempts = 3
	task := ownerBackendGrokTask(t, root, cfg, t.TempDir())
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	if err := runTaskVia(context.Background(), root, cfg, task, grokBuildRunnerName); err != nil {
		t.Fatal(err)
	}
	assertGrokTerminalUnknownHeld(t, root, task, productCalls, 0, fallbackStreamIncomplete, 1, 1, 0, true)
}

func TestRunTaskGrokToolModelInvalidTerminalHoldsUnknownOutcome(t *testing.T) {
	root := testRoot(t)
	bin, productCalls := fakeGrokBuildCounted(t, grokTerminalToolInvalidTerminalPayload(), grokTerminalFixtureStderrSentinel, 1)
	cfg := policyTestConfig()
	cfg.GrokBuildBin = bin
	cfg.MaxAttempts = 3
	task := ownerBackendGrokTask(t, root, cfg, t.TempDir())
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	if err := runTaskVia(context.Background(), root, cfg, task, grokBuildRunnerName); err != nil {
		t.Fatal(err)
	}
	assertGrokTerminalUnknownHeld(t, root, task, productCalls, 0, fallbackInvalidTerminal, 0, 1, 1, false, grokTerminalPrivateSession)
}

func TestRunTaskGrokObservationIncompleteZeroCounterMalformedHolds(t *testing.T) {
	root := testRoot(t)
	bin, productCalls := fakeGrokBuildCounted(t, grokTerminalMalformedPayload, grokTerminalFixtureStderrSentinel, 1)
	cfg := policyTestConfig()
	cfg.GrokBuildBin = bin
	cfg.MaxAttempts = 3
	task := ownerBackendGrokTask(t, root, cfg, t.TempDir())
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	if err := runTaskVia(context.Background(), root, cfg, task, grokBuildRunnerName); err != nil {
		t.Fatal(err)
	}
	// Incomplete observation with 0/0/0 is NOT safely presemantic — must hold.
	assertGrokTerminalUnknownHeld(t, root, task, productCalls, 0, fallbackStreamIncomplete, 0, 0, 0, false, grokTerminalMalformedPayload)
}

func grokTerminalSemanticMissingEndPayload() string {
	return `{"type":"system.version","version":"1.0.5"}` + "\n" +
		`{"type":"text","data":"` + grokTerminalFixtureSemanticText + `"}`
}

func grokTerminalToolInvalidTerminalPayload() string {
	return `{"type":"model_start"}` + "\n" +
		`{"type":"tool"}` + "\n" +
		`{"type":"end","stopReason":"max_tokens","sessionId":"` + grokTerminalPrivateSession + `"}`
}

func TestRunTaskGrokUnknownOutcomeHoldsDespiteCompetingDiagnostics(t *testing.T) {
	semanticPayload := grokTerminalSemanticMissingEndPayload()
	toolPayload := grokTerminalToolInvalidTerminalPayload()
	diagnostics := []struct {
		name   string
		stderr string
	}{
		{name: "quota", stderr: grokTerminalQuotaDiagnostic},
		{name: "transport", stderr: grokTerminalTransportDiagnostic},
		{name: "stall", stderr: grokTerminalStallDiagnostic},
		{name: "auth-permission", stderr: grokTerminalAuthPermDiagnostic},
		{name: "invalid-option", stderr: grokTerminalInvalidOptionDiag},
	}
	semanticObsComplete := map[string]bool{
		"quota": true, "transport": true, "stall": true,
		"auth-permission": false, "invalid-option": false,
	}

	type tc struct {
		name            string
		payload         string
		stderr          string
		wantKind        fallbackFailureKind
		wantSem         int
		wantModel       int
		wantTool        int
		wantObsComplete bool
		standalone      bool
		startAttempts   int
		leakNeedles     []string
	}
	var cases []tc
	for _, d := range diagnostics {
		cases = append(cases, tc{
			name: "semantic missing-end plus " + d.name, payload: semanticPayload, stderr: d.stderr,
			wantKind: fallbackStreamIncomplete, wantSem: 1, wantModel: 1, wantTool: 0,
			wantObsComplete: semanticObsComplete[d.name], leakNeedles: []string{d.stderr},
		}, tc{
			name: "tool invalid-terminal plus " + d.name, payload: toolPayload, stderr: d.stderr,
			wantKind: fallbackInvalidTerminal, wantSem: 0, wantModel: 1, wantTool: 1,
			wantObsComplete: false, leakNeedles: []string{d.stderr, grokTerminalPrivateSession},
		})
	}
	cases = append(cases,
		tc{
			name: "observation-incomplete zero counters plus quota", payload: grokTerminalMalformedPayload,
			stderr: grokTerminalQuotaDiagnostic, wantKind: fallbackStreamIncomplete,
			wantObsComplete: false, leakNeedles: []string{grokTerminalQuotaDiagnostic, grokTerminalMalformedPayload},
		},
		tc{
			name: "standalone non-Owner Grok semantic plus quota", payload: semanticPayload,
			stderr: grokTerminalQuotaDiagnostic, wantKind: fallbackStreamIncomplete,
			wantSem: 1, wantModel: 1, wantObsComplete: true, standalone: true,
			leakNeedles: []string{grokTerminalQuotaDiagnostic},
		},
		tc{
			name: "pre-existing Attempts semantic plus quota", payload: semanticPayload,
			stderr: grokTerminalQuotaDiagnostic, wantKind: fallbackStreamIncomplete,
			wantSem: 1, wantModel: 1, wantObsComplete: true, startAttempts: 2,
			leakNeedles: []string{grokTerminalQuotaDiagnostic},
		},
	)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := testRoot(t)
			bin, productCalls := fakeGrokBuildCounted(t, tc.payload, tc.stderr, 1)
			cfg := policyTestConfig()
			cfg.GrokBuildBin = bin
			cfg.MaxAttempts = 3
			var task *Task
			if tc.standalone {
				task = newTask(root, cfg, typeSequence, "standalone explicit grok", t.TempDir(), []string{"implement"}, 1)
				task.PreferRunner = grokBuildRunnerName
				task.RunnerExplicit = true
				task.GrokModel = "grok-4.6"
				task.GrokEffort = "xhigh"
				if task.OwnerRouteLeg != 0 {
					t.Fatalf("standalone fixture must not freeze an Owner leg: %+v", task)
				}
			} else {
				task = ownerBackendGrokTask(t, root, cfg, t.TempDir())
			}
			task.Attempts = tc.startAttempts
			if err := saveTask(root, task); err != nil {
				t.Fatal(err)
			}
			if err := runTaskVia(context.Background(), root, cfg, task, grokBuildRunnerName); err != nil {
				t.Fatal(err)
			}
			assertGrokTerminalUnknownHeld(t, root, task, productCalls, tc.startAttempts, tc.wantKind,
				tc.wantSem, tc.wantModel, tc.wantTool, tc.wantObsComplete, tc.leakNeedles...)
		})
	}
}

func TestRunTaskGrokCompleteMetadataOnlyMissingEndRemainsFallbackEligible(t *testing.T) {
	root := testRoot(t)
	bin, productCalls := fakeGrokBuildCounted(t, `{"type":"system.version","version":"1.0.5"}`, "", 1)
	cfg := policyTestConfig()
	cfg.GrokBuildBin = bin
	cfg.MaxAttempts = 3
	task := ownerBackendGrokTask(t, root, cfg, t.TempDir())
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
	// opus_backend_high_risk has a single primary Grok leg: proved 0/0/0 incomplete remains
	// fallback-eligible, then the existing no-next-leg hold fires. That must not be rewritten
	// as unknown_outcome.
	if got.Status != statusHeld || got.PreferRunner != grokBuildRunnerName || got.OwnerRouteLeg != 1 {
		t.Fatalf("proved complete 0/0/0 metadata-only missing end must remain on existing bounded path: %+v", got)
	}
	if got.FallbackReason != string(fallbackStreamIncomplete) {
		t.Fatalf("existing bounded path should retain stream_incomplete fallback reason: %+v", got)
	}
	if got.LastRouteAttempt == nil || got.LastRouteAttempt.FailureClass == "unknown_outcome" ||
		got.LastRouteAttempt.FailureKind != string(fallbackStreamIncomplete) {
		t.Fatalf("must not classify proved metadata-only incomplete as unknown_outcome: %+v", got.LastRouteAttempt)
	}
	if n := countProductCalls(t, productCalls); n != 1 {
		t.Fatalf("expected one Grok product invocation before existing bounded handling, got %d", n)
	}
	events, _, err := loadTaskEvents(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	sawExisting := false
	for _, ev := range events {
		if ev.Type == evHeld && ev.Actor == "runner:grok-build" &&
			ev.Detail["reason"] == "grok_terminal_unknown_outcome_held" {
			t.Fatalf("proved metadata-only incomplete must not take the unknown-outcome hold: %+v", ev)
		}
		if ev.Type == evHeld && ev.Actor == "runner:policy-fallback" &&
			ev.Detail["reason"] == "no_resolver_proven_next_leg" {
			sawExisting = true
		}
		if ev.Type == evRetry {
			sawExisting = true
		}
	}
	if !sawExisting {
		t.Fatalf("expected existing bounded fallback/no-next-leg handling, got %v", eventTypes(events))
	}
}

func TestRunTaskGrokValidEndTurnRemainsSuccessful(t *testing.T) {
	root := testRoot(t)
	payload := `{"type":"text","data":"OK"}` + "\n" +
		`{"type":"end","stopReason":"end_turn","sessionId":"session-ok","num_turns":1}`
	bin, productCalls := fakeGrokBuildCounted(t, payload, "", 0)
	cfg := policyTestConfig()
	cfg.GrokBuildBin = bin
	task := ownerBackendGrokTask(t, root, cfg, t.TempDir())
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
	// Owner backend completion may immediately hold for the required review gate; the Grok
	// terminal itself must still have succeeded.
	if got.SessionID == "" || got.TurnsUsed < 1 || got.Step < 1 {
		t.Fatalf("valid end/end_turn must succeed at the Grok terminal: %+v", got)
	}
	if got.Status != statusDone && got.Status != statusHeld {
		t.Fatalf("valid end/end_turn must not retry/fail: %+v", got)
	}
	if got.LastRouteAttempt != nil && got.LastRouteAttempt.FailureClass == "unknown_outcome" {
		t.Fatalf("successful terminal must not be unknown_outcome: %+v", got.LastRouteAttempt)
	}
	if n := countProductCalls(t, productCalls); n != 1 {
		t.Fatalf("expected one product invocation, got %d", n)
	}
	events, _, err := loadTaskEvents(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type == evRetry {
			t.Fatalf("successful terminal must not emit retry: %+v", ev)
		}
		if ev.Type == evHeld && ev.Detail["reason"] == "grok_terminal_unknown_outcome_held" {
			t.Fatalf("successful terminal must not take unknown-outcome hold: %+v", ev)
		}
	}
	assertNoGrokTerminalLeak(t, root, task.ID, grokTerminalFixtureSemanticText, grokTerminalFixtureStderrSentinel)
}
