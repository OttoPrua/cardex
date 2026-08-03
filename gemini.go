package main

// gemini.go —— Gemini CLI 备用执行器（第二异构执行器，2026-08-03 设计规格见
// docs/2026-08-03-gemini-executor-design.md）。
//
// 与 codex 同属异构执行器（独立 CLI、独立输出协议、独立订阅额度），但拿引擎档案的
// 既有基建补了 codex 的两块短板：
//   1. 有会话：--session-id <uuid> 起新会话（UUID 由本进程生成，不解析输出）、
//      --resume <uuid> 续跑——钉定卡多步/限额续跑可用；
//   2. 有车道冷却：复用 cooldown-gemini.json（engineCooldownPath("gemini")）。Google 配额是
//      **账号级每日请求数**（OAuth 免费档 1000/天、AI Pro 1500、AI Ultra 2000、API key
//      免费档 250/天仅 Flash，官方 quota-and-pricing 2026-08-03 核实），一张卡撞到当日耗尽
//      = 整条车道耗尽，必须挂车道而不是像 codex 只挂本卡（队里 20 张卡各撞一次是纯浪费）。
// 账本：usage.json 打 engine:"gemini" 标——不占 claude 五小时红线预算（budget.go 按标过滤）。

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// geminiTierSlotDefaults 是档位槽映射的内置默认（config.gemini_models 缺省时用）。
// 值是 gemini CLI 的**官方稳定别名**（官方 models.ts 常量，核实 2026-08-03）：Google 轮换
// 模型代次时别名自动跟新，配置不腐烂。当前解析：pro→gemini-3.1-pro-preview 线、
// flash→gemini-3.5-flash 线。高档取 pro 是委托人按编码交叉信号（SWE-bench V：pro 80.6% >
// flash 77.5%）的显式决定；统一标准线（AA II v4.1）上 flash=50 反超 pro=46，展示档位按
// 标准线披露（见 boardmodel.go），映射与展示两回事。
func geminiTierSlotDefaults() map[string]string {
	return map[string]string{"fable": "pro", "opus": "pro", "sonnet": "flash", "haiku": "flash-lite"}
}

// validGeminiApprovalModes 见 Config.GeminiApprovalMode。未知值载入即拒（fail fast）——
// 打错 approval mode 的表现是 headless 里工具全被拒、卡空转烧 attempts，比报错恶劣。
var validGeminiApprovalModes = map[string]bool{"default": true, "auto_edit": true, "yolo": true, "plan": true}

// validateGemini 校验 gemini 执行器的配置面，loadConfig 时调用（与 validateEngines 同点）。
func validateGemini(cfg *Config) error {
	for alias := range cfg.GeminiModels {
		switch alias {
		case "fable", "opus", "sonnet", "haiku":
		default:
			return fmt.Errorf("gemini_models 的键 %q 不是档位别名（可选: fable/opus/sonnet/haiku；具体模型请在卡上用 -gemini-model 钉）", alias)
		}
	}
	if m := cfg.GeminiApprovalMode; m != "" && !validGeminiApprovalModes[m] {
		return fmt.Errorf("gemini_approval_mode %q 不合法（可选: default/auto_edit/yolo/plan）", m)
	}
	return nil
}

// geminiVia 判断 via/runner 标签是否 gemini 执行器。
func geminiVia(via string) bool { return via == "gemini" }

