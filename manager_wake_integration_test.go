package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeWakeConfig(t *testing.T, root, bin, project, thread, subID string, enabled bool) {
	t.Helper()
	cfg := defaultConfig("claude")
	cfg.ManagerWake = testWakeCfg(bin, project, thread, subID)
	cfg.ManagerWake.Enabled = enabled
	if err := saveConfig(root, cfg); err != nil {
		t.Fatal(err)
	}
}

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := fn()
	_ = w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	return buf.String(), runErr
}

func TestCmdManagerWakeConfiguredPathDeltaZeroAndReadback(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	writeWakeConfig(t, root, bin, "wake-proj", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "mgr", true)

	if err := cmdManagerWake([]string{"once", "-root", root}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("delta=0 watchdog scan queued: %s", data)
	}
	if err := cmdManagerWake([]string{"once", "-root", root}); err != nil {
		t.Fatal(err)
	}
	if queueThreadCount(logPath) != 0 {
		t.Fatalf("second 1200s-class scan queued: %d", queueThreadCount(logPath))
	}

	tk := heldCommittedTask(t, root, "wake-proj", "cli-once")
	if rows := countWakeRows(t, root); len(rows) != 1 || rows[0].TaskID != tk.ID {
		t.Fatalf("production hold must already have a row before once: %+v", rows)
	}
	if err := cmdManagerWake([]string{"once", "-root", root}); err != nil {
		t.Fatal(err)
	}
	if queueThreadCount(logPath) != 1 {
		t.Fatalf("configured once must queue exactly once, got %d", queueThreadCount(logPath))
	}
	if err := cmdManagerWake([]string{"once", "-root", root}); err != nil {
		t.Fatal(err)
	}
	if queueThreadCount(logPath) != 1 {
		t.Fatalf("delta=0 after delivery must not queue again, got %d", queueThreadCount(logPath))
	}

	out, err := captureStdout(t, func() error {
		return cmdManagerWake([]string{"status", "-root", root})
	})
	if err != nil {
		t.Fatal(err)
	}
	var rb map[string]any
	if err := json.Unmarshal([]byte(out), &rb); err != nil {
		t.Fatalf("status json: %v %s", err, out)
	}
	if rb["watchdog_sec"] != float64(managerWakeWatchdogSec) {
		t.Fatalf("status watchdog=%v", rb["watchdog_sec"])
	}
	if rb["enabled"] != true {
		t.Fatalf("status enabled=%v", rb["enabled"])
	}
	low := strings.ToLower(out)
	for _, bad := range []string{"prompt", "token=", "secret=", "authorization", "api_key", "password="} {
		if strings.Contains(low, bad) {
			t.Fatalf("status leaked %q: %s", bad, out)
		}
	}
}

func TestCmdManagerWakeDefaultDisabledAndInvalidFailClosed(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	cfg := defaultConfig("claude")
	if cfg.ManagerWake != nil {
		t.Fatal("manager wake must stay disabled by default")
	}
	if err := saveConfig(root, cfg); err != nil {
		t.Fatal(err)
	}
	_ = heldCommittedTask(t, root, "wake-proj", "disabled-default")
	if rows := countWakeRows(t, root); len(rows) != 0 {
		t.Fatalf("disabled default must not append wake rows: %+v", rows)
	}
	if err := cmdManagerWake([]string{"once", "-root", root}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatal("disabled default must not queue")
	}
	if err := cmdManagerWake([]string{"install", "-root", root}); err == nil {
		t.Fatal("install while disabled must fail closed")
	}

	cfg.ManagerWake = &ManagerWakeConfig{Enabled: true, CodexBin: bin}
	if err := saveConfig(root, cfg); err != nil {
		t.Fatal(err)
	}
	if err := cmdManagerWake([]string{"install", "-root", root}); err == nil {
		t.Fatal("enabled incomplete config must fail closed on install")
	}
	doc, _ := captureStdout(t, func() error { return cmdDoctor([]string{"-root", root}) })
	if !strings.Contains(doc, "manager-wake") {
		t.Fatalf("doctor must surface manager-wake: %s", doc)
	}
}

func TestCmdManagerWakeInstallLaunchdWatchPathsAndInterval(t *testing.T) {
	root := testRoot(t)
	bin, _ := fakeCodexQueueBin(t, 0)
	writeWakeConfig(t, root, bin, "wake-proj", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", "mgr", true)
	pp := filepath.Join(t.TempDir(), "com.cardex.manager-wake.plist")
	withIsolatedManagerWakeLaunchd(t, pp)
	managerWakeLaunchctlRun = func(args ...string) error { return nil }
	if err := cmdManagerWake([]string{"install", "-root", root}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(pp)
	if err != nil {
		t.Fatal(err)
	}
	plist := string(data)
	if !strings.Contains(plist, "<string>manager-wake</string>") || !strings.Contains(plist, "<string>--once</string>") {
		t.Fatalf("plist missing once command: %s", plist)
	}
	if paths := managerWakePlistWatchPaths(plist); len(paths) != 1 || paths[0] != managerWakeOutboxPath(root) {
		t.Fatalf("WatchPaths=%v", paths)
	}
	sec, err := managerWakePlistStartInterval(plist)
	if err != nil || sec != managerWakeWatchdogSec {
		t.Fatalf("StartInterval=%d err=%v want %d", sec, err, managerWakeWatchdogSec)
	}
	if err := cmdManagerWake([]string{"uninstall", "-root", root}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pp); !os.IsNotExist(err) {
		t.Fatal("uninstall must remove plist")
	}
}
