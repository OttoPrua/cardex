package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- P1-1: closed Grok auth grammar rejects extra physical blank lines ----

func TestP1ClosedGrokAuthGrammarRejectsExtraPhysicalBlankLines(t *testing.T) {
	bare := grokBuildExactBareAuthDiagnostic
	for name, stderr := range map[string]string{
		"leading blank line":              "\n" + bare,
		"two leading blank lines":         "\n\n" + bare,
		"leading blank with newline tail": "\n" + bare + "\n",
		"extra trailing blank line":       bare + "\n\n",
		"blank sandwich":                  "\n" + bare + "\n\n",
		"blank then footer":               bare + "\n\nModel: grok-4.6",
		"whitespace-only leading line":    "   \n" + bare,
	} {
		if got := grokBuildAuthDiagnosticLine(stderr); got != "" {
			t.Fatalf("%s: extra physical blank lines must be rejected, got %q", name, got)
		}
	}
	// Both exact positives stay accepted: the bare one-line diagnostic (with or without the
	// process's single trailing newline) and the exact closed quoted OIDC wrapper.
	if got := grokBuildAuthDiagnosticLine(bare); got != bare {
		t.Fatalf("exact bare diagnostic must stay accepted, got %q", got)
	}
	if got := grokBuildAuthDiagnosticLine(bare + "\n"); got != bare {
		t.Fatalf("bare diagnostic with one trailing newline must stay accepted, got %q", got)
	}
	if got := grokBuildAuthDiagnosticLine(grokOIDCNoAuthContextClosedMetadata); got != grokBuildExactQuotedAuthBody {
		t.Fatalf("exact closed quoted wrapper must stay accepted, got %q", got)
	}
	// Non-exact multiline wrapper deviations are rejected.
	for name, stderr := range map[string]string{
		"wrapper leading blank":   "\n" + grokOIDCNoAuthContextClosedMetadata,
		"wrapper doubled blank":   strings.Replace(grokOIDCNoAuthContextClosedMetadata, ")\n\n", ")\n\n\n", 1),
		"wrapper missing blank":   strings.Replace(grokOIDCNoAuthContextClosedMetadata, ")\n\n", ")\n", 1),
		"wrapper trailing blanks": grokOIDCNoAuthContextClosedMetadata + "\n\n",
	} {
		if got := grokBuildAuthDiagnosticLine(stderr); got != "" {
			t.Fatalf("%s: non-exact multiline wrapper must be rejected, got %q", name, got)
		}
	}
}

func TestP1BlankLinePreflightDoesNotOpenCircuit(t *testing.T) {
	for name, stderr := range map[string]string{
		"leading blank":        "\n" + grokBuildExactBareAuthDiagnostic,
		"extra trailing blank": grokBuildExactBareAuthDiagnostic + "\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := testRoot(t)
			bin := fakeGrokBuildProbeFailure(t, stderr)
			cfg := policyTestConfig()
			cfg.GrokBuildBin = bin
			err := ensureGrokBuildAuth(context.Background(), root, cfg, "grok-4.6")
			if err == nil || isGrokBuildAuthProbeError(err) {
				t.Fatalf("blank-line form must stay a non-auth preflight failure: %T %v", err, err)
			}
			if cd := loadEngineCooldown(root, grokBuildCooldownName); grokBuildAuthCooldownActive(cd, time.Now()) {
				t.Fatalf("blank-line form must not open the Grok auth circuit: %+v", cd)
			}
		})
	}
}

func TestP1BlankLineModelPathStaysNonAuthIncompleteObservation(t *testing.T) {
	for name, stderr := range map[string]string{
		"leading blank":        "\n" + grokBuildExactBareAuthDiagnostic,
		"extra trailing blank": grokBuildExactBareAuthDiagnostic + "\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := testRoot(t)
			bin, _, _ := fakeGrokBuild(t, `{"type":"system.version","version":"1.0.5"}`, stderr, 1)
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
				t.Fatalf("blank-line stderr must remain an incomplete observation: %+v", got.LastRouteAttempt)
			}
			if got.LastRouteAttempt.FailureClass == string(failureAuth) || strings.HasPrefix(got.LastError, "[auth]") {
				t.Fatalf("blank-line stderr must not classify as authentication: %+v", got)
			}
			if cd := loadEngineCooldown(root, grokBuildCooldownName); grokBuildAuthCooldownActive(cd, time.Now()) {
				t.Fatalf("blank-line model stderr must not open the Grok auth circuit: %+v", cd)
			}
		})
	}
}

