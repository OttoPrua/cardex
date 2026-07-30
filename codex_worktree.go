package main

// codex_worktree.go —— CG-R3:codex 复审可写沙箱(承 BD-36 工具链③终裁 b / BD-39 附记 2026-07-24)。
//
// 【为什么必须存在】
// BD-36 附记(07-21)立据的悬置问题:runner.go 原本对 codex 复审卡硬编码 --sandbox read-only,
// 复审的多轮动态验证(跑测试/写夹具/生成对照 fixture)只能标 open——弱化了对抗复审的实证能力,
// 逼裁决方"凭静态阅读打 verdict"。委托人 BD-39 裁方案 b:改跑一次性隔离副本+workspace-write,
// 语义等价"绝不污染原仓"。本文件承载副本的创建/清理/崩溃对账三段。
//
// 【副本方案选择:git clone --local --no-hardlinks 而非 git worktree add】
// worktree 效率更高(共享 objects),但会在父仓 .git/worktrees/ 注册元数据,并使副本内 .git 是
// gitdir file(逻辑指回父仓 worktree registry)——一旦 codex 复审在副本内跑 `git commit`/`git gc`
// 等罕见但可能的操作,不慎写到共享 objects/refs 就污染父仓。委托人硬语义"原仓保护",在收益-风险
// 上倾向浅 clone:copy-once,完全独立 .git,副本随删,父仓无任何注册项,反例注入①的断言更硬。
// 缺点是磁盘/时间开销略大;ClaudeGo/中型仓可接受,大仓可回退 readonly。
//
// 【为什么覆盖未提交面】
// 复审卡的语义是"审现在的代码",不是"审 HEAD"。修复卡纪律恰是"不 commit、workspace 待复审"
// (CG-R2 sync-lane 就是为此才补 dirty+untracked 面)——若副本只带 HEAD,复审对着已提交面出
// verdict,复现的正是 CG-1 修复链三轮空转的原病。故 clone 完必再:
//   ① `git diff --binary --no-renames HEAD` → 副本 `git apply --binary`(dirty tracked)
//   ② `git ls-files --others --exclude-standard -z` → 逐一 cp 到副本(真正 untracked,尊重 .gitignore)
//      —— untracked 面是**域外输入**:symlink 按链接本体复制不跟随,非常规文件(FIFO/socket/设备)
//      一律跳过,每条之前查子预算。理由见 copyUntrackedList / copyUntrackedPath 的成因注释(CG-R3b R1)。
// .DS_Store / AppleDouble 等平台伪影不特殊处理:副本一次性存在,伪影随删,不像 CG-R2 那样两侧
// 对称落 fingerprint,无对称问题。
//
// 【为什么把副本放到 <root>/tmp/codex-review-work/ 而非 os.TempDir()】
// os.TempDir() 是 OS 级别的墓地,ClaudeGo 无对账权限,崩溃残留可能与其它系统进程的临时目录混在
// 一起。放到 <root>/tmp/ 下 ClaudeGo 自管,tick 每轮扫这一个目录即可对账清理孤儿。委托人硬语义
// "副本路径必须在原仓目录树之外"由 root 通常就在 ~/.claudego(与业务仓 <root_of_repo>/ 无关)
// 天然满足;测试用 t.TempDir() 时更是明确隔离。
//
// 【为什么带 pid + nano 后缀】
// 同一任务并发跑同一副本理论极少(任务级串行、目录锁),但 tick 崩溃重启后重派同 taskID 有可能
// 与残留副本同名。pid+nano 后缀让每次新建都是新目录,残留归残留、新副本归新副本,清理与执行不
// 抢同名。marker 记录 taskID 便于对账反查"这坨副本属于哪张卡"。
//
// 【孤儿清理判据:pid 死 且 taskID 不在 activeIDs】
// 单条件不够:
//   - 仅"pid 死":同 taskID 已在 tick 内重派、新 pid 已活,清了活着任务的副本;
//   - 仅"taskID 不在 activeIDs":pid 还活着说明 codex 还在跑(可能编排刚死透但子进程尚未清),
//     此时清副本会把跑中数据抹掉。
// 双条件相与:执行器已死透 + 编排上没在跑此卡 → 才是真正的崩溃残留。事件账本落 "reason":
// "codex_review_orphan_cleanup" 留痕;失败不阻断 tick,只 stderr 告警。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// codexPrepareTimeoutCap 是"建副本"阶段的绝对上限。包级 var 而非常量:测试用毫秒级值驱动
// "mock git 永不退出 → 建副本在限时内被击杀"的反例(见 codex_worktree_test.go)。
// 【为什么 10 分钟】建副本全是本机文件系统动作(clone --local 复制 objects、diff、apply、cp),
// 即便 GB 级仓库在 SSD 上也是分钟级;10 分钟是"正常路径绝不会碰到、卡死路径必然会碰到"的分水岭。
var codexPrepareTimeoutCap = 10 * time.Minute

