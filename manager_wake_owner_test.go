package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// R8 sender-receives acceptance targets M1-M12. Every case is independently runnable
// with `go test -run '^TestOwnerWakeM<N>...$'`.

const (
	ownerWakeRootThread  = "11111111-1111-1111-1111-111111111111"
	ownerWakeAlphaThread = "22222222-2222-2222-2222-222222222222"
	ownerWakeBetaThread  = "33333333-3333-3333-3333-333333333333"
)

func ownerWakeRootSub(id string, projects ...string) ManagerWakeSubscription {
	return ManagerWakeSubscription{
		ID:       id,
		ThreadID: ownerWakeRootThread,
		Role:     managerWakeRoleRoot,
		Projects: projects,
		Enabled:  true,
	}
}

// ownerWakeRootSubEvents is a root endpoint that only listens for some event types,
// which is the operator filter R8-REV-P1-2 turns into a delivery gap.
func ownerWakeRootSubEvents(id string, eventTypes []string, projects ...string) ManagerWakeSubscription {
	sub := ownerWakeRootSub(id, projects...)
	sub.EventTypes = eventTypes
	return sub
}

func writeOwnerWakeConfig(t *testing.T, root, bin string, ownerRouting bool, subs ...ManagerWakeSubscription) *ManagerWakeConfig {
	t.Helper()
	mw := &ManagerWakeConfig{
		Enabled:       true,
		CodexBin:      bin,
		WatchdogSec:   managerWakeWatchdogSec,
		OwnerRouting:  ownerRouting,
		Subscriptions: subs,
	}
	cfg := defaultConfig("claude")
	cfg.ManagerWake = mw
	if err := saveConfig(root, cfg); err != nil {
		t.Fatal(err)
	}
	return mw
}

func codexThreadRoute(requester, thread string, escalate bool) *TaskReplyRoute {
	return &TaskReplyRoute{
		Schema:                       taskReplyRouteSchemaV1,
		RequesterID:                  requester,
		EndpointKind:                 replyEndpointCodexThread,
		EndpointThread:               thread,
		TargetManagementConversation: "Cardex Control Plane",
		CallbackTopLevelSession:      "019fe484-12c5-78b2-986c-6a8bf0d2b079",
		ReceiptRoute:                 "receipts/cardex-control-plane",
		EscalateToRoot:               escalate,
	}
}

func rootRoute(requester string) *TaskReplyRoute {
	return &TaskReplyRoute{
		Schema:       taskReplyRouteSchemaV1,
		RequesterID:  requester,
		EndpointKind: replyEndpointRoot,
	}
}

// routedHeldTask pins the reply route before the first save, which is the only moment
// the card face accepts one, then drives a committed held transition so the outbox has
// exactly one wake row for it.
func routedHeldTask(t *testing.T, root, project, title string, route *TaskReplyRoute) *Task {
	t.Helper()
	tk := newTask(root, testCfg(), typeSequence, title, "/tmp", []string{"p"}, 5)
	tk.Project = project
	tk.ReplyRoute = route
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

func wakeIDOf(t *testing.T, root string, tk *Task) string {
	t.Helper()
	held := mustHeldEvent(t, root, tk.ID)
	return wakeEventID(tk.ID, held.Seq, held.TransitionID)
}

// canceledCommittedTask commits a routeless card on a second wake-eligible event type,
// so a root filter can be shown to still exclude what the operator excluded.
func canceledCommittedTask(t *testing.T, root, project, title string) *Task {
	t.Helper()
	tk := newTask(root, testCfg(), typeSequence, title, "/tmp", []string{"p"}, 5)
	tk.Project = project
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if err := terminalize(root, tk.ID, statusCanceled, "test:cancel", "test cancel", map[string]any{"reason": "test cancel"}); err != nil {
		t.Fatal(err)
	}
	fresh, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	return fresh
}

func outboxRow(t *testing.T, root, taskID string) managerWakeOutboxRow {
	t.Helper()
	rows, class, err := loadManagerWakeOutbox(root)
	if err != nil || class != "" {
		t.Fatalf("outbox load: class=%q err=%v", class, err)
	}
	for _, row := range rows {
		if row.TaskID == taskID {
			return row
		}
	}
	t.Fatalf("no outbox row for %s", taskID)
	return managerWakeOutboxRow{}
}

// rootCursorSeq is the outbox position the configured root endpoint has already
// consumed. A row below it can never be delivered again.
func rootCursorSeq(t *testing.T, root, subID string) int64 {
	t.Helper()
	cur, class, err := loadManagerWakeCursor(root, subID)
	if class != "" || err != nil {
		t.Fatalf("root cursor: class=%q err=%v", class, err)
	}
	if cur == nil {
		return 0
	}
	return cur.OutboxSeq
}

type ownerWakeCall struct {
	thread  string
	message string
}

type ownerWakeCapture struct {
	mu    sync.Mutex
	calls []ownerWakeCall
	fail  map[string]error
}

func captureOwnerWakeQueue(t *testing.T) *ownerWakeCapture {
	t.Helper()
	c := &ownerWakeCapture{fail: map[string]error{}}
	orig := managerWakeQueue
	t.Cleanup(func() { managerWakeQueue = orig })
	managerWakeQueue = func(bin, thread, message string) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.calls = append(c.calls, ownerWakeCall{thread: thread, message: message})
		return c.fail[thread]
	}
	return c
}

func (c *ownerWakeCapture) snapshot() []ownerWakeCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ownerWakeCall(nil), c.calls...)
}

func (c *ownerWakeCapture) threads() []string {
	var out []string
	for _, call := range c.snapshot() {
		out = append(out, call.thread)
	}
	sort.Strings(out)
	return out
}

// wakeIDsOn returns every wake= id delivered to one canonical thread, counting
// duplicates so a coalescing regression cannot hide behind set semantics.
func (c *ownerWakeCapture) wakeIDsOn(thread string) []string {
	want, _ := canonicalManagerWakeThreadID(thread)
	var out []string
	for _, call := range c.snapshot() {
		got, ok := canonicalManagerWakeThreadID(call.thread)
		if !ok || got != want {
			continue
		}
		for _, line := range strings.Split(call.message, "\n") {
			idx := strings.LastIndex(line, " wake=")
			if idx < 0 {
				continue
			}
			out = append(out, strings.TrimSpace(line[idx+len(" wake="):]))
		}
	}
	sort.Strings(out)
	return out
}

func ownerWakeFileNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// ---- M1: two root-endpoint cards coalesce into one root delivery ----

