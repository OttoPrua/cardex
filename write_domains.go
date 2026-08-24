package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

const (
	resourceRuntime    = "runtime"
	resourceDatabase   = "database"
	resourceProfile    = "profile"
	resourceManifest   = "manifest"
	resourceDevice     = "device"
	resourceCredential = "credential"
	resourceCutover    = "cutover"

	overlapKindExact    = "path-exact"
	overlapKindSubtree  = "path-subtree"
	overlapKindResource = "resource"
)

var (
	errWriteDomainTraversal        = errors.New("write domain path traversal")
	errWriteDomainEmptyClaim       = errors.New("write domain empty claim")
	errWriteDomainAmbiguousClaim   = errors.New("write domain ambiguous claim")
	errWriteDomainCanonicalization = errors.New("write domain path canonicalization")
	errWriteDomainMalformedID      = errors.New("write domain malformed identifier")
	errWriteDomainDuplicateLineage = errors.New("write domain duplicate lineage ownership")
	errWriteDomainPathOverlap      = errors.New("write domain path overlap")
	errWriteDomainResourceOverlap  = errors.New("write domain resource overlap")
	errWriteDomainUnknownResource  = errors.New("write domain unknown resource kind")
	errWriteDomainDuplicateClaim   = errors.New("write domain duplicate claim")

	integrationIDRe = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
	resourceIDRe    = regexp.MustCompile(`^[a-z][a-z0-9]*([.-][a-z0-9]+)*$`)
)

var closedResourceKinds = map[string]bool{
	resourceRuntime:    true,
	resourceDatabase:   true,
	resourceProfile:    true,
	resourceManifest:   true,
	resourceDevice:     true,
	resourceCredential: true,
	resourceCutover:    true,
}

// WriteDomain is a closed, lineage-owned set of repository-root-bound path
// claims and optional exclusive runtime resources. It is an auditable core
// value; task/runner/dispatch integration is a residual seam.
type WriteDomain struct {
	ID        string          `json:"id"`
	Lineage   string          `json:"lineage"`
	Component string          `json:"component"`
	Paths     []string        `json:"paths"`
	Resources []ResourceClaim `json:"resources,omitempty"`
}

// ResourceClaim is a closed exclusive resource. Kind must be one of the
// resource* constants; ID is a lowercase identifier.
type ResourceClaim struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// WriteDomainConflict is one deterministic overlap between two domains.
type WriteDomainConflict struct {
	Kind     string
	DomainA  string
	DomainB  string
	PathA    string
	PathB    string
	Resource ResourceClaim
}

// WriteDomainOverlapError lists every path or resource overlap. Callers may
// errors.Is the path/resource sentinels or errors.As this type.
type WriteDomainOverlapError struct {
	Conflicts []WriteDomainConflict
}

func (e *WriteDomainOverlapError) Error() string {
	if e == nil || len(e.Conflicts) == 0 {
		return "write domain overlap"
	}
	paths, resources := 0, 0
	for _, c := range e.Conflicts {
		if c.Kind == overlapKindResource {
			resources++
			continue
		}
		paths++
	}
	return fmt.Sprintf("write domain overlap: %d path, %d resource", paths, resources)
}

func (e *WriteDomainOverlapError) Is(target error) bool {
	if e == nil {
		return false
	}
	hasPath, hasRes := false, false
	for _, c := range e.Conflicts {
		if c.Kind == overlapKindResource {
			hasRes = true
			continue
		}
		if c.Kind == overlapKindExact || c.Kind == overlapKindSubtree {
			hasPath = true
		}
	}
	return (target == errWriteDomainPathOverlap && hasPath) ||
		(target == errWriteDomainResourceOverlap && hasRes)
}

func validIntegrationID(id string) bool {
	return integrationIDRe.MatchString(id)
}

func validResourceID(id string) bool {
	return resourceIDRe.MatchString(id)
}

