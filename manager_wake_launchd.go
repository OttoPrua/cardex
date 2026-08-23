package main

import (
	"encoding/xml"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	managerWakeLaunchdLabel = "com.cardex.manager-wake"
)

var (
	managerWakePlistPathFn  = defaultManagerWakeLaunchdPlistPath
	managerWakeExecutable   = os.Executable
	managerWakeLaunchctlRun = defaultManagerWakeLaunchctlRun
	managerWakeWritePlist   = atomicWriteSync
)

func defaultManagerWakeLaunchdPlistPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "LaunchAgents", managerWakeLaunchdLabel+".plist")
}

func defaultManagerWakeLaunchctlRun(args ...string) error {
	// launchctl writes the absent-unit diagnostic to stdout/stderr. Run()
	// would leave only "exit status 113" and misclassify a normal absence.
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err == nil {
		return nil
	}
	if launchctlServiceAbsent(fmt.Errorf("%s", out)) {
		return fmt.Errorf("not_loaded")
	}
	return fmt.Errorf("launchctl_failed")
}

func managerWakeLaunchdPlistPath() string {
	return managerWakePlistPathFn()
}

func plistXMLText(s string) (string, error) {
	for _, r := range s {
		if !plistXMLCharOK(r) {
			return "", fmt.Errorf("plist_xml_invalid")
		}
	}
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return "", fmt.Errorf("plist_xml_invalid")
	}
	if s != "" && b.Len() == 0 {
		return "", fmt.Errorf("plist_xml_invalid")
	}
	return b.String(), nil
}

func plistXMLCharOK(r rune) bool {
	switch r {
	case 0x09, 0x0A, 0x0D:
		return true
	}
	if r >= 0x20 && r <= 0xD7FF {
		return true
	}
	if r >= 0xE000 && r <= 0xFFFD {
		return true
	}
	return r >= 0x10000 && r <= 0x10FFFF
}

