package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func finalOwnerTask(model, routeClass, risk string) *Task {
	return &Task{
		ID:           "final-owner-fixture",
		Type:         typeSequence,
		Model:        model,
		RouteClass:   routeClass,
		RiskClass:    risk,
		PreferRunner: "codex",
		FreshSteps:   true,
		Prompts:      []string{"bounded work"},
	}
}

func finalOwnerLeg(runner, model, effort, stage string, readOnly bool) policyLeg {
	return policyLeg{Runner: runner, Model: model, Effort: effort, Stage: stage, ReadOnly: readOnly}
}

func TestFinalOwnerClosedAuthGrammarRejectsBareMultilineFooter(t *testing.T) {
	bareWithFooter := grokOIDCNoAuthContextDiagnostic + "\n" +
		"Model: grok-4.6\nAuth: Oidc\nVersion: 1.0.5\nAvailable: grok-4.6"
	if got := grokBuildAuthDiagnosticLine(bareWithFooter); got != "" {
		t.Fatalf("an unquoted multiline diagnostic must not authorize auth: %q", got)
	}
	if got := grokBuildAuthDiagnosticLine(grokOIDCNoAuthContextDiagnostic); got != grokOIDCNoAuthContextDiagnostic {
		t.Fatalf("the exact one-line bare diagnostic must remain accepted: %q", got)
	}
	if got := grokBuildAuthDiagnosticLine(grokOIDCNoAuthContextClosedMetadata); got == "" {
		t.Fatal("the exact quoted OIDC wrapper must remain accepted")
	}
}

func TestFinalOwnerMatrixResolvesEveryEffectiveRow(t *testing.T) {
	cfg := policyTestConfig()
	tests := []struct {
		name string
		task *Task
		want ownerRoute
	}{
		{
			name: "fable",
			task: finalOwnerTask("fable", "", ""),
			want: ownerRoute{Name: "fable_explicit", RiskClass: riskClassOrdinary, Legs: []policyLeg{
				finalOwnerLeg(cursorRunnerName, "claude-fable-5-thinking-max", "max", routeStagePrimary, true),
				finalOwnerLeg(grokBuildRunnerName, "grok-4.6", "xhigh", routeStageFableAnswer, true),
				finalOwnerLeg("codex", "gpt-5.6-sol", "ultra", routeStageFableMerge, true),
			}},
		},
		{
			name: "opus non-backend",
			task: finalOwnerTask("opus", routeClassGeneral, riskClassOrdinary),
			want: ownerRoute{Name: "opus_non_backend", RiskClass: riskClassOrdinary, Legs: []policyLeg{
				finalOwnerLeg(grokBuildRunnerName, "grok-4.6", "xhigh", routeStagePrimary, false),
				finalOwnerLeg(kimiCLIRunnerName, "kimi-code/k3", "max", routeStageFallbackReview, false),
			}, ConditionalSol: ptrLeg(finalOwnerLeg("codex", "gpt-5.6-sol", "xhigh", routeStageConditionalEscalation, true))},
		},
		{
			name: "opus backend ordinary",
			task: finalOwnerTask("opus", routeClassBackend, riskClassOrdinary),
			want: ownerRoute{Name: "opus_backend_ordinary", RiskClass: riskClassOrdinary,
				Legs:           []policyLeg{finalOwnerLeg(grokBuildRunnerName, "grok-4.6", "xhigh", routeStagePrimary, false)},
				Review:         ptrLeg(finalOwnerLeg(kimiCLIRunnerName, "kimi-code/k3", "max", routeStageAdversarialReview, true)),
				ConditionalSol: ptrLeg(finalOwnerLeg("codex", "gpt-5.6-sol", "xhigh", routeStageConditionalRelease, true))},
		},
		{
			name: "opus backend high-risk",
			task: finalOwnerTask("opus", routeClassBackend, riskClassHigh),
			want: ownerRoute{Name: "opus_backend_high_risk", RiskClass: riskClassHigh,
				Legs:        []policyLeg{finalOwnerLeg(grokBuildRunnerName, "grok-4.6", "xhigh", routeStagePrimary, false)},
				Review:      ptrLeg(finalOwnerLeg(kimiCLIRunnerName, "kimi-code/k3", "max", routeStageSecondView, true)),
				ReleaseGate: ptrLeg(finalOwnerLeg("codex", "gpt-5.6-sol", "max", routeStageReleaseGate, true))},
		},
		{
			name: "standalone ordinary review",
			task: func() *Task {
				q := finalOwnerTask("opus", routeClassGeneral, riskClassOrdinary)
				q.Type = typeReview
				return q
			}(),
			want: ownerRoute{Name: "review_standalone_ordinary", RiskClass: riskClassOrdinary,
				Legs: []policyLeg{finalOwnerLeg(kimiCLIRunnerName, "kimi-code/k3", "max", routeStageStandaloneReview, true)}},
		},
		{
			name: "standalone production review",
			task: func() *Task {
				q := finalOwnerTask("opus", routeClassGeneral, riskClassProduction)
				q.Type = typeReview
				return q
			}(),
			want: ownerRoute{Name: "review_standalone_critical", RiskClass: riskClassProduction,
				Legs: []policyLeg{finalOwnerLeg("codex", "gpt-5.6-sol", "max", routeStageStandaloneReview, true)}},
		},
		{
			name: "sonnet",
			task: finalOwnerTask("sonnet", routeClassGeneral, riskClassOrdinary),
			want: ownerRoute{Name: "sonnet", RiskClass: riskClassOrdinary, Legs: []policyLeg{
				finalOwnerLeg(grokBuildRunnerName, "grok-4.6", "high", routeStagePrimary, false),
				finalOwnerLeg(kimiCLIRunnerName, "kimi-code/k3", "max", routeStageFallbackReview, false),
			}},
		},
		{
			name: "haiku",
			task: finalOwnerTask("haiku", routeClassGeneral, riskClassOrdinary),
			want: ownerRoute{Name: "haiku", RiskClass: riskClassOrdinary, Legs: []policyLeg{
				finalOwnerLeg(grokBuildRunnerName, "grok-4.6", "high", routeStagePrimary, false),
				finalOwnerLeg(kimiCLIRunnerName, "kimi-code/k3", "max", routeStageFallbackReview, false),
			}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := resolveOwnerRoute(cfg, tc.task)
			if !ok || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("route mismatch: ok=%v\n got=%+v\nwant=%+v", ok, got, tc.want)
			}
		})
	}
}