// resolveGeminiModel 决定一次 gemini 执行用哪个模型与披露备注。优先序（与 resolveCodexModel 同构）：
//  1. XGeminiModel——交叉链入队冻结的引擎身份，恒最高；
//  2. 卡级 GeminiModel（-gemini-model 钉定）；
//  3. 按 t.Model 档位别名查 gemini_models 槽映射（缺省 geminiTierSlotDefaults；本档缺失
//     向下档回落并披露）；
//  4. config.gemini_model；
//  5. 内置 "pro"。
//
// **永不返回空**：空模型 = CLI 默认 auto 路由，撞配额会静默换模型——委托人已否决
// （档位漂移必须可见）。回落/兜底都返回非空 note，由 runTask 落任务日志。
func resolveGeminiModel(cfg *Config, t *Task) (string, string) {
	if t.XGeminiModel != "" {
		return t.XGeminiModel, ""
	}
	if t.GeminiModel != "" {
		return t.GeminiModel, ""
	}
	slots := cfg.GeminiModels
	if len(slots) == 0 {
		slots = geminiTierSlotDefaults()
	}
	if alias := modelAlias(t.Model); alias != "" {
		start := 0
		for i, a := range engineTierOrder {
			if a == alias {
				start = i
				break
			}
		}
		for _, a := range engineTierOrder[start:] {
			if m := slots[a]; m != "" {
				if a != alias {
					return m, fmt.Sprintf("档位 %s 无映射，回落 %s 档 → %s", alias, a, m)
				}
				return m, ""
			}
		}
	}
	if cfg.GeminiModel != "" {
		note := ""
		if t.Model != "" {
			note = fmt.Sprintf("档位映射无 %q 的落点，用 gemini_model → %s", t.Model, cfg.GeminiModel)
		}
		return cfg.GeminiModel, note
	}
	return "pro", "无映射且未配 gemini_model，兜底官方别名 pro（不落空——空即 auto 路由，已否决）"
}

// geminiApprovalModeFor 决定一次执行的 --approval-mode。非 sequence 卡（复审/协调/装配/
// 交叉/进度回收）恒 "plan"（只读）——gemini 无 OS 沙箱，plan 是唯一硬护栏，与 codex
// 「非 sequence 默认 read-only」同一纪律且不需要复审副本机制（read-only 挡不住写才需要副本）。
// sequence 卡用 config.gemini_approval_mode（空=yolo；写盘是这类卡的本职）。
func geminiApprovalModeFor(cfg *Config, t *Task) string {
	if t.Type != typeSequence {
		return "plan"
	}
	if cfg.GeminiApprovalMode != "" {
		return cfg.GeminiApprovalMode
	}
	return "yolo"
}

// newGeminiSessionID 生成 UUIDv4 作 --session-id。会话 ID 由 cardex 自己定（而非解析 CLI
// 输出）：--resume 续跑不依赖输出格式，输出解析面越小越稳。
func newGeminiSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 不可用的环境本身已不可信；退化为时间戳串（仅影响会话续跑，不影响执行）。
		return fmt.Sprintf("cardex-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// geminiJSONOut 是 gemini -o json 的输出形状（官方 headless 文档：response/stats/error）。
type geminiJSONOut struct {
	Response string          `json:"response"`
	Stats    json.RawMessage `json:"stats"`
	Error    *geminiJSONErr  `json:"error"`
}

type geminiJSONErr struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    any    `json:"code"`
}

// geminiStatsUsage 从 stats 里抽 token 计数（官方 result 事件形状：models.<id>.tokens.*）。
// 形状对不上返回 nil——宁可缺记录，不编造数字（项目纪律）。
func geminiStatsUsage(raw json.RawMessage) *usageInfo {
	if len(raw) == 0 {
		return nil
	}
	var st struct {
		Models map[string]struct {
			Tokens struct {
				Prompt     int `json:"prompt"`
				Candidates int `json:"candidates"`
				Cached     int `json:"cached"`
				Thoughts   int `json:"thoughts"`
				Tool       int `json:"tool"`
			} `json:"tokens"`
		} `json:"models"`
	}
	if json.Unmarshal(raw, &st) != nil || len(st.Models) == 0 {
		return nil
	}
	u := &usageInfo{}
	for _, m := range st.Models {
		in := m.Tokens.Prompt - m.Tokens.Cached
		if in < 0 {
			in = 0
		}
		u.InputTokens += in
		u.CacheReadInputTokens += m.Tokens.Cached
		u.OutputTokens += m.Tokens.Candidates + m.Tokens.Thoughts + m.Tokens.Tool
	}
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheReadInputTokens == 0 {
		return nil
	}
	return u
}

