package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	workflowSchemaV1 = "cardex.workflow.v1"

	workflowModeDirect    = "direct"
	workflowModeFederated = "federated"

	workflowStatusDesign          = "design"
	workflowStatusWriting         = "writing"
	workflowStatusReviewing       = "reviewing"
	workflowStatusRepairing       = "repairing"
	workflowStatusIntegrationHeld = "integration_held"
	workflowStatusLiveReady       = "live_ready"
	workflowStatusExhausted       = "exhausted"
	workflowStatusExternalBlocked = "external_blocked"
	workflowStatusOwnerChoice     = "owner_choice"

	workflowEngineGrokBuild = "grok-build"

	effectGateHeld     = "held"
	effectGateReleased = "released"

	rootNotifyLiveReady = "live_ready"
	rootNotifyExternal  = "true_external_dependency"
	rootNotifyOwner     = "owner_choice"
	rootNotifyExhausted = "exhausted_route"

	holdReasonMissingOutput      = "missing_output"
	holdReasonUnknownVocabulary  = "unknown_vocabulary"
	holdReasonConcerns           = "concerns"
	holdReasonBlock              = "block"
	holdReasonIncompleteEvidence = "incomplete_evidence"
	holdReasonCandidateMismatch  = "candidate_mismatch"
	holdReasonCustody            = "incomplete_custody"
	holdReasonNotPass            = "not_pass"
)

var (
	errWorkflowForbiddenEngine = errors.New("workflow forbids this implementation engine")
	errWorkflowUnknownEngine   = errors.New("workflow unknown implementation engine")
	errWorkflowDuplicateRole   = errors.New("workflow duplicate active role")
	errWorkflowWriteOverlap    = errors.New("workflow write domain overlap")
	errWorkflowReleaseHeld     = errors.New("workflow integration remains held")
	errWorkflowMalformed       = errors.New("workflow malformed")
	errWorkflowMode            = errors.New("workflow unknown mode")
)

// IntegrationGate is a fail-closed dispatch latch on a task. Durable review
// done is not enough: tick and cardex release both require a machine-checked
// admissible pass that matches the frozen candidate and custody evidence.
type IntegrationGate struct {
	ReviewTaskID    string `json:"review_task_id,omitempty"`
	WriterTaskID    string `json:"writer_task_id,omitempty"`
	WorkflowID      string `json:"workflow_id,omitempty"`
	CandidateCommit string `json:"candidate_commit,omitempty"`
	CandidateTree   string `json:"candidate_tree,omitempty"`
}

type WorkflowRecord struct {
	Schema            string                 `json:"schema"`
	ID                string                 `json:"id"`
	Mode              string                 `json:"mode"`
	ModuleID          string                 `json:"module_id"`
	GoalID            string                 `json:"goal_id"`
	Goal              string                 `json:"goal"`
	ParentID          string                 `json:"parent_id,omitempty"`
	Repo              string                 `json:"repo"`
	Worktree          string                 `json:"worktree"`
	WriteDomain       WriteDomain            `json:"write_domain"`
	TerminalCriteria  string                 `json:"terminal_criteria"`
	MaxRounds         int                    `json:"max_rounds"`
	CurrentRound      int                    `json:"current_round"`
	WriterEngine      string                 `json:"writer_engine"`
	ReviewerEngine    string                 `json:"reviewer_engine"`
	WriterTaskID      string                 `json:"writer_task_id,omitempty"`
	ReviewerTaskID    string                 `json:"reviewer_task_id,omitempty"`
	IntegrationTaskID string                 `json:"integration_task_id,omitempty"`
	Candidate         *WorkflowCandidate     `json:"candidate,omitempty"`
	Review            *WorkflowReview        `json:"review,omitempty"`
	EffectGates       WorkflowEffectGates    `json:"effect_gates"`
	Progress          WorkflowProgressCoords `json:"progress"`
	Status            string                 `json:"status"`
	MaterialNotify    *WorkflowNotify        `json:"material_notify,omitempty"`
	CreatedAt         string                 `json:"created_at"`
	UpdatedAt         string                 `json:"updated_at"`
}

type WorkflowCandidate struct {
	Commit       string   `json:"commit,omitempty"`
	Tree         string   `json:"tree,omitempty"`
	Branch       string   `json:"branch,omitempty"`
	ChangedPaths []string `json:"changed_paths,omitempty"`
}

