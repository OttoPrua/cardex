package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWakeDeliveryPreSpawnStartFailureRetainsClaimedAndRetriesOnce(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	st, err := os.Stat(bin)
	if err != nil || st.Mode()&0o111 == 0 {
		t.Fatalf("need a previously stat-able executable, stat=%v err=%v", st, err)
	}
	origQ := managerWakeQueue
	origHook := managerWakeBeforeStart
	t.Cleanup(func() {
		managerWakeQueue = origQ
		managerWakeBeforeStart = origHook
		_ = os.Chmod(bin, 0o755)
	})
	managerWakeQueue = defaultManagerWakeQueue

	thread := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbb1"
	mw := testWakeCfg(bin, "wake-proj", thread, "mgr")
	tk := heldCommittedTask(t, root, "wake-proj", "pre-spawn-start-fail")
	held := mustHeldEvent(t, root, tk.ID)
	wantID := wakeEventID(tk.ID, held.Seq, held.TransitionID)

	startHookRan := false
	managerWakeBeforeStart = func() {
		startHookRan = true
		if _, err := os.Stat(bin); err != nil {
			t.Errorf("bin must remain stat-able immediately before Start: %v", err)
			return
		}
		if err := os.Chmod(bin, 0); err != nil {
			t.Errorf("chmod to fail Start: %v", err)
		}
	}
	err = managerWakeOnce(root, mw)
	if !startHookRan {
		t.Fatal("defaultManagerWakeQueue cmd.Start path was not exercised")
	}
	if err == nil || err.Error() != "queue_start_failed" {
		t.Fatalf("definite pre-spawn Start failure want queue_start_failed, got %v", err)
	}
	if queueThreadCount(logPath) != 0 {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("Start failure must not run the child, log=%s", data)
	}
	rec, class, err := loadManagerWakeInflight(root, "mgr")
	if err != nil || class != "" || rec == nil || rec.Phase != inflightPhaseClaimed {
		t.Fatalf("pre-spawn Start failure must keep claimed inflight phase=%v class=%q err=%v", rec, class, err)
	}
	ids, rclass, rerr := loadManagerWakeReceiptIDs(root, "mgr")
	if rerr != nil || rclass != "" || len(ids) != 0 {
		t.Fatalf("Start failure must not persist receipts class=%q err=%v ids=%v", rclass, rerr, ids)
	}
	cur, cclass, cerr := loadManagerWakeCursor(root, "mgr")
	if cerr != nil || cclass != "" || cur == nil || cur.OutboxSeq != 0 {
		t.Fatalf("Start failure must not advance cursor seq=%v class=%q err=%v", cur, cclass, cerr)
	}
	if got := loadManagerWakeErrorClass(root); got != "queue_start_failed" {
		t.Fatalf("closed allowlisted error class=%q", got)
	}

	managerWakeBeforeStart = origHook
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatalf("claimed Start-failure inflight must retry after restore, got %v", err)
	}
	if queueThreadCount(logPath) != 1 {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("retry queue calls=%d payload=%s", queueThreadCount(logPath), data)
	}
	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), "wake="+wantID) {
		t.Fatalf("retry missing event-id contract: %s", data)
	}
	ids, rclass, rerr = loadManagerWakeReceiptIDs(root, "mgr")
	if rerr != nil || rclass != "" || !ids[wantID] {
		t.Fatalf("retry receipts class=%q err=%v ids=%v", rclass, rerr, ids)
	}
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatal(err)
	}
	if queueThreadCount(logPath) != 1 {
		t.Fatalf("restart duplicated wake: %d", queueThreadCount(logPath))
	}
}

