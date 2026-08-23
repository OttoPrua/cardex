package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func parseManagerWakeProgramArgumentsCLI(t *testing.T, argv []string) (action, root string) {
	t.Helper()
	if len(argv) < 3 {
		t.Fatalf("ProgramArguments too short for cardex manager-wake: %q", argv)
	}
	if argv[1] != "manager-wake" {
		t.Fatalf("ProgramArguments must invoke manager-wake, got %q", argv)
	}
	action = argv[2]
	switch action {
	case "once", "status", "install", "uninstall":
	default:
		t.Fatalf("cmdManagerWake rejects action %q; want positional once|status|install|uninstall: %q", action, argv)
	}
	fs := flag.NewFlagSet("manager-wake", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	rootFlag := fs.String("root", "", "数据目录")
	if err := fs.Parse(argv[3:]); err != nil {
		t.Fatalf("cmdManagerWake flag parse failed for %q: %v", argv, err)
	}
	if fs.NArg() > 0 {
		t.Fatalf("cmdManagerWake unexpected arguments %q from %q", fs.Args(), argv)
	}
	for _, a := range argv {
		if a == "--once" || a == "-once" {
			t.Fatalf("flag-form once is not cmdManagerWake grammar: %q", argv)
		}
	}
	return action, *rootFlag
}

func assertManagerWakePlistMatchesCLI(t *testing.T, plist, exe, root string) {
	t.Helper()
	argv := managerWakePlistProgramArguments(plist)
	want := managerWakeLaunchdArgv(exe, root)
	if len(argv) != len(want) {
		t.Fatalf("ProgramArguments %q want %q", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("ProgramArguments %q want %q", argv, want)
		}
	}
	action, parsedRoot := parseManagerWakeProgramArgumentsCLI(t, argv)
	if action != "once" || parsedRoot != root {
		t.Fatalf("CLI grammar action=%q root=%q want once %q from %q", action, parsedRoot, root, argv)
	}
}

func TestRenderManagerWakePlistWatchPathsAndInterval(t *testing.T) {
	root := "/tmp/cardex-wake-root"
	plist, err := renderManagerWakePlist("/opt/homebrew/bin/cardex", root, 0, "/tmp/wake.log")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plist, "<string>"+managerWakeLaunchdLabel+"</string>") {
		t.Fatalf("missing label:\n%s", plist)
	}
	assertManagerWakePlistMatchesCLI(t, plist, "/opt/homebrew/bin/cardex", root)
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
	assertManagerWakePlistMatchesCLI(t, plist, exe, root)
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
		if len(args) == 0 {
			return fmt.Errorf("missing args")
		}
		switch args[0] {
		case "load":
			return fmt.Errorf("boom")
		case "print":
			return fmt.Errorf("not_loaded")
		case "bootout", "unload":
			return nil
		default:
			return fmt.Errorf("unexpected %v", args)
		}
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
	managerWakeLaunchctlRun = func(args ...string) error {
		if len(args) == 0 {
			return fmt.Errorf("missing args")
		}
		switch args[0] {
		case "print":
			return fmt.Errorf("not_loaded")
		case "bootout", "unload", "load":
			return nil
		default:
			return fmt.Errorf("unexpected %v", args)
		}
	}
	t.Cleanup(func() {
		managerWakePlistPathFn = origPath
		managerWakeExecutable = origExec
		managerWakeLaunchctlRun = origCtl
	})
}

const observedLaunchctlAbsentUnitDiagnosticUID501 = `Could not find service "com.cardex.manager-wake" in domain for user gui: 501`

