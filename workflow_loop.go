package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

func pinWorkflowEngine(t *Task, engine string) error {
	if err := validateWorkflowEngine(engine); err != nil {
		return err
	}
	t.PreferRunner = engine
	return nil
}

func workflowHasActiveRole(root string, wf *WorkflowRecord, role string) (*Task, bool) {
	tasks, err := loadTasks(root)
	if err != nil {
		return nil, true
	}
	wantType := typeSequence
	if role == "reviewer" {
		wantType = typeReview
	}
	for _, t := range tasks {
		if t == nil {
			continue
		}
		if wf != nil && t.WorkflowID != wf.ID {
			if role == "reviewer" && wf.WriterTaskID != "" && t.ReviewOf == wf.WriterTaskID {
				// fall through
			} else {
				continue
			}
		}
		if t.Type != wantType {
			continue
		}
		switch t.Status {
		case statusQueued, statusRunning, statusLimitPaused, statusHeld:
			if role == "writer" && t.Type == typeSequence && t.IntegrationGate == nil && (wf == nil || t.ID == wf.WriterTaskID || t.WorkflowID == wf.ID) {
				return t, true
			}
			if role == "reviewer" && t.Type == typeReview {
				return t, true
			}
		}
	}
	return nil, false
}

func dedupeWorkflowRoles(root string, wf *WorkflowRecord) error {
	if wf == nil {
		return errWorkflowMalformed
	}
	if active, ok := workflowHasActiveRole(root, wf, "writer"); ok && wf.WriterTaskID != "" && active.ID != wf.WriterTaskID {
		return fmt.Errorf("%w: writer %s vs %s", errWorkflowDuplicateRole, wf.WriterTaskID, active.ID)
	}
	if active, ok := workflowHasActiveRole(root, wf, "reviewer"); ok && wf.ReviewerTaskID != "" && active.ID != wf.ReviewerTaskID {
		return fmt.Errorf("%w: reviewer %s vs %s", errWorkflowDuplicateRole, wf.ReviewerTaskID, active.ID)
	}
	return nil
}

