package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	routeStagePrimary               = "primary"
	routeStageFallbackReview        = "fallback_or_review"
	routeStageFableAnswer           = "fable_grok_answer"
	routeStageFableMerge            = "fable_sol_ultra_adversarial_merge"
	routeStageAdversarialReview     = "kimi_adversarial_review"
	routeStageSecondView            = "kimi_read_only_second_view"
	routeStageConditionalEscalation = "conditional_sol_escalation"
	routeStageConditionalRelease    = "conditional_sol_release_gate"
	routeStageReleaseGate           = "mandatory_sol_release_gate"
	routeStageStandaloneReview      = "standalone_review"
	routeStageTerminal              = "terminal"

	reviewStageKimiAdversarial = "kimi-k3-max-adversarial"
	reviewStageKimiSecondView  = "kimi-k3-max-second-view"
	reviewStageSolXHigh        = "sol-xhigh-final"
	reviewStageSolMax          = "sol-max-release"
	reviewStageFableSolUltra   = "sol-ultra-adversarial-merge"

	solEscalationSample           = "deterministic-20-percent-sample"
	solEscalationDisagreement     = "grok-kimi-disagreement"
	solEscalationAcceptanceFailed = "acceptance-failed"
	solEscalationExplicitHighRisk = "explicit-high-risk"
	solEscalationFrontend         = "specialized-frontend"

	fableMergeTerminal  = "terminal"
	fableMergeHoldOwner = "hold-owner"
)

var closedReviewStages = map[string]bool{
	reviewStageKimiAdversarial: true,
	reviewStageKimiSecondView:  true,
	reviewStageSolXHigh:        true,
	reviewStageSolMax:          true,
	reviewStageFableSolUltra:   true,
}

var closedOwnerRouteStages = map[string]bool{
	routeStagePrimary: true, routeStageFallbackReview: true,
	routeStageFableAnswer: true, routeStageFableMerge: true,
	routeStageAdversarialReview: true, routeStageSecondView: true,
	routeStageConditionalEscalation: true, routeStageConditionalRelease: true,
	routeStageReleaseGate: true, routeStageStandaloneReview: true, routeStageTerminal: true,
}

var closedSolEscalationReasons = map[string]bool{
	solEscalationSample: true, solEscalationDisagreement: true,
	solEscalationAcceptanceFailed: true, solEscalationExplicitHighRisk: true,
	solEscalationFrontend: true,
}

func closedOwnerTaskStateError(t *Task) error {
	if t == nil {
		return nil
	}
	if t.OwnerRouteStage != "" && !closedOwnerRouteStages[t.OwnerRouteStage] {
		return fmt.Errorf("unknown owner_route_stage %q", t.OwnerRouteStage)
	}
	if t.FallbackReason != "" {
		switch fallbackFailureKind(t.FallbackReason) {
		case fallbackQuota, fallbackTransport, fallbackStreamIncomplete, fallbackSemanticStall,
			fallbackInvalidTerminal, fallbackExecutionEnv:
		default:
			return fmt.Errorf("unknown fallback_reason %q", t.FallbackReason)
		}
	}
	if t.RiskClass != "" {
		switch t.RiskClass {
		case riskClassOrdinary, riskClassHigh, riskClassCritical, riskClassProduction:
		default:
			return fmt.Errorf("unknown risk_class %q", t.RiskClass)
		}
	}
	seenRequired := map[string]bool{}
	for _, stage := range t.RequiredReviews {
		if !closedReviewStages[stage] || seenRequired[stage] {
			return fmt.Errorf("invalid or duplicate required review stage %q", stage)
		}
		seenRequired[stage] = true
	}
	seenCompleted := map[string]bool{}
	for i, stage := range t.CompletedReviews {
		if !closedReviewStages[stage] || seenCompleted[stage] || !seenRequired[stage] ||
			i >= len(t.RequiredReviews) || t.RequiredReviews[i] != stage {
			return fmt.Errorf("invalid, duplicate, or unrequired completed review stage %q", stage)
		}
		seenCompleted[stage] = true
	}
	if t.ReviewPlanStage != "" && !closedReviewStages[t.ReviewPlanStage] {
		return fmt.Errorf("unknown review_plan_stage %q", t.ReviewPlanStage)
	}
	if (t.ReviewPlanStage == "") != (t.ReviewPlanRoot == "") {
		return fmt.Errorf("review_plan_stage and review_plan_root must be present together")
	}
	if t.SolEscalationReason != "" && !closedSolEscalationReasons[t.SolEscalationReason] {
		return fmt.Errorf("unknown sol_escalation_reason %q", t.SolEscalationReason)
	}
	if t.AutomaticSolCalls < 0 || t.AutomaticSolCalls > 1 ||
		t.AutomaticSolInvocations < 0 || t.AutomaticSolInvocations > 1 ||
		t.AutomaticSolInvocations > t.AutomaticSolCalls {
		return fmt.Errorf("automatic Sol counters invalid: calls=%d invocations=%d",
			t.AutomaticSolCalls, t.AutomaticSolInvocations)
	}
	if t.AutomaticCodex && t.AutomaticSolCalls != 1 {
		return fmt.Errorf("automatic_codex requires exactly one reserved Sol call")
	}
	if t.AutomaticCodex {
		if t.PreferRunner != "codex" || t.ReviewAfter {
			return fmt.Errorf("automatic_codex requires a direct Codex identity with review_after=false")
		}
		codexModel, crossModel := strings.TrimSpace(t.CodexModel), strings.TrimSpace(t.XCodexModel)
		if (codexModel == "") == (crossModel == "") {
			return fmt.Errorf("automatic_codex requires exactly one frozen Codex model field")
		}
		model := codexModel
		if model == "" {
			model = crossModel
		}
		if model != "gpt-5.6-sol" {
			return fmt.Errorf("automatic_codex model must be gpt-5.6-sol, got %q", model)
		}
		effort := strings.ToLower(strings.TrimSpace(t.Effort))
		switch t.OwnerRouteStage {
		case routeStageFableMerge:
			if effort != "ultra" {
				return fmt.Errorf("Fable automatic reviewer-merger must use Sol/ultra")
			}
		case routeStageConditionalEscalation:
			if effort != "xhigh" {
				return fmt.Errorf("conditional automatic Sol escalation must use xhigh")
			}
		case routeStageConditionalRelease:
			if effort != "xhigh" && effort != "max" {
				return fmt.Errorf("conditional automatic Sol release must use xhigh or max")
			}
		case routeStageReleaseGate, routeStageStandaloneReview:
			if effort != "max" {
				return fmt.Errorf("mandatory or standalone automatic Sol gate must use max")
			}
		case routeStageTerminal:
			if !t.FableReviewerMerger || effort != "ultra" || t.AutomaticSolInvocations != 1 {
				return fmt.Errorf("only a completed Fable Sol/ultra merger may retain terminal automatic_codex state")
			}
		default:
			return fmt.Errorf("automatic_codex has no closed route gate at stage %q", t.OwnerRouteStage)
		}
	}
	if t.FableReviewerMerger {
		if !t.AutomaticCodex || t.OwnerRouteName != "fable_explicit" ||
			t.RouteClass != routeClassGeneral || effectiveOwnerRiskClass(t) != riskClassOrdinary {
			return fmt.Errorf("Fable reviewer-merger must retain the automatic read-only general Fable identity")
		}
	}
	if strings.TrimSpace(t.OwnerCriticalBypassReason) != "" && !ownerCriticalBudgetBypass(t) {
		return fmt.Errorf("owner critical budget bypass reason requires high-risk, critical, or production classification")
	}
	return nil
}

