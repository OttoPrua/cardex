# cardex 进阶指南

**中文** | [English](guide.en.md) · 返回 [README](../README.md)

## 进度回收 → 分工协调 → 自动推进

多个会话并行干活时的编排闭环：

**桌面端也在管辖范围内**：Claude Code 桌面端与 CLI 共用 `~/.claude/projects` 会话存储和订阅额度，
所以桌面端里开的会话同样可以被列出、回收进度、`--resume` 接管。

```bash
# 0) 找会话：列出某项目最近的 claude 会话（桌面端 + CLI 同池），拿到会话 ID
cardex sessions -dir ~/Projects/myapp

# 1) 回收进度。交互式会话（含桌面端）：打印"整理进度"prompt，贴进去后报告自动写回 ~/.cardex/progress/
cardex brief -dir ~/Projects/myapp -title 鉴权重构
#    有会话 ID 的（队列任务 / sessions 列出的桌面会话）：入队 haiku 回收任务，全自动
cardex brief -id t0705-xxxx -auto
cardex brief -session <session-id> -dir ~/Projects/myapp -auto

# 2) 分工。协调任务运行时注入实时队列快照 + 全部进度报告，
#    产出：人话分工说明（每个任务做什么/建议模型/手动接管命令，留在 log 里）
#    + 分工任务自动入队（带 model 字段，被依赖的 priority 更高，可续跑的带 session_id）
cardex plan -dir ~/Projects/myapp "本周把上传模块收尾并补齐测试"

# 3) 自动推进：launchd/daemon 照常 tick，按模型建议逐个执行；随时查看与接管
cardex list                 # 看分工执行到哪了（标题列＝“标题 ▸ 最新进度”）
cardex log <协调任务ID>      # 看人话分工说明
cardex cmd <id>             # 想手动接管某任务：打印 claude 命令 + 当前步骤 prompt（先 hold）
cardex progress             # 进度一览（“现状”列看进展）；-show <KEY> 人读渲染、-in 手动导入
```

**看板即进度**：`cardex list` 的标题列显示每个任务的「标题 ▸ 最新进度」（优先取已回收进度报告的现状，没有则回落到最近一步输出的自动摘要）；`cardex progress` 列表带独立的「现状」列，`progress -show <KEY>` 改为人读渲染（目标/进行中/完成/剩余/阻塞/关键文件，几千字接力 prompt 默认折叠、`-full` 展开）——一眼读出进展，不再是静态标题。

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
- `plan -hold` / `assemble -hold`：分工产出的任务先挂起（held），人工审完 `cardex release <id>` 放行——
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
cardex cross -dir ~/Projects/myapp "某配置键缺省时的契约语义，请裁决"   # 用默认引擎对
cardex cross -profile my-pair -dir ~/Projects/myapp "..."             # 换成你在 cross_profiles 里自定义的对
cardex cross -list                                                     # 看可用引擎对
```

事件驱动三卡链，只需入队 A，B/C 自动衔接：

- **A**：引擎甲独立作答（第一性原理 + 对抗式自审），产出结论 A；
- **B**：A 完成后自动派出，引擎乙独立作答——**prompt 与 A 完全相同、不含 A 的结论**。甲结论落进 `~/.cardex/crosscheck/<链ID>.a` 隔离侧车（只由编排进程读写、`0600`、C 用完即删），既不进 B 的卡字段、也**不写进 A 或 B 的日志**（A 的结论日志被抹）；B 用的是与 A 卡 ID 无关的不透明链 ID，solo 模板还明令 B 不得读编排/状态目录。默认下 B 拿不到 A，是因为它**没被给、被明令别找**——**但这不是硬沙箱**：诚实说，B 卡里带着链 ID，而侧车路径由链 ID 确定性推导，所以那本质上是一个指向侧车的键；codex `--sandbox read-only` 又能读全盘，一个刻意搜索的执行器仍可能找到侧车。这里做到的是**被动暴露最小化 + 行为护栏**，真正的强隔离需限制执行器读权限（本工具不提供）；
- **C**：B 完成后自动派出，引擎乙从侧车取回 A、连同 B **对抗式交叉查漏**（谁遗漏了什么 / 分歧点裁断 / 仅一方发现的盲点），产出合并结论并落进度报告（`cardex progress -show <链ID>` 取回，交你综合定稿）。

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
- 交叉卡是**只读分析**（读契约/源码/改动、不写业务仓）；**本机** codex 侧默认走一次性隔离副本 + `--sandbox workspace-write`（副本随卡即建即删,原仓永不受写污染,见"沙箱"段的 CG-R3 `codex_review_sandbox`）；**远端** codex 侧只在目录位于 `remote_mirror_root` 之下（sync-lane 分发的一次性镜像）时才放宽为 `workspace-write`,跑在真实业务仓（三卡共用工作目录时的常态）维持 `--sandbox read-only` 硬保证（CG-R3 R1 P0-1）；`-dir` 不能是 cardex 数据根或其子目录；
- 甲乙必须**同执行位置**（都在本机，或都在同一台 `remote_hosts` 主机）——三卡共用一个工作目录，跨机引擎对会被 `cross` 直接拒绝；
- **护栏**：claude 冷却期即便开了 `codex_fallback`，claude 引擎的交叉卡也**绝不**被降级偷换成 codex，codex 钉定卡在 codex 不可用时也**绝不** fail-open 到 claude（否则甲乙同引擎、验证形同虚设）——引擎身份冻结，宁可排队等对应窗口。链任一步断裂会在母卡留痕（`list` 可见），不让单腿结果冒充终局。

**已知局限（诚实声明）**：①甲乙"不同引擎"是**文本级** best-effort 校验（比 kind + 模型名），拦不了模型别名指向同一模型——profile 是你自写，别名同引擎属配置责任；②入队时冻结的是引擎身份（kind/模型/思考档），**不冻结基建路径**（codex/ssh 可执行位置、远端 sandbox 等）——正常运行中改这些属基建变更、不算身份漂移；③三卡链事件驱动、非崩溃原子：进程恰在"标 done 与派后继卡之间"崩溃的单腿孤儿由每轮 `reconcile` 置 failed 兜底（可见），但"崩溃 + 人工 `clean` 归档母卡"叠加的极窄组合可能漏网。这些是罕见崩溃/配置边界，非正常路径缺陷。

### 存量角色会话的接管（此前手动维护的 审核/装配/执行 session）

一个项目文件夹里已经养了一批长驻角色会话时，按角色分流：

```bash
cardex sessions -dir ~/Projects/myapp        # 认领：按首条消息识别各角色会话，拿到 ID

# 有在途工作的（执行/细化 session）→ 先收进度，再决定续跑还是重开
cardex brief -session <ID> -auto             # 存量上下文提炼成进度报告（含 next_prompt）
cardex adopt <ID> -dir ~/Projects/myapp      # 没做完的直接接管续跑

