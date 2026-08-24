package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func mustWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func explicitTask(t *testing.T, root, title, dir, id, lineage, component string, paths []string, resources []ResourceClaim) *Task {
	t.Helper()
	tk := newTask(root, testCfg(), typeSequence, title, dir, []string{"p"}, 5)
	if err := applyTaskWriteDomain(tk, id, lineage, component, joinComma(paths), joinResources(resources)); err != nil {
		t.Fatal(err)
	}
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	return tk
}

func joinComma(in []string) string {
	out := ""
	for i, s := range in {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}

func joinResources(in []ResourceClaim) string {
	out := ""
	for i, r := range in {
		if i > 0 {
			out += ","
		}
		out += r.Kind + ":" + r.ID
	}
	return out
}

func TestWriterClaimsAllowDisjointSameRepoAndSerializeOverlap(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "internal", "auth", "token.go"), "package auth\n")
	mustWriteFile(t, filepath.Join(root, "internal", "billing", "bill.go"), "package billing\n")

	a := &Task{ID: "tauth", Dir: root, Type: typeSequence, WriteDomain: &WriteDomain{
		ID: "auth-tokens", Lineage: "auth-tokens-lineage", Component: "auth", Paths: []string{"internal/auth"},
	}}
	b := &Task{ID: "tbill", Dir: root, Type: typeSequence, WriteDomain: &WriteDomain{
		ID: "billing-core", Lineage: "billing-core-lineage", Component: "billing", Paths: []string{"internal/billing"},
	}}
	c := &Task{ID: "tauth2", Dir: root, Type: typeSequence, WriteDomain: &WriteDomain{
		ID: "auth-session", Lineage: "auth-session-lineage", Component: "auth", Paths: []string{"internal/auth/session.go"},
	}}
	if writerConflictsWithActive(b, []*Task{a}) {
		t.Fatal("disjoint explicit claims in the same repo must be concurrently admissible")
	}
	if !writerConflictsWithActive(c, []*Task{a}) {
		t.Fatal("subtree overlap must serialize")
	}

	same := &Task{ID: "tsame", Dir: root, Type: typeSequence, WriteDomain: &WriteDomain{
		ID: "auth-copy", Lineage: "auth-copy-lineage", Component: "auth", Paths: []string{"internal/auth/token.go"},
	}}
	if !writerConflictsWithActive(same, []*Task{a}) {
		t.Fatal("exact path overlap must serialize")
	}

	resA := &Task{ID: "resa", Dir: root, Type: typeSequence, WriteDomain: &WriteDomain{
		ID: "lane-a", Lineage: "lane-a-lineage", Component: "ops",
		Paths:     []string{"internal/auth"},
		Resources: []ResourceClaim{{Kind: resourceDatabase, ID: "auth.primary"}},
	}}
	resB := &Task{ID: "resb", Dir: t.TempDir(), Type: typeSequence, WriteDomain: &WriteDomain{
		ID: "lane-b", Lineage: "lane-b-lineage", Component: "ops",
		Paths:     []string{"internal/billing"},
		Resources: []ResourceClaim{{Kind: resourceDatabase, ID: "auth.primary"}},
	}}
	if !writerConflictsWithActive(resB, []*Task{resA}) {
		t.Fatal("shared closed resources must serialize across roots")
	}
}

