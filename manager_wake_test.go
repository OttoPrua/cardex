package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func fakeCodexQueueBin(t *testing.T, exit int) (bin, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "argv.log")
	bin = filepath.Join(dir, "codex")
	script := "#!/bin/sh\n" +
		"log=" + shSingleQuote(logPath) + "\n" +
		"printf '%s\\n' \"$0\" \"$@\" >> \"$log\"\n" +
		"exit " + strconv.Itoa(exit) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logPath
}

func testWakeCfg(bin, project, thread, subID string) *ManagerWakeConfig {
	return &ManagerWakeConfig{
		Enabled:     true,
		CodexBin:    bin,
		WatchdogSec: managerWakeWatchdogSec,
		Subscriptions: []ManagerWakeSubscription{{
			ID:       subID,
			ThreadID: thread,
			Projects: []string{project},
			Enabled:  true,
		}},
	}
}

func heldCommittedTask(t *testing.T, root, project, title string) *Task {
	t.Helper()
	cfg := testCfg()
	tk := newTask(root, cfg, typeSequence, title, "/tmp", []string{"p"}, 5)
	tk.Project = project
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if err := cmdSetStatus([]string{"-root", root, tk.ID}, "hold"); err != nil {
		t.Fatal(err)
	}
	fresh, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	return fresh
}

func countWakeRows(t *testing.T, root string) []managerWakeOutboxRow {
	t.Helper()
	rows, class, err := loadManagerWakeOutbox(root)
	if err != nil || class != "" {
		t.Fatalf("outbox load: class=%q err=%v", class, err)
	}
	return rows
}

