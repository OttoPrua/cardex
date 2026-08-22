package main

// 卡级 codex 模型钉定（-codex-model）与按档位降级模型（codex_fallback_*）。
// Fable/Opus 默认走 gpt-5.6-sol，Sonnet/Haiku 默认走 gpt-5.6-luna；低投入 Opus 降档
// 仅在显式配置时启用。本组测试钉死解析优先序与穿线，防路由被静默回退：
//   交叉冻结 XCodexModel > 卡级 CodexModel > 可选低投入 Opus > Opus 降级专用 > 按档位 > 通用降级 > 全局。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveCodexModel(t *testing.T) {
	cfg := &Config{
		CodexModel:                 "global-sol",
		CodexFallbackModel:         "fb-terra",
		CodexFallbackOpusModel:     "gpt-5.6-sol",
		CodexFallbackOpusReasoning: "xhigh",
		CodexOpusSimpleModel:       "gpt-5.6-luna",
		CodexOpusSimpleReasoning:   "max",
		CodexTierModels: map[string]string{
			"fable": "gpt-5.6-sol", "opus": "gpt-5.6-sol",
			"sonnet": "gpt-5.6-luna", "haiku": "gpt-5.6-luna",
		},
		CodexTierReasoning: map[string]string{
			"fable": "max", "opus": "xhigh", "sonnet": "xhigh", "haiku": "medium",
		},
	}
	cases := []struct {
		name string
		task *Task
		want string
	}{
		{"交叉冻结恒最高(引擎身份不可漂)", &Task{XCodexModel: "frozen-x", CodexModel: "pin-terra"}, "frozen-x"},
		{"卡级钉定盖过降级专用与全局", &Task{CodexModel: "pin-terra"}, "pin-terra"},
		{"codex主跑卡不吃降级模型(用全局)", &Task{PreferRunner: "codex"}, "global-sol"},
		{"claude卡降级径用降级专用模型", &Task{}, "fb-terra"},
		{"Opus卡降级径用5.6-sol", &Task{Model: "claude-opus-5"}, "gpt-5.6-sol"},
		{"低投入Opus卡降级径用5.6-luna", &Task{Model: "claude-opus-5", Stakes: stakesLow}, "gpt-5.6-luna"},
		{"Sonnet卡按档位用5.6-luna", &Task{Model: "claude-sonnet-5"}, "gpt-5.6-luna"},
		{"Haiku卡按档位用5.6-luna", &Task{Model: "haiku"}, "gpt-5.6-luna"},
		{"远端卡无降级径(用全局)", &Task{RemoteHost: "qmthost"}, "global-sol"},
		{"codex主跑+卡级钉定同样生效", &Task{PreferRunner: "codex", CodexModel: "pin-luna"}, "pin-luna"},
	}
	t.Run("按档位思考档与显式effort优先", func(t *testing.T) {
		if got := resolveCodexReasoning(cfg, &Task{Model: "opus", Effort: "high"}); got != "xhigh" {
			t.Fatalf("Opus默认应使用 xhigh, got %q", got)
		}
		if got := resolveCodexReasoning(cfg, &Task{Model: "opus", Effort: "high", Stakes: stakesLow}); got != "max" {
			t.Fatalf("低投入 Opus 应使用 max, got %q", got)
		}
		if got := resolveCodexReasoning(cfg, &Task{Model: "opus", Effort: "low", EffortExplicit: true}); got != "low" {
			t.Fatalf("显式 effort 应覆盖 Opus 默认, got %q", got)
		}
		if got := resolveCodexReasoning(cfg, &Task{Model: "sonnet", Effort: "high"}); got != "xhigh" {
			t.Fatalf("Sonnet 默认应使用 xhigh, got %q", got)
		}
		if got := resolveCodexReasoning(cfg, &Task{Model: "haiku", Effort: "high"}); got != "medium" {
			t.Fatalf("Haiku 默认应使用 medium, got %q", got)
		}
		if got := resolveCodexReasoning(cfg, &Task{Model: "opus", Effort: "max", EffortExplicit: true}); got != "max" {
			t.Fatalf("显式最难裁决应允许 Opus max, got %q", got)
		}
	})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveCodexModel(cfg, c.task); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
	t.Run("降级模型未配→claude卡降级回落全局", func(t *testing.T) {
		cfg2 := &Config{CodexModel: "global-sol"}
		if got := resolveCodexModel(cfg2, &Task{}); got != "global-sol" {
			t.Fatalf("got %q, want global-sol", got)
		}
	})
	t.Run("全空→空(不传-m跑codex默认)", func(t *testing.T) {
		if got := resolveCodexModel(&Config{}, &Task{}); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})
}

