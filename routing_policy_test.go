package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func policyTestConfig() *Config {
	cfg := defaultConfig("")
	cfg.DefaultRunner = "codex"
	cfg.OwnerRoutingEnforced = true
	cfg.AutomaticCodexBudgetStopPercent = 65
	cfg.OwnerProviderTargets = &OwnerProviderTargets{
		GrokMinPercent: 70, GrokMaxPercent: 80,
		KimiMinPercent: 15, KimiMaxPercent: 25,
		DirectSolMinPercent: 5, DirectSolMaxPercent: 10,
	}
	cfg.CodexBin = "/usr/bin/true"
	cfg.CodexModel = "gpt-5.6-sol"
	cfg.GrokBuildBin = "/usr/bin/true"
	cfg.KimiCLIBin = "/usr/bin/true"
	cfg.KimiCLIOpus = &KimiCLIOpusRoute{
		Enabled: true, ExcludeBackend: true, Model: "kimi-code/k3", Effort: "max",
	}
	cfg.GrokBuild = &GrokBuildRoute{
		Enabled: true, Model: "grok-4.6", Effort: "xhigh", KimiOpusFallback: true,
		CodexFallbackModel: "gpt-5.6-sol", CodexFallbackEffort: "xhigh",
		ReviewCodexModel: "gpt-5.6-sol", ReviewCodexEffort: "max",
		TierRoutes: map[string]GrokTierRoute{
			"opus_backend": {Effort: "xhigh", CodexFallbackModel: "gpt-5.6-sol", CodexFallbackEffort: "max"},
			"sonnet":       {Effort: "high", CodexFallbackModel: "gpt-5.6-luna", CodexFallbackEffort: "max"},
			"haiku":        {Effort: "medium", CodexFallbackModel: "gpt-5.6-luna", CodexFallbackEffort: "xhigh"},
		},
	}
	cfg.CursorBin = "/usr/bin/true"
	cfg.CursorModel = "cursor-grok-4.6-xhigh"
	cfg.CursorFable = &CursorFableRoute{
		Enabled: true, Model: "claude-fable-5-thinking-max", FallbackProfile: "fable-dual",
	}
	cfg.CrossProfiles = map[string]CrossProfile{
		"fable-dual": {
			A: CrossEngine{Kind: grokBuildRunnerName, Model: "grok-4.6", Effort: "xhigh"},
			B: CrossEngine{Kind: "codex", Effort: "ultra"},
		},
	}
	return cfg
}

func TestOwnerRouteResolutionFinalMatrixLock(t *testing.T) {
	cfg := policyTestConfig()
	if err := validateOwnerRoutingPolicy(cfg); err != nil {
		t.Fatalf("final Owner matrix rejected: %v", err)
	}
	cfg.CodexFallback = true
	if err := validateOwnerRoutingPolicy(cfg); err == nil || !strings.Contains(err.Error(), "codex_fallback=false") {
		t.Fatalf("global Codex fallback drift must fail closed: %v", err)
	}
	cfg.CodexFallback = false
	cfg.GrokBuild.FableClaudeFallback = true
	cfg.GrokBuild.FableFirstPrinciples = true
	if err := validateOwnerRoutingPolicy(cfg); err == nil || !strings.Contains(err.Error(), "legacy Fable") {
		t.Fatalf("legacy Fable third-leg flags must fail closed: %v", err)
	}
}

func TestStandaloneReviewUsesDirectSolMax(t *testing.T) {
	cfg := policyTestConfig()
	task := &Task{Type: typeReview, Model: "opus", RiskClass: riskClassProduction, PreferRunner: "codex", Prompts: []string{"review"}}
	route, ok := resolveOwnerRoute(cfg, task)
	if !ok || route.Name != "review_standalone_critical" || len(route.Legs) != 1 ||
		route.Legs[0] != (policyLeg{Runner: "codex", Model: "gpt-5.6-sol", Effort: "max", Stage: routeStageStandaloneReview, ReadOnly: true}) ||
		route.Review != nil {
		t.Fatalf("standalone review must route directly to Sol/max without review-of-review: %+v ok=%v", route, ok)
	}
	if runner, matched := ownerPrimaryDispatch(testRoot(t), cfg, task, time.Now()); !matched || runner != "codex" ||
		task.CodexModel != "gpt-5.6-sol" || task.Effort != "max" || !task.EffortExplicit || !task.AutomaticCodex {
		t.Fatalf("standalone review primary was not frozen to direct Sol/max: runner=%q matched=%v task=%+v", runner, matched, task)
	}
	brief := toBrief(cfg, task, time.Now())
	if brief.Runner != "codex" || brief.Model != "gpt-5.6-sol" || brief.Effort != "max" ||
		brief.ModelRoute != "Codex GPT-5.6 Sol/max" {
		t.Fatalf("board did not read back standalone Sol/max review: %+v", brief)
	}
}

func TestOwnerRouteDrivesManualCommandAndBoardForFinalMatrixBranches(t *testing.T) {
	cfg := policyTestConfig()
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name, model, routeClass, runner, wantModel, wantEffort, commandToken string
	}{
		{"fable", "fable", routeClassGeneral, cursorRunnerName, "claude-fable-5-thinking-max", "max", "--output-format stream-json"},
		{"opus general", "opus", routeClassGeneral, grokBuildRunnerName, "grok-4.6", "xhigh", "--reasoning-effort xhigh"},
		{"opus backend", "opus", routeClassBackend, grokBuildRunnerName, "grok-4.6", "xhigh", "--reasoning-effort xhigh"},
		{"sonnet", "sonnet", routeClassGeneral, grokBuildRunnerName, "grok-4.6", "high", "--reasoning-effort high"},
		{"haiku", "haiku", routeClassGeneral, grokBuildRunnerName, "grok-4.6", "medium", "--reasoning-effort medium"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			task := &Task{ID: "manual-" + strings.ReplaceAll(tc.name, " ", "-"), Type: typeSequence,
				Status: statusQueued, Model: tc.model, RouteClass: tc.routeClass, PreferRunner: "codex",
				FreshSteps: true, Dir: t.TempDir(), Prompts: []string{"implement"}}
			route, leg, command, ok := ownerManualDispatchCommand(cfg, task, "implement")
			if !ok || route.Name == "" || leg.Runner != tc.runner || leg.Model != tc.wantModel || leg.Effort != tc.wantEffort {
				t.Fatalf("manual resolver mismatch: route=%+v leg=%+v ok=%v", route, leg, ok)
			}
			if !strings.Contains(command, tc.wantModel) || !strings.Contains(command, tc.commandToken) {
				t.Fatalf("manual command does not use resolved provider identity: %s", command)
			}
			if tc.runner == grokBuildRunnerName {
				const want = "/usr/bin/true --no-auto-update --model grok-4.6"
				if !strings.Contains(command, want) || strings.Count(command, "--no-auto-update") != 1 {
					t.Fatalf("manual Grok command must mirror the runtime no-auto-update position exactly: %s", command)
				}
			}
			brief := toBrief(cfg, task, now)
			if brief.Runner != tc.runner || brief.Model != tc.wantModel || brief.Effort != tc.wantEffort ||
				brief.RunnerSource != "route_policy" || brief.ModelRoute == "" {
				t.Fatalf("board did not consume owner resolver: %+v", brief)
			}
		})
	}
}

