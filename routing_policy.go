package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// policyLeg is one frozen execution identity in the owner route table. It is deliberately small:
// route resolution is a dry-run/readback operation and does not mutate tasks or inspect cooldowns.
type policyLeg struct {
	Runner string
	Model  string
	Effort string
}

type ownerRoute struct {
	Name               string
	Legs               []policyLeg
	Review             *policyLeg
	Merge              *policyLeg
	IndependentAnswers bool
}

const routeReasonOwnerReviewSol = "owner_review_sol_max"

func ptrLeg(v policyLeg) *policyLeg { return &v }

func crossPolicyLeg(cfg *Config, engine CrossEngine) policyLeg {
	leg := policyLeg{Runner: engine.Kind, Model: strings.TrimSpace(engine.Model), Effort: strings.ToLower(strings.TrimSpace(engine.Effort))}
	switch engine.Kind {
	case "codex":
		if leg.Model == "" && cfg != nil {
			leg.Model = strings.TrimSpace(cfg.CodexModel)
			if leg.Model == "" {
				leg.Model = strings.TrimSpace(cfg.CodexTierModels["fable"])
			}
		}
	case grokBuildRunnerName:
		if leg.Model == "" && cfg != nil && cfg.GrokBuild != nil {
			leg.Model = strings.TrimSpace(cfg.GrokBuild.Model)
		}
	case cursorRunnerName:
		if leg.Model == "" && cfg != nil {
			leg.Model = strings.TrimSpace(cfg.CursorModel)
		}
		if leg.Effort == "" {
			leg.Effort = cursorEffortFromModel(leg.Model)
		}
	}
	return leg
}

// ownerAutoRouteEligible protects all identities that the owner kept outside automatic routing.
// In particular, a configured provider is not evidence that an explicit pin/session/remote card may
// be rewritten. Availability/cooldown is intentionally absent: resolution describes policy, not a
// guess about whether a provider will answer this invocation.
func ownerAutoRouteEligible(t *Task) bool {
	return t != nil && t.PreferRunner == "codex" && !t.RunnerExplicit && t.RemoteHost == "" && t.XRole == "" &&
		t.CodexModel == "" && t.XCodexModel == "" && t.GeminiModel == "" && t.OpenCodeModel == "" &&
		t.KimiModel == "" && t.GrokModel == "" && t.GrokEffort == "" && t.CursorModel == "" &&
		t.SessionID == "" && !t.MidStep && codexEligible(t)
}

// resolveOwnerRoute returns the exact six-row owner table without changing task state. The result is
// also the board/manual-dispatch authority: execution helpers consume the same configured identities.
func resolveOwnerRoute(cfg *Config, t *Task) (ownerRoute, bool) {
	if cfg == nil || invalidExplicitRouteClass(t) || !ownerAutoRouteEligible(t) {
		return ownerRoute{}, false
	}
	// A standalone review is an explicit review lane, not an Opus implementation. Keep it out of the
	// Kimi/Grok writer chain and pin one clean Sol/max session; typeReview is already non-recursive.
	if t.Type == typeReview {
		if cfg.GrokBuild == nil || strings.TrimSpace(cfg.GrokBuild.ReviewCodexModel) == "" ||
			strings.TrimSpace(cfg.GrokBuild.ReviewCodexEffort) == "" {
			return ownerRoute{}, false
		}
		return ownerRoute{
			Name: "review_standalone",
			Legs: []policyLeg{{Runner: "codex", Model: strings.TrimSpace(cfg.GrokBuild.ReviewCodexModel),
				Effort: strings.ToLower(strings.TrimSpace(cfg.GrokBuild.ReviewCodexEffort))}},
		}, true
	}
	tier := modelTierKeyword(cfg, t.Model)
	switch tier {
	case "fable":
		// Fable is opt-in only: an empty source model can never reach this row through a default.
		if strings.TrimSpace(t.Model) == "" || !cursorFablePolicyApplies(cfg, t) {
			return ownerRoute{}, false
		}
		prof, ok := cfg.CrossProfiles[strings.TrimSpace(cfg.CursorFable.FallbackProfile)]
		if !ok || prof.Merge == nil {
			return ownerRoute{}, false
		}
		primary := policyLeg{Runner: cursorRunnerName, Model: strings.TrimSpace(cfg.CursorFable.Model)}
		primary.Effort = cursorEffortFromModel(primary.Model)
		return ownerRoute{
			Name:               "fable_explicit",
			Legs:               []policyLeg{primary, crossPolicyLeg(cfg, prof.A), crossPolicyLeg(cfg, prof.B)},
			Merge:              ptrLeg(crossPolicyLeg(cfg, *prof.Merge)),
			IndependentAnswers: true,
		}, true
	case "opus":
		if backendDevelopmentTask(t) {
			_, route, ok := grokBuildTierRoute(cfg, t)
			if !ok {
				return ownerRoute{}, false
			}
			resolved := ownerRoute{
				Name: "opus_backend",
				Legs: []policyLeg{
					{Runner: grokBuildRunnerName, Model: strings.TrimSpace(cfg.GrokBuild.Model), Effort: route.Effort},
					{Runner: "codex", Model: route.CodexFallbackModel, Effort: route.CodexFallbackEffort},
				},
			}
			if cfg.GrokBuild.OpusAdversarialReview {
				resolved.Review = ptrLeg(policyLeg{Runner: "codex", Model: cfg.GrokBuild.ReviewCodexModel, Effort: cfg.GrokBuild.ReviewCodexEffort})
			}
			return resolved, true
		}
		if cfg.KimiCLIOpus == nil || !cfg.KimiCLIOpus.Enabled || !grokBuildKimiFallbackEnabled(cfg) {
			return ownerRoute{}, false
		}
		resolved := ownerRoute{
			Name: "opus_general",
			Legs: []policyLeg{
				{Runner: grokBuildRunnerName, Model: cfg.GrokBuild.Model, Effort: cfg.GrokBuild.Effort},
				{Runner: kimiCLIRunnerName, Model: cfg.KimiCLIOpus.Model, Effort: cfg.KimiCLIOpus.Effort},
				{Runner: "codex", Model: cfg.GrokBuild.CodexFallbackModel, Effort: cfg.GrokBuild.CodexFallbackEffort},
			},
		}
		if cfg.GrokBuild.OpusAdversarialReview {
			resolved.Review = ptrLeg(policyLeg{Runner: "codex", Model: cfg.GrokBuild.ReviewCodexModel, Effort: cfg.GrokBuild.ReviewCodexEffort})
		}
		return resolved, true
	case "sonnet", "haiku":
		key, route, ok := grokBuildTierRoute(cfg, t)
		if !ok || key != tier {
			return ownerRoute{}, false
		}
		return ownerRoute{
			Name: tier,
			Legs: []policyLeg{
				{Runner: grokBuildRunnerName, Model: cfg.GrokBuild.Model, Effort: route.Effort},
				{Runner: "codex", Model: route.CodexFallbackModel, Effort: route.CodexFallbackEffort},
			},
		}, true
	default:
		return ownerRoute{}, false
	}
}

