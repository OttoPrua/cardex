package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	if res == nil || res.IsError || runErr == nil || res.TerminalEvents != 0 {
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
	Role       string          `json:"role"`
	Type       string          `json:"type"`
	Version    string          `json:"version"`
	Command    string          `json:"command"`
	Content    json.RawMessage `json:"content"`
	SessionID  string          `json:"session_id"`
	Error      json.RawMessage `json:"error"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  json.RawMessage `json:"tool_calls"`
}

const (
	kimiCLISubtypeUnmatchedTool    = "kimi_cli_unmatched_tool_id"
	kimiCLISubtypeDuplicateTool    = "kimi_cli_duplicate_tool_id"
	kimiCLISubtypeMissingFinal     = "kimi_cli_missing_final"
	kimiCLISubtypeAfterHint        = "kimi_cli_semantic_after_hint"
	kimiCLISubtypeInvalidPostamble = "kimi_cli_invalid_completion_postamble"
	kimiCLISubtypeProcessExit      = "kimi_cli_process_exit"
	kimiCLISubtypeProcessSignal    = "kimi_cli_process_signal"
	kimiCLISubtypeProcessTimeout   = "kimi_cli_process_timeout"
	kimiCLISubtypeProcessFailure   = "kimi_cli_process_failure"
)

func kimiCLIContentText(raw json.RawMessage) string {
	text, _, _, _ := kimiCLIDecodeContent(raw)
	return text
}

func kimiCLIDecodeContent(raw json.RawMessage) (text string, structured bool, toolIDs []string, ok bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", false, nil, true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, false, nil, true
	}
	if raw[0] == '{' {
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil {
			return "", false, nil, false
		}
		var buf bytes.Buffer
		if json.Compact(&buf, raw) == nil {
			return buf.String(), true, nil, true
		}
		return string(raw), true, nil, true
	}
	if raw[0] != '[' {
		return "", false, nil, false
	}
	var blocks []struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		ID         string `json:"id"`
		ToolCallID string `json:"tool_call_id"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return "", false, nil, false
	}
	var texts []string
	for _, block := range blocks {
		typ := strings.ToLower(strings.TrimSpace(block.Type))
		if typ == "text" || typ == "" {
			if block.Text != "" {
				texts = append(texts, block.Text)
			}
			continue
		}
		if typ == "thinking" {
			continue // Reasoning is model work, never a deliverable final response.
		}
		if typ != "tool_use" && typ != "tool_call" {
			return "", false, nil, false
		}
		id := strings.TrimSpace(block.ID)
		if id == "" {
			id = strings.TrimSpace(block.ToolCallID)
		}
		if id == "" {
			return "", false, nil, false
		}
		toolIDs = append(toolIDs, id)
	}
	return strings.Join(texts, "\n"), false, toolIDs, true
}

func kimiCLIToolCallIDs(raw json.RawMessage) ([]string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, true
	}
	var calls []struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &calls) != nil {
		return nil, false
	}
	ids := make([]string, 0, len(calls))
	for _, call := range calls {
		id := strings.TrimSpace(call.ID)
		if id == "" {
			return nil, false
		}
		ids = append(ids, id)
	}
	return ids, true
}

func kimiCLINoteSubtype(res *claudeResult, subtype string) {
	if res.Subtype == "" {
		res.Subtype = subtype
	}
}

func kimiCLIResumeHintValid(ev kimiCLIEvent, content string, structured bool) bool {
	return ev.Role == "meta" && ev.Type == "session.resume_hint" &&
		strings.TrimSpace(ev.SessionID) != "" && strings.TrimSpace(ev.Command) != "" &&
		strings.TrimSpace(content) != "" && !structured
}

// Kimi 0.41's legacy agent-core stream closes an ordinary print with one
// session.resume_hint containing the resumable identity. Older observed
// streams did not have that postamble, so their compatibility path remains
// deliberately narrow instead of treating every assistant message as EOT.
func kimiCLICompletionContract(version, engine string) (requiresHint, known bool) {
	switch version {
	case "0.35.0", "0.36.1", "0.37.2":
		return false, engine == kimiEngineLegacy
	case "0.41", "0.41.0":
		return true, engine == kimiEngineLegacy
	default:
		return false, false
	}
}