func TestProductionCommittedTransitionWakeSeam(t *testing.T) {
	root := testRoot(t)
	bin, _ := fakeCodexQueueBin(t, 0)
	writeWakeConfig(t, root, bin, "wake-proj", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "mgr", true)
	cfg := testCfg()

	queued := newTask(root, cfg, typeSequence, "queued no wake", "/tmp", []string{"p"}, 5)
	if err := saveTask(root, queued); err != nil {
		t.Fatal(err)
	}
	emitTaskEvent(root, queued.ID, evHeld, "runner", statusHeld, queued.Step, map[string]any{"reason": "unjournaled"})
	if rows := countWakeRows(t, root); len(rows) != 0 {
		t.Fatalf("unjournaled event emitted wake rows: %+v", rows)
	}

	prepared := newTask(root, cfg, typeSequence, "prepared no wake", "/tmp", []string{"p"}, 5)
	if err := saveTask(root, prepared); err != nil {
		t.Fatal(err)
	}
	tid := newTransitionID()
	rec := &TransitionRecord{
		TransitionID:     tid,
		TaskID:           prepared.ID,
		ExpectedRevision: prepared.Revision,
		NewRevision:      prepared.Revision + 1,
		EventType:        evHeld,
		Status:           statusHeld,
		Actor:            "cli:hold",
		State:            transitionPrepared,
		CreatedAt:        time.Now().Format(time.RFC3339Nano),
	}
	if err := writeTransition(root, rec); err != nil {
		t.Fatal(err)
	}
	fake := TaskEvent{Type: evHeld, Status: statusHeld, Seq: 1, TransitionID: tid}
	if err := appendManagerWakeFromCommitted(root, prepared, fake); err != nil {
		t.Fatal(err)
	}
	if rows := countWakeRows(t, root); len(rows) != 0 {
		t.Fatalf("prepared transition emitted wake rows: %+v", rows)
	}

	held := newTask(root, cfg, typeSequence, "committed held wake", "/tmp", []string{"p"}, 5)
	if err := saveTask(root, held); err != nil {
		t.Fatal(err)
	}
	if err := cmdSetStatus([]string{"-root", root, held.ID}, "hold"); err != nil {
		t.Fatal(err)
	}
	heldRows := countWakeRows(t, root)
	if len(heldRows) != 1 || heldRows[0].EventType != evHeld || heldRows[0].TaskID != held.ID {
		t.Fatalf("committed held must emit exactly one wake row, got %+v", heldRows)
	}
	if err := cmdSetStatus([]string{"-root", root, held.ID}, "hold"); err != nil {
		t.Fatal(err)
	}
	if rows := countWakeRows(t, root); len(rows) != 1 {
		t.Fatalf("duplicate committed held must stay one row, got %+v", rows)
	}

	owner := newTask(root, cfg, typeSequence, "committed needs-owner wake", "/tmp", []string{"p"}, 5)
	if err := saveTask(root, owner); err != nil {
		t.Fatal(err)
	}
	fresh, err := loadTask(root, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := commitTaskTransition(root, fresh, transitionRequest{
		EventType:  evNeedsOwner,
		Actor:      "runner",
		Status:     fresh.Status,
		Step:       fresh.Step,
		Detail:     map[string]any{"reason": "custody_timeout", "reason_class": "needs_owner"},
		NeedsOwner: true,
	}); err != nil {
		t.Fatal(err)
	}
	rows := countWakeRows(t, root)
	ownerRows := 0
	heldStill := 0
	for _, row := range rows {
		switch {
		case row.TaskID == owner.ID && row.EventType == evNeedsOwner:
			ownerRows++
		case row.TaskID == held.ID && row.EventType == evHeld:
			heldStill++
		default:
			t.Fatalf("unexpected wake row: %+v", row)
		}
	}
	if ownerRows != 1 || heldStill != 1 {
		t.Fatalf("committed needs-owner must add exactly one row beside held, got owner=%d held=%d rows=%+v", ownerRows, heldStill, rows)
	}
}

func TestCrashWindowWakeDedupeDeterministicIdentity(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	tk := newTask(root, cfg, typeSequence, "crash-window dedupe", "/tmp", []string{"p"}, 5)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if err := cmdSetStatus([]string{"-root", root, tk.ID}, "hold"); err != nil {
		t.Fatal(err)
	}
	fresh, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	events, _, err := loadTaskEvents(root, fresh.ID)
	if err != nil {
		t.Fatal(err)
	}
	var held TaskEvent
	for _, ev := range events {
		if ev.Type == evHeld {
			held = ev
		}
	}
	if held.Seq == 0 || held.TransitionID == "" {
		t.Fatalf("expected committed held event, got %+v", events)
	}
	wantID := wakeEventID(fresh.ID, held.Seq, held.TransitionID)
	if wantID != fresh.ID+":"+strconv.FormatInt(held.Seq, 10)+":"+held.TransitionID {
		t.Fatalf("wake identity not deterministic: %q", wantID)
	}
	appendManagerWakeFromCommitted(root, fresh, held)
	appendManagerWakeFromCommitted(root, fresh, held)
	rows, class, err := loadManagerWakeOutbox(root)
	if err != nil || class != "" {
		t.Fatalf("outbox load: class=%q err=%v", class, err)
	}
	if len(rows) != 1 {
		t.Fatalf("crash-window recovery must keep one row, got %d %+v", len(rows), rows)
	}
	if rows[0].WakeEventID != wantID {
		t.Fatalf("wake id %q want %q", rows[0].WakeEventID, wantID)
	}
	if rows[0].TaskID != fresh.ID || rows[0].TaskEventSeq != held.Seq || rows[0].TransitionID != held.TransitionID {
		t.Fatalf("row identity drifted: %+v held=%+v", rows[0], held)
	}
	if err := os.Remove(managerWakeOutboxPath(root)); err != nil {
		t.Fatal(err)
	}
	reconcileManagerWakeOutbox(root)
	reconcileManagerWakeOutbox(root)
	recovered, _, err := loadManagerWakeOutbox(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].WakeEventID != wantID {
		t.Fatalf("lost-row recovery must rebuild the same identity: %+v", recovered)
	}
}

func TestStaleNeedsOwnerSupersededByCommittedTerminal(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	mw := testWakeCfg(bin, "wake-proj", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", "mgr")
	cfg := testCfg()
	tk := newTask(root, cfg, typeSequence, "needs-owner supersede", "/tmp", []string{"p"}, 5)
	tk.Project = "wake-proj"
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	fresh, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := commitTaskTransition(root, fresh, transitionRequest{
		EventType:  evNeedsOwner,
		Actor:      "runner",
		Status:     fresh.Status,
		Step:       fresh.Step,
		Detail:     map[string]any{"reason": "custody_timeout", "reason_class": "needs_owner"},
		NeedsOwner: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := cmdSetStatus([]string{"-root", root, tk.ID}, "hold"); err != nil {
		t.Fatal(err)
	}
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatal(err)
	}
	rows, _, err := loadManagerWakeOutbox(root)
	if err != nil {
		t.Fatal(err)
	}
	currentHeld := 0
	for _, row := range rows {
		tk, _ := findTaskAnywhere(root, row.TaskID)
		rebuilt, kind := bindCommittedWakeRow(root, tk, row)
		if kind != wakeBindOK {
			continue
		}
		if rebuilt.EventType == evNeedsOwner {
			t.Fatalf("stale needs_owner must not remain current: %+v", rebuilt)
		}
		if rebuilt.EventType == evHeld {
			currentHeld++
		}
	}
	if currentHeld != 1 {
		t.Fatalf("want current held projection, got %+v", rows)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if strings.Contains(got, evNeedsOwner) {
		t.Fatalf("adapter woke stale needs_owner: %s", got)
	}
	if !strings.Contains(got, "held") || !strings.Contains(got, tk.ID) {
		t.Fatalf("adapter missed current held: %s", got)
	}
}

func TestSemanticCrossValidationAgainstCommittedIdentity(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	mw := testWakeCfg(bin, "wake-proj", "cccccccc-cccc-cccc-cccc-cccccccccccc", "mgr")
	tk := heldCommittedTask(t, root, "wake-proj", "semantic xval")
	appendManagerWakeFromCommitted(root, tk, mustHeldEvent(t, root, tk.ID))
	rows, _, err := loadManagerWakeOutbox(root)
	if err != nil || len(rows) != 1 {
		t.Fatalf("seed row: %v %+v", err, rows)
	}
	forged := rows[0]
	forged.TransitionID = "forged-transition"
	data, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managerWakeOutboxPath(root), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := managerWakeOnce(root, mw); err == nil {
		t.Fatal("identity mismatch must fail closed")
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		payload, _ := os.ReadFile(logPath)
		t.Fatalf("mismatched identity must not queue: %s", payload)
	}
	rb := managerWakeReadback(root, mw)
	if rb["last_err_class"] != "outbox_identity_mismatch" {
		t.Fatalf("want outbox_identity_mismatch, got %+v", rb)
	}
}

func TestDurableVisibleOutboxErrors(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	mw := testWakeCfg(bin, "wake-proj", "dddddddd-dddd-dddd-dddd-dddddddddddd", "mgr")
	if err := os.MkdirAll(managerWakeDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managerWakeOutboxPath(root), []byte("{not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := managerWakeOnce(root, mw); err == nil {
		t.Fatal("corrupt outbox must fail closed")
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatal("corrupt outbox must not spawn queue")
	}
	raw, err := os.ReadFile(managerWakeErrorPath(root))
	if err != nil {
		t.Fatal(err)
	}
	var st managerWakeErrorState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if st.Class != "outbox_corrupt" {
		t.Fatalf("durable class=%q", st.Class)
	}
	low := strings.ToLower(string(raw))
	for _, bad := range []string{"not json", "invalid character", "prompt", "token=", "credential"} {
		if strings.Contains(low, bad) {
			t.Fatalf("error state leaked prose %q: %s", bad, raw)
		}
	}
	metrics := loadManagerWakeMetrics(root)
	if metrics.LastErrorClass != "outbox_corrupt" {
		t.Fatalf("metrics class=%q", metrics.LastErrorClass)
	}
	rb := managerWakeReadback(root, mw)
	if rb["last_err_class"] != "outbox_corrupt" {
		t.Fatalf("readback missing class: %+v", rb)
	}
}

func TestClosedTraversalSafeSubscriptionIDs(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	_ = heldCommittedTask(t, root, "wake-proj", "traversal")
	for _, id := range []string{"../passwd", "..", "a/b", "a\\b", ""} {
		mw := testWakeCfg(bin, "wake-proj", "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee", id)
		if err := managerWakeOnce(root, mw); err != nil {
			t.Fatalf("invalid id %q must fail closed without panic: %v", id, err)
		}
		if _, err := os.Stat(logPath); !os.IsNotExist(err) {
			t.Fatalf("traversal id %q must not spawn", id)
		}
		escape := filepath.Join(managerWakeDir(root), "passwd.json")
		if _, err := os.Stat(escape); err == nil {
			t.Fatalf("cursor escaped to %s", escape)
		}
		parent := filepath.Join(filepath.Dir(managerWakeCursorDir(root)), "passwd.json")
		if _, err := os.Stat(parent); err == nil {
			t.Fatalf("cursor escaped to %s", parent)
		}
		if p := managerWakeCursorPath(root, id); p != "" {
			t.Fatalf("cursor path for %q must be empty, got %s", id, p)
		}
	}
	mw := testWakeCfg(bin, "wake-proj", "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee", "mgr-1")
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatal(err)
	}
	curPath := managerWakeCursorPath(root, "mgr-1")
	rel, err := filepath.Rel(managerWakeCursorDir(root), curPath)
	if err != nil || strings.Contains(rel, "..") {
		t.Fatalf("valid cursor left the cursor dir: %s", curPath)
	}
}

func TestHardWatchdogDeltaZeroNoQueue(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	mw := testWakeCfg(bin, "wake-proj", "ffffffff-ffff-ffff-ffff-ffffffffffff", "mgr")
	mw.WatchdogSec = 7
	tk := newTask(root, testCfg(), typeSequence, "delta0", "/tmp", []string{"p"}, 5)
	tk.Project = "wake-proj"
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("delta=0 must not spawn queue, log=%q", data)
	}
	if _, err := os.Stat(managerWakeInflightPath(root, "mgr")); !os.IsNotExist(err) {
		t.Fatal("delta=0 with no inflight must not mint a claim")
	}
	metrics := loadManagerWakeMetrics(root)
	if metrics.WatchdogSec != managerWakeWatchdogSec {
		t.Fatalf("watchdog_sec=%d want %d", metrics.WatchdogSec, managerWakeWatchdogSec)
	}
	if metrics.QueueAttempts != 0 || metrics.NoDeltaScans < 1 {
		t.Fatalf("delta0 metrics: %+v", metrics)
	}
	rb := managerWakeReadback(root, mw)
	if rb["watchdog_sec"] != managerWakeWatchdogSec {
		t.Fatalf("readback watchdog: %+v", rb)
	}
	if rb["queue_attempts"] != 0 {
		t.Fatalf("delta=0 with no inflight must stay at zero model calls: %+v", rb)
	}
	diag := rb["diagnosis"].([]string)
	if !containsString(diag, "watchdog_sec_rejected") {
		t.Fatalf("non-canonical watchdog must be diagnosed: %v", diag)
	}
	plist, err := renderManagerWakePlist("/opt/homebrew/bin/cardex", root, 7, "/tmp/wake.log")
	if err != nil {
		t.Fatal(err)
	}
	sec, err := managerWakePlistStartInterval(plist)
	if err != nil || sec != managerWakeWatchdogSec {
		t.Fatalf("plist interval=%d err=%v", sec, err)
	}
}

func TestAdapterRestartAndCorruption(t *testing.T) {
	root := testRoot(t)
	okBin, okLog := fakeCodexQueueBin(t, 0)
	thread := "22222222-2222-2222-2222-222222222222"
	mw := testWakeCfg(okBin, "wake-proj", thread, "mgr")
	tk := heldCommittedTask(t, root, "wake-proj", "queue ok")
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatal(err)
	}
	cur, _, err := loadManagerWakeCursor(root, "mgr")
	if err != nil {
		t.Fatal(err)
	}
	if cur.OutboxSeq < 1 {
		t.Fatalf("cursor not advanced after success: %+v", cur)
	}
	data, err := os.ReadFile(okLog)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "queue") || !strings.Contains(got, "--thread") || !strings.Contains(got, thread) {
		t.Fatalf("argv recorder missing grammar: %q", got)
	}
	if !strings.Contains(got, "--message") {
		t.Fatalf("missing --message: %q", got)
	}
	if strings.Contains(got, "--idempotency-key") || strings.Contains(got, "--client-request-id") ||
		strings.Contains(got, "--request-id") || strings.Contains(got, "--dedupe-key") ||
		strings.Contains("\n"+got+"\n", "\n--id\n") {
		t.Fatalf("invented queue identity flag: %q", got)
	}
	if !strings.Contains(got, "wake=") {
		t.Fatalf("message missing wake_event_id contract: %q", got)
	}
	if !strings.Contains(got, tk.ID) {
		t.Fatalf("payload missing task id: %q", got)
	}
	before := cur.OutboxSeq
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatal(err)
	}
	cur2, _, _ := loadManagerWakeCursor(root, "mgr")
	if cur2.OutboxSeq != before {
		t.Fatalf("restart duplicated delivery cursor %d -> %d", before, cur2.OutboxSeq)
	}
	data2, _ := os.ReadFile(okLog)
	if strings.Count(string(data2), "--thread") != 1 {
		t.Fatalf("duplicate queue spawn: %q", data2)
	}

	if err := os.MkdirAll(managerWakeCursorDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managerWakeCursorPath(root, "mgr"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(okLog); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := managerWakeOnce(root, mw); err == nil {
		t.Fatal("corrupt cursor must fail closed")
	}
	if _, err := os.Stat(okLog); !os.IsNotExist(err) {
		t.Fatal("corrupt cursor must not spawn queue")
	}
}

func TestClosedSubscriptionRouting(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	mw := testWakeCfg(bin, "other-proj", "33333333-3333-3333-3333-333333333333", "mgr")
	other := newTask(root, testCfg(), typeSequence, "other", "/tmp", []string{"p"}, 5)
	other.Project = "other-proj"
	if err := saveTask(root, other); err != nil {
		t.Fatal(err)
	}
	_ = heldCommittedTask(t, root, "wake-proj", "routing")
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("non-matching project must not queue: %q", data)
	}
}

func TestSecretFreePayloadAndRows(t *testing.T) {
	root := testRoot(t)
	var captured string
	orig := managerWakeQueue
	defer func() { managerWakeQueue = orig }()
	managerWakeQueue = func(bin, thread, message string) error {
		captured = message
		return nil
	}
	bin, _ := fakeCodexQueueBin(t, 0)
	mw := testWakeCfg(bin, "wake-proj", "44444444-4444-4444-4444-444444444444", "mgr")
	cfg := testCfg()
	tk := newTask(root, cfg, typeSequence, "secret free", "/tmp", []string{"SECRET PROMPT TOKEN=abc"}, 5)
	tk.Project = "wake-proj"
	tk.LastError = "credential leak would be bad"
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if err := cmdSetStatus([]string{"-root", root, tk.ID}, "hold"); err != nil {
		t.Fatal(err)
	}
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatal(err)
	}
	if captured == "" {
		t.Fatal("expected a queue message")
	}
	low := strings.ToLower(captured)
	for _, bad := range []string{"secret prompt", "token=abc", "credential leak", tk.Prompts[0]} {
		if strings.Contains(low, strings.ToLower(bad)) {
			t.Fatalf("payload leaked %q: %q", bad, captured)
		}
	}
	if !strings.Contains(captured, tk.ID) || !strings.Contains(captured, "held") {
		t.Fatalf("payload missing coordinates: %q", captured)
	}
	rows, _, err := loadManagerWakeOutbox(root)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows: %v %+v", err, rows)
	}
	blob, err := json.Marshal(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	low = strings.ToLower(string(blob))
	for _, bad := range []string{"secret prompt", "token=abc", "credential leak", "prompt", tk.Prompts[0]} {
		if strings.Contains(low, strings.ToLower(bad)) {
			t.Fatalf("row leaked %q: %s", bad, blob)
		}
	}
}