// NormalizePathClaim returns a slash-separated, repository-relative path bound
// under the canonical real repoRoot. Existing prefixes are resolved through
// symlinks; a missing suffix is reattached so aliases collide. Traversal,
// empty, ambiguous, escaping, dangling, cyclic, or unprovable claims fail closed.
func NormalizePathClaim(repoRoot, raw string) (string, error) {
	root, err := canonicalizeRepoRoot(repoRoot)
	if err != nil {
		return "", err
	}
	rel, err := lexicalPathClaim(raw)
	if err != nil {
		return "", err
	}

	full := filepath.Clean(filepath.Join(root, filepath.FromSlash(rel)))
	if !pathInsideRoot(root, full) {
		return "", errWriteDomainTraversal
	}
	if full == root {
		return "", errWriteDomainAmbiguousClaim
	}

	resolved, err := resolveThroughExistingPrefix(root, full)
	if err != nil {
		return "", err
	}
	rootAgain, err := canonicalizeRepoRoot(repoRoot)
	if err != nil || rootAgain != root {
		return "", errWriteDomainCanonicalization
	}
	return confinedRepoRel(root, resolved)
}

func canonicalizeRepoRoot(repoRoot string) (string, error) {
	if strings.TrimSpace(repoRoot) == "" {
		return "", errWriteDomainEmptyClaim
	}
	if strings.TrimSpace(repoRoot) != repoRoot || !filepath.IsAbs(repoRoot) {
		return "", errWriteDomainAmbiguousClaim
	}
	root := filepath.Clean(repoRoot)
	if !filepath.IsAbs(root) {
		return "", errWriteDomainAmbiguousClaim
	}
	resolved, err := evalStable(root)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(resolved) {
		return "", errWriteDomainAmbiguousClaim
	}
	return resolved, nil
}