func TestFinalOwnerFableIsOneCodexReadOnlyTerminalChain(t *testing.T) {
	cfg := policyTestConfig()
	task := finalOwnerTask("fable", routeClassBackend, riskClassHigh)
	route, ok := resolveOwnerRoute(cfg, task)
	if !ok || route.Name != "fable_explicit" || route.RiskClass != riskClassOrdinary || route.Merge != nil || route.Review != nil || route.ReleaseGate != nil {
		t.Fatalf("Fable must resolve as its dedicated general read-only role: %+v ok=%v", route, ok)
	}
	if got := ownerModelRoute(route); !strings.Contains(got, "Grok answer → Sol/ultra adversarial merge → terminal") || strings.Contains(got, "Sol/max") || strings.Contains(strings.ToLower(got), "backend") {
		t.Fatalf("Fable board chain is not terminal after its one Codex call: %q", got)
	}
	for _, kind := range []fallbackFailureKind{fallbackQuota, fallbackTransport, fallbackStreamIncomplete, fallbackExecutionEnv} {
		if !fableFallbackKindEligible(kind) {
			t.Fatalf("eligible presemantic trigger %q was rejected", kind)
		}
	}
	for _, kind := range []fallbackFailureKind{fallbackSemanticStall, fallbackInvalidTerminal} {
		if fableFallbackKindEligible(kind) {
			t.Fatalf("semantic/acceptance failure %q must not broaden Fable fallback", kind)
		}
	}
	merger := &Task{FableReviewerMerger: true, AutomaticSolCalls: 1, ReviewAfter: false}
	if err := reserveAutomaticSolCall(merger, routeStageFableMerge); err == nil {
		t.Fatal("Fable must have no second automatic Sol call")
	}
	if disposition := fableMergeDisposition("conclusion\n```json\n{\"verdict\":\"hold\",\"confidence\":\"low\",\"p0\":1,\"p1\":0}\n```"); disposition != fableMergeHoldOwner {
		t.Fatalf("unresolved P0/P1 must hold for Owner, got %q", disposition)
	}
	for name, result := range map[string]string{
		"missing owner hold":    `{"verdict":"pass","confidence":"high","p0":0,"p1":0,"uncertainty":"none"}`,
		"missing uncertainty":   `{"verdict":"pass","confidence":"high","p0":0,"p1":0,"owner_hold":false}`,
		"remaining uncertainty": `{"verdict":"pass","confidence":"medium","p0":0,"p1":0,"owner_hold":false,"uncertainty":"acceptance evidence incomplete"}`,
	} {
		if disposition := fableMergeDisposition("```json\n" + result + "\n```"); disposition != fableMergeHoldOwner {
			t.Fatalf("%s must fail closed to Owner hold, got %q", name, disposition)
		}
	}
}

