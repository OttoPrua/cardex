package main

// RED/GREEN fixtures for the two blocking P1s found on integrated main a4a3acf.
//
//	P1-A  Corrupt or deleted durable task evidence disappeared from the role and
//	      custody scans. loadTasks warns and skips a file it cannot parse, so a
//	      corrupt active writer read as "no writer" and minted a second one, a
//	      corrupt rival reviewer vanished from the sweep that exists to find it,
//	      a deleted producer named by the gate read as "no producer named" and
//	      skipped every writer-bound check, and a gate naming no workflow got to
//	      match its own candidate fields against themselves.
//
//	P1-B  The verdict the gate consumed was neither final nor shape-strict.
//	      {"verdict":"pass"} decoded into empty p0/p1 arrays the reviewer never
//	      wrote, and an invalid final answer walked backward to an earlier valid
//	      one.
//
// Every case names the counter-injection that turns it red, so an assertion
// that stops proving its P1 is visible rather than merely green.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// corruptTaskFile rewrites a card's bytes as truncated JSON: the directory
// entry still exists and still ends in .json, but the card will not parse.
func corruptTaskFile(t *testing.T, root, id string) {
	t.Helper()
	if err := os.WriteFile(taskPath(root, id), []byte(`{"id":"`+id+`","status":`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func countTaskFiles(t *testing.T, root string) int {
	t.Helper()
	entries, err := os.ReadDir(tasksDir(root))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

// p1Baseline drives one workflow all the way to an admissible integration
// release and asserts that baseline before any fixture mutates it, so a later
// hold is attributable to the injection rather than to a broken setup.
func p1Baseline(t *testing.T) (root, dir string, cfg *Config, wf *WorkflowRecord, review, integ *Task) {
	t.Helper()
	root, dir = workflowTestRoot(t)
	cfg = workflowTestCfg(t, root)
	wf = initTestWorkflow(t, root, dir)
	wf, review = runWorkflowToReview(t, root, wf, verdictJSON("pass", nil, nil))
	var err error
	integ, err = loadTask(root, wf.IntegrationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if dec := evaluateIntegrationRelease(root, cfg, integ); !dec.Admit {
		t.Fatalf("baseline must be admissible before injection, got hold %q", dec.HoldReason)
	}
	return root, dir, cfg, wf, review, integ
}

// mustHold asserts both consumers of the latch refuse the same card.
func mustHold(t *testing.T, root string, cfg *Config, integ *Task, want, inject string) {
	t.Helper()
	dec := evaluateIntegrationRelease(root, cfg, integ)
	if dec.Admit {
		t.Fatalf("gate released on missing evidence; 反例注入: %s", inject)
	}
	if dec.HoldReason != want {
		t.Fatalf("hold reason = %q, want %q; 反例注入: %s", dec.HoldReason, want, inject)
	}
	if integrationGateAllows(root, cfg, integ) {
		t.Fatal("tick must refuse the same card the gate holds")
	}
}

// ---- P1-A: broken durable task evidence must never read as absence ----

// R1: a corrupt active writer must refuse a second writer, not disappear.
func TestP1ACorruptActiveWriterRefusesASecondWriter(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)

	writer, err := admitWorkflowWriter(root, cfg, wf, "")
	if err != nil {
		t.Fatal(err)
	}
	corruptTaskFile(t, root, writer.ID)
	before := countTaskFiles(t, root)

	// A manager restarting mid-round replays the same command against a stale
	// in-memory record, exactly as it would after a crash.
	stale := *wf
	if _, err := admitWorkflowWriter(root, cfg, &stale, ""); !errors.Is(err, errWorkflowBrokenEvidence) {
		t.Fatalf("an unreadable active writer must refuse admission, got %v; 反例注入: workflowActiveRole 改回 loadTasks", err)
	}
	if got := countTaskFiles(t, root); got != before {
		t.Fatalf("the refusal still minted a card: %d → %d cards", before, got)
	}
}

// R2: a corrupt rival reviewer must hold integration. The rival hidden by the
// corruption is the very card the custody sweep exists to find.
func TestP1ACorruptRivalReviewerHoldsIntegration(t *testing.T) {
	root, dir, cfg, wf, _, integ := p1Baseline(t)

	rival := newTask(root, cfg, typeReview, "rival review", dir, []string{"p"}, 5)
	rival.ReviewOf = wf.WriterTaskID
	rival.Status = statusRunning
	if err := saveTask(root, rival); err != nil {
		t.Fatal(err)
	}
	// Readable, the rival holds the gate on custody grounds.
	mustHold(t, root, cfg, integ, holdReasonCustody,
		"integrationCustodyOK 里删掉 rival reviewer 扫描")

	// Corrupting it must not buy the release that its readable form was denied.
	corruptTaskFile(t, root, rival.ID)
	mustHold(t, root, cfg, integ, holdReasonBrokenEvidence,
		"integrationCustodyOK 改回 loadTasks（损坏的 rival 被静默跳过）")
}

// R3: a producer the gate names but that no longer loads must hold, rather than
// read as "this review names no producer" and skip every writer-bound check.
func TestP1AProducerNamedByGateMustExistAndLoad(t *testing.T) {
	t.Run("deleted writer", func(t *testing.T) {
		root, _, cfg, wf, _, integ := p1Baseline(t)
		if err := os.Remove(taskPath(root, wf.WriterTaskID)); err != nil {
			t.Fatal(err)
		}
		mustHold(t, root, cfg, integ, holdReasonBrokenEvidence,
			"evaluateIntegrationRelease 里把 producer 查找改回 writer, _ = findTaskAnywhere(...)")
	})

	t.Run("corrupt writer", func(t *testing.T) {
		root, _, cfg, wf, _, integ := p1Baseline(t)
		corruptTaskFile(t, root, wf.WriterTaskID)
		mustHold(t, root, cfg, integ, holdReasonBrokenEvidence,
			"evaluateIntegrationRelease 里丢弃 producer 查找错误")
	})

	t.Run("writer named only by the review, deleted", func(t *testing.T) {
		root, _, cfg, wf, review, integ := p1Baseline(t)
		// Drop the gate's own binding so the producer can only come from the
		// review's subject, then delete that subject.
		integ.IntegrationGate.WriterTaskID = ""
		if err := saveTask(root, integ); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(taskPath(root, wf.WriterTaskID)); err != nil {
			t.Fatal(err)
		}
		if review.ReviewOf == "" {
			t.Fatal("fixture precondition: the review must name its subject")
		}
		mustHold(t, root, cfg, integ, holdReasonBrokenEvidence,
			"evaluateIntegrationRelease 里 review.ReviewOf 分支同样丢弃查找错误")
	})
}

// R4: a gate with no workflow binding, or one whose record carries no frozen
// candidate, has nothing durable to bind the verdict to and must hold. The
// gate's own commit/tree ride on the card a release would free, so matching
// them against themselves proves nothing.
func TestP1AGateWithoutWorkflowOrFrozenCandidateHolds(t *testing.T) {
	t.Run("blank workflow id", func(t *testing.T) {
		root, _, cfg, _, _, integ := p1Baseline(t)
		integ.IntegrationGate.WorkflowID = ""
		if err := saveTask(root, integ); err != nil {
			t.Fatal(err)
		}
		mustHold(t, root, cfg, integ, holdReasonIncompleteEvidence,
			"evaluateIntegrationRelease 里把 workflow 绑定改回 if gate.WorkflowID != \"\" 的可选分支")
	})

	t.Run("record without a frozen candidate", func(t *testing.T) {
		root, _, cfg, wf, _, integ := p1Baseline(t)
		fresh, err := loadWorkflow(root, cfg, wf.ID)
		if err != nil {
			t.Fatal(err)
		}
		fresh.Candidate = nil
		if err := saveWorkflow(root, cfg, fresh); err != nil {
			t.Fatal(err)
		}
		mustHold(t, root, cfg, integ, holdReasonIncompleteEvidence,
			"evaluateIntegrationRelease 里去掉 frozen == nil 的 fail-closed 分支")
	})

	t.Run("record that will not load", func(t *testing.T) {
		root, _, cfg, wf, _, integ := p1Baseline(t)
		if err := os.WriteFile(workflowPath(root, wf.ID), []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		mustHold(t, root, cfg, integ, holdReasonIncompleteEvidence,
			"evaluateIntegrationRelease 里把 loadWorkflow 失败当成\"这张卡没有 workflow\"")
	})
}

// R5: replay over the strict scan still mints exactly one card per role, and a
// named role whose card was deleted out from under the record refuses instead
// of being replaced.
func TestP1AReplayOverStrictScanMintsNoSecondRole(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)

	writer, err := admitWorkflowWriter(root, cfg, wf, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		stale := *wf
		if _, err := admitWorkflowWriter(root, cfg, &stale, ""); !errors.Is(err, errWorkflowDuplicateRole) {
			t.Fatalf("healthy replay %d minted a second writer: %v", i, err)
		}
	}

	// The record still names this writer; its card is gone.
	if err := os.Remove(taskPath(root, writer.ID)); err != nil {
		t.Fatal(err)
	}
	before := countTaskFiles(t, root)
	stale := *wf
	if _, err := admitWorkflowWriter(root, cfg, &stale, ""); !errors.Is(err, errWorkflowBrokenEvidence) {
		t.Fatalf("a deleted named writer must refuse, not be silently replaced: %v; 反例注入: admitWorkflowWriter 里删掉 requireNamedWorkflowRole", err)
	}
	if got := countTaskFiles(t, root); got != before {
		t.Fatalf("the refusal still minted a card: %d → %d cards", before, got)
	}
}

// R5 (reviewer half): the same discipline on the reviewer role.
func TestP1AReplayOverStrictScanMintsNoSecondReviewer(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)

	writer, err := admitWorkflowWriter(root, cfg, wf, "")
	if err != nil {
		t.Fatal(err)
	}
	markTaskDone(t, root, writer.ID)
	commit, tree := workflowHeadCandidate(t, dir)
	if err := freezeWorkflowCandidate(root, cfg, wf, WorkflowCandidate{Commit: commit, Tree: tree}); err != nil {
		t.Fatal(err)
	}
	review, err := admitWorkflowReviewer(root, cfg, wf)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		stale := *wf
		if _, err := admitWorkflowReviewer(root, cfg, &stale); !errors.Is(err, errWorkflowDuplicateRole) {
			t.Fatalf("healthy replay %d minted a second reviewer: %v", i, err)
		}
	}

	corruptTaskFile(t, root, review.ID)
	before := countTaskFiles(t, root)
	stale := *wf
	if _, err := admitWorkflowReviewer(root, cfg, &stale); !errors.Is(err, errWorkflowBrokenEvidence) {
		t.Fatalf("an unreadable named reviewer must refuse, not be replaced: %v; 反例注入: admitWorkflowReviewer 里删掉 requireNamedWorkflowRole", err)
	}
	if got := countTaskFiles(t, root); got != before {
		t.Fatalf("the refusal still minted a card: %d → %d cards", before, got)
	}
}

// G1: the one failure the scan may tolerate is the ReadDir→ReadFile race, where
// an entry is already gone by the time it is read. A dangling entry reproduces
// that deterministically; a corrupt one must still be an error.
func TestP1AScanToleratesTheArchiveRaceButNotCorruption(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)
	writer, err := admitWorkflowWriter(root, cfg, wf, "")
	if err != nil {
		t.Fatal(err)
	}

	// An entry ReadDir listed whose bytes are already gone: what a concurrent
	// archive or cancel looks like from inside the scan.
	ghost := taskPath(root, "t-archived-mid-scan")
	if err := os.Symlink(filepath.Join(root, "archive", "moved-away.json"), ghost); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	tasks, err := scanWorkflowTasks(root)
	if err != nil {
		t.Fatalf("a concurrently archived entry must be skipped, not fatal: %v; 反例注入: scanWorkflowTasks 里删掉 os.IsNotExist 分支", err)
	}
	found := false
	for _, tk := range tasks {
		if tk.ID == writer.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("the healthy writer must still be in the scan")
	}
	if _, _, err := workflowActiveRole(root, wf, "writer"); err != nil {
		t.Fatalf("the race must not hold the role scan closed: %v", err)
	}

	// Corruption is not the race and must not be tolerated as one.
	corruptTaskFile(t, root, writer.ID)
	if _, err := scanWorkflowTasks(root); !errors.Is(err, errWorkflowBrokenEvidence) {
		t.Fatalf("an unparseable card must be broken evidence, got %v", err)
	}
}

// ---- P1-B: the gate's verdict must be final and shape-complete ----

func TestP1BVerdictEvidenceShapeIsStrict(t *testing.T) {
	complete := `{"verdict":"pass","p0":[],"p1":[],"p2":[],"summary":"ok"}`
	fence := func(body string) string { return "报告\n```json\n" + body + "\n```\n" }

	cases := []struct {
		name, body, wantHold, wantVerdict string
	}{
		{"complete pass", fence(complete), "", "pass"},
		{"complete concerns", fence(`{"verdict":"concerns","p0":[],"p1":["x"],"p2":[],"summary":"s"}`), "", "concerns"},
		{"unfenced complete pass", "结论 " + complete + " 完", "", "pass"},
		{"empty transcript", "", holdReasonMissingOutput, ""},
		{"no verdict object", "自由文本报告，没有机读结论", holdReasonUnknownVocabulary, ""},
		{"unknown token", fence(`{"verdict":"ACCEPT","p0":[],"p1":[],"p2":[],"summary":"s"}`), holdReasonUnknownVocabulary, ""},
		// R6: the arrays are missing, not empty. A value-typed decode cannot
		// tell those apart, and at this gate they are opposite answers.
		{"missing all arrays", fence(`{"verdict":"pass"}`), holdReasonIncompleteEvidence, ""},
		{"missing p1", fence(`{"verdict":"pass","p0":[],"p2":[],"summary":"s"}`), holdReasonIncompleteEvidence, ""},
		// R7
		{"missing summary", fence(`{"verdict":"pass","p0":[],"p1":[],"p2":[]}`), holdReasonIncompleteEvidence, ""},
		{"blank summary", fence(`{"verdict":"pass","p0":[],"p1":[],"p2":[],"summary":"  "}`), holdReasonIncompleteEvidence, ""},
		// R8: an invalid final answer is the reviewer's answer. No walk-back.
		{"earlier pass then unknown final", fence(complete) + fence(`{"verdict":"maybe","p0":[],"p1":[],"p2":[],"summary":"s"}`), holdReasonUnknownVocabulary, ""},
		{"earlier pass then incomplete final", fence(complete) + fence(`{"verdict":"pass"}`), holdReasonIncompleteEvidence, ""},
		// R9
		{"two competing complete verdicts", fence(complete) + fence(complete), holdReasonCompetingVerdicts, ""},
		{"complete pass then complete block", fence(complete) + fence(`{"verdict":"block","p0":["x"],"p1":[],"p2":[],"summary":"s"}`), holdReasonCompetingVerdicts, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, hold := parseReviewVerdictEvidence(tc.body)
			if hold != tc.wantHold {
				t.Fatalf("hold = %q, want %q", hold, tc.wantHold)
			}
			if tc.wantVerdict == "" {
				if v != nil {
					t.Fatalf("a held terminal must yield no verdict, got %+v", v)
				}
				return
			}
			if v == nil || v.Verdict != tc.wantVerdict {
				t.Fatalf("verdict = %+v, want %q", v, tc.wantVerdict)
			}
		})
	}
}

