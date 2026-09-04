# manager-wake sender-receives contract (R8 v1)

谁派的卡，回报给谁。本文是 `manager_wake` 投递层在 R8 之后的权威契约：卡面上钉什么、
投递怎么解析、哪些坐标 **保留但不可执行**、以及回滚到 R8 之前的确切条件。

R8 之前，`manager_wake` 只有一种收件人：`config.manager_wake.subscriptions` 里按
project / dir_prefix / task_id 匹配的订阅。这在生产上够用，是因为当前只有一个
Codex thread 同时充当派发 session 与 wake 回报对象。但"收件人"这件事本该由**派卡的人**
在入队那一刻决定，而不是由投递时的配置反推。R8 把它提到卡面。

---

## 1. 卡面：`cardex.task.reply_route.v1`

任务 JSON 上新增可选字段 `reply_route`。缺失 = 存量卡，行为与 R8 之前逐字节相同。

```json
{
  "reply_route": {
    "schema": "cardex.task.reply_route.v1",
    "requester_id": "cardex-control-plane",
    "endpoint_kind": "codex-thread",
    "endpoint_thread": "019fe484-12c5-78b2-986c-6a8bf0d2b079",
    "target_management_conversation": "Cardex Control Plane",
    "callback_top_level_session": "019fe484-12c5-78b2-986c-6a8bf0d2b079",
    "receipt_route": "receipts/perlica/cardex-control-plane",
    "escalate_to_root": false
  }
}
```

| 字段 | 约束 |
|---|---|
| `schema` | 必须是 `cardex.task.reply_route.v1`（空串按本值补齐）。其它值载入即拒 |
| `requester_id` | 必须落在封闭订阅 ID 字符集 `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$` 内，且不得以保留前缀 `owner-` 开头。它派生出投递身份，因此必须是路径安全的文件名分量 |
| `endpoint_kind` | 见下表。未知值拒收 |
| `endpoint_thread` | 仅 `codex-thread` 使用，必须是 UUID；其余端点必须留空——按角色寻址的端点上写 thread 是一条永远不会被执行的指令，拒收比静默忽略诚实 |
| `target_management_conversation` / `callback_top_level_session` / `receipt_route` | 审计坐标。投递层不解释其内容，只保证它们随卡不可改地留痕。不得含前后空白、换行或 NUL |
| `escalate_to_root` | 同一条 wake 在投给 requester 之外，再扇出一份给 root 订阅 |

**入队即钉。** 路由在卡第一次落盘的那一刻固定：之后任何一次 `saveTask` 若改动、清空或
补挂 `reply_route`，都以 `reply_route_immutable` 失败。"卡面上没有路由"同样是钉死的事实——
一张存量卡不会在跑到一半时长出一个 owner。派生卡（自动审核、修复轮、收口、超轮限升级、
emit 子卡、交叉 B/C、workflow reviewer/repair、Cursor Fable 回退 A 卡）在**创建时**深拷贝
父卡的路由，因此终态回报不会在谱系的第一层就断链。

---

## 2. 端点类别

| `endpoint_kind` | v1 可执行 | 语义 |
|---|---|---|
| `codex-thread` | 是 | 投给 `endpoint_thread` 指定的 Codex thread（可以是派发它的子会话本身） |
| `root` | 是 | 投给唯一的 root 订阅（`subscriptions[].role="root"`） |
| `external-agent` | **否** | 保留坐标（Yvonne 等外部 agent）。投递以 `owner_endpoint_unsupported` fail-closed |
| `cursor-agent` | **否** | 保留坐标（Cursor agent）。同上 |
| `management-card` | **否** | 保留坐标（管理卡自身作为收件人）。同上 |

保留坐标是 **create-only**：卡可以声明它，`cardex add -reply-route` 会接受并持久化，
但投递永远拒绝执行。这样做的理由是，"声明一个本 build 不会送达的端点"必须表现为一条
显式的、可读的拒绝记录，而不是被悄悄改投到当时恰好配着的那个 thread。

