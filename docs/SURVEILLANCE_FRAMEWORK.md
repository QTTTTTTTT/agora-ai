# Trade Surveillance Framework (P1-7)

This is the design + operations doc for the platform's trade
surveillance / market-abuse detection layer, introduced in
P1-7. Pairs with `RECON_FRAMEWORK.md` (P1-3) — the two together
turn the trade pipeline into something a compliance team can
sign off on.

## Why this exists

Once the platform settles trades and reconciles books to a broker
statement (P1-1 → P1-3), the next thing a regulator or institutional
LP asks is: **"how do you know the agents aren't trading like
crooks?"** Trade surveillance answers that with a closed-loop
detection-review-audit flow:

1. The detector scans recent fills hourly and emits typed
   `surveillance_events` for any pattern that matches a configured
   rule (wash trade, marking the close, self-cross).
2. Each event lands in the admin dashboard as `open`. A reviewer
   acknowledges (`reviewing`), then either `cleared` (false positive)
   or `escalated` (real issue, route to compliance).
3. Every action lands on the audit hash chain so the trail is
   tamper-evident.

The detector is intentionally conservative — it never blocks an
order, and false positives are a feature, not a bug, because the
*absence* of false positives means the bar is too high to catch
real abuse. Reviewers are the safety net.

## Detection rules (v1)

| Rule code           | Pattern                                                                                        | Default severity | Notes |
| ---                 | ---                                                                                            | ---              | ---   |
| `wash_trade`        | `buy → sell → buy` (or inverse) on same fund / symbol within 10m, same qty (±5%), net ≈ 0     | warning          | Triplet detection; the "net qty ≈ 0" signature is what distinguishes a wash from a normal round trip across sessions. |
| `marking_close`     | Single fill within last 15m of session, **either** size ≥ 5% of avg-daily notional **OR** price ≥ 50 bps off recent VWAP | warning (critical when **both** flags fire) | Needs `MarketContext.SessionClose`, optional `AvgDailyNotional` and `RecentVWAP`. Rule short-circuits if `SessionClose` is missing. |
| `self_trade_pair`   | `buy + sell` from same fund / symbol within 5s with matching qty + price                       | critical         | Most damning of the three — instant round-trip. Prepares the ground for cross-trade prevention as a future hard block. |
| `rapid_fire_reversal` | Reserved in schema for future implementation                                                  | —                | Listed in the closed vocabulary so a future migration does not need to widen the CHECK constraint. |
| `layering_suspect`  | Reserved in schema for future implementation                                                  | —                | Same. |

The closed vocabulary on `rule_code` is enforced both by Go
constants (`surveillance.RuleCode`) AND a SQL `CHECK`. New rules
require a code change AND a migration that widens the constraint;
that's by design — the metric label space and dashboard filter
list both depend on it.

## Data model

### `surveillance_events` — one row per pattern detection

| Column            | Notes |
| ---               | ---   |
| `id`              | Stable PK. |
| `fund_id`         | FK; cascades on fund delete (events vanish with the fund). |
| `rule_code`       | One of the closed vocabulary above. |
| `severity`        | `info` / `warning` / `critical`. |
| `symbol`, `instrument_key` | Symbol the pattern centres on. |
| `window_start`, `window_end` | Time span the trades cover. |
| `trade_ids`       | JSONB array of contributing `trade_executions.id` values, in chronological order. |
| `summary`         | Pre-rendered one-line description for table view. |
| `metadata`        | Rule-specific JSONB blob (e.g. wash-trade puts the qty matrix; marking-close puts the size_ratio + vwap_deviation). |
| `status`          | `open` / `reviewing` / `cleared` / `escalated`. |
| `review_note`, `reviewed_by`, `reviewed_at` | Set when status moves out of `open`. |
| `detected_at`, `detector_version` | Detection metadata; version is stamped so a future rule-engine upgrade can re-run a window without dedup. |
| `fingerprint`     | SHA-256 over `(fund_id, rule_code, sorted trade_ids)`. Unique index dedupes re-runs over the same window. |

### `surveillance_runs` — one row per scan invocation

The run row is bookkeeping: "we scanned X trades and produced N
events". It mirrors `reconciliation_runs` and lets the operator
see whether the scheduler is alive without grepping logs.

## The diff engine (rule engine)

`internal/surveillance/engine.go` orchestrates the configured rules
over a `[]TradeSnapshot`. Key properties:

- **Pure**: no I/O, no DB calls. The cmd/server adapter
  (`surveillance_snapshot.go`) builds the snapshot from
  `trade_executions`; the engine consumes it.
- **Stateless**: a single Engine instance can be reused across
  scans, including concurrent calls.
- **Dedupes by fingerprint**: if two rules fire on the same
  trade pattern (e.g. a self-trade pair is also technically a
  wash sequence in some shapes), the more specific rule wins
  by being listed first in `DefaultRules()`.
- **Closed vocabulary**: the engine emits `Event.RuleCode` from
  the `RuleCode` constants only; the repo's `INSERT` rejects
  anything else via the CHECK.

