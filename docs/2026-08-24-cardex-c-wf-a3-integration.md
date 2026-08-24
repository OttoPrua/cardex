# Cardex C-WF (Issue #6) — A3 integration record

Tracking issue: [#6 — Cloud handoff: continue Workflow Modes WIP](https://github.com/OttoPrua/Cardex/issues/6)

This is the **integration-side** record for the C-WF workflow-modes chain. It
documents what was merged, on what review basis, and the post-merge gate
results on the integrated `main`. It is written after the fact and changes no
product behaviour.

## 1. What was merged

The reviewed chain was linear and already contained the post-C-PROC `main`:

| Field | Value |
| --- | --- |
| Base at merge time | `main` @ `33cba104badb092ddeae7a3f7e7e172d2b4a7313` (post C-PROC PR #8) |
| [PR #7](https://github.com/OttoPrua/Cardex/pull/7) head | `722670bf4abe0518ec78327781743b379bf5dd34` (`cursor/c-wf-issue6-9d89`) |
| [PR #9](https://github.com/OttoPrua/Cardex/pull/9) head | `e0e55da2667aa1be4f698ad3a2cc97f6fd158532` (`cursor/c-wf-p1-fix-9d89`) |
| Merge commit | `a4a3acf692dd49a589210093047824591d0270b3` |
| Merged tree | `772c0ef1936f241edd0f44b6a34821b33bb92e64` |

Ancestry verified before merging: `722670b` is an ancestor of `e0e55da`, and
`33cba10` is an ancestor of `e0e55da`. Because the chain head already contained
`main`, the `--no-ff` merge produced a tree **byte-identical** to the reviewed
tree at `e0e55da` (`git rev-parse HEAD^{tree}` equals
`e0e55da^{tree}` = `772c0ef1936f241edd0f44b6a34821b33bb92e64`). What is on
`main` is exactly what was reviewed.

Review basis: replacement review verdict **APPROVE, P0/P1 = 0** (reviewer
`bc-947f5d00`), covering PR #7 (workflow modes: durable serial/federated
records with fail-closed integration gate) plus PR #9 (P1 fix: writer/reviewer
separation, audit fail-closed, freeze verification).

Mechanics: merge pushed as `33cba10..a4a3acf main -> main`; the issue branch
`cursor/c-wf-issue6-9d89` was then fast-forwarded `722670b..e0e55da` so GitHub
records the whole chain as merged. PR #7 `mergedAt 2026-08-24T07:56:01Z`,
PR #9 `mergedAt 2026-08-24T07:56:03Z`.

Change set: 20 files, +3495 / −37. New files `workflow.go`, `workflow_cmd.go`,
`workflow_gate.go`, `workflow_loop.go`, `workflow_test.go`,
`templates/workflow-writer.md`; edits to `grok.go`, `main.go`, `runner.go`,
`stakes.go`, `task.go`, `tick.go`, docs and READMEs.

## 2. Post-merge gates on integrated `main`

Run on `main` @ `a4a3acf`, Go 1.24.6, linux/amd64 (the module requires 1.24;
the image ships 1.22.2, so a 1.24.6 toolchain was installed for the gates).

| Gate | Result |
| --- | --- |
| Focused (33 workflow tests, exact-name anchored) | pass, 60.4s |
| Full `go test -count=1 ./...` | pass, 130.4s |
| `go test -race -count=1 ./...` | run 1 fail (flake, see below); runs 2–3 pass outright, 374.1s / 351.1s, zero `DATA RACE` |
| `go vet ./...` | pass |
| `gofmt -l` | no merge-touched file flagged; same seven pre-existing files as baseline (see below) |
| `git fsck --full` | pass |
| Double build | byte-identical: plain build and `-a` rebuild both sha256 `db50d7bf476d675cf79401510a80f2a2bcc365e407c3fb89638e8a7e80d3891c` |

### Baseline separation

- `gofmt -l` flags the same seven files (`boardgoal.go`, `boardgoal_test.go`,
  `codex_reliability_test.go`, `events_test.go`, `limit_test.go`,
  `oauth_usage_test.go`, `reviewsync_workspace_test.go`) on the integrated
  `main` and on pre-merge `main` `33cba104`, re-verified in an isolated
  worktree. None of these files is touched by this merge. This is the same
  seven-file baseline recorded in the C-PROC evidence
  (`docs/2026-08-24-cardex-0.10.12-process-truth-review.md`, §4).

### Race-mode flake, recorded because it will recur

The first full `-race` run failed after 360.3s. The run was non-verbose and
only the tail was kept, so the failing test name was **not captured**; the
visible failing-test output was two workflow creations from the shared test
helper (`已创建 workflow wf0824-0807-… mode=serial module=auth … (held)`).
Two consecutive full `-race` re-runs with complete captured logs passed
outright with zero `DATA RACE` and zero `--- FAIL`. This matches the
load-sensitive flake class already recorded in the C-PROC evidence (§4), but
the failing test here is unidentified; the next full `-race` failure should be
run with a captured log so the test can be named.

## 3. Issue #6 state

The merge commit body contains `Closes #6`. At the time this record was
written the issue was still `OPEN` on GitHub; the closing keyword had not yet
been processed, and this environment's GitHub CLI is read-only, so the close
could not be forced. If the keyword does not take effect, Issue #6 needs a
manual close referencing merge `a4a3acf`.

## 4. Boundary

No install, launchd, scheduler, canary, or other live effect was performed.
All gate runs used temporary directories and scratch worktrees; the only
remote effects are the two branch pushes described in §1.

## 5. Status

C-WF is integrated. `main` @ `a4a3acf692dd49a589210093047824591d0270b3` holds
the reviewed tree with all offline gates green (race green on re-run, flake
recorded above). Ready for A4 (first W1–W4 packet) on top of this `main`.