func TestDefaultTaskTypesReserveFableForExplicitAdjudication(t *testing.T) {
	cfg := defaultConfig("")
	cases := []struct {
		typ           string
		wantTier      string
		wantModel     string
		wantReasoning string
	}{
		{typeAssembly, "opus", "gpt-5.6-sol", "xhigh"},
		{typeCoordinate, "opus", "gpt-5.6-sol", "xhigh"},
		{typeReview, "opus", "gpt-5.6-sol", "xhigh"},
		{typeSequence, "sonnet", "gpt-5.6-luna", "max"},
		{typeProgressPull, "haiku", "gpt-5.6-luna", "xhigh"},
	}
	for _, c := range cases {
		t.Run(c.typ, func(t *testing.T) {
			td, ok := typeDefaultsFor(cfg, c.typ)
			if !ok {
				t.Fatalf("缺少类型默认值 %q", c.typ)
			}
			task := &Task{Type: c.typ, Model: td.Model, Effort: td.Effort, PreferRunner: "codex"}
			if tier := modelTierKeyword(cfg, task.Model); tier != c.wantTier {
				t.Fatalf("来源档位=%q, want %q（Fable 不得成为普通类型默认）", tier, c.wantTier)
			}
			if model := resolveCodexModel(cfg, task); model != c.wantModel {
				t.Fatalf("实际模型=%q, want %q", model, c.wantModel)
			}
			if reasoning := resolveCodexReasoning(cfg, task); reasoning != c.wantReasoning {
				t.Fatalf("实际推理档=%q, want %q", reasoning, c.wantReasoning)
			}
		})
	}
}

func TestDefaultConfigKeepsLowStakesOpusOnSolXHigh(t *testing.T) {
	cfg := defaultConfig("")
	task := &Task{Model: "claude-opus-5", Stakes: stakesLow}
	if got := resolveCodexModel(cfg, task); got != "gpt-5.6-sol" {
		t.Fatalf("默认策略不得把低投入 Opus 降到 Luna, got model=%q", got)
	}
	if got := resolveCodexReasoning(cfg, task); got != "xhigh" {
		t.Fatalf("默认策略的低投入 Opus 仍应使用 xhigh, got reasoning=%q", got)
	}
}

