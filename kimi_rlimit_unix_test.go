//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestInvokeKimiCLIRejectsLowOpenFileLimitBeforeSpawn(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "spawned")
	bin := filepath.Join(dir, "kimi")
	script := "#!/bin/sh\n" +
		"touch " + shSingleQuote(marker) + "\n" +
		"printf '%s\\n' '{\"role\":\"assistant\",\"content\":\"SHOULD_NOT_RUN\"}'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := kimiCLITestConfig(t, bin)
	task := &Task{ID: "kimi-low-fd", Model: "opus", Type: typeSequence, Dir: dir}

	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &old); err != nil {
		t.Fatal(err)
	}
	low := old
	low.Cur = 256
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &low); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &old); err != nil {
			t.Errorf("restore RLIMIT_NOFILE: %v", err)
		}
	}()

	res, _, err := invokeKimiCLI(context.Background(), testRoot(t), cfg, task, "do not run")
	if err == nil || res == nil || !res.IsError || !strings.Contains(res.Result, "EMFILE") {
		t.Fatalf("low FD limit must fail before spawn: res=%+v err=%v", res, err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("Kimi process was spawned despite failed preflight: %v", statErr)
	}
}
