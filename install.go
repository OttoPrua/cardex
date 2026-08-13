package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
)

// buildProvenance is the narrow part of Go build metadata that Cardex needs to
// keep an installed binary bound to a reviewed commit. It deliberately does not
// invent a release registry or write any deployment state.
type buildProvenance struct {
	Revision      string
	Modified      bool
	ModifiedKnown bool
}

// These values are injected by the repository build target. debug.ReadBuildInfo
// remains a fallback for toolchains that stamp VCS settings themselves.
var (
	buildRevision string
	buildModified string
)

func currentBuildProvenance() buildProvenance {
	var provenance buildProvenance
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				provenance.Revision = strings.ToLower(strings.TrimSpace(setting.Value))
			case "vcs.modified":
				provenance.Modified = setting.Value == "true"
				provenance.ModifiedKnown = setting.Value == "true" || setting.Value == "false"
			}
		}
	}
	if revision := strings.ToLower(strings.TrimSpace(buildRevision)); revision != "" {
		provenance.Revision = revision
	}
	switch strings.ToLower(strings.TrimSpace(buildModified)) {
	case "true":
		provenance.Modified = true
		provenance.ModifiedKnown = true
	case "false":
		provenance.Modified = false
		provenance.ModifiedKnown = true
	}
	return provenance
}

func validateCleanBuildProvenance(provenance buildProvenance) error {
	revision := strings.TrimSpace(provenance.Revision)
	if revision == "" {
		return fmt.Errorf("缺少 vcs.revision，无法绑定已审核提交")
	}
	decoded, err := hex.DecodeString(revision)
	if err != nil || len(decoded) != 20 {
		return fmt.Errorf("vcs.revision=%q 不是完整 Git SHA", revision)
	}
	if !provenance.ModifiedKnown {
		return fmt.Errorf("缺少 vcs.modified，无法证明二进制来自干净工作树")
	}
	if provenance.Modified {
		return fmt.Errorf("vcs.modified=true：二进制来自脏工作树")
	}
	return nil
}

func validateInstallPreflight(provenance buildProvenance, target, expectedHead, expectedCurrentSHA256 string) error {
	if err := validateCleanBuildProvenance(provenance); err != nil {
		return err
	}
	expectedHead = strings.ToLower(strings.TrimSpace(expectedHead))
	if expectedHead == "" {
		return fmt.Errorf("缺少 expected-head")
	}
	if strings.ToLower(provenance.Revision) != expectedHead {
		return fmt.Errorf("候选提交 %s 与 expected-head %s 不一致", provenance.Revision, expectedHead)
	}
	if strings.TrimSpace(target) == "" {
		return fmt.Errorf("缺少安装目标")
	}

	expectedCurrentSHA256 = strings.ToLower(strings.TrimSpace(expectedCurrentSHA256))
	if expectedCurrentSHA256 == "absent" {
		if _, err := os.Lstat(target); os.IsNotExist(err) {
			return nil
		} else if err != nil {
			return fmt.Errorf("检查安装目标: %w", err)
		}
		return fmt.Errorf("安装目标 %s 已存在，但预期为 absent", target)
	}
	if decoded, err := hex.DecodeString(expectedCurrentSHA256); err != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("expected-current-sha256 必须是 64 位 SHA-256 或 absent")
	}
	info, err := os.Lstat(target)
	if err != nil {
		return fmt.Errorf("检查安装目标: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("安装目标 %s 不是普通文件", target)
	}
	file, err := os.Open(target)
	if err != nil {
		return fmt.Errorf("读取安装目标: %w", err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("计算安装目标 SHA-256: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("关闭安装目标: %w", closeErr)
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != expectedCurrentSHA256 {
		return fmt.Errorf("安装目标 SHA-256 已漂移: got %s, want %s", actual, expectedCurrentSHA256)
	}
	return nil
}

func cmdInstallPreflight(args []string) error {
	fs := flag.NewFlagSet("install-preflight", flag.ContinueOnError)
	target := fs.String("target", "", "将被替换的已安装二进制")
	expectedHead := fs.String("expected-head", "", "已审核候选的完整 Git SHA")
	expectedCurrent := fs.String("expected-current-sha256", "", "当前已安装二进制 SHA-256；首次安装用 absent")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := validateInstallPreflight(currentBuildProvenance(), *target, *expectedHead, *expectedCurrent); err != nil {
		return fmt.Errorf("安装预检失败: %w", err)
	}
	fmt.Printf("安装预检通过: candidate=%s target_preimage=%s\n",
		strings.TrimSpace(*expectedHead), strings.TrimSpace(*expectedCurrent))
	return nil
}
