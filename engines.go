package main

// engines.go —— 多订阅引擎档案（engine profiles，2026-08-02 设计规格见 docs/2026-08-02-engine-profiles-design.md）。
//
// 「引擎」= 一个 Anthropic 兼容端点的订阅计划（Kimi Code / GLM Coding Plan / MiniMax /
// 小米 MiMo / OpenCode Go / Ollama Cloud…）。执行复用同一个 claude CLI，差异全部收敛为
// **数据**：base_url、认证变量、模型映射、限额回退——所以是档案（config 里一段 JSON），
// 不是每家一条硬编码分支（codex 那样的异构执行器才需要分支）。
//
// 与 codex 备用执行器的三点本质差异：
//   1. 引擎有会话：跑的是 claude CLI，SessionID/--resume/多步全部可用（codex 无会话）；
//   2. 引擎按名字分冷却：cooldown-<name>.json，Kimi 撞限额绝不挂 claude 队列（也不挂 GLM）；
//      claude 的全局 cooldown.json 语义不变；
//   3. 引擎的账本记录打 engine 标：不混进 claude 的 5 小时红线预算（红线三通道只管 claude 订阅）。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// EngineProfile 描述一个订阅计划引擎（config.engines 的值）。
type EngineProfile struct {
	// BaseURL 该计划的 Anthropic 兼容端点（注入 ANTHROPIC_BASE_URL）。必填。
	BaseURL string `json:"base_url"`
	// ---- 认证三选一，解析优先序 auth_env > auth_file > auth_value ----
	// AuthEnv 存密钥的**环境变量名**（如 "KIMI_API_KEY"）：config 可安心进备份，密钥留在
	// shell 环境/launchd plist 里。推荐方式。
	AuthEnv string `json:"auth_env,omitempty"`
	// AuthFile 密钥文件路径（建议 0600）：launchd 场景不想改 plist 时用。支持 ~/ 前缀。
	AuthFile string `json:"auth_file,omitempty"`
	// AuthValue 明文密钥。**仅限无真实密钥的场景**（如自建网关的哑值）；订阅密钥请用
	// auth_env / auth_file——config.json 是 0644 且常被整目录备份。
	AuthValue string `json:"auth_value,omitempty"`
	// AuthVar 注入给 claude 子进程的变量名，只认两个取值（claude CLI 只认这两个）：
	// "ANTHROPIC_AUTH_TOKEN"（默认，GLM/MiniMax/MiMo/OpenCode Go/Ollama 官方文档用它）
	// "ANTHROPIC_API_KEY"（Kimi Code 官方文档用它）。
	AuthVar string `json:"auth_var,omitempty"`
	// Models 档位别名 → 该计划的模型 ID（fable/opus/sonnet/haiku 四键，可缺省）。
	// 卡上 Model 是档位别名（含 claude-fable-5 这类全名）时查此表；本档缺失向下档回落并在
	// 任务日志披露；全缺则原样透传（各家网关都会把 claude 模型名映射到自家当前主力，
	// 官方 quick-start 只设 BASE_URL+KEY 就能跑正是靠这个）。
	Models map[string]string `json:"models,omitempty"`
	// DefaultModel 卡上没写 Model 时用的模型 ID；空 = 不传 --model，由网关端默认。
	DefaultModel string `json:"default_model,omitempty"`
	// ExtraEnv 额外注入的环境变量（如 API_TIMEOUT_MS）。键固定排序后注入，输出确定。
	ExtraEnv map[string]string `json:"extra_env,omitempty"`
	// LimitFallbackMin 该引擎限额错误解析不出重置时间时的回退等待（分钟）；0 继承全局
	// limit_fallback_min。命中月度/计费周期措辞时自动抬到 ≥360（月度耗尽半小时一撞是纯浪费）。
	LimitFallbackMin int `json:"limit_fallback_min,omitempty"`
	// Tier 该订阅的能力档展示标签（fable/opus/sonnet/haiku），来自统一分级表
	// （评测源与 as-of 日期见 docs/guide.md），可被用户覆写。只作展示/审计，不参与派发决策。
	Tier string `json:"tier,omitempty"`
}

