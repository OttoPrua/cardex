package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPreSpawnClaimedInflightRetriesOnceWithEventIDDedupe(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	thread := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa1"
	mw := testWakeCfg(bin, "wake-proj", thread, "mgr")
	tk := heldCommittedTask(t, root, "wake-proj", "pre-spawn-retry")
	held := mustHeldEvent(t, root, tk.ID)
	wantID := wakeEventID(tk.ID, held.Seq, held.TransitionID)
	if err := saveManagerWakeInflightPhase(root, "mgr", thread, []managerWakeOutboxRow{{WakeEventID: wantID}}, 1, inflightPhaseClaimed); err != nil {
		t.Fatal(err)
	}
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatalf("claimed pre-spawn inflight must retry, got %v", err)
	}
	if queueThreadCount(logPath) != 1 {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("claimed retry queue calls=%d payload=%s", queueThreadCount(logPath), data)
	}
	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), "wake="+wantID) {
		t.Fatalf("retry missing event-id contract: %s", data)
	}
	ids, class, err := loadManagerWakeReceiptIDs(root, "mgr")
	if err != nil || class != "" || !ids[wantID] {
		t.Fatalf("receipts class=%q err=%v ids=%v", class, err, ids)
	}
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatal(err)
	}
	if queueThreadCount(logPath) != 1 {
		t.Fatalf("restart duplicated wake: %d", queueThreadCount(logPath))
	}
}

