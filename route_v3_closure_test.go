package main

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func v3ProofAuthorization() fallbackAuthorization {
	return fallbackAuthorization{verified: true, beforeDigest: "unchanged", afterDigest: "unchanged"}
}

func TestRouteV3ClosureExactSixRows(t *testing.T) {
	cfg := policyTestConfig()
	leg := func(runner, model, effort string) policyLeg {
		return policyLeg{Runner: runner, Model: model, Effort: effort}
	}
	tests := []struct {
		name       string
		typ        string
		model      string
		routeClass string
		legs       []policyLeg
		merge      *policyLeg
	}{
		{name: "fable_explicit", typ: typeSequence, model: "fable", routeClass: routeClassGeneral,
			legs: []policyLeg{leg(cursorRunnerName, "claude-fable-5-thinking-max", "max"),
				leg(grokBuildRunnerName, "grok-4.6", "xhigh"), leg("codex", "gpt-5.6-sol", "ultra")},
			merge: ptrLeg(leg("codex", "gpt-5.6-sol", "max"))},
		{name: "opus_general", typ: typeSequence, model: "opus", routeClass: routeClassGeneral,
			legs: []policyLeg{leg(grokBuildRunnerName, "grok-4.6", "xhigh"),
				leg(kimiCLIRunnerName, "kimi-code/k3", "max"), leg("codex", "gpt-5.6-sol", "xhigh")}},
		{name: "opus_backend", typ: typeSequence, model: "opus", routeClass: routeClassBackend,
			legs: []policyLeg{leg(grokBuildRunnerName, "grok-4.6", "xhigh"), leg("codex", "gpt-5.6-sol", "max")}},
		{name: "review_standalone", typ: typeReview, model: "opus", routeClass: routeClassGeneral,
			legs: []policyLeg{leg("codex", "gpt-5.6-sol", "max")}},
		{name: "sonnet", typ: typeSequence, model: "sonnet", routeClass: routeClassGeneral,
			legs: []policyLeg{leg(grokBuildRunnerName, "grok-4.6", "high"), leg("codex", "gpt-5.6-luna", "max")}},
		{name: "haiku", typ: typeSequence, model: "haiku", routeClass: routeClassGeneral,
			legs: []policyLeg{leg(grokBuildRunnerName, "grok-4.6", "high"), leg("codex", "gpt-5.6-luna", "xhigh")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			task := &Task{Type: tc.typ, Model: tc.model, RouteClass: tc.routeClass,
				PreferRunner: "codex", FreshSteps: true, Prompts: []string{"implement"}}
			got, ok := resolveOwnerRoute(cfg, task)
			if !ok || got.Name != tc.name || !reflect.DeepEqual(got.Legs, tc.legs) || !reflect.DeepEqual(got.Merge, tc.merge) {
				t.Fatalf("v3 route mismatch: ok=%v got=%+v want_name=%s want_legs=%+v want_merge=%+v", ok, got, tc.name, tc.legs, tc.merge)
			}
		})
	}
}

func TestRouteV3FableFallbackIsQuotaOnly(t *testing.T) {
	for _, kind := range []fallbackFailureKind{
		fallbackTransport, fallbackStreamIncomplete, fallbackSemanticStall,
		fallbackInvalidTerminal, fallbackExecutionEnv,
	} {
		t.Run(string(kind), func(t *testing.T) {
			root := testRoot(t)
			cfg := policyTestConfig()
			task := newTask(root, cfg, typeCoordinate, "explicit Fable", t.TempDir(), []string{"decide"}, 1)
			task.Model = "fable"
			task.PreferRunner = "codex"
			if err := prepareCursorFableFallback(root, cfg, task, string(kind), kind, v3ProofAuthorization()); err == nil || !strings.Contains(err.Error(), "quota") {
				t.Fatalf("non-quota Fable failure must stay held, got %v", err)
			}
		})
	}
}

