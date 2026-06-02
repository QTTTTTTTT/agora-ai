# Stress scenarios + per-fund stress runs (S7 / P3-3)

## Purpose

Third piece of the S7 Risk & Attribution sprint.  Apply a named
stress scenario (historical replay / hypothetical / regulatory)
to a fund's current holdings and produce a projected NAV with
per-holding contributions.

The runner is intentionally **on-demand and read-only on the
fund side**; only operators with the admin role can curate the
scenario library.  Per-fund users see scenarios in the picker
and run them but cannot mutate the catalog.

## Data model

### `stress_scenarios` (new)

```sql
CREATE TABLE stress_scenarios (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name        TEXT NOT NULL UNIQUE,
    category    TEXT NOT NULL,         -- historical | hypothetical | regulatory
    description TEXT NOT NULL DEFAULT '',
    shocks      JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_by  UUID NULL REFERENCES users(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

`shocks` is a JSONB array of `{target_type, target_key, value}`
objects.  The engine validates the shape on read; Postgres only
enforces `jsonb_typeof(shocks) = 'array'`.

### `portfolio_stress_results` (new, append-only)

```sql
CREATE TABLE portfolio_stress_results (
    id             BIGSERIAL PRIMARY KEY,
    fund_id        UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    scenario_id    UUID NOT NULL REFERENCES stress_scenarios(id) ON DELETE CASCADE,
    calculated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    nav_before     NUMERIC(20,6) NOT NULL,
    nav_after      NUMERIC(20,6) NOT NULL,
    pnl_total      NUMERIC(20,6) NOT NULL,
    pnl_pct        NUMERIC(10,6) NOT NULL,
    holding_count  INTEGER NOT NULL,
    shocked_count  INTEGER NOT NULL,
    impacts        JSONB NOT NULL DEFAULT '[]'::jsonb
);
```

`impacts` is a JSONB array of per-holding rows so the UI's
drill-down stays atomic with the parent stress run.

Indexes serve the dashboard timeline (`fund_time_idx`) and
the per-scenario history (`scenario_idx`).

## Shock model

Each shock has:

```
{
  "target_type": "instrument" | "market" | "asset_class" | "factor" | "wildcard",
  "target_key":  string,        // instrument_key, market code, asset_class, factor name, or "*"
  "value":       number         // signed decimal fraction; -0.20 = "-20%"
}
```

### Matching priority

Highest-specificity wins:

```
instrument > market > asset_class > factor > wildcard
```

Each holding picks **one** non-factor match (the highest-specificity
one).  Factor shocks are the exception — every matching factor
shock contributes additively:

```
applied = sum(shock.value * loading[shock.target_key])
        for shock in factor_shocks
        if loading[shock.target_key] is defined for the holding
```

This mirrors the textbook factor-model P&L attribution.  Factor
shocks consult `instrument_factor_loadings` (S7.1) at run time;
holdings with no loading skip the factor contribution.

### Sanity clamps

- Shock validator rejects `|value| > 10` and NaN/Inf.
- Engine clamps applied return to `[-1, +1]` so a pathological
  `loading × shock` combination can't wipe out 5x the position
  notional.

### Worked example

Scenario:

```json
[
  { "target_type": "asset_class", "target_key": "equity", "value": -0.40 },
  { "target_type": "instrument",  "target_key": "US:AAPL", "value": -0.30 },
  { "target_type": "factor",       "target_key": "momentum", "value": -0.10 }
]
```

A holding `US:AAPL` (equity, momentum loading 1.2) gets the
**instrument** match (highest specificity) and applies -30%.

A holding `US:MSFT` (equity, momentum loading 0.8) has no
instrument shock, no market shock; the asset_class match wins
and applies -40%.  The factor shock is shadowed by the more
specific asset_class match for this holding.

A holding `JP:7203` (asset_class=equity, momentum loading -0.5)
applies the asset_class -40% match (same logic as MSFT).

A holding `CN:600519` (asset_class=liquor) doesn't match any
asset_class shock.  No market or instrument match either.
The factor shock applies because momentum loading exists:
applied = -0.10 × loading.

## Engine (`server/internal/stress`)

Pure Go, no external deps beyond stdlib.  `Engine.Compute(fundID,
scenario, holdings, loadings)` returns a `Result` with:

- `NAVBefore`  — gross MV (sum of |MarketValue|)
- `NAVAfter`   — gross MV after applying every matched shock
- `PnLTotal`   — signed (negative = loss)
- `PnLPct`     — `PnLTotal / NAVBefore`
- `Impacts`    — per-holding rows sorted by `|PnL|` descending
                 so the worst contributors render first

11 engine unit tests cover: empty holdings, wildcard shock, the
priority ladder (instrument > market > asset_class > wildcard),
additive factor shocks, unshocked holdings, short-leg sign flip,
±100% clamp, magnitude-sorted impacts, asset-class case-insensitive
matching, shock validation, scenario validation.

## REST surface

### Admin (`admin_stress.go`)

```
GET    /api/admin/stress-scenarios[?category=X]    list
POST   /api/admin/stress-scenarios                 upsert by name
DELETE /api/admin/stress-scenarios/{id}            remove
```

All writes go through `h.requireAdmin` and audit-log via
`LogMutation` so operators can trace "who added the COVID
scenario" or "who edited the regulator's standard set".

### Per-fund (`stress_handler.go`)

```
POST /api/funds/{fundId}/risk/stress
  body: { "scenario_id": "<uuid>", "persist": false }

