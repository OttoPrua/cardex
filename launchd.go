package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
)

const (
	launchdLabel            = "com.cardex.tick"
	cardexTickOpenFileLimit = uint64(65536)
)

// legacyLaunchdLabel 是 BD-44 改名前的 label。本文件**不自动卸载**它——launchd 的卸载属
// cutover 操作员的动作(与 migrate 同一窗口),代码擅自 launchctl unload 会在人还没准备好时
// 停掉正在跑的调度。但装新定时器时必须**报**它还在:两份 plist 同时 load 会有两个 tick 各自
// 起 cardex/claudego 二进制、各自打各自的数据根,双调度器抢同一队列锁是真事故。
const legacyLaunchdLabel = "com.claudego.tick"

const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>run</string>
        <string>--quiet</string>
        <string>--root</string>
        <string>%s</string>
    </array>
    <key>StartInterval</key><integer>%d</integer>
    <key>RunAtLoad</key><true/>
    <key>SoftResourceLimits</key>
    <dict>
        <key>NumberOfFiles</key><integer>%d</integer>
    </dict>
    <key>StandardOutPath</key><string>%s</string>
    <key>StandardErrorPath</key><string>%s</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
		<key>CARDEX_REQUIRE_OWNER_ROUTING</key><string>%s</string>
    </dict>
</dict>
</plist>
`

var launchdOpenFileLimitRE = regexp.MustCompile(`(?s)<key>\s*SoftResourceLimits\s*</key>\s*<dict>.*?<key>\s*NumberOfFiles\s*</key>\s*<integer>\s*([0-9]+)\s*</integer>`)

func renderLaunchdPlist(exe, root string, intervalSec int, logOut string, requireOwner ...bool) string {
	required := "0"
	if len(requireOwner) > 0 && requireOwner[0] {
		required = "1"
	}
	return fmt.Sprintf(plistTemplate, launchdLabel, exe, root, intervalSec, cardexTickOpenFileLimit, logOut, logOut, required)
}

func launchdOpenFileLimit(data []byte) (uint64, error) {
	m := launchdOpenFileLimitRE.FindSubmatch(data)
	if len(m) != 2 {
		return 0, fmt.Errorf("%s 缺少 SoftResourceLimits.NumberOfFiles", launchdLabel)
	}
	limit, err := strconv.ParseUint(string(m[1]), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s 的 NumberOfFiles 非法: %w", launchdLabel, err)
	}
	return limit, nil
}

func installedLaunchdOpenFileLimit() (uint64, error) {
	pp, err := plistPath()
	if err != nil {
		return 0, err
	}
	data, err := os.ReadFile(pp)
	if err != nil {
		return 0, err
	}
	return launchdOpenFileLimit(data)
}

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

// legacyPlistPath 是旧 label 的 plist 路径,只用于"还在不在"的探测与告警,不做任何写/删。
func legacyPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", legacyLaunchdLabel+".plist"), nil
}

func installLaunchd(root string, intervalSec int) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	pp, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(pp), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(logsDir(root), 0o755); err != nil {
		return err
	}
	logOut := filepath.Join(logsDir(root), "launchd.log")
	cfg, err := loadConfig(root)
	if err != nil {
		return err
	}
	content := renderLaunchdPlist(exe, root, intervalSec, logOut, cfg.OwnerRoutingEnforced)
	if err := atomicWrite(pp, []byte(content)); err != nil {
		return err
	}
	_ = exec.Command("launchctl", "unload", pp).Run()
	if out, err := exec.Command("launchctl", "load", "-w", pp).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl load 失败: %v\n%s", err, out)
	}
	fmt.Printf("已安装 launchd 定时器（每 %d 秒）: %s\n二进制: %s\n数据目录: %s\n", intervalSec, pp, exe, root)
	fmt.Println("提示: 如果之后移动或重新编译了二进制到其他路径，需要重新运行 install-launchd。")
	warnLegacyLaunchd()
	return nil
}

// warnLegacyLaunchd 在旧 label 的 plist 仍在时打一条 stderr 告警。只报不动:卸载旧定时器
// 是 cutover 操作员在 migrate 窗口里的动作(见 cmdMigrate 的后续步骤清单)。
func warnLegacyLaunchd() {
	lp, err := legacyPlistPath()
	if err != nil {
		return
	}
	if _, err := os.Stat(lp); err != nil {
		return
	}
	fmt.Fprintf(os.Stderr,
		"警告: 旧定时器 %s 仍在（%s）。两份 plist 同时 load = 两个调度器抢同一队列，请在 cutover 窗口手工卸掉：\n  launchctl unload %s && rm %s\n",
		legacyLaunchdLabel, lp, lp, lp)
}

func uninstallLaunchd() error {
	pp, err := plistPath()
	if err != nil {
		return err
	}
	_ = exec.Command("launchctl", "unload", pp).Run()
	if err := os.Remove(pp); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Println("已卸载 launchd 定时器:", pp)
	return nil
}
