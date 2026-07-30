package main

// boardview_test.go —— 看板三项显示语义的验收：
//   [额度] 剩余口径     —— remaining_percent 由后端算并钳位，不让消费方各自做减法
//   [进度] 按性质拆分   —— 设计/落地/修复/审核/协调各报各的，总条口径不变
//   [归档] 项目折叠     —— 手动归档 + 有新卡自动复活 + 只写视图状态不动任务卡
//
// 反例注入是承重防线，不是"多写几个 case"：
//   ① 审核卡继承被审卡的 fix_round —— 判定顺序一旦写反，430 张审核卡会整体计进修复桶，
//      落地进度看着没变、修复桶凭空翻倍，而没有任何报错；
//   ② used_percent > 100 的样本真实存在 —— 不钳位就会算出负剩余，进度条渲染成 0 宽，
//      "已超额"被显示成"刚好耗尽"；
//   ③ 归档状态文件损坏 —— 静默当成"没归档"会让用户折叠的项目集体冒出来且零提示；
//   ④ 卡状态变化不得触发复活 —— 否则归档一个仍在跑的项目，下一次 tick 就自己弹回来。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- 辅助 ----

func kindOf(t *testing.T, tk *Task) kindMark {
	t.Helper()
	return deriveTaskKind(tk, nil)
}

func mkTask(id, title, typ string) *Task {
	return &Task{ID: id, Title: title, Type: typ, Status: statusDone}
}

// ================== 一、剩余额度口径 ==================

func TestRemainingPercentClampsBothEnds(t *testing.T) {
	cases := []struct {
		used, want float64
		why        string
	}{
		{0, 100, "一点没用 → 满额"},
		{31, 69, "常规样本"},
		{100, 0, "打满 → 归零"},
		{100.4, 0, "源数据略超 100（实测存在）→ 钳到 0，不得出负剩余"},
		{130, 0, "严重超额同样钳到 0"},
		{-5, 100, "负 used（脏样本）→ 钳到 100，不得出 >100 的剩余"},
	}
	for _, c := range cases {
		if got := remainingPercent(c.used); got != c.want {
			t.Errorf("remainingPercent(%v)=%v want %v（%s）", c.used, got, c.want, c.why)
		}
	}
}

// TestBurnSourceCarriesRemainingPercent —— 契约：remaining_percent 必须真的进 JSON。
// 前端拿不到这个键会回落到自己做 100−used 的兜底，钳位逻辑就绕过去了。
func TestBurnSourceCarriesRemainingPercent(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	samples := []rawSample{
		{at: now.Add(-40 * time.Minute), pct: 20, resetsAt: "2026-07-26T17:00:00Z"},
		{at: now.Add(-10 * time.Minute), pct: 77.5, resetsAt: "2026-07-26T17:00:00Z"},
	}
	src := buildBurnSource("s1", "claude", "acct", "账号", "session", "5 小时", 300,
		samples, now, 90*time.Minute)
	if src == nil {
		t.Fatal("样本齐全时不该返回 nil")
	}
	if src.UsedPercent != 77.5 {
		t.Fatalf("used_percent 不该被改动：got %v", src.UsedPercent)
	}
	if src.RemainingPercent != 22.5 {
		t.Fatalf("remaining_percent 应为 22.5，got %v", src.RemainingPercent)
	}
	b, _ := json.Marshal(src)
	if !strings.Contains(string(b), `"remaining_percent":22.5`) {
		t.Fatalf("JSON 契约缺 remaining_percent：%s", string(b))
	}
	// used_percent 保留不动：它是 CodexBar 的原始读数，改它会连坐所有历史消费方。
	if !strings.Contains(string(b), `"used_percent":77.5`) {
		t.Fatalf("used_percent 必须原样保留：%s", string(b))
	}
}

// ================== 二、工作性质分类 ==================

// TestKindReviewBeatsFix —— 承重反例：审核卡会继承被审卡的 fix_round，
// 判定顺序写反就会把审核卡整体计进修复桶（实测某项目 430 张审核卡）。
func TestKindReviewBeatsFix(t *testing.T) {
	tk := mkTask("t1", "审核: 修复R3: TA-3 WAL 窗口读路径P0", typeReview)
	tk.ReviewOf = "t0"
	tk.FixRound = 3
	got := kindOf(t, tk)
	if got.Kind != kindReview {
		t.Fatalf("带 fix_round 的审核卡必须判成审核，got %+v", got)
	}
	if got.Source != "review_of" {
		t.Fatalf("判定来源应是 review_of（盘上事实优先于标题），got %q", got.Source)
	}
}

func TestKindStructuralSignals(t *testing.T) {
	xc := mkTask("t-x", "交叉查漏", typeSequence)
	xc.XRole = "C"
	fixCard := mkTask("t-f", "修复R1: 门括号轴按类闭合", typeSequence)
	fixCard.FixRound = 1

	cases := []struct {
		name       string
		task       *Task
		wantKind   string
		wantSource string
	}{
		{"审核类型", mkTask("t-a", "任意标题", typeReview), kindReview, "type"},
		{"交叉链 C 卡", xc, kindReview, "x_role"},
		{"审核标题前缀", mkTask("t-b", "审核: OC-ARC-R5 门括号轴", typeSequence), kindReview, "title"},
		{"对抗复审前缀", mkTask("t-c", "对抗复审: OC-ARC-R5 残余三P1", typeSequence), kindReview, "title"},
		{"修复轮次字段", fixCard, kindFix, "fix_round"},
		{"修复标题前缀", mkTask("t-g", "修复R2: 哑死P0+契约申报收口", typeSequence), kindFix, "title"},
		{"协调类型", mkTask("t-h", "分工", typeCoordinate), kindCoord, "type"},
		{"进度回收类型", mkTask("t-i", "拉进度", typeProgressPull), kindCoord, "type"},
		{"收口前缀", mkTask("t-j", "收口: OC-ARC-R5 门括号轴", typeSequence), kindCoord, "title"},
		{"设计关键词", mkTask("t-k", "HB-KWRITE 知识写入合同[设计·重写自 t0717]", typeSequence), kindDesign, "title"},
		{"英文设计关键词", mkTask("t-l", "H2 control-plane RFC draft", typeSequence), kindDesign, "title"},
		{"兜底落地", mkTask("t-m", "TA-3 R4 WAL 窗口读路径P0+站点登记表P1", typeSequence), kindImpl, "default"},
	}
	for _, c := range cases {
		got := deriveTaskKind(c.task, nil)
		if got.Kind != c.wantKind || got.Source != c.wantSource {
			t.Errorf("%s：got %+v，want kind=%s source=%s", c.name, got, c.wantKind, c.wantSource)
		}
	}
}

