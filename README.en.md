# ClaudeGo

[中文](README.md) | **English**

[![LINUX DO](https://img.shields.io/badge/LINUX%20DO-community-ffb003?logo=discourse&logoColor=white)](https://linux.do)

A local task queue and scheduler built around Claude's 5-hour usage-limit window. A single Go binary, no external dependencies.

The core idea: **the orchestrator itself is pure local code and consumes zero Claude quota**; `claude -p` is invoked only when a task actually runs. When a limit is hit, the task auto-pauses and records the reset time, then reconnects to the *same* session via `--resume` once the window reopens — wringing every 5-hour window dry.

```
                 ┌──────────────────────────────────────────────┐
   claudego add  │  Task queue  (~/.claudego/tasks)             │
   assemble ────▶│  queued ──▶ running ──▶ done                 │
   review  plan  │              │  └──▶ failed (after backoff)  │
   adopt  brief  │              ▼                               │
                 │        limit_paused ──(reset reached)──┐     │
                 └────────────────────────────────────────┼─────┘
                                                          │
   launchd / daemon ticks every 5 min ──▶ pick a task ────┘
                                              │
                                              ▼
                  claude -p --model <model> --resume <session> ...
```

## The five task types

| Type | Purpose | Default permissions / model |
|---|---|---|
| `design-review` | Design-review session: read-only review of code/architecture, producing P0/P1/P2 graded findings | Read-only tools + git log/diff |
| `prompt-assembly` | Prompt-assembly session: researches the project, then decomposes a goal into a prompt sequence — **the tasks it produces auto-enqueue** | Read-only tools |
| `sequence` | Preset prompt sequence: several steps run in order within the same session (chained via `--resume` for continuous context) | acceptEdits + common build/test commands |
| `coordinate` | Coordination session: reads a **live** queue snapshot + per-session progress reports and splits a goal into division-of-labor tasks (with model suggestions) that auto-enqueue | Read-only tools, defaults to opus |
| `progress-pull` | Progress-pull session: `--resume` a session and have it emit a structured progress report to disk | Read-only tools, defaults to haiku |

Tasks chain together: `assemble` → emits a `sequence` that enqueues → runs to completion → `review_after` auto-enqueues a `design-review` of the changes just made.

## Progress pull → coordinate → auto-advance

The orchestration loop when several sessions work in parallel:

**The desktop app is in scope too**: Claude Code's desktop app and the CLI share the `~/.claude/projects` session store and the same subscription quota, so sessions opened in the desktop app can equally be listed, pulled for progress, and taken over with `--resume`.

```bash
# 0) Find sessions: list a project's recent claude sessions (desktop + CLI share one pool) and grab a session ID
claudego sessions -dir ~/Projects/myapp

# 1) Pull progress. Interactive sessions (incl. desktop): prints a "summarize progress" prompt; paste it in and the report is written back to ~/.claudego/progress/
claudego brief -dir ~/Projects/myapp -title auth-refactor
#    With a session ID (queue tasks / desktop sessions from `sessions`): enqueue a haiku pull task, fully automatic
claudego brief -id t0705-xxxx -auto
claudego brief -session <session-id> -dir ~/Projects/myapp -auto

# 2) Divide the work. A coordinate task injects a live queue snapshot + all progress reports at dispatch time,
#    producing: a plain-English division of labor (what each task does / suggested model / manual-takeover command, kept in the log)
#    + the split tasks auto-enqueued (with a model field; dependents get higher priority; resumable ones carry a session_id)
claudego plan -dir ~/Projects/myapp "finish the upload module this week and fill in the tests"

# 3) Auto-advance: launchd/daemon ticks as usual and runs tasks one by one per the model suggestions; inspect and take over anytime
claudego list                       # see how far the split has progressed (title column shows "title ▸ latest progress")
claudego log <coordinate-task-id>   # read the plain-English division of labor
claudego cmd <id>                   # to take over a task by hand: prints the claude command + current-step prompt (hold it first)
claudego progress                   # progress overview (a "status" column shows where each stands); -show <KEY> for a human-readable render; -in to paste-import
```

**The board doubles as progress**: `claudego list`'s title column shows each task's "title ▸ latest progress" (preferring the status from a pulled progress report, otherwise falling back to an auto-captured summary of the last step's output); `claudego progress` has a dedicated "status" column, and `progress -show <KEY>` is a human-readable render (goal / in-progress / done / remaining / blockers / key files, with the multi-thousand-word handoff prompt folded by default, `-full` to expand) — so you read *where things stand*, not a static title.

**Model routing**: a task carrying a `model` field runs with `--model` (subscription limits are weighted per model, so routing routine work to sonnet/haiku noticeably stretches the 5-hour window). Every add-style command accepts `-model`; a coordinate task's division output auto-suggests a model per task along "mechanical → haiku / routine implementation → sonnet / high-risk → strongest default", and you can set defaults in `type_defaults.*.model`. Leverage inversion: expensive models do only the small-token orchestration and arbitration (coordinate defaults to `opus`); cheap models burn the large-token execution.

**Design-phase profile (fable designs, codex/opus builds)**: when design quality is the top priority, switch the design trio to the strongest model — set `coordinate` / `design-review` / `prompt-assembly`'s model to `"claude-fable-5"` in `type_defaults`. The coordinate template then assigns a model to each emitted task along "design → `claude-fable-5`, implementation → prefer `runner:"codex"` (GPT-5.5, high-reasoning, its own independent quota, `model` left empty), mechanical → `sonnet`, trivial → `haiku`", reserving `opus` only for cards that genuinely need the Claude ecosystem (sub-agents, MCP tools, or resuming a Claude session). `model_weights` already ships `claude-fable-5` and `fable` at weight 10 by default. Once you enter heavy development, you can dial `design-review` back to `opus` to control spend.

**In-session sub-layering (sub-agents)**: `sequence` tasks whitelist the Task tool by default, so paired with user-level sub-agents (`~/.claude/agents/deep-reasoner.md` bound to opus, `fast-worker.md` bound to sonnet) an executing session can hand hard reasoning up and push mechanical labor down — routing by task across sessions and by stage within a session, two layers stacked.

### File-based state (`fresh_steps`) and human gating (`-hold`)

Keeping project state in **files** (state.md / TASKS.md, etc.) is recommended so tasks don't depend on session memory:

- `add -fresh`, or `"fresh_steps": true` in the emit JSON: steps don't `--resume` — each step is a brand-new session. The coordinate template bakes in a three-part contract: on start read the state file → make exactly one increment → on finish update the state file and task list.
- Benefits: you never hit the session-context ceiling ("Prompt is too long" failures vanish), a limit interruption simply re-sends the current step in a fresh session (no resume prompt needed), the codex backup executor can take over **any** step (no longer limited to single-step tasks), and it's audit-friendly (all state changes live in git).
- `plan -hold` / `assemble -hold`: the split tasks are parked (held) first; after a human review, `claudego release <id>` lets them proceed — the full loop of "split → gate → advance → review → update state".

### Cross-verification (fable stand-in: two independent engines + adversarial cross-check)

When a design-tier model (fable) hits its weekly limit, have two **different** engines each answer the same fable-tier task (design / review / ruling / ratification) independently, then let the second engine take the first's conclusion and adversarially hunt for gaps — two independent perspectives are far harder to lead astray with the same blind spot than one:

```bash
claudego cross -dir ~/Projects/myapp "rule on the contract semantics of a missing config key"  # default engine pair
claudego cross -profile my-pair -dir ~/Projects/myapp "..."                                     # switch to a pair you defined in cross_profiles
claudego cross -list                                                                            # list available pairs
```

An event-driven three-card chain — you enqueue only A; B/C chain on automatically:

- **A**: engine 甲 answers independently (first-principles + adversarial self-review), producing conclusion A;
- **B**: auto-dispatched once A finishes; engine 乙 answers independently — its **prompt is identical to A's and contains neither A's conclusion nor any pointer to it**. A's conclusion is parked in an isolation sidecar at `~/.claudego/crosscheck/<chain-id>.a` (read/written only by the orchestrator, deleted once C consumes it); it never enters B's card fields and is **not written to A's or B's log** (A's result log is redacted), and B carries only an opaque chain id unrelated to A's card id. The solo template also instructs B not to read orchestration/state directories. By default B can't reach A because it **isn't given it and is told not to look** — but this is **not a hard sandbox**: honestly, B's card carries the chain id, and the sidecar path is derived deterministically from it, so that id is effectively a pointer to the sidecar; codex `--sandbox read-only` can also read the whole disk, so a deliberately-searching executor could still find it. What this achieves is **passive-exposure minimization + a behavioral guard**; true hard isolation would require restricting the executor's read scope (which this tool does not provide);
- **C**: auto-dispatched once B finishes; engine 乙 reads A back from the sidecar and, together with B, **adversarially cross-checks** them (what did each miss / adjudicate disagreements / blind spots only one side caught), producing a merged conclusion written to a progress report (`claudego progress -show <chain-id>`, for you to finalize).

**The model source is switchable** via named engine pairs in `config.cross_profiles` (`default_cross_profile` picks the default). The default `opus-codex` = 甲 `claude opus·max` + 乙 `codex·max` (乙's concrete model comes from your `codex_model`; both at their top standard reasoning tier). Each engine's `kind` is one of `claude` / `codex` / `remote-claude` / `remote-codex`:

```jsonc
"default_cross_profile": "opus-codex",
"cross_profiles": {
  "opus-codex": {
    "a": { "kind": "claude", "model": "claude-opus-4-8", "effort": "max", "label": "opus·max" },
    "b": { "kind": "codex",  "effort": "max", "label": "codex·max" }
  }
}
```

- An engine's `effort` is the shared thinking level for claude and codex (claude → `--effort`, codex → `model_reasoning_effort`; same names, same order `low<medium<high<xhigh<max`), overriding the global `codex_reasoning` per task;
- A `claude` engine requires a `model`; a `codex` engine requires `codex_bin` + `codex_model` (otherwise it would run the account/CLI default model, contradicting what the profile advertises — the command errors out, so there's no silent downgrade);
- Cross cards are **read-only analysis** (read contracts/source/diffs, never write the repo; codex side runs `--sandbox read-only`); `-dir` may not be the claudego data root or a subdirectory of it;
- 甲 and 乙 must share an **execution location** (both local, or both on the same `remote_hosts` host) — the three cards share one working directory, so a cross-machine pair is rejected up front;
- **Guard rail**: during a claude cooldown, even with `codex_fallback` on, a claude-engine cross card is **never** silently diverted to codex, and a codex-pinned card **never** fails open to claude when codex is unavailable (either would collapse both engines onto one and make the verification a sham) — engine identity is frozen; the card waits for its window. If any step of the chain breaks, the parent card records it (visible in `list`), so a single-leg result never masquerades as the final verdict.

**Known limitations (stated honestly)**: (1) the 甲≠乙 "different engine" check is **textual** best-effort (kind + model name); it won't catch model aliases that point to the same model — profiles are user-authored, so alias-equals-same-engine is a config responsibility; (2) enqueue-time freezing pins engine identity (kind/model/effort), **not infrastructure paths** (codex/ssh binary location, remote sandbox, etc.) — changing those in normal operation is an infra change, not identity drift; (3) the three-card chain is event-driven, not crash-atomic: a single-leg orphan from a crash exactly between "mark done" and "spawn successor" is caught and marked failed by the per-tick `reconcile` (visible), but the narrow combination of "crash + manual `clean` archiving the parent" can slip through. These are rare crash/config edges, not normal-path defects.

### Taking over existing role sessions (the review/assembly/execute sessions you maintained by hand)

When a project folder already hosts a batch of long-lived role sessions, split them by role:

```bash
claudego sessions -dir ~/Projects/myapp        # Claim them: identify each role session by its first message, grab the ID

# Ones with work in flight (execute/refine sessions) → pull progress first, then decide to resume or restart
claudego brief -session <ID> -auto             # distill the existing context into a progress report (with next_prompt)
claudego adopt <ID> -dir ~/Projects/myapp      # take over and resume the unfinished ones directly

# The role sessions themselves → the matching type command + -session to mount, continuing on the old session's accumulation
claudego review   -session <old-review-session-id> "review this week's changes"
claudego assemble -session <old-assembly-session-id> "next goal"
claudego add -type sequence -session <old-execute-session-id> -file next-steps.md

# Or skip mounting: fold the role requirements distilled in the old session into templates/*.md, then start fresh each round (cheaper context)
```

Note: resuming an existing session in headless mode is a **fork** (a new session id is spun off; the original desktop session is untouched). After a task's first round, later rounds should mount the task's latest `session_id` (visible in `claudego list -json`), or just append steps to the same task. Long-lived session context grows ever more expensive, so the general advice is: sediment knowledge into templates/progress reports, and run execution in short sessions.

## Dispatch rules (tunable in config.json)

1. **Resume first** (`resume_first`): tasks interrupted by a limit run before new ones — finish the unfinished first;
2. **Higher priority wins**;
3. **Type order** (`type_order`): default `progress-pull > coordinate > review > sequence > assembly` (review is cheap and returns feedback fast; assembly spawns new work so it goes last);
4. FIFO within the same tier.

Limits are global: whenever any task hits a limit, a global cooldown is written (`cooldown.json`); during it no task is dispatched and no probe calls are wasted. The cooldown time prefers the reset timestamp in the error message; if none can be parsed it falls back to retrying after `limit_fallback_min` minutes.

## Quick start

```bash
make build && make install     # compile and install to /opt/homebrew/bin
claudego init                  # initialize ~/.claudego (override the data dir with CLAUDEGO_ROOT)

# 1) Preset prompt sequence: split steps in steps.md with a lone --- line
claudego add -title "refactor auth" -dir ~/Projects/myapp -priority 5 -review-after -file steps.md

# 2) Prompt assembly: have Claude research first, then auto-generate a task sequence and enqueue it
claudego assemble -dir ~/Projects/myapp "add resumable uploads to the upload module, with tests"

# 3) Design review: read-only review
claudego review -dir ~/Projects/myapp "concurrency and error handling"

# 4) Take over an interactive session just interrupted by a limit (desktop or CLI; find the session id with claudego sessions)
claudego adopt <session-id> -dir ~/Projects/myapp

claudego run                   # run one round manually to verify
claudego install-launchd       # install background scheduling: ticks every 5 min, starts at login
claudego list                  # board; log <id> for detail; doctor for a self-check
```

If you'd rather not install launchd, just run `claudego daemon` as a foreground resident.

**Cross-platform**: the core is pure Go and builds/runs on macOS, Linux, and Windows (`go build` yields a per-platform binary). `install-launchd` (login autostart + a tick every 5 min) only wires up macOS's launchd; on other platforms run `claudego daemon` as a foreground resident, or have the OS scheduler run `claudego run` every 5 minutes — systemd timers / cron on Linux, **Task Scheduler on Windows**. The single-instance lock is cross-platform (Windows uses `OpenProcess` for liveness), so scheduled runs won't collide.

## 5-hour quota redline (reserve headroom)

To leave headroom for bursty/interactive work: when the redline is active the queue stops dispatching (multi-step tasks also yield between steps), and `-force` crosses it. Three channels, inspectable anytime with `claudego quota`:

```jsonc
// ~/.claudego/config.json
"queue_budget_tokens": 2000000,  // ① local ledger: max weighted tokens the queue may spend in the sliding 5h window; 0 disables
"redline_percent": 85,           // ②③ shared redline for percentage channels: stop when any source's usedPercent hits the line; 0 disables
"usage_feed": "/Users/you/Library/Application Support/CodexBar/usage-history.jsonl",
"usage_feed_max_age_min": 90,    // ②   a stale sample is treated as unavailable → dispatch allowed (fail-open)
"oauth_usage": true,             // ③   subscription usage endpoint (third source), off by default; endpoint is undocumented
"oauth_usage_max_age_min": 15,   // ③   how long the endpoint response is considered fresh (minutes)
"oauth_usage_timeout_sec": 6,    // ③   HTTP timeout (seconds)
"model_weights": {"default":1,"opus":5,"sonnet":1,"haiku":0.2}   // per-model weighting for the ledger
```

- ① counts **only claudego's own calls** (desktop consumption is invisible to it); its semantics are a "queue budget ceiling" — your reserve = total quota − queue budget. Run `claudego quota` for a few days to see typical consumption before setting a value.
- ② is the global view; its sample format is compatible with CodexBar's usage-history.jsonl (enable the Claude-usage probe in CodexBar). Any tool that appends one JSONL line in the same format works too.
- ③ reads `api.anthropic.com/api/oauth/usage` directly (`anthropic-beta: oauth-2025-04-20` header + reusing the OAuth access token from `~/.claude/.credentials.json` or the macOS keychain), pulling 5h-window utilization. **The endpoint is undocumented and can change without notice** — any anomaly (network / creds / HTTP 4xx-5xx / missing field / format drift) is treated as "insufficient data" → fail-open. The implementation trusts **response body only** and never parses response headers (headers are trivially forged/overwritten by intermediaries, and verification has refuted the "response headers carry unified ratelimit numbers" claim). Override `oauth_usage_creds_path` / `oauth_usage_url` for tests or custom deployments.
- ②③ merge rule = **worst-value-wins** (redline is judged against the highest available percent) — when observations disagree, the worst-case assumption wins over voting or averaging. `claudego quota` prints all three sources side-by-side and flags any spread ≥5%.
- Genuine exhaustion still has the limit cooldown as a backstop (parse the reset time, resume when it arrives); the redline only yields *early*.

**Time-windowed redline** (`redline_windows`): inside a window, non-zero fields override the global thresholds; outside it they revert; cross midnight with `from > to`. `redline_lead_min` adds a pre-window buffer: for N minutes before a window, no new claude task is launched — a single-step task can't yield once started, so without the buffer a long task that starts right on the line burns into the reserved window (codex-pinned tasks are unaffected). Align a window's `from` with the quota window's real reset moment. A typical use — leave 25% headroom for interaction during the morning trading session, use the queue to the full the rest of the day:

```jsonc
"queue_budget_tokens": 0, "redline_percent": 0,   // global: unlimited
"redline_windows": [
  {"from": "06:50", "to": "11:50", "redline_percent": 75, "queue_budget_tokens": 300000}
]
```

## Codex backup executor (no downtime during limit gaps)

The scheduler itself is pure Go and spends no quota — a limit only makes tasks wait, it never takes the system down. But during a cooldown there's no execution capacity. Once you configure the codex CLI, whenever claude is blocked by cooldown or the redline, **single-step tasks with no existing claude session** (coordinate / review / assembly / single-step add — exactly the orchestration links that keep the pipeline moving) are automatically switched to run on `codex exec`:

```jsonc
"codex_bin": "/opt/homebrew/bin/codex",
"codex_fallback": true,
"codex_model": ""        // optional, passed through as -m
```

- Multi-step tasks that carry a claude session don't switch (context can't continue across CLIs); they resume automatically once the window resets;
- Codex runs on its own quota: not recorded in the claude ledger, its errors don't write the global cooldown, and its successes don't clear it;
- Sandboxing narrows by type: read-only tasks run `--sandbox read-only`, `sequence` uses `workspace-write`;
- The board and logs label `[codex]` / `runner=codex`, and the emit/progress-parsing pipeline works as usual (coordinate can keep enqueuing splits even during a cooldown);
- Reasoning effort is tunable via `codex_reasoning` (minimal/low/medium/high/xhigh), passed as `-c model_reasoning_effort=…`.

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

**Not doing**: no automatic replan/decompose — a CAMEL-style retry→decompose→replan is validated in
other work but ClaudeGo's `held`-to-human path is sufficient today; automatic replan on misclassification
means "the AI silently rewrote the task", which conflicts with the "complete task lineage must be
auditable" honesty discipline. If it's ever needed, a separate card.

## In-drain patrol + review-sync race root fix (CG-5)

Two independent stall-detection signals piggyback on the existing drain loop (no new daemon), covering
the "invisible" hang axis that `harvest`'s early reap (visible-completion axis) can't address.

**In-drain patrol**——`tick`'s cancel-reconciliation already rescans every `drain_rescan_sec` (default
15s); `patrolOnce` rides the same tick. For each running card it checks two independent signals:
- **Procgroup liveness**: `taskPG` registry + `processAlive(pid)` double-check (stale dead-pid entries
  in the registry can't fake liveness — the double-check is the counter-example defense).
- **Heartbeat**: whether the task log `~/.claudego/logs/<id>.log` file size is growing (executor's
  per-step `logBlock` continuously appends).

Verdict: `pgSeenAlive && !alive && dead-since ≥ 60s` (procgroup dead past `patrolPGGrace`) OR
`log-no-grow ≥ 30min` (`patrolHeartbeatTimeout`), either matches → judged stalled. **Heartbeat alone
does NOT prove liveness**: adversarial injection (test script appending log every 100ms while the
executor is dead) must not defeat patrol — procgroup liveness is the **authorization credential**,
heartbeat is merely an auxiliary signal. Startup-window protection: the `pgSeenAlive` guard excludes
false positives when a task just entered `activeIDs` but its `invoke` hasn't reached `cmd.Start` yet.

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
subshell, letting the outer shell still reach the marker-writing line. `rescueWaitDelay` stays as a
second-line defense for corner cases (marker not written as expected, e.g. wrap parse failure).

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

## Config quick reference (~/.claudego/config.json)

| Key | Default | Description |
|---|---|---|
| `poll_interval_sec` | 300 | launchd/daemon polling interval |
| `limit_fallback_min` | 30 | wait when no reset time can be parsed |
| `cooldown_margin_sec` | 90 | safety margin added on top of the reset time |
| `step_timeout_min` | 60 | hard per-step timeout (guards against runaways) |
| `max_attempts_per_step` | 3 | per-step retry ceiling |
| `retry_backoff_min` | 5 | base backoff between retries on non-limit errors |
| `resume_first` | true | interrupted tasks resume before new ones start |
| `type_order` | progress-pull > coordinate > review > sequence > assembly | type order at equal priority |
| `resume_prompt` | … | resume prompt sent after a limit interruption |
| `type_defaults.*.model` | coordinate opus; progress-pull haiku | default model per type (--model value); empty uses the account default |
| `no_fallback_models` | ["claude-fable-5","fable"] | design-tier models never downgraded to the codex backup — they wait for Claude |
| `thinking_tokens` | 0 | when >0, sets MAX_THINKING_TOKENS on Claude calls (larger thinking budget for design work) |
| `queue_budget_tokens` etc. | 0 (off) | 5-hour quota redline — see the dedicated section |
| `oauth_usage` / `oauth_usage_*` | false | subscription endpoint (third source); undocumented endpoint — anomalies treated as insufficient data |
| `max_parallel` | 1 | tasks per tick (writing tasks are serialized per directory; read-only types like design-review / progress-pull are exempt and may run concurrently in the same repo) |
| `codex_bin` / `codex_fallback` | empty / false | cooldown backup executor — see the dedicated section |
| `codex_reasoning` | "" | global codex reasoning effort (minimal/low/medium/high/xhigh/max/ultra) → `-c model_reasoning_effort=…`; a per-task effort overrides it |
| `cross_profiles` | {opus-codex} | cross-verification engine pairs (`claudego cross`) — see the dedicated section |
| `default_cross_profile` | "opus-codex" | engine pair used when `cross` gets no `-profile` |

Prompt templates live in `~/.claudego/templates/*.md` and can be edited directly (`{{GOAL}}` `{{DIR}}` `{{FOCUS}}` are substituted; `{{QUEUE}}` `{{PROGRESS}}` in `coordinate.md` are replaced with a live snapshot **at dispatch time**).

## Testing

```bash
make test   # run the full state machine against a mock claude: scheduling / limit pause / cooldown / resume / assembly enqueue / failure backoff / model routing / progress pull / coordination
```

## Acknowledgements

Shared on the [LINUX DO](https://linux.do) community — thanks to everyone there for the feedback.
