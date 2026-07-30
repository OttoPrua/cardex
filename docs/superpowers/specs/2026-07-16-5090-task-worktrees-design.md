# 5090 独立任务 Worktree 设计

> 注：产品已于 2026-07-31 更名 cardex（BD-44）；下文 claudego/ClaudeGo 均指现 cardex。

## 目标

把尚未启动及后续适合远端执行的 ClaudeGo 卡放到 win5090 的独立 Git worktree 中运行，允许同一 Mac lane 的任务并行协作，同时保证：

- 不打断已经在 Mac 运行的任务；
- 不覆盖 Mac lane 中已有的未提交改动；
- 每张卡有独立分支、目录和可追溯谱系；
- 远端实现、复审和修复链留在同一 worktree；
- 只有在完整复审链通过后才生成回写产物；
- 回写前验证基线，冲突时 fail closed，不静默覆盖。

批准设计时的四张候选 `gpt-5.6-sol` 卡（`t0716-1843-c2c4`、`t0716-1844-72bc`、`t0716-1844-268c`、`t0716-1846-0097`）在切换前均已由 Mac runner 完成，因此不做倒迁。首批 live pilot 改为后续进入队列、尚未开跑且满足远端执行资格的新卡；优先选择同一 lane 的两张卡验证独立 worktree 并行。已经进入 `running` 或 `done` 的卡始终不迁移、不打断。

## 方案选择

采用“远端 hub + synthetic snapshot + 每卡独立 worktree”。不采用每卡独立 clone，因为重复传输和存储较多；不采用每 lane 共用 worktree，因为它仍会被目录锁串行化，无法实现协作并行。

## 目录与命名

5090：

- hub：`D:/Project/PO-hubs/<lane>.git`
- 快照准备区：`D:/Project/PO-snapshots/<lane>-<timestamp>`
- 任务 worktree：`D:/Project/PO-worktrees/<task-id>`
- 任务分支：`claudego/<task-id>`
- lane 集成分支：`claudego/integrate/<lane>-<timestamp>`
- lane 集成 worktree：`D:/Project/PO-worktrees/integrate-<lane>-<timestamp>`

Mac 编排状态：

- manifest：`~/.claudego/remote-worktrees/<task-id>.json`
- 回写包：`~/.claudego/writeback/<lane>-<timestamp>.patch`
- 冲突与验证日志：`~/.claudego/writeback/<lane>-<timestamp>.log`

## 基线快照

1. 等相关 lane 当前正在运行的 Mac 卡结束；迁移窗口内 hold 该 lane 的其余未启动卡，冻结本地写入面。
2. 从 Mac 生成当前 branch 的 Git bundle。
3. 单独传输 `git diff --binary HEAD` 和真正的 untracked 文件；禁止整树 tar，避免 Windows CRLF 与 macOS `._` 扩展属性制造假改动。
4. 5090 hub 导入 bundle，在快照准备区应用 tracked diff 和 untracked 包。
5. 5090 创建仅用于传输的 synthetic snapshot commit。它不回写 Mac，也不改变用户分支历史。
6. manifest 记录 `mac_head`、`snapshot_commit`、`snapshot_tree`、lane、原任务 ID 和创建时间。

`snapshot_tree` 是冻结基线的内容指纹。后续所有任务 worktree 从同一个 snapshot commit 创建。

## 任务迁移与执行

对每张待迁卡：

1. 在 5090 hub 从 snapshot commit 创建 `claudego/<task-id>` 分支和独立 worktree。
2. 将旧卡复制为 held 新卡，保持 prompt、priority、`fix_round`、`review_of`、权限和 `review_after`；设置：
   - `remote_host=qmthost`
   - `runner_pref=codex`
   - `dir=D:/Project/PO-worktrees/<new-task-id>`
   - Codex 模型沿用配置中的 `gpt-5.6-sol`
3. 核验新旧 prompt 仅允许尾部换行规范化差异，然后取消归档旧卡并 release 新卡。
4. 原业务 prompt 中的 proposal-only / no-commit 纪律保持不变。外围编排 helper 在完整任务谱系通过后创建 transport commit，业务执行器不自行提交，ClaudeGo 核心本轮不增加完成钩子。
5. `review_after` 生成的 fable 审核卡在同一远端 worktree 运行；审核产生的修复链继承该 `remote_host` 和目录。

