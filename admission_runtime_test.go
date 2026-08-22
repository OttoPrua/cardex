package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type dispatchSnapshot struct {
	task     []byte
	events   []byte
	attempts map[string][]byte
}

func snapshotDispatch(t *testing.T, root, id string) dispatchSnapshot {
	t.Helper()
	task, err := os.ReadFile(taskPath(root, id))
	if err != nil {
		t.Fatalf("read task: %v", err)
	}
	events, _ := os.ReadFile(eventsPath(root, id))
	attempts := map[string][]byte{}
	entries, err := os.ReadDir(attemptsDir(root, id))
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			data, rerr := os.ReadFile(filepath.Join(attemptsDir(root, id), e.Name()))
			if rerr != nil {
				t.Fatalf("read attempt %s: %v", e.Name(), rerr)
			}
			attempts[e.Name()] = data
		}
	}
	return dispatchSnapshot{task: task, events: events, attempts: attempts}
}

func assertDispatchUnchanged(t *testing.T, root, id string, before dispatchSnapshot) {
	t.Helper()
	after := snapshotDispatch(t, root, id)
	if !bytes.Equal(before.task, after.task) {
		t.Fatalf("task JSON mutated\nbefore=%s\nafter=%s", before.task, after.task)
	}
	if !bytes.Equal(before.events, after.events) {
		t.Fatalf("event log mutated\nbefore=%q\nafter=%q", before.events, after.events)
	}
	if len(before.attempts) != len(after.attempts) {
		t.Fatalf("attempt directory mutated: before=%d files after=%d files afterNames=%v",
			len(before.attempts), len(after.attempts), attemptNames(after.attempts))
	}
	for name, data := range before.attempts {
		got, ok := after.attempts[name]
		if !ok {
			t.Fatalf("attempt file %s removed", name)
		}
		if !bytes.Equal(data, got) {
			t.Fatalf("attempt file %s mutated\nbefore=%s\nafter=%s", name, data, got)
		}
	}
}

func attemptNames(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	return out
}

