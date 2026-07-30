package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
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
	ClaudeBin         string `json:"claude_bin"`
	PollIntervalSec   int    `json:"poll_interval_sec"`
	LimitFallbackMin  int    `json:"limit_fallback_min"`
	CooldownMarginSec int    `json:"cooldown_margin_sec"`
	StepTimeoutMin    int    `json:"step_timeout_min"`
	MaxAttempts       int    `json:"max_attempts_per_step"`
	RetryBackoffMin   int    `json:"retry_backoff_min"`
	// MaxParallel: 单次 tick 内最多并行跑几个任务（同一工作目录始终串行）。1 为纯串行。
	MaxParallel int  `json:"max_parallel"`
	ResumeFirst bool `json:"resume_first"`
	// DrainRescanSec: drain 等待期间的重扫周期（秒）。每周期重扫队列补派新就绪任务，
	// 并做取消对账（running 任务的文件被标 canceled 即击杀其进程）。0 用默认 15。
	DrainRescanSec int                     `json:"drain_rescan_sec,omitempty"`
	TypeOrder      []string                `json:"type_order"`
	ResumePrompt   string                  `json:"resume_prompt"`
	TypeDefaults   map[string]TypeDefaults `json:"type_defaults"`

	// ---- 5 小时额度红线（保底额度，给交互/突发任务留余量）----
	// QueueBudgetTokens: 滑动 5 小时窗口内，队列最多消耗的加权 token 数；0 关闭。
	// 只统计 cardex 自己派发的调用（桌面端消耗不可见），本质是"队列预算上限"。
	QueueBudgetTokens int64 `json:"queue_budget_tokens"`
	// RedlinePercent + UsageFeed: 外部全局用量源（CodexBar usage-history.jsonl 格式），
	// 最新 claude 5h 窗口样本 usedPercent 达到红线即停止派发；样本过期则放行（fail-open）。
	RedlinePercent     int    `json:"redline_percent"`
	UsageFeed          string `json:"usage_feed"`
	UsageFeedMaxAgeMin int    `json:"usage_feed_max_age_min"`
	// ---- 第三用量源：oauth/usage 端点直读 ----
	// 端点未文档化（api.anthropic.com/api/oauth/usage + anthropic-beta: oauth-2025-04-20），
	// 复用 ~/.claude/.credentials.json 的 OAuth accessToken。任何异常（网络/凭据/格式变更）
	// 一律按"数据不足"处理，绝不 crash；判红线时与 usage_feed 合并取最保守（百分比更大）值。
	// **只信响应 body，不解析响应头**（核验已推翻响应头带限流数值之说；沿用会被人蓄意伪造）。
	OAuthUsage           bool   `json:"oauth_usage,omitempty"`
	OAuthUsageURL        string `json:"oauth_usage_url,omitempty"`
	OAuthUsageCredsPath  string `json:"oauth_usage_creds_path,omitempty"`
	OAuthUsageMaxAgeMin  int    `json:"oauth_usage_max_age_min,omitempty"`
	OAuthUsageTimeoutSec int    `json:"oauth_usage_timeout_sec,omitempty"`
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
	// CodexFallbackModel 降级专用模型：claude 卡经 codex_fallback 改道 codex 时用它。
	// 生产配置应选择已授权且适合落地实现的模型；不要把未授权/禁用模型当隐式回退。
	// 空 = 沿用全局 codex_model。仅降级径生效；runner_pref=codex 主跑与远端 codex 不受影响。
	CodexFallbackModel string `json:"codex_fallback_model,omitempty"`
	CodexModel         string `json:"codex_model,omitempty"`
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
	// DefaultCrossProfile: `cardex cross` 未指定 -profile 时用的 profile 名。
	DefaultCrossProfile string `json:"default_cross_profile,omitempty"`

	// ---- 默认复审分流（把只读复审负载默认压到第二台机器，均衡两侧额度）----
	// DefaultReviewHost 非空时：本地实现卡（RemoteHost 空）的 review_after 自动复审、未显式声明 ReviewHost 者，
	// 默认分流到该主机（RemoteHosts 的键）。RemoteMirrorRoot 是远端镜像根，自动推导 ReviewDir=<root>/<worktree 名>；
	// DefaultReviewSync 是分流前跑的同步命令（sh -c，cwd=实现卡 Dir），把本地泳道同步到远端镜像。三者齐备才生效。
	DefaultReviewHost string `json:"default_review_host,omitempty"`
	RemoteMirrorRoot  string `json:"remote_mirror_root,omitempty"`
	DefaultReviewSync string `json:"default_review_sync,omitempty"`

	// ---- 卡级投入产出分档（BD-44，承 2026-07-31 委托人指示）----
	// StakesPolicy 是 stakes 档位 → 复核深度的查表（键 = low/normal/high）。
	// **只在 add 入队时查一次并把结果固化到卡面**（ReviewAfter/Effort），运行期不再回查——
	// 否则入队后改这张表会让在队卡的复核深度静默漂移（与 XFrozenEngine "入队即钉引擎身份" 同一纪律）。
	// 用户 config.json 里只写部分档位时，其余档位沿用内置默认：json.Unmarshal 往非 nil map 里按键
	// 合并，不会整表顶掉（loadConfig 从 defaultConfig 起手）。
	StakesPolicy map[string]StakesRule `json:"stakes_policy,omitempty"`

	// RetroEveryNDone: 每累计 N 张卡进入 done 终态，自动入队一张 haiku 复盘卡
	// （只读统计归档卡的失败类/修复轮数/成本/复审 verdict/改道事件，**proposal-only 不改配置**）。
	// 0 = 关闭（默认）；建议 10。计数器持久化在 <root>/retro_counter.json，单写点 = 本二进制。
	// 不带 omitempty：让 `cardex init` 生成的 config.json 里显式出现这一行，开关可被发现。
	RetroEveryNDone int `json:"retro_every_n_done"`

	// CodexReviewSandbox 控制本机 codex 只读分析卡(design-review/crosscheck/coordinate 等,
	// 非 sequence)的沙箱策略——CG-R3(承 BD-36 工具链③终裁 b/BD-39 附记):
	//
	//   "worktree-write"(默认): 为目标 dir 建一次性隔离副本(git clone --local --no-hardlinks
	//     + 应用未提交面 + untracked),codex 以 `--sandbox workspace-write` 跑在副本内。
	//     副本收工即删,原仓永不受写污染;赋能复审跑测试/写夹具做实证验证。
	//   "readonly": 沿用旧行为——codex 直接以 `--sandbox read-only` 跑在原仓,不建副本。
	//     只保留为可配置回退,不建议生产开启(阻断复审多轮动态验证,BD-36 立据的悬置问题)。
	//
	// 远端 codex 复审受同键控制:默认放开为 workspace-write(远端镜像本身已是 sync-lane
	// 分发的隔离副本,原仓保护语义等价);"readonly" 时仍强制 read-only。
	CodexReviewSandbox string `json:"codex_review_sandbox,omitempty"`
}