// validateOwnerRoutingPolicy is the production configuration lock for the exact six-row table.
// The generic Cardex defaults leave it disabled; this host enables it explicitly. Validation asks the
// same resolver used by tick, board, and manual takeover to resolve all six rows, so a syntactically
// valid provider config cannot silently remove a row or weaken a model/review identity.
func validateOwnerRoutingPolicy(cfg *Config) error {
	if cfg == nil || !cfg.OwnerRoutingEnforced {
		return nil
	}
	if cfg.DefaultRunner != "codex" {
		return fmt.Errorf("owner_routing_enforced=true 需要 default_runner=codex")
	}
	if cfg.KimiCLIOpus == nil || !cfg.KimiCLIOpus.Enabled || !cfg.KimiCLIOpus.ExcludeBackend ||
		strings.TrimSpace(cfg.KimiCLIOpus.Model) != "kimi-code/k3" ||
		strings.ToLower(strings.TrimSpace(cfg.KimiCLIOpus.Effort)) != "max" {
		return fmt.Errorf("Owner 非后端 Opus 第二腿必须严格启用 kimi-code/k3/max 且 exclude_backend=true")
	}
	if cfg.GrokBuild == nil || !cfg.GrokBuild.Enabled || !cfg.GrokBuild.KimiOpusFallback {
		return fmt.Errorf("Owner 六行路由需要完整启用 Grok 接力")
	}
	if cfg.GrokBuild.OpusAdversarialReview {
		return fmt.Errorf("Owner 当前路由不自动追加 Opus 对抗复审；请使用独立 Sol/max 审核卡")
	}
	if strings.TrimSpace(cfg.GrokBuild.ReviewCodexModel) != "gpt-5.6-sol" ||
		strings.ToLower(strings.TrimSpace(cfg.GrokBuild.ReviewCodexEffort)) != "max" {
		return fmt.Errorf("Owner 独立审核卡必须严格使用 gpt-5.6-sol/max")
	}
	if cfg.CursorFable == nil || !cfg.CursorFable.Enabled {
		return fmt.Errorf("Owner 显式 Fable 行需要启用 Cursor Fable 5 主腿")
	}

	type expectedRoute struct {
		name        string
		model       string
		routeClass  string
		legs        []policyLeg
		review      *policyLeg
		merge       *policyLeg
		independent bool
	}
	solMax := policyLeg{Runner: "codex", Model: "gpt-5.6-sol", Effort: "max"}
	solXHigh := policyLeg{Runner: "codex", Model: "gpt-5.6-sol", Effort: "xhigh"}
	expected := []expectedRoute{
		{name: "fable_explicit", model: "fable", routeClass: routeClassGeneral,
			legs: []policyLeg{{Runner: cursorRunnerName, Model: "claude-fable-5-thinking-max", Effort: "max"},
				{Runner: grokBuildRunnerName, Model: "grok-4.6", Effort: "xhigh"},
				{Runner: "codex", Model: "gpt-5.6-sol", Effort: "ultra"}},
			merge: ptrLeg(solMax), independent: true},
		{name: "opus_general", model: "opus", routeClass: routeClassGeneral,
			legs: []policyLeg{{Runner: grokBuildRunnerName, Model: "grok-4.6", Effort: "xhigh"},
				{Runner: kimiCLIRunnerName, Model: "kimi-code/k3", Effort: "max"}, solXHigh}},
		{name: "opus_backend", model: "opus", routeClass: routeClassBackend,
			legs: []policyLeg{{Runner: grokBuildRunnerName, Model: "grok-4.6", Effort: "xhigh"}, solMax}},
		{name: "sonnet", model: "sonnet", routeClass: routeClassGeneral,
			legs: []policyLeg{{Runner: grokBuildRunnerName, Model: "grok-4.6", Effort: "high"},
				{Runner: "codex", Model: "gpt-5.6-luna", Effort: "max"}}},
		{name: "haiku", model: "haiku", routeClass: routeClassGeneral,
			legs: []policyLeg{{Runner: grokBuildRunnerName, Model: "grok-4.6", Effort: "high"},
				{Runner: "codex", Model: "gpt-5.6-luna", Effort: "xhigh"}}},
		{name: "review_standalone", model: "opus", routeClass: routeClassGeneral,
			legs: []policyLeg{solMax}},
	}
	for _, want := range expected {
		typ := typeSequence
		if want.name == "review_standalone" {
			typ = typeReview
		}
		t := &Task{Type: typ, Model: want.model, RouteClass: want.routeClass,
			PreferRunner: "codex", FreshSteps: true, Prompts: []string{"owner route validation"}}
		got, ok := resolveOwnerRoute(cfg, t)
		if !ok || got.Name != want.name || len(got.Legs) != len(want.legs) || got.IndependentAnswers != want.independent {
			return fmt.Errorf("Owner 路由 %s 无法由生产 resolver 完整解析", want.name)
		}
		for i := range want.legs {
			if got.Legs[i] != want.legs[i] {
				return fmt.Errorf("Owner 路由 %s 第 %d 腿漂移: got=%+v want=%+v", want.name, i+1, got.Legs[i], want.legs[i])
			}
		}
		if (got.Review == nil) != (want.review == nil) || (got.Review != nil && *got.Review != *want.review) {
			return fmt.Errorf("Owner 路由 %s 复审身份漂移", want.name)
		}
		if (got.Merge == nil) != (want.merge == nil) || (got.Merge != nil && *got.Merge != *want.merge) {
			return fmt.Errorf("Owner 路由 %s 合并身份漂移", want.name)
		}
	}
	return nil
}

