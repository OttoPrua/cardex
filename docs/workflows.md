# Cardex 推荐工作流：直派串联与联邦多管理线

**中文** | [English](workflows.en.md) · 返回 [README](../README.md)

Cardex 推荐两种工作流拓扑：

- **直派串联**：一个边界清楚的交付，按“设计 → 开发 → 独立审核 → 集成 → 明示 live 门”推进。
- **联邦多管理线**：中央管理线只管整体依赖与最终收口；多个子模块管理线各自循环“设计 → 开发 → 独立审核 → 模块集成”，之后统一 join、整体集成、最终复核。

两者都使用同一套 Cardex 任务、持久状态、DAG、attempt/lease、写域互斥和事件账本。第二种不是另起一套任务板，也不是让管理 session 绕过 Cardex 直接调执行器。

这套形态借鉴 Maestro Flow 的 graph / fork / join / gate / session 表达，但只把适合 Cardex 的部分落在现有控制面上。Cardex 继续负责持久排队、调度、资源互斥、失败留痕和证据坐标；不会被一个新的 graph walker、自动重规划器或第二套状态机取代。

## 先分清：当前硬执行、当前约定、后续路线图

| 能力 | 当前状态 | 精确语义 |
|---|---|---|
| `depends_on` DAG | 已执行 | 前置卡必须有可核验的 durable `done` transition；缺边、环、坏 ID、坏 domain binding 对相关分量 fail closed，不阻塞无关分量 |
| 显式 write domain | 已执行 | 仓相对路径先规范化；同仓 exact/subtree 重叠、同 domain/lineage、同封闭资源均串行 |
| 旧卡兼容 | 已执行 | 写卡没声明 write domain 时，同一 Git common dir 仍按整仓串行；不会因升级而意外放宽 |
| 独立审核角色 | 部分执行 | `design-review` 是只读类型并不占写域；“是否独立于 writer、是否消费了冻结候选、结论是否准入”仍须由 packet/manager 证明 |
| 直派 / 联邦模式名、父子 manager | 推荐约定 | 目前不是 Task 的一等 schema 字段；用 project、title、lineage、DAG、manager-wake scope 和收据表达 |
| 审核通过、集成通过、live、用户验收 | 不由 `done` 推断 | 必须是不同证据门；Cardex 卡完成不自动授权发布、服务重启、设备操作、凭证使用或外部写入 |
| reviewer attempt custody | 已知需加固 | 出现 attempt/producer/lease 矛盾时必须阻断审核采信和依赖释放；机器级完整不变量见后文路线图 |

因此，本页给出的命令是**现在可用的安全编排方式**，不是宣称所有联邦语义都已成为 Cardex schema。

## 两种模式共用的开工恢复

任何 manager 派卡前先只读恢复，而不是从聊天记忆直接重建：

1. 读取 active 与 archive 中已有卡，确认没有相同任务 ID、候选、lineage、review-of 或 hold 后继。
2. 读取目标 Git 仓、全部 worktree、精确 base commit/tree、dirty/untracked 路径和现有 branch owner。
3. 读取正在运行的 attempt、workspace lease、进程残留和当前 write-domain/resource claims。
4. 画出本轮 DAG；冻结共享 contract、schema、fixture 格式后，才开放依赖它们的并行 writer。
5. 为每条写线指定一个 isolated worktree/branch、一个 lineage、一个 active writer、闭合 owned paths 和资源。
6. 为每个审核门指定另一张只读卡/另一上下文。reviewer 不继承 writer 会话，不修改候选，不把旧 attempt 输出拼进新结论。
7. 预先命名模块集成线、整体集成线和 live/cutover 线；最后两类默认 held，等待明确证据与授权。

发现重复线时，无损去重：保留已经拥有 bytes/lease/attempt 的那一条；后来者改为只读 QA、尚未覆盖的验证或排队中的集成线。不要让两个 writer “先都做，最后再挑一个”。

## 写域与资源声明

### 路径

显式路径必须是相对任务仓根的闭合路径：