func TestWriterClaimsSymlinkAliasAndWorktreeLogicalPath(t *testing.T) {
	realRoot := t.TempDir()
	mustWriteFile(t, filepath.Join(realRoot, "internal", "auth", "token.go"), "package auth\n")
	alias := filepath.Join(t.TempDir(), "repo-alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	a := &Task{ID: "treal", Dir: realRoot, Type: typeSequence, WriteDomain: &WriteDomain{
		ID: "auth-tokens", Lineage: "auth-tokens-lineage", Component: "auth", Paths: []string{"internal/auth/token.go"},
	}}
	b := &Task{ID: "talias", Dir: alias, Type: typeSequence, WriteDomain: &WriteDomain{
		ID: "auth-alias", Lineage: "auth-alias-lineage", Component: "auth", Paths: []string{"internal/auth/token.go"},
	}}
	if !writerConflictsWithActive(b, []*Task{a}) {
		t.Fatal("symlink-alias overlap must serialize")
	}

	main := t.TempDir()
	mustWriteFile(t, filepath.Join(main, ".git", "HEAD"), "ref: refs/heads/main\n")
	mustWriteFile(t, filepath.Join(main, "internal", "auth", "token.go"), "package auth\n")
	wt := t.TempDir()
	mustWriteFile(t, filepath.Join(main, ".git", "worktrees", "lane", "commondir"), "gitdir\n")
	mustWriteFile(t, wt+"/.git", "gitdir: "+filepath.Join(main, ".git", "worktrees", "lane")+"\n")
	mustWriteFile(t, filepath.Join(wt, "internal", "auth", "token.go"), "package auth\n")
	wa := &Task{ID: "wt-a", Dir: main, Type: typeSequence, WriteDomain: &WriteDomain{
		ID: "auth-main", Lineage: "auth-main-lineage", Component: "auth", Paths: []string{"internal/auth"},
	}}
	wb := &Task{ID: "wt-b", Dir: wt, Type: typeSequence, WriteDomain: &WriteDomain{
		ID: "auth-wt", Lineage: "auth-wt-lineage", Component: "auth", Paths: []string{"internal/auth"},
	}}
	if !writerConflictsWithActive(wb, []*Task{wa}) {
		t.Fatal("same logical path in separate worktrees must serialize")
	}
}

func TestLegacyWriterSerializesWithExplicitSameDir(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	legacy := &Task{ID: "legacy", Dir: dir, Type: typeSequence}
	explicit := &Task{ID: "explicit", Dir: dir, Type: typeSequence, WriteDomain: &WriteDomain{
		ID: "auth-tokens", Lineage: "auth-tokens-lineage", Component: "auth", Paths: []string{"internal/auth"},
	}}
	if !writerConflictsWithActive(explicit, []*Task{legacy}) || !writerConflictsWithActive(legacy, []*Task{explicit}) {
		t.Fatal("legacy and explicit writers for the same directory must serialize")
	}
	legacy2 := &Task{ID: "legacy2", Dir: dir, Type: typeSequence}
	if !writerConflictsWithActive(legacy2, []*Task{legacy}) {
		t.Fatal("unrelated legacy same-dir exclusivity must remain")
	}
	unrelated := &Task{ID: "legacy-other", Dir: other, Type: typeSequence}
	if writerConflictsWithActive(unrelated, []*Task{legacy}) {
		t.Fatal("legacy writers in different directories must stay concurrent")
	}
	review := &Task{ID: "review", Dir: dir, Type: typeReview}
	if writerConflictsWithActive(review, []*Task{legacy}) {
		t.Fatal("read-only tasks must not occupy write exclusivity")
	}
}

func linkedGitWorktrees(t *testing.T) (mainDir, worktreeDir string) {
	t.Helper()
	mainDir = t.TempDir()
	mustWriteFile(t, filepath.Join(mainDir, ".git", "HEAD"), "ref: refs/heads/main\n")
	mustWriteFile(t, filepath.Join(mainDir, "internal", "auth", "token.go"), "package auth\n")
	mustWriteFile(t, filepath.Join(mainDir, "internal", "billing", "bill.go"), "package billing\n")
	worktreeDir = t.TempDir()
	mustWriteFile(t, filepath.Join(mainDir, ".git", "worktrees", "lane", "commondir"), "gitdir\n")
	mustWriteFile(t, worktreeDir+"/.git", "gitdir: "+filepath.Join(mainDir, ".git", "worktrees", "lane")+"\n")
	mustWriteFile(t, filepath.Join(worktreeDir, "internal", "auth", "token.go"), "package auth\n")
	mustWriteFile(t, filepath.Join(worktreeDir, "internal", "billing", "bill.go"), "package billing\n")
	return mainDir, worktreeDir
}

