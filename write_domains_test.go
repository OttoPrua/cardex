package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizePathClaimRejectsTraversalEmptyAndAmbiguous(t *testing.T) {
	root := t.TempDir()
	insideAbs := filepath.Join(root, "internal", "auth")

	cases := []struct {
		name string
		raw  string
		want error
	}{
		{name: "parent traversal", raw: "../secret", want: errWriteDomainTraversal},
		{name: "nested traversal", raw: "internal/auth/../../etc/passwd", want: errWriteDomainTraversal},
		{name: "backslash traversal", raw: `internal\..\secret`, want: errWriteDomainTraversal},
		{name: "absolute escape", raw: "/etc/passwd", want: errWriteDomainAmbiguousClaim},
		{name: "absolute under root", raw: insideAbs, want: errWriteDomainAmbiguousClaim},
		{name: "empty", raw: "", want: errWriteDomainEmptyClaim},
		{name: "whitespace", raw: "  ", want: errWriteDomainEmptyClaim},
		{name: "tab", raw: "\tinternal/auth", want: errWriteDomainAmbiguousClaim},
		{name: "repo root dot", raw: ".", want: errWriteDomainAmbiguousClaim},
		{name: "dot slash", raw: "./internal/auth", want: errWriteDomainAmbiguousClaim},
		{name: "double slash", raw: "internal//auth", want: errWriteDomainAmbiguousClaim},
		{name: "dot segment", raw: "internal/./auth", want: errWriteDomainAmbiguousClaim},
		{name: "glob", raw: "internal/auth/*", want: errWriteDomainAmbiguousClaim},
		{name: "home", raw: "~/auth", want: errWriteDomainAmbiguousClaim},
		{name: "env", raw: "$HOME/auth", want: errWriteDomainAmbiguousClaim},
		{name: "nul", raw: "internal/auth\x00.go", want: errWriteDomainAmbiguousClaim},
		{name: "newline", raw: "internal/auth\n", want: errWriteDomainAmbiguousClaim},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizePathClaim(root, tc.raw)
			if err == nil || !errors.Is(err, tc.want) {
				t.Fatalf("NormalizePathClaim(%q)=%q, %v; want %v", tc.raw, got, err, tc.want)
			}
			if got != "" {
				t.Fatalf("rejected claim must not return a path, got %q", got)
			}
		})
	}
}

func TestNormalizePathClaimBindsToRepositoryRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "internal", "auth"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := NormalizePathClaim(root, "internal/auth/token.go")
	if err != nil {
		t.Fatalf("valid relative claim: %v", err)
	}
	if got != "internal/auth/token.go" {
		t.Fatalf("canonical path=%q", got)
	}

	trimmed, err := NormalizePathClaim(root, "internal/auth/")
	if err != nil {
		t.Fatalf("trailing slash should normalize: %v", err)
	}
	if trimmed != "internal/auth" {
		t.Fatalf("trailing slash canonical=%q", trimmed)
	}

	if _, err := NormalizePathClaim("relative-root", "internal/auth"); !errors.Is(err, errWriteDomainAmbiguousClaim) {
		t.Fatalf("relative repo root must be rejected: %v", err)
	}
	if _, err := NormalizePathClaim("", "internal/auth"); !errors.Is(err, errWriteDomainEmptyClaim) {
		t.Fatalf("empty repo root must be rejected: %v", err)
	}
}

