package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
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

const (
	grokProcessPermissionEnvDiagnostic = "permission denied writing /tmp/cardex-grok-secret-session"
	grokProcessPermissionEnvLeak       = "/tmp/cardex-grok-secret-session"
	grokProcessReadOnlyEnvDiagnostic   = "Grok session directory is read-only"
	grokProcessUnknownURLLeak          = "https://example.invalid/grok-process-token"
	grokProcessUnknownDiagnostic       = grokTerminalFixtureStderrSentinel + " bearer SECRETTOKEN " + grokProcessUnknownURLLeak
)

type grokZeroEventProcessFixture struct {
	name        string
	stderr      string
	exitCode    int
	payload     string
	wantSubtype string
	leaks       []string
}

func grokZeroEventProcessFixtures() []grokZeroEventProcessFixture {
	versionOnly := `{"type":"system.version","version":"1.0.5"}`
	return []grokZeroEventProcessFixture{
		{
			name: "transport", stderr: grokTerminalTransportDiagnostic, exitCode: 1,
			wantSubtype: "grok_build_process_transport",
			leaks:       []string{grokTerminalTransportDiagnostic},
		},
		{
			name: "transport version-only stdout", stderr: grokTerminalTransportDiagnostic, exitCode: 1,
			payload: versionOnly, wantSubtype: "grok_build_process_transport",
			leaks: []string{grokTerminalTransportDiagnostic},
		},
		{
			name: "permission environment", stderr: grokProcessPermissionEnvDiagnostic, exitCode: 2,
			wantSubtype: "grok_build_process_permission_environment",
			leaks:       []string{grokProcessPermissionEnvLeak},
		},
		{
			name: "permission environment read-only session store", stderr: grokProcessReadOnlyEnvDiagnostic, exitCode: 5,
			payload: versionOnly, wantSubtype: "grok_build_process_permission_environment",
			leaks: []string{grokProcessReadOnlyEnvDiagnostic},
		},
		{
			name: "invalid invocation", stderr: grokTerminalInvalidOptionDiag, exitCode: 3,
			wantSubtype: "grok_build_process_invalid_invocation",
			leaks:       []string{grokTerminalInvalidOptionDiag, "--foo"},
		},
		{
			name: "unknown nonempty stderr", stderr: grokProcessUnknownDiagnostic, exitCode: 4,
			wantSubtype: "grok_build_process_unclassified",
			leaks:       []string{grokTerminalFixtureStderrSentinel, "SECRETTOKEN", grokProcessUnknownURLLeak},
		},
	}
}

func grokProcessExitStatus(code int) string {
	return "exit_status=" + strconv.Itoa(code)
}

func assertGrokProcessTerminalResult(t *testing.T, res *claudeResult, combined string, runErr error, want grokZeroEventProcessFixture) {
	t.Helper()
	if runErr == nil || res == nil || !res.IsError {
		t.Fatalf("zero-event process terminal must fail closed: res=%+v err=%v", res, runErr)
	}
	if res.SemanticEvents != 0 || res.ModelEvents != 0 || res.ToolEvents != 0 ||
		res.TerminalEvents != 0 || res.NumTurns != 0 {
		t.Fatalf("product invocation must remain zero-event: %+v", res)
	}
	if res.Subtype == "grok_build_stream_incomplete" {
		t.Fatalf("current code masked zero-event process terminal as generic stream_incomplete: %+v", res)
	}
	if res.Subtype != want.wantSubtype {
		t.Fatalf("process subtype=%q want %q res=%+v", res.Subtype, want.wantSubtype, res)
	}
	if !strings.Contains(res.Result, grokProcessExitStatus(want.exitCode)) {
		t.Fatalf("normalized exit status %d missing from result %q", want.exitCode, res.Result)
	}
	if kind, unknown := grokTerminalUnknownOutcome(res); unknown || kind != "" {
		t.Fatalf("process terminal must not take the unknown-outcome mask: kind=%q unknown=%v res=%+v", kind, unknown, res)
	}
	if kind, ok := classifyPolicyFallbackFailure(grokBuildRunnerName, res, combined, runErr); ok {
		t.Fatalf("process terminal must not authorize a fallback writer: kind=%q res=%+v", kind, res)
	}
	for _, needle := range want.leaks {
		if needle != "" && strings.Contains(res.Subtype, needle) {
			t.Fatalf("subtype leaked %q: %q", needle, res.Subtype)
		}
		if needle != "" && strings.Contains(res.Result, needle) {
			t.Fatalf("result leaked stderr/secret material %q: %q", needle, res.Result)
		}
		if needle != "" && strings.Contains(combined, needle) {
			t.Fatalf("combined observation leaked stderr/secret material %q", needle)
		}
		if needle != "" && strings.Contains(runErr.Error(), needle) {
			t.Fatalf("runErr leaked stderr/secret material %q: %v", needle, runErr)
		}
	}
}

