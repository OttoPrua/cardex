# 2026-08-24 · A4 W2 P1 repair — durable handoff

Repair-writer packet closing the two P1 fail-open seams from the independent
W2 review. Written by the repair writer only; **not self-reviewed** — a fresh
independent re-review is required before any integration decision.

| Item | Value |
|---|---|
| Repair-writer runtime model | `claude-fable-5` (Fable 5). Self-reported from the runtime system configuration; the run-metadata service was not consulted for a thinking-budget suffix, so no stronger slug is claimed. |
| Review authority | `docs/2026-08-24-cardex-a4-w2-independent-review.md` on `origin/cursor/cardex-a4-w2-review-40ce` — REQUEST_CHANGES, P0=0, P1=2, evidence_complete=false |
| Candidate under repair | `f9c0c2aed5418e5a5382f0f9e10e699d40b4ce99` (tree `6467e6341be7ad214767bd9d1dcb1450e2c24708`) |
| Base | `main` `a4a3acf692dd49a589210093047824591d0270b3` |
| **Repair commit (freeze for re-review)** | `e795e898b4e8edc90f9a6cb9e8b51bf446c5d8d0` |
| **Repair tree** | `4475340d0f2892c79eb0a264064daa7d463699ef` |
| Branch | `cursor/cardex-w1w4-first-packet-9d89` (continued; no parallel branch) |
| Toolchain | `GOTOOLCHAIN=go1.24.6` (preinstalled Go 1.22 cannot build the `go 1.24` module; same pin the reviewer used) |

## What was fixed

### P1-1 — attempt/transition ordering is now parsed-time order, fail-closed on unparseable stamps

`successorAttemptDrift` compared `CreatedAt` RFC3339Nano stamps with raw
string `>` in two places: the successor-attempt comparison and the
newest-committed-terminal selection. Both now parse with
`time.Parse(time.RFC3339Nano, …)` (`custodyStamp`) and compare `time.Time`
values; ties on equal instants keep the `TransitionID` tie-break. Any stamp
that fails to parse — on the terminal transition, the bound attempt, or any
other attempt — returns drift instead of being treated as an ordering tie.

### P1-2 — absent attempt evidence fails closed; no `admissible_review` without a bound attempt record

For a review terminal on `done`, `successorAttemptDrift` now requires, before
any receipt can form:

- at least one attempt record on disk;
- a committed terminal transition;
- that transition naming an attempt whose record is present.

Anything less is `custody_drift`: ingest resets the window, no receipt of any
kind forms, and both `evaluateIntegrationRelease` and `cardex release`
refuse. `held`/`canceled` containment terminals keep their previous lenient
shape (containment is manager-driven and only ever yields the non-semantic
`custody_reconciled_held` receipt). The docs
(`docs/workflows.md` / `docs/workflows.en.md` status table, machine-invariant
bullet, and W2 roadmap section) now state the process boundary explicitly:
the descendant and runner-residue probes carry information only inside a live
tick process; the cross-process proofs are the attempt PID/PGID start
identity and the workspace lease (flock) — which is exactly why absent
attempt evidence must never count as a successful disproof.

## RED→GREEN evidence

All four new fixtures were run against the frozen candidate (fix stashed,
`workflow_custody.go` at `f9c0c2ae` content) and failed exactly as the review
predicted, then pass with the fix:

- `TestCustodySuccessorOrderingIsChronologicalNotLexicographic` — mixed UTC
  offsets (`17:00+08:00` terminal vs `09:05Z` successor, 5 real minutes
  later) and fractional-width (`.5Z` vs `.5001Z`) subcases: RED reported no
  drift and the gate admitted; GREEN reports successor drift, ingest records
  `custody_drift`, gate holds.
- `TestCustodyTerminalSelectionOrdersTransitionsByTime` — a stale string-max
  terminal anchor hid a genuine post-terminal redispatch: RED clean, GREEN
  drift.
- `TestCustodyUnparseableStampsFailClosed` — garbage attempt/transition
  stamps: RED clean (string order silently absorbed them), GREEN drift.
