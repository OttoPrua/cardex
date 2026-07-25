# ClaudeGo runtime internals

[中文](internals.md) | **English** · back to [README](../README.en.md)

The scheduler's behavioral contracts on the failure paths: dispatch order, limit recovery, failure classification, stall patrol, the event ledger, idempotent tombstones, and permission boundaries.

## Dispatch rules (tunable in config.json)

1. **Resume first** (`resume_first`): tasks interrupted by a limit run before new ones — finish the unfinished first;
2. **Higher priority wins**;
3. **Type order** (`type_order`): default `progress-pull > coordinate > review > sequence > assembly` (review is cheap and returns feedback fast; assembly spawns new work so it goes last);
4. FIFO within the same tier.

Limits are global: whenever any task hits a limit, a global cooldown is written (`cooldown.json`); during it no task is dispatched and no probe calls are wasted. The cooldown time prefers the reset timestamp in the error message; if none can be parsed it falls back to retrying after `limit_fallback_min` minutes.

## Limit interruption and auto-recovery details

- Hitting a limit mid-step: the task is marked `limit_paused` and records `mid_step`. On resume it doesn't replay the original prompt — it sends a resume prompt (config.json's `resume_prompt`) into the **same session** so Claude continues from where it stopped, avoiding duplicate work.
- Every step is written to disk the moment it succeeds (the task file is written atomically), so no progress is lost even if the process is killed.
- A single-instance lock (`.lock`) keeps launchd's repeated triggers from running tasks concurrently; the lock is cleared automatically if the holding process dies.
- Other errors (network, timeout, etc.) back off and retry per `retry_backoff_min`; past `max_attempts_per_step` the task is marked failed, and `claudego retry <id>` re-enqueues it with session and progress intact.

## Failure classification triage (CG-3)

A single `retry_backoff` still burns `max_attempts_per_step` on obviously non-retryable errors (expired
credentials / permission denied / prompt too long) — i.e. spending subscription quota on retries that are
guaranteed to fail. CG-3 introduces a finite-enum failure classifier; each class maps to an independent policy:

| Class | Detector | Policy | Event |
|---|---|---|---|
| `auth` | 401 / invalid api key / oauth expired / please re-login | Directly `held`, **does NOT burn attempts**; wait for human `release` after relogin | `evHeld` actor=`runner:classifier` |
| `permission` | 403 / policy denied / blocked by policy/admin | Directly `held`, no attempts burnt; waits for policy/permission adjustment | `evHeld` |
| `input_too_long` | prompt too long / context length exceeded | Directly `failed`, no attempts burnt (same prompt will overflow again) | `evFailed` |
| `timeout` | `步骤超时(N 分钟)` / context deadline exceeded | Falls back to `retry_backoff` (retryable; class only used for audit rollups) | `evRetry`/`evFailed` |
| `executor_crash` | signal killed/aborted / executable not found | Falls back to `retry_backoff` | `evRetry`/`evFailed` |
| `unknown` | Everything the classifier can't identify | **Fallback to existing `retry_backoff`**, behavior byte-identical to the pre-CG-3 code (no class prefix) | `evRetry`/`evFailed` |

**Limits and the classifier are strictly disjoint**: `usage limit / limit reached / hit your limit /
session limit` is owned by `isLimitHit` (writes global cooldown + `limit_paused`); the classifier NEVER
takes the `limit` class. Counter-example injection is the core acceptance test: text that looks limit-like
but has no `limitRe` signature (e.g. `quota nearly consumed with 5% remaining`) **must** fall through to
`unknown` and **must not** write the global cooldown — a written `cooldown.json` immediately fails the test.

**Unknown-class fallback discipline**: the classifier is an "overreach burns quota" component — detectors
use strict enum regex against explicit server/executor phrasing (broad terms like `error`/`failed` are
banned); anything unclassified falls to `unknown` and takes the existing `retry_backoff` path. This is
the hard boundary that "the new classifier must not change validated behavior"; the regression-baseline
assertion (`TestRunTaskUnknownClass_RegressionBaseline`) pins it down.

