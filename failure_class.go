package main

// failure_class.go —— CG-3 失败分类分流。
//
// 【为什么必须存在】
// 现状"其他失败"只有单一 retry_backoff：认证过期/权限拒绝/输入超长这类**明显不可重试**的错误
// 也在盲目烧 max_attempts_per_step，把订阅额度打进注定失败的重试里——与"每 token 都要花在刀
// 刃上"的订阅哲学正面冲突。CG-2 的限额特判（写全局冷却）已经证明"分类分流"的必要与雏形，
// CG-3 把这套雏形扩展成有限枚举分类器：明确不可重试的直接 held/failed，明确可重试的沿用退避，
// **无法明确归类的一律兜底为现行 retry_backoff 行为**（不猜测）。
//
// 【为什么"未知类兜底现行行为"是硬纪律】
// 分类器是"越权就烧钱"的组件：一旦把非认证错误误判为 auth→held，任务在无人值守时段静默停摆；
// 一旦把可重试错误误判为 failed，一次瞬时抖动就永久失败。所以：
//   1. 判据用**严格枚举正则**——只认服务端与执行器明确措辞，不做泛化关键词猜测（"error"/"failed"
//      这类广谱词天然禁用）；
//   2. 与 limitRe 严格互斥——限额判定由前面的 isLimitHit 独占（写全局冷却），classifyFailure
//      绝不吃 limit 类；即便文本"看起来像限额"（含 quota / cap 但无 limitRe 特征），一律 unknown
//      走一般退避、**绝不写全局冷却**。这条是反例注入的核心，与"三源分歧取最保守值"的额度哲学
//      正交（不该被激进分类误伤成"限额挂起"）；
//   3. 判不出来的直接 unknown → 由调用方走现行 retry_backoff 分支，行为与旧代码逐字节一致
//      （回归基线断言把这条钉死）。
//
// 【为什么不做自动 replan/decompose】
// CAMEL 的 retry→decompose→replan 想法漂亮但需求未证：ClaudeGo 现状 held 升级人工的路径已够用
// （人工看清楚再 release/cancel），自动 replan 一旦误判就是"任务被 AI 悄悄改写"，与"完整任务
// 血缘可审计"的诚实性纪律冲突。CG-3 只做分类分流，不动 replan——真需要时再单独立卡。

import (
	"regexp"
	"strings"
)

// 【P0 教训 · Round-2 复审】早期版本把扫描窗口拼成 msg + tailWindow(combined, 8192)——想的是
// 「真错误行几乎都在末尾几百字节」，末段窗口比全量安全。但复审用真实自举负载证伪：codex exec 把
// 整段 agent 推理/工具输出 transcript 直接写进 stderr（runner.go:191 拼进 combined，runner.go:2066
// 附近注释也承认 transcript 天然充斥无害的 error 措辞），而分类正则里含裸短语——permission denied、
// no such file or directory、token expired、context length exceeded、input too long、prompt too
// long——这些字面量在正常 transcript 里频繁出现（grep 受限目录、审查引用文档、审查改动 failure_class.go
// 本身时引用正则字面量……）。一旦落进末段 8KB，就会把毫不相干的超时/正常错误误判成 permission 直接
// held（无人值守静默停摆，基线本会退避重试自愈）或 auth 直接 held（同样静默）或 input_too_long
// 直接 failed（永久终态）。这与文件顶部"越权就烧钱"的硬纪律直接冲突。
//
// 【根修 · 第一道防线】classifyFailure 只吃 msg——combined 完全不看：分类特征若没进 msg，就让分类器
// 坦然回落 unknown → 走 retry_backoff，即旧版基线行为，安全；宁可漏分类走退避，也不能凭 transcript
// 尾窗的裸短语误判成不可重试终态。tailWindow 辅助函数一并删除，杜绝未来"再把 combined 拼回扫描串"的
// 复辟诱惑。
//
// 【P1 教训 · Round-3 复审】仅第一道防线不够：msg 不是"没有 transcript 噪声"——它是 errorSummary
// 的产物，而 errorSummary 存在两条 transcript 旁路把 combined 挑出的行拼进 msg：
//   1) res.IsError 分支：res.Result 若被 invokeCodex/invokeRemoteCodex/invokeRemoteClaude 从 combined
//      挑行(codexErrorLine)或取首行(firstLine)填入,再由 firstLine(res.Result) 进 msg——见 runner.go
//      各 invoke 函数与 errorSummary。
//   2) res 为 nil 或非 IsError 时的 fallback：runErr.Error() + " | " + firstLine(combined) 直接
//      拼 combined 首行进 msg——invokeClaude 的 parseClaudeJSON 返回 nil 时走这条。
// 两条通路的证伪场景相同：codex 审查本仓代码时 transcript 天然含 authClassRe/permissionClassRe 等
// 字面量,只要该步 runErr!=nil 或 -o 空,codexErrorLine 就会命中 codexHardErrRe 挑走裸 "401 unauthorized"
// 行 → res.Result → msg → auth → held(基线本会退避自愈的超时被误判成不可重试终态)。
//
// 【根修 · 第二道防线】给 res 增加 ResultFromTranscript 标记(runner.go 的三处 invoke 挑行时打开),
// runTask 分类后调 classificationFromTranscript(res, runErr) 判断分类信号是否 transcript 来源:
//   - transcript 来源信号 + policy.Terminal!="" 一律**降级** retry_backoff(failure_class 事件仍写供审计,
//     LastError 不加 [class] 前缀避免误导人工)。
//   - 非 transcript 来源(claude 结构化 JSON 的 res.Result,或 runErr 本身的措辞如"步骤超时(...)")
//     照常走终态,与 Round-2 语义一致。
// 两道防线正交:第一道防 combined 全量污染分类,第二道防 combined 挑行经 res.Result 旁路进 msg。

