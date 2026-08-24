package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// W2 — reviewer custody validator (docs/workflows.md).
//
// W1 made the integration gate re-derive the semantic verdict; this file adds
// the process-custody half. A review terminal is only adoptable when the
// producer is provably gone (not merely `exited`) and the whole evidence set
// — task record, event ledger, attempt records, transition journal, review
// transcript, candidate identity, and process observation — has been stable
// for one full quiet window. The proof is a durable receipt under
// `control/custody/`, written only by explicit `cardex workflow ingest-review`
// invocations; the gate itself stays read-only and never advances a window.
//
// The validator fails closed on every drift shape from the incident fixture:
// an attempt record that says `exited` while the exact PID/PGID producer, a
// descendant, runner residue, or the workspace lease is still live; a second
// open attempt; or a successor attempt minted after the terminal attempt
// (same-card redispatch). None of these can be repaired here — containment
// belongs to the supported `cardex hold` path — so the only output is a
// refusal that keeps integration held and the candidate unreviewed.
const (
	custodySchemaV1    = "cardex.custody.v1"
	custodyQuietWindow = 20 * time.Second

	// custodyKindAdmissibleReview is the only receipt kind the integration
	// gate accepts. custodyKindReconciledHeld proves containment after a
	// supported hold and nothing else: it is not a semantic review receipt,
	// and whether to admit a fresh reviewer afterwards stays a new manager
	// decision.
	custodyKindAdmissibleReview = "admissible_review"
	custodyKindReconciledHeld   = "custody_reconciled_held"

	holdReasonCustodyDrift   = "custody_drift"
	holdReasonCustodyWindow  = "custody_quiet_window"
	holdReasonCustodyReceipt = "custody_receipt_not_semantic"
)

// custodyProbes are the live-process observations the validator consumes.
// Production uses the same primitives the control plane trusts; tests inject
// deterministic fakes so the incident fixtures never spawn or signal real
// processes.
type custodyProbes struct {
	attemptProducerAlive func(*AttemptRecord) bool
	taskProcsAlive       func(taskID string) bool
	taskResidue          func(taskID string) bool
	workspaceLeaseHeld   func(dir string) bool
}

var reviewCustodyProbes = custodyProbes{
	attemptProducerAlive: attemptProducerAlive,
	taskProcsAlive:       anyTaskProcAlive,
	taskResidue:          taskProcessResidue,
	workspaceLeaseHeld:   workspaceLeaseHeld,
}

// CustodyRecord is the durable quiet-window observation for one review task.
// While Kind is empty the window is still open (or was reset by drift); a
// non-empty Kind is a completed receipt bound to the exact evidence hash it
// observed. One file per task, rewritten in place: replaying an observation
// never accumulates receipts.
type CustodyRecord struct {
	Schema          string `json:"schema"`
	TaskID          string `json:"task_id"`
	EvidenceHash    string `json:"evidence_hash,omitempty"`
	WindowSeconds   int    `json:"window_seconds"`
	FirstObservedAt string `json:"first_observed_at"`
	LastObservedAt  string `json:"last_observed_at"`
	Drift           string `json:"drift,omitempty"`
	Kind            string `json:"kind,omitempty"`
	SemanticReview  bool   `json:"semantic_review"`
	CompletedAt     string `json:"completed_at,omitempty"`
}

func custodyDir(root string) string {
	return filepath.Join(controlDir(root), "custody")
}

func custodyRecordPath(root, taskID string) string {
	return filepath.Join(custodyDir(root), taskID+".json")
}

