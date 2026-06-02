# Value-at-Risk + Conditional VaR (S7 / P3-2)

## Purpose

Compute and expose one-period maximum-loss estimates for a fund's
NAV at the three canonical regulatory confidence levels (90% / 95% /
99%) using three complementary methods.  The methods share input
(daily returns from `nav_snapshots.daily_return`) but make different
distributional assumptions; the spread between them is itself the
diagnostic — calm markets shrink it, fat-tail regimes blow it open.

This is the second piece of the S7 Risk & Attribution sprint, after
S7.1 (Factor Exposure).

## Data model

### `nav_snapshots` (existing, reused as input)

The engine consumes `(trading_date, daily_return)` pairs from this
table.  No schema changes here — the column already existed and
`fund_repo` writes one row per fund per trading day.

### `portfolio_var_snapshots` (new, append-only archive)

```sql
CREATE TABLE portfolio_var_snapshots (
    id                  BIGSERIAL PRIMARY KEY,
    fund_id             UUID            NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    calculated_at       TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    method              TEXT            NOT NULL,   -- historical | parametric | monte_carlo
    confidence          NUMERIC(4, 3)   NOT NULL,   -- 0.90 | 0.95 | 0.99
    horizon_days        INTEGER         NOT NULL,   -- 1..20
    var_pct             NUMERIC(10, 6)  NOT NULL,   -- negative fraction of NAV
    cvar_pct            NUMERIC(10, 6)  NOT NULL,   -- always <= var_pct
    sample_window_start DATE            NULL,
    sample_window_end   DATE            NULL,
    sample_size         INTEGER         NOT NULL,
    lookback_days       INTEGER         NOT NULL,
    mean_daily_return   NUMERIC(12, 8)  NULL,
    stdev_daily_return  NUMERIC(12, 8)  NULL,
    monte_carlo_seed    BIGINT          NULL,
    monte_carlo_paths   INTEGER         NULL,
    -- Range / sign constraints documented in migration 068.
);
```

The table is **append-only**: a snapshot is one logical bundle of
(typically nine) rows sharing a `calculated_at`.  No `UNIQUE`
constraint, no `UPDATE` path.  Re-computing means writing a new
bundle.

Indexes:

- `(fund_id, calculated_at DESC)` — sparkline / "last N snapshots".
- `(fund_id, method, confidence, horizon_days, calculated_at DESC)`
  — "latest tile per (method, confidence) combo".

## Engine (`server/internal/varisk`)

Pure-Go package, no external deps beyond stdlib + lib/pq for the
repo.  Stateless `Engine` struct — safe for concurrent use.

### Sign convention

`Var <= 0` and `CVar <= Var`.  Example: `var_pct = -0.023` means
"we are 95% confident the one-day loss won't exceed 2.3% of NAV".
The constraints are enforced by the engine, repeated in the DB
CHECK constraints, and asserted in unit tests.

### Method 1 — Historical simulation

```
sort(returns)
VaR  = percentile(sorted, 1 - confidence)        // NumPy "linear" convention
CVaR = mean of all observations <= VaR
```

Non-parametric, no distributional assumption.  Picks up regime
shifts (e.g. March 2020) that parametric will smooth over.
Weakness: only sees what's actually happened — a one-year window
that didn't include a tail event will under-report.

### Method 2 — Parametric (variance-covariance, normal)

```
μ  = mean(returns)
σ  = stdev(returns, ddof=1)
z  = ppf(confidence)            // 1.282 / 1.645 / 2.326
VaR  = μ - z · σ
CVaR = μ - σ · φ(z) / (1 - confidence)         // closed-form ES under normality
```