// failureClass 是失败分类的有限枚举。
// 命名遵守"名词短语，语义与执行结果强绑定"，写进 events.jsonl 的 detail.failure_class 里做审计。
type failureClass string

const (
	// failureAuth 认证失效：凭据过期/无效/被吊销。重试无益（凭据不会因重试自动刷新），
	// 直接 held 升级人工——让委托人 relogin 后 release 续跑，比烧 attempts 再 failed 省额度。
	failureAuth failureClass = "auth"
	// failurePermission 权限拒绝：403 / policy 拒绝 / 未授权动作。策略/凭据层问题，重试无益。
	// 同样 held 升级人工——policy 侧调整或权限授权后 release。
	failurePermission failureClass = "permission"
	// failureInputTooLong 输入超长：上下文窗口不足/prompt 超限。重试同样超长必然再失败，
	// 直接 failed（不烧 attempts）。人工按 retry <id> 时可裁剪 prompt 或换更大窗口的模型。
	failureInputTooLong failureClass = "input_too_long"
	// failureTimeout 超时：步骤超时/远程步骤超时/上游 deadline exceeded。属可重试类，走退避。
	// 保留为独立类是为事件账本审计——按类别聚合能看清"是哪种失败在烧 attempts"。
	failureTimeout failureClass = "timeout"
	// failureExecutorCrash 执行器崩溃：signal killed/aborted、可执行文件找不到等。多为环境瞬时问题，
	// 可重试；沿用退避与 attempts 阈值，超轮限自然 failed。
	failureExecutorCrash failureClass = "executor_crash"
	// failureUnknown 未知类兜底：无法明确归类的错误。**必须**沿用现行 retry_backoff 行为，
	// 不做任何猜测——这是"新分类器不能改动已验证行为"的硬边界。
	failureUnknown failureClass = "unknown"
)

