# ClaudeGo configuration reference

[中文](config.md) | **English** · back to [README](../README.en.md)

## Config quick reference (~/.claudego/config.json)

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
| `no_fallback_models` | ["claude-fable-5","fable"] | design-tier models never downgraded to the codex backup — they wait for Claude |
| `thinking_tokens` | 0 | when >0, sets MAX_THINKING_TOKENS on Claude calls (larger thinking budget for design work) |
| `queue_budget_tokens` etc. | 0 (off) | 5-hour quota redline — see [guide · quota redline](guide.en.md#5-hour-quota-redline-reserve-headroom) |
| `oauth_usage` / `oauth_usage_*` | false | subscription endpoint (third source); undocumented endpoint — anomalies treated as insufficient data |
| `max_parallel` | 1 | tasks per tick (writing tasks are serialized per directory; read-only types like design-review / progress-pull are exempt and may run concurrently in the same repo) |
| `codex_bin` / `codex_fallback` | empty / false | cooldown backup executor — see [guide · codex backup executor](guide.en.md#codex-backup-executor-no-downtime-during-limit-gaps) |
| `codex_fallback_model` | "" | model used when a claude card downgrades to codex (tier-parity: opus→terra, not sol); empty falls back to `codex_model` |
| `codex_reasoning` | "" | global codex reasoning effort (minimal/low/medium/high/xhigh/max/ultra) → `-c model_reasoning_effort=…`; a per-task effort overrides it |
| `codex_review_sandbox` | "worktree-write" | Sandbox policy for codex read-only analysis cards (design-review/crosscheck etc.). Default `worktree-write`: **local** codex builds a one-shot isolated copy + `--sandbox workspace-write` so reviews can run tests / drop fixtures for dynamic verification; the copy lands in `<root>/tmp/codex-review-work/` and is torn down when the card ends — the source repo is never write-polluted (CG-R3). **Remote** codex only relaxes to `workspace-write` when `t.Dir` sits under `remote_mirror_root` (a genuine one-shot mirror); crosscheck/coordinate/fallback paths that run in a real business repo keep `--sandbox read-only` as a sandbox-level hard guarantee (CG-R3 R1 P0-1). Set to `readonly` to roll back to the old behavior everywhere (codex runs `--sandbox read-only` directly — static reading only). **An unrecognized value falls back to `readonly` (fail-closed) and is disclosed once in the log** — a one-letter typo must never silently buy a wider sandbox (CG-R3b); only an absent/empty key gets the `worktree-write` default. `sequence` cards are unaffected. |
| `cross_profiles` | {opus-codex} | cross-verification engine pairs (`claudego cross`) — see [guide · cross-verification](guide.en.md#cross-verification-fable-stand-in-two-independent-engines--adversarial-cross-check) |
| `default_cross_profile` | "opus-codex" | engine pair used when `cross` gets no `-profile` |
| `default_review_host` | "" | global default review host (`remote_hosts` key); auto-diverts local impl cards when the trio is set — see [guide · review divert](guide.en.md#review-divert-offload-read-only-review-to-a-second-machine) |
| `remote_mirror_root` | "" | remote mirror root; paired with `default_review_host`; ReviewDir auto-derived as `<root>/<worktree-name>` |
| `default_review_sync` | "" | global default pre-divert sync command (sh -c, cwd=impl card dir); all three keys must be set for the default to apply |
| `remote_hosts.<name>.codex_only` | false | When true, mechanically forbids Claude on that host; Claude-model and automatic review tasks are rerouted to remote Codex at dispatch |

Prompt templates live in `~/.claudego/templates/*.md` and can be edited directly (`{{GOAL}}` `{{DIR}}` `{{FOCUS}}` are substituted; `{{QUEUE}}` `{{PROGRESS}}` in `coordinate.md` are replaced with a live snapshot **at dispatch time**).