// ---- P1-2: mandatory high-risk categories defeat ordinary metadata ----

func TestP1MandatoryHighRiskCategoriesDefeatOrdinaryMetadata(t *testing.T) {
	cfg := policyTestConfig()
	fixtures := map[string]string{
		"identity/credential":        "轮换生产 OAuth 凭据，修复 token refresh 与密钥轮换",
		"db/schema/migration":        "执行 database schema migration 并回填数据表",
		"protocol/network execution": "实现 protocol handshake 与 network request 执行路径",
		"manifest/launchd":           "安装 launchd manifest 与启动清单",
		"control/authority":          "调整 control plane 的授权边界",
		"live cutover":               "执行生产切换 live cutover 方案",
		"security":                   "修复 security vulnerability 的注入面",
		"funds":                      "实现 payment 扣款与 settlement 结算",
	}
	for category, prompt := range fixtures {
		t.Run(category, func(t *testing.T) {
			task := &Task{ID: "p1-2-backend", Type: typeSequence, Model: "opus", RouteClass: routeClassBackend,
				RiskClass: riskClassOrdinary, PreferRunner: "codex", FreshSteps: true, Prompts: []string{prompt}}
			if got := effectiveOwnerRiskClass(task); got != riskClassHigh {
				t.Fatalf("%s must resolve high-risk despite ordinary metadata, got %q", category, got)
			}
			route, ok := resolveOwnerRoute(cfg, task)
			if !ok || route.Name != "opus_backend_high_risk" || route.RiskClass != riskClassHigh ||
				route.ReleaseGate == nil || route.ReleaseGate.Effort != "max" {
				t.Fatalf("%s must take the high-risk backend route with the mandatory Sol/max gate: ok=%v route=%+v", category, ok, route)
			}
		})
	}
	// The same categories on a general-route card also resolve high-risk; the route row itself
	// is unchanged apart from the added conditional Sol/xhigh release gate.
	general := &Task{ID: "p1-2-general", Type: typeSequence, Model: "opus", RouteClass: routeClassGeneral,
		RiskClass: riskClassOrdinary, PreferRunner: "codex", FreshSteps: true, Prompts: []string{"修复 security vulnerability 的注入面"}}
	if got := effectiveOwnerRiskClass(general); got != riskClassHigh {
		t.Fatalf("general-route security work must resolve high-risk, got %q", got)
	}
	route, ok := resolveOwnerRoute(cfg, general)
	if !ok || route.Name != "opus_non_backend" || route.ReleaseGate == nil || route.ReleaseGate.Effort != "xhigh" {
		t.Fatalf("general-route security work must add the conditional Sol/xhigh gate: ok=%v route=%+v", ok, route)
	}
	// Regression: a category-free ordinary backend card keeps the ordinary route.
	plain := &Task{ID: "p1-2-plain", Type: typeSequence, Model: "opus", RouteClass: routeClassBackend,
		RiskClass: riskClassOrdinary, PreferRunner: "codex", FreshSteps: true, Prompts: []string{"implement"}}
	for deterministicSolSample(plain.ID, 20) {
		plain.ID += "x"
	}
	route, ok = resolveOwnerRoute(cfg, plain)
	if !ok || route.Name != "opus_backend_ordinary" || route.ReleaseGate != nil {
		t.Fatalf("category-free ordinary backend must keep its route: ok=%v route=%+v", ok, route)
	}
}

// ---- P1-3: one automatic Sol per Fable lineage, durable across replay/crash ----

