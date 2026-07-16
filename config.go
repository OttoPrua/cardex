package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// TypeDefaults 是某一任务类型的默认执行参数，在 add/emit 时烘焙进任务。
type TypeDefaults struct {
	PermissionMode  string   `json:"permission_mode,omitempty"`
	AllowedTools    []string `json:"allowed_tools,omitempty"`
	SkipPermissions bool     `json:"skip_permissions,omitempty"`
	// Model 该类型默认使用的模型（--model 值，如 haiku/sonnet），空表示账号默认模型。
	Model string `json:"model,omitempty"`
	// Effort 该类型默认思考等级（--effort 值：low/medium/high/xhigh/max），空表示 CLI 默认。
	Effort string `json:"effort,omitempty"`
}

type Config struct {
	ClaudeBin         string                  `json:"claude_bin"`
	PollIntervalSec   int                     `json:"poll_interval_sec"`
	LimitFallbackMin  int                     `json:"limit_fallback_min"`
	CooldownMarginSec int                     `json:"cooldown_margin_sec"`
	StepTimeoutMin    int                     `json:"step_timeout_min"`
	MaxAttempts       int                     `json:"max_attempts_per_step"`
	RetryBackoffMin   int                     `json:"retry_backoff_min"`
	// MaxParallel: 单次 tick 内最多并行跑几个任务（同一工作目录始终串行）。1 为纯串行。
	MaxParallel int  `json:"max_parallel"`
	ResumeFirst bool `json:"resume_first"`
	// DrainRescanSec: drain 等待期间的重扫周期（秒）。每周期重扫队列补派新就绪任务，
	// 并做取消对账（running 任务的文件被标 canceled 即击杀其进程）。0 用默认 15。
	DrainRescanSec int `json:"drain_rescan_sec,omitempty"`
	TypeOrder         []string                `json:"type_order"`
	ResumePrompt      string                  `json:"resume_prompt"`
	TypeDefaults      map[string]TypeDefaults `json:"type_defaults"`

	// ---- 5 小时额度红线（保底额度，给交互/突发任务留余量）----
	// QueueBudgetTokens: 滑动 5 小时窗口内，队列最多消耗的加权 token 数；0 关闭。
	// 只统计 claudego 自己派发的调用（桌面端消耗不可见），本质是"队列预算上限"。
	QueueBudgetTokens int64 `json:"queue_budget_tokens"`
	// RedlinePercent + UsageFeed: 外部全局用量源（CodexBar usage-history.jsonl 格式），
	// 最新 claude 5h 窗口样本 usedPercent 达到红线即停止派发；样本过期则放行（fail-open）。
	RedlinePercent     int    `json:"redline_percent"`
	UsageFeed          string `json:"usage_feed"`
	UsageFeedMaxAgeMin int    `json:"usage_feed_max_age_min"`
	// ModelWeights: 各模型 token 的额度权重（订阅限额按模型加权），键为 --model 值，"default" 兜底。
	ModelWeights map[string]float64 `json:"model_weights"`
	// RedlineWindows: 分时段红线。时段内非零字段覆盖全局阈值，时段外回落全局配置。
	RedlineWindows []RedlineWindow `json:"redline_windows"`
	// RedlineLeadMin: 红线时段的前置缓冲（分钟）。时段开始前这么多分钟就停发 claude 任务，
	// 防止起跑的长任务踩进预留窗口（单步任务无法中途让位）。codex 钉定任务不受影响。
	RedlineLeadMin int `json:"redline_lead_min,omitempty"`

	// ---- Codex 备用执行器（claude 冷却/红线期间不断档）----
	// CodexBin 非空且 CodexFallback 开启时：claude 被冷却或红线拦住的时段，
	// 把"单步、无既有 claude 会话"的任务（协调/审核/装配/单步 add）切给 codex exec 执行；
	// 带会话的多步任务仍等 claude 重置（跨 CLI 无法延续上下文）。
	CodexBin      string `json:"codex_bin"`
	CodexFallback bool   `json:"codex_fallback"`
	CodexModel    string `json:"codex_model,omitempty"`
	// CodexReasoning 透传 -c model_reasoning_effort，空则用 codex 默认。合法档位（实测 codex 0.144.1，
	// 由低到高）：minimal < low < medium < high < xhigh < max < ultra（ultra 是多代理委派特档）。
	// 与 claude --effort 的 low<medium<high<xhigh<max 完全同序、同名——所以 Task.Effort 是二者共用的
	// "思考等级"字段（claude→--effort / codex→model_reasoning_effort）；任务级 Effort 非空时覆盖此全局值。
	CodexReasoning string `json:"codex_reasoning,omitempty"`
	// NoFallbackModels：这些模型的任务在 claude 冷却/红线期不降级到 codex，宁可排队等。
	// 设计类卡（fable）质量优先——降级执行violates分层原则。runner_pref 钉定不受此限。
	NoFallbackModels []string `json:"no_fallback_models,omitempty"`
	// ThinkingTokens >0 时给 claude 调用设置 MAX_THINKING_TOKENS（拉高思考预算，设计类任务受益）。
	ThinkingTokens int `json:"thinking_tokens,omitempty"`
	// MaxFixRounds: "实现→对抗审核→自动修复"闭环的轮次上限，超过后不再自动派修复卡，
	// 改挂 held 升级卡交人工/设计权威裁定（防同一叶卡在实现层无限打转）。0 用默认 3。
	MaxFixRounds int `json:"max_fix_rounds,omitempty"`

	// ---- 远程执行器（SSH → 远端 codex，让远端主机进编排）----
	// SSHBin 默认 "ssh"（测试可指向 mock-ssh）。RemoteHosts 键 = Task.RemoteHost（ssh 别名）。
	// 远端 codex 走自己的 GPT 额度：不记 claude 账本、不写全局冷却、不受 claude 冷却/红线阻塞。
	SSHBin      string                      `json:"ssh_bin,omitempty"`
	RemoteHosts map[string]RemoteHostConfig `json:"remote_hosts,omitempty"`

	// ---- 交叉验证引擎对（设计档模型撞限时以两个不同引擎顶替设计/审核/裁决/追认）----
	// CrossProfiles 键 = profile 名（如 "opus-codex"），值为一对引擎：甲先独立出结论，乙独立出结论后
	// 再拿甲的结论对抗式交叉查漏。引擎来源可切换——换 profile 即换模型对，无需改任何代码。
	CrossProfiles map[string]CrossProfile `json:"cross_profiles,omitempty"`
	// DefaultCrossProfile: `claudego cross` 未指定 -profile 时用的 profile 名。
	DefaultCrossProfile string `json:"default_cross_profile,omitempty"`

	// ---- 默认复审分流（把只读复审负载默认压到第二台机器，均衡两侧额度）----
	// DefaultReviewHost 非空时：本地实现卡（RemoteHost 空）的 review_after 自动复审、未显式声明 ReviewHost 者，
	// 默认分流到该主机（RemoteHosts 的键）。RemoteMirrorRoot 是远端镜像根，自动推导 ReviewDir=<root>/<worktree 名>；
	// DefaultReviewSync 是分流前跑的同步命令（sh -c，cwd=实现卡 Dir），把本地泳道同步到远端镜像。三者齐备才生效。
	DefaultReviewHost string `json:"default_review_host,omitempty"`
	RemoteMirrorRoot  string `json:"remote_mirror_root,omitempty"`
	DefaultReviewSync string `json:"default_review_sync,omitempty"`
}

