package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCmdAdmissionPauseResumeStatusNoDispatch(t *testing.T) {
	root := t.TempDir()

	paused := runCmdAdmission(t, []string{"pause", "-root", root})
	if paused.Epoch != 1 || !paused.Paused {
		t.Fatalf("pause state = %+v, want epoch 1 paused", paused)
	}
	if paused.Actor != "cli:admission" || paused.Reason != "pause" {
		t.Fatalf("pause actor/reason = %q/%q, want cli:admission/pause", paused.Actor, paused.Reason)
	}

	status := runCmdAdmission(t, []string{"status", "-root", root})
	if status != paused {
		t.Fatalf("status = %+v, want %+v", status, paused)
	}

	resumed := runCmdAdmission(t, []string{"resume", "-root", root})
	if resumed.Epoch != 2 || resumed.Paused {
		t.Fatalf("resume state = %+v, want epoch 2 open", resumed)
	}
	if resumed.Actor != "cli:admission" || resumed.Reason != "resume" {
		t.Fatalf("resume actor/reason = %q/%q, want cli:admission/resume", resumed.Actor, resumed.Reason)
	}

	assertNoAdmissionDispatchResidue(t, root)
}

func TestCmdAdmissionRejectsUnknownAction(t *testing.T) {
	for _, args := range [][]string{nil, {}, {"explode"}, {"PAUSE"}} {
		if err := cmdAdmission(args); err == nil {
			t.Fatalf("cmdAdmission %v: want usage error", args)
		}
	}
}

func TestCmdAdmissionRejectsExtraPositionalArgs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action string
	}{
		{name: "pause extra", action: "pause"},
		{name: "status extra", action: "status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := cmdAdmission([]string{tc.action, "-root", root, "extra"}); err == nil {
				t.Fatalf("cmdAdmission %s extra: want error", tc.action)
			}
			assertFreshRootZeroResidue(t, root)
		})
	}
}

func assertFreshRootZeroResidue(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		lower := strings.ToLower(rel)
		for _, needle := range []string{"admission", "task", "attempt", "event", "provider"} {
			if strings.Contains(lower, needle) {
				t.Errorf("unexpected %s residue: %s", needle, rel)
			}
		}
		t.Errorf("unexpected residue: %s", rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func runCmdAdmission(t *testing.T, args []string) admissionState {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	cmdErr := cmdAdmission(args)
	_ = w.Close()
	os.Stdout = old
	out, readErr := io.ReadAll(r)
	_ = r.Close()
	if cmdErr != nil {
		t.Fatalf("cmdAdmission %v: %v\nstdout=%s", args, cmdErr, out)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(out) == 0 || out[len(out)-1] != '\n' {
		t.Fatalf("want one JSON line with trailing newline, got %q", out)
	}
	if bytes.Count(out, []byte("\n")) != 1 {
		t.Fatalf("want exactly one stdout line, got %q", out)
	}
	var st admissionState
	if err := json.Unmarshal(bytes.TrimSpace(out), &st); err != nil {
		t.Fatalf("stdout json: %v (%q)", err, out)
	}
	return st
}

func assertNoAdmissionDispatchResidue(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		switch rel {
		case filepath.Join("control", "admission.json"),
			filepath.Join("control", "locks", "global-admission.lock"):
			return nil
		}
		lower := strings.ToLower(rel)
		for _, needle := range []string{"task", "attempt", "event", "runner", "provider", ".lock"} {
			if strings.Contains(lower, needle) && !strings.Contains(lower, "admission") {
				t.Errorf("unexpected dispatch residue: %s", rel)
			}
		}
		if rel != filepath.Join("control", "admission.json") &&
			rel != filepath.Join("control", "locks", "global-admission.lock") {
			t.Errorf("unexpected residue: %s", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
