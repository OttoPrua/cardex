package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeRetroFactTask(t *testing.T, root string, task *Task, archived bool, events ...TaskEvent) {
	t.Helper()
	if task.Prompts == nil {
		task.Prompts = []string{"fixture"}
	}
	if err := saveTask(root, task); err != nil {
		t.Fatal(err)
	}
	if len(events) > 0 {
		if err := os.MkdirAll(eventsDir(root), 0o755); err != nil {
			t.Fatal(err)
		}
		var lines []byte
		for _, ev := range events {
			data, err := json.Marshal(ev)
			if err != nil {
				t.Fatal(err)
			}
			lines = append(lines, data...)
			lines = append(lines, '\n')
		}
		if err := os.WriteFile(eventsPath(root, task.ID), lines, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if archived {
		if err := archiveTask(root, task); err != nil {
			t.Fatal(err)
		}
	}
}

func factEvent(seq int64, ts, typ string, detail map[string]any) TaskEvent {
	return TaskEvent{Seq: seq, TS: ts, Type: typ, Status: typ, Detail: detail}
}

// TestBuildRetroFactsSelectsDoneEvidence pins the production defect observed in retro-697:
// a newer canceled archive file must never displace a genuinely completed task from a "done" retrospective.
func TestBuildRetroFactsSelectsDoneEvidence(t *testing.T) {
	root := retroTestRoot(t)
	base := func(id, typ, status, updated string) *Task {
		return &Task{ID: id, Title: id, Type: typ, Status: status, CreatedAt: updated, UpdatedAt: updated}
	}

	a := base("done-a", typeSequence, statusDone, "2026-08-13T10:00:00+08:00")
	a.Model, a.Runner, a.Stakes, a.CostUSD, a.TurnsUsed = "sonnet", "codex", "normal", 1.25, 4
	a.Project, a.Dir, a.Effort = "Cardex", "/work/cardex", "high"
	writeRetroFactTask(t, root, a, true,
		factEvent(1, "2026-08-13T09:50:00+08:00", evRetry, map[string]any{"reason": "compile_error"}),
		factEvent(2, "2026-08-13T10:00:00+08:00", evDone, map[string]any{"cost_total": 1.25, "turns_total": 4}),
	)

	b := base("done-b", typeReview, statusDone, "2026-08-13T11:00:00+08:00")
	b.Model, b.Runner, b.FixRound = "opus", "remote:review", 2
	writeRetroFactTask(t, root, b, false,
		factEvent(1, "2026-08-13T11:00:00+08:00", evDone, map[string]any{"cost_unavailable": true, "cost_unavailable_reason": "no_usage_recorded"}),
		factEvent(2, "2026-08-13T11:00:01+08:00", evCloseout, map[string]any{"verdict": "block"}),
	)

	canceled := base("newer-canceled", typeSequence, statusCanceled, "2026-08-13T12:00:00+08:00")
	writeRetroFactTask(t, root, canceled, true,
		factEvent(1, "2026-08-13T12:00:00+08:00", evCanceled, nil),
	)

	self := base("retro-self", typeProgressPull, statusDone, "2026-08-13T13:00:00+08:00")
	self.ProgressKey = retroProgressKeyPrefix + "2"
	writeRetroFactTask(t, root, self, true,
		factEvent(1, "2026-08-13T13:00:00+08:00", evDone, map[string]any{"cost_unavailable": true}),
	)
	legacy := base("old-unselected-legacy", typeSequence, statusDone, "2026-01-01T00:00:00+08:00")
	writeRetroFactTask(t, root, legacy, true)

	// Make archive mtimes actively misleading. Selection must remain based on done evidence.
	future := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(archiveDir(root), canceled.ID+".json"), future, future); err != nil {
		t.Fatal(err)
	}

	facts, err := buildRetroFacts(root, 2, 42)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := facts.Window.TaskIDs, []string{"done-b", "done-a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("done cohort = %v, want %v", got, want)
	}
	if facts.Window.SelectedCards != 2 || facts.Window.Watermark != 42 {
		t.Fatalf("unexpected window: %+v", facts.Window)
	}
	if facts.Cost.AvailableCards != 1 || facts.Cost.UnavailableCards != 1 || facts.Cost.TotalUSD == nil || *facts.Cost.TotalUSD != 1.25 {
		t.Fatalf("unexpected cost facts: %+v", facts.Cost)
	}
	if facts.FailureClasses["retry:compile_error"] != 1 {
		t.Fatalf("failure classes = %#v", facts.FailureClasses)
	}
	if facts.ReviewVerdicts["block"] != 1 {
		t.Fatalf("review verdicts = %#v", facts.ReviewVerdicts)
	}
	if facts.FixRounds["2"] != 1 || facts.FixRounds["0"] != 1 {
		t.Fatalf("fix rounds = %#v", facts.FixRounds)
	}
	if facts.Coverage.DoneEventCards != 2 || facts.Coverage.CostCards != 1 {
		t.Fatalf("coverage = %+v", facts.Coverage)
	}
	if facts.Cards[1].Title != "done-a" || facts.Cards[1].Project != "Cardex" || facts.Cards[1].Dir != "/work/cardex" || facts.Counts.ByEffort["high"] != 1 {
		t.Fatalf("card context missing: %+v", facts.Cards[1])
	}
	for _, gap := range facts.Gaps {
		if gap.TaskID == legacy.ID {
			t.Fatalf("unselected legacy task leaked into cohort gaps: %+v", gap)
		}
	}
}

func TestMarshalRetroFactsStable(t *testing.T) {
	root := retroTestRoot(t)
	task := &Task{ID: "stable", Title: "stable", Type: typeSequence, Status: statusDone,
		CreatedAt: "2026-08-13T10:00:00+08:00", UpdatedAt: "2026-08-13T10:00:00+08:00"}
	writeRetroFactTask(t, root, task, true,
		factEvent(1, "2026-08-13T10:00:00+08:00", evDone, map[string]any{"cost_total": 0.5, "turns_total": 2}),
	)
	facts, err := buildRetroFacts(root, 1, 7)
	if err != nil {
		t.Fatal(err)
	}
	a, hashA, err := marshalRetroFacts(facts)
	if err != nil {
		t.Fatal(err)
	}
	b, hashB, err := marshalRetroFacts(facts)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) || hashA != hashB {
		t.Fatalf("facts output is not stable:\n%s\n%s\n%s != %s", a, b, hashA, hashB)
	}
	if !strings.Contains(string(a), `"task_ids": [`) || len(hashA) != 64 {
		t.Fatalf("missing explicit cohort/hash: hash=%q json=%s", hashA, a)
	}
}

