package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func nativeCompletionPayload(via, body string) string {
	text, _ := json.Marshal(body)
	switch via {
	case grokBuildRunnerName:
		return `{"type":"text","data":` + string(text) + `}` + "\n" + grok105PublicEnd
	case kimiCLIRunnerName:
		return `{"role":"meta","type":"system.version","version":"0.41.0"}` + "\n" +
			`{"role":"assistant","content":` + string(text) + `}` + "\n" +
			`{"role":"meta","type":"session.resume_hint","session_id":"synthetic-session","command":"kimi --resume synthetic-session","content":"Resume synthetic session"}`
	default:
		return `{"type":"text","part":{"text":` + string(text) + `}}` + "\n" + `{"type":"step_finish","part":{"reason":"stop"}}`
	}
}

func nativeCompletionConfig(t *testing.T, via, payload string, exitCode int) *Config {
	t.Helper()
	var cfg *Config
	switch via {
	case grokBuildRunnerName:
		bin, _, _ := fakeGrokBuild(t, payload, "", exitCode)
		cfg = grokBuildTestConfig(t, bin)
		cfg.GrokBuild.OpusAdversarialReview = false
	case kimiCLIRunnerName:
		bin, _, _ := fakeKimiCLI(t, payload, exitCode)
		cfg = kimiCLITestConfig(t, bin)
	default:
		bin, _ := fakeOpenCode(t, payload, exitCode)
		cfg = openCodeNightTestConfig(bin)
		cfg.OpenCodeModel = "opencode-go/gpt-5.6-luna"
	}
	return cfg
}

