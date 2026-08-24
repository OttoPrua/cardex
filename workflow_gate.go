package main

import (
	"os"
	"strings"
)

func reviewVerdictIsAdmissiblePass(v *reviewVerdict) bool {
	return v != nil && v.Verdict == "pass" && len(v.P0) == 0 && len(v.P1) == 0
}

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
			return holdReasonIncompleteEvidence
		}
		return ""
	default:
		return holdReasonUnknownVocabulary
	}
}

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

func integrationCustodyOK(root string, review, writer *Task) (bool, string) {
	if review == nil {
		return false, holdReasonCustody
	}
	if review.Type != typeReview {
		return false, holdReasonCustody
	}
	if review.WriteDomain != nil {
		return false, holdReasonCustody
	}
	if review.Status == statusRunning || review.Status == statusQueued || review.Status == statusLimitPaused {
		return false, holdReasonCustody
	}
	if review.Status != statusDone {
		return false, holdReasonCustody
	}
	if writer != nil {
		if review.ReviewOf != writer.ID {
			return false, holdReasonCustody
		}
		if review.ID == writer.ID {
			return false, holdReasonCustody
		}
		if review.SessionID != "" && review.SessionID == writer.SessionID {
			return false, holdReasonCustody
		}
	}
	tasks, err := loadTasks(root)
	if err != nil {
		return false, holdReasonCustody
	}
	activeReviewers := 0
	for _, other := range tasks {
		if other == nil || other.ID == review.ID {
			continue
		}
		if other.Type != typeReview {
			continue
		}
		if writer != nil && other.ReviewOf != writer.ID {
			continue
		}
		if writer == nil && review.ReviewOf != "" && other.ReviewOf != review.ReviewOf {
			continue
		}
		switch other.Status {
		case statusQueued, statusRunning, statusLimitPaused:
			activeReviewers++
		}
	}
	if activeReviewers > 0 {
		return false, holdReasonCustody
	}
	return true, ""
}

func candidateIdentitiesMatch(gate *IntegrationGate, review *WorkflowReview, frozen *WorkflowCandidate) bool {
	wantCommit, wantTree := "", ""
	if frozen != nil {
		wantCommit, wantTree = frozen.Commit, frozen.Tree
	}
	if gate != nil {
		if wantCommit != "" && gate.CandidateCommit != "" && gate.CandidateCommit != wantCommit {
			return false
		}
		if wantTree != "" && gate.CandidateTree != "" && gate.CandidateTree != wantTree {
			return false
		}
		if wantCommit == "" {
			wantCommit = gate.CandidateCommit
		}
		if wantTree == "" {
			wantTree = gate.CandidateTree
		}
	}
	if wantCommit == "" && wantTree == "" {
		return false
	}
	if review != nil {
		if review.CandidateCommit != "" && review.CandidateCommit != wantCommit {
			return false
		}
		if review.CandidateTree != "" && review.CandidateTree != wantTree {
			return false
		}
	}
	return true
}

func evaluateIntegrationRelease(root string, t *Task) IntegrationReleaseDecision {
	dec := IntegrationReleaseDecision{Admit: false, HoldReason: holdReasonIncompleteEvidence}
	if t == nil || t.IntegrationGate == nil {
		dec.HoldReason = holdReasonIncompleteEvidence
		return dec
	}
	gate := t.IntegrationGate
	if strings.TrimSpace(gate.ReviewTaskID) == "" {
		dec.HoldReason = holdReasonIncompleteEvidence
		return dec
	}
	review, err := findTaskAnywhere(root, gate.ReviewTaskID)
	if err != nil || review == nil {
		dec.HoldReason = holdReasonMissingOutput
		return dec
	}
	dec.Review = review
	var writer *Task
	if gate.WriterTaskID != "" {
		writer, _ = findTaskAnywhere(root, gate.WriterTaskID)
	} else if review.ReviewOf != "" {
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
	reviewSnap := &WorkflowReview{
		TaskID:           review.ID,
		Verdict:          v.Verdict,
		P0:               append([]string{}, v.P0...),
		P1:               append([]string{}, v.P1...),
		P2:               append([]string{}, v.P2...),
		EvidenceComplete: true,
		CandidateCommit:  gate.CandidateCommit,
		CandidateTree:    gate.CandidateTree,
	}
	var frozen *WorkflowCandidate
	if gate.WorkflowID != "" {
		if wf, err := loadWorkflow(root, gate.WorkflowID); err == nil && wf != nil {
			frozen = wf.Candidate
			if wf.Review != nil {
				if wf.Review.CandidateCommit != "" {
					reviewSnap.CandidateCommit = wf.Review.CandidateCommit
				}
				if wf.Review.CandidateTree != "" {
					reviewSnap.CandidateTree = wf.Review.CandidateTree
				}
			}
		}
	}
	if !candidateIdentitiesMatch(gate, reviewSnap, frozen) {
		dec.HoldReason = holdReasonCandidateMismatch
		return dec
	}
	dec.Admit = true
	dec.HoldReason = ""
	return dec
}

func integrationGateAllows(root string, t *Task) bool {
	if t == nil || t.IntegrationGate == nil {
		return true
	}
	return evaluateIntegrationRelease(root, t).Admit
}
