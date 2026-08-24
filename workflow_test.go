package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func workflowTestRoot(t *testing.T) (root, dir string) {
	t.Helper()
	root = testRoot(t)
	if err := saveConfig(root, defaultConfig("claude")); err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "internal", "auth", "token.go"), "package auth\n")
	mustWriteFile(t, filepath.Join(dir, "internal", "billing", "bill.go"), "package billing\n")
	return root, dir
}

func initTestWorkflow(t *testing.T, root, dir string, extra ...string) *WorkflowRecord {
	t.Helper()
	args := append([]string{
		"-root", root, "-mode", workflowModeDirect, "-module", "auth",
		"-goal-id", "auth-token-v1", "-goal", "ship auth tokens",
		"-dir", dir, "-terminal-criteria", "reviewed candidate, integration held",
		"-write-domain-id", "auth-tokens", "-write-domain-lineage", "auth-tokens-lineage",
		"-write-domain-component", "auth", "-write-paths", "internal/auth",
		"-max-rounds", "2",
	}, extra...)
	if err := cmdWorkflowInit(args); err != nil {
		t.Fatal(err)
	}
	wfs, err := loadWorkflows(root)
	if err != nil || len(wfs) != 1 {
		t.Fatalf("load workflows: %v %d", err, len(wfs))
	}
	return wfs[0]
}

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	runErr := fn()
	_ = w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	_ = r.Close()
	return buf.String(), runErr
}