// fakeCodexArgvCapture 返回把 argv 逐行写进 capture 文件的假 codex（exit 0）。
func fakeCodexArgvCapture(t *testing.T, capture string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + capture + "\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeSSHArgvCapture 模拟远端 Codex 已产出终稿，同时把 ssh 收到的远程命令钉下来。
// 这样测试的是 invokeRemoteCodex 的真实穿线，而不只是解析 helper。
func fakeSSHArgvCapture(t *testing.T, capture string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ssh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + capture + "\"\n" +
		"printf '===CARDEX_REMOTE_RESULT===\\nremote done\\n'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// 解析结果必须真的穿线到 codex argv 的 -m——只测 resolve 不测穿线，改坏调用点不会红。
func TestInvokeCodexThreadsResolvedModel(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "argv.txt")
	cfg := defaultConfig("")
	cfg.CodexBin = fakeCodexArgvCapture(t, capture)
	cfg.CodexModel = "global-sol"
	cfg.CodexFallbackModel = "fb-terra"
	cfg.CodexFallbackOpusModel = "gpt-5.6-sol"
	cfg.CodexFallbackOpusReasoning = "xhigh"

	readCapture := func() string {
		data, err := os.ReadFile(capture)
		if err != nil {
			t.Fatalf("argv 未捕获: %v", err)
		}
		return string(data)
	}

	// ① claude 卡降级径（PreferRunner 空）→ -m 降级专用模型
	tk := &Task{ID: "cm-fb", Dir: t.TempDir(), Type: typeSequence}
	root := admitDirectInvoke(t, "", tk)
	invokeCodex(context.Background(), root, cfg, tk, "ping")
	if got := readCapture(); !strings.Contains(got, "fb-terra") || strings.Contains(got, "global-sol") {
		t.Fatalf("降级径应 -m fb-terra 且不带全局模型, argv:\n%s", got)
	}

	// ①b Opus 卡降级径 → 专用 gpt-5.6-sol + xhigh；类型默认 high 不应压过档位默认。
	tkOpus := &Task{ID: "cm-opus", Dir: t.TempDir(), Type: typeSequence, Model: "opus", Effort: "high"}
	admitDirectInvoke(t, root, tkOpus)
	invokeCodex(context.Background(), root, cfg, tkOpus, "ping")
	if got := readCapture(); !strings.Contains(got, "gpt-5.6-sol") || !strings.Contains(got, "model_reasoning_effort=xhigh") {
		t.Fatalf("Opus降级应使用 gpt-5.6-sol + xhigh, argv:\n%s", got)
	}

	// ①c 默认关闭低投入旁路：stakes=low 的 Opus 仍是 gpt-5.6-sol + xhigh。
	tkSimple := &Task{ID: "cm-opus-simple", Dir: t.TempDir(), Type: typeSequence, Model: "opus", Stakes: stakesLow, Effort: "high"}
	admitDirectInvoke(t, root, tkSimple)
	invokeCodex(context.Background(), root, cfg, tkSimple, "ping")
	if got := readCapture(); !strings.Contains(got, "gpt-5.6-sol") || !strings.Contains(got, "model_reasoning_effort=xhigh") {
		t.Fatalf("默认低投入 Opus 仍应使用 gpt-5.6-sol + xhigh, argv:\n%s", got)
	}

	// ①d Sonnet 主跑 Codex → gpt-5.6-luna + max。
	tkSonnet := &Task{ID: "cm-sonnet", Dir: t.TempDir(), Type: typeSequence, Model: "sonnet", PreferRunner: "codex", Effort: "high"}
	admitDirectInvoke(t, root, tkSonnet)
	invokeCodex(context.Background(), root, cfg, tkSonnet, "ping")
	if got := readCapture(); !strings.Contains(got, "gpt-5.6-luna") || !strings.Contains(got, "model_reasoning_effort=max") {
		t.Fatalf("Sonnet Codex主跑应使用 gpt-5.6-luna + max, argv:\n%s", got)
	}

	// ② 卡级钉定盖过降级专用
	tk2 := &Task{ID: "cm-pin", Dir: t.TempDir(), Type: typeSequence, CodexModel: "pin-luna"}
	admitDirectInvoke(t, root, tk2)
	invokeCodex(context.Background(), root, cfg, tk2, "ping")
	if got := readCapture(); !strings.Contains(got, "pin-luna") || strings.Contains(got, "fb-terra") {
		t.Fatalf("卡级钉定应 -m pin-luna, argv:\n%s", got)
	}

	// ③ codex 主跑卡（PreferRunner=codex）不吃降级模型 → 全局
	tk3 := &Task{ID: "cm-main", Dir: t.TempDir(), Type: typeSequence, PreferRunner: "codex"}
	admitDirectInvoke(t, root, tk3)
	invokeCodex(context.Background(), root, cfg, tk3, "ping")
	if got := readCapture(); !strings.Contains(got, "global-sol") || strings.Contains(got, "fb-terra") {
		t.Fatalf("codex 主跑应 -m global-sol, argv:\n%s", got)
	}
}

func TestInvokeRemoteCodexThreadsResolvedReasoning(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "ssh-argv.txt")
	cfg := defaultConfig("")
	cfg.SSHBin = fakeSSHArgvCapture(t, capture)
	cfg.StepTimeoutMin = 1
	cfg.CodexReasoning = "high"
	cfg.RemoteHosts = map[string]RemoteHostConfig{
		"qmthost": {Shell: "cmd", Reasoning: "medium", CodexOnly: true},
	}

	readCapture := func() string {
		data, err := os.ReadFile(capture)
		if err != nil {
			t.Fatalf("ssh argv 未捕获: %v", err)
		}
		return string(data)
	}

	opus := &Task{
		ID: "remote-opus", Dir: "D:/work/repo", Type: typeSequence,
		Model: "claude-opus-5", Effort: "high", RemoteHost: "qmthost", PreferRunner: "codex",
	}
	adjudication := *opus
	adjudication.ID = "remote-adjudication"
	adjudication.Effort = "max"
	adjudication.EffortExplicit = true
	root := admitDirectInvoke(t, "", opus)
	res, _, err := invokeRemoteCodex(context.Background(), cfg, opus, "ping")
	if err != nil || res == nil || res.Result != "remote done" {
		t.Fatalf("fake 远端 Codex 应成功: res=%+v err=%v", res, err)
	}
	if got := readCapture(); !strings.Contains(got, "-m gpt-5.6-sol") ||
		!strings.Contains(got, "model_reasoning_effort=xhigh") ||
		strings.Contains(got, "model_reasoning_effort=high") {
		t.Fatalf("远端 Opus 必须实际穿线 Sol/xhigh, ssh argv:\n%s", got)
	}

	admitDirectInvoke(t, root, &adjudication)
	if _, _, err := invokeRemoteCodex(context.Background(), cfg, &adjudication, "ping"); err != nil {
		t.Fatalf("显式终裁远端执行失败: %v", err)
	}
	if got := readCapture(); !strings.Contains(got, "model_reasoning_effort=max") {
		t.Fatalf("显式最难裁决必须保留 max, ssh argv:\n%s", got)
	}

	plain := &Task{ID: "remote-plain", Dir: "D:/work/repo", Type: typeSequence, RemoteHost: "qmthost", PreferRunner: "codex"}
	if got := resolveRemoteCodexReasoning(cfg, plain); got != "medium" {
		t.Fatalf("无来源档位/卡面 effort 时应保留远端主机兜底, got %q", got)
	}
}