func TestOwnerRouteLeavesPinnedAndStatefulCardsUntouched(t *testing.T) {
	cfg := policyTestConfig()
	base := Task{Type: typeSequence, Model: "opus", RouteClass: routeClassGeneral,
		PreferRunner: "codex", FreshSteps: true, Prompts: []string{"p"}}
	tests := []struct {
		name string
		edit func(*Task)
	}{
		{"explicit runner", func(t *Task) { t.PreferRunner = kimiCLIRunnerName }},
		{"explicit codex runner", func(t *Task) { t.RunnerExplicit = true }},
		{"explicit codex model", func(t *Task) { t.CodexModel = "gpt-5.6-sol" }},
		{"explicit xcodex model", func(t *Task) { t.XCodexModel = "gpt-5.6-sol" }},
		{"explicit gemini model", func(t *Task) { t.GeminiModel = "gemini-2.5-pro" }},
		{"explicit opencode model", func(t *Task) { t.OpenCodeModel = "provider/model" }},
		{"explicit kimi model", func(t *Task) { t.KimiModel = "kimi-code/k3" }},
		{"explicit grok model", func(t *Task) { t.GrokModel = "grok-4.6" }},
		{"explicit grok effort", func(t *Task) { t.GrokEffort = "xhigh" }},
		{"explicit cursor model", func(t *Task) { t.CursorModel = "claude-fable-5-thinking-max" }},
		{"established session", func(t *Task) { t.SessionID = "existing" }},
		{"remote", func(t *Task) { t.RemoteHost = "win5090" }},
		{"cross profile", func(t *Task) { t.XRole = "A" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			probe := base
			tc.edit(&probe)
			if _, ok := resolveOwnerRoute(cfg, &probe); ok {
				t.Fatalf("explicit/stateful task entered auto-routing: %+v", probe)
			}
		})
	}
	for name, probe := range map[string]*Task{
		"implicit Fable": {Type: typeSequence, PreferRunner: "codex", Prompts: []string{"p"}},
		"multi-step Fable": {Type: typeSequence, Model: "fable", PreferRunner: "codex", FreshSteps: true,
			Prompts: []string{"p1", "p2"}},
	} {
		if _, ok := resolveOwnerRoute(cfg, probe); ok {
			t.Fatalf("%s must not enter the explicit fresh single-step Fable route", name)
		}
		if name == "multi-step Fable" && !cursorFableOwnerRouteRequired(cfg, probe) {
			t.Fatal("unsupported explicit Fable shape must be held instead of falling through to generic Codex")
		}
	}
	missingCursor := policyTestConfig()
	missingCursor.CursorFable.Enabled = false
	oneStep := &Task{Type: typeSequence, Model: "fable", PreferRunner: "codex", FreshSteps: true,
		Prompts: []string{"p"}}
	if cursorFablePolicyApplies(missingCursor, oneStep) || !cursorFableOwnerRouteRequired(missingCursor, oneStep) {
		t.Fatal("explicit Fable without a usable owner route must wait, never fall through to generic Codex")
	}
}

func TestOwnerReadbackRejectsLaterExplicitPin(t *testing.T) {
	cfg := policyTestConfig()
	task := &Task{Type: typeSequence, Model: "opus", RouteClass: routeClassGeneral,
		PreferRunner: "codex", FreshSteps: true, Dir: t.TempDir(), Prompts: []string{"implement"}}
	if runner, matched := ownerPrimaryDispatch(testRoot(t), cfg, task, time.Now()); !matched || runner != grokBuildRunnerName {
		t.Fatalf("primary dispatch=(%q,%v)", runner, matched)
	}
	if route, ok := resolveOwnerRouteReadback(cfg, task); !ok || route.Name != "opus_non_backend" {
		t.Fatalf("frozen primary snapshot should read back: route=%+v ok=%v", route, ok)
	}
	task.RunnerExplicit = true
	task.PreferRunner = "codex"
	task.CodexModel = "gpt-5.6-sol"
	task.GrokModel = ""
	task.GrokEffort = ""
	task.RouteReason = ""
	task.OwnerRouteName = ""
	task.OwnerRouteLeg = 0
	task.Runner = ""
	if _, ok := resolveOwnerRouteReadback(cfg, task); ok {
		t.Fatal("a later explicit runner/model pin must invalidate the old Owner route snapshot")
	}
	if got := effectiveModelRoute(cfg, task); strings.Contains(got, "Kimi") || strings.Contains(got, "Grok") {
		t.Fatalf("board must not display the stale automatic chain after explicit pin: %q", got)
	}
	brief := toBrief(cfg, task, time.Now())
	if brief.Runner != "codex" || brief.Model != "gpt-5.6-sol" || strings.Contains(brief.ModelRoute, "Kimi") {
		t.Fatalf("board effective identity must follow the later explicit Codex pin: %+v", brief)
	}
}

