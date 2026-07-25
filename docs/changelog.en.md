# ClaudeGo changelog

[中文](changelog.md) | **English** · back to [README](../README.en.md)

## 2026-07-26 · Board display-semantics fixes

Four fixes for readings that were **distorted in a specific direction** — none of them arithmetic bugs,
all of them "correct but reads optimistic / is awkward to use":

- **Quota now reports what's left**: the headline reading in the top quota strip and the burndown page
  flips from "used %" to "remaining %" (new `BurnSource.remaining_percent`, computed server-side and
  clamped to [0,100] — real samples above 100 exist, and without clamping you get a negative remainder
  that renders as a zero-width bar and reads as "just exhausted" rather than "over budget").
  The burndown curve becomes an actual burndown (descending, hitting bottom = exhausted; the projection
  line now terminates at zero remaining) and the y-axis is labelled `剩余%`. `used_percent` is
  **preserved verbatim** in the response and shown alongside in tooltips, subtitles and the sample table.
- **Progress split by kind of work (`Project.kinds`)**: one total bar averages design/impl/fix/review
  work **weighted by card count**, and since review+fix cards routinely make up 70% of cards with a
  naturally high completion rate, the total gets dragged to ~90% while impl may sit at 40%.
  Five buckets (design / impl / fix / review / coord) now each report their own completion using the
  identical formula; the total bar is kept as the comparable anchor. Classification prefers structural
  signals (`review_of` / `fix_round` / `type`) over keywords, and every card carries `kind_source`.
  **Review is decided before fix** — review cards inherit the reviewed card's `fix_round`, and the wrong
  order silently moves hundreds of review cards into the fix bucket. Unclassifiable cards fall into
  "impl", not "unclassified" (the distortion being prevented is *underestimating* remaining work).
  `board.json` gains `kind_rules` as a precise manual escape hatch; invalid rules are skipped one by one
  and reported via `kind_rule_error`.
- **Manual project archiving**: an "Archive" button on the project card and project page collapses
  long-finished projects off the overview. State lives in `~/.claudego/board_archive.json` — **not a
  single byte of any task card changes**, and scheduling and status counts are unaffected. A new card
  (higher count, or a newer `created_at`) restores the project to active with the reason shown;
  **card status changes do not trigger restoration** (otherwise archiving a running project bounces
  back on the next tick). Restoration is a read-only derivation, never written back, so GET stays
  write-free. `POST /api/project/archive` is the board's only write endpoint, behind three gates:
  POST only / `Content-Type: application/json` / same-origin `Origin` host. A corrupted state file is
  surfaced as an error and further writes are refused.