func TestWakeCurrentnessPostRebindPreStartLockBlocksStaleSpawn(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	origQ := managerWakeQueue
	origHook := managerWakeBeforeStart
	origSpawned := managerWakeQueueSpawned
	t.Cleanup(func() {
		managerWakeQueue = origQ
		managerWakeBeforeStart = origHook
		managerWakeQueueSpawned = origSpawned
	})
	managerWakeQueue = defaultManagerWakeQueue

	thread := "cccccccc-cccc-cccc-cccc-ccccccccccc1"
	mw := testWakeCfg(bin, "wake-proj", thread, "mgr")
	tk := heldCommittedTask(t, root, "wake-proj", "toctou-post-rebind")
	held := mustHeldEvent(t, root, tk.ID)
	wantID := wakeEventID(tk.ID, held.Seq, held.TransitionID)

	competitorDone := make(chan struct{})
	var generationAtStart, statusAtStart string
	observedAtSpawn := false
	managerWakeBeforeStart = func() {
		mu := lockForTaskControl(tk.ID)
		if mu.TryLock() {
			mu.Unlock()
			if err := cmdSetStatus([]string{"-root", root, tk.ID}, "release"); err != nil {
				t.Errorf("unprotected gap allowed competing release: %v", err)
			}
			close(competitorDone)
		} else {
			go func() {
				defer close(competitorDone)
				if err := cmdSetStatus([]string{"-root", root, tk.ID}, "release"); err != nil {
					t.Errorf("competing release after Start: %v", err)
				}
			}()
		}
	}
	managerWakeQueueSpawned = func() error {
		observedAtSpawn = true
		fresh, err := loadTask(root, tk.ID)
		if err != nil {
			t.Errorf("load task at Start: %v", err)
			return nil
		}
		generationAtStart = fresh.LastCommittedTransitionID
		statusAtStart = fresh.Status
		if origSpawned != nil {
			return origSpawned()
		}
		return nil
	}

	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatalf("current generation at Start must deliver, got %v", err)
	}
	select {
	case <-competitorDone:
	case <-time.After(10 * time.Second):
		t.Fatal("competing transition deadlocked")
	}
	if !observedAtSpawn {
		t.Fatal("successful Start boundary was not observed")
	}
	if statusAtStart != "held" || generationAtStart != held.TransitionID {
		t.Fatalf("Start generation=%q status=%q want held %s", generationAtStart, statusAtStart, held.TransitionID)
	}
	if queueThreadCount(logPath) != 1 {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("queue calls=%d payload=%s", queueThreadCount(logPath), data)
	}
	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), "wake="+wantID) {
		t.Fatalf("delivery must match generation at Start: %s", data)
	}
	ids, class, err := loadManagerWakeReceiptIDs(root, "mgr")
	if err != nil || class != "" || !ids[wantID] {
		t.Fatalf("receipts class=%q err=%v ids=%v", class, err, ids)
	}
	fresh, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status == "held" {
		t.Fatal("competing real transition never committed after Start")
	}
}

const wakeCrashAfterStartEnv = "CARDEX_WAKE_CRASH_AFTER_START"

func TestWakeSuccessfulStartBeforeDurableBoundaryCrashNoDuplicate(t *testing.T) {
	if os.Getenv(wakeCrashAfterStartEnv) == "1" {
		wakeCrashAfterSuccessfulStartHelper(t)
		return
	}
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	thread := "dddddddd-dddd-dddd-dddd-ddddddddddd1"
	tk := heldCommittedTask(t, root, "wake-proj", "start-gap-crash")
	held := mustHeldEvent(t, root, tk.ID)
	wantID := wakeEventID(tk.ID, held.Seq, held.TransitionID)

	cmd := exec.Command(os.Args[0], "-test.run=^TestWakeSuccessfulStartBeforeDurableBoundaryCrashNoDuplicate$")
	cmd.Env = append(os.Environ(),
		wakeCrashAfterStartEnv+"=1",
		"CARDEX_WAKE_CRASH_ROOT="+root,
		"CARDEX_WAKE_CRASH_BIN="+bin,
		"CARDEX_WAKE_CRASH_THREAD="+thread,
	)
	cmd.Dir = ""
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("crash child must not exit 0: %s", out)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 99 {
		t.Fatalf("crash child want exit 99, got %v out=%s", err, out)
	}
	if queueThreadCount(logPath) != 1 {
		t.Fatalf("successful Start must deliver once before crash, calls=%d log=%s", queueThreadCount(logPath), logPath)
	}
	rec, class, loadErr := loadManagerWakeInflight(root, "mgr")
	if loadErr != nil || class != "" || rec == nil || rec.Phase != inflightPhaseStarting {
		t.Fatalf("crash after successful Start must leave starting inflight phase=%v class=%q err=%v", rec, class, loadErr)
	}

	mw := testWakeCfg(bin, "wake-proj", thread, "mgr")
	if err := managerWakeOnce(root, mw); err == nil || err.Error() != "delivery_uncertain" {
		t.Fatalf("restart after Start-success crash want delivery_uncertain, got %v", err)
	}
	if queueThreadCount(logPath) != 1 {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("restart duplicated wake after Start-success crash: calls=%d payload=%s", queueThreadCount(logPath), data)
	}
	ids, rclass, rerr := loadManagerWakeReceiptIDs(root, "mgr")
	if rerr != nil || rclass != "" || ids[wantID] {
		t.Fatalf("crash before durable receipt must not mint a sender receipt class=%q err=%v ids=%v", rclass, rerr, ids)
	}
	if got := loadManagerWakeErrorClass(root); got != "delivery_uncertain" {
		t.Fatalf("durable class=%q want delivery_uncertain", got)
	}
}