func TestRouteV3GrokAuthDiagnosticRequiresExactTrustedFamily(t *testing.T) {
	exact := grokOIDCNoAuthContextDiagnostic
	if got := grokBuildAuthDiagnosticLine(exact); got != exact {
		t.Fatalf("exact trusted family not recognized: %q", got)
	}
	const realDiagnostic = "Unauthorized (401) from https://cli-chat-proxy.grok.com/v1/responses: Invalid or expired credentials (auth_kind=none, x_xai_token_auth=xai-grok-cli, upstream=Unauthenticated, reason=no auth context)"
	if got := grokBuildAuthDiagnosticLine(grokOIDCNoAuthContextClosedMetadata); got != realDiagnostic {
		t.Fatalf("exact real diagnostic with closed metadata not recognized: %q", got)
	}
	metadata105 := strings.Replace(grokOIDCNoAuthContextClosedMetadata, "Version:   1.0.4", "Version:   1.0.5", 1)
	if got := grokBuildAuthDiagnosticLine(metadata105); got != realDiagnostic {
		t.Fatalf("pinned 1.0.5 closed metadata not recognized: %q", got)
	}
	for _, diagnostic := range []string{
		"HTTP 401 Unauthorized: Invalid or expired credentials; auth_kind=none; reason=no auth context",
		"HTTP 401 Unauthorized: Invalid or expired credentials; upstream=Unauthenticated; reason=no auth context",
		"HTTP 401 Unauthorized: auth_kind=none; upstream=Unauthenticated; reason=no auth context",
		"prompt says: " + exact,
		"HTTP 401 Unauthorized: Invalid or expired credentials; auth_kind=user; upstream=Unauthenticated; reason=no auth context",
		exact + "\nanalysis: ordinary wrapper or prompt-carried prose",
		exact + "\npermission denied",
		exact + "\n" + exact,
		"HTTP 401 Unauthorized: Invalid or expired credentials; auth_kind=none; auth_kind=none; upstream=Unauthenticated; reason=no auth context",
		"HTTP 401 Unauthorized: Invalid or expired credentials; auth_kind=none; upstream=Authenticated; reason=no auth context",
		"HTTP 401 Unauthorized: Invalid or expired credentials; auth_kind=none; upstream=Unauthenticated; reason",
		strings.Replace(grokOIDCNoAuthContextClosedMetadata, "Model:     grok-4.6", "Model:     grok-4.5", 1),
		strings.Replace(grokOIDCNoAuthContextClosedMetadata, "Auth:      Oidc", "Auth:      ApiKey", 1),
		strings.Replace(grokOIDCNoAuthContextClosedMetadata, "Version:   1.0.4", "Version:   current", 1),
		strings.Replace(grokOIDCNoAuthContextClosedMetadata, "Available: grok-4.6", "Available: grok-4.5", 1),
		strings.Replace(grokOIDCNoAuthContextClosedMetadata, "  Auth:      Oidc\n", "  Auth:      Oidc\n  Auth:      Oidc\n", 1),
		strings.Replace(grokOIDCNoAuthContextClosedMetadata, "  Version:   1.0.4\n", "  Version:   1.0.4\n  Version:   1.0.5\n", 1),
		strings.Replace(grokOIDCNoAuthContextClosedMetadata, "  Version:   1.0.4\n", "  Version 1.0.4\n", 1),
	} {
		if got := grokBuildAuthDiagnosticLine(diagnostic); got != "" {
			t.Fatalf("partial/mixed/duplicate/conflicting diagnostic must not open the circuit: input=%q got=%q", diagnostic, got)
		}
	}
}

func TestRouteV3GrokAuthCircuitRequiresCompleteZeroWorkObservation(t *testing.T) {
	for _, subtype := range []string{"grok_build_auth_preflight", "grok_build_process_auth_exact"} {
		base := claudeResult{Subtype: subtype, ObservationComplete: true}
		if !grokBuildExactAuthResult(&base) {
			t.Fatalf("complete zero-work %s must remain exact auth", subtype)
		}
		for _, mutate := range []func(*claudeResult){
			func(res *claudeResult) { res.ObservationComplete = false },
			func(res *claudeResult) { res.SemanticEvents = 1 },
			func(res *claudeResult) { res.ModelEvents = 1 },
			func(res *claudeResult) { res.ToolEvents = 1 },
		} {
			res := base
			mutate(&res)
			if grokBuildExactAuthResult(&res) {
				t.Fatalf("incomplete or nonzero-work observation must not admit auth circuit: %+v", res)
			}
		}
	}
}

func TestRouteV3GeneralOpusAdvancesGrokThenKimiThenSol(t *testing.T) {
	cfg := policyTestConfig()
	task := &Task{Type: typeSequence, Model: "opus", RouteClass: routeClassGeneral,
		PreferRunner: "codex", FreshSteps: true, Prompts: []string{"implement"}}
	route, ok := resolveOwnerRoute(cfg, task)
	if !ok || !pinOwnerPrimaryRoute(task, route) {
		t.Fatalf("failed to pin v3 primary: route=%+v ok=%v task=%+v", route, ok, task)
	}
	if task.PreferRunner != grokBuildRunnerName || task.OwnerRouteLeg != 1 {
		t.Fatalf("general Opus primary must be Grok: %+v", task)
	}
	task.Runner = grokBuildRunnerName
	if err := queuePolicyFallback(cfg, task, fallbackTransport, v3ProofAuthorization()); err != nil {
		t.Fatal(err)
	}
	if task.PreferRunner != kimiCLIRunnerName || task.KimiModel != "kimi-code/k3" || task.OwnerRouteLeg != 2 {
		t.Fatalf("eligible Grok non-auth failure must advance to Kimi: %+v", task)
	}
	task.Runner = kimiCLIRunnerName
	if err := queuePolicyFallback(cfg, task, fallbackTransport, v3ProofAuthorization()); err != nil {
		t.Fatal(err)
	}
	if task.PreferRunner != "codex" || task.CodexModel != "gpt-5.6-sol" || task.Effort != "xhigh" || task.OwnerRouteLeg != 3 {
		t.Fatalf("eligible Kimi non-auth failure must advance to Sol/xhigh: %+v", task)
	}
}