`MarketContext` is the optional reference data:

- `SessionClose` — time the trading day closes (required for
  marking-close rule).
- `AvgDailyNotional[symbol]` — for the marking-close size flag.
- `RecentVWAP[symbol]` — for the marking-close price-deviation flag.

When `MarketContext` fields are empty, the marking-close rule
gracefully degrades (e.g. without VWAP it can still flag on size,
without size it can still flag on VWAP, without either it
short-circuits).

## Repository (idempotency)

`internal/surveillance/repo.go` provides:

- `InsertEvent` — writes one event with `ON CONFLICT (fingerprint)
  DO NOTHING`. Returns `Inserted=true` on first write,
  `Inserted=false` on dedupe (same fingerprint = same pattern).
- `ListEvents` / `GetEvent` — paginated reads; `ListEvents` orders
  by `(severity DESC, detected_at DESC)` so critical events surface
  first.
- `UpdateStatus` — flips lifecycle state. Re-opening (status='open')
  clears the reviewer fields so the row visibly returns to the queue.
- `CreateRun` / `ListRuns` — bookkeeping.

The unique index on `fingerprint` is what makes the loop safely
re-runnable. Operators triggering an on-demand scan over the same
window as the scheduler will see `Inserted=false` rows but no
duplicates.

## REST API

All endpoints under `/api/admin/surveillance/*`, all gated by
`requireAdmin`. Audit logging emits `audit.MutationEvent` for
review and scan actions.

| Method | Path                                            | Purpose |
| ---    | ---                                             | ---     |
| GET    | `/api/admin/surveillance/events`                | List events; filters: `fund_id`, `rule_code`, `status`, `severity`, `limit`, `offset`. |
| GET    | `/api/admin/surveillance/events/{id}`           | Full event detail (with `review_note` / `reviewed_by` / `reviewed_at`). |
| POST   | `/api/admin/surveillance/events/{id}/review`    | Body `{status, note?}`. Transitions status. Audit-logged. |
| GET    | `/api/admin/surveillance/runs`                  | Recent scan runs; filters: `fund_id`, `limit`. |
| POST   | `/api/admin/surveillance/scan`                  | Body `{fund_id, as_of_date?, session_close_utc?}`. On-demand scan; audit-logged. |

### Trigger scan request shape

```json
{
  "fund_id": "fund-uuid",
  "as_of_date": "2026-06-01",
  "session_close_utc": "20:00"
}
```

`as_of_date` defaults to today UTC. `session_close_utc` defaults
to 20:00 (a passable proxy for US-market 4PM ET).

## Scheduling loop

`cmd/server/surveillance_loop.go` runs the same FX / recon-loop
shape: a goroutine sleeping `Interval` (default 1h) ± `JitterPct`
(default 5%), per-fund timeout (30s), pulled from the active fund
list. Each tick, for every active fund:

1. Load today's filled trades.
2. Run the engine.
3. For each event, `InsertEvent` (idempotent).
4. Persist a `surveillance_run` row with the counts.

Failure handling mirrors recon: log + count + skip + move on.
A single fund's snapshot or insert failure can't stall the whole
wave.

## Prometheus metrics

`fundai_surveillance_events_total{event}` counter:

- `run_ok` / `run_failed` / `scheduled_skip` / `insert_error`
- `event_<rule_code>` — per-rule hit (only on first persist; dedupe doesn't bump)
- `severity_<level>` — secondary view for stacked dashboards
- `review_<status>` — operator review action

Recommended alerts and example PromQL live in
[`PROMETHEUS_QUERIES.md`](./PROMETHEUS_QUERIES.md#14-交易监控trade-surveillance-p1-7).

## Web UI

`AdminSurveillanceSection.tsx` mounts on the global Admin page
alongside `AdminFXSection` and `AdminReconSection`:

- Filter bar: `all / open / critical`.
- Sortable table by detected_at; severity tinted.
- Click-to-expand row shows metadata, contributing trade IDs,
  detection window, and review note (if any).
- Action buttons cycle through the lifecycle (acknowledge → clear
  / escalate → reopen).
- A "Trigger scan" dialog lets the operator fire an on-demand scan
  for a fund + day + session-close.
- A small "Recent scan runs" sub-panel surfaces the loop heartbeat
  with trade-count + event-count + duration so operators can spot
  a stuck scheduler at a glance.

## Future work

- **More rules**: rapid-fire reversal, layering suspect, and a
  cross-fund self-cross variant once cross-trade prevention lands.
- **MarketContext enrichment**: wire in `marketdata` to populate
  `AvgDailyNotional` and `RecentVWAP` so marking-close becomes a
  full-strength rule rather than the current size-only / vwap-only
  best-effort.
- **Per-fund / per-instrument session calendars**: today the
  loop assumes 20:00 UTC close; an exchange-calendar lookup would
  let A-share / HKEX / TSE instruments use the right session
  boundary.
- **Real-time hot-path**: optionally bolt the self-trade rule
  onto the order entry pipeline as a synchronous block once it's
  proven safe in shadow mode for several months.
