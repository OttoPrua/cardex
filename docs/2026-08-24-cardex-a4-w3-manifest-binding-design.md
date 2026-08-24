# 2026-08-24 · A4 W3 — manifest ↔ task binding: architecture & contract design

Design packet `CARDEX-A4-W3-DESIGN-R5-P1` (federated plan R5). **Design doc
only — no code in this packet.** Implementation starts only after PR #11 (W2
reviewer-custody validator) integrates into `main`.

| Item | Value |
|---|---|
| Writer runtime model | `claude-fable-5` (Fable 5). Self-reported from the runtime system configuration; no thinking-budget suffix is claimed. |
| Base | `main` `a4a3acf692dd49a589210093047824591d0270b3` |
| Branch | `cursor/cardex-a4-w3-design-9d89` |
| Owned path | `docs/2026-08-24-cardex-a4-w3-manifest-binding-design.md` (this file, the only file) |
| W2 reference (read-only) | PR #11 frozen repair `e795e898b4e8edc90f9a6cb9e8b51bf446c5d8d0` (tree `4475340d0f2892c79eb0a264064daa7d463699ef`), branch `cursor/cardex-w1w4-first-packet-9d89` |
| Charter | `docs/workflows.md` § "W3 — manifest 与现有 Cardex task 绑定" |
| Revision | R2 — narrow repair of the 2026-08-24 local design gate P1s: P1-1 held role occupants (§2.1, §2.2 check 1, §2.2.1, §2.3, §3, §4 R8–R10), P1-2 init durability (§2.4, §3, §4 R11). No other section's semantics changed. |
| Review status | **Not self-reviewed.** Fresh independent re-review of revision R2 required before implementation adopts this contract. |

## 0. One sentence

W3 makes the agreement between a durable workflow record
(`cardex.workflow.v1`, the *manifest*) and the Cardex task cards it names a
**machine-checked, fail-closed, read-only binding** at three surfaces —
enqueue, doctor, tick — while leaving every card without a `workflow_id`
byte-for-byte on today's legacy whole-repo serialization, with **no implicit
migration**.

## 1. What already exists (anchors on base `a4a3acf`)

W3 adds no new state machine. It binds together machinery that is already on
`main`:

| Anchor | File | Role in W3 |
|---|---|---|
| `WorkflowRecord` (`cardex.workflow.v1`), role slots `WriterTaskID` / `ReviewerTaskID` / `IntegrationTaskID`, `Candidate{Commit,Tree}`, `WriteDomain` | `workflow.go` | The manifest side of the binding. `scanWorkflows` already reports unloadable records; `auditWorkflowWriteDomains` already fail-closes on them. |
| `Task.WorkflowID`, `Task.IntegrationGate`, `Task.DependsOn`, `Task.WriteDomain`, `Task.ReviewOf`, `Task.LastCommittedTransitionID` | `task.go` | The task side. `WorkflowID` is today only ever set by `cardex workflow` subcommands; `cardex add` has no flag for it. |
| `AnalyzeDependencyDAG`, `BindDependencyDomains` | `dependency_dag.go` | Closed DAG diagnosis: duplicate nodes, dangling edges, SCC cycles — already deterministic and fail-closed per component. |
| `eligible`, `liveDAGReadyIDs`, `taskDurablyDone`, `writerConflictsWithActive`, `legacyWritersShareBoundary` | `dispatch.go`, `multilane_runtime.go` | Tick-side readiness and the **legacy whole-repo serialization** (same git common dir ⇒ writers serialize) that W3 must preserve untouched. |
| `evaluateIntegrationRelease`, `integrationGateAllows`, `candidateIdentitiesMatch`, hold-reason vocabulary | `workflow_gate.go`, `workflow.go` | The existing read-only consult pattern W3 copies: tick asks, never mutates. |
| `admitWorkflowWriter` / `admitWorkflowReviewer` / `admitWorkflowRepair`, `createHeldIntegrationTask`, `syncIntegrationGate`, `freezeWorkflowCandidate` → `verifyWorkflowCandidate`, `tryReleaseWorkflowIntegration`; `taskIsLive` vs `taskIsDispatchable`, `workflowActiveRole` | `workflow_loop.go` | The `cardex workflow` verbs — W3's enqueue surface — and the liveness predicates: admission dedupes over `taskIsLive` (which **includes** `held`), tick over the narrower `taskIsDispatchable`. §2.2 check 1 must use the former. |
| `handleReviewVerdict` fix loop: queued repair cards each round, and the held escalation shell past `max_fix_rounds` — both inherit `WorkflowID`/`WriteDomain` without touching the record slots | `runner.go` | The one existing path that mints workflow-carrying cards outside `cardex workflow` verbs; §2.2.1 defines how the binding surfaces them. |
| `projectWakeAfterCommitted` → `committedWakeEligible` → `wakeEventStillCurrent`; `wakeEventID(task,seq,transition)`; `outboxRowIdentityConsistent`; `backfillWakeForTask` | `manager_wake.go` | Manager-wake already projects **only** a task's committed *current* transition (`ev.TransitionID == LastCommittedTransitionID`), even on backfill. W3 pins this as the bound-node invariant; it does not change the outbox schema. |
| `cmdDoctor` | `main.go` | Existing read-only diagnosis surface W3 extends. |