func ownerPolicyRouteReason(reason string) bool {
	switch reason {
	case routeReasonCursorFable, routeReasonCursorFableFallbackPending, routeReasonCursorFableFallback,
		routeReasonKimiCLIOpus, routeReasonKimiToGrokPending, routeReasonKimiToGrok,
		routeReasonGrokToKimiPending, routeReasonGrokToKimi,
		routeReasonKimiToSolPending, routeReasonKimiToSol,
		routeReasonGrokOpusGeneral, routeReasonGrokOpusBackend, routeReasonGrokSonnet, routeReasonGrokHaiku,
		routeReasonGrokToSolPending, routeReasonGrokToSol,
		routeReasonGrokSonnetToLunaPending, routeReasonGrokSonnetToLuna,
		routeReasonGrokHaikuToLunaPending, routeReasonGrokHaikuToLuna,
		routeReasonOwnerReviewSol:
		return true
	default:
		return false
	}
}

// resolveOwnerRouteReadback resolves both a fresh primary and an already-entered policy leg. In-flight
// cards carry frozen provider fields that intentionally make them ineligible for a new automatic route;
// a copy is normalized only for readback/next-leg lookup after a Cardex-owned route_reason proves origin.
func resolveOwnerRouteReadback(cfg *Config, t *Task) (ownerRoute, bool) {
	if route, ok := resolveOwnerRoute(cfg, t); ok {
		return route, true
	}
	if cfg == nil || t == nil || !ownerPolicyRouteReason(t.RouteReason) || t.XRole != "" ||
		t.RunnerExplicit || t.RemoteHost != "" || t.SessionID != "" || t.MidStep ||
		t.OwnerRouteName == "" || t.OwnerRouteLeg < 1 {
		return ownerRoute{}, false
	}
	probe := *t
	probe.PreferRunner = "codex"
	probe.CodexModel = ""
	probe.XCodexModel = ""
	probe.GeminiModel = ""
	probe.OpenCodeModel = ""
	probe.KimiModel = ""
	probe.GrokModel = ""
	probe.GrokEffort = ""
	probe.CursorModel = ""
	route, ok := resolveOwnerRoute(cfg, &probe)
	if !ok || route.Name != t.OwnerRouteName || t.OwnerRouteLeg > len(route.Legs) ||
		!ownerRouteSnapshotLegMatches(t, route.Legs[t.OwnerRouteLeg-1]) {
		return ownerRoute{}, false
	}
	return route, true
}

func ownerRouteSnapshotLegMatches(t *Task, leg policyLeg) bool {
	if t == nil {
		return false
	}
	// Cross/remote/session identities were rejected by the caller. Every remaining provider pin must
	// either be exactly the frozen current leg or empty; an unrelated later pin invalidates readback.
	switch leg.Runner {
	case kimiCLIRunnerName:
		return t.PreferRunner == kimiCLIRunnerName && t.CodexModel == "" && t.XCodexModel == "" &&
			t.GeminiModel == "" && t.OpenCodeModel == "" && (t.KimiModel == "" || t.KimiModel == leg.Model) &&
			t.GrokModel == "" && t.GrokEffort == "" && t.CursorModel == "" &&
			(t.Effort == "" || (t.Effort == leg.Effort && t.EffortExplicit))
	case cursorRunnerName:
		return t.PreferRunner == "codex" && t.CodexModel == "" && t.XCodexModel == "" &&
			t.GeminiModel == "" && t.OpenCodeModel == "" && t.KimiModel == "" &&
			t.GrokModel == "" && t.GrokEffort == "" && (t.CursorModel == "" || t.CursorModel == leg.Model)
	case grokBuildRunnerName:
		return t.PreferRunner == grokBuildRunnerName && t.GrokModel == leg.Model && t.GrokEffort == leg.Effort &&
			t.CodexModel == "" && t.XCodexModel == "" && t.GeminiModel == "" &&
			t.OpenCodeModel == "" && t.KimiModel == "" && t.CursorModel == ""
	case "codex":
		return t.PreferRunner == "codex" && t.CodexModel == leg.Model && t.Effort == leg.Effort &&
			t.EffortExplicit && t.XCodexModel == "" && t.GeminiModel == "" &&
			t.OpenCodeModel == "" && t.KimiModel == "" && t.GrokModel == "" &&
			t.GrokEffort == "" && t.CursorModel == ""
	default:
		return false
	}
}

// resolvePinnedTaskLeg is the single read-only identity resolver for cards intentionally outside the
// six-row automatic table. Board and `cardex cmd` consume it so an explicit pin is never re-inferred
// as Cursor/Kimi/Grok by legacy display helpers.
func resolvePinnedTaskLeg(cfg *Config, t *Task) (policyLeg, bool) {
	if cfg == nil || t == nil || t.RemoteHost != "" || ownerRoutingPolicyWaitReason(cfg, t) != "" {
		return policyLeg{}, false
	}
	switch t.PreferRunner {
	case "codex":
		return policyLeg{Runner: "codex", Model: resolveCodexModel(cfg, t), Effort: resolveCodexReasoning(cfg, t)}, true
	case kimiCLIRunnerName:
		return policyLeg{Runner: kimiCLIRunnerName, Model: resolveKimiCLIModel(cfg, t), Effort: resolveKimiCLIEffort(cfg, t)}, true
	case grokBuildRunnerName:
		return policyLeg{Runner: grokBuildRunnerName, Model: resolveGrokBuildModel(cfg, t), Effort: resolveGrokBuildEffort(cfg, t)}, true
	case cursorRunnerName:
		model := resolveCursorModel(cfg, t)
		return policyLeg{Runner: cursorRunnerName, Model: model, Effort: cursorEffortFromModel(model)}, true
	case "opencode":
		return policyLeg{Runner: "opencode", Model: resolveOpenCodeRunModel(cfg, t), Effort: resolveOpenCodeRunVariant(cfg, t)}, true
	case "gemini":
		model, _ := resolveGeminiModel(cfg, t)
		return policyLeg{Runner: "gemini", Model: model}, true
	default:
		return policyLeg{}, false
	}
}

