package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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
	if exitCode < 0 {
		script += "kill -9 $$\n"
	} else {
		script += "exit " + strconv.Itoa(exitCode) + "\n"
	}
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

func TestRunTaskGrokCompleteMetadataOnlyMissingEndDoesNotAuthorizeFallback(t *testing.T) {
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
	assertNativeHeldWithoutReplay(t, root, got)
	if got.LastRouteAttempt == nil || got.LastRouteAttempt.FailureClass != "unknown_outcome" || got.LastRouteAttempt.FailureKind != "stream_incomplete" || got.FallbackReason != "" {
		t.Fatalf("missing end cannot authorize a bounded fallback: %+v", got)
	}
	if n := countProductCalls(t, productCalls); n != 1 {
		t.Fatalf("product calls=%d", n)
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
	name            string
	stderr          string
	exitCode        int
	payload         string
	wantSubtype     string
	wantClass       grokBuildProcessClass
	wantObsComplete bool
	leaks           []string
}

func grokZeroEventProcessFixtures() []grokZeroEventProcessFixture {
	versionOnly := `{"type":"system.version","version":"1.0.5"}`
	return []grokZeroEventProcessFixture{
		{
			name: "transport", stderr: grokTerminalTransportDiagnostic, exitCode: 1,
			wantSubtype: "grok_build_process_transport", wantClass: grokBuildProcessClassTransport,
			wantObsComplete: true, leaks: []string{grokTerminalTransportDiagnostic},
		},
		{
			name: "transport version-only stdout", stderr: grokTerminalTransportDiagnostic, exitCode: 1,
			payload: versionOnly, wantSubtype: "grok_build_process_transport",
			wantClass: grokBuildProcessClassTransport, wantObsComplete: true,
			leaks: []string{grokTerminalTransportDiagnostic},
		},
		{
			name: "permission environment", stderr: grokProcessPermissionEnvDiagnostic, exitCode: 2,
			wantSubtype: "grok_build_process_permission_environment",
			wantClass:   grokBuildProcessClassPermissionEnvironment, wantObsComplete: true,
			leaks: []string{grokProcessPermissionEnvLeak},
		},
		{
			name: "permission environment read-only session store", stderr: grokProcessReadOnlyEnvDiagnostic, exitCode: 5,
			payload: versionOnly, wantSubtype: "grok_build_process_permission_environment",
			wantClass: grokBuildProcessClassPermissionEnvironment, wantObsComplete: true,
			leaks: []string{grokProcessReadOnlyEnvDiagnostic},
		},
		{
			name: "invalid invocation", stderr: grokTerminalInvalidOptionDiag, exitCode: 3,
			wantSubtype: "grok_build_process_invalid_invocation",
			wantClass:   grokBuildProcessClassInvalidInvocation, wantObsComplete: true,
			leaks: []string{grokTerminalInvalidOptionDiag, "--foo"},
		},
		{
			name: "unknown nonempty stderr", stderr: grokProcessUnknownDiagnostic, exitCode: 4,
			wantSubtype: "grok_build_process_unclassified", wantClass: grokBuildProcessClassUnclassified,
			wantObsComplete: true,
			leaks:           []string{grokTerminalFixtureStderrSentinel, "SECRETTOKEN", grokProcessUnknownURLLeak},
		},
		{
			name: "unknown signal", stderr: grokTerminalFixtureStderrSentinel, exitCode: -1,
			wantSubtype: "grok_build_process_unclassified", wantClass: grokBuildProcessClassUnclassified,
			wantObsComplete: true, leaks: []string{grokTerminalFixtureStderrSentinel},
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
	if res.ObservationComplete != want.wantObsComplete {
		t.Fatalf("observation_complete=%v want %v (stdout-only zero-work proof) res=%+v",
			res.ObservationComplete, want.wantObsComplete, res)
	}
	if want.wantClass != "" && !strings.Contains(res.Subtype, string(want.wantClass)) {
		t.Fatalf("subtype %q missing closed process class %q", res.Subtype, want.wantClass)
	}
	if !strings.Contains(res.Result, grokProcessExitStatus(want.exitCode)) {
		t.Fatalf("normalized exit status %d missing from result %q", want.exitCode, res.Result)
	}
	if want.wantClass == grokBuildProcessClassUnclassified && want.exitCode > 0 {
		stderr := want.stderr + "\n"
		sha := fmt.Sprintf("%x", sha256.Sum256([]byte(stderr)))
		lineBucket := strings.Count(stderr, "\n")
		if lineBucket > grokBuildProcessStderrMaxLines {
			lineBucket = grokBuildProcessStderrMaxLines + 1
		}
		if res.ProcessStderrBytes != len(stderr) || res.ProcessStderrSHA256 != sha ||
			res.ProcessStderrLineCountBucket != lineBucket {
			t.Fatalf("unclassified stderr metadata mismatch: %+v result=%q", res, res.Result)
		}
		for _, value := range []string{
			"stderr_bytes=" + strconv.Itoa(len(stderr)),
			"stderr_sha256=" + sha,
			"stderr_line_count_bucket=" + strconv.Itoa(lineBucket),
		} {
			if !strings.Contains(res.Result, value) {
				t.Fatalf("unclassified result missing %q: %q", value, res.Result)
			}
		}
	} else if res.Result != grokProcessExitStatus(want.exitCode) || res.ProcessStderrBytes != 0 ||
		res.ProcessStderrSHA256 != "" || res.ProcessStderrLineCountBucket != 0 {
		t.Fatalf("stderr-free process metadata behavior changed: %+v result=%q", res, res.Result)
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
	if ra.FailureClass != string(want.wantClass) {
		t.Fatalf("failure_class=%q want truthful process class %q: %+v last_error=%q",
			ra.FailureClass, want.wantClass, ra, got.LastError)
	}
	if ra.SemanticEvents != 0 || ra.ModelEvents != 0 || ra.ToolEvents != 0 {
		t.Fatalf("counters must stay zero: %+v", ra)
	}
	if ra.ObservationOK != want.wantObsComplete {
		t.Fatalf("last_route_attempt observation_complete=%v want %v ra=%+v",
			ra.ObservationOK, want.wantObsComplete, ra)
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
	if held.Detail["failure_class"] != string(want.wantClass) {
		t.Fatalf("held failure_class=%v want %q: %+v", held.Detail["failure_class"], want.wantClass, held.Detail)
	}
	if held.Detail["failure_kind"] != want.wantSubtype ||
		held.Detail["reason"] != "grok_zero_event_process_exit_held" {
		t.Fatalf("held process identity is not truthfully bound: %+v", held.Detail)
	}
	if held.Detail["observation_complete"] != want.wantObsComplete {
		t.Fatalf("held observation_complete: %+v want %v", held.Detail, want.wantObsComplete)
	}
	if want.wantClass == grokBuildProcessClassUnclassified && want.exitCode > 0 {
		stderr := want.stderr + "\n"
		sha := fmt.Sprintf("%x", sha256.Sum256([]byte(stderr)))
		lineBucket := strings.Count(stderr, "\n")
		if lineBucket > grokBuildProcessStderrMaxLines {
			lineBucket = grokBuildProcessStderrMaxLines + 1
		}
		if intFromDetail(held.Detail["stderr_bytes"]) != len(stderr) ||
			held.Detail["stderr_sha256"] != sha ||
			intFromDetail(held.Detail["stderr_line_count_bucket"]) != lineBucket {
			t.Fatalf("held unclassified stderr metadata mismatch: %+v", held.Detail)
		}
		if !strings.Contains(got.LastError, "stderr_bytes="+strconv.Itoa(len(stderr))) ||
			!strings.Contains(got.LastError, "stderr_sha256="+sha) ||
			!strings.Contains(got.LastError, "stderr_line_count_bucket="+strconv.Itoa(lineBucket)) {
			t.Fatalf("last_error missing unclassified stderr identity: %q", got.LastError)
		}
	} else {
		for _, key := range []string{"stderr_bytes", "stderr_sha256", "stderr_line_count_bucket"} {
			if _, ok := held.Detail[key]; ok || strings.Contains(got.LastError, key+"=") {
				t.Fatalf("metadata-free exit gained stderr identity %q: detail=%+v last_error=%q", key, held.Detail, got.LastError)
			}
		}
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
	signalErr := exec.Command("/bin/sh", "-c", "kill -9 $$").Run()
	if got := grokBuildNormalizedProcessExitStatus(signalErr); got != "-1" {
		t.Fatalf("signal ExitError: got %q want -1", got)
	}
	if got := grokBuildNormalizedProcessExitStatus(errors.New("exec: not started")); got != grokBuildProcessExitNonExit {
		t.Fatalf("non-exit process error: got %q want %q", got, grokBuildProcessExitNonExit)
	}
	if got := grokBuildNormalizedProcessExitStatus(nil); got != grokBuildProcessExitNonExit {
		t.Fatalf("nil error: got %q want %q", got, grokBuildProcessExitNonExit)
	}
}

func TestGrokBuildUnclassifiedProcessMetadataDeterministicAndBounded(t *testing.T) {
	runErr := exec.Command("/bin/sh", "-c", "exit 7").Run()
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("expected real exit error 7, got %T %v", runErr, runErr)
	}
	terminal := func(stderr string) grokBuildProcessTerminal {
		t.Helper()
		got, res, ok := grokBuildZeroEventProcessTerminal("", stderr, runErr)
		if !ok || res == nil || got.class != grokBuildProcessClassUnclassified {
			t.Fatalf("expected unclassified zero-event terminal: terminal=%+v res=%+v ok=%v", got, res, ok)
		}
		return got
	}

	stderr := "opaque-secret-one\nopaque-secret-two"
	a, b := terminal(stderr), terminal(stderr)
	changed := terminal(stderr + "-changed")
	wantSHA := fmt.Sprintf("%x", sha256.Sum256([]byte(stderr)))
	if a != b {
		t.Fatalf("equal stderr metadata differs: a=%+v b=%+v", a, b)
	}
	if a.stderrBytes != len(stderr) || a.stderrSHA256 != wantSHA || a.stderrLineCountBucket != 2 {
		t.Fatalf("metadata mismatch: got=%+v want bytes=%d sha=%s lines=2", a, len(stderr), wantSHA)
	}
	if len(a.stderrSHA256) != 64 || strings.ToLower(a.stderrSHA256) != a.stderrSHA256 {
		t.Fatalf("sha256 must be 64 lowercase hex characters: %q", a.stderrSHA256)
	}
	if changed.stderrSHA256 == a.stderrSHA256 {
		t.Fatalf("changed stderr retained identity: before=%+v after=%+v", a, changed)
	}
	if strings.Contains(a.result(), "opaque-secret") {
		t.Fatalf("metadata result leaked stderr: %q", a.result())
	}
	manyLines := terminal(strings.Repeat("opaque\n", grokBuildProcessStderrMaxLines+100))
	if manyLines.stderrLineCountBucket != grokBuildProcessStderrMaxLines+1 {
		t.Fatalf("line bucket is not bounded: %+v", manyLines)
	}
}

func TestGrokBuildUnclassifiedProcessMetadataRequiresPositiveExit(t *testing.T) {
	signalErr := exec.Command("/bin/sh", "-c", "kill -9 $$").Run()
	for _, tc := range []struct {
		name       string
		err        error
		exitStatus string
	}{
		{name: "signal", err: signalErr, exitStatus: "-1"},
		{name: "ordinary", err: errors.New("exec: not started"), exitStatus: grokBuildProcessExitNonExit},
		{name: "context deadline", err: context.DeadlineExceeded, exitStatus: grokBuildProcessExitNonExit},
		{name: "wrapped context cancellation", err: fmt.Errorf("command stopped: %w", context.Canceled), exitStatus: grokBuildProcessExitNonExit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, res, ok := grokBuildZeroEventProcessTerminal("", "opaque diagnostic", tc.err)
			if !ok || res == nil || got.class != grokBuildProcessClassUnclassified {
				t.Fatalf("prior unclassified behavior changed: terminal=%+v res=%+v ok=%v", got, res, ok)
			}
			if got.stderrBytes != 0 || got.stderrSHA256 != "" || got.stderrLineCountBucket != 0 {
				t.Fatalf("non-exit error gained stderr metadata: %+v", got)
			}
			if got.result() != "exit_status="+tc.exitStatus {
				t.Fatalf("non-positive exit result gained stderr metadata: %q", got.result())
			}
		})
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

const (
	grokProcessLaterLineNoise          = "cli: loading module"
	grokProcessJSONEvidence            = `{"foo":1}`
	grokProcessMalformedJSONEvidence   = `{"type":"text","data":`
	grokProcessOversizedJSONLeak       = "oversized_secret_do_not_leak"
	grokProcessScannerOverflowSentinel = "SCANNER_OVERFLOW_SENTINEL_DO_NOT_LEAK"
	grokProcessVersionOnlyStdout       = `{"type":"system.version","version":"1.0.5"}`
)

func grokProcessLaterLineStderr(recognized string) string {
	return grokProcessLaterLineNoise + "\n" + recognized
}

func grokProcessLineBoundStderr(recognized string) string {
	var b strings.Builder
	for i := 0; i < grokBuildProcessStderrMaxLines; i++ {
		b.WriteString(grokProcessLaterLineNoise)
		b.WriteByte(' ')
		b.WriteString(strconv.Itoa(i))
		b.WriteByte('\n')
	}
	b.WriteString(recognized)
	return b.String()
}

func grokProcessBlankLineBoundStderr(recognized string) string {
	return strings.Repeat("\n", grokBuildProcessStderrMaxLines) + recognized
}

func grokProcessByteBoundStderr(recognized string) string {
	return strings.Repeat("n", grokBuildProcessStderrMaxBytes) + "\n" + recognized
}

func grokProcessInWindowThenPastBoundStderr(inWindow, pastBound string) string {
	var b strings.Builder
	b.WriteString(inWindow)
	b.WriteByte('\n')
	for i := 1; i < grokBuildProcessStderrMaxLines; i++ {
		b.WriteString(grokProcessLaterLineNoise)
		b.WriteByte(' ')
		b.WriteString(strconv.Itoa(i))
		b.WriteByte('\n')
	}
	b.WriteString(pastBound)
	return b.String()
}

func grokProcessOversizedMalformedJSONLine(prefix byte) string {
	return string(prefix) + `"` + grokProcessOversizedJSONLeak + `":"` + strings.Repeat("x", grokBuildProcessStderrMaxBytes)
}

func grokProcessScannerOverflowLine() string {
	n := grokBuildStderrScanMaxToken + 1 - len(grokProcessScannerOverflowSentinel)
	if n < 1 {
		n = grokBuildStderrScanMaxToken + 1
	}
	return grokProcessScannerOverflowSentinel + strings.Repeat("n", n)
}

func TestClassifyGrokBuildProcessStderrClosedMultilinePolicy(t *testing.T) {
	tests := []struct {
		name      string
		stderr    string
		wantClass grokBuildProcessClass
		wantOK    bool
	}{
		{name: "empty", stderr: "  \n  ", wantOK: false},
		{name: "first-line transport", stderr: grokTerminalTransportDiagnostic,
			wantClass: grokBuildProcessClassTransport, wantOK: true},
		{name: "later-line transport", stderr: grokProcessLaterLineStderr(grokTerminalTransportDiagnostic),
			wantClass: grokBuildProcessClassTransport, wantOK: true},
		{name: "later-line permission environment", stderr: grokProcessLaterLineStderr(grokProcessPermissionEnvDiagnostic),
			wantClass: grokBuildProcessClassPermissionEnvironment, wantOK: true},
		{name: "later-line invalid invocation", stderr: grokProcessLaterLineStderr(grokTerminalInvalidOptionDiag),
			wantClass: grokBuildProcessClassInvalidInvocation, wantOK: true},
		{name: "later-line unclassified noise", stderr: grokProcessLaterLineStderr(grokProcessUnknownDiagnostic),
			wantClass: grokBuildProcessClassUnclassified, wantOK: true},
		{name: "later-line transport wins over earlier permission",
			stderr:    grokProcessPermissionEnvDiagnostic + "\n" + grokTerminalTransportDiagnostic,
			wantClass: grokBuildProcessClassTransport, wantOK: true},
		{name: "out-of-line-bound later transport is not typed",
			stderr:    grokProcessLineBoundStderr(grokTerminalTransportDiagnostic),
			wantClass: grokBuildProcessClassUnclassified, wantOK: true},
		{name: "out-of-byte-bound later transport is not typed",
			stderr:    grokProcessByteBoundStderr(grokTerminalTransportDiagnostic),
			wantClass: grokBuildProcessClassUnclassified, wantOK: true},
		{name: "later-line stall remains fail-closed",
			stderr: grokProcessLaterLineStderr(grokTerminalStallDiagnostic), wantOK: false},
		{name: "later-line quota remains fail-closed",
			stderr: grokProcessLaterLineStderr(grokTerminalQuotaDiagnostic), wantOK: false},
		{name: "later-line auth remains fail-closed",
			stderr: grokProcessLaterLineStderr(grokBuildExactBareAuthDiagnostic), wantOK: false},
		{name: "later-line json remains fail-closed",
			stderr: grokProcessLaterLineStderr(`{"type":"text","data":"semantic-secret"}`), wantOK: false},
		{name: "later-line malformed json remains fail-closed",
			stderr: grokProcessLaterLineStderr(`{"type":"text","data":`), wantOK: false},
		{name: "transport then later stall remains fail-closed",
			stderr: grokTerminalTransportDiagnostic + "\n" + grokTerminalStallDiagnostic, wantOK: false},
		{name: "json on physical line 9 remains fail-closed",
			stderr: grokProcessLineBoundStderr(grokProcessJSONEvidence), wantOK: false},
		{name: "malformed json on physical line 9 remains fail-closed",
			stderr: grokProcessLineBoundStderr(grokProcessMalformedJSONEvidence), wantOK: false},
		{name: "stall on physical line 9 remains fail-closed",
			stderr: grokProcessLineBoundStderr(grokTerminalStallDiagnostic), wantOK: false},
		{name: "in-window transport plus json after line 8 remains fail-closed",
			stderr: grokProcessInWindowThenPastBoundStderr(grokTerminalTransportDiagnostic, grokProcessJSONEvidence), wantOK: false},
		{name: "in-window transport plus stall after line 8 remains fail-closed",
			stderr: grokProcessInWindowThenPastBoundStderr(grokTerminalTransportDiagnostic, grokTerminalStallDiagnostic), wantOK: false},
		{name: "out-of-byte-bound later json remains fail-closed",
			stderr: grokProcessByteBoundStderr(grokProcessJSONEvidence), wantOK: false},
		{name: "one oversized malformed object-prefixed line remains fail-closed",
			stderr: grokProcessOversizedMalformedJSONLine('{'), wantOK: false},
		{name: "one oversized malformed array-prefixed line remains fail-closed",
			stderr: grokProcessOversizedMalformedJSONLine('['), wantOK: false},
		{name: "scanner token overflow remains fail-closed",
			stderr: grokProcessScannerOverflowLine(), wantOK: false},
		{name: "oversized plain noise remains unclassified",
			stderr:    strings.Repeat("n", grokBuildProcessStderrMaxBytes+64),
			wantClass: grokBuildProcessClassUnclassified, wantOK: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := classifyGrokBuildProcessStderr(tc.stderr)
			if ok != tc.wantOK || got != tc.wantClass {
				t.Fatalf("class=%q ok=%v want class=%q ok=%v", got, ok, tc.wantClass, tc.wantOK)
			}
		})
	}
}

func TestInvokeGrokBuildLaterBoundedProcessDiagnostic(t *testing.T) {
	laterTransport := grokZeroEventProcessFixture{
		name: "later-line transport", stderr: grokProcessLaterLineStderr(grokTerminalTransportDiagnostic),
		exitCode: 1, wantSubtype: "grok_build_process_transport",
		wantClass: grokBuildProcessClassTransport, wantObsComplete: true,
		leaks: []string{grokProcessLaterLineNoise, grokTerminalTransportDiagnostic},
	}
	outOfBound := grokZeroEventProcessFixture{
		name:   "out-of-line-bound transport stays unclassified",
		stderr: grokProcessLineBoundStderr(grokTerminalTransportDiagnostic), exitCode: 1,
		wantSubtype: "grok_build_process_unclassified", wantClass: grokBuildProcessClassUnclassified,
		wantObsComplete: true,
		leaks:           []string{grokProcessLaterLineNoise, grokTerminalTransportDiagnostic},
	}
	for _, tc := range []grokZeroEventProcessFixture{laterTransport, outOfBound} {
		t.Run(tc.name, func(t *testing.T) {
			bin, productCalls := fakeGrokBuildCounted(t, tc.payload, tc.stderr, tc.exitCode)
			cfg := grokBuildTestConfig(t, bin)
			task := &Task{ID: "grok-process-multiline", Type: typeSequence, Dir: t.TempDir(), PreferRunner: grokBuildRunnerName}
			root := admitDirectInvoke(t, "", task)
			res, combined, err := invokeGrokBuild(context.Background(), root, cfg, task, "harmless prompt")
			if n := countProductCalls(t, productCalls); n != 1 {
				t.Fatalf("models preflight must pass and product must run once, got %d", n)
			}
			assertGrokProcessTerminalResult(t, res, combined, err, tc)
		})
	}
}

func TestInvokeGrokBuildLaterLineFailClosedEvidenceIsNotProcessClass(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
	}{
		{name: "later stall", stderr: grokProcessLaterLineStderr(grokTerminalStallDiagnostic)},
		{name: "later quota", stderr: grokProcessLaterLineStderr(grokTerminalQuotaDiagnostic)},
		{name: "later json", stderr: grokProcessLaterLineStderr(`{"type":"text","data":"semantic-secret"}`)},
		{name: "later unstructured json", stderr: grokProcessLaterLineStderr(`{"foo":1}`)},
		{name: "later malformed json", stderr: grokProcessLaterLineStderr(`{"type":"text","data":`)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bin, productCalls := fakeGrokBuildCounted(t, `{"type":"system.version","version":"1.0.5"}`, tc.stderr, 1)
			cfg := grokBuildTestConfig(t, bin)
			task := &Task{ID: "grok-process-fail-closed", Type: typeSequence, Dir: t.TempDir(), PreferRunner: grokBuildRunnerName}
			root := admitDirectInvoke(t, "", task)
			res, _, err := invokeGrokBuild(context.Background(), root, cfg, task, "harmless prompt")
			if n := countProductCalls(t, productCalls); n != 1 {
				t.Fatalf("expected one product invocation, got %d", n)
			}
			if err == nil || res == nil {
				t.Fatalf("must fail closed: res=%+v err=%v", res, err)
			}
			if strings.HasPrefix(res.Subtype, "grok_build_process_") &&
				res.Subtype != "grok_build_process_error" &&
				res.Subtype != "grok_build_process_auth" &&
				res.Subtype != "grok_build_process_auth_exact" {
				t.Fatalf("fail-closed evidence must not be rewritten as a typed process class: %+v", res)
			}
		})
	}
}

func TestRunTaskGrokLaterBoundedProcessDiagnosticHeld(t *testing.T) {
	tc := grokZeroEventProcessFixture{
		name: "later-line transport", stderr: grokProcessLaterLineStderr(grokTerminalTransportDiagnostic),
		exitCode: 1, wantSubtype: "grok_build_process_transport",
		wantClass: grokBuildProcessClassTransport, wantObsComplete: true,
		leaks: []string{grokProcessLaterLineNoise, grokTerminalTransportDiagnostic},
	}
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
}

func TestRunTaskGrokIncompleteStdoutProcessDiagnosticNotPromoted(t *testing.T) {
	root := testRoot(t)
	bin, productCalls := fakeGrokBuildCounted(t, grokTerminalMalformedPayload,
		grokProcessLaterLineStderr(grokTerminalTransportDiagnostic), 1)
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
	if got.LastRouteAttempt == nil || !got.LastRouteAttempt.ObservationSeen || got.LastRouteAttempt.ObservationOK {
		t.Fatalf("incomplete stdout must not be promoted to a complete process terminal: %+v", got.LastRouteAttempt)
	}
	if strings.Contains(got.LastError, "grok_build_process_transport") {
		t.Fatalf("incomplete observation must not be rewritten as process transport: %q", got.LastError)
	}
	assertGrokTerminalUnknownHeld(t, root, task, productCalls, 0, fallbackStreamIncomplete, 0, 0, 0, false,
		grokTerminalMalformedPayload, grokTerminalTransportDiagnostic, grokProcessLaterLineNoise)
}

func fakeGrokBuildCountedStderrFile(t *testing.T, payload, stderr string, exitCode int) (bin, productCalls string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "grok")
	productCalls = filepath.Join(dir, "product.calls")
	payloadPath := filepath.Join(dir, "result.jsonl")
	stderrPath := filepath.Join(dir, "stderr.txt")
	if err := os.WriteFile(payloadPath, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stderrPath, []byte(stderr), 0o644); err != nil {
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
		"cat " + shSingleQuote(payloadPath) + "\n" +
		"cat " + shSingleQuote(stderrPath) + " >&2\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, productCalls
}

func assertGrokZeroEventFailClosedNotProcessClass(t *testing.T, res *claudeResult, runErr error, leaks ...string) {
	t.Helper()
	if runErr == nil || res == nil || !res.IsError {
		t.Fatalf("must fail closed: res=%+v err=%v", res, runErr)
	}
	if strings.HasPrefix(res.Subtype, "grok_build_process_") &&
		res.Subtype != "grok_build_process_error" &&
		res.Subtype != "grok_build_process_auth" &&
		res.Subtype != "grok_build_process_auth_exact" {
		t.Fatalf("fail-closed evidence must not be rewritten as a typed process class: %+v", res)
	}
	if kind, unknown := grokTerminalUnknownOutcome(res); unknown == false && kind == "" &&
		strings.HasPrefix(res.Subtype, "grok_build_process_") &&
		res.Subtype != "grok_build_process_error" {
		t.Fatalf("typed process class escaped fail-closed: %+v", res)
	}
	for _, needle := range leaks {
		if needle == "" {
			continue
		}
		if strings.Contains(res.Subtype, needle) {
			t.Fatalf("subtype leaked %q: %q", needle, res.Subtype)
		}
		if strings.Contains(res.Result, needle) {
			t.Fatalf("result leaked stderr/secret material %q: %q", needle, res.Result)
		}
		if strings.Contains(runErr.Error(), needle) {
			t.Fatalf("runErr leaked stderr/secret material %q: %v", needle, runErr)
		}
	}
}

func TestInvokeGrokBuildFullStreamFailClosedEvidence(t *testing.T) {
	tests := []struct {
		name            string
		stderr          string
		wantObsComplete bool
		fileStderr      bool
		leaks           []string
	}{
		{
			name:   "version-only stdout plus json on physical line 9",
			stderr: grokProcessLineBoundStderr(grokProcessJSONEvidence),
			leaks:  []string{grokProcessJSONEvidence, grokProcessLaterLineNoise},
		},
		{
			name:   "in-window transport plus json after line 8",
			stderr: grokProcessInWindowThenPastBoundStderr(grokTerminalTransportDiagnostic, grokProcessJSONEvidence),
			leaks:  []string{grokProcessJSONEvidence, grokTerminalTransportDiagnostic, grokProcessLaterLineNoise},
		},
		{
			name:            "stall after line 8",
			stderr:          grokProcessBlankLineBoundStderr(grokTerminalStallDiagnostic),
			wantObsComplete: true,
			leaks:           []string{grokTerminalStallDiagnostic},
		},
		{
			name:   "one oversized malformed object-prefixed line",
			stderr: grokProcessOversizedMalformedJSONLine('{'),
			leaks:  []string{grokProcessOversizedJSONLeak},
		},
		{
			name:       "scanner token overflow",
			stderr:     grokProcessScannerOverflowLine(),
			fileStderr: true,
			leaks:      []string{grokProcessScannerOverflowSentinel},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var bin, productCalls string
			if tc.fileStderr {
				bin, productCalls = fakeGrokBuildCountedStderrFile(t, grokProcessVersionOnlyStdout, tc.stderr, 1)
			} else {
				bin, productCalls = fakeGrokBuildCounted(t, grokProcessVersionOnlyStdout, tc.stderr, 1)
			}
			cfg := grokBuildTestConfig(t, bin)
			task := &Task{ID: "grok-process-full-stream", Type: typeSequence, Dir: t.TempDir(), PreferRunner: grokBuildRunnerName}
			root := admitDirectInvoke(t, "", task)
			res, _, err := invokeGrokBuild(context.Background(), root, cfg, task, "harmless prompt")
			if n := countProductCalls(t, productCalls); n != 1 {
				t.Fatalf("expected one product invocation, got %d", n)
			}
			assertGrokZeroEventFailClosedNotProcessClass(t, res, err, tc.leaks...)
			if res.ObservationComplete != tc.wantObsComplete {
				t.Fatalf("observation_complete=%v want %v res=%+v", res.ObservationComplete, tc.wantObsComplete, res)
			}
			if res.Subtype != "grok_build_stream_incomplete" {
				t.Fatalf("fail-closed evidence must remain stream_incomplete, got %+v", res)
			}
		})
	}
}

func TestRunTaskGrokJSONPastWindowRemainsUnknownOutcome(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
		leaks  []string
	}{
		{
			name:   "json on physical line 9",
			stderr: grokProcessLineBoundStderr(grokProcessJSONEvidence),
			leaks:  []string{grokProcessJSONEvidence, grokProcessLaterLineNoise},
		},
		{
			name:   "in-window transport plus json after line 8",
			stderr: grokProcessInWindowThenPastBoundStderr(grokTerminalTransportDiagnostic, grokProcessJSONEvidence),
			leaks:  []string{grokProcessJSONEvidence, grokTerminalTransportDiagnostic, grokProcessLaterLineNoise},
		},
		{
			name:   "one oversized malformed object-prefixed line",
			stderr: grokProcessOversizedMalformedJSONLine('{'),
			leaks:  []string{grokProcessOversizedJSONLeak},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := testRoot(t)
			bin, productCalls := fakeGrokBuildCounted(t, grokProcessVersionOnlyStdout, tc.stderr, 1)
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
			if strings.Contains(got.LastError, "grok_build_process_transport") ||
				strings.Contains(got.LastError, "grok_build_process_unclassified") {
				t.Fatalf("json/malformed evidence must not become a typed process hold: %q", got.LastError)
			}
			assertGrokTerminalUnknownHeld(t, root, task, productCalls, 0, fallbackStreamIncomplete, 0, 0, 0, false, tc.leaks...)
		})
	}
}

