这是一条来自调度器的机器指令（自动复盘卡），不是常规工作请求。

**纪律（最高优先级）：只读、proposal-only。** 不要修改任何文件，不要改 config.json，不要改模板，不要入队或改动任务卡。唯一产物是下面规定的一个 JSON 报告。

## 任务

阅读调度器已经确定性编译的最近 **{{N}}** 张 done 业务卡事实，给出简短复盘结论和最多 3 条高 ROI 工作流改进建议。

事实 SHA-256：`{{FACTS_SHA256}}`

```json
{{FACTS_JSON}}
```

## 事实边界

- cohort、计数、成本、失败类、修复轮数、verdict、改道与覆盖率均以以上 JSON 为准；不要重新扫描目录、按文件时间换样本或自行重算数字。
- Cardex 的 done 不代表语义成功，只表示执行器完成；必须结合结构化 review verdict 与 `cards[].last_summary` 描述结果，绝不能把本窗口称为“10 张成功卡”。
- `gaps` 是证据缺口，不得当 0、不得猜测补齐。证据不足的判断写入 `deferred_edges`，不要占用高 ROI 建议名额。
- 建议必须引用 `cards[].id` 中的任务 ID，且只允许 proposal-only；不得声称已经修改或优化了配置。
- 每条结论和建议的 `evidence_task_ids` 至少包含一个本 cohort 的任务 ID；引用外部卡、换 hash、换 cohort 或超过 3 条建议都会被 Cardex 拒绝落盘，并将本复盘卡标为 failed。
- 只从本窗口能支持的事实推导结论。单例、罕见边界或无法证明会重复发生的问题，记录但不要建议立即开发。

## 输出格式

回复必须且只能是一个 `json` 代码块，前后不得有其他文字：

```json
{
  "schema_version": "cardex.retrospective_report.v2",
  "facts_sha256": "{{FACTS_SHA256}}",
  "cohort_task_ids": [],
  "conclusions": [
    {"finding": "", "evidence_task_ids": [], "confidence": "high|medium|low"}
  ],
  "recommendations": [
    {
      "target": "具体工作流对象",
      "change": "最小可执行修改",
      "evidence_task_ids": [],
      "expected_effect": "预期改善",
      "validation": "如何用后续真实任务证伪或确认"
    }
  ],
  "deferred_edges": []
}
```

`cohort_task_ids` 必须按事实中的 `window.task_ids` 原样填写；`facts_sha256` 必须原样返回。`recommendations` 最多 3 条，允许为空。