func freezeOwnerSolMaxReview(t *Task, route ownerRoute) {
	if t == nil || route.Review == nil || route.Review.Runner != "codex" ||
		route.Review.Model != "gpt-5.6-sol" || route.Review.Effort != "max" {
		return
	}
	t.ReviewAfter = true
	t.SolMaxAdversarialReview = true
	enforceReviewAfterEligibility(t)
}

// pinOwnerPrimaryRoute consumes the first leg returned by resolveOwnerRoute. Cursor is selected through
// the per-dispatch value; all other providers are frozen on the card before dispatch so config hot
// reload cannot change a queued leg's model or effort.
func pinOwnerPrimaryRoute(t *Task, route ownerRoute) bool {
	if t == nil || len(route.Legs) == 0 {
		return false
	}
	leg := route.Legs[0]
	t.OwnerRouteName = route.Name
	t.OwnerRouteLeg = 1
	switch leg.Runner {
	case cursorRunnerName:
		return true
	case kimiCLIRunnerName:
		t.KimiModel = leg.Model
		t.Effort = leg.Effort
		t.EffortExplicit = true
		return true
	case "codex":
		t.PreferRunner = "codex"
		t.CodexModel = leg.Model
		t.Effort = leg.Effort
		t.EffortExplicit = true
		if route.Name != "review_standalone" {
			return false
		}
		t.RouteReason = routeReasonOwnerReviewSol
		return true
	case grokBuildRunnerName:
		t.PreferRunner = grokBuildRunnerName
		t.GrokModel = leg.Model
		t.GrokEffort = leg.Effort
		switch route.Name {
		case "opus_general":
			t.RouteReason = routeReasonGrokOpusGeneral
		case "opus_backend":
			t.RouteReason = routeReasonGrokOpusBackend
		case "sonnet":
			t.RouteReason = routeReasonGrokSonnet
		case "haiku":
			t.RouteReason = routeReasonGrokHaiku
		default:
			return false
		}
		freezeOwnerSolMaxReview(t, route)
		return true
	default:
		return false
	}
}

// ownerPrimaryDispatch is the single production selector for fresh six-row routes. matched=true with
// an empty runner means the current first-leg lane is cooling down and the card must wait, never skip.
func ownerPrimaryDispatch(root string, cfg *Config, t *Task, now time.Time) (runner string, matched bool) {
	route, ok := resolveOwnerRoute(cfg, t)
	if !ok {
		return "", false
	}
	if !pinOwnerPrimaryRoute(t, route) {
		return "", true
	}
	leg := route.Legs[0]
	switch leg.Runner {
	case cursorRunnerName:
		if cursorReady(root, cfg, now) {
			return leg.Runner, true
		}
	case kimiCLIRunnerName:
		if kimiCLIReady(root, cfg, now) {
			return leg.Runner, true
		}
	case grokBuildRunnerName:
		if grokBuildReady(root, cfg, now) {
			return leg.Runner, true
		}
	case "codex":
		return leg.Runner, true
	}
	return "", true
}

type fallbackFailureKind string

const (
	fallbackQuota            fallbackFailureKind = "quota"
	fallbackTransport        fallbackFailureKind = "transport"
	fallbackStreamIncomplete fallbackFailureKind = "stream_incomplete"
	fallbackSemanticStall    fallbackFailureKind = "semantic_stall_timeout"
	fallbackInvalidTerminal  fallbackFailureKind = "invalid_terminal_result"
	fallbackExecutionEnv     fallbackFailureKind = "presemantic_execution_environment"
)