func TestRunTaskGrokStallPastWindowIsNotProcessHold(t *testing.T) {
	root := testRoot(t)
	bin, productCalls := fakeGrokBuildCounted(t, grokProcessVersionOnlyStdout,
		grokProcessBlankLineBoundStderr(grokTerminalStallDiagnostic), 1)
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
	if got.Status != statusHeld {
		t.Fatalf("expected held, got status=%q task=%+v", got.Status, got)
	}
	if got.Attempts != 0 {
		t.Fatalf("attempts must stay 0, got %d", got.Attempts)
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
	if strings.Contains(got.LastError, "grok_build_process_transport") ||
		strings.Contains(got.LastError, "grok_build_process_unclassified") ||
		strings.Contains(got.LastError, "grok_zero_event_process_exit_held") {
		t.Fatalf("stall after line 8 must not become a typed process hold: %q", got.LastError)
	}
	events, _, err := loadTaskEvents(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type == evRetry {
			t.Fatalf("must not emit retry: %+v", ev)
		}
		if ev.Type == evHeld && ev.Actor == "runner:classifier" &&
			ev.Detail["reason"] == "grok_zero_event_process_exit_held" {
			t.Fatalf("stall after line 8 must not take the process-class hold: %+v", ev)
		}
	}
	assertNoGrokTerminalLeak(t, root, task.ID, grokTerminalStallDiagnostic, grokProcessLaterLineNoise)
}

func TestRunTaskGrokScannerOverflowRemainsFailClosed(t *testing.T) {
	root := testRoot(t)
	bin, productCalls := fakeGrokBuildCountedStderrFile(t, grokProcessVersionOnlyStdout, grokProcessScannerOverflowLine(), 1)
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
	if strings.Contains(got.LastError, "grok_build_process_unclassified") ||
		strings.Contains(got.LastError, "grok_build_process_transport") {
		t.Fatalf("scanner overflow must not become a typed process hold: %q", got.LastError)
	}
	assertGrokTerminalUnknownHeld(t, root, task, productCalls, 0, fallbackStreamIncomplete, 0, 0, 0, false,
		grokProcessScannerOverflowSentinel)
}

func TestCardexReleaseIdentity(t *testing.T) {
	if version != "0.10.16" {
		t.Fatalf("release identity %q want 0.10.16", version)
	}
}