// 引擎名保留字：与 Runner 标签的既有语义冲突（""=claude、"codex"、"remote:<host>"）。
var engineReservedNames = map[string]bool{"claude": true, "codex": true, "remote": true}

// engineNameRe 引擎名要进文件名（cooldown-<name>.json）与 Runner 标签，限小写字母数字与连字符。
var engineNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// validAuthVars 见 EngineProfile.AuthVar。未知值直接拒绝（fail fast）——注错变量名的表现是
// claude CLI 静默用本机订阅跑，额度花错了账还全程无感，比报错恶劣得多。
var validAuthVars = map[string]bool{"ANTHROPIC_AUTH_TOKEN": true, "ANTHROPIC_API_KEY": true}

// validateEngines 校验 config.engines，loadConfig 时调用：配置面错误要在读入时炸，
// 不能等到派发/执行时才发现（那时卡已经在队里静默跳过或跑错账号）。
func validateEngines(cfg *Config) error {
	for name, p := range cfg.Engines {
		if engineReservedNames[name] {
			return fmt.Errorf("engines.%s: %q 是保留字（claude/codex/remote），不能作引擎名", name, name)
		}
		if !engineNameRe.MatchString(name) {
			return fmt.Errorf("engines.%s: 引擎名只能是小写字母/数字/连字符（要进 cooldown-<名>.json 文件名与 runner 标签）", name)
		}
		if strings.TrimSpace(p.BaseURL) == "" {
			return fmt.Errorf("engines.%s: base_url 不能为空", name)
		}
		if p.AuthVar != "" && !validAuthVars[p.AuthVar] {
			return fmt.Errorf("engines.%s: auth_var %q 不合法（可选: ANTHROPIC_AUTH_TOKEN / ANTHROPIC_API_KEY）", name, p.AuthVar)
		}
		for alias := range p.Models {
			switch alias {
			case "fable", "opus", "sonnet", "haiku":
			default:
				return fmt.Errorf("engines.%s: models 的键 %q 不是档位别名（可选: fable/opus/sonnet/haiku；供应商原生模型请在卡上用 -model 直接钉）", name, alias)
			}
		}
	}
	for _, name := range cfg.FallbackOrder {
		if name == "codex" {
			continue
		}
		if _, ok := cfg.Engines[name]; !ok {
			return fmt.Errorf("fallback_order 含未配置的引擎 %q（engines 里没有这个键）", name)
		}
	}
	// 自定义分级表：值域与 engines.models 的键同一套档位关键字；键强制全小写——
	// 允许混大小写会让 "GLM-5.2" 与 "glm-5.2" 两条并存，前缀匹配挑到哪条看 map 遍历运气。
	for model, tier := range cfg.ModelTiers {
		if strings.TrimSpace(model) == "" {
			return fmt.Errorf("model_tiers 含空模型名键")
		}
		if model != strings.ToLower(model) {
			return fmt.Errorf("model_tiers 的键 %q 必须全小写（匹配按小写做，混写会产生影子条目）", model)
		}
		switch tier {
		case "fable", "opus", "sonnet", "haiku":
		default:
			return fmt.Errorf("model_tiers[%q]=%q 不是档位关键字（可选: fable/opus/sonnet/haiku）", model, tier)
		}
	}
	return nil
}

// engineDisplayTier 返回引擎的展示档位：显式 tier 恒优先；没写则从档位映射里最高一档的
// 模型经 modelTierKeyword（自定义分级表优先）推导——多模型订阅（opencode-go/ollama）换个
// 映射就换档，不该要求用户手动同步 tier 字段。推不出返回 ""（展示层显示 "-"）。
func engineDisplayTier(cfg *Config, p EngineProfile) string {
	if p.Tier != "" {
		return p.Tier
	}
	for _, a := range engineTierOrder {
		if m := p.Models[a]; m != "" {
			if kw := modelTierKeyword(cfg, m); kw != "" {
				return kw
			}
		}
	}
	if p.DefaultModel != "" {
		return modelTierKeyword(cfg, p.DefaultModel)
	}
	return ""
}

