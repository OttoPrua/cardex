package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// BD-45 归组优先级与「未分类」兜底桶的回归测试。
//
// 每个用例都同时断言**结果名**与**判定来源**（projSource*）。只断言名字是不够的：
// 别名/模式/启发式经常给出同一个名字，优先级链被改错时用例照样绿——
// 来源是唯一能证明"这一层真的先命中了"的证据。

func pcard(dir, title string) *Task {
	return &Task{ID: "t-" + dir + "-" + title, Title: title, Dir: dir, Status: statusQueued}
}

// pbatch 是优先级链用例共用的一批卡。各目录的角色：
//
//	/work/Alpha          启发式项目 Alpha（3 张卡，够 minSoloProjectCards）
//	/work/Alpha-cmp      模式层：basename 以 "Alpha-" 开头；但它自己也够格成独立分量
//	                     （3 张卡）——故它能证明「模式 > 启发式」而不是两层碰巧同解
//	/lanes/x             别名层：/lanes/* → Alpha
//	/lanes/y             别名层指向 Gamma，且 basename 无模式特征
//	/work/Alpha-alias    别名层指向 Gamma，同时又命中模式（Alpha-）——证明「别名 > 模式」
//	/work/Solo           启发式（3 张卡，无任何规则命中）
//	/orphan/zzz          孤儿：1 张卡、单目录、无规则 → 未分类
func pbatch() []*Task {
	var ts []*Task
	for i := 0; i < 3; i++ {
		ts = append(ts, pcard("/work/Alpha", "a"+string(rune('0'+i))))
		ts = append(ts, pcard("/work/Alpha-cmp", "c"+string(rune('0'+i))))
		ts = append(ts, pcard("/work/Solo", "s"+string(rune('0'+i))))
	}
	ts = append(ts,
		pcard("/lanes/x", "lane x"),
		pcard("/lanes/y", "lane y"),
		pcard("/work/Alpha-alias", "alias 优先于模式"),
		pcard("/orphan/zzz", "孤儿卡"),
	)
	return ts
}

func pAliases() []boardProjectAlias {
	return []boardProjectAlias{
		{Match: "/lanes/x", Project: "Alpha"},
		{Match: "/lanes/y", Project: "Gamma"},
		{Match: "/work/Alpha-alias", Project: "Gamma"},
	}
}

func resolveOne(t *testing.T, res *projectResolver, tk *Task) (string, string) {
	t.Helper()
	name, src := res.resolve(tk)
	return name, src
}

func TestProjectPriorityChain(t *testing.T) {
	res := newProjectResolver(pbatch(), pAliases())

	cases := []struct {
		what     string
		task     *Task
		wantName string
		wantSrc  string
	}{
		{"显式 > 别名", &Task{Dir: "/lanes/x", Project: "Beta"}, "Beta", projSourceExplicit},
		{"显式 > 启发式", &Task{Dir: "/work/Alpha", Project: "Beta"}, "Beta", projSourceExplicit},
		{"别名 > 模式", pcard("/work/Alpha-alias", "x"), "Gamma", projSourceAlias},
		{"别名 > 启发式", pcard("/lanes/x", "x"), "Alpha", projSourceAlias},
		{"模式 > 启发式", pcard("/work/Alpha-cmp", "x"), "Alpha", projSourcePattern},
		{"启发式兜住无规则目录", pcard("/work/Solo", "x"), "Solo", projSourceHeuristic},
		{"都不中 → 未分类", pcard("/orphan/zzz", "x"), unclassifiedProject, projSourceUnclassified},
		{"无目录卡 → 未分类", &Task{Title: "无目录"}, unclassifiedProject, projSourceUnclassified},
	}
	for _, c := range cases {
		name, src := resolveOne(t, res, c.task)
		if name != c.wantName || src != c.wantSrc {
			t.Errorf("%s：dir=%q → (%q,%s)，want (%q,%s)",
				c.what, c.task.Dir, name, src, c.wantName, c.wantSrc)
		}
	}
}

// TestProjectExplicitIgnoresDirEntirely —— 显式字段连"目录压根没出现过"都要压过去。
func TestProjectExplicitIgnoresDirEntirely(t *testing.T) {
	res := newProjectResolver(pbatch(), pAliases())
	name, src := res.resolve(&Task{Dir: "D:/never/seen/before", Project: "  Alpha  "})
	if name != "Alpha" || src != projSourceExplicit {
		t.Fatalf("显式归组应 trim 后直接生效，got (%q,%s)", name, src)
	}
}

// TestProjectPatternNeedsKnownName —— 模式层只认「当批已知项目名」。
// 若 Alpha 自己都没站住脚（卡数不够），Alpha-cmp 就不该被拽进一个不存在的项目。
func TestProjectPatternNeedsKnownName(t *testing.T) {
	// Alpha 只有 1 张卡、单目录 → 不够格 → 不进已知名单
	ts := []*Task{pcard("/work/Alpha", "a")}
	for i := 0; i < 3; i++ {
		ts = append(ts, pcard("/work/Alpha-cmp", "c"+string(rune('0'+i))))
	}
	res := newProjectResolver(ts, nil)
	if name, src := res.resolve(pcard("/work/Alpha-cmp", "x")); name != "Alpha-cmp" || src != projSourceHeuristic {
		t.Fatalf("Alpha 未成项目时不得触发模式归组，got (%q,%s)", name, src)
	}
	// 别名表登记 Alpha 后，它立刻成为已知名，模式层随之生效。
	res = newProjectResolver(ts, []boardProjectAlias{{Match: "/nowhere", Project: "Alpha"}})
	if name, src := res.resolve(pcard("/work/Alpha-cmp", "x")); name != "Alpha" || src != projSourcePattern {
		t.Fatalf("别名登记的项目名应进已知名单，got (%q,%s)", name, src)
	}
}