func wakeCrashAfterSuccessfulStartHelper(t *testing.T) {
	t.Helper()
	root := os.Getenv("CARDEX_WAKE_CRASH_ROOT")
	bin := os.Getenv("CARDEX_WAKE_CRASH_BIN")
	thread := os.Getenv("CARDEX_WAKE_CRASH_THREAD")
	mw := testWakeCfg(bin, "wake-proj", thread, "mgr")
	managerWakeQueue = defaultManagerWakeQueue
	managerWakeCrashAt = "after_start"
	_ = managerWakeOnce(root, mw)
	os.Exit(12)
}

func TestWakeStartingBoundaryBeforeStartCrashIsUncertain(t *testing.T) {
	const env = "CARDEX_WAKE_CRASH_AFTER_STARTING"
	if os.Getenv(env) == "1" {
		root := os.Getenv("CARDEX_WAKE_CRASH_ROOT")
		bin := os.Getenv("CARDEX_WAKE_CRASH_BIN")
		thread := os.Getenv("CARDEX_WAKE_CRASH_THREAD")
		managerWakeQueue = defaultManagerWakeQueue
		managerWakeCrashAt = "after_starting"
		_ = managerWakeOnce(root, testWakeCfg(bin, "wake-proj", thread, "mgr"))
		os.Exit(12)
		return
	}
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	thread := "a7a7a7a7-a7a7-a7a7-a7a7-a7a7a7a7a7a7"
	_ = heldCommittedTask(t, root, "wake-proj", "starting-gap-crash")
	cmd := exec.Command(os.Args[0], "-test.run=^TestWakeStartingBoundaryBeforeStartCrashIsUncertain$")
	cmd.Env = append(os.Environ(),
		env+"=1",
		"CARDEX_WAKE_CRASH_ROOT="+root,
		"CARDEX_WAKE_CRASH_BIN="+bin,
		"CARDEX_WAKE_CRASH_THREAD="+thread,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("crash child must not exit 0: %s", out)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 99 {
		t.Fatalf("crash child want exit 99, got %v out=%s", err, out)
	}
	rec, class, loadErr := loadManagerWakeInflight(root, "mgr")
	if loadErr != nil || class != "" || rec == nil || rec.Phase != inflightPhaseStarting {
		t.Fatalf("pre-Start starting crash must leave starting inflight phase=%v class=%q err=%v", rec, class, loadErr)
	}
	if queueThreadCount(logPath) != 0 {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("pre-Start crash must not deliver, log=%s", data)
	}
	mw := testWakeCfg(bin, "wake-proj", thread, "mgr")
	if err := managerWakeOnce(root, mw); err == nil || err.Error() != "delivery_uncertain" {
		t.Fatalf("restart want delivery_uncertain, got %v", err)
	}
	if queueThreadCount(logPath) != 0 {
		t.Fatalf("starting crash retried a wake: %d", queueThreadCount(logPath))
	}
}

func TestWakeStartingPhaseRestartDeliveryUncertainNoDuplicate(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	thread := "f6f6f6f6-f6f6-f6f6-f6f6-f6f6f6f6f6f6"
	mw := testWakeCfg(bin, "wake-proj", thread, "mgr")
	tk := heldCommittedTask(t, root, "wake-proj", "starting-restart")
	held := mustHeldEvent(t, root, tk.ID)
	wantID := wakeEventID(tk.ID, held.Seq, held.TransitionID)
	if err := saveManagerWakeInflightPhase(root, "mgr", thread, []managerWakeOutboxRow{{WakeEventID: wantID}}, 1, inflightPhaseStarting); err != nil {
		t.Fatal(err)
	}
	if err := managerWakeOnce(root, mw); err == nil || err.Error() != "delivery_uncertain" {
		t.Fatalf("starting inflight must be delivery_uncertain, got %v", err)
	}
	if queueThreadCount(logPath) != 0 {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("starting inflight retried queue: %s", data)
	}
	if err := managerWakeOnce(root, mw); err == nil || err.Error() != "delivery_uncertain" {
		t.Fatalf("restart want delivery_uncertain, got %v", err)
	}
	if queueThreadCount(logPath) != 0 {
		t.Fatalf("starting inflight duplicated wake: %d", queueThreadCount(logPath))
	}
	if got := loadManagerWakeErrorClass(root); got != "delivery_uncertain" {
		t.Fatalf("durable class=%q", got)
	}
}

func TestWakePostStartQueueStartFailedTextIsNotDefinitePreSpawnFailure(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	thread := "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeee1"
	mw := testWakeCfg(bin, "wake-proj", thread, "mgr")
	tk := heldCommittedTask(t, root, "wake-proj", "text-alias-start")
	held := mustHeldEvent(t, root, tk.ID)
	wantID := wakeEventID(tk.ID, held.Seq, held.TransitionID)

	origQ := managerWakeQueue
	t.Cleanup(func() { managerWakeQueue = origQ })
	managerWakeQueue = func(bin, thread, message string) error {
		args := managerWakeQueueArgv(bin, thread, message)
		cmd := exec.Command(args[0], args[1:]...)
		if err := cmd.Run(); err != nil {
			return err
		}
		return fmt.Errorf("queue_start_failed")
	}

	err := managerWakeOnce(root, mw)
	if err == nil || err.Error() == "queue_start_failed" {
		t.Fatalf("post-Start text alias must not be definite pre-spawn failure, got %v", err)
	}
	if err.Error() != "delivery_uncertain" {
		t.Fatalf("post-Start delivery want delivery_uncertain, got %v", err)
	}
	if queueThreadCount(logPath) != 1 {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("child delivered once, calls=%d payload=%s", queueThreadCount(logPath), data)
	}
	if err := managerWakeOnce(root, mw); err == nil || err.Error() != "delivery_uncertain" {
		t.Fatalf("restart want delivery_uncertain, got %v", err)
	}
	if queueThreadCount(logPath) != 1 {
		t.Fatalf("text-alias misclassification retried a delivered wake: %d", queueThreadCount(logPath))
	}
	ids, class, rerr := loadManagerWakeReceiptIDs(root, "mgr")
	if rerr != nil || class != "" || ids[wantID] {
		t.Fatalf("no sender receipt after uncertain post-Start class=%q err=%v ids=%v", class, rerr, ids)
	}
}

func TestWakeOutgoingTaskIDMustBeExactCanonical(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	thread := "ffffffff-ffff-ffff-ffff-fffffffffff1"
	mw := testWakeCfg(bin, "wake-proj", thread, "mgr")
	tk := heldCommittedTask(t, root, "wake-proj", "canonical-id")
	held := mustHeldEvent(t, root, tk.ID)
	base := rebuildOutboxRowFromCommitted(1, tk, held)

	cases := []struct {
		name   string
		taskID string
	}{
		{"leading_space", " " + tk.ID},
		{"trailing_space", tk.ID + " "},
		{"tab_prefix", "\t" + tk.ID},
		{"traversal", "../cardex-wake-p1x2-escaped"},
		{"backslash_sep", `..\cardex-wake-p1x2-backslash`},
		{"slash_alias", "locks/../cardex-wake-p1x2-alias"},
		{"dot", "."},
		{"dotdot", ".."},
		{"reserved_once", managerWakeOnceLockID},
		{"reserved_outbox", managerWakeOutboxLockID},
	}
	locksBefore := listControlLockPaths(t, root)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forged := base
			forged.TaskID = tc.taskID
			forged.WakeEventID = rowWakeIdentity(forged)
			if !outboxRowIdentityConsistent(forged) && tc.taskID == "" {
				t.Fatal("empty already closed")
			}
			data, err := json.Marshal(forged)
			if err != nil {
				t.Fatal(err)
			}
			writeWakeOutboxLines(t, root, data)
			err = managerWakeOnce(root, mw)
			if err == nil || err.Error() != "outbox_identity_mismatch" {
				t.Fatalf("invalid TaskID %q must reject, got %v", tc.taskID, err)
			}
			if queueThreadCount(logPath) != 0 {
				payload, _ := os.ReadFile(logPath)
				t.Fatalf("invalid TaskID %q queued: %s", tc.taskID, payload)
			}
			cur, _, _ := loadManagerWakeCursor(root, "mgr")
			if cur != nil && cur.OutboxSeq != 0 {
				t.Fatalf("invalid TaskID %q must not ack cursor: %+v", tc.taskID, cur)
			}
		})
	}
	assertNoNewOrEscapedLocks(t, root, locksBefore)
}

