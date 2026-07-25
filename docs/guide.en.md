# ClaudeGo advanced guide

[中文](guide.md) | **English** · back to [README](../README.en.md)

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

### Review divert (offload read-only review to a second machine)

Run implementation locally while routing the adversarial review to another `remote_hosts` machine, balancing quota across both sides (review is read-only and therefore freely diverted):

- `add -review-host <host>`: the auto-queued review card after completion runs on this remote host (`remote_hosts` key). The fix chain inherits the declaration — subsequent review rounds keep diverting. If the **sync command fails**, it falls back to local review (the loop stays intact); if the remote review execution itself fails it is treated as an ordinary task failure (retried/backed off), not pulled back locally.
- `add -review-dir <mirror-path>`: the working directory for the review card on the review host (the directory the review template renders against). Used together with `-review-host`.
- `add -review-sync <command>`: before dispatching the review card, run this command locally via `sh -c` (e.g. rsync the changes to the review host; 120 s timeout); a non-zero exit code triggers the local-review fallback. Can be used alone (sync only, no divert). **The sync command runs with the implementation card's `dir` as cwd**, so relative paths (e.g. `rsync -a ./ hostb:/mirror/`) are relative to the implementation directory, not the daemon's start directory. The command must **complete in the foreground** (no `&` backgrounding); a zero exit code means sync is done — backgrounding would let the review start before the mirror is ready.

**Global default divert** (config trio — avoids per-card manual specification): when all three keys `default_review_host` / `remote_mirror_root` / `default_review_sync` are set, any local implementation card (`RemoteHost` empty) whose `review_after` review has no explicit `ReviewHost` is automatically diverted to `default_review_host`, with the review directory auto-derived as `<remote_mirror_root>/<impl-card-dirname>` and the sync command inherited from `default_review_sync`. **All three must be set** for the default to apply (any missing key disables it); per-card `-review-host` / `-review-dir` / `-review-sync` explicit values always take precedence; remote implementation cards (`RemoteHost` non-empty) are excluded (they are already reviewed remotely).

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
- Cross cards are **read-only analysis** (read contracts/source/diffs, never write the repo); the **local** codex side by default runs in a one-shot isolated copy + `--sandbox workspace-write` (the copy is built and torn down per card so the source repo is never write-polluted; see the "Sandbox" section on CG-R3 `codex_review_sandbox`); the **remote** codex side only relaxes to `workspace-write` when the directory sits under `remote_mirror_root` (a one-shot mirror distributed by sync-lane), and keeps `--sandbox read-only` as a sandbox-level hard guarantee when running in a real business repo (the normal case when the three cards share a working directory) (CG-R3 R1 P0-1); `-dir` may not be the claudego data root or a subdirectory of it;
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

## Web board (`board` command)

A live read-only kanban in your local browser.

```bash
claudego board               # default http://127.0.0.1:8787
claudego board -port 9000    # custom port
claudego board -ttl 30       # task-snapshot cache TTL in seconds (default 10)
```

Three inviolable rules:
- **Queue data is read-only**: every handler reads `~/.claudego` through `os.ReadFile` / `os.ReadDir` only — `tasks/` / `archive/` / `events/` / task JSON are never written, no task state is ever changed. The board sits on top of live queue data; any write would corrupt the real queue. The single exception is the board's **own view state**: `POST /api/project/archive` writes `~/.claudego/board_archive.json` (project collapse state, see "Project archiving" below) — it takes no part in scheduling, is never read by runner/tick/patrol, and deleting it loses no queue data. Every GET path remains write-free.
- **127.0.0.1 only**: responses contain full prompt text, directory paths, and quota data. `-addr` can override the bind address, but the default is always the loopback; binding to a non-loopback address prints a warning — not recommended.
- **TTL cache**: task snapshots and the burndown view each have their own TTL (burndown TTL = task TTL × 3, minimum 30 s), preventing a full disk scan of tasks/ and transcripts on every request. `/api/*` endpoints are gzip-compressed (2.5 MB → 320 KB in practice).

