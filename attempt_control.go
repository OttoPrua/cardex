package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	attemptReserved = "reserved"
	attemptBound    = "bound"
	attemptExited   = "exited"
	attemptRevoked  = "revoked"

	transitionPrepared  = "prepared"
	transitionCommitted = "committed"
)

var (
	errStaleTaskWrite       = errors.New("stale task write rejected")
	errSchedulerLockLost    = errors.New("scheduler lock ownership lost")
	errNotSchedulable       = errors.New("task is not scheduling-eligible")
	errAttemptConflict      = errors.New("active attempt already admitted")
	errCustodyTimeout       = errors.New("producer custody timeout")
	errAlreadyTerminal      = errors.New("task already terminal")
	errProducerStillAlive   = errors.New("attempt producer is still alive")
	errProducerInvalidated  = errors.New("producer writes stopped after rejected CAS")
	errTransitionCrash      = errors.New("injected transition crash")
	terminalizeWaitTimeout  = 45 * time.Second
	reservedAttemptFreshFor = 30 * time.Second
)

const (
	transitionCrashAfterPrepare         = "after_prepare"
	transitionCrashAfterAttemptClose    = "after_attempt_close"
	transitionCrashAfterCommit          = "after_commit"
	transitionCrashAfterTaskProjection  = "after_task_projection"
	transitionCrashAfterEventProjection = "after_event_projection"
)

// transitionCrashAt is a test-only seam. Production callers must leave it empty.
var transitionCrashAt string

func crashTransitionIf(point string) error {
	if transitionCrashAt != "" && transitionCrashAt == point {
		return errTransitionCrash
	}
	return nil
}

type AttemptRecord struct {
	TaskID           string `json:"task_id"`
	AttemptID        string `json:"attempt_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	ControlEpoch     int64  `json:"control_epoch"`
	RunnerID         string `json:"runner_id,omitempty"`
	PID              int    `json:"pid,omitempty"`
	PGID             int    `json:"pgid,omitempty"`
	StartIdentity    string `json:"start_identity,omitempty"`
	WorkspaceLeaseID string `json:"workspace_lease_id,omitempty"`
	State            string `json:"state"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
}

