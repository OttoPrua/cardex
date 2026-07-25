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
// 【R2·2026-07-23 变更】fingerprint 从"5 行 KV(DIRTY_/UNTRACKED_ split)"改为
// "3 行 header + `---` + per-file manifest"，dirty 与 untracked 合一按 path→内容哈希入 manifest。
// 目的：staged 未提交文件在 Mac 与镜像上分侧不一致的假过期(P1-1)被消灭；同时 manifest 明文
// 化后 Windows 沙箱可用 PowerShell 内置 Get-FileHash 逐行比对(P0-1)。
//
// 本文件覆盖本地模拟验收（CLAUDEGO_SYNC_MIRROR_MODE=local）——Windows 远端实跑属后续实测项。
// runner.go 的同步调用接口未改（config default_review_sync/review_host/mirror_root 命令面不动，
// 只改 ~/.claudego/*.sh 与 templates），因此既有 fallback 语义回归由 reviewdivert_test.go 覆盖不变。

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// syncScriptsRoot 返回 ~/.claudego 绝对路径。所有脚本共用一份装机版,测试与生产同源。
//
// 【CG-R2 R1·2026-07-23】环境变量升级门:
//   - 缺省(未设 CLAUDEGO_REQUIRE_SYNC_SCRIPTS):脚本缺失 → t.Skipf 兜底(CI/异环境不报红)。
//   - CLAUDEGO_REQUIRE_SYNC_SCRIPTS=1:脚本缺失 → t.Fatal 显式红,防上一轮"装机缺失全跳过、
//     套件仍绿、对护栏部署状态零证明力"的静默漂绿。已装机机器/装机验收流水必设此变量。
//   - 【R2·P1-3】仓内验收入口 `make accept-sync` 硬编码 env=1,收工汇报引用其实跑输出;
//     另有 TestSyncScriptsInstalled 哨兵测试,不依赖装机,单独探测装机情况;env=1 时它就是最后的红线。
//   - 【R2 新增】.ps1 版本 verify 脚本纳入必装清单(Windows 沙箱无 bash 通道时的兜底)。
func syncScriptsRoot(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	d := filepath.Join(home, ".claudego")
	requireEnv := os.Getenv("CLAUDEGO_REQUIRE_SYNC_SCRIPTS") == "1"
	// .sh 系必须可执行;.ps1 只需存在(PowerShell 靠扩展名调用,无执行位)。
	needExec := map[string]bool{
		"sync-lane-to-5090.sh":          true,
		"workspace-fingerprint.sh":      true,
		"verify-mirror-fingerprint.sh":  true,
		"verify-mirror-fingerprint.ps1": false,
	}
	for _, name := range []string{"sync-lane-to-5090.sh", "workspace-fingerprint.sh", "verify-mirror-fingerprint.sh", "verify-mirror-fingerprint.ps1"} {
		p := filepath.Join(d, name)
		fi, err := os.Stat(p)
		if err != nil {
			if requireEnv {
				t.Fatalf("装机脚本 %s 缺失 (%v)——CLAUDEGO_REQUIRE_SYNC_SCRIPTS=1 下必须存在", p, err)
			}
			t.Skipf("跳过:装机脚本 %s 缺失 (%v)——本卡验收依赖 ~/.claudego 就位;要在目标环境显式红请设 CLAUDEGO_REQUIRE_SYNC_SCRIPTS=1", p, err)
		}
		if needExec[name] && fi.Mode()&0o111 == 0 {
			t.Fatalf("装机脚本 %s 无执行权限(chmod +x)", p)
		}
	}
	return d
}

// TestSyncScriptsInstalled 哨兵:不依赖装机脚本本体运行,单独探测装机情况。
// 缺省报告缺失清单但不 fail(避免异环境噪音);CLAUDEGO_REQUIRE_SYNC_SCRIPTS=1 时缺失即红。
//
// 【R1 目的】上一轮 P1-1 的根因是"5 个装机依赖用例在缺失时全部 Skip 但套件绿,对护栏部署状态零证明力"。
// 该哨兵在装机验收流水(设 env=1)下能捕获脚本漂移/漏装/权限漂;缺省仍不吵闹。
// 【R2 追加】.ps1 版本作为 Windows 沙箱兜底,同样纳入清单;.ps1 不检查执行位(靠扩展名调用)。
// 【杀的突变】把 syncScriptsRoot 的 Skip 逻辑改回"仅 Skip 不看环境变量"→ 此哨兵在 env=1 下仍红。
func TestSyncScriptsInstalled(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	requireEnv := os.Getenv("CLAUDEGO_REQUIRE_SYNC_SCRIPTS") == "1"
	needExec := map[string]bool{
		"sync-lane-to-5090.sh":          true,
		"workspace-fingerprint.sh":      true,
		"verify-mirror-fingerprint.sh":  true,
		"verify-mirror-fingerprint.ps1": false,
	}
	scripts := []string{"sync-lane-to-5090.sh", "workspace-fingerprint.sh", "verify-mirror-fingerprint.sh", "verify-mirror-fingerprint.ps1"}
	var missing, unexec []string
	for _, name := range scripts {
		p := filepath.Join(home, ".claudego", name)
		fi, err := os.Stat(p)
		if err != nil {
			missing = append(missing, p)
			continue
		}
		if needExec[name] && fi.Mode()&0o111 == 0 {
			unexec = append(unexec, p)
		}
	}
	if len(missing) == 0 && len(unexec) == 0 {
		return
	}
	msg := ""
	if len(missing) > 0 {
		msg += "缺失:" + strings.Join(missing, ", ") + ";"
	}
	if len(unexec) > 0 {
		msg += "无执行权限:" + strings.Join(unexec, ", ") + ";"
	}
	msg += "修法:确保 ~/.claudego 装机脚本齐备且 chmod +x"
	if requireEnv {
		t.Fatal(msg)
	}
	// 未设 env=1 只作提示,不 fail(异环境如未装机 CI 不吵)。
	t.Log("提示(非致命):" + msg + " —— 要目标环境显式红请设 CLAUDEGO_REQUIRE_SYNC_SCRIPTS=1")
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

// readFingerprint 解析 .claudego-fingerprint 新格式(3 行 header KV + `---` + per-file manifest)。
// 【R2】只返回 header KV。manifest 明文用 readFingerprintManifest 单独取。
// 遇 `---` 分隔行即停,避免 manifest 行被误当 KV。
func readFingerprint(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fingerprint %s: %v", path, err)
	}
	kv := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "---" {
			break
		}
		i := strings.Index(line, "=")
		if i <= 0 {
			continue
		}
		kv[line[:i]] = line[i+1:]
	}
	return kv
}

