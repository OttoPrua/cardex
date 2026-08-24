package main

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// W2 acceptance fixtures (docs/workflows.md): the failure fixture reproduces
// "attempt record exited, but producer/children/lease still live, then
// same-card redispatch"; the recovery fixture allows only the supported hold
// to revoke scheduling/attempt, waits for the exact producers to disappear,
// completes the quiet window, and still keeps the review un-adopted. The two
// fixtures are paired deliberately: refusing the drift and proving the
// containment boundary are different obligations, and testing only "hold
// eventually works" would leave the refusal side unproven.

// custodyProbeStub replaces the live-process probes with deterministic state.
// The incident fixtures never spawn or signal real processes.
type custodyProbeStub struct {
	aliveAttempts map[string]bool
	taskAlive     bool
	residue       bool
	lease         bool
}

func stubCustodyProbes(t *testing.T) *custodyProbeStub {
	t.Helper()
	s := &custodyProbeStub{aliveAttempts: map[string]bool{}}
	orig := reviewCustodyProbes
	reviewCustodyProbes = custodyProbes{
		attemptProducerAlive: func(rec *AttemptRecord) bool {
			return rec != nil && s.aliveAttempts[rec.AttemptID]
		},
		taskProcsAlive:     func(string) bool { return s.taskAlive },
		taskResidue:        func(string) bool { return s.residue },
		workspaceLeaseHeld: func(string) bool { return s.lease },
	}
	t.Cleanup(func() { reviewCustodyProbes = orig })
	return s
}