// R6/R7/R8/R9 at the gate: every shape the strict reading refuses must also
// keep the integration card held through both consumers and through release.
func TestP1BIncompleteOrNonFinalVerdictNeverReleases(t *testing.T) {
	complete := `{"verdict":"pass","p0":[],"p1":[],"p2":[],"summary":"ok"}`
	fence := func(body string) string { return "报告\n```json\n" + body + "\n```\n" }

	cases := []struct {
		name, body, want, inject string
	}{
		{"R6 pass without arrays", fence(`{"verdict":"pass"}`), holdReasonIncompleteEvidence,
			"reviewVerdictWire 的指针字段改回值类型（缺失的 p0/p1 会解成空数组）"},
		{"R7 pass without summary", fence(`{"verdict":"pass","p0":[],"p1":[],"p2":[]}`), holdReasonIncompleteEvidence,
			"shape() 里去掉 Summary 必填"},
		{"R8 earlier pass, invalid final", fence(complete) + fence(`{"verdict":"maybe","p0":[],"p1":[],"p2":[],"summary":"s"}`), holdReasonUnknownVocabulary,
			"evaluateIntegrationRelease 改回 parseReviewVerdict（向前回溯到更早的合法 verdict）"},
		{"R9 two competing complete verdicts", fence(complete) + fence(complete), holdReasonCompetingVerdicts,
			"parseReviewVerdictEvidence 里去掉 complete > 1 的判断"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, dir := workflowTestRoot(t)
			cfg := workflowTestCfg(t, root)
			wf := initTestWorkflow(t, root, dir)
			wf, _ = runWorkflowToReview(t, root, wf, tc.body)

			integ, err := loadTask(root, wf.IntegrationTaskID)
			if err != nil {
				t.Fatal(err)
			}
			mustHold(t, root, cfg, integ, tc.want, tc.inject)

			if err := ingestWorkflowReview(root, cfg, wf); err != nil {
				t.Fatal(err)
			}
			if wf.Review == nil || wf.Review.Admissible {
				t.Fatalf("ingest must not record an admissible review: %+v", wf.Review)
			}
			if wf.Review.HoldReason != tc.want {
				t.Fatalf("ingested hold reason = %q, want %q", wf.Review.HoldReason, tc.want)
			}
			if err := tryReleaseWorkflowIntegration(root, cfg, wf); !errors.Is(err, errWorkflowHeld) {
				t.Fatalf("try-release must refuse: %v", err)
			}
			if err := cmdSetStatus([]string{"-root", root, integ.ID}, "release"); err == nil {
				t.Fatal("cardex release must refuse a gated card without admissible evidence")
			}
			after, err := loadTask(root, integ.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Status != statusHeld {
				t.Fatalf("integration must remain held, got %s", after.Status)
			}
		})
	}
}