func effectiveOwnerRiskClass(t *Task) string {
	if t == nil {
		return riskClassHigh
	}
	raw := strings.ToLower(strings.TrimSpace(t.RiskClass))
	if modelTierKeyword(nil, t.Model) == "fable" {
		return riskClassOrdinary
	}
	if t.Type == typeReview {
		switch raw {
		case riskClassOrdinary:
			return riskClassOrdinary
		case riskClassProduction:
			return riskClassProduction
		case riskClassCritical, riskClassHigh:
			return riskClassCritical
		default:
			return riskClassCritical
		}
	}
	if backendDevelopmentTask(t) {
		if raw == riskClassOrdinary {
			return riskClassOrdinary
		}
		return riskClassHigh
	}
	switch raw {
	case riskClassHigh, riskClassCritical, riskClassProduction:
		return riskClassHigh
	default:
		return riskClassOrdinary
	}
}

func deterministicSolSample(lineageID string, percent int) bool {
	if percent <= 0 || percent > 100 || strings.TrimSpace(lineageID) == "" {
		return percent == 100
	}
	digest := sha256.Sum256([]byte("cardex-owner-sol-sample-v1\x00" + lineageID))
	return int(binary.BigEndian.Uint64(digest[:8])%100) < percent
}

func fableFallbackKindEligible(kind fallbackFailureKind) bool {
	switch kind {
	case fallbackQuota, fallbackTransport, fallbackStreamIncomplete, fallbackExecutionEnv:
		return true
	default:
		return false
	}
}

func canonicalOpinionModel(leg policyLeg) string {
	provider := strings.ToLower(strings.TrimSpace(leg.Runner))
	model := strings.ToLower(strings.TrimSpace(leg.Model))
	joined := provider + "/" + model
	if strings.Contains(joined, "kimi") && strings.Contains(joined, "k3") {
		return "kimi-k3"
	}
	return joined
}

func independentModelOpinion(a, b policyLeg) bool {
	left, right := canonicalOpinionModel(a), canonicalOpinionModel(b)
	return left != "" && right != "" && left != right
}

type automaticCodexBudgetEvidence struct {
	Available   bool
	UsedPercent int
	Source      string
	Reason      string
}

