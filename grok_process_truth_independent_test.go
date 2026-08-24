package main

// Independent C-PROC verification for the 0.10.12 Grok process-truth candidate
// (commit ac88fa9c6a46399a7d9ad7ce793eaa3f7cf9d2b8, tree 891d2b306c230c19acb3051a60ca59a70c8788cb).
//
// These cases are written against the behaviour contract rather than the
// candidate's own fixtures, so that a defect in the candidate's assertions
// cannot also hide the defect in the product. Three properties are proved
// end to end:
//
//  1. Fail-closed evidence anywhere in the full stderr stream keeps the closed
//     process classes unreachable, no matter how far past the bounded
//     positive-selection window the evidence appears.
//  2. No byte of raw stderr, and no credential material carried on it, reaches
//     any file under the state root. The whole root is walked, including the
//     per-task log that the candidate's own leak assertion does not read.
//  3. A zero-semantic non-zero exit is held truthfully: no retry, no fallback
//     writer, no resume/backoff, no cooldown, no attempt consumption.
import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Credential-shaped material planted on stderr. None of these may match the
// auth/quota grammars, otherwise the fixture would exercise the pre-existing
// auth path instead of the candidate's process-class path.
const (
	cprocBearerSecret   = "xai-INDEPENDENT-BEARER-a1b2c3d4e5f6"
	cprocAPIKeySecret   = "sk-INDEPENDENT-APIKEY-9z8y7x6w"
	cprocURLSecret      = "https://cli-chat-proxy.invalid/v1?access_token=INDEPENDENT-URL-TOKEN-777"
	cprocPasswordSecret = "passphrase=INDEPENDENT-PASSPHRASE-QQQ"
	cprocHomePathSecret = "/Users/independent-operator/.grokcreds/INDEPENDENT-CRED-FILE"
	cprocStdoutSecret   = "INDEPENDENT-STDOUT-SEMANTIC-DO-NOT-LEAK"
)

const cprocVersionOnlyStdout = `{"type":"system.version","version":"1.0.5"}`

// cprocNoiseTag renders an index as letters only. Decimal indices are unusable
// here: a line numbered 429 would match the quota grammar and silently turn a
// classification control into a fail-closed case.
func cprocNoiseTag(i int) string {
	var out []byte
	for {
		out = append([]byte{byte('a' + i%26)}, out...)
		i /= 26
		if i == 0 {
			return string(out)
		}
		i--
	}
}

// cprocNoiseLine is plain stderr that matches no classifier at all, so it can
// pad a stream without changing which class is selected.
func cprocNoiseLine(i int) string {
	return "independent noise line " + cprocNoiseTag(i) + ": nothing here matches any classifier"
}

func cprocNoise(n int) []string {
	lines := make([]string, 0, n)
	for i := 0; i < n; i++ {
		lines = append(lines, cprocNoiseLine(i))
	}
	return lines
}

// cprocFailClosedProbes are the evidence forms that must always defeat positive
// process classification: a whole-line stall diagnostic, well-formed JSON, and
// malformed object/array-prefixed payloads.
func cprocFailClosedProbes() map[string]string {
	return map[string]string{
		"whole line stall":         "semantic stall",
		"whole line deadline":      "deadline exceeded",
		"error prefixed stall":     "error: model timeout",
		"valid json object":        `{"type":"assistant","text":"independent"}`,
		"valid json array":         `[{"type":"tool_use"}]`,
		"malformed object prefix":  `{"type":"assistant","text":`,
		"malformed array prefix":   `[{"type":`,
		"json with trailing comma": `{"type":"result",}`,
	}
}

