package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
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

func TestAdmissionClosedV1SchemaRejectsDuplicatesUnknownAndNonObject(t *testing.T) {
	const ts = "2026-01-01T00:00:00.000000000Z"
	cases := []struct {
		name string
		body string
	}{
		{
			name: "duplicate paused true then false",
			body: `{"version":1,"epoch":0,"paused":true,"paused":false,"updated_at":"` + ts + `"}`,
		},
		{
			name: "duplicate paused case alias",
			body: `{"version":1,"epoch":0,"paused":true,"Paused":false,"updated_at":"` + ts + `"}`,
		},
		{
			name: "duplicate epoch",
			body: `{"version":1,"epoch":0,"epoch":1,"paused":false,"updated_at":"` + ts + `"}`,
		},
		{
			name: "unknown top-level key",
			body: `{"version":1,"epoch":0,"paused":false,"updated_at":"` + ts + `","extra":true}`,
		},
		{
			name: "non-object",
			body: `[{"version":1,"epoch":0,"paused":false,"updated_at":"` + ts + `"}]`,
		},
		{
			name: "trailing json value",
			body: `{"version":1,"epoch":0,"paused":false,"updated_at":"` + ts + `"} false`,
		},
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
				t.Fatal("closed version-1 admission should fail load")
			}
			if admissionAllowsScheduling(root) {
				t.Fatal("closed version-1 admission should fail closed")
			}
		})
	}
}

func TestAdmissionRejectsNoncanonicalFieldAliases(t *testing.T) {
	const ts = "2026-01-01T00:00:00.000000000Z"
	cases := []struct {
		name string
		body string
		key  string
	}{
		{
			name: "version unicode simple-fold alias",
			body: `{"ver\u017fion":1,"epoch":0,"paused":false,"updated_at":"` + ts + `"}`,
			key:  "ver\u017fion",
		},
		{
			name: "epoch ascii alias",
			body: `{"version":1,"Epoch":0,"paused":false,"updated_at":"` + ts + `"}`,
			key:  "Epoch",
		},
		{
			name: "paused ascii alias",
			body: `{"version":1,"epoch":0,"Paused":false,"updated_at":"` + ts + `"}`,
			key:  "Paused",
		},
		{
			name: "actor ascii alias",
			body: `{"version":1,"epoch":0,"paused":false,"Actor":"ops","updated_at":"` + ts + `"}`,
			key:  "Actor",
		},
		{
			name: "reason ascii alias",
			body: `{"version":1,"epoch":0,"paused":false,"Reason":"drain","updated_at":"` + ts + `"}`,
			key:  "Reason",
		},
		{
			name: "updated_at ascii alias",
			body: `{"version":1,"epoch":0,"paused":false,"UPDATED_AT":"` + ts + `"}`,
			key:  "UPDATED_AT",
		},
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
			_, err := loadAdmissionState(root)
			if err == nil {
				t.Fatal("noncanonical field alias should fail load")
			}
			want := fmt.Sprintf("admission state: unknown field %q", tc.key)
			if err.Error() != want {
				t.Fatalf("error = %q, want %q", err.Error(), want)
			}
			if strings.Contains(err.Error(), "missing") {
				t.Fatalf("alias must fail as unknown field, not missing required key: %v", err)
			}
			if admissionAllowsScheduling(root) {
				t.Fatal("noncanonical field alias should fail closed")
			}
		})
	}
}

func TestAdmissionRejectsNullActorAndReason(t *testing.T) {
	const ts = "2026-01-01T00:00:00.000000000Z"
	cases := []struct {
		name string
		body string
	}{
		{
			name: "actor null",
			body: `{"version":1,"epoch":0,"paused":false,"actor":null,"updated_at":"` + ts + `"}`,
		},
		{
			name: "reason null",
			body: `{"version":1,"epoch":0,"paused":false,"reason":null,"updated_at":"` + ts + `"}`,
		},
		{
			name: "actor and reason null",
			body: `{"version":1,"epoch":0,"paused":false,"actor":null,"reason":null,"updated_at":"` + ts + `"}`,
		},
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
				t.Fatal("null actor/reason should fail load")
			}
			if admissionAllowsScheduling(root) {
				t.Fatal("null actor/reason should fail closed")
			}
		})
	}
}

func TestAdmissionEmptyActorAndReasonStringsLoad(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(controlDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"version":1,"epoch":0,"paused":false,"actor":"","reason":"","updated_at":"2026-01-01T00:00:00.000000000Z"}` + "\n"
	if err := os.WriteFile(admissionPath(root), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := loadAdmissionState(root)
	if err != nil {
		t.Fatalf("empty actor/reason strings should load: %v", err)
	}
	if st.Actor != "" || st.Reason != "" {
		t.Fatalf("empty actor/reason = %q/%q", st.Actor, st.Reason)
	}
	if !admissionAllowsScheduling(root) {
		t.Fatal("open admission with empty actor/reason strings should allow scheduling")
	}
}

func TestAdmissionCanonicalV1StillLoads(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(controlDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"version":1,"epoch":4,"paused":false,"actor":"ops","reason":"drain","updated_at":"2026-01-01T00:00:00.000000000Z"}` + "\n"
	if err := os.WriteFile(admissionPath(root), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := loadAdmissionState(root)
	if err != nil {
		t.Fatalf("canonical version-1 admission should load: %v", err)
	}
	if st.Version != 1 || st.Epoch != 4 || st.Paused || st.Actor != "ops" || st.Reason != "drain" {
		t.Fatalf("canonical state = %+v", st)
	}
	if _, err := time.Parse(time.RFC3339Nano, st.UpdatedAt); err != nil {
		t.Fatalf("canonical updated_at: %v", err)
	}
	if !admissionAllowsScheduling(root) {
		t.Fatal("canonical open admission should allow scheduling")
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