// readFingerprintManifest 取 `.claudego-fingerprint` 中 `---` 之后的每行(<hash>\t<path>)。
// 供 P1-2 用例断言"文件名/内容真正进入 manifest",不能被过滤/引号化跳过。
func readFingerprintManifest(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fingerprint %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	sep := -1
	for i, l := range lines {
		if l == "---" {
			sep = i
			break
		}
	}
	if sep < 0 {
		t.Fatalf(".claudego-fingerprint 缺 `---` 分隔符(旧版格式?):%s", path)
	}
	return lines[sep+1:]
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

	// 【R2】fingerprint 新格式:HEAD + COUNT + MANIFEST_SHA (无 DIRTY_/UNTRACKED_ split)。
	fp := readFingerprint(t, filepath.Join(m, ".claudego-fingerprint"))
	for _, k := range []string{"HEAD", "COUNT", "MANIFEST_SHA"} {
		if fp[k] == "" {
			t.Fatalf(".claudego-fingerprint 缺字段 %s\n实际:%+v", k, fp)
		}
	}
	// 联合集 COUNT=2:subdir/a.txt(dirty) + untr.txt(untracked);tmp/skipme.bin gitignored 不进。
	if fp["COUNT"] != "2" {
		t.Fatalf("COUNT 应为 2(dirty a.txt + untracked untr.txt), got %s", fp["COUNT"])
	}
	// manifest 逐行有内容哈希,断言两文件都进 manifest。
	manifest := readFingerprintManifest(t, filepath.Join(m, ".claudego-fingerprint"))
	haveA, haveU := false, false
	for _, ln := range manifest {
		if strings.Contains(ln, "\tsubdir/a.txt") {
			haveA = true
		}
		if strings.Contains(ln, "\tuntr.txt") {
			haveU = true
		}
	}
	if !haveA || !haveU {
		t.Fatalf("manifest 未覆盖两文件:haveA=%v haveU=%v\n%v", haveA, haveU, manifest)
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

	// 【R2】联合 MANIFEST_SHA 一个字段兜住 dirty/untracked 任一变化。
	if fp1["MANIFEST_SHA"] == fp2["MANIFEST_SHA"] {
		t.Fatal("dirty 或 untracked 变了,MANIFEST_SHA 应变化(否则说明重跑 sync 用了缓存旧内容)")
	}
	if fp1["COUNT"] == fp2["COUNT"] {
		t.Fatalf("新增 untracked 后 COUNT 应变化,got fp1=%s fp2=%s", fp1["COUNT"], fp2["COUNT"])
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

// ⑥ 【R1 P0-2 反例】macOS Finder 伪影 .DS_Store 在源仓 → sync 必须过滤,fingerprint 与镜像两侧对称。
// 上一轮复审报根因:UNTRACKED_COUNT=1(实际 Mac 源含 .DS_Store),mirror 侧现场重算 3
// (.DS_Store + ._.DS_Store + .claudego-fingerprint),两侧永不相等。
// 【杀的突变】把 sync/workspace-fingerprint 里对 .DS_Store 的过滤删掉 → 本用例红:UNTRACKED_COUNT 变化 / mirror 内出 .DS_Store。
func TestSyncFiltersDSStoreAndMirrorSideSymmetric(t *testing.T) {
	src := mkSyncSourceRepo(t)
	// 源仓塞一个 macOS Finder 伪影(现实场景就是这么产生的)。
	if err := os.WriteFile(filepath.Join(src, ".DS_Store"), []byte("finder-metadata\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "subdir", ".DS_Store"), []byte("subdir-finder\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mroot := t.TempDir()
	if _, out, code := runSyncLocal(t, src, mroot); code != 0 {
		t.Fatalf("sync 应成功, got exit=%d\n%s", code, out)
	}
	m := mirrorDir(t, src, mroot)

	// (a) 镜像根/子目录都不应含 .DS_Store(sync 层就过滤了)。
	if _, err := os.Stat(filepath.Join(m, ".DS_Store")); !os.IsNotExist(err) {
		t.Fatalf("镜像不应含 .DS_Store(sync 应过滤),stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(m, "subdir", ".DS_Store")); !os.IsNotExist(err) {
		t.Fatalf("镜像不应含 subdir/.DS_Store, stat err=%v", err)
	}

	// (b) 【R2】联合 COUNT=2(dirty subdir/a.txt + untracked untr.txt;.DS_Store 被两侧对称过滤)。
	fp := readFingerprint(t, filepath.Join(m, ".claudego-fingerprint"))
	if fp["COUNT"] != "2" {
		t.Fatalf("COUNT 应为 2(过滤 .DS_Store 后:dirty a.txt + untracked untr.txt), got %s\n完整 fp:%+v", fp["COUNT"], fp)
	}

	// (c) verify 通过(两侧对称)——若过滤不对称必红。
	if code := runVerify(t, m); code != 0 {
		t.Fatalf("对称过滤后 verify 应通过, got exit=%d", code)
	}
}

// ⑦ 【R1 P0-2 反例】AppleDouble ._<name> 伪影出现在镜像后,verify 必须仍通过(fingerprint 对称过滤兜住)。
// 现场场景:Windows tar.exe 解包 xattr → 生成 ._foo 实体文件。fingerprint 若不过滤 ._*,verify 报"过期"空转。
// 【杀的突变】删掉 workspace-fingerprint 里 ._ 分支 → 本用例红:verify exit 1(镜像 ._ 计数≠源 0)。
func TestVerifyToleratesAppleDoubleArtifactsOnMirror(t *testing.T) {
	src := mkSyncSourceRepo(t)
	mroot := t.TempDir()
	if _, _, code := runSyncLocal(t, src, mroot); code != 0 {
		t.Fatalf("sync 应成功, got exit=%d", code)
	}
	m := mirrorDir(t, src, mroot)

	// 模拟 Win tar 解包 xattr → 生成 AppleDouble 伪影(Mac 源没有,只出现在镜像)。
	// 这里手工往镜像塞两个 ._ 文件,断言 fingerprint 侧的过滤能吸收这类伪影。
	if err := os.WriteFile(filepath.Join(m, "._untr.txt"), []byte("appledouble\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m, "subdir", "._a.txt"), []byte("appledouble-sub\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// verify 必须仍通过——若报 exit 1,说明 workspace-fingerprint.sh 未对称过滤 ._*,
	// 复审就会误报"镜像过期"空转对不上现场。
	if code := runVerify(t, m); code != 0 {
		t.Fatalf("AppleDouble 伪影在镜像上时 verify 仍应通过(过滤对称), got exit=%d", code)
	}
}

// ⑧ 【R1 P0-1 反例】护栏脚本必须被 sync 分发到 <mirror>/.claudego-scripts/,复审沙箱允许目录内直连。
// 上一轮 CG-R2 复审 exit 3(环境缺陷)根因就是脚本在远端 ~/.claudego 不可达。
// 【杀的突变】删掉 sync-lane-to-5090.sh 里的 mkdir/cp 段 → 本用例红:in-mirror 脚本缺失。
func TestSyncBundlesGuardScriptsIntoMirror(t *testing.T) {
	src := mkSyncSourceRepo(t)
	mroot := t.TempDir()
	if _, _, code := runSyncLocal(t, src, mroot); code != 0 {
		t.Fatalf("sync 应成功, got exit=%d", code)
	}
	m := mirrorDir(t, src, mroot)

	// .sh 系需可执行位;.ps1 只需存在(PowerShell 靠扩展名调用,Windows 沙箱兜底通道)。
	needExec := map[string]bool{
		"workspace-fingerprint.sh":      true,
		"verify-mirror-fingerprint.sh":  true,
		"verify-mirror-fingerprint.ps1": false,
	}
	for _, name := range []string{"workspace-fingerprint.sh", "verify-mirror-fingerprint.sh", "verify-mirror-fingerprint.ps1"} {
		p := filepath.Join(m, ".claudego-scripts", name)
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("in-mirror 脚本 %s 应存在(复审沙箱允许目录直连), err=%v", p, err)
		}
		if needExec[name] && fi.Mode()&0o111 == 0 {
			t.Fatalf("in-mirror 脚本 %s 应可执行(chmod +x), 模式=%v", p, fi.Mode())
		}
	}

	// 且这些落盘产物不能反噬 fingerprint:.claudego-scripts/ 必须被 workspace-fingerprint.sh 过滤。
	// 【R2】联合 COUNT=2(dirty a.txt + untracked untr.txt),.claudego-scripts/* 被对称过滤。
	fp := readFingerprint(t, filepath.Join(m, ".claudego-fingerprint"))
	if fp["COUNT"] != "2" {
		t.Fatalf("落分发脚本不应扭曲 COUNT,应仍为 2(dirty a.txt + untracked untr.txt), got %s", fp["COUNT"])
	}
	if code := runVerify(t, m); code != 0 {
		t.Fatalf("落分发脚本后 verify 应通过(过滤 .claudego-scripts/), got exit=%d", code)
	}
}

// ⑨ 【R1 P0-1 反例】verify-mirror-fingerprint.sh 应优先用 in-mirror workspace-fingerprint.sh,
// 而非硬绑 $HOME/.claudego(远端沙箱不可达)。做法:切一个假 $HOME,断言 verify 仍能跑通(靠 in-mirror)。
// 【杀的突变】把 verify 里"首选 in-mirror"逻辑改回"只看 $HOME"→ 本用例红:环境缺陷 exit 3。
func TestVerifyPrefersInMirrorFingerprintScript(t *testing.T) {
	src := mkSyncSourceRepo(t)
	mroot := t.TempDir()
	if _, _, code := runSyncLocal(t, src, mroot); code != 0 {
		t.Fatalf("sync 应成功, got exit=%d", code)
	}
	m := mirrorDir(t, src, mroot)
	scripts := syncScriptsRoot(t)

	// 关键:切一个假 HOME(无 workspace-fingerprint.sh),模拟远端 Win 侧 ~/.claudego 缺失。
	fakeHome := t.TempDir()
	cmd := exec.Command("bash", filepath.Join(scripts, "verify-mirror-fingerprint.sh"), m)
	cmd.Env = []string{
		"HOME=" + fakeHome,
		"PATH=" + os.Getenv("PATH"),
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("HOME=%s(空)时 verify 仍应通过(靠 in-mirror 脚本), 实际 exit=%d\n输出:%s", fakeHome, ee.ExitCode(), buf.String())
		}
		t.Fatalf("verify 非 ExitError 失败:%v\n输出:%s", err, buf.String())
	}
	// exit 0 = 通过(证 in-mirror 路径起作用)
}

// ⑩ 【R1 P0-1 反例】in-mirror 脚本缺失 & $HOME 也无脚本 → verify 必须报 exit 3(环境缺陷)。
// 【杀的突变】verify 若在此情形下 exit 0/默认放行 → 本用例红:护栏对"两条路径都失守"未拦。
func TestVerifyExitsEnvErrorWhenBothScriptPathsMissing(t *testing.T) {
	src := mkSyncSourceRepo(t)
	mroot := t.TempDir()
	if _, _, code := runSyncLocal(t, src, mroot); code != 0 {
		t.Fatalf("sync 应成功, got exit=%d", code)
	}
	m := mirrorDir(t, src, mroot)
	scripts := syncScriptsRoot(t)

	// 破坏 in-mirror 脚本(模拟分发链路断)。
	if err := os.Remove(filepath.Join(m, ".claudego-scripts", "workspace-fingerprint.sh")); err != nil {
		t.Fatal(err)
	}
	// 假 HOME(无 workspace-fingerprint.sh),模拟远端 Win 侧 ~/.claudego 也没装机。
	fakeHome := t.TempDir()

	cmd := exec.Command("bash", filepath.Join(scripts, "verify-mirror-fingerprint.sh"), m)
	cmd.Env = []string{
		"HOME=" + fakeHome,
		"PATH=" + os.Getenv("PATH"),
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	if err == nil {
		t.Fatalf("两条脚本路径均缺失时 verify 必须非零退出(env 缺陷), 实际 exit 0\n输出:%s", buf.String())
	}
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("verify 非 ExitError 失败:%v\n输出:%s", err, buf.String())
	}
	if ee.ExitCode() != 3 {
		t.Fatalf("两条脚本路径均缺失应 exit 3(环境缺陷), got %d\n输出:%s", ee.ExitCode(), buf.String())
	}
}

// ⑪ 【R2 P1-1 反例】staged 未提交新文件必须不触发假"镜像过期"。
//
// 场景:修复卡纪律恰是"不 commit、workspace 待复审"。修复者 git add 一个新文件但不 commit,
// sync 送 tracked 未提交面 → 镜像 git apply 落文件到 worktree 但不动 index → 镜像重算时
// 该文件在 Mac 侧计入 dirty、镜像侧掉进 untracked。
//
// 【R1 版 bug】DIRTY_COUNT 和 UNTRACKED_COUNT 两侧同时不等 → verify exit 1 无辜停卡。
// 【R2 修法】联合成单 manifest,path→内容哈希不分面 → 两侧算出同一 manifest → verify pass。
//
// 【杀的突变】把 workspace-fingerprint.sh 里的联合改回 dirty/untracked 分开落 → 本用例红。
func TestVerifyPassesWithStagedButNotCommittedFile(t *testing.T) {
	src := mkSyncSourceRepo(t)
	// 【核心 setup】git add 一个新文件但不 commit(修复卡守则)。
	if err := os.WriteFile(filepath.Join(src, "staged_new.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", src, "add", "staged_new.go").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	// 状态断言:Mac 端 git 视 staged_new.go 为 dirty(在 diff HEAD 中)、非 untracked。
	statusOut, _ := exec.Command("git", "-C", src, "status", "--porcelain").CombinedOutput()
	if !strings.Contains(string(statusOut), "A  staged_new.go") {
		t.Fatalf("前置:Mac 端 staged_new.go 应为 staged-add 状态, got status=%q", statusOut)
	}

	mroot := t.TempDir()
	if out, errOut, code := runSyncLocal(t, src, mroot); code != 0 {
		t.Fatalf("sync 应成功, exit=%d\nstdout:%s\nstderr:%s", code, out, errOut)
	}
	m := mirrorDir(t, src, mroot)

	// 关键状态断言:镜像上 staged_new.go 是 untracked(git apply 不动 index)。
	mirrorStatus, _ := exec.Command("git", "-C", m, "status", "--porcelain").CombinedOutput()
	if !strings.Contains(string(mirrorStatus), "?? staged_new.go") {
		t.Fatalf("前置:镜像端 staged_new.go 应为 untracked, got status=%q", mirrorStatus)
	}

	// 【关键结论】即使两侧分类不同,verify 必须通过——统一联合集消灭 P1-1 类 bug。
	if code := runVerify(t, m); code != 0 {
		t.Fatalf("staged 未提交文件 verify 必须 exit 0(联合 manifest 消灭 split-plane bug), got %d\n"+
			"若为 1 说明 fingerprint 又分 dirty/untracked → P1-1 复发", code)
	}

	// manifest 必含 staged_new.go 且哈希是文件内容 sha256(不是 DELETED)。
	manifest := readFingerprintManifest(t, filepath.Join(m, ".claudego-fingerprint"))
	found := false
	for _, ln := range manifest {
		if strings.HasSuffix(ln, "\tstaged_new.go") {
			if strings.HasPrefix(ln, "DELETED\t") {
				t.Fatalf("staged_new.go 不应记为 DELETED\t: %q", ln)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("manifest 应含 staged_new.go, got:\n%s", strings.Join(manifest, "\n"))
	}
}

// ⑫ 【R2 P1-2 反例】非 ASCII(中文)文件名必须真实进入 manifest,不能被 core.quotepath 引号化跳过。
//
// 场景:项目文档以中文为主,现实概率高。旧版 git 默认 core.quotepath=true 会把中文路径变成
// `"docs/\346\226\207..."` → [ -e "$f" ] 必假 → 两侧对称记 DELETED → 镜像该文件被篡改/漏传
// 时 verify 仍 exit 0(fail-open 盲区)。
//
// 【R2 修法】所有 git 调用加 -c core.quotepath=false。本用例断言:
//   (a) verify 通过(路径可达 → 内容哈希入 manifest)
//   (b) manifest 明文含中文路径(证 -c core.quotepath=false 生效)
//   (c) manifest 该行不是 DELETED\t(证 [ -e ] 真找到文件了)
//   (d) 镜像端改动该文件内容后 verify 必须 exit 1(证防线可捕获改动,不是形式过滤)
//
// 【杀的突变】① 删掉 workspace-fingerprint.sh 里 -c core.quotepath=false → 本用例 (b) 红:
// manifest 里出现八进制引号;② 或让 filter 忽略中文 → (c) 变 DELETED;
// ③ 或 workspace-fingerprint 只按名不按内容判 → 篡改后仍算通过 → (d) 红。
func TestVerifyPassesWithNonAsciiFilenameAndDetectsMirrorTamper(t *testing.T) {
	src := mkSyncSourceRepo(t)
	// 中文文件名的 dirty 面(改已提交文件,类型偏一致):塞进 docs 子目录。
	if err := os.MkdirAll(filepath.Join(src, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	cnPath := filepath.Join("docs", "文档-中.md")
	// 先提交一个 baseline,再改动 → 现实场景(修复中改文档,不 commit)。
	if err := os.WriteFile(filepath.Join(src, cnPath), []byte("# 原稿\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", src, "add", cnPath).CombinedOutput(); err != nil {
		t.Fatalf("git add cn: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", src, "commit", "-q", "-m", "add cn doc").CombinedOutput(); err != nil {
		t.Fatalf("git commit cn: %v\n%s", err, out)
	}
	// 现在改中文文件(dirty tracked 面)。
	if err := os.WriteFile(filepath.Join(src, cnPath), []byte("# 原稿\n新增内容\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 另加一个中文 untracked 面。
	cnUntr := filepath.Join("docs", "笔记-新.md")
	if err := os.WriteFile(filepath.Join(src, cnUntr), []byte("笔记内容\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mroot := t.TempDir()
	if out, errOut, code := runSyncLocal(t, src, mroot); code != 0 {
		t.Fatalf("sync 应成功, exit=%d\nstdout:%s\nstderr:%s", code, out, errOut)
	}
	m := mirrorDir(t, src, mroot)

	// (a) verify 通过(路径可达 + 内容哈希入 manifest)。
	if code := runVerify(t, m); code != 0 {
		t.Fatalf("中文路径 verify 应通过, got exit=%d", code)
	}
	// (b) manifest 明文含中文路径,不含八进制转义。
	manifest := readFingerprintManifest(t, filepath.Join(m, ".claudego-fingerprint"))
	joinedManifest := strings.Join(manifest, "\n")
	if !strings.Contains(joinedManifest, "docs/文档-中.md") {
		t.Fatalf("manifest 应含真实 UTF-8 中文路径 docs/文档-中.md, got:\n%s", joinedManifest)
	}
	if !strings.Contains(joinedManifest, "docs/笔记-新.md") {
		t.Fatalf("manifest 应含真实 UTF-8 中文路径 docs/笔记-新.md, got:\n%s", joinedManifest)
	}
	if strings.Contains(joinedManifest, "\\346") || strings.Contains(joinedManifest, "\\344") {
		t.Fatalf("manifest 不应含 core.quotepath 八进制转义(说明 -c core.quotepath=false 未生效), got:\n%s", joinedManifest)
	}
	// (c) 中文文件 manifest 行不是 DELETED(证 [ -e ] 真找到内容)。
	for _, ln := range manifest {
		if strings.HasSuffix(ln, "\tdocs/文档-中.md") || strings.HasSuffix(ln, "\tdocs/笔记-新.md") {
			if strings.HasPrefix(ln, "DELETED\t") {
				t.Fatalf("中文文件 manifest 行不应是 DELETED\t(引号化盲区): %q", ln)
			}
		}
	}
	// (d) 镜像端手工篡改中文文件内容 → verify 必须 exit 1(证内容防线真起作用)。
	if err := os.WriteFile(filepath.Join(m, "docs", "文档-中.md"), []byte("# 原稿\n篡改内容\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runVerify(t, m); code != 1 {
		t.Fatalf("镜像端中文文件被篡改后 verify 必须 exit 1(内容不一致), got %d——若为 0 说明 fingerprint 未检测到中文文件的实际内容改动 = P1-2 fail-open 盲区复发", code)
	}
}

// ⑬ 【R2 P0-1 反例·分发】.ps1 版本必须随 sync 落进 <mirror>/.claudego-scripts/,
// 且是良构 PowerShell 脚本(至少含 param、Get-FileHash 用法,不是空文件)。
//
// 【杀的突变】① sync-lane-to-5090.sh 里删掉 .ps1 cp 段 → 文件不存在,红;
// ② .ps1 变空文件 / 缺 param → 结构断言红。
func TestSyncDistributesPowerShellVerifyScript(t *testing.T) {
	src := mkSyncSourceRepo(t)
	mroot := t.TempDir()
	if _, _, code := runSyncLocal(t, src, mroot); code != 0 {
		t.Fatalf("sync 应成功, got exit=%d", code)
	}
	m := mirrorDir(t, src, mroot)

	ps1 := filepath.Join(m, ".claudego-scripts", "verify-mirror-fingerprint.ps1")
	fi, err := os.Stat(ps1)
	if err != nil {
		t.Fatalf(".ps1 应随 sync 落到镜像内(Windows 沙箱 bash 不通时的兜底通道), stat err=%v", err)
	}
	if fi.Size() < 500 {
		t.Fatalf(".ps1 尺寸异常小 (%d bytes) —— 疑似空文件或占位符", fi.Size())
	}
	body, err := os.ReadFile(ps1)
	if err != nil {
		t.Fatal(err)
	}
	// PowerShell 良构基线:必须能解析 Mirror 参数 + 用 Get-FileHash 计算逐文件哈希 + 有 exit 码路径。
	// 【CG-R2c·2026-07-25】先剥掉行首 # 注释行，在代码执行位做断言；
	// 防"注释里有相同字面 → 删真代码测试仍绿"恒真化（与 TestPowerShellVerifyScriptContainsR3Fixes 同类闭合）。
	var ps1CodeLines []string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		ps1CodeLines = append(ps1CodeLines, line)
	}
	codeBody := strings.Join(ps1CodeLines, "\n")
	must := []string{
		"param(",                     // 参数定义
		"Mirror",                     // 契约参数名
		".claudego-fingerprint",      // 目标文件
		"Get-FileHash",               // 逐文件哈希(P0-1 无 sha256sum 通道的核心)
		"core.quotepath=false",       // P1-2 契约同源
		"exit 0",                     // 一致退出
		"exit 1",                     // 不一致退出
		"exit 2",                     // 缺失退出
		"exit 3",                     // 环境缺陷退出
	}
	for _, kw := range must {
		if !strings.Contains(codeBody, kw) {
			preview := string(body)
			if len(preview) > 400 {
				preview = preview[:400]
			}
			t.Fatalf(".ps1 代码执行位缺关键片段 %q(CG-R2c 剥注释断言,防注释满足恒真),body 前 400 字节:\n%s", kw, preview)
		}
	}
}

// ⑭ 【R2 P1-3 反例·仓内验收入口】仓内必须有 `make accept-sync` 目标,把
// CLAUDEGO_REQUIRE_SYNC_SCRIPTS=1 硬编码到验收路径 —— 否则 Skip 静默漂绿在缺省路径原样存在。
//
// 【R1 遗漏】env=1 只是"约定俗成设一下"，无仓内验收入口挂钩,收工汇报无法引用具体命令的实跑输出。
// 【R2 修法】Makefile 提供 accept-sync 目标,go test 假 Skip 无处遁形。
// 本用例断言 Makefile 含该目标及必需 env 硬编码。
//
// 【杀的突变】① 删掉 Makefile 里 accept-sync 段 → 本用例红;
// ② 该目标里去掉 CLAUDEGO_REQUIRE_SYNC_SCRIPTS=1 → 关键字缺失红。
func TestVerifyAcceptSyncMakefileTargetExists(t *testing.T) {
	// 定位仓根:测试文件位于 <repo>/reviewsync_workspace_test.go,cwd 就是 <repo>。
	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	mkPath := filepath.Join(repoRoot, "Makefile")
	body, err := os.ReadFile(mkPath)
	if err != nil {
		t.Fatalf("Makefile 应存在于仓根: %v", err)
	}
	// 【CG-R2b R3·2026-07-24 类闭合续】"非执行位满足护栏"病类第二级兄弟洞:
	// R2 已剥 `#` 注释,但 Makefile recipe 中还有一类"显示位":`@echo "..."` / `@printf "..."`——
	// 是消息展示指令(其参数是文本消息串,不是被断言的执行体如 `go test -run '...'`)。
	// 首证 t0724-2104-c2a0 P1-1 复审:accept-sync:26 的 @echo 展示行字面含 "Sync|Verify|DesignReview",
	// 仅漂 :27 exec 行的 pattern → codeBody 里 "DesignReview" 依然由 :26 @echo 提供 → 突变逃逸。
	// 系统性修法:除 `#` 起头的注释行外,同步剔除 recipe 显示行(命令位为 echo/printf,@ 前缀可选)。
	// 类闭合命令位识别:剥掉 tab 与可选 `@` 后,首 token 为 echo/printf 即视为显示行。
	var codeLines []string
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		// Makefile recipe 显示行剔除:剥 `@` 前缀后,首命令位是 echo/printf → 展示指令。
		cmd := strings.TrimLeft(trimmed, "@")
		if strings.HasPrefix(cmd, "echo ") || strings.HasPrefix(cmd, "printf ") ||
			cmd == "echo" || cmd == "printf" {
			continue
		}
		codeLines = append(codeLines, line)
	}
	codeBody := strings.Join(codeLines, "\n")

	must := []string{
		"accept-sync:",                    // 目标声明
		"CLAUDEGO_REQUIRE_SYNC_SCRIPTS=1", // 硬编码 env
		"go test",                         // 触发验收测试
		// 【CG-R2b R2·2026-07-24】pattern 必须覆盖 DesignReview 类装机模板断言;
		// 首证 t0724-2033-0a20 P1-1:pattern 仅 'Sync|Verify' 时
		// TestDesignReviewAndFixCycleTemplatesEmbedContractContent 的 (c) 段装机副本红线
		// 脱离验收流,同 ⑭(R2 P1-3)"env 门无入口挂钩"病类。从 codeBody 里删掉 "DesignReview" 立即红。
		"DesignReview",
	}
	for _, kw := range must {
		if !strings.Contains(codeBody, kw) {
			t.Fatalf("Makefile 代码执行位缺关键片段 %q(注释里的字面提及不算,CG-R2b R2 类闭合) —— accept-sync 目标未接线, codeBody:\n%s", kw, codeBody)
		}
	}
}

// ⑯ 【CG-R2b R2·2026-07-24 / R3·2026-07-24 类闭合升级】accept-sync 的 -run pattern 必须
// 覆盖所有 env=1-gated 测试名;且 Makefile 中所有 `-run '...'` 出现位 pattern 必须彼此一致
// (消灭"@echo 展示行 pattern 与 exec 执行行 pattern 分裂"的 R3 兄弟洞)。
//
// 【为什么单独立测】TestVerifyAcceptSyncMakefileTargetExists 只锁"pattern 里含 DesignReview 字面",
// 未来若新增第 4/5 个 env=1-gated 测试(名字不带 Sync/Verify/DesignReview),仍会静默漂绿
// (复刻本轮 P1-1 病类)。本挡动态枚举文件里所有 Test* 函数,只要函数体或其传递依赖的助手
// (syncScriptsRoot/runSyncLocal/runVerify)引用了 CLAUDEGO_REQUIRE_SYNC_SCRIPTS,就必须能被
// Makefile 的 -run pattern 匹上;匹不上 → 红。加装机测试却忘同步 pattern → 本挡红。
//
// 【R3 兄弟洞闭合】旧版 FindStringSubmatch 只取第一处出现位——若 accept-sync:26 是 @echo 展示行
// 先出现,:27 是真 exec 行后出现,只漂 :27 pattern → 首位匹到的仍是 :26 的旧串,本挡逃逸(首证
// t0724-2104-c2a0 P1-1)。修法:FindAllStringSubmatch 全量提取,断言所有出现位 pattern 一致;
// 分裂即红。逻辑上把 @echo 展示行 pattern 与 exec 行 pattern 锁死同源,任一侧漂另一侧未同步必红。
//
// 【类闭合枚举】既有 env=1-gated 测试(书面盘点,用于 sanity 检查):
//   - Test*Sync* / Test*Verify* 系(经由 syncScriptsRoot/runSyncLocal/runVerify);
//   - TestSyncScriptsInstalled(自读 env);
//   - TestDesignReviewAndFixCycleTemplatesEmbedContractContent(自读 env,本轮首证)。
//
// 【杀的突变】
//   ① 把 Makefile pattern 改回 'Sync|Verify' → TestDesignReviewAnd... 匹不上 → 本挡红;
//   ② 新增一个 TestPrepFooEnvGated 但忘同步 pattern → 本挡红(pattern 覆盖度不足);
//   ③ 只漂 exec 行 pattern(:27)不动 @echo 展示行(:26) → 两处不一致 → 本挡红(R3 兄弟洞闭合)。
func TestAcceptSyncMakefilePatternCoversAllEnvGatedTests(t *testing.T) {
	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	// 从 Makefile 提出 -run 后面的 pattern 字符串(单引号包裹)。
	mkBody, err := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	// 【R3 类闭合】-run '...' 全量提取——不只取第一处,消灭 @echo 展示行与 exec 行 pattern 分裂的兄弟洞。
	runRe := regexp.MustCompile(`-run '([^']+)'`)
	ms := runRe.FindAllStringSubmatch(string(mkBody), -1)
	if len(ms) == 0 {
		t.Fatalf("Makefile 里未找到 `-run '...'`(accept-sync 目标) —— body:\n%s", string(mkBody))
	}
	// 所有 -run 出现位 pattern 必须一致:分裂即红(@echo 展示与 exec 执行同源锁,R3 类闭合)。
	firstPat := ms[0][1]
	for i, m := range ms {
		if m[1] != firstPat {
			t.Fatalf("Makefile 出现多个不一致的 -run pattern(@echo 展示行与 exec 执行行 pattern 分裂):\n"+
				"  第 1 处=%q\n  第 %d 处=%q\n"+
				"漂移复刻:t0724-2104-c2a0 P1-1(只漂 exec 行 pattern 不漂 @echo 展示行则 codeBody\n"+
				"positive 断言与 FindString 首匹配双双逃逸)。修法:所有 -run 出现位两侧同步改。",
				firstPat, i+1, m[1])
		}
	}
	pattern := firstPat
	patRe, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("Makefile -run pattern %q 编译为正则失败: %v", pattern, err)
	}

	// 读本测试文件,枚举所有 `func Test*(t *testing.T)` 函数;
	// 函数体从 `func TestX(` 一直到下一个 `\nfunc `(或文件尾)。
	self, err := os.ReadFile(filepath.Join(repoRoot, "reviewsync_workspace_test.go"))
	if err != nil {
		t.Fatalf("read self test file: %v", err)
	}
	src := string(self)

	// 已知 env=1-gated 助手(引用 CLAUDEGO_REQUIRE_SYNC_SCRIPTS,或传递依赖它的 helper):
	// syncScriptsRoot 直接读 env;runSyncLocal 与 runVerify 传递调用 syncScriptsRoot。
	envGatedHelpers := []string{
		"CLAUDEGO_REQUIRE_SYNC_SCRIPTS",
		"syncScriptsRoot(",
		"runSyncLocal(",
		"runVerify(",
	}

	funcRe := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(t \*testing\.T\) \{`)
	locs := funcRe.FindAllStringSubmatchIndex(src, -1)
	if len(locs) == 0 {
		t.Fatalf("枚举 Test* 函数为空 —— 正则漂?")
	}
	// 追加一个哨兵位置,方便算最后一个函数的 end。
	ends := make([]int, len(locs))
	for i := 0; i < len(locs); i++ {
		if i+1 < len(locs) {
			ends[i] = locs[i+1][0]
		} else {
			ends[i] = len(src)
		}
	}
	var envGated []string
	for i, loc := range locs {
		name := src[loc[2]:loc[3]]
		body := src[loc[0]:ends[i]]
		hit := false
		for _, h := range envGatedHelpers {
			if strings.Contains(body, h) {
				hit = true
				break
			}
		}
		if hit {
			envGated = append(envGated, name)
		}
	}
	if len(envGated) == 0 {
		t.Fatalf("未枚举出任何 env=1-gated 测试 —— 助手清单或函数正则失效")
	}
	// sanity: 至少含本轮首证的 3 个已知点。
	sanity := []string{"TestSyncScriptsInstalled", "TestDesignReviewAndFixCycleTemplatesEmbedContractContent"}
	for _, s := range sanity {
		found := false
		for _, n := range envGated {
			if n == s {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("env=1-gated 枚举缺已知测试 %s —— 助手识别正则漂;实际枚举=%v", s, envGated)
		}
	}
	// 逐个断言 pattern 匹上。
	var uncovered []string
	for _, name := range envGated {
		if !patRe.MatchString(name) {
			uncovered = append(uncovered, name)
		}
	}
	if len(uncovered) > 0 {
		t.Fatalf("Makefile accept-sync 的 -run pattern %q 未覆盖 env=1-gated 测试 %v —— 装机漂移红线脱离验收流(P1-1 类闭合);修法:把测试名或名子串补进 pattern",
			pattern, uncovered)
	}
}

// ⑱ 【CG-R2b R3·2026-07-24 类闭合】accept-sync 目标的 exec 执行行本身必须携带完整 -run pattern——
// "非执行位满足护栏"病类的正面锚定挡:直接锁定"tab-indented 且非 @echo/@printf 展示位"的
// 那一行,断言它自带 CLAUDEGO_REQUIRE_SYNC_SCRIPTS=1 与 -run '<全 pattern>' 的完整表达。
//
// 【为什么再加一挡】⑯ 靠"全量提取 -run 出现位并断言一致"消灭分裂,但若把 exec 行整段 -run 拆走
// (只留 @echo 展示行) → 全量提取只剩 @echo 里那一处 → 一致性成立 → coverage 检查仍绿 → 逃逸。
// 本挡直接锚定"exec 执行行自身要携带 -run 完整 pattern",消灭"exec 行整体缺失 -run"的极端漂移。
//
// 【exec 行识别规则】
//   - Makefile recipe 行必须以 tab 起头;
//   - 剥 tab + 可选 `@` 后,首命令 token 若为 echo/printf,是展示位,跳过;
//   - 剩下的 recipe 行,若含 CLAUDEGO_REQUIRE_SYNC_SCRIPTS=1 与 `go test`,即为 exec 执行行;
//   - 该行必须整体含 `-run '<pattern>'` 且 pattern 与全量首匹配一致。
//
// 【杀的突变】
//   ① 从 :27 exec 行删掉 `-run 'Sync|Verify|DesignReview'` 整段 → exec 行不携带 -run → 本挡红;
//   ② 只漂 :27 exec 行 pattern 不漂 :26 @echo 展示行 → exec 行 pattern 与全量首匹配不一致 → 本挡红
//      (与 ⑯ 双杀:⑯ 从"全量一致"角度报,本挡从"exec 行本身要与全量首匹配一致"角度报);
//   ③ 把 exec 行 recipe 命令换成 @echo(伪装成显示) → 找不到 exec 行 → 本挡红。
func TestAcceptSyncExecutionLineCarriesRunPattern(t *testing.T) {
	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	mkBody, err := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	runRe := regexp.MustCompile(`-run '([^']+)'`)
	msAll := runRe.FindAllStringSubmatch(string(mkBody), -1)
	if len(msAll) == 0 {
		t.Fatalf("Makefile 里未找到 `-run '...'` —— accept-sync 目标可能被删")
	}
	primaryPat := msAll[0][1]

	// 识别 exec 执行行:tab 起头 + 剥 `@` 后首命令非 echo/printf + 含 env 变量 + 含 `go test`。
	var execLines []string
	for _, line := range strings.Split(string(mkBody), "\n") {
		if !strings.HasPrefix(line, "\t") {
			continue // 非 recipe 行
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue // recipe 内注释
		}
		cmd := strings.TrimLeft(trimmed, "@")
		if strings.HasPrefix(cmd, "echo ") || strings.HasPrefix(cmd, "printf ") ||
			cmd == "echo" || cmd == "printf" {
			continue // 展示位,跳过
		}
		if strings.Contains(trimmed, "CLAUDEGO_REQUIRE_SYNC_SCRIPTS=1") &&
			strings.Contains(trimmed, "go test") {
			execLines = append(execLines, trimmed)
		}
	}
	if len(execLines) == 0 {
		t.Fatalf("找不到 accept-sync 的 exec 执行行(tab-indented 非-@echo 且含 CLAUDEGO_REQUIRE_SYNC_SCRIPTS=1 与 go test) ——\n"+
			"可能 recipe 被拆走了或被伪装成 @echo 展示位;R3 类闭合逃逸场景③命中。Makefile body:\n%s", string(mkBody))
	}
	// exec 行必须自带完整 -run '<pattern>' 且 pattern 与全量首匹配一致。
	for _, line := range execLines {
		m := runRe.FindStringSubmatch(line)
		if len(m) < 2 {
			t.Fatalf("exec 执行行本身缺 `-run '...'` 表达(R3 类闭合逃逸场景①命中):\n  %s\n"+
				"修法:把 -run '<pattern>' 补回 exec 行;不允许仅在 @echo 展示行提及", line)
		}
		if m[1] != primaryPat {
			t.Fatalf("exec 执行行 -run pattern %q ≠ Makefile 全量首匹配 %q(R3 类闭合逃逸场景②命中):\n  exec 行:%s\n"+
				"漂移复刻:只漂 exec 行不漂 @echo 展示行的兄弟洞;修法:两侧同步改", m[1], primaryPat, line)
		}
	}
}

// ⑰ 【CG-R2b R2·2026-07-24】integration.sh 与模板渲染的字符级契约锁——类闭合。
//
// 【为什么】上一轮 CG-R2b R1 提交把 templates/design-review.md:3 `审核关注点：` 全角冒号
// 改为半角 `审核关注点:`,而 test/integration.sh:60 grep 目标是全角 `审核关注点：focus` →
// set -euo pipefail 下无匹配即静默 exit 1,截断 30+ 场景。本挡把"整数-sh grep 目标 <->
// template 里的原文"两侧字节同源锁死;任一侧漂移(改字/半全角化)必红。
//
// 【类闭合枚举】integration.sh 中对模板渲染文本的 grep 断言:
//   - line 60: `审核关注点：focus` <- templates/design-review.md 渲染 {{FOCUS}};
//   - line 512: `按类闭合` <- templates/fix-cycle.md `## 按类闭合(硬要求)` 章节。
//
// 【杀的突变】
//   ① templates/design-review.md 全角冒号改半角(本轮首证) → 本挡红;
//   ② test/integration.sh:60 grep 改半角(未同步) → 本挡红;
//   ③ templates/fix-cycle.md 删掉「按类闭合」章节 → 本挡红。
func TestIntegrationShTemplateRenderContract(t *testing.T) {
	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	read := func(rel string) string {
		body, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(body)
	}
	integration := read("test/integration.sh")
	contracts := []struct {
		name             string
		templatePath     string
		templateNeeds    string // 模板里必须出现的字节序列
		integrationNeeds string // integration.sh 里必须出现的字节序列(常见形式是 grep 参数)
	}{
		{
			name:             "design-review 审核关注点(全角冒号)<-> integration.sh:60 grep",
			templatePath:     "templates/design-review.md",
			templateNeeds:    "审核关注点：{{FOCUS}}",
			integrationNeeds: `"审核关注点：focus"`,
		},
		{
			name:             "fix-cycle 按类闭合章节 <-> integration.sh:512 grep",
			templatePath:     "templates/fix-cycle.md",
			templateNeeds:    "按类闭合",
			integrationNeeds: `"按类闭合"`,
		},
	}
	for _, c := range contracts {
		tpl := read(c.templatePath)
		if !strings.Contains(tpl, c.templateNeeds) {
			t.Errorf("[%s] %s 缺片段 %q —— 模板侧漂移(如半/全角化),integration.sh set -euo pipefail 下会静默 exit 1 截断",
				c.name, c.templatePath, c.templateNeeds)
		}
		if !strings.Contains(integration, c.integrationNeeds) {
			t.Errorf("[%s] test/integration.sh 缺 grep 目标 %q —— 集成侧漂移(改字未同步模板)",
				c.name, c.integrationNeeds)
		}
	}
}

// ⑮ 【R3 P0-1 + P1-1 结构断言】in-mirror .ps1 必须包含:
//   (a) System.StringComparer::Ordinal 排序原语(消灭 R2 版 Sort-Object -CaseSensitive 的语言学序假 STALE);
//   (b) System.Text.UTF8Encoding + [Console]::OutputEncoding + [Console]::InputEncoding 三处切换
//       (消灭 Windows OEM CP 下 UTF-8 中文路径乱码 → Test-Path 假 + 逐行不等的假 STALE);
//   (c) .claudego-fingerprint.files 伴生文件的 sha256 校验(TA-2 契约同源)。
//
// 【为什么单独立测】TestSyncDistributesPowerShellVerifyScript 已挡"分发/尺寸/param/Get-FileHash";
// 本用例挡的是 R3 修法字面片段本身,防".ps1 回滚到 R2 版仍能过分发测试"的漂移。
//
// 【杀的突变】① 把 SortedSet+Ordinal 改回 Sort-Object -CaseSensitive -Unique → (a) 红;
// ② 删掉 UTF-8 编码 setter 段 → (b) 红;
// ③ 删掉伴生文件 sha256 校验 → (c) 红。
func TestPowerShellVerifyScriptContainsR3Fixes(t *testing.T) {
	src := mkSyncSourceRepo(t)
	mroot := t.TempDir()
	if _, _, code := runSyncLocal(t, src, mroot); code != 0 {
		t.Fatalf("sync 应成功, got exit=%d", code)
	}
	m := mirrorDir(t, src, mroot)
	ps1 := filepath.Join(m, ".claudego-scripts", "verify-mirror-fingerprint.ps1")
	body, err := os.ReadFile(ps1)
	if err != nil {
		t.Fatalf(".ps1 应存在: %v", err)
	}

	// 【CG-R2b R1·2026-07-24 类闭合】护栏正向片段可能被 rationale 注释满足:
	// 首证是 `-cmatch` —— ps1:132 注释含字面 `-cmatch`,把 145 行 `$p -cmatch $pat`
	// 回滚为 `$p -match $pat` 后 strings.Contains(body,"-cmatch") 仍绿 → 突变逃逸。
	// 同类风险面(枚举):`SortedSet` / `[Console]::OutputEncoding` / `[Console]::InputEncoding` /
	// `.claudego-fingerprint.files` / `MANIFEST_SHA` 在 ps1 的 rationale 注释里都有字面提及,
	// 单独用 body 级 strings.Contains 都会被注释满足。系统性修法:先剥掉所有 `#` 起始的注释行,
	// 只在代码执行位的字节体 codeBody 上做正向断言 —— 一次闭合"注释可满足护栏"整个类。
	var codeLines []string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		codeLines = append(codeLines, line)
	}
	codeBody := strings.Join(codeLines, "\n")

	must := []string{
		"StringComparer]::Ordinal",    // P0-1: 字节序等同排序
		"SortedSet",                   // P0-1: 单遍排序+去重
		"System.Text.UTF8Encoding",    // P1-1: UTF-8 编码构造
		"[Console]::OutputEncoding",   // P1-1: stdout 通道编码
		"[Console]::InputEncoding",    // P1-1: stdin 通道编码
		".claudego-fingerprint.files", // TA-2: 伴生文件校验
		"MANIFEST_SHA",                // TA-2: 与 header 的 MANIFEST_SHA 交叉断言
		"$p -cmatch $pat",             // CG-R2b R1: 大小写敏感过滤;片段改代码唯一表达式(注释里只有裸 `-cmatch` 无 `$p ... $pat` 上下文)
	}
	for _, kw := range must {
		if !strings.Contains(codeBody, kw) {
			t.Fatalf(".ps1 代码执行位缺 R3 修法关键片段 %q(注释里的字面提及不算,CG-R2b R1 类闭合) —— 说明分发的是 R2 或更早版本(或修法漂),完整 body 长度=%d, codeBody 长度=%d", kw, len(body), len(codeBody))
		}
	}
	// 反面挡:R2 的 `| Sort-Object` 管道用法(语言学序)不应残留在代码路径。
	// 【注】.ps1 rationale 注释里为解释修法会用反引号引用 `Sort-Object -CaseSensitive -Unique` 字面串,
	// 那不是代码执行路径,不应误报;所以匹配"管道进入 Sort-Object"这一执行位模式(注释里的引用无 `| ` 前缀)。
	//
	// 【CG-R2b R1·2026-07-24】新增:大小写敏感对称的负面挡。
	// PowerShell `-match`/`-imatch` 默认不分大小写,与 sh 侧 `grep -Ev`(ERE,分大小写)不同源
	// → untracked 大小写变体(.ds_store / .CLAUDEGO-FINGERPRINT) Mac 侧计入 stored manifest,
	//   而 ps1 A 通道过滤时被 `-match` 误剔 → COUNT+逐行必不等 → 假 STALE 空转。
	// 【杀的突变】把过滤循环里 `$p -cmatch $pat` 回滚为 `$p -match $pat` 或 `$p -imatch $pat` → 本挡红。
	// 小写化后匹配,兼容 `-Match`/`-MATCH` 等大小写变体(PowerShell 操作符本身大小写不敏感)。
	// `-cmatch $pat` 首字符差异不会命中 `-match $pat`/`-imatch $pat` 子串(手工 offset 验证过),
	// 因此不会误报正确写法。
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "| Sort-Object") {
			t.Fatalf(".ps1 代码路径仍含 `| Sort-Object`(R2 版走 .NET 语言学序,与 sh 的 LC_ALL=C sort -u 不同源)—— 该行必须已换成 SortedSet+Ordinal;命中行:%q", trimmed)
		}
		lower := strings.ToLower(trimmed)
		if strings.Contains(lower, "-match $pat") {
			t.Fatalf(".ps1 代码路径含 `-match $pat`(PowerShell 默认不分大小写,与 sh 侧 `grep -Ev` 不同源 → 大小写变体假 STALE)—— 必须用 `-cmatch`;命中行:%q", trimmed)
		}
		if strings.Contains(lower, "-imatch $pat") {
			t.Fatalf(".ps1 代码路径含 `-imatch $pat`(强制不分大小写,与 sh 侧不同源)—— 必须用 `-cmatch`;命中行:%q", trimmed)
		}
	}
}

// ⑯ 【R3 P0-1 行为端到端·pwsh-if-available】混大小写 + 中文文件名的联合集,
// R2 版 .ps1 走 Sort-Object 语言学序,与 sh 侧字节序不同源 → verify 假 STALE(exit 1);
// R3 版 SortedSet+Ordinal 与字节序恒等 → verify 通过(exit 0)。
//
// 【纪律】pwsh/powershell 不在时 Skip(Mac 开发盒无 pwsh 是常态,不能红);
// 装机验收流水在 Windows 端会跑到本用例,构成 P0-1 的行为闸门。
//
// 【杀的突变】.ps1 排序回滚到语言学序 → 本用例 Windows 上会红(混大小写 manifest 首行不等)。
func TestVerifyPowerShellScriptOnMixedCaseAndChineseFixture(t *testing.T) {
	pwsh := findPowerShell()
	if pwsh == "" {
		t.Skip("跳过:pwsh/powershell 不在 PATH(Mac 开发盒常态,Windows 装机验收流水必须跑到本用例)")
	}
	src := mkSyncSourceRepo(t)
	// 混大小写 fixture:大写开头 + 小写开头交错。字节序('M'0x4D < 'a'0x61 < 'b'0x62 < 'z'0x7A)
	// 与 .NET 默认语言学序('a'~'A'~'M'~'b' 相近字母集群)不同源;R2 版会在此翻车。
	mixedNames := []string{"Makefile.notes", "README.local", "aaa.txt", "bbb.txt", "ZZZ.txt", "ccc.txt"}
	for _, n := range mixedNames {
		if err := os.WriteFile(filepath.Join(src, n), []byte("body-"+n+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 中文文件名也放一份(顺带覆盖 P1-1 UTF-8 编码切换在 pwsh 下起作用)。
	if err := os.WriteFile(filepath.Join(src, "文档-混.md"), []byte("# 混大小写\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mroot := t.TempDir()
	if out, errOut, code := runSyncLocal(t, src, mroot); code != 0 {
		t.Fatalf("sync 应成功, exit=%d\nstdout:%s\nstderr:%s", code, out, errOut)
	}
	m := mirrorDir(t, src, mroot)

	// (a) pwsh 侧 verify 必须 exit 0(证 SortedSet+Ordinal 与 sh 侧字节序同源)。
	code, output := runPowerShellVerify(t, pwsh, m)
	if code != 0 {
		t.Fatalf("混大小写 + 中文 fixture 下 pwsh verify 应 exit 0(SortedSet+Ordinal 与 sh 侧同源), got=%d\n输出:%s", code, output)
	}

	// (b) 篡改镜像上某文件内容后 pwsh 侧 verify 必须 exit 1(证内容防线真起作用,不是排序绕过)。
	if err := os.WriteFile(filepath.Join(m, "aaa.txt"), []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, output = runPowerShellVerify(t, pwsh, m)
	if code != 1 {
		t.Fatalf("篡改后 pwsh verify 必须 exit 1(内容不等), got=%d\n输出:%s", code, output)
	}
}

// findPowerShell 探测 pwsh 或 powershell 可执行文件;都无则返回空串。
// 【为什么两条】pwsh 是 PowerShell 7+ 的跨平台命令;powershell 是 Windows 5.1 内置;远端 Win 沙箱
// 至少有一个,Mac/Linux 开发盒通常俩都没有 → 用它做 Skip 门。
func findPowerShell() string {
	if p, err := exec.LookPath("pwsh"); err == nil {
		return p
	}
	if p, err := exec.LookPath("powershell"); err == nil {
		return p
	}
	return ""
}

// runPowerShellVerify 用探到的 pwsh 跑 in-mirror .ps1,返回 (exit code, 输出)。
func runPowerShellVerify(t *testing.T, pwsh, mirror string) (int, string) {
	t.Helper()
	ps1 := filepath.Join(mirror, ".claudego-scripts", "verify-mirror-fingerprint.ps1")
	cmd := exec.Command(pwsh, "-File", ps1, "-Mirror", mirror)
	cmd.Env = os.Environ()
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	if err == nil {
		return 0, buf.String()
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), buf.String()
	}
	t.Fatalf("pwsh verify 非 ExitError 失败:%v\n输出:%s", err, buf.String())
	return -1, buf.String()
}

// ⑰ 【R3 完整闭环·DELETED 分支端到端】HEAD 追踪的文件在源仓被 rm 后,workspace-fingerprint.sh
// 的 [ -e "$f" ] 假分支应发 `DELETED\t<path>` 行;sync 全量重建镜像 → 镜像也无该文件 →
// verify 侧 Test-Path 也假 → 同样 DELETED → 逐行比对通过。
//
// 【为什么单独立测】旧套件里 mkSyncSourceRepo 只覆盖 modified/untracked/gitignored/中文/staged
// 几类,DELETED 分支从未端到端跑通;万一 sync 侧漏落 rm 或 verify 侧 Test-Path 语义漂,现在会红。
//
// 【杀的突变】① workspace-fingerprint.sh 里 else 分支不发 DELETED 行 → 联合集 COUNT 少 1 → 红;
// ② sync 不 rm 镜像端已删的 tracked 文件 → 镜像该路径仍存在 → 一侧 DELETED / 一侧 sha → 首行不等 → 红。
func TestVerifyPassesWithDeletedTrackedFile(t *testing.T) {
	src := mkSyncSourceRepo(t)
	// 基线 sync 前把 baseline 追踪文件删掉(不 commit,仿"修复卡·workspace 待复审"守则)。
	if err := os.Remove(filepath.Join(src, "keep.txt")); err != nil {
		t.Fatal(err)
	}
	// git 侧状态:keep.txt 在 diff --name-only HEAD 里,不在 ls-files --others 里。
	statusOut, _ := exec.Command("git", "-C", src, "status", "--porcelain").CombinedOutput()
	if !strings.Contains(string(statusOut), " D keep.txt") {
		t.Fatalf("前置:源端 keep.txt 应为 deleted-tracked, got status=%q", statusOut)
	}

	mroot := t.TempDir()
	if out, errOut, code := runSyncLocal(t, src, mroot); code != 0 {
		t.Fatalf("sync 应成功, exit=%d\nstdout:%s\nstderr:%s", code, out, errOut)
	}
	m := mirrorDir(t, src, mroot)

	// (a) 镜像端 keep.txt 应已被 sync 抹除(全量重建 workspace 覆盖)。
	if _, err := os.Stat(filepath.Join(m, "keep.txt")); !os.IsNotExist(err) {
		t.Fatalf("镜像 keep.txt 应被 sync 抹除(仿源端 rm), stat err=%v", err)
	}

	// (b) manifest 含 `DELETED\tkeep.txt` 行。
	manifest := readFingerprintManifest(t, filepath.Join(m, ".claudego-fingerprint"))
	foundDeleted := false
	for _, ln := range manifest {
		if ln == "DELETED\tkeep.txt" {
			foundDeleted = true
			break
		}
	}
	if !foundDeleted {
		t.Fatalf("manifest 应含 `DELETED\\tkeep.txt` 行(workspace-fingerprint.sh else 分支),实际:\n%s", strings.Join(manifest, "\n"))
	}

	// (c) verify 通过(两侧同 DELETED)。
	if code := runVerify(t, m); code != 0 {
		t.Fatalf("DELETED 分支两侧对称,verify 应 exit 0, got=%d", code)
	}
}

// ⑱ 【CG-R2b R1·2026-07-24】模板 D 通道自门 + 修 1 剔除清单同源片段护栏。
//
// 【为什么】上一轮 CG-R2b 修 1 只落在 ~/.claudego/templates/design-review.md,
// 仓库内嵌源 templates/design-review.md 曾缺整个开工自门章节;templates.go:41-51
// loadTemplate 在数据目录模板缺失时静默回退内嵌版 → 新机装机 / ~/.claudego 副本被清
// → 自门连同修 1(剔除 .claudego-fingerprint.files 伴生文件)整体无声消失,假 STALE 病
// 类"闭合"随之蒸发。同类风险:fix-cycle.md 的"提交前机械门"章节曾同样只在装机侧,
// 修法失效同理。本用例把内嵌源 + 装机副本双侧断言,一次闭合"关键修法仅落装机侧"整个类。
//
// 【杀的突变】
//   ① 从 templates/design-review.md 里删掉开工自门章节 → 内嵌断言红;
//   ② 从 templates/design-review.md 里删掉 `.claudego-fingerprint(.files)?` 剔除行
//      → 内嵌断言红(CG-R2b 修 1 直击);
//   ③ 从 templates/fix-cycle.md 里删掉"提交前机械门"章节 → 内嵌断言红;
//   ④ 装机侧模板漂移到无自门/无机械门 → env=1 下装机断言红。
func TestDesignReviewAndFixCycleTemplatesEmbedContractContent(t *testing.T) {
	// (a) 内嵌 design-review.md 必含开工自门 + 修 1 剔除清单同源片段。
	//     用不存在的 root 让 loadTemplate 走 embedded 兜底路径。
	fakeRoot := filepath.Join(t.TempDir(), "no-such-root")
	drEmbedded, err := loadTemplate(fakeRoot, "design-review")
	if err != nil {
		t.Fatalf("loadTemplate design-review 内嵌兜底失败: %v", err)
	}
	drMustEmbedded := []string{
		"开工自门",                          // 章节头
		"verify-mirror-fingerprint",     // 自门主脚本引用
		".claudego-fingerprint(.files)?", // CG-R2b 修 1:剔除清单同源正则(伴生文件必包含)
		"D·原语拆解",                       // D 通道原语兜底章节标识
	}
	for _, kw := range drMustEmbedded {
		if !strings.Contains(drEmbedded, kw) {
			t.Fatalf("内嵌 templates/design-review.md 缺 CG-R2 自门/CG-R2b 修 1 关键片段 %q —— 说明修法仅落 ~/.claudego 副本,新机装机/副本被清将无声退化;修法:把 ~/.claudego/templates/design-review.md 完整回灌到仓库 templates/design-review.md", kw)
		}
	}

	// (b) 内嵌 fix-cycle.md 必含"提交前机械门"章节 + go build/go test 关键片段。
	fcEmbedded, err := loadTemplate(fakeRoot, "fix-cycle")
	if err != nil {
		t.Fatalf("loadTemplate fix-cycle 内嵌兜底失败: %v", err)
	}
	fcMustEmbedded := []string{
		"提交前机械门",
		"go build ./...",
		"go test ./...",
	}
	for _, kw := range fcMustEmbedded {
		if !strings.Contains(fcEmbedded, kw) {
			t.Fatalf("内嵌 templates/fix-cycle.md 缺提交前机械门关键片段 %q —— 修法仅落 ~/.claudego 副本,内嵌兜底会让修复卡失去 build/test 硬要求;修法:把 ~/.claudego/templates/fix-cycle.md 回灌到仓库 templates/fix-cycle.md", kw)
		}
	}

	// (c) CLAUDEGO_REQUIRE_SYNC_SCRIPTS=1 下再断言装机副本 ~/.claudego/templates/*.md 也含上述片段
	//    (装机验收流水的显式红线;缺省不吵异环境)。
	requireEnv := os.Getenv("CLAUDEGO_REQUIRE_SYNC_SCRIPTS") == "1"
	if !requireEnv {
		t.Log("提示(非致命):设 CLAUDEGO_REQUIRE_SYNC_SCRIPTS=1 可对 ~/.claudego 装机副本追加同源断言")
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	for _, name := range []string{"design-review", "fix-cycle"} {
		p := filepath.Join(home, ".claudego", "templates", name+".md")
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("装机模板 %s 缺失或不可读 (%v) —— CLAUDEGO_REQUIRE_SYNC_SCRIPTS=1 下必须存在", p, err)
		}
		var must []string
		if name == "design-review" {
			must = drMustEmbedded
		} else {
			must = fcMustEmbedded
		}
		for _, kw := range must {
			if !strings.Contains(string(body), kw) {
				t.Fatalf("装机 %s 缺关键片段 %q —— 装机副本漂/旧版本;修法:重跑 writeDefaultTemplates 或从仓库 templates/%s.md 覆盖", p, kw, name)
			}
		}
	}
}
