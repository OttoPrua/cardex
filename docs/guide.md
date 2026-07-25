# ClaudeGo 进阶指南

**中文** | [English](guide.en.md) · 返回 [README](../README.md)

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

**全局默认分流**（config 三件套，省去每张卡手动指定）：三键 `default_review_host` / `remote_mirror_root` / `default_review_sync` 齐备时，本地实现卡（`RemoteHost` 空）且未显式声明 `ReviewHost` 的 `review_after` 自动审核，一律分流到 `default_review_host`，审核目录自动推导为 `<remote_mirror_root>/<实现卡目录名>`，同步命令继承 `default_review_sync`。三键**缺任一则不套默认**；任务级 `-review-host` / `-review-dir` / `-review-sync` 显式值恒优先；远端实现卡（`RemoteHost` 非空）不受此默认影响（已在远端审）。

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
- 交叉卡是**只读分析**（读契约/源码/改动、不写业务仓）；**本机** codex 侧默认走一次性隔离副本 + `--sandbox workspace-write`（副本随卡即建即删,原仓永不受写污染,见"沙箱"段的 CG-R3 `codex_review_sandbox`）；**远端** codex 侧只在目录位于 `remote_mirror_root` 之下（sync-lane 分发的一次性镜像）时才放宽为 `workspace-write`,跑在真实业务仓（三卡共用工作目录时的常态）维持 `--sandbox read-only` 硬保证（CG-R3 R1 P0-1）；`-dir` 不能是 claudego 数据根或其子目录；
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

## Web 看板（board 命令）

实时只读 kanban，本机浏览器访问。

```bash
claudego board               # 默认 http://127.0.0.1:8787
claudego board -port 9000    # 自定义端口
claudego board -ttl 30       # 任务快照缓存秒数（默认 10）
```

三条纪律（不可破）：
- **只读**：所有接口仅经 `os.ReadFile` / `os.ReadDir` 读文件，绝不写任何任务状态，无 write endpoint。看板挂在生产队列数据上，误写会污染真实队列。
- **只听 127.0.0.1**：响应含 prompt 全文、目录路径、账号额度，不应出本机。`-addr` 可显式覆盖，但默认永远是回环，非回环地址会打印警告——不建议外放。
- **TTL 缓存**：任务快照与燃尽视图各有独立 TTL（燃尽 TTL = 任务快照 TTL × 3，至少 30s），防止每次请求全盘扫 tasks/ + transcript。`/api/*` 带 gzip 压缩（实测单次响应 2.5MB → 320KB）。

**项目覆盖块 `~/.claudego/board.json`**：项目/阶段自动推导的介绍难免干瘪，可人工写一份更准的；文件缺失就全部走自动推导。允许字段：`name` / `desc` / `phases.<name>` / `goal`。

**`goal` 字段（CG-8「落地进度」）**：项目"离目标多远"的机械化视图，与卡片进度**并列**呈现（不替换）。V1 只做合成，不做历史/趋势。

> ⚠️ **下面示例块里的 `//` 只是文档演示注释——实际 `board.json` 是严格 JSON，不接受注释、不接受尾逗号**。抄示例时请去掉这些 `//`。写坏了？看板顶端会挂出红色告警 `board.json 未生效`（`OverviewResp.board_override_error`）。分两种降级——都会披露，永不静默：**语法错**（漏逗号、抄了 jsonc 注释、括号不闭合）无法部分解析，整块 override 失效并回落自动推导；**字段类型手误**（如 `"weight":"1"`、`"done_percent":"50%"`）Unmarshal 会 skip 该字段并继续填充其它字段，因此**保留部分结果**（其它无手误的 name/desc/phases/goal 仍生效）+ 挂错披露——一处手误不连坐吃掉整个覆盖块，但降级本身必须挂告警。

