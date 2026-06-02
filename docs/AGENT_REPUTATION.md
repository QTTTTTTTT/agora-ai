# S8.4 — Agent Reputation Ledger

## Why

By the end of S8.3 every analyst / advocate / PM call comes out
of the LLM as a structured `AnalystReport` or `AdvocateArgument`
with a stated `direction` (bullish / bearish / neutral) and
`confidence` 0..100. We persist them, but until now nothing
closes the loop: the system has no memory of **which agents are
actually right over time** and which one chronically calls the
wrong direction with high confidence.

S8.4 introduces the **agent reputation ledger** — a rolling
per-agent record of realised alpha vs benchmark that the PM
will read at decision time (S9.x) to up- or down-weight agents.
The ledger lives in two new tables and is rebuilt nightly from
the analyst-panel + debate-transcript history.

## Data model

### `agent_reputation_outcomes`

One row per `(fund_id, agent_id, symbol, asof, horizon_days)`:
the agent's stated direction + confidence, the realised forward
return over the horizon, the benchmark return, and the alpha
(realised - benchmark). `source_panel_id` / `source_debate_id`
point back at the panel / debate that produced the call so the
outcome is fully auditable.

The unique constraint on `(fund_id, agent_id, symbol, asof,
horizon_days)` means re-running the backfill driver is
idempotent — existing rows are overwritten with the latest
realised numbers.

### `agent_reputation_stats`

Denormalised summary keyed by `(fund_id, agent_id)`:
`decisions_count`, `hits_count`, `misses_count` (hit = sign
of realised matches direction), `avg_alpha`, `sum_alpha`,
`avg_confidence`, `last_decision_at`. Rebuilt by
`Repo.RecomputeStats(ctx, fundID)` after every wave.

## Backend

### `internal/agentreputation`

- `Repo` — `UpsertOutcomes`, `RecomputeStats`, `ListStats`,
  `ListOutcomes`, `GetStats`. Strict validation on `Outcome`
  (must have fund + agent + kind + direction + symbol +
  confidence in `[0,100]`).
- `Backfill` — orchestrator. Takes a `PanelSource`, optional
  `DebateSource`, and a `RealisedReturnFn`. For every panel
  report it sees in the lookback window, it asks the realised-
  return function for each horizon (`{1, 5, 21}` days by
  default), constructs one Outcome per (agent, horizon), and
  batches into `Repo.UpsertOutcomes`. Then re-runs
  `Repo.RecomputeStats`.
- `RealisedReturnFn` is the deployment seam — it's the only
  piece that needs a price feed. The default wired in this
  sprint (`nullRealisedReturn`) returns `ok=false` for every
  query, so the loop runs safely on every deployment but the
  outcomes table simply stays empty until a real price source
  is plugged in.

### `cmd/server/agent_reputation_sources.go`

Thin adapters that translate `analystreport.PanelRow` and
`debaterepo.TranscriptRow` into the minimal projections
`agentreputation.Backfill` needs. They live outside the
package so `agentreputation` stays free of cycles with
`internal/agent` (which the analyst-report repo depends on).

### `cmd/server/agent_reputation_loop.go`

The nightly driver. Defaults: 24 h interval ± 5 % jitter,
30 day lookback, `{1, 5, 21}` day horizons, 60 s per-fund
timeout. Runs in a single goroutine; `RunOnce` is what the
admin "rebuild" button calls. `RebuildForFund` temporarily
narrows the lister to one fund.

### REST API

Fund-scoped (require fund access):

- `GET /api/funds/{fundId}/agent-reputation/stats?kind=...&limit=...`
- `GET /api/funds/{fundId}/agent-reputation/outcomes?agent_id=...&symbol=...&limit=...`

Admin (require admin role):

- `GET /api/admin/agent-reputation/stats?fund_id=...&kind=...&limit=...`
- `POST /api/admin/agent-reputation/rebuild` body `{ "fund_id": "..." }` (empty = all funds)

The admin rebuild is audit-logged (`agent_reputation.rebuild`,
target `agent_reputation`, after-payload = `{fund_id,
outcomes_written}`).

## Frontend

- `web/src/components/AgentReputationSection.tsx` — per-fund
  read-only view mounted on `FundPerformance`. Sortable
  rolling-stats table + recent settled outcomes table. Filter
  by role.
- `web/src/components/AdminAgentReputationSection.tsx` —
  cross-fund admin view + rebuild trigger (all funds or a
  single fund). Mounted on `Admin.tsx` next to the other S7 /
  S8 admin sections.

i18n is hooked through `shared/api-client/src/i18n.ts`
(`agentReputation` namespace) so both `zh-CN` and `en-US` are
covered.

## What happens when the platform doesn't have a price feed

The loop still runs nightly, but every `RealisedReturnFn` call
returns `ok=false` so zero rows land in
`agent_reputation_outcomes`. The read endpoints simply return
empty lists, and the UI shows the empty-state message. The
moment ops wires a real price source (overriding the
`nullRealisedReturn` default in main.go) and clicks "Rebuild
all funds" in the admin panel, the historical analyst panels +
debates get back-scored and the leaderboard fills in.

## Next sprint hooks (S9.1)

The reputation ledger is the data layer that S9.1 reads. The
PM prompt will:

1. Pull the top-N and bottom-N agents by `avg_alpha` for the
   fund.
2. Annotate every analyst report / debate argument arriving in
   the round with `[hit_rate=X%, avg_alpha=Y%]` so the LLM has
   the prior baked into the context window.
3. Inject "alpha-tagged lessons" from `memories` joined against
   `agent_reputation_outcomes` so the PM remembers e.g.
   "fundamentals_analyst's last 3 BUY calls on AAPL realised
   −2.3 % vs SPX".
