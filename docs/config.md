# cardex 配置参考

**中文** | [English](config.en.md) · 返回 [README](../README.md)

## 配置速查（~/.cardex/config.json）

| 键 | 默认 | 说明 |
|---|---|---|
| `poll_interval_sec` | 300 | launchd/daemon 轮询间隔 |
| `limit_fallback_min` | 30 | 解析不到重置时间时的等待 |
| `cooldown_margin_sec` | 90 | 重置时间上再加的安全余量 |
| `step_timeout_min` | 60 | 单步硬超时（防跑飞） |
| `max_attempts_per_step` | 3 | 单步失败重试上限 |
| `retry_backoff_min` | 5 | 非限额错误的重试退避基数（分钟） |
| `resume_first` | true | 被打断任务优先续跑 |
| `type_order` | 进度回收>协调>审核>序列>装配 | 同优先级时的类型顺序 |
| `resume_prompt` | … | 限额中断后的续跑提示词 |
| `type_defaults.*.model` | 装配/协调/审核 Opus；落地 Sonnet；回收 Haiku | 各类型默认来源档位；Codex 主路由按档位解析实际 GPT-5.6 模型；Fable 仅供显式最难裁决 |
| `type_defaults.<类型>` | 见内置表 | 类型默认执行参数。**条目内没写的字段沿用内置同类型的值**：JSON 只按键合并，只写 `{"design-review": {"model": "opus"}}` 会把整条替换成"只有 model"，`allowed_tools` 变空——复审卡的只读工具集就此静默消失。想彻底不下发工具白名单请写 `skip_permissions: true`，不要把 `allowed_tools` 留空（留空与"没写"在 JSON 里不可区分）。整个类型条目缺失则不烘焙任何参数（那是"该类型没配"这另一种意图） |
| `no_fallback_models` | ["claude-fable-5","fable"] | 这些设计档模型冷却期不降级 codex，宁可排队等 claude |
| `thinking_tokens` | 0 | >0 时给 claude 调用设 MAX_THINKING_TOKENS（设计活加大思考预算） |
| `max_fix_rounds` | 3 | "实现→对抗审核→自动修复"闭环的**全局**轮次上限，超过挂 held 升级卡交人裁；被 `stakes_policy.<档>.max_fix_rounds` 按档覆盖 |
| `stakes_policy` | low=不复审 / normal=跟随 / high=实现卡强制复审+抬 high+自动修复 1 轮 | 卡级投入产出分档查表（`add -stakes`），档内字段 `review` / `default_effort` / `max_fix_rounds`（`0`=跟随全局）。自动复审仅允许 `sequence`；其它类型入队与运行期都会归零。**入队即钉到卡面**、运行期不回查；见[进阶指南 · 投入产出分档](guide.md#卡级投入产出分档-stakes--复核深度查表) |
| `retro_every_n_done` | 0（关） | 每 N 张卡进 done 终态自动入队一张 haiku 复盘卡（只读统计、proposal-only）；建议 10，见[进阶指南 · 自动复盘卡](guide.md#自动复盘卡retro_every_n_done) |
| `queue_budget_tokens` 等 | 0（关） | 5 小时额度红线，见[进阶指南 · 额度红线](guide.md#5-小时额度红线保底额度) |
| `oauth_usage` / `oauth_usage_*` | false | 订阅端点直读（第三用量源），端点未文档化——异常按数据不足处理 |
| `max_parallel` | 1 | 单次 tick 并行任务数（写类任务同目录串行；design-review/progress-pull 只读类型豁免，可同仓并发） |
| `default_runner` | ""（历史 Claude） | 未显式钉执行器的新卡默认主路由；`codex` 会覆盖手工卡和自动审核/修复/收口/复盘/emit 卡。显式 Claude 会话续跑与 cross profile 不被改写 |
| `owner_routing_enforced` | `false` | Owner 六行生产路由硬锁；开启后，完整路由、精确模型/effort、Fable 配额唯一触发的 Sol/ultra 双答案与 Sol/max 第一性合并、独立审核卡 Sol/max 身份任一漂移均在加载期拒绝。新 `sequence` 卡必须显式填写 `route_class=backend|general`。受管 board/tick 再设 `CARDEX_REQUIRE_OWNER_ROUTING=1`，可防删除该键后静默退回旧策略 |
| `codex_bin` / `codex_fallback` | 空 / false | 冷却期备用执行器，见[进阶指南 · Codex 备用执行器](guide.md#codex-备用执行器限额空窗不断档) |
| `codex_fallback_model` | "" | 非 Opus claude 卡降级到 codex 时的通用模型；空回退 `codex_model` |
| `codex_fallback_opus_model` / `codex_fallback_opus_reasoning` | `gpt-5.6-sol` / `xhigh` | Opus 档 claude 卡降级到 codex 时的默认模型与思考档；默认不因 `stakes=low` 降档 |
| `codex_opus_simple_model` / `codex_opus_simple_reasoning` | 空（关闭） | 可选的低投入 Opus 显式降档；只有两项配置非空且卡为结构化 `stakes=low` 才生效，当前生产策略保持关闭 |
| `codex_tier_models` / `codex_tier_reasoning` | 见内置映射 | Codex 主跑与可回退卡的档位槽位：fable→sol/max、opus→sol/xhigh、sonnet→luna/max、haiku→luna/xhigh |
| `codex_reasoning` | "" | 无来源档位可解析时的全局兜底推理档；卡面显式 effort 优先于档位默认 |
| `codex_review_sandbox` | "worktree-write" | codex 只读分析卡(design-review/crosscheck 等)的沙箱策略。默认 `worktree-write`:**本机** codex 建一次性隔离副本 + `--sandbox workspace-write`,复审可跑测试/写夹具做动态验证,副本落 `<root>/tmp/codex-review-work/`,卡结束即删,原仓永不受写污染(CG-R3)。**远端** codex 只对 `t.Dir` 位于 `remote_mirror_root` 之下的镜像卡放宽；默认用 `workspace-write`，若该主机显式配置 `sandbox: "danger-full-access"`（Windows OS sandbox runner 不可用），严格镜像子孙继承该值。交叉/协调/回退等真实业务仓仍维持 `--sandbox read-only` 硬保证。改 `readonly` 全线回落旧行为。**取值写错时按最小权限回落 `readonly`(fail-closed)并在日志披露一次**；键留空/不写才用默认 `worktree-write`。sequence 卡不受此配置影响。 |
| `gemini_bin` | 空 | Gemini CLI 可执行文件路径，非空即启用第二异构执行器：`-runner gemini` 可钉定主跑；`fallback_order` 含 `"gemini"` 时 claude 空窗期也可改道（五道闸与 codex 全量同规）。车道冷却落 `cooldown-gemini.json`（配额是账号级的，挂车道不挂单卡）；账本打 `engine:"gemini"` 标，不占 claude 红线预算。见[进阶指南 · Gemini CLI 备用执行器](guide.md#gemini-cli-备用执行器第二异构执行器) |
| `gemini_model` | ""（内置 "pro"） | 卡无模型时的默认；推荐官方稳定别名 `pro`/`flash`/`flash-lite`（核实 2026-08-03）。**永不落空传给 CLI**——空即 `auto` 路由，撞配额会静默换模型，已否决 |
| `gemini_models` | fable/opus→pro，sonnet→flash，haiku→flash-lite | 档位槽映射（fable/opus/sonnet/haiku → gemini 模型/别名），claude 卡改道 gemini 时按 `t.Model` 档位查此表；高档取 `pro` 是按编码交叉信号（SWE-bench）的显式决定 |
| `gemini_approval_mode` | ""（=yolo） | `sequence` 卡的 `--approval-mode`（default/auto_edit/yolo/plan）。非 `sequence` 卡（复审/协调/装配/交叉/进度回收）恒强制 `plan`（只读）——gemini 无 OS 沙箱，plan 是唯一硬护栏，与 codex「非 sequence 默认 read-only」同一纪律 |
| `gemini_auth_env` | "" | 可选：命名一个环境变量，其值在执行时注入 `GEMINI_API_KEY`（密钥不进 config，与引擎档案 `auth_env` 同一纪律）。空 = 继承环境（OAuth 缓存凭据 / 已 export 的 `GEMINI_API_KEY`）。注意（2026-08-03 实测）：OAuth 个人免费档已被 gemini-cli 0.42+ 拒绝，需 API key 或 Google AI Pro/Ultra 订阅 OAuth |
| `opencode_bin` / `opencode_model` / `opencode_models` | 空 | 原生 OpenCode CLI 执行器；复用 OpenCode 本地登录凭据，不复制 API key。`-runner opencode` 可显式钉定 |
| `opencode_night_opus` | 空（关闭） | 兼容的单目标夜间 OpenCode Opus 自动路由：`enabled/start_hour/end_hour/timezone/model/variant/limit_fallback_min`；显式 `-runner opencode` 不受窗口限制 |
| `kimi_cli_bin` / `kimi_cli_home` / `kimi_cli_model` / `kimi_cli_effort` | 空 | 原生 Kimi Code CLI 执行器；`kimi_cli_home` 指向已登录目录（空时 `~/.kimi-code`）。Cardex 在自身数据根建立隔离运行目录，只软链接 OAuth credentials，不复制 token；生产 K3 使用 `kimi-code/k3`，max 通过 CLI 官方 `KIMI_MODEL_THINKING_EFFORT` 子进程变量注入 |
| `kimi_cli_opus` | 空（关闭） | Owner 的非 backend Opus 第二腿：第一腿 Grok 4.6/xhigh 只有在合格非鉴权失败且三轴证明通过后才串行排队严格 Kimi K3/max；Kimi 合格失败后才到 Sol/xhigh，cooldown 不能用来跳腿 |
| `grok_build_bin` / `grok_build` | 空 / 关闭 | Owner 的 Grok 第一腿与精确逐档末腿：general Opus xhigh→Kimi K3/max→Sol/xhigh，backend Opus xhigh→Sol/max，Sonnet high→Luna/max，Haiku high→Luna/xhigh。生产必须把 `grok_build_bin` 钉到版本化二进制，任务前用 `--no-auto-update models` 做无模型登录预检。仅受信 runner/process 诊断中同时具备完整 401 Unauthorized、Invalid or expired credentials、auth_kind=none、upstream=Unauthenticated 和 reason=no auth context 家族才开启引擎级认证熔断；首卡 held 且不烧 attempts，后续卡保留原 Grok 腿，鉴权绝不推进 Kimi/Sol。`cardex doctor` 的真实预检成功后只解除认证熔断。任务/事件保存 requested/actual 执行身份、腿、attempt、observation、residue 与 fallback 读回。实现卡不自动追加复审，独立 `design-review` 卡直达 fresh Sol/max；配置漂移加载即拒 |
| `cursor_bin` / `cursor_model` / `cursor_fable` | 空 / 关闭 | 显式 Fable 主腿必须 `claude-fable-5-thinking-max`；仅在确认的 eligible Fable quota-limit 失败且安全证明通过后，profile 才串行只读 A=`grok-4.6/xhigh`、B=`gpt-5.6-sol/ultra`，独立 `merge=gpt-5.6-sol/max`。transport/stream/stall 等非配额失败保持 held，不建立 fallback 链。B/C 的模型由全局 `codex_model=gpt-5.6-sol` 冻结，profile 的 `model` 必须省略。Cardex 不启用 Cursor `--auto-review`/`--force`/`--yolo` |
| `engines` | {}（空） | 多订阅引擎档案：键=引擎名（小写字母数字连字符；claude/codex/remote 保留），值含 base_url、认证三选一（auth_env 环境变量名引用 / auth_file 文件 / auth_value 明文）、auth_var（注入变量名，仅 ANTHROPIC_AUTH_TOKEN/ANTHROPIC_API_KEY）、models 档位映射（fable/opus/sonnet/haiku → 供应商模型 ID）、default_model、extra_env、limit_fallback_min（0 继承全局）、tier 展示档位。内置预设用 `cardex engines add <名>` 并入；见[进阶指南 · 多订阅引擎](guide.md#多订阅引擎engine-profileskimi--glm--minimax--mimo--opencode-go--ollama-cloud) |
| `fallback_order` | ["codex"] | claude 冷却/红线时的改道顺序（`"codex"`/`"gemini"` 与 engines 键混排，逐个找第一个可用出路）；质量地板（`no_fallback_models`/交叉卡/复审位）对链上每一项同等生效 |
| `model_tiers` | {}（空） | 自定义分级表：模型 ID（全小写，精确或前缀匹配）→ 档位关键字（fable/opus/sonnet/haiku），优先于内置统一标准线。它同时驱动档位展示、引擎档位推导和 Owner 六行自动路由；因此映射变化会改变未显式 pin 新卡的实际派发。坏值载入即拒。见[进阶指南 · 自定义分级](guide.md#自定义分级model_tiers无更强模型的机队按牌面定档) |
| `cross_profiles` | {opus-codex} | 交叉验证链（`cardex cross`）：A/B 独立作答；可选 `merge` 冻结第三个合并引擎，省略时兼容旧行为由 B 承担 C。见[进阶指南 · 交叉验证](guide.md#交叉验证fable-顶替双引擎独立作答--对抗式交叉查漏) |
| `default_cross_profile` | "opus-codex" | `cross` 未指定 `-profile` 时用的引擎对 |
| `default_review_host` | "" | 全局默认审核主机（`remote_hosts` 的键）；三键齐备时本地实现卡自动分流，见[进阶指南 · 审核分流](guide.md#审核分流把只读审核负载摊到第二台机器) |
| `remote_mirror_root` | "" | 远端镜像根；与 `default_review_host` 成对，审核目录自动推导为 `<root>/<worktree名>` |
| `default_review_sync` | "" | 全局默认分流前同步命令（sh -c，cwd=实现卡目录）；三键缺一不套默认 |
| `remote_hosts.<name>.codex_only` | false | 为 true 时该主机机械禁止 Claude；带 Claude 模型及自动审核卡均在派发入口改道远端 Codex |

## 看板配置速查（~/.cardex/board.json）

看板**只读**这个文件，永不写它；文件缺失 = 全部走自动推导。

| 键 | 位置 | 说明 |
|---|---|---|
| `projects.<项目id>.name` / `.desc` / `.phases.<阶段名>` | 项目块 | 人工文案覆盖自动推导的项目/阶段介绍 |
| `projects.<项目id>.goal` | 项目块 | 目标锚定的「落地进度」（与卡片进度并列，不替换），见[进阶指南](guide.md#项目覆盖块-cardexboardjson) |
| `projects.<项目id>.kind_rules` | 项目块 | 人工分类规则（标题子串或任务 ID 全串 → kind）；坏规则逐条跳过并挂 `kind_rule_error` |
| `projects.<项目id>.planned_total_cards` | 项目块 | 「含预估余量」进度口径的人工计划锚点（阶段性计划总卡量），声明后恒压过历史膨胀率自动估算；计划达成/调整时人工更新即校准。0/不写 = 走自动估算 |
| `project_aliases` | **顶层** | 有序的「目录 → 项目」归组规则表 `[{"match":"<精确目录或 glob>","title":"<标题子串，可选>","project":"<项目名>"}]`。首条命中即用；归组优先级 **显式(`add -project`) > 别名 > 内建模式 > 目录启发式 > 「未分类」**。改这张表不动任何任务卡，下次快照重建全量追溯生效——这是整理存量野项目的手段。坏规则逐条跳过并挂 `project_alias_error`。详见[进阶指南 · 项目归属](guide.md#项目归属显式--别名--模式--启发式--未分类) |

`match` 不含通配符时是**精确匹配该目录**（防止给容器目录写一条规则就吞掉它下面所有项目）；要覆盖子树写 `X/*`（glob 匹配该目录或其任一祖先，故覆盖任意深度）。大小写不敏感。

提示词模板在 `~/.cardex/templates/*.md`，可直接修改（`{{GOAL}}` `{{DIR}}` `{{FOCUS}}` 会被替换；
`coordinate.md` 里的 `{{QUEUE}}` `{{PROGRESS}}` 在**派发时**替换为实时快照）。
