# 2026-08-24 · A4 W2 P1 repair — independent re-review

Fresh, read-only re-review of the W2 reviewer-custody repair. Every claim from
the first review round and from the repair writer's handoff was re-verified
from scratch against a frozen worktree; nothing was carried forward on trust.
No candidate file was modified.

| Item | Value |
|---|---|
| Re-reviewer runtime model | `claude-opus-5` (Opus 5). Self-reported from the runtime system identity; no thinking-budget suffix is claimed, because it is not observable from inside the run. |
| Reviewed commit | `e795e898b4e8edc90f9a6cb9e8b51bf446c5d8d0` |
| Reviewed tree | `4475340d0f2892c79eb0a264064daa7d463699ef` (re-verified before and after every gate) |
| Handoff doc commit | `48c0811f5c6c5728cc66676fdb9aaf3dbac1723f` |
| Base | `main` `a4a3acf692dd49a589210093047824591d0270b3` |
| Prior candidate (superseded for code) | `f9c0c2aed5418e5a5382f0f9e10e699d40b4ce99` (tree `6467e6341be7ad214767bd9d1dcb1450e2c24708`) |
| Branch | `cursor/cardex-w1w4-first-packet-9d89` |
| Review authority being answered | `docs/2026-08-24-cardex-a4-w2-independent-review.md` — REQUEST_CHANGES, P0=0, P1=2, evidence_complete=false |
| **Verdict** | **APPROVE** |
| P0 | 0 |
| P1 | 0 |
| P2 | 8 |
| evidence_complete | **true** |

All SHAs in the freeze were confirmed exactly as stated, including the parent
chain `a4a3acf → f9c0c2a → 7590d7f → e795e89 → 48c0811`. The review ran from a
detached `git worktree` pinned to `e795e898…`; the tree hash read
`4475340d0f2892c79eb0a264064daa7d463699ef` before the first gate and again
after the last one.

## Verification performed

Toolchain: `GOTOOLCHAIN=go1.24.6`. The preinstalled Go is 1.22.2, which cannot
build this `go 1.24` module, and the bare `go1.24` directive does not resolve
under `GOTOOLCHAIN=auto`; an explicit patch version is required. This
reproduces the first review's finding and the writer's pin.

| Gate | Result |
|---|---|
| `gofmt -l` on the three changed Go files | clean |
| `gofmt -l .` (whole tree) | 7 files unformatted — `boardgoal.go`, `boardgoal_test.go`, `codex_reliability_test.go`, `events_test.go`, `limit_test.go`, `oauth_usage_test.go`, `reviewsync_workspace_test.go`. Identical to base `main`; the repair neither adds to nor fixes them. |
| `go vet ./...` | clean, exit 0 |
| `go test ./... -count=1` on the repair | `ok cardex 190.170s`, exit 0 |
| `make test` on the repair | **127 pass, 3 fail** |
| `make test` on base `main` `a4a3acf` | **117 pass, 3 fail** |

The failing-assertion text is byte-identical between base and repair after
normalising a card ID and one elapsed-time figure: scenario 19 (default
reasoning-level passthrough), scenario 22 (read-only review blocked by the
directory mutex), scenario 27 (`cancel` output). **No regression**, and the
delta is exactly `+10` passing assertions, which is precisely the count of new
assertions in scenario 35. The `127 = 117 + 10` claim holds.

## P1-1 — CLOSED

`successorAttemptDrift` no longer compares RFC3339Nano stamps as strings. The
new `custodyStamp` helper parses with `time.Parse(time.RFC3339Nano, …)` and
every ordering decision now runs on `time.Time`: the newest-committed-terminal
selection (`at.After(termAt)`, with the `TransitionID` tie-break retained for
equal instants), the bound terminal attempt, and each candidate successor. An
unparseable stamp on the terminal transition, the bound attempt, or any other
attempt returns drift rather than silently becoming an ordering tie.

