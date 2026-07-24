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
// 前缀 `.claudego-` 与 CG-R2 fingerprint(.claudego-fingerprint)保持家族一致,便于人工识别。
const codexWorkMarkerName = ".claudego-codex-work.json"

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
	for _, rel := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if rel == "" {
			continue
		}
		srcPath := filepath.Join(src, rel)
		dstPath := filepath.Join(copyDir, rel)
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
			return fmt.Errorf("mkdir dst dir for %s: %w", rel, err)
		}
		if err := copyFile(srcPath, dstPath); err != nil {
			return fmt.Errorf("cp untracked %s: %w", rel, err)
		}
	}
	return nil
}

// copyFile 是保留文件权限的普通 IO 拷贝——untracked 里可能含 shell 脚本,复审若要跑需保留 +x。
// 不做 chown(源与目标同 uid,不需要)。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	// 目录 / socket 等非普通文件:untracked 里不该出现,但硬防御一下,skip 不报错。
	if !fi.Mode().IsRegular() {
		return nil
	}
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

// writeCodexWorkMarker 落 marker 到副本内 <copyDir>/.claudego-codex-work.json。
// 原子写(tmp+rename)避免半截 marker 骗过对账。
func writeCodexWorkMarker(copyDir string, m codexWorkMarker) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(copyDir, codexWorkMarkerName)
	return atomicWrite(path, append(data, '\n'))
}

// readCodexWorkMarker 读副本目录内的 marker;文件不存在/损坏视作"半成品",返回 (nil,false)。
// 对账侧据此把无 marker 的目录也当孤儿清(clone 中崩溃/apply 中崩溃的残留兜底)。
func readCodexWorkMarker(copyDir string) (*codexWorkMarker, bool) {
	data, err := os.ReadFile(filepath.Join(copyDir, codexWorkMarkerName))
	if err != nil {
		return nil, false
	}
	var m codexWorkMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, false
	}
	return &m, true
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
