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
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	grokBuildRunnerName         = "grok-build"
	grokBuildCooldownName       = "grok-build"
	grokBuildAuthCooldownPrefix = "auth: "

	routeReasonGrokExplicit            = "grok_build_explicit"
	routeReasonKimiToGrokPending       = "kimi_to_grok_pending"
	routeReasonKimiToGrok              = "kimi_to_grok"
	routeReasonFableToGrokPending      = "fable_to_grok_pending"
	routeReasonFableToGrok             = "fable_to_grok"
	routeReasonFableToSolPending       = "fable_grok_unavailable_to_sol_pending"
	routeReasonFableToSol              = "fable_grok_unavailable_to_sol"
	routeReasonGrokToSolPending        = "grok_to_sol_pending"
	routeReasonGrokToSol               = "grok_to_sol"
	routeReasonGrokToKimiPending       = "grok_to_kimi_pending"
	routeReasonGrokToKimi              = "grok_to_kimi"
	routeReasonKimiToSolPending        = "kimi_to_sol_pending"
	routeReasonKimiToSol               = "kimi_to_sol"
	routeReasonGrokOpusGeneral         = "grok_opus_general_preferred"
	routeReasonGrokOpusBackend         = "grok_opus_backend_preferred"
	routeReasonGrokSonnet              = "grok_sonnet_preferred"
	routeReasonGrokHaiku               = "grok_haiku_preferred"
	routeReasonGrokSonnetToLunaPending = "grok_sonnet_to_luna_pending"
	routeReasonGrokSonnetToLuna        = "grok_sonnet_to_luna"
	routeReasonGrokHaikuToLunaPending  = "grok_haiku_to_luna_pending"
	routeReasonGrokHaikuToLuna         = "grok_haiku_to_luna"
)

const (
	grokBuildAuthCooldownDuration = 24 * time.Hour
	grokBuildAuthProbeTimeout     = 20 * time.Second
)

// grokBuildAuthProbeMu makes the credential check and breaker transition atomic across workers in
// one tick. Once the first probe proves expired credentials, later workers observe the persisted
// circuit instead of each starting another Grok process and multiplying one incident into N retries.
var grokBuildAuthProbeMu sync.Mutex

type grokBuildAuthProbeError struct {
	reason      string
	circuitOpen bool
}

func (e *grokBuildAuthProbeError) Error() string {
	if e == nil {
		return "Grok Build 登录预检失败"
	}
	prefix := "Grok Build 登录预检失败（未启动模型任务）: "
	if e.circuitOpen {
		prefix = "Grok Build 认证熔断中（未启动模型任务）: "
	}
	return prefix + e.reason
}

func isGrokBuildAuthProbeError(err error) bool {
	var target *grokBuildAuthProbeError
	return errors.As(err, &target)
}

func isGrokBuildAuthCircuitError(err error) bool {
	var target *grokBuildAuthProbeError
	return errors.As(err, &target) && target.circuitOpen
}

func grokBuildExactAuthResult(res *claudeResult) bool {
	if res == nil || !res.ObservationComplete || res.SemanticEvents != 0 ||
		res.ModelEvents != 0 || res.ToolEvents != 0 {
		return false
	}
	return res.Subtype == "grok_build_auth_preflight" || res.Subtype == "grok_build_process_auth_exact"
}

func grokBuildAuthCooldownActive(cd *Cooldown, now time.Time) bool {
	return cd != nil && cd.active(now) && strings.HasPrefix(cd.Reason, grokBuildAuthCooldownPrefix)
}

func grokBuildAuthReason(cd *Cooldown) string {
	if cd == nil {
		return "登录态无效或已过期"
	}
	reason := strings.TrimSpace(strings.TrimPrefix(cd.Reason, grokBuildAuthCooldownPrefix))
	if reason == "" {
		return "登录态无效或已过期"
	}
	return reason
}

func canonicalGrokBuildAuthReason(scan string) string {
	lower := strings.ToLower(scan)
	switch {
	case strings.Contains(lower, "unauthorized (401)") && strings.Contains(lower, "invalid or expired credentials"):
		return "Unauthorized (401): Invalid or expired credentials"
	case strings.Contains(lower, "401"):
		return "Unauthorized (401): credentials invalid or expired"
	default:
		return "登录态无效或已过期"
	}
}

