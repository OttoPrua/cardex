package main

// codex 失败可靠性回归：codexErrorLine 必须跨过 codex 横幅/配置噪声取真错误，
// transientRe 必须认得 codex 的瞬时网络错误——否则失败一律显示 "Reading additional input from stdin"
// 且被当硬失败烧 attempts，单腿审核一挂就没了。

import (
	"strings"
	"testing"
)

// 真实 codex exec 失败输出的形态（横幅在第一行，真错误在末尾）。
const codexFailOutput = `Reading additional input from stdin...
OpenAI Codex v0.144.1
--------
workdir: /x
model: mock-model
provider: openai
approval: never
sandbox: read-only
reasoning effort: xhigh
session id: 019f-...
--------
user
审核一下这个改动
stream error: stream disconnected before completion
`

func TestCodexErrorLineSkipsBanner(t *testing.T) {
	got := codexErrorLine(codexFailOutput)
	// 必须取到真错误,绝不能是横幅。
	if strings.Contains(got, "Reading additional input") || strings.HasPrefix(got, "OpenAI Codex") {
		t.Fatalf("codexErrorLine 取到了横幅噪声而非真错误: %q", got)
	}
	if !strings.Contains(got, "stream disconnected") {
		t.Fatalf("codexErrorLine 应取到真错误 'stream disconnected', got %q", got)
	}
	// 且提取出的真错误应被 transientRe 认作瞬时错误（→ 退避重试而非硬失败）。
	if !transientRe.MatchString(got) {
		t.Fatalf("提取的 codex 网络错误应匹配 transientRe（否则被当硬失败）: %q", got)
	}
}

// 老 bug 实证：横幅本身不匹配 transientRe——所以 firstLine 取横幅时,瞬时错误被误判硬失败。
func TestBannerNotTransient(t *testing.T) {
	if transientRe.MatchString("Reading additional input from stdin...") {
		t.Fatal("横幅不该匹配 transientRe（否则测试构造无意义）")
	}
	// 而 firstLine 恰好会取到横幅——这正是被修的 bug。
	if fl := firstLine(codexFailOutput); !strings.Contains(fl, "Reading additional input") {
		t.Fatalf("前提:firstLine 取到横幅(旧 bug), got %q", fl)
	}
}

func TestTransientMatchesCodexNetErrors(t *testing.T) {
	transient := []string{
		"stream error: stream disconnected before completion",
		"error sending request to https://...: connection reset by peer",
		"connection refused",
		"503 Service Unavailable",
		"read tcp 10.0.0.1:443: i/o timeout",
		"request timed out",
	}
	for _, s := range transient {
		if !transientRe.MatchString(s) {
			t.Fatalf("应认作瞬时错误(退避重试): %q", s)
		}
	}
	// 反例守卫：真正的硬错误不应被误判为瞬时（否则永远重试不失败）。
	for _, s := range []string{"invalid api key", "model not supported", "permission denied writing file"} {
		if transientRe.MatchString(s) {
			t.Fatalf("硬错误不该匹配 transientRe: %q", s)
		}
	}
}

// 无 transient/硬错误行时返回 ""，由调用方回退到真实 runErr / "无最终消息"诊断——
// 绝不回退到横幅或从 transcript 里瞎抓一行。
func TestCodexErrorLineEmptyWhenNoRealError(t *testing.T) {
	onlyNoise := "Reading additional input from stdin...\nOpenAI Codex v0.144.1\n--------\nmodel: x\n"
	if got := codexErrorLine(onlyNoise); got != "" {
		t.Fatalf("全噪声应返回 空, got %q", got)
	}
}

// 核心回归：codex exec 把整段审查正文写进 stderr，正文天然含 "cannot/error/failed/invalid"。
// codexErrorLine 绝不能把这类无害审查内容当"错误"上报（旧 bug：报成 "...You cannot rationalize..."，
// 掩盖真因=superpowers/gsd 框架注入耗尽回合预算的空终稿故障）。
func TestCodexErrorLineIgnoresReviewProse(t *testing.T) {
	prose := `user
对抗复审 L5.1-b——独立验证 R+2 修复是否真闭。
我会严格只读，先核对提交/差异。
This is not negotiable. This is not optional. You cannot rationalize your way out of this.
原漏洞是命令退0/磁盘0写；若断言未覆盖生产反例=假闭 block。
appendTombstone 的失败被吞掉，CLI 无条件退 0，这是 invalid 的。
`
	if got := codexErrorLine(prose); got != "" {
		t.Fatalf("审查正文里的 cannot/invalid/block 不该被当错误上报, got %q", got)
	}
}

// codexHardErrRe：服务端硬错误（含 OpenAI 网络安全审查闸）必须被清晰上报，别被吞成空。
func TestCodexErrorLineSurfacesProviderHardErrors(t *testing.T) {
	cases := map[string]string{
		"网络安全审查闸":  "This request has been flagged for possible cybersecurity risk.",
		"cloudflare 拦": "Access blocked by Cloudflare",
		"高负载":       "Codex is currently experiencing high load.",
		"预算耗尽":      "Goal budget reached - the turn was stopped.",
	}
	for name, line := range cases {
		combined := "OpenAI Codex v0.144.1\n--------\nmodel: x\nuser\n审核\n" + line + "\n"
		got := codexErrorLine(combined)
		if got == "" || !strings.Contains(got, strings.SplitN(line, " ", 3)[0]) {
			t.Fatalf("%s: 服务端硬错误应被上报, line=%q got=%q", name, line, got)
		}
	}
}
