package main

// board.go — Web 看板的 HTTP 层。
//
// 三条纪律：
//  1. **队列数据只读**。所有 handler 读 ~/.claudego 只经 os.ReadFile / os.ReadDir，
//     tasks/ / archive/ / events/ / 任务 JSON 一个字节都不写、任何任务状态都不改。
//     看板挂在生产队列数据上，误写会污染真实队列。
//     唯一的例外是**看板自己的视图状态**：POST /api/project/archive 写
//     <root>/board_archive.json（项目折叠状态，见 boardarchive.go）。它不参与调度、
//     不被 runner/tick/patrol 读取、删掉也不丢任何队列数据；GET 路径仍然零写入。
//  2. **只听 127.0.0.1**。数据里含 prompt 全文、目录路径、账号额度，不该出本机。
//     -addr 可显式覆盖，但默认永远是回环地址。
//  3. **带缓存**。tasks/ + archive/ 近 2000 个 JSON、transcript 数十 MB，
//     每次请求全盘扫会把磁盘打满；快照与燃尽各有 TTL 缓存。

import (
	"compress/gzip"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// 前端静态资源内嵌进二进制（照 templates.go 的先例），部署时只有一个文件。
//
//go:embed web/*
var boardWeb embed.FS

// ---- 响应结构（字段名严格对齐契约）----

type OverviewResp struct {
	GeneratedAt string       `json:"generated_at"`
	Root        string       `json:"root"`
	Totals      boardTotals  `json:"totals"`
	MaxParallel int          `json:"max_parallel"`
	Projects    []*Project   `json:"projects"`
	Quota       QuotaSummary `json:"quota"`
	// BoardOverrideError 是 board.json 加载/解析失败的诊断串（有则挂前端告警）。
	// omitempty：正常场景下响应里不出现该键；一旦出现，前端必须显式披露——
	// 静默吞掉解析错误会让"整个 override 静默蒸发"与"没配 override"看起来完全一样，
	// 违反 fail-honest 纪律（教训：一个逗号打错就把项目名 / desc / phases / goal 全丢）。
	BoardOverrideError string `json:"board_override_error,omitempty"`
	// 【R3·P1-2】BoardOverrideErrorKind ∈ {"type", "syntax"}(未出错时为空,omitempty 消失)。
	// 前端 app.js 按此把顶端横幅文案分开:
	//   type   → "部分覆盖仍生效,出错字段已跳过"（Go encoding/json 语义:skip 出错字段继续填充其它）
	//   syntax → "覆盖全部失效,页面显示回落自动推导"（整块无法保留）
	// 上一轮 review-divert P1-2 根因就是横幅无条件写 syntax 文案——name/desc 明明还生效,
	// 披露自身失实即 fail-honest 卡自身破线。契约字段名不得擅改(app.js grep 有依赖)。
	BoardOverrideErrorKind string `json:"board_override_error_kind,omitempty"`
	// ArchiveStateError 是 board_archive.json 读失败的诊断串（有则挂前端告警）。
	// 与 override 同理：读不出归档状态时**必须**说出来——静默当成"没有任何项目被归档"
	// 会让用户手动折叠的十个项目一次性全部冒出来，且界面上零提示。
	ArchiveStateError string `json:"archive_state_error,omitempty"`
	// ArchivedCount 是被折叠掉的项目数。前端默认不渲染归档项目，
	// 不发这个数的话「总览少了三个项目」与「这三个项目没卡了」在界面上无法区分。
	ArchivedCount int `json:"archived_count"`
}

// BoardColumn 是 kanban 的一列。
// Total 与 Truncated 是**必需**的披露字段：done 列会被 doneColumnCap 截断，
// 只发 tasks 的话前端无从知道自己只拿到了一部分，会把「60 张」当成全部
// （实测某项目 622 张 done 只发了 60 张）。
type BoardColumn struct {
	Key       string       `json:"key"`
	Label     string       `json:"label"`
	Total     int          `json:"total"`
	Truncated bool         `json:"truncated"`
	Tasks     []TaskDetail `json:"tasks"`
}

type PhaseLane struct {
	Phase   Phase         `json:"phase"`
	Columns []BoardColumn `json:"columns"`
}

type ActivityItem struct {
	At     string `json:"at"`
	TaskID string `json:"task_id"`
	Title  string `json:"title"`
	Event  string `json:"event"`
}

type ProjectResp struct {
	Project           *Project       `json:"project"`
	Columns           []BoardColumn  `json:"columns"`
	PhaseLanes        []PhaseLane    `json:"phase_lanes"`
	RecentActivity    []ActivityItem `json:"recent_activity"`
	ArchiveStateError string         `json:"archive_state_error,omitempty"`
}

// ---- 看板服务 ----

type boardServer struct {
	root  string
	snap  *boardCache
	burn  *burnCache
	clock func() time.Time
	// arch 给 board_archive.json 的 read-modify-write 上锁。多标签页同时点归档时，
	// 无锁会丢更新（两个页面各归档一个项目，后写的把先写的抹掉）。
	arch boardArchiveStore
}

// 列顺序按契约固定：running, queued, limit_paused, held, failed, done。
var boardColumnOrder = []struct{ key, label string }{
	{statusRunning, "进行中"},
	{statusQueued, "排队中"},
	{statusLimitPaused, "限额暂停"},
	{statusHeld, "已挂起"},
	{statusFailed, "失败"},
	{statusDone, "已完成"},
}

// doneColumnCap 限制「已完成」列的条数。单个项目有近 700 张 done 卡，
// 每张还带 600 字 prompt 摘录——全吐出去响应会到几 MB，看板没有任何价值增益。
// 未终态列不设上限（本来就少，且是用户真正要盯的）。
const doneColumnCap = 60

func newBoardServer(root string, ttl time.Duration) *boardServer {
	return &boardServer{
		root: root,
		snap: &boardCache{ttl: ttl},
		// transcript 扫描最贵，燃尽视图 TTL 取任务快照的 3 倍（至少 30 秒）。
		burn:  &burnCache{ttl: maxDuration(3*ttl, 30*time.Second)},
		clock: time.Now,
	}
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 看板数据每次都可能变，禁掉缓存免得浏览器拿旧的额度数字骗人。
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		// 响应已开始写，只能记日志（这里静默：看板不该因为一个断开的连接刷屏）
		_ = err
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}

// gzipWriter 把响应体接到 gzip 上。WriteHeader 必须删掉 Content-Length——
// 压缩后长度必然与上游算的不同，留着会让浏览器截断响应。
type gzipWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
}

