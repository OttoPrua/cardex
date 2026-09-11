package main

import (
	"crypto/sha256"
	"fmt"
	"io"
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

// reviewOutput binds the existing RESULT log bytes to their actual producer. It is
// not a new file contract for ordinary tasks or a cached verdict.
type reviewOutput struct {
	TaskID          string `json:"task_id"`
	AttemptID       string `json:"attempt_id"`
	ControlEpoch    int64  `json:"control_epoch"`
	Step            int    `json:"step"`
	Offset          int64  `json:"offset"`
	Bytes           int64  `json:"bytes"`
	SHA256          string `json:"sha256"`
	CandidateCommit string `json:"candidate_commit,omitempty"`
	CandidateTree   string `json:"candidate_tree,omitempty"`
}

func writeReviewOutput(f *os.File, t *Task, result string) error {
	t.ReviewOutput = nil
	if t.ActiveAttemptID == "" {
		return fmt.Errorf("review output has no producer attempt")
	}
	if _, err := fmt.Fprint(f, "--- RESULT ---\n"); err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	body := strings.TrimSpace(result)
	if _, err := io.WriteString(f, body+"\n"); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	r := &reviewOutput{TaskID: t.ID, AttemptID: t.ActiveAttemptID, ControlEpoch: t.ControlEpoch,
		Step: t.Step, Offset: info.Size(), Bytes: int64(len(body)), SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(body)))}
	if t.ReviewCandidate != nil {
		r.CandidateCommit, r.CandidateTree = t.ReviewCandidate.Commit, t.ReviewCandidate.Tree
	}
	t.ReviewOutput = r
	return nil
}

func loadTaskResultForGate(root string, t *Task) string {
	if t == nil || t.Type != typeReview || t.Status != statusDone || t.ReviewOutput == nil {
		return ""
	}
	r := t.ReviewOutput
	if r.TaskID != t.ID || r.AttemptID == "" || r.ControlEpoch != t.ControlEpoch || r.Step != t.Step || r.Bytes <= 0 || r.Offset < 0 {
		return ""
	}
	// The terminal transition, not merely any old exited attempt, owns these bytes.
	terminal, err := loadTransition(root, t.ID, t.LastCommittedTransitionID)
	if err != nil || terminal == nil || terminal.State != transitionCommitted || terminal.Status != statusDone || terminal.AttemptID != r.AttemptID || terminal.ControlEpoch != t.ControlEpoch {
		return ""
	}
	if t.ReviewCandidate != nil && (r.CandidateCommit != t.ReviewCandidate.Commit || r.CandidateTree != t.ReviewCandidate.Tree) {
		return ""
	}
	attempt, err := loadAttempt(root, t.ID, r.AttemptID)
	if err != nil || attempt == nil || attempt.State != attemptExited || attempt.ControlEpoch != t.ControlEpoch || attemptProducerAlive(attempt) {
		return ""
	}
	f, info, err := openRegularFileNoBlock(taskLogPath(root, t.ID))
	if err != nil {
		return ""
	}
	defer f.Close()
	if r.Offset > info.Size() || r.Bytes > info.Size()-r.Offset {
		return ""
	}
	// Stream the bounded range first: corrupted task metadata cannot force an unbounded allocation.
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, r.Offset, r.Bytes)); err != nil || fmt.Sprintf("%x", h.Sum(nil)) != r.SHA256 {
		return ""
	}
	data, err := io.ReadAll(io.NewSectionReader(f, r.Offset, r.Bytes))
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != r.SHA256 {
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
	if review != nil && ((review.CandidateCommit == "" && review.CandidateTree == "") || !agrees(review.CandidateCommit, review.CandidateTree)) {
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
		CandidateCommit: review.ReviewOutput.CandidateCommit,
		CandidateTree:   review.ReviewOutput.CandidateTree,
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