**Event ledger**: the classification lands in `events.jsonl` `detail.failure_class`; combined with the
CG-2 event stream you can roll up "which class is burning attempts" and "how many of the `held` cards
are auth failures". `auth`/`permission`/`input_too_long` tasks carry `[<class>]` prefix in `LastError`
so CLI/board can identify them at a glance; `unknown` intentionally keeps the raw message to preserve
the regression baseline.

**Transcript-derived signals never land terminal (Round-3 audit P1)**: `classifyFailure` consuming only
`errorSummary`'s `msg` is the **first line of defense**, but `msg` may itself be a transcript-picked
line — `invokeCodex`/`invokeRemoteCodex` route `codexErrorLine`'s hit (which matches naked
`401 unauthorized`/`403 forbidden`/`invalid api key` etc.) into `res.Result`, and `invokeRemoteClaude`'s
`parseClaudeJSON=nil` fallback takes `firstLine(combined)`. Those quoted lines flow through `msg` and
get classified as auth/permission → held, silently parking retries that would have self-healed under
back-off. **Second line of defense**: `claudeResult` grew a `ResultFromTranscript` flag; `runTask` calls
`classificationFromTranscript(res, runErr)` after classifying — if true, terminal classes are **softened**
back to `retry_backoff` (`failure_class` still recorded for audit, plus
`detail.softened_from_terminal=true` and `reason=softened_transcript_derived`). `LastError` reverts to
the unprefixed form, byte-identical to the legacy retry path. Regression tests
`TestRunTaskCodexTranscriptDerivedAuth_SoftenedToRetry` and `..._Permission_..._SoftenedToRetry` pin
the softening; the reverse control `TestRunTaskClaudeStructuredAuth_StillHeld` proves genuine 401 from
claude's structured JSON (with `ResultFromTranscript=false`) still lands `held` — no false-positive
softening.

**Not doing**: no automatic replan/decompose — a CAMEL-style retry→decompose→replan is validated in
other work but ClaudeGo's `held`-to-human path is sufficient today; automatic replan on misclassification
means "the AI silently rewrote the task", which conflicts with the "complete task lineage must be
auditable" honesty discipline. If it's ever needed, a separate card.

## In-drain patrol + review-sync race root fix (CG-5)

Two independent stall-detection signals piggyback on the existing drain loop (no new daemon), covering
the "invisible" hang axis that `harvest`'s early reap (visible-completion axis) can't address.

