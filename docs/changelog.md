# ClaudeGo 更新记录

**中文** | [English](changelog.en.md) · 返回 [README](../README.md)

## 2026-07（v0.10 线，开源首推之后的 56 个提交）

上一次公开推送（PR #1，审核分流）之后的主要变化，按主题归并：

**可观测性 —— 从"状态反推"改成"事件为准"**

- **事件账本（CG-2）**：新增 per-task `events.jsonl`，状态迁移点单写点收敛（不新增写者），归档时同步搬运。
  Web 看板的活动流改读事件流，**不再由当前状态反推历史**（反推会伪造出从未发生过的时间线）；
  账本缺口（崩溃残尾、旧任务无账本）在 UI 上**显式披露**而不是静默补全。
- **幂等墓碑（CG-4）**：per-task `tombstones/<id>.json` 保证续接/回收/release 三处注入**至多一次**，
  `bound=2` 挡住崩溃-重启风暴；每任务墓碑锁 + 两阶段临界区，`emit` 先于 `saveTask` 消灭"零披露"窗口。
- **Web 看板（`claudego board`）**：只读看板落地——项目/阶段/任务三层总览 + kanban + 额度燃尽曲线，
  模型等级分类色、阶段介绍脱离折叠、默认全展开；`board.json` 静态快照机械化为目标锚定进度（CG-8），
  `goal_source` 按实际入账打标、合成层加非有限数守护、人工 `done_percent` 越界判"数据不足"而非直渲负百分比。

**稳态运行 —— 失败不再变成静默卡死**

- **失败分类分流（CG-3）**：认证/权限失败直接 `held`（重试只会烧额度）、输入超长直接 `failed`、
  未知类兜底 `retry_backoff`；`classifyFailure` **只吃结果 msg、拒 transcript 污染**，
  `isLimitHit` 三向收敛挡住自审仓 transcript 的误命中。
- **drain 内巡逻（CG-5）**：drain 期间独立巡逻，**两个独立信号**才判卡死，marker 文件见证真实退出码；
  心跳不再独立触发、阈值随配置伸缩。
- **单实例锁**：`acquireLock` / `acquireEventLock` 原子挂名，防双持锁。
- **codex 可靠性**：结果在手早收割（远端结果已回、ssh 被孙进程吊住时两拍击杀，不再空挂到超时）；
  限额识别补 session limit 措辞与**跨天重置**解析；超轮限升级卡继承被审卡 `remote_host`（否则远端链的升级卡被派回本机 cd 失败）。

**额度 —— 第三个用量源与百分域收口**

- **订阅用量端点直读（CG-1）**：`oauth_usage` 打开后直读 `api.anthropic.com/api/oauth/usage` 作为第三用量源，
  与 CodexBar 两源**分歧时取最保守值**；只信响应 body、**拒解析响应头**（易被中间层伪造）。
- **百分域语义收口（CG-1b）**：`utilization` / `used_percent` / `percent` 一律按 **0-100 百分域原样取整**，
  任一自动归一都是假触线温床；落在 `(0,1]` 的取值判为**刻度歧义**、拒判为"数据不足"，`>100` 同拒。
- **凭据硬隔离**：`oauth_usage_creds_path` 非空时只读该文件，不再兜底 `~/.claude` / keychain。

**审核分流 —— 镜像可信与沙箱收窄**

- **review-sync 工作树洞根修（CG-R1/CG-R2）**：sync 补送**未提交面**并落 fingerprint，
  自门拦住"镜像过期"空转（此前远端会对着旧镜像出一份看似正常的审核报告）。
- **codex 复审沙箱（CG-R3/CG-R3b）**：只读分析卡默认建**一次性隔离副本 + `--sandbox workspace-write`**，
  复审得以跑测试、写夹具做动态验证，副本随卡即建即删、原仓永不受写污染；
  **远端**只对位于 `remote_mirror_root` 之下的镜像卡放宽（前缀判据加 `path.Clean` 词法归约，消除 `..` / `.` 逃逸），
  真实业务仓维持 `read-only` 硬保证；`codex_review_sandbox` **取值写错按最小权限回落 `readonly`（fail-closed）**；
  建副本阶段跑在 `min(step_timeout, 10min)` 独立子预算内（拷贝腿每文件边界查预算），
  且**从不打开非常规文件**（FIFO/socket/设备跳过、symlink 按本体复制不跟随——一条指向无写端管道的链接就能让 `open` 永久阻塞、占死整条泳道）。

**其他**

- **交叉验证（`claudego cross`）**：双引擎独立作答 → 对抗式交叉查漏的三卡链，含 codex 失败可靠性根修。
- **卡级 codex 模型钉定**：`-codex-model` 与降级专用 `codex_fallback_model`（档位对等，opus→terra 不降 sol）。
- **文档与测试**：新增 `docs/specs/` 生态对标三件套（landscape 调研 / CG 卡 / 定位与非目标）；
  README 配置键名加 `go test` 校验防文档漂移；mock claude 全状态机验收测试覆盖崩溃残尾与账本缺口注入。