func TestForgedCursorAndReceiptsFailClosedWithoutAdvance(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	thread := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbb1"
	mw := testWakeCfg(bin, "wake-proj", thread, "mgr")
	tk := heldCommittedTask(t, root, "wake-proj", "forged-cursor")
	held := mustHeldEvent(t, root, tk.ID)
	wantID := wakeEventID(tk.ID, held.Seq, held.TransitionID)

	t.Run("forged_high_seq", func(t *testing.T) {
		cur := &managerWakeCursor{
			Schema:         cursorSchemaV1,
			SubscriptionID: "mgr",
			OutboxSeq:      99,
			LastWakeID:     "forged:1:tid",
			UpdatedAt:      "2026-01-01T00:00:00Z",
		}
		if err := saveManagerWakeCursor(root, cur); err != nil {
			t.Fatal(err)
		}
		if err := managerWakeOnce(root, mw); err == nil {
			t.Fatal("forged cursor must fail closed")
		}
		if queueThreadCount(logPath) != 0 {
			t.Fatal("forged cursor must not queue")
		}
		got, class, err := loadManagerWakeCursor(root, "mgr")
		if err != nil || class != "" {
			t.Fatalf("cursor load class=%q err=%v", class, err)
		}
		if got.OutboxSeq != 99 {
			t.Fatalf("malformed cursor must not be rewritten to an ack: %+v", got)
		}
	})

	t.Run("foreign_receipt", func(t *testing.T) {
		root2 := testRoot(t)
		bin2, log2 := fakeCodexQueueBin(t, 0)
		mw2 := testWakeCfg(bin2, "wake-proj", thread, "mgr")
		_ = heldCommittedTask(t, root2, "wake-proj", "foreign-receipt")
		rec := managerWakeReceipt{
			Schema:         receiptSchemaV1,
			SubscriptionID: "mgr",
			WakeEventIDs:   []string{wantID, "forged-receipt:9:tid"},
			UpdatedAt:      "2026-01-01T00:00:00Z",
		}
		data, err := json.MarshalIndent(rec, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		path := managerWakeReceiptPath(root2, "mgr")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := managerWakeOnce(root2, mw2); err == nil || err.Error() != "receipt_foreign" {
			t.Fatalf("foreign receipt must fail visibly, got %v", err)
		}
		if queueThreadCount(log2) != 0 {
			t.Fatal("foreign receipt must not queue")
		}
		cur, _, _ := loadManagerWakeCursor(root2, "mgr")
		if cur != nil && cur.OutboxSeq != 0 {
			t.Fatalf("foreign receipt must not advance cursor: %+v", cur)
		}
	})

	t.Run("wrong_subscription_identity", func(t *testing.T) {
		root2 := testRoot(t)
		bin2, log2 := fakeCodexQueueBin(t, 0)
		mw2 := testWakeCfg(bin2, "wake-proj", thread, "mgr")
		_ = heldCommittedTask(t, root2, "wake-proj", "wrong-sub")
		raw := `{
  "schema": "cardex.manager_wake.cursor.v1",
  "subscription_id": "other",
  "outbox_seq": 1,
  "updated_at": "2026-01-01T00:00:00Z"
}
`
		path := managerWakeCursorPath(root2, "mgr")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := managerWakeOnce(root2, mw2); err == nil || err.Error() != "cursor_corrupt" {
			t.Fatalf("foreign cursor identity must fail, got %v", err)
		}
		if queueThreadCount(log2) != 0 {
			t.Fatal("foreign cursor must not queue")
		}
	})
}

func TestWakeProjectionReadAndAppendFailuresStayRetryable(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	writeWakeConfig(t, root, bin, "wake-proj", "cccccccc-cccc-cccc-cccc-ccccccccccc1", "mgr", true)
	mw := testWakeCfg(bin, "wake-proj", "cccccccc-cccc-cccc-cccc-ccccccccccc1", "mgr")
	tk := heldCommittedTask(t, root, "wake-proj", "proj-fail")
	held := mustHeldEvent(t, root, tk.ID)
	if err := os.Remove(managerWakeOutboxPath(root)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	evPath := filepath.Join(eventsDir(root), tk.ID+".jsonl")
	if err := os.Chmod(evPath, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(evPath, 0o644) })
	projectWakeAfterCommitted(root, tk, held.TransitionID)
	if loadManagerWakeErrorClass(root) == "" {
		t.Fatal("event-read failure after commit must leave durable retry state")
	}
	_ = os.Chmod(evPath, 0o644)

	dir := tasksDir(root)
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if err := managerWakeOnce(root, mw); err == nil {
		t.Fatal("watchdog ReadDir failure must not succeed")
	}
	if loadManagerWakeErrorClass(root) == "" {
		t.Fatal("watchdog failure must leave durable error state")
	}
	if queueThreadCount(logPath) != 0 {
		t.Fatal("failed projection must not queue")
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatalf("recovery after permission restore: %v", err)
	}
	if queueThreadCount(logPath) != 1 {
		t.Fatalf("recovered projection must queue once, got %d", queueThreadCount(logPath))
	}
}

func TestCanceledTerminalWakesOnceWithSupersession(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	thread := "dddddddd-dddd-dddd-dddd-ddddddddddd1"
	writeWakeConfig(t, root, bin, "wake-proj", thread, "mgr", true)
	mw := testWakeCfg(bin, "wake-proj", thread, "mgr")
	tk := newTask(root, testCfg(), typeSequence, "cancel-wake", "/tmp", []string{"p"}, 5)
	tk.Project = "wake-proj"
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if err := cmdSetStatus([]string{"-root", root, tk.ID}, "hold"); err != nil {
		t.Fatal(err)
	}
	if err := cmdSetStatus([]string{"-root", root, tk.ID}, "cancel"); err != nil {
		t.Fatal(err)
	}
	rows := countWakeRows(t, root)
	canceled := 0
	held := 0
	for _, row := range rows {
		if row.TaskID != tk.ID {
			continue
		}
		switch row.EventType {
		case evCanceled:
			canceled++
		case evHeld:
			held++
		}
	}
	if canceled != 1 {
		t.Fatalf("committed cancel must emit one wake row, canceled=%d held=%d rows=%+v", canceled, held, rows)
	}
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatal(err)
	}
	if queueThreadCount(logPath) != 1 {
		t.Fatalf("canceled wake queue=%d", queueThreadCount(logPath))
	}
	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), "type="+evCanceled) {
		t.Fatalf("payload missing canceled: %s", data)
	}
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatal(err)
	}
	if queueThreadCount(logPath) != 1 {
		t.Fatalf("canceled wake duplicated: %d", queueThreadCount(logPath))
	}
}

