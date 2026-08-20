//go:build !windows

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var errWorkspaceExecutionLeaseBusy = errors.New("workspace execution lease is already held")

func workspaceExecutionLeasePath(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve workspace execution lease identity: %w", err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace execution lease target is not a directory: %s", real)
	}
	sum := sha256.Sum256([]byte(real))
	base := filepath.Join(os.TempDir(), fmt.Sprintf("cardex-writer-leases-%d", os.Getuid()))
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(base, hex.EncodeToString(sum[:])+".lock"), nil
}

func acquireWorkspaceExecutionLease(dir string) (*os.File, error) {
	path, err := workspaceExecutionLeasePath(dir)
	if err != nil || path == "" {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("%w for %s", errWorkspaceExecutionLeaseBusy, dir)
		}
		return nil, err
	}
	return f, nil
}

// workspaceProcessResidue is restart-safe: flock state lives in the kernel and remains held by an
// inherited descriptor in a detached descendant even if the Cardex process and its in-memory maps die.
// It is also workspace-keyed, so a fallback child with a different task ID cannot overlap its parent.
func workspaceProcessResidue(dir string) bool {
	f, err := acquireWorkspaceExecutionLease(dir)
	if err != nil {
		return true
	}
	if f == nil {
		return false
	}
	unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	closeErr := f.Close()
	return unlockErr != nil || closeErr != nil
}

func prepareTaskProcessLease(cmd *exec.Cmd, taskID, workspaceDir string) (*taskProcessLease, error) {
	if taskID == "" {
		return nil, nil
	}
	workspaceLease, err := acquireWorkspaceExecutionLease(workspaceDir)
	if err != nil {
		return nil, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		if workspaceLease != nil {
			_ = workspaceLease.Close()
		}
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, reader)
		_ = reader.Close()
		close(done)
	}()
	cmd.ExtraFiles = append(cmd.ExtraFiles, writer)
	if workspaceLease != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, workspaceLease)
	}
	return &taskProcessLease{
		done: done,
		commit: func() {
			// The direct child now owns the inherited copy; close only Cardex's parent copy.
			_ = writer.Close()
			if workspaceLease != nil {
				_ = workspaceLease.Close()
			}
		},
		abort: func() {
			_ = writer.Close()
			_ = reader.Close()
			if workspaceLease != nil {
				_ = workspaceLease.Close()
			}
		},
	}, nil
}

// setupProcGroup 让执行器子进程自成进程组，取消/超时时可整组击杀：
// claude 会派生 bash 等孙进程，只杀直接子进程会留孤儿继续改仓库、烧额度
// （ssh 同路径——杀本地 ssh 释放槽位与目录锁；远端进程杀不到，只能断连）。
// WaitDelay 兜底：组内进程若 setsid 逃逸并吊住 stdout 管道，击杀后 10s 强制收尾。
func setupProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 10 * time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return killProcGroup(cmd.Process.Pid)
	}
}

func killProcGroup(pid int) error {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if err == syscall.ESRCH {
		return os.ErrProcessDone
	}
	return err
}

func processGroupAlive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	err := syscall.Kill(-pgid, syscall.Signal(0))
	return err == nil || err == syscall.EPERM
}

func policyFallbackProcessProofSupported() bool { return true }