# 角色会话本身 → 对应类型命令 + -session 挂载，新一轮工作续用老会话的积累
cardex review   -session <老审核会话ID> "审查本周改动"
cardex assemble -session <老装配会话ID> "下一个目标"
cardex add -type sequence -session <老执行会话ID> -file 下一批步骤.md

# 或者放弃挂载：把老会话里沉淀的角色要求改进 templates/*.md，以后每轮全新开（上下文更便宜）
```

注意：headless 续跑既有会话是**分叉**（fork 出新 session id，原桌面会话不受影响）；任务首轮跑完后，
后续轮次应挂任务里最新的 session_id（`cardex list -json` 可见），或直接对同一任务追加步骤。
长驻会话上下文会越滚越贵，一般建议：知识沉淀进模板/进度报告，执行用短会话。

## Web 看板（board 命令）

实时只读 kanban，本机浏览器访问。

```bash
cardex board               # 默认 http://127.0.0.1:8787
cardex board -port 9000    # 自定义端口
cardex board -ttl 30       # 任务快照缓存秒数（默认 10）
```

三条纪律（不可破）：
- **队列数据只读**：所有接口读 `~/.cardex` 仅经 `os.ReadFile` / `os.ReadDir`，`tasks/` / `archive/` / `events/` / 任务 JSON 一个字节都不写，绝不改任何任务状态。看板挂在生产队列数据上，误写会污染真实队列。唯一的写入是**看板自己的视图状态**：`POST /api/project/archive` 写 `~/.cardex/board_archive.json`（项目折叠状态，见下文「项目归档」）——它不参与调度、runner/tick/patrol 一概不读、删掉也不丢任何队列数据，所有 GET 路径仍是零写入。
- **只听 127.0.0.1**：响应含 prompt 全文、目录路径、账号额度，不应出本机。`-addr` 可显式覆盖，但默认永远是回环，非回环地址会打印警告——不建议外放。
- **TTL 缓存**：任务快照与燃尽视图各有独立 TTL（燃尽 TTL = 任务快照 TTL × 3，至少 30s），防止每次请求全盘扫 tasks/ + transcript。`/api/*` 带 gzip 压缩（实测单次响应 2.5MB → 320KB）。

**总览是横向轨道**：一个项目一列，左右滚动切项目，纵向空间全部留给单列内的阶段/任务清单（列内独立滚动，列高按轨道实际位置算，不出双滚动条）。项目之间是并列关系而非先后关系，纵向堆叠会让第二个项目被第一个项目的几百张卡顶出屏幕。窄屏（≤720px）自动退回纵向堆叠。页面**宽度跟随视口**，不设固定上限——超宽屏上每多一点宽度就多露一点下一个项目，把宽度让给留白等于白白少看一列。

**项目次序可拖拽**：列头左侧的手柄可拖动调整项目次序（也可聚焦后按 ←/→ 移动，拖放对键盘用户完全不可用，所以这不是可选补充）。次序存本浏览器的 localStorage——这是**观看偏好**而非队列事实，不同机器盯的重点不一样，没道理互相覆盖（归档走服务端是因为那是"这个项目还看不看"的判断，性质不同）。人工次序生效时页头常驻披露条并可一键恢复默认：默认排序本身带信息量（有活儿的排前面·其次按最近活动），盖掉了就得说。排序之后新出现的项目**排到最前**而不是最后——轨道是横向滚动的，末尾意味着要横滚几屏才看得见，而新冒出来的项目恰恰最该被看见；披露条会写明有几个是新的。

**状态呈现次序**：进度条分段、状态图例、页头状态芯片统一用 `已取消 → 已完成 │ 进行中 → 排队中 → 限额暂停 → 已挂起 → 失败`——已尘埃落定的在左，越往右越需要人管。进度条是一条**填充计**（像电量条），左边那截就是"已经不用再管的部分"，"未完成的都堆在右侧"这个直觉才成立；右半段内部按「离完成还有多远」递增，前三档机器自己会往前走，后两档必须人介入，右端因此天然是"该看这里"。三处同序是因为它们是同一份读数的三种呈现，各排各的会让芯片与它下面那条彩带对不上号。**kanban 的列序不跟着变**（那个在后端 `boardColumnOrder`）：看板是工作流看板，卡从左往右流向"已完成"是通用惯例，把 done 挪到最左会把它读成起点——填充计与流程板是两套隐喻，各守各的惯例。

**状态筛选（只藏清单，不动读数）**：页头那排状态计数芯片同时是开关，点一下把该状态的卡片清单折叠掉（总览折叠任务行，项目页直接整列消失——kanban 的列就是状态，列还在却空着会被读成"这个状态一张卡都没有"）。

纪律只有一条但不可破：**筛选绝不改任何读数**。芯片上的数字、进度条、五个分类桶、ETA 一律按全部卡算，哪怕该状态一张都没铺出来。藏掉「已完成」之后进度条跟着掉下去的话，那不是筛选，是伪造快照——用户会拿着一个自己无意中造出来的数字去做决定。生效期间页头常驻一条披露横幅写明藏了什么、并重申计数未受影响；「后端只发了 40 条」与「我自己把某些状态藏了」两条提示分开写，合成一句会让人以为看板丢了数据。筛选状态存 localStorage（一屏上千张卡，藏「已完成」是常态操作，每次刷新重设一遍没人会用），代价由那条常驻横幅兜住。

**额度显示口径 = 剩余**：顶部额度条与燃尽页的主读数一律是**剩余额度**（`BurnSource.remaining_percent`，后端算并钳在 [0,100]），燃尽曲线也是往下走、触底即耗尽。源数据（CodexBar）给的是已用 %，`used_percent` 原样保留在响应里、并在悬停/副标题/样本表里同时展示——两个口径同屏时永远标明是哪一个。用户在这一屏做的决定（还能不能再派一批卡）是"还剩多少"的直接函数，"已经烧了多少"要先在脑子里做一次减法。

**进度双口径（顶栏「卡/~」切换，委托人 2026-08-02 追加）**：项目总进度默认按**现有卡**计
分母（12 卡完成 9 → 75%），但 cardex 的工作模式会持续派生新卡（review_after 复审、修复轮、
emit 产出），现有口径常高估完成度。顶栏切换到**含预估余量**口径后，分母换成预估最终总卡数
（进度条尾部补一段斜纹「预估余量」幽灵段，百分比带 `~` 后缀），预估来源两级：
- **计划锚点优先**：`board.json` 的 `projects.<id>.planned_total_cards`（阶段性计划总卡量）。
  计划达成/调整时人工更新它——这就是预估口径的校准 hook；被现存量超出时按现存计并提示更新。
- **历史膨胀率替补**：无锚点时按本项目全史「卡数/根卡数」系数 × 未完结根卡数推算余量
  （根卡 = 非复审/非修复轮/非交叉派生腿）。每次快照按最新历史**即时重算（自校准）**，
  不需要定时任务；系数含在途链故偏保守，且只估现有工作的衍生量、不预测新立项。
  样本不足（根卡 <5 或衍生卡 <3）时显式回落现存卡数并说明——预估必须带 basis，绝不给
  来历不明的百分比。口径只切**项目总条**；kind 分桶与阶段条保持现有卡口径（预估只做到
  项目粒度，往下拆是把粗估伪装成细账）。切换偏好存 localStorage。

**项目覆盖块 `~/.cardex/board.json`**：项目/阶段自动推导的介绍难免干瘪，可人工写一份更准的；文件缺失就全部走自动推导。项目块内允许字段：`name` / `desc` / `phases.<name>` / `goal` / `kind_rules` / `planned_total_cards`；文件顶层还有一张 `project_aliases` 归组规则表（见下节）。

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
       "path":"/Users/you/.cardex/logs/check.json", // **必须**绝对路径；相对路径直接判"数据不足"，不做 CWD/boardRoot 兜底
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
- **evidence.path 强制绝对路径**：相对路径无论解析到进程 CWD 还是 `board.json` 所在目录，都存在"同名文件静默兜底"的兜底路径（`~/.cardex` 里常有同名脚手架/临时文件），配错时零告警读错——直接判"数据不足"是唯一让读数出处可追溯的做法；
- evidence 文件缺失 / 超 `max_age_hours` / pointer 取不到 **JSON 数值类型**（如字段是字符串 `"9/21"`）→ 该里程碑标"数据不足"，合成值仅基于可用里程碑并标 `partial`——**evidence 存在即独占**，失败/超龄一律"数据不足"，**绝不回退到人工值**（读数含义漂移就是造假）；
- `board.json` 存在但 JSON 语法非法（含 jsonc 注释、尾逗号、括号不闭合等）→ `OverviewResp.board_override_error` 挂出错误，前端顶部红色告警常驻，**整个 override 块**全部失效并回落自动推导；**字段类型手误**（`"weight":"1"`、`"done_percent":"50%"` 等 `*json.UnmarshalTypeError`）→ 同样挂 `board_override_error` 披露，但 **保留 Unmarshal 已尽力填充的其它字段**（其它无手误项目的 name/desc/phases/goal 仍生效）——一处手误不连坐吃掉整个覆盖块。两种降级都**不静默吞**。

**看板只读 evidence 文件、绝不执行命令**：落盘取数由编排 session / 卡（如 `ops/test-ready/check`）负责，看板只读它们的产出。

### 项目归属：显式 > 别名 > 模式 > 启发式 > 未分类

任务卡本来**没有**项目字段，看板靠工作目录反推项目。反推对"一个项目一个稳定目录"成立；一旦大量卡跑在**任务级临时目录**上（远端 `D:/Project/PO-tasks/<taskid>` 一卡一目录、日期工作树 `Trading-<slug>-20260730`、复审散目录 `HB-*`/`S3-*`/`card-*`），每个目录都会自成一个"项目"——实测一次盘点渲染出 **80 个项目，真实的只有 9 个**，"结论按项目可寻"就此失效。

归属判定分五层，**上层命中即停**：

| 层 | 判据 | 判定来源 | 写在哪 |
|---|---|---|---|
| 1 | `add -project <名>` 显式钉定 | `explicit` | 卡上（入队即钉） |
| 2 | `board.json` 顶层 `project_aliases` 首条命中 | `alias` | 配置（改一次全量追溯生效） |
| 3 | 目录（或其任一祖先）basename 以「已知项目名 + `-`」开头 | `pattern` | 代码内建 |
| 4 | 工作目录并查集分量（镜像对 / 车道 / 同名 / 祖先包含） | `heuristic` | 代码内建 |
| 5 | 都不中 | `unclassified` | 「未分类」收件箱 |

**`add -project`（入队即钉）**：派卡人知道这卡属于哪个项目，而目录只是它当下碰巧落脚的地方。字段落在卡面上，之后改配置、改别名表都不会让它漂移（与 `-stakes` 同一纪律）。派生卡——审核卡、修复卡、收口卡、超轮限升级卡、交叉链 B/C、协调 emit 出来的子卡——**自动继承**；不继承的话，一张钉了 `-project` 的实现卡派出的审核卡（跑在审核主机的镜像目录上）会掉进收件箱。

**`project_aliases`（存量整理机制）**：有序规则表，首条命中即用。改这张表**一个字节的任务卡都不动**，看板下次重建快照即全量追溯生效——这就是整理存量野项目的手段。

```jsonc
"project_aliases": [
  {"match": "/Users/you/Projects/PH-lanes/*", "project": "PerlicaHermes"},
  {"match": "D:/Project/PO-tasks/*", "title": "Hermes", "project": "PerlicaHermes"},
  {"match": "D:/tmp/qmt-*", "project": "Trading"},
  {"match": "D:/Project/Trading-docs", "project": "Trading-docs"},  // 别名 > 模式：挡住 "Trading-" 模式把它并进 Trading
  {"match": "/Users/you/Projects", "project": "未分类"}              // 容器目录自身丢进收件箱
]
```

- `match` **不含通配符 = 精确匹配该目录本身**；含 `*` `?` `[` 时按 glob 匹配「该目录或它的任一祖先」，故 `X/*` 覆盖 X 下**任意深度**。一律大小写不敏感（同一个远端目录在两台机器的卡上大小写常不一致）。
- **为什么裸路径不做前缀语义**：给容器目录（如 `~/Projects`）写一条前缀规则，会把它下面所有项目一次性吞进同一个项目，且账面完全看不出来。两种误用代价不对称——漏配只是几个目录留在收件箱（可见可改），过配是整块看板塌成一个项目。要覆盖子树请显式写 `/*`。
- `title` 是标题子串（大小写不敏感）。远端任务级目录名是随机任务 ID，**目录本身不含任何项目信息**，只能靠标题判归属。与 `match` 同写 = 必须**同时**命中（AND），防止标题规则泄漏到全盘。
- 坏规则（缺 `project`、`match` 与 `title` 都缺、glob 语法非法）**逐条跳过并披露**（`OverviewResp.project_alias_error`，看板总览顶部渲染成黄色告警），不连坐整表——与 `kind_rules` 同一纪律。

**内建模式规则（第 3 层）**：目录或其祖先的 basename 以「已知项目名 + `-`」开头即归该项目，专治日期工作树野化：`Trading-paper-strategy-envelope-20260730` → `Trading`、`PerlicaHermes-cmp-sol` → `PerlicaHermes`、`Trading-strategy-research-20260726/c-etf-regime` → `Trading`（证据在祖先上）。「已知项目名」= **当批任务里已经站住脚的项目代表名** + 别名表登记的名字 + 卡上显式钉的名字；通用目录名（`docs`/`src`/`config` 等）排除在外。同时有多个已知名可匹配时取**最长**的那个（`Trading-docs-mirror` 归 `Trading-docs` 而不是 `Trading`）。

**等值优先于前缀**：basename **恰好等于**某个已知项目名时，等值先判，不会再被更短的已知名以前缀吞掉。
- 祖先等值 → 直接归那个名字：`~/Projects/Trading-docs/notes` 归 `Trading-docs`（不是 `Trading`）。
- 目录**自身**等值且该名字被**声明**过（登记在 `project_aliases`，或有卡用 `-project` 钉过），或它有**跨根同名证据**（如本机 `~/Projects/Trading-docs` 与远端 `D:/Project/mirrors/Trading-docs` 两条独立路径同名）→ 交回启发式，由同名/镜像证据并进正确项目。
- 目录自身等值、但那个名字只是「某个单目录工作树碰巧攒够了卡」→ 仍按前缀收拢（`Alpha-cmp` → `Alpha`）。想让这种目录稳定成为独立项目，用 `-project` 钉一次或登记进 `project_aliases`；在此之前，它自身与它的子目录可能分属两个项目（已知不对称，登记待裁）。

**「未分类」收件箱**：归组失败的目录**不再各自成项目**，统一进名为「未分类」的项目（固定 id `unclassified`，**恒显示**，哪怕 0 张卡——空收件箱本身就是信息）。进桶的有两类：没有工作目录的卡；以及只有一个目录、卡数又不足 3 张的分量。跨机镜像互证的分量（卡上有 `review_dir`）不受卡数门槛限制——两个互证目录本身就是强证据。

真实新项目的第一张卡**会先落进收件箱**：它此刻确实没有任何可归组证据。这是设计意图而非缺陷——桶的语义是"待整理收件箱"，转正靠 `-project` 或在 `project_aliases` 登记，而不是靠攒卡数自己冒出来（攒到第 3 张也会自动成项目，那只是不让小项目永远卡在桶里的兜底）。

**add 时的软约束**：新卡按当前账本会落进「未分类」时，`add` 往 stderr 打一行提示并**照常入队**：

```
警告: 目录 /Users/you/Projects/NewThing 未匹配任何显式/别名/模式/启发式归组，该卡将落入看板「未分类」；
      如属既有项目请用 -project 指定，或在 board.json 的 project_aliases 登记。
```

**不硬阻**是刻意的：合法的新项目本来就该能一条命令派出去，硬拦等于逼人先改配置文件才能开工。

### 按工作性质拆分的进度（`Project.kinds`）

单条项目进度条的分母是全部卡、分子是 done 卡——没算错，但它把三种性质完全不同的活按**张数等权平均**了：审核卡与修复卡生命周期短、完成率天然高，张数又常占七成以上（实测某项目 430 张 `design-review` 对 800 张 `sequence`），于是总条被它们抬到 90% 上下，而真正的落地卡可能才走了四成。**总条方向性地偏乐观**，正是"对后期工作过分低估"的来源。

于是每个项目额外回吐 `kinds[]`，按 **设计 / 落地 / 修复 / 审核 / 协调** 五桶各报各的完成占比（口径与总条完全一致：`done ÷ (total − canceled)`），空桶不回吐。总条**保留不动**——它是唯一与历史读数可比的口径，也是那条不依赖任何分类判断的锚。实测效果：某项目总条 87.9%，拆开是「设计 83.3% ／ **落地 73.2%** ／ 修复 95.5% ／ 审核 100%」。

分类顺序即优先级，**结构信号优先、关键词垫底**，每张卡都带 `kind_source` 如实交代判定依据：

| 顺序 | 判据 | `kind_source` | 归入 |
|---|---|---|---|
| 1 | `board.json` 的 `kind_rules` 首条命中 | `override` | 规则指定 |
| 2 | `x_role=C` ／ `review_of` 非空 ／ `type=design-review` ／ 标题「审核:／对抗复审:…」 | `x_role`／`review_of`／`type`／`title` | 审核 |
| 3 | `fix_round>0` ／ 标题「修复R1:」（**必须带冒号**） | `fix_round`／`title` | 修复 |
| 4 | `type ∈ {coordinate, progress-pull, prompt-assembly, batch}` ／ 标题「收口:／进度:」 | `type`／`title` | 协调 |
| 5 | 标题含设计/方案/规划/调研/选型/架构/评估/蓝图/草案/立项/盘点/design/spec/rfc/roadmap/proposal/blueprint/research | `title` | 设计 |
| 6 | 以上皆不命中 | `default` | 落地 |

两条不可改的取舍：
- **审核必须先于修复判**。审核卡会继承被审卡的 `fix_round`，顺序写反会把成百张审核卡整体计进修复桶——落地进度看着没变、修复桶凭空翻倍，而没有任何报错。
- **判不出的落到「落地」而不是「未分类」**（与 phase 的"未分阶段"取舍相反）。本卡要防的失真方向是低估剩余工作量，把归不了类的活算成待落地的活是往保守一侧偏；单开一个"未分类"桶反而会让落地桶显得比实际更空。`kind_source=default` 已把这件事说清楚。

关键词是启发式、判错在所难免，`kind_rules` 是精确出口（比继续往关键词表里堆词更诚实——堆词会让别的项目跟着遭殃）：

```jsonc
"kind_rules": [
  {"match": "HB-",             "kind": "design"},   // 标题子串，大小写不敏感
  {"match": "t0723-0304-c0d8", "kind": "coord"}     // 也可以写任务 ID 全串
]
```

`kind` 合法取值仅 `design` / `impl` / `fix` / `review` / `coord`。写错的规则**逐条跳过**（其余仍生效，一条手误不连坐整个列表），但被跳过的会挂进 `Project.kind_rule_error`、在项目卡上显示黄色告警——静默失效即造读数。

### 项目归档（总览折叠）

项目多了以后，早就收尾的项目仍常年占着总览的一列。项目卡与项目页上的「归档」按钮把它折叠掉：

- 归档状态写在 `~/.cardex/board_archive.json`，**任务卡一个字节都不动**，调度、ETA、状态计数全都不受影响；顶部状态计数仍含已归档项目的卡，页头会写明「已归档 N 个项目（下方状态计数仍含它们的卡）」。
- 已归档项目默认不铺在总览轨道上，右上角「已归档 N」可临时展开、就地取消归档。
- **有新卡自动切回活跃**：归档时记下当时的 (卡数, 最新 `created_at`)，之后卡数变多、或出现更新的 `created_at`，即判定有新卡并自动恢复活跃，卡上标出「已自动恢复活跃」+ 原因（不说明原因，用户会以为自己没点上归档）。两条判据是 OR：只看卡数会被"删一张加一张"骗过，只看时间会被 `created_at` 缺失的卡漏过。
- **卡状态变化不触发复活**（queued→done、running→failed 都不算）。手动归档表达的是"这个项目我暂时不看了"，已知卡跑完并不构成"有新东西要看"；若按 `updated_at` 判，归档一个仍在跑的项目下一次 tick 就会自己弹回来。
- 自动复活是**只读推导、不回写文件**：归档记录原样留着，每次请求重算。GET 路径因此保持零写入，也不存在"复活写盘失败 → 状态半档"。
- 状态文件读不出来（损坏）时**落错披露**（`archive_state_error`，前端挂黄色告警）并按未归档渲染，且**拒绝在损坏文件上继续写**——静默当成"没归档"会让用户折叠的十个项目集体冒出来且零提示。

`POST /api/project/archive` 是看板唯一的写入端点（body `{"id":"<项目 id>","archived":true}`），三道闸：只收 POST；`Content-Type` 必须是 `application/json`（HTML 表单发不出这个类型，挡住跨站自动提交表单）；带 `Origin` 头时其 host 必须等于请求 Host（浏览器跨站 fetch 必带 Origin）。命令行 `curl` 不带 Origin，放行——本机运维要能脚本化。

### 时间窗口：两份账一起切（`range=24h|7d|30d|all`）

燃尽页顶部的窗口标签页**同时管两块内容**——队列任务消耗与 token 曲线。它们共用窗口，但**不是同一份账**，永远不该被读成同一个数：

| | 队列任务消耗 `task_spend` | token 曲线 `token_series` |
|---|---|---|
| 源 | 任务卡上的 `cost_usd` / `turns_used` | `~/.claude/projects` 的 transcript |
| 口径 | **队列**：一行一张卡，交互会话不在内 | **不分来源**：混着你在 Claude Code 里手敲的会话 |
| 量纲 | 美元（API 等价成本） | 绝对 token 吞吐（等权口径） |
| 拉长窗口的代价 | 零——快照已在内存 | 真金白银扫盘：24h≈104MB / 7d≈419MB / 30d≈1.06GB |

**"为什么曲线里常常只看得到一两个模型"**就是这个分工没说清造成的误读：24 小时里恰好只跑过那一两个，而且多半来自交互会话。把窗口拉到 7 天，实测立刻出现 7 个模型。

**token 曲线的扫描参数随窗口伸缩**（`tokenScanPlanFor`）：桶从 15 分钟粗化到 12 小时（否则 30 天要画 2880 个点，既卡又没信息），字节预算从 512 MB 涨到 4 GB（取实测体量的 2 倍余量，transcript 会随使用增长，卡太紧会在某天悄悄开始截断）。实测冷启耗时 24h/0.6s · 7d/1.3s · 30d/3.1s，四个窗口都没触发截断。`range=all` 对 transcript **按 90 天封顶**并在 basis 里说明——那个目录没有上限，给一个跑不完的窗口不如给一个说得清的。

一旦真的撞上字节闸，`token_series` 会自报 `truncated` / `files_matched` / `files_scanned` / `bytes_scanned`，前端挂红色告警。这不是可选装饰：**一条少了后半段的曲线，与"那段时间没跑活"在图上长得一模一样**，静默截断就是造读数。

`burnCache` 也因此改成**按窗口分格**：扫描量随窗口从 104 MB 涨到 1 GB，共用一格会让每次换标签页都重扫最贵的那个。未知 `range` 归一到 `24h` 那一格，`?range=乱写` 撑不爆缓存。

### 队列任务消耗（`task_spend`）

源是**任务卡自己的 `cost_usd` / `turns_used`**（runner 在卡跑完时把 claude CLI 回报的 `total_cost_usd` / `num_turns` 写回卡上）。这份账随卡长期留存（含 `archive/`），要看多久就看多久且零额外扫描。给出：合计花费 / 计入的卡数 / 无花费数据的卡数 / 合计轮数、**按模型分**（用 `effectiveModel` 解析每张卡实际生效的模型，codex 侧经 `resolveCodexModel`）、**按项目分**。

**为什么按项目而不是逐卡**：一个 30 天窗口里有近千张卡，逐卡表只能截到前几十行，而那几十行往往是同一个项目的连续修复链——看完并不知道"钱花在哪条线上"。项目才是实际做取舍的粒度（哪条线该停、哪条线该加码）。每行给**「计入 / 全部」两个卡数**：只给金额的话，"这条线只花了 $3" 分不清是活少还是花费没记上（codex 侧不回报）；另给该项目花得最多的**主力模型**，回答"这条线的钱主要烧在哪个档位上"。

**两条必须说出口的边界**（`task_spend.basis` 原样呈现在页面上）：

- `cost_usd` 是 claude CLI 回报的 **API 等价成本**，订阅制下**不是实际扣款**，只是"这些活按 API 价该值多少钱"。当成账单看会得出一个吓人且错误的数字。
- **codex / 远端 codex 卡不回报花费**，未跑或已取消的卡同样为空——实测 1423 张卡里 448 张没有花费数据。它们烧的是另一套额度，**未计入**合计；缺口张数必须显示，否则"codex 那半边没花钱"会被当成事实。

时间维度用卡的 **`updated_at`（跑完那一刻）**而非 `created_at`：花费是在卡跑完时产生并写回的，按创建时间归档会把一张上周入队、今天才跑完的卡算进上周——那笔钱是今天花的。`range` 取未知值时回落 `24h`，不报错也不猜。窗口由请求参数决定，所以 `task_spend` **不进 `burnCache`**（那份缓存装的是与窗口无关的 transcript 扫描，混在一起会让每个窗口各占一份昂贵缓存），改为逐请求现算。

**燃尽视图三源**（`/api/burn`）：三源中 `usage-history.jsonl`（即 `usage_feed`）与 `cardex quota` 共用同源；`claude.json` 与 transcript 扫描为 board 独有读数，`cardex quota` 不读这两源：
1. **CodexBar `claude.json`**：claude 侧各账号 session / weekly / opus 窗口的百分比时间序列；
2. **CodexBar `usage-history.jsonl`**（= `usage_feed`）：codex 侧 primary（5h）/ secondary（周）百分比时间序列；
3. **`~/.claude/projects/*/*.jsonl` transcript**：每条 assistant 消息的绝对 token 用量（四类等权相加，附额度口径折算）。

**"数据不足"语义**：只有一个样本点时没有速率可算；样本比它所描述的窗口还老（如 5h 窗口配一条 14 小时前的样本）；或重置时刻已过——三种情况一律 `verdict="数据不足"`，`burn_rate` / `exhaust_at` 保持 null，绝不编造估算值。只在当前窗口周期内（与最新样本同属同一 resetsAt 边界，容差 90s）的点才参与速率拟合。

## 5 小时额度红线（保底额度）

给突发/交互任务留余量：红线生效时队列停止派发（多步任务也会在步骤间让位），`-force` 可越线。三条通道，`cardex quota` 随时查看：

```jsonc
// ~/.cardex/config.json
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

