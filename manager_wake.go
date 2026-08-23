package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	managerWakeWatchdogSec     = 1200
	managerWakeQueueTimeoutSec = 90
	outboxSchemaV1             = "cardex.manager_wake.outbox.v1"
	cursorSchemaV1             = "cardex.manager_wake.cursor.v1"
	errorSchemaV1              = "cardex.manager_wake.error.v1"
	receiptSchemaV1            = "cardex.manager_wake.receipt.v1"
	inflightSchemaV1           = "cardex.manager_wake.inflight.v1"
	inflightPhaseClaimed       = "claimed"
	inflightPhaseStarting      = "starting"
	inflightPhaseSpawned       = "spawned"
	managerWakeOnceLockID      = "_manager-wake-once"
	managerWakeOutboxLockID    = "_manager-wake-outbox"
)

// ManagerWakeSubscription is a closed, scoped delivery target. IDs must be
// traversal-safe; unscoped subscriptions match nothing.
type ManagerWakeSubscription struct {
	ID          string   `json:"id"`
	ThreadID    string   `json:"thread_id"`
	Projects    []string `json:"projects,omitempty"`
	TaskIDs     []string `json:"task_ids,omitempty"`
	DirPrefixes []string `json:"dir_prefixes,omitempty"`
	EventTypes  []string `json:"event_types,omitempty"`
	Enabled     bool     `json:"enabled"`
}

// ManagerWakeConfig is the self-contained wake core config. It is stored on
// Config.manager_wake as an omitempty field and stays disabled by default.
// WatchdogSec is diagnostic only; the running policy is hard-coded to
// managerWakeWatchdogSec.
type ManagerWakeConfig struct {
	Enabled       bool                      `json:"enabled"`
	CodexBin      string                    `json:"codex_bin,omitempty"`
	WatchdogSec   int                       `json:"watchdog_sec,omitempty"`
	Subscriptions []ManagerWakeSubscription `json:"subscriptions,omitempty"`
}

var (
	managerWakeThreadRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	managerWakeSubIDRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	managerWakeReasonRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	managerWakeQueue    = defaultManagerWakeQueue
	// managerWakeQueueTimeout is the production enqueue deadline. Tests may
	// shrink it; it must stay well below managerWakeWatchdogSec.
	managerWakeQueueTimeout = time.Duration(managerWakeQueueTimeoutSec) * time.Second
	// managerWakeCommitCursor is the post-queue ack. Tests inject a one-shot
	// save failure to prove receipts suppress a second queue spawn.
	managerWakeCommitCursor = saveManagerWakeCursor
	// managerWakePersistReceipts is the sender receipt fsync. Tests inject a
	// crash after queue success and before this write.
	managerWakePersistReceipts = rememberManagerWakeReceipts
	// managerWakeOnceEntered fires at the start of managerWakeOnce so tests can
	// overlap a second once while the first still holds the exclusive section.
	managerWakeOnceEntered func()
	// managerWakeQueueSpawned is invoked after the queue child has started and
	// been registered, before Wait. Tests may replace managerWakeQueue entirely.
	managerWakeQueueSpawned func() error
	// managerWakeBeforeQueue fires after scan and before the inflight claim so
	// tests can inject a committed supersession between bind and queue.
	managerWakeBeforeQueue func()
	// managerWakeBeforeStart fires after the queue command is prepared and
	// immediately before cmd.Start. Tests inject a definite Start failure or a
	// competing transition in the post-rebind/pre-Start window.
	managerWakeBeforeStart func()
	// managerWakeCrashAt is a test-only durable-protocol crash seam. Production
	// never sets it. Values: after_claimed, after_starting, after_start,
	// after_spawned, after_start_revert.
	managerWakeCrashAt string
)

// wakeQueueStartFailed is the only definite pre-spawn cmd.Start failure.
// Post-Start hooks and arbitrary errors with the same text must not produce it.
type wakeQueueStartFailed struct{}

func (wakeQueueStartFailed) Error() string { return "queue_start_failed" }

func isDefiniteWakeQueueStartFailure(err error) bool {
	_, ok := err.(wakeQueueStartFailed)
	return ok
}

func crashWakeIf(point string) {
	if managerWakeCrashAt != "" && managerWakeCrashAt == point {
		os.Exit(99)
	}
}

func usingDefaultManagerWakeQueue() bool {
	return reflect.ValueOf(managerWakeQueue).Pointer() == reflect.ValueOf(defaultManagerWakeQueue).Pointer()
}

// errManagerWakeQueueStart is the typed definite pre-spawn Start failure.
var errManagerWakeQueueStart error = wakeQueueStartFailed{}

type managerWakeOutboxRow struct {
	Schema       string `json:"schema"`
	Seq          int64  `json:"seq"`
	WakeEventID  string `json:"wake_event_id"`
	TaskID       string `json:"task_id"`
	TaskEventSeq int64  `json:"task_event_seq"`
	TransitionID string `json:"transition_id,omitempty"`
	Project      string `json:"project,omitempty"`
	EventType    string `json:"event_type"`
	Status       string `json:"status"`
	NeedsOwner   bool   `json:"needs_owner,omitempty"`
	ReasonClass  string `json:"reason_class"`
	TS           string `json:"ts"`
}

type managerWakeCursor struct {
	Schema         string `json:"schema"`
	SubscriptionID string `json:"subscription_id"`
	OutboxSeq      int64  `json:"outbox_seq"`
	LastWakeID     string `json:"last_wake_id,omitempty"`
	LastErrorClass string `json:"last_err_class,omitempty"`
	LastDeliveryAt string `json:"last_delivery_at,omitempty"`
	UpdatedAt      string `json:"updated_at"`
}

type managerWakeMetrics struct {
	OutboxHighWater  int64  `json:"outbox_high_water"`
	Pending          int    `json:"pending"`
	NoDeltaScans     int    `json:"no_delta_scans"`
	StaleSuppressed  int    `json:"stale_suppressed"`
	QueueAttempts    int    `json:"queue_attempts"`
	QueueSuccesses   int    `json:"queue_successes"`
	QueueFailures    int    `json:"queue_failures"`
	LastErrorClass   string `json:"last_err_class,omitempty"`
	LastWatchdogAt   string `json:"last_watchdog_at,omitempty"`
	WatchdogSec      int    `json:"watchdog_sec"`
	Enabled          bool   `json:"enabled"`
	LaunchdInstalled bool   `json:"launchd_installed"`
}

type managerWakeErrorState struct {
	Schema string `json:"schema"`
	Class  string `json:"class"`
	At     string `json:"at"`
}

type managerWakeReceipt struct {
	Schema         string   `json:"schema"`
	SubscriptionID string   `json:"subscription_id"`
	WakeEventIDs   []string `json:"wake_event_ids"`
	UpdatedAt      string   `json:"updated_at"`
}

type managerWakeInflight struct {
	Schema          string   `json:"schema"`
	SubscriptionID  string   `json:"subscription_id"`
	ThreadID        string   `json:"thread_id,omitempty"`
	Phase           string   `json:"phase"`
	WakeEventIDs    []string `json:"wake_event_ids"`
	OutboxHighWater int64    `json:"outbox_high_water,omitempty"`
	UpdatedAt       string   `json:"updated_at"`
}

type wakeBindKind int

const (
	wakeBindOK wakeBindKind = iota
	wakeBindStale
	wakeBindMismatch
)

func managerWakeDir(root string) string {
	return filepath.Join(controlDir(root), "manager-wake")
}

func managerWakeOutboxPath(root string) string {
	return filepath.Join(managerWakeDir(root), "outbox.jsonl")
}

func managerWakeCursorDir(root string) string {
	return filepath.Join(managerWakeDir(root), "cursors")
}

func managerWakeMetricsPath(root string) string {
	return filepath.Join(managerWakeDir(root), "metrics.json")
}

func managerWakeErrorPath(root string) string {
	return filepath.Join(managerWakeDir(root), "error.json")
}

func managerWakeReceiptDir(root string) string {
	return filepath.Join(managerWakeDir(root), "receipts")
}

func managerWakeReceiptPath(root, subID string) string {
	return closedManagerWakeSubFile(managerWakeReceiptDir(root), subID)
}

func managerWakeInflightDir(root string) string {
	return filepath.Join(managerWakeDir(root), "inflight")
}

func managerWakeInflightPath(root, subID string) string {
	return closedManagerWakeSubFile(managerWakeInflightDir(root), subID)
}

func closedManagerWakeSubFile(dir, subID string) string {
	id, ok := closedManagerWakeSubID(subID)
	if !ok {
		return ""
	}
	path := filepath.Join(dir, id+".json")
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == "" || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return ""
	}
	if filepath.Base(path) != id+".json" {
		return ""
	}
	return path
}

func closedManagerWakeSubID(id string) (string, bool) {
	id = strings.TrimSpace(id)
	if id == "" || id == "." || id == ".." {
		return "", false
	}
	if strings.ContainsRune(id, 0) || strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return "", false
	}
	if !managerWakeSubIDRE.MatchString(id) {
		return "", false
	}
	if filepath.Base(id) != id || filepath.Clean(id) != id {
		return "", false
	}
	return id, true
}