func writeReviewAttempt(t *testing.T, root, taskID, attemptID, state string, createdAt time.Time) *AttemptRecord {
	t.Helper()
	rec := &AttemptRecord{
		TaskID:           taskID,
		AttemptID:        attemptID,
		ExpectedRevision: 1,
		ControlEpoch:     1,
		PID:              424242,
		PGID:             424242,
		StartIdentity:    "fixture-start-identity",
		State:            state,
		CreatedAt:        createdAt.Format(time.RFC3339Nano),
	}
	if err := writeAttempt(root, rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func writeCommittedDoneTransition(t *testing.T, root, taskID, transitionID, attemptID string, createdAt time.Time) {
	t.Helper()
	rec := &TransitionRecord{
		TransitionID:     transitionID,
		TaskID:           taskID,
		ExpectedRevision: 1,
		NewRevision:      2,
		ControlEpoch:     1,
		AttemptID:        attemptID,
		EventType:        evDone,
		Status:           statusDone,
		State:            transitionCommitted,
		CreatedAt:        createdAt.Format(time.RFC3339Nano),
	}
	if err := writeTransition(root, rec); err != nil {
		t.Fatal(err)
	}
}

// backdateCustodyWindow simulates elapsed wall-clock time by moving the
// durable first-observation timestamp into the past. The 20-second window
// constant itself is never shrunk, so the production semantics stay pinned.
func backdateCustodyWindow(t *testing.T, root, taskID string, d time.Duration) {
	t.Helper()
	rec, err := loadCustodyRecord(root, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil {
		t.Fatal("no custody observation to backdate")
	}
	first, err := time.Parse(time.RFC3339Nano, rec.FirstObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	rec.FirstObservedAt = first.Add(-d).Format(time.RFC3339Nano)
	if err := writeCustodyRecord(root, rec); err != nil {
		t.Fatal(err)
	}
}

// completeReviewCustodyWindow drives the two explicit observations an
// admissible receipt requires: one to open the window, one after the window
// has (simulatedly) elapsed over unchanged evidence.
func completeReviewCustodyWindow(t *testing.T, root string, cfg *Config, wf *WorkflowRecord) {
	t.Helper()
	if err := ingestWorkflowReview(root, cfg, wf); err != nil {
		t.Fatal(err)
	}
	backdateCustodyWindow(t, root, wf.ReviewerTaskID, custodyQuietWindow+time.Second)
	if err := ingestWorkflowReview(root, cfg, wf); err != nil {
		t.Fatal(err)
	}
}

func rootNotifyCount(t *testing.T, root string) int {
	t.Helper()
	entries, err := os.ReadDir(workflowNotifyDir(root))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return len(entries)
}

// ---- failure fixture ----

// TestCustodyFailureFixtureExitedButProducerLiveThenRedispatch is the failure
// side of the W2 pair: the attempt record says `exited`, the exact producer is
// still observably alive, and a successor attempt then appears on the same
// card. Every adoption path must refuse: ingest must not mark the pass
// admissible, tick and `cardex release` must hold the integration card, and no
// quiet-window receipt of any kind may form while the drift is observable.
func TestCustodyFailureFixtureExitedButProducerLiveThenRedispatch(t *testing.T) {
	stub := stubCustodyProbes(t)
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)
	wf, review := runWorkflowToReview(t, root, wf, verdictJSON("pass", nil, nil))

	// The incident: the reviewer attempt closed as `exited`, but the exact
	// PID/PGID producer is still alive.
	writeReviewAttempt(t, root, review.ID, "at-fixture-1", attemptExited, time.Now().Add(-time.Minute))
	stub.aliveAttempts["at-fixture-1"] = true

	if err := ingestWorkflowReview(root, cfg, wf); err != nil {
		t.Fatal(err)
	}
	if wf.Review == nil || wf.Review.Admissible || wf.Review.HoldReason != holdReasonCustodyDrift {
		t.Fatalf("exited-but-alive must not be adoptable: %+v; 反例注入: reviewCustodyDrift 里删掉 attemptProducerAlive 检查", wf.Review)
	}
	if rec, err := loadCustodyRecord(root, review.ID); err != nil || rec == nil || rec.Kind != "" || rec.Drift == "" {
		t.Fatalf("drift must leave a reset observation, never a receipt: %+v err=%v", rec, err)
	}

	integ, err := loadTask(root, wf.IntegrationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if dec := evaluateIntegrationRelease(root, cfg, integ); dec.Admit || dec.HoldReason != holdReasonCustodyDrift {
		t.Fatalf("gate must hold on custody drift: admit=%v reason=%q", dec.Admit, dec.HoldReason)
	}
	if integrationGateAllows(root, cfg, integ) {
		t.Fatal("tick must not dispatch while the review producer is alive")
	}
	if err := tryReleaseWorkflowIntegration(root, cfg, wf); !errors.Is(err, errWorkflowHeld) {
		t.Fatalf("try-release must refuse: %v", err)
	}
	if err := cmdSetStatus([]string{"-root", root, integ.ID}, "release"); err == nil {
		t.Fatal("cardex release must refuse while custody is broken")
	}

	// The redispatch: a second attempt appears on the same card. Even with the
	// first producer now gone, an open successor attempt is its own drift.
	stub.aliveAttempts["at-fixture-1"] = false
	writeReviewAttempt(t, root, review.ID, "at-fixture-2", attemptReserved, time.Now())
	if drift := reviewCustodyDrift(root, review); drift == "" || !strings.Contains(drift, "open") {
		t.Fatalf("an open successor attempt is custody drift: %q; 反例注入: reviewCustodyDrift 里删掉 reserved/bound 计数", drift)
	}
	if err := ingestWorkflowReview(root, cfg, wf); err != nil {
		t.Fatal(err)
	}
	if wf.Review.Admissible || wf.Review.HoldReason != holdReasonCustodyDrift {
		t.Fatalf("redispatch must not become adoptable: %+v", wf.Review)
	}

	// Backdating cannot smuggle a window past live drift: every drifted
	// observation resets the window instead of aging it.
	if err := ingestWorkflowReview(root, cfg, wf); err != nil {
		t.Fatal(err)
	}
	if rec, _ := loadCustodyRecord(root, review.ID); rec == nil || rec.Kind != "" {
		t.Fatalf("no receipt may form under drift: %+v", rec)
	}

	after, err := loadTask(root, integ.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != statusHeld {
		t.Fatalf("integration must remain held, got %s", after.Status)
	}
	if n := rootNotifyCount(t, root); n != 0 {
		t.Fatalf("a drifted review must never notify Root of a pass: %d receipts", n)
	}
}

// TestCustodyEveryProducerGoneComponentFailsClosed proves `exited` is not
// `producerGone`: each disproof channel — exact PID/PGID identity, registered
// descendants, runner residue, and the workspace lease — independently holds
// the gate closed.
func TestCustodyEveryProducerGoneComponentFailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		inject func(s *custodyProbeStub)
	}{
		{"exact pid/pgid identity still alive", func(s *custodyProbeStub) { s.aliveAttempts["at-fixture-1"] = true }},
		{"registered descendant still alive", func(s *custodyProbeStub) { s.taskAlive = true }},
		{"runner residue remains", func(s *custodyProbeStub) { s.residue = true }},
		{"workspace lease still held", func(s *custodyProbeStub) { s.lease = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := stubCustodyProbes(t)
			root, dir := workflowTestRoot(t)
			cfg := workflowTestCfg(t, root)
			wf := initTestWorkflow(t, root, dir)
			wf, review := runWorkflowToReview(t, root, wf, verdictJSON("pass", nil, nil))
			writeReviewAttempt(t, root, review.ID, "at-fixture-1", attemptExited, time.Now().Add(-time.Minute))

			// Positive control first: with everything gone the same evidence
			// completes a window and admits, so each failing leg below fails
			// for its injected reason and not for an unrelated one.
			completeReviewCustodyWindow(t, root, cfg, wf)
			if !wf.Review.Admissible {
				t.Fatalf("clean custody baseline must admit: %+v", wf.Review)
			}

			tc.inject(stub)
			integ, err := loadTask(root, wf.IntegrationTaskID)
			if err != nil {
				t.Fatal(err)
			}
			if dec := evaluateIntegrationRelease(root, cfg, integ); dec.Admit || dec.HoldReason != holdReasonCustodyDrift {
				t.Fatalf("%s must hold the gate: admit=%v reason=%q", tc.name, dec.Admit, dec.HoldReason)
			}
			if err := tryReleaseWorkflowIntegration(root, cfg, wf); !errors.Is(err, errWorkflowHeld) {
				t.Fatalf("try-release must refuse under %s: %v", tc.name, err)
			}
		})
	}
}

// TestCustodySuccessorAttemptAfterTerminalIsRedispatchDrift covers the closed
// successor: a second attempt minted after the terminal attempt breaks custody
// even when every process probe is already clean, because the transcript can
// no longer be attributed to the terminal attempt alone.
func TestCustodySuccessorAttemptAfterTerminalIsRedispatchDrift(t *testing.T) {
	_ = stubCustodyProbes(t)
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)
	wf, stale := runWorkflowToReview(t, root, wf, verdictJSON("pass", nil, nil))
	// Fresh-read the reviewer terminal; the admit-time copy predates `done`.
	review, err := loadTask(root, stale.ID)
	if err != nil {
		t.Fatal(err)
	}

	base := time.Now().Add(-time.Hour)
	writeReviewAttempt(t, root, review.ID, "at-terminal", attemptExited, base)
	writeCommittedDoneTransition(t, root, review.ID, "tr-terminal", "at-terminal", base.Add(time.Minute))

	// Positive control: terminal attempt alone is clean custody.
	if drift := reviewCustodyDrift(root, review); drift != "" {
		t.Fatalf("terminal attempt alone must be clean: %q", drift)
	}

	writeReviewAttempt(t, root, review.ID, "at-successor", attemptExited, base.Add(2*time.Minute))
	drift := reviewCustodyDrift(root, review)
	if drift == "" || !strings.Contains(drift, "successor") {
		t.Fatalf("a successor attempt after the terminal is redispatch drift: %q; 反例注入: successorAttemptDrift 里删掉 CreatedAt 比较", drift)
	}
	if err := ingestWorkflowReview(root, cfg, wf); err != nil {
		t.Fatal(err)
	}
	if wf.Review.Admissible || wf.Review.HoldReason != holdReasonCustodyDrift {
		t.Fatalf("spliced-attempt evidence must not be adoptable: %+v", wf.Review)
	}
	integ, err := loadTask(root, wf.IntegrationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if dec := evaluateIntegrationRelease(root, cfg, integ); dec.Admit || dec.HoldReason != holdReasonCustodyDrift {
		t.Fatalf("gate must hold: admit=%v reason=%q", dec.Admit, dec.HoldReason)
	}
}

// ---- recovery fixture ----

// TestCustodyRecoveryFixtureSupportedHoldReconcilesWithoutAdoptingReview is
// the paired recovery side: only the supported hold contains the card; the
// reconciliation window completes only after the producers are gone; and the
// resulting receipt proves containment only — it is not a semantic review
// receipt, the stale pass output stays rejected, the candidate stays
// unreviewed, and the held integration card stays held.
func TestCustodyRecoveryFixtureSupportedHoldReconcilesWithoutAdoptingReview(t *testing.T) {
	stub := stubCustodyProbes(t)
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)

	writer, err := admitWorkflowWriter(root, cfg, wf, "")
	if err != nil {
		t.Fatal(err)
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
	// The incident shape: the reviewer attempt closed as exited while the
	// producer is still observably alive, and its stale "pass" output already
	// sits in the transcript as redispatch bait.
	writeReviewAttempt(t, root, review.ID, "at-fixture-1", attemptExited, time.Now().Add(-time.Minute))
	stub.aliveAttempts["at-fixture-1"] = true
	writeReviewLog(t, root, review.ID, verdictJSON("pass", nil, nil))

	// Containment goes through the supported hold only. No test-side signaling
	// of processes, no manual attempt surgery.
	if err := terminalize(root, review.ID, statusHeld, "owner:containment", "custody drift containment", nil); err != nil {
		t.Fatal(err)
	}
	held, err := loadTask(root, review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if held.Status != statusHeld {
		t.Fatalf("supported hold must terminalize the reviewer: %s", held.Status)
	}

	// While the exact producer is still observable, reconciliation must not
	// progress: every observation resets on drift.
	if err := ingestWorkflowReview(root, cfg, wf); err != nil {
		t.Fatal(err)
	}
	if rec, _ := loadCustodyRecord(root, review.ID); rec == nil || rec.Kind != "" || rec.Drift == "" {
		t.Fatalf("reconciliation must wait for producer absence: %+v", rec)
	}

	// The producer and its one-shot copies disappear; only now may the
	// quiet window run, and it completes as containment, not as a review.
	stub.aliveAttempts["at-fixture-1"] = false
	if err := ingestWorkflowReview(root, cfg, wf); err != nil {
		t.Fatal(err)
	}
	backdateCustodyWindow(t, root, review.ID, custodyQuietWindow+time.Second)
	if err := ingestWorkflowReview(root, cfg, wf); err != nil {
		t.Fatal(err)
	}
	rec, err := loadCustodyRecord(root, review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil || rec.Kind != custodyKindReconciledHeld || rec.SemanticReview {
		t.Fatalf("containment yields a reconciled-held receipt only: %+v; 反例注入: observeReviewCustody 里给 statusHeld 也发 admissible_review", rec)
	}

	// The reconciled receipt is not a semantic review receipt: the stale pass
	// stays un-adopted, the candidate stays unreviewed, integration stays held.
	if wf.Review == nil || wf.Review.Admissible {
		t.Fatalf("a contained review must stay un-adopted: %+v", wf.Review)
	}
	if reason := reviewCustodyReceiptReason(root, held, commit, tree); reason != holdReasonCustodyReceipt {
		t.Fatalf("a reconciled receipt must be refused as non-semantic: %q; 反例注入: reviewCustodyReceiptReason 里接受 custodyKindReconciledHeld", reason)
	}
	integ, err := loadTask(root, wf.IntegrationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if dec := evaluateIntegrationRelease(root, cfg, integ); dec.Admit {
		t.Fatalf("integration must stay held after containment: %+v", dec)
	}
	if integ.Status != statusHeld {
		t.Fatalf("integration card status=%s", integ.Status)
	}
	if err := tryReleaseWorkflowIntegration(root, cfg, wf); !errors.Is(err, errWorkflowHeld) {
		t.Fatalf("try-release must still refuse: %v", err)
	}
	if n := rootNotifyCount(t, root); n != 0 {
		t.Fatalf("containment is not a review pass and must not notify Root: %d receipts", n)
	}
	// Whether to admit a fresh reviewer afterwards is a new manager decision;
	// nothing here may have minted one implicitly.
	tasks, err := loadTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	reviewers := 0
	for _, tk := range tasks {
		if tk.WorkflowID == wf.ID && tk.Type == typeReview {
			reviewers++
		}
	}
	if reviewers != 1 {
		t.Fatalf("containment must not mint a replacement reviewer: %d", reviewers)
	}
}

// ---- quiet-window semantics ----

// TestCustodyQuietWindowResetsWheneverEvidenceMoves proves the terminal
// quiet-window contract: task/event/attempt/process/source hashes must be
// stable across the whole window, so late-arriving output (old-output splice)
// or any record churn restarts the count instead of being absorbed.
func TestCustodyQuietWindowResetsWheneverEvidenceMoves(t *testing.T) {
	_ = stubCustodyProbes(t)
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)
	wf, review := runWorkflowToReview(t, root, wf, verdictJSON("pass", nil, nil))

	if err := ingestWorkflowReview(root, cfg, wf); err != nil {
		t.Fatal(err)
	}
	if wf.Review.Admissible || wf.Review.HoldReason != holdReasonCustodyWindow {
		t.Fatalf("first observation only opens the window: %+v; 反例注入: observeReviewCustody 里首次观察即发收据", wf.Review)
	}
	before, err := loadCustodyRecord(root, review.ID)
	if err != nil || before == nil {
		t.Fatalf("observation missing: %v", err)
	}

	// Old-output splice: late text lands in the transcript mid-window.
	writeReviewLog(t, root, review.ID, verdictJSON("pass", nil, nil)+"\nlate-arriving splice\n")
	backdateCustodyWindow(t, root, review.ID, custodyQuietWindow+time.Second)
	if err := ingestWorkflowReview(root, cfg, wf); err != nil {
		t.Fatal(err)
	}
	if wf.Review.Admissible {
		t.Fatal("spliced output must not ride the previous window; 反例注入: custodyEvidenceHash 里去掉 transcript 摘要")
	}
	after, err := loadCustodyRecord(root, review.ID)
	if err != nil || after == nil {
		t.Fatalf("observation missing after splice: %v", err)
	}
	if after.Kind != "" || after.EvidenceHash == before.EvidenceHash {
		t.Fatalf("splice must restart the window with a new hash: %+v", after)
	}

	// Attempt-record churn mid-window resets too.
	backdateCustodyWindow(t, root, review.ID, custodyQuietWindow+time.Second)
	writeReviewAttempt(t, root, review.ID, "at-late", attemptExited, time.Now())
	if err := ingestWorkflowReview(root, cfg, wf); err != nil {
		t.Fatal(err)
	}
	if wf.Review.Admissible {
		t.Fatal("attempt churn must not ride the previous window")
	}

	// Stability over a full window finally admits: the reset is a latch, not a
	// one-way trap.
	completeReviewCustodyWindow(t, root, cfg, wf)
	if !wf.Review.Admissible {
		t.Fatalf("stable evidence over a full window must admit: %+v", wf.Review)
	}
	integ, err := loadTask(root, wf.IntegrationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if dec := evaluateIntegrationRelease(root, cfg, integ); !dec.Admit {
		t.Fatalf("gate must admit the completed receipt: %q", dec.HoldReason)
	}
}

// TestCustodyReceiptGoesStaleWhenEvidenceMovesAfterCompletion: a completed
// receipt is bound to the exact evidence hash it observed. If the evidence
// moves later, the gate re-derivation catches the mismatch, the released card
// is re-held through the supported path, and only a fresh window re-admits.
func TestCustodyReceiptGoesStaleWhenEvidenceMovesAfterCompletion(t *testing.T) {
	_ = stubCustodyProbes(t)
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)
	wf, review := runWorkflowToReview(t, root, wf, verdictJSON("pass", nil, nil))

	completeReviewCustodyWindow(t, root, cfg, wf)
	if err := tryReleaseWorkflowIntegration(root, cfg, wf); err != nil {
		t.Fatal(err)
	}

	// The transcript changes after release — still a parseable pass, but no
	// longer the bytes the receipt observed.
	writeReviewLog(t, root, review.ID, verdictJSON("pass", nil, nil)+"\npost-receipt tail\n")
	integ, err := loadTask(root, wf.IntegrationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if dec := evaluateIntegrationRelease(root, cfg, integ); dec.Admit || dec.HoldReason != holdReasonCustodyWindow {
		t.Fatalf("moved evidence must invalidate the receipt: admit=%v reason=%q; 反例注入: reviewCustodyReceiptReason 里不再比对 EvidenceHash", dec.Admit, dec.HoldReason)
	}
	if err := tryReleaseWorkflowIntegration(root, cfg, wf); !errors.Is(err, errWorkflowHeld) {
		t.Fatalf("try-release must re-hold: %v", err)
	}
	reheld, err := loadTask(root, wf.IntegrationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if reheld.Status != statusHeld {
		t.Fatalf("re-held status=%s", reheld.Status)
	}

	// A fresh window over the new evidence re-admits.
	completeReviewCustodyWindow(t, root, cfg, wf)
	if err := tryReleaseWorkflowIntegration(root, cfg, wf); err != nil {
		t.Fatalf("fresh window must re-admit: %v", err)
	}
}

// TestCustodyObservationReplayIsStable: replaying ingest after a completed
// receipt neither regresses admissibility nor rewrites the receipt, and there
// is exactly one custody file per review task.
func TestCustodyObservationReplayIsStable(t *testing.T) {
	_ = stubCustodyProbes(t)
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)
	wf, review := runWorkflowToReview(t, root, wf, verdictJSON("pass", nil, nil))

	completeReviewCustodyWindow(t, root, cfg, wf)
	first, err := loadCustodyRecord(root, review.ID)
	if err != nil || first == nil || first.Kind != custodyKindAdmissibleReview {
		t.Fatalf("receipt: %+v err=%v", first, err)
	}
	for i := 0; i < 3; i++ {
		if err := ingestWorkflowReview(root, cfg, wf); err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
		if !wf.Review.Admissible {
			t.Fatalf("replay %d regressed admissibility: %+v", i, wf.Review)
		}
	}
	again, err := loadCustodyRecord(root, review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.CompletedAt != first.CompletedAt || again.FirstObservedAt != first.FirstObservedAt ||
		again.EvidenceHash != first.EvidenceHash {
		t.Fatalf("replay must not rewrite a completed receipt: %+v vs %+v", again, first)
	}
	entries, err := os.ReadDir(custodyDir(root))
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			files++
		}
	}
	if files != 1 {
		t.Fatalf("one custody file per review task, got %d", files)
	}
}

// TestCustodyGateStaysReadOnly: consulting the gate never opens or advances a
// quiet window; only explicit ingest observations write custody state. This is
// the tick-must-not-advance-workflow boundary applied to W2.
func TestCustodyGateStaysReadOnly(t *testing.T) {
	_ = stubCustodyProbes(t)
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)
	wf, review := runWorkflowToReview(t, root, wf, verdictJSON("pass", nil, nil))

	integ, err := loadTask(root, wf.IntegrationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if dec := evaluateIntegrationRelease(root, cfg, integ); dec.Admit || dec.HoldReason != holdReasonCustodyWindow {
			t.Fatalf("gate consult %d: admit=%v reason=%q", i, dec.Admit, dec.HoldReason)
		}
	}
	if rec, err := loadCustodyRecord(root, review.ID); err != nil || rec != nil {
		t.Fatalf("gate consults must not write custody observations: %+v err=%v; 反例注入: reviewCustodyReceiptReason 里调用 observeReviewCustody", rec, err)
	}
}