func TestWriteRetroFactsEnvelope(t *testing.T) {
	root := retroTestRoot(t)
	task := &Task{ID: "cli-done", Title: "cli-done", Type: typeSequence, Status: statusDone,
		CreatedAt: "2026-08-13T10:00:00+08:00", UpdatedAt: "2026-08-13T10:00:00+08:00"}
	writeRetroFactTask(t, root, task, true,
		factEvent(1, "2026-08-13T10:00:00+08:00", evDone, map[string]any{"cost_total": 0.25, "turns_total": 1}),
	)
	var out bytes.Buffer
	if err := writeRetroFacts(&out, root, 1, 9); err != nil {
		t.Fatal(err)
	}
	var envelope RetroFactsEnvelope
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("invalid CLI JSON: %v\n%s", err, out.String())
	}
	if len(envelope.FactsSHA256) != 64 || envelope.Facts == nil || envelope.Facts.Window.TaskIDs[0] != "cli-done" {
		t.Fatalf("unexpected envelope: %+v", envelope)
	}
}

func TestQueueRetroTaskFreezesFactsInPrompt(t *testing.T) {
	root := retroTestRoot(t)
	cfg := retroTestConfig(1)
	task := &Task{ID: "frozen-done", Title: "frozen", Type: typeSequence, Status: statusDone,
		CreatedAt: "2026-08-13T10:00:00+08:00", UpdatedAt: "2026-08-13T10:00:00+08:00"}
	writeRetroFactTask(t, root, task, true,
		factEvent(1, "2026-08-13T10:00:00+08:00", evDone, map[string]any{"cost_total": 0.75, "turns_total": 3}),
	)
	id, err := queueRetroTask(root, cfg, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	card, err := loadTask(root, id)
	if err != nil {
		t.Fatal(err)
	}
	prompt := card.Prompts[0]
	for _, want := range []string{retroFactsSchema, "frozen-done", `"facts_sha256"`, `"task_ids"`} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("frozen prompt missing %q:\n%s", want, prompt)
		}
	}
	for _, placeholder := range []string{"{{FACTS_JSON}}", "{{FACTS_SHA256}}", "{{N}}"} {
		if strings.Contains(prompt, placeholder) {
			t.Fatalf("frozen prompt retained placeholder %s", placeholder)
		}
	}
}