func managerWakeCursorPath(root, subID string) string {
	return closedManagerWakeSubFile(managerWakeCursorDir(root), subID)
}

func wakeEligibleEventType(evType string) bool {
	switch evType {
	case evDone, evHeld, evFailed, evNeedsOwner, evCanceled:
		return true
	default:
		return false
	}
}

// wakeEventID is the only identity recovery may mint. Retry after a crash that
// already fsynced the row is a no-op; retry after a crash that lost the row
// rebuilds the same id. A second identity for the same committed event is never
// allowed.
func wakeEventID(taskID string, seq int64, transitionID string) string {
	return fmt.Sprintf("%s:%d:%s", taskID, seq, transitionID)
}

func rowWakeIdentity(row managerWakeOutboxRow) string {
	return wakeEventID(row.TaskID, row.TaskEventSeq, row.TransitionID)
}

func outboxRowIdentityConsistent(row managerWakeOutboxRow) bool {
	if row.Schema != outboxSchemaV1 || row.Seq <= 0 || row.TaskEventSeq <= 0 || row.TransitionID == "" {
		return false
	}
	if _, ok := exactCanonicalOutgoingTaskID(row.TaskID); !ok {
		return false
	}
	if !wakeEligibleEventType(row.EventType) {
		return false
	}
	return row.WakeEventID == rowWakeIdentity(row)
}

func exactCanonicalOutgoingTaskID(id string) (string, bool) {
	if id == "" || strings.TrimSpace(id) != id {
		return "", false
	}
	closed, ok := closedManagerWakeTaskID(id)
	if !ok || closed != id {
		return "", false
	}
	if id == managerWakeOnceLockID || id == managerWakeOutboxLockID {
		return "", false
	}
	return id, true
}

func failOutgoingTaskIdentity(root string, metrics *managerWakeMetrics) error {
	if metrics != nil {
		metrics.LastErrorClass = "outbox_identity_mismatch"
	}
	noteManagerWakeError(root, metrics, "outbox_identity_mismatch")
	return fmt.Errorf("outbox_identity_mismatch")
}

func closedWakeReasonClass(actor, evType string, detail map[string]any) string {
	c := closedReasonClass(actor, evType, detail)
	if managerWakeReasonRE.MatchString(c) {
		return c
	}
	if managerWakeReasonRE.MatchString(evType) {
		return evType
	}
	return "unknown"
}

func wakeEventStillCurrent(t *Task, ev TaskEvent) bool {
	if t == nil || ev.TransitionID == "" || ev.Seq <= 0 {
		return false
	}
	if t.LastCommittedTransitionID != ev.TransitionID {
		return false
	}
	if t.Status != "" && ev.Status != "" && t.Status != ev.Status {
		return false
	}
	return true
}

func committedEventIdentity(root string, t *Task, ev TaskEvent) bool {
	if t == nil || root == "" || ev.Seq <= 0 || ev.TransitionID == "" || !wakeEligibleEventType(ev.Type) {
		return false
	}
	if !transitionDurablyCommitted(root, t.ID, ev.TransitionID) {
		return false
	}
	rec, err := loadTransition(root, t.ID, ev.TransitionID)
	if err != nil || rec == nil || rec.TaskID != t.ID {
		return false
	}
	if rec.EventType != "" && rec.EventType != ev.Type {
		return false
	}
	events, _, err := loadTaskEvents(root, t.ID)
	if err != nil {
		return false
	}
	for _, got := range events {
		if got.Seq != ev.Seq {
			continue
		}
		return got.TransitionID == ev.TransitionID && got.Type == ev.Type
	}
	return false
}

func committedWakeEligible(root string, t *Task, ev TaskEvent) bool {
	if t == nil || !wakeEligibleEventType(ev.Type) || ev.Seq <= 0 || ev.TransitionID == "" {
		return false
	}
	if !wakeEventStillCurrent(t, ev) {
		return false
	}
	return committedEventIdentity(root, t, ev)
}

func allowlistedWakeError(err error, fallback string) string {
	if err != nil {
		c := strings.TrimSpace(err.Error())
		if managerWakeReasonRE.MatchString(c) {
			return c
		}
	}
	if managerWakeReasonRE.MatchString(fallback) {
		return fallback
	}
	return "outbox_append_failed"
}

func failWakeProjection(root string, err error) error {
	class := allowlistedWakeError(err, "outbox_append_failed")
	metrics := loadManagerWakeMetrics(root)
	noteManagerWakeError(root, &metrics, class)
	return fmt.Errorf("%s", class)
}

func projectCommittedWake(root string, t *Task, ev TaskEvent) error {
	if root == "" || t == nil || !committedWakeEligible(root, t, ev) {
		return nil
	}
	if err := appendOutboxRow(root, t, ev); err != nil {
		return failWakeProjection(root, err)
	}
	return nil
}

func backfillWakeForTask(root string, t *Task) error {
	if t == nil || root == "" {
		return nil
	}
	events, _, err := loadTaskEvents(root, t.ID)
	if err != nil {
		return failWakeProjection(root, err)
	}
	for _, ev := range events {
		if err := projectCommittedWake(root, t, ev); err != nil {
			return err
		}
	}
	return nil
}

func appendManagerWakeFromCommitted(root string, t *Task, ev TaskEvent) error {
	return projectCommittedWake(root, t, ev)
}

// projectWakeAfterCommitted appends a wake row only after the exact committed
// task event exists. Uncommitted or unjournaled state is a no-op. Disabled
// (default) configuration emits no outbox files.
func projectWakeAfterCommitted(root string, t *Task, transitionID string) {
	if t == nil || root == "" || transitionID == "" || !managerWakeProjectionEnabled(root) {
		return
	}
	events, _, err := loadTaskEvents(root, t.ID)
	if err != nil {
		_ = failWakeProjection(root, err)
		return
	}
	for _, ev := range events {
		if ev.TransitionID != transitionID {
			continue
		}
		if err := appendManagerWakeFromCommitted(root, t, ev); err != nil {
			_ = failWakeProjection(root, err)
		}
		return
	}
}

func managerWakeProjectionEnabled(root string) bool {
	cfg, err := loadConfig(root)
	if err != nil || cfg == nil {
		return false
	}
	return managerWakeEnabled(cfg.ManagerWake)
}

func managerWakeFromConfig(cfg *Config) *ManagerWakeConfig {
	if cfg == nil {
		return nil
	}
	return cfg.ManagerWake
}

func managerWakeConfigBlocking(root string, mw *ManagerWakeConfig) []string {
	if !managerWakeEnabled(mw) {
		return []string{"manager_wake_disabled"}
	}
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(mw.Subscriptions) == 0 {
		add("unscoped_subscription")
	}
	for _, d := range diagnoseManagerWake(root, mw) {
		switch d {
		case "watchdog_sec_rejected", "invalid_subscription_id", "duplicate_subscription",
			"invalid_thread_id", "unscoped_subscription", "empty_project", "missing_codex_bin",
			"invalid_event_type", "invalid_task_id", "invalid_dir_prefix":
			add(d)
		default:
			if strings.HasPrefix(d, "subscription_") {
				add(d)
			}
		}
	}
	return out
}

func appendOutboxRow(root string, t *Task, ev TaskEvent) error {
	if t == nil || root == "" || !committedWakeEligible(root, t, ev) {
		return nil
	}
	return withTaskControlLock(root, managerWakeOutboxLockID, func() error {
		return appendOutboxRowLocked(root, t, ev)
	})
}

func appendOutboxRowLocked(root string, t *Task, ev TaskEvent) error {
	if !committedWakeEligible(root, t, ev) {
		return nil
	}
	if err := os.MkdirAll(managerWakeDir(root), 0o755); err != nil {
		return err
	}
	id := wakeEventID(t.ID, ev.Seq, ev.TransitionID)
	existing, class, err := loadManagerWakeOutbox(root)
	if class != "" {
		if err != nil {
			return err
		}
		return fmt.Errorf("%s", class)
	}
	for _, row := range existing {
		if row.WakeEventID == id || rowWakeIdentity(row) == id {
			return nil
		}
	}
	var high int64
	for _, row := range existing {
		if row.Seq > high {
			high = row.Seq
		}
	}
	needsOwner := ev.Type == evNeedsOwner
	row := managerWakeOutboxRow{
		Schema:       outboxSchemaV1,
		Seq:          high + 1,
		WakeEventID:  id,
		TaskID:       t.ID,
		TaskEventSeq: ev.Seq,
		TransitionID: ev.TransitionID,
		Project:      t.Project,
		EventType:    ev.Type,
		Status:       ev.Status,
		NeedsOwner:   needsOwner,
		ReasonClass:  closedWakeReasonClass(ev.Actor, ev.Type, ev.Detail),
		TS:           ev.TS,
	}
	if row.TS == "" {
		row.TS = time.Now().Format(time.RFC3339Nano)
	}
	if row.Status == "" {
		row.Status = t.Status
	}
	data, err := json.Marshal(row)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := managerWakeOutboxPath(root)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func loadManagerWakeOutbox(root string) ([]managerWakeOutboxRow, string, error) {
	path := managerWakeOutboxPath(root)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", nil
		}
		return nil, "outbox_unreadable", fmt.Errorf("outbox_unreadable")
	}
	defer f.Close()
	var rows []managerWakeOutboxRow
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row managerWakeOutboxRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return rows, "outbox_corrupt", fmt.Errorf("outbox_corrupt")
		}
		rows = append(rows, row)
	}
	if err := sc.Err(); err != nil {
		return rows, "outbox_corrupt", fmt.Errorf("outbox_corrupt")
	}
	return rows, "", nil
}

