package main

// boardmodel.go — 看板的领域模型层：把 tasks/ + archive/ 的任务卡聚合成
// 「项目 → 阶段 → 任务」三层视图。
//
// 为什么需要这一层：任务卡里**没有** project / phase / eta 三个字段（见 task.go），
// 队列本身只认「一张卡 + 一个工作目录」。看板要的层级必须**推导**出来：
//   - 项目：从 Dir 归并（同一项目有主仓/worktree 车道/远端 Windows 镜像三种形态）；
//   - 阶段：从标题推导（先剥掉「审核:/修复R1:/复审」这类谱系包装，再取阶段记号）；
//   - ETA ：从历史完成节奏推导（见 boardeta.go）。
// 推导都是启发式，所以每一处都带 *_source 字段或 basis 文案如实说明来源，
// 不确定时宁可标「未分阶段 / 数据不足」，不编一个看起来合理的答案。
//
// 只读纪律：本文件只用 os.ReadFile / os.ReadDir 读数据根，
// 绝不写入任何文件——看板挂在生产队列数据上，任何写入都可能污染真实队列。

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---- 对外 JSON 结构（字段名严格对齐 HTTP API 契约，不得擅改）----

// boardStats 是一组任务的状态计数（Phase/Project 用，带 total）。
type boardStats struct {
	Queued      int `json:"queued"`
	Running     int `json:"running"`
	LimitPaused int `json:"limit_paused"`
	Held        int `json:"held"`
	Failed      int `json:"failed"`
	Done        int `json:"done"`
	Canceled    int `json:"canceled"`
	Total       int `json:"total"`
}

// boardTotals 是 /api/overview 顶层的全局计数。契约里它**没有** total 字段，
// 故与 boardStats 分开定义，避免多吐一个键。
type boardTotals struct {
	Queued      int `json:"queued"`
	Running     int `json:"running"`
	LimitPaused int `json:"limit_paused"`
	Held        int `json:"held"`
	Failed      int `json:"failed"`
	Done        int `json:"done"`
	Canceled    int `json:"canceled"`
}

func (s *boardStats) add(status string) {
	s.Total++
	switch status {
	case statusQueued:
		s.Queued++
	case statusRunning:
		s.Running++
	case statusLimitPaused:
		s.LimitPaused++
	case statusHeld:
		s.Held++
	case statusFailed:
		s.Failed++
	case statusDone:
		s.Done++
	case statusCanceled:
		s.Canceled++
	}
}

// activeTotal 是未终态卡数（排队+运行+限额暂停+挂起）。
func (s *boardStats) activeTotal() int { return s.Queued + s.Running + s.LimitPaused + s.Held }

// schedulable 是**可被调度器自动推进**的卡数。held 不算：挂起卡要等人工 release，
// 把它算进 ETA 会得出一个永远兑现不了的完成时间。
func (s *boardStats) schedulable() int { return s.Queued + s.Running + s.LimitPaused }

// BoardETA 是一个估算结果。三个数值字段都是指针：数据不足时序列化成 null，
// 而不是 0——0 会被前端画成「马上就完」，那是编造。
type BoardETA struct {
	FinishAt   *string  `json:"finish_at"`
	P50Minutes *float64 `json:"p50_minutes"`
	P80Minutes *float64 `json:"p80_minutes"`
	Confidence string   `json:"confidence"`
	Basis      string   `json:"basis"`
}

// BoardModelStat 是一个模型在某项目内的使用计数。
type BoardModelStat struct {
	Model string `json:"model"`
	Tier  string `json:"tier"`
	Count int    `json:"count"`
}

