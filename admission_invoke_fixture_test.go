package main

import (
	"strings"
	"testing"
)

// admitDirectInvoke is the test-only seam for legacy direct provider/review-copy
// fixtures. Production remains fail-closed: a nonempty taskID with no taskExecRoot
// and no exact reserved attempt must not Start. Tests that assert that denial
// must not call this helper.
//
// It establishes one isolated Cardex root (or reuses root), persists task, takes
// the scheduler lock, reserves an attempt, and maps taskExecRoot before invoke.
func admitDirectInvoke(t *testing.T, root string, task *Task) string {
	t.Helper()
	if task == nil || strings.TrimSpace(task.ID) == "" {
		t.Fatal("direct invoke fixture requires a nonempty task ID")
	}
	if root == "" {
		root = testRoot(t)
	}
	if !holdsSchedulerLock(root) {
		withSchedulerLock(t, root)
	}
	if strings.TrimSpace(task.Dir) == "" {
		task.Dir = t.TempDir()
	}
	if task.Status == "" {
		task.Status = statusQueued
	}
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	if err := reserveDispatchAttempt(root, task); err != nil {
		t.Fatal(err)
	}
	if task.ActiveAttemptID == "" {
		t.Fatal("expected reserved attempt")
	}
	taskExecRoot.Store(task.ID, root)
	t.Cleanup(func() { taskExecRoot.Delete(task.ID) })
	return root
}