func decodeClosedJSON(data []byte, dest any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing")
	}
	return nil
}

func loadManagerWakeCursor(root, subID string) (*managerWakeCursor, string, error) {
	path := managerWakeCursorPath(root, subID)
	if path == "" {
		return nil, "invalid_subscription_id", fmt.Errorf("invalid_subscription_id")
	}
	id, ok := closedManagerWakeSubID(subID)
	if !ok {
		return nil, "invalid_subscription_id", fmt.Errorf("invalid_subscription_id")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &managerWakeCursor{Schema: cursorSchemaV1, SubscriptionID: id}, "", nil
		}
		return nil, "cursor_unreadable", fmt.Errorf("cursor_unreadable")
	}
	var cur managerWakeCursor
	if err := decodeClosedJSON(data, &cur); err != nil {
		return nil, "cursor_corrupt", fmt.Errorf("cursor_corrupt")
	}
	if cur.Schema != cursorSchemaV1 || cur.SubscriptionID != id || cur.OutboxSeq < 0 {
		return nil, "cursor_corrupt", fmt.Errorf("cursor_corrupt")
	}
	if cur.LastWakeID != "" && strings.TrimSpace(cur.LastWakeID) != cur.LastWakeID {
		return nil, "cursor_corrupt", fmt.Errorf("cursor_corrupt")
	}
	return &cur, "", nil
}

func validateCursorAgainstOutbox(cur *managerWakeCursor, rows []managerWakeOutboxRow) string {
	if cur == nil {
		return "cursor_corrupt"
	}
	var high int64
	seen := map[string]bool{}
	for _, row := range rows {
		if row.Seq > high {
			high = row.Seq
		}
		if row.WakeEventID != "" {
			seen[row.WakeEventID] = true
		}
	}
	if cur.OutboxSeq > high {
		return "cursor_seq_bounds"
	}
	if cur.LastWakeID != "" && !seen[cur.LastWakeID] {
		return "cursor_seq_bounds"
	}
	return ""
}

func saveManagerWakeCursor(root string, cur *managerWakeCursor) error {
	if cur == nil {
		return fmt.Errorf("invalid_subscription_id")
	}
	id, ok := closedManagerWakeSubID(cur.SubscriptionID)
	if !ok {
		return fmt.Errorf("invalid_subscription_id")
	}
	cur.SubscriptionID = id
	cur.Schema = cursorSchemaV1
	cur.UpdatedAt = time.Now().Format(time.RFC3339Nano)
	path := managerWakeCursorPath(root, id)
	if path == "" {
		return fmt.Errorf("invalid_subscription_id")
	}
	data, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteSync(path, append(data, '\n'))
}

func failCursorSave(root string, metrics *managerWakeMetrics, err error) error {
	class := allowlistedWakeError(err, "cursor_save_failed")
	noteManagerWakeError(root, metrics, class)
	return fmt.Errorf("%s", class)
}

func loadManagerWakeReceiptIDs(root, subID string) (map[string]bool, string, error) {
	path := managerWakeReceiptPath(root, subID)
	if path == "" {
		return nil, "invalid_subscription_id", fmt.Errorf("invalid_subscription_id")
	}
	ids := map[string]bool{}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ids, "", nil
		}
		return nil, "receipt_unreadable", fmt.Errorf("receipt_unreadable")
	}
	want, ok := closedManagerWakeSubID(subID)
	if !ok {
		return nil, "invalid_subscription_id", fmt.Errorf("invalid_subscription_id")
	}
	var rec managerWakeReceipt
	if err := decodeClosedJSON(data, &rec); err != nil {
		return nil, "receipt_corrupt", fmt.Errorf("receipt_corrupt")
	}
	if rec.Schema != receiptSchemaV1 || rec.SubscriptionID != want {
		return nil, "receipt_corrupt", fmt.Errorf("receipt_corrupt")
	}
	for _, id := range rec.WakeEventIDs {
		if id == "" || strings.TrimSpace(id) != id || ids[id] {
			return nil, "receipt_corrupt", fmt.Errorf("receipt_corrupt")
		}
		ids[id] = true
	}
	return ids, "", nil
}

func validateReceiptMembership(ids map[string]bool, rows []managerWakeOutboxRow) string {
	if len(ids) == 0 {
		return ""
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if row.WakeEventID != "" {
			seen[row.WakeEventID] = true
		}
	}
	for id := range ids {
		if !seen[id] {
			return "receipt_foreign"
		}
	}
	return ""
}

