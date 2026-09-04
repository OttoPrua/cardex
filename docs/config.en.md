# cardex configuration reference

[中文](config.md) | **English** · back to [README](../README.en.md)

## Config quick reference (~/.cardex/config.json)

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
| `type_defaults.*.model` | assembly/coordinate/review Opus; implementation Sonnet; pull Haiku | source tier per type; the Codex primary route resolves it to a concrete GPT-5.6 model; Fable is explicit hardest-adjudication only |
| `type_defaults.<type>` | see built-in table | Per-type execution defaults. **Fields you omit inside an entry fall back to the built-in value for that type**: JSON merges by key only, so writing `{"design-review": {"model": "opus"}}` replaces the whole entry with one that has nothing but `model`, emptying `allowed_tools` — which silently removes the review card's read-only tool set. To genuinely ship no tool allowlist, write `skip_permissions: true`; do not leave `allowed_tools` empty (empty and "not written" are indistinguishable in JSON). If the whole type entry is missing, no defaults are baked in at all — that is the different intent "this type has no defaults configured" |
| `no_fallback_models` | ["claude-fable-5","fable"] | design-tier models never downgraded to the codex backup — they wait for Claude |
| `thinking_tokens` | 0 | when >0, sets MAX_THINKING_TOKENS on Claude calls (larger thinking budget for design work) |
| `max_fix_rounds` | 3 | **global** round cap on the implement → adversarial-review → auto-fix loop; past it, a held escalation card goes to a human. Overridden per tier by `stakes_policy.<tier>.max_fix_rounds` |
| `stakes_policy` | low=no review / normal=follow / high=force review on implementation cards + raise to high + one auto-fix | per-card stakes → review-depth lookup (`add -stakes`); per-tier fields are `review` / `default_effort` / `max_fix_rounds` (`0` = follow the global cap). Automatic review is allowed only for `sequence`; other types are cleared both at enqueue and runtime. **Frozen onto the card at enqueue**, never re-read at run time — see [guide · stakes tiering](guide.en.md#per-card-stakes-tiering--stakes--review-depth-lookup-table) |
| `retro_every_n_done` | 0 (off) | every N cards reaching `done`, auto-enqueue a haiku retrospective card (read-only tally, proposal-only); 10 is a reasonable start — see [guide · retrospective cards](guide.en.md#automatic-retrospective-cards-retro_every_n_done) |
| `queue_budget_tokens` etc. | 0 (off) | 5-hour quota redline — see [guide · quota redline](guide.en.md#5-hour-quota-redline-reserve-headroom) |
| `oauth_usage` / `oauth_usage_*` | false | subscription endpoint (third source); undocumented endpoint — anomalies treated as insufficient data |
| `max_parallel` | 1 | tasks per tick (writing tasks are serialized per directory; read-only types like design-review / progress-pull are exempt and may run concurrently in the same repo) |
| `default_runner` | "" (legacy Claude) | Supports `claude`, `codex`, or an enabled `agy`; retired `gemini` is rejected at load |
| `owner_routing_enforced` | `false` | Hard lock for the final Owner matrix. When enabled, drift in any risk/review branch, exact provider/runner/model/effort, the sole Fable Sol/ultra terminal, or any explicit Sol gate is rejected at load. New `sequence` cards declare `route_class=backend|general`; backend is ordinary only with explicit `risk_class=ordinary`, while missing or ambiguous risk fails closed to high risk. Set `CARDEX_REQUIRE_OWNER_ROUTING=1` on managed board/tick units so deleting the key fails startup instead of silently restoring legacy policy |
| `automatic_codex_budget_stop_percent` / `owner_provider_targets` | `0` / empty | Final Owner mode requires 65%; provider-specific automatic-Codex usage at that point preserves about 35%, and unavailable evidence also holds. Only an Owner-pinned critical card with a visible durable reason may bypass. Reporting-only targets are Grok 70–80%, Kimi/OpenCode 15–25%, and direct Sol 5–10%; they never mutate existing tasks |
| `codex_bin` / `codex_fallback` | empty / false | cooldown backup executor — see [guide · codex backup executor](guide.en.md#codex-backup-executor-no-downtime-during-limit-gaps) |
| `codex_fallback_model` | "" | generic model for non-Opus Claude cards downgraded to Codex; empty falls back to `codex_model` |
| `codex_fallback_opus_model` / `codex_fallback_opus_reasoning` | `gpt-5.6-sol` / `xhigh` | default model and reasoning effort for Opus-tier Claude cards downgraded to Codex; `stakes=low` does not downgrade by default |
| `codex_opus_simple_model` / `codex_opus_simple_reasoning` | empty (disabled) | optional explicit low-stakes Opus downgrade; it applies only when both fields are configured and the card has structured `stakes=low`. The production policy keeps it disabled |
| `codex_tier_models` / `codex_tier_reasoning` | see built-in map | Codex primary and eligible fallback tier slots: fable→sol/max, opus→sol/xhigh, sonnet→luna/max, haiku→luna/xhigh |
| `codex_reasoning` | "" | global fallback effort when no source tier resolves; an explicitly set card effort takes priority over the tier default |
| `codex_review_sandbox` | "worktree-write" | Sandbox policy for codex read-only analysis cards. **Local** codex uses a one-shot copy with `workspace-write`. **Remote** codex only relaxes inside a strict descendant of `remote_mirror_root`: it normally uses `workspace-write`, or inherits an explicitly configured host `sandbox: "danger-full-access"` when the Windows OS sandbox runner is unavailable. Real business repositories used by crosscheck/coordinate/fallback paths remain `read-only`. Set this key to `readonly` to force the old read-only behavior everywhere. Unknown values fail closed to `readonly`; an absent key keeps the default. Sequence cards are unaffected. |
| `antigravity_bin` / `antigravity` | empty / disabled | Native `agy` route. Before dispatch it runs `agy models` with only proxy variables and the native HOME, then selects the highest actually advertised Claude Opus. It never substitutes Sonnet/Gemini when Opus is absent, and omits `--effort` for thinking-encoded models |
| `gemini_*` | historical compatibility only | Existing tasks/config remain decodable and visible; new cards, defaults, fallback entries, workflows, and runtime execution are rejected |
| `opencode_bin` / `opencode_model` / `opencode_models` | empty | Native OpenCode CLI executor. It reuses OpenCode's local credential store instead of copying an API key; `-runner opencode` pins it explicitly |
| `opencode_night_opus` | empty (disabled) | Compatible single-target nighttime OpenCode Opus route: `enabled/start_hour/end_hour/timezone/model/variant/limit_fallback_min`; explicit `-runner opencode` pins are not constrained by the window |
| `kimi_cli_bin` / `kimi_cli_home` / `kimi_cli_model` / `kimi_cli_effort` | empty | Native Kimi Code CLI executor. `kimi_cli_home` points at the authenticated home (defaults to `~/.kimi-code`). Cardex creates an isolated runtime home under its own data root and symlinks only OAuth credentials, never copying the token. Production K3 uses `kimi-code/k3`; max is injected through the CLI's official `KIMI_MODEL_THINKING_EFFORT` variable |
| `kimi_cli_opus` | empty (disabled) | Native Kimi lane; `max_parallel` is an independent cap and defaults to 24 when empty/zero, still bounded by global parallelism and write-domain exclusion |
| `grok_build_bin` / `grok_build` | empty / disabled | Native Grok lane; `max_parallel` also defaults to 24. A value-blind preflight persists auth/proxy/rate/model readiness before semantic attempt accounting |
| `cursor_bin` / `cursor_model` / `cursor_fable` | empty / disabled | Explicit Fable is exactly `claude-fable-5-thinking-max`, always general and read-only. Confirmed quota or an eligible proven presemantic failure creates only A=`grok-4.6/xhigh`; B=`gpt-5.6-sol/ultra` is the lineage's sole automatic Codex call and receives the original problem/evidence plus A, reconstructs constraints, attacks and repairs the proposal, and emits the terminal conclusion. Profile `merge` must be omitted. Semantic/acceptance failure does not trigger; there is no blind Sol answer, third Sol/max leg, or review-of-review, and unresolved P0/P1/uncertainty holds for Owner |
| `engines` | {} (empty) | multi-subscription engine profiles: key = engine name (lowercase alnum/hyphens; claude/codex/remote reserved), value carries base_url, one of three credential refs (auth_env env-var name / auth_file / auth_value plaintext), auth_var (injected var, only ANTHROPIC_AUTH_TOKEN/ANTHROPIC_API_KEY), models tier map (fable/opus/sonnet/haiku → vendor model IDs), default_model, extra_env, limit_fallback_min (0 inherits global), tier display label. Merge built-in presets with `cardex engines add <name>`; see [guide · multi-subscription engines](guide.en.md#multi-subscription-engines-engine-profiles-kimi--glm--minimax--mimo--opencode-go--ollama-cloud) |
| `fallback_order` | ["codex"] | divert order during Claude cooldown/redline; retired `gemini` entries are rejected and `agy` is an explicit native runner only |
| `model_tiers` | {} (empty) | custom tier table: model ID (lowercase, exact or prefix match) → tier keyword (fable/opus/sonnet/haiku), overriding the built-in standard line. It drives tier display, engine-tier derivation, and final Owner matrix resolution, so changing a mapping changes dispatch for new unpinned cards. Bad values are rejected at load. See [guide · custom tiering](guide.en.md#custom-tiering-model_tiers-fleets-without-stronger-models-rank-by-the-cards-they-hold) |
| `cross_profiles` | {opus-codex} | cross-verification chains (`cardex cross`): A/B answer independently; optional `merge` freezes a third merger, while omission preserves the legacy B-as-C behavior — see [guide · cross-verification](guide.en.md#cross-verification-fable-stand-in-two-independent-engines--adversarial-cross-check) |
| `default_cross_profile` | "opus-codex" | engine pair used when `cross` gets no `-profile` |
| `default_review_host` | "" | global default review host (`remote_hosts` key); auto-diverts local impl cards when the trio is set — see [guide · review divert](guide.en.md#review-divert-offload-read-only-review-to-a-second-machine) |
| `remote_mirror_root` | "" | remote mirror root; paired with `default_review_host`; ReviewDir auto-derived as `<root>/<worktree-name>` |
| `default_review_sync` | "" | global default pre-divert sync command (sh -c, cwd=impl card dir); all three keys must be set for the default to apply |
| `remote_hosts.<name>.codex_only` | false | When true, mechanically forbids Claude on that host; Claude-model and automatic review tasks are rerouted to remote Codex at dispatch |
| `manager_wake` | empty (off) | Management-card wake. An incomplete `enabled` block fails closed at load; a wake never authorizes anything beyond one model turn |
| `manager_wake.owner_routing` | false | R8 sender-receives delivery. Off is a byte-level rollback: no `owner-<requester_id>` subscription is derived, no `owner-*` cursor/receipt/inflight file is created, and a card's pinned `reply_route` stays inert. Unrelated to top-level `owner_routing_enforced` (the Owner provider matrix) |
| `manager_wake.subscriptions[].role` | "" (plain scoped subscription) | Set to `root` to mark the single root reply endpoint, which receives `endpoint_kind=root` cards and `escalate_to_root` fan-out. Two of them fail closed with `duplicate_root_subscription`; subscription ids may not use the reserved `owner-` prefix |

For the card-face `reply_route` (`cardex.task.reply_route.v1`), the executable versus
reserved endpoints, the isolation boundary and the M1–M12 targets, see the
[sender-receives contract](manager-wake-sender-contract.md).

## Board config quick reference (~/.cardex/board.json)

The board **only reads** this file, never writes it; a missing file simply means "derive everything".

| Key | Where | Description |
|---|---|---|
| `projects.<project-id>.name` / `.desc` / `.phases.<phase>` | project block | hand-written text overriding the derived project/phase blurbs |
| `projects.<project-id>.goal` | project block | goal-anchored "landed progress" (shown alongside card progress, never replacing it) — see [guide](guide.en.md#project-override-cardexboardjson) |
| `projects.<project-id>.kind_rules` | project block | manual classification rules (title substring or full task ID → kind); bad rules are skipped individually and disclosed via `kind_rule_error` |
| `projects.<project-id>.planned_total_cards` | project block | manual planned anchor for the estimated-remaining progress scale (phase-plan card total); always beats the automatic spawn-factor estimate — update it when plans land/change (that's the calibration hook). 0/absent = automatic estimate |
| `project_aliases` | **top level** | ordered directory → project grouping table: `[{"match":"<exact dir or glob>","title":"<optional title substring>","project":"<project name>"}]`. First match wins; attribution priority is **explicit (`add -project`) > alias > built-in pattern > directory heuristic > "未分类" (unclassified)**. Editing this table touches no task card and applies retroactively on the next snapshot rebuild — this is how you clean up a backlog of wild projects. Bad rules are skipped individually and disclosed via `project_alias_error`. See [guide · project attribution](guide.en.md#project-attribution-explicit--alias--pattern--heuristic--unclassified) |

A `match` without wildcards is an **exact directory match** (so that one rule on a container directory cannot swallow every project underneath it); write `X/*` to cover a subtree (the glob is matched against the directory or any ancestor, hence any depth). Matching is case-insensitive.

Prompt templates live in `~/.cardex/templates/*.md` and can be edited directly (`{{GOAL}}` `{{DIR}}` `{{FOCUS}}` are substituted; `{{QUEUE}}` `{{PROGRESS}}` in `coordinate.md` are replaced with a live snapshot **at dispatch time**).
