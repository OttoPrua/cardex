package main

import (
	"strings"
	"testing"
	"time"
)

func codexPrimaryTestConfig() *Config {
	return &Config{
		DefaultRunner: "codex",
		CodexBin:      "codex",
		CodexModel:    "gpt-5.6-sol",
		CodexTierModels: map[string]string{
			"fable": "gpt-5.6-sol", "opus": "gpt-5.6-sol",
			"sonnet": "gpt-5.6-luna", "haiku": "gpt-5.6-luna",
		},
		CodexTierReasoning: map[string]string{
			"fable": "max", "opus": "xhigh", "sonnet": "max", "haiku": "xhigh",
		},
		TypeDefaults: map[string]TypeDefaults{
			typeSequence: {Model: "sonnet", Effort: "xhigh"},
			typeReview:   {Model: "claude-opus-5", Effort: "max"},
		},
	}
}

func TestGeneratedChildUsesLatestConfigWithoutChangingParentRun(t *testing.T) {
	root := testRoot(t)
	oldCfg := codexPrimaryTestConfig()
	oldCfg.TypeDefaults[typeReview] = TypeDefaults{Model: "claude-fable-5", Effort: "high"}

	latest := defaultConfig("")
	latest.DefaultRunner = "codex"
	latest.CodexBin = "codex"
	if err := saveConfig(root, latest); err != nil {
		t.Fatal(err)
	}

	impl := newTask(root, oldCfg, typeSequence, "长任务", t.TempDir(), []string{"实现"}, 1)
	impl.ReviewAfter = true
	if err := saveTask(root, impl); err != nil {
		t.Fatal(err)
	}

	postComplete(root, oldCfg, impl, &claudeResult{Result: "done"}, nil)
	review := findReviewCard(t, root)
	if review == nil {
		t.Fatal("review_after 应生成审核卡")
	}
	if review.Model != "claude-opus-5" || review.Effort != "max" || review.PreferRunner != "codex" {
		t.Fatalf("新子卡必须读取磁盘上的最新独立 Sol/max/Codex 策略，而非父任务旧快照: %+v", review)
	}
}

func TestDefaultRunnerCodexBakedIntoNewTask(t *testing.T) {
	cfg := codexPrimaryTestConfig()
	task := newTask(testRoot(t), cfg, typeReview, "审核", t.TempDir(), []string{"审"}, 1)

	if task.PreferRunner != "codex" {
		t.Fatalf("default_runner=codex 应烘焙到新卡, got %q", task.PreferRunner)
	}
	if remoteUsesClaude(task) {
		t.Fatal("默认 Codex 审核卡不得命中 Claude")
	}
	if got := resolveCodexModel(cfg, task); got != "gpt-5.6-sol" {
		t.Fatalf("Opus 审核应映射 Sol, got %q", got)
	}
	if got := resolveCodexReasoning(cfg, task); got != "xhigh" {
		t.Fatalf("通用模式的 Opus 审核映射应保持 xhigh；Owner 强制模式由独立审核 resolver 提升到 max, got %q", got)
	}
}

func TestDefaultRunnerCodexCoversAutomaticReview(t *testing.T) {
	root := testRoot(t)
	cfg := codexPrimaryTestConfig()
	impl := newTask(root, cfg, typeSequence, "实现", t.TempDir(), []string{"实现"}, 1)
	impl.ReviewAfter = true
	impl.ReviewHost = "qmthost"
	impl.ReviewDir = "D:/review/repo"
	if err := saveTask(root, impl); err != nil {
		t.Fatal(err)
	}

	postComplete(root, cfg, impl, &claudeResult{Result: "done"}, nil)

	review := findReviewCard(t, root)
	if review == nil {
		t.Fatal("review_after 应生成审核卡")
	}
	if review.PreferRunner != "codex" || remoteUsesClaude(review) {
		t.Fatalf("自动审核必须沿用全局 Codex 主路由: %+v", review)
	}
	if review.Model != "claude-opus-5" || resolveCodexModel(cfg, review) != "gpt-5.6-sol" {
		t.Fatalf("自动审核应保留 Opus 档并映射 Sol: %+v", review)
	}
}

func TestLegacyReviewCardWithReviewAfterDoesNotSpawnReviewOfReview(t *testing.T) {
	root := testRoot(t)
	cfg := codexPrimaryTestConfig()
	review := newTask(root, cfg, typeReview, "人工独立审核", t.TempDir(), []string{"审核"}, 1)
	// 模拟升级前已经错误入队的存量卡：运行期护栏必须兜住，不能只保护新入队卡。
	review.ReviewAfter = true
	if err := saveTask(root, review); err != nil {
		t.Fatal(err)
	}

	postComplete(root, cfg, review, &claudeResult{Result: "done"}, nil)

	if child := findReviewCard(t, root); child != nil {
		t.Fatalf("存量审核卡不得生成二次审核: %+v", child)
	}
}

func TestDefaultRunnerCodexCoversAutomaticFix(t *testing.T) {
	root := testRoot(t)
	cfg := codexPrimaryTestConfig()
	orig := newTask(root, cfg, typeSequence, "实现", t.TempDir(), []string{"实现"}, 1)
	orig.MaxFixRounds = 2
	if err := saveTask(root, orig); err != nil {
		t.Fatal(err)
	}
	review := &Task{ID: "review", Type: typeReview, ReviewOf: orig.ID}
	report := "```json\n{\"verdict\":\"block\",\"summary\":\"需修\",\"p0\":[],\"p1\":[\"修复一处\"]}\n```"

	handleReviewVerdict(root, cfg, review, report, nil)

	tasks, err := loadTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	var fix *Task
	for _, task := range tasks {
		if strings.HasPrefix(task.Title, "修复R1:") {
			fix = task
			break
		}
	}
	if fix == nil {
		t.Fatal("block 应自动生成修复卡")
	}
	if fix.PreferRunner != "codex" || fix.SessionID != "" {
		t.Fatalf("自动修复必须是无 Claude 会话的 Codex 卡: %+v", fix)
	}
	if resolveCodexModel(cfg, fix) != "gpt-5.6-luna" || resolveCodexReasoning(cfg, fix) != "max" {
		t.Fatalf("普通实现修复应映射 Luna/max: %+v", fix)
	}
}

