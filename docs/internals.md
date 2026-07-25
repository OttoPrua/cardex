# ClaudeGo 运行时内核

**中文** | [English](internals.en.md) · 返回 [README](../README.md)

调度器在异常路径上的行为契约：派发顺序、限额恢复、失败分类、卡死巡逻、事件账本、幂等墓碑、权限边界。

## 派发规则（可在 config.json 调整）

1. **续跑优先**（`resume_first`）：被限额打断的任务先于新任务——先把没做完的做完；
2. **priority 大者优先**；
3. **类型顺序**（`type_order`）：默认 审核 > 序列 > 装配（审核便宜且能尽快给出反馈，装配会派生新工作放最后）；
4. 同级按先进先出。

限额是全局的：任何任务撞到限额，写入全局冷却（`cooldown.json`），期间不再派发任何任务、不浪费探测调用；冷却时间优先取错误信息里的重置时间戳，解析不到则回退 `limit_fallback_min` 分钟后重试。

## 限额中断与自动恢复的细节

- 步骤执行中撞限额：任务标记 `limit_paused` 并记录 `mid_step`。到点续跑时不会重发原 prompt，而是向**同一个会话**发送续跑提示（`config.json` 的 `resume_prompt`），让 Claude 从中断处接着做，避免重复劳动。
- 每一步成功后立刻落盘（任务文件原子写入），进程被杀也不丢进度。
- 单实例锁（`.lock`）保证 launchd 的多次触发不会并发跑任务；持锁进程死掉会自动清锁。
- 其他错误（网络、超时等）按 `retry_backoff_min` 退避重试，超过 `max_attempts_per_step` 次标记失败，`claudego retry <id>` 可带着会话与进度重新入队。

## 失败分类分流（CG-3）

单一 `retry_backoff` 遇到明显不可重试的错误（凭据失效/权限拒绝/输入超长）仍在盲目烧 `max_attempts_per_step`，
是把订阅额度打进注定失败的重试里。CG-3 引入有限枚举的失败分类器，每类映射独立策略：

| 类别 | 判据 | 策略 | 事件 |
|---|---|---|---|
| `auth` | 401 / invalid api key / oauth expired / 请重新登录 | 直接 `held`，**不烧 attempts**，等人工 relogin 后 `release` | `evHeld` actor=`runner:classifier` |
| `permission` | 403 / policy 拒绝 / policy/admin blocked | 直接 `held`，不烧 attempts，等 policy/授权调整 | `evHeld` |
| `input_too_long` | prompt too long / context length exceeded / 上下文超长 | 直接 `failed`，不烧 attempts（同 prompt 再送必然再超长） | `evFailed` |
| `timeout` | 步骤超时(N 分钟) / context deadline exceeded | 沿用退避重试（可重试类，只是审计聚合上标类别） | `evRetry`/`evFailed` |
| `executor_crash` | signal killed/aborted / executable not found | 沿用退避重试 | `evRetry`/`evFailed` |
| `unknown` | 判不出来的一切 | **兜底为现行 `retry_backoff`**，行为与旧版逐字节一致（不加类别前缀） | `evRetry`/`evFailed` |

**限额与分类器严格互斥**：`usage limit / limit reached / hit your limit / session limit` 由 `isLimitHit` 独占
（写全局冷却+`limit_paused`），分类器绝不吃 `limit` 类。反例注入的核心验收：与限额相似但无 `limitRe` 特征
的文本（如 `quota nearly consumed with 5% remaining`）**必须**走 `unknown`、不写全局冷却——若被误分类为限额，
`cooldown.json` 一被写测试立即报红。

**未知类兜底纪律**：分类器是"越权就烧钱"的组件——判据用严格枚举正则，只认服务端与执行器明确措辞
（`error`/`failed` 这类广谱词禁用）；判不出来一律 `unknown`，让调用方走现行 `retry_backoff`。这是"新分类器
不能改动已验证行为"的硬边界，回归基线断言（`TestRunTaskUnknownClass_RegressionBaseline`）把它钉死。

**事件账本**：分类结果写入 `events.jsonl` 的 `detail.failure_class` 字段，配合 CG-2 事件流可按类别聚合
"哪种失败在烧 attempts"、"held 里几张是认证问题"。auth/permission/input_too_long 的 `LastError` 带
`[<class>]` 前缀让 CLI/看板一眼可辨；unknown 保持不加前缀以维持回归基线。

