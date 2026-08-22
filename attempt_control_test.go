package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func withSchedulerLock(t *testing.T, root string) {
	t.Helper()
	if !acquireLock(root, 24*time.Hour) {
		t.Fatal("acquire scheduler lock")
	}
	t.Cleanup(func() { releaseLock(root) })
}

func gatedClaudeBin(t *testing.T, gatePath, stdoutJSON string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	script := "#!/bin/sh\n"
	script += "while [ ! -f " + shSingleQuote(gatePath) + " ]; do sleep 0.05; done\n"
	if stdoutJSON != "" {
		script += "cat <<'JSON_EOF'\n" + stdoutJSON + "\nJSON_EOF\n"
	}
	script += "exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func detachLeaseClaudeBin(t *testing.T, pidFile string) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required to setsid a detached descendant")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	script := fmt.Sprintf(`#!/bin/sh
python3 -c '
import os, time
os.setsid()
open(%q, "w").write(str(os.getpid()))
devnull = os.open("/dev/null", os.O_RDWR)
for fd in (0, 1, 2, 3):
    try:
        os.dup2(devnull, fd)
    except OSError:
        pass
while True:
    time.sleep(1)
' &
sleep 60
exit 1
`, pidFile)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitBoundAttempt(t *testing.T, root, id string) *Task {
	t.Helper()
	return waitTaskPred(t, root, id, 5*time.Second, func(got *Task) bool {
		if got.Status != statusRunning || got.ActiveAttemptID == "" {
			return false
		}
		rec, err := loadAttempt(root, got.ID, got.ActiveAttemptID)
		return err == nil && rec != nil && rec.PID > 0 && rec.State == attemptBound
	})
}

func waitTaskPred(t *testing.T, root, id string, timeout time.Duration, pred func(*Task) bool) *Task {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last *Task
	for time.Now().Before(deadline) {
		got, err := loadTask(root, id)
		if err == nil {
			last = got
			if pred(got) {
				return got
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if last == nil {
		t.Fatalf("task %s never became loadable", id)
	}
	t.Fatalf("task %s did not reach expected state (status=%s attempt=%q control=%s)", id, last.Status, last.ActiveAttemptID, last.effectiveControlState())
	return last
}

func lateTypesAfterHold(events []TaskEvent) []string {
	var holdSeq int64
	for _, ev := range events {
		if ev.Type == evHeld && ev.Seq > holdSeq {
			holdSeq = ev.Seq
		}
	}
	var out []string
	for _, ev := range events {
		if ev.Seq <= holdSeq {
			continue
		}
		switch ev.Type {
		case evRetry, evDispatched, evStepOK, evDone:
			out = append(out, ev.Type)
		}
	}
	return out
}

func TestLegacyTaskJSONLoadableFailClosed(t *testing.T) {
	root := testRoot(t)
	legacy := []byte(`{
  "id": "t0101-0001-aaaa",
  "title": "legacy",
  "type": "sequence",
  "priority": 1,
  "status": "held",
  "dir": "/tmp",
  "prompts": ["p"],
  "step": 0,
  "created_at": "2026-01-01T00:00:00Z",
  "updated_at": "2026-01-01T00:00:00Z"
}
`)
	if err := os.WriteFile(taskPath(root, "t0101-0001-aaaa"), legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	tk, err := loadTask(root, "t0101-0001-aaaa")
	if err != nil {
		t.Fatal(err)
	}
	if tk.schedulingAllowed() {
		t.Fatal("legacy held JSON must not be schedulable")
	}
	if eligible(tk, time.Now()) {
		t.Fatal("legacy held JSON must fail eligible()")
	}
	done := []byte(`{
  "id": "t0101-0001-bbbb",
  "title": "legacy done",
  "type": "sequence",
  "priority": 1,
  "status": "done",
  "dir": "/tmp",
  "prompts": ["p"],
  "step": 1,
  "created_at": "2026-01-01T00:00:00Z",
  "updated_at": "2026-01-01T00:00:00Z"
}
`)
	if err := os.WriteFile(taskPath(root, "t0101-0001-bbbb"), done, 0o644); err != nil {
		t.Fatal(err)
	}
	dk, err := loadTask(root, "t0101-0001-bbbb")
	if err != nil {
		t.Fatal(err)
	}
	if dk.schedulingAllowed() || eligible(dk, time.Now()) {
		t.Fatal("legacy done JSON must not be schedulable")
	}
	queued := []byte(`{
  "id": "t0101-0001-cccc",
  "title": "legacy queued",
  "type": "sequence",
  "priority": 1,
  "status": "queued",
  "dir": "/tmp",
  "prompts": ["p"],
  "step": 0,
  "created_at": "2026-01-01T00:00:00Z",
  "updated_at": "2026-01-01T00:00:00Z"
}
`)
	if err := os.WriteFile(taskPath(root, "t0101-0001-cccc"), queued, 0o644); err != nil {
		t.Fatal(err)
	}
	qk, err := loadTask(root, "t0101-0001-cccc")
	if err != nil {
		t.Fatal(err)
	}
	if !qk.schedulingAllowed() || !eligible(qk, time.Now()) {
		t.Fatal("legacy queued JSON must remain schedulable")
	}
}

func TestCLIHoldRejectsLateRunnerRetryWriteback(t *testing.T) {
	root := testRoot(t)
	gate := filepath.Join(t.TempDir(), "gate")
	cfg := runTaskCfg(t, gatedClaudeBin(t, gate, mkOKResultJSON("sess-late")))
	ws := t.TempDir()
	tk := newTask(root, cfg, typeSequence, "held late retry", ws, []string{"do the work"}, 5)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		fresh, err := loadTask(root, tk.ID)
		if err != nil {
			done <- err
			return
		}
		done <- runTask(context.Background(), root, cfg, fresh, false)
	}()
	running := waitBoundAttempt(t, root, tk.ID)
	if err := cmdSetStatus([]string{"-root", root, running.ID}, "hold"); err != nil {
		t.Fatalf("cli hold: %v", err)
	}
	if err := os.WriteFile(gate, []byte("go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runner: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("runner did not return after hold")
	}
	held, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if held.Status != statusHeld {
		t.Fatalf("status=%s, want held", held.Status)
	}
	if held.schedulingAllowed() {
		t.Fatal("held card must not be schedulable")
	}
	if late := lateTypesAfterHold(readAllEventsRaw(t, root, held.ID)); len(late) > 0 {
		t.Fatalf("late runner writeback became visible after hold: %v", late)
	}
}

func TestHoldWhileChildAliveThenCommitViaCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group custody is POSIX")
	}
	root := testRoot(t)
	ws := t.TempDir()
	canary := filepath.Join(ws, "product.txt")
	if err := os.WriteFile(canary, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hang := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(hang, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := runTaskCfg(t, hang)
	tk := newTask(root, cfg, typeSequence, "live child hold", ws, []string{"do the work"}, 5)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		fresh, err := loadTask(root, tk.ID)
		if err != nil {
			done <- err
			return
		}
		done <- runTask(context.Background(), root, cfg, fresh, false)
	}()
	_ = waitBoundAttempt(t, root, tk.ID)
	old := terminalizeWaitTimeout
	terminalizeWaitTimeout = 3 * time.Second
	defer func() { terminalizeWaitTimeout = old }()
	if err := cmdSetStatus([]string{"-root", root, tk.ID}, "hold"); err != nil {
		t.Fatalf("cli hold: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runner: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("runner did not return after hold")
	}
	fresh, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status != statusHeld || fresh.effectiveControlState() != controlTerminal {
		t.Fatalf("status=%s control=%s", fresh.Status, fresh.effectiveControlState())
	}
	if fresh.ActiveAttemptID != "" {
		t.Fatalf("active attempt still bound: %q", fresh.ActiveAttemptID)
	}
	got, _ := os.ReadFile(canary)
	if string(got) != "before\n" {
		t.Fatalf("product bytes mutated: %q", got)
	}
}

func TestDetachedDescendantBlocksHeldUntilGone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("workspace flock is POSIX")
	}
	root := testRoot(t)
	ws := t.TempDir()
	pidFile := filepath.Join(ws, "descendant.pid")
	cfg := runTaskCfg(t, detachLeaseClaudeBin(t, pidFile))
	t.Cleanup(func() {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil || pid <= 0 {
			return
		}
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
			_, _ = p.Wait()
		}
	})
	tk := newTask(root, cfg, typeSequence, "detached descendant", ws, []string{"do the work"}, 5)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		fresh, err := loadTask(root, tk.ID)
		if err != nil {
			done <- err
			return
		}
		done <- runTask(context.Background(), root, cfg, fresh, false)
	}()
	running := waitBoundAttempt(t, root, tk.ID)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, pidErr := os.Stat(pidFile)
		if pidErr == nil && workspaceProcessResidue(ws) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(pidFile); err != nil {
		t.Fatal("runner did not create a detached descendant")
	}
	old := terminalizeWaitTimeout
	terminalizeWaitTimeout = 800 * time.Millisecond
	defer func() { terminalizeWaitTimeout = old }()
	err := cmdSetStatus([]string{"-root", root, running.ID}, "hold")
	if !errors.Is(err, errCustodyTimeout) {
		t.Fatalf("want custody timeout for detached descendant, got %v", err)
	}
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("runner did not return after hold revoke")
	}
	fresh, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status == statusHeld && fresh.effectiveControlState() == controlTerminal {
		t.Fatal("must not commit held while detached descendant holds the workspace")
	}
	if fresh.schedulingAllowed() {
		t.Fatal("must remain non-schedulable after revoke")
	}
	events := readAllEventsRaw(t, root, tk.ID)
	sawNeeds := false
	for _, ev := range events {
		if ev.Type == evNeedsOwner {
			sawNeeds = true
		}
		if ev.Type == evHeld {
			t.Fatalf("held must not become visible: %v", eventTypes(events))
		}
	}
	if !sawNeeds {
		t.Fatalf("expected needs_owner, got %v", eventTypes(events))
	}
}

