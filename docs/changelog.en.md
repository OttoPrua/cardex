# cardex changelog

[中文](changelog.md) | **English** · back to [README](../README.en.md)

## 2026-07-31 · Project renamed ClaudeGo → cardex

- **Rename decision**: the product is renamed from ClaudeGo to cardex (decision BD-44). Command name
  `claudego` → `cardex`; default data directory `~/.claudego` → `~/.cardex`; environment variable
  `CLAUDEGO_ROOT` → `CARDEX_ROOT`.
- **Old name still works**: `make install install-shim` lays down a `claudego → cardex` compatibility
  symlink (transition-period aid, removable once the rename is finished); when `CARDEX_ROOT` is unset,
  the legacy `CLAUDEGO_ROOT` is still read once, with a warning.
- **Data migration**: a new `cardex migrate` subcommand moves the old data root (`tasks`/`events`/
  `progress`/`archive`) to the new root, fail-closed (does nothing at all if any precondition fails)
  with zero-loss reconciliation (per-directory file-count/byte-count tally before and after), then
  prints the remaining manual steps (reinstall the scheduler, old symlink, re-approving TCC/keychain
  access, etc.).
- **Out of scope for this entry**: in-repo artifact/sync-script names such as `.claudego-fingerprint` /
  `.claudego-scripts` stay frozen under the old name (per decision BD-44 §2.2); entries above this one
  are left as originally written, and any `claudego`/`~/.claudego` in them refers to the product/path
  as it was before the rename.

## 2026-07-26 · Token curve follows the window + spend goes per-project

- **The token curve follows the time window**: previously pinned at 24 hours, it now shares the
  window tabs with queue task spend. Scan parameters scale with the window (`tokenScanPlanFor`):
  buckets 15 min → 12 h (30 days would otherwise plot 2880 points), byte budget 512 MB → 4 GB
  (twice the measured volume). `range=all` is capped at 90 days for transcripts and says so in
  `basis` — that directory has no upper bound. Measured cold start: 24h/0.6s · 7d/1.3s · 30d/3.1s,
  no window truncating; the 7-day window surfaces all 7 models. Also switched the per-line prefilter
  from `strings.Contains(string(line),…)` to `bytes.Contains` — the former copies **every line**
  into a fresh string, invisible at 104 MB but a gratuitous 1 GB of allocation at 30 days.
- **Hitting the gate self-reports**: `token_series` gains `truncated` / `files_matched` /
  `files_scanned` / `bytes_scanned`, with a red banner on truncation. A curve missing its back half
  looks exactly like "nothing ran then" — silent truncation is a fabricated reading.
- **`burnCache` is partitioned by window**: the scan grows from 104 MB to 1 GB across windows, and
  a shared slot would re-run the most expensive one on every tab switch. Unknown ranges normalize
  onto the 24h slot so `?range=garbage` cannot blow the cache up.
