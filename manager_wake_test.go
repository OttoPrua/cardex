package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
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
	for _, row := range rows {
		if row.EventType == evNeedsOwner {
			t.Fatalf("stale needs_owner must not remain current in outbox: %+v", rows)
		}
	}
	if len(rows) != 1 || rows[0].EventType != evHeld {
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
	diag := rb["diagnosis"].([]string)
	if !containsString(diag, "watchdog_sec_rejected") {
		t.Fatalf("non-canonical watchdog must be diagnosed: %v", diag)
	}
	plist := renderManagerWakePlist("/opt/homebrew/bin/cardex", root, 7, "/tmp/wake.log")
	sec, err := managerWakePlistStartInterval(plist)
	if err != nil || sec != managerWakeWatchdogSec {
		t.Fatalf("plist interval=%d err=%v", sec, err)
	}
}

func TestAdapterRestartAndCorruption(t *testing.T) {
	root := testRoot(t)
	failBin, _ := fakeCodexQueueBin(t, 1)
	okBin, okLog := fakeCodexQueueBin(t, 0)
	thread := "22222222-2222-2222-2222-222222222222"
	mw := testWakeCfg(failBin, "wake-proj", thread, "mgr")
	tk := heldCommittedTask(t, root, "wake-proj", "queue fail")
	if err := managerWakeOnce(root, mw); err == nil {
		t.Fatal("failed queue must surface")
	}
	cur, _, err := loadManagerWakeCursor(root, "mgr")
	if err != nil {
		t.Fatal(err)
	}
	if cur.OutboxSeq != 0 {
		t.Fatalf("cursor advanced on failure: %+v", cur)
	}
	if cur.LastErrorClass != "queue_failed" {
		t.Fatalf("want durable queue_failed, got %+v", cur)
	}
	mw.CodexBin = okBin
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatal(err)
	}
	cur, _, _ = loadManagerWakeCursor(root, "mgr")
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