```jsonc
"goal": {
  "statement": "落地实际使用",
  "as_of": "2026-07-23",              // 人工评估日期；驱动 goal_source=manual@as_of
  "milestones": [
    {"id":"M1","title":"设计收口","weight":1,"done_percent":100,"basis":"REVIEW Go"},
    {"id":"M4","title":"test-ready gates","weight":1,
     "evidence": {                     // 有 evidence 就用它覆盖人工 done_percent
       "path":"/Users/you/.claudego/logs/check.json", // **必须**绝对路径；相对路径直接判"数据不足"，不做 CWD/boardRoot 兜底
       "numerator":"gate_counts.pass",  // 点分路径，取到的必须是 JSON 数值
       "denominator":["gate_counts.pass","gate_counts.blocked"],
       "max_age_hours": 24              // 超龄→里程碑标 stale + 数据不足；负值判配置错误
     },
     "basis":"ops/test-ready/check"}
  ]
}
```

合成：`landed_percent = Σ(weight × done_percent) / Σweight`，与 `progress_percent` 并列展示，来源披露按**实际入账来源**打标（不按配置形态；下列任一非 `insufficient` 标签均可与 `partial=true` 共存——同类里程碑仅部分成功入账时以 `partial` 披露局部失效，标签本身**不承诺**该类全部入账）：`goal_source=manual@as_of`（至少一条 manual 入账，且未配置任何 evidence）/`evidence`（至少一条 evidence 入账，且无 manual 入账；evidence 集合部分失效时以 `partial` 披露）/`mixed@as_of`（manual 与 evidence 各至少一条入账）/`manual+degraded@as_of`（配了 evidence 但一条都没入账，靠 manual 撑住合成——**披露降级**，不虚报"混合来源"）/`insufficient`（无任何有效入账）。

**fail-honest 兜底**（不可绕）：
- goal 缺失 → 前端**完全不显示**该区块（不猜）；
- 权重和 ≤ 0 或任一 weight<0 → 整块标"数据不足"，`landed_percent` 为 `null`（不出 NaN/Inf/任何数字）；权重出现非有限数（`NaN`/`±Inf`，如 `MaxFloat64` 相乘溢出、`NaN` 权重绕过 `<0` 判断）→ 合成前 `math.IsNaN`/`IsInf` 守护同样整块"数据不足"（否则 `round1(Inf)` 转 int64 是"实现相关"，前端会渲染出 0% 或天文级负数——比缺数糟糕）；
- 人工 `done_percent` 越界（<0 或 >100）→ 里程碑判"数据不足"，不得直出负数百分数或 250%（教训：round1 的 int64 截断会把 -50 算成 -49.9，直渲成"-49.9%"是造读数）；
- evidence 折算结果超 100%（如 pointer 配错让 `num=30, den=[10]` 算出 300%）或折算为负（分子/分母有一个是负）→ 同样"数据不足"，不直出 300% 或 -30%；防线放在 `num` 与 `den` 各自的绝对值上——`num<0` 拒、`den` **分量级 `v<0` 拒**（`{pass:5, blocked:10, adjustment:-3}` 求和为 12>0 但 adjustment 分量为负，若只挡求和会零告警渗出 41.7%）、`den` 求和 `≤0` 拒（除零守护 + 全零分量兜底）；只挡 `pct<0` 会被"双负相消"绕过（如 `{pass:-9, blocked:-2}` 会算出 +81.8%）；
- **evidence.path 强制绝对路径**：相对路径无论解析到进程 CWD 还是 `board.json` 所在目录，都存在"同名文件静默兜底"的兜底路径（`~/.claudego` 里常有同名脚手架/临时文件），配错时零告警读错——直接判"数据不足"是唯一让读数出处可追溯的做法；
- evidence 文件缺失 / 超 `max_age_hours` / pointer 取不到 **JSON 数值类型**（如字段是字符串 `"9/21"`）→ 该里程碑标"数据不足"，合成值仅基于可用里程碑并标 `partial`——**evidence 存在即独占**，失败/超龄一律"数据不足"，**绝不回退到人工值**（读数含义漂移就是造假）；
- `board.json` 存在但 JSON 语法非法（含 jsonc 注释、尾逗号、括号不闭合等）→ `OverviewResp.board_override_error` 挂出错误，前端顶部红色告警常驻，**整个 override 块**全部失效并回落自动推导；**字段类型手误**（`"weight":"1"`、`"done_percent":"50%"` 等 `*json.UnmarshalTypeError`）→ 同样挂 `board_override_error` 披露，但 **保留 Unmarshal 已尽力填充的其它字段**（其它无手误项目的 name/desc/phases/goal 仍生效）——一处手误不连坐吃掉整个覆盖块。两种降级都**不静默吞**。

