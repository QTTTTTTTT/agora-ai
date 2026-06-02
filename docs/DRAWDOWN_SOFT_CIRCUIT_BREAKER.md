# Drawdown soft circuit breaker (P3-5)

## Goal

When a fund's NAV slides past a configured drawdown threshold,
*automatically* propose (and optionally execute) a position trim
through the existing order pipeline — instead of waiting for the
PM agent to react, or worse, waiting for the operator to notice.

The "soft" qualifier matters. We already have a **hard** circuit
breaker in `internal/agent/risk.go` that REJECTS a plan when the
fund is below `circuit_breaker_pct` (default −20%). The hard
breaker is binary: trade-or-don't. The soft breaker is graded:

  * Tier 1 (−5%): trim 25% of every long position pro-rata.
  * Tier 2 (−10%): trim 50% pro-rata.
  * Tier 3 (−15%): flatten — sell everything to cash.

Operators tune the tiers per fund. The engine takes the worst
breached tier in any given evaluation.

## Why we needed this

* The PM agent works on a daily cadence; mid-session drawdowns
  can pile up before the next plan run.
* The hard breaker is too binary — a fund at −12% can still
  trade, so it just keeps drawing down quietly.
* Operators are not in the loop on intraday risk; they want to
  delegate "if DD > X% trim Y%" to a deterministic engine while
  keeping a 4-eye gate before any actual order fires.

## Components

### Schema (migration `062_drawdown.sql`)

Two tables:

* `drawdown_policies` — per-fund tier configuration. One row per
  `(fund_id, tier)`. CHECK constraints enforce the closed
  vocabulary (`action ∈ {trim_proportional, flatten,
  defensive_only}`) and value ranges (`dd_pct < 0`, `trim_ratio ∈
  [0,1]`, `cooldown_hours ∈ [0, 720]`). Storing per-tier rows
  (rather than inlining all tiers as JSON) lets the audit chain
  record each knob bump as a discrete diff.
* `drawdown_events` — one row per detected breach. Holds the DD
  level at detection, the resolved trim plan as JSONB, the
  lifecycle status, and reviewer fields. Indexed for "open queue
  first" by `(fund_id, status, detected_at DESC)`.

We deliberately did NOT add a third table for "high-water mark"
— peak NAV is recomputed on every snapshot from the existing
`nav_snapshots` table over a `lookback_days` window (default 90).
Storing it again would create another source-of-truth fight.

### Engine (`internal/drawdown/`)

Pure / stateless / no DB handles.

* `ComputeDD(peak, current) float64` — non-positive fraction.
* `Engine.Evaluate(snap, policy) → BreachEvent | nil` — returns
  the worst breached tier or nil (no breach / all tiers in
  cooldown / empty policy).
* `BuildTrimPlan(positions, tier) []TrimPlanItem` — pure helper
  callable independently for "preview" before the operator
  approves.

### Snapshot adapter (`cmd/server/drawdown_snapshot.go`)

Reads:

  * Current NAV from `funds.nav` (the running mark, kept fresh
    by `navcalc`).
  * Peak NAV from `nav_snapshots` over the lookback window.
  * Open `holding_positions` (long-only; shorts are out of scope
    for v1 because "trim a short" requires a buy that increases
    risk).
  * `LastFiredAt[tier]` from `drawdown_events` over a 7-day
    window (covers any cooldown ≤ 168h).

Returns a fully-populated `drawdown.Snapshot`.

### REST API (`cmd/server/admin_drawdown.go`)

All endpoints require admin and audit-log every mutation.

| Method | Path                                                                | Purpose                                          |
| ------ | -------                                                             | -------                                          |
| GET    | `/api/admin/drawdown/funds/{fundId}/policy`                         | List configured tiers                            |
| PUT    | `/api/admin/drawdown/funds/{fundId}/policy/tiers/{tier}`           | Upsert one tier                                  |
| DELETE | `/api/admin/drawdown/funds/{fundId}/policy/tiers/{tier}`           | Remove one tier                                  |
| GET    | `/api/admin/drawdown/funds/{fundId}/status`                         | Live preview: peak/current/DD + would-emit event |
| POST   | `/api/admin/drawdown/funds/{fundId}/check`                          | On-demand evaluate; persists `proposed` event    |
| GET    | `/api/admin/drawdown/events`                                        | List events (filters: fund_id, status, limit)    |
| GET    | `/api/admin/drawdown/events/{id}`                                   | Get one event with full trim_plan                |
| POST   | `/api/admin/drawdown/events/{id}/review`                            | Transition status (proposed/approved/dismissed)  |

The review endpoint REJECTS manually setting `executed` — that
status is reserved for the auto-execute worker which back-fills
`trade_ids` once orders queue. The flow is:

```
proposed → approved (operator click)
                  → executed (worker once orders submit)
proposed → dismissed (operator declines)
proposed → superseded (deeper-tier event takes over)
```

### Scheduler (`cmd/server/drawdown_loop.go`)

