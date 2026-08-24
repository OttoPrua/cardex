package main

import (
	"os"
	"strings"
)

// IntegrationReleaseDecision is the single fail-closed answer both tick and
// `cardex release` consult. Admit is true only when every check passed; every
// other outcome carries a HoldReason so the refusal is legible.
type IntegrationReleaseDecision struct {
	Admit      bool
	HoldReason string
	Verdict    *reviewVerdict
	Review     *Task
}

// reviewVerdictIsAdmissiblePass encodes templates/design-review.md: `pass` is
// only a pass when p0 and p1 are both empty. `ACCEPT`, `HELD`, and any other
// token are not machine vocabulary at all.
func reviewVerdictIsAdmissiblePass(v *reviewVerdict) bool {
	return v != nil && v.Verdict == "pass" && len(v.P0) == 0 && len(v.P1) == 0
}

// reviewHoldReason maps a parsed terminal to the reason integration stays
// held. An empty string means the review itself raised no objection.
func reviewHoldReason(v *reviewVerdict, raw string) string {
	if strings.TrimSpace(raw) == "" {
		return holdReasonMissingOutput
	}
	if v == nil {
		return holdReasonUnknownVocabulary
	}
	switch v.Verdict {
	case "concerns":
		return holdReasonConcerns
	case "block":
		return holdReasonBlock
	case "pass":
		if !reviewVerdictIsAdmissiblePass(v) {
			return holdReasonFindingsOpen
		}
		return ""
	default:
		return holdReasonUnknownVocabulary
	}
}

// loadTaskResultForGate reads the review transcript from disk rather than
// trusting a cached summary, so replay always re-derives the verdict.
func loadTaskResultForGate(root string, t *Task) string {
	if t == nil {
		return ""
	}
	data, err := os.ReadFile(taskLogPath(root, t.ID))
	if err != nil {
		return ""
	}
	return string(data)
}

// integrationCustodyOK is the role-level custody proof: the reviewer is a
// distinct read-only role instance that terminated on its own card and has no
// rival active reviewer for the same writer. The process-level half —
// producerGone disproof and the quiet-window receipt (docs/workflows.md W2) —
// lives in workflow_custody.go and is enforced separately by the gate.
func integrationCustodyOK(root string, review, writer *Task) (bool, string) {
	if review == nil || review.Type != typeReview {
		return false, holdReasonCustody
	}
	if review.WriteDomain != nil {
		return false, holdReasonCustody
	}
	if review.Status != statusDone {
		return false, holdReasonCustody
	}
	if writer != nil {
		if review.ID == writer.ID || review.ReviewOf != writer.ID {
			return false, holdReasonCustody
		}
		if review.SessionID != "" && review.SessionID == writer.SessionID {
			return false, holdReasonCustody
		}
	}
	reviewOf := review.ReviewOf
	if writer != nil {
		reviewOf = writer.ID
	}
	if reviewOf == "" {
		// Without a named subject there is nothing to prove single-reviewer
		// custody against, and the review is not bound to any candidate producer.
		return false, holdReasonCustody
	}
	tasks, err := loadTasks(root)
	if err != nil {
		return false, holdReasonCustody
	}
	for _, other := range tasks {
		if other == nil || other.ID == review.ID || other.Type != typeReview {
			continue
		}
		if other.ReviewOf != reviewOf {
			continue
		}
		switch other.Status {
		case statusQueued, statusRunning, statusLimitPaused:
			return false, holdReasonCustody
		}
	}
	return true, ""
}