// TestProjectPatternTakesLongestKnownName —— "Trading" 与 "Trading-docs" 同时已知时，
// Trading-docs-mirror 必须归给更长的那个，否则 Trading-docs 的子目录会被 Trading 吞掉。
func TestProjectPatternTakesLongestKnownName(t *testing.T) {
	var ts []*Task
	for i := 0; i < 3; i++ {
		n := string(rune('0' + i))
		ts = append(ts, pcard("/w/Trading", "t"+n), pcard("/w/Trading-docs", "d"+n))
	}
	res := newProjectResolver(ts, nil)
	if name, src := res.resolve(pcard("/w/Trading-docs-mirror", "x")); name != "Trading-docs" || src != projSourcePattern {
		t.Fatalf("最长已知名优先，got (%q,%s)", name, src)
	}
}

// TestProjectPatternWalksAncestors —— 日期工作树下面再挂子课题目录时，
// 证据在**祖先**的 basename 上（Trading-strategy-research-20260726/c-etf-regime）。
func TestProjectPatternWalksAncestors(t *testing.T) {
	var ts []*Task
	for i := 0; i < 3; i++ {
		ts = append(ts, pcard("/w/Trading", "t"+string(rune('0'+i))))
	}
	res := newProjectResolver(ts, nil)
	if name, src := res.resolve(pcard("/w/Trading-research-20260726/c-etf-regime", "x")); name != "Trading" || src != projSourcePattern {
		t.Fatalf("模式层应沿祖先链找证据，got (%q,%s)", name, src)
	}
	// 项目根自身（basename 恰等于已知名）不该被模式接管——它走启发式。
	if name, src := res.resolve(pcard("/w/Trading", "x")); src != projSourceHeuristic || name != "Trading" {
		t.Fatalf("项目根自身应走启发式，got (%q,%s)", name, src)
	}
}

// ---- 等值必须先于前缀（BD-45 R1·P1-1 的靶子）----
//
// 缺陷形态：'Trading-docs' 与更短的 'Trading' 同时是已知名时，前一版 matchPattern 只把
// **等长**的那个候选排除掉，循环继续走到 'Trading'，用 'Trading-' 前缀把 Trading-docs
// 的子目录/新根静默判给 Trading（来源仍显示 pattern，界面无从察觉）。
// 下面两个用例分别钉住「真祖先等值」与「链首等值」两条路径。

// TestProjectPatternEqualNameBeatsShorterPrefix —— 祖先 basename 与长已知名等值时，
// 必须归长的那个，绝不能被短已知名以前缀吞并。
func TestProjectPatternEqualNameBeatsShorterPrefix(t *testing.T) {
	var ts []*Task
	for i := 0; i < 3; i++ {
		n := string(rune('0' + i))
		ts = append(ts, pcard("/w/Trading", "t"+n), pcard("/w/Trading-docs", "d"+n))
	}
	sub := pcard("/w/Trading-docs/notes", "子目录卡")
	ts = append(ts, sub)
	res := newProjectResolver(ts, nil)

	name, src := res.resolve(sub)
	if name == "Trading" {
		t.Fatalf("Trading-docs 的子目录被短已知名 'Trading' 前缀吞并（来源 %s）", src)
	}
	if name != "Trading-docs" {
		t.Fatalf("/w/Trading-docs/notes 应归 Trading-docs，got (%q,%s)", name, src)
	}
}

// TestProjectPatternNewRootWithKnownNameNotSwallowed —— 目录**自身** basename 就等于已知名
// （未登记的新根，如跨机镜像 D:/Project/mirrors/Trading-docs）时：
//  1. 不得被更短的已知名吞并；
//  2. 交回启发式后，groupDirs 的「同名目录」证据把它并进正确的项目；
//  3. 连同名证据都没有时落「未分类」（可见）——而不是被静默判给隔壁项目。
func TestProjectPatternNewRootWithKnownNameNotSwallowed(t *testing.T) {
	var ts []*Task
	for i := 0; i < 3; i++ {
		n := string(rune('0' + i))
		ts = append(ts, pcard("/w/Trading", "t"+n), pcard("/w/Trading-docs", "d"+n))
	}
	mirror := pcard("D:/Project/mirrors/Trading-docs", "镜像上的卡")
	ts = append(ts, mirror)
	res := newProjectResolver(ts, nil)

	name, src := res.resolve(mirror)
	if name == "Trading" {
		t.Fatalf("未登记的新根被短已知名 'Trading' 吞并（来源 %s）", src)
	}
	if name != "Trading-docs" || src != projSourceHeuristic {
		t.Fatalf("同名目录证据应把新根并进 Trading-docs（启发式），got (%q,%s)", name, src)
	}

}

// TestProjectPatternDeclaredRootIsNotSwallowed —— 名字一旦被**声明**过（别名表登记或 -project 钉过），
// 它的新根与子目录都不得再被更短的已知名前缀吞掉，且两侧结论一致。
// 这是 matchPattern 注释里给出的规避出口，必须真的可用（否则那句注释就是空头支票）。
func TestProjectPatternDeclaredRootIsNotSwallowed(t *testing.T) {
	var ts []*Task
	for i := 0; i < 3; i++ {
		n := string(rune('0' + i))
		ts = append(ts, pcard("/w/Trading", "t"+n), pcard("/newroot/Trading-docs", "d"+n))
	}
	sub := pcard("/newroot/Trading-docs/notes", "子目录卡")
	ts = append(ts, sub)
	// 声明来源一：别名表里出现过这个项目名（match 指向别处，只贡献"这是个项目"这条信息）。
	res := newProjectResolver(ts, []boardProjectAlias{{Match: "/nowhere", Project: "Trading-docs"}})
	root := pcard("/newroot/Trading-docs", "根上的卡")
	if name, src := res.resolve(root); name != "Trading-docs" {
		t.Fatalf("已声明的项目根不得被 'Trading-' 吞并，got (%q,%s)", name, src)
	}
	if name, src := res.resolve(sub); name != "Trading-docs" {
		t.Fatalf("已声明项目的子目录应与根同项目，got (%q,%s)", name, src)
	}

	// 声明来源二：某张卡用 -project 钉过这个名字，效果相同。
	var ts2 []*Task
	for i := 0; i < 3; i++ {
		n := string(rune('0' + i))
		ts2 = append(ts2, pcard("/w/Trading", "t"+n), pcard("/newroot/Trading-docs", "d"+n))
	}
	pinned := pcard("D:/anywhere", "钉过的卡")
	pinned.Project = "Trading-docs"
	ts2 = append(ts2, pinned)
	res2 := newProjectResolver(ts2, nil)
	if name, src := res2.resolve(pcard("/newroot/Trading-docs", "x")); name != "Trading-docs" {
		t.Fatalf("-project 钉过的名字应同样免于前缀吞并，got (%q,%s)", name, src)
	}
}

