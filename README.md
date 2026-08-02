# cardex

**中文** | [English](README.en.md)

[![LINUX DO](https://img.shields.io/badge/LINUX%20DO-社区分享-ffb003?logo=discourse&logoColor=white)](https://linux.do)

> 本项目原名 ClaudeGo，2026-07-31 更名为 cardex（旧命令名 `claudego` 仍可通过兼容软链继续用，见下方「快速开始」）。

**把 Claude 订阅的 5 小时限额窗口榨干。** 本地任务队列 + 调度器：任务撞到限额自动挂起并记下重置时间，到点自动 `--resume` 接回**同一个会话**继续干。单个 Go 二进制，无外部依赖，**编排本身不花一分额度**。

```bash
cardex add -title "重构鉴权" -dir ~/Projects/myapp -file steps.md   # 把活丢进队列
cardex install-launchd                                            # 之后它自己跑
```

## 能办到什么

| 你的处境 | cardex 做的事 |
|---|---|
| 限额一撞，人就得守着等窗口重开 | 自动挂起 + 记住重置时刻，到点自动续跑同一会话——**你睡觉时队列在跑** |
| 一个目标要手写十几段 prompt | `assemble`：让 Claude 先调研项目，再把目标拆成 prompt 序列**自动入队** |
| 几个会话并行干活，进度只在脑子里 | `brief` 回收结构化进度，`plan` 读实时队列做分工、分工任务自动入队 |
| 什么活都用最贵的模型 | 按任务路由模型：机械活 haiku、常规实现 sonnet、高风险最强档；贵模型只做编排与仲裁 |
| 冷却期彻底停摆 | 冷却期把单步编排卡改道 `codex exec`（走另一份额度），管线不断档 |
| 手里还有 Kimi/GLM/MiniMax 这些订阅在吃灰 | 引擎档案一键接入（`cardex engines add kimi`）：钉定主跑或纳入自定义降级链，每家独立冷却独立记账，统一能力分级排序 |
| 改完还得自己盯着审 | 完成后自动派对抗式审核卡；只读审核还能分流到第二台机器跑 |
| “现在到底跑到哪了” | `cardex list` + Web 看板（kanban / 额度燃尽 / 落地进度）+ 每张卡的事件账本 |

## 快速开始

```bash
make build && make install     # 编译并装到 /opt/homebrew/bin
cardex init                    # 初始化 ~/.cardex（数据目录可用 CARDEX_ROOT 覆盖；旧变量名 CLAUDEGO_ROOT 仍兼容读一次并提示）
cardex doctor                  # 自检：claude CLI、目录、配置
```

旧名 `claudego` 命令仍要保留：`make install install-shim` 会额外铺一条 `claudego → cardex` 的兼容软链（`ln -sf`，跟随二进制升级），过渡期用，改名收尾后可移除。

三种最常用的入队方式，挑一个开始：

```bash
# ① 步骤已经想清楚了：steps.md 里用单独一行 --- 分隔每一步
cardex add -title "重构鉴权" -dir ~/Projects/myapp -priority 5 -review-after -file steps.md

# ② 只有一个目标：让 Claude 先调研，再自动生成任务序列入队
cardex assemble -dir ~/Projects/myapp "给上传模块加断点续传，含测试"

# ③ 手上的会话刚被限额打断：直接接管续跑（会话 id 用 cardex sessions 查）
cardex adopt <session-id> -dir ~/Projects/myapp
```

让它自己跑起来：

```bash
cardex run                   # 先手动跑一轮验证
cardex install-launchd       # 后台调度：每 5 分钟 tick 一次，开机自启（macOS）
cardex list                  # 看板：标题列＝「标题 ▸ 最新进度」
cardex log <id>              # 某张卡的细节；cmd <id> 打印手动接管命令
cardex board                 # Web 看板 http://127.0.0.1:8787
```

非 macOS：`cardex daemon` 前台常驻，或让 systemd timer / cron / Windows 任务计划程序每 5 分钟拉一次 `cardex run`。核心是纯 Go，三大平台都能编译；单实例锁已跨平台，定时并发不会撞车。

## 五种任务类型

| 类型 | 用途 | 默认权限 / 模型 |
|---|---|---|
| `design-review` | 设计审核 session：只读审查代码/架构，产出 P0/P1/P2 分级报告 | 只读工具 + git log/diff |
| `prompt-assembly` | prompt 装配 session：调研项目后把目标拆成 prompt 序列，**产出的任务自动入队** | 只读工具 |
| `sequence` | 预设 prompt 序列：多个步骤在同一个会话中依次执行（`--resume` 串联，上下文连续） | acceptEdits + 常用构建/测试命令 |
| `coordinate` | 分工协调 session：读**实时**队列快照 + 各会话进度报告，把目标拆成分工任务（含模型建议）自动入队 | 只读工具，默认 opus |
| `progress-pull` | 进度回收 session：`--resume` 某个会话，让它输出结构化进度报告并落盘 | 只读工具，默认 haiku |

任务可以链式衔接：`assemble`（装配）→ 产出 `sequence` 入队 → 执行完成 → `review_after` 自动入队一个 `design-review` 审查刚才的改动。

**桌面端也在管辖范围内**：Claude Code 桌面端与 CLI 共用 `~/.claude/projects` 会话存储和订阅额度，所以桌面端里开的会话同样可以被列出、回收进度、`--resume` 接管。

## 怎么做到的

```
                 ┌──────────────────────────────────────────────┐
   cardex add    │                    任务队列 (~/.cardex/tasks)  │
   assemble ────▶│  queued ──▶ running ──▶ done                 │
   review  plan  │               │  └──▶ failed（退避重试后）      │
   adopt  brief  │               ▼                              │
                 │         limit_paused ──(到达重置时间)──┐        │
                 └───────────────────────────────────┼────────┘
                                                     │
   launchd / daemon 每 5 分钟 tick ──▶ 派发规则选一个任务 ─┘
                                      │
                                      ▼
                    claude -p --model <模型> --resume <会话> ...
```

四件事撑起整个循环：

1. **编排零额度**——调度器是纯 Go 本地代码，只读写 `~/.cardex` 下的 JSON 文件，自己不调用任何模型；派发、退避、看板、账本一律不花额度，只有任务真正执行时才 `claude -p`。
2. **限额是可恢复状态，不是失败**——撞限额时从错误里解析重置时间戳，写全局冷却（`cooldown.json`），期间一个探测调用都不发；到点后向**同一个会话**发续跑提示，从中断处接着做，不重发原 prompt、不重复劳动。
3. **派发有序、落盘即安全**——续跑优先 →`priority` 大者优先 → 类型顺序（审核便宜先跑，装配会派生新工作放最后）→ 同级 FIFO。每一步成功立刻原子写盘，进程被杀不丢进度；单实例锁保证 launchd 多次触发不并发。
4. **失败分类而非盲目重试**——认证/权限失败直接 `held` 等人处理，输入超长直接 `failed`（同 prompt 再送必然再超长），判不出来的才走退避重试，不把额度打进注定失败的重试里。

## 更进一步

跑顺之后按需要挑，每条在[进阶指南](docs/guide.md)里都有完整说明：

- **[进度回收 → 分工协调 → 自动推进](docs/guide.md#进度回收--分工协调--自动推进)**——多会话并行时的编排闭环：回收进度 → 协调任务读实时队列做分工 → 自动逐个推进，随时接管。
- **[文件化状态与人工把关](docs/guide.md#文件化状态fresh_steps与人工把关-hold)**——状态放文件里、每步开新会话，永不撞上下文上限；`-hold` 让分工产出先挂起，人工审完再放行。
- **[审核分流](docs/guide.md#审核分流把只读审核负载摊到第二台机器)**——实现在本机跑、对抗审核改到第二台机器跑，平衡两侧额度；同步失败自动回落本机，闭环不断。
- **[交叉验证](docs/guide.md#交叉验证fable-顶替双引擎独立作答--对抗式交叉查漏)**——两个不同引擎对同一问题独立作答，再对抗式交叉查漏；设计档模型撞周限额时的顶替方案。
- **[Web 看板](docs/guide.md#web-看板board-命令)**——项目横排 kanban + **剩余**额度燃尽曲线 + 按「设计/落地/修复/审核」拆分的进度 + 目标锚定的「落地进度」+ 进度双口径（现有卡 / 含预估余量，计划锚点或历史膨胀率，口径全披露），数据不足一律显式披露，绝不编造估算；对队列数据只读（唯一写入是看板自己的项目折叠状态）。
- **[5 小时额度红线](docs/guide.md#5-小时额度红线保底额度)**——给突发/交互任务留余量：本地账本 + CodexBar 用量源 + 订阅端点三通道，分歧时取最保守值；支持分时段红线。
- **[Codex 备用执行器](docs/guide.md#codex-备用执行器限额空窗不断档)**——claude 冷却期把单步编排卡切给 codex；设计档模型钉定不降级，交叉验证的引擎独立性不被偷换。
- **[多订阅引擎](docs/guide.md#多订阅引擎engine-profileskimi--glm--minimax--mimo--opencode-go--ollama-cloud)**——Kimi Code / GLM Coding Plan / MiniMax / 小米 MiMo / OpenCode Go / Ollama Cloud 订阅经引擎档案接入：复用 claude CLI + 环境注入，独立冷却、独立记账、统一能力分级（评测源锚定 Claude 各档），降级顺序自定义。
- **[存量角色会话的接管](docs/guide.md#存量角色会话的接管此前手动维护的-审核装配执行-session)**——手工养的审核/装配/执行 session 按角色收编进队列。

想知道异常路径上到底怎么处理的（派发规则全文、限额恢复、失败分类、卡死巡逻、事件账本、幂等墓碑、权限边界）→ [运行时内核](docs/internals.md)。

## 配置速查（~/.cardex/config.json）

常用键，全量表见[配置参考](docs/config.md)：

| 键 | 默认 | 说明 |
|---|---|---|
| `poll_interval_sec` | 300 | launchd/daemon 轮询间隔 |
| `limit_fallback_min` | 30 | 解析不到重置时间时的等待 |
| `step_timeout_min` | 60 | 单步硬超时（防跑飞） |
| `max_attempts_per_step` | 3 | 单步失败重试上限 |
| `retry_backoff_min` | 5 | 非限额错误的重试退避基数（分钟） |
| `resume_first` | true | 被打断任务优先续跑 |
| `type_order` | 进度回收>协调>审核>序列>装配 | 同优先级时的类型顺序 |
| `type_defaults.*.model` | 协调 opus；回收 haiku | 各类型默认模型（--model 值），空用账号默认 |
| `max_parallel` | 1 | 单次 tick 并行任务数（写类任务同目录串行，只读类型豁免） |
| `queue_budget_tokens` 等 | 0（关） | 5 小时额度红线，见[进阶指南](docs/guide.md#5-小时额度红线保底额度) |
| `no_fallback_models` | ["claude-fable-5","fable"] | 这些设计档模型冷却期不降级 codex，宁可排队等 claude |
| `codex_bin` / `codex_fallback` | 空 / false | 冷却期备用执行器，见[进阶指南](docs/guide.md#codex-备用执行器限额空窗不断档) |
| `codex_fallback_model` | "" | claude 卡降级到 codex 时用此模型（档位对等：opus→terra，不降 sol）；空回退 `codex_model` |
| `engines` | {}（空） | 多订阅引擎档案（Kimi/GLM/MiniMax/MiMo/OpenCode Go/Ollama Cloud），`cardex engines add <名>` 并入预设，见[进阶指南](docs/guide.md#多订阅引擎engine-profileskimi--glm--minimax--mimo--opencode-go--ollama-cloud) |
| `fallback_order` | ["codex"] | claude 冷却/红线时的改道顺序（codex 与引擎名混排，质量地板对全链生效） |
| `model_tiers` | {}（空） | 自定义分级表（模型→档位，优先于内置标准线）：无更强模型的机队按牌面定档 |
| `default_review_host` / `remote_mirror_root` / `default_review_sync` | "" | 审核分流三件套：三键齐备时本地实现卡的自动审核默认分流到远端 |
| `remote_hosts.<name>.codex_only` | false | 主机级额度硬边界：为 true 时该远端只运行 Codex，自动审核也不会调用 Claude |

提示词模板在 `~/.cardex/templates/*.md`，可直接修改（`{{GOAL}}` `{{DIR}}` `{{FOCUS}}` 会被替换；`coordinate.md` 里的 `{{QUEUE}}` `{{PROGRESS}}` 在**派发时**替换为实时快照）。

**权限默认收紧**：任务默认**不**使用 `--dangerously-skip-permissions`——审核/装配是只读工具白名单，`sequence` 默认 `acceptEdits` + 常用构建测试命令白名单。需要完全自主时对单张卡加 `-skip-permissions`，详见[运行时内核 · 权限与安全](docs/internals.md#权限与安全)。

## 文档

| 文档 | 内容 |
|---|---|
| [进阶指南](docs/guide.md) | 分工协调闭环、文件化状态、审核分流、交叉验证、Web 看板、额度红线、codex 备用执行器 |
| [运行时内核](docs/internals.md) | 派发规则、限额恢复、失败分类、卡死巡逻、事件账本、幂等墓碑、权限与安全 |
| [配置参考](docs/config.md) | `~/.cardex/config.json` 全量键表 + 模板说明 |
| [更新记录](docs/changelog.md) | 按主题归并的版本变化 |

## 测试

```bash
make test   # mock claude 跑完整状态机：调度/限额暂停/冷却/续跑/装配入队/失败退避/模型路由/进度回收/分工协调
```

## 许可

[PolyForm Noncommercial 1.0.0](LICENSE) —— **个人随便用，商用需授权**。

- **无需联系，直接用**：个人自用、学习研究、业余项目，以及慈善 / 教育 / 公共研究机构的使用。
- **需要授权**：在公司内部用于生产或交付工作、用它承接付费项目、把它或其衍生品作为产品或
  服务的一部分提供给他人。开个 [issue](https://github.com/OttoPrua/cardex/issues) 说明用途即可。

注意两点：这不是 OSI 定义的开源许可证（它对商业用途有限制），别按开源依赖直接引入贵司的
合规清单；2026-08-03 之前发布的版本（截至提交 `b1ed92b`）仍是 MIT，那部分授权不可撤销，
本次变更只对其后的版本生效。详见 [LICENSE](LICENSE)。

## 致谢

本项目在 [LINUX DO](https://linux.do) 社区分享，感谢社区佬友的反馈。
