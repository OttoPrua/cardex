package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fixtures below cover the five acceptance areas for workflow modes:
// review-result gates, write-domain conflicts, restart/replay idempotence,
// bounded loops, and durable manager hooks. Each RED case names the exact
// counter-injection that turns it red so the assertion cannot rot into a
// tautology.

// workflowGit runs one git command in the fixture worktree with a pinned
// identity, so freeze verification exercises the same object database checks
// production does.
func workflowGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir,
		"-c", "user.name=cardex-test", "-c", "user.email=cardex-test@example.invalid"}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// workflowHeadCandidate reads the real frozen identity of the worktree's HEAD.
func workflowHeadCandidate(t *testing.T, dir string) (commit, tree string) {
	t.Helper()
	return workflowGit(t, dir, "rev-parse", "HEAD"), workflowGit(t, dir, "rev-parse", "HEAD^{tree}")
}

func workflowTestRoot(t *testing.T) (root, dir string) {
	t.Helper()
	root = testRoot(t)
	if err := saveConfig(root, defaultConfig("claude")); err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "internal", "auth", "token.go"), "package auth\n")
	mustWriteFile(t, filepath.Join(dir, "internal", "billing", "bill.go"), "package billing\n")
	workflowGit(t, dir, "init", "-q")
	workflowGit(t, dir, "add", "-A")
	workflowGit(t, dir, "commit", "-q", "-m", "fixture")
	return root, dir
}

