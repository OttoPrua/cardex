package main

// failure_class_test.go —— CG-3 失败分类分流验收测试。
//
// 每条测试映射到 spec 的一项验收:
//  1) TestClassifyFailure_Enumerations       —— 分类判据枚举:每类必命中,反例(限额相似文本)必判 unknown。
//  2) TestPolicyForFailureClass_MutationKill —— 策略表锁定:把 auth→held 改成"无限重试"必须报红。
//  3) TestRunTaskAuthClass_HeldWithoutBurningAttempts —— 认证失败不消耗 max_attempts_per_step,直接 held。
//  4) TestRunTaskUnknownClass_RegressionBaseline —— 未知乱码错误重试次数与退避间隔与现版逐一致。
//  5) TestRunTaskLimitLike_NotClassifiedAsLimit  —— 反例注入:与限额相似但无 limitRe 特征的文本必须走
//     一般类(unknown),不写全局冷却;若被误分类为限额则报红。
//  6) TestRunTaskInputTooLong_DirectFailedNoRetry —— 输入超长直接 failed,不烧 attempts。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// ---------- 分类器单元测试 ----------

// TestClassifyFailure_Enumerations 每类各挑代表性真实措辞验判据命中;反例段确保限额相似文本
// 不会被误分类(核心反例注入,与 limitRe 严格互斥)。
func TestClassifyFailure_Enumerations(t *testing.T) {
	cases := []struct {
		name    string
		msg     string
		want    failureClass
		wantWhy string // 断言失败时的解释,便于定位
	}{
		// 认证类:401/invalid key/oauth 过期/需重新登录
		{"401", "codex_error: 401 Unauthorized from api", failureAuth, "401 应判 auth"},
		{"invalid_key", "authentication failed: invalid api key", failureAuth, "invalid api key 应判 auth"},
		{"oauth_expired", "oauth token has expired, please re-login", failureAuth, "oauth expired 应判 auth"},
		{"chinese_relogin", "认证错误: 请重新登录", failureAuth, "请重新登录 应判 auth"},

		// 权限类:403/policy 拒绝
		{"403", "remote_codex_error: 403 Forbidden", failurePermission, "403 应判 permission"},
		{"policy_denied", "access denied by policy: cannot write to /etc", failurePermission, "policy denied 应判 permission"},
		{"org_blocked", "operation blocked by organization admin", failurePermission, "org blocked 应判 permission"},

		// 输入超长:context/prompt 超限
		{"context_length", "context length exceeded: 200000 tokens", failureInputTooLong, "context length exceeded 应判 input_too_long"},
		{"prompt_too_long", "prompt is too long for this model", failureInputTooLong, "prompt too long 应判 input_too_long"},
		{"chinese_input", "上下文超长,请裁剪 prompt", failureInputTooLong, "上下文超长 应判 input_too_long"},

		// 超时:我们自己拼的中文串
		{"step_timeout", "步骤超时(60 分钟)", failureTimeout, "步骤超时 应判 timeout"},
		{"remote_step_timeout", "远程步骤超时(60 分钟)", failureTimeout, "远程步骤超时 应判 timeout"},
		{"deadline_exceeded", "context deadline exceeded", failureTimeout, "deadline exceeded 应判 timeout"},

		// 执行器崩溃:信号杀/二进制缺失
		{"signal_killed", "signal: killed", failureExecutorCrash, "signal killed 应判 executor_crash"},
		{"signal_segv", "signal: segmentation fault", failureExecutorCrash, "signal segv 应判 executor_crash"},
		{"exec_not_found", `exec: "codex": executable file not found in $PATH`, failureExecutorCrash, "executable not found 应判 executor_crash"},

		// 反例:与限额相似但无 limitRe 特征——必须回落 unknown,绝不误判为限额或其他类。
		// 这是 spec 反例注入的核心验收:能识别"看起来像限额但没有可辨识特征的文本"。
		{"limit_like_no_signal", "quota nearly consumed with 5% remaining", failureUnknown, "限额相似但无特征应回落 unknown"},
		{"vague_cap", "some cap was almost reached; try later", failureUnknown, "泛化 cap 措辞应回落 unknown"},
		{"generic_error", "unknown internal error", failureUnknown, "泛化 error 一词应回落 unknown"},
		// 真限额措辞理论上不会走到这里(前面 isLimitHit 会截),但为诚实起见,即便 classifyFailure
		// 被单独调用也不该把 limit 类拉进来污染新语义——归 unknown 是最保守选择(unknown 走 retry,
		// 限额路径由 isLimitHit 独占,两条链绝不重叠)。
		{"real_limit_still_unknown", "Claude AI usage limit reached", failureUnknown, "限额措辞在分类器里应仍归 unknown(限额由 isLimitHit 独占)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyFailure(tc.msg, tc.msg, nil, nil)
			if got != tc.want {
				t.Fatalf("%s: got=%q want=%q msg=%q", tc.wantWhy, got, tc.want, tc.msg)
			}
		})
	}
}