Default cadence: 5 minutes ± 5% jitter. Per-fund timeout: 30 s.
Each wave:

  1. Iterate `fund_repo.ListActive()`.
  2. For each fund: load policy → build snapshot → evaluate.
  3. On breach: insert event with `proposed` (default) or
     `approved` (if the matched tier has `auto_execute=true` and
     the loop has an `AutoExecuteHandler` wired).
  4. If `approved` and handler is wired: call handler to queue
     the trim orders; promote to `executed` only after the
     handler succeeds.

The default startup wires the loop with `AutoExecuteHandler =
nil`, so even `auto_execute=true` tiers will land as `approved`
and wait for an operator to push the actual orders. This is the
safe default for v1 — until the order pipeline integration is
explicitly opted-in, no automated firing happens. Operators can
flip auto-execute on once they're comfortable.

## Why graceful degradation matters

Several layers can fail without the loop dying:

  * Missing `funds` row → snapshot returns error → recorded as
    `check_failed`, fund skipped, loop continues.
  * Empty `nav_snapshots` (brand-new fund) → peak falls back to
    current NAV → no DD → no event.
  * Missing repo (test wiring) → `LastFiredAt` defaults to empty
    map; cooldown logic still safe.
  * Missing fund lister → `scheduled_skip` recorded, loop sleeps
    until next tick.

This matches the rest of the platform's "loop doesn't crash on
single-fund failure" pattern (FX, recon, surveillance).

## Web UI

`web/src/components/AdminDrawdownSection.tsx` (mounted in
`web/src/pages/Admin.tsx` after the surveillance section):

  * Operator types `fund_id` → "Load" → status panel renders
    peak NAV, current NAV, current DD %, configured tiers.
  * Tier editor (max 5 tiers) with action picker, trim ratio
    slider, cooldown hours, auto_execute toggle, free-form note.
  * "Run check now" button: triggers on-demand evaluate; the
    badge tells the operator whether anything was emitted.
  * Events table with status filter; per-row Approve / Dismiss /
    Re-open buttons that open a note dialog (the note lands on
    the audit chain).

## Metrics

`fundai_drawdown_events_total{event="..."}` counter with these
labels:

| event                       | meaning                                                |
| ---                         | ---                                                    |
| `check_ok` / `check_failed` | scheduler loop OR on-demand evaluate                   |
| `breach_tier_<n>`           | tier `<n>` fired (1..5)                                |
| `action_<action>`           | distribution by action                                  |
| `review_<status>`           | operator transitions                                    |
| `auto_executed`             | auto-execute handler queued orders                     |
| `policy_upsert`             | admin tier edit (new or modify)                        |
| `policy_delete`             | admin removed a tier                                   |
| `scheduled_skip`            | loop skipped wave (no lister or lister error)          |

See `docs/PROMETHEUS_QUERIES.md` §15 for queries and alert
rules.

## Testing

  * `internal/drawdown/types_test.go` — engine: 13 cases
    covering peak/current edge values, worst-tier wins,
    fractional-share floor, defensive-only empty plan,
    cooldown skipping, fund_id mismatch, empty policy.
  * `internal/drawdown/repo_test.go` — repo: upsert validation
    (bad dd_pct, bad tier, bad action, trim_ratio clamping),
    GetPolicy empty + happy, InsertEvent happy, UpdateStatus
    not-found / bad-status, LastFiredAtForFund, DeleteTier.
  * `cmd/server/drawdown_loop_test.go` — defaults applied,
    explicit zero-jitter preserved, nil-builder no-op,
    nil/erroring fund lister handling, jitter bounding.
  * `cmd/server/admin_drawdown_test.go` — admin endpoints:
    auth (401/403), GetPolicy happy, UpsertTier validation
    (invalid body, out-of-range tier, bad action), Review
    validation (bad status, refusing manual `executed`),
    Trigger validation.

All tests pass.

## Future work (out of scope for v1)

  1. **Order pipeline integration** for `auto_execute=true`. The
     loop has the `AutoExecuteHandler` hook ready; we just need
     a concrete handler that translates `TrimPlanItem` into
     `trade_executions` rows through the audit chain and the
     idempotent submit path.
  2. **`defensive_only` enforcement at order entry** — the
     engine emits `defensive_only` events but nothing currently
     consults the breached state to block NEW long orders. The
     fix is a small change in `internal/api/order_handler.go`
     to read the active drawdown event and reject long-side
     orders while a `defensive_only` tier is active.
  3. **Per-position trim weights** — v1 trims pro-rata. v2
     could trim higher-beta names harder, or skip names that
     are <X% of NAV.
  4. **Tier history audit view** — the audit chain captures every
     `policy_upsert` already; the admin UI should expose a
     "history" link per fund so reviewers can see how the
     thresholds evolved.
  5. **Cross-fund correlation** — when many funds breach tier 3
     at once, we have a system-level event (market shock) and
     might want to coalesce alerts.
