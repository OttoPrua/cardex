package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// R8 sender-receives delivery.
//
// The v1 production shape is one Codex thread that is simultaneously the dispatch
// session and the wake report endpoint. This file adds the *coordinate* layer around
// that: every card may pin, at enqueue, who asked for it (`requester_id`) and where the
// terminal wake must land (`endpoint_kind` + `endpoint_thread`). Delivery then derives a
// subscription identity `owner-<requester_id>` per requester and reuses the existing
// cursor/receipt/inflight v1 protocol verbatim — the durability, coalescing and
// fail-closed rules of manager_wake.go are not re-implemented here.
//
// Only two endpoint kinds execute in v1: codex-thread and root. external-agent,
// cursor-agent and management-card are reserved fail-closed coordinates: a card may
// declare them, and delivery then refuses with owner_endpoint_unsupported rather than
// silently retargeting the reply at whatever thread happens to be configured.
const (
	// ownerWakeSubPrefix is reserved. Config subscriptions may not use it, so a
	// configured id can never collide with a derived requester identity.
	ownerWakeSubPrefix = "owner-"
	// managerWakeRoleRoot marks the single root reply endpoint.
	managerWakeRoleRoot = "root"
)

// Closed owner-routing failure classes. All of them are pre-claim endpoint
// reachability failures, which is exactly the scope in which one requester's problem is
// allowed to be isolated from another's. Anything that touches shared state
// (outbox/cursor/receipt/inflight/codex bin) keeps the existing pass-level fail-stop.
const (
	ownerErrEndpointUnsupported = "owner_endpoint_unsupported"
	ownerErrInvalidRequester    = "owner_invalid_requester"
	ownerErrInvalidThread       = "owner_invalid_thread"
	ownerErrRouteSchema         = "owner_route_schema_rejected"
	ownerErrRouteFieldRejected  = "owner_route_field_rejected"
	ownerErrRouteConflict       = "owner_route_conflict"
	ownerErrRootUnreachable     = "owner_root_unreachable"
)

// ownerWakeBlock is a durable, per-requester fail-closed coordinate: the requester was
// identified, its endpoint was not reachable, and nothing was queued for it.
type ownerWakeBlock struct {
	SubscriptionID string
	RequesterID    string
	TaskIDs        []string
	Class          string
}

// ownerWakeRoutePlan is the whole delivery shape for one wake pass.
type ownerWakeRoutePlan struct {
	// configSubs are the configured subscriptions with routed cards removed from
	// their scope, plus the root subscription widened by root-endpoint and escalated
	// cards. Order matches config order so delivery stays deterministic.
	configSubs []ManagerWakeSubscription
	ownerSubs  []ManagerWakeSubscription
	blocked    []ownerWakeBlock
}

func (p *ownerWakeRoutePlan) ownerSubscriptions() []ManagerWakeSubscription {
	if p == nil {
		return nil
	}
	return p.ownerSubs
}

func ownerWakeRoutingEnabled(mw *ManagerWakeConfig) bool {
	return managerWakeEnabled(mw) && mw.OwnerRouting
}

// ownerWakeSubscriptionID derives the delivery identity for a requester. The derived id
// must itself satisfy the closed subscription-id grammar, because it names a file under
// cursors/, receipts/ and inflight/.
func ownerWakeSubscriptionID(requesterID string) (string, bool) {
	id := strings.TrimSpace(requesterID)
	if id == "" || id != requesterID {
		return "", false
	}
	if _, ok := closedManagerWakeSubID(id); !ok {
		return "", false
	}
	// A requester that already carries the reserved prefix would make
	// owner-owner-x indistinguishable from a requester literally named owner-x.
	if strings.HasPrefix(id, ownerWakeSubPrefix) {
		return "", false
	}
	full := ownerWakeSubPrefix + id
	if _, ok := closedManagerWakeSubID(full); !ok {
		return "", false
	}
	return full, true
}