func TestFinalOwnerBackendRiskAndDeterministicSolGate(t *testing.T) {
	cfg := policyTestConfig()
	for _, risk := range []string{"", "ambiguous", riskClassHigh, riskClassCritical, riskClassProduction} {
		task := finalOwnerTask("opus", routeClassBackend, risk)
		route, ok := resolveOwnerRoute(cfg, task)
		if !ok || route.Name != "opus_backend_high_risk" || route.RiskClass != riskClassHigh || route.ReleaseGate == nil || route.ReleaseGate.Effort != "max" {
			t.Fatalf("backend risk %q must fail closed high-risk: %+v ok=%v", risk, route, ok)
		}
	}
	ordinary := finalOwnerTask("opus", routeClassBackend, riskClassOrdinary)
	ordinary.ID = "ordinary-not-sampled"
	for deterministicSolSample(ordinary.ID, 20) {
		ordinary.ID += "x"
	}
	route, ok := resolveOwnerRoute(cfg, ordinary)
	if !ok || route.Name != "opus_backend_ordinary" || route.ReleaseGate != nil {
		t.Fatalf("unsampled ordinary backend must end after Kimi unless escalated: %+v", route)
	}
	ordinary.SolEscalationReason = solEscalationDisagreement
	route, ok = resolveOwnerRoute(cfg, ordinary)
	if !ok || route.ReleaseGate == nil || route.ReleaseGate.Effort != "xhigh" {
		t.Fatalf("Grok-Kimi disagreement must require Sol/xhigh: %+v", route)
	}
	ordinary.SolEscalationReason = "transport"
	if route, _ := resolveOwnerRoute(cfg, ordinary); route.ReleaseGate != nil {
		t.Fatalf("unlisted escalation must not become an automatic Sol gate: %+v", route)
	}

	nonBackend := finalOwnerTask("opus", routeClassGeneral, riskClassOrdinary)
	nonBackend.SolEscalationReason = solEscalationAcceptanceFailed
	if route, _ := resolveOwnerRoute(cfg, nonBackend); route.ReleaseGate == nil || route.ReleaseGate.Effort != "xhigh" {
		t.Fatalf("non-backend failed acceptance must activate its explicit Sol/xhigh gate: %+v", route)
	}
	explicitHighRisk := finalOwnerTask("opus", routeClassGeneral, riskClassHigh)
	if route, _ := resolveOwnerRoute(cfg, explicitHighRisk); route.ReleaseGate == nil || route.ReleaseGate.Effort != "xhigh" {
		t.Fatalf("non-backend explicit high-risk classification must activate Sol/xhigh: %+v", route)
	}

	sampled := finalOwnerTask("opus", routeClassBackend, riskClassOrdinary)
	sampled.ID = "ordinary-sampled"
	for !deterministicSolSample(sampled.ID, 20) {
		sampled.ID += "x"
	}
	if route, _ := resolveOwnerRoute(cfg, sampled); route.ReleaseGate == nil || route.ReleaseGate.Effort != "xhigh" {
		t.Fatalf("deterministic 20 percent sample must require Sol/xhigh: id=%s route=%+v", sampled.ID, route)
	}
}

func TestFinalOwnerKimiProviderRedundancyIsNotIndependentOpinion(t *testing.T) {
	kimiCLI := finalOwnerLeg(kimiCLIRunnerName, "kimi-code/k3", "max", routeStageAdversarialReview, true)
	openCodeGo := finalOwnerLeg("opencode", "opencode-go/kimi-k3", "high", routeStageSecondView, true)
	if independentModelOpinion(kimiCLI, openCodeGo) {
		t.Fatal("Kimi CLI and OpenCode Go Kimi K3 are capacity redundancy, not independent opinions")
	}
	if !independentModelOpinion(finalOwnerLeg(grokBuildRunnerName, "grok-4.6", "xhigh", routeStagePrimary, false), kimiCLI) {
		t.Fatal("Grok and Kimi must remain independent opinions")
	}
}