func workflowTestCfg(t *testing.T, root string) *Config {
	t.Helper()
	cfg, err := loadConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func initTestWorkflow(t *testing.T, root, dir string, extra ...string) *WorkflowRecord {
	t.Helper()
	args := append([]string{
		"-root", root, "-mode", workflowModeSerial, "-module", "auth",
		"-goal-id", "auth-token-v1", "-goal", "ship an independently reviewed auth token vertical",
		"-dir", dir,
		"-terminal-criteria", "independent review pass with empty p0/p1; integration and live stay held",
		"-write-domain-id", "auth-tokens", "-write-domain-lineage", "auth-tokens-lineage",
		"-write-domain-component", "auth", "-write-paths", "internal/auth",
		"-engine", grokBuildRunnerName,
		"-max-rounds", "2",
	}, extra...)
	if err := cmdWorkflowInit(args); err != nil {
		t.Fatalf("workflow init: %v", err)
	}
	cfg := workflowTestCfg(t, root)
	wfs, err := loadWorkflows(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, wf := range wfs {
		if wf.ModuleID == "auth" {
			return wf
		}
	}
	t.Fatalf("auth workflow not found among %d records", len(wfs))
	return nil
}

func writeReviewLog(t *testing.T, root, id, body string) {
	t.Helper()
	if err := os.MkdirAll(logsDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(taskLogPath(root, id), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func verdictJSON(verdict string, p0, p1 []string) string {
	if p0 == nil {
		p0 = []string{}
	}
	if p1 == nil {
		p1 = []string{}
	}
	blob, _ := json.Marshal(map[string]any{
		"verdict": verdict, "p0": p0, "p1": p1, "p2": []string{}, "summary": "fixture",
	})
	return "报告正文\n```json\n" + string(blob) + "\n```\n"
}

func markTaskDone(t *testing.T, root, id string) *Task {
	t.Helper()
	tk, err := loadTask(root, id)
	if err != nil {
		t.Fatal(err)
	}
	tk.Status = statusDone
	tk.touch()
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	return tk
}

// runWorkflowToReview drives one workflow from init through a terminated
// reviewer bound to the frozen candidate, then writes the given review body.
// The candidate is the worktree's real HEAD: freeze now verifies identities
// against the repository, so fabricated strings are refused by design.
func runWorkflowToReview(t *testing.T, root string, wf *WorkflowRecord, body string) (*WorkflowRecord, *Task) {
	t.Helper()
	cfg := workflowTestCfg(t, root)
	writer, err := admitWorkflowWriter(root, cfg, wf, "")
	if err != nil {
		t.Fatalf("admit writer: %v", err)
	}
	markTaskDone(t, root, writer.ID)
	commit, tree := workflowHeadCandidate(t, wf.Worktree)
	if err := freezeWorkflowCandidate(root, cfg, wf, WorkflowCandidate{Commit: commit, Tree: tree}); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	review, err := admitWorkflowReviewer(root, cfg, wf)
	if err != nil {
		t.Fatalf("admit reviewer: %v", err)
	}
	markTaskDone(t, root, review.ID)
	writeReviewLog(t, root, review.ID, body)
	fresh, err := loadWorkflow(root, cfg, wf.ID)
	if err != nil {
		t.Fatal(err)
	}
	return fresh, review
}

// ---- area 1: review-result gates ----

func TestWorkflowInitDefaultHoldsIntegrationAndAllEffectGates(t *testing.T) {
	root, dir := workflowTestRoot(t)
	wf := initTestWorkflow(t, root, dir)

	if wf.Mode != workflowModeSerial {
		t.Fatalf("mode=%s", wf.Mode)
	}
	if wf.EffectGates.Integration != effectGateHeld ||
		wf.EffectGates.Live != effectGateHeld ||
		wf.EffectGates.Cutover != effectGateHeld {
		t.Fatalf("all three gates must start held: %+v", wf.EffectGates)
	}
	integ, err := loadTask(root, wf.IntegrationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if integ.Status != statusHeld {
		t.Fatalf("integration card must be created held, got %s; 反例注入: createHeldIntegrationTask 里去掉 t.Status = statusHeld", integ.Status)
	}
	if integ.IntegrationGate == nil || integ.IntegrationGate.WorkflowID != wf.ID {
		t.Fatalf("integration card must carry its gate: %+v", integ.IntegrationGate)
	}
	if integ.WriteDomain == nil || integ.WriteDomain.ID != "auth-tokens" {
		t.Fatalf("integration card must claim the module write domain: %+v", integ.WriteDomain)
	}
}

func TestReviewResultGateHoldsEveryNonAdmissibleTerminal(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"missing-output", "", holdReasonMissingOutput},
		{"unknown-accept", verdictJSON("ACCEPT", nil, nil), holdReasonUnknownVocabulary},
		{"unknown-held", verdictJSON("HELD", nil, nil), holdReasonUnknownVocabulary},
		{"concerns", verdictJSON("concerns", nil, []string{"p1 finding"}), holdReasonConcerns},
		{"block", verdictJSON("block", []string{"p0 finding"}, nil), holdReasonBlock},
		{"pass-with-open-p0", verdictJSON("pass", []string{"still a p0"}, nil), holdReasonFindingsOpen},
		{"pass-with-open-p1", verdictJSON("pass", nil, []string{"still a p1"}), holdReasonFindingsOpen},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, dir := workflowTestRoot(t)
			cfg := workflowTestCfg(t, root)
			wf := initTestWorkflow(t, root, dir)
			wf, _ = runWorkflowToReview(t, root, wf, tc.body)

			integ, err := loadTask(root, wf.IntegrationTaskID)
			if err != nil {
				t.Fatal(err)
			}
			dec := evaluateIntegrationRelease(root, cfg, integ)
			if dec.Admit || dec.HoldReason != tc.want {
				t.Fatalf("admit=%v reason=%q want %q", dec.Admit, dec.HoldReason, tc.want)
			}
			if integrationGateAllows(root, cfg, integ) {
				t.Fatal("tick must not dispatch a non-admissible integration card")
			}
			if err := tryReleaseWorkflowIntegration(root, cfg, wf); !errors.Is(err, errWorkflowHeld) {
				t.Fatalf("try-release must refuse: %v", err)
			}
			if err := cmdSetStatus([]string{"-root", root, integ.ID}, "release"); err == nil {
				t.Fatal("cardex release must refuse a gated card without admissible evidence")
			}
			after, err := loadTask(root, integ.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Status != statusHeld {
				t.Fatalf("integration must remain held, got %s", after.Status)
			}
		})
	}
}

func TestAdmissiblePassReleasesIntegrationButNeverLive(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)
	wf, _ = runWorkflowToReview(t, root, wf, verdictJSON("pass", nil, nil))

	// W2: the first ingest only opens the custody quiet window; the pass
	// becomes admissible after a second observation over unchanged evidence.
	if err := ingestWorkflowReview(root, cfg, wf); err != nil {
		t.Fatal(err)
	}
	if wf.Review == nil || wf.Review.Admissible || wf.Review.HoldReason != holdReasonCustodyWindow {
		t.Fatalf("first ingest must hold for the quiet window: %+v", wf.Review)
	}
	backdateCustodyWindow(t, root, wf.ReviewerTaskID, custodyQuietWindow+time.Second)
	if err := ingestWorkflowReview(root, cfg, wf); err != nil {
		t.Fatal(err)
	}
	if wf.Review == nil || !wf.Review.Admissible {
		t.Fatalf("empty p0/p1 pass must be admissible: %+v", wf.Review)
	}
	if wf.Status != workflowStatusReviewPassed {
		t.Fatalf("status=%s", wf.Status)
	}
	if wf.EffectGates.Integration != effectGateHeld {
		t.Fatal("ingesting a pass must not itself release integration")
	}

	if err := tryReleaseWorkflowIntegration(root, cfg, wf); err != nil {
		t.Fatalf("admissible pass must release: %v", err)
	}
	integ, err := loadTask(root, wf.IntegrationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if integ.Status != statusQueued {
		t.Fatalf("released integration status=%s", integ.Status)
	}
	if wf.EffectGates.Integration != effectGateReleased {
		t.Fatalf("integration gate=%s", wf.EffectGates.Integration)
	}
	if wf.EffectGates.Live != effectGateHeld || wf.EffectGates.Cutover != effectGateHeld {
		t.Fatalf("integration release is not live authority: %+v; 反例注入: tryReleaseWorkflowIntegration 里把 Live/Cutover 一并置 released", wf.EffectGates)
	}
}

func TestGateRefusesWhenCandidateIdentityDrifts(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)
	wf, _ = runWorkflowToReview(t, root, wf, verdictJSON("pass", nil, nil))

	integ, err := loadTask(root, wf.IntegrationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	integ.IntegrationGate.CandidateCommit = "someone-elses-commit"
	if err := saveTask(root, integ); err != nil {
		t.Fatal(err)
	}
	dec := evaluateIntegrationRelease(root, cfg, integ)
	if dec.Admit || dec.HoldReason != holdReasonCandidateMismatch {
		t.Fatalf("drifted candidate must hold: admit=%v reason=%q", dec.Admit, dec.HoldReason)
	}
}

func TestGateRefusesWithoutAnyFrozenCandidate(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)

	// A reviewer may not even be admitted before the bytes are frozen.
	writer, err := admitWorkflowWriter(root, cfg, wf, "")
	if err != nil {
		t.Fatal(err)
	}
	markTaskDone(t, root, writer.ID)
	if _, err := admitWorkflowReviewer(root, cfg, wf); !errors.Is(err, errWorkflowMalformed) {
		t.Fatalf("reviewer without frozen candidate must be refused: %v", err)
	}
	integ, err := loadTask(root, wf.IntegrationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if dec := evaluateIntegrationRelease(root, cfg, integ); dec.Admit {
		t.Fatal("a gate with no review task must never admit")
	}
}

func TestGateRefusesReviewerThatIsNotAnIndependentReadOnlyRole(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)
	wf, review := runWorkflowToReview(t, root, wf, verdictJSON("pass", nil, nil))
	completeReviewCustodyWindow(t, root, cfg, wf)
	integ, err := loadTask(root, wf.IntegrationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if dec := evaluateIntegrationRelease(root, cfg, integ); !dec.Admit {
		t.Fatalf("baseline must be admissible before mutation: %q", dec.HoldReason)
	}

	t.Run("reviewer claims a write domain", func(t *testing.T) {
		rv, _ := loadTask(root, review.ID)
		rv.WriteDomain = copyWriteDomain(wf.WriteDomain)
		if err := saveTask(root, rv); err != nil {
			t.Fatal(err)
		}
		defer func() {
			rv.WriteDomain = nil
			_ = saveTask(root, rv)
		}()
		if dec := evaluateIntegrationRelease(root, cfg, integ); dec.Admit || dec.HoldReason != holdReasonCustody {
			t.Fatalf("a reviewer holding write claims is not read-only: %+v", dec)
		}
	})

	t.Run("reviewer shares the writer session", func(t *testing.T) {
		writer, _ := loadTask(root, wf.WriterTaskID)
		writer.SessionID = "shared-session"
		_ = saveTask(root, writer)
		rv, _ := loadTask(root, review.ID)
		rv.SessionID = "shared-session"
		if err := saveTask(root, rv); err != nil {
			t.Fatal(err)
		}
		defer func() {
			rv.SessionID = ""
			_ = saveTask(root, rv)
		}()
		if dec := evaluateIntegrationRelease(root, cfg, integ); dec.Admit || dec.HoldReason != holdReasonCustody {
			t.Fatalf("a reviewer inheriting the writer session is not independent: %+v", dec)
		}
	})

	t.Run("a rival reviewer is still live for the same writer", func(t *testing.T) {
		rival := newTask(root, cfg, typeReview, "rival review", dir, []string{"p"}, 5)
		rival.ReviewOf = wf.WriterTaskID
		rival.Status = statusRunning
		if err := saveTask(root, rival); err != nil {
			t.Fatal(err)
		}
		defer func() {
			rival.Status = statusCanceled
			_ = saveTask(root, rival)
		}()
		if dec := evaluateIntegrationRelease(root, cfg, integ); dec.Admit || dec.HoldReason != holdReasonCustody {
			t.Fatalf("two active reviewers for one writer must hold: %+v", dec)
		}
	})
}

func TestTickDrainsOrdinaryCardsButNeverAGatedIntegrationCard(t *testing.T) {
	orig := tickRunTask
	t.Cleanup(func() { tickRunTask = orig })

	root, dir := workflowTestRoot(t)
	// Pin the workflow to the plain claude lane so the integration card is
	// otherwise perfectly dispatchable; the gate must be the only thing
	// stopping it, not an unconfigured engine.
	wf := initTestWorkflow(t, root, dir, "-writer-engine", "claude", "-reviewer-engine", "claude")
	integ, err := loadTask(root, wf.IntegrationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	restoreScheduling(integ)
	integ.Status = statusQueued
	if err := saveTask(root, integ); err != nil {
		t.Fatal(err)
	}

	cfg := defaultConfig(fakeClaudeBin(t, mkOKResultJSON("sess"), "", 0))
	// Positive control: without this, "nothing ran" would also be satisfied by a
	// tick that simply drained nothing at all.
	control := newTask(root, cfg, typeSequence, "ordinary card", t.TempDir(), []string{"p"}, 9)
	if err := saveTask(root, control); err != nil {
		t.Fatal(err)
	}

	dispatched := map[string]bool{}
	tickRunTask = func(ctx context.Context, root string, cfg *Config, tk *Task, via string) error {
		dispatched[tk.ID] = true
		tk.Status = statusDone
		tk.touch()
		_ = saveTask(root, tk)
		return nil
	}
	if err := tick(root, cfg, true, true); err != nil {
		t.Fatal(err)
	}
	if !dispatched[control.ID] {
		t.Fatal("positive control never ran, so this tick proves nothing about the gate")
	}
	if dispatched[integ.ID] {
		t.Fatal("tick dispatched a gated integration card; 反例注入: tick.go 里删掉 integrationGateAllows 分支")
	}
	fresh, err := loadTask(root, integ.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status != statusQueued {
		t.Fatalf("integration card status=%s", fresh.Status)
	}
}

func TestHandleReviewVerdictRejectsPassWithOpenFindings(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	impl := mkImplTask(t, root, cfg)
	impl.Closeout = "write done"
	if err := saveTask(root, impl); err != nil {
		t.Fatal(err)
	}
	rv := mkReviewTask(t, root, cfg, impl)
	handleReviewVerdict(root, cfg, rv, verdictJSON("pass", []string{"unclosed p0"}, nil), nil)
	for _, x := range listQueued(t, root) {
		if strings.HasPrefix(x.Title, "收口:") {
			t.Fatalf("pass with open findings must not close out: %+v", x)
		}
	}
}

func TestReviewVerdictAdmissiblePassVocabulary(t *testing.T) {
	if reviewVerdictIsAdmissiblePass(&reviewVerdict{Verdict: "pass", P0: []string{"x"}}) {
		t.Fatal("pass with a p0 is not admissible")
	}
	if reviewVerdictIsAdmissiblePass(&reviewVerdict{Verdict: "pass", P1: []string{"x"}}) {
		t.Fatal("pass with a p1 is not admissible")
	}
	if !reviewVerdictIsAdmissiblePass(&reviewVerdict{Verdict: "pass"}) {
		t.Fatal("pass with empty p0/p1 is admissible")
	}
	for _, token := range []string{"ACCEPT", "HELD", "done", ""} {
		if reviewVerdictIsAdmissiblePass(&reviewVerdict{Verdict: token}) {
			t.Fatalf("%q is not machine review vocabulary", token)
		}
	}
	if reviewVerdictIsAdmissiblePass(nil) {
		t.Fatal("a nil verdict is not a pass")
	}
}

// ---- area 2: write-domain conflicts ----

func TestWorkflowWriteDomainConflictsFailClosed(t *testing.T) {
	root, dir := workflowTestRoot(t)
	_ = initTestWorkflow(t, root, dir)

	t.Run("exact path overlap", func(t *testing.T) {
		err := cmdWorkflowInit([]string{
			"-root", root, "-module", "authcopy", "-goal-id", "authcopy-v1", "-goal", "dup",
			"-dir", dir, "-terminal-criteria", "c", "-engine", grokBuildRunnerName,
			"-write-domain-id", "auth-copy", "-write-domain-lineage", "auth-copy-lineage",
			"-write-domain-component", "auth", "-write-paths", "internal/auth",
		})
		if !errors.Is(err, errWorkflowWriteOverlap) {
			t.Fatalf("same path in one repo must fail closed: %v", err)
		}
	})

	t.Run("subtree overlap", func(t *testing.T) {
		err := cmdWorkflowInit([]string{
			"-root", root, "-module", "authtoken", "-goal-id", "authtoken-v1", "-goal", "sub",
			"-dir", dir, "-terminal-criteria", "c", "-engine", grokBuildRunnerName,
			"-write-domain-id", "auth-token-file", "-write-domain-lineage", "auth-token-file-lineage",
			"-write-domain-component", "auth", "-write-paths", "internal/auth/token.go",
		})
		if !errors.Is(err, errWorkflowWriteOverlap) {
			t.Fatalf("a file under a claimed subtree must fail closed: %v", err)
		}
	})

	t.Run("disjoint paths in the same repo are allowed", func(t *testing.T) {
		if err := cmdWorkflowInit([]string{
			"-root", root, "-module", "billing", "-goal-id", "billing-v1", "-goal", "bill",
			"-dir", dir, "-terminal-criteria", "c", "-engine", grokBuildRunnerName,
			"-write-domain-id", "billing-core", "-write-domain-lineage", "billing-core-lineage",
			"-write-domain-component", "billing", "-write-paths", "internal/billing",
			"-write-resources", "database:billing.primary",
		}); err != nil {
			t.Fatalf("genuinely disjoint lanes must be admitted: %v", err)
		}
	})

	t.Run("shared closed resource across repos", func(t *testing.T) {
		other := t.TempDir()
		mustWriteFile(t, filepath.Join(other, "internal", "search", "s.go"), "package search\n")
		err := cmdWorkflowInit([]string{
			"-root", root, "-module", "search", "-goal-id", "search-v1", "-goal", "search",
			"-dir", other, "-terminal-criteria", "c", "-engine", grokBuildRunnerName,
			"-write-domain-id", "search-core", "-write-domain-lineage", "search-core-lineage",
			"-write-domain-component", "search", "-write-paths", "internal/search",
			"-write-resources", "database:billing.primary",
		})
		if !errors.Is(err, errWorkflowWriteOverlap) {
			t.Fatalf("disjoint paths do not excuse a shared database: %v; 反例注入: auditWorkflowWriteDomains 里删掉全局 detectWriteDomainOverlaps 资源检查", err)
		}
	})

	t.Run("duplicate lineage", func(t *testing.T) {
		other := t.TempDir()
		mustWriteFile(t, filepath.Join(other, "internal", "reports", "r.go"), "package reports\n")
		err := cmdWorkflowInit([]string{
			"-root", root, "-module", "reports", "-goal-id", "reports-v1", "-goal", "reports",
			"-dir", dir, "-terminal-criteria", "c", "-engine", grokBuildRunnerName,
			"-write-domain-id", "reports-core", "-write-domain-lineage", "auth-tokens-lineage",
			"-write-domain-component", "reports", "-write-paths", "internal/reports",
		})
		if !errors.Is(err, errWorkflowWriteOverlap) {
			t.Fatalf("one lineage may own only one live domain: %v", err)
		}
	})
}

func TestTerminalWorkflowReleasesItsWriteDomainClaim(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)

	dup := []string{
		"-root", root, "-module", "authnext", "-goal-id", "authnext-v1", "-goal", "successor",
		"-dir", dir, "-terminal-criteria", "c", "-engine", grokBuildRunnerName,
		"-write-domain-id", "auth-next", "-write-domain-lineage", "auth-next-lineage",
		"-write-domain-component", "auth", "-write-paths", "internal/auth",
	}
	if err := cmdWorkflowInit(dup); !errors.Is(err, errWorkflowWriteOverlap) {
		t.Fatalf("a live claim must block a successor: %v", err)
	}
	if err := markWorkflowRoute(root, cfg, wf, "owner", "handed to owner"); err != nil {
		t.Fatal(err)
	}
	if err := cmdWorkflowInit(dup); err != nil {
		t.Fatalf("a terminal route must release its claim: %v", err)
	}
}

func TestFederatedModeBindsParentAndSerialModeRefusesOne(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	parent := initTestWorkflow(t, root, dir)

	childDir := t.TempDir()
	mustWriteFile(t, filepath.Join(childDir, "internal", "search", "s.go"), "package search\n")
	childArgs := []string{
		"-root", root, "-mode", workflowModeFederated, "-module", "search",
		"-goal-id", "search-v1", "-goal", "search module", "-parent", parent.ID,
		"-dir", childDir, "-terminal-criteria", "c", "-engine", grokBuildRunnerName,
		"-write-domain-id", "search-core", "-write-domain-lineage", "search-core-lineage",
		"-write-domain-component", "search", "-write-paths", "internal/search",
	}
	if err := cmdWorkflowInit(childArgs); err != nil {
		t.Fatal(err)
	}
	wfs, err := loadWorkflows(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var child *WorkflowRecord
	for _, wf := range wfs {
		if wf.ModuleID == "search" {
			child = wf
		}
	}
	if child == nil || child.ParentID != parent.ID || child.Mode != workflowModeFederated {
		t.Fatalf("federated child must name its parent: %+v", child)
	}
	if child.EffectGates.Integration != effectGateHeld {
		t.Fatal("a federated module integrate card is held like any other")
	}

	t.Run("unknown parent", func(t *testing.T) {
		bad := t.TempDir()
		mustWriteFile(t, filepath.Join(bad, "internal", "ghost", "g.go"), "package ghost\n")
		err := cmdWorkflowInit([]string{
			"-root", root, "-mode", workflowModeFederated, "-module", "ghost",
			"-goal-id", "ghost-v1", "-goal", "ghost", "-parent", "wf0101-0000-aaaaaa",
			"-dir", bad, "-terminal-criteria", "c", "-engine", grokBuildRunnerName,
			"-write-domain-id", "ghost-core", "-write-domain-lineage", "ghost-core-lineage",
			"-write-domain-component", "ghost", "-write-paths", "internal/ghost",
		})
		if !errors.Is(err, errWorkflowParent) {
			t.Fatalf("a dangling parent must fail closed: %v", err)
		}
	})

	t.Run("serial mode rejects a parent", func(t *testing.T) {
		bad := t.TempDir()
		mustWriteFile(t, filepath.Join(bad, "internal", "solo", "s.go"), "package solo\n")
		err := cmdWorkflowInit([]string{
			"-root", root, "-mode", workflowModeSerial, "-module", "solo",
			"-goal-id", "solo-v1", "-goal", "solo", "-parent", parent.ID,
			"-dir", bad, "-terminal-criteria", "c", "-engine", grokBuildRunnerName,
			"-write-domain-id", "solo-core", "-write-domain-lineage", "solo-core-lineage",
			"-write-domain-component", "solo", "-write-paths", "internal/solo",
		})
		if !errors.Is(err, errWorkflowParent) {
			t.Fatalf("serial mode has no parent: %v", err)
		}
	})
}

// ---- area 3: restart/replay idempotence ----

func TestReplayNeverCreatesASecondWriterOrReviewer(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)

	writer, err := admitWorkflowWriter(root, cfg, wf, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		// A manager that restarts mid-round re-runs the same command against a
		// stale in-memory record.
		stale := *wf
		if _, err := admitWorkflowWriter(root, cfg, &stale, ""); !errors.Is(err, errWorkflowDuplicateRole) {
			t.Fatalf("replay %d minted a second writer: %v", i, err)
		}
	}
	markTaskDone(t, root, writer.ID)
	commit, tree := workflowHeadCandidate(t, dir)
	if err := freezeWorkflowCandidate(root, cfg, wf, WorkflowCandidate{Commit: commit, Tree: tree}); err != nil {
		t.Fatal(err)
	}
	review, err := admitWorkflowReviewer(root, cfg, wf)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		stale := *wf
		if _, err := admitWorkflowReviewer(root, cfg, &stale); !errors.Is(err, errWorkflowDuplicateRole) {
			t.Fatalf("replay %d minted a second reviewer: %v", i, err)
		}
	}

	writers, reviewers := 0, 0
	tasks, err := loadTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range tasks {
		if tk.WorkflowID != wf.ID {
			continue
		}
		switch {
		case tk.Type == typeReview:
			reviewers++
		case tk.IntegrationGate == nil:
			writers++
		}
	}
	if writers != 1 || reviewers != 1 {
		t.Fatalf("exactly one writer and one reviewer must exist, got %d/%d", writers, reviewers)
	}
	if review.ID == writer.ID {
		t.Fatal("reviewer and writer must be different cards")
	}
}

func TestFreezeRefusesWhileTheWriterIsStillLive(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)
	if _, err := admitWorkflowWriter(root, cfg, wf, ""); err != nil {
		t.Fatal(err)
	}
	commit, tree := workflowHeadCandidate(t, dir)
	err := freezeWorkflowCandidate(root, cfg, wf, WorkflowCandidate{Commit: commit, Tree: tree})
	if !errors.Is(err, errWorkflowDuplicateRole) {
		t.Fatalf("bytes under a live writer are not frozen: %v", err)
	}
}

func TestReplayedIngestAndReleaseAreStable(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)
	wf, _ = runWorkflowToReview(t, root, wf, verdictJSON("pass", nil, nil))

	// W2: establish the custody receipt once; replays must then be stable.
	completeReviewCustodyWindow(t, root, cfg, wf)
	for i := 0; i < 3; i++ {
		if err := ingestWorkflowReview(root, cfg, wf); err != nil {
			t.Fatalf("ingest replay %d: %v", i, err)
		}
		if !wf.Review.Admissible || wf.Review.TaskID != wf.ReviewerTaskID {
			t.Fatalf("ingest replay %d drifted: %+v", i, wf.Review)
		}
	}
	notifies, err := os.ReadDir(workflowNotifyDir(root))
	if err != nil {
		t.Fatal(err)
	}
	if len(notifies) != 1 {
		t.Fatalf("replayed ingest must rewrite one receipt, not accumulate %d", len(notifies))
	}

	for i := 0; i < 3; i++ {
		if err := tryReleaseWorkflowIntegration(root, cfg, wf); err != nil {
			t.Fatalf("release replay %d: %v", i, err)
		}
	}
	integ, err := loadTask(root, wf.IntegrationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if integ.Status != statusQueued {
		t.Fatalf("release replay left status=%s", integ.Status)
	}
	if wf.EffectGates.Live != effectGateHeld || wf.EffectGates.Cutover != effectGateHeld {
		t.Fatalf("replay must not widen effect gates: %+v", wf.EffectGates)
	}
}

func TestReleasedIntegrationIsReHeldWhenEvidenceDisappears(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)
	wf, review := runWorkflowToReview(t, root, wf, verdictJSON("pass", nil, nil))
	completeReviewCustodyWindow(t, root, cfg, wf)
	if err := tryReleaseWorkflowIntegration(root, cfg, wf); err != nil {
		t.Fatal(err)
	}

	// The review transcript is truncated after release, e.g. by a log rotation
	// or an operator clearing a workspace.
	writeReviewLog(t, root, review.ID, "")
	if err := tryReleaseWorkflowIntegration(root, cfg, wf); !errors.Is(err, errWorkflowHeld) {
		t.Fatalf("lost evidence must re-hold: %v", err)
	}
	integ, err := loadTask(root, wf.IntegrationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if integ.Status != statusHeld {
		t.Fatalf("status=%s; 反例注入: tryReleaseWorkflowIntegration 的 !dec.Admit 分支里删掉 re-hold", integ.Status)
	}
	if integrationGateAllows(root, cfg, integ) {
		t.Fatal("tick must also refuse the re-held card")
	}

	// Restoring the evidence must make the card releasable again, otherwise the
	// re-hold is a one-way trap rather than a fail-closed latch.
	writeReviewLog(t, root, review.ID, verdictJSON("pass", nil, nil))
	if err := tryReleaseWorkflowIntegration(root, cfg, wf); err != nil {
		t.Fatalf("restored evidence must release again: %v", err)
	}
	again, err := loadTask(root, wf.IntegrationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != statusQueued {
		t.Fatalf("re-release left status=%s", again.Status)
	}
}

func TestGateRefusesAReviewBoundToNoWriter(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)
	wf, review := runWorkflowToReview(t, root, wf, verdictJSON("pass", nil, nil))

	integ, err := loadTask(root, wf.IntegrationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	// Drop both the gate's writer binding and the review's own subject, leaving a
	// verdict that names no candidate producer at all.
	integ.IntegrationGate.WriterTaskID = ""
	if err := saveTask(root, integ); err != nil {
		t.Fatal(err)
	}
	rv, err := loadTask(root, review.ID)
	if err != nil {
		t.Fatal(err)
	}
	rv.ReviewOf = ""
	if err := saveTask(root, rv); err != nil {
		t.Fatal(err)
	}
	dec := evaluateIntegrationRelease(root, cfg, integ)
	if dec.Admit || dec.HoldReason != holdReasonCustody {
		t.Fatalf("an unbound review proves nothing: admit=%v reason=%q; 反例注入: integrationCustodyOK 里去掉 reviewOf == \"\" 的 fail-closed 分支", dec.Admit, dec.HoldReason)
	}
}

func TestTerminalMarkRefusedWhileTheWorkflowStillHasARunnableCard(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)
	writer, err := admitWorkflowWriter(root, cfg, wf, "")
	if err != nil {
		t.Fatal(err)
	}

	// Marking terminal releases the write-domain claim. Doing that with a queued
	// writer would let a successor module claim the same paths under it.
	err = markWorkflowRoute(root, cfg, wf, "owner", "handing off")
	if err == nil || !errors.Is(err, errWorkflowDuplicateRole) {
		t.Fatalf("a runnable card must block terminalization: %v; 反例注入: markWorkflowRoute 里删掉 workflowDispatchableCards 检查", err)
	}
	if !strings.Contains(err.Error(), writer.ID) {
		t.Fatalf("the refusal must name the blocking card: %v", err)
	}
	fresh, err := loadWorkflow(root, cfg, wf.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status == workflowStatusOwnerChoice {
		t.Fatal("a refused mark must not have persisted the terminal status")
	}
	if entries, err := os.ReadDir(workflowNotifyDir(root)); err == nil && len(entries) != 0 {
		t.Fatalf("a refused mark must not leave a Root receipt: %v", entries)
	}

	if err := terminalize(root, writer.ID, statusHeld, "test", "hold the writer", nil); err != nil {
		t.Fatal(err)
	}
	if err := markWorkflowRoute(root, cfg, wf, "owner", "handing off"); err != nil {
		t.Fatalf("with every card held the route may terminalize: %v", err)
	}
}

func TestCorruptWorkflowRecordIsSkippedNotFatal(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)
	if err := os.WriteFile(workflowPath(root, "wf0101-0000-bbbbbb"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	wfs, err := loadWorkflows(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(wfs) != 1 || wfs[0].ID != wf.ID {
		t.Fatalf("a corrupt sibling must not hide a healthy record: %+v", wfs)
	}
}

func TestWorkflowRecordRejectsWidenedLiveGateOnReload(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)

	raw, err := os.ReadFile(workflowPath(root, wf.ID))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["effect_gates"] = map[string]any{
		"integration": effectGateReleased, "live": effectGateReleased, "cutover": effectGateHeld,
	}
	edited, _ := json.Marshal(doc)
	if err := os.WriteFile(workflowPath(root, wf.ID), edited, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadWorkflow(root, cfg, wf.ID); !errors.Is(err, errWorkflowMalformed) {
		t.Fatalf("a hand-edited live release must be rejected on load: %v", err)
	}
}

// ---- area 4: bounded loops ----

func TestRepairRoundsAreBoundedAndExhaustionNotifiesRoot(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir) // -max-rounds 2

	for round := 0; round <= wf.MaxRounds; round++ {
		if round == 0 {
			wf, _ = runWorkflowToReview(t, root, wf, verdictJSON("block", []string{"still broken"}, nil))
		} else {
			markTaskDone(t, root, wf.WriterTaskID)
			// Each repair round produces new bytes; commit them so the frozen
			// candidate is a distinct, verifiable repository identity.
			mustWriteFile(t, filepath.Join(dir, "internal", "auth", "token.go"),
				"package auth\n// round "+string(rune('0'+round))+"\n")
			workflowGit(t, dir, "add", "-A")
			workflowGit(t, dir, "commit", "-q", "-m", "repair round")
			commit, tree := workflowHeadCandidate(t, dir)
			if err := freezeWorkflowCandidate(root, cfg, wf, WorkflowCandidate{
				Commit: commit, Tree: tree,
			}); err != nil {
				t.Fatal(err)
			}
			review, err := admitWorkflowReviewer(root, cfg, wf)
			if err != nil {
				t.Fatal(err)
			}
			markTaskDone(t, root, review.ID)
			writeReviewLog(t, root, review.ID, verdictJSON("block", []string{"still broken"}, nil))
		}
		if err := ingestWorkflowReview(root, cfg, wf); err != nil {
			t.Fatal(err)
		}
		_, err := admitWorkflowRepair(root, cfg, wf, "P0-1: still broken\n", "round "+string(rune('0'+round)))
		if round < wf.MaxRounds {
			if err != nil {
				t.Fatalf("round %d must be allowed: %v", round, err)
			}
			if wf.CurrentRound != round+1 {
				t.Fatalf("round counter=%d want %d", wf.CurrentRound, round+1)
			}
			continue
		}
		if !errors.Is(err, errWorkflowRoundsExceeded) {
			t.Fatalf("round %d must exhaust, got %v; 反例注入: admitWorkflowRepair 里删掉 CurrentRound+1 > MaxRounds 判据", round, err)
		}
	}

	if wf.Status != workflowStatusExhausted {
		t.Fatalf("status=%s", wf.Status)
	}
	if wf.MaterialNotify == nil || wf.MaterialNotify.Kind != rootNotifyExhausted {
		t.Fatalf("exhaustion is material and must notify Root: %+v", wf.MaterialNotify)
	}
	integ, err := loadTask(root, wf.IntegrationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if integ.Status != statusHeld {
		t.Fatalf("an exhausted route leaves integration held, got %s", integ.Status)
	}
}

func TestRepairClearsThePreviousCandidateAndVerdict(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)
	wf, _ = runWorkflowToReview(t, root, wf, verdictJSON("concerns", nil, []string{"tighten this"}))
	if err := ingestWorkflowReview(root, cfg, wf); err != nil {
		t.Fatal(err)
	}
	if _, err := admitWorkflowRepair(root, cfg, wf, "P1-1: tighten this\n", "round 1"); err != nil {
		t.Fatal(err)
	}
	if wf.Candidate != nil || wf.Review != nil || wf.ReviewerTaskID != "" {
		t.Fatalf("repaired bytes are a new candidate: %+v", wf)
	}
	integ, err := loadTask(root, wf.IntegrationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if integ.IntegrationGate.CandidateCommit != "" || integ.IntegrationGate.ReviewTaskID != "" {
		t.Fatalf("the gate must forget the stale candidate: %+v; 反例注入: admitWorkflowRepair 里不再置空 Candidate/Review", integ.IntegrationGate)
	}
	if dec := evaluateIntegrationRelease(root, cfg, integ); dec.Admit {
		t.Fatal("a repair round must not leave the gate admitting the old pass")
	}
}

func TestRepairRefusesAfterAnAdmissiblePass(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)
	wf, _ = runWorkflowToReview(t, root, wf, verdictJSON("pass", nil, nil))
	completeReviewCustodyWindow(t, root, cfg, wf)
	if _, err := admitWorkflowRepair(root, cfg, wf, "", ""); !errors.Is(err, errWorkflowMalformed) {
		t.Fatalf("there is nothing to repair after a pass: %v", err)
	}
}

func TestMaxRoundsMustBeAtLeastOne(t *testing.T) {
	root, dir := workflowTestRoot(t)
	err := cmdWorkflowInit([]string{
		"-root", root, "-module", "auth", "-goal", "g", "-dir", dir,
		"-terminal-criteria", "c", "-engine", grokBuildRunnerName,
		"-write-paths", "internal/auth", "-max-rounds", "0",
	})
	if !errors.Is(err, errWorkflowMalformed) {
		t.Fatalf("max_rounds 0 is not a bounded loop: %v", err)
	}
}

// ---- area 5: durable manager hooks ----

func TestRoutineProgressIsDurableAndNeverNotifiesRoot(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)

	for _, path := range []string{wf.Progress.JSON, wf.Progress.Markdown} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("progress coordinate %s missing: %v", path, err)
		}
	}
	if wf.MaterialNotify != nil {
		t.Fatal("init is routine, not material")
	}

	writer, err := admitWorkflowWriter(root, cfg, wf, "")
	if err != nil {
		t.Fatal(err)
	}
	markTaskDone(t, root, writer.ID)
	commit, tree := workflowHeadCandidate(t, dir)
	if err := freezeWorkflowCandidate(root, cfg, wf, WorkflowCandidate{Commit: commit, Tree: tree}); err != nil {
		t.Fatal(err)
	}
	if _, err := admitWorkflowReviewer(root, cfg, wf); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(workflowNotifyDir(root)); err == nil && len(entries) != 0 {
		t.Fatalf("writer/freeze/review are routine progress, not Root hooks: %v", entries)
	}

	var progress map[string]any
	raw, err := os.ReadFile(wf.Progress.JSON)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &progress); err != nil {
		t.Fatal(err)
	}
	if progress["schema"] != "cardex.workflow.progress.v1" {
		t.Fatalf("progress schema=%v", progress["schema"])
	}
	if progress["reviewer_task_id"] != wf.ReviewerTaskID || progress["status"] != wf.Status {
		t.Fatalf("progress projection is stale: %v", progress)
	}
	md, err := os.ReadFile(wf.Progress.Markdown)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(md), wf.ReviewerTaskID) {
		t.Fatal("the Markdown projection must name the current reviewer")
	}
}

