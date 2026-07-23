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

// TestIsLimitHitCodexUsesErrorLineOnly (CG-R1 修复反例 · 治 (b))
// codex 分支不再扫全量 transcript, 改用 codexErrorLine 挑出的错误行——codexHardErrRe 已含
// "usage limit"、"quota exceeded" 等真限额措辞, 真限额行会被挑出保护不丢。
// 【反例】造 combined = 大量 audit prose(含 usage limit 字面量) + 一行真 transient error("timed out");
// codexErrorLine 会挑出 "timed out" 那行(transient 优先且 audit 正文常被 codexNoiseRe 覆盖),
// 不含 usage limit → isLimitHitCodex 不命中. 旧 isLimitHit(res, combined) 会全量扫命中 → 反例红.
func TestIsLimitHitCodexUsesErrorLineOnly(t *testing.T) {
	// codexErrorLine 是首匹配即返回(遍历行序):把 transient 行放在前, audit prose 里含"usage limit"
	// 字面量放在后。挑出的是 transient 行不含限额, scan 不命中. 治 (b) 的语义:不再全量扫,收敛到
	// 挑出的错误行——残余风险(prose 单行含 hardErr 措辞被首挑) 用户已裁接受, 相较全量 100→1 行数量级.
	combined := "network error: connection reset by peer while calling upstream\n" +
		"the reviewer discussed usage limit thresholds in policy doc\n"
	res := &claudeResult{Type: "result", IsError: true, Subtype: "codex_error", Result: "", ResultFromTranscript: false}
	if isLimitHitCodex(res, combined) {
		t.Fatal("audit prose 含 usage limit 但真错误行是 transient——isLimitHitCodex 不应误命中")
	}
	// 反证:真 codex 限额行必须命中.
	realLimitCombined := "some noise\n" + "ERROR: You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage\n"
	if !isLimitHitCodex(res, realLimitCombined) {
		t.Fatal("真 codex 限额行(codexHardErrRe 含 usage limit) 必须命中——治 (b) 保护不丢")
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