var (
	grokBuildBearerSecretRe     = regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._~+/=-]+`)
	grokBuildAuthorizationRe    = regexp.MustCompile(`(?i)\bauthorization\b\s*[:=]\s*[^,\s)]+`)
	grokBuildSecretAssignmentRe = regexp.MustCompile(`(?i)\b(access[_-]?token|refresh[_-]?token|api[_-]?key|x-api-key)\b\s*[:=]\s*[^,\s)]+`)
	grokBuildAuthEnvelopeRe     = regexp.MustCompile(`(?i)^(?:(?:http(?:/\d(?:\.\d)?)?\s+)?401\s+unauthorized|unauthorized\s*\(\s*401\s*\)(?:\s+from\s+https://[a-z0-9._~:/?#\[\]@!$&'*+,;=%-]+)?)\s*:\s*invalid or expired credentials\s*(.*)$`)
	grokBuildTrustedAuthLineRe  = regexp.MustCompile(`(?i)^(?:(?:http(?:/\d(?:\.\d)?)?\s+)?401\s+unauthorized|unauthorized\s*\(\s*401\s*\))(?:\s*:\s*(?:invalid or expired credentials|invalid api key|credentials invalid or expired))?(?:\s*;\s*[a-z_]+\s*=\s*[a-z0-9 _-]+)*$`)
)

const (
	grokBuildExactBareAuthDiagnostic = "HTTP 401 Unauthorized: Invalid or expired credentials; auth_kind=none; upstream=Unauthenticated; reason=no auth context"
	grokBuildExactQuotedAuthBody     = "Unauthorized (401) from https://cli-chat-proxy.grok.com/v1/responses: Invalid or expired credentials (auth_kind=none, x_xai_token_auth=xai-grok-cli, upstream=Unauthenticated, reason=no auth context)"
)

// Only the two process bodies above were observed and authorized for the engine-wide circuit.
// Context still matters: grokBuildAuthDiagnosticLine admits the first only as a bare one-line
// stderr and the second only inside the exact quoted wrapper and closed footer.

func grokBuildTrustedAuthDiagnosticLine(stderr string) string {
	// The same closed physical-line discipline as the circuit-opening grammar: a process's single
	// trailing newline is tolerated, but every further blank physical line is an extra line that
	// keeps the observation from being promoted into authentication evidence.
	lines := strings.Split(stderr, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	if len(lines) != 1 {
		return ""
	}
	diagnostic := strings.TrimSpace(lines[0])
	if diagnostic == "" {
		return ""
	}
	if grokBuildTrustedAuthLineRe.MatchString(diagnostic) || grokBuildAuthEnvelopeRe.MatchString(diagnostic) {
		return diagnostic
	}
	return ""
}

func safeGrokBuildProbeDiagnostic(raw string, runErr error) string {
	line := firstLine(strings.TrimSpace(raw))
	if line == "" && runErr != nil {
		line = firstLine(runErr.Error())
	}
	line = grokBuildBearerSecretRe.ReplaceAllString(line, "Bearer <redacted>")
	line = grokBuildAuthorizationRe.ReplaceAllString(line, "Authorization=<redacted>")
	line = grokBuildSecretAssignmentRe.ReplaceAllString(line, "$1=<redacted>")
	if len(line) > 400 {
		line = line[:400] + "…"
	}
	if line == "" {
		line = "unknown preflight failure"
	}
	return line
}

// runGrokBuildAuthProbe asks the CLI for its model list. This reaches the CLI's authenticated
// control path without opening a model session, sending a task prompt, or mutating a product repo.
func runGrokBuildAuthProbe(ctx context.Context, cfg *Config, model string) error {
	if !grokBuildEnabled(cfg) {
		return fmt.Errorf("grok_build_bin/grok_build 未启用")
	}
	probeCtx, cancel := context.WithTimeout(ctx, grokBuildAuthProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, cfg.GrokBuildBin, "--no-auto-update", "models")
	setupProcGroup(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := runCmdRegistered(cmd)
	if probeCtx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("Grok Build 登录预检超时（%s）", grokBuildAuthProbeTimeout)
	}
	scan := stdout.String() + "\n" + stderr.String()
	if runErr != nil {
		if authLine := grokBuildAuthDiagnosticLine(stderr.String()); authLine != "" {
			return &grokBuildAuthProbeError{reason: canonicalGrokBuildAuthReason(authLine)}
		}
		return fmt.Errorf("Grok Build 登录预检失败（未启动模型任务）: %s",
			safeGrokBuildProbeDiagnostic(stderr.String(), runErr))
	}
	if model != "" && !strings.Contains(strings.ToLower(scan), strings.ToLower(model)) {
		return fmt.Errorf("Grok Build 模型清单缺 %s（未启动模型任务）", model)
	}
	return nil
}

func setGrokBuildAuthCooldown(root, reason string, now time.Time) {
	reason = strings.TrimSpace(strings.TrimPrefix(reason, grokBuildAuthCooldownPrefix))
	if reason == "" {
		reason = "登录态无效或已过期"
	}
	setEngineCooldown(root, grokBuildCooldownName, now.Add(grokBuildAuthCooldownDuration).Unix(),
		grokBuildAuthCooldownPrefix+reason)
}

// ensureGrokBuildAuth is the per-job fail-closed gate. The mutex plus persisted cooldown means one
// expired login produces one real probe; followers remain on the Grok leg and never become fallback
// writers. A successful probe does not clear quota cooldowns.
func ensureGrokBuildAuth(ctx context.Context, root string, cfg *Config, model string) error {
	grokBuildAuthProbeMu.Lock()
	defer grokBuildAuthProbeMu.Unlock()
	now := time.Now()
	if cd := loadEngineCooldown(root, grokBuildCooldownName); grokBuildAuthCooldownActive(cd, now) {
		return &grokBuildAuthProbeError{reason: grokBuildAuthReason(cd), circuitOpen: true}
	}
	err := runGrokBuildAuthProbe(ctx, cfg, model)
	if isGrokBuildAuthProbeError(err) {
		var authErr *grokBuildAuthProbeError
		_ = errors.As(err, &authErr)
		setGrokBuildAuthCooldown(root, authErr.reason, now)
	}
	return err
}

// refreshGrokBuildAuth is used by doctor after a human login. It bypasses only an auth circuit,
// performs a fresh no-model probe, and clears that circuit on success. Quota cooldowns are preserved.
func refreshGrokBuildAuth(ctx context.Context, root string, cfg *Config, model string) error {
	grokBuildAuthProbeMu.Lock()
	defer grokBuildAuthProbeMu.Unlock()
	now := time.Now()
	err := runGrokBuildAuthProbe(ctx, cfg, model)
	if err != nil {
		if isGrokBuildAuthProbeError(err) {
			var authErr *grokBuildAuthProbeError
			_ = errors.As(err, &authErr)
			setGrokBuildAuthCooldown(root, authErr.reason, now)
		}
		return err
	}
	if cd := loadEngineCooldown(root, grokBuildCooldownName); grokBuildAuthCooldownActive(cd, now) {
		clearEngineCooldown(root, grokBuildCooldownName)
	}
	return nil
}

func grokBuildVia(via string) bool { return via == grokBuildRunnerName }

func grokBuildEnabled(cfg *Config) bool {
	return cfg != nil && cfg.GrokBuild != nil && cfg.GrokBuild.Enabled &&
		strings.TrimSpace(cfg.GrokBuildBin) != ""
}

func resolveGrokBuildModel(cfg *Config, t *Task) string {
	if t != nil && strings.TrimSpace(t.GrokModel) != "" {
		return strings.TrimSpace(t.GrokModel)
	}
	if cfg == nil || cfg.GrokBuild == nil {
		return ""
	}
	return strings.TrimSpace(cfg.GrokBuild.Model)
}

func resolveGrokBuildEffort(cfg *Config, t *Task) string {
	if t != nil && strings.TrimSpace(t.GrokEffort) != "" {
		return strings.ToLower(strings.TrimSpace(t.GrokEffort))
	}
	if cfg == nil || cfg.GrokBuild == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(cfg.GrokBuild.Effort))
}

func grokBuildTierRoute(cfg *Config, t *Task) (string, GrokTierRoute, bool) {
	if !grokBuildEnabled(cfg) || t == nil || cfg.GrokBuild.TierRoutes == nil {
		return "", GrokTierRoute{}, false
	}
	tier := modelTierKeyword(cfg, t.Model)
	key := tier
	if tier == "opus" {
		if !backendDevelopmentTask(t) {
			return "", GrokTierRoute{}, false
		}
		key = "opus_backend"
	}
	if key != "sonnet" && key != "haiku" && key != "opus_backend" {
		return "", GrokTierRoute{}, false
	}
	route, ok := cfg.GrokBuild.TierRoutes[key]
	return key, route, ok
}

// grokBuildAutoRouteApplies 是旧的单阶 Grok 兼容判定；生产六行路由
// resolveOwnerRoute 统一处理，包括非后端 Opus 的 Grok 第一腿。
func grokBuildAutoRouteApplies(cfg *Config, t *Task) bool {
	if t == nil || t.PreferRunner != "codex" || t.RemoteHost != "" || t.XRole != "" ||
		t.CodexModel != "" || t.XCodexModel != "" || !codexEligible(t) {
		return false
	}
	_, _, ok := grokBuildTierRoute(cfg, t)
	return ok
}

func grokAutoRouteReason(key string) string {
	switch key {
	case "opus_backend":
		return routeReasonGrokOpusBackend
	case "sonnet":
		return routeReasonGrokSonnet
	case "haiku":
		return routeReasonGrokHaiku
	default:
		return ""
	}
}

func ensureGrokOpusAdversarialReview(cfg *Config, t *Task) {
	if cfg == nil || cfg.GrokBuild == nil || !cfg.GrokBuild.OpusAdversarialReview || t == nil ||
		modelTierKeyword(cfg, t.Model) != "opus" {
		return
	}
	t.ReviewAfter = true
	t.SolMaxAdversarialReview = true
	enforceReviewAfterEligibility(t)
}

// pinGrokOpusAdversarialReview 把 Grok Opus 实现的自动审核钉到独立 Codex Sol/max。
// review_after 模板仍负责对抗式方法与修复闭环；这里只冻结模型身份，防审核卡再次被
// Opus 的 Kimi/Grok 主路由接走而形成同模型自审。
func pinGrokOpusAdversarialReview(cfg *Config, parent, review *Task) {
	if parent == nil || review == nil || !parent.ReviewAfter || !parent.SolMaxAdversarialReview {
		return
	}
	review.PreferRunner = "codex"
	review.RunnerExplicit = true
	review.CodexModel = "gpt-5.6-sol"
	review.Effort = "max"
	review.EffortExplicit = true
	review.SessionID = ""
	review.MidStep = false
}

func pinGrokBuildAutoRoute(cfg *Config, t *Task) bool {
	key, route, ok := grokBuildTierRoute(cfg, t)
	if !ok {
		return false
	}
	pinGrokBuild(cfg, t, grokAutoRouteReason(key))
	t.GrokEffort = route.Effort
	return true
}

func grokBuildReady(root string, cfg *Config, now time.Time) bool {
	if !grokBuildEnabled(cfg) {
		return false
	}
	cd := loadEngineCooldown(root, grokBuildCooldownName)
	return cd == nil || !cd.active(now)
}

func grokBuildPinnedReady(root string, cfg *Config, now time.Time) bool {
	return grokBuildReady(root, cfg, now)
}

func grokBuildKimiFallbackEnabled(cfg *Config) bool {
	return grokBuildEnabled(cfg) && cfg.GrokBuild.KimiOpusFallback
}

func pinGrokBuild(cfg *Config, t *Task, routeReason string) {
	if t == nil {
		return
	}
	if route, ok := resolveOwnerRoute(cfg, t); ok {
		for i, leg := range route.Legs {
			if leg.Runner == grokBuildRunnerName {
				t.OwnerRouteName = route.Name
				t.OwnerRouteLeg = i + 1
				break
			}
		}
	}
	t.PreferRunner = grokBuildRunnerName
	t.RouteReason = routeReason
	if cfg == nil || cfg.GrokBuild == nil {
		return
	}
	if model := strings.TrimSpace(cfg.GrokBuild.Model); model != "" {
		t.GrokModel = model
	}
	if effort := strings.ToLower(strings.TrimSpace(cfg.GrokBuild.Effort)); effort != "" {
		t.GrokEffort = effort
	}
	ensureGrokOpusAdversarialReview(cfg, t)
}

func grokCodexFallbackModel(cfg *Config) string {
	if cfg != nil && cfg.GrokBuild != nil {
		if model := strings.TrimSpace(cfg.GrokBuild.CodexFallbackModel); model != "" {
			return model
		}
	}
	if cfg != nil {
		if model := strings.TrimSpace(cfg.CodexTierModels["fable"]); model != "" {
			return model
		}
	}
	return "gpt-5.6-sol"
}

func grokCodexFallbackEffort(cfg *Config) string {
	if cfg != nil && cfg.GrokBuild != nil {
		if effort := strings.ToLower(strings.TrimSpace(cfg.GrokBuild.CodexFallbackEffort)); effort != "" {
			return effort
		}
	}
	if cfg != nil {
		if effort := strings.TrimSpace(cfg.CodexTierReasoning["fable"]); effort != "" {
			return effort
		}
	}
	return "max"
}

func pinGrokBuildCodexFallback(cfg *Config, t *Task) {
	if t == nil {
		return
	}
	origin := t.RouteReason
	t.PreferRunner = "codex"
	t.CodexModel = grokCodexFallbackModel(cfg)
	t.Effort = grokCodexFallbackEffort(cfg)
	if cfg != nil && cfg.GrokBuild != nil {
		key := ""
		switch origin {
		case routeReasonGrokSonnet:
			key = "sonnet"
		case routeReasonGrokHaiku:
			key = "haiku"
		case routeReasonGrokOpusBackend:
			key = "opus_backend"
		}
		if route, ok := cfg.GrokBuild.TierRoutes[key]; ok {
			t.CodexModel = strings.TrimSpace(route.CodexFallbackModel)
			t.Effort = strings.ToLower(strings.TrimSpace(route.CodexFallbackEffort))
		}
	}
	t.EffortExplicit = true
	switch origin {
	case routeReasonGrokSonnet:
		t.RouteReason = routeReasonGrokSonnetToLunaPending
	case routeReasonGrokHaiku:
		t.RouteReason = routeReasonGrokHaikuToLunaPending
	default:
		t.RouteReason = routeReasonGrokToSolPending
	}
	t.SessionID = ""
	t.MidStep = false
	t.ResumeAtEpoch = 0
}

func grokBuildPolicyFallbackTask(t *Task) bool {
	if t == nil {
		return false
	}
	switch t.RouteReason {
	case routeReasonKimiToGrokPending, routeReasonKimiToGrok,
		routeReasonFableToGrokPending, routeReasonFableToGrok,
		routeReasonGrokOpusBackend, routeReasonGrokSonnet, routeReasonGrokHaiku:
		return true
	default:
		return false
	}
}

type grokBuildUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

type grokBuildEvent struct {
	Type         string          `json:"type"`
	Data         string          `json:"data"`
	Message      string          `json:"message"`
	StopReason   string          `json:"stopReason"`
	SessionID    string          `json:"sessionId"`
	NumTurns     int             `json:"num_turns"`
	TotalCostUSD float64         `json:"total_cost_usd"`
	DurationMS   int64           `json:"duration_ms"`
	Usage        *grokBuildUsage `json:"usage"`
}

func grokBuildJSONType(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	switch raw[0] {
	case '"':
		return "string"
	case '{':
		return "object"
	case '[':
		return "array"
	case 'n':
		return "null"
	case 't', 'f':
		return "boolean"
	default:
		return "number"
	}
}

func grokBuildExactShape(fields map[string]json.RawMessage, keyTypes ...string) bool {
	if len(keyTypes)%2 != 0 || len(fields) != len(keyTypes)/2 {
		return false
	}
	for i := 0; i < len(keyTypes); i += 2 {
		raw, ok := fields[keyTypes[i]]
		if !ok || grokBuildJSONType(raw) != keyTypes[i+1] {
			return false
		}
	}
	return true
}

func grokBuildEndForbiddenField(fields map[string]json.RawMessage) bool {
	if grokBuildHasContent(fields) {
		return true
	}
	for _, key := range []string{"kind", "locations", "title", "toolCallId", "toolName", "tools"} {
		if raw, ok := fields[key]; ok && grokBuildJSONType(raw) != "null" {
			return true
		}
	}
	return false
}

func grokBuildEndShape(fields map[string]json.RawMessage) (valid, public105 bool) {
	known := map[string]string{
		"type": "string", "stopReason": "string", "sessionId": "string",
		"num_turns": "number", "total_cost_usd": "number", "duration_ms": "number",
		"usage": "object", "requestId": "string", "modelUsage": "object",
		// Public 1.0.5 may report this numeric marker. It stays absent from grokBuildEvent
		// so it cannot enter accounting, and it remains illegal on the legacy envelope.
		"total_cost_usd_ticks": "number",
	}
	if _, ok := fields["type"]; !ok {
		return false, false
	}
	if _, ok := fields["stopReason"]; !ok {
		return false, false
	}
	_, hasRequestID := fields["requestId"]
	_, hasModelUsage := fields["modelUsage"]
	public105 = hasRequestID || hasModelUsage
	if public105 && (!hasRequestID || !hasModelUsage) {
		return false, true
	}
	if _, hasTicks := fields["total_cost_usd_ticks"]; hasTicks && !public105 {
		return false, public105
	}
	if grokBuildEndForbiddenField(fields) {
		return false, public105
	}
	for key, raw := range fields {
		want, ok := known[key]
		if !ok {
			continue
		}
		if grokBuildJSONType(raw) != want {
			return false, public105
		}
	}
	return true, public105
}

func grokBuildPostEndAccountingOrMetadata(typ string, fields map[string]json.RawMessage) bool {
	if grokBuildHasContent(fields) {
		return false
	}
	switch typ {
	case "usage":
		return grokBuildUsageShape(fields)
	case "start", "system", "metadata":
		return grokBuildExactShape(fields, "type", "string")
	case "system.version":
		return grokBuildExactShape(fields, "type", "string", "version", "string")
	case "available_commands":
		return grokBuildAvailableCommandsShape(fields)
	default:
		return false
	}
}

func grokBuildUsageShape(fields map[string]json.RawMessage) bool {
	return grokBuildExactShape(fields,
		"signature", "string", "type", "string", "usage", "object") ||
		grokBuildExactShape(fields,
			"messageId", "string", "signature", "string", "stopReason", "string",
			"type", "string", "usage", "object")
}

func grokBuildAvailableCommandsShape(fields map[string]json.RawMessage) bool {
	return grokBuildExactShape(fields,
		"commands", "array", "tools", "array", "type", "string") ||
		grokBuildExactShape(fields, "tools", "array", "type", "string")
}

func grokBuildToolCallShape(fields map[string]json.RawMessage) bool {
	return grokBuildExactShape(fields,
		"content", "array", "kind", "string", "locations", "array", "rawInput", "object",
		"status", "string", "title", "string", "toolCallId", "string", "toolName", "string",
		"type", "string") ||
		grokBuildExactShape(fields,
			"status", "string", "toolCallId", "string", "toolName", "string", "type", "string")
}

func grokBuildToolCallUpdateShape(fields map[string]json.RawMessage) bool {
	if len(fields) != 6 ||
		grokBuildJSONType(fields["content"]) != "array" ||
		grokBuildJSONType(fields["locations"]) != "array" ||
		grokBuildJSONType(fields["toolCallId"]) != "string" ||
		grokBuildJSONType(fields["type"]) != "string" {
		return false
	}
	rawOutputType := grokBuildJSONType(fields["rawOutput"])
	statusType := grokBuildJSONType(fields["status"])
	return rawOutputType == "null" && statusType == "null" ||
		rawOutputType == "object" && statusType == "string"
}

func grokBuildHasContent(fields map[string]json.RawMessage) bool {
	for _, key := range []string{"content", "data", "error", "message", "rawInput", "rawOutput", "result", "text"} {
		if raw, ok := fields[key]; ok && grokBuildJSONType(raw) != "null" {
			return true
		}
	}
	return false
}

func grokBuildCountUnclassified(res *claudeResult, typ string, fields map[string]json.RawMessage) {
	lower := strings.ToLower(typ)
	if strings.Contains(lower, "tool") {
		res.ToolEvents++
		return
	}
	if strings.Contains(lower, "model") {
		res.ModelEvents++
	}
	if grokBuildHasContent(fields) {
		res.SemanticEvents++
		res.ModelEvents++
	}
}

func observeGrokBuildUsage(res *claudeResult, usage *grokBuildUsage, authoritative bool) {
	if usage == nil {
		return
	}
	if authoritative {
		res.Usage = &usageInfo{
			InputTokens:              usage.InputTokens,
			CacheReadInputTokens:     usage.CacheReadInputTokens,
			CacheCreationInputTokens: usage.CacheCreationInputTokens,
			OutputTokens:             usage.OutputTokens,
		}
	}
	if usage.InputTokens != 0 || usage.OutputTokens != 0 ||
		usage.CacheReadInputTokens != 0 || usage.CacheCreationInputTokens != 0 {
		res.ModelEvents++
	}
}

func markGrokBuildInvalidTerminal(res *claudeResult) {
	res.IsError = true
	res.Subtype = "grok_build_invalid_terminal"
	res.Result = "Grok Build 未正常完成: 终局 end 事件无效或不是最后事件"
	res.ObservationComplete = false
}

func parseGrokBuildJSONL(raw string) *claudeResult {
	res := &claudeResult{Type: "result", ObservationComplete: true}
	var text bytes.Buffer
	sawEnd := false
	s := bufio.NewScanner(strings.NewReader(raw))
	s.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal([]byte(line), &fields) != nil || fields == nil {
			if sawEnd {
				markGrokBuildInvalidTerminal(res)
			}
			res.ObservationComplete = false
			continue
		}
		var typ string
		if json.Unmarshal(fields["type"], &typ) != nil || typ == "" {
			if sawEnd {
				markGrokBuildInvalidTerminal(res)
			}
			res.ObservationComplete = false
			grokBuildCountUnclassified(res, "", fields)
			continue
		}
		var ev grokBuildEvent
		if json.Unmarshal([]byte(line), &ev) != nil {
			if sawEnd {
				markGrokBuildInvalidTerminal(res)
			}
			res.ObservationComplete = false
			grokBuildCountUnclassified(res, typ, fields)
			continue
		}
		if sawEnd {
			// A valid end_turn is the terminal. Closed usage/metadata may follow it; anything
			// else, including a second end, stays an invalid terminal.
			if grokBuildPostEndAccountingOrMetadata(ev.Type, fields) {
				continue
			}
			markGrokBuildInvalidTerminal(res)
			continue
		}
		switch ev.Type {
		case "text":
			text.WriteString(ev.Data)
			res.SemanticEvents++
			res.ModelEvents++
			if !grokBuildExactShape(fields, "data", "string", "type", "string") {
				res.ObservationComplete = false
			}
		case "thinking", "thought", "reasoning":
			res.SemanticEvents++
			res.ModelEvents++
			if !grokBuildExactShape(fields, "data", "string", "type", "string") {
				res.ObservationComplete = false
			}
		case "model", "model_start", "model_end":
			res.ModelEvents++
			if !grokBuildExactShape(fields, "type", "string") {
				res.ObservationComplete = false
			}
		case "tool", "tool_use", "tool_result":
			res.ToolEvents++
			if !grokBuildExactShape(fields, "type", "string") {
				res.ObservationComplete = false
			}
		case "tool_call":
			res.ToolEvents++
			if !grokBuildToolCallShape(fields) {
				res.ObservationComplete = false
			}
		case "tool_call_update":
			res.ToolEvents++
			if !grokBuildToolCallUpdateShape(fields) {
				res.ObservationComplete = false
			}
		case "start", "system", "metadata":
			// Invocation metadata is not model/tool work.
			if !grokBuildExactShape(fields, "type", "string") {
				res.ObservationComplete = false
				grokBuildCountUnclassified(res, ev.Type, fields)
			}
		case "system.version":
			if !grokBuildExactShape(fields, "type", "string", "version", "string") {
				res.ObservationComplete = false
				grokBuildCountUnclassified(res, ev.Type, fields)
			}
		case "available_commands":
			if !grokBuildAvailableCommandsShape(fields) {
				res.ObservationComplete = false
				grokBuildCountUnclassified(res, ev.Type, fields)
			}
		case "usage":
			// Usage remains accounting metadata, never a terminal, and non-zero usage
			// conservatively proves model work. Identifier/signature values are shape-checked
			// in the raw envelope only and are never copied into the parsed result.
			valid := grokBuildUsageShape(fields)
			observeGrokBuildUsage(res, ev.Usage, valid)
			if !valid {
				res.ObservationComplete = false
				grokBuildCountUnclassified(res, ev.Type, fields)
			}
		case "error":
			res.IsError = true
			res.Subtype = "grok_build_error"
			if grokBuildExactShape(fields, "message", "string", "type", "string") {
				res.Result = strings.TrimSpace(ev.Message)
			} else {
				res.ObservationComplete = false
				res.Result = "Grok Build 返回未识别 error 事件"
			}
		case "end":
			sawEnd = true
			res.TerminalEvents++
			validEnd, public105End := grokBuildEndShape(fields)
			if !validEnd || ev.StopReason != "end_turn" || res.TerminalEvents != 1 {
				observeGrokBuildUsage(res, ev.Usage, false)
				grokBuildCountUnclassified(res, ev.Type, fields)
				markGrokBuildInvalidTerminal(res)
				break
			}
			// The documented 1.0.5 envelope carries request/session identifiers and opaque
			// modelUsage metadata. Validate their types without retaining their values. The
			// legacy envelope keeps its established resumable session behavior.
			if !public105End {
				res.SessionID = ev.SessionID
			}
			res.NumTurns = ev.NumTurns
			res.TotalCostUSD = ev.TotalCostUSD
			res.DurationMS = ev.DurationMS
			observeGrokBuildUsage(res, ev.Usage, true)
			if ev.TotalCostUSD != 0 {
				res.ModelEvents++
			}
		default:
			res.ObservationComplete = false
			grokBuildCountUnclassified(res, ev.Type, fields)
		}
	}
	if s.Err() != nil {
		res.ObservationComplete = false
	}
	if res.NumTurns > res.SemanticEvents {
		res.SemanticEvents = res.NumTurns
	}
	if res.NumTurns > res.ModelEvents {
		res.ModelEvents = res.NumTurns
	}
	if !res.IsError && (!sawEnd || !res.ObservationComplete) {
		res.IsError = true
		res.Subtype = "grok_build_stream_incomplete"
		if !sawEnd {
			res.Result = "Grok Build 流缺少终局 end 事件"
		} else {
			res.Result = "Grok Build 流包含未完整识别事件"
		}
	} else if !res.IsError && text.Len() > 0 {
		res.Result = text.String()
	}
	return res
}

