package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func taskIsReadOnlyType(t *Task) bool {
	return t != nil && (t.Type == typeReview || t.Type == typeProgressPull)
}

type liveWriterClaim struct {
	taskID    string
	dir       string
	dirKey    string
	repoKey   string
	explicit  bool
	valid     bool
	domainID  string
	lineage   string
	paths     []string
	reals     []string
	resources []ResourceClaim
}

func writerDirKey(dir string) string {
	return filepath.Clean(strings.TrimSpace(dir))
}

func integrationRepoKey(dir string) string {
	abs, err := filepath.Abs(strings.TrimSpace(dir))
	if err != nil || abs == "" {
		return writerDirKey(dir)
	}
	abs = filepath.Clean(abs)
	if key, ok := gitCommonDirKey(abs); ok {
		return key
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil && real != "" {
		return filepath.Clean(real)
	}
	return abs
}

func gitCommonDirKey(dir string) (string, bool) {
	gitMeta := filepath.Join(dir, ".git")
	info, err := os.Lstat(gitMeta)
	if err != nil {
		return "", false
	}
	gitDir := gitMeta
	if !info.IsDir() {
		data, err := os.ReadFile(gitMeta)
		if err != nil {
			return "", false
		}
		line := strings.TrimSpace(string(data))
		const prefix = "gitdir:"
		if len(line) < len(prefix) || !strings.EqualFold(line[:len(prefix)], prefix) {
			return "", false
		}
		p := strings.TrimSpace(line[len(prefix):])
		if p == "" {
			return "", false
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		gitDir = filepath.Clean(p)
	}
	common := gitDir
	if filepath.Base(filepath.Dir(gitDir)) == "worktrees" {
		common = filepath.Dir(filepath.Dir(gitDir))
	}
	if real, err := filepath.EvalSymlinks(common); err == nil && real != "" {
		return filepath.Clean(real), true
	}
	return common, true
}

func taskRepoRoot(t *Task) string {
	if t == nil {
		return ""
	}
	abs, err := filepath.Abs(strings.TrimSpace(t.Dir))
	if err != nil {
		return writerDirKey(t.Dir)
	}
	return filepath.Clean(abs)
}

func writerClaimForTask(t *Task) liveWriterClaim {
	c := liveWriterClaim{valid: true}
	if t == nil {
		return liveWriterClaim{}
	}
	c.taskID = t.ID
	c.dir = t.Dir
	c.dirKey = writerDirKey(t.Dir)
	c.repoKey = integrationRepoKey(t.Dir)
	if t.WriteDomain == nil {
		return c
	}
	c.explicit = true
	root := taskRepoRoot(t)
	norm, err := NormalizeWriteDomain(root, *t.WriteDomain)
	if err != nil {
		c.valid = false
		c.domainID = t.WriteDomain.ID
		c.lineage = t.WriteDomain.Lineage
		return c
	}
	c.domainID = norm.ID
	c.lineage = norm.Lineage
	c.paths = append([]string{}, norm.Paths...)
	c.resources = append([]ResourceClaim{}, norm.Resources...)
	for _, rel := range c.paths {
		full := filepath.Clean(filepath.Join(root, filepath.FromSlash(rel)))
		if real, err := filepath.EvalSymlinks(full); err == nil && real != "" {
			c.reals = append(c.reals, filepath.Clean(real))
		}
	}
	sort.Strings(c.reals)
	return c
}

func writerClaimsConflict(a, b liveWriterClaim) bool {
	if a.taskID == "" || b.taskID == "" || a.taskID == b.taskID {
		return false
	}
	if a.explicit && b.explicit {
		if !a.valid || !b.valid {
			return a.dirKey == b.dirKey || a.repoKey == b.repoKey
		}
		if a.domainID != "" && a.domainID == b.domainID {
			return true
		}
		if a.lineage != "" && a.lineage == b.lineage {
			return true
		}
		for _, ra := range a.resources {
			for _, rb := range b.resources {
				if ra.Kind == rb.Kind && ra.ID == rb.ID {
					return true
				}
			}
		}
		sameRepo := a.repoKey != "" && a.repoKey == b.repoKey
		for _, pa := range a.paths {
			for _, pb := range b.paths {
				if _, ok := pathOverlapKind(pa, pb); ok && sameRepo {
					return true
				}
			}
		}
		for _, ra := range a.reals {
			for _, rb := range b.reals {
				if ra == rb || strings.HasPrefix(ra, rb+string(filepath.Separator)) || strings.HasPrefix(rb, ra+string(filepath.Separator)) {
					return true
				}
			}
		}
		return false
	}
	if !a.explicit && !b.explicit {
		return a.dir == b.dir
	}
	return a.dirKey == b.dirKey
}

func writerConflictsWithActive(candidate *Task, active []*Task) bool {
	if candidate == nil || taskIsReadOnlyType(candidate) {
		return false
	}
	claim := writerClaimForTask(candidate)
	if claim.explicit && !claim.valid {
		return true
	}
	for _, other := range active {
		if other == nil || other.ID == candidate.ID || taskIsReadOnlyType(other) {
			continue
		}
		if writerClaimsConflict(claim, writerClaimForTask(other)) {
			return true
		}
	}
	return false
}

func applyTaskWriteDomain(t *Task, id, lineage, component, pathsCSV, resourcesCSV string) error {
	if t == nil {
		return fmt.Errorf("empty task")
	}
	id = strings.TrimSpace(id)
	lineage = strings.TrimSpace(lineage)
	component = strings.TrimSpace(component)
	pathsCSV = strings.TrimSpace(pathsCSV)
	resourcesCSV = strings.TrimSpace(resourcesCSV)
	if id == "" && lineage == "" && component == "" && pathsCSV == "" && resourcesCSV == "" {
		return nil
	}
	if id == "" || lineage == "" || component == "" || pathsCSV == "" {
		return fmt.Errorf("write domain requires id, lineage, component, and paths")
	}
	domain := WriteDomain{
		ID:        id,
		Lineage:   lineage,
		Component: component,
		Paths:     splitComma(pathsCSV),
	}
	if resourcesCSV != "" {
		for _, raw := range splitComma(resourcesCSV) {
			kind, rid, ok := strings.Cut(raw, ":")
			kind = strings.TrimSpace(kind)
			rid = strings.TrimSpace(rid)
			if !ok || kind == "" || rid == "" {
				return fmt.Errorf("%w: %q", errWriteDomainUnknownResource, raw)
			}
			domain.Resources = append(domain.Resources, ResourceClaim{Kind: kind, ID: rid})
		}
	}
	norm, err := NormalizeWriteDomain(taskRepoRoot(t), domain)
	if err != nil {
		return err
	}
	t.WriteDomain = &norm
	return nil
}

func applyTaskDependsOn(t *Task, csv string) error {
	if t == nil {
		return fmt.Errorf("empty task")
	}
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return nil
	}
	ids := splitComma(csv)
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			return fmt.Errorf("%w: dep %q", errDAGMalformedID, id)
		}
		if !validIntegrationID(id) {
			return fmt.Errorf("%w: dep %q", errDAGMalformedID, id)
		}
		if id == t.ID {
			return fmt.Errorf("%w: %s", errDAGCycle, id)
		}
		if seen[id] {
			return fmt.Errorf("%w: %s -> %s", errDAGDuplicateNode, t.ID, id)
		}
		seen[id] = true
		out = append(out, id)
	}
	t.DependsOn = out
	return nil
}

