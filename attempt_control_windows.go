//go:build windows

package main

func withControlFileLock(_ string, _ string, fn func() error) error {
	return fn()
}

func attemptProcessPGID(pid int) int { return pid }

func canonicalWorkspaceID(dir string) string { return dir }

func processStartIdentity(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	return fmtWindowsPIDIdentity(pid)
}

func fmtWindowsPIDIdentity(pid int) (string, bool) {
	return "", false
}

func verifyAttemptProcess(rec *AttemptRecord) bool {
	if rec == nil || rec.PID <= 0 {
		return false
	}
	if rec.StartIdentity == "" || rec.WorkspaceLeaseID == "" {
		return false
	}
	return processAlive(rec.PID)
}
