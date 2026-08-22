//go:build !windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func reservedTaskExec(t *testing.T, workspace string) (root, taskID string) {
	t.Helper()
	root = testRoot(t)
	withSchedulerLock(t, root)
	if workspace == "" {
		workspace = t.TempDir()
	}
	tk := queuedSequence(t, root, testCfg(), "reserved exec", workspace)
	if err := reserveDispatchAttempt(root, tk); err != nil {
		t.Fatal(err)
	}
	if tk.ActiveAttemptID == "" {
		t.Fatal("expected reserved attempt")
	}
	taskExecRoot.Store(tk.ID, root)
	t.Cleanup(func() { taskExecRoot.Delete(tk.ID) })
	return root, tk.ID
}

func TestInheritedProcessLeaseDetectsSetsidDescendant(t *testing.T) {
	if os.Getenv("CARDEX_TEST_SETSID_LEASE_CHILD") == "1" {
		if _, err := syscall.Setsid(); err != nil && !errors.Is(err, syscall.EPERM) {
			os.Exit(2)
		}
		time.Sleep(700 * time.Millisecond)
		os.Exit(0)
	}

	_, taskID := reservedTaskExec(t, "")
	cmd := exec.CommandContext(context.Background(), "sh", "-c",
		`"$CARDEX_TEST_BINARY" -test.run=TestInheritedProcessLeaseDetectsSetsidDescendant >/dev/null 2>&1 &`)
	cmd.Env = append(os.Environ(),
		"CARDEX_TEST_BINARY="+os.Args[0],
		"CARDEX_TEST_SETSID_LEASE_CHILD=1",
	)
	setupProcGroup(cmd)
	if err := runCmdRegisteredForTask(cmd, taskID); err != nil {
		t.Fatal(err)
	}
	if !taskProcessResidue(taskID) {
		t.Fatal("a setsid descendant retaining the inherited execution lease must block fallback")
	}
	deadline := time.Now().Add(3 * time.Second)
	for taskProcessResidue(taskID) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if taskProcessResidue(taskID) {
		t.Fatal("lease residue must clear after the detached descendant exits and closes its inherited fd")
	}
}

func TestCompletedLeaseProactivelyLeavesResidueMap(t *testing.T) {
	const taskID = "completed-lease-prune"
	done := make(chan struct{})
	markTaskLeaseResidue(taskID, done)
	close(done)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		taskPGMu.Lock()
		remaining := len(taskLeaseResidue[taskID])
		taskPGMu.Unlock()
		if remaining == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("closed execution lease remained in the global residue map without a later task query")
}

func TestWorkspaceLeaseSurvivesMapLossAndBlocksDifferentFallbackTask(t *testing.T) {
	if os.Getenv("CARDEX_TEST_WORKSPACE_LEASE_CHILD") == "1" {
		if _, err := syscall.Setsid(); err != nil && !errors.Is(err, syscall.EPERM) {
			os.Exit(2)
		}
		time.Sleep(900 * time.Millisecond)
		os.Exit(0)
	}

	dir := t.TempDir()
	_, parentID := reservedTaskExec(t, dir)
	cmd := exec.CommandContext(context.Background(), "sh", "-c",
		`"$CARDEX_TEST_BINARY" -test.run=TestWorkspaceLeaseSurvivesMapLossAndBlocksDifferentFallbackTask >/dev/null 2>&1 &`)
	cmd.Env = append(os.Environ(),
		"CARDEX_TEST_BINARY="+os.Args[0],
		"CARDEX_TEST_WORKSPACE_LEASE_CHILD=1",
	)
	cmd.Dir = dir
	setupProcGroup(cmd)
	if err := runCmdRegisteredForTask(cmd, parentID); err != nil {
		t.Fatal(err)
	}

	// Simulate a Cardex restart: all process-local task maps disappear. The kernel-held workspace
	// flock must remain sufficient to prevent a differently-IDed fallback writer from starting.
	taskPGMu.Lock()
	delete(taskPG, parentID)
	delete(taskPGResidue, parentID)
	delete(taskLeaseResidue, parentID)
	taskPGMu.Unlock()
	if !workspaceProcessResidue(dir) {
		t.Fatal("detached descendant must remain visible after process-local residue maps are lost")
	}
	second := exec.CommandContext(context.Background(), "sh", "-c", "true")
	second.Dir = t.TempDir() // simulate a Codex review copy distinct from the authoritative workspace
	setupProcGroup(second)
	if err := runCmdRegisteredForTaskWorkspace(second, "different-fallback-task", dir); !errors.Is(err, errWorkspaceExecutionLeaseBusy) {
		t.Fatalf("different task ID must not acquire the same live workspace writer lease: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for workspaceProcessResidue(dir) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if workspaceProcessResidue(dir) {
		t.Fatal("workspace lease must release after every inheriting descendant exits")
	}
}
