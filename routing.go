package main

import (
	"fmt"
	"strings"
)

func invalidExplicitRouteClass(t *Task) bool {
	if t == nil {
		return false
	}
	class := strings.ToLower(strings.TrimSpace(t.RouteClass))
	return class != "" && class != routeClassBackend && class != routeClassGeneral
}

// ownerRoutingPolicyWaitReason centralizes fail-closed task-shape guards consumed by tick, board,
// and manual takeover. A manually edited persisted card must not bypass the add-time validator and
// silently fall into a different model lane.
func ownerRoutingPolicyWaitReason(cfg *Config, t *Task) string {
	if cfg == nil || !cfg.OwnerRoutingEnforced || t == nil {
		return ""
	}
	if invalidExplicitRouteClass(t) {
		return fmt.Sprintf("route_class=%q 无效；仅允许 backend|general", t.RouteClass)
	}
	if err := closedOwnerTaskStateError(t); err != nil {
		return "Owner 闭合路由状态无效：" + err.Error()
	}
	if cursorFableOwnerRouteRequired(cfg, t) {
		return "显式 Fable 当前形状/配置无法解析完整 Owner 路由"
	}
	return ""
}

func validateNewTaskRouteClass(cfg *Config, t *Task) error {
	if t == nil || t.Type != typeSequence {
		return nil
	}
	class := strings.ToLower(strings.TrimSpace(t.RouteClass))
	if invalidExplicitRouteClass(t) {
		return fmt.Errorf("route_class 只允许 backend|general，收到 %q", t.RouteClass)
	}
	if cfg != nil && cfg.OwnerRoutingEnforced && modelTierKeyword(cfg, t.Model) == "fable" {
		// Fable is a read-only decision/synthesis lineage. Subject matter cannot classify the Fable
		// card as backend; any later implementation is a separate card with its own classification.
		t.RouteClass = routeClassGeneral
		return nil
	}
	if cfg != nil && cfg.OwnerRoutingEnforced && class == "" {
		return fmt.Errorf("owner_routing_enforced=true 时新 sequence 卡必须显式指定 route_class=backend|general")
	}
	return nil
}

// mandatoryHighRiskCategory reports the closed Owner high-risk category a sequence task's declared
// work falls into, independent of its mutable risk_class metadata. Identity/credential,
// DB/schema/migration, protocol/network execution, manifest/launchd, Control/authority, live
// cutover, security, and funds can never be downgraded to ordinary by card metadata; the resolver
// fails them closed into the high-risk route. Returns "" when no category matches.
func mandatoryHighRiskCategory(t *Task) string {
	if t == nil || t.Type != typeSequence {
		return ""
	}
	haystack := strings.ToLower(t.Title + "\n" + strings.Join(t.Prompts, "\n"))
	// Keep the markers grouped by the eight authority categories so docs/templates/tests can audit
	// exact parity. Category hits never downgrade; absence never upgrades a backend card whose
	// risk is already fail-closed high by omission.
	groups := []struct {
		category string
		markers  []string
	}{
		{"identity/credential", []string{
			"身份", "凭据", "认证", "证书", "密钥轮换", "令牌刷新",
			"identity", "credential", "authentication", "certificate", "key rotation",
			"oauth", "jwt", "token refresh", "refresh token", "access token",
		}},
		{"db/schema/migration", []string{
			"数据库", "数据表", "数据库迁移", "模式迁移", "database", "schema", "migration",
		}},
		{"protocol/network execution", []string{
			"协议", "握手", "网络执行", "网络调用", "网络请求", "接口开发", "接口实现", "网络重试",
			"protocol", "wire format", "handshake", "network execution", "network request", "network retry",
			"api endpoint", "rest api", "graphql api",
		}},
		{"manifest/launchd", []string{
			"运行清单", "启动清单", "服务清单", "launchd", "manifest", "launch agent", "launch daemon",
		}},
		{"control/authority", []string{
			"权限", "控制权限", "控制面", "授权边界", "权威边界",
			"authority", "authority boundary", "control authority", "control plane",
		}},
		{"live cutover", []string{
			"在线切换", "实时切换", "生产切换", "上线切换", "live cutover", "production cutover", "go-live cutover",
		}},
		{"security", []string{
			"安全", "漏洞", "越权", "注入", "加密", "解密", "签名校验", "签名验证",
			"security", "vulnerability", "exploit", "injection", "encryption", "decryption", "signature verification",
		}},
		{"funds", []string{
			"资金", "支付", "扣款", "转账", "结算", "退款", "计费",
			"funds", "payment", "transfer", "settlement", "refund", "billing", "charge",
		}},
	}
	for _, group := range groups {
		for _, marker := range group.markers {
			if strings.Contains(haystack, marker) {
				return group.category
			}
		}
	}
	return ""
}

// backendDevelopmentTask 先尊重任务卡的显式分类；存量卡为空时仅对 sequence 卡做确定性文本判定。
// general 是人工消歧开关，可覆盖标题中诸如“后端对比”但实际不改后端的误命中。
func backendDevelopmentTask(t *Task) bool {
	if t == nil {
		return false
	}
	class := strings.ToLower(strings.TrimSpace(t.RouteClass))
	switch class {
	case routeClassBackend:
		return true
	case routeClassGeneral:
		return false
	}
	if class != "" {
		// Unknown explicit values are never reinterpreted from prompt prose. New-card boundaries reject
		// them; this guard keeps manually edited legacy JSON from silently becoming backend/general.
		return false
	}
	if t.Type != typeSequence {
		return false
	}
	haystack := strings.ToLower(t.Title + "\n" + strings.Join(t.Prompts, "\n"))
	// Owner backend definition is broader than a conventional web backend. Keep the compatibility
	// markers grouped by the nine authority categories so docs/templates/tests can audit exact parity:
	// service, persistence, protocol, database, network execution, identity/credential,
	// manifest/launchd, Control/authority, and live cutover.
	markers := []string{
		// service
		"后端", "服务", "服务端", "服务层", "服务生命周期", "微服务", "守护进程", "调度器", "消息队列", "网关",
		"backend", "back-end", "server-side", "service", "service layer", "service lifecycle", "microservice", "daemon", "scheduler", "queue worker", "gateway",
		// persistence
		"持久化", "存储层", "状态存储", "缓存服务", "persistence", "storage layer", "state store", "redis", "cache service",
		// protocol
		"协议", "握手", "protocol", "wire format", "handshake",
		// database
		"数据库", "数据表", "数据库迁移", "database", "database migration", "schema migration",
		// network execution
		"网络执行", "网络调用", "网络请求", "接口开发", "接口实现", "网络重试",
		"network execution", "network request", "network retry", "api endpoint", "rest api", "graphql api",
		// identity / credential
		"身份", "凭据", "认证", "证书", "密钥轮换", "令牌刷新", "identity", "credential", "authentication", "certificate", "key rotation",
		"oauth", "jwt", "token refresh", "refresh token", "access token",
		// manifest / launchd
		"清单", "运行清单", "启动清单", "服务清单", "launchd", "manifest", "launch agent", "launch daemon",
		// Control / authority
		"control 权限", "权限", "控制权限", "控制面", "授权边界", "权威边界", "authority", "authority boundary", "control authority", "control plane",
		// live cutover
		"在线切换", "实时切换", "生产切换", "上线切换", "live cutover", "production cutover", "go-live cutover",
	}
	for _, marker := range markers {
		if strings.Contains(haystack, marker) {
			return true
		}
	}
	return false
}