func TestInvokeGrokBuildZeroEventProcessTerminals(t *testing.T) {
	for _, tc := range grokZeroEventProcessFixtures() {
		t.Run(tc.name, func(t *testing.T) {
			bin, productCalls := fakeGrokBuildCounted(t, tc.payload, tc.stderr, tc.exitCode)
			cfg := grokBuildTestConfig(t, bin)
			task := &Task{ID: "grok-process-invoke", Type: typeSequence, Dir: t.TempDir(), PreferRunner: grokBuildRunnerName}
			root := admitDirectInvoke(t, "", task)
			res, combined, err := invokeGrokBuild(context.Background(), root, cfg, task, "harmless prompt")
			if n := countProductCalls(t, productCalls); n != 1 {
				t.Fatalf("models preflight must pass and product must run once, got %d", n)
			}
			assertGrokProcessTerminalResult(t, res, combined, err, tc)
		})
	}
}

func assertGrokProcessTerminalHeld(t *testing.T, root string, task *Task, productCalls string, want grokZeroEventProcessFixture) {
	t.Helper()
	got, err := loadTask(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusHeld {
		t.Fatalf("expected held, got status=%q task=%+v", got.Status, got)
	}
	if got.Attempts != 0 {
		t.Fatalf("attempts must stay 0, got %d", got.Attempts)
	}
	if got.PreferRunner != grokBuildRunnerName || got.CodexModel != "" || got.OwnerRouteLeg != 1 {
		t.Fatalf("must not fallback/requeue to another writer: %+v", got)
	}
	if got.ResumeAtEpoch != 0 || got.NotBeforeEpoch != 0 {
		t.Fatalf("must not schedule resume/backoff: %+v", got)
	}
	if got.FallbackReason != "" {
		t.Fatalf("must not persist a fallback reason: %+v", got)
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
	if ra.FailureKind == string(fallbackStreamIncomplete) || ra.FailureClass == "unknown_outcome" {
		t.Fatalf("process terminal masked as stream_incomplete/unknown_outcome: %+v last_error=%q", ra, got.LastError)
	}
	if ra.FailureClass != string(failurePermission) {
		t.Fatalf("existing classifier must hold permission-class process terminals: %+v last_error=%q", ra, got.LastError)
	}
	if ra.SemanticEvents != 0 || ra.ModelEvents != 0 || ra.ToolEvents != 0 {
		t.Fatalf("counters must stay zero: %+v", ra)
	}
	if !strings.Contains(got.LastError, want.wantSubtype) {
		t.Fatalf("last_error must retain typed process class %q: %q", want.wantSubtype, got.LastError)
	}
	if !strings.Contains(got.LastError, grokProcessExitStatus(want.exitCode)) {
		t.Fatalf("last_error must retain exit status: %q", got.LastError)
	}
	if strings.Contains(got.LastError, "grok_build_stream_incomplete") {
		t.Fatalf("last_error still masked as stream_incomplete: %q", got.LastError)
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
			t.Fatalf("must not emit retry after zero-event process terminal: %+v", ev)
		}
		if ev.Type == evLimitPaused {
			t.Fatalf("must not emit limit_paused after zero-event process terminal: %+v", ev)
		}
		if ev.Type == evHeld && ev.Actor == "runner:classifier" {
			held = ev
		}
		if ev.Type == evHeld && ev.Actor == "runner:grok-build" &&
			ev.Detail["reason"] == "grok_terminal_unknown_outcome_held" {
			t.Fatalf("process terminal must not take the unknown-outcome hold: %+v", ev)
		}
		if ev.Type == evHeld && ev.Actor == "runner:policy-fallback" {
			t.Fatalf("process terminal must not take policy fallback: %+v", ev)
		}
	}
	if held == nil {
		t.Fatalf("missing runner:classifier held event in %+v", eventTypes(events))
	}
	if held.Detail["failure_class"] != string(failurePermission) {
		t.Fatalf("held failure_class: %+v", held.Detail)
	}
	errText, _ := held.Detail["err"].(string)
	if !strings.Contains(errText, want.wantSubtype) || !strings.Contains(errText, grokProcessExitStatus(want.exitCode)) {
		t.Fatalf("held err must retain typed process class and exit status: %+v", held.Detail)
	}
	needles := append([]string{grokTerminalFixtureSemanticText}, want.leaks...)
	assertNoGrokTerminalLeak(t, root, task.ID, needles...)
}

func TestRunTaskGrokZeroEventProcessTerminalsHeld(t *testing.T) {
	for _, tc := range grokZeroEventProcessFixtures() {
		t.Run(tc.name, func(t *testing.T) {
			root := testRoot(t)
			bin, productCalls := fakeGrokBuildCounted(t, tc.payload, tc.stderr, tc.exitCode)
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
			assertGrokProcessTerminalHeld(t, root, task, productCalls, tc)
		})
	}
}

func TestRunTaskGrokZeroEventQuotaRemainsUnchanged(t *testing.T) {
	root := testRoot(t)
	bin, productCalls := fakeGrokBuildCounted(t, `{"type":"system.version","version":"1.0.5"}`, grokTerminalQuotaDiagnostic, 1)
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
	if n := countProductCalls(t, productCalls); n != 1 {
		t.Fatalf("expected one product invocation, got %d", n)
	}
	if got.LastRouteAttempt == nil || got.LastRouteAttempt.FailureKind != string(fallbackQuota) {
		t.Fatalf("zero-event quota must keep existing quota handling: %+v", got.LastRouteAttempt)
	}
	if strings.Contains(got.LastError, "grok_build_process_") {
		t.Fatalf("quota must not be rewritten as a process class: %q", got.LastError)
	}
}

func TestGrokBuildNormalizedProcessExitStatus(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 7")
	err := cmd.Run()
	if got := grokBuildNormalizedProcessExitStatus(err); got != "7" {
		t.Fatalf("ordinary ExitError: got %q want 7", got)
	}
	if got := grokBuildNormalizedProcessExitStatus(errors.New("exec: not started")); got != grokBuildProcessExitNonExit {
		t.Fatalf("non-exit process error: got %q want %q", got, grokBuildProcessExitNonExit)
	}
	if got := grokBuildNormalizedProcessExitStatus(nil); got != grokBuildProcessExitNonExit {
		t.Fatalf("nil error: got %q want %q", got, grokBuildProcessExitNonExit)
	}
}

func TestGrokTerminalUnknownOutcomeProcessClassesAreNotUnknown(t *testing.T) {
	for _, subtype := range []string{
		"grok_build_process_transport",
		"grok_build_process_permission_environment",
		"grok_build_process_invalid_invocation",
		"grok_build_process_unclassified",
	} {
		kind, unknown := grokTerminalUnknownOutcome(&claudeResult{
			Subtype: subtype, ObservationComplete: true,
		})
		if kind != "" || unknown {
			t.Fatalf("%s must not be an unknown-outcome mask: kind=%q unknown=%v", subtype, kind, unknown)
		}
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
