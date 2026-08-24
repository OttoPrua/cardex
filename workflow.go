package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Workflow modes are the durable half of docs/workflows.md. `serial` is mode A
// (design → writer → independent review → held integration → separate live
// gate); `federated` is mode B, where a parent program record joins child
// module records that each own a disjoint write domain.
//
// The record is state, not a scheduler. Nothing here walks the graph on its
// own: every transition is an explicit `cardex workflow` step. Cardex keeps
// being the single task authority, and the only thing this package adds to
// dispatch is the fail-closed integration latch in workflow_gate.go.
const (
	workflowSchemaV1 = "cardex.workflow.v1"

	workflowModeSerial    = "serial"
	workflowModeFederated = "federated"

	workflowStatusDesign          = "design"
	workflowStatusWriting         = "writing"
	workflowStatusReviewing       = "reviewing"
	workflowStatusRepairing       = "repairing"
	workflowStatusIntegrationHeld = "integration_held"
	workflowStatusReviewPassed    = "review_passed"
	workflowStatusIntegrating     = "integrating"
	workflowStatusExhausted       = "exhausted"
	workflowStatusExternalBlocked = "external_blocked"
	workflowStatusOwnerChoice     = "owner_choice"

	effectGateHeld     = "held"
	effectGateReleased = "released"

	rootNotifyReviewPassed = "review_passed"
	rootNotifyExternal     = "true_external_dependency"
	rootNotifyOwner        = "owner_choice"
	rootNotifyExhausted    = "exhausted_route"

	holdReasonMissingReview      = "missing_review_task"
	holdReasonMissingOutput      = "missing_output"
	holdReasonUnknownVocabulary  = "unknown_vocabulary"
	holdReasonConcerns           = "concerns"
	holdReasonBlock              = "block"
	holdReasonFindingsOpen       = "pass_with_open_findings"
	holdReasonIncompleteEvidence = "incomplete_evidence"
	holdReasonCandidateMismatch  = "candidate_mismatch"
	holdReasonCustody            = "incomplete_custody"
	holdReasonNoGate             = "no_integration_gate"
)

var (
	errWorkflowUnknownEngine  = errors.New("workflow engine cannot be pinned without fail-open")
	errWorkflowDuplicateRole  = errors.New("workflow duplicate active role")
	errWorkflowWriteOverlap   = errors.New("workflow write domain overlap")
	errWorkflowHeld           = errors.New("workflow integration remains held")
	errWorkflowMalformed      = errors.New("workflow malformed")
	errWorkflowMode           = errors.New("workflow unknown mode")
	errWorkflowRoundsExceeded = errors.New("workflow repair rounds exhausted")
	errWorkflowParent         = errors.New("workflow parent binding")
)

// IntegrationGate is a fail-closed dispatch latch carried on an integration
// task. A durable review `done` is deliberately not enough: both tick and
// `cardex release` re-derive an admissible `verdict=pass` (empty p0/p1) that
// matches the frozen candidate and passes the custody checks before the card
// may leave `held`.
type IntegrationGate struct {
	WorkflowID      string `json:"workflow_id,omitempty"`
	ReviewTaskID    string `json:"review_task_id,omitempty"`
	WriterTaskID    string `json:"writer_task_id,omitempty"`
	CandidateCommit string `json:"candidate_commit,omitempty"`
	CandidateTree   string `json:"candidate_tree,omitempty"`
}

// WorkflowRecord is the durable module/program goal binding.
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

// WorkflowCandidate is the frozen source identity a review is bound to.
type WorkflowCandidate struct {
	Commit       string   `json:"commit,omitempty"`
	Tree         string   `json:"tree,omitempty"`
	Branch       string   `json:"branch,omitempty"`
	ChangedPaths []string `json:"changed_paths,omitempty"`
}

// WorkflowReview is the machine-readable projection of one review terminal.
// Admissible is only ever set from a re-derived parse plus custody checks.
type WorkflowReview struct {
	TaskID          string   `json:"task_id"`
	Round           int      `json:"round"`
	Verdict         string   `json:"verdict,omitempty"`
	P0              []string `json:"p0,omitempty"`
	P1              []string `json:"p1,omitempty"`
	P2              []string `json:"p2,omitempty"`
	Admissible      bool     `json:"admissible"`
	CandidateCommit string   `json:"candidate_commit,omitempty"`
	CandidateTree   string   `json:"candidate_tree,omitempty"`
	HoldReason      string   `json:"hold_reason,omitempty"`
}

