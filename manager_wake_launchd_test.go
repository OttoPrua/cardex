package main

import (
	"strings"
	"testing"
)

func TestRenderManagerWakePlistWatchPathsAndInterval(t *testing.T) {
	root := "/tmp/cardex-wake-root"
	plist := renderManagerWakePlist("/opt/homebrew/bin/cardex", root, 0, "/tmp/wake.log")
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
	plist7 := renderManagerWakePlist("/opt/homebrew/bin/cardex", root, 7, "/tmp/wake.log")
	sec7, err := managerWakePlistStartInterval(plist7)
	if err != nil || sec7 != managerWakeWatchdogSec {
		t.Fatalf("hard watchdog ignored 7: interval=%d err=%v", sec7, err)
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