// TaskBrief 是任务卡在看板上的摘要视图。
type TaskBrief struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Desc     string `json:"desc"`
	Status   string `json:"status"`
	Type     string `json:"type"`
	Priority int    `json:"priority"`
	// Step 是 **0-based** 的当前步索引，与任务卡上的原始语义一致
	// （runner.go 用 t.Prompts[t.Step] 取当前 prompt）。展示成「第 N/M 步」时必须 +1，
	// 同一响应里 recent_activity 的文案就是这么渲染的。字段名读起来像「N of M」，
	// 直接渲染 step/steps_total 会得到 0/1，与自家活动流的「第 1/1 步」对不上。
	Step int `json:"step"`
	// StepsTotal 恒等于 prompts 数（与 TaskDetail.prompts_count 同源）。
	StepsTotal  int      `json:"steps_total"`
	Model       string   `json:"model"`
	ModelTier   string   `json:"model_tier"`
	ModelSource string   `json:"model_source"`
	Runner      string   `json:"runner"`
	Effort      string   `json:"effort"`
	ETA         BoardETA `json:"eta"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
	LastSummary string   `json:"last_summary"`
	LastError   string   `json:"last_error"`
	// ElapsedMinutes 只对 running 卡有意义（自最近一次状态变更起算），其余卡为 0。
	ElapsedMinutes float64 `json:"elapsed_minutes"`
	Attempts       int     `json:"attempts"`
	FixRound       int     `json:"fix_round"`
	ReviewOf       string  `json:"review_of"`
	XRole          string  `json:"x_role"`
	RemoteHost     string  `json:"remote_host"`
	BlockedReason  string  `json:"blocked_reason"`
	// Kind 是工作性质（design/impl/fix/review/coord，见 boardkind.go），与 phase 正交：
	// phase 回答"这活属于哪一波"，kind 回答"这是哪种活"。
	Kind string `json:"kind"`
	// KindSource 交代这次判定靠什么得出（review_of / fix_round / type / title / override / default）。
	// 必须与 Kind 成对出现：结构信号是盘上事实、标题关键词是猜的，
	// 只发 kind 会让两者在界面上长得一模一样，等于把猜测伪装成事实。
	KindSource string `json:"kind_source"`
}

// TaskDetail 是 TaskBrief 加上单项目页才需要的重字段（prompt 摘录等）。
// 内嵌 TaskBrief 让 JSON 自动平铺，字段名与契约一致。
type TaskDetail struct {
	TaskBrief
	PromptExcerpt string   `json:"prompt_excerpt"`
	Dir           string   `json:"dir"`
	AllowedTools  []string `json:"allowed_tools"`
	PromptsCount  int      `json:"prompts_count"`
	CostUSD       float64  `json:"cost_usd"`
	TurnsUsed     int      `json:"turns_used"`
}

type Phase struct {
	ID              string      `json:"id"`
	Name            string      `json:"name"`
	Desc            string      `json:"desc"`
	DescSource      string      `json:"desc_source"`
	Status          string      `json:"status"`
	Order           int         `json:"order"`
	Stats           boardStats  `json:"stats"`
	ProgressPercent float64     `json:"progress_percent"`
	ETA             BoardETA    `json:"eta"`
	Tasks           []TaskBrief `json:"tasks"`
}

type Project struct {
	ID              string           `json:"id"`
	Name            string           `json:"name"`
	Desc            string           `json:"desc"`
	DescSource      string           `json:"desc_source"`
	Dirs            []string         `json:"dirs"`
	Stats           boardStats       `json:"stats"`
	ActiveTotal     int              `json:"active_total"`
	ProgressPercent float64          `json:"progress_percent"`
	ETA             BoardETA         `json:"eta"`
	Models          []BoardModelStat `json:"models"`
	LastActivity    string           `json:"last_activity"`
	Phases          []Phase          `json:"phases"`
	// Goal 是「离项目目标多远」的落地进度视图（CG-8）。为什么用指针+omitempty：
	// board.json 未配置 goal 块时，前端契约要求「完全不显示该区块」，用 nil 让 JSON
	// 里彻底不出现该键；如果给零值 struct，前端会把「无目标」误画成「目标 0% 完成」，
	// 那就是编造。ProgressPercent 是「派出的活干完多少」，Goal 是「离目标多远」，
	// 两条并列而非替换——参见 boardgoal.go 顶部注释。
	Goal *ProjectGoal `json:"goal,omitempty"`
	// Kinds 是按工作性质切分的进度切片（设计/落地/修复/审核/协调，见 boardkind.go）。
	// 与 ProgressPercent **并列而非替换**：总条口径不变（全部卡的完成占比），
	// 分桶只是把"总条为什么这么高"摊开——审核卡与修复卡完成率天然高、占比又大，
	// 单看总条会把"落地才走了四成"读成"整体快完了"。
	Kinds []KindProgress `json:"kinds"`
	// KindRuleError 是 board.json kind_rules 里被跳过的规则的披露串（无问题时 omitempty 消失）。
	// 坏规则逐条跳过而非整块拒，但被跳过的必须说出来——静默失效即造读数。
	KindRuleError string `json:"kind_rule_error,omitempty"`

	// ---- 手动归档（视图状态，见 boardarchive.go；不改任何任务卡）----
	// Archived=true 时总览默认折叠该项目。任务卡状态一个字节都不变。
	Archived bool `json:"archived"`
	// ArchivedAt 是人工归档时刻；即使已被新卡自动复活也保留，供前端说清"何时归的档"。
	ArchivedAt string `json:"archived_at,omitempty"`
	// ArchiveRevived=true 表示有归档记录但检测到新卡，已自动切回活跃。
	ArchiveRevived bool `json:"archive_revived,omitempty"`
	// ArchiveRevivedReason 是复活原因的人话说明。只说"恢复了"不说为什么，
	// 用户会怀疑是自己没点上归档按钮。
	ArchiveRevivedReason string `json:"archive_revived_reason,omitempty"`
}

// ---- 快照与缓存 ----

// boardSnapshot 是一次全量扫描的结果。tasks/ + archive/ 合计近 2000 个 JSON，
// 每个 HTTP 请求都重扫会把磁盘打满，故整份快照带 TTL 缓存。
type boardSnapshot struct {
	GeneratedAt time.Time
	Root        string
	Cfg         *Config
	Projects    []*Project
	Totals      boardTotals
	// BoardOverrideError 记录 board.json 加载/解析出错的原因（若无则为空）。
	// 上层 handler 会把它塞进 /api/overview 顶层，前端显式挂告警——
	// **不能**静默返回空 override，那等同"造读数"。
	BoardOverrideError string
	// ProjectAliasError 是 board.json project_aliases 里被跳过的规则的披露串（无问题时为空）。
	// 与 kind_rules 同一纪律：坏规则逐条跳过而非整表拒，但必须说出来——
	// 静默跳过会让用户以为"登记过了"，而看板照旧把那些目录散着放。
	ProjectAliasError string
	// 【R3·P1-2】BoardOverrideErrorKind ∈ {"", "type", "syntax"}：
	//   type 时 name/desc/phases 覆盖仍生效(Unmarshal skip 掉出错字段继续填充),
	//        前端应写"部分覆盖仍生效,出错字段已跳过"；
	//   syntax 时整块覆盖蒸发,前端写"覆盖全部失效"（原文案）。
	// 契约字段名进入前端 board_override_error_kind,不得擅改。
	BoardOverrideErrorKind string
	// byID 保留原始卡，供 /api/project 拼 TaskDetail（避免二次读盘）。
	byID map[string]*Task
	// projTasks 是 project id → 该项目全部原始卡（含归档），ETA 与详情页复用。
	projTasks map[string][]*Task
	// phaseOf 是 task id → 阶段名，避免详情页重算。
	phaseOf map[string]string
	// kindOf 是 task id → 工作性质判定（含来源），/api/project 拼 TaskBrief 时复用。
	// 与 phaseOf 同理：分类是 per-project 的（board.json kind_rules 按项目配），
	// 不能在详情页重算——重算时拿不到项目上下文就会丢掉人工规则。
	kindOf map[string]kindMark
}

// boardCache 给快照做 TTL 缓存。看板是只读视图，10 秒内的陈旧度完全可接受，
// 换来的是近 2000 次文件读被摊掉。
type boardCache struct {
	mu   sync.Mutex
	ttl  time.Duration
	at   time.Time
	snap *boardSnapshot
	err  error
}

func (c *boardCache) get(root string, now time.Time) (*boardSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snap != nil && now.Sub(c.at) < c.ttl {
		return c.snap, c.err
	}
	snap, err := buildSnapshot(root, now)
	c.at, c.snap, c.err = now, snap, err
	return snap, err
}

// ---- 任务读取 ----

// loadArchivedTasks 读 archive/ 里的已归档卡。task.go 的 loadTasks 只覆盖 tasks/，
// 归档目录没有现成 helper，这里按同样的规则读（路径仍走 archiveDir()）。
// 归档卡进看板是必要的：progress_percent 的分母得算上已经清理掉的历史卡，
// 否则 clean 一次进度就凭空跳到 100%。
func loadArchivedTasks(root string) []*Task {
	entries, err := os.ReadDir(archiveDir(root))
	if err != nil {
		return nil // 没有 archive/ 目录是正常状态（从没 clean 过）
	}
	var out []*Task
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(archiveDir(root), e.Name()))
		if err != nil {
			continue
		}
		var t Task
		if json.Unmarshal(data, &t) != nil || t.ID == "" {
			continue // 损坏的归档文件静默跳过，看板不该因为一个坏文件整页挂掉
		}
		out = append(out, &t)
	}
	return out
}

// loadBoardTasks 汇总活跃卡与归档卡。同 ID 以 tasks/ 为准（活跃态更新）。
func loadBoardTasks(root string) ([]*Task, error) {
	live, err := loadTasks(root)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(live))
	for _, t := range live {
		seen[t.ID] = true
	}
	all := append([]*Task(nil), live...)
	for _, t := range loadArchivedTasks(root) {
		if !seen[t.ID] {
			all = append(all, t)
		}
	}
	return all, nil
}

// ---- 项目归并（Dir → 项目）----

// normDir 归一化工作目录：反斜杠转正斜杠、去掉结尾斜杠。
// 远端 Windows 卡的 dir 形如 D:/Project/PO-lanes/wt-l1-core，与本机路径混在同一批里比较。
func normDir(d string) string {
	d = strings.ReplaceAll(strings.TrimSpace(d), "\\", "/")
	for len(d) > 1 && strings.HasSuffix(d, "/") {
		d = strings.TrimSuffix(d, "/")
	}
	return d
}

func dirBase(d string) string {
	if i := strings.LastIndex(d, "/"); i >= 0 {
		return d[i+1:]
	}
	return d
}

func dirParent(d string) string {
	if i := strings.LastIndex(d, "/"); i > 0 {
		return d[:i]
	}
	return ""
}

// laneSuffixes 是「车道容器目录」的命名后缀：<项目>-lanes/<车道> 属于 <项目>。
var laneSuffixes = []string{"-lanes", "-worktrees", "-worktree", "-lane", "-wt"}

// genericBase 是过于通用的目录名——不能仅凭同名就判定两个路径是同一项目
// （两个不同项目都可能有 docs/ 或 src/）。
var genericBase = map[string]bool{
	"docs": true, "doc": true, "src": true, "app": true, "web": true, "test": true,
	"tests": true, "tmp": true, "build": true, "dist": true, "lib": true, "bin": true,
	"cmd": true, "internal": true, "pkg": true, "services": true, "packages": true,
	"scripts": true, "config": true, "data": true, "core": true, "main": true,
	"server": true, "client": true, "api": true, "ui": true, "project": true, "projects": true,
}

// dsu 是并查集：把所有 dir 变体连成连通分量，一个分量 = 一个项目。
// 为什么用并查集而不是「前缀匹配 remote_mirror_root」：实测 remote_mirror_root
// （D:/Project/PO-lanes）下同时挂着 PerlicaOptimize / PerlicaHermes / PerlicaTLink
// 三个项目的镜像，按前缀归并会把三个项目错误合并成一个。
type dsu struct{ parent map[string]string }

func newDSU() *dsu { return &dsu{parent: map[string]string{}} }

func (d *dsu) find(x string) string {
	if _, ok := d.parent[x]; !ok {
		d.parent[x] = x
		return x
	}
	for d.parent[x] != x {
		d.parent[x] = d.parent[d.parent[x]] // 路径压缩
		x = d.parent[x]
	}
	return x
}

func (d *dsu) union(a, b string) {
	ra, rb := d.find(a), d.find(b)
	if ra != rb {
		d.parent[ra] = rb
	}
}

// groupDirs 把所有出现过的工作目录归并成项目分量，返回 dir → 分量代表。
//
// 四类归并证据，强度从高到低：
//  1. 显式镜像对：同一张卡的 Dir 与 ReviewDir 天然是同一项目的两个副本
//     （复审分流把实现卡镜像到第二台机器）。这是盘上的**事实**，不是猜测。
//  2. 车道结构：<X>-lanes/<车道> 与 <X> 同项目——但要求 <X> 确实被某张卡用过，
//     否则不凭空造一个项目根。
//  3. 同名目录：跨平台镜像常同名（D:/Project/Trading 与 ~/Projects/Trading）。
//     通用名（docs/src/…）排除在外。
//  4. 祖先包含：某卡的目录落在另一个已知项目目录之下（如 Trading/QMT）即归入其中。
//     带容器护栏：若该祖先下挂着 ≥5 个已知目录，它更像工作区容器而非单个项目，不归并。
func groupDirs(tasks []*Task) map[string]string {
	observed := map[string]int{}
	for _, t := range tasks {
		if d := normDir(t.Dir); d != "" {
			observed[d]++
		}
	}
	u := newDSU()
	for d := range observed {
		u.find(d)
	}

	// (1) 显式镜像对
	for _, t := range tasks {
		d, rd := normDir(t.Dir), normDir(t.ReviewDir)
		if d != "" && rd != "" {
			u.find(d)
			u.find(rd)
			u.union(d, rd)
		}
	}

	all := make([]string, 0, len(u.parent))
	for d := range u.parent {
		all = append(all, d)
	}

	// (2) 车道结构
	for _, d := range all {
		parent := dirParent(d)
		pb := dirBase(parent)
		for _, suf := range laneSuffixes {
			if strings.HasSuffix(pb, suf) && len(pb) > len(suf) {
				sib := dirParent(parent) + "/" + strings.TrimSuffix(pb, suf)
				if _, ok := observed[sib]; ok {
					u.union(d, sib)
				}
				break
			}
		}
	}

	// (3) 同名目录
	byBase := map[string][]string{}
	for _, d := range all {
		b := dirBase(d)
		if b == "" || genericBase[strings.ToLower(b)] {
			continue
		}
		byBase[b] = append(byBase[b], d)
	}
	for _, ds := range byBase {
		for i := 1; i < len(ds); i++ {
			u.union(ds[0], ds[i])
		}
	}

	// (4) 祖先包含（带容器护栏）
	descendants := map[string]int{}
	for _, d := range all {
		for o := range observed {
			if o != d && strings.HasPrefix(d, o+"/") {
				descendants[o]++
			}
		}
	}
	for _, d := range all {
		p := dirParent(d)
		for lvl := 0; lvl < 6 && p != "" && strings.Contains(p, "/"); lvl++ {
			if _, ok := observed[p]; ok && descendants[p] < 5 {
				u.union(d, p)
				break
			}
			p = dirParent(p)
		}
	}

	out := make(map[string]string, len(all))
	for _, d := range all {
		out[d] = u.find(d)
	}
	return out
}

// slugify 把项目名转成稳定的 URL id：驼峰拆成短横线小写（PerlicaOptimize → perlica-optimize）。
func slugify(name string) string {
	var b strings.Builder
	prevLower := false
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			if prevLower {
				b.WriteByte('-')
			}
			b.WriteRune(r - 'A' + 'a')
			prevLower = false
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevLower = true
		default:
			b.WriteByte('-')
			prevLower = false
		}
	}
	s := b.String()
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-")
	if s == "" {
		// 非 ASCII 名（中文目录）会被上面的循环吞成空串。返回固定的 "project"
		// 等于让**任意两个**中文命名的项目 100% 撞 id，故退回内容哈希保持可区分。
		s = "p-" + shortHash(name)
	}
	return s
}

// shortHash 是 FNV-1a 的短十六进制摘要，用于生成稳定且与遍历顺序无关的 id 后缀。
func shortHash(s string) string {
	var h uint64 = 1469598103934665603
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return strconv.FormatUint(h, 16)[:8]
}

// pickProjectDir 在一个分量里挑最能代表项目的目录作为项目名来源：
// 优先「本机绝对路径 + 非车道目录 + 卡最多」，退而求其次取卡最多的。
func pickProjectDir(dirs []string, count map[string]int) string {
	isLane := func(d string) bool {
		pb := dirBase(dirParent(d))
		for _, suf := range laneSuffixes {
			if strings.HasSuffix(pb, suf) {
				return true
			}
		}
		return false
	}
	best, bestScore := "", -1
	for _, d := range dirs {
		score := count[d] * 10
		if strings.HasPrefix(d, "/") {
			score += 5000 // 本机路径优先于 D:/ 镜像
		}
		if !isLane(d) {
			score += 2000 // 项目根优先于车道
		}
		if score > bestScore || (score == bestScore && d < best) {
			best, bestScore = d, score
		}
	}
	return best
}

// ---- 阶段推导（标题 → 阶段）----

var (
	// wrapperRe 是任务标题的「谱系包装」前缀：审核卡 / 修复轮次卡 / 方括号批注。
	// 必须先递归剥掉才能看到根任务的阶段记号——
	// 「审核: 修复R1: L1.4-b …」属于 L1 阶段，而不是自成一类。
	wrapperRe = regexp.MustCompile(`^\s*(?:\[[^\]]*\]\s*)|^\s*(?:审核|复审|复核|终审|裁决|收口|进度|backfill)\s*[:：]\s*|^\s*(?:审核|复审|复核|终审)\s+|^\s*(?:backfill)?修复(?:R\d+)?\s*[:：]\s*`)
	// 阶段记号：波次(wave3/W4)、里程碑(H0/P1/F2)、叶卡道(L1)、以及 IMPL-03 / NET-01 这类带前缀编号。
	phaseWaveRe   = regexp.MustCompile(`^(?i:wave)[-_ ]?(\d+)$`)
	phaseAlphaNum = regexp.MustCompile(`^([A-Za-z]{1,6})-(\d{1,3})`)
	// 单字母+数字（W4/H0/L1/P0），第 3 组捕获 .9-c 这类子段后缀（见 derivePhase 的位置规则）。
	// 刻意排除 v/V 开头：那是版本号（"LANES v2:wave-3"）而非阶段记号，
	// 放进来不但会造出假阶段，还会把同一标题里更靠后、更准确的 wave-3 记号挡掉。
	phaseLetterRe = regexp.MustCompile(`^([A-UWXYZa-uwxyz])(\d{1,2})([.\-_·].*)?$`)
	// 任务号里内嵌阶段：T-P1-04 的真阶段是中段的 P1。
	// 这类 token 两条主规则都匹配不上（phaseAlphaNum 要求 "T-" 后面直接跟数字），
	// 于是扫描会继续后移，命中后面的子系统号 S3/S4/S6——实测把同属 P1 的 5 张卡
	// 打散成 5 个各含 1 张卡的假阶段。
	phaseEmbeddedRe = regexp.MustCompile(`^[A-Za-z]-([A-Za-z]\d{1,2})-\d{1,3}$`)
	phaseSplitRe    = regexp.MustCompile(`[\s:：·\[\]（）()]+`)

	// modelPrefix 是会被 phaseAlphaNum 误吃成阶段记号的模型标签前缀。
	// 实测 "TLink 设计优化+代码审查 [gpt-5.6-sol·…]" 被判成阶段 "GPT-5"，
	// 还把两个不同档位的模型（gpt-5.6-sol 与 gpt-5.5）并进同一个假阶段。
	modelPrefix = map[string]bool{
		"gpt": true, "claude": true, "opus": true, "sonnet": true, "fable": true,
		"haiku": true, "sol": true, "luna": true, "terra": true,
		"gemini": true, "grok": true, "llama": true, "qwen": true,
	}
)

// normPhaseMark 归一化单字母+数字记号。W/w 开头统一折成 waveN——
// 同一个波次实测有 wave3 与 W3 两种写法（本文件的注释也把它们列为同一类记号），
// 不归一就会分裂成两个互不相干的阶段，各算各的 stats / progress / ETA，
// 连同一片叶子的规划卡与实现卡都会被分到两个桶里。
func normPhaseMark(letter, num string) string {
	u := strings.ToUpper(letter)
	if u == "W" {
		return "wave" + num
	}
	return u + num
}

const phaseUnsorted = "未分阶段"

// derivePhase 从标题推导阶段名。返回空串表示识别不出（调用方落到「未分阶段」）。
func derivePhase(title string) string {
	t := strings.TrimSpace(title)
	// 递归剥包装：审核卡可能嵌套多层（审核: 修复R3: 修复R5·终局: …）
	for i := 0; i < 8; i++ {
		nt := strings.TrimSpace(wrapperRe.ReplaceAllString(t, ""))
		if nt == t {
			break
		}
		t = nt
	}
	// 再剥掉自动修复卡的轮次前缀与判定尾注（复用 runner.go 既有规则）
	t = strings.TrimSpace(baseFixTitle(t))

	// 只扫标题最前面的几个 token：阶段记号总在开头，越往后越可能是正文里的偶然编号。
	// 为什么不是只看第 1 个：包装词的分隔符并不统一，实测有「修复R5·终局: L1.9-c …」
	// 这种用 · 而非冒号连接的写法，剥不掉包装，阶段记号就落到了第 3 个 token 上。
	scanned := 0
	for _, tok := range phaseSplitRe.Split(t, -1) {
		if tok == "" {
			continue
		}
		if scanned++; scanned > 4 {
			break
		}
		if m := phaseWaveRe.FindStringSubmatch(tok); m != nil {
			return "wave" + m[1]
		}
		// 先试「任务号内嵌阶段」（T-P1-04 → P1），否则扫描会滑过去命中后面的子系统号。
		if m := phaseEmbeddedRe.FindStringSubmatch(tok); m != nil {
			return normPhaseMark(m[1][:1], m[1][1:])
		}
		if m := phaseAlphaNum.FindStringSubmatch(tok); m != nil && !modelPrefix[strings.ToLower(m[1])] {
			return strings.ToUpper(m[1]) + "-" + m[2]
		}
		if m := phaseLetterRe.FindStringSubmatch(tok); m != nil {
			// 位置规则：**裸**记号（A2）只在标题最前面两个 token 里可信，靠后的多半是
			// 正文里的文档编号——实测「skill2loop 评估收尾:A2 走附录+登记规范」被判成阶段 A2。
			// 带子段的记号（L1.9-c）结构性足够强，靠后也认：包装词的分隔符并不统一，
			// 「修复R5·终局: L1.9-c」剥不掉包装，真记号就落在第 3 个 token 上。
			if scanned <= 2 || m[3] != "" {
				return normPhaseMark(m[1], m[2])
			}
		}
	}
	return ""
}

// phaseForTask 决定一张卡归哪个阶段。审核卡优先**继承被审卡**的阶段
// （review_of 是盘上的确定链接，比标题解析可靠），解析不出再退回标题推导。
func phaseForTask(t *Task, byID map[string]*Task, depth int) string {
	if t.ReviewOf != "" && depth < 4 {
		if parent, ok := byID[t.ReviewOf]; ok {
			if p := phaseForTask(parent, byID, depth+1); p != "" && p != phaseUnsorted {
				return p
			}
		}
	}
	if p := derivePhase(t.Title); p != "" {
		return p
	}
	return phaseUnsorted
}

// ---- 模型等级 ----

// modelTier 把模型名映射成人话等级。GPT 侧按对等档位对齐 claude 侧
// （sol≈fable 设计档 / terra≈opus 实现档 / luna≈sonnet）。
func modelTier(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case m == "":
		return "未知"
	case strings.Contains(m, "fable"), strings.Contains(m, "sol"):
		return "旗舰"
	case strings.Contains(m, "opus"), strings.Contains(m, "terra"):
		return "高"
	case strings.Contains(m, "sonnet"), strings.Contains(m, "luna"):
		return "中"
	case strings.Contains(m, "haiku"):
		return "轻"
	default:
		return "未知"
	}
}

// effectiveModel 还原一张卡**实际生效**的模型与来源。
// 三条路径：codex 系走 resolveCodexModel（含交叉冻结/卡级钉定/降级专用/全局回落），
// claude 系用卡上的 Model，卡上为空则回填 type_defaults。
func effectiveModel(cfg *Config, t *Task) (model, source string) {
	codexSide := t.Runner == "codex" || t.PreferRunner == "codex" ||
		(strings.HasPrefix(t.Runner, "remote:") && t.Model == "") ||
		(t.RemoteHost != "" && t.Model == "")
	if codexSide {
		if m := resolveCodexModel(cfg, t); m != "" {
			return m, "codex_model"
		}
	}
	if t.Model != "" {
		return t.Model, "task"
	}
	// 走 typeDefaultsFor 而非裸查表：用户部分覆写该类型（只写 allowed_tools 之类）时，
	// 裸查表拿到的 Model 是空串，看板会把这张卡显示成"无模型"，而 newTask 烘焙进卡的是内置模型——
	// 展示与实际不一致。靶：TestEffectiveModelUsesMergedTypeDefault。
	if td, ok := typeDefaultsFor(cfg, t.Type); ok && td.Model != "" {
		return td.Model, "type_default"
	}
	return "", "task"
}

// ---- 任务摘要 ----

var (
	mdNoiseRe   = regexp.MustCompile(`^[#>*\-\s]+`)
	spaceRunRe  = regexp.MustCompile(`\s+`)
	sentenceEnd = regexp.MustCompile(`[。！？!?;；]`)
)