func parseKimiCLIJSONL(raw string) *claudeResult {
	return parseKimiCLIJSONLForEngine(raw, kimiEngineLegacy)
}

func parseKimiCLIJSONLForEngine(raw, engine string) *claudeResult {
	res := &claudeResult{Type: "result", ObservationComplete: true}
	called := map[string]int{}
	resolved := map[string]int{}
	openTools := 0
	sawTool := false
	sawHint := false
	hintCount := 0
	validHintCount := 0
	version := ""
	versionCount := 0
	finalAfterTools := false
	finalOutput := ""
	finalMessage := false
	lastAssistant := ""
	s := bufio.NewScanner(strings.NewReader(raw))
	s.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		var ev kimiCLIEvent
		if json.Unmarshal([]byte(line), &ev) != nil {
			res.ObservationComplete = false
			continue
		}
		if ev.SessionID != "" {
			res.SessionID = ev.SessionID
		}
		text, structured, contentToolIDs, contentOK := kimiCLIDecodeContent(ev.Content)
		if !contentOK {
			res.ObservationComplete = false
		}
		role := strings.ToLower(strings.TrimSpace(ev.Role))
		typ := strings.ToLower(strings.TrimSpace(ev.Type))
		output := strings.TrimSpace(text)
		if output == "" && structured {
			output = text
		}
		if sawHint {
			res.ObservationComplete = false
			kimiCLINoteSubtype(res, kimiCLISubtypeAfterHint)
		}
		if role == "assistant" {
			// Only the current assistant message can own the final response. A later
			// missing, reasoning-only or tool message invalidates the earlier candidate.
			finalOutput, finalMessage, finalAfterTools = "", false, false
			if typ != "" && typ != "assistant" {
				res.ObservationComplete = false
			}
			// Any assistant event proves model work even when its content is reasoning/tool metadata
			// that this compatibility parser cannot render as text.
			res.SemanticEvents++
			res.ModelEvents++
			res.NumTurns++
			if sawHint {
				res.ObservationComplete = false
				kimiCLINoteSubtype(res, kimiCLISubtypeAfterHint)
			}
			callIDs, idsOK := kimiCLIToolCallIDs(ev.ToolCalls)
			if !idsOK {
				res.ObservationComplete = false
			}
			callIDs = append(callIDs, contentToolIDs...)
			if output != "" {
				lastAssistant = output
			}
			// A typed empty text message is still a final message; reasoning-only or
			// missing content is not. The version-specific postamble is checked below.
			if openTools == 0 && len(callIDs) == 0 && (output != "" || grokBuildJSONType(ev.Content) == "string") {
				finalOutput, finalMessage, finalAfterTools = output, true, true
			}
			for _, id := range callIDs {
				res.ToolEvents++
				if called[id] > 0 {
					res.ObservationComplete = false
					kimiCLINoteSubtype(res, kimiCLISubtypeDuplicateTool)
				}
				called[id]++
				if resolved[id] == 0 {
					openTools++
				}
				finalAfterTools = false
			}
		}
		if role != "assistant" && (role == "tool" || typ == "tool_result") {
			res.ToolEvents++
			sawTool = true
			finalAfterTools = false
			if sawHint {
				res.ObservationComplete = false
				kimiCLINoteSubtype(res, kimiCLISubtypeAfterHint)
			}
			id := strings.TrimSpace(ev.ToolCallID)
			if id == "" {
				res.ObservationComplete = false
				kimiCLINoteSubtype(res, kimiCLISubtypeUnmatchedTool)
			} else if called[id] == 0 {
				res.ObservationComplete = false
				kimiCLINoteSubtype(res, kimiCLISubtypeUnmatchedTool)
			} else if resolved[id] > 0 {
				res.ObservationComplete = false
				kimiCLINoteSubtype(res, kimiCLISubtypeDuplicateTool)
			} else {
				resolved[id]++
				if openTools > 0 {
					openTools--
				}
			}
		}
		if strings.Contains(role, "model") || strings.Contains(typ, "model.") {
			res.ModelEvents++
		}
		if typ == "error" || role == "error" {
			res.IsError = true
			if output != "" {
				res.Result = output
			} else if len(ev.Error) > 0 {
				res.Result = string(ev.Error)
			}
		}
		presemanticMeta := role == "meta" && (typ == "system.version" || typ == "system.init" || typ == "session.init" || typ == "session.resume_hint" || typ == "start")
		if role == "meta" && typ == "system.version" {
			if res.SemanticEvents > 0 || res.ToolEvents > 0 || sawHint {
				res.ObservationComplete = false
			}
			versionCount++
			if strings.TrimSpace(ev.Version) == "" {
				res.ObservationComplete = false
			} else if version == "" {
				version = strings.TrimSpace(ev.Version)
			} else if version != strings.TrimSpace(ev.Version) {
				res.ObservationComplete = false
			}
		}
		if role == "meta" && typ == "session.resume_hint" {
			hintCount++
			if kimiCLIResumeHintValid(ev, output, structured) {
				validHintCount++
			}
			sawHint = true
		}
		known := role == "assistant" || role == "tool" || role == "error" || presemanticMeta ||
			role == "system" || role == "user" || typ == "error" || typ == "tool_result"
		if !known {
			res.ObservationComplete = false
		}
	}
	if s.Err() != nil {
		res.ObservationComplete = false
	}
	// A completely observed typed error before model/tool work preserves the existing
	// quota policy. It is a failed invocation, never native successful completion.
	if res.IsError && res.ObservationComplete && res.SemanticEvents == 0 && res.ModelEvents == 0 && res.ToolEvents == 0 {
		res.Subtype = "kimi_cli_error"
		return res
	}
	if openTools > 0 {
		res.ObservationComplete = false
		kimiCLINoteSubtype(res, kimiCLISubtypeUnmatchedTool)
	}
	if versionCount != 1 {
		res.ObservationComplete = false
		kimiCLINoteSubtype(res, kimiCLISubtypeInvalidPostamble)
	}
	if _, known := kimiCLICompletionContract(version, engine); known {
		res.NativeVersion = version
	} else {
		res.NativeVersion = "unsupported"
	}
	if requiresHint, known := kimiCLICompletionContract(version, engine); !known {
		res.ObservationComplete = false
		kimiCLINoteSubtype(res, kimiCLISubtypeInvalidPostamble)
	} else if requiresHint && (hintCount != 1 || validHintCount != 1) {
		res.ObservationComplete = false
		kimiCLINoteSubtype(res, kimiCLISubtypeInvalidPostamble)
	}
	if !res.IsError && finalMessage && openTools == 0 && (!sawTool || finalAfterTools) && res.ObservationComplete {
		res.TerminalEvents = 1
		res.FinalReason = "final_assistant_eof"
		if version == "0.41" || version == "0.41.0" {
			res.FinalReason = "session.resume_hint"
		}
		res.Result = finalOutput
		return res
	}
	if res.Result == "" && lastAssistant != "" {
		res.Result = lastAssistant
	} else if res.Result == "" && finalOutput != "" {
		res.Result = finalOutput
	}
	if !res.IsError && res.TerminalEvents == 0 && (sawTool || openTools > 0) && res.Subtype == "" {
		kimiCLINoteSubtype(res, kimiCLISubtypeMissingFinal)
	}
	return res
}

