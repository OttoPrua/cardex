# Agent 协作生态对标与理想终态（2026-07-23）

> 来源：15-agent 调研工作流（5 路网络调研 + 逐条独立事实核验 + 2 路本地深读 + 三路综合）。
> 约 50 条关键事实主张经对抗式核验：48 confirmed / 2 refuted（已在文中更正）。
> 本文是设计输入与再评估依据，不是裁决；立卡见同目录 `2026-07-23-eco-borrow-cards.md`。

## 1. 生态地图（四层）

**互操作协议层** — 解决"agent 与工具/客户端/彼此如何标准化通信"：

- **MCP**：模型↔工具事实标准。2025-11-25 版新增实验性 Tasks 异步原语；下一版（RC 于 2026-05-21 锁定，计划 2026-07-28 定稿，截至本文尚未定稿）为破坏性版本：协议核心无状态化，Roots/Sampling/Logging 弃用，Tasks 降为扩展。对 ClaudeGo 的价值在"向执行器暴露编排器状态/工具"，不在编排本身；勿依赖 Sampling。
- **ACP（Zed 的 Agent Client Protocol）**：标准化的恰是 ClaudeGo 用 `claude -p` 自造的那一层——本地 client↔coding agent 子进程，JSON-RPC 2.0 over stdio（session/new、session/prompt、流式 session/update、session/request_permission）。官方列表 agent 侧 43 个：Gemini CLI 原生、Claude Code 经 Zed 维护的 adapter（claude-agent-acp，非 Anthropic 原生）、Codex CLI 经 Zed adapter、Copilot CLI 2026-01 公测 ACP 支持。风险：规范由 Zed 主导演进；2026-06 Anthropic 曾计划把第三方 ACP 用量拆出订阅按 API 计费（暂缓执行，需持续盯）。
- **A2A（Google→Linux Foundation）**：跨组织服务型 agent 互调，2026-04 v1.0（Signed Agent Cards），150+ 组织、企业云侧生产部署真实。对单机本地编排是错误层级，仅跨机场景做储备。
- **IBM ACP / BeeAI**：2025-08 已并入 A2A，终结，无需跟踪。ANP、AGNTCY/OASF：活跃但早期，观望。

**编排框架层** — 进程内多 agent 的图/角色/对话编排（LangGraph、Magentic-One、CrewAI、OpenAI Agents SDK、MetaGPT、AgentScope 2.0、CAMEL）。它们编排库内 agent 对象而非订阅制 CLI 进程——与 ClaudeGo 同构不同料：机制可借鉴、代码难复用。格局注意：AutoGen 2025-10 起维护模式，官方继任是 Microsoft Agent Framework。

**CLI-agent 编排工具层** — 与 ClaudeGo 同赛道。亚类：本地并行管理（claude-squad 8.2k★、Conductor 闭源推断/免费）、看板化（vibe-kanban 27.5k★，母公司 2026-04 关停转社区维护）、角色化重编排（Gas Town 17.2k★、ruflo/原claude-flow 65.5k★）、同会话桥接（AgentBridge）、远程遥控（happy 22.8k★；omnara 已归档——其 CLI wrapper 因 Claude Code 频繁更新不可维护，教训值得记取）、治理层（MartinLoop 39★）。**全部查证项目中，"订阅限额感知调度"与"claude+codex 跨厂商交叉验证"均无成熟对应物——这是 ClaudeGo 的真实差异化。**

**一方官方能力层** — 划定"不应重复造"的边界：