// taskDesc 从首步 prompt 提炼 1-2 句人话摘要。prompt 常以 markdown 标题/角色声明开场，
// 逐行找到第一段有实质内容的文字再截断（按 rune 截，中文不能按字节切）。
func taskDesc(t *Task) string {
	if len(t.Prompts) == 0 {
		return ""
	}
	for _, line := range strings.Split(t.Prompts[0], "\n") {
		line = strings.TrimSpace(mdNoiseRe.ReplaceAllString(line, ""))
		line = spaceRunRe.ReplaceAllString(line, " ")
		if len([]rune(line)) < 8 {
			continue // 跳过分隔线、单词标签这类噪声行
		}
		// 截到第一个句末标点，太长再按 rune 硬截
		if loc := sentenceEnd.FindStringIndex(line); loc != nil && len([]rune(line[:loc[1]])) >= 12 {
			line = line[:loc[1]]
		}
		r := []rune(line)
		if len(r) > 110 {
			return string(r[:110]) + "…"
		}
		return line
	}
	return ""
}

// blockedReason 给 held/failed 卡一句人话的卡住原因。
func blockedReason(t *Task) string {
	switch t.Status {
	case statusHeld:
		if strings.Contains(t.Title, "超轮限") {
			return "修复轮次超过上限，已升级为待人工裁定"
		}
		if t.EmitHold {
			return "产出卡默认挂起，等待人工 release 放行"
		}
		return "已挂起，等待人工 release 放行"
	case statusFailed:
		if t.LastError != "" {
			e := oneLine(t.LastError)
			r := []rune(e)
			if len(r) > 120 {
				return string(r[:120]) + "…"
			}
			return e
		}
		return "执行失败，无错误详情"
	}
	return ""
}

