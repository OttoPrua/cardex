package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestDeniedPresemanticStartDoesNotConsumeResumeTombstone is the P0 crash/order
// counterexample: MidStep resume writes pending, then provider Start is denied by
// admission pause or epoch bump. invoke returns without a semantic start.
//
// RED: injectAtMostOnce upgrades pending→final before the reserved attempt is
// durably abandoned. A crash in that window leaves Status=running + final;
// running-side reentry skips as tombstone-exhausted and holds the card.
// GREEN: the denied Start rolls back the pending write first; the order hook
// therefore observes an unconsumed tombstone before abandon. Exact attempt
// closure stays truthful, retry remains eligible, and a later real start still
// finalizes (at-most-once).
func TestDeniedPresemanticStartDoesNotConsumeResumeTombstone(t *testing.T) {
	for _, tc := range []struct {
		name               string
		pauseResume        bool
		crashBeforeAbandon bool
	}{
		{name: "paused"},
		{name: "epoch-stale-after-resume", pauseResume: true},
		{name: "paused-crash-before-abandon", crashBeforeAbandon: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := testRoot(t)
			counter := filepath.Join(t.TempDir(), "calls")
			cfg := runTaskCfg(t, countingClaudeBin(t, counter, mkOKResultJSON("sess-resume-tomb")))
			ws := t.TempDir()
			tk := newTask(root, cfg, typeSequence, "midstep resume admission tombstone", ws, []string{"step-one", "step-two"}, 5)
			tk.MidStep = true
			tk.SessionID = "sess-resume-tomb"
			if tc.crashBeforeAbandon {
				tk.Status = statusRunning
			} else {
				tk.Status = statusLimitPaused
			}
			if err := saveTask(root, tk); err != nil {
				t.Fatal(err)
			}

			entered := make(chan struct{})
			release := make(chan struct{})
			admissionPreStartHook = func() {
				select {
				case <-entered:
				default:
					close(entered)
				}
				<-release
			}
			sawFinalBeforeAbandon := false
			resumeTombstoneAfterInjectHook = func() error {
				j, corrupted, err := readTombstoneJournal(root, tk.ID)
				if err != nil || corrupted {
					t.Errorf("order hook read tombstone: corrupted=%v err=%v", corrupted, err)
					return err
				}
				if e, ok := j.Entries[resumeKind(0)]; ok {
					if e.Phase == tombstonePhaseFinal {
						sawFinalBeforeAbandon = true
						t.Errorf("RED: pending upgraded to final before reserved attempt abandon: %+v", e)
					} else {
						t.Errorf("denied Start left a consumed pending tombstone before abandon: %+v", e)
					}
				}
				if tc.crashBeforeAbandon {
					return errTransitionCrash
				}
				return nil
			}
			t.Cleanup(func() {
				admissionPreStartHook = nil
				resumeTombstoneAfterInjectHook = nil
				select {
				case <-release:
				default:
					close(release)
				}
			})

			done := make(chan error, 1)
			go func() {
				fresh, err := loadTask(root, tk.ID)
				if err != nil {
					done <- err
					return
				}
				done <- runTask(context.Background(), root, cfg, fresh, false)
			}()

			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("did not reach pre-start hook")
			}
			running, err := loadTask(root, tk.ID)
			if err != nil {
				t.Fatal(err)
			}
			attemptID := running.ActiveAttemptID
			if attemptID == "" {
				t.Fatal("expected a reserved attempt before Start")
			}
			denyAdmission(t, root, tc.pauseResume)
			close(release)

			var runErr error
			select {
			case runErr = <-done:
			case <-time.After(8 * time.Second):
				t.Fatal("runner did not return after denied Start")
			}
			admissionPreStartHook = nil
			resumeTombstoneAfterInjectHook = nil
			if sawFinalBeforeAbandon {
				t.Fatal("denied presemantic Start finalized the resume tombstone before attempt abandon")
			}
			if n := providerCallCount(t, counter); n != 0 {
				t.Fatalf("provider stub call count=%d, want 0 (no semantic start)", n)
			}
			assertResumeTombstoneUnconsumed(t, root, tk.ID, 0)

			if tc.crashBeforeAbandon {
				if !errors.Is(runErr, errTransitionCrash) {
					t.Fatalf("crash-before-abandon err=%v, want %v", runErr, errTransitionCrash)
				}
				fresh, err := loadTask(root, tk.ID)
				if err != nil {
					t.Fatal(err)
				}
				if fresh.Status != statusRunning {
					t.Fatalf("crash before abandon must leave status=running, got %s", fresh.Status)
				}
				if fresh.ActiveAttemptID != attemptID {
					t.Fatalf("crash before abandon must leave reserved attempt %s, got %q", attemptID, fresh.ActiveAttemptID)
				}
				rec, err := loadAttempt(root, tk.ID, attemptID)
				if err != nil || rec == nil {
					t.Fatalf("reserved attempt missing after crash: %v", err)
				}
				if rec.State != attemptReserved && rec.State != attemptBound {
					t.Fatalf("crash before abandon must not pretend the attempt closed, state=%s", rec.State)
				}
				calls := 0
				skipped, _, injErr := injectAtMostOnce(root, tk.ID, resumeKind(0), func() error {
					calls++
					return nil
				})
				if injErr != nil || skipped || calls != 1 {
					t.Fatalf("crash window must leave resume retry-eligible: skipped=%v err=%v calls=%d", skipped, injErr, calls)
				}
				assertNoSemanticCompletion(t, root, tk.ID)
				return
			}

			if runErr != nil {
				t.Fatalf("runner: %v", runErr)
			}
			assertAttemptClosed(t, root, tk.ID, attemptID)
			got, err := loadTask(root, tk.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != statusQueued {
				t.Fatalf("status=%s, want queued (retry eligible)", got.Status)
			}
			events := readAllEventsRaw(t, root, tk.ID)
			for _, ev := range events {
				if ev.Type == evHeld {
					t.Fatalf("unexpected held after denied Start: %v", eventTypes(events))
				}
			}
			assertNoSemanticCompletion(t, root, tk.ID)

			if !tc.pauseResume {
				if _, err := setAdmissionPaused(root, false, "ops", "resume"); err != nil {
					t.Fatalf("re-open admission: %v", err)
				}
			}
			if err := runTask(context.Background(), root, cfg, got, false); err != nil {
				t.Fatalf("retry after denied Start: %v", err)
			}
			if n := providerCallCount(t, counter); n == 0 {
				t.Fatal("retry after denied Start did not invoke provider")
			}
			j, _, err := readTombstoneJournal(root, tk.ID)
			if err != nil {
				t.Fatal(err)
			}
			e := j.Entries[resumeKind(0)]
			if e.Phase != tombstonePhaseFinal {
				t.Fatalf("real semantic start must finalize resume tombstone, got %+v", e)
			}
			calls := 0
			skipped, _, injErr := injectAtMostOnce(root, tk.ID, resumeKind(0), func() error {
				calls++
				return nil
			})
			if injErr != nil || !skipped || calls != 0 {
				t.Fatalf("at-most-once after real start broken: skipped=%v err=%v calls=%d", skipped, injErr, calls)
			}
		})
	}
}

func assertResumeTombstoneUnconsumed(t *testing.T, root, id string, step int) {
	t.Helper()
	j, corrupted, err := readTombstoneJournal(root, id)
	if err != nil || corrupted {
		t.Fatalf("read tombstone: corrupted=%v err=%v", corrupted, err)
	}
	kind := resumeKind(step)
	if e, ok := j.Entries[kind]; ok {
		t.Fatalf("denied presemantic Start must not consume %s: %+v", kind, e)
	}
}