// invokeGemini 用 gemini CLI 执行一步。prompt 走 stdin（headless 由非 TTY stdin 触发，
// 官方 headless 文档核实；不当 argv 绕开 ARG_MAX），结果从 stdout 的 -o json 取回。
// 返回值第三项是模型解析披露备注（同 invokeEngine 的 note 语义）。
func invokeGemini(ctx context.Context, root string, cfg *Config, t *Task, prompt string) (*claudeResult, string, string, error) {
	_ = root // 与 invokeCodex 签名对齐；gemini 无副本机制（plan 只读即硬护栏），root 暂无用途
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.StepTimeoutMin)*time.Minute)
	defer cancel()

	model, note := resolveGeminiModel(cfg, t)
	args := []string{"-o", "json", "--approval-mode", geminiApprovalModeFor(cfg, t), "--skip-trust", "-m", model}
	sid := t.SessionID
	if sid != "" {
		args = append(args, "--resume", sid)
	} else {
		sid = newGeminiSessionID()
		args = append(args, "--session-id", sid)
	}

	cmd := exec.CommandContext(ctx, cfg.GeminiBin, args...)
	setupProcGroup(cmd)
	cmd.Dir = t.Dir
	// 前导复用 codex 的 subagent 抑制（文本与执行器无关）：gemini CLI 同样会加载 GEMINI.md/
	// extensions/skills，回合预算被框架流程吃穿的失败形态与 codex 完全同构。
	cmd.Stdin = strings.NewReader(codexSubagentPreamble + prompt)
	env := append(os.Environ(), "NO_COLOR=1")
	if cfg.GeminiAuthEnv != "" {
		if v := strings.TrimSpace(os.Getenv(cfg.GeminiAuthEnv)); v != "" {
			env = append(env, "GEMINI_API_KEY="+v)
		}
		// 变量为空不报错：留给认证判据在执行时上报（车道冷却自愈），与档案 auth_env 的
		// fail-fast 不同——gemini 还有 OAuth 缓存这条独立认证路径，静态判不了"缺认证"。
	}
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := runCmdRegisteredForTask(cmd, t.ID)
	runErr = rescueWaitDelay(runErr, cmd)
	if ctx.Err() == context.DeadlineExceeded {
		runErr = fmt.Errorf("步骤超时（%d 分钟）", cfg.StepTimeoutMin)
	}
	combined := stdout.String() + "\n" + stderr.String()

	res := &claudeResult{Type: "result"}
	jout := parseGeminiJSON(stdout.Bytes())
	switch {
	case jout != nil && jout.Error != nil:
		res.IsError = true
		res.Subtype = "gemini_error"
		res.Result = strings.TrimSpace(jout.Error.Message)
		if res.Result == "" {
			res.Result = "gemini 返回错误（error.message 为空，type=" + jout.Error.Type + "）"
		}
	case jout != nil && strings.TrimSpace(jout.Response) != "":
		res.Result = strings.TrimSpace(jout.Response)
		if runErr != nil {
			// 结果在手即成功：response 已完整落盘而进程收尾报错（WaitDelay 族）时不作废产物。
			res.ResultFromTranscript = true
		}
	case runErr != nil:
		res.IsError = true
		res.Subtype = "gemini_error"
		if line := geminiErrorLine(combined); line != "" {
			res.Result = line
			// 挑走的是 stderr/transcript 里的一行——分类侧不得据此落终态（codex P1 教训同规）。
			res.ResultFromTranscript = true
		} else {
			res.Result = firstLine(runErr.Error())
		}
	default:
		// 进程 exit 0 但 stdout 无可解析 JSON / response 为空：回合停在工具调用或输出被吞。
		res.IsError = true
		res.Subtype = "gemini_no_final_message"
		res.Result = "gemini 回合完成但未产出最终消息（-o json 无 response）——重试通常可成"
	}
	if jout != nil {
		res.Usage = geminiStatsUsage(jout.Stats)
	}
	// 会话回写信号：产出正常、或属限额/认证挂起（车道恢复后续跑要用）时上报本次会话 ID；
	// 其余硬错误不上报——半死会话被 --resume 会放大成连环错。
	if !res.IsError || geminiSuspendKind(res, combined) != "" {
		res.SessionID = sid
	}
	return res, combined, note, runErr
}