func taskDurablyDone(root string, t *Task) bool {
	if t == nil || t.Status != statusDone || t.LastCommittedTransitionID == "" {
		return false
	}
	rec, err := loadTransition(root, t.ID, t.LastCommittedTransitionID)
	if err != nil || rec == nil || rec.State != transitionCommitted {
		return false
	}
	if rec.EventType != "" && rec.EventType != evDone {
		return false
	}
	return rec.Status == statusDone || rec.EventType == evDone
}

func loadDAGUniverse(root string, live []*Task) []*Task {
	out := make([]*Task, 0, len(live))
	seen := map[string]bool{}
	for _, t := range live {
		if t == nil || t.ID == "" || seen[t.ID] {
			continue
		}
		seen[t.ID] = true
		out = append(out, t)
	}
	for _, t := range live {
		if t == nil {
			continue
		}
		for _, dep := range t.DependsOn {
			if seen[dep] {
				continue
			}
			found, err := findTaskAnywhere(root, dep)
			if err != nil || found == nil {
				continue
			}
			seen[dep] = true
			out = append(out, found)
		}
	}
	return out
}

func liveDependencyNode(root string, t *Task) (DependencyNode, []WriteDomain, error) {
	n := DependencyNode{
		ID:        t.ID,
		Lineage:   t.ID,
		DependsOn: append([]string{}, t.DependsOn...),
		Satisfied: taskDurablyDone(root, t),
	}
	if t.WriteDomain == nil {
		return n, nil, nil
	}
	n.Lineage = t.WriteDomain.Lineage
	n.DomainID = t.WriteDomain.ID
	norm, err := NormalizeWriteDomain(taskRepoRoot(t), *t.WriteDomain)
	if err != nil {
		return n, nil, err
	}
	if n.DomainID != norm.ID {
		return n, nil, fmt.Errorf("%w: %s", errDAGUnknownDomainBinding, n.DomainID)
	}
	n.DomainID = norm.ID
	n.Lineage = norm.Lineage
	return n, []WriteDomain{norm}, nil
}