func (w *gzipWriter) Write(b []byte) (int, error) { return w.gz.Write(b) }

func (w *gzipWriter) WriteHeader(code int) {
	w.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeader(code)
}

// withGzip 按 Accept-Encoding 协商压缩。
// 看板的 JSON 压缩比实测 8 倍（/api/project 2.5MB → 320KB），而前端每 30 秒轮询一次；
// 不压等于把带宽和 JSON 解析开销都白付一遍。用标准库 compress/gzip，不引第三方依赖。
//
// 只包 /api/* 与首页：/static/ 走 http.FileServer，它自己管 Content-Length、
// Range 与 304，套压缩要多处理一堆边角，而那几个静态文件本来就不大。
func withGzip(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			h(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		h(&gzipWriter{ResponseWriter: w, gz: gz}, r)
	}
}

func (s *boardServer) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/overview", withGzip(s.handleOverview))
	mux.HandleFunc("/api/project", withGzip(s.handleProject))
	mux.HandleFunc("/api/project/archive", s.handleArchive)
	mux.HandleFunc("/api/burn", withGzip(s.handleBurn))
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/static/", s.handleStatic)
	mux.HandleFunc("/", withGzip(s.handleIndex))
	return mux
}

func (s *boardServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version})
}

func (s *boardServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := boardWeb.ReadFile("web/index.html")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内嵌前端缺失: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (s *boardServer) handleStatic(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(boardWeb, "web")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "内嵌前端缺失: "+err.Error())
		return
	}
	// 前端资源随二进制内嵌、无版本指纹（embed.FS 的 ModTime 为零，FileServer 也不发
	// ETag/Last-Modified）。若任由浏览器启发式缓存，make install 换了新二进制后老页面仍
	// 跑旧 app.js/style.css，看板显示与实际不符。故与 index 一致标 no-store，每次都取最新。
	// 资源仅 ~80KB 且只走本机回环，重取成本可忽略。
	w.Header().Set("Cache-Control", "no-store")
	http.StripPrefix("/static/", http.FileServer(http.FS(sub))).ServeHTTP(w, r)
}