**看板只读 evidence 文件、绝不执行命令**：落盘取数由编排 session / 卡（如 `ops/test-ready/check`）负责，看板只读它们的产出。

**燃尽视图三源**（`/api/burn`）：三源中 `usage-history.jsonl`（即 `usage_feed`）与 `claudego quota` 共用同源；`claude.json` 与 transcript 扫描为 board 独有读数，`claudego quota` 不读这两源：
1. **CodexBar `claude.json`**：claude 侧各账号 session / weekly / opus 窗口的百分比时间序列；
2. **CodexBar `usage-history.jsonl`**（= `usage_feed`）：codex 侧 primary（5h）/ secondary（周）百分比时间序列；
3. **`~/.claude/projects/*/*.jsonl` transcript**：每条 assistant 消息的绝对 token 用量（四类等权相加，附额度口径折算）。

**"数据不足"语义**：只有一个样本点时没有速率可算；样本比它所描述的窗口还老（如 5h 窗口配一条 14 小时前的样本）；或重置时刻已过——三种情况一律 `verdict="数据不足"`，`burn_rate` / `exhaust_at` 保持 null，绝不编造估算值。只在当前窗口周期内（与最新样本同属同一 resetsAt 边界，容差 90s）的点才参与速率拟合。

## 5 小时额度红线（保底额度）

给突发/交互任务留余量：红线生效时队列停止派发（多步任务也会在步骤间让位），`-force` 可越线。三条通道，`claudego quota` 随时查看：

```jsonc
// ~/.claudego/config.json
"queue_budget_tokens": 2000000,  // ① 本地账本：滑动 5h 窗口内队列最多消耗的加权 token，0 关
"redline_percent": 85,           // ②③ 百分比通道共享红线：任一源 usedPercent 达线即停，0 关
"usage_feed": "/Users/you/Library/Application Support/CodexBar/usage-history.jsonl",
"usage_feed_max_age_min": 90,    // ②   样本过期视为不可用→放行（fail-open）；**0/负值归位默认 90 min，不接受"永远采信"**
"oauth_usage": true,             // ③   订阅端点直读（第三用量源），默认关；端点未文档化
"oauth_usage_max_age_min": 15,   // ③   兼作**端点结果的进程级缓存 TTL**：tick 15s 循环内复用不重打；0/负值归位默认 15 min
"oauth_usage_timeout_sec": 6,    // ③   HTTP 超时（秒）
"oauth_usage_creds_path": "",    // ③   显式指定即**硬隔离**——只信该文件,不再兜底 ~/.claude/keychain（测试/自定义部署用；空=按默认顺序找）
"model_weights": {"default":1,"opus":5,"sonnet":1,"haiku":0.2}   // 账本的模型加权
```