func parseRFC3339(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

// toBrief 把原始卡转成看板摘要。eta 由调用方补（需要项目级节奏样本）。
func toBrief(cfg *Config, t *Task, now time.Time) TaskBrief {
	model, source := effectiveModel(cfg, t)
	b := TaskBrief{
		ID:            t.ID,
		Title:         t.Title,
		Desc:          taskDesc(t),
		Status:        t.Status,
		Type:          t.Type,
		Priority:      t.Priority,
		Step:          t.Step,
		StepsTotal:    len(t.Prompts),
		Model:         model,
		ModelTier:     modelTier(model),
		ModelSource:   source,
		Runner:        t.Runner,
		Effort:        t.Effort,
		CreatedAt:     t.CreatedAt,
		UpdatedAt:     t.UpdatedAt,
		LastSummary:   t.LastSummary,
		LastError:     t.LastError,
		Attempts:      t.Attempts,
		FixRound:      t.FixRound,
		ReviewOf:      t.ReviewOf,
		XRole:         t.XRole,
		RemoteHost:    t.RemoteHost,
		BlockedReason: blockedReason(t),
	}
	if b.Runner == "" {
		b.Runner = "claude" // 空 runner = 本机 claude，前端不该猜
	}
	if t.Status == statusRunning {
		if ts, ok := parseRFC3339(t.UpdatedAt); ok {
			if m := now.Sub(ts).Minutes(); m > 0 {
				b.ElapsedMinutes = round1(m)
			}
		}
	}
	return b
}

// applyKind 把分类结果贴到摘要上。空 mark（理论上不该出现：kindOf 由同一批卡填充）
// 落到 impl/"default"，与 buildKindProgress 的兜底保持一致——两处若各自兜底成不同的桶，
// 单卡显示的类别与该类别的计数就会对不上。
func (b *TaskBrief) applyKind(m kindMark) {
	if m.Kind == "" {
		m = kindMark{Kind: kindImpl, Source: "default"}
	}
	b.Kind, b.KindSource = m.Kind, m.Source
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }

// ---- 看板覆盖文件（board.json）----

// boardOverride 是可选的人工文案覆盖：<root>/board.json。
// 自动推导的项目/阶段介绍难免干瘪，允许人工写一份更准的；文件不存在就全部走推导。
// 看板只读它，永不写它。
//
// root 是加载时的目录，供 goal.evidence.path 相对路径解析用（相对 board.json 所在目录，
// 不是进程 CWD——launchd/systemd/手动 cd 会漂移）。它由 loadBoardOverride 塞入，
// 不进 JSON，也不对外暴露；因此 struct tag 用 `json:"-"`。
type boardOverride struct {
	Projects map[string]boardOverrideProject `json:"projects"`
	// ProjectAliases 是有序的目录→项目归组规则表（BD-45，见 boardproject.go）。
	// 放在**顶层**而不是某个项目下：它的作用正是决定"这个目录属于哪个项目"，
	// 挂进项目块就成了先有鸡再有蛋。改这张表不动任何任务卡，下次快照重建全量追溯生效
	// ——这就是存量野项目的整理机制。
	ProjectAliases []boardProjectAlias `json:"project_aliases,omitempty"`
	root           string              `json:"-"`
}

// boardOverrideProject 是 board.json 里单个项目的覆盖块。
// 拆成命名类型是为了容纳 CG-8 的 goal 字段——原先内联匿名 struct 无法从别的文件
// （boardgoal.go）引用，加字段就要动这里的 shape，索性一次拆干净。
type boardOverrideProject struct {
	Name   string             `json:"name"`
	Desc   string             `json:"desc"`
	Phases map[string]string  `json:"phases"`
	Goal   *boardOverrideGoal `json:"goal,omitempty"`
	// KindRules 是人工分类规则（见 boardkind.go）。标题关键词启发式再怎么调都会判错几张卡，
	// 给一个精确出口比继续往 kindDesignHints 里堆词更诚实——堆词会让别的项目跟着遭殃。
	KindRules []boardOverrideKindRule `json:"kind_rules,omitempty"`
}

// boardOverrideKindRule 是单条人工分类规则：match 命中标题子串（大小写不敏感）或任务 ID 全串，
// 命中即把该卡判成 kind。首条命中即用，优先级高于所有自动判定。
type boardOverrideKindRule struct {
	Match string `json:"match"`
	Kind  string `json:"kind"`
}

// 【R3·P1-2】overrideErrKind 分类:app.js 需按这个把顶端横幅的文案区分开 ——
//   "type" 场景 name/desc/phases 覆盖仍生效(Unmarshal skip 掉出错字段继续填充其它),
//       写"全部失效"是失实披露(fail-honest 卡的披露自身失实,自身破线)。
//   "syntax" 场景整块 override 蒸发,前端应写"全部失效,回落自动推导"。
// 常量导出到包级,前端与测试可 grep 到具体字符串保防误改。
const (
	overrideErrKindSyntax = "syntax"
	overrideErrKindType   = "type"
)

// loadBoardOverride 读 <root>/board.json。返回 (override, 错误串, 错误分类)：
//   - 文件不存在（IsNotExist）→ 返回空 override + err="" + kind="" ，等价"未配置"；
//   - 文件存在但 JSON 语法错 → 返回空 override + 具体错误描述 + kind="syntax"（部分保留不可能）；
//   - 文件存在但**字段类型错**（如 goal.weight 写成字符串、done_percent 写成 "50%"）→
//     保留 Unmarshal 已尽力填充的部分（name/desc/phases 与其它无手误项目）+ 错误描述 + kind="type"。
//
// 为什么解析错误不能静默返回空 override：原实现把「配了 override 但一个逗号打错 / 抄了 jsonc 注释」
// 与「根本没有 override 文件」压成同一个状态，含 name/desc/phases/goal 在内的所有覆盖块
// 会集体静默蒸发；这违反本卡的 fail-honest 纪律——「无声降级」正是"造读数"的一种，
// 用户以为看到的是自己配的项目名，实际看到的是自动推导结果。落错 + 前端显式披露方能闭环。
//
// 为什么区分类型错与语法错：Go encoding/json 的语义是「遇到字段类型不匹配（*UnmarshalTypeError）
// 会 skip 该字段但继续解析剩余字段，尽力填充后返回最早的类型错」。CG-8 新加 weight/done_percent/
// max_age_hours 三个数值字段——委托人在 desc 里写"50%"这种手误概率高，不能因为一个 milestone 的
// weight 写成字符串就把整个 board.json（含所有项目的 name/desc/phases）连坐蒸发。语法错（逗号少了、
// 括号不闭合、抄了 jsonc 注释）无法部分保留，必须整块丢弃。
//
// 【R3·P1-2】为什么加 kind 第三返值:上一轮 web/app.js:511 顶端横幅无条件写"项目 name/desc/phases/goal
// 覆盖已全部失效,页面显示回落自动推导",但类型错场景 name/desc/phases 明明生效(见
// boardgoal_test.go TestLoadBoardOverrideTypeErrorPreservesOtherFields)——fail-honest 披露自身失实。
// 前端要区分两态:type 显示"部分覆盖仍生效,出错字段已跳过",syntax 显示现有"全部失效"文案。
// 教训（tool-output-reliability）：闸门级读数任何"静默连坐"都是造读数——凡有降级必有披露。
func loadBoardOverride(root string) (*boardOverride, string, string) {
	path := filepath.Join(root, "board.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &boardOverride{root: root}, "", ""
		}
		msg := "读 board.json 失败: " + err.Error()
		log.Printf("[board] %s", msg)
		// I/O 错(非 IsNotExist)也走整块丢——保守按 syntax 归类,前端显示"全部失效"。
		return &boardOverride{root: root}, msg, overrideErrKindSyntax
	}
	var o boardOverride
	if jerr := json.Unmarshal(data, &o); jerr != nil {
		// 【R2·P1-2】msg 加自描述前缀,让两态从错误串本身可辨识——belt-and-suspenders:
		// error_kind 是首选信道,前缀是备份信道。若前端旧版未升级仍看错误串、或日志/告警
		// 通道只透原始错误串(如 log grep、监控告警),用户仍能从前缀立即认出降级形态。
		// 上一轮 review 抓的正是「后端两分支错误串前缀相同,前端无从区分」——加自描述前缀,
		// 让分类信号在 error_kind 之外多一条独立通道,防止未来某处消费方遗漏 kind 字段时又静默。
		var typeErr *json.UnmarshalTypeError
		if errors.As(jerr, &typeErr) {
			// 字段类型手误:Unmarshal skip 出错字段但继续填充其余——保留部分结果 + 披露 + kind=type。
			msg := "board.json 部分字段类型手误(出错字段已按 *UnmarshalTypeError 跳过,其余 name/desc/phases/goal 覆盖仍生效): " + jerr.Error()
			log.Printf("[board] %s", msg)
			o.root = root
			return &o, msg, overrideErrKindType
		}
		// 语法错(注释/尾逗号/括号不闭合等)无法部分解析:整块丢 + 披露 + kind=syntax。
		// 保留"注释/尾逗号"帮助自诊,委托人抄 README jsonc 示例是高频坑。
		msg := "board.json 整块 JSON 语法错(整个 override 失效,回落自动推导；提示：board.json 是严格 JSON,不接受注释/尾逗号): " + jerr.Error()
		log.Printf("[board] %s", msg)
		return &boardOverride{root: root}, msg, overrideErrKindSyntax
	}
	o.root = root
	return &o, "", ""
}