// codexPrepareTimeout 返回建副本阶段的子预算 = min(step_timeout, cap)。
//
// 【为什么建副本必须有超时】CG-R3b 修 2:本文件的 git 子进程原先一律 exec.Command(无 ctx),而
// invokeCodex 的 context.WithTimeout(StepTimeoutMin) 建在建副本之后——大仓 clone 卡死(NFS 停顿、
// git 等凭据输入、锁竞争)不受任何超时约束,runTask 就在 clone 上永久挂住:巡逻此时看到的是"进程组
// 活着",不触发;step 超时根本还没起算。整条泳道被一张卡的建副本阶段无声堵死。
//
// 【为什么给独立子预算而非直接吃 step 的统一预算】只挂父 ctx 确实能兜住"永不退出",但留了个更隐蔽
// 的病:clone 卡死会把整整一个 step 预算(默认 60min)烧光,codex 一秒没跑就判超时,重试再烧一轮。
// 独立短预算让超时即回落 read-only,复审仍能在剩余预算里跑完——降级而非空转。子预算仍派生自传入的
// ctx,父被 patrol/取消击杀时子同步死,统一击杀路径不破。
func codexPrepareTimeout(cfg *Config) time.Duration {
	limit := codexPrepareTimeoutCap
	if cfg == nil {
		return limit
	}
	if step := time.Duration(cfg.StepTimeoutMin) * time.Minute; step > 0 && step < limit {
		return step
	}
	return limit
}

// copyGitStep 描述建副本阶段的一条 git 子命令。label 是人读的阶段名(进错误消息),
// stdin 非空时接到子进程标准输入(仅 git apply 用),args 是 git 之后的完整参数表。
type copyGitStep struct {
	label string
	stdin string
	args  []string
}

// runCopyGit 跑一条建副本用的 git 子进程,把它同时挂到三条路径上:
//   ① ctx —— exec.CommandContext,超时/取消即触发 Cancel;
//   ② 进程组击杀 —— setupProcGroup 让 Cancel 走 killProcGroup(整组 SIGKILL),并设 WaitDelay(10s),
//      使 git 派生的孙进程吊住管道时 Wait 仍能收尾(与 invokeClaude/invokeCodex/runReviewSync 同源);
//   ③ 在册登记 —— runCmdRegisteredForTask 让 Ctrl-C/SIGTERM 处理器连坐击杀,并让巡逻在建副本期间
//      看得见该卡的活进程(否则建副本这段时间任务进程组是"空的",是巡逻误判的素材)。
// 裸 exec.Command 三条全无,这正是 CG-R3b 修 2 要闭掉的洞。
func runCopyGit(ctx context.Context, taskID string, s copyGitStep) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, "git", s.args...)
	setupProcGroup(cmd)
	if s.stdin != "" {
		cmd.Stdin = strings.NewReader(s.stdin)
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	runErr := runCmdRegisteredForTask(cmd, taskID)
	if ctxErr := ctx.Err(); ctxErr != nil {
		// ctx 到期/被取消 → 进程已被 Cancel 整组收掉,runErr 只是"signal: killed"这类下游噪声。
		// 统一改报 ctx 错并 %w 包装:上层 errors.Is(err, context.DeadlineExceeded) 才能把"卡死被击杀"
		// 与"git 真失败"分成两个事件 reason,审计时不混。
		return out.Bytes(), errBuf.Bytes(), fmt.Errorf("git %s 被超时/取消击杀: %w", s.label, ctxErr)
	}
	return out.Bytes(), errBuf.Bytes(), runErr
}

// codexWorkRoot 返回本机 codex 复审副本的根目录。<root>/tmp/codex-review-work/
// 使用 root 之下的固定子目录:tick 只扫这一处对账清理,不与 os.TempDir() 的其它临时目录混淆。
func codexWorkRoot(root string) string { return filepath.Join(root, "tmp", "codex-review-work") }

