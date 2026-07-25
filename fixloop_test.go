package main

// 修复闭环(实现→对抗审核→自动修复)回归测试。
// 场景来自实战:审核 verdict=concerns 后循环断裂靠人肉续派——本闭环让 verdict 有了消费者。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testRoot(t *testing.T) string {
	root := t.TempDir()
	for _, d := range []string{"tasks", "archive", "logs", "templates"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func testCfg() *Config {
	return &Config{TypeDefaults: map[string]TypeDefaults{}}
}

func listQueued(t *testing.T, root string) []*Task {
	tasks, err := loadTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	return tasks
}

const reviewReport = "审查完毕。以下为对抗性审核报告。\n…分析若干…\n```json\n" +
	`{"verdict":"concerns","p0":["a.mjs:10 恒真门,注入反例不报红,修法:改真实断言"],"p1":["b.mjs:20 fail-open 吞错,修法:显式失败"],"p2":["小问题"],"summary":"两处承重缺陷"}` +
	"\n```\n"

func mkImplTask(t *testing.T, root string, cfg *Config) *Task {
	impl := newTask(root, cfg, typeSequence, "实现 任务甲", "/tmp/proj", []string{"实现甲。纪律:不 push。"}, 7)
	impl.ReviewAfter = true
	impl.Model = "claude-opus-4-8"
	impl.SkipPermissions = true
	if err := saveTask(root, impl); err != nil {
		t.Fatal(err)
	}
	return impl
}

func mkReviewTask(t *testing.T, root string, cfg *Config, impl *Task) *Task {
	rv := newTask(root, cfg, typeReview, "审核: "+impl.Title, impl.Dir, []string{"审"}, impl.Priority)
	rv.ReviewOf = impl.ID
	rv.FixRound = impl.FixRound
	if err := saveTask(root, rv); err != nil {
		t.Fatal(err)
	}
	return rv
}

func TestParseReviewVerdict(t *testing.T) {
	v := parseReviewVerdict(reviewReport)
	if v == nil || v.Verdict != "concerns" || len(v.P0) != 1 || len(v.P1) != 1 {
		t.Fatalf("verdict 解析失败: %+v", v)
	}
	// 未围栏形态(平衡扫描兜底)
	v2 := parseReviewVerdict(`结论 {"verdict":"pass","p0":[],"p1":[],"p2":[],"summary":"ok"} 完`)
	if v2 == nil || v2.Verdict != "pass" {
		t.Fatalf("未围栏 pass 解析失败: %+v", v2)
	}
	// 纯叙述→nil(旧格式兼容)
	if v3 := parseReviewVerdict("没有结论块的自由文本报告"); v3 != nil {
		t.Fatalf("纯叙述应返回 nil: %+v", v3)
	}
	// 非法 verdict 值→nil
	if v4 := parseReviewVerdict("```json\n{\"verdict\":\"maybe\"}\n```"); v4 != nil {
		t.Fatalf("非法 verdict 应 nil: %+v", v4)
	}
}

func TestFixLoopConcernsEmitsFixCard(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	impl := mkImplTask(t, root, cfg)
	rv := mkReviewTask(t, root, cfg, impl)
	handleReviewVerdict(root, cfg, rv, reviewReport, nil)

	var fix *Task
	for _, x := range listQueued(t, root) {
		if strings.HasPrefix(x.Title, "修复R1: ") {
			fix = x
		}
	}
	if fix == nil {
		t.Fatal("concerns 应自动入队修复R1卡")
	}
	if fix.FixRound != 1 || !fix.ReviewAfter || fix.Dir != impl.Dir || fix.Model != impl.Model || !fix.SkipPermissions {
		t.Fatalf("修复卡参数继承错误: %+v", fix)
	}
	if fix.Effort != "high" {
		t.Fatalf("修复卡 effort 应缺省抬到 high, got %q", fix.Effort)
	}
	p := fix.Prompts[0]
	for _, want := range []string{"P0-1: a.mjs:10", "P1-1: b.mjs:20", "按类闭合", "实现甲。纪律:不 push。", "第 1 轮"} {
		if !strings.Contains(p, want) {
			t.Fatalf("修复 prompt 缺少 %q\n----\n%s", want, p)
		}
	}
	if strings.Contains(p, "小问题") {
		t.Fatal("P2 不应进必修清单")
	}
}

func TestFixLoopPassStops(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	impl := mkImplTask(t, root, cfg)
	rv := mkReviewTask(t, root, cfg, impl)
	before := len(listQueued(t, root))
	passReport := "```json\n{\"verdict\":\"pass\",\"p0\":[],\"p1\":[],\"p2\":[],\"summary\":\"过\"}\n```"
	handleReviewVerdict(root, cfg, rv, passReport, nil)
	if len(listQueued(t, root)) != before {
		t.Fatal("pass 不应产生任何新卡（无 closeout 时）")
	}
}

func TestFixLoopPassEmitsCloseout(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	impl := mkImplTask(t, root, cfg)
	impl.Closeout = "把叶卡 X frontmatter status 改为 done 并记 _LOG"
	if err := saveTask(root, impl); err != nil {
		t.Fatal(err)
	}
	rv := mkReviewTask(t, root, cfg, impl)
	passReport := "```json\n{\"verdict\":\"pass\",\"p0\":[],\"p1\":[],\"p2\":[],\"summary\":\"过\"}\n```"
	handleReviewVerdict(root, cfg, rv, passReport, nil)
	var co *Task
	for _, x := range listQueued(t, root) {
		if strings.HasPrefix(x.Title, "收口: ") {
			co = x
		}
	}
	if co == nil {
		t.Fatal("带 closeout 的卡 pass 后应入队收口卡")
	}
	if co.Model != "haiku" || co.Prompts[0] != impl.Closeout || co.Dir != impl.Dir {
		t.Fatalf("收口卡参数错误: %+v", co)
	}
	if co.ReviewAfter {
		t.Fatal("收口卡不应再挂审核")
	}
}

func TestFixLoopCloseoutInheritsThroughFix(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	impl := mkImplTask(t, root, cfg)
	impl.Closeout = "写回 done"
	if err := saveTask(root, impl); err != nil {
		t.Fatal(err)
	}
	rv := mkReviewTask(t, root, cfg, impl)
	handleReviewVerdict(root, cfg, rv, reviewReport, nil) // concerns → 修复R1
	var fix *Task
	for _, x := range listQueued(t, root) {
		if strings.HasPrefix(x.Title, "修复R1: ") {
			fix = x
		}
	}
	if fix == nil || fix.Closeout != "写回 done" {
		t.Fatalf("修复卡应继承 closeout: %+v", fix)
	}
}

func TestFixLoopRoundLimitEscalates(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg() // MaxFixRounds=0 → 默认 3
	impl := mkImplTask(t, root, cfg)
	impl.Title = "修复R3: 实现 任务甲 [concerns:0P0+1P1]" // 第 3 轮修复卡
	impl.FixRound = 3
	impl.RemoteHost = "host-a" // 远端链：升级卡若不继承主机,远端 dir 会被派到本机 cd 失败
	if err := saveTask(root, impl); err != nil {
		t.Fatal(err)
	}
	rv := mkReviewTask(t, root, cfg, impl)
	handleReviewVerdict(root, cfg, rv, reviewReport, nil)

	var esc *Task
	for _, x := range listQueued(t, root) {
		if strings.Contains(x.Title, "超轮限") {
			esc = x
		}
	}
	if esc == nil {
		t.Fatal("R4 超默认轮限应挂升级卡")
	}
	if esc.Status != statusHeld {
		t.Fatalf("升级卡应为 held, got %s", esc.Status)
	}
	if esc.ReviewAfter {
		t.Fatal("升级卡不应再挂审核")
	}
	if esc.RemoteHost != "host-a" {
		t.Fatalf("升级卡应继承被审卡的 remote_host, got %q", esc.RemoteHost)
	}
	// 标题不嵌套:应剥掉"修复R3: "前缀与判定尾注
	if strings.Contains(esc.Title, "修复R3") || !strings.Contains(esc.Title, "实现 任务甲") {
		t.Fatalf("升级卡标题应用谱系根标题: %s", esc.Title)
	}
}

func TestFixLoopTitleNoNesting(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	impl := mkImplTask(t, root, cfg)
	impl.Title = "修复R1: 实现 任务甲 [block:2P0+6P1]"
	impl.FixRound = 1
	if err := saveTask(root, impl); err != nil {
		t.Fatal(err)
	}
	rv := mkReviewTask(t, root, cfg, impl)
	handleReviewVerdict(root, cfg, rv, reviewReport, nil)
	found := false
	for _, x := range listQueued(t, root) {
		if strings.HasPrefix(x.Title, "修复R2: 实现 任务甲 [concerns:1P0+1P1]") {
			found = true
			if strings.Count(x.Title, "修复R") != 1 {
				t.Fatalf("标题嵌套: %s", x.Title)
			}
		}
	}
	if !found {
		t.Fatal("应产出修复R2卡且标题干净")
	}
}

func TestFixLoopNoVerdictNoop(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	impl := mkImplTask(t, root, cfg)
	rv := mkReviewTask(t, root, cfg, impl)
	before := len(listQueued(t, root))
	handleReviewVerdict(root, cfg, rv, "自由文本报告,无 json 结论", nil)
	if len(listQueued(t, root)) != before {
		t.Fatal("无 verdict 时应静默跳过(兼容旧格式)")
	}
}

func TestFixLoopArchivedOrigStillWorks(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	impl := mkImplTask(t, root, cfg)
	rv := mkReviewTask(t, root, cfg, impl)
	// 被审卡被 clean 归档后闭环仍应工作
	if err := os.Rename(taskPath(root, impl.ID), filepath.Join(archiveDir(root), impl.ID+".json")); err != nil {
		t.Fatal(err)
	}
	handleReviewVerdict(root, cfg, rv, reviewReport, nil)
	found := false
	for _, x := range listQueued(t, root) {
		if strings.HasPrefix(x.Title, "修复R1: ") {
			found = true
		}
	}
	if !found {
		t.Fatal("被审卡已归档时闭环应仍工作(findTaskAnywhere)")
	}
}
