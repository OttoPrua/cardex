package main

// 限额识别与重置时间解析回归测试。
// 场景来自实战:远端账号限额提示是 "hit your session limit"(比本地多个 session 词),
// 旧 limitRe 不匹配 → 走普通失败路径烧 attempts,三次后任务假失败。

import (
	"strconv"
	"testing"
	"time"
)

func TestIsLimitHitPhrasings(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"远端 session limit 措辞", "You've hit your session limit · resets 8:20pm (Asia/Singapore)", true},
		{"headless 带 epoch", "Claude AI usage limit reached|1751600000", true},
		{"resets at 钟点", "You've reached your usage limit ... resets at 3pm", true},
		{"5 小时窗口", "5-hour limit reached", true},
		{"hit your limit 原形", "You've hit your limit", true},
		{"weekly 变体", "You've hit your weekly limit · resets Tuesday", true},
		{"普通错误不误判", "connection refused while fetching model list", false},
	}
	for _, c := range cases {
		res := &claudeResult{Type: "result", Subtype: "success", IsError: true, Result: c.text}
		if got := isLimitHit(res, ""); got != c.want {
			t.Errorf("%s: isLimitHit=%v, want %v (text=%q)", c.name, got, c.want, c.text)
		}
	}
}

func TestIsLimitHitGuardsOnSuccess(t *testing.T) {
	// 成功结果里出现限额字样（如任务产出讨论限额机制）不得误判为限额命中。
	res := &claudeResult{Type: "result", Subtype: "success", IsError: false,
		Result: "本工具围绕 usage limit 做队列调度……"}
	if isLimitHit(res, "") {
		t.Fatal("IsError=false 时不应判为限额命中")
	}
}

func TestParseResetEpochClockPhrase(t *testing.T) {
	cfg := &Config{LimitFallbackMin: 30, CooldownMarginSec: 90}
	loc := time.FixedZone("UTC+8", 8*3600)
	now := time.Date(2026, 7, 10, 18, 47, 0, 0, loc)

	got := parseResetEpoch("You've hit your session limit · resets 8:20pm (Asia/Singapore)", cfg, now)
	want := time.Date(2026, 7, 10, 20, 20, 0, 0, loc).Unix() + 90
	if got != want {
		t.Errorf("钟点措辞解析: got %d (%s), want %d (%s)",
			got, time.Unix(got, 0).In(loc), want, time.Unix(want, 0).In(loc))
	}

	// 已过钟点滚到次日
	lateNow := time.Date(2026, 7, 10, 21, 0, 0, 0, loc)
	got = parseResetEpoch("resets 8:20pm", cfg, lateNow)
	want = time.Date(2026, 7, 11, 20, 20, 0, 0, loc).Unix() + 90
	if got != want {
		t.Errorf("次日滚动: got %s, want %s", time.Unix(got, 0).In(loc), time.Unix(want, 0).In(loc))
	}

	// epoch 形态优先
	epoch := now.Add(3 * time.Hour).Unix()
	got = parseResetEpoch("Claude AI usage limit reached|"+strconv.FormatInt(epoch, 10), cfg, now)
	if got != epoch+90 {
		t.Errorf("epoch 形态: got %d, want %d", got, epoch+90)
	}

	// 全都解析不到 → 配置回退等待
	got = parseResetEpoch("some opaque failure", cfg, now)
	want = now.Add(30*time.Minute).Unix() + 90
	if got != want {
		t.Errorf("回退等待: got %d, want %d", got, want)
	}
}

