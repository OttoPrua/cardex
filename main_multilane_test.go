package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestCmdAddPersistsWriteDomainAndDependsOnSecretFree(t *testing.T) {
	root := testRoot(t)
	if err := saveConfig(root, defaultConfig("claude")); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "internal", "auth"), 0o755); err != nil {
		t.Fatal(err)
	}
	dep := newTask(root, testCfg(), typeSequence, "dep", dir, []string{"p"}, 5)
	if err := saveTask(root, dep); err != nil {
		t.Fatal(err)
	}
	if err := cmdAdd([]string{
		"-root", root, "-dir", dir, "-title", "auth lane",
		"-write-domain-id", "auth-tokens",
		"-write-domain-lineage", "auth-tokens-lineage",
		"-write-domain-component", "auth",
		"-write-paths", "internal/auth",
		"-write-resources", "database:auth.primary",
		"-depends-on", dep.ID,
		"SECRET PROMPT TOKEN=abc",
	}); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return cmdList([]string{"-root", root, "-json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var tasks []Task
	if err := json.Unmarshal([]byte(out), &tasks); err != nil {
		t.Fatalf("list json: %v %s", err, out)
	}
	var got *Task
	for i := range tasks {
		if tasks[i].WriteDomain != nil {
			got = &tasks[i]
			break
		}
	}
	if got == nil || got.WriteDomain.ID != "auth-tokens" || len(got.DependsOn) != 1 || got.DependsOn[0] != dep.ID {
		t.Fatalf("persisted claims missing: %+v", tasks)
	}
	if got.WriteDomain.Paths[0] != "internal/auth" || got.WriteDomain.Resources[0].ID != "auth.primary" {
		t.Fatalf("domain: %+v", got.WriteDomain)
	}
	blob, _ := json.Marshal(got.WriteDomain)
	if bytes.Contains(bytes.ToLower(blob), []byte("secret")) || bytes.Contains(bytes.ToLower(blob), []byte("token=abc")) {
		t.Fatalf("write domain leaked prompt: %s", blob)
	}
}

func finishTickTask(root string, tk *Task) {
	tk.Status = statusDone
	markControlTerminal(tk)
	tk.touch()
	_ = writeTaskFile(root, tk)
}

func TestTickEnforcesDisjointLanesAndLegacySerial(t *testing.T) {
	orig := tickRunTask
	t.Cleanup(func() { tickRunTask = orig })

	overlapTick := func(root string, cfg *Config, hold time.Duration) int32 {
		var concurrent, maxC atomic.Int32
		tickRunTask = func(ctx context.Context, root string, cfg *Config, tk *Task, via string) error {
			n := concurrent.Add(1)
			for {
				old := maxC.Load()
				if n <= old || maxC.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(hold)
			concurrent.Add(-1)
			finishTickTask(root, tk)
			return nil
		}
		if err := tick(root, cfg, true, true); err != nil {
			t.Fatal(err)
		}
		return maxC.Load()
	}

	root := testRoot(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "internal", "auth"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "internal", "billing"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig("claude")
	cfg.MaxParallel = 2
	cfg.DrainRescanSec = 1
	cfg.ClaudeBin = fakeClaudeBin(t, mkOKResultJSON("sess"), "", 0)
	_ = explicitTask(t, root, "auth", dir, "auth-tokens", "auth-tokens-lineage", "auth", []string{"internal/auth"}, nil)
	_ = explicitTask(t, root, "bill", dir, "billing-core", "billing-core-lineage", "billing", []string{"internal/billing"}, nil)
	if got := overlapTick(root, cfg, 80*time.Millisecond); got < 2 {
		t.Fatalf("disjoint explicit lanes must overlap in tick, concurrent=%d", got)
	}

	root2 := testRoot(t)
	same := t.TempDir()
	legacy := newTask(root2, testCfg(), typeSequence, "legacy a", same, []string{"p"}, 5)
	if err := saveTask(root2, legacy); err != nil {
		t.Fatal(err)
	}
	legacyB := newTask(root2, testCfg(), typeSequence, "legacy b", same, []string{"p"}, 5)
	if err := saveTask(root2, legacyB); err != nil {
		t.Fatal(err)
	}
	if got := overlapTick(root2, cfg, 80*time.Millisecond); got != 1 {
		t.Fatalf("legacy same-dir must serialize in tick, concurrent=%d", got)
	}
}