- **Overview becomes a horizontal rail**: one column per project, scroll sideways to switch, with all
  vertical space given to the phase/task list inside a column (independent scrolling; column height is
  derived from the rail's real position so there are never two nested scrollbars). Projects are parallel,
  not sequential — stacking them vertically pushes the second project behind the first one's several
  hundred cards. Narrow screens (≤720 px) fall back to vertical stacking.

## 2026-07 (v0.10 line — 56 commits since the first public push)

Main changes since the last public push (PR #1, review divert), grouped by theme:

**Observability — from "infer from state" to "the event ledger is the truth"**

- **Event ledger (CG-2)**: new per-task `events.jsonl`; every state transition funnels through a single writer
  (no new writers introduced) and the ledger is carried along on archival.
  The board's activity stream now reads the event stream instead of **reconstructing history from current state**
  (reconstruction fabricates timelines that never happened); ledger gaps (crash tails, pre-ledger tasks)
  are **disclosed explicitly** in the UI rather than silently filled in.
- **Idempotent tombstones (CG-4)**: per-task `tombstones/<id>.json` makes resume / harvest / release injection
  **at-most-once**, with `bound=2` stopping crash-restart storms; a per-task tombstone lock plus a two-phase
  critical section, and `emit` before `saveTask`, close the "zero disclosure" window.
- **Web board (`claudego board`)**: read-only board — project / phase / task overview + kanban + quota burndown,
  model-tier colors, phase intros outside the fold, everything expanded by default; the static `board.json`
  snapshot became mechanized goal-anchored progress (CG-8), with `goal_source` tagged by what actually landed,
  non-finite guards in the synthesis layer, and out-of-range manual `done_percent` judged "insufficient data"
  instead of rendering a negative percentage.

**Steady-state operation — failures no longer turn into silent deadlock**

- **Failure classification triage (CG-3)**: auth/permission failures go straight to `held` (retrying only burns quota),
  over-long input straight to `failed`, unknown classes fall back to `retry_backoff`; `classifyFailure`
  **reads only the result msg and refuses transcript contamination**, and `isLimitHit` converges three ways
  so a self-review repo's transcript can't false-positive.
- **In-drain patrol (CG-5)**: independent patrol during drain, **two independent signals** required to declare a hang,
  and a marker file witnessing the real exit code; the heartbeat no longer triggers on its own and its
  threshold scales with config.
- **Single-instance lock**: `acquireLock` / `acquireEventLock` claim atomically to prevent double-held locks.
- **Codex reliability**: harvest-when-the-result-is-in-hand (when the remote result has already come back but ssh
  is held open by a grandchild process, kill in two beats instead of hanging to the timeout); limit detection
  now covers the "session limit" wording and **cross-day reset** parsing; a round-limit escalation card inherits
  the reviewed card's `remote_host` (otherwise a remote chain's escalation card gets dispatched locally and fails to cd).

**Quota — a third usage source and percent-domain lockdown**

- **Direct subscription-usage endpoint read (CG-1)**: with `oauth_usage` on, read `api.anthropic.com/api/oauth/usage`
  as a third usage source and **take the most conservative value when the three sources disagree**;
  only the response body is trusted — **response headers are explicitly not parsed** (easily forged by middleboxes).
- **Percent-domain semantics (CG-1b)**: `utilization` / `used_percent` / `percent` are all taken as
  **0-100 percent, truncated as-is**; any auto-normalization is a breeding ground for false redline hits.
  A value in `(0,1]` is judged a **scale ambiguity** and refused as "insufficient data"; `>100` likewise.
- **Credential isolation**: when `oauth_usage_creds_path` is non-empty, only that file is read — no fallback
  to `~/.claude` or the keychain.

**Review divert — mirror trustworthiness and sandbox narrowing**

- **review-sync worktree root fix (CG-R1/CG-R2)**: sync now also ships the **uncommitted surface** and writes a
  fingerprint, so the self-gate blocks "stale mirror" no-ops (previously the remote would happily produce a
  normal-looking review against an outdated mirror).
- **Codex review sandbox (CG-R3/CG-R3b)**: read-only analysis cards now build a **one-shot isolated copy +
  `--sandbox workspace-write`** by default, so reviews can run tests and drop fixtures for dynamic verification;
  the copy is created and torn down with the card and the source repo is never write-polluted.
  **Remote** only relaxes for mirror cards under `remote_mirror_root` (the prefix test now goes through
  `path.Clean` lexical reduction, closing `..` / `.` escapes); real business repos keep the `read-only` hard guarantee.
  An unrecognized `codex_review_sandbox` value **falls back to `readonly` (fail-closed)**.
  Copy preparation runs inside its own `min(step_timeout, 10min)` sub-budget (the copy leg checks the budget at every
  file boundary) and **never opens irregular files** (FIFO/socket/device skipped; symlinks copied as links, not followed —
  one untracked link pointing at a pipe with no writer would block `open` forever and wedge the whole lane).

**Other**

- **Cross-verification (`claudego cross`)**: three-card chain of two engines answering independently →
  adversarial cross-check, including a root fix for codex failure reliability.
- **Per-card codex model pinning**: `-codex-model` plus a downgrade-specific `codex_fallback_model`
  (tier parity: opus→terra, never down to sol).
- **Docs and tests**: new `docs/specs/` ecosystem trio (landscape survey / CG cards / positioning and non-goals);
  README config keys are validated by `go test` to prevent doc drift; the mock-claude state-machine acceptance
  test now covers crash tails and injected ledger gaps.
