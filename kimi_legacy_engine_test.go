package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeKimiCLIEngineProbe is a deterministic stand-in for the installed Kimi binary. It records the
// exact engine selector environment the Cardex child receives; it never contacts a real provider.
func fakeKimiCLIEngineProbe(t *testing.T, payload string) (bin, envDump string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "kimi")
	envDump = filepath.Join(dir, "engine.dump")
	payloadPath := filepath.Join(dir, "result.jsonl")
	if err := os.WriteFile(payloadPath, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$KIMI_CODE_LEGACY_FLAG\" > " + shSingleQuote(envDump) + "\n" +
		"cat " + shSingleQuote(payloadPath) + "\n" +
		"exit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, envDump
}

func unsetenvForTest(t *testing.T, key string) {
	t.Helper()
	if value, ok := os.LookupEnv(key); ok {
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Setenv(key, value) })
	}
}

func TestInvokeKimiCLISelectsLegacyEngineFor0372(t *testing.T) {
	// Kimi 0.37.2 defaults to the v2 engine whose recursive workspace watcher fails in this
	// environment. Without a caller override, the Cardex child must explicitly select the
	// official legacy agent-core engine via KIMI_CODE_LEGACY_FLAG=1.
	unsetenvForTest(t, "KIMI_CODE_LEGACY_FLAG")
	payload := `{"role":"meta","type":"system.version","version":"0.37.2"}` + "\n" + `{"role":"assistant","content":"OK"}` + "\n" +
		`{"role":"meta","type":"session.resume_hint","session_id":"session-0372"}`
	bin, envDump := fakeKimiCLIEngineProbe(t, payload)
	cfg := kimiCLITestConfig(t, bin)
	task := &Task{ID: "kimi-0372-legacy", Model: "opus", Type: typeSequence, Dir: t.TempDir()}
	root := admitDirectInvoke(t, "", task)
	res, _, err := invokeKimiCLI(context.Background(), root, cfg, task, "p")
	if err != nil || res == nil || res.Result != "OK" {
		t.Fatalf("invoke failed: res=%+v err=%v", res, err)
	}
	raw, err := os.ReadFile(envDump)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(raw)); got != "1" {
		t.Fatalf("Kimi 0.37.2 child must explicitly select the legacy engine, KIMI_CODE_LEGACY_FLAG=%q", got)
	}
}

func TestInvokeKimiCLIPreservesExplicitCallerEngineOverride(t *testing.T) {
	payload := `{"role":"meta","type":"system.version","version":"0.37.2"}` + "\n" + `{"role":"assistant","content":"OK"}`
	for _, override := range []string{"0", "1"} {
		t.Run("override="+override, func(t *testing.T) {
			t.Setenv("KIMI_CODE_LEGACY_FLAG", override)
			bin, envDump := fakeKimiCLIEngineProbe(t, payload)
			cfg := kimiCLITestConfig(t, bin)
			task := &Task{ID: "kimi-override-" + override, Model: "opus", Type: typeSequence, Dir: t.TempDir()}
			root := admitDirectInvoke(t, "", task)
			if _, _, err := invokeKimiCLI(context.Background(), root, cfg, task, "p"); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(envDump)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(raw)); got != override {
				t.Fatalf("explicit caller override must reach the child untouched: want %q got %q", override, got)
			}
		})
	}
}

func TestKimi0372LegacyStreamStaysCompatible(t *testing.T) {
	// The accepted 0.35/0.36 contracts plus the observed 0.37.2 legacy-engine shape
	// (assistant semantic event + resume metadata, no version event) must all parse complete.
	for name, raw := range map[string]string{
		"0.35 stream": `{"role":"meta","type":"system.version","version":"0.35.0"}` + "\n" +
			`{"role":"assistant","content":"OK"}` + "\n" +
			`{"role":"meta","type":"session.resume_hint","session_id":"s-035"}`,
		"0.36.1 stream": `{"role":"meta","type":"system.version","version":"0.36.1"}` + "\n" +
			`{"role":"assistant","content":"OK"}` + "\n" +
			`{"role":"meta","type":"session.resume_hint","session_id":"s-0361"}`,
		"0.37.2 legacy stream": `{"role":"meta","type":"system.version","version":"0.37.2"}` + "\n" + `{"role":"assistant","content":"OK"}` + "\n" +
			`{"role":"meta","type":"session.resume_hint","session_id":"s-0372"}`,
	} {
		res := parseKimiCLIJSONL(raw)
		if res == nil || res.IsError || !res.ObservationComplete || res.Result != "OK" {
			t.Fatalf("%s must remain a complete accepted contract: %+v", name, res)
		}
	}
}