func TestCorruptOutboxFailClosed(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	mw := testWakeCfg(bin, "wake-proj", "55555555-5555-5555-5555-555555555555", "mgr")
	if err := os.MkdirAll(managerWakeDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managerWakeOutboxPath(root), []byte("{not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := managerWakeOnce(root, mw); err == nil {
		t.Fatal("corrupt outbox must fail closed")
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatal("corrupt outbox must not spawn queue")
	}
	rb := managerWakeReadback(root, mw)
	if rb["last_err_class"] != "outbox_corrupt" && !containsString(rb["diagnosis"].([]string), "outbox_corrupt") {
		t.Fatalf("diagnosis missing: %+v", rb)
	}
}

func TestInvalidThreadFailClosedNoSpawn(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	mw := testWakeCfg(bin, "wake-proj", "not-a-uuid", "mgr")
	_ = heldCommittedTask(t, root, "wake-proj", "bad thread")
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatal("invalid thread must not spawn")
	}
	cur, _, _ := loadManagerWakeCursor(root, "mgr")
	if cur.LastErrorClass != "invalid_thread_id" {
		t.Fatalf("want invalid_thread_id, got %+v", cur)
	}
}

func TestManagerWakeDisabledNoSpawn(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	_ = heldCommittedTask(t, root, "wake-proj", "disabled")
	mw := &ManagerWakeConfig{Enabled: false, CodexBin: bin}
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatal("disabled must not spawn")
	}
	if err := managerWakeOnce(root, nil); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownProjectFailClosed(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	mw := testWakeCfg(bin, "no-such-project", "66666666-6666-6666-6666-666666666666", "mgr")
	_ = heldCommittedTask(t, root, "wake-proj", "unknown proj")
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatal("unknown project must not spawn")
	}
}