func TestOwnerWakeM1RootToRootCoalesce(t *testing.T) {
	root := testRoot(t)
	bin, _ := fakeCodexQueueBin(t, 0)
	mw := writeOwnerWakeConfig(t, root, bin, true, ownerWakeRootSub("mgr", "wake-proj"))
	q := captureOwnerWakeQueue(t)

	a := routedHeldTask(t, root, "wake-proj", "root endpoint alpha", rootRoute("alpha"))
	b := routedHeldTask(t, root, "wake-proj", "root endpoint beta", rootRoute("beta"))

	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatalf("owner pass: %v", err)
	}
	calls := q.snapshot()
	if len(calls) != 1 {
		t.Fatalf("two root-endpoint cards must coalesce into one root queue, got %d: %+v", len(calls), calls)
	}
	if got, _ := canonicalManagerWakeThreadID(calls[0].thread); got != ownerWakeRootThread {
		t.Fatalf("root coalesce delivered to %q", calls[0].thread)
	}
	want := []string{wakeIDOf(t, root, a), wakeIDOf(t, root, b)}
	sort.Strings(want)
	if got := q.wakeIDsOn(ownerWakeRootThread); !equalStrings(got, want) {
		t.Fatalf("root message ids = %v, want %v", got, want)
	}
	if names := ownerWakeFileNames(t, managerWakeCursorDir(root)); len(names) != 1 || names[0] != "mgr.json" {
		t.Fatalf("root-only delivery must not mint an owner cursor: %v", names)
	}
	// A second pass has nothing new: root must not be woken again.
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if got := len(q.snapshot()); got != 1 {
		t.Fatalf("idempotent pass queued again: %d", got)
	}
}

// ---- M2: a Codex sub-session endpoint replies to itself, not to root ----

func TestOwnerWakeM2CodexSubSessionRepliesToSelf(t *testing.T) {
	root := testRoot(t)
	bin, _ := fakeCodexQueueBin(t, 0)
	mw := writeOwnerWakeConfig(t, root, bin, true, ownerWakeRootSub("mgr", "wake-proj"))
	q := captureOwnerWakeQueue(t)

	tk := routedHeldTask(t, root, "wake-proj", "sub-session self reply",
		codexThreadRoute("alpha", ownerWakeAlphaThread, false))

	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatalf("owner pass: %v", err)
	}
	if got := q.threads(); len(got) != 1 || got[0] != ownerWakeAlphaThread {
		t.Fatalf("sub-session card must reply only to its own thread, got %v", got)
	}
	want := []string{wakeIDOf(t, root, tk)}
	if got := q.wakeIDsOn(ownerWakeAlphaThread); !equalStrings(got, want) {
		t.Fatalf("owner thread ids = %v, want %v", got, want)
	}
	if got := q.wakeIDsOn(ownerWakeRootThread); len(got) != 0 {
		t.Fatalf("routed card leaked into the root config subscription: %v", got)
	}
	if _, err := os.Stat(managerWakeReceiptPath(root, "owner-alpha")); err != nil {
		t.Fatalf("owner receipt missing: %v", err)
	}
	if _, err := os.Stat(managerWakeInflightPath(root, "owner-alpha")); !os.IsNotExist(err) {
		t.Fatalf("owner inflight must be cleared after a receipted delivery: %v", err)
	}
}

// ---- M3: external-agent is a reserved coordinate, never an executable endpoint ----

func TestOwnerWakeM3ExternalAgentFailsClosed(t *testing.T) {
	root := testRoot(t)
	bin, _ := fakeCodexQueueBin(t, 0)
	mw := writeOwnerWakeConfig(t, root, bin, true, ownerWakeRootSub("mgr", "wake-proj"))
	q := captureOwnerWakeQueue(t)

	route := codexThreadRoute("yvonne", ownerWakeAlphaThread, false)
	route.EndpointKind = replyEndpointExternalAgent
	route.EndpointThread = ""
	routedHeldTask(t, root, "wake-proj", "yvonne external agent", route)

	for pass := 0; pass < 2; pass++ {
		if err := managerWakeOnce(root, mw); err != nil {
			t.Fatalf("pass %d must stay green while the endpoint is refused: %v", pass, err)
		}
	}
	if calls := q.snapshot(); len(calls) != 0 {
		t.Fatalf("unsupported endpoint queued a model turn: %+v", calls)
	}
	blocked := loadOwnerWakeBlocks(root)
	if len(blocked) != 1 || blocked[0].Class != ownerErrEndpointUnsupported || blocked[0].RequesterID != "yvonne" {
		t.Fatalf("durable fail-closed coordinate = %+v", blocked)
	}
	rb := managerWakeReadback(root, mw)
	report, _ := rb["owner_blocked"].([]map[string]any)
	if len(report) != 1 || report[0]["last_err_class"] != ownerErrEndpointUnsupported {
		t.Fatalf("readback must surface the refused requester: %+v", rb["owner_blocked"])
	}
	if diag, _ := rb["diagnosis"].([]string); !containsString(diag, ownerErrEndpointUnsupported) {
		t.Fatalf("diagnosis must name the refused endpoint: %v", diag)
	}
}

// ---- M4: Yvonne / cursor-agent / management-card are create-only stubs ----

func TestOwnerWakeM4ReservedEndpointsAreCreateOnly(t *testing.T) {
	for _, kind := range []string{replyEndpointExternalAgent, replyEndpointCursorAgent, replyEndpointManagementCard} {
		t.Run(kind, func(t *testing.T) {
			if replyEndpointExecutable(kind) {
				t.Fatalf("%s must not be executable in R8 v1", kind)
			}
			root := testRoot(t)
			bin, _ := fakeCodexQueueBin(t, 0)
			mw := writeOwnerWakeConfig(t, root, bin, true, ownerWakeRootSub("mgr", "wake-proj"))
			q := captureOwnerWakeQueue(t)

			// Create-only: the coordinate is accepted and persisted verbatim.
			card := &Task{}
			raw := `{"schema":"` + taskReplyRouteSchemaV1 + `","requester_id":"yvonne","endpoint_kind":"` + kind + `",` +
				`"target_management_conversation":"Cardex Control Plane"}`
			if err := applyTaskReplyRoute(card, raw); err != nil {
				t.Fatalf("reserved endpoint must be accepted at enqueue: %v", err)
			}
			if card.ReplyRoute == nil || card.ReplyRoute.EndpointKind != kind {
				t.Fatalf("reserved endpoint not pinned: %+v", card.ReplyRoute)
			}
			// A thread on a role-addressed endpoint would be an unhonored instruction.
			if err := applyTaskReplyRoute(&Task{}, `{"requester_id":"yvonne","endpoint_kind":"`+kind+
				`","endpoint_thread":"`+ownerWakeAlphaThread+`"}`); err == nil {
				t.Fatal("reserved endpoint must reject a thread it would never use")
			}

			tk := routedHeldTask(t, root, "wake-proj", "reserved "+kind, card.ReplyRoute)
			if err := managerWakeOnce(root, mw); err != nil {
				t.Fatalf("pass: %v", err)
			}
			if calls := q.snapshot(); len(calls) != 0 {
				t.Fatalf("%s delivered: %+v", kind, calls)
			}
			if blocked := loadOwnerWakeBlocks(root); len(blocked) != 1 ||
				blocked[0].Class != ownerErrEndpointUnsupported ||
				!containsString(blocked[0].TaskIDs, tk.ID) {
				t.Fatalf("%s must record a fail-closed coordinate: %+v", kind, blocked)
			}
		})
	}
}

// ---- M5: two endpoints that canonicalize to one thread deliver a wake once ----

