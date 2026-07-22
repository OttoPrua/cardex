package main

// events_test.go —— CG-2 事件账本验收测试。
//
// 每条测试映射到 spec 的一项验收：
//  1) 全状态机 mock 集成：每个状态迁移恰有一条对应事件（枚举遗漏即红）。
//  2) 崩溃注入：事件写入中途 kill -9 → 重启后文件仍可解析（尾部残行丢弃并披露），后续追加不损坏。
//  3) 反例注入：手工删除中间一条事件 → board 活动流必须显示"事件缺口"（用状态反推补齐冒充完整历史则报红）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------- 基础工具 ----------

// readAllEvents 直接从盘上按行读事件（跳过 loadTaskEvents 的容错分支，测试要看原始状态）。
func readAllEventsRaw(t *testing.T, root, id string) []TaskEvent {
	t.Helper()
	events, _, err := loadTaskEvents(root, id)
	if err != nil {
		t.Fatalf("读事件账本失败: %v", err)
	}
	return events
}

func eventTypes(events []TaskEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Type)
	}
	return out
}

// fakeClaudeBin 生成一个只回一次 JSON 结果的假 claude 可执行文件。
// stdoutJSON 是要打到 stdout 的一整段（可以是 result JSON、也可以是 limit reached 文本）；
// exitCode 非 0 让 runTask 走"IsError" 分支。
func fakeClaudeBin(t *testing.T, stdoutJSON, stderrLine string, exitCode int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	// stdout 全量、stderr 一行、指定退出码——runTask 会把 stdout 交给 parseClaudeJSON、
	// 把 stdout+stderr 交给 isLimitHit / errorSummary。
	script := "#!/bin/sh\n"
	if stdoutJSON != "" {
		// 用单引号 heredoc 保原样，避免 shell 展开 result 里的 $。
		script += "cat <<'JSON_EOF'\n" + stdoutJSON + "\nJSON_EOF\n"
	}
	if stderrLine != "" {
		script += "printf '%s\\n' " + shSingleQuote(stderrLine) + " >&2\n"
	}
	script += fmt.Sprintf("exit %d\n", exitCode)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func shSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func mkOKResultJSON(sessionID string) string {
	return fmt.Sprintf(`{"type":"result","result":"done step","session_id":%q,"num_turns":1,"total_cost_usd":0.01,"duration_ms":123,"is_error":false}`, sessionID)
}

// runTaskCfg 造一份适合 runTask 的最小 Config：claudeBin + 短超时 + 无限额回退。
func runTaskCfg(t *testing.T, claudeBin string) *Config {
	cfg := defaultConfig(claudeBin)
	cfg.StepTimeoutMin = 1
	cfg.MaxAttempts = 3
	cfg.RetryBackoffMin = 1
	cfg.CooldownMarginSec = 0
	cfg.LimitFallbackMin = 30
	return cfg
}

// ---------- 单元层：事件账本本身 ----------

func TestRecordEventSeqMonotonicAndAppendOnly(t *testing.T) {
	root := testRoot(t)
	emitTaskEvent(root, "t1", evQueued, "cli:add", statusQueued, 0, map[string]any{"k": 1})
	emitTaskEvent(root, "t1", evDispatched, "runner", statusRunning, 0, nil)
	emitTaskEvent(root, "t1", evStepOK, "runner", statusRunning, 1, nil)
	events := readAllEventsRaw(t, root, "t1")
	if len(events) != 3 {
		t.Fatalf("应有 3 条事件, got %d: %+v", len(events), events)
	}
	for i, want := range []int64{1, 2, 3} {
		if events[i].Seq != want {
			t.Fatalf("seq 单调递增被破坏: 第 %d 条 seq=%d want %d", i, events[i].Seq, want)
		}
	}
	if events[0].Type != evQueued || events[1].Type != evDispatched || events[2].Type != evStepOK {
		t.Fatalf("追加顺序错乱: %v", eventTypes(events))
	}
}

func TestRecordEventCrashTailToleratedAndSubsequentAppendClean(t *testing.T) {
	root := testRoot(t)
	// 先正常落两条。
	emitTaskEvent(root, "t2", evQueued, "cli:add", statusQueued, 0, nil)
	emitTaskEvent(root, "t2", evDispatched, "runner", statusRunning, 0, nil)

	// 模拟 kill -9：往末尾追加一段"半截 JSON 无换行"——
	// 这正是崩溃在 fsync 前留下的最险恶残尾（下一条 append 会被"粘"到坏行头）。
	f, err := os.OpenFile(eventsPath(root, "t2"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":3,"type":"step_ok","ts":"2026-`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	// 直接读要能识别损坏。
	events, hadCorrupt, err := readEvents(eventsPath(root, "t2"))
	if err != nil {
		t.Fatal(err)
	}
	if !hadCorrupt {
		t.Fatal("崩溃残尾必须被读出 hadCorruption=true")
	}
	if len(events) != 2 {
		t.Fatalf("崩溃残尾以外应剩 2 条合法事件, got %d: %+v", len(events), events)
	}

	// 关键：后续追加不能被残尾"吃"——ensureTrailingNewline 应先补 \n 把残尾封成独立坏行。
	emitTaskEvent(root, "t2", evStepOK, "runner", statusRunning, 1, nil)
	events2, hadCorrupt2, err := readEvents(eventsPath(root, "t2"))
	if err != nil {
		t.Fatal(err)
	}
	if !hadCorrupt2 {
		t.Fatal("残尾仍应被披露（新事件不消灭历史损坏）")
	}
	if len(events2) != 3 {
		t.Fatalf("新事件必须完整独立成行, got %d: %+v", len(events2), events2)
	}
	if events2[2].Seq != 3 || events2[2].Type != evStepOK {
		t.Fatalf("新事件应 seq=3 type=step_ok, got seq=%d type=%q", events2[2].Seq, events2[2].Type)
	}
}

func TestArchiveTaskAlsoArchivesEvents(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	tk := newTask(root, cfg, typeSequence, "归档", "/tmp", []string{"p"}, 5)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	emitTaskEvent(root, tk.ID, evQueued, "cli:add", statusQueued, 0, nil)
	if err := archiveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(eventsPath(root, tk.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active events.jsonl 应被移走, stat err=%v", err)
	}
	if _, err := os.Stat(archivedEventsPath(root, tk.ID)); err != nil {
		t.Fatalf("归档事件账本应存在: %v", err)
	}
	// loadTaskEvents 需要能读到归档后的账本，看板活动流才能不断线。
	events, _, err := loadTaskEvents(root, tk.ID)
	if err != nil {
		t.Fatalf("loadTaskEvents 应能读归档账本: %v", err)
	}
	if len(events) != 1 || events[0].Type != evQueued {
		t.Fatalf("归档账本内容错乱: %+v", events)
	}
}

// ---------- 集成层：runTask 全状态机覆盖 ----------

// TestRunTaskEmitsExpectedEventsHappyPath 验证顺利完成路径每个状态迁移恰有一条事件。
// 两步任务：dispatched → step_ok → step_ok(final) → done。
func TestRunTaskEmitsExpectedEventsHappyPath(t *testing.T) {
	root := testRoot(t)
	claudeBin := fakeClaudeBin(t, mkOKResultJSON("sess-1"), "", 0)
	cfg := runTaskCfg(t, claudeBin)

	work := t.TempDir()
	tk := newTask(root, cfg, typeSequence, "两步顺利", work, []string{"step-1", "step-2"}, 5)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	// 手工补一条 queued 事件，模拟 cmdAdd 的落点——runTask 只负责从 dispatched 起。
	emitTaskEvent(root, tk.ID, evQueued, "cli:add", statusQueued, 0, nil)

	if err := runTask(context.Background(), root, cfg, tk, false); err != nil {
		t.Fatal(err)
	}
	events := readAllEventsRaw(t, root, tk.ID)
	got := eventTypes(events)
	want := []string{evQueued, evDispatched, evStepOK, evStepOK, evDone}
	if !equalStringSlices(got, want) {
		t.Fatalf("事件序列不符\ngot  %v\nwant %v", got, want)
	}
	// 末步事件应带 final_step=true。
	if final, _ := events[3].Detail["final_step"].(bool); !final {
		t.Fatalf("末步 step_ok 应 detail.final_step=true, got %+v", events[3].Detail)
	}
	// 每条 seq 单调不空。
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Fatalf("第 %d 条 seq=%d 应 %d", i, e.Seq, i+1)
		}
	}
}

// TestRunTaskEmitsLimitPausedEvent 验证限额挂起路径生成 limit_paused 事件（带 engine=claude 与 resume_at）。
func TestRunTaskEmitsLimitPausedEvent(t *testing.T) {
	root := testRoot(t)
	// fake claude 打限额结果 JSON + 非零退出。isLimitHit 靠 result.IsError + limitRe。
	future := time.Now().Add(20 * time.Minute).Unix()
	limitJSON := fmt.Sprintf(`{"type":"result","is_error":true,"subtype":"limit","result":"Claude AI usage limit reached|%d"}`, future)
	claudeBin := fakeClaudeBin(t, limitJSON, "", 1)
	cfg := runTaskCfg(t, claudeBin)

	work := t.TempDir()
	tk := newTask(root, cfg, typeSequence, "限额", work, []string{"p"}, 5)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if err := runTask(context.Background(), root, cfg, tk, false); err != nil {
		t.Fatal(err)
	}
	events := readAllEventsRaw(t, root, tk.ID)
	if len(events) != 2 {
		t.Fatalf("dispatched + limit_paused 恰 2 条, got %d: %v", len(events), eventTypes(events))
	}
	if events[0].Type != evDispatched || events[1].Type != evLimitPaused {
		t.Fatalf("事件枚举缺失: %v", eventTypes(events))
	}
	if engine, _ := events[1].Detail["engine"].(string); engine != "claude" {
		t.Fatalf("limit_paused.detail.engine 应 claude, got %q", engine)
	}
	// resume_at 必须落进 detail 里（供恢复时对账）。
	if v, ok := events[1].Detail["resume_at"]; !ok || v == nil {
		t.Fatalf("limit_paused.detail.resume_at 缺失: %+v", events[1].Detail)
	}
}

// TestRunTaskEmitsRetryThenFailed 验证退避重试与 attempts 超限的两阶段事件。
func TestRunTaskEmitsRetryThenFailed(t *testing.T) {
	root := testRoot(t)
	// 非限额错误：JSON is_error=true 但内容里不含 limit 关键词。
	errJSON := `{"type":"result","is_error":true,"subtype":"random_error","result":"some transient hiccup"}`
	claudeBin := fakeClaudeBin(t, errJSON, "", 1)
	cfg := runTaskCfg(t, claudeBin)
	cfg.MaxAttempts = 2

	work := t.TempDir()
	tk := newTask(root, cfg, typeSequence, "重试路径", work, []string{"p"}, 5)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	// 第一次跑：应记 dispatched + retry。
	if err := runTask(context.Background(), root, cfg, tk, false); err != nil {
		t.Fatal(err)
	}
	events := readAllEventsRaw(t, root, tk.ID)
	if len(events) != 2 || events[0].Type != evDispatched || events[1].Type != evRetry {
		t.Fatalf("首轮期望 dispatched+retry, got %v", eventTypes(events))
	}
	if a, _ := events[1].Detail["attempts"].(float64); a != 1 { // JSON 数字回来是 float64
		t.Fatalf("retry.detail.attempts 应 1, got %v", events[1].Detail["attempts"])
	}

	// 第二次跑：attempts=2 达到 MaxAttempts → dispatched + failed。
	tk2, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := runTask(context.Background(), root, cfg, tk2, false); err != nil {
		t.Fatal(err)
	}
	events = readAllEventsRaw(t, root, tk.ID)
	// dispatched1 + retry1 + dispatched2 + failed
	if len(events) != 4 {
		t.Fatalf("超轮限期望 4 条, got %d: %v", len(events), eventTypes(events))
	}
	if events[3].Type != evFailed {
		t.Fatalf("末条应 failed, got %v", eventTypes(events))
	}
}

// TestCliSetStatusHoldReleaseCancelEvents 验证 CLI 的 hold/release/cancel 各自留一条对应事件。
func TestCliSetStatusHoldReleaseCancelEvents(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	tk := newTask(root, cfg, typeSequence, "cli 状态迁移", "/tmp", []string{"p"}, 5)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}

	// hold
	if err := cmdSetStatus([]string{"-root", root, tk.ID}, "hold"); err != nil {
		t.Fatal(err)
	}
	events := readAllEventsRaw(t, root, tk.ID)
	if len(events) != 1 || events[0].Type != evHeld || events[0].Actor != "cli:hold" {
		t.Fatalf("hold 应留 1 条 held 事件 actor=cli:hold, got %+v", events)
	}
	// release
	if err := cmdSetStatus([]string{"-root", root, tk.ID}, "release"); err != nil {
		t.Fatal(err)
	}
	events = readAllEventsRaw(t, root, tk.ID)
	if len(events) != 2 || events[1].Type != evQueued || events[1].Actor != "cli:release" {
		t.Fatalf("release 应追加 queued 事件, got %+v", events)
	}
	// cancel（非 running 卡：cmdSetStatus cancel 会走归档路径，事件必须先于归档写入）
	if err := cmdSetStatus([]string{"-root", root, tk.ID}, "cancel"); err != nil {
		t.Fatal(err)
	}
	// 归档后：eventsPathAnywhere 优先归档路径。
	events, _, err := loadTaskEvents(root, tk.ID)
	if err != nil {
		t.Fatalf("归档事件应可读: %v", err)
	}
	if len(events) != 3 || events[2].Type != evCanceled || events[2].Actor != "cli:cancel" {
		t.Fatalf("cancel 应追加 canceled 事件（先写事件再归档）, got %+v", events)
	}
}

// TestReviewVerdictClosepathEmitsCloseoutAndQueued 验证 pass→closeout 时父卡 closeout + 子卡 queued 双事件。
func TestReviewVerdictClosepathEmitsCloseoutAndQueued(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	impl := mkImplTask(t, root, cfg)
	impl.Closeout = "回写 done"
	if err := saveTask(root, impl); err != nil {
		t.Fatal(err)
	}
	rv := mkReviewTask(t, root, cfg, impl)
	passReport := "```json\n{\"verdict\":\"pass\",\"p0\":[],\"p1\":[],\"p2\":[],\"summary\":\"ok\"}\n```"
	handleReviewVerdict(root, cfg, rv, passReport, nil)

	// 父审核卡应留 closeout 事件（指向子卡）。
	parent := readAllEventsRaw(t, root, rv.ID)
	if len(parent) != 1 || parent[0].Type != evCloseout {
		t.Fatalf("父审核卡应恰 1 条 closeout 事件, got %+v", parent)
	}
	childID, _ := parent[0].Detail["child"].(string)
	if childID == "" {
		t.Fatal("closeout.detail.child 缺失")
	}
	child := readAllEventsRaw(t, root, childID)
	if len(child) != 1 || child[0].Type != evQueued || child[0].Actor != "runner:closeout" {
		t.Fatalf("子收口卡应恰 1 条 queued 事件 actor=runner:closeout, got %+v", child)
	}
}

// ---------- 活动流：事件缺口与拒绝状态反推 ----------

func TestBoardActivityShowsGapWhenMiddleEventDeleted(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	tk := newTask(root, cfg, typeSequence, "gap-target", "/tmp", []string{"p"}, 5)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	// 写三条事件（seq 1/2/3），随后手工删除 seq=2 那行 → 期望活动流出现"事件缺口"。
	emitTaskEvent(root, tk.ID, evQueued, "cli:add", statusQueued, 0, nil)
	emitTaskEvent(root, tk.ID, evDispatched, "runner", statusRunning, 0, nil)
	emitTaskEvent(root, tk.ID, evStepOK, "runner", statusRunning, 1, nil)

	deleteEventBySeq(t, eventsPath(root, tk.ID), 2)

	items := buildActivity(root, []*Task{tk})
	if !containsGapItem(items) {
		t.Fatalf("删除中间事件后活动流必须显示'事件缺口', got %+v", items)
	}
	// 反例守卫：绝不允许用状态反推补一条"派上执行"。删掉 dispatched 后，
	// 若活动流里还出现 evDispatched 的人读文本，说明代码悄悄补齐了——报红。
	for _, it := range items {
		if it.Event == "派上执行（第 1 步）" {
			t.Fatalf("活动流禁止用状态反推冒充完整历史: %+v", items)
		}
	}
}

func TestBoardActivityRefusesStatusInferenceWhenNoEventsFile(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	// 只造卡不写事件——旧卡场景/CG-2 前入队的历史卡。
	tk := newTask(root, cfg, typeSequence, "no-events", "/tmp", []string{"p"}, 5)
	tk.Status = statusRunning
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	items := buildActivity(root, []*Task{tk})
	if len(items) != 0 {
		t.Fatalf("无事件账本时活动流必须为空（禁止用 Status=running 反推伪造历史）, got %+v", items)
	}
}

// TestBoardActivityUsesEventsNotStatus 交叉验证：卡当前 Status=failed 但事件流里有 done——
// 活动流必须按事件流讲"已完成"，绝不按 Status 讲"失败"。这是"事件是唯一真相源"的正向守卫。
func TestBoardActivityUsesEventsNotStatus(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	tk := newTask(root, cfg, typeSequence, "truth-source", "/tmp", []string{"p"}, 5)
	tk.Status = statusFailed // 故意扭曲当前状态
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	emitTaskEvent(root, tk.ID, evQueued, "cli:add", statusQueued, 0, nil)
	emitTaskEvent(root, tk.ID, evDone, "runner", statusDone, 1, nil)
	items := buildActivity(root, []*Task{tk})
	sawDone := false
	for _, it := range items {
		if strings.Contains(it.Event, "已完成") {
			sawDone = true
		}
		if strings.Contains(it.Event, "失败") {
			t.Fatalf("活动流不得按 Status=failed 反推'失败'，事件里没有 failed: %+v", items)
		}
	}
	if !sawDone {
		t.Fatalf("事件里有 done，活动流必须讲'已完成': %+v", items)
	}
}

// ---------- 辅助 ----------

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsGapItem(items []ActivityItem) bool {
	for _, it := range items {
		if strings.Contains(it.Event, "事件缺口") {
			return true
		}
	}
	return false
}

func deleteEventBySeq(t *testing.T, path string, seq int64) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		var ev TaskEvent
		if json.Unmarshal([]byte(trimmed), &ev) == nil && ev.Seq == seq {
			continue // 跳过该行
		}
		out = append(out, []byte(line+"\n")...)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}