var (
	policyTransportRe      = regexp.MustCompile(`(?i)connection (?:reset|refused|closed)|socket (?:closed|error)|error sending request|tls (?:handshake|error)|unexpected eof|disconnected before|(?:read|write) tcp|dial tcp|i/o timeout|econn(?:reset|refused)|stream disconnect|no route to host|network is unreachable|host is unreachable|temporary failure in name resolution|no such host|dns (?:lookup|resolution) (?:failed|error)|lookup [^\s]+(?: on [^:]+)?: no such host|getaddrinfo\s+(?:enotfound|eai_again)|\b(?:enotfound|eai_again)\b`)
	policyStallRe          = regexp.MustCompile(`(?i)semantic (?:stall|timeout)|model (?:stall|timeout)|步骤超时|context deadline exceeded|deadline exceeded|timed out waiting for (?:model|semantic)`)
	policyInvalidRe        = regexp.MustCompile(`(?i)invalid terminal|invalid final|empty terminal|未返回最终文本|未正常完成|stopreason=|terminal result.*invalid`)
	policyUnsafeTerminalRe = regexp.MustCompile(`(?i)credential(?:s)? (?:missing|invalid|expired|unavailable)|not logged in|login required|unsupported model|model (?:not found|not supported|unavailable)|unknown model|invalid (?:argument|option|flag)|usage: .*--|policy denied`)
	// This is deliberately narrower than a generic permission/filesystem failure. It recognizes the
	// observed Grok CLI cold-start failure where its local session database/directory is read-only;
	// no session or model event exists yet, so the normal zero-residue proof may authorize one serial
	// next-engine transition. It does not turn arbitrary EACCES/permission failures into fallbacks.
	policyGrokReadOnlySessionStoreRe = regexp.MustCompile(`(?i)(?:(?:read-?only(?: file system| database)?|readonly database|erofs).{0,240}(?:\$?grok_home|[/\\]\.grok(?:[/\\]|$))|(?:\$?grok_home|[/\\]\.grok(?:[/\\]|$)).{0,240}(?:read-?only(?: file system| database)?|readonly database|erofs)|grok.{0,80}session(?: database| directory| store)?.{0,80}(?:read-?only|readonly|erofs)|(?:read-?only|readonly|erofs).{0,80}grok.{0,80}session(?: database| directory| store)?)`)
	// Plain stderr has no event framing. Only whole, single-purpose diagnostics may be discarded from
	// the semantic observation stream; a recognized substring followed by prose must remain visible and
	// make the proof incomplete. The broad classifiers above still decide the failure kind separately.
	policyPlainTransportDiagnosticRe    = regexp.MustCompile(`(?i)^(?:error:\s*)?(?:connection (?:reset by peer|refused|closed)|socket (?:closed|error)|error sending request|tls (?:handshake|error)|unexpected eof|disconnected before(?: receiving)?(?: a)? response|i/o timeout|econn(?:reset|refused)|stream disconnect|no route to host|network is unreachable|host is unreachable|temporary failure in name resolution|no such host|dns (?:lookup|resolution) (?:failed|error)|getaddrinfo\s+(?:enotfound|eai_again)(?:\s+[a-z0-9._:-]+)?|dial tcp:\s*lookup\s+[a-z0-9._-]+(?:\s+on\s+[^:;]+)?:\s*no such host|(?:read|write) tcp [^;]+:\s*(?:connection reset by peer|i/o timeout))$`)
	policyPlainStallDiagnosticRe        = regexp.MustCompile(`(?i)^(?:error:\s*)?(?:semantic (?:stall|timeout)|model (?:stall|timeout)|步骤超时|context deadline exceeded|deadline exceeded|timed out waiting for (?:model|semantic))$`)
	policyPlainInvalidDiagnosticRe      = regexp.MustCompile(`(?i)^(?:error:\s*)?(?:invalid terminal(?: result)?|invalid final|empty terminal|未返回最终文本|未正常完成|terminal result invalid)$`)
	policyPlainGrokReadOnlyDiagnosticRe = regexp.MustCompile(`(?i)^(?:error:\s*)?(?:(?:erofs:\s*)?read-?only file system,\s*(?:mkdir|open|write|create)\s+['"]?(?:\$grok_home(?:[/\\][a-z0-9._-]+)*|~[/\\]\.grok(?:[/\\][a-z0-9._-]+)*|(?:/[a-z0-9._~+-]+)+/\.grok(?:/[a-z0-9._-]+)*|[a-z]:\\(?:[a-z0-9._ ~+-]+\\)*\.grok(?:\\[a-z0-9._ -]+)*)['"]?|(?:\$grok_home(?:[/\\][a-z0-9._-]+)*|~[/\\]\.grok(?:[/\\][a-z0-9._-]+)*|(?:/[a-z0-9._~+-]+)+/\.grok(?:/[a-z0-9._-]+)*|[a-z]:\\(?:[a-z0-9._ ~+-]+\\)*\.grok(?:\\[a-z0-9._ -]+)*)\s*:\s*(?:read-?only(?: file system| database)?|readonly database|erofs)|grok session(?: database| directory| store)?\s+(?:is\s+)?(?:read-?only|readonly|erofs))$`)
	policyPlainQuotaDiagnosticRe        = regexp.MustCompile(`(?i)^(?:error:\s*)?(?:(?:http(?:/\d(?:\.\d)?)?\s+)?429(?::\s*(?:too many requests|rate limit(?:ed)?|usage limit(?: reached)?|quota (?:exceeded|exhausted)))?|too many requests|rate limit|usage limit|quota (?:exceeded|exhausted)|insufficient (?:quota|credits)|out of (?:credits|usage)|额度(?:不足|已用完)|配额(?:不足|已用尽)|限额(?:不足|已用尽))$`)
)

// providerJSONObservation preserves the full stdout contract while also admitting structured JSONL
// events written to stderr by a provider wrapper. Plain stderr remains diagnostic input for failure
// classification; it is not model output. This closes the unsafe case where an assistant/tool event
// on stderr was invisible to the zero-work proof.
func providerPlainStderrKnownPresemantic(via, line string) bool {
	line = strings.TrimSpace(line)
	if policyPlainTransportDiagnosticRe.MatchString(line) || policyPlainStallDiagnosticRe.MatchString(line) ||
		policyPlainInvalidDiagnosticRe.MatchString(line) {
		return true
	}
	switch via {
	case kimiCLIRunnerName:
		return policyPlainQuotaDiagnosticRe.MatchString(line)
	case grokBuildRunnerName:
		return policyPlainQuotaDiagnosticRe.MatchString(line) || policyPlainGrokReadOnlyDiagnosticRe.MatchString(line)
	case cursorRunnerName:
		return policyPlainQuotaDiagnosticRe.MatchString(line)
	default:
		return false
	}
}

func providerJSONObservation(via, stdout, stderr string) string {
	var out strings.Builder
	out.WriteString(stdout)
	s := strings.NewReader(stderr)
	scanner := bufio.NewScanner(s)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !json.Valid([]byte(line)) {
			// Only a narrow provider-specific set of startup/terminal diagnostics is known to be
			// presemantic. All other plain stderr may contain semantic or tool prose and therefore makes
			// the observation incomplete; a second writer remains forbidden.
			if !strings.HasPrefix(line, "{") && !strings.HasPrefix(line, "[") &&
				providerPlainStderrKnownPresemantic(via, line) {
				continue
			}
			line = "__CARDEX_MALFORMED_STDERR_JSON__"
		}
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(line)
	}
	if scanner.Err() != nil {
		out.WriteString("\n__CARDEX_UNOBSERVED_STDERR__")
	}
	return out.String()
}

func policyUnsafeTerminal(scan string) bool {
	return authClassRe.MatchString(scan) || permissionClassRe.MatchString(scan) ||
		inputTooLongClassRe.MatchString(scan) || policyUnsafeTerminalRe.MatchString(scan)
}