// applyArchiveState 把归档状态叠加到项目副本上，返回被折叠的项目数。
//
// 为什么在 handler 里叠而不是在 buildSnapshot 里：快照带 10 秒 TTL 缓存，
// 归档状态写完后如果要等缓存过期才生效，用户点完按钮会看到"没反应"然后突然跳变。
// 归档文件只有几百字节，每次请求重读的代价可以忽略；换来的是"点了立刻生效"。
//
// arc 为 nil（读失败）时**全部按未归档处理**，同时由调用方把错误挂进响应——
// 静默按"未归档"渲染且不报错才是问题，报了错的降级是诚实的。
func applyArchiveState(projects []*Project, tasksOf map[string][]*Task, arc *boardArchiveFile) int {
	if arc == nil {
		return 0
	}
	n := 0
	for _, p := range projects {
		rec, ok := arc.Projects[p.ID]
		if !ok {
			continue
		}
		count, maxCreated := projectCardMark(tasksOf[p.ID])
		v := archiveViewFor(&rec, count, maxCreated)
		p.Archived = v.Archived
		p.ArchivedAt = v.ArchivedAt
		p.ArchiveRevived = v.Revived
		p.ArchiveRevivedReason = v.Reason
		if v.Archived {
			n++
		}
	}
	return n
}