func TestPinnedAndRemoteManualCommandsUseFrozenRunnerIdentity(t *testing.T) {
	cfg := policyTestConfig()
	cfg.OpenCodeBin = "/usr/bin/opencode"
	cfg.RemoteHosts = map[string]RemoteHostConfig{
		"buildbox": {CodexBin: "codex", CodexOnly: true, Sandbox: "workspace-write", Shell: "posix"},
	}
	tests := []struct {
		name string
		task *Task
		want string
	}{
		{"codex fable pin", &Task{Type: typeSequence, Model: "fable", PreferRunner: "codex", RunnerExplicit: true, CodexModel: "gpt-5.6-sol", Effort: "max", Dir: "/tmp/repo"}, "exec -C"},
		{"kimi pin", &Task{Type: typeSequence, PreferRunner: kimiCLIRunnerName, RunnerExplicit: true, KimiModel: "kimi-code/k3", Effort: "max", Dir: "/tmp/repo"}, "kimi-code/k3"},
		{"grok pin", &Task{Type: typeSequence, PreferRunner: grokBuildRunnerName, RunnerExplicit: true, GrokModel: "grok-4.6", GrokEffort: "xhigh", Dir: "/tmp/repo"}, "--reasoning-effort xhigh"},
		{"cursor pin", &Task{Type: typeSequence, PreferRunner: cursorRunnerName, RunnerExplicit: true, CursorModel: "cursor-grok-4.6-xhigh", Dir: "/tmp/repo"}, "cursor-grok-4.6-xhigh"},
		{"opencode pin", &Task{Type: typeSequence, PreferRunner: "opencode", RunnerExplicit: true, OpenCodeModel: "provider/model", Effort: "high", Dir: "/tmp/repo"}, "opencode run"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			leg, ok := resolvePinnedTaskLeg(cfg, tc.task)
			if !ok {
				t.Fatal("pinned identity did not resolve")
			}
			command, ok := manualDispatchCommandForLeg(cfg, tc.task, "prompt", leg)
			if !ok || !strings.Contains(command, tc.want) {
				t.Fatalf("manual command=%q ok=%v want token %q", command, ok, tc.want)
			}
		})
	}
	remote := &Task{Type: typeSequence, PreferRunner: "codex", RunnerExplicit: true, RemoteHost: "buildbox",
		CodexModel: "gpt-5.6-sol", Effort: "max", Dir: "/srv/repo"}
	command, err := remoteManualDispatchCommand(cfg, remote, "prompt")
	if err != nil || !strings.Contains(command, "ssh") || !strings.Contains(command, "buildbox") ||
		!strings.Contains(command, "codex exec") {
		t.Fatalf("remote manual command=%q err=%v", command, err)
	}
}

func TestOwnerRouteConfigRejectsIdentityDrift(t *testing.T) {
	base := policyTestConfig()
	if err := validateGrokBuild(base); err != nil {
		t.Fatalf("exact owner Grok table rejected: %v", err)
	}
	if err := validateCursor(base); err != nil {
		t.Fatalf("exact owner Cursor table rejected: %v", err)
	}
	if err := validateOwnerRoutingPolicy(base); err != nil {
		t.Fatalf("exact six-row Owner route rejected: %v", err)
	}

	t.Run("backend fallback effort", func(t *testing.T) {
		cfg := policyTestConfig()
		route := cfg.GrokBuild.TierRoutes["opus_backend"]
		route.CodexFallbackEffort = "xhigh"
		cfg.GrokBuild.TierRoutes["opus_backend"] = route
		if err := validateGrokBuild(cfg); err != nil {
			t.Fatalf("legacy global Codex fallback fields are inert in Owner mode, got %v", err)
		}
		task := finalOwnerTask("opus", routeClassBackend, riskClassHigh)
		resolved, _ := resolveOwnerRoute(cfg, task)
		if resolved.ReleaseGate == nil || resolved.ReleaseGate.Effort != "max" {
			t.Fatalf("explicit high-risk release gate must remain Sol/max: %+v", resolved)
		}
	})
	t.Run("Grok model identity", func(t *testing.T) {
		cfg := policyTestConfig()
		cfg.GrokBuild.Model = "grok-4.5"
		if err := validateGrokBuild(cfg); err == nil {
			t.Fatal("owner tier routes must reject a non-4.6 Grok model")
		}
	})
	t.Run("review identity", func(t *testing.T) {
		cfg := policyTestConfig()
		cfg.GrokBuild.ReviewCodexEffort = "xhigh"
		task := &Task{Type: typeReview, RiskClass: riskClassProduction, PreferRunner: "codex", Prompts: []string{"review"}}
		resolved, ok := resolveOwnerRoute(cfg, task)
		if !ok || resolved.Legs[0].Effort != "max" {
			t.Fatalf("legacy review field must not alter explicit standalone Sol/max: %+v", resolved)
		}
	})
	t.Run("automatic review remains disabled", func(t *testing.T) {
		cfg := policyTestConfig()
		cfg.GrokBuild.OpusAdversarialReview = true
		if err := validateOwnerRoutingPolicy(cfg); err == nil {
			t.Fatal("owner table must not silently add automatic reviews to Opus implementation cards")
		}
	})
	t.Run("complete six-row table", func(t *testing.T) {
		cfg := policyTestConfig()
		delete(cfg.GrokBuild.TierRoutes, "haiku")
		if err := validateGrokBuild(cfg); err == nil {
			t.Fatal("an enabled owner Kimi route must not silently omit another owner row")
		}
	})
	t.Run("fable primary", func(t *testing.T) {
		cfg := policyTestConfig()
		cfg.CursorFable.Model = "claude-fable-5-max"
		if err := validateCursor(cfg); err == nil {
			t.Fatal("Fable must use the exact thinking-max Cursor model")
		}
	})
	t.Run("fable codex identity", func(t *testing.T) {
		cfg := policyTestConfig()
		cfg.CodexModel = "gpt-5.6-luna"
		if err := validateCursor(cfg); err == nil {
			t.Fatal("Fable B must freeze the exact global Sol model")
		}
	})
	t.Run("fable profile codex model is not a real override", func(t *testing.T) {
		cfg := policyTestConfig()
		prof := cfg.CrossProfiles["fable-dual"]
		prof.B.Model = "gpt-5.6-sol"
		cfg.CrossProfiles["fable-dual"] = prof
		if err := validateCursor(cfg); err == nil {
			t.Fatal("a no-op profile B.model must fail instead of displaying a model that execution rejects")
		}
	})
	t.Run("fable independent answer must be Sol ultra", func(t *testing.T) {
		cfg := policyTestConfig()
		prof := cfg.CrossProfiles["fable-dual"]
		prof.B.Effort = "max"
		cfg.CrossProfiles["fable-dual"] = prof
		if err := validateCursor(cfg); err == nil {
			t.Fatal("Fable B drift from Sol/ultra to Sol/max must be rejected")
		}
	})
	t.Run("fable merge must be Sol max", func(t *testing.T) {
		cfg := policyTestConfig()
		prof := cfg.CrossProfiles["fable-dual"]
		prof.Merge = &CrossEngine{Kind: cursorRunnerName, Model: "cursor-grok-4.6-xhigh"}
		cfg.CrossProfiles["fable-dual"] = prof
		if err := validateCursor(cfg); err == nil {
			t.Fatal("Fable merge drift back to Cursor Grok must be rejected")
		}
	})
	t.Run("fable Grok leg must be executable", func(t *testing.T) {
		cfg := policyTestConfig()
		cfg.GrokBuild.Enabled = false
		if err := validateCursor(cfg); err == nil {
			t.Fatal("Fable config must fail at load when its Grok answer leg is unavailable")
		}
	})
	t.Run("fable Sol leg must be executable", func(t *testing.T) {
		cfg := policyTestConfig()
		cfg.CodexBin = ""
		if err := validateCursor(cfg); err == nil {
			t.Fatal("Fable config must fail at load when its Sol answer leg is unavailable")
		}
	})
	for _, tc := range []struct {
		name string
		edit func(*Config)
	}{
		{"Kimi row disabled", func(cfg *Config) { cfg.KimiCLIOpus.Enabled = false }},
		{"Grok row disabled", func(cfg *Config) { cfg.GrokBuild.Enabled = false }},
		{"Cursor row disabled", func(cfg *Config) { cfg.CursorFable.Enabled = false }},
		{"Sonnet row removed", func(cfg *Config) { delete(cfg.GrokBuild.TierRoutes, "sonnet") }},
	} {
		t.Run("owner lock rejects "+tc.name, func(t *testing.T) {
			cfg := policyTestConfig()
			tc.edit(cfg)
			if err := validateOwnerRoutingPolicy(cfg); err == nil {
				t.Fatalf("valid-looking hot config drift %q must be rejected", tc.name)
			}
		})
	}
}