// codexWorkMarkerName 是每个副本目录内的元信息文件名,含 taskID/pid/created_at/src。
const codexWorkMarkerName = ".cardex-codex-work.json"

// legacyCodexWorkMarkerName 是 BD-44 改名前的 marker 名,**只读不写**。
//
// 【为什么必须留】marker 是崩溃残留副本的唯一身份凭据。改名当天 <root>/tmp/codex-review-work/
// 下完全可能躺着上一版二进制留下的副本,里面是旧名 marker。若对账只认新名,这些残留会被判成
// "marker 缺失 → 半成品",走 5 分钟 mtime 兜底删除——听着也删掉了,但删的路径丢了 TaskID:
// 事件账本记的是 reason=no_marker_or_corrupt + 空 taskID(落进 codex-review-orphans 兜底桶),
// 人事后回查"这坨副本原属哪张卡"就没了线索。更坏的是活任务分支:旧名 marker 读不出 TaskID/PID,
// activeIDs 与 processAlive 两道保护全部失效,一个**正在跑**的旧版副本只要建成超过 5 分钟就会被
// 新版 tick 当半成品删掉——直接毁掉在跑任务的执行数据。
//
// 【什么时候能删掉这行】<root>/tmp/codex-review-work/ 被清空一次(或确认没有旧版二进制在跑)之后。
const legacyCodexWorkMarkerName = ".claudego-codex-work.json"

// codexWorkMarker 是副本的元信息载体。清理侧只需 TaskID(反查 activeIDs)与 PID(processAlive
// 判活),Src/CreatedAt 便于人工审计"这坨副本原属谁、什么时候建的"。
type codexWorkMarker struct {
	TaskID    string `json:"task_id"`
	PID       int    `json:"pid"`
	Src       string `json:"src"`
	CreatedAt string `json:"created_at"`
}

// codexReviewNeedsWorktree 判断本次 codex 调用是否应该走一次性副本沙箱。
// 条件:
//   ① 归一后的 CodexReviewSandbox == worktree-write(默认;显式 readonly 就直接回退旧行为);
//   ② 任务非 sequence——sequence 卡本就要落码到原仓(commit 等),不建副本;
//   ③ 目标 dir 是 git 工作树(clone --local 依赖 .git)。
// 三条件缺一即 false;后续调用侧就走 read-only 老路(硬语义:原仓永不受写污染的兜底)。
// ctx 一路透传到 ③ 的 git 探测:该探测同样会因仓库/文件系统异常挂住,不能是无约束的裸子进程。
func codexReviewNeedsWorktree(ctx context.Context, cfg *Config, t *Task) bool {
	if !codexReviewWantsWorktree(cfg, t) {
		return false
	}
	// 探测挂建副本的同一条子预算:探测卡死等价于建副本卡死,不该按 step 的 60min 量级去等。
	probeCtx, cancel := context.WithTimeout(ctx, codexPrepareTimeout(cfg))
	defer cancel()
	return isGitWorkTree(probeCtx, t.Dir)
}

// codexReviewWantsWorktree 是上面判据里**不起子进程**的前两条(策略 + 卡类型)。
// 【为什么必须单独抽出来】第三条 git 探测会起子进程、会被 ctx 击杀,而击杀后的返回值 false 与
// "这压根不是 git 工作树"完全同形。调用侧若不能把两者分开,一次超时就被静默记成"本卡不需要副本":
// 无错误、无事件、无 stderr,降级彻底隐身——正是 CG-R3b 修 2 要消灭的那类无声失败,只是换了个入口。
// 有了这个谓词,prepare 才能只在"策略本来就想要副本"时把 ctx 死亡当错误报出去。
func codexReviewWantsWorktree(cfg *Config, t *Task) bool {
	if resolvedCodexReviewSandbox(cfg) != codexReviewSandboxWorktreeWrite {
		return false
	}
	return t != nil && t.Type != typeSequence
}

// isGitWorkTree 用 `git -C <dir> rev-parse --show-toplevel` 侦测 dir 是否在 git 工作树里。
// 非零退出/非目录/ctx 到期都视作 false(clone --local 会直接失败,提前判掉更清晰;探测都挂住的
// 目录更不可能 clone 成功,判 false 回落 read-only 正是保守侧)。
func isGitWorkTree(ctx context.Context, dir string) bool {
	if dir == "" {
		return false
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return false
	}
	// taskID 传空:探测不属于任何一次 invoke 的执行期,不该进 taskPG 影响巡逻的"活/死"判断;
	// runCopyGit 内的 procGroups 登记仍在,Ctrl-C 连坐击杀不漏。
	_, _, err := runCopyGit(ctx, "", copyGitStep{
		label: "rev-parse --show-toplevel",
		args:  []string{"-C", dir, "rev-parse", "--show-toplevel"},
	})
	return err == nil
}