**The overview is a horizontal rail**: one column per project, scroll sideways to switch projects, with all vertical space given to the phase/task list inside a single column (each column scrolls on its own; column height is computed from the rail's actual position, so you never get two nested scrollbars). Projects are parallel to each other, not sequential — stacking them vertically pushes the second project off-screen behind the first one's several hundred cards. Narrow screens (≤720 px) fall back to vertical stacking. Page width **follows the viewport** with no fixed cap — on an ultrawide display every extra pixel reveals a bit more of the next project, so spending that width on margins costs you a whole column.

**Status filter (hides lists, never touches readings)**: the row of status-count chips in the page header doubles as a set of toggles — click one to collapse that status's card list (the overview hides task rows; the project page drops the whole column, since a kanban column *is* a status and an empty-but-present column reads as "there are no cards in this status").

One inviolable rule: **filtering never changes any reading**. The chip counts, the progress bars, the five kind buckets and the ETA are all computed over every card, even when none of that status is rendered. If hiding "done" also dropped the progress bar, that wouldn't be filtering — it would be fabricating a snapshot the user then makes decisions from. While a filter is active a banner stays pinned under the page header stating what is hidden and reaffirming that counts are unaffected; "the backend only sent 40 of these" and "I hid some statuses myself" are reported as two separate lines, because merging them reads as the board having lost data. The filter persists in localStorage (with a thousand cards on screen, hiding "done" is routine, and re-setting it on every refresh means nobody would use it) — the pinned banner is what makes that persistence safe.

**Quota is shown as remaining**: the headline reading in both the top quota strip and the burndown page is **remaining quota** (`BurnSource.remaining_percent`, computed server-side and clamped to [0,100]); the burndown curve descends and hitting zero means exhausted. The source data (CodexBar) reports used %, so `used_percent` is preserved verbatim in the response and shown alongside in tooltips / subtitles / the sample table — whenever both appear on screen, which one you're looking at is always labelled. The decision you make on this screen ("can I dispatch another batch?") is a direct function of what's left; "how much has burned" requires a subtraction first.

**Project override `~/.claudego/board.json`**: auto-derived project/phase blurbs are often dry — write a better one by hand if you like; missing file simply falls back to full derivation. Allowed fields: `name` / `desc` / `phases.<name>` / `goal` / `kind_rules`.

**`goal` field (CG-8 "landed progress")**: a mechanized "how far from the project goal" view, displayed **alongside** the card-based `progress_percent` (never replacing it). V1 does synthesis only — no history/trend.

> ⚠️ **The `//` lines below are documentation comments only — the actual `board.json` is strict JSON: no comments, no trailing commas.** Strip the `//` before you paste. Break the JSON and the board top strip surfaces a red `board.json invalid` banner (`OverviewResp.board_override_error`). Two degradation shapes — both surface loudly, never silently: **syntax errors** (missing commas, jsonc comments, unclosed braces) cannot be partially recovered, so the entire override drops back to auto-derivation; **field-type typos** (e.g. `"weight":"1"`, `"done_percent":"50%"`) trigger `*json.UnmarshalTypeError` — Unmarshal skips the offending field but keeps filling the rest, so `loadBoardOverride` **preserves the partial result** (other projects' name/desc/phases/goal still take effect) alongside the banner. One typo does not collectively erase the whole override — but any degradation must be surfaced.

```jsonc
"goal": {
  "statement": "Ship real usage",
  "as_of": "2026-07-23",              // human-anchored eval date; drives goal_source=manual@as_of
  "milestones": [
    {"id":"M1","title":"Design freeze","weight":1,"done_percent":100,"basis":"REVIEW Go"},
    {"id":"M4","title":"test-ready gates","weight":1,
     "evidence": {                     // if present, overrides manual done_percent
       "path":"/Users/you/.claudego/logs/check.json", // **must be absolute**; relative paths are rejected outright (no CWD/boardRoot fallback)
       "numerator":"gate_counts.pass",  // dotted path; must resolve to a JSON number
       "denominator":["gate_counts.pass","gate_counts.blocked"],
       "max_age_hours": 24              // stale → milestone marked stale + insufficient; negative rejected as config error
     },
     "basis":"ops/test-ready/check"}
  ]
}
```

Synthesis: `landed_percent = Σ(weight × done_percent) / Σweight`, shown next to `progress_percent`. Source disclosure is tagged by **what actually landed**, not by config shape (any non-`insufficient` tag below can co-exist with `partial=true` when a subset of same-class milestones failed to land — the tag itself does **not** promise the whole class resolved): `goal_source = manual@as_of` (at least one manual entry landed and no milestone was configured with evidence) / `evidence` (at least one evidence entry landed and no manual entry landed; partial-resolution of the evidence set is disclosed via `partial`) / `mixed@as_of` (both channels landed at least one entry) / `manual+degraded@as_of` (evidence was configured but not a single entry landed — manual milestones carried the synthesis; degradation **is disclosed** rather than misreported as "mixed") / `insufficient` (nothing valid landed).

**Fail-honest guarantees** (non-negotiable):
- goal missing → the frontend **hides the whole block** (no guessing);
- weight sum ≤ 0 or any weight < 0 → the whole block is marked "insufficient data", `landed_percent` is `null` (never NaN / Inf / any number); non-finite weights (`NaN`/`±Inf` — e.g. `MaxFloat64` products overflowing, or a `NaN` weight sneaking past the `<0` check because `NaN<0` is `false`) are caught by a pre-synthesis `math.IsNaN`/`IsInf` guard and mapped to the same "insufficient data" outcome (otherwise `round1(Inf)`'s int64 conversion is "implementation-defined" and the frontend renders `0%` or an astronomically negative number — worse than "no data");
- manual `done_percent` outside `[0, 100]` → that milestone is marked "insufficient data"; never render negative percentages or 250% (lesson: `round1`'s int64 truncation renders `-50` as `-49.9`, which then displays as authoritative "-49.9%");
- evidence synthesis exceeds 100% (e.g. a misconfigured pointer yields `num=30, den=[10]` → 300%) or yields a negative value (numerator or denominator is negative) → same "insufficient data" treatment; the raw 300% or -30% is never surfaced. The guards sit on the **absolute values** of `num` and `den`: `num<0` rejected, **each `den` component `v<0` rejected** (`{pass:5, blocked:10, adjustment:-3}` sums to 12>0 but the `adjustment` component is negative — sum-only guarding leaks a silent 41.7% reading), `den` sum `≤0` rejected (divide-by-zero + all-zero fallback). Guarding only `pct<0` gets bypassed by "sign cancellation" — e.g. `{pass:-9, blocked:-2}` cancels to `+81.8%`;
- **evidence.path must be absolute**: relative paths — whether resolved against process CWD or the `board.json` directory — silently fall back to same-named files (e.g. scaffolding under `~/.claudego`), so a misconfiguration reads the wrong file with zero warning. Rejecting relative paths outright is the only way to keep the provenance auditable;
- evidence file missing / older than `max_age_hours` / pointer does not resolve to a **JSON numeric** (e.g. the field is a string like `"9/21"`) → that milestone is marked "insufficient data"; the composite is computed from remaining milestones only and flagged `partial` — **evidence is exclusive once configured**; failure / staleness / bad pointer means "insufficient", **never a fallback to the manual value** (silently swapping data provenance is a form of fabrication);
- `board.json` present but syntactically invalid (jsonc comments, trailing commas, unclosed braces) → `OverviewResp.board_override_error` is populated, the red banner stays up, and **the entire override block** drops back to auto-derivation; **field-type typos** (`"weight":"1"` / `"done_percent":"50%"`, i.e. `*json.UnmarshalTypeError`) → the banner is populated too, but **the partial-fill result is preserved** (other projects' unaffected name/desc/phases/goal still apply) — one typo does not collectively erase the whole override. Neither shape is **ever silently swallowed**.

**The board only reads evidence files, never executes commands**: producing that JSON is the job of the orchestration session/card (e.g. `ops/test-ready/check`); the board only consumes what has been written to disk.

### Progress split by kind of work (`Project.kinds`)

A single project progress bar divides done cards by all cards — not wrong, but it averages three completely different kinds of work **weighted by card count**. Review and fix cards are short-lived, so their completion rate is naturally high, and they routinely make up 70%+ of the cards (one real project: 430 `design-review` cards against 800 `sequence` ones). The total bar gets dragged up to ~90% while the cards that actually land the work may be at 40%. **The total bar is optimistic in a specific direction** — exactly the "later-stage work gets underestimated" problem.

So each project also emits `kinds[]`: five buckets — **design / impl / fix / review / coord** — each reporting its own completion using the identical formula as the total (`done ÷ (total − canceled)`). Empty buckets are omitted. The total bar is **kept unchanged**: it is the only reading comparable with historical screenshots, and the one anchor that depends on no classification judgement at all. Real example: a project whose total reads 87.9% splits into design 83.3% / **impl 73.2%** / fix 95.5% / review 100%.

Classification order *is* the priority order — **structural signals first, keywords last** — and every card carries a `kind_source` stating what the verdict was based on:

| Order | Signal | `kind_source` | Bucket |
|---|---|---|---|
| 1 | first matching `kind_rules` entry in `board.json` | `override` | as specified |
| 2 | `x_role=C` / non-empty `review_of` / `type=design-review` / title prefixed 审核:／对抗复审: | `x_role`/`review_of`/`type`/`title` | review |
| 3 | `fix_round>0` / title prefixed 修复R1: (**colon required**) | `fix_round`/`title` | fix |
| 4 | `type ∈ {coordinate, progress-pull, prompt-assembly, batch}` / title prefixed 收口:／进度: | `type`/`title` | coord |
| 5 | title contains 设计/方案/规划/调研/选型/架构/评估/蓝图/草案/立项/盘点 or design/spec/rfc/roadmap/proposal/blueprint/research | `title` | design |
| 6 | nothing matched | `default` | impl |

Two deliberate, non-negotiable choices:
- **Review must be decided before fix.** Review cards inherit the reviewed card's `fix_round`; getting the order wrong silently moves hundreds of review cards into the fix bucket — impl progress looks unchanged, the fix bucket doubles out of nowhere, and nothing errors.
- **Unclassifiable cards go to "impl", not "unclassified"** (the opposite of the phase layer's "unsorted"). The distortion this feature exists to prevent is *underestimating remaining work*, so counting unclassifiable work as work still to land errs on the conservative side; a separate "unclassified" bucket would instead make impl look emptier than it is. `kind_source=default` states this plainly.

Keywords are a heuristic and will misfire; `kind_rules` is the precise escape hatch (more honest than piling more words into the keyword list, which would hurt unrelated projects):

```jsonc
"kind_rules": [
  {"match": "HB-",             "kind": "design"},   // title substring, case-insensitive
  {"match": "t0723-0304-c0d8", "kind": "coord"}     // or a full task ID
]
```

Valid `kind` values are only `design` / `impl` / `fix` / `review` / `coord`. Invalid rules are **skipped one by one** (the rest still apply — one typo doesn't take out the whole list), but every skipped rule is reported through `Project.kind_rule_error` and shown as a yellow warning on the project card — silent no-ops are fabricated readings.

### Project archiving (collapse on the overview)

Once you have enough projects, long-finished ones still occupy a column on the overview forever. The "归档 / Archive" button on the project card and project page collapses one away:

- Archive state lives in `~/.claudego/board_archive.json`; **not a single byte of any task card changes**, and scheduling, ETA and status counts are all unaffected. The top status counts still include archived projects' cards, and the page header says so explicitly ("N projects archived — the status counts below still include their cards").
- Archived projects are not laid out on the overview rail by default; the "已归档 N" toggle in the top right expands them temporarily so you can un-archive in place.
- **A new card automatically restores the project to active.** Archiving records the (card count, newest `created_at`) at that moment; if the count later grows, or a newer `created_at` appears, the project is judged to have new cards and flips back to active, with a badge and the reason ("auto-restored") on the card — omit the reason and users assume their archive click never registered. The two criteria are OR'd: count alone is fooled by "remove one, add one"; timestamp alone misses cards with a missing `created_at`.
- **Card status changes do not trigger restoration** (queued→done, running→failed all count as no change). Manual archiving means "I don't want to look at this project for now"; a known card finishing is not new information. Judging by `updated_at` would make archiving a still-running project bounce back on the very next tick.
- Auto-restoration is a **read-only derivation and is never written back**: the archive record stays put and is re-evaluated on every request. That keeps GET paths write-free and rules out "restoration failed to persist → half-archived state".
- If the state file cannot be read (corrupted), the error is **surfaced** (`archive_state_error`, yellow banner) and everything renders as un-archived; writes onto a corrupted file are **refused**. Silently treating it as "nothing archived" would make ten hand-collapsed projects reappear at once with zero explanation.

`POST /api/project/archive` is the board's only write endpoint (body `{"id":"<project id>","archived":true}`), behind three gates: POST only; `Content-Type` must be `application/json` (HTML forms cannot produce that type, which blocks cross-site auto-submitting forms); and when an `Origin` header is present its host must equal the request Host (browser cross-site fetches always send Origin). Command-line `curl` sends no Origin and is allowed through — local ops needs to be scriptable.

**Burndown view — three sources** (`/api/burn`): of these, `usage-history.jsonl` (= `usage_feed`) is the only source shared with `claudego quota`; `claude.json` and transcript scanning are board-exclusive — `claudego quota` does not read those two sources:
1. **CodexBar `claude.json`**: per-account session / weekly / opus window percentage time-series for the claude side;
2. **CodexBar `usage-history.jsonl`** (= `usage_feed`): primary (5 h) / secondary (weekly) percentage time-series for the codex side;
3. **`~/.claude/projects/*/*.jsonl` transcripts**: absolute token usage from each assistant message (four components summed equally, plus quota-weighted totals).

**"Insufficient data" semantics**: a single sample point has no computable rate; a sample older than the window it describes (e.g. a 5 h window with a 14-hour-old sample); or a reset time that has already passed — all three cases produce `verdict="insufficient data"`, and `burn_rate` / `exhaust_at` remain null. Only points within the current window period (sharing the same `resetsAt` boundary as the latest sample, with a 90 s tolerance) participate in rate fitting; no values are fabricated.

## 5-hour quota redline (reserve headroom)

To leave headroom for bursty/interactive work: when the redline is active the queue stops dispatching (multi-step tasks also yield between steps), and `-force` crosses it. Three channels, inspectable anytime with `claudego quota`:

```jsonc
// ~/.claudego/config.json
"queue_budget_tokens": 2000000,  // ① local ledger: max weighted tokens the queue may spend in the sliding 5h window; 0 disables
"redline_percent": 85,           // ②③ shared redline for percentage channels: stop when any source's usedPercent hits the line; 0 disables
"usage_feed": "/Users/you/Library/Application Support/CodexBar/usage-history.jsonl",
"usage_feed_max_age_min": 90,    // ②   a stale sample is treated as unavailable → dispatch allowed (fail-open); **0/negative reverts to default 90 min, never "trust forever"**
"oauth_usage": true,             // ③   subscription usage endpoint (third source), off by default; endpoint is undocumented
"oauth_usage_max_age_min": 15,   // ③   also acts as the **process-level cache TTL**: reused inside the 15s tick loop; 0/negative reverts to default 15 min
"oauth_usage_timeout_sec": 6,    // ③   HTTP timeout (seconds)
"oauth_usage_creds_path": "",    // ③   when set it is **hard-isolated** — only this file is trusted, no fallback to ~/.claude/keychain (for tests / custom deployments; empty = default lookup order)
"model_weights": {"default":1,"opus":5,"sonnet":1,"haiku":0.2}   // per-model weighting for the ledger
```

- ① counts **only claudego's own calls** (desktop consumption is invisible to it); its semantics are a "queue budget ceiling" — your reserve = total quota − queue budget. Run `claudego quota` for a few days to see typical consumption before setting a value.
- ② is the global view; its sample format is compatible with CodexBar's usage-history.jsonl (enable the Claude-usage probe in CodexBar). Any tool that appends one JSONL line in the same format works too.
- ③ reads `api.anthropic.com/api/oauth/usage` directly (`anthropic-beta: oauth-2025-04-20` header + reusing the OAuth access token from `~/.claude/.credentials.json` or the macOS keychain), pulling 5h-window utilization. **The endpoint is undocumented and can change without notice** — any anomaly (network / creds / HTTP 4xx-5xx / missing field / ambiguous field value / format drift) is treated as "insufficient data" → fail-open. The implementation trusts **response body only** and never parses response headers (headers are trivially forged/overwritten by intermediaries, and verification has refuted the "response headers carry unified ratelimit numbers" claim). `utilization` is measured as a 0-100 percentage domain taken as-is (real sample: the endpoint returns `31.0`, i.e. 31%, cross-confirmed by `limits[].percent=31`), `used_percent`/`percent` is likewise a 0-100 percentage domain taken as-is — **any auto-normalization is a false-trigger breeding ground** (lesson: the old heuristic turned `utilization:1`→100% and `used_percent:0.8`→80%, both locking the queue); `utilization` values in `(0,1]` are rejected as scale-ambiguous (could be an old fractional-style value or a genuine sub-1% reading — either read is unreliable) → treated as insufficient data, and values `>100` are likewise rejected as out-of-domain. When `oauth_usage_creds_path` is non-empty it is **hard-isolated** — only that file is trusted, no fallback to `~/.claude`/keychain (which avoids Windows `UserHomeDir` sneaking real credentials into what should be an isolated test/deployment). Endpoint results carry a **process-level cache** (TTL = `oauth_usage_max_age_min` or default 15 min): the 15s tick loop reuses it instead of hammering the endpoint (and, on macOS, instead of repeatedly triggering keychain prompts); if a refresh after expiry fails, the stale sample is retained and disclosed as "expired + refresh failed" so `quota` can report honestly.
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
- Sandboxing narrows by type: `sequence` (code-writing) cards run `--sandbox workspace-write`; read-only cards (design-review/crosscheck/coordinate/progress-pull) by default build a **one-shot isolated copy + `--sandbox workspace-write`** (CG-R3, per BD-36 tool-chain③ final ruling b / BD-39 addendum 2026-07-24) — the copy lands under `<root>/tmp/codex-review-work/<taskID>-<pid>-<nano>/` and carries the dirty+untracked surface so the review can actually run tests and drop fixtures for dynamic verification; the copy is torn down when the card ends, and crash residue is swept by the per-tick reconciliation (dual condition: dead pid **and** taskID not in `activeIDs`); the source repo is never write-polluted (hard semantics). The copy-build phase (probe/clone/apply/copy) runs under its own `min(step_timeout, 10min)` sub-budget (CG-R3b): git subprocesses are process-group-killed on timeout, and the copy leg re-checks the sub-budget **at every file boundary**, stopping the moment it expires — it also **never opens a non-regular file** (FIFOs/sockets/devices are skipped; symlinks are copied as links and never followed — otherwise one untracked link pointing at a writer-less pipe would block `open` forever, wedging the whole lane in a way no cancellation can undo). Whichever leg hangs, the card falls back to `read-only` and keeps going, and the event ledger records `codex_review_prepare_timeout` — degrade rather than wedge the whole lane. Set `"codex_review_sandbox": "readonly"` in `config.json` to roll back to the old read-only behavior (loses dynamic-verification power). Remote codex reviews get the same treatment: the remote mirror is itself an isolated copy, so the default is also relaxed to `workspace-write`.
- The board and logs label `[codex]` / `runner=codex`, and the emit/progress-parsing pipeline works as usual (coordinate can keep enqueuing splits even during a cooldown);
- Reasoning effort is tunable via `codex_reasoning` (minimal/low/medium/high/xhigh), passed as `-c model_reasoning_effort=…`.

**Downgrade-specific model and tier-parity rule (`codex_fallback_model`)**: when `codex_fallback` is active and a claude card is rerouted to codex, `codex_fallback_model` takes priority over the global `codex_model`. Tier-parity mapping: **opus-tier cards downgrade to the same-tier terra (o3), not the design-tier sol (GPT-5)** — design-tier doesn't go fill implementation-tier roles. Empty falls back to `codex_model`; this key only applies to the downgrade path (task `runner_pref≠codex` and not remote) — codex-primary cards and remote codex are unaffected.

**Pinned cards never fail open**: models in `no_fallback_models` (default `["claude-fable-5","fable"]`) are **never downgraded to the codex backup during a claude cooldown/redline — they queue and wait for the claude window to reopen**. Design-tier cards are quality-first; downgrading them violates the layering principle and breaks the engine independence that cross-verification requires (codex-pinned cross cards equally never fail open to claude when codex is unavailable).