---

## 3. 投递

### 3.1 开关

`config.manager_wake.owner_routing`，默认 `false`。关闭时本文描述的一切都不发生：
不派生 owner 订阅、不创建任何 `owner-*` 的 cursor/receipt/inflight 文件、
不写 `owner-blocked.json`，卡面上已钉的 `reply_route` 完全惰性，投递字节与 R8 之前一致。
这也是回滚路径：把开关改回 `false` 即可，不需要迁移或清理卡面。

（注意与顶层 `owner_routing_enforced` 无关，后者是 Owner provider 矩阵。）

### 3.2 派生订阅身份

每个 requester 派生出一个订阅 `owner-<requester_id>`，其 cursor、receipt、inflight
文件与配置订阅**共用同一套 v1 协议**：

- `control/manager-wake/cursors/owner-<rid>.json`（`cardex.manager_wake.cursor.v1`）
- `control/manager-wake/receipts/owner-<rid>.json`（`cardex.manager_wake.receipt.v1`）
- `control/manager-wake/inflight/owner-<rid>.json`（`cardex.manager_wake.inflight.v1`）

R8 没有为 owner 投递新写一套持久化协议。claimed→starting→spawned 的相位序、
receipt 先于 cursor、`delivery_uncertain` 不重试、锁序，全部原样复用。

`owner-` 是保留前缀：配置里出现以它开头的订阅 ID 一律以
`reserved_subscription_prefix` fail-closed，因此配置 ID 与派生身份不可能撞名。

### 3.3 路由判定

对 outbox 里每一行，按其任务卡：

1. **无 `reply_route`（存量卡）** → 走既有的配置订阅匹配，行为不变。
2. **有 `reply_route`** → 该卡退出配置订阅匹配（否则一条 wake 会既投给 requester
   又投给某个 project 订阅），改由下列规则投递：
   - `codex-thread` → 投给 `owner-<rid>`，thread = `endpoint_thread`
   - `root` → 投给 root 订阅
   - `escalate_to_root=true` → 在上面之外，追加投给 root 订阅
   - 保留端点或任何结构性错误 → 该 requester 整体 blocked，**一条都不投**

“退出配置匹配”对合法与不合法的路由一视同仁：路由写坏时的正确行为是没有人收到，
而不是回落到配置订阅去猜一个收件人。

### 3.4 合并（coalescing）

- 同一个 root 订阅上，`endpoint_kind=root` 的卡与 `escalate_to_root` 扇出的卡
  合并成**一次** queue 调用（M1）。
- thread 以小写规范化后作为去重键：若 requester 的 endpoint thread 与 root thread
  规范化后是同一个，同一条 wake event id 只会送达一次（M5）。这一层复用既有的
  destination inventory，不是新机制。

### 3.5 隔离边界

**一个 requester 的端点不可达，不得饿死其他 requester。** 但这条豁免的范围被刻意
限死在 **pre-claim 的端点可达性失败**：判定在任何 inflight claim 之前完成，失败时
该 requester 一行都没送出、一个文件都没写。

| 类别 | 处置 |
|---|---|
| `owner_endpoint_unsupported` | 隔离：该 requester blocked，其余照常投递 |
| `owner_invalid_requester` | 隔离 |
| `owner_invalid_thread` | 隔离 |
| `owner_route_schema_rejected` | 隔离 |
| `owner_route_field_rejected` | 隔离 |
| `owner_route_conflict`（同一 requester 的两张卡声明了不同 thread） | 隔离 |
| `owner_root_unreachable`（要求 root 扇出但没有 root 订阅） | 隔离 |
| outbox / cursor / receipt / inflight 损坏、`delivery_uncertain`、`missing_codex_bin`、`reserved_subscription_prefix`、`duplicate_root_subscription` | **pass 级 fail-stop**，整轮中止 |

