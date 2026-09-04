package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const (
	cursorRunnerName   = "cursor"
	cursorCooldownName = "cursor"

	routeReasonCursorExplicit             = "cursor_explicit"
	routeReasonCursorFable                = "cursor_fable_preferred"
	routeReasonCursorFablePolicyWait      = "cursor_fable_policy_wait"
	routeReasonCursorFableFallbackPending = "cursor_fable_dual_fallback_pending"
	routeReasonCursorFableFallback        = "cursor_fable_dual_fallback"
)

func cursorVia(via string) bool { return via == cursorRunnerName }

func cursorEnabled(cfg *Config) bool {
	return cfg != nil && strings.TrimSpace(cfg.CursorBin) != ""
}

func cursorFableEnabled(cfg *Config) bool {
	return cursorEnabled(cfg) && cfg.CursorFable != nil && cfg.CursorFable.Enabled
}

func cursorFableAutoBaseEligible(cfg *Config, t *Task) bool {
	if t == nil || t.PreferRunner != "codex" || t.RemoteHost != "" || t.XRole != "" ||
		t.CodexModel != "" || t.XCodexModel != "" || t.SessionID != "" || t.MidStep ||
		t.Step != 0 || len(t.Prompts) != 1 {
		return false
	}
	return modelTierKeyword(cfg, t.Model) == "fable" && codexEligible(t)
}

func cursorFablePolicyApplies(cfg *Config, t *Task) bool {
	return cursorFableEnabled(cfg) && cursorFableAutoBaseEligible(cfg, t)
}

// cursorFableOwnerRouteRequired catches explicit Fable cards that belong to the
// owner auto-route but cannot yet be represented by its lossless single-prompt
// fallback chain. Such a card must wait instead of falling through to generic
// Codex and silently changing the primary model identity.
func cursorFableOwnerRouteRequired(cfg *Config, t *Task) bool {
	return cfg != nil && t != nil && strings.TrimSpace(t.Model) != "" &&
		modelTierKeyword(cfg, t.Model) == "fable" && ownerAutoRouteEligible(t)
}

func cursorReady(root string, cfg *Config, now time.Time) bool {
	if !cursorEnabled(cfg) {
		return false
	}
	cd := loadEngineCooldown(root, cursorCooldownName)
	return cd == nil || !cd.active(now)
}

func cursorPinnedReady(root string, cfg *Config, now time.Time) bool {
	return cursorReady(root, cfg, now)
}

func resolveCursorModel(cfg *Config, t *Task) string {
	if t != nil && strings.TrimSpace(t.CursorModel) != "" {
		return strings.TrimSpace(t.CursorModel)
	}
	if cfg == nil {
		return ""
	}
	if t != nil && t.PreferRunner != cursorRunnerName && cfg.CursorFable != nil &&
		modelTierKeyword(cfg, t.Model) == "fable" {
		if model := strings.TrimSpace(cfg.CursorFable.Model); model != "" {
			return model
		}
	}
	return strings.TrimSpace(cfg.CursorModel)
}

func cursorEffortFromModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	for _, effort := range []string{"xhigh", "max", "high", "medium", "low", "none"} {
		if strings.Contains(m, "-"+effort) || strings.Contains(m, "effort="+effort) {
			return effort
		}
	}
	return ""
}

type cursorUsage struct {
	InputTokens      int `json:"inputTokens"`
	OutputTokens     int `json:"outputTokens"`
	CacheReadTokens  int `json:"cacheReadTokens"`
	CacheWriteTokens int `json:"cacheWriteTokens"`
}

type cursorEvent struct {
	Type       string          `json:"type"`
	Subtype    string          `json:"subtype"`
	SessionID  string          `json:"session_id"`
	Result     string          `json:"result"`
	Error      string          `json:"error"`
	IsError    bool            `json:"is_error"`
	DurationMS int64           `json:"duration_ms"`
	Usage      *cursorUsage    `json:"usage"`
	Message    json.RawMessage `json:"message"`
}

func cursorMessageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		return strings.TrimSpace(plain)
	}
	var msg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return ""
	}
	var parts []string
	for _, item := range msg.Content {
		if item.Type == "text" && strings.TrimSpace(item.Text) != "" {
			parts = append(parts, item.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func cursorMessageToolEvents(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 {
		return 0, true
	}
	var msg struct {
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return 0, false
	}
	count := 0
	for _, item := range msg.Content {
		if strings.Contains(strings.ToLower(item.Type), "tool") {
			count++
		}
	}
	return count, true
}

func parseCursorJSONL(raw string) *claudeResult {
	res := &claudeResult{Type: "result", ObservationComplete: true}
	var assistant []string
	sawSemantic, sawResult := false, false
	s := bufio.NewScanner(strings.NewReader(raw))
	s.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		var ev cursorEvent
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Type == "" {
			res.ObservationComplete = false
			continue
		}
		if ev.SessionID != "" {
			res.SessionID = ev.SessionID
		}
		switch ev.Type {
		case "system":
			// Session/init metadata alone is presemantic.
		case "user":
			// A plain echoed input prompt is presemantic, but a user envelope may carry tool_result.
			toolEvents, complete := cursorMessageToolEvents(ev.Message)
			res.ToolEvents += toolEvents
			res.ObservationComplete = res.ObservationComplete && complete
		case "thinking", "reasoning":
			res.SemanticEvents++
			res.ModelEvents++
		case "assistant":
			res.SemanticEvents++
			res.ModelEvents++
			toolEvents, complete := cursorMessageToolEvents(ev.Message)
			res.ToolEvents += toolEvents
			res.ObservationComplete = res.ObservationComplete && complete
			if text := cursorMessageText(ev.Message); text != "" {
				assistant = append(assistant, text)
				sawSemantic = true
			}
		case "tool", "tool_use", "tool_result", "tool_call":
			res.ToolEvents++
		case "result":
			sawResult = true
			res.TerminalEvents++
			res.DurationMS = ev.DurationMS
			if ev.Usage != nil {
				res.Usage = &usageInfo{
					InputTokens: ev.Usage.InputTokens, OutputTokens: ev.Usage.OutputTokens,
					CacheReadInputTokens:     ev.Usage.CacheReadTokens,
					CacheCreationInputTokens: ev.Usage.CacheWriteTokens,
				}
				if ev.Usage.InputTokens != 0 || ev.Usage.OutputTokens != 0 || ev.Usage.CacheReadTokens != 0 || ev.Usage.CacheWriteTokens != 0 {
					res.ModelEvents++
				}
			}
			terminalError := ev.IsError || (ev.Subtype != "" && ev.Subtype != "success")
			if terminalError {
				res.IsError = true
				res.Subtype = "cursor_" + ev.Subtype
				if strings.TrimSpace(ev.Result) != "" {
					res.Result = strings.TrimSpace(ev.Result)
				}
			} else if strings.TrimSpace(ev.Result) != "" {
				res.Result = strings.TrimSpace(ev.Result)
				sawSemantic = true
				res.SemanticEvents++
				res.ModelEvents++
			}
		case "model", "model_start", "model_end":
			res.ModelEvents++
		case "error":
			res.IsError = true
			res.Subtype = "cursor_error"
			toolEvents, complete := cursorMessageToolEvents(ev.Message)
			res.ToolEvents += toolEvents
			res.ObservationComplete = res.ObservationComplete && complete
			res.Result = strings.TrimSpace(ev.Error)
			if res.Result == "" {
				res.Result = cursorMessageText(ev.Message)
			}
		default:
			if strings.Contains(strings.ToLower(ev.Type), "tool") {
				res.ToolEvents++
			} else {
				res.ObservationComplete = false
			}
		}
	}
	if s.Err() != nil {
		res.ObservationComplete = false
	}
	if res.Result == "" && len(assistant) > 0 {
		res.Result = strings.TrimSpace(strings.Join(assistant, "\n"))
	}
	if sawSemantic && res.NumTurns == 0 {
		res.NumTurns = 1
	}
	if !res.IsError && (!sawResult || !sawSemantic) {
		res.IsError = true
		res.Subtype = "cursor_stream_incomplete"
		res.Result = "Cursor 流缺少语义终局 result 事件"
	}
	return res
}

func invokeCursor(ctx context.Context, cfg *Config, t *Task, prompt string) (*claudeResult, string, error) {
	if !cursorEnabled(cfg) {
		return nil, "", fmt.Errorf("cursor_bin 未配置")
	}
	model := resolveCursorModel(cfg, t)
	if model == "" {
		return nil, "", fmt.Errorf("未解析出 Cursor 模型")
	}
	args := []string{
		"--print", "--trust", "--output-format", "stream-json",
		"--model", model, "--workspace", t.Dir, "--sandbox", "enabled",
	}
	fableReadOnly := cfg.OwnerRoutingEnforced &&
		(t.OwnerRouteName == "fable_explicit" || modelTierKeyword(cfg, t.Model) == "fable")
	if t.Type != typeSequence || fableReadOnly {
		args = append(args, "--mode", "ask")
	}
	if t.SessionID != "" {
		args = append(args, "--resume", t.SessionID)
	}
	// Cursor 2026.08.11 没有 prompt-file 参数；exec.Command 直接传 argv，不经 shell 展开。
	// --auto-review / --force / --yolo 均不启用，保留 Cardex 既有权限边界。
	args = append(args, prompt)
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.StepTimeoutMin)*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(runCtx, cfg.CursorBin, args...)
	setupProcGroup(cmd)
	cmd.Dir = t.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := runCmdRegisteredForTask(cmd, t.ID)
	if runCtx.Err() == context.DeadlineExceeded {
		runErr = fmt.Errorf("步骤超时（%d 分钟）", cfg.StepTimeoutMin)
	}
	combined := stdout.String() + "\n" + stderr.String()
	res := parseCursorJSONL(providerJSONObservation(cursorRunnerName, stdout.String(), stderr.String()))
	if runErr != nil {
		if res == nil {
			res = &claudeResult{Type: "result"}
		}
		res.IsError = true
		res.Subtype = "cursor_process_error"
		if line := firstLine(strings.TrimSpace(stderr.String())); line != "" {
			res.Result = line
		} else if strings.TrimSpace(res.Result) == "" {
			res.Result = runErr.Error()
		}
	}
	if res != nil && res.IsError && runErr == nil {
		runErr = fmt.Errorf("Cursor 返回错误: %s", summarizeResult(res.Result))
	}
	if res != nil && !res.IsError && strings.TrimSpace(res.Result) == "" && runErr == nil {
		runErr = fmt.Errorf("Cursor 未返回最终文本")
	}
	return res, combined, runErr
}

var cursorQuotaRe = regexp.MustCompile(`(?i)(?:^|[^0-9])429(?:[^0-9]|$)|too many requests|rate limit|usage limit|quota (?:exceeded|exhausted)|insufficient (?:quota|credits)|out of (?:credits|usage)|额度(?:不足|已用完)|配额(?:不足|已用尽)|限额(?:不足|已用尽)`)
var cursorPolicyRe = regexp.MustCompile(`(?i)actionrequired(?:error)?:.*review data policy|acknowledge .*data retention policy|review data policy`)

func cursorErrorScanText(res *claudeResult, combined string) string {
	var parts []string
	for _, line := range strings.Split(combined, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev cursorEvent
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Type != "" {
			if ev.Type == "error" || (ev.Type == "result" && ev.IsError) {
				parts = append(parts, ev.Error, ev.Result, cursorMessageText(ev.Message))
			}
			continue // user/assistant/thinking/system prose is never an error signal
		}
		parts = append(parts, line) // stderr / pre-stream CLI failure
	}
	if res != nil && res.IsError && strings.TrimSpace(res.Result) != "" {
		parts = append(parts, res.Result)
	}
	return strings.Join(parts, "\n")
}

func isLimitHitCursor(res *claudeResult, combined string) bool {
	if res != nil && !res.IsError && strings.TrimSpace(res.Result) != "" {
		return false
	}
	return cursorQuotaRe.MatchString(cursorErrorScanText(res, combined))
}

func isCursorDataPolicyGate(res *claudeResult, combined string) bool {
	if res != nil && !res.IsError && strings.TrimSpace(res.Result) != "" {
		return false
	}
	return cursorPolicyRe.MatchString(cursorErrorScanText(res, combined))
}

func cursorResetEpoch(cfg *Config, res *claudeResult, combined string, now time.Time) int64 {
	fallback := 0
	if cfg != nil && cfg.CursorFable != nil {
		fallback = cfg.CursorFable.LimitFallbackMin
	}
	scan := cursorErrorScanText(res, combined)
	return engineResetEpoch(combined+"\n"+resultText(res), scan, cfg, EngineProfile{LimitFallbackMin: fallback}, now)
}

// prepareCursorFableFallback 原子地把尚未产生语义工作的 Fable 母卡改造成交叉 A 卡。
// A/B/C 的引擎规格同时冻结；任何校验或落盘失败都不改调用方内存，避免半条链。
func prepareCursorFableFallback(root string, cfg *Config, t *Task, reason string, kind fallbackFailureKind, auth fallbackAuthorization) error {
	if !auth.verified || auth.beforeDigest == "" || auth.beforeDigest != auth.afterDigest {
		return fmt.Errorf("Cursor Fable fallback refused without verified serial authorization")
	}
	if !fableFallbackKindEligible(kind) {
		return fmt.Errorf("Cursor Fable fallback accepts only confirmed quota or proven presemantic triggers; %s must remain held", kind)
	}
	if cfg == nil || cfg.CursorFable == nil || t == nil {
		return fmt.Errorf("Cursor Fable 回退配置不完整")
	}
	if t.Step != 0 || len(t.Prompts) != 1 || t.SessionID != "" || t.MidStep {
		return fmt.Errorf("Cursor Fable 回退只允许首个语义事件前的 fresh 单步卡")
	}
	name := strings.TrimSpace(cfg.CursorFable.FallbackProfile)
	prof, ok := cfg.CrossProfiles[name]
	if !ok {
		return fmt.Errorf("Cursor Fable 回退 profile %q 不存在", name)
	}
	tpl, err := loadTemplate(root, "crosscheck-solo")
	if err != nil {
		return err
	}
	originalTask := t.Prompts[0]
	next := *t
	// `next := *t` shallow-copies the reply-route pointer, so both the staged card and
	// the caller's card would share one struct. The route must survive the fallback
	// (the requester still owns this work) but must not be aliased across cards.
	next.ReplyRoute = inheritTaskReplyRoute(t.ReplyRoute)
	next.Prompts = []string{renderTemplate(tpl, map[string]string{"TASK": originalTask})}
	next.Type = typeCrossCheck
	next.SkipPermissions = false
	next.Title = "交叉A[" + name + "]: " + crossBase(t.Title)
	next.XRole = "A"
	next.XKey = newCrossKey()
	next.XProfile = name
	next.XTask = originalTask
	next.XEngineB = nil
	next.XEngineC = nil
	next.SessionID = ""
	next.Step = 0
	next.Runner = ""
	next.Status = statusQueued
	next.NotBeforeEpoch = 0
	next.ResumeAtEpoch = 0
	next.Attempts = 0
	next.MidStep = false
	next.ReviewAfter = false
	next.EmitTasks = false
	next.EmitProgress = false
	next.AdvisoryReview = false
	next.FableFirstPrinciplesReview = false
	next.FableReviewerMerger = false
	next.RouteClass = routeClassGeneral
	next.RiskClass = riskClassOrdinary
	next.OwnerRouteName = "fable_explicit"
	next.OwnerRouteLeg = 2
	next.OwnerRouteStage = routeStageFableAnswer
	next.RequiredReviews = nil
	next.CompletedReviews = nil
	var reviewErr error
	next.RequiredReviews, reviewErr = appendClosedReview(next.RequiredReviews, reviewStageFableSolUltra)
	if reviewErr != nil {
		return reviewErr
	}
	next.LastError = "Cursor Fable 已确认 quota 或 eligible presemantic trigger，转 Grok/xhigh 独立只读答案，再由单次 fresh Sol/ultra 对抗合并并直接终局: " + reason
	if err := applyCrossEngine(&next, prof.A, cfg); err != nil {
		return fmt.Errorf("回退引擎甲(%s): %w", crossEngineLabel(prof.A), err)
	}
	if prof.A.Kind == "codex" || prof.A.Kind == "remote-codex" {
		next.XCodexModel = cfg.CodexModel
	}
	frozenB, err := freezeCrossEngine(prof.B, cfg)
	if err != nil {
		return fmt.Errorf("回退引擎乙(%s): %w", crossEngineLabel(prof.B), err)
	}
	next.XEngineB = frozenB
	if prof.Merge != nil {
		return fmt.Errorf("Cursor Fable 回退 profile %q 必须移除第三 Sol/max merge 腿", name)
	}
	next.XEngineC = nil
	next.RouteReason = routeReasonCursorFableFallbackPending
	next.touch()
	if err := saveTask(root, &next); err != nil {
		return err
	}
	*t = next
	detail := map[string]any{
		"reason": "cursor_fable_quota_fallback", "profile": name, "x_key": t.XKey,
		"detail": reason, "failure_kind": kind, "workspace_fingerprint_before": auth.beforeDigest,
		"workspace_fingerprint_after": auth.afterDigest, "semantic_events": 0,
		"model_events": 0, "tool_events": 0, "process_residue": false,
	}
	return persistTaskEvent(root, t, evRetry, "runner:cursor", statusQueued, 0, detail)
}

func validateCursor(cfg *Config) error {
	if cfg == nil {
		return nil
	}
	cfg.CursorBin = strings.TrimSpace(cfg.CursorBin)
	cfg.CursorModel = strings.TrimSpace(cfg.CursorModel)
	r := cfg.CursorFable
	if r == nil || !r.Enabled {
		return nil
	}
	if cfg.CursorBin == "" {
		return fmt.Errorf("cursor_fable.enabled=true 需要配置 cursor_bin")
	}
	r.Model = strings.TrimSpace(r.Model)
	if strings.ToLower(r.Model) != "claude-fable-5-thinking-max" {
		return fmt.Errorf("cursor_fable.model 必须严格使用 claude-fable-5-thinking-max")
	}
	if r.LimitFallbackMin < 0 {
		return fmt.Errorf("cursor_fable.limit_fallback_min 不能为负数")
	}
	r.FallbackProfile = strings.TrimSpace(r.FallbackProfile)
	prof, ok := cfg.CrossProfiles[r.FallbackProfile]
	if !ok || r.FallbackProfile == "" {
		return fmt.Errorf("cursor_fable.fallback_profile %q 不存在", r.FallbackProfile)
	}
	if prof.A.Kind != grokBuildRunnerName || strings.ToLower(strings.TrimSpace(prof.A.Effort)) != "xhigh" {
		return fmt.Errorf("Cursor Fable 回退 profile 的 A 必须是 grok-build/xhigh")
	}
	if leg := crossPolicyLeg(cfg, prof.A); leg.Model != "grok-4.6" {
		return fmt.Errorf("Cursor Fable 回退 profile 的 A 必须严格使用 grok-4.6/xhigh")
	}
	if prof.B.Kind != "codex" || strings.ToLower(strings.TrimSpace(prof.B.Effort)) != "ultra" {
		return fmt.Errorf("Cursor Fable 回退 profile 的 B 必须是 codex Sol/ultra")
	}
	if strings.TrimSpace(prof.B.Model) != "" {
		return fmt.Errorf("Cursor Fable 回退 profile 的 B.model 必须留空；Codex 模型由 codex_model 冻结")
	}
	if leg := crossPolicyLeg(cfg, prof.B); leg.Model != "gpt-5.6-sol" {
		return fmt.Errorf("Cursor Fable 回退 profile 的 B 必须通过 codex_model 严格使用 gpt-5.6-sol/ultra")
	}
	if prof.Merge != nil {
		return fmt.Errorf("Cursor Fable 回退 profile 不得配置第三 Sol/max merge；Sol/ultra reviewer-merger 直接终局")
	}
	// Exact names are not enough: every frozen leg must be executable under the loaded config.
	// Reject a broken chain before Cursor Fable can run, rather than discovering a missing Grok/Codex
	// binary only after the primary has safely failed and the task is already transitioning.
	for _, leg := range []struct {
		name string
		eng  CrossEngine
	}{
		{"A", prof.A},
		{"reviewer-merger", prof.B},
	} {
		if _, err := freezeCrossEngine(leg.eng, cfg); err != nil {
			return fmt.Errorf("Cursor Fable 回退 profile 的 %s 不可执行: %w", leg.name, err)
		}
	}
	return nil
}