func TestOwnerWakeM5CanonicalThreadCoalesce(t *testing.T) {
	root := testRoot(t)
	bin, _ := fakeCodexQueueBin(t, 0)
	mw := writeOwnerWakeConfig(t, root, bin, true, ownerWakeRootSub("mgr", "wake-proj"))
	q := captureOwnerWakeQueue(t)

	// The requester's own endpoint is the root thread written in upper case, and it
	// also escalates. Both legs resolve to one canonical destination.
	upper := strings.ToUpper(ownerWakeRootThread)
	tk := routedHeldTask(t, root, "wake-proj", "same thread twice", codexThreadRoute("alpha", upper, true))

	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatalf("owner pass: %v", err)
	}
	want := []string{wakeIDOf(t, root, tk)}
	if got := q.wakeIDsOn(ownerWakeRootThread); !equalStrings(got, want) {
		t.Fatalf("canonical thread received %v, want exactly %v once", got, want)
	}
	for _, call := range q.snapshot() {
		if got, _ := canonicalManagerWakeThreadID(call.thread); got != ownerWakeRootThread {
			t.Fatalf("unexpected destination %q", call.thread)
		}
	}
}

// ---- M6: one unreachable requester never blocks another; shared state still fail-stops ----

func TestOwnerWakeM6OwnerIsolation(t *testing.T) {
	root := testRoot(t)
	bin, _ := fakeCodexQueueBin(t, 0)
	mw := writeOwnerWakeConfig(t, root, bin, true, ownerWakeRootSub("mgr", "wake-proj"))
	q := captureOwnerWakeQueue(t)

	broken := codexThreadRoute("alpha", ownerWakeAlphaThread, false)
	broken.EndpointKind = replyEndpointCursorAgent
	broken.EndpointThread = ""
	routedHeldTask(t, root, "wake-proj", "alpha unreachable", broken)
	healthy := routedHeldTask(t, root, "wake-proj", "beta reachable",
		codexThreadRoute("beta", ownerWakeBetaThread, false))

	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatalf("a refused endpoint must not fail the pass: %v", err)
	}
	if got := q.wakeIDsOn(ownerWakeBetaThread); !equalStrings(got, []string{wakeIDOf(t, root, healthy)}) {
		t.Fatalf("healthy requester was starved by its neighbour: %v", got)
	}
	if got := q.wakeIDsOn(ownerWakeAlphaThread); len(got) != 0 {
		t.Fatalf("refused requester was delivered anyway: %v", got)
	}
	blocked := loadOwnerWakeBlocks(root)
	if len(blocked) != 1 || blocked[0].RequesterID != "alpha" {
		t.Fatalf("isolation record = %+v", blocked)
	}

	// Isolation is scoped to pre-claim endpoint reachability only. Corrupt shared
	// state must still stop the whole pass.
	if err := os.WriteFile(managerWakeOutboxPath(root), []byte("{not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := managerWakeOnce(root, mw); err == nil || err.Error() != "outbox_corrupt" {
		t.Fatalf("shared-state corruption must fail the pass, got %v", err)
	}
}

// ---- M7: escalate_to_root fans one wake out to owner and root ----

func TestOwnerWakeM7EscalateToRootFanout(t *testing.T) {
	root := testRoot(t)
	bin, _ := fakeCodexQueueBin(t, 0)
	mw := writeOwnerWakeConfig(t, root, bin, true, ownerWakeRootSub("mgr", "wake-proj"))
	q := captureOwnerWakeQueue(t)

	tk := routedHeldTask(t, root, "wake-proj", "escalating card",
		codexThreadRoute("alpha", ownerWakeAlphaThread, true))
	want := []string{wakeIDOf(t, root, tk)}

	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatalf("owner pass: %v", err)
	}
	if got := q.wakeIDsOn(ownerWakeAlphaThread); !equalStrings(got, want) {
		t.Fatalf("owner leg = %v, want %v", got, want)
	}
	if got := q.wakeIDsOn(ownerWakeRootThread); !equalStrings(got, want) {
		t.Fatalf("root escalation leg = %v, want %v", got, want)
	}
	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if got := len(q.snapshot()); got != 2 {
		t.Fatalf("escalation repeated on a no-delta scan: %d calls", got)
	}

	// Without a root subscription the escalation has nowhere to land, and the whole
	// requester is held rather than half-delivered.
	bare := testRoot(t)
	bareBin, _ := fakeCodexQueueBin(t, 0)
	bareMW := writeOwnerWakeConfig(t, bare, bareBin, true, ManagerWakeSubscription{
		ID: "plain", ThreadID: ownerWakeBetaThread, Projects: []string{"wake-proj"}, Enabled: true,
	})
	bareQ := captureOwnerWakeQueue(t)
	routedHeldTask(t, bare, "wake-proj", "escalation with no root",
		codexThreadRoute("alpha", ownerWakeAlphaThread, true))
	if err := managerWakeOnce(bare, bareMW); err != nil {
		t.Fatalf("missing root must isolate, not fail the pass: %v", err)
	}
	if calls := bareQ.snapshot(); len(calls) != 0 {
		t.Fatalf("escalation with no root endpoint half-delivered: %+v", calls)
	}
	if blocked := loadOwnerWakeBlocks(bare); len(blocked) != 1 || blocked[0].Class != ownerErrRootUnreachable {
		t.Fatalf("missing root class = %+v", blocked)
	}
}

// ---- R8-REV-P1-1: a switched-off root subscription is not a reachable root ----
//
// Config may keep its role=root subscription while it is disabled, and delivery skips
// disabled subscriptions. Counting one as reachable would make an endpoint_kind=root card
// disappear with no record, and would let an escalation deliver its owner leg while the
// root leg silently evaporated. A disabled root must behave exactly like a missing one.

func TestOwnerWakeM1RootEndpointDisabledRootFailsClosed(t *testing.T) {
	root := testRoot(t)
	bin, _ := fakeCodexQueueBin(t, 0)
	off := ownerWakeRootSub("mgr", "wake-proj")
	off.Enabled = false
	mw := writeOwnerWakeConfig(t, root, bin, true, off)
	q := captureOwnerWakeQueue(t)

	tk := routedHeldTask(t, root, "wake-proj", "root endpoint with root switched off", rootRoute("alpha"))

	for pass := 0; pass < 2; pass++ {
		if err := managerWakeOnce(root, mw); err != nil {
			t.Fatalf("pass %d: a disabled root must isolate, not fail the pass: %v", pass, err)
		}
	}
	if calls := q.snapshot(); len(calls) != 0 {
		t.Fatalf("disabled root still queued a model turn: %+v", calls)
	}
	blocked := loadOwnerWakeBlocks(root)
	if len(blocked) != 1 || blocked[0].Class != ownerErrRootUnreachable ||
		blocked[0].RequesterID != "alpha" || !containsString(blocked[0].TaskIDs, tk.ID) {
		t.Fatalf("endpoint_kind=root into a disabled root must be a durable fail-closed coordinate, got %+v", blocked)
	}
	rb := managerWakeReadback(root, mw)
	if diag, _ := rb["diagnosis"].([]string); !containsString(diag, ownerErrRootUnreachable) {
		t.Fatalf("diagnosis must name the unreachable root: %v", diag)
	}

	// Fail-closed, not lost: re-enabling root delivers the held wake and clears the record.
	on := writeOwnerWakeConfig(t, root, bin, true, ownerWakeRootSub("mgr", "wake-proj"))
	if err := managerWakeOnce(root, on); err != nil {
		t.Fatalf("pass after re-enabling root: %v", err)
	}
	if got := q.wakeIDsOn(ownerWakeRootThread); !equalStrings(got, []string{wakeIDOf(t, root, tk)}) {
		t.Fatalf("re-enabled root received %v, want the held wake once", got)
	}
	if blocked := loadOwnerWakeBlocks(root); len(blocked) != 0 {
		t.Fatalf("a recovered requester must clear its block record: %+v", blocked)
	}
}

