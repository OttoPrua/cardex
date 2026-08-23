# Recommended Cardex workflows: direct serial and federated management

[中文](workflows.md) | **English** · Back to [README](../README.en.md)

Cardex recommends two workflow topologies:

- **Direct serial**: advance one bounded delivery through design → development → independent review → integration → an explicit live gate.
- **Federated management**: a central management lane owns only the program dependencies and final convergence; multiple module managers each run their own design → development → independent review → module-integration loop before the program join, system integration, and final review.

Both modes use the same Cardex tasks, durable state, DAG, attempt/lease custody, write-domain exclusion, and event ledger. Federated management is not a second task board, and it does not let management sessions bypass Cardex to invoke another writer directly.

The shape adapts Maestro Flow ideas such as graph, fork, join, gate, and session expression to the existing Cardex control plane. Cardex remains responsible for durable scheduling, resource exclusion, failure disclosure, and evidence coordinates; it is not replaced by a new graph walker, silent replanner, or second state machine.

## Current enforcement, current convention, and future work

| Capability | Current state | Exact meaning |
|---|---|---|
| `depends_on` DAG | Enforced | A predecessor must have a verifiable durable `done` transition. Missing edges, cycles, malformed IDs, and bad domain bindings fail closed for the affected component while unrelated components continue |
| Explicit write domain | Enforced | Repository-relative paths are normalized first. Exact/subtree overlap in one repository, equal domain/lineage, and equal closed resources all serialize |
| Legacy compatibility | Enforced | A writing task without a write domain retains whole-repository exclusion on the same Git common dir; an upgrade does not silently widen concurrency |
| Independent reviewer role | Partly enforced | `design-review` is read-only and does not occupy a write domain. The packet/manager must still prove independence from the writer, use of the frozen candidate, and admissibility of the verdict |
| Direct/federated mode names and manager hierarchy | Recommended convention | They are not first-class Task schema fields yet. Express them with project, title, lineage, DAG, manager-wake scope, and receipts |
| Review accepted, integrated, live, user accepted | Never inferred from `done` | These are separate evidence gates. A completed Cardex task does not authorize publishing, service restart, device action, credential use, or external mutation |
| Reviewer-attempt custody | Known hardening gap | Any contradiction among attempt, producer, and lease blocks review acceptance and dependency release. The machine-enforcement roadmap below makes the full invariant explicit |

The commands on this page are therefore a safe workflow available now, not a claim that every federated concept already has a Cardex schema field.

## Read-only recovery before either mode starts

Every manager recovers current truth before dispatch instead of reconstructing it from chat memory:

1. Read active and archived cards and rule out an existing task ID, candidate, lineage, review-of edge, held successor, or equivalent packet.
2. Read the target repository, every worktree, exact base commit/tree, dirty and untracked paths, and existing branch owners.
3. Read current attempts, workspace leases, process residue, and declared write-domain/resource claims.
4. Draw the workflow DAG. Freeze shared contracts, schemas, and fixture formats before opening dependent parallel writers.
5. Give every writing lane one isolated worktree/branch, one lineage, one active writer, and closed owned paths/resources.
6. Give every review gate another read-only card and another context. A reviewer does not inherit the writer session, modify the candidate, or splice an old attempt's output into a new conclusion.
7. Name the module-integration, program-integration, and live/cutover lineages in advance. The last two stay held until their evidence and authority gates are explicit.

When duplicate lanes are found, deduplicate without losing evidence: retain the lane that already owns bytes, a lease, or an attempt; convert the later lane into read-only QA, an uncovered validation, or a queued integration role. Never let both writers continue with a plan to choose one later.

## Write-domain and resource claims

### Paths

An explicit path is a closed path relative to the task repository root:

- Valid: `internal/auth/token.go`, `internal/search`, `docs/workflows.md`.
- Invalid: absolute paths, `.` / `./x`, `..`, globs, `~`, environment variables, backslash aliases, or unstable symlink escapes.
- A directory claim owns the complete subtree. `internal/auth` conflicts with `internal/auth/token.go`.
- Linked worktrees of the same logical repository share Git identity. Moving a task into another worktree does not make the same path disjoint.

Different paths in one component may run in parallel. For example, `internal/auth` and `internal/billing` may have separate lineages; the same path may not.

### Closed resources

The current resource kinds are:

`runtime`, `database`, `profile`, `manifest`, `device`, `credential`, and `cutover`.

Use stable business coordinates, not a PID, temporary directory, or prose: for example, `database:app.primary`, `manifest:app.release`, and `cutover:app.production`. If two repositories mutate the same database, profile, device, or live window, give them the same resource ID; the shared resource serializes them even though their paths live in different repositories.

