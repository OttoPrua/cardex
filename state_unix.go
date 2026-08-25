//go:build !windows

package main

import (
	"os"
	"runtime"
	"syscall"
)

// processAlive 报告 pid 对应的进程是否存活。
// POSIX（macOS/Linux）：os.FindProcess 恒成功，靠向进程发 0 号信号探活——
// 存活返回 nil，进程已死返回 ESRCH 类错误。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func tryLockFile(f *os.File) error {
	fd := int(f.Fd())
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		runtime.KeepAlive(f)
		if err != syscall.EINTR {
			return err
		}
	}
}

func unlockFile(f *os.File) error {
	fd := int(f.Fd())
	for {
		err := syscall.Flock(fd, syscall.LOCK_UN)
		runtime.KeepAlive(f)
		if err != syscall.EINTR {
			return err
		}
	}
}