### W2 semantics referenced read-only (frozen `e795e898`)

`workflow_custody.go` at `e795e898` defines: custody receipts
`cardex.custody.v1` under `control/custody/`, the 20-second quiet window,
receipt kinds `admissible_review` / `custody_reconciled_held`, and hold
reasons `custody_drift` / `custody_quiet_window` /
`custody_receipt_not_semantic`. A `done` review terminal requires at least
one attempt record, a committed terminal transition, and that transition
naming a present attempt record; time ordering is parsed-time, and an
unparseable stamp is drift.

**W3 does not re-implement, wrap, relax, or extend any of this.** The
division of labor is:

- **W2 proves process custody**: producer provably gone, no successor
  attempt, evidence hash stable through the quiet window.
- **W3 proves static identity agreement**: the IDs, roles, edges, domains,
  and digests written on the manifest and on the cards name each other and
  nothing else.
- The integration gate composes both; neither subsumes the other. A perfect
  W3 binding with drifted custody stays held (`custody_drift`); a perfect
  custody receipt on a mis-bound card stays held (W3 reason codes below).

W3 implementation must not modify `workflow_custody.go`,
`workflow_custody_test.go`, integration scenario 35, or any other PR #11
surface; it builds on post-merge `main` and consumes custody results only
through `evaluateIntegrationRelease`.

## 2. Binding schema

### 2.1 The manifest is the existing record; the binding is derived, never stored

There is **no new durable schema and no second truth source**. The manifest
side stays `cardex.workflow.v1`; the task side stays the existing task JSON.
W3 introduces one pure, deterministic derivation (implementation sketch:
`workflow_binding.go`):

```go
// read-only; recomputed on every call; never persisted as authority
func analyzeWorkflowBinding(root string, cfg *Config) (WorkflowBindingDiagnosis, error)
```

Its report may be printed as JSON with the **report** schema label
`cardex.workflow.binding.v1` (a projection, like the progress files — not
state):

```json
{
  "schema": "cardex.workflow.binding.v1",
  "generated_at": "RFC3339",
  "workflows": [
    {
      "workflow_id": "wf0824-...",
      "mode": "serial|federated",
      "parent_id": "",
      "round": 1,
      "status": "writing",
      "verdict": "bound|blocked|broken",
      "nodes": [
        {
          "role": "writer|reviewer|integration",
          "task_id": "t0824-...",
          "task_status": "queued|running|done|held|...",
          "checks": [ {"name": "role_shape", "ok": true, "reason": ""} ],
          "verdict": "bound|broken",
          "reasons": []
        }
      ],
      "edges": [ {"from": "t...", "to": "t...", "resolves": true} ],
      "domain": {"id": "...", "lineage": "...", "paths": ["..."]},
      "candidate": {"commit": "", "tree": ""},
      "gates": {"integration": "held", "live": "held", "cutover": "held"},
      "blockers": [
        {
          "task_id": "t0824-...",
          "role": "writer|reviewer|integration",
          "task_status": "held",
          "fix_round": 4,
          "reason": "binding_held_blocker",
          "decision": "owner"
        }
      ]
    }
  ],
  "orphans": [ {"task_id": "t...", "workflow_id": "wf...", "reason": "binding_unbound_task"} ],
  "unreadable": [ {"path": "workflows/x.json", "reason": "binding_unreadable"} ]
}
```

Determinism requirements (so W4 can render it reproducibly and tests can
golden it): workflows sorted by ID, nodes in fixed role order
(writer, reviewer, integration), blockers sorted by task ID, edges and
reasons sorted, no timestamps inside per-node content (only the top-level
`generated_at`).

The workflow-level `verdict` is three-valued. `bound`: every §2.2 check
passes, `blockers` is empty, init is complete. `blocked`: identity agreement
is intact but progress requires a **named** explicit operator action — a held
occupant awaiting an owner decision (§2.2.1) or an `init_pending` record
awaiting replay (§2.4); the report names the action. `broken`: at least one
§2.2 check failed. `broken` dominates `blocked`. Node verdicts stay
two-valued; blockers are report entries, not nodes — they are live cards the
current slots do **not** name, which is exactly why they cannot appear in
`nodes`.

### 2.2 Node binding checks (the contract)

For a workflow record `wf` and the task universe (active + archive, via
`findTaskAnywhere`), each named node must satisfy **all** checks; any failure
marks the node — and the workflow — `broken` with a closed reason code. One
exception: the held branch of check 1 yields the workflow-level `blocked`
verdict of §2.1 instead of `broken`, because identity agreement itself is
intact there — what remains is a named owner decision:

1. **Resolution (both directions).** Every non-empty role slot
   (`WriterTaskID`, `ReviewerTaskID`, `IntegrationTaskID`) resolves to a
   loadable task, and that task's `WorkflowID == wf.ID`. Conversely, every
   **live** task carrying `WorkflowID == wf.ID` — live per `taskIsLive`:
   `queued`, `running`, `limit_paused`, **and `held`** — must be named by a
   role slot of the current round. The reverse scan must use the same
   liveness predicate admission uses (`workflowActiveRole` dedupes over
   `taskIsLive`), never the narrower `taskIsDispatchable` predicate tick
   uses: a held card cannot run, but it **occupies a role** for admission, so
   an unnamed held card the doctor skipped would be a permanent,
   unattributed admission blocker. Severity splits by dispatchability: an
   unnamed *dispatchable* card is `binding_unbound_task` (workflow `broken` —
   tick could run it); an unnamed *held* card is `binding_held_blocker`
   (workflow `blocked` — it cannot run, but writer/reviewer/repair admission
   refuses as duplicate until an owner decides; see §2.2.1). Historical
   terminal cards from earlier rounds (superseded writers/reviewers, status
   `done`/`failed`/`canceled`) are lineage, not orphans; every live claim
   must be named or surfaced.
2. **Role shape.**
   - writer / repair-writer: `Type == typeSequence`, no `IntegrationGate`,
     `WriteDomain` present and (after `NormalizeWriteDomain` against the
     record's repo root) **equal** to `wf.WriteDomain` (same ID, lineage,
     component, path set, resource set), `ReviewAfter == false`,
     `FixRound == wf.CurrentRound`.
   - reviewer: `Type == typeReview`, `WriteDomain == nil`,
     `ReviewOf == wf.WriterTaskID`, empty `SessionID` or one distinct from
     the writer's, no second live reviewer for the same writer.
   - integration: `Type == typeSequence`, `IntegrationGate != nil`,
     `gate.WorkflowID == wf.ID`, gate's writer/review IDs equal to the
     record's current slots (the `syncIntegrationGate` postcondition,
     re-checked rather than trusted).
3. **Edges.** For every bound card, each `depends_on` entry resolves via
   `findTaskAnywhere`, and the component containing bound cards passes
   `AnalyzeDependencyDAG` (no dangling edge, no cycle) and
   `BindDependencyDomains`.
4. **Domains.** `auditWorkflowWriteDomains` passes for the record set
   (unloadable sibling records fail the audit, as today), and the bound
   cards' claims do not overlap any other live writer claim
   (`writerClaimsConflict` semantics).
5. **Candidate digest.** When the record has a frozen candidate, the
   integration gate's `CandidateCommit`/`CandidateTree`, the record's
   `Candidate`, and (when present) the ingested review snapshot's candidate
   fields all agree per `candidateIdentitiesMatch`. When the record has *no*
   frozen candidate (fresh round after repair), no live reviewer card bound
   to a previous candidate may remain dispatchable.
6. **Readability.** Any file that must be read to prove 1–5 (record, task,
   transition, event ledger) that cannot be read or parsed is itself a
   violation — absence of evidence is never treated as absence of a claim
   (same doctrine as `auditWorkflowWriteDomains` and W2 P1-2).

### 2.2.1 Held occupants, and where the max-round escalation shell lives

Base `a4a3acf` mints exactly one legitimate held occupant outside the record
slots: when the fix loop exceeds `max_fix_rounds`, `handleReviewVerdict`
(`runner.go`) creates a held escalation shell that inherits `WorkflowID` and
`WriteDomain` precisely so `workflowActiveRole` still sees an occupant and
cannot admit a duplicate writer — but nothing writes that shell into
`WriterTaskID`. The intermediate fix-loop rounds mint queued repair cards the
same way. Under the amended check 1 both shapes are named: queued →
`binding_unbound_task`, held → `binding_held_blocker`. Nothing that can block
admission may be invisible to doctor.

**Decision: surface as a named Owner-decision blocker; do not bind the shell
into the current slot.** Rationale, in order of weight:

