package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func kimiCLITestConfig(t *testing.T, bin string) *Config {
	t.Helper()
	cfg := defaultConfig("")
	cfg.DefaultRunner = "codex"
	cfg.CodexBin = "/usr/bin/true"
	cfg.KimiCLIBin = bin
	cfg.KimiCLIHome = t.TempDir()
	if err := os.WriteFile(filepath.Join(cfg.KimiCLIHome, "config.toml"), []byte("default_model = \"kimi-code/k3\"\n\n[providers.\"managed:kimi-code\"]\ntype = \"kimi\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(cfg.KimiCLIHome, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.KimiCLIModel = "kimi-code/k3"
	cfg.KimiCLIEffort = "max"
	cfg.StepTimeoutMin = 1
	cfg.CooldownMarginSec = 0
	cfg.KimiCLIOpus = &KimiCLIOpusRoute{
		Enabled: true, ExcludeBackend: true, Model: "kimi-code/k3", Effort: "max", LimitFallbackMin: 180,
	}
	cfg.GrokBuildBin = "/usr/bin/true"
	cfg.GrokBuild = &GrokBuildRoute{
		Enabled: true, Model: "grok-4.6", Effort: "xhigh", KimiOpusFallback: true,
		CodexFallbackModel: "gpt-5.6-sol", CodexFallbackEffort: "xhigh",
		ReviewCodexModel: "gpt-5.6-sol", ReviewCodexEffort: "max", OpusAdversarialReview: true,
		TierRoutes: map[string]GrokTierRoute{
			"opus_backend": {Effort: "xhigh", CodexFallbackModel: "gpt-5.6-sol", CodexFallbackEffort: "max"},
			"sonnet":       {Effort: "high", CodexFallbackModel: "gpt-5.6-luna", CodexFallbackEffort: "max"},
			"haiku":        {Effort: "high", CodexFallbackModel: "gpt-5.6-luna", CodexFallbackEffort: "xhigh"},
		},
	}
	return cfg
}

// kimiCredentialSentinel 是测试用的假凭据值。任何 Cardex 侧的读取、复制或序列化都会让它
// 出现在 Cardex 运行目录里，从而被 assertNoKimiCredentialCopy 抓住。
const kimiCredentialSentinel = "SENTINEL-KIMI-OAUTH-VALUE-DO-NOT-COPY"

func writeKimiSourceCredential(t *testing.T, cfg *Config, name, body string) string {
	t.Helper()
	path := filepath.Join(cfg.KimiCLIHome, "credentials", name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func kimiDirNames(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name()+":"+entry.Type().String())
	}
	return strings.Join(names, ",")
}

// assertNoKimiCredentialCopy 证明 Cardex 没有把凭据内容复制进自己的状态目录。软链接按链接本身
// 计数（filepath.Walk 用 Lstat），因此只有真实的内容副本才会命中。
func assertNoKimiCredentialCopy(t *testing.T, root string) {
	t.Helper()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(data), kimiCredentialSentinel) {
			t.Fatalf("Cardex 状态目录出现凭据内容副本: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertKimiCredentialSymlink(t *testing.T, destCredentials, sourceCredentials, name string) {
	t.Helper()
	path := filepath.Join(destCredentials, name)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("缺少任务级凭据投影 %s: %v", path, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("凭据必须按文件级软链接投影，%s mode=%v", path, info.Mode())
	}
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(sourceCredentials, name); target != want {
		t.Fatalf("凭据软链接目标错误 %s -> %q，应为 %q", path, target, want)
	}
}

func assertKimiTaskLocalCredentialDir(t *testing.T, destCredentials string) {
	t.Helper()
	info, err := os.Lstat(destCredentials)
	if err != nil {
		t.Fatalf("任务级凭据目录不可用 %s: %v", destCredentials, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("任务级凭据目录不得是目录级软链接: %s", destCredentials)
	}
	if !info.IsDir() {
		t.Fatalf("任务级凭据路径必须是真实目录: %s mode=%v", destCredentials, info.Mode())
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("任务级凭据目录权限必须是 0700，实际 %#o", perm)
	}
}

// TestPrepareKimiCLIHomeProjectsCredentialsAsPerFileSymlinks 钉住修复后的投影形状：任务级
// credentials 是 Cardex 自己的 0700 真实目录，源凭据按文件逐个精确软链接，且重复准备幂等。
func TestPrepareKimiCLIHomeProjectsCredentialsAsPerFileSymlinks(t *testing.T) {
	root := testRoot(t)
	cfg := kimiCLITestConfig(t, "/usr/bin/true")
	sourceCredentials := filepath.Join(cfg.KimiCLIHome, "credentials")
	writeKimiSourceCredential(t, cfg, "kimi-code.json", `{"access_token":"`+kimiCredentialSentinel+`"}`)
	writeKimiSourceCredential(t, cfg, "managed-kimi-code.json", `{"refresh_token":"`+kimiCredentialSentinel+`-2"}`)
	sourceBefore := kimiDirNames(t, sourceCredentials)

	runtimeHome, err := prepareKimiCLIHome(root, cfg, false)
	if err != nil {
		t.Fatalf("prepare 失败: %v", err)
	}
	destCredentials := filepath.Join(runtimeHome, "credentials")
	assertKimiTaskLocalCredentialDir(t, destCredentials)
	for _, name := range []string{"kimi-code.json", "managed-kimi-code.json"} {
		assertKimiCredentialSymlink(t, destCredentials, sourceCredentials, name)
	}
	if got := kimiDirNames(t, destCredentials); got != "kimi-code.json:L---------,managed-kimi-code.json:L---------" {
		t.Fatalf("任务级凭据目录条目不符: %q", got)
	}
	assertNoKimiCredentialCopy(t, root)

	// 幂等：再次准备不得报错、不得改变投影，也不得触碰全局源目录。
	if _, err := prepareKimiCLIHome(root, cfg, false); err != nil {
		t.Fatalf("重复准备必须幂等: %v", err)
	}
	assertKimiTaskLocalCredentialDir(t, destCredentials)
	for _, name := range []string{"kimi-code.json", "managed-kimi-code.json"} {
		assertKimiCredentialSymlink(t, destCredentials, sourceCredentials, name)
	}
	if got := kimiDirNames(t, sourceCredentials); got != sourceBefore {
		t.Fatalf("全局凭据目录被改动: %q != %q", got, sourceBefore)
	}

	// 目录权限被外部放宽后，下一次准备必须收回到 0700。
	if err := os.Chmod(destCredentials, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareKimiCLIHome(root, cfg, false); err != nil {
		t.Fatalf("权限修复路径必须幂等成功: %v", err)
	}
	assertKimiTaskLocalCredentialDir(t, destCredentials)

	// 源新增凭据文件后，下一次准备补投影。
	writeKimiSourceCredential(t, cfg, "extra.json", "{}")
	if _, err := prepareKimiCLIHome(root, cfg, false); err != nil {
		t.Fatalf("源新增凭据后准备失败: %v", err)
	}
	assertKimiCredentialSymlink(t, destCredentials, sourceCredentials, "extra.json")
}

// TestPrepareKimiCLIHomeCredentialAtomicReplaceStaysTaskLocal 复现 Kimi 刷新令牌的真实写法：
// 在凭据目录内建临时文件再同目录 rename 覆盖。修复后这一步必须完全落在任务级目录内，全局源
// 凭据的内容、权限、inode 与目录条目都不得变化，之后重新准备仍然安全幂等。
func TestPrepareKimiCLIHomeCredentialAtomicReplaceStaysTaskLocal(t *testing.T) {
	root := testRoot(t)
	cfg := kimiCLITestConfig(t, "/usr/bin/true")
	sourceCredentials := filepath.Join(cfg.KimiCLIHome, "credentials")
	const name = "kimi-code.json"
	original := `{"access_token":"` + kimiCredentialSentinel + `"}`
	sourcePath := writeKimiSourceCredential(t, cfg, name, original)
	sourceInfoBefore, err := os.Lstat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	sourceListBefore := kimiDirNames(t, sourceCredentials)

	runtimeHome, err := prepareKimiCLIHome(root, cfg, false)
	if err != nil {
		t.Fatalf("prepare 失败: %v", err)
	}
	destCredentials := filepath.Join(runtimeHome, "credentials")

	const refreshed = `{"access_token":"REFRESHED-TASK-LOCAL"}`
	tmp, err := os.CreateTemp(destCredentials, ".tmp-credential-*")
	if err != nil {
		t.Fatalf("任务级凭据目录必须允许 Kimi 建临时文件: %v", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		t.Fatal(err)
	}
	if _, err := tmp.WriteString(refreshed); err != nil {
		tmp.Close()
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(destCredentials, name)); err != nil {
		t.Fatalf("同目录原子替换必须成功: %v", err)
	}

	assertGlobalSourceUntouched := func(stage string) {
		t.Helper()
		got, err := os.ReadFile(sourcePath)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != original {
			t.Fatalf("%s: 全局源凭据内容被改写", stage)
		}
		after, err := os.Lstat(sourcePath)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(sourceInfoBefore, after) {
			t.Fatalf("%s: 全局源凭据被换成了新 inode", stage)
		}
		if after.Mode() != sourceInfoBefore.Mode() {
			t.Fatalf("%s: 全局源凭据权限被改动 %v -> %v", stage, sourceInfoBefore.Mode(), after.Mode())
		}
		if !after.ModTime().Equal(sourceInfoBefore.ModTime()) {
			t.Fatalf("%s: 全局源凭据 mtime 被改动", stage)
		}
		if got := kimiDirNames(t, sourceCredentials); got != sourceListBefore {
			t.Fatalf("%s: 全局凭据目录条目被改动 %q != %q", stage, got, sourceListBefore)
		}
	}
	assertGlobalSourceUntouched("原子替换后")

	// Kimi 已经把软链接换成任务级 0600 普通文件；重新准备必须接受这一状态而不是覆盖回软链接。
	if _, err := prepareKimiCLIHome(root, cfg, false); err != nil {
		t.Fatalf("原子替换后重新准备必须安全幂等: %v", err)
	}
	replaced := filepath.Join(destCredentials, name)
	info, err := os.Lstat(replaced)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("不得把 Kimi 写入的任务级凭据文件重新覆盖成软链接")
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("任务级凭据文件权限被改动: %#o", perm)
	}
	data, err := os.ReadFile(replaced)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != refreshed {
		t.Fatalf("任务级凭据内容被改动: %q", data)
	}
	assertGlobalSourceUntouched("重新准备后")
	assertKimiTaskLocalCredentialDir(t, destCredentials)
}

// TestPrepareKimiCLIHomeCredentialProjectionFailsClosed 钉住全部 fail-closed 分支：错误目标、
// 目录级软链接、意外文件类型、越界路径。任何一条都必须报错，且不得静默改写既有状态。
func TestPrepareKimiCLIHomeCredentialProjectionFailsClosed(t *testing.T) {
	newCase := func(t *testing.T) (root string, cfg *Config, sourceCredentials, executeHome string) {
		t.Helper()
		root = testRoot(t)
		cfg = kimiCLITestConfig(t, "/usr/bin/true")
		sourceCredentials = filepath.Join(cfg.KimiCLIHome, "credentials")
		writeKimiSourceCredential(t, cfg, "kimi-code.json", `{"access_token":"`+kimiCredentialSentinel+`"}`)
		return root, cfg, sourceCredentials, filepath.Join(root, "kimi-cli-home", "execute")
	}

	t.Run("遗留整目录软链接", func(t *testing.T) {
		root, cfg, sourceCredentials, executeHome := newCase(t)
		if err := os.MkdirAll(executeHome, 0o700); err != nil {
			t.Fatal(err)
		}
		legacy := filepath.Join(executeHome, "credentials")
		if err := os.Symlink(sourceCredentials, legacy); err != nil {
			t.Fatal(err)
		}
		if _, err := prepareKimiCLIHome(root, cfg, false); err == nil {
			t.Fatal("整目录软链接必须 fail-closed，不得被当成合法投影")
		}
		info, err := os.Lstat(legacy)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("fail-closed 不得静默改写既有路径: info=%v err=%v", info, err)
		}
	})

	t.Run("任务级凭据路径是普通文件", func(t *testing.T) {
		root, cfg, _, executeHome := newCase(t)
		if err := os.MkdirAll(executeHome, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(executeHome, "credentials"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := prepareKimiCLIHome(root, cfg, false); err == nil {
			t.Fatal("任务级凭据路径是普通文件必须 fail-closed")
		}
	})

	t.Run("软链接目标越界", func(t *testing.T) {
		root, cfg, sourceCredentials, executeHome := newCase(t)
		if _, err := prepareKimiCLIHome(root, cfg, false); err != nil {
			t.Fatal(err)
		}
		projected := filepath.Join(executeHome, "credentials", "kimi-code.json")
		for _, target := range []string{
			filepath.Join(cfg.KimiCLIHome, "config.toml"),
			filepath.Join(sourceCredentials, "..", "config.toml"),
			"/etc/hosts",
		} {
			if err := os.Remove(projected); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, projected); err != nil {
				t.Fatal(err)
			}
			if _, err := prepareKimiCLIHome(root, cfg, false); err == nil {
				t.Fatalf("越界软链接目标必须 fail-closed: %s", target)
			}
		}
	})

	t.Run("软链接没有对应源凭据", func(t *testing.T) {
		root, cfg, sourceCredentials, executeHome := newCase(t)
		if _, err := prepareKimiCLIHome(root, cfg, false); err != nil {
			t.Fatal(err)
		}
		orphan := filepath.Join(executeHome, "credentials", "ghost.json")
		if err := os.Symlink(filepath.Join(sourceCredentials, "ghost.json"), orphan); err != nil {
			t.Fatal(err)
		}
		if _, err := prepareKimiCLIHome(root, cfg, false); err == nil {
			t.Fatal("悬空凭据软链接必须 fail-closed")
		}
	})

	t.Run("任务级凭据目录出现意外类型", func(t *testing.T) {
		root, cfg, _, executeHome := newCase(t)
		if _, err := prepareKimiCLIHome(root, cfg, false); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(executeHome, "credentials", "nested"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := prepareKimiCLIHome(root, cfg, false); err == nil {
			t.Fatal("任务级凭据目录出现子目录必须 fail-closed")
		}
	})

	t.Run("源目录出现子目录", func(t *testing.T) {
		root, cfg, sourceCredentials, _ := newCase(t)
		if err := os.Mkdir(filepath.Join(sourceCredentials, "nested"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := prepareKimiCLIHome(root, cfg, false); err == nil {
			t.Fatal("源凭据目录出现子目录必须 fail-closed")
		}
	})

	t.Run("源目录出现软链接条目", func(t *testing.T) {
		root, cfg, sourceCredentials, _ := newCase(t)
		if err := os.Symlink(filepath.Join(cfg.KimiCLIHome, "config.toml"), filepath.Join(sourceCredentials, "linked.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := prepareKimiCLIHome(root, cfg, false); err == nil {
			t.Fatal("源凭据目录出现软链接条目必须 fail-closed")
		}
	})

	t.Run("源凭据路径不是目录", func(t *testing.T) {
		root := testRoot(t)
		cfg := kimiCLITestConfig(t, "/usr/bin/true")
		sourceCredentials := filepath.Join(cfg.KimiCLIHome, "credentials")
		if err := os.Remove(sourceCredentials); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sourceCredentials, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := prepareKimiCLIHome(root, cfg, false); err == nil {
			t.Fatal("源凭据路径不是目录必须 fail-closed")
		}
	})
}

// TestPrepareKimiCLIHomeCredentialProjectionIsolatesExecuteAndReview 保持 execute/review 运行时
// 隔离：review 准备不得创建 execute 目录，两侧凭据各自任务级独立，一侧原子替换不串到另一侧或全局。
func TestPrepareKimiCLIHomeCredentialProjectionIsolatesExecuteAndReview(t *testing.T) {
	root := testRoot(t)
	cfg := kimiCLITestConfig(t, "/usr/bin/true")
	sourceCredentials := filepath.Join(cfg.KimiCLIHome, "credentials")
	const name = "kimi-code.json"
	original := `{"access_token":"` + kimiCredentialSentinel + `"}`
	sourcePath := writeKimiSourceCredential(t, cfg, name, original)

	reviewHome, err := prepareKimiCLIHome(root, cfg, true)
	if err != nil {
		t.Fatalf("review 准备失败: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "kimi-cli-home", "execute")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("review 准备不得创建 execute 运行目录: %v", err)
	}
	executeHome, err := prepareKimiCLIHome(root, cfg, false)
	if err != nil {
		t.Fatalf("execute 准备失败: %v", err)
	}
	if reviewHome == executeHome {
		t.Fatal("execute/review 必须使用不同运行时家目录")
	}
	reviewCredentials := filepath.Join(reviewHome, "credentials")
	executeCredentials := filepath.Join(executeHome, "credentials")
	for _, dir := range []string{reviewCredentials, executeCredentials} {
		assertKimiTaskLocalCredentialDir(t, dir)
		assertKimiCredentialSymlink(t, dir, sourceCredentials, name)
	}

	tmp, err := os.CreateTemp(reviewCredentials, ".tmp-credential-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.WriteString(`{"access_token":"REVIEW-ONLY"}`); err != nil {
		tmp.Close()
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(reviewCredentials, name)); err != nil {
		t.Fatal(err)
	}

	assertKimiCredentialSymlink(t, executeCredentials, sourceCredentials, name)
	if got, err := os.ReadFile(sourcePath); err != nil || string(got) != original {
		t.Fatalf("review 侧替换不得改动全局源凭据: %q err=%v", got, err)
	}
	if _, err := prepareKimiCLIHome(root, cfg, false); err != nil {
		t.Fatalf("另一侧替换后 execute 准备仍须成功: %v", err)
	}
	assertKimiCredentialSymlink(t, executeCredentials, sourceCredentials, name)
}

// TestPrepareKimiCLIHomeNeverReadsCredentialContent 从两个角度证明 Cardex 只做路径级投影：
// 投影代码里没有任何内容读取/摘要 API；并且源凭据即使不可读，投影依然成功。
func TestPrepareKimiCLIHomeNeverReadsCredentialContent(t *testing.T) {
	src, err := os.ReadFile("kimi.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(src)
	const marker = "func projectKimiTaskLocalCredentials("
	start := strings.Index(source, marker)
	if start < 0 {
		t.Fatalf("未找到凭据投影函数 %q", marker)
	}
	body := source[start:]
	if end := strings.Index(body[len(marker):], "\nfunc "); end >= 0 {
		body = body[:len(marker)+end]
	}
	for _, forbidden := range []string{"ReadFile", "ReadAll", "os.Open", "bufio.", "sha256", "sha1", "md5", "Hash", "io.Copy"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("凭据投影不得使用内容读取/摘要 API: %s", forbidden)
		}
	}
	if got := strings.Count(source, "os.ReadFile("); got != 1 || !strings.Contains(source, "os.ReadFile(sourceConfig)") {
		t.Fatalf("kimi.go 只允许读取 config.toml，一次 os.ReadFile；实际 %d 次", got)
	}

	if os.Geteuid() == 0 {
		t.Skip("root 会绕过文件权限，无法用不可读源凭据证明零读取")
	}
	root := testRoot(t)
	cfg := kimiCLITestConfig(t, "/usr/bin/true")
	unreadable := writeKimiSourceCredential(t, cfg, "kimi-code.json", `{"access_token":"`+kimiCredentialSentinel+`"}`)
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o600) })
	runtimeHome, err := prepareKimiCLIHome(root, cfg, false)
	if err != nil {
		t.Fatalf("不可读源凭据下投影必须成功（证明 Cardex 从不打开凭据内容）: %v", err)
	}
	assertKimiCredentialSymlink(t, filepath.Join(runtimeHome, "credentials"), filepath.Join(cfg.KimiCLIHome, "credentials"), "kimi-code.json")
	assertNoKimiCredentialCopy(t, root)
}

func TestParseKimiCLIJSONL(t *testing.T) {
	raw := `{"role":"meta","type":"system.version","version":"0.35.0"}` + "\n" +
		`{"role":"assistant","content":"OK"}` + "\n" +
		`{"role":"meta","type":"session.resume_hint","session_id":"session-1"}`
	res := parseKimiCLIJSONL(raw)
	if res.Result != "OK" || res.SessionID != "session-1" || res.NumTurns != 1 || res.IsError {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestParseKimiCLIJSONLUnknownMetadataFailsProofClosed(t *testing.T) {
	res := parseKimiCLIJSONL(`{"role":"meta","type":"model.started"}`)
	if res.ObservationComplete {
		t.Fatalf("unknown metadata must not be assumed presemantic: %+v", res)
	}
}

func TestKimiCLIOpusSecondLegAvailabilityAndBackendExclusion(t *testing.T) {
	root := testRoot(t)
	cfg := kimiCLITestConfig(t, "/usr/bin/true")
	for _, hour := range []int{0, 3, 12, 23} {
		task := &Task{Model: "opus", PreferRunner: "codex", Type: typeSequence, Dir: t.TempDir(), Prompts: []string{"实现前端页面"}}
		now := time.Date(2026, 8, 13, hour, 0, 0, 0, time.Local)
		if !kimiCLIOpusEligible(root, cfg, task, now) {
			t.Fatalf("%02d:00 非后端 Opus 的 Kimi 第二腿应可用", hour)
		}
	}

	for name, task := range map[string]*Task{
		"显式后端": {Model: "opus", PreferRunner: "codex", Type: typeSequence, RouteClass: routeClassBackend, Prompts: []string{"实现功能"}},
		"存量词项": {Model: "opus", PreferRunner: "codex", Type: typeSequence, Prompts: []string{"实现数据库迁移和 API endpoint"}},
		"人工消歧": {Model: "opus", PreferRunner: "codex", Type: typeSequence, RouteClass: routeClassGeneral, Prompts: []string{"评估后端方案但只改文档"}},
	} {
		t.Run(name, func(t *testing.T) {
			excluded := kimiCLIBackendExcluded(cfg, task)
			if name == "人工消歧" && excluded {
				t.Fatal("route_class=general 必须覆盖文本误命中")
			}
			if name != "人工消歧" && !excluded {
				t.Fatal("后端开发必须排除 Kimi 自动路由")
			}
		})
	}

	sonnet := &Task{Model: "sonnet", PreferRunner: "codex", Type: typeSequence, Prompts: []string{"实现前端"}}
	if kimiCLIOpusEligible(root, cfg, sonnet, time.Now()) {
		t.Fatal("Sonnet 不得进入 Opus Kimi 路由")
	}
}

func fakeKimiCLI(t *testing.T, payload string, exitCode int) (bin, argsDump, envDump string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "kimi")
	argsDump = filepath.Join(dir, "args.dump")
	envDump = filepath.Join(dir, "env.dump")
	payloadPath := filepath.Join(dir, "result.jsonl")
	if err := os.WriteFile(payloadPath, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + shSingleQuote(argsDump) + "\n" +
		"printf '%s\\n' \"$KIMI_MODEL_THINKING_EFFORT\" \"$KIMI_CODE_NO_AUTO_UPDATE\" \"$KIMI_CODE_HOME\" > " + shSingleQuote(envDump) + "\n" +
		"cat " + shSingleQuote(payloadPath) + "\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsDump, envDump
}

func TestInvokeKimiCLIUsesK3MaxAndVersionCompatibleFlags(t *testing.T) {
	payload := `{"role":"meta","type":"system.version","version":"0.35.0"}` + "\n" +
		`{"role":"assistant","content":"OK"}` + "\n" +
		`{"role":"meta","type":"session.resume_hint","session_id":"session-ok"}`
	bin, argsDump, envDump := fakeKimiCLI(t, payload, 0)
	cfg := kimiCLITestConfig(t, bin)
	task := &Task{ID: "kimi-k3-max", Model: "opus", PreferRunner: "codex", Effort: "xhigh", Type: typeSequence, Dir: t.TempDir()}
	root := testRoot(t)
	admitDirectInvoke(t, root, task)
	res, combined, err := invokeKimiCLI(context.Background(), root, cfg, task, "p")
	if err != nil || res == nil || res.Result != "OK" || res.SessionID != "session-ok" {
		t.Fatalf("invoke failed: res=%+v err=%v", res, err)
	}
	if strings.Contains(combined, "COLD-START TRANSPORT RETRY") {
		t.Fatalf("0.35 semantic stream must not be replayed:\n%s", combined)
	}
	args, _ := os.ReadFile(argsDump)
	got := string(args)
	for _, want := range []string{"--model\nkimi-code/k3\n", "--prompt\np\n", "--output-format\nstream-json\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Kimi CLI argv 缺 %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "--auto\n") || strings.Contains(got, "--yolo\n") || strings.Contains(got, "--plan\n") {
		t.Fatalf("Kimi CLI 0.35.0 禁止 --prompt 与 --auto/--yolo/--plan 同用:\n%s", got)
	}
	env, _ := os.ReadFile(envDump)
	envLines := strings.Split(strings.TrimSpace(string(env)), "\n")
	executeHome := filepath.Join(root, "kimi-cli-home", "execute")
	if len(envLines) != 3 || envLines[0] != "max" || envLines[1] != "1" || envLines[2] != executeHome {
		t.Fatalf("K3 max 必须通过官方环境变量注入, env=%q", env)
	}
	runtimeConfig, err := os.ReadFile(filepath.Join(executeHome, "config.toml"))
	if err != nil || !strings.Contains(string(runtimeConfig), `default_permission_mode = "auto"`) ||
		!strings.Contains(string(runtimeConfig), `default_plan_mode = false`) {
		t.Fatalf("Cardex 隔离配置必须启用 prompt 模式自动权限: err=%v config=%q", err, runtimeConfig)
	}
}

func TestInvokeKimiCLIReviewUsesIsolatedConfiguredPlanMode(t *testing.T) {
	payload := `{"role":"assistant","content":"REVIEW_OK"}`
	bin, argsDump, envDump := fakeKimiCLI(t, payload, 0)
	cfg := kimiCLITestConfig(t, bin)
	task := &Task{ID: "kimi-review", Model: "opus", Type: "review", Dir: t.TempDir()}
	root := testRoot(t)
	admitDirectInvoke(t, root, task)
	res, _, err := invokeKimiCLI(context.Background(), root, cfg, task, "review")
	if err != nil || res == nil || res.Result != "REVIEW_OK" {
		t.Fatalf("review invoke failed: res=%+v err=%v", res, err)
	}
	args, _ := os.ReadFile(argsDump)
	if got := string(args); strings.Contains(got, "--plan\n") || strings.Contains(got, "--auto\n") || strings.Contains(got, "--yolo\n") {
		t.Fatalf("prompt mode must not receive incompatible permission flags:\n%s", got)
	}
	reviewHome := filepath.Join(root, "kimi-cli-home", "review")
	env, _ := os.ReadFile(envDump)
	if !strings.HasSuffix(strings.TrimSpace(string(env)), reviewHome) {
		t.Fatalf("review must use its isolated runtime home: %q", env)
	}
	runtimeConfig, err := os.ReadFile(filepath.Join(reviewHome, "config.toml"))
	if err != nil || !strings.Contains(string(runtimeConfig), `default_permission_mode = "auto"`) ||
		!strings.Contains(string(runtimeConfig), `default_plan_mode = true`) {
		t.Fatalf("review runtime must enable configured plan mode: err=%v config=%q", err, runtimeConfig)
	}
	executeConfig := filepath.Join(root, "kimi-cli-home", "execute", "config.toml")
	if _, err := os.Stat(executeConfig); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("review must not create or mutate the execute runtime: %v", err)
	}
}

func TestInvokeKimiCLI0361RetriesMetadataOnlyColdStart(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "kimi")
	countPath := filepath.Join(dir, "count")
	script := "#!/bin/sh\n" +
		"count=0\n" +
		"if [ -f " + shSingleQuote(countPath) + " ]; then count=$(cat " + shSingleQuote(countPath) + "); fi\n" +
		"count=$((count + 1))\n" +
		"printf '%s' \"$count\" > " + shSingleQuote(countPath) + "\n" +
		"printf '%s\\n' '{\"role\":\"meta\",\"type\":\"system.version\",\"version\":\"0.36.1\"}'\n" +
		"if [ \"$count\" -eq 1 ]; then exit 1; fi\n" +
		"printf '%s\\n' '{\"role\":\"assistant\",\"content\":\"KIMI_0361_OK\"}'\n" +
		"printf '%s\\n' '{\"role\":\"meta\",\"type\":\"session.resume_hint\",\"session_id\":\"session-0361\"}'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := kimiCLITestConfig(t, bin)
	task := &Task{ID: "kimi-0361-cold-start", Model: "opus", Type: typeSequence, Dir: t.TempDir()}
	root := admitDirectInvoke(t, "", task)
	res, combined, err := invokeKimiCLI(context.Background(), root, cfg, task, "read only")
	if err != nil || res == nil || res.Result != "KIMI_0361_OK" || res.SessionID != "session-0361" {
		t.Fatalf("0.36.1 metadata-only cold start should restart transport once: res=%+v err=%v\n%s", res, err, combined)
	}
	count, err := os.ReadFile(countPath)
	if err != nil || string(count) != "2" {
		t.Fatalf("expected exactly two transport starts, count=%q err=%v", count, err)
	}
}

func TestValidateKimiCLIOpenFileLimit(t *testing.T) {
	if err := validateKimiCLIOpenFileLimit(256); err == nil ||
		!strings.Contains(err.Error(), "EMFILE") || !strings.Contains(err.Error(), "65536") {
		t.Fatalf("low launchd ceiling must fail with actionable EMFILE diagnosis: %v", err)
	}
	if err := validateKimiCLIOpenFileLimit(65536); err != nil {
		t.Fatalf("recommended ceiling must pass: %v", err)
	}
}

func TestKimiCLIEMFILEStderrOverridesVersionMetadata(t *testing.T) {
	stdout := `{"role":"meta","type":"system.version","version":"0.36.1"}`
	stderr := "[unexpected] Error: EMFILE: too many open files, watch\n"
	runErr := errors.New("exit status 1")
	res := parseKimiCLIJSONL(stdout)
	preserveKimiCLIProcessError(res, stderr, runErr)
	got := errorSummary(res, stdout+"\n"+stderr, runErr)
	if !strings.Contains(got, "EMFILE: too many open files, watch") {
		t.Fatalf("last_error must surface concrete stderr root cause, got %q", got)
	}
	if strings.Contains(got, "system.version") {
		t.Fatalf("system.version is metadata and must not mask root cause, got %q", got)
	}
}

func TestInvokeKimiCLI0361PreservesEMFILEStderr(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "kimi")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' '{\"role\":\"meta\",\"type\":\"system.version\",\"version\":\"0.36.1\"}'\n" +
		"printf '%s\\n' '[unexpected] Error: EMFILE: too many open files, watch' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := kimiCLITestConfig(t, bin)
	task := &Task{ID: "kimi-0361-emfile", Model: "opus", Type: typeSequence, Dir: dir}
	root := admitDirectInvoke(t, "", task)
	res, combined, runErr := invokeKimiCLI(context.Background(), root, cfg, task, "read only")
	if runErr == nil || res == nil || !res.IsError ||
		!strings.Contains(errorSummary(res, combined, runErr), "EMFILE: too many open files, watch") {
		t.Fatalf("EMFILE stderr must survive version metadata: res=%+v err=%v\n%s", res, runErr, combined)
	}
	if strings.Contains(combined, "COLD-START TRANSPORT RETRY") {
		t.Fatalf("concrete stderr must not trigger metadata-only retry:\n%s", combined)
	}
}

func TestRunTaskKimiCLILimitHoldsWithoutGlobalSolFallback(t *testing.T) {
	root := testRoot(t)
	payload := `{"role":"meta","type":"error","content":"HTTP 429: usage limit reached"}`
	bin, _, _ := fakeKimiCLI(t, payload, 1)
	cfg := kimiCLITestConfig(t, bin)
	cfg.OwnerRoutingEnforced = true
	task := newTask(root, cfg, typeSequence, "kimi limit", t.TempDir(), []string{"p"}, 1)
	task.Model = "opus"
	task.RouteClass = routeClassGeneral
	route, ok := resolveOwnerRoute(cfg, task)
	if !ok || !pinOwnerPrimaryRoute(task, route) {
		t.Fatal("无法钉定 Grok 第一腿")
	}
	task.Runner = grokBuildRunnerName
	if err := queuePolicyFallback(cfg, task, fallbackTransport, v3ProofAuthorization()); err != nil {
		t.Fatal(err)
	}
	task.Runner = kimiCLIRunnerName
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	if err := runTaskVia(context.Background(), root, cfg, task, kimiCLIRunnerName); err != nil {
		t.Fatal(err)
	}
	got, err := loadTask(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusHeld || got.RouteReason != routeReasonGrokToKimi ||
		got.PreferRunner != kimiCLIRunnerName || got.CodexModel != "" || got.OwnerRouteLeg != 2 ||
		got.SessionID != "" || got.Attempts != 0 || !strings.Contains(got.LastError, "global Codex fallback disabled") {
		t.Fatalf("Kimi 安全限额必须保持第二腿并关闭全局 Sol 回退: %+v", got)
	}
	cd := loadEngineCooldown(root, kimiCLICooldownName)
	if cd == nil || !cd.active(time.Now()) || cd.UntilEpoch < time.Now().Add(179*time.Minute).Unix() {
		t.Fatalf("应写独立 Kimi CLI 车道冷却约 180 分钟: %+v", cd)
	}
	if _, err := os.Stat(cooldownPath(root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Kimi CLI 限额不得写 Claude 全局冷却: %v", err)
	}
	events := readAllEventsRaw(t, root, task.ID)
	last := events[len(events)-1]
	if last.Type != evHeld || last.Actor != "runner:policy-fallback" ||
		last.Detail["reason"] != "no_resolver_proven_next_leg" ||
		last.Detail["workspace_fingerprint_before"] == "" ||
		last.Detail["workspace_fingerprint_before"] != last.Detail["workspace_fingerprint_after"] {
		t.Fatalf("应留下 Kimi CLI 无解析下一腿的三证持有事件: %+v", last)
	}
}

func TestKimiCLIBoardShowsActualModelEffortAndRoute(t *testing.T) {
	cfg := kimiCLITestConfig(t, "/usr/bin/true")
	now := time.Now()
	queued := &Task{ID: "queued", Status: statusQueued, PreferRunner: "codex", Model: "opus", Effort: "xhigh", Type: typeSequence, Prompts: []string{"前端实现"}}
	brief := toBrief(cfg, queued, now)
	if brief.Runner != grokBuildRunnerName || brief.RunnerSource != "route_policy" ||
		brief.Model != "grok-4.6" || brief.ModelSource != "grok_model" ||
		brief.Effort != "xhigh" || brief.EffortSource != "grok_effort" || brief.RouteReason != routeReasonGrokOpusGeneral ||
		!strings.Contains(brief.ModelRoute, "Grok Build 4.6/xhigh →（仅安全失败）Kimi CLI K3/max") {
		t.Fatalf("未派发 Opus/general 卡必须显示 Grok→Kimi 开头的全路由: %+v", brief)
	}

	backend := &Task{ID: "backend", Status: statusQueued, PreferRunner: "codex", Model: "opus", Effort: "xhigh", Type: typeSequence, RouteClass: routeClassBackend, Prompts: []string{"后端实现"}}
	brief = toBrief(cfg, backend, now)
	if brief.Runner != grokBuildRunnerName || brief.Model != "grok-4.6" || brief.Effort != "xhigh" || brief.RouteReason != routeReasonGrokOpusBackend {
		t.Fatalf("后端 Opus 必须显示 Grok/xhigh 主腿: %+v", brief)
	}
}

func TestValidateKimiCLIConfig(t *testing.T) {
	cfg := kimiCLITestConfig(t, "/usr/bin/true")
	if err := validateKimiCLI(cfg); err != nil {
		t.Fatalf("合法 Kimi CLI 策略被拒: %v", err)
	}
	missingGrok := kimiCLITestConfig(t, "/usr/bin/true")
	missingGrok.GrokBuild = nil
	if err := validateKimiCLI(missingGrok); err == nil {
		t.Fatal("Kimi 自动路由缺 Grok 下一腿必须 fail-fast")
	}
	cfg.KimiCLIOpus.Effort = "ultra"
	if err := validateKimiCLI(cfg); err == nil {
		t.Fatal("非法 Kimi effort 必须 fail-fast")
	}
}

func TestKimiCLIRealCanary(t *testing.T) {
	if os.Getenv("CARDEX_KIMI_REAL_CANARY") != "1" {
		t.Skip("set CARDEX_KIMI_REAL_CANARY=1 for the authenticated integration canary")
	}
	bin := strings.TrimSpace(os.Getenv("CARDEX_KIMI_BIN"))
	home := strings.TrimSpace(os.Getenv("CARDEX_KIMI_HOME"))
	if bin == "" || home == "" {
		t.Fatal("CARDEX_KIMI_BIN and CARDEX_KIMI_HOME are required")
	}
	cfg := defaultConfig("")
	cfg.KimiCLIBin = bin
	cfg.KimiCLIHome = home
	cfg.KimiCLIModel = "kimi-code/k3"
	cfg.KimiCLIEffort = "max"
	cfg.StepTimeoutMin = 2
	task := &Task{
		ID: "kimi-real-canary", Type: typeSequence, Dir: t.TempDir(), Model: "opus",
		Prompts: []string{"run pwd"}, PreferRunner: "codex",
	}
	root := admitDirectInvoke(t, "", task)
	res, _, err := invokeKimiCLI(context.Background(), root, cfg, task,
		"这是 Cardex Kimi CLI 原生执行器测试。运行 pwd，不做任何写入，然后只回复 KIMI_CARDEX_NATIVE_OK。")
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !strings.Contains(res.Result, "KIMI_CARDEX_NATIVE_OK") {
		t.Fatalf("unexpected Kimi result: %+v", res)
	}
}

func TestKimiCLIRealReviewCanary(t *testing.T) {
	if os.Getenv("CARDEX_KIMI_REAL_REVIEW_CANARY") != "1" {
		t.Skip("set CARDEX_KIMI_REAL_REVIEW_CANARY=1 for the authenticated review canary")
	}
	bin := strings.TrimSpace(os.Getenv("CARDEX_KIMI_BIN"))
	home := strings.TrimSpace(os.Getenv("CARDEX_KIMI_HOME"))
	if bin == "" || home == "" {
		t.Fatal("CARDEX_KIMI_BIN and CARDEX_KIMI_HOME are required")
	}
	cfg := defaultConfig("")
	cfg.KimiCLIBin = bin
	cfg.KimiCLIHome = home
	cfg.KimiCLIModel = "kimi-code/k3"
	cfg.KimiCLIEffort = "max"
	cfg.StepTimeoutMin = 2
	task := &Task{
		ID: "kimi-real-review-canary", Type: "review", Dir: t.TempDir(), Model: "opus",
		Prompts: []string{"review pwd"}, PreferRunner: "codex",
	}
	root := testRoot(t)
	admitDirectInvoke(t, root, task)
	res, combined, err := invokeKimiCLI(context.Background(), root, cfg, task,
		"这是 Cardex Kimi CLI 只读复盘执行器测试。运行 pwd，不做任何写入，然后只回复 KIMI_CARDEX_REVIEW_OK。")
	if err != nil {
		t.Fatalf("review canary failed: %v\n%s", err, combined)
	}
	if res == nil || !strings.Contains(res.Result, "KIMI_CARDEX_REVIEW_OK") {
		t.Fatalf("unexpected Kimi review result: %+v", res)
	}
	runtimeConfig, err := os.ReadFile(filepath.Join(root, "kimi-cli-home", "review", "config.toml"))
	if err != nil || !strings.Contains(string(runtimeConfig), `default_plan_mode = true`) {
		t.Fatalf("real review canary did not use configured plan mode: err=%v config=%q", err, runtimeConfig)
	}
}