// ---- 快照构建 ----

func buildSnapshot(root string, now time.Time) (*boardSnapshot, error) {
	cfg, err := loadConfig(root)
	if err != nil {
		return nil, err
	}
	tasks, err := loadBoardTasks(root)
	if err != nil {
		return nil, err
	}
	ov, ovErr, ovErrKind := loadBoardOverride(root)

	byID := make(map[string]*Task, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}

	dirCount := map[string]int{}
	for _, t := range tasks {
		if d := normDir(t.Dir); d != "" {
			dirCount[d]++
		}
	}
	// 归属判定（BD-45）：显式 > 别名 > 模式 > 启发式 > 未分类，见 boardproject.go。
	// 分组的键从"启发式分量代表"改成了**项目名**——显式字段与别名表给出的是名字，
	// 只有按名字分组，「-project PerlicaHermes 的卡」与「~/Projects/PerlicaHermes 的卡」
	// 才会落进同一个项目而不是两个同名项目。
	res := newProjectResolver(tasks, ov.ProjectAliases)

	projTaskList := map[string][]*Task{}
	projDirs := map[string]map[string]bool{}
	var names []string
	touchProject := func(name string) {
		if _, ok := projDirs[name]; !ok {
			projDirs[name] = map[string]bool{}
			names = append(names, name)
		}
	}
	// 兜底桶恒存在：0 张卡时也要出现在总览里（空收件箱本身就是信息）。
	touchProject(unclassifiedProject)
	for _, t := range tasks {
		name, _ := res.resolve(t)
		touchProject(name)
		projTaskList[name] = append(projTaskList[name], t)
		if d := normDir(t.Dir); d != "" {
			projDirs[name][d] = true
		}
		// ReviewDir 也要登记：只登记 Dir 的话，「只作为 review_dir 出现」的远端镜像目录
		// ——也就是复审分流真正落地执行的那个目录——在整个 API 里一处都看不到。
		if rd := normDir(t.ReviewDir); rd != "" {
			projDirs[name][rd] = true
		}
	}

	snap := &boardSnapshot{
		GeneratedAt:            now,
		Root:                   root,
		Cfg:                    cfg,
		BoardOverrideError:     ovErr,
		BoardOverrideErrorKind: ovErrKind,
		ProjectAliasError:      res.aliasErr,
		byID:                   byID,
		projTasks:              map[string][]*Task{},
		phaseOf:                map[string]string{},
		kindOf:                 map[string]kindMark{},
	}

	// 项目必须按稳定键遍历：Go 的 map 迭代顺序是随机化的，
	// 而下面的 slug 冲突消解依赖遍历顺序。不排序的话，两个 slugify 到同一个串的项目
	// 每次重建快照都可能互换 id —— 而 id 是 /api/project?id= 的主键，
	// 前端书签、刷新、跨缓存代的两个请求会静默指向另一个项目。
	sort.Strings(names)

	// 先数一遍每个 slug 被几个项目占用：只要有冲突，**所有**冲突方都带内容哈希后缀。
	// 不能只给「后来者」加后缀——那样新项目一旦排到前面，老项目的 id 就会被挤走。
	// 例外是兜底桶：它的 id 固定为 unclassifiedProjectID，冲突时让**对方**带后缀——
	// 收件箱的 id 一旦漂移，board_archive.json 里的归档记录与前端书签就集体失联。
	slugUsers := map[string]int{}
	for _, name := range names {
		slugUsers[projectSlug(name)]++
	}

	for _, name := range names {
		ts := projTaskList[name]
		dirs := make([]string, 0, len(projDirs[name]))
		for d := range projDirs[name] {
			dirs = append(dirs, d)
		}
		sort.Slice(dirs, func(i, j int) bool {
			if dirCount[dirs[i]] != dirCount[dirs[j]] {
				return dirCount[dirs[i]] > dirCount[dirs[j]]
			}
			return dirs[i] < dirs[j]
		})

		id := projectSlug(name)
		if slugUsers[id] > 1 && name != unclassifiedProject {
			id += "-" + shortHash(name)
		}

		p := buildProject(cfg, ov, id, name, dirs, ts, byID, now, snap.phaseOf, snap.kindOf)
		if name == unclassifiedProject && p.DescSource != "override" {
			p.Desc = unclassifiedDesc + "（当前 " + p.Desc + "）"
		}
		snap.Projects = append(snap.Projects, p)
		snap.projTasks[id] = ts
	}

	for _, t := range tasks {
		snap.Totals.addStatus(t.Status)
	}

	// 项目排序：有活儿的排前面（未终态卡多者优先），其次按最近活动时间。
	sort.Slice(snap.Projects, func(i, j int) bool {
		a, b := snap.Projects[i], snap.Projects[j]
		if (a.ActiveTotal > 0) != (b.ActiveTotal > 0) {
			return a.ActiveTotal > 0
		}
		if a.ActiveTotal != b.ActiveTotal {
			return a.ActiveTotal > b.ActiveTotal
		}
		if a.LastActivity != b.LastActivity {
			return a.LastActivity > b.LastActivity
		}
		return a.Name < b.Name
	})
	return snap, nil
}