// 分类判据——严格枚举正则，只认服务端与执行器**明确的措辞**，不做泛化关键词猜测。
// 【为什么不复用 codexHardErrRe】codexHardErrRe 是"从 stderr 里挑真错误行"的样式识别器，含 usage
// limit / quota exceeded 等本该被 limitRe 独占的措辞——若拿来做分类会把限额也吞进 auth 类。分类
// 器要与 limitRe 严格互斥，故独立正则、只挑本类别专属措辞。
var (
	// authClassRe 认证类：401 / invalid key / oauth 过期 / 需重新登录。
	// 【为什么不认光"unauthorized"一词】现代 codex/claude 输出常把 "authorization error" 与
	// permission 混用；只认带具体动词/状态码的措辞，避免把权限类误判为认证类导致 held 语义走偏。
	authClassRe = regexp.MustCompile(`(?i)401 unauthorized|invalid api key|authentication failed|oauth token (?:has )?(?:expired|invalid|revoked)|please (?:re-?)?login|please log in again|请重新登录|token expired`)
	// permissionClassRe 权限拒绝：403 / policy 拒绝 / 明确"未授权动作"。
	permissionClassRe = regexp.MustCompile(`(?i)403 forbidden|permission denied|access denied by (?:policy|admin|organization)|not authorized to (?:perform|access)|operation not permitted by (?:policy|admin)|blocked by (?:policy|admin|organization)`)
	// inputTooLongClassRe 输入超长：prompt/context 超限。
	// 【为什么不吃泛化的 "too large"】太宽——磁盘/请求体也可能带 "too large" 词；只认与
	// prompt/context/input 强绑定的措辞。
	inputTooLongClassRe = regexp.MustCompile(`(?i)prompt is too long|prompt too long|context (?:length|window)(?: was)? exceeded|maximum context length|context length exceeded|request too large.*(?:token|prompt|context)|input too long|输入超长|上下文超长`)
	// timeoutClassRe 超时：我们自己拼的中文串（invokeClaude/invokeCodex 的"步骤超时(N 分钟)"
	// "远程步骤超时(N 分钟)"）与常见英文 deadline 措辞。**不认** transientRe 里的 "timed out"——
	// 那属可退避网络类且已被 unknown 分支覆盖（走同样的退避），单独抽到 timeout 类没有行为差异，
	// 反倒模糊边界。
	// 【为什么锚"步骤超时+开括号"而非"^步骤超时"】errorSummary 会把 subtype 拼在 msg 前面
	// （如"codex_error: 步骤超时(60 分钟)"），"^" 锚点在真实 msg 里永远匹配不到；而"步骤超时"
	// 后必带全角/半角开括号的数字分钟格式，是我们内部构造的唯一形态——业务 prompt 里几乎不会
	// 精确出现"步骤超时(数字"这种拼装，故此判据既宽容又精确。
	timeoutClassRe = regexp.MustCompile(`(?i)步骤超时[（(]|远程步骤超时[（(]|远端步骤超时[（(]|context deadline exceeded|deadline exceeded \(`)
	// executorCrashClassRe 执行器崩溃：进程被信号杀 / 二进制找不到。
	executorCrashClassRe = regexp.MustCompile(`(?i)signal: killed|signal: aborted|signal: segmentation fault|signal: quit|signal: bus error|exec(?:utable)?: .* file not found|no such file or directory|找不到可执行文件|executable file not found`)
)

// classifyFailure 判定失败类别。
// 参数：
//   msg      errorSummary 拼好的一行摘要（含 res.Subtype/首行 result 或 runErr+首行 combined）
//   combined stdout+stderr 合并串（长文本，含 codex 天然 transcript 噪声，**不作为分类依据**，
//            仍保留在签名里是给未来可能"锚定错误行框架"式判据留手，不改上层调用点）
//   res      claudeResult（可能为 nil，如 parseClaudeJSON 失败）
//   runErr   进程层错误（可能为 nil，如 res.IsError=true 但进程正常退出）
//
// 【纪律】
//   1. **限额类不属于本分类器**——limitRe 命中的路径在 runTask 前段的 isLimitHit 分支独占处理
//      (写全局冷却/挂 limit_paused)，走到 classifyFailure 时 combined 已经证明不是限额；即便
//      文本恰巧含 "limit-like" 词而无 limitRe 特征，也必须回落 unknown、绝不写全局冷却。
//   2. 判据顺序：auth → permission → input_too_long → timeout → executor_crash → unknown。
//      有交叉措辞（如 "401 unauthorized (forbidden)"）以更精确的 auth 优先，与 held 升级方向一致。
//   3. **只扫 msg**——见文件顶部 P0 教训。combined 全量丢弃：分类信号本该在 msg（errorSummary 的
//      产物），若不在则宁可 unknown 走 retry_backoff，也不能用裸短语正则去 transcript 尾窗里碰运气。
//   4. **本函数只做归类，不做策略降级**——transcript 来源信号（msg 里的分类特征其实是从 combined
//      挑行/取首行经 res.Result 或 fallback 分支旁路进来的）由上层 runTask 调 classificationFromTranscript
//      判断后降级 retry_backoff（见文件顶部 P1 · Round-3 教训）。这里若擅自把 transcript 来源降级
//      unknown，会丢失事件账本的原分类审计信号——分层清晰：本函数出 cls，上层出 policy。
func classifyFailure(msg, combined string, res *claudeResult, runErr error) failureClass {
	// combined/res/runErr 有意不看：msg 已经是 errorSummary 提炼的摘要（第一道防线，见文件顶部
	// P0 教训）；transcript 挑行经 res.Result 旁路的第二道防线由 runTask 侧的 classificationFromTranscript
	// 承接（见 P1 · Round-3 教训）——本函数专职归类，不做策略侧的降级。
	// 参数保留仅为签名向后兼容，避免打散上层调用点(生产调用点在 runner.go:runTask 一处)。Go 未用形参
	// 不报错——若未来加"锚定错误行框架"式判据可以直接用这些字段，无需再改函数签名。
	scan := msg
	if authClassRe.MatchString(scan) {
		return failureAuth
	}
	if permissionClassRe.MatchString(scan) {
		return failurePermission
	}
	if inputTooLongClassRe.MatchString(scan) {
		return failureInputTooLong
	}
	if timeoutClassRe.MatchString(scan) {
		return failureTimeout
	}
	if executorCrashClassRe.MatchString(scan) {
		return failureExecutorCrash
	}
	return failureUnknown
}