Where `φ` is the standard normal PDF.  Fast and stable, but
understates real tail risk because returns are leptokurtic ("fatter
tails than normal").  Always compare against historical when
making capital decisions.

### Method 3 — Monte Carlo

```
sample[i] = μ + σ · N(0, 1)                    for i in [0, paths)
sort(sample)
VaR  = percentile(sample, 1 - confidence)
CVaR = mean of sample[j] for sample[j] <= VaR
```

With normal sampling this converges to parametric as `paths -> ∞`;
default `paths = 50 000` gives stable percentiles to ~3 digits at
99% confidence.  Useful as scaffolding for the planned non-normal
upgrade (Student-t / EWMA-weighted historical) without changing
the call sites.

### Horizon scaling

All three methods compute one-day VaR/CVaR and scale by
`sqrt(horizon)` — the standard square-root-of-time approximation
under IID returns.  Horizons > 10d become unreliable because mean
reversion and regime switching dominate the random walk.

### Sample size guard

Below `MinSampleSize = 20` daily returns, the engine refuses to
compute and the handler returns HTTP 422 `insufficient_history`.
Samples 20-30 are computed but the UI flags them as "low
confidence" in the tile subtitle (TODO once we add that hint).

## REST surface

### Per-fund read (`var_handler.go`)

```
GET /api/funds/{fundId}/risk/var
GET /api/funds/{fundId}/risk/var?lookback=252&horizon=1
GET /api/funds/{fundId}/risk/var?lookback=60&horizon=5&persist=1

GET /api/funds/{fundId}/risk/var/trend?method=historical&confidence=0.95&horizon=1
GET /api/funds/{fundId}/risk/var/trend?method=monte_carlo&confidence=0.99&horizon=1&limit=180
```

The snapshot route returns the full 9-tile result by default
(`3 methods × 3 confidences`).  Pass `?persist=1` to archive the
result in `portfolio_var_snapshots` in the same round trip; the
write is best-effort and a failure is surfaced via
`persist_error` without blocking the read.

Authorisation reuses the shared `authorizeFundAccess` gate
(same as `cash_ledger_handler.go` and the factor exposure handler).

### Why we don't expose Monte Carlo as the default

All nine tiles are shown together.  Picking only one method would
defeat the purpose — the agreement / disagreement between methods
is the signal.

## UI

### `web/src/components/VaRPanel.tsx`

- Horizon dropdown (1 / 5 / 10 days) on the header.
- Header card: sample size, lookback, mean daily return, daily
  volatility, sample window dates.
- Body: three method rows (with subtitle explaining how the
  method works) × three confidence tiles each — VaR / CVaR side by
  side.  Numbers are formatted as signed percentages with 2 dp.
- Footer card: short interpretation of "what VaR means" and
  "what CVaR means" so the PM doesn't mis-read the sign.

Polls every 5 minutes by default and on horizon change.

Wired into `web/src/pages/FundPerformance.tsx` after
`FactorExposurePanel`.

### i18n

`shared/api-client/src/i18n.ts` adds a `varRisk` namespace; both
`zh-CN` and `en-US` filled.

## What this commit deliberately does NOT do

- **Continuous archival loop.**  The endpoint persists on demand
  (`?persist=1`).  A scheduled "daily VaR write" loop is planned
  for the next sprint when the trend chart goes from "manual
  snapshots" to "always-on".
- **Backtested VaR breaches.**  We compute today's VaR but don't
  yet keep score of how many days the realised loss exceeded the
  VaR threshold.  That goes with the regulatory-reporting sprint
  (S9 backtesting / S10 quant lab).
- **Component VaR / risk contribution.**  Currently we treat the
  portfolio's NAV-return time series as a black box.  Adding
  per-asset risk contribution requires the position-level
  covariance matrix and lands with S7.4 (Brinson attribution UI).
- **Android `VaRPanel`.**  Shared types are published in
  `@fundai/api-client` so the RN client can call the same REST
  surface; the panel UI itself lands when the mobile flow needs
  it.

## Validation gates

- `go build ./...`
- `go test ./...` (including `-race ./internal/varisk/...`)
- `tsc -p web --noEmit`
- `tsc -p shared/api-client`
- `eslint --max-warnings 0`
- `scripts/validate-api-contract.mjs` (routes ↔ client calls)
- `scripts/verify.sh`

All pass with zero warnings as of the commit landing this feature.
