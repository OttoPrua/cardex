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
	openCodeCooldownName                    = "opencode"
	routeReasonOpenCodeExplicit             = "opencode_explicit"
	routeReasonOpenCodeNightOpus            = "opencode_night_opus_preferred"
	routeReasonOpenCodeLimitFallbackPending = "opencode_limit_fallback_pending"
	routeReasonOpenCodeLimitFallback        = "opencode_limit_fallback"
)

func resolveOpenCodeModel(cfg *Config, t *Task) string {
	if t != nil && strings.TrimSpace(t.OpenCodeModel) != "" {
		return strings.TrimSpace(t.OpenCodeModel)
	}
	if cfg == nil {
		return ""
	}
	if t != nil {
		if tier := modelTierKeyword(cfg, t.Model); tier != "" {
			if model := strings.TrimSpace(cfg.OpenCodeModels[tier]); model != "" {
				return model
			}
		}
	}
	return strings.TrimSpace(cfg.OpenCodeModel)
}

// resolveOpenCodeRunModel/Variant 解析这一次真实 OpenCode 调用。夜间自动路由与卡面
// Effort 解耦：同一张 Opus 卡在 Kimi 上用 max，撞限额回 Codex 后仍由 Codex 档位表解析为 xhigh。
func resolveOpenCodeRunModel(cfg *Config, t *Task) string {
	if t != nil && t.PreferRunner != "opencode" && cfg != nil && cfg.OpenCodeNightOpus != nil {
		if model := strings.TrimSpace(cfg.OpenCodeNightOpus.Model); model != "" {
			return model
		}
	}
	return resolveOpenCodeModel(cfg, t)
}

func resolveOpenCodeRunVariant(cfg *Config, t *Task) string {
	if t != nil && t.PreferRunner != "opencode" && cfg != nil && cfg.OpenCodeNightOpus != nil {
		if variant := strings.TrimSpace(cfg.OpenCodeNightOpus.Variant); variant != "" {
			return variant
		}
	}
	if t == nil {
		return ""
	}
	return strings.TrimSpace(t.Effort)
}

func openCodeHourInWindow(hour, start, end int) bool {
	if start < end {
		return hour >= start && hour < end
	}
	return hour >= start || hour < end
}

func openCodeNightRouteActive(cfg *Config, now time.Time) bool {
	if cfg == nil || cfg.OpenCodeNightOpus == nil || !cfg.OpenCodeNightOpus.Enabled {
		return false
	}
	r := cfg.OpenCodeNightOpus
	loc, err := time.LoadLocation(r.Timezone)
	if err != nil {
		return false // loadConfig 已 fail-fast；保留守卫防测试/内存构造绕过校验。
	}
	return openCodeHourInWindow(now.In(loc).Hour(), r.StartHour, r.EndHour)
}

func openCodeReady(root string, cfg *Config, now time.Time) bool {
	if cfg == nil || strings.TrimSpace(cfg.OpenCodeBin) == "" {
		return false
	}
	cd := loadEngineCooldown(root, openCodeCooldownName)
	return cd == nil || !cd.active(now)
}

// openCodePinnedReady 只检查执行器与独立车道冷却。时间窗属于“自动把默认 Codex Opus
// 改道到 OpenCode”的选择策略，不是模型禁用窗；用户显式 -runner opencode 的人工测试
// 在窗外仍可启动。显式意图只在真实额度命中时由 runner 分支转 Codex。
func openCodePinnedReady(root string, cfg *Config, _ *Task, now time.Time) bool {
	return openCodeReady(root, cfg, now)
}

// openCodeNightOpusEligible 只改道“默认走 Codex、仍可无损回 Codex”的 Opus 卡。
// 显式 Codex 模型、交叉链、远端卡和已有会话均保留原路由；Fable/Sonnet/Haiku 天然不命中。
func openCodeNightOpusEligible(root string, cfg *Config, t *Task, now time.Time) bool {
	if cfg == nil || cfg.OwnerRoutingEnforced || t == nil || t.PreferRunner != "codex" ||
		t.RunnerExplicit || t.RemoteHost != "" || !isOpusTask(cfg, t) {
		return false
	}
	if t.CodexModel != "" || t.XCodexModel != "" || t.GeminiModel != "" || t.AgyModel != "" || t.OpenCodeModel != "" ||
		t.KimiModel != "" || t.GrokModel != "" || t.GrokEffort != "" || t.CursorModel != "" ||
		t.XRole != "" || t.SessionID != "" || t.MidStep || !codexEligible(t) {
		return false
	}
	if t.RouteReason == routeReasonOpenCodeLimitFallbackPending || t.RouteReason == routeReasonOpenCodeLimitFallback {
		return false
	}
	return openCodeNightRouteActive(cfg, now) && openCodeReady(root, cfg, now)
}

var openCodeQuotaRe = regexp.MustCompile(`(?i)(?:^|[^0-9])429(?:[^0-9]|$)|too many requests|rate limit|quota (?:exceeded|exhausted)|insufficient (?:quota|credits)|额度(?:不足|已用完)|配额(?:不足|已用尽)`)

