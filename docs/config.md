# cardex 配置参考

**中文** | [English](config.en.md) · 返回 [README](../README.md)

## 配置速查（~/.cardex/config.json）

| 键 | 默认 | 说明 |
|---|---|---|
| `poll_interval_sec` | 300 | launchd/daemon 轮询间隔 |
| `limit_fallback_min` | 30 | 解析不到重置时间时的等待 |
| `cooldown_margin_sec` | 90 | 重置时间上再加的安全余量 |
| `step_timeout_min` | 60 | 单步硬超时（防跑飞） |
| `max_attempts_per_step` | 3 | 单步失败重试上限 |
| `retry_backoff_min` | 5 | 非限额错误的重试退避基数（分钟） |
| `resume_first` | true | 被打断任务优先续跑 |
| `type_order` | 进度回收>协调>审核>序列>装配 | 同优先级时的类型顺序 |
| `resume_prompt` | … | 限额中断后的续跑提示词 |
| `type_defaults.*.model` | 协调 opus；回收 haiku | 各类型默认模型（--model 值），空用账号默认 |
| `type_defaults.<类型>` | 见内置表 | 类型默认执行参数。**条目内没写的字段沿用内置同类型的值**：JSON 只按键合并，只写 `{"design-review": {"model": "opus"}}` 会把整条替换成"只有 model"，`allowed_tools` 变空——复审卡的只读工具集就此静默消失。想彻底不下发工具白名单请写 `skip_permissions: true`，不要把 `allowed_tools` 留空（留空与"没写"在 JSON 里不可区分）。整个类型条目缺失则不烘焙任何参数（那是"该类型没配"这另一种意图） |
| `no_fallback_models` | ["claude-fable-5","fable"] | 这些设计档模型冷却期不降级 codex，宁可排队等 claude |
| `thinking_tokens` | 0 | >0 时给 claude 调用设 MAX_THINKING_TOKENS（设计活加大思考预算） |
| `stakes_policy` | low=不复审 / normal=跟随 / high=强制复审+抬 high | 卡级投入产出分档查表（`add -stakes`），**入队即钉到卡面**、运行期不回查；见[进阶指南 · 投入产出分档](guide.md#卡级投入产出分档-stakes--复核深度查表) |
| `retro_every_n_done` | 0（关） | 每 N 张卡进 done 终态自动入队一张 haiku 复盘卡（只读统计、proposal-only）；建议 10，见[进阶指南 · 自动复盘卡](guide.md#自动复盘卡retro_every_n_done) |
| `queue_budget_tokens` 等 | 0（关） | 5 小时额度红线，见[进阶指南 · 额度红线](guide.md#5-小时额度红线保底额度) |
| `oauth_usage` / `oauth_usage_*` | false | 订阅端点直读（第三用量源），端点未文档化——异常按数据不足处理 |
| `max_parallel` | 1 | 单次 tick 并行任务数（写类任务同目录串行；design-review/progress-pull 只读类型豁免，可同仓并发） |
| `codex_bin` / `codex_fallback` | 空 / false | 冷却期备用执行器，见[进阶指南 · Codex 备用执行器](guide.md#codex-备用执行器限额空窗不断档) |
| `codex_fallback_model` | "" | claude 卡降级到 codex 时用此模型（档位对等：opus→terra，不降 sol）；空回退 `codex_model` |
| `codex_reasoning` | "" | 全局 codex 推理档（minimal/low/medium/high/xhigh/max/ultra）→ `-c model_reasoning_effort=…`；任务级 effort 可覆盖 |
| `codex_review_sandbox` | "worktree-write" | codex 只读分析卡(design-review/crosscheck 等)的沙箱策略。默认 `worktree-write`:**本机** codex 建一次性隔离副本 + `--sandbox workspace-write`,复审可跑测试/写夹具做动态验证,副本落 `<root>/tmp/codex-review-work/`,卡结束即删,原仓永不受写污染(CG-R3)。**远端** codex 只对 `t.Dir` 位于 `remote_mirror_root` 之下的镜像卡放宽；默认用 `workspace-write`，若该主机显式配置 `sandbox: "danger-full-access"`（Windows OS sandbox runner 不可用），严格镜像子孙继承该值。交叉/协调/回退等真实业务仓仍维持 `--sandbox read-only` 硬保证。改 `readonly` 全线回落旧行为。**取值写错时按最小权限回落 `readonly`(fail-closed)并在日志披露一次**；键留空/不写才用默认 `worktree-write`。sequence 卡不受此配置影响。 |
| `cross_profiles` | {opus-codex} | 交叉验证引擎对（`cardex cross`），见[进阶指南 · 交叉验证](guide.md#交叉验证fable-顶替双引擎独立作答--对抗式交叉查漏) |
| `default_cross_profile` | "opus-codex" | `cross` 未指定 `-profile` 时用的引擎对 |
| `default_review_host` | "" | 全局默认审核主机（`remote_hosts` 的键）；三键齐备时本地实现卡自动分流，见[进阶指南 · 审核分流](guide.md#审核分流把只读审核负载摊到第二台机器) |
| `remote_mirror_root` | "" | 远端镜像根；与 `default_review_host` 成对，审核目录自动推导为 `<root>/<worktree名>` |
| `default_review_sync` | "" | 全局默认分流前同步命令（sh -c，cwd=实现卡目录）；三键缺一不套默认 |
| `remote_hosts.<name>.codex_only` | false | 为 true 时该主机机械禁止 Claude；带 Claude 模型及自动审核卡均在派发入口改道远端 Codex |

## 看板配置速查（~/.cardex/board.json）

看板**只读**这个文件，永不写它；文件缺失 = 全部走自动推导。

| 键 | 位置 | 说明 |
|---|---|---|
| `projects.<项目id>.name` / `.desc` / `.phases.<阶段名>` | 项目块 | 人工文案覆盖自动推导的项目/阶段介绍 |
| `projects.<项目id>.goal` | 项目块 | 目标锚定的「落地进度」（与卡片进度并列，不替换），见[进阶指南](guide.md#项目覆盖块-cardexboardjson) |
| `projects.<项目id>.kind_rules` | 项目块 | 人工分类规则（标题子串或任务 ID 全串 → kind）；坏规则逐条跳过并挂 `kind_rule_error` |
| `project_aliases` | **顶层** | 有序的「目录 → 项目」归组规则表 `[{"match":"<精确目录或 glob>","title":"<标题子串，可选>","project":"<项目名>"}]`。首条命中即用；归组优先级 **显式(`add -project`) > 别名 > 内建模式 > 目录启发式 > 「未分类」**。改这张表不动任何任务卡，下次快照重建全量追溯生效——这是整理存量野项目的手段。坏规则逐条跳过并挂 `project_alias_error`。详见[进阶指南 · 项目归属](guide.md#项目归属显式--别名--模式--启发式--未分类) |

`match` 不含通配符时是**精确匹配该目录**（防止给容器目录写一条规则就吞掉它下面所有项目）；要覆盖子树写 `X/*`（glob 匹配该目录或其任一祖先，故覆盖任意深度）。大小写不敏感。

提示词模板在 `~/.cardex/templates/*.md`，可直接修改（`{{GOAL}}` `{{DIR}}` `{{FOCUS}}` 会被替换；
`coordinate.md` 里的 `{{QUEUE}}` `{{PROGRESS}}` 在**派发时**替换为实时快照）。
