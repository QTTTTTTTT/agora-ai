-- Migration 070 — Brinson attribution (S7 / P3-4).
--
-- Two tables:
--
--   * brinson_benchmark_compositions
--       The Brinson model decomposes a portfolio's active return
--       (portfolio - benchmark) into three effects by bucketing
--       both books along the same dimension (asset_class, market,
--       or a custom sector mapping). For the math we need:
--         w_b[k]  = benchmark weight in bucket k
--         r_b[k]  = benchmark return in bucket k
--         w_p[k]  = portfolio weight (computed live from holdings)
--         r_p[k]  = portfolio return (computed live from holdings)
--       This table stores the benchmark side, admin-managed. One
--       row per (benchmark_id, bucket_dimension, asof) so trend
--       charts can compare "today's allocation effect with last
--       quarter's".
--
--   * brinson_attribution_snapshots
--       Append-only archive of every run, so the operator can
--       point to "this is what we showed the LP on date X". The
--       per-bucket breakdown is JSONB.
--
-- Why benchmark_id is TEXT not UUID — the benchmark catalog is
-- code-level (internal/benchmark/catalog.go), not a DB table.
-- This column stores the catalog Series.ID ("spx", "csi300",
-- ...) which is the stable handle the UI uses.

-- BEGIN;  -- stripped: outer migration runner already wraps each file in a transaction

CREATE TABLE IF NOT EXISTS brinson_benchmark_compositions (
    id               UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    benchmark_id     TEXT         NOT NULL,
    -- Bucket dimension: which column of HoldingPosition to group
    -- portfolio holdings by when matching to benchmark buckets.
    -- "sector" is reserved for a future GICS-style mapping table;
    -- for now we accept asset_class and market.
    bucket_dimension TEXT         NOT NULL,
    asof             DATE         NOT NULL,
    -- buckets is a JSONB array of objects:
    --   { "key": "equity", "weight": 0.65, "return_pct": 0.012 }
    -- Weights should sum to ~1.0 (CHECK could validate this but
    -- the engine does it with a tolerance). return_pct is the
    -- benchmark's realised return for that bucket over the
    -- period the snapshot covers.
    buckets          JSONB        NOT NULL DEFAULT '[]'::jsonb,
    note             TEXT         NOT NULL DEFAULT '',
    created_by       UUID         NULL REFERENCES users(id) ON DELETE SET NULL,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT brinson_compositions_dim_check CHECK (
        bucket_dimension IN ('asset_class', 'market', 'sector')
    ),
    CONSTRAINT brinson_compositions_buckets_array_check CHECK (
        jsonb_typeof(buckets) = 'array'
    ),
    UNIQUE (benchmark_id, bucket_dimension, asof)
);

COMMENT ON TABLE brinson_benchmark_compositions IS
'Benchmark side of the Brinson attribution model. One row per (benchmark_id, bucket_dimension, asof) with weights + returns per bucket stored as JSONB.';

CREATE INDEX IF NOT EXISTS brinson_compositions_bench_dim_asof_idx
    ON brinson_benchmark_compositions (benchmark_id, bucket_dimension, asof DESC);

CREATE TABLE IF NOT EXISTS brinson_attribution_snapshots (
    id                BIGSERIAL    PRIMARY KEY,
    fund_id           UUID         NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    benchmark_id      TEXT         NOT NULL,
    bucket_dimension  TEXT         NOT NULL,
    composition_id    UUID         NOT NULL REFERENCES brinson_benchmark_compositions(id) ON DELETE CASCADE,
    calculated_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    -- Aggregate effects across all buckets. The active return
    -- decomposition identity:
    --   r_p - r_b = sum_k allocation_k + selection_k + interaction_k
    allocation_total   NUMERIC(12, 8) NOT NULL,
    selection_total    NUMERIC(12, 8) NOT NULL,
    interaction_total  NUMERIC(12, 8) NOT NULL,
    active_return      NUMERIC(12, 8) NOT NULL,
    portfolio_return   NUMERIC(12, 8) NOT NULL,
    benchmark_return   NUMERIC(12, 8) NOT NULL,
    bucket_count       INTEGER      NOT NULL,
    -- Per-bucket detail as JSONB:
    --   [{ "key": "equity",
    --      "portfolio_weight": 0.72, "benchmark_weight": 0.65,
    --      "portfolio_return": 0.018, "benchmark_return": 0.012,
    --      "allocation_effect": 0.00084, ... }]
    bucket_details     JSONB        NOT NULL DEFAULT '[]'::jsonb,
    CONSTRAINT brinson_snapshots_dim_check CHECK (
        bucket_dimension IN ('asset_class', 'market', 'sector')
    )
);

COMMENT ON TABLE brinson_attribution_snapshots IS
'Append-only archive of Brinson attribution runs. composition_id links the snapshot to the benchmark composition row used at run-time so the math is reproducible.';

CREATE INDEX IF NOT EXISTS brinson_snapshots_fund_time_idx
    ON brinson_attribution_snapshots (fund_id, calculated_at DESC);

CREATE INDEX IF NOT EXISTS brinson_snapshots_bench_time_idx
    ON brinson_attribution_snapshots (benchmark_id, calculated_at DESC);

-- COMMIT;  -- stripped: outer migration runner already wraps each file in a transaction