// TestIsLimitHitIgnoresResultFromTranscript (CG-R1 修复反例 · 治 (a))
// codex/远端 claude 的失败路径会把 codexErrorLine 挑走的行或 firstLine(combined) 塞进 res.Result
// 并打 ResultFromTranscript=true——那是 transcript prose(审查引用/工具输出/正常叙述)。旧法把
// res.Result 拼进扫描串, prose 含 "usage limit" 字面量就命中 limitRe → 卡被误挂 limit_paused
// 26h 静默(远端全 stopping);本机 claude 径还写全局冷却停摆所有 claude 泳道。
// 【反例】把 isLimitHit 里 `!res.ResultFromTranscript` 条件去掉(直接拼), 本测试报红。
func TestIsLimitHitIgnoresResultFromTranscript(t *testing.T) {
	res := &claudeResult{
		Type: "result", IsError: true,
		Result:               "审查引用:文档提到 You've reached your usage limit 措辞",
		ResultFromTranscript: true,
	}
	if isLimitHit(res, "") {
		t.Fatal("ResultFromTranscript=true 时 res.Result 不参与扫描,transcript prose 含 usage limit 不应误命中")
	}
	// 反证:同样的 Result 若非 transcript 来源(结构化 result 字段真报限额), 必须命中——防"过矫枉".
	res2 := &claudeResult{
		Type: "result", IsError: true,
		Result: "You've reached your usage limit ... resets at 3pm",
	}
	if !isLimitHit(res2, "") {
		t.Fatal("结构化 res.Result 里的真限额措辞必须命中(否则治 (a) 治过头,漏识别真限额)")
	}
}

// TestIsLimitHitClaudeSkipsStdoutJSONTranscript (CG-R1 修复反例 · 治 (c) 核心反例)
// 场景直落:本地 claude 超时被 kill 前 stdout 已吐半截 tool_use JSON(含 audit 正文引用
// "usage limit reached"字面量), parseClaudeJSON 解不出 res(=nil), stderr 空(CLI 无限额提示)。
// 旧 isLimitHit(nil, combined) 扫全量 combined 命中 → 挂 limit_paused(远端 claude 全泳道停摆
// 26h)+写全局 claude 冷却。isLimitHitClaude 剥 stdout 骨架后 scan 只含 stderr 段(空), 不命中。
// 【反例】把 runTask 里 `isLimitHitClaude(res, combined)` 回退成 `isLimitHit(res, combined)`,
// 本测试报红——限额被误命中。
func TestIsLimitHitClaudeSkipsStdoutJSONTranscript(t *testing.T) {
	// 模拟 claude --output-format json 半截 tool_use 消息:含 usage limit prose 但未闭合 result 对象.
	// stderr 段为空. 组装形态 = stdout + "\n" + stderr.
	stdout := `{"type":"assistant","message":{"content":[{"type":"text","text":"审查本仓 CG-3 提到 You've reached your usage limit 措辞"}]}`
	stderr := ""
	combined := stdout + "\n" + stderr
	if isLimitHitClaude(nil, combined) {
		t.Fatal("stdout transcript 里含 usage limit prose(res=nil)不应误命中——isLimitHitClaude 应剥 stdout 只扫 stderr 尾段")
	}
	// 对照组:同样 combined 用旧 isLimitHit 全量扫必命中——证明反例场景为真.
	if !isLimitHit(nil, combined) {
		t.Fatal("旧 isLimitHit(res=nil) 全量扫应命中 combined 里 usage limit prose——若不命中说明反例场景构造失败")
	}
	// 反证:stderr 里真限额措辞时 isLimitHitClaude 必须命中(否则治 (c) 治过头,漏识别真限额)。
	stderrHit := "Error: You've hit your usage limit\n"
	if !isLimitHitClaude(nil, "{}\n"+stderrHit) {
		t.Fatal("stderr 尾段里的真限额措辞必须命中")
	}
}