func TestKimiEngineReadbackRecordsRequestedAndActual(t *testing.T) {
	unsetenvForTest(t, "KIMI_CODE_LEGACY_FLAG")
	payload := `{"role":"meta","type":"system.version","version":"0.37.2"}` + "\n" + `{"role":"assistant","content":"OK"}`
	bin, _ := fakeKimiCLIEngineProbe(t, payload)
	cfg := kimiCLITestConfig(t, bin)
	task := &Task{ID: "kimi-engine-readback", Model: "opus", Type: typeSequence, Dir: t.TempDir(), PreferRunner: kimiCLIRunnerName}
	beginRouteAttemptReadback(cfg, task, false)
	root := admitDirectInvoke(t, "", task)
	if _, _, err := invokeKimiCLI(context.Background(), root, cfg, task, "p"); err != nil {
		t.Fatal(err)
	}
	if task.LastRouteAttempt == nil || task.LastRouteAttempt.RequestedEngine != "legacy_agent_core" ||
		task.LastRouteAttempt.ActualEngine != "legacy_agent_core" {
		t.Fatalf("task readback must record requested/actual legacy engine: %+v", task.LastRouteAttempt)
	}
	detail := withRouteAttempt(map[string]any{}, task)
	if detail["requested_engine"] != "legacy_agent_core" || detail["actual_engine"] != "legacy_agent_core" {
		t.Fatalf("route/event readback must carry the engine identity: %+v", detail)
	}
	brief := toBrief(cfg, task, time.Now())
	if brief.RequestedEngine != "legacy_agent_core" || brief.ActualEngine != "legacy_agent_core" {
		t.Fatalf("board readback must carry the engine identity: %+v", brief)
	}
	raw, err := json.Marshal(task.LastRouteAttempt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"requested_engine":"legacy_agent_core"`) {
		t.Fatalf("engine readback must serialize deterministically: %s", raw)
	}
}

func TestKimiEngineReadbackReflectsOverrideWithoutEnvValues(t *testing.T) {
	payload := `{"role":"meta","type":"system.version","version":"0.37.2"}` + "\n" + `{"role":"assistant","content":"OK"}`
	t.Run("explicit v2 override", func(t *testing.T) {
		t.Setenv("KIMI_CODE_LEGACY_FLAG", "0")
		bin, _ := fakeKimiCLIEngineProbe(t, payload)
		cfg := kimiCLITestConfig(t, bin)
		task := &Task{ID: "kimi-engine-v2", Model: "opus", Type: typeSequence, Dir: t.TempDir(), PreferRunner: kimiCLIRunnerName}
		beginRouteAttemptReadback(cfg, task, false)
		root := admitDirectInvoke(t, "", task)
		if _, _, err := invokeKimiCLI(context.Background(), root, cfg, task, "p"); err != nil {
			t.Fatal(err)
		}
		if task.LastRouteAttempt.RequestedEngine != "legacy_agent_core" || task.LastRouteAttempt.ActualEngine != "agent_v2" {
			t.Fatalf("override readback must pair the legacy request with the actual v2 engine: %+v", task.LastRouteAttempt)
		}
	})
	t.Run("opaque caller value never leaks", func(t *testing.T) {
		const sentinel = "caller-managed-secret-sentinel"
		t.Setenv("KIMI_CODE_LEGACY_FLAG", sentinel)
		bin, _ := fakeKimiCLIEngineProbe(t, payload)
		cfg := kimiCLITestConfig(t, bin)
		task := &Task{ID: "kimi-engine-opaque", Model: "opus", Type: typeSequence, Dir: t.TempDir(), PreferRunner: kimiCLIRunnerName}
		beginRouteAttemptReadback(cfg, task, false)
		root := admitDirectInvoke(t, "", task)
		if _, _, err := invokeKimiCLI(context.Background(), root, cfg, task, "p"); err != nil {
			t.Fatal(err)
		}
		if task.LastRouteAttempt.ActualEngine == "" || strings.Contains(task.LastRouteAttempt.ActualEngine, sentinel) ||
			task.LastRouteAttempt.RequestedEngine == "" || strings.Contains(task.LastRouteAttempt.RequestedEngine, sentinel) {
			t.Fatalf("engine readback must be symbolic and never echo environment values: %+v", task.LastRouteAttempt)
		}
		detail := withRouteAttempt(map[string]any{}, task)
		raw, err := json.Marshal(detail)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), sentinel) {
			t.Fatalf("event readback must not echo environment values: %s", raw)
		}
	})
}
