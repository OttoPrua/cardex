package main

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
)

// ---- 卡片归属哪个项目（显式 > 别名 > 模式 > 启发式 > 未分类）----
//
// BD-45 承 2026-07-31 委托人裁定：**显式归组优先 + 「未分类」兜底桶**。
//
// 【要解决什么】任务卡里没有 project 字段（见 task.go 的设计说明），看板只能从工作目录
// 反推项目（boardmodel.go groupDirs 的四类并查集证据）。反推对"一个项目一个稳定目录"
// 的假设成立；但实测账本里大量卡跑在**任务级临时目录**上——远端 D:/Project/PO-tasks/<taskid>
// 一卡一目录、日期工作树 Trading-<slug>-<date>、复审散目录 HB-*/S3-*/card-*。
// 每个这样的目录都自成一个"项目"，于是看板渲染出 80 个项目，其中真实的只有约 10 个。
// 结论文件化要"按项目可寻"，80 个野项目等于寻不到；跨 claude/codex 续用时更是丢上下文。
//
// 【四层归属，强度从高到低】
//  1. explicit：Task.Project（add -project 显式指定，入队即钉）。最强，压过全部启发式。
//  2. alias：board.json 顶层 project_aliases 的有序规则表（存量整理机制：改配置即全量
//     追溯生效，**不改任何卡文件**——快照每次重建都重新解析）。
//  3. pattern：代码内建通用模式——目录 basename 以「已知项目名 + '-'」开头即归该项目
//     （治日期工作树野化：Trading-paper-strategy-envelope-20260730 → Trading）。
//  4. heuristic：groupDirs 的并查集分量（既有行为，一字未改）。
//  5. 都不中 → 「未分类」兜底桶（不再各自成项目）。
//
// 【为什么兜底桶不叫"失败"而叫"收件箱"】真实新项目的第一张卡必然落进这里——它此刻确实
// 没有任何可归组证据。桶的语义是"待整理"，转正靠 -project 或在 project_aliases 登记，
// 而不是靠攒够卡数自己冒出来。add 时的 stderr 软警告（warnIfUnclassified）是这条流程的入口，
// **不硬阻**：合法新项目本来就该能一条命令派出去。
const (
	// unclassifiedProject 是兜底桶的项目名。看板恒显示（即使 0 张卡）——
	// 一个"空收件箱"是有信息量的（说明当前没有待整理的野目录），消失则等于把这条纪律藏起来。
	unclassifiedProject = "未分类"
	// unclassifiedProjectID 是兜底桶的固定 id。不能走 slugify：中文名会被 slugify 吞成
	// "p-<hash>"，既不可读，也会随名字微调而漂移——而 id 是 /api/project?id= 的主键，
	// 前端书签与 board_archive.json 的归档记录都按 id 存。
	unclassifiedProjectID = "unclassified"
)

// minSoloProjectCards 是「单目录分量」被认可为独立项目的卡数门槛。
//
// 【为什么需要一个门槛】启发式分量有两种：多目录的（跨机镜像/车道互证，证据充分）和
// 单目录的（只有一个目录出现过，没有任何佐证）。野目录清一色是后者——一卡一目录的
// 任务级工作树。但真实项目也可能只有一个目录（如 ~/Projects/TShare，12 张卡、无镜像），
// 所以不能一刀切把单目录分量全判野：还要看它**是否在持续产出**。
//
// 【为什么是 3】实测账本里野目录 100% 是 1~2 张卡（一次性任务工作树），
// 而最小的真实单目录项目是 12 张卡。3 落在这条鸿沟里且靠近下界——宁可让一个刚起步的
// 真项目在收件箱里多待两张卡（转正只需一行 -project），也不愿意再让野目录污染看板。
const minSoloProjectCards = 3

// 归组证据来源。给测试与诊断用：优先级链一旦被改错，断言的是**来源**而不只是结果名，
// 否则"别名恰好和启发式给出同一个名字"会让优先级回归测试假绿。
const (
	projSourceExplicit     = "explicit"
	projSourceAlias        = "alias"
	projSourcePattern      = "pattern"
	projSourceHeuristic    = "heuristic"
	projSourceUnclassified = "unclassified"
)