// TestStderrTailFromClaudeCombinedBraceBoundary (CG-R1 R3 P2-1 修复反例)
// stderrTailFromClaudeCombined 旧法用 strings.LastIndex(combined, "}") 剥 stdout, 两向皆错:
//
//	(a) combined 恰以 `}` 结尾 (stderr 尾是 JSON 错误对象) → 旧条件 `i+1 < len` 不成立回退返
//	    回全量 combined → stdout prose 含 usage limit 字面量重被扫 → 误挂 limit_paused 26h;
//	(b) stderr 内含 `}` 而限额行在其前 → LastIndex 指到 stderr 内的 `}`, 切掉限额行 → 漏识别。
//
// 新法: 从起始位置做带字符串字面量识别的括号深度扫描, stdout JSON 首个深度归零处即边界,
// 之后全归 stderr。两向缺陷同时闭合。
//
// 【反例】把 stderrTailFromClaudeCombined 回退成 `strings.LastIndex(combined, "}")` 版本,
// 本测试的 case A/B/D/E 会分别报红。
func TestStderrTailFromClaudeCombinedBraceBoundary(t *testing.T) {
	cases := []struct {
		name     string
		combined string
		want     string
	}{
		{
			// case A (P2-1 方向 a): combined 恰以 `}` 结尾 (stderr 尾为 JSON 错误对象)。
			// 旧法回退全量 combined → 重开 stdout prose 误命中之门。新法返回空串。
			name:     "combined 以 } 结尾时返回空串, 不回退全量 combined",
			combined: `{"type":"result","result":"foo"}` + "\n" + `{"error":"parse"}`,
			want:     "",
		},
		{
			// case B (P2-1 方向 b): stderr 里含 `}`, 前有真限额行。旧法 LastIndex 指到 stderr
			// 内的 `}`, 切掉限额行漏识别。新法按 stdout JSON 深度找边界, stderr 完整保留。
			name:     "stderr 含 } 时保留其前的限额行, 不切窗",
			combined: `{"type":"result","result":"foo"}` + "\nError: You've hit your usage limit\n{\"detail\":\"x\"}",
			want:     "\nError: You've hit your usage limit\n{\"detail\":\"x\"}",
		},
		{
			// case C (基线): 标准 combined = stdout JSON + \n + stderr 无 `}`。剥 stdout 后
			// 只剩 stderr 段。
			name:     "标准 stdout JSON + stderr 明文",
			combined: `{"type":"result","result":"foo"}` + "\nsome stderr text\n",
			want:     "\nsome stderr text\n",
		},
		{
			// case D (方向 a 场景 · JSON 字符串字面量含 `}`): stdout result 字段里含 `}`
			// 字面量 (审查文本引用), 应被字符串字面量识别跳过, 不当边界。
			name:     "stdout JSON 字符串字面量内 } 不当边界",
			combined: `{"type":"result","result":"discussion of }} braces"}` + "\nstderr tail",
			want:     "\nstderr tail",
		},
		{
			// case E (方向 a 场景 · 嵌套 JSON): stdout 的 usage 字段是嵌套对象, 内含 `}`, 应
			// 由深度扫描跨越, 不误当外层 JSON 边界。
			name:     "stdout JSON 嵌套对象内 } 不当边界",
			combined: `{"type":"result","result":"foo","usage":{"input_tokens":10}}` + "\nreal stderr",
			want:     "\nreal stderr",
		},
		{
			// case F: 半截 stdout JSON (kill 前 stdout 未成型, 深度不归零), 保守回退整体扫描。
			// 与旧法此路径行为等价——事件重复胜过真限额永久漏识别。
			name:     "半截 JSON 深度不归零回退整体扫描",
			combined: `{"type":"assistant","message":{"content":`,
			want:     `{"type":"assistant","message":{"content":`,
		},
		{
			// case G: 字符串字面量内含转义引号, 跨越后仍应识别到真边界。
			name:     "字符串字面量转义引号后深度扫描继续",
			combined: `{"type":"result","result":"quoted \"}\" text"}` + "\nstderr",
			want:     "\nstderr",
		},
	}
	for _, c := range cases {
		if got := stderrTailFromClaudeCombined(c.combined); got != c.want {
			t.Errorf("%s: 剥 stdout 后 stderr 尾段错\ngot:  %q\nwant: %q", c.name, got, c.want)
		}
	}

	// 端到端反证 (case A 语义闭合): stdout 里含 usage limit prose + combined 以 stderr 的 `}`
	// 结尾, isLimitHitClaude 必须**不**命中——否则旧 LastIndex 回退全量 combined 的 26h 挂起
	// 之门重开。
	stdoutProse := `{"type":"assistant","message":{"content":[{"type":"text","text":"审查引用: You've reached your usage limit"}]}`
	stderrJSON := `{"detail":"error json"}`
	if isLimitHitClaude(nil, stdoutProse+"\n"+stderrJSON) {
		t.Fatal("端到端 (case A): combined 以 stderr } 结尾, stdout prose 含 usage limit 不应误命中")
	}

	// 端到端反证 (case B 语义闭合): stderr 中限额行在前, JSON `}` 在后, isLimitHitClaude 必须
	// 命中真限额——否则真限额被切窗漏识别。
	stdoutFine := `{"type":"result","result":"ok"}`
	stderrLimitBeforeBrace := "\nError: You've hit your usage limit · resets 8:20pm\n{\"detail\":\"x\"}"
	if !isLimitHitClaude(nil, stdoutFine+stderrLimitBeforeBrace) {
		t.Fatal("端到端 (case B): stderr 中 limit 行在 } 前, 必须命中——否则切窗漏识别")
	}
}

