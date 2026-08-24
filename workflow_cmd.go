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
		return fmt.Errorf("用法: cardex workflow init|list|show|advance|freeze-candidate|ingest-review|try-release-integration|mark ...")
	}
	switch args[0] {
	case "init":
		return cmdWorkflowInit(args[1:])
	case "list":
		return cmdWorkflowList(args[1:])
	case "show":
		return cmdWorkflowShow(args[1:])
	case "advance":
		return cmdWorkflowAdvance(args[1:])
	case "freeze-candidate":
		return cmdWorkflowFreeze(args[1:])
	case "ingest-review":
		return cmdWorkflowIngest(args[1:])
	case "try-release-integration":
		return cmdWorkflowTryRelease(args[1:])
	case "mark":
		return cmdWorkflowMark(args[1:])
	default:
		return fmt.Errorf("未知 workflow 子命令 %s", args[0])
	}
}

func cmdWorkflowInit(args []string) error {
	fs := flag.NewFlagSet("workflow init", flag.ExitOnError)
	rootFlag := fs.String("root", "", "数据目录")
	mode := fs.String("mode", workflowModeDirect, "direct 或 federated")
	moduleID := fs.String("module", "", "模块 ID")
	goalID := fs.String("goal-id", "", "目标 ID")
	goal := fs.String("goal", "", "目标陈述")
	parent := fs.String("parent", "", "联邦父 workflow ID")
	dir := fs.String("dir", "", "模块 worktree")
	repo := fs.String("repo", "", "仓库根（默认与 worktree 的 git top 相同）")
	criteria := fs.String("terminal-criteria", "", "模块完成标准")
	maxRounds := fs.Int("max-rounds", 3, "最大修复轮次")
	writeDomainID := fs.String("write-domain-id", "", "写域 ID")
	writeDomainLineage := fs.String("write-domain-lineage", "", "写域谱系")
	writeDomainComponent := fs.String("write-domain-component", "", "写域组件")
	writePaths := fs.String("write-paths", "", "逗号分隔仓相对路径")
	writeResources := fs.String("write-resources", "", "逗号分隔 kind:id")
	engine := fs.String("engine", workflowEngineGrokBuild, "writer/reviewer 引擎（只允许 grok-build）")
	_ = fs.Parse(args)

	root := resolveRoot(*rootFlag)
	cfg, err := loadConfig(root)
	if err != nil {
		return err
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
	} else {
		repoRoot, err = filepath.Abs(repoRoot)
		if err != nil {
			return err
		}
	}
	if *goalID == "" {
		*goalID = *moduleID
	}
	if *writeDomainID == "" {
		*writeDomainID = *moduleID
	}
	if *writeDomainLineage == "" {
		*writeDomainLineage = *moduleID + "-lineage"
	}
	if *writeDomainComponent == "" {
		*writeDomainComponent = *moduleID
	}
	if strings.TrimSpace(*goal) == "" {
		return fmt.Errorf("-goal 不能为空")
	}
	if strings.TrimSpace(*criteria) == "" {
		return fmt.Errorf("-terminal-criteria 不能为空")
	}
	if strings.TrimSpace(*moduleID) == "" {
		return fmt.Errorf("-module 不能为空")
	}
	if err := validateWorkflowEngine(*engine); err != nil {
		return err
	}
	domain := WriteDomain{
		ID:        *writeDomainID,
		Lineage:   *writeDomainLineage,
		Component: *writeDomainComponent,
		Paths:     splitComma(*writePaths),
	}
	if *writeResources != "" {
		for _, raw := range splitComma(*writeResources) {
			kind, rid, ok := strings.Cut(raw, ":")
			if !ok {
				return fmt.Errorf("%w: %q", errWriteDomainUnknownResource, raw)
			}
			domain.Resources = append(domain.Resources, ResourceClaim{Kind: strings.TrimSpace(kind), ID: strings.TrimSpace(rid)})
		}
	}
	wf := &WorkflowRecord{
		Schema:           workflowSchemaV1,
		ID:               newWorkflowID(root),
		Mode:             *mode,
		ModuleID:         *moduleID,
		GoalID:           *goalID,
		Goal:             *goal,
		ParentID:         strings.TrimSpace(*parent),
		Repo:             repoRoot,
		Worktree:         wd,
		WriteDomain:      domain,
		TerminalCriteria: *criteria,
		MaxRounds:        *maxRounds,
		WriterEngine:     *engine,
		ReviewerEngine:   *engine,
		EffectGates: WorkflowEffectGates{
			Integration: effectGateHeld,
			Live:        effectGateHeld,
			Cutover:     effectGateHeld,
		},
		Status: workflowStatusDesign,
	}
	if err := normalizeWorkflowRecord(wf); err != nil {
		return err
	}
	if err := auditWorkflowWriteDomains(root, wf); err != nil {
		return err
	}
	if _, err := createHeldIntegrationTask(root, cfg, wf); err != nil {
		return err
	}
	if err := writeWorkflowProgress(root, wf); err != nil {
		return err
	}
	if err := saveWorkflow(root, wf); err != nil {
		return err
	}
	fmt.Printf("已创建 workflow %s mode=%s module=%s integration=%s (held)\n",
		wf.ID, wf.Mode, wf.ModuleID, wf.IntegrationTaskID)
	return nil
}