func TestBuildRetroFactsDisclosesMissingReviewVerdict(t *testing.T) {
	root := retroTestRoot(t)
	review := &Task{ID: "review-no-verdict", Title: "manual review", Type: typeReview, Status: statusDone,
		CreatedAt: "2026-08-13T10:00:00+08:00", UpdatedAt: "2026-08-13T10:00:00+08:00"}
	writeRetroFactTask(t, root, review, true,
		factEvent(1, "2026-08-13T10:00:00+08:00", evDone, map[string]any{"cost_total": 0.1, "turns_total": 1}),
	)
	facts, err := buildRetroFacts(root, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Coverage.ReviewCards != 1 || facts.Coverage.ReviewVerdictCards != 0 {
		t.Fatalf("review coverage = %+v", facts.Coverage)
	}
	found := false
	for _, gap := range facts.Gaps {
		if gap.Code == "review_verdict_missing" && gap.TaskID == review.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing structured review verdict was not disclosed: %+v", facts.Gaps)
	}
}

// The runner emits done and calls noteTaskDone before its final saveTask. The triggering card must
// still be frozen into the cohort from the in-memory terminal task, not displaced by an older card.
func TestRetroTriggerIncludesCompletingCardBeforeFinalSave(t *testing.T) {
	root := retroTestRoot(t)
	cfg := retroTestConfig(1)
	onDisk := &Task{ID: "triggering", Title: "triggering", Type: typeSequence, Status: statusRunning,
		CreatedAt: "2026-08-13T10:00:00+08:00", UpdatedAt: "2026-08-13T10:00:00+08:00", Prompts: []string{"p"}}
	if err := saveTask(root, onDisk); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(eventsDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	event := factEvent(1, "2026-08-13T10:01:00+08:00", evDone, map[string]any{"cost_total": 0.2, "turns_total": 1})
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(eventsPath(root, onDisk.ID), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	completing := *onDisk
	completing.Status = statusDone
	completing.UpdatedAt = "2026-08-13T10:01:00+08:00"
	id, err := noteTaskDone(root, cfg, &completing)
	if err != nil || id == "" {
		t.Fatalf("retro trigger: id=%q err=%v", id, err)
	}
	card, err := loadTask(root, id)
	if err != nil {
		t.Fatal(err)
	}
	prompt := card.Prompts[0]
	start := strings.Index(prompt, `"task_ids": [`)
	end := -1
	if start >= 0 {
		if relative := strings.Index(prompt[start:], "]"); relative >= 0 {
			end = start + relative
		}
	}
	if start < 0 || end < 0 || !strings.Contains(prompt[start:end], `"triggering"`) {
		t.Fatalf("triggering card missing from frozen cohort before final save:\n%s", card.Prompts[0])
	}
}

func TestRetroReportValidationRejectsDrift(t *testing.T) {
	root := retroTestRoot(t)
	cfg := retroTestConfig(1)
	done := &Task{ID: "report-source", Title: "source", Type: typeSequence, Status: statusDone,
		CreatedAt: "2026-08-13T10:00:00+08:00", UpdatedAt: "2026-08-13T10:00:00+08:00"}
	writeRetroFactTask(t, root, done, true,
		factEvent(1, "2026-08-13T10:00:00+08:00", evDone, map[string]any{"cost_total": 0.1, "turns_total": 1}),
	)
	id, err := queueRetroTask(root, cfg, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	card, err := loadTask(root, id)
	if err != nil {
		t.Fatal(err)
	}
	if card.RetroFactsSHA256 == "" || !reflect.DeepEqual(card.RetroCohortTaskIDs, []string{"report-source"}) {
		t.Fatalf("retro task missing frozen report contract: %+v", card)
	}

	valid := map[string]any{
		"schema_version":  retroReportSchema,
		"facts_sha256":    card.RetroFactsSHA256,
		"cohort_task_ids": []string{"report-source"},
		"conclusions": []any{map[string]any{
			"finding": "one supported finding", "evidence_task_ids": []string{"report-source"}, "confidence": "high",
		}},
		"recommendations": []any{map[string]any{
			"target": "retro template", "change": "keep facts frozen", "evidence_task_ids": []string{"report-source"},
			"expected_effect": "less drift", "validation": "next cohort keeps the same hash",
		}},
		"deferred_edges": []any{},
	}
	encode := func(report map[string]any) string {
		data, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		return "```json\n" + string(data) + "\n```"
	}
	if _, err := saveProgressFromResult(root, card, encode(valid)); err != nil {
		t.Fatalf("valid retrospective report rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"wrong facts hash", func(report map[string]any) { report["facts_sha256"] = strings.Repeat("0", 64) }},
		{"wrong cohort", func(report map[string]any) { report["cohort_task_ids"] = []string{"foreign"} }},
		{"foreign evidence", func(report map[string]any) {
			report["conclusions"] = []any{map[string]any{"finding": "x", "evidence_task_ids": []string{"foreign"}, "confidence": "high"}}
		}},
		{"too many recommendations", func(report map[string]any) {
			item := map[string]any{"target": "x", "change": "x", "evidence_task_ids": []string{"report-source"}, "expected_effect": "x", "validation": "x"}
			report["recommendations"] = []any{item, item, item, item}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, _ := json.Marshal(valid)
			var report map[string]any
			_ = json.Unmarshal(data, &report)
			tc.mutate(report)
			if _, err := saveProgressFromResult(root, card, encode(report)); err == nil {
				t.Fatal("drifted retrospective report was accepted")
			}
		})
	}
}

func TestPostCompleteInvalidRetroReportFailsClosed(t *testing.T) {
	root := retroTestRoot(t)
	cfg := retroTestConfig(1)
	done := &Task{ID: "post-source", Title: "source", Type: typeSequence, Status: statusDone,
		CreatedAt: "2026-08-13T10:00:00+08:00", UpdatedAt: "2026-08-13T10:00:00+08:00"}
	writeRetroFactTask(t, root, done, true,
		factEvent(1, "2026-08-13T10:00:00+08:00", evDone, map[string]any{"cost_total": 0.1, "turns_total": 1}),
	)
	id, err := queueRetroTask(root, cfg, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	card, err := loadTask(root, id)
	if err != nil {
		t.Fatal(err)
	}
	card.Status = statusDone
	emitTaskEvent(root, card.ID, evDone, "runner", statusDone, 1, withCostTelemetry(nil, card))
	lg, err := os.CreateTemp(t.TempDir(), "retro-log-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	postComplete(root, cfg, card, &claudeResult{Result: "```json\n{\"schema_version\":\"wrong\"}\n```"}, lg)
	if card.Status != statusFailed || !strings.Contains(card.LastError, "复盘报告") {
		t.Fatalf("invalid retro report must fail closed: status=%s err=%q", card.Status, card.LastError)
	}
	events, _, err := loadTaskEvents(root, card.ID)
	if err != nil || len(events) == 0 || events[len(events)-1].Type != evFailed {
		t.Fatalf("invalid report missing failed terminal event: events=%v err=%v", events, err)
	}
}

func TestLegacyRetroReportRemainsCompatible(t *testing.T) {
	root := retroTestRoot(t)
	legacy := &Task{ID: "legacy-retro", Title: "legacy", Type: typeProgressPull, Status: statusDone,
		ProgressKey: retroProgressKeyPrefix + "old", EmitProgress: true}
	if _, err := saveProgressFromResult(root, legacy, "```json\n{\"window\":{\"cards\":1},\"recommendations\":[]}\n```"); err != nil {
		t.Fatalf("legacy retrospective without frozen metadata must remain readable/retryable: %v", err)
	}
}

func TestQueueRetroTaskFallsBackFromLegacyLocalTemplate(t *testing.T) {
	root := retroTestRoot(t)
	cfg := retroTestConfig(1)
	if err := os.MkdirAll(templatesDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := "LEGACY_SENTINEL {{ARCHIVE_DIR}}"
	path := filepath.Join(templatesDir(root), retroTemplate+".md")
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	done := &Task{ID: "template-source", Title: "source", Type: typeSequence, Status: statusDone,
		CreatedAt: "2026-08-13T10:00:00+08:00", UpdatedAt: "2026-08-13T10:00:00+08:00"}
	writeRetroFactTask(t, root, done, true,
		factEvent(1, "2026-08-13T10:00:00+08:00", evDone, map[string]any{"cost_total": 0.1, "turns_total": 1}),
	)
	id, err := queueRetroTask(root, cfg, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	card, err := loadTask(root, id)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(card.Prompts[0], "LEGACY_SENTINEL") || !strings.Contains(card.Prompts[0], retroReportSchema) {
		t.Fatalf("legacy local template did not fall back to embedded v2:\n%s", card.Prompts[0])
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || string(unchanged) != legacy {
		t.Fatalf("fallback must not rewrite user template: data=%q err=%v", unchanged, err)
	}
	events, _, err := loadTaskEvents(root, id)
	if err != nil || len(events) == 0 || retroDetailString(events[0].Detail, "template_source") != "embedded_v2" {
		t.Fatalf("template fallback source not disclosed: events=%v err=%v", events, err)
	}
}

func TestQueueRetroTaskKeepsCompatibleLocalTemplate(t *testing.T) {
	root := retroTestRoot(t)
	cfg := retroTestConfig(1)
	if err := os.MkdirAll(templatesDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	custom := `CUSTOM_V2 {{FACTS_JSON}} {{FACTS_SHA256}} cardex.retrospective_report.v2 "cohort_task_ids" "evidence_task_ids"`
	if err := os.WriteFile(filepath.Join(templatesDir(root), retroTemplate+".md"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	done := &Task{ID: "custom-template-source", Title: "source", Type: typeSequence, Status: statusDone,
		CreatedAt: "2026-08-13T10:00:00+08:00", UpdatedAt: "2026-08-13T10:00:00+08:00"}
	writeRetroFactTask(t, root, done, true,
		factEvent(1, "2026-08-13T10:00:00+08:00", evDone, map[string]any{"cost_total": 0.1, "turns_total": 1}),
	)
	id, err := queueRetroTask(root, cfg, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	card, err := loadTask(root, id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(card.Prompts[0], "CUSTOM_V2") || strings.Contains(card.Prompts[0], "{{FACTS_JSON}}") {
		t.Fatalf("compatible local template was not rendered: %s", card.Prompts[0])
	}
	events, _, err := loadTaskEvents(root, id)
	if err != nil || len(events) == 0 || retroDetailString(events[0].Detail, "template_source") != "local_v2" {
		t.Fatalf("local template source not disclosed: events=%v err=%v", events, err)
	}
}
