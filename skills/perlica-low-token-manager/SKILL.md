---
name: perlica-low-token-manager
description: Reduce context and token use in long-lived Perlica or Cardex management conversations by restoring state from durable pointers, delegating ordinary execution to fresh workers, waking only on material events, and reporting compact state deltas. Use when an agent manages projects, components, cards, reviews, releases, or several execution sessions. This is a portable management-agent playbook, not a Writer or Reviewer execution skill.
---

# Perlica Low Token Manager

Treat this directory as a portable onboarding attachment for a management agent. Cardex is an execution surface the manager may use; do not inject this Skill into ordinary Cardex Writer, Reviewer, test, or release prompts.

## Keep the manager small

The manager owns only:

- objective, scope, priority, dependency order, and acceptance boundary;
- task decomposition and selection of an execution surface;
- decisions when evidence changes direction;
- consumption of material terminals and the final user-facing status.

Delegate source work, long scans, model evaluations, test runs, review, and release preparation to fresh Cardex cards or fresh bounded workers. Directly handle work only when it is a read-only or mechanical check that should finish in under five minutes and does not mutate source, runtime, external systems, or user data.

If Cardex is unavailable, use one fresh `MANUAL_LOCAL_GOVERNED_FALLBACK` worker with the same repository, owned paths, model route, attempt ceiling, acceptance boundary, callback, and receipt route. Do not move the work back into the long-lived manager conversation.

## Restore from durable state

On wake, reconstruct only this working set:

1. current objective and acceptance boundary;
2. active card or worker identifiers;
3. latest material terminal for each active lane;
4. current blockers and resource ownership;
5. exact next gate.

Prefer receipts, files, card state, and concise thread summaries over replaying full conversation history. Read older turns or raw logs only when an unresolved decision depends on them.

## Run an event-driven loop

1. Dispatch each bounded task once and record its identifier and callback destination.
2. End the management turn after dispatch when no decision remains.
3. Wake on a material terminal, an approval/decision request, a resource conflict, or new user input.
4. Use thread waits or callbacks for active work. Do not run shell `sleep` polling loops, repeatedly read unchanged tasks, or narrate unchanged progress.
5. Send component-local results directly to that component manager. Wake the top-level manager only for cross-component order, shared-resource conflicts, Owner decisions, or final portfolio status.

## Compress every callback

Report deltas, not history. Include only:

- stable coordinate and classification;
- highest proven layer: design, candidate, review, integrated, prelive, or live;
- what changed since the previous material event;
- card, commit, receipt, and digest pointers needed to verify it;
- first blocker or falsifier;
- one exact next gate and responsible manager.

Link large artifacts instead of pasting them. Do not paste raw model streams, full prompts, repeated test output, or entire prior packets unless the consumer must inspect those bytes.

Use this compact shape when practical:

```text
MATERIAL_DELTA_V1
coordinate=<stable coordinate>
classification=<state>
highest_layer=<layer>
changed=<short delta>
evidence=<card/commit/receipt/digest pointers>
blocker=<first blocker or none>
next_gate=<one action + owner>
```

## Preserve acceptance semantics

Keep these states distinct: design ready, candidate frozen, independently reviewed, integrated, prelive accepted, and live accepted. A running process, successful provider login, green focused tests, or a receipt alone does not promote a later state.

Treat unknown outcomes, missing or invalid terminals, incomplete custody, and changed source outside scope as held. Do not retry, splice partial output, or create a successor without the applicable authority.

## Measure the optimization honestly

Keep management-session and execution-session usage separate. When measuring, record input, cached input, uncached input, output, reasoning, total, call count, and observation window. Preserve unavailable values as unknown rather than deriving false precision.

The desired outcome is fewer manager calls and less uncached manager input while useful execution moves to fresh Cardex or worker sessions. Do not optimize merely by hiding execution usage inside the management total.

## Avoid these regressions

- Do not keep a manager alive to babysit a running card.
- Do not create duplicate cards because a callback is late.
- Do not send every component event through the top-level manager.
- Do not make managers re-read full repository context already captured by a task packet.
- Do not add new schemas, gates, or audit layers solely to save tokens.
- Do not inject this Skill into Cardex execution prompts; workers should receive only their bounded task contract.