func withFakeLaunchctl(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "launchctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func withFakeLaunchctlCombinedOutput(t *testing.T, output string, exit int) {
	t.Helper()
	dir := t.TempDir()
	outPath := filepath.Join(dir, "combined.out")
	if err := os.WriteFile(outPath, []byte(output), 0o644); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\ncat %q >&2\nexit %d\n", outPath, exit)
	if err := os.WriteFile(filepath.Join(dir, "launchctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func assertClosedLaunchctlAdapterError(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s, got nil", want)
	}
	if err.Error() != want {
		t.Fatalf("want %s, got %v", want, err)
	}
	msg := err.Error()
	low := strings.ToLower(msg)
	if strings.Contains(low, "could not find") || strings.Contains(low, "no such") || strings.Contains(low, "not found") ||
		strings.Contains(msg, managerWakeLaunchdLabel) || strings.Contains(msg, launchctlAbsentUnitDiagnostic()) {
		t.Fatalf("must not expose raw launchctl output: %v", err)
	}
}

func withRealManagerWakeLaunchctlRunner(t *testing.T, plistPath string) {
	t.Helper()
	withIsolatedManagerWakeLaunchd(t, plistPath)
	managerWakeLaunchctlRun = defaultManagerWakeLaunchctlRun
}

func TestDefaultManagerWakeLaunchctlRunClassifiesAbsentUnitFromPrintOutput(t *testing.T) {
	withFakeLaunchctlCombinedOutput(t, launchctlAbsentUnitDiagnostic()+"\n", 113)
	err := defaultManagerWakeLaunchctlRun("print", managerWakeLaunchdTarget())
	assertClosedLaunchctlAdapterError(t, err, "not_loaded")
	if !launchctlServiceAbsent(err) {
		t.Fatalf("real runner must classify absent-unit diagnostic, got %v", err)
	}
}

func TestDefaultManagerWakeLaunchctlRunNonAbsentStaysFailClosed(t *testing.T) {
	withFakeLaunchctlCombinedOutput(t, "launchctl print failed: input error\n", 113)
	err := defaultManagerWakeLaunchctlRun("print", managerWakeLaunchdTarget())
	assertClosedLaunchctlAdapterError(t, err, "launchctl_failed")
	if launchctlServiceAbsent(err) {
		t.Fatalf("exit 113 without absent diagnostic must not classify as absent: %v", err)
	}
}

func TestObservedLaunchctlAbsentUnitDiagnosticShape(t *testing.T) {
	got := fmt.Sprintf("Could not find service %q in domain for user gui: %d", managerWakeLaunchdLabel, 501)
	if got != observedLaunchctlAbsentUnitDiagnosticUID501 {
		t.Fatalf("observed print diagnostic shape changed: %q", got)
	}
}

func TestLaunchctlServiceAbsentRejectsBroadSubstrings(t *testing.T) {
	for _, s := range []string{
		"could not find",
		"no such",
		"not found",
		observedLaunchctlAbsentUnitDiagnosticUID501,
		launchctlAbsentUnitDiagnostic(),
	} {
		if launchctlServiceAbsent(fmt.Errorf("%s", s)) {
			t.Fatalf("broad substring %q must not classify as absent", s)
		}
	}
	if !launchctlServiceAbsent(fmt.Errorf("not_loaded")) {
		t.Fatal("closed token not_loaded must remain absent")
	}
}

func TestDefaultManagerWakeLaunchctlRunSurroundingWhitespaceStillAbsent(t *testing.T) {
	withFakeLaunchctlCombinedOutput(t, "\n  "+launchctlAbsentUnitDiagnostic()+"  \n", 113)
	err := defaultManagerWakeLaunchctlRun("print", managerWakeLaunchdTarget())
	assertClosedLaunchctlAdapterError(t, err, "not_loaded")
}

