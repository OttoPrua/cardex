package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

var (
	errDAGMalformedID          = errors.New("dependency dag malformed identifier")
	errDAGDuplicateNode        = errors.New("dependency dag duplicate node")
	errDAGCycle                = errors.New("dependency dag cycle")
	errDAGMissingDependency    = errors.New("dependency dag missing dependency")
	errDAGUnknownDomainBinding = errors.New("dependency dag unknown domain binding")
)

// DependencyNode is one closed DAG vertex. Lineage is required integration
// lineage naming. DomainID, when set, must be a well-formed write-domain id;
// binding that id onto a live WriteDomain is a residual integration seam.
type DependencyNode struct {
	ID        string
	Lineage   string
	DomainID  string
	DependsOn []string
	Satisfied bool
}

// MissingDependency names a DependsOn edge whose target is not in the graph.
type MissingDependency struct {
	NodeID    string
	MissingID string
}

// DAGDiagnosis is a deterministic readiness report. Ready IDs are sorted.
// Cycles are SCC member lists rotated onto the least identifier.
type DAGDiagnosis struct {
	Ready   []string
	Cycles  [][]string
	Missing []MissingDependency
}

// DAGAuditError carries cycle and missing-dependency diagnosis. Malformed
// identifiers are returned as bare sentinels without diagnosis.
type DAGAuditError struct {
	Diagnosis DAGDiagnosis
}

func (e *DAGAuditError) Error() string {
	if e == nil {
		return "dependency dag audit"
	}
	return fmt.Sprintf("dependency dag: %d cycle(s), %d missing dependency(ies)",
		len(e.Diagnosis.Cycles), len(e.Diagnosis.Missing))
}

func (e *DAGAuditError) Is(target error) bool {
	if e == nil {
		return false
	}
	return (target == errDAGCycle && len(e.Diagnosis.Cycles) > 0) ||
		(target == errDAGMissingDependency && len(e.Diagnosis.Missing) > 0)
}

