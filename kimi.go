package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	kimiCLIRunnerName                      = "kimi-cli"
	kimiCLICooldownName                    = "kimi-cli"
	routeReasonKimiCLIExplicit             = "kimi_cli_explicit"
	routeReasonKimiCLIOpus                 = "kimi_cli_opus_preferred"
	routeReasonKimiCLICooldownFallback     = "kimi_cli_cooldown_fallback"
	routeReasonKimiCLILimitFallbackPending = "kimi_cli_limit_fallback_pending"
	routeReasonKimiCLILimitFallback        = "kimi_cli_limit_fallback"
	routeReasonCodexBackendExcluded        = "codex_backend_excluded"
)

func kimiCLIVia(via string) bool { return via == kimiCLIRunnerName }

func validateKimiCLIOpenFileLimit(limit uint64) error {
	if limit < cardexTickOpenFileLimit {
		return fmt.Errorf("EMFILE 风险: 文件描述符软上限=%d，Kimi CLI 要求至少=%d（%s SoftResourceLimits.NumberOfFiles）",
			limit, cardexTickOpenFileLimit, launchdLabel)
	}
	return nil
}

func preserveKimiCLIProcessError(res *claudeResult, stderr string, runErr error) {
	if res == nil || res.IsError || runErr == nil {
		return
	}
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)
		if line == "" || (!strings.Contains(lower, "emfile") && !strings.Contains(lower, "too many open files")) {
			continue
		}
		res.IsError = true
		res.Subtype = "kimi_cli_process_error"
		res.Result = line
		return
	}
}

func resolveKimiCLIModel(cfg *Config, t *Task) string {
	if t != nil && strings.TrimSpace(t.KimiModel) != "" {
		return strings.TrimSpace(t.KimiModel)
	}
	if cfg == nil {
		return ""
	}
	if t != nil && t.PreferRunner != kimiCLIRunnerName && cfg.KimiCLIOpus != nil {
		if model := strings.TrimSpace(cfg.KimiCLIOpus.Model); model != "" {
			return model
		}
	}
	if model := strings.TrimSpace(cfg.KimiCLIModel); model != "" {
		return model
	}
	if cfg.KimiCLIOpus != nil {
		return strings.TrimSpace(cfg.KimiCLIOpus.Model)
	}
	return ""
}

func resolveKimiCLIEffort(cfg *Config, t *Task) string {
	if cfg == nil {
		return ""
	}
	if t != nil && t.PreferRunner != kimiCLIRunnerName && cfg.KimiCLIOpus != nil {
		return strings.TrimSpace(cfg.KimiCLIOpus.Effort)
	}
	if t != nil {
		switch effort := strings.ToLower(strings.TrimSpace(t.Effort)); effort {
		case "low", "high", "max":
			return effort
		}
	}
	if effort := strings.TrimSpace(cfg.KimiCLIEffort); effort != "" {
		return effort
	}
	if cfg.KimiCLIOpus != nil {
		return strings.TrimSpace(cfg.KimiCLIOpus.Effort)
	}
	return ""
}

func kimiCLIReady(root string, cfg *Config, now time.Time) bool {
	if cfg == nil || strings.TrimSpace(cfg.KimiCLIBin) == "" {
		return false
	}
	cd := loadEngineCooldown(root, kimiCLICooldownName)
	return cd == nil || !cd.active(now)
}

func kimiCLIPinnedReady(root string, cfg *Config, now time.Time) bool {
	return kimiCLIReady(root, cfg, now)
}

func kimiCLIAutoOpusBaseEligible(cfg *Config, t *Task) bool {
	if t == nil || t.PreferRunner != "codex" || t.RemoteHost != "" || !isOpusTask(cfg, t) {
		return false
	}
	return t.CodexModel == "" && t.XCodexModel == "" && t.XRole == "" && codexEligible(t)
}