func TestOwnerWakeM7EscalationDisabledRootNoHalfDelivery(t *testing.T) {
	root := testRoot(t)
	bin, _ := fakeCodexQueueBin(t, 0)
	off := ownerWakeRootSub("mgr", "wake-proj")
	off.Enabled = false
	mw := writeOwnerWakeConfig(t, root, bin, true, off)
	q := captureOwnerWakeQueue(t)

	esc := routedHeldTask(t, root, "wake-proj", "escalation into a disabled root",
		codexThreadRoute("alpha", ownerWakeAlphaThread, true))
	neighbour := routedHeldTask(t, root, "wake-proj", "beta needs no root",
		codexThreadRoute("beta", ownerWakeBetaThread, false))

	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatalf("a disabled root must isolate, not fail the pass: %v", err)
	}
	if got := q.wakeIDsOn(ownerWakeAlphaThread); len(got) != 0 {
		t.Fatalf("owner leg delivered while the root leg had nowhere to land: %v", got)
	}
	if got := q.wakeIDsOn(ownerWakeRootThread); len(got) != 0 {
		t.Fatalf("a disabled root received a turn: %v", got)
	}
	if got := q.wakeIDsOn(ownerWakeBetaThread); !equalStrings(got, []string{wakeIDOf(t, root, neighbour)}) {
		t.Fatalf("a requester that needs no root was starved by its neighbour: %v", got)
	}
	blocked := loadOwnerWakeBlocks(root)
	if len(blocked) != 1 || blocked[0].SubscriptionID != "owner-alpha" ||
		blocked[0].Class != ownerErrRootUnreachable || !containsString(blocked[0].TaskIDs, esc.ID) {
		t.Fatalf("escalation into a disabled root = %+v", blocked)
	}
	// A requester that never delivered must not own any protocol file.
	for _, path := range []string{
		managerWakeCursorPath(root, "owner-alpha"),
		managerWakeReceiptPath(root, "owner-alpha"),
		managerWakeInflightPath(root, "owner-alpha"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("blocked requester left %s behind: %v", filepath.Base(path), err)
		}
	}

	// Both legs land once root is reachable again; neither leg was consumed by the block.
	on := writeOwnerWakeConfig(t, root, bin, true, ownerWakeRootSub("mgr", "wake-proj"))
	if err := managerWakeOnce(root, on); err != nil {
		t.Fatalf("pass after re-enabling root: %v", err)
	}
	want := []string{wakeIDOf(t, root, esc)}
	if got := q.wakeIDsOn(ownerWakeAlphaThread); !equalStrings(got, want) {
		t.Fatalf("recovered owner leg = %v, want %v", got, want)
	}
	if got := q.wakeIDsOn(ownerWakeRootThread); !equalStrings(got, want) {
		t.Fatalf("recovered root leg = %v, want %v", got, want)
	}
}

// ---- R8-REV-P1-2: a root subscription that filters out the row is not a reachable root ----
//
// role=root may carry an event_types filter. When a card pinned at endpoint_kind=root (or
// escalating to root) produces a row of a type that filter excludes, planning used to
// count root as reachable while subscriptionMatches skipped the root leg at delivery. The
// enabled root then advanced its cursor past the row, so unlike the disabled-root case the
// wake was not held but destroyed. Planning must treat the gap as an unreachable root:
// record the requester fail-closed and hold the root leg's cursor for the pass.

func TestOwnerWakeM1RootEndpointEventTypeGapFailsClosed(t *testing.T) {
	root := testRoot(t)
	bin, _ := fakeCodexQueueBin(t, 0)
	// Root only listens for done; the card below commits a held row.
	mw := writeOwnerWakeConfig(t, root, bin, true, ownerWakeRootSubEvents("mgr", []string{evDone}, "wake-proj"))
	q := captureOwnerWakeQueue(t)

	tk := routedHeldTask(t, root, "wake-proj", "root endpoint outside the root filter", rootRoute("alpha"))

	for pass := 0; pass < 2; pass++ {
		if err := managerWakeOnce(root, mw); err != nil {
			t.Fatalf("pass %d: a filtered-out root leg must isolate, not fail the pass: %v", pass, err)
		}
	}
	if calls := q.snapshot(); len(calls) != 0 {
		t.Fatalf("a root that cannot carry the row still queued a model turn: %+v", calls)
	}
	blocked := loadOwnerWakeBlocks(root)
	if len(blocked) != 1 || blocked[0].Class != ownerErrRootUnreachable ||
		blocked[0].RequesterID != "alpha" || !containsString(blocked[0].TaskIDs, tk.ID) {
		t.Fatalf("endpoint_kind=root outside the root event filter must be a durable fail-closed coordinate, got %+v", blocked)
	}
	rb := managerWakeReadback(root, mw)
	if diag, _ := rb["diagnosis"].([]string); !containsString(diag, ownerErrRootUnreachable) {
		t.Fatalf("diagnosis must name the unreachable root: %v", diag)
	}
	// Held, not destroyed: the root leg must not have consumed the row it refused.
	row := outboxRow(t, root, tk.ID)
	if got := rootCursorSeq(t, root, "mgr"); got >= row.Seq {
		t.Fatalf("root cursor consumed the row it silently skipped: cursor=%d row=%d", got, row.Seq)
	}

	// Widening the root filter to cover the row delivers the held wake and clears the record.
	on := writeOwnerWakeConfig(t, root, bin, true, ownerWakeRootSubEvents("mgr", []string{evDone, evHeld}, "wake-proj"))
	if err := managerWakeOnce(root, on); err != nil {
		t.Fatalf("pass after widening the root filter: %v", err)
	}
	if got := q.wakeIDsOn(ownerWakeRootThread); !equalStrings(got, []string{wakeIDOf(t, root, tk)}) {
		t.Fatalf("repaired root received %v, want the held wake once", got)
	}
	if blocked := loadOwnerWakeBlocks(root); len(blocked) != 0 {
		t.Fatalf("a recovered requester must clear its block record: %+v", blocked)
	}
}