// TestLaneSuffixesLongestFirst —— 同类位点：groupDirs 证据 (2) 的车道后缀表也是
// 「多候选、break 取首个」的匹配（boardmodel.go:421）。一旦某个后缀是另一个的真后缀
// （如将来补进 "-tree" 与 "-worktree"），短的排在前面就会先命中、把项目名截错。
// 这里钉住表的排序契约：**真后缀者必须排在被包含者之前**。
// 现表 {-lanes,-worktrees,-worktree,-lane,-wt} 两两互不为后缀，本用例当前是"守门"性质；
// 它杀死的突变是"未来新增/重排后缀时把短的写在前面"。
func TestLaneSuffixesLongestFirst(t *testing.T) {
	for i := 0; i < len(laneSuffixes); i++ {
		for j := i + 1; j < len(laneSuffixes); j++ {
			a, b := laneSuffixes[i], laneSuffixes[j]
			if len(a) < len(b) && strings.HasSuffix(b, a) {
				t.Errorf("laneSuffixes[%d]=%q 是 laneSuffixes[%d]=%q 的真后缀，短的排在前会先命中：应把 %q 提前",
					i, a, j, b, b)
			}
		}
	}
	// 行为桩：车道目录确实并进项目根（证明这条证据本身活着，不只是表的形状对）。
	var ts []*Task
	for i := 0; i < 3; i++ {
		ts = append(ts, pcard("/w/Proj", "p"+string(rune('0'+i))))
	}
	ts = append(ts, pcard("/w/Proj-lanes/a", "车道卡"))
	rep := groupDirs(ts)
	if rep["/w/Proj-lanes/a"] != rep["/w/Proj"] {
		t.Fatalf("车道目录应与项目根同分量，got %q vs %q", rep["/w/Proj-lanes/a"], rep["/w/Proj"])
	}
}

// TestGroupDirsPrefixNeedsSeparator —— 同类位点：groupDirs 证据 (4)「祖先包含」是启发式层里
// 另一处"短名可能吞长名"的入口。它现在靠**按 '/' 分段**取祖先（dirParent 链）来避免：
// '/w/Trading-docs' 的父目录是 '/w'，永远不会是 '/w/Trading'。
//
// 实测（本轮跑过的突变）：把 descendants 计数里的 HasPrefix(d, o+"/") 改成 HasPrefix(d, o)
// **杀不死**本用例——那行只喂容器护栏的计数，不参与并入判定；写注释时别把它当成防线。
// 真正被本用例杀死的突变：把这段父链遍历改写成对 observed 的裸前缀扫描
// （for o := range observed { if HasPrefix(d, o) { union } }）——一改即红。
func TestGroupDirsPrefixNeedsSeparator(t *testing.T) {
	ts := []*Task{
		pcard("/w/Trading", "a"),
		pcard("/w/Trading-docs", "b"),
		pcard("/w/Trading/sub", "c"), // 真子目录：这个才该并进去
	}
	rep := groupDirs(ts)
	if rep["/w/Trading-docs"] == rep["/w/Trading"] {
		t.Fatalf("同级的 Trading-docs 不得被当成 Trading 的子孙并入同一分量")
	}
	if rep["/w/Trading/sub"] != rep["/w/Trading"] {
		t.Fatalf("真子目录应并入父分量，got %q vs %q", rep["/w/Trading/sub"], rep["/w/Trading"])
	}
}

// TestSoloProjectCardThreshold —— 单目录分量的卡数门槛两侧各钉一根桩。
//
// 卡数刻意写**字面量 2 / 3** 而不是 minSoloProjectCards±1：用常量表达就成了自我循环，
// 门槛被改成 2 或 5 时用例跟着一起变，照样绿。门槛是对外契约（"第几张卡开始自己成项目"），
// 契约值必须由测试独立钉死。
func TestSoloProjectCardThreshold(t *testing.T) {
	mk := func(n int) *projectResolver {
		var ts []*Task
		for i := 0; i < n; i++ {
			ts = append(ts, pcard("/w/Duo", "d"+string(rune('0'+i))))
		}
		return newProjectResolver(ts, nil)
	}
	if name, src := mk(2).resolve(pcard("/w/Duo", "x")); src != projSourceUnclassified {
		t.Fatalf("单目录 2 张卡必须进收件箱，got (%q,%s)", name, src)
	}
	if name, src := mk(3).resolve(pcard("/w/Duo", "x")); name != "Duo" || src != projSourceHeuristic {
		t.Fatalf("单目录第 3 张卡起成项目，got (%q,%s)", name, src)
	}
}

// TestMirroredDirQualifiesWithOneCard —— 跨机镜像（review_dir）是强证据：
// 哪怕只有一张卡，两个互证目录也足以成项目，不该被门槛误杀。
func TestMirroredDirQualifiesWithOneCard(t *testing.T) {
	tk := pcard("/w/Twin", "唯一一张")
	tk.ReviewDir = "D:/mirror/Twin"
	res := newProjectResolver([]*Task{tk}, nil)
	if name, src := res.resolve(tk); name != "Twin" || src != projSourceHeuristic {
		t.Fatalf("有镜像互证的单卡项目不该进收件箱，got (%q,%s)", name, src)
	}
}