func kimiCLIBackendExcluded(cfg *Config, t *Task) bool {
	if cfg == nil {
		return false
	}
	r := cfg.KimiCLIOpus
	return r != nil && r.Enabled && r.ExcludeBackend &&
		kimiCLIAutoOpusBaseEligible(cfg, t) && backendDevelopmentTask(t)
}

func kimiCLIOpusPolicyApplies(cfg *Config, t *Task) bool {
	if cfg == nil || cfg.KimiCLIOpus == nil || !cfg.KimiCLIOpus.Enabled ||
		!grokBuildKimiFallbackEnabled(cfg) || !kimiCLIAutoOpusBaseEligible(cfg, t) || kimiCLIBackendExcluded(cfg, t) {
		return false
	}
	return t.RouteReason != routeReasonKimiCLILimitFallbackPending &&
		t.RouteReason != routeReasonKimiCLILimitFallback
}

// kimiCLIOpusEligible 是通用模式的 Kimi 可用性兼容判定。Owner 六行生产路由中，
// resolveOwnerRoute 只在 Grok 合格失败后才把同一张 Opus/general 卡推进到 Kimi 第二腿。
// Kimi 安全失败接力后的卡以 route_reason 钉住下一腿，冷却结束后也不会弹回 Kimi。
func kimiCLIOpusEligible(root string, cfg *Config, t *Task, now time.Time) bool {
	return kimiCLIOpusPolicyApplies(cfg, t) && kimiCLIReady(root, cfg, now)
}

func kimiCLIPinnedUsesOpusModel(cfg *Config, t *Task) bool {
	if cfg == nil || cfg.KimiCLIOpus == nil || t == nil || t.PreferRunner != kimiCLIRunnerName {
		return false
	}
	pinned := strings.TrimSpace(resolveKimiCLIModel(cfg, t))
	target := strings.TrimSpace(cfg.KimiCLIOpus.Model)
	return pinned != "" && target != "" && strings.EqualFold(pinned, target)
}

func kimiCLILosslessFallback(t *Task) bool {
	if t == nil || t.MidStep {
		return false
	}
	return t.FreshSteps || (len(t.Prompts) == 1 && t.Step == 0)
}

func pinKimiCLILimitFallback(cfg *Config, t *Task) {
	if t == nil {
		return
	}
	t.PreferRunner = "codex"
	if cfg == nil {
		return
	}
	model := strings.TrimSpace(cfg.CodexFallbackOpusModel)
	if model == "" {
		model = strings.TrimSpace(cfg.CodexTierModels["opus"])
	}
	if model != "" {
		t.CodexModel = model
	}
	reasoning := strings.TrimSpace(cfg.CodexFallbackOpusReasoning)
	if reasoning == "" {
		reasoning = strings.TrimSpace(cfg.CodexTierReasoning["opus"])
	}
	if reasoning != "" {
		t.Effort = reasoning
		t.EffortExplicit = true
	}
}

var kimiCLIQuotaRe = regexp.MustCompile(`(?i)(?:^|[^0-9])429(?:[^0-9]|$)|too many requests|rate limit|quota (?:exceeded|exhausted)|insufficient (?:quota|credits)|usage limit|额度(?:不足|已用完)|配额(?:不足|已用尽)|限额(?:不足|已用尽)`)

type kimiCLIEvent struct {
	Role      string          `json:"role"`
	Type      string          `json:"type"`
	Version   string          `json:"version"`
	Content   json.RawMessage `json:"content"`
	SessionID string          `json:"session_id"`
	Error     json.RawMessage `json:"error"`
}

func kimiCLIContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var texts []string
		for _, block := range blocks {
			if block.Text != "" {
				texts = append(texts, block.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

func parseKimiCLIJSONL(raw string) *claudeResult {
	res := &claudeResult{Type: "result", ObservationComplete: true}
	var texts []string
	s := bufio.NewScanner(strings.NewReader(raw))
	s.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for s.Scan() {
		var ev kimiCLIEvent
		if json.Unmarshal([]byte(strings.TrimSpace(s.Text())), &ev) != nil {
			res.ObservationComplete = false
			continue
		}
		if ev.SessionID != "" {
			res.SessionID = ev.SessionID
		}
		text := kimiCLIContentText(ev.Content)
		role := strings.ToLower(strings.TrimSpace(ev.Role))
		typ := strings.ToLower(strings.TrimSpace(ev.Type))
		if role == "assistant" {
			// Any assistant event proves model work even when its content is reasoning/tool metadata
			// that this compatibility parser cannot render as text.
			res.SemanticEvents++
			res.ModelEvents++
			res.NumTurns++
			if text != "" {
				texts = append(texts, text)
				res.TerminalEvents++
			}
		}
		if strings.Contains(role, "tool") || strings.Contains(typ, "tool") {
			res.ToolEvents++
		}
		if strings.Contains(role, "model") || strings.Contains(typ, "model.") {
			res.ModelEvents++
		}
		if typ == "error" || role == "error" {
			res.IsError = true
			if text != "" {
				res.Result = text
			} else if len(ev.Error) > 0 {
				res.Result = string(ev.Error)
			}
		}
		presemanticMeta := role == "meta" && (typ == "system.version" || typ == "system.init" || typ == "session.init" || typ == "session.resume_hint" || typ == "start")
		known := role == "assistant" || role == "tool" || role == "error" || presemanticMeta ||
			role == "system" || role == "user" || typ == "error" || strings.HasPrefix(typ, "system.") ||
			strings.Contains(typ, "tool")
		if !known {
			res.ObservationComplete = false
		}
	}
	if s.Err() != nil {
		res.ObservationComplete = false
	}
	if len(texts) > 0 {
		res.Result = strings.Join(texts, "\n")
	}
	return res
}

// kimiCLI0361VersionOnly reports the narrow cold-start signature observed after upgrading the
// standalone CLI to 0.36.1 before its native search worker cache had been materialized. The process
// exits non-zero after system.version and before any assistant, tool, error, or resumable session
// event. No other output shape is safe to replay automatically.
func kimiCLI0361VersionOnly(raw string) bool {
	sawVersion := false
	s := bufio.NewScanner(strings.NewReader(raw))
	s.Buffer(make([]byte, 0, 4*1024), 1024*1024)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		var ev kimiCLIEvent
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Role != "meta" ||
			ev.Type != "system.version" || ev.Version != "0.36.1" {
			return false
		}
		sawVersion = true
	}
	return sawVersion && s.Err() == nil
}

func kimiCLILimitScanText(res *claudeResult, combined string) string {
	var parts []string
	for _, line := range strings.Split(combined, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev kimiCLIEvent
		if json.Unmarshal([]byte(line), &ev) == nil && (ev.Role != "" || ev.Type != "") {
			if ev.Type == "error" || ev.Role == "error" {
				parts = append(parts, kimiCLIContentText(ev.Content), string(ev.Error))
			}
			continue
		}
		parts = append(parts, line)
	}
	if res != nil && res.IsError && strings.TrimSpace(res.Result) != "" {
		parts = append(parts, res.Result)
	}
	return strings.Join(parts, "\n")
}

func isLimitHitKimiCLI(res *claudeResult, combined string) bool {
	if res != nil && !res.IsError && strings.TrimSpace(res.Result) != "" {
		return false
	}
	scan := kimiCLILimitScanText(res, combined)
	return limitRe.MatchString(scan) || engineQuotaRe.MatchString(scan) || kimiCLIQuotaRe.MatchString(scan)
}

func kimiCLIResetEpoch(cfg *Config, res *claudeResult, combined string, now time.Time) int64 {
	scan := kimiCLILimitScanText(res, combined)
	fallback := 0
	if cfg != nil && cfg.KimiCLIOpus != nil {
		fallback = cfg.KimiCLIOpus.LimitFallbackMin
	}
	return engineResetEpoch(combined+"\n"+resultText(res), scan, cfg, EngineProfile{LimitFallbackMin: fallback}, now)
}

func replaceProcessEnv(base []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(base)+1)
	for _, item := range base {
		if !strings.HasPrefix(item, prefix) {
			out = append(out, item)
		}
	}
	return append(out, prefix+value)
}

func overrideKimiTopLevelConfig(raw string, values map[string]string) string {
	lines := strings.Split(raw, "\n")
	inTable := false
	kept := make([]string, 0, len(lines)+len(values))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			inTable = true
		}
		if !inTable {
			drop := false
			for key := range values {
				if strings.HasPrefix(trimmed, key+" ") || strings.HasPrefix(trimmed, key+"=") {
					drop = true
					break
				}
			}
			if drop {
				continue
			}
		}
		kept = append(kept, line)
	}
	prefix := make([]string, 0, len(values)+1)
	for _, key := range []string{"default_permission_mode", "default_plan_mode"} {
		if value, ok := values[key]; ok {
			prefix = append(prefix, key+" = "+value)
		}
	}
	prefix = append(prefix, "")
	return strings.Join(append(prefix, kept...), "\n")
}

