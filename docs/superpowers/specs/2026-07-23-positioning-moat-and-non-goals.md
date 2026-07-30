# ClaudeGo 定位、护城河与非目标（产品叙事素材）

> 注：产品已于 2026-07-31 更名 cardex（BD-44）；下文 claudego/ClaudeGo 均指现 cardex。

> 用途：设计立据 + 未来开源时的产品叙事素材（README 的 "Why ClaudeGo" / "Non-goals" 节可直接取材）。
> 依据：2026-07-23 生态对标（同目录 `2026-07-23-agent-collab-landscape.md`，约 50 条主张独立核验）。
> 裁决立据：PerlicaOptimize design-blueprint/_DECISIONS.md BD-39 及附记。

## 重命名触发器（2026-07-23 委托人裁）

开源决定落地 → 释放 held 卡 **CG-7 rename 设计卡（t0723-0304-c0d8）**：名字裁选材料（"Claude*" 品牌指引风险 + 现有名低售跨厂商护城河，perlicabridge 等 Perlica 家族名候选）+ appName 单点化 + 数据根迁移命令（在途任务零丢失，fail-closed）+ CLI 兼容 shim（保护 Hermes 卡等跨仓活调用方）+ 远端足迹迁移顺序 + 开源卫生扫描。释放前须先按当时现状更新卡 prompt。改名评估结论（2026-07-23）：代码层面小活，真实成本在活着的状态（~/.claudego 在途数据、launchd label、远端足迹）与调用方；单点化/迁移/shim 是改名的组成部分而非可选优化，但在改名需求证实前不预做。

## 定位一句话

**ClaudeGo 是围绕订阅额度经济学设计的本地 coding-agent 编排器**：编排器本身纯本地零额度消耗，贵模型只出判断、便宜模型只出劳动，把每个 5 小时窗口榨干。

## 护城河（2025-2026 全生态查证，无成熟对应物）

1. **订阅限额感知调度**。生态里只有观测工具（CodexBar/ccusage/usage 端点），"观测→调度决策"这一层是空白：额度红线停发、跨 5h/7d 窗口排队、限额中断解析重置时间到点续跑、按 utilization 降级模型/切换执行器——查证的全部同类项目均不做。
2. **claude+codex 跨厂商双盲交叉验证**。没有任何一方或第三方做跨厂商 cross：各家只编排自家模型。A/B 独立作答（侧车隔离、引擎身份入队冻结、宁排队不降级偷换引擎）→ C 对抗合并，是独有机制。

配套差异化：类型化任务流水线（design-review/prompt-assembly/fix-cycle 模板链）、自管远程主机审核分流、跨厂商统一本地账本与审计。

## 设计哲学

四项非目标是同一个判断的四次应用：**任何借鉴都不能以削弱护城河、或以接管"一方正在做的事"为代价。抄机制，不抄载体；抄思想，不抄依赖。**

## 非目标（Non-goals）

### ① 不做 ACP 统一执行器层（不替换三套 CLI 包装）

**含义**：ACP（Zed 的 Agent Client Protocol）把"客户端↔coding agent 子进程"标准化为 JSON-RPC（统一 session/prompt 发任务、流式 session/update 收进度、权限回调）。做了它，ClaudeGo 变成 headless ACP 客户端，支持 ACP 的 agent 理论上即插即用。

**若做的影响**：短期收益真实（第四家执行器近乎零成本、结构化事件流替代 stdout 解析），但代价四层——
- runner/proc 层整体重写，而它恰是被实战打磨最狠的部分（限额措辞识别、孙进程吊管道两拍击杀、Windows 远端 shell 差异），重写等于重踩全部坑；
- Claude 的 ACP adapter 是 **Zed 维护的第三方翻译层，非 Anthropic 原生**——omnara 的死因就是"包一层第三方翻译层追 Claude Code 的频繁更新，不可维护"；
- **计费风险真实**：2026-06 Anthropic 曾计划把第三方 ACP 用量拆出订阅按 API 计价（宣布暂缓，未撤回）。ClaudeGo 的存在意义就是榨干订阅额度，ACP 路径若被划出订阅，工具的经济学崩塌；
- 规范由 Zed 单方演进，破坏性变更成本由我们承担。