func TestBackendClassificationOwnerCategoriesBilingual(t *testing.T) {
	if !backendDevelopmentTask(&Task{Type: typeSequence, RouteClass: routeClassBackend, Title: "render a local button"}) {
		t.Fatal("explicit backend must be authoritative even when text has no compatibility marker")
	}
	tests := []struct{ name, text string }{
		{"service zh", "实现服务生命周期"}, {"service en", "implement the service lifecycle"},
		{"persistence zh", "修复持久化边界"}, {"persistence en", "repair the persistence boundary"},
		{"protocol zh", "升级协议握手"}, {"protocol en", "upgrade the protocol handshake"},
		{"database zh", "迁移数据库"}, {"database en", "migrate the database"},
		{"network execution zh", "实现网络执行与重试"}, {"network execution en", "implement network execution and retries"},
		{"identity credential zh", "收紧身份与凭据校验"}, {"identity credential en", "tighten identity and credential checks"},
		{"oauth identity", "repair OAuth token refresh"}, {"jwt identity", "validate JWT claims"},
		{"redis persistence", "migrate Redis persistence keys"},
		{"manifest launchd zh", "更新清单与 launchd 守护"}, {"manifest launchd en", "update the manifest and launchd agent"},
		{"control authority zh", "修复 Control 权限边界"}, {"control authority en", "repair the Control authority boundary"},
		{"live cutover zh", "执行在线切换"}, {"live cutover en", "implement the live cutover"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			task := &Task{Type: typeSequence, Title: tc.text}
			if !backendDevelopmentTask(task) {
				t.Fatalf("owner backend category was not inferred: %q", tc.text)
			}
			task.RouteClass = routeClassGeneral
			if backendDevelopmentTask(task) {
				t.Fatalf("explicit general must override inference: %q", tc.text)
			}
		})
	}
	if backendDevelopmentTask(&Task{Type: typeReview, Title: "review database protocol"}) {
		t.Fatal("text inference must be limited to eligible implementation cards")
	}
}

func TestOwnerEnforcementRequiresRouteClassOnNewSequenceCards(t *testing.T) {
	cfg := policyTestConfig()
	if err := validateNewTaskRouteClass(cfg, &Task{Type: typeSequence}); err == nil {
		t.Fatal("new sequence cards must not enter Owner routing without an explicit backend/general classification")
	}
	for _, class := range []string{routeClassBackend, routeClassGeneral} {
		if err := validateNewTaskRouteClass(cfg, &Task{Type: typeSequence, RouteClass: class}); err != nil {
			t.Fatalf("explicit route class %q rejected: %v", class, err)
		}
	}
	if err := validateNewTaskRouteClass(cfg, &Task{Type: typeSequence, RouteClass: "banana"}); err == nil {
		t.Fatal("unknown explicit route_class must be rejected instead of falling into text inference")
	}
	if backendDevelopmentTask(&Task{Type: typeSequence, RouteClass: "banana", Title: "database backend"}) {
		t.Fatal("unknown explicit route_class must not be reinterpreted from prompt text")
	}
	invalidPersisted := &Task{Type: typeSequence, Model: "opus", PreferRunner: "codex", FreshSteps: true,
		Prompts: []string{"p"}, RouteClass: "banana"}
	if _, ok := resolveOwnerRoute(cfg, invalidPersisted); ok {
		t.Fatal("a manually edited persisted route_class must not enter any owner model lane")
	}
	if _, ok := resolvePinnedTaskLeg(cfg, invalidPersisted); ok {
		t.Fatal("invalid persisted route_class must not fall through to the default Codex pin")
	}
	if reason := ownerRoutingPolicyWaitReason(cfg, invalidPersisted); !strings.Contains(reason, "route_class") {
		t.Fatalf("invalid persisted route_class must expose an auditable policy wait, got %q", reason)
	}
	if err := validateNewTaskRouteClass(cfg, &Task{Type: typeReview}); err != nil {
		t.Fatalf("non-sequence cards do not require backend implementation classification: %v", err)
	}
	cfg.OwnerRoutingEnforced = false
	if err := validateNewTaskRouteClass(cfg, &Task{Type: typeSequence}); err != nil {
		t.Fatalf("generic Cardex mode must retain legacy compatibility: %v", err)
	}
}