The current `WriteDomain` requires at least one path. A runtime-only or cutover-only task with no honest repository path must conservatively omit an explicit write domain (which preserves whole-repository serialization), or first define a dedicated ops/receipt output root. Do not invent a fake path merely to obtain parallel admission.

### Roles

| Role | Cardex shape | Write authority |
|---|---|---|
| manager | External management session plus a scoped manager-wake subscription | Does not write product bytes; recovers state, decomposes, dispatches, and judges evidence |
| designer | `design-review`, or a separate narrow-domain `sequence` when a design document truly must be written | Read-only by default; a design-document writer still has a separate lineage from implementation |
| writer | `sequence` plus explicit write domain | Writes only the declared paths/resources; one active writer per lineage |
| reviewer | Independent `design-review`, no write domain | Reads the frozen candidate; never edits, integrates, or becomes a replacement writer |
| integrator | Separate `sequence` plus integration lineage | Consumes accepted candidates only; never silently rewrites module semantics |
| live/cutover owner | Separate held gate plus shared-resource claim | Releases only after separate live authority and a rollback coordinate exist |

`coordinate` is a coordination role at the model-permission layer, but the current writer-exclusion read-only exemption covers only `design-review` and `progress-pull`. Do not use `coordinate` as a reviewer under the assumption that it can never occupy a writing lane.

## Mode A: direct serial

Use this mode when the goal is singular, interfaces are stable, there is normally one implementation write domain, and one independent review plus one integration gate is sufficient.

```text
design (R) -> implement (W) -> independent-review (R)
                                      |
                                      v
                                integrate (W, held)
                                      |
                              explicit live authority
                                      v
                                 cutover (W)
```

Recommended card shape:

```bash
# 1. Read-only design decision; record the returned task ID as <design-card>
cardex add -project myapp -type design-review -title "auth design" \
  -dir /absolute/path/to/myapp \
  "Freeze the goal, interfaces, non-goals, tests, and rollback boundary in a conclusion the next card can cite."

# 2. The sole writer; Ready only after <design-card> is durably done
cardex add -project myapp -type sequence -title "auth implementation" \
  -dir /absolute/path/to/myapp-worktree \
  -depends-on <design-card> \
  -write-domain-id auth-impl -write-domain-lineage auth-impl-r1 \
  -write-domain-component auth -write-paths internal/auth,tests/auth \
  "Implement only the frozen design; return exact commit/tree, changed paths, focused/full tests, and effect counters."

# 3. Independent reviewer: read-only, no write domain, bound to one exact candidate
cardex add -project myapp -type design-review -title "review auth candidate" \
  -dir /absolute/path/to/read-only-candidate \
  -depends-on <implementation-card> \
  "Review the exact commit/tree; return evidence_complete, P0/P1/P2, and ACCEPT/HELD. Do not edit the candidate."

# 4. Start the integration gate held; a done review card is not itself management ACCEPT
cardex add -project myapp -type sequence -title "integrate auth candidate" -hold \
  -dir /absolute/path/to/integration-worktree \
  -depends-on <review-card> \
  -write-domain-id auth-integration -write-domain-lineage auth-integration-r1 \
  -write-domain-component auth -write-paths internal/auth,tests/auth \
  -write-resources manifest:myapp.release \
  "Integrate only the accepted candidate; rerun mechanical gates and produce a separate integration receipt."
```

The manager reads both the review verdict and attempt custody before `release <integration-card>`. If the review command merely ended but returned HELD or incomplete evidence, or if attempt/producer state is contradictory, integration remains held.

Live/cutover is another held card and declares the applicable `runtime`, `database`, `profile`, `manifest`, `device`, `credential`, or `cutover` resources. Enqueue, review done, and integration done are not live authority.

## Mode B: federated management

Use this mode when one product contains multiple independently useful modules, each with its own backlog, long-lived manager, and user path, while the module candidates must still converge through system integration and central final review.

```text
                         central design / interface freeze (R)
                         /                 |                 \
             module A manager      module B manager      module C manager
             design -> write       design -> write       design -> write
                    -> review              -> review              -> review
                    -> integrate           -> integrate           -> integrate
                         \                 |                 /
                              program join / integration (W)
                                           |
                                  independent final review (R)
                                           |
                                owner acceptance + live gate
                                           |
                                       cutover (W)
```

### Management responsibilities

- **Central manager**: owns the program interface/authority DAG, shared-resource table, module join conditions, system integration, and final review. It does not take over module writers or replace module review.
- **Module manager**: owns only its task/project/dir-prefix scope. It may split more disjoint write domains inside the module, dispatch a writer and independent reviewer, and emit one module-acceptance receipt for the central join.
- **Execution cards**: always remain durable Cardex tasks. A management session does not bypass Cardex to run a second writer and does not treat a chat message as a task transition.
- **Independent final review**: consumes the frozen system candidate and all required module receipts, separate from every writer and integrator.