func renderManagerWakePlist(exe, root string, watchdogSec int, logOut string) (string, error) {
	_ = watchdogSec
	outbox := managerWakeOutboxPath(root)
	label, err := plistXMLText(managerWakeLaunchdLabel)
	if err != nil {
		return "", err
	}
	outboxXML, err := plistXMLText(outbox)
	if err != nil {
		return "", err
	}
	logXML, err := plistXMLText(logOut)
	if err != nil {
		return "", err
	}
	argv := managerWakeLaunchdArgv(exe, root)
	argvXML := make([]string, 0, len(argv))
	for _, a := range argv {
		s, err := plistXMLText(a)
		if err != nil {
			return "", err
		}
		argvXML = append(argvXML, s)
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>`)
	b.WriteString(label)
	b.WriteString(`</string>
    <key>ProgramArguments</key>
    <array>
`)
	for _, s := range argvXML {
		b.WriteString("        <string>")
		b.WriteString(s)
		b.WriteString("</string>\n")
	}
	b.WriteString(`    </array>
    <key>WatchPaths</key>
    <array>
        <string>`)
	b.WriteString(outboxXML)
	b.WriteString(`</string>
    </array>
    <key>StartInterval</key><integer>`)
	b.WriteString(strconv.Itoa(managerWakeWatchdogSec))
	b.WriteString(`</integer>
    <key>RunAtLoad</key><true/>
    <key>StandardOutPath</key><string>`)
	b.WriteString(logXML)
	b.WriteString(`</string>
    <key>StandardErrorPath</key><string>`)
	b.WriteString(logXML)
	b.WriteString(`</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
    </dict>
</dict>
</plist>
`)
	return b.String(), nil
}

func managerWakeLaunchdArgv(exe, root string) []string {
	return []string{exe, "manager-wake", "once", "--root", root}
}

func managerWakePlistProgramArguments(plist string) []string {
	var args []string
	in := false
	for _, line := range strings.Split(plist, "\n") {
		trim := strings.TrimSpace(line)
		if strings.Contains(trim, "<key>ProgramArguments</key>") {
			in = true
			continue
		}
		if !in {
			continue
		}
		if strings.Contains(trim, "</array>") {
			break
		}
		if strings.HasPrefix(trim, "<string>") && strings.HasSuffix(trim, "</string>") {
			raw := strings.TrimSuffix(strings.TrimPrefix(trim, "<string>"), "</string>")
			args = append(args, html.UnescapeString(raw))
		}
	}
	return args
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
				raw := strings.TrimSuffix(strings.TrimPrefix(trim, "<string>"), "</string>")
				paths = append(paths, html.UnescapeString(raw))
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

func managerWakeLaunchdTarget() string {
	return fmt.Sprintf("gui/%d/%s", os.Getuid(), managerWakeLaunchdLabel)
}

func launchctlServiceAbsent(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not_loaded") ||
		strings.Contains(s, "could not find") ||
		strings.Contains(s, "no such") ||
		strings.Contains(s, "not found")
}

func managerWakeUnloadService() error {
	target := managerWakeLaunchdTarget()
	if err := managerWakeLaunchctlRun("bootout", target); err != nil && !launchctlServiceAbsent(err) {
		return fmt.Errorf("launchctl_unload_failed")
	}
	return nil
}

func managerWakeServiceLoaded() (bool, error) {
	target := managerWakeLaunchdTarget()
	err := managerWakeLaunchctlRun("print", target)
	if err == nil {
		return true, nil
	}
	if launchctlServiceAbsent(err) {
		return false, nil
	}
	return false, fmt.Errorf("launchctl_print_failed")
}

func restoreManagerWakeLaunchd(pp string, prior []byte, priorOK bool) error {
	if priorOK {
		if err := managerWakeWritePlist(pp, prior); err != nil {
			return err
		}
		if err := managerWakeUnloadService(); err != nil {
			return err
		}
		if err := managerWakeLaunchctlRun("load", "-w", pp); err != nil {
			return err
		}
		loaded, err := managerWakeServiceLoaded()
		if err != nil {
			return err
		}
		if !loaded {
			return fmt.Errorf("launchd_restore_unverified")
		}
		got, err := os.ReadFile(pp)
		if err != nil {
			return err
		}
		if !sameStringSlice(managerWakePlistProgramArguments(string(got)), managerWakePlistProgramArguments(string(prior))) {
			return fmt.Errorf("launchd_restore_unverified")
		}
		return nil
	}
	if err := managerWakeUnloadService(); err != nil {
		return err
	}
	loaded, err := managerWakeServiceLoaded()
	if err != nil {
		return err
	}
	if loaded {
		return fmt.Errorf("launchctl_unload_failed")
	}
	return durableUnlinkFile(pp)
}

func resolveManagerWakeExecutable() (string, error) {
	exe, err := managerWakeExecutable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}

func installManagerWakeLaunchd(root string, mw *ManagerWakeConfig) error {
	if !managerWakeEnabled(mw) {
		return fmt.Errorf("manager_wake_disabled")
	}
	exe, err := resolveManagerWakeExecutable()
	if err != nil {
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
	content, err := renderManagerWakePlist(exe, root, managerWakeWatchdogSec, logOut)
	if err != nil {
		return err
	}
	var prior []byte
	priorOK := false
	if data, err := os.ReadFile(pp); err == nil {
		prior = append([]byte(nil), data...)
		priorOK = true
	}
	if err := managerWakeWritePlist(pp, []byte(content)); err != nil {
		return err
	}
	_ = managerWakeUnloadService()
	if err := managerWakeLaunchctlRun("load", "-w", pp); err != nil {
		if rerr := restoreManagerWakeLaunchd(pp, prior, priorOK); rerr != nil {
			return fmt.Errorf("launchd_restore_failed")
		}
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
	plistMissing := false
	if _, err := os.Stat(pp); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		plistMissing = true
	}
	if err := managerWakeUnloadService(); err != nil {
		return err
	}
	loaded, err := managerWakeServiceLoaded()
	if err != nil {
		return err
	}
	if loaded {
		return fmt.Errorf("launchctl_unload_failed")
	}
	if !plistMissing {
		if err := durableUnlinkFile(pp); err != nil {
			return err
		}
	}
	fmt.Println("已卸载 manager-wake launchd:", pp)
	return nil
}