// closedTaskReplyRoute validates card-face shape only, and normalizes what delivery
// compares on. Reserved endpoint kinds are shape-valid on purpose: R8 v1 accepts them at
// enqueue as create-only coordinates and refuses them at delivery.
func closedTaskReplyRoute(r *TaskReplyRoute) (TaskReplyRoute, string) {
	if r == nil {
		return TaskReplyRoute{}, ownerErrRouteSchema
	}
	out := *r
	out.Schema = strings.TrimSpace(out.Schema)
	if out.Schema == "" {
		out.Schema = taskReplyRouteSchemaV1
	}
	if out.Schema != taskReplyRouteSchemaV1 {
		return out, ownerErrRouteSchema
	}
	if _, ok := ownerWakeSubscriptionID(out.RequesterID); !ok {
		return out, ownerErrInvalidRequester
	}
	switch out.EndpointKind {
	case replyEndpointCodexThread:
		thread, ok := canonicalManagerWakeThreadID(out.EndpointThread)
		if !ok {
			return out, ownerErrInvalidThread
		}
		out.EndpointThread = thread
	case replyEndpointRoot, replyEndpointExternalAgent, replyEndpointCursorAgent, replyEndpointManagementCard:
		// v1 addresses these by role, not by thread. A thread here would be an
		// unhonored instruction, which is worse than a rejection.
		if out.EndpointThread != "" {
			return out, ownerErrInvalidThread
		}
	default:
		return out, ownerErrEndpointUnsupported
	}
	for _, f := range []string{out.TargetManagementConversation, out.CallbackTopLevelSession, out.ReceiptRoute} {
		if f != strings.TrimSpace(f) || strings.ContainsAny(f, "\n\r\x00") {
			return out, ownerErrRouteFieldRejected
		}
	}
	return out, ""
}

// replyEndpointExecutable reports whether R8 v1 can actually deliver to a kind.
func replyEndpointExecutable(kind string) bool {
	return kind == replyEndpointCodexThread || kind == replyEndpointRoot
}

// closedReplyRouteForEnqueue is the enqueue-time gate. It accepts reserved endpoints
// (create-only) and rejects anything structurally malformed, so a card can never be
// pinned to a coordinate delivery would later have to guess about.
func closedReplyRouteForEnqueue(r *TaskReplyRoute) (*TaskReplyRoute, error) {
	if r == nil {
		return nil, nil
	}
	closed, class := closedTaskReplyRoute(r)
	if class != "" {
		return nil, fmt.Errorf("%s", class)
	}
	return &closed, nil
}

// applyTaskReplyRoute pins the enqueue-time reply coordinate from the CLI. An empty flag
// leaves the card routeless, which is the legacy config-matched shape.
func applyTaskReplyRoute(t *Task, raw string) error {
	raw = strings.TrimSpace(raw)
	if t == nil || raw == "" {
		return nil
	}
	var route TaskReplyRoute
	if err := decodeClosedJSON([]byte(raw), &route); err != nil {
		return fmt.Errorf("-reply-route 不是封闭的 %s JSON: %v", taskReplyRouteSchemaV1, err)
	}
	closed, err := closedReplyRouteForEnqueue(&route)
	if err != nil {
		return fmt.Errorf("-reply-route 被拒: %w", err)
	}
	t.ReplyRoute = closed
	return nil
}

func replyRouteRequesterLabel(r *TaskReplyRoute) string {
	if r == nil {
		return ""
	}
	return r.RequesterID
}

func replyRouteEndpointLabel(r *TaskReplyRoute) string {
	if r == nil {
		return ""
	}
	return r.EndpointKind
}

type ownerWakeGroup struct {
	subID       string
	requesterID string
	thread      string
	taskIDs     []string
	class       string
}

