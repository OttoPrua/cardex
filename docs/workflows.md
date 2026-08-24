# Cardex 推荐工作流：直派串联与联邦模块管理

**中文** | [English](workflows.en.md) · 返回 [README](../README.md)

Cardex 推荐两种工作流拓扑，并提供最小可生产的模块管理控制面：

- **直派串联**：一个边界清楚的交付，按“设计 → 开发 → 独立审核 → 集成 → 明示 live 门”推进。既有 `review_after` / 修复闭环仍然可用。
- **联邦模块循环**：每个长期产品模块有耐久目标记录，循环 Grok writer → 独立 Grok 对抗审核 → 修复轮；模块集成默认 held。中央只 join 已通过的模块，并另设最终复核与 live 门。

两者都使用同一套 Cardex 任务、持久状态、`depends_on` DAG、写域互斥和事件账本。联邦模式不是第二套任务板，管理 session 不得绕过 Cardex 直接派第二 writer，也不得把 Codex/Sol 当作模块实现引擎。

## 当前硬执行、当前约定、后续路线图

| 能力 | 当前状态 | 精确语义 |
|---|---|---|
| `depends_on` DAG | 已执行 | 前置卡必须 durable `done`；缺边、环、坏 ID、坏 domain binding 对该分量 fail closed，不阻塞无关分量 |
| 显式 write domain | 已执行 | 仓相对路径先规范化；同仓 exact/subtree 重叠、同 domain/lineage、同封闭资源均串行。无写域的写卡仍按同一 Git common dir 整仓串行 |
| 模块 workflow 记录 | 已执行 | `cardex workflow` 耐久绑定 goal/module、repo/worktree、写域、完成标准、轮次上限、候选/审核身份、effect gates、本地进度坐标 |
| 集成门 | 已执行 | 模块集成卡创建即 held；`cardex release` 与 tick 都要求机器核验 `verdict=pass` 且 `p0/p1` 为空，并匹配候选与 custody。`concerns` / `block` / 未知词汇 / 缺输出 / 证据不全一律继续 held。durable review `done` 不够 |
| 模块 writer/reviewer | 已执行 | 模块循环钉 `grok-build`；拒绝 Codex/Sol。未配置 `engines.grok-build` 时钉定卡等待，绝不 fail-open 到 claude |
| 去重与写域冲突 | 已执行 | 同一模块同时最多一个 active writer、一个 active reviewer；重叠 path/resource 拒绝第二 writer |
| Root 通知 | 已执行 | 只对 live-ready、真实外部依赖、Owner 抉择、路线耗尽写 `workflows/root-notify/`；例行进度只落本地 JSON/Markdown |
| 完整 attempt/producer/lease custody | 已知需加固 | 本树以任务终态、非 running、独立 reviewer、无写域、无共享 session、无第二 active reviewer 为最小 custody。PID/PGID `producerGone` 与 20 秒 quiet-window 仍是后续夹具 |
| live / cutover | 默认 held | 集成释放也不等于 live。live 与 cutover 仍是独立 effect gate |

并行联邦写者需要 `max_parallel` 大于 1；默认仍是 1。旧卡不加写域时行为不变。

机器审核词汇只有现役模板的 `pass|concerns|block`。`pass` 要求 `p0=[]` 且 `p1=[]`。`ACCEPT` / `HELD` 不是合法机器结论，除非另有已审核 adapter。

## 直派串联（现有卡仍然有效）

```text
design (R) -> implement (W, optional -review-after) -> independent-review (R)
                                      |
                                      v
                          integrate (W, held + integration_gate)
                                      |
                              explicit live authority
                                      v
                                 cutover (W)
```

```bash
cardex add -project myapp -type sequence -title "auth implementation" \
  -dir /absolute/path/to/myapp-worktree \
  -write-domain-id auth-impl -write-domain-lineage auth-impl-r1 \
  -write-domain-component auth -write-paths internal/auth,tests/auth \
  -review-after \
  "只实现冻结设计。"

cardex add -project myapp -type sequence -title "integrate auth" -hold \
  -dir /absolute/path/to/integration-worktree \
  -depends-on <review-card> \
  -write-domain-id auth-integration -write-domain-lineage auth-integration-r1 \
  -write-domain-component auth -write-paths internal/auth,tests/auth \
  "只集成已接受候选。"
```

无 `integration_gate` 的旧 held 卡仍可用 `cardex release` 放行（兼容）。带 `integration_gate` 的集成卡即使人工 `release`，也会在证据不足时被拒绝；tick 同样 fail closed。

## 联邦模块循环（`cardex workflow`）

```bash
cardex workflow init -mode federated -module auth -goal-id auth-token-v1 \
  -goal "交付独立审核过的 auth token 垂直" \
  -dir /absolute/path/to/auth-worktree \
  -write-domain-id auth-tokens -write-domain-lineage auth-tokens-lineage \
  -write-domain-component auth -write-paths internal/auth \
  -terminal-criteria "独立审核 pass 且 p0/p1 为空；集成与 live 仍 held" \
  -max-rounds 3

cardex workflow advance <id>                 # Grok writer；writer done 后再派独立 Grok reviewer
cardex workflow freeze-candidate <id> -commit <sha> -tree <tree>
cardex workflow ingest-review <id>           # 读审核日志；concerns/block/未知/缺失 → 继续 held
cardex workflow try-release-integration <id> # 仅 admissible pass 才把集成卡 queued
```

模块循环：

1. 耐久记录绑定身份、仓、写域、轮次上限、进度文件。
2. `advance` 派 Grok writer（`runner_pref=grok-build`，`review_after=false`，控制面自管审核）。
3. writer `done` 后派**新的** Grok `design-review`：无写域、不继承 writer session、`review_of` 指向 writer。
4. 审核输出按 `parseReviewVerdict` 读取。`pass` 且空 p0/p1、候选 commit/tree 与 custody 一致 → 状态 `live_ready`，集成仍 held，并写一条 Root 通知。
5. `concerns` / `block` 且未超轮 → 派修复 writer。超轮 → `exhausted`，通知 Root。
6. 例行进度只更新 `workflows/<id>.progress.json` 与 `.progress.md`。

`grok-build` 通过 `config.engines.grok-build` 档案执行（本树尚无独立 Grok CLI 执行器）。未配置时钉定卡等待，不会改道 claude/codex/sol。

## 写域与资源

显式路径必须是相对仓根的闭合路径：`internal/auth` 合法；绝对路径、`..`、glob、`~`、环境变量不合法。目录声明覆盖整个 subtree。同一 Git common dir 的 linked worktree 共享身份。

封闭资源种类：`runtime`、`database`、`profile`、`manifest`、`device`、`credential`、`cutover`。资源 ID 用稳定业务坐标，例如 `database:app.primary`。

## Root 通知

只在 `workflows/root-notify/` 写入以下种类：`live_ready`、`true_external_dependency`、`owner_choice`、`exhausted_route`。`cardex workflow mark -kind external|owner|exhausted` 记录后两类/耗尽。聊天 hook 不是任务状态。

## 后续夹具（未在本卡宣称已执行）

- W2：完整 reviewer attempt custody（`exited` ≠ `producerGone`，quiet-window hash）。
- 原生 Grok Build CLI 执行器（当前靠引擎档案或等待）。
- live/cutover 的 Owner 授权机器门。