**In-drain patrol**——`tick`'s cancel-reconciliation already rescans every `drain_rescan_sec` (default
15s); `patrolOnce` rides the same tick. For each running card it checks two signals:
- **Procgroup liveness**: `taskPG` registry + `processAlive(pid)` double-check (stale dead-pid entries
  in the registry can't fake liveness — the double-check is the counter-example defense).
- **Heartbeat**: whether the task log `~/.claudego/logs/<id>.log` file size is growing (only crosses
  step boundaries — within a single step nothing appends).

Verdict (CG-5 R2.2 revision, single strict trigger face): **only** `pgDeadTooLong = pgSeenAlive && !alive
&& dead-since ≥ patrolPGGrace` (default 60s) — patrol only handles "executor procgroup is dead-too-long"
shutdown-hangs. Heartbeat (log-file-size stale ≥ `patrolHeartbeatTimeout`) is **no longer an independent
trigger**; it only upgrades the reason field from `procgroup_dead` to `procgroup_dead_and_no_heartbeat`
once `pgDeadTooLong` already holds. Rationale: runner.go collects stdout into an in-memory buffer,
`logBlock` only appends after `invoke` returns, so task logs have **zero growth during a single step**;
`step_timeout_min`'s default 60min covers healthy opus heavy cards running 30+ min single-steps that
would be permanently canceled-and-archived under any heartbeat-only trigger. R2.1 used
"heartbeat-stale + `pgDead` (raw)" as an independent trigger channel, which could **bypass pgGrace**
and fire early (in step_timeout≥70min configurations, during the WaitDelay 10s window after
step_timeout kill or the invoke-swap window between steps). R2.2 narrows the trigger face to
`pgDeadTooLong` alone to eliminate this corridor. "Alive but silent" is delegated to `invoke`'s
built-in `WithTimeout(step_timeout_min)`; patrol no longer touches that axis. Procgroup liveness
remains the **sole authorization credential**: adversarial injection (script fakes heartbeat while
the real executor is dead) is still caught by `pgDeadTooLong`. Startup protection: `pgSeenAlive`
gate excludes false positives when `invoke` hasn't reached `cmd.Start` yet.

`patrolHeartbeatTimeout` scales with `cfg.StepTimeoutMin` — `tick` entry sets it to
`max(70min, step_timeout_min + 10min)` (default 60min → 70min; production has run at ~150min
step_timeout → 160min). A hardcoded 70min would misclassify long-step tasks as `no_heartbeat`
right after they die and cross pgGrace, polluting the audit view. After scaling, the threshold
is always ≥ step_timeout + 10min.

**Patrol compatibility for review-sync in postComplete** (CG-5 R2 P1-1 addendum): `runReviewSync` runs
inside `postComplete` while the task is still in `activeIDs`, so the sync pid **must** register into
`taskPG` too (paired `registerTaskInvoke`/`unregisterTaskInvoke`). Otherwise `anyTaskProcAlive` returns
false → `pgSeenAlive && !alive` timer starts → any sync running longer than `patrolPGGrace` gets
misclassified as `procgroup_dead`, emitting a fake `evStalled` (status=`running` while sync is in
progress) and firing a no-op cancel against an already-done task — the event chain becomes
`done→stalled with no canceled`, violating the `dispatched→stalled→canceled` causal contract.

On match: **emit `evStalled` first, then `cancel`**. `evStalled` is a "disclosure of stall verdict"
diagnostic event (status stays `running`); the subsequent `cancelRun()` goes through the same shutdown
pipeline (`cmd.Cancel = killProcGroup` → `ctx.Err()` → `finalizeCanceled` → emit `evCanceled`) — no
second kill path is introduced. The event sequence `dispatched → stalled(diagnostic) → canceled(shutdown)`
preserves the full causal chain. `patrolEventCooldown` (default 5min) suppresses `evStalled` noise
when the cancel signal is momentarily delayed.

**Review-sync race root fix** (`runReviewSync` marker file)——the old path was "recorded but not fixed":
when sync completes at ~110s+ and grandchildren (ssh mux from rsync-over-ssh, etc.) hold the stdout
pipe, `WaitDelay`'s 10s finalization crosses the 120s deadline; a successful sync is misreported as
timed out via `ctx.Err()=DeadlineExceeded` → divert is torn down → every round falls back to local
review, silently defeating the divert feature. Window is narrow but real — long ran with the bug
under "fallback is harmless" hand-wave.

Root fix: wrap the user command and write a marker file that witnesses the real exit code. When the
marker exists → decision is 100% by the marker's recorded exit code; `ctx.Err()`/`cmd.Wait` errors
become derivatives of pipe behavior, no longer authoritative. On seeing the marker, immediately kill
the process group so `cmd.Wait` returns quickly instead of waiting for `WaitDelay`/ctx. The user
command runs inside a `( ... )` subshell so an explicit `exit N` in the user command exits only the
subshell, letting the outer shell still reach the marker-writing line. **The closing `)` must live on
its own line** (CG-5 R2 P1-2 addendum) — if collapsed inline as `( <cmd> )`, a user command ending in
a `#` trailing comment (`rsync ... # notes`) or containing a heredoc lets the `#` swallow the `)` →
sh syntax error exit 2 → marker never written → every sync fails → divert silently falls back to local
review forever, i.e., the very failure this card was created to fix reappears in a different form.
`rescueWaitDelay` stays as a second-line defense for corner cases (marker not written as expected).

## Event ledger (per-task `events.jsonl`)

The board's activity stream is driven by each card's event ledger; it no longer forges history by
inferring from `task.Status` (which flattens the real `queued→running→limit_paused→running→done`
trajectory into a single "currently running", contradicting the board's honesty-first discipline).

- Location: `~/.claudego/events/<id>.jsonl` while the card is live; `clean` moves it to
  `archive/events/<id>.jsonl` alongside the archived card.
- Each event is one JSON line: `seq` (monotonic per card) + `ts` (RFC3339Nano) + `type` +
  `actor` (who triggered it) + `status` (post-transition snapshot) + `step` + `detail` (e.g.
  resume timestamp, error summary, downstream card ID).
- The `type` enum maps 1-to-1 to state-machine transitions: `queued` / `dispatched` / `step_ok` /
  `limit_paused` / `held` / `retry` / `canceled` / `done` / `failed` / `closeout` (downstream cards
  enqueued after completion).
- Writes use `O_APPEND` + `fsync`: POSIX guarantees append is atomic (concurrent tick + CLI writers
  never interleave bytes), and `fsync` guarantees any claimed-written event has hit disk. A `kill -9`
  half-JSON tail is sealed on the next append with a leading `\n` so subsequent readers reject only
  the corrupt line; earlier events are untouched.
- **`seq` assignment uses two layers of locking**: `O_APPEND` only makes the write positional-atomic;
  it does not serialize the `nextSeq`(read)-compute-`Write` compound sequence. Two writers can each
  read max=N and both write seq=N+1 — two events share a seq and "delete a middle event" no longer
  triggers a gap. So the `nextSeq+append+fsync` trio is wrapped by (a) a per-task in-process
  `sync.Mutex` (guards same-process goroutines and the stale-lock bootstrap race) plus (b) an
  `<id>.jsonl.lock` file lock (O_EXCL grab, 5s TTL, guards cross-process contention).
- **Gaps are disclosed, never masked**: the board's activity stream monitors `seq` monotonicity from
  seq=1 (so a deleted head-of-file event also triggers a gap) and inserts an explicit "event gap
  (missing seq X..Y)" item whenever a jump is detected, plus a "event gap (trailing residue or
  corrupt line discarded)" item when the tail is corrupt. It never synthesizes a fake event by
  inferring from status to plug the hole.
- No signing in this first pass (no proven need for tamper-evidence — only append-only leave-trace
  semantics are needed).

## Idempotent tombstones (per-task `tombstones/<id>.json`)

The event ledger records "what happened"; the tombstone ledger records "has this side effect
already been injected" — preventing a process restart from repeating the same injection at the
same site.

Three "at-most-once injection" sites are guarded by the tombstone rail:
1. **limit_paused recovery**: after a task hits the usage limit and is later re-dispatched by tick,
   `runTask` sends `resume_prompt` (e.g. "Continue. The previous instruction was interrupted by a
   usage limit…") into the same claude session when `MidStep=true+SessionID` is non-empty. If the
   process crashes after "prompt sent, state not persisted", the next tick would send it again,
   amplifying one continuation into N wasted-token re-sends.
2. **mid_step continuation**: same code path as (1); the difference is the trigger (crash-interrupt
   rather than limit-pause).
3. **cross-chain reconcile**: each tick sweep looks for orphaned A/B cards (`done` + no successor)
   and calls `saveTask(failed) + emit failed`. A crash between the two loses the event; two tick
   rounds may repeat the verdict.

Guard semantics (`tombstones.go`):
- Write `pending(attempt+1)` before the injection; write `final` after it succeeds.
- On tick re-sweep, `phase=final` → skip; `phase=pending && attempt≥bound` → skip
  (`bound=2` allows one crash + one retry).
- **Counter-example injection**: corrupt bytes are treated as no tombstone with stderr disclosure —
  neither crash nor silent skip (silent skip would never re-inject, hiding the deadlock).
- **Reset-at-entry**: when `runTask` enters and the previous on-disk status was NOT `running`
  (`queued/limit_paused/held`), the current step's resume tombstone is cleared — a signal that
  the orchestrator sanctioned a new attempt cycle, so the fresh `bound=2` counter starts from zero.
  If the previous status was `running`, the tombstone is preserved to cap crash storms.
- **Explicit CLI reset (Round-1 addendum)**: `cmdSetStatus`' `retry` and `release` branches both
  call `resetTombstoneKind(reconcileCrossKind())` — these are the only CLI paths from `held/failed`
  back to `queued`, mirroring the resume side's fresh-entry rule. Without this reset, a `final`
  tombstone would silently keep an orphaned card masquerading as a trustworthy `done`.
- **Skipped upgrades to held (Round-1 addendum)**: when `reconcileCrossChains` sees
  `injectAtMostOnce` return `skipped=true` (final already set, or bound exhausted), the card is
  lifted from `done` to `held` and an `evHeld(reason=reconcile_cross_tombstone_exhausted,
  actor=runner:reconcile-tombstone)` is emitted — no more single-leg `done` cards impersonating
  trustworthy results, no more infinite stderr spam masquerading as disclosure.
- **Per-task tombstone lock + two-phase critical section (Round-2 addendum)**: Round-1's explicit
  CLI reset introduced the first concurrent read-modify-write on the same tombstone ledger by
  CLI vs. runner tick. Without a lock, `injectAtMostOnce`'s final writeback would silently
  resurrect an entry that a concurrent `resetTombstoneKind` had just deleted — nullifying `retry`'s
  promise of "clear the tombstone, let `bound=2` re-arm, let reconciliation try again." Root fix:
  - `sync.Mutex` (in-process goroutines) + `tombstones/<id>.json.lock` (cross-process; tmp +
    `os.Link` atomic rename, TTL 5s; stale judgment and PID-checked release both reuse the
    isomorphic `staleEventLock`/`releaseEventLock` from `events.go`).
  - `injectAtMostOnce` is two-phase: phase 1 holds the lock while reading the ledger and writing
    `pending`, then releases; phase 2 runs `inject` unlocked (resume-side LLM subprocesses take
    minutes and holding the lock across would trip stale eviction); phase 3 re-acquires the lock,
    claims the entry, upgrades to `final`.
  - **Entry-gone / nonce two-layer defense**: on phase 3's re-read, if the entry has been deleted
    by a concurrent `reset`, or the nonce no longer matches our pending, we abandon the `final`
    rebuild — even if lock semantics regress, `final` cannot silently resurrect a `reset` entry.
  - **Class closure**: `resetTombstoneKind` (runTask fresh-entry / CLI `retry` / CLI `release`) and
    `archiveTaskTombstones` (`clean` / `postComplete` archive paths) all serialize through the same
    lock. Bare `os.Rename` in archive would otherwise race phase 3 into an "archived file + live
    ledger with only one `final` row" audit-forgery scene.
- **Emit must precede saveTask (Round-3 addendum)**: `reconcileCrossChains`'s skipped→held branch
  and the failed branch inside the `injectAtMostOnce` closure both used the old order (save first,
  emit after). If a crash or IO error lands between them, the card is persisted as `held/failed`
  but the orphan predicate `status==done` permanently excludes it, and `evHeld/evFailed` is
  permanently lost with no re-emit path — the ledger shows a `done→held/failed` zero-event jump,
  exactly the "zero-disclosure" defect class tombstones exist to eliminate. Fix: move `emit` before
  `saveTask`, aligning with `runTask`'s resume-side order — if the crash lands in the middle, disk
  still says `done`, the next tick re-enters the orphan predicate, and re-emit+save self-heals.
  The cost is at most one duplicate event (`bound=2` blocks unbounded repetition), preferable to
  permanent silence. Event duplication over permanent event absence is the first-principles reason
  tombstones exist. `TestReconcileSkippedHeldEmitsBeforeSave` / `TestReconcileFailedEmitsBeforeSave`
  proxy "crash between save and emit" via a failing `saveTask`; any regression of the order fails
  red. `TestResumeHeldSourceOrder` uses a static source-code guard to lock the resume-side contract
  against future refactor inversions.
- **Archived with the card**: the tombstone ledger is moved to `archive/tombstones/<id>.json`
  by `archiveTask`, so an audit can inspect exactly how many attempts each injection needed.

Data model (single JSON, not JSONL): `{"version": 1, "entries": {"resume:0": {kind, attempt,
phase, nonce, ts}, "reconcile:cross": {...}}}` — one `atomicWrite` (tmp→rename) replaces the
whole ledger atomically. Injection sites are few (bounded by step count), so line-append
semantics are unnecessary.

## Permissions and safety

By default tasks do **not** use `--dangerously-skip-permissions`:

- Review/assembly tasks use a read-only tool whitelist;
- `sequence` defaults to `acceptEdits` + a whitelist of common build/test commands; Bash commands outside the whitelist are auto-denied in headless mode (Claude works around them or explains).

For full autonomy on a single task, add `-skip-permissions`, or set `skip_permissions` for the corresponding type in `config.json`. Tune the tool whitelist per type in `type_defaults.*.allowed_tools`.
