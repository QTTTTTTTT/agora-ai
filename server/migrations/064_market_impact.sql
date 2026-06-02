-- Migration 064 — market-impact calibration store (S6.2).
--
-- What this stores
--
-- Per-instrument liquidity + volatility parameters that drive the
-- size-aware slippage model. Each row is "what would a trade of
-- size Q against this name move the price by?".
--
-- Why per-instrument and not per-asset-class
--
-- Two SP500 names with the same notional impact wildly different
-- amounts — TSLA at 1% of ADV is much more painful than KO at 1%
-- of ADV. Asset-class defaults are a fallback when no row exists;
-- this table is where operators land calibrated numbers.
--
-- Why the "min_slippage_bps" / "max_slippage_bps" bounds
--
-- Two reasons:
--   * Floor — for very small orders, the spread crossing is the
--     dominant cost. The square-root model produces near-zero
--     impact for tiny orders, but in reality you still pay half
--     the spread. The floor pins the result above 0.
--   * Ceiling — for orders sized > ADV (rare, but possible during
--     emergency exits), the model extrapolates to cartoon numbers.
--     Cap at e.g. 500 bps so a buggy ADV (e.g. 0) doesn't cause
--     a 99% slippage bill in the simulator.

CREATE TABLE IF NOT EXISTS instrument_liquidity (
    instrument_key      VARCHAR(64) PRIMARY KEY,
    -- canonical symbol mirrored for admin list view convenience.
    symbol              VARCHAR(64) NOT NULL,
    market              VARCHAR(16) NOT NULL,
    asset_class         VARCHAR(24) NOT NULL DEFAULT 'equity',
    -- adv_shares is the average daily volume in shares (or
    -- contracts for futures). NULL = unknown; the adapter falls
    -- back to spread-cross when ADV is missing.
    adv_shares          NUMERIC(20, 4),
    -- adv_notional is the same number expressed in quote
    -- currency, included to make calibration spreadsheets
    -- consumable without needing a price (and to support
    -- cross-asset notional comparisons in admin lists).
    adv_notional        NUMERIC(20, 2),
    -- adv_window_days is the lookback used for the average
    -- (typical: 20). Stored so an auditor can reconstruct the
    -- math.
    adv_window_days     INT NOT NULL DEFAULT 20
                          CHECK (adv_window_days BETWEEN 1 AND 252),
    -- daily_volatility is sigma (e.g. 0.02 = 2%/day). Used by
    -- the square-root impact model; missing means the engine
    -- substitutes the asset-class default.
    daily_volatility    NUMERIC(8, 6),
    -- impact_coefficient and impact_exponent shape the
    -- adverse-bps formula:
    --   bps = daily_volatility * coef * (qty/adv_shares)^exponent * 10000
    -- Defaults sized to land near academic findings:
    --   coef = 1.0, exponent = 0.5  → square-root law.
    impact_coefficient  NUMERIC(8, 4) NOT NULL DEFAULT 1.0
                          CHECK (impact_coefficient > 0 AND impact_coefficient <= 10),
    impact_exponent     NUMERIC(4, 3) NOT NULL DEFAULT 0.5
                          CHECK (impact_exponent > 0 AND impact_exponent <= 1),
    -- Floor / ceiling bounds applied to the engine's output.
    -- Sane defaults: 1 bps min (the spread half), 500 bps max
    -- (sanity cap for misconfig).
    min_slippage_bps    NUMERIC(8, 2) NOT NULL DEFAULT 1
                          CHECK (min_slippage_bps >= 0 AND min_slippage_bps <= 1000),
    max_slippage_bps    NUMERIC(8, 2) NOT NULL DEFAULT 500
                          CHECK (max_slippage_bps >= 0 AND max_slippage_bps <= 5000),
    -- last_calibrated_at: when the operator (or an automated
    -- calibrator) last refreshed adv / sigma / coefficients.
    -- Surfaced in the admin UI so stale calibrations are
    -- visible.
    last_calibrated_at  TIMESTAMPTZ,
    -- 'manual' = operator typed in the admin UI.
    -- 'historical' = automated job filled from price history.
    -- 'broker_reported' = imported from broker analytics.
    calibration_source  VARCHAR(24) NOT NULL DEFAULT 'manual'
                          CHECK (calibration_source IN ('manual', 'historical', 'broker_reported')),
    note                TEXT,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by          UUID,
    -- Sanity invariant: lower bound cannot exceed upper.
    CHECK (min_slippage_bps <= max_slippage_bps)
);

CREATE INDEX IF NOT EXISTS instrument_liquidity_market_idx
    ON instrument_liquidity (market, asset_class);
CREATE INDEX IF NOT EXISTS instrument_liquidity_symbol_idx
    ON instrument_liquidity (symbol);