// policyFailureScanText removes echoed user/assistant prose before generic failure matching. Without
// this boundary, a prompt such as "diagnose network error" plus an unrelated auth failure could be
// misclassified as transport failure and authorize a different writer. Provider helpers retain only
// structured error events, stderr/non-stream lines, and an already-classified error result.
func policyFailureScanText(via string, res *claudeResult, combined string, runErr error) string {
	var scan string
	switch via {
	case kimiCLIRunnerName:
		scan = kimiCLILimitScanText(res, combined)
	case grokBuildRunnerName:
		scan = grokBuildLimitScanText(res, combined)
	case cursorRunnerName:
		scan = cursorErrorScanText(res, combined)
	default:
		scan = combined
	}
	if runErr != nil {
		scan += "\n" + runErr.Error()
	}
	return scan
}

func classifyPolicyFallbackFailure(via string, res *claudeResult, combined string, runErr error) (fallbackFailureKind, bool) {
	if runErr == nil && res != nil && !res.IsError {
		return "", false
	}
	scan := policyFailureScanText(via, res, combined, runErr)
	if res != nil {
		scan += "\n" + res.Subtype
	}
	// Authentication, permission, unsupported-model, malformed invocation, and oversized-input
	// terminals are never safe reasons to switch writers. Check these before quota/transport because
	// a wrapper may report more than one phrase in the same terminal diagnostic.
	if policyUnsafeTerminal(scan) {
		return "", false
	}
	if via == grokBuildRunnerName && res != nil && res.Subtype == "grok_build_process_error" &&
		res.SemanticEvents == 0 && res.ModelEvents == 0 && res.ToolEvents == 0 &&
		policyGrokReadOnlySessionStoreRe.MatchString(scan) {
		return fallbackExecutionEnv, true
	}
	switch via {
	case kimiCLIRunnerName:
		if isLimitHitKimiCLI(res, combined) {
			return fallbackQuota, true
		}
		if runErr != nil && kimiCLI0361VersionOnly(combined) {
			return fallbackTransport, true
		}
	case grokBuildRunnerName:
		if isLimitHitGrokBuild(res, combined) {
			return fallbackQuota, true
		}
	case cursorRunnerName:
		if isLimitHitCursor(res, combined) {
			return fallbackQuota, true
		}
		if isCursorDataPolicyGate(res, combined) {
			// A structured presemantic policy terminal is an invalid terminal result for this card.
			// It never authorizes Cardex to acknowledge the policy; it only enters the independent chain.
			return fallbackInvalidTerminal, true
		}
	default:
		return "", false
	}
	subtype := ""
	if res != nil {
		subtype = strings.ToLower(strings.TrimSpace(res.Subtype))
	}
	if policyStallRe.MatchString(scan) || strings.Contains(subtype, "semantic_stall") {
		return fallbackSemanticStall, true
	}
	if strings.Contains(subtype, "invalid_terminal") || policyInvalidRe.MatchString(scan) {
		return fallbackInvalidTerminal, true
	}
	if policyTransportRe.MatchString(scan) {
		return fallbackTransport, true
	}
	if strings.Contains(subtype, "stream_incomplete") {
		return fallbackStreamIncomplete, true
	}
	return "", false
}

type policyWorkspaceFingerprint struct {
	Root   string
	Digest string
}

// hashPolicyWorkspaceTree records every path, type, permission bit, symlink target, and regular-file
// byte outside the root .git entry. Hashing actual tracked bytes (rather than trusting Git diff alone)
// is required for skip-worktree/assume-unchanged paths; directories are included so mkdir residue is
// visible. Any unreadable or special entry fails closed.
func writePolicyFingerprintField(h io.Writer, data []byte) error {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(data)))
	if _, err := h.Write(size[:]); err != nil {
		return err
	}
	_, err := h.Write(data)
	return err
}

func hashPolicyWorkspaceTree(h io.Writer, root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		kind := ""
		switch mode := info.Mode(); {
		case mode.IsDir():
			kind = "dir"
		case mode.IsRegular():
			kind = "file"
		case mode&os.ModeSymlink != 0:
			kind = "symlink"
		default:
			return fmt.Errorf("workspace fingerprint refuses special entry %s (%s)", path, mode)
		}
		for _, field := range []string{rel, kind, fmt.Sprintf("%#o", info.Mode().Perm())} {
			if err := writePolicyFingerprintField(h, []byte(field)); err != nil {
				return err
			}
		}
		switch kind {
		case "file":
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			stat, statErr := f.Stat()
			if statErr != nil {
				_ = f.Close()
				return statErr
			}
			var size [8]byte
			binary.BigEndian.PutUint64(size[:], uint64(stat.Size()))
			if _, err := h.Write(size[:]); err != nil {
				_ = f.Close()
				return err
			}
			_, copyErr := io.CopyN(h, f, stat.Size())
			var extra [1]byte
			extraN, extraErr := f.Read(extra[:])
			if copyErr == nil && (extraN != 0 || (extraErr != nil && !errors.Is(extraErr, io.EOF))) {
				copyErr = fmt.Errorf("workspace file changed while hashing: %s", path)
			}
			closeErr := f.Close()
			return errors.Join(copyErr, closeErr)
		case "symlink":
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return writePolicyFingerprintField(h, []byte(target))
		default:
			return writePolicyFingerprintField(h, nil)
		}
	})
}

func gitOutput(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return out, nil
}