func runNativeCompletionTask(t *testing.T, via, payload, taskType string, exitCode int) (string, *Config, *Task) {
	t.Helper()
	root := testRoot(t)
	cfg := nativeCompletionConfig(t, via, payload, exitCode)
	task := newTask(root, cfg, taskType, "synthetic completion", t.TempDir(), []string{"review only the current synthetic candidate"}, 1)
	task.Model, task.PreferRunner, task.RunnerExplicit = "haiku", via, true
	if taskType == typeReview {
		writer := newTask(root, cfg, typeSequence, "synthetic writer", task.Dir, []string{"p"}, 1)
		writer.Status = statusDone
		if err := saveTask(root, writer); err != nil {
			t.Fatal(err)
		}
		task.ReviewOf = writer.ID
		task.ReviewCandidate = &WorkflowCandidate{Commit: "synthetic-current-commit", Tree: "synthetic-current-tree"}
	}
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	if err := runTaskVia(context.Background(), root, cfg, task, via); err != nil {
		t.Fatal(err)
	}
	got, err := loadTask(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return root, cfg, got
}

func assertNativeHeldWithoutReplay(t *testing.T, root string, got *Task) {
	t.Helper()
	if got.Status != statusHeld || got.Step != 0 || got.Attempts != 0 || got.ActiveAttemptID != "" || got.NotBeforeEpoch != 0 || got.ResumeAtEpoch != 0 {
		t.Fatalf("unknown native execution must stay held without replay: %+v", got)
	}
	events, _, err := loadTaskEvents(root, got.ID)
	if err != nil {
		t.Fatal(err)
	}
	dispatched := 0
	for _, event := range events {
		if event.Type == evDispatched {
			dispatched++
		}
		if event.Type == evRetry || event.Type == evStepOK || event.Type == evDone {
			t.Fatalf("held attempt escaped: %+v", event)
		}
	}
	if dispatched != 1 {
		t.Fatalf("dispatch count=%d", dispatched)
	}
}

func TestNativeCompletionProcessAndConsumer(t *testing.T) {
	for _, via := range []string{grokBuildRunnerName, kimiCLIRunnerName, "opencode"} {
		t.Run(via, func(t *testing.T) {
			body := verdictJSON("pass", nil, nil)
			payload := nativeCompletionPayload(via, body)
			t.Run("required review artifact", func(t *testing.T) {
				root, cfg, got := runNativeCompletionTask(t, via, payload, typeReview, 0)
				if got.Status != statusDone || got.Step != 1 || got.ReviewOutput == nil {
					t.Fatalf("normal review must deliver: %+v", got)
				}
				if actual := loadTaskResultForGate(root, got); actual != strings.TrimSpace(body) {
					t.Fatalf("artifact readback=%q", actual)
				}
				gate := &Task{IntegrationGate: &IntegrationGate{WriterTaskID: got.ReviewOf, ReviewTaskID: got.ID, CandidateCommit: got.ReviewCandidate.Commit, CandidateTree: got.ReviewCandidate.Tree}}
				if dec := evaluateIntegrationRelease(root, cfg, gate); !dec.Admit {
					t.Fatalf("consumer rejected current artifact: %+v", dec)
				}
				events, _, err := loadTaskEvents(root, got.ID)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, e := range events {
					if e.Type == evStepOK {
						found = true
						if e.Detail["terminal_count"] != float64(1) || e.Detail["review_output"] == nil {
							t.Fatalf("terminal and artifact receipt missing from event: %+v", e)
						}
					}
				}
				if !found {
					t.Fatal("missing persisted success event")
				}
				// A pass elsewhere in the transcript never replaces the bound current result.
				f, err := os.OpenFile(taskLogPath(root, got.ID), os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = f.WriteString("\n--- PROMPT ---\n" + verdictJSON("block", []string{"old finding"}, nil))
				_ = f.Close()
				if dec := evaluateIntegrationRelease(root, cfg, gate); !dec.Admit {
					t.Fatalf("unrelated transcript tail changed current artifact: %+v", dec)
				}
			})
			t.Run("ordinary task needs no artifact", func(t *testing.T) {
				_, _, got := runNativeCompletionTask(t, via, nativeCompletionPayload(via, ""), typeSequence, 0)
				if got.Status != statusDone || got.Step != 1 || got.ReviewOutput != nil {
					t.Fatalf("normal no-artifact task must complete: %+v", got)
				}
			})
			t.Run("process fails after native terminal", func(t *testing.T) {
				root, _, got := runNativeCompletionTask(t, via, payload, typeReview, 7)
				assertNativeHeldWithoutReplay(t, root, got)
				r := got.LastRouteAttempt
				if r == nil || !r.ObservationOK || r.TerminalCount != 1 || r.FailureClass != "process_failure" || got.ReviewOutput != nil {
					t.Fatalf("must retain native fact, hold failed process and reject delivery: %+v", got)
				}
				if loadTaskResultForGate(root, got) != "" {
					t.Fatal("failed process supplied an artifact")
				}
			})
			t.Run("completion loss", func(t *testing.T) {
				lines := strings.Split(payload, "\n")
				root, _, got := runNativeCompletionTask(t, via, strings.Join(lines[:len(lines)-1], "\n"), typeReview, 0)
				assertNativeHeldWithoutReplay(t, root, got)
				if got.ReviewOutput != nil {
					t.Fatal("incomplete execution supplied an artifact")
				}
			})
		})
	}
}

func TestNativeCompletionThenTimeoutOrSignal(t *testing.T) {
	for _, via := range []string{grokBuildRunnerName, kimiCLIRunnerName, "opencode"} {
		for _, failure := range []string{"timeout", "signal"} {
			if failure == "timeout" && via != grokBuildRunnerName {
				continue // The production step deadline is exercised once; every adapter also gets an actual signal.
			}
			t.Run(via+"/"+failure, func(t *testing.T) {
				root := testRoot(t)
				cfg := nativeCompletionConfig(t, via, nativeCompletionPayload(via, "synthetic final"), 0)
				bin := cfg.OpenCodeBin
				if via == grokBuildRunnerName {
					bin = cfg.GrokBuildBin
				}
				if via == kimiCLIRunnerName {
					bin = cfg.KimiCLIBin
				}
				script, err := os.ReadFile(bin)
				if err != nil {
					t.Fatal(err)
				}
				ending := "kill -KILL $$\n"
				ctx := context.Background()
				if failure == "timeout" {
					ending = "exec sleep 70\n"
					cfg.StepTimeoutMin = 1
				}
				if !strings.HasSuffix(string(script), "exit 0\n") {
					t.Fatal("fake process fixture must end with exit 0")
				}
				if err := os.WriteFile(bin, []byte(strings.TrimSuffix(string(script), "exit 0\n")+ending), 0700); err != nil {
					t.Fatal(err)
				}
				task := newTask(root, cfg, typeSequence, "synthetic process failure", t.TempDir(), []string{"synthetic prompt"}, 1)
				task.Model, task.PreferRunner, task.RunnerExplicit = "haiku", via, true
				if err := saveTask(root, task); err != nil {
					t.Fatal(err)
				}
				if err := runTaskVia(ctx, root, cfg, task, via); err != nil {
					t.Fatal(err)
				}
				got, err := loadTask(root, task.ID)
				if err != nil {
					t.Fatal(err)
				}
				assertNativeHeldWithoutReplay(t, root, got)
				if r := got.LastRouteAttempt; r == nil || r.TerminalCount != 1 || !r.ObservationOK || r.FailureClass != "process_failure" {
					t.Fatalf("native observation must survive process %s: %+v", failure, got)
				}
			})
		}
	}
}

func TestNativeReviewConsumerUsesOnlyFinalConclusion(t *testing.T) {
	pass := verdictJSON("pass", nil, nil)
	for _, tc := range []struct {
		name, body string
		admit      bool
	}{
		{"last pass", verdictJSON("block", []string{"old finding"}, nil) + pass, true},
		{"last block", pass + verdictJSON("block", []string{"current finding"}, nil), false},
		{"last unknown", pass + verdictJSON("maybe", nil, nil), false},
		{"last malformed", pass + "```json\n{\"verdict\":\"pass\",\n```", false},
		{"last unfinished fence", pass + "```json\n{\"verdict\":", false},
		{"unfenced tail", pass + `{ "verdict": "block" }`, false},
		{"unfenced latest unknown", `{ "verdict": "pass" } { "verdict": "maybe" }`, false},
		{"unfenced latest malformed", `{ "verdict": "pass" } { "verdict":`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, cfg, got := runNativeCompletionTask(t, "opencode", nativeCompletionPayload("opencode", tc.body), typeReview, 0)
			if got.Status != statusDone || got.ReviewOutput == nil || got.LastRouteAttempt.TerminalCount != 1 {
				t.Fatalf("execution must finish even when review verdict is inadmissible: %+v", got)
			}
			gate := &Task{IntegrationGate: &IntegrationGate{WriterTaskID: got.ReviewOf, ReviewTaskID: got.ID, CandidateCommit: got.ReviewCandidate.Commit, CandidateTree: got.ReviewCandidate.Tree}}
			if dec := evaluateIntegrationRelease(root, cfg, gate); dec.Admit != tc.admit {
				t.Fatalf("admit=%v want=%v decision=%+v", dec.Admit, tc.admit, dec)
			}
		})
	}
}

func TestNativeReviewArtifactRefusesMissingStaleMismatchedAndUnreadable(t *testing.T) {
	body := verdictJSON("pass", nil, nil)
	for _, name := range []string{"missing", "truncated", "wrong task", "old epoch", "old step", "wrong candidate version", "wrong attempt", "other exited attempt", "changed content", "unreadable", "old prompt pass"} {
		t.Run(name, func(t *testing.T) {
			root, cfg, got := runNativeCompletionTask(t, "opencode", nativeCompletionPayload("opencode", body), typeReview, 0)
			if got.ReviewOutput == nil {
				t.Fatalf("positive fixture failed: %+v", got)
			}
			gate := &Task{IntegrationGate: &IntegrationGate{WriterTaskID: got.ReviewOf, ReviewTaskID: got.ID, CandidateCommit: got.ReviewCandidate.Commit, CandidateTree: got.ReviewCandidate.Tree}}
			if dec := evaluateIntegrationRelease(root, cfg, gate); !dec.Admit {
				t.Fatalf("positive control failed: %+v", dec)
			}
			switch name {
			case "missing":
				_ = os.Remove(taskLogPath(root, got.ID))
			case "truncated":
				_ = os.Truncate(taskLogPath(root, got.ID), got.ReviewOutput.Offset)
			case "wrong task":
				got.ReviewOutput.TaskID = "another-task"
			case "old epoch":
				got.ReviewOutput.ControlEpoch--
			case "old step":
				got.ReviewOutput.Step--
			case "wrong candidate version":
				got.ReviewOutput.CandidateCommit = "old-candidate"
			case "wrong attempt":
				got.ReviewOutput.AttemptID = newAttemptID()
			case "other exited attempt":
				got.ReviewOutput.AttemptID = newAttemptID()
				if err := writeAttempt(root, &AttemptRecord{TaskID: got.ID, AttemptID: got.ReviewOutput.AttemptID, ControlEpoch: got.ControlEpoch, State: attemptExited}); err != nil {
					t.Fatal(err)
				}
			case "changed content":
				_ = os.WriteFile(taskLogPath(root, got.ID), []byte(strings.Repeat("x", int(got.ReviewOutput.Offset+got.ReviewOutput.Bytes))), 0600)
			case "unreadable":
				_ = os.Remove(taskLogPath(root, got.ID))
				_ = os.Mkdir(taskLogPath(root, got.ID), 0700)
			case "old prompt pass":
				got.ReviewOutput = nil // Legacy whole-log pass has no current-result proof.
			}
			if err := saveTask(root, got); err != nil {
				t.Fatal(err)
			}
			if dec := evaluateIntegrationRelease(root, cfg, gate); dec.Admit {
				t.Fatalf("%s artifact accepted: %+v", name, dec)
			}
			if got.LastRouteAttempt.TerminalCount != 1 || !got.LastRouteAttempt.ObservationOK {
				t.Fatal("delivery rejection erased execution fact")
			}
		})
	}
}

func TestNativeGrokLifecycleAndSyntheticMeta(t *testing.T) {
	text := `{"type":"text","data":"OK"}`
	end := grok105PublicEnd
	plan := `{"type":"plan","entries":[{"content":"synthetic step","priority":"high","status":"completed"}]}`
	call := `{"type":"tool_call","toolCallId":"private-call-canary","toolName":"read_file","status":"pending"}`
	update := `{"content":[],"locations":[],"rawOutput":{},"status":"completed","toolCallId":"private-call-canary","type":"tool_call_update"}`
	for _, tc := range []struct {
		name, raw string
		valid     bool
	}{
		{"plan", plan + "\n" + text + "\n" + end, true},
		{"matched tool", call + "\n" + update + "\n" + text + "\n" + end, true},
		{"handled tool failure", call + "\n" + strings.Replace(update, "completed", "failed", 1) + "\n" + text + "\n" + end, true},
		{"open tool", call + "\n" + text + "\n" + end, false},
		{"unknown tool status", strings.Replace(call, "pending", "almost_done", 1) + "\n" + text + "\n" + end, false},
		{"unmatched update", update + "\n" + text + "\n" + end, false},
		{"duplicate tool", call + "\n" + call + "\n" + update + "\n" + text + "\n" + end, false},
		{"duplicate completion", call + "\n" + update + "\n" + update + "\n" + text + "\n" + end, false},
		{"late tool", text + "\n" + end + "\n" + call, false},
		{"unknown plan extension", strings.TrimSuffix(plan, "}") + `,"extension":true}` + "\n" + text + "\n" + end, false},
		{"unknown plan entry", strings.Replace(plan, `"content":"synthetic step"`, `"content":"synthetic step","private":"canary"`, 1) + "\n" + text + "\n" + end, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := parseGrokBuildJSONL(tc.raw)
			if runnerNativeTerminalValid(grokBuildRunnerName, res, nil) != tc.valid {
				t.Fatalf("valid=%v res=%+v", tc.valid, res)
			}
			encoded, _ := json.Marshal(res.GrokDiagnostics)
			if strings.Contains(string(encoded), "private-call-canary") {
				t.Fatal("opaque tool ID entered diagnostics")
			}
		})
	}
	for _, meta := range []struct {
		raw   string
		valid bool
	}{
		{`{"synthetic":true}`, true}, {`null`, false}, {`{"synthetic":false}`, false}, {`{"synthetic":true,"other":1}`, false}, {`{"synthetic":"true"}`, false},
	} {
		for _, event := range []string{text, plan, call, end} {
			t.Run("meta_"+event[:12]+meta.raw, func(t *testing.T) {
				modified := strings.TrimSuffix(event, "}") + `,"_meta":` + meta.raw + `}`
				raw := modified + "\n" + text + "\n" + end
				if event == end {
					raw = text + "\n" + modified
				}
				if event == call {
					raw = modified + "\n" + update + "\n" + text + "\n" + end
				}
				if got := parseGrokBuildJSONL(raw); runnerNativeTerminalValid(grokBuildRunnerName, got, nil) != meta.valid {
					t.Fatalf("meta acceptance=%v: %+v", meta.valid, got)
				}
			})
		}
	}
}