func TestStalePostCompleteDoesNotPublishFollowOn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group custody is POSIX")
	}
	root := testRoot(t)
	payload := mkOKResultJSON("sess-post") + "\n```json\n{\"goal\":\"g\",\"done\":[\"d\"],\"tasks\":[{\"title\":\"child\",\"prompt\":\"x\"}]}\n```"
	gate := filepath.Join(t.TempDir(), "gate")
	cfg := runTaskCfg(t, gatedClaudeBin(t, gate, payload))
	cfg.MaxFixRounds = 3
	ws := t.TempDir()
	tk := newTask(root, cfg, typeSequence, "stale postcomplete", ws, []string{"do the work"}, 5)
	tk.EmitProgress = true
	tk.EmitTasks = true
	tk.ReviewAfter = true
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		fresh, err := loadTask(root, tk.ID)
		if err != nil {
			done <- err
			return
		}
		done <- runTask(context.Background(), root, cfg, fresh, false)
	}()
	_ = waitBoundAttempt(t, root, tk.ID)
	if err := cmdSetStatus([]string{"-root", root, tk.ID}, "hold"); err != nil {
		t.Fatalf("cli hold: %v", err)
	}
	if err := os.WriteFile(gate, []byte("go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runner: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("runner did not return after hold")
	}
	fresh, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status != statusHeld {
		t.Fatalf("status=%s, want held", fresh.Status)
	}
	if _, err := os.Stat(progressPath(root, tk.ID)); err == nil {
		t.Fatal("stale postComplete must not publish progress")
	}
	tasks, err := loadTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range tasks {
		if got.ID != tk.ID {
			t.Fatalf("stale postComplete created child %s", got.ID)
		}
	}
}