- ① 只统计 cardex 自己的调用（桌面端消耗不可见），语义是"队列预算上限"——保底 = 总额度 − 队列预算。先跑几天 `cardex quota` 看典型消耗再定值。
- ② 是全局视角，样本格式兼容 CodexBar 的 usage-history.jsonl（需在 CodexBar 里开启 Claude 用量探测）；任何工具按同格式落一行 JSONL 都能接。
- ③ 直读 `api.anthropic.com/api/oauth/usage`（`anthropic-beta: oauth-2025-04-20` 头 + 复用 `~/.claude/.credentials.json` 或 macOS keychain 的 OAuth accessToken），取 5h 窗口 utilization。**端点未文档化、可随时变更**——任何异常（网络/凭据/HTTP 4xx-5xx/字段缺失/字段值歧义/格式漂移）一律按"数据不足"→ fail-open 放行；实现只信响应 body，绝不解析响应头（响应头容易被中间层伪造/覆盖，且核验已推翻"响应头带 unified 限流数值"之说）。`utilization` 实测 0-100 百分域原样取整（实样：端点回 31.0 即 31%，与 `limits[].percent=31` 互证）、`used_percent/percent` 同为 0-100 域原样取，**任一自动归一都是假触线温床**（教训：老版 `utilization:1`→100%、`used_percent:0.8`→80% 都能锁死队列）；`utilization` 落在 (0,1] 区间判为刻度歧义（旧分数写法 vs 新百分写法两判均可能错）拒判为数据不足，>100 域外同拒。`oauth_usage_creds_path` 非空即**硬隔离**——只读该文件，不再兜底 `~/.claude`/keychain（避免 Windows `UserHomeDir` 兜底摸真实凭据造成测试隔离失效或权限漂移）。端点结果自带**进程级缓存**（TTL=`oauth_usage_max_age_min` 或默认 15 min），tick 每 15s 重扫不会每次都打端点、也不会重复触发 macOS keychain 弹窗；缓存过期后重抓失败会保留旧样本并披露"已过期+重抓失败"两要素，让 `quota` 能诚实展示。
- ②③ 合并规则=**最保守值优先**（可用样本里 percent 最大者判线）——观测口径不一致时,最坏假设兜住,而不是投票或平均。`cardex quota` 会并列展示三源读数并在分歧 ≥5% 时明确披露区间。
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