// boardProjectAlias 是 board.json 顶层 project_aliases 里的一条归组规则。
//
//	{"match": "<目录前缀或 glob>", "title": "<标题子串，可选>", "project": "<项目名>"}
//
// 语义（首条命中即用，故**顺序有意义**）：
//   - match 不含通配符时 = **精确匹配该目录本身**；含 * ? [ 时按 path.Match 匹配
//     「该目录或它的任一祖先」，故 "X/*" 覆盖 X 下任意深度的子目录。
//     一律大小写不敏感——同一个远端目录在 Windows 卡与本机卡里大小写常不一致。
//   - 【为什么裸路径不做前缀语义】前缀语义下，给一个**容器目录**（如 ~/Projects）写一条规则
//     会把它下面所有项目一次性吞进同一个项目，且账面完全看不出来。两种误用的代价不对称：
//     漏配的后果是几个目录留在收件箱（可见、可改），过配的后果是整块看板塌成一个项目。
//     故裸路径取窄义，要覆盖子树必须显式写 "/*" —— 多打两个字符换一道防塌方护栏。
//   - title 是标题子串（大小写不敏感）。为什么需要它：远端任务级目录
//     D:/Project/PO-tasks/<taskid> 的目录名是随机任务 ID，**目录本身不含任何项目信息**，
//     唯一能判归属的是标题（"Hermes runtime truth R3" → PerlicaHermes）。
//   - 两者都写 = 必须同时命中（AND）。这是刻意的：让"仅限某个容器目录下"的标题规则
//     不会泄漏到全盘——单独的 title 规则会命中任意目录的同名卡。
//   - 只写 title、不写 match 是合法的（全盘标题规则），但请谨慎用。
type boardProjectAlias struct {
	Match   string `json:"match"`
	Title   string `json:"title,omitempty"`
	Project string `json:"project"`
}

// parseProjectAliases 逐条校验别名规则：坏规则**逐条跳过并披露**，不整块拒。
// 与 kind_rules 同一纪律（见 boardkind.go parseKindRules）：一条手误不该让整张表失效，
// 但被跳过的必须说出来——静默失效即造读数（用户以为登记了，看板照旧散着）。
func parseProjectAliases(raw []boardProjectAlias) ([]boardProjectAlias, string) {
	var out []boardProjectAlias
	var bad []string
	for i, r := range raw {
		m := strings.TrimSpace(r.Match)
		ti := strings.TrimSpace(r.Title)
		p := strings.TrimSpace(r.Project)
		switch {
		case p == "":
			bad = append(bad, fmt.Sprintf("#%d 缺 project", i+1))
			continue
		case m == "" && ti == "":
			bad = append(bad, fmt.Sprintf("#%d match 与 title 至少要有一个", i+1))
			continue
		}
		if isGlob(m) {
			// path.Match 的语法错（如未闭合的 [）只有真正 Match 一次才暴露。
			// 留到匹配期就成了"每张卡都静默不命中"——正是这条规则要防的失效形态。
			if _, err := path.Match(strings.ToLower(m), "x"); err != nil {
				bad = append(bad, fmt.Sprintf("#%d glob 非法 (%s): %v", i+1, m, err))
				continue
			}
		}
		out = append(out, boardProjectAlias{Match: m, Title: ti, Project: p})
	}
	if len(bad) == 0 {
		return out, ""
	}
	return out, "board.json project_aliases 有 " + fmt.Sprint(len(bad)) +
		" 条规则被跳过（其余仍生效）: " + strings.Join(bad, "; ")
}

func isGlob(s string) bool { return strings.ContainsAny(s, "*?[") }

// dirSelfAndAncestors 返回目录自身及其各级祖先，由近及远。
// 归属判定一律沿这条链走：野目录常挂在容器下（D:/Project/PO-tasks/<taskid>、
// ~/Projects/Trading-strategy-research-20260726/<子课题>），只看 basename 会漏掉容器给出的证据。
func dirSelfAndAncestors(d string) []string {
	var out []string
	for c := d; c != ""; c = dirParent(c) {
		out = append(out, c)
		if !strings.Contains(c, "/") {
			break
		}
	}
	return out
}

// matchesDir 判断规则的 match 是否命中该目录。match 为空视为"不限目录"。
// 裸路径只比目录自身，glob 沿目录→祖先链比（见 boardProjectAlias 的语义说明）。
func (a boardProjectAlias) matchesDir(d string) bool {
	if a.Match == "" {
		return true
	}
	m := strings.ToLower(a.Match)
	d = strings.ToLower(d)
	if !isGlob(m) {
		return d == m
	}
	for _, c := range dirSelfAndAncestors(d) {
		if ok, err := path.Match(m, c); err == nil && ok {
			return true
		}
	}
	return false
}

