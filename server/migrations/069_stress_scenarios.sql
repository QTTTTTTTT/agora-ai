-- Migration 069 — Stress scenarios + per-fund stress results
-- (S7 / P3-3).
--
-- Two tables:
--
--   * stress_scenarios — definitions, admin-managed. Each row is
--     a named scenario like "2008 Lehman" or "COVID Mar 2020".
--     Shocks are stored as a JSONB array of {target_type,
--     target_key, value} so a single scenario can mix asset-class
--     shocks (equity = -40%), market shocks (US = -25%),
--     instrument-specific shocks (US:AAPL = -10%), and
--     factor-based shocks (momentum = -2 sigma) without the
--     schema growing N relations.
--
--   * portfolio_stress_results — append-only archive of every
--     (fund, scenario, calculated_at) run. Stores per-holding
--     impact as JSONB so the UI can drill from "fund lost X" to
--     "AAPL contributed -2.3% of NAV".
--
-- Shock matching priority (engine):
--   instrument_key (exact)
--   > market
--   > asset_class
--   > factor (via instrument_factor_loadings lookup)
--   > wildcard "*"
--
-- A holding picks the most-specific match and applies it. Multiple
-- factor shocks compound additively (loading_size * shock_size +
-- loading_value * shock_value + ...). Asset-class / market /
-- instrument shocks are mutually exclusive — the highest-priority
-- match wins.

-- BEGIN;  -- stripped: outer migration runner already wraps each file in a transaction

CREATE TABLE IF NOT EXISTS stress_scenarios (
    id           UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    name         TEXT         NOT NULL UNIQUE,
    category     TEXT         NOT NULL,
    description  TEXT         NOT NULL DEFAULT '',
    -- JSONB array of shock objects. Each object has the shape:
    --   {
    --     "target_type": "instrument" | "asset_class" | "market" | "factor" | "wildcard",
    --     "target_key":  string (instrument_key / asset_class name / market code / factor name / "*"),
    --     "value":       number (decimal fraction; -0.20 = -20% return shock)
    --   }
    -- The CHECK constraint validates it's a non-null array but
    -- not the per-element shape (Postgres can't reach that far);
    -- the engine + handler validate shape on read/write.
    shocks       JSONB        NOT NULL DEFAULT '[]'::jsonb,
    -- Operator who last touched the row. Nullable to allow
    -- migration of seed scenarios that have no creator.
    created_by   UUID         NULL REFERENCES users(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT stress_scenarios_category_check CHECK (
        category IN ('historical', 'hypothetical', 'regulatory')
    ),
    CONSTRAINT stress_scenarios_shocks_array_check CHECK (
        jsonb_typeof(shocks) = 'array'
    )
);

COMMENT ON TABLE stress_scenarios IS
'Named stress-test scenarios with a JSONB array of shock specs. Categories: historical (replays of 2008/COVID/etc.), hypothetical (forward-looking what-ifs), regulatory (FSB / CSRC standard scenarios).';

CREATE INDEX IF NOT EXISTS stress_scenarios_category_idx
    ON stress_scenarios (category, name);

CREATE TABLE IF NOT EXISTS portfolio_stress_results (
    id             BIGSERIAL    PRIMARY KEY,
    fund_id        UUID         NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    scenario_id    UUID         NOT NULL REFERENCES stress_scenarios(id) ON DELETE CASCADE,
    calculated_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    -- nav_before is the gross MV the engine saw (sum of |MV|).
    -- nav_after is the projected gross MV under the shock.
    -- pnl_total = nav_after - nav_before (signed; negative is a loss).
    -- pnl_pct   = pnl_total / nav_before (signed fraction of NAV).
    nav_before     NUMERIC(20, 6) NOT NULL,
    nav_after      NUMERIC(20, 6) NOT NULL,
    pnl_total      NUMERIC(20, 6) NOT NULL,
    pnl_pct        NUMERIC(10, 6) NOT NULL,
    -- holding_count is the number of positions evaluated;
    -- shocked_count is the subset that matched at least one
    -- shock. (NAV - shocked) / NAV is the "coverage" of the
    -- scenario relative to the book.
    holding_count  INTEGER      NOT NULL,
    shocked_count  INTEGER      NOT NULL,
    -- Per-holding impact rows for the UI drill-down.
    -- JSONB array of:
    --   { "instrument_key", "symbol", "asset_class",
    --     "market_value_before", "market_value_after", "pnl",
    --     "applied_shock": { "target_type", "target_key", "value" } | null }
    impacts        JSONB        NOT NULL DEFAULT '[]'::jsonb
);

COMMENT ON TABLE portfolio_stress_results IS
'Append-only archive of stress-test runs. One row per (fund, scenario, calculated_at). impacts JSONB carries per-holding P&L so the UI can drill from "fund lost X%" to the contributing positions.';

CREATE INDEX IF NOT EXISTS portfolio_stress_results_fund_time_idx
    ON portfolio_stress_results (fund_id, calculated_at DESC);

CREATE INDEX IF NOT EXISTS portfolio_stress_results_scenario_idx
    ON portfolio_stress_results (scenario_id, calculated_at DESC);

-- COMMIT;  -- stripped: outer migration runner already wraps each file in a transaction
