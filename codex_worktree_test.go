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
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

	copyDir, cleanup, err := prepareCodexReviewWorkspace(context.Background(), root, cfg, task)
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
	if codexReviewNeedsWorktree(context.Background(), cfg, task) {
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

	copyDir, cleanup, err := prepareCodexReviewWorkspace(context.Background(), root, cfg, task)
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

	copyDir, cleanup, err := prepareCodexReviewWorkspace(context.Background(), root, cfg, task)
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
//
//	① sandbox 切换:argv 含 --sandbox workspace-write,不含 read-only;
//	② -C 指向副本(而非原仓);副本路径落在 codexWorkRoot(root) 之下;
//	③ writable_roots 拼接的是副本 .git 路径(不是原仓 .git);
//	④ defer cleanup:invokeCodex 返回后副本目录已被删。
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
			"Windows 主机显式禁用 OS sandbox + 严格镜像目录 → danger-full-access",
			&Config{
				RemoteMirrorRoot: "D:/Project/PO-lanes",
				RemoteHosts: map[string]RemoteHostConfig{
					"qmthost": {Sandbox: "danger-full-access"},
				},
			},
			&Task{Type: typeReview, Dir: "D:/Project/PO-lanes/ClaudeGo", RemoteHost: "qmthost"},
			"danger-full-access",
		},
		{
			"Windows 主机 danger-full-access 不得放宽镜像根外真实仓",
			&Config{
				RemoteMirrorRoot: "D:/Project/PO-lanes",
				RemoteHosts: map[string]RemoteHostConfig{
					"qmthost": {Sandbox: "danger-full-access"},
				},
			},
			&Task{Type: typeReview, Dir: "D:/Project/production/ClaudeGo", RemoteHost: "qmthost"},
			"read-only",
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
		{
			// CG-R3 R2 P1-1:反例①—— ".." 词法逃逸:纯前缀比对下
			// "D:/Project/PO-lanes/../OtherRepo" 字面以 "D:/Project/PO-lanes/" 起头,
			// 会被误判为镜像→真实业务仓拿到 workspace-write。path.Clean 后应展开为
			// "D:/Project/OtherRepo",不再是根子孙。
			"'..' 逃逸(词法归约后跳出根) → read-only",
			&Config{RemoteMirrorRoot: "D:/Project/PO-lanes"},
			&Task{Type: typeReview, Dir: "D:/Project/PO-lanes/../OtherRepo"},
			"read-only",
		},
		{
			// CG-R3 R2 P1-1:反例②—— "/." 边缘桩:纯前缀比对下
			// "D:/Project/PO-lanes/." 字面不以 "D:/Project/PO-lanes/" 起头,却与根等价,
			// 应视为"等于边缘"归 read-only(与 dir==root 同义)。
			"'/.' 边缘等价桩(词法归约后等于根) → read-only",
			&Config{RemoteMirrorRoot: "D:/Project/PO-lanes"},
			&Task{Type: typeReview, Dir: "D:/Project/PO-lanes/."},
			"read-only",
		},
		{
			// CG-R3 R2 P1-1:反例③—— 混合 ".." 与 "." 后仍落在子孙,应算镜像。
			// 保证 Clean 归约不误伤合法子孙(正向不越界)。
			"混合 .. / . 归约后仍是严格子孙 → workspace-write",
			&Config{RemoteMirrorRoot: "D:/Project/PO-lanes"},
			&Task{Type: typeReview, Dir: "D:/Project/PO-lanes/./sub/../ClaudeGo"},
			"workspace-write",
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

// ---- CG-R3b 修 1:codex_review_sandbox 未知值必须 fail-closed(回落最小权限) ----

// TestResolvedCodexReviewSandboxUnknownFailsClosed 是修 1 的可证伪核心。
//
// 【修的病】旧实现 switch 只认 "readonly",default 把**空值与未知值一并**回落 worktree-write:
// 委托人本意写 "readonly"(把 codex 关进只读沙箱),把小写 l 打成大写 I 写出 "readonIy",配置就
// 静默生效为"clone 副本 + workspace-write"的更宽权限,全程零提示——收紧意图被拼写事故反向放大。
// 这是安全向 fail-open:权限开关解析不了时倒向宽松侧。
//
// 【杀的突变】把 config.go 的 default 分支改回 `return codexReviewSandboxWorktreeWrite`
// → 下表 4 条未知值用例全红。把 `cfg.CodexReviewSandbox == ""` 的早返回删掉(让空值也压 readonly)
// → "未设置"两条红(那是另一种事故:BD-39 终裁的默认策略被整个翻掉)。
func TestResolvedCodexReviewSandboxUnknownFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		want string
	}{
		{"cfg==nil(无配置) → 默认 worktree-write", nil, codexReviewSandboxWorktreeWrite},
		{"空串(配置里没写这一项) → 默认 worktree-write", &Config{}, codexReviewSandboxWorktreeWrite},
		{"显式 worktree-write → 原样", &Config{CodexReviewSandbox: codexReviewSandboxWorktreeWrite}, codexReviewSandboxWorktreeWrite},
		{"显式 readonly → 原样", &Config{CodexReviewSandbox: codexReviewSandboxReadonly}, codexReviewSandboxReadonly},
		// 以下四条即本修的红线:任何一条落到 worktree-write 都是 fail-open 复活。
		{"拼错:readonIy(大写 I 冒充小写 l) → readonly", &Config{CodexReviewSandbox: "readonIy"}, codexReviewSandboxReadonly},
		{"拼错:read-only(混淆 codex 的 --sandbox 值) → readonly", &Config{CodexReviewSandbox: "read-only"}, codexReviewSandboxReadonly},
		{"拼错:worktree_write(下划线) → readonly", &Config{CodexReviewSandbox: "worktree_write"}, codexReviewSandboxReadonly},
		{"整个写错:workspace-write → readonly", &Config{CodexReviewSandbox: "workspace-write"}, codexReviewSandboxReadonly},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolvedCodexReviewSandbox(c.cfg); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestUnknownCodexReviewSandboxDoesNotReachWorktreeWrite 类闭合:归一函数改对了不算完,
// 两个下游消费点必须真的因此收紧——否则修的只是一个没人看的返回值。
//   - 本机径 codexReviewNeedsWorktree:拼错值不得决定建可写副本;
//   - 远端径 remoteCodexReviewSandbox:拼错值 + 目录恰在镜像根下(旧实现下最宽的格子)
//     不得给出 workspace-write。
//
// 【杀的突变】把 config.go 的 default 分支改回 worktree-write → 两条断言同时红。
func TestUnknownCodexReviewSandboxDoesNotReachWorktreeWrite(t *testing.T) {
	src := mkCodexReviewSrcRepo(t) // 真 git 工作树:排除"因为不是 git 仓库才 false"的假通过
	bad := &Config{CodexReviewSandbox: "readonIy", RemoteMirrorRoot: "D:/Project/PO-lanes"}

	if codexReviewNeedsWorktree(context.Background(), bad, &Task{ID: "cg-r3b-typo", Type: typeReview, Dir: src}) {
		t.Fatal("拼错的 codex_review_sandbox 不得让本机径建可写副本(必须回落最小权限 readonly)")
	}
	// 前提守卫:同一目录在合法 worktree-write 下确实会建副本——否则上面的 false 可能来自别的原因,
	// 断言就成了恒真摆设。
	ok := &Config{CodexReviewSandbox: codexReviewSandboxWorktreeWrite}
	if !codexReviewNeedsWorktree(context.Background(), ok, &Task{ID: "cg-r3b-ok", Type: typeReview, Dir: src}) {
		t.Fatal("前提不成立:合法 worktree-write + git 工作树本应决定建副本,断言无法证伪")
	}

	mirrorTask := &Task{Type: typeReview, Dir: "D:/Project/PO-lanes/ClaudeGo"}
	if got := remoteCodexReviewSandbox(bad, mirrorTask); got != "read-only" {
		t.Fatalf("拼错值 + 镜像目录(旧实现最宽格子)应回落 read-only, got %q", got)
	}
}

// TestUnknownCodexReviewSandboxDisclosedOnce 断言"静默"这一半也被修掉:未知值要披露,
// 但每个不同的值只披露一次——resolvedCodexReviewSandbox 每次 invoke/每轮 tick 都被调,
// 不去重会把 launchd 日志刷成同一行噪声,反而淹没这条本该显眼的权限告警。
// 【杀的突变】删掉 warnUnknownCodexReviewSandbox 调用 → 首条断言红;删掉 LoadOrStore 去重 → 末条红。
func TestUnknownCodexReviewSandboxDisclosedOnce(t *testing.T) {
	origW := codexSandboxWarnW
	defer func() {
		codexSandboxWarnW = origW
		codexSandboxWarned.Delete("readonIy")
		codexSandboxWarned.Delete("worktree_write")
	}()
	// 同进程内别的用例可能已披露过同一值,先清掉记忆,让本用例从零起算。
	codexSandboxWarned.Delete("readonIy")
	codexSandboxWarned.Delete("worktree_write")

	var buf bytes.Buffer
	codexSandboxWarnW = &buf

	for i := 0; i < 5; i++ {
		resolvedCodexReviewSandbox(&Config{CodexReviewSandbox: "readonIy"})
	}
	if n := strings.Count(buf.String(), "readonIy"); n != 1 {
		t.Fatalf("同一未知值应恰好披露一次, got %d 次:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), codexReviewSandboxReadonly) {
		t.Fatalf("披露文案须点明回落到的策略(%q), got:\n%s", codexReviewSandboxReadonly, buf.String())
	}
	// 合法值与空值绝不披露(否则告警本身成噪声)。
	buf.Reset()
	resolvedCodexReviewSandbox(&Config{})
	resolvedCodexReviewSandbox(&Config{CodexReviewSandbox: codexReviewSandboxReadonly})
	resolvedCodexReviewSandbox(&Config{CodexReviewSandbox: codexReviewSandboxWorktreeWrite})
	if buf.Len() != 0 {
		t.Fatalf("合法值/空值不得触发披露, got:\n%s", buf.String())
	}
	// 另一个不同的未知值仍应各自披露一次(去重是按值,不是全局一次)。
	buf.Reset()
	resolvedCodexReviewSandbox(&Config{CodexReviewSandbox: "worktree_write"})
	resolvedCodexReviewSandbox(&Config{CodexReviewSandbox: "worktree_write"})
	if n := strings.Count(buf.String(), "worktree_write"); n != 1 {
		t.Fatalf("另一未知值应独立披露一次, got %d 次:\n%s", n, buf.String())
	}
}

// ---- CG-R3b 修 2:建副本阶段必须受超时/击杀约束 ----

// fakeGitHangingOn 造一个假 git 并把它塞到 PATH 最前:遇到 hangOn 子命令永不退出,其余一律 exit 0。
// hangOn="clone" → rev-parse 探测正常通过、卡在建副本;hangOn="rev-parse" → 卡在前置探测本身。
// 【为什么按参数扫描而非 $1】真实调用形如 `git -c core.quotepath=false -C <dir> diff ...`,
// 子命令并不在 $1;逐参数扫才能精准只吊住目标子命令,其余步骤保持可用。
// 【为什么 exec sleep 而非 sleep】exec 让 sleep 顶替 sh 的 pid,进程组击杀直接命中,
// 不给"sh 死了但 sleep 变孤儿继续跑"留缝。
func fakeGitHangingOn(t *testing.T, hangOn string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nfor a in \"$@\"; do\n  case \"$a\" in\n    " + hangOn + ") exec sleep 600 ;;\n  esac\ndone\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestCodexReviewPrepareKilledOnHangingGit 是修 2 的回红反例(mock git 永不退出)。
//
// 【修的病】建副本的四条 git 子进程原先一律 exec.Command(不带 ctx),且整段跑在 invokeCodex 的
// context.WithTimeout(StepTimeoutMin) **之前**——大仓 clone 卡死(NFS 停顿/等凭据/锁竞争)不受任何
// 超时约束:巡逻看到进程组活着不触发,step 超时压根还没起算,整条泳道被一张卡无声堵死到天荒地老。
//
// 【断言的四件事】① 建副本在子预算内被击杀(不是挂死);② 回落 read-only 后复审照跑(降级不中断);
// ③ 事件账本落 codex_review_prepare_timeout 留痕(不只有一行随日志轮转消失的 stderr);
// ④ 副本残留被清干净。
//
// 【杀的突变】把 codex_worktree.go 的 runCopyGit 换回 exec.Command(去掉 ctx),或把 runner.go 的
// WithTimeout 挪回 prepareCodexReviewWorkspace 之后 → clone 不再被击杀 → 本测试卡在 select 超时红。
func TestCodexReviewPrepareKilledOnHangingGit(t *testing.T) {
	root := testRoot(t)
	src := mkCodexReviewSrcRepo(t) // 必须在劫持 PATH **之前**建:造仓要用真 git
	argvCap := filepath.Join(t.TempDir(), "argv.txt")

	origCap := codexPrepareTimeoutCap
	codexPrepareTimeoutCap = 700 * time.Millisecond
	defer func() { codexPrepareTimeoutCap = origCap }()
	fakeGitHangingOn(t, "clone")

	cfg := defaultConfig("")
	cfg.CodexBin = fakeCodexArgvCapture(t, argvCap)
	cfg.StepTimeoutMin = 5 // 远大于 700ms 子预算:证明击杀来自建副本子预算,不是步超时顺带收的
	task := &Task{ID: "cg-r3b-hang", Type: typeReview, Dir: src}

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, _, err := invokeCodex(context.Background(), root, cfg, task, "ping")
		done <- err
	}()
	select {
	case err := <-done:
		if el := time.Since(start); el > 20*time.Second {
			t.Fatalf("建副本阶段应在子预算(700ms)+收尾内被击杀, 实际耗时 %v", el)
		}
		// 断言 ②:回落 read-only 后 fake codex 照常 exit 0——建副本失败是降级,不是把卡判失败。
		if err != nil {
			t.Fatalf("建副本超时应回落 read-only 继续跑, got err=%v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("建副本阶段未被超时击杀(clone 挂死不受任何约束)——修 2 回归")
	}

	// 断言 ②(证据面):argv 走的是 read-only 回落径,绝不是 workspace-write。
	argv, err := os.ReadFile(argvCap)
	if err != nil {
		t.Fatalf("未捕获 codex argv: %v", err)
	}
	if got := string(argv); !strings.Contains(got, "read-only") || strings.Contains(got, "workspace-write") {
		t.Fatalf("建副本失败后应回落 --sandbox read-only, got argv:\n%s", got)
	}

	// 断言 ③:事件账本留痕,且 reason 精确到"超时"而非泛化失败(区分卡死与 git 真报错)。
	events, _, err := readEvents(eventsPath(root, task.ID))
	if err != nil {
		t.Fatalf("读事件账本: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev.Type != evStalled || ev.Actor != "runner:codex_review_prepare" {
			continue
		}
		if reason, _ := ev.Detail["reason"].(string); reason == "codex_review_prepare_timeout" {
			if fb, _ := ev.Detail["fallback_sandbox"].(string); fb != "read-only" {
				t.Fatalf("事件须披露回落到的沙箱, got fallback_sandbox=%q", fb)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("未找到 codex_review_prepare_timeout 事件(降级必须留痕), events=%+v", events)
	}

	// 断言 ④:半成品副本目录不得残留在 workRoot 下。
	entries, err := os.ReadDir(codexWorkRoot(root))
	if err == nil && len(entries) != 0 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("建副本被击杀后不应残留副本目录, got: %v", names)
	}
}

// TestReadmeCopyPhaseContractMatchesImplementation 绑定 P1-1③:双语契约句必须与实现同步且不超卖。
//
// 契约句所在文档:docs/guide.md / docs/guide.en.md(README 瘦身后,细节段落搬进进阶指南;
// 若日后再搬,改这里的 file 字段即可,双语同锚的语义不变)。
//
// 【修的病】上一轮双语契约句把"拷贝"写进 min(step_timeout,10min) 的子预算承诺,而拷贝腿
// 当时压根不查 ctx、还会跟随链接打开无写端 FIFO——契约句超卖,消费方(委托人/复审)据此以为
// "泳道不会被建副本堵死",实际堵得死死的。契约句是对外承诺,必须有测试钉住。
//
// 【锚三件事】
//
//	① 两份指南都得有这句 —— 只改中文不改英文照样过 = 双语漂移的假绿;
//	② 句子里必须出现本轮新增的两条硬约束(逐文件边界查预算 / 从不打开非常规文件),
//	   防止有人把句子改回旧版超卖措辞;
//	③ 实现侧**剥掉注释后**必须真有这两条的代码痕迹 —— 否则 README 可以单方面写得漂亮而代码回退,
//	   注释里的漂亮话也不算数(承 CG-R2c"剥注释后做 must[] 断言"的既定纪律)。
//
// 【杀的突变】把任一 README 的契约句改回旧版 → ① 或 ② 红;把 copyUntrackedPath 的 os.Lstat 换回
// os.Open、或删掉循环里的 ctx.Err() → ③ 红(即便注释还写着也照红)。
func TestReadmeCopyPhaseContractMatchesImplementation(t *testing.T) {
	docs := []struct {
		file   string
		anchor string
		must   []string
	}{
		{"docs/guide.md", "建副本阶段(探测/clone/apply/拷贝)", []string{
			"min(step_timeout, 10min)", "每个文件边界", "从不打开非常规文件",
			"symlink", "codex_review_prepare_timeout",
		}},
		{"docs/guide.en.md", "The copy-build phase (probe/clone/apply/copy)", []string{
			"min(step_timeout, 10min)", "at every file boundary", "never opens a non-regular file",
			"symlink", "codex_review_prepare_timeout",
		}},
	}
	for _, d := range docs {
		data, err := os.ReadFile(d.file)
		if err != nil {
			t.Fatalf("read %s: %v", d.file, err)
		}
		idx := strings.Index(string(data), d.anchor)
		if idx < 0 {
			t.Fatalf("%s: 找不到建副本子预算契约句锚点 %q(句子被删/改写 → 契约无处可核)", d.file, d.anchor)
		}
		// 只在该句所在的这一段里核 —— 换成全文 Contains 会被文档别处的同名词汇满足,变成恒真。
		para := string(data)[idx:]
		if end := strings.Index(para, "\n"); end >= 0 {
			para = para[:end]
		}
		for _, m := range d.must {
			if !strings.Contains(para, m) {
				t.Errorf("%s: 契约句缺少 %q —— 承诺与实现不符(上一轮 concerns 正是此病)\n段落: %s", d.file, m, para)
			}
		}
	}

	// ③ 实现侧痕迹:剥注释后再核,防"注释里写了就算数"。
	code := stripGoLineComments(t, "codex_worktree.go")
	for _, m := range []string{"ctx.Err()", "os.Lstat(", "os.Readlink(", "os.Symlink("} {
		if !strings.Contains(code, m) {
			t.Errorf("codex_worktree.go(剥注释后)缺少 %q —— README 的承诺在代码里没有对应实现", m)
		}
	}
}

// stripGoLineComments 去掉 Go 源码里的 `//` 行注释,只留可执行代码。
// 【为什么必须剥】本仓注释密度极高,直接对源文件做 Contains 会被注释里的同名词汇满足——
// 代码回退了、注释还在,断言照样绿(CG-R2c 已就同类恒真化立过规矩)。
func stripGoLineComments(t *testing.T, file string) string {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	var b strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// ---- CG-R3b R1·P1-1①:拷贝腿必须真的吃子预算(纯 Go 循环,没人替它查 ctx)----

// TestCopyUntrackedListChecksCtxBeforeFirstCopy 绑定"查在迭代**前**":ctx 已死时一条都不许搬。
//
// 【修的病】copyUntracked 的循环签名里收了 ctx,循环体一次不查——README 承诺"拷贝跑在
// min(step_timeout,10min) 子预算内",实现里却没有任何一处会因子预算到期而停手。超大 untracked 面
// (未忽略的 node_modules 之流)让这条承诺静默作废。
// 【杀的突变】删掉 copyUntrackedList 里的 `if err := ctx.Err(); err != nil` → 第一段两条断言全红
// (err 变 nil、a.txt 被搬进 dst)。
// 【为什么带正向对照】只断"死 ctx 报错"会被"函数恒报错"这种废实现满足;第二段用活 ctx 跑同一张表,
// 要求文件确实落地,把恒真解排除掉。
func TestCopyUntrackedListChecksCtxBeforeFirstCopy(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	rels := []string{"a.txt"}

	dead := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := copyUntrackedList(ctx, src, dead, rels)
	if err == nil {
		t.Fatal("ctx 已取消时拷贝腿必须立即中止并报错(否则子预算对这条腿形同虚设)")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("错误须 %%w 包装 ctx 错,调用侧才能落 codex_review_prepare_canceled/timeout, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dead, "a.txt")); statErr == nil {
		t.Fatal("ctx 已死却仍搬了第一条——检查点必须在每次迭代**之前**,不是之后")
	}

	// 正向对照:活 ctx 下同一张表必须照常搬完(排除"恒报错"的废实现满足上面的断言)。
	live := t.TempDir()
	if err := copyUntrackedList(context.Background(), src, live, rels); err != nil {
		t.Fatalf("活 ctx 下拷贝不应失败: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(live, "a.txt")); err != nil || string(data) != "A" {
		t.Fatalf("活 ctx 下 a.txt 应被原样搬过去, got data=%q err=%v", data, err)
	}
}

// TestCopyUntrackedListStopsMidLoop 绑定"**每次**迭代都查",而不是进循环前查一次就完事。
//
// 【为什么单独一条】只查一次的实现能让上面那条测试全绿,但真实病灶——"子预算在搬到一半时到期"
// (大仓/NFS 停顿/超大 untracked 面)——原样存活:循环照样一路搬到底。
// 【构造为什么是确定的】第一条故意做成 32MiB:watcher 一看到目标文件**被创建**就 cancel,此刻
// io.Copy 才刚开始搬这 32MiB(毫秒级),ctx 因此必定在"第一条搬完、第二条开搬之前"就已死透。
// 检测延迟是微秒级、拷贝是毫秒级,量级差三个数量级,不是靠竞速取胜。
// 【杀的突变】把 ctx 检查从循环体内挪到循环外(只查一次)→ b/c 照样被搬完,末条断言红。
func TestCopyUntrackedListStopsMidLoop(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a-big.bin"), make([]byte, 32<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(src, n), []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rels := []string{"a-big.bin", "b.txt", "c.txt"} // git ls-files 输出有序,此处同序

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := os.Stat(filepath.Join(dst, "a-big.bin")); err == nil {
				cancel() // 第一条刚开搬就掐掉子预算
				return
			}
			runtime.Gosched()
		}
	}()

	err := copyUntrackedList(ctx, src, dst, rels)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("子预算中途死掉,拷贝腿应报 ctx 错, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "c.txt")); err == nil {
		t.Fatal("子预算中途死掉后仍把整张表搬完——ctx 只在进循环前查了一次,大仓/NFS 场景照旧失控")
	}
}

// TestCodexReviewPrepareHonorsParentCtx 类闭合:子预算不得切断父 ctx。
// 【为什么单独一条】修 2 给建副本加了 min(step_timeout, 10min) 的独立子预算——若实现写成
// context.WithTimeout(context.Background(), ...) 之类脱离父 ctx 的形式,上面那条测试照样绿,
// 但 patrol/Ctrl-C/上游取消就再也传不进建副本阶段,"统一击杀路径"名存实亡。
// 【杀的突变】把 prepareCodexReviewWorkspace 里的 WithTimeout 基底换成 context.Background()
// → 父 ctx 300ms 到期后 clone 仍活着,本测试撞 select 超时红。
func TestCodexReviewPrepareHonorsParentCtx(t *testing.T) {
	root := testRoot(t)
	src := mkCodexReviewSrcRepo(t)

	origCap := codexPrepareTimeoutCap
	codexPrepareTimeoutCap = 10 * time.Minute // 子预算故意留得极宽:击杀只能来自父 ctx
	defer func() { codexPrepareTimeoutCap = origCap }()
	fakeGitHangingOn(t, "clone")

	cfg := &Config{CodexReviewSandbox: codexReviewSandboxWorktreeWrite} // StepTimeoutMin=0 → 子预算取 cap
	task := &Task{ID: "cg-r3b-parent", Type: typeReview, Dir: src}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	type result struct {
		dir string
		err error
	}
	done := make(chan result, 1)
	go func() {
		dir, cleanup, err := prepareCodexReviewWorkspace(ctx, root, cfg, task)
		cleanup()
		done <- result{dir, err}
	}()
	select {
	case r := <-done:
		if r.err == nil {
			t.Fatal("父 ctx 已到期,建副本不应报成功")
		}
		if !errors.Is(r.err, context.DeadlineExceeded) {
			t.Fatalf("错误须 %%w 包装 ctx 错(调用侧据此分事件 reason), got %v", r.err)
		}
		if r.dir != src {
			t.Fatalf("失败时 workDir 必须回落原仓, got %q want %q", r.dir, src)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("父 ctx 到期未能击杀建副本——子预算切断了父链,统一击杀路径失效")
	}
}

// TestCodexReviewPrepareReportsProbeKill 类闭合:被击杀的**前置探测**不得伪装成"不需要副本"。
//
// 【修的病(本轮突变演练发现)】codexReviewNeedsWorktree 的第三条件是 `git rev-parse` 探测,
// 它被 ctx 击杀时返回 false——与"这压根不是 git 工作树"完全同形。prepare 若不区分二者,就会以
// (t.Dir, noop, nil) 正常返回:invokeCodex 看到 err==nil,既不落事件也不打 stderr,一次超时被
// 静默记成"本卡不需要副本"。这与修 2 要消灭的"无声挂死"是同一类病,只是换了个入口——挂死改成了
// 无声降级,账本上依旧查不出这轮复审为什么没有动态验证能力。
//
// 【构造】假 git 吊住 rev-parse(而非 clone),父 ctx 300ms:探测必被击杀,且此时策略确实想要副本
// (worktree-write + design-review 卡),故必须报错而非静默回落。
// 【杀的突变】删掉 prepareCodexReviewWorkspace 里 `if ctxErr := ctx.Err(); ctxErr != nil && ...`
// 那段 → prepare 返回 nil error,本测试首条断言红。
func TestCodexReviewPrepareReportsProbeKill(t *testing.T) {
	root := testRoot(t)
	src := mkCodexReviewSrcRepo(t)

	origCap := codexPrepareTimeoutCap
	codexPrepareTimeoutCap = 10 * time.Minute // 子预算留宽:击杀只能来自父 ctx
	defer func() { codexPrepareTimeoutCap = origCap }()
	fakeGitHangingOn(t, "rev-parse")

	cfg := &Config{CodexReviewSandbox: codexReviewSandboxWorktreeWrite}
	task := &Task{ID: "cg-r3b-probe", Type: typeReview, Dir: src}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	type result struct {
		dir string
		err error
	}
	done := make(chan result, 1)
	go func() {
		dir, cleanup, err := prepareCodexReviewWorkspace(ctx, root, cfg, task)
		cleanup()
		done <- result{dir, err}
	}()
	select {
	case r := <-done:
		if r.err == nil {
			t.Fatal("前置探测被击杀必须报错(否则超时被静默记成'本卡不需要副本',降级隐身)")
		}
		if !errors.Is(r.err, context.DeadlineExceeded) {
			t.Fatalf("错误须 %%w 包装 ctx 错,调用侧才能落 prepare_timeout 事件, got %v", r.err)
		}
		if r.dir != src {
			t.Fatalf("失败时 workDir 必须回落原仓, got %q want %q", r.dir, src)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("前置探测未被 ctx 击杀")
	}

	// 反面守卫:策略本就不要副本(readonly)时,即便 ctx 已死透也不得报错——那是正常回落,
	// 报错会让 readonly 配置每轮白落一条降级事件,把账本刷成噪声。
	dead, deadCancel := context.WithCancel(context.Background())
	deadCancel()
	roCfg := &Config{CodexReviewSandbox: codexReviewSandboxReadonly}
	if dir, cleanup, err := prepareCodexReviewWorkspace(dead, root, roCfg, task); err != nil {
		cleanup()
		t.Fatalf("readonly 策略下 ctx 死透也应静默回落, got err=%v", err)
	} else {
		cleanup()
		if dir != src {
			t.Fatalf("readonly 回落应返回原仓, got %q", dir)
		}
	}
}