func (s *boardServer) handleOverview(w http.ResponseWriter, r *http.Request) {
	now := s.clock()
	snap, err := s.snap.get(s.root, now)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	projects := make([]*Project, 0, len(snap.Projects))
	for _, p := range snap.Projects {
		projects = append(projects, projectForOverview(p))
	}
	arc, arcErr := loadBoardArchive(s.root)
	archivedN := applyArchiveState(projects, snap.projTasks, arc)
	burn := s.burn.get(s.root, snap.Cfg, now)
	resp := OverviewResp{
		GeneratedAt:            now.Format(time.RFC3339),
		Root:                   snap.Root,
		Totals:                 snap.Totals,
		MaxParallel:            snap.Cfg.MaxParallel,
		Projects:               projects,
		Quota:                  quotaSummary(burn.Sources),
		BoardOverrideError:     snap.BoardOverrideError,
		BoardOverrideErrorKind: snap.BoardOverrideErrorKind,
		ArchivedCount:          archivedN,
	}
	if arcErr != nil {
		resp.ArchiveStateError = "读 board_archive.json 失败（本次全部按未归档显示）: " + arcErr.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// archiveReq 是 POST /api/project/archive 的请求体。
type archiveReq struct {
	ID       string `json:"id"`
	Archived bool   `json:"archived"`
}

// handleArchive 是看板唯一的写入端点：归档 / 取消归档一个项目。
//
// 【三道闸】这是本进程唯一会落盘的 HTTP 路径，而看板可能被 -addr 放到局域网上，
// 所以就算写的只是视图状态也要挡住"别人替你点按钮"：
//  1. 只收 POST——GET 会被 <img src> 之类的东西随手触发；
//  2. Content-Type 必须是 application/json——HTML 表单只能发 urlencoded/multipart/plain，
//     这一条就把"页面里埋个自动提交表单"的跨站写入挡在门外；
//  3. 带 Origin 头时其 host 必须等于本次请求的 Host——浏览器发起的跨站 fetch 必带 Origin，
//     不匹配即拒。同源 fetch 也会带 Origin，故这不是可选校验。
//     命令行 curl 不带 Origin，放行（本机运维要能脚本化）。
func (s *boardServer) handleArchive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeErr(w, http.StatusMethodNotAllowed, "只接受 POST")
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeErr(w, http.StatusUnsupportedMediaType, "Content-Type 必须是 application/json")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && !sameOriginHost(origin, r.Host) {
		writeErr(w, http.StatusForbidden, "跨源写入被拒绝")
		return
	}
	var req archiveReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "缺少参数 id")
		return
	}
	now := s.clock()
	snap, err := s.snap.get(s.root, now)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 归档一个不存在的项目 id 会在状态文件里留下永远清不掉的垃圾记录（前端看不到它，
	// 也就没有按钮能取消它）。存在性校验挡在写入之前。
	found := false
	for _, p := range snap.Projects {
		if p.ID == id {
			found = true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, "找不到项目: "+id)
		return
	}
	count, maxCreated := projectCardMark(snap.projTasks[id])
	rec, err := s.arch.set(s.root, id, req.Archived, count, maxCreated, now)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "写归档状态失败: "+err.Error())
		return
	}
	out := map[string]any{"ok": true, "id": id, "archived": rec != nil}
	if rec != nil {
		out["archived_at"] = rec.ArchivedAt
	}
	writeJSON(w, http.StatusOK, out)
}

// sameOriginHost 比对 Origin 头的 host:port 与请求 Host。
// 只比 host 部分，不比 scheme——看板同时可能经 http 直连与反代访问，
// 比 scheme 会把正常的反代场景误杀，而 CSRF 防护要的正是 host 这一维。
func sameOriginHost(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Host == host
}