// add 的 -codex-model 旗标：钉进任务 JSON 落盘；-model 与 -runner codex 互斥报错防误导。
func TestAddCodexModelFlag(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"codex_bin":"/usr/bin/true"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()

	loadOnly := func() *Task {
		t.Helper()
		entries, err := os.ReadDir(filepath.Join(root, "tasks"))
		if err != nil || len(entries) != 1 {
			t.Fatalf("应恰有 1 张任务 JSON, got %d, err=%v", len(entries), err)
		}
		data, err := os.ReadFile(filepath.Join(root, "tasks", entries[0].Name()))
		if err != nil {
			t.Fatal(err)
		}
		var tk Task
		if err := json.Unmarshal(data, &tk); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(root, "tasks", entries[0].Name())); err != nil {
			t.Fatal(err)
		}
		return &tk
	}

	t.Run("codex主跑卡钉模型", func(t *testing.T) {
		if err := cmdAdd([]string{"-root", root, "-dir", work,
			"-runner", "codex", "-codex-model", "gpt-5.6-terra", "跑个任务"}); err != nil {
			t.Fatal(err)
		}
		tk := loadOnly()
		if tk.PreferRunner != "codex" || tk.CodexModel != "gpt-5.6-terra" {
			t.Fatalf("runner_pref=%q codex_model=%q", tk.PreferRunner, tk.CodexModel)
		}
	})

	t.Run("claude卡可带降级钉定(不配runner)", func(t *testing.T) {
		if err := cmdAdd([]string{"-root", root, "-dir", work,
			"-model", "opus", "-codex-model", "gpt-5.6-terra", "跑个任务"}); err != nil {
			t.Fatal(err)
		}
		tk := loadOnly()
		if tk.Model != "opus" || tk.CodexModel != "gpt-5.6-terra" || tk.PreferRunner != "" {
			t.Fatalf("model=%q codex_model=%q runner_pref=%q", tk.Model, tk.CodexModel, tk.PreferRunner)
		}
	})

	t.Run("-model与-runner codex互斥报错", func(t *testing.T) {
		err := cmdAdd([]string{"-root", root, "-dir", work,
			"-runner", "codex", "-model", "opus", "跑个任务"})
		if err == nil || !strings.Contains(err.Error(), "-codex-model") {
			t.Fatalf("应报错并指引 -codex-model, got: %v", err)
		}
	})
}