func TestOwnerWakeM7EscalationEventTypeGapNoHalfDelivery(t *testing.T) {
	root := testRoot(t)
	bin, _ := fakeCodexQueueBin(t, 0)
	mw := writeOwnerWakeConfig(t, root, bin, true, ownerWakeRootSubEvents("mgr", []string{evDone}, "wake-proj"))
	q := captureOwnerWakeQueue(t)

	esc := routedHeldTask(t, root, "wake-proj", "escalation outside the root filter",
		codexThreadRoute("alpha", ownerWakeAlphaThread, true))
	neighbour := routedHeldTask(t, root, "wake-proj", "beta needs no root",
		codexThreadRoute("beta", ownerWakeBetaThread, false))

	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatalf("a filtered-out root leg must isolate, not fail the pass: %v", err)
	}
	if got := q.wakeIDsOn(ownerWakeAlphaThread); len(got) != 0 {
		t.Fatalf("owner leg delivered while the root leg was being filtered away: %v", got)
	}
	if got := q.wakeIDsOn(ownerWakeRootThread); len(got) != 0 {
		t.Fatalf("a root that cannot carry the row received a turn: %v", got)
	}
	if got := q.wakeIDsOn(ownerWakeBetaThread); !equalStrings(got, []string{wakeIDOf(t, root, neighbour)}) {
		t.Fatalf("a requester that needs no root was starved by its neighbour: %v", got)
	}
	blocked := loadOwnerWakeBlocks(root)
	if len(blocked) != 1 || blocked[0].SubscriptionID != "owner-alpha" ||
		blocked[0].Class != ownerErrRootUnreachable || !containsString(blocked[0].TaskIDs, esc.ID) {
		t.Fatalf("escalation outside the root event filter = %+v", blocked)
	}
	// A requester that never delivered must not own any protocol file.
	for _, path := range []string{
		managerWakeCursorPath(root, "owner-alpha"),
		managerWakeReceiptPath(root, "owner-alpha"),
		managerWakeInflightPath(root, "owner-alpha"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("blocked requester left %s behind: %v", filepath.Base(path), err)
		}
	}
	escRow := outboxRow(t, root, esc.ID)
	if got := rootCursorSeq(t, root, "mgr"); got >= escRow.Seq {
		t.Fatalf("root cursor consumed the escalated row it could not carry: cursor=%d row=%d", got, escRow.Seq)
	}

	// Both legs land once the root filter covers the row; neither leg was consumed.
	on := writeOwnerWakeConfig(t, root, bin, true, ownerWakeRootSubEvents("mgr", []string{evDone, evHeld}, "wake-proj"))
	if err := managerWakeOnce(root, on); err != nil {
		t.Fatalf("pass after widening the root filter: %v", err)
	}
	want := []string{wakeIDOf(t, root, esc)}
	if got := q.wakeIDsOn(ownerWakeAlphaThread); !equalStrings(got, want) {
		t.Fatalf("recovered owner leg = %v, want %v", got, want)
	}
	if got := q.wakeIDsOn(ownerWakeRootThread); !equalStrings(got, want) {
		t.Fatalf("recovered root leg = %v, want %v", got, want)
	}
}

// The repair must not buy root reachability by widening the operator's filter: a root
// event_types list still excludes every unrouted row the operator excluded.
func TestOwnerWakeRootEventTypeFilterNotWidened(t *testing.T) {
	root := testRoot(t)
	bin, _ := fakeCodexQueueBin(t, 0)
	mw := writeOwnerWakeConfig(t, root, bin, true, ownerWakeRootSubEvents("mgr", []string{evHeld}, "wake-proj"))
	q := captureOwnerWakeQueue(t)

	pinned := routedHeldTask(t, root, "wake-proj", "root endpoint inside the root filter", rootRoute("alpha"))
	legacy := canceledCommittedTask(t, root, "wake-proj", "legacy canceled outside the root filter")

	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatalf("covered root pass: %v", err)
	}
	if got := q.wakeIDsOn(ownerWakeRootThread); !equalStrings(got, []string{wakeIDOf(t, root, pinned)}) {
		t.Fatalf("a root filter that covers the pinned row must deliver it and nothing else: %v", got)
	}
	if blocked := loadOwnerWakeBlocks(root); len(blocked) != 0 {
		t.Fatalf("a covered root must not be reported unreachable: %+v", blocked)
	}

	rows, class, err := loadManagerWakeOutbox(root)
	if err != nil || class != "" {
		t.Fatalf("outbox load: class=%q err=%v", class, err)
	}
	plan, planClass, planErr := planOwnerWakeDeliveries(root, mw, rows)
	if planClass != "" || planErr != nil {
		t.Fatalf("plan: class=%q err=%v", planClass, planErr)
	}
	if len(plan.configSubs) != 1 {
		t.Fatalf("config subscriptions = %+v", plan.configSubs)
	}
	planned := plan.configSubs[0]
	if !planned.Enabled {
		t.Fatalf("a root that covers every root-bound row must stay deliverable: %+v", planned)
	}
	if !equalStrings(planned.EventTypes, []string{evHeld}) {
		t.Fatalf("planner rewrote the configured root event filter: %v", planned.EventTypes)
	}
	legacyTask, err := findTaskAnywhere(root, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if subscriptionMatches(planned, legacyTask, outboxRow(t, root, legacy.ID)) {
		t.Fatalf("planner widened the root filter onto an unrouted %s row", evCanceled)
	}
}

// A card that is already refused never reaches the root leg, so it must not hold it: the
// gap is measured on the cards this pass would actually deliver at root.
func TestOwnerWakeRootFilterGapIgnoresRefusedCards(t *testing.T) {
	root := testRoot(t)
	bin, _ := fakeCodexQueueBin(t, 0)
	mw := writeOwnerWakeConfig(t, root, bin, true, ownerWakeRootSubEvents("mgr", []string{evDone}, "wake-proj"))
	q := captureOwnerWakeQueue(t)

	refused := codexThreadRoute("yvonne", ownerWakeAlphaThread, true)
	refused.EndpointKind = replyEndpointExternalAgent
	refused.EndpointThread = ""
	tk := routedHeldTask(t, root, "wake-proj", "refused endpoint that also escalates", refused)

	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if calls := q.snapshot(); len(calls) != 0 {
		t.Fatalf("a refused endpoint delivered: %+v", calls)
	}
	blocked := loadOwnerWakeBlocks(root)
	if len(blocked) != 1 || blocked[0].Class != ownerErrEndpointUnsupported ||
		!containsString(blocked[0].TaskIDs, tk.ID) {
		t.Fatalf("a refused endpoint must keep its own class, got %+v", blocked)
	}
	rows, class, err := loadManagerWakeOutbox(root)
	if err != nil || class != "" {
		t.Fatalf("outbox load: class=%q err=%v", class, err)
	}
	plan, planClass, planErr := planOwnerWakeDeliveries(root, mw, rows)
	if planClass != "" || planErr != nil {
		t.Fatalf("plan: class=%q err=%v", planClass, planErr)
	}
	if len(plan.configSubs) != 1 || !plan.configSubs[0].Enabled {
		t.Fatalf("a card that never reaches root must not hold the root leg: %+v", plan.configSubs)
	}
}

// ---- R8-REV-R3-P1-3: a padded event_types entry is a delivery gap, not coverage ----
//
// closedManagerWakeSubscription validates each event_types entry after TrimSpace but never
// writes the trimmed form back, and subscriptionMatches compares the stored entry to the
// row's event type by raw string equality. A padded entry like "held " therefore passes
// every config gate yet can never match a row at delivery. Planning used to trim before
// checking coverage, so it counted the padded entry as coverage delivery does not have:
// the root leg stayed enabled, subscriptionMatches skipped the row, and the enabled root
// consumed it — a silent permanent drop for endpoint_kind=root, a half-delivery for
// escalate_to_root. Coverage must be computed with exactly the comparison delivery uses.