// enginePresets 是内置预设（cardex engines add <名> 并入 config，不含密钥）。
// 端点/认证变量/模型 ID 全部取自各家官方文档（核实日期 2026-08-02，出处见 docs/guide.md）；
// 模型代次更新快，映射只是起手值，以订阅页当前提供为准、可随时改 config。
func enginePresets() map[string]EngineProfile {
	return map[string]EngineProfile{
		// Kimi Code（Kimi 会员）：单一全球端点；官方档位 kimi-for-coding / k3 / k3-256k。
		// 5 小时窗口约 300–1200 次请求（按会员档位），限额错误措辞见官方 error-reference。
		"kimi": {
			BaseURL: "https://api.kimi.com/coding/",
			AuthEnv: "KIMI_API_KEY",
			AuthVar: "ANTHROPIC_API_KEY",
			Models:  map[string]string{"opus": "k3", "sonnet": "kimi-for-coding", "haiku": "kimi-for-coding"},
			Tier:    "opus", // K3：AA II 57.1 / SWE-bench V 93.4%（快照 2026-08-02）
		},
		// GLM Coding Plan：国内计费端点。glm-5.2（1M 上下文）已含于 Coding Plan。
		"glm-cn": {
			BaseURL:  "https://open.bigmodel.cn/api/anthropic",
			AuthEnv:  "GLM_API_KEY",
			Models:   map[string]string{"opus": "glm-5.2", "sonnet": "glm-5.2", "haiku": "glm-4.5-air"},
			ExtraEnv: map[string]string{"API_TIMEOUT_MS": "3000000"},
			Tier:     "sonnet", // glm-5.2：AA II 51.1
		},
		// 同计划国际端点（USD 计费，api.z.ai）。
		"glm-global": {
			BaseURL:  "https://api.z.ai/api/anthropic",
			AuthEnv:  "GLM_API_KEY",
			Models:   map[string]string{"opus": "glm-5.2", "sonnet": "glm-5.2", "haiku": "glm-4.5-air"},
			ExtraEnv: map[string]string{"API_TIMEOUT_MS": "3000000"},
			Tier:     "sonnet",
		},
		// MiniMax Coding Plan：国内端点。主力 MiniMax-M3（1M 上下文）。
		"minimax-cn": {
			BaseURL: "https://api.minimaxi.com/anthropic",
			AuthEnv: "MINIMAX_API_KEY",
			Models:  map[string]string{"opus": "MiniMax-M3", "sonnet": "MiniMax-M3", "haiku": "MiniMax-M3"},
			Tier:    "haiku", // MiniMax-M3：AA II 44.4
		},
		"minimax-global": {
			BaseURL: "https://api.minimax.io/anthropic",
			AuthEnv: "MINIMAX_API_KEY",
			Models:  map[string]string{"opus": "MiniMax-M3", "sonnet": "MiniMax-M3", "haiku": "MiniMax-M3"},
			Tier:    "haiku",
		},
		// 小米 MiMo 开放平台（Token Plan 预付费按量）。
		"mimo": {
			BaseURL: "https://api.xiaomimimo.com/anthropic",
			AuthEnv: "MIMO_API_KEY",
			Models:  map[string]string{"opus": "mimo-v2.5-pro", "sonnet": "mimo-v2.5-pro", "haiku": "mimo-v2-flash"},
			Tier:    "haiku", // MiMo-V2.5-Pro：AA II 42.2
		},
		// OpenCode Go：一把钥匙 17 模型的美元等值额度订阅（$12/5h、$30/周、$60/月）。
		// 官方 Anthropic 兼容端点 /zen/go/v1（/v1/messages）。档位=映射模型的档位。
		"opencode-go": {
			BaseURL: "https://opencode.ai/zen/go/v1",
			AuthEnv: "OPENCODE_API_KEY",
			Models:  map[string]string{"opus": "kimi-k3", "sonnet": "glm-5.2", "haiku": "deepseek-v4-flash"},
			Tier:    "opus", // 映射 kimi-k3 时
		},
		// Ollama Cloud 官方云订阅（Free/Pro/Max 档）：直连 ollama.com，云端模型带 :cloud 后缀
		// （目录以 ollama.com/search?c=cloud 为准）。同机制改 base_url 也可指本地实例（自定义用法）。
		"ollama": {
			BaseURL: "https://ollama.com",
			AuthEnv: "OLLAMA_API_KEY",
			Models:  map[string]string{"opus": "glm-4.7:cloud", "sonnet": "glm-4.7:cloud", "haiku": "minimax-m2.1:cloud"},
			Tier:    "haiku", // 随映射模型；glm-4.7 AA II 33.7
		},
	}
}