func liveDAGReadyIDs(root string, live []*Task) map[string]bool {
	ready := map[string]bool{}
	universe := loadDAGUniverse(root, live)
	byID := map[string]*Task{}
	for _, t := range universe {
		byID[t.ID] = t
	}
	participants := map[string]bool{}
	for _, t := range universe {
		if len(t.DependsOn) == 0 {
			continue
		}
		participants[t.ID] = true
		for _, dep := range t.DependsOn {
			participants[dep] = true
		}
	}
	for _, t := range live {
		if t == nil {
			continue
		}
		if !participants[t.ID] {
			ready[t.ID] = true
		}
	}
	parent := map[string]string{}
	var find func(string) string
	find = func(id string) string {
		if parent[id] == "" {
			parent[id] = id
		}
		if parent[id] != id {
			parent[id] = find(parent[id])
		}
		return parent[id]
	}
	union := func(a, b string) {
		pa, pb := find(a), find(b)
		if pa != pb {
			if pa < pb {
				parent[pb] = pa
			} else {
				parent[pa] = pb
			}
		}
	}
	for id := range participants {
		_ = find(id)
	}
	for _, t := range universe {
		if !participants[t.ID] {
			continue
		}
		for _, dep := range t.DependsOn {
			union(t.ID, dep)
		}
	}
	groups := map[string][]string{}
	for id := range participants {
		groups[find(id)] = append(groups[find(id)], id)
	}
	for _, members := range groups {
		nodes := make([]DependencyNode, 0, len(members))
		var domains []WriteDomain
		blocked := false
		for _, id := range members {
			t := byID[id]
			if t == nil {
				continue
			}
			n, ds, err := liveDependencyNode(root, t)
			if err != nil {
				blocked = true
				break
			}
			nodes = append(nodes, n)
			domains = append(domains, ds...)
		}
		if blocked || len(nodes) == 0 {
			continue
		}
		if err := BindDependencyDomains(nodes, domains); err != nil {
			continue
		}
		diag, err := AnalyzeDependencyDAG(nodes)
		if err != nil {
			continue
		}
		for _, id := range diag.Ready {
			ready[id] = true
		}
	}
	return ready
}

func dagAllowsTask(root string, live []*Task, id string) bool {
	if id == "" {
		return false
	}
	return liveDAGReadyIDs(root, live)[id]
}

func secretFreeWriteDomain(d *WriteDomain) *WriteDomain {
	if d == nil {
		return nil
	}
	out := *d
	out.Paths = append([]string{}, d.Paths...)
	out.Resources = append([]ResourceClaim{}, d.Resources...)
	return &out
}