1. Slot-binding would make the runner a second writer of
   `cardex.workflow.v1` records. Today only `cardex workflow` verbs persist
   records (§6's single-origin doctrine), and the fix loop runs inside the
   runner for workflow and non-workflow reviews alike.
2. The shell legitimately fails writer role shape: its `FixRound` exceeds
   `MaxRounds` (and therefore `CurrentRound`), and its prompt is an
   adjudication request, not an admitted round's work order. Binding it into
   the slot would force §2.2 check 2 to carry an exemption for exactly the
   card that most needs scrutiny.
3. The blocker listing gives the owner the same information — occupant ID,
   occupied role, fix round, held reason — with zero new record writers.

Contract for every live unnamed occupant:

- **Report.** Listed under the workflow's `blockers` array with its
  shape-derived occupancy (gateless sequence card → writer; review card →
  reviewer; gate-bearing card → integration), `task_status`, `fix_round`,
  `reason` (`binding_held_blocker` when held), and `decision: "owner"`.
- **Attribution parity.** The duplicate-role refusal (`duplicateRoleErr`) and
  the blocker entry must name the same task ID: an admission refusal can
  never cite an occupant the report does not show, and the report can never
  show an occupant admission would not refuse on.
- **Owner exits are the existing explicit paths only.** Terminalize the
  shell, or route the workflow (`workflow mark`, which already tolerates held
  cards — it checks `workflowDispatchableCards`). `cardex release` of an
  unnamed occupant refuses (`binding_unbound_task`), so a blocker cannot leak
  into dispatch as a side effect of adjudication.

### 2.3 Closed reason-code enum

New codes, disjoint from the integration-gate hold-reason vocabulary and from
W2's custody reasons so no surface can shadow another:

| Code | Meaning |
|---|---|
| `binding_missing_workflow` | A card names a `workflow_id` whose record is absent or unloadable (the *missing workflow node* shape). |
| `binding_unbound_task` | A dispatchable card carries a `workflow_id` but no current role slot of that record names it; also the refusal code when requeue/`release` would make an unnamed card dispatchable. |
| `binding_held_blocker` | A held card carries a `workflow_id`, no current role slot names it, and it occupies a role for admission (`taskIsLive` dedupe): it cannot run, but writer/reviewer/repair admission refuses as duplicate on it. Owner decision required (§2.2.1). |
| `binding_init_incomplete` | A record is `init_pending` (interrupted `workflow init`, §2.4): resumable only by an init replay that reuses the recorded identities; every other verb on the record refuses. |
| `binding_dangling_role` | A record role slot names a task ID that resolves nowhere (active or archive). |
| `binding_role_mismatch` | Role shape violated (wrong type, reviewer with a write domain, `review_of` not the current writer, gate/record disagreement, shared session). |
| `binding_dangling_depends_on` | A bound card's `depends_on` names an unresolvable task ID. |
| `binding_cyclic_depends_on` | A bound card participates in a `depends_on` cycle (SCC from `AnalyzeDependencyDAG`). |
| `binding_domain_mismatch` | Bound writer/integration card's write domain is not exactly the record's normalized domain. |
| `binding_domain_overlap` | Record-set or live-claim overlap (paths per git identity, resources globally). |
| `binding_candidate_drift` | Static digest disagreement among gate, frozen record candidate, and review snapshot. |
| `binding_unreadable` | Evidence required to prove the binding could not be read; fail-closed umbrella. |

### 2.4 `workflow init` durability: intent-first, replay-safe

**Why this section exists.** On base `a4a3acf`, `cmdWorkflowInit` calls
`createHeldIntegrationTask` before `persistWorkflow`, and the helper writes
the held task (`saveTask`) and its held event durably before
`IntegrationTaskID` is even assigned in memory. A fault at the record write
(e.g. `workflows/` unwritable) therefore leaves a durable held task,
integration gate and event carrying a fresh workflow ID with **zero**
workflow records — an orphan that `binding_missing_workflow` can only
*detect* and nothing can *replay*, whose task-side write-domain claim
persists with no record behind it. A fault is not a refusal, so §3's
zero-write refusal posture does not cover it: the init write ordering itself
is part of the W3 contract.

**Durable write points, required order.** Init performs exactly four durable
steps. The implementation must keep this order and make each step
independently idempotent:

| # | Write | Content | State if the crash lands after this write |
|---|---|---|---|
| I1 | Intent (record) | Full `cardex.workflow.v1` under `workflows/` carrying the **preallocated** workflow `ID` and the **preallocated** `IntegrationTaskID`, with `status: init_pending` — a new closed value of the *existing* `status` field, not a new field, so no stored-schema change and no migration; only the new init path ever writes it | Record names a task that does not resolve yet; doctor names `binding_init_incomplete`; replay resumes |
| I2 | Task | Held integration card written under the preallocated task ID, `IntegrationGate.WorkflowID == ID` | Record and task agree; held event missing; replay emits it |
| I3 | Event | The held ledger event for that task | Everything durable but the record still `init_pending`; replay finalizes |
| I4 | Finalize | Record status flip `init_pending → design` plus the progress projection (`persistWorkflow`) — the commit point; init reports success only after I4 | Init complete |

Every identity is minted **before I1** and recorded **in I1**; no later step
mints an identity. Flag validation, engine checks and
`auditWorkflowWriteDomains` all run before I1, so the first durable write is
already an audited, non-overlapping domain claim — which also closes the
second poison shape of the base bug: from I1 on, the domain claim is always
record-backed.

**Zero-orphan invariant.** At every crash point, every durable artifact
carrying a workflow ID is reachable from a durable `cardex.workflow.v1`
record that names it. Before I1 nothing is durable; from I1 on, the record
is. A task or event with no record behind it
(`binding_missing_workflow`) can therefore only be injected, never produced
by init.

**Replay rules.**

- Re-running `workflow init` while an `init_pending` record exists for the
  same module ID and write-domain identity **resumes that record**: each of
  I2–I4 reads before it writes and writes only what is absent, using the
  identities recorded at I1. A retry never mints a second workflow ID, task
  ID, or a second held event for the same task.
- A retry whose flags disagree with the pending record's stored fields
  (goal, worktree, domain, engines, rounds) refuses and names the pending
  record (`binding_init_incomplete`); it never silently overwrites an
  intent.
- While a record is `init_pending`: every other `cardex workflow` verb on it
  refuses (`binding_init_incomplete`); its cards are never dispatched;
  doctor names the pending init and the resume action. Doctor itself never
  resumes anything — replay is an explicit command re-run, keeping doctor
  strictly read-only.

**Rejected alternative: best-effort rollback.** Keeping the base order and
deleting the task when the record write fails does not close the window: the
process can die between the durable task write and the cleanup, and the
cleanup itself can fail on the same faulted filesystem. Only
durable-intent-first ordering makes every crash point safe; rollback on top
of it is at most cosmetic.

## 3. Enforcement surfaces and fail-closed matrix

Three surfaces, one shared derivation, three different fail-closed postures.
"Fail-closed" is surface-specific and defined here precisely:

- **enqueue** — the card (or the requeue transition) is **refused**; nothing
  is written; the error names the reason code. Enqueue means: the
  `cardex workflow` admit paths (`writer`, `review`, `repair`, `init`'s held
  integration card), `try-release-integration`, and `cardex release` on a
  gated card. `cardex add` gains **no** `-workflow-id` flag in W3; binding
  creation stays exclusive to `cardex workflow` commands, so arbitrary cards
  cannot claim membership at add time. Refusal is zero-write; a **fault**
  mid-command is not a refusal and gets its own contract: init follows
  §2.4's intent-first order (on base `a4a3acf`, `cmdWorkflowInit` writes the
  held task and event before any record write — the implementation must
  invert that), so at every crash point every durable artifact stays
  reachable from a durable record and replay converges without duplicate
  identities.