func TestUncommittedAndStaleEventNoWake(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	mw := testWakeCfg(bin, "wake-proj", "77777777-7777-7777-7777-777777777777", "mgr")
	tk := newTask(root, testCfg(), typeSequence, "stale wake", "/tmp", []string{"p"}, 5)
	tk.Project = "wake-proj"
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	emitTaskEvent(root, tk.ID, evHeld, "runner", statusHeld, tk.Step, map[string]any{"reason": "uncommitted"})
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatal("uncommitted held event must not queue")
	}
	if err := cmdSetStatus([]string{"-root", root, tk.ID}, "hold"); err != nil {
		t.Fatal(err)
	}
	if err := cmdSetStatus([]string{"-root", root, tk.ID}, "release"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(logPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("superseded held must not wake after release: %q", data)
	}
}

func TestCoalesceOneQueueCall(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	mw := testWakeCfg(bin, "wake-proj", "88888888-8888-8888-8888-888888888888", "mgr")
	a := heldCommittedTask(t, root, "wake-proj", "coalesce-a")
	b := heldCommittedTask(t, root, "wake-proj", "coalesce-b")
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "--thread") != 1 {
		t.Fatalf("multiple matching rows must coalesce into one queue call: %q", data)
	}
	got := string(data)
	if !strings.Contains(got, a.ID) || !strings.Contains(got, b.ID) {
		t.Fatalf("coalesced payload missing task ids: %q", got)
	}
}

func TestManagerWakeReadbackSecretFree(t *testing.T) {
	root := testRoot(t)
	bin, _ := fakeCodexQueueBin(t, 0)
	mw := testWakeCfg(bin, "wake-proj", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "mgr")
	tk := newTask(root, testCfg(), typeSequence, "readback", "/tmp", []string{"SECRET PROMPT TOKEN=abc"}, 5)
	tk.Project = "wake-proj"
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if err := cmdSetStatus([]string{"-root", root, tk.ID}, "hold"); err != nil {
		t.Fatal(err)
	}
	rb := managerWakeReadback(root, mw)
	blob, err := json.Marshal(rb)
	if err != nil {
		t.Fatal(err)
	}
	low := strings.ToLower(string(blob))
	for _, bad := range []string{"secret prompt", "token=abc", "api_key", "authorization"} {
		if strings.Contains(low, bad) {
			t.Fatalf("readback leaked %q: %s", bad, blob)
		}
	}
	if rb["outbox_high_water"] == nil || rb["watchdog_sec"] != managerWakeWatchdogSec {
		t.Fatalf("readback missing coordinates: %+v", rb)
	}
	if rb["enabled"] != true {
		t.Fatalf("enabled missing: %+v", rb)
	}
}

func TestQueueOkReceiptCrashRestartNoDuplicateWake(t *testing.T) {
	root := testRoot(t)
	bin, _ := fakeCodexQueueBin(t, 0)
	mw := testWakeCfg(bin, "wake-proj", "12121212-1212-1212-1212-121212121212", "mgr")
	tk := heldCommittedTask(t, root, "wake-proj", "queue-ok receipt-crash")
	held := mustHeldEvent(t, root, tk.ID)
	wantID := wakeEventID(tk.ID, held.Seq, held.TransitionID)

	var calls []string
	origQ := managerWakeQueue
	origR := managerWakePersistReceipts
	defer func() {
		managerWakeQueue = origQ
		managerWakePersistReceipts = origR
	}()
	managerWakeQueue = func(bin, thread, message string) error {
		calls = append(calls, message)
		if !strings.Contains(message, "wake="+wantID) {
			t.Fatalf("receiver contract missing wake id %q in %q", wantID, message)
		}
		argv := managerWakeQueueArgv(bin, thread, message)
		joined := strings.Join(argv, "\n")
		for _, bad := range []string{"--idempotency-key", "--client-request-id", "--request-id", "--dedupe-key", "--client-user-message-id"} {
			if strings.Contains(joined, bad) {
				t.Fatalf("invented flag %s: %q", bad, argv)
			}
		}
		return nil
	}
	managerWakePersistReceipts = func(root, subID string, rows []managerWakeOutboxRow) error {
		return fmt.Errorf("killed_after_queue_before_receipt")
	}

	if err := managerWakeOnce(root, mw); err == nil {
		t.Fatal("post-queue/pre-receipt crash must surface")
	}
	if len(calls) != 1 {
		t.Fatalf("queue calls=%d want 1", len(calls))
	}
	ids, class, err := loadManagerWakeReceiptIDs(root, "mgr")
	if err != nil || class != "" {
		t.Fatalf("receipt load: class=%q err=%v", class, err)
	}
	if ids[wantID] {
		t.Fatal("crash before receipt fsync must not leave a sender receipt")
	}
	cur, _, err := loadManagerWakeCursor(root, "mgr")
	if err != nil {
		t.Fatal(err)
	}
	if cur.OutboxSeq != 0 {
		t.Fatalf("cursor claimed delivery after receipt crash: %+v", cur)
	}

	if err := managerWakeOnce(root, mw); err == nil {
		t.Fatal("uncertain post-queue state must stay fail-closed")
	}
	if len(calls) != 1 {
		t.Fatalf("restart duplicated manager model turn: calls=%d second=%q", len(calls), calls)
	}
	if loadManagerWakeErrorClass(root) != "delivery_uncertain" {
		t.Fatalf("durable class=%q want delivery_uncertain", loadManagerWakeErrorClass(root))
	}
	cur, _, _ = loadManagerWakeCursor(root, "mgr")
	if cur.OutboxSeq != 0 {
		t.Fatalf("uncertain state must not silently ack cursor: %+v", cur)
	}
	metrics := loadManagerWakeMetrics(root)
	if metrics.WatchdogSec != managerWakeWatchdogSec {
		t.Fatalf("watchdog_sec=%d want %d", metrics.WatchdogSec, managerWakeWatchdogSec)
	}
	if metrics.QueueAttempts != 1 {
		t.Fatalf("watchdog must not spawn a second model queue: %+v", metrics)
	}
	rb := managerWakeReadback(root, mw)
	if rb["last_err_class"] != "delivery_uncertain" {
		t.Fatalf("readback missing delivery_uncertain: %+v", rb)
	}
	if rb["watchdog_sec"] != managerWakeWatchdogSec {
		t.Fatalf("readback watchdog: %+v", rb)
	}
	pending, _ := rb["pending"].(int)
	if pending < 1 {
		t.Fatalf("uncertain state must keep pending visible: %+v", rb)
	}
}

