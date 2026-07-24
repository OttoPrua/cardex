package main

// CG-R3(承 BD-36 工具链③终裁 b / BD-39 附记 2026-07-24):codex 复审可写沙箱回归。
//
// 【本文件覆盖的四道闸门】
//   ① 反例注入①:副本内写文件 → 断言原仓无此文件(硬语义:原仓永不受写污染)。
//   ② 反例注入②:CodexReviewSandbox="readonly" → codex argv 含 --sandbox read-only,
//      且不建副本(回落旧行为)。
//   ③ 崩溃注入:副本+marker 建齐后,pid 已死透 且 taskID 不在 activeIDs →
//      cleanupCodexReviewOrphans 移除副本 + 事件账本落 codex_review_orphan_cleanup。
//   ④ 正向覆盖:副本吃 dirty tracked + untracked + 尊重 .gitignore(与 CG-R2 sync 面对齐)。
//
// 【为什么用假 codex 而非跑真 CLI】测试环境不必装 codex;fake codex 用 argv-capture 脚本
// 承接 --sandbox/-C/-m 等旗标,让路径穿线可测(TestInvokeCodexThreadsResolvedModel 已用同法)。

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mkCodexReviewSrcRepo 造一个可当"原仓"用的 git 工作树,含已提交 + dirty + untracked + gitignored 四类面。
// 复用 CG-R2 mkSyncSourceRepo 的思路但独立成型,避免测试文件互相依赖(mkSyncSourceRepo 是 test-only)。
func mkCodexReviewSrcRepo(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	run := func(name string, args ...string) {
		cmd := exec.Command(name, args...)
		cmd.Dir = src
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	run("git", "init", "-q", "-b", "main", ".")
	run("git", "config", "user.email", "cg-r3@example.com")
	run("git", "config", "user.name", "cg-r3")
	if err := os.WriteFile(filepath.Join(src, "committed.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".gitignore"), []byte("ignored/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", ".")
	run("git", "commit", "-q", "-m", "init")
	// dirty tracked:改 committed.txt。
	if err := os.WriteFile(filepath.Join(src, "committed.txt"), []byte("v1\nDIRTY\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// untracked:untr.txt。
	if err := os.WriteFile(filepath.Join(src, "untr.txt"), []byte("fresh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// gitignored:ignored/hidden.bin(不应落到副本)。
	if err := os.MkdirAll(filepath.Join(src, "ignored"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "ignored", "hidden.bin"), []byte("skip me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return src
}

// snapshotSrcSurface 采集原仓的"字节可比对面"（tracked+untracked 相对路径 → 内容,
// 排除 .git;含 dirty tracked 的当前内容 + 真 untracked;.gitignore 也在其中）。
// 用于反例注入①:codex 副本收工后原仓面必须与执行前逐字节相同。
func snapshotSrcSurface(t *testing.T, src string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		// 跳过 .git(内部索引变动不算业务面变动;git status 已在下方独立断言)。
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		snap[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("walk src: %v", err)
	}
	return snap
}

// TestCodexReviewCopyIsolatesWrites 反例注入①:副本内写新文件 → 原仓面字节不变。
// 也顺带覆盖正向面(dirty/untracked/gitignored):副本内应有 dirty 修改与 untracked,不应有 ignored。
func TestCodexReviewCopyIsolatesWrites(t *testing.T) {
	root := testRoot(t)
	src := mkCodexReviewSrcRepo(t)
	before := snapshotSrcSurface(t, src)

	cfg := &Config{CodexReviewSandbox: codexReviewSandboxWorktreeWrite}
	task := &Task{ID: "cg-r3-iso", Type: typeReview, Dir: src}

	copyDir, cleanup, err := prepareCodexReviewWorkspace(root, cfg, task)
	if err != nil {
		t.Fatalf("prepare copy: %v", err)
	}
	defer cleanup()
	if copyDir == src {
		t.Fatal("副本路径必须不同于原仓")
	}
	// 副本必须在原仓目录树之外(原仓保护硬语义)。
	if pathContains(src, copyDir) || pathContains(copyDir, src) {
		t.Fatalf("副本 %s 与原仓 %s 存在包含关系,违反原仓保护语义", copyDir, src)
	}

	// 正向面:dirty 已落到副本 committed.txt。
	if data, err := os.ReadFile(filepath.Join(copyDir, "committed.txt")); err != nil {
		t.Fatalf("副本读 committed.txt: %v", err)
	} else if string(data) != "v1\nDIRTY\n" {
		t.Fatalf("副本 committed.txt 应含 dirty 修改, got %q", string(data))
	}
	// 正向面:untr.txt 已落到副本。
	if data, err := os.ReadFile(filepath.Join(copyDir, "untr.txt")); err != nil {
		t.Fatalf("副本读 untr.txt: %v", err)
	} else if string(data) != "fresh\n" {
		t.Fatalf("副本 untr.txt 内容错, got %q", string(data))
	}
	// 反面:.gitignore 排除的文件不应落到副本(尊重 .gitignore)。
	if _, err := os.Stat(filepath.Join(copyDir, "ignored", "hidden.bin")); !os.IsNotExist(err) {
		t.Fatalf("副本不应含 gitignored 文件, stat err=%v", err)
	}

	// 反例注入①:副本内造一个新文件 + 改一个已提交文件。
	if err := os.WriteFile(filepath.Join(copyDir, "new-by-codex.txt"), []byte("codex wrote this\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(copyDir, "committed.txt"), []byte("codex overwrote\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 断言 A:副本新增文件不得出现在原仓。
	if _, err := os.Stat(filepath.Join(src, "new-by-codex.txt")); !os.IsNotExist(err) {
		t.Fatalf("原仓被污染:new-by-codex.txt 竟然出现在原仓, stat err=%v", err)
	}
	// 断言 B:原仓 committed.txt 应仍是执行前的 dirty 内容,不含"codex overwrote"。
	if data, err := os.ReadFile(filepath.Join(src, "committed.txt")); err != nil {
		t.Fatalf("原仓读 committed.txt: %v", err)
	} else if string(data) != "v1\nDIRTY\n" {
		t.Fatalf("原仓 committed.txt 被副本写覆盖, got %q", string(data))
	}
	// 断言 C:整个业务面字节相同(排除 .git)。
	after := snapshotSrcSurface(t, src)
	if len(before) != len(after) {
		t.Fatalf("原仓文件数变化: before=%d after=%d", len(before), len(after))
	}
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("原仓 %s 字节被改动:\nbefore=%q\nafter =%q", k, v, after[k])
		}
	}
}

// TestCodexReviewSandboxRollbackReadonly 反例注入②:cfg.CodexReviewSandbox="readonly" →
// codex argv 应含 "--sandbox read-only" 且不建副本(workRoot 下无残留目录)。
//
// 【为什么直接测 argv 而非行为】codex 沙箱语义是 codex CLI 内部约束,单测无法真起沙箱;
// 但"argv 里带 read-only 且未走副本路径"是回落的可观测证据面——组合等价于"退回旧行为"。
func TestCodexReviewSandboxRollbackReadonly(t *testing.T) {
	root := testRoot(t)
	src := mkCodexReviewSrcRepo(t)
	capture := filepath.Join(t.TempDir(), "argv.txt")

	cfg := defaultConfig("")
	cfg.CodexBin = fakeCodexArgvCapture(t, capture)
	cfg.CodexReviewSandbox = codexReviewSandboxReadonly
	cfg.StepTimeoutMin = 1

	task := &Task{ID: "cg-r3-ro", Type: typeReview, Dir: src}

	// codexReviewNeedsWorktree 必须返回 false(判据:CodexReviewSandbox 归一后 != worktree-write)。
	if codexReviewNeedsWorktree(cfg, task) {
		t.Fatal("readonly 模式下不应决定建副本")
	}

	// 走 invokeCodex 让 argv 被 fake 脚本捕获。fake codex exit 0,err 应为 nil。
	if _, _, err := invokeCodex(context.Background(), root, cfg, task, "ping"); err != nil {
		t.Fatalf("fake codex 应 exit 0: %v", err)
	}
	argv, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("未捕获 argv: %v", err)
	}
	got := string(argv)
	if !strings.Contains(got, "read-only") {
		t.Fatalf("argv 应含 --sandbox read-only, got:\n%s", got)
	}
	if strings.Contains(got, "workspace-write") {
		t.Fatalf("回落模式 argv 不应含 workspace-write, got:\n%s", got)
	}
	// 副本目录不应存在(要么没建,要么建了立刻清)。回落路径根本不走 prepareCodexReviewWorkspace,故 workRoot 应为空。
	entries, err := os.ReadDir(codexWorkRoot(root))
	if err == nil && len(entries) != 0 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("回落模式不应产生副本目录, got: %v", names)
	}
}

// TestCleanupCodexReviewOrphansRemovesCrashed 崩溃注入:副本+marker 建齐后,pid 假设已死透
// 且 taskID 不在 activeIDs → cleanupCodexReviewOrphans 应删除副本并落事件。
func TestCleanupCodexReviewOrphansRemovesCrashed(t *testing.T) {
	root := testRoot(t)
	src := mkCodexReviewSrcRepo(t)
	cfg := &Config{CodexReviewSandbox: codexReviewSandboxWorktreeWrite}
	task := &Task{ID: "cg-r3-crash", Type: typeReview, Dir: src}

	copyDir, cleanup, err := prepareCodexReviewWorkspace(root, cfg, task)
	if err != nil {
		t.Fatalf("prepare copy: %v", err)
	}
	// 【故意不 defer cleanup】测的正是"崩溃残留",要靠 cleanupCodexReviewOrphans 兜底,
	// 若这里 defer 反而遮蔽 orphan 判据。
	_ = cleanup

	// 覆写 marker:pid 换成一个"绝无可能存活"的 pid(例如 int32 上限附近),模拟崩溃。
	// 用 os.Getpid()+999999 有非零撞现役进程概率;直接用 2^31-1 更硬。
	marker := codexWorkMarker{
		TaskID:    task.ID,
		PID:       2147483646, // int32 max-1,基本不可能是活进程
		Src:       src,
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	if err := writeCodexWorkMarker(copyDir, marker); err != nil {
		t.Fatalf("rewrite marker: %v", err)
	}

	// 前置断言:副本此刻应存在。
	if _, err := os.Stat(copyDir); err != nil {
		t.Fatalf("清理前副本应存在: %v", err)
	}

	// 调对账:activeIDs 空 → taskID 不在里面 + pid 已"死透" → 清。
	cleanupCodexReviewOrphans(root, map[string]bool{})

	// 后置断言 A:副本目录已删。
	if _, err := os.Stat(copyDir); !os.IsNotExist(err) {
		t.Fatalf("清理后副本应消失, stat err=%v", err)
	}
	// 后置断言 B:事件账本落了 codex_review_orphan_cleanup。
	events, _, err := readEvents(eventsPath(root, task.ID))
	if err != nil {
		t.Fatalf("读事件账本: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev.Type == evStalled && ev.Actor == "runner:codex_review_cleanup" {
			if reason, _ := ev.Detail["reason"].(string); reason == "codex_review_orphan_cleanup" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatalf("未找到 codex_review_orphan_cleanup 事件, events=%+v", events)
	}
}

// TestCleanupCodexReviewOrphansSkipsActive 反面:taskID 在 activeIDs 里就不动副本
// (活任务的执行数据不能被对账误清)。
func TestCleanupCodexReviewOrphansSkipsActive(t *testing.T) {
	root := testRoot(t)
	src := mkCodexReviewSrcRepo(t)
	cfg := &Config{CodexReviewSandbox: codexReviewSandboxWorktreeWrite}
	task := &Task{ID: "cg-r3-active", Type: typeReview, Dir: src}

	copyDir, cleanup, err := prepareCodexReviewWorkspace(root, cfg, task)
	if err != nil {
		t.Fatalf("prepare copy: %v", err)
	}
	defer cleanup()

	// 覆写 marker 让 pid 也"死透",单靠 pid 死不够——taskID 在 activeIDs 就要跳过。
	if err := writeCodexWorkMarker(copyDir, codexWorkMarker{
		TaskID: task.ID, PID: 2147483646, Src: src, CreatedAt: time.Now().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	cleanupCodexReviewOrphans(root, map[string]bool{task.ID: true})

	if _, err := os.Stat(copyDir); err != nil {
		t.Fatalf("活任务的副本不应被清: %v", err)
	}
}
