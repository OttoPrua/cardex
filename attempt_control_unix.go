//go:build !windows

package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

func withControlFileLock(root, taskID string, fn func() error) error {
	path := controlLockPath(root, taskID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}

func attemptProcessPGID(pid int) int {
	if pid <= 0 {
		return 0
	}
	pg, err := syscall.Getpgid(pid)
	if err != nil {
		return 0
	}
	return pg
}

func canonicalWorkspaceID(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs
	}
	return real
}

func processStartIdentity(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	if id, ok := linuxProcStartIdentity(pid); ok {
		return id, true
	}
	return darwinProcStartIdentity(pid)
}

func linuxProcStartIdentity(pid int) (string, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", false
	}
	s := string(data)
	rparen := strings.LastIndex(s, ")")
	if rparen < 0 || rparen+2 >= len(s) {
		return "", false
	}
	fields := strings.Fields(s[rparen+2:])
	// After comm: state ppid pgrp session tty_nr tpgid flags minflt cminflt
	// majflt cmajflt utime stime cutime cstime priority nice num_threads
	// itrealvalue starttime. starttime is field 20 of this slice (0-based 19).
	if len(fields) < 20 {
		return "", false
	}
	return "linux:" + fields[19], true
}

func darwinProcStartIdentity(pid int) (string, bool) {
	const (
		sysProcInfo         = 336
		procInfoCallPIDInfo = 2
		procPIDTBSDInfo     = 3
		pbiStartTvsecOff    = 120
		pbiStartTvusecOff   = 128
		pbiPgidOff          = 100
		pbiPidOff           = 12
		bufSize             = 256
	)
	buf := make([]byte, bufSize)
	r1, _, errno := syscall.RawSyscall6(
		sysProcInfo,
		procInfoCallPIDInfo,
		uintptr(pid),
		procPIDTBSDInfo,
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	if errno != 0 || int(r1) < pbiStartTvusecOff+8 {
		return "", false
	}
	gotPID := int32(binary.LittleEndian.Uint32(buf[pbiPidOff : pbiPidOff+4]))
	if int(gotPID) != pid {
		return "", false
	}
	sec := binary.LittleEndian.Uint64(buf[pbiStartTvsecOff : pbiStartTvsecOff+8])
	usec := binary.LittleEndian.Uint64(buf[pbiStartTvusecOff : pbiStartTvusecOff+8])
	pgid := binary.LittleEndian.Uint32(buf[pbiPgidOff : pbiPgidOff+4])
	return "darwin:" + strconv.FormatUint(sec, 10) + "." + strconv.FormatUint(usec, 10) +
		":pgid=" + strconv.FormatUint(uint64(pgid), 10), true
}

func verifyAttemptProcess(rec *AttemptRecord) bool {
	if rec == nil {
		return false
	}
	pid := rec.PID
	if pid <= 0 {
		return false
	}
	if !processAlive(pid) {
		return false
	}
	if rec.StartIdentity == "" || rec.WorkspaceLeaseID == "" {
		return false
	}
	got, ok := processStartIdentity(pid)
	if !ok || got != rec.StartIdentity {
		return false
	}
	if rec.PGID > 0 {
		pg, err := syscall.Getpgid(pid)
		if err != nil {
			return false
		}
		if pg != rec.PGID {
			return false
		}
	}
	if rec.WorkspaceLeaseID != "" && !workspaceLeaseHeld(rec.WorkspaceLeaseID) {
		return false
	}
	return true
}