Example DAG:

| Node | Depends on | Role / claim |
|---|---|---|
| `interface-freeze` | none | central read-only design gate |
| `auth-design` / `search-design` | `interface-freeze` | module read-only design |
| `auth-impl` | `auth-design` | writer, `internal/auth` |
| `search-impl` | `search-design` | writer, `internal/search` |
| `auth-review` / `search-review` | respective impl | independent `design-review`, no write domain |
| `auth-integrate` / `search-integrate` | respective review | module integration lineage; do not release when verdict is HELD |
| `program-integrate` | both module integrations | system-integration lineage plus shared manifest/resource |
| `program-final-review` | `program-integrate` | independent final review, no write domain |
| `program-cutover` | `program-final-review` | held; shared cutover/runtime resources and separate Owner live authority required |

A module may be split again. If auth's token contract and session store truly have disjoint paths and resources, two writers may run in parallel and join at `auth-integrate`. Freeze their shared schema first. If both must write the same schema, fixture, or manifest, serialize that surface while unrelated work remains parallel.

### Where manager-wake fits

When enabled, scope every manager-wake subscription with `projects`, `task_ids`, or `dir_prefixes` to that manager's responsibility. The central manager subscribes only to the module joins, needs-owner events, and system gates it must consume.

Manager-wake is a **notification projection of committed task transitions**:

- A message carries subscription, high-water, task/event/transition/wake IDs. The manager must fresh-read the task, event, attempt, Git state, and receipt.
- Several events may coalesce into one wake. A no-delta scan must not create a model turn.
- A wake is not task truth, a review verdict, a resource lock, live authority, or permission to “dispatch another one.”
- A replay or resumed manager must deduplicate by task ID, lineage, candidate, review-of, worktree, and active attempt before creating a card.

## Mandatory failure fixture: review-attempt custody drift

Federated workflow acceptance must include this real incident shape as a blocking fixture:

1. An attempt record says `exited` while its reviewer wrapper/binary, child tests, or exact PID/PGID remains alive.
2. The same task then receives another attempt/runner.
3. The reviewer temporary worktree lease contradicts the attempt state.
4. There is no admissible semantic verdict, but late output or another dispatch could be mistaken for one.

In this shape, every module or central manager must:

- reject old output, evidence splicing, and dependency release;
- not retry, release, or replace the reviewer, and not signal a process whose exact ownership is unresolved;
- return custody to the Cardex control-plane owner for atomic scheduling/attempt-lease containment;
- require at most one active role instance and one active attempt for the role/task;
- permit terminalization only after `producerGone` is mechanically proven: the exact attempt PID/PGID and descendants are gone, runner/reviewer residue is absent, the workspace lease is acquirable, and no successor attempt exists;
- require final task, event, attempt records, candidate/source identity, and process absence to agree and remain hash-stable through a terminal quiet window;
- treat any fresh reviewer after containment as a new manager transport/recovery decision, never as a splice of the old attempt and never as review-of-review.

This does not ask a module manager to mutate production containment. It requires the opposite: fail closed and hand the incident to the Cardex owner.

The paired **recovery fixture** must preserve the same boundary. The Cardex owner invokes the supported `cardex hold` exactly once and sends no manual PID/PGID signals; the control plane atomically revokes scheduling and closes the active attempt. `terminal held / custody reconciled` is admissible only after every exact runner/reviewer/test descendant and disposable review copy is absent, the source candidate remains clean, and the readback is stable for at least a 20-second quiet window. This PASS proves containment and terminal consistency only. Old or late output stays rejected, the candidate remains **unreviewed**, and integration remains forbidden. Test the failure and recovery fixtures as a pair; a test that merely reaches held is insufficient.

## Terminal and evidence semantics

| Cardex state/event | What it proves | What it does not prove |
|---|---|---|
| `done` | This card has a durable completion transition; its DAG successors may regard the dependency as satisfied | Correct design, review ACCEPT, integrated candidate, live runtime, or user acceptance |
| `held` / `needs_owner` | Automatic progress should stop pending external judgment or repair | The fault is repaired, replacement/retry is safe, or a successor may be released |
| `failed` | This attempt/task failed under its declared policy and left evidence | Product infeasibility, permission to skip review, or permission to replace a writer without deduplication |
| `canceled` | This card is terminal and supplies no delivery outcome | The work was completed or no equivalent active writer exists |
| manager wake | A committed transition entered a subscription's notification surface | Manager consumption, review acceptance, or permission to release the next card |

