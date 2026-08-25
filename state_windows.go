//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// processAlive 报告 pid 对应的进程是否存活。
// Windows 上 os.FindProcess 会 OpenProcess：pid 不存在则返回错误，据此判活。
// 不能用 proc.Signal(syscall.Signal(0))——Windows 对非 Kill 信号一律返回
// "not supported by windows"，会把存活进程误判为已死、破坏单实例锁。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = proc.Release()
	return true
}

const (
	lockfileFailImmediately = 0x00000001
	lockfileExclusiveLock   = 0x00000002
)

var (
	modkernel32      = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = modkernel32.NewProc("LockFileEx")
	procUnlockFileEx = modkernel32.NewProc("UnlockFileEx")
)

func tryLockFile(f *os.File) error {
	return lockFileEx(f, lockfileExclusiveLock|lockfileFailImmediately)
}

func unlockFile(f *os.File) error {
	var ol syscall.Overlapped
	r1, _, err := procUnlockFileEx.Call(
		f.Fd(),
		0,
		uintptr(^uint32(0)),
		uintptr(^uint32(0)),
		uintptr(unsafe.Pointer(&ol)),
	)
	if r1 == 0 {
		return err
	}
	return nil
}

func lockFileEx(f *os.File, flags uint32) error {
	var ol syscall.Overlapped
	r1, _, err := procLockFileEx.Call(
		f.Fd(),
		uintptr(flags),
		0,
		uintptr(^uint32(0)),
		uintptr(^uint32(0)),
		uintptr(unsafe.Pointer(&ol)),
	)
	if r1 == 0 {
		return err
	}
	return nil
}