func TestOwnerWakeM1RootEndpointPaddedEventTypeFailsClosed(t *testing.T) {
	root := testRoot(t)
	bin, _ := fakeCodexQueueBin(t, 0)
	// Trailing pad: validation trims and accepts the entry, delivery never matches it.
	mw := writeOwnerWakeConfig(t, root, bin, true, ownerWakeRootSubEvents("mgr", []string{evHeld + " "}, "wake-proj"))
	q := captureOwnerWakeQueue(t)

	tk := routedHeldTask(t, root, "wake-proj", "root endpoint behind a padded filter", rootRoute("alpha"))

	for pass := 0; pass < 2; pass++ {
		if err := managerWakeOnce(root, mw); err != nil {
			t.Fatalf("pass %d: a padded root filter must isolate, not fail the pass: %v", pass, err)
		}
	}
	if calls := q.snapshot(); len(calls) != 0 {
		t.Fatalf("a root whose raw filter cannot match the row still queued a model turn: %+v", calls)
	}
	blocked := loadOwnerWakeBlocks(root)
	if len(blocked) != 1 || blocked[0].Class != ownerErrRootUnreachable ||
		blocked[0].RequesterID != "alpha" || !containsString(blocked[0].TaskIDs, tk.ID) {
		t.Fatalf("endpoint_kind=root behind a padded root filter must be a durable fail-closed coordinate, got %+v", blocked)
	}
	rb := managerWakeReadback(root, mw)
	if diag, _ := rb["diagnosis"].([]string); !containsString(diag, ownerErrRootUnreachable) {
		t.Fatalf("diagnosis must name the unreachable root: %v", diag)
	}
	// Held, not destroyed: the root leg must not have consumed the row its raw filter
	// would have skipped at delivery.
	row := outboxRow(t, root, tk.ID)
	if got := rootCursorSeq(t, root, "mgr"); got >= row.Seq {
		t.Fatalf("root cursor consumed the row its padded filter silently skipped: cursor=%d row=%d", got, row.Seq)
	}

	// The planner holds the pass; it never rewrites the operator's configured bytes.
	rows, class, err := loadManagerWakeOutbox(root)
	if err != nil || class != "" {
		t.Fatalf("outbox load: class=%q err=%v", class, err)
	}
	plan, planClass, planErr := planOwnerWakeDeliveries(root, mw, rows)
	if planClass != "" || planErr != nil {
		t.Fatalf("plan: class=%q err=%v", planClass, planErr)
	}
	if len(plan.configSubs) != 1 || plan.configSubs[0].Enabled {
		t.Fatalf("a root leg behind a padded filter must be held for the pass: %+v", plan.configSubs)
	}
	if !equalStrings(plan.configSubs[0].EventTypes, []string{evHeld + " "}) {
		t.Fatalf("planner rewrote the configured root event filter: %v", plan.configSubs[0].EventTypes)
	}

	// Fail-closed, not lost: repairing the entry to its canonical spelling delivers the
	// held wake and clears the record.
	on := writeOwnerWakeConfig(t, root, bin, true, ownerWakeRootSubEvents("mgr", []string{evHeld}, "wake-proj"))
	if err := managerWakeOnce(root, on); err != nil {
		t.Fatalf("pass after repairing the root filter: %v", err)
	}
	if got := q.wakeIDsOn(ownerWakeRootThread); !equalStrings(got, []string{wakeIDOf(t, root, tk)}) {
		t.Fatalf("repaired root received %v, want the held wake once", got)
	}
	if blocked := loadOwnerWakeBlocks(root); len(blocked) != 0 {
		t.Fatalf("a recovered requester must clear its block record: %+v", blocked)
	}
}

func TestOwnerWakeM7EscalationPaddedEventTypeNoHalfDelivery(t *testing.T) {
	root := testRoot(t)
	bin, _ := fakeCodexQueueBin(t, 0)
	// Leading pad: the same bypass from the other side of the entry.
	mw := writeOwnerWakeConfig(t, root, bin, true, ownerWakeRootSubEvents("mgr", []string{" " + evHeld}, "wake-proj"))
	q := captureOwnerWakeQueue(t)

	esc := routedHeldTask(t, root, "wake-proj", "escalation behind a padded filter",
		codexThreadRoute("alpha", ownerWakeAlphaThread, true))
	neighbour := routedHeldTask(t, root, "wake-proj", "beta needs no root",
		codexThreadRoute("beta", ownerWakeBetaThread, false))

	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatalf("a padded root filter must isolate, not fail the pass: %v", err)
	}
	if got := q.wakeIDsOn(ownerWakeAlphaThread); len(got) != 0 {
		t.Fatalf("owner leg delivered while the padded filter starved the root leg: %v", got)
	}
	if got := q.wakeIDsOn(ownerWakeRootThread); len(got) != 0 {
		t.Fatalf("a root whose raw filter cannot match the row received a turn: %v", got)
	}
	if got := q.wakeIDsOn(ownerWakeBetaThread); !equalStrings(got, []string{wakeIDOf(t, root, neighbour)}) {
		t.Fatalf("a requester that needs no root was starved by its neighbour: %v", got)
	}
	blocked := loadOwnerWakeBlocks(root)
	if len(blocked) != 1 || blocked[0].SubscriptionID != "owner-alpha" ||
		blocked[0].Class != ownerErrRootUnreachable || !containsString(blocked[0].TaskIDs, esc.ID) {
		t.Fatalf("escalation behind a padded root filter = %+v", blocked)
	}
	// A requester that never delivered must not own any protocol file.
	for _, path := range []string{
		managerWakeCursorPath(root, "owner-alpha"),
		managerWakeReceiptPath(root, "owner-alpha"),
		managerWakeInflightPath(root, "owner-alpha"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("blocked requester left %s behind: %v", filepath.Base(path), err)
		}
	}
	escRow := outboxRow(t, root, esc.ID)
	if got := rootCursorSeq(t, root, "mgr"); got >= escRow.Seq {
		t.Fatalf("root cursor consumed the escalated row its padded filter skipped: cursor=%d row=%d", got, escRow.Seq)
	}

	// Both legs land exactly once when the entry is repaired; neither was consumed.
	on := writeOwnerWakeConfig(t, root, bin, true, ownerWakeRootSubEvents("mgr", []string{evHeld}, "wake-proj"))
	if err := managerWakeOnce(root, on); err != nil {
		t.Fatalf("pass after repairing the root filter: %v", err)
	}
	want := []string{wakeIDOf(t, root, esc)}
	if got := q.wakeIDsOn(ownerWakeAlphaThread); !equalStrings(got, want) {
		t.Fatalf("recovered owner leg = %v, want %v", got, want)
	}
	if got := q.wakeIDsOn(ownerWakeRootThread); !equalStrings(got, want) {
		t.Fatalf("recovered root leg = %v, want %v", got, want)
	}
}

