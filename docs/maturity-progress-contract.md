# 成熟度进度合同 v2

Cardex 的三档进度现在有明确边界：

| 视图 | 回答的问题 | 主数据 |
|---|---|---|
| 实发进度 | 已派出的卡完成了多少 | Cardex 任务状态；保持原口径 |
| 预估进度 | 设计目标离可验收实现还有多远 | 版本化能力切片的 0–4 结构成熟度 |
| 工时进度 | 考虑切片工作量后还有多少成熟度工作，以及风险修正完成窗口 | 同一成熟度合同的显式 `effort_weight` 与 ETA |

预估/工时档不得用 Cardex done、代码量、提交量、测试数量、运行进程或 `turns` 直接晋级。
这些数据可以作为活动或诊断证据，但不是成熟度状态。

## 固定计分

`cardex-maturity-progress/v1` 与 `cardex-maturity-progress/v2` 固定以下分值：

- `设计/待开发` = 0
- `遗留待迁移` = 1
- `开发/复审中` = 1
- `隔离候选` = 2
- `主线/集成已有` = 3
- `Live/有界 Canary` = 4

结构成熟度为逐切片分值之和除以固定最大分。分母由 `denominator.version` 管理；增加范围时必须升级分母版本并建立新基线，不能静默改写旧分母。

工时成熟度是独立指数：只有 `weighted_by_effort=true` 且每个切片都提供有限正数 `effort_weight` 时才加权。否则界面明确回退到未加权结构成熟度，不使用任务 `turns` 或 done 率替代。

## 五类成熟度拆分（v2）

`cardex-maturity-progress/v2` 要求每个能力切片声明且只声明一个 `kind`：

| `kind` | 看板标签 | 典型边界 |
|---|---|---|
| `design` | 设计 | 合同冻结、蓝图、权威边界与方案 |
| `impl` | 落地 | 能力实现、集成、适配器与用户路径 |
| `fix` | 修复 | 已知正确性、安全性或恢复问题的修正 |
| `review` | 审核 | 基线对账、评估、等价性和验收门 |
| `coord` | 协调 | 发布、迁移、退役、跨系统组装与收口 |

这是版本化的“主要工作性质”归类，由对账者根据原始设计目标人工维护，不从卡标题或 done 数临时推断。`kind` 只改变展示分组，不改变切片的状态、0–4 分值、迁移 ledger 或 acceptance gate。

每类明细行显示“该类已取得分 / 该类满分”。总条另按“该类已取得分 / 项目总满分”确定各彩色段宽度，因此全部彩色段之和严格等于总成熟度；所有未取得分不再按类别拆散，而是合并成最右侧的一段。工时档使用同样结构，但分子分母改为显式 `effort_weight` 加权分。

看板仍兼容读取 v1 合同；v1 没有稳定 `kind` 时只显示旧式总成熟度和阶段，不猜测分类。新合同应使用 v2。

## 晋级与回退

- 开发/复审中：已有实质代码、合同、测试或可运行实现；Cardex done 本身无效。
- 隔离候选：clean exact SHA、相关测试可复算、fresh independent review `P0=0/P1=0`。
- 主线/集成已有：已进入目标主线或 managed integration，并通过必需门；literal main 与 managed integration 分开记账。
- Live/有界 Canary：必须有 fresh、真实且有界的用户或业务路径；进程、health、配置和服务启动不足以晋级。
- `USER_ACCEPTED` 独立于 0–4 分，必须有用户路径及恢复/回滚验收证据。

`migration_ledger` 允许负向迁移。fresh review 发现反例时，`previous_state=隔离候选`、`current_state=开发/复审中` 会产生 `-1`，不能被钳成零，也不能只留一条文字备注。

## 四层证据

每份合同必须有以下四类 `evidence_layers`：

1. `design`：原始目标、范围、阶段和 acceptance gate；
2. `cardex`：任务登记与调度活动；
3. `git_test`：exact SHA、代码、测试和独立复审；
4. `runtime`：fresh read-only runtime 证据。

插件只读取稳定 JSON，不解析 Codex 会话 JSONL，也不执行证据命令。合同由对账流程生成；看板负责严格解析、逐项重算、超龄检查和展示。

## board.json 配置

```json
{
  "projects": {
    "perlica-hermes": {
      "maturity": {
        "path": "/absolute/path/to/perlica-progress-current-v2.json",
        "project": "PerlicaHermes",
        "max_age_hours": 24
      }
    }
  }
}
```

`path` 必须是绝对路径。合同不存在、字段手误、分母漂移、状态计分被改写、四层证据缺失或快照超龄时，预估/工时档显示“数据不足”；不会静默回落实发进度。

## 回归基准

仓内 `testdata/perlica-progress-reference-v1.json` 的文件名保留其 `perlica-54-slices-v1` 分母身份；内容已升级为 v2 schema，并为 54 个切片固定五类归属。它只代表 `2026-08-09T23:02:58+08:00` 的历史快照，不代表当前状态。测试必须重算得到：

- `96/216 = 44.44%`
- 相对上次同步 `+5` 点、`+2.31pp`
- `6` 项前进、`1` 项证据回退
- literal main / live / 用户验收晋级均为 `0`

fixture 同时保留当时 `513/545 = 94.13%` 的 Cardex done 活动诊断；若主进度得到约 `94.13%`，说明实现错误地使用了 done 率。