// failurePolicy 是分类到策略的映射结果。
// Terminal="" 表示"沿用现行 retry_backoff 行为"（未知类兜底 + 明确可重试类）；
// Terminal=statusHeld / statusFailed 表示直接落终态（held 等人工/failed 不再重试）。
// ConsumesAttempt=false 表示终态不烧 attempts（held/failed 都不烧——它们本身就是"决定不再试"）。
type failurePolicy struct {
	Class           failureClass
	Terminal        string // "" | statusHeld | statusFailed
	ConsumesAttempt bool
	// Reason 写入事件 detail.reason，让审计能一眼看清"为什么落这个终态"。
	Reason string
}

// policyFor 返回分类的策略。
// 【为什么策略表放独立函数】mutation-kill 测试要求"把 auth→held 改成无限重试必须报红"——把
// 策略集中在此函数里，测试断言此函数返回值即可覆盖；策略若散落在 switch case 里，改一处漏一处
// 的风险更高，且难被单点断言覆盖。
func policyFor(cls failureClass) failurePolicy {
	switch cls {
	case failureAuth:
		return failurePolicy{Class: cls, Terminal: statusHeld, ConsumesAttempt: false, Reason: "auth_class_held"}
	case failurePermission:
		return failurePolicy{Class: cls, Terminal: statusHeld, ConsumesAttempt: false, Reason: "permission_class_held"}
	case failureInputTooLong:
		return failurePolicy{Class: cls, Terminal: statusFailed, ConsumesAttempt: false, Reason: "input_too_long_no_retry"}
	case failureTimeout, failureExecutorCrash, failureUnknown:
		// 走现行 retry_backoff：终态由"是否超 max_attempts"决定，与旧行为逐字节一致。
		return failurePolicy{Class: cls, Terminal: "", ConsumesAttempt: true}
	}
	// 未来若新增枚举忘更新策略表：兜底回未知类的现行行为（宁可回归也不静默改变行为）。
	return failurePolicy{Class: failureUnknown, Terminal: "", ConsumesAttempt: true}
}

