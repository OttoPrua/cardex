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
| `type_defaults.*.model` | coordinate opus; progress-pull haiku | default model per type (--model value); empty uses the account default |
| `type_defaults.<type>` | see built-in table | Per-type execution defaults. **Fields you omit inside an entry fall back to the built-in value for that type**: JSON merges by key only, so writing `{"design-review": {"model": "opus"}}` replaces the whole entry with one that has nothing but `model`, emptying `allowed_tools` — which silently removes the review card's read-only tool set. To genuinely ship no tool allowlist, write `skip_permissions: true`; do not leave `allowed_tools` empty (empty and "not written" are indistinguishable in JSON). If the whole type entry is missing, no defaults are baked in at all — that is the different intent "this type has no defaults configured" |
| `no_fallback_models` | ["claude-fable-5","fable"] | design-tier models never downgraded to the codex backup — they wait for Claude |
| `thinking_tokens` | 0 | when >0, sets MAX_THINKING_TOKENS on Claude calls (larger thinking budget for design work) |
| `stakes_policy` | low=no review / normal=follow / high=force review + raise to high | per-card stakes → review-depth lookup (`add -stakes`); **frozen onto the card at enqueue**, never re-read at run time — see [guide · stakes tiering](guide.en.md#per-card-stakes-tiering--stakes--review-depth-lookup-table) |
| `retro_every_n_done` | 0 (off) | every N cards reaching `done`, auto-enqueue a haiku retrospective card (read-only tally, proposal-only); 10 is a reasonable start — see [guide · retrospective cards](guide.en.md#automatic-retrospective-cards-retro_every_n_done) |
| `queue_budget_tokens` etc. | 0 (off) | 5-hour quota redline — see [guide · quota redline](guide.en.md#5-hour-quota-redline-reserve-headroom) |
| `oauth_usage` / `oauth_usage_*` | false | subscription endpoint (third source); undocumented endpoint — anomalies treated as insufficient data |
| `max_parallel` | 1 | tasks per tick (writing tasks are serialized per directory; read-only types like design-review / progress-pull are exempt and may run concurrently in the same repo) |
| `codex_bin` / `codex_fallback` | empty / false | cooldown backup executor — see [guide · codex backup executor](guide.en.md#codex-backup-executor-no-downtime-during-limit-gaps) |
| `codex_fallback_model` | "" | model used when a claude card downgrades to codex (tier-parity: opus→terra, not sol); empty falls back to `codex_model` |
| `codex_reasoning` | "" | global codex reasoning effort (minimal/low/medium/high/xhigh/max/ultra) → `-c model_reasoning_effort=…`; a per-task effort overrides it |
| `codex_review_sandbox` | "worktree-write" | Sandbox policy for codex read-only analysis cards. **Local** codex uses a one-shot copy with `workspace-write`. **Remote** codex only relaxes inside a strict descendant of `remote_mirror_root`: it normally uses `workspace-write`, or inherits an explicitly configured host `sandbox: "danger-full-access"` when the Windows OS sandbox runner is unavailable. Real business repositories used by crosscheck/coordinate/fallback paths remain `read-only`. Set this key to `readonly` to force the old read-only behavior everywhere. Unknown values fail closed to `readonly`; an absent key keeps the default. Sequence cards are unaffected. |
| `cross_profiles` | {opus-codex} | cross-verification engine pairs (`cardex cross`) — see [guide · cross-verification](guide.en.md#cross-verification-fable-stand-in-two-independent-engines--adversarial-cross-check) |
| `default_cross_profile` | "opus-codex" | engine pair used when `cross` gets no `-profile` |
| `default_review_host` | "" | global default review host (`remote_hosts` key); auto-diverts local impl cards when the trio is set — see [guide · review divert](guide.en.md#review-divert-offload-read-only-review-to-a-second-machine) |
| `remote_mirror_root` | "" | remote mirror root; paired with `default_review_host`; ReviewDir auto-derived as `<root>/<worktree-name>` |
| `default_review_sync` | "" | global default pre-divert sync command (sh -c, cwd=impl card dir); all three keys must be set for the default to apply |
| `remote_hosts.<name>.codex_only` | false | When true, mechanically forbids Claude on that host; Claude-model and automatic review tasks are rerouted to remote Codex at dispatch |

Prompt templates live in `~/.cardex/templates/*.md` and can be edited directly (`{{GOAL}}` `{{DIR}}` `{{FOCUS}}` are substituted; `{{QUEUE}}` `{{PROGRESS}}` in `coordinate.md` are replaced with a live snapshot **at dispatch time**).