func captureGitPolicyFingerprint(root string) (*policyWorkspaceFingerprint, error) {
	h := sha256.New()
	_, _ = io.WriteString(h, "cardex-policy-workspace-v1\x00git\x00")
	if err := hashPolicyWorkspaceTree(h, root); err != nil {
		return nil, fmt.Errorf("fingerprint workspace tree: %w", err)
	}
	head, err := gitOutput(root, "rev-parse", "HEAD")
	unborn := err != nil
	if err != nil {
		// Unborn repositories still have a fully comparable worktree; use an explicit sentinel.
		head = []byte("UNBORN\n")
	}
	h.Write(head)
	// A writer also mutates repository identity, not only bytes. Two branches may resolve to the same
	// commit while subsequent commits land on different refs; linked worktrees likewise have distinct
	// gitdirs. Bind all three identities so a branch/worktree switch cannot authorize the next writer.
	gitDir, err := gitOutput(root, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, fmt.Errorf("fingerprint gitdir identity: %w", err)
	}
	commonDir, err := gitOutput(root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, fmt.Errorf("fingerprint common gitdir identity: %w", err)
	}
	symbolicHead, symbolicErr := gitOutput(root, "symbolic-ref", "-q", "HEAD")
	if symbolicErr != nil {
		symbolicHead = []byte("DETACHED\n")
	}
	_, _ = io.WriteString(h, "\x00GITDIR\x00")
	h.Write(gitDir)
	_, _ = io.WriteString(h, "\x00COMMON_GITDIR\x00")
	h.Write(commonDir)
	_, _ = io.WriteString(h, "\x00SYMBOLIC_HEAD\x00")
	h.Write(symbolicHead)
	cachedArgs := []string{"diff", "--cached", "--binary", "--no-ext-diff"}
	if !unborn {
		cachedArgs = append(cachedArgs, "HEAD")
	}
	cachedArgs = append(cachedArgs, "--")
	cached, err := gitOutput(root, cachedArgs...)
	if err != nil {
		return nil, fmt.Errorf("fingerprint staged bytes: %w", err)
	}
	_, _ = io.WriteString(h, "\x00INDEX\x00")
	h.Write(cached)
	// The staged diff is not an exact representation of Git's index. Flags such as skip-worktree
	// and assume-unchanged can change writer-visible state without changing `git diff --cached`.
	// Hash the authoritative index bytes as well, including in linked worktrees.
	indexOut, err := gitOutput(root, "rev-parse", "--git-path", "index")
	if err != nil {
		return nil, fmt.Errorf("fingerprint git index path: %w", err)
	}
	indexPath := strings.TrimSpace(string(indexOut))
	if indexPath == "" {
		return nil, fmt.Errorf("fingerprint git index path is empty")
	}
	if !filepath.IsAbs(indexPath) {
		indexPath = filepath.Join(root, indexPath)
	}
	_, _ = io.WriteString(h, "\x00INDEX_FILE\x00")
	indexInfo, indexErr := os.Lstat(indexPath)
	switch {
	case indexErr == nil && indexInfo.Mode().IsRegular():
		indexFile, openErr := os.Open(indexPath)
		if openErr != nil {
			return nil, fmt.Errorf("fingerprint git index bytes: %w", openErr)
		}
		_, copyErr := io.Copy(h, indexFile)
		closeErr := indexFile.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return nil, fmt.Errorf("fingerprint git index bytes: %w", err)
		}
	case os.IsNotExist(indexErr):
		// A completely empty/unborn repository may not have materialized an index yet.
		_, _ = io.WriteString(h, "ABSENT")
	case indexErr != nil:
		return nil, fmt.Errorf("fingerprint git index: %w", indexErr)
	default:
		return nil, fmt.Errorf("fingerprint git index refuses non-regular path %s (%s)", indexPath, indexInfo.Mode())
	}
	unstaged, err := gitOutput(root, "diff", "--binary", "--no-ext-diff", "--")
	if err != nil {
		return nil, fmt.Errorf("fingerprint unstaged bytes: %w", err)
	}
	_, _ = io.WriteString(h, "\x00WORKTREE\x00")
	h.Write(unstaged)
	return &policyWorkspaceFingerprint{Root: root, Digest: hex.EncodeToString(h.Sum(nil))}, nil
}

func capturePlainPolicyFingerprint(dir string) (*policyWorkspaceFingerprint, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	_, _ = io.WriteString(h, "cardex-policy-workspace-v1\x00plain\x00")
	if err := hashPolicyWorkspaceTree(h, abs); err != nil {
		return nil, err
	}
	return &policyWorkspaceFingerprint{Root: abs, Digest: hex.EncodeToString(h.Sum(nil))}, nil
}

// capturePolicyWorkspaceFingerprint compares the exact workspace tree (tracked, pre-dirty, untracked,
// ignored, permission bits, symlinks, and empty directories). Git repositories additionally include
// HEAD, staged/unstaged binary diffs, and exact index bytes. Any unreadable/special entry fails closed.
func capturePolicyWorkspaceFingerprint(dir string) (*policyWorkspaceFingerprint, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("workspace fingerprint requires a task directory")
	}
	rootBytes, err := gitOutput(dir, "rev-parse", "--show-toplevel")
	if err == nil {
		root := strings.TrimSpace(string(rootBytes))
		if root == "" {
			return nil, fmt.Errorf("git returned an empty worktree root")
		}
		return captureGitPolicyFingerprint(root)
	}
	return capturePlainPolicyFingerprint(dir)
}

type policyFallbackProof struct {
	Before              *policyWorkspaceFingerprint
	After               *policyWorkspaceFingerprint
	SemanticEvents      int
	ModelEvents         int
	ToolEvents          int
	ObservationComplete bool
	ProcessResidue      bool
}

type fallbackAuthorization struct {
	verified     bool
	beforeDigest string
	afterDigest  string
}