func TestSessionResumeOverridesOnlyDefaultCodexRunner(t *testing.T) {
	cfg := codexPrimaryTestConfig()
	task := newTask(testRoot(t), cfg, typeSequence, "续跑", t.TempDir(), []string{"继续"}, 1)
	task.SessionID = "claude-session"
	preserveSessionRunner(task)

	if task.PreferRunner != "" {
		t.Fatalf("显式 Claude session 不可被默认 Codex 接管, got %q", task.PreferRunner)
	}
}

func TestSessionResumeOverridesOnlyDefaultAntigravityRunner(t *testing.T) {
	cfg := codexPrimaryTestConfig()
	cfg.DefaultRunner = antigravityRunnerName
	cfg.AntigravityBin = "agy"
	cfg.Antigravity = &AntigravityRoute{Enabled: true, Effort: "high"}
	task := newTask(testRoot(t), cfg, typeSequence, "续跑", t.TempDir(), []string{"继续"}, 1)
	task.SessionID = "claude-session"
	preserveSessionRunner(task)

	if task.PreferRunner != "" {
		t.Fatalf("显式 Claude session 不可被默认 Antigravity 接管, got %q", task.PreferRunner)
	}
}

func TestLegacyPendingCardsKeepRunnerIdentityThroughReadback(t *testing.T) {
	// P1-6: new defaults reach task bytes only through the explicit creation path. Pre-existing
	// cards — queued, held, limit-paused, failed, or cross/session identities — must pass through
	// load, resolver, and board readback with their runner identity and bytes untouched.
	cfg := codexPrimaryTestConfig()
	cases := []struct {
		name string
		task *Task
	}{
		{"旧排队卡保持无 runner", &Task{Status: statusQueued}},
		{"旧挂起卡保持无 runner", &Task{Status: statusHeld}},
		{"旧限额暂停卡保持无 runner", &Task{Status: statusLimitPaused}},
		{"旧失败卡保持无 runner", &Task{Status: statusFailed}},
		{"在跑卡不改", &Task{Status: statusRunning}},
		{"Claude 会话不改", &Task{Status: statusQueued, SessionID: "s"}},
		{"交叉引擎不改", &Task{Status: statusQueued, XRole: "A", Model: "claude-opus-5"}},
		{"显式 Gemini 不改", &Task{Status: statusQueued, PreferRunner: "gemini"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := tc.task.PreferRunner
			_ = toBrief(cfg, tc.task, time.Now())
			_, _ = resolveOwnerRoute(cfg, tc.task)
			_, _ = resolveOwnerRouteReadback(cfg, tc.task)
			if tc.task.PreferRunner != before {
				t.Fatalf("readback must never rebake a default runner into a pre-existing card: %+v", tc.task)
			}
		})
	}
	// The creation path still stamps the current default on brand-new cards.
	fresh := newTask(testRoot(t), cfg, typeSequence, "新卡", t.TempDir(), []string{"p"}, 1)
	if fresh.PreferRunner != "codex" {
		t.Fatalf("explicit creation path must still bake the current default: %+v", fresh)
	}
}

func TestValidateDefaultRunner(t *testing.T) {
	cfg := &Config{DefaultRunner: " CODEX ", CodexBin: "codex"}
	if err := validateDefaultRunner(cfg); err != nil {
		t.Fatalf("有效 Codex 默认路由不应报错: %v", err)
	}
	if cfg.DefaultRunner != "codex" {
		t.Fatalf("default_runner 应规范化为 codex, got %q", cfg.DefaultRunner)
	}
	if err := validateDefaultRunner(&Config{DefaultRunner: "codex"}); err == nil {
		t.Fatal("缺 codex_bin 时必须 fail fast")
	}
	agy := &Config{
		DefaultRunner:  " AGY ",
		AntigravityBin: "agy",
		Antigravity:    &AntigravityRoute{Enabled: true, Effort: "high"},
	}
	if err := validateDefaultRunner(agy); err != nil {
		t.Fatalf("启用的 agy 默认路由不应报错: %v", err)
	}
	if agy.DefaultRunner != antigravityRunnerName {
		t.Fatalf("default_runner 应规范化为 agy, got %q", agy.DefaultRunner)
	}
	if task := newTask(testRoot(t), agy, typeSequence, "Antigravity", t.TempDir(), []string{"实现"}, 1); task.PreferRunner != antigravityRunnerName {
		t.Fatalf("default_runner=agy 应烘焙到新卡: %+v", task)
	}
	if err := validateDefaultRunner(&Config{DefaultRunner: "agy"}); err == nil {
		t.Fatal("未启用 antigravity 时 default_runner=agy 必须 fail fast")
	}
	if err := validateDefaultRunner(&Config{DefaultRunner: "gemini"}); err == nil {
		t.Fatal("default_runner=gemini 必须因退役而 fail fast")
	}
	if err := validateDefaultRunner(&Config{DefaultRunner: "unknown"}); err == nil {
		t.Fatal("未知 default_runner 必须 fail fast")
	}
}
