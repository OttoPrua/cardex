//go:build windows

package main

import "os"

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