func writeWorkflowProgress(root string, wf *WorkflowRecord) error {
	if err := os.MkdirAll(workflowsDir(root), 0o700); err != nil {
		return err
	}
	payload := map[string]any{
		"schema":              "cardex.workflow.progress.v1",
		"workflow_id":         wf.ID,
		"mode":                wf.Mode,
		"module_id":           wf.ModuleID,
		"goal_id":             wf.GoalID,
		"status":              wf.Status,
		"round":               wf.CurrentRound,
		"max_rounds":          wf.MaxRounds,
		"writer_task_id":      wf.WriterTaskID,
		"reviewer_task_id":    wf.ReviewerTaskID,
		"integration_task_id": wf.IntegrationTaskID,
		"candidate":           wf.Candidate,
		"review":              wf.Review,
		"effect_gates":        wf.EffectGates,
		"updated_at":          wf.UpdatedAt,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	jsonPath := workflowProgressJSONPath(root, wf.ID)
	if err := atomicWriteMode(jsonPath, append(data, '\n'), 0o600); err != nil {
		return err
	}
	verdict := ""
	hold := ""
	if wf.Review != nil {
		verdict = wf.Review.Verdict
		hold = wf.Review.HoldReason
	}
	commit, tree := "", ""
	if wf.Candidate != nil {
		commit, tree = wf.Candidate.Commit, wf.Candidate.Tree
	}
	md := fmt.Sprintf(`# Workflow %s

- mode: %s
- module: %s
- goal: %s (%s)
- status: %s
- round: %d / %d
- writer: %s
- reviewer: %s
- integration: %s (%s)
- live/cutover: %s / %s
- candidate: %s / %s
- review verdict: %s
- hold reason: %s

Routine progress stays in this file. Root is notified only for live-ready, true external dependency, owner choice, or exhausted route.
`, wf.ID, wf.Mode, wf.ModuleID, wf.GoalID, wf.Goal, wf.Status, wf.CurrentRound, wf.MaxRounds,
		orDash(wf.WriterTaskID), orDash(wf.ReviewerTaskID), orDash(wf.IntegrationTaskID), wf.EffectGates.Integration,
		wf.EffectGates.Live, wf.EffectGates.Cutover, orDash(commit), orDash(tree), orDash(verdict), orDash(hold))
	mdPath := workflowProgressMDPath(root, wf.ID)
	if err := atomicWriteMode(mdPath, []byte(md), 0o600); err != nil {
		return err
	}
	wf.Progress = WorkflowProgressCoords{JSON: jsonPath, Markdown: mdPath}
	return nil
}

func emitRootNotify(root string, wf *WorkflowRecord, kind, summary string) error {
	switch kind {
	case rootNotifyLiveReady, rootNotifyExternal, rootNotifyOwner, rootNotifyExhausted:
	default:
		return fmt.Errorf("non-material notify kind %q", kind)
	}
	if err := os.MkdirAll(workflowNotifyDir(root), 0o700); err != nil {
		return err
	}
	n := &WorkflowNotify{
		Kind:     kind,
		At:       time.Now().Format(time.RFC3339),
		Summary:  summary,
		Workflow: wf.ID,
	}
	if wf.Candidate != nil {
		n.Candidate = wf.Candidate.Commit
		n.Tree = wf.Candidate.Tree
	}
	name := fmt.Sprintf("%s-%s.json", wf.ID, kind)
	path := filepathJoinNotify(root, name)
	n.Receipt = path
	data, err := json.MarshalIndent(n, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWriteMode(path, append(data, '\n'), 0o600); err != nil {
		return err
	}
	wf.MaterialNotify = n
	return nil
}

func filepathJoinNotify(root, name string) string {
	return workflowNotifyDir(root) + "/" + name
}

func admitWorkflowWriter(root string, cfg *Config, wf *WorkflowRecord, prompt string) (*Task, error) {
	if err := refreshWorkflow(root, wf); err != nil {
		return nil, err
	}
	if err := dedupeWorkflowRoles(root, wf); err != nil {
		return nil, err
	}
	if active, ok := workflowHasActiveRole(root, wf, "writer"); ok {
		wf.WriterTaskID = active.ID
		return active, fmt.Errorf("%w: writer %s", errWorkflowDuplicateRole, active.ID)
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = wf.Goal + "\n\nTerminal criteria:\n" + wf.TerminalCriteria
	}
	tpl, err := loadTemplate(root, "workflow-writer")
	if err == nil && strings.TrimSpace(tpl) != "" {
		prompt = renderTemplate(tpl, map[string]string{
			"GOAL":     wf.Goal,
			"CRITERIA": wf.TerminalCriteria,
			"MODULE":   wf.ModuleID,
			"DIR":      wf.Worktree,
			"ROUND":    fmt.Sprintf("%d", wf.CurrentRound),
		})
	}
	t := newTask(root, cfg, typeSequence, "workflow writer: "+wf.ModuleID, wf.Worktree, []string{prompt}, 8)
	t.Project = wf.ModuleID
	t.WorkflowID = wf.ID
	t.ReviewAfter = false
	t.MaxFixRounds = wf.MaxRounds
	t.FixRound = wf.CurrentRound
	t.SkipPermissions = true
	if err := pinWorkflowEngine(t, wf.WriterEngine); err != nil {
		return nil, err
	}
	d := wf.WriteDomain
	t.WriteDomain = &WriteDomain{
		ID:        d.ID,
		Lineage:   d.Lineage,
		Component: d.Component,
		Paths:     append([]string{}, d.Paths...),
		Resources: append([]ResourceClaim{}, d.Resources...),
	}
	if err := saveTask(root, t); err != nil {
		return nil, err
	}
	emitTaskEvent(root, t.ID, evQueued, "workflow:writer", statusQueued, 0, map[string]any{
		"workflow": wf.ID, "module": wf.ModuleID, "engine": t.PreferRunner,
	})
	wf.WriterTaskID = t.ID
	wf.Status = workflowStatusWriting
	if err := writeWorkflowProgress(root, wf); err != nil {
		return t, err
	}
	return t, saveWorkflow(root, wf)
}

func admitWorkflowReviewer(root string, cfg *Config, wf *WorkflowRecord) (*Task, error) {
	if err := refreshWorkflow(root, wf); err != nil {
		return nil, err
	}
	if wf.WriterTaskID == "" {
		return nil, fmt.Errorf("%w: no writer", errWorkflowMalformed)
	}
	if err := dedupeWorkflowRoles(root, wf); err != nil {
		return nil, err
	}
	if active, ok := workflowHasActiveRole(root, wf, "reviewer"); ok {
		wf.ReviewerTaskID = active.ID
		return active, fmt.Errorf("%w: reviewer %s", errWorkflowDuplicateRole, active.ID)
	}
	writer, err := findTaskAnywhere(root, wf.WriterTaskID)
	if err != nil {
		return nil, err
	}
	tpl, err := loadTemplate(root, typeReview)
	if err != nil {
		return nil, err
	}
	focus := fmt.Sprintf("独立审核 workflow %s 模块 %s 的冻结候选（commit/tree 必须与 workflow 记录一致）；不得修改候选。", wf.ID, wf.ModuleID)
	prompt := renderTemplate(tpl, map[string]string{"DIR": wf.Worktree, "FOCUS": focus})
	t := newTask(root, cfg, typeReview, "workflow review: "+wf.ModuleID, wf.Worktree, []string{prompt}, writer.Priority)
	t.Project = wf.ModuleID
	t.WorkflowID = wf.ID
	t.ReviewOf = writer.ID
	t.FixRound = writer.FixRound
	t.MaxFixRounds = wf.MaxRounds
	t.SessionID = ""
	if err := pinWorkflowEngine(t, wf.ReviewerEngine); err != nil {
		return nil, err
	}
	if err := saveTask(root, t); err != nil {
		return nil, err
	}
	emitTaskEvent(root, t.ID, evQueued, "workflow:reviewer", statusQueued, 0, map[string]any{
		"workflow": wf.ID, "review_of": writer.ID, "engine": t.PreferRunner,
	})
	wf.ReviewerTaskID = t.ID
	wf.Status = workflowStatusReviewing
	if wf.IntegrationTaskID != "" {
		if integ, err := loadTask(root, wf.IntegrationTaskID); err == nil {
			if integ.IntegrationGate == nil {
				integ.IntegrationGate = &IntegrationGate{}
			}
			integ.IntegrationGate.ReviewTaskID = t.ID
			integ.IntegrationGate.WriterTaskID = writer.ID
			integ.IntegrationGate.WorkflowID = wf.ID
			if wf.Candidate != nil {
				integ.IntegrationGate.CandidateCommit = wf.Candidate.Commit
				integ.IntegrationGate.CandidateTree = wf.Candidate.Tree
			}
			_ = saveTask(root, integ)
		}
	}
	if err := writeWorkflowProgress(root, wf); err != nil {
		return t, err
	}
	return t, saveWorkflow(root, wf)
}

func admitWorkflowRepair(root string, cfg *Config, wf *WorkflowRecord, findings, summary, verdict string) (*Task, error) {
	if err := refreshWorkflow(root, wf); err != nil {
		return nil, err
	}
	if wf.CurrentRound+1 > wf.MaxRounds {
		wf.Status = workflowStatusExhausted
		_ = emitRootNotify(root, wf, rootNotifyExhausted, "repair rounds exhausted")
		_ = writeWorkflowProgress(root, wf)
		_ = saveWorkflow(root, wf)
		return nil, fmt.Errorf("%w: exhausted", errWorkflowReleaseHeld)
	}
	if active, ok := workflowHasActiveRole(root, wf, "writer"); ok {
		return active, fmt.Errorf("%w: writer %s", errWorkflowDuplicateRole, active.ID)
	}
	writer, err := findTaskAnywhere(root, wf.WriterTaskID)
	if err != nil {
		return nil, err
	}
	round := wf.CurrentRound + 1
	tpl, err := loadTemplate(root, "fix-cycle")
	if err != nil {
		return nil, err
	}
	prompt := renderTemplate(tpl, map[string]string{
		"TITLE":       writer.Title,
		"VERDICT":     verdict,
		"ROUND":       fmt.Sprintf("%d", round),
		"SUMMARY":     summary,
		"FINDINGS":    findings,
		"ORIG_PROMPT": strings.Join(writer.Prompts, "\n"),
		"REVIEW_LOG":  taskLogPath(root, wf.ReviewerTaskID),
	})
	t := newTask(root, cfg, typeSequence, fmt.Sprintf("修复R%d: %s", round, wf.ModuleID), wf.Worktree, []string{prompt}, writer.Priority)
	t.Project = wf.ModuleID
	t.WorkflowID = wf.ID
	t.ReviewAfter = false
	t.FixRound = round
	t.MaxFixRounds = wf.MaxRounds
	t.SkipPermissions = writer.SkipPermissions
	if writer.WriteDomain != nil {
		d := *writer.WriteDomain
		t.WriteDomain = &d
	}
	if err := pinWorkflowEngine(t, wf.WriterEngine); err != nil {
		return nil, err
	}
	if err := saveTask(root, t); err != nil {
		return nil, err
	}
	emitTaskEvent(root, t.ID, evQueued, "workflow:repair", statusQueued, 0, map[string]any{
		"workflow": wf.ID, "round": round, "engine": t.PreferRunner,
	})
	wf.WriterTaskID = t.ID
	wf.CurrentRound = round
	wf.Status = workflowStatusRepairing
	wf.ReviewerTaskID = ""
	if err := writeWorkflowProgress(root, wf); err != nil {
		return t, err
	}
	return t, saveWorkflow(root, wf)
}

func ingestWorkflowReview(root string, wf *WorkflowRecord) error {
	if err := refreshWorkflow(root, wf); err != nil {
		return err
	}
	if wf.ReviewerTaskID == "" {
		return fmt.Errorf("%w: no reviewer", errWorkflowMalformed)
	}
	review, err := findTaskAnywhere(root, wf.ReviewerTaskID)
	if err != nil {
		return err
	}
	raw := loadTaskResultForGate(root, review)
	v := parseReviewVerdict(raw)
	hold := reviewHoldReason(v, raw)
	snap := &WorkflowReview{TaskID: review.ID, HoldReason: hold}
	if v != nil {
		snap.Verdict = v.Verdict
		snap.P0 = append([]string{}, v.P0...)
		snap.P1 = append([]string{}, v.P1...)
		snap.P2 = append([]string{}, v.P2...)
	}
	if wf.Candidate != nil {
		snap.CandidateCommit = wf.Candidate.Commit
		snap.CandidateTree = wf.Candidate.Tree
	}
	if hold == "" && reviewVerdictIsAdmissiblePass(v) {
		if wf.Candidate == nil || (wf.Candidate.Commit == "" && wf.Candidate.Tree == "") {
			hold = holdReasonCandidateMismatch
			snap.HoldReason = hold
		} else {
			var writer *Task
			if wf.WriterTaskID != "" {
				writer, _ = findTaskAnywhere(root, wf.WriterTaskID)
			}
			if ok, reason := integrationCustodyOK(root, review, writer); !ok {
				hold = reason
				snap.HoldReason = hold
			} else {
				snap.EvidenceComplete = true
			}
		}
	}
	wf.Review = snap
	if snap.EvidenceComplete && reviewVerdictIsAdmissiblePass(v) && snap.HoldReason == "" {
		wf.Status = workflowStatusLiveReady
		wf.EffectGates.Integration = effectGateHeld
		_ = emitRootNotify(root, wf, rootNotifyLiveReady, "admissible pass; integration and live remain held")
	} else {
		wf.Status = workflowStatusIntegrationHeld
		wf.EffectGates.Integration = effectGateHeld
	}
	if err := writeWorkflowProgress(root, wf); err != nil {
		return err
	}
	return saveWorkflow(root, wf)
}

func tryReleaseWorkflowIntegration(root string, wf *WorkflowRecord) error {
	if err := refreshWorkflow(root, wf); err != nil {
		return err
	}
	if wf.IntegrationTaskID == "" {
		return fmt.Errorf("%w: no integration card", errWorkflowMalformed)
	}
	integ, err := loadTask(root, wf.IntegrationTaskID)
	if err != nil {
		return err
	}
	if integ.IntegrationGate != nil && wf.ReviewerTaskID != "" {
		integ.IntegrationGate.ReviewTaskID = wf.ReviewerTaskID
		integ.IntegrationGate.WriterTaskID = wf.WriterTaskID
		integ.IntegrationGate.WorkflowID = wf.ID
		if wf.Candidate != nil {
			integ.IntegrationGate.CandidateCommit = wf.Candidate.Commit
			integ.IntegrationGate.CandidateTree = wf.Candidate.Tree
		}
	}
	dec := evaluateIntegrationRelease(root, integ)
	if !dec.Admit {
		reason := dec.HoldReason
		if reason == "" {
			reason = holdReasonNotPass
		}
		if integ.Status != statusHeld {
			integ.Status = statusHeld
			_ = saveTask(root, integ)
		}
		if wf.Review != nil {
			wf.Review.HoldReason = reason
		}
		wf.EffectGates.Integration = effectGateHeld
		wf.Status = workflowStatusIntegrationHeld
		_ = writeWorkflowProgress(root, wf)
		_ = saveWorkflow(root, wf)
		return fmt.Errorf("%w: %s", errWorkflowReleaseHeld, reason)
	}
	if integ.Status == statusHeld {
		integ.Status = statusQueued
		integ.NotBeforeEpoch = 0
		if err := saveTask(root, integ); err != nil {
			return err
		}
		emitTaskEvent(root, integ.ID, evQueued, "workflow:release-integration", statusQueued, integ.Step, map[string]any{
			"workflow": wf.ID, "review": wf.ReviewerTaskID,
		})
	}
	wf.EffectGates.Integration = effectGateReleased
	wf.EffectGates.Live = effectGateHeld
	wf.EffectGates.Cutover = effectGateHeld
	if err := writeWorkflowProgress(root, wf); err != nil {
		return err
	}
	return saveWorkflow(root, wf)
}

func createHeldIntegrationTask(root string, cfg *Config, wf *WorkflowRecord) (*Task, error) {
	prompt := fmt.Sprintf("只集成 workflow %s 已接受候选。默认 held；仅在机器核验 verdict=pass 且 p0/p1 为空、候选与 custody 一致后才可 release。不得 live/cutover。", wf.ID)
	t := newTask(root, cfg, typeSequence, "workflow integrate: "+wf.ModuleID, wf.Worktree, []string{prompt}, 5)
	t.Project = wf.ModuleID
	t.WorkflowID = wf.ID
	t.Status = statusHeld
	t.ReviewAfter = false
	d := wf.WriteDomain
	t.WriteDomain = &WriteDomain{
		ID:        d.ID,
		Lineage:   d.Lineage,
		Component: d.Component,
		Paths:     append([]string{}, d.Paths...),
		Resources: append([]ResourceClaim{}, d.Resources...),
	}
	t.IntegrationGate = &IntegrationGate{WorkflowID: wf.ID, WriterTaskID: wf.WriterTaskID}
	if err := pinWorkflowEngine(t, wf.WriterEngine); err != nil {
		return nil, err
	}
	if err := saveTask(root, t); err != nil {
		return nil, err
	}
	emitTaskEvent(root, t.ID, evQueued, "workflow:integration", statusQueued, 0, map[string]any{"workflow": wf.ID})
	emitTaskEvent(root, t.ID, evHeld, "workflow:integration", statusHeld, 0,
		withCostTelemetry(map[string]any{"reason": "default-held-integration"}, t))
	wf.IntegrationTaskID = t.ID
	wf.EffectGates.Integration = effectGateHeld
	return t, nil
}

func advanceWorkflow(root string, cfg *Config, wf *WorkflowRecord) error {
	if wf == nil {
		return errWorkflowMalformed
	}
	if err := refreshWorkflow(root, wf); err != nil {
		return err
	}
	switch wf.Status {
	case workflowStatusExhausted, workflowStatusOwnerChoice, workflowStatusExternalBlocked:
		return nil
	}
	if err := dedupeWorkflowRoles(root, wf); err != nil && !strings.Contains(err.Error(), wf.WriterTaskID) && !strings.Contains(err.Error(), wf.ReviewerTaskID) {
		return err
	}
	if wf.WriterTaskID == "" {
		_, err := admitWorkflowWriter(root, cfg, wf, "")
		return err
	}
	writer, err := findTaskAnywhere(root, wf.WriterTaskID)
	if err != nil {
		return err
	}
	if writer.Status == statusQueued || writer.Status == statusRunning || writer.Status == statusLimitPaused || writer.Status == statusHeld {
		return nil
	}
	if writer.Status != statusDone {
		return nil
	}
	if wf.ReviewerTaskID == "" {
		_, err := admitWorkflowReviewer(root, cfg, wf)
		return err
	}
	review, err := findTaskAnywhere(root, wf.ReviewerTaskID)
	if err != nil {
		return err
	}
	if review.Status == statusQueued || review.Status == statusRunning || review.Status == statusLimitPaused {
		return nil
	}
	if review.Status != statusDone {
		return nil
	}
	if wf.Review == nil || wf.Review.TaskID != review.ID {
		if err := ingestWorkflowReview(root, wf); err != nil {
			return err
		}
	}
	if wf.Review != nil && wf.Review.EvidenceComplete && reviewVerdictIsAdmissiblePass(&reviewVerdict{Verdict: wf.Review.Verdict, P0: wf.Review.P0, P1: wf.Review.P1}) {
		return nil
	}
	if wf.Review != nil && (wf.Review.Verdict == "concerns" || wf.Review.Verdict == "block") {
		findings := ""
		for i, p := range wf.Review.P0 {
			findings += fmt.Sprintf("P0-%d: %s\n", i+1, p)
		}
		for i, p := range wf.Review.P1 {
			findings += fmt.Sprintf("P1-%d: %s\n", i+1, p)
		}
		if findings == "" {
			return nil
		}
		_, err := admitWorkflowRepair(root, cfg, wf, findings, "", wf.Review.Verdict)
		return err
	}
	return nil
}

func advanceWorkflows(root string, cfg *Config) {
	wfs, err := loadWorkflows(root)
	if err != nil {
		return
	}
	for _, wf := range wfs {
		_ = advanceWorkflow(root, cfg, wf)
	}
}