- Claude Code：subagents（model/effort/background/isolation:worktree，v2.1.198 起默认后台）；agent view（`claude --bg`/`claude agents --json`，supervisor 托管、自动 worktree+draft PR，research preview）；agent teams（实验性：共享任务列表+文件锁+mailbox）；headless `claude -p --bare --output-format stream-json --json-schema`+`--resume`。
- Codex CLI：`exec --json`、`exec resume/--last`、`fork`、`cloud exec --attempts 1-4`（best-of-N）、`codex apply`、`codex mcp-server`。
- Gemini CLI：2026-04 subagents；云端异步走 Jules extension。
- Copilot coding agent：GitHub Actions 沙箱、59 分钟硬上限、单仓库、仅 GitHub 托管（核验更正：现行文档无"draft PR"表述，是分支上工作、按需建 PR）。
- 额度观测：CodexBar（63 provider）、ccusage、以及**未文档化的 `api.anthropic.com/api/oauth/usage` 端点**（`anthropic-beta: oauth-2025-04-20` 头 + 复用 Claude Code OAuth 凭据，返回 5h/7d utilization 与 resets_at）。核验更正：该端点响应头**不带** `anthropic-ratelimit-unified-*` 系列（那出现在 Messages API 推理响应上），只可信 body。

## 2. 与 ClaudeGo 最相关的项目对比

**AgentBridge**（raysonmeng，v0.1.25，MIT，Bun）：互补非竞争。面向交互式 TUI 的同会话双向协作：Claude Code Channels（MCP research preview）+ Codex App Server（JSON-RPC），bridge.ts 前台 + daemon.ts 常驻，支持 mid-execution injection。亮点：三级标签路由（[IMPORTANT] 立即 / [STATUS] 攒批 3 条或 15s / [FYI] 丢弃）且只有 agentMessage 过桥；协作契约单点存 AGENTS.md 不逐消息附带；额度接力=turn 边界干净停 + `.agent/checkpoint.md` + per-pending 幂等墓碑 at-most-once 续接（实验性/opt-in，Claude 侧 best-effort）。风险：Channels 依赖 research preview 无一方声明；配套 agent-quota-guard 无可查独立仓库；默认 `--dangerously-skip-permissions`/`--yolo`。Roadmap（issue #212 投票）：OpenCode、OpenClaw、**Hermes Agent**、Gemini CLI adapter。

**Gas Town**（现 gastownhall/gastown，Go）：同为 Go 本地多 agent 队列，走角色化重架构（Mayor/Polecats/Witness/Deacon/Refinery）。它有而我们没有：git-backed 任务账本 Beads（任务 DAG 入 git，崩溃可恢复）；Bors 式 merge queue+失败自动 bisect；独立 watchdog 巡逻卡死。我们有而它没有：限额感知调度、双引擎交叉验证、codex 降级链、类型化模板流水线。其 token 消耗量级（第三方报道约 $100/时）与订阅额度哲学相悖。

**MartinLoop**（仅 39★ 但理念最近）：治理第一公民——USD/token/迭代硬预算停机、verifier gate（npm test 类真实检查才算完成）、13 类失败分类学、HMAC 签名 JSONL run receipt。正补我们"无事件日志、失败只会退避重试"两个短板。

**vibe-kanban**：任务表示最完整（卡片→worktree→diff→行内评论→PR，10+ 可插拔 executor，MCP server 供 agent 反向读写任务板）。关停警示此定位难商业化（自用无此虑）。

**Claude Code agent view/teams**：与我们重叠最大的一方能力。启示：sequence/coordinate 里"纯并行开会话"的部分应逐步薄封装官方机制，自研集中在官方明确不做的缝隙（见 §3）。

**happy**：移动/网页遥控+E2E 加密+push 远程审批，是 held 卡远程放行人机界面的最佳参照。

## 3. 一方不做、留给 ClaudeGo 的缝隙（护城河清单）

1. **跨厂商仲裁/交叉验证**——无任何一方做 claude+codex cross；各家只编排自家模型。
2. **订阅限额感知调度**——生态只有观测（CodexBar/ccusage/usage 端点），"观测→调度决策"这层空白，且原料现成。
3. **跨厂商统一任务队列+本地审计**——一方状态各自散在 `~/.claude`/`~/.codex`，无统一账本。
4. **自管远程主机分流**——一方 cloud 全绑自家沙箱+GitHub。
5. **类型化任务流水线**（design-review/prompt-assembly/fix-cycle 模板链）仍属应用层空白。