// matchesTitle 判断规则的 title 是否命中卡标题。title 为空视为"不限标题"。
func (a boardProjectAlias) matchesTitle(title string) bool {
	if a.Title == "" {
		return true
	}
	return strings.Contains(strings.ToLower(title), strings.ToLower(a.Title))
}

// projectResolver 把「一批卡」编译成一张归属判定器。
//
// 为什么要先吃下整批卡才能判单张卡：pattern 层要用的「已知项目名」不是常量，
// 而是**当批任务里已经站住脚的项目代表名**（见 knownNames）；heuristic 层的分量与
// 分量是否够格，也只有看完全批才知道。故 resolve 必须在 newProjectResolver 之后调用。
type projectResolver struct {
	aliases  []boardProjectAlias
	aliasErr string
	// rep 是 dir → 启发式分量代表（groupDirs 的输出，行为未改）。
	rep map[string]string
	// compName 是分量代表 → 项目名（沿用 pickProjectDir 的选法）。
	compName map[string]string
	// compOK 是分量是否够格成为独立项目（见 minSoloProjectCards）。
	compOK map[string]bool
	// known 是 pattern 层的已知项目名，**按长度降序**：
	// "Trading" 与 "Trading-docs" 同时已知时，Trading-docs-xxx 必须归给更长的那个。
	known []string
}

func newProjectResolver(tasks []*Task, rawAliases []boardProjectAlias) *projectResolver {
	aliases, aliasErr := parseProjectAliases(rawAliases)
	r := &projectResolver{
		aliases:  aliases,
		aliasErr: aliasErr,
		rep:      groupDirs(tasks),
		compName: map[string]string{},
		compOK:   map[string]bool{},
	}

	dirCount := map[string]int{}
	compDirs := map[string]map[string]bool{}
	compCards := map[string]int{}
	addDir := func(d string) {
		if d == "" {
			return
		}
		c, ok := r.rep[d]
		if !ok {
			return
		}
		if compDirs[c] == nil {
			compDirs[c] = map[string]bool{}
		}
		compDirs[c][d] = true
	}
	for _, t := range tasks {
		d := normDir(t.Dir)
		if d == "" {
			continue
		}
		dirCount[d]++
		compCards[r.rep[d]]++
		addDir(d)
		// review_dir 是同一项目在审核机上的镜像目录（显式镜像对，groupDirs 证据一）。
		// 它算作分量的一个目录：这正是"跨机互证"这条强证据的物证。
		addDir(normDir(t.ReviewDir))
	}
	for c, dirs := range compDirs {
		ds := make([]string, 0, len(dirs))
		for d := range dirs {
			ds = append(ds, d)
		}
		sort.Strings(ds) // pickProjectDir 同分时取字典序小者，先排序保证结果与遍历顺序无关
		r.compName[c] = dirBase(pickProjectDir(ds, dirCount))
		r.compOK[c] = len(ds) >= 2 || compCards[c] >= minSoloProjectCards
	}
	r.known = knownNames(tasks, aliases, r.compName, r.compOK)
	return r
}

// knownNames 汇总 pattern 层可用的「已知项目名」：够格分量的代表名 + 别名表登记的项目名
// + 卡上显式钉的项目名。三者都是"这个名字确实是个项目"的当批证据。
//
// 为什么排除 genericBase：docs/src/config 这类通用目录名一旦进了已知名单，
// "docs-legacy"、"config-v2" 这些跟项目毫无关系的目录会被硬拽进一个假项目。
func knownNames(tasks []*Task, aliases []boardProjectAlias,
	compName map[string]string, compOK map[string]bool) []string {

	set := map[string]bool{}
	add := func(n string) {
		n = strings.TrimSpace(n)
		if n == "" || n == unclassifiedProject || genericBase[strings.ToLower(n)] {
			return
		}
		set[n] = true
	}
	for c, ok := range compOK {
		if ok {
			add(compName[c])
		}
	}
	for _, a := range aliases {
		add(a.Project)
	}
	for _, t := range tasks {
		add(t.Project)
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j]) // 长名优先：最长匹配语义
		}
		return out[i] < out[j] // 同长按字典序，保证与 map 遍历顺序无关
	})
	return out
}