func TestOnlyMaterialTransitionsWriteRootReceipts(t *testing.T) {
	for _, tc := range []struct{ kind, want, status string }{
		{"external", rootNotifyExternal, workflowStatusExternalBlocked},
		{"owner", rootNotifyOwner, workflowStatusOwnerChoice},
		{"exhausted", rootNotifyExhausted, workflowStatusExhausted},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			root, dir := workflowTestRoot(t)
			cfg := workflowTestCfg(t, root)
			wf := initTestWorkflow(t, root, dir)
			if err := markWorkflowRoute(root, cfg, wf, tc.kind, "fixture reason"); err != nil {
				t.Fatal(err)
			}
			if wf.Status != tc.status {
				t.Fatalf("status=%s want %s", wf.Status, tc.status)
			}
			receipt := workflowNotifyPath(root, wf.ID, tc.want)
			data, err := os.ReadFile(receipt)
			if err != nil {
				t.Fatalf("material receipt missing: %v", err)
			}
			var n WorkflowNotify
			if err := json.Unmarshal(data, &n); err != nil {
				t.Fatal(err)
			}
			if n.Kind != tc.want || n.Workflow != wf.ID || n.Summary != "fixture reason" {
				t.Fatalf("receipt=%+v", n)
			}
		})
	}
}