Verified by reproduction, not by reading. I wrote four adversarial fixtures of
my own from the first review's exploit descriptions — deliberately not reusing
the writer's — and ran them against a copy of the repair tree with **only**
`workflow_custody.go` reverted to its `7590d7f` content (byte-verified
identical to the pre-repair file). All four are genuinely RED, and reproduce
the first review's numbers exactly:

```
RED (pre-repair validator)
  mixed UTC offsets (17:00+08:00 terminal vs 09:05Z successor, 5 real minutes later)
    reviewCustodyDrift = ""
    admissible=true holdReason="" releaseAdmit=true releaseReason=""
  fractional width (.5Z terminal vs .5001Z successor, 100us later)
    reviewCustodyDrift = ""
  stale string-max terminal anchor hiding a post-terminal redispatch
    reviewCustodyDrift = ""

GREEN (repair validator, same fixtures)
  reviewCustodyDrift = "successor attempt rp1-successor was minted after
                        terminal attempt rp1-terminal: same-card redispatch"
    admissible=false holdReason="custody_drift"
    releaseAdmit=false releaseReason="custody_drift"
  ... and the fractional-width and stale-anchor cases likewise report successor drift
```

My fixtures assert on the end-to-end outcome — `ingestWorkflowReview`
adoption and `evaluateIntegrationRelease` admission — not on drift strings, so
they could not have been satisfied by a message change.

I also confirmed the writer's own four fixtures are RED against the pre-repair
validator rather than trivially green:
`TestCustodySuccessorOrderingIsChronologicalNotLexicographic` (both subcases),
`TestCustodyTerminalSelectionOrdersTransitionsByTime`,
`TestCustodyUnparseableStampsFailClosed` (both subcases), and
`TestCustodyAbsentAttemptEvidenceFailsClosed` (three of four subcases; the
`named attempt record missing` shape was already refused before the repair,
and the positive control passes on both sides, as it should).

Separately, I checked whether a redispatch could evade the successor scan by
reusing an attempt ID instead of minting a successor.
`reserveDispatchAttempt` (`attempt_control.go:976-978`) always calls
`newAttemptID()` and stamps a fresh `time.Now()`, closing off the last
inconsistent-ordering route I could construct.

## P1-2 — CLOSED

For a review terminal on `done`, `successorAttemptDrift` now requires at least
one attempt record, a committed terminal transition, and that the transition
name an attempt whose record is present on disk. Anything less is
`custody_drift`.

The first review's exploit is gone. Same fixture, both validators:

```
RED (pre-repair)   attempt records = 0; committed terminal transitions naming an attempt = 0
                   receipt kind="admissible_review" semantic=true drift=""
                   admissible=true hold="" releaseAdmit=true reason=""

GREEN (repair)     attempt records = 0; committed terminal transitions naming an attempt = 0
                   receipt kind="" semantic=false
                   drift="no attempt records exist for this review terminal:
                          absent evidence is incomplete, not proven producerGone"
                   admissible=false hold="custody_drift"
                   releaseAdmit=false reason="custody_drift"
```

Two observations across a backdated full window still mint no receipt of any
kind, because drift resets the window on every ingest.

The second half of P1-2 — the documentation overclaim — is also answered. I
re-confirmed the underlying fact independently: `anyTaskProcAlive`
(`proc.go:200`) and `taskProcessResidue` (`proc.go:156`) read the package-level
`taskPG` / `taskPGResidue` / `taskLeaseResidue` maps, which only `runTask`
populates in the same process, so both are structurally `false` in a
short-lived `ingest-review` or `release` invocation; `attemptProducerAlive`
(`attempt_control.go:1132`) reads the durable record's PID/PGID start identity
and is genuinely cross-process. The status table, the machine-invariant
bullet, and the W2 roadmap section in both `docs/workflows.md` and
`docs/workflows.en.md` now state that process boundary explicitly and name the
two probes that survive it. Requiring attempt evidence is what gives the
CLI-path proof a second real cross-process leg instead of collapsing to a lone
`flock` check, so the fix and the doc change are answering the same finding
rather than papering over it.