func TestLegacyWritersSerializeOnSharedRepoCanonicalAliasAndStayConcurrentWhenUnrelated(t *testing.T) {
	mainDir, wtDir := linkedGitWorktrees(t)
	legacyMain := &Task{ID: "legacy-main", Dir: mainDir, Type: typeSequence}
	legacyWT := &Task{ID: "legacy-wt", Dir: wtDir, Type: typeSequence}
	if !writerConflictsWithActive(legacyWT, []*Task{legacyMain}) || !writerConflictsWithActive(legacyMain, []*Task{legacyWT}) {
		t.Fatal("legacy writers in linked worktrees of the same git common dir must serialize")
	}

	explicitMain := &Task{ID: "explicit-main", Dir: mainDir, Type: typeSequence, WriteDomain: &WriteDomain{
		ID: "auth-tokens", Lineage: "auth-tokens-lineage", Component: "auth", Paths: []string{"internal/auth"},
	}}
	if !writerConflictsWithActive(explicitMain, []*Task{legacyWT}) || !writerConflictsWithActive(legacyWT, []*Task{explicitMain}) {
		t.Fatal("mixed explicit/legacy writers sharing a git common dir must serialize")
	}

	explicitBilling := &Task{ID: "explicit-bill", Dir: wtDir, Type: typeSequence, WriteDomain: &WriteDomain{
		ID: "billing-core", Lineage: "billing-core-lineage", Component: "billing", Paths: []string{"internal/billing"},
	}}
	if writerConflictsWithActive(explicitBilling, []*Task{explicitMain}) {
		t.Fatal("disjoint explicit claims across linked worktrees must stay concurrent")
	}

	realRoot := t.TempDir()
	mustWriteFile(t, filepath.Join(realRoot, "internal", "auth", "token.go"), "package auth\n")
	alias := filepath.Join(t.TempDir(), "repo-alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	legacyReal := &Task{ID: "legacy-real", Dir: realRoot, Type: typeSequence}
	legacyAlias := &Task{ID: "legacy-alias", Dir: alias, Type: typeSequence}
	if !writerConflictsWithActive(legacyAlias, []*Task{legacyReal}) {
		t.Fatal("legacy writers on canonical symlink aliases must serialize")
	}
	explicitAlias := &Task{ID: "explicit-alias", Dir: alias, Type: typeSequence, WriteDomain: &WriteDomain{
		ID: "auth-alias", Lineage: "auth-alias-lineage", Component: "auth", Paths: []string{"internal/auth/token.go"},
	}}
	if !writerConflictsWithActive(explicitAlias, []*Task{legacyReal}) {
		t.Fatal("mixed explicit/legacy canonical aliases must serialize")
	}

	spelling := &Task{ID: "legacy-dot", Dir: realRoot + string(filepath.Separator) + ".", Type: typeSequence}
	if !writerConflictsWithActive(spelling, []*Task{legacyReal}) {
		t.Fatal("legacy writers with equivalent directory spellings must serialize")
	}

	other := t.TempDir()
	unrelated := &Task{ID: "legacy-other-dir", Dir: other, Type: typeSequence}
	if writerConflictsWithActive(unrelated, []*Task{legacyReal}) || writerConflictsWithActive(unrelated, []*Task{legacyMain}) {
		t.Fatal("legacy writers in unrelated directories must stay concurrent")
	}

	cloneA := t.TempDir()
	cloneB := t.TempDir()
	mustWriteFile(t, filepath.Join(cloneA, ".git", "HEAD"), "ref: refs/heads/main\n")
	mustWriteFile(t, filepath.Join(cloneB, ".git", "HEAD"), "ref: refs/heads/main\n")
	legacyCloneA := &Task{ID: "legacy-clone-a", Dir: cloneA, Type: typeSequence}
	legacyCloneB := &Task{ID: "legacy-clone-b", Dir: cloneB, Type: typeSequence}
	if writerConflictsWithActive(legacyCloneB, []*Task{legacyCloneA}) {
		t.Fatal("legacy writers in independent repositories must stay concurrent")
	}
}

func TestLiveDAGFailClosedReadiness(t *testing.T) {
	cardexRoot := testRoot(t)
	dir := t.TempDir()
	done := newTask(cardexRoot, testCfg(), typeSequence, "done dep", dir, []string{"p"}, 5)
	done.Status = statusDone
	if err := saveTask(cardexRoot, done); err != nil {
		t.Fatal(err)
	}

	ok := newTask(cardexRoot, testCfg(), typeSequence, "ready child", dir, []string{"p"}, 5)
	ok.DependsOn = []string{done.ID}
	if err := saveTask(cardexRoot, ok); err != nil {
		t.Fatal(err)
	}
	independent := newTask(cardexRoot, testCfg(), typeSequence, "independent", dir, []string{"p"}, 5)
	if err := saveTask(cardexRoot, independent); err != nil {
		t.Fatal(err)
	}
	tasks, err := loadTasks(cardexRoot)
	if err != nil {
		t.Fatal(err)
	}
	ready := liveDAGReadyIDs(cardexRoot, tasks)
	if !ready[ok.ID] || !ready[independent.ID] {
		t.Fatalf("healthy dag ready=%v", ready)
	}

	held := newTask(cardexRoot, testCfg(), typeSequence, "held dep", dir, []string{"p"}, 5)
	if err := saveTask(cardexRoot, held); err != nil {
		t.Fatal(err)
	}
	if err := cmdSetStatus([]string{"-root", cardexRoot, held.ID}, "hold"); err != nil {
		t.Fatal(err)
	}
	blocked := newTask(cardexRoot, testCfg(), typeSequence, "blocked on held", dir, []string{"p"}, 5)
	blocked.DependsOn = []string{held.ID}
	if err := saveTask(cardexRoot, blocked); err != nil {
		t.Fatal(err)
	}
	tasks, _ = loadTasks(cardexRoot)
	ready = liveDAGReadyIDs(cardexRoot, tasks)
	if ready[blocked.ID] {
		t.Fatal("held dependency must not satisfy readiness")
	}

	missing := newTask(cardexRoot, testCfg(), typeSequence, "missing edge", dir, []string{"p"}, 5)
	missing.DependsOn = []string{"ghost-task"}
	if err := saveTask(cardexRoot, missing); err != nil {
		t.Fatal(err)
	}
	cycleA := newTask(cardexRoot, testCfg(), typeSequence, "cycle a", dir, []string{"p"}, 5)
	cycleB := newTask(cardexRoot, testCfg(), typeSequence, "cycle b", dir, []string{"p"}, 5)
	cycleA.DependsOn = []string{cycleB.ID}
	cycleB.DependsOn = []string{cycleA.ID}
	if err := saveTask(cardexRoot, cycleA); err != nil {
		t.Fatal(err)
	}
	if err := saveTask(cardexRoot, cycleB); err != nil {
		t.Fatal(err)
	}
	malformed := newTask(cardexRoot, testCfg(), typeSequence, "malformed", dir, []string{"p"}, 5)
	malformed.DependsOn = []string{"Not_Valid"}
	if err := saveTask(cardexRoot, malformed); err != nil {
		t.Fatal(err)
	}
	dup := newTask(cardexRoot, testCfg(), typeSequence, "dup", dir, []string{"p"}, 5)
	dup.DependsOn = []string{"solo-dep", "solo-dep"}
	if err := saveTask(cardexRoot, dup); err != nil {
		t.Fatal(err)
	}
	unknown := newTask(cardexRoot, testCfg(), typeSequence, "unknown domain", dir, []string{"p"}, 5)
	unknown.DependsOn = []string{"ghost-domain-dep"}
	unknown.WriteDomain = &WriteDomain{ID: "ghost-domain", Lineage: "ghost-domain-lineage", Component: "ghost", Paths: []string{"../escape"}}
	if err := saveTask(cardexRoot, unknown); err != nil {
		t.Fatal(err)
	}
	failed := newTask(cardexRoot, testCfg(), typeSequence, "failed dep", dir, []string{"p"}, 5)
	failed.Status = statusFailed
	if err := saveTask(cardexRoot, failed); err != nil {
		t.Fatal(err)
	}
	onFailed := newTask(cardexRoot, testCfg(), typeSequence, "on failed", dir, []string{"p"}, 5)
	onFailed.DependsOn = []string{failed.ID}
	if err := saveTask(cardexRoot, onFailed); err != nil {
		t.Fatal(err)
	}

	tasks, _ = loadTasks(cardexRoot)
	ready = liveDAGReadyIDs(cardexRoot, tasks)
	for _, id := range []string{missing.ID, cycleA.ID, cycleB.ID, malformed.ID, dup.ID, unknown.ID, onFailed.ID, blocked.ID} {
		if ready[id] {
			t.Fatalf("%s must not be ready: %v", id, ready)
		}
	}
	if !ready[independent.ID] {
		t.Fatal("unrelated task must remain independently ready")
	}
	if !taskDurablyDone(cardexRoot, done) {
		t.Fatal("done fixture must be durably done")
	}
}

func TestApplyTaskWriteDomainAndDependsOn(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "internal", "auth", "token.go"), "package auth\n")
	tk := &Task{ID: "tapply", Dir: dir}
	if err := applyTaskWriteDomain(tk, "auth-tokens", "auth-tokens-lineage", "auth", "internal/auth", "database:auth.primary"); err != nil {
		t.Fatal(err)
	}
	if tk.WriteDomain == nil || tk.WriteDomain.Paths[0] != "internal/auth" {
		t.Fatalf("persisted domain: %+v", tk.WriteDomain)
	}
	if err := applyTaskDependsOn(tk, "tdone-one,tdone-two"); err != nil {
		t.Fatal(err)
	}
	if len(tk.DependsOn) != 2 {
		t.Fatalf("depends: %v", tk.DependsOn)
	}
	if err := applyTaskDependsOn(tk, "Bad_ID"); !errors.Is(err, errDAGMalformedID) {
		t.Fatalf("malformed depends: %v", err)
	}
	if err := applyTaskDependsOn(tk, "tdone-one,tdone-one"); !errors.Is(err, errDAGDuplicateNode) {
		t.Fatalf("duplicate depends: %v", err)
	}
}