// ---- 认证解析 ----

// resolveEngineAuth 解析引擎密钥，优先序 auth_env > auth_file > auth_value。
// 返回值只进子进程 env——**永不落日志/事件/cmd 输出**（那些出口用 engineAuthRef）。
func resolveEngineAuth(p EngineProfile) (string, error) {
	if p.AuthEnv != "" {
		if v := strings.TrimSpace(os.Getenv(p.AuthEnv)); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("auth_env 指定的环境变量 %s 为空（export %s=<key> 或改用 auth_file）", p.AuthEnv, p.AuthEnv)
	}
	if p.AuthFile != "" {
		path := p.AuthFile
		if strings.HasPrefix(path, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			path = filepath.Join(home, path[2:])
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("读取 auth_file 失败: %w", err)
		}
		if v := strings.TrimSpace(string(data)); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("auth_file %s 内容为空", p.AuthFile)
	}
	if v := strings.TrimSpace(p.AuthValue); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("未配置认证（auth_env / auth_file / auth_value 三选一）")
}

// engineAuthRef 返回**引用形态**的密钥表达（给 cardex cmd 的手动接管命令用），绝不解析明文：
// auth_env → "$KIMI_API_KEY"；auth_file → `"$(cat <path>)"`；auth_value → 占位提示。
func engineAuthRef(p EngineProfile) string {
	switch {
	case p.AuthEnv != "":
		return "$" + p.AuthEnv
	case p.AuthFile != "":
		return `"$(cat ` + shellQuote(p.AuthFile) + `)"`
	case p.AuthValue != "":
		return "'<auth_value，见 config.json>'"
	}
	return "'<未配置认证>'"
}

func engineAuthVar(p EngineProfile) string {
	if p.AuthVar != "" {
		return p.AuthVar
	}
	return "ANTHROPIC_AUTH_TOKEN"
}

// ---- 模型解析 ----

// engineTierOrder 档位从高到低——本档映射缺失时向下档回落的查找序。
var engineTierOrder = []string{"fable", "opus", "sonnet", "haiku"}

// modelAlias 把卡上的 Model 归一成档位别名；不是别名（供应商原生模型名）返回 ""。
// "claude-fable-5"/"fable" → fable；"claude-opus-4-8"/"opus" → opus；依此类推。
func modelAlias(model string) string {
	lm := strings.ToLower(model)
	for _, a := range engineTierOrder {
		if strings.Contains(lm, a) {
			return a
		}
	}
	return ""
}

// resolveEngineModel 决定引擎执行用的模型 ID 与披露备注：
//   - 卡无 Model → default_model（可为空 = 不传 --model，网关端默认）；
//   - Model 是档位别名 → 查 models 映射，本档缺失向下档回落（fable→opus→sonnet→haiku），
//     全缺且有 default_model 用之；连 default_model 都没有则**原样透传**——各家网关都会把
//     claude 模型名映射到自家当前主力（官方 quick-start 只设 BASE_URL+KEY 就能跑）；
//   - 其余字符串视为供应商原生模型名直通（-model k3 / -model glm-5.2 钉定）。
//
// 回落/透传都返回非空 note，由 runTask 落任务日志——档位漂移必须可见，静默降档是事故。
func resolveEngineModel(p EngineProfile, taskModel string) (string, string) {
	if taskModel == "" {
		return p.DefaultModel, ""
	}
	alias := modelAlias(taskModel)
	if alias == "" {
		return taskModel, ""
	}
	start := 0
	for i, a := range engineTierOrder {
		if a == alias {
			start = i
			break
		}
	}
	for _, a := range engineTierOrder[start:] {
		m := p.Models[a]
		if m == "" {
			continue
		}
		if a != alias {
			return m, fmt.Sprintf("档位 %s 无映射，回落 %s 档 → %s", alias, a, m)
		}
		return m, ""
	}
	if p.DefaultModel != "" {
		return p.DefaultModel, fmt.Sprintf("档位 %s 无映射，用 default_model → %s", alias, p.DefaultModel)
	}
	return taskModel, fmt.Sprintf("档位 %s 无映射且无 default_model，原样透传 %s 由网关端映射", alias, taskModel)
}