// TestClassifyFailure_CombinedIgnored_TranscriptDoesNotPoisonMsg 【P0 Round-2 反例注入】
// combined 全量不影响分类判据,只吃 msg。codex exec 天然把整段 agent 推理/工具输出 transcript
// 写进 stderr——里面必然出现 permission denied / no such file or directory / token expired /
// context length exceeded 这类字面量(grep 受限目录、审查引用文档、审查改动 failure_class.go
// 本身时引用正则字面量)。早期版本把 combined 末段 8KB 一并扔进正则,任意一条无害字面量落进窗口
// 就把超时/正常错误误判成不可重试终态(permission→held 静默停摆;input_too_long→failed 永久终态)。
// 根修:classifyFailure 只吃 msg;分类特征不在 msg 就 unknown → retry_backoff(旧版基线,安全)。
//
// 【这条测试就是复审要求的证伪场景】:msg='codex_error: 步骤超时(60 分钟)' + combined 尾部含
// 无害 "Permission denied" 工具输出 → 修前会命中 permissionClassRe(判据顺序:permission 早于
// timeout)误判 permission → 直接 held;修后命中 msg 里的 timeoutClassRe → 判 timeout → 走
// retry_backoff。旧版跑此测试必红,新版必绿。
func TestClassifyFailure_CombinedIgnored_TranscriptDoesNotPoisonMsg(t *testing.T) {
	// 用户要求的核心证伪:超时 + transcript 尾部裸 Permission denied。
	msgTimeout := "codex_error: 步骤超时(60 分钟)"
	transcriptWithPermDenied := strings.Repeat("some agent reasoning line noise. ", 100) +
		"\n[tool:grep] /etc/shadow: Permission denied\n" +
		"[tool:cat] failed with error\n"
	if got := classifyFailure(msgTimeout, transcriptWithPermDenied, nil, nil); got != failureTimeout {
		t.Fatalf("超时+transcript 含 'Permission denied' 工具输出:必判 timeout(msg 主导),got=%q\n"+
			"(旧版扫 combined 尾窗会命中 permissionClassRe 误判 permission→held 静默停摆;根修后此测试转绿)", got)
	}

	// 同类反例——每个都覆盖一条裸短语正则(permission / auth / input_too_long / executor_crash),
	// 全部走同一形态:msg 是无分类特征的中性摘要 + combined 尾部埋一条 transcript 里常见的字面量。
	// 所有用例都必须回落 unknown(msg 无特征),证明 combined 完全不再喂给分类器。
	sameShapeCases := []struct {
		name       string
		transcript string
	}{
		{"perm_in_prose", "audit review discussing when 'Permission denied' errors are safe."},
		{"nsfoud_in_prose", "grep of a missing path returned 'no such file or directory' — expected."},
		{"token_expired_in_prose", "docs sample: server may respond 'token expired' after 1h — informational."},
		{"context_len_in_prose", "we noted 'context length exceeded' as a known llm error type in the review."},
		{"input_too_long_in_prose", "the report says 'input too long' should be handled by chunking."},
	}
	msgNeutral := "codex_error: something went wrong" // 无任何分类特征
	for _, tc := range sameShapeCases {
		t.Run(tc.name, func(t *testing.T) {
			// combined 尾部即含裸短语(位于末段 8KB 内),旧版必定命中对应终态类。
			got := classifyFailure(msgNeutral, tc.transcript, nil, nil)
			if got != failureUnknown {
				t.Fatalf("中性 msg + transcript 含裸 %q:必回落 unknown(combined 已不再喂给分类器),got=%q\n"+
					"(旧版会误判对应终态类→held/failed;根修后此测试转绿)", tc.name, got)
			}
		})
	}

	// 反向对照:msg 里含真正的分类特征时,必被正确识别——证明"只吃 msg"路径本身没坏。
	if got := classifyFailure("codex_error: 401 Unauthorized from api", "no transcript noise", nil, nil); got != failureAuth {
		t.Fatalf("msg 含真 401 特征应判 auth,got=%q", got)
	}
}