**transcript 来源判据不落终态（Round-3 复审 P1）**：`classifyFailure` 只吃 `errorSummary` 提炼的
`msg` 是**第一道防线**，但 `msg` 可能是 `invokeCodex/invokeRemoteCodex/invokeRemoteClaude` 从
`combined`（stdout+stderr transcript）挑出的行——`codexErrorLine` 会命中 codex 硬错误正则挑走裸
`401 unauthorized`/`403 forbidden`/`invalid api key` 等字面量，`invokeRemoteClaude` 的
`parseClaudeJSON=nil` 分支会取 `firstLine(combined)`。这些引用行进 `res.Result` 后经 `msg` 被
`classifyFailure` 判成 auth/permission 直接落 held，等价于让基线本会退避自愈的超时/瞬时抖动被无人
值守静默停摆。**第二道防线**：在 `res` 增加 `ResultFromTranscript` 标记，`runTask` 分类后调
`classificationFromTranscript(res, runErr)`——命中即把终态分类**降级** `retry_backoff`
（`failure_class` 事件仍写供审计，`detail.softened_from_terminal=true`+`reason=softened_transcript_derived`
标注"原本会落 held/failed 被降级"）。`LastError` 按 `unknown` 语义不加前缀，与旧版逐字节一致。
反例注入：`TestRunTaskCodexTranscriptDerivedAuth_SoftenedToRetry` 与 `..._Permission_..._SoftenedToRetry`
两测试锁定该分流；反向对照 `TestRunTaskClaudeStructuredAuth_StillHeld` 证明 claude 结构化 JSON 的
真 401（`ResultFromTranscript=false`）仍走 held 不误伤。

**不做**：不做自动 replan/decompose——已被 CAMEL 类工作验证但 ClaudeGo 现状 held 升级人工的路径已够用，
自动 replan 一旦误判等于"任务被 AI 悄悄改写"，与"完整任务血缘可审计"的诚实性纪律冲突。真需要时单独立卡。

## drain 内巡逻 + review sync 竞态根修（CG-5）

两条独立卡死信号叠加事件账本，让"看得见的完成态"（harvest 早收割）与"什么都看不见"的僵态（patrol）
都进内核处理，不新增守护进程。

**drain 内巡逻（patrol）**——`tick` 循环里已经在跑的取消对账每 `drain_rescan_sec`（默认 15s）扫一轮；
`patrolOnce` 贴附同一循环节奏，对每张在跑卡查两条信号：
- **进程组存活**：`taskPG` 登记表 + `processAlive(pid)` 双查（伪存活/死 pid 残留不骗过巡逻）。
- **心跳**：任务日志 `~/.claudego/logs/<id>.log` 文件 size 是否增长（多步任务的步骤边界才有增长）。

