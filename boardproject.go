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
	projSourceExplicit  = "explicit"
	projSourceAlias     = "alias"
	projSourcePattern   = "pattern"
	projSourceHeuristic = "heuristic"
	// projSourceLineage：派生谱系（review_of / emitted_by 上溯到父卡的归属）。
	// 排在启发式之后、收件箱之前——只救原本要落进收件箱的派生卡，见 resolveLineage。
	projSourceLineage      = "lineage"
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
	// authoritative 是「这个名字确实是一个**地方**，不是某个项目的工作树标签」的已知名集合
	// （键为小写名）。两种证据，任一即可：
	//   - 声明：别名表里写了它，或有卡用 -project 钉了它（人明说的，最强）；
	//   - 跨根：它的启发式分量里有 ≥2 个**互不为祖先**的目录（跨机镜像 D:/…/X 与本机 /w/X、
	//     或两条独立路径同名）——同一棵树下的父子目录不算，因为子目录不为"这个名字是项目"
	//     提供任何独立证据。
	// 只用于 matchPattern 的链首等值判定，见那里的说明。
	authoritative map[string]bool
	// byID 供 lineage 层沿谱系上溯父卡（见 resolveLineage）。
	byID map[string]*Task
	// lineageMemo 缓存谱系层的判定结果（同一条链上多张卡会重复上溯同一批父卡）。
	// 值为 "" 表示"上溯过、没结论"，与未算过（键不存在）区分开。
	lineageMemo map[string]string
}

// canonicalProjectNames 把「slug 相同的一组项目名」折叠成一个显示名。
//
// 【治什么】实测账本里同一个项目裂成两格：一张卡手打 `-project trading`（小写），启发式对
// 同一批目录派生出 `Trading`——两个名字 slug 都是 "trading"，撞 id 后**双方各带一个哈希
// 后缀**（trading-9af2ec54 / trading-756e2198），于是总览上并排出现两个 Trading，其中一个
// 只有 1 张卡。`~/.cardex` 的复盘卡同理：dirBase 派生出 `.cardex`，与 `cardex` 撞 slug。
// 用户看到的就是"孤卡自成一个项目"。哈希后缀本是 id 唯一性的兜底，不该被用来把明显同一个
// 项目钉成两个。
//
// 【为什么按 slug 折叠而不只按大小写】slug 归一（大小写、点、空格、连字符）恰好是"人眼看来
// 是同一个名字"的边界：`trading`/`Trading`、`.cardex`/`cardex`、`Foo Bar`/`foo-bar`。真要分开
// 两个 slug 相同的项目，出口与本功能一贯的一致——`project_aliases` 登记或 `-project` 钉一次。
//
// 【显示名怎么挑】按权威度：别名表声明过的 > 卡上显式钉过的 > 卡数多的 > 字典序靠前的。
// 前两条是"人明说的名字"，最后一条只为让结果与 map 遍历顺序无关。
func canonicalProjectNames(names []string, cardCount map[string]int,
	aliases []boardProjectAlias, tasks []*Task) map[string]string {

	declared := map[string]int{} // 名字 → 权威度（2=别名表，1=卡上显式钉）
	for _, a := range aliases {
		if n := strings.TrimSpace(a.Project); n != "" {
			declared[n] = 2
		}
	}
	for _, t := range tasks {
		if n := strings.TrimSpace(t.Project); n != "" && declared[n] < 1 {
			declared[n] = 1
		}
	}
	bySlug := map[string][]string{}
	for _, n := range names {
		if n == unclassifiedProject {
			continue // 兜底桶 id 固定，不参与折叠
		}
		s := projectSlug(n)
		bySlug[s] = append(bySlug[s], n)
	}
	out := map[string]string{}
	for _, group := range bySlug {
		if len(group) < 2 {
			continue
		}
		sort.Slice(group, func(i, j int) bool {
			if declared[group[i]] != declared[group[j]] {
				return declared[group[i]] > declared[group[j]]
			}
			if cardCount[group[i]] != cardCount[group[j]] {
				return cardCount[group[i]] > cardCount[group[j]]
			}
			return group[i] < group[j]
		})
		for _, n := range group[1:] {
			out[n] = group[0]
		}
	}
	return out
}

