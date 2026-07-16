package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type usageInfo struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

type claudeResult struct {
	Type         string     `json:"type"`
	Subtype      string     `json:"subtype"`
	IsError      bool       `json:"is_error"`
	Result       string     `json:"result"`
	SessionID    string     `json:"session_id"`
	NumTurns     int        `json:"num_turns"`
	TotalCostUSD float64    `json:"total_cost_usd"`
	DurationMS   int64      `json:"duration_ms"`
	Usage        *usageInfo `json:"usage"`
}

var (
	// 已知的限额提示形态：
	//   "Claude AI usage limit reached|1751600000"（headless 常见，带重置的 unix 时间戳）
	//   "You've reached your usage limit ... resets at 3pm"
	//   "5-hour limit reached"
	//   "You've hit your session limit · resets 8:20pm (Asia/Singapore)"（远端账号常见；
	//   曾因 "hit your limit" 中间多了 session 一词不匹配 → 走普通失败路径烧 attempts 假失败）
	limitRe     = regexp.MustCompile(`(?i)usage limit|limit reached|out of extra usage|hit your (?:\w+ )?limit|limit will reset|out of usage credits|out of credits|/usage-credits|session limit`)
	epochRe     = regexp.MustCompile(`\|\s*(\d{9,11})`)
	resetTimeRe = regexp.MustCompile(`(?i)reset[s]?\s*(?:at\s*)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?`)
	// resetDateRe：跨天限额（周限额）措辞——"resets Jul 16 at 1am (Asia/Shanghai)"。
	// resetTimeRe 只认紧跟 reset 的钟点、跨不过中间的 "Jul 16 at" → 落 30min 回退，
	// 之后每 30min 醒来再 429 再暂停空转到真解冻。这里带月+日先解析，覆盖多日窗口。
	resetDateRe = regexp.MustCompile(`(?i)reset[s]?\s+(?:on\s+)?(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-z]*\.?\s+(\d{1,2})(?:st|nd|rd|th)?\s+(?:at\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?`)
	// transientRe：可退避重试的瞬时错误。补齐 codex/上游常见网络形态——否则真错误被误判为硬失败，
	// 烧完 attempts 直接 failed（单腿审核一挂就没了）。
	transientRe = regexp.MustCompile(`(?i)rate.?limit|overloaded|internal server error|api error 5\d\d|econnre|network error|timed? ?out|stream (?:error|disconnect)|disconnected before|connection (?:reset|refused|closed|error)|error sending request|temporarily unavailable|502|503|read tcp|i/o timeout`)
	// codexNoiseRe：codex exec 的横幅/配置/进度噪声行——永远不是错误，错误提取时跳过。
	codexNoiseRe = regexp.MustCompile(`(?i)^(reading additional input|openai codex v|-{3,}$|workdir:|model:|provider:|approval:|sandbox:|reasoning|session id:|user$|codex$|assistant$|tokens used|deprecated:|enable it with|hook:|see https?://|[\d,]+$)`)
	// codexHardErrRe：codex/上游服务端「硬错误」明确措辞（区别于泛化 codexErrRe：只认这些具体标识、
	// 不会误命中审查正文里的 "cannot/error"）。含 OpenAI 网络安全审查闸——账号跑对抗性安全复审多了会被
	// 累计打标、开额外安全检查甚至整请求拦截（"flagged for possible cybersecurity risk"），这不是 claudego
	// 能修的，但要让它清晰上报，别被吞成"无最终消息"。非瞬时，走 attempts 退避后可见失败。
	codexHardErrRe = regexp.MustCompile(`(?i)flagged for possible cybersecurity|access blocked by cloudflare|openai-authorization-error|authorization error|experiencing high load|goal budget reached|conversation interrupted|usage limit|quota exceeded|401 unauthorized|403 forbidden|invalid api key`)
	fencedRe       = regexp.MustCompile("(?s)```json\\s*(.*?)```")
	// emit 容错阶梯用：任意语言标签的围栏（模型常漏写 json 标签）与输出中提到的 .json 文件名。
	anyFencedRe = regexp.MustCompile("(?s)```[a-zA-Z]*[ \t]*\\n?(.*?)```")
	jsonFileRe  = regexp.MustCompile(`[\w./\\-]+\.json`)
)

func invokeClaude(ctx context.Context, cfg *Config, t *Task, prompt string) (*claudeResult, string, error) {
	args := []string{"-p", "--output-format", "json"}
	if t.SessionID != "" {
		args = append(args, "--resume", t.SessionID)
	}
	if t.Model != "" {
		args = append(args, "--model", t.Model)
	}
	if t.Effort != "" {
		args = append(args, "--effort", t.Effort)
	}
	if t.SkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	} else if t.PermissionMode != "" {
		args = append(args, "--permission-mode", t.PermissionMode)
	}
	if len(t.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(t.AllowedTools, ","))
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.StepTimeoutMin)*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, cfg.ClaudeBin, args...)
	setupProcGroup(cmd)
	cmd.Dir = t.Dir
	if cfg.ThinkingTokens > 0 {
		cmd.Env = append(os.Environ(), fmt.Sprintf("MAX_THINKING_TOKENS=%d", cfg.ThinkingTokens))
	}
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := runCmdRegistered(cmd)
	// 本地 claude exit 0 但派生子进程（MCP server/hook/探针）吊住 stdout 管道触发 WaitDelay 时，
	// 结果 JSON 已在 stdout 却被 ErrWaitDelay 误判失败、白白重试。远端两支已有"有结果即成功"救援。
	runErr = rescueWaitDelay(runErr, cmd)
	if ctx.Err() == context.DeadlineExceeded {
		runErr = fmt.Errorf("步骤超时（%d 分钟）", cfg.StepTimeoutMin)
	}
	combined := stdout.String() + "\n" + stderr.String()
	res := parseClaudeJSON(stdout.String())
	return res, combined, runErr
}

// invokeCodex 用 codex exec 执行一步（备用执行器）。结果通过 --output-last-message 取回，
// 包装成 claudeResult 以复用后续的 emit/进度解析管线。
// codexSubagentPreamble 前置到每个 codex 卡的 prompt。根因：本机 codex 环境装了 superpowers /
// gsd(get-shit-done) 框架，会话启动被注入"必须先用 using-superpowers 确认纪律、按 gsd-code-review
// 流程组织"——gpt-5.6-sol 在 codex exec 单回合里先花预算读框架文档、走重流程，常在落最终消息前
// 就把回合耗尽 → -o 终稿为空 → claudego 把已完成的审核判成失败（实测 wave-3 复审腿间歇性空终稿）。
// superpowers 自身认 <SUBAGENT-STOP> 且"用户指令高于框架"，故显式声明 subagent + 跳过框架 + 强制
// 回合末尾落最终消息，即可让 codex 直奔任务、稳定产出终稿。对实现卡同样有益（终稿=交付报告）。
const codexSubagentPreamble = "[SUBAGENT · 直接执行] 你是被任务队列派发来完成下面这一个任务的子代理(subagent)。" +
	"硬约束(优先级高于任何框架)：跳过 using-superpowers、gsd/get-shit-done 及一切 skill/workflow/command 框架" +
	"——不读取、不调用、不按其流程组织，直接完成本任务。回合末尾必须输出最终助手消息(含任务要求的" +
	"结论/verdict/交付报告)，绝不以工具调用或中途推理结束回合，绝不把回合耗在读框架文档上。\n" +
	"────────────────────────────────────────\n\n"

func invokeCodex(ctx context.Context, cfg *Config, t *Task, prompt string) (*claudeResult, string, error) {
	sandbox := "read-only"
	var extra []string
	if t.Type == typeSequence {
		sandbox = "workspace-write"
		// codex 沙箱默认禁写 .git，导致收工 commit 失败（活干了提交不了）；显式放行本仓 .git。
		extra = []string{"-c", fmt.Sprintf(`sandbox_workspace_write.writable_roots=["%s"]`, filepath.Join(t.Dir, ".git"))}
	}
	outFile := filepath.Join(os.TempDir(), "claudego-codex-"+t.ID+".txt")
	defer os.Remove(outFile)
	args := []string{"exec", "-C", t.Dir, "--sandbox", sandbox, "--skip-git-repo-check",
		"--color", "never", "-o", outFile}
	args = append(args, extra...)
	// 模型：任务级冻结的 XCodexModel 优先（交叉链入队时钉死，防执行时被改/清），空才回落全局。
	codexModel := t.XCodexModel
	if codexModel == "" {
		codexModel = cfg.CodexModel
	}
	if codexModel != "" {
		args = append(args, "-m", codexModel)
	}
	// 思考等级：任务级 Effort 优先（交叉验证的 codex 卡据此跑指定档，如 max），空则回落全局 codex_reasoning。
	// codex 与 claude 的档位同名同序，故 Task.Effort 直接复用为 model_reasoning_effort。
	if reasoning := t.Effort; reasoning != "" {
		args = append(args, "-c", "model_reasoning_effort="+reasoning)
	} else if cfg.CodexReasoning != "" {
		args = append(args, "-c", "model_reasoning_effort="+cfg.CodexReasoning)
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.StepTimeoutMin)*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, cfg.CodexBin, args...)
	setupProcGroup(cmd)
	cmd.Dir = t.Dir
	// prompt 走 stdin（codex exec 无 prompt 参数时读 stdin），同 invokeRemoteClaude/invokeClaude——
	// 不再把 prompt 当 argv 参数，绕开 ARG_MAX 上限，交叉验证的合并 prompt 可注入完整甲/乙结论不截断。
	// 前置 subagent 前导，抑制 superpowers/gsd 框架注入耗尽回合预算致空终稿（见 codexSubagentPreamble）。
	cmd.Stdin = strings.NewReader(codexSubagentPreamble + prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := runCmdRegistered(cmd)
	// codex exit 0 但派生子进程吊住 stdout 管道触发 WaitDelay 时，-o 结果文件已写好却因 ErrWaitDelay
	// 被下方 `runErr != nil` 判定标 IsError、白白重试。同 runReviewSync/invokeClaude 的同类救援。
	runErr = rescueWaitDelay(runErr, cmd)
	if ctx.Err() == context.DeadlineExceeded {
		runErr = fmt.Errorf("步骤超时（%d 分钟）", cfg.StepTimeoutMin)
	}
	combined := stdout.String() + "\n" + stderr.String()
	last, _ := os.ReadFile(outFile)
	res := &claudeResult{Type: "result", Result: strings.TrimSpace(string(last))}
	switch {
	case runErr != nil:
		// 进程真出错/超时：先挑瞬时网络样式行（据此退避快重试），没有则用真实 runErr（如"步骤超时"）。
		// 不再拿泛化 codexErrRe 从 prose transcript 里瞎抓行——那会把审查正文里一句无害"cannot/error"当错误。
		res.IsError = true
		res.Subtype = "codex_error"
		if res.Result == "" {
			if line := codexErrorLine(combined); line != "" {
				res.Result = line
			} else {
				res.Result = firstLine(runErr.Error())
			}
		}
	case res.Result == "":
		// 进程正常退出(task_complete)但 -o 终稿为空：codex 回合末尾停在工具调用/推理，没落最终消息。
		// 多因 skill/workflow 框架注入耗尽回合预算（已加 codexSubagentPreamble 抑制）。非瞬时错误，
		// 给明确诊断而非从 transcript 里瞎抓一行——重试(带前导)通常可成。
		res.IsError = true
		res.Subtype = "codex_no_final_message"
		res.Result = "codex 回合完成但未产出最终消息(-o 空,末尾停在工具调用/推理)——常因 skill/workflow 框架注入耗尽预算;已加 subagent 前导抑制,重试通常可成"
	}
	return res, combined, runErr
}