## 4. 可借鉴清单（按价值排序）

1. **订阅用量端点直读**（一方生态）：治"本地账本看不见桌面端消耗"。成本低。→ CG-1
2. **Progress Ledger + 停滞触发重规划**（Magentic-One 双账本）：coordinate/sequence 每步产出"完成否/卡住否/下步派谁"，stall 超限换执行器/重拆。成本中。（暂不立卡，待 CG-2 事件流就绪后按已证需求评估）
3. **失败分类分流**（MartinLoop 13 类 + CAMEL retry→decompose→replan）：替代单一 retry_backoff。成本中。→ CG-3
4. **run receipt 事件账本**（MartinLoop）：治"看板无事件日志、活动流靠状态反推"。成本低。→ CG-2
5. **幂等墓碑续接**（AgentBridge）：limit_paused/mid_step/cross reconcile 注入至多一次。成本低。→ CG-4
6. **watchdog 巡逻**（Gas Town）：正面处理"记录不修"的 review sync 超时误报竞态。成本中低。→ CG-5
7. **动作级审批谓词+暂停态序列化**（OpenAI Agents SDK needs_approval/RunState）：held 细粒度化，支撑远程审批。成本中。（待远程审批有已证需求再立卡）
8. **审阅结论蒸馏**（CrewAI learn=True）：design-review 结论蒸馏回 prompt-assembly 素材库。成本中。（观察）
9. **任务板 MCP server**（vibe-kanban）：队列/额度暴露为 MCP 工具，coordinate 从"注入快照文本"升级为实时查询；对齐 MCP Tasks 方向。成本中。（观察，等 MCP 新版定稿）
10. **git-backed 任务账本**（Gas Town Beads）：只借"状态变更留痕"思想（由 CG-2/CG-4 小代价覆盖），整体迁移不做。
11. **ACP headless client**：战略价值高但储备观望（见"明确不做"，含再评估触发器）。
12. **跨 agent 消息降噪**（AgentBridge 标签分级+攒批）：cross 链模板层可低成本吸收。

## 5. 理想终态：从"想法"到"生产外效"的可证伪流水线

### 5.1 分层架构与单一事实源

四层结构，编排关系单向向下：

- **L3 人（裁决层）**：唯一的意图与授权来源。裁决立 BD 条目入 `_DECISIONS`（单写点：编排 session 回填）；hold/release 是人对机器的唯一波次闸门。
- **L2 ClaudeGo（开发编排层）**：不生产代码，只做四件事——派卡、守额度红线、跑交叉审核链、把"完成"约束在机械验收接口上。开发进度唯一真相 = ClaudeGo 队列状态；索引不自标 done，会话记忆不算数。
- **L1 coding agent（开发执行层）**：Claude/codex CLI 按卡执行，产物只有三种——git commit、digest 绑定的证据文件、待登记清单。一次性劳工，状态全落盘，进程死了从文件续。
- **L0 生产面**：Perlica 控制面（SQLite control store）持有耐久控制真相（authority/grant/lease/receipt）；**Hermes 运行时 agent 只拥有"执行"**（会话、run、control tool bridge），本地文件仅运行态非权威；领域系统拥有业务真相；连接器不拥有策略，只产可对账 receipt。

事实源四处分立、互不越权：裁决在 _DECISIONS、开发进度在 ClaudeGo 队列、控制状态在 Perlica store、业务真相在领域系统。每处一个 owner，其余全是投影。

**杠杆倒置的模型路由**：贵而稀缺的模型（fable/opus）只花小 token 干高杠杆事——协调分工、对抗复审、死锁终裁；大 token 搬砖（实现/修复/机械回收）交 codex sol/terra、sonnet、haiku。额度是一等调度输入：双通道红线；claude 冷却按资格降级到 codex 独立 GPT 池；但钉定卡宁排队不换引擎——**交叉验证的独立性高于吞吐**。