// TestIsLimitHitCodexScansAllCandidateLines (CG-R1 R3 P1-1 修复反例)
// codex 会话中途撞真限额: transcript 前部工具输出行常含 transientRe 字面量 (timed out /
// connection reset / rate limit——本仓 runner.go transientRe 源码即含, 自审必现), 尾部才
// 吐真限额行 "You've hit your usage limit". 旧 isLimitHitCodex 走 codexErrorLine 首匹配
// 即返回, 挑走前部 transient 行, limitRe 不命中真限额行 → 真限额被误判 transient →
// retry_backoff 烧尽 attempts 落 held 等人工, 破坏无人值守 auto-resume (P1-1 场景直落)。
// 新法: isLimitHitCodex 用 codexLimitScanText 扫**全部**候选错误行 (非 codexNoiseRe 且
// transientRe|codexHardErrRe 命中), 任一命中 limitRe 即判限额——顺序无关。
//
// 【反例】把 isLimitHitCodex 回退成 `isLimitHit(res, codexErrorLine(combined))` (旧法首行
// 挑一), 场景 1 立即报红:真限额被 transient 前置行遮蔽 → 返回 false → 断言失败。
//
// 【关于替代掉的 TestIsLimitHitCodexUsesErrorLineOnly】旧测试把"prose 含 usage limit 措辞
// 不应命中"钦定为期望行为, 恰好锁定了 P1-1 漏识别路径 (审查判定"漏报方向未披露未裁,钦定为
// 期望行为")。新契约: prose 单行含 hardErr 措辞会被识别为限额(误报方向的可接受回退), 换真
// 限额永不漏识别——见 runner.go isLimitHitCodex 注释「双向都是可接受回退」段。
func TestIsLimitHitCodexScansAllCandidateLines(t *testing.T) {
	res := &claudeResult{Type: "result", IsError: true, Subtype: "codex_error"}

	// 场景 1 (P1-1 核心反例): transient 行在前 + 真限额行在后, 必须命中真限额。
	// 旧法首行挑一挑走 transient 行 (匹配 transientRe), limitRe 不命中 → 漏识别真限额。
	transientBeforeLimit := "network error: connection reset by peer while calling upstream\n" +
		"stream disconnected before completion\n" +
		"ERROR: You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage\n"
	if !isLimitHitCodex(res, transientBeforeLimit) {
		t.Fatal("P1-1 核心场景: transient 行在前 + 真限额行在后, 必须命中——" +
			"否则真限额被误判 transient 烧尽 attempts 落 held, 破坏无人值守 auto-resume")
	}

	// 场景 2 (反证): 纯 transient 无真限额措辞, 不得误命中。
	// 若误命中, transient 抖动会被挂 26h 等冷却, 是过矫枉。
	onlyTransient := "network error: connection reset by peer\n" +
		"timed out waiting for upstream\n" +
		"stream disconnected\n"
	if isLimitHitCodex(res, onlyTransient) {
		t.Fatal("纯 transient 抖动: 不得误命中 limit_paused (否则短时抖动被 26h 挂起)")
	}

	// 场景 3 (反证): 真限额行独存, 必命中——治 (b) 保护不丢。
	onlyLimit := "You've hit your usage limit · resets 8:20pm\n"
	if !isLimitHitCodex(res, onlyLimit) {
		t.Fatal("真限额行独存: 必须命中")
	}

	// 场景 4: codexNoiseRe 匹配的 codex 横幅/进度行不进候选集, 即便含 usage limit 字面量
	// 也不应误命中 (防未来 codexNoiseRe 被误改窄口径)。
	noiseOnly := "OpenAI Codex v0.42.0\n" +
		"workdir: /tmp/work\n" +
		"model: gpt-5.6-sol\n" +
		"reasoning: max\n"
	if isLimitHitCodex(res, noiseOnly) {
		t.Fatal("codex 横幅/配置块 (codexNoiseRe 全跳过): 无候选行, 不得命中")
	}
}