func TestInvalidSubscriptionFilterDoesNotConsumeCursor(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	thread := "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeee1"
	mw := testWakeCfg(bin, "wake-proj", thread, "mgr")
	mw.Subscriptions[0].EventTypes = []string{"not-a-type"}
	_ = heldCommittedTask(t, root, "wake-proj", "bad-filter")
	if err := managerWakeOnce(root, mw); err == nil || err.Error() != "invalid_event_type" {
		t.Fatalf("invalid event filter must fail, got %v", err)
	}
	if queueThreadCount(logPath) != 0 {
		t.Fatal("invalid filter must not queue")
	}
	cur, _, _ := loadManagerWakeCursor(root, "mgr")
	if cur != nil && cur.OutboxSeq != 0 {
		t.Fatalf("invalid filter consumed cursor: %+v", cur)
	}
}

func TestOverlappingSameThreadSubscriptionsDedupeEventIDs(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	thread := "ffffffff-ffff-ffff-ffff-fffffffffff1"
	mw := &ManagerWakeConfig{
		Enabled:     true,
		CodexBin:    bin,
		WatchdogSec: managerWakeWatchdogSec,
		Subscriptions: []ManagerWakeSubscription{
			{ID: "mgr-a", ThreadID: thread, Projects: []string{"wake-proj"}, Enabled: true},
			{ID: "mgr-b", ThreadID: thread, Projects: []string{"wake-proj"}, Enabled: true},
		},
	}
	tk := heldCommittedTask(t, root, "wake-proj", "overlap-thread")
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatal(err)
	}
	if queueThreadCount(logPath) != 1 {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("same-thread overlap must queue once, got %d %s", queueThreadCount(logPath), data)
	}
	held := mustHeldEvent(t, root, tk.ID)
	wantID := wakeEventID(tk.ID, held.Seq, held.TransitionID)
	idsA, _, _ := loadManagerWakeReceiptIDs(root, "mgr-a")
	if !idsA[wantID] {
		t.Fatal("first subscription must persist destination receipt")
	}
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatal(err)
	}
	if queueThreadCount(logPath) != 1 {
		t.Fatalf("overlap restart duplicated: %d", queueThreadCount(logPath))
	}
}

func TestWakeCurrentnessRaceBeforeQueueDropsStaleTurn(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	thread := "12121212-1212-1212-1212-1212121212a1"
	mw := testWakeCfg(bin, "wake-proj", thread, "mgr")
	tk := heldCommittedTask(t, root, "wake-proj", "toctou-current")
	orig := managerWakeBeforeQueue
	t.Cleanup(func() { managerWakeBeforeQueue = orig })
	managerWakeBeforeQueue = func() {
		if err := cmdSetStatus([]string{"-root", root, tk.ID}, "release"); err != nil {
			t.Errorf("release in race hook: %v", err)
		}
	}
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatalf("superseded scan must not fail the run, got %v", err)
	}
	if queueThreadCount(logPath) != 0 {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("stale turn queued after race: %s", data)
	}
}

