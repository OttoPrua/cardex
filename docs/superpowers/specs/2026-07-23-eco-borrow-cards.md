# ClaudeGo 开发任务卡草案（2026-07-23）

> 依据：同目录 `2026-07-23-agent-collab-landscape.md` 可借鉴清单 × ClaudeGo v0.10.0 已知局限。
> 第一性纪律：每张卡动机锚定已证缺口；验收全部可证伪且含反例注入应报红条目。共 6 张，宁缺毋滥。
> 状态：**已裁决入队**（2026-07-23 委托人对话直裁，立据 PerlicaOptimize design-blueprint/_DECISIONS.md BD-39）。
> 队列映射：CG-2=t0723-0230-4751(p5) / CG-1=t0723-0230-d9f6(p4) / CG-4=t0723-0230-2b84(p4) / CG-3=t0723-0230-fe89(p3·自门依赖CG-2) / CG-5=t0723-0230-bf18(p3) / CG-6=t0723-0230-7572(p2)；全部 -review-after，模型 opus（CG-6 sonnet）。

## CG-1 订阅用量端点直读（第三用量源）

**动机**：借鉴清单 #1（一方 `api.anthropic.com/api/oauth/usage` 端点，`anthropic-beta: oauth-2025-04-20` 头 + 复用 Claude Code OAuth 凭据，返回 5h/7d utilization 与 resets_at）。对应已知局限④"本地账本看不见桌面端消耗"：queue_budget_tokens 只记队列自身，usage_feed 依赖 CodexBar 桌面端存活且样本过期即 fail-open，红线判定存在双盲区。注意：该端点未文档化，属可随时变更的非承诺接口——实现须把"端点消失/格式变更"当一等失败路径。

**设计概要**：
- 新增用量源 kind=oauth_usage，直读端点取 5h 窗口 utilization；
- 与 usage_feed 同接口接入红线判定，多源分歧取最保守值；
- 不解析任何响应头（核验已推翻"响应带 unified 限流头"之说，只信 body）；
- 端点失败/凭据缺失沿用既有 fail-open 语义并日志披露；
- `quota` 命令并列展示三源读数与分歧。

**验收**：
- [ ] mock 端点返回 utilization ≥ redline_percent → tick 不派发新任务；改注入低于线样本 → 恢复派发（自动化断言）。
- [ ] **反例注入**：mock 响应 body 缺 5h 字段但响应头带伪造限流数值 → 必须判"数据不足"；若实现取用了响应头数值，测试报红。
- [ ] mock 端点 500/凭据缺失 → 派发行为与无此源时逐一致（fail-open 回归），quota 明确标注来源不可用。
- [ ] 构造三源（本地账本/usage_feed/oauth_usage）读数不同的用例 → 断言按最保守值判线。

**规模**：S　**依赖**：无

## CG-2 run receipt 事件账本（per-task JSONL）

**动机**：借鉴清单 #4（MartinLoop 签名 JSONL run receipt）。对应已知局限"看板无事件日志，活动流由当前状态反推非完整历史"——这是看板"诚实性第一"原则目前最大的自相矛盾点。

**设计概要**：
- 每任务 append-only `events.jsonl`：入队/派发/步成功/limit_paused/held/retry/canceled/终态/closeout，带时间戳与写入方；
- 单写点收敛在现有状态机原子落盘处，不新增写者；
- board 活动流改读事件流，事件缺口处显式标注"事件缺口"，禁止状态反推补齐；
- clean 时随卡归档 archive/；首期不做签名（防篡改无已证需求，只取追加留痕语义）。

**验收**：
- [ ] mock-claude/codex 集成测试跑全状态机 → 断言每个状态迁移恰有一条对应事件，枚举遗漏即红。
- [ ] 崩溃注入：事件写入中途 kill -9 → 重启后文件仍可解析（尾部残行丢弃并记披露事件），后续追加不损坏。
- [ ] **反例注入**：手工删除中间一条事件 → board 活动流必须显示"事件缺口"；若用状态反推补齐冒充完整历史，测试报红。