// ---- 子进程环境注入 ----

// engineManagedEnvKeys 是引擎执行前必须从继承环境里剥掉的变量：用户 shell 里的全局
// ANTHROPIC_* / 模型映射若混进来，会把 A 引擎的调用打到 B 端点或算错模型——串味是静默事故。
// ANTHROPIC_SMALL_FAST_MODEL 是 DEFAULT_HAIKU_MODEL 的旧名，一并剥。
var engineManagedEnvKeys = []string{
	"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_MODEL",
	"ANTHROPIC_DEFAULT_FABLE_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL",
	"ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL",
	"ANTHROPIC_SMALL_FAST_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL",
}

// buildEngineEnv 组装引擎子进程的完整环境：os.Environ() 起手 → 剥受管键与 extra_env 同名键
// → 注入 base_url/认证/档位映射/extra_env/MAX_THINKING_TOKENS。**默认 claude 路径不经此函数**
// （invokeClaude 的 env 组装一字未动，见 runner.go）。
func buildEngineEnv(cfg *Config, p EngineProfile) ([]string, error) {
	token, err := resolveEngineAuth(p)
	if err != nil {
		return nil, err
	}
	drop := map[string]bool{}
	for _, k := range engineManagedEnvKeys {
		drop[k] = true
	}
	for k := range p.ExtraEnv {
		drop[k] = true
	}
	var env []string
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 && drop[kv[:i]] {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "ANTHROPIC_BASE_URL="+strings.TrimSpace(p.BaseURL))
	env = append(env, engineAuthVar(p)+"="+token)
	// 档位映射一并注入：claude CLI 内部的子代理/摘要调用（按别名挑模型）也要落在该供应商，
	// 不能静默漏回 Anthropic 的模型名打到网关上撞 404。
	tierEnvKeys := map[string]string{
		"fable": "ANTHROPIC_DEFAULT_FABLE_MODEL", "opus": "ANTHROPIC_DEFAULT_OPUS_MODEL",
		"sonnet": "ANTHROPIC_DEFAULT_SONNET_MODEL", "haiku": "ANTHROPIC_DEFAULT_HAIKU_MODEL",
	}
	for _, a := range engineTierOrder {
		if m := p.Models[a]; m != "" {
			env = append(env, tierEnvKeys[a]+"="+m)
		}
	}
	if m := p.Models["haiku"]; m != "" {
		env = append(env, "CLAUDE_CODE_SUBAGENT_MODEL="+m)
	}
	// extra_env 键排序后注入：map 遍历随机，环境组装必须确定（测试要逐字节断言）。
	extraKeys := make([]string, 0, len(p.ExtraEnv))
	for k := range p.ExtraEnv {
		extraKeys = append(extraKeys, k)
	}
	sort.Strings(extraKeys)
	for _, k := range extraKeys {
		env = append(env, k+"="+p.ExtraEnv[k])
	}
	if cfg.ThinkingTokens > 0 {
		env = append(env, fmt.Sprintf("MAX_THINKING_TOKENS=%d", cfg.ThinkingTokens))
	}
	return env, nil
}

// ---- 按引擎冷却（claude 的全局 cooldown.json 语义不变）----

func engineCooldownPath(root, name string) string {
	return filepath.Join(root, "cooldown-"+name+".json")
}