func TestQueueOkCursorFailRestartNoDuplicateWake(t *testing.T) {
	root := testRoot(t)
	bin, _ := fakeCodexQueueBin(t, 0)
	mw := testWakeCfg(bin, "wake-proj", "99999999-9999-9999-9999-999999999999", "mgr")
	tk := heldCommittedTask(t, root, "wake-proj", "queue-ok cursor-fail")
	held := mustHeldEvent(t, root, tk.ID)
	wantID := wakeEventID(tk.ID, held.Seq, held.TransitionID)

	var calls []string
	origQ := managerWakeQueue
	origC := managerWakeCommitCursor
	defer func() {
		managerWakeQueue = origQ
		managerWakeCommitCursor = origC
	}()
	managerWakeQueue = func(bin, thread, message string) error {
		calls = append(calls, message)
		if !strings.Contains(message, "wake="+wantID) {
			t.Fatalf("receiver contract missing wake id %q in %q", wantID, message)
		}
		argv := managerWakeQueueArgv(bin, thread, message)
		joined := strings.Join(argv, "\n")
		for _, bad := range []string{"--idempotency-key", "--client-request-id", "--request-id", "--dedupe-key"} {
			if strings.Contains(joined, bad) {
				t.Fatalf("invented flag %s: %q", bad, argv)
			}
		}
		return nil
	}
	saves := 0
	managerWakeCommitCursor = func(root string, cur *managerWakeCursor) error {
		saves++
		if saves == 1 {
			return fmt.Errorf("cursor_save_failed")
		}
		return saveManagerWakeCursor(root, cur)
	}

	if err := managerWakeOnce(root, mw); err == nil {
		t.Fatal("queue-ok/cursor-fail must surface")
	}
	if loadManagerWakeErrorClass(root) != "cursor_save_failed" {
		t.Fatalf("durable class=%q", loadManagerWakeErrorClass(root))
	}
	if len(calls) != 1 {
		t.Fatalf("queue calls=%d want 1", len(calls))
	}
	cur, _, err := loadManagerWakeCursor(root, "mgr")
	if err != nil {
		t.Fatal(err)
	}
	if cur.OutboxSeq != 0 {
		t.Fatalf("cursor advanced after failed save: %+v", cur)
	}

	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("restart duplicated manager wake: calls=%d second=%q", len(calls), calls)
	}
	cur, _, _ = loadManagerWakeCursor(root, "mgr")
	if cur.OutboxSeq < 1 {
		t.Fatalf("restart must ack without re-queue: %+v", cur)
	}
	if cur.LastWakeID != wantID {
		t.Fatalf("last_wake_id=%q want %q", cur.LastWakeID, wantID)
	}
}

func TestAppendOutboxRowFailureImmediateAndReconcile(t *testing.T) {
	blockOutboxWrite := func(t *testing.T, root string) {
		t.Helper()
		if err := os.MkdirAll(managerWakeDir(root), 0o755); err != nil {
			t.Fatal(err)
		}
		path := managerWakeOutboxPath(root)
		if err := os.WriteFile(path, nil, 0o444); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o444); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	}
	t.Run("immediate", func(t *testing.T) {
		root := testRoot(t)
		tk := heldCommittedTask(t, root, "wake-proj", "immediate-append-fail")
		blockOutboxWrite(t, root)
		err := appendManagerWakeFromCommitted(root, tk, mustHeldEvent(t, root, tk.ID))
		if err == nil {
			t.Fatal("immediate append I/O must fail closed")
		}
		if err.Error() != "outbox_append_failed" {
			t.Fatalf("allowlisted class=%q", err.Error())
		}
		if loadManagerWakeErrorClass(root) != "outbox_append_failed" {
			t.Fatalf("durable class=%q", loadManagerWakeErrorClass(root))
		}
		raw, err := os.ReadFile(managerWakeErrorPath(root))
		if err != nil {
			t.Fatal(err)
		}
		low := strings.ToLower(string(raw))
		for _, bad := range []string{"permission denied", "is a directory", "no such file", "operation not permitted"} {
			if strings.Contains(low, bad) {
				t.Fatalf("error state leaked prose %q: %s", bad, raw)
			}
		}
	})
	t.Run("reconcile", func(t *testing.T) {
		root := testRoot(t)
		bin, logPath := fakeCodexQueueBin(t, 0)
		mw := testWakeCfg(bin, "wake-proj", "abababab-abab-abab-abab-abababababab", "mgr")
		_ = heldCommittedTask(t, root, "wake-proj", "reconcile-append-fail")
		blockOutboxWrite(t, root)
		if err := managerWakeOnce(root, mw); err == nil {
			t.Fatal("reconcile append I/O must fail closed")
		}
		if _, err := os.Stat(logPath); !os.IsNotExist(err) {
			t.Fatal("failed projection must not queue")
		}
		if loadManagerWakeErrorClass(root) != "outbox_append_failed" {
			t.Fatalf("durable class=%q", loadManagerWakeErrorClass(root))
		}
	})
}