func TestRestartNeverRebindsPersistedAttempt(t *testing.T) {
	root := testRoot(t)
	withSchedulerLock(t, root)
	cfg := runTaskCfg(t, fakeClaudeBin(t, mkOKResultJSON("sess-rebind"), "", 0))
	ws := t.TempDir()
	tk := newTask(root, cfg, typeSequence, "restart rebind", ws, []string{"do the work"}, 5)
	tk.Status = statusRunning
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if err := reserveDispatchAttempt(root, tk); err != nil {
		t.Fatal(err)
	}
	oldID := tk.ActiveAttemptID
	if oldID == "" {
		t.Fatal("expected reserved attempt")
	}
	rec := &AttemptRecord{
		TaskID: tk.ID, AttemptID: oldID, State: attemptBound,
		PID: 999999, PGID: 999999, StartIdentity: "dead", WorkspaceLeaseID: canonicalWorkspaceID(ws),
		CreatedAt: time.Now().Add(-time.Minute).Format(time.RFC3339Nano),
	}
	if err := writeAttempt(root, rec); err != nil {
		t.Fatal(err)
	}
	fresh, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := runTask(context.Background(), root, cfg, fresh, false); err != nil {
		t.Fatal(err)
	}
	done, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != statusDone {
		t.Fatalf("status=%s", done.Status)
	}
	entries, err := os.ReadDir(attemptsDir(root, tk.ID))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		id := e.Name()[:len(e.Name())-len(".json")]
		ids = append(ids, id)
		att, err := loadAttempt(root, tk.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if att.AttemptID == oldID && (att.State == attemptBound || att.State == attemptReserved) {
			t.Fatalf("old attempt reused live: %+v", att)
		}
		if att.PID == 999999 && att.State == attemptBound {
			t.Fatal("new producer rebound into the crash-left attempt")
		}
	}
	if len(ids) < 2 {
		t.Fatalf("expected a fresh attempt plus the leftover, got %v", ids)
	}
}

func TestPIDReuseNeverSignaled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process-group identity")
	}
	livePID, cleanup := spawnLiveForeignPID(t)
	defer cleanup()
	start, ok := processStartIdentity(livePID)
	if !ok {
		t.Fatal("could not capture live start identity")
	}
	pgid := attemptProcessPGID(livePID)
	rec := &AttemptRecord{
		PID: livePID, PGID: livePID + 1_000_003, State: attemptBound,
		StartIdentity: start, WorkspaceLeaseID: "/tmp/foreign-ws",
	}
	if verifyAttemptProcess(rec) {
		t.Fatal("mismatched PGID must not verify as the attempt producer")
	}
	verifyAndSignalAttempt(rec)
	if !processAlive(livePID) {
		t.Fatal("foreign/reused PID must not be signaled")
	}
	rec.PGID = pgid
	rec.StartIdentity = start + "-stale"
	if verifyAttemptProcess(rec) {
		t.Fatal("start-identity mismatch must not verify")
	}
	verifyAndSignalAttempt(rec)
	if !processAlive(livePID) {
		t.Fatal("start-identity mismatch must not be signaled")
	}
	rec.StartIdentity = start
	rec.WorkspaceLeaseID = ""
	if verifyAttemptProcess(rec) {
		t.Fatal("missing workspace lease must fail closed")
	}

	root := testRoot(t)
	cfg := testCfg()
	ws := t.TempDir()
	tk := newTask(root, cfg, typeSequence, "pid reuse hold", ws, []string{"p"}, 5)
	tk.Status = statusRunning
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	withSchedulerLock(t, root)
	if err := reserveDispatchAttempt(root, tk); err != nil {
		t.Fatal(err)
	}
	bound := &AttemptRecord{
		TaskID: tk.ID, AttemptID: tk.ActiveAttemptID, State: attemptBound,
		PID: livePID, PGID: pgid, StartIdentity: start,
		WorkspaceLeaseID: canonicalWorkspaceID(ws),
		CreatedAt:        time.Now().Format(time.RFC3339Nano),
	}
	if err := writeAttempt(root, bound); err != nil {
		t.Fatal(err)
	}
	old := terminalizeWaitTimeout
	terminalizeWaitTimeout = 400 * time.Millisecond
	defer func() { terminalizeWaitTimeout = old }()
	_ = cmdSetStatus([]string{"-root", root, tk.ID}, "hold")
	if !processAlive(livePID) {
		t.Fatal("hold must not signal a reused foreign PID/process-group leader")
	}
}

func TestOptimizeHeldSeq5RejectsLateSeq6To11(t *testing.T) {
	root := testRoot(t)
	ws := t.TempDir()
	first := runTaskCfg(t, fakeClaudeBin(t, mkOKResultJSON("sess-opt-1"), "", 0))
	tk := newTask(root, first, typeSequence, "optimize held-to-done", ws, []string{"step-1"}, 5)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	emitTaskEvent(root, tk.ID, evQueued, "cli:add", statusQueued, 0, nil)
	if err := runTask(context.Background(), root, first, tk, false); err != nil {
		t.Fatal(err)
	}
	doneCard, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if doneCard.Status != statusDone {
		t.Fatalf("setup status=%s", doneCard.Status)
	}
	if err := cmdSetStatus([]string{"-root", root, doneCard.ID}, "retry"); err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(t.TempDir(), "gate")
	second := runTaskCfg(t, gatedClaudeBin(t, gate, mkOKResultJSON("sess-opt-2")))
	queued, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- runTask(context.Background(), root, second, queued, false)
	}()
	_ = waitBoundAttempt(t, root, tk.ID)
	if err := cmdSetStatus([]string{"-root", root, tk.ID}, "hold"); err != nil {
		t.Fatalf("cli hold: %v", err)
	}
	if err := os.WriteFile(gate, []byte("go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runner: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("runner did not return after hold")
	}
	after := readAllEventsRaw(t, root, tk.ID)
	if late := lateTypesAfterHold(after); len(late) > 0 {
		t.Fatalf("late seq6-11 writeback became visible: %v all=%v", late, eventTypes(after))
	}
	fresh, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status != statusHeld || fresh.schedulingAllowed() {
		t.Fatalf("held must remain authoritative: status=%s eligible=%v", fresh.Status, fresh.schedulingAllowed())
	}
}