func TestFinalOwnerAutomaticCodexBudgetAndSingleCall(t *testing.T) {
	task := finalOwnerTask("opus", routeClassBackend, riskClassHigh)
	task.AutomaticCodex = true
	for _, tc := range []struct {
		name     string
		evidence automaticCodexBudgetEvidence
		want     bool
	}{
		{"below stop", automaticCodexBudgetEvidence{Available: true, UsedPercent: 64}, true},
		{"at stop", automaticCodexBudgetEvidence{Available: true, UsedPercent: 65}, false},
		{"unavailable", automaticCodexBudgetEvidence{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := automaticCodexBudgetAllowed(task, tc.evidence, 65); got != tc.want {
				t.Fatalf("allowed=%v want=%v", got, tc.want)
			}
		})
	}
	task.OwnerCriticalBypassReason = "Owner-pinned production authentication repair"
	if ok, reason := automaticCodexBudgetAllowed(task, automaticCodexBudgetEvidence{}, 65); !ok || !strings.Contains(reason, "Owner") {
		t.Fatalf("visible Owner-critical reason must be the only bypass: ok=%v reason=%q", ok, reason)
	}
	if err := reserveAutomaticSolCall(task, routeStageReleaseGate); err != nil || task.AutomaticSolCalls != 1 {
		t.Fatalf("first automatic Sol call must reserve exactly once: task=%+v err=%v", task, err)
	}
	if err := reserveAutomaticSolCall(task, routeStageConditionalRelease); err == nil || task.AutomaticSolCalls != 1 {
		t.Fatalf("second automatic Sol call must fail closed: task=%+v err=%v", task, err)
	}
	if err := beginAutomaticSolInvocation(task); err != nil || task.AutomaticSolInvocations != 1 {
		t.Fatalf("first automatic Sol invocation must be persisted exactly once: task=%+v err=%v", task, err)
	}
	if err := beginAutomaticSolInvocation(task); err == nil || task.AutomaticSolInvocations != 1 {
		t.Fatalf("automatic Sol retry/resume must fail closed before a second invocation: task=%+v err=%v", task, err)
	}
}

func TestFinalOwnerAutomaticCodexBudgetReadsOnlyCodexProvider(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage-history.jsonl")
	now := time.Now().UTC()
	var lines []byte
	var claudeOnly []byte
	for _, sample := range []map[string]any{
		{"provider": "claude", "sampledAt": now.Format(time.RFC3339), "usedPercent": 12, "windowMinutes": 300, "windowKind": "primary"},
		{"provider": "codex", "sampledAt": now.Add(time.Second).Format(time.RFC3339), "usedPercent": 66, "windowMinutes": 300, "windowKind": "primary"},
	} {
		line, err := json.Marshal(sample)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, append(line, '\n')...)
		if sample["provider"] == "claude" {
			claudeOnly = append(claudeOnly, append(line, '\n')...)
		}
	}
	if err := os.WriteFile(path, lines, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := policyTestConfig()
	cfg.UsageFeed = path
	cfg.UsageFeedMaxAgeMin = 90
	evidence := currentAutomaticCodexBudgetEvidence(cfg, now.Add(2*time.Second))
	if !evidence.Available || evidence.UsedPercent != 66 || evidence.Source != "usage_feed:codex" {
		t.Fatalf("automatic Codex gate consumed the wrong provider sample: %+v", evidence)
	}
	if ok, _ := automaticCodexBudgetAllowed(&Task{AutomaticCodex: true}, evidence, 65); ok {
		t.Fatal("codex=66% must stop even when claude=12%")
	}
	if err := os.WriteFile(path, claudeOnly, 0o600); err != nil {
		t.Fatal(err)
	}
	missing := currentAutomaticCodexBudgetEvidence(cfg, now.Add(2*time.Second))
	if missing.Available || !strings.Contains(missing.Reason, "codex") {
		t.Fatalf("a Claude-only feed must fail closed for automatic Codex: %+v", missing)
	}
}