func TestWakeOutgoingTaskIDDuplicateAliasRejected(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	thread := "a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1"
	mw := testWakeCfg(bin, "wake-proj", thread, "mgr")
	tk := heldCommittedTask(t, root, "wake-proj", "alias-dup")
	held := mustHeldEvent(t, root, tk.ID)
	canonical := rebuildOutboxRowFromCommitted(1, tk, held)
	aliased := canonical
	aliased.Seq = 2
	aliased.TaskID = " " + tk.ID + " "
	aliased.WakeEventID = rowWakeIdentity(aliased)
	data1, err := json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}
	data2, err := json.Marshal(aliased)
	if err != nil {
		t.Fatal(err)
	}
	writeWakeOutboxLines(t, root, data1, data2)
	locksBefore := listControlLockPaths(t, root)
	if err := managerWakeOnce(root, mw); err == nil || err.Error() != "outbox_identity_mismatch" {
		t.Fatalf("duplicate alias TaskID must reject, got %v", err)
	}
	if queueThreadCount(logPath) != 0 {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("alias pair queued: %s", data)
	}
	assertNoNewOrEscapedLocks(t, root, locksBefore)
}

func TestWakeTraversalTaskIDMustNotCreateEscapedLockOrQueue(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	thread := "b2b2b2b2-b2b2-b2b2-b2b2-b2b2b2b2b2b2"
	mw := testWakeCfg(bin, "wake-proj", thread, "mgr")
	cur, class, err := loadManagerWakeCursor(root, "mgr")
	if err != nil || class != "" || cur == nil {
		t.Fatalf("cursor class=%q err=%v", class, err)
	}
	metrics := &managerWakeMetrics{}
	badID := "../cardex-wake-p1x2-escaped"
	locksBefore := listControlLockPaths(t, root)
	qerr := queueWakeRows(root, mw.Subscriptions[0], []managerWakeOutboxRow{{
		Schema:       outboxSchemaV1,
		Seq:          1,
		TaskID:       badID,
		TaskEventSeq: 1,
		TransitionID: "tr-escape",
		EventType:    evHeld,
		Status:       statusHeld,
		WakeEventID:  wakeEventID(badID, 1, "tr-escape"),
	}}, 1, bin, cur, metrics, nil)
	escaped := filepath.Join(root, "control", "cardex-wake-p1x2-escaped.lock")
	if _, err := os.Stat(escaped); err == nil {
		t.Fatalf("escaped lock created: %s", escaped)
	}
	if qerr == nil || qerr.Error() != "outbox_identity_mismatch" {
		t.Fatalf("traversal TaskID must reject before lock, got %v", qerr)
	}
	if queueThreadCount(logPath) != 0 {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("traversal queued: %s", data)
	}
	assertNoNewOrEscapedLocks(t, root, locksBefore)
}