func TestLiveSchedulerLockNotStolenByTTL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX lock")
	}
	root := testRoot(t)
	livePID, cleanup := spawnLiveForeignPID(t)
	defer cleanup()
	path := lockPath(root)
	info, _ := json.Marshal(lockInfo{PID: livePID, At: time.Now().Format(time.RFC3339)})
	if err := os.WriteFile(path, info, 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
	if staleLock(path, time.Second) {
		t.Fatal("live owner must not be stale merely because mtime exceeded TTL")
	}
	if acquireLock(root, time.Second) {
		t.Fatal("must not steal live scheduler lock via TTL")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var li lockInfo
	if err := json.Unmarshal(data, &li); err != nil || li.PID != livePID {
		t.Fatalf("lock owner mutated: %+v err=%v", li, err)
	}
}

func TestFormerOwnerCannotWriteAfterLockLoss(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX lock")
	}
	root := testRoot(t)
	if !acquireLock(root, time.Hour) {
		t.Fatal("acquire")
	}
	cfg := testCfg()
	tk := newTask(root, cfg, typeSequence, "lock loss", "/tmp", []string{"p"}, 5)
	tk.Status = statusRunning
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if err := reserveDispatchAttempt(root, tk); err != nil {
		t.Fatal(err)
	}
	releaseLock(root)
	livePID, cleanup := spawnLiveForeignPID(t)
	defer cleanup()
	info, _ := json.Marshal(lockInfo{PID: livePID, At: time.Now().Format(time.RFC3339)})
	if err := os.WriteFile(lockPath(root), info, 0o644); err != nil {
		t.Fatal(err)
	}
	tk.LastError = "late writeback after lock loss"
	if err := saveTask(root, tk); !errors.Is(err, errSchedulerLockLost) && !errors.Is(err, errStaleTaskWrite) {
		t.Fatalf("former owner write: %v", err)
	}
	if err := persistTaskEvent(root, tk, evDone, "runner", statusDone, tk.Step, withCostTelemetry(nil, tk)); !errors.Is(err, errSchedulerLockLost) && !errors.Is(err, errStaleTaskWrite) {
		t.Fatalf("former owner terminal: %v", err)
	}
	if err := reserveDispatchAttempt(root, tk); !errors.Is(err, errSchedulerLockLost) && !errors.Is(err, errStaleTaskWrite) && !errors.Is(err, errNotSchedulable) && !errors.Is(err, errAttemptConflict) {
		t.Fatalf("former owner dispatch: %v", err)
	}
}

func TestSchedulerWriteDeniedWithoutLockFile(t *testing.T) {
	root := testRoot(t)
	if !schedulerWriteAllowed(root) {
		t.Fatal("CLI/tests that never acquired may write when no lock file is present")
	}
	if holdsSchedulerLock(root) {
		t.Fatal("missing lock is not ownership")
	}
	if !acquireLock(root, time.Hour) {
		t.Fatal("acquire")
	}
	if !schedulerWriteAllowed(root) {
		t.Fatal("exact owner must be allowed to write")
	}
	releaseLock(root)
	if schedulerWriteAllowed(root) {
		t.Fatal("former owner must be fail-closed after losing the lock file")
	}
}

func TestUnreadableLockDeniesRunnerWrites(t *testing.T) {
	root := testRoot(t)
	if err := os.MkdirAll(filepath.Dir(lockPath(root)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath(root), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if schedulerWriteAllowed(root) {
		t.Fatal("unreadable lock must be fail-closed for runner writes")
	}
}

func TestDeadOwnerLockStillRecoverable(t *testing.T) {
	root := testRoot(t)
	path := lockPath(root)
	info, _ := json.Marshal(lockInfo{PID: 999999, At: time.Now().Format(time.RFC3339)})
	if err := os.WriteFile(path, info, 0o644); err != nil {
		t.Fatal(err)
	}
	if !staleLock(path, time.Hour) {
		t.Fatal("dead owner must remain recoverable")
	}
	if !acquireLock(root, time.Hour) {
		t.Fatal("dead owner lock must be stealable")
	}
	if !holdsSchedulerLock(root) {
		t.Fatal("recovery must create a new explicit owner")
	}
	releaseLock(root)
}

func TestTwoRunInstancesSingleLockOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX lock")
	}
	root := testRoot(t)
	if !acquireLock(root, time.Hour) {
		t.Fatal("acquire")
	}
	defer releaseLock(root)
	if acquireLock(root, time.Millisecond) {
		t.Fatal("second instance must not own the lock")
	}
	if !holdsSchedulerLock(root) {
		t.Fatal("first instance must still own the lock")
	}
}

func TestTransitionJournalCrashReplayOnce(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	tk := newTask(root, cfg, typeSequence, "journal replay", "/tmp", []string{"p"}, 5)
	tk.Status = statusHeld
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	tid := newTransitionID()
	tk.LastCommittedTransitionID = tid
	tk.Revision++
	markControlTerminal(tk)
	if err := writeTaskFile(root, tk); err != nil {
		t.Fatal(err)
	}
	rec := &TransitionRecord{
		TransitionID:     tid,
		TaskID:           tk.ID,
		ExpectedRevision: tk.Revision - 1,
		NewRevision:      tk.Revision,
		ControlEpoch:     tk.ControlEpoch,
		EventType:        evHeld,
		Status:           statusHeld,
		Actor:            "cli:hold",
		State:            transitionPrepared,
		CreatedAt:        time.Now().Format(time.RFC3339Nano),
	}
	if err := writeTransition(root, rec); err != nil {
		t.Fatal(err)
	}
	reconcilePreparedTransitions(root)
	events := readAllEventsRaw(t, root, tk.ID)
	held := 0
	for _, ev := range events {
		if ev.Type == evHeld && ev.TransitionID == tid {
			held++
		}
	}
	if held != 1 {
		t.Fatalf("replay held count=%d events=%v", held, eventTypes(events))
	}
	reconcilePreparedTransitions(root)
	events = readAllEventsRaw(t, root, tk.ID)
	held = 0
	for _, ev := range events {
		if ev.Type == evHeld && ev.TransitionID == tid {
			held++
		}
	}
	if held != 1 {
		t.Fatalf("second replay duplicated: %v", eventTypes(events))
	}
	got, err := loadTransition(root, tk.ID, tid)
	if err != nil || got.State != transitionCommitted {
		t.Fatalf("journal state=%v err=%v", got, err)
	}
	abandoned := &TransitionRecord{
		TransitionID:     newTransitionID(),
		TaskID:           tk.ID,
		ExpectedRevision: 0,
		NewRevision:      99,
		EventType:        evDone,
		Status:           statusDone,
		State:            transitionPrepared,
		CreatedAt:        time.Now().Format(time.RFC3339Nano),
	}
	if err := writeTransition(root, abandoned); err != nil {
		t.Fatal(err)
	}
	reconcilePreparedTransitions(root)
	fresh, _ := loadTask(root, tk.ID)
	if fresh.Status != statusHeld {
		t.Fatalf("abandoned prepare fabricated status=%s", fresh.Status)
	}
}

