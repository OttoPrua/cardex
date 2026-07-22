package main

// 审核分流（实现本机跑、对抗审核分流到第二台主机）回归测试。
// 机制放编排器、策略留派卡方：声明 review_host/review_dir/review_sync，
// 把只读审核负载摊到另一台机器；分流失败一律回退本机审核，闭环不断。

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// findReviewCard 取 postComplete 派出的审核卡（标题前缀「审核: 」）。
func findReviewCard(t *testing.T, root string) *Task {
	t.Helper()
	for _, x := range listQueued(t, root) {
		if strings.HasPrefix(x.Title, "审核: ") {
			return x
		}
	}
	return nil
}

// ① 分流成功：审核卡带审核主机与镜像目录，模板 {{DIR}} 用镜像目录渲染。
func TestReviewDivertToRemoteHost(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	impl := mkImplTask(t, root, cfg)
	impl.ReviewHost = "remotehost"
	impl.ReviewDir = "/remote/mirror/proj"
	if err := saveTask(root, impl); err != nil {
		t.Fatal(err)
	}
	postComplete(root, cfg, impl, &claudeResult{Result: ""}, nil)

	rv := findReviewCard(t, root)
	if rv == nil {
		t.Fatal("review_after 应派出审核卡")
	}
	if rv.RemoteHost != "remotehost" {
		t.Fatalf("审核卡应分流到审核主机, got RemoteHost=%q", rv.RemoteHost)
	}
	if rv.Dir != "/remote/mirror/proj" {
		t.Fatalf("审核卡 Dir 应为镜像目录, got %q", rv.Dir)
	}
	if !strings.Contains(rv.Prompts[0], "/remote/mirror/proj") {
		t.Fatalf("审核模板 {{DIR}} 应渲染镜像目录\n----\n%s", rv.Prompts[0])
	}
	// 分流审核卡必须继承实现卡模型(非空)——否则 runTask 远端分支会把空 Model 的审核路由到
	// 烧 GPT 额度的远端 codex,击穿"平衡两侧 claude 额度"的立项目标。
	if rv.Model != impl.Model {
		t.Fatalf("分流审核卡应继承实现卡模型 %q, got %q", impl.Model, rv.Model)
	}
}

// P0-1 类闭合：远端 typeReview 默认命中远端 claude；但 runner_pref=codex 是用户显式钉定，
// 必须优先于 type/model 默认值。remoteUsesClaude 是 runTask 远端分支的选择判据。
func TestRemoteReviewRoutesToClaudeNotCodex(t *testing.T) {
	// 空 Model 的远程审核卡:必须走远端 claude(平衡 claude 额度),绝不走烧 GPT 额度的远端 codex。
	if !remoteUsesClaude(&Task{Type: typeReview, RemoteHost: "remotehost"}) {
		t.Fatal("空 Model 的远程审核卡应走远端 claude，而非远端 codex")
	}
	// 反例守卫:无模型的填充类远程卡仍应走远端 codex(不能把该分支也误强制成 claude)。
	if remoteUsesClaude(&Task{Type: typeSequence, RemoteHost: "remotehost"}) {
		t.Fatal("无模型的填充类远程卡应走远端 codex")
	}
	// 带模型的远程卡走远端 claude。
	if !remoteUsesClaude(&Task{Type: typeSequence, Model: "opus", RemoteHost: "h"}) {
		t.Fatal("带模型的远程卡应走远端 claude")
	}
	// 显式 runner_pref=codex 必须覆盖审核类型和由 type_defaults 继承来的 fable 模型。
	// 这是远端 Sol 复审卡的真实形态：若不覆盖，会名义用 Codex、实际继续烧 Fable。
	if remoteUsesClaude(&Task{
		Type:         typeReview,
		Model:        "claude-fable-5",
		PreferRunner: "codex",
		CodexModel:   "gpt-5.6-sol",
		RemoteHost:   "remotehost",
	}) {
		t.Fatal("显式 runner_pref=codex 的远端审核卡应走远端 codex")
	}
}