func newProjectResolver(tasks []*Task, rawAliases []boardProjectAlias) *projectResolver {
	aliases, aliasErr := parseProjectAliases(rawAliases)
	r := &projectResolver{
		aliases:     aliases,
		aliasErr:    aliasErr,
		rep:         groupDirs(tasks),
		compName:    map[string]string{},
		compOK:      map[string]bool{},
		byID:        make(map[string]*Task, len(tasks)),
		lineageMemo: map[string]string{},
	}
	for _, t := range tasks {
		r.byID[t.ID] = t // 谱系层按父卡 ID 上溯（review_of / emitted_by）
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
	r.authoritative = map[string]bool{}
	declare := func(n string) {
		if n = strings.TrimSpace(n); n != "" {
			r.authoritative[strings.ToLower(n)] = true
		}
	}
	for _, a := range aliases {
		declare(a.Project)
	}
	for _, t := range tasks {
		declare(t.Project)
	}
	for c, dirs := range compDirs {
		ds := make([]string, 0, len(dirs))
		for d := range dirs {
			ds = append(ds, d)
		}
		sort.Strings(ds) // pickProjectDir 同分时取字典序小者，先排序保证结果与遍历顺序无关
		r.compName[c] = dirBase(pickProjectDir(ds, dirCount))
		r.compOK[c] = len(ds) >= 2 || compCards[c] >= minSoloProjectCards
		if r.compOK[c] && countIndependentRoots(ds) >= 2 {
			declare(r.compName[c])
		}
	}
	r.known = knownNames(tasks, aliases, r.compName, r.compOK)
	return r
}

// countIndependentRoots 数一个分量里**互不为祖先**的目录个数。
// /w/X 与 /w/X/sub 只算 1 个（子目录是同一棵树的一部分，不构成独立证据）；
// /w/X 与 D:/mirror/X 算 2 个（两条独立路径都叫 X，这才说明 X 是个项目名）。
func countIndependentRoots(ds []string) int {
	n := 0
	for _, d := range ds {
		isDesc := false
		for _, e := range ds {
			if e != d && strings.HasPrefix(d, e+"/") {
				isDesc = true
				break
			}
		}
		if !isDesc {
			n++
		}
	}
	return n
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
	if name, src := r.resolveExcludingLineage(t); src != projSourceUnclassified {
		return name, src
	}
	// 前四层都无结论：沿派生谱系上溯，把审核/修复/emit 产出接回它父卡的项目（见 resolveLineage）。
	if n := r.resolveLineage(t, 0); n != "" {
		return n, projSourceLineage
	}
	return unclassifiedProject, projSourceUnclassified
}

// lineageMaxDepth 是谱系上溯的深度上限。修复链最长受 max_fix_rounds（默认 3、high 档 4）约束，
// 每轮两跳（实现→审核→修复），再加 emit 链几层——8 层足够覆盖真实谱系，同时挡住数据异常
// （父指针成环/指向自身）把快照重建吊死。
const lineageMaxDepth = 8

// resolveLineage 沿派生谱系上溯，返回父卡的项目归属；无谱系或上溯不到结论返回 ""。
//
// 【为什么需要这一层】派生卡常跑在**一次性目录**上：复审分流的远端镜像 D:/Project/mirrors/…、
// 交叉验证腿的 scratchpad、任务级工作树。这些目录天然没有归组证据（一卡一目录），于是审核卡
// 落进「未分类」——但一张审核卡的项目归属根本不需要猜：它审的是谁，就属于谁的项目。
// 谱系是**比目录更强的证据**，只是此前完全没被用上。
//
// 【为什么排在启发式之后而不是之前】卡自身的目录若已有扎实证据（跨机镜像互证、够格分量），
// 那是关于**这张卡**的直接证据，比"我父亲属于哪"更具体。本层只做兜底救援：把原本要落进
// 收件箱的派生卡接回它本来的项目，不改动任何已经判出结论的卡（纯增益，无回归面）。
//
// 【环与深度】父指针来自卡面字段（review_of/emitted_by），理论上不该成环，但账本是可以被
// 手工编辑的文件——seen 集合 + 深度上限保证任何数据形态下都能停下来。
func (r *projectResolver) resolveLineage(t *Task, depth int) string {
	if t == nil || depth > lineageMaxDepth {
		return ""
	}
	if n, ok := r.lineageMemo[t.ID]; ok {
		return n
	}
	// 先占位防环：同一条链上再次回到本卡时直接拿到 ""，不会无限递归。
	r.lineageMemo[t.ID] = ""

	parentID := t.ReviewOf
	if parentID == "" {
		parentID = t.EmittedBy
	}
	if parentID == "" {
		return ""
	}
	parent := r.byID[parentID]
	if parent == nil {
		return "" // 父卡已被 clean 清掉：谱系断了，如实回落收件箱
	}
	// 父卡自身也可能是派生卡（修复链：实现→审核→修复→审核…），故递归。
	name, src := r.resolveExcludingLineage(parent)
	if src == projSourceUnclassified {
		name = r.resolveLineage(parent, depth+1)
	}
	r.lineageMemo[t.ID] = name
	return name
}

// resolveExcludingLineage 跑前四层（显式 > 别名 > 模式 > 启发式），不进谱系层。
// 两个调用方：resolve 的主链，以及 resolveLineage 判断父卡自身是否已有结论
// （分开写是为了让"谱系层不能再触发谱系层"这条防递归约束在类型层面显而易见）。
func (r *projectResolver) resolveExcludingLineage(t *Task) (string, string) {
	if p := strings.TrimSpace(t.Project); p != "" {
		return p, projSourceExplicit
	}
	d := normDir(t.Dir)
	if d == "" {
		// 无目录卡（历史上单列一个 "(无目录)" 项目）：它按定义没有任何目录证据，
		// 正是收件箱要收的东西，不该在总览里另占一格（但仍可被谱系层救回）。
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
//
// 【等值必须先于前缀判（BD-45 R1·P1-1）】上一版只用 len(b) > len(k)+1 把「等长自吞」排除掉，
// 但排除的只是那一个 k：'Trading-docs' 这个 basename 与已知名 'Trading-docs' 等值时被跳过，
// 循环继续走到更短的已知名 'Trading'，于是以 'Trading-' 前缀把它吞进 Trading。
// 后果是 ~/Projects/Trading-docs/notes（子目录）、D:/Project/mirrors/Trading-docs（跨机新根）
// 这类卡被判给**错误的项目**，且判定来源仍是 pattern——界面上看不出任何异常。
// 靶子：TestProjectPatternEqualNameBeatsShorterPrefix / …NewRootWithKnownNameNotSwallowed。
//
// known 已按长度降序（见 knownNames），故对同一个 basename，等值候选（len(k)==len(b)）
// 必然排在所有前缀候选（len(k)<len(b)-1）之前——一层循环里先判等值即可，无需二次扫描。
//
// 等值命中分两种，处理不同，差别只在**链首**（目录自身）：
//
//   - 真祖先等值 → 直接归该已知名。~/Projects/Trading-docs/notes 的祖先 basename 就是
//     Trading-docs 本人，没有比这更具体的证据，绝不能再往下用 'Trading-' 前缀去吞。
//
//   - 链首等值 → 仅当该名字是 authoritative（人明确声明过，或有跨根同名证据；见字段说明）
//     才返回**不命中**、把这个目录交回启发式（groupDirs 的证据 (3)「同名目录」会把跨机新根
//     并进正确分量）。不 authoritative 时**继续走前缀**——这是刻意保留的既有契约：
//     Alpha-cmp 这种「只在一个目录上攒了几张卡」的对照工作树，名字虽然进了 known（分量够格），
//     但它本来就该被 pattern 收回 Alpha（TestProjectPriorityChain 的「模式 > 启发式」用例）。
//
// 【已知不对称，登记待裁】未声明、单根、但分量够格的 X-suffix 工作树：它自身 → X（前缀），
// 它的子目录 → X-suffix（祖先等值）——父子分属两个项目。消除它要么让链首等值一律不命中
// （会打掉上面那条「模式 > 启发式」契约），要么让祖先等值也要求 authoritative
// （会让本轮 P1-1 的头号场景 Trading-docs/notes 继续被吞）。两条都动到卡的核心契约，
// 故此处按"少吞并、可见优先"取当前解，差异登记到复盘由设计权威裁定。
// 规避方式与本功能一贯的出口一致：把该名字登记进 project_aliases 或用 -project 钉一次即可
// （登记后两侧一致，靶子：TestProjectPatternDeclaredRootIsNotSwallowed）。
func (r *projectResolver) matchPattern(d string) (string, bool) {
	for i, c := range dirSelfAndAncestors(d) {
		b := dirBase(c)
		for _, k := range r.known {
			if strings.EqualFold(b, k) {
				if i > 0 {
					return k, true
				}
				if r.authoritative[strings.ToLower(k)] {
					return "", false
				}
				continue // 非权威的链首等值：按既有契约继续用更短的已知名做前缀收拢
			}
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