func freezeTestCandidate(t *testing.T, root, id, commit, tree string) {
	t.Helper()
	if err := cmdWorkflowFreeze([]string{"-root", root, "-commit", commit, "-tree", tree, id}); err != nil {
		t.Fatal(err)
	}
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

func passReport(commit, tree string) string {
	return "报告\n```json\n" +
		`{"verdict":"pass","p0":[],"p1":[],"p2":[],"summary":"ok ` + commit + ` ` + tree + `"}` +
		"\n```\n"
}

func TestWorkflowInitHoldsIntegrationAndPinsGrok(t *testing.T) {
	root, dir := workflowTestRoot(t)
	wf := initTestWorkflow(t, root, dir)
	if wf.Mode != workflowModeDirect || wf.EffectGates.Integration != effectGateHeld {
		t.Fatalf("gates: %+v", wf.EffectGates)
	}
	if wf.WriterEngine != workflowEngineGrokBuild || wf.ReviewerEngine != workflowEngineGrokBuild {
		t.Fatalf("engine pin: %s/%s", wf.WriterEngine, wf.ReviewerEngine)
	}
	integ, err := loadTask(root, wf.IntegrationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if integ.Status != statusHeld || integ.IntegrationGate == nil {
		t.Fatalf("integration must be default-held: %+v", integ)
	}
	if _, err := os.Stat(wf.Progress.JSON); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wf.Progress.Markdown); err != nil {
		t.Fatal(err)
	}
	if wf.MaterialNotify != nil {
		t.Fatal("routine init must not notify Root")
	}
}

func TestWorkflowRejectsCodexAndSolEngines(t *testing.T) {
	root, dir := workflowTestRoot(t)
	err := cmdWorkflowInit([]string{
		"-root", root, "-module", "auth", "-goal", "g", "-dir", dir,
		"-terminal-criteria", "c", "-write-paths", "internal/auth",
		"-engine", "codex",
	})
	if !errors.Is(err, errWorkflowForbiddenEngine) {
		t.Fatalf("codex must be rejected: %v", err)
	}
	err = cmdWorkflowInit([]string{
		"-root", root, "-module", "auth", "-goal", "g", "-dir", dir,
		"-terminal-criteria", "c", "-write-paths", "internal/auth",
		"-engine", "sol",
	})
	if !errors.Is(err, errWorkflowForbiddenEngine) {
		t.Fatalf("sol must be rejected: %v", err)
	}
}

func TestWorkflowAdvanceWriterReviewerDedupeAndGrokPin(t *testing.T) {
	root, dir := workflowTestRoot(t)
	wf := initTestWorkflow(t, root, dir)
	cfg, _ := loadConfig(root)
	if err := advanceWorkflow(root, cfg, wf); err != nil {
		t.Fatal(err)
	}
	wf, _ = loadWorkflow(root, wf.ID)
	writer, err := loadTask(root, wf.WriterTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if writer.PreferRunner != workflowEngineGrokBuild || writer.ReviewAfter {
		t.Fatalf("writer pin/review_after: %+v", writer)
	}
	if writer.WriteDomain == nil || writer.WriteDomain.ID != "auth-tokens" {
		t.Fatalf("writer domain: %+v", writer.WriteDomain)
	}
	if _, err := admitWorkflowWriter(root, cfg, wf, "again"); !errors.Is(err, errWorkflowDuplicateRole) {
		t.Fatalf("duplicate writer: %v", err)
	}
	writer.Status = statusDone
	if err := saveTask(root, writer); err != nil {
		t.Fatal(err)
	}
	if err := advanceWorkflow(root, cfg, wf); err != nil {
		t.Fatal(err)
	}
	wf, _ = loadWorkflow(root, wf.ID)
	review, err := loadTask(root, wf.ReviewerTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if review.Type != typeReview || review.PreferRunner != workflowEngineGrokBuild || review.WriteDomain != nil {
		t.Fatalf("reviewer: %+v", review)
	}
	if review.ReviewOf != writer.ID || review.SessionID != "" || review.ID == writer.ID {
		t.Fatalf("reviewer independence: %+v", review)
	}
	if _, err := admitWorkflowReviewer(root, cfg, wf); !errors.Is(err, errWorkflowDuplicateRole) {
		t.Fatalf("duplicate reviewer: %v", err)
	}
}

func TestWorkflowOverlappingClaimsFailClosed(t *testing.T) {
	root, dir := workflowTestRoot(t)
	_ = initTestWorkflow(t, root, dir)
	err := cmdWorkflowInit([]string{
		"-root", root, "-module", "auth-copy", "-goal-id", "auth-copy-v1", "-goal", "dup",
		"-dir", dir, "-terminal-criteria", "c",
		"-write-domain-id", "auth-copy", "-write-domain-lineage", "auth-copy-lineage",
		"-write-domain-component", "auth", "-write-paths", "internal/auth",
	})
	if !errors.Is(err, errWorkflowWriteOverlap) {
		t.Fatalf("path overlap: %v", err)
	}
	err = cmdWorkflowInit([]string{
		"-root", root, "-module", "billing", "-goal-id", "billing-v1", "-goal", "bill",
		"-dir", dir, "-terminal-criteria", "c",
		"-write-domain-id", "billing-core", "-write-domain-lineage", "billing-core-lineage",
		"-write-domain-component", "billing", "-write-paths", "internal/billing",
		"-write-resources", "database:auth.primary",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = cmdWorkflowInit([]string{
		"-root", root, "-module", "search", "-goal-id", "search-v1", "-goal", "search",
		"-dir", t.TempDir(), "-terminal-criteria", "c",
		"-write-domain-id", "search-core", "-write-domain-lineage", "search-core-lineage",
		"-write-domain-component", "search", "-write-paths", "internal/search",
		"-write-resources", "database:auth.primary",
	})
	// search dir has no internal/search yet — create it
	if err == nil || !errors.Is(err, errWorkflowWriteOverlap) {
		other := t.TempDir()
		mustWriteFile(t, filepath.Join(other, "internal", "search", "s.go"), "package search\n")
		err = cmdWorkflowInit([]string{
			"-root", root, "-module", "search", "-goal-id", "search-v1", "-goal", "search",
			"-dir", other, "-terminal-criteria", "c",
			"-write-domain-id", "search-core", "-write-domain-lineage", "search-core-lineage",
			"-write-domain-component", "search", "-write-paths", "internal/search",
			"-write-resources", "database:auth.primary",
		})
		if !errors.Is(err, errWorkflowWriteOverlap) {
			t.Fatalf("resource overlap: %v", err)
		}
	}
}

func TestIntegrationGateHoldsUnknownConcernsBlockMissingAndPassWithFindings(t *testing.T) {
	root, dir := workflowTestRoot(t)
	wf := initTestWorkflow(t, root, dir)
	cfg, _ := loadConfig(root)
	_ = advanceWorkflow(root, cfg, wf)
	wf, _ = loadWorkflow(root, wf.ID)
	writer, _ := loadTask(root, wf.WriterTaskID)
	writer.Status = statusDone
	_ = saveTask(root, writer)
	freezeTestCandidate(t, root, wf.ID, "abc", "def")
	_ = advanceWorkflow(root, cfg, wf)
	wf, _ = loadWorkflow(root, wf.ID)
	review, _ := loadTask(root, wf.ReviewerTaskID)

	cases := []struct {
		name, body, want string
	}{
		{name: "missing", body: "", want: holdReasonMissingOutput},
		{name: "unknown", body: "```json\n{\"verdict\":\"ACCEPT\",\"p0\":[],\"p1\":[]}\n```", want: holdReasonUnknownVocabulary},
		{name: "concerns", body: reviewReport, want: holdReasonConcerns},
		{name: "block", body: "```json\n{\"verdict\":\"block\",\"p0\":[\"x\"],\"p1\":[]}\n```", want: holdReasonBlock},
		{name: "pass-with-findings", body: "```json\n{\"verdict\":\"pass\",\"p0\":[\"still a p0\"],\"p1\":[]}\n```", want: holdReasonIncompleteEvidence},
	}
	integ, _ := loadTask(root, wf.IntegrationTaskID)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			review.Status = statusDone
			_ = saveTask(root, review)
			writeReviewLog(t, root, review.ID, tc.body)
			dec := evaluateIntegrationRelease(root, integ)
			if dec.Admit || dec.HoldReason != tc.want {
				t.Fatalf("admit=%v reason=%q want %q", dec.Admit, dec.HoldReason, tc.want)
			}
			if err := cmdSetStatus([]string{"-root", root, integ.ID}, "release"); err == nil {
				t.Fatal("cardex release must refuse")
			}
		})
	}
}

func TestAdmissiblePassReleasesOnlyWithMatchingCandidateAndCustody(t *testing.T) {
	root, dir := workflowTestRoot(t)
	wf := initTestWorkflow(t, root, dir)
	cfg, _ := loadConfig(root)
	_ = advanceWorkflow(root, cfg, wf)
	wf, _ = loadWorkflow(root, wf.ID)
	writer, _ := loadTask(root, wf.WriterTaskID)
	writer.Status = statusDone
	_ = saveTask(root, writer)
	freezeTestCandidate(t, root, wf.ID, "c1", "t1")
	_ = advanceWorkflow(root, cfg, wf)
	wf, _ = loadWorkflow(root, wf.ID)
	review, _ := loadTask(root, wf.ReviewerTaskID)
	review.Status = statusDone
	review.PreferRunner = workflowEngineGrokBuild
	_ = saveTask(root, review)
	writeReviewLog(t, root, review.ID, passReport("c1", "t1"))
	if err := ingestWorkflowReview(root, wf); err != nil {
		t.Fatal(err)
	}
	wf, _ = loadWorkflow(root, wf.ID)
	if wf.Status != workflowStatusLiveReady || wf.EffectGates.Integration != effectGateHeld {
		t.Fatalf("live-ready must still hold integration: %+v", wf)
	}
	if wf.MaterialNotify == nil || wf.MaterialNotify.Kind != rootNotifyLiveReady {
		t.Fatalf("root notify: %+v", wf.MaterialNotify)
	}
	if err := tryReleaseWorkflowIntegration(root, wf); err != nil {
		t.Fatal(err)
	}
	integ, _ := loadTask(root, wf.IntegrationTaskID)
	if integ.Status != statusQueued {
		t.Fatalf("released integration status=%s", integ.Status)
	}
	wf, _ = loadWorkflow(root, wf.ID)
	if wf.EffectGates.Integration != effectGateReleased || wf.EffectGates.Live != effectGateHeld {
		t.Fatalf("live must remain held: %+v", wf.EffectGates)
	}
}

func TestMismatchedCandidateHolds(t *testing.T) {
	root, dir := workflowTestRoot(t)
	wf := initTestWorkflow(t, root, dir)
	cfg, _ := loadConfig(root)
	_ = advanceWorkflow(root, cfg, wf)
	wf, _ = loadWorkflow(root, wf.ID)
	writer, _ := loadTask(root, wf.WriterTaskID)
	writer.Status = statusDone
	_ = saveTask(root, writer)
	freezeTestCandidate(t, root, wf.ID, "c1", "t1")
	_ = advanceWorkflow(root, cfg, wf)
	wf, _ = loadWorkflow(root, wf.ID)
	review, _ := loadTask(root, wf.ReviewerTaskID)
	review.Status = statusDone
	_ = saveTask(root, review)
	writeReviewLog(t, root, review.ID, passReport("c1", "t1"))
	integ, _ := loadTask(root, wf.IntegrationTaskID)
	integ.IntegrationGate.CandidateCommit = "other"
	integ.IntegrationGate.CandidateTree = "t1"
	_ = saveTask(root, integ)
	dec := evaluateIntegrationRelease(root, integ)
	if dec.Admit || dec.HoldReason != holdReasonCandidateMismatch {
		t.Fatalf("mismatch: %+v", dec)
	}
}

func TestRepairThenExhaustedNotifiesRoot(t *testing.T) {
	root, dir := workflowTestRoot(t)
	wf := initTestWorkflow(t, root, dir)
	cfg, _ := loadConfig(root)
	_ = advanceWorkflow(root, cfg, wf)
	wf, _ = loadWorkflow(root, wf.ID)
	writer, _ := loadTask(root, wf.WriterTaskID)
	writer.Status = statusDone
	_ = saveTask(root, writer)
	freezeTestCandidate(t, root, wf.ID, "c1", "t1")
	_ = advanceWorkflow(root, cfg, wf)
	wf, _ = loadWorkflow(root, wf.ID)
	review, _ := loadTask(root, wf.ReviewerTaskID)
	review.Status = statusDone
	_ = saveTask(root, review)
	writeReviewLog(t, root, review.ID, reviewReport)
	if err := ingestWorkflowReview(root, wf); err != nil {
		t.Fatal(err)
	}
	wf, _ = loadWorkflow(root, wf.ID)
	if _, err := admitWorkflowRepair(root, cfg, wf, "P0-1: x\n", "bad", "concerns"); err != nil {
		t.Fatal(err)
	}
	wf, _ = loadWorkflow(root, wf.ID)
	if wf.CurrentRound != 1 || wf.Status != workflowStatusRepairing {
		t.Fatalf("repair: %+v", wf)
	}
	writer2, _ := loadTask(root, wf.WriterTaskID)
	writer2.Status = statusDone
	_ = saveTask(root, writer2)
	_ = advanceWorkflow(root, cfg, wf)
	wf, _ = loadWorkflow(root, wf.ID)
	review2, _ := loadTask(root, wf.ReviewerTaskID)
	review2.Status = statusDone
	_ = saveTask(root, review2)
	writeReviewLog(t, root, review2.ID, reviewReport)
	_ = ingestWorkflowReview(root, wf)
	wf, _ = loadWorkflow(root, wf.ID)
	if _, err := admitWorkflowRepair(root, cfg, wf, "P0-1: still\n", "bad", "block"); err != nil {
		t.Fatal(err)
	}
	wf, _ = loadWorkflow(root, wf.ID)
	writer3, _ := loadTask(root, wf.WriterTaskID)
	writer3.Status = statusDone
	_ = saveTask(root, writer3)
	_ = advanceWorkflow(root, cfg, wf)
	wf, _ = loadWorkflow(root, wf.ID)
	review3, _ := loadTask(root, wf.ReviewerTaskID)
	review3.Status = statusDone
	_ = saveTask(root, review3)
	writeReviewLog(t, root, review3.ID, reviewReport)
	_ = ingestWorkflowReview(root, wf)
	wf, _ = loadWorkflow(root, wf.ID)
	_, err := admitWorkflowRepair(root, cfg, wf, "P0-1: still\n", "bad", "block")
	if err == nil {
		t.Fatal("round 3 must exhaust")
	}
	wf, _ = loadWorkflow(root, wf.ID)
	if wf.Status != workflowStatusExhausted || wf.MaterialNotify == nil || wf.MaterialNotify.Kind != rootNotifyExhausted {
		t.Fatalf("exhausted notify: %+v", wf)
	}
}

func TestRoutineProgressDoesNotNotifyRoot(t *testing.T) {
	root, dir := workflowTestRoot(t)
	wf := initTestWorkflow(t, root, dir)
	cfg, _ := loadConfig(root)
	_ = advanceWorkflow(root, cfg, wf)
	wf, _ = loadWorkflow(root, wf.ID)
	if wf.MaterialNotify != nil {
		t.Fatal("writer admit is not a Root hook")
	}
	entries, _ := os.ReadDir(workflowNotifyDir(root))
	if len(entries) != 0 {
		t.Fatalf("notify dir: %v", entries)
	}
}

func TestFederatedChildBindsParent(t *testing.T) {
	root, dir := workflowTestRoot(t)
	parent := initTestWorkflow(t, root, dir)
	childDir := t.TempDir()
	mustWriteFile(t, filepath.Join(childDir, "internal", "search", "s.go"), "package search\n")
	if err := cmdWorkflowInit([]string{
		"-root", root, "-mode", workflowModeFederated, "-module", "search",
		"-goal-id", "search-v1", "-goal", "search", "-parent", parent.ID,
		"-dir", childDir, "-terminal-criteria", "c",
		"-write-domain-id", "search-core", "-write-domain-lineage", "search-core-lineage",
		"-write-domain-component", "search", "-write-paths", "internal/search",
	}); err != nil {
		t.Fatal(err)
	}
	wfs, _ := loadWorkflows(root)
	var child *WorkflowRecord
	for _, wf := range wfs {
		if wf.ModuleID == "search" {
			child = wf
		}
	}
	if child == nil || child.ParentID != parent.ID || child.Mode != workflowModeFederated {
		t.Fatalf("child: %+v", child)
	}
}

func TestCmdAddPersistsWriteDomainAndDependsOnSecretFree(t *testing.T) {
	root := testRoot(t)
	if err := saveConfig(root, defaultConfig("claude")); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "internal", "auth"), 0o755); err != nil {
		t.Fatal(err)
	}
	dep := newTask(root, testCfg(), typeSequence, "dep", dir, []string{"p"}, 5)
	if err := saveTask(root, dep); err != nil {
		t.Fatal(err)
	}
	if err := cmdAdd([]string{
		"-root", root, "-dir", dir, "-title", "auth lane",
		"-write-domain-id", "auth-tokens",
		"-write-domain-lineage", "auth-tokens-lineage",
		"-write-domain-component", "auth",
		"-write-paths", "internal/auth",
		"-write-resources", "database:auth.primary",
		"-depends-on", dep.ID,
		"SECRET PROMPT TOKEN=abc",
	}); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return cmdList([]string{"-root", root, "-json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var tasks []Task
	if err := json.Unmarshal([]byte(out), &tasks); err != nil {
		t.Fatalf("list json: %v %s", err, out)
	}
	var got *Task
	for i := range tasks {
		if tasks[i].WriteDomain != nil {
			got = &tasks[i]
			break
		}
	}
	if got == nil || got.WriteDomain.ID != "auth-tokens" || len(got.DependsOn) != 1 || got.DependsOn[0] != dep.ID {
		t.Fatalf("persisted claims missing: %+v", tasks)
	}
	blob, _ := json.Marshal(got.WriteDomain)
	if bytes.Contains(bytes.ToLower(blob), []byte("secret")) || bytes.Contains(bytes.ToLower(blob), []byte("token=abc")) {
		t.Fatalf("write domain leaked prompt: %s", blob)
	}
}

func finishTickTask(root string, tk *Task) {
	tk.Status = statusDone
	tk.touch()
	_ = saveTask(root, tk)
}

func TestTickEnforcesDisjointLanesAndLegacySerial(t *testing.T) {
	orig := tickRunTask
	t.Cleanup(func() { tickRunTask = orig })

	overlapTick := func(root string, cfg *Config, hold time.Duration) int32 {
		var concurrent, maxC atomic.Int32
		tickRunTask = func(ctx context.Context, root string, cfg *Config, tk *Task, via string) error {
			n := concurrent.Add(1)
			for {
				old := maxC.Load()
				if n <= old || maxC.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(hold)
			concurrent.Add(-1)
			finishTickTask(root, tk)
			return nil
		}
		if err := tick(root, cfg, true, true); err != nil {
			t.Fatal(err)
		}
		return maxC.Load()
	}

	root := testRoot(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "internal", "auth"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "internal", "billing"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig("claude")
	cfg.MaxParallel = 2
	cfg.DrainRescanSec = 1
	cfg.ClaudeBin = fakeClaudeBin(t, mkOKResultJSON("sess"), "", 0)
	_ = explicitTask(t, root, "auth", dir, "auth-tokens", "auth-tokens-lineage", "auth", []string{"internal/auth"}, nil)
	_ = explicitTask(t, root, "bill", dir, "billing-core", "billing-core-lineage", "billing", []string{"internal/billing"}, nil)
	if got := overlapTick(root, cfg, 80*time.Millisecond); got < 2 {
		t.Fatalf("disjoint explicit lanes must overlap in tick, concurrent=%d", got)
	}

	root2 := testRoot(t)
	same := t.TempDir()
	legacy := newTask(root2, testCfg(), typeSequence, "legacy a", same, []string{"p"}, 5)
	if err := saveTask(root2, legacy); err != nil {
		t.Fatal(err)
	}
	legacyB := newTask(root2, testCfg(), typeSequence, "legacy b", same, []string{"p"}, 5)
	if err := saveTask(root2, legacyB); err != nil {
		t.Fatal(err)
	}
	if got := overlapTick(root2, cfg, 80*time.Millisecond); got != 1 {
		t.Fatalf("legacy same-dir must serialize in tick, concurrent=%d", got)
	}
}

func TestTickDoesNotFailOpenGrokBuildToClaude(t *testing.T) {
	orig := tickRunTask
	t.Cleanup(func() { tickRunTask = orig })
	var ran atomic.Int32
	tickRunTask = func(ctx context.Context, root string, cfg *Config, tk *Task, via string) error {
		ran.Add(1)
		if via == "" || via == "claude" {
			t.Errorf("grok-build must not fail open to claude, via=%q", via)
		}
		finishTickTask(root, tk)
		return nil
	}
	root := testRoot(t)
	cfg := defaultConfig(fakeClaudeBin(t, mkOKResultJSON("s"), "", 0))
	tk := newTask(root, cfg, typeSequence, "grok writer", t.TempDir(), []string{"p"}, 5)
	tk.PreferRunner = workflowEngineGrokBuild
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if err := tick(root, cfg, true, true); err != nil {
		t.Fatal(err)
	}
	fresh, _ := loadTask(root, tk.ID)
	if fresh.Status != statusQueued {
		t.Fatalf("unconfigured grok-build must wait, status=%s", fresh.Status)
	}
	if ran.Load() != 0 {
		t.Fatalf("ran=%d", ran.Load())
	}
}

func TestTickHoldsIntegrationWithoutAdmissiblePass(t *testing.T) {
	orig := tickRunTask
	t.Cleanup(func() { tickRunTask = orig })
	tickRunTask = func(ctx context.Context, root string, cfg *Config, tk *Task, via string) error {
		t.Errorf("integration must not run, %s", tk.ID)
		finishTickTask(root, tk)
		return nil
	}
	root, dir := workflowTestRoot(t)
	wf := initTestWorkflow(t, root, dir)
	integ, _ := loadTask(root, wf.IntegrationTaskID)
	integ.Status = statusQueued
	_ = saveTask(root, integ)
	cfg := defaultConfig(fakeClaudeBin(t, mkOKResultJSON("s"), "", 0))
	if err := tick(root, cfg, true, true); err != nil {
		t.Fatal(err)
	}
	fresh, _ := loadTask(root, integ.ID)
	if fresh.Status != statusQueued {
		t.Fatalf("status=%s", fresh.Status)
	}
}

func TestHandleReviewVerdictPassWithFindingsDoesNotCloseout(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	impl := mkImplTask(t, root, cfg)
	impl.Closeout = "write done"
	_ = saveTask(root, impl)
	rv := mkReviewTask(t, root, cfg, impl)
	handleReviewVerdict(root, cfg, rv, "```json\n{\"verdict\":\"pass\",\"p0\":[\"still\"],\"p1\":[]}\n```", nil)
	for _, x := range listQueued(t, root) {
		if strings.HasPrefix(x.Title, "收口:") {
			t.Fatalf("pass-with-findings must not closeout: %+v", x)
		}
	}
}

func TestReviewVerdictAdmissiblePass(t *testing.T) {
	if reviewVerdictIsAdmissiblePass(&reviewVerdict{Verdict: "pass", P0: []string{"x"}}) {
		t.Fatal("pass with p0 is not admissible")
	}
	if !reviewVerdictIsAdmissiblePass(&reviewVerdict{Verdict: "pass"}) {
		t.Fatal("empty p0/p1 pass is admissible")
	}
	if reviewVerdictIsAdmissiblePass(parseReviewVerdict("```json\n{\"verdict\":\"ACCEPT\"}\n```")) {
		t.Fatal("ACCEPT is unknown vocabulary")
	}
}
