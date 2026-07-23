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
	"path/filepath"
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

// ---------- classificationFromTranscript 单元 & 反例注入(P1 · Round-3) ----------

// TestClassificationFromTranscript_ByErrorSummaryPath 断言判据与 errorSummary 三条 msg 构造路径一一对齐:
//  1) path 1 (res != nil && res.IsError, res.Result 拼 msg) → 由 invoke 侧的 ResultFromTranscript 标决定;
//     标 true(codexErrorLine/agent -o 终稿/invokeRemoteClaude 挑行) → true;
//     标 false(claude 结构化 JSON 的 API 错误) → false。
//  2) path 2 (runErr != nil 且 !path1) → msg 恒含 firstLine(combined) → true。
//  3) path 3 (res==nil && runErr==nil) → msg 恒含 firstLine(combined) → true。
//
// 【为什么覆盖 path 3】invokeClaude 在 claude CLI 退出 0 但 stdout 非 JSON 时返回 (nil, combined, nil);
// errorSummary 走 path 3 拼 firstLine(combined) 进 msg,若 combined 首行含 "permission denied"/
// "401 unauthorized" 等字面量会误判 held——早期实现漏此分支,本轮补齐。
//
// 【mutation-kill】把 classificationFromTranscript 改成永远返回 false → 任一 want=true 分支即红;
// 改成永远返回 true → path 1 未打标(claude 结构化 JSON)分支即红。
func TestClassificationFromTranscript_ByErrorSummaryPath(t *testing.T) {
	cases := []struct {
		name   string
		res    *claudeResult
		runErr error
		want   bool
		why    string
	}{
		// path 1 · ResultFromTranscript=true:codex/remote 挑行 / agent -o 终稿 → transcript 来源
		{"path1_transcript_marked", &claudeResult{IsError: true, ResultFromTranscript: true}, nil, true,
			"invoke 侧挑行/agent 终稿打标"},
		{"path1_marked_with_runErr", &claudeResult{IsError: true, ResultFromTranscript: true},
			errors.New("步骤超时"), true, "打标优先,runErr 冗余"},
		// path 1 · ResultFromTranscript=false:claude 结构化 JSON 的 API 错误 → 非 transcript,保留终态语义
		{"path1_structured_claude_error", &claudeResult{IsError: true, ResultFromTranscript: false}, nil, false,
			"claude 结构化 JSON 的 is_error=true(如 401 Unauthorized)属 API 侧判定,可信作终态"},
		{"path1_structured_with_runErr", &claudeResult{IsError: true, ResultFromTranscript: false},
			errors.New("非致命 wrapper"), false, "path 1 优先(res.IsError=true),runErr 不覆盖"},

		// path 2:runErr != nil 且 !res.IsError → errorSummary 拼 firstLine(combined)
		{"path2_nil_res_with_runErr", nil, errors.New("步骤超时(60 分钟)"), true, "res=nil+runErr → path 2"},
		{"path2_non_error_res_with_runErr", &claudeResult{IsError: false},
			errors.New("步骤超时"), true, "非 IsError res+runErr → path 2"},

		// path 3:res==nil && runErr==nil → errorSummary "无法解析 claude 输出 | firstLine(combined)"
		// 【关键】早期实现漏此分支返回 false → 边缘案例(claude CLI 退出 0 但非 JSON 输出含
		// 'permission denied')会误判 held。本轮补齐必须返回 true。
		{"path3_nil_all_still_transcript", nil, nil, true,
			"invokeClaude 退 0+非 JSON stdout → errorSummary path 3 拼 firstLine(combined)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classificationFromTranscript(tc.res, tc.runErr); got != tc.want {
				t.Fatalf("%s: got=%v want=%v (%s;mutation-kill:去掉对应分支即报红)",
					tc.name, got, tc.want, tc.why)
			}
		})
	}
}

// ---------- runTask 集成 · P1 Round-3 反例注入(codex transcript 挑行导致的伪终态) ----------