func (t *boardTotals) addStatus(status string) {
	switch status {
	case statusQueued:
		t.Queued++
	case statusRunning:
		t.Running++
	case statusLimitPaused:
		t.LimitPaused++
	case statusHeld:
		t.Held++
	case statusFailed:
		t.Failed++
	case statusDone:
		t.Done++
	case statusCanceled:
		t.Canceled++
	}
}

// maxPhaseTasks 是总览里每个阶段最多带的任务数（单项目页不截断）。
const maxPhaseTasks = 40

func buildProject(cfg *Config, ov *boardOverride, id, name string, dirs []string,
	ts []*Task, byID map[string]*Task, now time.Time,
	phaseOf map[string]string, kindOf map[string]kindMark) *Project {

	p := &Project{ID: id, Name: name, Dirs: dirs, DescSource: "derived"}

	// 人工分类规则先取出来：分桶时每张卡都要过一遍，不能在循环里反复解析。
	// 坏规则逐条跳过并把披露串挂到项目上（见 parseKindRules 的注释）。
	var kindRules []boardKindRule
	if o, ok := ov.Projects[id]; ok && len(o.KindRules) > 0 {
		kindRules, p.KindRuleError = parseKindRules(o.KindRules)
	}

	// 阶段分桶（同时定工作性质：两者正交，一趟循环各记各的）
	phaseTasks := map[string][]*Task{}
	for _, t := range ts {
		ph := phaseForTask(t, byID, 0)
		phaseOf[t.ID] = ph
		kindOf[t.ID] = deriveTaskKind(t, kindRules)
		phaseTasks[ph] = append(phaseTasks[ph], t)
		p.Stats.add(t.Status)
		if t.UpdatedAt > p.LastActivity {
			p.LastActivity = t.UpdatedAt
		}
	}
	p.ActiveTotal = p.Stats.activeTotal()
	p.ProgressPercent = progressPercent(&p.Stats)
	p.Kinds = buildKindProgress(ts, kindOf)

	// 模型分布（覆盖项目全部卡：这是「这个项目在用什么档位的模型」的完整画像）
	mc := map[string]int{}
	for _, t := range ts {
		m, _ := effectiveModel(cfg, t)
		if m == "" {
			m = "(账号默认)"
		}
		mc[m]++
	}
	for m, c := range mc {
		p.Models = append(p.Models, BoardModelStat{Model: m, Tier: modelTier(m), Count: c})
	}
	sort.Slice(p.Models, func(i, j int) bool {
		if p.Models[i].Count != p.Models[j].Count {
			return p.Models[i].Count > p.Models[j].Count
		}
		return p.Models[i].Model < p.Models[j].Model
	})

	// 项目节奏样本 → 项目 ETA
	pace := newPaceModel(cfg, ts, now)
	p.ETA = pace.estimate(p.Stats.schedulable(), p.Stats.Held, "项目")

	// 阶段。siblings 一律传项目全集 ts：调度器没有阶段概念，
	// 阶段内排位没有调度意义，传阶段切片会让同一张卡在同一个响应里出现两个 finish_at。
	for phName, pts := range phaseTasks {
		ph := buildPhase(cfg, id, phName, pts, ts, now, pace, kindOf)
		if o, ok := ov.Projects[id]; ok {
			if d, ok2 := o.Phases[phName]; ok2 && d != "" {
				ph.Desc, ph.DescSource = d, "override"
			}
		}
		p.Phases = append(p.Phases, ph)
	}
	// 待推进阶段在前，已完成/已取消在后；组内按最早创建时间（自然的推进顺序）
	sort.Slice(p.Phases, func(i, j int) bool {
		a, b := p.Phases[i], p.Phases[j]
		ad, bd := phaseSettled(a.Status), phaseSettled(b.Status)
		if ad != bd {
			return !ad
		}
		if (a.Name == phaseUnsorted) != (b.Name == phaseUnsorted) {
			return b.Name == phaseUnsorted // 未分阶段永远垫底
		}
		return phaseFirstCreated(a) < phaseFirstCreated(b)
	})
	for i := range p.Phases {
		p.Phases[i].Order = i
	}

	// 介绍：override 优先，否则自动推导
	p.Desc = derivedProjectDesc(p, dirs)
	if o, ok := ov.Projects[id]; ok {
		if o.Name != "" {
			p.Name = o.Name
		}
		if o.Desc != "" {
			p.Desc, p.DescSource = o.Desc, "override"
		}
		// 目标锚定进度（CG-8）：goal 块缺失时 buildProjectGoal 返回 nil，
		// Project.Goal 的 omitempty 保证 JSON 里不出现该键——前端"不显示"契约成立。
		// root 是 board.json 所在目录，evidence.path 相对路径按它解析（不用进程 CWD）。
		p.Goal = buildProjectGoal(o.Goal, ov.root, now)
	}

	// 注意：这里**不**截断 phases[].tasks——/api/project 契约要求完整清单。
	// 总览的 40 条上限在序列化时用 projectForOverview 做浅拷贝截断，两个端点共用同一份快照。
	return p
}