同一卡谱系在最终 `pass` 前不进入回写阶段。若达到超轮限或出现基础设施失败，保留远端 worktree 并生成 held 人工处理卡。

## 集成与回写

同一 lane 的多张卡可能修改相同账本或日志，因此不把各卡 patch 直接依次应用到 Mac：

1. 每张通过复审的任务分支由编排层创建 transport commit。
2. 从 snapshot commit 建立 lane 集成分支，按原任务 priority、created_at 顺序合并通过分支。
3. 无冲突时自动合并；有冲突时创建一张钉定 `gpt-5.6-sol` 的远端集成卡，只解决冲突并重跑受影响测试。
4. 集成完成后生成 `snapshot_commit..integration_commit` 的单一 binary patch。
5. Mac 用临时 index 计算当前工作树 tree hash，不修改用户 index：
   - 等于 `snapshot_tree`：允许自动应用 patch；
   - 不等：禁止自动应用，生成 held writeback 冲突卡并保留 patch。
6. 应用成功后核对 Mac 与远端 integration tree 内容一致，再清理远端任务 worktree；hub、manifest、日志和 patch 保留用于审计。

## 失败处理

- bundle、diff 或 untracked 同步失败：不创建或不 release 远端卡。
- 远端快照状态与 Mac 状态清单不一致：删除失败快照并重新同步。
- worktree/branch 已存在：要求 manifest 指纹一致才能复用，否则报冲突。
- 远端任务失败：保留 worktree，沿用 ClaudeGo retry/held 机制。
- 复审未通过：修复链继续在原 worktree，不提前回写。
- 集成冲突或 Mac 基线漂移：fail closed，绝不强制覆盖或自动 reset。
- 回写后验证失败：用本次 binary patch 的 reverse apply 回滚刚才的应用；保留远端 integration branch、正向 patch、回滚日志和验证证据。

## 安全边界

- 不对 Mac 用户分支创建 synthetic commit。
- 不运行 `git reset --hard`、`git clean -fd` 或覆盖式复制。
- 临时包不包含 `.git`、凭证、ClaudeGo 数据根或 node_modules。
- 远端任务只访问对应 worktree；任务 ID、branch、snapshot 和回写包形成一一对应的审计链。
- 清理仅针对 manifest 明确登记、且已经完成回写验证的远端 worktree。

## 实现构件

第一阶段使用可审计 helper 落地，不扩张 ClaudeGo 任务 schema：

- Mac helper：负责 freeze、bundle/diff/untracked 打包、manifest、任务复制、状态监控、patch 回收和基线校验。
- 5090 helper：负责 hub 导入、synthetic snapshot、worktree 创建、transport commit、集成和导出。
- 针对首批 live pilot 卡的迁移清单与 old→new 映射；原四张候选只记录为“切换前已完成、未迁移”。

稳定运行后再评估把 manifest/writeback 生命周期收进 ClaudeGo 原生字段和完成钩子；本轮不修改全局 `max_parallel`，也不做通用 `model→host` 路由。

## 验收标准

- 当前运行中的 Mac 卡没有被中断或重提。
- 首批 live pilot 的每张卡都有独立 5090 worktree、branch 和 manifest。
- 新卡实际为 `remote:qmthost` + `gpt-5.6-sol`，谱系字段与原卡一致。
- 两张同 lane 卡可以同时运行，目录互不相同。
- synthetic snapshot 的文件状态与冻结时 Mac 工作树一致，无 CRLF/`._` 噪声。
- 至少一次 dry-run 证明：基线相等时可生成/应用回写；基线漂移时拒绝自动应用。
- 远端冲突不会静默覆盖，能够生成 held 集成或 writeback 卡。
- 最终 Mac tree 与远端 integration tree 内容一致后才允许清理任务 worktree。

## 非目标

- 不迁移已经运行或完成的卡。
- 不自动解决业务语义冲突。
- 不改变 Mac 当前分支历史或强制提交现有脏改动。
- 不在本轮实现全局每主机并发上限。