func grokBuildAuthDiagnosticLine(stderr string) string {
	// Closed physical grammar: split into exact physical lines and tolerate only the process's
	// single trailing newline. Empty lines are never skipped over — any extra physical blank line
	// (leading, interleaved beyond the wrapper's own one, or trailing) rejects the observation.
	lines := strings.Split(stderr, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	if len(lines) == 0 {
		return ""
	}

	// Grok 1.0.4 emitted the OIDC failure either as one bare diagnostic or in this exact quoted
	// six-physical-line wrapper: quoted body, its one internal blank line, then the ordered unique
	// Model/Auth/Version/Available footer closed by the trailing quote. Strip only that closed
	// wrapper; arbitrary wrapper/task prose and every non-exact multiline form are rejected.
	quotedWrapper := strings.HasPrefix(lines[0], `Internal error: "`)
	if quotedWrapper {
		if len(lines) != 6 || strings.TrimSpace(lines[1]) != "" || !strings.HasSuffix(lines[5], `"`) {
			return ""
		}
		for _, raw := range append([]string{lines[0]}, lines[2:]...) {
			if strings.TrimSpace(raw) == "" {
				return ""
			}
		}
		body := strings.TrimSpace(strings.TrimPrefix(lines[0], `Internal error: "`))
		if body != grokBuildExactQuotedAuthBody {
			return ""
		}
		// The incident's metadata footer is deliberately a closed grammar. Field order,
		// cardinality, spelling and values are fixed; 1.0.4 is the observed incident and 1.0.5 is
		// the pinned CLI. The wrapper's closing quote rides on the final footer line.
		footer := []string{lines[2], lines[3], lines[4], strings.TrimSuffix(lines[5], `"`)}
		expected := [4][2]string{
			{"Model:", "grok-4.6"},
			{"Auth:", "Oidc"},
			{"Version:", ""},
			{"Available:", "grok-4.6"},
		}
		for i, want := range expected {
			fields := strings.Fields(footer[i])
			if len(fields) != 2 || fields[0] != want[0] {
				return ""
			}
			if want[0] == "Version:" {
				if fields[1] != "1.0.4" && fields[1] != "1.0.5" {
					return ""
				}
			} else if fields[1] != want[1] {
				return ""
			}
		}
		return body
	}

	// The only unquoted production form is the complete diagnostic on one physical line. A bare
	// diagnostic followed by an otherwise-valid footer, or accompanied by any blank line, is still
	// an unquoted multiline diagnostic and must not be promoted into authentication evidence.
	if len(lines) != 1 {
		return ""
	}
	line := strings.TrimSpace(lines[0])
	if line != grokBuildExactBareAuthDiagnostic {
		return ""
	}
	return line
}

func grokBuildIndependentStdoutSuccess(res *claudeResult) bool {
	return res != nil && !res.IsError && res.ObservationComplete &&
		res.TerminalEvents == 1 && strings.TrimSpace(res.Result) != ""
}

func grokBuildStderrHasAuthOrQuotaEvidence(stderr string) bool {
	if grokBuildAuthDiagnosticLine(stderr) != "" || grokBuildTrustedAuthDiagnosticLine(stderr) != "" {
		return true
	}
	return grokBuildQuotaRe.MatchString(stderr) || authClassRe.MatchString(stderr)
}

// grokBuildJSONObservation keeps streaming-json stdout authoritative. Unknown non-JSON plain
// stderr is omitted only after an exit-zero stdout-alone parse proves a nonempty complete
// success. Every JSON stderr record, malformed structured JSON, and scanner loss still join
// the observation. Auth/quota evidence and every other failure keep the generic adapter.
func grokBuildJSONObservation(stdout, stderr string, runErr error) string {
	if runErr != nil || grokBuildStderrHasAuthOrQuotaEvidence(stderr) ||
		!grokBuildIndependentStdoutSuccess(parseGrokBuildJSONL(stdout)) {
		return providerJSONObservation(grokBuildRunnerName, stdout, stderr)
	}
	var out strings.Builder
	out.WriteString(stdout)
	scanner := bufio.NewScanner(strings.NewReader(stderr))
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !json.Valid([]byte(line)) {
			if !strings.HasPrefix(line, "{") && !strings.HasPrefix(line, "[") {
				continue
			}
			line = "__CARDEX_MALFORMED_STDERR_JSON__"
		}
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(line)
	}
	if scanner.Err() != nil {
		out.WriteString("\n__CARDEX_UNOBSERVED_STDERR__")
	}
	return out.String()
}

