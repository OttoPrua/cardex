# 多订阅引擎档案（engine profiles）设计规格

日期：2026-08-02 ｜ 状态：已确认范围，分期落地 ｜ 委托人确认记录见本文末尾

## 0. 一句话

把「订阅计划」从 claude/codex 二元硬编码扩展成**通用引擎档案**：任何 Anthropic 兼容端点
（Kimi Code、GLM Coding Plan、MiniMax、小米 MiMo、OpenCode Go、Ollama Cloud…）都通过复用
`claude` CLI + 按任务注入环境变量接入；每个引擎独立冷却、独立限额解析、账本打标；派发侧
支持钉定主跑与用户自定义降级链。全部预设的端点/模型/接入方式**以各家官方文档为准**
（委托人 2026-08-02 明确），出处与 as-of 日期随分级表落 docs/guide.md。

## 1. 为什么是「档案」而不是逐家硬编码

- 各家官方接入方式完全同构：`ANTHROPIC_BASE_URL` + 认证变量 + 模型映射环境变量，跑的还是
  同一个 `claude` CLI。差异只在**数据**（端点/变量名/模型名/限额语义），不在**控制流**。
- 现有 codex 集成的教训：`useCodex bool` 这种二元开关每加一家就要再劈一刀。档案化后加
  DeepSeek/Qwen 只是 config 里多一段 JSON。
- codex 保持现状不动（它是真正的异构执行器：不同 CLI、不同输出协议、不同沙箱语义）。

## 2. 首批支持范围（委托人 2026-08-02 确认）

内置预设（`cardex engines add <preset>` 一键并入 config）：

| 预设名 | base_url | 认证注入变量 | 说明 |
|---|---|---|---|
| `kimi` | `https://api.kimi.com/coding/` | `ANTHROPIC_API_KEY` | Kimi 会员（Kimi Code），单一全球端点 |
| `glm-cn` | `https://open.bigmodel.cn/api/anthropic` | `ANTHROPIC_AUTH_TOKEN` | GLM Coding Plan 国内计费 |
| `glm-global` | `https://api.z.ai/api/anthropic` | `ANTHROPIC_AUTH_TOKEN` | 同计划国际端点（USD） |
| `minimax-cn` | `https://api.minimaxi.com/anthropic` | `ANTHROPIC_AUTH_TOKEN` | MiniMax Coding Plan 国内 |
| `minimax-global` | `https://api.minimax.io/anthropic` | `ANTHROPIC_AUTH_TOKEN` | 同计划国际端点 |
| `mimo` | `https://api.xiaomimimo.com/anthropic` | `ANTHROPIC_AUTH_TOKEN` | 小米 MiMo 开放平台（Token Plan 按量） |
| `opencode-go` | `https://opencode.ai/zen/go/v1` | `ANTHROPIC_AUTH_TOKEN` | OpenCode Go 多模型订阅（$12/5h、$30/周、$60/月美元等值额度，17 模型一把钥匙） |
| `ollama` | `https://ollama.com` | `ANTHROPIC_AUTH_TOKEN`（值=Ollama API key） | Ollama Cloud 官方云订阅（Free/Pro/Max 档），云端模型带 `:cloud` 后缀 |

两点澄清（委托人 2026-08-02 纠正后核实）：
- **OpenCode Go 是订阅计划而非执行器**：官方提供 Anthropic 兼容端点
  `https://opencode.ai/zen/go/v1`（`/v1/messages`），Claude Code 直接可用——因此走引擎
  档案接入，**不需要**先做 opencode CLI 执行器。opencode CLI 执行器降级为可选后续
  （引擎多样性/交叉验证用途，见 §10）。
- **`ollama` 预设指官方云订阅**（ollama.com，API key 认证，模型如 `glm-4.7:cloud`），
  不是本地推理；同一档案机制改 base_url 为 `http://localhost:11434` 也能指向本地实例，
  但那是用户自定义用法，预设按官方云订阅配置。

## 3. 统一能力分级（跨供应商标准线）

**评测源**：Artificial Analysis Intelligence Index（2026 版，快照 2026-08-02）为主，
SWE-bench Verified（Vals AI，2026-07）为编码向交叉核对。选它的理由：单一方法学覆盖全部
待评模型（167 个）、持续更新、有公开分数可引用——满足"统一标准下的分级而非各家自分高中低"。

