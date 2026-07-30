package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	statusQueued      = "queued"
	statusRunning     = "running"
	statusLimitPaused = "limit_paused"
	statusHeld        = "held"
	statusDone        = "done"
	statusFailed      = "failed"
	statusCanceled    = "canceled"
)

type Task struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Type     string   `json:"type"`
	Priority int      `json:"priority"`
	Status   string   `json:"status"`
	Dir      string   `json:"dir"`
	Prompts  []string `json:"prompts"`
	Step     int      `json:"step"`

	SessionID string `json:"session_id,omitempty"`
	// Model 非空时以 --model 传给 claude（如 haiku/sonnet/opus），用于按任务难度路由模型。
	Model string `json:"model,omitempty"`
	// Runner 记录实际执行器："codex" 表示由备用 codex CLI 执行（claude 冷却期切换）。
	Runner string `json:"runner,omitempty"`
	// PreferRunner 为 "codex" 时任务钉在 codex 上执行（不管 claude 忙闲），
	// 用独立的 GPT 额度跑填充类任务；要求任务满足 codexEligible（fresh 或单步无会话）。
	PreferRunner string `json:"runner_pref,omitempty"`
	// CodexModel 钉定本卡经 codex 执行时的具体模型（-codex-model，如 gpt-5.6-terra）。
	// 两条路径生效：①runner_pref=codex 主跑；②claude 卡触发 codex_fallback 降级改道。
	// 空 = 按径回落（降级径先看 config.codex_fallback_model，再全局 codex_model；主跑径直接全局）。
	// 交叉链卡的 XCodexModel（入队冻结的引擎身份）恒优先于本字段。
	CodexModel string `json:"codex_model,omitempty"`
	// RemoteHost 非空时任务在该远程主机执行（SSH → 远端 codex），键入 Config.RemoteHosts。
	// 让远端机器进编排（跨机 dev）；要求 remoteEligible（单步/fresh、无 claude 会话）。
	RemoteHost string `json:"remote_host,omitempty"`
	// ReviewHost 非空时，本卡完成后自动派的对抗审核卡分流到该远程主机执行（键入 Config.RemoteHosts）。
	// 用于把只读审核负载分流到第二台机器、平衡两侧模型额度；实现卡自身仍在原处执行。经修复链继承。
	ReviewHost string `json:"review_host,omitempty"`
	// ReviewDir 是审核卡在审核主机上的工作目录（镜像路径），与 ReviewHost 成对使用；
	// 渲染审核模板 {{DIR}} 时替换实现卡的 Dir。经修复链继承。
	ReviewDir string `json:"review_dir,omitempty"`
	// ReviewSync 非空时，派审核卡前先在本地以 sh -c 执行该命令（如把改动 rsync 到审核主机）。
	// 失败则回退本地审核——闭环绝不因分流失败而断。可单独存在（仅同步不分流）。经修复链继承。
	ReviewSync string `json:"review_sync,omitempty"`
	// MidStep 表示当前步骤执行到一半被限额打断：恢复时发送续跑提示而不是重发原 prompt。
	MidStep bool `json:"mid_step,omitempty"`

	EmitTasks   bool `json:"emit_tasks,omitempty"`
	ReviewAfter bool `json:"review_after,omitempty"`
	// Effort 非空时以 --effort 传给 claude（low/medium/high/xhigh/max），按任务难度调思考等级。
	Effort string `json:"effort,omitempty"`
	// Stakes 是本卡的投入产出档位（low|normal|high，缺省 normal）。**只作审计留档**：
	// add 时按 config.stakes_policy 查表，把复核深度固化进 ReviewAfter/Effort，运行期不再据本字段
	// 判定任何行为（入队即钉，防"改配置让在队卡的复核深度静默漂移"）。见 stakes.go 文件头。
	Stakes string `json:"stakes,omitempty"`
	// ReviewOf: 本卡是审核卡时指向被审卡 ID；修复闭环靠它取被审卡的 prompt/参数做继承。
	ReviewOf string `json:"review_of,omitempty"`
	// FixRound: "实现→对抗审核→修复"循环轮次。实现卡 0，第 n 轮自动修复卡为 n；审核卡继承被审卡轮次。
	// 达到 config.max_fix_rounds 后不再自动派修复，改挂 held 升级卡交人工/设计权威裁定。
	FixRound int `json:"fix_round,omitempty"`
	// Closeout: 收口回写指令（opt-in）。非空时，本卡的对抗复审 verdict=pass 后，自动入队一张
	// 廉价（haiku）收口卡跑此 prompt——把"done"回写权威账本的动作绑定到 pass 事件而非实现卡自评，
	// 根治"实现卡提前自标 done / 老实等审却 pass 后没人翻"的双真相源漂移。经修复链继承。
	Closeout string `json:"closeout,omitempty"`
	// FreshSteps 表示步骤间不 --resume：每一步都是全新会话（配合"状态在文件里"的项目规约，
	// prompt 自带读状态文件的开工动作）。永不依赖会话记忆，也就不会撞会话上下文上限。
	FreshSteps bool `json:"fresh_steps,omitempty"`
	// ---- 交叉验证链（fable 顶替流：双引擎独立作答→引擎乙对抗式交叉查漏）----
	// XRole 交叉验证链中的角色："A"(引擎甲,先独立作答) / "B"(引擎乙,独立作答) / "C"(引擎乙交叉查漏)。空=非交叉链。
	XRole string `json:"x_role,omitempty"`
	// XKey 交叉验证链的谱系键（A/B/C 共享，不透明随机键、非 A 卡 ID），用于关联与把 C 的最终结论落进度报告。
	XKey string `json:"x_key,omitempty"`
	// XProfile 使用的引擎对 profile 名（config.cross_profiles 的键），派生 B/C 时据此选引擎乙。
	XProfile string `json:"x_profile,omitempty"`
	// XTask 原始任务内容（未包裹方法纪律的裸文本），随链 A→B→C 传递，供 C 的合并模板重述任务对象。
	XTask string `json:"x_task,omitempty"`
	// XEngineB 是**冻结**的乙引擎执行规格（cmdCross 入队时解析并钉死，随链 A→B→C 传递）。
	// B/C 由它套用，绝不再从当前 config.cross_profiles 重解析——否则入队后改 profile 会静默换乙引擎/
	// 令甲乙相同（身份漂移）。
	XEngineB *XFrozenEngine `json:"x_engine_b,omitempty"`
	// XCodexModel 是本卡冻结的 codex 模型（codex/远端 codex 引擎）。invokeCodex/invokeRemoteCodex 优先用它，
	// 空才回落全局 codex_model——否则入队后改/清 codex_model 会静默换模型或掉 -m 跑默认模型。
	XCodexModel string `json:"x_codex_model,omitempty"`
	// 注：甲的结论**不**放在任何交叉卡的字段里，也不写进 A 的日志（RESULT 被抹）——最小化 B 的执行器
	// 从盘上被动读到 A 的表面。A 完成后其结论落进 <root>/crosscheck/<XKey>.a 隔离侧车，仅由编排进程在派 C 时
	// 读取注入 C 的 prompt、用完即删。B 卡不含 A 卡 ID、不含 A 结论。**诚实边界**：B 持有 XKey，而侧车路径
	// 由 XKey 确定性推导——严格说 XKey 就是指向侧车的键。故这不是硬沙箱，是被动暴露最小化 + 行为护栏（见上）。
	// 这是被动暴露最小化 + 行为护栏（solo 模板明令别找），**非硬沙箱**：codex read-only 能读全盘，刻意搜索
	// 仍可能触达侧车——强隔离需限制执行器读权限，本工具不提供（见 README 诚实声明）。
	// EmitHold 表示本任务产出的任务先挂起（held），人工审核后 release 放行。
	EmitHold bool `json:"emit_hold,omitempty"`
	// EmitProgress 表示任务完成后把最终输出中的 json 块存为进度报告（progress/ 目录）。
	EmitProgress bool `json:"emit_progress,omitempty"`
	// ProgressKey 是进度报告的落盘键；EmitProgress 时使用，空则用任务 ID。
	ProgressKey string `json:"progress_key,omitempty"`

	ResumeAtEpoch  int64 `json:"resume_at_epoch,omitempty"`
	NotBeforeEpoch int64 `json:"not_before_epoch,omitempty"`
	Attempts       int   `json:"attempts,omitempty"`

	PermissionMode  string   `json:"permission_mode,omitempty"`
	AllowedTools    []string `json:"allowed_tools,omitempty"`
	SkipPermissions bool     `json:"skip_permissions,omitempty"`

	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
	LastError string  `json:"last_error,omitempty"`
	TurnsUsed int     `json:"turns_used,omitempty"`
	CostUSD   float64 `json:"cost_usd,omitempty"`
	// LastSummary 是最近一步执行输出的一行摘要，供 list 看板展示“最新进度概述”。
	LastSummary string `json:"last_summary,omitempty"`
}