// annotatedError 给 LastError 加上 [class] 前缀，让 CLI/看板/人工审计一眼可辨。保留原始 msg 全文
// 方便定位真因。
//
// 【P1 教训 · Round-2 复审】早期版本仅豁免 unknown 不加前缀，其它类（含可重试的 timeout/
// executor_crash）也加前缀。但双语 README 同 commit 承诺的契约是"仅 auth/permission/input_too_long
// 带 [<class>] 前缀"——契约文档与代码直接矛盾；且回归基线要求"重试类的错误呈现与旧版逐字节一致"
// 并不仅止于 unknown（timeout/executor_crash 同属重试类，旧版 LastError 无前缀，加前缀就是行为
// 漂移）。修法：以策略表 policyFor(cls).Terminal 为判据——仅**终态类**（auth/permission → held；
// input_too_long → failed）加前缀，重试类（timeout/executor_crash/unknown/未来新增的可重试类）
// 一律不加。判据挂在策略表上：README 契约与策略表同源，未来若新增终态类，前缀自动跟随策略扩展，
// 不必手动同步这里。
// classificationFromTranscript 判断 classifyFailure 用的 msg 是否源自 combined(transcript)/agent
// 任意生成内容。是则 runTask 侧必须把终态分类(held/failed) **降级** retry_backoff——transcript/
// agent 终稿天然充斥 permission denied / 401 unauthorized / context length exceeded 等分类正则
// 字面量(审查引用/工具错误输出/正常叙述),不能作为不可重试终态的判据(基线本会退避自愈的超时/
// 瞬时抖动会被误判静默停摆)。
//
// 【为什么这条判据挂在 failure_class.go】它与 classifyFailure/policyFor 是**同一条策略链**的
// 三段——归类(classifyFailure)→策略(policyFor)→降级(classificationFromTranscript)。放这里
// 便于把"transcript 来源不落终态"这个第二道防线与文件顶部的第一道防线(msg-only)成对阅读、
// 成对维护。runTask 只做调用与事件写入,不掺策略逻辑。
//
// 【判据 · 与 errorSummary 三条分支一一对齐】
// errorSummary(res, combined, runErr) 有三条 msg 构造路径:
//
//	path 1: res != nil && res.IsError   → msg = res.Subtype + ": " + firstLine(res.Result)
//	path 2: runErr != nil               → msg = runErr.Error() + " | " + firstLine(combined)
//	path 3: 其它(res==nil && runErr==nil) → msg = "无法解析 claude 输出 | " + firstLine(combined)
//
// path 1 的 res.Result 来源分两种:
//   - claude 结构化 JSON 的 API 错误(parseClaudeJSON 产物)——**非 transcript**,可信作终态判据;
//   - invokeCodex/invokeRemoteCodex/invokeRemoteClaude 从 combined 挑行(codexErrorLine)、取首行
//     (firstLine(combined)) 或 codex -o 文件的 agent 终稿——**是 transcript/agent 任意内容**,
//     不应作终态判据。invoke 侧在这些路径上会给 res.ResultFromTranscript 打标以区分二者。
//
// path 2 与 path 3 均直接把 firstLine(combined) 拼进 msg——**恒 transcript 来源**,永不作终态判据。
//
// 故判据:
//   - path 1 命中(res != nil && res.IsError)时,回 res.ResultFromTranscript(true=transcript,
//     false=结构化);
//   - 其它情形(path 2/3)恒回 true。
//
// 【为什么允许把 runErr!=nil 的 Go 侧措辞也视为 transcript 来源】"步骤超时(60 分钟)"/"signal:
// killed" 由 Go 侧构造非 transcript,但走 path 2 也被本函数判 true——这符合分类语义:此类真错误
// 属可重试类(timeout/executor_crash),policy.Terminal="" 本就不受降级影响,误伤面为零;宁可宽容
// 也不能漏放 transcript 来源。
//
// 【为什么 res==nil && runErr==nil 也要判 true】invokeClaude 在 claude CLI 退出 0 但 stdout 非
// JSON 时会返回 (nil, combined, nil);errorSummary 走 path 3 拼 firstLine(combined) 进 msg,分类
// 信号完全来自 transcript。早期版本漏该分支导致纯 CLI 无解析输出的边缘案例可被误判 held——本轮
// 补齐。
func classificationFromTranscript(res *claudeResult, runErr error) bool {
	// path 1: res 有结构化 IsError → 由 invoke 侧打的 ResultFromTranscript 标决定。
	if res != nil && res.IsError {
		return res.ResultFromTranscript
	}
	// path 2/3: msg 恒含 firstLine(combined),视为 transcript 来源。
	return true
}

func annotatedError(cls failureClass, msg string) string {
	if policyFor(cls).Terminal == "" {
		return msg
	}
	// 极端长的 msg 也不截断：last_error 供人工诊断，宁可长也别丢关键上下文。
	return "[" + string(cls) + "] " + strings.TrimSpace(msg)
}
