package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func cmdWorkflow(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: cardex workflow init|list|show|writer|freeze-candidate|review|ingest-review|repair|try-release-integration|mark ...")
	}
	switch args[0] {
	case "init":
		return cmdWorkflowInit(args[1:])
	case "list":
		return cmdWorkflowList(args[1:])
	case "show":
		return cmdWorkflowShow(args[1:])
	case "writer":
		return cmdWorkflowWriter(args[1:])
	case "freeze-candidate":
		return cmdWorkflowFreeze(args[1:])
	case "review":
		return cmdWorkflowReview(args[1:])
	case "ingest-review":
		return cmdWorkflowIngest(args[1:])
	case "repair":
		return cmdWorkflowRepair(args[1:])
	case "try-release-integration":
		return cmdWorkflowTryRelease(args[1:])
	case "mark":
		return cmdWorkflowMark(args[1:])
	default:
		return fmt.Errorf("未知 workflow 子命令 %s", args[0])
	}
}

// workflowTarget resolves the shared `-root <id>` shape used by every
// per-record subcommand.
func workflowTarget(fs *flag.FlagSet, rootFlag *string, usage string) (string, *Config, *WorkflowRecord, error) {
	if fs.NArg() < 1 {
		return "", nil, nil, fmt.Errorf("用法: %s", usage)
	}
	root := resolveRoot(*rootFlag)
	cfg, err := loadConfig(root)
	if err != nil {
		return "", nil, nil, err
	}
	wf, err := loadWorkflow(root, cfg, fs.Arg(0))
	if err != nil {
		return "", nil, nil, err
	}
	return root, cfg, wf, nil
}

// firstNonBlank is firstNonEmpty with trimming, so a flag holding only spaces
// falls through to the default instead of becoming a blank identifier.
func firstNonBlank(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func parseResourceClaims(csv string) ([]ResourceClaim, error) {
	var out []ResourceClaim
	for _, raw := range splitComma(csv) {
		kind, id, ok := strings.Cut(raw, ":")
		if !ok {
			return nil, fmt.Errorf("%w: %q（应为 kind:id）", errWriteDomainUnknownResource, raw)
		}
		out = append(out, ResourceClaim{Kind: strings.TrimSpace(kind), ID: strings.TrimSpace(id)})
	}
	return out, nil
}

func cmdWorkflowInit(args []string) error {
	fs := flag.NewFlagSet("workflow init", flag.ContinueOnError)
	rootFlag := fs.String("root", "", "数据目录")
	mode := fs.String("mode", workflowModeSerial, "serial（直派串联）或 federated（联邦模块）")
	moduleID := fs.String("module", "", "模块 ID（小写标识符）")
	goalID := fs.String("goal-id", "", "目标 ID，默认同 -module")
	goal := fs.String("goal", "", "目标陈述")
	parent := fs.String("parent", "", "联邦父 workflow ID（仅 federated）")
	dir := fs.String("dir", "", "模块 worktree")
	repo := fs.String("repo", "", "仓库根，默认取 worktree 的 git top-level")
	criteria := fs.String("terminal-criteria", "", "模块完成标准")
	maxRounds := fs.Int("max-rounds", 3, "最大修复轮次（>=1）")
	writeDomainID := fs.String("write-domain-id", "", "写域 ID，默认同 -module")
	writeDomainLineage := fs.String("write-domain-lineage", "", "写域谱系，默认 <module>-lineage")
	writeDomainComponent := fs.String("write-domain-component", "", "写域组件，默认同 -module")
	writePaths := fs.String("write-paths", "", "逗号分隔的仓相对写路径")
	writeResources := fs.String("write-resources", "", "逗号分隔的封闭资源 kind:id")
	engine := fs.String("engine", "", "writer/reviewer 引擎（必填，必须是 tick 能钉定且不会 fail-open 的执行器）")
	writerEngine := fs.String("writer-engine", "", "写者引擎，默认同 -engine")
	reviewerEngine := fs.String("reviewer-engine", "", "审核引擎，默认同 -engine")
	if err := fs.Parse(args); err != nil {
		return err
	}

	root := resolveRoot(*rootFlag)
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*moduleID) == "" {
		return fmt.Errorf("-module 不能为空")
	}
	if strings.TrimSpace(*goal) == "" {
		return fmt.Errorf("-goal 不能为空")
	}
	if strings.TrimSpace(*criteria) == "" {
		return fmt.Errorf("-terminal-criteria 不能为空")
	}
	wd, err := resolveDir(*dir)
	if err != nil {
		return err
	}
	repoRoot := strings.TrimSpace(*repo)
	if repoRoot == "" {
		if top, _, unc := resolveGitIdentity(wd); !unc && top != "" {
			repoRoot = top
		} else {
			repoRoot = wd
		}
	} else if repoRoot, err = filepath.Abs(repoRoot); err != nil {
		return err
	}

	wEngine, rEngine := strings.TrimSpace(*writerEngine), strings.TrimSpace(*reviewerEngine)
	if wEngine == "" {
		wEngine = strings.TrimSpace(*engine)
	}
	if rEngine == "" {
		rEngine = strings.TrimSpace(*engine)
	}
	if wEngine == "" || rEngine == "" {
		return fmt.Errorf("%w: 必须显式指定 -engine（或 -writer-engine/-reviewer-engine）", errWorkflowUnknownEngine)
	}

	domain := WriteDomain{
		ID:        firstNonBlank(*writeDomainID, *moduleID),
		Lineage:   firstNonBlank(*writeDomainLineage, *moduleID+"-lineage"),
		Component: firstNonBlank(*writeDomainComponent, *moduleID),
		Paths:     splitComma(*writePaths),
	}
	if domain.Resources, err = parseResourceClaims(*writeResources); err != nil {
		return err
	}

	id, err := newWorkflowID(root)
	if err != nil {
		return err
	}
	wf := &WorkflowRecord{
		Schema:           workflowSchemaV1,
		ID:               id,
		Mode:             strings.TrimSpace(*mode),
		ModuleID:         strings.TrimSpace(*moduleID),
		GoalID:           firstNonBlank(*goalID, *moduleID),
		Goal:             strings.TrimSpace(*goal),
		ParentID:         strings.TrimSpace(*parent),
		Repo:             repoRoot,
		Worktree:         wd,
		WriteDomain:      domain,
		TerminalCriteria: strings.TrimSpace(*criteria),
		MaxRounds:        *maxRounds,
		WriterEngine:     wEngine,
		ReviewerEngine:   rEngine,
		EffectGates: WorkflowEffectGates{
			Integration: effectGateHeld,
			Live:        effectGateHeld,
			Cutover:     effectGateHeld,
		},
		Status: workflowStatusDesign,
	}
	if err := normalizeWorkflowRecord(cfg, wf); err != nil {
		return err
	}
	if wf.Mode == workflowModeFederated && wf.ParentID != "" {
		if _, err := loadWorkflow(root, cfg, wf.ParentID); err != nil {
			return fmt.Errorf("%w: parent %s not loadable: %v", errWorkflowParent, wf.ParentID, err)
		}
	}
	if err := auditWorkflowWriteDomains(root, cfg, wf); err != nil {
		return err
	}
	if _, err := createHeldIntegrationTask(root, cfg, wf); err != nil {
		return err
	}
	if err := persistWorkflow(root, cfg, wf); err != nil {
		return err
	}
	fmt.Printf("已创建 workflow %s mode=%s module=%s integration=%s（held）\n",
		wf.ID, wf.Mode, wf.ModuleID, wf.IntegrationTaskID)
	return nil
}

