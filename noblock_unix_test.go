//go:build !windows

package main

// noblock_unix_test.go —— CG-R3b R1:"域外路径的阻塞式 open"按类闭合回归。
//
// 【为什么整文件带 !windows 标签】用例要造 FIFO(syscall.Mkfifo)与 symlink(Windows 需特权),
// 二者都是 unix 专有;验收平台按卡契约是 darwin/Linux(BD-39 附记恒规),Windows 只保编译。
// 标签让 GOOS=windows 下整文件不参与编译,不给"Windows 编译红"留缝。
//
// 【本文件的共同回红结构】被测函数若退回"先 open 后判型"的写法,调用它就会**永久阻塞**在
// 无写端 FIFO 的 open 上——测试不会自己红,只会挂死。故每条都用 mustReturnWithin 把调用扔进
// goroutine + select 超时:挂死 → 报"未在限时内返回",红得干脆,不污染整轮 go test。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// mkFifo 造一个无读写端的命名管道:对它 os.Open(读)或 os.WriteFile(写)都会按 POSIX 永久阻塞。
func mkFifo(t *testing.T, path string) string {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Fatalf("mkfifo %s: %v", path, err)
	}
	return path
}

// mustReturnWithin 跑 fn 并要求它在 d 内返回,否则判"被阻塞"红。
// 【为什么不用 t.Deadline/直接调用】直接调用被阻塞的实现会让整个 go test 挂到 panic timeout(10min),
// 输出只有一坨 goroutine dump,看不出是哪条契约破了;包一层才能给出可读的失败原因。
func mustReturnWithin(t *testing.T, what string, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s 未在 %v 内返回——它把非常规文件(FIFO)打开了,阻塞在纯 Go syscall 里,"+
			"ctx/进程组击杀/patrol 一路都解不开", what, d)
	}
}

// TestOpenRegularFileNoBlockRejectsNonRegular 是共用闸门 openRegularFileNoBlock 的直接绑定。
// 【杀的突变】把实现换回 os.Open(path):FIFO 与 symlink→FIFO 两例挂死 → mustReturnWithin 红;
// 把 fstat 复核删掉:两例返回 err==nil → "必须报 errNotRegularFile" 断言红;
// 把 O_RDONLY 前的 O_NONBLOCK 去掉:FIFO 两例同样挂死(这才是真正防阻塞的那一位)。
func TestOpenRegularFileNoBlockRejectsNonRegular(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, "plain.txt")
	if err := os.WriteFile(reg, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	fifo := mkFifo(t, filepath.Join(dir, "pipe"))
	linkToFifo := filepath.Join(dir, "link-to-pipe")
	if err := os.Symlink(fifo, linkToFifo); err != nil {
		t.Fatal(err)
	}
	linkToReg := filepath.Join(dir, "link-to-plain")
	if err := os.Symlink(reg, linkToReg); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		path       string
		wantNotReg bool // 期望 errors.Is(err, errNotRegularFile)
		wantOK     bool // 期望成功打开并读到内容
	}{
		{"普通文件", reg, false, true},
		{"指向普通文件的链接(必须照常跟随)", linkToReg, false, true},
		{"裸 FIFO", fifo, true, false},
		{"指向 FIFO 的链接", linkToFifo, true, false},
		{"目录", dir, true, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			var f *os.File
			var err error
			mustReturnWithin(t, "openRegularFileNoBlock("+c.name+")", 10*time.Second, func() {
				f, _, err = openRegularFileNoBlock(c.path)
			})
			if f != nil {
				defer f.Close()
			}
			if c.wantNotReg {
				if !errors.Is(err, errNotRegularFile) {
					t.Fatalf("非普通文件必须报 errNotRegularFile(调用侧据此'跳过'而非'当 IO 故障'), got %v", err)
				}
				if f != nil {
					t.Fatal("非普通文件不得返回可用 fd")
				}
				return
			}
			if err != nil {
				t.Fatalf("普通文件(含指向它的链接)必须能正常打开, got %v", err)
			}
			data, err := os.ReadFile(c.path)
			if err != nil || string(data) != "hello" {
				t.Fatalf("内容应可读, got %q err=%v", data, err)
			}
		})
	}

	// 缺失路径:普通的 ENOENT,不该被误判成 errNotRegularFile(否则调用侧会把"文件没了"当成"跳过")。
	if _, _, err := openRegularFileNoBlock(filepath.Join(dir, "nope")); err == nil || errors.Is(err, errNotRegularFile) {
		t.Fatalf("不存在的路径应报 ENOENT 类错误而非 errNotRegularFile, got %v", err)
	}
}

