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

// integrationCustodyOK is the minimal custody proof this tree can actually
// make: the reviewer is a distinct read-only role instance that terminated on
// its own card and has no rival active reviewer for the same writer. Full
// producerGone/quiet-window custody (docs/workflows.md W2) is still a
// roadmap fixture, so this deliberately errs toward holding.
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
	// The rival-reviewer sweep exists to find a card that should not be there.
	// A scan that silently drops the files it cannot parse is exactly the wrong
	// tool for it: a corrupt rival disappears from the sweep looking for it.
	tasks, err := scanWorkflowTasks(root)
	if err != nil {
		return false, holdReasonBrokenEvidence
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

	// The producer of the reviewed bytes has to exist and load. Discarding this
	// lookup error is what let a deleted or corrupt writer present itself as
	// "no producer named", which skips every writer-bound custody check and
	// releases on a review whose subject is gone.
	producer := strings.TrimSpace(gate.WriterTaskID)
	if producer == "" {
		producer = strings.TrimSpace(review.ReviewOf)
	}
	if producer == "" {
		dec.HoldReason = holdReasonCustody
		return dec
	}
	writer, err := loadWorkflowRoleTask(root, "writer", producer)
	if err != nil {
		dec.HoldReason = holdReasonBrokenEvidence
		return dec
	}
	if ok, reason := integrationCustodyOK(root, review, writer); !ok {
		dec.HoldReason = reason
		return dec
	}

	raw := loadTaskResultForGate(root, review)
	v, shapeHold := parseReviewVerdictEvidence(raw)
	dec.Verdict = v
	if shapeHold != "" {
		dec.HoldReason = shapeHold
		return dec
	}
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
	// The workflow binding and its frozen candidate are required, not optional.
	// gate.CandidateCommit/Tree live on the very card a release would free, so
	// they cannot stand alone as the frozen identity — matching them against
	// themselves proves nothing. Only the separately durable record can say
	// which bytes the review was actually about.
	if strings.TrimSpace(gate.WorkflowID) == "" {
		dec.HoldReason = holdReasonIncompleteEvidence
		return dec
	}
	wf, err := loadWorkflow(root, cfg, gate.WorkflowID)
	if err != nil {
		// A gate naming a workflow that will not load is missing evidence,
		// not a card that happens to have no workflow.
		dec.HoldReason = holdReasonIncompleteEvidence
		return dec
	}
	frozen := wf.Candidate
	if frozen == nil || (strings.TrimSpace(frozen.Commit) == "" && strings.TrimSpace(frozen.Tree) == "") {
		dec.HoldReason = holdReasonIncompleteEvidence
		return dec
	}
	if wf.Review != nil {
		if wf.Review.CandidateCommit != "" {
			snapshot.CandidateCommit = wf.Review.CandidateCommit
		}
		if wf.Review.CandidateTree != "" {
			snapshot.CandidateTree = wf.Review.CandidateTree
		}
	}
	if !candidateIdentitiesMatch(gate, snapshot, frozen) {
		dec.HoldReason = holdReasonCandidateMismatch
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