- 可用：`internal/auth/token.go`、`internal/search`、`docs/workflows.md`。
- 不可用：绝对路径、`.` / `./x`、`..`、glob、`~`、环境变量、反斜杠别名或不稳定 symlink 逃逸。
- 声明目录代表整个 subtree；`internal/auth` 与 `internal/auth/token.go` 会冲突。
- 同一逻辑仓的 linked worktree 共享 Git identity；换 worktree 不会让同一路径变得“互不冲突”。

同一 component 内的不同路径可以并行。例如 `internal/auth` 与 `internal/billing` 可由两个 lineage 同时写；同一路径不能。

### 封闭资源

当前资源种类只有：

`runtime`、`database`、`profile`、`manifest`、`device`、`credential`、`cutover`。

资源 ID 用稳定的业务坐标，而不是 PID、临时目录或自由文本。例如 `database:app.primary`、`manifest:app.release`、`cutover:app.production`。两个仓若会写同一数据库、profile、设备或 live 窗口，就声明同一个资源 ID；路径虽分仓，资源仍会使它们串行。

当前 `WriteDomain` 至少需要一条 path。纯 runtime/cutover 卡若没有诚实的仓内 owned path，必须保守地不声明 write domain（按整仓串行），或先设计独立的 ops/receipt 输出根；不要编造一个假路径只为获得并行资格。

### 角色

| 角色 | Cardex 形态 | 写入权 |
|---|---|---|
| manager | 外部管理 session + scoped manager-wake subscription | 不写 product bytes；只做恢复、拆分、派卡、证据判定 |
| designer | `design-review`，或确需落设计文档时用单独窄写域 `sequence` | 默认只读；写设计文档时也与实现分 lineage |
| writer | `sequence` + explicit write domain | 只写声明路径/资源；一个 lineage 同时一个 active writer |
| reviewer | 独立 `design-review`，不声明 write domain | 只读冻结候选；不得修改、整合或成为 replacement writer |
| integrator | 单独 `sequence` + integration lineage | 只消费已接受候选；不能悄悄改写子模块语义 |
| live/cutover owner | 单独 held gate + shared resource claim | 只有额外 live 授权和回滚坐标齐备后才 release |

`coordinate` 在模型权限上是协调用途，但当前 writer-exclusion 的只读豁免只覆盖 `design-review` 与 `progress-pull`。不要把 `coordinate` 当作“肯定不占写线”的 reviewer 类型。

## 模式 A：直派串联

适用条件：目标单一、接口边界稳定、通常只有一条实现写域，并且一次独立审核与一次集成门足够。

```text
design (R) -> implement (W) -> independent-review (R)
                                      |
                                      v
                                integrate (W, held)
                                      |
                              explicit live authority
                                      v
                                 cutover (W)
```

推荐卡片形态：

```bash
# 1. 只读设计裁决；记录返回的任务 ID 为 <design-card>
cardex add -project myapp -type design-review -title "auth design" \
  -dir /absolute/path/to/myapp \
  "冻结目标、接口、非目标、测试与回滚边界；结论必须可被下一张卡引用。"

# 2. 唯一 writer；<design-card> durable done 后才 Ready
cardex add -project myapp -type sequence -title "auth implementation" \
  -dir /absolute/path/to/myapp-worktree \
  -depends-on <design-card> \
  -write-domain-id auth-impl -write-domain-lineage auth-impl-r1 \
  -write-domain-component auth -write-paths internal/auth,tests/auth \
  "只实现冻结设计；提交精确 commit/tree、changed paths、focused/full tests 与 effect counters。"

# 3. 独立 reviewer：只读、无 write domain、绑定精确候选
cardex add -project myapp -type design-review -title "review auth candidate" \
  -dir /absolute/path/to/read-only-candidate \
  -depends-on <implementation-card> \
  "审核精确 commit/tree；给出 evidence_complete、P0/P1/P2 与 ACCEPT/HELD。不得编辑候选。"

# 4. 集成门先 held；审核卡 done 也不等于管理上已 ACCEPT
cardex add -project myapp -type sequence -title "integrate auth candidate" -hold \
  -dir /absolute/path/to/integration-worktree \
  -depends-on <review-card> \
  -write-domain-id auth-integration -write-domain-lineage auth-integration-r1 \
  -write-domain-component auth -write-paths internal/auth,tests/auth \
  -write-resources manifest:myapp.release \
  "只集成已接受候选；重跑机械门并产生独立 integration receipt。"
```