## 多订阅引擎（engine profiles：Kimi / GLM / MiniMax / MiMo / OpenCode Go / Ollama Cloud）

Claude 之外的编码订阅计划基本都提供 **Anthropic 兼容端点**——接入方式同构：还是跑同一个
`claude` CLI，只是按任务注入 `ANTHROPIC_BASE_URL` + 认证变量 + 模型映射。cardex 把这层差异
收敛成**引擎档案**（`config.engines`）：每个订阅一段配置、独立冷却、独立限额解析、账本打标；
不配则一切行为与旧版完全一致。端点/模型 ID 均取自各家官方文档（核实日期 2026-08-02），
模型代次更新快，**以你订阅页当前提供为准**，预设只是起手值。

三步接入（以 Kimi 为例）：

```bash
cardex engines add kimi        # 并入内置预设（不含密钥）；可用预设见 cardex engines
export KIMI_API_KEY=sk-...     # 密钥走环境变量引用（config 可安心备份）；launchd 场景见下
cardex doctor                  # 核认证可解析（值不回显）
```

用法两种，可并存：

```bash
# ① 钉定主跑：这张卡花 Kimi 的额度（-model 可用档位别名 sonnet/opus，或供应商原生 ID 如 k3）
cardex add -runner kimi -model sonnet -dir ~/proj "重构上传模块"

# ② 纳入降级链：claude 冷却/红线期自动改道，顺序自定义
#    config.json: "fallback_order": ["codex", "kimi", "glm-cn"]
```

