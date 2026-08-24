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

// reviewFixtureAttemptID is the producing attempt runWorkflowToReview binds
// the reviewer terminal to; its committed done transition names this ID.
const reviewFixtureAttemptID = "at-producer-fixture"

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
		{"exact pid/pgid identity still alive", func(s *custodyProbeStub) { s.aliveAttempts[reviewFixtureAttemptID] = true }},
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
			wf, _ = runWorkflowToReview(t, root, wf, verdictJSON("pass", nil, nil))

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
	// The bare variant: this fixture writes its own terminal attempt evidence
	// with controlled timestamps.
	wf, stale := runWorkflowToBareReview(t, root, wf, verdictJSON("pass", nil, nil))
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

	// Attempt-record churn mid-window resets too. The late-surfacing record is
	// backdated before the terminal attempt so it is churn (a hash move), not
	// a successor: redispatch drift is pinned by its own fixtures.
	backdateCustodyWindow(t, root, review.ID, custodyQuietWindow+time.Second)
	writeReviewAttempt(t, root, review.ID, "at-late", attemptExited, time.Now().Add(-2*time.Hour))
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

// ---- P1 repairs from the 2026-08-24 independent review ----

// bareReviewForCustody drives a workflow to a done reviewer with a pass
// verdict but no attempt/transition evidence, and fresh-reads the terminal.
func bareReviewForCustody(t *testing.T) (root string, cfg *Config, wf *WorkflowRecord, review *Task) {
	t.Helper()
	root, dir := workflowTestRoot(t)
	cfg = workflowTestCfg(t, root)
	wf = initTestWorkflow(t, root, dir)
	wf, stale := runWorkflowToBareReview(t, root, wf, verdictJSON("pass", nil, nil))
	review, err := loadTask(root, stale.ID)
	if err != nil {
		t.Fatal(err)
	}
	return root, cfg, wf, review
}

// TestCustodySuccessorOrderingIsChronologicalNotLexicographic (P1-1): attempt
// stamps are RFC3339Nano with the writer's local offset and trimmed fractional
// zeros, so raw string `>` is not wall-clock order. A successor minted minutes
// after the terminal attempt must be drift no matter how the two stamps are
// encoded — a launchd job and an interactive shell routinely write the same
// data root with different offsets, and DST alone flips one twice a year.
func TestCustodySuccessorOrderingIsChronologicalNotLexicographic(t *testing.T) {
	cases := []struct {
		name      string
		terminal  time.Time
		successor time.Time
	}{
		{
			// 17:00+08:00 is 09:00Z; the successor lands five real minutes
			// later but its UTC stamp compares *smaller* as a string.
			name:      "mixed utc offsets",
			terminal:  time.Date(2026, 8, 24, 17, 0, 0, 1, time.FixedZone("UTC+8", 8*3600)),
			successor: time.Date(2026, 8, 24, 9, 5, 0, 1, time.UTC),
		},
		{
			// RFC3339Nano trims trailing zeros: ".5Z" compares larger than the
			// 100µs-later ".5001Z" because byte 'Z' > byte '0'.
			name:      "fractional second width",
			terminal:  time.Date(2026, 8, 24, 9, 0, 0, 500000000, time.UTC),
			successor: time.Date(2026, 8, 24, 9, 0, 0, 500100000, time.UTC),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_ = stubCustodyProbes(t)
			root, cfg, wf, review := bareReviewForCustody(t)
			writeReviewAttempt(t, root, review.ID, "at-terminal", attemptExited, tc.terminal)
			writeCommittedDoneTransition(t, root, review.ID, "tr-terminal", "at-terminal", tc.terminal)
			if drift := reviewCustodyDrift(root, review); drift != "" {
				t.Fatalf("terminal attempt alone must be clean: %q", drift)
			}

			writeReviewAttempt(t, root, review.ID, "at-successor", attemptExited, tc.successor)
			drift := reviewCustodyDrift(root, review)
			if drift == "" || !strings.Contains(drift, "successor") {
				t.Fatalf("a successor later in wall time must be drift regardless of stamp encoding: %q; 反例注入: successorAttemptDrift 里退回 CreatedAt 字符串比较", drift)
			}
			if err := ingestWorkflowReview(root, cfg, wf); err != nil {
				t.Fatal(err)
			}
			if wf.Review.Admissible || wf.Review.HoldReason != holdReasonCustodyDrift {
				t.Fatalf("mixed-encoding redispatch must not be adoptable: %+v", wf.Review)
			}
			integ, err := loadTask(root, wf.IntegrationTaskID)
			if err != nil {
				t.Fatal(err)
			}
			if dec := evaluateIntegrationRelease(root, cfg, integ); dec.Admit || dec.HoldReason != holdReasonCustodyDrift {
				t.Fatalf("gate must hold: admit=%v reason=%q", dec.Admit, dec.HoldReason)
			}
		})
	}
}

