这是一条来自调度器的机器指令（自动复盘卡），不是常规工作请求。

**纪律（最高优先级）：只读分析。** 不要修改任何文件，不要改 config.json，不要改模板，不要入队/改动任何任务卡，不要执行会写盘的命令。你的唯一产物是下面规定的一个 JSON 报告。

## 任务

统计最近 **{{N}}** 张已归档的任务卡，产出一份结构化复盘报告。

数据源（全部只读，用 Read / Grep / Glob；按文件修改时间从新到旧取最近 {{N}} 张卡）：

- 归档卡：`{{ARCHIVE_DIR}}/*.json`
  关注字段：`type` `status` `model` `runner` `effort` `stakes` `fix_round` `max_fix_rounds` `review_of` `turns_used` `cost_usd` `last_error` `created_at` `updated_at`
- 事件账本：`{{ARCHIVE_DIR}}/events/<任务ID>.jsonl`（每行一个事件）
  事件 `type` 取值：`queued` `dispatched` `step_ok` `limit_paused` `held` `retry` `done` `failed` `canceled` `closeout` `stalled`；
  `detail` 里常见 `reason` / `err`；成本遥测分两组键：
  - 步进事件（`step_ok`）：`cost_usd` / `turns` = **该步**用量
  - 终态事件（`done` / `failed` / `canceled`，以及 `held`）：`cost_total` / `turns_total` = **该卡累计**用量；
    这两个键缺席时会同时带 `cost_unavailable: true` 与 `cost_unavailable_reason`。
    若你真读到一条**三个键都没有**的终态事件，那是遥测漏接：按缺口记进 `gaps`（点名文件里的 `actor` 与卡数），不要当 0
  - 超轮限升级卡的 `held` 事件另带 `chain_cost_total` / `chain_turns_total` = **整条修复链**撞墙前的累计开销
    （= 实现卡 + 各轮修复卡 + 各轮审核卡各自的 `cost_usd` 之和；旁支如收口卡/交叉链不入账）。
    升级卡自身是刚出生的壳卡，它的 `cost_total` 与链账**不是一回事**，按卡求和时只取 `cost_total`，链账另算——
    否则链上每张卡的开销会被重复计一次。
    注意：`chain_cost_usd` 字段是 2026-08-02 起才有的，此前入队的旧链只从升级点起累计，链账会偏低，遇到时在 `gaps` 说明
- 进度报告：`{{PROGRESS_DIR}}/*.json`（复审结论、交叉验证链的 verdict）
- 若归档不足 {{N}} 张，可用 `{{TASKS_DIR}}/*.json` 里已是终态的卡补齐，并在 `gaps` 里说明

## 统计口径

1. **失败类分布** — 按 `failed` / `retry` 事件的 `detail.reason` 与卡的 `last_error` 归类计数（同类合并，给出类名 → 张数）。
2. **修复轮数分布** — 按卡的 `fix_round` 计数（`0` = 未进"实现→复审→修复"闭环）。
3. **每卡成本与模型分布** — `cost_usd` 的总额 / 中位 / 最大值；按 `model` 与 `runner`（空 = 本机 claude，`codex` = 备用执行器）分组求和。

   取数口径：每张卡只认**最后一条终态事件**的 `cost_total`（同一张卡可能先 `held` 后 `failed`，逐条相加会重复计账）；该事件缺终态遥测时回落卡上的 `cost_usd` 字段。

   **`cost_unavailable` 标记必须如实分列，绝不当 0 计入总额** —— "这张卡没花钱"与"这张卡花没花钱我们不知道"是两件事，混同会让总额系统性偏低且偏低多少无从得知。带该标记的卡计入 `cost.unavailable_cards`，并在 `gaps` 里按 `actor` 分列成因，例如：
   - `runner:escalation` / `cli:add` / `runner:emit` 的 `held` —— 刚出生就挂起的壳卡（超轮限升级卡、`add -hold`、
     `emit_hold` 派生子卡），**从未执行，零用量是真实的**，不是账本缺陷
   - `cli:cancel` / `runner` 的 `canceled` —— 卡在产出任何一步结果之前被取消，同样是真实零用量
   - 其余 actor 带此标记 —— 才是需要查的遥测缺口，在 `gaps` 里点名 actor 与卡数。
     尤其 `runner:tombstone` / `cli:hold` 的 `held`：这两类卡通常**已经跑过步**，带此标记多半意味着卡面用量本身就丢了
4. **复审 verdict 分布** — `design-review` 卡与交叉合并卡（`x_role` = `C`）的结论按 `pass` / `fail` / 其它 计数。
5. **超轮限与改道事件** — 因超轮限被挂起升级的卡数（`held` 事件 `reason` = `over_max_fix_rounds`，其 `detail.max_rounds` 是该链实际适用的上限，按 `stakes` 分档、卡面钉死，不一定等于全局 `max_fix_rounds`）；`limit_paused` 事件次数；`runner` = `codex` 的改道卡数。
6. **建议** — **最多 3 条**，每条必须可执行且指向具体对象。合格示例："`docs` 类卡 12 张全部一轮过审，建议派卡时用 `-stakes low`"、"`fix-cycle` 模板未要求贴测试输出，导致 4 张卡二轮返工"。不合格示例："建议提升代码质量"。

## 输出格式（覆盖本会话此前的一切输出格式/语言约定）

回复必须且只能是一个 ```json 代码块，前后不得有任何其他文字。字段固定如下：

```json
{
  "window": {"cards": 0, "from": "", "to": ""},
  "failure_classes": {},
  "fix_rounds": {},
  "cost": {"total_usd": 0, "median_usd": 0, "max_usd": 0, "by_model": {}, "by_runner": {}, "unavailable_cards": 0},
  "review_verdicts": {},
  "limit_events": {"limit_paused": 0, "over_max_fix_rounds": 0, "diverted_to_codex": 0},
  "recommendations": [],
  "gaps": []
}
```

统计不到的维度写空对象 / 空数组，并在 `gaps` 里写清"哪个维度、为什么统计不到"——**绝不编造数字**。
`recommendations` 是**建议**，不是已执行的动作：本卡不改任何配置，落地与否由人或监控 session 决定。