// TestCPROCFullStreamFailClosedEvidenceAtExtremeDistance proves the fail-closed
// decision is taken on the whole stream. The evidence is planted far beyond both
// the 8-line and 2048-byte bounds of the positive-selection window, and in the
// hardest case behind a fully valid in-window transport diagnostic that would
// otherwise type the terminal.
func TestCPROCFullStreamFailClosedEvidenceAtExtremeDistance(t *testing.T) {
	// 2000 lines is 250x the 8-line bound and ~75x the 2048-byte bound, which is
	// far enough to distinguish a full-stream scan from a windowed one. Going
	// further only multiplies race-detector cost for no extra discrimination.
	const farLines = 2000
	for probeName, probe := range cprocFailClosedProbes() {
		for _, shape := range []struct {
			name  string
			build func(evidence string) string
		}{
			{
				name: "evidence far past window",
				build: func(evidence string) string {
					return strings.Join(append(cprocNoise(farLines), evidence), "\n")
				},
			},
			{
				name: "in-window transport then far evidence",
				build: func(evidence string) string {
					lines := append([]string{"connection reset by peer"}, cprocNoise(farLines)...)
					return strings.Join(append(lines, evidence), "\n")
				},
			},
			{
				name: "in-window permission then far evidence",
				build: func(evidence string) string {
					lines := append([]string{"permission denied opening the session store"}, cprocNoise(farLines)...)
					return strings.Join(append(lines, evidence), "\n")
				},
			},
			{
				name: "crlf stream with far evidence",
				build: func(evidence string) string {
					return strings.Join(append(cprocNoise(farLines), evidence, ""), "\r\n")
				},
			},
			{
				name: "evidence on the very last line without trailing newline",
				build: func(evidence string) string {
					return strings.Join(cprocNoise(farLines), "\n") + "\n" + evidence
				},
			},
		} {
			t.Run(probeName+"/"+shape.name, func(t *testing.T) {
				stderr := shape.build(probe)
				if !grokBuildProcessStderrHasFailClosedEvidence(stderr) {
					t.Fatalf("full-stream scan missed fail-closed evidence %q", probe)
				}
				class, ok := classifyGrokBuildProcessStderr(stderr)
				if ok {
					t.Fatalf("fail-closed evidence %q must not yield a process class, got %q", probe, class)
				}
				if class != "" {
					t.Fatalf("rejected classification must be empty, got %q", class)
				}
			})
		}
	}
}

// TestCPROCFarNoiseAloneStaysClassifiable is the control for the test above: an
// equally long stream that carries no fail-closed evidence must still be typed,
// otherwise the fail-closed proof would be vacuous.
func TestCPROCFarNoiseAloneStaysClassifiable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first string
		want  grokBuildProcessClass
	}{
		{"noise only", cprocNoiseLine(0), grokBuildProcessClassUnclassified},
		{"transport first", "connection reset by peer", grokBuildProcessClassTransport},
		{"permission first", "permission denied opening the session store", grokBuildProcessClassPermissionEnvironment},
		{"invalid invocation first", "invalid option --independent", grokBuildProcessClassInvalidInvocation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stderr := strings.Join(append([]string{tc.first}, cprocNoise(2000)...), "\n")
			class, ok := classifyGrokBuildProcessStderr(stderr)
			if !ok {
				t.Fatalf("evidence-free stream must stay classifiable, got ok=false")
			}
			if class != tc.want {
				t.Fatalf("class=%q want %q", class, tc.want)
			}
		})
	}
}

// TestCPROCBoundedWindowEvidenceImpliesFullStreamFailClosed is the
// non-regression invariant against the pre-candidate rule, which only inspected
// the bounded window. Every stream the old in-window rule would have rejected
// must still be rejected now, so the change can only ever be more fail-closed.
func TestCPROCBoundedWindowEvidenceImpliesFullStreamFailClosed(t *testing.T) {
	var corpus []string
	for _, probe := range cprocFailClosedProbes() {
		for pos := 0; pos < 10; pos++ {
			lines := cprocNoise(pos)
			lines = append(lines, probe)
			lines = append(lines, cprocNoise(3)...)
			corpus = append(corpus, strings.Join(lines, "\n"))
			corpus = append(corpus, strings.Join(lines, "\r\n"))
			corpus = append(corpus, "\n\n"+strings.Join(lines, "\n\n"))
			corpus = append(corpus, strings.Repeat("x", 900)+"\n"+strings.Join(lines, "\n"))
		}
	}
	for i, stderr := range corpus {
		boundedHasEvidence := false
		for _, line := range grokBuildProcessStderrBoundedLines(stderr) {
			if policyPlainStallDiagnosticRe.MatchString(line) || grokBuildProcessLineJSONEvidence(line) {
				boundedHasEvidence = true
				break
			}
		}
		if !boundedHasEvidence {
			continue
		}
		if !grokBuildProcessStderrHasFailClosedEvidence(stderr) {
			t.Fatalf("corpus[%d]: bounded window saw evidence but full-stream scan did not", i)
		}
		if _, ok := classifyGrokBuildProcessStderr(stderr); ok {
			t.Fatalf("corpus[%d]: bounded window saw evidence but a class was still selected", i)
		}
	}
}