func TestWakeWhitespaceTaskIDDoesNotAliasCanonicalLockOrQueue(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	thread := "c3c3c3c3-c3c3-c3c3-c3c3-c3c3c3c3c3c3"
	mw := testWakeCfg(bin, "wake-proj", thread, "mgr")
	tk := heldCommittedTask(t, root, "wake-proj", "ws-alias-lock")
	held := mustHeldEvent(t, root, tk.ID)
	padded := " " + tk.ID + " "
	row := rebuildOutboxRowFromCommitted(1, tk, held)
	row.TaskID = padded
	row.WakeEventID = rowWakeIdentity(row)

	origQ := managerWakeQueue
	started := make(chan struct{})
	t.Cleanup(func() { managerWakeQueue = origQ })
	managerWakeQueue = func(bin, thread, message string) error {
		close(started)
		return nil
	}
	cur, class, err := loadManagerWakeCursor(root, "mgr")
	if err != nil || class != "" {
		t.Fatalf("cursor class=%q err=%v", class, err)
	}
	metrics := &managerWakeMetrics{}
	qerr := queueWakeRows(root, mw.Subscriptions[0], []managerWakeOutboxRow{row}, 1, bin, cur, metrics, map[string]map[string]bool{})
	if qerr == nil || qerr.Error() != "outbox_identity_mismatch" {
		t.Fatalf("whitespace TaskID must reject, got %v", qerr)
	}
	select {
	case <-started:
		t.Fatal("whitespace TaskID must not start queue process")
	default:
	}
	mu := lockForTaskControl(tk.ID)
	if !mu.TryLock() {
		t.Fatal("whitespace TaskID aliased onto the canonical task lock")
	}
	mu.Unlock()
	if queueThreadCount(logPath) != 0 {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("whitespace identity queued: %s", data)
	}
}

func TestWakeCanonicalTaskIDCurrentnessStillSerializesCompetingTransition(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	thread := "d4d4d4d4-d4d4-d4d4-d4d4-d4d4d4d4d4d4"
	mw := testWakeCfg(bin, "wake-proj", thread, "mgr")
	tk := heldCommittedTask(t, root, "wake-proj", "canonical-compete")
	held := mustHeldEvent(t, root, tk.ID)
	wantID := wakeEventID(tk.ID, held.Seq, held.TransitionID)

	origQ := managerWakeQueue
	origHook := managerWakeBeforeStart
	t.Cleanup(func() {
		managerWakeQueue = origQ
		managerWakeBeforeStart = origHook
	})
	managerWakeQueue = defaultManagerWakeQueue
	heldAtStart := false
	managerWakeBeforeStart = func() {
		mu := lockForTaskControl(tk.ID)
		if mu.TryLock() {
			mu.Unlock()
			t.Error("canonical currentness lock missing at Start")
			return
		}
		heldAtStart = true
	}
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatalf("canonical identity must deliver, got %v", err)
	}
	if !heldAtStart {
		t.Fatal("Start was not observed under the exact canonical task lock")
	}
	if queueThreadCount(logPath) != 1 {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("queue calls=%d payload=%s", queueThreadCount(logPath), data)
	}
	ids, class, err := loadManagerWakeReceiptIDs(root, "mgr")
	if err != nil || class != "" || !ids[wantID] {
		t.Fatalf("receipts class=%q err=%v ids=%v", class, err, ids)
	}
}

