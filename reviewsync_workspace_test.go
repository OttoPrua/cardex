package main

// CG-R2 review-sync 工作树洞根修：验收本地模拟同步 + 指纹自门。
//
// 【背景】旧版 ~/.claudego/sync-lane-to-5090.sh 仅同步已提交历史（git bundle），修复卡守
// "不 commit、workspace 待复审"纪律，远端复审面对修复前代码 → CG-1 修复链三轮空转。
//
// 【两层修法】
//   ① 同步层：sync 补送 `git diff --binary --no-renames HEAD` + 真正 untracked 文件 tar
//      （尊重 .gitignore，禁整树 tar 防 CRLF/xattr 假改动，同 5090 spec 既有原则）；
//   ② 护栏层：sync 生成 workspace-fingerprint 落 <mirror>/.claudego-fingerprint；
//      design-review 模板开工自门跑 verify-mirror-fingerprint.sh 现场重算+比对，不一致停卡报"镜像过期"。
//
// 本文件覆盖本地模拟验收（CLAUDEGO_SYNC_MIRROR_MODE=local）——Windows 远端实跑属后续实测项。
// runner.go 的同步调用接口未改（config default_review_sync/review_host/mirror_root 命令面不动，
// 只改 ~/.claudego/*.sh 与 templates），因此既有 fallback 语义回归由 reviewdivert_test.go 覆盖不变。

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// syncScriptsRoot 返回 ~/.claudego 绝对路径。所有脚本共用一份装机版,测试与生产同源。
func syncScriptsRoot(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	d := filepath.Join(home, ".claudego")
	for _, name := range []string{"sync-lane-to-5090.sh", "workspace-fingerprint.sh", "verify-mirror-fingerprint.sh"} {
		p := filepath.Join(d, name)
		fi, err := os.Stat(p)
		if err != nil {
			t.Skipf("跳过:装机脚本 %s 缺失 (%v)——本卡验收依赖 ~/.claudego 就位", p, err)
		}
		if fi.Mode()&0o111 == 0 {
			t.Fatalf("装机脚本 %s 无执行权限(chmod +x)", p)
		}
	}
	return d
}