// WorkflowEffectGates keeps integration, live, and cutover as three separate
// held gates. Releasing integration never implies the other two.
type WorkflowEffectGates struct {
	Integration string `json:"integration"`
	Live        string `json:"live"`
	Cutover     string `json:"cutover"`
}

// WorkflowProgressCoords records where routine progress is written so a
// manager can read it without a chat turn.
type WorkflowProgressCoords struct {
	JSON     string `json:"json"`
	Markdown string `json:"markdown"`
}

// WorkflowNotify is a material Root-facing receipt. Routine progress never
// produces one.
type WorkflowNotify struct {
	Kind      string `json:"kind"`
	At        string `json:"at"`
	Summary   string `json:"summary"`
	Receipt   string `json:"receipt"`
	Workflow  string `json:"workflow_id"`
	Candidate string `json:"candidate,omitempty"`
	Tree      string `json:"tree,omitempty"`
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

func workflowNotifyPath(root, id, kind string) string {
	// One receipt per (workflow, kind) so replaying a material transition
	// rewrites the same file instead of accumulating duplicates.
	return filepath.Join(workflowNotifyDir(root), id+"-"+kind+".json")
}

// newWorkflowID mints a collision-checked ID. Unlike task IDs it must also be
// a valid integration identifier so it can key write-domain audits.
func newWorkflowID(root string) (string, error) {
	for attempt := 0; attempt < 64; attempt++ {
		b := make([]byte, 3)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		id := fmt.Sprintf("wf%s-%s", time.Now().Format("0102-1504"), hex.EncodeToString(b))
		if !validIntegrationID(id) {
			continue
		}
		if _, err := os.Stat(workflowPath(root, id)); os.IsNotExist(err) {
			return id, nil
		}
	}
	return "", fmt.Errorf("%w: cannot mint workflow id", errWorkflowMalformed)
}

// validateWorkflowEngine accepts only runner identities that tick pins without
// fail-open. An unknown name would silently land on the default provider,
// which destroys the writer/reviewer engine separation the mode depends on.
func validateWorkflowEngine(cfg *Config, engine string) error {
	e := strings.TrimSpace(engine)
	if e == "" {
		return fmt.Errorf("%w: empty", errWorkflowUnknownEngine)
	}
	switch e {
	case "claude", "codex", "gemini", "opencode", kimiCLIRunnerName, grokBuildRunnerName, cursorRunnerName:
		return nil
	}
	if cfg != nil {
		if _, ok := cfg.Engines[e]; ok {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", errWorkflowUnknownEngine, e)
}

func normalizeWorkflowRecord(cfg *Config, wf *WorkflowRecord) error {
	if wf == nil {
		return errWorkflowMalformed
	}
	if wf.Schema != workflowSchemaV1 {
		return fmt.Errorf("%w: schema %q", errWorkflowMalformed, wf.Schema)
	}
	if !validIntegrationID(wf.ID) || !validIntegrationID(wf.ModuleID) || !validIntegrationID(wf.GoalID) {
		return fmt.Errorf("%w: id/module/goal must be lowercase identifiers", errWorkflowMalformed)
	}
	switch wf.Mode {
	case workflowModeSerial:
		if wf.ParentID != "" {
			return fmt.Errorf("%w: serial mode has no parent", errWorkflowParent)
		}
	case workflowModeFederated:
		if wf.ParentID != "" && !validIntegrationID(wf.ParentID) {
			return fmt.Errorf("%w: malformed parent id", errWorkflowParent)
		}
		if wf.ParentID == wf.ID {
			return fmt.Errorf("%w: self parent", errWorkflowParent)
		}
	default:
		return fmt.Errorf("%w: %s", errWorkflowMode, wf.Mode)
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
		return fmt.Errorf("%w: max_rounds must be >= 1", errWorkflowMalformed)
	}
	if wf.CurrentRound < 0 || wf.CurrentRound > wf.MaxRounds {
		return fmt.Errorf("%w: current_round %d out of 0..%d", errWorkflowMalformed, wf.CurrentRound, wf.MaxRounds)
	}
	if err := validateWorkflowEngine(cfg, wf.WriterEngine); err != nil {
		return err
	}
	if err := validateWorkflowEngine(cfg, wf.ReviewerEngine); err != nil {
		return err
	}
	norm, err := NormalizeWriteDomain(workflowRepoRoot(wf), wf.WriteDomain)
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
	// Live and cutover have no release path in this tree at all; a record that
	// claims otherwise is corrupt, not merely stale.
	if wf.EffectGates.Live != effectGateHeld || wf.EffectGates.Cutover != effectGateHeld {
		return fmt.Errorf("%w: live/cutover gates are held-only", errWorkflowMalformed)
	}
	if wf.Status == "" {
		wf.Status = workflowStatusDesign
	}
	return nil
}

// workflowRepoRoot prefers the Git top-level of the worktree so that linked
// worktrees of the same repository normalize their paths identically.
func workflowRepoRoot(wf *WorkflowRecord) string {
	if top, _, unc := resolveGitIdentity(wf.Worktree); !unc && top != "" {
		return top
	}
	return wf.Repo
}

func saveWorkflow(root string, cfg *Config, wf *WorkflowRecord) error {
	if err := normalizeWorkflowRecord(cfg, wf); err != nil {
		return err
	}
	wf.UpdatedAt = time.Now().Format(time.RFC3339)
	if wf.CreatedAt == "" {
		wf.CreatedAt = wf.UpdatedAt
	}
	if err := os.MkdirAll(workflowsDir(root), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(wf, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteSync(workflowPath(root, wf.ID), append(data, '\n'))
}

func loadWorkflow(root string, cfg *Config, id string) (*WorkflowRecord, error) {
	if !validIntegrationID(id) {
		return nil, fmt.Errorf("%w: workflow id %q", errWorkflowMalformed, id)
	}
	data, err := os.ReadFile(workflowPath(root, id))
	if err != nil {
		return nil, err
	}
	var wf WorkflowRecord
	if err := json.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("parse workflow %s: %w", id, err)
	}
	if err := normalizeWorkflowRecord(cfg, &wf); err != nil {
		return nil, err
	}
	return &wf, nil
}

// refreshWorkflow re-reads from disk so a caller holding a stale in-memory copy
// cannot replay an old round or resurrect a cleared reviewer binding.
func refreshWorkflow(root string, cfg *Config, wf *WorkflowRecord) error {
	if wf == nil || wf.ID == "" {
		return errWorkflowMalformed
	}
	latest, err := loadWorkflow(root, cfg, wf.ID)
	if err != nil {
		return err
	}
	*wf = *latest
	return nil
}

func loadWorkflows(root string, cfg *Config) ([]*WorkflowRecord, error) {
	entries, err := os.ReadDir(workflowsDir(root))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*WorkflowRecord
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".progress.json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		wf, err := loadWorkflow(root, cfg, id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "警告: 跳过损坏的 workflow 文件 %s: %v\n", name, err)
			continue
		}
		out = append(out, wf)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// workflowClaimsActive reports whether a record still owns its write domain.
// Terminal routes release the claim so a successor module can take the paths.
func workflowClaimsActive(wf *WorkflowRecord) bool {
	switch wf.Status {
	case workflowStatusExhausted, workflowStatusOwnerChoice, workflowStatusExternalBlocked:
		return false
	}
	return true
}

// auditWorkflowWriteDomains fail-closes a new or edited record against every
// other live record. Paths are compared per Git identity (linked worktrees of
// one repository share it); closed resources are compared globally, because a
// shared database or manifest serializes across repositories too.
func auditWorkflowWriteDomains(root string, cfg *Config, incoming *WorkflowRecord) error {
	if incoming == nil {
		return errWorkflowMalformed
	}
	existing, err := loadWorkflows(root, cfg)
	if err != nil {
		return err
	}
	byRepo := map[string][]WriteDomain{}
	repoRoots := map[string]string{}
	var all []WriteDomain
	add := func(wf *WorkflowRecord) error {
		if !workflowClaimsActive(wf) {
			return nil
		}
		repoRoot := workflowRepoRoot(wf)
		key := integrationRepoKey(wf.Worktree)
		if key == "" {
			key = physicalDirKey(repoRoot)
		}
		norm, err := NormalizeWriteDomain(repoRoot, wf.WriteDomain)
		if err != nil {
			return err
		}
		if _, ok := repoRoots[key]; !ok {
			repoRoots[key] = repoRoot
		}
		byRepo[key] = append(byRepo[key], norm)
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
	for key, domains := range byRepo {
		if _, err := AuditWriteDomains(repoRoots[key], domains); err != nil {
			return fmt.Errorf("%w: %v", errWorkflowWriteOverlap, err)
		}
	}
	for _, c := range detectWriteDomainOverlaps(all) {
		if c.Kind == overlapKindResource {
			return fmt.Errorf("%w: shared resource %s:%s between %s and %s",
				errWorkflowWriteOverlap, c.Resource.Kind, c.Resource.ID, c.DomainA, c.DomainB)
		}
	}
	return nil
}