// fakeCodexTranscriptAuth 造一个假 codex:退出码非 0(runErr!=nil),写 codex 横幅+审查引用行(裸
// "401 unauthorized")到 stderr,**不写** -o 输出文件(res.Result="")。触发 invokeCodex 的
// codexErrorLine(combined) 挑行→ res.Result 被填成 transcript 里的 "401 unauthorized" 引用行,
// ResultFromTranscript=true。
//
// 【为什么这条形态是核心证伪场景】codex 审查卡审查本仓代码时,stderr 天然含引用 failure_class.go 里
// authClassRe 字面量的工具输出行(裸 "401 unauthorized"/"invalid api key",无 limitRe 词)。旧代码
// 会经 codexErrorLine → res.Result → errorSummary → msg="codex_error: ...401 unauthorized..." →
// authClassRe 命中 → auth → held——基线本会退避自愈的超时/瞬时抖动被误判成不可重试终态、无人值守
// 静默停摆。修法给 res 打 ResultFromTranscript=true,runTask 侧的 classificationFromTranscript 判定
// 后降级 retry_backoff。本测试就是该修法的正例(应走 retry)+回退验证(去掉打标必红)。
func fakeCodexTranscriptAuth(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	// stderr 打 codex 横幅 + 审查引用行(裸 "401 unauthorized"),stdout 空,不写 -o,退出 1。
	// codexErrorLine 会跳过横幅(codexNoiseRe 命中)、挑到裸 401 行(codexHardErrRe 命中)。
	// 特意加了"—>"前缀,避免被 codexNoiseRe 误吞;并确保**不包含** limitRe 特征词避免走限额分支。
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" + // 吸掉 stdin(prompt)
		"printf '%s\\n' 'OpenAI Codex v0.144.1' >&2\n" +
		"printf '%s\\n' '--------' >&2\n" +
		"printf '%s\\n' 'workdir: /tmp/x' >&2\n" +
		"printf '%s\\n' 'model: mock' >&2\n" +
		"printf '%s\\n' 'user' >&2\n" +
		"printf '%s\\n' '审查 failure_class.go 的 authClassRe' >&2\n" +
		"printf '%s\\n' '  -> 引用了字面量 401 unauthorized' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunTaskCodexTranscriptDerivedAuth_SoftenedToRetry 【P1 Round-3 反例注入】
// mock codex(runErr!=nil、-o 空、combined 含 '401 unauthorized' 引用行) → 断言走 retry(queued、
// attempts=1)而非 held。回退验证:把 invokeCodex 里 res.ResultFromTranscript=true 那行去掉,本测试
// 必红(策略侧无法区分 transcript 来源 → 走 auth → held → status=held/attempts=0/前缀 [auth])。
func TestRunTaskCodexTranscriptDerivedAuth_SoftenedToRetry(t *testing.T) {
	root := testRoot(t)
	work := t.TempDir()

	cfg := defaultConfig("")
	cfg.CodexBin = fakeCodexTranscriptAuth(t)
	cfg.StepTimeoutMin = 1
	cfg.MaxAttempts = 3
	cfg.RetryBackoffMin = 1
	cfg.LimitFallbackMin = 30
	cfg.CooldownMarginSec = 0

	tk := newTask(root, cfg, typeSequence, "codex transcript auth 引用", work, []string{"审核"}, 1)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	// useCodex=true → invokeCodex 主跑
	if err := runTask(context.Background(), root, cfg, tk, true); err != nil {
		t.Fatal(err)
	}

	got, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 反例断言 1: 状态 = queued(退避重试),不是 held
	if got.Status != statusQueued {
		t.Fatalf("transcript 挑走 '401 unauthorized' 不该落 held,应降级 retry_backoff\n"+
			"got status=%q last_error=%q\n"+
			"(回退验证:去掉 invokeCodex 的 ResultFromTranscript=true → 分类走 auth → held,本断言即红)",
			got.Status, got.LastError)
	}
	// 反例断言 2: attempts 递增到 1(走 retry_backoff 分支)
	if got.Attempts != 1 {
		t.Fatalf("attempts 应递增到 1(retry 分支), got=%d", got.Attempts)
	}
	// 反例断言 3: LastError 不加 [auth] 前缀(softened 场景 lastErrCls 拉回 unknown)
	if strings.HasPrefix(got.LastError, "[auth]") || strings.HasPrefix(got.LastError, "[") {
		t.Fatalf("softened 场景 LastError 不该带 [class] 前缀(与旧版逐字节一致),got=%q", got.LastError)
	}

	// 反例断言 4: 事件是 evRetry,failure_class 记 auth(审计保留),softened_from_terminal=true
	events := readAllEventsRaw(t, root, tk.ID)
	if len(events) < 2 {
		t.Fatalf("至少 dispatched+retry 两条事件,got=%v", eventTypes(events))
	}
	ev := events[len(events)-1]
	if ev.Type != evRetry {
		t.Fatalf("末条事件应为 retry(softened 走 retry_backoff 分支),got type=%q\n"+
			"(若为 held,说明 transcript 来源判据未生效)", ev.Type)
	}
	if fc, _ := ev.Detail["failure_class"].(string); fc != "auth" {
		t.Fatalf("failure_class 审计信号应保留=auth,got=%q\n"+
			"(降级不该丢原分类信号,便于审计聚合识别『真 unknown』vs『被 transcript 降级的 auth』)", fc)
	}
	if softened, _ := ev.Detail["softened_from_terminal"].(bool); !softened {
		t.Fatalf("softened_from_terminal 应=true,detail=%+v", ev.Detail)
	}
	if reason, _ := ev.Detail["reason"].(string); reason != "softened_transcript_derived" {
		t.Fatalf("reason 应=softened_transcript_derived,got=%q", reason)
	}
}

// TestRunTaskCodexTranscriptDerivedPermission_SoftenedToRetry 同类反例:transcript 挑到裸
// "403 forbidden" 引用行(permissionClassRe 命中,policy 落 held)。同理应降级 retry。
// 覆盖 codexHardErrRe∩permissionClassRe 的第二个交集点 (前一个是 auth 的 401 unauthorized)。
func TestRunTaskCodexTranscriptDerivedPermission_SoftenedToRetry(t *testing.T) {
	root := testRoot(t)
	work := t.TempDir()
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" +
		"printf '%s\\n' 'OpenAI Codex v0.144.1' >&2\n" +
		"printf '%s\\n' '--------' >&2\n" +
		"printf '%s\\n' 'user' >&2\n" +
		"printf '%s\\n' '审查引用 permissionClassRe 里的字面量: 403 forbidden' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := defaultConfig("")
	cfg.CodexBin = path
	cfg.StepTimeoutMin = 1
	cfg.MaxAttempts = 3
	cfg.RetryBackoffMin = 1

	tk := newTask(root, cfg, typeSequence, "codex transcript permission 引用", work, []string{"审核"}, 1)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if err := runTask(context.Background(), root, cfg, tk, true); err != nil {
		t.Fatal(err)
	}
	got, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != statusQueued || got.Attempts != 1 {
		t.Fatalf("permission 引用行应降级 retry,got status=%q attempts=%d", got.Status, got.Attempts)
	}
	events := readAllEventsRaw(t, root, tk.ID)
	ev := events[len(events)-1]
	if ev.Type != evRetry {
		t.Fatalf("末条事件应为 retry,got=%q", ev.Type)
	}
	if fc, _ := ev.Detail["failure_class"].(string); fc != "permission" {
		t.Fatalf("failure_class 审计信号应=permission,got=%q", fc)
	}
	if softened, _ := ev.Detail["softened_from_terminal"].(bool); !softened {
		t.Fatalf("softened_from_terminal 应=true,detail=%+v", ev.Detail)
	}
}

// TestRunTaskClaudeStructuredAuth_StillHeld 反向对照:claude 结构化 JSON 的 IsError=true(auth 类)
// **不**被视为 transcript 来源,仍应直接 held——证明第二道防线不误伤 claude 的合法结构化错误。
// 【为什么这条对照必要】softening 若过宽,会把 claude 真 401(res.IsError=true, ResultFromTranscript=false)
// 也降级 retry_backoff——把不可重试的凭据错误重复烧 attempts,与 CG-3 立卡动机(直接 held 省额度)相悖。
func TestRunTaskClaudeStructuredAuth_StillHeld(t *testing.T) {
	root := testRoot(t)
	authJSON := `{"type":"result","is_error":true,"subtype":"api_error","result":"401 Unauthorized: invalid api key"}`
	claudeBin := fakeClaudeBin(t, authJSON, "", 1)
	cfg := runTaskCfg(t, claudeBin)
	cfg.MaxAttempts = 3

	work := t.TempDir()
	tk := newTask(root, cfg, typeSequence, "claude 结构化 401", work, []string{"p"}, 5)
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
	if got.Status != statusHeld {
		t.Fatalf("claude 结构化 401(非 transcript 来源)应保持 held,got status=%q\n"+
			"(若被误降级 retry_backoff,说明 classificationFromTranscript 通道 2 判据过宽)",
			got.Status)
	}
	if got.Attempts != 0 {
		t.Fatalf("held 不该烧 attempts,got=%d", got.Attempts)
	}
	if !strings.HasPrefix(got.LastError, "[auth]") {
		t.Fatalf("结构化 auth 类应保留 [auth] 前缀,got=%q", got.LastError)
	}
}

// ---------- runTask 集成 · P1 Round-3 补齐(按类闭合的两处漏网) ----------

// fakeCodexTerminalMessageWithAuthRef 造一个假 codex:找到 -o 参数,把 agent 终稿(含引用
// authClassRe 字面量的 "401 unauthorized" 叙述)写进去,退出 1 模拟 codex 进程写完终稿后
// 因非致命告警退出非零(或步骤超时/子进程崩溃等)。invokeCodex 因 runErr!=nil 且 res.Result!=""
// 走"保留 -o 内容"分支——这是 R3 复审枚举但未修补的同构漏网:agent 终稿是任意生成内容,不属结
// 构化错误信息,应打 ResultFromTranscript 标由 classificationFromTranscript 承接降级。
func fakeCodexTerminalMessageWithAuthRef(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	// 注:script 用 shell parameter expansion 找到紧跟 "-o" 的下一个位置参数;写入含 "401
	// unauthorized" 字面量的一行(命中 authClassRe),然后退出 1。
	script := "#!/bin/sh\n" +
		"cat >/dev/null\n" + // 吸掉 stdin(prompt+preamble)
		"prev=\"\"\n" +
		"for a in \"$@\"; do\n" +
		"  if [ \"$prev\" = \"-o\" ]; then\n" +
		"    printf '%s\\n' 'agent 审查报告: 401 unauthorized 错误在 authClassRe 里被识别为认证类' > \"$a\"\n" +
		"    break\n" +
		"  fi\n" +
		"  prev=\"$a\"\n" +
		"done\n" +
		"exit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunTaskCodexAgentTerminalMessageWithAuthRef_SoftenedToRetry 【P1 Round-3 补齐 · 漏网 1】
// mock codex:agent 已把终稿写进 -o 文件(含引用 "401 unauthorized" 字面量),但进程退出非零
// (模拟 codex 收尾竞态/非致命告警/超时后仍有终稿的场景)。invokeCodex 走
// "runErr!=nil AND res.Result!=""" 分支保留 -o 内容作 res.Result。
//
// 【为什么这条是必修漏网】早期 R3 修复只在 res.Result=="" 且 codexErrorLine 挑到行时打
// ResultFromTranscript 标——保留 -o 内容的分支被漏。agent 终稿可任意引用分类正则字面量
// (审查/工具输出/正常叙述),不打标会导致误判 auth/permission→held 静默停摆。
//
// 修法:在 case runErr!=nil 里给 res.Result != "" 的 else 分支同样打
// ResultFromTranscript=true。回退验证:去掉该 else 分支即分类走 auth→held,本测试即红。
func TestRunTaskCodexAgentTerminalMessageWithAuthRef_SoftenedToRetry(t *testing.T) {
	root := testRoot(t)
	work := t.TempDir()

	cfg := defaultConfig("")
	cfg.CodexBin = fakeCodexTerminalMessageWithAuthRef(t)
	cfg.StepTimeoutMin = 1
	cfg.MaxAttempts = 3
	cfg.RetryBackoffMin = 1
	cfg.LimitFallbackMin = 30
	cfg.CooldownMarginSec = 0

	tk := newTask(root, cfg, typeSequence, "codex agent 终稿含 auth 引用", work, []string{"审核"}, 1)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	if err := runTask(context.Background(), root, cfg, tk, true); err != nil {
		t.Fatal(err)
	}
	got, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}

	if got.Status != statusQueued {
		t.Fatalf("codex agent 终稿含 '401 unauthorized' 引用不该落 held(agent 任意内容不作终态判据),\n"+
			"got status=%q last_error=%q\n"+
			"(回退验证:去掉 invokeCodex `case runErr!=nil` 里 else 分支的 res.ResultFromTranscript=true → \n"+
			" 分类走 auth → held,本断言即红)",
			got.Status, got.LastError)
	}
	if got.Attempts != 1 {
		t.Fatalf("attempts 应递增到 1(retry_backoff 分支), got=%d", got.Attempts)
	}
	if strings.HasPrefix(got.LastError, "[auth]") {
		t.Fatalf("softened 场景 LastError 不该带 [auth] 前缀,got=%q", got.LastError)
	}
	events := readAllEventsRaw(t, root, tk.ID)
	if len(events) < 2 {
		t.Fatalf("至少 dispatched+retry 两条,got=%v", eventTypes(events))
	}
	ev := events[len(events)-1]
	if ev.Type != evRetry {
		t.Fatalf("末条事件应为 retry(softened 走 retry_backoff),got type=%q", ev.Type)
	}
	if fc, _ := ev.Detail["failure_class"].(string); fc != "auth" {
		t.Fatalf("failure_class 审计信号应保留=auth,got=%q", fc)
	}
	if softened, _ := ev.Detail["softened_from_terminal"].(bool); !softened {
		t.Fatalf("softened_from_terminal 应=true,detail=%+v", ev.Detail)
	}
	if reason, _ := ev.Detail["reason"].(string); reason != "softened_transcript_derived" {
		t.Fatalf("reason 应=softened_transcript_derived,got=%q", reason)
	}
}

// TestRunTaskClaudeParseFailNonJSONWithPermissionRef_SoftenedToRetry 【P1 Round-3 补齐 · 漏网 2】
// mock claude:退出 0 但 stdout 非 JSON(含 "permission denied" 首行)。invokeClaude 因
// parseClaudeJSON 返回 nil 且 runErr==nil,返回 (nil, combined, nil)——errorSummary 走 path 3
// (无 res 无 runErr)恒拼 firstLine(combined) 进 msg,命中 permissionClassRe → policy=held。
//
// 【为什么这条是必修漏网】早期 classificationFromTranscript 判据只覆盖两条通道
// (ResultFromTranscript 打标 + runErr!=nil),漏了 res==nil && runErr==nil 情形。虽然罕见
// (需 claude CLI 退 0 且 stdout 无有效 JSON——如输出格式漂移/异常退出/CLI bug),但一旦发生
// 首行含 permission denied/401 unauthorized 就会误判 held 静默停摆。
//
// 修法:classificationFromTranscript 与 errorSummary 三条 path 一一对齐——path 1(res.IsError)
// 由 ResultFromTranscript 决定,path 2/3 恒 true。回退验证:把 path 3 分支(res==nil 时的
// return true)去掉则重现 held,本测试即红。
func TestRunTaskClaudeParseFailNonJSONWithPermissionRef_SoftenedToRetry(t *testing.T) {
	root := testRoot(t)
	// 非 JSON 首行含 "permission denied"——parseClaudeJSON 会返回 nil。**不含** limitRe 特征词
	// 避免走限额分支。
	nonJSONStdout := "permission denied by tool wrapper — retry with correct scope"
	claudeBin := fakeClaudeBin(t, nonJSONStdout, "", 0)
	cfg := runTaskCfg(t, claudeBin)
	cfg.MaxAttempts = 3

	work := t.TempDir()
	tk := newTask(root, cfg, typeSequence, "claude 非 JSON 含 permission denied", work, []string{"p"}, 5)
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
	if got.Status != statusQueued {
		t.Fatalf("claude 非 JSON 输出 firstLine 含 'permission denied' 不该落 held(path 3 恒 transcript 来源),\n"+
			"got status=%q last_error=%q\n"+
			"(回退验证:把 classificationFromTranscript 的 res==nil 分支删掉 → 走 permission → held,\n"+
			" 本断言即红)",
			got.Status, got.LastError)
	}
	if got.Attempts != 1 {
		t.Fatalf("attempts 应递增到 1,got=%d", got.Attempts)
	}
	if strings.HasPrefix(got.LastError, "[permission]") {
		t.Fatalf("softened 场景 LastError 不该带 [permission] 前缀,got=%q", got.LastError)
	}
	events := readAllEventsRaw(t, root, tk.ID)
	if len(events) < 2 {
		t.Fatalf("至少 dispatched+retry 两条,got=%v", eventTypes(events))
	}
	ev := events[len(events)-1]
	if ev.Type != evRetry {
		t.Fatalf("末条事件应为 retry,got=%q", ev.Type)
	}
	if fc, _ := ev.Detail["failure_class"].(string); fc != "permission" {
		t.Fatalf("failure_class 审计信号应保留=permission,got=%q", fc)
	}
	if softened, _ := ev.Detail["softened_from_terminal"].(bool); !softened {
		t.Fatalf("softened_from_terminal 应=true,detail=%+v", ev.Detail)
	}
	if reason, _ := ev.Detail["reason"].(string); reason != "softened_transcript_derived" {
		t.Fatalf("reason 应=softened_transcript_derived,got=%q", reason)
	}
}