// TestCopyUntrackedPathNeverOpensNonRegular 绑定 P1-1② 的三条:Lstat 前置判型、symlink 复制链接本体、
// 非常规文件跳过不报错。
//
// 【修的病】旧 copyFile 先 os.Open 后判 IsRegular——实测 `git ls-files --others --exclude-standard`
// 会把 untracked symlink 列出来(dangling 与指向 FIFO 的都列;裸 FIFO 反而不列),os.Open 跟随链接
// 打开无写端 FIFO 按 POSIX 永久阻塞,而阻塞发生在 IsRegular 防御**之前**。
// 【杀的突变】改回 os.Open 在前 → 第二子例挂死红;删掉 symlink 分支改成跟随复制 → dangling 子例
// 报 ENOENT(整个 prepare 就此必败)、链接子例变成普通文件(链接本体断言红)。
func TestCopyUntrackedPathNeverOpensNonRegular(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	// ① 裸 FIFO:跳过,不报错,也不在副本里留任何东西。
	fifo := mkFifo(t, filepath.Join(src, "pipe"))
	var err error
	mustReturnWithin(t, "copyUntrackedPath(裸 FIFO)", 10*time.Second, func() {
		err = copyUntrackedPath(fifo, filepath.Join(dst, "pipe"))
	})
	if err != nil {
		t.Fatalf("非常规文件应跳过而非报错(报错会让整个 prepare 失败→无谓全局降级), got %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(dst, "pipe")); statErr == nil {
		t.Fatal("裸 FIFO 不该在副本里留下任何条目")
	}

	// ② 指向 FIFO 的 symlink:复制链接本体,绝不跟随打开。
	if err := os.Symlink("pipe", filepath.Join(src, "link-to-pipe")); err != nil {
		t.Fatal(err)
	}
	mustReturnWithin(t, "copyUntrackedPath(symlink→FIFO)", 10*time.Second, func() {
		err = copyUntrackedPath(filepath.Join(src, "link-to-pipe"), filepath.Join(dst, "link-to-pipe"))
	})
	if err != nil {
		t.Fatalf("symlink→FIFO 应按链接本体复制, got %v", err)
	}
	assertSymlinkTo(t, filepath.Join(dst, "link-to-pipe"), "pipe")

	// ③ dangling symlink:同样按链接本体复制,不得报错(旧实现在此 ENOENT → prepare 必败 → 该仓
	//    每次复审都无谓降级 read-only,正是审查报告点名的兄弟洞)。
	if err := os.Symlink("nowhere", filepath.Join(src, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := copyUntrackedPath(filepath.Join(src, "dangling"), filepath.Join(dst, "dangling")); err != nil {
		t.Fatalf("dangling symlink 不得让拷贝腿失败, got %v", err)
	}
	assertSymlinkTo(t, filepath.Join(dst, "dangling"), "nowhere")

	// ④ 普通文件:内容 + 权限位原样(untracked 里的 shell 脚本要保住 +x,复审才跑得起来)。
	script := filepath.Join(src, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyUntrackedPath(script, filepath.Join(dst, "run.sh")); err != nil {
		t.Fatalf("普通文件拷贝不应失败: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dst, "run.sh"))
	if err != nil || !strings.Contains(string(data), "echo hi") {
		t.Fatalf("普通文件内容应原样, got %q err=%v", data, err)
	}
	fi, err := os.Stat(filepath.Join(dst, "run.sh"))
	if err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("可执行位必须保留(untracked 脚本复审要跑), got mode=%v err=%v", fi.Mode(), err)
	}
}

// assertSymlinkTo 断言 path 是 symlink **本体**(Lstat 判型)且指向 want。
// 【为什么用 Lstat 而不是 Stat】Stat 跟随链接:若实现退回"跟随复制",Stat 看到的是普通文件、
// 断言照样能过——恒真化。Lstat 才能证明副本里躺着的是链接而不是内容拷贝。
func assertSymlinkTo(t *testing.T, path, want string) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("副本里应有 %s: %v", path, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s 必须是 symlink 本体(mode=%v)——跟随复制会把无写端 FIFO 变成永久阻塞", path, fi.Mode())
	}
	got, err := os.Readlink(path)
	if err != nil || got != want {
		t.Fatalf("%s 链接目标应为 %q, got %q err=%v", path, want, got, err)
	}
}

// TestCodexReviewCopySurvivesUntrackedSymlinkToFifo 是 P1-1 的端到端回红反例:
// 原仓 untracked 面里躺着 symlink→FIFO + dangling symlink,建副本必须照常在限时内建成。
//
// 【为什么必须端到端】单元测 copyUntrackedPath 只证"这个函数不开非常规文件";真正的契约是
// "整条建副本腿不会被一条 untracked 链接堵死"。这条走 prepareCodexReviewWorkspace,连带覆盖
// git ls-files 会不会把这些链接列出来(实测:会)。
// 【杀的突变】copyUntrackedPath 改回先 os.Open → 本测试挂死在 30s 红。
func TestCodexReviewCopySurvivesUntrackedSymlinkToFifo(t *testing.T) {
	root := testRoot(t)
	src := mkCodexReviewSrcRepo(t)
	mkFifo(t, filepath.Join(src, "realfifo"))
	for _, l := range [][2]string{{"realfifo", "link-to-fifo"}, {"nowhere", "dangling"}, {"committed.txt", "good-link"}} {
		if err := os.Symlink(l[0], filepath.Join(src, l[1])); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &Config{CodexReviewSandbox: codexReviewSandboxWorktreeWrite}
	task := &Task{ID: "cg-r3b-r1-fifo", Type: typeReview, Dir: src}

	var copyDir string
	var cleanup func()
	var err error
	mustReturnWithin(t, "prepareCodexReviewWorkspace(untracked 面含 symlink→FIFO)", 30*time.Second, func() {
		copyDir, cleanup, err = prepareCodexReviewWorkspace(context.Background(), root, cfg, task)
	})
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("untracked 面里的链接不得让建副本失败(dangling 也不行,否则该仓每轮复审都降级): %v", err)
	}
	if copyDir == src {
		t.Fatal("应当真的建了副本(否则下面的断言全是空转)")
	}

	// 链接按本体投影,FIFO 本身不进副本(git ls-files 不列裸 FIFO,即便列了也会被跳过)。
	assertSymlinkTo(t, filepath.Join(copyDir, "link-to-fifo"), "realfifo")
	assertSymlinkTo(t, filepath.Join(copyDir, "dangling"), "nowhere")
	assertSymlinkTo(t, filepath.Join(copyDir, "good-link"), "committed.txt")
	if _, err := os.Lstat(filepath.Join(copyDir, "realfifo")); err == nil {
		t.Fatal("裸 FIFO 不该被投影进副本")
	}
	// 正向面不回归:普通 untracked 与 dirty tracked 照常在副本里。
	if data, err := os.ReadFile(filepath.Join(copyDir, "untr.txt")); err != nil || string(data) != "fresh\n" {
		t.Fatalf("普通 untracked 仍须落副本, got %q err=%v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(copyDir, "committed.txt")); err != nil || !strings.Contains(string(data), "DIRTY") {
		t.Fatalf("dirty tracked 面仍须落副本, got %q err=%v", data, err)
	}
}

// TestCodexReviewCopyGuardsMarkerNamespace 是本轮自查发现的兄弟洞:marker 命名空间被 untracked 面污染。
//
// 【病】副本内容部分来自业务仓 untracked 面(域外输入)。若那边有个名叫 .claudego-codex-work.json.tmp
// 的 symlink→FIFO 被投影进副本,随后 writeCodexWorkMarker → atomicWrite → os.WriteFile 会**以写端
// 打开无读端 FIFO**,同样按 POSIX 永久阻塞——与 P1-1 同类,只是方向从读换成了写,且照样把泳道占死。
// 【杀的突变】删掉 copyUntrackedList 里的 marker 命名空间跳过 **且** 删掉 writeCodexWorkMarker 里的
// 两次 os.Remove → prepare 挂死在 30s 红(两道防线各删一道则另一道兜住,这正是要的冗余;实测:
// 只删跳过、保留 Remove 时本测试仍绿)。
// 【链接为什么必须是绝对路径】裸 FIFO 不会被 git 列出、也不会被拷贝腿投影,所以副本里没有 `pipe`。
// 相对链接落到副本里就是条 dangling link,os.WriteFile 会顺着它新建普通文件——不阻塞,只把 marker
// 变成链接(旧构造只能测到这一层,配不上"永久阻塞"的说法)。指向原仓 FIFO 的**绝对**链接才真的把
// 无读端管道带进写路径,突变下才会如实挂死。
func TestCodexReviewCopyGuardsMarkerNamespace(t *testing.T) {
	root := testRoot(t)
	src := mkCodexReviewSrcRepo(t)
	fifo := mkFifo(t, filepath.Join(src, "pipe"))
	for _, name := range []string{codexWorkMarkerName, codexWorkMarkerName + ".tmp"} {
		if err := os.Symlink(fifo, filepath.Join(src, name)); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &Config{CodexReviewSandbox: codexReviewSandboxWorktreeWrite}
	task := &Task{ID: "cg-r3b-r1-marker", Type: typeReview, Dir: src}

	var copyDir string
	var cleanup func()
	var err error
	mustReturnWithin(t, "prepareCodexReviewWorkspace(untracked 面冒名 marker)", 30*time.Second, func() {
		copyDir, cleanup, err = prepareCodexReviewWorkspace(context.Background(), root, cfg, task)
	})
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("untracked 面冒名 marker 不得让建副本失败: %v", err)
	}
	// marker 必须是 ClaudeGo 自己写的那份真货(内容对得上),而不是被业务仓的同名链接顶掉。
	m, ok := readCodexWorkMarker(copyDir)
	if !ok {
		t.Fatal("marker 必须建齐并可读(否则崩溃对账把活副本当半成品清掉)")
	}
	if m.TaskID != task.ID || m.PID != os.Getpid() {
		t.Fatalf("marker 内容须来自本进程, got %+v", m)
	}
	if fi, err := os.Lstat(filepath.Join(copyDir, codexWorkMarkerName)); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("marker 必须是普通文件而非被投影进来的链接, got mode=%v err=%v", fi.Mode(), err)
	}
}

// TestCleanupCodexReviewOrphansNeverBlocksOnFifoMarker 类闭合:tick 的孤儿对账读 marker 也是域外读。
//
// 【病】崩溃点若落在"untracked 拷贝完成、marker 未写"之间,残留副本里就可能有个名叫
// .claudego-codex-work.json 的 symlink→FIFO(来自业务仓 untracked 面)。readCodexWorkMarker 若用
// os.ReadFile,tick 每轮扫到它就永久阻塞——整条对账线程占死,比被清理的残留严重得多。
// 【杀的突变】readCodexWorkMarker 改回 os.ReadFile → 本测试挂死在 20s 红。
func TestCleanupCodexReviewOrphansNeverBlocksOnFifoMarker(t *testing.T) {
	root := testRoot(t)
	workRoot := codexWorkRoot(root)
	copyDir := filepath.Join(workRoot, "crashed-1234-1")
	if err := os.MkdirAll(copyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mkFifo(t, filepath.Join(copyDir, "pipe"))
	if err := os.Symlink("pipe", filepath.Join(copyDir, codexWorkMarkerName)); err != nil {
		t.Fatal(err)
	}
	// 半成品分支要求目录 mtime 老于 5 分钟(防误清正在建的副本),这里把它做旧。
	old := time.Now().Add(-30 * time.Minute)
	if err := os.Chtimes(copyDir, old, old); err != nil {
		t.Fatal(err)
	}

	mustReturnWithin(t, "cleanupCodexReviewOrphans(残留里 marker 是 FIFO 链接)", 20*time.Second, func() {
		cleanupCodexReviewOrphans(root, map[string]bool{})
	})
	if _, err := os.Stat(copyDir); !os.IsNotExist(err) {
		t.Fatalf("marker 读不出(非普通文件)应按半成品清掉, 残留仍在: %v", err)
	}
}

// TestExtractEmitTasksSkipsNonRegularRescueFile 类闭合:runner 的"文件救援"腿同属域外读。
//
// 【病】救援腿按模型输出里出现的 *.json 文件名去任务目录(= 业务仓工作目录)里捞清单。旧闸门只判
// IsDir/Size:os.Stat 跟随链接,symlink→FIFO 的 stat 是"非目录、size=0",能过闸,随后 os.ReadFile
// 在纯 Go syscall 里永久阻塞——与 copyFile 那条腿同一条泳道、同一种死法。
// 【杀的突变】把闸门改回 `fi.IsDir()`(不判 IsRegular)→ 第一子例挂死在 20s 红;
// 把 os.Stat 改成 os.Lstat(过度收紧)→ 第三子例红(指向普通文件的链接被误杀)。
func TestExtractEmitTasksSkipsNonRegularRescueFile(t *testing.T) {
	dir := t.TempDir()
	mkFifo(t, filepath.Join(dir, "pipe"))
	if err := os.Symlink("pipe", filepath.Join(dir, "_WAVE-1-TASKS.json")); err != nil {
		t.Fatal(err)
	}
	good := `{"tasks":[{"title":"真清单","prompts":["p1"]}]}`
	if err := os.WriteFile(filepath.Join(dir, "_WAVE-2-TASKS.json"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("_WAVE-2-TASKS.json", filepath.Join(dir, "_WAVE-3-TASKS.json")); err != nil {
		t.Fatal(err)
	}

	// ① 指向 FIFO 的救援文件:必须跳过并如实报"没捞到",不得挂死。
	var tasks []emitTask
	var err error
	mustReturnWithin(t, "extractEmitTasks(救援文件是 FIFO 链接)", 20*time.Second, func() {
		tasks, err = extractEmitTasks("清单已写入 _WAVE-1-TASKS.json,请查收", dir)
	})
	if err == nil {
		t.Fatalf("救援文件不可读时应如实报错, got tasks=%+v", tasks)
	}

	// ② 正向不回归:普通救援文件照捞(排除"把整条救援腿关掉"这种偷懒解)。
	if tasks, err = extractEmitTasks("清单见 _WAVE-2-TASKS.json", dir); err != nil || len(tasks) != 1 || tasks[0].Title != "真清单" {
		t.Fatalf("普通救援文件必须照常解析, got tasks=%+v err=%v", tasks, err)
	}

	// ③ 指向普通文件的链接同样该捞到:闸门收紧不得误伤合法 symlink(判的是 stat 后的目标类型)。
	if tasks, err = extractEmitTasks("清单见 _WAVE-3-TASKS.json", dir); err != nil || len(tasks) != 1 {
		t.Fatalf("指向普通文件的链接不该被闸门误杀, got tasks=%+v err=%v", tasks, err)
	}
}

// TestSessionTitleSkipsNonRegular 类闭合:sessions 腿读的是 claude CLI 自己的目录(域外)。
// 【病】listSessions 只滤 IsDir + .jsonl 后缀,而 DirEntry.IsDir() 对 symlink 恒为 false——
// 一条 <id>.jsonl → FIFO 的链接就能让 `claudego sessions` 整条命令挂死。
// 【杀的突变】sessionTitle 改回 os.Open → 第一子例挂死红;第二子例保证没把功能整条关掉。
func TestSessionTitleSkipsNonRegular(t *testing.T) {
	dir := t.TempDir()
	mkFifo(t, filepath.Join(dir, "pipe"))
	if err := os.Symlink("pipe", filepath.Join(dir, "blocked.jsonl")); err != nil {
		t.Fatal(err)
	}
	var got string
	mustReturnWithin(t, "sessionTitle(会话文件是 FIFO 链接)", 20*time.Second, func() {
		got = sessionTitle(filepath.Join(dir, "blocked.jsonl"))
	})
	if got != "-" {
		t.Fatalf("不可读的会话文件应回落 %q, got %q", "-", got)
	}

	real := filepath.Join(dir, "ok.jsonl")
	if err := os.WriteFile(real, []byte(`{"type":"user","message":{"content":"帮我看看限额"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := sessionTitle(real); got != "帮我看看限额" {
		t.Fatalf("正常会话文件的标题应照常解析, got %q", got)
	}
}

// TestBuildTokenSeriesSkipsNonRegularTranscript 类闭合:board 燃尽曲线扫的 ~/.claude/projects 同属域外。
// 【病】WalkDir 的 d.IsDir() 对 symlink 恒为 false,一条 *.jsonl → FIFO 的链接会让采样 goroutine
// 永久阻塞——board 的 web handler 再不返回,燃尽页从此定格。
// 【杀的突变】boardburn 里改回 os.Open(p) → 本测试挂死在 20s 红。
func TestBuildTokenSeriesSkipsNonRegularTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	projDir := filepath.Join(home, ".claude", "projects", "some-proj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mkFifo(t, filepath.Join(projDir, "pipe"))
	if err := os.Symlink("pipe", filepath.Join(projDir, "sess.jsonl")); err != nil {
		t.Fatal(err)
	}
	// 前提守卫:transcriptRoot 必须真的指到这个临时 HOME,否则本测试是空转的假绿。
	if got := transcriptRoot(); got != filepath.Join(home, ".claude", "projects") {
		t.Fatalf("transcript 根未指向测试 HOME(用例空转), got %q", got)
	}

	mustReturnWithin(t, "buildTokenSeries(transcript 里有 FIFO 链接)", 20*time.Second, func() {
		_ = buildTokenSeries(&Config{}, time.Now(), "24h")
	})
}
