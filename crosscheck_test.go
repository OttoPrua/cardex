package main

// 交叉验证链（fable 顶替流）回归测试。
// 机制：引擎甲独立作答(A) → 引擎乙独立作答(B，绝不见甲结论) → 引擎乙拿甲结论对抗式交叉查漏(C)。
// 独立性：甲结论不进任何交叉卡的字段/盘上文件、不进 A 的日志（被动暴露最小化 + 行为护栏，非硬沙箱）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testCrossCfg() *Config {
	return &Config{
		CodexBin:            "/usr/bin/codex",
		CodexModel:          "codex-model-x",
		TypeDefaults:        map[string]TypeDefaults{},
		DefaultCrossProfile: "opus-codex",
		CrossProfiles: map[string]CrossProfile{
			"opus-codex": {
				A: CrossEngine{Kind: "claude", Model: "claude-opus-4-8", Effort: "max", Label: "opus·max"},
				B: CrossEngine{Kind: "codex", Effort: "max", Label: "codex·max"},
			},
		},
	}
}

func findByPrefix(t *testing.T, root, prefix string) *Task {
	t.Helper()
	for _, x := range listQueued(t, root) {
		if strings.HasPrefix(x.Title, prefix) {
			return x
		}
	}
	return nil
}

// A 完成 → 甲结论落隔离侧车（不进 B 卡）；派引擎乙独立作答的 B 卡：套引擎乙、prompt 与 A 相同。
// 核心不变式：B 的盘上文件绝不含甲结论（被动暴露最小化，不只是"不进 prompt"）。
func TestCrossAtoB(t *testing.T) {
	root := testRoot(t)
	cfg := testCrossCfg()
	const aSecret = "甲的机密结论：verdict=通过，理由 XYZ-独有洞"
	a := newTask(root, cfg, typeCrossCheck, "交叉A[opus-codex]: 裁决X", "/tmp/proj",
		[]string{"独立作答模板。任务=裁决X。第一性原理+对抗式自审。"}, 5)
	a.XRole = "A"
	a.XKey = "xopaquekey01"
	a.XProfile = "opus-codex"
	a.XTask = "裁决X 的原始任务文本"
	if err := applyCrossEngine(a, cfg.CrossProfiles["opus-codex"].A, cfg); err != nil {
		t.Fatal(err)
	}
	fb, ferr := freezeCrossEngine(cfg.CrossProfiles["opus-codex"].B, cfg) // 冻结乙引擎钉进 A
	if ferr != nil {
		t.Fatal(ferr)
	}
	a.XEngineB = fb
	if err := saveTask(root, a); err != nil {
		t.Fatal(err)
	}
	postComplete(root, cfg, a, &claudeResult{Result: aSecret}, nil)

	b := findByPrefix(t, root, "交叉B[")
	if b == nil {
		t.Fatal("A 完成应自动派出 B 卡")
	}
	if b.XRole != "B" || b.XProfile != "opus-codex" || b.XKey != a.XKey || b.XTask != a.XTask {
		t.Fatalf("B 谱系字段继承错误: %+v", b)
	}
	// 引擎乙(codex)已套上(冻结规格)，思考档=max，codex 模型冻结、XEngineB 随链传递。
	if b.PreferRunner != "codex" || b.Effort != "max" || b.Model != "" || b.RemoteHost != "" {
		t.Fatalf("B 应套引擎乙(codex·max): %+v", b)
	}
	if b.XCodexModel != cfg.CodexModel || b.XEngineB == nil {
		t.Fatalf("B 应带冻结的 codex 模型与 XEngineB: XCodexModel=%q XEngineB=%v", b.XCodexModel, b.XEngineB)
	}
	// B 的 prompt 必须与 A 相同（公平的独立作答）。
	if b.Prompts[0] != a.Prompts[0] {
		t.Fatalf("B 的独立作答 prompt 应与 A 相同\nA=%q\nB=%q", a.Prompts[0], b.Prompts[0])
	}
	// 独立性铁律①：B 卡的**盘上文件**绝不含甲结论（不只是 prompt）——B 的执行器读自己的 json 也读不到 A。
	raw, err := os.ReadFile(taskPath(root, b.ID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), aSecret) || strings.Contains(string(raw), "机密") {
		t.Fatalf("B 卡盘上文件泄漏了甲结论，独立性破产:\n%s", raw)
	}
	// 独立性铁律②：甲结论落隔离侧车（编排进程读写、0600、不进 B 卡字段/日志）。
	peer, err := os.ReadFile(crossPeerPath(root, a.XKey))
	if err != nil || string(peer) != aSecret {
		t.Fatalf("甲结论应落隔离侧车 %s, got %q err=%v", crossPeerPath(root, a.XKey), peer, err)
	}
	if b.Title != "交叉B[opus-codex]: 裁决X" {
		t.Fatalf("B 标题应剥前缀用谱系根标题, got %q", b.Title)
	}
}