func TestForgedOutboxProjectTypeStatusFailClosed(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*managerWakeOutboxRow)
	}{
		{"project", func(row *managerWakeOutboxRow) { row.Project = "forged-project" }},
		{"type", func(row *managerWakeOutboxRow) { row.EventType = evDone }},
		{"status", func(row *managerWakeOutboxRow) { row.Status = statusDone }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := testRoot(t)
			bin, logPath := fakeCodexQueueBin(t, 0)
			mw := testWakeCfg(bin, "wake-proj", "cdcdecdc-dcdc-dcdc-dcdc-dcdcdcdcdcdc", "mgr")
			tk := heldCommittedTask(t, root, "wake-proj", "forged-"+tc.name)
			if err := appendManagerWakeFromCommitted(root, tk, mustHeldEvent(t, root, tk.ID)); err != nil {
				t.Fatal(err)
			}
			rows, class, err := loadManagerWakeOutbox(root)
			if err != nil || class != "" || len(rows) != 1 {
				t.Fatalf("seed row: class=%q err=%v %+v", class, err, rows)
			}
			forged := rows[0]
			tc.mutate(&forged)
			if forged.WakeEventID != rowWakeIdentity(forged) {
				t.Fatalf("test must keep formula-consistent identity: %+v", forged)
			}
			data, err := json.Marshal(forged)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(managerWakeOutboxPath(root), append(data, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := managerWakeOnce(root, mw); err == nil {
				t.Fatal("forged committed fields must fail closed, not stale-ack")
			}
			if _, err := os.Stat(logPath); !os.IsNotExist(err) {
				payload, _ := os.ReadFile(logPath)
				t.Fatalf("forged %s must not queue: %s", tc.name, payload)
			}
			cur, _, _ := loadManagerWakeCursor(root, "mgr")
			if cur != nil && cur.OutboxSeq != 0 {
				t.Fatalf("mismatch must not ack cursor: %+v", cur)
			}
			rb := managerWakeReadback(root, mw)
			if rb["last_err_class"] != "outbox_identity_mismatch" {
				t.Fatalf("want outbox_identity_mismatch, got %+v", rb)
			}
		})
	}
}

func mustHeldEvent(t *testing.T, root, taskID string) TaskEvent {
	t.Helper()
	events, _, err := loadTaskEvents(root, taskID)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == evHeld {
			return events[i]
		}
	}
	t.Fatalf("no held event for %s: %+v", taskID, events)
	return TaskEvent{}
}

func containsString(in []string, want string) bool {
	for _, s := range in {
		if s == want {
			return true
		}
	}
	return false
}

func fakeCodexQueueSignalBin(t *testing.T) (bin, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "argv.log")
	bin = filepath.Join(dir, "codex")
	script := "#!/bin/sh\n" +
		"log=" + shSingleQuote(logPath) + "\n" +
		"printf '%s\\n' \"$0\" \"$@\" >> \"$log\"\n" +
		"kill -TERM $$\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logPath
}

func queueThreadCount(logPath string) int {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "--thread")
}

func inflightRaw(t *testing.T, root, subID string) []byte {
	t.Helper()
	data, err := os.ReadFile(managerWakeInflightPath(root, subID))
	if err != nil {
		t.Fatalf("inflight missing: %v", err)
	}
	return data
}

func writeInflightRaw(t *testing.T, root, subID string, data []byte) {
	t.Helper()
	path := managerWakeInflightPath(root, subID)
	if path == "" {
		t.Fatal("inflight path rejected")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertUncertainNoRetry(t *testing.T, root string, mw *ManagerWakeConfig, logPath string, before []byte) {
	t.Helper()
	if err := managerWakeOnce(root, mw); err == nil || err.Error() != "delivery_uncertain" {
		t.Fatalf("want delivery_uncertain, got %v", err)
	}
	if loadManagerWakeErrorClass(root) != "delivery_uncertain" {
		t.Fatalf("durable class=%q", loadManagerWakeErrorClass(root))
	}
	got := inflightRaw(t, root, "mgr")
	if before != nil && string(got) != string(before) {
		t.Fatalf("inflight overwritten:\n%s\nvs\n%s", before, got)
	}
	cur, _, _ := loadManagerWakeCursor(root, "mgr")
	if cur != nil && cur.OutboxSeq != 0 {
		t.Fatalf("cursor advanced over unresolved inflight: %+v", cur)
	}
	firstCalls := queueThreadCount(logPath)
	if err := managerWakeOnce(root, mw); err == nil || err.Error() != "delivery_uncertain" {
		t.Fatalf("restart want delivery_uncertain, got %v", err)
	}
	if queueThreadCount(logPath) != firstCalls {
		t.Fatalf("must not auto-requeue: before=%d after=%d", firstCalls, queueThreadCount(logPath))
	}
	metrics := loadManagerWakeMetrics(root)
	if metrics.WatchdogSec != managerWakeWatchdogSec {
		t.Fatalf("watchdog_sec=%d want %d", metrics.WatchdogSec, managerWakeWatchdogSec)
	}
	rb := managerWakeReadback(root, mw)
	if rb["last_err_class"] != "delivery_uncertain" {
		t.Fatalf("readback last_err_class=%v", rb["last_err_class"])
	}
	if rb["watchdog_sec"] != managerWakeWatchdogSec {
		t.Fatalf("readback watchdog: %+v", rb)
	}
}

func TestQueueNonzeroRetainsInflightNoRetry(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 1)
	mw := testWakeCfg(bin, "wake-proj", "13131313-1313-1313-1313-131313131313", "mgr")
	_ = heldCommittedTask(t, root, "wake-proj", "queue-nonzero")
	if err := managerWakeOnce(root, mw); err == nil || err.Error() != "delivery_uncertain" {
		t.Fatalf("nonzero queue is ambiguous success, got %v", err)
	}
	if queueThreadCount(logPath) != 1 {
		t.Fatalf("queue calls=%d want 1", queueThreadCount(logPath))
	}
	before := inflightRaw(t, root, "mgr")
	assertUncertainNoRetry(t, root, mw, logPath, before)
	if queueThreadCount(logPath) != 1 {
		t.Fatalf("nonzero queue retried: %d", queueThreadCount(logPath))
	}
}

func TestQueueSignalRetainsInflightNoRetry(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueSignalBin(t)
	mw := testWakeCfg(bin, "wake-proj", "14141414-1414-1414-1414-141414141414", "mgr")
	_ = heldCommittedTask(t, root, "wake-proj", "queue-signal")
	if err := managerWakeOnce(root, mw); err == nil || err.Error() != "delivery_uncertain" {
		t.Fatalf("signaled queue is ambiguous success, got %v", err)
	}
	if queueThreadCount(logPath) != 1 {
		t.Fatalf("queue calls=%d want 1", queueThreadCount(logPath))
	}
	before := inflightRaw(t, root, "mgr")
	assertUncertainNoRetry(t, root, mw, logPath, before)
	if queueThreadCount(logPath) != 1 {
		t.Fatalf("signal queue retried: %d", queueThreadCount(logPath))
	}
}

func fakeCodexQueueHangBin(t *testing.T) (bin, logPath, startedPath, pidPath, descPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "argv.log")
	startedPath = filepath.Join(dir, "started")
	pidPath = filepath.Join(dir, "pid")
	descPath = filepath.Join(dir, "desc")
	bin = filepath.Join(dir, "codex")
	script := "#!/bin/sh\n" +
		"started=" + shSingleQuote(startedPath) + "\n" +
		"pidfile=" + shSingleQuote(pidPath) + "\n" +
		"descfile=" + shSingleQuote(descPath) + "\n" +
		"log=" + shSingleQuote(logPath) + "\n" +
		"echo $$ > \"$pidfile\"\n" +
		"sleep 3600 &\n" +
		"echo $! > \"$descfile\"\n" +
		": > \"$started\"\n" +
		"printf '%s\\n' \"$0\" \"$@\" >> \"$log\"\n" +
		"trap '' TERM INT\n" +
		"wait\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if data, err := os.ReadFile(pidPath); err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
				_ = killProcGroup(pid)
			}
		}
	})
	return bin, logPath, startedPath, pidPath, descPath
}