// parseGeminiJSON 从 stdout 提取 -o json 对象。stdout 理应是纯 JSON，但防御性地截取
// 首个 '{' 到末个 '}'（Node 侧偶发的 stdout 污染不该让整步作废）。解析不出返回 nil。
func parseGeminiJSON(out []byte) *geminiJSONOut {
	s := bytes.TrimSpace(out)
	if i := bytes.IndexByte(s, '{'); i >= 0 {
		if j := bytes.LastIndexByte(s, '}'); j > i {
			s = s[i : j+1]
		}
	}
	var j geminiJSONOut
	if json.Unmarshal(s, &j) != nil {
		return nil
	}
	if j.Response == "" && j.Error == nil && len(j.Stats) == 0 {
		return nil
	}
	return &j
}

// ---- 限额 / 认证判据 ----
//
// 依据 = gemini CLI 官方源码的错误分类（packages/core googleQuotaErrors.ts /
// errorClassification.ts，核实 2026-08-03）：
//   TerminalQuotaError（当日耗尽，不可重试）：quotaId 含 PerDay/Daily、"You have exhausted
//     your daily quota"、QUOTA_EXHAUSTED、"limit: 0"、INSUFFICIENT_G1_CREDITS_BALANCE；
//   RetryableQuotaError（每分钟限流）：PerMinute、一般 429/503——**不在此判**，留给
//     transientRe 退避重试（瞬时拥堵挂 6 小时车道冷却是把小病治成大病）。

// geminiDailyRe 终止态（当日/账期耗尽）措辞——命中即挂车道冷却。
var geminiDailyRe = regexp.MustCompile(`(?i)exhausted your daily quota|daily quota|quota .{0,24}per ?day|perday|QUOTA_EXHAUSTED|TerminalQuotaError|limit:\s*0|INSUFFICIENT_G1_CREDITS`)

// geminiAuthErrRe 认证/资格错误——重试无益（凭据不自愈），同样挂车道（reason 前缀 auth），
// 修好认证后冷却到点自恢复。IneligibleTierError/UNSUPPORTED_CLIENT 是 2026-08-03 实测形态：
// OAuth 个人免费档被 0.42+ 客户端拒绝（Google 迁移 Antigravity 公告）。
var geminiAuthErrRe = regexp.MustCompile(`(?i)IneligibleTierError|UNSUPPORTED_CLIENT|Error authenticating|UNAUTHENTICATED|API key not valid|invalid api key|please sign in|not logged in|reauth`)

// geminiRetryDelayRe 解析 Google 错误里的 retryDelay/"retry in Ns" 形态（RetryInfo detail）。
var geminiRetryDelayRe = regexp.MustCompile(`(?i)retry ?(?:delay|in)["':\s]*(\d+(?:\.\d+)?)\s*s`)

// geminiLimitScanText 返回判据扫描面：stderr 尾段 + 结构化错误信息（res.Result 仅当非
// transcript 来源），**绝不扫模型 response prose**——自审本仓时 prose 满是限额字面量。
func geminiLimitScanText(res *claudeResult, combined string) string {
	text := stderrTailFromClaudeCombined(combined)
	if res != nil && res.IsError && !res.ResultFromTranscript {
		text += "\n" + res.Result
	}
	return text
}

// geminiSuspendKind 判定本次失败属哪类车道挂起："limit"（当日配额耗尽）/ "auth"（认证/资格）/
// ""（都不是——走普通失败分类）。成功结果恒 ""。
func geminiSuspendKind(res *claudeResult, combined string) string {
	if res != nil && !res.IsError {
		return ""
	}
	scan := geminiLimitScanText(res, combined)
	switch {
	case geminiDailyRe.MatchString(scan):
		return "limit"
	case geminiAuthErrRe.MatchString(scan):
		return "auth"
	}
	return ""
}

// isLimitHitGemini 是 limitHitForRunner 的 gemini 分支判据（limit 与 auth 都按"车道挂起"走
// 限额分支——二者的正确响应同构：挂车道、卡不烧 attempt、到点自愈）。
func isLimitHitGemini(res *claudeResult, combined string) bool {
	return geminiSuspendKind(res, combined) != ""
}