func (s *boardServer) handleBurn(w http.ResponseWriter, r *http.Request) {
	now := s.clock()
	// 燃尽视图的百分比/token 部分只需要 config；任务消耗那一段要整份快照。
	// 快照顺带把 config 读好了，复用它；快照读不出来时退回单读 config——
	// 额度曲线不该因为队列目录有问题就整页挂掉。
	var cfg *Config
	var snap *boardSnapshot
	if sn, err := s.snap.get(s.root, now); err == nil {
		snap, cfg = sn, sn.Cfg
	} else if c, cerr := loadConfig(s.root); cerr == nil {
		cfg = c
	} else {
		writeErr(w, http.StatusInternalServerError, cerr.Error())
		return
	}
	resp := *s.burn.get(s.root, cfg, now)
	// 任务消耗按请求的窗口现算：它只是把快照里已在内存的卡再过一遍，不碰磁盘，
	// 所以不进 burnCache——否则每个窗口都要占一份 transcript 扫描的缓存位。
	if snap != nil {
		resp.TaskSpend = buildTaskSpend(cfg, snap, r.URL.Query().Get("range"), now)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *boardServer) handleProject(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "缺少参数 id")
		return
	}
	now := s.clock()
	snap, err := s.snap.get(s.root, now)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var proj *Project
	for _, p := range snap.Projects {
		if p.ID == id {
			proj = p
			break
		}
	}
	if proj == nil {
		writeErr(w, http.StatusNotFound, "找不到项目: "+id)
		return
	}
	tasks := snap.projTasks[id]
	// 一个项目一份 paceModel，两处渲染共用。以前 buildColumns 在内部自建，
	// 而它被「整个项目」和「单个阶段」两种口径的切片各调用一次，
	// 于是同一张卡在同一个响应里拿到两套 p50/p80/finish_at。
	pace := newPaceModel(snap.Cfg, tasks, now)

	// 归档状态叠在**副本**上：snap.Projects 里的那份是缓存的共享对象，
	// 就地改写会让并发的另一个请求读到半改状态。
	pc := *proj
	arc, arcErr := loadBoardArchive(s.root)
	applyArchiveState([]*Project{&pc}, snap.projTasks, arc)
	resp := ProjectResp{
		Project:        &pc,
		Columns:        s.buildColumns(snap.Cfg, snap.kindOf, pace, tasks, tasks, now, false),
		PhaseLanes:     s.buildLanes(snap, proj, pace, tasks, now),
		RecentActivity: buildActivity(s.root, tasks),
	}
	if arcErr != nil {
		resp.ArchiveStateError = "读 board_archive.json 失败（本次按未归档显示）: " + arcErr.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// buildColumns 把一批卡摊成 kanban 列。
//
// pace 与 siblings 都由调用方传入且必须是**项目级**的：排位要按整个项目的
// 可调度队列算，否则泳道里的卡会得到一个与调度现实无关的名次。
// lite=true 时省掉 prompt 摘录与工具清单——泳道与总列展示的是同一批卡，
// 重字段发两遍是纯粹的浪费（实测单次响应 2.5MB，泳道占七成）。
func (s *boardServer) buildColumns(cfg *Config, kindOf map[string]kindMark, pace *paceModel,
	tasks, siblings []*Task, now time.Time, lite bool) []BoardColumn {

	byStatus := map[string][]*Task{}
	for _, t := range tasks {
		byStatus[t.Status] = append(byStatus[t.Status], t)
	}
	cols := make([]BoardColumn, 0, len(boardColumnOrder))
	for _, c := range boardColumnOrder {
		ts := byStatus[c.key]
		sort.Slice(ts, func(i, j int) bool { return ts[i].UpdatedAt > ts[j].UpdatedAt })
		col := BoardColumn{Key: c.key, Label: c.label, Total: len(ts), Tasks: []TaskDetail{}}
		if c.key == statusDone && len(ts) > doneColumnCap {
			ts, col.Truncated = ts[:doneColumnCap], true
		}
		for _, t := range ts {
			col.Tasks = append(col.Tasks, toDetail(cfg, kindOf[t.ID], t, pace, siblings, now, lite))
		}
		cols = append(cols, col)
	}
	return cols
}

// buildLanes 按阶段分泳道，每条泳道内再分 kanban 列。
// 泳道只负责分组展示，不重算节奏也不重算排位——pace 与 siblings 一路透传项目级的那份。
func (s *boardServer) buildLanes(snap *boardSnapshot, proj *Project, pace *paceModel,
	tasks []*Task, now time.Time) []PhaseLane {

	byPhase := map[string][]*Task{}
	for _, t := range tasks {
		byPhase[snap.phaseOf[t.ID]] = append(byPhase[snap.phaseOf[t.ID]], t)
	}
	lanes := make([]PhaseLane, 0, len(proj.Phases))
	for _, ph := range proj.Phases {
		lanes = append(lanes, PhaseLane{
			Phase:   ph,
			Columns: s.buildColumns(snap.Cfg, snap.kindOf, pace, byPhase[ph.Name], tasks, now, true),
		})
	}
	return lanes
}

// promptExcerptLen 是详情里 prompt 摘录的字数上限（按 rune，中文不能按字节切）。
const promptExcerptLen = 600

func toDetail(cfg *Config, km kindMark, t *Task, pace *paceModel, siblings []*Task,
	now time.Time, lite bool) TaskDetail {

	br := toBrief(cfg, t, now)
	br.ETA = pace.estimateTask(t, siblings, now)
	br.applyKind(km)
	d := TaskDetail{
		TaskBrief:    br,
		Dir:          t.Dir,
		AllowedTools: t.AllowedTools,
		PromptsCount: len(t.Prompts),
		CostUSD:      t.CostUSD,
		TurnsUsed:    t.TurnsUsed,
	}
	if lite {
		// 泳道里的卡与 columns 里的是同一批，重字段在 columns 那份里拿。
		d.AllowedTools = []string{}
		return d
	}
	if d.AllowedTools == nil {
		d.AllowedTools = []string{}
	}
	if len(t.Prompts) > 0 {
		r := []rune(t.Prompts[0])
		if len(r) > promptExcerptLen {
			d.PromptExcerpt = string(r[:promptExcerptLen]) + "…"
		} else {
			d.PromptExcerpt = t.Prompts[0]
		}
	}
	return d
}

const activityLimit = 40

// buildActivity 读事件账本（events.jsonl）拼装活动流——诚实历史锚点。
//
// 【为什么必须换】早期实现只用 task.Status + UpdatedAt 反推一句话事件，同一张 UpdatedAt 覆盖前
// 只能显示"最后一次状态"。真实历史"queued→running→limit_paused→running→done"被压平成一句
// "已完成"，与看板"诚实性第一"原则直接冲突。改读事件流后每条状态迁移都还原为独立事件。
//
// 【为什么显式披露事件缺口】事件账本用 seq 单调递增；崩溃残尾/手工删除会造成 seq 跳号。
// 见跳号必须插入 event_gap 项让前端显示"事件缺口"——绝不能拿相邻事件补齐冒充完整历史，
// 那是把"没记录到"伪装成"记录了什么"，还不如没有历史。
func buildActivity(root string, tasks []*Task) []ActivityItem {
	// 事件是 per-task 的，活动流是跨 task 时序合并——遍历所有 task 加载其事件账本，
	// 缺账本（旧卡尚未生成事件）直接跳过：绝不为无事件的卡合成一条状态反推事件，
	// 否则"事件流是诚实历史"的招牌就被 fallback 反推打脸。
	type row struct {
		item ActivityItem
		key  string // sort key: at + "|" + taskID + "|" + seq (稳定次序)
	}
	rows := make([]row, 0, len(tasks)*4)
	for _, t := range tasks {
		events, hadCorruption, err := loadTaskEvents(root, t.ID)
		if err != nil || len(events) == 0 {
			continue
		}
		// prevSeq 从 0 起算而非 events[0].Seq-1:头部若被删(events[0].Seq>1)必须能触发缺口披露。
		// 早期 i==0 特判把 prevSeq 硬拉到 ev.Seq-1,等于宣告"起点就是这里,前面没有历史"——手工删掉
		// seq=1 后剩下 [2,3,...] 会被当完整历史,反例注入"删中间"能报红、"删头部"却逃检,守卫留后门。
		var prevSeq int64
		for _, ev := range events {
			// seq 跳号即缺口,插一条 event_gap 显式披露"这里有事件读不出"(崩溃残尾/被删/头部截断)。
			if ev.Seq > prevSeq+1 {
				rows = append(rows, row{
					item: ActivityItem{
						At: ev.TS, TaskID: t.ID, Title: t.Title,
						Event: fmt.Sprintf("事件缺口（缺失 seq %d..%d）", prevSeq+1, ev.Seq-1),
					},
					key: ev.TS + "|" + t.ID + "|gap",
				})
			}
			rows = append(rows, row{
				item: ActivityItem{
					At: ev.TS, TaskID: t.ID, Title: t.Title, Event: describeEvent(ev),
				},
				key: fmt.Sprintf("%s|%s|%010d", ev.TS, t.ID, ev.Seq),
			})
			prevSeq = ev.Seq
		}
		if hadCorruption {
			// 尾部残尾/损坏行：整体披露一次（避免每条坏行都刷一条噪声）。
			// 用当前 task.UpdatedAt 而非事件时间——损坏事件本身没有可信 ts。
			rows = append(rows, row{
				item: ActivityItem{
					At: t.UpdatedAt, TaskID: t.ID, Title: t.Title,
					Event: "事件缺口（尾部残行或损坏行已丢弃）",
				},
				key: t.UpdatedAt + "|" + t.ID + "|corrupt",
			})
		}
	}
	// 倒序排：最新在前。key 里含 seq 保证同一 ts 内按 seq 稳定排序。
	sort.Slice(rows, func(i, j int) bool { return rows[i].key > rows[j].key })
	if len(rows) > activityLimit {
		rows = rows[:activityLimit]
	}
	out := make([]ActivityItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.item)
	}
	return out
}