// projectForOverview 返回适合总览的项目副本：每阶段任务清单截到 maxPhaseTasks 条。
// 浅拷贝 + 重切片即可——切片头是值拷贝，截断副本不会动到快照里的原始数据。
func projectForOverview(p *Project) *Project {
	cp := *p
	cp.Phases = make([]Phase, len(p.Phases))
	for i, ph := range p.Phases {
		c := ph
		if len(c.Tasks) > maxPhaseTasks {
			c.Tasks = c.Tasks[:maxPhaseTasks]
		}
		cp.Phases[i] = c
	}
	return &cp
}

// phaseFirstCreated 取阶段内最早的创建时间，作为阶段的自然排序键。
func phaseFirstCreated(p Phase) string {
	best := ""
	for _, t := range p.Tasks {
		if best == "" || (t.CreatedAt != "" && t.CreatedAt < best) {
			best = t.CreatedAt
		}
	}
	return best
}

// progressPercent 是完成占比。分母**排除已取消卡**：取消卡既不是完成、也永远不会完成，
// 留在分母里会让进度永远到不了 100%，还会造出「状态已完成 + 进度 76.5%」这种自相矛盾的对象。
func progressPercent(s *boardStats) float64 {
	den := s.Total - s.Canceled
	if den <= 0 {
		return 0
	}
	return round1(float64(s.Done) / float64(den) * 100)
}