func TestNonMaterialNotifyKindIsRefused(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)

	if err := emitRootNotify(root, wf, "routine_progress", "nope"); !errors.Is(err, errWorkflowMalformed) {
		t.Fatalf("routine progress must never reach Root: %v", err)
	}
	if err := markWorkflowRoute(root, cfg, wf, "whatever", ""); !errors.Is(err, errWorkflowMalformed) {
		t.Fatalf("unknown mark kind: %v", err)
	}
	if entries, err := os.ReadDir(workflowNotifyDir(root)); err == nil && len(entries) != 0 {
		t.Fatalf("refused notifies must leave no receipt: %v", entries)
	}
}

func TestWorkflowEnginesMustBePinnableWithoutFailOpen(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)

	for _, engine := range []string{"", "   ", "definitely-not-a-runner", "sol"} {
		if err := validateWorkflowEngine(cfg, engine); !errors.Is(err, errWorkflowUnknownEngine) {
			t.Fatalf("engine %q must be refused: %v", engine, err)
		}
	}
	for _, engine := range []string{"claude", "codex", "gemini", grokBuildRunnerName, cursorRunnerName, kimiCLIRunnerName} {
		if err := validateWorkflowEngine(cfg, engine); err != nil {
			t.Fatalf("engine %q is a real pinned lane: %v", engine, err)
		}
	}

	if err := cmdWorkflowInit([]string{
		"-root", root, "-module", "auth", "-goal", "g", "-dir", dir,
		"-terminal-criteria", "c", "-write-paths", "internal/auth",
	}); !errors.Is(err, errWorkflowUnknownEngine) {
		t.Fatalf("an unspecified engine would silently become the default provider: %v", err)
	}

	wf := initTestWorkflow(t, root, dir)
	writer, err := admitWorkflowWriter(root, cfg, wf, "")
	if err != nil {
		t.Fatal(err)
	}
	if writer.PreferRunner != grokBuildRunnerName || !writer.RunnerExplicit {
		t.Fatalf("the writer must be explicitly pinned: runner=%q explicit=%v; 反例注入: admitWorkflowWriter 里不再写 PreferRunner", writer.PreferRunner, writer.RunnerExplicit)
	}
	if writer.ReviewAfter {
		t.Fatal("the workflow owns the review gate; an auto-review child would be a second reviewer")
	}
	if writer.WriteDomain == nil || writer.WriteDomain.ID != "auth-tokens" {
		t.Fatalf("writer domain: %+v", writer.WriteDomain)
	}

	markTaskDone(t, root, writer.ID)
	commit, tree := workflowHeadCandidate(t, dir)
	if err := freezeWorkflowCandidate(root, cfg, wf, WorkflowCandidate{Commit: commit, Tree: tree}); err != nil {
		t.Fatal(err)
	}
	review, err := admitWorkflowReviewer(root, cfg, wf)
	if err != nil {
		t.Fatal(err)
	}
	if review.Type != typeReview || review.WriteDomain != nil || review.SessionID != "" {
		t.Fatalf("the reviewer must be an independent read-only role: %+v", review)
	}
	if review.ReviewOf != writer.ID || review.PreferRunner != grokBuildRunnerName {
		t.Fatalf("reviewer binding: review_of=%s runner=%s", review.ReviewOf, review.PreferRunner)
	}
}