func TestWakeTaskLockReleasedAfterSpawnedWhileChildRuns(t *testing.T) {
	root := testRoot(t)
	bin, logPath, startedPath, _, _ := fakeCodexQueueHangBin(t)
	origTimeout := managerWakeQueueTimeout
	origQ := managerWakeQueue
	t.Cleanup(func() {
		managerWakeQueueTimeout = origTimeout
		managerWakeQueue = origQ
	})
	managerWakeQueueTimeout = 8 * time.Second
	managerWakeQueue = defaultManagerWakeQueue

	thread := "e5e5e5e5-e5e5-e5e5-e5e5-e5e5e5e5e5e5"
	mw := testWakeCfg(bin, "wake-proj", thread, "mgr")
	tk := heldCommittedTask(t, root, "wake-proj", "lock-release-wait")

	acquired := make(chan struct{})
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for {
			if _, err := os.Stat(startedPath); err == nil {
				break
			}
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		_ = withTaskControlLock(root, tk.ID, func() error {
			close(acquired)
			return nil
		})
	}()

	errCh := make(chan error, 1)
	go func() { errCh <- managerWakeOnce(root, mw) }()
	select {
	case <-acquired:
	case runErr := <-errCh:
		t.Fatalf("queue returned before competing transition acquired task lock: %v", runErr)
	case <-time.After(4 * time.Second):
		t.Fatal("task lock still held through queue child Wait")
	}
	if queueThreadCount(logPath) != 1 {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("hanging child must have started, log=%s", data)
	}
	killRegisteredProcGroups()
	select {
	case <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("wake once did not return after child stop")
	}
}

func writeWakeOutboxLines(t *testing.T, root string, lines ...[]byte) {
	t.Helper()
	path := managerWakeOutboxPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var payload []byte
	for _, line := range lines {
		payload = append(payload, line...)
		payload = append(payload, '\n')
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
}

func listControlLockPaths(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	control := controlDir(root)
	_ = filepath.Walk(control, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".lock") {
			rel, relErr := filepath.Rel(control, path)
			if relErr == nil {
				out[rel] = true
			}
		}
		return nil
	})
	return out
}

func assertNoNewOrEscapedLocks(t *testing.T, root string, before map[string]bool) {
	t.Helper()
	after := listControlLockPaths(t, root)
	locksDir := filepath.Join(controlDir(root), "locks")
	allowedNew := map[string]bool{
		filepath.Join("locks", managerWakeOnceLockID+".lock"):   true,
		filepath.Join("locks", managerWakeOutboxLockID+".lock"): true,
	}
	for rel := range after {
		abs := filepath.Join(controlDir(root), rel)
		inLocks, err := filepath.Rel(locksDir, abs)
		if err != nil || strings.HasPrefix(inLocks, "..") || strings.Contains(inLocks, string(filepath.Separator)) {
			t.Fatalf("escaped lock %s", abs)
		}
		if before[rel] || allowedNew[rel] {
			continue
		}
		t.Fatalf("invalid identity created lock %s", abs)
	}
}

func overlappingStartingDedupeCfg(bin, project, firstThread, originThread string, originEnabled, includeOrigin bool) *ManagerWakeConfig {
	mw := &ManagerWakeConfig{
		Enabled:     true,
		CodexBin:    bin,
		WatchdogSec: managerWakeWatchdogSec,
		Subscriptions: []ManagerWakeSubscription{{
			ID:       "first",
			ThreadID: firstThread,
			Projects: []string{project},
			Enabled:  true,
		}},
	}
	if includeOrigin {
		mw.Subscriptions = append(mw.Subscriptions, ManagerWakeSubscription{
			ID:       "origin",
			ThreadID: originThread,
			Projects: []string{project},
			Enabled:  originEnabled,
		})
	}
	return mw
}