// grokBuildWriteCapable is sequence or SkipPermissions. SkipPermissions is full
// autonomy, so a non-sequence task still gets workspace/auto and --no-plan.
// typeCrossCheck is fail-closed read-only/plan even when SkipPermissions is set.
func grokBuildWriteCapable(t *Task) bool {
	if t.Type == typeCrossCheck {
		return false
	}
	return t.Type == typeSequence || t.SkipPermissions
}

func invokeGrokBuild(ctx context.Context, root string, cfg *Config, t *Task, prompt string) (*claudeResult, string, error) {
	if !grokBuildEnabled(cfg) {
		return nil, "", fmt.Errorf("grok_build_bin/grok_build 未启用")
	}
	model := resolveGrokBuildModel(cfg, t)
	effort := resolveGrokBuildEffort(cfg, t)
	if model == "" || effort == "" {
		return nil, "", fmt.Errorf("未解析出 Grok Build 模型/推理档")
	}
	if effort == "max" {
		return nil, "", fmt.Errorf("Grok 4.6 不支持 reasoning effort=max；最高可用档为 xhigh")
	}
	if err := ensureGrokBuildAuth(ctx, root, cfg, model); err != nil {
		subtype := "grok_build_preflight_error"
		if isGrokBuildAuthProbeError(err) {
			subtype = "grok_build_auth_preflight"
		}
		if isGrokBuildAuthCircuitError(err) {
			subtype = "grok_build_auth_circuit_open"
		}
		res := &claudeResult{Type: "result", IsError: true, Subtype: subtype,
			Result: err.Error(), ObservationComplete: true}
		return res, err.Error(), err
	}
	promptFile, err := os.CreateTemp("", "cardex-grok-prompt-*.txt")
	if err != nil {
		return nil, "", err
	}
	promptPath := promptFile.Name()
	defer os.Remove(promptPath)
	if err := promptFile.Chmod(0o600); err != nil {
		promptFile.Close()
		return nil, "", err
	}
	if _, err := promptFile.WriteString(prompt); err != nil {
		promptFile.Close()
		return nil, "", err
	}
	if err := promptFile.Close(); err != nil {
		return nil, "", err
	}

	writeCapable := grokBuildWriteCapable(t)
	sandbox, permission := "read-only", "plan"
	if writeCapable {
		sandbox, permission = "workspace", "auto"
	}
	args := []string{
		"--no-auto-update",
		"--model", model,
		"--reasoning-effort", effort,
		"--output-format", "streaming-json",
		"--sandbox", sandbox,
		"--permission-mode", permission,
		"--no-memory", "--no-subagents", "--disable-web-search", "--verbatim",
	}
	if writeCapable {
		args = append(args, "--no-plan")
	}
	if t.SessionID != "" {
		args = append(args, "--resume", t.SessionID)
	}
	args = append(args, "--prompt-file", promptPath)

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.StepTimeoutMin)*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(runCtx, cfg.GrokBuildBin, args...)
	setupProcGroup(cmd)
	cmd.Dir = t.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := runCmdRegisteredForTask(cmd, t.ID)
	if runCtx.Err() == context.DeadlineExceeded {
		runErr = fmt.Errorf("步骤超时（%d 分钟）", cfg.StepTimeoutMin)
	}
	combined := stdout.String() + "\n" + stderr.String()
	exactAuthLine := grokBuildAuthDiagnosticLine(stderr.String())
	observedStderr := stderr.String()
	if exactAuthLine != "" {
		// The whole stderr has already matched the closed diagnostic grammar. It is trusted
		// presemantic process metadata, so it must not make an otherwise complete version-only
		// observation look partial. Mixed or unknown stderr never reaches this branch.
		observedStderr = ""
	}
	res := parseGrokBuildJSONL(grokBuildJSONObservation(stdout.String(), observedStderr, runErr))
	// Grok 1.0.4/1.0.5 may emit only system.version on stdout and put the concrete OIDC 401 on
	// stderr. The metadata-only parser correctly calls that stream incomplete, but that synthetic
	// symptom must not mask a trusted process diagnostic: preserve the auth root cause so the generic
	// classifier can hold without attempts and open the engine circuit.
	if runErr != nil && res != nil && res.Subtype == "grok_build_stream_incomplete" &&
		res.SemanticEvents == 0 && res.ModelEvents == 0 && res.ToolEvents == 0 {
		if exactAuthLine != "" && res.ObservationComplete {
			res.IsError = true
			res.Subtype = "grok_build_process_auth_exact"
			res.Result = exactAuthLine
		} else if authLine := grokBuildTrustedAuthDiagnosticLine(stderr.String()); authLine != "" {
			// A strict whole-line ordinary auth diagnostic remains held, but only the complete closed
			// family above (exact bare one-liner or exact quoted wrapper) may open the engine-wide circuit.
			res.IsError = true
			res.Subtype = "grok_build_process_auth"
			res.Result = authLine
		}
	}
	if runErr != nil && (res == nil || !res.IsError) {
		if res == nil {
			res = &claudeResult{Type: "result"}
		}
		res.IsError = true
		res.Subtype = "grok_build_process_error"
		res.Result = firstLine(stderr.String())
		if res.Result == "" {
			res.Result = runErr.Error()
		}
	}
	if res != nil && res.IsError && runErr == nil {
		runErr = fmt.Errorf("Grok Build 返回错误: %s", summarizeResult(res.Result))
	}
	if res != nil && !res.IsError && strings.TrimSpace(res.Result) == "" && runErr == nil {
		runErr = fmt.Errorf("Grok Build 未返回最终文本")
	}
	return res, combined, runErr
}