// TestCustodyTerminalSelectionOrdersTransitionsByTime (P1-1, second seam): the
// newest committed terminal must be picked by parsed time, not by string-max.
// A stale terminal whose offset stamp compares largest would otherwise anchor
// the successor check at the wrong attempt and hide a genuine redispatch.
func TestCustodyTerminalSelectionOrdersTransitionsByTime(t *testing.T) {
	_ = stubCustodyProbes(t)
	root, _, _, review := bareReviewForCustody(t)
	zone := time.FixedZone("UTC+8", 8*3600)

	// Real order: at-old (08:55Z) → tr-old (09:00Z, stamped 17:00+08:00) →
	// at-new (09:55Z) → tr-new (10:00Z). String-max would pick tr-old and
	// anchor at at-old.
	writeReviewAttempt(t, root, review.ID, "at-old", attemptExited, time.Date(2026, 8, 24, 16, 55, 0, 1, zone))
	writeCommittedDoneTransition(t, root, review.ID, "tr-old", "at-old", time.Date(2026, 8, 24, 17, 0, 0, 1, zone))
	writeReviewAttempt(t, root, review.ID, "at-new", attemptExited, time.Date(2026, 8, 24, 9, 55, 0, 1, time.UTC))
	writeCommittedDoneTransition(t, root, review.ID, "tr-new", "at-new", time.Date(2026, 8, 24, 10, 0, 0, 1, time.UTC))
	if drift := reviewCustodyDrift(root, review); drift != "" {
		t.Fatalf("re-terminalized card without a post-terminal attempt must be clean: %q", drift)
	}

	// The redispatch: minted after the *true* newest terminal attempt, but
	// before the stale string-max anchor's stamp.
	writeReviewAttempt(t, root, review.ID, "at-ghost", attemptExited, time.Date(2026, 8, 24, 10, 30, 0, 1, time.UTC))
	drift := reviewCustodyDrift(root, review)
	if drift == "" || !strings.Contains(drift, "successor") {
		t.Fatalf("anchoring at a stale string-max terminal hides the redispatch: %q; 反例注入: successorAttemptDrift 终局选择退回 CreatedAt 字符串比较", drift)
	}
}

// TestCustodyUnparseableStampsFailClosed (P1-1): a stamp that will not parse
// cannot prove ordering, so it is drift — never an ordering tie in either
// direction.
func TestCustodyUnparseableStampsFailClosed(t *testing.T) {
	t.Run("attempt stamp", func(t *testing.T) {
		_ = stubCustodyProbes(t)
		root, _, _, review := bareReviewForCustody(t)
		base := time.Now().Add(-time.Hour)
		writeReviewAttempt(t, root, review.ID, "at-terminal", attemptExited, base)
		writeCommittedDoneTransition(t, root, review.ID, "tr-terminal", "at-terminal", base.Add(time.Minute))
		// A garbage stamp that compares string-smaller than any RFC3339 date:
		// the old string order would silently treat it as "not after".
		rec := &AttemptRecord{
			TaskID: review.ID, AttemptID: "at-garbage", ExpectedRevision: 1, ControlEpoch: 1,
			State: attemptExited, CreatedAt: "1999-bogus-stamp",
		}
		if err := writeAttempt(root, rec); err != nil {
			t.Fatal(err)
		}
		drift := reviewCustodyDrift(root, review)
		if drift == "" || !strings.Contains(drift, "unparseable") {
			t.Fatalf("an unparseable attempt stamp must fail closed: %q", drift)
		}
	})
	t.Run("transition stamp", func(t *testing.T) {
		_ = stubCustodyProbes(t)
		root, _, _, review := bareReviewForCustody(t)
		writeReviewAttempt(t, root, review.ID, "at-terminal", attemptExited, time.Now().Add(-time.Hour))
		tr := &TransitionRecord{
			TransitionID: "tr-garbage", TaskID: review.ID, ExpectedRevision: 1, NewRevision: 2,
			ControlEpoch: 1, AttemptID: "at-terminal", EventType: evDone, Status: statusDone,
			State: transitionCommitted, CreatedAt: "not-a-time",
		}
		if err := writeTransition(root, tr); err != nil {
			t.Fatal(err)
		}
		drift := reviewCustodyDrift(root, review)
		if drift == "" || !strings.Contains(drift, "unparseable") {
			t.Fatalf("an unparseable transition stamp must fail closed: %q", drift)
		}
	})
}

