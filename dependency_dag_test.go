package main

import (
	"errors"
	"reflect"
	"testing"
)

func TestAnalyzeDependencyDAGReadinessIsDeterministic(t *testing.T) {
	nodes := []DependencyNode{
		{ID: "cutover", Lineage: "cutover-lineage", DependsOn: []string{"migrate", "auth-tokens"}},
		{ID: "auth-tokens", Lineage: "auth-tokens-lineage"},
		{ID: "migrate", Lineage: "migrate-lineage", DependsOn: []string{"schema"}, Satisfied: true},
		{ID: "schema", Lineage: "schema-lineage", Satisfied: true},
		{ID: "docs", Lineage: "docs-lineage"},
	}
	got, err := AnalyzeDependencyDAG([]DependencyNode{nodes[0], nodes[2], nodes[4], nodes[1], nodes[3]})
	if err != nil {
		t.Fatalf("healthy dag: %v", err)
	}
	wantReady := []string{"auth-tokens", "docs"}
	if !reflect.DeepEqual(got.Ready, wantReady) {
		t.Fatalf("ready=%v want %v", got.Ready, wantReady)
	}
	if len(got.Cycles) != 0 || len(got.Missing) != 0 {
		t.Fatalf("healthy diagnosis must be empty: %+v", got)
	}

	shuffled, err := AnalyzeDependencyDAG([]DependencyNode{nodes[4], nodes[1], nodes[0], nodes[3], nodes[2]})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(shuffled.Ready, wantReady) {
		t.Fatalf("readiness must ignore input order: %v", shuffled.Ready)
	}
}

func TestAnalyzeDependencyDAGDiagnosesCyclesAndMissingDependencies(t *testing.T) {
	nodes := []DependencyNode{
		{ID: "alpha", Lineage: "alpha-lineage", DependsOn: []string{"beta"}},
		{ID: "beta", Lineage: "beta-lineage", DependsOn: []string{"gamma"}},
		{ID: "gamma", Lineage: "gamma-lineage", DependsOn: []string{"alpha"}},
		{ID: "leaf", Lineage: "leaf-lineage", DependsOn: []string{"ghost", "alpha"}},
		{ID: "ready", Lineage: "ready-lineage"},
	}
	got, err := AnalyzeDependencyDAG(nodes)
	if err == nil {
		t.Fatal("cycle and missing deps must fail closed")
	}
	if !errors.Is(err, errDAGCycle) || !errors.Is(err, errDAGMissingDependency) {
		t.Fatalf("want cycle+missing sentinels, got %v", err)
	}
	var audit *DAGAuditError
	if !errors.As(err, &audit) {
		t.Fatalf("diagnosis must be inspectable: %v", err)
	}
	if len(got.Ready) != 0 || len(audit.Diagnosis.Ready) != 0 {
		t.Fatalf("audit error must withhold ready authorization: %+v", got)
	}
	if len(got.Cycles) != 1 || !reflect.DeepEqual(got.Cycles[0], []string{"alpha", "beta", "gamma"}) {
		t.Fatalf("cycle must rotate onto the least id: %v", got.Cycles)
	}
	if len(got.Missing) != 1 || got.Missing[0].NodeID != "leaf" || got.Missing[0].MissingID != "ghost" {
		t.Fatalf("missing dependency: %+v", got.Missing)
	}

	self := []DependencyNode{{ID: "loop", Lineage: "loop-lineage", DependsOn: []string{"loop"}}}
	selfGot, selfErr := AnalyzeDependencyDAG(self)
	if !errors.Is(selfErr, errDAGCycle) || len(selfGot.Cycles) != 1 || !reflect.DeepEqual(selfGot.Cycles[0], []string{"loop"}) {
		t.Fatalf("self-cycle: %+v / %v", selfGot, selfErr)
	}
	if len(selfGot.Ready) != 0 {
		t.Fatalf("self-cycle must withhold ready: %+v", selfGot)
	}
}