被隔离的 requester 记进 `control/manager-wake/owner-blocked.json`
（`cardex.manager_wake.owner_blocked.v1`），并出现在 `cardex manager-wake status`
的 `owner_blocked` 与 `diagnosis`、以及 `cardex doctor` 里。它**不**写全局 `error.json`：
一个端点被拒不是全平面故障，把它伪装成全平面故障会让真正的全平面故障失去信号。

`owner-blocked.json` 每轮重写，requester 修好后自动消失。

### 3.6 root 订阅

`subscriptions[].role = "root"` 标记唯一的 root 回报端点。配置里出现两个
`role="root"` 以 `duplicate_root_subscription` fail-closed——root 有歧义时，
"投给其中一个"是猜，不是路由。未知 role 值以 `invalid_subscription_role` 拒收。

root 订阅同时保留它自己的 project/dir_prefix/task_id 作用域：存量卡照旧由它接收。

---

## 4. 命令行

```
cardex add -reply-route '{"requester_id":"cardex-control-plane",
                          "endpoint_kind":"codex-thread",
                          "endpoint_thread":"019fe484-12c5-78b2-986c-6a8bf0d2b079"}' "..."
```

JSON 以封闭解析（未知字段即拒）。校验失败时卡根本不会入队。

`cardex manager-wake status` 增加 `owner_routing`（布尔）与 `owner_blocked`（数组）。

---

## 5. 验收靶（M1–M12）

见 `manager_wake_owner_test.go`，每条可单跑：

| # | 靶 |
|---|---|
| M1 | 两张 root 端点卡合并成一次 root 投递 |
| M2 | Codex 子会话端点回报给自己，不外溢到 root 配置订阅 |
| M3 | `external-agent` fail-closed，零 queue，落持久坐标 |
| M4 | Yvonne / cursor-agent / management-card 只可创建，不可投递 |
| M5 | 规范化后同一 thread 的两条腿，同一 wake id 只送一次 |
| M6 | 端点不可达只隔离该 requester；共享状态损坏仍 pass 级 fail-stop |
| M7 | `escalate_to_root` 扇出到 owner + root，且无 root 订阅时整体隔离 |
| M8 | 存量卡与带路由卡在同一轮共存 |
| M9 | 崩溃相位协议在 owner 身份下同样成立，重启为 `delivery_uncertain` 不重发 |
| M10 | `owner_routing=false` 是逐字节回滚，不产生任何 `owner-*` 文件 |
| M11 | 保留前缀与重复 root 双双 fail-closed |
| M12 | 路由入队即钉、落盘后不可改、继承是深拷贝 |

---

## 6. Future endpoints (R8b)

当前生产形态是 **单一 Codex thread 同时作为派发 session 与 wake 回报对象**——它是 v1
唯一可执行的端点，`root` 只是给这个 thread 起的角色名。

Owner 已确认下一轮（**不是本 lineage**）要把 **Yvonne** 与 **Cursor** 也变成可派发、
可回报的对象。届时：

- `external-agent` 将成为 Yvonne 的可执行 adapter；
- `cursor-agent` 将成为 Cursor agent 的可执行 wake adapter；
- `management-card` 的处置在 R8b 单独裁定（它与前两者不同：收件人是 Cardex 自身的一张卡，
  涉及"唤醒是否会自繁殖"这条独立的边界，不能顺带放行）。

本轮把这三个 kind 保留为 fail-closed 坐标，正是为了让 R8b 只需要**新增 adapter**，
不需要改卡面 schema、不需要迁移已入队的卡、也不需要重新定义隔离边界。

**本 lineage 明确不做**，即使实现看起来很近：

- 不实现 Yvonne runtime；
- 不实现 Cursor agent wake adapter；
- 不引入任何自治循环（wake → 自动派卡 → 再 wake）。R8 v1 的 wake 仍然只是
  "告诉收件人有事发生"，唤醒本身永不授权一次模型 turn 之外的任何动作。