// 跨天（周限额）措辞：实战 429 "resets Jul 16 at 1am"——旧 resetTimeRe 跨不过 "Jul 16 at"
// → 落 30min 回退，每 30min 空转到真解冻。回归此坑。
func TestParseResetEpochCrossDay(t *testing.T) {
	cfg := &Config{LimitFallbackMin: 30, CooldownMarginSec: 90}
	loc := time.FixedZone("Asia/Shanghai", 8*3600)
	now := time.Date(2026, 7, 13, 14, 23, 0, 0, loc)

	got := parseResetEpoch("You've hit your limit · resets Jul 16 at 1am (Asia/Shanghai)", cfg, now)
	want := time.Date(2026, 7, 16, 1, 0, 0, 0, loc).Unix() + 90
	if got != want {
		t.Errorf("跨天解析: got %s, want %s", time.Unix(got, 0).In(loc), time.Unix(want, 0).In(loc))
	}

	// 带 :MM 与序数日 + "on" + 跨月（窗口内）
	julEndNow := time.Date(2026, 7, 30, 10, 0, 0, 0, loc)
	got = parseResetEpoch("resets on Aug 3rd at 9:30pm", cfg, julEndNow)
	want = time.Date(2026, 8, 3, 21, 30, 0, 0, loc).Unix() + 90
	if got != want {
		t.Errorf("序数日+分钟: got %s, want %s", time.Unix(got, 0).In(loc), time.Unix(want, 0).In(loc))
	}

	// 跨年：年末读到次年 1 月初的重置 → 滚到明年（1 月的日期本年已过，AddDate(1)）
	decNow := time.Date(2026, 12, 30, 12, 0, 0, 0, loc)
	got = parseResetEpoch("resets Jan 2 at 1am", cfg, decNow)
	want = time.Date(2027, 1, 2, 1, 0, 0, 0, loc).Unix() + 90
	if got != want {
		t.Errorf("跨年滚动: got %s, want %s", time.Unix(got, 0).In(loc), time.Unix(want, 0).In(loc))
	}

	// 越界（>14 天）不轻信 → 回退等待，不误锁多日
	got = parseResetEpoch("resets Sep 1 at 1am", cfg, now)
	want = now.Add(30*time.Minute).Unix() + 90
	if got != want {
		t.Errorf("越界回退: got %d, want %d", got, want)
	}
}