// TestCPROCScannerLossIsFailClosed proves an unreadable stream is never typed.
// A single token past the scanner bound makes the observation unprovable, which
// must be treated as evidence of loss rather than as an absence of evidence.
//
// The stream deliberately opens with a valid transport diagnostic sitting inside
// the positive-selection window. That is the case the candidate's own overflow
// fixtures do not cover, and the only one where losing the tail could silently
// promote a partially-read stream to a typed terminal. Scanning 8 MiB under the
// race detector is expensive, so this stays a single targeted case.
func TestCPROCScannerLossIsFailClosed(t *testing.T) {
	stderr := "connection reset by peer\n" + strings.Repeat("a", grokBuildStderrScanMaxToken+4096)
	if !grokBuildProcessStderrHasFailClosedEvidence(stderr) {
		t.Fatal("scanner loss must count as fail-closed evidence")
	}
	if class, ok := classifyGrokBuildProcessStderr(stderr); ok {
		t.Fatalf("scanner loss must not yield a class, got %q", class)
	}
}

// cprocFakeGrok writes a fake Grok CLI that passes the models preflight, then on
// the product invocation emits payload on stdout, stderr on stderr, and exits
// with exitCode. When killSignal is set the process kills itself instead, which
// is how a signal death (ExitCode() == -1) is reproduced.
func cprocFakeGrok(t *testing.T, payload, stderr string, exitCode int, killSignal bool) (bin, productCalls string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "grok")
	productCalls = filepath.Join(dir, "product.calls")
	payloadPath := filepath.Join(dir, "stdout.jsonl")
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
		"cat " + shSingleQuote(stderrPath) + " >&2\n"
	if killSignal {
		script += "kill -9 $$\n"
	}
	script += "exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, productCalls
}