func rememberManagerWakeReceipts(root, subID string, rows []managerWakeOutboxRow) error {
	path := managerWakeReceiptPath(root, subID)
	if path == "" {
		return fmt.Errorf("invalid_subscription_id")
	}
	ids, class, err := loadManagerWakeReceiptIDs(root, subID)
	if class != "" {
		if err != nil {
			return err
		}
		return fmt.Errorf("%s", class)
	}
	for _, row := range rows {
		if row.WakeEventID != "" {
			ids[row.WakeEventID] = true
		}
	}
	list := make([]string, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	sort.Strings(list)
	id, _ := closedManagerWakeSubID(subID)
	rec := managerWakeReceipt{
		Schema:         receiptSchemaV1,
		SubscriptionID: id,
		WakeEventIDs:   list,
		UpdatedAt:      time.Now().Format(time.RFC3339Nano),
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteSync(path, append(data, '\n'))
}

func durableUnlinkFile(path string) error {
	if path == "" {
		return fmt.Errorf("invalid_subscription_id")
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return syncDirAfterRename(filepath.Dir(path))
}

func ensureDurableManagerWakeInflightDirs(root string) error {
	inflight := managerWakeInflightDir(root)
	if err := os.MkdirAll(inflight, 0o755); err != nil {
		return err
	}
	if err := syncDirAfterRename(inflight); err != nil {
		return err
	}
	return syncDirAfterRename(managerWakeDir(root))
}

func parseValidManagerWakeInflight(data []byte, subID string) (*managerWakeInflight, bool) {
	id, ok := closedManagerWakeSubID(subID)
	if !ok {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var rec managerWakeInflight
	if err := dec.Decode(&rec); err != nil {
		return nil, false
	}
	if dec.More() {
		return nil, false
	}
	if rec.Schema != inflightSchemaV1 || rec.SubscriptionID != id {
		return nil, false
	}
	if rec.Phase != inflightPhaseClaimed && rec.Phase != inflightPhaseStarting && rec.Phase != inflightPhaseSpawned {
		return nil, false
	}
	if rec.ThreadID != strings.TrimSpace(rec.ThreadID) || !managerWakeThreadRE.MatchString(rec.ThreadID) {
		return nil, false
	}
	if len(rec.WakeEventIDs) == 0 {
		return nil, false
	}
	seen := map[string]bool{}
	for _, wid := range rec.WakeEventIDs {
		if wid == "" || strings.TrimSpace(wid) != wid || seen[wid] {
			return nil, false
		}
		seen[wid] = true
	}
	return &rec, true
}

func loadManagerWakeInflight(root, subID string) (*managerWakeInflight, string, error) {
	path := managerWakeInflightPath(root, subID)
	if path == "" {
		return nil, "invalid_subscription_id", fmt.Errorf("invalid_subscription_id")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", nil
		}
		return nil, "inflight_unreadable", fmt.Errorf("inflight_unreadable")
	}
	rec, ok := parseValidManagerWakeInflight(data, subID)
	if !ok {
		return nil, "delivery_uncertain", fmt.Errorf("delivery_uncertain")
	}
	return rec, "", nil
}

func saveManagerWakeInflight(root, subID, threadID string, rows []managerWakeOutboxRow, highWater int64) error {
	return saveManagerWakeInflightPhase(root, subID, threadID, rows, highWater, inflightPhaseClaimed)
}

func saveManagerWakeInflightPhase(root, subID, threadID string, rows []managerWakeOutboxRow, highWater int64, phase string) error {
	path := managerWakeInflightPath(root, subID)
	if path == "" {
		return fmt.Errorf("invalid_subscription_id")
	}
	if phase != inflightPhaseClaimed && phase != inflightPhaseStarting && phase != inflightPhaseSpawned {
		return fmt.Errorf("inflight_save_failed")
	}
	if err := ensureDurableManagerWakeInflightDirs(root); err != nil {
		return err
	}
	id, _ := closedManagerWakeSubID(subID)
	list := make([]string, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		wid := strings.TrimSpace(row.WakeEventID)
		if wid == "" || seen[wid] {
			continue
		}
		seen[wid] = true
		list = append(list, wid)
	}
	sort.Strings(list)
	if len(list) == 0 {
		return fmt.Errorf("inflight_save_failed")
	}
	rec := managerWakeInflight{
		Schema:          inflightSchemaV1,
		SubscriptionID:  id,
		ThreadID:        strings.TrimSpace(threadID),
		Phase:           phase,
		WakeEventIDs:    list,
		OutboxHighWater: highWater,
		UpdatedAt:       time.Now().Format(time.RFC3339Nano),
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteSync(path, append(data, '\n'))
}

func clearManagerWakeInflight(root, subID string) error {
	path := managerWakeInflightPath(root, subID)
	if path == "" {
		return fmt.Errorf("invalid_subscription_id")
	}
	return durableUnlinkFile(path)
}

func receiptsCoverInflight(receipts map[string]bool, ids []string) bool {
	if len(ids) == 0 {
		return false
	}
	for _, id := range ids {
		if !receipts[id] {
			return false
		}
	}
	return true
}

func inflightClaimUnresolved(root, subID, expectThread string) bool {
	rec, class, _ := loadManagerWakeInflight(root, subID)
	if class != "" {
		return class != "invalid_subscription_id"
	}
	if rec == nil {
		return false
	}
	if expectThread != "" && rec.ThreadID != strings.TrimSpace(expectThread) {
		return true
	}
	receipts, class, _ := loadManagerWakeReceiptIDs(root, subID)
	if class != "" {
		return true
	}
	return !receiptsCoverInflight(receipts, rec.WakeEventIDs)
}

func unresolvedManagerWakeInflightExists(root string, mw *ManagerWakeConfig) bool {
	seen := map[string]bool{}
	if mw != nil {
		for _, sub := range mw.Subscriptions {
			if inflightClaimUnresolved(root, sub.ID, sub.ThreadID) {
				return true
			}
			seen[sub.ID] = true
		}
	}
	entries, err := os.ReadDir(managerWakeInflightDir(root))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if seen[id] {
			continue
		}
		if inflightClaimUnresolved(root, id, "") {
			return true
		}
	}
	return false
}

func reconcileManagerWakeInflight(root string, sub ManagerWakeSubscription, cur *managerWakeCursor, metrics *managerWakeMetrics, rows []managerWakeOutboxRow) ([]managerWakeOutboxRow, error) {
	rec, class, err := loadManagerWakeInflight(root, sub.ID)
	if class == "inflight_unreadable" || class == "invalid_subscription_id" {
		metrics.LastErrorClass = class
		noteManagerWakeError(root, metrics, class)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%s", class)
	}
	if class != "" {
		return nil, failDeliveryUncertain(root, cur, metrics)
	}
	if rec == nil {
		return nil, nil
	}
	if rec.ThreadID != strings.TrimSpace(sub.ThreadID) {
		return nil, failDeliveryUncertain(root, cur, metrics)
	}
	receipts, class, _ := loadManagerWakeReceiptIDs(root, sub.ID)
	if class != "" {
		return nil, failDeliveryUncertain(root, cur, metrics)
	}
	if receiptsCoverInflight(receipts, rec.WakeEventIDs) {
		if err := clearManagerWakeInflight(root, sub.ID); err != nil {
			class := allowlistedWakeError(err, "inflight_save_failed")
			metrics.LastErrorClass = class
			noteManagerWakeError(root, metrics, class)
			return nil, fmt.Errorf("%s", class)
		}
		return nil, nil
	}
	if rec.Phase != inflightPhaseClaimed {
		return nil, failDeliveryUncertain(root, cur, metrics)
	}
	retry, foreign, mismatch := rebindInflightEventIDs(root, sub, rows, rec.WakeEventIDs)
	if mismatch {
		metrics.LastErrorClass = "outbox_identity_mismatch"
		noteManagerWakeError(root, metrics, "outbox_identity_mismatch")
		return nil, fmt.Errorf("outbox_identity_mismatch")
	}
	if foreign {
		return nil, failDeliveryUncertain(root, cur, metrics)
	}
	if len(retry) == 0 {
		if err := clearManagerWakeInflight(root, sub.ID); err != nil {
			class := allowlistedWakeError(err, "inflight_save_failed")
			metrics.LastErrorClass = class
			noteManagerWakeError(root, metrics, class)
			return nil, fmt.Errorf("%s", class)
		}
		return nil, nil
	}
	return retry, nil
}

func rebindInflightEventIDs(root string, sub ManagerWakeSubscription, rows []managerWakeOutboxRow, ids []string) (current []managerWakeOutboxRow, foreign, mismatch bool) {
	byID := map[string]managerWakeOutboxRow{}
	for _, row := range rows {
		byID[row.WakeEventID] = row
	}
	for _, id := range ids {
		row, ok := byID[id]
		if !ok {
			return nil, true, false
		}
		if _, ok := exactCanonicalOutgoingTaskID(row.TaskID); !ok {
			return nil, false, true
		}
		t, err := findTaskAnywhere(root, row.TaskID)
		if err != nil && t == nil && !strings.Contains(err.Error(), "不存在") {
			return nil, false, true
		}
		rebuilt, kind := bindCommittedWakeRow(root, t, row)
		switch kind {
		case wakeBindMismatch:
			return nil, false, true
		case wakeBindStale:
			continue
		case wakeBindOK:
			if subscriptionMatches(sub, t, rebuilt) {
				current = append(current, rebuilt)
			}
		}
	}
	return current, false, false
}

func rebindWakeDelta(root string, sub ManagerWakeSubscription, delta []managerWakeOutboxRow) ([]managerWakeOutboxRow, error) {
	var out []managerWakeOutboxRow
	for _, row := range delta {
		if _, ok := exactCanonicalOutgoingTaskID(row.TaskID); !ok {
			return nil, fmt.Errorf("outbox_identity_mismatch")
		}
		t, err := findTaskAnywhere(root, row.TaskID)
		if err != nil && t == nil && !strings.Contains(err.Error(), "不存在") {
			return nil, fmt.Errorf("outbox_identity_mismatch")
		}
		rebuilt, kind := bindCommittedWakeRow(root, t, row)
		switch kind {
		case wakeBindMismatch:
			return nil, fmt.Errorf("outbox_identity_mismatch")
		case wakeBindStale:
			continue
		case wakeBindOK:
			if subscriptionMatches(sub, t, rebuilt) {
				out = append(out, rebuilt)
			}
		}
	}
	return out, nil
}

func failDeliveryUncertain(root string, cur *managerWakeCursor, metrics *managerWakeMetrics) error {
	if cur != nil {
		cur.LastErrorClass = "delivery_uncertain"
		_ = saveManagerWakeCursor(root, cur)
	}
	if metrics != nil {
		metrics.LastErrorClass = "delivery_uncertain"
	}
	noteManagerWakeError(root, metrics, "delivery_uncertain")
	return fmt.Errorf("delivery_uncertain")
}

func loadManagerWakeMetrics(root string) managerWakeMetrics {
	var m managerWakeMetrics
	data, err := os.ReadFile(managerWakeMetricsPath(root))
	if err != nil {
		return m
	}
	_ = json.Unmarshal(data, &m)
	return m
}

func saveManagerWakeMetrics(root string, m managerWakeMetrics) {
	m.WatchdogSec = managerWakeWatchdogSec
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	_ = atomicWriteSync(managerWakeMetricsPath(root), append(data, '\n'))
}

func persistManagerWakeError(root, class string) {
	if strings.TrimSpace(class) == "" || !managerWakeReasonRE.MatchString(class) {
		return
	}
	st := managerWakeErrorState{
		Schema: errorSchemaV1,
		Class:  class,
		At:     time.Now().Format(time.RFC3339Nano),
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	_ = atomicWriteSync(managerWakeErrorPath(root), append(data, '\n'))
}

func clearManagerWakeError(root string) {
	_ = os.Remove(managerWakeErrorPath(root))
}

func loadManagerWakeErrorClass(root string) string {
	data, err := os.ReadFile(managerWakeErrorPath(root))
	if err != nil {
		return ""
	}
	var st managerWakeErrorState
	if json.Unmarshal(data, &st) != nil || st.Class == "" {
		return "error_state_corrupt"
	}
	if !managerWakeReasonRE.MatchString(st.Class) {
		return "error_state_corrupt"
	}
	return st.Class
}

func noteManagerWakeError(root string, metrics *managerWakeMetrics, class string) {
	if metrics != nil {
		metrics.LastErrorClass = class
		saveManagerWakeMetrics(root, *metrics)
	}
	persistManagerWakeError(root, class)
}

func effectiveManagerWakeWatchdog(_ *ManagerWakeConfig) int {
	return managerWakeWatchdogSec
}

func managerWakeEnabled(mw *ManagerWakeConfig) bool {
	return mw != nil && mw.Enabled
}

func closedManagerWakeTaskID(id string) (string, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", false
	}
	if _, ok := closedManagerWakeSubID(id); !ok {
		return "", false
	}
	return id, true
}

func closedManagerWakeDirPrefix(p string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" || strings.TrimSpace(p) != p {
		return "", false
	}
	if strings.Contains(p, "..") {
		return "", false
	}
	clean := filepath.Clean(p)
	if clean == "." || clean == string(filepath.Separator) {
		return "", false
	}
	if filepath.IsAbs(p) {
		if clean != filepath.Clean(p) {
			return "", false
		}
		return clean, true
	}
	if filepath.IsAbs(clean) {
		return "", false
	}
	return clean, true
}

func closedManagerWakeSubscription(sub ManagerWakeSubscription) (ManagerWakeSubscription, string) {
	id, ok := closedManagerWakeSubID(sub.ID)
	if !ok {
		return sub, "invalid_subscription_id"
	}
	sub.ID = id
	if !managerWakeThreadRE.MatchString(strings.TrimSpace(sub.ThreadID)) {
		return sub, "invalid_thread_id"
	}
	sub.ThreadID = strings.TrimSpace(sub.ThreadID)
	if len(sub.Projects) == 0 && len(sub.TaskIDs) == 0 && len(sub.DirPrefixes) == 0 {
		return sub, "unscoped_subscription"
	}
	seenET := map[string]bool{}
	for _, et := range sub.EventTypes {
		et = strings.TrimSpace(et)
		if et == "" || !wakeEligibleEventType(et) || seenET[et] {
			return sub, "invalid_event_type"
		}
		seenET[et] = true
	}
	seenTID := map[string]bool{}
	for i, id := range sub.TaskIDs {
		tid, ok := closedManagerWakeTaskID(id)
		if !ok || seenTID[tid] {
			return sub, "invalid_task_id"
		}
		seenTID[tid] = true
		sub.TaskIDs[i] = tid
	}
	for i, p := range sub.DirPrefixes {
		pref, ok := closedManagerWakeDirPrefix(p)
		if !ok {
			return sub, "invalid_dir_prefix"
		}
		sub.DirPrefixes[i] = pref
	}
	for _, p := range sub.Projects {
		if strings.TrimSpace(p) == "" {
			return sub, "empty_project"
		}
	}
	return sub, ""
}

func noteWakeClass(root string, cur *managerWakeCursor, metrics *managerWakeMetrics, class string) error {
	if cur != nil {
		cur.LastErrorClass = class
		_ = saveManagerWakeCursor(root, cur)
	}
	if metrics != nil {
		metrics.LastErrorClass = class
	}
	noteManagerWakeError(root, metrics, class)
	return fmt.Errorf("%s", class)
}

func diagnoseManagerWake(root string, mw *ManagerWakeConfig) []string {
	var out []string
	if mw == nil {
		return []string{"manager_wake_disabled"}
	}
	if !mw.Enabled {
		out = append(out, "manager_wake_disabled")
	}
	if mw.WatchdogSec != 0 && mw.WatchdogSec != managerWakeWatchdogSec {
		out = append(out, "watchdog_sec_rejected")
	}
	seen := map[string]bool{}
	for i, sub := range mw.Subscriptions {
		id := strings.TrimSpace(sub.ID)
		if id == "" {
			out = append(out, fmt.Sprintf("subscription_%d_missing_id", i))
			continue
		}
		if _, ok := closedManagerWakeSubID(id); !ok {
			out = append(out, "invalid_subscription_id")
			continue
		}
		if seen[id] {
			out = append(out, "duplicate_subscription")
		}
		seen[id] = true
		if _, class := closedManagerWakeSubscription(sub); class != "" {
			out = append(out, class)
		}
		if !managerWakeThreadRE.MatchString(strings.TrimSpace(sub.ThreadID)) {
			out = append(out, "invalid_thread_id")
		}
		if len(sub.Projects) == 0 && len(sub.TaskIDs) == 0 && len(sub.DirPrefixes) == 0 {
			out = append(out, "unscoped_subscription")
		}
		for _, p := range sub.Projects {
			if strings.TrimSpace(p) == "" {
				out = append(out, "empty_project")
				continue
			}
			if !projectKnown(root, p) {
				out = append(out, "unknown_project")
			}
		}
	}
	bin := strings.TrimSpace(mw.CodexBin)
	if mw.Enabled && bin == "" {
		out = append(out, "missing_codex_bin")
	} else if bin != "" {
		if _, err := os.Stat(bin); err != nil {
			if _, lookErr := exec.LookPath(bin); lookErr != nil {
				out = append(out, "missing_codex_bin")
			}
		}
	}
	if _, class, _ := loadManagerWakeOutbox(root); class != "" {
		out = append(out, class)
	}
	if class := loadManagerWakeErrorClass(root); class != "" {
		out = append(out, class)
	}
	entries, err := os.ReadDir(managerWakeCursorDir(root))
	if err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			id := strings.TrimSuffix(e.Name(), ".json")
			if _, ok := closedManagerWakeSubID(id); !ok {
				out = append(out, "cursor_name_rejected")
				continue
			}
			if _, class, _ := loadManagerWakeCursor(root, id); class != "" {
				out = append(out, class)
			}
		}
	}
	entries, err = os.ReadDir(managerWakeInflightDir(root))
	if err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			id := strings.TrimSuffix(e.Name(), ".json")
			if _, ok := closedManagerWakeSubID(id); !ok {
				out = append(out, "inflight_name_rejected")
				continue
			}
			rec, class, _ := loadManagerWakeInflight(root, id)
			if class != "" {
				out = append(out, class)
				continue
			}
			if rec == nil {
				continue
			}
			receipts, rclass, _ := loadManagerWakeReceiptIDs(root, id)
			if rec.Phase == inflightPhaseClaimed && rclass == "" {
				continue
			}
			if rclass != "" || !receiptsCoverInflight(receipts, rec.WakeEventIDs) {
				out = append(out, "delivery_uncertain")
			}
		}
	}
	return out
}