Scope note, not a finding: the completeness requirement is gated on
`statusDone` only. `held`/`canceled` keep the lenient shape, but they can only
ever produce the non-semantic `custody_reconciled_held` receipt, which
`reviewCustodyReceiptReason` rejects with `custody_receipt_not_semantic`;
`statusFailed` reaches no receipt branch at all. Neither is a release path.

## CLI end-to-end — verified, and it closes `evidence_complete`

`test/integration.sh` scenario 35 was the first review's missing gate: W2 had
no evidence outside the Go test package. It now exists and it passes on the
repair, all ten assertions:

```
== 场景35: workflow W2 custody CLI 端到端 ==
  ✔ workflow writer 由真实 runner 跑完
  ✔ workflow reviewer 由真实 runner 跑完
  ✔ reviewer 终局带真实 attempt+committed transition 证据
  ✔ 首次 ingest 只开 quiet window（hold=custody_quiet_window）
  ✔ 窗口未满前不产生任何收据
  ✔ quiet window 未满时 release 被拒
  ✔ 窗口满后第二次 ingest 采信 pass
  ✔ 耐久收据 kind=admissible_review 已落盘
  ✔ 集成卡释放为 queued
  ✔ 只放 integration；live/cutover 仍 held
```

This is the shape the first review asked for: a real runner produces the
reviewer terminal, the attempt record and the committed `done` transition
naming it are asserted from disk, the 20-second window elapses in real
wall-clock time (`sleep 21`; the constant is not shrunk and no timestamp is
backfilled), release is refused mid-window and admitted only after the durable
`admissible_review` receipt exists, and `live`/`cutover` stay held. The
scenario uses an isolated data root and the mock `claude`; `~/.cardex` and
`~/.hermes` do not exist on this machine after every gate was run.

With per-P1 RED fixtures and this end-to-end path both present and
independently reproduced, `evidence_complete` is **true**.

## P2 findings (8 open, none blocking)

Five carry over from the first review unchanged, and I re-confirmed each is
still open in the repaired file:

- **P2-4** — the quiet window is sampled at two endpoints, not observed
  throughout. `observeReviewCustody` still compares only `first_observed_at`
  against `now` plus the stored hash, so one ingest, an arbitrary gap, and a
  second ingest satisfy it.
- **P2-5** — no monotonic guard on `first_observed_at`. It is self-written
  wall-clock text and trusted on reload; a forward clock adjustment completes
  the window early.
- **P2-7** — the A4 dry-run receipt's binary digest is still not
  third-party reproducible (no `-trimpath`, no `-buildvcs=false`, toolchain
  patch version unrecorded).
- **P2-8** — `observeReviewCustody` still does load-modify-write outside
  `withTaskControlLock`, unlike every other durable control record here.
- **P2-9** — `reviewCustodyDrift` still re-implements the composition that
  `producerGone` (`attempt_control.go:1207`) already performs.

Three are new this round:

- **P2-10 — the stock `design-review` template poisons W1 transcript parsing.**
  The repair writer disclosed this and I verified it independently:
  `templates/design-review.md` embeds three literal `{"verdict":"block",…}`
  examples (lines 24, 35, 38) and contains an odd number of fence markers, so
  the prompt the runner echoes into `logs/<id>.log` carries parseable verdict
  JSON. `parseReviewVerdict` (`runner.go:3476`) scans fenced blocks
  last-to-first and then falls back to a raw `"verdict"` scan, and resolves to
  the template's example. Scenario 35 routes around it by installing a
  minimal example-free template in its isolated root. The direction is
  fail-closed — the template only contains `block` examples, and its
  `"pass|concerns|block"` schema line is not a valid verdict value, so the
  pollution can never manufacture a `pass` — which is why this is P2 and not
  P1. But it does mean a real CLI reviewer on the stock template cannot reach
  a machine `pass` through the W1 gate, and scenario 35 therefore does not
  cover the shipped template path.