func TestAnalyzeDependencyDAGWithholdsReadyWhenDisjointNodeCoexistsWithAuditFailure(t *testing.T) {
	disjoint := DependencyNode{ID: "ready", Lineage: "ready-lineage"}

	t.Run("cycle", func(t *testing.T) {
		got, err := AnalyzeDependencyDAG([]DependencyNode{
			{ID: "alpha", Lineage: "alpha-lineage", DependsOn: []string{"beta"}},
			{ID: "beta", Lineage: "beta-lineage", DependsOn: []string{"alpha"}},
			disjoint,
		})
		assertEmptyReadyOnAudit(t, got, err, errDAGCycle)
		if len(got.Cycles) != 1 || !reflect.DeepEqual(got.Cycles[0], []string{"alpha", "beta"}) {
			t.Fatalf("cycle diagnosis must remain: %v", got.Cycles)
		}
		if len(got.Missing) != 0 {
			t.Fatalf("missing must stay empty: %+v", got.Missing)
		}
	})

	t.Run("missing", func(t *testing.T) {
		got, err := AnalyzeDependencyDAG([]DependencyNode{
			{ID: "leaf", Lineage: "leaf-lineage", DependsOn: []string{"ghost"}},
			disjoint,
		})
		assertEmptyReadyOnAudit(t, got, err, errDAGMissingDependency)
		if len(got.Missing) != 1 || got.Missing[0].NodeID != "leaf" || got.Missing[0].MissingID != "ghost" {
			t.Fatalf("missing diagnosis must remain: %+v", got.Missing)
		}
		if len(got.Cycles) != 0 {
			t.Fatalf("cycles must stay empty: %v", got.Cycles)
		}
	})
}

func assertEmptyReadyOnAudit(t *testing.T, got DAGDiagnosis, err error, want error) {
	t.Helper()
	if err == nil {
		t.Fatal("dag audit must fail closed")
	}
	if !errors.Is(err, want) {
		t.Fatalf("want %v, got %v", want, err)
	}
	var audit *DAGAuditError
	if !errors.As(err, &audit) {
		t.Fatalf("diagnosis must be inspectable: %v", err)
	}
	if len(got.Ready) != 0 || len(audit.Diagnosis.Ready) != 0 {
		t.Fatalf("non-nil dag audit error must yield empty ready: %+v", got)
	}
}

func TestAnalyzeDependencyDAGRejectsMalformedAndDuplicateIdentifiers(t *testing.T) {
	valid := DependencyNode{ID: "auth-tokens", Lineage: "auth-tokens-lineage"}
	cases := []struct {
		name  string
		nodes []DependencyNode
		want  error
	}{
		{name: "empty id", nodes: []DependencyNode{{ID: "", Lineage: "auth-tokens-lineage"}}, want: errDAGMalformedID},
		{name: "uppercase id", nodes: []DependencyNode{{ID: "Auth", Lineage: "auth-tokens-lineage"}}, want: errDAGMalformedID},
		{name: "missing lineage", nodes: []DependencyNode{{ID: "auth-tokens"}}, want: errDAGMalformedID},
		{name: "bad lineage", nodes: []DependencyNode{{ID: "auth-tokens", Lineage: "auth_tokens"}}, want: errDAGMalformedID},
		{name: "bad domain ref", nodes: []DependencyNode{{ID: "auth-tokens", Lineage: "auth-tokens-lineage", DomainID: "Auth"}}, want: errDAGMalformedID},
		{name: "empty dep", nodes: []DependencyNode{{ID: "auth-tokens", Lineage: "auth-tokens-lineage", DependsOn: []string{""}}}, want: errDAGMalformedID},
		{name: "duplicate node", nodes: []DependencyNode{valid, valid}, want: errDAGDuplicateNode},
		{name: "duplicate edges", nodes: []DependencyNode{{
			ID: "cutover", Lineage: "cutover-lineage", DependsOn: []string{"schema", "schema"},
		}}, want: errDAGDuplicateNode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AnalyzeDependencyDAG(tc.nodes)
			if err == nil || !errors.Is(err, tc.want) {
				t.Fatalf("got %v; want %v", err, tc.want)
			}
			if len(got.Ready) != 0 || len(got.Cycles) != 0 || len(got.Missing) != 0 {
				t.Fatalf("malformed input must not emit diagnosis: %+v", got)
			}
		})
	}
}

func TestBindDependencyDomainsFailClosedOnUnknownDomain(t *testing.T) {
	nodes := []DependencyNode{
		{ID: "auth-tokens", Lineage: "auth-tokens-lineage", DomainID: "auth-tokens"},
		{ID: "docs", Lineage: "docs-lineage"},
	}
	domains := []WriteDomain{{ID: "auth-tokens", Lineage: "auth-tokens-lineage", Component: "auth", Paths: []string{"internal/auth"}}}
	if err := BindDependencyDomains(nodes, domains); err != nil {
		t.Fatalf("bound domain: %v", err)
	}
	if err := BindDependencyDomains(nodes, nil); !errors.Is(err, errDAGUnknownDomainBinding) {
		t.Fatalf("unknown domain must fail closed: %v", err)
	}
	unknown := []DependencyNode{{ID: "cutover", Lineage: "cutover-lineage", DomainID: "ghost-domain"}}
	if err := BindDependencyDomains(unknown, domains); !errors.Is(err, errDAGUnknownDomainBinding) {
		t.Fatalf("ghost domain must fail closed: %v", err)
	}
}