func extraWorkIDs(t *testing.T, root, parentID string) []string {
	t.Helper()
	var ids []string
	for _, dir := range []string{tasksDir(root), archiveDir(root)} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			id := strings.TrimSuffix(e.Name(), ".json")
			if id != parentID {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func countingClaudeBin(t *testing.T, counterPath, stdoutJSON string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	script := "#!/bin/sh\n"
	script += "printf x >> " + shSingleQuote(counterPath) + "\n"
	if stdoutJSON != "" {
		script += "cat <<'JSON_EOF'\n" + stdoutJSON + "\nJSON_EOF\n"
	}
	script += "exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func providerCallCount(t *testing.T, counterPath string) int {
	t.Helper()
	data, err := os.ReadFile(counterPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return len(data)
}

func writeMalformedAdmission(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(controlDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(admissionPath(root), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func queuedSequence(t *testing.T, root string, cfg *Config, title, ws string) *Task {
	t.Helper()
	tk := newTask(root, cfg, typeSequence, title, ws, []string{"do the work"}, 5)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}
	return tk
}

func assertNoSemanticCompletion(t *testing.T, root, id string) {
	t.Helper()
	events := readAllEventsRaw(t, root, id)
	for _, ev := range events {
		switch ev.Type {
		case evStepOK, evDone:
			t.Fatalf("model/semantic completion event %s in %v", ev.Type, eventTypes(events))
		}
	}
	got, err := loadTask(root, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastRouteAttempt != nil && (got.LastRouteAttempt.ObservationSeen ||
		got.LastRouteAttempt.SemanticEvents > 0 || got.LastRouteAttempt.ModelEvents > 0) {
		t.Fatalf("route attempt recorded model/semantic completion: %+v", got.LastRouteAttempt)
	}
}

func assertAttemptClosed(t *testing.T, root, taskID, attemptID string) {
	t.Helper()
	if attemptID == "" {
		t.Fatal("missing attempt id")
	}
	fresh, err := loadTask(root, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ActiveAttemptID == attemptID {
		t.Fatalf("active attempt %s was not cleared", attemptID)
	}
	rec, err := loadAttempt(root, taskID, attemptID)
	if err != nil || rec == nil {
		t.Fatalf("closed attempt missing: %v", err)
	}
	if rec.State != attemptExited && rec.State != attemptRevoked {
		t.Fatalf("attempt state=%s, want exited or revoked", rec.State)
	}
}

func TestPausedReservationZeroMutation(t *testing.T) {
	root := testRoot(t)
	withSchedulerLock(t, root)
	cfg := testCfg()
	tk := queuedSequence(t, root, cfg, "paused reserve", "/tmp")
	if _, err := setAdmissionPaused(root, true, "ops", "drain"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	before := snapshotDispatch(t, root, tk.ID)
	err := reserveDispatchAttempt(root, tk)
	if !errors.Is(err, errAdmissionDenied) {
		t.Fatalf("paused reserve err=%v, want %v", err, errAdmissionDenied)
	}
	assertDispatchUnchanged(t, root, tk.ID, before)
}

func TestMalformedAdmissionReservationZeroMutation(t *testing.T) {
	root := testRoot(t)
	withSchedulerLock(t, root)
	cfg := testCfg()
	tk := queuedSequence(t, root, cfg, "malformed reserve", "/tmp")
	writeMalformedAdmission(t, root)
	before := snapshotDispatch(t, root, tk.ID)
	err := reserveDispatchAttempt(root, tk)
	if !errors.Is(err, errAdmissionDenied) {
		t.Fatalf("malformed reserve err=%v, want %v", err, errAdmissionDenied)
	}
	assertDispatchUnchanged(t, root, tk.ID, before)
}

func waitReservedAttempt(t *testing.T, root, id string, timeout time.Duration) *Task {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		got, err := loadTask(root, id)
		if err == nil && got.ActiveAttemptID != "" {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

func TestPauseAfterReservationBeforeLaunchNoProvider(t *testing.T) {
	root := testRoot(t)
	counter := filepath.Join(t.TempDir(), "calls")
	cfg := runTaskCfg(t, countingClaudeBin(t, counter, mkOKResultJSON("sess-pause-launch")))
	ws := t.TempDir()
	tk := queuedSequence(t, root, cfg, "pause before launch", ws)

	release := make(chan struct{})
	admissionPreInvokeHook = func() { <-release }
	t.Cleanup(func() {
		admissionPreInvokeHook = nil
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

	running := waitReservedAttempt(t, root, tk.ID, 5*time.Second)
	attemptID := ""
	if running != nil {
		attemptID = running.ActiveAttemptID
	}
	if _, err := setAdmissionPaused(root, true, "ops", "drain"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runner: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("runner did not return after pause")
	}
	if n := providerCallCount(t, counter); n != 0 {
		t.Fatalf("provider stub call count=%d, want 0", n)
	}
	if attemptID != "" {
		assertAttemptClosed(t, root, tk.ID, attemptID)
	}
	assertNoSemanticCompletion(t, root, tk.ID)
}

func TestPauseThenResumeAfterReservationOldEpochCannotLaunch(t *testing.T) {
	root := testRoot(t)
	counter := filepath.Join(t.TempDir(), "calls")
	cfg := runTaskCfg(t, countingClaudeBin(t, counter, mkOKResultJSON("sess-stale-epoch")))
	ws := t.TempDir()
	tk := queuedSequence(t, root, cfg, "stale epoch launch", ws)

	release := make(chan struct{})
	admissionPreInvokeHook = func() { <-release }
	t.Cleanup(func() {
		admissionPreInvokeHook = nil
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

	running := waitReservedAttempt(t, root, tk.ID, 5*time.Second)
	attemptID := ""
	var boundEpoch uint64
	if running != nil {
		attemptID = running.ActiveAttemptID
		boundEpoch = running.AdmissionEpoch
	}
	if _, err := setAdmissionPaused(root, true, "ops", "drain"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := setAdmissionPaused(root, false, "ops", "resume"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runner: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("runner did not return after pause/resume")
	}
	if n := providerCallCount(t, counter); n != 0 {
		t.Fatalf("provider stub call count=%d, want 0", n)
	}
	if attemptID != "" {
		assertAttemptClosed(t, root, tk.ID, attemptID)
	}
	assertNoSemanticCompletion(t, root, tk.ID)
	if extra := extraWorkIDs(t, root, tk.ID); len(extra) != 0 {
		t.Fatalf("old epoch created follow-on work %v", extra)
	}
	st, err := loadAdmissionState(root)
	if err != nil {
		t.Fatal(err)
	}
	if st.Epoch == boundEpoch {
		t.Fatalf("expected epoch change, still %d", boundEpoch)
	}
}

func TestPausedOrEpochStalePostCompleteNoFollowOn(t *testing.T) {
	payload := "done\n```json\n{\"goal\":\"g\",\"done\":[\"d\"],\"tasks\":[{\"title\":\"emitted-child\",\"prompt\":\"x\"}]}\n```"
	passReport := "```json\n{\"verdict\":\"pass\",\"p0\":[],\"p1\":[],\"p2\":[],\"summary\":\"过\"}\n```"
	concerns := reviewReport

	for _, tc := range []struct {
		name        string
		pauseResume bool
	}{
		{name: "paused", pauseResume: false},
		{name: "epoch-stale-after-resume", pauseResume: true},
	} {
		t.Run(tc.name+"/review-emit-progress", func(t *testing.T) {
			root := testRoot(t)
			withSchedulerLock(t, root)
			cfg := testCfg()
			cfg.MaxFixRounds = 3
			tk := queuedSequence(t, root, cfg, "follow-on parent", t.TempDir())
			tk.EmitProgress = true
			tk.EmitTasks = true
			tk.ReviewAfter = true
			if err := saveTask(root, tk); err != nil {
				t.Fatal(err)
			}
			if err := reserveDispatchAttempt(root, tk); err != nil {
				t.Fatal(err)
			}
			denyAdmission(t, root, tc.pauseResume)
			postComplete(root, cfg, tk, &claudeResult{Result: payload}, nil)
			if extra := extraWorkIDs(t, root, tk.ID); len(extra) != 0 {
				t.Fatalf("postComplete created child %v", extra)
			}
			if _, err := os.Stat(progressPath(root, tk.ID)); err == nil {
				t.Fatal("postComplete published progress")
			}
		})
		t.Run(tc.name+"/fix-closeout", func(t *testing.T) {
			root := testRoot(t)
			withSchedulerLock(t, root)
			cfg := testCfg()
			impl := mkImplTask(t, root, cfg)
			impl.Closeout = "write back done"
			if err := saveTask(root, impl); err != nil {
				t.Fatal(err)
			}
			if err := reserveDispatchAttempt(root, impl); err != nil {
				t.Fatal(err)
			}
			rv := mkReviewTask(t, root, cfg, impl)
			if err := reserveDispatchAttempt(root, rv); err != nil {
				t.Fatal(err)
			}
			denyAdmission(t, root, tc.pauseResume)
			before := extraWorkIDs(t, root, rv.ID)
			postComplete(root, cfg, rv, &claudeResult{Result: concerns}, nil)
			postComplete(root, cfg, rv, &claudeResult{Result: passReport}, nil)
			after := extraWorkIDs(t, root, rv.ID)
			for _, id := range after {
				known := false
				if id == impl.ID {
					known = true
				}
				for _, b := range before {
					if b == id {
						known = true
					}
				}
				if !known {
					t.Fatalf("review postComplete created follow-on %s (before=%v after=%v)", id, before, after)
				}
			}
		})
		t.Run(tc.name+"/cross", func(t *testing.T) {
			root := testRoot(t)
			withSchedulerLock(t, root)
			cfg := testCrossCfg()
			a := newTask(root, cfg, typeCrossCheck, "交叉A[opus-codex]: 裁决X", "/tmp/proj",
				[]string{"独立作答"}, 5)
			a.XRole = "A"
			a.XKey = "xopaque-admission"
			a.XProfile = "opus-codex"
			a.XTask = "裁决X"
			if err := applyCrossEngine(a, cfg.CrossProfiles["opus-codex"].A, cfg); err != nil {
				t.Fatal(err)
			}
			fb, ferr := freezeCrossEngine(cfg.CrossProfiles["opus-codex"].B, cfg)
			if ferr != nil {
				t.Fatal(ferr)
			}
			a.XEngineB = fb
			if err := saveTask(root, a); err != nil {
				t.Fatal(err)
			}
			if err := reserveDispatchAttempt(root, a); err != nil {
				t.Fatal(err)
			}
			denyAdmission(t, root, tc.pauseResume)
			postComplete(root, cfg, a, &claudeResult{Result: "甲结论"}, nil)
			if extra := extraWorkIDs(t, root, a.ID); len(extra) != 0 {
				t.Fatalf("cross postComplete created child %v", extra)
			}
		})
	}
}

func denyAdmission(t *testing.T, root string, pauseResume bool) {
	t.Helper()
	if _, err := setAdmissionPaused(root, true, "ops", "drain"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if pauseResume {
		if _, err := setAdmissionPaused(root, false, "ops", "resume"); err != nil {
			t.Fatalf("resume: %v", err)
		}
	}
}

func TestTickWhilePausedDoesNotReconcileReviewChild(t *testing.T) {
	root := testRoot(t)
	cfg := runTaskCfg(t, fakeClaudeBin(t, mkOKResultJSON("sess-tick"), "", 0))
	cfg.DrainRescanSec = 1
	parent := queuedSequence(t, root, cfg, "pending review parent", t.TempDir())
	parent.Status = statusHeld
	parent.ReviewAfter = true
	parent.SolMaxAdversarialReview = true
	parent.ReviewObligationPending = true
	parent.SchedulingEligible = boolPtr(false)
	parent.ControlState = controlTerminal
	if err := saveTask(root, parent); err != nil {
		t.Fatal(err)
	}
	if _, err := setAdmissionPaused(root, true, "ops", "drain"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := tick(root, cfg, false, true); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if extra := extraWorkIDs(t, root, parent.ID); len(extra) != 0 {
		t.Fatalf("tick while paused created review/cross child %v", extra)
	}
}

func TestForceDoesNotBypassPausedAdmission(t *testing.T) {
	root := testRoot(t)
	counter := filepath.Join(t.TempDir(), "calls")
	cfg := runTaskCfg(t, countingClaudeBin(t, counter, mkOKResultJSON("sess-force")))
	cfg.DrainRescanSec = 1
	tk := queuedSequence(t, root, cfg, "force while paused", t.TempDir())
	if _, err := setAdmissionPaused(root, true, "ops", "drain"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := tick(root, cfg, true, true); err != nil {
		t.Fatalf("tick -force: %v", err)
	}
	fresh, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status != statusQueued || fresh.ActiveAttemptID != "" {
		t.Fatalf("force bypassed admission: status=%s attempt=%q", fresh.Status, fresh.ActiveAttemptID)
	}
	if n := providerCallCount(t, counter); n != 0 {
		t.Fatalf("force provider call count=%d, want 0", n)
	}
}

func TestFreshReservationUnderResumedEpochProceeds(t *testing.T) {
	root := testRoot(t)
	counter := filepath.Join(t.TempDir(), "calls")
	cfg := runTaskCfg(t, countingClaudeBin(t, counter, mkOKResultJSON("sess-fresh")))
	ws := t.TempDir()
	if _, err := setAdmissionPaused(root, true, "ops", "drain"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	resumed, err := setAdmissionPaused(root, false, "ops", "resume")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.Epoch != 2 || resumed.Paused {
		t.Fatalf("resume state=%+v, want epoch 2 open", resumed)
	}
	bind := queuedSequence(t, root, cfg, "fresh bind", "/tmp")
	withSchedulerLock(t, root)
	if err := reserveDispatchAttempt(root, bind); err != nil {
		t.Fatalf("fresh reserve: %v", err)
	}
	if bind.AdmissionEpoch != resumed.Epoch {
		t.Fatalf("task admission epoch=%d, want %d", bind.AdmissionEpoch, resumed.Epoch)
	}
	rec, err := loadAttempt(root, bind.ID, bind.ActiveAttemptID)
	if err != nil || rec == nil {
		t.Fatalf("load attempt: %v", err)
	}
	if rec.AdmissionEpoch != resumed.Epoch {
		t.Fatalf("attempt admission epoch=%d, want %d", rec.AdmissionEpoch, resumed.Epoch)
	}
	tk := queuedSequence(t, root, cfg, "fresh after resume", ws)
	if err := runTask(context.Background(), root, cfg, tk, false); err != nil {
		t.Fatalf("runTask: %v", err)
	}
	if n := providerCallCount(t, counter); n == 0 {
		t.Fatal("fresh reservation did not invoke provider")
	}
	done, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != statusDone {
		t.Fatalf("status=%s, want done", done.Status)
	}
}

func TestLegacyMissingAdmissionEpochZeroCompatible(t *testing.T) {
	root := testRoot(t)
	withSchedulerLock(t, root)
	cfg := runTaskCfg(t, fakeClaudeBin(t, mkOKResultJSON("sess-legacy"), "", 0))
	bind := queuedSequence(t, root, cfg, "legacy bind", "/tmp")
	st, err := loadAdmissionState(root)
	if err != nil {
		t.Fatal(err)
	}
	if st.Paused || st.Epoch != 0 {
		t.Fatalf("legacy admission=%+v, want open epoch 0", st)
	}
	if err := reserveDispatchAttempt(root, bind); err != nil {
		t.Fatalf("legacy reserve: %v", err)
	}
	if bind.AdmissionEpoch != 0 {
		t.Fatalf("legacy task epoch=%d, want 0", bind.AdmissionEpoch)
	}
	rec, err := loadAttempt(root, bind.ID, bind.ActiveAttemptID)
	if err != nil || rec == nil {
		t.Fatalf("legacy attempt: %v rec=%+v", err, rec)
	}
	if rec.AdmissionEpoch != 0 {
		t.Fatalf("legacy attempt epoch=%d, want 0", rec.AdmissionEpoch)
	}
	tk := queuedSequence(t, root, cfg, "legacy epoch 0", t.TempDir())
	if err := runTask(context.Background(), root, cfg, tk, false); err != nil {
		t.Fatalf("legacy runTask: %v", err)
	}
	done, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != statusDone {
		t.Fatalf("legacy status=%s, want done", done.Status)
	}
}

func TestPauseBetweenValidationAndStartNoProvider(t *testing.T) {
	root := testRoot(t)
	counter := filepath.Join(t.TempDir(), "calls")
	cfg := runTaskCfg(t, countingClaudeBin(t, counter, mkOKResultJSON("sess-pause-prestart")))
	ws := t.TempDir()
	tk := queuedSequence(t, root, cfg, "pause between validate and start", ws)

	entered := make(chan struct{})
	release := make(chan struct{})
	admissionPreStartHook = func() {
		close(entered)
		<-release
	}
	t.Cleanup(func() {
		admissionPreStartHook = nil
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
	if _, err := setAdmissionPaused(root, true, "ops", "drain"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runner: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("runner did not return after pause")
	}
	if n := providerCallCount(t, counter); n != 0 {
		t.Fatalf("provider stub call count=%d, want 0", n)
	}
	if attemptID != "" {
		assertAttemptClosed(t, root, tk.ID, attemptID)
	}
	assertNoSemanticCompletion(t, root, tk.ID)
}

func TestPauseBetweenStep1AndStep2NoSecondProvider(t *testing.T) {
	root := testRoot(t)
	counter := filepath.Join(t.TempDir(), "calls")
	cfg := runTaskCfg(t, countingClaudeBin(t, counter, mkOKResultJSON("sess-pause-step2")))
	ws := t.TempDir()
	tk := newTask(root, cfg, typeSequence, "pause between steps", ws, []string{"step one", "step two"}, 5)
	if err := saveTask(root, tk); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	admissionBetweenStepsHook = func() {
		close(entered)
		<-release
	}
	t.Cleanup(func() {
		admissionBetweenStepsHook = nil
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
	case <-time.After(8 * time.Second):
		t.Fatal("did not reach between-steps hook")
	}
	running, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	attemptID := running.ActiveAttemptID
	if _, err := setAdmissionPaused(root, true, "ops", "drain"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runner: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("runner did not return after pause")
	}
	if n := providerCallCount(t, counter); n != 1 {
		t.Fatalf("provider stub call count=%d, want 1", n)
	}
	if attemptID != "" {
		assertAttemptClosed(t, root, tk.ID, attemptID)
	}
	got, err := loadTask(root, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == statusDone {
		t.Fatalf("status=%s, want not done after pause between steps", got.Status)
	}
	events := readAllEventsRaw(t, root, tk.ID)
	for _, ev := range events {
		if ev.Type == evDone {
			t.Fatalf("unexpected done event in %v", eventTypes(events))
		}
	}
}

func TestMissingTaskExecRootZeroStart(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "printf started > "+shSingleQuote(marker))
	setupProcGroup(cmd)
	err := runCmdRegisteredForTask(cmd, "missing-exec-root")
	if !errors.Is(err, errAdmissionDenied) {
		t.Fatalf("missing taskExecRoot err=%v, want %v", err, errAdmissionDenied)
	}
	if cmd.Process != nil {
		t.Fatal("task-aware Start ran without taskExecRoot")
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("provider process ran despite missing taskExecRoot")
	}
}

func TestAttemptSwapZeroStartAndBind(t *testing.T) {
	root := testRoot(t)
	withSchedulerLock(t, root)
	cfg := testCfg()
	ws := t.TempDir()
	tk := queuedSequence(t, root, cfg, "attempt swap start", ws)
	if err := reserveDispatchAttempt(root, tk); err != nil {
		t.Fatal(err)
	}
	originalID := tk.ActiveAttemptID
	if originalID == "" {
		t.Fatal("expected reserved attempt")
	}
	original, err := loadAttempt(root, tk.ID, originalID)
	if err != nil || original == nil {
		t.Fatalf("load original attempt: %v", err)
	}
	swappedID := newAttemptID()
	taskExecRoot.Store(tk.ID, root)
	t.Cleanup(func() { taskExecRoot.Delete(tk.ID) })

	admissionPreStartHook = func() {
		now := time.Now().Format(time.RFC3339Nano)
		swapped := *original
		swapped.AttemptID = swappedID
		swapped.CreatedAt = now
		swapped.UpdatedAt = now
		swapped.PID = 0
		swapped.PGID = 0
		swapped.StartIdentity = ""
		swapped.State = attemptReserved
		if werr := writeAttempt(root, &swapped); werr != nil {
			t.Errorf("write swapped attempt: %v", werr)
			return
		}
		fresh, lerr := loadTask(root, tk.ID)
		if lerr != nil {
			t.Errorf("load task for swap: %v", lerr)
			return
		}
		fresh.ActiveAttemptID = swappedID
		if werr := writeTaskFile(root, fresh); werr != nil {
			t.Errorf("write swapped task: %v", werr)
		}
	}
	t.Cleanup(func() { admissionPreStartHook = nil })

	marker := filepath.Join(t.TempDir(), "started")
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "printf started > "+shSingleQuote(marker))
	cmd.Dir = ws
	setupProcGroup(cmd)
	err = runCmdRegisteredForTask(cmd, tk.ID)
	if !errors.Is(err, errAdmissionDenied) {
		t.Fatalf("attempt swap err=%v, want %v", err, errAdmissionDenied)
	}
	if cmd.Process != nil {
		t.Fatal("Start ran after active attempt swap")
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("process ran after active attempt swap")
	}
	orig, oerr := loadAttempt(root, tk.ID, originalID)
	if oerr != nil || orig == nil {
		t.Fatalf("reload original attempt: %v", oerr)
	}
	if orig.State == attemptBound || orig.PID != 0 {
		t.Fatalf("original attempt bound after swap: state=%s pid=%d", orig.State, orig.PID)
	}
	swapped, serr := loadAttempt(root, tk.ID, swappedID)
	if serr != nil || swapped == nil {
		t.Fatalf("reload swapped attempt: %v", serr)
	}
	if swapped.State == attemptBound || swapped.PID != 0 {
		t.Fatalf("swapped attempt bound: state=%s pid=%d", swapped.State, swapped.PID)
	}
}

func TestBindWriteFailureReapsProcessAndReleasesLease(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("workspace flock is POSIX")
	}
	root := testRoot(t)
	withSchedulerLock(t, root)
	cfg := testCfg()
	ws := t.TempDir()
	tk := queuedSequence(t, root, cfg, "bind write failure", ws)
	if err := reserveDispatchAttempt(root, tk); err != nil {
		t.Fatal(err)
	}
	taskExecRoot.Store(tk.ID, root)
	t.Cleanup(func() { taskExecRoot.Delete(tk.ID) })
	attemptWriteHook = func(rec *AttemptRecord) error {
		if rec != nil && rec.State == attemptBound {
			return errors.New("injected bind write failure")
		}
		return nil
	}
	t.Cleanup(func() { attemptWriteHook = nil })

	cmd := exec.CommandContext(context.Background(), "sleep", "60")
	cmd.Dir = ws
	setupProcGroup(cmd)
	err := runCmdRegisteredForTask(cmd, tk.ID)
	if err == nil {
		t.Fatal("injected bind write failure returned nil")
	}
	if cmd.Process == nil {
		t.Fatal("bind path must Start before durable bind")
	}
	if cmd.ProcessState == nil {
		t.Fatal("bind failure must Wait/reap the new process")
	}
	if processAlive(cmd.Process.Pid) {
		t.Fatalf("new process pid=%d still alive after bind failure", cmd.Process.Pid)
	}
	lease, lerr := acquireWorkspaceExecutionLease(ws)
	if lerr != nil {
		t.Fatalf("workspace lease not acquirable after bind failure: %v", lerr)
	}
	if lease != nil {
		_ = lease.Close()
	}
	if workspaceProcessResidue(ws) {
		t.Fatal("workspace execution lease still held after bind failure cleanup")
	}
}

func TestBindFailureHoldsLaunchGateUntilProcessReaped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("workspace flock is POSIX")
	}
	root := testRoot(t)
	withSchedulerLock(t, root)
	cfg := testCfg()
	ws := t.TempDir()
	tk := queuedSequence(t, root, cfg, "bind failure holds pause", ws)
	if err := reserveDispatchAttempt(root, tk); err != nil {
		t.Fatal(err)
	}
	taskExecRoot.Store(tk.ID, root)
	t.Cleanup(func() { taskExecRoot.Delete(tk.ID) })

	bindPID := make(chan int, 1)
	releaseBind := make(chan struct{})
	attemptWriteHook = func(rec *AttemptRecord) error {
		if rec != nil && rec.State == attemptBound {
			bindPID <- rec.PID
			<-releaseBind
			return errors.New("injected bind write failure")
		}
		return nil
	}
	t.Cleanup(func() { attemptWriteHook = nil })

	cmd := exec.CommandContext(context.Background(), "sleep", "60")
	cmd.Dir = ws
	setupProcGroup(cmd)
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = killProcGroup(cmd.Process.Pid)
			_ = cmd.Process.Kill()
		}
	})

	launchDone := make(chan error, 1)
	go func() {
		launchDone <- runCmdRegisteredForTask(cmd, tk.ID)
	}()

	var pid int
	select {
	case pid = <-bindPID:
	case <-time.After(5 * time.Second):
		t.Fatal("did not reach durable bind write")
	}
	if pid <= 0 || !processAlive(pid) {
		t.Fatalf("bind-failure process pid=%d not live before cleanup", pid)
	}

	oldMax := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(oldMax) })
	aboutToPause := make(chan struct{})
	pauseSawLive := make(chan bool, 1)
	go func() {
		close(aboutToPause)
		if _, err := setAdmissionPaused(root, true, "ops", "drain"); err != nil {
			t.Errorf("pause: %v", err)
		}
		pauseSawLive <- processAlive(pid)
	}()
	<-aboutToPause
	for i := 0; i < 100; i++ {
		runtime.Gosched()
	}
	select {
	case live := <-pauseSawLive:
		t.Fatalf("setAdmissionPaused returned before bind-failure cleanup (processAlive=%v)", live)
	default:
	}
	if !processAlive(pid) {
		t.Fatal("process exited while bind hook still held the launch gate")
	}

	close(releaseBind)

	select {
	case err := <-launchDone:
		if err == nil {
			t.Fatal("injected bind write failure returned nil")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("bind-failure launch did not return")
	}

	select {
	case live := <-pauseSawLive:
		if live {
			t.Fatal("setAdmissionPaused returned while bind-failure cleanup process is still live")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("setAdmissionPaused did not return after bind-failure cleanup")
	}
	if processAlive(pid) {
		t.Fatalf("new process pid=%d still alive after bind-failure cleanup", pid)
	}
	if cmd.ProcessState == nil {
		t.Fatal("bind failure must Wait/reap the new process before releasing the launch gate")
	}
}