- `TestCustodyAbsentAttemptEvidenceFailsClosed` — four absence shapes (zero
  attempt records / no committed terminal transition / transition naming no
  attempt / named record missing) plus a positive control: RED minted a full
  `admissible_review` on the zero-attempt path; GREEN refuses every shape on
  every channel (drift string, ingest, receipt absence, gate, try-release,
  `cardex release`) and the positive control still admits.

Green baselines no longer ride the zero-attempt path: `runWorkflowToReview`
now writes an exited attempt plus a committed done transition naming it
(backdated), and `runWorkflowToBareReview` preserves the evidence-free shape
for the refusal fixtures.

## CLI end-to-end evidence (closes the reviewer's P2-6 / evidence_complete gap)

`test/integration.sh` scenario 35, isolated data root, real runner
(mock claude), zero effects outside the script's `$TMP`:

writer runs via `cardex run` → `freeze-candidate` → reviewer runs via
`cardex run` (real attempt record + committed done transition naming it,
asserted from disk) → `ingest-review` opens the window
(`hold=custody_quiet_window`, no receipt) → `try-release-integration`
**refuses** while the window is open → 21 real seconds elapse (constant not
shrunk, no timestamps backfilled) → second `ingest-review` mints the durable
`admissible_review` receipt (`control/custody/<review>.json`,
`semantic_review=true`, `status=review_passed`) → `try-release-integration`
releases integration to `queued` with live/cutover still held.

Two pre-existing seams the scenario had to route around, disclosed here and
left unfixed (out of the frozen P1 scope):

- **W1 transcript-parse vs prompt echo**: the runner echoes the prompt into
  `logs/<id>.log`, and the default `templates/design-review.md` embeds
  literal `{"verdict":"block",…}` example JSON plus a lone fence marker in
  prose. The gate-side `parseReviewVerdict` over the whole log then resolves
  to the template's `block` example, so a real CLI reviewer using the stock
  template can never produce a machine-`pass` through the W1 gate. The
  scenario installs a minimal example-free review template in its isolated
  root (templates are init-written, operator-configurable data). Recommend a
  follow-up finding for the next review round.
- **Mock session identity**: resetting the mock call counter per run hands
  writer and reviewer the same `session_id`, which `integrationCustodyOK`
  correctly refuses as a reviewer inheriting the writer session — the
  scenario shares one plan/counter so the identities differ. (This is the
  check working as designed, recorded so the next writer does not trip on it.)

## Gates run on the repair commit

| Gate | Result |
|---|---|
| `gofmt -l` on changed Go files | clean (the 7 pre-existing unformatted files on `main` are untouched) |
| `go vet ./...` | clean, exit 0 |
| `go test ./... -count=1` | `ok cardex 161.6s`, exit 0 |
| `make test` | **127 pass, 3 fail** — the same three pre-existing failures (scenarios 19/22/27) the review verified on base `main`; all 10 new scenario-35 assertions pass. Baseline was 117 pass / 3 fail, so the delta is exactly the new scenario. |
| Home hygiene | `~/.cardex` and `~/.claudego` do not exist on this machine after all runs; every write stayed under repo, `/tmp` fixtures, or the test `$TMP`. No `tick`/`install`/`install-launchd`/live/cutover path was invoked outside the mock-driven test scenario. |

## Remaining gates (not this packet's authority)

- Independent re-review of repair commit `e795e898…` (tree `4475340d…`) —
  this handoff is writer-side only; verdict and evidence_complete are the
  re-reviewer's call.
- Reviewer P2s remain open: P2-4 (two-endpoint window sampling), P2-5 (no
  monotonic guard on `first_observed_at`), P2-7 (non-reproducible dry-run
  binary digest), P2-8 (`observeReviewCustody` outside the task control
  lock), P2-9 (producerGone helper duplication), plus the W1 transcript-parse
  seam disclosed above.
- Scenarios 19/22/27 stay open on `main`, out of scope here.
- Integration remains held; `live` and `cutover` stay independently held
  gates. Nothing in this packet authorises release, install, or launchd
  changes. W3/W4 remain roadmap — scope was not expanded.