// prepareCodexReviewWorkspace 为一张 codex 复审卡建一次性副本。
// 返回:
//   - workDir: 副本绝对路径(codex --sandbox workspace-write 将 cd 到此)。失败时回落 t.Dir。
//   - cleanup: 收工必调,幂等(多次调用无害)。删除副本目录 + marker。
//   - err:     建副本过程的错误(git clone/apply/cp)。调用侧据此决定回落 read-only 还是报错;
//               当前策略是"建失败即回落 read-only,原仓保护语义不破,只是失去 workspace-write 收益"。
//
// 【为什么 clone --local --no-hardlinks】--local 用文件系统副本(不走 git 协议解析),避开 file://
// 的一些边角;--no-hardlinks 让 objects 完全独立,避免副本 gc/commit 反噬父仓 pack。--quiet 抑制
// clone 大段进度输出,日志纯净。--depth 1 会丢历史,若复审需 git log 会伪造盲区,不加。
//
// 【为什么用 git apply 而非 patch 命令】git apply 认 --binary,能吃 CG-R2 sync 已验证的
// `git diff --binary --no-renames HEAD` 输出;system patch 对二进制/无换行末尾等边角坑更多。
// diff 为空(HEAD 干净)时不 apply,避免 apply 因 stdin 空转报"unexpected EOF"。
//
// 【ctx 的作用】整个建副本阶段(探测 + clone + diff + apply + ls-files)跑在 min(step_timeout, 10min)
// 的子预算内,超时/父取消即整组击杀并回落 read-only——见 codexPrepareTimeout 的成因注释。
func prepareCodexReviewWorkspace(ctx context.Context, root string, cfg *Config, t *Task) (string, func(), error) {
	noop := func() {}
	// 子预算在 needsWorktree 探测之前建:探测本身也是 git 子进程,同样要受限时约束。
	ctx, cancel := context.WithTimeout(ctx, codexPrepareTimeout(cfg))
	defer cancel()
	if !codexReviewNeedsWorktree(ctx, cfg, t) {
		// 探测被 ctx 击杀时 needsWorktree 也返回 false,与"不是 git 工作树"同形(见
		// codexReviewWantsWorktree 注释)。只有在策略本来就想要副本时才把 ctx 死亡改报错误——
		// 否则 readonly/sequence 这些"本就不建副本"的正常回落会被误报成降级、白落一条事件。
		if ctxErr := ctx.Err(); ctxErr != nil && codexReviewWantsWorktree(cfg, t) {
			return t.Dir, noop, fmt.Errorf("建副本前置探测被超时/取消击杀: %w", ctxErr)
		}
		return t.Dir, noop, nil
	}
	src, err := filepath.Abs(t.Dir)
	if err != nil {
		return t.Dir, noop, fmt.Errorf("resolve src abs: %w", err)
	}
	workRoot := codexWorkRoot(root)
	if err := os.MkdirAll(workRoot, 0o755); err != nil {
		return t.Dir, noop, fmt.Errorf("mkdir work root: %w", err)
	}
	// 硬语义护栏:副本路径必须在原仓目录树之外(即 workRoot 不能是 src 的子孙,反之亦然)。
	// 若 root 恰好被人配到业务仓内部,或 src=/(极端),提前 abort,避免副本落进原仓被 codex 认作
	// 原仓的一部分。EvalSymlinks 消解 symlink 陷阱(macOS /var → /private/var 等)。
	srcReal, _ := filepath.EvalSymlinks(src)
	workRootReal, _ := filepath.EvalSymlinks(workRoot)
	if srcReal == "" {
		srcReal = src
	}
	if workRootReal == "" {
		workRootReal = workRoot
	}
	if pathContains(srcReal, workRootReal) || pathContains(workRootReal, srcReal) {
		return t.Dir, noop, fmt.Errorf("副本路径 %s 与原仓 %s 存在包含关系,拒绝建立(原仓保护硬语义)",
			workRootReal, srcReal)
	}

	// 目录名 = <taskID>-<pid>-<nano>:pid+nano 让并发/重启不撞名;taskID 首字段方便人工识别归属。
	// os.MkdirTemp 会保证唯一性,把 pattern 里 * 替换为随机后缀——再加上前缀的 pid/nano 冗余,残留
	// 与新副本永不撞。0o700 防止其它用户读到复审中的临时代码。
	prefix := fmt.Sprintf("%s-%d-%d-", t.ID, os.Getpid(), time.Now().UnixNano())
	copyDir, err := os.MkdirTemp(workRoot, prefix+"*")
	if err != nil {
		return t.Dir, noop, fmt.Errorf("mktemp copy dir: %w", err)
	}
	// mkdir 是把目录建到 workRoot/prefix<rand>/;但接下来的 clone --local <src> <copy> 要求
	// 目标目录不存在(或空)。清掉刚 mktemp 建的空目录,让 clone 自己建同名新目录。
	if err := os.Remove(copyDir); err != nil {
		return t.Dir, noop, fmt.Errorf("prep copy dir for clone: %w", err)
	}

	// 收工/失败清理闭包:多次调用幂等;移除副本目录 + 兜底(即便 marker 写入失败也能被后续对账清)。
	cleanup := func() {
		_ = os.RemoveAll(copyDir)
	}

	// 三段 git 全部走 runCopyGit(ctx + 进程组击杀 + 在册登记);%w 包装保住 ctx 错的类型,
	// 让调用侧能按 errors.Is(context.DeadlineExceeded) 区分"卡死被击杀"与"git 真失败"。
	if _, errOut, err := runCopyGit(ctx, t.ID, copyGitStep{
		label: "clone --local",
		args:  []string{"clone", "--local", "--no-hardlinks", "--quiet", src, copyDir},
	}); err != nil {
		cleanup()
		return t.Dir, noop, fmt.Errorf("git clone --local 建副本失败: %w\n%s", err, errOut)
	}

	// 应用未提交面:先 tracked dirty patch,再 untracked cp。任一失败即 cleanup+报错。
	if err := applyUncommittedTracked(ctx, t.ID, src, copyDir); err != nil {
		cleanup()
		return t.Dir, noop, fmt.Errorf("apply dirty patch to copy: %w", err)
	}
	if err := copyUntracked(ctx, t.ID, src, copyDir); err != nil {
		cleanup()
		return t.Dir, noop, fmt.Errorf("copy untracked to copy: %w", err)
	}

	// 写 marker——最后一步,marker 存在即证明副本"建齐了";崩溃对账见 marker 才当孤儿处理,
	// 缺 marker 的半成品(clone 中崩溃/apply 中崩溃)由 cleanup() 上面兜底路径清理。
	marker := codexWorkMarker{
		TaskID:    t.ID,
		PID:       os.Getpid(),
		Src:       src,
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	if err := writeCodexWorkMarker(copyDir, marker); err != nil {
		cleanup()
		return t.Dir, noop, fmt.Errorf("write marker: %w", err)
	}

	return copyDir, cleanup, nil
}

// applyUncommittedTracked 把源仓当前的 `git diff --binary --no-renames HEAD`(tracked 未提交面)
// 灌进副本的 `git apply --binary`。diff 为空(工作树干净)则跳过,不当错误。
func applyUncommittedTracked(ctx context.Context, taskID, src, copyDir string) error {
	patch, _, err := runCopyGit(ctx, taskID, copyGitStep{
		label: "diff HEAD",
		args: []string{"-c", "core.quotepath=false", "-C", src,
			"diff", "--binary", "--no-renames", "HEAD"},
	})
	if err != nil {
		return fmt.Errorf("git diff HEAD: %w", err)
	}
	if len(patch) == 0 {
		return nil
	}
	_, errOut, err := runCopyGit(ctx, taskID, copyGitStep{
		label: "apply",
		stdin: string(patch),
		args:  []string{"-C", copyDir, "apply", "--binary", "--whitespace=nowarn"},
	})
	if err != nil {
		return fmt.Errorf("git apply on copy: %w\n%s", err, errOut)
	}
	return nil
}

// copyUntracked 用 `git ls-files --others --exclude-standard -z` 列出真正 untracked(尊重 .gitignore)
// 后逐一 cp 到副本。空目录不特殊处理:git 不追踪空目录,复审用不到。
// -c core.quotepath=false 让中文文件名以真实 UTF-8 传出(否则 git 默认 8 进制引号化,后续 stat 找不到)。
func copyUntracked(ctx context.Context, taskID, src, copyDir string) error {
	out, _, err := runCopyGit(ctx, taskID, copyGitStep{
		label: "ls-files --others",
		args: []string{"-c", "core.quotepath=false", "-C", src,
			"ls-files", "--others", "--exclude-standard", "-z"},
	})
	if err != nil {
		return fmt.Errorf("git ls-files --others: %w", err)
	}
	if len(out) == 0 {
		return nil
	}
	return copyUntrackedList(ctx, src, copyDir, strings.Split(strings.TrimRight(string(out), "\x00"), "\x00"))
}

// copyUntrackedList 把 rels(相对 src 的 untracked 路径表)逐条投影到副本。
//
// 【为什么从 copyUntracked 里抽出来】拷贝腿是建副本阶段唯一的**纯 Go 循环**:git 腿靠
// exec.CommandContext 吃 ctx,循环靠自己查。抽成独立函数后测试能直接喂"路径表 + 受控 ctx",
// 精确绑定"每次迭代前查、到期即止"这条契约,而不必依赖 git 子进程的时序去逼出中断点。
//
// 【为什么每次迭代前查 ctx.Err()(CG-R3b R1·P1-1①)】旧实现签名里收了 ctx、循环体一次不查:
//   - 超大 untracked 面(未忽略的 node_modules 之流)会让 min(step_timeout,10min) 子预算**静默失效**
//     ——循环照跑到底,谁也不喊停,README 承诺的"拷贝跑在子预算内"当场作废;
//   - NFS 停顿等慢 IO 场景同理:父 ctx 早死透了,拷贝腿还在一条一条搬。
// 查在**迭代前**而非迭代后:ctx 已死时一条都不该再搬(多搬一条就是多一份无谓 IO 与磁盘占用)。
// 【粒度的诚实边界】Go 的文件 IO 不接 ctx,故中止粒度是**单文件边界**:正在 io.Copy 的那一条会
// 读完才停。真正会永久挂住的非常规文件已在 copyUntrackedPath 里被挡在 open 之外,剩下的普通文件
// 即便在慢盘上也是有限时间收敛——README 契约句按这个粒度写,不超卖。
func copyUntrackedList(ctx context.Context, src, copyDir string, rels []string) error {
	for i, rel := range rels {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("拷贝 untracked 面被超时/取消中止(已处理 %d/%d 条): %w", i, len(rels), err)
		}
		if rel == "" {
			continue
		}
		// marker 命名空间归 cardex:副本根下的 .cardex-codex-work.json(及 atomicWrite 的 .tmp
		// 中间文件)是崩溃对账的凭据,不接受来自业务仓 untracked 面的同名投影。不跳的话,一个名叫
		// `.cardex-codex-work.json.tmp` 的 symlink→FIFO 会让随后 writeCodexWorkMarker 的
		// os.WriteFile **以写端打开无读端 FIFO**——同样永久阻塞,与本卡要闭的病同类只是换了个方向;
		// 且该名字的普通文件会在崩溃残留里冒充 marker 干扰孤儿判据。反正它下一步就会被真 marker 覆盖。
		//
		// 【旧名同样拦】readCodexWorkMarker 在新名缺失时会回落读旧名(见那里的过渡期说明),所以业务仓
		// 里一个名叫 .claudego-codex-work.json 的文件投影进来同样能冒充 marker——命名空间保护必须
		// 覆盖被读的全部名字,否则兼容读就成了新开的注入口。
		if rel == codexWorkMarkerName || rel == codexWorkMarkerName+".tmp" ||
			rel == legacyCodexWorkMarkerName || rel == legacyCodexWorkMarkerName+".tmp" {
			continue
		}
		srcPath := filepath.Join(src, rel)
		dstPath := filepath.Join(copyDir, rel)
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
			return fmt.Errorf("mkdir dst dir for %s: %w", rel, err)
		}
		if err := copyUntrackedPath(srcPath, dstPath); err != nil {
			return fmt.Errorf("cp untracked %s: %w", rel, err)
		}
	}
	return nil
}