内置预设与统一能力分级（评测源：Artificial Analysis 智能指数 2026 快照 **2026-08-02**，
锚点 = Claude 各档同快照分数：Fable 5 = 59.9 / Opus 4.8 = 55.7 / Sonnet 5 = 53.4；
编码向以 SWE-bench Verified（Vals AI，2026-07）交叉核对。**分级是统一标准线下的档位，
不是各家自报的高中低**；无同快照分数的模型标"未评"，不编造）：

| 预设 | 订阅计划 | 端点 | 默认映射模型（AA 分数） | 统一档位 |
|---|---|---|---|---|
| `kimi` | Kimi 会员（Kimi Code），5h 窗约 300–1200 次请求 | api.kimi.com/coding | k3（57.1，SWE-bench 93.4% vs Fable 5 的 95.0%）/ kimi-for-coding | **opus 档**（距旗舰带 0.9 分，编码实测接近旗舰） |
| `glm-cn` / `glm-global` | GLM Coding Plan（5h 窗按 prompt 计数） | open.bigmodel.cn / api.z.ai | glm-5.2（51.1，1M 上下文）/ glm-4.5-air | **sonnet 档** |
| `minimax-cn` / `minimax-global` | MiniMax Coding Plan | api.minimaxi.com / api.minimax.io | MiniMax-M3（44.4，1M 上下文） | haiku 档 |
| `mimo` | 小米 MiMo Token Plan（预付费按量） | api.xiaomimimo.com | mimo-v2.5-pro（42.2）/ mimo-v2-flash | haiku 档 |
| `opencode-go` | OpenCode Go（$12/5h、$30/周、$60/月美元等值，17 模型一把钥匙） | opencode.ai/zen/go/v1 | kimi-k3 / glm-5.2 / deepseek-v4-flash | 随映射模型（默认 opus 档） |
| `ollama` | Ollama Cloud 云订阅（Free/Pro/Max） | ollama.com | glm-4.7:cloud（33.7）等 `:cloud` 目录 | 随映射模型 |

