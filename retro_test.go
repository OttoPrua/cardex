package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// retroTestRoot 建一个最小数据根（tasks/ 足够 saveTask 落盘；模板走内置回退，不必写盘）。
func retroTestRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{tasksDir(root), archiveDir(root), logsDir(root)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func retroTestConfig(n int) *Config {
	cfg := defaultConfig("claude")
	cfg.RetroEveryNDone = n
	return cfg
}

// TestRetroDefaultOff 复盘默认必须是关的：自动入队的卡会烧额度，
// 升级不该在用户没表态时就开始自己派卡。
func TestRetroDefaultOff(t *testing.T) {
	if n := defaultConfig("claude").RetroEveryNDone; n != 0 {
		t.Errorf("retro_every_n_done 默认应为 0(关闭), got %d", n)
	}
}

// doneCard 是一张普通业务卡（非复盘卡），用来喂计数器。
func doneCard(id string) *Task {
	return &Task{ID: id, Type: typeSequence, Status: statusDone, Prompts: []string{"p"}}
}

func readCounter(t *testing.T, root string) retroCounter {
	t.Helper()
	data, err := os.ReadFile(retroCounterPath(root))
	if err != nil {
		t.Fatalf("读计数器: %v", err)
	}
	var c retroCounter
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("解析计数器: %v", err)
	}
	return c
}

func retroTasks(t *testing.T, root string) []*Task {
	t.Helper()
	all, err := loadTasks(root)
	if err != nil {
		t.Fatalf("loadTasks: %v", err)
	}
	var out []*Task
	for _, task := range all {
		if isRetroTask(task) {
			out = append(out, task)
		}
	}
	return out
}

// TestRetroCounterWatermark 钉死计数器的水位算术。
// 【突变致死】把 `c.DoneTotal-c.TriggeredAt < n` 改成 `<=`、把水位写成 `watermark-1`、
// 或触发后忘记推进 TriggeredAt，都会在这里报红：触发点从第 3 张漂到第 2/4 张，
// 或第二轮触发点从第 6 张漂走。
func TestRetroCounterWatermark(t *testing.T) {
	root := retroTestRoot(t)
	cfg := retroTestConfig(3)

	// 第 1、2 张：只计数不触发。
	for i, id := range []string{"a1", "a2"} {
		got, err := noteTaskDone(root, cfg, doneCard(id))
		if err != nil {
			t.Fatalf("noteTaskDone(%s): %v", id, err)
		}
		if got != "" {
			t.Fatalf("第 %d 张 done 就触发复盘了(N=3): %s", i+1, got)
		}
		c := readCounter(t, root)
		if c.DoneTotal != int64(i+1) || c.TriggeredAt != 0 {
			t.Fatalf("第 %d 张后计数器应为 done_total=%d/triggered_at=0, got %+v", i+1, i+1, c)
		}
	}

	// 第 3 张：恰好触发，水位必须推到 3（不是 0、不是 2、不是 4）。
	id3, err := noteTaskDone(root, cfg, doneCard("a3"))
	if err != nil {
		t.Fatalf("noteTaskDone(a3): %v", err)
	}
	if id3 == "" {
		t.Fatal("第 3 张 done 应触发复盘卡(N=3), 但没有入队")
	}
	c := readCounter(t, root)
	if c.DoneTotal != 3 || c.TriggeredAt != 3 {
		t.Fatalf("触发后计数器应为 done_total=3/triggered_at=3, got %+v", c)
	}
	if c.LastRetroTask != id3 {
		t.Errorf("last_retro_task=%q, 应为 %q", c.LastRetroTask, id3)
	}

	// 第 4、5 张：水位已在 3，距下次触发还差；不得再入队。
	for i, id := range []string{"a4", "a5"} {
		got, err := noteTaskDone(root, cfg, doneCard(id))
		if err != nil {
			t.Fatalf("noteTaskDone(%s): %v", id, err)
		}
		if got != "" {
			t.Fatalf("第 %d 张不该触发(上次水位=3, N=3): %s", i+4, got)
		}
	}
	// 第 6 张：第二轮触发，水位推到 6。
	id6, err := noteTaskDone(root, cfg, doneCard("a6"))
	if err != nil {
		t.Fatalf("noteTaskDone(a6): %v", err)
	}
	if id6 == "" {
		t.Fatal("第 6 张 done 应触发第二张复盘卡")
	}
	if c := readCounter(t, root); c.DoneTotal != 6 || c.TriggeredAt != 6 {
		t.Fatalf("第二轮触发后应为 done_total=6/triggered_at=6, got %+v", c)
	}
	if got := retroTasks(t, root); len(got) != 2 {
		t.Fatalf("6 张 done(N=3) 应恰好入队 2 张复盘卡, got %d", len(got))
	}
}