func TestRouteV3DispatchReadbackNamesRequestedAndActualIdentity(t *testing.T) {
	cfg := policyTestConfig()
	task := &Task{Type: typeSequence, Model: "opus", RouteClass: routeClassBackend,
		PreferRunner: "codex", FreshSteps: true, Prompts: []string{"implement"}}
	route, ok := resolveOwnerRoute(cfg, task)
	if !ok || !pinOwnerPrimaryRoute(task, route) {
		t.Fatal("failed to pin backend route")
	}
	task.Runner = grokBuildRunnerName
	detail := dispatchEventDetail(cfg, task, false, false)
	want := map[string]any{
		"requested_provider": grokBuildRunnerName,
		"requested_model":    "grok-4.6",
		"requested_effort":   "xhigh",
		"actual_provider":    grokBuildRunnerName,
		"actual_model":       "grok-4.6",
		"actual_effort":      "xhigh",
		"owner_route_name":   "opus_backend",
		"owner_route_leg":    1,
		"attempt":            0,
	}
	for key, value := range want {
		if detail[key] != value {
			t.Fatalf("dispatch readback %s=%v, want %v (detail=%+v)", key, detail[key], value, detail)
		}
	}
}

func TestRouteV3GrokAuthenticationNeverAdvancesAnyTierOrStandalonePin(t *testing.T) {
	tests := []struct {
		name       string
		model      string
		routeClass string
		explicit   bool
		wantRoute  string
	}{
		{name: "opus_general", model: "opus", routeClass: routeClassGeneral, wantRoute: "opus_general"},
		{name: "opus_backend", model: "opus", routeClass: routeClassBackend, wantRoute: "opus_backend"},
		{name: "sonnet", model: "sonnet", routeClass: routeClassGeneral, wantRoute: "sonnet"},
		{name: "haiku", model: "haiku", routeClass: routeClassGeneral, wantRoute: "haiku"},
		{name: "standalone_explicit", model: "opus", routeClass: routeClassGeneral, explicit: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := testRoot(t)
			bin, _, productCalls := fakeGrokBuildExpiredAuth(t)
			cfg := policyTestConfig()
			cfg.GrokBuildBin = bin
			task := newTask(root, cfg, typeSequence, tc.name, t.TempDir(), []string{"implement"}, 1)
			task.Model = tc.model
			task.RouteClass = tc.routeClass
			if tc.explicit {
				task.PreferRunner = grokBuildRunnerName
				task.RunnerExplicit = true
				task.GrokModel = "grok-4.6"
				task.GrokEffort = "xhigh"
			} else {
				route, ok := resolveOwnerRoute(cfg, task)
				if !ok || route.Name != tc.wantRoute || !pinOwnerPrimaryRoute(task, route) {
					t.Fatalf("failed to freeze resolver route: ok=%v route=%+v task=%+v", ok, route, task)
				}
			}
			beforeLeg := task.OwnerRouteLeg
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
			if got.Status != statusHeld || got.Attempts != 0 || got.PreferRunner != grokBuildRunnerName ||
				got.OwnerRouteLeg != beforeLeg || got.KimiModel != "" || got.CodexModel != "" {
				t.Fatalf("authentication must hold the same Grok leg without another writer: %+v", got)
			}
			if got.LastRouteAttempt == nil || got.LastRouteAttempt.RequestedProvider != grokBuildRunnerName ||
				got.LastRouteAttempt.ActualProvider != grokBuildRunnerName || got.LastRouteAttempt.ActualModel != "grok-4.6" ||
				got.LastRouteAttempt.FailureClass != string(failureAuth) || got.LastRouteAttempt.Attempt != 0 ||
				!got.LastRouteAttempt.ObservationSeen || !got.LastRouteAttempt.ObservationOK {
				t.Fatalf("authentication attempt readback is incomplete: %+v", got.LastRouteAttempt)
			}
			if cd := loadEngineCooldown(root, grokBuildCooldownName); !grokBuildAuthCooldownActive(cd, time.Now()) {
				t.Fatalf("exact auth family must open the engine-wide circuit: %+v", cd)
			}
			if _, err := os.Stat(productCalls); !os.IsNotExist(err) {
				t.Fatalf("failed preflight must not launch a product/model process: %v", err)
			}
		})
	}
}