func TestSafeFallbackKindsFollowOnlyResolvedSerialLegsAndNeverGlobalCodex(t *testing.T) {
	cfg := policyTestConfig()
	kinds := []fallbackFailureKind{
		fallbackQuota, fallbackTransport, fallbackStreamIncomplete, fallbackSemanticStall, fallbackInvalidTerminal,
		fallbackExecutionEnv,
	}
	for _, kind := range kinds {
		t.Run(string(kind), func(t *testing.T) {
			proof := safeFixtureProof(t)
			auth, err := authorizePolicyFallback(proof)
			if err != nil {
				t.Fatal(err)
			}

			general := &Task{Type: typeSequence, Model: "opus", RouteClass: routeClassGeneral,
				PreferRunner: "codex", FreshSteps: true, Prompts: []string{"p"}}
			generalRoute, ok := resolveOwnerRoute(cfg, general)
			if !ok || !pinOwnerPrimaryRoute(general, generalRoute) {
				t.Fatal("failed to freeze Grok owner primary")
			}
			general.Runner = grokBuildRunnerName
			if err := queuePolicyFallback(cfg, general, kind, auth); err != nil {
				t.Fatal(err)
			}
			if general.PreferRunner != kimiCLIRunnerName || general.KimiModel != "kimi-code/k3" ||
				general.Effort != "max" || general.RouteReason != routeReasonGrokToKimiPending {
				t.Fatalf("Grok safe failure did not queue Kimi: %+v", general)
			}

			general.Runner = kimiCLIRunnerName
			general.RouteReason = routeReasonGrokToKimi
			if err := queuePolicyFallback(cfg, general, kind, auth); err == nil {
				t.Fatal("Kimi failure must stop: the final non-backend row has no global Codex leg")
			}

			for _, tier := range []struct {
				name, model, routeClass, riskClass, routeReason string
				wantKimi                                        bool
			}{
				{"backend opus missing risk", "opus", routeClassBackend, "", routeReasonGrokOpusBackend, false},
				{"sonnet", "sonnet", routeClassGeneral, riskClassOrdinary, routeReasonGrokSonnet, true},
				{"haiku", "haiku", routeClassGeneral, riskClassOrdinary, routeReasonGrokHaiku, true},
			} {
				grok := &Task{Type: typeSequence, Model: tier.model, RouteClass: tier.routeClass,
					RiskClass: tier.riskClass, PreferRunner: "codex", FreshSteps: true, Prompts: []string{"p"}}
				grokRoute, ok := resolveOwnerRoute(cfg, grok)
				if !ok || !pinOwnerPrimaryRoute(grok, grokRoute) {
					t.Fatalf("%s: failed to freeze Grok owner primary", tier.name)
				}
				grok.Runner = grokBuildRunnerName
				grok.RouteReason = tier.routeReason
				err := queuePolicyFallback(cfg, grok, kind, auth)
				if !tier.wantKimi {
					if err == nil {
						t.Fatalf("%s must hold because review/release legs are not fallback writers", tier.name)
					}
					continue
				}
				if err != nil {
					t.Fatalf("%s: %v", tier.name, err)
				}
				if grok.PreferRunner != kimiCLIRunnerName || grok.KimiModel != "kimi-code/k3" ||
					grok.Effort != "max" || grok.RouteReason != routeReasonGrokToKimiPending {
					t.Fatalf("%s resolved fallback mismatch: %+v", tier.name, grok)
				}
			}
		})
	}
}

func TestSafeFallbackProofBlocksEveryUnsafeSignal(t *testing.T) {
	base := safeFixtureProof(t)
	tests := []struct {
		name string
		edit func(*policyFallbackProof)
	}{
		{"semantic event", func(p *policyFallbackProof) { p.SemanticEvents = 1 }},
		{"model event", func(p *policyFallbackProof) { p.ModelEvents = 1 }},
		{"tool event", func(p *policyFallbackProof) { p.ToolEvents = 1 }},
		{"changed existing dirty file", func(p *policyFallbackProof) { p.After.Digest = "changed" }},
		{"new untracked file", func(p *policyFallbackProof) { p.After.Digest = "new-untracked" }},
		{"surviving process residue", func(p *policyFallbackProof) { p.ProcessResidue = true }},
		{"unproven stream", func(p *policyFallbackProof) { p.ObservationComplete = false }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			probe := base
			before, after := *base.Before, *base.After
			probe.Before, probe.After = &before, &after
			tc.edit(&probe)
			if _, err := authorizePolicyFallback(probe); err == nil {
				t.Fatal("unsafe proof authorized fallback")
			}
		})
	}
	if err := queuePolicyFallback(policyTestConfig(), &Task{}, fallbackQuota, fallbackAuthorization{}); err == nil {
		t.Fatal("a next writer must not be queued without a completed proof authorization")
	}
}