func TestNativeKimiVersionAndToolLifecycle(t *testing.T) {
	version := `{"role":"meta","type":"system.version","version":"0.41.0"}`
	call := `{"role":"assistant","content":"read two fixtures","tool_calls":[{"id":"a"},{"id":"b"}]}`
	toolA := `{"role":"tool","tool_call_id":"a","content":"handled tool error"}`
	toolB := `{"role":"tool","tool_call_id":"b","content":"value"}`
	final := `{"role":"assistant","content":"OK"}`
	hint := `{"role":"meta","type":"session.resume_hint","session_id":"synthetic","command":"kimi --resume synthetic","content":"Resume"}`
	complete := version + "\n" + call + "\n" + toolB + "\n" + toolA + "\n" + final + "\n" + hint
	for _, tc := range []struct {
		name, raw, engine string
		valid             bool
	}{
		{"multi tool handled error", complete, kimiEngineLegacy, true},
		{"legacy EOF", strings.Replace(version, "0.41.0", "0.37.2", 1) + "\n" + final, kimiEngineLegacy, true},
		{"legacy cannot imply v2", strings.Replace(version, "0.41.0", "0.37.2", 1) + "\n" + final, kimiEngineV2, false},
		{"missing version", final + "\n" + hint, kimiEngineLegacy, false},
		{"future version", strings.Replace(complete, "0.41.0", "0.42.0", 1), kimiEngineLegacy, false},
		{"malformed version", strings.Replace(complete, "0.41.0", "0.41garbage.0", 1), kimiEngineLegacy, false},
		{"missing hint", version + "\n" + final, kimiEngineLegacy, false},
		{"v2 hint is unproved", complete, kimiEngineV2, false},
		{"unmatched tool", version + "\n" + toolA + "\n" + final + "\n" + hint, kimiEngineLegacy, false},
		{"unclosed tool", version + "\n" + call + "\n" + toolA + "\n" + final + "\n" + hint, kimiEngineLegacy, false},
		{"missing final", version + "\n" + call + "\n" + toolA + "\n" + toolB + "\n" + hint, kimiEngineLegacy, false},
		{"duplicate result", strings.Replace(complete, toolA, toolA+"\n"+toolA, 1), kimiEngineLegacy, false},
		{"duplicate hint", complete + "\n" + hint, kimiEngineLegacy, false},
		{"late assistant", complete + "\n" + final, kimiEngineLegacy, false},
		{"late unknown", complete + "\n" + `{"role":"meta","type":"future"}`, kimiEngineLegacy, false},
		{"version after work", final + "\n" + strings.Replace(version, "0.41.0", "0.37.2", 1), kimiEngineLegacy, false},
		{"unknown assistant type", strings.Replace(complete, `"role":"assistant","content":"OK"`, `"role":"assistant","type":"future","content":"OK"`, 1), kimiEngineLegacy, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseKimiCLIJSONLForEngine(tc.raw, tc.engine)
			if runnerNativeTerminalValid(kimiCLIRunnerName, got, nil) != tc.valid {
				t.Fatalf("valid=%v res=%+v", tc.valid, got)
			}
			if tc.valid && got.Result != "OK" {
				t.Fatalf("only post-tool final belongs in result: %q", got.Result)
			}
		})
	}
}