// mkSyncSourceRepo 造一个含"已提交 + dirty tracked + untracked + gitignored"四类文件的源仓,
// 用于验证 sync 覆盖 tracked 未提交面 + 真正 untracked 且尊重 .gitignore。返回工作树顶。
func mkSyncSourceRepo(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	run := func(dir, name string, args ...string) {
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	run(src, "git", "init", "-q", "-b", "main", ".")
	run(src, "git", "config", "user.email", "sync-test@example.com")
	run(src, "git", "config", "user.name", "sync-test")
	// 已提交面:tracked/a.txt、tracked/keep.txt、.gitignore(排除 tmp/)。
	if err := os.MkdirAll(filepath.Join(src, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "subdir", "a.txt"), []byte("orig\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "keep.txt"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".gitignore"), []byte("tmp/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(src, "git", "add", ".")
	run(src, "git", "commit", "-q", "-m", "init")
	// dirty tracked:改 subdir/a.txt(旧 sync 落不下来)。
	if err := os.WriteFile(filepath.Join(src, "subdir", "a.txt"), []byte("orig\nMODIFIED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// untracked:untr.txt(旧 sync 落不下来)。
	if err := os.WriteFile(filepath.Join(src, "untr.txt"), []byte("new content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// gitignored:tmp/skipme.bin(不应落到镜像)。
	if err := os.MkdirAll(filepath.Join(src, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "tmp", "skipme.bin"), []byte("should be ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return src
}

// runSyncLocal 以本地模拟模式跑 sync-lane-to-5090.sh:cwd=src,MIRROR_ROOT=mroot。
func runSyncLocal(t *testing.T, src, mroot string) (stdout, stderr string, exitCode int) {
	t.Helper()
	scripts := syncScriptsRoot(t)
	cmd := exec.Command("bash", filepath.Join(scripts, "sync-lane-to-5090.sh"))
	cmd.Dir = src
	cmd.Env = append(os.Environ(),
		"CLAUDEGO_SYNC_MIRROR_MODE=local",
		"CLAUDEGO_SYNC_LOCAL_MIRROR_ROOT="+mroot,
	)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	exitCode = 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}
	return outBuf.String(), errBuf.String(), exitCode
}

// mirrorDir 返回本地模拟模式下的镜像路径:$mroot/<wt 名>。
func mirrorDir(t *testing.T, src, mroot string) string {
	t.Helper()
	return filepath.Join(mroot, filepath.Base(src))
}

// readFingerprint 解析 .claudego-fingerprint 五行 KV 结构,便于 case 级断言。
func readFingerprint(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fingerprint %s: %v", path, err)
	}
	kv := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		i := strings.Index(line, "=")
		if i <= 0 {
			continue
		}
		kv[line[:i]] = line[i+1:]
	}
	return kv
}

// ① 主验收:本地未提交 tracked 改动 + 真正 untracked 文件 → sync → 断言镜像含两者且内容一致,
// 同时断言 .gitignore 文件(tmp/skipme.bin)不进镜像(.gitignore 由 git ls-files --exclude-standard 保证)。
func TestSyncCarriesDirtyAndUntrackedAndRespectsGitignore(t *testing.T) {
	src := mkSyncSourceRepo(t)
	mroot := t.TempDir()
	out, errOut, code := runSyncLocal(t, src, mroot)
	if code != 0 {
		t.Fatalf("sync 应 exit 0, got %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	m := mirrorDir(t, src, mroot)

	// dirty tracked 落到镜像且内容与源等同(旧版 bundle-only 会缺 "MODIFIED" 那行)。
	got, err := os.ReadFile(filepath.Join(m, "subdir", "a.txt"))
	if err != nil {
		t.Fatalf("mirror subdir/a.txt: %v", err)
	}
	if string(got) != "orig\nMODIFIED\n" {
		t.Fatalf("dirty tracked 未完整落到镜像\n实际:%q\n期望:%q", got, "orig\nMODIFIED\n")
	}

	// 真正 untracked 落到镜像。
	got, err = os.ReadFile(filepath.Join(m, "untr.txt"))
	if err != nil {
		t.Fatalf("mirror untr.txt(untracked)未落到镜像: %v", err)
	}
	if string(got) != "new content\n" {
		t.Fatalf("mirror untr.txt 内容不一致\n实际:%q\n期望:%q", got, "new content\n")
	}

	// .gitignore 文件不进镜像(整树 tar 会带,禁——同 5090 spec 原则)。
	if _, err := os.Stat(filepath.Join(m, "tmp", "skipme.bin")); !os.IsNotExist(err) {
		t.Fatalf("gitignored 文件不应进镜像 (tmp/ 已 gitignore),但 stat err=%v", err)
	}

	// fingerprint 文件落盘且五字段齐全。
	fp := readFingerprint(t, filepath.Join(m, ".claudego-fingerprint"))
	for _, k := range []string{"HEAD", "DIRTY_COUNT", "DIRTY_MANIFEST_SHA", "UNTRACKED_COUNT", "UNTRACKED_MANIFEST_SHA"} {
		if fp[k] == "" {
			t.Fatalf(".claudego-fingerprint 缺字段 %s\n实际:%+v", k, fp)
		}
	}
	// dirty=1(subdir/a.txt), untracked=1(untr.txt;.gitignore 已提交、tmp/skipme.bin gitignored)。
	if fp["DIRTY_COUNT"] != "1" {
		t.Fatalf("DIRTY_COUNT 应为 1, got %s", fp["DIRTY_COUNT"])
	}
	if fp["UNTRACKED_COUNT"] != "1" {
		t.Fatalf("UNTRACKED_COUNT 应为 1(只有 untr.txt), got %s", fp["UNTRACKED_COUNT"])
	}
}

// ② 反例①(镜像缺未提交改动 → verify 必红):sync 完成后手工 revert 镜像上的 dirty 文件到 HEAD 内容,
// 模拟"旧版 sync 只带已提交历史"的破坏。verify-mirror-fingerprint.sh 必须报 exit 1 现场指纹不等。
// 【杀的突变】把 sync 里的 apply_to_mirror 中 `git apply` 一行删掉 → 镜像仍是 HEAD 原样 → 本用例红。
func TestVerifyMirrorFingerprintRedOnDirtyRevert(t *testing.T) {
	src := mkSyncSourceRepo(t)
	mroot := t.TempDir()
	if _, _, code := runSyncLocal(t, src, mroot); code != 0 {
		t.Fatalf("准备阶段 sync 应成功, got exit=%d", code)
	}
	m := mirrorDir(t, src, mroot)

	// 破坏:把镜像上 subdir/a.txt 回退到 HEAD("orig\n"),等价于"旧版 bundle-only 同步"的产物。
	if err := os.WriteFile(filepath.Join(m, "subdir", "a.txt"), []byte("orig\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code := runVerify(t, m)
	if code != 1 {
		t.Fatalf("镜像缺未提交改动时 verify-mirror-fingerprint.sh 必须 exit 1(内容不一致), got %d——若为 0 说明现场指纹重算漏掉了 dirty 面,不能挡住旧版 sync 缺陷", code)
	}
}

// ③ 反例②(指纹文件缺失 → verify 报 stale 非默认放行):删掉 .claudego-fingerprint。
// verify 必须报 exit 2("镜像过期"),而非静默通过。
// 【杀的突变】verify 若把"文件缺失"当成"未跑过 sync,可跳过检查"默认放行 → 本用例红。
func TestVerifyMirrorFingerprintRedOnMissingFingerprintFile(t *testing.T) {
	src := mkSyncSourceRepo(t)
	mroot := t.TempDir()
	if _, _, code := runSyncLocal(t, src, mroot); code != 0 {
		t.Fatalf("准备阶段 sync 应成功, got exit=%d", code)
	}
	m := mirrorDir(t, src, mroot)

	// 破坏:删掉指纹文件,模拟"旧版 sync 从没写过它"。
	if err := os.Remove(filepath.Join(m, ".claudego-fingerprint")); err != nil {
		t.Fatal(err)
	}

	code := runVerify(t, m)
	if code != 2 {
		t.Fatalf(".claudego-fingerprint 缺失时 verify 必须 exit 2(镜像过期), got %d——若为 0 会让复审对旧代码空转出 verdict", code)
	}
}

// ④ 反例:sync 层反向证明——sync 缺 fingerprint 脚本时必须非零退出。
// 保证 runner.go 的既有 fallback 语义(sync exit 非 0 → 回退本机审)在新 sync 上仍成立。
func TestSyncFailsWhenFingerprintScriptMissing(t *testing.T) {
	src := mkSyncSourceRepo(t)
	mroot := t.TempDir()
	scripts := syncScriptsRoot(t)

	// 假 HOME → workspace-fingerprint.sh 不存在 → sync exit≠0(阻 runner 无害回退)。
	fakeHome := t.TempDir()
	cmd := exec.Command("bash", filepath.Join(scripts, "sync-lane-to-5090.sh"))
	cmd.Dir = src
	cmd.Env = []string{
		"HOME=" + fakeHome,
		"PATH=" + os.Getenv("PATH"),
		"CLAUDEGO_SYNC_MIRROR_MODE=local",
		"CLAUDEGO_SYNC_LOCAL_MIRROR_ROOT=" + mroot,
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	if err == nil {
		t.Fatalf("fingerprint 脚本缺失时 sync 应失败(runner 靠此非零退出触发回退本机审), 实际 exit 0\n输出:%s", buf.String())
	}
}

// runVerify 直接跑 verify-mirror-fingerprint.sh <mirror>,返回退出码。
// 复审 LLM 在 design-review.md 开工自门里跑的就是这段——用同一脚本才能证明模板自门覆盖到位。
func runVerify(t *testing.T, mirror string) int {
	t.Helper()
	scripts := syncScriptsRoot(t)
	cmd := exec.Command("bash", filepath.Join(scripts, "verify-mirror-fingerprint.sh"), mirror)
	cmd.Env = os.Environ()
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	t.Fatalf("verify-mirror-fingerprint.sh 非 ExitError 失败:%v\n输出:%s", err, buf.String())
	return -1
}

// ⑤ 反例③:未提交面在源仓被再次修改后,重跑 sync 应把镜像更新到最新 workspace,fingerprint 随更新。
// 保证"两次 sync 之间源仓再变" 的正确流转,防"缓存跑偏"。
func TestSyncReflectsFreshWorkspaceOnRerun(t *testing.T) {
	src := mkSyncSourceRepo(t)
	mroot := t.TempDir()
	if _, _, code := runSyncLocal(t, src, mroot); code != 0 {
		t.Fatalf("首次 sync 应成功, got exit=%d", code)
	}
	m := mirrorDir(t, src, mroot)
	fp1 := readFingerprint(t, filepath.Join(m, ".claudego-fingerprint"))

	// 追加一次未提交改动 + 一个新的 untracked。
	if err := os.WriteFile(filepath.Join(src, "subdir", "a.txt"), []byte("orig\nMODIFIED\nSECOND\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "untr2.txt"), []byte("2nd untr\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runSyncLocal(t, src, mroot); code != 0 {
		t.Fatalf("重跑 sync 应成功, got exit=%d", code)
	}
	fp2 := readFingerprint(t, filepath.Join(m, ".claudego-fingerprint"))

	if fp1["DIRTY_MANIFEST_SHA"] == fp2["DIRTY_MANIFEST_SHA"] {
		t.Fatal("dirty 面变了,DIRTY_MANIFEST_SHA 应变化(否则说明重跑 sync 用了缓存旧内容)")
	}
	if fp1["UNTRACKED_MANIFEST_SHA"] == fp2["UNTRACKED_MANIFEST_SHA"] {
		t.Fatal("untracked 面新增文件,UNTRACKED_MANIFEST_SHA 应变化")
	}
	// 镜像内容也应更新(mirror 是每次全量重建)。
	got, err := os.ReadFile(filepath.Join(m, "subdir", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "orig\nMODIFIED\nSECOND\n" {
		t.Fatalf("重跑后镜像 dirty 文件应带 SECOND, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(m, "untr2.txt")); err != nil {
		t.Fatalf("重跑后镜像应含新 untracked 文件 untr2.txt: %v", err)
	}

	// 现场重算与新 fingerprint 一致(sync 完成态自恰)。
	code := runVerify(t, m)
	if code != 0 {
		t.Fatalf("重跑后 verify 应通过, got exit=%d", code)
	}
}
