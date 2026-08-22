package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	managerWakeWatchdogSec = 1200
	outboxSchemaV1         = "cardex.manager_wake.outbox.v1"
	cursorSchemaV1         = "cardex.manager_wake.cursor.v1"
	errorSchemaV1          = "cardex.manager_wake.error.v1"
	receiptSchemaV1        = "cardex.manager_wake.receipt.v1"
	inflightSchemaV1       = "cardex.manager_wake.inflight.v1"
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

// ManagerWakeConfig is the self-contained wake core config. It is not a field
// on Config: this lane cannot patch shared files. WatchdogSec is diagnostic
// only; the running policy is hard-coded to managerWakeWatchdogSec.
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
	// managerWakeCommitCursor is the post-queue ack. Tests inject a one-shot
	// save failure to prove receipts suppress a second queue spawn.
	managerWakeCommitCursor = saveManagerWakeCursor
	// managerWakePersistReceipts is the sender receipt fsync. Tests inject a
	// crash after queue success and before this write.
	managerWakePersistReceipts = rememberManagerWakeReceipts
)

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
	case evDone, evHeld, evFailed, evNeedsOwner:
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
	if row.Schema != outboxSchemaV1 || row.Seq <= 0 || row.TaskID == "" || row.TaskEventSeq <= 0 || row.TransitionID == "" {
		return false
	}
	if !wakeEligibleEventType(row.EventType) {
		return false
	}
	return row.WakeEventID == rowWakeIdentity(row)
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

func appendOutboxRow(root string, t *Task, ev TaskEvent) error {
	if t == nil || root == "" {
		return nil
	}
	return withTaskControlLock(root, "_manager-wake-outbox", func() error {
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

func loadManagerWakeCursor(root, subID string) (*managerWakeCursor, string, error) {
	path := managerWakeCursorPath(root, subID)
	if path == "" {
		return nil, "invalid_subscription_id", fmt.Errorf("invalid_subscription_id")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			id, _ := closedManagerWakeSubID(subID)
			return &managerWakeCursor{Schema: cursorSchemaV1, SubscriptionID: id}, "", nil
		}
		return nil, "cursor_unreadable", fmt.Errorf("cursor_unreadable")
	}
	var cur managerWakeCursor
	if err := json.Unmarshal(data, &cur); err != nil {
		return nil, "cursor_corrupt", fmt.Errorf("cursor_corrupt")
	}
	if cur.SubscriptionID == "" {
		id, _ := closedManagerWakeSubID(subID)
		cur.SubscriptionID = id
	}
	return &cur, "", nil
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
	var rec managerWakeReceipt
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, "receipt_corrupt", fmt.Errorf("receipt_corrupt")
	}
	for _, id := range rec.WakeEventIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			ids[id] = true
		}
	}
	return ids, "", nil
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

func loadManagerWakeInflightIDs(root, subID string) (map[string]bool, string, error) {
	path := managerWakeInflightPath(root, subID)
	if path == "" {
		return nil, "invalid_subscription_id", fmt.Errorf("invalid_subscription_id")
	}
	ids := map[string]bool{}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ids, "", nil
		}
		return nil, "inflight_unreadable", fmt.Errorf("inflight_unreadable")
	}
	var rec managerWakeInflight
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, "inflight_corrupt", fmt.Errorf("inflight_corrupt")
	}
	for _, id := range rec.WakeEventIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			ids[id] = true
		}
	}
	return ids, "", nil
}