// B 完成 → 从侧车取甲结论 → 派引擎乙交叉查漏的 C 卡：合并模板注入**完整**甲+乙+TASK；侧车用后删除。
func TestCrossBtoC(t *testing.T) {
	root := testRoot(t)
	cfg := testCrossCfg()
	b := newTask(root, cfg, typeCrossCheck, "交叉B[opus-codex]: 裁决X", "/tmp/proj", []string{"独立作答"}, 5)
	b.XRole = "B"
	b.XKey = "xkey-btoc"
	b.XProfile = "opus-codex"
	b.XTask = "裁决X 的原始任务文本"
	b.XEngineB, _ = freezeCrossEngine(cfg.CrossProfiles["opus-codex"].B, cfg)
	if err := saveTask(root, b); err != nil {
		t.Fatal(err)
	}
	// 侧车（A 的结论）由 A 阶段落盘；这里预置以驱动 B→C。
	if err := writeCrossPeer(root, "xkey-btoc", "甲结论内容AAA-仅甲发现的漏洞"); err != nil {
		t.Fatal(err)
	}
	postComplete(root, cfg, b, &claudeResult{Result: "乙结论内容BBB-仅乙发现的边界"}, nil)

	c := findByPrefix(t, root, "交叉C汇总[")
	if c == nil {
		t.Fatal("B 完成应自动派出 C 卡")
	}
	if c.XRole != "C" || c.XKey != "xkey-btoc" || c.XProfile != "opus-codex" {
		t.Fatalf("C 谱系字段错误: %+v", c)
	}
	if !c.EmitProgress || c.ProgressKey != "xkey-btoc" {
		t.Fatalf("C 应把最终结论落进度报告(键=XKey): %+v", c)
	}
	if c.PreferRunner != "codex" || c.Effort != "max" || c.XCodexModel != cfg.CodexModel || c.XEngineB == nil {
		t.Fatalf("交叉查漏应由引擎乙(codex·max, 冻结规格)执行: %+v", c)
	}
	p := c.Prompts[0]
	for _, want := range []string{"甲结论内容AAA-仅甲发现的漏洞", "乙结论内容BBB-仅乙发现的边界", "裁决X 的原始任务文本", "交叉审查"} {
		if !strings.Contains(p, want) {
			t.Fatalf("C 的合并 prompt 缺少 %q\n----\n%s", want, p)
		}
	}
	// 侧车使命完成应删除。
	if _, err := os.Stat(crossPeerPath(root, "xkey-btoc")); !os.IsNotExist(err) {
		t.Fatalf("C 派出后侧车应被删除, stat err=%v", err)
	}
}