// describeEvent 把事件类型翻译成人读文本。event_gap 在 buildActivity 里独立生成不走这里。
//
// 【步号语义纪律 · 一定要读】runner 侧 emit 事件时的 ev.Step 语义因迁移点不同而不同:
//   - evDispatched: 派上时 Step=待执行步序(0-indexed),显示 +1 得到"第 1 步";
//   - evStepOK/evDone/evFailed: 已在 runner.go:779 做过 t.Step++,Step 是"已完成步数"(1-indexed),
//     直接显示即"第 N 步";若再 +1 会显示"第 N+1 步·末步"这类不存在的步(实测两步卡末步曾报"第 3 步")。
// 其他 evLimitPaused/evRetry/evHeld/evCanceled 的 ev.Step 视迁移点各异但描述不显示步号,不受此坑影响。
func describeEvent(ev TaskEvent) string {
	switch ev.Type {
	case evQueued:
		return "入队"
	case evDispatched:
		return fmt.Sprintf("派上执行（第 %d 步）", ev.Step+1)
	case evStepOK:
		if final, _ := ev.Detail["final_step"].(bool); final {
			return fmt.Sprintf("步完成（第 %d 步·末步）", ev.Step)
		}
		return fmt.Sprintf("步完成（第 %d 步）", ev.Step)
	case evLimitPaused:
		return "限额暂停，等待额度重置"
	case evHeld:
		return "已挂起"
	case evRetry:
		return "错误退避重试"
	case evCanceled:
		return "已取消"
	case evDone:
		return "已完成"
	case evFailed:
		return "失败"
	case evCloseout:
		return "派生下游卡（closeout）"
	}
	return ev.Type
}