参考分数（同快照）：Kimi K2.6 = 44.2、DeepSeek V4 Pro = 44.3、Qwen3.7 Max = 46.0、
GLM-5 = 39.5、GLM-4.7 = 33.7、MiniMax M2.7 = 38.1。据此的**推荐降级链**（按档位从高到低，
只把你真的订阅了的加进去）：`["codex", "kimi", "opencode-go", "glm-cn", "minimax-cn", "mimo", "ollama"]`。

**行为语义**（与 codex 备用执行器的差异是理解重点）：

- **会话可续**：引擎跑的就是 claude CLI，钉定卡有 SessionID、多步 `--resume`、限额续跑全可用
  （codex 无会话）。改道（非钉定）运行**不回写会话**——否则冷却结束后卡回到 claude 会
  `--resume` 引擎会话，跨引擎身份漂移，与交叉链"入队即钉引擎"同一禁区；
- **冷却分账**：每个引擎独立 `cooldown-<名>.json`。Kimi 撞限额绝不挂 claude 队列，反之亦然；
  claude 的全局 `cooldown.json` 语义不变。限额无重置时间戳时按档案级回退等待（月度/计费周期
  措辞自动抬到 ≥6 小时）；
- **账各归各**：引擎调用不占 claude 的 5 小时红线预算（红线三通道只管 claude 订阅）；
  各家**没有公开用量端点**，看板/quota 只披露"冷却状态 + 本地账窗口计数（下限口径）"，
  不做燃尽估算——数据不足显式披露，绝不编造；
- **质量地板全量沿用**：`no_fallback_models` 钉定模型、交叉卡、复审位（design-review /
  交叉裁决 C 卡）在降级链上对**每一个引擎**同等不降级；钉定引擎的卡在该引擎冷却/缺配置时
  跳过等待，绝不 fail-open 回 claude；