func TestWriteDomainRequiresClosedClaimsAndIntegrationLineage(t *testing.T) {
	root := t.TempDir()
	valid := WriteDomain{
		ID:        "auth-tokens",
		Lineage:   "auth-tokens-lineage",
		Component: "auth",
		Paths:     []string{"internal/auth/token.go"},
		Resources: []ResourceClaim{{Kind: resourceDatabase, ID: "auth.primary"}},
	}
	got, err := NormalizeWriteDomain(root, valid)
	if err != nil {
		t.Fatalf("valid domain: %v", err)
	}
	if got.Paths[0] != "internal/auth/token.go" || got.Resources[0].Kind != resourceDatabase {
		t.Fatalf("normalized domain: %+v", got)
	}

	cases := []struct {
		name   string
		mutate func(*WriteDomain)
		want   error
	}{
		{name: "empty lineage", mutate: func(d *WriteDomain) { d.Lineage = "" }, want: errWriteDomainMalformedID},
		{name: "uppercase lineage", mutate: func(d *WriteDomain) { d.Lineage = "AuthTokens" }, want: errWriteDomainMalformedID},
		{name: "underscore lineage", mutate: func(d *WriteDomain) { d.Lineage = "auth_tokens" }, want: errWriteDomainMalformedID},
		{name: "slash lineage", mutate: func(d *WriteDomain) { d.Lineage = "auth/tokens" }, want: errWriteDomainMalformedID},
		{name: "empty id", mutate: func(d *WriteDomain) { d.ID = "" }, want: errWriteDomainMalformedID},
		{name: "uppercase id", mutate: func(d *WriteDomain) { d.ID = "Auth-tokens" }, want: errWriteDomainMalformedID},
		{name: "empty component", mutate: func(d *WriteDomain) { d.Component = "" }, want: errWriteDomainMalformedID},
		{name: "empty paths", mutate: func(d *WriteDomain) { d.Paths = nil }, want: errWriteDomainEmptyClaim},
		{name: "blank path", mutate: func(d *WriteDomain) { d.Paths = []string{" "} }, want: errWriteDomainEmptyClaim},
		{name: "unknown resource", mutate: func(d *WriteDomain) { d.Resources = []ResourceClaim{{Kind: "network", ID: "eth0"}} }, want: errWriteDomainUnknownResource},
		{name: "empty resource id", mutate: func(d *WriteDomain) { d.Resources = []ResourceClaim{{Kind: resourceRuntime, ID: ""}} }, want: errWriteDomainEmptyClaim},
		{name: "malformed resource id", mutate: func(d *WriteDomain) { d.Resources = []ResourceClaim{{Kind: resourceDevice, ID: "GPU-0"}} }, want: errWriteDomainMalformedID},
		{name: "duplicate path", mutate: func(d *WriteDomain) {
			d.Paths = []string{"internal/auth/token.go", "internal/auth/token.go"}
		}, want: errWriteDomainDuplicateClaim},
		{name: "duplicate resource", mutate: func(d *WriteDomain) {
			d.Resources = []ResourceClaim{
				{Kind: resourceProfile, ID: "grok-build"},
				{Kind: resourceProfile, ID: "grok-build"},
			}
		}, want: errWriteDomainDuplicateClaim},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := valid
			d.Paths = append([]string{}, valid.Paths...)
			d.Resources = append([]ResourceClaim{}, valid.Resources...)
			tc.mutate(&d)
			if _, err := NormalizeWriteDomain(root, d); err == nil || !errors.Is(err, tc.want) {
				t.Fatalf("got %v; want %v", err, tc.want)
			}
		})
	}
}

func TestAuditWriteDomainsBlocksOverlapButAllowsDisjointSameComponent(t *testing.T) {
	root := t.TempDir()
	authTokens := WriteDomain{
		ID: "auth-tokens", Lineage: "auth-tokens-lineage", Component: "auth",
		Paths: []string{"internal/auth/token.go"},
	}
	authSessions := WriteDomain{
		ID: "auth-sessions", Lineage: "auth-sessions-lineage", Component: "auth",
		Paths: []string{"internal/auth/session.go"},
	}
	got, err := AuditWriteDomains(root, []WriteDomain{authSessions, authTokens})
	if err != nil {
		t.Fatalf("disjoint paths in the same component must be allowed: %v", err)
	}
	if len(got) != 2 || got[0].ID != "auth-sessions" || got[1].ID != "auth-tokens" {
		t.Fatalf("audit must keep deterministic domain order: %+v", got)
	}

	exact := WriteDomain{
		ID: "auth-dup", Lineage: "auth-dup-lineage", Component: "auth",
		Paths: []string{"internal/auth/token.go"},
	}
	if _, err := AuditWriteDomains(root, []WriteDomain{authTokens, exact}); !errors.Is(err, errWriteDomainPathOverlap) {
		t.Fatalf("exact path overlap: %v", err)
	}

	subtree := WriteDomain{
		ID: "auth-tree", Lineage: "auth-tree-lineage", Component: "auth",
		Paths: []string{"internal/auth"},
	}
	if _, err := AuditWriteDomains(root, []WriteDomain{authTokens, subtree}); !errors.Is(err, errWriteDomainPathOverlap) {
		t.Fatalf("subtree overlap: %v", err)
	}

	sibling := WriteDomain{
		ID: "auth-extra", Lineage: "auth-extra-lineage", Component: "auth",
		Paths: []string{"internal/auth-extra/keys.go"},
	}
	if _, err := AuditWriteDomains(root, []WriteDomain{subtree, sibling}); err != nil {
		t.Fatalf("prefix-sibling paths must not count as subtree overlap: %v", err)
	}
}