func cmdWorkflowList(args []string) error {
	fs := flag.NewFlagSet("workflow list", flag.ExitOnError)
	rootFlag := fs.String("root", "", "数据目录")
	asJSON := fs.Bool("json", false, "输出 JSON")
	_ = fs.Parse(args)
	root := resolveRoot(*rootFlag)
	wfs, err := loadWorkflows(root)
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
		fmt.Printf("%s\t%s\t%s\t%s\tround %d/%d\tintegration=%s\n",
			wf.ID, wf.Mode, wf.ModuleID, wf.Status, wf.CurrentRound, wf.MaxRounds, wf.EffectGates.Integration)
	}
	return nil
}

func cmdWorkflowShow(args []string) error {
	fs := flag.NewFlagSet("workflow show", flag.ExitOnError)
	rootFlag := fs.String("root", "", "数据目录")
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		return fmt.Errorf("用法: cardex workflow show <id>")
	}
	root := resolveRoot(*rootFlag)
	wf, err := loadWorkflow(root, fs.Arg(0))
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(wf)
}

func cmdWorkflowAdvance(args []string) error {
	fs := flag.NewFlagSet("workflow advance", flag.ExitOnError)
	rootFlag := fs.String("root", "", "数据目录")
	_ = fs.Parse(args)
	root := resolveRoot(*rootFlag)
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	if fs.NArg() < 1 {
		advanceWorkflows(root, cfg)
		fmt.Println("已推进全部 workflow。")
		return nil
	}
	wf, err := loadWorkflow(root, fs.Arg(0))
	if err != nil {
		return err
	}
	if err := advanceWorkflow(root, cfg, wf); err != nil {
		return err
	}
	fmt.Printf("workflow %s status=%s writer=%s reviewer=%s\n", wf.ID, wf.Status, wf.WriterTaskID, wf.ReviewerTaskID)
	return nil
}