// ---- P1 repairs (review bc-78137f61 on PR #7 @ 722670b) ----

// P1-1a: the Grok Opus adversarial-review obligation must never attach to a
// workflow-bound card. The workflow record owns its single independent
// reviewer; ReviewAfter here would mint a second one for the same candidate.
func TestGrokOpusAdversarialReviewNeverRegrowsAReviewerOnWorkflowCards(t *testing.T) {
	cfg := grokBuildTestConfig(t, "/usr/bin/true")

	writer := &Task{Type: typeSequence, Model: "opus", WorkflowID: "wf0101-0000-aaaaaa"}
	ensureGrokOpusAdversarialReview(cfg, writer)
	if writer.ReviewAfter || writer.SolMaxAdversarialReview {
		t.Fatalf("workflow writer regrew an auto reviewer: review_after=%v sol_max=%v; 反例注入: ensureGrokOpusAdversarialReview 里删掉 WorkflowID 守卫",
			writer.ReviewAfter, writer.SolMaxAdversarialReview)
	}

	integ := &Task{Type: typeSequence, Model: "opus", WorkflowID: "wf0101-0000-aaaaaa",
		IntegrationGate: &IntegrationGate{WorkflowID: "wf0101-0000-aaaaaa"}}
	ensureGrokOpusAdversarialReview(cfg, integ)
	if integ.ReviewAfter || integ.SolMaxAdversarialReview {
		t.Fatalf("integration card regrew an auto reviewer: %+v", integ)
	}

	// Positive control: without it, deleting the whole function body would also
	// turn this test green while silently dropping the Opus obligation.
	plain := &Task{Type: typeSequence, Model: "opus"}
	ensureGrokOpusAdversarialReview(cfg, plain)
	if !plain.ReviewAfter || !plain.SolMaxAdversarialReview {
		t.Fatalf("non-workflow Opus card must still owe its adversarial review: %+v", plain)
	}

	// The shared admission guardrail is the backstop for every other entry
	// point that flips ReviewAfter on (owner routes, stakes, emit).
	bound := &Task{Type: typeSequence, WorkflowID: "wf0101-0000-aaaaaa", ReviewAfter: true}
	enforceReviewAfterEligibility(bound)
	if bound.ReviewAfter {
		t.Fatal("enforceReviewAfterEligibility must clear ReviewAfter on workflow-bound cards; 反例注入: stakes.go 守卫里删掉 WorkflowID 条件")
	}
}

