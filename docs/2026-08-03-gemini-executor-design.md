# Gemini CLI 备用执行器设计规格

日期：2026-08-03 ｜ 状态：已确认范围，单轮落地 ｜ 委托人确认记录见本文末尾

## 0. 一句话

把 Google 的 `gemini` CLI 接成 cardex 的**第二个异构执行器**（第一个是 codex）：独立 CLI、
独立输出协议、独立 Google 订阅额度；支持 `-runner gemini` 钉定主跑、`fallback_order` 降级改道、
交叉验证第五种引擎 kind，并按统一标准线给 Gemini 模型定档。接入方式一律以官方文档为准
（核实日期 2026-08-03，出处见 docs/guide.md）。

## 1. 为什么是异构执行器而不是引擎档案

引擎档案（engines.go）的前提是「Anthropic 兼容端点 + 复用 claude CLI」。Google 没有为
Gemini 提供官方 Anthropic 兼容端点，`gemini` CLI 是独立工具（Node，`-p` headless、
`--output-format json`、`--approval-mode`、`--session-id/--resume`），所以走 codex 那条
执行器分支路线。与 codex 的三点不同（都是拿引擎档案的既有基建来补 codex 的短板）：

1. **有会话**：`--session-id <uuid>` 起新会话（UUID 由 cardex 生成，不解析输出）、
   `--resume <uuid>` 续跑——钉定卡多步/限额续跑可用（codex 无会话）。
2. **有车道冷却**：复用引擎冷却基建 `cooldown-gemini.json`。Google 的配额是**账号级每日
   请求数**（OAuth 免费档 1000/天、AI Pro 1500、AI Ultra 2000、API key 免费档 250/天仅
   Flash），一张卡撞到当日耗尽 = 整条车道耗尽，必须挂车道而不是像 codex 只挂本卡。
3. **记账**：usage.json 打 `engine:"gemini"` 标（红线三通道照旧只管 claude；boardspend
   仍是 Unpriced 披露口径——CLI 报 token 不报美元）。

## 2. 统一能力分级（快照 2026-08-03）

评测源与既有分级表同源：Artificial Analysis 智能指数 **v4.1**（锚点同量表核对：Fable 5≈60、
Opus 4.8=55.7 沿用 2026-08-02 快照、K3=57、GLM-5.2=51、MiniMax-M3=44 全部吻合，无版本漂移）；
SWE-bench Verified（Vals AI 子集，2026-07/08）交叉核对。

| 模型（CLI 别名） | AA II v4.1 | SWE-bench V | 统一档位 |
|---|---|---|---|
| gemini-3.5-flash / gemini-3.6-flash（`flash`） | **50** | 68.6% / 77.5% | **sonnet 档**（与 GLM-5.2 同带） |
| gemini-3.1-pro-preview（`pro`） | **46** | **80.6%** | **haiku 档上沿**（距 sonnet 下沿 47 差 1 分） |
| gemini-3.5-flash-lite（`flash-lite`） | 36 | — | haiku 档 |
| gemini-2.5-pro | 26 | — | haiku 档 |

两个要点显式披露：

- **Flash 线在标准线上反超 Pro 线**（Google 的 Flash 迭代节奏快过 Pro），但编码向交叉信号
  （SWE-bench）Pro 仍略强；两线都明显低于 Kimi K3（93.4%）。
- **委托人决定**（2026-08-03）：档位槽映射按编码信号取 **高档→pro、低档→flash**
  （fable/opus→`pro`，sonnet→`flash`，haiku→`flash-lite`）；展示档位一律按标准线披露
  （pro=haiku 档上沿），要按牌面抬档用 `model_tiers` 覆写。
- **映射用 CLI 官方稳定别名**（`pro`/`flash`/`flash-lite`，来自官方 models.ts 常量），
  Google 轮换代次不用改配置；当前解析（2026-08-03）：pro→gemini-3.1-pro-preview 线、
  flash→gemini-3.5-flash 线。`auto` 被否决——撞配额会静默换模型，违反「档位漂移必须可见」。

## 3. 配置面（config.json）

```jsonc
"gemini_bin": "/opt/homebrew/bin/gemini",   // 空 = 未启用（钉定与降级都不可用）
"gemini_model": "pro",                       // 卡无模型时的默认；推荐官方别名
"gemini_models": {                           // 档位槽映射（缺省即下表，可整体覆写）
  "fable": "pro", "opus": "pro", "sonnet": "flash", "haiku": "flash-lite"
},
"gemini_approval_mode": "yolo",              // default|auto_edit|yolo|plan；空=yolo。
                                             // 复审位卡（design-review/交叉 C）恒强制 plan（只读）
"gemini_auth_env": "",                       // 可选：命名一个环境变量，其值注入 GEMINI_API_KEY
                                             // （密钥不进 config；空 = 继承环境/OAuth 缓存凭据）
"fallback_order": ["codex", "gemini"]        // gemini 进降级链（委托人 2026-08-03 确认排序）
```

