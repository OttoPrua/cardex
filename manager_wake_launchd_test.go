package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderManagerWakePlistWatchPathsAndInterval(t *testing.T) {
	root := "/tmp/cardex-wake-root"
	plist, err := renderManagerWakePlist("/opt/homebrew/bin/cardex", root, 0, "/tmp/wake.log")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plist, "<string>"+managerWakeLaunchdLabel+"</string>") {
		t.Fatalf("missing label:\n%s", plist)
	}
	if !strings.Contains(plist, "<string>manager-wake</string>") || !strings.Contains(plist, "<string>--once</string>") {
		t.Fatalf("missing command:\n%s", plist)
	}
	paths := managerWakePlistWatchPaths(plist)
	wantPath := managerWakeOutboxPath(root)
	if len(paths) != 1 || paths[0] != wantPath {
		t.Fatalf("WatchPaths=%v want [%s]", paths, wantPath)
	}
	sec, err := managerWakePlistStartInterval(plist)
	if err != nil || sec != managerWakeWatchdogSec {
		t.Fatalf("StartInterval=%d err=%v", sec, err)
	}
	if !strings.Contains(plist, "<key>RunAtLoad</key><true/>") {
		t.Fatal("missing RunAtLoad")
	}
	if strings.Contains(plist, "com.cardex.tick") {
		t.Fatal("manager-wake plist must not bind the tick unit")
	}
	plist7, err := renderManagerWakePlist("/opt/homebrew/bin/cardex", root, 7, "/tmp/wake.log")
	if err != nil {
		t.Fatal(err)
	}
	sec7, err := managerWakePlistStartInterval(plist7)
	if err != nil || sec7 != managerWakeWatchdogSec {
		t.Fatalf("hard watchdog ignored 7: interval=%d err=%v", sec7, err)
	}
}

func TestRenderManagerWakePlistXMLEscapesText(t *testing.T) {
	exe := `/opt/bin/cardex & "tool"`
	root := `/tmp/cardex & root/<wake>`
	logOut := `/tmp/wake & <err>.log`
	plist, err := renderManagerWakePlist(exe, root, 0, logOut)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plist, `cardex & root`) || strings.Contains(plist, `<wake>`) ||
		strings.Contains(plist, `cardex & "tool"`) || strings.Contains(plist, `wake & <err>`) {
		t.Fatalf("unescaped XML special characters:\n%s", plist)
	}
	exeXML, err := plistXMLText(exe)
	if err != nil {
		t.Fatal(err)
	}
	rootXML, err := plistXMLText(root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plist, exeXML) || !strings.Contains(plist, rootXML) {
		t.Fatalf("escaped exe/root missing:\n%s", plist)
	}
	outbox := managerWakeOutboxPath(root)
	outboxXML, err := plistXMLText(outbox)
	if err != nil {
		t.Fatal(err)
	}
	logXML, err := plistXMLText(logOut)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plist, outboxXML) || !strings.Contains(plist, logXML) {
		t.Fatalf("escaped outbox/log missing:\n%s", plist)
	}
	paths := managerWakePlistWatchPaths(plist)
	if len(paths) != 1 || paths[0] != outbox {
		t.Fatalf("WatchPaths unescape=%v want [%s]", paths, outbox)
	}
}

func TestRenderManagerWakePlistControlCharsFailClosed(t *testing.T) {
	if _, err := renderManagerWakePlist("/opt/bin/cardex", "/tmp/cardex\x01root", 0, "/tmp/wake.log"); err == nil {
		t.Fatal("XML control characters must fail closed")
	} else if err.Error() != "plist_xml_invalid" {
		t.Fatalf("err=%v", err)
	}
	if got, err := renderManagerWakePlist("/opt/bin/cardex", "/tmp/cardex\x00root", 0, "/tmp/wake.log"); err == nil {
		t.Fatalf("NUL must fail closed, got %q", got)
	} else if err.Error() != "plist_xml_invalid" {
		t.Fatalf("NUL err=%v", err)
	}
	if text, err := plistXMLText("ok\x07path"); err == nil || text != "" {
		t.Fatalf("control char escape must not yield an empty path: text=%q err=%v", text, err)
	}
}

func TestInstallManagerWakeRestoreErrorSurfaces(t *testing.T) {
	root := testRoot(t)
	dir := t.TempDir()
	pp := filepath.Join(dir, managerWakeLaunchdLabel+".plist")
	prior := []byte("PRIOR PLIST\n")
	if err := os.WriteFile(pp, prior, 0o644); err != nil {
		t.Fatal(err)
	}
	withIsolatedManagerWakeLaunchd(t, pp)
	managerWakeLaunchctlRun = func(args ...string) error {
		if len(args) > 0 && args[0] == "load" {
			return fmt.Errorf("boom")
		}
		return nil
	}
	if err := installManagerWakeLaunchd(root, &ManagerWakeConfig{Enabled: true}); err == nil {
		t.Fatal("restore load failure must surface")
	} else if err.Error() != "launchd_restore_failed" {
		t.Fatalf("err=%v", err)
	}
}

func TestTickPlistUnchangedNoWatchPaths(t *testing.T) {
	got := renderLaunchdPlist("/opt/homebrew/bin/cardex", "/tmp/cardex-root", 300, "/tmp/cardex.log")
	if strings.Contains(got, "WatchPaths") {
		t.Fatalf("tick plist must not gain WatchPaths:\n%s", got)
	}
	if strings.Contains(got, "manager-wake") {
		t.Fatalf("tick plist must not invoke manager-wake:\n%s", got)
	}
	if !strings.Contains(got, "<string>run</string>") {
		t.Fatal("tick plist lost run")
	}
}

