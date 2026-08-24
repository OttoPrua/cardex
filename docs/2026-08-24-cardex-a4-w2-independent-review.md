# 2026-08-24 · A4 W2 independent review — reviewer-custody validator

Independent, read-only review of the W2 reviewer-custody packet. The reviewer is a
separate agent context from the writer; no candidate code was modified.

| Item | Value |
|---|---|
| Reviewer runtime model | `claude-opus-5` (Opus 5, extended thinking). The orchestrator's requested slug string `claude-opus-5-thinking-high` could not be confirmed verbatim: the run-metadata service returned `originalModelName: null`, so the exact thinking-budget suffix is unverifiable from inside the run. |
| Reviewed commit | `f9c0c2aed5418e5a5382f0f9e10e699d40b4ce99` |
| Reviewed tree | `6467e6341be7ad214767bd9d1dcb1450e2c24708` |
| Base | `main` `a4a3acf692dd49a589210093047824591d0270b3` |
| Dry-run receipt commit | `7590d7fe02a3acfa372aee9b9a6694554a4ba1ac` (tree `0dec50b4989f81711592af5332533e3353f4c7cd`) |
| Branch | `cursor/cardex-w1w4-first-packet-9d89` |
| **Verdict** | **REQUEST_CHANGES** |
| P0 | 0 |
| P1 | 2 |
| P2 | 7 |
| evidence_complete | **false** |

The candidate was reviewed from a detached `git worktree` pinned to
`f9c0c2ae…`; the tree hash was re-verified as `6467e634…` before and after every
command, and the packet was never reviewed against a moving branch head.

## Verification performed

All commands ran on the frozen checkout with `GOTOOLCHAIN=go1.24.6`. The
preinstalled toolchain is Go 1.22, which cannot build a `go 1.24` module, and
`GOTOOLCHAIN=auto` fails to resolve the bare `go1.24` directive; an explicit
patch version is required.

| Gate | Result |
|---|---|
| `gofmt -l` on the five changed Go files | clean |
| `gofmt -l .` (whole tree) | 7 files unformatted — `boardgoal.go`, `boardgoal_test.go`, `codex_reliability_test.go`, `events_test.go`, `limit_test.go`, `oauth_usage_test.go`, `reviewsync_workspace_test.go`. All 7 are identically unformatted on base `main`; the candidate neither adds to nor fixes them. |
| `go vet ./...` | clean, exit 0 |
| `go test ./... -count=1` on candidate | `ok cardex 159.980s` |
| `go test ./... -count=1` on base `main` | `ok cardex 129.704s` |
| `go test -run 'Custody\|Workflow' -v` | all 8 W2 test functions pass, including the 4 sub-cases of `TestCustodyEveryProducerGoneComponentFailsClosed` |
| `make test` (`bash test/integration.sh`) on candidate | 117 pass, 3 fail — scenarios 19, 22, 27 |
| `make test` on base `main` | 117 pass, 3 fail — scenarios 19, 22, 27 (identical) |

**No regression** against the acknowledged baseline: the integration script
produces byte-identical pass/fail counts and the same three failing scenarios on
base and candidate.

## What the packet gets right

These were verified independently rather than taken on the writer's word.

- The gate really is read-only with respect to custody. `reviewCustodyReceiptReason`
  re-derives drift and the evidence hash on every call and only *compares* against
  the durable receipt; `ingestWorkflowReview` is the sole non-test caller of
  `observeReviewCustody`, and the only non-test caller of `ingestWorkflowReview` is
  the explicit `cardex workflow ingest-review` CLI. `tick` reaches custody only
  through `integrationGateAllows` → `evaluateIntegrationRelease`, which writes nothing.
- The receipt is bound to the evidence hash, not merely to the terminal, so
  post-receipt transcript movement genuinely re-holds a released card
  (`TestCustodyReceiptGoesStaleWhenEvidenceMovesAfterCompletion`), and the
  hash inputs used by the gate and by ingest are forced to agree because
  `candidateIdentitiesMatch` runs before the custody check.
- The containment terminal really is non-semantic: `held`/`canceled` can only
  produce `custody_reconciled_held`, and `reviewCustodyReceiptReason` rejects it
  with `custody_receipt_not_semantic`.
- `custody_quiet_window` completion requires two separate observations across real
  wall-clock time. The 20-second constant is never shrunk for tests; the fixtures
  backdate the durable `first_observed_at` instead, which keeps production
  semantics pinned.
- The fixtures never spawn or signal a real process; probes are injected through
  `reviewCustodyProbes` and restored via `t.Cleanup`.
- `docs/workflows.md`, `docs/workflows.en.md` and both changelogs are updated
  consistently with the code, and the roadmap row is flipped with the W3/W4 items
  left explicitly unlanded.

## Dry-run receipt: independently reproduced

The dry-run receipt is unusually well-constructed and was reproduced rather than
read. Both self-describing digests in the receipt regenerate exactly from the
document's own fenced blocks:

