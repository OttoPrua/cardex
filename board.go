package main

// board.go — 只读 Web 看板的 HTTP 层。
//
// 三条纪律：
//  1. **只读**。所有 handler 只经 os.ReadFile / os.ReadDir 读 ~/.claudego，
//     不写任何文件、不改任何任务状态。看板挂在生产队列数据上，误写会污染真实队列。
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
	Project        *Project       `json:"project"`
	Columns        []BoardColumn  `json:"columns"`
	PhaseLanes     []PhaseLane    `json:"phase_lanes"`
	RecentActivity []ActivityItem `json:"recent_activity"`
}

// ---- 看板服务 ----

type boardServer struct {
	root  string
	snap  *boardCache
	burn  *burnCache
	clock func() time.Time
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
	http.StripPrefix("/static/", http.FileServer(http.FS(sub))).ServeHTTP(w, r)
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
	burn := s.burn.get(s.root, snap.Cfg, now)
	writeJSON(w, http.StatusOK, OverviewResp{
		GeneratedAt: now.Format(time.RFC3339),
		Root:        snap.Root,
		Totals:      snap.Totals,
		MaxParallel: snap.Cfg.MaxParallel,
		Projects:    projects,
		Quota:       quotaSummary(burn.Sources),
	})
}

func (s *boardServer) handleBurn(w http.ResponseWriter, r *http.Request) {
	now := s.clock()
	// 燃尽视图只需要 config，不需要整份任务快照；但快照顺带把 config 读好了，复用它。
	var cfg *Config
	if snap, err := s.snap.get(s.root, now); err == nil {
		cfg = snap.Cfg
	} else if c, cerr := loadConfig(s.root); cerr == nil {
		cfg = c
	} else {
		writeErr(w, http.StatusInternalServerError, cerr.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.burn.get(s.root, cfg, now))
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

	writeJSON(w, http.StatusOK, ProjectResp{
		Project:        proj,
		Columns:        s.buildColumns(snap.Cfg, pace, tasks, tasks, now, false),
		PhaseLanes:     s.buildLanes(snap, proj, pace, tasks, now),
		RecentActivity: buildActivity(tasks),
	})
}

// buildColumns 把一批卡摊成 kanban 列。
//
// pace 与 siblings 都由调用方传入且必须是**项目级**的：排位要按整个项目的
// 可调度队列算，否则泳道里的卡会得到一个与调度现实无关的名次。
// lite=true 时省掉 prompt 摘录与工具清单——泳道与总列展示的是同一批卡，
// 重字段发两遍是纯粹的浪费（实测单次响应 2.5MB，泳道占七成）。
func (s *boardServer) buildColumns(cfg *Config, pace *paceModel, tasks, siblings []*Task,
	now time.Time, lite bool) []BoardColumn {

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
			col.Tasks = append(col.Tasks, toDetail(cfg, t, pace, siblings, now, lite))
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
			Columns: s.buildColumns(snap.Cfg, pace, byPhase[ph.Name], tasks, now, true),
		})
	}
	return lanes
}

// promptExcerptLen 是详情里 prompt 摘录的字数上限（按 rune，中文不能按字节切）。
const promptExcerptLen = 600

func toDetail(cfg *Config, t *Task, pace *paceModel, siblings []*Task, now time.Time, lite bool) TaskDetail {
	br := toBrief(cfg, t, now)
	br.ETA = pace.estimateTask(t, siblings, now)
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

// buildActivity 按最近更新时间列出活动流。任务卡没有事件日志，
// 只有 updated_at 这一个时间戳，所以「事件」是由**当前状态**反推的一句话描述——
// 它表示「这张卡最近一次变成了什么状态」，不是完整历史。
func buildActivity(tasks []*Task) []ActivityItem {
	ts := append([]*Task(nil), tasks...)
	sort.Slice(ts, func(i, j int) bool { return ts[i].UpdatedAt > ts[j].UpdatedAt })
	if len(ts) > activityLimit {
		ts = ts[:activityLimit]
	}
	out := make([]ActivityItem, 0, len(ts))
	for _, t := range ts {
		out = append(out, ActivityItem{
			At: t.UpdatedAt, TaskID: t.ID, Title: t.Title, Event: activityEvent(t),
		})
	}
	return out
}

func activityEvent(t *Task) string {
	switch t.Status {
	case statusDone:
		return "已完成"
	case statusRunning:
		return fmt.Sprintf("执行中（第 %d/%d 步）", t.Step+1, len(t.Prompts))
	case statusQueued:
		return "排队中"
	case statusLimitPaused:
		return "限额暂停，等待额度重置"
	case statusHeld:
		return "已挂起，等待人工放行"
	case statusFailed:
		return "失败"
	case statusCanceled:
		return "已取消"
	}
	return t.Status
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
	}
	httpSrv := &http.Server{
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return httpSrv.Serve(ln)
}