// ---- 别名规则的匹配语义 ----

func TestAliasBarePathIsExactNotPrefix(t *testing.T) {
	a := boardProjectAlias{Match: "/Users/me/Projects", Project: "未分类"}
	if !a.matchesDir("/Users/me/Projects") {
		t.Fatal("裸路径应命中目录自身")
	}
	// 这是防塌方护栏：容器目录写成裸路径时**不得**把它下面的所有项目一起吞掉。
	if a.matchesDir("/Users/me/Projects/TShare") {
		t.Fatal("裸路径不得做前缀语义（否则一条规则吞掉整块看板）")
	}
}

func TestAliasGlobCoversSubtreeAndIsCaseInsensitive(t *testing.T) {
	a := boardProjectAlias{Match: "D:/Project/PO-tasks/*", Project: "X"}
	for _, d := range []string{"D:/Project/PO-tasks/abc", "D:/Project/PO-tasks/abc/deep/er", "d:/project/po-tasks/ABC"} {
		if !a.matchesDir(d) {
			t.Errorf("glob 应命中 %s（含子孙、且大小写不敏感）", d)
		}
	}
	for _, d := range []string{"D:/Project/PO-tasks", "D:/Project/PO-lanes/abc"} {
		if a.matchesDir(d) {
			t.Errorf("glob 不该命中 %s", d)
		}
	}
}

func TestAliasTitleAndDirAreANDed(t *testing.T) {
	res := newProjectResolver(nil, []boardProjectAlias{
		{Match: "D:/tasks/*", Title: "Hermes", Project: "PerlicaHermes"},
	})
	if name, _ := res.resolve(pcard("D:/tasks/t1", "Hermes runtime truth R3")); name != "PerlicaHermes" {
		t.Fatalf("目录与标题都命中时应生效，got %q", name)
	}
	if name, _ := res.resolve(pcard("D:/tasks/t1", "Optimize registry")); name != unclassifiedProject {
		t.Fatalf("标题不命中即整条规则不命中，got %q", name)
	}
	if name, _ := res.resolve(pcard("D:/other/t1", "Hermes runtime truth")); name != unclassifiedProject {
		t.Fatalf("目录不命中即整条规则不命中（标题规则不得泄漏到全盘），got %q", name)
	}
}

func TestAliasBadRulesSkippedAndDisclosed(t *testing.T) {
	raw := []boardProjectAlias{
		{Match: "/w/ok", Project: "Good"},
		{Match: "/w/x"},                            // 缺 project
		{Project: "Nowhere"},                       // match 与 title 都缺
		{Match: "/w/[bad", Project: "BadGlob"},     // glob 语法错
		{Match: "/w/ok2", Project: "  AlsoGood  "}, // 前后空白应被 trim
	}
	rules, disc := parseProjectAliases(raw)
	if len(rules) != 2 {
		t.Fatalf("应只保留 2 条好规则，got %d 条：%+v", len(rules), rules)
	}
	if rules[1].Project != "AlsoGood" {
		t.Fatalf("project 应 trim，got %q", rules[1].Project)
	}
	for _, want := range []string{"#2", "#3", "#4", "3 条"} {
		if !strings.Contains(disc, want) {
			t.Fatalf("披露串必须点名被跳过的规则（缺 %q）：%s", want, disc)
		}
	}
	if disc == "" {
		t.Fatal("坏规则被静默跳过 = 造读数：必须有披露串")
	}
	// 好规则照常生效：坏规则不得连坐整张表。
	res := newProjectResolver([]*Task{pcard("/w/ok", "x")}, raw)
	if name, src := res.resolve(pcard("/w/ok", "x")); name != "Good" || src != projSourceAlias {
		t.Fatalf("坏规则不得让好规则失效，got (%q,%s)", name, src)
	}
}

func TestAliasFirstMatchWins(t *testing.T) {
	res := newProjectResolver(nil, []boardProjectAlias{
		{Match: "D:/t/*", Title: "Hermes", Project: "PerlicaHermes"},
		{Match: "D:/t/*", Project: "兜底"},
	})
	if name, _ := res.resolve(pcard("D:/t/a", "Hermes x")); name != "PerlicaHermes" {
		t.Fatalf("首条命中即用，got %q", name)
	}
	if name, _ := res.resolve(pcard("D:/t/a", "别的活")); name != "兜底" {
		t.Fatalf("首条不命中应继续往下，got %q", name)
	}
}

// ---- 快照层：兜底桶行为 ----

