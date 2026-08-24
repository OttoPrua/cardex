# Recommended Cardex workflows: direct serial and federated module management

[中文](workflows.md) | **English** · Back to [README](../README.en.md)

Cardex recommends two workflow topologies and ships a smallest production-capable module-management control plane:

- **Direct serial**: advance one bounded delivery through design → development → independent review → integration → an explicit live gate. The existing `review_after` / fix loop remains valid.
- **Federated module loop**: each long-horizon product module has a durable goal record and repeats Grok writer → fresh Grok adversarial review → repair rounds. Module integration is created held. A central manager only joins accepted modules, then runs final review and the live gate.

Both modes use the same Cardex tasks, durable state, `depends_on` DAG, write-domain exclusion, and event ledger. Federated mode is not a second task board. Management sessions must not bypass Cardex to dispatch a second writer, and must not use Codex/Sol as a module implementation engine.

## Current enforcement, current convention, and future work

| Capability | Current state | Exact meaning |
|---|---|---|
| `depends_on` DAG | Enforced | A predecessor must be durably `done`. Missing edges, cycles, malformed IDs, and bad domain bindings fail closed for that component; unrelated components continue |
| Explicit write domain | Enforced | Repository-relative paths are normalized first. Exact/subtree overlap in one repository, equal domain/lineage, and equal closed resources all serialize. A writing task without a write domain keeps whole-repository exclusion on the same Git common dir |
| Module workflow record | Enforced | `cardex workflow` durably binds goal/module identity, repo/worktree, write domain, terminal criteria, max rounds, candidate/review identities, effect gates, and local progress-document coordinates |
| Integration gate | Enforced | Module integration cards are created held. Both `cardex release` and tick require a machine-checked `verdict=pass` with empty `p0/p1` and matching candidate/custody evidence. `concerns`, `block`, unknown vocabulary, missing output, or incomplete evidence keep the gate held. Durable review `done` is not enough |
| Module writer/reviewer | Enforced | The module loop pins `grok-build` and rejects Codex/Sol. If `engines.grok-build` is not configured, pinned cards wait and never fail open to Claude |
| Dedupe and write-domain conflicts | Enforced | At most one active writer and one active reviewer per module. Overlapping paths/resources refuse a second writer |
| Root notify | Enforced | Only live-ready, true external dependency, Owner choice, or exhausted route write `workflows/root-notify/`. Routine progress stays in local JSON/Markdown |
| Full attempt/producer/lease custody | Known hardening gap | This tree uses terminal task status, not-running, independent reviewer, no write domain, no shared session, and no second active reviewer as the smallest custody check. PID/PGID `producerGone` and a 20-second quiet window remain a later fixture |
| live / cutover | Default held | Releasing integration is not live. Live and cutover remain separate effect gates |

Parallel federated writers need `max_parallel` greater than 1; the default remains 1. Legacy cards without a write domain keep their previous behaviour.

The only machine review vocabulary is the live template's `pass|concerns|block`. `pass` requires `p0=[]` and `p1=[]`. `ACCEPT` / `HELD` are not valid machine verdicts unless a separately reviewed adapter maps them.

## Direct serial (existing cards remain valid)

```text
design (R) -> implement (W, optional -review-after) -> independent-review (R)
                                      |
                                      v
                          integrate (W, held + integration_gate)
                                      |
                              explicit live authority
                                      v
                                 cutover (W)
```

Legacy held cards without `integration_gate` can still be `cardex release`d. Cards with `integration_gate` are refused when evidence is incomplete; tick is equally fail-closed.

## Federated module loop (`cardex workflow`)

```bash
cardex workflow init -mode federated -module auth -goal-id auth-token-v1 \
  -goal "ship an independently reviewed auth-token vertical" \
  -dir /absolute/path/to/auth-worktree \
  -write-domain-id auth-tokens -write-domain-lineage auth-tokens-lineage \
  -write-domain-component auth -write-paths internal/auth \
  -terminal-criteria "independent review pass with empty p0/p1; integration and live stay held" \
  -max-rounds 3

cardex workflow advance <id>
cardex workflow freeze-candidate <id> -commit <sha> -tree <tree>
cardex workflow ingest-review <id>
cardex workflow try-release-integration <id>
```

The loop pins `grok-build` on writer and reviewer cards (`review_after=false`; the control plane owns the review). A fresh reviewer does not inherit the writer session and must not declare a write domain. Integration stays held until an admissible pass matches the frozen candidate and custody evidence.

`grok-build` runs through `config.engines.grok-build` (this tree has no standalone Grok CLI executor). Unconfigured pins wait; they do not divert to Claude, Codex, or Sol.

## Write domains and resources

Explicit paths are closed paths relative to the repository root. A directory claim owns the subtree. Linked worktrees of one Git common dir share identity. Closed resource kinds: `runtime`, `database`, `profile`, `manifest`, `device`, `credential`, `cutover`.

## Root notify

Only `live_ready`, `true_external_dependency`, `owner_choice`, and `exhausted_route` are written under `workflows/root-notify/`. Chat hooks are not task transitions.

## Later fixtures (not claimed as executed here)

- W2: full reviewer-attempt custody (`exited` ≠ `producerGone`, quiet-window hashes).
- Native Grok Build CLI executor (engine profile or wait today).
- Machine Owner-authority gate for live/cutover.