// ---------- 策略表 mutation-kill ----------

// TestPolicyForFailureClass_MutationKill 断言策略表——把任何一条终态策略改动(如 auth→无限重试)
// 都必须报红。这是 mutation-kill 验收:策略表不能被静默改动。
func TestPolicyForFailureClass_MutationKill(t *testing.T) {
	// 认证/权限必须 held(升级人工),不烧 attempts
	if p := policyFor(failureAuth); p.Terminal != statusHeld || p.ConsumesAttempt {
		t.Fatalf("auth 策略必须 held+不烧 attempts,got=%+v (mutation-kill:改成无限重试即被本断言捕获)", p)
	}
	if p := policyFor(failurePermission); p.Terminal != statusHeld || p.ConsumesAttempt {
		t.Fatalf("permission 策略必须 held+不烧 attempts,got=%+v", p)
	}
	// 输入超长必须 failed(不重试),不烧 attempts
	if p := policyFor(failureInputTooLong); p.Terminal != statusFailed || p.ConsumesAttempt {
		t.Fatalf("input_too_long 策略必须 failed+不烧 attempts,got=%+v", p)
	}
	// 超时/执行器崩溃/未知必须走现行 retry_backoff(Terminal="",烧 attempts)
	for _, cls := range []failureClass{failureTimeout, failureExecutorCrash, failureUnknown} {
		p := policyFor(cls)
		if p.Terminal != "" || !p.ConsumesAttempt {
			t.Fatalf("%s 策略必须走 retry_backoff(Terminal=''+烧 attempts),got=%+v", cls, p)
		}
	}
}

// ---------- runTask 集成:认证类不烧 attempts 直接 held ----------

// TestRunTaskAuthClass_HeldWithoutBurningAttempts spec 验收 #1:
// 注入 mock 认证失败文本 → 不消耗 max_attempts_per_step 重试,直接 held 且事件记明类别。
func TestRunTaskAuthClass_HeldWithoutBurningAttempts(t *testing.T) {
	root := testRoot(t)
	// fake claude 打 401 认证失败(is_error=true + result 含 401 unauthorized)。
	authJSON := `{"type":"result","is_error":true,"subtype":"api_error","result":"401 Unauthorized: invalid api key"}`
	claudeBin := fakeClaudeBin(t, authJSON, "", 1)
	cfg := runTaskCfg(t, claudeBin)
	cfg.MaxAttempts = 3

	work := t.TempDir()
	tk := newTask(root, cfg, typeSequence, "认证失败", work, []string{"p"}, 5)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if err := runTask(context.Background(), root, cfg, tk, false); err != nil {
		t.Fatal(err)
	}

	// 关键断言 1: attempts 未递增(直接 held,不烧重试次数)
	got, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempts != 0 {
		t.Fatalf("auth 类不该烧 attempts,got=%d 期望=0 (mutation-kill:若策略被改为重试,此断言即报红)", got.Attempts)
	}
	// 关键断言 2: 状态是 held(不是 queued/retry,也不是 failed)
	if got.Status != statusHeld {
		t.Fatalf("auth 类应直接 held,got=%q", got.Status)
	}
	// 关键断言 3: LastError 带 [auth] 前缀(annotatedError 分类标注)
	if !strings.HasPrefix(got.LastError, "[auth]") {
		t.Fatalf("auth 类 LastError 应带 [auth] 前缀,got=%q", got.LastError)
	}

	// 关键断言 4: 事件账本记 failure_class=auth,actor=runner:classifier
	events := readAllEventsRaw(t, root, tk.ID)
	if len(events) != 2 { // dispatched + held
		t.Fatalf("期望 dispatched+held 恰 2 条,got %d: %v", len(events), eventTypes(events))
	}
	if events[1].Type != evHeld {
		t.Fatalf("第 2 条应 held,got=%q", events[1].Type)
	}
	if events[1].Actor != "runner:classifier" {
		t.Fatalf("held 事件应由 runner:classifier 触发,got actor=%q", events[1].Actor)
	}
	if fc, _ := events[1].Detail["failure_class"].(string); fc != "auth" {
		t.Fatalf("detail.failure_class 应 auth,got=%q", fc)
	}
}