func openCodeLimitScanText(res *claudeResult, combined string) string {
	var parts []string
	for _, line := range strings.Split(combined, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev openCodeEvent
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Type != "" {
			if ev.Type == "error" && len(ev.Error) > 0 {
				parts = append(parts, string(ev.Error))
			}
			continue
		}
		// OpenCode 的早期 HTTP/CLI 错误可能只落 stderr，不是 JSONL 事件。
		parts = append(parts, line)
	}
	if res != nil && strings.TrimSpace(res.Result) != "" {
		parts = append(parts, res.Result)
	}
	if len(parts) == 0 {
		return combined
	}
	return strings.Join(parts, "\n")
}

func isLimitHitOpenCode(res *claudeResult, combined string) bool {
	// 成功文本可能在讨论“rate limit”；只要 OpenCode 真给了非错误终稿就绝不能误挂车道。
	if res != nil && !res.IsError && strings.TrimSpace(res.Result) != "" {
		return false
	}
	scan := openCodeLimitScanText(res, combined)
	return limitRe.MatchString(scan) || engineQuotaRe.MatchString(scan) || openCodeQuotaRe.MatchString(scan)
}

func openCodeResetEpoch(cfg *Config, res *claudeResult, combined string, now time.Time) int64 {
	scan := openCodeLimitScanText(res, combined)
	fallback := 0
	if cfg != nil && cfg.OpenCodeNightOpus != nil {
		fallback = cfg.OpenCodeNightOpus.LimitFallbackMin
	}
	return engineResetEpoch(combined+"\n"+resultText(res), scan, cfg, EngineProfile{LimitFallbackMin: fallback}, now)
}

type openCodeEvent struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionID"`
	Error     json.RawMessage `json:"error"`
	Part      struct {
		Text string `json:"text"`
		Time struct {
			Start int64 `json:"start"`
			End   int64 `json:"end"`
		} `json:"time"`
		Tokens struct {
			Input  int `json:"input"`
			Output int `json:"output"`
		} `json:"tokens"`
		Cost float64 `json:"cost"`
	} `json:"part"`
}

func parseOpenCodeJSONL(raw string) *claudeResult {
	res := &claudeResult{Type: "result"}
	var texts []string
	s := bufio.NewScanner(strings.NewReader(raw))
	s.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for s.Scan() {
		var ev openCodeEvent
		if json.Unmarshal([]byte(strings.TrimSpace(s.Text())), &ev) != nil {
			continue
		}
		if ev.SessionID != "" {
			res.SessionID = ev.SessionID
		}
		switch ev.Type {
		case "text":
			if ev.Part.Text != "" {
				texts = append(texts, ev.Part.Text)
			}
			if ev.Part.Time.End > ev.Part.Time.Start {
				res.DurationMS += ev.Part.Time.End - ev.Part.Time.Start
			}
		case "step_finish":
			res.NumTurns++
			res.TotalCostUSD += ev.Part.Cost
			if res.Usage == nil {
				res.Usage = &usageInfo{}
			}
			res.Usage.InputTokens += ev.Part.Tokens.Input
			res.Usage.OutputTokens += ev.Part.Tokens.Output
		case "error":
			res.IsError = true
			res.Result = string(ev.Error)
		}
	}
	if len(texts) > 0 {
		res.Result = strings.Join(texts, "\n")
	}
	return res
}

// invokeOpenCode uses OpenCode's own credential store, so Cardex never needs
// to read or duplicate the OpenCode Go API key.
func invokeOpenCode(ctx context.Context, cfg *Config, t *Task, prompt string) (*claudeResult, string, error) {
	if cfg == nil || strings.TrimSpace(cfg.OpenCodeBin) == "" {
		return nil, "", fmt.Errorf("opencode_bin 未配置")
	}
	model := resolveOpenCodeRunModel(cfg, t)
	if model == "" {
		return nil, "", fmt.Errorf("未解析出 OpenCode 模型")
	}
	args := []string{"run", "--pure", "--format", "json", "--model", model, "--dir", t.Dir}
	if t.SessionID != "" {
		args = append(args, "--session", t.SessionID)
	}
	if variant := resolveOpenCodeRunVariant(cfg, t); variant != "" {
		args = append(args, "--variant", variant)
	}
	if t.Type == typeSequence || t.SkipPermissions {
		args = append(args, "--auto")
	}
	args = append(args, prompt)

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.StepTimeoutMin)*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(runCtx, cfg.OpenCodeBin, args...)
	setupProcGroup(cmd)
	cmd.Dir = t.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := runCmdRegisteredForTask(cmd, t.ID)
	if runCtx.Err() == context.DeadlineExceeded {
		runErr = fmt.Errorf("步骤超时（%d 分钟）", cfg.StepTimeoutMin)
	}
	combined := stdout.String() + "\n" + stderr.String()
	res := parseOpenCodeJSONL(stdout.String())
	if res.IsError && runErr == nil {
		runErr = fmt.Errorf("OpenCode 返回错误: %s", summarizeResult(res.Result))
	}
	if strings.TrimSpace(res.Result) == "" && runErr == nil {
		runErr = fmt.Errorf("OpenCode 未返回最终文本")
	}
	return res, combined, runErr
}