// CrossEngine 描述交叉验证链中一个引擎的执行位置（模型来源可切换的落点）。
type CrossEngine struct {
	// Kind 执行器种类：
	//   "claude"        本机 claude（用 Model+Effort，走本机 claude 账号额度）
	//   "codex"         本机 codex（钉 runner=codex，用 config.codex_model/codex_reasoning=独立 GPT 额度）
	//   "remote-claude" SSH 远端 claude（用 Host+Model，走该远端账号额度）
	//   "remote-codex"  SSH 远端 codex（用 Host，走该远端 GPT 额度）
	Kind string `json:"kind"`
	// Model claude 系引擎的 --model 值（如 claude-opus-4-8）。remote-claude 必填（否则被路由到远端 codex）。
	Model string `json:"model,omitempty"`
	// Effort 该引擎的思考等级。claude 系→--effort，codex 系→model_reasoning_effort（二者同名同序：
	// low<medium<high<xhigh<max）。非空即覆盖全局 codex_reasoning；空则 claude 用 CLI 默认、codex 用全局值。
	Effort string `json:"effort,omitempty"`
	// Host remote-* 引擎的 remote_hosts 键。
	Host string `json:"host,omitempty"`
	// Label 展示名（如 "opus-4.8·max"）；仅用于 CLI/日志，不影响执行。
	Label string `json:"label,omitempty"`
}

