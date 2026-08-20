//go:build windows

package main

import (
	"os"
	"os/exec"
	"time"
)

func prepareTaskProcessLease(_ *exec.Cmd, _, _ string) (*taskProcessLease, error) { return nil, nil }

func workspaceProcessResidue(_ string) bool { return true }

// Windows 无 POSIX 进程组语义：取消/超时退回默认的单进程 Kill（孙进程可能残留），
// WaitDelay 防残留进程吊住 stdout 管道不放。
func setupProcGroup(cmd *exec.Cmd) {
	cmd.WaitDelay = 10 * time.Second
}

func killProcGroup(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// Windows has no POSIX process-group probe in this compatibility layer. A reaped direct child cannot
// prove that it left no detached descendants, so policy fallback fails closed by reporting residue.
// Ordinary task completion is unaffected; only the proof-gated next-leg transition consumes this bit.
func processGroupAlive(pid int) bool { return true }

// Cardex has not yet bound provider descendants to a Windows Job Object, so it cannot prove the
// zero-writer/process condition required for an automatic next-engine transition.
func policyFallbackProcessProofSupported() bool { return false }
