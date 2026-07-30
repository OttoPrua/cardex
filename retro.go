package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---- 自动复盘卡（retro_every_n_done）----
//
// BD-44 承 2026-07-31 委托人指示。
//
// 【要解决什么】队列跑了几十张卡之后，"哪类卡老失败、修复要几轮、钱花在哪个模型上、复审到底
// 拦下过什么"这些账没人算。人不会主动去翻 archive/ 与 events.jsonl，于是同一个模板缺陷、
// 同一类改道事故会反复发生。把"每 N 张 done 算一次账"做成机械动作，不依赖人的自觉。
//
// 【proposal-only（D11 恒规）】复盘卡是**只读分析**：读归档卡/事件账本/进度报告，产出一份结构化
// 报告落 progress/，最多给 3 条建议。它不改 config、不改模板、不改任何卡——建议由人或监控 session
// 消费。让自动化去改自动化自己的参数，是把"错误的建议"直接变成"错误的生产配置"。

const (
	// retroProgressKeyPrefix 是复盘卡的进度键前缀，同时是"这张卡是复盘卡"的判据。
	retroProgressKeyPrefix = "retro-"
	// retroTemplate 是内置复盘模板名（templates/retro.md）。
	retroTemplate = "retro"
)

// retroCounter 是终态计数器（<root>/retro_counter.json）。
//
// TriggeredAt 是**已触发复盘的水位**，幂等性全靠它：入队复盘卡前先把水位推到当前 DoneTotal 并落盘
// （claim-then-enqueue），崩溃/重启后重新走到这里只会看到"距上次触发不足 N 张"而跳过。
// 方向是刻意选的——崩在写水位与入队之间只会**少**一张复盘卡；反过来（先入队后落水位）崩溃会
// **重复**入队，既烧额度又往 progress/ 里塞重复报告，还会让下一次复盘统计到自己的噪声。
// 少一张复盘远比多一张便宜。（与 tombstones.go 的"宁可少注入一次也不重复注入"同源纪律。）
type retroCounter struct {
	DoneTotal     int64  `json:"done_total"`
	TriggeredAt   int64  `json:"triggered_at"`
	LastRetroTask string `json:"last_retro_task,omitempty"`
	UpdatedAt     string `json:"updated_at,omitempty"`
}

func retroCounterPath(root string) string { return filepath.Join(root, "retro_counter.json") }

// retroMu 串行化计数器的读-改-写。
// 【为什么进程内互斥就够】写者只有一个二进制，且只在 runner 的终态收尾处写：
//   - 同进程内 max_parallel>1 时多个 runTask goroutine 会并发到这里 → 本 mutex 挡住；
//   - 跨进程并发由 tick 的单实例锁（state.go:acquireLock）挡住，两个 cardex 实例不会同时 drain。
//
// 所以不另起一套文件锁（多一套锁就多一处死锁/陈旧锁面）。这也是配置注释里"单写点=本二进制"的含义。
var retroMu sync.Mutex

func loadRetroCounter(root string) *retroCounter {
	data, err := os.ReadFile(retroCounterPath(root))
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "警告: 读取复盘计数器失败 %s: %v（按 0 起算）\n", retroCounterPath(root), err)
		}
		return &retroCounter{}
	}
	var c retroCounter
	if err := json.Unmarshal(data, &c); err != nil {
		// 损坏文件不静默当 0 用：从 0 起算意味着下一次复盘要多等 N 张，人得知道为什么。
		fmt.Fprintf(os.Stderr, "警告: 复盘计数器 %s 损坏（%v），按 0 重新起算\n", retroCounterPath(root), err)
		return &retroCounter{}
	}
	// 水位高于总数只可能来自手工编辑/回滚：钳回去，否则差值恒为负，复盘永不再触发（静默失效）。
	if c.TriggeredAt > c.DoneTotal {
		c.TriggeredAt = c.DoneTotal
	}
	return &c
}

func saveRetroCounter(root string, c *retroCounter) error {
	c.UpdatedAt = time.Now().Format(time.RFC3339)
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	return atomicWrite(retroCounterPath(root), append(data, '\n'))
}

// isRetroTask 判断一张卡是不是复盘卡自身。
// 复盘卡完成时**不计数**：否则"复盘产出"会被算进"下一轮复盘要统计的产能"，
// N 的含义从"N 张业务卡"漂成"N-1 张业务卡 + 自己"，而且 retro_every_n_done=1 时会自我循环。
func isRetroTask(t *Task) bool {
	return t != nil && t.Type == typeProgressPull && strings.HasPrefix(t.ProgressKey, retroProgressKeyPrefix)
}