// CrossProfile 是一对交叉验证引擎：A 先独立作答，B 独立作答后再拿 A 的结论对抗式交叉查漏。
type CrossProfile struct {
	A CrossEngine `json:"a"`
	B CrossEngine `json:"b"`
}

// XFrozenEngine 是入队时钉死的引擎执行规格——把从 config 解析出的执行参数快照进卡，B/C 直接套用，
// 不再随链在执行时从当前 config 重解析（防"入队后改 profile/codex_model 静默换引擎"的身份漂移）。
type XFrozenEngine struct {
	Model        string `json:"model,omitempty"`
	Effort       string `json:"effort,omitempty"`
	PreferRunner string `json:"prefer_runner,omitempty"`
	RemoteHost   string `json:"remote_host,omitempty"`
	CodexModel   string `json:"codex_model,omitempty"` // codex/远端 codex 引擎冻结的具体模型
	Label        string `json:"label,omitempty"`
}

// RemoteHostConfig 描述一台远程执行主机（SSH 可达 + 已装 codex）。
type RemoteHostConfig struct {
	// CodexBin 远端 codex 可执行名/路径（默认 "codex"）。
	CodexBin string `json:"codex_bin,omitempty"`
	// ClaudeBin 远端 claude 可执行名/路径（默认 "claude"）；跑远程 fable 设计等 claude 模型时用。
	// 远端 claude 走该主机自己的账号额度，与本机 claude 独立。
	ClaudeBin string `json:"claude_bin,omitempty"`
	// Sandbox 远端 codex 沙箱模式；Windows OS 沙箱不可用时用 "danger-full-access"
	// （靠 prompt 护栏 + 人工审 diff 兜底，不接 live 凭证/不下单）。默认 workspace-write。
	Sandbox string `json:"sandbox,omitempty"`
	// TmpDir 远端存放结果文件的目录（如 "D:/tmp"，用正斜杠）；空则用远端 cwd。
	TmpDir string `json:"tmp_dir,omitempty"`
	// Shell 远端 shell 类型："cmd"（Windows，默认）用 & 分隔 + type + 反斜杠路径；
	// "posix" 用 ; 分隔 + cat + 正斜杠路径。
	Shell string `json:"shell,omitempty"`
	// Reasoning 透传 -c model_reasoning_effort（空则用 codex 默认）。
	Reasoning string `json:"reasoning,omitempty"`
}

// RedlineWindow 是按每日本地时间生效的红线时段（"HH:MM"，跨零点用 from > to 表示）。
type RedlineWindow struct {
	From              string `json:"from"`
	To                string `json:"to"`
	RedlinePercent    int    `json:"redline_percent,omitempty"`
	QueueBudgetTokens int64  `json:"queue_budget_tokens,omitempty"`
}

const (
	typeReview       = "design-review"
	typeAssembly     = "prompt-assembly"
	typeSequence     = "sequence"
	typeCoordinate   = "coordinate"    // 分工协调：读队列+进度报告，产出任务分工并自动入队
	typeProgressPull = "progress-pull" // 进度回收：--resume 某会话，让它输出结构化进度报告
	typeCrossCheck   = "crosscheck"    // 交叉验证：双引擎独立作答→引擎乙拿引擎甲结论对抗式查漏（fable 顶替流）
)