// candidateIdentitiesMatch requires one non-empty candidate identity that the
// gate, the frozen workflow candidate, and the review snapshot all agree on.
// An unnamed candidate is a mismatch: a review with nothing bound to it cannot
// release anything.
func candidateIdentitiesMatch(gate *IntegrationGate, review *WorkflowReview, frozen *WorkflowCandidate) bool {
	commit, tree := "", ""
	if gate != nil {
		commit, tree = gate.CandidateCommit, gate.CandidateTree
	}
	if commit == "" && tree == "" {
		return false
	}
	agrees := func(gotCommit, gotTree string) bool {
		if gotCommit != "" && gotCommit != commit {
			return false
		}
		if gotTree != "" && gotTree != tree {
			return false
		}
		return true
	}
	if frozen != nil {
		if frozen.Commit == "" && frozen.Tree == "" {
			return false
		}
		if !agrees(frozen.Commit, frozen.Tree) {
			return false
		}
	}
	if review != nil && !agrees(review.CandidateCommit, review.CandidateTree) {
		return false
	}
	return true
}

// evaluateIntegrationRelease re-derives admissibility from durable evidence
// every time it is called. It reads only; the caller decides what to persist.
func evaluateIntegrationRelease(root string, cfg *Config, t *Task) IntegrationReleaseDecision {
	dec := IntegrationReleaseDecision{HoldReason: holdReasonNoGate}
	if t == nil || t.IntegrationGate == nil {
		return dec
	}
	gate := t.IntegrationGate
	if strings.TrimSpace(gate.ReviewTaskID) == "" {
		dec.HoldReason = holdReasonMissingReview
		return dec
	}
	review, err := findTaskAnywhere(root, gate.ReviewTaskID)
	if err != nil || review == nil {
		dec.HoldReason = holdReasonMissingReview
		return dec
	}
	dec.Review = review

	var writer *Task
	switch {
	case gate.WriterTaskID != "":
		writer, _ = findTaskAnywhere(root, gate.WriterTaskID)
	case review.ReviewOf != "":
		writer, _ = findTaskAnywhere(root, review.ReviewOf)
	}
	if ok, reason := integrationCustodyOK(root, review, writer); !ok {
		dec.HoldReason = reason
		return dec
	}

	raw := loadTaskResultForGate(root, review)
	v := parseReviewVerdict(raw)
	dec.Verdict = v
	if reason := reviewHoldReason(v, raw); reason != "" {
		dec.HoldReason = reason
		return dec
	}

	snapshot := &WorkflowReview{
		TaskID:          review.ID,
		Verdict:         v.Verdict,
		CandidateCommit: gate.CandidateCommit,
		CandidateTree:   gate.CandidateTree,
	}
	var frozen *WorkflowCandidate
	if gate.WorkflowID != "" {
		wf, err := loadWorkflow(root, cfg, gate.WorkflowID)
		if err != nil {
			// A gate naming a workflow that will not load is missing evidence,
			// not a card that happens to have no workflow.
			dec.HoldReason = holdReasonIncompleteEvidence
			return dec
		}
		frozen = wf.Candidate
		if wf.Review != nil {
			if wf.Review.CandidateCommit != "" {
				snapshot.CandidateCommit = wf.Review.CandidateCommit
			}
			if wf.Review.CandidateTree != "" {
				snapshot.CandidateTree = wf.Review.CandidateTree
			}
		}
	}
	if !candidateIdentitiesMatch(gate, snapshot, frozen) {
		dec.HoldReason = holdReasonCandidateMismatch
		return dec
	}

	// W2 process-custody latch: a semantic pass with matching identities is
	// still not adoptable until producerGone is proven and a quiet-window
	// receipt covering exactly this evidence exists. Read-only: the receipt is
	// written only by explicit ingest observations, never by the gate.
	if reason := reviewCustodyReceiptReason(root, review, snapshot.CandidateCommit, snapshot.CandidateTree); reason != "" {
		dec.HoldReason = reason
		return dec
	}

	dec.Admit = true
	dec.HoldReason = ""
	return dec
}

// integrationGateAllows is the tick-side latch. Cards without a gate are
// unaffected, which keeps every pre-existing card's behavior identical.
func integrationGateAllows(root string, cfg *Config, t *Task) bool {
	if t == nil || t.IntegrationGate == nil {
		return true
	}
	return evaluateIntegrationRelease(root, cfg, t).Admit
}