type WorkflowReview struct {
	TaskID           string   `json:"task_id"`
	Verdict          string   `json:"verdict,omitempty"`
	P0               []string `json:"p0,omitempty"`
	P1               []string `json:"p1,omitempty"`
	P2               []string `json:"p2,omitempty"`
	EvidenceComplete bool     `json:"evidence_complete"`
	CandidateCommit  string   `json:"candidate_commit,omitempty"`
	CandidateTree    string   `json:"candidate_tree,omitempty"`
	HoldReason       string   `json:"hold_reason,omitempty"`
}

type WorkflowEffectGates struct {
	Integration string `json:"integration"`
	Live        string `json:"live"`
	Cutover     string `json:"cutover"`
}

type WorkflowProgressCoords struct {
	JSON     string `json:"json"`
	Markdown string `json:"markdown"`
}

type WorkflowNotify struct {
	Kind      string `json:"kind"`
	At        string `json:"at"`
	Summary   string `json:"summary"`
	Receipt   string `json:"receipt"`
	Workflow  string `json:"workflow_id"`
	Candidate string `json:"candidate,omitempty"`
	Tree      string `json:"tree,omitempty"`
}

type IntegrationReleaseDecision struct {
	Admit      bool
	HoldReason string
	Verdict    *reviewVerdict
	Review     *Task
}

func workflowsDir(root string) string { return filepath.Join(root, "workflows") }
func workflowPath(root, id string) string {
	return filepath.Join(workflowsDir(root), id+".json")
}
func workflowProgressJSONPath(root, id string) string {
	return filepath.Join(workflowsDir(root), id+".progress.json")
}
func workflowProgressMDPath(root, id string) string {
	return filepath.Join(workflowsDir(root), id+".progress.md")
}
func workflowNotifyDir(root string) string {
	return filepath.Join(workflowsDir(root), "root-notify")
}

func newWorkflowID(root string) string {
	for {
		b := make([]byte, 2)
		_, _ = rand.Read(b)
		id := fmt.Sprintf("wf%s-%s", time.Now().Format("0102-1504"), hex.EncodeToString(b))
		if _, err := os.Stat(workflowPath(root, id)); os.IsNotExist(err) {
			return id
		}
	}
}

func validateWorkflowEngine(engine string) error {
	e := strings.TrimSpace(strings.ToLower(engine))
	if e == "" {
		return fmt.Errorf("%w: empty", errWorkflowUnknownEngine)
	}
	switch {
	case e == "codex", e == "sol", strings.HasPrefix(e, "sol-"):
		return fmt.Errorf("%w: %s", errWorkflowForbiddenEngine, e)
	case e == workflowEngineGrokBuild:
		return nil
	default:
		return fmt.Errorf("%w: %s", errWorkflowUnknownEngine, e)
	}
}

func normalizeWorkflowRecord(wf *WorkflowRecord) error {
	if wf == nil {
		return errWorkflowMalformed
	}
	if wf.Schema != workflowSchemaV1 {
		return fmt.Errorf("%w: schema %q", errWorkflowMalformed, wf.Schema)
	}
	if !validIntegrationID(wf.ID) || !validIntegrationID(wf.ModuleID) || !validIntegrationID(wf.GoalID) {
		return fmt.Errorf("%w: id/module/goal", errWorkflowMalformed)
	}
	switch wf.Mode {
	case workflowModeDirect, workflowModeFederated:
	default:
		return fmt.Errorf("%w: %s", errWorkflowMode, wf.Mode)
	}
	if wf.ParentID != "" && !validIntegrationID(wf.ParentID) {
		return fmt.Errorf("%w: parent", errWorkflowMalformed)
	}
	if wf.Mode == workflowModeFederated && wf.ParentID == "" {
		// Program root is allowed to be parent-less; child modules must name a parent.
		// Presence of ParentID is checked by the caller when creating children.
	}
	if strings.TrimSpace(wf.Repo) == "" || strings.TrimSpace(wf.Worktree) == "" {
		return fmt.Errorf("%w: repo/worktree", errWorkflowMalformed)
	}
	if !filepath.IsAbs(wf.Repo) || !filepath.IsAbs(wf.Worktree) {
		return fmt.Errorf("%w: repo/worktree must be absolute", errWorkflowMalformed)
	}
	if strings.TrimSpace(wf.Goal) == "" || strings.TrimSpace(wf.TerminalCriteria) == "" {
		return fmt.Errorf("%w: goal/terminal criteria", errWorkflowMalformed)
	}
	if wf.MaxRounds < 1 {
		return fmt.Errorf("%w: max_rounds", errWorkflowMalformed)
	}
	if err := validateWorkflowEngine(wf.WriterEngine); err != nil {
		return err
	}
	if err := validateWorkflowEngine(wf.ReviewerEngine); err != nil {
		return err
	}
	repoRoot := wf.Repo
	if top, _, unc := resolveGitIdentity(wf.Worktree); !unc && top != "" {
		repoRoot = top
	}
	norm, err := NormalizeWriteDomain(repoRoot, wf.WriteDomain)
	if err != nil {
		return err
	}
	wf.WriteDomain = norm
	if wf.EffectGates.Integration == "" {
		wf.EffectGates.Integration = effectGateHeld
	}
	if wf.EffectGates.Live == "" {
		wf.EffectGates.Live = effectGateHeld
	}
	if wf.EffectGates.Cutover == "" {
		wf.EffectGates.Cutover = effectGateHeld
	}
	if wf.Status == "" {
		wf.Status = workflowStatusDesign
	}
	return nil
}