func kimiCLIProcessFailureSubtype(runErr error) string {
	if runErr == nil {
		return ""
	}
	if errors.Is(runErr, context.DeadlineExceeded) || strings.Contains(strings.ToLower(runErr.Error()), "步骤超时") {
		return kimiCLISubtypeProcessTimeout
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		if exitErr.ExitCode() < 0 {
			return kimiCLISubtypeProcessSignal
		}
		return kimiCLISubtypeProcessExit
	}
	return kimiCLISubtypeProcessFailure
}

// kimiCLI0361VersionOnly reports the narrow 0.36.1 metadata-only exit: non-zero after
// system.version and before any assistant, tool, error, or resumable session event. A started
// child with this shape stays fail-closed; it must not restart transport or switch writers.
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

// Kimi 0.37.2 defaults to the v2 agent engine, whose recursive workspace watcher fails in this
// environment (EMFILE on large trees). The official compatibility selector is the child-process
// environment flag below; selecting the legacy agent-core engine restores the accepted 0.35/0.36
// JSON stream contract. Cardex never mutates the global shell, provider config, user config,
// credentials, or the third-party binary to achieve this — only the child process environment.
const (
	kimiLegacyEngineEnvFlag = "KIMI_CODE_LEGACY_FLAG"
	kimiEngineLegacy        = "legacy_agent_core"
	kimiEngineV2            = "agent_v2"
	kimiEngineCallerOpaque  = "caller_override"
)