// AnalyzeDependencyDAG validates identifiers, then reports ready nodes,
// cycles, and missing edges. Input order does not affect Ready, Cycles, or
// Missing. Any cycle, missing dependency, or other DAG audit error
// fail-closes Ready authorization; Cycles and Missing remain inspectable.
func AnalyzeDependencyDAG(nodes []DependencyNode) (DAGDiagnosis, error) {
	byID := make(map[string]DependencyNode, len(nodes))
	order := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if err := validateDependencyNode(n); err != nil {
			return DAGDiagnosis{}, err
		}
		if _, ok := byID[n.ID]; ok {
			return DAGDiagnosis{}, fmt.Errorf("%w: %s", errDAGDuplicateNode, n.ID)
		}
		byID[n.ID] = n
		order = append(order, n.ID)
	}

	var missing []MissingDependency
	edges := make(map[string][]string, len(byID))
	for _, id := range order {
		n := byID[id]
		seenDep := map[string]bool{}
		deps := make([]string, 0, len(n.DependsOn))
		for _, dep := range n.DependsOn {
			if seenDep[dep] {
				return DAGDiagnosis{}, fmt.Errorf("%w: %s -> %s", errDAGDuplicateNode, n.ID, dep)
			}
			seenDep[dep] = true
			if _, ok := byID[dep]; !ok {
				missing = append(missing, MissingDependency{NodeID: n.ID, MissingID: dep})
				continue
			}
			deps = append(deps, dep)
		}
		sort.Strings(deps)
		edges[id] = deps
	}
	sort.Slice(missing, func(i, j int) bool {
		if missing[i].NodeID != missing[j].NodeID {
			return missing[i].NodeID < missing[j].NodeID
		}
		return missing[i].MissingID < missing[j].MissingID
	})

	cycles := diagnoseCycles(order, edges)
	if len(cycles) > 0 || len(missing) > 0 {
		diag := DAGDiagnosis{Cycles: cycles, Missing: missing}
		return diag, &DAGAuditError{Diagnosis: diag}
	}

	ready := make([]string, 0)
	sort.Strings(order)
	for _, id := range order {
		n := byID[id]
		if n.Satisfied {
			continue
		}
		blocked := false
		for _, dep := range n.DependsOn {
			target, ok := byID[dep]
			if !ok || !target.Satisfied {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		ready = append(ready, id)
	}

	return DAGDiagnosis{Ready: ready}, nil
}

// BindDependencyDomains fail-closes when a node names a DomainID that is not
// the exact identifier of a provided write domain. Empty DomainID is unbound.
func BindDependencyDomains(nodes []DependencyNode, domains []WriteDomain) error {
	known := map[string]bool{}
	for _, d := range domains {
		if !validIntegrationID(d.ID) {
			return fmt.Errorf("%w: %s", errDAGUnknownDomainBinding, d.ID)
		}
		known[d.ID] = true
	}
	for _, n := range nodes {
		if n.DomainID == "" {
			continue
		}
		if !known[n.DomainID] {
			return fmt.Errorf("%w: %s", errDAGUnknownDomainBinding, n.DomainID)
		}
	}
	return nil
}

func validateDependencyNode(n DependencyNode) error {
	if !validIntegrationID(n.ID) || !validIntegrationID(n.Lineage) {
		return fmt.Errorf("%w: id=%q lineage=%q", errDAGMalformedID, n.ID, n.Lineage)
	}
	if n.DomainID != "" && !validIntegrationID(n.DomainID) {
		return fmt.Errorf("%w: domain %q", errDAGMalformedID, n.DomainID)
	}
	for _, dep := range n.DependsOn {
		if !validIntegrationID(dep) {
			return fmt.Errorf("%w: dep %q", errDAGMalformedID, dep)
		}
	}
	return nil
}

func diagnoseCycles(order []string, edges map[string][]string) [][]string {
	index := make(map[string]int, len(order))
	low := make(map[string]int, len(order))
	on := make(map[string]bool, len(order))
	var stack []string
	next := 0
	var sccs [][]string

	var strong func(v string)
	strong = func(v string) {
		next++
		index[v] = next
		low[v] = next
		stack = append(stack, v)
		on[v] = true
		for _, w := range edges[v] {
			if index[w] == 0 {
				strong(w)
				if low[w] < low[v] {
					low[v] = low[w]
				}
			} else if on[w] && index[w] < low[v] {
				low[v] = index[w]
			}
		}
		if low[v] == index[v] {
			var scc []string
			for {
				n := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				on[n] = false
				scc = append(scc, n)
				if n == v {
					break
				}
			}
			sccs = append(sccs, scc)
		}
	}

	ids := append([]string{}, order...)
	sort.Strings(ids)
	for _, id := range ids {
		if index[id] == 0 {
			strong(id)
		}
	}

	selfLoop := map[string]bool{}
	for _, id := range ids {
		for _, dep := range edges[id] {
			if dep == id {
				selfLoop[id] = true
			}
		}
	}

	var cycles [][]string
	for _, scc := range sccs {
		members := append([]string{}, scc...)
		sort.Strings(members)
		if len(members) > 1 {
			cycles = append(cycles, rotateCycle(members, edges))
			continue
		}
		if len(members) == 1 && selfLoop[members[0]] {
			cycles = append(cycles, []string{members[0]})
		}
	}
	sort.Slice(cycles, func(i, j int) bool {
		return strings.Join(cycles[i], "\x00") < strings.Join(cycles[j], "\x00")
	})
	return cycles
}

func rotateCycle(members []string, edges map[string][]string) []string {
	if len(members) == 0 {
		return members
	}
	in := map[string]bool{}
	for _, id := range members {
		in[id] = true
	}
	start := members[0]
	for _, id := range members[1:] {
		if id < start {
			start = id
		}
	}
	out := make([]string, 0, len(members))
	seen := map[string]bool{}
	cur := start
	for i := 0; i < len(members); i++ {
		out = append(out, cur)
		seen[cur] = true
		next := ""
		for _, dep := range edges[cur] {
			if !in[dep] || seen[dep] {
				continue
			}
			if next == "" || dep < next {
				next = dep
			}
		}
		if next == "" {
			break
		}
		cur = next
	}
	if len(out) != len(members) {
		return members
	}
	return out
}