func defaultConfig(claudeBin string) *Config {
	return &Config{
		ClaudeBin:         claudeBin,
		PollIntervalSec:   300,
		LimitFallbackMin:  30,
		CooldownMarginSec: 90,
		StepTimeoutMin:    60,
		MaxAttempts:       3,
		RetryBackoffMin:   5,
		MaxParallel:       1,
		ResumeFirst:       true,
		DrainRescanSec:    15,
		TypeOrder:         []string{typeProgressPull, typeCoordinate, typeReview, typeSequence, typeAssembly},
		QueueBudgetTokens: 0,
		RedlinePercent:    0,
		UsageFeedMaxAgeMin: 90,
		ModelWeights: map[string]float64{
			"default": 1, "opus": 5, "sonnet": 1, "haiku": 0.2, "claude-fable-5": 10, "fable": 10,
		},
		NoFallbackModels: []string{"claude-fable-5", "fable"},
		// 交叉验证默认引擎对：设计档模型撞限时，用两个不同引擎独立作答再交叉查漏顶替。
		// 甲=本机 claude opus（最高档 max）；乙=本机 codex，具体模型/推理档来自全局 codex_model/codex_reasoning
		// （乙的 Effort=max 覆盖为最高档）。换 profile 即换模型来源，无需改代码；档位改一个 Effort 字段即可。
		DefaultCrossProfile: "opus-codex",
		CrossProfiles: map[string]CrossProfile{
			"opus-codex": {
				A: CrossEngine{Kind: "claude", Model: "claude-opus-4-8", Effort: "max", Label: "opus·max"},
				B: CrossEngine{Kind: "codex", Effort: "max", Label: "codex·max"},
			},
		},
		ResumePrompt: "继续。上一条指令因为用量限额被中断，请从中断的地方接着完成当前任务；如果其实已经完成了，请直接说明完成情况。",
		TypeDefaults: map[string]TypeDefaults{
			typeReview: {
				PermissionMode: "default",
				AllowedTools: []string{
					"Read", "Grep", "Glob",
					"Bash(git log:*)", "Bash(git diff:*)", "Bash(git show:*)", "Bash(git status:*)", "Bash(ls:*)",
				},
			},
			typeAssembly: {
				PermissionMode: "default",
				AllowedTools: []string{
					"Read", "Grep", "Glob",
					"Bash(git log:*)", "Bash(git status:*)", "Bash(ls:*)",
				},
			},
			// 协调是全队列的编排大脑：小 token 量、高杠杆，用最强模型（预算紧可改 sonnet）；
			// 进度回收是机械总结，haiku 即可。
			typeCoordinate: {
				PermissionMode: "default",
				Model:          "opus",
				AllowedTools: []string{
					"Read", "Grep", "Glob",
					"Bash(git log:*)", "Bash(git status:*)", "Bash(ls:*)",
				},
			},
			typeProgressPull: {
				PermissionMode: "default",
				Model:          "haiku",
				AllowedTools: []string{
					"Read", "Grep", "Glob",
					"Bash(git log:*)", "Bash(git status:*)", "Bash(git diff:*)",
				},
			},
			// 交叉验证卡是只读分析（读契约/源码/改动，产出结论，不写业务仓）——模型由引擎对在派卡时套上。
			typeCrossCheck: {
				PermissionMode: "default",
				AllowedTools: []string{
					"Read", "Grep", "Glob",
					"Bash(git log:*)", "Bash(git diff:*)", "Bash(git show:*)", "Bash(git status:*)", "Bash(ls:*)",
				},
			},
			typeSequence: {
				PermissionMode: "acceptEdits",
				AllowedTools: []string{
					"Read", "Grep", "Glob", "Edit", "Write", "MultiEdit", "Task",
					"Bash(git add:*)", "Bash(git commit:*)", "Bash(git status:*)", "Bash(git diff:*)", "Bash(git log:*)",
					"Bash(mkdir:*)", "Bash(ls:*)",
					"Bash(go build:*)", "Bash(go test:*)", "Bash(go vet:*)",
					"Bash(npm run:*)", "Bash(npm test:*)", "Bash(pnpm run:*)", "Bash(pnpm test:*)",
					"Bash(python3 -m pytest:*)",
				},
			},
		},
	}
}

func configPath(root string) string    { return filepath.Join(root, "config.json") }
func tasksDir(root string) string      { return filepath.Join(root, "tasks") }
func progressDir(root string) string   { return filepath.Join(root, "progress") }
func crosscheckDir(root string) string { return filepath.Join(root, "crosscheck") }
func archiveDir(root string) string    { return filepath.Join(root, "archive") }
func logsDir(root string) string       { return filepath.Join(root, "logs") }
func templatesDir(root string) string  { return filepath.Join(root, "templates") }
func cooldownPath(root string) string  { return filepath.Join(root, "cooldown.json") }
func usagePath(root string) string     { return filepath.Join(root, "usage.json") }
func lockPath(root string) string      { return filepath.Join(root, ".lock") }
func taskLogPath(root, id string) string { return filepath.Join(logsDir(root), id+".log") }

func defaultRoot() string {
	if v := os.Getenv("CLAUDEGO_ROOT"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".claudego"
	}
	return filepath.Join(home, ".claudego")
}

func resolveRoot(flagVal string) string {
	if flagVal != "" {
		abs, err := filepath.Abs(flagVal)
		if err == nil {
			return abs
		}
		return flagVal
	}
	return defaultRoot()
}

func loadConfig(root string) (*Config, error) {
	data, err := os.ReadFile(configPath(root))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("找不到 %s，请先运行: claudego init", configPath(root))
		}
		return nil, err
	}
	cfg := defaultConfig("claude")
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", configPath(root), err)
	}
	return cfg, nil
}

func saveConfig(root string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(configPath(root), append(data, '\n'))
}

func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