// copyUntrackedPath 把一条 untracked 路径投影到副本,保留权限位(untracked 里可能是 shell 脚本,
// 复审要跑得留 +x);不做 chown(源与目标同 uid)。三条硬约束:
//
//	① **先 Lstat 判型,绝不先开**(CG-R3b R1·P1-1②)。旧实现 os.Open 在前、IsRegular 在后:
//	   实测 `git ls-files --others --exclude-standard` 会把 untracked **symlink** 列出来
//	   (dangling / 指向 FIFO 的都列;裸 FIFO 反而不列),而 os.Open 跟随链接打开无写端 FIFO
//	   按 POSIX 永久阻塞——阻塞发生在 IsRegular 防御**之前**,且是纯 Go syscall,ctx/进程组击杀/
//	   patrol 全都解不开,runTask goroutine 与泳道槽位就此占死。Lstat 不跟随链接,判型不碰 open。
//	② **symlink 复制链接本体**(os.Readlink + os.Symlink,不跟随)。这与 git clone --local 对
//	   tracked symlink 的处理同构(实测:clone 原样重建链接,含指向仓外的绝对链接),副本因此在
//	   "链接"这件事上 tracked/untracked 两侧语义一致;顺带闭合"dangling symlink → open 报 ENOENT
//	   → 整个 prepare 必败 → 该仓每次复审都无谓降级 read-only"的兄弟洞。
//	③ **open 后 fstat 复核**(openRegularFileNoBlock 内):Lstat 判型到 open 之间的 TOCTOU 缝由
//	   O_NONBLOCK + fstat 收口,两道互不依赖。
//
// 【留档·非本卡裁量】链接本体复制意味着副本内可能存在指向原仓的 symlink。这不是本修法引入的新面:
// tracked symlink 早已由 clone 原样带过去(已实测),写穿与否的实际闸门在 codex 沙箱侧
// (workspace-write 按真实路径授权 writable_roots)。若要在 ClaudeGo 侧再加一道"越界链接过滤",
// 必须 tracked/untracked 一视同仁,属独立设计决策,不在本卡私自扩围。
func copyUntrackedPath(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		// 副本是全新目录,dst 冲突理论上不发生(tracked 与 untracked 路径互斥);删一次让重建幂等。
		// os.Remove 删的是链接本身,不跟随,不会误删链接目标。
		_ = os.Remove(dst)
		return os.Symlink(target, dst)
	}
	if !fi.Mode().IsRegular() {
		// FIFO/socket/设备/目录:git 不会列出裸 FIFO,但 untracked 面是域外输入,硬防御一下。
		// 跳过而非报错——为一条不该存在的特殊文件让整个 prepare 失败,只会换来无谓的全局降级。
		return nil
	}
	in, _, err := openRegularFileNoBlock(src)
	if err != nil {
		if errors.Is(err, errNotRegularFile) {
			return nil // TOCTOU:Lstat 之后被换成非常规文件,与 ② 同样跳过
		}
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}