Manager 必须先读 reviewer 结论和 attempt custody，再 `release <integration-card>`。若 review 卡只是“命令执行结束”但结论是 HELD、evidence incomplete，或 attempt/producer 矛盾，集成卡继续 held。

Live/cutover 再用一张独立 held 卡，至少声明相应 `runtime` / `database` / `profile` / `manifest` / `device` / `credential` / `cutover` 资源。Cardex 入队、review done、integration done 都不是 live 授权。

## 模式 B：联邦多管理线

适用条件：一个产品含多个可独立交付的子模块；各模块有自己的 backlog、长期 manager 和用户路径；模块候选最终还需要整体集成与中央终审。

```text
                         central design / interface freeze (R)
                         /                 |                 \
             module A manager      module B manager      module C manager
             design -> write       design -> write       design -> write
                    -> review              -> review              -> review
                    -> integrate           -> integrate           -> integrate
                         \                 |                 /
                              program join / integration (W)
                                           |
                                  independent final review (R)
                                           |
                                owner acceptance + live gate
                                           |
                                       cutover (W)
```

### 管理职责

- **中央 manager**：维护整体 interface/authority DAG、共享资源表、模块 join 条件、整体集成与最终复核；不接管模块 writer，也不替模块 self-review。
- **模块 manager**：只管理自己的 task/project/dir-prefix scope；在模块内继续拆独立 write domains，派 writer 与独立 reviewer，产出一个可被中央 join 消费的 module acceptance receipt。
- **执行卡**：始终由 Cardex 持久化和调度。管理 session 不绕过 Cardex 直接运行第二 writer，也不把聊天消息当 task transition。
- **独立终审**：消费冻结后的整体候选和各 module receipts；与所有 writer/integrator 分离。

示例 DAG：

| Node | 依赖 | 角色 / claim |
|---|---|---|
| `interface-freeze` | 无 | 中央只读设计门 |
| `auth-design` / `search-design` | `interface-freeze` | 模块只读设计 |
| `auth-impl` | `auth-design` | writer，`internal/auth` |
| `search-impl` | `search-design` | writer，`internal/search` |
| `auth-review` / `search-review` | 各自 impl | 独立 `design-review`，无写域 |
| `auth-integrate` / `search-integrate` | 各自 review | 模块集成 lineage；结论 HELD 时不 release |
| `program-integrate` | 两个 module integrate | 整体 integration lineage + shared manifest/resource |
| `program-final-review` | `program-integrate` | 独立终审，无写域 |
| `program-cutover` | `program-final-review` | held；共享 `cutover`/runtime 等资源；另需 Owner live 授权 |

同一模块也可继续细分。例如 auth 的 token contract 与 session store 若 paths/resources 真正不重叠，可以是两个并行 writer，随后在 `auth-integrate` join。先冻结共享 schema；若两条都要写同一 schema/fixture/manifest，就那一部分串行，其他部分继续并行。

### manager-wake 在联邦模式中的位置

启用时，每个 manager-wake subscription 用 `projects`、`task_ids` 或 `dir_prefixes` 缩到该 manager 的责任域；中央 manager 只订阅 module join、needs-owner 和整体门所需事件。

Manager-wake 是**已提交任务 transition 的通知投影**：

- 消息带 subscription、high-water、task/event/transition/wake ID；manager 收到后必须 fresh-read 卡、event、attempt、Git 和 receipt。
- 多个事件可合并成一次 wake；无 delta 不应产生 model turn。
- 它不是任务状态、审核 verdict、resource lock、live 授权或“自动再派一张”的许可。
- wake 重放或 manager 恢复不得创建重复卡；先按 task ID、lineage、candidate、review-of、worktree 和 active attempt 去重。

