package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const grokOIDCNoAuthContextDiagnostic = "HTTP 401 Unauthorized: Invalid or expired credentials; auth_kind=none; upstream=Unauthenticated; reason=no auth context"

const grokOIDCNoAuthContextClosedMetadata = "Internal error: \"Unauthorized (401) from https://cli-chat-proxy.grok.com/v1/responses: Invalid or expired credentials " +
	"(auth_kind=none, x_xai_token_auth=xai-grok-cli, upstream=Unauthenticated, reason=no auth context)\n\n" +
	"  Model:     grok-4.6\n" +
	"  Auth:      Oidc\n" +
	"  Version:   1.0.4\n" +
	"  Available: grok-4.6\""

func ownerBackendGrokTask(t *testing.T, root string, cfg *Config, dir string) *Task {
	t.Helper()
	task := newTask(root, cfg, typeSequence, "owner backend grok oidc", dir, []string{"implement"}, 1)
	task.Model = "opus"
	task.RouteClass = routeClassBackend
	route, ok := resolveOwnerRoute(cfg, task)
	if !ok || route.Name != "opus_backend" || !pinOwnerPrimaryRoute(task, route) {
		t.Fatalf("failed to freeze resolver-proven Owner Grok leg: route=%+v ok=%v task=%+v", route, ok, task)
	}
	return task
}

func fakeGrokBuildProbeFailure(t *testing.T, stderr string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "grok")
	script := "#!/bin/sh\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$arg\" = 'models' ]; then\n" +
		"    printf '%s\\n' " + shSingleQuote(stderr) + " >&2\n" +
		"    exit 1\n" +
		"  fi\n" +
		"done\n" +
		"exit 97\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestGrokAuthPreflightMixedStderrDoesNotOpenCircuit(t *testing.T) {
	root := testRoot(t)
	bin := fakeGrokBuildProbeFailure(t, grokOIDCNoAuthContextDiagnostic+"\nanalysis: ordinary wrapper or prompt-carried prose")
	cfg := policyTestConfig()
	cfg.GrokBuildBin = bin
	err := ensureGrokBuildAuth(context.Background(), root, cfg, "grok-4.6")
	if err == nil || isGrokBuildAuthProbeError(err) {
		t.Fatalf("mixed stderr must remain a non-auth preflight failure: %T %v", err, err)
	}
	if cd := loadEngineCooldown(root, grokBuildCooldownName); grokBuildAuthCooldownActive(cd, time.Now()) {
		t.Fatalf("mixed preflight stderr must not open the Grok auth circuit: %+v", cd)
	}
}

func TestRunTaskGrokMixedStderrIncompleteObservationOpensNoCircuit(t *testing.T) {
	root := testRoot(t)
	stderr := grokOIDCNoAuthContextDiagnostic + "\nanalysis: ordinary wrapper or prompt-carried prose"
	bin, _, _ := fakeGrokBuild(t, `{"type":"system.version","version":"1.0.4"}`, stderr, 1)
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
	if got.LastRouteAttempt == nil || !got.LastRouteAttempt.ObservationSeen || got.LastRouteAttempt.ObservationOK {
		t.Fatalf("mixed stderr must remain an incomplete observation: %+v", got.LastRouteAttempt)
	}
	if got.LastRouteAttempt.FailureClass == string(failureAuth) || strings.HasPrefix(got.LastError, "[auth]") {
		t.Fatalf("mixed stderr must return non-auth: %+v", got)
	}
	if cd := loadEngineCooldown(root, grokBuildCooldownName); grokBuildAuthCooldownActive(cd, time.Now()) {
		t.Fatalf("mixed model stderr must not open the Grok auth circuit: %+v", cd)
	}
}

func TestRunTaskGrokClosedMetadataAuthIsCompleteAndOpensCircuit(t *testing.T) {
	root := testRoot(t)
	bin, _, _ := fakeGrokBuild(t, `{"type":"system.version","version":"1.0.4"}`, grokOIDCNoAuthContextClosedMetadata, 1)
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
	if got.Status != statusHeld || got.Attempts != 0 || got.LastRouteAttempt == nil ||
		!got.LastRouteAttempt.ObservationSeen || !got.LastRouteAttempt.ObservationOK ||
		got.LastRouteAttempt.SemanticEvents != 0 || got.LastRouteAttempt.ModelEvents != 0 ||
		got.LastRouteAttempt.ToolEvents != 0 || got.LastRouteAttempt.FailureClass != string(failureAuth) {
		t.Fatalf("closed real OIDC metadata shape must be complete zero-work authentication: %+v", got)
	}
	if cd := loadEngineCooldown(root, grokBuildCooldownName); !grokBuildAuthCooldownActive(cd, time.Now()) {
		t.Fatalf("closed real OIDC metadata shape must open the Grok auth circuit: %+v", cd)
	}
}

