package main

// 卡级 codex 模型钉定（-codex-model）与降级专用模型（codex_fallback_model）。
// 档位对等制（2026-07-17）：sol≈fable（设计档）、terra≈opus（实现档）、luna≈sonnet（机械档）——
// opus 卡降级应走同档 terra 而非设计档 sol。本组测试钉死解析优先序与穿线，防路由被静默回退：
//   交叉冻结 XCodexModel > 卡级 CodexModel > 降级径 codex_fallback_model > 全局 codex_model。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveCodexModel(t *testing.T) {
	cfg := &Config{CodexModel: "global-sol", CodexFallbackModel: "fb-terra"}
	cases := []struct {
		name string
		task *Task
		want string
	}{
		{"交叉冻结恒最高(引擎身份不可漂)", &Task{XCodexModel: "frozen-x", CodexModel: "pin-terra"}, "frozen-x"},
		{"卡级钉定盖过降级专用与全局", &Task{CodexModel: "pin-terra"}, "pin-terra"},
		{"codex主跑卡不吃降级模型(用全局)", &Task{PreferRunner: "codex"}, "global-sol"},
		{"claude卡降级径用降级专用模型", &Task{}, "fb-terra"},
		{"远端卡无降级径(用全局)", &Task{RemoteHost: "qmthost"}, "global-sol"},
		{"codex主跑+卡级钉定同样生效", &Task{PreferRunner: "codex", CodexModel: "pin-luna"}, "pin-luna"},
	}
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

// 解析结果必须真的穿线到 codex argv 的 -m——只测 resolve 不测穿线，改坏调用点不会红。
func TestInvokeCodexThreadsResolvedModel(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "argv.txt")
	cfg := defaultConfig("")
	cfg.CodexBin = fakeCodexArgvCapture(t, capture)
	cfg.CodexModel = "global-sol"
	cfg.CodexFallbackModel = "fb-terra"

	readCapture := func() string {
		data, err := os.ReadFile(capture)
		if err != nil {
			t.Fatalf("argv 未捕获: %v", err)
		}
		return string(data)
	}

	// ① claude 卡降级径（PreferRunner 空）→ -m 降级专用模型
	tk := &Task{ID: "cm-fb", Dir: t.TempDir(), Type: typeSequence}
	invokeCodex(context.Background(), t.TempDir(), cfg, tk, "ping")
	if got := readCapture(); !strings.Contains(got, "fb-terra") || strings.Contains(got, "global-sol") {
		t.Fatalf("降级径应 -m fb-terra 且不带全局模型, argv:\n%s", got)
	}

	// ② 卡级钉定盖过降级专用
	tk2 := &Task{ID: "cm-pin", Dir: t.TempDir(), Type: typeSequence, CodexModel: "pin-luna"}
	invokeCodex(context.Background(), t.TempDir(), cfg, tk2, "ping")
	if got := readCapture(); !strings.Contains(got, "pin-luna") || strings.Contains(got, "fb-terra") {
		t.Fatalf("卡级钉定应 -m pin-luna, argv:\n%s", got)
	}

	// ③ codex 主跑卡（PreferRunner=codex）不吃降级模型 → 全局
	tk3 := &Task{ID: "cm-main", Dir: t.TempDir(), Type: typeSequence, PreferRunner: "codex"}
	invokeCodex(context.Background(), t.TempDir(), cfg, tk3, "ping")
	if got := readCapture(); !strings.Contains(got, "global-sol") || strings.Contains(got, "fb-terra") {
		t.Fatalf("codex 主跑应 -m global-sol, argv:\n%s", got)
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