// TestCustodyAbsentAttemptEvidenceFailsClosed (P1-2): zero attempt records —
// or a terminal that cannot be bound to a present attempt record — is
// incomplete evidence, not proven producerGone. No admissible_review receipt
// may form, and both CLI adoption paths must refuse. This matters most in a
// fresh `ingest-review`/`release` process, where the descendant and residue
// probes are structurally silent and the "proof" would otherwise collapse to
// a single workspace-lease check.
func TestCustodyAbsentAttemptEvidenceFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		seed func(t *testing.T, root string, review *Task)
		want string
	}{
		{
			name: "zero attempt records",
			seed: func(t *testing.T, root string, review *Task) {},
			want: "no attempt records",
		},
		{
			name: "no committed terminal transition",
			seed: func(t *testing.T, root string, review *Task) {
				writeReviewAttempt(t, root, review.ID, "at-other", attemptExited, time.Now().Add(-time.Hour))
			},
			want: "no committed terminal transition",
		},
		{
			name: "terminal transition names no attempt",
			seed: func(t *testing.T, root string, review *Task) {
				writeReviewAttempt(t, root, review.ID, "at-other", attemptExited, time.Now().Add(-time.Hour))
				writeCommittedDoneTransition(t, root, review.ID, "tr-anon", "", time.Now().Add(-30*time.Minute))
			},
			want: "names no attempt",
		},
		{
			name: "named attempt record missing",
			seed: func(t *testing.T, root string, review *Task) {
				writeReviewAttempt(t, root, review.ID, "at-other", attemptExited, time.Now().Add(-time.Hour))
				writeCommittedDoneTransition(t, root, review.ID, "tr-ghost", "at-ghost", time.Now().Add(-30*time.Minute))
			},
			want: "record is missing",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_ = stubCustodyProbes(t)
			root, cfg, wf, review := bareReviewForCustody(t)
			tc.seed(t, root, review)

			drift := reviewCustodyDrift(root, review)
			if drift == "" || !strings.Contains(drift, tc.want) {
				t.Fatalf("incomplete attempt evidence must be drift (%q): %q; 反例注入: successorAttemptDrift 里删掉 statusDone 的证据完整性检查", tc.want, drift)
			}

			// Two observations across a backdated window must still refuse:
			// drift resets the window on every ingest, so no receipt of any
			// kind may form.
			if err := ingestWorkflowReview(root, cfg, wf); err != nil {
				t.Fatal(err)
			}
			backdateCustodyWindow(t, root, review.ID, custodyQuietWindow+time.Second)
			if err := ingestWorkflowReview(root, cfg, wf); err != nil {
				t.Fatal(err)
			}
			if wf.Review.Admissible || wf.Review.HoldReason != holdReasonCustodyDrift {
				t.Fatalf("absent evidence must not become adoptable: %+v", wf.Review)
			}
			if rec, err := loadCustodyRecord(root, review.ID); err != nil || rec == nil || rec.Kind != "" || rec.Drift == "" {
				t.Fatalf("no receipt may form on incomplete evidence: %+v err=%v", rec, err)
			}

			integ, err := loadTask(root, wf.IntegrationTaskID)
			if err != nil {
				t.Fatal(err)
			}
			if dec := evaluateIntegrationRelease(root, cfg, integ); dec.Admit || dec.HoldReason != holdReasonCustodyDrift {
				t.Fatalf("gate must hold on incomplete evidence: admit=%v reason=%q", dec.Admit, dec.HoldReason)
			}
			if err := tryReleaseWorkflowIntegration(root, cfg, wf); !errors.Is(err, errWorkflowHeld) {
				t.Fatalf("try-release must refuse: %v", err)
			}
			if err := cmdSetStatus([]string{"-root", root, integ.ID}, "release"); err == nil {
				t.Fatal("cardex release must refuse while attempt evidence is missing")
			}
		})
	}

	// Positive control: the same workflow shape with a present attempt record
	// and a committed terminal transition naming it completes a quiet window
	// and admits — the refusal above is a latch on evidence, not a trap.
	t.Run("positive control with complete evidence", func(t *testing.T) {
		_ = stubCustodyProbes(t)
		root, cfg, wf, review := bareReviewForCustody(t)
		at := time.Now().Add(-time.Hour)
		writeReviewAttempt(t, root, review.ID, "at-terminal", attemptExited, at)
		writeCommittedDoneTransition(t, root, review.ID, "tr-terminal", "at-terminal", at.Add(time.Minute))
		completeReviewCustodyWindow(t, root, cfg, wf)
		if !wf.Review.Admissible {
			t.Fatalf("complete evidence over a full window must admit: %+v", wf.Review)
		}
	})
}