func loadCustodyRecord(root, taskID string) (*CustodyRecord, error) {
	data, err := os.ReadFile(custodyRecordPath(root, taskID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var rec CustodyRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("parse custody record %s: %w", taskID, err)
	}
	if rec.Schema != custodySchemaV1 || rec.TaskID != taskID {
		return nil, fmt.Errorf("custody record identity mismatch: schema=%q task=%q want %q", rec.Schema, rec.TaskID, taskID)
	}
	return &rec, nil
}

func writeCustodyRecord(root string, rec *CustodyRecord) error {
	if rec == nil || rec.TaskID == "" {
		return fmt.Errorf("empty custody record")
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteSync(custodyRecordPath(root, rec.TaskID), append(data, '\n'))
}

// listReviewAttempts loads every attempt record of one task. An unreadable
// record is an error, not an absence: a record that cannot be read could still
// describe a live producer, so the caller must hold closed.
func listReviewAttempts(root, taskID string) ([]*AttemptRecord, error) {
	entries, err := os.ReadDir(attemptsDir(root, taskID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*AttemptRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		rec, err := loadAttempt(root, taskID, strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			return nil, fmt.Errorf("attempt record %s unreadable: %w", e.Name(), err)
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AttemptID < out[j].AttemptID })
	return out, nil
}

// reviewCustodyDrift reports the first custody violation it can prove, or ""
// when no drift evidence exists. `exited` is deliberately not trusted:
// producerGone must be jointly disproved by the exact PID/PGID identity,
// descendant/runner residue, the workspace lease, and successor-attempt
// absence. Any positive finding forbids review redispatch, adopting the
// verdict as `pass`, splicing old output, and releasing any successor card.
func reviewCustodyDrift(root string, review *Task) string {
	if review == nil {
		return "review task unreadable"
	}
	attempts, err := listReviewAttempts(root, review.ID)
	if err != nil {
		return err.Error()
	}
	open := 0
	for _, rec := range attempts {
		switch rec.State {
		case attemptReserved, attemptBound:
			open++
		}
		if reviewCustodyProbes.attemptProducerAlive(rec) {
			return fmt.Sprintf("attempt %s producer still alive (state=%s): exited is not producerGone", rec.AttemptID, rec.State)
		}
	}
	if open > 0 {
		return fmt.Sprintf("%d attempt(s) still open: one role/task allows at most one active attempt, and none across a terminal", open)
	}
	if reviewCustodyProbes.taskProcsAlive(review.ID) {
		return "a registered executor process for this task is still alive"
	}
	if reviewCustodyProbes.taskResidue(review.ID) {
		return "runner process residue remains for this task"
	}
	if policyFallbackProcessProofSupported() && reviewCustodyProbes.workspaceLeaseHeld(review.Dir) {
		return "workspace lease is still held: descendants may survive in the reviewer worktree"
	}
	if reason := successorAttemptDrift(root, review, attempts); reason != "" {
		return reason
	}
	return ""
}

// custodyStamp parses a record timestamp for ordering. RFC3339Nano stamps are
// not lexicographically ordered: they carry the writer's local UTC offset and
// trim trailing fractional-second zeros, so raw string `>` inverts across
// mixed offsets (launchd job vs interactive shell, DST transitions) and across
// fractional widths. Every ordering decision must go through parsed
// time.Time values; a stamp that will not parse is drift (fail closed), never
// an ordering tie.
func custodyStamp(stamp string) (time.Time, bool) {
	at, err := time.Parse(time.RFC3339Nano, stamp)
	return at, err == nil
}

// successorAttemptDrift detects the same-card redispatch shape: the committed
// terminal transition is bound to one exact attempt, so any attempt record
// minted after that one means a second producer touched the card after its
// terminal. Even a successor that later exited breaks custody — the transcript
// can no longer be attributed to the terminal attempt alone.
//
// It also owns the attempt-evidence completeness requirement for the semantic
// `done` terminal: zero attempt records, a missing committed terminal
// transition, or a terminal transition that names no attempt all mean
// producerGone is unproven, not disproven. Absent evidence must never mint an
// admissible receipt — in a short-lived CLI process the descendant and
// runner-residue probes are structurally silent, so without attempt records
// the "proof" would collapse to a single workspace-lease check.
func successorAttemptDrift(root string, review *Task, attempts []*AttemptRecord) string {
	if !isTerminalTransitionStatus(review.Status) {
		return ""
	}
	transitions, err := listTaskTransitions(root, review.ID)
	if err != nil {
		return fmt.Sprintf("transition journal unreadable: %v", err)
	}
	var term *TransitionRecord
	var termAt time.Time
	for _, rec := range transitions {
		if rec == nil || rec.State != transitionCommitted || !isTerminalTransitionStatus(rec.Status) {
			continue
		}
		at, ok := custodyStamp(rec.CreatedAt)
		if !ok {
			return fmt.Sprintf("terminal transition %s carries unparseable created_at %q: attempt order cannot be proven", rec.TransitionID, rec.CreatedAt)
		}
		if term == nil || at.After(termAt) || (at.Equal(termAt) && rec.TransitionID > term.TransitionID) {
			term, termAt = rec, at
		}
	}
	if review.Status == statusDone {
		if len(attempts) == 0 {
			return "no attempt records exist for this review terminal: absent evidence is incomplete, not proven producerGone"
		}
		if term == nil {
			return "no committed terminal transition exists for this review terminal: the producing attempt cannot be identified"
		}
		if term.AttemptID == "" {
			return fmt.Sprintf("terminal transition %s names no attempt: the producing attempt cannot be identified", term.TransitionID)
		}
	}
	if term == nil || term.AttemptID == "" {
		return ""
	}
	var bound *AttemptRecord
	for _, a := range attempts {
		if a.AttemptID == term.AttemptID {
			bound = a
		}
	}
	if bound == nil {
		return fmt.Sprintf("terminal transition %s names attempt %s but its record is missing", term.TransitionID, term.AttemptID)
	}
	boundAt, ok := custodyStamp(bound.CreatedAt)
	if !ok {
		return fmt.Sprintf("terminal attempt %s carries unparseable created_at %q: attempt order cannot be proven", bound.AttemptID, bound.CreatedAt)
	}
	for _, a := range attempts {
		if a.AttemptID == term.AttemptID {
			continue
		}
		at, ok := custodyStamp(a.CreatedAt)
		if !ok {
			return fmt.Sprintf("attempt %s carries unparseable created_at %q: attempt order cannot be proven", a.AttemptID, a.CreatedAt)
		}
		if at.After(boundAt) {
			return fmt.Sprintf("successor attempt %s was minted after terminal attempt %s: same-card redispatch", a.AttemptID, term.AttemptID)
		}
	}
	return ""
}

// custodyEvidenceHash digests the entire durable evidence set plus the current
// process observation. Any late-arriving output, event, attempt, transition,
// task rewrite, candidate relabel, or process flap changes the hash and resets
// the quiet window — which is exactly how old-output splice is rejected.
// The digest stores no content, only identity.
func custodyEvidenceHash(root string, review *Task, candCommit, candTree string) (string, error) {
	if review == nil {
		return "", fmt.Errorf("review task unreadable")
	}
	attempts, err := listReviewAttempts(root, review.ID)
	if err != nil {
		return "", err
	}
	events, _, err := loadTaskEvents(root, review.ID)
	if err != nil {
		return "", err
	}
	transitions, err := listTaskTransitions(root, review.ID)
	if err != nil {
		return "", err
	}
	sort.Slice(transitions, func(i, j int) bool { return transitions[i].TransitionID < transitions[j].TransitionID })
	transcript, err := os.ReadFile(taskLogPath(root, review.ID))
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	transcriptSum := sha256.Sum256(transcript)
	processObservation := map[string]bool{
		"task_procs_alive": reviewCustodyProbes.taskProcsAlive(review.ID),
		"task_residue":     reviewCustodyProbes.taskResidue(review.ID),
		"lease_held":       policyFallbackProcessProofSupported() && reviewCustodyProbes.workspaceLeaseHeld(review.Dir),
	}
	attemptsAlive := make([]bool, 0, len(attempts))
	for _, rec := range attempts {
		attemptsAlive = append(attemptsAlive, reviewCustodyProbes.attemptProducerAlive(rec))
	}
	payload := map[string]any{
		"schema":            custodySchemaV1,
		"task":              review,
		"events":            events,
		"attempts":          attempts,
		"attempts_alive":    attemptsAlive,
		"transitions":       transitions,
		"transcript_sha256": hex.EncodeToString(transcriptSum[:]),
		"candidate_commit":  candCommit,
		"candidate_tree":    candTree,
		"process":           processObservation,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// observeReviewCustody records one explicit observation and returns the
// resulting durable state. Drift resets the window unconditionally; a changed
// hash restarts it; a completed receipt whose hash still matches is returned
// untouched, so replays are stable. Completion requires the review task to
// already sit on a terminal status: `done` yields the semantic
// admissible-review receipt, while `held`/`canceled` (the supported
// containment terminals) yield only the non-semantic reconciliation receipt.
func observeReviewCustody(root string, review *Task, hash, drift string) (*CustodyRecord, error) {
	if review == nil {
		return nil, fmt.Errorf("review task unreadable")
	}
	now := time.Now()
	stamp := now.Format(time.RFC3339Nano)
	rec, err := loadCustodyRecord(root, review.ID)
	if err != nil {
		return nil, err
	}
	if drift != "" {
		rec = &CustodyRecord{
			Schema:          custodySchemaV1,
			TaskID:          review.ID,
			EvidenceHash:    hash,
			WindowSeconds:   int(custodyQuietWindow / time.Second),
			FirstObservedAt: stamp,
			LastObservedAt:  stamp,
			Drift:           drift,
		}
		return rec, writeCustodyRecord(root, rec)
	}
	if rec != nil && rec.Kind != "" && rec.EvidenceHash == hash {
		return rec, nil
	}
	windowOpen := rec != nil && rec.Kind == "" && rec.Drift == "" && rec.EvidenceHash == hash
	if windowOpen {
		if _, perr := time.Parse(time.RFC3339Nano, rec.FirstObservedAt); perr != nil {
			windowOpen = false
		}
	}
	if !windowOpen {
		rec = &CustodyRecord{
			Schema:          custodySchemaV1,
			TaskID:          review.ID,
			EvidenceHash:    hash,
			WindowSeconds:   int(custodyQuietWindow / time.Second),
			FirstObservedAt: stamp,
		}
	}
	rec.LastObservedAt = stamp
	first, _ := time.Parse(time.RFC3339Nano, rec.FirstObservedAt)
	if now.Sub(first) >= custodyQuietWindow {
		switch review.Status {
		case statusDone:
			rec.Kind = custodyKindAdmissibleReview
			rec.SemanticReview = true
		case statusHeld, statusCanceled:
			rec.Kind = custodyKindReconciledHeld
			rec.SemanticReview = false
		}
		if rec.Kind != "" {
			rec.CompletedAt = stamp
		}
	}
	return rec, writeCustodyRecord(root, rec)
}

// reviewCustodyReceiptReason is the read-only gate-side check. It re-derives
// drift and the evidence hash fresh on every call and compares them against
// the durable receipt; it never writes an observation, so tick consulting the
// gate cannot advance a quiet window. An empty return means the receipt is a
// currently-valid semantic admissible-review proof.
func reviewCustodyReceiptReason(root string, review *Task, candCommit, candTree string) string {
	if drift := reviewCustodyDrift(root, review); drift != "" {
		return holdReasonCustodyDrift
	}
	hash, err := custodyEvidenceHash(root, review, candCommit, candTree)
	if err != nil {
		return holdReasonCustodyDrift
	}
	rec, err := loadCustodyRecord(root, review.ID)
	if err != nil || rec == nil || rec.Kind == "" || rec.EvidenceHash != hash {
		return holdReasonCustodyWindow
	}
	if rec.Kind != custodyKindAdmissibleReview || !rec.SemanticReview {
		return holdReasonCustodyReceipt
	}
	return ""
}