func bootProjectRoot(t *testing.T, boardJSON string, tasks ...*Task) string {
	t.Helper()
	root := testRoot(t)
	if err := saveConfig(root, defaultConfig("claude")); err != nil {
		t.Fatal(err)
	}
	for _, tk := range tasks {
		tk.ID = newID(root)
		tk.CreatedAt = fixedTime().Format("2006-01-02T15:04:05Z07:00")
		tk.UpdatedAt = tk.CreatedAt
		if err := saveTask(root, tk); err != nil {
			t.Fatal(err)
		}
	}
	if boardJSON != "" {
		if err := os.WriteFile(filepath.Join(root, "board.json"), []byte(boardJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func projectByID(snap *boardSnapshot, id string) *Project {
	for _, p := range snap.Projects {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// TestUnclassifiedAbsorbsOrphanDirs —— 负例：孤儿目录**不再各自成项目**，
// 而是统一进「未分类」。这是本卡要解决的"80 个野项目"问题的核心断言。
func TestUnclassifiedAbsorbsOrphanDirs(t *testing.T) {
	var ts []*Task
	for i := 0; i < 3; i++ {
		ts = append(ts, pcard("/w/Alpha", "a"+string(rune('0'+i))))
	}
	ts = append(ts,
		pcard("D:/Project/PO-tasks/t0730-1", "野卡一"),
		pcard("D:/Project/PO-tasks/t0730-2", "野卡二"),
		pcard("D:/tmp/oneoff-20260731", "野卡三"),
	)
	root := bootProjectRoot(t, "", ts...)
	snap, err := buildSnapshot(root, fixedTime())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Projects) != 2 {
		var got []string
		for _, p := range snap.Projects {
			got = append(got, p.ID)
		}
		t.Fatalf("3 个野目录应合并进 1 个收件箱（共 2 个项目），got %v", got)
	}
	bucket := projectByID(snap, unclassifiedProjectID)
	if bucket == nil {
		t.Fatal("必须有「未分类」项目")
	}
	if bucket.Name != unclassifiedProject {
		t.Fatalf("兜底桶名应为 %q，got %q", unclassifiedProject, bucket.Name)
	}
	if bucket.Stats.Total != 3 {
		t.Fatalf("三张野卡都应进桶，got %d", bucket.Stats.Total)
	}
	if len(bucket.Dirs) != 3 {
		t.Fatalf("桶里应列出全部待整理目录（可寻），got %v", bucket.Dirs)
	}
	if !strings.Contains(bucket.Desc, "待整理") {
		t.Fatalf("桶的介绍应说清语义（收件箱而非垃圾桶），got %q", bucket.Desc)
	}
}

// TestUnclassifiedMigratesOutAfterAliasRegistered —— 登记别名后**不动任何卡文件**，
// 下一次快照重建即把卡迁出收件箱。这是存量整理机制本身的验收。
func TestUnclassifiedMigratesOutAfterAliasRegistered(t *testing.T) {
	ts := []*Task{
		pcard("D:/Project/PO-tasks/t1", "Hermes runtime truth"),
		pcard("D:/Project/PO-tasks/t2", "Hermes receipt R2"),
	}
	root := bootProjectRoot(t, "", ts...)

	snap, err := buildSnapshot(root, fixedTime())
	if err != nil {
		t.Fatal(err)
	}
	if b := projectByID(snap, unclassifiedProjectID); b == nil || b.Stats.Total != 2 {
		t.Fatalf("登记前两张卡应在收件箱里，got %+v", b)
	}
	before := taskFileFingerprint(t, root)

	// 只改 board.json，一个字节的卡文件都不碰。
	if err := os.WriteFile(filepath.Join(root, "board.json"),
		[]byte(`{"project_aliases":[{"match":"D:/Project/PO-tasks/*","title":"Hermes","project":"PerlicaHermes"}]}`),
		0o644); err != nil {
		t.Fatal(err)
	}
	snap2, err := buildSnapshot(root, fixedTime())
	if err != nil {
		t.Fatal(err)
	}
	ph := projectByID(snap2, "perlica-hermes")
	if ph == nil || ph.Stats.Total != 2 {
		t.Fatalf("登记别名后两张卡应迁入 PerlicaHermes，got %+v", ph)
	}
	if b := projectByID(snap2, unclassifiedProjectID); b == nil || b.Stats.Total != 0 {
		t.Fatalf("迁出后收件箱应清空但仍在（恒显示），got %+v", b)
	}
	if after := taskFileFingerprint(t, root); after != before {
		t.Fatal("整理存量不得改动任何任务卡文件")
	}
}

// taskFileFingerprint 是 tasks/ 目录全部文件内容的指纹，用于证明"没动卡文件"。
func taskFileFingerprint(t *testing.T, root string) string {
	t.Helper()
	entries, err := os.ReadDir(tasksDir(root))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(tasksDir(root), e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString(e.Name())
		b.Write(data)
	}
	return b.String()
}

// TestUnclassifiedAlwaysPresent —— 一张野卡都没有时，收件箱仍要显示（空桶也是信息）。
func TestUnclassifiedAlwaysPresent(t *testing.T) {
	var ts []*Task
	for i := 0; i < 3; i++ {
		ts = append(ts, pcard("/w/Alpha", "a"+string(rune('0'+i))))
	}
	root := bootProjectRoot(t, "", ts...)
	snap, err := buildSnapshot(root, fixedTime())
	if err != nil {
		t.Fatal(err)
	}
	b := projectByID(snap, unclassifiedProjectID)
	if b == nil {
		t.Fatal("收件箱必须恒显示（哪怕 0 张卡）")
	}
	if b.Stats.Total != 0 || b.ProgressPercent != 0 {
		t.Fatalf("空桶不该有卡或进度，got %+v", b.Stats)
	}
}

// TestExplicitProjectMergesWithDerivedProject —— 显式钉的卡与目录推导出的卡
// 必须落进**同一个**项目，而不是两个同名项目。
func TestExplicitProjectMergesWithDerivedProject(t *testing.T) {
	var ts []*Task
	for i := 0; i < 3; i++ {
		ts = append(ts, pcard("/w/Alpha", "a"+string(rune('0'+i))))
	}
	pinned := pcard("D:/somewhere/else", "远端一次性目录上的 Alpha 卡")
	pinned.Project = "Alpha"
	ts = append(ts, pinned)

	root := bootProjectRoot(t, "", ts...)
	snap, err := buildSnapshot(root, fixedTime())
	if err != nil {
		t.Fatal(err)
	}
	p := projectByID(snap, "alpha")
	if p == nil {
		t.Fatal("找不到 alpha 项目")
	}
	if p.Stats.Total != 4 {
		t.Fatalf("显式钉的卡应并进同一个项目，got %d 张", p.Stats.Total)
	}
	if b := projectByID(snap, unclassifiedProjectID); b.Stats.Total != 0 {
		t.Fatalf("显式钉过的卡不该同时进收件箱，got %d", b.Stats.Total)
	}
}

// TestSnapshotSurfacesProjectAliasError —— 坏别名规则必须一路透到 /api/overview。
func TestSnapshotSurfacesProjectAliasError(t *testing.T) {
	root := bootProjectRoot(t, `{"project_aliases":[{"match":"/w/x"}]}`, pcard("/w/x", "卡"))
	snap, err := buildSnapshot(root, fixedTime())
	if err != nil {
		t.Fatal(err)
	}
	if snap.ProjectAliasError == "" {
		t.Fatal("坏别名规则必须挂到快照上供前端披露")
	}
	if !strings.Contains(snap.ProjectAliasError, "project_aliases") {
		t.Fatalf("披露串应指明来源，got %q", snap.ProjectAliasError)
	}
}

// ---- add -project 与软约束 ----

func TestAddPinsProjectAndWarnsOnUnclassified(t *testing.T) {
	root := testRoot(t)
	if err := saveConfig(root, defaultConfig("claude")); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	loadOnly := func() *Task {
		tasks, err := loadTasks(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) != 1 {
			t.Fatalf("应只有一张卡，got %d", len(tasks))
		}
		tk := tasks[0]
		if err := os.Remove(taskPath(root, tk.ID)); err != nil {
			t.Fatal(err)
		}
		return tk
	}

	if err := cmdAdd([]string{"-root", root, "-dir", work, "-project", "PerlicaHermes", "干活"}); err != nil {
		t.Fatal(err)
	}
	if tk := loadOnly(); tk.Project != "PerlicaHermes" {
		t.Fatalf("-project 应入队即钉，got %q", tk.Project)
	}

	if err := cmdAdd([]string{"-root", root, "-dir", work, "干活"}); err != nil {
		t.Fatal(err)
	}
	if tk := loadOnly(); tk.Project != "" {
		t.Fatalf("不带 -project 时字段应为空（走推导），got %q", tk.Project)
	}

	// 只有空白的 -project 是手误，不该被 trim 成"没指定"而静默放行。
	if err := cmdAdd([]string{"-root", root, "-dir", work, "-project", "   ", "干活"}); err == nil {
		t.Fatal("-project 只有空白应报错")
	}
}

// TestWarnIfUnclassifiedIsAdvisoryOnly —— 软约束绝不阻断，且判定必须与看板一致。
func TestWarnIfUnclassifiedIsAdvisoryOnly(t *testing.T) {
	root := bootProjectRoot(t, "", pcard("/w/orphan", "野卡"))
	// 不 panic、不改盘、无返回值：这里断言的是"能安全调用"这条契约。
	warnIfUnclassified(root, pcard("/w/orphan", "野卡"))
	warnIfUnclassified(root, &Task{Dir: "/w/orphan", Project: "Alpha"})

	res := newProjectResolver([]*Task{pcard("/w/orphan", "野卡")}, nil)
	if _, src := res.resolve(pcard("/w/orphan", "野卡")); src != projSourceUnclassified {
		t.Fatal("测试前置：这张卡本应判为未分类")
	}
}

// ---- 初始别名表（testdata/project_aliases_initial.json）----

// TestInitialAliasTableClassifiesRealInventory 用**实测盘点**里的代表目录/标题钉死初始
// 别名表的行为：任何一条规则被改错或删掉，这里必红。
//
// 这张表就是 ~/.cardex/board.json 里写入的那一份（同一个文件内容），
// 所以它既是回归测试，也是这份配置在仓库内的可复核副本。
func TestInitialAliasTableClassifiesRealInventory(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "project_aliases_initial.json"))
	if err != nil {
		t.Fatal(err)
	}
	var aliases []boardProjectAlias
	if err := json.Unmarshal(data, &aliases); err != nil {
		t.Fatalf("初始别名表必须是合法 JSON：%v", err)
	}
	if _, disc := parseProjectAliases(aliases); disc != "" {
		t.Fatalf("初始别名表不得有被跳过的规则：%s", disc)
	}

	// 合成一批"和真实账本同形"的卡：项目根各 3 张（让它们成为已知项目名），
	// 野目录各 1 张（正是要被规则收拢的那些）。
	var ts []*Task
	root3 := func(dir string) {
		for i := 0; i < 3; i++ {
			ts = append(ts, pcard(dir, "常规活"+string(rune('0'+i))))
		}
	}
	root3("/Users/ottoprua/Projects/PerlicaHermes")
	root3("/Users/ottoprua/Projects/PerlicaOptimize")
	root3("/Users/ottoprua/Projects/Trading")
	root3("/Users/ottoprua/Projects/TShare")
	root3("/Users/ottoprua/Projects/Trading-docs")
	root3("/Users/ottoprua/Projects/ClaudeGo")

	wild := []*Task{
		pcard("/Users/ottoprua/Projects/PH-lanes/u-snow", "车道活"),
		pcard("/Users/ottoprua/Projects/PH-lanes/g17-budget", "车道活"),
		pcard("D:/Project/PO-tasks/HB-R7-review", "重审: HB-KWRITE R7 ghost"),
		pcard("D:/Project/PO-tasks/S3-review-c3cef49", "重审: TrackA-S3 R3集成终审"),
		pcard("D:/Project/PO-tasks/card-g-r4-cleanup", "卡G R4: 撤回评估越权实现"),
		pcard("D:/Project/PO-tasks/t0730-1906-0429", "Hermes MVP runtime pin 生产阻塞最小迁移"),
		pcard("D:/Project/PO-tasks/t0727-0512-4f50", "TrackA-S3 R5: 合法旧迁移兼容"),
		pcard("D:/Project/PO-tasks/t0727-0714-2843", "Card A R4: provider full-file freshness"),
		pcard("D:/Project/PO-tasks/t0728-2046-3763", "Optimize model capability R1"),
		pcard("D:/Project/PO-worktrees/t0716-2144-5a5c", "修复R2: 落地 L1.4-c"),
		pcard("D:/tmp/qmt-guard-r4-20260729", "QMT 守护"),
		pcard("D:/tmp/4a-linuxdo-data-unblock-20260730", "4A 解封"),
		pcard("/Users/ottoprua/Projects/Trading-linuxdo-capacity-trend-20260729", "容量趋势"),
		pcard("/Users/ottoprua/Projects/Trading-strategy-research-20260726/c-etf-regime", "ETF 择时"),
		pcard("/Users/ottoprua/Projects/PerlicaHermes-cmp-sol", "对照跑"),
		pcard("D:/Project/PO-lanes/ClaudeGo", "远端车道"),
		pcard("/Users/ottoprua/Projects/cardex", "本仓活"),
		pcard("D:/Project/Trading-docs", "文档活"),
		pcard("/Users/ottoprua/Projects", "容器目录上的卡"),
		pcard("/tmp", "临时目录上的卡"),
		pcard("D:/Project/mirrors/PO-review-ln1", "一次性镜像"),
	}
	ts = append(ts, wild...)

	res := newProjectResolver(ts, aliases)
	want := map[string]string{
		"/Users/ottoprua/Projects/PH-lanes/u-snow":                                 "PerlicaHermes",
		"/Users/ottoprua/Projects/PH-lanes/g17-budget":                             "PerlicaHermes",
		"D:/Project/PO-tasks/HB-R7-review":                                         "PerlicaHermes",
		"D:/Project/PO-tasks/S3-review-c3cef49":                                    "PerlicaHermes",
		"D:/Project/PO-tasks/card-g-r4-cleanup":                                    "PerlicaHermes",
		"D:/Project/PO-tasks/t0730-1906-0429":                                      "PerlicaHermes",
		"D:/Project/PO-tasks/t0727-0512-4f50":                                      "PerlicaHermes",
		"D:/Project/PO-tasks/t0727-0714-2843":                                      "PerlicaHermes",
		"D:/Project/PO-tasks/t0728-2046-3763":                                      "PerlicaOptimize",
		"D:/Project/PO-worktrees/t0716-2144-5a5c":                                  "PerlicaOptimize",
		"D:/tmp/qmt-guard-r4-20260729":                                             "Trading",
		"D:/tmp/4a-linuxdo-data-unblock-20260730":                                  "Trading",
		"/Users/ottoprua/Projects/Trading-linuxdo-capacity-trend-20260729":         "Trading",
		"/Users/ottoprua/Projects/Trading-strategy-research-20260726/c-etf-regime": "Trading",
		"/Users/ottoprua/Projects/PerlicaHermes-cmp-sol":                           "PerlicaHermes",
		"D:/Project/PO-lanes/ClaudeGo":                                             "cardex",
		"/Users/ottoprua/Projects/ClaudeGo":                                        "cardex",
		"/Users/ottoprua/Projects/cardex":                                          "cardex",
		// 别名 > 模式的关键例外：Trading-docs 是独立项目，不能被 "Trading-" 模式吞掉。
		"D:/Project/Trading-docs":               "Trading-docs",
		"/Users/ottoprua/Projects/Trading-docs": "Trading-docs",
		// 容器与临时目录进收件箱。
		"/Users/ottoprua/Projects":         unclassifiedProject,
		"/tmp":                             unclassifiedProject,
		"D:/Project/mirrors/PO-review-ln1": unclassifiedProject,
		// 真实项目根照旧走启发式。
		"/Users/ottoprua/Projects/TShare": "TShare",
	}
	for _, tk := range ts {
		exp, ok := want[normDir(tk.Dir)]
		if !ok {
			continue
		}
		got, src := res.resolve(tk)
		if got != exp {
			t.Errorf("%s（%s）→ %q（来源 %s），want %q", tk.Dir, tk.Title, got, src, exp)
		}
	}

	// 容器规则**只**吃容器自身：它下面的项目一个都不能少。
	if got, _ := res.resolve(pcard("/Users/ottoprua/Projects/TShare", "x")); got != "TShare" {
		t.Fatalf("容器别名不得吞掉其下的项目，TShare → %q", got)
	}
}

// ---- 谱系层（lineage）与项目名折叠：2026-08-03 修委托人报的"孤卡自成一个项目" ----

// 派生卡跑在一次性目录上（复审分流的远端镜像、交叉腿的 scratchpad）时，目录天然没有归组
// 证据，此前一律落进收件箱。谱系层沿 review_of / emitted_by 上溯：一张审核卡审的是谁，
// 就属于谁的项目——这比猜目录强得多。
func TestLineageRescuesDerivedCardsFromInbox(t *testing.T) {
	var ts []*Task
	for i := 0; i < 3; i++ { // 够格的启发式项目 Alpha
		ts = append(ts, pcard("/work/Alpha", "a"+string(rune('0'+i))))
	}
	impl := ts[0]
	impl.ID = "t-impl"
	// 审核卡：跑在一次性镜像目录上，只有 review_of 指回实现卡。
	rv := &Task{ID: "t-review", Title: "审核: a0", Dir: "D:/mirrors/one-shot-1",
		Status: statusDone, ReviewOf: "t-impl"}
	// emit 产出：跑在另一个一次性目录，只有 emitted_by。
	em := &Task{ID: "t-emit", Title: "子卡", Dir: "D:/mirrors/one-shot-2",
		Status: statusQueued, EmittedBy: "t-impl"}
	ts = append(ts, rv, em)

	res := newProjectResolver(ts, nil)
	for _, c := range []*Task{rv, em} {
		name, src := resolveOne(t, res, c)
		if name != "Alpha" || src != projSourceLineage {
			t.Errorf("%s: got (%q,%q), want (Alpha,lineage)", c.ID, name, src)
		}
	}
}

// 谱系是**兜底**层：卡自身目录已有结论时不得被父卡的归属顶掉（那是关于这张卡的直接证据）。
func TestLineageDoesNotOverrideOwnEvidence(t *testing.T) {
	var ts []*Task
	for i := 0; i < 3; i++ {
		ts = append(ts, pcard("/work/Alpha", "a"+string(rune('0'+i))))
		ts = append(ts, pcard("/work/Beta", "b"+string(rune('0'+i))))
	}
	ts[0].ID = "t-alpha-impl"
	// 审核卡跑在 Beta 的目录里、父卡属于 Alpha：自身目录证据优先。
	rv := &Task{ID: "t-rv", Title: "审核: a0", Dir: "/work/Beta", Status: statusDone, ReviewOf: "t-alpha-impl"}
	ts = append(ts, rv)
	res := newProjectResolver(ts, nil)
	if name, src := resolveOne(t, res, rv); name != "Beta" || src != projSourceHeuristic {
		t.Fatalf("自身目录证据应优先于谱系: got (%q,%q)", name, src)
	}
}

// 多跳谱系（实现→审核→修复→审核）与断链/成环：都必须停得下来且不误判。
func TestLineageMultiHopAndBrokenChain(t *testing.T) {
	var ts []*Task
	for i := 0; i < 3; i++ {
		ts = append(ts, pcard("/work/Alpha", "a"+string(rune('0'+i))))
	}
	ts[0].ID = "t-impl"
	r1 := &Task{ID: "t-r1", Title: "审核", Dir: "D:/m/1", Status: statusDone, ReviewOf: "t-impl"}
	f1 := &Task{ID: "t-f1", Title: "修复R1", Dir: "D:/m/2", Status: statusDone, EmittedBy: "t-r1"}
	r2 := &Task{ID: "t-r2", Title: "审核R1", Dir: "D:/m/3", Status: statusQueued, ReviewOf: "t-f1"}
	// 断链：父卡已被 clean 清掉。
	orphan := &Task{ID: "t-orphan", Title: "审核", Dir: "D:/m/4", Status: statusDone, ReviewOf: "t-gone"}
	// 成环：账本是可手工编辑的文件，环不能把快照重建吊死。
	c1 := &Task{ID: "t-c1", Title: "环A", Dir: "D:/m/5", Status: statusDone, ReviewOf: "t-c2"}
	c2 := &Task{ID: "t-c2", Title: "环B", Dir: "D:/m/6", Status: statusDone, ReviewOf: "t-c1"}
	ts = append(ts, r1, f1, r2, orphan, c1, c2)

	res := newProjectResolver(ts, nil)
	for _, c := range []*Task{r1, f1, r2} {
		if name, src := resolveOne(t, res, c); name != "Alpha" || src != projSourceLineage {
			t.Errorf("%s 多跳上溯: got (%q,%q), want (Alpha,lineage)", c.ID, name, src)
		}
	}
	for _, c := range []*Task{orphan, c1, c2} {
		if name, src := resolveOne(t, res, c); name != unclassifiedProject || src != projSourceUnclassified {
			t.Errorf("%s 断链/成环应如实回落收件箱: got (%q,%q)", c.ID, name, src)
		}
	}
}

// 项目名折叠：slug 相同的名字合并成一个项目，显示名按权威度挑
// （别名声明 > 卡上显式钉 > 卡数多 > 字典序）。实测账本里 trading/Trading 与
// .cardex/cardex 就是这样各带哈希后缀裂成两格的。
func TestCanonicalProjectNamesFoldsSlugCollisions(t *testing.T) {
	cases := []struct {
		name    string
		names   []string
		count   map[string]int
		aliases []boardProjectAlias
		tasks   []*Task
		want    map[string]string // 输入名 → 归一后的名（未列出的表示不折叠）
	}{
		{
			name:  "大小写差异按卡数挑显示名",
			names: []string{"Trading", "trading"},
			count: map[string]int{"Trading": 147, "trading": 1},
			want:  map[string]string{"trading": "Trading"},
		},
		{
			name:  "前导点：.cardex 并入 cardex",
			names: []string{".cardex", "cardex"},
			count: map[string]int{".cardex": 7, "cardex": 139},
			want:  map[string]string{".cardex": "cardex"},
		},
		{
			name:    "别名表声明的名字胜过卡数更多的派生名",
			names:   []string{"Trading", "trading"},
			count:   map[string]int{"Trading": 147, "trading": 1},
			aliases: []boardProjectAlias{{Match: "/x", Project: "trading"}},
			want:    map[string]string{"Trading": "trading"},
		},
		{
			name:  "卡上显式钉的名字胜过纯派生名",
			names: []string{"Alpha", "alpha"},
			count: map[string]int{"Alpha": 9, "alpha": 2},
			tasks: []*Task{{ID: "t1", Project: "alpha"}},
			want:  map[string]string{"Alpha": "alpha"},
		},
		{
			name:  "slug 不同不折叠（Trading-docs 是独立项目）",
			names: []string{"Trading", "Trading-docs"},
			count: map[string]int{"Trading": 147, "Trading-docs": 64},
			want:  map[string]string{},
		},
		{
			name:  "收件箱不参与折叠",
			names: []string{unclassifiedProject, "未分类X"},
			count: map[string]int{unclassifiedProject: 6, "未分类X": 1},
			want:  map[string]string{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := canonicalProjectNames(c.names, c.count, c.aliases, c.tasks)
			if len(got) != len(c.want) {
				t.Fatalf("折叠条数不符: got %v, want %v", got, c.want)
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Errorf("%q → %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// 终态卡（已完成/已取消）与活跃卡同等参与归组——委托人点名"已取消、已完成也需要归并"。
// 这条钉住的是"终态不被排除"这个事实本身：若哪天有人给归组链加个 !t.terminal() 过滤，
// 一个做完的项目会整个从看板消失。
func TestTerminalCardsGroupLikeActiveOnes(t *testing.T) {
	var ts []*Task
	for i, st := range []string{statusDone, statusCanceled, statusDone} {
		c := pcard("/work/Gamma", "g"+string(rune('0'+i)))
		c.Status = st
		ts = append(ts, c)
	}
	res := newProjectResolver(ts, nil)
	for _, c := range ts {
		if name, src := resolveOne(t, res, c); name != "Gamma" || src != projSourceHeuristic {
			t.Errorf("%s(%s): got (%q,%q), want (Gamma,heuristic)", c.ID, c.Status, name, src)
		}
	}
}