// P1-1b: fix-loop repair and escalation cards must preserve the reviewed
// card's WorkflowID and WriteDomain. A repair card without them is invisible
// to workflowActiveRole (a second writer could be admitted onto the same
// domain) and fail-opens the write-domain audit.
func TestFixLoopRepairAndEscalationCardsPreserveWorkflowBinding(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)
	wf, review := runWorkflowToReview(t, root, wf, verdictJSON("concerns", nil, []string{"tighten"}))

	rv, err := loadTask(root, review.ID)
	if err != nil {
		t.Fatal(err)
	}
	handleReviewVerdict(root, cfg, rv, verdictJSON("concerns", nil, []string{"tighten"}), nil)

	tasks, err := loadTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	var fix *Task
	for _, tk := range tasks {
		if strings.HasPrefix(tk.Title, "修复R1: ") {
			fix = tk
		}
	}
	if fix == nil {
		t.Fatal("concerns must still enqueue the repair card")
	}
	if fix.WorkflowID != wf.ID {
		t.Fatalf("repair card lost its workflow binding: %q; 反例注入: handleReviewVerdict 修复卡不再继承 orig.WorkflowID", fix.WorkflowID)
	}
	if fix.WriteDomain == nil || fix.WriteDomain.ID != "auth-tokens" {
		t.Fatalf("repair card lost the write-domain claim: %+v; 反例注入: 修复卡不再继承 orig.WriteDomain", fix.WriteDomain)
	}
	if fix.ReviewAfter {
		t.Fatal("a workflow-bound repair card must not carry review_after: the workflow owns its single reviewer")
	}
	// The preserved binding is what keeps one-writer-one-reviewer machine-checkable:
	// while the repair card is live, no second writer can be admitted.
	if _, err := admitWorkflowWriter(root, cfg, wf, ""); !errors.Is(err, errWorkflowDuplicateRole) {
		t.Fatalf("a live repair card must block a second writer: %v", err)
	}

	// Escalation shell (over max rounds) preserves the same lineage. Cancel the
	// live repair card first so the held escalation is the only writer-shaped card.
	if err := terminalize(root, fix.ID, statusCanceled, "test", "clear repair", nil); err != nil {
		t.Fatal(err)
	}
	rv.FixRound = rv.MaxFixRounds // next round would exceed the bound
	if err := saveTask(root, rv); err != nil {
		t.Fatal(err)
	}
	handleReviewVerdict(root, cfg, rv, verdictJSON("block", []string{"still broken"}, nil), nil)
	tasks, err = loadTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	var esc *Task
	for _, tk := range tasks {
		if strings.HasPrefix(tk.Title, "[超轮限") {
			esc = tk
		}
	}
	if esc == nil {
		t.Fatal("over-limit review must enqueue the held escalation card")
	}
	if esc.WorkflowID != wf.ID || esc.WriteDomain == nil || esc.WriteDomain.ID != "auth-tokens" {
		t.Fatalf("escalation card lost workflow binding or claim: workflow=%q domain=%+v", esc.WorkflowID, esc.WriteDomain)
	}
	if esc.Status != statusHeld {
		t.Fatalf("escalation card must be held, got %s", esc.Status)
	}
}