func TestManagerWakeQueueChildStoppedWithRegisteredGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group stop is POSIX")
	}
	origTimeout := managerWakeQueueTimeout
	t.Cleanup(func() { managerWakeQueueTimeout = origTimeout })
	managerWakeQueueTimeout = 8 * time.Second
	bin, _, startedPath, pidPath, descPath := fakeCodexQueueHangBin(t)
	msg := "cardex-wake v1 n=1 high_water=1 sub=mgr\n" +
		"t1 type=held project=p status=held reason=user_hold transition=tid wake=t1:1:tid"
	errCh := make(chan error, 1)
	go func() {
		errCh <- defaultManagerWakeQueue(bin, "15151515-1515-1515-1515-151515151515", msg)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(startedPath); err == nil {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatal("queue child did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	leader := readPIDFile(t, pidPath)
	desc := readPIDFile(t, descPath)
	killRegisteredProcGroups()
	waitGone(t, leader, 2*time.Second)
	waitGone(t, desc, 2*time.Second)
	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("queue did not return after registered-group kill")
	}
}

func TestMalformedTaskJSONBlocksWakeReconcile(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	mw := testWakeCfg(bin, "wake-proj", "33333333-3333-3333-3333-3333333333a1", "mgr")
	if err := os.MkdirAll(tasksDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir(root), "t0101-0101-dead.json"), []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := managerWakeOnce(root, mw); err == nil {
		t.Fatal("malformed task must fail watchdog reconcile")
	}
	if loadManagerWakeErrorClass(root) == "" {
		t.Fatal("malformed task must leave durable error")
	}
	if queueThreadCount(logPath) != 0 {
		t.Fatal("malformed task must not queue")
	}
}

func TestInvalidDirPrefixAndTaskIDDoNotAck(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	thread := "44444444-4444-4444-4444-4444444444a1"
	mw := testWakeCfg(bin, "wake-proj", thread, "mgr")
	mw.Subscriptions[0].Projects = nil
	mw.Subscriptions[0].DirPrefixes = []string{"../escape"}
	_ = heldCommittedTask(t, root, "wake-proj", "bad-dir")
	if err := managerWakeOnce(root, mw); err == nil || err.Error() != "invalid_dir_prefix" {
		t.Fatalf("dir prefix must fail closed, got %v", err)
	}
	mw.Subscriptions[0].DirPrefixes = nil
	mw.Subscriptions[0].TaskIDs = []string{"../t"}
	if err := managerWakeOnce(root, mw); err == nil || err.Error() != "invalid_task_id" {
		t.Fatalf("task id must fail closed, got %v", err)
	}
	if queueThreadCount(logPath) != 0 {
		t.Fatal("invalid closed config queued")
	}
}

func TestClaimedInflightForeignIDsStayFailClosed(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	thread := "55555555-5555-5555-5555-5555555555a1"
	mw := testWakeCfg(bin, "wake-proj", thread, "mgr")
	if err := saveManagerWakeInflightPhase(root, "mgr", thread, []managerWakeOutboxRow{{WakeEventID: "ghost:1:tid"}}, 1, inflightPhaseClaimed); err != nil {
		t.Fatal(err)
	}
	before := inflightRaw(t, root, "mgr")
	assertUncertainNoRetry(t, root, mw, logPath, before)
	if queueThreadCount(logPath) != 0 {
		t.Fatal("foreign claimed inflight must not invent a wake")
	}
}

func TestRunningCancelRecurrenceWakesOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group cancel is POSIX")
	}
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	thread := "66666666-6666-6666-6666-6666666666a1"
	writeWakeConfig(t, root, bin, "wake-proj", thread, "mgr", true)
	mw := testWakeCfg(bin, "wake-proj", thread, "mgr")
	ws := t.TempDir()
	hang := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(hang, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := runTaskCfg(t, hang)
	tk := newTask(root, cfg, typeSequence, "running-cancel-wake", ws, []string{"do the work"}, 5)
	tk.Project = "wake-proj"
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
	if err := cmdSetStatus([]string{"-root", root, tk.ID}, "cancel"); err != nil {
		t.Fatalf("cli cancel: %v", err)
	}
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("runner did not return after cancel")
	}
	rows := countWakeRows(t, root)
	n := 0
	for _, row := range rows {
		if row.TaskID == tk.ID && row.EventType == evCanceled {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("running cancel must project one canceled wake, got %d rows=%+v", n, rows)
	}
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatal(err)
	}
	if queueThreadCount(logPath) != 1 {
		t.Fatalf("running cancel queue=%d", queueThreadCount(logPath))
	}
}