func TestNativeOpenCodeLifecycle(t *testing.T) {
	text := `{"type":"text","part":{"text":"OK"}}`
	stop := `{"type":"step_finish","part":{"reason":"stop"}}`
	tool := `{"type":"tool_use","part":{"callID":"a","state":{"status":"completed"}}}`
	for _, tc := range []struct {
		name, raw string
		valid     bool
	}{
		{"normal", text + "\n" + stop, true},
		{"no reason legacy fixture", text + "\n" + `{"type":"step_finish","part":{"tokens":{"input":7,"output":5},"cost":0.25}}`, false},
		{"multi tool turns", tool + "\n" + `{"type":"step_finish","part":{"reason":"tool-calls"}}` + "\n" + strings.Replace(tool, `"a"`, `"b"`, 1) + "\n" + text + "\n" + stop, true},
		{"handled tool error", strings.Replace(tool, "completed", "error", 1) + "\n" + text + "\n" + stop, true},
		{"intermediate only", text + "\n" + `{"type":"step_finish","part":{"reason":"tool-calls"}}`, false},
		{"length", text + "\n" + strings.Replace(stop, "stop", "length", 1), false},
		{"unknown", text + "\n" + strings.Replace(stop, "stop", "private-reason-canary", 1), false},
		{"normalization is not contract", text + "\n" + strings.Replace(stop, "stop", " STOP ", 1), false},
		{"unclosed tool", strings.Replace(tool, "completed", "running", 1) + "\n" + text + "\n" + stop, false},
		{"unmatched result", strings.Replace(tool, "tool_use", "tool_result", 1) + "\n" + text + "\n" + stop, false},
		{"duplicate tool result", tool + "\n" + tool + "\n" + text + "\n" + stop, false},
		{"permission denial", strings.Replace(tool, "completed", "permission_denied", 1) + "\n" + text + "\n" + stop, false},
		{"duplicate stop", text + "\n" + stop + "\n" + stop, false},
		{"late text", text + "\n" + stop + "\n" + text, false},
		{"malformed tail", text + "\n" + stop + "\n" + `{bad`, false},
		{"scanner loss", text + "\n" + strings.Repeat("x", 4*1024*1024+1) + "\n" + stop, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseOpenCodeJSONL(tc.raw)
			if runnerNativeTerminalValid("opencode", got, nil) != tc.valid {
				t.Fatalf("valid=%v res=%+v", tc.valid, got)
			}
			if strings.Contains(got.FinalReason, "private-reason-canary") {
				t.Fatal("opaque reason entered safe readback")
			}
		})
	}
}