- embedded script → `11fa3abfd662cc706864b802bc2612ba72c0f3a5650d773d5f0c1c643d952fd0` (matches)
- transcript → `822c4f693ccd2475b428138100b92285441b5fda2eafb5986bc76dda65f4cd41` (matches)

Re-running the embedded script verbatim against the frozen candidate (with only
`REPO`, `BASE`, and the Go toolchain export redirected) produced output that is
line-for-line identical to the recorded transcript after normalising card IDs,
digests and the base path. The single residual difference is the sort order of
three writer cards in one listing, which is ID-dependent. `~/.cardex` does not
exist on this machine, confirming the untouched-home claim; the script's writes
are confined to its own `/tmp` base, and no `tick`, `install`, `install-launchd`,
Board/web, or live/cutover path is invoked.

Two scope caveats on that receipt are recorded as P2-6 and P2-7 below.

## Findings

### P1-1 — `successorAttemptDrift` compares RFC3339Nano stamps as raw strings, so same-card redispatch is missed and the gate admits release

`workflow_custody.go:209` and `workflow_custody.go:230` order attempt and
transition records with `>` on `CreatedAt` strings. Those stamps are produced by
`time.Now().Format(time.RFC3339Nano)` (`attempt_control.go:977`,
`attempt_control.go:657`), which emits the **local** UTC offset and **trims
trailing zeros** from the fractional second. Lexicographic order is therefore not
chronological order.

Demonstrated on the frozen candidate with an out-of-tree probe test. A successor
attempt minted five real minutes after the terminal attempt, stamped with a
different UTC offset, is not detected:

```
terminal  attempt CreatedAt = 2026-08-24T17:00:00.000000001+08:00   (09:00:00Z)
successor attempt CreatedAt = 2026-08-24T09:05:00.000000001Z        (09:05:00Z, 5m later)

absolute order: successor is 5m0s after the terminal attempt
reviewCustodyDrift(...)            = ""            <-- no drift reported
wf.Review.Admissible               = true          <-- pass adopted
wf.Review.HoldReason               = ""
evaluateIntegrationRelease(...).Admit = true       <-- integration release admitted
```

This is precisely the incident shape W2 exists to refuse, and the ordering check
is the only line of defence for it: a *closed* successor is invisible to the
`reserved`/`bound` open-attempt count and to `attemptProducerAlive`, and writing
the successor record only resets the quiet window rather than failing it, so 20
seconds of stability is enough to reach the gate.

Mixed offsets are not exotic for this repository. It ships `install-launchd`, so
the same data root is routinely written both by a scheduled job and by an
interactive shell, and any DST-observing zone changes its own offset twice a year
— a fall-back transition alone inverts the comparison for records minted minutes
apart.

Suggested fix: parse both stamps with `time.Parse(time.RFC3339Nano, …)` and
compare the resulting `time.Time` values, treating an unparseable stamp as drift
(fail closed) rather than as an ordering tie. The same treatment is needed for the
terminal-transition selection loop, which picks the newest committed terminal by
the same string comparison and can therefore select the wrong terminal attempt to
anchor against.

### P1-2 — absent attempt evidence is treated as proven `producerGone`, and in a fresh CLI process two of the four probe legs cannot fire at all

`listReviewAttempts` returns `(nil, nil)` when the attempts directory does not
exist, and `reviewCustodyDrift` then skips the per-attempt loop entirely and
reports no drift. `successorAttemptDrift` likewise returns early when the newest
committed terminal transition carries an empty `AttemptID`. A review terminal with
no attempt evidence at all therefore mints a full `admissible_review` receipt:

```
attempt records on disk for the review terminal: 0
committed terminal transitions naming an attempt: 0
receipt kind="admissible_review" semantic=true admissible=true
```

On its own this would be a hardening item. What makes it P1 is what remains of
the proof in the processes that actually write and re-derive the receipt. Of the
four probes in `custodyProbes`, two are backed by package-level maps that are
populated only by `runTask` in the *same* process — `taskPG`, `taskPGResidue` and
`taskLeaseResidue` in `proc.go:42-55` are never persisted or rebuilt from disk.
`cardex workflow ingest-review` and `cardex release` are short-lived CLI
invocations, so `anyTaskProcAlive` and `taskProcessResidue` are structurally
`false` there regardless of what is running. Only two legs are genuinely
cross-process: `attemptProducerAlive` (reads the PID/PGID start identity) and
`workspaceLeaseHeld` (kernel `flock`, correctly documented as restart-safe in
`proc_unix.go:67`).

Combine the two facts and the CLI-path custody proof for a review terminal with no
attempt records collapses to a single `flock` probe on `review.Dir` — not the
five-component disproof the docs claim. The status table in `docs/workflows.md:24`
and the roadmap bullet at `docs/workflows.md:370` both list "descendant absence"
and "runner residue" as machine-enforced components without qualifying that they
only carry information inside a live `tick` process.

Suggested fix: treat missing attempt evidence as `incomplete_evidence` rather than
as clean — require at least one attempt record, and require the newest committed
terminal transition to name an attempt whose record is present, before any
`admissible_review` receipt can form. Separately, either give the descendant and
residue probes a durable backing or downgrade the documentation claim to name the
two probes that survive a process boundary. Note that the packet's own green
baselines mostly ride the zero-attempt path (`runWorkflowToReview` writes no
attempt records), so no current fixture pins this requirement.