// writeCodexWorkMarker 落 marker 到副本内 <copyDir>/.cardex-codex-work.json。
// 原子写(tmp+rename)避免半截 marker 骗过对账。
//
// 【为什么先 Remove 两条路径(CG-R3b R1 类闭合)】副本内容部分来自业务仓 untracked 面,是域外输入。
// 若那边有个同名的 symlink→FIFO 被投影进来,atomicWrite 的 os.WriteFile 会**以写端打开无读端 FIFO**
// ——同样按 POSIX 永久阻塞,只是方向从读换成了写。copyUntrackedList 已在源头跳过该命名空间;这里再
// 删一次是第二道:os.Remove 删链接本身不跟随,对正常路径(文件本不存在)是无副作用的 no-op。
func writeCodexWorkMarker(copyDir string, m codexWorkMarker) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(copyDir, codexWorkMarkerName)
	_ = os.Remove(path)
	_ = os.Remove(path + ".tmp") // atomicWrite 的中间文件名,同样不能是别人留下的管道
	return atomicWrite(path, append(data, '\n'))
}

// readCodexWorkMarker 读副本目录内的 marker;文件不存在/损坏/非普通文件视作"半成品",返回 (nil,false)。
// 对账侧据此把无 marker 的目录也当孤儿清(clone 中崩溃/apply 中崩溃的残留兜底)。
//
// 【BD-44 过渡期:新名缺失时回落旧名】改名前建的副本里躺的是 .claudego-codex-work.json。
// 只认新名的话,那些残留(乃至旧版二进制正在用的活副本)会退化成"marker 缺失"分支——见
// legacyCodexWorkMarkerName 处的完整说明。回落只发生在**新名读不出来**时,新名永远优先。
//
// 【为什么走 readRegularFileNoBlock(CG-R3b R1 类闭合)】本函数由 tick 的孤儿对账调用,读的是
// **副本目录**——其内容部分来自业务仓 untracked 面(域外)。崩溃点若落在"拷贝完成、marker 未写"之间,
// 残留里就可能有个名叫 .cardex-codex-work.json 的 symlink→FIFO:os.ReadFile 会在此永久阻塞,
// 把 tick 整条对账线程占死。改用不阻塞的读法后,它被判成"损坏 marker"→ 按半成品清掉,正是我们要的。
// 旧名回落走同一个读法,同样不会阻塞。
func readCodexWorkMarker(copyDir string) (*codexWorkMarker, bool) {
	for _, name := range []string{codexWorkMarkerName, legacyCodexWorkMarkerName} {
		data, err := readRegularFileNoBlock(filepath.Join(copyDir, name))
		if err != nil {
			continue
		}
		var m codexWorkMarker
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		return &m, true
	}
	return nil, false
}