func TestDefaultManagerWakeLaunchctlRunMismatchedOutputFailClosed(t *testing.T) {
	target := managerWakeLaunchdTarget()
	diagnostic := launchctlAbsentUnitDiagnostic()
	wrongTarget := fmt.Sprintf("gui/%d/com.cardex.tick", os.Getuid())
	wrongService := fmt.Sprintf("Could not find service %q in domain for user gui: %d", "com.cardex.tick", os.Getuid())
	cases := []struct {
		name   string
		args   []string
		output string
		exit   int
	}{
		{name: "wrong_exit", args: []string{"print", target}, output: diagnostic + "\n", exit: 1},
		{name: "wrong_operation_bootout", args: []string{"bootout", target}, output: diagnostic + "\n", exit: 113},
		{name: "wrong_operation_load", args: []string{"load", "-w", "/tmp/x.plist"}, output: diagnostic + "\n", exit: 113},
		{name: "wrong_target", args: []string{"print", wrongTarget}, output: diagnostic + "\n", exit: 113},
		{name: "wrong_service", args: []string{"print", target}, output: wrongService + "\n", exit: 113},
		{name: "legacy_could_not_find", args: []string{"print", target}, output: "could not find\n", exit: 113},
		{name: "legacy_no_such", args: []string{"print", target}, output: "no such process\n", exit: 113},
		{name: "legacy_not_found", args: []string{"print", target}, output: "not found\n", exit: 113},
		{name: "extra_semantic_text", args: []string{"print", target}, output: diagnostic + "\nPermission denied\n", exit: 113},
		{name: "empty_output", args: []string{"print", target}, output: "", exit: 113},
		{name: "permission_denied", args: []string{"print", target}, output: "Permission denied\n", exit: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withFakeLaunchctlCombinedOutput(t, tc.output, tc.exit)
			err := defaultManagerWakeLaunchctlRun(tc.args...)
			assertClosedLaunchctlAdapterError(t, err, "launchctl_failed")
			if launchctlServiceAbsent(err) {
				t.Fatalf("%s must remain fail-closed, got %v", tc.name, err)
			}
		})
	}
}

func TestDefaultManagerWakeLaunchctlRunCommandNotFoundFailClosed(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := defaultManagerWakeLaunchctlRun("print", managerWakeLaunchdTarget())
	assertClosedLaunchctlAdapterError(t, err, "launchctl_failed")
	if launchctlServiceAbsent(err) {
		t.Fatalf("command-not-found must not classify as absent: %v", err)
	}
}