var grokBuildQuotaRe = regexp.MustCompile(`(?i)(?:^|[^0-9])429(?:[^0-9]|$)|too many requests|rate limit|usage limit|quota (?:exceeded|exhausted)|insufficient (?:quota|credits)|out of (?:credits|usage)|额度(?:不足|已用完)|配额(?:不足|已用尽)|限额(?:不足|已用尽)`)

func grokBuildLimitScanText(res *claudeResult, combined string) string {
	var parts []string
	for _, line := range strings.Split(combined, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev grokBuildEvent
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Type != "" {
			if ev.Type == "error" && ev.Message != "" {
				parts = append(parts, ev.Message)
			}
			continue
		}
		parts = append(parts, line) // stderr / pre-stream CLI failures
	}
	if res != nil && res.IsError && strings.TrimSpace(res.Result) != "" {
		parts = append(parts, res.Result)
	}
	return strings.Join(parts, "\n")
}

func isLimitHitGrokBuild(res *claudeResult, combined string) bool {
	if res != nil && !res.IsError && strings.TrimSpace(res.Result) != "" {
		return false
	}
	return grokBuildQuotaRe.MatchString(grokBuildLimitScanText(res, combined))
}

func grokBuildResetEpoch(cfg *Config, res *claudeResult, combined string, now time.Time) int64 {
	scan := grokBuildLimitScanText(res, combined)
	fallback := 0
	if cfg != nil && cfg.GrokBuild != nil {
		fallback = cfg.GrokBuild.LimitFallbackMin
	}
	return engineResetEpoch(combined+"\n"+resultText(res), scan, cfg, EngineProfile{LimitFallbackMin: fallback}, now)
}