### P2-3 — fractional-second width inverts the same comparison

Same root cause and same fix as P1-1, recorded separately because it needs no
timezone difference. `RFC3339Nano` trims trailing zeros, so a stamp that is a
string prefix of a later one compares as larger:

```
terminal  attempt CreatedAt = 2026-08-24T09:00:00.5Z
successor attempt CreatedAt = 2026-08-24T09:00:00.5001Z   (100us later)
reviewCustodyDrift(...) = ""
```

### P2-4 — the quiet window is sampled at two endpoints, not observed throughout

`observeReviewCustody` compares only `first_observed_at` against `now` and the
stored hash against the current hash. Nothing requires the intervening period to
have been observed, so a single ingest, an arbitrary gap, and one more ingest
satisfy the window; evidence that churned and was restored in between, or a
producer that was alive for most of the interval and gone at the second sample,
passes. `docs/workflows.md:332` states the hash must be stable *within* the
20-second window, which is stronger than what two samples establish. Either
require a minimum number of observations spanning the window or soften the claim
to the sampled semantics the code implements.

### P2-5 — no monotonic guard on the durable window timestamp

`first_observed_at` is self-written, stored as wall-clock text, and trusted on
reload. A forward clock adjustment completes the window early; a backward one
leaves it permanently open (fail-closed, so only the forward direction matters).
Consider storing a monotonic-safe duration alongside the stamp, or at least
rejecting a `first_observed_at` in the future.

### P2-6 — the dry-run receipt exercises none of the code under review

The dry run refuses release at `missing_review_task`, i.e. the gate short-circuits
before reaching the verdict or custody checks, and its own transcript records
`(control/custody not created)`. The receipt is a sound offline-orchestration
proof, but it shares only the binary with W2 and provides no coverage of the
custody validator. The label "三槽队列 / three-slot queue" is also stronger than
the evidence: the transcript shows `max_parallel=3` plus three enqueued disjoint
lanes and explicitly never runs a `tick`, so three-way parallel dispatch is not
demonstrated. Likewise "writer/reviewer separation" is proven as role-duplication
and ordering refusals (`duplicate active role`, `freeze a candidate before
review`), not as the writer≠reviewer identity check in `integrationCustodyOK`
— that one is covered by unit tests instead.

### P2-7 — the receipt's binary digest is not third-party reproducible

The receipt pins `508a6900910626b02e10fc0fdf60ec0aa5b3d36ebf44dc04e658619aa88b0ae3`
for the built binary. Rebuilding the identical tree gives a stable but different
digest — `7405531887…` under `go1.24.6` and `cb11c08fd5…` under `go1.25.9` — because
the build is not normalised: no `-trimpath`, no `-buildvcs=false`, and the
toolchain patch version is unpinned (the embedded `export PATH=/usr/local/go/bin`
does not resolve on this machine, so the actual toolchain used is not recorded).
The receipt's own claim is only self-reproducibility within one environment, which
does hold, but the digest anchors nothing for an independent reviewer. Adding
`-trimpath -buildvcs=false` and recording `go version` would make it verifiable.

### P2-8 — `observeReviewCustody` does load-modify-write without the task control lock

Every other durable control record in this tree is mutated under
`withTaskControlLock`. Two concurrent `ingest-review` invocations on the same
review can interleave load and write here. The worst observed outcome is a lost
window reset rather than a spurious receipt, since each caller re-derives drift
itself, but the asymmetry with the surrounding code is worth closing.

### P2-9 — the producerGone disproof duplicates the existing helper

`producerGone(t, rec)` at `attempt_control.go:1207` already composes exactly the
same four probes. `reviewCustodyDrift` re-implements the composition inline to
attach per-component reasons and to iterate all attempts. That is a reasonable
motivation, but the two copies can now drift apart; consider expressing one in
terms of the other.

## Remaining gates

- P1-1 and P1-2 must be closed, and each needs a fixture that fails before the fix.
- W2 has no end-to-end evidence outside the Go test package. A dry run that
  actually reaches `custody_quiet_window` and `admissible_review` through the CLI
  (`ingest-review`, wait, `ingest-review`, `release`) would close the
  `evidence_complete` gap left by P2-6.
- The three pre-existing `make test` failures (scenarios 19, 22, 27) remain open on
  `main` and are out of scope for this packet.
- Integration remains held. This review is not an integration decision; `live` and
  `cutover` stay independently held gates and nothing here authorises release,
  install, or launchd changes.

## Boundary

Read-only review. No candidate file was modified: `go fmt ./...` was invoked once
against the frozen worktree, immediately reverted with `git checkout -- .`, and the
tree hash re-confirmed as `6467e634…`. The three reproduction probes live in a
throwaway copy outside the repository and are not proposed for the tree. No PR was
created, commented on, or merged; no branch other than this review branch was
written.