func p1FableAnswerCard(t *testing.T, root string, cfg *Config) *Task {
	t.Helper()
	task := newTask(root, cfg, typeCoordinate, "hard decision", t.TempDir(), []string{"decide from first principles"}, 9)
	task.Model = "fable"
	task.PreferRunner = "codex"
	if err := prepareCursorFableFallback(root, cfg, task, "cursor quota exhausted", fallbackQuota, verifiedCursorFallbackAuth(t)); err != nil {
		t.Fatal(err)
	}
	return task
}

func p1CountFableMergers(t *testing.T, root, xkey string) int {
	t.Helper()
	n := 0
	for _, dir := range []string{tasksDir(root), archiveDir(root)} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			var candidate Task
			if err := json.Unmarshal(data, &candidate); err != nil {
				t.Fatal(err)
			}
			if candidate.XKey == xkey && candidate.XRole == "C" {
				n++
			}
		}
	}
	return n
}

func TestP1FableAnswerReplayCreatesAtMostOneSolMerger(t *testing.T) {
	root := testRoot(t)
	cfg := cursorTestConfig()
	task := p1FableAnswerCard(t, root, cfg)
	handleCrossStage(root, cfg, task, &claudeResult{Result: "Grok answer"}, nil)
	if task.AutomaticSolCalls != 1 {
		t.Fatalf("the answer card itself must durably record the lineage Sol reservation: %+v", task)
	}
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	// Replay the same completed A leg (crash/reconcile re-entry): no second merger may appear and
	// the idempotent path must not break the already-complete chain.
	replayed, err := loadTask(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	replayed.Status = statusDone
	handleCrossStage(root, cfg, replayed, &claudeResult{Result: "Grok answer"}, nil)
	if replayed.Status != statusDone {
		t.Fatalf("idempotent replay must not fail the completed chain: %+v", replayed)
	}
	if err := saveTask(root, replayed); err != nil {
		t.Fatal(err)
	}
	if n := p1CountFableMergers(t, root, task.XKey); n != 1 {
		t.Fatalf("replayed completion must leave exactly one Sol/ultra merger, got %d", n)
	}
}

func TestP1FableCrashBeforeAnswerSaveDoesNotDuplicateMerger(t *testing.T) {
	root := testRoot(t)
	cfg := cursorTestConfig()
	task := p1FableAnswerCard(t, root, cfg)
	handleCrossStage(root, cfg, task, &claudeResult{Result: "Grok answer"}, nil)
	// Crash window: the merger child is durable but the answer card's post-completion save never
	// landed. The on-disk A still shows the pre-completion state (no reservation recorded).
	stale := *task
	stale.AutomaticSolCalls = 0
	stale.Status = statusQueued
	if err := saveTask(root, &stale); err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadTask(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	handleCrossStage(root, cfg, reloaded, &claudeResult{Result: "Grok answer again"}, nil)
	if reloaded.Status != statusQueued {
		t.Fatalf("existing merger must make the replay a no-op, not a chain break: %+v", reloaded)
	}
	if reloaded.AutomaticSolCalls != 1 {
		t.Fatalf("replay must backfill the durable lineage reservation on the answer card: %+v", reloaded)
	}
	if err := saveTask(root, reloaded); err != nil {
		t.Fatal(err)
	}
	if n := p1CountFableMergers(t, root, task.XKey); n != 1 {
		t.Fatalf("crash-window replay must leave exactly one Sol/ultra merger, got %d", n)
	}
}

// ---- P1-4: future-dated Codex usage evidence is invalid and fail-closed ----

func p1WriteCodexFeed(t *testing.T, path string, samples ...map[string]any) {
	t.Helper()
	var lines []byte
	for _, sample := range samples {
		line, err := json.Marshal(sample)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, append(line, '\n')...)
	}
	if err := os.WriteFile(path, lines, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestP1FutureCodexUsageSampleFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage-history.jsonl")
	now := time.Now().UTC()
	cfg := policyTestConfig()
	cfg.UsageFeed = path
	cfg.UsageFeedMaxAgeMin = 90

	// A 1%-used Codex sample dated 24h in the future must not satisfy the 65% automatic gate.
	p1WriteCodexFeed(t, path, map[string]any{
		"provider": "codex", "sampledAt": now.Add(24 * time.Hour).Format(time.RFC3339),
		"usedPercent": 1, "windowMinutes": 300, "windowKind": "primary",
	})
	evidence := currentAutomaticCodexBudgetEvidence(cfg, now)
	if evidence.Available {
		t.Fatalf("future-dated Codex sample must be invalid: %+v", evidence)
	}
	if ok, reason := automaticCodexBudgetAllowed(&Task{AutomaticCodex: true}, evidence, 65); ok {
		t.Fatalf("future-dated evidence must not authorize automatic Codex: %q", reason)
	}
	// A poisoned future newest line shadows the feed; the reader must not shop backward for an
	// older friendlier sample.
	p1WriteCodexFeed(t, path,
		map[string]any{
			"provider": "codex", "sampledAt": now.Add(-time.Minute).Format(time.RFC3339),
			"usedPercent": 10, "windowMinutes": 300, "windowKind": "primary",
		},
		map[string]any{
			"provider": "codex", "sampledAt": now.Add(time.Hour).Format(time.RFC3339),
			"usedPercent": 1, "windowMinutes": 300, "windowKind": "primary",
		})
	if evidence := currentAutomaticCodexBudgetEvidence(cfg, now); evidence.Available {
		t.Fatalf("poisoned newest sample must close the gate even with an older valid sample: %+v", evidence)
	}
	// Missing, stale, and non-Codex rejection is preserved.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if evidence := currentAutomaticCodexBudgetEvidence(cfg, now); evidence.Available {
		t.Fatalf("missing feed must stay unavailable: %+v", evidence)
	}
	p1WriteCodexFeed(t, path, map[string]any{
		"provider": "codex", "sampledAt": now.Add(-3 * time.Hour).Format(time.RFC3339),
		"usedPercent": 1, "windowMinutes": 300, "windowKind": "primary",
	})
	if evidence := currentAutomaticCodexBudgetEvidence(cfg, now); evidence.Available {
		t.Fatalf("stale Codex sample must stay rejected: %+v", evidence)
	}
	p1WriteCodexFeed(t, path, map[string]any{
		"provider": "claude", "sampledAt": now.Add(-time.Minute).Format(time.RFC3339),
		"usedPercent": 1, "windowMinutes": 300, "windowKind": "primary",
	})
	if evidence := currentAutomaticCodexBudgetEvidence(cfg, now); evidence.Available {
		t.Fatalf("non-Codex sample must never satisfy the Codex gate: %+v", evidence)
	}
	// A valid recent Codex sample still authorizes below the stop.
	p1WriteCodexFeed(t, path, map[string]any{
		"provider": "codex", "sampledAt": now.Add(-time.Second).Format(time.RFC3339),
		"usedPercent": 12, "windowMinutes": 300, "windowKind": "primary",
	})
	evidence = currentAutomaticCodexBudgetEvidence(cfg, now)
	if !evidence.Available || evidence.UsedPercent != 12 {
		t.Fatalf("valid recent Codex sample must remain usable: %+v", evidence)
	}
	if ok, _ := automaticCodexBudgetAllowed(&Task{AutomaticCodex: true}, evidence, 65); !ok {
		t.Fatal("valid recent evidence below the stop must still authorize")
	}
}

// ---- P1-5: reachable, durable Grok-Kimi disagreement producer ----

func p1OrdinaryBackendLineage(t *testing.T, root string, cfg *Config) (*Task, *Task) {
	t.Helper()
	impl := newTask(root, cfg, typeSequence, "ordinary bounded backend", t.TempDir(), []string{"implement bounded backend change"}, 5)
	impl.Model = "opus"
	impl.RouteClass = routeClassBackend
	impl.RiskClass = riskClassOrdinary
	impl.ID = "ordinary-disagreement"
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
	review.Status = statusDone
	return impl, review
}

func TestP1KimiContradictoryPassProducesDurableDisagreementEscalation(t *testing.T) {
	root := testRoot(t)
	cfg := policyTestConfig()
	impl, review := p1OrdinaryBackendLineage(t, root, cfg)
	// The closed review contract accepts pass only with empty p0/p1. A pass that still carries
	// blocking findings is the reviewer's durable disagreement with the implementation it
	// formally accepted; the lineage must escalate to its single conditional Sol/xhigh gate.
	contradictory := "```json\n{\"verdict\":\"pass\",\"p0\":[],\"p1\":[\"auth rollback evidence missing\"],\"p2\":[],\"summary\":\"passes but carries a blocking finding\"}\n```"
	postComplete(root, cfg, review, &claudeResult{Result: contradictory}, nil)
	rootAfter, err := findTaskAnywhere(root, impl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rootAfter.SolEscalationReason != solEscalationDisagreement {
		t.Fatalf("contradictory pass must durably record the Grok-Kimi disagreement: %+v", rootAfter)
	}
	if !reviewCompleted(rootAfter, reviewStageKimiAdversarial) ||
		pendingRequiredReviewStage(rootAfter) != reviewStageSolXHigh {
		t.Fatalf("disagreement must require the conditional Sol/xhigh gate: %+v", rootAfter)
	}
	if rootAfter.Status != statusHeld || rootAfter.AutomaticSolCalls != 1 {
		t.Fatalf("disagreement lineage must hold with exactly one reserved Sol call: %+v", rootAfter)
	}
	sol, err := existingPlannedReviewChild(root, impl.ID, reviewStageSolXHigh)
	if err != nil {
		t.Fatal(err)
	}
	if sol == nil || sol.PreferRunner != "codex" || sol.CodexModel != "gpt-5.6-sol" || sol.Effort != "xhigh" ||
		!sol.AutomaticCodex || sol.ReviewPlanRoot != impl.ID || sol.ReviewOf != impl.ID {
		t.Fatalf("disagreement must materialize the bound fresh Sol/xhigh gate: %+v", sol)
	}
	probe := &Task{ID: impl.ID, Type: typeSequence, Model: "opus", RouteClass: routeClassBackend,
		RiskClass: riskClassOrdinary, PreferRunner: "codex", FreshSteps: true, Prompts: impl.Prompts,
		SolEscalationReason: rootAfter.SolEscalationReason}
	route, ok := resolveOwnerRoute(cfg, probe)
	if !ok || route.ReleaseGate == nil || route.ReleaseGate.Effort != "xhigh" {
		t.Fatalf("resolver must agree the disagreement lineage has the Sol/xhigh gate: ok=%v route=%+v", ok, route)
	}
	events, _, err := loadTaskEvents(root, impl.ID)
	if err != nil || len(events) == 0 {
		t.Fatalf("disagreement must leave an event trail: events=%d err=%v", len(events), err)
	}
	found := false
	for _, ev := range events {
		if ev.Detail["reason"] == "grok_kimi_disagreement" && ev.Detail["review_child"] == review.ID &&
			ev.Detail["owner_route_name"] == impl.OwnerRouteName {
			found = true
		}
	}
	if !found {
		t.Fatalf("disagreement event must bind lineage, route state, and review child: %+v", events)
	}
}

func TestP1CleanPassAndUnknownVerdictNeverClaimDisagreement(t *testing.T) {
	t.Run("clean pass completes without escalation", func(t *testing.T) {
		root := testRoot(t)
		cfg := policyTestConfig()
		impl, review := p1OrdinaryBackendLineage(t, root, cfg)
		pass := "```json\n{\"verdict\":\"pass\",\"p0\":[],\"p1\":[],\"p2\":[],\"summary\":\"acceptance clean\"}\n```"
		postComplete(root, cfg, review, &claudeResult{Result: pass}, nil)
		rootAfter, err := findTaskAnywhere(root, impl.ID)
		if err != nil {
			t.Fatal(err)
		}
		if rootAfter.SolEscalationReason != "" || rootAfter.Status != statusDone {
			t.Fatalf("clean pass must complete without any escalation claim: %+v", rootAfter)
		}
	})
	t.Run("unknown terminal holds without escalation", func(t *testing.T) {
		root := testRoot(t)
		cfg := policyTestConfig()
		impl, review := p1OrdinaryBackendLineage(t, root, cfg)
		postComplete(root, cfg, review, &claudeResult{Result: "no structured verdict here"}, nil)
		rootAfter, err := findTaskAnywhere(root, impl.ID)
		if err != nil {
			t.Fatal(err)
		}
		if rootAfter.SolEscalationReason != "" || rootAfter.Status != statusHeld {
			t.Fatalf("unknown review terminal must hold without claiming disagreement: %+v", rootAfter)
		}
	})
}

// ---- P1-6: pre-existing task bytes survive every readback path unchanged ----

func TestP1LegacyTaskBytesSurviveReadbackAndPendingScan(t *testing.T) {
	root := testRoot(t)
	cfg := policyTestConfig()
	legacy := func(id, status string) string {
		return `{"id":"` + id + `","title":"legacy ` + status + `","type":"sequence","priority":5,"status":"` + status + `",` +
			`"dir":"/tmp/legacy","prompts":["legacy step"],"step":0,"model":"sonnet",` +
			`"created_at":"2026-01-01T00:00:00+08:00","updated_at":"2026-01-01T00:00:00+08:00"}` + "\n"
	}
	fixtures := map[string]string{
		filepath.Join(tasksDir(root), "t0101-0000-legq.json"):   legacy("t0101-0000-legq", statusQueued),
		filepath.Join(tasksDir(root), "t0101-0000-legh.json"):   legacy("t0101-0000-legh", statusHeld),
		filepath.Join(tasksDir(root), "t0101-0000-legl.json"):   legacy("t0101-0000-legl", statusLimitPaused),
		filepath.Join(tasksDir(root), "t0101-0000-legf.json"):   legacy("t0101-0000-legf", statusFailed),
		filepath.Join(tasksDir(root), "t0101-0000-legd.json"):   legacy("t0101-0000-legd", statusDone),
		filepath.Join(archiveDir(root), "t0101-0000-arc1.json"): legacy("t0101-0000-arc1", statusDone),
		filepath.Join(archiveDir(root), "t0101-0000-arc2.json"): legacy("t0101-0000-arc2", statusCanceled),
	}
	if err := os.MkdirAll(archiveDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	before := map[string]string{}
	for path, content := range fixtures {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(content))
		before[path] = hex.EncodeToString(sum[:])
	}
	// Exercise every readback/resolver path the scheduler and board expose to legacy cards.
	tasks, err := loadTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	board, err := loadBoardTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 5 || len(board) != 7 {
		t.Fatalf("fixture visibility mismatch: tasks=%d board=%d", len(tasks), len(board))
	}
	now := time.Now()
	for _, task := range board {
		_ = toBrief(cfg, task, now)
		_, _ = resolveOwnerRoute(cfg, task)
		_, _ = resolveOwnerRouteReadback(cfg, task)
		_ = ownerRoutingPolicyWaitReason(cfg, task)
		_ = effectiveOwnerRiskClass(task)
		_ = backendDevelopmentTask(task)
		if task.PreferRunner != "" {
			t.Fatalf("readback must not reinterpret a legacy card's runner identity: %+v", task)
		}
	}
	for path, want := range before {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != want {
			t.Fatalf("legacy task bytes changed at %s", path)
		}
	}
	// Source-level guard: the tick candidate loop must not rebake new defaults into pre-existing
	// pending cards; only the explicit creation path may stamp current defaults.
	data, err := os.ReadFile("tick.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "applyDefaultRunnerToPending") {
		t.Fatal("tick must not persist new runner defaults into pre-existing queued/held/limit-paused/failed cards")
	}
}