- **凭据三选一**：`auth_env`（环境变量名引用，推荐）> `auth_file`（0600 文件，launchd 不想改
  plist 时用）> `auth_value`（明文，仅限无真实密钥的场景）。`cardex cmd <id>` 打印的手动接管
  命令只给引用形态（`$KIMI_API_KEY` / `$(cat 文件)`），永不解析明文；doctor 只报"可解析/缺失"。

手动接管一张引擎卡时 `cardex cmd <id>` 会打出完整 env 前缀命令，例如：
`ANTHROPIC_BASE_URL=https://api.kimi.com/coding/ ANTHROPIC_API_KEY=$KIMI_API_KEY claude --model kimi-for-coding`。

### 自定义分级（`model_tiers`：无更强模型的机队按牌面定档）

上面的统一分级是**绝对标准线**（锚点是 Claude 各档）。但如果你的机队里没有更强的订阅——
比如只有 GLM + MiniMax——按标准线你的最强模型永远只是"中档"，而实际使用中它就是你的
最强档。`model_tiers` 让你按手里的牌面自定义档位，**自定义恒优先于标准线**：

```jsonc
// GLM-only 机队示例：glm-5.2 顶最强档，minimax-m3 顶中档
"model_tiers": {
  "glm-5.2":    "fable",     // 键=模型 ID（全小写；前缀匹配，"glm-4.7" 盖住 "glm-4.7:cloud"）
  "minimax-m3": "sonnet",    // 值=档位关键字 fable/opus/sonnet/haiku（写错载入即拒）
  "glm-4.5-air": "haiku"
},
// 配合引擎档案的槽位映射，把最强模型填进 fable 槽——设计/复审类卡（默认 claude-fable-5）
// 钉定该引擎时就落在你的最强模型上，且不再产生"档位回落"披露噪声：
"engines": {
  "glm-cn": { "models": { "fable": "glm-5.2", "opus": "glm-5.2", "sonnet": "glm-5.2", "haiku": "glm-4.5-air" }, ... }
}
```

生效面：看板/`cardex list` 的模型档位标签、`cardex engines`/`cardex quota`/看板额度条的
引擎档位（未显式写 `tier` 时从最高档映射模型自动推导）、消耗页的主力模型档位。
**派发路由不吃档位**——真正决定"哪张卡跑哪个模型"的是引擎档案里的 `models` 槽位映射与
`fallback_order`，`model_tiers` 只管把展示与推导对齐到你的机队现实；统一标准线表仍在
（未列条目回落它），两套口径孰是孰非不需要争：你列的条目就是你的口径。

## 卡级投入产出分档（`-stakes` → 复核深度查表）

"这张卡值不值得配一轮对抗复审、值不值得抬思考档"是**投入产出**判断。以前它只活在派卡人的纪律里：
忘了加 `-review-after`，一张高风险卡就裸奔进主干；顺手加上 `-review-after`，一张改错别字的卡也要烧一轮 fable 复审。
`-stakes` 把这个判断收敛成一个问题——**这卡多重要**——深度由查表决定：

```bash
cardex add -stakes low  -dir ~/proj "把 README 里的拼写改一下"      # 不配复审
cardex add -stakes high -dir ~/proj "改鉴权中间件的会话失效逻辑"      # 强制复审 + 思考档抬到 high
cardex add -dir ~/proj "常规改动"                                  # 缺省 normal：保持既有行为
```

查表在 `config.json` 里，三档带默认值：

```jsonc
"stakes_policy": {
  "low":    {"review": "off"},                            // 强制不配复审
  "normal": {"review": "follow"},                         // 跟随 -review-after 的显式指定（缺省档）
  "high":   {"review": "on", "default_effort": "high",    // 强制配复审 + 思考档地板
             "max_fix_rounds": 4}                         // 修复轮限放宽一轮（全局是 3）
}
```

- `review` 取值 `on` / `off` / `follow`（`follow` = 不干预，保留 `-review-after` 的原值）；
- `default_effort` 是**地板不是覆盖**：只在没显式给 `-effort` 时生效，且只抬不降——类型默认已经是 `max` 的卡不会被拉低到 `high`；
- `max_fix_rounds` 按档覆盖全局的同名配置（"实现→对抗审核→自动修复"闭环的轮次上限，全局缺省 3）。
  内置默认只在 `high` 档抬到 **4**，`low` / `normal` 写 `0`（= 跟随全局）。
  **为什么只放宽高档**：`retro-77` 复盘样本里，10 张高 effort 规格对齐类卡有 9 张撞在上限 3 上进了人裁壳，
  事后复核一律判"壳清、工作在新链继续"——上限对这类卡不是护栏而是噪声源，它没拦住任何打转，
  只是把同一件事换条链重跑，白烧一次派卡和一次人工翻看。低价值卡在实现层打转三轮就该停，不动；
- `-effort` 显式指定恒优先于地板（`-stakes high -effort low` 就是 `low`）：命令行说了算，否则命令行不再可信；
- 只写部分档位也可以，没写的档位沿用内置默认（按键合并，不是整表顶掉）；
- **档内没写的字段也沿用内置同档位的值**——JSON 的合并粒度只到键，`{"high": {"default_effort": "xhigh"}}`
  会把整条 high 规则替换成"只有 default_effort"，`review` 变成空串。若把空串当 `follow` 处理，
  这一行"我只想抬思考地板"就会顺带**解除所有 `-stakes high` 卡的强制复审**，而且不报任何错。
  所以留空 = 继承，**要"不干预"必须显式写 `"review": "follow"`**；
- 已知缺口：`default_effort` 同理留空即继承，因此"high 档但不要思考地板"在 high 档表达不出来
  （没有代表"无地板"的合法档位字面量），只能显式写一个更低的档位。`max_fix_rounds` 同理：`0` = 继承，
  所以"high 档跟随全局轮限"也表达不出来，只能把全局那个数显式写进 high 档；
- 查表值写错（`review: "yes"`、`default_effort: "ultra"`、`max_fix_rounds: -1`）**在 add 时直接报错**，不静默按默认放行——一张写错的表静默失效比报错贵得多。

**入队即钉（防漂移）**：查表**只在 `add` 时执行一次**，结果固化到卡面（`review_after` / `effort` / `max_fix_rounds` 三个字段），
运行期一律不再回查 `config.json`。否则改一次 `stakes_policy`，队列里所有在跑/在等的卡的复核深度都会静默变化，
而卡面看不出任何差别。卡上的 `stakes` 字段只作审计留档，不是运行期判据（与交叉验证"入队即钉引擎身份"同一纪律）。

轮限钉的是**解析后的绝对轮数**（不是 `0` 这种"跟随全局"的哨兵值）：否则一张卡入队时留 `0`，跑到第 3 轮时
有人改了全局 `max_fix_rounds`，它的上限就静默变了。钉绝对值后，`cardex show` 上直接看得见这张卡还能修几轮。
该字段随修复链、审核卡、超轮限升级卡一并继承——不继承的话，一张 `-stakes high` 的卡只有第一轮享受放宽的上限，
第二轮起又被静默截回全局值。