func firstPrinciplesReviewModel(cfg *Config) string {
	if cfg != nil && cfg.GrokBuild != nil {
		if model := strings.TrimSpace(cfg.GrokBuild.ReviewCodexModel); model != "" {
			return model
		}
	}
	return grokCodexFallbackModel(cfg)
}

func firstPrinciplesReviewEffort(cfg *Config) string {
	if cfg != nil && cfg.GrokBuild != nil {
		if effort := strings.TrimSpace(cfg.GrokBuild.ReviewCodexEffort); effort != "" {
			return effort
		}
	}
	return grokCodexFallbackEffort(cfg)
}

func ensureFableFirstPrinciplesReview(root string, cfg *Config, parent *Task, result string, lg *os.File) {
	if parent == nil || !parent.FableFirstPrinciplesReview || cfg == nil || cfg.GrokBuild == nil ||
		!cfg.GrokBuild.FableFirstPrinciples {
		return
	}
	all, err := loadBoardTasks(root)
	if err == nil {
		for _, task := range all {
			if task.ReviewOf == parent.ID && task.AdvisoryReview {
				return
			}
		}
	}
	prompt := "你是独立的设计审查者。请从第一性原理审查下面的设计任务与候选方案。\n\n" +
		"强制方法：\n" +
		"1. 不预设现有结果、方向或架构是正确的；先从零重建目标、不可变约束、失败条件与可验证成功标准。\n" +
		"2. 至少提出一个真正不同的替代方向，再比较候选方案与替代方向。\n" +
		"3. 主动寻找遗漏约束、反例、二阶影响、迁移/回滚风险、不可逆决定与验证盲点。\n" +
		"4. 清楚区分事实、假设与待验证项；给出需要补盲的最小行动。\n" +
		"5. 这是顾问审查，不修改仓库、不自动执行修复，也不要因为候选方案已经存在就偏向认可它。\n\n" +
		"原始任务：\n" + strings.Join(parent.Prompts, "\n\n--- 下一步 ---\n\n") +
		"\n\n候选方案/结果：\n" + result
	review := newTask(root, cfg, typeReview, "第一性补盲: "+parent.Title, parent.Dir, []string{prompt}, parent.Priority)
	review.PreferRunner = "codex"
	review.Model = "fable" // 来源档位与实际 Sol/max 投入一致，便于按真实组合复盘。
	review.CodexModel = firstPrinciplesReviewModel(cfg)
	review.Effort = firstPrinciplesReviewEffort(cfg)
	review.EffortExplicit = true
	review.ReviewOf = parent.ID
	review.AdvisoryReview = true
	review.Project = parent.Project
	review.ReviewAfter = false
	review.RemoteHost = ""
	review.ReviewHost = ""
	review.ReviewDir = ""
	review.ReviewSync = ""
	if err := saveTask(root, review); err != nil {
		logBlock(lg, "FIRST_PRINCIPLES_REVIEW", "补盲卡入队失败: "+err.Error())
		return
	}
	emitTaskEvent(root, parent.ID, evCloseout, "runner:first-principles-review", statusDone, parent.Step, map[string]any{
		"kind": "fable_first_principles_review", "child": review.ID,
	})
	emitTaskEvent(root, review.ID, evQueued, "runner:first-principles-review", statusQueued, 0, map[string]any{
		"parent": parent.ID, "review_of": parent.ID, "advisory": true,
	})
	logBlock(lg, "FIRST_PRINCIPLES_REVIEW", "已入队独立 Sol/max 第一性补盲卡: "+review.ID)
}