- `gemini` 进 `engineReservedNames`（不得作引擎档案名）与 `fallback_order` 白名单。
- `gemini_models` 键限 fable/opus/sonnet/haiku；`gemini_approval_mode` 超出四值载入即拒。
- **无 gemini_fallback 开关**：进不进降级链由 `fallback_order` 成员资格决定（引擎同规）；
  也无 `gemini_reasoning`——gemini CLI 没有思考等级参数，交叉 profile 写 effort 直接报错
  （fail fast，不静默吞）。

## 4. 执行面（gemini.go）

- argv：`gemini -o json --approval-mode <mode> --skip-trust -m <model>`；prompt 走 stdin
  （headless 由非 TTY stdin 触发，`-p` 仅是另一触发方式；大 prompt 不担心 ARG_MAX）。
- 会话：卡无会话 → 生成 UUIDv4 传 `--session-id`；MidStep/多步续跑 → `--resume <uuid>`。
  会话只在**钉定主跑**回写卡面（改道卡不回写——冷却结束回 claude 时带着 gemini 会话就是
  跨引擎 --resume，与引擎档案同一禁区）。
- 模型解析 `resolveGeminiModel` 优先序：XGeminiModel（交叉冻结）> 卡级 GeminiModel
  （`-gemini-model`）> 按 t.Model 档位别名查 `gemini_models` 槽映射（本档缺失向下回落并
  披露 note）> `gemini_model` > 内置 `pro`。**永不落空**（空 = CLI 默认 auto，已被否决）。
- **非 sequence 卡全部**强制 `--approval-mode plan`（只读；复审/协调/装配/交叉/进度回收——
  与 codex「非 sequence 默认 read-only」同一纪律，且比只护复审位更严）；sequence 卡用
  `gemini_approval_mode`（空=yolo）。gemini 无 OS 沙箱，plan 是唯一硬护栏。**不建复审副本**
  ——plan 只读使 codex_worktree 那套副本机制非必需（codex 的 read-only 沙箱挡不住写才需要副本）。
- 输出：解析 stdout JSON `{response, stats, error}` → claudeResult。退出码 0/1/42/53。
  错误路径 Result 来自 stderr 抓取时**必须打 ResultFromTranscript**（codex P1 教训）。

## 5. 限额 / 认证 / 冷却

判据依据 = CLI 官方源码的错误分类（packages/core googleQuotaErrors.ts，核实 2026-08-03）：

- **终止态（当日耗尽）→ 车道冷却**：`PerDay|Daily` quotaId、"You have exhausted your daily
  quota"、`QUOTA_EXHAUSTED`、`limit: 0`、`INSUFFICIENT_G1_CREDITS_BALANCE`。写
  `cooldown-gemini.json`，卡挂 limit_paused；重置时间：先解析 `retryDelay`/瀑布，无则
  回退 `max(limit_fallback_min, 360)`——每日配额重置时刻无公开时间戳，半小时一撞是纯浪费。
- **认证/资格错误 → 同样挂车道（360min）**，reason 前缀 auth：`IneligibleTierError`、
  `UNSUPPORTED_CLIENT`、"Error authenticating"、`UNAUTHENTICATED`、"API key not valid"。
  重试无益且烧 attempts；挂车道让队列自愈（修好认证后冷却到点自恢复）。
  **实测背景（2026-08-03）**：本机 OAuth 个人免费档已被 gemini-cli 0.42 拒绝
  （`IneligibleTierError: UNSUPPORTED_CLIENT`，Google 要求个人免费档迁移 Antigravity）——
  启用需 GEMINI_API_KEY（AI Studio，免费档 250/天仅 Flash）或 Google AI Pro/Ultra 订阅
  OAuth 或 Vertex。此设计保证认证未修好时把 gemini 加进 fallback_order 也无害。
- **每分钟限流（PerMinute）/裸 429/503**：留给 transientRe/重试退避，不判限额不挂车道
  （瞬时拥堵挂 6 小时冷却是把小病治成大病——与引擎档案同一纪律）。
- 扫描面收敛：stderr 尾段 + JSON error.message（结构化），**不扫模型 response prose**。

## 6. 派发面（tick.go）