// P1-1 类闭合：实现卡 Model 为空(典型远端 codex 实现卡)+ config type_defaults.review.model 非空(opus)时,
// 分流审核卡不得被无条件覆写抹成空串——须保留 newTask 从 typeDefaults 烘焙的审核模型,否则远端审核落到
// 账号默认模型、质量静默降级(相对 origin/main 的回归)。杀的突变:'if reviewHost!="" { rv.Model = t.Model }'。
func TestReviewDivertKeepsConfigReviewModelWhenImplModelEmpty(t *testing.T) {
	root := testRoot(t)
	// 审核类型默认模型 opus;实现类型无默认模型(远端 codex 实现卡 Model 为空)。
	cfg := &Config{TypeDefaults: map[string]TypeDefaults{
		typeReview: {Model: "opus"},
	}}
	impl := newTask(root, cfg, typeSequence, "实现 任务甲", "/tmp/proj", []string{"实现甲"}, 7)
	impl.ReviewAfter = true
	impl.ReviewHost = "remotehost"
	impl.ReviewDir = "/remote/mirror/proj"
	if impl.Model != "" {
		t.Fatalf("前提:实现卡 Model 应为空(typeDefaults.sequence 无 model), got %q", impl.Model)
	}
	if err := saveTask(root, impl); err != nil {
		t.Fatal(err)
	}
	postComplete(root, cfg, impl, &claudeResult{Result: ""}, nil)

	rv := findReviewCard(t, root)
	if rv == nil {
		t.Fatal("review_after 应派出审核卡")
	}
	if rv.RemoteHost != "remotehost" {
		t.Fatalf("审核卡应分流到审核主机, got RemoteHost=%q", rv.RemoteHost)
	}
	if rv.Model != "opus" {
		t.Fatalf("空实现卡 Model 不得抹掉 typeDefaults.review 烘焙的审核模型 opus, got %q", rv.Model)
	}
}

// P1-2 类闭合：runReviewSync 的同步命令必须以实现卡 Dir 为工作目录执行——本库其余本地执行器均 cmd.Dir=t.Dir。
// 未钉 cmd.Dir 时命令在 daemon 进程 cwd 跑,用户写相对路径命令(rsync ./ ...)会静默同步启动目录。
// 用哨兵文件+相对路径直击该陷阱:只有 cmd.Dir==t.Dir 时 'cat sentinel.txt' 才命中;并用 pwd -P 二次钉住。
func TestReviewSyncRunsInTaskDir(t *testing.T) {
	root := testRoot(t)
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "sentinel.txt"), []byte("MARK"), 0o644); err != nil {
		t.Fatal(err)
	}
	impl := &Task{ID: "impl-sync-dir", Dir: work, ReviewSync: "pwd -P && cat sentinel.txt"}
	lg, err := os.Create(taskLogPath(root, impl.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	// cmd.Dir 未钉时命令在 daemon cwd 跑,相对路径读不到哨兵文件 → cat 失败 → runReviewSync 返回非 nil。
	if err := runReviewSync(impl, lg); err != nil {
		t.Fatalf("sync 命令应在实现卡 Dir 执行、相对路径命中哨兵文件, got err=%v", err)
	}
	logData, err := os.ReadFile(taskLogPath(root, impl.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "MARK") {
		t.Fatalf("sync 应在 t.Dir 执行(相对路径读到哨兵文件), 日志缺 MARK\n----\n%s", logData)
	}
	// pwd -P 二次钉住:子进程物理 cwd 必须等于实现卡 Dir(解符号链接后比对,macOS /var→/private/var)。
	wantDir, err := filepath.EvalSymlinks(work)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), wantDir) {
		t.Fatalf("sync 子进程 pwd 应为 t.Dir %q, 日志未见\n----\n%s", wantDir, logData)
	}
}