Every join/gate reads at least the base and candidate commit/tree, changed paths, focused/full tests, independent-review verdict, effect counters, not-integrated/not-live state, and required rollback or compensation coordinate. Shared runtime, database, profile, manifest, device, credential, external mutation, or cutover work also requires preimage/postimage, atomicity, fresh runtime readback, and an independent release gate.

Keep “bytes authored,” “tests passed,” “Cardex done,” “integrated,” “running,” “real user path passed,” and “Owner accepted” in separate fields or receipts. Do not replace them with one percentage or one terminal status.

## What Cardex adapts from Maestro Flow

| Maestro-like idea | Cardex adaptation |
|---|---|
| graph / fork / join | `depends_on`, explicit module joins, and a fail-closed DAG; Cardex remains task authority |
| gate / eval | independent review cards, held integration/live cards, and digest-bound receipts; `done` is not automatically gate pass |
| session / manager | scoped management sessions plus manager-wake; session memory is not hidden canonical state |
| project spec / knowhow injection | frozen repository docs, contracts, templates, and evidence refs; chat memory is not canonical source |
| hooks | committed event → outbox → manager-wake as a narrow notification path; hooks neither write products nor replan tasks |
| bounded retry / recovery | Cardex attempts, leases, terminalization, hold, and Owner escalation; no silent automatic replan |
| visualization | a future workflow manifest may project a graph; the UI graph never becomes scheduler truth |

## Staged machine-enforcement roadmap

The DAG and write-domain scheduler already exist; Cardex does not need a broad new framework. Stabilize the workflow contract first, then add narrow increments:

### W1 — offline `workflow.v1` validator

Add an optional, zero-runtime-effect manifest/validator for `mode`, manager/role, task ID, parent/module, dependencies, candidate/evidence refs, write domain, review-of, integration/final/live gates. It parses and diagnoses only; it does not dispatch.

It must validate:

- closure of design→write→independent-review→integration→live-gate in direct mode;
- a module review/integration loop for every federated module, followed by program join and another final review;
- no DAG cycle/missing edge, normalized domains/paths/resources, and one writer per lineage/domain;
- a reviewer role instance different from the writer and no product write claim on the reviewer;
- live/cutover nodes held by default and never Ready without Owner authority/evidence;
- receipt/evidence references contain coordinates and digests, not prompts, output, or secrets.

### W2 — reviewer-custody validator and incident fixture

Add paired redacted fixtures. The failure side reproduces “attempt says exited, but producer/children/lease remain live, then the same task is redispatched.” The recovery side permits only a supported hold to revoke scheduling/attempt custody, waits for exact producers and the disposable copy to disappear, and completes a 20-second quiet window while leaving the review unaccepted. The validator must fail closed and prove:

- one active role instance / active attempt per role/task;
- `exited` does not imply `producerGone`; the latter requires PID/PGID identity, descendant absence, workspace lease, runner residue, and successor-attempt absence;
- no reviewer redispatch, terminal acceptance, old-output splice, or dependency release before producerGone;
- task/event/attempt/process/source hashes remain stable through a terminal quiet window before an admissible review receipt exists.
- a custody-reconciliation held receipt is not a semantic-review receipt; only a new module-manager decision may admit a fresh reviewer after recovery.

### W3 — bind the manifest to existing Cardex tasks

At enqueue/doctor/tick, add read-only or fail-closed binding of task ID, `depends_on`, write domain, role, candidate digest, and workflow node. Manager-wake projects only the committed current transition for that node. Keep legacy whole-repository serialization and avoid implicit migration.

### W4 — graph projection and controlled recovery

The board may read-only display modules, roles, forks, joins, gates, claim conflicts, and evidence maturity. Any retry, reviewer replacement, DAG rewrite, or automatic replan emits a proposal or held Owner decision; a graph executor never crosses Cardex attempt/resource/live boundaries.

## Pre-dispatch checklist

- [ ] Current Cardex/Git/worktree/attempt/lease/dirty state was recovered read-only and duplicate cards were ruled out.
- [ ] Direct or federated mode was chosen and the DAG, joins, and gates are explicit.
- [ ] Every writer has an isolated worktree, lineage, and closed paths/resources; shared interfaces were frozen first.
- [ ] Every reviewer is an independent read-only role with no write claim and no old-attempt output splice.
- [ ] Dependencies use Cardex task IDs; chat and wake messages never replace transitions.
- [ ] Attempt custody is consistent; `producerGone` and the terminal quiet window hold before review acceptance.
- [ ] Module integration, program integration, final review, and live/cutover are distinct gates.
- [ ] Shared runtime/database/profile/manifest/device/credential/cutover resources are serialized explicitly.
- [ ] Receipts name exact bytes, tests, review verdict, effects, rollback, and not-integrated/not-live boundaries.