// phaseSettled 表示这个阶段已经尘埃落定（做完了或整个被取消），排序时垫底。
func phaseSettled(status string) bool { return status == "done" || status == "canceled" }

// buildPhase 组装一个阶段。
//
// projTasks 是**整个项目**的卡，专门用于单卡排位——阶段只是展示分组，
// 调度器不认阶段，用阶段切片算排位会得出一个与调度现实无关的名次。
func buildPhase(cfg *Config, projID, name string, ts, projTasks []*Task, now time.Time,
	pace *paceModel, kindOf map[string]kindMark) Phase {

	ph := Phase{
		ID:         projID + "/" + name,
		Name:       name,
		DescSource: "derived",
	}
	for _, t := range ts {
		ph.Stats.add(t.Status)
	}
	ph.ProgressPercent = progressPercent(&ph.Stats)
	switch {
	case ph.Stats.Held > 0 || ph.Stats.Failed > 0:
		ph.Status = "blocked"
	case ph.Stats.Running > 0:
		ph.Status = "active"
	case ph.Stats.Queued > 0 || ph.Stats.LimitPaused > 0:
		ph.Status = "queued"
	case ph.Stats.Done == 0 && ph.Stats.Canceled > 0:
		// 全部终态但一张都没完成 = 整个阶段被取消掉了，不等于做完了。
		// 原来的 default 分支是以「没有待办」反推「已完成」，从不看 Done/Canceled，
		// 于是只含取消卡的阶段会报出「状态已完成 + 进度 0%」。
		ph.Status = "canceled"
	default:
		ph.Status = "done"
	}
	ph.ETA = pace.estimate(ph.Stats.schedulable(), ph.Stats.Held, "阶段")

	// 未终态卡优先展示，其次按更新时间倒序
	sort.Slice(ts, func(i, j int) bool {
		a, b := ts[i], ts[j]
		if a.terminal() != b.terminal() {
			return !a.terminal()
		}
		return a.UpdatedAt > b.UpdatedAt
	})
	for _, t := range ts {
		br := toBrief(cfg, t, now)
		br.ETA = pace.estimateTask(t, projTasks, now)
		br.applyKind(kindOf[t.ID])
		ph.Tasks = append(ph.Tasks, br)
	}
	ph.Desc = derivedPhaseDesc(&ph)
	return ph
}