// planOwnerWakeDeliveries turns the outbox plus card-face routes into the exact set of
// subscriptions this pass will deliver. The returned class is a shared-state failure and
// stops the whole pass; per-requester endpoint failures come back in plan.blocked.
func planOwnerWakeDeliveries(root string, mw *ManagerWakeConfig, rows []managerWakeOutboxRow) (*ownerWakeRoutePlan, string, error) {
	plan := &ownerWakeRoutePlan{}
	rootIdx := -1
	// Delivery skips a disabled subscription, so a switched-off role=root endpoint is
	// unreachable in exactly the way a missing one is.
	rootReachable := false
	for i, sub := range mw.Subscriptions {
		id, ok := closedManagerWakeSubID(sub.ID)
		if !ok {
			return nil, "invalid_subscription_id", fmt.Errorf("invalid_subscription_id")
		}
		if strings.HasPrefix(id, ownerWakeSubPrefix) {
			return nil, "reserved_subscription_prefix", fmt.Errorf("reserved_subscription_prefix")
		}
		switch strings.TrimSpace(sub.Role) {
		case "":
		case managerWakeRoleRoot:
			if rootIdx >= 0 {
				return nil, "duplicate_root_subscription", fmt.Errorf("duplicate_root_subscription")
			}
			rootIdx = i
			rootReachable = sub.Enabled
		default:
			return nil, "invalid_subscription_role", fmt.Errorf("invalid_subscription_role")
		}
	}

	routes, class, err := loadOutboxReplyRoutes(root, rows)
	if err != nil || class != "" {
		return nil, class, err
	}

	groups := map[string]*ownerWakeGroup{}
	order := []string{}
	rootExtra := map[string]bool{}
	routedTasks := map[string]bool{}
	group := func(subID, requesterID string) *ownerWakeGroup {
		g, ok := groups[subID]
		if !ok {
			g = &ownerWakeGroup{subID: subID, requesterID: requesterID}
			groups[subID] = g
			order = append(order, subID)
		}
		return g
	}

	for _, taskID := range sortedRouteKeys(routes) {
		// Any card carrying a route leaves legacy config matching, valid or not.
		// Fail-closed: a malformed route means nobody receives that wake.
		routedTasks[taskID] = true
		route := routes[taskID]
		closed, rclass := closedTaskReplyRoute(route)
		subID, idOK := ownerWakeSubscriptionID(closed.RequesterID)
		if !idOK {
			// Unusable requester: reportable, but it must never name a file.
			subID = ""
		}
		g := group(subID, closed.RequesterID)
		if !idOK {
			g.requesterID = sanitizedRequesterLabel(route.RequesterID)
		}
		g.taskIDs = append(g.taskIDs, taskID)
		if rclass != "" {
			g.markClass(rclass)
			continue
		}
		if !replyEndpointExecutable(closed.EndpointKind) {
			g.markClass(ownerErrEndpointUnsupported)
			continue
		}
		wantRoot := closed.EndpointKind == replyEndpointRoot || closed.EscalateToRoot
		if wantRoot && !rootReachable {
			g.markClass(ownerErrRootUnreachable)
			continue
		}
		if closed.EndpointKind == replyEndpointCodexThread {
			if g.thread != "" && g.thread != closed.EndpointThread {
				// Two cards claim the same requester but different threads.
				// Picking either one would deliver somebody's reply to the
				// wrong session, so the requester is held whole.
				g.markClass(ownerErrRouteConflict)
				continue
			}
			g.thread = closed.EndpointThread
		}
		if wantRoot {
			rootExtra[taskID] = true
		}
	}

	// A requester with any blocked card is blocked whole: a half-delivered owner is
	// indistinguishable, downstream, from a complete one.
	for _, subID := range order {
		g := groups[subID]
		if g.class != "" {
			for _, id := range g.taskIDs {
				delete(rootExtra, id)
			}
		}
	}

	for _, subID := range order {
		g := groups[subID]
		sort.Strings(g.taskIDs)
		if g.class != "" {
			plan.blocked = append(plan.blocked, ownerWakeBlock{
				SubscriptionID: subID,
				RequesterID:    g.requesterID,
				TaskIDs:        append([]string(nil), g.taskIDs...),
				Class:          g.class,
			})
			continue
		}
		if g.thread == "" {
			// Every remaining card of this requester replies at root; there is
			// no separate owner endpoint to queue.
			continue
		}
		owned := make([]string, 0, len(g.taskIDs))
		for _, id := range g.taskIDs {
			owned = append(owned, id)
		}
		plan.ownerSubs = append(plan.ownerSubs, ManagerWakeSubscription{
			ID:       subID,
			ThreadID: g.thread,
			TaskIDs:  owned,
			Enabled:  true,
			derived:  true,
		})
	}
	sort.Slice(plan.ownerSubs, func(i, j int) bool { return plan.ownerSubs[i].ID < plan.ownerSubs[j].ID })

	plan.configSubs = make([]ManagerWakeSubscription, 0, len(mw.Subscriptions))
	for i, sub := range mw.Subscriptions {
		next := sub
		next.Projects = append([]string(nil), sub.Projects...)
		next.TaskIDs = append([]string(nil), sub.TaskIDs...)
		next.DirPrefixes = append([]string(nil), sub.DirPrefixes...)
		next.EventTypes = append([]string(nil), sub.EventTypes...)
		exclude := map[string]bool{}
		for id := range routedTasks {
			exclude[id] = true
		}
		if rootReachable && i == rootIdx {
			listed := map[string]bool{}
			for _, id := range next.TaskIDs {
				listed[strings.TrimSpace(id)] = true
			}
			for _, id := range sortedIDSet(rootExtra) {
				delete(exclude, id)
				if listed[id] {
					continue
				}
				listed[id] = true
				next.TaskIDs = append(next.TaskIDs, id)
			}
		}
		next.excludeTaskIDs = exclude
		plan.configSubs = append(plan.configSubs, next)
	}
	return plan, "", nil
}

func (g *ownerWakeGroup) markClass(class string) {
	if g.class == "" {
		g.class = class
	}
}