func currentAutomaticCodexBudgetEvidence(cfg *Config, now time.Time) automaticCodexBudgetEvidence {
	if cfg == nil {
		return automaticCodexBudgetEvidence{Reason: "provider-specific usage configuration unavailable"}
	}
	// Automatic Codex gates must consume only the configured CodexBar/usage-feed evidence.
	// The legacy OAuth source measures a different provider and therefore can never authorize
	// an automatic Codex call, even when it happens to report a lower percentage.
	read := readUsageFeedProviderPercent(cfg, now, "codex")
	if read.Available {
		return automaticCodexBudgetEvidence{
			Available: true, UsedPercent: read.Percent, Source: read.Source,
		}
	}
	reason := strings.TrimSpace(read.Reason)
	if reason == "" {
		reason = "unavailable"
	}
	return automaticCodexBudgetEvidence{Reason: read.Source + ": " + reason}
}

func ownerCriticalBudgetBypass(t *Task) bool {
	if t == nil || strings.TrimSpace(t.OwnerCriticalBypassReason) == "" {
		return false
	}
	switch effectiveOwnerRiskClass(t) {
	case riskClassHigh, riskClassCritical, riskClassProduction:
		return true
	default:
		return false
	}
}

func automaticCodexBudgetAllowed(t *Task, evidence automaticCodexBudgetEvidence, stopPercent int) (bool, string) {
	if t == nil || !t.AutomaticCodex {
		return true, "explicit or non-automatic Codex invocation"
	}
	if ownerCriticalBudgetBypass(t) {
		return true, "Owner-pinned critical bypass: " + strings.TrimSpace(t.OwnerCriticalBypassReason)
	}
	if stopPercent != 65 {
		return false, fmt.Sprintf("automatic Codex budget policy unavailable: stop=%d, required=65", stopPercent)
	}
	if !evidence.Available || evidence.UsedPercent < 0 || evidence.UsedPercent > 100 {
		reason := strings.TrimSpace(evidence.Reason)
		if reason == "" {
			reason = "provider-specific usage evidence unavailable"
		}
		return false, "automatic Codex held: " + reason
	}
	if evidence.UsedPercent >= stopPercent {
		return false, fmt.Sprintf("automatic Codex held at %d%% used (stop=%d%%, reserve=%d%%)", evidence.UsedPercent, stopPercent, 100-stopPercent)
	}
	return true, fmt.Sprintf("automatic Codex budget %d%% used below %d%% stop", evidence.UsedPercent, stopPercent)
}

func reserveAutomaticSolCall(t *Task, stage string) error {
	if t == nil {
		return fmt.Errorf("automatic Sol reservation requires a task")
	}
	if t.AutomaticSolCalls != 0 {
		return fmt.Errorf("automatic Sol call limit reached for lineage: %d/1", t.AutomaticSolCalls)
	}
	t.AutomaticSolCalls = 1
	t.OwnerRouteStage = stage
	return nil
}

func beginAutomaticSolInvocation(t *Task) error {
	if t == nil || !t.AutomaticCodex {
		return nil
	}
	if t.AutomaticSolCalls != 1 {
		return fmt.Errorf("automatic Sol route reservation invalid: %d/1", t.AutomaticSolCalls)
	}
	if t.AutomaticSolInvocations != 0 {
		return fmt.Errorf("automatic Sol invocation limit reached: %d/1", t.AutomaticSolInvocations)
	}
	t.AutomaticSolInvocations = 1
	return nil
}

func appendClosedReview(list []string, stage string) ([]string, error) {
	if !closedReviewStages[stage] {
		return list, fmt.Errorf("unknown review stage %q", stage)
	}
	for _, existing := range list {
		if existing == stage {
			return list, nil
		}
	}
	return append(list, stage), nil
}

func reviewCompleted(t *Task, stage string) bool {
	if t == nil || !closedReviewStages[stage] {
		return false
	}
	for _, completed := range t.CompletedReviews {
		if completed == stage {
			return true
		}
	}
	return false
}

func fableMergeDisposition(result string) string {
	raw := lastFencedJSON(result)
	if raw == "" {
		return fableMergeHoldOwner
	}
	var payload map[string]any
	if json.Unmarshal([]byte(raw), &payload) != nil {
		return fableMergeHoldOwner
	}
	readCount := func(key string) (int, bool) {
		v, ok := payload[key]
		if !ok {
			return 0, false
		}
		n, ok := v.(float64)
		if !ok || n < 0 || n != float64(int(n)) {
			return 0, false
		}
		return int(n), true
	}
	p0, p0OK := readCount("p0")
	p1, p1OK := readCount("p1")
	if !p0OK || !p1OK || p0 != 0 || p1 != 0 {
		return fableMergeHoldOwner
	}
	hold, holdOK := payload["owner_hold"].(bool)
	if !holdOK || hold {
		return fableMergeHoldOwner
	}
	uncertainty, uncertaintyOK := payload["uncertainty"].(string)
	if !uncertaintyOK || strings.ToLower(strings.TrimSpace(uncertainty)) != "none" {
		return fableMergeHoldOwner
	}
	if strings.TrimSpace(rptStr(payload["verdict"])) == "" {
		return fableMergeHoldOwner
	}
	return fableMergeTerminal
}
