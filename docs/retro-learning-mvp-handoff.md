# Cardex retrospective learning MVP handoff

Last updated: 2026-08-13 23:29 +08:00

## Ownership and safety envelope

- Owner: current Codex desktop session; do not start a second writer.
- Cardex tracking card: `t0813-1212-9d17` (`held`, tracking only; must not be dispatched).
- Worktree: `/Users/ottoprua/Projects/cardex-retro-mvp`
- Branch: `codex/retro-learning-mvp`
- Remote branch: `origin/codex/retro-learning-mvp`
- Draft PR: [OttoPrua/cardex#4](https://github.com/OttoPrua/cardex/pull/4), targeting `main`; keep it Draft until the natural runtime gate below passes.
- Base commit: `29f3e9694d9501bff2d0c038dea6b39fb15cb92a`
- Tested implementation commits: `f4f4e22` (`feat(retro): freeze deterministic retrospective facts`), `effcb24` (`feat(retro): validate evidence-bound reports`), and `b50b73b` (`fix(doctor): detect launchd signing drift`).
- The main worktree `/Users/ottoprua/Projects/cardex` was already dirty before this packet (27 tracked and 8 untracked paths observed). Do not copy, clean, stage, or commit those bytes.
- The user authorized the exact production install and one Board restart on 2026-08-13. That cutover is complete. Do not change `/Users/ottoprua/.cardex/config.json` or perform another restart without a new reason; preserve the `0.0.0.0:8788` LAN/Tailscale binding.
- The user subsequently authorized direct modification of already-evidenced problems. Apply that only to reproducible, recurring/core-path, narrow, reversible defects with a focused RED and no authority expansion; keep policy/routing/promotion changes evidence-gated.

## MVP outcome

Turn retrospective input into reproducible facts before asking a model for recommendations. Prove the smallest useful path on real Cardex history, then use the result to choose the next change.

The MVP is intentionally not a knowledge base, automatic policy editor, DAG engine, intent classifier, or automatic promotion system.

Reusable conclusions and the direct-fix decision rule are maintained in
[`docs/retrospective-development-lessons.md`](retrospective-development-lessons.md).

## Confirmed starting point

- `retro_every_n_done` is currently `10` in the live Cardex config.
- Existing `retro.go` increments a durable watermark, enqueues a read-only `progress-pull`, and keeps recommendations proposal-only.
- Existing `templates/retro.md` asks the model to select recent cards and calculate all metrics itself.
- The latest observed report, `retro-697`, selected 10 canceled cards and marked every card's cost unavailable. This is useful evidence, but the cohort selection and arithmetic are not independently reproducible from the report alone.
- Earlier `retro-77` already produced a high-ROI fix: terminal cost telemetry was added to previously uncovered early-exit paths. That validates focusing on retrospective evidence before broader orchestration work.

## Current implementation decision

Implement one deterministic facts compiler shared by:

1. a read-only CLI preview against an explicit data root, so the current session can validate real history without changing live state; and
2. automatic retrospective task creation, which freezes the selected cohort and facts into the task prompt before the model interprets them.

Core statistics should come from Go code. The model may explain the facts and propose at most three changes; it must not recalculate or silently replace them.

## Implemented candidate

- Added the read-only `cardex retro -root D -n N -watermark W` preview command.
- Added `cardex.retro_facts.v1`, including an exact done cohort, per-card context, counts, cost coverage, review-verdict coverage, limit events, structured gaps, and a SHA-256 over the facts JSON.
- Automatic retrospective enqueue now compiles and freezes the same facts JSON/hash in the task prompt and records the hash on the queued event.
- Cohort selection now uses `status=done` plus the final `done` event timestamp, excludes retrospective tasks, reads active and archived cards, and uses `updated_at` only as a disclosed legacy fallback.
- The runner emits `done` before its final task-file save. The current in-memory done card is therefore overlaid during compilation so the card that triggers a retrospective cannot be omitted from its own cohort.
- The model template no longer scans files or performs arithmetic. It produces conclusions, at most three recommendations, and deferred edges from the frozen facts.
- New retrospective tasks persist the facts SHA-256 and exact cohort on the task. Progress publication validates schema/hash/cohort, requires cohort-scoped evidence for every conclusion/recommendation, and enforces the three-recommendation cap. Invalid output fails closed and marks the retrospective task `failed`; legacy cards without frozen metadata remain compatible.
- The live data root currently has a legacy local retrospective template. New code checks its contract before use; incompatible local templates fall back to embedded v2 with an event/stderr disclosure and are never overwritten. Compatible local custom templates remain supported.
- The template explicitly states that Cardex `done` means runner completion, not semantic success. Missing review verdicts remain missing rather than being inferred from titles.
- Public Chinese and English guides describe the new boundary and preview command.

## Acceptance criteria

- Same on-disk input produces byte-stable semantic JSON (timestamps from source data only).
- Cohort task IDs and selection rule are explicit.
- Cost gaps remain gaps and are never coerced to zero.
- Missing or malformed card/event data is disclosed without crashing the whole report.
- Automatic retrospective remains read-only and proposal-only.
- Focused tests pass, then `go test ./...` and `go build ./...` pass.
- A real-history run under `/Users/ottoprua/.cardex` is captured in this document with exact command, result summary, and limitations.

## Deferred until the MVP has runtime evidence

- Learning-candidate ledger, semantic deduplication, contradiction handling, promotion receipts, and policy canaries.
- Front-of-pipeline intent extraction.
- Generic workflow/DAG or Team/Wave/Swarm orchestration.
- Rare legacy event variants that do not affect the selected real-history window; record exact examples below instead of widening the first implementation.

## Running log

- 2026-08-13 12:12 +08:00 — Read-only recovery complete. Created isolated worktree and held tracking card. No production config or service changes.
- 2026-08-13 12:15 +08:00 — Clean-base `env GOCACHE=/tmp/cardex-retro-mvp-go-cache go test ./...` passed. The default macOS Go cache is outside the managed sandbox, so all recorded test commands use this isolated cache.
- 2026-08-13 12:16 +08:00 — Highest-ROI live defect confirmed: `retro-697` says "10 done" but selected 10 canceled cards because the prompt used archive file modification time rather than done evidence.
- 2026-08-13 12:18 +08:00 — Focused RED established for done-only event selection and byte-stable facts; implementation turned it GREEN.
- 2026-08-13 12:21 +08:00 — First real-history run selected the correct done cohort but emitted gaps for hundreds of unselected legacy cards (~52 KB output). Added a failing regression test, then restricted event/data gaps to the selected cohort.
- 2026-08-13 12:24 +08:00 — Second real-history run showed that `done` includes PASS, READY, BLOCK, and BLOCKED summaries. Added the explicit semantic boundary and structured review-verdict coverage rather than parsing titles.
- 2026-08-13 12:27 +08:00 — Tracking card `t0813-1212-9d17` rechecked as `held`. Production binary/config/service remain unchanged.
- 2026-08-13 12:30 +08:00 — Pre-commit source walk found the runner order `emit done → trigger retro → final save`. Added a failing integration regression test, then passed the in-memory terminal card as a facts-only overlay. Focused test turned GREEN.
- 2026-08-13 12:33 +08:00 — Final full suite, build, vet, diff check, and real-history hash verification passed.
- 2026-08-13 12:35 +08:00 — Created tested implementation commit `f4f4e22`; not pushed, merged, installed, or activated.
- 2026-08-13 12:39 +08:00 — Added the minimal report-publication fence after RED tests proved drifted hash/cohort/evidence and four recommendations were previously accepted. Invalid new-style reports now fail closed; no learning ledger or policy mutation was added.
- 2026-08-13 12:44 +08:00 — Deployment walk found `/Users/ottoprua/.cardex/templates/retro.md` lacks the v2 facts contract. Added RED/GREEN coverage for non-destructive embedded fallback plus continued use of compatible local customization.
- 2026-08-13 12:47 +08:00 — Full suite and mechanical gates passed after the report/template fences; created tested implementation commit `effcb24`. Still not pushed, merged, installed, or activated.
- 2026-08-13 13:19 +08:00 — Recovered the exact production envelope before cutover: installed SHA-256 `e803020640fb94118d69000b4bd2673bb0bfba75b7282ce1c7a7453e9a819a4e`, Board PID `97428`, health `0.10.0`, bind `0.0.0.0:8788`, retrospective counter `702/697`, and no queued/running cards.
- 2026-08-13 13:20 +08:00 — Rebuilt commit `70ebe2041222cf114401d820701a2a07a6240c69` to SHA-256 `545bd5730e51f9641a785465c280cdf4248ba5783ec98709c373a979a7378c71`; code signature verified. Backed up the old production binary before replacement.
- 2026-08-13 13:21 +08:00 — Installed the verified candidate through a new inode and restarted only `com.cardex.board`. New PID `74082` listens on `*:8788`; `/api/health` returns `{"ok":true,"version":"0.10.0"}`. A production-path `cardex retro` read returned the unchanged facts hash `dc13e961c205584fd675f4bc50c60501ee65a10dec2074a1f191385ee5f5c976`.
- 2026-08-13 13:22 +08:00 — Verified `com.cardex.tick` still invokes `/opt/homebrew/bin/cardex run --quiet --root /Users/ottoprua/.cardex` every 300 seconds, with last exit code 0. Natural runtime acceptance remains pending because the next threshold is 707 and the live counter is still 702.
- 2026-08-13 13:24 +08:00 — Created current-thread heartbeat `cardex-mvp` at an hourly cadence. It is restricted to read-only watermark/report checks until a natural trigger, then owns exact v2 report acceptance and pauses itself after closure.
- 2026-08-13 13:24 +08:00 — The first scheduled tick after binary replacement exposed `last exit reason = OS_REASON_CODESIGNING` and `needs LWCR update`. Board health alone was therefore not sufficient deployment evidence.
- 2026-08-13 13:26 +08:00 — Backed up the tick plist, then ran the repository-required `cardex install-launchd` refresh with the unchanged 300-second interval and data root. Its immediate RunAtLoad completed with exit code 0; the signing error and LWCR warning disappeared. Counter/task state remained unchanged at `702/697` and `t0812-1747-df1a`.
- 2026-08-13 13:35 +08:00 — User authorized direct fixes for already-evidenced defects while retaining the minimal-MVP/high-ROI rule. Quantified 54 legacy reports/158 recommendations, but classified their canceled/archive-heavy sampling as biased rather than turning those counts into global policy.
- 2026-08-13 13:37 +08:00 — Added a focused RED showing doctor had no parser for `OS_REASON_CODESIGNING` or `needs LWCR update`; the test failed to compile on the missing behavior as intended.
- 2026-08-13 13:40 +08:00 — Minimal GREEN added: on macOS, doctor now verifies the loaded `com.cardex.tick` job and rejects the two known signing-policy drift markers. An idle periodic timer with last exit 0 remains healthy. A freshly built candidate correctly passed the live launchd check; doctor still exited 1 for the unrelated, pre-existing Gemini-auth gap.
- 2026-08-13 13:40 +08:00 — Added the durable lessons/checklist document and updated both README quick starts to require launchd re-registration after binary replacement. Full regression, commit, and production replacement remain pending at this log point.
- 2026-08-13 13:42 +08:00 — Full regression passed: `go test ./... -count=1` → `ok cardex 53.963s`; `go vet ./...`, macOS build/signature verification, Windows amd64 test-binary cross-compile, and `git diff --check` all exited 0. Committed exact candidate `b50b73bc3b8813ac24cc1a8b0ce01910b0e926df`.
- 2026-08-13 13:44 +08:00 — Backed up the prior retrospective-MVP production binary, installed `b50b73b` through a verified new inode, re-registered `com.cardex.tick`, and restarted only `com.cardex.board`. Installed SHA-256 is `9e296b0a80fb6b0a985c5c56b98c05581d9c1a2eb2bb63b21171bbae4dbbb8dd`.
- 2026-08-13 13:45 +08:00 — Production verification passed: Board PID `87702`, bind `*:8788`, health `0.10.0`; fresh tick RunAtLoad exit 0 with no signing/LWCR marker; doctor reports launchd can execute the current binary. Doctor's overall exit remains 1 solely because the pre-existing Gemini authentication check is not ready; no credential/config change was made. Production retro facts remained hash/cohort stable and the natural counter remained `702/697`.
- 2026-08-13 14:23 +08:00 — Heartbeat read-only check: counter remains `done_total=702`, `triggered_at=697` (delta `0/0` from the acceptance baseline), and `last_retro_task=t0812-1747-df1a`. Cardex remains 30 done / 43 held with no queued or running cards; tracking card `t0813-1212-9d17` remains held. No task or event file newer than the 13:45 production verification was found. Natural `707` trigger remains five genuine business completions away; no action taken.
- 2026-08-13 15:23 +08:00 — Heartbeat read-only check: counter remains `702/697`, with no change from the 14:23 observation; `last_retro_task` is still `t0812-1747-df1a`. Cardex remains 30 done / 43 held, queued/running are both zero, and tracking card `t0813-1212-9d17` remains held. No task or event file newer than 14:23 was found. Natural `707` trigger remains five genuine business completions away; no action taken.
- 2026-08-13 16:23 +08:00 — Heartbeat read-only check: counter remains `702/697`, unchanged from 15:23, and the latest retrospective remains `retro-697` (`t0812-1747-df1a`). Cardex remains 30 done / 43 held with no queued or running cards; tracking card `t0813-1212-9d17` remains held. No task or event file newer than 15:23 was found. Natural `707` trigger remains five genuine business completions away; no action taken.
- 2026-08-13 17:27 +08:00 — Heartbeat read-only check: counter remains `702/697` and `last_retro_task=t0812-1747-df1a`, so no natural retrospective has triggered. Business activity resumed after 16:23; by the final read Cardex was 30 done / 43 held / 7 queued and zero running. Observed new ledgers repeatedly reached `dispatched` then `retry → queued` with `codex_error: exit status 1`; one initial review card (`t0813-1724-4ddb`) was archived while successors appeared. Tracking card `t0813-1212-9d17` remains held. The `707` gate remains five completed business cards away; this heartbeat made no mutation or implementation change.
- 2026-08-13 18:24 +08:00 — Heartbeat read-only check: counter advanced naturally from `702/697` to `705/697` (`+3` done events), while `last_retro_task` remains `t0812-1747-df1a`; therefore no v2 retrospective exists yet. The three new done events were `t0813-1734-7fef` (measured `$0.045433205` / 27 turns), `t0813-1728-ecbd` (usage unavailable; later canceled), and `t0813-1735-fa67` (usage unavailable). Cardex's final task snapshot was 31 done / 52 held with no queued or running cards; the difference between done-event count and current done-card count is preserved rather than normalized. Tracking card `t0813-1212-9d17` remains held. The `707` gate is now two genuine business completions away; no mutation or implementation action taken.
- 2026-08-13 19:24 +08:00 — Heartbeat read-only check: counter remains `705/697`, unchanged from 18:24, and `last_retro_task` remains `t0812-1747-df1a`. Cardex remains 31 done / 52 held with no queued or running cards; no task, event, or `retro-*` progress file newer than 18:24 was found. Tracking card `t0813-1212-9d17` remains held. The `707` gate remains two genuine business completions away; no action taken.
- 2026-08-13 20:27 +08:00 — Heartbeat read-only check: counter remains `705/697`, unchanged from 19:24, and `last_retro_task` remains `t0812-1747-df1a`. Cardex remains 31 done / 52 held with no queued or running cards; no task, event, or `retro-*` progress file newer than 19:24 was found. Tracking card `t0813-1212-9d17` remains held. The `707` gate remains two genuine business completions away; no action taken.
- 2026-08-13 21:27 +08:00 — Heartbeat read-only check: counter remains `705/697`, unchanged from 20:27, and `last_retro_task` remains `t0812-1747-df1a`. Cardex remains 31 done / 52 held with no queued or running cards; no task, event, or `retro-*` progress file newer than 20:27 was found. Tracking card `t0813-1212-9d17` remains held. The `707` gate remains two genuine business completions away; no action taken.
- 2026-08-13 22:27 +08:00 — Heartbeat read-only check: counter remains `705/697` and `last_retro_task=t0812-1747-df1a`; no v2 retrospective has triggered. Three real review attempts appeared after 21:27: `t0813-2224-1977` (OpenCode K3) and `t0813-2225-c784` (Kimi CLI K3) each dispatched, retried on exit status 1, then held/canceled without usage; successor `t0813-2226-123d` followed the same dispatch/retry path and remains held. Cardex is now 31 done / 53 held with no queued or running cards. Tracking card `t0813-1212-9d17` remains held. The `707` gate remains two completed business cards away; no mutation or implementation action taken.
- 2026-08-13 22:36 +08:00 — Published `codex/retro-learning-mvp` to `origin` and opened Draft PR [#4](https://github.com/OttoPrua/cardex/pull/4) against `main`. Creation was independently verified as `OPEN`, `isDraft=true`, base `main`, head `ce131a0767642a3dd8cf95d4307cc3e413193aa3`, after GitHub returned one transient push error, a connector `403`, and a CLI GraphQL `502`; the REST/PR read confirmed that the retry had succeeded, so no duplicate branch or PR was created. Runtime acceptance remains open at `705/697`, and the tracking card remains held.
- 2026-08-13 23:29 +08:00 — Heartbeat read-only check: counter advanced naturally from `705/697` to `706/697` (`+1` since 22:27, `+4` from the `702/697` acceptance baseline), while `last_retro_task` remains `t0812-1747-df1a`; therefore no v2 retrospective exists yet. The new completion is business review card `t0813-2234-0c2f`: Kimi CLI K3 dispatched at 22:34, emitted final `step_ok` then `done` at 22:44, with 5 turns and recorded `cost_total=0`. Cardex is now 32 done / 52 held with no queued or running cards; tracking card `t0813-1212-9d17` remains held. The natural `707` gate is one completed business card away. No production, counter, template, service, or task-state mutation was made.

## Production activation and rollback evidence

- Installed binary: `/opt/homebrew/bin/cardex`
- Installed candidate commit: `b50b73bc3b8813ac24cc1a8b0ce01910b0e926df`
- Installed SHA-256: `9e296b0a80fb6b0a985c5c56b98c05581d9c1a2eb2bb63b21171bbae4dbbb8dd`
- Recoverable backup: `/Users/ottoprua/.cardex/backups/cardex-0.10.0-pre-retro-mvp-20260813T131933+0800`
- Backup SHA-256: `e803020640fb94118d69000b4bd2673bb0bfba75b7282ce1c7a7453e9a819a4e`
- Immediate pre-doctor-fix backup: `/Users/ottoprua/.cardex/backups/cardex-0.10.0-pre-doctor-b50b73b-20260813T134349+0800`, SHA-256 `545bd5730e51f9641a785465c280cdf4248ba5783ec98709c373a979a7378c71`
- Board service: `com.cardex.board`, PID `87702`, bind `0.0.0.0:8788`, health version `0.10.0`
- Scheduler service: `com.cardex.tick`, re-registered against the installed inode, 300-second interval, fresh RunAtLoad exit code 0
- Scheduler plist backup: `/Users/ottoprua/.cardex/backups/com.cardex.tick.plist-pre-retro-mvp-20260813T132522+0800`, SHA-256 `3f2995f557dc7418399f6bf84e084906ee815273c3819d66ae5bcaeb0f1541b2` (identical to the regenerated plist)
- Continuity monitor: Codex heartbeat `cardex-mvp`, `ACTIVE`, hourly, attached to the current thread
- Cutover did not modify the live config, retrospective counter, task state, or local retrospective template.
- Rollback procedure: copy the backup to a new temporary inode under `/opt/homebrew/bin`, verify its SHA-256 and code signature, atomically move it to `/opt/homebrew/bin/cardex`, restart `com.cardex.board`, re-run `cardex install-launchd -root /Users/ottoprua/.cardex -interval 300` to refresh launchd's inode/signing policy, then re-check PID, `*:8788`, health, and a fresh scheduler exit code. Do not overwrite in place on macOS.

Runtime acceptance gate:

1. Wait for five real business-task completions; do not edit `retro_counter.json` or create synthetic completion cards.
2. Confirm `triggered_at >= 707` and `last_retro_task` changes from `t0812-1747-df1a`.
3. Verify the new retrospective task used the embedded v2 template fallback, contains a frozen `cardex.retro_facts.v1` hash/cohort, and publishes only a schema/hash/cohort/evidence-valid report with at most three recommendations.
4. Keep the tracking card held until that report is inspected. A healthy service or successful enqueue alone is not runtime acceptance.

## Real-history evidence

Exact candidate command:

```bash
/tmp/cardex-retro-mvp-cardex retro -root /Users/ottoprua/.cardex -n 10 -watermark 697
```

Final facts SHA-256: `dc13e961c205584fd675f4bc50c60501ee65a10dec2074a1f191385ee5f5c976`. Two immediate independent reads returned the same hash.

Selected cohort, newest first:

1. `t0813-1212-3600`
2. `t0812-1930-d456`
3. `t0812-1905-df99`
4. `t0812-1824-cbf1`
5. `t0812-1727-dff6`
6. `t0812-1720-d03d`
7. `t0812-1720-2d53`
8. `t0812-1720-8a92`
9. `t0812-1655-27c8`
10. `t0812-1711-0231`

Recomputed facts:

- Cohort/event integrity: 10 selected, 10 event ledgers, 10 done events, no timestamp fallback.
- Work mix: 7 sequence, 3 design-review; 5 local Codex, 5 remote `qmthost`; all 10 xhigh.
- Fix rounds: 7 at round 0, 1 at round 1, 2 at round 2.
- Structured review evidence: 3 review cards, 2 structured verdicts, both `block`; `t0812-1930-d456` has no structured verdict and is disclosed as a gap.
- Cost evidence: 0/10 available; all ten terminal events explicitly report `no_usage_recorded`. No model/runner cost comparison is authorized from this cohort.
- Semantic summaries include PASS, READY, BLOCK, and BLOCKED despite every selected card having Cardex status `done`; therefore `done` must not be reported as success.

Runtime-backed conclusion:

1. The cohort-selection bug was the immediate high-ROI defect and is closed in the candidate.
2. The candidate now provides enough contextual evidence for a useful human/model conclusion without letting the model invent arithmetic.
3. It is not currently safe to optimize model or runner choice by cost; the measured coverage is 0/10.
4. Two structured review blocks and three cards beyond round 0 are a useful candidate signal, but this window is dominated by one Trading delivery cluster. Do not generalize a global policy from this single correlated cohort.

## Final verification

- `env GOCACHE=/tmp/cardex-retro-mvp-go-cache-full2 go test ./...` → exit 0, `ok cardex 56.976s`.
- `env GOCACHE=/tmp/cardex-retro-mvp-go-cache-build go build ./...` → exit 0.
- `env GOCACHE=/tmp/cardex-retro-mvp-go-cache-vet go vet ./...` → exit 0.
- `git diff --check` → exit 0.
- Rebuilt `/tmp/cardex-retro-mvp-cardex`; a final real-history read returned the same facts hash `dc13e961c205584fd675f4bc50c60501ee65a10dec2074a1f191385ee5f5c976`.
- After `effcb24`: `env GOCACHE=/tmp/cardex-retro-mvp-go-cache-full4 go test ./...` → exit 0, `ok cardex 55.264s`; build, vet, and `git diff --check` also exited 0. Two fresh real-history reads again returned the same hash.

## Deferred edge cases discovered

- Older done cards without event ledgers use `updated_at` fallback and disclose `done_event_missing`; they do not affect the observed newest-ten window.
- Malformed task/event files are skipped with structured gaps. No such file was present in the selected real cohort.
- Cross-check `x_role=C` verdict recovery from old progress-only records is not implemented. Add it only if a real selected cohort shows material missing coverage.
- Cost/turn telemetry for Codex and remote runners is 0/10 in the real window. This is frequent and blocks cost-based routing, but provider usage extraction is a separate packet because guessing subscription cost would be worse than retaining an explicit gap.
- The candidate has replaced the production Cardex binary and passed immediate service/CLI verification. It has not yet observed a naturally triggered retrospective report; that report remains the only open runtime gate.

## Resume checklist

1. Confirm `git status --short --branch` in this worktree.
2. Confirm Cardex card `t0813-1212-9d17` is still held.
3. Read this document and the latest commit before editing.
4. Re-run focused tests before interpreting any real-history result.
5. Check the live counter against the 707 threshold and inspect the new retrospective task only after a natural trigger.
