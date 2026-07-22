# ClaudeGo

**中文** | [English](README.en.md)

[![LINUX DO](https://img.shields.io/badge/LINUX%20DO-社区分享-ffb003?logo=discourse&logoColor=white)](https://linux.do)

围绕 Claude 5 小时用量限额设计的本地任务队列与调度器。单个 Go 二进制，无外部依赖。

核心思路：**编排器本身是纯本地代码，不消耗任何 Claude 额度**；只有任务真正执行时才调用 `claude -p`。撞到限额时任务自动暂停并记下重置时间，到点后用 `--resume` 接回同一个会话继续干活，把每个 5 小时窗口榨干。

```
                 ┌──────────────────────────────────────────────┐
   claudego add  │                  任务队列 (~/.claudego/tasks)  │
   assemble ────▶│  queued ──▶ running ──▶ done                 │
   review  plan  │               │  └──▶ failed（退避重试后）      │
   adopt  brief  │               ▼                              │
                 │         limit_paused ──(到达重置时间)──┐        │
                 └───────────────────────────────────┼────────┘
                                                     │
   launchd / daemon 每 5 分钟 tick ──▶ 派发规则选一个任务 ─┘
                                      │
                                      ▼
                    claude -p --model <模型> --resume <会话> ...
```

## 五种任务类型

| 类型 | 用途 | 默认权限 / 模型 |
|---|---|---|
| `design-review` | 设计审核 session：只读审查代码/架构，产出 P0/P1/P2 分级报告 | 只读工具 + git log/diff |
| `prompt-assembly` | prompt 装配 session：调研项目后把目标拆成 prompt 序列，**产出的任务自动入队** | 只读工具 |
| `sequence` | 预设 prompt 序列：多个步骤在同一个会话中依次执行（`--resume` 串联，上下文连续） | acceptEdits + 常用构建/测试命令 |
| `coordinate` | 分工协调 session：读**实时**队列快照 + 各会话进度报告，把目标拆成分工任务（含模型建议）自动入队 | 只读工具，默认 opus |
| `progress-pull` | 进度回收 session：`--resume` 某个会话，让它输出结构化进度报告并落盘 | 只读工具，默认 haiku |

任务可以链式衔接：`assemble`（装配）→ 产出 `sequence` 入队 → 执行完成 → `review_after` 自动入队一个 `design-review` 审查刚才的改动。

## 进度回收 → 分工协调 → 自动推进

多个会话并行干活时的编排闭环：

**桌面端也在管辖范围内**：Claude Code 桌面端与 CLI 共用 `~/.claude/projects` 会话存储和订阅额度，
所以桌面端里开的会话同样可以被列出、回收进度、`--resume` 接管。

```bash
# 0) 找会话：列出某项目最近的 claude 会话（桌面端 + CLI 同池），拿到会话 ID
claudego sessions -dir ~/Projects/myapp

# 1) 回收进度。交互式会话（含桌面端）：打印"整理进度"prompt，贴进去后报告自动写回 ~/.claudego/progress/
claudego brief -dir ~/Projects/myapp -title 鉴权重构
#    有会话 ID 的（队列任务 / sessions 列出的桌面会话）：入队 haiku 回收任务，全自动
claudego brief -id t0705-xxxx -auto
claudego brief -session <session-id> -dir ~/Projects/myapp -auto

# 2) 分工。协调任务运行时注入实时队列快照 + 全部进度报告，
#    产出：人话分工说明（每个任务做什么/建议模型/手动接管命令，留在 log 里）
#    + 分工任务自动入队（带 model 字段，被依赖的 priority 更高，可续跑的带 session_id）
claudego plan -dir ~/Projects/myapp "本周把上传模块收尾并补齐测试"

# 3) 自动推进：launchd/daemon 照常 tick，按模型建议逐个执行；随时查看与接管
claudego list                 # 看分工执行到哪了（标题列＝“标题 ▸ 最新进度”）
claudego log <协调任务ID>      # 看人话分工说明
claudego cmd <id>             # 想手动接管某任务：打印 claude 命令 + 当前步骤 prompt（先 hold）
claudego progress             # 进度一览（“现状”列看进展）；-show <KEY> 人读渲染、-in 手动导入
```

**看板即进度**：`claudego list` 的标题列显示每个任务的「标题 ▸ 最新进度」（优先取已回收进度报告的现状，没有则回落到最近一步输出的自动摘要）；`claudego progress` 列表带独立的「现状」列，`progress -show <KEY>` 改为人读渲染（目标/进行中/完成/剩余/阻塞/关键文件，几千字接力 prompt 默认折叠、`-full` 展开）——一眼读出进展，不再是静态标题。

**模型路由**：任务带 `model` 字段则以 `--model` 执行（订阅限额按模型加权，例行工作路由到
sonnet/haiku 能显著拉伸 5 小时窗口）。所有添加命令支持 `-model`，协调任务的分工输出里
按"机械→haiku / 常规实现→sonnet / 高风险→默认最强"自动建议，也可在 `type_defaults.*.model` 配默认值。
杠杆倒置原则：贵模型只做小 token 量的编排与仲裁（coordinate 默认 opus），便宜模型烧大 token 量的执行。

**设计期 profile（fable 出设计、opus 落地）**：设计质量为第一优先级的阶段，把设计三件套切到最强模型——
`type_defaults` 里 coordinate / design-review / prompt-assembly 的 model 设为 `"claude-fable-5"`，
协调模板会按"设计→fable、落地→opus、机械→sonnet、琐碎→haiku"给产出任务指派模型；
`model_weights` 默认已带 `"claude-fable-5": 10`（`fable` 同权重）。进入密集开发期后可把 design-review 回调到 opus 控制消耗。

**会话内再分层（子 agent）**：`sequence` 任务默认放行 Task 工具，配合用户级子 agent
（`~/.claude/agents/deep-reasoner.md` 绑 opus、`fast-worker.md` 绑 sonnet），执行会话可以把
疑难推理上交、机械劳动下放——跨会话按任务路由 + 会话内按环节路由，两层叠加。

### 文件化状态（fresh_steps）与人工把关（-hold）

推荐把项目状态放在**文件**里（state.md / TASKS.md 等），任务不依赖会话记忆：

- `add -fresh` 或 emit JSON 里 `"fresh_steps": true`：步骤间不 `--resume`，每步全新会话。
  协调模板已内建三段式规约：开工读状态文件 → 只做一个增量 → 收工更新状态文件与任务清单。
- 好处：永不撞会话上下文上限（"Prompt is too long" 类失败绝迹）、限额中断后直接重发本步开新会话
  （无需续跑提示）、codex 备用执行器可接管**任意一步**（不再限单步任务）、审计友好（状态变更全在 git 里）。
- `plan -hold` / `assemble -hold`：分工产出的任务先挂起（held），人工审完 `claudego release <id>` 放行——
  "拆分 → 把关 → 推进 → 审核 → 更新状态" 的完整循环。

### 审核分流（把只读审核负载摊到第二台机器）

实现在本机跑、对抗审核改到另一台 `remote_hosts` 主机上跑，平衡两侧模型额度（审核只读、可分流）：

- `add -review-host <主机>`：完成后自动派的对抗审核卡改在该远程主机（`remote_hosts` 的键）执行，
  修复链继承此声明——下一轮审核继续分流。**同步命令失败**时回退本机审核（闭环不断）；分流后
  远端审核执行本身失败则按普通任务失败处理（重试/退避），不额外拉回本机。
- `add -review-dir <镜像路径>`：审核卡在审核主机上的工作目录，与 `-review-host` 成对指定（渲染审核模板的目标目录）。
- `add -review-sync <命令>`：派审核卡前先在本机以 `sh -c` 跑此同步命令（如把改动 rsync 到审核主机，120s 超时）；
  退出码非 0 即回退本机审核。可单独使用（只同步、不分流）。**同步命令以实现卡 `dir` 为工作目录执行**，
  故命令里的相对路径（如 `rsync -a ./ hostb:/mirror/`）以实现卡目录为基准，而非 daemon 启动目录。
  同步命令须**前台执行完毕**（勿用 `&` 后台化），exit 0 即视为同步完成——后台化会让审核开跑时镜像尚未就绪。

### 交叉验证（fable 顶替：双引擎独立作答 + 对抗式交叉查漏）

设计档模型（fable）撞周限额时，用两个**不同**引擎对同一 fable 级任务（设计/审核/裁决/追认）各自独立作答，再让第二个引擎拿第一个的结论对抗式查漏——两份独立视角比一个更难被同一盲点带偏：

```bash
claudego cross -dir ~/Projects/myapp "某配置键缺省时的契约语义，请裁决"   # 用默认引擎对
claudego cross -profile my-pair -dir ~/Projects/myapp "..."             # 换成你在 cross_profiles 里自定义的对
claudego cross -list                                                     # 看可用引擎对
```

事件驱动三卡链，只需入队 A，B/C 自动衔接：

- **A**：引擎甲独立作答（第一性原理 + 对抗式自审），产出结论 A；
- **B**：A 完成后自动派出，引擎乙独立作答——**prompt 与 A 完全相同、不含 A 的结论**。甲结论落进 `~/.claudego/crosscheck/<链ID>.a` 隔离侧车（只由编排进程读写、`0600`、C 用完即删），既不进 B 的卡字段、也**不写进 A 或 B 的日志**（A 的结论日志被抹）；B 用的是与 A 卡 ID 无关的不透明链 ID，solo 模板还明令 B 不得读编排/状态目录。默认下 B 拿不到 A，是因为它**没被给、被明令别找**——**但这不是硬沙箱**：诚实说，B 卡里带着链 ID，而侧车路径由链 ID 确定性推导，所以那本质上是一个指向侧车的键；codex `--sandbox read-only` 又能读全盘，一个刻意搜索的执行器仍可能找到侧车。这里做到的是**被动暴露最小化 + 行为护栏**，真正的强隔离需限制执行器读权限（本工具不提供）；
- **C**：B 完成后自动派出，引擎乙从侧车取回 A、连同 B **对抗式交叉查漏**（谁遗漏了什么 / 分歧点裁断 / 仅一方发现的盲点），产出合并结论并落进度报告（`claudego progress -show <链ID>` 取回，交你综合定稿）。

**模型来源可切换** = `config.cross_profiles` 里的命名引擎对（`default_cross_profile` 定默认）。默认 `opus-codex` = 甲 `claude opus·max` + 乙 `codex·max`（乙的具体模型由你的 `codex_model` 决定；两侧都跑各自最高标准思考档）。每个引擎的 `kind` 可选 `claude` / `codex` / `remote-claude` / `remote-codex`：

```jsonc
"default_cross_profile": "opus-codex",
"cross_profiles": {
  "opus-codex": {
    "a": { "kind": "claude", "model": "claude-opus-4-8", "effort": "max", "label": "opus·max" },
    "b": { "kind": "codex",  "effort": "max", "label": "codex·max" }
  }
}
```

- 引擎的 `effort` 是 claude / codex 共用的思考等级（claude → `--effort`，codex → `model_reasoning_effort`，同名同序 `low<medium<high<xhigh<max`），任务级覆盖全局 `codex_reasoning`；
- `claude` 引擎要求指定 `model`、`codex` 引擎要求配好 `codex_bin` + `codex_model`（否则会跑成账号/CLI 默认模型、与 profile 宣称不符，命令直接报错，杜绝静默降级）；
- 交叉卡是**只读分析**（读契约/源码/改动、不写业务仓，codex 侧 `--sandbox read-only`）；`-dir` 不能是 claudego 数据根或其子目录；
- 甲乙必须**同执行位置**（都在本机，或都在同一台 `remote_hosts` 主机）——三卡共用一个工作目录，跨机引擎对会被 `cross` 直接拒绝；
- **护栏**：claude 冷却期即便开了 `codex_fallback`，claude 引擎的交叉卡也**绝不**被降级偷换成 codex，codex 钉定卡在 codex 不可用时也**绝不** fail-open 到 claude（否则甲乙同引擎、验证形同虚设）——引擎身份冻结，宁可排队等对应窗口。链任一步断裂会在母卡留痕（`list` 可见），不让单腿结果冒充终局。

**已知局限（诚实声明）**：①甲乙"不同引擎"是**文本级** best-effort 校验（比 kind + 模型名），拦不了模型别名指向同一模型——profile 是你自写，别名同引擎属配置责任；②入队时冻结的是引擎身份（kind/模型/思考档），**不冻结基建路径**（codex/ssh 可执行位置、远端 sandbox 等）——正常运行中改这些属基建变更、不算身份漂移；③三卡链事件驱动、非崩溃原子：进程恰在"标 done 与派后继卡之间"崩溃的单腿孤儿由每轮 `reconcile` 置 failed 兜底（可见），但"崩溃 + 人工 `clean` 归档母卡"叠加的极窄组合可能漏网。这些是罕见崩溃/配置边界，非正常路径缺陷。

### 存量角色会话的接管（此前手动维护的 审核/装配/执行 session）

一个项目文件夹里已经养了一批长驻角色会话时，按角色分流：

```bash
claudego sessions -dir ~/Projects/myapp        # 认领：按首条消息识别各角色会话，拿到 ID

# 有在途工作的（执行/细化 session）→ 先收进度，再决定续跑还是重开
claudego brief -session <ID> -auto             # 存量上下文提炼成进度报告（含 next_prompt）
claudego adopt <ID> -dir ~/Projects/myapp      # 没做完的直接接管续跑

# 角色会话本身 → 对应类型命令 + -session 挂载，新一轮工作续用老会话的积累
claudego review   -session <老审核会话ID> "审查本周改动"
claudego assemble -session <老装配会话ID> "下一个目标"
claudego add -type sequence -session <老执行会话ID> -file 下一批步骤.md

# 或者放弃挂载：把老会话里沉淀的角色要求改进 templates/*.md，以后每轮全新开（上下文更便宜）
```

注意：headless 续跑既有会话是**分叉**（fork 出新 session id，原桌面会话不受影响）；任务首轮跑完后，
后续轮次应挂任务里最新的 session_id（`claudego list -json` 可见），或直接对同一任务追加步骤。
长驻会话上下文会越滚越贵，一般建议：知识沉淀进模板/进度报告，执行用短会话。

## 派发规则（可在 config.json 调整）

1. **续跑优先**（`resume_first`）：被限额打断的任务先于新任务——先把没做完的做完；
2. **priority 大者优先**；
3. **类型顺序**（`type_order`）：默认 审核 > 序列 > 装配（审核便宜且能尽快给出反馈，装配会派生新工作放最后）；
4. 同级按先进先出。

限额是全局的：任何任务撞到限额，写入全局冷却（`cooldown.json`），期间不再派发任何任务、不浪费探测调用；冷却时间优先取错误信息里的重置时间戳，解析不到则回退 `limit_fallback_min` 分钟后重试。

## 快速开始

```bash
make build && make install     # 编译并装到 /opt/homebrew/bin
claudego init                  # 初始化 ~/.claudego（数据目录可用 CLAUDEGO_ROOT 覆盖）

# 1) 预设 prompt 序列：steps.md 里用单独一行 --- 分隔步骤
claudego add -title "重构鉴权" -dir ~/Projects/myapp -priority 5 -review-after -file steps.md

# 2) prompt 装配：让 Claude 先调研再自动生成任务序列并入队
claudego assemble -dir ~/Projects/myapp "给上传模块加断点续传，含测试"

# 3) 设计审核：只读审查
claudego review -dir ~/Projects/myapp "并发与错误处理"

# 4) 接管一个刚被限额打断的交互式会话（桌面端或 CLI；会话 id 用 claudego sessions 查）
claudego adopt <session-id> -dir ~/Projects/myapp

claudego run                   # 手动跑一轮验证
claudego install-launchd       # 安装后台调度：每 5 分钟 tick 一次，开机自启
claudego list                  # 看板；log <id> 看细节；doctor 自检
```

不想装 launchd 时可以直接 `claudego daemon` 前台常驻。

**跨平台**：核心是纯 Go，macOS / Linux / Windows 都能编译运行（`go build` 出对应平台二进制）。`install-launchd`（开机自启 + 每 5 分钟自动 tick）只对接 macOS 的 launchd；其他平台用 `claudego daemon` 前台常驻，或让系统定时器每 5 分钟拉一次 `claudego run`——Linux 用 systemd timer / cron，**Windows 用任务计划程序（Task Scheduler）**。单实例锁已跨平台（Windows 走 `OpenProcess` 探活），定时并发不会撞车。

## 5 小时额度红线（保底额度）

给突发/交互任务留余量：红线生效时队列停止派发（多步任务也会在步骤间让位），`-force` 可越线。两条独立通道，`claudego quota` 随时查看：

```jsonc
// ~/.claudego/config.json
"queue_budget_tokens": 2000000,  // ① 本地账本：滑动 5h 窗口内队列最多消耗的加权 token，0 关
"redline_percent": 85,           // ② 外部全局用量源：5h 窗口 usedPercent 达线即停，0 关
"usage_feed": "/Users/you/Library/Application Support/CodexBar/usage-history.jsonl",
"usage_feed_max_age_min": 90,    //    样本过期视为不可用→放行（fail-open）
"model_weights": {"default":1,"opus":5,"sonnet":1,"haiku":0.2}   // 账本的模型加权
```

- ①只统计 claudego 自己的调用（桌面端消耗不可见），语义是"队列预算上限"——保底 = 总额度 − 队列预算。先跑几天 `claudego quota` 看典型消耗再定值。
- ②是全局视角，样本格式兼容 CodexBar 的 usage-history.jsonl（需在 CodexBar 里开启 Claude 用量探测）；任何工具按同格式落一行 JSONL 都能接。
- 真正耗尽时仍有限额冷却兜底（解析重置时间、到点续跑），红线只是提前让路。

**分时段红线**（`redline_windows`）：时段内非零字段覆盖全局阈值，时段外回落全局；跨零点用 from > to。
`redline_lead_min` 给时段加前置缓冲：开始前 N 分钟就停发 claude 任务——单步任务起跑后无法中途让位，
不加缓冲的话踩线起跑的长任务会烧进预留窗口（codex 钉定任务不受影响）。时段 from 建议对齐配额窗口的真实重置时刻。
典型用法——交易早盘给交互留 25% 余量，其他时段队列用满：

```jsonc
"queue_budget_tokens": 0, "redline_percent": 0,   // 全局：不限
"redline_windows": [
  {"from": "06:50", "to": "11:50", "redline_percent": 75, "queue_budget_tokens": 300000}
]
```

## Codex 备用执行器（限额空窗不断档）

调度器本身是纯 Go，不耗额度，限额只会让任务等待、不会让系统瘫痪。但冷却期内没有执行力——
配置 codex CLI 后，claude 被冷却或红线拦住的时段，**单步且无既有 claude 会话**的任务
（协调 / 审核 / 装配 / 单步 add——正是维持管线运转的编排环节）自动切给 `codex exec` 执行：

```jsonc
"codex_bin": "/opt/homebrew/bin/codex",
"codex_fallback": true,
"codex_model": ""        // 可选，-m 透传
```

- 带 claude 会话的多步任务不切换（跨 CLI 无法延续上下文），等重置自动续跑；
- codex 走自己的额度：不记 claude 账本、其错误不写全局冷却、成功也不清冷却；
- 沙箱按类型收窄：只读类任务 `--sandbox read-only`，sequence 用 `workspace-write`；
- 看板与日志标注 `[codex]` / `runner=codex`，emit/进度解析管线照常工作（协调分工在冷却期也能继续入队）。

## 限额中断与自动恢复的细节

- 步骤执行中撞限额：任务标记 `limit_paused` 并记录 `mid_step`。到点续跑时不会重发原 prompt，而是向**同一个会话**发送续跑提示（`config.json` 的 `resume_prompt`），让 Claude 从中断处接着做，避免重复劳动。
- 每一步成功后立刻落盘（任务文件原子写入），进程被杀也不丢进度。
- 单实例锁（`.lock`）保证 launchd 的多次触发不会并发跑任务；持锁进程死掉会自动清锁。
- 其他错误（网络、超时等）按 `retry_backoff_min` 退避重试，超过 `max_attempts_per_step` 次标记失败，`claudego retry <id>` 可带着会话与进度重新入队。

## 事件账本（per-task `events.jsonl`）

看板"活动流"由每张卡的事件账本驱动，不再拿 `task.Status` 反推伪造历史（把
`queued→running→limit_paused→running→done` 压平成一句"当前 running"违反诚实性纪律）。

- 位置：活动卡在 `~/.claudego/events/<id>.jsonl`，随 `clean` 归档到 `archive/events/<id>.jsonl`；
- 每条事件是一行 JSON：`seq`（卡内单调递增）+ `ts`（RFC3339Nano）+ `type` + `actor`（谁触发）
  + `status`（迁移后的状态快照）+ `step` + `detail`（如恢复时间戳、错误摘要、下游派生卡 ID 等）；
- `type` 枚举与状态机迁移一一对应：`queued` / `dispatched` / `step_ok` / `limit_paused` / `held`
  / `retry` / `canceled` / `done` / `failed` / `closeout`（完成后派生的下游卡入队）；
- 写入是 `O_APPEND` + `fsync`：POSIX 保证追加是原子的（多进程 tick 与 CLI 同时改状态机也不会互写字节），
  `fsync` 保证宣称已写的事件必落盘。`kill -9` 留在末尾的半截 JSON 下轮追加时先补 `\n` 封成独立坏行，
  读者按行 `Unmarshal` 失败即丢弃，已落事件不受影响；
- **事件缺口不掩盖**：看板活动流按 `seq` 单调检测跳号，见跳号即插一条"事件缺口（缺失 seq X..Y）"
  显式披露；尾部残尾也披露一条"事件缺口（尾部残行或损坏行已丢弃）"。绝不用相邻事件补齐冒充完整历史。
- 首期不做签名（防篡改无已证需求，只取追加留痕语义）。

## 幂等墓碑（per-task `tombstones/<id>.json`）

事件账本记录"发生了什么"；墓碑账本记录"这个副作用是否已被注入过"——防止进程重启在同一个注入点上反复重发。

三处"至多一次注入"点由墓碑护栏包住：
1. **limit_paused 恢复**：任务撞用量限额挂起后再被 tick 复派，`runTask` 顶部若 `MidStep=true+SessionID` 非空，
   会向同一 claude 会话发 `resume_prompt`（"继续。上一条指令因为用量限额被中断…"）。若崩溃落在"提示已发送、
   状态未回写"处，重启后 tick 会再撞一次，把一次续跑放大成 N 次重发烧 token。
2. **mid_step 续跑**：与 1) 同一代码路径，区别在触发原因是崩溃中断而非限额。
3. **cross 链 reconcile**：`tick` 每轮扫孤儿 A/B 卡，判 `done+无后继` 就 `saveTask(failed) + emit failed`。
   崩溃落在两者之间会漏事件；两轮 tick 之间也可能重复裁决。

护栏语义（`tombstones.go`）：
- 每次注入前先写 `pending(attempt+1)`；注入成功后再落 `final`。
- Tick 复扫时见 `phase=final` 即跳过；见 `phase=pending` 且 `attempt≥bound` 也跳过（bound=2，允许崩一次+重试一次）。
- **反例注入**：损坏字节按无墓碑处理并 stderr 披露，不 crash 也不静默跳步——静默跳步会永远不再注入，卡死更隐蔽。
- **reset-at-entry**：`runTask` 顶部若上一轮盘上状态非 `running`（`queued/limit_paused/held`），则清当前步的
  resume 墓碑——这是"编排层认可的新一轮尝试"信号，让新一轮 bound=2 保护从零起算；若上一轮仍是 `running`，
  则保留墓碑挡住崩溃风暴。
- **随卡归档**：墓碑账本随 `archiveTask` 移到 `archive/tombstones/<id>.json`，审计可看"这张卡的每次注入都尝试了几次"。

数据模型（单 JSON 而非 JSONL）：`{"version": 1, "entries": {"resume:0": {kind, attempt, phase, nonce, ts}, "reconcile:cross": {...}}}`
——一次 `atomicWrite`（tmp→rename）即可原子替换整份账本，注入点数量少（步数上限），不需要行级追加语义。

## 权限与安全

任务默认**不**使用 `--dangerously-skip-permissions`：

- 审核/装配任务是只读工具白名单；
- `sequence` 默认 `acceptEdits` + 常用构建测试命令白名单，白名单外的 Bash 命令在无头模式下会被自动拒绝（Claude 会绕开或说明）。

需要完全自主时对单个任务加 `-skip-permissions`，或改 `config.json` 里对应类型的 `skip_permissions`。工具白名单在 `type_defaults.*.allowed_tools` 中按类型调整。

## 配置速查（~/.claudego/config.json）

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
| `type_defaults.*.model` | 协调 opus；回收 haiku | 各类型默认模型（--model 值），空用账号默认 |
| `no_fallback_models` | ["claude-fable-5","fable"] | 这些设计档模型冷却期不降级 codex，宁可排队等 claude |
| `thinking_tokens` | 0 | >0 时给 claude 调用设 MAX_THINKING_TOKENS（设计活加大思考预算） |
| `queue_budget_tokens` 等 | 0（关） | 5 小时额度红线，见上文专节 |
| `max_parallel` | 1 | 单次 tick 并行任务数（写类任务同目录串行；design-review/progress-pull 只读类型豁免，可同仓并发） |
| `codex_bin` / `codex_fallback` | 空 / false | 冷却期备用执行器，见上文专节 |
| `codex_reasoning` | "" | 全局 codex 推理档（minimal/low/medium/high/xhigh/max/ultra）→ `-c model_reasoning_effort=…`；任务级 effort 可覆盖 |
| `cross_profiles` | {opus-codex} | 交叉验证引擎对（`claudego cross`），见上文专节 |
| `default_cross_profile` | "opus-codex" | `cross` 未指定 `-profile` 时用的引擎对 |

提示词模板在 `~/.claudego/templates/*.md`，可直接修改（`{{GOAL}}` `{{DIR}}` `{{FOCUS}}` 会被替换；
`coordinate.md` 里的 `{{QUEUE}}` `{{PROGRESS}}` 在**派发时**替换为实时快照）。

## 测试

```bash
make test   # mock claude 跑完整状态机：调度/限额暂停/冷却/续跑/装配入队/失败退避/模型路由/进度回收/分工协调
```

## 致谢

本项目在 [LINUX DO](https://linux.do) 社区分享，感谢社区佬友的反馈。
