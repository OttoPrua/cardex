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
//   combined stdout+stderr 合并串（长文本，含真错误行）
//   res      claudeResult（可能为 nil，如 parseClaudeJSON 失败）
//   runErr   进程层错误（可能为 nil，如 res.IsError=true 但进程正常退出）
//
// 【纪律】
//   1. **限额类不属于本分类器**——limitRe 命中的路径在 runTask 前段的 isLimitHit 分支独占处理
//      (写全局冷却/挂 limit_paused)，走到 classifyFailure 时 combined 已经证明不是限额；即便
//      文本恰巧含 "limit-like" 词而无 limitRe 特征，也必须回落 unknown、绝不写全局冷却。
//   2. 判据顺序：auth → permission → input_too_long → timeout → executor_crash → unknown。
//      有交叉措辞（如 "401 unauthorized (forbidden)"）以更精确的 auth 优先，与 held 升级方向一致。
//   3. 只扫 combined + msg 前 8KB——真错误行几乎都在末尾几百字节，超长文本扫全量既慢又易被推理
//      正文里的巧合词命中。用 tailWindow 取合并串末段扫描。
func classifyFailure(msg, combined string, res *claudeResult, runErr error) failureClass {
	// 取扫描窗口：msg 一定看（它是摘要，最集中）；combined 只看末段 8KB（真错误几乎都在末尾）。
	scan := msg + "\n" + tailWindow(combined, 8192)
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

// tailWindow 取字符串末段 n 字节（rune 边界不切）。长文本扫描窗口，避免推理正文里的巧合词命中。
func tailWindow(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// 从 n 字节前找一个 rune 起点（找不到就直接切；rune 切错顶多让首字符成半个 utf8，
	// 正则匹配上不会命中假类别——真错误行仍在末段完整存在）。
	start := len(s) - n
	for start < len(s) && (s[start]&0xC0) == 0x80 { // utf8 continuation byte
		start++
	}
	return s[start:]
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

// annotatedError 给 LastError 加上 [class] 前缀，人工/审计能一眼看清分类。
// 保留原始 msg 全文，方便定位真因。
func annotatedError(cls failureClass, msg string) string {
	if cls == failureUnknown {
		// 未知类不加前缀——回归基线要求"错误呈现与旧版逐字节一致"，加前缀会污染 last_error。
		return msg
	}
	// 极端长的 msg 也不截断：last_error 供人工诊断，宁可长也别丢关键上下文。
	return "[" + string(cls) + "] " + strings.TrimSpace(msg)
}
