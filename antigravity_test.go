package main

import (
	"strings"
	"testing"
)

func TestAntigravityThinkingModelOmitsEffort(t *testing.T) {
	cfg := defaultConfig("")
	cfg.Antigravity = &AntigravityRoute{Enabled: true, Effort: "high"}
	argv := antigravityArgs(cfg, &Task{Type: typeSequence}, "claude-opus-4-6-thinking", "harmless")
	args := "\n" + strings.Join(argv, "\n") + "\n"
	if strings.Contains(args, "\n--effort\n") {
		t.Fatalf("thinking-encoded model must not receive --effort: %v", argv)
	}
	if !strings.Contains(args, "\n--model\nclaude-opus-4-6-thinking\n") {
		t.Fatalf("actual advertised Opus route missing from argv: %v", argv)
	}
}

func TestParseAntigravityJSONFailsClosedWithoutTerminal(t *testing.T) {
	missing := parseAntigravityJSON([]byte(`{}`))
	if missing == nil || !missing.IsError || !missing.ObservationComplete {
		t.Fatalf("valid JSON without terminal must be a complete fail-closed observation: %+v", missing)
	}
	invalid := parseAntigravityJSON([]byte(`not-json`))
	if invalid == nil || !invalid.IsError || invalid.ObservationComplete {
		t.Fatalf("invalid JSON must remain an incomplete fail-closed observation: %+v", invalid)
	}
}

func TestEnqueueEmittedRunnerAntigravity(t *testing.T) {
	root := testRoot(t)
	cfg := defaultConfig("")
	cfg.AntigravityBin = "agy"
	cfg.Antigravity = &AntigravityRoute{Enabled: true, Effort: "high"}
	parent := newTask(root, cfg, typeCoordinate, "父", t.TempDir(), []string{"p"}, 1)
	result := "```json\n{\"tasks\":[{\"title\":\"填充\",\"type\":\"sequence\",\"runner\":\"agy\",\"agy_model\":\"claude-opus-4-6-thinking\",\"prompts\":[\"做事\"]}]}\n```"
	ids, err := enqueueEmitted(root, cfg, parent, result)
	if err != nil || len(ids) != 1 {
		t.Fatalf("emit 应入队 1 张 agy 卡: ids=%v err=%v", ids, err)
	}
	nt, err := loadTask(root, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if nt.PreferRunner != antigravityRunnerName || nt.AgyModel != "claude-opus-4-6-thinking" || !nt.RunnerExplicit {
		t.Fatalf("emit 的 agy runner/model 应随卡: %+v", nt)
	}
}