// StakesRule 是一个 stakes 档位的复核深度规则（config.stakes_policy 的值）。
type StakesRule struct {
	// Review 决定该档位是否配对抗复审：
	//   "on"     强制配（高价值卡不许省这一刀）
	//   "off"    强制不配（低价值卡不烧复审额度）
	//   "follow" 跟随 -review-after 的显式指定（空值同义，即不干预）
	Review string `json:"review,omitempty"`
	// DefaultEffort 是该档位的思考等级地板：**未显式 -effort 时**，卡上 effort 低于它就抬到它。
	// 只抬不降——已经是更高档（如类型默认给了 max）的卡不会被这张表拉低。
	DefaultEffort string `json:"default_effort,omitempty"`
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
	// CodexOnly 禁止该主机调用 Claude。命中时即使任务带 claude 模型或属于审核类型，
	// 也会在派发入口强制改道远端 codex；用于把主机额度边界做成机械约束而非派卡纪律。
	CodexOnly bool `json:"codex_only,omitempty"`
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

// CodexReviewSandbox 的合法取值。默认走 worktree-write 走一次性副本（BD-36 终裁 b）。
const (
	codexReviewSandboxWorktreeWrite = "worktree-write"
	codexReviewSandboxReadonly      = "readonly"
)

// codexSandboxWarnW 是"未知 codex_review_sandbox 取值"的披露出口。包级 var 而非直写 os.Stderr:
// 测试要能断言"确实披露了、且只披露一次"(见 TestResolvedCodexReviewSandboxUnknownFailsClosed)。
var codexSandboxWarnW io.Writer = os.Stderr

// codexSandboxWarned 记已披露过的未知值(原始串 → true)。resolvedCodexReviewSandbox 在每次 invoke、
// 每轮 tick 都会被调,不去重会把 launchd 日志刷成同一行噪声,反而淹没这条本该显眼的权限告警。
var codexSandboxWarned sync.Map

// resolvedCodexReviewSandbox 返回归一后的策略。取值域是三分而非二分:
//   - 未设置(cfg==nil / 空串)      → worktree-write(BD-39 终裁的默认策略);
//   - 显式合法值(两个常量之一)      → 原样;
//   - 未知值(拼错,如 "readonIy")    → readonly(保守侧)+ 首次遇到时披露一次。
//
// 集中一处兜底,避免 invokeCodex/invokeRemoteCodex/清理路径三处各自处理默认值时漂移。
//
// 【为什么未知值必须落保守侧】CG-R3b 修 1:旧实现把未知值与空值一并 default 到 worktree-write,
// 是安全向的 fail-open——委托人本意写 "readonly"(把 codex 关进只读沙箱),大写 I 与小写 l 打错一个
// 字母就静默拿到"clone 副本 + workspace-write"的更宽权限,且全程无任何提示:配置的收紧意图被拼写
// 事故反向放大成放宽。权限开关的通用纪律是"解析不了就取最小权限",故未知值一律 readonly。
// 【为什么空值不算未知】空值语义是"配置里没写这一项"——loadConfig 从 defaultConfig 起手再 unmarshal,
// 没写就该留默认值;把空值也压到 readonly 会把 BD-39 终裁的默认策略整个翻掉(且默认 config.json
// 的 omitempty 会让"没改过"的配置正好是空串),那是另一种事故,不是修复。
func resolvedCodexReviewSandbox(cfg *Config) string {
	if cfg == nil || cfg.CodexReviewSandbox == "" {
		return codexReviewSandboxWorktreeWrite
	}
	switch cfg.CodexReviewSandbox {
	case codexReviewSandboxWorktreeWrite:
		return codexReviewSandboxWorktreeWrite
	case codexReviewSandboxReadonly:
		return codexReviewSandboxReadonly
	default:
		warnUnknownCodexReviewSandbox(cfg.CodexReviewSandbox)
		return codexReviewSandboxReadonly
	}
}

// warnUnknownCodexReviewSandbox 对每个不同的未知取值披露一次:说清"读到了什么、回落到哪、想要更宽
// 该写什么"。静默回落是本病的另一半——权限被收紧而人不知情,下一轮复审只能静态阅读却查不出原因。
func warnUnknownCodexReviewSandbox(raw string) {
	if _, seen := codexSandboxWarned.LoadOrStore(raw, true); seen {
		return
	}
	fmt.Fprintf(codexSandboxWarnW,
		"警告: config.codex_review_sandbox=%q 不是合法取值(合法值: %q / %q);"+
			"按最小权限回落 %q——若本意是可写副本,请把该键改写为 %q。\n",
		raw, codexReviewSandboxWorktreeWrite, codexReviewSandboxReadonly,
		codexReviewSandboxReadonly, codexReviewSandboxWorktreeWrite)
}

// isRemoteMirrorPath 判断远端 dir 是否位于 cfg.RemoteMirrorRoot 之下(sync-lane 分发的一次性镜像)。
// 【为什么不用 filepath】远端 dir 可能是 Windows 路径("D:/Project/PO-lanes/ClaudeGo")或
// posix 路径("/mirror/foo"),且路径分隔符可能反斜/正斜混用(config 手写 vs sync 脚本推导)——
// filepath 会按本机分隔符做判断,跨平台易错。改用纯 / 语义的 path.Clean 归一 + 强制 trailing "/"
// 边界,消除"/foo" 假匹配"/foo-bar"这类前缀陷阱(见 TestRemoteCodexReviewSandbox 桌面案)。
// 【为什么要 path.Clean】纯前缀比对不会展开 ".." / "/." 段:
// "D:/Project/PO-lanes/../OtherRepo" 字面以 "D:/Project/PO-lanes/" 起头会误判为镜像,
// 真实业务仓就拿到 workspace-write——击穿"严格子孙才是镜像"硬保证(CG-R3 R2 P1-1)。
// 先归一分隔符再 path.Clean(纯 /,与本机分隔符无关)才能杀掉这类词法绕过。
func isRemoteMirrorPath(cfg *Config, dir string) bool {
	if cfg == nil || cfg.RemoteMirrorRoot == "" || dir == "" {
		return false
	}
	normDir := path.Clean(strings.ReplaceAll(dir, `\`, "/"))
	normRoot := path.Clean(strings.ReplaceAll(cfg.RemoteMirrorRoot, `\`, "/"))
	// path.Clean("") == ".";空/"." 根不足以确权,保守不算镜像。
	if normRoot == "" || normRoot == "." {
		return false
	}
	// 等于边缘不是子孙(严格子孙才算镜像);Clean 已把 "root/." 归一为 "root",
	// 所以这里同时挡住 ".../PO-lanes/." 这类"看着不等实际等"的桩。
	if normDir == normRoot {
		return false
	}
	return strings.HasPrefix(normDir, normRoot+"/")
}

// remoteCodexReviewSandbox 返回远端 codex 非 sequence 卡的沙箱模式(CG-R3 R1 P0-1):
//   - CodexReviewSandbox="readonly" → 强制 read-only(旧行为可配置回落);
//   - t.Dir 位于 cfg.RemoteMirrorRoot 之下(sync-lane 一次性镜像) → 默认 workspace-write;
//     若该远端主机显式配置 Sandbox=danger-full-access,则继承它。Windows 上 Codex 的
//     workspace-write/read-only runner 可能无法建立 pipe-in,而 danger-full-access 不走该
//     OS sandbox;这里只允许严格镜像子孙继承,真实业务仓仍不得放宽。
//   - 其余(crosscheck 远端腿/coordinate/progress-pull 远端卡/review divert 回退后的原仓)
//     → read-only,恢复"原仓字节永不受写污染"的沙箱级硬保证——这些路径 t.Dir 是真实业务仓,
//     不是一次性镜像,仅 prompt 纪律兜底不够(见 CG-R3 R1 复审 P0-1)。
//
// 判据"目录确为一次性镜像"用 isRemoteMirrorPath(cfg.RemoteMirrorRoot 前缀)——它是编排方
// 通过 sync-lane 分发副本的落地根,严格子孙才算镜像;等于/边缘不算(边界样例见测试)。
func remoteCodexReviewSandbox(cfg *Config, t *Task) string {
	if resolvedCodexReviewSandbox(cfg) == codexReviewSandboxReadonly {
		return "read-only"
	}
	if t != nil && isRemoteMirrorPath(cfg, t.Dir) {
		if cfg != nil {
			if host, ok := cfg.RemoteHosts[t.RemoteHost]; ok && host.Sandbox == "danger-full-access" {
				return "danger-full-access"
			}
		}
		return "workspace-write"
	}
	return "read-only"
}

func defaultConfig(claudeBin string) *Config {
	return &Config{
		ClaudeBin:          claudeBin,
		PollIntervalSec:    300,
		LimitFallbackMin:   30,
		CooldownMarginSec:  90,
		StepTimeoutMin:     60,
		MaxAttempts:        3,
		RetryBackoffMin:    5,
		MaxParallel:        1,
		ResumeFirst:        true,
		DrainRescanSec:     15,
		CodexReviewSandbox: codexReviewSandboxWorktreeWrite,
		TypeOrder:          []string{typeProgressPull, typeCoordinate, typeReview, typeSequence, typeAssembly},
		QueueBudgetTokens:  0,
		RedlinePercent:     0,
		UsageFeedMaxAgeMin: 90,
		ModelWeights: map[string]float64{
			"default": 1, "opus": 5, "sonnet": 1, "haiku": 0.2, "claude-fable-5": 10, "fable": 10,
		},
		NoFallbackModels: []string{"claude-fable-5", "fable"},
		StakesPolicy:     defaultStakesPolicy(),
		RetroEveryNDone:  0,
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
				Model:          "claude-fable-5",
				Effort:         "high",
				AllowedTools: []string{
					"Read", "Grep", "Glob",
					"Bash(git log:*)", "Bash(git diff:*)", "Bash(git show:*)", "Bash(git status:*)", "Bash(ls:*)",
				},
			},
			typeAssembly: {
				PermissionMode: "default",
				Model:          "claude-fable-5",
				Effort:         "high",
				AllowedTools: []string{
					"Read", "Grep", "Glob",
					"Bash(git log:*)", "Bash(git status:*)", "Bash(ls:*)",
				},
			},
			// 协调需要较强规划能力，但默认使用 Fable 高档即可；只有明确的复杂仲裁才单卡升 Opus。
			// 进度回收是机械总结，haiku 即可。
			typeCoordinate: {
				PermissionMode: "default",
				Model:          "claude-fable-5",
				Effort:         "high",
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
				Model:          "claude-fable-5",
				Effort:         "high",
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

func configPath(root string) string      { return filepath.Join(root, "config.json") }
func tasksDir(root string) string        { return filepath.Join(root, "tasks") }
func progressDir(root string) string     { return filepath.Join(root, "progress") }
func crosscheckDir(root string) string   { return filepath.Join(root, "crosscheck") }
func archiveDir(root string) string      { return filepath.Join(root, "archive") }
func logsDir(root string) string         { return filepath.Join(root, "logs") }
func templatesDir(root string) string    { return filepath.Join(root, "templates") }
func cooldownPath(root string) string    { return filepath.Join(root, "cooldown.json") }
func usagePath(root string) string       { return filepath.Join(root, "usage.json") }
func lockPath(root string) string        { return filepath.Join(root, ".lock") }
func taskLogPath(root, id string) string { return filepath.Join(logsDir(root), id+".log") }

// rootDirName / legacyRootDirName 是数据根在 $HOME 下的目录名（BD-44 改名）。
const (
	rootDirName       = ".cardex"
	legacyRootDirName = ".claudego"
)

// defaultRoot 解析数据根，顺序（-root flag 由 resolveRoot 在更外层优先处理）：
//
//	$CARDEX_ROOT > $CLAUDEGO_ROOT(兼容读,提示一次) > ~/.cardex(存在则用) > ~/.claudego(存在则用+提示) > ~/.cardex(全新默认)
//
// 【为什么"存在才用"而不是无条件切到 ~/.cardex】改名当天所有人的数据都还在 ~/.claudego。
// 无条件切新路径会让 cardex 在一个空目录上开张：队列看着一张卡都没有、launchd 照常 tick、
// 旧根里在跑的活没人管——这是最坏的失败模式（看起来正常，实则整队丢失）。所以新根不存在时
// 认旧根，并明说"legacy root，建议 cardex migrate"，把切换点交给人。
//
// 【为什么两个都不存在时落 ~/.cardex】那是全新装机，没有历史包袱，直接用新名。
func defaultRoot() string {
	if v := getenvCompat(envRoot, envRootLegacy); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return rootDirName
	}
	root := filepath.Join(home, rootDirName)
	if isExistingDir(root) {
		return root
	}
	if legacy := filepath.Join(home, legacyRootDirName); isExistingDir(legacy) {
		warnLegacyRootOnce(legacy)
		return legacy
	}
	return root
}

func isExistingDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

var legacyRootWarnOnce sync.Once

func warnLegacyRootOnce(legacy string) {
	legacyRootWarnOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "提示: 正在使用改名前的数据根 %s（legacy root）。迁移到 %s 请运行: cardex migrate\n",
			legacy, filepath.Join(filepath.Dir(legacy), rootDirName))
	})
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
			return nil, fmt.Errorf("找不到 %s，请先运行: cardex init", configPath(root))
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