func TestJournalFailureDoesNotExposeExitedAttempt(t *testing.T) {
	root := testRoot(t)
	withSchedulerLock(t, root)
	cfg := testCfg()
	tk := newTask(root, cfg, typeSequence, "journal fail", t.TempDir(), []string{"p"}, 5)
	tk.Status = statusRunning
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if err := reserveDispatchAttempt(root, tk); err != nil {
		t.Fatal(err)
	}
	id := tk.ActiveAttemptID
	transDir := transitionsDir(root, tk.ID)
	if err := os.MkdirAll(transDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Plant a regular file where the transitions directory's parent lock path is fine,
	// but make the transition write fail by turning the transitions dir into a file after
	// creating the attempt. writeTransition MkdirAll + write; replace dir with a file.
	if err := os.RemoveAll(transDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transDir, []byte("not-a-dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	tk.Status = statusDone
	err := persistTaskEvent(root, tk, evDone, "runner", statusDone, tk.Step, withCostTelemetry(nil, tk))
	if err == nil {
		t.Fatal("journal failure must surface")
	}
	fresh, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status == statusDone {
		t.Fatal("journal failure must not expose a terminal task")
	}
	rec, err := loadAttempt(root, tk.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State == attemptExited {
		t.Fatal("journal failure must not expose an exited attempt")
	}
}

func TestTerminalizeIdempotent(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	tk := newTask(root, cfg, typeSequence, "idempotent hold", "/tmp", []string{"p"}, 5)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if err := terminalize(root, tk.ID, statusHeld, "cli:hold", "cli hold", nil); err != nil {
		t.Fatal(err)
	}
	if err := terminalize(root, tk.ID, statusHeld, "cli:hold", "cli hold", nil); err != nil {
		t.Fatal(err)
	}
	events := readAllEventsRaw(t, root, tk.ID)
	n := 0
	for _, ev := range events {
		if ev.Type == evHeld {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("held events=%d %v", n, eventTypes(events))
	}
}

func TestReservePersistsAttemptIdentity(t *testing.T) {
	root := testRoot(t)
	withSchedulerLock(t, root)
	cfg := testCfg()
	tk := newTask(root, cfg, typeSequence, "persist attempt", "/tmp", []string{"p"}, 5)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if err := reserveDispatchAttempt(root, tk); err != nil {
		t.Fatal(err)
	}
	if tk.ActiveAttemptID == "" {
		t.Fatal("in-memory attempt id missing")
	}
	fresh, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ActiveAttemptID != tk.ActiveAttemptID {
		t.Fatalf("attempt id not persisted: disk=%q mem=%q", fresh.ActiveAttemptID, tk.ActiveAttemptID)
	}
	rec, err := loadAttempt(root, tk.ID, fresh.ActiveAttemptID)
	if err != nil || rec == nil || rec.State != attemptReserved {
		t.Fatalf("durable attempt missing: rec=%+v err=%v", rec, err)
	}
}

func TestTwoDispatchersSingleAttempt(t *testing.T) {
	root := testRoot(t)
	withSchedulerLock(t, root)
	cfg := testCfg()
	tk := newTask(root, cfg, typeSequence, "race reserve", "/tmp", []string{"p"}, 5)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	const n = 8
	var ok, fail int32
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			snap, err := loadTask(root, tk.ID)
			if err != nil {
				atomic.AddInt32(&fail, 1)
				return
			}
			if err := reserveDispatchAttempt(root, snap); err != nil {
				atomic.AddInt32(&fail, 1)
				return
			}
			atomic.AddInt32(&ok, 1)
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("admitted=%d rejected=%d want exactly 1 admission", ok, fail)
	}
	fresh, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ActiveAttemptID == "" {
		t.Fatal("winning attempt was not persisted")
	}
}

func TestRunnerTerminalFailureClosesAttempt(t *testing.T) {
	root := testRoot(t)
	cfg := runTaskCfg(t, fakeClaudeBin(t, "", "boom", 1))
	cfg.MaxAttempts = 1
	ws := t.TempDir()
	tk := newTask(root, cfg, typeSequence, "runner terminal fail", ws, []string{"do the work"}, 5)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if err := runTask(context.Background(), root, cfg, tk, false); err != nil {
		t.Fatal(err)
	}
	fresh, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status != statusFailed {
		t.Fatalf("status=%s, want failed", fresh.Status)
	}
	if fresh.effectiveControlState() != controlTerminal {
		t.Fatalf("control_state=%s, want terminal", fresh.effectiveControlState())
	}
	if fresh.schedulingAllowed() {
		t.Fatal("terminal failed card must not be schedulable")
	}
	if fresh.ActiveAttemptID != "" {
		t.Fatalf("active_attempt_id still bound: %q", fresh.ActiveAttemptID)
	}
	if fresh.LastCommittedTransitionID == "" {
		t.Fatal("last_committed_transition_id empty")
	}
	entries, err := os.ReadDir(attemptsDir(root, fresh.ID))
	if err != nil {
		t.Fatal(err)
	}
	closed := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		id := e.Name()[:len(e.Name())-len(".json")]
		att, err := loadAttempt(root, fresh.ID, id)
		if err != nil || att == nil {
			continue
		}
		if att.State == attemptBound || att.State == attemptReserved {
			t.Fatalf("attempt %s still live: %+v", id, att)
		}
		if att.State == attemptExited || att.State == attemptRevoked {
			closed++
		}
	}
	if closed == 0 {
		t.Fatal("expected a closed attempt record")
	}
}

func TestProcessStartIdentityStable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process identity")
	}
	pid, cleanup := spawnLiveForeignPID(t)
	defer cleanup()
	a, ok := processStartIdentity(pid)
	if !ok || a == "" {
		t.Fatalf("start identity missing: %q ok=%v", a, ok)
	}
	b, ok := processStartIdentity(pid)
	if !ok || a != b {
		t.Fatalf("start identity drifted: %q vs %q", a, b)
	}
}

func seedRunningReservedAttempt(t *testing.T, root string) *Task {
	t.Helper()
	withSchedulerLock(t, root)
	cfg := testCfg()
	tk := newTask(root, cfg, typeSequence, "crash boundary", t.TempDir(), []string{"p"}, 5)
	tk.Status = statusRunning
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if err := reserveDispatchAttempt(root, tk); err != nil {
		t.Fatal(err)
	}
	fresh, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ActiveAttemptID == "" {
		t.Fatal("expected reserved attempt")
	}
	return fresh
}

func writeDoneTransition(t *testing.T, root string, tk *Task, tid, state string) *TransitionRecord {
	t.Helper()
	rec := &TransitionRecord{
		TransitionID:     tid,
		TaskID:           tk.ID,
		ExpectedRevision: tk.Revision,
		NewRevision:      tk.Revision + 1,
		ControlEpoch:     tk.ControlEpoch,
		AttemptID:        tk.ActiveAttemptID,
		EventType:        evDone,
		Status:           statusDone,
		Actor:            "runner",
		State:            state,
		CreatedAt:        time.Now().Format(time.RFC3339Nano),
	}
	if err := writeTransition(root, rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func projectDoneTaskFromJournal(t *testing.T, root string, rec *TransitionRecord) {
	t.Helper()
	tk, err := loadTask(root, rec.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	tk.Status = rec.Status
	markControlTerminal(tk)
	tk.Revision = rec.NewRevision
	if rec.ControlEpoch != 0 {
		tk.ControlEpoch = rec.ControlEpoch
	}
	tk.LastCommittedTransitionID = rec.TransitionID
	applyControlDefaults(tk)
	tk.touch()
	if err := writeTaskFile(root, tk); err != nil {
		t.Fatal(err)
	}
}

func countTransitionEvents(events []TaskEvent, evType, tid string) int {
	n := 0
	for _, ev := range events {
		if ev.Type == evType && ev.TransitionID == tid {
			n++
		}
	}
	return n
}

func assertNoD1ProductionEffects(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := strings.ToLower(d.Name())
		switch {
		case strings.Contains(name, "outbox"),
			strings.Contains(name, "manager-wake"),
			strings.Contains(name, "watchdog"),
			name == "wake",
			name == "wakes",
			strings.Contains(name, "subscription"):
			t.Errorf("D1 artifact present: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertCrashPointVisibility(t *testing.T, root, point string, tk *Task, tid string) {
	t.Helper()
	fresh, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	att, attErr := loadAttempt(root, tk.ID, tk.ActiveAttemptID)
	if attErr != nil || att == nil {
		t.Fatalf("attempt load: %v", attErr)
	}
	committed := transitionDurablyCommitted(root, tk.ID, tid)
	doneEvents := countTransitionEvents(readAllEventsRaw(t, root, tk.ID), evDone, tid)
	preCommit := point == transitionCrashAfterPrepare || point == transitionCrashAfterAttemptClose
	switch point {
	case transitionCrashAfterPrepare:
		if committed {
			t.Fatal("prepare crash must not durably commit")
		}
		if att.State == attemptExited || att.State == attemptRevoked {
			t.Fatalf("prepare crash must leave attempt live, got %s", att.State)
		}
	case transitionCrashAfterAttemptClose:
		if committed {
			t.Fatal("attempt-close crash must not durably commit")
		}
		if att.State != attemptExited && att.State != attemptRevoked {
			t.Fatalf("attempt-close crash must close attempt, got %s", att.State)
		}
	case transitionCrashAfterCommit:
		if !committed {
			t.Fatal("commit crash must leave a committed journal")
		}
		if att.State != attemptExited && att.State != attemptRevoked {
			t.Fatalf("commit crash must already have a closed attempt, got %s", att.State)
		}
	case transitionCrashAfterTaskProjection, transitionCrashAfterEventProjection:
		if !committed {
			t.Fatal("post-commit projection crash must keep the committed journal")
		}
		if fresh.Status != statusDone || fresh.effectiveControlState() != controlTerminal {
			t.Fatalf("task projection missing: status=%s control=%s", fresh.Status, fresh.effectiveControlState())
		}
		if fresh.ActiveAttemptID != "" {
			t.Fatalf("terminal task retained active attempt %q", fresh.ActiveAttemptID)
		}
	}
	if preCommit || point == transitionCrashAfterCommit {
		if fresh.Status == statusDone || fresh.effectiveControlState() == controlTerminal {
			t.Fatalf("%s exposed terminal task status=%s control=%s", point, fresh.Status, fresh.effectiveControlState())
		}
		if fresh.LastCommittedTransitionID == tid {
			t.Fatalf("%s projected last_committed_transition_id before allowed", point)
		}
		if doneEvents != 0 {
			t.Fatalf("%s exposed terminal event count=%d", point, doneEvents)
		}
	}
	if point == transitionCrashAfterTaskProjection && doneEvents != 0 {
		t.Fatalf("task-projection crash must not yet expose the terminal event, count=%d", doneEvents)
	}
	if point == transitionCrashAfterEventProjection && doneEvents != 1 {
		t.Fatalf("event-projection crash must leave exactly one terminal event, count=%d", doneEvents)
	}
	assertNoD1ProductionEffects(t, root)
}

func assertRecoveredTerminalDone(t *testing.T, root string, tk *Task, tid string) {
	t.Helper()
	fresh, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status != statusDone || fresh.effectiveControlState() != controlTerminal {
		t.Fatalf("recovered status=%s control=%s", fresh.Status, fresh.effectiveControlState())
	}
	if fresh.ActiveAttemptID != "" {
		t.Fatalf("recovered terminal kept active attempt %q", fresh.ActiveAttemptID)
	}
	if fresh.LastCommittedTransitionID != tid {
		t.Fatalf("recovered last_committed_transition_id=%q want %q", fresh.LastCommittedTransitionID, tid)
	}
	if !transitionDurablyCommitted(root, tk.ID, tid) {
		t.Fatal("recovered journal is not committed")
	}
	if fresh.schedulingAllowed() {
		t.Fatal("recovered terminal must not be schedulable")
	}
	att, err := loadAttempt(root, tk.ID, tk.ActiveAttemptID)
	if err != nil || att == nil {
		t.Fatalf("recovered attempt: %v", err)
	}
	if att.State != attemptExited && att.State != attemptRevoked {
		t.Fatalf("recovered attempt still live: %+v", att)
	}
	events := readAllEventsRaw(t, root, tk.ID)
	if got := countTransitionEvents(events, evDone, tid); got != 1 {
		t.Fatalf("recovered done events=%d types=%v", got, eventTypes(events))
	}
	stale := *tk
	if err := persistTaskEvent(root, &stale, evStepOK, "runner", statusRunning, stale.Step, nil); !errors.Is(err, errStaleTaskWrite) && !errors.Is(err, errAlreadyTerminal) && !errors.Is(err, errProducerInvalidated) && !errors.Is(err, errNotSchedulable) {
		t.Fatalf("stale writeback after recovery: %v", err)
	}
	if err := persistTaskEvent(root, &stale, evDone, "runner", statusDone, stale.Step, withCostTelemetry(nil, &stale)); err != nil && !errors.Is(err, errStaleTaskWrite) && !errors.Is(err, errAlreadyTerminal) && !errors.Is(err, errProducerInvalidated) {
		t.Fatalf("late terminal writeback after recovery: %v", err)
	}
	if got := countTransitionEvents(readAllEventsRaw(t, root, tk.ID), evDone, tid); got != 1 {
		t.Fatalf("late writeback duplicated terminal event: %d", got)
	}
	assertNoD1ProductionEffects(t, root)
}

func TestTerminalCrashPointDiskStatesRecover(t *testing.T) {
	points := []string{
		transitionCrashAfterPrepare,
		transitionCrashAfterAttemptClose,
		transitionCrashAfterCommit,
		transitionCrashAfterTaskProjection,
		transitionCrashAfterEventProjection,
	}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			root := testRoot(t)
			tk := seedRunningReservedAttempt(t, root)
			tid := newTransitionID()
			state := transitionPrepared
			if point != transitionCrashAfterPrepare && point != transitionCrashAfterAttemptClose {
				state = transitionCommitted
			}
			rec := writeDoneTransition(t, root, tk, tid, state)
			if point != transitionCrashAfterPrepare {
				closeAttemptRecord(root, tk.ID, tk.ActiveAttemptID, attemptExited)
			}
			if point == transitionCrashAfterTaskProjection || point == transitionCrashAfterEventProjection {
				projectDoneTaskFromJournal(t, root, rec)
			}
			if point == transitionCrashAfterEventProjection {
				if err := recordEvent(root, tk.ID, TaskEvent{
					Type:         rec.EventType,
					Actor:        rec.Actor,
					Status:       rec.Status,
					Step:         tk.Step,
					TransitionID: rec.TransitionID,
					Revision:     rec.NewRevision,
					AttemptID:    rec.AttemptID,
				}); err != nil {
					t.Fatal(err)
				}
			}
			assertCrashPointVisibility(t, root, point, tk, tid)
			reconcilePreparedTransitions(root)
			reconcilePreparedTransitions(root)
			assertRecoveredTerminalDone(t, root, tk, tid)
		})
	}
}

func restoreAttemptControlSeams(t *testing.T) {
	t.Helper()
	prevLoad := attemptLoadHook
	prevWrite := attemptWriteHook
	prevSync := syncDirAfterRename
	t.Cleanup(func() {
		attemptLoadHook = prevLoad
		attemptWriteHook = prevWrite
		syncDirAfterRename = prevSync
		transitionCrashAt = ""
	})
}

func assertNoCommittedTerminalVisibility(t *testing.T, root string, tk *Task) {
	t.Helper()
	fresh, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status == statusDone || fresh.effectiveControlState() == controlTerminal {
		t.Fatalf("pre-commit terminal visibility status=%s control=%s", fresh.Status, fresh.effectiveControlState())
	}
	if fresh.LastCommittedTransitionID != tk.LastCommittedTransitionID {
		t.Fatalf("pre-commit last_committed_transition_id=%q", fresh.LastCommittedTransitionID)
	}
	recs, err := listTaskTransitions(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		if rec != nil && rec.State == transitionCommitted {
			t.Fatalf("journal %s committed without durable attempt close", rec.TransitionID)
		}
	}
	for _, ev := range readAllEventsRaw(t, root, tk.ID) {
		if ev.Type == evDone {
			t.Fatalf("terminal event visible before durable close: %+v", ev)
		}
	}
	assertNoD1ProductionEffects(t, root)
}

func retryTerminalDone(t *testing.T, root string, snapshot *Task) string {
	t.Helper()
	retry, err := loadTask(root, snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	retry.Status = statusDone
	if err := persistTaskEvent(root, retry, evDone, "runner", statusDone, retry.Step, withCostTelemetry(nil, retry)); err != nil {
		t.Fatal(err)
	}
	reconcilePreparedTransitions(root)
	reconcilePreparedTransitions(root)
	recs, err := listTaskTransitions(root, snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("want exactly one journal after recovery, got %d", len(recs))
	}
	assertRecoveredTerminalDone(t, root, snapshot, recs[0].TransitionID)
	return recs[0].TransitionID
}

func TestAttemptClosePersistenceFailuresBlockCommit(t *testing.T) {
	t.Run("load_failure", func(t *testing.T) {
		restoreAttemptControlSeams(t)
		root := testRoot(t)
		tk := seedRunningReservedAttempt(t, root)
		snapshot := *tk
		attemptLoadHook = func(string, string, string) error {
			return errors.New("injected attempt load failure")
		}
		tk.Status = statusDone
		err := persistTaskEvent(root, tk, evDone, "runner", statusDone, tk.Step, withCostTelemetry(nil, tk))
		attemptLoadHook = nil
		if err == nil {
			t.Fatal("attempt load failure must block terminal commit")
		}
		assertNoCommittedTerminalVisibility(t, root, &snapshot)
		att, attErr := loadAttempt(root, snapshot.ID, snapshot.ActiveAttemptID)
		if attErr != nil || att == nil {
			t.Fatalf("load failure must leave the on-disk attempt inspectable: %v", attErr)
		}
		if att.State == attemptExited || att.State == attemptRevoked {
			t.Fatalf("load failure must not close the attempt, got %s", att.State)
		}
		retryTerminalDone(t, root, &snapshot)
	})
	t.Run("write_failure", func(t *testing.T) {
		restoreAttemptControlSeams(t)
		root := testRoot(t)
		tk := seedRunningReservedAttempt(t, root)
		snapshot := *tk
		attemptWriteHook = func(*AttemptRecord) error {
			return errors.New("injected attempt write failure")
		}
		tk.Status = statusDone
		err := persistTaskEvent(root, tk, evDone, "runner", statusDone, tk.Step, withCostTelemetry(nil, tk))
		if err == nil {
			t.Fatal("attempt write failure must block terminal commit")
		}
		assertNoCommittedTerminalVisibility(t, root, &snapshot)
		att, attErr := loadAttempt(root, snapshot.ID, snapshot.ActiveAttemptID)
		if attErr != nil || att == nil {
			t.Fatalf("write failure must leave the attempt readable: %v", attErr)
		}
		if att.State == attemptExited || att.State == attemptRevoked {
			t.Fatalf("write failure must not persist attempt close, got %s", att.State)
		}
		attemptWriteHook = nil
		retryTerminalDone(t, root, &snapshot)
	})
}

func TestArchivedCommittedRecoveryPreservesArchiveResidency(t *testing.T) {
	root := testRoot(t)
	tk := seedRunningReservedAttempt(t, root)
	if err := recordEvent(root, tk.ID, TaskEvent{
		Type: evQueued, Actor: "test", Status: statusQueued, AttemptID: tk.ActiveAttemptID,
	}); err != nil {
		t.Fatal(err)
	}
	tid := newTransitionID()
	writeDoneTransition(t, root, tk, tid, transitionCommitted)
	attemptID := tk.ActiveAttemptID
	tk.Status = statusDone
	tk.ControlState = controlTerminal
	tk.SchedulingEligible = boolPtr(false)
	tk.LastCommittedTransitionID = tid
	tk.ActiveAttemptID = attemptID
	if err := writeTaskFile(root, tk); err != nil {
		t.Fatal(err)
	}
	if err := archiveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(taskPath(root, tk.ID)); !os.IsNotExist(err) {
		t.Fatal("setup must start from an archived task with no live file")
	}
	reconcilePreparedTransitions(root)
	reconcilePreparedTransitions(root)
	if _, err := os.Stat(taskPath(root, tk.ID)); !os.IsNotExist(err) {
		t.Fatal("archived recovery resurrected a live tasks/ file")
	}
	if _, err := os.Stat(filepath.Join(archiveDir(root), tk.ID+".json")); err != nil {
		t.Fatalf("archived task missing after recovery: %v", err)
	}
	live, err := loadTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range live {
		if got != nil && got.ID == tk.ID {
			t.Fatal("archived recovery resurrected the task onto the active board")
		}
	}
	if n := countTransitionEvents(readAllEventsRaw(t, root, tk.ID), evDone, tid); n != 1 {
		t.Fatalf("archived recovery done events=%d", n)
	}
	assertNoD1ProductionEffects(t, root)
}

func TestJournalRenameParentDirSyncFailureBlocksCommit(t *testing.T) {
	restoreAttemptControlSeams(t)
	root := testRoot(t)
	tk := seedRunningReservedAttempt(t, root)
	snapshot := *tk
	syncDirAfterRename = func(string) error {
		return errors.New("injected parent directory sync failure")
	}
	tk.Status = statusDone
	err := persistTaskEvent(root, tk, evDone, "runner", statusDone, tk.Step, withCostTelemetry(nil, tk))
	if err == nil {
		t.Fatal("parent-directory sync failure must block durable journal commit")
	}
	assertNoCommittedTerminalVisibility(t, root, &snapshot)
	syncDirAfterRename = syncContainingDirectory
	retryTerminalDone(t, root, &snapshot)
}

func TestLiveTerminalCrashInjectionRecovers(t *testing.T) {
	points := []string{
		transitionCrashAfterPrepare,
		transitionCrashAfterAttemptClose,
		transitionCrashAfterCommit,
		transitionCrashAfterTaskProjection,
		transitionCrashAfterEventProjection,
	}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			root := testRoot(t)
			tk := seedRunningReservedAttempt(t, root)
			snapshot := *tk
			tk.Status = statusDone
			transitionCrashAt = point
			err := persistTaskEvent(root, tk, evDone, "runner", statusDone, tk.Step, withCostTelemetry(nil, tk))
			transitionCrashAt = ""
			if point != transitionCrashAfterEventProjection {
				if !errors.Is(err, errTransitionCrash) {
					t.Fatalf("injected crash %s: err=%v", point, err)
				}
			} else if err != nil && !errors.Is(err, errTransitionCrash) {
				t.Fatalf("event-projection crash: %v", err)
			}
			journalID := ""
			recs, listErr := listTaskTransitions(root, snapshot.ID)
			if listErr != nil {
				t.Fatal(listErr)
			}
			if len(recs) != 1 {
				t.Fatalf("want exactly one journal, got %d", len(recs))
			}
			journalID = recs[0].TransitionID
			assertCrashPointVisibility(t, root, point, &snapshot, journalID)
			reconcilePreparedTransitions(root)
			reconcilePreparedTransitions(root)
			assertRecoveredTerminalDone(t, root, &snapshot, journalID)
		})
	}
}