### 5.2 全流程走查：给 Perlica 加一个新连接器

1. **提出**（人→文件）：编排 session 过第一性三问（问题链已证？承重假设证据等级？必要性测试？），通过立 BD；拿不准登记待裁不擅断。
2. **设计**（codex sol，xhigh）：设计卡 `claudego add -file` 入队。产出连接器状态机（prepared→claimed→effect_started→{succeeded|unknown|safely_failed}）、receipt schema、验收门定义——每道门写明"注入什么反例应报红"；报不了红的门不许存在。
3. **对称复审**（fable，小 token）：`-review-after` 自动生成；sol 产出必经 fable 对抗证伪；p0/p1 皆空才 pass；三轮死锁升终裁。
4. **拆卡放行**（prompt-assembly→人）：拆 2~6 步可验证序列，产出卡自动入队并 `-hold`；人逐卡 release 即波次门。
5. **并行实现**（opus/codex 多卡）：每卡独立 worktree；开工首步自门——git log 核对前卡 commit 锚，缺失即停卡挂 finding（队列顺序不当依赖门）。
6. **交叉审核**（双引擎链）：A 独立作答→B 同题独立作答（甲结论走 0600 侧车隔离）→C 对抗合并。入队冻结引擎身份防漂移；实现复审可分流远端，设计复审留 Mac（须实读 pinned 源码证伪）。
7. **修复链**（fix-cycle）：按类闭合、禁改弱断言凑绿、规格歧义登记待裁；超 max_fix_rounds 升级卡 held 交人。
8. **验收**（机械接口，不认 prose）：make test 全绿 + OfflineGuard 出网/外效计数为零 + 变异测试（删边界校验门必须变红）+ test-ready gate——blocked 只允许因缺真实 live/人工证据，文档与 fixture 不能豁免硬门。live 测试双 opt-in，唯一授权路径 = 08 gate 序列 + scope-bound operator 批准；卡与文档均不构成授权。
9. **生产接线**（开发/生产分界线）：coding agent 使命到"commit+证据"为止。此后：Perlica registry 装载连接器 → policy compiler 生成 per-profile 允单 → Hermes profile agent 经 control tool bridge 在授权与租约下调用 → 每次外效产 receipt → unknown 经 business-key 对账收敛。**coding agent 永不出现在运行时。**
10. **沉淀**：Closeout 事件驱动收口防假 done；复审结论蒸馏回模板素材库；待登记清单由编排 session 单写点回填；README 与中文注释随代码同 commit。

### 5.3 三档落地路径

- **现在就能做**：骨架已跑通（H2 波即实例）。低成本补三件：run receipt 事件账本（CG-2）、oauth/usage 端点直读（CG-1）、幂等墓碑（CG-4）。
- **需要新机制**：失败分类分流（CG-3）；watchdog 巡逻+review sync 竞态根修（CG-5）；progress ledger 停滞检测；held 动作级审批谓词+远程放行界面；任务板 MCP server。Hermes 侧：五危险 toolset 等 read-only split 或异 UID 沙箱才解冻；live gate 逐条用真实证据转绿。
- **远期依赖协议成熟**：ACP 统一 executor 层（储备观望）；MCP Tasks 定稿后对齐任务板；生产 telemetry 反哺开发面——按第一性纪律，等已证需求出现再建。

**终态一句话：人只出裁决，贵模型只出判断，便宜模型只出劳动；所有状态在文件里，所有"通过"都能被反例打红，每个账本只有一支笔。**

## 6. 明确不做与再评估触发器

简版见 `2026-07-23-eco-borrow-cards.md` 末节"明确不做"；**完整展开版（含义/情景/若做的影响/为何不做，兼作未来开源产品叙事素材）见 `2026-07-23-positioning-moat-and-non-goals.md`**。