- **Spend moves from per-task to per-project**: a 30-day window holds close to a thousand cards, a
  per-task table shows only the top few dozen, and those are usually one project's consecutive fix
  chain — you finish reading without knowing which line of work the money went to. Each row carries
  a **"priced / total" card count** (an amount alone cannot distinguish "little work" from "cost
  not recorded"; codex reports none) plus that project's **top model by spend**.

## 2026-07-26 · Queue task spend (windowed + per-task)

**Why**: the "last 24 hours" token curve on the burndown page frequently showed a single model,
which looks like the board dropped data. It is neither a bug nor missing data — the curve scans
transcripts under `~/.claude/projects` with a fixed 24-hour window (30 days of transcripts measures
1.0 GB against a 512 MB scan budget, so widening it would silently truncate, and a "full month"
that read half the data is worse than no such window), and a single day often ran only one or two
models. That directory also interleaves hand-typed Claude Code sessions, so its scope was never
the queue to begin with.

**New `task_spend`** (`/api/burn?range=24h|7d|30d|all`) uses a different source — **the task cards'
own ledger** (`cost_usd` / `turns_used`, written back by the runner when a card finishes). It
persists with the cards (including `archive/`), so any window costs no extra scanning (the snapshot
already holds every card), and it is queue-scoped by construction. It reports total spend / priced
cards / unpriced cards / total turns, a per-model breakdown (resolved via `effectiveModel`), and a
per-task table sorted by cost (capped at 30 rows with the remainder disclosed). The burndown page
gains window tabs, defaulting to 7 days — 24 hours often contains only one or two models, and
landing on a single-model view reads as missing data when the real cause is just a narrow window.

**Two boundaries are written into `basis` and stay on screen**: (1) `cost_usd` is the
**API-equivalent cost** reported by the claude CLI, not an actual charge on a subscription;
(2) codex / remote-codex report no cost (448 of 1423 cards in practice), burn a different quota,
and are excluded from the total — the size of that gap must be visible. Time is filed by each
card's `updated_at` (when it finished) rather than `created_at`: a card queued last week and
finished today spent that money today. An unknown `range` falls back to `24h`; `task_spend` stays
out of `burnCache` (the window comes from a parameter, and mixing them would give every window its
own copy of the expensive transcript scan) and is computed per request. The transcript section also
gained a pinned note explaining the one-or-two-models effect on the spot.

## 2026-07-26 · Drag-reorderable projects + status order as a fill gauge

- **Projects are drag-reorderable**: the grip in each column header drags a project to a new
  position (or focus it and press ←/→ — drag-and-drop is unusable by keyboard, so this is not an
  optional extra; focus returns to the same grip after each move). The order lives in
  localStorage: a viewing preference, not a queue fact. While a manual order is active a banner
  stays pinned with a one-click reset (the default sort carries information: active work first,
  then most recent activity). Projects that appear after you set an order go to the **front**, not
  the end — the rail scrolls horizontally, so the end is several screens away and a brand-new
  project is exactly the one you want to see.
- **Status presentation order is now `canceled → done │ running → queued → limit_paused → held →
  failed`**: the progress bar is a fill gauge (like a battery meter), so the left portion is "the
  part you no longer have to think about", which is what makes "everything unfinished is on the
  right" true. The old order put running/queued at the far left and done second-from-right, which
  read as "the longer the bar, the worse it is" — the opposite of everyone's default expectation
  of a progress bar. Within the right half the order increases by distance from done (the first
  three advance on their own; the last two need a human). Bar segments, legend and header chips
  share one order — they are three renderings of the same reading, and ordering them differently
  would stop the chips lining up with the ribbon beneath them. **Kanban column order does not
  follow**: a workflow board flows left-to-right toward "done"; fill gauge and flow board are
  different metaphors, each keeping its own convention.
- Also fixed: `frag.append(null)` inserts the literal string "null" (the native API does not skip
  empty values the way our own `h()` does — with both optional banners absent the page grew a
  stray "nullnull" line). Conditional blocks now go through a new `appendMaybe()`.

## 2026-07-26 · Viewport-width layout + status filter

- **Page width follows the viewport**: `.app` / `.topbar-inner` were pinned at
  `max-width: 1760px`, which left wide margins on ultrawide displays (2560/3440) and cut the
  project rail short. The rail is precisely the kind of layout where every extra pixel reveals a
  bit more of the next project, so spending that width on margins costs a whole column.
- **Status filter (hides lists, never touches readings)**: the status-count chips in the page
  header became toggles — clicking one collapses that status's card list (task rows on the
  overview; the whole column on the project page, since a kanban column *is* a status and an
  empty-but-present column reads as "no cards in this status"). **Filtering never changes any
  reading**: chip counts, progress bars, the five kind buckets and the ETA are all computed over
  every card. A banner stays pinned while a filter is active, and "the backend only sent 40" vs.
  "I hid some statuses myself" are reported as two separate lines. The filter persists in
  localStorage, made safe by that pinned banner.

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
