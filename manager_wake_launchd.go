package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	managerWakeLaunchdLabel  = "com.cardex.manager-wake"
	managerWakePlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>manager-wake</string>
        <string>--once</string>
        <string>--root</string>
        <string>%s</string>
    </array>
    <key>WatchPaths</key>
    <array>
        <string>%s</string>
    </array>
    <key>StartInterval</key><integer>%d</integer>
    <key>RunAtLoad</key><true/>
    <key>StandardOutPath</key><string>%s</string>
    <key>StandardErrorPath</key><string>%s</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
    </dict>
</dict>
</plist>
`
)

func managerWakeLaunchdPlistPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "LaunchAgents", managerWakeLaunchdLabel+".plist")
}

func renderManagerWakePlist(exe, root string, watchdogSec int, logOut string) string {
	_ = watchdogSec
	outbox := managerWakeOutboxPath(root)
	return fmt.Sprintf(managerWakePlistTemplate, managerWakeLaunchdLabel, exe, root, outbox, managerWakeWatchdogSec, logOut, logOut)
}

func managerWakePlistWatchPaths(plist string) []string {
	var paths []string
	in := false
	for _, line := range strings.Split(plist, "\n") {
		trim := strings.TrimSpace(line)
		if strings.Contains(trim, "<key>WatchPaths</key>") {
			in = true
			continue
		}
		if in {
			if strings.Contains(trim, "</array>") {
				break
			}
			if strings.HasPrefix(trim, "<string>") && strings.HasSuffix(trim, "</string>") {
				paths = append(paths, strings.TrimSuffix(strings.TrimPrefix(trim, "<string>"), "</string>"))
			}
		}
	}
	return paths
}

func managerWakePlistStartInterval(plist string) (int, error) {
	const key = "<key>StartInterval</key><integer>"
	i := strings.Index(plist, key)
	if i < 0 {
		return 0, fmt.Errorf("missing StartInterval")
	}
	rest := plist[i+len(key):]
	j := strings.Index(rest, "</integer>")
	if j < 0 {
		return 0, fmt.Errorf("missing StartInterval value")
	}
	return strconv.Atoi(strings.TrimSpace(rest[:j]))
}

func installManagerWakeLaunchd(root string, mw *ManagerWakeConfig) error {
	if !managerWakeEnabled(mw) {
		return fmt.Errorf("manager_wake_disabled")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	pp := managerWakeLaunchdPlistPath()
	if pp == "" {
		return fmt.Errorf("launchd_path_unresolved")
	}
	if err := os.MkdirAll(filepath.Dir(pp), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(logsDir(root), 0o755); err != nil {
		return err
	}
	logOut := filepath.Join(logsDir(root), "manager-wake.log")
	content := renderManagerWakePlist(exe, root, managerWakeWatchdogSec, logOut)
	if err := atomicWrite(pp, []byte(content)); err != nil {
		return err
	}
	_ = exec.Command("launchctl", "unload", pp).Run()
	if err := exec.Command("launchctl", "load", "-w", pp).Run(); err != nil {
		return fmt.Errorf("launchctl_load_failed")
	}
	fmt.Printf("已安装 manager-wake launchd（WatchPaths + StartInterval=%d）: %s\n", managerWakeWatchdogSec, pp)
	return nil
}

func uninstallManagerWakeLaunchd() error {
	pp := managerWakeLaunchdPlistPath()
	if pp == "" {
		return fmt.Errorf("launchd_path_unresolved")
	}
	_ = exec.Command("launchctl", "unload", pp).Run()
	if err := os.Remove(pp); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Println("已卸载 manager-wake launchd:", pp)
	return nil
}
