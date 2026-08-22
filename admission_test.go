package main

import (
	"bytes"
	"os"
	"sync"
	"testing"
	"time"
)

func TestAdmissionMissingDefaultsOpen(t *testing.T) {
	root := t.TempDir()
	st, err := loadAdmissionState(root)
	if err != nil {
		t.Fatalf("load missing admission: %v", err)
	}
	if st.Version != 1 {
		t.Fatalf("version = %d, want 1", st.Version)
	}
	if st.Paused {
		t.Fatal("missing admission should default to open")
	}
	if st.Epoch != 0 {
		t.Fatalf("epoch = %d, want 0", st.Epoch)
	}
	if !admissionAllowsScheduling(root) {
		t.Fatal("scheduling should be allowed when admission file is missing")
	}
}

func TestAdmissionPausePersistsAndEpochMonotonic(t *testing.T) {
	root := t.TempDir()

	paused, err := setAdmissionPaused(root, true, "ops", "drain")
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if paused.Epoch != 1 {
		t.Fatalf("pause epoch = %d, want 1", paused.Epoch)
	}
	if !paused.Paused {
		t.Fatal("pause did not set paused")
	}
	if paused.Actor != "ops" || paused.Reason != "drain" {
		t.Fatalf("actor/reason = %q/%q, want ops/drain", paused.Actor, paused.Reason)
	}
	if _, err := time.Parse(time.RFC3339Nano, paused.UpdatedAt); err != nil {
		t.Fatalf("pause timestamp: %v", err)
	}

	again, err := setAdmissionPaused(root, true, "ops", "drain")
	if err != nil {
		t.Fatalf("repeat pause: %v", err)
	}
	if again.Epoch != 1 {
		t.Fatalf("repeat pause epoch = %d, want 1", again.Epoch)
	}

	resumed, err := setAdmissionPaused(root, false, "lead", "resume")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.Epoch != 2 {
		t.Fatalf("resume epoch = %d, want 2", resumed.Epoch)
	}
	if resumed.Paused {
		t.Fatal("resume left admission paused")
	}
	if resumed.Actor != "lead" || resumed.Reason != "resume" {
		t.Fatalf("actor/reason = %q/%q, want lead/resume", resumed.Actor, resumed.Reason)
	}
	if _, err := time.Parse(time.RFC3339Nano, resumed.UpdatedAt); err != nil {
		t.Fatalf("resume timestamp: %v", err)
	}

	loaded, err := loadAdmissionState(root)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if loaded.Epoch != 2 || loaded.Paused || loaded.Actor != "lead" || loaded.Reason != "resume" {
		t.Fatalf("persisted state = %+v", loaded)
	}
	if !admissionAllowsScheduling(root) {
		t.Fatal("scheduling should be allowed after resume")
	}
}

func TestAdmissionVersion1RequiresExplicitFields(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "version only", body: `{"version":1}`},
		{name: "missing epoch", body: `{"version":1,"paused":false,"updated_at":"2026-01-01T00:00:00.000000000Z"}`},
		{name: "missing paused", body: `{"version":1,"epoch":0,"updated_at":"2026-01-01T00:00:00.000000000Z"}`},
		{name: "paused null", body: `{"version":1,"epoch":0,"paused":null,"updated_at":"2026-01-01T00:00:00.000000000Z"}`},
		{name: "missing updated_at", body: `{"version":1,"epoch":0,"paused":false}`},
		{name: "invalid updated_at", body: `{"version":1,"epoch":0,"paused":false,"updated_at":"not-rfc3339"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(controlDir(root), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(admissionPath(root), []byte(tc.body+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := loadAdmissionState(root); err == nil {
				t.Fatal("load should fail for incomplete version-1 admission")
			}
			if admissionAllowsScheduling(root) {
				t.Fatal("incomplete version-1 admission should fail closed")
			}
		})
	}
}

func TestAdmissionPauseDeniesScheduling(t *testing.T) {
	root := t.TempDir()
	if _, err := setAdmissionPaused(root, true, "ops", "drain"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if admissionAllowsScheduling(root) {
		t.Fatal("scheduling should be denied after durable pause")
	}
}

func TestAdmissionIdempotentPreservesTransitionMetadata(t *testing.T) {
	root := t.TempDir()
	first, err := setAdmissionPaused(root, true, "ops", "drain")
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	before, err := os.ReadFile(admissionPath(root))
	if err != nil {
		t.Fatalf("read after first pause: %v", err)
	}

	second, err := setAdmissionPaused(root, true, "other", "again")
	if err != nil {
		t.Fatalf("idempotent pause: %v", err)
	}
	if second != first {
		t.Fatalf("idempotent pause returned %+v, want %+v", second, first)
	}
	after, err := os.ReadFile(admissionPath(root))
	if err != nil {
		t.Fatalf("read after second pause: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("idempotent pause rewrote admission file:\nbefore=%q\nafter=%q", before, after)
	}
}

func TestAdmissionMalformedFailsClosed(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(controlDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(admissionPath(root), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAdmissionState(root); err == nil {
		t.Fatal("malformed json should fail load")
	}
	if admissionAllowsScheduling(root) {
		t.Fatal("malformed json should fail closed")
	}

	if err := os.WriteFile(admissionPath(root), []byte(`{"version":2,"epoch":1,"paused":false,"updated_at":"2026-01-01T00:00:00Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAdmissionState(root); err == nil {
		t.Fatal("unsupported version should fail load")
	}
	if admissionAllowsScheduling(root) {
		t.Fatal("unsupported version should fail closed")
	}
}

func TestAdmissionConcurrentUpdatesSerialized(t *testing.T) {
	root := t.TempDir()
	const n = 64
	start := make(chan struct{})
	var wg sync.WaitGroup
	epochs := make([]uint64, n)
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			st, err := setAdmissionPaused(root, i%2 == 0, "race", "concurrent")
			epochs[i] = st.Epoch
			errs[i] = err
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}

	final, err := loadAdmissionState(root)
	if err != nil {
		t.Fatalf("final load: %v", err)
	}
	seen := make(map[uint64]struct{})
	var max uint64
	for _, epoch := range epochs {
		if epoch == 0 {
			continue
		}
		seen[epoch] = struct{}{}
		if epoch > max {
			max = epoch
		}
	}
	if max != final.Epoch {
		t.Fatalf("max returned epoch %d != final epoch %d", max, final.Epoch)
	}
	for want := uint64(1); want <= final.Epoch; want++ {
		if _, ok := seen[want]; !ok {
			t.Fatalf("missing epoch %d in returned values (final=%d)", want, final.Epoch)
		}
	}
}
