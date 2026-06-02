-- Migration 068 — VaR / CVaR snapshots (S7 / P3-2).
--
-- What this stores
--
-- An append-only archive of every VaR / CVaR computation. The
-- live read path (GET /api/funds/{fundId}/risk/var) computes on
-- demand from nav_snapshots.daily_return and optionally persists
-- the result here; the trend chart and the daily compliance
-- packet read from the archive.
--
-- Why three methods
--
--   * historical — non-parametric, no distributional assumption.
--     Robust to fat tails but only sees what's actually happened.
--     Sensitive to sample window: 252 trading days = one year of
--     stress + calm; 60 days = sensitive but noisy.
--
--   * parametric — assumes daily returns are normally distributed
--     with mean μ and std σ. Computes VaR = μ - z·σ for the
--     confidence's z-score. Fast, but understates tail risk
--     because real return distributions are leptokurtic
--     ("fatter tails than normal").
--
--   * monte_carlo — simulates 10 000+ draws from N(μ, σ) and
--     takes the empirical percentile. With normal sampling it
--     converges to parametric; useful as scaffolding for later
--     migration to non-normal distributions (Student-t, EWMA).
--
-- Every method gets its own row so the UI / compliance packet
-- can present "VaR_95(historical) = -2.3% vs VaR_95(normal) =
-- -1.8%" side by side — the spread itself is a fat-tail
-- diagnostic.

BEGIN;

CREATE TABLE IF NOT EXISTS portfolio_var_snapshots (
    id                  BIGSERIAL PRIMARY KEY,
    fund_id             UUID            NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    calculated_at       TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    method              TEXT            NOT NULL,
    -- Confidence is stored as a numeric fraction (0.95, 0.99).
    -- Constrained to a small allowed set to keep the API
    -- comparable across snapshots.
    confidence          NUMERIC(4, 3)   NOT NULL,
    -- Horizon in trading days. 1d is the regulator default; 5d
    -- and 10d are common for swing strategies. > 10d horizons
    -- get unreliable because mean-reversion / regime-switching
    -- dominates the gaussian random walk we square-root scale.
    horizon_days        INTEGER         NOT NULL,
    -- VaR is the loss threshold at the given confidence. We
    -- store as a NEGATIVE percentage of NAV so the
    -- "more negative = bigger loss" convention matches the UI.
    -- Example: var_pct = -0.023 means "we are 95% confident
    -- one-day loss won't exceed 2.3% of NAV".
    var_pct             NUMERIC(10, 6)  NOT NULL,
    -- CVaR is the expected loss given that VaR was breached.
    -- Same sign convention. Always at least as negative as VaR.
    cvar_pct            NUMERIC(10, 6)  NOT NULL,
    -- Sample window the historical method used (NULL for
    -- parametric / monte_carlo if they were fed only μ + σ
    -- summary statistics; in practice all three currently
    -- consume the same nav_snapshots window).
    sample_window_start DATE            NULL,
    sample_window_end   DATE            NULL,
    -- sample_size is the number of daily returns that fed the
    -- computation. <30 → the UI flags "small sample" because
    -- both methods are unreliable below that floor.
    sample_size         INTEGER         NOT NULL,
    -- lookback_days is the operator-requested lookback (e.g.
    -- 252). Lets the UI render "VaR over last 1y vs last 3y"
    -- comparisons without recomputing.
    lookback_days       INTEGER         NOT NULL,
    -- mean_daily_return + stdev_daily_return are echoed for the
    -- compliance packet. NULL when not computable (sample < 2).
    mean_daily_return   NUMERIC(12, 8)  NULL,
    stdev_daily_return  NUMERIC(12, 8)  NULL,
    -- For Monte Carlo, the seed lets us reproduce the exact
    -- snapshot. NULL for the deterministic methods.
    monte_carlo_seed    BIGINT          NULL,
    monte_carlo_paths   INTEGER         NULL,
    CONSTRAINT portfolio_var_snapshots_method_check CHECK (
        method IN ('historical', 'parametric', 'monte_carlo')
    ),
    CONSTRAINT portfolio_var_snapshots_confidence_check CHECK (
        confidence IN (0.90, 0.95, 0.99)
    ),
    CONSTRAINT portfolio_var_snapshots_horizon_check CHECK (
        horizon_days BETWEEN 1 AND 20
    ),
    CONSTRAINT portfolio_var_snapshots_var_negative_check CHECK (
        var_pct <= 0 AND cvar_pct <= 0
    ),
    CONSTRAINT portfolio_var_snapshots_cvar_at_least_var CHECK (
        -- CVaR is the conditional tail expectation, which is
        -- always at least as negative as VaR (or equal in
        -- degenerate cases).
        cvar_pct <= var_pct
    )
);

COMMENT ON TABLE portfolio_var_snapshots IS
'Append-only archive of fund-level Value-at-Risk and Conditional VaR computations. One row per (fund, method, confidence, horizon, calculated_at). Live read computes on demand; persisted snapshots feed the trend chart and the daily compliance packet.';

-- The dashboard / sparkline query is "last N snapshots for fund
-- X". A composite (fund_id, calculated_at DESC) index serves it
-- directly.
CREATE INDEX IF NOT EXISTS portfolio_var_snapshots_fund_time_idx
    ON portfolio_var_snapshots (fund_id, calculated_at DESC);

-- The "tile per (method, confidence)" UI uses the latest row of
-- each combo. The partial index trims the working set so the
-- LIMIT 1 + ORDER BY scan stays in cache.
CREATE INDEX IF NOT EXISTS portfolio_var_snapshots_latest_idx
    ON portfolio_var_snapshots (fund_id, method, confidence, horizon_days, calculated_at DESC);

COMMIT;