**锚点 = Claude 各档在同一快照的分数**：fable 档锚 Claude Fable 5（59.9）/Opus 5（60.7），
opus 档锚 Claude Opus 4.8（55.7），sonnet 档锚 Claude Sonnet 5（53.4）。带边界取相邻锚点
中点；sonnet 带下沿取 47（= Sonnet 5 − 半档宽，Haiku 4.5 在 2026 量表上无同快照分数，
显式披露此近似）。**档位可被 config 逐引擎覆写**（`tier` 字段），表格只是推荐默认。

| 模型 | AA II | SWE-bench V | 档位 | 备注 |
|---|---|---|---|---|
| Kimi K3 | 57.1 | 93.4%（Fable 5 = 95.0%） | **opus 档** | 距 fable 带 0.9 分，编码实测接近旗舰 |
| GLM-5.2 | 51.1 | — | **sonnet 档** | |
| Qwen3.7 Max | 46.0 | — | haiku 档上沿 | 未内置预设，自定义档案可配 |
| MiniMax M3 | 44.4 | — | **haiku 档** | 1M 上下文 |
| Kimi K2.6 | 44.2 | — | haiku 档 | |
| DeepSeek V4 Pro | 44.3 | — | haiku 档 | 未内置预设 |
| MiMo-V2.5-Pro | 42.2 | — | **haiku 档** | |
| GLM-4.7 | 33.7 | — | haiku 档下沿 | Ollama Cloud `glm-4.7:cloud` 同分 |
| OpenCode Go（多模型） | 随所选模型 | — | **opus 档**（映射 kimi-k3 时） | 档位=其 models 映射实际指到的模型档 |
| Ollama Cloud（多模型） | 随所选模型 | — | 随映射模型 | 云端目录以 ollama.com/search?c=cloud 为准 |

多模型订阅（opencode-go/ollama）的档位不是订阅本身的属性，而是**其 models 映射当前指向
的模型的档位**——doctor/看板按映射结果显示，换映射即换档。

**推荐降级链默认值**（按映射模型的档位序，仅当用户显式启用对应引擎才进入链）：
`fallback_order: ["codex", "kimi", "opencode-go", "glm-cn", "minimax-cn", "mimo", "ollama"]`。
出厂默认仍是 `["codex"]`——**不配置引擎则行为与今日完全一致**。

数据不足纪律：分数表带 as-of 日期写进 docs/guide.md；没有分数的模型（新 GLM 小版本等）
不编造，标"未评"，档位需用户显式指定。

## 4. 配置面（config.json）

```jsonc
"engines": {
  "kimi": {
    "base_url": "https://api.kimi.com/coding/",
    "auth_env":  "KIMI_API_KEY",          // 三选一：环境变量名（推荐）
    "auth_file": "",                       //         密钥文件路径（0600）
    "auth_value": "",                      //         明文（仅限 ollama 这类哑值，文档警告）
    "auth_var":  "ANTHROPIC_API_KEY",      // 注入给 claude 子进程的变量名（预设已按官方文档配好）
    "models": { "fable": "", "opus": "k3", "sonnet": "kimi-for-coding", "haiku": "kimi-for-coding" },
    "default_model": "kimi-for-coding",
    "extra_env": { "API_TIMEOUT_MS": "3000000" },
    "limit_fallback_min": 30,              // 该引擎限额无重置时间戳时的保守回退；0 继承全局
    "tier": "opus"                         // 该订阅的能力档（评级表默认，可覆写）
  }
},
"fallback_order": ["codex"]                // claude 冷却/红线时的改道顺序；默认只有 codex
```

- **认证解析优先序 `auth_env > auth_file > auth_value`**；解析结果只进子进程 env，
  永不落日志/事件/`cmd` 输出。`auth_env`/`auth_file` 两种都支持是委托人指定。
- **键名保留字**：`claude`、`codex`、`remote` 不得作为引擎名（与 Runner 标签语义冲突，
  loadConfig 校验拒绝）。
- 预设由 `cardex engines add <preset>` 写入 config（不含密钥），`cardex engines` 列出
  已配引擎 + 可用预设 + 各自冷却状态。