判据（CG-5 R2.2 修订，触发面严格单一条件）：**只**看 `pgDeadTooLong = pgSeenAlive && !alive && dead-since ≥
`patrolPGGrace`（默认 60s）——`patrol` 只处理"执行器进程组已死透且死超 pgGrace"的收尾僵态。心跳
（日志文件 size 静默 ≥ `patrolHeartbeatTimeout`）**不再作独立触发**，只在 `pgDeadTooLong` 成立时用于把事件
从 `procgroup_dead` 升级为 `procgroup_dead_and_no_heartbeat` reason。原因：runner.go 里 stdout 收进内存
buffer，`logBlock` 只在 `invoke` 返回后追加，单步执行期任务日志**天然零增长**；`step_timeout_min` 默认 60min
里 opus 重卡跑单步 30+ 分钟是常态，若心跳独立触发会永久取消归档健康重卡。R2.1 曾把"心跳超阈值 + `pgDead`
(raw)"作为独立触发通道，可绕过 pgGrace 保护窗提前开火（step_timeout≥70min 配置下 step_timeout 击杀后
WaitDelay 10s 窗口或步间 invoke 切换窗口）——R2.2 收紧到 `pgDeadTooLong` 单一触发面消灭该走廊。"活但静默"
场景交给 `invoke` 自带的 `WithTimeout(step_timeout_min)` 兜底。procgroup 存活是**唯一授权凭证**：反例注入
（脚本刷心跳但真实执行器已死）仍被 `pgDeadTooLong` 揪出。启动窗口保护：`pgSeenAlive` 前置守卫排除"任务
刚进 activeIDs 但 invoke 尚未 register"的假阳性。

`patrolHeartbeatTimeout` 随 `cfg.StepTimeoutMin` 伸缩——`tick` 入口按 `max(70min, step_timeout_min + 10min)`
设置（默认 60min → 70min；生产曾按 ~150min 步超时运行 → 160min）。硬编码 70min 会让长步任务死透后一进
pgGrace 就被误归为 `no_heartbeat` 类污染审计视图。伸缩后阈值恒 ≥ step_timeout+10min。

**review sync 从 postComplete 调用时的 patrol 兼容**（CG-5 R2 P1-1 补）：`runReviewSync` 在 `postComplete`
里跑（此时任务仍在 `activeIDs`），必须把 sync pid 也登记进 `taskPG`（`registerTaskInvoke`/`unregisterTaskInvoke`
配对），否则 `anyTaskProcAlive` 假 → `pgSeenAlive && !alive` 计时 → sync 跑 >`patrolPGGrace` 就会被 patrol
误判 `procgroup_dead`，落假 `evStalled`（状态 running 实际 sync 中）+ 对已完成任务空放 cancel，事件链呈
`done→stalled 无 canceled`，违背 `dispatched→stalled→canceled` 因果契约。

触发后**先记 `evStalled` 事件再走 `cancel`**：`evStalled` 是"披露判定卡死"的诊断事件（状态仍 running）,
随后 `cancelRun()` 走同一收尾管线（`cmd.Cancel = killProcGroup` → `ctx.Err()` → `finalizeCanceled` → emit
`evCanceled`）——不引入第二套击杀路径。事件序列 `dispatched → stalled(诊断) → canceled(收尾)`保留完整因果。
`patrolEventCooldown`（默认 5min）挡重复 `evStalled` 噪声。

**review sync 竞态根修**（`runReviewSync` marker 文件）——旧路径"记录不修"：sync 在 ~110s+ 完成且孙进程
（rsync-over-ssh 的 ssh mux 等）吊住 stdout 管道时，`WaitDelay` 10s 收尾跨过 120s deadline，成功同步被
`ctx.Err()=DeadlineExceeded` 误报超时 → 收掉 divert、每轮回退本机审、分流特性静默失效。窗口极窄但真实
存在，长期以"回退无害"带病运行。

根修：包壳跑用户命令并写 marker 文件见证退出码——marker 存在 → 完全按 marker 记录判定，`ctx.Err()`/
`cmd.Wait` 都是 pipe 行为的次生产物不再作证。见到 marker 立即整组击杀，让 `cmd.Wait` 从"知道退出码但还
得等 WaitDelay/ctx 到期"提速到"知道退出码且 wait 立即返回"。用 `( ... )` 子壳包裹用户命令，防用户显式
`exit N` 直接退外壳跳过写 marker 逻辑。**闭括号必须独立成行**（CG-5 R2 P1-2 补）——若与用户命令并行成
`( <cmd> )`，用户命令以 `#` 尾注释结尾（`rsync ... # notes`）或含 heredoc 时 `#` 会吞掉 `)` → sh 语法错误
exit 2 → marker 不写 → 每次同步必失败 → divert 永久静默回退本机审，即本卡立项要救的故障被另一形态重新
引入。`rescueWaitDelay` 保留作二道防线（marker 未按预期写出的边角）。

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
- **`seq` 分配用双层锁**：`O_APPEND` 只挡 write 定位竞态，不挡 `nextSeq`(读)-算-`Write` 的组合竞态——
  两个写者同时读到 max=N 各写 seq=N+1 → 两条同 seq 被"删中间事件也测不出"绕过缺口检测。因此
  `nextSeq+append+fsync` 三步整体加 (a) 进程内 `sync.Mutex`（挡同进程 goroutine + 挡 stale-lock
  bootstrap 竞态）+ (b) `<id>.jsonl.lock` 文件锁（O_EXCL 抢占, TTL 5s, 挡跨进程）。
- **事件缺口不掩盖**：看板活动流按 `seq` 单调检测跳号，见跳号（含 `seq=1` 之前的头部缺失）即插一条
  "事件缺口（缺失 seq X..Y）"显式披露；尾部残尾也披露一条"事件缺口（尾部残行或损坏行已丢弃）"。
  绝不用相邻事件补齐或用 `task.Status` 反推冒充完整历史。
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
- **cli 明示重置（Round-1 追加）**：`cmdSetStatus` 的 `retry` 与 `release` 分支显式调
  `resetTombstoneKind(reconcileCrossKind())` 清 reconcile 墓碑——这两条是 `held/failed → queued` 的唯一
  cli 通道，与 `resume` 侧的 fresh-entry 同源；不清就会让"final 挡住的孤儿卡"永久冒充可采信 done。