func TestGrokSystemVersionOnlyIsCompleteStreamIncompleteMetadata(t *testing.T) {
	res := parseGrokBuildJSONL(`{"type":"system.version","version":"1.0.4"}`)
	if res == nil || !res.IsError || res.Subtype != "grok_build_stream_incomplete" {
		t.Fatalf("version-only stream must remain stream_incomplete: %+v", res)
	}
	if !res.ObservationComplete || res.SemanticEvents != 0 || res.ModelEvents != 0 ||
		res.ToolEvents != 0 || res.TerminalEvents != 0 {
		t.Fatalf("system.version must be complete nonsemantic invocation metadata: %+v", res)
	}

	proof := safeFixtureProof(t)
	proof.SemanticEvents = res.SemanticEvents
	proof.ModelEvents = res.ModelEvents
	proof.ToolEvents = res.ToolEvents
	proof.ObservationComplete = res.ObservationComplete
	if _, err := authorizePolicyFallback(proof); err != nil {
		t.Fatalf("version-only observation should become eligible only after the remaining proof axes pass: %v", err)
	}
}

func TestRunTaskOwnerGrokOIDCNoAuthContextHoldsSameCardAndOpensCircuit(t *testing.T) {
	root := testRoot(t)
	bin, _, _ := fakeGrokBuild(t, `{"type":"system.version","version":"1.0.4"}`, grokOIDCNoAuthContextDiagnostic, 1)
	cfg := policyTestConfig()
	cfg.GrokBuildBin = bin
	productDir := t.TempDir()
	task := ownerBackendGrokTask(t, root, cfg, productDir)
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	emitTaskEvent(root, task.ID, evQueued, "test", statusQueued, 0, map[string]any{"packet": "grok_oidc"})

	if err := runTaskVia(context.Background(), root, cfg, task, grokBuildRunnerName); err != nil {
		t.Fatal(err)
	}
	got, err := loadTask(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusHeld || got.ID != task.ID || got.PreferRunner != grokBuildRunnerName ||
		got.CodexModel != "" || got.OwnerRouteName != "opus_backend" || got.OwnerRouteLeg != 1 ||
		got.RouteReason != routeReasonGrokOpusBackend || got.Attempts != 0 ||
		!strings.HasPrefix(got.LastError, "[auth]") || !strings.Contains(got.LastError, "Invalid or expired credentials") {
		t.Fatalf("exact OIDC terminal must hold the same Grok card without fallback/attempt burn: %+v", got)
	}
	if got.LastRouteAttempt == nil || !got.LastRouteAttempt.ObservationSeen || !got.LastRouteAttempt.ObservationOK ||
		got.LastRouteAttempt.SemanticEvents != 0 || got.LastRouteAttempt.ModelEvents != 0 ||
		got.LastRouteAttempt.ToolEvents != 0 {
		t.Fatalf("exact OIDC terminal must be a complete zero-work observation: %+v", got.LastRouteAttempt)
	}
	brief := toBrief(cfg, got, time.Now())
	if brief.Runner != grokBuildRunnerName || brief.Model != "grok-4.6" || brief.Effort != "xhigh" {
		t.Fatalf("auth-held card must remain visibly owned by the Grok leg: %+v", brief)
	}
	tasks, err := loadBoardTasks(root)
	if err != nil || len(tasks) != 1 || tasks[0].ID != task.ID {
		t.Fatalf("auth hold must keep one task record and create no clone: tasks=%+v err=%v", tasks, err)
	}
	events, _, err := loadTaskEvents(root, task.ID)
	if err != nil || len(events) < 3 {
		t.Fatalf("auth hold history missing: events=%+v err=%v", events, err)
	}
	last := events[len(events)-1]
	if last.Type != evHeld || last.Actor != "runner:classifier" ||
		last.Detail["failure_class"] != string(failureAuth) || last.Detail["reason"] != "auth_class_held" ||
		last.Detail["requested_provider"] != grokBuildRunnerName || last.Detail["actual_provider"] != grokBuildRunnerName ||
		last.Detail["requested_model"] != "grok-4.6" || last.Detail["actual_model"] != "grok-4.6" ||
		last.Detail["requested_effort"] != "xhigh" || last.Detail["actual_effort"] != "xhigh" ||
		last.Detail["owner_route_name"] != "opus_backend" || last.Detail["owner_route_leg"] != float64(1) ||
		last.Detail["attempt"] != float64(0) {
		t.Fatalf("auth hold did not preserve the classified terminal receipt: %+v", last)
	}
	cd := loadEngineCooldown(root, grokBuildCooldownName)
	if cd == nil || !grokBuildAuthCooldownActive(cd, time.Now()) {
		t.Fatalf("exact OIDC terminal must open the Grok auth circuit: %+v", cd)
	}
	if data, err := os.ReadFile(eventsPath(root, task.ID)); err != nil || strings.Contains(strings.ToLower(string(data)), "token=") {
		t.Fatalf("event ledger must exist without recording token material: err=%v", err)
	}
}

func TestGrokOIDCNoAuthContextFallbackFailsClosedOutsideExactProof(t *testing.T) {
	t.Run("standalone explicit Grok", func(t *testing.T) {
		root := testRoot(t)
		bin, _, _ := fakeGrokBuild(t, `{"type":"system.version","version":"1.0.4"}`, grokOIDCNoAuthContextDiagnostic, 1)
		cfg := policyTestConfig()
		cfg.GrokBuildBin = bin
		cfg.MaxAttempts = 1
		task := newTask(root, cfg, typeSequence, "explicit Grok", t.TempDir(), []string{"implement"}, 1)
		task.PreferRunner = grokBuildRunnerName
		task.RunnerExplicit = true
		task.GrokModel = "grok-4.6"
		task.GrokEffort = "xhigh"
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
		if got.PreferRunner != grokBuildRunnerName || got.CodexModel != "" ||
			(got.Status != statusHeld && got.Status != statusFailed) {
			t.Fatalf("standalone explicit Grok must fail closed without fallback: %+v", got)
		}
	})

	t.Run("auth-like task prompt and unrelated failure", func(t *testing.T) {
		root := testRoot(t)
		bin, _, _ := fakeGrokBuild(t, `{"type":"system.version","version":"1.0.4"}`, "permission denied", 1)
		cfg := policyTestConfig()
		cfg.GrokBuildBin = bin
		cfg.MaxAttempts = 1
		task := ownerBackendGrokTask(t, root, cfg, t.TempDir())
		task.Prompts = []string{"quoted diagnostic only: " + grokOIDCNoAuthContextDiagnostic}
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
		if got.PreferRunner == "codex" || got.CodexModel != "" || got.OwnerRouteLeg != 1 {
			t.Fatalf("prompt-carried auth text must not authorize fallback: %+v", got)
		}
	})

	for _, tc := range []struct {
		name       string
		payload    string
		diagnostic string
	}{
		{name: "missing upstream field", payload: `{"type":"system.version","version":"1.0.4"}`,
			diagnostic: "HTTP 401 Unauthorized: Invalid or expired credentials; auth_kind=none; reason=no auth context"},
		{name: "mixed semantic event", payload: `{"type":"system.version","version":"1.0.4"}` + "\n" + `{"type":"text","data":"work started"}`,
			diagnostic: grokOIDCNoAuthContextDiagnostic},
		{name: "model event", payload: `{"type":"system.version","version":"1.0.4"}` + "\n" + `{"type":"model_start"}`,
			diagnostic: grokOIDCNoAuthContextDiagnostic},
		{name: "tool event", payload: `{"type":"system.version","version":"1.0.4"}` + "\n" + `{"type":"tool_call"}`,
			diagnostic: grokOIDCNoAuthContextDiagnostic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := testRoot(t)
			bin, _, _ := fakeGrokBuild(t, tc.payload, tc.diagnostic, 1)
			cfg := policyTestConfig()
			cfg.GrokBuildBin = bin
			cfg.MaxAttempts = 1
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
			if got.PreferRunner == "codex" || got.CodexModel != "" || got.OwnerRouteLeg != 1 {
				t.Fatalf("unsafe diagnostic/observation must not authorize fallback: %+v", got)
			}
			if cd := loadEngineCooldown(root, grokBuildCooldownName); grokBuildAuthCooldownActive(cd, time.Now()) {
				t.Fatalf("partial or mixed diagnostics must not open the Grok auth circuit: %+v", cd)
			}
		})
	}
}