## 5. 执行面（runner）

- **模型解析** `resolveEngineModel(profile, t.Model)`：空→`default_model`；命中档位别名
  （haiku/sonnet/opus/fable，含 `claude-fable-5` 归一）→查 `models` 映射，本档为空则**向下
  一档回落并在任务日志披露**；其余字符串视为供应商原生模型名直通（`-model k3` 钉定）。
- **env 注入** `buildEngineEnv`：从 `os.Environ()` 起手，**先剥掉**全部受管变量
  （`ANTHROPIC_BASE_URL/AUTH_TOKEN/API_KEY/MODEL`、`ANTHROPIC_DEFAULT_*_MODEL`、
  `CLAUDE_CODE_SUBAGENT_MODEL`）再注入档案值——防用户 shell 里的全局 ANTHROPIC_* 与档案
  串味；`ANTHROPIC_DEFAULT_{OPUS,SONNET,HAIKU}_MODEL` 一并注入，让 claude CLI 内部的
  子代理/摘要调用也落在该供应商；`MAX_THINKING_TOKENS` 沿用全局逻辑。
  默认 claude 路径 env 组装**一字不动**（继承现状）。
- **会话语义与 claude 同构**：引擎卡有 SessionID、可 `--resume`、多步可用（会话是本地
  transcript，重放到同一端点）。这点优于 codex（无会话）。
- **降级改道仍限 codexEligible 形状**（单步/fresh、无既有会话）：跨引擎接续会话在机制上
  可行但语义上是引擎身份漂移，与交叉链纪律同理，不做。钉定主跑（`-runner kimi`）无此限制。

## 6. 限额与冷却（按引擎分账）

- 冷却文件：claude 沿用全局 `cooldown.json`（语义不变）；每个引擎新增
  `cooldown-<name>.json`（`engineCooldownPath`）。**Kimi 撞限额绝不挂住 claude 队列，
  反之亦然**——这是本设计要买的核心东西之一。
- runTask 新增分支 1c（引擎限额）：命中→`setEngineCooldown` + 卡面 `resume_at`，
  有会话则 `MidStep` 续跑语义与 claude 分支同构；成功→只清自己的引擎冷却。
- **限额判据**：复用 `isLimitHitClaude` 的扫描面收敛（stderr 尾段 + 非 transcript 的
  res.Result）——引擎跑的就是 claude CLI，输出形状同构。limitRe 已天然覆盖 Kimi 官方错误
  文档的全部限额措辞（429 五小时窗/月度、**403 计费周期**都含 "usage limit" 字面量——
  403 因此先于失败分类命中限额分支，不会被误判成 permission→held）。引擎侧追加保守模式：
  `quota exceeded|insufficient quota|额度不足|余额不足`（只挂在引擎判据上，不动全局 limitRe）。
- **重置时间**：各家均无公开重置时间戳 → `parseResetEpoch` 瀑布解析不动，落到档案级
  `limit_fallback_min`；命中 `monthly|billing cycle` 措辞时抬到 `max(fallback, 360min)`
  ——月度耗尽半小时一撞是纯浪费。
- 裸 429/overloaded（Kimi「引擎过载/并发超限」）**留在 transientRe 退避重试**，不判限额
  ——瞬时拥堵挂半小时冷却是把小病治成大病。

## 7. 派发面（tick）

- `viaCodex map[string]bool` → `viaRunner map[string]string`（""=claude / "codex" / 引擎名）。
- 候选路由 switch 扩展：
  1. `PreferRunner == <引擎名>`：档案存在且该引擎不在冷却 → 钉定派发（与 codex 钉定同级，
     不受 claude 冷却/红线影响——独立额度）；档案缺失/冷却中 → 跳过，**绝不 fail-open 回
     claude**（与 codex 钉定同一纪律）。
  2. claude 被冷却/红线拦住：按 `fallback_order` 逐个找第一个可用出路——codex 沿用
     `codexDivertOK` 五道闸；引擎项检查：档案存在、引擎不在冷却、`codexEligible` 形状、
     `noFallback` 模型黑名单、`typeCrossCheck`、`qualityFloorCard` 复审位地板。质量地板
     规则**全量沿用**，一条不松。