// C 是终点：合规结论（含 verdict json）不再派卡、不留 LastError。
func TestCrossCTerminal(t *testing.T) {
	root := testRoot(t)
	if err := os.MkdirAll(progressDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := testCrossCfg()
	c := newTask(root, cfg, typeCrossCheck, "交叉C汇总[opus-codex]: 裁决X", "/tmp/proj", []string{"合并"}, 5)
	c.XRole = "C"
	c.XKey = "k"
	c.XProfile = "opus-codex"
	c.EmitProgress = true
	c.ProgressKey = "k"
	if err := saveTask(root, c); err != nil {
		t.Fatal(err)
	}
	before := len(listQueued(t, root))
	postComplete(root, cfg, c, &claudeResult{Result: "分析…\n```json\n{\"verdict\":\"合并通过\",\"confidence\":\"high\",\"summary\":\"ok\"}\n```"}, nil)
	if len(listQueued(t, root)) != before {
		t.Fatal("C 完成不应再派任何交叉卡（终点）")
	}
	if c.LastError != "" || c.Status == statusFailed {
		t.Fatalf("合规 C 不应留 LastError/失败, got status=%q err=%q", c.Status, c.LastError)
	}
}

// B5：C 结论未按合并契约收尾（无 verdict json）→ 在 C 卡 LastError 显式留痕，不冒充有效终局。
func TestCrossCNonConformingVisible(t *testing.T) {
	root := testRoot(t)
	if err := os.MkdirAll(progressDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := testCrossCfg()
	c := newTask(root, cfg, typeCrossCheck, "交叉C汇总[opus-codex]: X", "/tmp/proj", []string{"merge"}, 5)
	c.XRole = "C"
	c.XKey = "k"
	c.XProfile = "opus-codex"
	c.EmitProgress = true
	c.ProgressKey = "k"
	if err := saveTask(root, c); err != nil {
		t.Fatal(err)
	}
	postComplete(root, cfg, c, &claudeResult{Result: "一段没有 json 结论块的自由文本"}, nil)
	if !strings.Contains(c.LastError, "未按合并契约") || c.Status != statusFailed {
		t.Fatalf("非合规 C 应置 failed + LastError 留痕, got status=%q err=%q", c.Status, c.LastError)
	}
}

func TestCrossMergeVerdictOK(t *testing.T) {
	if !crossMergeVerdictOK("分析…\n```json\n{\"verdict\":\"合并结论\",\"confidence\":\"high\"}\n```") {
		t.Fatal("含 verdict+合法 confidence 应 OK")
	}
	if crossMergeVerdictOK("没有 json 的自由文本") {
		t.Fatal("无 json 应 false")
	}
	if crossMergeVerdictOK("```json\n{\"confidence\":\"high\"}\n```") {
		t.Fatal("缺 verdict 字段应 false")
	}
	if crossMergeVerdictOK("```json\n{\"verdict\":\"\",\"confidence\":\"high\"}\n```") {
		t.Fatal("空 verdict 应 false")
	}
	// B5：verdict 非空但缺枚举 confidence（无结构 verdict 的 banana 伪合规）应被拦。
	if crossMergeVerdictOK("```json\n{\"verdict\":\"banana\"}\n```") {
		t.Fatal("verdict 非空但无枚举 confidence 应 false")
	}
	if crossMergeVerdictOK("```json\n{\"verdict\":\"通过\",\"confidence\":\"很高\"}\n```") {
		t.Fatal("confidence 非 high/medium/low 应 false")
	}
}

// B5 展示面：交叉合并报告在 list 状态行与 progress -show 都要显示,而非落空显 —。
func TestReportStatusCrossMerge(t *testing.T) {
	r := map[string]any{"verdict": "合并通过", "confidence": "high", "summary": "一句话最终结论"}
	if got := reportStatus(r); !strings.Contains(got, "一句话最终结论") {
		t.Fatalf("交叉合并报告的 list 状态应显示结论,而非 —, got %q", got)
	}
}

// renderTemplate 单遍替换：注入值含字面 {{B}} 不被二次替换（防甲结论把乙顶掉/非确定）。
func TestRenderTemplateSinglePass(t *testing.T) {
	out := renderTemplate("A={{A}} | B={{B}}", map[string]string{"A": "甲含 {{B}} 字样", "B": "乙内容"})
	if out != "A=甲含 {{B}} 字样 | B=乙内容" {
		t.Fatalf("注入值被二次替换（单遍失败）: %q", out)
	}
	if got := renderTemplate("x {{UNKNOWN}} y", map[string]string{"A": "1"}); got != "x {{UNKNOWN}} y" {
		t.Fatalf("未知占位符应原样保留, got %q", got)
	}
	for i := 0; i < 30; i++ { // 确定性：与 map 迭代顺序无关
		if renderTemplate("{{A}}{{B}}{{C}}", map[string]string{"A": "a", "B": "b", "C": "c"}) != "abc" {
			t.Fatal("渲染结果应确定")
		}
	}
}

// B4：链断裂（profile 缺失等）必须在母卡 LastError 显式留痕，且不产卡——不让单腿结果冒充终局。
func TestCrossChainBreakVisible(t *testing.T) {
	root := testRoot(t)
	cfg := testCrossCfg()
	a := newTask(root, cfg, typeCrossCheck, "交叉A[opus-codex]: X", "/tmp/proj", []string{"solo"}, 5)
	a.XRole = "A"
	a.XKey = "k"
	a.XProfile = "opus-codex"
	// 不设 XEngineB（模拟旧卡/数据损坏）→ handleCrossStage 断链
	if err := saveTask(root, a); err != nil {
		t.Fatal(err)
	}
	before := len(listQueued(t, root))
	postComplete(root, cfg, a, &claudeResult{Result: "甲结论"}, nil)
	// B4：置 failed（list 对 failed 显示 LastError 且不折叠）+ 留痕，断裂才可见（round-1 只写 LastError=不可见）。
	if a.Status != statusFailed || !strings.Contains(a.LastError, "交叉链断裂") {
		t.Fatalf("链断应把母卡置 failed + LastError 留痕, got status=%q err=%q", a.Status, a.LastError)
	}
	if len(listQueued(t, root)) != before {
		t.Fatal("链断不应产卡")
	}
}

func TestApplyCrossEngine(t *testing.T) {
	cfg := testCrossCfg() // 有 codex_bin + codex_model
	cfgR := &Config{CodexBin: "c", CodexModel: "m", RemoteHosts: map[string]RemoteHostConfig{"h": {}}}

	// claude：设 Model+Effort，不钉 codex/远端。
	tt := &Task{Prompts: []string{"x"}}
	if err := applyCrossEngine(tt, CrossEngine{Kind: "claude", Model: "claude-opus-4-8", Effort: "max"}, cfg); err != nil {
		t.Fatal(err)
	}
	if tt.Model != "claude-opus-4-8" || tt.Effort != "max" || tt.PreferRunner != "" || tt.RemoteHost != "" {
		t.Fatalf("claude 引擎字段错误: %+v", tt)
	}
	// B2：claude 引擎缺 model → 报错（防跑成账号默认模型、质量静默降级）。
	if applyCrossEngine(&Task{Prompts: []string{"x"}}, CrossEngine{Kind: "claude", Effort: "max"}, cfg) == nil {
		t.Fatal("claude 引擎缺 model 应报错")
	}

	// codex：需 codex_bin + codex_model；缺则报错。
	if applyCrossEngine(&Task{Prompts: []string{"x"}}, CrossEngine{Kind: "codex"}, &Config{}) == nil {
		t.Fatal("codex 引擎缺 codex_bin 应报错")
	}
	if applyCrossEngine(&Task{Prompts: []string{"x"}}, CrossEngine{Kind: "codex", Effort: "max"}, &Config{CodexBin: "c"}) == nil {
		t.Fatal("codex 引擎缺 codex_model 应报错")
	}
	tt2 := &Task{Prompts: []string{"x"}}
	if err := applyCrossEngine(tt2, CrossEngine{Kind: "codex", Effort: "max"}, cfg); err != nil {
		t.Fatal(err)
	}
	if tt2.PreferRunner != "codex" || tt2.Effort != "max" {
		t.Fatalf("codex 引擎应钉 runner=codex 且带 Effort: %+v", tt2)
	}
	// B2 残留：codex 引擎写了 model 是零效 no-op（模型由 codex_model 决定）→ 报错防误导。
	if applyCrossEngine(&Task{Prompts: []string{"x"}}, CrossEngine{Kind: "codex", Effort: "max", Model: "gpt-x"}, cfg) == nil {
		t.Fatal("codex 引擎写 model（no-op）应报错")
	}
	if applyCrossEngine(&Task{Prompts: []string{"x"}}, CrossEngine{Kind: "remote-codex", Host: "h", Model: "gpt-x"}, cfgR) == nil {
		t.Fatal("remote-codex 引擎写 model（no-op）应报错")
	}

	// remote-claude：必须有 model。
	if applyCrossEngine(&Task{Prompts: []string{"x"}}, CrossEngine{Kind: "remote-claude", Host: "h"}, cfgR) == nil {
		t.Fatal("remote-claude 缺 model 应报错")
	}
	tt3 := &Task{Prompts: []string{"x"}}
	if err := applyCrossEngine(tt3, CrossEngine{Kind: "remote-claude", Host: "h", Model: "claude-fable-5"}, cfgR); err != nil {
		t.Fatal(err)
	}
	if tt3.RemoteHost != "h" || tt3.Model != "claude-fable-5" || !remoteUsesClaude(tt3) {
		t.Fatalf("remote-claude 应远端 claude: %+v", tt3)
	}

	// remote-codex：无模型 → 远端 codex。
	tt4 := &Task{Prompts: []string{"x"}}
	if err := applyCrossEngine(tt4, CrossEngine{Kind: "remote-codex", Host: "h"}, cfgR); err != nil {
		t.Fatal(err)
	}
	if tt4.RemoteHost != "h" || tt4.Model != "" || remoteUsesClaude(tt4) {
		t.Fatalf("remote-codex 应远端 codex: %+v", tt4)
	}

	// 未配置远端主机 / 未知 kind → 报错。
	if applyCrossEngine(&Task{Prompts: []string{"x"}}, CrossEngine{Kind: "remote-codex", Host: "nope"}, cfgR) == nil {
		t.Fatal("未配置远端主机应报错")
	}
	if applyCrossEngine(&Task{Prompts: []string{"x"}}, CrossEngine{Kind: "bogus"}, cfg) == nil {
		t.Fatal("未知 kind 应报错")
	}

	// 复用时先清引擎相关字段防串味。
	tt5 := &Task{Prompts: []string{"x"}, Model: "old", Effort: "low", PreferRunner: "codex", RemoteHost: "old"}
	if err := applyCrossEngine(tt5, CrossEngine{Kind: "claude", Model: "m"}, cfg); err != nil {
		t.Fatal(err)
	}
	if tt5.PreferRunner != "" || tt5.RemoteHost != "" || tt5.Effort != "" || tt5.Model != "m" {
		t.Fatalf("切引擎应清残留字段: %+v", tt5)
	}
}

func TestCrossEngineLoc(t *testing.T) {
	cases := []struct {
		eng  CrossEngine
		want string
	}{
		{CrossEngine{Kind: "claude"}, "local"},
		{CrossEngine{Kind: "codex"}, "local"},
		{CrossEngine{Kind: "remote-claude", Host: "h"}, "remote:h"},
		{CrossEngine{Kind: "remote-codex", Host: "h"}, "remote:h"},
	}
	for _, c := range cases {
		if got := crossEngineLoc(c.eng); got != c.want {
			t.Fatalf("crossEngineLoc(%+v)=%q, want %q", c.eng, got, c.want)
		}
	}
}

// 甲乙执行位置不同的 profile 必须在 cmdCross 落卡前被拒绝（否则 B/C 拿错目录被派到错误机器）。
func TestCmdCrossRejectsCrossLocationProfile(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"tasks", "logs"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := `{"codex_bin":"/usr/bin/codex","codex_model":"m","remote_hosts":{"h":{}},` +
		`"cross_profiles":{"mixed":{"a":{"kind":"claude","model":"claude-opus-4-8"},"b":{"kind":"remote-codex","host":"h"}}}}`
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	err := cmdCross([]string{"-root", root, "-profile", "mixed", "-dir", work, "任务内容"})
	if err == nil || !strings.Contains(err.Error(), "执行位置不同") {
		t.Fatalf("跨位置 profile 应被拒绝, got: %v", err)
	}
}

// B1：-dir 不能是 claudego 数据根（否则交叉卡 cwd 直接含 tasks/，B 一读就见 A）。
func TestCmdCrossRejectsDataRootDir(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"tasks", "logs"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"codex_bin":"/usr/bin/codex","codex_model":"m"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdCross([]string{"-root", root, "-dir", root, "任务内容"}); err == nil || !strings.Contains(err.Error(), "数据根") {
		t.Fatalf("-dir=数据根应被拒绝, got %v", err)
	}
}

func TestCrossBase(t *testing.T) {
	cases := map[string]string{
		"交叉A[opus-codex]: 裁决X":  "裁决X",
		"交叉B[opus-codex]: 裁决X":  "裁决X",
		"交叉C汇总[opus-codex]: 契约歧义": "契约歧义",
		"交叉A[my-pair]: 审 R4":    "审 R4",
		"普通标题":                  "普通标题",
	}
	for in, want := range cases {
		if got := crossBase(in); got != want {
			t.Fatalf("crossBase(%q)=%q, want %q", in, got, want)
		}
	}
}

// cmdCross 只铺 A 卡；A 套引擎甲、谱系键**不透明**（非 A 卡 ID）、带原始任务、prompt 由 solo 模板渲染。
func TestCmdCrossCreatesACard(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"tasks", "logs"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// 最小 config：codex_bin+codex_model（引擎乙预检要）；cross_profiles/default 由 defaultConfig 合并提供。
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"codex_bin":"/usr/bin/codex","codex_model":"codex-model-x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	if err := cmdCross([]string{"-root", root, "-dir", work, "-title", "配置键裁决", "某配置键缺省时的契约语义，请裁决"}); err != nil {
		t.Fatal(err)
	}
	a := findByPrefix(t, root, "交叉A[opus-codex]: ")
	if a == nil {
		t.Fatal("cmdCross 应入队 A 卡")
	}
	if a.XRole != "A" || a.XProfile != "opus-codex" {
		t.Fatalf("A 谱系字段错误: %+v", a)
	}
	// 谱系键不透明：非 A 卡 ID，前缀 x。
	if a.XKey == a.ID || !strings.HasPrefix(a.XKey, "x") {
		t.Fatalf("XKey 应为不透明键(非 A 卡 ID), got XKey=%q id=%q", a.XKey, a.ID)
	}
	if !strings.Contains(a.XTask, "某配置键缺省") {
		t.Fatalf("A.XTask 应含原始任务, got %q", a.XTask)
	}
	if a.Model != "claude-opus-4-8" || a.Effort != "max" {
		t.Fatalf("A 应套引擎甲(opus max): model=%q effort=%q", a.Model, a.Effort)
	}
	if !strings.Contains(a.Prompts[0], "某配置键缺省") || !strings.Contains(a.Prompts[0], "第一性原理") {
		t.Fatalf("A 的 prompt 应由 solo 模板渲染并含任务:\n%s", a.Prompts[0])
	}
	if n := len(listQueued(t, root)); n != 1 {
		t.Fatalf("cmdCross 只应铺 1 张 A 卡, got %d", n)
	}
}

// ---- round-4：身份/冻结/崩溃对账/C 终局时序 ----

// 甲乙身份判定：同 kind+同模型=同引擎(须拒),否则不同。
func TestCrossEngineIdentity(t *testing.T) {
	cfg := testCrossCfg() // CodexModel=codex-model-x
	same := func(a, b CrossEngine) bool { return crossEngineIdentity(a, cfg) == crossEngineIdentity(b, cfg) }
	if !same(CrossEngine{Kind: "codex"}, CrossEngine{Kind: "codex"}) {
		t.Fatal("两 codex(同全局模型)应同身份")
	}
	if same(CrossEngine{Kind: "codex"}, CrossEngine{Kind: "claude", Model: "claude-opus-4-8"}) {
		t.Fatal("codex 与 claude 应不同身份")
	}
	if !same(CrossEngine{Kind: "claude", Model: "opus"}, CrossEngine{Kind: "claude", Model: "opus"}) {
		t.Fatal("同 model 的 claude 应同身份")
	}
	if same(CrossEngine{Kind: "claude", Model: "opus"}, CrossEngine{Kind: "claude", Model: "sonnet"}) {
		t.Fatal("不同 model 的 claude 应不同身份")
	}
}

// cmdCross 拒绝甲乙同引擎的 profile(单引擎自审)。
func TestCmdCrossRejectsSameEngine(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"tasks", "logs"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	cfg := `{"codex_bin":"/usr/bin/codex","codex_model":"m",` +
		`"cross_profiles":{"same":{"a":{"kind":"claude","model":"claude-opus-4-8"},"b":{"kind":"claude","model":"claude-opus-4-8"}}}}`
	os.WriteFile(filepath.Join(root, "config.json"), []byte(cfg), 0o644)
	err := cmdCross([]string{"-root", root, "-profile", "same", "-dir", t.TempDir(), "任务"})
	if err == nil || !strings.Contains(err.Error(), "同一引擎") {
		t.Fatalf("甲乙同引擎应被拒绝, got: %v", err)
	}
}

// blocker 2：入队后篡改 config 不得影响 B 的引擎——B 用冻结规格。
func TestCrossFreezeIgnoresConfigChange(t *testing.T) {
	root := testRoot(t)
	cfg := testCrossCfg()
	a := newTask(root, cfg, typeCrossCheck, "交叉A[opus-codex]: X", "/tmp/proj", []string{"solo"}, 5)
	a.XRole, a.XKey, a.XProfile, a.XTask = "A", "fk", "opus-codex", "t"
	if err := applyCrossEngine(a, cfg.CrossProfiles["opus-codex"].A, cfg); err != nil {
		t.Fatal(err)
	}
	a.XEngineB, _ = freezeCrossEngine(cfg.CrossProfiles["opus-codex"].B, cfg) // 冻结 codex/codex-model-x
	saveTask(root, a)
	// 入队后篡改：清空 codex_model + 把 profile 乙改成 claude(令甲乙同引擎)
	cfg.CodexModel = ""
	cfg.CrossProfiles["opus-codex"] = CrossProfile{
		A: CrossEngine{Kind: "claude", Model: "claude-opus-4-8", Effort: "max"},
		B: CrossEngine{Kind: "claude", Model: "claude-opus-4-8", Effort: "max"},
	}
	postComplete(root, cfg, a, &claudeResult{Result: "甲结论"}, nil)
	b := findByPrefix(t, root, "交叉B[")
	if b == nil {
		t.Fatal("应派 B 卡")
	}
	// B 必须用**冻结**的乙引擎(codex + codex-model-x)，不受 config 篡改影响
	if b.PreferRunner != "codex" || b.XCodexModel != "codex-model-x" {
		t.Fatalf("B 应用冻结引擎(codex/codex-model-x)而非篡改后 config: PreferRunner=%q XCodexModel=%q", b.PreferRunner, b.XCodexModel)
	}
}

// blocker 3：崩溃对账——done 的 A 无后继 B → failed + 清侧车；有 B 或 active 的不动。
func TestReconcileCrossOrphan(t *testing.T) {
	root := testRoot(t)
	cfg := testCrossCfg()
	mk := func(role, key string) *Task {
		x := newTask(root, cfg, typeCrossCheck, "交叉"+role+"[opus-codex]: X", "/tmp", []string{"s"}, 5)
		x.XRole, x.XKey, x.Status = role, key, statusDone
		saveTask(root, x)
		return x
	}
	orphan := mk("A", "k1")
	writeCrossPeer(root, "k1", "甲结论") // 孤儿侧车
	withNext := mk("A", "k2")
	mk("B", "k2") // k2 有后继(tasks/)
	active := mk("A", "k3")
	// k4：后继 B 已完成并**归档**(archive/)——不应被误判孤儿
	archNext := mk("A", "k4")
	if err := archiveTask(root, mk("B", "k4")); err != nil {
		t.Fatal(err)
	}
	bOrphan := mk("B", "k5") // done B 无后继 C → 孤儿

	tasks, _ := loadTasks(root)
	reconcileCrossChains(root, tasks, map[string]bool{active.ID: true})

	if r, _ := loadTask(root, orphan.ID); r.Status != statusFailed {
		t.Fatalf("孤儿 A 应置 failed, got %s", r.Status)
	}
	if _, err := os.Stat(crossPeerPath(root, "k1")); !os.IsNotExist(err) {
		t.Fatal("孤儿链的侧车应被清理")
	}
	if r, _ := loadTask(root, withNext.ID); r.Status != statusDone {
		t.Fatal("有后继 B(tasks/)的 A 不应被动")
	}
	if r, _ := loadTask(root, active.ID); r.Status != statusDone {
		t.Fatal("仍在 active 的 A 应被守卫跳过(避免误判在跑的正常窗口)")
	}
	if r, _ := loadTask(root, archNext.ID); r.Status != statusDone {
		t.Fatal("后继已归档(archive/)的 A 不应被误判孤儿")
	}
	if r, _ := loadTask(root, bOrphan.ID); r.Status != statusFailed {
		t.Fatal("done B 无后继 C 应置 failed")
	}
}

// blocker 4：C 不合规必须**发布前**拦——进度报告不得落盘 + C 置 failed。
func TestCrossCVerdictBeforePublish(t *testing.T) {
	root := testRoot(t)
	os.MkdirAll(progressDir(root), 0o755)
	cfg := testCrossCfg()
	c := newTask(root, cfg, typeCrossCheck, "交叉C汇总[opus-codex]: X", "/tmp", []string{"m"}, 5)
	c.XRole, c.XKey, c.EmitProgress, c.ProgressKey = "C", "ck", true, "ck"
	saveTask(root, c)
	// 预置一份陈旧进度报告(上一轮的),非合规 C 必须把它清掉,别让 progress -show 冒充当前终局。
	if err := saveProgress(root, &ProgressEntry{Key: "ck", Report: map[string]any{"verdict": "旧的合并结论", "confidence": "high"}}); err != nil {
		t.Fatal(err)
	}
	postComplete(root, cfg, c, &claudeResult{Result: "垃圾结论,无 verdict json"}, nil)
	if c.Status != statusFailed {
		t.Fatalf("非合规 C 应 failed, got %s", c.Status)
	}
	if _, err := os.Stat(progressPath(root, "ck")); !os.IsNotExist(err) {
		t.Fatal("非合规 C 应清掉陈旧进度报告、且不发布新的(验证须在发布前)")
	}
}

// -dir 不能是数据根的**父目录**(-dir=$HOME → cwd 含 $HOME/.claudego)。
func TestCmdCrossRejectsParentDir(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, ".claudego")
	for _, d := range []string{"tasks", "logs"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"codex_bin":"/usr/bin/codex","codex_model":"m"}`), 0o644)
	err := cmdCross([]string{"-root", root, "-dir", base, "任务"}) // base 是 root 的父目录
	if err == nil || !strings.Contains(err.Error(), "父目录") {
		t.Fatalf("-dir=数据根父目录应被拒绝, got: %v", err)
	}
}

func TestApplyCrossEngineEffortValidation(t *testing.T) {
	cfg := testCrossCfg()
	if applyCrossEngine(&Task{Prompts: []string{"x"}}, CrossEngine{Kind: "claude", Model: "m", Effort: "bogus"}, cfg) == nil {
		t.Fatal("非法 effort 应报错")
	}
	if err := applyCrossEngine(&Task{Prompts: []string{"x"}}, CrossEngine{Kind: "claude", Model: "m", Effort: "xhigh"}, cfg); err != nil {
		t.Fatalf("合法 effort 不应报错: %v", err)
	}
}

// blocker(r5)：空 effort 的 reasoning 冻结必须冻**执行时真正回落的源**——本机 codex 用全局 codex_reasoning，
// 远端 codex 用该主机的 reasoning（invokeRemoteCodex 的既有契约）。冻错源会正常路径跑错档位。
func TestFreezeReasoningSource(t *testing.T) {
	cfg := &Config{
		CodexBin: "c", CodexModel: "m", CodexReasoning: "global-r",
		RemoteHosts: map[string]RemoteHostConfig{"h": {Reasoning: "host-r"}},
	}
	fc, err := freezeCrossEngine(CrossEngine{Kind: "codex"}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if fc.Effort != "global-r" {
		t.Fatalf("本机 codex 空 effort 应冻结全局 reasoning, got %q", fc.Effort)
	}
	fr, err := freezeCrossEngine(CrossEngine{Kind: "remote-codex", Host: "h"}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if fr.Effort != "host-r" {
		t.Fatalf("远端 codex 空 effort 应冻结 host reasoning(非全局), got %q", fr.Effort)
	}
	fe, _ := freezeCrossEngine(CrossEngine{Kind: "codex", Effort: "max"}, cfg)
	if fe.Effort != "max" {
		t.Fatalf("显式 effort 应优先, got %q", fe.Effort)
	}
}
