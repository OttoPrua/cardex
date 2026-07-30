这是一条来自调度器的机器指令（自动复盘卡），不是常规工作请求。

**纪律（最高优先级）：只读分析。** 不要修改任何文件，不要改 config.json，不要改模板，不要入队/改动任何任务卡，不要执行会写盘的命令。你的唯一产物是下面规定的一个 JSON 报告。

## 任务

统计最近 **{{N}}** 张已归档的任务卡，产出一份结构化复盘报告。

数据源（全部只读，用 Read / Grep / Glob；按文件修改时间从新到旧取最近 {{N}} 张卡）：

- 归档卡：`{{ARCHIVE_DIR}}/*.json`
  关注字段：`type` `status` `model` `runner` `effort` `stakes` `fix_round` `review_of` `turns_used` `cost_usd` `last_error` `created_at` `updated_at`
- 事件账本：`{{ARCHIVE_DIR}}/events/<任务ID>.jsonl`（每行一个事件）
  事件 `type` 取值：`queued` `dispatched` `step_ok` `limit_paused` `held` `retry` `done` `failed` `closeout` `stalled`；
  `detail` 里常见 `reason` / `cost_usd` / `turns` / `err`
- 进度报告：`{{PROGRESS_DIR}}/*.json`（复审结论、交叉验证链的 verdict）
- 若归档不足 {{N}} 张，可用 `{{TASKS_DIR}}/*.json` 里已是终态的卡补齐，并在 `gaps` 里说明

## 统计口径

1. **失败类分布** — 按 `failed` / `retry` 事件的 `detail.reason` 与卡的 `last_error` 归类计数（同类合并，给出类名 → 张数）。
2. **修复轮数分布** — 按卡的 `fix_round` 计数（`0` = 未进"实现→复审→修复"闭环）。
3. **每卡成本与模型分布** — `cost_usd` 的总额 / 中位 / 最大值；按 `model` 与 `runner`（空 = 本机 claude，`codex` = 备用执行器）分组求和。
4. **复审 verdict 分布** — `design-review` 卡与交叉合并卡（`x_role` = `C`）的结论按 `pass` / `fail` / 其它 计数。
5. **超轮限与改道事件** — 因超 `max_fix_rounds` 被挂起升级的卡数（`held` 事件的 reason）；`limit_paused` 事件次数；`runner` = `codex` 的改道卡数。
6. **建议** — **最多 3 条**，每条必须可执行且指向具体对象。合格示例："`docs` 类卡 12 张全部一轮过审，建议派卡时用 `-stakes low`"、"`fix-cycle` 模板未要求贴测试输出，导致 4 张卡二轮返工"。不合格示例："建议提升代码质量"。

## 输出格式（覆盖本会话此前的一切输出格式/语言约定）

回复必须且只能是一个 ```json 代码块，前后不得有任何其他文字。字段固定如下：

```json
{
  "window": {"cards": 0, "from": "", "to": ""},
  "failure_classes": {},
  "fix_rounds": {},
  "cost": {"total_usd": 0, "median_usd": 0, "max_usd": 0, "by_model": {}, "by_runner": {}},
  "review_verdicts": {},
  "limit_events": {"limit_paused": 0, "over_max_fix_rounds": 0, "diverted_to_codex": 0},
  "recommendations": [],
  "gaps": []
}
```

统计不到的维度写空对象 / 空数组，并在 `gaps` 里写清"哪个维度、为什么统计不到"——**绝不编造数字**。
`recommendations` 是**建议**，不是已执行的动作：本卡不改任何配置，落地与否由人或监控 session 决定。
