package main

import (
	"os"
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