func loadEngineCooldown(root, name string) *Cooldown {
	data, err := os.ReadFile(engineCooldownPath(root, name))
	if err != nil {
		return nil
	}
	var c Cooldown
	if json.Unmarshal(data, &c) != nil {
		return nil
	}
	return &c
}

func setEngineCooldown(root, name string, until int64, reason string) {
	c := Cooldown{UntilEpoch: until, Reason: reason, SetAt: time.Now().Format(time.RFC3339)}
	data, _ := json.MarshalIndent(c, "", "  ")
	_ = atomicWrite(engineCooldownPath(root, name), append(data, '\n'))
}

func clearEngineCooldown(root, name string) { _ = os.Remove(engineCooldownPath(root, name)) }

// ---- 限额判据（引擎特化）----

// engineQuotaRe 是引擎侧**追加**的限额措辞（GLM/MiniMax/MiMo 未公开错误格式，按通用配额语汇
// 保守收网）。只挂在 isLimitHitEngine 上，不动全局 limitRe——扩全局正则会放大 claude/codex
// 路径的 transcript 误命中面。Kimi 的官方限额措辞（429 五小时窗/月度、403 计费周期）全部
// 含 "usage limit" 字面量，limitRe 已天然覆盖，无需在此重复。
var engineQuotaRe = regexp.MustCompile(`(?i)quota (?:exceeded|exhausted)|insufficient quota|exceeded your .{0,24}quota|额度不足|额度已用完|配额不足|配额已用尽|余额不足|欠费`)

// engineCycleRe 识别"月度/计费周期耗尽"措辞——这类限额的恢复以天计，回退等待抬到 ≥6 小时，
// 别按窗口级 fallback 半小时一撞（纯浪费派发轮次，还刷脏事件账本）。
var engineCycleRe = regexp.MustCompile(`(?i)monthly usage limit|billing cycle|monthly quota|本月额度|月度额度`)

// isLimitHitEngine 是引擎分支的限额判据。引擎跑的就是 claude CLI，输出形状与本机 claude
// 同构——扫描面收敛沿用 isLimitHitClaude 的纪律（stderr 尾段 + 非 transcript 的 res.Result，
// 不扫 stdout transcript 全量，防自审本仓 prose 误命中），在其上追加 engineQuotaRe。
func isLimitHitEngine(res *claudeResult, combined string) bool {
	if res != nil && !res.IsError {
		return false
	}
	scan := stderrTailFromClaudeCombined(combined)
	if isLimitHit(res, scan) {
		return true
	}
	text := scan
	if res != nil && !res.ResultFromTranscript {
		text += "\n" + res.Result
	}
	return engineQuotaRe.MatchString(text)
}

// limitHitForRunner 把 runTask 的限额判据路由从 (useCodex, remote) 二布尔扩展到 via 三态：
// via=引擎名（本机）→ isLimitHitEngine；其余组合原样走 limitHitForEngine（该函数与其测试
// TestLimitHitForEngineRoutesByFlags 一字未动——引擎是新增维度，不重排旧映射）。
func limitHitForRunner(via string, remote bool, t *Task, res *claudeResult, combined string) bool {
	if engineVia(via) && !remote {
		return isLimitHitEngine(res, combined)
	}
	return limitHitForEngine(via == "codex", remote, t, res, combined)
}

// engineVia 判断 via 标签是否引擎名（""=claude、"codex"=备用执行器，其余为引擎）。
func engineVia(via string) bool { return via != "" && via != "codex" }

// engineResetEpoch 解析引擎限额的恢复时刻：瀑布解析（parseResetEpoch）原样复用，只把
// 兜底回退换成档案级 limit_fallback_min；scanText（已收敛的限额措辞段）命中月度/计费周期
// 时回退抬到 ≥360 分钟。fullText 给瀑布（重置时间戳可能在任何一段），scanText 给周期分类
// （全量 transcript 里 "monthly" 是常见散文词，不能拿来定性）。
func engineResetEpoch(fullText, scanText string, cfg *Config, p EngineProfile, now time.Time) int64 {
	fb := p.LimitFallbackMin
	if fb <= 0 {
		fb = cfg.LimitFallbackMin
	}
	if engineCycleRe.MatchString(scanText) && fb < 360 {
		fb = 360
	}
	tmp := *cfg
	tmp.LimitFallbackMin = fb
	return parseResetEpoch(fullText, &tmp, now)
}

