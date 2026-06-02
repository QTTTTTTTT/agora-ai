-- Migration 067 — Factor exposure (S7 / P3-1).
--
-- What this stores
--
-- Two tables together model the "what factors is this portfolio
-- really exposed to?" question that complements the S1-S6 work on
-- per-trade safety and concentration limits.
--
--   * `instrument_factor_loadings` — per-(instrument, factor, asof)
--     factor loading. A loading is a unitless number, typically in
--     [-3, +3] after z-score normalisation, that says "how much of
--     factor F does this instrument carry". Six canonical factors
--     are enumerated below.
--
--   * `portfolio_factor_snapshots` — append-only audit of every
--     portfolio-level exposure computation. Lets the UI show
--     trend lines ("our momentum tilt has crept up 0.3σ over the
--     last 30 days") and the operator inspect what was reported
--     to the PM prompt at any point in time.
--
-- Why six factors
--
-- The Fama-French three-factor model (market / size / value) is the
-- academic minimum. Carhart adds momentum; the modern "investable
-- multi-factor" consensus (AQR, MSCI, Russell, BlackRock) adds
-- quality and low-volatility. That's six factors, which is the
-- smallest set that covers what every PM-targeted risk dashboard
-- in the industry shows. We deliberately do NOT include a
-- generic "sector" factor — sector concentration is already
-- handled by internal/exposure's SectorWeight (S1 era).
--
-- Why store loadings instead of recomputing
--
-- Computing factor loadings requires multi-year returns regression
-- against factor return series. That's a Quant Lab batch job
-- (planned for S10), not a synchronous request. By storing the
-- snapshot in this table we let the synchronous "give me exposure
-- for fund X right now" path stay fast (one keyed lookup per
-- holding, one weighted sum across holdings) and let the
-- expensive computation run on its own schedule.
--
-- Why portfolio_factor_snapshots is separate
--
-- The same loading table feeds both:
--   - the live read path (sum across current holdings → return)
--   - the archival path (every time we compute the live read,
--     persist the result for trend / drift detection)
--
-- A flat snapshot table is simpler to query than reconstructing
-- exposures from a historical holdings × loadings join. Disk is
-- cheap; row count stays bounded by (funds × factors × days).

BEGIN;

-- ---------------------------------------------------------------------------
-- Reference: canonical factor names. The CHECK constraint keeps
-- typo-resistant; new factors require a follow-up migration.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS instrument_factor_loadings (
    instrument_key TEXT      NOT NULL,
    factor         TEXT      NOT NULL,
    asof           DATE      NOT NULL,
    loading        DOUBLE PRECISION NOT NULL,
    source         TEXT      NOT NULL DEFAULT 'manual',
    note           TEXT      NOT NULL DEFAULT '',
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (instrument_key, factor, asof),
    CONSTRAINT instrument_factor_loadings_factor_check CHECK (
        factor IN ('size', 'value', 'momentum', 'quality', 'lowvol', 'market_beta')
    ),
    CONSTRAINT instrument_factor_loadings_source_check CHECK (
        source IN ('manual', 'eastmoney', 'msci', 'computed', 'override')
    ),
    -- Loadings outside [-10, 10] are almost certainly data errors;
    -- after z-score normalisation the typical range is [-3, 3].
    -- The wider gate prevents the lab from accidentally writing
    -- absurd values without forcing operators to bicker over the
    -- exact percentile.
    CONSTRAINT instrument_factor_loadings_loading_range CHECK (
        loading >= -10 AND loading <= 10
    )
);

COMMENT ON TABLE instrument_factor_loadings IS
'Per-(instrument, factor, asof) factor loading. The asof key lets the lab append new vintages without losing history; the live read path picks max(asof) <= today.';

-- The hot path is "give me the freshest loading for each holding
-- in fund X, for each of six factors". DESC on asof makes the
-- index serve "latest as-of" lookups directly.
CREATE INDEX IF NOT EXISTS instrument_factor_loadings_latest_idx
    ON instrument_factor_loadings (instrument_key, factor, asof DESC);

-- The admin / lab page filters by factor across all instruments
-- to spot calibration outliers.
CREATE INDEX IF NOT EXISTS instrument_factor_loadings_by_factor_idx
    ON instrument_factor_loadings (factor, asof DESC);

-- ---------------------------------------------------------------------------
-- Portfolio-level snapshots. Append-only; we never UPDATE rows.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS portfolio_factor_snapshots (
    id              BIGSERIAL PRIMARY KEY,
    fund_id         UUID            NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    calculated_at   TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    factor          TEXT            NOT NULL,
    -- net_exposure = sum over holdings of (weight * loading); a
    -- portfolio that's long 10% AAPL (momentum loading +1.2) and
    -- short 5% META (momentum loading +0.8) has net momentum
    -- exposure 0.1*1.2 - 0.05*0.8 = 0.08.
    net_exposure    DOUBLE PRECISION NOT NULL,
    -- gross_exposure = sum of |weight * loading|. Surfaces hidden
    -- factor bets that net-out at zero (e.g. long-short pair
    -- trades that are net-zero momentum but heavily gross-long).
    gross_exposure  DOUBLE PRECISION NOT NULL,
    -- capital_pct is the fraction of NAV that contributed to this
    -- factor calculation. Holdings with missing loadings are
    -- excluded; this column makes "how much of the portfolio did
    -- this number cover?" inspectable.
    capital_pct     DOUBLE PRECISION NOT NULL,
    -- holding_count is the number of distinct holdings that
    -- contributed (i.e. had a non-null loading for this factor).
    holding_count   INTEGER         NOT NULL,
    -- Source lookback day for the loadings used. Lets the UI flag
    -- "loadings stale: last refresh 9 days ago" without joining
    -- back to instrument_factor_loadings.
    loadings_asof   DATE            NOT NULL,
    CONSTRAINT portfolio_factor_snapshots_factor_check CHECK (
        factor IN ('size', 'value', 'momentum', 'quality', 'lowvol', 'market_beta')
    ),
    CONSTRAINT portfolio_factor_snapshots_capital_pct_range CHECK (
        capital_pct >= 0 AND capital_pct <= 1
    )
);

COMMENT ON TABLE portfolio_factor_snapshots IS
'Append-only archive of fund-level factor exposures. Each (fund_id, calculated_at) tuple has exactly six rows (one per factor) for trivially joinable UI rendering.';

-- The most common UI query is "show me the last 30 days of
-- snapshots for fund X" or "show me the latest exposure now".
-- A composite index handles both.
CREATE INDEX IF NOT EXISTS portfolio_factor_snapshots_fund_time_idx
    ON portfolio_factor_snapshots (fund_id, calculated_at DESC, factor);

COMMIT;
