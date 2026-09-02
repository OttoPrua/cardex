package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	antigravityRunnerName = "agy"
)

func antigravityVia(via string) bool { return via == antigravityRunnerName }

func antigravityEnabled(cfg *Config) bool {
	return cfg != nil && cfg.Antigravity != nil && cfg.Antigravity.Enabled &&
		strings.TrimSpace(cfg.AntigravityBin) != ""
}

func resolveAntigravityModel(_ *Config, t *Task) string {
	if t != nil && strings.TrimSpace(t.AgyModel) != "" {
		return strings.TrimSpace(t.AgyModel)
	}
	return ""
}

func resolveAntigravityEffort(cfg *Config) string {
	if cfg == nil || cfg.Antigravity == nil || strings.TrimSpace(cfg.Antigravity.Effort) == "" {
		return "high"
	}
	return strings.ToLower(strings.TrimSpace(cfg.Antigravity.Effort))
}

func parseAntigravityJSON(raw []byte) *claudeResult {
	res := &claudeResult{Type: "result"}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		res.IsError = true
		res.Subtype = "antigravity_invalid_terminal"
		res.Result = "Antigravity 未返回有效 JSON 终局"
		return res
	}
	for _, key := range []string{"result", "response", "text"} {
		var value string
		if json.Unmarshal(object[key], &value) == nil && strings.TrimSpace(value) != "" {
			res.Result = strings.TrimSpace(value)
			res.ObservationComplete = true
			res.SemanticEvents = 1
			res.ModelEvents = 1
			return res
		}
	}
	if rawErr, ok := object["error"]; ok && string(rawErr) != "null" {
		res.IsError = true
		res.Subtype = "antigravity_error"
		var text string
		if json.Unmarshal(rawErr, &text) == nil {
			res.Result = strings.TrimSpace(text)
		}
		if res.Result == "" {
			res.Result = "Antigravity 返回结构化错误"
		}
		res.ObservationComplete = true
		return res
	}
	res.IsError = true
	res.Subtype = "antigravity_missing_terminal"
	res.Result = "Antigravity JSON 缺少 result/response/text 终局"
	res.ObservationComplete = true
	return res
}

func antigravityArgs(cfg *Config, t *Task, model, prompt string) []string {
	mode := "plan"
	if t.Type == typeSequence && !t.SkipPermissions {
		mode = "accept-edits"
	}
	args := []string{"--output-format", "json", "--mode", mode, "--model", model}
	if !strings.Contains(strings.ToLower(model), "thinking") {
		args = append(args, "--effort", resolveAntigravityEffort(cfg))
	}
	return append(args, "--disable-slash-commands", "--print", prompt)
}

func invokeAntigravity(ctx context.Context, cfg *Config, t *Task, prompt string) (*claudeResult, string, error) {
	if !antigravityEnabled(cfg) {
		return nil, "", fmt.Errorf("antigravity/antigravity_bin 未启用")
	}
	if !providerPreflightReady(t, antigravityRunnerName) {
		return nil, "", fmt.Errorf("Antigravity 未通过当前派发的 provider preflight")
	}
	model := resolveAntigravityModel(cfg, t)
	if strings.TrimSpace(model) == "" {
		return nil, "", fmt.Errorf("未解析出 Antigravity 模型")
	}
	args := antigravityArgs(cfg, t, model, prompt)
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.StepTimeoutMin)*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(runCtx, cfg.AntigravityBin, args...)
	setupProcGroup(cmd)
	cmd.Dir = t.Dir
	home, _ := os.UserHomeDir()
	cmd.Env = providerChildEnv(home, map[string]string{"NO_COLOR": "1"})
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := runCmdRegisteredForTask(cmd, t.ID)
	if runCtx.Err() == context.DeadlineExceeded {
		runErr = fmt.Errorf("步骤超时（%d 分钟）", cfg.StepTimeoutMin)
	}
	res := parseAntigravityJSON(stdout.Bytes())
	if runErr != nil {
		res.IsError = true
		if res.Subtype == "" || res.Subtype == "antigravity_missing_terminal" {
			res.Subtype = "antigravity_process_error"
			res.Result = "Antigravity 进程未正常完成"
		}
	}
	if res.IsError && runErr == nil {
		runErr = fmt.Errorf("Antigravity 返回错误: %s", summarizeResult(res.Result))
	}
	return res, stdout.String() + "\n" + stderr.String(), runErr
}