func TestInstallManagerWakeRefusedWhenDisabled(t *testing.T) {
	root := testRoot(t)
	if err := installManagerWakeLaunchd(root, nil); err == nil {
		t.Fatal("disabled config must refuse install")
	}
	if err := installManagerWakeLaunchd(root, &ManagerWakeConfig{Enabled: false}); err == nil {
		t.Fatal("enabled=false must refuse install")
	}
}

func withIsolatedManagerWakeLaunchd(t *testing.T, plistPath string) {
	t.Helper()
	origPath := managerWakePlistPathFn
	origExec := managerWakeExecutable
	origCtl := managerWakeLaunchctlRun
	exe := filepath.Join(t.TempDir(), "cardex")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	managerWakePlistPathFn = func() string { return plistPath }
	managerWakeExecutable = func() (string, error) { return exe, nil }
	t.Cleanup(func() {
		managerWakePlistPathFn = origPath
		managerWakeExecutable = origExec
		managerWakeLaunchctlRun = origCtl
	})
}

func TestInstallManagerWakeDurableWriteAndLoadFailureRestoresPrior(t *testing.T) {
	root := testRoot(t)
	dir := t.TempDir()
	pp := filepath.Join(dir, managerWakeLaunchdLabel+".plist")
	prior := []byte("PRIOR PLIST & <unit>\n")
	if err := os.WriteFile(pp, prior, 0o644); err != nil {
		t.Fatal(err)
	}
	withIsolatedManagerWakeLaunchd(t, pp)
	loads := 0
	unloads := 0
	managerWakeLaunchctlRun = func(args ...string) error {
		if len(args) == 0 {
			return fmt.Errorf("missing args")
		}
		switch args[0] {
		case "unload":
			unloads++
			return nil
		case "load":
			loads++
			if loads == 1 {
				return fmt.Errorf("boom")
			}
			return nil
		default:
			return fmt.Errorf("unexpected %v", args)
		}
	}
	mw := &ManagerWakeConfig{Enabled: true, CodexBin: "codex"}
	if err := installManagerWakeLaunchd(root, mw); err == nil {
		t.Fatal("load failure must surface")
	} else if err.Error() != "launchctl_load_failed" {
		t.Fatalf("err=%v", err)
	}
	got, err := os.ReadFile(pp)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(prior) {
		t.Fatalf("prior plist not restored: %q", got)
	}
	if loads < 2 {
		t.Fatalf("restore must reload prior unit, loads=%d unloads=%d", loads, unloads)
	}
	if _, err := os.Stat(pp + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("durable write must not leave tmp")
	}
}

func TestInstallManagerWakeLoadFailureRemovesNewPlistWhenNoPrior(t *testing.T) {
	root := testRoot(t)
	pp := filepath.Join(t.TempDir(), managerWakeLaunchdLabel+".plist")
	withIsolatedManagerWakeLaunchd(t, pp)
	managerWakeLaunchctlRun = func(args ...string) error {
		if len(args) > 0 && args[0] == "load" {
			return fmt.Errorf("boom")
		}
		return nil
	}
	if err := installManagerWakeLaunchd(root, &ManagerWakeConfig{Enabled: true}); err == nil {
		t.Fatal("load failure must surface")
	}
	if _, err := os.Stat(pp); !os.IsNotExist(err) {
		t.Fatalf("new plist must be removed when no prior unit: %v", err)
	}
}

func TestUninstallManagerWakeUnloadFailureSurfaces(t *testing.T) {
	pp := filepath.Join(t.TempDir(), managerWakeLaunchdLabel+".plist")
	if err := os.WriteFile(pp, []byte("PLIST\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withIsolatedManagerWakeLaunchd(t, pp)
	managerWakeLaunchctlRun = func(args ...string) error {
		if len(args) > 0 && args[0] == "unload" {
			return fmt.Errorf("boom")
		}
		return nil
	}
	if err := uninstallManagerWakeLaunchd(); err == nil {
		t.Fatal("unload failure must surface")
	} else if err.Error() != "launchctl_unload_failed" {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(pp); err != nil {
		t.Fatalf("failed unload must not unlink plist: %v", err)
	}
}

func TestUninstallManagerWakeDurablePlistUnlink(t *testing.T) {
	dir := t.TempDir()
	pp := filepath.Join(dir, managerWakeLaunchdLabel+".plist")
	if err := os.WriteFile(pp, []byte("PLIST\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withIsolatedManagerWakeLaunchd(t, pp)
	managerWakeLaunchctlRun = func(args ...string) error { return nil }
	var synced []string
	orig := syncDirAfterRename
	t.Cleanup(func() { syncDirAfterRename = orig })
	syncDirAfterRename = func(d string) error {
		synced = append(synced, d)
		return orig(d)
	}
	if err := uninstallManagerWakeLaunchd(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pp); !os.IsNotExist(err) {
		t.Fatalf("plist must be unlinked: %v", err)
	}
	if !containsString(synced, dir) {
		t.Fatalf("plist unlink must sync containing dir: %v", synced)
	}
}

func TestInstallManagerWakeSuccessDurablePlist(t *testing.T) {
	root := testRoot(t)
	pp := filepath.Join(t.TempDir(), managerWakeLaunchdLabel+".plist")
	withIsolatedManagerWakeLaunchd(t, pp)
	managerWakeLaunchctlRun = func(args ...string) error { return nil }
	if err := installManagerWakeLaunchd(root, &ManagerWakeConfig{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(pp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "<string>manager-wake</string>") {
		t.Fatalf("installed plist missing command:\n%s", got)
	}
	if _, err := os.Stat(pp + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("durable write must not leave tmp")
	}
}