func (t *Task) touch() { t.UpdatedAt = time.Now().Format(time.RFC3339) }

func (t *Task) terminal() bool {
	return t.Status == statusDone || t.Status == statusFailed || t.Status == statusCanceled
}

// newCrossKey 生成交叉验证链的不透明谱系键：随机、与 A 卡 ID 无关——故 B 无法据它推出 A 的
// 日志/侧车路径去偷读甲结论（独立性的一环）。
func newCrossKey() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "x" + hex.EncodeToString(b)
}

func newID(root string) string {
	for {
		b := make([]byte, 2)
		_, _ = rand.Read(b)
		id := fmt.Sprintf("t%s-%s", time.Now().Format("0102-1504"), hex.EncodeToString(b))
		if _, err := os.Stat(filepath.Join(tasksDir(root), id+".json")); os.IsNotExist(err) {
			return id
		}
	}
}

// newTask 创建任务并把类型默认参数烘焙进去。
func newTask(root string, cfg *Config, typ, title, dir string, prompts []string, priority int) *Task {
	now := time.Now().Format(time.RFC3339)
	t := &Task{
		ID:        newID(root),
		Title:     title,
		Type:      typ,
		Priority:  priority,
		Status:    statusQueued,
		Dir:       dir,
		Prompts:   prompts,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if td, ok := typeDefaultsFor(cfg, typ); ok {
		t.PermissionMode = td.PermissionMode
		t.AllowedTools = append([]string(nil), td.AllowedTools...)
		t.SkipPermissions = td.SkipPermissions
		t.Model = td.Model
		t.Effort = td.Effort
	}
	return t
}

// findTaskAnywhere 先在 tasks/ 再在 archive/ 按精确 ID 找任务
// （修复闭环取谱系时，被审卡可能已被 clean 归档）。
func findTaskAnywhere(root, id string) (*Task, error) {
	for _, dir := range []string{tasksDir(root), archiveDir(root)} {
		p := filepath.Join(dir, id+".json")
		if data, err := os.ReadFile(p); err == nil {
			var t Task
			if err := json.Unmarshal(data, &t); err != nil {
				return nil, err
			}
			return &t, nil
		}
	}
	return nil, fmt.Errorf("任务 %s 不存在（tasks/ 与 archive/ 均无）", id)
}

func taskPath(root, id string) string { return filepath.Join(tasksDir(root), id+".json") }

func saveTask(root string, t *Task) error {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(taskPath(root, t.ID), append(data, '\n'))
}

func loadTask(root, id string) (*Task, error) {
	data, err := os.ReadFile(taskPath(root, id))
	if err != nil {
		return nil, err
	}
	var t Task
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("解析任务 %s 失败: %w", id, err)
	}
	return &t, nil
}