- ① 只统计 claudego 自己的调用（桌面端消耗不可见），语义是"队列预算上限"——保底 = 总额度 − 队列预算。先跑几天 `claudego quota` 看典型消耗再定值。
- ② 是全局视角，样本格式兼容 CodexBar 的 usage-history.jsonl（需在 CodexBar 里开启 Claude 用量探测）；任何工具按同格式落一行 JSONL 都能接。
- ③ 直读 `api.anthropic.com/api/oauth/usage`（`anthropic-beta: oauth-2025-04-20` 头 + 复用 `~/.claude/.credentials.json` 或 macOS keychain 的 OAuth accessToken），取 5h 窗口 utilization。**端点未文档化、可随时变更**——任何异常（网络/凭据/HTTP 4xx-5xx/字段缺失/字段值歧义/格式漂移）一律按"数据不足"→ fail-open 放行；实现只信响应 body，绝不解析响应头（响应头容易被中间层伪造/覆盖，且核验已推翻"响应头带 unified 限流数值"之说）。`utilization` 实测 0-100 百分域原样取整（实样：端点回 31.0 即 31%，与 `limits[].percent=31` 互证）、`used_percent/percent` 同为 0-100 域原样取，**任一自动归一都是假触线温床**（教训：老版 `utilization:1`→100%、`used_percent:0.8`→80% 都能锁死队列）；`utilization` 落在 (0,1] 区间判为刻度歧义（旧分数写法 vs 新百分写法两判均可能错）拒判为数据不足，>100 域外同拒。`oauth_usage_creds_path` 非空即**硬隔离**——只读该文件，不再兜底 `~/.claude`/keychain（避免 Windows `UserHomeDir` 兜底摸真实凭据造成测试隔离失效或权限漂移）。端点结果自带**进程级缓存**（TTL=`oauth_usage_max_age_min` 或默认 15 min），tick 每 15s 重扫不会每次都打端点、也不会重复触发 macOS keychain 弹窗；缓存过期后重抓失败会保留旧样本并披露"已过期+重抓失败"两要素，让 `quota` 能诚实展示。
- ②③ 合并规则=**最保守值优先**（可用样本里 percent 最大者判线）——观测口径不一致时,最坏假设兜住,而不是投票或平均。`claudego quota` 会并列展示三源读数并在分歧 ≥5% 时明确披露区间。
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
- 沙箱按类型收窄：`sequence` 落码卡走 `--sandbox workspace-write`；只读类任务(design-review/crosscheck/coordinate/progress-pull)默认建**一次性隔离副本 + `--sandbox workspace-write`**(CG-R3 承 BD-36 工具链③终裁 b, BD-39 附记 2026-07-24)——副本落 `<root>/tmp/codex-review-work/<taskID>-<pid>-<nano>/`,承载 dirty+untracked 面,复审可跑测试/写夹具做动态验证;卡结束即删,崩溃残留由 tick 对账清(pid 死透 + taskID 不在 activeIDs 双条件);原仓永不受写污染(硬语义)。建副本阶段(探测/clone/apply/拷贝)跑在 `min(step_timeout, 10min)` 的独立子预算内(CG-R3b):git 子进程超时即整组击杀,拷贝腿**每个文件边界**查一次子预算、到期即止,且**从不打开非常规文件**(FIFO/socket/设备跳过;symlink 按链接本体复制、不跟随——否则一条指向无写端管道的 untracked 链接就能让 `open` 永久阻塞,把整条泳道占死且任何取消都解不开)。任一腿卡住都回落 `read-only` 继续跑,事件账本落 `codex_review_prepare_timeout` 留痕——降级而非把整条泳道堵死。`config.json` 加 `"codex_review_sandbox": "readonly"` 可回落旧的只读行为(退失去动态验证力)。远端复审同治:远端镜像本身已是影本,默认也放宽到 `workspace-write`。
- 看板与日志标注 `[codex]` / `runner=codex`，emit/进度解析管线照常工作（协调分工在冷却期也能继续入队）。

**降级专用模型与档位对等规则（`codex_fallback_model`）**：`codex_fallback` 生效时若 claude 卡被改道 codex，优先用 `codex_fallback_model`，而非全局 `codex_model`。档位对等映射：**opus 档降级首选同档的 terra（o3），不降设计档的 sol（GPT-5）**——设计档不去干实现档的活。空值回退 `codex_model`；此键仅对降级径（任务 `runner_pref≠codex` 且非远端）生效，codex 主跑卡与远端 codex 不受影响。

**钉定卡绝不 fail-open**：`no_fallback_models`（默认 `["claude-fable-5","fable"]`）列表中的模型在 claude 冷却/红线期**不降级 codex——宁可排队等 claude 额度恢复**。设计档质量优先；降级会破坏交叉验证的引擎独立性（钉定 `codex` 的交叉卡在 codex 不可用时同样绝不 fail-open 到 claude）。