// G2: none of the above may cost the legitimate path. A single complete
// well-formed final pass still ingests, releases integration, and still leaves
// live and cutover held.
func TestP1GreenCompleteFinalPassStillReleasesEndToEnd(t *testing.T) {
	root, dir := workflowTestRoot(t)
	cfg := workflowTestCfg(t, root)
	wf := initTestWorkflow(t, root, dir)
	wf, _ = runWorkflowToReview(t, root, wf,
		"审核正文\n```json\n{\"verdict\":\"pass\",\"p0\":[],\"p1\":[],\"p2\":[\"nit\"],\"summary\":\"complete terminal\"}\n```\n")

	if err := ingestWorkflowReview(root, cfg, wf); err != nil {
		t.Fatal(err)
	}
	if wf.Review == nil || !wf.Review.Admissible {
		t.Fatalf("a complete final pass must stay admissible: %+v", wf.Review)
	}
	if err := tryReleaseWorkflowIntegration(root, cfg, wf); err != nil {
		t.Fatalf("a complete final pass must still release integration: %v", err)
	}
	integ, err := loadTask(root, wf.IntegrationTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if integ.Status != statusQueued {
		t.Fatalf("released integration status = %s", integ.Status)
	}
	if !integrationGateAllows(root, cfg, integ) {
		t.Fatal("tick must dispatch the released card")
	}
	if wf.EffectGates.Integration != effectGateReleased {
		t.Fatalf("integration gate = %s", wf.EffectGates.Integration)
	}
	if wf.EffectGates.Live != effectGateHeld || wf.EffectGates.Cutover != effectGateHeld {
		t.Fatalf("integration release is never live authority: %+v", wf.EffectGates)
	}
}