func TestGrokOIDCNoAuthContextProofBlocksWorkspaceAndResidue(t *testing.T) {
	versionOnly := parseGrokBuildJSONL(`{"type":"system.version","version":"1.0.4"}`)
	base := safeFixtureProof(t)
	base.SemanticEvents = versionOnly.SemanticEvents
	base.ModelEvents = versionOnly.ModelEvents
	base.ToolEvents = versionOnly.ToolEvents
	base.ObservationComplete = versionOnly.ObservationComplete

	t.Run("workspace delta", func(t *testing.T) {
		proof := base
		before, after := *base.Before, *base.After
		proof.Before, proof.After = &before, &after
		proof.After.Digest = "changed-after-provider"
		if _, err := authorizePolicyFallback(proof); err == nil || !strings.Contains(err.Error(), "fingerprint changed") {
			t.Fatalf("workspace delta must block exact provider fallback: %v", err)
		}
	})
	t.Run("writer process residue", func(t *testing.T) {
		proof := base
		proof.ProcessResidue = true
		if _, err := authorizePolicyFallback(proof); err == nil || !strings.Contains(err.Error(), "process residue") {
			t.Fatalf("writer/process residue must block exact provider fallback: %v", err)
		}
	})
}

func TestGrokOIDCFallbackPatchUsesOnlyNormalPersistenceAPIs(t *testing.T) {
	// This source-level guard keeps this narrow policy repair from acquiring a second task writer.
	// The runtime test above separately proves that the same task ID advances and no clone appears.
	for _, file := range []string{"grok.go", "routing_policy.go", "runner.go"} {
		data, err := os.ReadFile(filepath.Join(".", file))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if strings.Contains(text, "os.WriteFile(taskPath(") || strings.Contains(text, "os.OpenFile(taskPath(") {
			t.Fatalf("%s directly mutates task JSON; use saveTask/event APIs", file)
		}
	}
}