func TestUninstallManagerWakeRemovesPlistAfterRealRunnerVerifiesAbsence(t *testing.T) {
	dir := t.TempDir()
	pp := filepath.Join(dir, managerWakeLaunchdLabel+".plist")
	if err := os.WriteFile(pp, []byte("PLIST\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withRealManagerWakeLaunchctlRunner(t, pp)
	withFakeLaunchctl(t, `#!/bin/sh
case "$1" in
bootout)
	exit 0
	;;
print)
	printf '%s\n' '`+launchctlAbsentUnitDiagnostic()+`' >&2
	exit 113
	;;
*)
	exit 1
	;;
esac
`)
	if err := uninstallManagerWakeLaunchd(); err != nil {
		t.Fatalf("verified absence must uninstall, got %v", err)
	}
	if _, err := os.Stat(pp); !os.IsNotExist(err) {
		t.Fatalf("plist must be unlinked after verified absence: %v", err)
	}
}

func TestUninstallManagerWakeNonAbsentPrintLeavesPlist(t *testing.T) {
	dir := t.TempDir()
	pp := filepath.Join(dir, managerWakeLaunchdLabel+".plist")
	if err := os.WriteFile(pp, []byte("PLIST\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withRealManagerWakeLaunchctlRunner(t, pp)
	withFakeLaunchctl(t, `#!/bin/sh
case "$1" in
bootout)
	exit 0
	;;
print)
	printf '%s\n' 'launchctl print failed: input error' >&2
	exit 113
	;;
*)
	exit 1
	;;
esac
`)
	if err := uninstallManagerWakeLaunchd(); err == nil || err.Error() != "launchctl_print_failed" {
		t.Fatalf("non-absent print must fail closed, got %v", err)
	}
	if _, err := os.Stat(pp); err != nil {
		t.Fatalf("failed print must not unlink plist: %v", err)
	}
}

func TestInstallManagerWakeLoadFailureRestoresPriorThroughRealRunner(t *testing.T) {
	root := testRoot(t)
	dir := t.TempDir()
	pp := filepath.Join(dir, managerWakeLaunchdLabel+".plist")
	prior := []byte("PRIOR PLIST\n")
	if err := os.WriteFile(pp, prior, 0o644); err != nil {
		t.Fatal(err)
	}
	withRealManagerWakeLaunchctlRunner(t, pp)
	loads := filepath.Join(t.TempDir(), "loads")
	t.Setenv("CARDEX_TEST_LAUNCHCTL_LOADS", loads)
	withFakeLaunchctl(t, `#!/bin/sh
loads="${CARDEX_TEST_LAUNCHCTL_LOADS}"
case "$1" in
load)
	n=0
	if [ -f "$loads" ]; then
		read n < "$loads" || n=0
	fi
	n=$((n+1))
	echo "$n" > "$loads"
	if [ "$n" -eq 1 ]; then
		printf '%s\n' 'Bootstrapping failed' >&2
		exit 1
	fi
	exit 0
	;;
bootout)
	exit 0
	;;
print)
	n=0
	if [ -f "$loads" ]; then
		read n < "$loads" || n=0
	fi
	if [ "$n" -ge 2 ]; then
		exit 0
	fi
	printf '%s\n' '`+launchctlAbsentUnitDiagnostic()+`' >&2
	exit 113
	;;
*)
	exit 1
	;;
esac
`)
	if err := installManagerWakeLaunchd(root, &ManagerWakeConfig{Enabled: true}); err == nil {
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
}

func TestInstallManagerWakeLoadFailureRemovesNewPlistAfterRealRunnerAbsence(t *testing.T) {
	root := testRoot(t)
	pp := filepath.Join(t.TempDir(), managerWakeLaunchdLabel+".plist")
	withRealManagerWakeLaunchctlRunner(t, pp)
	withFakeLaunchctl(t, `#!/bin/sh
case "$1" in
load)
	printf '%s\n' 'Bootstrapping failed' >&2
	exit 1
	;;
bootout)
	exit 0
	;;
print)
	printf '%s\n' '`+launchctlAbsentUnitDiagnostic()+`' >&2
	exit 113
	;;
*)
	exit 1
	;;
esac
`)
	if err := installManagerWakeLaunchd(root, &ManagerWakeConfig{Enabled: true}); err == nil {
		t.Fatal("load failure must surface")
	} else if err.Error() != "launchctl_load_failed" {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(pp); !os.IsNotExist(err) {
		t.Fatalf("new plist must be removed after verified absence: %v", err)
	}
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
		case "unload", "bootout":
			unloads++
			return nil
		case "print":
			if loads >= 2 {
				return nil
			}
			return fmt.Errorf("not_loaded")
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
		if len(args) == 0 {
			return fmt.Errorf("missing args")
		}
		switch args[0] {
		case "load":
			return fmt.Errorf("boom")
		case "print":
			return fmt.Errorf("not_loaded")
		case "bootout", "unload":
			return nil
		default:
			return fmt.Errorf("unexpected %v", args)
		}
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
		if len(args) == 0 {
			return fmt.Errorf("missing args")
		}
		switch args[0] {
		case "bootout", "unload":
			return fmt.Errorf("boom")
		case "print":
			return nil
		default:
			return fmt.Errorf("unexpected %v", args)
		}
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
	managerWakeLaunchctlRun = func(args ...string) error {
		if len(args) == 0 {
			return fmt.Errorf("missing args")
		}
		switch args[0] {
		case "print":
			return fmt.Errorf("not_loaded")
		case "bootout", "unload", "load":
			return nil
		default:
			return fmt.Errorf("unexpected %v", args)
		}
	}
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

func TestUninstallManagerWakeMissingPlistStillRequiresServiceAbsent(t *testing.T) {
	pp := filepath.Join(t.TempDir(), managerWakeLaunchdLabel+".plist")
	withIsolatedManagerWakeLaunchd(t, pp)
	prints := 0
	bootouts := 0
	managerWakeLaunchctlRun = func(args ...string) error {
		if len(args) == 0 {
			return fmt.Errorf("missing args")
		}
		switch args[0] {
		case "bootout":
			bootouts++
			if !strings.Contains(args[1], managerWakeLaunchdLabel) || !strings.HasPrefix(args[1], "gui/") {
				t.Fatalf("bootout must be domain-and-label, got %v", args)
			}
			return nil
		case "print":
			prints++
			return nil
		default:
			return fmt.Errorf("unexpected %v", args)
		}
	}
	if err := uninstallManagerWakeLaunchd(); err == nil || err.Error() != "launchctl_unload_failed" {
		t.Fatalf("loaded service without plist must not report success, got %v", err)
	}
	if bootouts == 0 || prints == 0 {
		t.Fatalf("must bootout and print, bootouts=%d prints=%d", bootouts, prints)
	}
}

func TestRollbackUnloadFailureDoesNotUnlinkOrReportSuccess(t *testing.T) {
	dir := t.TempDir()
	pp := filepath.Join(dir, managerWakeLaunchdLabel+".plist")
	if err := os.WriteFile(pp, []byte("NEW PLIST\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withIsolatedManagerWakeLaunchd(t, pp)
	managerWakeLaunchctlRun = func(args ...string) error {
		if len(args) == 0 {
			return fmt.Errorf("missing args")
		}
		switch args[0] {
		case "bootout", "unload":
			return fmt.Errorf("boom")
		case "print":
			return nil
		default:
			return fmt.Errorf("unexpected %v", args)
		}
	}
	if err := restoreManagerWakeLaunchd(pp, nil, false); err == nil || err.Error() != "launchctl_unload_failed" {
		t.Fatalf("unload failure must surface, got %v", err)
	}
	if _, err := os.Stat(pp); err != nil {
		t.Fatalf("failed unload must not unlink plist: %v", err)
	}
}

func TestRollbackRestoresPriorArgvBeforeSuccess(t *testing.T) {
	root := testRoot(t)
	dir := t.TempDir()
	pp := filepath.Join(dir, managerWakeLaunchdLabel+".plist")
	prior, err := renderManagerWakePlist("/prior/cardex", "/prior/root", managerWakeWatchdogSec, "/tmp/prior.log")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pp, []byte(prior), 0o644); err != nil {
		t.Fatal(err)
	}
	withIsolatedManagerWakeLaunchd(t, pp)
	loads := 0
	managerWakeLaunchctlRun = func(args ...string) error {
		if len(args) == 0 {
			return fmt.Errorf("missing args")
		}
		switch args[0] {
		case "bootout", "unload":
			return fmt.Errorf("not_loaded")
		case "print":
			if loads >= 2 {
				return nil
			}
			return fmt.Errorf("not_loaded")
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
	if err := installManagerWakeLaunchd(root, &ManagerWakeConfig{Enabled: true}); err == nil {
		t.Fatal("load failure must surface")
	}
	got, err := os.ReadFile(pp)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != prior {
		t.Fatalf("prior argv not restored: %q", got)
	}
	want := managerWakePlistProgramArguments(prior)
	if !sameStringSlice(managerWakePlistProgramArguments(string(got)), want) {
		t.Fatalf("restored argv %q want %q", managerWakePlistProgramArguments(string(got)), want)
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
	exe, err := resolveManagerWakeExecutable()
	if err != nil {
		t.Fatal(err)
	}
	assertManagerWakePlistMatchesCLI(t, string(got), exe, root)
	if _, err := os.Stat(pp + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("durable write must not leave tmp")
	}
}