// cleanupCodexReviewOrphans 扫 <root>/tmp/codex-review-work/,把崩溃残留副本清掉。
// 判据("孤儿"的双条件):
//   ① marker 缺失/损坏 → 半成品,直接清(建到一半就崩,活任务不会用它);
//   ② marker 存在但 pid 已死透 且 taskID 不在当前 activeIDs → 崩溃残留,清。
// pid 活着或 taskID 在 activeIDs 里就跳过——那可能是活任务的副本,清了就毁执行数据。
// 事件账本落 reason=codex_review_orphan_cleanup 留痕,失败仅 stderr 告警不阻断 tick。
func cleanupCodexReviewOrphans(root string, activeIDs map[string]bool) {
	workRoot := codexWorkRoot(root)
	entries, err := os.ReadDir(workRoot)
	if err != nil {
		// 目录不存在是正常路径(还没建过任何副本);其它错(权限)只告警不返回。
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "警告: 扫描 codex 副本对账目录失败 %s: %v\n", workRoot, err)
		}
		return
	}
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		copyDir := filepath.Join(workRoot, ent.Name())
		marker, ok := readCodexWorkMarker(copyDir)
		if !ok {
			// 半成品(marker 缺失/损坏):按 mtime 老于 5 分钟才清,避免误清正在建的副本。
			// 5 分钟窗口足以覆盖 clone+apply+cp 的最长期望,同时孤儿也不会长期占盘。
			if fi, err := os.Stat(copyDir); err == nil && time.Since(fi.ModTime()) > 5*time.Minute {
				removeCodexReviewCopy(root, copyDir, "no_marker_or_corrupt", "")
			}
			continue
		}
		if activeIDs[marker.TaskID] {
			continue // 当前有活任务,不动
		}
		if marker.PID > 0 && processAlive(marker.PID) {
			continue // 建副本的进程还活着(可能是长跑同 taskID 的另一实例),不动
		}
		removeCodexReviewCopy(root, copyDir, "codex_review_orphan_cleanup", marker.TaskID)
	}
}