func saveWorkflow(root string, wf *WorkflowRecord) error {
	if err := normalizeWorkflowRecord(wf); err != nil {
		return err
	}
	wf.UpdatedAt = time.Now().Format(time.RFC3339)
	if wf.CreatedAt == "" {
		wf.CreatedAt = wf.UpdatedAt
	}
	if err := os.MkdirAll(workflowsDir(root), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(wf, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteMode(workflowPath(root, wf.ID), append(data, '\n'), 0o600)
}

func refreshWorkflow(root string, wf *WorkflowRecord) error {
	if wf == nil || wf.ID == "" {
		return errWorkflowMalformed
	}
	latest, err := loadWorkflow(root, wf.ID)
	if err != nil {
		return err
	}
	*wf = *latest
	return nil
}

func loadWorkflow(root, id string) (*WorkflowRecord, error) {
	data, err := os.ReadFile(workflowPath(root, id))
	if err != nil {
		return nil, err
	}
	var wf WorkflowRecord
	if err := json.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("parse workflow %s: %w", id, err)
	}
	if err := normalizeWorkflowRecord(&wf); err != nil {
		return nil, err
	}
	return &wf, nil
}

func loadWorkflows(root string) ([]*WorkflowRecord, error) {
	entries, err := os.ReadDir(workflowsDir(root))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*WorkflowRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.Contains(e.Name(), ".progress.") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		wf, err := loadWorkflow(root, id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "警告: 跳过损坏的 workflow 文件 %s: %v\n", e.Name(), err)
			continue
		}
		out = append(out, wf)
	}
	return out, nil
}

func workflowRepoIdentity(wf *WorkflowRecord) (key, repoRoot string) {
	top, common, unc := resolveGitIdentity(wf.Worktree)
	if !unc && common != "" {
		key = common
	} else if !unc && top != "" {
		key = top
	} else {
		key = physicalDirKey(wf.Repo)
	}
	if top != "" {
		repoRoot = top
	} else {
		repoRoot = physicalDirKey(wf.Worktree)
	}
	return key, repoRoot
}

func auditWorkflowWriteDomains(root string, incoming *WorkflowRecord) error {
	if incoming == nil {
		return errWorkflowMalformed
	}
	existing, err := loadWorkflows(root)
	if err != nil {
		return err
	}
	type grouped struct {
		repoRoot string
		domains  []WriteDomain
	}
	byRepo := map[string]*grouped{}
	var all []WriteDomain
	add := func(wf *WorkflowRecord) error {
		if wf.Status == workflowStatusExhausted || wf.Status == workflowStatusOwnerChoice {
			return nil
		}
		key, repoRoot := workflowRepoIdentity(wf)
		g := byRepo[key]
		if g == nil {
			g = &grouped{repoRoot: repoRoot}
			byRepo[key] = g
		}
		norm, err := NormalizeWriteDomain(repoRoot, wf.WriteDomain)
		if err != nil {
			return err
		}
		g.domains = append(g.domains, norm)
		all = append(all, norm)
		return nil
	}
	for _, wf := range existing {
		if wf.ID == incoming.ID {
			continue
		}
		if err := add(wf); err != nil {
			return fmt.Errorf("%w: %v", errWorkflowWriteOverlap, err)
		}
	}
	if err := add(incoming); err != nil {
		return err
	}
	for _, g := range byRepo {
		if _, err := AuditWriteDomains(g.repoRoot, g.domains); err != nil {
			return fmt.Errorf("%w: %v", errWorkflowWriteOverlap, err)
		}
	}
	for _, c := range detectWriteDomainOverlaps(all) {
		if c.Kind == overlapKindResource {
			return fmt.Errorf("%w: shared resource %s:%s", errWorkflowWriteOverlap, c.Resource.Kind, c.Resource.ID)
		}
	}
	return nil
}

func atomicWriteMode(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