func managerWakeHasAnyTask(root string) bool {
	tasks, err := loadTasks(root)
	if err == nil && len(tasks) > 0 {
		return true
	}
	entries, err := os.ReadDir(archiveDir(root))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			return true
		}
	}
	return false
}

func projectKnown(root, project string) bool {
	project = strings.TrimSpace(project)
	if project == "" {
		return false
	}
	tasks, err := loadTasks(root)
	if err != nil {
		return false
	}
	for _, t := range tasks {
		if t.Project == project {
			return true
		}
	}
	archived, err := os.ReadDir(archiveDir(root))
	if err != nil {
		return false
	}
	for _, e := range archived {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		t, err := findTaskAnywhere(root, strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue
		}
		if t.Project == project {
			return true
		}
	}
	return false
}

func subscriptionMatches(sub ManagerWakeSubscription, t *Task, row managerWakeOutboxRow) bool {
	if !sub.Enabled {
		return false
	}
	if _, ok := closedManagerWakeSubID(sub.ID); !ok {
		return false
	}
	if len(sub.EventTypes) > 0 {
		ok := false
		for _, et := range sub.EventTypes {
			if et == row.EventType {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	matched := false
	if len(sub.TaskIDs) > 0 {
		for _, id := range sub.TaskIDs {
			if id == row.TaskID {
				matched = true
				break
			}
		}
	}
	if !matched && t != nil && len(sub.DirPrefixes) > 0 {
		dir := filepath.Clean(t.Dir)
		for _, p := range sub.DirPrefixes {
			pref := filepath.Clean(p)
			if dir == pref || strings.HasPrefix(dir, pref+string(os.PathSeparator)) {
				matched = true
				break
			}
		}
	}
	if !matched && len(sub.Projects) > 0 {
		for _, p := range sub.Projects {
			if p == row.Project {
				matched = true
				break
			}
		}
	}
	return matched
}

func reconcileManagerWakeOutbox(root string) error {
	seen := map[string]bool{}
	var first error
	walk := func(dir string) {
		if first != nil {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				return
			}
			first = failWakeProjection(root, err)
			return
		}
		for _, e := range entries {
			if first != nil {
				return
			}
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			id := strings.TrimSuffix(e.Name(), ".json")
			if seen[id] {
				continue
			}
			seen[id] = true
			t, err := findTaskAnywhere(root, id)
			if err != nil {
				first = failWakeProjection(root, err)
				return
			}
			if err := backfillWakeForTask(root, t); err != nil {
				first = err
				return
			}
		}
	}
	walk(tasksDir(root))
	walk(archiveDir(root))
	return first
}

// Local Codex 0.149.0 `queue` grammar is only `--thread` and `--message`.
// `--idempotency-key`, `--id`, `--client-request-id`, `--request-id`, and
// `--dedupe-key` are unexpected arguments. Do not invent a flag.
// `wake=` in `--message` and app-server `clientUserMessageId` are correlation
// only; they do not dedupe receiver model turns. A durable inflight claim
// must be fsynced before queue, and a missing receipt after that claim is
// fail-closed `delivery_uncertain` with no further model spawn.
func managerWakeQueueArgv(bin, thread, message string) []string {
	return []string{bin, "queue", "--thread", thread, "--message", message}
}

type managerWakeQueueChild struct {
	wait func() error
	kill func()
}

func beginDefaultManagerWakeQueue(bin, thread, message string) (*managerWakeQueueChild, error) {
	killHandlerOnce.Do(installKillHandler)
	if !wakeMessageCarriesEventIDs(message) {
		return nil, fmt.Errorf("queue_idempotency_unsupported")
	}
	args := managerWakeQueueArgv(bin, thread, message)
	ctx, cancel := context.WithTimeout(context.Background(), managerWakeQueueTimeout)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdin = nil
	setupProcGroup(cmd)
	if fn := managerWakeBeforeStart; fn != nil {
		fn()
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, errManagerWakeQueueStart
	}
	if managerWakeCrashAt == "after_start" {
		if cmd.Process != nil {
			_ = cmd.Wait()
		}
		cancel()
		os.Exit(99)
	}
	pid := 0
	if cmd.Process != nil {
		pid = cmd.Process.Pid
	}
	if pid > 0 {
		procMu.Lock()
		procGroups[pid] = true
		procMu.Unlock()
	}
	unregister := func() {
		if pid > 0 {
			procMu.Lock()
			delete(procGroups, pid)
			procMu.Unlock()
		}
		cancel()
	}
	if fn := managerWakeQueueSpawned; fn != nil {
		if err := fn(); err != nil {
			if pid > 0 {
				_ = killProcGroup(pid)
			}
			_ = cmd.Wait()
			unregister()
			return nil, err
		}
	}
	return &managerWakeQueueChild{
		wait: func() error {
			defer unregister()
			if err := rescueWaitDelay(cmd.Wait(), cmd); err != nil {
				return fmt.Errorf("queue_failed")
			}
			return nil
		},
		kill: func() {
			if pid > 0 {
				_ = killProcGroup(pid)
			}
		},
	}, nil
}

func defaultManagerWakeQueue(bin, thread, message string) error {
	child, err := beginDefaultManagerWakeQueue(bin, thread, message)
	if err != nil {
		return err
	}
	return child.wait()
}

func compactWakeMessage(sub ManagerWakeSubscription, rows []managerWakeOutboxRow, highWater int64) string {
	var b strings.Builder
	b.WriteString("cardex-wake v1")
	fmt.Fprintf(&b, " n=%d high_water=%d sub=%s", len(rows), highWater, sub.ID)
	for _, row := range rows {
		fmt.Fprintf(&b, "\n%s type=%s project=%s status=%s reason=%s transition=%s wake=%s",
			row.TaskID, row.EventType, emptyDash(row.Project), row.Status, row.ReasonClass, emptyDash(row.TransitionID), row.WakeEventID)
	}
	return b.String()
}

func wakeMessageCarriesEventIDs(msg string) bool {
	for _, line := range strings.Split(msg, "\n") {
		if strings.HasPrefix(line, "cardex-wake ") {
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.Contains(line, " wake=") {
			return false
		}
		id := strings.TrimSpace(line[strings.LastIndex(line, " wake=")+len(" wake="):])
		if id == "" || id == "-" {
			return false
		}
	}
	return strings.Contains(msg, " wake=")
}

func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func managerWakeOnce(root string, mw *ManagerWakeConfig) error {
	killHandlerOnce.Do(installKillHandler)
	if fn := managerWakeOnceEntered; fn != nil {
		fn()
	}
	// Dedicated control-lock identity, acquired before per-task reconcile
	// locks and the outbox append lock, and never the scheduler lock.
	return withTaskControlLock(root, managerWakeOnceLockID, func() error {
		return managerWakeOnceLocked(root, mw)
	})
}

func managerWakeOnceLocked(root string, mw *ManagerWakeConfig) error {
	reconcileControlPlane(root)
	if err := reconcileManagerWakeOutbox(root); err != nil {
		metrics := loadManagerWakeMetrics(root)
		metrics.WatchdogSec = managerWakeWatchdogSec
		metrics.Enabled = managerWakeEnabled(mw)
		class := allowlistedWakeError(err, "outbox_append_failed")
		noteManagerWakeError(root, &metrics, class)
		return fmt.Errorf("%s", class)
	}
	metrics := loadManagerWakeMetrics(root)
	metrics.WatchdogSec = managerWakeWatchdogSec
	metrics.Enabled = managerWakeEnabled(mw)
	metrics.LastWatchdogAt = time.Now().Format(time.RFC3339Nano)
	if _, err := os.Stat(managerWakeLaunchdPlistPath()); err == nil {
		metrics.LaunchdInstalled = true
	}

	rows, class, err := loadManagerWakeOutbox(root)
	if class != "" {
		noteManagerWakeError(root, &metrics, class)
		if err != nil {
			return err
		}
		return fmt.Errorf("%s", class)
	}
	for _, row := range rows {
		if !outboxRowIdentityConsistent(row) {
			noteManagerWakeError(root, &metrics, "outbox_identity_mismatch")
			return fmt.Errorf("outbox_identity_mismatch")
		}
	}
	var high int64
	for _, row := range rows {
		if row.Seq > high {
			high = row.Seq
		}
	}
	metrics.OutboxHighWater = high

	if !managerWakeEnabled(mw) {
		metrics.Pending = 0
		saveManagerWakeMetrics(root, metrics)
		return nil
	}

	bin := strings.TrimSpace(mw.CodexBin)
	pending := 0
	destSeen, destClass, destErr := preloadDestinationEventIDs(root, mw)
	if destClass != "" {
		if destClass == "delivery_uncertain" {
			return failDeliveryUncertain(root, nil, &metrics)
		}
		noteManagerWakeError(root, &metrics, destClass)
		if destErr != nil {
			return destErr
		}
		return fmt.Errorf("%s", destClass)
	}
	for _, sub := range mw.Subscriptions {
		if !sub.Enabled {
			continue
		}
		if err := deliverSubscription(root, sub, rows, bin, &metrics, destSeen); err != nil {
			saveManagerWakeMetrics(root, metrics)
			return err
		}
	}
	for _, sub := range mw.Subscriptions {
		if !sub.Enabled {
			continue
		}
		cur, _, _ := loadManagerWakeCursor(root, sub.ID)
		acked := int64(0)
		if cur != nil {
			acked = cur.OutboxSeq
		}
		for _, row := range rows {
			if row.Seq <= acked {
				continue
			}
			t, _ := findTaskAnywhere(root, row.TaskID)
			rebuilt, kind := bindCommittedWakeRow(root, t, row)
			if kind == wakeBindOK && subscriptionMatches(sub, t, rebuilt) {
				pending++
			}
		}
	}
	metrics.Pending = pending
	if metrics.LastErrorClass == "" {
		clearManagerWakeError(root)
	}
	saveManagerWakeMetrics(root, metrics)
	return nil
}

func rebuildOutboxRowFromCommitted(seq int64, t *Task, ev TaskEvent) managerWakeOutboxRow {
	status := ev.Status
	if status == "" && t != nil {
		status = t.Status
	}
	ts := ev.TS
	if ts == "" {
		ts = time.Now().Format(time.RFC3339Nano)
	}
	project := ""
	taskID := ""
	if t != nil {
		project = t.Project
		taskID = t.ID
	}
	return managerWakeOutboxRow{
		Schema:       outboxSchemaV1,
		Seq:          seq,
		WakeEventID:  wakeEventID(taskID, ev.Seq, ev.TransitionID),
		TaskID:       taskID,
		TaskEventSeq: ev.Seq,
		TransitionID: ev.TransitionID,
		Project:      project,
		EventType:    ev.Type,
		Status:       status,
		NeedsOwner:   ev.Type == evNeedsOwner,
		ReasonClass:  closedWakeReasonClass(ev.Actor, ev.Type, ev.Detail),
		TS:           ts,
	}
}

func bindCommittedWakeRow(root string, t *Task, row managerWakeOutboxRow) (managerWakeOutboxRow, wakeBindKind) {
	if !outboxRowIdentityConsistent(row) {
		return managerWakeOutboxRow{}, wakeBindMismatch
	}
	if t == nil || t.ID != row.TaskID {
		return managerWakeOutboxRow{}, wakeBindStale
	}
	events, _, err := loadTaskEvents(root, t.ID)
	if err != nil {
		return managerWakeOutboxRow{}, wakeBindMismatch
	}
	var ev TaskEvent
	found := false
	for _, got := range events {
		if got.Seq == row.TaskEventSeq {
			ev = got
			found = true
			break
		}
	}
	if !found {
		return managerWakeOutboxRow{}, wakeBindMismatch
	}
	if ev.TransitionID != row.TransitionID || ev.Type != row.EventType {
		return managerWakeOutboxRow{}, wakeBindMismatch
	}
	wantStatus := ev.Status
	if wantStatus == "" {
		wantStatus = t.Status
	}
	if row.Status != wantStatus {
		return managerWakeOutboxRow{}, wakeBindMismatch
	}
	if row.Project != t.Project {
		return managerWakeOutboxRow{}, wakeBindMismatch
	}
	if row.WakeEventID != wakeEventID(t.ID, ev.Seq, ev.TransitionID) {
		return managerWakeOutboxRow{}, wakeBindMismatch
	}
	if !committedWakeEligible(root, t, ev) {
		return managerWakeOutboxRow{}, wakeBindStale
	}
	return rebuildOutboxRowFromCommitted(row.Seq, t, ev), wakeBindOK
}

func canonicalManagerWakeThreadID(thread string) (string, bool) {
	thread = strings.TrimSpace(thread)
	if thread == "" || !managerWakeThreadRE.MatchString(thread) {
		return "", false
	}
	return strings.ToLower(thread), true
}

func seedDestinationEventIDs(dest map[string]map[string]bool, thread string, ids []string) bool {
	if dest == nil {
		return false
	}
	key, ok := canonicalManagerWakeThreadID(thread)
	if !ok {
		return false
	}
	if dest[key] == nil {
		dest[key] = map[string]bool{}
	}
	for _, id := range ids {
		if id != "" {
			dest[key][id] = true
		}
	}
	return true
}

func preloadDestinationEventIDs(root string, mw *ManagerWakeConfig) (map[string]map[string]bool, string, error) {
	dest := map[string]map[string]bool{}
	enabled := map[string]bool{}
	if mw != nil {
		for _, sub := range mw.Subscriptions {
			if id, ok := closedManagerWakeSubID(sub.ID); ok && sub.Enabled {
				enabled[id] = true
			}
			ids, class, _ := loadManagerWakeReceiptIDs(root, sub.ID)
			if class != "" {
				continue
			}
			for id := range ids {
				_ = seedDestinationEventIDs(dest, sub.ThreadID, []string{id})
			}
		}
	}
	entries, err := os.ReadDir(managerWakeInflightDir(root))
	if err != nil {
		if os.IsNotExist(err) {
			return dest, "", nil
		}
		return dest, "inflight_unreadable", fmt.Errorf("inflight_unreadable")
	}
	orphanUncertain := false
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if _, ok := closedManagerWakeSubID(id); !ok {
			return dest, "delivery_uncertain", fmt.Errorf("delivery_uncertain")
		}
		if managerWakeInflightPath(root, id) == "" {
			return dest, "delivery_uncertain", fmt.Errorf("delivery_uncertain")
		}
		rec, class, err := loadManagerWakeInflight(root, id)
		if class != "" {
			if err != nil {
				return dest, class, err
			}
			return dest, class, fmt.Errorf("%s", class)
		}
		if rec == nil {
			continue
		}
		if rec.Phase == inflightPhaseClaimed {
			continue
		}
		if rec.Phase != inflightPhaseStarting && rec.Phase != inflightPhaseSpawned {
			return dest, "delivery_uncertain", fmt.Errorf("delivery_uncertain")
		}
		if _, ok := canonicalManagerWakeThreadID(rec.ThreadID); !ok {
			return dest, "delivery_uncertain", fmt.Errorf("delivery_uncertain")
		}
		receipts, rclass, rerr := loadManagerWakeReceiptIDs(root, id)
		if rclass != "" {
			if rerr != nil {
				return dest, rclass, rerr
			}
			return dest, rclass, fmt.Errorf("%s", rclass)
		}
		if !seedDestinationEventIDs(dest, rec.ThreadID, rec.WakeEventIDs) {
			return dest, "delivery_uncertain", fmt.Errorf("delivery_uncertain")
		}
		if !receiptsCoverInflight(receipts, rec.WakeEventIDs) && !enabled[id] {
			orphanUncertain = true
		}
	}
	if orphanUncertain {
		return dest, "delivery_uncertain", fmt.Errorf("delivery_uncertain")
	}
	return dest, "", nil
}

func markDestinationEventIDs(dest map[string]map[string]bool, thread string, rows []managerWakeOutboxRow) {
	if dest == nil {
		return
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.WakeEventID != "" {
			ids = append(ids, row.WakeEventID)
		}
	}
	_ = seedDestinationEventIDs(dest, thread, ids)
}

func deliverSubscription(root string, sub ManagerWakeSubscription, rows []managerWakeOutboxRow, bin string, metrics *managerWakeMetrics, destSeen map[string]map[string]bool) error {
	closed, cfgClass := closedManagerWakeSubscription(sub)
	id, ok := closedManagerWakeSubID(sub.ID)
	if !ok {
		metrics.LastErrorClass = "invalid_subscription_id"
		noteManagerWakeError(root, metrics, "invalid_subscription_id")
		return fmt.Errorf("invalid_subscription_id")
	}
	sub.ID = id
	cur, class, err := loadManagerWakeCursor(root, sub.ID)
	if class != "" {
		metrics.LastErrorClass = class
		noteManagerWakeError(root, metrics, class)
		if err != nil {
			return err
		}
		return fmt.Errorf("%s", class)
	}
	if cfgClass != "" {
		return noteWakeClass(root, cur, metrics, cfgClass)
	}
	sub = closed
	if class := validateCursorAgainstOutbox(cur, rows); class != "" {
		return noteWakeClass(root, cur, metrics, class)
	}
	retry, err := reconcileManagerWakeInflight(root, sub, cur, metrics, rows)
	if err != nil {
		return err
	}
	for _, p := range sub.Projects {
		if strings.TrimSpace(p) != "" && !projectKnown(root, p) && managerWakeHasAnyTask(root) {
			return noteWakeClass(root, cur, metrics, "unknown_project")
		}
	}
	receipts, class, err := loadManagerWakeReceiptIDs(root, sub.ID)
	if class != "" {
		metrics.LastErrorClass = class
		noteManagerWakeError(root, metrics, class)
		if err != nil {
			return err
		}
		return fmt.Errorf("%s", class)
	}
	if class := validateReceiptMembership(receipts, rows); class != "" {
		return noteWakeClass(root, cur, metrics, class)
	}
	if len(retry) > 0 {
		if err := queueWakeRows(root, sub, retry, retry[len(retry)-1].Seq, bin, cur, metrics, destSeen); err != nil {
			return err
		}
	}
	var delta []managerWakeOutboxRow
	var scanHigh int64 = cur.OutboxSeq
	stale := 0
	for _, row := range rows {
		if row.Seq <= cur.OutboxSeq {
			continue
		}
		if row.Seq > scanHigh {
			scanHigh = row.Seq
		}
		if _, ok := exactCanonicalOutgoingTaskID(row.TaskID); !ok {
			return failOutgoingTaskIdentity(root, metrics)
		}
		t, ferr := findTaskAnywhere(root, row.TaskID)
		if ferr != nil && t == nil && !strings.Contains(ferr.Error(), "不存在") {
			metrics.LastErrorClass = "outbox_identity_mismatch"
			noteManagerWakeError(root, metrics, "outbox_identity_mismatch")
			return fmt.Errorf("outbox_identity_mismatch")
		}
		rebuilt, kind := bindCommittedWakeRow(root, t, row)
		switch kind {
		case wakeBindMismatch:
			metrics.LastErrorClass = "outbox_identity_mismatch"
			noteManagerWakeError(root, metrics, "outbox_identity_mismatch")
			return fmt.Errorf("outbox_identity_mismatch")
		case wakeBindStale:
			stale++
			continue
		}
		if !subscriptionMatches(sub, t, rebuilt) {
			continue
		}
		delta = append(delta, rebuilt)
	}
	metrics.StaleSuppressed += stale
	if len(delta) == 0 {
		metrics.NoDeltaScans++
		if scanHigh > cur.OutboxSeq {
			cur.OutboxSeq = scanHigh
			if err := saveManagerWakeCursor(root, cur); err != nil {
				return failCursorSave(root, metrics, err)
			}
		}
		return nil
	}
	sort.Slice(delta, func(i, j int) bool { return delta[i].Seq < delta[j].Seq })
	return queueWakeRows(root, sub, delta, scanHigh, bin, cur, metrics, destSeen)
}

func canonicalOutgoingWakeTaskIDs(rows []managerWakeOutboxRow) ([]string, error) {
	seen := make(map[string]bool, len(rows))
	var ids []string
	for _, row := range rows {
		id, ok := exactCanonicalOutgoingTaskID(row.TaskID)
		if !ok {
			return nil, fmt.Errorf("outbox_identity_mismatch")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}

func withSortedTaskControlLocks(root string, taskIDs []string, fn func() error) error {
	ids := append([]string(nil), taskIDs...)
	sort.Strings(ids)
	var acquire func(int) error
	acquire = func(i int) error {
		if i == len(ids) {
			return fn()
		}
		return withTaskControlLock(root, ids[i], func() error {
			return acquire(i + 1)
		})
	}
	return acquire(0)
}

func saveWakeInflightOrClass(root, subID, threadID string, rows []managerWakeOutboxRow, highWater int64, phase string, metrics *managerWakeMetrics) error {
	if err := saveManagerWakeInflightPhase(root, subID, threadID, rows, highWater, phase); err != nil {
		class := allowlistedWakeError(err, "inflight_save_failed")
		if metrics != nil {
			metrics.LastErrorClass = class
		}
		noteManagerWakeError(root, metrics, class)
		return fmt.Errorf("%s", class)
	}
	return nil
}

func queueWakeRows(root string, sub ManagerWakeSubscription, delta []managerWakeOutboxRow, scanHigh int64, bin string, cur *managerWakeCursor, metrics *managerWakeMetrics, destSeen map[string]map[string]bool) error {
	if fn := managerWakeBeforeQueue; fn != nil {
		fn()
	}
	ids, err := canonicalOutgoingWakeTaskIDs(delta)
	if err != nil {
		return failOutgoingTaskIdentity(root, metrics)
	}
	type pendingWakeQueue struct {
		toSend []managerWakeOutboxRow
		delta  []managerWakeOutboxRow
		wait   func() error
	}
	var pending *pendingWakeQueue
	lockErr := withSortedTaskControlLocks(root, ids, func() error {
		rebound, err := rebindWakeDelta(root, sub, delta)
		if err != nil {
			return failOutgoingTaskIdentity(root, metrics)
		}
		delta = rebound
		if _, err := canonicalOutgoingWakeTaskIDs(delta); err != nil {
			return failOutgoingTaskIdentity(root, metrics)
		}
		if strings.TrimSpace(bin) == "" {
			metrics.LastErrorClass = "missing_codex_bin"
			noteManagerWakeError(root, metrics, "missing_codex_bin")
			return fmt.Errorf("missing_codex_bin")
		}
		if _, err := os.Stat(bin); err != nil {
			if _, lookErr := exec.LookPath(bin); lookErr != nil {
				metrics.LastErrorClass = "missing_codex_bin"
				noteManagerWakeError(root, metrics, "missing_codex_bin")
				return fmt.Errorf("missing_codex_bin")
			}
		}
		receipts, class, err := loadManagerWakeReceiptIDs(root, sub.ID)
		if class != "" {
			metrics.LastErrorClass = class
			noteManagerWakeError(root, metrics, class)
			if err != nil {
				return err
			}
			return fmt.Errorf("%s", class)
		}
		var already map[string]bool
		if destSeen != nil {
			if key, ok := canonicalManagerWakeThreadID(sub.ThreadID); ok {
				already = destSeen[key]
			}
		}
		var toSend []managerWakeOutboxRow
		for _, row := range delta {
			if receipts[row.WakeEventID] || (already != nil && already[row.WakeEventID]) {
				continue
			}
			toSend = append(toSend, row)
		}
		commit := func() error {
			cur.OutboxSeq = scanHigh
			if len(delta) > 0 {
				cur.LastWakeID = delta[len(delta)-1].WakeEventID
			}
			cur.LastErrorClass = ""
			cur.LastDeliveryAt = time.Now().Format(time.RFC3339Nano)
			if err := managerWakeCommitCursor(root, cur); err != nil {
				return failCursorSave(root, metrics, err)
			}
			_ = clearManagerWakeInflight(root, sub.ID)
			return nil
		}
		if len(toSend) == 0 {
			if len(delta) == 0 {
				return nil
			}
			return commit()
		}
		msg := compactWakeMessage(sub, toSend, scanHigh)
		if !wakeMessageCarriesEventIDs(msg) {
			metrics.LastErrorClass = "queue_idempotency_unsupported"
			noteManagerWakeError(root, metrics, "queue_idempotency_unsupported")
			return fmt.Errorf("queue_idempotency_unsupported")
		}
		if secretfulWakeMessage(msg) {
			metrics.LastErrorClass = "payload_rejected"
			noteManagerWakeError(root, metrics, "payload_rejected")
			return fmt.Errorf("payload_rejected")
		}
		if err := saveWakeInflightOrClass(root, sub.ID, sub.ThreadID, toSend, scanHigh, inflightPhaseClaimed, metrics); err != nil {
			return err
		}
		crashWakeIf("after_claimed")
		if err := saveWakeInflightOrClass(root, sub.ID, sub.ThreadID, toSend, scanHigh, inflightPhaseStarting, metrics); err != nil {
			return err
		}
		crashWakeIf("after_starting")
		metrics.QueueAttempts++
		var (
			wait        func() error
			kill        func()
			startFailed bool
			qerr        error
		)
		if usingDefaultManagerWakeQueue() {
			child, err := beginDefaultManagerWakeQueue(bin, sub.ThreadID, msg)
			qerr = err
			startFailed = isDefiniteWakeQueueStartFailure(err)
			if child != nil {
				wait = child.wait
				kill = child.kill
			}
		} else {
			qerr = managerWakeQueue(bin, sub.ThreadID, msg)
			startFailed = isDefiniteWakeQueueStartFailure(qerr)
		}
		if startFailed {
			metrics.QueueFailures++
			if err := saveWakeInflightOrClass(root, sub.ID, sub.ThreadID, toSend, scanHigh, inflightPhaseClaimed, metrics); err != nil {
				return err
			}
			crashWakeIf("after_start_revert")
			return noteWakeClass(root, cur, metrics, "queue_start_failed")
		}
		if qerr != nil {
			metrics.QueueFailures++
			_ = saveManagerWakeInflightPhase(root, sub.ID, sub.ThreadID, toSend, scanHigh, inflightPhaseSpawned)
			if kill != nil {
				kill()
			}
			if wait != nil {
				_ = wait()
			}
			return failDeliveryUncertain(root, cur, metrics)
		}
		if err := saveManagerWakeInflightPhase(root, sub.ID, sub.ThreadID, toSend, scanHigh, inflightPhaseSpawned); err != nil {
			if kill != nil {
				kill()
			}
			if wait != nil {
				_ = wait()
			}
			return failDeliveryUncertain(root, cur, metrics)
		}
		crashWakeIf("after_spawned")
		if wait != nil {
			pending = &pendingWakeQueue{toSend: append([]managerWakeOutboxRow(nil), toSend...), delta: append([]managerWakeOutboxRow(nil), delta...), wait: wait}
			return nil
		}
		metrics.QueueSuccesses++
		if err := managerWakePersistReceipts(root, sub.ID, toSend); err != nil {
			return failDeliveryUncertain(root, cur, metrics)
		}
		markDestinationEventIDs(destSeen, sub.ThreadID, toSend)
		_ = clearManagerWakeInflight(root, sub.ID)
		return commit()
	})
	if lockErr != nil {
		return lockErr
	}
	if pending == nil {
		return nil
	}
	if err := pending.wait(); err != nil {
		metrics.QueueFailures++
		return failDeliveryUncertain(root, cur, metrics)
	}
	metrics.QueueSuccesses++
	if err := managerWakePersistReceipts(root, sub.ID, pending.toSend); err != nil {
		return failDeliveryUncertain(root, cur, metrics)
	}
	markDestinationEventIDs(destSeen, sub.ThreadID, pending.toSend)
	_ = clearManagerWakeInflight(root, sub.ID)
	cur.OutboxSeq = scanHigh
	if len(pending.delta) > 0 {
		cur.LastWakeID = pending.delta[len(pending.delta)-1].WakeEventID
	}
	cur.LastErrorClass = ""
	cur.LastDeliveryAt = time.Now().Format(time.RFC3339Nano)
	if err := managerWakeCommitCursor(root, cur); err != nil {
		return failCursorSave(root, metrics, err)
	}
	return nil
}

func secretfulWakeMessage(msg string) bool {
	low := strings.ToLower(msg)
	for _, n := range []string{"prompt=", "token=", "authorization", "api_key", "password=", "cookie=", "secret="} {
		if strings.Contains(low, n) {
			return true
		}
	}
	return false
}

func managerWakeReadback(root string, mw *ManagerWakeConfig) map[string]any {
	rows, class, _ := loadManagerWakeOutbox(root)
	var high int64
	for _, row := range rows {
		if row.Seq > high {
			high = row.Seq
		}
	}
	metrics := loadManagerWakeMetrics(root)
	subs := []map[string]any{}
	pending := 0
	if mw != nil {
		for _, sub := range mw.Subscriptions {
			cur, _, _ := loadManagerWakeCursor(root, sub.ID)
			acked := int64(0)
			lastWake := ""
			lastErr := ""
			if cur != nil {
				acked = cur.OutboxSeq
				lastWake = cur.LastWakeID
				lastErr = cur.LastErrorClass
			}
			if inflightClaimUnresolved(root, sub.ID, sub.ThreadID) {
				lastErr = "delivery_uncertain"
			}
			subPending := 0
			for _, row := range rows {
				if row.Seq <= acked {
					continue
				}
				t, _ := findTaskAnywhere(root, row.TaskID)
				rebuilt, kind := bindCommittedWakeRow(root, t, row)
				if kind == wakeBindOK && subscriptionMatches(sub, t, rebuilt) {
					subPending++
				}
			}
			pending += subPending
			subs = append(subs, map[string]any{
				"id":             sub.ID,
				"enabled":        sub.Enabled,
				"acked_seq":      acked,
				"pending":        subPending,
				"last_wake_id":   lastWake,
				"last_err_class": lastErr,
			})
		}
	}
	installed := false
	if _, err := os.Stat(managerWakeLaunchdPlistPath()); err == nil {
		installed = true
	}
	lastErr := firstNonEmpty(class, loadManagerWakeErrorClass(root), metrics.LastErrorClass)
	if unresolvedManagerWakeInflightExists(root, mw) {
		lastErr = "delivery_uncertain"
	}
	return map[string]any{
		"enabled":           managerWakeEnabled(mw),
		"outbox_high_water": high,
		"pending":           pending,
		"watchdog_sec":      managerWakeWatchdogSec,
		"last_err_class":    lastErr,
		"no_delta_scans":    metrics.NoDeltaScans,
		"queue_attempts":    metrics.QueueAttempts,
		"queue_successes":   metrics.QueueSuccesses,
		"queue_failures":    metrics.QueueFailures,
		"launchd_installed": installed,
		"subscriptions":     subs,
		"diagnosis":         diagnoseManagerWake(root, mw),
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