// TestKindFixNeedsColon —— 反例：不带冒号的"修复"在正文里太常见，
// 放宽成任意位置匹配会把实现卡整片吃进修复桶。
func TestKindFixNeedsColon(t *testing.T) {
	tk := mkTask("t-n", "OC-ARC-R5 恒真门修复方案落地", typeSequence)
	if got := deriveTaskKind(tk, nil); got.Kind == kindFix {
		t.Fatalf("正文里的「修复」二字不构成修复卡，got %+v", got)
	}
}

func TestKindOverrideRules(t *testing.T) {
	rules := []boardKindRule{
		{Match: "HB-", Kind: kindDesign},
		{Match: "t-exact", Kind: kindCoord},
	}
	byTitle := deriveTaskKind(mkTask("t1", "HB-CRON 调度唯一意图源", typeSequence), rules)
	if byTitle.Kind != kindDesign || byTitle.Source != "override" {
		t.Fatalf("标题子串规则未生效：%+v", byTitle)
	}
	byID := deriveTaskKind(mkTask("t-exact", "毫无关系的标题", typeSequence), rules)
	if byID.Kind != kindCoord || byID.Source != "override" {
		t.Fatalf("任务 ID 规则未生效：%+v", byID)
	}
	// 人工规则优先于**所有**自动判定，包括最强的 review_of 结构信号——
	// 否则"我明知这张审核卡该算设计"的意图会被结构信号无声否决。
	rv := mkTask("t2", "HB-REVIEW 某审核", typeReview)
	rv.ReviewOf = "t0"
	if got := deriveTaskKind(rv, rules); got.Kind != kindDesign {
		t.Fatalf("人工规则应盖过 review_of，got %+v", got)
	}
}

// TestParseKindRulesSkipsBadOnesAndDiscloses —— 坏规则逐条跳过而非整块拒，
// 但被跳过的必须出现在披露串里：静默失效即造读数。
func TestParseKindRulesSkipsBadOnesAndDiscloses(t *testing.T) {
	raw := []boardOverrideKindRule{
		{Match: "好规则", Kind: kindDesign},
		{Match: "  ", Kind: kindImpl},
		{Match: "坏 kind", Kind: "设计"},
	}
	rules, msg := parseKindRules(raw)
	if len(rules) != 1 || rules[0].Match != "好规则" {
		t.Fatalf("合法规则应保留且只保留一条，got %+v", rules)
	}
	if msg == "" {
		t.Fatal("被跳过的规则必须披露，不得静默")
	}
	for _, want := range []string{"第2条", "第3条", "设计"} {
		if !strings.Contains(msg, want) {
			t.Errorf("披露串应包含 %q：%s", want, msg)
		}
	}
}

// ---- 分桶聚合 ----

func TestBuildKindProgressBucketsAndOrder(t *testing.T) {
	mk := func(id, title, typ, status string) *Task {
		return &Task{ID: id, Title: title, Type: typ, Status: status}
	}
	ts := []*Task{
		mk("d1", "架构方案定稿", typeSequence, statusDone),
		mk("i1", "L1.2 落地读路径", typeSequence, statusDone),
		mk("i2", "L1.3 落地写路径", typeSequence, statusQueued),
		mk("i3", "L1.4 落地索引", typeSequence, statusCanceled),
		mk("f1", "修复R1: L1.2 读路径", typeSequence, statusDone),
		mk("r1", "审核: L1.2 读路径", typeReview, statusDone),
		mk("r2", "审核: L1.3 写路径", typeReview, statusDone),
		mk("c1", "分工", typeCoordinate, statusDone),
	}
	marks := map[string]kindMark{}
	for _, tk := range ts {
		marks[tk.ID] = deriveTaskKind(tk, nil)
	}
	kinds := buildKindProgress(ts, marks)

	// 顺序固定为工作流自然序，而不是按张数——按张数排会让审核桶常年霸占第一行。
	var gotOrder []string
	for _, k := range kinds {
		gotOrder = append(gotOrder, k.Key)
	}
	want := []string{kindDesign, kindImpl, kindFix, kindReview, kindCoord}
	if strings.Join(gotOrder, ",") != strings.Join(want, ",") {
		t.Fatalf("分桶顺序错：got %v want %v", gotOrder, want)
	}

	byKey := map[string]KindProgress{}
	for _, k := range kinds {
		byKey[k.Key] = k
	}
	// 落地桶：3 张（1 done / 1 queued / 1 canceled），分母排除取消卡 → 1/2 = 50%
	impl := byKey[kindImpl]
	if impl.Stats.Total != 3 || impl.Stats.Canceled != 1 {
		t.Fatalf("落地桶计数错：%+v", impl.Stats)
	}
	if impl.ProgressPercent != 50 {
		t.Fatalf("落地桶进度应为 50（分母排除取消卡），got %v", impl.ProgressPercent)
	}
	if byKey[kindReview].ProgressPercent != 100 || byKey[kindReview].Stats.Total != 2 {
		t.Fatalf("审核桶应 2/2=100%%：%+v", byKey[kindReview])
	}
	// 桶内张数之和必须等于总张数：漏一张就意味着某类活在界面上凭空消失。
	sum := 0
	for _, k := range kinds {
		sum += k.Stats.Total
	}
	if sum != len(ts) {
		t.Fatalf("各桶张数之和 %d ≠ 总张数 %d", sum, len(ts))
	}
	if byKey[kindImpl].Label != "落地" || byKey[kindReview].Label != "审核" {
		t.Fatal("桶标签应为中文人话")
	}
}