- **doctor** — read-only diagnosis. Fail-closed here means: every violation
  is **named**, never skipped; unreadable evidence is itself a named finding;
  doctor never mutates, repairs, holds, or releases anything. Two forms:
  a section in `cardex doctor` (informational; exit semantics unchanged for
  existing consumers), and `cardex workflow doctor [-json]` (gate form:
  exit nonzero iff any finding — the surface scripts and CI can gate on).
- **tick** — the bound card is **not dispatched** this tick (same read-only
  consult position as `integrationGateAllows`); no mutation, no auto-hold, no
  event per tick (a metrics counter only, to avoid ledger spam). Unrelated
  cards and unrelated DAG components stay dispatchable, matching the existing
  per-component fail-closure of `liveDAGReadyIDs`.

Matrix (violation × surface → behavior, reason code in parentheses):

| Violation | enqueue | doctor | tick |
|---|---|---|---|
| Unbound task ID | Workflow admit paths cannot mint one (they always bind); a requeue/`release` of an injected unbound card is refused (`binding_unbound_task`) | Listed under `orphans` (`binding_unbound_task`) | Card skipped, never dispatched (`binding_unbound_task`) |
| Missing workflow node (record absent/unloadable) | `cardex workflow <verb>` refuses — `loadWorkflow` fails; `cardex release` on the gated card already refuses (`incomplete_evidence`); W3 extends refusal to writer/reviewer requeue (`binding_missing_workflow`) | Listed under `unreadable`/`orphans` (`binding_missing_workflow`) | All cards naming that record skipped (`binding_missing_workflow`) |
| Dangling role slot | `freeze-candidate`/`review`/`ingest-review`/`try-release` refuse when the slot they consume dangles (`binding_dangling_role`) | Node `broken` (`binding_dangling_role`) | Sibling bound cards of that record skipped (`binding_dangling_role`) |
| Dangling `depends_on` | Bound-card admit refuses at enqueue (`binding_dangling_depends_on`); legacy cards keep form-only validation | Edge `resolves:false`; node `broken` (`binding_dangling_depends_on`) | Already blocked by component DAG; W3 adds the named reason to the skip (`binding_dangling_depends_on`) |
| Cyclic `depends_on` | Bound-card admit refuses when the new edge closes a cycle (`binding_cyclic_depends_on`) | Cycle members listed (`binding_cyclic_depends_on`) | Already blocked per component; named reason added (`binding_cyclic_depends_on`) |
| Write-domain mismatch card↔record | Admit paths copy the record domain today; a mutated/injected card is refused on requeue/release (`binding_domain_mismatch`) | Node `broken` (`binding_domain_mismatch`) | Card skipped (`binding_domain_mismatch`) |
| Write-domain overlap across records/claims | `workflow init`/record edits already refuse (`auditWorkflowWriteDomains`); W3 names it (`binding_domain_overlap`) | Workflow `broken` (`binding_domain_overlap`) | Writer conflict already blocks dispatch; named reason added (`binding_domain_overlap`) |
| Role mismatch | Admit paths refuse (duplicate-role and shape checks); injected shape drift refused on requeue/release (`binding_role_mismatch`) | Node `broken` (`binding_role_mismatch`) | Card skipped (`binding_role_mismatch`) |
| Candidate-digest drift | `try-release-integration`/`cardex release` already refuse (`candidate_mismatch`); W3 doctor/tick name the static drift before release is even attempted (`binding_candidate_drift`) | Workflow `broken` (`binding_candidate_drift`) | Integration card held as today; stale reviewer bound to a cleared/changed candidate skipped (`binding_candidate_drift`) |
| Unreadable evidence | Refuse (`binding_unreadable`) | Named finding, never skipped (`binding_unreadable`) | Bound cards of the affected record skipped (`binding_unreadable`) |
| Held role occupant outside current slots (incl. the max-round escalation shell, §2.2.1) | `workflow writer`/`review`/`repair` refuse, naming the occupant's task ID (`binding_held_blocker`); `cardex release` of the occupant refuses (`binding_unbound_task`) | Listed under `blockers` as a named Owner-decision blocker; workflow verdict `blocked`, never silently `bound` (`binding_held_blocker`) | The occupant is held, hence never dispatchable; correctly bound slot-named siblings keep dispatching (identity agreement is intact); no release path can make the occupant dispatchable without passing enqueue |
| Interrupted init (`init_pending` record, §2.4) | Every verb except init replay refuses (`binding_init_incomplete`); replay resumes with the I1-recorded identities | Named finding with the resume action (`binding_init_incomplete`); workflow verdict `blocked`; injected task/event sets with zero records stay `binding_missing_workflow` | Cards of an `init_pending` record never dispatched (`binding_init_incomplete`) |