**规模**：M　**依赖**：无

## CG-3 失败分类分流（替代单一 retry_backoff）

**动机**：借鉴清单 #3（MartinLoop 13 类失败分类 + CAMEL retry→decompose→replan 分流思想）。对应现状"失败只有单一 retry_backoff，超限标 failed"：限额错误已被特判（写全局冷却）证明分类机制必要且有雏形，但认证失效/权限拒绝/输入超长等不可重试类仍在盲目重试烧额度，与订阅额度哲学直接冲突。

**设计概要**：
- 将 codexErrorLine 式错误解析扩展为失败分类器：有限枚举（限额/认证/权限拒绝/超时/输入超长/执行器崩溃）；
- 每类映射策略：退避重试 / 直接 held 升级人工 / 直接 failed 不重试；
- 未知类一律兜底为现行 retry_backoff 行为，不猜测；
- 分类结果写入 CG-2 事件账本；不做自动重拆/replan（无已证需求）。

**验收**：
- [ ] 注入 mock 认证失败文本 → 不消耗 max_attempts_per_step 重试，直接 held 且事件记明类别。
- [ ] 回归基线：注入未知乱码错误 → 重试次数与退避间隔与现版逐一致。
- [ ] mutation-kill：故意把"认证类→held"策略改为"无限重试"再跑测试 → 必须报红（策略表被断言覆盖）。
- [ ] **反例注入**：构造与限额错误相似但不含可辨识特征的文本 → 必须走一般类且不写全局冷却；若被误分类为限额，测试报红。

**规模**：M　**依赖**：CG-2

## CG-4 幂等墓碑续接（注入至多一次）

**动机**：借鉴清单 #5（AgentBridge 额度接力的 checkpoint + per-pending 幂等墓碑）。对应已知局限③"三卡链非崩溃原子，崩溃+人工 clean 窄缝可能漏网"，以及 limit_paused/mid_step 恢复的"向同会话发续跑提示"在进程重启反复对账下可能重复注入。

**设计概要**：
- 注入动作（续跑提示/断链 reconcile）前写 per-任务墓碑（步序号+单调 nonce），注入成功后落终稿；
- tick 对账见终稿墓碑即跳过，实现"跨进程重启至多注入一次"；
- 覆盖三个注入点：limit_paused 恢复、mid_step 续跑、cross 链 reconcile；墓碑随卡归档。

**验收**：
- [ ] 崩溃窗口注入：mock 在"提示已发送、终稿未落盘"处 kill 进程 → 重启后该步至多再注入一次，mock 计数断言总注入次数 ≤ 2；若无上限，测试报红。
- [ ] 同一步连续两轮恢复对账 → 第二轮零注入（mock 计数断言为 0）。
- [ ] **反例注入**：向墓碑文件写入损坏字节 → 按无墓碑处理并日志披露，不 crash、不静默跳步；若静默跳步则报红。

**规模**：S　**依赖**：无（与 CG-2 协同可强化审计断言，非硬依赖）

## CG-5 drain 内巡逻 + review sync 竞态根修

**动机**：借鉴清单 #6（Gas Town Witness/Deacon watchdog，仅取"巡逻检测卡死"最小内核，不引入独立守护进程）。对应代码内"记录不修"的已知竞态：review sync ~110s 完成 + 孙进程吊管道导致超时误报（当前仅回退无害，属带病运行）。

**设计概要**：
- 不新增独立守护进程——在既有 drain 15s 循环挂巡逻：检查 running 任务进程组存活 + 输出心跳；
- review sync 完成判定由"管道关闭"改为 marker 文件/退出码，根修孙进程吊管道竞态；
- 疑似卡死先记分类事件再按策略处置（整组击杀沿用 cancel 对账机制）。

**验收**：
- [ ] 竞态复现用例：mock 同步脚本 110s 完成且孙进程持有管道 → **修复前此用例必须报红**（证明真实复现），修复后判成功。
- [ ] 真挂死 mock（永不退出、无输出）→ 被巡逻正确标记并整组击杀，事件记类别。
- [ ] **反例注入**：伪造心跳（持续刷新心跳信号但进程组已死）→ 仍判卡死；若被伪心跳骗过，测试报红。

