package main

import (
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestMultiLaneMetricsCountThroughputConflictAndWait(t *testing.T) {
	m := NewMultiLaneMetrics()
	m.AddThroughput(4)
	m.AddConflicts(2)
	m.AddWaits(7)
	m.AddThroughput(1)
	snap := m.Snapshot()
	if snap.Throughput != 5 || snap.Conflicts != 2 || snap.Waits != 7 {
		t.Fatalf("snapshot=%+v", snap)
	}
	later := m.Snapshot()
	if later != snap {
		t.Fatalf("snapshot must be a value copy: %+v vs %+v", later, snap)
	}
	m.AddWaits(1)
	if m.Snapshot().Waits != 8 || snap.Waits != 7 {
		t.Fatalf("mutating counters must not change prior snapshots")
	}
}

func TestMultiLaneSnapshotIsValueFreeAndTriggersNoModelWork(t *testing.T) {
	m := NewMultiLaneMetrics()
	m.AddThroughput(3)
	m.AddConflicts(1)
	m.AddWaits(2)
	snap := m.Snapshot()

	rt := reflect.TypeOf(snap)
	if rt.Kind() != reflect.Struct || rt.NumField() != 3 {
		t.Fatalf("snapshot must expose exactly three count fields, got %s", rt)
	}
	seen := map[string]bool{}
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		seen[f.Name] = true
		switch f.Type.Kind() {
		case reflect.Uint64:
		default:
			t.Fatalf("field %s has value-bearing type %s", f.Name, f.Type)
		}
		lower := strings.ToLower(f.Name)
		for _, needle := range []string{"path", "prompt", "model", "resource", "lineage", "payload", "domain"} {
			if strings.Contains(lower, needle) {
				t.Fatalf("snapshot field %s is not value-free", f.Name)
			}
		}
	}
	if !seen["Throughput"] || !seen["Conflicts"] || !seen["Waits"] {
		t.Fatalf("snapshot fields=%v", seen)
	}
	if MultiLaneTriggersModelWork(snap) {
		t.Fatal("value-free lane metrics must not trigger model work")
	}
}

func TestMultiLaneMetricsConcurrentAddsAreRaceFree(t *testing.T) {
	m := NewMultiLaneMetrics()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.AddThroughput(1)
			m.AddConflicts(1)
			m.AddWaits(1)
			_ = m.Snapshot()
		}()
	}
	wg.Wait()
	snap := m.Snapshot()
	if snap.Throughput != 32 || snap.Conflicts != 32 || snap.Waits != 32 {
		t.Fatalf("concurrent snapshot=%+v", snap)
	}
	if MultiLaneTriggersModelWork(snap) {
		t.Fatal("concurrent metrics must still trigger no model work")
	}
}
