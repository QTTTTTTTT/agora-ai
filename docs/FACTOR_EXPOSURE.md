# Factor Exposure (S7 / P3-1)

## What this is

The first piece of the S7 risk-and-attribution sprint. Given the
current portfolio, compute the six canonical factor exposures and
surface them to the PM dashboard and the Admin page:

- **size** — small minus big
- **value** — high book-to-market minus low book-to-market
- **momentum** — recent winners minus losers
- **quality** — high ROE / low debt minus the opposite
- **lowvol** — low realised vol minus high realised vol
- **market_beta** — sensitivity to the market factor

The exposures complement the per-trade safety gates (S1) and the
concentration / drawdown gates (S5 / P3-5) by surfacing *factor*
risk — the kind of thing that lives in the prospectus risk
section and that every prime broker risk report tracks.

## Data model

### `instrument_factor_loadings` — calibration store

PK on `(instrument_key, factor, asof)`. The Quant Lab batch (S10,
planned) is the primary writer with `source='computed'`. Operators
can hand-write rows with `source='manual'` for typo fixes and
`source='override'` for emergency reweights. Third-party vendors
(`msci`, `eastmoney`) are also accepted.

Loading values are clamped to `[-10, 10]` by a CHECK constraint —
after z-score normalisation the typical range is `[-3, 3]`; the
wider gate prevents the lab from accidentally writing absurd
values without forcing operators to bicker over the exact
percentile.

Two indexes:

- `(instrument_key, factor, asof DESC)` for the hot "latest
  loading for each portfolio holding" query.
- `(factor, asof DESC)` for the admin browser when looking at
  one factor across all instruments.

### `portfolio_factor_snapshots` — archive

Append-only. Every time the live `/api/funds/{id}/risk/factor-exposure?persist=1`
endpoint fires, six rows are written (one per canonical factor)
in a single transaction so trend-line readers never see a
half-written vintage.

`(fund_id, calculated_at DESC, factor)` indexed for the dashboard
sparkline query.

## Pure engine

`internal/factorexposure/engine.go` is pure Go — no I/O, no clock
read (you inject a `Now` for tests). The math:

```
weight_i = MV_i / sum(|MV_j|)
net_F   = sum(weight_i * loading_i_F)
gross_F = sum(|weight_i * loading_i_F|)
coverage_F = sum(|weight_i| for i with non-nil loading)
```

Long-short books use *gross* MV as the weight denominator because
*net* would (1) blow up the weights for short-heavy books and (2)
go negative. Gross gives the correct "fraction of capital at risk
on this name" reading.

Missing loadings contribute zero to both net and gross but reduce
coverage. The UI flags `coverage < 70%` with an amber pill so the
PM knows "0.4 net momentum exposure, but only 75% of book had
loadings" — much more honest than silently treating missing == 0.

## REST surface

### Admin (S7 / P3-1)

```
GET    /api/admin/factor-loadings           list (?factor, ?instrument_key, ?limit, ?offset)
POST   /api/admin/factor-loadings           upsert one (instrument, factor, asof)
DELETE /api/admin/factor-loadings           delete one (instrument, factor, asof)
```

All admin writes audit-log via `LogMutation` so operators can
trace "who pushed what loading when" — especially useful when a
quant lab batch and a manual override disagree.

### Per-fund

```
GET /api/funds/{fundId}/risk/factor-exposure[?persist=1]
GET /api/funds/{fundId}/risk/factor-exposure/trend[?factor=...&limit=N]
```

`persist=1` is best-effort: a `persist_error` is returned in the
JSON body when the archive write fails but the live read
succeeds. The caller (web + future android) decides whether to
retry.

## UI

- `web/src/components/AdminFactorExposureSection.tsx` — admin
  CRUD over the calibration store. Wired into `Admin.tsx`
  alongside the other S5/S6 admin panels.
- `web/src/components/FactorExposurePanel.tsx` — fund-level
  read with bidirectional bars for net exposure (positive grows
  right, negative grows left) and a one-sided blue bar for gross.
  Wired into `FundPerformance.tsx` below the existing
  `StrategyAttributionPanel`.

i18n strings live in `shared/api-client/src/i18n.ts` under the
`factorExposure` namespace; both `zh-CN` and `en-US` are filled.

## Defaults & feature flags

Nothing to flip. The endpoints are always live; if the loadings
table is empty (initial deployment), the engine returns
all-zero rows with `coverage=0` and the UI surfaces the empty
state cleanly. Operators populate loadings either via the admin
UI or by running the Quant Lab batch (S10).

## What this commit does NOT do

- **Computing loadings from scratch** — that requires multi-year
  returns regression against factor return series and lives in
  the Quant Lab (S10).
- **Persisting snapshots on a schedule** — the live read writes
  on-demand via `persist=1`; a background loop that archives one
  snapshot per fund per day will land in a later sprint once the
  trend chart needs continuous data.
- **Android factor-exposure panel** — the shared types are
  published in `@fundai/api-client`, but the RN UI is
  out-of-scope for this PR; android consumers can call the same
  REST endpoint when added.