func assertStartingInflightDoesNotDuplicateDestination(t *testing.T, root, logPath, originID, wantID string, mw *ManagerWakeConfig) {
	t.Helper()
	err := managerWakeOnce(root, mw)
	data, _ := os.ReadFile(logPath)
	if queueThreadCount(logPath) != 0 {
		t.Fatalf("starting inflight destination dedupe leaked a duplicate queue: %d payload=%s", queueThreadCount(logPath), data)
	}
	if strings.Contains(string(data), "wake="+wantID) {
		t.Fatalf("starting inflight destination dedupe leaked wake=%s: %s", wantID, data)
	}
	if err == nil || err.Error() != "delivery_uncertain" {
		t.Fatalf("unresolved starting inflight want delivery_uncertain, got %v", err)
	}
	if got := loadManagerWakeErrorClass(root); got != "delivery_uncertain" {
		t.Fatalf("durable class=%q want delivery_uncertain", got)
	}
	rec, class, loadErr := loadManagerWakeInflight(root, originID)
	if loadErr != nil || class != "" || rec == nil || rec.Phase != inflightPhaseStarting {
		t.Fatalf("origin starting inflight must remain phase=%v class=%q err=%v", rec, class, loadErr)
	}
	ids, rclass, rerr := loadManagerWakeReceiptIDs(root, "first")
	if rerr != nil || rclass != "" || ids[wantID] {
		t.Fatalf("overlapping subscription must not mint a duplicate receipt class=%q err=%v ids=%v", rclass, rerr, ids)
	}
}

func TestWakeStartingInflightNotSeededIntoDestinationDedupeOverlapOrder(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	thread := "7b7b7b7b-7b7b-7b7b-7b7b-7b7b7b7b7b01"
	mw := overlappingStartingDedupeCfg(bin, "wake-proj", thread, thread, true, true)
	tk := heldCommittedTask(t, root, "wake-proj", "starting-dest-overlap-order")
	held := mustHeldEvent(t, root, tk.ID)
	wantID := wakeEventID(tk.ID, held.Seq, held.TransitionID)
	if err := saveManagerWakeInflightPhase(root, "origin", thread, []managerWakeOutboxRow{{WakeEventID: wantID}}, 1, inflightPhaseStarting); err != nil {
		t.Fatal(err)
	}
	assertStartingInflightDoesNotDuplicateDestination(t, root, logPath, "origin", wantID, mw)
}

func TestWakeStartingInflightDisabledOriginDestinationDedupe(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	thread := "7b7b7b7b-7b7b-7b7b-7b7b-7b7b7b7b7b02"
	mw := overlappingStartingDedupeCfg(bin, "wake-proj", thread, thread, false, true)
	tk := heldCommittedTask(t, root, "wake-proj", "starting-dest-disabled-origin")
	held := mustHeldEvent(t, root, tk.ID)
	wantID := wakeEventID(tk.ID, held.Seq, held.TransitionID)
	if err := saveManagerWakeInflightPhase(root, "origin", thread, []managerWakeOutboxRow{{WakeEventID: wantID}}, 1, inflightPhaseStarting); err != nil {
		t.Fatal(err)
	}
	assertStartingInflightDoesNotDuplicateDestination(t, root, logPath, "origin", wantID, mw)
}

func TestWakeStartingInflightRemovedOriginDestinationDedupe(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	thread := "7b7b7b7b-7b7b-7b7b-7b7b-7b7b7b7b7b03"
	mw := overlappingStartingDedupeCfg(bin, "wake-proj", thread, thread, false, false)
	tk := heldCommittedTask(t, root, "wake-proj", "starting-dest-removed-origin")
	held := mustHeldEvent(t, root, tk.ID)
	wantID := wakeEventID(tk.ID, held.Seq, held.TransitionID)
	if err := saveManagerWakeInflightPhase(root, "origin", thread, []managerWakeOutboxRow{{WakeEventID: wantID}}, 1, inflightPhaseStarting); err != nil {
		t.Fatal(err)
	}
	assertStartingInflightDoesNotDuplicateDestination(t, root, logPath, "origin", wantID, mw)
}

func TestWakeStartingInflightCanonicalThreadAliasDestinationDedupe(t *testing.T) {
	const canonical = "7b7b7b7b-7b7b-41b4-a1b4-7b7b7b7b7b04"
	const alias = "7B7B7B7B-7B7B-41B4-A1B4-7B7B7B7B7B04"
	cases := []struct {
		name         string
		firstThread  string
		originThread string
	}{
		{"upper_inflight_lower_first", canonical, alias},
		{"lower_inflight_upper_first", alias, canonical},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := testRoot(t)
			bin, logPath := fakeCodexQueueBin(t, 0)
			mw := overlappingStartingDedupeCfg(bin, "wake-proj", tc.firstThread, tc.originThread, true, true)
			tk := heldCommittedTask(t, root, "wake-proj", "starting-dest-alias-"+tc.name)
			held := mustHeldEvent(t, root, tk.ID)
			wantID := wakeEventID(tk.ID, held.Seq, held.TransitionID)
			if err := saveManagerWakeInflightPhase(root, "origin", tc.originThread, []managerWakeOutboxRow{{WakeEventID: wantID}}, 1, inflightPhaseStarting); err != nil {
				t.Fatal(err)
			}
			assertStartingInflightDoesNotDuplicateDestination(t, root, logPath, "origin", wantID, mw)
			data, _ := os.ReadFile(logPath)
			if strings.Contains(string(data), "--thread") && !strings.Contains(string(data), "--thread\n"+tc.firstThread+"\n") {
				t.Fatalf("queue argv must keep first subscription thread %q: %s", tc.firstThread, data)
			}
		})
	}
}