func TestGitIdentityFromNestedTaskDirAndUnreadableMetadata(t *testing.T) {
	repo := t.TempDir()
	mustWriteFile(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n")
	mustWriteFile(t, filepath.Join(repo, "internal", "auth", "token.go"), "package auth\n")
	nested := filepath.Join(repo, "internal", "auth")
	legacyNested := &Task{ID: "legacy-nested", Dir: nested, Type: typeSequence}
	legacyRoot := &Task{ID: "legacy-root", Dir: repo, Type: typeSequence}
	if !writerConflictsWithActive(legacyNested, []*Task{legacyRoot}) || !writerConflictsWithActive(legacyRoot, []*Task{legacyNested}) {
		t.Fatal("nested Task.Dir must resolve to the same git identity as the worktree root")
	}
	top, common, unc := resolveGitIdentity(nested)
	if unc || top == "" || common == "" {
		t.Fatalf("nested git identity uncertain top=%q common=%q unc=%v", top, common, unc)
	}
	if taskRepoRoot(legacyNested) != top {
		t.Fatalf("taskRepoRoot nested=%q want %q", taskRepoRoot(legacyNested), top)
	}

	broken := t.TempDir()
	gitFile := filepath.Join(broken, ".git")
	mustWriteFile(t, gitFile, "gitdir: /nope\n")
	if err := os.Chmod(gitFile, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(gitFile, 0o644) })
	bad := &Task{ID: "legacy-unreadable", Dir: broken, Type: typeSequence}
	claim := writerClaimForTask(bad)
	if claim.valid {
		t.Fatal("unreadable git metadata must fail closed")
	}
	if !writerConflictsWithActive(bad, nil) {
		t.Fatal("uncertain git identity must block dispatch")
	}
}

func TestReconstructLiveWriterClaimsFromRunningStatus(t *testing.T) {
	cardexRoot := testRoot(t)
	ws := t.TempDir()
	mustWriteFile(t, filepath.Join(ws, "internal", "auth", "token.go"), "package auth\n")
	live := explicitTask(t, cardexRoot, "live writer", ws, "auth-tokens", "auth-tokens-lineage", "auth", []string{"internal/auth"}, nil)
	live.Status = statusRunning
	if err := saveTask(cardexRoot, live); err != nil {
		t.Fatal(err)
	}
	other := &Task{ID: "other-auth", Dir: ws, Type: typeSequence, WriteDomain: &WriteDomain{
		ID: "auth-other", Lineage: "auth-other-lineage", Component: "auth", Paths: []string{"internal/auth"},
	}}
	got := reconstructLiveWriterClaims(cardexRoot)
	if !writerConflictsWithActive(other, got) {
		t.Fatal("restart dispatch must serialize against reconstructed running claims")
	}
	if writerConflictsWithActive(other, nil) && !writerConflictsWithActive(other, got) {
		t.Fatal("conflict must come from reconstructed claims, not the candidate alone")
	}
}