// ---------- runTask 集成:未知类回归基线 ----------

// TestRunTaskUnknownClass_RegressionBaseline spec 验收 #2:
// 注入未知乱码错误 → 重试次数与退避间隔与现版逐一致。
// 【回归基线】未知类必须走 retry_backoff 分支,行为(attempts++、not_before=now+backoff、
// LastError 不带前缀、事件 evRetry 带 attempts/not_before 字段)与旧版逐字节一致。
func TestRunTaskUnknownClass_RegressionBaseline(t *testing.T) {
	root := testRoot(t)
	// 未知乱码错误:不含任何分类枚举关键词,也不含 limitRe 特征。
	unknownJSON := `{"type":"result","is_error":true,"subtype":"weird","result":"xhrjnvkl frobnicated the widget"}`
	claudeBin := fakeClaudeBin(t, unknownJSON, "", 1)
	cfg := runTaskCfg(t, claudeBin)
	cfg.MaxAttempts = 3
	cfg.RetryBackoffMin = 5

	work := t.TempDir()
	tk := newTask(root, cfg, typeSequence, "未知乱码", work, []string{"p"}, 5)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	if err := runTask(context.Background(), root, cfg, tk, false); err != nil {
		t.Fatal(err)
	}

	got, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 基线 1: attempts 递增到 1(不是 0——未知类必须走退避)
	if got.Attempts != 1 {
		t.Fatalf("未知类应走 retry_backoff attempts++, got=%d 期望=1", got.Attempts)
	}
	// 基线 2: 状态回 queued(不是 held/failed)
	if got.Status != statusQueued {
		t.Fatalf("未知类未超轮限时应 queued,got=%q", got.Status)
	}
	// 基线 3: LastError 不带分类前缀(annotatedError 对 unknown 返回原 msg)——回归旧版行为
	if strings.HasPrefix(got.LastError, "[unknown]") || strings.HasPrefix(got.LastError, "[") {
		t.Fatalf("未知类 LastError 不该带分类前缀(回归基线要求),got=%q", got.LastError)
	}
	// 基线 4: not_before 大致等于 now + RetryBackoffMin(未知非 transient → 首次 backoff * attempts=1)
	expected := before.Add(time.Duration(cfg.RetryBackoffMin) * time.Minute).Unix()
	// 允许 ±60s 抖动(测试执行到 saveTask 有耗时)
	if got.NotBeforeEpoch < expected-5 || got.NotBeforeEpoch > expected+60 {
		t.Fatalf("退避间隔应约 %d 分钟, got not_before=%d expected≈%d", cfg.RetryBackoffMin, got.NotBeforeEpoch, expected)
	}

	// 基线 5: 事件是 evRetry 带 attempts + not_before, actor=runner(不是 runner:classifier);
	// detail 里可以额外带 failure_class=unknown(不改变原有断言字段)
	events := readAllEventsRaw(t, root, tk.ID)
	if len(events) != 2 || events[1].Type != evRetry {
		t.Fatalf("未知类应留 dispatched+retry,got=%v", eventTypes(events))
	}
	if events[1].Actor != "runner" {
		t.Fatalf("未知类 retry 事件 actor 应 runner(与旧版一致),got=%q", events[1].Actor)
	}
	if a, _ := events[1].Detail["attempts"].(float64); a != 1 {
		t.Fatalf("retry.detail.attempts 应 1(基线字段),got=%v", events[1].Detail["attempts"])
	}
	if _, ok := events[1].Detail["not_before"]; !ok {
		t.Fatalf("retry.detail.not_before 应存在(基线字段),detail=%+v", events[1].Detail)
	}
	if fc, _ := events[1].Detail["failure_class"].(string); fc != "unknown" {
		t.Fatalf("retry.detail.failure_class 应 unknown(新增审计字段),got=%q", fc)
	}
}

// ---------- runTask 集成:反例注入 ----------