func loadTasks(root string) ([]*Task, error) {
	entries, err := os.ReadDir(tasksDir(root))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("找不到 %s，请先运行: cardex init", tasksDir(root))
		}
		return nil, err
	}
	var tasks []*Task
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		t, err := loadTask(root, strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			// ReadDir 与逐个读文件之间任务可能被并发归档移走（如 cancel 收尾），不是损坏，静默跳过。
			if !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "警告: 跳过损坏的任务文件 %s: %v\n", e.Name(), err)
			}
			continue
		}
		tasks = append(tasks, t)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt < tasks[j].CreatedAt })
	return tasks, nil
}

// findTask 支持 ID 前缀匹配。
func findTask(root, prefix string) (*Task, error) {
	tasks, err := loadTasks(root)
	if err != nil {
		return nil, err
	}
	var matches []*Task
	for _, t := range tasks {
		if t.ID == prefix {
			return t, nil
		}
		if strings.HasPrefix(t.ID, prefix) {
			matches = append(matches, t)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("找不到任务: %s", prefix)
	case 1:
		return matches[0], nil
	default:
		var ids []string
		for _, m := range matches {
			ids = append(ids, m.ID)
		}
		return nil, fmt.Errorf("前缀 %q 匹配到多个任务: %s", prefix, strings.Join(ids, ", "))
	}
}

// diskCanceled 判断任务在盘上是否已被取消。cancel 命令与调度进程各跑各的，
// 只能靠任务文件表态：状态为 canceled，或文件已不在 tasks/（非运行态 cancel
// 直接归档移走、或被人工删除）都算取消。读不出的损坏文件不算（别误杀）。
func diskCanceled(root, id string) bool {
	t, err := loadTask(root, id)
	if err != nil {
		return os.IsNotExist(err)
	}
	return t.Status == statusCanceled
}

func archiveTask(root string, t *Task) error {
	if err := os.MkdirAll(archiveDir(root), 0o755); err != nil {
		return err
	}
	if err := os.Rename(taskPath(root, t.ID), filepath.Join(archiveDir(root), t.ID+".json")); err != nil {
		return err
	}
	// 事件账本随卡归档：activity 层遇归档卡时仍能取到完整历史,而不是"卡在但事件消失"的断线。
	// 归档失败只打警告不回退——任务本体已归档,事件迁移属留痕,回退更容易造成"卡半档"的数据不一致。
	if err := archiveTaskEvents(root, t.ID); err != nil {
		fmt.Fprintf(os.Stderr, "警告: 事件账本归档失败 %s: %v\n", t.ID, err)
	}
	// CG-4 墓碑随卡归档:审计要看"这张卡的注入尝试了几次、有没有耗尽 bound",归档后墓碑也得跟着走,
	// 否则 clean 之后墓碑残留在 tombstones/,新卡碰到同 ID(极小概率)会读到旧墓碑误跳注入。
	// 同事件账本策略:失败只警告不回退,避免"卡半档"。
	if err := archiveTaskTombstones(root, t.ID); err != nil {
		fmt.Fprintf(os.Stderr, "警告: 墓碑账本归档失败 %s: %v\n", t.ID, err)
	}
	return nil
}