func TestWakeClaimedInflightDoesNotSuppressOverlappingFirstDelivery(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	thread := "7b7b7b7b-7b7b-7b7b-7b7b-7b7b7b7b7b05"
	alias := "7B7B7B7B-7B7B-7B7B-7B7B-7B7B7B7B7B05"
	mw := overlappingStartingDedupeCfg(bin, "wake-proj", alias, thread, false, true)
	tk := heldCommittedTask(t, root, "wake-proj", "claimed-dest-no-suppress")
	held := mustHeldEvent(t, root, tk.ID)
	wantID := wakeEventID(tk.ID, held.Seq, held.TransitionID)
	if err := saveManagerWakeInflightPhase(root, "origin", thread, []managerWakeOutboxRow{{WakeEventID: wantID}}, 1, inflightPhaseClaimed); err != nil {
		t.Fatal(err)
	}
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatalf("claimed inflight must remain first-delivery retryable, got %v", err)
	}
	data, _ := os.ReadFile(logPath)
	if queueThreadCount(logPath) != 1 {
		t.Fatalf("claimed inflight suppressed a required first delivery: %d payload=%s", queueThreadCount(logPath), data)
	}
	if !strings.Contains(string(data), "wake="+wantID) {
		t.Fatalf("first delivery missing wake=%s: %s", wantID, data)
	}
	if !strings.Contains(string(data), "--thread\n"+alias+"\n") {
		t.Fatalf("queue argv must keep first subscription thread %q: %s", alias, data)
	}
	rec, class, err := loadManagerWakeInflight(root, "origin")
	if err != nil || class != "" || rec == nil || rec.Phase != inflightPhaseClaimed {
		t.Fatalf("disabled origin claimed inflight must remain phase=%v class=%q err=%v", rec, class, err)
	}
}

func TestWakeMalformedInflightInventoryFailsClosedBeforeQueue(t *testing.T) {
	thread := "7b7b7b7b-7b7b-7b7b-7b7b-7b7b7b7b7b06"
	cases := []struct {
		name  string
		write func(t *testing.T, root string)
	}{
		{
			name: "garbage_json",
			write: func(t *testing.T, root string) {
				writeInflightRaw(t, root, "origin", []byte("{nope\n"))
			},
		},
		{
			name: "foreign_subscription",
			write: func(t *testing.T, root string) {
				raw := `{
  "schema": "cardex.manager_wake.inflight.v1",
  "subscription_id": "other",
  "thread_id": "` + thread + `",
  "phase": "starting",
  "wake_event_ids": ["foreign:1:tid"],
  "updated_at": "2026-01-01T00:00:00Z"
}
`
				writeInflightRaw(t, root, "origin", []byte(raw))
			},
		},
		{
			name: "noncanonical_thread",
			write: func(t *testing.T, root string) {
				raw := `{
  "schema": "cardex.manager_wake.inflight.v1",
  "subscription_id": "origin",
  "thread_id": "{` + thread + `}",
  "phase": "starting",
  "wake_event_ids": ["brace:1:tid"],
  "updated_at": "2026-01-01T00:00:00Z"
}
`
				writeInflightRaw(t, root, "origin", []byte(raw))
			},
		},
		{
			name: "path_unsafe_name",
			write: func(t *testing.T, root string) {
				dir := managerWakeInflightDir(root)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "bad name.json")
				if err := os.WriteFile(path, []byte(`{"schema":"cardex.manager_wake.inflight.v1"}
`), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := testRoot(t)
			bin, logPath := fakeCodexQueueBin(t, 0)
			mw := overlappingStartingDedupeCfg(bin, "wake-proj", thread, thread, false, false)
			_ = heldCommittedTask(t, root, "wake-proj", "malformed-inflight-"+tc.name)
			tc.write(t, root)
			err := managerWakeOnce(root, mw)
			if err == nil {
				t.Fatal("malformed inflight inventory must fail closed")
			}
			if queueThreadCount(logPath) != 0 {
				data, _ := os.ReadFile(logPath)
				t.Fatalf("malformed inflight inventory queued: %s", data)
			}
			if got := loadManagerWakeErrorClass(root); got == "" {
				t.Fatal("malformed inflight inventory must leave a durable error")
			}
		})
	}
}