- **skipped 强升 held（Round-1 追加）**：`reconcileCrossChains` 的 `injectAtMostOnce` 返回 `skipped=true`
  时（final 已在 / bound 耗尽），把卡从 `done` 升级到 `held` 并 emit
  `evHeld(reason=reconcile_cross_tombstone_exhausted, actor=runner:reconcile-tombstone)`——不再让单腿
  `done` 卡永久冒充可采信结果，也不再靠每轮 drain 的 stderr 无限刷屏当披露。
- **每任务墓碑锁 + 两阶段临界区（Round-2 追加）**：Round-1 的 cli 明示重置让 CLI 与 runner tick 首次
  并发读-改-写同一墓碑账本。无锁下 `injectAtMostOnce` 的 final 回写会静默复活被并发 `resetTombstoneKind`
  删除的条目——`retry` 承诺的"清墓碑重新起 bound=2 自动再裁决"被作废。根修：
  - `sync.Mutex`（同进程 goroutine）+ `tombstones/<id>.json.lock`（跨进程；tmp+`os.Link` 原子挂名，TTL 5s，
    stale 判据与 release 侧 PID 校验都复用 `staleEventLock`/`releaseEventLock` 同源实现）。
  - `injectAtMostOnce` 两阶段：阶段 1 持锁读账本 + 写 pending → 释锁；阶段 2 无锁跑 `inject`
    （resume 侧的 LLM 子进程单次可达数分钟，持锁横穿会被 stale 强夺）；阶段 3 再取锁认领并升级 final。
  - **entry-gone / nonce 二层防御**：阶段 3 再读账本时若条目已被并发 `reset` 删掉，或 nonce 已不是我们写的
    pending，一律放弃 final 重建——即便锁语义未来回归，final 也不会静默复活 reset。
  - **类闭合**：`resetTombstoneKind`（`runTask` fresh-entry / cli `retry` / cli `release` 三处）与
    `archiveTaskTombstones`（`clean` / `postComplete` 两条 archive 路径）都走同一把锁；`archive` 侧的
    裸 `os.Rename` 与 `injectAtMostOnce` 阶段 3 竞态会造出"归档文件 + 只剩 final 一条的活动墓碑"骗审现场。
- **emit 必须先于 saveTask（Round-3 追加）**：`reconcileCrossChains` 的 skipped→held 分支与
  `injectAtMostOnce` 闭包的 failed 分支旧顺序（save 先 emit 后）在崩溃/IO 错落两者之间时——卡已落盘
  `held/failed`，孤儿谓词 `status==done` 永久排除该卡，`evHeld/evFailed` 永久丢失且无补发路径，账本呈现
  `done→held/failed` 零事件跳变，正是墓碑要消灭的"零披露"缺陷类。修复：把 emit 挪到 `saveTask` 之前，与
  `runTask` resume 侧顺序对齐——崩溃落中间时盘上仍 `done`，下轮 tick 重入孤儿谓词，再 emit+save 自愈收敛；
  代价至多一条重复事件（`bound=2` 挡住无限重复），优于永久静默。事件重复优于事件永久缺失，是墓碑存在的
  第一性理由；`TestReconcileSkippedHeldEmitsBeforeSave` / `TestReconcileFailedEmitsBeforeSave` 用
  `saveTask` 失败代理"崩溃在 save 后 emit 前"，任一处顺序回退即报红；`TestResumeHeldSourceOrder` 用源码
  静态守卫锁死 resume 侧同一契约防未来重构反转。
- **随卡归档**：墓碑账本随 `archiveTask` 移到 `archive/tombstones/<id>.json`，审计可看"这张卡的每次注入都尝试了几次"。

数据模型（单 JSON 而非 JSONL）：`{"version": 1, "entries": {"resume:0": {kind, attempt, phase, nonce, ts}, "reconcile:cross": {...}}}`
——一次 `atomicWrite`（tmp→rename）即可原子替换整份账本，注入点数量少（步数上限），不需要行级追加语义。

## 权限与安全

任务默认**不**使用 `--dangerously-skip-permissions`：

- 审核/装配任务是只读工具白名单；
- `sequence` 默认 `acceptEdits` + 常用构建测试命令白名单，白名单外的 Bash 命令在无头模式下会被自动拒绝（Claude 会绕开或说明）。

需要完全自主时对单个任务加 `-skip-permissions`，或改 `config.json` 里对应类型的 `skip_permissions`。工具白名单在 `type_defaults.*.allowed_tools` 中按类型调整。
