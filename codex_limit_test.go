package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const codexUsageLimitText = "ERROR: You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage"

func fakeCodexUsageLimit(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"" + codexUsageLimitText + "\" >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInvokeCodexUsageLimitFlowsToLimitDetector(t *testing.T) {
	work := t.TempDir()
	cfg := defaultConfig("")
	cfg.CodexBin = fakeCodexUsageLimit(t)
	cfg.StepTimeoutMin = 1
	task := &Task{ID: "usage-limit-flow", Type: typeSequence, Dir: work}

	res, combined, err := invokeCodex(context.Background(), cfg, task, "test prompt")
	if err == nil {
		t.Fatal("fake codex 应以非零状态退出")
	}
	if res == nil || !res.IsError {
		t.Fatalf("codex 限额应形成 IsError result, got %+v", res)
	}
	if !strings.Contains(res.Result, "usage limit") {
		t.Fatalf("真实限额文本应从 codexErrorLine 流入 res.Result, got %q", res.Result)
	}
	if !isLimitHit(res, combined) {
		t.Fatalf("真实 codex 限额文本应被 isLimitHit 识别, result=%q combined=%q", res.Result, combined)
	}
}

func TestRunTaskCodexUsageLimitPausesWithoutAttemptOrClaudeCooldown(t *testing.T) {
	root := testRoot(t)
	work := t.TempDir()
	cfg := defaultConfig("")
	cfg.CodexBin = fakeCodexUsageLimit(t)
	cfg.StepTimeoutMin = 1
	cfg.LimitFallbackMin = 30
	cfg.CooldownMarginSec = 0
	cfg.MaxAttempts = 3

	task := newTask(root, cfg, typeSequence, "codex usage limit", work, []string{"test prompt"}, 1)
	task.Attempts = 2
	task.SessionID = "stale-session"
	task.MidStep = true
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}

	if err := runTask(context.Background(), root, cfg, task, true); err != nil {
		t.Fatal(err)
	}

	got, err := loadTask(root, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusLimitPaused {
		t.Fatalf("codex 限额应暂停而非失败, status=%q last_error=%q", got.Status, got.LastError)
	}
	if got.Attempts != 2 {
		t.Fatalf("codex 限额不得消耗 attempts, got %d want 2", got.Attempts)
	}
	if got.ResumeAtEpoch <= 0 {
		t.Fatalf("codex 限额应设置 resume_at_epoch, got %d", got.ResumeAtEpoch)
	}
	if eligible(got, time.Unix(got.ResumeAtEpoch-1, 0)) {
		t.Fatal("resume_at 到点前不得重新派发 codex 卡")
	}
	if !eligible(got, time.Unix(got.ResumeAtEpoch, 0)) {
		t.Fatal("resume_at 到点后 tick 应自动重新派发 codex 卡")
	}
	if got.MidStep || got.SessionID != "" {
		t.Fatalf("codex 无会话可续，应清空续跑态: mid_step=%v session_id=%q", got.MidStep, got.SessionID)
	}
	if !strings.HasPrefix(got.LastError, "codex 用量限额: ") {
		t.Fatalf("应记录 codex 专属限额原因, got %q", got.LastError)
	}
	if _, err := os.Stat(cooldownPath(root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("codex 限额不得写 claude 全局 cooldown, stat err=%v", err)
	}
}