// removeCodexReviewCopy 删除单个副本目录并落事件。事件绑到 taskID(空 taskID 用兜底桶
// "codex-review-orphans"避免 emit 空 taskID 早返回)。删除失败仅告警不 return,防单个坏目录卡死清理。
func removeCodexReviewCopy(root, copyDir, reason, taskID string) {
	if err := os.RemoveAll(copyDir); err != nil {
		fmt.Fprintf(os.Stderr, "警告: 清理 codex 副本目录失败 %s: %v\n", copyDir, err)
		return
	}
	if taskID == "" {
		taskID = "codex-review-orphans"
	}
	emitTaskEvent(root, taskID, evStalled, "runner:codex_review_cleanup", statusRunning, 0, map[string]any{
		"reason":   reason,
		"copy_dir": copyDir,
	})
}

// pathContains 判断 parent 是否包含或等于 child(路径级)。用于原仓保护硬语义护栏——
// 副本目录不能在原仓目录树内、原仓也不能在副本目录树内。EvalSymlinks 消解 macOS
// /var → /private/var 一类符号链接陷阱由 caller 完成。
func pathContains(parent, child string) bool {
	if parent == "" || child == "" {
		return false
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	// filepath.Rel("/a", "/a/b") == "b"(不含 ..) ⇒ 是子孙;含 ".." ⇒ 不是子孙。
	return !strings.HasPrefix(rel, "..")
}