// noteTaskDone 在一张卡落入 done 终态时记一笔；累计满 retro_every_n_done 张即自动入队一张复盘卡。
// 返回新入队的复盘卡 ID（未触发时为空串）。
func noteTaskDone(root string, cfg *Config, t *Task) (string, error) {
	if isRetroTask(t) {
		return "", nil
	}
	retroMu.Lock()
	defer retroMu.Unlock()

	c := loadRetroCounter(root)
	c.DoneTotal++
	n := int64(0)
	if cfg != nil {
		n = int64(cfg.RetroEveryNDone)
	}
	// 关闭（0/负）时仍然计数：开关打开那一刻就有历史基数可用，不必从零重新攒 N 张。
	if n <= 0 || c.DoneTotal-c.TriggeredAt < n {
		return "", saveRetroCounter(root, c)
	}

	// claim-then-enqueue：先把水位推到当前总数并落盘，再入队（见 retroCounter 文档注释）。
	watermark := c.DoneTotal
	c.TriggeredAt = watermark
	if err := saveRetroCounter(root, c); err != nil {
		return "", err
	}
	id, err := queueRetroTask(root, cfg, watermark, n)
	if err != nil {
		return "", err
	}
	c.LastRetroTask = id
	// 留痕失败无害：幂等只依赖 TriggeredAt，那一笔已经落盘了。
	if err := saveRetroCounter(root, c); err != nil {
		fmt.Fprintf(os.Stderr, "警告: 复盘卡 %s 已入队但计数器留痕失败: %v\n", id, err)
	}
	return id, nil
}

// noteTaskDoneLogged 是 runner 侧的调用壳：把结果写进任务日志，错误只警告不影响卡本身的终态。
// 【为什么不让错误上抛】卡已经 done 了，复盘是附加动作；因为算不动账就把一张跑成功的卡判成
// 执行出错，是把次要机制的故障放大成主链故障。
func noteTaskDoneLogged(root string, cfg *Config, t *Task, lg *os.File) {
	id, err := noteTaskDone(root, cfg, t)
	if err != nil {
		fmt.Fprintf(os.Stderr, "警告: 复盘计数器更新失败（%s）: %v\n", t.ID, err)
		logBlock(lg, "RETRO", "复盘计数器更新失败: "+err.Error())
		return
	}
	if id != "" {
		logBlock(lg, "RETRO", fmt.Sprintf("已累计 %d 张 done，自动入队复盘卡 %s（只读统计，proposal-only）",
			cfg.RetroEveryNDone, id))
	}
}

// queueRetroTask 入队一张复盘卡：progress-pull 类型 + haiku（机械统计，不需要贵模型）+
// EmitProgress（结论直接落 progress/<key>.json，供人与监控 session 消费）。
//
// 工作目录钉在数据根：复盘的数据源全在 <root>/archive、<root>/events、<root>/progress 下，
// 钉在业务仓反而让只读工具的相对路径落空。
func queueRetroTask(root string, cfg *Config, watermark, n int64) (string, error) {
	tpl, err := loadTemplate(root, retroTemplate)
	if err != nil {
		return "", err
	}
	key := retroProgressKeyPrefix + strconv.FormatInt(watermark, 10)
	prompt := renderTemplate(tpl, map[string]string{
		"N":            strconv.FormatInt(n, 10),
		"ROOT":         root,
		"ARCHIVE_DIR":  archiveDir(root),
		"TASKS_DIR":    tasksDir(root),
		"PROGRESS_DIR": progressDir(root),
		"PROGRESS_KEY": key,
	})
	title := fmt.Sprintf("复盘: 最近 %d 张 done（累计 %d）", n, watermark)
	t := newTask(root, cfg, typeProgressPull, title, root, []string{prompt}, 0)
	// 显式钉 haiku：不依赖 type_defaults.progress-pull.model 恰好是 haiku（用户可能改过）。
	t.Model = "haiku"
	t.EmitProgress = true
	t.ProgressKey = key
	// 复盘卡无会话可续，且它是纯读盘分析——每步全新会话，永不因会话上下文上限失败。
	t.FreshSteps = true
	if err := saveTask(root, t); err != nil {
		return "", err
	}
	emitTaskEvent(root, t.ID, evQueued, "runner:retro", statusQueued, 0, map[string]any{
		"type": t.Type, "reason": "retro_every_n_done",
		"n": n, "watermark": watermark, "progress_key": key,
	})
	return t.ID, nil
}