func TestFinalOwnerRuntimeRefusesSecondAutomaticSolInvocation(t *testing.T) {
	root := testRoot(t)
	cfg := policyTestConfig()
	task := finalOwnerTask("opus", routeClassBackend, riskClassHigh)
	task.ID = "already-invoked-sol"
	task.Dir = t.TempDir()
	task.Status = statusQueued
	task.AutomaticCodex = true
	task.AutomaticSolCalls = 1
	task.AutomaticSolInvocations = 1
	task.CodexModel = "gpt-5.6-sol"
	task.Effort = "max"
	task.EffortExplicit = true
	task.OwnerRouteStage = routeStageReleaseGate
	task.OwnerCriticalBypassReason = "Owner-pinned critical runtime limit test"
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	if err := runTaskVia(context.Background(), root, cfg, task, "codex"); err != nil {
		t.Fatal(err)
	}
	got, err := findTaskAnywhere(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusHeld || got.AutomaticSolInvocations != 1 ||
		!strings.Contains(got.LastError, "automatic Sol invocation limit reached") {
		t.Fatalf("second automatic Sol invocation was not held before execution: %+v", got)
	}
}

func TestFinalOwnerLineageRefusesSecondSolReviewChild(t *testing.T) {
	root := testRoot(t)
	cfg := policyTestConfig()
	parent := finalOwnerTask("opus", routeClassBackend, riskClassOrdinary)
	parent.ID = "sol-consumed-lineage"
	parent.Dir = t.TempDir()
	parent.Status = statusHeld
	parent.ReviewAfter = true
	parent.RequiredReviews = []string{reviewStageSolXHigh}
	parent.AutomaticSolCalls = 1
	parent.AutomaticSolInvocations = 1
	if err := saveTask(root, parent); err != nil {
		t.Fatal(err)
	}
	child, err := ensureReviewAfterTask(root, cfg, parent, nil)
	if err == nil || child != nil || !strings.Contains(err.Error(), "already consumed by lineage") {
		t.Fatalf("a used lineage minted another Sol review child: child=%+v err=%v", child, err)
	}
	if existing, lookupErr := existingPlannedReviewChild(root, parent.ID, reviewStageSolXHigh); lookupErr != nil || existing != nil {
		t.Fatalf("second Sol child reached disk: child=%+v err=%v", existing, lookupErr)
	}
}

func TestFinalOwnerAdditiveTaskFieldsAreClosed(t *testing.T) {
	valid := finalOwnerTask("opus", routeClassBackend, riskClassHigh)
	valid.OwnerRouteStage = routeStageReleaseGate
	valid.FallbackReason = string(fallbackSemanticStall)
	valid.RequiredReviews = []string{reviewStageKimiSecondView, reviewStageSolMax}
	valid.CompletedReviews = []string{reviewStageKimiSecondView}
	valid.SolEscalationReason = solEscalationExplicitHighRisk
	if err := closedOwnerTaskStateError(valid); err != nil {
		t.Fatalf("valid closed state rejected: %v", err)
	}
	for _, mutate := range []func(*Task){
		func(q *Task) { q.OwnerRouteStage = "later" },
		func(q *Task) { q.FallbackReason = "maybe" },
		func(q *Task) { q.RiskClass = "maybe" },
		func(q *Task) { q.RequiredReviews = []string{"unknown-review"} },
		func(q *Task) { q.CompletedReviews = []string{reviewStageSolMax} },
		func(q *Task) { q.ReviewPlanStage, q.ReviewPlanRoot = reviewStageSolMax, "" },
		func(q *Task) { q.SolEscalationReason = "transport" },
		func(q *Task) { q.AutomaticSolCalls, q.AutomaticSolInvocations = 1, 2 },
		func(q *Task) {
			q.AutomaticCodex, q.AutomaticSolCalls = true, 1
			q.PreferRunner, q.CodexModel, q.Effort = "codex", "gpt-5.6-terra", "max"
			q.OwnerRouteStage = routeStageReleaseGate
		},
		func(q *Task) { q.RiskClass, q.OwnerCriticalBypassReason = riskClassOrdinary, "not critical" },
	} {
		probe := *valid
		probe.RequiredReviews = append([]string(nil), valid.RequiredReviews...)
		probe.CompletedReviews = append([]string(nil), valid.CompletedReviews...)
		mutate(&probe)
		if err := closedOwnerTaskStateError(&probe); err == nil {
			t.Fatalf("invalid additive state was accepted: %+v", probe)
		}
	}
}

func TestFinalOwnerFrontendGateAndBoardReadback(t *testing.T) {
	cfg := policyTestConfig()
	haiku := finalOwnerTask("haiku", routeClassGeneral, riskClassOrdinary)
	haiku.QualitySensitive = true
	haikuRoute, haikuOK := resolveOwnerRoute(cfg, haiku)
	if !haikuOK || len(haikuRoute.Legs) != 2 || haikuRoute.Legs[0].Effort != "high" ||
		haikuRoute.Legs[1].Runner != kimiCLIRunnerName || haikuRoute.ReleaseGate != nil || haikuRoute.ConditionalSol != nil {
		t.Fatalf("quality-sensitive Haiku must be Grok/high then eligible Kimi with no automatic Sol: %+v ok=%v", haikuRoute, haikuOK)
	}
	task := finalOwnerTask("opus", routeClassGeneral, riskClassOrdinary)
	task.ID = "frontend-quality"
	task.SpecializedFrontend = true
	route, ok := resolveOwnerRoute(cfg, task)
	if !ok || route.ReleaseGate == nil || route.ReleaseGate.Model != "gpt-5.6-sol" || route.ReleaseGate.Effort != "xhigh" {
		t.Fatalf("complex frontend must require a fresh Sol/xhigh final review: %+v", route)
	}
	if !pinOwnerPrimaryRoute(task, route) {
		t.Fatal("failed to pin frontend route")
	}
	high := finalOwnerTask("opus", routeClassGeneral, riskClassHigh)
	high.SpecializedFrontend = true
	if highRoute, _ := resolveOwnerRoute(cfg, high); highRoute.ReleaseGate == nil || highRoute.ReleaseGate.Effort != "max" {
		t.Fatalf("high-risk specialized frontend must require Sol/max: %+v", highRoute)
	}
	task.Runner = grokBuildRunnerName
	task.FallbackReason = string(fallbackTransport)
	task.CompletedReviews = []string{reviewStageKimiAdversarial}
	brief := toBrief(cfg, task, time.Now())
	if brief.ActualProvider != grokBuildRunnerName || brief.ActualRunner != grokBuildRunnerName || brief.ActualModel != "grok-4.6" || brief.ActualEffort != "xhigh" ||
		brief.RouteStage != routeStagePrimary || brief.FallbackReason != string(fallbackTransport) || brief.RiskClass != riskClassOrdinary ||
		!reflect.DeepEqual(brief.RequiredReviews, task.RequiredReviews) || !reflect.DeepEqual(brief.CompletedReviews, task.CompletedReviews) {
		t.Fatalf("board readback omitted final route evidence: %+v", brief)
	}
}

func TestFinalOwnerBoardExposesCriticalBudgetBypassReason(t *testing.T) {
	cfg := policyTestConfig()
	task := finalOwnerTask("opus", routeClassBackend, riskClassHigh)
	task.OwnerCriticalBypassReason = "Owner-pinned production authentication repair"
	brief := toBrief(cfg, task, time.Now())
	if brief.OwnerCriticalBypassReason != task.OwnerCriticalBypassReason {
		t.Fatalf("critical automatic-Codex bypass reason is not visible in board readback: %+v", brief)
	}
}

func TestFinalOwnerQueuedBoardReadbackUsesPlanWithoutClaimingExecution(t *testing.T) {
	cfg := policyTestConfig()
	task := finalOwnerTask("opus", routeClassBackend, riskClassHigh)
	task.Status = statusQueued
	brief := toBrief(cfg, task, time.Now())
	if brief.RouteStage != routeStagePrimary || brief.RiskClass != riskClassHigh ||
		!reflect.DeepEqual(brief.RequiredReviews, []string{reviewStageKimiSecondView, reviewStageSolMax}) {
		t.Fatalf("queued board readback omitted the exact deterministic plan: %+v", brief)
	}
	if brief.ActualProvider != "" || brief.ActualRunner != "" || brief.ActualModel != "" || brief.ActualEffort != "" {
		t.Fatalf("an unrun queued card must not be presented as actual execution evidence: %+v", brief)
	}
}

func TestFinalOwnerEmittedCardEntersDefaultRouteWithoutExplicitPin(t *testing.T) {
	root := testRoot(t)
	cfg := policyTestConfig()
	parent := newTask(root, cfg, typeCoordinate, "emit final route", t.TempDir(), []string{"split"}, 5)
	result := `{"tasks":[{"title":"ordinary backend","type":"sequence","model":"opus","route_class":"backend","risk_class":"ordinary","fresh_steps":true,"prompts":["implement"]}]}`
	ids, err := enqueueEmitted(root, cfg, parent, result)
	if err != nil || len(ids) != 1 {
		t.Fatalf("final Owner emit failed: ids=%v err=%v", ids, err)
	}
	child, err := loadTask(root, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if child.PreferRunner != "codex" || child.RunnerExplicit {
		t.Fatalf("default Owner route was accidentally materialized as an explicit pin: %+v", child)
	}
	route, ok := resolveOwnerRoute(cfg, child)
	if !ok || route.Name != "opus_backend_ordinary" {
		t.Fatalf("emitted card did not enter the final deterministic resolver: route=%+v ok=%v", route, ok)
	}
}

func TestFinalOwnerLegacyTaskReadbackDoesNotRewriteBytesOrState(t *testing.T) {
	root := testRoot(t)
	raw := []byte("{\n  \"id\": \"legacy-preserved\",\n  \"title\": \"existing held task\",\n  \"type\": \"sequence\",\n  \"status\": \"held\",\n  \"step\": 0,\n  \"prompts\": [\"existing work\"],\n  \"dir\": \"/tmp/existing\",\n  \"model\": \"sonnet\"\n}\n")
	path := taskPath(root, "legacy-preserved")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	tasks, err := loadBoardTasks(root)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("legacy task readback failed: tasks=%+v err=%v", tasks, err)
	}
	if tasks[0].Status != statusHeld || tasks[0].Step != 0 || tasks[0].RiskClass != "" || tasks[0].OwnerRouteStage != "" {
		t.Fatalf("additive Owner fields changed legacy task semantics: %+v", tasks[0])
	}
	_ = toBrief(policyTestConfig(), tasks[0], time.Now())
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(raw) {
		t.Fatalf("board/resolver readback rewrote existing task bytes:\n%s", after)
	}
}

func TestFinalOwnerFableRuntimeCreatesDirectTerminalMerger(t *testing.T) {
	root := testRoot(t)
	cfg := policyTestConfig()
	task := newTask(root, cfg, typeSequence, "hard decision", t.TempDir(), []string{"decide from evidence"}, 9)
	task.Model = "fable"
	task.PreferRunner = "codex"
	if err := prepareCursorFableFallback(root, cfg, task, "eligible quota", fallbackQuota, verifiedCursorFallbackAuth(t)); err != nil {
		t.Fatal(err)
	}
	handleCrossStage(root, cfg, task, &claudeResult{Result: "Grok proposal"}, nil)
	tasks, err := loadTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	var merger *Task
	for _, candidate := range tasks {
		if candidate.XKey != task.XKey {
			continue
		}
		if candidate.XRole == "B" {
			t.Fatalf("Fable must not create a blind Sol answer B: %+v", candidate)
		}
		if candidate.XRole == "C" {
			merger = candidate
		}
	}
	if merger == nil || !merger.FableReviewerMerger || !merger.AutomaticCodex || merger.PreferRunner != "codex" ||
		merger.XCodexModel != "gpt-5.6-sol" || merger.Effort != "ultra" || merger.AutomaticSolCalls != 1 ||
		merger.ReviewAfter || merger.XEngineC != nil || merger.RouteClass != routeClassGeneral {
		t.Fatalf("direct Fable terminal reviewer-merger drifted: %+v", merger)
	}
	if !strings.Contains(merger.Prompts[0], "decide from evidence") || !strings.Contains(merger.Prompts[0], "Grok proposal") ||
		!strings.Contains(merger.Prompts[0], "Do not write product bytes") {
		t.Fatalf("reviewer-merger did not receive the original problem and Grok answer: %q", merger.Prompts[0])
	}
	merger.Status = statusDone
	merger.Step = len(merger.Prompts)
	postComplete(root, cfg, merger, &claudeResult{Result: "conclusion\n```json\n{\"verdict\":\"pass\",\"confidence\":\"high\",\"p0\":0,\"p1\":0,\"owner_hold\":false,\"uncertainty\":\"none\"}\n```"}, nil)
	after, err := loadBoardTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range after {
		if candidate.ReviewOf == merger.ID || (candidate.XKey == merger.XKey && candidate.XRole == "B") {
			t.Fatalf("terminal Fable merger created a review-of-review or blind answer: %+v", candidate)
		}
	}
}

func TestFinalOwnerSerialBackendReviewPlan(t *testing.T) {
	root := testRoot(t)
	cfg := policyTestConfig()
	impl := finalOwnerTask("opus", routeClassBackend, riskClassHigh)
	impl.ID = "high-risk-lineage"
	impl.Dir = t.TempDir()
	impl.Title = "identity protocol change"
	impl.Status = statusHeld
	impl.Step = len(impl.Prompts)
	route, ok := resolveOwnerRoute(cfg, impl)
	if !ok || !pinOwnerPrimaryRoute(impl, route) {
		t.Fatalf("failed to pin high-risk route: %+v ok=%v", route, ok)
	}
	if err := saveTask(root, impl); err != nil {
		t.Fatal(err)
	}
	kimi, err := ensureReviewAfterTask(root, cfg, impl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if kimi == nil || kimi.ReviewPlanStage != reviewStageKimiSecondView || kimi.PreferRunner != kimiCLIRunnerName ||
		kimi.KimiModel != "kimi-code/k3" || kimi.Effort != "max" || !kimi.AdvisoryReview || kimi.ReviewAfter {
		t.Fatalf("high-risk first gate must be a fresh read-only Kimi second view: %+v", kimi)
	}
	pass := "```json\n{\"verdict\":\"pass\",\"p0\":[],\"p1\":[],\"p2\":[],\"summary\":\"independent view complete\"}\n```"
	advancePlannedReview(root, cfg, kimi, pass, nil)
	rootAfterKimi, err := findTaskAnywhere(root, impl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rootAfterKimi.Status != statusHeld || rootAfterKimi.AutomaticCodex || rootAfterKimi.AutomaticSolCalls != 1 ||
		!reviewCompleted(rootAfterKimi, reviewStageKimiSecondView) || reviewCompleted(rootAfterKimi, reviewStageSolMax) {
		t.Fatalf("Kimi must serially precede the mandatory Sol gate: %+v", rootAfterKimi)
	}
	sol, err := existingPlannedReviewChild(root, impl.ID, reviewStageSolMax)
	if err != nil {
		t.Fatal(err)
	}
	if sol == nil || sol.PreferRunner != "codex" || sol.CodexModel != "gpt-5.6-sol" || sol.Effort != "max" ||
		!sol.AutomaticCodex || sol.AutomaticSolCalls != 1 || sol.ReviewAfter || sol.ReviewOf != impl.ID || sol.ReviewPlanRoot != impl.ID {
		t.Fatalf("mandatory Sol/max release gate drifted or became review-of-review: %+v", sol)
	}
	sol.AutomaticSolInvocations = 1
	advancePlannedReview(root, cfg, sol, pass, nil)
	terminal, err := findTaskAnywhere(root, impl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Status != statusDone || terminal.OwnerRouteStage != routeStageTerminal ||
		terminal.AutomaticSolCalls != 1 || terminal.AutomaticSolInvocations != 1 ||
		!reflect.DeepEqual(terminal.CompletedReviews, []string{reviewStageKimiSecondView, reviewStageSolMax}) {
		t.Fatalf("high-risk lineage did not close after both serial gates: %+v", terminal)
	}
}

func TestFinalOwnerOrdinaryBackendKimiAdversarialReviewCreatesSeparatelyRoutedRepair(t *testing.T) {
	root := testRoot(t)
	cfg := policyTestConfig()
	impl := newTask(root, cfg, typeSequence, "ordinary bounded backend", t.TempDir(), []string{"implement bounded backend change"}, 5)
	impl.Model = "opus"
	impl.RouteClass = routeClassBackend
	impl.RiskClass = riskClassOrdinary
	impl.ID = "ordinary-review-repair"
	for deterministicSolSample(impl.ID, 20) {
		impl.ID += "x"
	}
	route, ok := resolveOwnerRoute(cfg, impl)
	if !ok || route.Name != "opus_backend_ordinary" || !pinOwnerPrimaryRoute(impl, route) {
		t.Fatalf("ordinary backend route was not frozen: route=%+v ok=%v task=%+v", route, ok, impl)
	}
	impl.Status = statusHeld
	impl.Step = len(impl.Prompts)
	if err := saveTask(root, impl); err != nil {
		t.Fatal(err)
	}
	review, err := ensureReviewAfterTask(root, cfg, impl, nil)
	if err != nil || review == nil {
		t.Fatalf("Kimi adversarial review was not created: review=%+v err=%v", review, err)
	}
	if review.ReviewPlanStage != reviewStageKimiAdversarial || review.PreferRunner != kimiCLIRunnerName ||
		review.KimiModel != "kimi-code/k3" || review.Effort != "max" || review.AdvisoryReview || review.ReviewAfter {
		t.Fatalf("ordinary backend review identity drifted: %+v", review)
	}
	result := "```json\n{\"verdict\":\"concerns\",\"p0\":[],\"p1\":[\"acceptance evidence missing\"],\"p2\":[],\"summary\":\"repair required\"}\n```"
	review.Status = statusDone
	postComplete(root, cfg, review, &claudeResult{Result: result}, nil)
	rootAfter, err := findTaskAnywhere(root, impl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rootAfter.Status != statusHeld || rootAfter.SolEscalationReason != solEscalationAcceptanceFailed ||
		!reviewCompleted(rootAfter, reviewStageKimiAdversarial) || pendingRequiredReviewStage(rootAfter) != reviewStageSolXHigh {
		t.Fatalf("failed Kimi acceptance did not hold and require the conditional Sol gate: %+v", rootAfter)
	}
	all, err := loadTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	var repair *Task
	for _, candidate := range all {
		if strings.HasPrefix(candidate.Title, "修复R1: ordinary bounded backend") {
			repair = candidate
			break
		}
	}
	if repair == nil || repair.Type != typeSequence || repair.RouteClass != routeClassBackend ||
		repair.RiskClass != riskClassOrdinary || repair.SolEscalationReason != solEscalationAcceptanceFailed ||
		repair.PreferRunner != "codex" || repair.RunnerExplicit {
		t.Fatalf("Kimi concerns did not create a separately classified deterministic repair card: %+v", repair)
	}
	repairRoute, routeOK := resolveOwnerRoute(cfg, repair)
	if !routeOK || repairRoute.Name != "opus_backend_ordinary" || repairRoute.ReleaseGate == nil ||
		repairRoute.ReleaseGate.Effort != "xhigh" {
		t.Fatalf("repair card did not independently resolve the failed-acceptance Sol gate: route=%+v ok=%v", repairRoute, routeOK)
	}
}