// invokeRemoteClaude 通过 SSH 在远程主机上跑 claude -p（远程 opus 设计等，用该主机自己的 claude 账号）。
// prompt 走 ssh stdin；claude -p --output-format json 直接把结果 JSON 打到 stdout（无需 marker/文件，复用 parseClaudeJSON）。
// claude -p 无 -C 参数，故先 cd 到工作目录（cmd 用 cd /d + 反斜杠，posix 用 cd + 正斜杠）。
func invokeRemoteClaude(ctx context.Context, cfg *Config, t *Task, prompt string) (*claudeResult, string, error) {
	rh, ok := cfg.RemoteHosts[t.RemoteHost]
	if !ok {
		return &claudeResult{Type: "result", IsError: true, Subtype: "remote_config"}, "",
			fmt.Errorf("未配置远程主机 %q（config.remote_hosts）", t.RemoteHost)
	}
	sshBin := cfg.SSHBin
	if sshBin == "" {
		sshBin = "ssh"
	}
	claudeBin := rh.ClaudeBin
	if claudeBin == "" {
		claudeBin = "claude"
	}
	cdCmd, dir := "cd /d", strings.ReplaceAll(t.Dir, "/", `\`)
	if rh.Shell == "posix" {
		cdCmd, dir = "cd", t.Dir
	}
	args := claudeBin + " -p --output-format json"
	if t.Model != "" {
		args += " --model " + t.Model
	}
	if t.Effort != "" {
		args += " --effort " + t.Effort
	}
	if t.SkipPermissions {
		args += " --dangerously-skip-permissions"
	} else if t.PermissionMode != "" {
		args += " --permission-mode " + t.PermissionMode
	}
	// allowedTools 清单含空格（如 "Bash(python3 -m pytest:*)"），拼进远程 shell 串必须整体加引号，
	// 否则 cmd/posix 都会按空格劈开、碎片被 claude 当独立参数（实测炸出 unknown option '-m'）。
	// skip-permissions 下清单本就无效，干脆不传，少一段引号地狱。
	if len(t.AllowedTools) > 0 && !t.SkipPermissions {
		args += ` --allowedTools "` + strings.Join(t.AllowedTools, ",") + `"`
	}
	remoteCmd := fmt.Sprintf(`%s "%s" && %s`, cdCmd, dir, args)

	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.StepTimeoutMin)*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, sshBin, "-o", "BatchMode=yes", t.RemoteHost, remoteCmd)
	setupProcGroup(cmd)
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr syncBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// 早收割看门狗：结果 JSON 已完整落缓冲而 ssh 因远端孙进程吊管道不退时，
	// 两拍后整组击杀（实测曾挂满 150 分钟：完成品不被收割 + 目录锁堵死串行队列）。
	runErr := runCmdRegisteredHarvest(cmd, func() bool {
		return parseClaudeJSON(stdout.String()) != nil
	})
	if ctx.Err() == context.DeadlineExceeded {
		runErr = fmt.Errorf("远程步骤超时（%d 分钟）", cfg.StepTimeoutMin)
	}
	combined := stdout.String() + "\n" + stderr.String()
	res := parseClaudeJSON(stdout.String())
	if res == nil {
		res = &claudeResult{Type: "result", IsError: true, Subtype: "remote_claude_error", Result: firstLine(combined)}
	} else if runErr != nil && !res.IsError {
		// 远端 claude 已把完整结果 JSON 打到 stdout,但收尾被吊住（见上）或退出竞态非零——
		// 结果在手即成功：看门狗击杀的退出码在此洗白（同 invokeRemoteCodex 的"有结果即成功"原则）。
		runErr = nil
	}
	return res, combined, runErr
}

// invokeRemoteCodex 通过 SSH 在远程主机上跑 codex exec（让远端主机进编排）。
// prompt 走 ssh stdin 灌进 codex（codex exec 无 prompt 参数时读 stdin），彻底绕开 Windows cmd 引号；
// 结果由远端 codex 写到 -o 文件，再用 marker + type/cat 回捕到 stdout，隔开 codex 的执行日志噪声。
// 远端 codex 走自己的 GPT 额度：不记 claude 账本、不写全局冷却。安全靠 prompt 护栏 + 人工审 diff。
func invokeRemoteCodex(ctx context.Context, cfg *Config, t *Task, prompt string) (*claudeResult, string, error) {
	rh, ok := cfg.RemoteHosts[t.RemoteHost]
	if !ok {
		return &claudeResult{Type: "result", IsError: true, Subtype: "remote_config"}, "",
			fmt.Errorf("未配置远程主机 %q（config.remote_hosts）", t.RemoteHost)
	}
	sshBin := cfg.SSHBin
	if sshBin == "" {
		sshBin = "ssh"
	}
	codexBin := rh.CodexBin
	if codexBin == "" {
		codexBin = "codex"
	}
	sandbox := rh.Sandbox
	if sandbox == "" {
		sandbox = "workspace-write"
	}
	// 只读类任务（如交叉验证卡）强制 read-only：不写业务仓，与本机 invokeCodex 的按类型选沙箱一致，
	// 也不被主机配的 workspace-write/danger-full-access 放宽（写权限只给真正要落码的 sequence 卡）。
	if t.Type != typeSequence {
		sandbox = "read-only"
	}
	tmp := rh.TmpDir
	if tmp == "" {
		tmp = "."
	}
	outFile := tmp + "/claudego-remote-" + t.ID + ".txt"

	const marker = "===CLAUDEGO_REMOTE_RESULT==="
	// 远端 shell 差异：cmd（Windows，默认）用 & 分隔 + type + 反斜杠路径；posix 用 ; + cat + 正斜杠。
	sep, catCmd, printPath := "&", "type", strings.ReplaceAll(outFile, "/", `\`)
	if rh.Shell == "posix" {
		sep, catCmd, printPath = ";", "cat", outFile
	}
	// codex -C / -o 用正斜杠（codex 自会规范化写盘）；结果打印用 shell 对应的路径分隔符。
	remoteCmd := fmt.Sprintf(`%s exec -C "%s" --sandbox %s --skip-git-repo-check --color never -o "%s"`,
		codexBin, t.Dir, sandbox, outFile)
	// 思考等级：任务级 Effort 优先（交叉验证远端 codex 卡据此跑指定档），空则回落该主机 reasoning 配置。
	if reasoning := t.Effort; reasoning != "" {
		remoteCmd += " -c model_reasoning_effort=" + reasoning
	} else if rh.Reasoning != "" {
		remoteCmd += " -c model_reasoning_effort=" + rh.Reasoning
	}
	codexModel := t.XCodexModel
	if codexModel == "" {
		codexModel = cfg.CodexModel
	}
	if codexModel != "" {
		remoteCmd += " -m " + codexModel
	}
	remoteCmd += fmt.Sprintf(` %s echo %s %s %s "%s"`, sep, marker, sep, catCmd, printPath)

	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.StepTimeoutMin)*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, sshBin, "-o", "BatchMode=yes", t.RemoteHost, remoteCmd)
	setupProcGroup(cmd)
	// 前置 subagent 前导，与本机 invokeCodex 同治（远端若也装了 superpowers/gsd 同样抑制；没装则无害）。
	cmd.Stdin = strings.NewReader(codexSubagentPreamble + prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := runCmdRegistered(cmd)
	if ctx.Err() == context.DeadlineExceeded {
		runErr = fmt.Errorf("远程步骤超时（%d 分钟）", cfg.StepTimeoutMin)
	}
	combined := stdout.String() + "\n" + stderr.String()

	// marker 之后的 stdout 即结果文件内容（LastIndex 隔开 codex exec 的执行日志噪声）。
	result := ""
	if idx := strings.LastIndex(stdout.String(), marker); idx >= 0 {
		result = strings.TrimSpace(stdout.String()[idx+len(marker):])
	}
	res := &claudeResult{Type: "result", Result: result}
	// 拿到 -o 结果文件内容即视为成功：Windows codex exec 常因非致命告警（model refresh 超时等）
	// 退出非零，退出码不足为凭；成功与否由是否产出终稿 + 人工审 diff 判定。
	if result != "" {
		return res, combined, nil
	}
	res.IsError = true
	if line := codexErrorLine(combined); line != "" {
		res.Subtype = "remote_codex_error"
		res.Result = line
	} else {
		res.Subtype = "remote_codex_no_final_message"
		res.Result = "远端 codex 回合完成但 marker 后无终稿——常因框架注入耗尽预算/未落最终消息;已加 subagent 前导抑制"
	}
	if runErr == nil {
		runErr = fmt.Errorf("远端 codex 无结果输出（marker 后为空）")
	}
	return res, combined, runErr
}

// remoteUsesClaude 判定远端任务走远端 claude(true)还是远端 codex(false)：
// 带 claude 模型走 claude；只读审核卡(typeReview)强制走 claude——审核分流的立项目标是平衡
// 两侧 claude 额度,空 Model 也绝不路由到烧 GPT 额度的远端 codex(空则用远端账号默认模型)。
func remoteUsesClaude(t *Task) bool {
	return t.Model != "" || t.Type == typeReview
}

// codexEligible 判断任务能否交给备用执行器：没有 claude 会话要延续即可——
// 单步未开跑的任务，以及 fresh_steps 任务的任意一步（状态在文件里，谁来跑都一样）。
func codexEligible(t *Task) bool {
	if t.SessionID != "" || t.MidStep {
		return false
	}
	return t.FreshSteps || (len(t.Prompts) == 1 && t.Step == 0)
}

func parseClaudeJSON(out string) *claudeResult {
	var res claudeResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &res); err == nil && res.Type != "" {
		return &res
	}
	// 输出前可能混入了非 JSON 行，逐行找 result 对象
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `{"type"`) {
			continue
		}
		var r claudeResult
		if err := json.Unmarshal([]byte(line), &r); err == nil && r.Type == "result" {
			return &r
		}
	}
	return nil
}

func isLimitHit(res *claudeResult, combined string) bool {
	if res != nil && !res.IsError {
		return false
	}
	text := combined
	if res != nil {
		text += "\n" + res.Result
	}
	return limitRe.MatchString(text)
}

// parseResetEpoch 从错误输出中解析限额重置时间；解析不到则用配置的回退等待。
func parseResetEpoch(text string, cfg *Config, now time.Time) int64 {
	margin := int64(cfg.CooldownMarginSec)
	if m := epochRe.FindStringSubmatch(text); m != nil {
		if v, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			if v > now.Unix()-600 && v < now.Add(26*time.Hour).Unix() {
				return clampEpoch(v+margin, now)
			}
		}
	}
	// 跨天窗口（周限额）：带月+日的措辞先于纯钟点解析，避免落 30min 回退后空转到真解冻。
	if m := resetDateRe.FindStringSubmatch(text); m != nil {
		if mon := monthNum(m[1]); mon != 0 {
			day, _ := strconv.Atoi(m[2])
			hour, _ := strconv.Atoi(m[3])
			minute := 0
			if m[4] != "" {
				minute, _ = strconv.Atoi(m[4])
			}
			switch strings.ToLower(m[5]) {
			case "pm":
				if hour < 12 {
					hour += 12
				}
			case "am":
				if hour == 12 {
					hour = 0
				}
			}
			if day >= 1 && day <= 31 && hour < 24 {
				cand := time.Date(now.Year(), mon, day, hour, minute, 0, 0, now.Location())
				// 跨年：如 12 月的重置在 1 月被读到，滚到明年（留 1 天容差防边界抖动）。
				if cand.Before(now.Add(-24 * time.Hour)) {
					cand = cand.AddDate(1, 0, 0)
				}
				// 只信 14 天内的重置；越界（年份错算等）宁可退回配置回退。
				if cand.After(now) && cand.Before(now.Add(14*24*time.Hour)) {
					return clampEpoch(cand.Unix()+margin, now)
				}
			}
		}
	}
	if m := resetTimeRe.FindStringSubmatch(text); m != nil {
		hour, _ := strconv.Atoi(m[1])
		minute := 0
		if m[2] != "" {
			minute, _ = strconv.Atoi(m[2])
		}
		switch strings.ToLower(m[3]) {
		case "pm":
			if hour < 12 {
				hour += 12
			}
		case "am":
			if hour == 12 {
				hour = 0
			}
		}
		if hour < 24 {
			cand := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
			if !cand.After(now) {
				cand = cand.Add(24 * time.Hour)
			}
			return clampEpoch(cand.Unix()+margin, now)
		}
	}
	return now.Add(time.Duration(cfg.LimitFallbackMin)*time.Minute).Unix() + margin
}

// monthNum 把英文月份缩写（jan..dec）映射到 time.Month；不识别返回 0。
func monthNum(s string) time.Month {
	switch strings.ToLower(s[:3]) {
	case "jan":
		return time.January
	case "feb":
		return time.February
	case "mar":
		return time.March
	case "apr":
		return time.April
	case "may":
		return time.May
	case "jun":
		return time.June
	case "jul":
		return time.July
	case "aug":
		return time.August
	case "sep":
		return time.September
	case "oct":
		return time.October
	case "nov":
		return time.November
	case "dec":
		return time.December
	}
	return 0
}

func clampEpoch(v int64, now time.Time) int64 {
	if min := now.Unix() + 120; v < min {
		return min
	}
	return v
}

// runTask 在一次派发内循环执行任务的各个步骤，直到完成、失败、撞上限额或被取消。
// 同一任务的多个步骤通过 --resume 复用同一会话，保持上下文连续。
// useCodex 为 true 时用备用执行器 codex exec（限单步任务，冷却/红线期间由 tick 决定）。
// ctx 由 tick 持有：任务被 cancel 后 tick 对账发现即取消 ctx，整组击杀执行进程。
func runTask(ctx context.Context, root string, cfg *Config, t *Task, useCodex bool) error {
	// 守门：pickNext 之后、开跑之前任务可能刚被 cancel（非运行态 cancel 直接归档移走文件），
	// 别把已取消的任务写回 tasks/ 复活成 running。
	if diskCanceled(root, t.ID) {
		t.Status = statusCanceled
		_ = archiveTask(root, t)
		return nil
	}
	t.Status = statusRunning
	// 交叉 C 重跑（如 claudego retry）先撤下旧的终局报告：否则若这次在执行器层就失败（未进 postComplete），
	// 上一轮的旧报告仍会被 progress -show 当成当前终局。首跑时无报告可删，无害。
	if t.XRole == "C" {
		_ = os.Remove(progressPath(root, t.ProgressKey))
	}
	remote := t.RemoteHost != ""
	switch {
	case remote:
		t.Runner = "remote:" + t.RemoteHost
	case useCodex:
		t.Runner = "codex"
	default:
		t.Runner = "" // 清除历史执行器标签（如降级失败后的重试回到 claude）
	}
	t.touch()
	if err := saveTask(root, t); err != nil {
		return err
	}
	lg, err := openTaskLog(root, t.ID)
	if err != nil {
		return err
	}
	defer lg.Close()

	for {
		now := time.Now()
		// 多步任务在步骤之间复查红线：越线则回到排队（会话与进度保留），等窗口滑走。
		if t.Step > 0 && !t.MidStep {
			if blocked, reason := budgetBlocked(root, cfg, now); blocked {
				t.Status = statusQueued
				t.NotBeforeEpoch = now.Add(5 * time.Minute).Unix()
				t.LastError = "额度红线: " + reason
				t.touch()
				logBlock(lg, "BUDGET", reason)
				return saveTask(root, t)
			}
		}
		var prompt string
		resuming := t.MidStep && t.SessionID != ""
		switch {
		case resuming:
			prompt = cfg.ResumePrompt
		case t.Step < len(t.Prompts):
			prompt = t.Prompts[t.Step]
			// 交叉卡不走 injectLiveContext：它们的 prompt 不用 {{QUEUE}}/{{PROGRESS}}，而注入的甲/乙结论或
			// 用户任务若含这些字面量会被二次替换污染（非确定/注入）。
			if t.Type != typeCrossCheck {
				prompt = injectLiveContext(root, t.ID, prompt)
			}
		default:
			t.Status = statusDone
			t.touch()
			return saveTask(root, t)
		}

		logSection(lg, fmt.Sprintf("步骤 %d/%d%s  session=%s%s", t.Step+1, len(t.Prompts),
			map[bool]string{true: "（限额中断后续跑）", false: ""}[resuming], orDash(t.SessionID),
			map[bool]string{true: "  runner=codex", false: ""}[useCodex]))
		logBlock(lg, "PROMPT", prompt)

		var res *claudeResult
		var combined string
		var runErr error
		switch {
		case remote:
			// 远端执行：带 claude 模型(如 opus)走远端 claude；只读审核卡强制走远端 claude——
			// 审核分流的立项目标是平衡两侧 claude 额度,绝不把审核路由到烧 GPT 额度的远端 codex
			// (即便 Model 为空,空则用远端账号默认模型)。其余无模型的填充类任务仍走远端 codex。
			// 两者都走该远端主机自己的账号额度，不记本机 claude 账本、不写全局冷却。
			if remoteUsesClaude(t) {
				res, combined, runErr = invokeRemoteClaude(ctx, cfg, t, prompt)
			} else {
				res, combined, runErr = invokeRemoteCodex(ctx, cfg, t, prompt)
			}
		case useCodex:
			// codex 走自己的额度：不记 claude 账本；其限额/错误按普通错误退避，不写全局冷却。
			res, combined, runErr = invokeCodex(ctx, cfg, t, prompt)
		default:
			res, combined, runErr = invokeClaude(ctx, cfg, t, prompt)
			if res != nil && res.SessionID != "" {
				t.SessionID = res.SessionID
			}
			if res != nil {
				appendUsage(root, cfg, t, res.Usage)
			}
		}

		// 0) 取消：tick 对账发现盘上已标 canceled 后取消 ctx 击杀进程组；也可能进程
		// 自然结束后才发现取消标记（cancel 落在步骤间隙）。两种都按取消收尾：
		// 产物丢弃（远端"结果在手即成功"的救援同样让位），不再把内存态写回任务文件。
		// 残余竞态窗口：cancel 恰落在本检查与本步 saveTask 之间的微秒级间隙会被盖掉，
		// 由 tick 的周期对账兜底不了（文件已非 canceled），接受——窗口从整步时长缩到微秒。
		if ctx.Err() != nil || diskCanceled(root, t.ID) {
			return finalizeCanceled(root, t, lg)
		}

		// 1) 限额：记录恢复时间，全局冷却，等 tick 到点自动续跑
		if !useCodex && !remote && isLimitHit(res, combined) {
			until := parseResetEpoch(combined+"\n"+resultText(res), cfg, now)
			setCooldown(root, until, firstLine(combined))
			t.Status = statusLimitPaused
			t.ResumeAtEpoch = until
			t.MidStep = t.SessionID != ""
			if t.FreshSteps {
				// 状态在文件里：恢复时重发本步 prompt 开新会话即可，不需要续跑提示。
				t.SessionID = ""
				t.MidStep = false
			}
			t.LastError = "usage limit: " + firstLine(combined)
			t.touch()
			logBlock(lg, "LIMIT", fmt.Sprintf("命中用量限额，%s 后恢复（%s）\n%s", fmtIn(until, now), fmtClock(until), firstLine(combined)))
			return saveTask(root, t)
		}

		// 1a) 本机 codex 撞自己的 ChatGPT 用量限额：按本任务 resume_at 挂起，绝不写全局
		// claude 冷却（两边账号额度独立）；eligible() 到滚动窗恢复时会重派并重发本步。
		if useCodex && !remote && isLimitHit(res, combined) {
			until := parseResetEpoch(combined+"\n"+resultText(res), cfg, now)
			t.Status = statusLimitPaused
			t.ResumeAtEpoch = until
			t.MidStep = false
			t.SessionID = "" // codex 无会话可续，恢复时重发本步
			t.LastError = "codex 用量限额: " + firstLine(resultText(res))
			t.touch()
			logBlock(lg, "LIMIT", fmt.Sprintf("codex 用量限额，%s 后恢复（%s）", fmtIn(until, now), fmtClock(until)))
			return saveTask(root, t)
		}

		// 1b) 远端撞该主机账号限额（远端机器自己的 claude/GPT 账号）：按本任务 resume_at 挂起，
		// 不写全局冷却（远端账号与本机独立）；eligible() 到刷新时刻才再派 → 无损接力自动续跑。
		if remote && isLimitHit(res, combined) {
			until := parseResetEpoch(combined+"\n"+resultText(res), cfg, now)
			t.Status = statusLimitPaused
			t.ResumeAtEpoch = until
			t.MidStep = false
			t.SessionID = "" // 远端无会话可续，恢复时重发本步开新会话
			t.LastError = "远端账号限额: " + firstLine(resultText(res))
			t.touch()
			logBlock(lg, "LIMIT", fmt.Sprintf("远端账号限额，%s 后恢复（%s）", fmtIn(until, now), fmtClock(until)))
			return saveTask(root, t)
		}

		// 2) 其他失败：退避重试，超过次数则失败
		if runErr != nil || res == nil || res.IsError {
			msg := errorSummary(res, combined, runErr)
			t.Attempts++
			t.LastError = msg
			logBlock(lg, "ERROR", fmt.Sprintf("第 %d 次失败: %s", t.Attempts, msg))
			if t.Attempts >= cfg.MaxAttempts {
				t.Status = statusFailed
			} else {
				t.Status = statusQueued
				backoff := time.Duration(cfg.RetryBackoffMin) * time.Minute
				if transientRe.MatchString(msg) {
					backoff = time.Duration(cfg.RetryBackoffMin) * time.Minute
				} else {
					backoff *= time.Duration(t.Attempts)
				}
				t.NotBeforeEpoch = now.Add(backoff).Unix()
			}
			t.touch()
			return saveTask(root, t)
		}

		// 3) 成功：推进步骤（codex/远端成功不代表 claude 限额解除，冷却只由 claude 路径清除）
		if !useCodex && !remote {
			clearCooldown(root)
		}
		t.Attempts = 0
		t.NotBeforeEpoch = 0
		t.MidStep = false
		t.Step++
		if t.FreshSteps {
			t.SessionID = "" // 下一步全新会话
		}
		t.TurnsUsed += res.NumTurns
		t.CostUSD += res.TotalCostUSD
		t.LastError = ""
		// 交叉验证 A 卡的结论不落可达面：不写进日志 RESULT、不写进 LastSummary(list 摘要)——
		// 减少引擎乙从盘上被动读到甲的表面(logs/<A>.log 与 tasks/<A>.json 都是绝对路径可 Read 的)。
		// 甲结论仅在隔离侧车(供 C 用)与 C 卡合并 prompt 里可审计。这是被动暴露最小化,非硬沙箱(见 README)。
		if t.XRole != "A" {
			if s := summarizeResult(res.Result); s != "" {
				t.LastSummary = s
			}
			logBlock(lg, "RESULT", res.Result)
		} else {
			logBlock(lg, "RESULT", "[交叉A结论已隔离——不落可达日志,避免引擎乙从盘上读到甲;完整结论见隔离侧车与链汇总 C 卡]")
		}
		logSection(lg, fmt.Sprintf("步骤完成  turns=%d cost=$%.4f duration=%.0fs", res.NumTurns, res.TotalCostUSD, float64(res.DurationMS)/1000))

		if t.Step >= len(t.Prompts) {
			t.Status = statusDone
			// 交叉 C 终局：标 done 前先定合并契约（不合规直接 failed），让**首次落盘即是终态**——
			// 避免"done 先落盘、校验在后、failed 靠第二次保存"的崩溃窗口（reconcile 不覆盖 C）。
			if t.XRole == "C" && !crossMergeVerdictOK(res.Result) {
				t.Status = statusFailed
				t.LastError = "交叉C 结论未按合并契约收尾（缺合法 verdict/confidence），不发布进度、勿采信"
				_ = os.Remove(progressPath(root, t.ProgressKey)) // 清可能残留的陈旧报告，防冒充终局
				logBlock(lg, "CROSS", t.LastError)
			}
			t.touch()
			if err := saveTask(root, t); err != nil {
				return err
			}
			postComplete(root, cfg, t, res, lg)
			// postComplete 期间任务可能被 cancel 并归档（done 态 cancel 走立即归档），
			// 注解回写别把归档移走的文件复活。
			if diskCanceled(root, t.ID) {
				return nil
			}
			return saveTask(root, t)
		}
		t.touch()
		if err := saveTask(root, t); err != nil {
			return err
		}
	}
}

// finalizeCanceled 按取消收尾：执行进程（组）已被击杀或已自然结束，本步产物丢弃，
// 归档盘上的任务文件——cancel 命令对 running 任务只写取消标记不归档（进程还活着），
// 归档由这里补上；文件已被移走（非运行态 cancel 或人工删除）则忽略。
func finalizeCanceled(root string, t *Task, lg *os.File) error {
	t.Status = statusCanceled
	logBlock(lg, "CANCELED", "任务已取消：终止执行进程，丢弃本步产物并归档。")
	if err := archiveTask(root, t); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// runReviewSync 在本地以 sh -c 执行审核分流的同步命令（如把改动 rsync 到审核主机），
// stdout/stderr 落任务日志，120s 超时。返回非 nil 即视为同步失败，调用方回退本地审核。
// 已知竞态（记录不修）：同步在 ~110s+ 完成且孙进程仍吊住管道时，WaitDelay 的 10s 收尾会跨过
// 120s deadline，成功的同步被误报为超时→回退本地审。窗口极窄且回退无害（多审一次本机），不值复杂化。
func runReviewSync(t *Task, lg *os.File) error {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", t.ReviewSync)
	// 同步命令以实现卡 Dir 为工作目录执行：本库本地执行器（invokeClaude/invokeCodex）均 cmd.Dir=t.Dir，
	// 不钉的话命令在 daemon 进程 cwd 跑，用户写 'rsync -a --delete ./ hostb:/mirror/' 等相对路径命令时
	// 会静默同步 daemon 启动目录、--delete 清空远端镜像，审核对错误代码出 verdict 喂进修复链且全程无报错。
	cmd.Dir = t.Dir
	// setupProcGroup 给 cmd 设 Setpgid + Cancel（超时整组击杀）+ WaitDelay=10s：
	// rsync 派生的 ssh 孙进程握住 stdout 管道写端时，只 kill 直接子进程 Wait 永不返回，
	// postComplete 卡死、并行槽位与 dir 互斥永久泄漏（同 b84de71 已踩过的坑）。用 buffer 而非
	// CombinedOutput 内建管道，配合 WaitDelay 在超时/孙进程吊管道时强制收尾。runCmdRegistered
	// 登记进程供 Ctrl-C/SIGTERM 连坐击杀。
	setupProcGroup(cmd)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := runCmdRegistered(cmd)
	// 救援：sync 命令 exit 0 但派生后台子进程握住 stdout 管道（ssh mux、rsync -e ssh）时，
	// WaitDelay 到点 cmd.Wait 返回 ErrWaitDelay 而非 nil，但进程本身成功退出。不救则成功的同步
	// 被误判失败→postComplete 收掉 divert、每轮回退本机审核，分流特性静默失效（已独立探针实测）。
	err = rescueWaitDelay(err, cmd)
	logBlock(lg, "REVIEW-SYNC", fmt.Sprintf("$ %s\n%s", t.ReviewSync, strings.TrimSpace(buf.String())))
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("同步命令超时（120s）")
	}
	return err
}

// applyDefaultReviewDivert 全局默认复审分流（均衡两侧额度）：本地实现卡未显式声明 ReviewHost 时，
// 若 config 三件齐备（default_review_host + remote_mirror_root + default_review_sync），自动把只读复审
// 分流到该主机——推导远端镜像目录 <root>/<worktree 名> + 挂默认同步命令（先把本地泳道同步到远端镜像）。
// 远端实现卡（RemoteHost 非空）已在远端审，不套此默认；任务级 -review-host / -review-dir / -review-sync 显式值恒优先。
func applyDefaultReviewDivert(t *Task, cfg *Config) {
	if t.ReviewHost != "" || t.RemoteHost != "" {
		return
	}
	if cfg.DefaultReviewHost == "" || cfg.RemoteMirrorRoot == "" || cfg.DefaultReviewSync == "" {
		return
	}
	t.ReviewHost = cfg.DefaultReviewHost
	if t.ReviewDir == "" {
		t.ReviewDir = cfg.RemoteMirrorRoot + "/" + filepath.Base(t.Dir)
	}
	if t.ReviewSync == "" {
		t.ReviewSync = cfg.DefaultReviewSync
	}
}

// postComplete 处理任务链：进度报告落盘；装配/协调任务产出的新任务入队；review_after 自动入队设计审核。
func postComplete(root string, cfg *Config, t *Task, res *claudeResult, lg *os.File) {
	// C 存在即意味 B→C 已成，甲结论侧车使命完成——清理（兜住 B→C 在删侧车前崩溃的残留）。
	if t.XRole == "C" {
		_ = os.Remove(crossPeerPath(root, t.XKey))
	}
	// 交叉 C 终局：先验证合并契约，不合规就**不发布 progress**（否则伪终局会被 progress -show 当有效终局
	// 展示，"发布在前、验证在后"会漏），清陈旧报告、置 failed 收尾。
	if t.XRole == "C" && !crossMergeVerdictOK(res.Result) {
		t.Status = statusFailed
		t.LastError = "交叉C 结论未按合并契约收尾（缺合法 verdict/confidence），不发布进度、勿采信为有效终局"
		_ = os.Remove(progressPath(root, t.ProgressKey)) // 清可能残留的旧报告，防 progress -show 冒充终局
		logBlock(lg, "CROSS", t.LastError)
		return
	}
	if t.EmitProgress {
		if key, err := saveProgressFromResult(root, t, res.Result); err != nil {
			t.LastError = "进度报告落盘失败: " + err.Error()
			logBlock(lg, "PROGRESS", t.LastError)
			if t.XRole == "C" {
				t.Status = statusFailed                          // C 的终局报告没落盘=终局缺失，别显示成功
				_ = os.Remove(progressPath(root, t.ProgressKey)) // 清陈旧报告，别留旧的冒充当前终局
			}
		} else {
			logBlock(lg, "PROGRESS", "进度报告已写入: "+progressPath(root, key))
		}
	}
	if t.EmitTasks {
		created, err := enqueueEmitted(root, cfg, t, res.Result)
		if err != nil {
			t.LastError = "解析产出任务失败: " + err.Error()
			// 把原始 json 块存盘：模型常犯"字符串内未转义引号"这类小错，留着人工修复后补投。
			dump := filepath.Join(logsDir(root), t.ID+".emit-failed.json")
			if raw := lastFencedJSON(res.Result); raw != "" {
				_ = os.WriteFile(dump, []byte(raw+"\n"), 0o644)
				logBlock(lg, "EMIT", t.LastError+"\n原始 JSON 已存: "+dump)
			} else {
				logBlock(lg, "EMIT", t.LastError)
			}
		} else {
			logBlock(lg, "EMIT", "已入队: "+strings.Join(created, ", "))
		}
	}
	if t.ReviewAfter {
		tpl, err := loadTemplate(root, typeReview)
		if err == nil {
			// 审核默认在被审卡所在处执行（远程卡在远程审）；声明 ReviewHost/ReviewSync 时把只读审核
			// 负载分流到第二台机器。分流前若有 ReviewSync 先本地同步改动，失败则只收掉 divert——
			// reviewHost/reviewDir 回到初值 t.RemoteHost/t.Dir（远程实现卡仍在远程审，绝不抹主机把审核
			// 错拉回本机以远端路径当本地目录），闭环不断。
			applyDefaultReviewDivert(t, cfg) // 全局默认复审分流（config 配了 default_review_host 时自动压到第二台机器）
			reviewDir := t.Dir
			reviewHost := t.RemoteHost
			divert := t.ReviewHost != ""
			if t.ReviewSync != "" {
				if err := runReviewSync(t, lg); err != nil {
					logBlock(lg, "REVIEW", "审核同步失败，回退本地审核: "+err.Error())
					divert = false
				}
			}
			if divert {
				reviewDir = t.ReviewDir
				reviewHost = t.ReviewHost
			}
			focus := fmt.Sprintf("审查任务「%s」刚刚在该目录产生的改动（可结合 git diff/log）", t.Title)
			prompt := renderTemplate(tpl, map[string]string{"DIR": reviewDir, "FOCUS": focus})
			rv := newTask(root, cfg, typeReview, "审核: "+t.Title, reviewDir, []string{prompt}, t.Priority)
			// 修复闭环谱系：审核卡记住被审卡，并继承其循环轮次与（分流后的）执行位置。
			rv.ReviewOf = t.ID
			rv.FixRound = t.FixRound
			rv.RemoteHost = reviewHost
			// 远程审核（分流或远程实现卡回退）的模型选取：非空烘焙值恒优先，空才继承实现卡模型。
			// newTask 已从 type_defaults.review.model 烘焙 rv.Model；仅当它为空（typeDefaults 未配审核模型）
			// 才继承实现卡 Model，保证远端命中 claude 而非烧 GPT 额度的 codex。绝不用实现卡 Model 覆写非空烘焙值——
			// 否则 type_defaults.review.model=opus + 实现卡 -model haiku 时审核会被静默降级成 haiku（本地审核却仍用
			// opus），配置的审核模型被践踏。runTask 对 typeReview 亦兜底走远端 claude，空 Model 不会被路由到远端 codex。
			if reviewHost != "" && rv.Model == "" {
				rv.Model = t.Model
			}
			if saveTask(root, rv) == nil {
				logBlock(lg, "REVIEW", "已入队审核任务: "+rv.ID)
			}
		}
	}
	// 修复闭环：对抗审核完成后消费 verdict——pass 收口；concerns/block 自动派下一轮修复卡；
	// 超轮限挂 held 升级卡交人工。这是"实现→审核→修复→再审"循环的自动闭合点。
	if t.Type == typeReview {
		handleReviewVerdict(root, cfg, t, res.Result, lg)
	}
	// 交叉验证链：A 完成 → 派独立引擎乙的 B 卡（不注入 A）；B 完成 → 派引擎乙交叉查漏的 C 卡（注入 A+B）。
	// C 是终点，交编排者综合（C 的 json 结论经 EmitProgress 落进度报告）。
	if t.XRole == "A" || t.XRole == "B" {
		handleCrossStage(root, cfg, t, res, lg)
	}
}

// crossMergeVerdictOK 判断 C 的输出是否按合并契约收尾：末尾有 json 块，含非空 verdict + 枚举 confidence。
// confidence 枚举校验拦住 {"verdict":"banana"} 这类无结构 verdict 的伪合规终局（缺 confidence 即判不合规）。
func crossMergeVerdictOK(result string) bool {
	raw := lastFencedJSON(result)
	if raw == "" {
		return false
	}
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) != nil {
		return false
	}
	if v, _ := m["verdict"].(string); strings.TrimSpace(v) == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(rptStr(m["confidence"]))) {
	case "high", "medium", "low":
		return true
	}
	return false
}

type emitTask struct {
	Title       string   `json:"title"`
	Type        string   `json:"type"`
	Dir         string   `json:"dir"`
	Priority    int      `json:"priority"`
	Model       string   `json:"model"`
	SessionID   string   `json:"session_id"`
	ReviewAfter bool     `json:"review_after"`
	FreshSteps  bool     `json:"fresh_steps"`
	Runner      string   `json:"runner"`
	Prompts     []string `json:"prompts"`
	// 模型常见的字段名漂移，做别名容错：steps=[...] / prompt="..." / 标题写成 role 或 id。
	Steps  []string `json:"steps"`
	Prompt string   `json:"prompt"`
	Role   string   `json:"role"`
	ID     string   `json:"id"`
	// Effort 思考等级（low/medium/high/xhigh/max），非法值忽略。
	Effort string `json:"effort"`
}

type emitSpec struct {
	Tasks []emitTask `json:"tasks"`
}

// parseEmitTasks 尝试把一段文本按 {"tasks":[...]} 或裸数组 [...] 解析成任务清单。
// 解析不出或列表为空返回 nil。
func parseEmitTasks(raw string) []emitTask {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var spec emitSpec
	if err := json.Unmarshal([]byte(raw), &spec); err == nil && len(spec.Tasks) > 0 {
		return spec.Tasks
	}
	var arr []emitTask
	if err := json.Unmarshal([]byte(raw), &arr); err == nil && len(arr) > 0 {
		return arr
	}
	return nil
}

// extractEmitTasks 从模型输出里提取产出任务，容错阶梯（实战三次 emit 失败的教训——
// 模型会漏写 json 围栏标签、把 JSON 混在叙述里、甚至只把清单写进文件不回显）：
//  1. 围栏块（json 标签或裸围栏，内容以 {/[ 开头），后出现的优先；
//  2. 未围栏平衡扫描：在 "tasks" 关键字前回溯 '{'，用 Decoder 解出首个合法对象；
//  3. 文件救援：输出中提到的 .json 文件（限任务目录内），读文件解析。
func extractEmitTasks(result, dir string) ([]emitTask, error) {
	// ① 围栏块，后出现的优先（模型习惯把操作性 JSON 放最后）。
	ms := anyFencedRe.FindAllStringSubmatch(result, -1)
	for i := len(ms) - 1; i >= 0; i-- {
		body := strings.TrimSpace(ms[i][1])
		if !strings.HasPrefix(body, "{") && !strings.HasPrefix(body, "[") {
			continue
		}
		if tasks := parseEmitTasks(body); tasks != nil {
			return tasks, nil
		}
	}
	// ② 未围栏：定位每个 "tasks" 出现点，向前回溯若干 '{' 逐一试解
	// （最近的 '{' 可能落在嵌套对象或字符串内，逐层外推直到解出）。
	for off := 0; ; {
		k := strings.Index(result[off:], `"tasks"`)
		if k < 0 {
			break
		}
		pos := off + k
		brace := pos
		for attempt := 0; attempt < 32; attempt++ {
			brace = strings.LastIndex(result[:brace], "{")
			if brace < 0 {
				break
			}
			var spec emitSpec
			dec := json.NewDecoder(strings.NewReader(result[brace:]))
			if err := dec.Decode(&spec); err == nil && len(spec.Tasks) > 0 {
				return spec.Tasks, nil
			}
		}
		off = pos + len(`"tasks"`)
	}
	// ③ 文件救援：模型把清单写进了任务目录下的 .json 文件（如 _WAVE-N-TASKS.json）。
	if dir != "" {
		base := filepath.Clean(dir)
		seen := map[string]bool{}
		for _, tok := range jsonFileRe.FindAllString(result, -1) {
			p := strings.ReplaceAll(tok, "\\", "/")
			if !filepath.IsAbs(p) {
				p = filepath.Join(base, p)
			}
			p = filepath.Clean(p)
			// 只信任务目录内的文件，拒绝越界路径。
			if p != base && !strings.HasPrefix(p, base+string(filepath.Separator)) {
				continue
			}
			if seen[p] {
				continue
			}
			seen[p] = true
			if fi, err := os.Stat(p); err != nil || fi.IsDir() || fi.Size() > 2<<20 {
				continue
			}
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			if tasks := parseEmitTasks(string(b)); tasks != nil {
				return tasks, nil
			}
		}
	}
	return nil, fmt.Errorf("输出中没有合法的 tasks JSON（围栏/平衡扫描/文件救援均未命中）")
}

// reviewVerdict 是对抗审核报告末尾的机读结论（design-review 模板约定的 json 块）。
type reviewVerdict struct {
	Verdict string   `json:"verdict"`
	P0      []string `json:"p0"`
	P1      []string `json:"p1"`
	P2      []string `json:"p2"`
	Summary string   `json:"summary"`
}

// parseReviewVerdict 从审核输出提取 verdict json。围栏块从后往前找（模板要求结论放最后），
// 兜底走未围栏平衡扫描（对 "verdict" 关键字回溯 '{' 试解）。解析不出返回 nil（旧格式兼容：不闭环只记日志）。
func parseReviewVerdict(result string) *reviewVerdict {
	try := func(raw string) *reviewVerdict {
		raw = strings.TrimSpace(raw)
		if raw == "" || !strings.HasPrefix(raw, "{") {
			return nil
		}
		var v reviewVerdict
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil
		}
		switch v.Verdict {
		case "pass", "concerns", "block":
			return &v
		}
		return nil
	}
	ms := anyFencedRe.FindAllStringSubmatch(result, -1)
	for i := len(ms) - 1; i >= 0; i-- {
		if v := try(ms[i][1]); v != nil {
			return v
		}
	}
	for off := 0; ; {
		k := strings.Index(result[off:], `"verdict"`)
		if k < 0 {
			break
		}
		pos := off + k
		brace := pos
		for attempt := 0; attempt < 32; attempt++ {
			brace = strings.LastIndex(result[:brace], "{")
			if brace < 0 {
				break
			}
			var v reviewVerdict
			dec := json.NewDecoder(strings.NewReader(result[brace:]))
			if err := dec.Decode(&v); err == nil {
				switch v.Verdict {
				case "pass", "concerns", "block":
					return &v
				}
			}
		}
		off = pos + len(`"verdict"`)
	}
	return nil
}

var fixTitleRe = regexp.MustCompile(`^修复R\d+: `)
var fixTitleTailRe = regexp.MustCompile(` \[(?:concerns|block):\d+P0\+\d+P1\]$`)

// baseFixTitle 剥掉自动修复卡标题的轮次前缀与判定尾注，得到谱系根标题（防止标题逐轮嵌套增长）。
func baseFixTitle(title string) string {
	title = fixTitleRe.ReplaceAllString(title, "")
	return fixTitleTailRe.ReplaceAllString(title, "")
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n…（截断）"
}

// handleReviewVerdict 让"实现→对抗审核→修复"循环自动闭合：
//   - pass → 记录收口（叶卡状态回写归实现卡自身纪律，编排器不写业务仓）；
//   - concerns/block 且轮次未超限 → 依 fix-cycle 模板自动派下一轮修复卡（继承被审卡参数，
//     强制按类闭合纪律，effort 缺省抬到 high——修复是最该多想的环节）；
//   - 超轮限 → 挂 held 升级卡：大概率是规格歧义/契约冲突，继续在实现层打转是浪费，交人裁。
func handleReviewVerdict(root string, cfg *Config, t *Task, result string, lg *os.File) {
	v := parseReviewVerdict(result)
	if v == nil {
		logBlock(lg, "FIXLOOP", "审核输出中未找到 verdict json（旧格式或审核未按模板收尾），闭环跳过")
		return
	}
	if v.Verdict == "pass" {
		logBlock(lg, "FIXLOOP", fmt.Sprintf("复审 PASS（第 %d 轮收口）: %s", t.FixRound, v.Summary))
		// 收口回写：pass 是权威的"done"事件。若被审卡带 Closeout 指令，入队一张廉价收口卡
		// 把 done 回写账本（绑定到 pass 而非实现卡自评，根治 frontmatter 双真相源漂移）。
		if t.ReviewOf != "" {
			if orig, err := findTaskAnywhere(root, t.ReviewOf); err == nil && strings.TrimSpace(orig.Closeout) != "" {
				co := newTask(root, cfg, typeSequence, "收口: "+baseFixTitle(orig.Title), orig.Dir, []string{orig.Closeout}, orig.Priority)
				co.Model = "haiku"
				co.SkipPermissions = orig.SkipPermissions
				co.RemoteHost = orig.RemoteHost
				if saveTask(root, co) == nil {
					logBlock(lg, "FIXLOOP", "已入队收口卡 "+co.ID+"（pass→回写账本 done）")
				}
			}
		}
		return
	}
	if t.ReviewOf == "" {
		logBlock(lg, "FIXLOOP", "verdict="+v.Verdict+" 但审核卡无 review_of 谱系（手工审核卡），闭环跳过")
		return
	}
	orig, err := findTaskAnywhere(root, t.ReviewOf)
	if err != nil {
		logBlock(lg, "FIXLOOP", "verdict="+v.Verdict+" 但被审卡不可得: "+err.Error())
		return
	}
	round := t.FixRound + 1
	maxRounds := cfg.MaxFixRounds
	if maxRounds <= 0 {
		maxRounds = 3
	}
	base := baseFixTitle(orig.Title)
	findings := ""
	for i, p := range v.P0 {
		findings += fmt.Sprintf("P0-%d: %s\n", i+1, p)
	}
	for i, p := range v.P1 {
		findings += fmt.Sprintf("P1-%d: %s\n", i+1, p)
	}
	if findings == "" {
		// concerns 却没给 P0/P1 条目——没有可执行清单就不盲派，交日志提醒人看报告。
		logBlock(lg, "FIXLOOP", "verdict="+v.Verdict+" 但 p0/p1 均空，无可执行清单，闭环跳过（查看审核报告后人工处理）")
		return
	}
	if round > maxRounds {
		prompt := fmt.Sprintf(`任务「%s」的实现→对抗审核→修复循环已达 %d 轮仍未收敛（最新判定 %s）。
这通常不是实现问题而是规格问题：请先判断叶卡/契约规格是否存在歧义或自相矛盾（对照最新审核报告的未闭合项），
需要修订规格的部分列出修订建议并登记待裁（不擅改契约），确属实现缺陷的部分给出精确修法后再改。
最新一轮未闭合项：
%s
审核摘要：%s`, base, maxRounds, v.Verdict, findings, v.Summary)
		// held 升级卡用被审卡（实现卡）的 Dir：远程审核分流时审核卡 Dir 是镜像路径，
		// 人裁应回到实现卡本处。orig 已在上文取到（不可得时才退回 t.Dir）。
		escDir := orig.Dir
		if escDir == "" {
			escDir = t.Dir
		}
		esc := newTask(root, cfg, typeSequence, fmt.Sprintf("[超轮限R%d·需人裁] %s", round, base), escDir, []string{prompt}, t.Priority)
		esc.Status = statusHeld
		// 远端链的升级卡必须继承执行主机：dir 是远端路径（如 D:/...），缺 remote_host
		// 会在 release 后被派到本机、cd 直接失败（实测远端 R4 卡两张踩中）。
		esc.RemoteHost = orig.RemoteHost
		esc.FixRound = round
		if saveTask(root, esc) == nil {
			logBlock(lg, "FIXLOOP", fmt.Sprintf("超轮限（R%d>上限%d），已挂 held 升级卡 %s 交人工裁定", round, maxRounds, esc.ID))
		}
		return
	}
	tpl, err := loadTemplate(root, "fix-cycle")
	if err != nil {
		logBlock(lg, "FIXLOOP", "fix-cycle 模板不可得: "+err.Error())
		return
	}
	prompt := renderTemplate(tpl, map[string]string{
		"TITLE":      base,
		"VERDICT":    v.Verdict,
		"ROUND":      fmt.Sprintf("%d", round),
		"SUMMARY":    v.Summary,
		"FINDINGS":   strings.TrimSpace(findings),
		"ORIG_PROMPT": truncateRunes(strings.Join(orig.Prompts, "\n---\n"), 4000),
		"REVIEW_LOG": filepath.Join(logsDir(root), t.ID+".log"),
	})
	title := fmt.Sprintf("修复R%d: %s [%s:%dP0+%dP1]", round, base, v.Verdict, len(v.P0), len(v.P1))
	nt := newTask(root, cfg, typeSequence, title, orig.Dir, []string{prompt}, orig.Priority)
	nt.ReviewAfter = true
	nt.FixRound = round
	nt.Model = orig.Model
	nt.SkipPermissions = orig.SkipPermissions
	nt.PermissionMode = orig.PermissionMode
	nt.AllowedTools = append([]string(nil), orig.AllowedTools...)
	nt.RemoteHost = orig.RemoteHost
	// 审核分流三字段随修复链继承：下一轮修复完成后的审核继续分流到审核主机。
	nt.ReviewHost = orig.ReviewHost
	nt.ReviewDir = orig.ReviewDir
	nt.ReviewSync = orig.ReviewSync
	nt.Closeout = orig.Closeout // 收口指令随修复链继承，pass 时才由末轮触发
	// 修复是最该多想的环节：未显式指定时抬到 high（质量优先）。不继承 PreferRunner——
	// 修复吃上下文推理，钉 codex 的填充类偏好不适用。
	nt.Effort = orig.Effort
	if nt.Effort == "" {
		nt.Effort = "high"
	}
	if err := saveTask(root, nt); err != nil {
		logBlock(lg, "FIXLOOP", "修复卡保存失败: "+err.Error())
		return
	}
	logBlock(lg, "FIXLOOP", fmt.Sprintf("verdict=%s → 已自动入队第 %d 轮修复卡 %s（%dP0+%dP1，effort=%s）", v.Verdict, round, nt.ID, len(v.P0), len(v.P1), nt.Effort))
}

var crossTitleRe = regexp.MustCompile(`^交叉[^\[]*\[[^\]]*\]:\s*`)

// crossBase 剥掉交叉卡标题的「交叉X[profile]: 」前缀，得到谱系根标题。
func crossBase(title string) string { return crossTitleRe.ReplaceAllString(title, "") }

// crossPeerPath 是甲结论的隔离侧车路径：<root>/crosscheck/<XKey>.a。甲结论不进任何交叉卡的字段/日志，
// 仅编排进程读写、C 用完即删、0600 最小权限。**诚实边界**：B 卡里带着 XKey，而侧车路径由 XKey 确定性
// 推导——严格说 B 握有指向侧车的键。所以这**不是硬沙箱**（codex read-only 读全盘，刻意搜索仍可能触达）；
// 它做到的是被动暴露最小化 + 行为护栏（solo 明令别找），默认下 B 拿不到甲是因为没被给、被明令别找。
func crossPeerPath(root, xkey string) string { return filepath.Join(crosscheckDir(root), xkey+".a") }

func writeCrossPeer(root, xkey, data string) error {
	if err := os.MkdirAll(crosscheckDir(root), 0o700); err != nil {
		return err
	}
	// 0600 原子写：敏感中间产物给最小 OS 权限（同用户不构成硬隔离，但不给其余用户/进程可读）。
	p := crossPeerPath(root, xkey)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(data), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// applyCrossEngine 把交叉验证引擎的执行位置写进卡——这是"模型来源可切换"的落点：
// 换 profile.A/B 的 Kind/Model/Host 即换引擎，卡的结构与链路不变。先清引擎相关字段防串味。
var validXEfforts = map[string]bool{"minimal": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true, "ultra": true}

// crossEngineIdentity 给引擎算一个规范身份串（执行器+具体模型）；甲乙身份相同=同引擎=单引擎自审，须拒。
// 这是**文本级 best-effort**：拦得住"两 codex / 同 model claude"这类直白的同引擎；但拦不了模型别名
// （如 "opus" 与 "claude-opus-4-8" 实为同一模型）——无模型规范化表无法穷尽，profile 是用户自写、别名同引擎
// 属用户配置责任。
func crossEngineIdentity(eng CrossEngine, cfg *Config) string {
	switch eng.Kind {
	case "claude":
		return "claude|" + eng.Model
	case "codex":
		return "codex|" + cfg.CodexModel
	case "remote-claude":
		return "remote-claude|" + eng.Host + "|" + eng.Model
	case "remote-codex":
		return "remote-codex|" + eng.Host + "|" + cfg.CodexModel
	}
	return eng.Kind
}

// freezeCrossEngine 把引擎解析成冻结执行规格（同时做 applyCrossEngine 的全部校验）。
func freezeCrossEngine(eng CrossEngine, cfg *Config) (*XFrozenEngine, error) {
	tmp := &Task{Prompts: []string{"x"}}
	if err := applyCrossEngine(tmp, eng, cfg); err != nil {
		return nil, err
	}
	f := &XFrozenEngine{Model: tmp.Model, Effort: tmp.Effort, PreferRunner: tmp.PreferRunner, RemoteHost: tmp.RemoteHost, Label: crossEngineLabel(eng)}
	// 冻结空 effort 的 reasoning 回落——但必须冻结**执行时真正会用的那个源**：本机 codex 回落全局
	// codex_reasoning，远端 codex 回落该主机的 reasoning。冻错源会在正常路径就跑错档位。
	switch eng.Kind {
	case "codex":
		f.CodexModel = cfg.CodexModel
		if f.Effort == "" {
			f.Effort = cfg.CodexReasoning
		}
	case "remote-codex":
		f.CodexModel = cfg.CodexModel
		if f.Effort == "" {
			if rh, ok := cfg.RemoteHosts[eng.Host]; ok {
				f.Effort = rh.Reasoning // invokeRemoteCodex 的既有回落源是 host 的 reasoning，非全局
			}
		}
	}
	return f, nil
}

// applyFrozenEngine 把冻结规格套到卡（B/C 用它，不再从 config 重解析——身份冻结）。
func applyFrozenEngine(t *Task, f *XFrozenEngine) {
	t.Model, t.Effort, t.PreferRunner, t.RemoteHost = f.Model, f.Effort, f.PreferRunner, f.RemoteHost
	t.XCodexModel = f.CodexModel
}

func applyCrossEngine(t *Task, eng CrossEngine, cfg *Config) error {
	if eng.Effort != "" && !validXEfforts[eng.Effort] {
		return fmt.Errorf("交叉引擎 effort %q 非法（minimal/low/medium/high/xhigh/max/ultra）", eng.Effort)
	}
	t.Model, t.Effort, t.PreferRunner, t.RemoteHost = "", "", "", ""
	switch eng.Kind {
	case "claude":
		// 本机 claude：默认执行器，走本机 claude 账号额度（如 opus-4.8 max）。
		// Model 必填：空则 invokeClaude 省略 --model、跑账号默认模型（典型 Max 部署=Sonnet），
		// 而日志/Label 仍宣称 opus——验证质量被静默降级。镜像 remote-claude 的守卫。
		if eng.Model == "" {
			return fmt.Errorf("claude 交叉引擎必须显式指定 model，否则跑成账号默认模型、验证质量静默降级")
		}
		t.Model = eng.Model
		t.Effort = eng.Effort
	case "codex":
		// 本机 codex：钉 runner=codex，用独立 GPT 额度；模型来自全局 codex_model，思考档取 eng.Effort
		// （空则回落全局 codex_reasoning）——invokeCodex 已优先 t.Effort，故交叉验证的 codex 卡可跑在 max。
		if cfg.CodexBin == "" {
			return fmt.Errorf("codex 引擎需 config.codex_bin")
		}
		// 必须显式钉模型：否则跑成 codex 内置默认模型，与 profile 宣称不符，验证质量被静默降级。
		if cfg.CodexModel == "" {
			return fmt.Errorf("codex 交叉引擎需 config.codex_model 显式指定，否则会跑成 codex 默认模型")
		}
		// codex 的模型由全局 codex_model 决定；profile 里再写 model 是零效 no-op（会误导使用者以为设了）。
		if eng.Model != "" {
			return fmt.Errorf("codex 交叉引擎的 model 由 config.codex_model 决定，请从 profile 删掉 model 字段（此处写它会被忽略）")
		}
		t.PreferRunner = "codex"
		t.Effort = eng.Effort
	case "remote-claude":
		// SSH 远端 claude：Model 必填，否则 remoteUsesClaude 判 false 会被路由到远端 codex。
		if _, ok := cfg.RemoteHosts[eng.Host]; !ok {
			return fmt.Errorf("remote_hosts 未配置主机 %q", eng.Host)
		}
		if eng.Model == "" {
			return fmt.Errorf("remote-claude 引擎必须指定 model（否则会被路由到远端 codex）")
		}
		t.RemoteHost = eng.Host
		t.Model = eng.Model
		t.Effort = eng.Effort
	case "remote-codex":
		// SSH 远端 codex：无模型 → remoteUsesClaude 判 false → 走远端 codex，用该远端 GPT 额度。
		// 思考档同样取 eng.Effort（空则回落该主机的 reasoning 配置）。远端 codex 亦用全局 codex_model。
		if _, ok := cfg.RemoteHosts[eng.Host]; !ok {
			return fmt.Errorf("remote_hosts 未配置主机 %q", eng.Host)
		}
		if cfg.CodexModel == "" {
			return fmt.Errorf("remote-codex 交叉引擎需 config.codex_model 显式指定，否则会跑成 codex 默认模型")
		}
		if eng.Model != "" {
			return fmt.Errorf("remote-codex 交叉引擎的 model 由 config.codex_model 决定，请从 profile 删掉 model 字段")
		}
		t.RemoteHost = eng.Host
		t.Effort = eng.Effort
	default:
		return fmt.Errorf("未知交叉引擎 kind %q（可选 claude/codex/remote-claude/remote-codex）", eng.Kind)
	}
	// codex/远端引擎要求 codexEligible（单步无会话）——交叉卡都是单步，正常满足；防御性兜底。
	if (t.PreferRunner == "codex" || t.RemoteHost != "") && !codexEligible(t) {
		return fmt.Errorf("codex/远端引擎要求单步无会话")
	}
	return nil
}

// handleCrossStage 推进交叉验证链：
//   A 完成 → 甲结论落隔离侧车（不进任何卡字段）→ 派引擎乙独立作答的 B 卡（prompt 与 A 相同、不含 A、无 A 指针）；
//   B 完成 → 从侧车取甲结论 → 派引擎乙交叉查漏的 C 卡（合并模板注入完整甲+乙结论）→ 删侧车。
// 链任一步断裂把母卡置 failed（list 对 failed 显示 LastError 且不折叠，故断裂可见），绝不让单腿结果冒充成功。
func handleCrossStage(root string, cfg *Config, t *Task, res *claudeResult, lg *os.File) {
	// breakChain 把母卡从 done 改判为 failed + 写断裂原因。runTask 在 postComplete 后 saveTask 落盘；
	// 置 failed 而非仅写 LastError 是因为 list/cmdList 只对 failed 卡渲染 LastError，done 卡从不显示它
	// （否则断裂母卡与成功卡视觉全等——round-1 的"已可见"是空头承诺）。
	breakChain := func(reason string) {
		t.Status = statusFailed
		t.LastError = "交叉链断裂: " + reason
		_ = os.Remove(crossPeerPath(root, t.XKey)) // 断裂时清理侧车,不留甲结论长期残驻
		logBlock(lg, "CROSS", "链中断（母卡置 failed 留痕）: "+reason)
	}
	// 乙引擎用**冻结**规格（入队时钉死），不从当前 config 重解析——防身份漂移。
	if t.XEngineB == nil {
		breakChain("缺冻结的乙引擎规格 XEngineB（旧卡/数据损坏）")
		return
	}
	base := crossBase(t.Title)
	label := t.XEngineB.Label
	if label == "" {
		label = "引擎乙"
	}
	switch t.XRole {
	case "A":
		// 甲结论落隔离侧车——不进 B 的任何字段/prompt/日志（被动暴露最小化，非硬沙箱）。
		if err := writeCrossPeer(root, t.XKey, resultText(res)); err != nil {
			breakChain("甲结论侧车落盘失败: " + err.Error())
			return
		}
		// B 用与 A 完全相同的独立作答 prompt（公平 + 独立），套**冻结**的乙引擎。
		b := newTask(root, cfg, typeCrossCheck, "交叉B["+t.XProfile+"]: "+base, t.Dir, []string{t.Prompts[0]}, t.Priority)
		b.XRole = "B"
		b.XKey = t.XKey
		b.XProfile = t.XProfile
		b.XTask = t.XTask
		b.XEngineB = t.XEngineB       // 冻结规格随链传递
		applyFrozenEngine(b, t.XEngineB)
		if err := saveTask(root, b); err != nil {
			breakChain("B 卡落盘失败: " + err.Error())
			return
		}
		logBlock(lg, "CROSS", fmt.Sprintf("引擎甲完成 → 已派独立引擎乙 B 卡 %s（%s，不含甲结论、无 A 指针）", b.ID, label))
	case "B":
		tpl, err := loadTemplate(root, "crosscheck-merge")
		if err != nil {
			breakChain("crosscheck-merge 模板不可得: " + err.Error())
			return
		}
		peer, err := os.ReadFile(crossPeerPath(root, t.XKey))
		if err != nil {
			breakChain("甲结论侧车不可得: " + err.Error())
			return
		}
		// 全量注入甲+乙结论（stdin 传 prompt，无 argv 上限，不截断）。
		prompt := renderTemplate(tpl, map[string]string{
			"TASK": t.XTask,
			"A":    strings.TrimSpace(string(peer)),
			"B":    strings.TrimSpace(resultText(res)),
		})
		c := newTask(root, cfg, typeCrossCheck, "交叉C汇总["+t.XProfile+"]: "+base, t.Dir, []string{prompt}, t.Priority)
		c.XRole = "C"
		c.XKey = t.XKey
		c.XProfile = t.XProfile
		c.XEngineB = t.XEngineB
		c.EmitProgress = true // C 的 json 最终结论落进度报告，键=XKey，供 progress -show 取回
		c.ProgressKey = t.XKey
		applyFrozenEngine(c, t.XEngineB)
		if err := saveTask(root, c); err != nil {
			breakChain("C 卡落盘失败: " + err.Error())
			return
		}
		_ = os.Remove(crossPeerPath(root, t.XKey)) // C 已烘入甲+乙全文，侧车使命完成，清理
		logBlock(lg, "CROSS", fmt.Sprintf("引擎乙完成 → 已派交叉查漏 C 卡 %s（%s，注入甲+乙完整结论）", c.ID, label))
	}
}

// reconcileCrossChains 崩溃对账：done 的交叉 A/B 卡若无后继卡（同 XKey 的 B/C），是进程在"标 done"与
// "派后继卡"之间崩溃遗留的单腿孤儿——正常窗口内后继已在同一 runTask 落盘，故一个"已结算(不在 active)+done+
// 无后继"的卡必是崩溃孤儿。置 failed 留痕并清侧车，不让单腿结果静默冒充成功。active 守卫排除仍在跑
// （runTask 可能正处于 postComplete 派后继的微秒窗口）的卡，避免误判。
func reconcileCrossChains(root string, tasks []*Task, active map[string]bool) {
	has := map[string]map[string]bool{} // xkey -> {role: present}
	for _, t := range tasks {
		if t.XKey != "" && t.XRole != "" {
			if has[t.XKey] == nil {
				has[t.XKey] = map[string]bool{}
			}
			has[t.XKey][t.XRole] = true
		}
	}
	var archived map[string]map[string]bool // 惰性加载：仅在有候选孤儿时才扫 archive/
	for _, t := range tasks {
		if active[t.ID] || t.Status != statusDone || (t.XRole != "A" && t.XRole != "B") {
			continue
		}
		next := "C"
		if t.XRole == "A" {
			next = "B"
		}
		if has[t.XKey][next] {
			continue // 后继在 tasks/，链正常
		}
		// 后继不在 tasks/：可能已完成并归档（clean/cancel），查 archive/ 再定夺，避免误判正常完成的链。
		if archived == nil {
			archived = crossRolesInArchive(root)
		}
		if archived[t.XKey][next] {
			continue
		}
		t.Status = statusFailed
		t.LastError = fmt.Sprintf("交叉链在 %s 完成后崩溃中断（无后继 %s 卡），单腿结果不可采信", t.XRole, next)
		_ = os.Remove(crossPeerPath(root, t.XKey))
		_ = saveTask(root, t)
	}
}

// crossRolesInArchive 扫 archive/ 里交叉卡的 XKey→role（供 reconcile 判"后继是否已完成归档"）。
func crossRolesInArchive(root string) map[string]map[string]bool {
	m := map[string]map[string]bool{}
	entries, err := os.ReadDir(archiveDir(root))
	if err != nil {
		return m
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(archiveDir(root), e.Name()))
		if err != nil {
			continue
		}
		var t Task
		if json.Unmarshal(data, &t) != nil || t.XKey == "" || t.XRole == "" {
			continue
		}
		if m[t.XKey] == nil {
			m[t.XKey] = map[string]bool{}
		}
		m[t.XKey][t.XRole] = true
	}
	return m
}

// crossEngineLoc 返回引擎的执行位置："local"（本机 claude/codex）或 "remote:<host>"。
// 交叉链的 A/B/C 共用一个工作目录（handleCrossStage 复制 A.Dir），故一对引擎必须同位置——
// 否则 B/C 拿着 A 的目录被派到错误的机器（本机路径派去远端 cd 失败，或远端路径被当本地目录）。
func crossEngineLoc(eng CrossEngine) string {
	if eng.Kind == "remote-claude" || eng.Kind == "remote-codex" {
		return "remote:" + eng.Host
	}
	return "local"
}

// crossEngineLabel 给引擎生成展示名：优先 Label，否则据 kind/model/host 拼一个。
func crossEngineLabel(eng CrossEngine) string {
	if eng.Label != "" {
		return eng.Label
	}
	switch eng.Kind {
	case "claude":
		return orDash(eng.Model) + "·" + orDash(eng.Effort)
	case "codex":
		return "codex"
	case "remote-claude", "remote-codex":
		return eng.Kind + "@" + eng.Host
	}
	return eng.Kind
}

func enqueueEmitted(root string, cfg *Config, parent *Task, result string) ([]string, error) {
	tasks, err := extractEmitTasks(result, parent.Dir)
	if err != nil {
		return nil, err
	}
	if len(tasks) > 10 {
		tasks = tasks[:10]
	}
	var ids []string
	for _, s := range tasks {
		if len(s.Prompts) == 0 && len(s.Steps) > 0 {
			s.Prompts = s.Steps
		}
		if len(s.Prompts) == 0 && strings.TrimSpace(s.Prompt) != "" {
			s.Prompts = []string{s.Prompt}
		}
		if len(s.Prompts) == 0 {
			continue
		}
		typ := s.Type
		// 模型有时自造类型名（如 "batch"）；未知类型会导致 newTask 不烘焙 typeDefaults
		// → 空权限裸跑被拒。未知或空一律回退 sequence，保证权限正确烘焙。
		if !validTypes[typ] {
			typ = typeSequence
		}
		dir := s.Dir
		if dir == "" {
			dir = parent.Dir
		}
		title := s.Title
		if title == "" {
			title = s.Role
		}
		if title == "" {
			title = s.ID
		}
		if title == "" {
			title = "由 " + parent.ID + " 生成"
		}
		nt := newTask(root, cfg, typ, title, dir, s.Prompts, s.Priority)
		nt.ReviewAfter = s.ReviewAfter
		// 协调链：产出的 coordinate 任务同样具备 emit 能力（自愈式续排——每批收尾排下一批）。
		// 递归有界：每次 emit ≤10 张、每张协调都消耗额度且受红线节流，模板负责终止条款。
		if typ == typeCoordinate {
			nt.EmitTasks = true
		}
		nt.FreshSteps = s.FreshSteps
		// 协调可把填充类任务钉在 codex 上（独立 GPT 额度）；形状不合规则忽略指定。
		if s.Runner == "codex" {
			nt.PreferRunner = "codex"
			if !codexEligible(nt) {
				nt.PreferRunner = ""
			}
		}
		if s.Model != "" {
			nt.Model = s.Model
		}
		if validEfforts[s.Effort] {
			nt.Effort = s.Effort
		}
		nt.SessionID = s.SessionID
		if parent.EmitHold {
			nt.Status = statusHeld
		}
		if err := saveTask(root, nt); err != nil {
			return ids, err
		}
		ids = append(ids, nt.ID)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("所有产出任务都缺少 prompts")
	}
	return ids, nil
}

func lastFencedJSON(text string) string {
	ms := fencedRe.FindAllStringSubmatch(text, -1)
	if len(ms) == 0 {
		return ""
	}
	return strings.TrimSpace(ms[len(ms)-1][1])
}

func resultText(res *claudeResult) string {
	if res == nil {
		return ""
	}
	return res.Result
}

func errorSummary(res *claudeResult, combined string, runErr error) string {
	if res != nil && res.IsError {
		s := res.Subtype
		if r := firstLine(res.Result); r != "" {
			s += ": " + r
		}
		return s
	}
	if runErr != nil {
		return runErr.Error() + " | " + firstLine(combined)
	}
	return "无法解析 claude 输出 | " + firstLine(combined)
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			if len(line) > 300 {
				return line[:300]
			}
			return line
		}
	}
	return ""
}

// codexErrorLine 从 codex 的 stdout+stderr 里提取**真正的失败原因**，跳过横幅/配置/进度噪声。
// codex exec 的第一行永远是横幅 "Reading additional input from stdin..."、随后是 OpenAI Codex 版本/
// workdir/model/... 配置块——firstLine 恰好取到横幅，掩盖真因（网络/限额/流断在末尾），还让 transientRe
// 匹配不到、把可退避的瞬时错误当硬失败烧 attempts（单腿审核一挂就没了）。优先返回命中错误/瞬时样式的
// 行，否则返回最后一条非噪声行，都没有才回退 firstLine。
// codexErrorLine 只挑「瞬时网络/上游」样式(transientRe)的真错误行，供 runErr 路径退避快重试。
// 早期还兼挑泛化 codexErrRe(error|failed|cannot|invalid...)行，但 codex exec 把整段推理/审查正文
// 都写进 stderr、正文天然充斥这些词——按 codexErrRe 挑行会把审查正文里一句无害内容当"错误"上报，
// 掩盖真因（实测 superpowers/gsd 注入耗尽回合预算的空终稿故障，被误报成 "...You cannot rationalize..."）。
// 故只认 transientRe；挑不到就返回空，由调用方回退到真实 runErr / 明确诊断。
func codexErrorLine(combined string) string {
	for _, l := range strings.Split(combined, "\n") {
		t := strings.TrimSpace(l)
		if t == "" || codexNoiseRe.MatchString(t) {
			continue
		}
		if transientRe.MatchString(t) || codexHardErrRe.MatchString(t) {
			if len(t) > 300 {
				return t[:300]
			}
			return t
		}
	}
	return ""
}

// summarizeResult 取一步最终输出里第一条实质内容行，作为 list 看板“最新进度概述”的回落来源。
// 跳过空行/代码围栏，剥掉常见 markdown 前缀，rune 安全截到 80。
func summarizeResult(result string) string {
	for _, ln := range strings.Split(result, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "```") {
			continue
		}
		ln = strings.TrimSpace(strings.TrimLeft(ln, "#*->・•· \t"))
		if len([]rune(ln)) >= 4 {
			if r := []rune(ln); len(r) > 80 {
				return string(r[:80]) + "…"
			}
			return ln
		}
	}
	return ""
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func openTaskLog(root, id string) (*os.File, error) {
	if err := os.MkdirAll(logsDir(root), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(taskLogPath(root, id), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
}

func logSection(f *os.File, s string) {
	fmt.Fprintf(f, "\n===== %s  %s =====\n", time.Now().Format("2006-01-02 15:04:05"), s)
}

func logBlock(f *os.File, label, body string) {
	fmt.Fprintf(f, "--- %s ---\n%s\n", label, strings.TrimSpace(body))
}