func authorizePolicyFallback(proof policyFallbackProof) (fallbackAuthorization, error) {
	if !policyFallbackProcessProofSupported() {
		return fallbackAuthorization{}, fmt.Errorf("fallback proof unavailable: platform cannot prove descendant process cleanup")
	}
	if proof.Before == nil || proof.After == nil || proof.Before.Digest == "" || proof.After.Digest == "" {
		return fallbackAuthorization{}, fmt.Errorf("fallback proof unavailable: workspace fingerprint missing")
	}
	if !proof.ObservationComplete {
		return fallbackAuthorization{}, fmt.Errorf("fallback proof unavailable: semantic/model/tool observation incomplete")
	}
	if proof.SemanticEvents != 0 {
		return fallbackAuthorization{}, fmt.Errorf("fallback blocked: semantic/model events=%d", proof.SemanticEvents)
	}
	if proof.ModelEvents != 0 {
		return fallbackAuthorization{}, fmt.Errorf("fallback blocked: model events=%d", proof.ModelEvents)
	}
	if proof.ToolEvents != 0 {
		return fallbackAuthorization{}, fmt.Errorf("fallback blocked: tool events=%d", proof.ToolEvents)
	}
	if proof.Before.Root != proof.After.Root || proof.Before.Digest != proof.After.Digest {
		return fallbackAuthorization{}, fmt.Errorf("fallback blocked: product/worktree fingerprint changed")
	}
	if proof.ProcessResidue {
		return fallbackAuthorization{}, fmt.Errorf("fallback blocked: surviving writer/process residue")
	}
	return fallbackAuthorization{verified: true, beforeDigest: proof.Before.Digest, afterDigest: proof.After.Digest}, nil
}

func policyFallbackCandidate(cfg *Config, t *Task, via string) bool {
	if cfg == nil || t == nil {
		return false
	}
	switch via {
	case cursorRunnerName:
		return t.RouteReason == routeReasonCursorFable || cursorFablePolicyApplies(cfg, t)
	case kimiCLIRunnerName:
		return t.RouteReason == routeReasonKimiCLIOpus || t.RouteReason == routeReasonGrokToKimiPending ||
			t.RouteReason == routeReasonGrokToKimi
	case grokBuildRunnerName:
		switch t.RouteReason {
		case routeReasonKimiToGrokPending, routeReasonKimiToGrok,
			routeReasonGrokOpusGeneral, routeReasonGrokOpusBackend, routeReasonGrokSonnet, routeReasonGrokHaiku:
			return true
		}
	}
	return false
}

func resetQueuedPolicyLeg(t *Task) {
	t.Status = statusQueued
	t.NotBeforeEpoch = 0
	t.ResumeAtEpoch = 0
	t.SessionID = ""
	t.MidStep = false
	t.Attempts = 0
}

func policyFallbackResolvedModel(cfg *Config, t *Task) string {
	if t == nil {
		return ""
	}
	switch t.PreferRunner {
	case grokBuildRunnerName:
		return resolveGrokBuildModel(cfg, t)
	case kimiCLIRunnerName:
		return resolveKimiCLIModel(cfg, t)
	case "codex":
		return resolveCodexModel(cfg, t)
	default:
		return ""
	}
}

func policyFallbackResolvedEffort(cfg *Config, t *Task) string {
	if t == nil {
		return ""
	}
	switch t.PreferRunner {
	case grokBuildRunnerName:
		return resolveGrokBuildEffort(cfg, t)
	case kimiCLIRunnerName:
		return resolveKimiCLIEffort(cfg, t)
	case "codex":
		return resolveCodexReasoning(cfg, t)
	default:
		return ""
	}
}

// queuePolicyFallback is the only mutation helper for Grok/Kimi/Codex next-leg transitions. Its opaque
// authorization is created only after the previous invocation returned and all three proof axes pass.
func queuePolicyFallback(cfg *Config, t *Task, kind fallbackFailureKind, auth fallbackAuthorization) error {
	if !auth.verified || auth.beforeDigest == "" || auth.beforeDigest != auth.afterDigest {
		return fmt.Errorf("fallback transition refused without verified serial authorization")
	}
	if t == nil {
		return fmt.Errorf("fallback transition requires a task")
	}
	route, ok := resolveOwnerRouteReadback(cfg, t)
	if !ok || t.OwnerRouteLeg < 1 || t.OwnerRouteLeg >= len(route.Legs) {
		return fmt.Errorf("task has no resolver-proven next route leg")
	}
	current := route.Legs[t.OwnerRouteLeg-1]
	if current.Runner != t.Runner && current.Runner != t.PreferRunner {
		return fmt.Errorf("task current provider does not match resolver leg %d", t.OwnerRouteLeg)
	}
	nextIndex := t.OwnerRouteLeg
	next := route.Legs[nextIndex]
	// Clear every provider-specific pin before freezing the next leg. The route snapshot and last
	// attempt readback retain the previous identity; carrying its concrete fields would make readback
	// ambiguous and could silently substitute a provider after config reload.
	t.PreferRunner = "codex"
	t.CodexModel = ""
	t.XCodexModel = ""
	t.GeminiModel = ""
	t.OpenCodeModel = ""
	t.KimiModel = ""
	t.GrokModel = ""
	t.GrokEffort = ""
	t.CursorModel = ""
	t.Effort = ""
	t.EffortExplicit = false
	switch next.Runner {
	case grokBuildRunnerName:
		t.PreferRunner = grokBuildRunnerName
		t.GrokModel, t.GrokEffort = next.Model, next.Effort
		t.RouteReason = routeReasonKimiToGrokPending // legacy in-flight compatibility only
	case kimiCLIRunnerName:
		t.PreferRunner = kimiCLIRunnerName
		t.KimiModel = next.Model
		t.Effort, t.EffortExplicit = next.Effort, true
		t.RouteReason = routeReasonGrokToKimiPending
	case "codex":
		t.PreferRunner = "codex"
		t.CodexModel = next.Model
		t.Effort, t.EffortExplicit = next.Effort, true
		switch route.Name {
		case "sonnet":
			t.RouteReason = routeReasonGrokSonnetToLunaPending
		case "haiku":
			t.RouteReason = routeReasonGrokHaikuToLunaPending
		case "opus_general":
			t.RouteReason = routeReasonKimiToSolPending
		default:
			t.RouteReason = routeReasonGrokToSolPending
		}
	default:
		return fmt.Errorf("resolver next leg uses unsupported provider %q", next.Runner)
	}
	t.OwnerRouteName = route.Name
	t.OwnerRouteLeg = nextIndex + 1
	freezeOwnerSolMaxReview(t, route)
	resetQueuedPolicyLeg(t)
	t.LastError = fmt.Sprintf("%s 安全回退已证明，串行排队下一执行腿", kind)
	t.touch()
	return nil
}
