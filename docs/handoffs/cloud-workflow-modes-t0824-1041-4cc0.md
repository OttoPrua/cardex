# Cloud continuation: Workflow Modes implementation

This branch is a custody snapshot of the unfinished Cardex Workflow Modes
implementation from task `t0824-1041-4cc0`. It exists so a cloud development
agent can continue from exact source bytes without relying on the operator's
local worktree.

## Identity and status

- Snapshot base: `29f3e9694d9501bff2d0c038dea6b39fb15cb92a`
- Base tree: `b79e96a67c212cabb18dd0952d15c1bf30a7dac4`
- Accepted public `main` at handoff: `db0cf1077bf8dd9de321fce5bc44698ae89862ac`
- Task terminal: `held / unknown_outcome / invalid_terminal_result`
- Observed Grok events: semantic `4500`, model `4572`, tool `716`
- Source snapshot: 12 modified paths and 14 newly added paths
- Tracked binary-diff SHA-256:
  `b9a66a361290340bbb0e4bf3fa851a9039f3fe1aa22d6bbfa22ac660f8e5fdc5`

This commit is `WIP_CUSTODY_ONLY`. It is not a candidate, review result,
integration decision, install, or live release.

## Intended capability

Continue Cardex from the accepted Workflow Modes documentation into two
machine-enforced orchestration modes:

1. a direct serial design -> writer -> independent review -> integration flow;
2. federated module-goal loops with disjoint write domains, local durable
   progress, bounded rounds, module review, and a final central integration
   gate.

Unknown, incomplete, concerns, or block review terminals must never release an
integration node. Restart/replay must not create duplicate writers or
reviewers. Overlapping paths/resources must serialize. Live effects and Sol
review remain separate gates.

## Cloud-agent continuation contract

1. Fetch this branch and the current public `main`; do not merge this snapshot
   directly.
2. Create a fresh implementation branch from current `main`, reconcile the WIP
   patch path by path, and retain only a coherent minimal implementation.
3. Establish focused RED/GREEN coverage for DAG readiness, result-vocabulary
   parsing, write-domain conflict rejection, restart idempotence, round limits,
   and durable manager hooks.
4. Run focused tests, full tests, race tests, `go vet`, `gofmt`, diff/fsck, and
   deterministic double-build checks before freezing a candidate.
5. The author must produce a clean candidate commit. A different Grok context
   performs the first independent review. Sol is used only after Grok returns
   evidence-complete P0/P1 zero and real pre-live readiness.
6. Do not install Cardex, resume scheduling, mutate production task state, or
   change Board/config/launchd from this branch.

Suggested checkout:

```bash
git fetch origin wip/cloud-handoff/workflow-modes-t0824-1041-4cc0
git switch --detach FETCH_HEAD
```
