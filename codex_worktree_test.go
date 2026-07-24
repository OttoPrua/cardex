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
	"fmt"
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

// fakeCodexArgvStdinCapture 返回把 argv 逐行写进 argvOut、把 stdin 全文写进 stdinOut 的假 codex(exit 0)。
// 【为什么】fakeCodexArgvCapture 只捕 argv,但 P1-1 修复的关键证据在 stdin(路径映射前导)。
// 单独一个 stdin 捕获脚本让"前导已注入"可测——脚本用 `cat >` 读 stdin,argv 用 printf 逐行落磁盘。
func fakeCodexArgvStdinCapture(t *testing.T, argvOut, stdinOut string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvOut + "\ncat > " + stdinOut + "\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// argvValueAfter 在 argv 逐行捕获里找 flag 的下一行值(如 "-C" 后一行 = workDir)。
// 空表示没找到——测试侧一律 t.Fatalf 阻断,不给静默通过的机会。
func argvValueAfter(argv, flag string) string {
	lines := strings.Split(argv, "\n")
	for i, l := range lines {
		if l == flag && i+1 < len(lines) {
			return lines[i+1]
		}
	}
	return ""
}

// TestInvokeCodexWorktreeWriteArgvAndCleanup 正向覆盖·CG-R3 R1 P1-2:
// 默认 worktree-write 模式经 invokeCodex 的关键接线全套断言,任何一处回归全套测试都会红:
//   ① sandbox 切换:argv 含 --sandbox workspace-write,不含 read-only;
//   ② -C 指向副本(而非原仓);副本路径落在 codexWorkRoot(root) 之下;
//   ③ writable_roots 拼接的是副本 .git 路径(不是原仓 .git);
//   ④ defer cleanup:invokeCodex 返回后副本目录已被删。
//
// 若无此测试,cleanup 回归会被 orphan reaper 的"pid 活即跳过"掩盖(codex_worktree.go:336-338):
// 测试进程 pid 是活的 → reaper 跳过 → 副本残留只在 stat 时才见 → 全套测试仍绿。
func TestInvokeCodexWorktreeWriteArgvAndCleanup(t *testing.T) {
	root := testRoot(t)
	src := mkCodexReviewSrcRepo(t)
	argvCap := filepath.Join(t.TempDir(), "argv.txt")
	stdinCap := filepath.Join(t.TempDir(), "stdin.txt")

	cfg := defaultConfig("")
	cfg.CodexBin = fakeCodexArgvStdinCapture(t, argvCap, stdinCap)
	cfg.StepTimeoutMin = 1
	// 默认 CodexReviewSandbox=worktree-write(defaultConfig 已配),此测试正是要验证默认路径。

	task := &Task{ID: "cg-r3-ww", Type: typeReview, Dir: src}

	if _, _, err := invokeCodex(context.Background(), root, cfg, task, "ping"); err != nil {
		t.Fatalf("fake codex 应 exit 0: %v", err)
	}

	argvRaw, err := os.ReadFile(argvCap)
	if err != nil {
		t.Fatalf("argv 未捕获: %v", err)
	}
	got := string(argvRaw)

	// 断言 ①:--sandbox workspace-write,不含 read-only。
	if !strings.Contains(got, "workspace-write") {
		t.Fatalf("argv 应含 --sandbox workspace-write, got:\n%s", got)
	}
	if strings.Contains(got, "read-only") {
		t.Fatalf("默认模式 argv 不应含 read-only, got:\n%s", got)
	}

	// 断言 ②:-C 指向副本(非原仓);副本必须在 codexWorkRoot(root) 下。
	cPath := argvValueAfter(got, "-C")
	if cPath == "" {
		t.Fatalf("argv 未找到 -C <path>, got:\n%s", got)
	}
	if cPath == src {
		t.Fatalf("-C 目标应为副本,不是原仓 %s", src)
	}
	// 不做 EvalSymlinks:invokeCodex 返回时 defer cleanup 已删掉副本目录,
	// EvalSymlinks(cPath) 失败返回 "" 而 EvalSymlinks(workRoot) 成功返回 /private/var/... 前缀,
	// 二者对不齐(/var vs /private/var 假不匹配)。cPath 与 workRoot 都由我方按同一 root 字符串
	// 拼接而成,一定共享前缀链,直接字符串比对即可。
	workRootRaw := codexWorkRoot(root)
	if !strings.HasPrefix(cPath, workRootRaw+string(filepath.Separator)) {
		t.Fatalf("-C 目标不在副本根 %s 下: %s", workRootRaw, cPath)
	}

	// 断言 ③:writable_roots=[<副本>/.git],不是原仓 .git。
	expectWritableRoots := fmt.Sprintf(`sandbox_workspace_write.writable_roots=["%s"]`, filepath.Join(cPath, ".git"))
	if !strings.Contains(got, expectWritableRoots) {
		t.Fatalf("argv 应含 writable_roots=[<副本>/.git]\n  期望片段: %s\n  实得:\n%s", expectWritableRoots, got)
	}
	forbidWritableRoots := fmt.Sprintf(`sandbox_workspace_write.writable_roots=["%s"]`, filepath.Join(src, ".git"))
	if strings.Contains(got, forbidWritableRoots) {
		t.Fatalf("writable_roots 不应指向原仓 .git\n  禁止片段: %s\n  实得:\n%s", forbidWritableRoots, got)
	}

	// 断言 ④:invokeCodex 返回后,副本目录已被 defer cleanup 删除。
	if _, err := os.Stat(cPath); !os.IsNotExist(err) {
		t.Fatalf("invokeCodex 返回后副本应已删(defer cleanup),但仍存在: err=%v", err)
	}
}

// TestInvokeCodexInjectsCopyPreamble 正向覆盖·CG-R3 R1 P1-1:
// 副本模式下 stdin 前置必含路径映射(原仓路径 + 副本路径 + "复审副本模式"字面量),
// 且必须在用户 prompt 之前;否则 codex 依 prompt(原仓路径)去写,workspace-write 全废。
func TestInvokeCodexInjectsCopyPreamble(t *testing.T) {
	root := testRoot(t)
	src := mkCodexReviewSrcRepo(t)
	argvCap := filepath.Join(t.TempDir(), "argv.txt")
	stdinCap := filepath.Join(t.TempDir(), "stdin.txt")

	cfg := defaultConfig("")
	cfg.CodexBin = fakeCodexArgvStdinCapture(t, argvCap, stdinCap)
	cfg.StepTimeoutMin = 1

	task := &Task{ID: "cg-r3-pre", Type: typeReview, Dir: src}
	if _, _, err := invokeCodex(context.Background(), root, cfg, task, "USER_PROMPT_MARKER"); err != nil {
		t.Fatalf("fake codex 应 exit 0: %v", err)
	}
	// EvalSymlinks 后比对(原仓 t.Dir 传入 invokeCodex 未 EvalSymlinks,但副本经 EvalSymlinks 前
	// 可能是同源;前导直接写 workDir + t.Dir 字面量,故用原字面量断言即可)。
	argvRaw, err := os.ReadFile(argvCap)
	if err != nil {
		t.Fatalf("argv 未捕获: %v", err)
	}
	workDir := argvValueAfter(string(argvRaw), "-C")
	if workDir == "" || workDir == src {
		t.Fatalf("测试前提破坏:workDir 应为副本,实得 %q (src=%s)", workDir, src)
	}

	stdinRaw, err := os.ReadFile(stdinCap)
	if err != nil {
		t.Fatalf("stdin 未捕获: %v", err)
	}
	got := string(stdinRaw)

	// 断言 A:含"复审副本模式"字面量(前导起首,唯一辨识)。
	if !strings.Contains(got, "复审副本模式") {
		t.Fatalf("stdin 应含'复审副本模式'前导, got:\n%s", got)
	}
	// 断言 B:同时含原仓路径 src 与副本路径 workDir。
	if !strings.Contains(got, src) {
		t.Fatalf("stdin 前导应含原仓路径 %s, got:\n%s", src, got)
	}
	if !strings.Contains(got, workDir) {
		t.Fatalf("stdin 前导应含副本路径 %s, got:\n%s", workDir, got)
	}
	// 断言 C:前导在用户 prompt 之前(不是尾巴,否则 codex 已按 prompt 落地才看到映射说明)。
	idxPre := strings.Index(got, "复审副本模式")
	idxUser := strings.Index(got, "USER_PROMPT_MARKER")
	if idxUser < 0 || idxPre < 0 || idxPre >= idxUser {
		t.Fatalf("前导应在用户 prompt 之前, 前导位置=%d USER_PROMPT_MARKER位置=%d\nstdin:\n%s", idxPre, idxUser, got)
	}
}

// TestInvokeCodexNoCopyPreambleInReadonly 反面:readonly 回落模式(workDir==t.Dir)不注入副本前导。
// 若这里注入,codex 会被引导去查一个不存在的"副本路径",反而混淆——回落语义就是"跑在原仓,只读"。
func TestInvokeCodexNoCopyPreambleInReadonly(t *testing.T) {
	root := testRoot(t)
	src := mkCodexReviewSrcRepo(t)
	argvCap := filepath.Join(t.TempDir(), "argv.txt")
	stdinCap := filepath.Join(t.TempDir(), "stdin.txt")

	cfg := defaultConfig("")
	cfg.CodexBin = fakeCodexArgvStdinCapture(t, argvCap, stdinCap)
	cfg.CodexReviewSandbox = codexReviewSandboxReadonly
	cfg.StepTimeoutMin = 1

	task := &Task{ID: "cg-r3-nopre", Type: typeReview, Dir: src}
	if _, _, err := invokeCodex(context.Background(), root, cfg, task, "USER_PROMPT_MARKER"); err != nil {
		t.Fatalf("fake codex 应 exit 0: %v", err)
	}
	stdinRaw, err := os.ReadFile(stdinCap)
	if err != nil {
		t.Fatalf("stdin 未捕获: %v", err)
	}
	got := string(stdinRaw)
	if strings.Contains(got, "复审副本模式") {
		t.Fatalf("readonly 模式 stdin 不应含副本模式前导, got:\n%s", got)
	}
}

// TestRemoteCodexReviewSandbox 单元表:CG-R3 R1 P0-1 修法的可证伪核心——
// 决定远端 codex 非 sequence 卡沙箱的判据必须按"目录确为一次性镜像"收窄,
// 交叉/协调/回退等真实业务仓路径必须回落 read-only(硬保证:原仓字节永不受写污染)。
// 若某天有人把 remoteCodexReviewSandbox 又改回"非 sequence 就 workspace-write",本表全红。
func TestRemoteCodexReviewSandbox(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		task *Task
		want string
	}{
		{
			"镜像目录(review divert 成功) → workspace-write",
			&Config{RemoteMirrorRoot: "D:/Project/PO-lanes"},
			&Task{Type: typeReview, Dir: "D:/Project/PO-lanes/ClaudeGo"},
			"workspace-write",
		},
		{
			"交叉卡远端腿(真实业务仓) → read-only",
			&Config{RemoteMirrorRoot: "D:/Project/PO-lanes"},
			&Task{Type: typeCrossCheck, Dir: "C:/work/otherrepo"},
			"read-only",
		},
		{
			"协调卡远端(真实业务仓) → read-only",
			&Config{RemoteMirrorRoot: "D:/Project/PO-lanes"},
			&Task{Type: typeCoordinate, Dir: "D:/other/somewhere"},
			"read-only",
		},
		{
			"progress-pull 远端(真实业务仓) → read-only",
			&Config{RemoteMirrorRoot: "D:/Project/PO-lanes"},
			&Task{Type: typeProgressPull, Dir: "D:/work/proj"},
			"read-only",
		},
		{
			"review 卡 sync 失败回退到原仓 → read-only",
			&Config{RemoteMirrorRoot: "D:/Project/PO-lanes"},
			&Task{Type: typeReview, Dir: "C:/work/therepo"},
			"read-only",
		},
		{
			"CodexReviewSandbox=readonly 恒 read-only(即便在镜像下也强制回落)",
			&Config{RemoteMirrorRoot: "D:/Project/PO-lanes", CodexReviewSandbox: codexReviewSandboxReadonly},
			&Task{Type: typeReview, Dir: "D:/Project/PO-lanes/ClaudeGo"},
			"read-only",
		},
		{
			"RemoteMirrorRoot 未配 → read-only(无法判定,保守取硬保证)",
			&Config{},
			&Task{Type: typeReview, Dir: "D:/anywhere"},
			"read-only",
		},
		{
			"前缀假匹配防线:'D:/Project/foo-bar/x' 不算 'D:/Project/foo' 的子孙",
			&Config{RemoteMirrorRoot: "D:/Project/foo"},
			&Task{Type: typeReview, Dir: "D:/Project/foo-bar/repo"},
			"read-only",
		},
		{
			"posix + Windows 分隔符归一:反斜杠 root 与正斜杠 dir 应匹配",
			&Config{RemoteMirrorRoot: `D:\Project\PO-lanes`},
			&Task{Type: typeReview, Dir: "D:/Project/PO-lanes/ClaudeGo"},
			"workspace-write",
		},
		{
			"dir == root(等于边缘不算子孙,严格子孙才是镜像) → read-only",
			&Config{RemoteMirrorRoot: "D:/Project/PO-lanes"},
			&Task{Type: typeReview, Dir: "D:/Project/PO-lanes"},
			"read-only",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := remoteCodexReviewSandbox(c.cfg, c.task); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}