func TestWorkspaceFingerprintIncludesDirtyAndUntrackedBytes(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	tracked := filepath.Join(dir, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "tracked.txt", ".gitignore")
	runGit(t, dir, "commit", "-m", "base")
	if err := os.WriteFile(tracked, []byte("already dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	untracked := filepath.Join(dir, "untracked.txt")
	if err := os.WriteFile(untracked, []byte("existing untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	first, err := capturePolicyWorkspaceFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "tracked.txt")
	staged, err := capturePolicyWorkspaceFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest == staged.Digest {
		t.Fatal("staging an already-dirty file must change the repository fingerprint")
	}
	runGit(t, dir, "reset", "HEAD", "--", "tracked.txt")
	if err := os.WriteFile(tracked, []byte("changed dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := capturePolicyWorkspaceFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest == second.Digest {
		t.Fatal("changing an already-dirty tracked file must change the fingerprint")
	}
	if _, err := authorizePolicyFallback(policyFallbackProof{Before: first, After: second, ObservationComplete: true}); err == nil {
		t.Fatal("changed existing-dirty bytes must block fallback authorization")
	}
	if err := os.WriteFile(tracked, []byte("already dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	third, err := capturePolicyWorkspaceFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest == third.Digest {
		t.Fatal("a new untracked file must change the fingerprint")
	}
	if _, err := authorizePolicyFallback(policyFallbackProof{Before: first, After: third, ObservationComplete: true}); err == nil {
		t.Fatal("a new untracked file must block fallback authorization")
	}
	if err := os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("ignored product output\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fourth, err := capturePolicyWorkspaceFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if third.Digest == fourth.Digest {
		t.Fatal("a new ignored-untracked file must change the fingerprint")
	}
	if err := os.Mkdir(filepath.Join(dir, "empty-product-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	fifth, err := capturePolicyWorkspaceFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fourth.Digest == fifth.Digest {
		t.Fatal("a new empty directory is workspace residue and must change the fingerprint")
	}
}

func TestWorkspaceFingerprintSupportsUnbornGitRepository(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("staged before first commit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "staged.txt")
	if _, err := capturePolicyWorkspaceFingerprint(dir); err != nil {
		t.Fatalf("unborn repositories must still produce a fail-closed comparable fingerprint: %v", err)
	}
}

func TestWorkspaceFingerprintIncludesExactGitIndexFlags(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	path := filepath.Join(dir, "tracked.txt")
	if err := os.WriteFile(path, []byte("stable\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "tracked.txt")
	runGit(t, dir, "commit", "-m", "base")
	before, err := capturePolicyWorkspaceFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "update-index", "--skip-worktree", "tracked.txt")
	after, err := capturePolicyWorkspaceFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before.Digest == after.Digest {
		t.Fatal("skip-worktree changes exact index bytes and must invalidate fallback authorization")
	}
	if _, err := authorizePolicyFallback(policyFallbackProof{Before: before, After: after, ObservationComplete: true}); err == nil {
		t.Fatal("changed Git index flags must block fallback")
	}
}

func TestWorkspaceFingerprintSeesSkipWorktreeContentChanges(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	path := filepath.Join(dir, "tracked.txt")
	if err := os.WriteFile(path, []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "tracked.txt")
	runGit(t, dir, "commit", "-m", "base")
	runGit(t, dir, "update-index", "--skip-worktree", "tracked.txt")
	before, err := capturePolicyWorkspaceFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("hidden product mutation\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := capturePolicyWorkspaceFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before.Digest == after.Digest {
		t.Fatal("actual bytes under skip-worktree must still invalidate fallback authorization")
	}
}

func TestWorkspaceFingerprintIncludesSymbolicBranchIdentity(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "cardex-test@example.invalid")
	runGit(t, dir, "config", "user.name", "Cardex Test")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("same commit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "tracked.txt")
	runGit(t, dir, "commit", "-m", "base")
	runGit(t, dir, "branch", "same-commit")
	before, err := capturePolicyWorkspaceFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "switch", "same-commit")
	after, err := capturePolicyWorkspaceFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before.Digest == after.Digest {
		t.Fatal("switching symbolic branch at the same commit must invalidate fallback authorization")
	}
}

func TestBackendOpusSolFallbackStillCreatesSeparateSolMaxReview(t *testing.T) {
	root := testRoot(t)
	cfg := policyTestConfig()
	parent := newTask(root, cfg, typeSequence, "backend protocol implementation", t.TempDir(), []string{"implement"}, 9)
	parent.Status = statusDone
	parent.Model = "opus"
	parent.RouteClass = routeClassBackend
	parent.Runner = "codex"
	parent.GrokModel = "grok-4.6" // records that the implementation entered and safely left the Grok leg
	parent.GrokEffort = "xhigh"
	parent.CodexModel = "gpt-5.6-sol"
	parent.Effort = "max"
	parent.ReviewAfter = true
	parent.SolMaxAdversarialReview = true
	if err := saveTask(root, parent); err != nil {
		t.Fatal(err)
	}
	postComplete(root, cfg, parent, &claudeResult{Result: "implemented by Sol"}, nil)

	all, err := loadBoardTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	var review *Task
	for _, task := range all {
		if task.ReviewOf == parent.ID {
			review = task
		}
	}
	if review == nil {
		t.Fatal("backend Opus fallback completion did not create a review")
	}
	if review.ID == parent.ID || review.SessionID != "" || review.PreferRunner != "codex" ||
		review.CodexModel != "gpt-5.6-sol" || review.Effort != "max" || review.ReviewAfter {
		t.Fatalf("review must be a new clean non-recursive Sol/max session: %+v", review)
	}
}

func ptrPolicyLeg(v policyLeg) *policyLeg { return &v }

func safeFixtureProof(t *testing.T) policyFallbackProof {
	t.Helper()
	dir := t.TempDir()
	before, err := capturePolicyWorkspaceFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	after, err := capturePolicyWorkspaceFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	return policyFallbackProof{
		Before: before, After: after, ObservationComplete: true,
	}
}

func TestFallbackFailureClassifierCoversRequiredTerminalShapes(t *testing.T) {
	tests := []struct {
		name, via, combined string
		res                 *claudeResult
		err                 error
		want                fallbackFailureKind
	}{
		{"quota", kimiCLIRunnerName, `{"role":"error","type":"error","content":"quota exceeded"}`, &claudeResult{IsError: true, Result: "quota exceeded"}, errors.New("quota"), fallbackQuota},
		{"transport", kimiCLIRunnerName, "connection reset by peer", &claudeResult{IsError: true}, errors.New("connection reset by peer"), fallbackTransport},
		{"dns transport", grokBuildRunnerName, "dial tcp: lookup api.example: no such host", &claudeResult{IsError: true}, errors.New("exit status 1"), fallbackTransport},
		{"node dns transport", grokBuildRunnerName, "getaddrinfo ENOTFOUND api.example", &claudeResult{IsError: true}, errors.New("exit status 1"), fallbackTransport},
		{"stream incomplete", grokBuildRunnerName, "", &claudeResult{IsError: true, Subtype: "grok_build_stream_incomplete"}, errors.New("stream incomplete"), fallbackStreamIncomplete},
		{"semantic stall", grokBuildRunnerName, "", &claudeResult{IsError: true}, errors.New("semantic stall timeout"), fallbackSemanticStall},
		{"invalid terminal", grokBuildRunnerName, "", &claudeResult{IsError: true, Subtype: "grok_build_invalid_terminal"}, errors.New("invalid terminal"), fallbackInvalidTerminal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := classifyPolicyFallbackFailure(tc.via, tc.res, tc.combined, tc.err); !ok || got != tc.want {
				t.Fatalf("got (%q,%v), want (%q,true)", got, ok, tc.want)
			}
		})
	}
	if _, ok := classifyPolicyFallbackFailure(kimiCLIRunnerName, &claudeResult{IsError: true}, "permission denied", errors.New("permission denied")); ok {
		t.Fatal("unlisted failures must retain the existing held/retry policy, not auto-fallback")
	}
	if _, ok := classifyPolicyFallbackFailure(grokBuildRunnerName, &claudeResult{IsError: true},
		"HTTP 401: Unauthorized; connection reset by peer", errors.New("exit status 1")); ok {
		t.Fatal("HTTP 401 authentication terminal must veto a coincident transport phrase")
	}
	if _, ok := classifyPolicyFallbackFailure(grokBuildRunnerName, &claudeResult{IsError: true},
		"HTTP status 401; connection reset by peer", errors.New("exit status 1")); ok {
		t.Fatal("a bare HTTP status 401 authentication terminal must veto transport fallback")
	}
	echoedPrompt := `{"role":"user","type":"message","content":"diagnose this network error"}`
	if _, ok := classifyPolicyFallbackFailure(kimiCLIRunnerName,
		&claudeResult{IsError: true, Result: "permission denied"}, echoedPrompt, errors.New("permission denied")); ok {
		t.Fatal("echoed prompt prose must not turn an unrelated failure into a transport fallback")
	}
	echoedStderr := "please diagnose network error\npermission denied"
	if _, ok := classifyPolicyFallbackFailure(kimiCLIRunnerName,
		&claudeResult{IsError: true, Result: "permission denied"}, echoedStderr, errors.New("exit status 1")); ok {
		t.Fatal("an unsafe permission terminal must dominate echoed transport prose")
	}
}

func TestStructuredSemanticEventOnStderrBlocksFallbackProof(t *testing.T) {
	stdout := `{"role":"meta","type":"system.version","version":"0.36.1"}`
	stderr := `{"role":"assistant","type":"message","content":"started work"}` + "\nconnection reset by peer"
	res := parseKimiCLIJSONL(providerJSONObservation(kimiCLIRunnerName, stdout, stderr))
	if res.SemanticEvents == 0 || res.ModelEvents == 0 || !res.ObservationComplete {
		t.Fatalf("structured stderr assistant event must be observed completely: %+v", res)
	}
	proof := safeFixtureProof(t)
	proof.SemanticEvents = res.SemanticEvents
	proof.ModelEvents = res.ModelEvents
	proof.ToolEvents = res.ToolEvents
	proof.ObservationComplete = res.ObservationComplete
	if _, err := authorizePolicyFallback(proof); err == nil {
		t.Fatal("semantic/model work on stderr must block a second writer")
	}
}

func TestMalformedJSONLookingStderrMakesObservationIncomplete(t *testing.T) {
	observed := providerJSONObservation(kimiCLIRunnerName, `{"role":"meta","type":"system.version","version":"0.36.1"}`,
		`{"role":"assistant","content":"truncated"`)
	res := parseKimiCLIJSONL(observed)
	if res.ObservationComplete {
		t.Fatalf("a truncated structured stderr event must fail closed: observation=%q result=%+v", observed, res)
	}
}

func TestUnknownPlainStderrMakesObservationIncomplete(t *testing.T) {
	stdout := `{"role":"meta","type":"system.version","version":"0.36.1"}`
	observed := providerJSONObservation(kimiCLIRunnerName, stdout, "ordinary prose that may be semantic work")
	res := parseKimiCLIJSONL(observed)
	if res.ObservationComplete {
		t.Fatal("unrecognized plain stderr must fail closed as an incomplete semantic/tool observation")
	}
	known := parseKimiCLIJSONL(providerJSONObservation(kimiCLIRunnerName, stdout, "connection reset by peer"))
	if !known.ObservationComplete {
		t.Fatal("a narrowly recognized presemantic transport diagnostic should remain fully observable")
	}
	mixed := parseKimiCLIJSONL(providerJSONObservation(kimiCLIRunnerName, stdout,
		"connection reset by peer; analysis: repository edits may already exist"))
	if mixed.ObservationComplete {
		t.Fatal("a transport substring sharing a plain-stderr line with semantic prose must fail closed")
	}
	mixedGrok := parseGrokBuildJSONL(providerJSONObservation(grokBuildRunnerName, "",
		"analysis: Grok session directory is read-only"))
	if mixedGrok.ObservationComplete {
		t.Fatal("a Grok read-only substring with a semantic prefix must fail closed")
	}
	mixedGrokPath := parseGrokBuildJSONL(providerJSONObservation(grokBuildRunnerName, "",
		"assistant analysis completed, /home/u/.grok/session.db: readonly database"))
	if mixedGrokPath.ObservationComplete {
		t.Fatal("a .grok path preceded by semantic prose must fail closed")
	}
}

func TestWorkspaceFingerprintFramingDistinguishesFormerConcatenationCollision(t *testing.T) {
	left := t.TempDir()
	right := t.TempDir()
	// The previous raw concatenation encoded these two trees identically:
	// header(a)+"b\\0file\\00644\\0X" versus header(a)+header(b)+"X".
	if err := os.WriteFile(filepath.Join(left, "a"), []byte("b\x00file\x000644\x00X"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(right, "a"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(right, "b"), []byte("X"), 0o644); err != nil {
		t.Fatal(err)
	}
	leftFP, err := capturePolicyWorkspaceFingerprint(left)
	if err != nil {
		t.Fatal(err)
	}
	rightFP, err := capturePolicyWorkspaceFingerprint(right)
	if err != nil {
		t.Fatal(err)
	}
	if leftFP.Digest == rightFP.Digest {
		t.Fatal("length-framed workspace fingerprints must distinguish entry boundaries")
	}
}

func TestUnsupportedFableManualReadbackCannotFallThroughToCodexPin(t *testing.T) {
	cfg := policyTestConfig()
	task := &Task{ID: "manual-fable-wait", Type: typeSequence, Model: "fable", PreferRunner: "codex",
		FreshSteps: true, Prompts: []string{"one", "two"}, RouteClass: routeClassGeneral}
	if _, _, _, ok := ownerManualDispatchCommand(cfg, task, task.Prompts[0]); ok {
		t.Fatal("unsupported Fable shape must not resolve an owner manual command")
	}
	if _, ok := resolvePinnedTaskLeg(cfg, task); ok {
		t.Fatal("unsupported Fable shape must not resolve a generic Codex manual pin")
	}
	if reason := ownerRoutingPolicyWaitReason(cfg, task); !strings.Contains(reason, "Fable") {
		t.Fatalf("unsupported Fable shape must retain a policy-wait reason, got %q", reason)
	}
}

func TestInvalidPersistedRouteClassBlocksRemoteManualCommand(t *testing.T) {
	root := testRoot(t)
	cfg := policyTestConfig()
	cfg.RemoteHosts = map[string]RemoteHostConfig{"review-host": {CodexBin: "codex", CodexOnly: true}}
	if err := saveConfig(root, cfg); err != nil {
		t.Fatal(err)
	}
	task := newTask(root, cfg, typeSequence, "invalid remote route", t.TempDir(), []string{"p"}, 1)
	task.RouteClass = "banana"
	task.RemoteHost = "review-host"
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	err := cmdCmd([]string{"-root", root, task.ID})
	if err == nil || !strings.Contains(err.Error(), "route_class") {
		t.Fatalf("invalid persisted route_class must block before remote manual dispatch: %v", err)
	}
}

func TestGrokReadOnlySessionStoreIsPresemanticExecutionEnvironmentFallback(t *testing.T) {
	cases := []string{
		"EROFS: read-only file system, mkdir '/tmp/fake-home/.grok/sessions/run-1'",
		"$GROK_HOME/session.db: readonly database",
		"Grok session directory is read-only",
	}
	for _, diagnostic := range cases {
		res := &claudeResult{
			Type:                "result",
			IsError:             true,
			Subtype:             "grok_build_process_error",
			ObservationComplete: true,
		}
		kind, ok := classifyPolicyFallbackFailure(grokBuildRunnerName, res, diagnostic, errors.New("exit status 1"))
		if !ok || kind != fallbackExecutionEnv {
			t.Fatalf("Grok session-store read-only failure should enter one serial execution-environment fallback: kind=%q ok=%v diagnostic=%q", kind, ok, diagnostic)
		}
	}

	// The acceptance is intentionally not a blanket permission bypass: unrelated filesystem or
	// policy denials remain non-fallback terminals.
	for _, diagnostic := range []string{
		"permission denied writing /srv/product/config.json",
		"EROFS: read-only file system, mkdir '/srv/product/output'",
		"Error: attempt to write a readonly database",
		"EROFS: read-only file system, open '/srv/product/database.sqlite'",
		"permission denied; EROFS: read-only file system, mkdir '/tmp/fake-home/.grok/sessions/run-1'",
		"HTTP 401: Unauthorized; EROFS: read-only file system, mkdir '/tmp/fake-home/.grok/sessions/run-1'",
		"403 forbidden: access denied by organization",
	} {
		res := &claudeResult{Type: "result", IsError: true, Subtype: "grok_build_process_error", ObservationComplete: true}
		if kind, ok := classifyPolicyFallbackFailure(grokBuildRunnerName, res, diagnostic, errors.New("exit status 1")); ok {
			t.Fatalf("unrelated permission/product filesystem terminal must not authorize fallback: kind=%q diagnostic=%q", kind, diagnostic)
		}
	}
}

func TestMandatorySolReviewObligationReconcilesWithoutRerunningImplementation(t *testing.T) {
	root := testRoot(t)
	cfg := policyTestConfig()
	parent := newTask(root, cfg, typeSequence, "Opus backend implementation", t.TempDir(), []string{"implement"}, 1)
	parent.Model = "opus"
	parent.RouteClass = routeClassBackend
	parent.Step = len(parent.Prompts)
	parent.Status = statusHeld
	parent.ReviewAfter = true
	parent.SolMaxAdversarialReview = true
	parent.ReviewObligationPending = true
	if err := saveTask(root, parent); err != nil {
		t.Fatal(err)
	}
	reconcileMandatoryReviewObligations(root, cfg, []*Task{parent}, map[string]bool{})
	got, err := loadTask(root, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusDone || got.ReviewObligationPending || got.ReviewTaskID == "" || got.Step != len(got.Prompts) {
		t.Fatalf("mandatory obligation did not reconcile to done without another model step: %+v", got)
	}
	review, err := findTaskAnywhere(root, got.ReviewTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if review.ReviewOf != parent.ID || review.PreferRunner != "codex" || review.CodexModel != "gpt-5.6-sol" ||
		review.Effort != "max" || review.SessionID != "" || review.ReviewAfter {
		t.Fatalf("mandatory reviewer identity drifted: %+v", review)
	}
	// Reconciliation is idempotent by ReviewOf and cannot create a second reviewer.
	reconcileMandatoryReviewObligations(root, cfg, []*Task{got}, map[string]bool{})
	children := 0
	for _, task := range listQueued(t, root) {
		if task.ReviewOf == parent.ID {
			children++
		}
	}
	if children != 1 {
		t.Fatalf("expected exactly one mandatory reviewer, got %d", children)
	}
}

func TestOwnerRoutingProcessContractRejectsConfigOptOut(t *testing.T) {
	root := testRoot(t)
	cfg := defaultConfig("claude")
	cfg.OwnerRoutingEnforced = false
	if err := saveConfig(root, cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ownerRoutingRequireEnv, "1")
	if _, err := loadConfig(root); err == nil || !strings.Contains(err.Error(), ownerRoutingRequireEnv) {
		t.Fatalf("process startup contract must reject removal of owner_routing_enforced: %v", err)
	}
}

func TestCursorErrorTerminalTextIsNotCountedAsModelWork(t *testing.T) {
	res := parseCursorJSONL(`{"type":"result","subtype":"error","is_error":true,"result":"connection reset by peer"}`)
	if !res.IsError || res.SemanticEvents != 0 || res.ModelEvents != 0 {
		t.Fatalf("error-terminal text is a diagnostic, not model work: %+v", res)
	}
}

func TestPolicyFallbackProofReasonsAreAuditable(t *testing.T) {
	proof := safeFixtureProof(t)
	proof.SemanticEvents = 1
	_, err := authorizePolicyFallback(proof)
	if err == nil || !strings.Contains(err.Error(), "semantic") {
		t.Fatalf("blocked proof should name the failed condition: %v", err)
	}
}

func TestNextWriterQueuesOnlyAfterCompletedProcessResidueCheck(t *testing.T) {
	cfg := policyTestConfig()
	task := &Task{Type: typeSequence, Model: "opus", RouteClass: routeClassGeneral,
		PreferRunner: "codex", FreshSteps: true, Prompts: []string{"p"}}
	route, ok := resolveOwnerRoute(cfg, task)
	if !ok || !pinOwnerPrimaryRoute(task, route) {
		t.Fatal("failed to freeze Grok owner route")
	}
	task.Runner = grokBuildRunnerName
	if err := queuePolicyFallback(cfg, task, fallbackTransport, fallbackAuthorization{}); err == nil {
		t.Fatal("a next writer was queued before the post-invocation proof returned")
	}
	proof := safeFixtureProof(t)
	proof.ProcessResidue = true
	if _, err := authorizePolicyFallback(proof); err == nil {
		t.Fatal("a completed check reporting process residue must not mint authorization")
	}
	proof.ProcessResidue = false
	auth, err := authorizePolicyFallback(proof)
	if err != nil {
		t.Fatal(err)
	}
	if err := queuePolicyFallback(cfg, task, fallbackTransport, auth); err != nil {
		t.Fatalf("verified post-invocation proof should queue exactly the next leg: %v", err)
	}
	if task.PreferRunner != kimiCLIRunnerName {
		t.Fatalf("expected only the serial Kimi next leg, got %+v", task)
	}
}

func TestProcessResidueSurvivesAttemptResetUntilGroupIsDead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows policy fallback is deliberately fail-closed because descendant liveness is not provable")
	}
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "sleep 30")
	setupProcGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	finished := false
	t.Cleanup(func() {
		if finished {
			return
		}
		_ = killProcGroup(pgid)
		_ = cmd.Wait()
	})
	const taskID = "policy-residue-persists"
	markTaskProcessResidue(taskID, pgid)
	resetTaskProcessResidue(taskID)
	if !taskProcessResidue(taskID) {
		t.Fatal("starting another attempt must not erase a still-live writer process group")
	}
	if err := killProcGroup(pgid); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	finished = true
	resetTaskProcessResidue(taskID)
	if taskProcessResidue(taskID) {
		t.Fatal("a residue record may clear only after the OS proves its process group dead")
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