// prepareKimiCLIHome creates a mode-specific Cardex-owned runtime home so prompt mode can use
// automatic permissions without mutating the user's global Kimi config. Review tasks use Kimi's
// configured plan mode, while sequence tasks use execute mode. Keeping separate homes avoids a
// concurrent review changing the permissions of an executing task (or vice versa). OAuth
// credentials remain in the user's Kimi home and are referenced through a directory symlink; the
// token is never copied into Cardex state.
func prepareKimiCLIHome(root string, cfg *Config, planMode bool) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("Kimi CLI 配置为空")
	}
	sourceHome := strings.TrimSpace(cfg.KimiCLIHome)
	if sourceHome == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		sourceHome = filepath.Join(userHome, ".kimi-code")
	}
	sourceConfig := filepath.Join(sourceHome, "config.toml")
	raw, err := os.ReadFile(sourceConfig)
	if err != nil {
		return "", fmt.Errorf("读取 Kimi CLI 配置失败 %s: %w", sourceConfig, err)
	}
	sourceCredentials := filepath.Join(sourceHome, "credentials")
	if _, err := os.Stat(sourceCredentials); err != nil {
		return "", fmt.Errorf("Kimi CLI OAuth 凭据目录不可用 %s: %w", sourceCredentials, err)
	}

	mode := "execute"
	if planMode {
		mode = "review"
	}
	runtimeHome := filepath.Join(root, "kimi-cli-home", mode)
	if err := os.MkdirAll(runtimeHome, 0o700); err != nil {
		return "", err
	}
	destCredentials := filepath.Join(runtimeHome, "credentials")
	if info, err := os.Lstat(destCredentials); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return "", fmt.Errorf("Kimi CLI 隔离凭据路径已存在且不是软链接: %s", destCredentials)
		}
		target, readErr := os.Readlink(destCredentials)
		if readErr != nil || target != sourceCredentials {
			return "", fmt.Errorf("Kimi CLI 隔离凭据软链接目标不一致: %s", destCredentials)
		}
	} else if os.IsNotExist(err) {
		if err := os.Symlink(sourceCredentials, destCredentials); err != nil {
			return "", err
		}
	} else {
		return "", err
	}

	planValue := "false"
	if planMode {
		planValue = "true"
	}
	rendered := overrideKimiTopLevelConfig(string(raw), map[string]string{
		"default_permission_mode": `"auto"`,
		"default_plan_mode":       planValue,
	})
	tmp, err := os.CreateTemp(runtimeHome, ".config-*.toml")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.WriteString(rendered); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, filepath.Join(runtimeHome, "config.toml")); err != nil {
		return "", err
	}
	return runtimeHome, nil
}