// ---- 命令入口 ----

func cmdBoard(args []string) error {
	// 变量名避开 fs——本文件导入了 io/fs，同名会把包遮掉。
	fset := flag.NewFlagSet("board", flag.ExitOnError)
	rootFlag := fset.String("root", "", "数据目录")
	port := fset.Int("port", 8787, "监听端口")
	addr := fset.String("addr", "127.0.0.1", "监听地址（默认只听回环；数据含 prompt 全文与额度，不建议外放）")
	ttlSec := fset.Int("ttl", 10, "任务快照缓存秒数")
	_ = fset.Parse(args)

	root := resolveRoot(*rootFlag)
	// 先验一次配置：数据目录不对的话立刻报错，别等用户打开浏览器才看见 500。
	if _, err := loadConfig(root); err != nil {
		return err
	}
	srv := newBoardServer(root, time.Duration(*ttlSec)*time.Second)

	listenAddr := net.JoinHostPort(*addr, fmt.Sprint(*port))
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", listenAddr, err)
	}
	fmt.Printf("看板已启动（只读）: http://%s\n", listenAddr)
	fmt.Printf("数据目录: %s\n", root)
	if *addr != "127.0.0.1" && *addr != "localhost" && *addr != "::1" {
		fmt.Printf("⚠️  正在监听 %s（非回环）：响应含 prompt 全文、目录路径与账号额度，请确认这是你要的。\n", *addr)
		fmt.Printf("    同网段的人还能调 POST /api/project/archive 折叠/展开项目（只改 %s，不动任何任务卡）。\n",
			boardArchivePath(root))
	}
	httpSrv := &http.Server{
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return httpSrv.Serve(ln)
}