- **P2-11 — `listTaskTransitions` silently drops unreadable records.**
  `attempt_control.go:932` does `if err != nil || rec == nil { continue }`,
  which contradicts the principle `listReviewAttempts` states and enforces
  three files away ("an unreadable record is an error, not an absence"). I
  traced the consequences in this path and they are conservative: if the only
  terminal transition is unreadable the `statusDone` completeness check fires,
  and if a newer one is dropped the scan anchors at an older terminal, which
  makes successor detection stricter, not looser. So there is no fail-open
  today — but the asymmetry is unpinned by any fixture and would become one if
  the anchor logic were ever inverted.
- **P2-12 — equal-instant successors are not detected.** The successor scan
  uses `at.After(boundAt)`, so an attempt stamped at the exact same instant as
  the terminal attempt is not drift. I could not construct a reachable exploit
  (`reserveDispatchAttempt` always takes a fresh `time.Now()`, and two
  distinct attempts sharing a nanosecond is not practically attainable), so
  this is hardening only: a tie between two distinct attempt IDs is at best
  unproven ordering and could fail closed.

Two of the first review's P2s are resolved: P2-3 (fractional-second width) was
fixed as part of P1-1 and I verified it RED→GREEN above, and P2-6 (the dry-run
receipt exercising none of the code under review) is closed by scenario 35.

## Fixture-quality note

The repair moved every non-custody workflow fixture off the zero-attempt path:
`runWorkflowToReview` now writes a backdated exited attempt plus a committed
`done` transition naming it, and `runWorkflowToBareReview` preserves the
evidence-free shape for the refusal fixtures. That is the right split — the
first review's observation that "the packet's own green baselines mostly ride
the zero-attempt path" no longer holds.

One consequential side effect, checked and accepted: in
`TestCustodyQuietWindowResetsWheneverEvidenceMoves` the late-surfacing
`at-late` record had to be backdated two hours, because at real `time.Now()`
it would now be redispatch drift rather than mid-window churn. The assertion
it carries (a hash move restarts the window) is preserved, and the drift
reading it would otherwise produce is pinned by its own fixtures, so this is a
re-partition of coverage rather than a weakening. Both outcomes are refusals.

## Handoff accuracy

The writer's handoff (`docs/2026-08-24-cardex-a4-w2-p1-repair.md`) was checked
claim by claim and I found no overstatement. Every gate result, the RED→GREEN
narrative, the `127 = 117 + 10` arithmetic, the disclosed W1 template seam, and
the home-hygiene claim reproduce. The only divergence is a timing figure —
`go test ./...` took 190.2s here against the reported 161.6s — which is
machine variance, not a discrepancy. The handoff also correctly declines to
self-review and correctly lists the open P2s.

## Remaining gates

- Both prior P1s are closed and `evidence_complete` is true, so this packet
  clears code review. **Integration itself remains a separate decision and is
  still held**; this review authorises nothing beyond the verdict.
- `live` and `cutover` remain independently held gates with no release path in
  this tree. Nothing here authorises release, `install`, or `launchd` changes.
- The eight P2s above are open follow-ups. P2-10 is the one worth scheduling
  first, because it makes the stock reviewer template unusable through the W1
  gate even though it fails in the safe direction.
- Scenarios 19, 22 and 27 remain failing on base `main` and are out of scope
  for this packet.
- W3 and W4 remain unlanded roadmap items; the repair did not expand scope.

## Boundary

Read-only review. The repair worktree ended with tree hash
`4475340d0f2892c79eb0a264064daa7d463699ef`, `git diff` against
`e795e898…` empty, and zero tracked modifications — the only untracked path
was the gitignored `bin/` build output from `make test`. My four probe
fixtures and the reverted-validator copy live in throwaway directories outside
the repository and are not proposed for the tree. `~/.cardex` and `~/.hermes`
do not exist on this machine. No `tick`, `install`, `install-launchd`, live, or
cutover path was invoked outside the mock-driven test scenarios. No PR was
created, commented on, or merged, and nothing was merged into any branch; the
only branch written is this review branch.