// invokeKimiCLI uses the CLI's OAuth credential store. Cardex only selects the configured model and
// sets KIMI_MODEL_THINKING_EFFORT for this child process; it never reads or copies the OAuth token.
func invokeKimiCLI(ctx context.Context, root string, cfg *Config, t *Task, prompt string) (*claudeResult, string, error) {
	if cfg == nil || strings.TrimSpace(cfg.KimiCLIBin) == "" {
		return nil, "", fmt.Errorf("kimi_cli_bin 未配置")
	}
	model := resolveKimiCLIModel(cfg, t)
	if model == "" {
		return nil, "", fmt.Errorf("未解析出 Kimi CLI 模型")
	}
	openFileLimit, err := currentOpenFileSoftLimit()
	if err != nil {
		return nil, "", fmt.Errorf("Kimi CLI 启动前读取文件描述符上限失败: %w", err)
	}
	if err := validateKimiCLIOpenFileLimit(openFileLimit); err != nil {
		res := &claudeResult{Type: "result", IsError: true, Subtype: "kimi_cli_preflight", Result: err.Error()}
		return res, "", err
	}
	args := make([]string, 0, 10)
	if t.SessionID != "" {
		args = append(args, "--session", t.SessionID)
	}
	args = append(args, "--model", model)
	args = append(args, "--prompt", prompt, "--output-format", "stream-json")
	planMode := t.Type != typeSequence && !t.SkipPermissions
	runtimeHome, err := prepareKimiCLIHome(root, cfg, planMode)
	if err != nil {
		return nil, "", err
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.StepTimeoutMin)*time.Minute)
	defer cancel()
	cmdEnv := replaceProcessEnv(os.Environ(), "KIMI_CODE_NO_AUTO_UPDATE", "1")
	cmdEnv = replaceProcessEnv(cmdEnv, "KIMI_CODE_HOME", runtimeHome)
	if effort := resolveKimiCLIEffort(cfg, t); effort != "" {
		cmdEnv = replaceProcessEnv(cmdEnv, "KIMI_MODEL_THINKING_EFFORT", effort)
	}
	runOnce := func() (string, string, error) {
		cmd := exec.CommandContext(runCtx, cfg.KimiCLIBin, args...)
		setupProcGroup(cmd)
		cmd.Dir = t.Dir
		cmd.Env = cmdEnv
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := runCmdRegisteredForTask(cmd, t.ID)
		return stdout.String(), stderr.String(), err
	}

	stdout, stderr, runErr := runOnce()
	combined := stdout + "\n" + stderr
	// Kimi Code 0.36.1 can exit once during first-use native worker materialization after emitting
	// only system.version. With no stderr, semantic event, session, or resumed context there is no
	// model/tool work to duplicate, so restart this transport exactly once inside the same Cardex
	// attempt. Any other shape keeps the original fail-closed behavior.
	if runErr != nil && runCtx.Err() == nil && t.SessionID == "" && strings.TrimSpace(stderr) == "" &&
		!policyFallbackCandidate(cfg, t, kimiCLIRunnerName) &&
		kimiCLI0361VersionOnly(stdout) {
		retryStdout, retryStderr, retryErr := runOnce()
		combined += "\n--- KIMI 0.36.1 COLD-START TRANSPORT RETRY ---\n" + retryStdout + "\n" + retryStderr
		stdout += "\n" + retryStdout
		stderr += "\n" + retryStderr
		runErr = retryErr
	}
	if runCtx.Err() == context.DeadlineExceeded {
		runErr = fmt.Errorf("步骤超时（%d 分钟）", cfg.StepTimeoutMin)
	}
	res := parseKimiCLIJSONL(providerJSONObservation(kimiCLIRunnerName, stdout, stderr))
	preserveKimiCLIProcessError(res, stderr, runErr)
	if res.IsError && runErr == nil {
		runErr = fmt.Errorf("Kimi CLI 返回错误: %s", summarizeResult(res.Result))
	}
	if strings.TrimSpace(res.Result) == "" && runErr == nil {
		runErr = fmt.Errorf("Kimi CLI 未返回最终文本")
	}
	return res, combined, runErr
}