- 账本：`usage.json` 记录加 `engine` 字段（omitempty，旧记录=claude）；`queueWindowSpent`
  只统计 claude 记录——**红线三通道语义不变，只管 claude 订阅**。引擎用量本地计数在看板
  披露（P2），不参与红线判定（各家无公开用量端点，编造保底线违反项目纪律）。

## 8. CLI 面

- `-runner` 值域：`codex` ∪ config.engines 键；报错信息列出全部可选值。
- `cardex cmd <id>`：引擎卡打印 env 前缀形态的手动接管命令，密钥用 `$KIMI_API_KEY` 变量
  引用或 `$(cat <auth_file>)`，**绝不解析出明文**。
- `cardex doctor`：逐引擎检查 base_url 非空、认证可解析（只报"已配置/缺失"，不回显值）、
  models 映射非空、tier 合法；`ollama` 额外探活提示（不强制）。
- `cardex engines [add <preset>]`：见 §4。

## 9. 看板（P2，披露式）

- 卡片 Runner 标签显示引擎名（boardmodel 空 Runner 回填 "claude" 的现状保持）。
- 额度区新增引擎行：冷却状态（cooldown-<name>.json）+ 本地账本窗口内调用计数。
  燃尽曲线/订阅端点用量**不做**——无公开数据源，按项目惯例显式披露"数据不足"，绝不估算。
- boardspend：引擎卡的 CostUSD 是 claude CLI 按 Anthropic 价目估的口径（供应商价目不同），
  归入 Unpriced 披露口径，不混进平均值。
- modelTier 映射补充：k3→高档，kimi-for-coding/glm-5.2→中档，minimax-m3/mimo-v2.5-pro/
  glm-4.5-air/k2.6→轻量档（§3 表）。

## 10. 分期与非目标

- **P1**：§4-§8（配置/执行/限额冷却/派发/CLI）+ 全量测试。
- **P2**：§3 分级落码 + §9 看板披露 + 推荐链文档。
- **P3**：docs/guide.md 新章节（中英）、config.md 键表、README 能力表、changelog。
- **P4（后续单开，已降级为可选）**：opencode CLI 执行器（OpenCode Go 订阅已由引擎档案
  覆盖，执行器仅为引擎多样性/交叉验证价值）；交叉验证 profile 支持 `engine:<name>` 引擎
  种类（把 Kimi/GLM 纳入双引擎交叉链）。
- **非目标**：各家用量端点轮询/逆向（无公开接口）；CLAUDE_CONFIG_DIR 会话隔离（P1 共享
  `~/.claude`，接受"引擎会话混在同一 projects 目录"，list/进度回收因此天然可用）；
  跨引擎会话接续（引擎身份漂移）。

## 11. 委托人确认记录（2026-08-02）

1. 首批：Kimi Code + GLM Coding Plan（各分节点）+ MiniMax/MiMo/Ollama/OpenCode 逐个核实
   纳入；分级必须是统一标准线下的档位而非各家内部高中低。→ §2/§3。
2. 一等引擎逐个可钉定；找可信评测源做推荐排序与分级，用户自由指定哪些订阅加入消耗计划、
   自定义降级顺序。→ §3/§7。
3. 看板本轮披露式接入。→ §9。
4. 凭据 auth_env / auth_file 两种都支持。→ §4。
5. 纠正（同日晚些）：ollama 指**官方云订阅**（非本地模型）；opencode go 指 **OpenCode
   官方多模型订阅计划**；所列各家均为官方线上订阅，端点/模型/接入方式一律以官方文档为准。
   → §2 澄清段、`opencode-go`/`ollama` 预设改写、P4 降级为可选。
6. 追加（P1-P3 交付后）：增设**自定义分级选项**——无更强模型的机队按当前模型能力配置使用。
   落地为 `config.model_tiers`（模型→档位关键字，优先于 §3 的绝对标准线；精确>前缀匹配、
   键全小写、坏值载入即拒），作用于档位展示与引擎档位推导；派发路由仍由 engines.models
   槽位映射与 fallback_order 决定，档位不参与路由。§3 的标准线表退为"未列条目的回落默认"。