// geminiSuspendReason 返回挂起原因行：扫描面里**命中判据的那一行**（按 kind 选正则），
// 而不是扫描面第一行——stderr 首行常是 "YOLO mode is enabled" 这类横幅（实测 2026-08-03），
// 拿它当原因会让冷却文件/卡面/事件三处披露全部说错话。找不到命中行退回首行。
func geminiSuspendReason(res *claudeResult, combined, kind string) string {
	scan := geminiLimitScanText(res, combined)
	re := geminiDailyRe
	if kind == "auth" {
		re = geminiAuthErrRe
	}
	for _, line := range strings.Split(scan, "\n") {
		if l := strings.TrimSpace(line); l != "" && re.MatchString(l) {
			return l
		}
	}
	return firstLine(strings.TrimSpace(scan))
}

// geminiResetEpoch 解析车道恢复时刻：retryDelay 秒数 > 通用瀑布（parseResetEpoch）> 回退。
// 每日配额无公开重置时间戳（官方文档未给），回退抬到 max(limit_fallback_min, 360)——
// 与引擎档案的月度/计费周期同一保守纪律。
func geminiResetEpoch(fullText, scanText string, cfg *Config, now time.Time) int64 {
	if m := geminiRetryDelayRe.FindStringSubmatch(scanText); m != nil {
		var secs float64
		if _, err := fmt.Sscanf(m[1], "%f", &secs); err == nil && secs > 0 {
			return now.Add(time.Duration(secs * float64(time.Second))).Unix()
		}
	}
	fb := cfg.LimitFallbackMin
	if fb < 360 {
		fb = 360
	}
	tmp := *cfg
	tmp.LimitFallbackMin = fb
	return parseResetEpoch(fullText, &tmp, now)
}

// geminiErrorLine 从 combined 里挑一行真实错误（stderr 优先形态）。判据与 codexErrorLine
// 同族：只认瞬时网络样式或 gemini 硬错误措辞，不从 prose 里瞎抓"error"字样。
func geminiErrorLine(combined string) string {
	for _, line := range strings.Split(combined, "\n") {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		if transientRe.MatchString(l) || geminiDailyRe.MatchString(l) || geminiAuthErrRe.MatchString(l) ||
			strings.Contains(l, "Error") || strings.Contains(l, "FATAL") {
			if strings.HasPrefix(l, "Warning:") || strings.HasPrefix(l, "Ripgrep is not available") {
				continue
			}
			return l
		}
	}
	return ""
}

// ---- 派发护栏 ----

// pinnedGeminiReady 判断钉定 gemini（PreferRunner="gemini"）此刻能否派发：gemini_bin 已配
// 且车道不在冷却。不满足时跳过本轮，**绝不 fail-open 回 claude**（与 codex/引擎钉定同一纪律）。
// 钉定径不要求 codexEligible：gemini 有会话（--session-id/--resume），多步可用。
func pinnedGeminiReady(root string, cfg *Config, now time.Time) bool {
	if cfg == nil || cfg.GeminiBin == "" {
		return false
	}
	return !loadEngineCooldown(root, "gemini").active(now)
}

// geminiDivertOK 判断 claude 被冷却/红线拦住时，该卡能否改道 gemini。
// 质量地板与 codexDivertOK 全量同规（一条不松）：无会话可断（codexEligible 形状）、
// no_fallback_models 钉定模型不降级、交叉卡引擎身份不偷换、复审位恒不降级；
// 额外一条：gemini 车道自己不在冷却（与 engineDivertOK 同规）。
func geminiDivertOK(root string, cfg *Config, t *Task, now time.Time) bool {
	if cfg == nil || t == nil || cfg.GeminiBin == "" {
		return false
	}
	if loadEngineCooldown(root, "gemini").active(now) {
		return false
	}
	if !codexEligible(t) {
		return false
	}
	if noFallback(cfg, t.Model) {
		return false
	}
	if t.Type == typeCrossCheck {
		return false
	}
	if qualityFloorCard(t) {
		return false
	}
	return true
}