func cmdWorkflowList(args []string) error {
	fs := flag.NewFlagSet("workflow list", flag.ContinueOnError)
	rootFlag := fs.String("root", "", "数据目录")
	asJSON := fs.Bool("json", false, "输出 JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root := resolveRoot(*rootFlag)
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	wfs, err := loadWorkflows(root, cfg)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(wfs)
	}
	if len(wfs) == 0 {
		fmt.Println("没有 workflow。用 cardex workflow init 创建。")
		return nil
	}
	for _, wf := range wfs {
		fmt.Printf("%s\t%s\t%s\t%s\tround %d/%d\tintegration=%s\tlive=%s\n",
			wf.ID, wf.Mode, wf.ModuleID, wf.Status, wf.CurrentRound, wf.MaxRounds,
			wf.EffectGates.Integration, wf.EffectGates.Live)
	}
	return nil
}

func cmdWorkflowShow(args []string) error {
	fs := flag.NewFlagSet("workflow show", flag.ContinueOnError)
	rootFlag := fs.String("root", "", "数据目录")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, _, wf, err := workflowTarget(fs, rootFlag, "cardex workflow show <id>")
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(wf)
}

func cmdWorkflowWriter(args []string) error {
	fs := flag.NewFlagSet("workflow writer", flag.ContinueOnError)
	rootFlag := fs.String("root", "", "数据目录")
	prompt := fs.String("prompt", "", "覆盖写者 prompt（默认用 workflow-writer 模板）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, cfg, wf, err := workflowTarget(fs, rootFlag, "cardex workflow writer <id> [-prompt ...]")
	if err != nil {
		return err
	}
	t, err := admitWorkflowWriter(root, cfg, wf, *prompt)
	if err != nil {
		return err
	}
	fmt.Printf("workflow %s writer=%s round=%d engine=%s\n", wf.ID, t.ID, wf.CurrentRound, t.PreferRunner)
	return nil
}

func cmdWorkflowFreeze(args []string) error {
	fs := flag.NewFlagSet("workflow freeze-candidate", flag.ContinueOnError)
	rootFlag := fs.String("root", "", "数据目录")
	commit := fs.String("commit", "", "候选 commit")
	tree := fs.String("tree", "", "候选 tree")
	branch := fs.String("branch", "", "候选分支")
	paths := fs.String("changed-paths", "", "逗号分隔 changed paths")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, cfg, wf, err := workflowTarget(fs, rootFlag,
		"cardex workflow freeze-candidate <id> -commit C -tree T")
	if err != nil {
		return err
	}
	cand := WorkflowCandidate{
		Commit:       *commit,
		Tree:         *tree,
		Branch:       *branch,
		ChangedPaths: splitComma(*paths),
	}
	if err := freezeWorkflowCandidate(root, cfg, wf, cand); err != nil {
		return err
	}
	fmt.Printf("workflow %s 已冻结候选 commit=%s tree=%s\n", wf.ID, wf.Candidate.Commit, wf.Candidate.Tree)
	return nil
}

func cmdWorkflowReview(args []string) error {
	fs := flag.NewFlagSet("workflow review", flag.ContinueOnError)
	rootFlag := fs.String("root", "", "数据目录")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, cfg, wf, err := workflowTarget(fs, rootFlag, "cardex workflow review <id>")
	if err != nil {
		return err
	}
	t, err := admitWorkflowReviewer(root, cfg, wf)
	if err != nil {
		return err
	}
	fmt.Printf("workflow %s reviewer=%s review_of=%s engine=%s\n", wf.ID, t.ID, t.ReviewOf, t.PreferRunner)
	return nil
}

func cmdWorkflowIngest(args []string) error {
	fs := flag.NewFlagSet("workflow ingest-review", flag.ContinueOnError)
	rootFlag := fs.String("root", "", "数据目录")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, cfg, wf, err := workflowTarget(fs, rootFlag, "cardex workflow ingest-review <id>")
	if err != nil {
		return err
	}
	if err := ingestWorkflowReview(root, cfg, wf); err != nil {
		return err
	}
	verdict, hold := "", ""
	if wf.Review != nil {
		verdict, hold = wf.Review.Verdict, wf.Review.HoldReason
	}
	fmt.Printf("workflow %s status=%s verdict=%s hold=%s integration=%s\n",
		wf.ID, wf.Status, orDash(verdict), orDash(hold), wf.EffectGates.Integration)
	return nil
}

func cmdWorkflowRepair(args []string) error {
	fs := flag.NewFlagSet("workflow repair", flag.ContinueOnError)
	rootFlag := fs.String("root", "", "数据目录")
	summary := fs.String("summary", "", "修复轮摘要")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, cfg, wf, err := workflowTarget(fs, rootFlag, "cardex workflow repair <id> [-summary ...]")
	if err != nil {
		return err
	}
	findings := ""
	if wf.Review != nil {
		for i, p := range wf.Review.P0 {
			findings += fmt.Sprintf("P0-%d: %s\n", i+1, p)
		}
		for i, p := range wf.Review.P1 {
			findings += fmt.Sprintf("P1-%d: %s\n", i+1, p)
		}
	}
	t, err := admitWorkflowRepair(root, cfg, wf, findings, *summary)
	if err != nil {
		return err
	}
	fmt.Printf("workflow %s repair=%s round=%d/%d\n", wf.ID, t.ID, wf.CurrentRound, wf.MaxRounds)
	return nil
}

func cmdWorkflowTryRelease(args []string) error {
	fs := flag.NewFlagSet("workflow try-release-integration", flag.ContinueOnError)
	rootFlag := fs.String("root", "", "数据目录")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, cfg, wf, err := workflowTarget(fs, rootFlag, "cardex workflow try-release-integration <id>")
	if err != nil {
		return err
	}
	if err := tryReleaseWorkflowIntegration(root, cfg, wf); err != nil {
		return err
	}
	fmt.Printf("workflow %s integration=%s（live=%s cutover=%s 仍 held）\n",
		wf.ID, wf.EffectGates.Integration, wf.EffectGates.Live, wf.EffectGates.Cutover)
	return nil
}

func cmdWorkflowMark(args []string) error {
	fs := flag.NewFlagSet("workflow mark", flag.ContinueOnError)
	rootFlag := fs.String("root", "", "数据目录")
	kind := fs.String("kind", "", "external | owner | exhausted")
	summary := fs.String("summary", "", "简要原因")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, cfg, wf, err := workflowTarget(fs, rootFlag,
		"cardex workflow mark <id> -kind external|owner|exhausted -summary ...")
	if err != nil {
		return err
	}
	if err := markWorkflowRoute(root, cfg, wf, *kind, *summary); err != nil {
		return err
	}
	fmt.Printf("workflow %s status=%s（已写 Root 通知 %s）\n", wf.ID, wf.Status, wf.MaterialNotify.Kind)
	return nil
}