**为何不做**：现有三套包装稳定、mock 集成测试覆盖全状态机、无已证痛点——为尚未出现的需求重写正在工作的核心，是教科书式预留式重构。
**再评估触发器**：Anthropic 官方 adapter / 规范进中立基金会 / 某执行器 headless CLI 破坏性变更迫使重写。

### ② 不做 git-backed 任务账本（Beads 式）

**含义**：Gas Town 把任务 DAG 与状态存进 git（Beads），每次状态变更即提交，git 天然是完整历史与一致性恢复点。做了它，`~/.claudego/tasks` 的 JSON 账本换成 git 仓库。

**若做的影响**：优点存在（任意崩溃点回滚、git log 即审计、多机同步天然），但状态机每个写入点重写 + 并发模型重设计——git 不擅长"tick/drain 每 15s 高频小写入 + 多任务并行"负载，index lock 争用会成为**新的**故障源；加数据迁移与回滚路径，L 级投入。而它能证明的两个需求——完整留痕、崩溃窄缝——CG-2（append-only 事件账本）与 CG-4（幂等墓碑）用**小一个量级的代价**分别覆盖。

**为何不做**：同样的已证需求存在便宜十倍的解法时选贵的，就是为"更漂亮的架构"付迁移税。只吸收思想（状态变更必须留痕），不搬载体。

### ③ 不给看板加写操作（board 永远只读）

**含义**：vibe-kanban 式看板是操作台（拖卡建 worktree 启动 agent、界面 review diff、开 PR）。做了它，board 从观察面变控制面。

**若做的影响**：三重代价——
- **与一方能力重复**：Claude Code agent view 已内建后台会话+worktree+自动 PR，且 `claude agents --json` 可脚本化；vibe-kanban 母公司关停侧证此定位的维护成本；
- **炸掉安全边界**：board 三纪律（只读、只听 127.0.0.1、TTL 缓存）意味着最坏后果不过"看到过期数据"；有写操作后 board 变成攻击面与误操作面，认证/CSRF/审计从零设计；
- **写路径分裂**：队列唯一写入者是 CLI+daemon 单写点，board 加写就是第二支笔——正是"每个账本只有一支笔"纪律要消灭的东西。

**为何不做**：收益（免开终端）远小于安全与一致性成本。远程操作的正确形态是 happy 式"遥控既有 CLI 会话+push 远程审批"（不破坏单写点），列为观察项待已证需求。

### ④ 不做同会话桥接 / mid-execution 实时注入（AgentBridge 式）

**含义**：让两个正在运行的交互式会话互相实时推消息（Codex 结论直推 Claude 活跃会话、review 意见执行中途注入）。诱惑具体：cross 三卡串行（小时级）似乎能变实时对话（分钟级）。

**若做的影响**：
- **地基是流沙**：依赖的 Claude Code Channels 是 research preview，无 Anthropic 承诺（社区原生支持请求被 stale bot 自动关闭）；配套 agent-quota-guard 连独立仓库都查不到——地基一撤全部报废，omnara 剧本重演；
- **权限哲学相悖**：AgentBridge 默认 `--dangerously-skip-permissions`/`--yolo` 不是草率，而是实时互推**结构性地要求**免确认（否则每条消息卡在权限提示）；引入它等于引入"必须裸奔才好用"的机制，与"权限默认不 skip+类型级白名单"直接冲突；
- **腐蚀最值钱的机制**：cross 的全部价值在 A/B 独立作答互不污染（侧车隔离、引擎身份冻结皆为此服务）；实时互推是"两个 agent 边干边互相说服"——盲点同化，交叉验证退化成回音室。**慢，在这里是特性不是缺陷。**

**为何不做**：定位不符（headless 队列 vs 交互 TUI）、依赖存疑、纪律相悖、反噬护城河。唯一硬通货——"turn 边界干净停 + 幂等墓碑 at-most-once 续接"——已被 CG-4 单独吸收。