func cmdWorkflowFreeze(args []string) error {
	fs := flag.NewFlagSet("workflow freeze-candidate", flag.ExitOnError)
	rootFlag := fs.String("root", "", "数据目录")
	commit := fs.String("commit", "", "候选 commit")
	tree := fs.String("tree", "", "候选 tree")
	branch := fs.String("branch", "", "分支")
	paths := fs.String("changed-paths", "", "逗号分隔 changed paths")
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		return fmt.Errorf("用法: cardex workflow freeze-candidate <id> -commit C -tree T")
	}
	if strings.TrimSpace(*commit) == "" || strings.TrimSpace(*tree) == "" {
		return fmt.Errorf("-commit 与 -tree 都必须提供")
	}
	root := resolveRoot(*rootFlag)
	wf, err := loadWorkflow(root, fs.Arg(0))
	if err != nil {
		return err
	}
	wf.Candidate = &WorkflowCandidate{
		Commit:       strings.TrimSpace(*commit),
		Tree:         strings.TrimSpace(*tree),
		Branch:       strings.TrimSpace(*branch),
		ChangedPaths: splitComma(*paths),
	}
	if wf.IntegrationTaskID != "" {
		if integ, err := loadTask(root, wf.IntegrationTaskID); err == nil && integ.IntegrationGate != nil {
			integ.IntegrationGate.CandidateCommit = wf.Candidate.Commit
			integ.IntegrationGate.CandidateTree = wf.Candidate.Tree
			_ = saveTask(root, integ)
		}
	}
	if err := writeWorkflowProgress(root, wf); err != nil {
		return err
	}
	if err := saveWorkflow(root, wf); err != nil {
		return err
	}
	fmt.Printf("已冻结候选 %s tree %s\n", wf.Candidate.Commit, wf.Candidate.Tree)
	return nil
}

func cmdWorkflowIngest(args []string) error {
	fs := flag.NewFlagSet("workflow ingest-review", flag.ExitOnError)
	rootFlag := fs.String("root", "", "数据目录")
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		return fmt.Errorf("用法: cardex workflow ingest-review <id>")
	}
	root := resolveRoot(*rootFlag)
	wf, err := loadWorkflow(root, fs.Arg(0))
	if err != nil {
		return err
	}
	if err := ingestWorkflowReview(root, wf); err != nil {
		return err
	}
	reason := ""
	if wf.Review != nil {
		reason = wf.Review.HoldReason
	}
	fmt.Printf("workflow %s status=%s hold=%s\n", wf.ID, wf.Status, orDash(reason))
	return nil
}

func cmdWorkflowTryRelease(args []string) error {
	fs := flag.NewFlagSet("workflow try-release-integration", flag.ExitOnError)
	rootFlag := fs.String("root", "", "数据目录")
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		return fmt.Errorf("用法: cardex workflow try-release-integration <id>")
	}
	root := resolveRoot(*rootFlag)
	wf, err := loadWorkflow(root, fs.Arg(0))
	if err != nil {
		return err
	}
	if err := tryReleaseWorkflowIntegration(root, wf); err != nil {
		return err
	}
	fmt.Printf("workflow %s integration=%s\n", wf.ID, wf.EffectGates.Integration)
	return nil
}

func cmdWorkflowMark(args []string) error {
	fs := flag.NewFlagSet("workflow mark", flag.ExitOnError)
	rootFlag := fs.String("root", "", "数据目录")
	kind := fs.String("kind", "", "external | owner | exhausted")
	summary := fs.String("summary", "", "简要原因")
	_ = fs.Parse(args)
	if fs.NArg() < 1 || *kind == "" {
		return fmt.Errorf("用法: cardex workflow mark <id> -kind external|owner|exhausted -summary ...")
	}
	root := resolveRoot(*rootFlag)
	wf, err := loadWorkflow(root, fs.Arg(0))
	if err != nil {
		return err
	}
	var notifyKind, status string
	switch *kind {
	case "external":
		notifyKind, status = rootNotifyExternal, workflowStatusExternalBlocked
	case "owner":
		notifyKind, status = rootNotifyOwner, workflowStatusOwnerChoice
	case "exhausted":
		notifyKind, status = rootNotifyExhausted, workflowStatusExhausted
	default:
		return fmt.Errorf("未知 mark kind %q", *kind)
	}
	wf.Status = status
	if err := emitRootNotify(root, wf, notifyKind, strings.TrimSpace(*summary)); err != nil {
		return err
	}
	if err := writeWorkflowProgress(root, wf); err != nil {
		return err
	}
	if err := saveWorkflow(root, wf); err != nil {
		return err
	}
	fmt.Printf("workflow %s marked %s（已写 Root 通知）\n", wf.ID, notifyKind)
	return nil
}