// TestBuildKindProgressOmitsEmptyBuckets —— 从没派过修复卡的项目不该显示"修复 0/0"。
func TestBuildKindProgressOmitsEmptyBuckets(t *testing.T) {
	ts := []*Task{{ID: "i1", Title: "落地一步", Type: typeSequence, Status: statusDone}}
	marks := map[string]kindMark{"i1": deriveTaskKind(ts[0], nil)}
	kinds := buildKindProgress(ts, marks)
	if len(kinds) != 1 || kinds[0].Key != kindImpl {
		t.Fatalf("只有落地卡时应只回吐落地一桶，got %+v", kinds)
	}
}

// ---- 快照级：拆分不改总条 ----

// bootKindRoot 造一个含设计/落地/修复/审核四类卡的项目。
func bootKindRoot(t *testing.T, boardJSON string) string {
	t.Helper()
	root := testRoot(t)
	if err := saveConfig(root, defaultConfig("claude")); err != nil {
		t.Fatal(err)
	}
	mk := func(typ, title, status string, mut func(*Task)) {
		tk := newTask(root, testCfg(), typ, title, "/tmp/kindproj", []string{"干活"}, 5)
		tk.Status = status
		if mut != nil {
			mut(tk)
		}
		if err := saveTask(root, tk); err != nil {
			t.Fatal(err)
		}
	}
	mk(typeSequence, "L1 架构方案定稿", statusDone, nil)
	mk(typeSequence, "L1.2 落地读路径", statusDone, nil)
	mk(typeSequence, "L1.3 落地写路径", statusQueued, nil)
	mk(typeSequence, "修复R1: L1.2 读路径", statusDone, func(tk *Task) { tk.FixRound = 1 })
	mk(typeReview, "审核: L1.2 读路径", statusDone, func(tk *Task) { tk.ReviewOf = "zzz" })
	if boardJSON != "" {
		if err := os.WriteFile(filepath.Join(root, "board.json"), []byte(boardJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func findProject(t *testing.T, snap *boardSnapshot, id string) *Project {
	t.Helper()
	for _, p := range snap.Projects {
		if p.ID == id {
			return p
		}
	}
	var ids []string
	for _, p := range snap.Projects {
		ids = append(ids, p.ID)
	}
	t.Fatalf("找不到项目 %s，现有：%v", id, ids)
	return nil
}

// TestSnapshotKindsPresentAndTotalUnchanged —— 拆分是"只加不改"：
// 总条 progress_percent / stats 必须与拆分前一字不差，否则历史读数全部失去可比性。
func TestSnapshotKindsPresentAndTotalUnchanged(t *testing.T) {
	root := bootKindRoot(t, "")
	snap, err := buildSnapshot(root, fixedTime())
	if err != nil {
		t.Fatal(err)
	}
	p := findProject(t, snap, "kindproj")

	// 5 张卡、4 张 done → 总条 80%
	if p.Stats.Total != 5 || p.Stats.Done != 4 {
		t.Fatalf("前置计数不符：%+v", p.Stats)
	}
	if p.ProgressPercent != 80 {
		t.Fatalf("总条口径必须不变（4/5=80），got %v", p.ProgressPercent)
	}
	byKey := map[string]KindProgress{}
	for _, k := range p.Kinds {
		byKey[k.Key] = k
	}
	// 这正是本卡要暴露的分布：审核/修复 100%，落地只有 50%，而总条是 80%。
	if byKey[kindImpl].ProgressPercent != 50 {
		t.Fatalf("落地桶应 1/2=50%%：%+v", byKey[kindImpl])
	}
	if byKey[kindReview].ProgressPercent != 100 || byKey[kindFix].ProgressPercent != 100 {
		t.Fatalf("审核/修复桶应 100%%：review=%+v fix=%+v", byKey[kindReview], byKey[kindFix])
	}
	if byKey[kindDesign].Stats.Total != 1 {
		t.Fatalf("设计桶应有 1 张：%+v", byKey[kindDesign])
	}
	// 单卡也要带 kind + kind_source：只发 kind 会让"盘上事实"与"关键词猜的"长得一样。
	found := false
	for _, ph := range p.Phases {
		for _, task := range ph.Tasks {
			if task.Kind == "" || task.KindSource == "" {
				t.Fatalf("任务 %s 缺 kind/kind_source：%+v", task.ID, task)
			}
			if task.Kind == kindReview && task.KindSource == "review_of" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("审核卡应带 kind_source=review_of")
	}
	b, _ := json.Marshal(p)
	if !strings.Contains(string(b), `"kinds":`) {
		t.Fatalf("JSON 契约缺 kinds：%s", string(b))
	}
}

func TestSnapshotKindRulesFromBoardJSON(t *testing.T) {
	root := bootKindRoot(t, `{
  "projects": {
    "kindproj": {
      "kind_rules": [
        {"match":"L1.3","kind":"design"},
        {"match":"坏的","kind":"不存在的类"}
      ]
    }
  }
}`)
	snap, err := buildSnapshot(root, fixedTime())
	if err != nil {
		t.Fatal(err)
	}
	p := findProject(t, snap, "kindproj")
	if p.KindRuleError == "" {
		t.Fatal("非法 kind 的规则必须披露")
	}
	// 合法规则仍生效：一条写错不连坐吃掉整个列表。
	var hit bool
	for _, ph := range p.Phases {
		for _, task := range ph.Tasks {
			if strings.Contains(task.Title, "L1.3") {
				hit = true
				if task.Kind != kindDesign || task.KindSource != "override" {
					t.Fatalf("人工规则未生效：%+v", task)
				}
			}
		}
	}
	if !hit {
		t.Fatal("没找到 L1.3 那张卡")
	}
}

// ================== 三、项目归档 ==================

func newTestBoardServer(t *testing.T, root string, now time.Time) *boardServer {
	t.Helper()
	// TTL=0 关掉快照缓存：clock 被钉成固定时刻，任何正数 TTL 都会让缓存永不过期
	// （now.Sub(at) 恒为 0），后续新增的任务卡就永远进不了快照。
	s := newBoardServer(root, 0)
	s.clock = func() time.Time { return now }
	return s
}

func postArchive(t *testing.T, s *boardServer, body string, mut func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/project/archive", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if mut != nil {
		mut(req)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec
}

func getOverview(t *testing.T, s *boardServer) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/overview 应 200，got %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("总览响应不是合法 JSON: %v", err)
	}
	return out
}

func projectFromOverview(t *testing.T, ov map[string]any, id string) map[string]any {
	t.Helper()
	projects, _ := ov["projects"].([]any)
	for _, raw := range projects {
		p, _ := raw.(map[string]any)
		if p != nil && p["id"] == id {
			return p
		}
	}
	t.Fatalf("总览里找不到项目 %s", id)
	return nil
}

func TestArchiveRoundTrip(t *testing.T) {
	root := bootKindRoot(t, "")
	now := fixedTime()
	s := newTestBoardServer(t, root, now)

	if p := projectFromOverview(t, getOverview(t, s), "kindproj"); p["archived"] != false {
		t.Fatalf("默认不该是归档态：%v", p["archived"])
	}

	rec := postArchive(t, s, `{"id":"kindproj","archived":true}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("归档应 200，got %d: %s", rec.Code, rec.Body.String())
	}
	ov := getOverview(t, s)
	p := projectFromOverview(t, ov, "kindproj")
	if p["archived"] != true {
		t.Fatalf("归档后 archived 应为 true：%v", p["archived"])
	}
	if p["archived_at"] == nil || p["archived_at"] == "" {
		t.Fatal("归档后应带 archived_at")
	}
	if n, _ := ov["archived_count"].(float64); n != 1 {
		t.Fatalf("archived_count 应为 1，got %v", ov["archived_count"])
	}

	// 归档**不得**改任何任务卡：状态、更新时间一个字节都不动。
	tasks, err := loadTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range tasks {
		if tk.Status == "" {
			t.Fatalf("任务 %s 被写坏了", tk.ID)
		}
	}

	rec = postArchive(t, s, `{"id":"kindproj","archived":false}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("取消归档应 200，got %d: %s", rec.Code, rec.Body.String())
	}
	if p := projectFromOverview(t, getOverview(t, s), "kindproj"); p["archived"] != false {
		t.Fatalf("取消归档后 archived 应为 false：%v", p["archived"])
	}
	// 取消归档应把记录彻底删掉，而不是留一条 archived=false 的僵尸记录。
	f, err := loadBoardArchive(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Projects["kindproj"]; ok {
		t.Fatal("取消归档后状态文件里不该还留着该项目的记录")
	}
}

// TestArchiveSurvivesStatusChange —— 承重反例：卡状态变化（queued→done）**不得**触发复活。
// 手动归档表达的是"这个项目我暂时不看了"，已知卡跑完并不构成"有新东西要看"；
// 若按 updated_at 判复活，归档一个仍在跑的项目下一次 tick 就会自己弹回来。
func TestArchiveSurvivesStatusChange(t *testing.T) {
	root := bootKindRoot(t, "")
	now := fixedTime()
	s := newTestBoardServer(t, root, now)
	if rec := postArchive(t, s, `{"id":"kindproj","archived":true}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("归档失败：%s", rec.Body.String())
	}

	tasks, err := loadTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	changed := false
	for _, tk := range tasks {
		if tk.Status == statusQueued {
			tk.Status = statusDone
			tk.touch()
			if err := saveTask(root, tk); err != nil {
				t.Fatal(err)
			}
			changed = true
		}
	}
	if !changed {
		t.Fatal("测试前置：应有一张 queued 卡可改")
	}

	p := projectFromOverview(t, getOverview(t, s), "kindproj")
	if p["archived"] != true {
		t.Fatalf("仅状态变化不该复活归档态：%v", p["archived"])
	}
	if p["archive_revived"] == true {
		t.Fatal("仅状态变化不该标记为自动复活")
	}
}

// TestArchiveRevivesOnNewCard —— 有新卡即自动切回活跃，并说明原因。
func TestArchiveRevivesOnNewCard(t *testing.T) {
	root := bootKindRoot(t, "")
	now := fixedTime()
	s := newTestBoardServer(t, root, now)
	if rec := postArchive(t, s, `{"id":"kindproj","archived":true}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("归档失败：%s", rec.Body.String())
	}

	// 这张卡走的是「张数变多」这条判据（created_at 用 newTask 的默认值即可）。
	fresh := newTask(root, testCfg(), typeSequence, "L1.5 新落地卡", "/tmp/kindproj", []string{"干活"}, 5)
	if err := saveTask(root, fresh); err != nil {
		t.Fatal(err)
	}

	ov := getOverview(t, s)
	p := projectFromOverview(t, ov, "kindproj")
	if p["archived"] != false {
		t.Fatalf("有新卡应自动切回活跃：%v", p["archived"])
	}
	if p["archive_revived"] != true {
		t.Fatal("自动复活必须标记出来，否则用户会以为自己没点上归档")
	}
	if s, _ := p["archive_revived_reason"].(string); s == "" {
		t.Fatal("自动复活必须给出原因文案")
	}
	// 归档时刻要保留：它是"何时归的档"这一事实，复活不该把它抹掉。
	if p["archived_at"] == nil || p["archived_at"] == "" {
		t.Fatal("复活后仍应保留 archived_at")
	}
	if n, _ := ov["archived_count"].(float64); n != 0 {
		t.Fatalf("复活后 archived_count 应为 0，got %v", ov["archived_count"])
	}
}

// TestArchiveRevivesOnSwapNoCountChange —— 删一张加一张：张数没变但出现了更新的 created_at，
// 只看张数会被骗过去，必须靠 max_created_at 这条判据兜住。
func TestArchiveRevivesOnSwapNoCountChange(t *testing.T) {
	root := bootKindRoot(t, "")
	now := fixedTime()
	s := newTestBoardServer(t, root, now)
	if rec := postArchive(t, s, `{"id":"kindproj","archived":true}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("归档失败：%s", rec.Body.String())
	}
	tasks, err := loadTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(taskPath(root, tasks[0].ID)); err != nil {
		t.Fatal(err)
	}
	// created_at 必须真的晚于既有卡（既有卡用的是 newTask 的真实时钟，不是 fixedTime），
	// 否则这条 case 测的就不是"更新的卡"而是"更旧的卡"。
	fresh := newTask(root, testCfg(), typeSequence, "L1.6 换进来的新卡", "/tmp/kindproj", []string{"干活"}, 5)
	fresh.CreatedAt = time.Now().Add(time.Hour).Format(time.RFC3339)
	if err := saveTask(root, fresh); err != nil {
		t.Fatal(err)
	}
	if p := projectFromOverview(t, getOverview(t, s), "kindproj"); p["archived"] != false {
		t.Fatalf("张数不变但有更新的卡，应复活：%v", p["archived"])
	}
}

// TestArchiveStateCorruptIsDisclosed —— 状态文件损坏时必须落错披露，
// 而不是静默当成"没有任何项目被归档"（那会让用户折叠的项目集体冒出来且零提示）。
func TestArchiveStateCorruptIsDisclosed(t *testing.T) {
	root := bootKindRoot(t, "")
	if err := os.WriteFile(boardArchivePath(root), []byte("{ 这不是 JSON"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newTestBoardServer(t, root, fixedTime())
	ov := getOverview(t, s)
	msg, _ := ov["archive_state_error"].(string)
	if msg == "" {
		t.Fatal("归档状态读失败必须挂 archive_state_error")
	}
	if !strings.Contains(msg, "board_archive.json") {
		t.Fatalf("披露串应指明是哪个文件：%s", msg)
	}
	// 降级后按未归档渲染是可以的——前提是上面那条披露在。
	if p := projectFromOverview(t, ov, "kindproj"); p["archived"] != false {
		t.Fatalf("降级时应按未归档显示：%v", p["archived"])
	}
	// 损坏文件上不得继续写：那会把用户已有的归档状态整块吞掉。
	rec := postArchive(t, s, `{"id":"kindproj","archived":true}`, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("损坏状态文件上的写入应失败，got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestArchiveEndpointGuards —— 这是看板唯一的写入端点，三道闸缺一不可。
func TestArchiveEndpointGuards(t *testing.T) {
	root := bootKindRoot(t, "")
	s := newTestBoardServer(t, root, fixedTime())

	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/project/archive", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 应 405（<img src> 之类会随手触发），got %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/project/archive",
		strings.NewReader("id=kindproj&archived=true"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("表单编码应 415（挡住跨站自动提交表单），got %d", rec.Code)
	}

	rec = postArchive(t, s, `{"id":"kindproj","archived":true}`, func(r *http.Request) {
		r.Header.Set("Origin", "http://evil.example")
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("跨源 Origin 应 403，got %d", rec.Code)
	}

	rec = postArchive(t, s, `{"id":"根本不存在","archived":true}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知项目 id 应 404（否则状态文件会留下清不掉的垃圾记录），got %d", rec.Code)
	}

	// 同源 Origin 必须放行——浏览器同源 fetch 也会带 Origin 头。
	rec = postArchive(t, s, `{"id":"kindproj","archived":true}`, func(r *http.Request) {
		r.Header.Set("Origin", "http://"+r.Host)
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("同源 Origin 应放行，got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSameOriginHost(t *testing.T) {
	cases := []struct {
		origin, host string
		want         bool
	}{
		{"http://127.0.0.1:8788", "127.0.0.1:8788", true},
		// 反代场景：scheme 不同但 host 一致要放行，比 scheme 会误杀正常访问。
		{"https://board.lan:8788", "board.lan:8788", true},
		{"http://evil.example", "127.0.0.1:8788", false},
		{"", "127.0.0.1:8788", false},
		{"null", "127.0.0.1:8788", false},
		{"http://127.0.0.1:9999", "127.0.0.1:8788", false},
	}
	for _, c := range cases {
		if got := sameOriginHost(c.origin, c.host); got != c.want {
			t.Errorf("sameOriginHost(%q,%q)=%v want %v", c.origin, c.host, got, c.want)
		}
	}
}

// TestArchiveDoesNotTouchQueueFiles —— 归档只写 board_archive.json，
// tasks/ 目录的内容与修改时间必须原封不动（看板对队列数据只读的底线）。
func TestArchiveDoesNotTouchQueueFiles(t *testing.T) {
	root := bootKindRoot(t, "")
	s := newTestBoardServer(t, root, fixedTime())

	before := map[string]string{}
	entries, err := os.ReadDir(tasksDir(root))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(tasksDir(root), e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		before[e.Name()] = string(data)
	}
	if len(before) == 0 {
		t.Fatal("测试前置：tasks/ 应有卡")
	}

	if rec := postArchive(t, s, `{"id":"kindproj","archived":true}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("归档失败：%s", rec.Body.String())
	}
	for name, want := range before {
		data, err := os.ReadFile(filepath.Join(tasksDir(root), name))
		if err != nil {
			t.Fatalf("任务文件 %s 读不出来了：%v", name, err)
		}
		if string(data) != want {
			t.Fatalf("归档改动了任务卡 %s —— 看板对队列数据必须只读", name)
		}
	}
}

// ================== 四、队列任务消耗（按时间窗口）==================
//
// 反例注入：
//   ① 无花费数据的卡（codex 不回报 cost）若被算进"计入的卡"，平均每卡花费会凭空腰斩；
//   ② 窗口按 created_at 而非 updated_at 归档的话，上周入队今天跑完的卡会被算进上周——
//      那笔钱是今天花的；
//   ③ by_model 次序不定的话，30 秒一次的自动刷新会让表格行每次都跳位；
//   ④ 逐卡表被截断却不披露，30 行会被当成"这个窗口只有 30 张卡有花费"。

// bootSpendRoot 造一批带花费的卡：窗口内 3 张有花费 + 1 张无花费，窗口外 1 张。
func bootSpendRoot(t *testing.T, now time.Time) string {
	t.Helper()
	root := testRoot(t)
	if err := saveConfig(root, defaultConfig("claude")); err != nil {
		t.Fatal(err)
	}
	mk := func(title, model string, cost float64, turns int, updated time.Time) {
		tk := newTask(root, testCfg(), typeSequence, title, "/tmp/spendproj", []string{"干活"}, 5)
		tk.Status = statusDone
		tk.Model = model
		tk.CostUSD = cost
		tk.TurnsUsed = turns
		tk.UpdatedAt = updated.Format(time.RFC3339)
		if err := saveTask(root, tk); err != nil {
			t.Fatal(err)
		}
	}
	mk("窗口内·贵的", "opus", 30, 300, now.Add(-2*time.Hour))
	mk("窗口内·便宜的", "opus", 5, 50, now.Add(-3*time.Hour))
	mk("窗口内·另一个模型", "claude-fable-5", 12, 120, now.Add(-4*time.Hour))
	mk("窗口内·codex 无花费", "", 0, 0, now.Add(-5*time.Hour))       // 反例①
	mk("窗口外·三天前", "sonnet", 999, 9999, now.Add(-72*time.Hour)) // 反例②
	return root
}

func spendFor(t *testing.T, root string, key string, now time.Time) TaskSpend {
	t.Helper()
	snap, err := buildSnapshot(root, now)
	if err != nil {
		t.Fatal(err)
	}
	return buildTaskSpend(snap.Cfg, snap, key, now)
}

func TestTaskSpendWindowAndUnpriced(t *testing.T) {
	now := fixedTime()
	root := bootSpendRoot(t, now)
	sp := spendFor(t, root, "24h", now)

	if sp.Tasks != 4 {
		t.Fatalf("24h 窗口内应有 4 张卡（三天前那张在窗口外），got %d", sp.Tasks)
	}
	// 反例①：无花费的卡计进 Tasks/Unpriced，但**不**计进 Priced 与合计金额。
	if sp.Priced != 3 || sp.Unpriced != 1 {
		t.Fatalf("有花费 3 / 无花费 1，got priced=%d unpriced=%d", sp.Priced, sp.Unpriced)
	}
	if sp.CostUSD != 47 {
		t.Fatalf("合计应为 30+5+12=47，got %v", sp.CostUSD)
	}
	if sp.TurnsUsed != 470 {
		t.Fatalf("轮数只统计有花费的卡（300+50+120），got %d", sp.TurnsUsed)
	}
	// 反例②：窗口外那张 $999 一分钱都不能漏进来。
	if sp.CostUSD >= 999 {
		t.Fatal("窗口外的卡被算进来了——时间过滤失效")
	}
	if sp.Since == "" {
		t.Fatal("有限窗口必须给出起点 since")
	}
}

func TestTaskSpendAllRangeIncludesEverything(t *testing.T) {
	now := fixedTime()
	root := bootSpendRoot(t, now)
	sp := spendFor(t, root, "all", now)
	if sp.Tasks != 5 || sp.Priced != 4 {
		t.Fatalf("all 窗口应收全部 5 张（4 张有花费），got tasks=%d priced=%d", sp.Tasks, sp.Priced)
	}
	if sp.CostUSD != 1046 {
		t.Fatalf("合计应为 47+999=1046，got %v", sp.CostUSD)
	}
	// all 表示"有史以来"，不是"从 0 时刻起"——给个假的 since 会让人以为窗口有下界。
	if sp.Since != "" {
		t.Fatalf("range=all 不该给 since，got %q", sp.Since)
	}
}

func TestTaskSpendByModelSortedAndDeterministic(t *testing.T) {
	now := fixedTime()
	root := bootSpendRoot(t, now)
	first := spendFor(t, root, "24h", now)
	if len(first.ByModel) != 2 {
		t.Fatalf("应有 opus / claude-fable-5 两个模型，got %+v", first.ByModel)
	}
	if first.ByModel[0].Model != "opus" || first.ByModel[0].CostUSD != 35 {
		t.Fatalf("花费最高的应是 opus $35（30+5），got %+v", first.ByModel[0])
	}
	if first.ByModel[0].Tasks != 2 {
		t.Fatalf("opus 应计 2 张有花费的卡，got %d", first.ByModel[0].Tasks)
	}
	// 反例③：map 遍历顺序随机，不定序的话自动刷新会让行每次跳位。跑 5 遍必须一致。
	for i := 0; i < 5; i++ {
		again := spendFor(t, root, "24h", now)
		for j := range again.ByModel {
			if again.ByModel[j].Model != first.ByModel[j].Model {
				t.Fatalf("第 %d 次重算次序变了：%v vs %v", i+1, again.ByModel, first.ByModel)
			}
		}
		if len(again.ByProject) != len(first.ByProject) ||
			again.ByProject[0].ID != first.ByProject[0].ID {
			t.Fatalf("第 %d 次重算按项目表次序变了", i+1)
		}
	}
}

// TestTaskSpendByProjectAggregates —— 项目维度：金额/轮数按项目合并，
// 卡数给「计入 / 全部」两个，主力模型取项目内花得最多的那个。
func TestTaskSpendByProjectAggregates(t *testing.T) {
	now := fixedTime()
	root := bootSpendRoot(t, now)
	sp := spendFor(t, root, "24h", now)

	if len(sp.ByProject) != 1 || sp.ProjectsN != 1 {
		t.Fatalf("四张卡同目录，应聚成 1 个项目，got %+v", sp.ByProject)
	}
	p := sp.ByProject[0]
	if p.CostUSD != 47 {
		t.Fatalf("项目花费应为 30+5+12=47，got %v", p.CostUSD)
	}
	// 反例①的项目级镜像：无花费的 codex 卡要计进 Tasks 但不计进 Priced——
	// 只给一个卡数会让"这条线只花了 $47"分不清是活少还是花费没记上。
	if p.Tasks != 4 || p.Priced != 3 {
		t.Fatalf("卡数应为 计入3/全部4，got tasks=%d priced=%d", p.Tasks, p.Priced)
	}
	if p.TurnsUsed != 470 {
		t.Fatalf("轮数只统计有花费的卡，got %d", p.TurnsUsed)
	}
	// opus 两张合 $35 > fable 一张 $12，主力模型应是 opus。
	if p.TopModel != "opus" || p.TopModelCost != 35 {
		t.Fatalf("主力模型应为 opus $35，got %s $%v", p.TopModel, p.TopModelCost)
	}
	if sp.TopTruncated {
		t.Fatal("只有 1 个项目时不该标截断")
	}
}

// TestTaskSpendByProjectSeparatesProjects —— 不同目录的卡必须落进不同项目行，
// 否则"钱花在哪条线上"这个问题根本答不出来。
func TestTaskSpendByProjectSeparatesProjects(t *testing.T) {
	now := fixedTime()
	root := testRoot(t)
	if err := saveConfig(root, defaultConfig("claude")); err != nil {
		t.Fatal(err)
	}
	// 两个项目各一张卡：卡少到不足以自证是项目（BD-45 的「未分类」门槛），
	// 故显式钉 -project——本用例要测的是"按项目聚合花费"，不是归组推导本身。
	mk := func(dir string, cost float64) {
		tk := newTask(root, testCfg(), typeSequence, "活", dir, []string{"干活"}, 5)
		tk.Project = dirBase(dir)
		tk.Status = statusDone
		tk.Model = "opus"
		tk.CostUSD = cost
		tk.UpdatedAt = now.Add(-time.Hour).Format(time.RFC3339)
		if err := saveTask(root, tk); err != nil {
			t.Fatal(err)
		}
	}
	mk("/tmp/alpha", 10)
	mk("/tmp/beta", 90)
	sp := spendFor(t, root, "24h", now)
	if len(sp.ByProject) != 2 {
		t.Fatalf("两个目录应是两个项目，got %+v", sp.ByProject)
	}
	// 花得多的排前面。
	if sp.ByProject[0].CostUSD != 90 || sp.ByProject[1].CostUSD != 10 {
		t.Fatalf("按项目表未按花费降序：%+v", sp.ByProject)
	}
	if sp.ByProject[0].Name == sp.ByProject[1].Name {
		t.Fatal("两行的项目名不该相同")
	}
}

// TestTaskSpendBasisDisclosesCaveats —— 两条边界必须出现在披露串里：
// 花的是 API 等价成本（不是扣款）、有多少张卡没有花费数据。
func TestTaskSpendBasisDisclosesCaveats(t *testing.T) {
	now := fixedTime()
	root := bootSpendRoot(t, now)
	sp := spendFor(t, root, "24h", now)
	for _, want := range []string{"API 等价成本", "不是实际扣款", "codex", "未计入", "updated_at", "队列口径"} {
		if !strings.Contains(sp.Basis, want) {
			t.Errorf("披露串缺 %q：%s", want, sp.Basis)
		}
	}
}

func TestResolveSpendRangeFallsBackTo24h(t *testing.T) {
	for _, k := range []string{"", "90d", "../etc", "ALL"} {
		if got := resolveSpendRange(k); got.Key != "24h" {
			t.Errorf("未知窗口 %q 应回落 24h，got %q", k, got.Key)
		}
	}
	for _, k := range []string{"24h", "7d", "30d", "all"} {
		if got := resolveSpendRange(k); got.Key != k {
			t.Errorf("合法窗口 %q 被改成了 %q", k, got.Key)
		}
	}
}

// TestBurnEndpointCarriesTaskSpend —— 契约：/api/burn 必须带 task_spend，且 range 参数生效。
func TestBurnEndpointCarriesTaskSpend(t *testing.T) {
	now := fixedTime()
	root := bootSpendRoot(t, now)
	s := newTestBoardServer(t, root, now)

	get := func(q string) map[string]any {
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/burn"+q, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("/api/burn%s 应 200，got %d", q, rec.Code)
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("响应不是合法 JSON: %v", err)
		}
		sp, _ := out["task_spend"].(map[string]any)
		if sp == nil {
			t.Fatalf("/api/burn%s 响应缺 task_spend", q)
		}
		return sp
	}
	if got := get("?range=24h")["tasks"]; got != float64(4) {
		t.Fatalf("24h 应 4 张卡，got %v", got)
	}
	if got := get("?range=all")["tasks"]; got != float64(5) {
		t.Fatalf("all 应 5 张卡，got %v", got)
	}
	// 不带 range 时回落 24h，不得 500。
	if got := get("")["range"]; got != "24h" {
		t.Fatalf("缺省 range 应回落 24h，got %v", got)
	}
}

func TestRound2KeepsCents(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{0.044, 0.04}, {0.045, 0.05}, {12.3456, 12.35}, {0, 0}, {1234.567, 1234.57},
	}
	for _, c := range cases {
		if got := round2(c.in); got != c.want {
			t.Errorf("round2(%v)=%v want %v —— 花费是钱，分位不能抹", c.in, got, c.want)
		}
	}
}

// ---- token 曲线跟随窗口 ----

// TestTokenScanPlanScalesWithRange —— 窗口一长，桶要变粗、预算要跟上。
// 桶不变粗则 30 天要画 2880 个点（卡且无信息）；预算不跟上则必然截断，
// 而"扫了一半的全月"是造读数。实测体量：24h≈104MB / 7d≈419MB / 30d≈1.06GB。
func TestTokenScanPlanScalesWithRange(t *testing.T) {
	prev := tokenScanPlanFor("24h")
	if prev.LookbackHours != 24 || prev.BucketMinutes != 15 {
		t.Fatalf("24h 计划应为 24 小时/15 分钟桶，got %+v", prev)
	}
	for _, k := range []string{"7d", "30d", "all"} {
		p := tokenScanPlanFor(k)
		if p.LookbackHours <= prev.LookbackHours {
			t.Errorf("%s 的回看窗口应比上一档长：%d vs %d", k, p.LookbackHours, prev.LookbackHours)
		}
		if p.BucketMinutes <= prev.BucketMinutes {
			t.Errorf("%s 的桶应比上一档粗（否则点数爆炸）：%d vs %d", k, p.BucketMinutes, prev.BucketMinutes)
		}
		if p.ByteBudget <= prev.ByteBudget {
			t.Errorf("%s 的字节预算应比上一档大（否则必然截断）：%d vs %d", k, p.ByteBudget, prev.ByteBudget)
		}
		prev = p
	}
	// 未知窗口回落 24h，与 resolveSpendRange 保持一致。
	if got := tokenScanPlanFor("不存在"); got.LookbackHours != 24 {
		t.Fatalf("未知窗口应回落 24h 计划，got %+v", got)
	}
	// 30 天的预算必须覆盖实测体量（1.06 GB）并留余量，否则一上线就截断。
	if tokenScanPlanFor("30d").ByteBudget < 2*1024*mib {
		t.Fatal("30d 预算不足以覆盖实测 1.06 GB 的两倍余量")
	}
}

// TestTokenSeriesCarriesScanDisclosure —— 契约：曲线必须自报窗口与扫描完整性。
// 少了 truncated，一条读了一半的曲线与"那段时间没跑活"在图上无法区分。
func TestTokenSeriesCarriesScanDisclosure(t *testing.T) {
	ts := buildTokenSeries(testCfg(), fixedTime(), "7d")
	if ts.Range != "7d" {
		t.Fatalf("曲线应回吐自己的窗口，got %q", ts.Range)
	}
	if ts.Since == "" {
		t.Fatal("曲线必须给出窗口起点 since")
	}
	if ts.BucketMinutes != tokenScanPlanFor("7d").BucketMinutes {
		t.Fatalf("bucket_minutes 应与扫描计划一致，got %d", ts.BucketMinutes)
	}
	b, _ := json.Marshal(ts)
	for _, key := range []string{`"range"`, `"since"`, `"truncated"`, `"files_matched"`, `"files_scanned"`, `"bytes_scanned"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("JSON 契约缺 %s：%s", key, string(b))
		}
	}
}

// TestBurnCacheIsPerRange —— 反例：窗口共用一格缓存的话，
// 每次切标签页都会重扫最贵的那个窗口（30 天要读 1 GB）。
func TestBurnCacheIsPerRange(t *testing.T) {
	root := bootSpendRoot(t, fixedTime())
	c := &burnCache{ttl: time.Hour}
	now := fixedTime()
	a := c.get(root, testCfg(), now, "24h")
	b := c.get(root, testCfg(), now, "7d")
	if a == b {
		t.Fatal("不同窗口必须落在不同缓存格，否则切换窗口拿到的是另一个窗口的读数")
	}
	if again := c.get(root, testCfg(), now, "24h"); again != a {
		t.Fatal("同一窗口在 TTL 内应命中缓存，而不是重扫")
	}
	if len(c.entries) != 2 {
		t.Fatalf("应有两格缓存，got %d", len(c.entries))
	}
	// 未知窗口归一到 24h 那一格，不额外开格（否则 ?range=xxx 能把缓存撑爆）。
	if again := c.get(root, testCfg(), now, "乱写的"); again != a {
		t.Fatal("未知窗口应归一到 24h 那一格")
	}
	if len(c.entries) != 2 {
		t.Fatalf("未知窗口不该新开缓存格，got %d", len(c.entries))
	}
}