// resolve 判定一张卡归哪个项目，并返回判定来源（见 projSource* 常量）。
// 顺序即优先级，任何一层命中立即返回——这是本功能的核心契约，改动请连带改测试。
func (r *projectResolver) resolve(t *Task) (string, string) {
	if p := strings.TrimSpace(t.Project); p != "" {
		return p, projSourceExplicit
	}
	d := normDir(t.Dir)
	if d == "" {
		// 无目录卡（历史上单列一个 "(无目录)" 项目）：它按定义没有任何归组证据，
		// 正是收件箱要收的东西，不该在总览里另占一格。
		return unclassifiedProject, projSourceUnclassified
	}
	for _, a := range r.aliases {
		if a.matchesDir(d) && a.matchesTitle(t.Title) {
			return a.Project, projSourceAlias
		}
	}
	if n, ok := r.matchPattern(d); ok {
		return n, projSourcePattern
	}
	if c, ok := r.rep[d]; ok && r.compOK[c] {
		return r.compName[c], projSourceHeuristic
	}
	return unclassifiedProject, projSourceUnclassified
}

// matchPattern 是内建通用模式：目录（或其任一祖先）的 basename 以「已知项目名 + '-'」
// 开头即归该项目。日期工作树 Trading-paper-strategy-envelope-20260730、
// 对照工作树 PerlicaHermes-cmp-sol、以及挂在 Trading-strategy-research-20260726/ 下的
// 子课题目录，都靠这一条收回本项目。
//
// 要求 '-' 后面**至少还有一个字符**：项目根目录自身（basename 恰等于已知名）不该被
// 这条规则接管——它本来就该走启发式，走 pattern 只是绕了一圈得到同一个答案。
func (r *projectResolver) matchPattern(d string) (string, bool) {
	for _, c := range dirSelfAndAncestors(d) {
		b := dirBase(c)
		for _, k := range r.known {
			if len(b) > len(k)+1 && b[len(k)] == '-' && strings.EqualFold(b[:len(k)], k) {
				return k, true
			}
		}
	}
	return "", false
}

// projectSlug 把项目名转成看板 id。兜底桶用固定 id（见 unclassifiedProjectID）。
func projectSlug(name string) string {
	if name == unclassifiedProject {
		return unclassifiedProjectID
	}
	return slugify(name)
}

// unclassifiedDesc 是兜底桶的固定说明前缀。它讲的是**语义**（这是收件箱，不是垃圾桶），
// 自动统计那段仍由 derivedProjectDesc 追加在后面。board.json 里给 unclassified 配了
// desc 时以人工文案为准（与其它项目同规则）。
const unclassifiedDesc = "待整理收件箱：这些目录暂时没有可归组的证据（新项目的第一张卡、" +
	"一次性任务工作树、临时目录都会先落这里）。用 add -project 指定，" +
	"或在 board.json 的 project_aliases 登记后，下次快照重建即自动迁出。"

// warnIfUnclassified 是 add 时的软约束：新卡按当前账本判定会落进「未分类」时，
// 往 stderr 打一行提示。**绝不阻断**——合法新项目的第一张卡本来就会落这里，
// 硬拦等于逼人先去配文件才能派活。判定复用 resolve 本身（不是另写一套近似规则），
// 所以提示与看板的实际归组结果永远一致。
//
// 读盘失败一律静默返回：这只是一句提示，不能让它把 add 的成功路径搞出噪音或失败。
func warnIfUnclassified(root string, t *Task) {
	if strings.TrimSpace(t.Project) != "" {
		return
	}
	tasks, err := loadBoardTasks(root)
	if err != nil {
		return
	}
	ov, _, _ := loadBoardOverride(root)
	res := newProjectResolver(tasks, ov.ProjectAliases)
	if _, src := res.resolve(t); src != projSourceUnclassified {
		return
	}
	fmt.Fprintf(os.Stderr,
		"警告: 目录 %s 未匹配任何显式/别名/模式/启发式归组，该卡将落入看板「%s」；"+
			"如属既有项目请用 -project 指定，或在 board.json 的 project_aliases 登记。\n",
		t.Dir, unclassifiedProject)
}