**规模**：M　**依赖**：无（卡死类别可先独立落地，CG-3 就绪后并入其枚举）

## CG-6 README 文档补账 + 键名校验脚本

**动机**：开发惯例要求 README 双语与代码同步 commit，而现状自证的文档缺口：web 看板（7/21-22 新增）、default_review_divert 三件套、codex_fallback_model 均未进 README（README 停在 7/13）。

**设计概要**：
- 双语 README 补三节：board 命令（只读/127.0.0.1/TTL 三纪律 + 燃尽三源与"数据不足"语义）、审核分流三键与默认分流触发条件、codex_fallback_model 档位对等规则（opus 降 terra 不降 sol）与钉定卡绝不 fail-open 语义；
- 附 config 键清单校验脚本：README 中出现的配置键必须 ⊆ config.go 实际键集。

**验收**：
- [ ] 校验脚本断言 board/default_review_host/remote_mirror_root/default_review_sync/codex_fallback_model 在中英两份 README 均出现且键名与 config.go 一致。
- [ ] **反例注入**：向 README 写入一个 config.go 不存在的假键（如 codex_fallback_modle）→ 校验脚本报红。
- [ ] 中英 README 同一 commit 更新（commit 断言两文件均在改动集内）。

**规模**：S　**依赖**：无

## CG-7 rename 设计卡（追加 2026-07-23 · held 待开源触发）

**动机**：委托人问询改名（如 perlicabridge）成本后裁定预立设计卡。评估结论：代码改名小活，真实成本在活状态与跨仓调用方；开源时 "Claude*" 命名有品牌指引风险且低售跨厂商护城河。**触发器=开源决定落地**，此前保持 held；释放前须先按当时现状更新卡 prompt。

**范围**：只出设计不动代码——名字裁选材料/appName 单点化/数据根迁移命令（在途任务零丢失+fail-closed）/CLI 兼容 shim/远端足迹迁移顺序/开源卫生扫描。验收含反例注入（迁移目标已存在须拒绝覆盖，静默覆盖判红）。

**规模**：M（设计）　**依赖**：开源裁决　**队列**：t0723-0304-c0d8（held，p1，opus，-review-after）

## 明确不做

1. **ACP 统一执行器层替换三套 CLI 包装**（借鉴清单 #11）：规范由 Zed 主导演进、Claude adapter 非 Anthropic 原生、计费政策 2026-06 有过变动信号（暂缓执行但未撤回）；现有三套包装稳定且 mock 集成测试覆盖全状态机，无已证痛点。属"预留式重构"，违反第一性纪律。**再评估触发器**：Anthropic 发布官方 ACP adapter，或规范进中立基金会，或任一执行器 headless CLI 破坏性变更迫使重写包装。
2. **git-backed 任务账本（Gas Town Beads 式）**（借鉴清单 #10）：与"每步成功原子落盘 + archive 归档"的现有文件模型冲突大、迁移成本 L；其可证需求（留痕、崩溃窄缝）已分别由 CG-2/CG-4 以小一个量级的代价覆盖。整体迁移属投机性重构。
3. **vibe-kanban 式卡片→worktree→diff→PR 写操作看板**：与一方官方能力重复——Claude Code agent view 已内建后台会话+worktree+draft PR，且自家 5090 spec 已有每卡 worktree+transport commit 方案。board 维持"只读、127.0.0.1、TTL"三纪律不动摇；给看板加写操作既重复官方又破坏只读安全边界。
4. **AgentBridge 式同会话桥接 / mid-execution 注入**：定位不符（ClaudeGo 是 headless 队列，非交互式 TUI 协作）；依赖存疑（Channels 系 research preview 无一方声明；配套 agent-quota-guard 无可查仓库）；且其默认 `--dangerously-skip-permissions` 与 ClaudeGo"权限默认不 skip + 类型级白名单"纪律直接相悖。仅其幂等墓碑思想被 CG-4 单独吸收。