func saveManagerWakeInflight(root, subID, threadID string, rows []managerWakeOutboxRow, highWater int64) error {
	path := managerWakeInflightPath(root, subID)
	if path == "" {
		return fmt.Errorf("invalid_subscription_id")
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
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func inflightCoversUnacked(inflight map[string]bool, rows []managerWakeOutboxRow) bool {
	for _, row := range rows {
		if inflight[row.WakeEventID] {
			return true
		}
	}
	return false
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
			inflight, class, _ := loadManagerWakeInflightIDs(root, id)
			if class != "" {
				out = append(out, class)
				continue
			}
			receipts, _, _ := loadManagerWakeReceiptIDs(root, id)
			for wid := range inflight {
				if !receipts[wid] {
					out = append(out, "delivery_uncertain")
					break
				}
			}
		}
	}
	return out
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
				continue
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

func defaultManagerWakeQueue(bin, thread, message string) error {
	if !wakeMessageCarriesEventIDs(message) {
		return fmt.Errorf("queue_idempotency_unsupported")
	}
	args := managerWakeQueueArgv(bin, thread, message)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("queue_failed")
	}
	return nil
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
	for _, sub := range mw.Subscriptions {
		if !sub.Enabled {
			continue
		}
		if err := deliverSubscription(root, sub, rows, bin, &metrics); err != nil {
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

func deliverSubscription(root string, sub ManagerWakeSubscription, rows []managerWakeOutboxRow, bin string, metrics *managerWakeMetrics) error {
	id, ok := closedManagerWakeSubID(sub.ID)
	if !ok {
		metrics.LastErrorClass = "invalid_subscription_id"
		noteManagerWakeError(root, metrics, "invalid_subscription_id")
		return nil
	}
	sub.ID = id
	if !managerWakeThreadRE.MatchString(strings.TrimSpace(sub.ThreadID)) {
		metrics.LastErrorClass = "invalid_thread_id"
		cur, class, _ := loadManagerWakeCursor(root, sub.ID)
		if class != "" {
			metrics.LastErrorClass = class
			noteManagerWakeError(root, metrics, class)
			return fmt.Errorf("%s", class)
		}
		if cur == nil {
			cur = &managerWakeCursor{SubscriptionID: sub.ID}
		}
		cur.LastErrorClass = "invalid_thread_id"
		_ = saveManagerWakeCursor(root, cur)
		noteManagerWakeError(root, metrics, "invalid_thread_id")
		return nil
	}
	for _, p := range sub.Projects {
		if strings.TrimSpace(p) != "" && !projectKnown(root, p) {
			metrics.LastErrorClass = "unknown_project"
			cur, class, _ := loadManagerWakeCursor(root, sub.ID)
			if class != "" {
				metrics.LastErrorClass = class
				noteManagerWakeError(root, metrics, class)
				return fmt.Errorf("%s", class)
			}
			if cur == nil {
				cur = &managerWakeCursor{SubscriptionID: sub.ID}
			}
			cur.LastErrorClass = "unknown_project"
			_ = saveManagerWakeCursor(root, cur)
			noteManagerWakeError(root, metrics, "unknown_project")
			return nil
		}
	}
	cur, class, err := loadManagerWakeCursor(root, sub.ID)
	if class != "" {
		metrics.LastErrorClass = class
		noteManagerWakeError(root, metrics, class)
		if err != nil {
			return err
		}
		return fmt.Errorf("%s", class)
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
		t, _ := findTaskAnywhere(root, row.TaskID)
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
	sort.Slice(delta, func(i, j int) bool { return delta[i].Seq < delta[j].Seq })
	receipts, class, err := loadManagerWakeReceiptIDs(root, sub.ID)
	if class != "" {
		metrics.LastErrorClass = class
		noteManagerWakeError(root, metrics, class)
		if err != nil {
			return err
		}
		return fmt.Errorf("%s", class)
	}
	var toSend []managerWakeOutboxRow
	for _, row := range delta {
		if receipts[row.WakeEventID] {
			continue
		}
		toSend = append(toSend, row)
	}
	inflight, class, err := loadManagerWakeInflightIDs(root, sub.ID)
	if class != "" {
		metrics.LastErrorClass = class
		noteManagerWakeError(root, metrics, class)
		if err != nil {
			return err
		}
		return fmt.Errorf("%s", class)
	}
	commit := func() error {
		cur.OutboxSeq = scanHigh
		cur.LastWakeID = delta[len(delta)-1].WakeEventID
		cur.LastErrorClass = ""
		cur.LastDeliveryAt = time.Now().Format(time.RFC3339Nano)
		if err := managerWakeCommitCursor(root, cur); err != nil {
			return failCursorSave(root, metrics, err)
		}
		_ = clearManagerWakeInflight(root, sub.ID)
		return nil
	}
	if inflightCoversUnacked(inflight, toSend) {
		return failDeliveryUncertain(root, cur, metrics)
	}
	if len(toSend) == 0 {
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
	if err := saveManagerWakeInflight(root, sub.ID, sub.ThreadID, toSend, scanHigh); err != nil {
		class := allowlistedWakeError(err, "inflight_save_failed")
		metrics.LastErrorClass = class
		noteManagerWakeError(root, metrics, class)
		return fmt.Errorf("%s", class)
	}
	metrics.QueueAttempts++
	if err := managerWakeQueue(bin, sub.ThreadID, msg); err != nil {
		_ = clearManagerWakeInflight(root, sub.ID)
		metrics.QueueFailures++
		metrics.LastErrorClass = "queue_failed"
		cur.LastErrorClass = "queue_failed"
		_ = saveManagerWakeCursor(root, cur)
		noteManagerWakeError(root, metrics, "queue_failed")
		return fmt.Errorf("queue_failed")
	}
	metrics.QueueSuccesses++
	if err := managerWakePersistReceipts(root, sub.ID, toSend); err != nil {
		return failDeliveryUncertain(root, cur, metrics)
	}
	_ = clearManagerWakeInflight(root, sub.ID)
	return commit()
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
	return map[string]any{
		"enabled":           managerWakeEnabled(mw),
		"outbox_high_water": high,
		"pending":           pending,
		"watchdog_sec":      managerWakeWatchdogSec,
		"last_err_class":    firstNonEmpty(class, loadManagerWakeErrorClass(root), metrics.LastErrorClass),
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