type TransitionRecord struct {
	TransitionID     string `json:"transition_id"`
	TaskID           string `json:"task_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	NewRevision      int64  `json:"new_revision"`
	ControlEpoch     int64  `json:"control_epoch"`
	AttemptID        string `json:"attempt_id,omitempty"`
	EventType        string `json:"event_type"`
	Status           string `json:"status"`
	Actor            string `json:"actor,omitempty"`
	DetailDigest     string `json:"detail_digest,omitempty"`
	State            string `json:"state"`
	CreatedAt        string `json:"created_at"`
}

type transitionRequest struct {
	EventType            string
	Actor                string
	Status               string
	Step                 int
	Detail               map[string]any
	NeedsOwner           bool
	RequireSchedulerLock bool
}

var (
	taskControlMuByTask  sync.Map
	invalidatedProducers sync.Map // producerKey -> struct{}
)

func lockForTaskControl(taskID string) *sync.Mutex {
	if v, ok := taskControlMuByTask.Load(taskID); ok {
		return v.(*sync.Mutex)
	}
	mu := &sync.Mutex{}
	actual, _ := taskControlMuByTask.LoadOrStore(taskID, mu)
	return actual.(*sync.Mutex)
}

func controlDir(root string) string {
	return filepath.Join(root, "control")
}

func attemptsDir(root, taskID string) string {
	return filepath.Join(controlDir(root), "attempts", taskID)
}

func attemptPath(root, taskID, attemptID string) string {
	return filepath.Join(attemptsDir(root, taskID), attemptID+".json")
}

func transitionsDir(root, taskID string) string {
	return filepath.Join(controlDir(root), "transitions", taskID)
}

func transitionPath(root, taskID, transitionID string) string {
	return filepath.Join(transitionsDir(root, taskID), transitionID+".json")
}

func controlLockPath(root, taskID string) string {
	return filepath.Join(controlDir(root), "locks", taskID+".lock")
}

func newOpaqueID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return prefix + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return prefix + hex.EncodeToString(b)
}

func newAttemptID() string    { return newOpaqueID("at") }
func newTransitionID() string { return newOpaqueID("tr") }

func producerKey(t *Task) string {
	if t == nil {
		return ""
	}
	return t.ID + "\x00" + t.ActiveAttemptID
}

func invalidateProducer(t *Task) {
	if t == nil || t.ID == "" {
		return
	}
	invalidatedProducers.Store(producerKey(t), struct{}{})
}

func producerInvalidated(t *Task) bool {
	if t == nil || t.ID == "" {
		return false
	}
	_, ok := invalidatedProducers.Load(producerKey(t))
	return ok
}

func producerWriteStopped(err error) bool {
	return errors.Is(err, errStaleTaskWrite) ||
		errors.Is(err, errSchedulerLockLost) ||
		errors.Is(err, errNotSchedulable) ||
		errors.Is(err, errAttemptConflict) ||
		errors.Is(err, errProducerInvalidated) ||
		errors.Is(err, errAlreadyTerminal)
}

func finishIfStopped(err error) error {
	if producerWriteStopped(err) {
		return nil
	}
	return err
}

func atomicWriteSync(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func withTaskControlLock(root, taskID string, fn func() error) error {
	if taskID == "" {
		return fn()
	}
	mu := lockForTaskControl(taskID)
	mu.Lock()
	defer mu.Unlock()
	return withControlFileLock(root, taskID, fn)
}

func persistTaskCAS(root string, t *Task) error {
	if t == nil || t.ID == "" {
		return fmt.Errorf("empty task")
	}
	if producerInvalidated(t) {
		return errStaleTaskWrite
	}
	return withTaskControlLock(root, t.ID, func() error {
		return persistTaskCASLocked(root, t)
	})
}

func persistTaskCASLocked(root string, t *Task) error {
	current, err := loadTask(root, t.ID)
	if err != nil {
		if os.IsNotExist(err) {
			applyControlDefaults(t)
			if t.Revision == 0 {
				t.Revision = 1
			}
			return writeTaskFile(root, t)
		}
		return err
	}
	if current.ActiveAttemptID != "" && t.ActiveAttemptID == current.ActiveAttemptID &&
		t.effectiveControlState() == controlEligible && isActiveStatus(t.Status) &&
		!schedulerWriteAllowed(root) {
		noteStaleTaskWrite(root, current, t, errSchedulerLockLost)
		invalidateProducer(t)
		return errSchedulerLockLost
	}
	if err := assertTaskWriteAuthority(root, current, t); err != nil {
		noteStaleTaskWrite(root, current, t, err)
		invalidateProducer(t)
		return err
	}
	t.Revision = current.Revision + 1
	if t.ControlEpoch == 0 {
		t.ControlEpoch = current.ControlEpoch
	}
	applyControlDefaults(t)
	return writeTaskFile(root, t)
}

func isActiveStatus(status string) bool {
	switch status {
	case statusQueued, statusRunning, statusLimitPaused:
		return true
	default:
		return false
	}
}

func assertTaskWriteAuthority(root string, current, next *Task) error {
	if current == nil || next == nil {
		return fmt.Errorf("nil task")
	}
	if next.ControlEpoch < current.ControlEpoch {
		return errStaleTaskWrite
	}
	if next.ControlEpoch > current.ControlEpoch+1 {
		return errStaleTaskWrite
	}
	if next.Revision != current.Revision {
		return errStaleTaskWrite
	}
	epochBump := next.ControlEpoch == current.ControlEpoch+1
	if epochBump {
		if next.schedulingAllowed() {
			switch current.Status {
			case statusHeld, statusDone, statusFailed, statusCanceled, statusLimitPaused:
			default:
				return errStaleTaskWrite
			}
			if next.Status != statusQueued {
				return errStaleTaskWrite
			}
			return nil
		}
		if next.Status == statusDone || next.Status == statusFailed {
			return errStaleTaskWrite
		}
		return nil
	}
	curControl := current.effectiveControlState()
	if curControl == controlRevoking || curControl == controlTerminal {
		sameRevokingAttention := curControl == controlRevoking &&
			next.effectiveControlState() == controlRevoking && next.Status == current.Status
		if isActiveStatus(next.Status) && !sameRevokingAttention {
			return errStaleTaskWrite
		}
		if next.Status == statusDone && current.Status != statusDone && current.Status != statusHeld {
			return errStaleTaskWrite
		}
		if current.Status == statusHeld && next.Status == statusFailed {
			return errStaleTaskWrite
		}
		if current.Status == statusCanceled && next.Status != statusCanceled {
			return errStaleTaskWrite
		}
	}
	controlWrite := next.effectiveControlState() == controlRevoking || next.effectiveControlState() == controlTerminal ||
		curControl == controlRevoking || curControl == controlTerminal
	if current.ActiveAttemptID != "" && next.ActiveAttemptID != current.ActiveAttemptID {
		clearing := next.ActiveAttemptID == "" && controlWrite
		replacingDead := false
		if next.ActiveAttemptID != "" {
			if live, _ := liveAttempt(root, current); !live {
				replacingDead = true
			}
		}
		if !clearing && !replacingDead {
			return errStaleTaskWrite
		}
	}
	if !current.schedulingAllowed() && next.schedulingAllowed() {
		return errStaleTaskWrite
	}
	if curControl == controlTerminal && next.effectiveControlState() != controlTerminal && !epochBump {
		return errStaleTaskWrite
	}
	return nil
}

func noteStaleTaskWrite(root string, current, next *Task, cause error) {
	if current == nil {
		return
	}
	attemptID := current.ActiveAttemptID
	if attemptID == "" && next != nil {
		attemptID = next.ActiveAttemptID
	}
	if hasStaleWriteEvent(root, current.ID, attemptID) {
		return
	}
	detail := map[string]any{
		"reason_class": "stale_attempt_write",
		"cause":        cause.Error(),
	}
	if attemptID != "" {
		detail["attempt_id"] = attemptID
	}
	if current.LastCommittedTransitionID != "" {
		detail["last_committed_transition_id"] = current.LastCommittedTransitionID
	}
	ev := TaskEvent{
		Type:      evStaleAttemptWrite,
		Actor:     "control",
		Status:    current.Status,
		Step:      current.Step,
		Detail:    detail,
		AttemptID: attemptID,
		Revision:  current.Revision,
	}
	if err := recordEvent(root, current.ID, ev); err != nil {
		fmt.Fprintf(os.Stderr, "警告: stale write 诊断写入失败 %s: %v\n", current.ID, err)
	}
}

func hasStaleWriteEvent(root, taskID, attemptID string) bool {
	events, _, err := loadTaskEvents(root, taskID)
	if err != nil {
		return false
	}
	for _, ev := range events {
		if ev.Type != evStaleAttemptWrite {
			continue
		}
		if attemptID == "" || ev.AttemptID == attemptID {
			return true
		}
	}
	return false
}

func writeAttempt(root string, rec *AttemptRecord) error {
	if rec == nil || rec.TaskID == "" || rec.AttemptID == "" {
		return fmt.Errorf("empty attempt record")
	}
	rec.UpdatedAt = time.Now().Format(time.RFC3339Nano)
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteSync(attemptPath(root, rec.TaskID, rec.AttemptID), append(data, '\n'))
}

func loadAttempt(root, taskID, attemptID string) (*AttemptRecord, error) {
	data, err := os.ReadFile(attemptPath(root, taskID, attemptID))
	if err != nil {
		return nil, err
	}
	var rec AttemptRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

func writeTransition(root string, rec *TransitionRecord) error {
	if rec == nil || rec.TaskID == "" || rec.TransitionID == "" {
		return fmt.Errorf("empty transition record")
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteSync(transitionPath(root, rec.TaskID, rec.TransitionID), append(data, '\n'))
}

func loadTransition(root, taskID, transitionID string) (*TransitionRecord, error) {
	data, err := os.ReadFile(transitionPath(root, taskID, transitionID))
	if err != nil {
		return nil, err
	}
	var rec TransitionRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

func digestDetail(detail map[string]any) string {
	if len(detail) == 0 {
		return ""
	}
	safe := map[string]any{}
	for k, v := range detail {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "prompt") || strings.Contains(lk, "token") ||
			strings.Contains(lk, "secret") || strings.Contains(lk, "credential") ||
			strings.Contains(lk, "password") || lk == "err" || lk == "error" ||
			strings.Contains(lk, "output") || strings.Contains(lk, "result") {
			continue
		}
		safe[k] = v
	}
	data, err := json.Marshal(safe)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

func commitTaskTransition(root string, t *Task, req transitionRequest) error {
	if t == nil || t.ID == "" {
		return fmt.Errorf("empty task")
	}
	if producerInvalidated(t) && req.RequireSchedulerLock {
		return errStaleTaskWrite
	}
	return withTaskControlLock(root, t.ID, func() error {
		return commitTaskTransitionLocked(root, t, req)
	})
}

func commitTaskTransitionLocked(root string, t *Task, req transitionRequest) error {
	if err := recoverIncompleteTerminalLocked(root, t.ID); err != nil {
		return err
	}
	current, err := loadTask(root, t.ID)
	if err != nil {
		return err
	}
	if req.NeedsOwner || req.EventType == evNeedsOwner {
		t.Revision = current.Revision
		t.ControlEpoch = current.ControlEpoch
		t.Status = current.Status
		t.ControlState = current.ControlState
		t.SchedulingEligible = current.SchedulingEligible
		t.ActiveAttemptID = current.ActiveAttemptID
		t.LastCommittedTransitionID = current.LastCommittedTransitionID
		t.Step = current.Step
		req.Status = current.Status
		req.Step = current.Step
	}
	if req.RequireSchedulerLock {
		if !schedulerWriteAllowed(root) {
			noteStaleTaskWrite(root, current, t, errSchedulerLockLost)
			invalidateProducer(t)
			return errSchedulerLockLost
		}
	}
	if req.Status == "" {
		req.Status = t.Status
	}
	closeAttempt, terminal, clearID := transitionAttemptDisposition(req)
	if terminal && req.EventType != evNeedsOwner &&
		current.effectiveControlState() == controlTerminal && current.Status == req.Status &&
		current.LastCommittedTransitionID != "" &&
		transitionDurablyCommitted(root, current.ID, current.LastCommittedTransitionID) {
		last, lerr := loadTransition(root, current.ID, current.LastCommittedTransitionID)
		if lerr == nil && last != nil && last.EventType == req.EventType && last.Status == req.Status {
			*t = *current
			return nil
		}
	}
	if err := assertTaskWriteAuthority(root, current, t); err != nil {
		noteStaleTaskWrite(root, current, t, err)
		if req.RequireSchedulerLock {
			invalidateProducer(t)
		}
		return err
	}
	closingAttemptID := t.ActiveAttemptID
	if closingAttemptID == "" {
		closingAttemptID = current.ActiveAttemptID
	}

	var rec *AttemptRecord
	if closingAttemptID != "" {
		rec, _ = loadAttempt(root, t.ID, closingAttemptID)
	}
	if !terminal && closeAttempt && req.EventType != evNeedsOwner && !producerGone(t, rec) {
		return errProducerStillAlive
	}

	tid := newTransitionID()
	newRev := current.Revision + 1
	prevTID := t.LastCommittedTransitionID
	prevRev := t.Revision
	prevAttempt := t.ActiveAttemptID
	prevControl := t.ControlState
	prevSched := t.SchedulingEligible
	prevStatus := t.Status

	if t.ControlEpoch == 0 {
		t.ControlEpoch = current.ControlEpoch
	}
	journal := &TransitionRecord{
		TransitionID:     tid,
		TaskID:           t.ID,
		ExpectedRevision: current.Revision,
		NewRevision:      newRev,
		ControlEpoch:     t.ControlEpoch,
		AttemptID:        closingAttemptID,
		EventType:        req.EventType,
		Status:           req.Status,
		Actor:            req.Actor,
		DetailDigest:     digestDetail(req.Detail),
		State:            transitionPrepared,
		CreatedAt:        time.Now().Format(time.RFC3339Nano),
	}
	if err := writeTransition(root, journal); err != nil {
		return err
	}

	if terminal {
		return commitPreparedTerminalLocked(root, t, req, journal, rec, closingAttemptID, prevRev, prevTID, prevAttempt, prevControl, prevSched, prevStatus)
	}

	if clearID {
		t.ActiveAttemptID = ""
	}
	t.Revision = newRev
	t.LastCommittedTransitionID = tid
	applyControlDefaults(t)
	t.touch()
	if err := writeTaskFile(root, t); err != nil {
		t.Revision = prevRev
		t.LastCommittedTransitionID = prevTID
		t.ActiveAttemptID = prevAttempt
		t.ControlState = prevControl
		t.SchedulingEligible = prevSched
		t.Status = prevStatus
		_ = os.Remove(transitionPath(root, t.ID, tid))
		return err
	}

	ev := TaskEvent{
		Type:         req.EventType,
		Actor:        req.Actor,
		Status:       req.Status,
		Step:         req.Step,
		Detail:       req.Detail,
		TransitionID: tid,
		Revision:     newRev,
		AttemptID:    closingAttemptID,
	}
	if err := recordEvent(root, t.ID, ev); err != nil {
		fmt.Fprintf(os.Stderr, "警告: 事件账本写入失败 %s(%s): %v\n", t.ID, req.EventType, err)
		return err
	}
	journal.State = transitionCommitted
	if err := writeTransition(root, journal); err != nil {
		return err
	}
	if closeAttempt && closingAttemptID != "" {
		closeAttemptForDisposition(root, t.ID, closingAttemptID, rec, false)
	}
	return nil
}

func commitPreparedTerminalLocked(root string, t *Task, req transitionRequest, journal *TransitionRecord, rec *AttemptRecord, closingAttemptID string, prevRev int64, prevTID, prevAttempt, prevControl string, prevSched *bool, prevStatus string) error {
	if err := crashTransitionIf(transitionCrashAfterPrepare); err != nil {
		return err
	}
	if closingAttemptID != "" {
		if fresh, err := loadAttempt(root, t.ID, closingAttemptID); err == nil && fresh != nil {
			rec = fresh
		}
	}
	if req.EventType != evNeedsOwner && !producerGone(t, rec) {
		return errProducerStillAlive
	}
	if closingAttemptID != "" {
		closeAttemptForDisposition(root, t.ID, closingAttemptID, rec, true)
	}
	if err := crashTransitionIf(transitionCrashAfterAttemptClose); err != nil {
		return err
	}
	journal.State = transitionCommitted
	if err := writeTransition(root, journal); err != nil {
		return err
	}
	if err := crashTransitionIf(transitionCrashAfterCommit); err != nil {
		return err
	}
	return projectCommittedTerminalLocked(root, t, req, journal, closingAttemptID, prevRev, prevTID, prevAttempt, prevControl, prevSched, prevStatus)
}

func projectCommittedTerminalLocked(root string, t *Task, req transitionRequest, journal *TransitionRecord, closingAttemptID string, prevRev int64, prevTID, prevAttempt, prevControl string, prevSched *bool, prevStatus string) error {
	if !transitionDurablyCommitted(root, t.ID, journal.TransitionID) {
		return fmt.Errorf("refusing to project uncommitted transition %s", journal.TransitionID)
	}
	markControlTerminal(t)
	t.Revision = journal.NewRevision
	t.LastCommittedTransitionID = journal.TransitionID
	applyControlDefaults(t)
	t.touch()
	if err := writeTaskFile(root, t); err != nil {
		t.Revision = prevRev
		t.LastCommittedTransitionID = prevTID
		t.ActiveAttemptID = prevAttempt
		t.ControlState = prevControl
		t.SchedulingEligible = prevSched
		t.Status = prevStatus
		return err
	}
	if err := crashTransitionIf(transitionCrashAfterTaskProjection); err != nil {
		return err
	}
	if err := recordLiveTransitionEventOnce(root, t, req, journal, closingAttemptID); err != nil {
		return err
	}
	return crashTransitionIf(transitionCrashAfterEventProjection)
}

func recordLiveTransitionEventOnce(root string, t *Task, req transitionRequest, journal *TransitionRecord, closingAttemptID string) error {
	if hasTransitionEvent(root, t.ID, journal.TransitionID) {
		return nil
	}
	ev := TaskEvent{
		Type:         req.EventType,
		Actor:        req.Actor,
		Status:       req.Status,
		Step:         req.Step,
		Detail:       req.Detail,
		TransitionID: journal.TransitionID,
		Revision:     journal.NewRevision,
		AttemptID:    closingAttemptID,
	}
	if err := recordEvent(root, t.ID, ev); err != nil {
		fmt.Fprintf(os.Stderr, "警告: 事件账本写入失败 %s(%s): %v\n", t.ID, req.EventType, err)
		return err
	}
	return nil
}

func closeAttemptForDisposition(root, taskID, attemptID string, rec *AttemptRecord, terminal bool) {
	if taskID == "" || attemptID == "" {
		return
	}
	state := attemptExited
	if rec != nil && rec.State == attemptRevoked && !terminal {
		state = attemptRevoked
	}
	closeAttemptRecord(root, taskID, attemptID, state)
}

// persistTaskEvent is the runner-facing CAS path. A rejected terminal CAS is a
// hard stop: the error is returned so callers cannot continue into
// noteTaskDoneLogged, postComplete, progress, child cards, or retry.
func persistTaskEvent(root string, t *Task, evType, actor, status string, step int, detail map[string]any) error {
	if t == nil || t.ID == "" {
		return fmt.Errorf("empty task")
	}
	if producerInvalidated(t) {
		return errStaleTaskWrite
	}
	if status == "" {
		status = t.Status
	}
	req := transitionRequest{
		EventType:            evType,
		Actor:                actor,
		Status:               status,
		Step:                 step,
		Detail:               detail,
		RequireSchedulerLock: true,
	}
	closeAttempt, terminal, _ := transitionAttemptDisposition(req)
	if (closeAttempt || terminal) && evType != evNeedsOwner {
		var rec *AttemptRecord
		if t.ActiveAttemptID != "" {
			rec, _ = loadAttempt(root, t.ID, t.ActiveAttemptID)
		}
		if waitErr := waitProducerExit(root, t, rec, terminalizeWaitTimeout); waitErr != nil {
			if errors.Is(waitErr, errCustodyTimeout) {
				if fresh, loadErr := loadTask(root, t.ID); loadErr == nil {
					*t = *fresh
				}
				_ = commitTaskTransition(root, t, transitionRequest{
					EventType:            evNeedsOwner,
					Actor:                actor,
					Status:               t.Status,
					Step:                 t.Step,
					Detail:               withCostTelemetry(map[string]any{"reason": "custody_timeout", "reason_class": "needs_owner"}, t),
					NeedsOwner:           true,
					RequireSchedulerLock: true,
				})
			}
			invalidateProducer(t)
			return waitErr
		}
	}
	err := commitTaskTransition(root, t, req)
	if producerWriteStopped(err) {
		invalidateProducer(t)
	}
	return err
}

func transitionAttemptDisposition(req transitionRequest) (closeAttempt, terminal, clearID bool) {
	if req.EventType == evNeedsOwner {
		return false, false, false
	}
	switch req.Status {
	case statusDone, statusFailed, statusHeld, statusCanceled:
		return true, true, true
	case statusLimitPaused:
		return true, false, true
	case statusQueued:
		if req.EventType == evRetry {
			return true, false, true
		}
	}
	switch req.EventType {
	case evRetry, evLimitPaused:
		return true, false, true
	}
	return false, false, false
}

func closeAttemptRecord(root, taskID, attemptID, state string) {
	if taskID == "" || attemptID == "" {
		return
	}
	rec, err := loadAttempt(root, taskID, attemptID)
	if err != nil || rec == nil {
		return
	}
	if rec.State == state {
		return
	}
	if rec.State == attemptExited && state != attemptExited && state != attemptRevoked {
		return
	}
	rec.State = state
	_ = writeAttempt(root, rec)
}

func transitionDurablyCommitted(root, taskID, transitionID string) bool {
	if taskID == "" || transitionID == "" {
		return false
	}
	rec, err := loadTransition(root, taskID, transitionID)
	return err == nil && rec != nil && rec.State == transitionCommitted
}

func listTaskTransitions(root, taskID string) ([]*TransitionRecord, error) {
	entries, err := os.ReadDir(transitionsDir(root, taskID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*TransitionRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		rec, err := loadTransition(root, taskID, strings.TrimSuffix(e.Name(), ".json"))
		if err != nil || rec == nil {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

func reserveDispatchAttempt(root string, t *Task) error {
	if t == nil || t.ID == "" {
		return fmt.Errorf("empty task")
	}
	if producerInvalidated(t) {
		return errStaleTaskWrite
	}
	return withTaskControlLock(root, t.ID, func() error {
		if !schedulerWriteAllowed(root) {
			return errSchedulerLockLost
		}
		current, err := loadTask(root, t.ID)
		if err != nil {
			return err
		}
		if current.Revision != t.Revision {
			return errStaleTaskWrite
		}
		if !current.schedulingAllowed() || current.effectiveControlState() != controlEligible {
			return errNotSchedulable
		}
		if live, _ := liveAttempt(root, current); live {
			return errAttemptConflict
		}
		if current.ActiveAttemptID != "" {
			closeAttemptRecord(root, current.ID, current.ActiveAttemptID, attemptExited)
		}
		id := newAttemptID()
		now := time.Now().Format(time.RFC3339Nano)
		rec := &AttemptRecord{
			TaskID:           current.ID,
			AttemptID:        id,
			ExpectedRevision: current.Revision,
			ControlEpoch:     current.ControlEpoch,
			RunnerID:         currentRunnerID(),
			State:            attemptReserved,
			CreatedAt:        now,
			UpdatedAt:        now,
			WorkspaceLeaseID: canonicalWorkspaceID(current.Dir),
		}
		if err := writeAttempt(root, rec); err != nil {
			return err
		}
		t.ActiveAttemptID = id
		t.ControlEpoch = current.ControlEpoch
		t.Revision = current.Revision
		t.ControlState = controlEligible
		if t.SchedulingEligible == nil {
			t.SchedulingEligible = boolPtr(true)
		}
		return persistTaskCASLocked(root, t)
	})
}

func saveAuthorizedTask(root string, t *Task) error {
	if producerInvalidated(t) {
		return errStaleTaskWrite
	}
	if !schedulerWriteAllowed(root) {
		invalidateProducer(t)
		return errSchedulerLockLost
	}
	return saveTask(root, t)
}

func followOnWritesAllowed(root string, t *Task) bool {
	return t != nil && !producerInvalidated(t) && !diskControlRevoked(root, t.ID) && schedulerWriteAllowed(root)
}

func currentRunnerID() string {
	if id, ok := processStartIdentity(os.Getpid()); ok && id != "" {
		return id
	}
	return fmt.Sprintf("pid:%d", os.Getpid())
}

func liveAttempt(root string, t *Task) (bool, *AttemptRecord) {
	if t == nil || t.ActiveAttemptID == "" {
		return false, nil
	}
	rec, err := loadAttempt(root, t.ID, t.ActiveAttemptID)
	if err != nil {
		return false, nil
	}
	if rec.State == attemptRevoked || rec.State == attemptExited {
		return attemptProducerAlive(rec), rec
	}
	if attemptProducerAlive(rec) {
		return true, rec
	}
	if rec.State == attemptReserved {
		if rec.PID > 0 {
			return attemptProducerAlive(rec), rec
		}
		if rec.RunnerID != "" && rec.RunnerID == currentRunnerID() {
			if ts, err := time.Parse(time.RFC3339Nano, rec.CreatedAt); err == nil && time.Since(ts) < reservedAttemptFreshFor {
				return true, rec
			}
		}
		return false, rec
	}
	return false, rec
}

func attemptProducerAlive(rec *AttemptRecord) bool {
	if rec == nil || rec.PID <= 0 {
		return false
	}
	return verifyAttemptProcess(rec)
}

func bindAttemptProcess(root, taskID string, pid int) {
	if root == "" || taskID == "" || pid <= 0 {
		return
	}
	_ = withTaskControlLock(root, taskID, func() error {
		t, err := loadTask(root, taskID)
		if err != nil || t.ActiveAttemptID == "" {
			return nil
		}
		rec, err := loadAttempt(root, taskID, t.ActiveAttemptID)
		if err != nil {
			return nil
		}
		start, ok := processStartIdentity(pid)
		if !ok {
			return nil
		}
		pgid := attemptProcessPGID(pid)
		if rec.State == attemptBound && rec.PID > 0 {
			if rec.PID != pid || rec.StartIdentity != start || rec.PGID != pgid {
				return nil
			}
		}
		ws := canonicalWorkspaceID(t.Dir)
		if rec.WorkspaceLeaseID != "" && rec.WorkspaceLeaseID != ws {
			return nil
		}
		rec.PID = pid
		rec.PGID = pgid
		rec.StartIdentity = start
		rec.State = attemptBound
		rec.WorkspaceLeaseID = ws
		return writeAttempt(root, rec)
	})
}

func diskControlRevoked(root, id string) bool {
	t, err := loadTask(root, id)
	if err != nil {
		return os.IsNotExist(err)
	}
	if t.Status == statusCanceled || t.Status == statusHeld {
		return true
	}
	return t.effectiveControlState() == controlRevoking
}

func workspaceLeaseHeld(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	if _, err := os.Stat(dir); err != nil {
		return false
	}
	return workspaceProcessResidue(dir)
}

func producerGone(t *Task, rec *AttemptRecord) bool {
	if attemptProducerAlive(rec) {
		return false
	}
	if t != nil {
		if anyTaskProcAlive(t.ID) || taskProcessResidue(t.ID) {
			return false
		}
		if policyFallbackProcessProofSupported() && workspaceLeaseHeld(t.Dir) {
			return false
		}
	}
	return true
}

func verifyAndSignalAttempt(rec *AttemptRecord) {
	if rec == nil {
		return
	}
	if rec.PID > 0 && verifyAttemptProcess(rec) {
		if rec.PGID > 0 {
			_ = killProcGroup(rec.PGID)
		}
		if p, err := os.FindProcess(rec.PID); err == nil {
			_ = p.Kill()
		}
		return
	}
	if rec.TaskID != "" {
		signalRegisteredTaskProcs(rec.TaskID)
	}
}

func waitProducerExit(root string, t *Task, rec *AttemptRecord, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = terminalizeWaitTimeout
	}
	deadline := time.Now().Add(timeout)
	signaled := false
	for {
		if rec != nil && rec.AttemptID != "" && t != nil && root != "" {
			if fresh, err := loadAttempt(root, t.ID, rec.AttemptID); err == nil && fresh != nil {
				rec = fresh
			}
		}
		if producerGone(t, rec) {
			return nil
		}
		if time.Now().After(deadline) {
			return errCustodyTimeout
		}
		if !signaled {
			verifyAndSignalAttempt(rec)
			if t != nil {
				signalRegisteredTaskProcs(t.ID)
			}
			signaled = true
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// terminalize revokes scheduling and the exact attempt lease first, then
// establishes producer exit, then commits terminal state. Repeated calls
// for the same status are idempotent.
func terminalize(root string, id, status, actor, reason string, detail map[string]any) error {
	if status != statusHeld && status != statusCanceled {
		return fmt.Errorf("terminalize 仅支持 held/canceled，得到 %s", status)
	}
	t, err := loadTask(root, id)
	if err != nil {
		return err
	}
	if t.effectiveControlState() == controlTerminal && t.Status == status {
		return nil
	}
	if detail == nil {
		detail = map[string]any{}
	}
	if reason != "" {
		if _, ok := detail["reason"]; !ok {
			detail["reason"] = reason
		}
	}
	detail = withCostTelemetry(detail, t)

	var rec *AttemptRecord
	err = withTaskControlLock(root, t.ID, func() error {
		current, err := loadTask(root, t.ID)
		if err != nil {
			return err
		}
		t = current
		if t.effectiveControlState() == controlTerminal && t.Status == status {
			return errAlreadyTerminal
		}
		if t.ActiveAttemptID != "" {
			rec, _ = loadAttempt(root, t.ID, t.ActiveAttemptID)
			if rec != nil {
				rec.State = attemptRevoked
				_ = writeAttempt(root, rec)
			}
		}
		revokeScheduling(t)
		t.touch()
		return persistTaskCASLocked(root, t)
	})
	if errors.Is(err, errAlreadyTerminal) {
		return nil
	}
	if err != nil {
		return err
	}

	verifyAndSignalAttempt(rec)
	if waitErr := waitProducerExit(root, t, rec, terminalizeWaitTimeout); waitErr != nil {
		if fresh, loadErr := loadTask(root, t.ID); loadErr == nil {
			t = fresh
		}
		needs := withCostTelemetry(map[string]any{
			"reason":       "custody_timeout",
			"reason_class": "needs_owner",
		}, t)
		if attErr := commitTaskTransition(root, t, transitionRequest{
			EventType:  evNeedsOwner,
			Actor:      actor,
			Status:     t.Status,
			Step:       t.Step,
			Detail:     needs,
			NeedsOwner: true,
		}); attErr != nil {
			fmt.Fprintf(os.Stderr, "警告: needs_owner 落盘失败 %s: %v\n", t.ID, attErr)
		}
		return waitErr
	}

	fresh, err := loadTask(root, t.ID)
	if err != nil {
		return err
	}
	t = fresh
	if t.effectiveControlState() == controlTerminal && t.Status == status {
		return nil
	}
	t.Status = status
	markControlTerminal(t)
	evType := evHeld
	if status == statusCanceled {
		evType = evCanceled
	}
	return commitTaskTransition(root, t, transitionRequest{
		EventType: evType,
		Actor:     actor,
		Status:    status,
		Step:      t.Step,
		Detail:    withCostTelemetry(detail, t),
	})
}

func reconcileControlPlane(root string) {
	reconcilePreparedTransitions(root)
}

func reconcilePreparedTransitions(root string) {
	base := filepath.Join(controlDir(root), "transitions")
	taskDirs, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, td := range taskDirs {
		if !td.IsDir() {
			continue
		}
		_ = withTaskControlLock(root, td.Name(), func() error {
			return recoverTaskTransitionsLocked(root, td.Name())
		})
	}
}

func recoverTaskTransitionsLocked(root, taskID string) error {
	return recoverTaskTransitionsLockedFiltered(root, taskID, false)
}

func recoverIncompleteTerminalLocked(root, taskID string) error {
	return recoverTaskTransitionsLockedFiltered(root, taskID, true)
}

func recoverTaskTransitionsLockedFiltered(root, taskID string, terminalOnly bool) error {
	recs, err := listTaskTransitions(root, taskID)
	if err != nil {
		return err
	}
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].CreatedAt != recs[j].CreatedAt {
			return recs[i].CreatedAt < recs[j].CreatedAt
		}
		return recs[i].TransitionID < recs[j].TransitionID
	})
	for _, rec := range recs {
		if rec == nil {
			continue
		}
		if terminalOnly && !isTerminalTransitionStatus(rec.Status) {
			continue
		}
		if err := finishTransitionRecordLocked(root, rec); err != nil {
			return err
		}
	}
	return nil
}

func finishTransitionRecordLocked(root string, rec *TransitionRecord) error {
	if rec == nil {
		return nil
	}
	if !isTerminalTransitionStatus(rec.Status) {
		if rec.State == transitionCommitted {
			if transitionRecordClosesAttempt(rec) {
				ensureAttemptClosedForTransition(root, rec)
			}
			return nil
		}
		finishPreparedNonterminal(root, rec)
		return nil
	}
	t, err := loadTaskOrArchived(root, rec.TaskID)
	if err != nil || t == nil {
		return nil
	}
	if terminalTransitionSuperseded(t, rec) {
		return nil
	}
	if rec.State != transitionCommitted {
		var att *AttemptRecord
		if rec.AttemptID != "" {
			att, _ = loadAttempt(root, rec.TaskID, rec.AttemptID)
		}
		if rec.AttemptID != "" && !producerGone(t, att) {
			return nil
		}
		closeAttemptForTransitionRecord(root, rec)
		rec.State = transitionCommitted
		if err := writeTransition(root, rec); err != nil {
			return err
		}
	}
	return projectCommittedTerminalFromRecordLocked(root, rec)
}

func projectCommittedTerminalFromRecordLocked(root string, rec *TransitionRecord) error {
	if !transitionDurablyCommitted(root, rec.TaskID, rec.TransitionID) {
		return nil
	}
	t, err := loadTaskOrArchived(root, rec.TaskID)
	if err != nil || t == nil {
		return nil
	}
	if terminalTransitionSuperseded(t, rec) {
		return nil
	}
	if t.LastCommittedTransitionID != rec.TransitionID {
		if t.Revision != rec.ExpectedRevision {
			return nil
		}
		t.Status = rec.Status
		markControlTerminal(t)
		if rec.ControlEpoch != 0 {
			t.ControlEpoch = rec.ControlEpoch
		}
		t.Revision = rec.NewRevision
		t.LastCommittedTransitionID = rec.TransitionID
		applyControlDefaults(t)
		t.touch()
		if err := writeTaskFile(root, t); err != nil {
			return err
		}
	} else if t.ActiveAttemptID != "" {
		markControlTerminal(t)
		applyControlDefaults(t)
		t.touch()
		if err := writeTaskFile(root, t); err != nil {
			return err
		}
	}
	if err := recordRecoveredTransitionEvent(root, rec, t); err != nil {
		return err
	}
	closeAttemptForTransitionRecord(root, rec)
	return nil
}

func finishPreparedNonterminal(root string, rec *TransitionRecord) {
	t, err := loadTaskOrArchived(root, rec.TaskID)
	if err != nil || t == nil {
		return
	}
	if t.LastCommittedTransitionID != rec.TransitionID {
		return
	}
	if !hasTransitionEvent(root, rec.TaskID, rec.TransitionID) {
		ev := TaskEvent{
			Type:         rec.EventType,
			Actor:        rec.Actor,
			Status:       rec.Status,
			Step:         t.Step,
			TransitionID: rec.TransitionID,
			Revision:     rec.NewRevision,
			AttemptID:    rec.AttemptID,
			Detail: map[string]any{
				"reason":       "journal_replay",
				"reason_class": closedReasonClass(rec.Actor, rec.EventType, nil),
			},
		}
		if err := recordEvent(root, rec.TaskID, ev); err != nil {
			return
		}
	}
	rec.State = transitionCommitted
	if err := writeTransition(root, rec); err != nil {
		return
	}
	ensureAttemptClosedForTransition(root, rec)
}

func finishPreparedTransition(root string, rec *TransitionRecord) {
	_ = finishTransitionRecordLocked(root, rec)
}

func ensureAttemptClosedForTransition(root string, rec *TransitionRecord) {
	if rec == nil || rec.AttemptID == "" {
		return
	}
	t, err := loadTaskOrArchived(root, rec.TaskID)
	if err != nil || t == nil {
		return
	}
	if t.LastCommittedTransitionID != rec.TransitionID {
		return
	}
	closeAttemptForTransitionRecord(root, rec)
}

func closeAttemptForTransitionRecord(root string, rec *TransitionRecord) {
	if rec == nil || rec.AttemptID == "" {
		return
	}
	state := attemptExited
	if loaded, lerr := loadAttempt(root, rec.TaskID, rec.AttemptID); lerr == nil && loaded != nil && loaded.State == attemptRevoked {
		if !isTerminalTransitionStatus(rec.Status) {
			state = attemptRevoked
		}
	}
	closeAttemptRecord(root, rec.TaskID, rec.AttemptID, state)
}

func loadTaskOrArchived(root, taskID string) (*Task, error) {
	t, err := loadTask(root, taskID)
	if err == nil {
		return t, nil
	}
	if archived, aerr := findTaskAnywhere(root, taskID); aerr == nil {
		return archived, nil
	}
	return nil, err
}

func isTerminalTransitionStatus(status string) bool {
	switch status {
	case statusDone, statusFailed, statusHeld, statusCanceled:
		return true
	default:
		return false
	}
}

func transitionRecordClosesAttempt(rec *TransitionRecord) bool {
	if rec == nil {
		return false
	}
	closeAttempt, _, _ := transitionAttemptDisposition(transitionRequest{
		EventType: rec.EventType,
		Status:    rec.Status,
	})
	return closeAttempt
}

func terminalTransitionSuperseded(t *Task, rec *TransitionRecord) bool {
	if t == nil || rec == nil {
		return true
	}
	if t.LastCommittedTransitionID == rec.TransitionID {
		// Retry/release can leave the historical terminal journal id in place while the
		// task is no longer that terminal. Only keep recovering while the task still
		// sits on this committed terminal.
		return t.Status != rec.Status || t.effectiveControlState() != controlTerminal
	}
	return t.Revision != rec.ExpectedRevision
}

func hasTransitionEvent(root, taskID, transitionID string) bool {
	if taskID == "" || transitionID == "" {
		return false
	}
	events, _, err := loadTaskEvents(root, taskID)
	if err != nil {
		return false
	}
	for _, ev := range events {
		if ev.TransitionID == transitionID {
			return true
		}
	}
	return false
}

func recordRecoveredTransitionEvent(root string, rec *TransitionRecord, t *Task) error {
	if hasTransitionEvent(root, rec.TaskID, rec.TransitionID) {
		return nil
	}
	step := 0
	if t != nil {
		step = t.Step
	}
	ev := TaskEvent{
		Type:         rec.EventType,
		Actor:        rec.Actor,
		Status:       rec.Status,
		Step:         step,
		TransitionID: rec.TransitionID,
		Revision:     rec.NewRevision,
		AttemptID:    rec.AttemptID,
		Detail: map[string]any{
			"reason":       "journal_replay",
			"reason_class": closedReasonClass(rec.Actor, rec.EventType, nil),
		},
	}
	if rec.EventType == evDone || rec.EventType == evFailed || rec.EventType == evHeld || rec.EventType == evCanceled {
		ev.Detail = withCostTelemetry(ev.Detail, t)
	}
	return recordEvent(root, rec.TaskID, ev)
}

func closedReasonClass(actor, evType string, detail map[string]any) string {
	if detail != nil {
		if c, ok := detail["reason_class"].(string); ok && strings.TrimSpace(c) != "" {
			return strings.TrimSpace(c)
		}
		if r, ok := detail["reason"].(string); ok {
			switch strings.TrimSpace(r) {
			case "cli hold", "add -hold":
				return "cli_hold"
			case "custody_timeout":
				return "needs_owner"
			case "runner_cancel", "backfill":
				return "canceled"
			}
		}
	}
	switch evType {
	case evDone:
		return "done"
	case evHeld:
		if strings.HasPrefix(actor, "cli:") {
			return "cli_hold"
		}
		return "held"
	case evFailed:
		return "failed"
	case evNeedsOwner:
		return "needs_owner"
	case evCanceled:
		return "canceled"
	default:
		return evType
	}
}
