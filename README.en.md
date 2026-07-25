# ClaudeGo

[中文](README.md) | **English**

[![LINUX DO](https://img.shields.io/badge/LINUX%20DO-community-ffb003?logo=discourse&logoColor=white)](https://linux.do)

**Wring every Claude 5-hour usage window dry.** A local task queue and scheduler: when a task hits the limit it auto-pauses, records the reset time, and at reset reconnects to the *same* session via `--resume` to keep going. A single Go binary, no external dependencies, and **the orchestration itself costs zero quota**.

```bash
claudego add -title "refactor auth" -dir ~/Projects/myapp -file steps.md   # drop work in the queue
claudego install-launchd                                                   # it runs itself from here
```

## What it does for you

| Your situation | What ClaudeGo does |
|---|---|
| Hit the limit and you have to babysit the window | Auto-pause + remember the reset instant, then resume the same session on time — **the queue runs while you sleep** |
| One goal means hand-writing a dozen prompts | `assemble`: Claude researches the project first, then decomposes the goal into a prompt sequence that **auto-enqueues** |
| Several sessions in flight, progress only in your head | `brief` pulls structured progress; `plan` reads a live queue snapshot, splits the work, and enqueues the split tasks |
| Everything runs on the priciest model | Per-task model routing: haiku for mechanical work, sonnet for regular implementation, the top tier for high-risk work — expensive models only orchestrate and arbitrate |
| Cooldown means total downtime | During cooldown, single-step orchestration cards divert to `codex exec` (a separate quota), so the pipeline never stalls |
| You still have to review the changes yourself | An adversarial review card is auto-dispatched on completion; read-only reviews can even be offloaded to a second machine |
| "Where is any of this right now?" | `claudego list` + a web board (kanban / quota burndown / landed progress) + a per-task event ledger |

## Quick start

```bash
make build && make install     # compile and install to /opt/homebrew/bin
claudego init                  # initialize ~/.claudego (override the data dir with CLAUDEGO_ROOT)
claudego doctor                # self-check: claude CLI, directories, config
```

Three common ways to enqueue — pick one to start:

```bash
# 1) You already know the steps: split them in steps.md with a lone --- line
claudego add -title "refactor auth" -dir ~/Projects/myapp -priority 5 -review-after -file steps.md

# 2) You only have a goal: have Claude research first, then auto-generate and enqueue a task sequence
claudego assemble -dir ~/Projects/myapp "add resumable uploads to the upload module, with tests"

# 3) A session in front of you just got cut off by a limit: take it over and continue
claudego adopt <session-id> -dir ~/Projects/myapp     # find the id with claudego sessions
```

Let it run on its own:

```bash
claudego run                   # run one round manually to verify
claudego install-launchd       # background scheduling: ticks every 5 min, starts at login (macOS)
claudego list                  # board; the title column is "title ▸ latest progress"
claudego log <id>              # detail for one card; cmd <id> prints the manual-takeover command
claudego board                 # web board at http://127.0.0.1:8787
```

Not on macOS: run `claudego daemon` as a foreground resident, or have systemd timers / cron / Windows Task Scheduler invoke `claudego run` every 5 minutes. The core is pure Go and builds on all three platforms; the single-instance lock is cross-platform, so scheduled runs won't collide.

## The five task types

| Type | Purpose | Default permissions / model |
|---|---|---|
| `design-review` | Design-review session: read-only review of code/architecture, producing P0/P1/P2 graded findings | Read-only tools + git log/diff |
| `prompt-assembly` | Prompt-assembly session: researches the project, then decomposes a goal into a prompt sequence — **the tasks it produces auto-enqueue** | Read-only tools |
| `sequence` | Preset prompt sequence: several steps run in order within the same session (chained via `--resume` for continuous context) | acceptEdits + common build/test commands |
| `coordinate` | Coordination session: reads a **live** queue snapshot + per-session progress reports and splits a goal into division-of-labor tasks (with model suggestions) that auto-enqueue | Read-only tools, defaults to opus |
| `progress-pull` | Progress-pull session: `--resume` a session and have it emit a structured progress report to disk | Read-only tools, defaults to haiku |

Tasks chain together: `assemble` → emits a `sequence` that enqueues → runs to completion → `review_after` auto-enqueues a `design-review` of the changes just made.

**The desktop app is in scope too**: Claude Code's desktop app and the CLI share the `~/.claude/projects` session store and the same subscription quota, so sessions opened in the desktop app can equally be listed, pulled for progress, and taken over with `--resume`.

## How it works

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

Four things hold the loop together:

1. **Zero-quota orchestration** — the scheduler is pure local Go that only reads and writes JSON under `~/.claudego`; it never calls a model itself. Dispatch, backoff, the board, and the ledgers are all free; `claude -p` runs only when a task actually executes.
2. **A limit is a recoverable state, not a failure** — on a limit hit the reset timestamp is parsed out of the error and written to a global cooldown (`cooldown.json`); no probe calls are wasted during it. At reset, a resume prompt goes to the *same* session so work continues from the interruption point — the original prompt is never re-sent and the work is never redone.
3. **Ordered dispatch, safe on disk** — resume first → higher `priority` → type order (review is cheap and returns feedback fast; assembly spawns new work so it goes last) → FIFO within a tier. Every successful step is written atomically, so a killed process loses no progress; a single-instance lock keeps repeated launchd triggers from running concurrently.
4. **Classified failures instead of blind retries** — auth/permission failures go straight to `held` for a human, over-long input goes straight to `failed` (the same prompt will overflow again), and only the unclassifiable falls back to backoff retries — quota never gets burned on retries that are certain to fail.

## Going further

Once it's running, pick what you need — each of these is covered in full in the [advanced guide](docs/guide.en.md):

- **[Progress pull → coordinate → auto-advance](docs/guide.en.md#progress-pull--coordinate--auto-advance)** — the orchestration loop for parallel sessions: pull progress → a coordinate task reads the live queue and splits the work → tasks advance one by one, with takeover available at any point.
- **[File-based state and human gating](docs/guide.en.md#file-based-state-fresh_steps-and-human-gating--hold)** — keep state in files and start each step fresh so context limits are never hit; `-hold` parks the produced tasks until a human releases them.
- **[Review divert](docs/guide.en.md#review-divert-offload-read-only-review-to-a-second-machine)** — implement locally, run the adversarial review on a second machine to balance both quotas; a failed sync falls back to local review so the loop never breaks.
- **[Cross-verification](docs/guide.en.md#cross-verification-fable-stand-in-two-independent-engines--adversarial-cross-check)** — two different engines answer the same question independently, then cross-check adversarially; the stand-in when the design-tier model hits its weekly limit.
- **[Web board](docs/guide.en.md#web-board-board-command)** — projects side by side as a horizontal rail, **remaining**-quota burndown, progress split by kind of work (design / impl / fix / review), and goal-anchored "landed progress"; insufficient data is always disclosed rather than estimated, and queue data stays read-only (the only write is the board's own project-collapse state).
- **[5-hour quota redline](docs/guide.en.md#5-hour-quota-redline-reserve-headroom)** — reserve headroom for interactive work: a local ledger + CodexBar usage feed + the subscription endpoint, taking the most conservative reading when they disagree; time-window redlines supported.
- **[Codex backup executor](docs/guide.en.md#codex-backup-executor-no-downtime-during-limit-gaps)** — divert single-step orchestration cards to codex during claude cooldown; design-tier models are pinned and never downgraded, so cross-verification's engine independence is never swapped out.
- **[Taking over existing role sessions](docs/guide.en.md#taking-over-existing-role-sessions-the-reviewassemblyexecute-sessions-you-maintained-by-hand)** — fold hand-maintained review/assembly/execute sessions into the queue by role.

For what happens on the failure paths (full dispatch rules, limit recovery, failure classification, stall patrol, the event ledger, idempotent tombstones, permission boundaries) → [runtime internals](docs/internals.en.md).

## Config quick reference (~/.claudego/config.json)

The keys you'll actually touch; the full table lives in the [configuration reference](docs/config.en.md):

| Key | Default | Description |
|---|---|---|
| `poll_interval_sec` | 300 | launchd/daemon polling interval |
| `limit_fallback_min` | 30 | wait when no reset time can be parsed |
| `step_timeout_min` | 60 | hard per-step timeout (guards against runaways) |
| `max_attempts_per_step` | 3 | per-step retry ceiling |
| `retry_backoff_min` | 5 | base backoff between retries on non-limit errors |
| `resume_first` | true | interrupted tasks resume before new ones start |
| `type_order` | progress-pull > coordinate > review > sequence > assembly | type order at equal priority |
| `type_defaults.*.model` | coordinate opus; progress-pull haiku | default model per type (--model value); empty uses the account default |
| `max_parallel` | 1 | tasks per tick (writing tasks are serialized per directory; read-only types are exempt) |
| `queue_budget_tokens` etc. | 0 (off) | 5-hour quota redline — see the [guide](docs/guide.en.md#5-hour-quota-redline-reserve-headroom) |
| `no_fallback_models` | ["claude-fable-5","fable"] | design-tier models never downgraded to the codex backup — they wait for Claude |
| `codex_bin` / `codex_fallback` | empty / false | cooldown backup executor — see the [guide](docs/guide.en.md#codex-backup-executor-no-downtime-during-limit-gaps) |
| `codex_fallback_model` | "" | model used when a claude card downgrades to codex (tier-parity: opus→terra, not sol); empty falls back to `codex_model` |
| `default_review_host` / `remote_mirror_root` / `default_review_sync` | "" | the review-divert trio: with all three set, auto-review of local impl cards diverts to the remote host by default |

Prompt templates live in `~/.claudego/templates/*.md` and can be edited directly (`{{GOAL}}` `{{DIR}}` `{{FOCUS}}` are substituted; `{{QUEUE}}` `{{PROGRESS}}` in `coordinate.md` are replaced with a live snapshot **at dispatch time**).

**Permissions stay tight by default**: tasks do **not** use `--dangerously-skip-permissions` — review/assembly get a read-only tool allowlist, and `sequence` defaults to `acceptEdits` plus an allowlist of common build/test commands. Add `-skip-permissions` to a single card when full autonomy is needed; details in [runtime internals · permissions and safety](docs/internals.en.md#permissions-and-safety).

## Documentation

| Doc | Contents |
|---|---|
| [Advanced guide](docs/guide.en.md) | Coordination loop, file-based state, review divert, cross-verification, web board, quota redline, codex backup executor |
| [Runtime internals](docs/internals.en.md) | Dispatch rules, limit recovery, failure classification, stall patrol, event ledger, idempotent tombstones, permissions |
| [Configuration reference](docs/config.en.md) | The full `~/.claudego/config.json` key table + templates |
| [Changelog](docs/changelog.en.md) | Version changes grouped by theme |

## Testing

```bash
make test   # a mock claude drives the full state machine: dispatch / limit pause / cooldown / resume / assembly enqueue / failure backoff / model routing / progress pull / coordination
```

## Acknowledgements

This project is shared with the [LINUX DO](https://linux.do) community — thanks to everyone there for the feedback.