## 强制失败夹具：review attempt custody drift

联邦模式必须把下面这类真实事故视为 **acceptance-blocking fixture**：

1. attempt record 已写 `exited`，但对应 reviewer wrapper/binary、子测试进程或 exact PID/PGID 仍活着；
2. 同一卡随后出现新的 attempt/runner；
3. reviewer 临时 worktree 的 workspace lease 与 attempt 状态矛盾；
4. 没有可采信的 semantic verdict，却存在晚到输出或再次派发诱因。

在这个形状下，任何模块 manager 或中央 manager都必须：

- 不采信旧输出、不 splice evidence、不释放 review 依赖；
- 不 retry/release/replace reviewer，不自行 signal 不明所有权的进程；
- 把卡交回 Cardex control-plane owner 做原子 scheduling/attempt-lease containment；
- 要求同一 role/task 同时最多一个 active role instance 和一个 active attempt；
- 只有 `producerGone` 被机械证明后才允许 terminal：exact attempt PID/PGID 及后代消失、runner/reviewer 无残留、workspace lease 可获取、没有后继 attempt；
- 最终 task、event、attempt records、candidate/source identity 与 process absence 一致，并通过 terminal quiet-window 的稳定 hash read-back；
- containment 后是否重开一次 fresh reviewer 是新的 manager 决策，不能把旧 attempt 伪装成终局，也不能 review-of-review。

这不是要求模块 manager 操作生产 containment；恰恰相反，发现该夹具后应 fail closed，把控制权交给 Cardex owner。

与该失败夹具配套的**恢复夹具**也必须保留边界：由 Cardex owner 只调用一次受支持的 `cardex hold`，不手工 signal PID/PGID；控制面原子撤销 scheduling、关闭 active attempt，等 exact runner/reviewer/test 后代全部消失、一次性审核副本消失、候选源仍干净，并通过至少 20 秒 quiet-window 后，才可形成 `terminal held / custody reconciled`。这个 PASS 只证明 containment 和终态一致，旧/晚到输出仍被拒绝，候选仍是 **unreviewed**，不得据此集成。失败与恢复两个 fixture 必须成对测试，不能只测“最后能 hold”。

## 终态与证据语义

| Cardex 状态/事件 | 可以证明 | 不能证明 |
|---|---|---|
| `done` | 这张卡有 durable 完成 transition；DAG 可把它视为满足 | 设计正确、review ACCEPT、候选已集成、已 live、用户已验收 |
| `held` / `needs_owner` | 当前不应继续自动调度，需要外部判断或修复 | 失败已修复、可以 replacement/retry、可以释放后继 |
| `failed` | 本 attempt/任务按既定策略失败并留痕 | 产品不可行、可以跳过审核、可以换 writer 而不去重 |
| `canceled` | 该卡终止且不再提供交付结果 | 工作已完成、同目标不存在其它 active writer |
| manager wake | 某个 committed transition 进入了订阅通知面 | manager 已消费、review 已接受、下一张卡可 release |

每个 join/gate 至少读取：base/candidate commit+tree、changed paths、focused/full tests、独立 review verdict、effect counters、未集成/未 live 状态、必要的 rollback/compensation 坐标。涉及共享 runtime、数据库、profile、manifest、device、credential、external write 或 cutover 时，再要求 preimage/postimage、原子性、fresh runtime read-back 和独立 release gate。

把“代码已写”“测试通过”“Cardex done”“已集成”“运行中”“真实用户路径通过”“Owner accepted”保持为不同字段/receipt，不用一个百分比或一个终态替代。

## 从 Maestro Flow 借什么，不借什么