func TestAddRejectsExplicitTierModelConflictUnderCodexDefault(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"tasks", "events", "archive", "logs", "templates"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := defaultConfig("/usr/bin/true")
	cfg.CodexBin = "/usr/bin/true"
	cfg.DefaultRunner = "codex"
	if err := saveConfig(root, cfg); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()

	err := cmdAdd([]string{"-root", root, "-dir", work, "-model", "sonnet",
		"-codex-model", "gpt-5.6-sol", "旧策略命令"})
	if err == nil || !strings.Contains(err.Error(), "Codex 路由冲突") || !strings.Contains(err.Error(), "gpt-5.6-luna") {
		t.Fatalf("Sonnet+Sol 显式冲突必须被拒并提示 Luna, got %v", err)
	}
	if tasks, loadErr := loadTasks(root); loadErr != nil || len(tasks) != 0 {
		t.Fatalf("冲突卡不得落盘: tasks=%d err=%v", len(tasks), loadErr)
	}

	if err := cmdAdd([]string{"-root", root, "-dir", work, "-model", "sonnet",
		"-codex-model", "gpt-5.6-luna", "新策略命令"}); err != nil {
		t.Fatalf("Sonnet+Luna 一致路由应允许: %v", err)
	}
}

// emit 契约：协调器发的 codex 卡可带 codex_model（档位对等制按档发 terra/luna）。
func TestEmitTaskCodexModelDecodes(t *testing.T) {
	raw := `{"tasks":[{"title":"填充","runner":"codex","codex_model":"gpt-5.6-luna","prompt":"p"}]}`
	ts := parseEmitTasks(raw)
	if len(ts) != 1 {
		t.Fatalf("应解析出 1 张, got %d", len(ts))
	}
	if ts[0].CodexModel != "gpt-5.6-luna" {
		t.Fatalf("codex_model=%q", ts[0].CodexModel)
	}
}