func TestAuditWriteDomainsBlocksSharedClosedResourcesAndDuplicateLineage(t *testing.T) {
	root := t.TempDir()
	kinds := []string{
		resourceRuntime, resourceDatabase, resourceProfile, resourceManifest,
		resourceDevice, resourceCredential, resourceCutover,
	}
	for _, kind := range kinds {
		a := WriteDomain{
			ID: "lane-a", Lineage: "lane-a-lineage", Component: "billing",
			Paths:     []string{"internal/billing/a.go"},
			Resources: []ResourceClaim{{Kind: kind, ID: "shared.item"}},
		}
		b := WriteDomain{
			ID: "lane-b", Lineage: "lane-b-lineage", Component: "billing",
			Paths:     []string{"internal/billing/b.go"},
			Resources: []ResourceClaim{{Kind: kind, ID: "shared.item"}},
		}
		if _, err := AuditWriteDomains(root, []WriteDomain{a, b}); !errors.Is(err, errWriteDomainResourceOverlap) {
			t.Fatalf("shared %s must block: %v", kind, err)
		}
	}

	left := WriteDomain{
		ID: "left", Lineage: "shared-lineage", Component: "ops",
		Paths: []string{"internal/ops/left.go"},
	}
	right := WriteDomain{
		ID: "right", Lineage: "shared-lineage", Component: "ops",
		Paths: []string{"internal/ops/right.go"},
	}
	if _, err := AuditWriteDomains(root, []WriteDomain{left, right}); !errors.Is(err, errWriteDomainDuplicateLineage) {
		t.Fatalf("duplicate lineage ownership: %v", err)
	}

	dupID := WriteDomain{
		ID: "left", Lineage: "other-lineage", Component: "ops",
		Paths: []string{"internal/ops/other.go"},
	}
	if _, err := AuditWriteDomains(root, []WriteDomain{left, dupID}); !errors.Is(err, errWriteDomainMalformedID) {
		t.Fatalf("duplicate domain id: %v", err)
	}

	var overlap *WriteDomainOverlapError
	conflictA := WriteDomain{
		ID: "db-a", Lineage: "db-a-lineage", Component: "billing",
		Paths:     []string{"internal/billing/ledger.go"},
		Resources: []ResourceClaim{{Kind: resourceDatabase, ID: "orders.primary"}},
	}
	conflictB := WriteDomain{
		ID: "db-b", Lineage: "db-b-lineage", Component: "billing",
		Paths:     []string{"internal/billing/invoice.go"},
		Resources: []ResourceClaim{{Kind: resourceDatabase, ID: "orders.primary"}},
	}
	_, err := AuditWriteDomains(root, []WriteDomain{conflictB, conflictA})
	if !errors.As(err, &overlap) || len(overlap.Conflicts) != 1 {
		t.Fatalf("inspectable overlap: %+v / %v", overlap, err)
	}
	c := overlap.Conflicts[0]
	if c.Kind != overlapKindResource || c.DomainA != "db-a" || c.DomainB != "db-b" || c.Resource.ID != "orders.primary" {
		t.Fatalf("deterministic conflict payload: %+v", c)
	}
	if strings.Contains(err.Error(), "SELECT") || strings.Contains(err.Error(), "password") {
		t.Fatalf("overlap error must stay value-free: %v", err)
	}
}