| Maestro-like idea | Cardex 适配 |
|---|---|
| graph / fork / join | 用 `depends_on`、显式 module join 与 fail-closed DAG；Cardex 仍是 task authority |
| gate / eval | 用独立 review 卡、held integration/live 卡和 digest-bound receipt；`done` 不自动等于 gate pass |
| session / manager | 用 scoped 管理 session + manager-wake；session 不成为隐藏状态源 |
| project spec / knowhow injection | 用冻结的 repo 文档、contract、template 和 evidence refs；不把聊天记忆当 canonical source |
| hooks | 用 committed event → outbox → manager-wake 的窄通知面；hook 不直接写产品或重规划任务 |
| bounded retry / recovery | 保留 Cardex attempt、lease、terminalization、hold 和人工 Owner 路径；不做静默自动 replan |
| visualization | 未来可把 workflow manifest 投影为图；不能反过来让 UI 图成为调度真相 |

## 分阶段机器化路线图

当前已有 DAG 与 write-domain 调度，不需要另造一个大框架。本轮推荐先把工作流契约稳定下来，再做以下窄增量：

### W1 — 离线 `workflow.v1` validator

新增可选、无运行副作用的 manifest/validator，表达：`mode`、manager/role、task ID、parent/module、depends、candidate/evidence refs、write domain、review-of、integration/final/live gate。只做解析和诊断，不自动派卡。

必须校验：

- direct 模式的 design→write→independent-review→integration→live-gate 闭合；
- federated 模式每个 module 有自己的 review/integration，且整体 join 后另有 final review；
- DAG 无环/缺边，domain/path/resource 正规化，one writer per lineage/domain；
- reviewer 与被审 writer 不同 role instance，reviewer 无 product write claim；
- live/cutover node 默认 held，且没有 Owner authority/evidence 时不可标 Ready；
- receipt/evidence 只引用 digest 与位置，不把 prompt、output、secret 塞入 claim。

### W2 — reviewer custody validator 与事故 fixture

先加入一对脱敏 fixture：失败侧复现“attempt record exited，但 producer/children/lease 仍 live，随后同卡 redispatch”；恢复侧只允许受支持的 hold 撤销 scheduling/attempt，等待 exact producers 和一次性副本消失并完成 20 秒 quiet-window，同时保持 review 未采信。validator 必须 fail closed，并证明：

- 一个 role/task 只有一个 active role instance / active attempt；
- `exited` 不等于 `producerGone`；后者必须由 PID/PGID identity、descendant absence、workspace lease、runner residue 和 successor-attempt absence 共同证明；
- producerGone 前禁止 review redispatch、terminal acceptance、old-output splice 和 dependency release；
- terminal quiet-window 内 task/event/attempt/process/source hashes 稳定，才可生成 admissible review receipt。
- custody reconciliation 的 held receipt 不是 semantic review receipt；恢复后是否创建 fresh reviewer 仍需模块 manager 新决策。

### W3 — manifest 与现有 Cardex task 绑定

在 enqueue/doctor/tick 做只读或 fail-closed 绑定：task ID、`depends_on`、write domain、role、candidate digest 与 workflow node 一致；manager-wake 只投影该 node 的 committed current transition。先保留旧卡整仓串行兼容，不做隐式迁移。

### W4 — 图形投影与受控恢复

看板只读展示 module/role/fork/join/gate、claim 冲突和 evidence maturity。任何“重试、替换 reviewer、改 DAG、自动 replan”都输出 proposal 或 held owner decision；不让图执行器越过 Cardex attempt/resource/live 边界。

## 派卡前清单

- [ ] 当前 Cardex/Git/worktree/attempt/lease/dirty state 已只读恢复，重复卡已排除。
- [ ] 选择了 direct 或 federated，且 DAG/join/gate 画清楚。
- [ ] 每个 writer 有独立 worktree、lineage、闭合 paths/resources；共享接口先冻结。
- [ ] 每个 reviewer 是独立只读 role，无 write claim，不消费旧 attempt 拼接结果。
- [ ] 所有依赖用 Cardex task ID 表达；聊天消息和 wake 不替代 transition。
- [ ] attempt custody 一致，`producerGone` 和 terminal quiet-window 在接受 review 前成立。
- [ ] module integration、program integration、final review、live/cutover 是不同门。
- [ ] shared runtime/database/profile/manifest/device/credential/cutover 被显式串行。
- [ ] receipt 明确 exact bytes、测试、review verdict、effects、rollback，以及 not-integrated/not-live 边界。