// kimiChildEngineEnv resolves the deterministic engine selection for one Kimi child invocation.
// Cardex always requests the official legacy agent-core engine. An explicit caller-provided
// KIMI_CODE_LEGACY_FLAG is preserved untouched and reported symbolically; the raw environment
// value is never copied into task, route, event, or board readback.
func kimiChildEngineEnv(env []string) (requested, actual string, childEnv []string) {
	requested = kimiEngineLegacy
	prefix := kimiLegacyEngineEnvFlag + "="
	for _, item := range env {
		if value, ok := strings.CutPrefix(item, prefix); ok {
			switch strings.ToLower(strings.TrimSpace(value)) {
			case "1", "true":
				actual = kimiEngineLegacy
			case "0", "false":
				actual = kimiEngineV2
			default:
				actual = kimiEngineCallerOpaque
			}
			return requested, actual, env
		}
	}
	return requested, kimiEngineLegacy, replaceProcessEnv(env, kimiLegacyEngineEnvFlag, "1")
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
	userHome, _ := os.UserHomeDir()
	requestedEngine, actualEngine, cmdEnv := kimiChildEngineEnv(providerChildEnv(userHome, nil))
	cmdEnv = replaceProcessEnv(cmdEnv, "KIMI_CODE_NO_AUTO_UPDATE", "1")
	cmdEnv = replaceProcessEnv(cmdEnv, "KIMI_CODE_HOME", runtimeHome)
	if effort := resolveKimiCLIEffort(cfg, t); effort != "" {
		cmdEnv = replaceProcessEnv(cmdEnv, "KIMI_MODEL_THINKING_EFFORT", effort)
	}
	if t.LastRouteAttempt != nil {
		t.LastRouteAttempt.RequestedEngine = requestedEngine
		t.LastRouteAttempt.ActualEngine = actualEngine
	}
	cmd := exec.CommandContext(runCtx, cfg.KimiCLIBin, args...)
	setupProcGroup(cmd)
	cmd.Dir = t.Dir
	cmd.Env = cmdEnv
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdoutBuf, &stderrBuf
	runErr := runCmdRegisteredForTask(cmd, t.ID)
	stdout, stderr := stdoutBuf.String(), stderrBuf.String()
	combined := stdout + "\n" + stderr
	if runCtx.Err() == context.DeadlineExceeded {
		runErr = fmt.Errorf("步骤超时（%d 分钟）", cfg.StepTimeoutMin)
	}
	res := parseKimiCLIJSONLForEngine(providerJSONObservation(kimiCLIRunnerName, stdout, stderr), actualEngine)
	preserveKimiCLIProcessError(res, stderr, runErr)
	if res.IsError && runErr == nil {
		runErr = fmt.Errorf("Kimi CLI 返回错误: %s", summarizeResult(res.Result))
	}

	return res, combined, runErr
}