// loadOutboxReplyRoutes reads each distinct outbox task once. A task that is gone
// (archived away, cleaned) contributes no route: its rows already bind stale and are
// suppressed by the existing delivery path.
func loadOutboxReplyRoutes(root string, rows []managerWakeOutboxRow) (map[string]*TaskReplyRoute, string, error) {
	routes := map[string]*TaskReplyRoute{}
	seen := map[string]bool{}
	for _, row := range rows {
		id, ok := exactCanonicalOutgoingTaskID(row.TaskID)
		if !ok {
			return nil, "outbox_identity_mismatch", fmt.Errorf("outbox_identity_mismatch")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		t, err := findTaskAnywhere(root, id)
		if err != nil && t == nil {
			if strings.Contains(err.Error(), "不存在") {
				continue
			}
			return nil, "outbox_identity_mismatch", fmt.Errorf("outbox_identity_mismatch")
		}
		if t == nil || t.ReplyRoute == nil {
			continue
		}
		routes[id] = t.ReplyRoute
	}
	return routes, "", nil
}

const ownerBlockedSchemaV1 = "cardex.manager_wake.owner_blocked.v1"

type managerWakeOwnerBlockRow struct {
	SubscriptionID string   `json:"subscription_id,omitempty"`
	RequesterID    string   `json:"requester_id"`
	Class          string   `json:"class"`
	TaskIDs        []string `json:"task_ids,omitempty"`
}

type managerWakeOwnerBlockState struct {
	Schema    string                     `json:"schema"`
	Blocked   []managerWakeOwnerBlockRow `json:"blocked"`
	UpdatedAt string                     `json:"updated_at"`
}

func managerWakeOwnerBlockPath(root string) string {
	return filepath.Join(managerWakeDir(root), "owner-blocked.json")
}

// recordOwnerWakeBlocks writes the refused requesters of this pass to one durable file,
// so a fail-closed endpoint stays a visible coordinate instead of a log line that is gone
// on the next scan. It deliberately does not touch the global error state or any owner
// cursor: an isolated endpoint failure must not present itself as a whole-plane failure,
// and an unusable requester id must never get to name a file.
func recordOwnerWakeBlocks(root string, plan *ownerWakeRoutePlan) error {
	path := managerWakeOwnerBlockPath(root)
	if plan == nil || len(plan.blocked) == 0 {
		return durableUnlinkFile(path)
	}
	st := managerWakeOwnerBlockState{Schema: ownerBlockedSchemaV1, UpdatedAt: time.Now().Format(time.RFC3339Nano)}
	for _, blocked := range plan.blocked {
		st.Blocked = append(st.Blocked, managerWakeOwnerBlockRow{
			SubscriptionID: blocked.SubscriptionID,
			RequesterID:    blocked.RequesterID,
			Class:          blocked.Class,
			TaskIDs:        blocked.TaskIDs,
		})
	}
	sort.Slice(st.Blocked, func(i, j int) bool {
		if st.Blocked[i].SubscriptionID != st.Blocked[j].SubscriptionID {
			return st.Blocked[i].SubscriptionID < st.Blocked[j].SubscriptionID
		}
		return st.Blocked[i].RequesterID < st.Blocked[j].RequesterID
	})
	if err := os.MkdirAll(managerWakeDir(root), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteSync(path, append(data, '\n'))
}

func loadOwnerWakeBlocks(root string) []managerWakeOwnerBlockRow {
	data, err := os.ReadFile(managerWakeOwnerBlockPath(root))
	if err != nil {
		return nil
	}
	var st managerWakeOwnerBlockState
	if err := decodeClosedJSON(data, &st); err != nil || st.Schema != ownerBlockedSchemaV1 {
		return []managerWakeOwnerBlockRow{{RequesterID: "-", Class: "owner_block_state_corrupt"}}
	}
	return st.Blocked
}

// ownerWakeBlockReadback reports refused requesters for `manager-wake status` from the
// durable record, not from the last in-memory pass.
func ownerWakeBlockReadback(root string, mw *ManagerWakeConfig) []map[string]any {
	out := []map[string]any{}
	if !ownerWakeRoutingEnabled(mw) {
		return out
	}
	for _, b := range loadOwnerWakeBlocks(root) {
		out = append(out, map[string]any{
			"id":             b.SubscriptionID,
			"requester_id":   b.RequesterID,
			"last_err_class": b.Class,
			"task_ids":       append([]string(nil), b.TaskIDs...),
		})
	}
	return out
}

func sortedRouteKeys(routes map[string]*TaskReplyRoute) []string {
	ids := make([]string, 0, len(routes))
	for id := range routes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func sortedIDSet(set map[string]bool) []string {
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// sanitizedRequesterLabel keeps an unusable requester reportable without ever letting it
// name a file: the result is only used as a map key and in readback text.
func sanitizedRequesterLabel(raw string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
		if b.Len() >= 32 {
			break
		}
	}
	if b.Len() == 0 {
		return "unnamed"
	}
	return b.String()
}
