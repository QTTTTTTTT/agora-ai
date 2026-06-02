# Brinson Attribution (S7 / P3-4)

## Purpose

Decompose a fund's **active return** (portfolio − benchmark) into
three independent effects per bucket, so PMs can answer:

* **Allocation effect** — did we beat / lag the benchmark because
  we overweighted the right slices? `(w_p[k] − w_b[k]) · r_b[k]`
* **Selection effect** — within each slice, did our stock picks
  beat the slice's benchmark return? `w_b[k] · (r_p[k] − r_b[k])`
* **Interaction effect** — did the bets compound favourably?
  `(w_p[k] − w_b[k]) · (r_p[k] − r_b[k])`

Identity: `r_p − r_b = Σ_k (allocation_k + selection_k + interaction_k)`.

## Data model

Two tables (migration `070_brinson_attribution.sql`):

| Table | Purpose |
| --- | --- |
| `brinson_benchmark_compositions` | Admin-managed benchmark side. One row per `(benchmark_id, bucket_dimension, asof)` with `buckets` JSONB array `[{key, weight, return_pct}]`. |
| `brinson_attribution_snapshots` | Append-only archive of every run. Aggregate effects in real columns, per-bucket detail in `bucket_details` JSONB. References the composition used so the math is reproducible. |

Bucket dimensions: `asset_class`, `market`, `sector` (sector is
schema-ready but the per-fund handler can't extract it from
`holding_positions` yet — see *Scope*).

## Engine

`internal/brinson`:

* `Engine.Compute(portfolio, benchmark) → Result`
  * Case-insensitive key matching.
  * Buckets present on only one side still contribute to the
    identity (missing side has `w=0`, `r=0`).
  * Per-bucket rows sorted by `|total_effect|` descending.
* `PortfolioFromHoldings(dim, holdings, asof) → Composition`
  * Derives portfolio buckets MV-weighted from current holdings.
  * Shorts contribute `|MV|` to bucket notional but their return
    is naturally sign-flipped by the negative MV propagation.

Validation rejects negative weights, weights >5× (caller passed
percentages instead of fractions), duplicate keys, and weight sums
outside `[0.99, 1.01]`.

## REST surface

**Admin** (require admin role, audit-logged):

| Method | Path | Description |
| --- | --- | --- |
| GET | `/api/admin/brinson-compositions[?benchmarkId=&dimension=&limit=]` | List rows. |
| POST | `/api/admin/brinson-compositions` | Upsert by `(benchmark_id, dimension, asof)`. |
| DELETE | `/api/admin/brinson-compositions/{id}` | Hard delete (cascades archived snapshots). |

**Per-fund** (authenticated, `authorizeFundAccess`):

| Method | Path | Description |
| --- | --- | --- |
| POST | `/api/funds/{fundId}/brinson/run` | Run attribution. Body: `{benchmark_id, dimension, composition_id?, asof?, persist?}`. Returns the full `Result` plus the `composition_id` used. |
| GET | `/api/funds/{fundId}/brinson/history[?benchmarkId=&dimension=&limit=]` | Archived runs newest-first. |

**Catalog** (any authenticated user):

| Method | Path | Description |
| --- | --- | --- |
| GET | `/api/brinson/benchmarks` | Deduped `(benchmark_id, dimension, latest_asof)` rows for the fund-level dropdown. |

`POST` is intentional on the run path — every run touches an
external `composition_id`, optionally writes an archive row, and
must not be cached.

## UI

* `web/src/components/BrinsonAttributionPanel.tsx`
  * Fund-level dashboard panel. Benchmark + dimension picker, run
    button, the 6-tile aggregate decomposition card, and the
    per-bucket drill-down table (signed colour coding: green
    positive / red negative).
* `web/src/components/AdminBrinsonCompositionsSection.tsx`
  * Admin CRUD: inline upsert form with dynamic bucket rows,
    per-row delete with confirmation, dimension filter.

Both wired into `web/src/pages/FundPerformance.tsx` and
`web/src/pages/Admin.tsx` respectively.

## i18n

`shared/api-client/src/i18n.ts` adds the `brinsonAttribution`
namespace with full `zh-CN` and `en-US` strings.

## Scope exclusions (deferred to a future sprint)

* **Sector dimension wiring**: `holding_positions` has no sector
  column today. The dimension enum and DB CHECK both accept
  `sector`, the engine handles it, but `holdingsToBrinsonInputs`
  currently skips sector rows. Plan: add `sector` to
  `instruments` (preferred — single source of truth) or
  `holding_positions`, then drop the skip.
* **Multi-period (geometric) linking**: the engine reports
  single-period Brinson. Linking quarterly snapshots into a
  cumulative attribution (Carino, Frongello, etc.) belongs in
  the next sprint.
* **Currency separation**: cross-currency funds collapse the
  FX contribution into the selection bucket. A proper Brinson-
  Fachler / Karnosky-Singer extension is queued behind FX P&L
  (S4) finalisation.