### 复审位质量地板（复审卡恒不被降级改道）

`no_fallback_models` 是一张**按模型名**的黑名单。它挡不住这种情况：有人把复审卡的模型从 fable 换成 opus 或 sonnet
（`type_defaults.design-review.model` 或派卡时 `-model`），护栏就静默失效——而失效的表现恰恰是"复审照跑、照出 verdict"，
只是换了个引擎、审得更浅。**审得浅 ≠ 没审**：账面仍然 pass，闭环仍然放行，没有任何信号。

所以在模型名黑名单之上补一条**按卡的角色**的地板：

- `design-review` 卡（实现→复审→修复闭环里唯一的质量裁决点）
- 交叉验证的合并/裁决卡（`x_role = C`，`crosscheck-merge` 模板）

这两类卡在 claude 冷却/红线期**恒不参与 codex 改道降级**，与它们挂的是什么模型无关。
代价是它们在 claude 空窗期只能排队等——这正是本条要买的东西：**宁可复审晚做，不要复审做浅**。

## 自动复盘卡（`retro_every_n_done`）

队列跑了几十张卡之后，"哪类卡老失败、修复要几轮、钱花在哪个模型上、复审到底拦下过什么"这些账没人算。
人不会主动去翻 `archive/` 与 `events.jsonl`，于是同一个模板缺陷、同一类改道事故会反复发生。
打开这个开关，"每 N 张 done 算一次账"就变成机械动作：

```jsonc
"retro_every_n_done": 10    // 0 = 关闭（默认）；建议 10
```

每累计 N 张卡进入 `done` 终态，自动入队一张 `progress-pull` + `haiku` 的复盘卡（模板 `templates/retro.md`，可自行修改），
工作目录钉在数据根，只读统计最近 N 张归档卡的：

1. 失败类分布（`failed`/`retry` 事件的 reason 与卡的 `last_error`）
2. 修复轮数分布（`fix_round`）
3. 每卡成本与模型分布（`cost_usd` 按 `model` / `runner` 分组；带 `cost_unavailable` 标记的卡另计并在 `gaps` 分列，绝不当 0 计入总额）
4. 复审 verdict 分布（`design-review` 与 `x_role=C` 的结论）
5. 超轮限与改道事件（超轮限的升级卡、`limit_paused` 次数、`runner=codex` 的改道卡）
6. **最多 3 条**可执行建议（如"某类卡建议派卡时用 `-stakes low`"、"某模板缺 X 导致反复返工"）

报告落 `progress/retro-<水位>.json`，用 `cardex progress -show retro-<水位>` 查看。

**proposal-only（D11 恒规）**：复盘卡是**只读分析**——不改 `config.json`、不改模板、不改任何卡。
建议由人或监控 session 消费。让自动化去改自动化自己的参数，是把"错误的建议"直接变成"错误的生产配置"。

**幂等（崩溃不重复入队）**：计数器落在 `<root>/retro_counter.json`（单写点 = 本二进制；进程内互斥 + tick 单实例锁）。
它记两个数：`done_total`（累计终态张数）与 `triggered_at`（**已触发复盘的水位**）。
入队复盘卡前先把水位推到当前总数并落盘，再入队——崩在中间只会**少**一张复盘卡，绝不会重复入队。
方向是刻意选的：重复的复盘卡既烧额度又往 `progress/` 里塞重复报告，还会让下一次复盘统计到自己的噪声；
少一张复盘远比多一张便宜（与墓碑机制"宁可少注入一次也不重复注入"同源）。
开关关闭（0）时**仍然计数**，所以打开那一刻就有历史基数可用，不必从零重新攒 N 张。复盘卡自身完成时不计数。

### 终态事件的成本遥测（复盘的算账口径）

复盘按**事件账本**算成本，所以每条终态事件（`done` / `failed` / `canceled`，以及 `held`）都必须回答"这张卡花了多少"：

- 卡上有累计用量 → 事件 `detail` 带 `cost_total` / `turns_total`（该卡**累计**用量，跨限额中断续跑仍累加）；
- 卡上没有任何用量 → 带 `cost_unavailable: true` 与 `cost_unavailable_reason`，**不许静默省掉字段**。

**为什么无数据也要显式落标记**：省掉字段，复盘就只能猜；而"这张卡没花钱"与"这张卡花没花钱我们不知道"
在账面上会长得一模一样，报出的总额系统性偏低且偏低多少无从得知。`retro-77` 样本里 10 张卡有 9 张查不到
成本——正常完成路径写了，但取消、超轮限升级、分类器直判 failed/held 这些**提前退出**路径只写 `reason` 就收工。
静默缺字段让不完整的统计看起来完整，与事件账本"缺口显式披露、绝不靠反推伪造完整历史"的第一性纪律直接冲突。

**覆盖面（可现算，别按数量词记）**：本仓所有 `emitTaskEvent` 的终态事件（`done`/`failed`/`canceled`/`held`）
调用点，其 `detail` 都必须包一层 `withCostTelemetry`——这条全称声称由 `TestEveryTerminalEmitSiteWrapsCostTelemetry`
机械钉住：它解析本包非测试源码的 AST，逐个 emit 点核对，漏一个就报红并点名文件:行号。
新增终态 emit 点忘了接遥测，不必等复盘丢账才发现。

同一张卡可能先 `held` 后 `failed`，各带一条当时的累计值：算账时**每张卡只认最后一条终态事件**，逐条相加会重复计账。

**链累计口径（`chain_cost_total` / `chain_turns_total`）**：超轮限升级卡的 `held` 另带这两个键，
它们是**整条修复链**撞墙前的累计开销 = 实现卡 + 各轮修复卡 + 各轮审核卡各自的 `cost_usd` 之和。
链账逐轮沿卡面 `chain_cost_usd` / `chain_turns_used` 继承（`handleReviewVerdict` 派下一轮修复卡时
按 `上一环链账 + 被审卡自身 + 本轮审核卡自身` 累加），不是"最后一张修复卡的账"——修复卡由新建卡起算，
只取它自己的 `cost_usd` 会把 $5.85 的链报成 $0.50。靶：`TestChainCostAccumulatesAcrossFixRounds`。

链账的**边界，勿超范围引用**：只覆盖"实现→审核→修复"这条链上的卡；pass 后的收口卡、`emit` 派生子卡、
交叉验证链等旁支不入账。**存量卡**（升级前入队、卡面无 `chain_cost_usd`）从升级点起累计，
升级之前已跑过的轮次没有链账可继承、会让旧链偏低——这是已知且有意接受的降级，不靠反推补数。

升级卡自身是刚出生的壳卡（`cost_unavailable` 是真实的零用量，不是账本缺陷），两组键分开，谁也不冒充谁：
按卡求和只取 `cost_total`，链账另算，否则同一笔开销会被链上每张卡各记一次。