// engineLimitScanText 返回引擎判据实际命中的收敛文本（挂起原因/周期分类用），与
// isLimitHitEngine 的扫描面严格同源。
func engineLimitScanText(res *claudeResult, combined string) string {
	text := stderrTailFromClaudeCombined(combined)
	if res != nil && !res.ResultFromTranscript {
		text += "\n" + res.Result
	}
	return text
}

// ---- 派发护栏 ----

// engineProfile 按名取档案；不存在返回 false（调用方决定跳过还是报错）。
func engineProfile(cfg *Config, name string) (EngineProfile, bool) {
	if cfg == nil {
		return EngineProfile{}, false
	}
	p, ok := cfg.Engines[name]
	return p, ok
}

// taskEngineName 判定一张卡归属哪个引擎（钉定意图优先，其次最近实际执行器），按
// config.engines **成员资格**判定而非字符串启发——Runner 的取值域还有 "codex"/"remote:<host>"，
// 猜前缀迟早误伤。卡不属任何已配引擎返回 ""。
func taskEngineName(cfg *Config, t *Task) string {
	if cfg == nil || t == nil {
		return ""
	}
	if _, ok := cfg.Engines[t.PreferRunner]; ok {
		return t.PreferRunner
	}
	if _, ok := cfg.Engines[t.Runner]; ok {
		return t.Runner
	}
	return ""
}

// pinnedEngineReady 判断钉定引擎（PreferRunner=<引擎名>）此刻能否派发：档案存在且不在冷却。
// 不满足时**跳过本轮，绝不 fail-open 回 claude**——与 codex 钉定同一纪律（引擎身份是
// 用户显式意图，也常是额度归属的边界）。钉定径不要求 codexEligible：引擎有会话，多步可用。
func pinnedEngineReady(root string, cfg *Config, name string, now time.Time) bool {
	if _, ok := engineProfile(cfg, name); !ok {
		return false
	}
	return !loadEngineCooldown(root, name).active(now)
}

// engineDivertOK 判断 claude 被冷却/红线拦住时，该卡能否改道到引擎 name。
// 质量地板与 codexDivertOK 全量同规（一条不松）：无会话可断（codexEligible 形状）、
// no_fallback_models 钉定模型不降级、交叉卡引擎身份不偷换、复审位恒不降级；
// 额外一条：目标引擎自己不在冷却。
// 跨引擎接续会话在机制上可行（会话是本地 transcript），但那是引擎身份漂移——不做。
func engineDivertOK(root string, cfg *Config, name string, t *Task, now time.Time) bool {
	if cfg == nil || t == nil {
		return false
	}
	if _, ok := engineProfile(cfg, name); !ok {
		return false
	}
	if loadEngineCooldown(root, name).active(now) {
		return false
	}
	if !codexEligible(t) {
		return false
	}
	if noFallback(cfg, t.Model) {
		return false
	}
	if t.Type == typeCrossCheck {
		return false
	}
	if qualityFloorCard(t) {
		return false
	}
	return true
}

// pickDivertRunner 按 fallback_order 逐个找 claude 空窗期的第一个可用出路：
// "codex" 项走 codexDivertOK（五道闸原样），引擎项走 engineDivertOK。找不到返回 ""。
// 顺序即优先级——用户按能力/成本自定义（推荐序见 docs/guide.md 分级表）。
func pickDivertRunner(root string, cfg *Config, t *Task, now time.Time) string {
	if cfg == nil {
		return ""
	}
	for _, name := range cfg.FallbackOrder {
		if name == "codex" {
			if codexDivertOK(cfg, t) {
				return "codex"
			}
			continue
		}
		if engineDivertOK(root, cfg, name, t, now) {
			return name
		}
	}
	return ""
}