// TestRunTaskLimitLike_NotClassifiedAsLimit spec 验收 #4(反例注入):
// 构造与限额错误相似但不含可辨识特征的文本 → 必须走一般类且不写全局冷却;
// 若被误分类为限额则报红(cooldown 文件被写即报红——限额路径的证据)。
//
// 【为什么这条是核心】isLimitHit 与 classifyFailure 必须严格互斥:限额判定由 limitRe 独占,
// 分类器绝不吃 limit 类;哪怕文本"看起来像"限额(带 quota/cap 措辞但无 limitRe 关键词),
// 也必须回落 unknown。否则会出现"限额相似的正常错误被写全局冷却→整队停发"的最严重回归。
func TestRunTaskLimitLike_NotClassifiedAsLimit(t *testing.T) {
	root := testRoot(t)
	// 反例文本:含 quota / cap 之类"限额相似"词,但**不匹配 limitRe**(无 "usage limit"/"limit reached"/
	// "hit your limit"/"limit will reset"/"out of credits"/"session limit" 等特征)。
	limitLikeJSON := `{"type":"result","is_error":true,"subtype":"weird","result":"quota nearly consumed with 5% remaining; some cap probably nearby"}`
	claudeBin := fakeClaudeBin(t, limitLikeJSON, "", 1)
	cfg := runTaskCfg(t, claudeBin)
	cfg.MaxAttempts = 3
	cfg.RetryBackoffMin = 5

	work := t.TempDir()
	tk := newTask(root, cfg, typeSequence, "限额相似反例", work, []string{"p"}, 5)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if err := runTask(context.Background(), root, cfg, tk, false); err != nil {
		t.Fatal(err)
	}

	got, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 反例断言 1: 全局冷却文件必须不存在(限额路径的核心证据)
	if _, err := os.Stat(cooldownPath(root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("限额相似但无特征的文本绝不该触发全局冷却,cooldown 文件不该存在;若存在则被误分类为限额(核心反例注入报红)")
	}
	// 反例断言 2: 状态不是 limit_paused(限额路径终态)
	if got.Status == statusLimitPaused {
		t.Fatalf("反例文本不该走限额路径 limit_paused,got status=%q", got.Status)
	}
	// 反例断言 3: 状态应回 queued(走 unknown 的 retry_backoff)
	if got.Status != statusQueued {
		t.Fatalf("反例文本应走 unknown → retry_backoff 挂回 queued,got=%q", got.Status)
	}
	// 反例断言 4: 事件不是 limit_paused,应是 retry;failure_class=unknown
	events := readAllEventsRaw(t, root, tk.ID)
	if len(events) < 2 {
		t.Fatalf("至少 dispatched+retry 两条,got=%v", eventTypes(events))
	}
	if events[1].Type == evLimitPaused {
		t.Fatalf("反例事件不该是 limit_paused,got=%q", events[1].Type)
	}
	if events[1].Type != evRetry {
		t.Fatalf("反例应记 retry(unknown 走 retry_backoff),got=%q", events[1].Type)
	}
	if fc, _ := events[1].Detail["failure_class"].(string); fc != "unknown" {
		t.Fatalf("反例 failure_class 应 unknown,got=%q", fc)
	}
}

// ---------- runTask 集成:输入超长直接 failed ----------

// TestRunTaskInputTooLong_DirectFailedNoRetry 输入超长:直接 failed,不烧 attempts。
// 补齐 spec"输入超长"策略的验收——重试同样超长必然再失败,首次即失败节省额度。
func TestRunTaskInputTooLong_DirectFailedNoRetry(t *testing.T) {
	root := testRoot(t)
	// prompt too long 措辞。
	longJSON := `{"type":"result","is_error":true,"subtype":"api_error","result":"prompt is too long for this model context"}`
	claudeBin := fakeClaudeBin(t, longJSON, "", 1)
	cfg := runTaskCfg(t, claudeBin)
	cfg.MaxAttempts = 3

	work := t.TempDir()
	tk := newTask(root, cfg, typeSequence, "输入超长", work, []string{"p"}, 5)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if err := runTask(context.Background(), root, cfg, tk, false); err != nil {
		t.Fatal(err)
	}

	got, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempts != 0 {
		t.Fatalf("input_too_long 不该烧 attempts, got=%d 期望=0", got.Attempts)
	}
	if got.Status != statusFailed {
		t.Fatalf("input_too_long 应直接 failed,got=%q", got.Status)
	}
	if !strings.HasPrefix(got.LastError, "[input_too_long]") {
		t.Fatalf("input_too_long 类 LastError 应带 [input_too_long] 前缀,got=%q", got.LastError)
	}
	events := readAllEventsRaw(t, root, tk.ID)
	if len(events) != 2 || events[1].Type != evFailed {
		t.Fatalf("期望 dispatched+failed,got=%v", eventTypes(events))
	}
	if events[1].Actor != "runner:classifier" {
		t.Fatalf("failed 事件应由 runner:classifier 触发,got=%q", events[1].Actor)
	}
	if fc, _ := events[1].Detail["failure_class"].(string); fc != "input_too_long" {
		t.Fatalf("failed.detail.failure_class 应 input_too_long,got=%q", fc)
	}
	if reason, _ := events[1].Detail["reason"].(string); reason != "input_too_long_no_retry" {
		t.Fatalf("failed.detail.reason 应带策略原因,got=%q", reason)
	}
}

// ---------- 断言 auth 与 limit 严格互斥 ----------

// TestClassifyFailure_AuthNotShadowLimit 补充断言:即便 auth 文本里恰好含 "limit" 词,
// 也必须先判 auth(判据顺序:auth 早于任何 limit-like 判定;而且分类器本就不认 limit 类)。
func TestClassifyFailure_AuthNotShadowLimit(t *testing.T) {
	// 极端拼装:"401 Unauthorized (per-key rate limit config)"——含 rate limit 也含 401。
	// 判据顺序 auth 早于任何 limit-like,且分类器不认 limit 类,故必判 auth。
	got := classifyFailure("401 Unauthorized (per-key rate limit config)", "", nil, nil)
	if got != failureAuth {
		t.Fatalf("含 401 时应优先判 auth,got=%q", got)
	}
}

// TestAnnotatedError_PrefixOnlyForTerminalClasses 【P1 Round-2】双语 README 承诺契约"仅 auth/
// permission/input_too_long 带 [<class>] 前缀"——即**仅终态类**加前缀。早期版本只豁免 unknown,
// timeout/executor_crash 也加前缀→与文档矛盾且重试类的 LastError 呈现偏离旧版。根修判据挂到
// policyFor(cls).Terminal:终态类(Terminal!="")加前缀,重试类(Terminal="")不加。本测试同时锁定
// mutation-kill——若未来有人把"回归基线仅止 unknown"的旧逻辑反悔改回来,timeout/executor_crash
// 分支即报红。
func TestAnnotatedError_PrefixOnlyForTerminalClasses(t *testing.T) {
	raw := "some random error message"

	// 终态类:必须加前缀(README 契约的三类)
	terminalCases := []struct {
		cls  failureClass
		want string
	}{
		{failureAuth, fmt.Sprintf("[%s] %s", failureAuth, raw)},
		{failurePermission, fmt.Sprintf("[%s] %s", failurePermission, raw)},
		{failureInputTooLong, fmt.Sprintf("[%s] %s", failureInputTooLong, raw)},
	}
	for _, tc := range terminalCases {
		t.Run("terminal_"+string(tc.cls), func(t *testing.T) {
			if got := annotatedError(tc.cls, raw); got != tc.want {
				t.Fatalf("%s 类应加前缀,got=%q want=%q", tc.cls, got, tc.want)
			}
		})
	}

	// 可重试类:必须不加前缀(与旧版逐字节一致——回归基线要求覆盖 unknown/timeout/executor_crash 全部)
	// 【P1 mutation-kill】这三条断言就是根修凭据:若 annotatedError 回退到"只豁免 unknown",
	// timeout/executor_crash 会拿到带前缀返回值,本测试即报红。
	retryableCases := []failureClass{failureUnknown, failureTimeout, failureExecutorCrash}
	for _, cls := range retryableCases {
		t.Run("retryable_"+string(cls), func(t *testing.T) {
			if got := annotatedError(cls, raw); got != raw {
				t.Fatalf("%s 类不该加前缀(README 契约仅终态类带前缀;重试类需与旧版逐字节一致),got=%q want=%q",
					cls, got, raw)
			}
		})
	}

}