// TestRetroIdempotentAfterCrash 是委托人点名的幂等负例：
// 模拟"计数器已记下已触发的水位"（等价于崩溃后重启、复盘卡已入队或已丢失），
// 重复调用不得重复入队。
func TestRetroIdempotentAfterCrash(t *testing.T) {
	root := retroTestRoot(t)
	cfg := retroTestConfig(10)

	// 崩溃恢复现场：盘上水位显示"第 10 张时已触发过"。
	if err := saveRetroCounter(root, &retroCounter{DoneTotal: 10, TriggeredAt: 10}); err != nil {
		t.Fatal(err)
	}
	// 重放第 11..19 张 done：距水位不足 10 张，一张复盘卡都不该出现。
	for i := 11; i <= 19; i++ {
		got, err := noteTaskDone(root, cfg, doneCard("crash"))
		if err != nil {
			t.Fatalf("第 %d 张: %v", i, err)
		}
		if got != "" {
			t.Fatalf("第 %d 张重复入队了复盘卡 %s —— 水位幂等失效", i, got)
		}
		if c := readCounter(t, root); c.TriggeredAt != 10 {
			t.Fatalf("第 %d 张后水位被改动: triggered_at=%d, 应仍为 10", i, c.TriggeredAt)
		}
	}
	if got := retroTasks(t, root); len(got) != 0 {
		t.Fatalf("崩溃恢复重放期间不应入队任何复盘卡, got %d 张: %v", len(got), got)
	}
	// 第 20 张才是下一个触发点。
	id, err := noteTaskDone(root, cfg, doneCard("crash20"))
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("第 20 张(距水位 10 满)应触发复盘卡")
	}
	if c := readCounter(t, root); c.TriggeredAt != 20 {
		t.Fatalf("触发后水位应推到 20, got %d", c.TriggeredAt)
	}
}

// TestRetroCounterClamp 手工编辑/回滚造成"水位 > 总数"时钳回，
// 否则差值恒为负、复盘永不再触发（静默失效）。
func TestRetroCounterClamp(t *testing.T) {
	root := retroTestRoot(t)
	cfg := retroTestConfig(2)
	if err := saveRetroCounter(root, &retroCounter{DoneTotal: 1, TriggeredAt: 99}); err != nil {
		t.Fatal(err)
	}
	// 钳到 1 后：本次 done 让总数变 2，距水位 1 张，未满 N=2。
	if got, err := noteTaskDone(root, cfg, doneCard("c1")); err != nil || got != "" {
		t.Fatalf("不应触发, got id=%q err=%v", got, err)
	}
	if c := readCounter(t, root); c.TriggeredAt != 1 || c.DoneTotal != 2 {
		t.Fatalf("水位应被钳到 1、总数 2, got %+v", c)
	}
	// 再来一张即满 N=2 → 触发。
	if got, err := noteTaskDone(root, cfg, doneCard("c2")); err != nil || got == "" {
		t.Fatalf("钳位后应能正常触发, got id=%q err=%v", got, err)
	}
}

// TestRetroDisabledStillCounts 关闭(0)时不入队但继续记账——开关打开那一刻就有历史基数可用。
func TestRetroDisabledStillCounts(t *testing.T) {
	root := retroTestRoot(t)
	cfg := retroTestConfig(0)
	for i := 0; i < 5; i++ {
		got, err := noteTaskDone(root, cfg, doneCard("z"))
		if err != nil {
			t.Fatal(err)
		}
		if got != "" {
			t.Fatalf("retro_every_n_done=0 时不得入队复盘卡, got %s", got)
		}
	}
	if c := readCounter(t, root); c.DoneTotal != 5 || c.TriggeredAt != 0 {
		t.Fatalf("关闭时仍应计数, got %+v", c)
	}
	if got := retroTasks(t, root); len(got) != 0 {
		t.Fatalf("关闭时不应有复盘卡, got %d", len(got))
	}
}

// TestRetroCardNotSelfCounted 复盘卡自身完成时不计数：否则 N 的含义从"N 张业务卡"
// 漂成"N-1 张业务卡 + 自己"，且 N=1 时会自我循环。
func TestRetroCardNotSelfCounted(t *testing.T) {
	root := retroTestRoot(t)
	cfg := retroTestConfig(2)

	self := &Task{ID: "r1", Type: typeProgressPull, Status: statusDone,
		ProgressKey: retroProgressKeyPrefix + "2", Prompts: []string{"p"}}
	if !isRetroTask(self) {
		t.Fatal("测试夹具不成立: 该卡应被识别为复盘卡")
	}
	for i := 0; i < 5; i++ {
		if got, err := noteTaskDone(root, cfg, self); err != nil || got != "" {
			t.Fatalf("复盘卡自身不应计数/触发, got id=%q err=%v", got, err)
		}
	}
	if _, err := os.Stat(retroCounterPath(root)); !os.IsNotExist(err) {
		t.Errorf("复盘卡自身完成不该写计数器, err=%v", err)
	}
	// 普通进度回收卡（非复盘键）照常计数，别把整类 progress-pull 误排除。
	plain := &Task{ID: "p1", Type: typeProgressPull, Status: statusDone, ProgressKey: "s-abc", Prompts: []string{"p"}}
	if isRetroTask(plain) {
		t.Fatal("普通进度回收卡被误判为复盘卡")
	}
	if _, err := noteTaskDone(root, cfg, plain); err != nil {
		t.Fatal(err)
	}
	if c := readCounter(t, root); c.DoneTotal != 1 {
		t.Fatalf("普通 progress-pull 卡应计数, got %+v", c)
	}
}