// TestLimitHitForEngineRoutesByFlags (CG-R1 R3 P2-3 修复反例)
// 钉住 runTask 三处限额判据统一路由: (useCodex, remote) 与远端子路径 remoteUsesClaude(t) 三向
// 分派到 wrapper 的映射。
// 【为什么必要】上一轮 R2 复审 concerns:isLimitHitClaude / isLimitHitCodex wrapper 单测钉住语义,
// 但 runTask call site 的"哪种组合 → 哪个 wrapper"分派仅靠 3 个手写 if 覆盖, 没测试拦截 → 有人
// 误改条件顺序 (如把 useCodex 分支挪到 remote 之前, 或漏掉 remoteUsesClaude 子路径), wrapper
// 单测全绿, 静默用错 wrapper → 事故仍会发生。
// 【差异化输入】用 "session limit reached" — limitRe 会命中 (session limit 在正则里), 但既不匹配
// codex 候选行判据 (transientRe/codexHardErrRe 均无此措辞) 也不属 codexNoiseRe → codexLimitScanText
// 返回空 → isLimitHitCodex=false; 反观 isLimitHitClaude 走 stderrTailFromClaudeCombined (无 `{`
// 时退回全量) → limitRe 命中 → true。这样同一输入让两 wrapper 给相反答案, 路由错拿必测试红。
func TestLimitHitForEngineRoutesByFlags(t *testing.T) {
	combined := "session limit reached\n"
	res := &claudeResult{Type: "result", IsError: true}

	// 前提确认: 差异化输入让两 wrapper 给相反答案 (否则测试无法钉住路由)。
	if !isLimitHitClaude(res, combined) {
		t.Fatal("差异化前提失效: isLimitHitClaude 未命中差异化输入, 无法钉住路由")
	}
	if isLimitHitCodex(res, combined) {
		t.Fatal("差异化前提失效: isLimitHitCodex 命中差异化输入, 无法钉住路由")
	}

	claudeTask := &Task{Model: "opus"}          // remoteUsesClaude=true (Model!="")
	codexPrefTask := &Task{PreferRunner: "codex"} // remoteUsesClaude=false (显式 codex)
	bareTask := &Task{}                          // remoteUsesClaude=false (无 Model 无 Review 类型)

	cases := []struct {
		name             string
		useCodex, remote bool
		t                *Task
		want             bool
	}{
		{"本地 claude → isLimitHitClaude", false, false, nil, true},
		{"本地 codex → isLimitHitCodex", true, false, nil, false},
		{"远端 claude 子路径 (Model!=\"\") → isLimitHitClaude", false, true, claudeTask, true},
		{"远端 codex 子路径 (PreferRunner=codex) → isLimitHitCodex", false, true, codexPrefTask, false},
		{"远端 codex 子路径 (无 Model) → isLimitHitCodex", false, true, bareTask, false},
		// useCodex=true 且 remote=true: remote 分支优先, 由 remoteUsesClaude 决定
		// (useCodex 在远端不影响路由——远端引擎选择由 Task 字段承担)。
		{"远端 + useCodex=true + Model!=\"\" → 仍走 claude 子路径", true, true, claudeTask, true},
	}
	for _, tc := range cases {
		got := limitHitForEngine(tc.useCodex, tc.remote, tc.t, res, combined)
		if got != tc.want {
			t.Errorf("%s: limitHitForEngine=%v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestParseResetEpochTryAgainAtPhrase (BD-42/CG-1c 回归)
// 实战场景：codex 远端限额错误原文用 "try again at <英文月份日期>" 措辞而非 "reset[s]"，且远端
// 输出常被截断（分钟位只剩半截数字）。旧解析（resetDateRe/resetTimeRe 均只认 "reset[s]" 前缀）
// 完全不识别此形态 → 落 cfg.LimitFallbackMin 回退 → 每 30min 空撞到真解冻（跨天限额，代价远大于
// 多等）。t0730-2007-5b2f 实测：从 03:05 起已空撞多轮，直到 8 月 5 日才能真正解冻。
//
// 【反例】把 parseResetEpoch 里 resetTryAgainRe/resetTryAgainDateOnlyRe 两段解析删掉（回退到
// 修复前状态），本测试第一个 case 报红：resume 时间落在 31min 后而非 8 月 5 日。
func TestParseResetEpochTryAgainAtPhrase(t *testing.T) {
	cfg := &Config{LimitFallbackMin: 31, CooldownMarginSec: 90}
	loc := time.UTC
	now := time.Date(2026, 7, 31, 3, 5, 0, 0, loc)

	// 实战原文：远端输出在分钟位被截断（"12:0…"），年份显式给出。
	realWorld := "You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage " +
		"to purchase more credits or try again at Aug 5th, 2026 12:0…"
	got := parseResetEpoch(realWorld, cfg, now)
	gotTime := time.Unix(got, 0).In(loc)
	if gotTime.Year() != 2026 || gotTime.Month() != time.August || gotTime.Day() != 5 {
		t.Fatalf("截断实战原文应解出 8 月 5 日，got %s (raw=%d)", gotTime, got)
	}
	// 反证：不得落回 31min 回退（跨天限额下这是空撞的根因）。
	if fallback := now.Add(31 * time.Minute).Unix() + 90; got == fallback {
		t.Fatalf("不得回退到 cfg.LimitFallbackMin, got 与 31min 回退值相同: %d", got)
	}

	// 完整变体：带分钟与 am/pm、"August"全称、无逗号——解析应精确到分钟。
	full := "Sorry, please try again at August 5 2026 at 9:30pm"
	got = parseResetEpoch(full, cfg, now)
	want := time.Date(2026, 8, 5, 21, 30, 0, 0, loc).Unix() + 90
	if got != want {
		t.Errorf("完整变体应精确解析: got %s, want %s", time.Unix(got, 0).In(loc), time.Unix(want, 0).In(loc))
	}

	// 仅月+日、完全无时刻（更狠的截断/格式外变体）：置信不足，保守退避到"明日同时刻"而非 31min。
	dateOnly := "usage limit reached, try again at Sep"
	got = parseResetEpoch(dateOnly, cfg, now)
	want = now.Add(24 * time.Hour).Unix() + 90
	if got != want {
		t.Errorf("日期不完整时应退避到明日同时刻: got %s, want %s", time.Unix(got, 0).In(loc), time.Unix(want, 0).In(loc))
	}

	// 反例（保持现行为）：不含日期的限额文本，仍应落 cfg.LimitFallbackMin 回退，不受本次改动影响。
	noDate := "You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage"
	got = parseResetEpoch(noDate, cfg, now)
	want = now.Add(31 * time.Minute).Unix() + 90
	if got != want {
		t.Errorf("无日期文本应保持原 31min 回退: got %s, want %s", time.Unix(got, 0).In(loc), time.Unix(want, 0).In(loc))
	}
}

// TestParseResetEpochTryAgainNearMissDoesNotOversleep (CG-1c 修复轮1 P1-1 核心反例)
// 复审 t0731-0424-f127 发现：实战截断文案 "try again at Aug 5th, 2026 12:0…" 里的分钟位
// 只截出一个 "0"，resetTryAgainRe 的 (?::(\d{2}))? 吃不满 2 位数字，minute 回落成 0，于是精确
// 解成 "12:00" ——而实际分钟可能是 00~09 里的任意值。如果唤醒发生在真实重置点前后几分钟内
// （比如 12:03 复撞重解析），此时 cand(12:00) 已 <= now(12:03)，旧法会贯穿到 resetTryAgainDateOnlyRe
// 分支——它与 resetTryAgainRe 共享 "try again at <month>" 前缀且无条件命中——返回 now+24h。
// 于是本该几分钟内解冻的限额被错判过睡近 24 小时（修复前基线：此路径只需承担 cfg.LimitFallbackMin
// 的短回退代价，即 31min）。
//
// 【反例】把 runner.go parseResetEpoch 里 resetTryAgainDateOnlyRe 分支的 `!tryAgainFullMatched &&`
// 门槛去掉（或把完整形态分支里 `if !cand.After(now) { return ...LimitFallbackMin... }` 短回退删掉），
// 本测试报红：返回值会变成 now+24h 而非 now+31min。
func TestParseResetEpochTryAgainNearMissDoesNotOversleep(t *testing.T) {
	cfg := &Config{LimitFallbackMin: 31, CooldownMarginSec: 90}
	loc := time.UTC
	// 复撞重解析发生在真实重置点（12:00~12:09 之间某刻）之后几分钟：now=12:03。
	now := time.Date(2026, 8, 5, 12, 3, 0, 0, loc)

	realWorld := "You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage " +
		"to purchase more credits or try again at Aug 5th, 2026 12:0…"
	got := parseResetEpoch(realWorld, cfg, now)
	want := now.Add(31 * time.Minute).Unix() + 90
	if got != want {
		t.Fatalf("cand(12:00) <= now(12:03) 应落 cfg.LimitFallbackMin 短回退, got %s (resume=%s), want %s (resume=%s)",
			time.Unix(got, 0).In(loc), time.Unix(got, 0).In(loc),
			time.Unix(want, 0).In(loc), time.Unix(want, 0).In(loc))
	}
	// 反证：不得过睡到 24h 兜底——这正是复审揪出的过睡放大。
	oversleep := now.Add(24 * time.Hour).Unix() + 90
	if got == oversleep {
		t.Fatalf("不得落 24h 过睡兜底: got 与 24h 过睡值相同 %d", got)
	}
}

// TestParseResetEpochDateRecentlyPassedFallsBackShort (CG-1c 修复轮1 · 按类审计)
// 复审要求按类排查同构缺陷：resetDateRe（"resets <month> <day> at <time>" 措辞）与
// resetTryAgainRe 结构相同（月+日+时精确解析、window 校验失败/cand<=now 时会 fall through），
// 但 resetDateRe **没有**配对的宽松 date-only 兜底正则（本仓只有 resetTryAgainDateOnlyRe，
// 专配 resetTryAgainRe），所以 resetDateRe 分支 fall through 之后既不匹配 resetTryAgainRe/
// resetTryAgainDateOnlyRe（前缀是 "try again at" 不是 "reset[s]"），也不会误配 resetTimeRe
// （"reset[s]?" 后必须紧跟空白+可选 "at"+数字，中间的月份日期文本挡住），最终会安全落到
// cfg.LimitFallbackMin 短回退——本测试钉住这条审计结论，防止未来有人给 resetDateRe 也加一条
// 无条件的 date-only 兜底、复现同一 class 的过睡缺陷。
//
// 【反例】若未来给 resetDateRe 配一条无条件 "resets <month>" 宽松兜底（重现 P1-1 那种耦合），
// 本测试会报红：返回值从 now+31min 变成某个远期兜底值。
func TestParseResetEpochDateRecentlyPassedFallsBackShort(t *testing.T) {
	cfg := &Config{LimitFallbackMin: 31, CooldownMarginSec: 90}
	loc := time.UTC
	now := time.Date(2026, 8, 5, 12, 3, 0, 0, loc)

	// 与 resetTryAgainRe 场景对称构造：分钟位截断到只剩一位，精确解析早判为 12:00。
	truncated := "You've hit your limit · resets Aug 5 at 12:0"
	got := parseResetEpoch(truncated, cfg, now)
	want := now.Add(31 * time.Minute).Unix() + 90
	if got != want {
		t.Fatalf("resetDateRe 的 cand<=now 场景应安全落短回退（无配对 date-only 兜底可贯穿）, got %s, want %s",
			time.Unix(got, 0).In(loc), time.Unix(want, 0).In(loc))
	}
}