Ordering: surfaces evaluate checks in the §2.2 order and report the **first**
failure per node plus **all** failures in doctor's report form (doctor
aggregates; enqueue/tick short-circuit). Reason codes are stable strings; the
enum is closed — an unknown code is itself a bug, not a new vocabulary entry.

## 4. RED fixture matrix (acceptance-blocking, for the implementation packet)

Every fixture below must be written RED-first against the implementation
base (post-#11 `main`) and go GREEN only via the W3 implementation. Fixtures
inject broken shapes by writing task/record JSON directly into a temp root
(the technique `workflow_test.go` already uses); no fixture spawns real
engines, performs network access, or touches `~/.cardex`.

| # | Family | enqueue fixture | doctor fixture | tick fixture |
|---|---|---|---|---|
| R1 | Unbound task ID (card claims `wf`, record does not name it) | `TestBindingEnqueueRefusesUnboundTaskRequeue` — injected queued card + `cardex release` path refuses | `TestBindingDoctorNamesUnboundTask` — orphan listed with `binding_unbound_task` | `TestBindingTickSkipsUnboundTask` — never dispatched; unrelated card on same tick still dispatches |
| R2 | Dangling `depends_on` on a bound card | `TestBindingEnqueueRefusesDanglingDependsOn` | `TestBindingDoctorNamesDanglingDependsOn` | `TestBindingTickSkipsDanglingDependsOn` — component blocked, disjoint component unaffected |
| R3 | Cyclic `depends_on` among bound cards | `TestBindingEnqueueRefusesCycleClosingEdge` | `TestBindingDoctorListsCycleMembers` — SCC members, deterministic order | `TestBindingTickSkipsCycleMembers` |
| R4 | Write-domain overlap / card↔record domain mismatch | `TestBindingEnqueueRefusesDomainMismatchRequeue` | `TestBindingDoctorNamesDomainMismatchAndOverlap` | `TestBindingTickSkipsDomainMismatchedWriter` |
| R5 | Role mismatch (reviewer with write domain; `review_of` ≠ current writer; gate/record disagreement) | `TestBindingEnqueueRefusesRoleShapeDrift` | `TestBindingDoctorNamesRoleMismatch` | `TestBindingTickSkipsMismatchedReviewer` |
| R6 | Candidate-digest drift (gate C2/T2 vs frozen C1/T1; stale reviewer after repair cleared candidate) | `TestBindingReleaseRefusesCandidateDrift` (extends existing `candidate_mismatch` coverage with the binding code) | `TestBindingDoctorNamesCandidateDrift` | `TestBindingTickSkipsStaleReviewerAfterCandidateClear` |
| R7 | Missing workflow node (record file deleted/corrupt while cards remain) | `TestBindingEnqueueRefusesVerbOnMissingRecord` | `TestBindingDoctorNamesMissingWorkflow` — unreadable record is a finding, not a skip | `TestBindingTickSkipsCardsOfMissingRecord` |
| R8 | Held **writer** occupant outside the slots (injected held sequence card carrying `workflow_id`; record slot empty or naming another card) | `TestBindingWriterAdmitRefusesNamingHeldOccupant` — `workflow writer` and `repair` refuse, error names the occupant's task ID (`binding_held_blocker`); `cardex release` of the occupant refuses (`binding_unbound_task`) | `TestBindingDoctorNamesHeldWriterBlocker` — `blockers` entry with role writer, workflow verdict `blocked`, never silently `bound`; refusal ID equals the report's blocker ID | `TestBindingTickNeverDispatchesHeldBlocker` — occupant never dispatched; correctly bound slot-named sibling still dispatches |
| R9 | Held **reviewer** occupant outside the slots (injected held review card carrying `workflow_id`) | `TestBindingReviewerAdmitRefusesNamingHeldOccupant` — `workflow review` refuses naming the occupant (`binding_held_blocker`) | `TestBindingDoctorNamesHeldReviewerBlocker` — `blockers` entry with role reviewer, verdict `blocked` | `TestBindingTickSkipsHeldReviewerBlocker` |
| R10 | **Max-round escalation shell**, reached through the real fix loop (reviewer verdict drives `handleReviewVerdict` past `max_fix_rounds`), not injection | `TestBindingRepairRefusesNamingEscalationShell` — `workflow repair`/`writer` refuse naming the shell's task ID; `cardex release` of the shell refuses (`binding_unbound_task`) | `TestBindingDoctorNamesEscalationShellAsOwnerBlocker` — blocker entry carries role, `fix_round`, `decision:"owner"`; after the owner terminalizes the shell, the blocker entry disappears and the workflow stops reporting `blocked` | `TestBindingTickNeverDispatchesEscalationShell` |
| R11 | **Interrupted init** — per-write-point fault injection, one run per durable write point I1–I4 (§2.4) | `TestBindingInitFaultAtEachWritePoint` — init fails with a named error at every injected point and the zero-orphan invariant holds (no durable task/event/claim unreachable from a record); `TestBindingInitReplayReusesIdentities` — replay after the fault clears completes with the I1-recorded workflow ID and integration task ID, leaving exactly one record, one task, one held event; `TestBindingInitRetryRefusesMismatchedFlags` | `TestBindingDoctorNamesPendingInit` — `binding_init_incomplete` named with the resume action, verdict `blocked`; an injected task/event set with zero records stays `binding_missing_workflow` | `TestBindingTickSkipsCardsOfPendingInit` |

Each RED fixture must assert three things: (a) the operation refuses / the
card is not dispatched, (b) the exact closed reason code is surfaced, and
(c) **no state was mutated** by the refusal (task JSON, record JSON, event
ledger, and custody dir byte-identical before/after — doctor and tick are
read-only; enqueue refusals write nothing). R11's fault paths are the one
deliberate exception to (c): a fault, unlike a refusal, has already written
the intent, so R11 asserts the §2.4 invariants instead — zero orphans at
every crash point, identity-stable replay, no duplicates.

## 5. GREEN path

**G1 — bound manifest end-to-end (serial).** `workflow init` → `writer` →
writer done → `freeze-candidate` (digests re-derived by
`verifyWorkflowCandidate`) → `review` → reviewer done → `ingest-review`
(admissible pass; on post-#11 base this includes the W2 quiet-window receipt)
→ `try-release-integration`. At every step: `cardex workflow doctor` reports
`verdict:bound` with zero findings, enqueue admits, tick dispatches the one
eligible bound card. Fixture: `TestBindingGreenSerialRoundTrip`.

**G2 — manager-wake projects only the committed current transition of the
bound node.** With a bound manifest and wake projection enabled: the writer's
committed `done` produces exactly one outbox row whose identity is
`wakeEventID(task, seq, transition)` and whose `TransitionID` equals the
task's `LastCommittedTransitionID`; a superseded transition never projects
(including via `backfillWakeForTask`); **no** wake row exists for workflow
*record* mutations (freeze, ingest, gate sync — they are not task
transitions); replay mints no second identity. W3 changes **nothing** in the
outbox schema — the alternative of annotating rows with `workflow_id` was
considered and rejected: an annotation asserts a binding at projection time
that the manager must re-derive anyway (fresh-read discipline,
`docs/workflows.md` manager-wake section), and a stale annotation would be a
new fail-open. The G2 deliverable is the **pinned invariant**, fixture:
`TestBindingWakeProjectsOnlyCurrentCommittedTransition`.

**G3 — legacy cards untouched.** A card with empty `workflow_id` and no
write domain: never consulted by the binding derivation, never named by
doctor, dispatched exactly as today, and two such writers on the same git
common dir still serialize whole-repo (`legacyWritersShareBoundary`).
Fixture: `TestBindingLeavesLegacyCardsUntouched` plus the entire existing
test suite passing unchanged.

## 6. Legacy compatibility and no implicit migration

- Cards with empty `workflow_id` keep today's exact semantics: form-only
  `depends_on` validation at `cardex add`, component-level DAG fail-closure
  at tick, and **legacy whole-repo serialization** (same git common dir ⇒
  writers serialize) exactly as `legacyWritersShareBoundary` implements it.
- No stored schema changes: no new required fields on task JSON or
  `cardex.workflow.v1`; loaders never rewrite files they read; the binding
  report is derived output only.
- No migration command, and no implicit adoption: W3 never writes
  `workflow_id` onto an existing card, never infers membership from paths,
  projects, or prompts, and never converts a legacy card into a bound node.
  Binding membership has exactly one origin: an explicit `cardex workflow`
  command minting the card.
- `cardex add` keeps refusing nothing new for legacy cards; the stricter
  enqueue checks in §3 apply **only** to cards that carry a `workflow_id`.

## 7. W3 → W4 interface boundary

W4 (graph projection & controlled recovery, `docs/workflows.md` W4 section)
consumes W3's outputs **read-only**:

| W4 consumes | Provided by | Mutability |
|---|---|---|
| Binding diagnosis (`cardex.workflow.binding.v1` report) | `analyzeWorkflowBinding` / `cardex workflow doctor -json` | None — recomputed, deterministic, never stored |
| DAG diagnosis (ready/cycles/missing) | `AnalyzeDependencyDAG` | None |
| Gate decision | `evaluateIntegrationRelease` | None (read-only re-derivation) |
| Custody receipts | `control/custody/` (W2, frozen semantics) | None — W4 renders `Kind`/`SemanticReview`, never writes |
| Progress projections | `workflows/<id>.progress.json` | None |

Boundary rules the implementation must keep:

1. **The projection is never scheduling truth.** W4 renders what the
   derivation returns; tick consults the same derivation directly. If they
   ever disagree, the bug is in W4's rendering, by construction.
2. **No mutating API is exported to W4.** Any W4 "retry, replace reviewer,
   edit DAG, replan" is a proposal artifact or a held owner-decision card
   that routes through the existing explicit commands (`cardex workflow …`,
   `cardex hold`, `cardex release`); W3 exposes zero write entry points.
3. **Determinism is part of the contract.** Sorted output, closed reason
   codes, no embedded timestamps per node — so W4 golden-tests its rendering
   against stable fixtures.
4. **Reason codes are the shared vocabulary.** W4 must display the closed
   enum verbatim; it may not invent, translate, or merge codes.

## 8. Non-goals (this packet and the W3 implementation packet)

- No graph walker, auto-advance, auto-replan, or second state machine; tick
  still never advances a workflow.
- No re-implementation or modification of W2 custody (`workflow_custody.go`
  and its tests stay as PR #11 merged them); no change to the 20-second
  quiet window or receipt kinds.
- No live/cutover release path; both gates stay held-only.
- No board/web UI, no graph rendering (that is W4).
- No manager-wake outbox schema change; no new wake kinds; no wake for
  workflow-record mutations.
- No `-workflow-id` flag on `cardex add`; no migration of stored JSON; no
  implicit adoption of legacy cards.
- No doctor-side auto-heal: doctor names `init_pending` records and held
  blockers but never resumes, releases, or terminalizes anything; init
  replay is an explicit command re-run, and blocker exits go through the
  existing explicit commands only (§2.2.1).
- No changes to `docs/workflows*.md`, changelogs, or any `.go` file in this
  design packet (this file is the packet's entire diff).
- No merge, install, launchd, live tick, or scheduler execution performed by
  this packet.

## 9. Durable handoff (footer)

| Item | Value |
|---|---|
| Packet | `CARDEX-A4-W3-DESIGN-R5-P1` (federated plan R5) |
| Writer runtime model | `claude-fable-5` |
| Base | `main` `a4a3acf692dd49a589210093047824591d0270b3` |
| Branch | `cursor/cardex-a4-w3-design-9d89` |
| Owned path | `docs/2026-08-24-cardex-a4-w3-manifest-binding-design.md` (only file changed) |
| W2 frozen reference (read-only) | `e795e898b4e8edc90f9a6cb9e8b51bf446c5d8d0` (tree `4475340d0f2892c79eb0a264064daa7d463699ef`) on `cursor/cardex-w1w4-first-packet-9d89` (PR #11) |
| Implementation precondition | PR #11 integrated into `main`; implementation packet rebases this contract's anchors onto post-merge `main` (semantics are commit-pinned, only line anchors may move) |
| Non-goals | §8 above — binding is read-only/fail-closed; no scheduler, no custody changes, no migration, no live/cutover path, no UI |
| Revision | R2 — repairs the 2026-08-24 local design gate P1-1 (held role occupants: §2.1, §2.2 check 1, §2.2.1, §2.3, §3, §4 R8–R10) and P1-2 (init durability: §2.4, §3, §4 R11) |
| Review status | Not self-reviewed; fresh independent re-review of revision R2 required before the implementation packet adopts §2–§7 as contract |
| Commit / tree | Recorded in the PR description and branch tip (this file cannot contain its own commit hash) |
