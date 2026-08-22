//go:build !windows

package main

import (
	"os"
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