func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		t.Fatalf("pid in %s: %q", path, data)
	}
	return pid
}

func waitGone(t *testing.T, pid int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for processAlive(pid) || processGroupAlive(pid) {
		if !time.Now().Before(deadline) {
			t.Fatalf("pid %d still alive after %s", pid, d)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDefaultManagerWakeQueueHungChildDeadlineRetainsInflight(t *testing.T) {
	origTimeout := managerWakeQueueTimeout
	t.Cleanup(func() { managerWakeQueueTimeout = origTimeout })
	managerWakeQueueTimeout = 2 * time.Second

	bin, logPath, startedPath, pidPath, descPath := fakeCodexQueueHangBin(t)
	msg := "cardex-wake v1 n=1 high_water=1 sub=mgr\n" +
		"t1 type=held project=p status=held reason=user_hold transition=tid wake=t1:1:tid"
	errCh := make(chan error, 1)
	go func() {
		errCh <- defaultManagerWakeQueue(bin, "15151515-1515-1515-1515-151515151515", msg)
	}()
	startedDeadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(startedPath); err == nil {
			break
		}
		select {
		case runErr := <-errCh:
			dump := ""
			if data, err := os.ReadFile(logPath); err == nil {
				dump = string(data)
			}
			entries, _ := os.ReadDir(filepath.Dir(startedPath))
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("queue returned before child started: %v log=%q files=%v", runErr, dump, names)
		default:
		}
		if !time.Now().Before(startedDeadline) {
			t.Fatalf("timed out waiting for %s", startedPath)
		}
		time.Sleep(5 * time.Millisecond)
	}
	leader := readPIDFile(t, pidPath)
	desc := readPIDFile(t, descPath)
	startedAt := time.Now()
	var runErr error
	select {
	case runErr = <-errCh:
	case <-time.After(5 * time.Second):
		t.Fatal("defaultManagerWakeQueue hung past the test deadline")
	}
	elapsed := time.Since(startedAt)
	if runErr == nil {
		t.Fatal("hung queue must return an error")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("queue return was unbounded: %s", elapsed)
	}
	waitGone(t, leader, 2*time.Second)
	waitGone(t, desc, 2*time.Second)

	root := testRoot(t)
	mw := testWakeCfg(bin, "wake-proj", "15151515-1515-1515-1515-151515151515", "mgr")
	_ = heldCommittedTask(t, root, "wake-proj", "queue-timeout-prod")
	if err := managerWakeOnce(root, mw); err == nil || err.Error() != "delivery_uncertain" {
		t.Fatalf("timed-out production queue is ambiguous success, got %v", err)
	}
	if queueThreadCount(logPath) != 2 {
		t.Fatalf("queue calls=%d want 2 (direct + once)", queueThreadCount(logPath))
	}
	before := inflightRaw(t, root, "mgr")
	assertUncertainNoRetry(t, root, mw, logPath, before)
	if queueThreadCount(logPath) != 2 {
		t.Fatalf("timeout queue retried: %d", queueThreadCount(logPath))
	}
}

func TestQueueTimeoutRetainsInflightNoRetry(t *testing.T) {
	root := testRoot(t)
	bin, _ := fakeCodexQueueBin(t, 0)
	mw := testWakeCfg(bin, "wake-proj", "15151515-1515-1515-1515-151515151515", "mgr")
	_ = heldCommittedTask(t, root, "wake-proj", "queue-timeout")
	var calls int
	orig := managerWakeQueue
	t.Cleanup(func() { managerWakeQueue = orig })
	managerWakeQueue = func(bin, thread, message string) error {
		calls++
		return fmt.Errorf("queue_timeout")
	}
	if err := managerWakeOnce(root, mw); err == nil || err.Error() != "delivery_uncertain" {
		t.Fatalf("timed-out queue is ambiguous success, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("queue calls=%d want 1", calls)
	}
	before := inflightRaw(t, root, "mgr")
	if err := managerWakeOnce(root, mw); err == nil || err.Error() != "delivery_uncertain" {
		t.Fatalf("restart want delivery_uncertain, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("timeout queue retried: calls=%d", calls)
	}
	if string(inflightRaw(t, root, "mgr")) != string(before) {
		t.Fatal("timeout must retain inflight")
	}
	rb := managerWakeReadback(root, mw)
	if rb["last_err_class"] != "delivery_uncertain" {
		t.Fatalf("readback last_err_class=%v", rb["last_err_class"])
	}
}

func TestPreSpawnInflightCrashDeltaZeroFailClosed(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	thread := "16161616-1616-1616-1616-161616161616"
	mw := testWakeCfg(bin, "wake-proj", thread, "mgr")
	rec := managerWakeInflight{
		Schema:         inflightSchemaV1,
		SubscriptionID: "mgr",
		ThreadID:       thread,
		WakeEventIDs:   []string{"pre-spawn:1:tid"},
		UpdatedAt:      "2026-01-01T00:00:00Z",
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeInflightRaw(t, root, "mgr", append(data, '\n'))
	before := inflightRaw(t, root, "mgr")
	assertUncertainNoRetry(t, root, mw, logPath, before)
	if queueThreadCount(logPath) != 0 {
		t.Fatal("pre-spawn claim must not spawn on delta=0")
	}
	metrics := loadManagerWakeMetrics(root)
	if metrics.QueueAttempts != 0 {
		t.Fatalf("delta=0 with unresolved inflight must not queue: %+v", metrics)
	}
}

func TestPreSpawnInflightCrashStaleScanFailClosed(t *testing.T) {
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	thread := "17171717-1717-1717-1717-171717171717"
	mw := testWakeCfg(bin, "wake-proj", thread, "mgr")
	tk := heldCommittedTask(t, root, "wake-proj", "pre-spawn-stale")
	held := mustHeldEvent(t, root, tk.ID)
	wantID := wakeEventID(tk.ID, held.Seq, held.TransitionID)
	if err := saveManagerWakeInflight(root, "mgr", thread, []managerWakeOutboxRow{{WakeEventID: wantID}}, 1); err != nil {
		t.Fatal(err)
	}
	if err := cmdSetStatus([]string{"-root", root, tk.ID}, "release"); err != nil {
		t.Fatal(err)
	}
	before := inflightRaw(t, root, "mgr")
	assertUncertainNoRetry(t, root, mw, logPath, before)
	if queueThreadCount(logPath) != 0 {
		data, _ := os.ReadFile(logPath)
		t.Fatalf("stale scan must not queue or overwrite inflight: %s", data)
	}
}

func TestInflightRecordsFailClosedNoRetry(t *testing.T) {
	thread := "18181818-1818-1818-1818-181818181818"
	otherThread := "19191919-1919-1919-1919-191919191919"
	cases := []struct {
		name     string
		raw      string
		receipts []string
	}{
		{
			name: "empty",
			raw: `{
  "schema": "cardex.manager_wake.inflight.v1",
  "subscription_id": "mgr",
  "thread_id": "` + thread + `",
  "wake_event_ids": [],
  "updated_at": "2026-01-01T00:00:00Z"
}
`,
		},
		{
			name: "mismatched_subscription_id",
			raw: `{
  "schema": "cardex.manager_wake.inflight.v1",
  "subscription_id": "other",
  "thread_id": "` + thread + `",
  "wake_event_ids": ["forged:1:tid"],
  "updated_at": "2026-01-01T00:00:00Z"
}
`,
		},
		{
			name: "mismatched_thread_id",
			raw: `{
  "schema": "cardex.manager_wake.inflight.v1",
  "subscription_id": "mgr",
  "thread_id": "` + otherThread + `",
  "wake_event_ids": ["forged:1:tid"],
  "updated_at": "2026-01-01T00:00:00Z"
}
`,
		},
		{
			name: "tampered_schema",
			raw: `{
  "schema": "cardex.manager_wake.inflight.v0",
  "subscription_id": "mgr",
  "thread_id": "` + thread + `",
  "wake_event_ids": ["forged:1:tid"],
  "updated_at": "2026-01-01T00:00:00Z"
}
`,
		},
		{
			name: "tampered_unknown_field",
			raw: `{
  "schema": "cardex.manager_wake.inflight.v1",
  "subscription_id": "mgr",
  "thread_id": "` + thread + `",
  "wake_event_ids": ["forged:1:tid"],
  "injected": true,
  "updated_at": "2026-01-01T00:00:00Z"
}
`,
		},
		{
			name: "duplicate_ids",
			raw: `{
  "schema": "cardex.manager_wake.inflight.v1",
  "subscription_id": "mgr",
  "thread_id": "` + thread + `",
  "wake_event_ids": ["dup:1:tid", "dup:1:tid"],
  "updated_at": "2026-01-01T00:00:00Z"
}
`,
		},
		{
			name: "partial_receipts",
			raw: `{
  "schema": "cardex.manager_wake.inflight.v1",
  "subscription_id": "mgr",
  "thread_id": "` + thread + `",
  "wake_event_ids": ["keep:1:tid", "drop:2:tid"],
  "updated_at": "2026-01-01T00:00:00Z"
}
`,
			receipts: []string{"keep:1:tid"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := testRoot(t)
			bin, logPath := fakeCodexQueueBin(t, 0)
			mw := testWakeCfg(bin, "wake-proj", thread, "mgr")
			_ = heldCommittedTask(t, root, "wake-proj", "inflight-"+tc.name)
			writeInflightRaw(t, root, "mgr", []byte(tc.raw))
			if len(tc.receipts) > 0 {
				rec := managerWakeReceipt{
					Schema:         receiptSchemaV1,
					SubscriptionID: "mgr",
					WakeEventIDs:   tc.receipts,
					UpdatedAt:      "2026-01-01T00:00:00Z",
				}
				data, err := json.MarshalIndent(rec, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				path := managerWakeReceiptPath(root, "mgr")
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			before := inflightRaw(t, root, "mgr")
			assertUncertainNoRetry(t, root, mw, logPath, before)
			if queueThreadCount(logPath) != 0 {
				data, _ := os.ReadFile(logPath)
				t.Fatalf("%s spawned queue: %s", tc.name, data)
			}
		})
	}
}

func TestInflightDirectoryDurability(t *testing.T) {
	thread := "1a1a1a1a-1a1a-1a1a-1a1a-1a1a1a1a1a1a"
	t.Run("create_syncs_inflight_and_manager_wake_parent", func(t *testing.T) {
		root := testRoot(t)
		var synced []string
		orig := syncDirAfterRename
		t.Cleanup(func() { syncDirAfterRename = orig })
		syncDirAfterRename = func(dir string) error {
			synced = append(synced, dir)
			return orig(dir)
		}
		if err := saveManagerWakeInflight(root, "mgr", thread, []managerWakeOutboxRow{{WakeEventID: "dur:1:tid"}}, 1); err != nil {
			t.Fatal(err)
		}
		if !containsString(synced, managerWakeInflightDir(root)) {
			t.Fatalf("inflight dir not synced: %v", synced)
		}
		if !containsString(synced, managerWakeDir(root)) {
			t.Fatalf("manager-wake parent not synced: %v", synced)
		}
	})
	t.Run("unlink_syncs_containing_dir", func(t *testing.T) {
		root := testRoot(t)
		if err := saveManagerWakeInflight(root, "mgr", thread, []managerWakeOutboxRow{{WakeEventID: "dur:1:tid"}}, 1); err != nil {
			t.Fatal(err)
		}
		var synced []string
		orig := syncDirAfterRename
		t.Cleanup(func() { syncDirAfterRename = orig })
		syncDirAfterRename = func(dir string) error {
			synced = append(synced, dir)
			return orig(dir)
		}
		if err := clearManagerWakeInflight(root, "mgr"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(managerWakeInflightPath(root, "mgr")); !os.IsNotExist(err) {
			t.Fatal("inflight file must be unlinked")
		}
		if !containsString(synced, managerWakeInflightDir(root)) {
			t.Fatalf("unlink did not sync inflight dir: %v", synced)
		}
	})
	t.Run("parent_sync_failure_blocks_claim_and_queue", func(t *testing.T) {
		root := testRoot(t)
		bin, logPath := fakeCodexQueueBin(t, 0)
		mw := testWakeCfg(bin, "wake-proj", thread, "mgr")
		_ = heldCommittedTask(t, root, "wake-proj", "dir-sync-fail")
		orig := syncDirAfterRename
		t.Cleanup(func() { syncDirAfterRename = orig })
		syncDirAfterRename = func(dir string) error {
			if dir == managerWakeDir(root) {
				return fmt.Errorf("injected_parent_dir_sync_failure")
			}
			return orig(dir)
		}
		if err := managerWakeOnce(root, mw); err == nil {
			t.Fatal("parent dir sync failure must block the inflight claim")
		}
		if _, err := os.Stat(logPath); !os.IsNotExist(err) {
			data, _ := os.ReadFile(logPath)
			t.Fatalf("must not queue after inflight dir durability failure: %s", data)
		}
	})
}
