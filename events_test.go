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
	"sync"
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

// ---------- Round-1 修复回红反例（每个 P1 一个,反例注入即报红）----------

// TestBudgetRedlineEmitsRetryEvent (P1-1) 验证:多步任务在步与步之间踩红线回排队时,
// 必须留一条 evRetry 事件(reason=budget_redline)。反例注入:删掉 runner.go:596 那条 emit,
// 事件流会呈现 dispatched→dispatched 静默断档且无 seq 缺口可测,活动流看不出真历史——
// "每状态迁移恰一条事件"被绕过。
func TestBudgetRedlineEmitsRetryEvent(t *testing.T) {
	root := testRoot(t)
	claudeBin := fakeClaudeBin(t, mkOKResultJSON("sess-budget"), "", 0)
	cfg := runTaskCfg(t, claudeBin)
	// 队列封顶 1000 加权 token,预写一条 1500 的用量记录 → budgetBlocked 触发 QueueBudgetTokens 通道。
	cfg.QueueBudgetTokens = 1000

	work := t.TempDir()
	tk := newTask(root, cfg, typeSequence, "红线让位", work, []string{"step-1", "step-2"}, 5)
	// 第 1 步已跑过:Step=1、非 MidStep → 进入 runner.go:587 的"步与步之间复查红线"分支。
	tk.Step = 1
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	// 预写 1500 加权 token 到 usage.json 让 queueWindowSpent 返回 >= 1000。
	spent := usageRec{At: time.Now().Unix(), TaskID: "prior", Model: "opus", Weighted: 1500}
	blob, _ := json.Marshal([]usageRec{spent})
	if err := os.WriteFile(usagePath(root), blob, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runTask(context.Background(), root, cfg, tk, false); err != nil {
		t.Fatal(err)
	}
	events := readAllEventsRaw(t, root, tk.ID)
	if len(events) == 0 {
		t.Fatalf("红线路径至少应留 1 条 retry 事件,got 0")
	}
	last := events[len(events)-1]
	if last.Type != evRetry {
		t.Fatalf("红线让位应 emit evRetry,got type=%q 序列=%v", last.Type, eventTypes(events))
	}
	if reason, _ := last.Detail["reason"].(string); reason != "budget_redline" {
		t.Fatalf("retry.detail.reason 应为 budget_redline,got %q", reason)
	}
	if last.Status != statusQueued {
		t.Fatalf("红线让位后 status 应为 queued(下一 tick 重试),got %q", last.Status)
	}
}

// TestPostCompleteProgressFailureEmitsFailed (P1-2) 验证:交叉 C 卡完成路径已 emit evDone,
// 若 saveProgressFromResult 失败改判 failed,必须补一条 evFailed——否则事件账本终局是 done,
// 盘上是 failed,活动流按事件流讲"已完成"的假历史。反例注入:删掉 runner.go:954 那条 emit,
// 就能构造事件历史与盘态不一致。
func TestPostCompleteProgressFailureEmitsFailed(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	// C 卡:XRole=C + EmitProgress=true + 合法 verdict(通过 crossMergeVerdictOK)。
	tk := newTask(root, cfg, typeSequence, "C-progress-fail", "/tmp", []string{"p"}, 5)
	tk.XRole = "C"
	tk.EmitProgress = true
	tk.ProgressKey = "k1"
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	// 预写一条 evDone 事件(模拟 runTask 完成路径先 emit 的 done)——postComplete 里改判 failed 必须再补一条。
	emitTaskEvent(root, tk.ID, evDone, "runner", statusDone, 1, nil)

	// 关键:在 <root>/progress 位置预先造一个"文件"占位,让 MkdirAll("<root>/progress") 报 ENOTDIR。
	if err := os.WriteFile(progressDir(root), []byte("blocker"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 造一个合法 verdict 的结果字符串,避免走前面的 cross_merge_contract_violation 分支。
	result := "分析毕。\n```json\n{\"verdict\":\"pass\",\"confidence\":\"high\",\"summary\":\"ok\"}\n```"
	res := &claudeResult{Result: result}

	// 打开一个临时日志文件供 postComplete 用。
	lg, err := os.CreateTemp(t.TempDir(), "log-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()

	postComplete(root, cfg, tk, res, lg)

	if tk.Status != statusFailed {
		t.Fatalf("C 卡 progress 落盘失败必须改判 failed,got %q", tk.Status)
	}
	events := readAllEventsRaw(t, root, tk.ID)
	if len(events) < 2 {
		t.Fatalf("应至少有 done + failed 两条事件,got %d: %v", len(events), eventTypes(events))
	}
	last := events[len(events)-1]
	if last.Type != evFailed {
		t.Fatalf("末条事件应为 failed(补一条终局改判事件),got type=%q 序列=%v", last.Type, eventTypes(events))
	}
	if reason, _ := last.Detail["reason"].(string); reason != "progress_persist_failed" {
		t.Fatalf("failed.detail.reason 应为 progress_persist_failed,got %q", reason)
	}
	if last.Actor != "runner:postComplete" {
		t.Fatalf("failed.actor 应为 runner:postComplete(区分 runTask 主路径的 failed),got %q", last.Actor)
	}
}

// TestDescribeEventStepNumbers (P1-3) 验证 describeEvent 的步号语义:
//   - evDispatched: 派上时 Step=待执行步序(0-indexed),显示 +1;
//   - evStepOK: runner.go:779 已 t.Step++,Step=已完成步数(1-indexed),直接显示;
//   - 末步 evStepOK(final_step=true): 同样直接显示,不再 +1(否则两步卡末步会说"第 3 步")。
// 反例注入:把 evStepOK 分支恢复成 ev.Step+1,末步文本会退化为"第 N+1 步·末步",本测试报红。
func TestDescribeEventStepNumbers(t *testing.T) {
	// 派上执行:runner.go:775 dispatched 时 Step 是 0-indexed 待执行序,显示应为"第 Step+1 步"。
	got := describeEvent(TaskEvent{Type: evDispatched, Step: 0})
	if got != "派上执行（第 1 步）" {
		t.Fatalf("dispatched Step=0 应显示'第 1 步',got %q", got)
	}
	got = describeEvent(TaskEvent{Type: evDispatched, Step: 1})
	if got != "派上执行（第 2 步）" {
		t.Fatalf("dispatched Step=1 应显示'第 2 步',got %q", got)
	}
	// 中间步 step_ok:Step=已完成步数(1-indexed),直接显示。
	got = describeEvent(TaskEvent{Type: evStepOK, Step: 1})
	if got != "步完成（第 1 步）" {
		t.Fatalf("中间步 step_ok Step=1 应显示'第 1 步',got %q", got)
	}
	// 末步 step_ok:同样直接显示 Step,不再 +1。两步卡末步 Step=2 应显示"第 2 步·末步"而非"第 3 步·末步"。
	got = describeEvent(TaskEvent{Type: evStepOK, Step: 2, Detail: map[string]any{"final_step": true}})
	if got != "步完成（第 2 步·末步）" {
		t.Fatalf("末步 step_ok Step=2 应显示'第 2 步·末步',got %q", got)
	}
}

// TestBoardActivityShowsGapWhenHeadEventDeleted (P1-4) 验证:删掉 seq=1 的头部事件后,
// 活动流必须显示"事件缺口(缺失 seq 1..1)"。反例注入:恢复 buildActivity 里的 i==0 特判
// (prevSeq = events[0].Seq - 1),头部剪掉后剩下 [2,3,...] 会被当完整历史,本测试报红。
func TestBoardActivityShowsGapWhenHeadEventDeleted(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	tk := newTask(root, cfg, typeSequence, "head-gap", "/tmp", []string{"p"}, 5)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	emitTaskEvent(root, tk.ID, evQueued, "cli:add", statusQueued, 0, nil)
	emitTaskEvent(root, tk.ID, evDispatched, "runner", statusRunning, 0, nil)
	emitTaskEvent(root, tk.ID, evStepOK, "runner", statusRunning, 1, nil)
	// 删头部 seq=1
	deleteEventBySeq(t, eventsPath(root, tk.ID), 1)

	items := buildActivity(root, []*Task{tk})
	if !containsGapItem(items) {
		t.Fatalf("删除头部 seq=1 后活动流必须显示'事件缺口',got %+v", items)
	}
	// 应披露具体缺失范围 1..1,而非只有尾部损坏披露。
	sawHeadGap := false
	for _, it := range items {
		if strings.Contains(it.Event, "缺失 seq 1..1") {
			sawHeadGap = true
		}
	}
	if !sawHeadGap {
		t.Fatalf("头部缺口披露应含'缺失 seq 1..1',got %+v", items)
	}
}

// TestRecordEventConcurrentWritesNoSeqCollision (P1-5) 验证:同一任务的并发 emit 全都唯一 seq。
// 反例注入:去掉 events.go:101 的 acquireEventLock,多个 goroutine 会 read-compute-write 竞态,
// 两条事件同 seq——"seq 单调递增"契约破,seq 序列表面完整但存在重复,反例注入删中间事件也测不出来。
func TestRecordEventConcurrentWritesNoSeqCollision(t *testing.T) {
	root := testRoot(t)
	const workers = 20
	const perWorker = 5
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				emitTaskEvent(root, "concurrent", evStepOK, fmt.Sprintf("worker-%d", i), statusRunning, j, map[string]any{"i": i, "j": j})
			}
		}(i)
	}
	wg.Wait()

	events := readAllEventsRaw(t, root, "concurrent")
	want := workers * perWorker
	if len(events) != want {
		t.Fatalf("并发写入应留 %d 条事件,got %d", want, len(events))
	}
	seen := make(map[int64]bool, want)
	var maxSeq int64
	for _, ev := range events {
		if seen[ev.Seq] {
			t.Fatalf("发现重复 seq=%d(锁失效!),事件=%+v", ev.Seq, ev)
		}
		seen[ev.Seq] = true
		if ev.Seq > maxSeq {
			maxSeq = ev.Seq
		}
	}
	// seq 必须密集连续 1..N,任何跳号都是"锁挡住但 nextSeq 计算窗口错开"的隐蔽 bug。
	for s := int64(1); s <= int64(want); s++ {
		if !seen[s] {
			t.Fatalf("并发写入 seq 应密集 1..%d,缺 seq=%d(实测最大 seq=%d)", want, s, maxSeq)
		}
	}
}

// TestFinalizeCanceledSkipsEmitAfterCliCancel (P1-6) 验证:cli:cancel 已在盘上留下 canceled 状态
// 并 emit 过一条 evCanceled 后,drain 复扫触发 finalizeCanceled 时必须**跳过再一次 emit**——
// 否则一次取消迁移落两条 canceled 事件,直接违背"每状态迁移恰一条事件"验收。
// 反例注入:去掉 runner.go:865 的 cliAlreadyEmitted 判断,本测试的第一 subtest 报红。
// 第二 subtest 覆盖对偶路径:非 cli 触发的 ctx 超时/父上下文取消,盘上没有 cli:cancel 记录,
// 此时 runner 是取消的第一手记录者,必须由 finalizeCanceled emit 一条(不能因过度去重漏事件)。
func TestFinalizeCanceledSkipsEmitAfterCliCancel(t *testing.T) {
	t.Run("cli_cancel_then_finalize_only_one_event", func(t *testing.T) {
		root := testRoot(t)
		cfg := testCfg()
		tk := newTask(root, cfg, typeSequence, "cli-cancel-dedup", "/tmp", []string{"p"}, 5)
		tk.Status = statusRunning
		if err := saveTask(root, tk); err != nil {
			t.Fatal(err)
		}
		// 模拟 cli:cancel:先写 canceled 到盘 + emit 一条 evCanceled(和 main.go:1441 一致)。
		tk.Status = statusCanceled
		if err := saveTask(root, tk); err != nil {
			t.Fatal(err)
		}
		emitTaskEvent(root, tk.ID, evCanceled, "cli:cancel", statusCanceled, tk.Step, map[string]any{"was_running": true})

		// drain 复扫触发 finalizeCanceled;此时盘上已 canceled,cliAlreadyEmitted=true 应跳过 emit。
		lg, err := os.CreateTemp(t.TempDir(), "log-*.log")
		if err != nil {
			t.Fatal(err)
		}
		defer lg.Close()
		if err := finalizeCanceled(root, tk, lg); err != nil {
			t.Fatal(err)
		}

		events, _, err := loadTaskEvents(root, tk.ID)
		if err != nil {
			t.Fatal(err)
		}
		cancelCount := 0
		for _, ev := range events {
			if ev.Type == evCanceled {
				cancelCount++
			}
		}
		if cancelCount != 1 {
			t.Fatalf("cli:cancel + finalizeCanceled 只能留 1 条 canceled 事件,got %d: %v", cancelCount, eventTypes(events))
		}
		// 且这唯一一条必须是 cli:cancel 记的(不是 runner_cancel)——保 actor 语义正确。
		for _, ev := range events {
			if ev.Type == evCanceled && ev.Actor != "cli:cancel" {
				t.Fatalf("唯一 canceled 事件的 actor 应为 cli:cancel,got %q", ev.Actor)
			}
		}
	})

	t.Run("runner_cancel_without_cli_still_emits", func(t *testing.T) {
		root := testRoot(t)
		cfg := testCfg()
		tk := newTask(root, cfg, typeSequence, "runner-only-cancel", "/tmp", []string{"p"}, 5)
		tk.Status = statusRunning // 盘上是 running,不是 cli:cancel 走位——finalizeCanceled 是取消第一手记录者。
		if err := saveTask(root, tk); err != nil {
			t.Fatal(err)
		}

		lg, err := os.CreateTemp(t.TempDir(), "log-*.log")
		if err != nil {
			t.Fatal(err)
		}
		defer lg.Close()
		if err := finalizeCanceled(root, tk, lg); err != nil {
			t.Fatal(err)
		}

		events, _, err := loadTaskEvents(root, tk.ID)
		if err != nil {
			t.Fatal(err)
		}
		cancelCount := 0
		var runnerCancel *TaskEvent
		for i := range events {
			if events[i].Type == evCanceled {
				cancelCount++
				runnerCancel = &events[i]
			}
		}
		if cancelCount != 1 {
			t.Fatalf("非 cli:cancel 路径 finalizeCanceled 必须 emit 1 条 canceled 事件,got %d: %v", cancelCount, eventTypes(events))
		}
		if runnerCancel.Actor != "runner" {
			t.Fatalf("runner 兜底记的 canceled actor 应为 runner,got %q", runnerCancel.Actor)
		}
		if reason, _ := runnerCancel.Detail["reason"].(string); reason != "runner_cancel" {
			t.Fatalf("detail.reason 应为 runner_cancel,got %q", reason)
		}
	})
}