// rootEventTypesCover must use delivery's raw comparison verbatim: an empty filter covers
// everything, a canonical entry covers exactly its own row type, and a padded entry —
// which subscriptionMatches can never match — covers nothing.
func TestOwnerWakeRootEventTypesCoverRawComparison(t *testing.T) {
	filter := func(types ...string) ManagerWakeSubscription {
		sub := ownerWakeRootSub("mgr", "wake-proj")
		sub.EventTypes = types
		return sub
	}
	held := map[string]bool{evHeld: true}
	if !rootEventTypesCover(filter(), held) {
		t.Fatal("an empty filter must cover every row")
	}
	if !rootEventTypesCover(filter(evHeld, evDone), map[string]bool{}) {
		t.Fatal("no root-bound rows means nothing to cover")
	}
	if !rootEventTypesCover(filter(evHeld), held) {
		t.Fatal("a canonical entry must cover its own row type")
	}
	if rootEventTypesCover(filter(evHeld+" "), held) {
		t.Fatal("a trailing-padded entry never matches at delivery and must not count as coverage")
	}
	if rootEventTypesCover(filter(" "+evHeld), held) {
		t.Fatal("a leading-padded entry never matches at delivery and must not count as coverage")
	}
	if rootEventTypesCover(filter(evDone, evHeld+" "), held) {
		t.Fatal("a canonical neighbour must not lend coverage to a padded entry")
	}
	if !rootEventTypesCover(filter(evDone, evHeld), held) {
		t.Fatal("canonical entries must keep covering after the repair")
	}
}

// ---- M8: routed and legacy cards coexist in one pass ----

func TestOwnerWakeM8LegacyMix(t *testing.T) {
	root := testRoot(t)
	bin, _ := fakeCodexQueueBin(t, 0)
	mw := writeOwnerWakeConfig(t, root, bin, true, ManagerWakeSubscription{
		ID: "plain", ThreadID: ownerWakeRootThread, Projects: []string{"wake-proj"}, Enabled: true,
	})
	q := captureOwnerWakeQueue(t)

	legacy := heldCommittedTask(t, root, "wake-proj", "legacy no route")
	routed := routedHeldTask(t, root, "wake-proj", "routed card",
		codexThreadRoute("alpha", ownerWakeAlphaThread, false))

	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatalf("mixed pass: %v", err)
	}
	if got := q.wakeIDsOn(ownerWakeRootThread); !equalStrings(got, []string{wakeIDOf(t, root, legacy)}) {
		t.Fatalf("legacy card must keep config matching, got %v", got)
	}
	if got := q.wakeIDsOn(ownerWakeAlphaThread); !equalStrings(got, []string{wakeIDOf(t, root, routed)}) {
		t.Fatalf("routed card must reach its requester, got %v", got)
	}
	if _, err := os.Stat(managerWakeCursorPath(root, "plain")); err != nil {
		t.Fatalf("legacy cursor missing: %v", err)
	}
	if _, err := os.Stat(managerWakeCursorPath(root, "owner-alpha")); err != nil {
		t.Fatalf("owner cursor missing: %v", err)
	}
}

// ---- M9: the durable crash protocol is owner-scoped ----

func TestOwnerWakeM9CrashSeamsAreOwnerScoped(t *testing.T) {
	const env = "CARDEX_OWNER_WAKE_CRASH"
	if os.Getenv(env) == "1" {
		child := os.Getenv("CARDEX_OWNER_WAKE_ROOT")
		bin := os.Getenv("CARDEX_OWNER_WAKE_BIN")
		managerWakeQueue = defaultManagerWakeQueue
		managerWakeCrashAt = "after_starting"
		_ = managerWakeOnce(child, writeOwnerWakeConfigForChild(bin))
		os.Exit(12)
		return
	}
	root := testRoot(t)
	bin, logPath := fakeCodexQueueBin(t, 0)
	mw := writeOwnerWakeConfig(t, root, bin, true, ownerWakeRootSub("mgr", "wake-proj"))
	routedHeldTask(t, root, "wake-proj", "owner crash seam",
		codexThreadRoute("alpha", ownerWakeAlphaThread, false))

	cmd := exec.Command(os.Args[0], "-test.run=^TestOwnerWakeM9CrashSeamsAreOwnerScoped$")
	cmd.Env = append(os.Environ(), env+"=1", "CARDEX_OWNER_WAKE_ROOT="+root, "CARDEX_OWNER_WAKE_BIN="+bin)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 99 {
		t.Fatalf("crash child want exit 99, got %v out=%s", err, out)
	}
	rec, class, loadErr := loadManagerWakeInflight(root, "owner-alpha")
	if loadErr != nil || class != "" || rec == nil || rec.Phase != inflightPhaseStarting {
		t.Fatalf("crash must leave a starting claim under the owner identity: rec=%+v class=%q err=%v", rec, class, loadErr)
	}
	if rec.ThreadID != ownerWakeAlphaThread {
		t.Fatalf("owner claim bound to %q", rec.ThreadID)
	}
	if queueThreadCount(logPath) != 0 {
		t.Fatalf("pre-Start crash delivered anyway: %d", queueThreadCount(logPath))
	}
	if err := managerWakeOnce(root, mw); err == nil || err.Error() != "delivery_uncertain" {
		t.Fatalf("restart after an owner starting-phase crash want delivery_uncertain, got %v", err)
	}
	if queueThreadCount(logPath) != 0 {
		t.Fatalf("owner crash was retried into a second model turn: %d", queueThreadCount(logPath))
	}
}

func writeOwnerWakeConfigForChild(bin string) *ManagerWakeConfig {
	return &ManagerWakeConfig{
		Enabled:       true,
		CodexBin:      bin,
		WatchdogSec:   managerWakeWatchdogSec,
		OwnerRouting:  true,
		Subscriptions: []ManagerWakeSubscription{ownerWakeRootSub("mgr", "wake-proj")},
	}
}

// ---- M10: owner_routing=false is a byte-level rollback ----

func TestOwnerWakeM10OwnerRoutingOffRollback(t *testing.T) {
	root := testRoot(t)
	bin, _ := fakeCodexQueueBin(t, 0)
	mw := writeOwnerWakeConfig(t, root, bin, false, ownerWakeRootSub("mgr", "wake-proj"))
	q := captureOwnerWakeQueue(t)

	routed := routedHeldTask(t, root, "wake-proj", "routed but rolled back",
		codexThreadRoute("alpha", ownerWakeAlphaThread, true))
	legacy := heldCommittedTask(t, root, "wake-proj", "legacy neighbour")

	if err := managerWakeOnce(root, mw); err != nil {
		t.Fatalf("rollback pass: %v", err)
	}
	want := []string{wakeIDOf(t, root, legacy), wakeIDOf(t, root, routed)}
	sort.Strings(want)
	if got := q.wakeIDsOn(ownerWakeRootThread); !equalStrings(got, want) {
		t.Fatalf("with owner routing off both cards must go to the config subscription: got %v want %v", got, want)
	}
	if got := q.wakeIDsOn(ownerWakeAlphaThread); len(got) != 0 {
		t.Fatalf("owner routing off still used the card-face endpoint: %v", got)
	}
	for _, dir := range []string{managerWakeCursorDir(root), managerWakeReceiptDir(root), managerWakeInflightDir(root)} {
		for _, name := range ownerWakeFileNames(t, dir) {
			if strings.HasPrefix(name, ownerWakeSubPrefix) {
				t.Fatalf("owner routing off created %s in %s", name, dir)
			}
		}
	}
	if _, err := os.Stat(managerWakeOwnerBlockPath(root)); !os.IsNotExist(err) {
		t.Fatalf("owner routing off wrote %s", filepath.Base(managerWakeOwnerBlockPath(root)))
	}
	rb := managerWakeReadback(root, mw)
	if rb["owner_routing"] != false {
		t.Fatalf("readback owner_routing = %v", rb["owner_routing"])
	}
	if blocked, _ := rb["owner_blocked"].([]map[string]any); len(blocked) != 0 {
		t.Fatalf("rollback readback reported owner state: %+v", blocked)
	}
}