// P1-1：远程实现卡(-host hostA)+分流(-review-host hostB)+sync 失败：回退不得抹掉 t.RemoteHost。
// 应回到 hostA 的 t.Dir 跑(远程卡远程审),而非被拉回本机以 hostA 的远端路径当本地目录(必失败)。
func TestReviewSyncFailKeepsRemoteImplHost(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	impl := mkImplTask(t, root, cfg)
	impl.RemoteHost = "hosta"
	impl.ReviewHost = "hostb"
	impl.ReviewDir = "/remote/b/mirror"
	impl.ReviewSync = "exit 7" // 同步命令必失败
	if err := saveTask(root, impl); err != nil {
		t.Fatal(err)
	}
	lg, err := os.Create(taskLogPath(root, impl.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	postComplete(root, cfg, impl, &claudeResult{Result: ""}, lg)

	rv := findReviewCard(t, root)
	if rv == nil {
		t.Fatal("同步失败也应派出审核卡")
	}
	if rv.RemoteHost != "hosta" {
		t.Fatalf("sync 失败回退应保留实现卡远程主机 hosta(远程卡远程审), got %q", rv.RemoteHost)
	}
	if rv.Dir != impl.Dir {
		t.Fatalf("回退审核卡 Dir 应为实现卡 Dir %q, got %q", impl.Dir, rv.Dir)
	}
}

// P1-2：runReviewSync 的 sync 命令派生的孙进程握住 stdout 管道时,不得吊死 runner——
// setupProcGroup 的 WaitDelay(10s)必须强制收尾。构造:sh 立即 exit 0,但后台 sleep 继承管道写端。
// 未打防线时 Wait 会阻塞到 sleep 结束(此处 40s);有 WaitDelay 时 ~10s 内返回。
func TestReviewSyncDoesNotHangOnPipeHoldingChild(t *testing.T) {
	root := testRoot(t)
	impl := &Task{ID: "impl-sync-hang", Dir: "/tmp", ReviewSync: "sleep 40 & exit 0"}
	lg, err := os.Create(taskLogPath(root, impl.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	done := make(chan struct{})
	var syncErr error
	start := time.Now()
	go func() {
		syncErr = runReviewSync(impl, lg)
		close(done)
	}()
	select {
	case <-done:
		if el := time.Since(start); el > 25*time.Second {
			t.Fatalf("runReviewSync 被吊住管道的孙进程拖住 %v(应由 WaitDelay 在 ~10s 内收尾)", el)
		}
		// 核心断言：sh 已 exit 0,仅孙进程吊管道触发 WaitDelay(ErrWaitDelay)——须被 rescueWaitDelay
		// 救回 nil。否则成功的同步被误判失败→postComplete 收掉 divert、每轮回退本机,分流静默失效。
		// 杀的突变:删掉 runReviewSync 里的 err = rescueWaitDelay(err, cmd)。
		if syncErr != nil {
			t.Fatalf("sh exit 0、仅孙进程吊管道时 runReviewSync 应救援为 nil, got %v", syncErr)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("runReviewSync 被吊住管道的孙进程卡死(未生效 WaitDelay/进程组击杀)")
	}
}

// P1-1 类闭合（助手直测）：rescueWaitDelay 是三处本地执行器共用的救援。构造进程组 + WaitDelay 下
// sh 立即 exit 0、后台 sleep 吊住 stdout 管道 → cmd.Wait 返回 ErrWaitDelay 但 ProcessState.Success()。
// 断言 rescueWaitDelay 救回 nil。杀的突变:救援返回原 err。（Success() 守卫按 Go 契约在
// ErrWaitDelay 场景恒真——该错误仅在进程 exit 0 后等管道超时才产生,删守卫测试仍绿,不作杀测靶。）
func TestRescueWaitDelayOnPipeHoldingChild(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "sleep 20 & exit 0")
	setupProcGroup(cmd)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	start := time.Now()
	raw := runCmdRegistered(cmd)
	if el := time.Since(start); el > 18*time.Second {
		t.Fatalf("WaitDelay 应在 ~10s 内收尾, 实际 %v", el)
	}
	// 前提:未救援时确应是 ErrWaitDelay(否则本测试构造无效、无法证伪救援)。
	if !errors.Is(raw, exec.ErrWaitDelay) {
		t.Fatalf("前提:吊管道的孙进程应让 Wait 返回 ErrWaitDelay, got %v", raw)
	}
	if got := rescueWaitDelay(raw, cmd); got != nil {
		t.Fatalf("进程 exit 0(Success())时 rescueWaitDelay 应救回 nil, got %v", got)
	}
	// 反例守卫:真正的非零退出(Success()=false)不得被救援吞掉。
	bad := exec.CommandContext(context.Background(), "sh", "-c", "exit 5")
	setupProcGroup(bad)
	badErr := runCmdRegistered(bad)
	if rescueWaitDelay(badErr, bad) == nil {
		t.Fatal("非零退出不得被 rescueWaitDelay 救成 nil")
	}
}

// P1-2 类闭合：type_defaults.review.model=opus + 实现卡 -model haiku -review-host 时,分流审核卡的
// 模型必须保留烘焙的 opus(配置的审核模型),绝不被实现卡的 haiku 覆写(否则本地审核用 opus、远程审核
// 用 haiku,配置审核模型被静默降级)。杀的突变:'if reviewHost!="" && t.Model!="" { rv.Model = t.Model }'。
func TestReviewDivertKeepsConfigReviewModelOverImplModel(t *testing.T) {
	root := testRoot(t)
	cfg := &Config{TypeDefaults: map[string]TypeDefaults{
		typeReview: {Model: "opus"},
	}}
	impl := newTask(root, cfg, typeSequence, "实现 任务甲", "/tmp/proj", []string{"实现甲"}, 7)
	impl.ReviewAfter = true
	impl.Model = "haiku" // 实现卡显式模型:非空
	impl.ReviewHost = "remotehost"
	impl.ReviewDir = "/remote/mirror/proj"
	if err := saveTask(root, impl); err != nil {
		t.Fatal(err)
	}
	postComplete(root, cfg, impl, &claudeResult{Result: ""}, nil)

	rv := findReviewCard(t, root)
	if rv == nil {
		t.Fatal("review_after 应派出审核卡")
	}
	if rv.Model != "opus" {
		t.Fatalf("非空烘焙的审核模型 opus 不得被实现卡 haiku 覆写, got %q", rv.Model)
	}
}

// ② ReviewSync 失败：回退本机审核（Dir=实现卡 Dir、无 RemoteHost），且任务日志含回退记录。
func TestReviewSyncFailFallsBackLocal(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	impl := mkImplTask(t, root, cfg)
	impl.ReviewHost = "remotehost"
	impl.ReviewDir = "/remote/mirror/proj"
	impl.ReviewSync = "exit 3" // 同步命令必失败
	if err := saveTask(root, impl); err != nil {
		t.Fatal(err)
	}
	lg, err := os.Create(taskLogPath(root, impl.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	postComplete(root, cfg, impl, &claudeResult{Result: ""}, lg)

	rv := findReviewCard(t, root)
	if rv == nil {
		t.Fatal("同步失败也应派出审核卡（回退本机）")
	}
	if rv.RemoteHost != "" {
		t.Fatalf("回退本机审核不应带 RemoteHost, got %q", rv.RemoteHost)
	}
	if rv.Dir != impl.Dir {
		t.Fatalf("回退本机审核 Dir 应为实现卡 Dir %q, got %q", impl.Dir, rv.Dir)
	}
	logData, err := os.ReadFile(taskLogPath(root, impl.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "回退本地审核") {
		t.Fatalf("任务日志应含回退记录\n----\n%s", logData)
	}
}

// ③ concerns → 修复卡完整继承分流三字段（下一轮审核继续分流）。
func TestReviewDivertInheritsThroughFix(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg()
	impl := mkImplTask(t, root, cfg)
	impl.ReviewHost = "remotehost"
	impl.ReviewDir = "/remote/mirror/proj"
	impl.ReviewSync = "rsync-placeholder"
	if err := saveTask(root, impl); err != nil {
		t.Fatal(err)
	}
	rv := mkReviewTask(t, root, cfg, impl)
	handleReviewVerdict(root, cfg, rv, reviewReport, nil) // concerns → 修复R1

	var fix *Task
	for _, x := range listQueued(t, root) {
		if strings.HasPrefix(x.Title, "修复R1: ") {
			fix = x
		}
	}
	if fix == nil {
		t.Fatal("concerns 应自动入队修复R1卡")
	}
	if fix.ReviewHost != "remotehost" || fix.ReviewDir != "/remote/mirror/proj" || fix.ReviewSync != "rsync-placeholder" {
		t.Fatalf("修复卡应继承分流三字段: %+v", fix)
	}
}

// ④ 超轮限：held 升级卡 Dir 用实现卡 Dir，而非（远程审核的）镜像目录。
func TestReviewDivertEscalationUsesOrigDir(t *testing.T) {
	root := testRoot(t)
	cfg := testCfg() // MaxFixRounds=0 → 默认 3
	impl := mkImplTask(t, root, cfg)
	impl.Title = "修复R3: 实现 任务甲 [concerns:0P0+1P1]"
	impl.FixRound = 3
	if err := saveTask(root, impl); err != nil {
		t.Fatal(err)
	}
	rv := mkReviewTask(t, root, cfg, impl)
	rv.Dir = "/remote/mirror/proj" // 远程审核卡的 Dir 是镜像路径
	if err := saveTask(root, rv); err != nil {
		t.Fatal(err)
	}
	handleReviewVerdict(root, cfg, rv, reviewReport, nil)

	var esc *Task
	for _, x := range listQueued(t, root) {
		if strings.Contains(x.Title, "超轮限") {
			esc = x
		}
	}
	if esc == nil {
		t.Fatal("R4 超默认轮限应挂升级卡")
	}
	if esc.Dir != impl.Dir {
		t.Fatalf("升级卡 Dir 应为实现卡 Dir %q（非审核镜像 %q）, got %q", impl.Dir, rv.Dir, esc.Dir)
	}
}

// ⑤ -review-host 指向未配置主机 → add 报错。
func TestAddReviewHostUnconfiguredErrors(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgJSON := `{"remote_hosts":{"remotehost":{}}}`
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	err := cmdAdd([]string{
		"-root", root,
		"-dir", work,
		"-review-host", "unknownhost",
		"-review-dir", "/remote/mirror/proj",
		"跑个任务",
	})
	if err == nil {
		t.Fatal("-review-host 指向未配置主机应报错")
	}
	if !strings.Contains(err.Error(), "未配置审核主机") {
		t.Fatalf("错误信息应指明未配置审核主机, got: %v", err)
	}
}

// ⑤b -review-host 与 -review-dir 未成对 → add 报错。
func TestAddReviewHostDirMustPair(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"remote_hosts":{"remotehost":{}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	err := cmdAdd([]string{
		"-root", root,
		"-dir", work,
		"-review-host", "remotehost",
		"跑个任务",
	})
	if err == nil || !strings.Contains(err.Error(), "成对") {
		t.Fatalf("-review-host 缺 -review-dir 应报成对错误, got: %v", err)
	}
}