// P1-2: an unloadable record is a possible live overlapping claim. The audit
// must hold closed instead of skipping it, or a successor module could take
// paths that a broken-but-real record still owns.
func TestUnloadableWorkflowRecordHoldsWriteDomainAuditClosed(t *testing.T) {
	root, dir := workflowTestRoot(t)
	_ = initTestWorkflow(t, root, dir)
	brokenPath := workflowPath(root, "wf0101-0000-cccccc")
	if err := os.WriteFile(brokenPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	disjoint := []string{
		"-root", root, "-module", "billing", "-goal-id", "billing-v1", "-goal", "bill",
		"-dir", dir, "-terminal-criteria", "c", "-engine", grokBuildRunnerName,
		"-write-domain-id", "billing-core", "-write-domain-lineage", "billing-core-lineage",
		"-write-domain-component", "billing", "-write-paths", "internal/billing",
	}
	err := cmdWorkflowInit(disjoint)
	if !errors.Is(err, errWorkflowWriteOverlap) {
		t.Fatalf("an unloadable record must hold the audit closed even for disjoint paths: %v; 反例注入: auditWorkflowWriteDomains 用回跳过损坏文件的 loadWorkflows", err)
	}
	if !strings.Contains(err.Error(), "wf0101-0000-cccccc.json") {
		t.Fatalf("the refusal must name the unloadable file: %v", err)
	}

	// Repairing (here: removing) the broken record reopens the lane. Without
	// this leg the hold-closed check could be satisfied by an audit that always
	// refuses everything.
	if err := os.Remove(brokenPath); err != nil {
		t.Fatal(err)
	}
	if err := cmdWorkflowInit(disjoint); err != nil {
		t.Fatalf("with the broken record repaired the disjoint lane must be admitted: %v", err)
	}
}

// P1-3: freeze must prove the operator-supplied commit/tree against the
// repository object database and store the resolved SHAs; a fabricated pair
// would otherwise become the label the integration gate later matches against
// itself.
func TestFreezeCandidateVerifiesIdentityAgainstTheRepository(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)
	commit1, tree1 := workflowHeadCandidate(t, dir)
	mustWriteFile(t, filepath.Join(dir, "internal", "auth", "token.go"), "package auth\n// v2\n")
	workflowGit(t, dir, "add", "-A")
	workflowGit(t, dir, "commit", "-q", "-m", "second")
	commit2, tree2 := workflowHeadCandidate(t, dir)

	t.Run("fabricated commit refused", func(t *testing.T) {
		err := freezeWorkflowCandidate(root, cfg, wf, WorkflowCandidate{
			Commit: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", Tree: tree1})
		if !errors.Is(err, errWorkflowMalformed) {
			t.Fatalf("a commit the repository does not contain must be refused: %v; 反例注入: freezeWorkflowCandidate 里删掉 verifyWorkflowCandidate 调用", err)
		}
	})

	t.Run("tree of a different commit refused", func(t *testing.T) {
		err := freezeWorkflowCandidate(root, cfg, wf, WorkflowCandidate{Commit: commit1, Tree: tree2})
		if !errors.Is(err, errWorkflowMalformed) {
			t.Fatalf("a commit/tree pair naming two snapshots must be refused: %v", err)
		}
	})

	t.Run("commit id pasted into the tree flag refused", func(t *testing.T) {
		err := freezeWorkflowCandidate(root, cfg, wf, WorkflowCandidate{Commit: commit1, Tree: commit1})
		if !errors.Is(err, errWorkflowMalformed) {
			t.Fatalf("a non-tree object in -tree must be refused: %v; 反例注入: verifyWorkflowCandidate 里删掉 cat-file -t 检查", err)
		}
	})

	t.Run("non-git worktree fails closed", func(t *testing.T) {
		if _, _, err := verifyWorkflowCandidate(t.TempDir(), commit1, tree1); err == nil {
			t.Fatal("an unverifiable candidate identity must never be frozen")
		}
	})

	t.Run("abbreviated identities are resolved and stored in full", func(t *testing.T) {
		if err := freezeWorkflowCandidate(root, cfg, wf, WorkflowCandidate{
			Commit: commit2[:10], Tree: tree2[:10]}); err != nil {
			t.Fatalf("verifiable abbreviations must be accepted: %v", err)
		}
		if wf.Candidate.Commit != commit2 || wf.Candidate.Tree != tree2 {
			t.Fatalf("the record must store the resolved SHAs, not the operator strings: %+v", wf.Candidate)
		}
		integ, err := loadTask(root, wf.IntegrationTaskID)
		if err != nil {
			t.Fatal(err)
		}
		if integ.IntegrationGate.CandidateCommit != commit2 || integ.IntegrationGate.CandidateTree != tree2 {
			t.Fatalf("the gate must mirror the resolved identities: %+v", integ.IntegrationGate)
		}
	})
}

func TestWorkflowRecordDoesNotStoreThePromptBody(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)
	if _, err := admitWorkflowWriter(root, cfg, wf, "SECRET PROMPT TOKEN=abc123"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{workflowPath(root, wf.ID), wf.Progress.JSON, wf.Progress.Markdown} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "TOKEN=abc123") {
			t.Fatalf("%s leaked the prompt body", path)
		}
	}
}