// ---- M11: reserved owner- prefix and a second root subscription are refused ----

func TestOwnerWakeM11ReservedPrefixAndDuplicateRoot(t *testing.T) {
	t.Run("reserved prefix", func(t *testing.T) {
		root := testRoot(t)
		bin, _ := fakeCodexQueueBin(t, 0)
		mw := writeOwnerWakeConfig(t, root, bin, true, ManagerWakeSubscription{
			ID: "owner-alpha", ThreadID: ownerWakeAlphaThread, Projects: []string{"wake-proj"}, Enabled: true,
		})
		q := captureOwnerWakeQueue(t)
		heldCommittedTask(t, root, "wake-proj", "reserved prefix card")

		if err := managerWakeOnce(root, mw); err == nil || err.Error() != "reserved_subscription_prefix" {
			t.Fatalf("configured owner- id must fail closed, got %v", err)
		}
		if calls := q.snapshot(); len(calls) != 0 {
			t.Fatalf("reserved prefix still delivered: %+v", calls)
		}
		if issues := managerWakeConfigBlocking(root, mw); !containsString(issues, "reserved_subscription_prefix") {
			t.Fatalf("install gate missed the reserved prefix: %v", issues)
		}
		if _, class := closedManagerWakeSubscription(mw.Subscriptions[0]); class != "reserved_subscription_prefix" {
			t.Fatalf("closed subscription class = %q", class)
		}
		derived := mw.Subscriptions[0]
		derived.derived = true
		if _, class := closedManagerWakeSubscription(derived); class != "" {
			t.Fatalf("a delivery-derived owner subscription must keep the prefix, class=%q", class)
		}
	})

	t.Run("duplicate root", func(t *testing.T) {
		root := testRoot(t)
		bin, _ := fakeCodexQueueBin(t, 0)
		mw := writeOwnerWakeConfig(t, root, bin, true,
			ownerWakeRootSub("mgr", "wake-proj"),
			ownerWakeRootSub("mgr2", "wake-proj"),
		)
		q := captureOwnerWakeQueue(t)
		heldCommittedTask(t, root, "wake-proj", "duplicate root card")

		if err := managerWakeOnce(root, mw); err == nil || err.Error() != "duplicate_root_subscription" {
			t.Fatalf("two root subscriptions must fail closed, got %v", err)
		}
		if calls := q.snapshot(); len(calls) != 0 {
			t.Fatalf("ambiguous root still delivered: %+v", calls)
		}
		if issues := managerWakeConfigBlocking(root, mw); !containsString(issues, "duplicate_root_subscription") {
			t.Fatalf("install gate missed the duplicate root: %v", issues)
		}
	})

	t.Run("unknown role", func(t *testing.T) {
		sub := ownerWakeRootSub("mgr", "wake-proj")
		sub.Role = "supervisor"
		if _, class := closedManagerWakeSubscription(sub); class != "invalid_subscription_role" {
			t.Fatalf("unknown role class = %q", class)
		}
	})
}

// ---- M12: the reply route is pinned at enqueue and immutable afterwards ----

func TestOwnerWakeM12RouteImmutability(t *testing.T) {
	root := testRoot(t)
	writeOwnerWakeConfig(t, root, "codex", true, ownerWakeRootSub("mgr", "wake-proj"))

	pinned := newTask(root, testCfg(), typeSequence, "pinned", "/tmp", []string{"p"}, 5)
	pinned.ReplyRoute = codexThreadRoute("alpha", ownerWakeAlphaThread, false)
	if err := saveTask(root, pinned); err != nil {
		t.Fatal(err)
	}

	moved, err := loadTask(root, pinned.ID)
	if err != nil {
		t.Fatal(err)
	}
	moved.ReplyRoute.EndpointThread = ownerWakeBetaThread
	if err := saveTask(root, moved); !errors.Is(err, errReplyRouteImmutable) {
		t.Fatalf("redirecting a pinned route must be refused, got %v", err)
	}

	dropped, err := loadTask(root, pinned.ID)
	if err != nil {
		t.Fatal(err)
	}
	dropped.ReplyRoute = nil
	if err := saveTask(root, dropped); !errors.Is(err, errReplyRouteImmutable) {
		t.Fatalf("dropping a pinned route must be refused, got %v", err)
	}

	// "No route" is equally pinned: a card cannot acquire an owner after enqueue.
	routeless := newTask(root, testCfg(), typeSequence, "routeless", "/tmp", []string{"p"}, 5)
	if err := saveTask(root, routeless); err != nil {
		t.Fatal(err)
	}
	late, err := loadTask(root, routeless.ID)
	if err != nil {
		t.Fatal(err)
	}
	late.ReplyRoute = rootRoute("alpha")
	if err := saveTask(root, late); !errors.Is(err, errReplyRouteImmutable) {
		t.Fatalf("attaching a route after enqueue must be refused, got %v", err)
	}

	// The on-disk bytes never moved.
	after, err := loadTask(root, pinned.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ReplyRoute == nil || after.ReplyRoute.EndpointThread != ownerWakeAlphaThread {
		t.Fatalf("persisted route drifted: %+v", after.ReplyRoute)
	}

	// Inheritance is a deep copy: a derived card can never rewrite its parent's route.
	child := inheritTaskReplyRoute(after.ReplyRoute)
	child.RequesterID = "beta"
	if after.ReplyRoute.RequesterID != "alpha" {
		t.Fatalf("inherited route aliased the parent: %+v", after.ReplyRoute)
	}

	// Malformed coordinates are refused at enqueue, before anything is pinned.
	for name, raw := range map[string]string{
		"bad schema":    `{"schema":"cardex.task.reply_route.v0","requester_id":"a","endpoint_kind":"root"}`,
		"bad requester": `{"requester_id":"../escape","endpoint_kind":"root"}`,
		"owner prefix":  `{"requester_id":"owner-alpha","endpoint_kind":"root"}`,
		"bad kind":      `{"requester_id":"alpha","endpoint_kind":"telepathy"}`,
		"bad thread":    `{"requester_id":"alpha","endpoint_kind":"codex-thread","endpoint_thread":"not-a-uuid"}`,
		"unknown field": `{"requester_id":"alpha","endpoint_kind":"root","surprise":1}`,
	} {
		if err := applyTaskReplyRoute(&Task{}, raw); err == nil {
			t.Errorf("%s must be refused at enqueue", name)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