- 钉定：`PreferRunner=="gemini"` && `gemini_bin` 非空 && 车道不在冷却 → 派；否则跳过本轮，
  **绝不 fail-open 回 claude**。不要求 codexEligible（有会话，多步可用）。
- 改道：`geminiDivertOK` = gemini_bin 非空 + 车道不冷却 + **五道闸全量同规**
  （codexEligible 形状 / no_fallback_models / 交叉卡 / 复审位 / ——对应 codexDivertOK
  ①②③④⑤，一条不松）。`pickDivertRunner` 循环加 "gemini" 分支。
- `engineVia` 排除 "gemini"（否则会被误路由进引擎档案分支撞「档案不存在」）。

## 7. 交叉验证（第五种 kind）

`cross_profiles.<名>.a/b.kind` 增加 `"gemini"`：要求 `gemini_bin` 与 `gemini_model` 都非空
（身份必须显式，不吃内置 pro 回落）；profile 写 `model` 报错（用 config.gemini_model，与
codex 同规）；写 `effort` 报错（gemini 无思考等级参数）。冻结面：XFrozenEngine.GeminiModel
+ Task.XGeminiModel（与 XCodexModel 平行），identity = `"gemini|"+cfg.GeminiModel`。

## 8. CLI 面

- `-runner gemini`（add）；`-gemini-model` 卡级钉定。
- `cardex cmd`：gemini 卡打 `cd <dir> && gemini -m <模型> --approval-mode <mode>
  [--resume <sid>]`；**顺带修复 codex 卡打成 claude 命令的既有缺口**（本轮附带修）。
- `cardex doctor`：gemini_bin 存在性、认证信号（GEMINI_API_KEY / gemini_auth_env /
  OAuth 缓存凭据三者报「已配置/缺失」，值不回显；OAuth-仅 时提示免费个人档已被新客户端
  拒绝）、车道冷却状态。**顺带补 codex_bin 检查**（既有缺口）。

## 9. 看板 / 账本

- modelTierKeyword 内置线补 gemini 分支（flash-lite→haiku 先于 flash→sonnet；gemini 前缀
  其余→haiku；裸别名 pro/flash/flash-lite 精确匹配同档）；model_tiers 自定义覆写照常恒优先。
- effectiveModel 补 geminiSide 路径（source="gemini_model"）。
- usage.json 打 `engine:"gemini"`；boardspend/boardweight 缺口披露文案把 gemini 并入
  Unpriced 口径说明。

## 10. 调用优先级（编程类质价比，委托人 2026-08-03 中途指示）

依据委托人提供的 AA v4.1 质价比图（extracted 2026-07-31）+ 本轮快照 + SWE-bench 交叉：
本机已配车道的编程类改道顺位 = `["codex", "gemini"]`（gpt-5.6-sol max：AA 59 /
$1.23/task；gemini flash/pro：50/46，订阅额度按请求计）。复审位/交叉卡/no_fallback(fable)
恒不进链——「仅针对编程类」由质量地板天然保证（链上跑的就是编程执行形状的卡）。
未来若增订阅，编码向推荐顺位：codex → kimi(K3, SWE 93.4%) → gemini(flash) → glm →
minimax → mimo（gemini 排 glm 前是编码交叉信号已知 77.5% vs 未评；通用链仍按 AA 主档排序）。

## 11. 非目标

- 远端 gemini（RemoteHostConfig 全是 codex/claude 专名，等有真实需求再扩）；
- gemini 容器沙箱（`-s` 需 Docker/Podman，plan/yolo 二态已覆盖本轮场景）；
- 用量端点轮询（Google 无公开每日剩余额度端点——按项目纪律披露「本地计数下限」口径）；
- `engine:<name>` 交叉 kind 泛化（本轮只加 "gemini" 具名 kind，泛化等订阅引擎有交叉需求）。

## 12. 委托人确认记录（2026-08-03）

1. 档位映射以编码信号为准：高档→pro、低档→flash；用官方稳定别名；auto 否决。→ §2/§4。
2. 派发入口 = 钉定 + 降级链排 codex 后（`["codex","gemini"]`）。→ §3/§6/§10。
3. 交叉验证本轮纳入（第五种 kind）。→ §7。
4. 中途追加：按编程类质价比重排调用优先级（附 AA v4.1 质价比图）。→ §10。
5. 环境事实：本机 OAuth 免费档被 0.42 客户端拒绝（IneligibleTierError/UNSUPPORTED_CLIENT，
   迁移 Antigravity 公告）；集成按认证无关设计，认证故障挂车道自愈。→ §5。