func cprocProductCalls(t *testing.T, path string) int {
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

// cprocAssertRootFree walks every file under the state root and fails on any
// occurrence of the given material. This deliberately covers more than the
// task and event files: the per-task log, cost/usage ledgers, progress files
// and any cooldown written along the way are all in scope.
func cprocAssertRootFree(t *testing.T, root string, needles ...string) {
	t.Helper()
	scanned := 0
	sawTask, sawEvents, sawLog := false, false, false
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch filepath.Base(filepath.Dir(path)) {
		case "tasks":
			sawTask = true
		case "events":
			sawEvents = true
		case "logs":
			sawLog = true
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				return nil
			}
			return readErr
		}
		scanned++
		blob := string(data)
		for _, needle := range needles {
			if needle == "" {
				continue
			}
			if strings.Contains(blob, needle) {
				rel, _ := filepath.Rel(root, path)
				t.Fatalf("persistent state file %s retained forbidden material %q", rel, needle)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned == 0 {
		t.Fatal("walked the state root but read no files; the leak scan would be vacuous")
	}
	if !sawTask || !sawEvents || !sawLog {
		t.Fatalf("leak scan missed a persistence surface: task=%v events=%v log=%v", sawTask, sawEvents, sawLog)
	}
}

type cprocProcessCase struct {
	name      string
	stderr    string
	exitCode  int
	wantClass grokBuildProcessClass
	secrets   []string
}

// cprocProcessCases place credential-shaped material on stderr lines that do not
// themselves carry the classifying diagnostic, which is the realistic shape: the
// CLI prints a recognizable failure plus surrounding context that may include a
// token, a URL or a private path.
func cprocProcessCases() []cprocProcessCase {
	return []cprocProcessCase{
		{
			name:      "transport with bearer token in context",
			stderr:    "connection reset by peer\ncontext: authorization " + cprocBearerSecret + " target " + cprocURLSecret,
			exitCode:  1,
			wantClass: grokBuildProcessClassTransport,
			secrets:   []string{cprocBearerSecret, cprocURLSecret, "connection reset by peer"},
		},
		{
			name:      "permission environment with credential path",
			stderr:    "permission denied opening " + cprocHomePathSecret + "\nkey " + cprocAPIKeySecret,
			exitCode:  2,
			wantClass: grokBuildProcessClassPermissionEnvironment,
			secrets:   []string{cprocHomePathSecret, cprocAPIKeySecret, "permission denied"},
		},
		{
			name:      "invalid invocation echoing a secret flag value",
			stderr:    "invalid option --token=" + cprocBearerSecret,
			exitCode:  3,
			wantClass: grokBuildProcessClassInvalidInvocation,
			secrets:   []string{cprocBearerSecret, "invalid option"},
		},
		{
			name:      "unclassified carrying every secret shape",
			stderr:    "internal failure " + cprocBearerSecret + " " + cprocAPIKeySecret + " " + cprocURLSecret + " " + cprocPasswordSecret + " " + cprocHomePathSecret,
			exitCode:  4,
			wantClass: grokBuildProcessClassUnclassified,
			secrets: []string{
				cprocBearerSecret, cprocAPIKeySecret, cprocURLSecret,
				cprocPasswordSecret, cprocHomePathSecret, "internal failure",
			},
		},
		{
			name:      "transport with secret far past the bounded window",
			stderr:    "connection refused\n" + strings.Join(cprocNoise(500), "\n") + "\ntrailing " + cprocBearerSecret,
			exitCode:  5,
			wantClass: grokBuildProcessClassTransport,
			secrets:   []string{cprocBearerSecret, "connection refused"},
		},
	}
}

// cprocRunHeld drives one full runTaskVia and returns the reloaded card.
func cprocRunHeld(t *testing.T, stderr string, exitCode int, killSignal bool) (root string, got *Task, productCalls string) {
	t.Helper()
	root = testRoot(t)
	bin, productCalls := cprocFakeGrok(t, cprocVersionOnlyStdout, stderr, exitCode, killSignal)
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
	return root, got, productCalls
}

// cprocAssertTruthfulHold checks the state a held zero-event process exit must
// leave behind: the card is held on the same writer, nothing was retried,
// nothing was scheduled, and the recorded class is the closed truthful one.
func cprocAssertTruthfulHold(t *testing.T, root string, got *Task, productCalls string, wantClass grokBuildProcessClass, wantExitStatus string) {
	t.Helper()
	if got.Status != statusHeld {
		t.Fatalf("status=%q want held (task=%+v)", got.Status, got)
	}
	if got.Attempts != 0 {
		t.Fatalf("attempts=%d want 0; a held process exit must not consume an attempt", got.Attempts)
	}
	if got.PreferRunner != grokBuildRunnerName {
		t.Fatalf("prefer_runner=%q want %q; must not route to another writer", got.PreferRunner, grokBuildRunnerName)
	}
	if got.CodexModel != "" || got.FallbackReason != "" {
		t.Fatalf("fallback state leaked: codex_model=%q fallback_reason=%q", got.CodexModel, got.FallbackReason)
	}
	if got.ResumeAtEpoch != 0 || got.NotBeforeEpoch != 0 {
		t.Fatalf("must not schedule resume/backoff: resume=%d not_before=%d", got.ResumeAtEpoch, got.NotBeforeEpoch)
	}
	if cd := loadEngineCooldown(root, grokBuildCooldownName); cd != nil {
		t.Fatalf("must not write an engine cooldown: %+v", cd)
	}
	if _, err := os.Stat(cooldownPath(root)); !os.IsNotExist(err) {
		t.Fatalf("must not write a global cooldown, stat err=%v", err)
	}
	if got.LastRouteAttempt == nil {
		t.Fatal("missing LastRouteAttempt")
	}
	ra := got.LastRouteAttempt
	if ra.FailureClass != string(wantClass) {
		t.Fatalf("failure_class=%q want %q", ra.FailureClass, wantClass)
	}
	if ra.FailureKind != "grok_build_process_"+string(wantClass) {
		t.Fatalf("failure_kind=%q want grok_build_process_%s", ra.FailureKind, wantClass)
	}
	if ra.SemanticEvents != 0 || ra.ModelEvents != 0 || ra.ToolEvents != 0 {
		t.Fatalf("counters must stay zero: %+v", ra)
	}
	if !ra.ObservationOK {
		t.Fatalf("observation must be recorded complete for a proven zero-work exit: %+v", ra)
	}
	if !strings.Contains(got.LastError, "exit_status="+wantExitStatus) {
		t.Fatalf("last_error must carry the normalized exit status %q: %q", wantExitStatus, got.LastError)
	}
	if strings.Contains(got.LastError, "grok_build_stream_incomplete") {
		t.Fatalf("last_error must not be masked as stream_incomplete: %q", got.LastError)
	}
	if n := cprocProductCalls(t, productCalls); n != 1 {
		t.Fatalf("product invoked %d times, want exactly 1 (no retry, no fallback)", n)
	}
	events, _, err := loadTaskEvents(root, got.ID)
	if err != nil {
		t.Fatal(err)
	}
	var held *TaskEvent
	for i := range events {
		ev := &events[i]
		switch ev.Type {
		case evRetry:
			t.Fatalf("must not emit retry: %+v", ev)
		case evLimitPaused:
			t.Fatalf("must not emit limit_paused: %+v", ev)
		case evHeld:
			if ev.Detail["reason"] == "grok_zero_event_process_exit_held" {
				held = ev
			}
		}
	}
	if held == nil {
		t.Fatalf("missing the zero-event process hold event, saw %+v", eventTypes(events))
	}
	if held.Detail["failure_class"] != string(wantClass) {
		t.Fatalf("held failure_class=%v want %q", held.Detail["failure_class"], wantClass)
	}
	if held.Detail["observation_complete"] != true {
		t.Fatalf("held observation_complete=%v want true", held.Detail["observation_complete"])
	}
}

// TestCPROCPersistentStateNeverRetainsRawStderrOrCredentials is the leak proof.
// Every file under the state root is read back, including the per-task log.
func TestCPROCPersistentStateNeverRetainsRawStderrOrCredentials(t *testing.T) {
	for _, tc := range cprocProcessCases() {
		t.Run(tc.name, func(t *testing.T) {
			root, got, productCalls := cprocRunHeld(t, tc.stderr, tc.exitCode, false)
			cprocAssertTruthfulHold(t, root, got, productCalls, tc.wantClass, strconv.Itoa(tc.exitCode))
			needles := append([]string{cprocStdoutSecret}, tc.secrets...)
			// Every non-empty physical stderr line is also forbidden verbatim.
			for _, line := range strings.Split(tc.stderr, "\n") {
				if line = strings.TrimSpace(line); line != "" {
					needles = append(needles, line)
				}
			}
			cprocAssertRootFree(t, root, needles...)
		})
	}
}

// TestCPROCZeroSemanticNonzeroExitsHoldTruthfully sweeps the exit-status space
// that a real CLI can produce, including the shell's 126/127 and the 128+signal
// range, and proves each one keeps a truthful held/no-retry/no-fallback card.
func TestCPROCZeroSemanticNonzeroExitsHoldTruthfully(t *testing.T) {
	for _, exitCode := range []int{1, 2, 3, 4, 5, 7, 64, 65, 70, 126, 127, 128, 130, 137, 254, 255} {
		t.Run("exit_"+strconv.Itoa(exitCode), func(t *testing.T) {
			stderr := "connection reset by peer\ncontext " + cprocBearerSecret
			root, got, productCalls := cprocRunHeld(t, stderr, exitCode, false)
			cprocAssertTruthfulHold(t, root, got, productCalls,
				grokBuildProcessClassTransport, strconv.Itoa(exitCode))
			cprocAssertRootFree(t, root, cprocBearerSecret, "connection reset by peer")
		})
	}
}

// TestCPROCSignalKilledZeroEventHoldsTruthfully covers the death-by-signal case,
// where there is no ordinary exit status to report. The hold must still be
// truthful and value-free rather than falling back to a fabricated class.
func TestCPROCSignalKilledZeroEventHoldsTruthfully(t *testing.T) {
	stderr := "connection refused\ncontext " + cprocAPIKeySecret
	root, got, productCalls := cprocRunHeld(t, stderr, 0, true)
	cprocAssertTruthfulHold(t, root, got, productCalls, grokBuildProcessClassTransport, "-1")
	cprocAssertRootFree(t, root, cprocAPIKeySecret, "connection refused")
}

// TestCPROCFailClosedEvidenceNeverBecomesAProcessHold is the end-to-end mirror of
// the classifier proof: stderr carrying stall or JSON evidence past the window
// must not reach the closed process-hold path at all.
func TestCPROCFailClosedEvidenceNeverBecomesAProcessHold(t *testing.T) {
	for probeName, probe := range cprocFailClosedProbes() {
		t.Run(probeName, func(t *testing.T) {
			stderr := "connection reset by peer\n" +
				strings.Join(cprocNoise(200), "\n") + "\n" + probe + "\ncontext " + cprocBearerSecret
			root, got, _ := cprocRunHeld(t, stderr, 1, false)
			if got.Status != statusHeld {
				t.Fatalf("fail-closed evidence must still hold the card, got %q", got.Status)
			}
			if strings.Contains(got.LastError, "grok_build_process_transport") ||
				strings.Contains(got.LastError, "grok_build_process_unclassified") ||
				strings.Contains(got.LastError, "grok_build_process_permission_environment") ||
				strings.Contains(got.LastError, "grok_build_process_invalid_invocation") {
				t.Fatalf("fail-closed evidence was promoted to a typed process class: %q", got.LastError)
			}
			if got.Attempts != 0 {
				t.Fatalf("attempts=%d want 0", got.Attempts)
			}
			if got.CodexModel != "" || got.FallbackReason != "" {
				t.Fatalf("fail-closed evidence must not authorize a fallback writer: %+v", got)
			}
			cprocAssertRootFree(t, root, cprocBearerSecret)
		})
	}
}

// TestCPROCProcessHoldIsDeterministic runs the same fixture in two independent
// roots and requires byte-identical process-truth fields, so the recorded class
// cannot depend on map iteration order, timing or scan position.
func TestCPROCProcessHoldIsDeterministic(t *testing.T) {
	stderr := "connection reset by peer\npermission denied opening " + cprocHomePathSecret +
		"\ninvalid option --token=" + cprocBearerSecret
	type snapshot struct{ status, lastError, class, kind string }
	var seen []snapshot
	for i := 0; i < 3; i++ {
		_, got, _ := cprocRunHeld(t, stderr, 9, false)
		if got.LastRouteAttempt == nil {
			t.Fatal("missing LastRouteAttempt")
		}
		seen = append(seen, snapshot{
			status:    got.Status,
			lastError: got.LastError,
			class:     got.LastRouteAttempt.FailureClass,
			kind:      got.LastRouteAttempt.FailureKind,
		})
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] != seen[0] {
			t.Fatalf("non-deterministic process truth: run0=%+v run%d=%+v", seen[0], i, seen[i])
		}
	}
	// Transport is listed first in the closed precedence order, so a stream
	// carrying all three diagnostics must resolve to transport every time.
	if seen[0].class != string(grokBuildProcessClassTransport) {
		t.Fatalf("precedence not honoured: class=%q want transport", seen[0].class)
	}
}

// TestCPROCAuthAndQuotaPrecedenceUnchanged guards the blast radius: the closed
// process classes must not capture streams that the pre-existing auth and quota
// paths already own.
func TestCPROCAuthAndQuotaPrecedenceUnchanged(t *testing.T) {
	for name, stderr := range map[string]string{
		"quota in window":      "quota exceeded",
		"quota past window":    strings.Join(cprocNoise(50), "\n") + "\nrate limit",
		"auth in window":       "invalid api key",
		"auth past window":     strings.Join(cprocNoise(50), "\n") + "\n401 Unauthorized",
		"transport then quota": "connection reset by peer\n" + strings.Join(cprocNoise(50), "\n") + "\nquota exceeded",
		"transport then 401":   "connection reset by peer\n" + strings.Join(cprocNoise(50), "\n") + "\nHTTP 401 Unauthorized",
		"transport then no auth": "connection reset by peer\n" + strings.Join(cprocNoise(50), "\n") +
			"\nreason=no auth context",
	} {
		t.Run(name, func(t *testing.T) {
			if class, ok := classifyGrokBuildProcessStderr(stderr); ok {
				t.Fatalf("auth/quota evidence must not be typed as a process class, got %q", class)
			}
		})
	}
}