func TestEnqueueEmittedRejectsExplicitRouteConflict(t *testing.T) {
	root := testRoot(t)
	cfg := codexPrimaryTestConfig()
	parent := &Task{ID: "parent", Dir: t.TempDir(), Project: "p"}
	bad := `{"tasks":[{"title":"旧策略子卡","runner":"codex","model":"sonnet","codex_model":"gpt-5.6-sol","effort":"xhigh","prompt":"p"}]}`
	if _, err := enqueueEmitted(root, cfg, parent, bad); err == nil || !strings.Contains(err.Error(), "Codex 路由冲突") {
		t.Fatalf("emit 的 Sonnet+Sol 冲突必须拒绝, got %v", err)
	}
	if tasks, err := loadTasks(root); err != nil || len(tasks) != 0 {
		t.Fatalf("冲突 emit 子卡不得落盘: tasks=%d err=%v", len(tasks), err)
	}

	good := `{"tasks":[{"title":"新策略子卡","runner":"codex","model":"sonnet","codex_model":"gpt-5.6-luna","effort":"max","prompt":"p"}]}`
	ids, err := enqueueEmitted(root, cfg, parent, good)
	if err != nil || len(ids) != 1 {
		t.Fatalf("emit 的 Sonnet+Luna 一致路由应允许: ids=%v err=%v", ids, err)
	}
}

func TestDispatchEventDetailRecordsResolvedCodexRoute(t *testing.T) {
	cfg := codexPrimaryTestConfig()
	task := &Task{
		Runner: "codex", PreferRunner: "codex", Model: "sonnet", Effort: "max",
	}
	detail := dispatchEventDetail(cfg, task, true, false)
	if got := detail["codex_model"]; got != "gpt-5.6-luna" {
		t.Fatalf("派发账本应记录实际模型, got %v", got)
	}
	if got := detail["codex_reasoning"]; got != "max" {
		t.Fatalf("派发账本应记录实际推理档, got %v", got)
	}
	if got := detail["source_tier"]; got != "sonnet" {
		t.Fatalf("派发账本应保留来源档位, got %v", got)
	}

	cfg.RemoteHosts = map[string]RemoteHostConfig{
		"qmthost": {Reasoning: "medium", CodexOnly: true},
	}
	remoteOpus := &Task{
		Runner: "remote:qmthost", PreferRunner: "codex", RemoteHost: "qmthost",
		Model: "opus", Effort: "high",
	}
	remoteDetail := dispatchEventDetail(cfg, remoteOpus, false, true)
	if got := remoteDetail["codex_reasoning"]; got != "xhigh" {
		t.Fatalf("远端派发账本必须与实际 Opus/xhigh 参数一致, got %v", got)
	}

	claudeDetail := dispatchEventDetail(cfg, &Task{Model: "opus"}, false, false)
	if _, ok := claudeDetail["codex_model"]; ok {
		t.Fatalf("非 Codex 路由不得伪造 codex_model: %+v", claudeDetail)
	}
}

func TestEffectiveEffortShowsResolvedCodexReasoning(t *testing.T) {
	cfg := codexPrimaryTestConfig()

	opus := &Task{Model: "claude-opus-5", Effort: "high", PreferRunner: "codex"}
	if got, source := effectiveEffort(cfg, opus); got != "xhigh" || source != "codex_reasoning" {
		t.Fatalf("Opus 看板应显示实际 Sol/xhigh 推理档, got (%q,%q)", got, source)
	}

	sonnet := &Task{Model: "sonnet", Effort: "xhigh", PreferRunner: "codex"}
	if got, _ := effectiveEffort(cfg, sonnet); got != "max" {
		t.Fatalf("Sonnet 看板应显示 Luna/max, got %q", got)
	}

	adjudication := &Task{Model: "opus", Effort: "max", EffortExplicit: true, PreferRunner: "codex"}
	if got, _ := effectiveEffort(cfg, adjudication); got != "max" {
		t.Fatalf("显式最难裁决应显示 max, got %q", got)
	}

	cfg.RemoteHosts = map[string]RemoteHostConfig{
		"qmthost": {Reasoning: "medium", CodexOnly: true},
	}
	remoteOpus := &Task{
		Runner: "remote:qmthost", PreferRunner: "codex", RemoteHost: "qmthost",
		Model: "opus", Effort: "high",
	}
	if got, _ := effectiveEffort(cfg, remoteOpus); got != "xhigh" {
		t.Fatalf("远端 Opus 看板应与实际参数一致显示 xhigh, got %q", got)
	}
}