GET  /api/funds/{fundId}/risk/stress/history[?scenarioId=X&limit=N]

GET  /api/risk/stress-scenarios[?category=X]
     # public read of the scenario library for the picker
```

Why POST for the runner — every run touches an external
scenario id, optionally writes an archive row, and we want it
to be non-idempotent (clicking "run again" produces a fresh
snapshot).  GETs would also be confusable with HTTP cache
proxies and we don't want a cached stale stress result.

Fund-access authorisation reuses the shared
`authorizeFundAccess` gate (same as cash ledger, factor
exposure, VaR handlers).

9 handler tests cover: unauth, missing scenario id, scenario
not found, happy path (asset-class shock against AAPL),
persist transactional archive, history happy path.

## UI

### Fund dashboard (`web/src/components/StressTestPanel.tsx`)

- Scenario dropdown grouped by category prefix.
- Run button POSTs to `/api/funds/{fundId}/risk/stress`.
- Header card: NAV before / after, PnL, PnL %, holdings count,
  shocked count.
- Per-holding table sorted by |PnL| desc with applied return
  and matched shock columns so the PM can see *why* each
  position moved.

Wired into `FundPerformance.tsx` after `VaRPanel`.

### Admin (`web/src/components/AdminStressScenariosSection.tsx`)

- Category filter dropdown.
- Inline upsert form: name, category, description, shocks
  array (with add/remove buttons per shock row).
- Per-row delete with confirmation dialog.

Wired into `Admin.tsx` after `AdminFactorExposureSection`.

### i18n

`shared/api-client/src/i18n.ts` adds a `stressTest` namespace
(panel + admin strings); zh-CN + en-US filled.

## What this commit deliberately does NOT do

- **Scheduled stress runs.**  The current runner is on-demand
  (the operator clicks "Run").  A daily / weekly cron that
  runs every scenario against every fund and writes the archive
  belongs to the regulatory-reporting sprint (S9).
- **Reverse stress test.**  Given a target loss, find the
  shock spec that produces it.  Requires an optimisation
  solver and ships separately.
- **Component VaR / scenario risk contribution.**  Today we
  show per-holding P&L, not per-factor or per-sector
  decomposition.  Brinson attribution (S7.4) lands the
  three-bucket asset-allocation / selection / interaction
  view.
- **Android `StressTestPanel`.**  Shared types are published
  in `@fundai/api-client`; the RN client can call the same
  REST surface when the mobile flow needs it.

## Validation gates

- `go build ./...`
- `go test ./...` (including `-race ./internal/stress/...`)
- `tsc -p web --noEmit`
- `tsc -p shared/api-client`
- `eslint --max-warnings 0`
- `scripts/validate-api-contract.mjs` (259 routes ↔ 293 calls)
- `scripts/verify.sh`

All pass with zero warnings as of the commit landing this feature.