// TestRetroCardShape 钉死自动入队的复盘卡形态：类型/模型/落盘键/只读纪律。
// 【突变致死】把 -model 换掉、把 EmitProgress 去掉、把 ProgressKey 前缀改了，都会红。
func TestRetroCardShape(t *testing.T) {
	root := retroTestRoot(t)
	cfg := retroTestConfig(1)

	id, err := noteTaskDone(root, cfg, doneCard("x1"))
	if err != nil || id == "" {
		t.Fatalf("N=1 应立即触发, got id=%q err=%v", id, err)
	}
	card, err := loadTask(root, id)
	if err != nil {
		t.Fatalf("读复盘卡: %v", err)
	}
	if card.Type != typeProgressPull {
		t.Errorf("复盘卡类型 = %q, 应为 %q", card.Type, typeProgressPull)
	}
	if card.Model != "haiku" {
		t.Errorf("复盘卡模型 = %q, 应为 haiku(机械统计不烧贵模型)", card.Model)
	}
	if !card.EmitProgress {
		t.Error("复盘卡应开 EmitProgress —— 报告要落 progress/ 供人与监控 session 消费")
	}
	if card.ProgressKey != retroProgressKeyPrefix+"1" {
		t.Errorf("复盘卡 progress_key = %q, 应为 %q", card.ProgressKey, retroProgressKeyPrefix+"1")
	}
	if card.Dir != root {
		t.Errorf("复盘卡工作目录 = %q, 应为数据根 %q(数据源都在根下)", card.Dir, root)
	}
	if card.Status != statusQueued {
		t.Errorf("复盘卡状态 = %q, 应为 %q", card.Status, statusQueued)
	}
	if card.ReviewAfter {
		t.Error("复盘卡不该再配对抗复审(只读统计卡, 复审无收益)")
	}
	// 只读纪律：不得带任何写盘工具。
	for _, tool := range card.AllowedTools {
		if strings.HasPrefix(tool, "Write") || strings.HasPrefix(tool, "Edit") || strings.HasPrefix(tool, "MultiEdit") {
			t.Errorf("复盘卡带了写盘工具 %q —— proposal-only 纪律被破坏", tool)
		}
	}
	if card.SkipPermissions {
		t.Error("复盘卡不得 skip-permissions")
	}
	// prompt 必须已把模板占位符替换掉（否则执行器读到的是字面 {{ARCHIVE_DIR}}）。
	prompt := card.Prompts[0]
	for _, ph := range []string{"{{N}}", "{{ARCHIVE_DIR}}", "{{PROGRESS_DIR}}", "{{TASKS_DIR}}", "{{ROOT}}"} {
		if strings.Contains(prompt, ph) {
			t.Errorf("复盘 prompt 残留未替换的占位符 %s", ph)
		}
	}
	if !strings.Contains(prompt, archiveDir(root)) {
		t.Errorf("复盘 prompt 未注入归档目录 %s", archiveDir(root))
	}
	if !strings.Contains(prompt, "只读") {
		t.Error("复盘 prompt 缺少只读纪律声明")
	}
}

// TestRetroTemplateEmbedded 内置模板必须随二进制发布（新装机没有 templates/retro.md 也能触发复盘），
// 且必须带齐本代码注入的占位符与 proposal-only 纪律。
func TestRetroTemplateEmbedded(t *testing.T) {
	data, err := embeddedTemplates.ReadFile("templates/" + retroTemplate + ".md")
	if err != nil {
		t.Fatalf("内置复盘模板缺失: %v", err)
	}
	tpl := string(data)
	for _, ph := range []string{"{{N}}", "{{ARCHIVE_DIR}}", "{{PROGRESS_DIR}}", "{{TASKS_DIR}}"} {
		if !strings.Contains(tpl, ph) {
			t.Errorf("模板缺占位符 %s", ph)
		}
	}
	// 委托人点名的六项统计口径都得在模板里有落点。
	for _, must := range []string{"失败类", "修复轮数", "成本", "verdict", "改道", "建议"} {
		if !strings.Contains(tpl, must) {
			t.Errorf("模板缺统计口径: %s", must)
		}
	}
	if !strings.Contains(tpl, "不要改 config.json") {
		t.Error("模板缺 proposal-only 纪律(不得改配置)")
	}
	// writeDefaultTemplates 会把它铺到数据目录，供用户改写。
	root := t.TempDir()
	if err := writeDefaultTemplates(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(templatesDir(root), retroTemplate+".md")); err != nil {
		t.Errorf("init 未把复盘模板铺到数据目录: %v", err)
	}
}