func lexicalPathClaim(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errWriteDomainEmptyClaim
	}
	if strings.TrimSpace(raw) != raw {
		return "", errWriteDomainAmbiguousClaim
	}
	if filepath.IsAbs(raw) {
		return "", errWriteDomainAmbiguousClaim
	}
	for _, r := range raw {
		if r == '/' {
			continue
		}
		if r == 0 || unicode.IsControl(r) || !unicode.IsPrint(r) {
			return "", errWriteDomainAmbiguousClaim
		}
	}
	if strings.HasPrefix(raw, "~") || strings.Contains(raw, "$") || strings.ContainsAny(raw, "*?[]") {
		return "", errWriteDomainAmbiguousClaim
	}
	if strings.Contains(raw, `\`) {
		slash := strings.ReplaceAll(raw, `\`, "/")
		if pathHasDotDot(slash) {
			return "", errWriteDomainTraversal
		}
		return "", errWriteDomainAmbiguousClaim
	}
	if raw == "." || raw == "./" || strings.HasPrefix(raw, "./") || strings.Contains(raw, "/./") || strings.Contains(raw, "//") {
		return "", errWriteDomainAmbiguousClaim
	}

	trimmed := strings.TrimSuffix(raw, "/")
	if trimmed == "" || trimmed == "." {
		return "", errWriteDomainAmbiguousClaim
	}
	if pathHasDotDot(trimmed) {
		return "", errWriteDomainTraversal
	}
	for _, seg := range strings.Split(trimmed, "/") {
		if seg == "" || seg == "." {
			return "", errWriteDomainAmbiguousClaim
		}
	}
	return trimmed, nil
}

func resolveThroughExistingPrefix(root, full string) (string, error) {
	existing, missing, err := deepestExistingPrefix(root, full)
	if err != nil {
		return "", err
	}
	resolved, err := evalStable(existing)
	if err != nil {
		return "", err
	}
	existing2, missing2, err := deepestExistingPrefix(root, full)
	if err != nil {
		return "", err
	}
	resolved2, err := evalStable(existing2)
	if err != nil {
		return "", err
	}
	if existing2 != existing || resolved2 != resolved || !sameStringSlice(missing, missing2) {
		return "", errWriteDomainCanonicalization
	}
	if !pathInsideRoot(root, resolved) {
		return "", errWriteDomainTraversal
	}
	if len(missing) == 0 {
		return resolved, nil
	}
	out := filepath.Clean(filepath.Join(append([]string{resolved}, missing...)...))
	if !pathInsideRoot(root, out) {
		return "", errWriteDomainTraversal
	}
	return out, nil
}

func deepestExistingPrefix(root, full string) (string, []string, error) {
	existing := filepath.Clean(full)
	var missing []string
	for {
		_, err := os.Lstat(existing)
		if err == nil {
			return existing, missing, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", nil, errWriteDomainCanonicalization
		}
		if existing == root {
			return "", nil, errWriteDomainCanonicalization
		}
		parent := filepath.Dir(existing)
		if parent == existing || !pathInsideRoot(root, parent) {
			return "", nil, errWriteDomainCanonicalization
		}
		missing = append([]string{filepath.Base(existing)}, missing...)
		existing = parent
	}
}

func evalStable(path string) (string, error) {
	first, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", errWriteDomainCanonicalization
	}
	first = filepath.Clean(first)
	if !filepath.IsAbs(first) {
		return "", errWriteDomainCanonicalization
	}
	second, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", errWriteDomainCanonicalization
	}
	if filepath.Clean(second) != first {
		return "", errWriteDomainCanonicalization
	}
	return first, nil
}

func confinedRepoRel(root, path string) (string, error) {
	path = filepath.Clean(path)
	if !pathInsideRoot(root, path) {
		return "", errWriteDomainTraversal
	}
	if path == root {
		return "", errWriteDomainAmbiguousClaim
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", errWriteDomainCanonicalization
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || strings.Contains(rel, "/../") {
		return "", errWriteDomainTraversal
	}
	if rel == "" || strings.HasPrefix(rel, "/") || strings.Contains(rel, `\`) {
		return "", errWriteDomainCanonicalization
	}
	return rel, nil
}

func pathInsideRoot(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func pathHasDotDot(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// NormalizeWriteDomain validates identifiers, closed resources, and path
// claims, returning a copy with canonical paths.
func NormalizeWriteDomain(repoRoot string, domain WriteDomain) (WriteDomain, error) {
	if !validIntegrationID(domain.ID) || !validIntegrationID(domain.Lineage) || !validIntegrationID(domain.Component) {
		return WriteDomain{}, fmt.Errorf("%w: id=%q lineage=%q component=%q", errWriteDomainMalformedID, domain.ID, domain.Lineage, domain.Component)
	}
	if len(domain.Paths) == 0 {
		return WriteDomain{}, errWriteDomainEmptyClaim
	}

	out := WriteDomain{
		ID:        domain.ID,
		Lineage:   domain.Lineage,
		Component: domain.Component,
		Paths:     make([]string, 0, len(domain.Paths)),
		Resources: make([]ResourceClaim, 0, len(domain.Resources)),
	}
	seenPath := map[string]bool{}
	for _, raw := range domain.Paths {
		if strings.TrimSpace(raw) == "" {
			return WriteDomain{}, errWriteDomainEmptyClaim
		}
		path, err := NormalizePathClaim(repoRoot, raw)
		if err != nil {
			return WriteDomain{}, err
		}
		if seenPath[path] {
			return WriteDomain{}, fmt.Errorf("%w: path %s", errWriteDomainDuplicateClaim, path)
		}
		seenPath[path] = true
		out.Paths = append(out.Paths, path)
	}

	seenRes := map[string]bool{}
	for _, res := range domain.Resources {
		if !closedResourceKinds[res.Kind] {
			return WriteDomain{}, fmt.Errorf("%w: %q", errWriteDomainUnknownResource, res.Kind)
		}
		if strings.TrimSpace(res.ID) == "" {
			return WriteDomain{}, errWriteDomainEmptyClaim
		}
		if !validResourceID(res.ID) {
			return WriteDomain{}, fmt.Errorf("%w: resource %q", errWriteDomainMalformedID, res.ID)
		}
		key := res.Kind + "\x00" + res.ID
		if seenRes[key] {
			return WriteDomain{}, fmt.Errorf("%w: resource %s %s", errWriteDomainDuplicateClaim, res.Kind, res.ID)
		}
		seenRes[key] = true
		out.Resources = append(out.Resources, ResourceClaim{Kind: res.Kind, ID: res.ID})
	}
	return out, nil
}

// AuditWriteDomains normalizes every domain, rejects duplicate identifiers or
// lineage ownership, and fail-closes on exact/subtree path overlap or shared
// closed resources. Genuinely disjoint paths may share a component.
func AuditWriteDomains(repoRoot string, domains []WriteDomain) ([]WriteDomain, error) {
	normalized := make([]WriteDomain, 0, len(domains))
	seenID := map[string]bool{}
	seenLineage := map[string]bool{}
	for _, domain := range domains {
		got, err := NormalizeWriteDomain(repoRoot, domain)
		if err != nil {
			return nil, err
		}
		if seenID[got.ID] {
			return nil, fmt.Errorf("%w: duplicate domain %s", errWriteDomainMalformedID, got.ID)
		}
		if seenLineage[got.Lineage] {
			return nil, fmt.Errorf("%w: %s", errWriteDomainDuplicateLineage, got.Lineage)
		}
		seenID[got.ID] = true
		seenLineage[got.Lineage] = true
		normalized = append(normalized, got)
	}

	conflicts := detectWriteDomainOverlaps(normalized)
	if len(conflicts) > 0 {
		return nil, &WriteDomainOverlapError{Conflicts: conflicts}
	}
	return normalized, nil
}

func detectWriteDomainOverlaps(domains []WriteDomain) []WriteDomainConflict {
	var conflicts []WriteDomainConflict
	for i := 0; i < len(domains); i++ {
		for j := i + 1; j < len(domains); j++ {
			a, b := domains[i], domains[j]
			if a.ID > b.ID {
				a, b = b, a
			}
			for _, pa := range a.Paths {
				for _, pb := range b.Paths {
					kind, ok := pathOverlapKind(pa, pb)
					if !ok {
						continue
					}
					conflicts = append(conflicts, WriteDomainConflict{
						Kind:    kind,
						DomainA: a.ID,
						DomainB: b.ID,
						PathA:   pa,
						PathB:   pb,
					})
				}
			}
			for _, ra := range a.Resources {
				for _, rb := range b.Resources {
					if ra.Kind != rb.Kind || ra.ID != rb.ID {
						continue
					}
					conflicts = append(conflicts, WriteDomainConflict{
						Kind:     overlapKindResource,
						DomainA:  a.ID,
						DomainB:  b.ID,
						Resource: ra,
					})
				}
			}
		}
	}
	sort.Slice(conflicts, func(i, j int) bool {
		ci, cj := conflicts[i], conflicts[j]
		if ci.Kind != cj.Kind {
			return ci.Kind < cj.Kind
		}
		if ci.DomainA != cj.DomainA {
			return ci.DomainA < cj.DomainA
		}
		if ci.DomainB != cj.DomainB {
			return ci.DomainB < cj.DomainB
		}
		if ci.PathA != cj.PathA {
			return ci.PathA < cj.PathA
		}
		if ci.PathB != cj.PathB {
			return ci.PathB < cj.PathB
		}
		if ci.Resource.Kind != cj.Resource.Kind {
			return ci.Resource.Kind < cj.Resource.Kind
		}
		return ci.Resource.ID < cj.Resource.ID
	})
	return conflicts
}

func pathOverlapKind(a, b string) (string, bool) {
	if a == b {
		return overlapKindExact, true
	}
	if strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/") {
		return overlapKindSubtree, true
	}
	return "", false
}
