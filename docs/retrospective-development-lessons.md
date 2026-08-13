# Cardex development lessons from completed work

Snapshot: 2026-08-13. This is an operating checklist for later sessions, not a claim that every
historical recommendation is still current.

## Evidence boundary

Two evidence sets must not be blended:

1. There are 54 legacy retrospective reports with 158 recommendations. Of those reports, 42 say
   all ten sampled cards lacked usable cost evidence. Recommendation text mentions held/cancel
   paths 109 times, cost/telemetry 62 times, runner/transport 44 times, review/verdict 38 times,
   and fix rounds 33 times. These counts expose recurring observability and closeout concerns, but
   the legacy selector was biased toward recently modified archive files and often selected canceled
   cards. They are not valid business success/failure rates.
2. The corrected latest-done compiler selected ten real done business cards with 10/10 event and
   done-event coverage. Cost coverage was 0/10; three review cards had two structured verdicts,
   both `block`; three cards were beyond fix round zero. The cohort was dominated by one Trading
   delivery cluster, so it supports local workflow fixes, not a global model or runner policy.

The strongest historical closed loop is `retro-77`: it exposed terminal-path cost gaps and excessive
fix-round escalation, which led to commits `113362f` and `f492af3`. The current MVP then exposed and
closed a wrong-cohort defect, report drift, and a post-install launchd signing failure. This proves
that retrospectives can improve the workflow, but it also shows why each recommendation needs a
small falsifiable change instead of a broad redesign.

## Defaults to carry into future development

| Lesson | Default rule | How to apply it |
|---|---|---|
| Facts before interpretation | Deterministic code owns cohort selection, arithmetic, coverage, and hashes. Models explain; they do not reconstruct the ledger. | Freeze input facts and digest before review. Reject a report that changes the hash, cohort, evidence scope, or recommendation cap. |
| State labels are not semantic verdicts | `done` means runner completion, not PASS, merged, live, or user-accepted. | Report design, Cardex, Git/test, fresh runtime, independent review, and real user path as separate layers. Name the first missing gate. |
| Every terminal branch needs the same receipt discipline | Success, failed, held, canceled, limit, recovery, and early-return paths must record either measured usage or an explicit unavailable reason. | When adding a terminal branch, add a negative test that removes its receipt and must fail. Never infer missing telemetry from a title or status. |
| Unknown is not zero | Missing cost/verdict evidence cannot drive routing or ROI claims. | Publish coverage alongside every metric. Keep cost-based model selection disabled until real coverage is sufficient; subscription cost must not be guessed. |
| One correlated window is a candidate, not policy | A single project burst can dominate ten cards. | Validate a recommendation on later independent work before changing global routing, limits, templates, or promotion rules. Put one-off edges in a deferred list. |
| Deployment is compare-and-swap | A later build from another worktree can silently replace an accepted binary before its natural canary. | Bind installation to a clean full commit and the expected current production SHA-256. Reject dirty builds and preimage drift before deleting the old inode. |
| Deployment includes every consumer | Binary hash and Web health do not prove scheduled execution or that the accepted binary is still installed. | Re-read installed provenance/hash at the natural gate; verify Board PID and `*:8788`, health, loaded `com.cardex.tick`, one fresh tick exit, then the first natural retrospective report. |
| Held work must explain how it becomes ready | A held count is not latent executable capacity. | Every held card needs an explicit release condition and owner. Do not create placeholder cards merely to reserve future work; avoid bulk cancellation as normal closeout. |
| Small isolated packets preserve learning | Broad changes hide which hypothesis worked. | One writer, exact worktree/SHA, focused RED, smallest GREEN, full regression, exact commit, rollback point, and local handoff evidence. |
| Recommendations remain proposal-only by default | A plausible retrospective must not silently rewrite production policy. | Automatic output stays read-only. A directly evidenced, frequent, reversible defect inside the current envelope may be fixed immediately; authority/routing/promotion changes still require explicit validation. |
| Preserve the resume point | Session limits and handoffs are normal operating conditions. | Keep decisions, commands, hashes, open gates, and deferred edges in a repository document after every meaningful phase. |

## Decision rule for direct fixes

Fix without another planning round only when all of the following are true:

- the failure is reproducible from current evidence;
- it affects the core path or is likely to recur;
- the change is narrow and reversible;
- a focused RED can prove the old behavior and the smallest GREEN can close it;
- the change does not silently expand write, routing, promotion, credential, or live-effect authority.

Otherwise record the issue with its trigger and evidence requirement. A rare edge, an unavailable
metric, or a conclusion supported only by one correlated cohort is not an immediate feature request.

## Next use of these lessons

1. Keep the standard install fail-closed on dirty build provenance, unexpected candidate commit,
   and production SHA-256 preimage drift. `doctor` must reject a dirty-worktree binary even if its
   service is healthy.
2. Natural report `retro-707` is not a v2 acceptance artifact: an unrelated dirty-tree `0.10.2`
   binary replaced the accepted candidate before the threshold, so the report has no frozen facts,
   hash, cohort, or evidence-bound schema. Do not edit the counter or create a synthetic replay.
   Restore an authorized v2-capable production build first, then use the next natural threshold
   (`717`) for acceptance.
3. After a valid report, choose at most one next packet. Current evidence makes usage coverage and
   structured review-verdict coverage candidates; instrument only a source that can report truthful
   data, and do not invent subscription cost.
4. Defer cross-window learning ledgers, semantic deduplication, contradiction handling, and automatic
   policy promotion until at least the natural report proves the minimum loop useful.
5. Only after the retrospective loop is producing useful validated changes should intent extraction
   be expanded. Its input should be observed specification/intent failures, not a generic parser built
   in advance of evidence.
