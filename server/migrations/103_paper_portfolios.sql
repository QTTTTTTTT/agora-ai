-- Migration: 103_paper_portfolios
-- Description:
--   Stage 4 — Paper Trading + SHA256 + OpenTimestamps shell.
--
--   The user's plan needs a tamper-evident performance archive for
--   the public-facing "AI Investment Research Co-Pilot" SaaS. The
--   core trick is:
--
--     1. AI proposes a target portfolio at decision time T.
--     2. The proposal payload (symbol, target weight, reasoning) is
--        canonicalised and SHA256'd; the hash is published to
--        Twitter / Discord / blog within minutes — and stamped via
--        OpenTimestamps so the Bitcoin-anchored timestamp proves the
--        hash existed by T.
--     3. The next trading day's open price is used as the executed
--        price; we record (decided_price, executed_price) so the user
--        can verify "no after-the-fact tweaking happened".
--     4. NAV is updated daily by re-pricing positions.
--
--   This migration just adds the tables; the SHA256 + OTS-stub logic
--   lives in internal/papertrading. Adding the tables now means the
--   service layer can ship behind a feature flag without a follow-up
--   schema bump.
--
--   Schema overview:
--     paper_portfolios            one row per public-facing portfolio
--                                 (strategy + market + cohort).
--     paper_orders                append-only ledger of AI proposals.
--                                 hash_signature + (later) ots_proof_url
--                                 prove the proposal existed at T.
--     paper_holdings_snapshots    per-portfolio per-day snapshot of
--                                 current weights + cash + total_value.
--     paper_nav_history           per-portfolio per-day NAV + daily
--                                 return + benchmark NAV. Frontend
--                                 plots this directly.

CREATE TABLE IF NOT EXISTS paper_portfolios (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name              TEXT NOT NULL,
    strategy          TEXT NOT NULL,                    -- e.g. "momentum_top30_monthly"
    market            TEXT NOT NULL,                    -- "us_equity" / "a_share"
    benchmark_symbol  TEXT,                             -- "IWM" / "000300.SH"
    initial_capital   NUMERIC(20,4) NOT NULL,
    current_nav       NUMERIC(20,4) NOT NULL,
    cash_balance      NUMERIC(20,4) NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_rebalance_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_paper_portfolios_market_strategy
    ON paper_portfolios(market, strategy);

CREATE TABLE IF NOT EXISTS paper_orders (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    portfolio_id      UUID NOT NULL REFERENCES paper_portfolios(id) ON DELETE CASCADE,
    symbol            TEXT NOT NULL,
    action            TEXT NOT NULL CHECK (action IN ('BUY','SELL','REBALANCE')),
    target_weight     NUMERIC(8,6),                 -- post-trade target weight (0.0-1.0)
    shares_change     NUMERIC(20,4),                -- positive for buys, negative for sells
    decided_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    decided_price     NUMERIC(20,4),                -- snapshot price at decision time
    executed_at       TIMESTAMPTZ,                  -- next-day fill time
    executed_price    NUMERIC(20,4),                -- next-day open price
    ai_reasoning      JSONB,                        -- full agent vote / reasoning blob
    hash_signature    TEXT NOT NULL,                -- SHA-256 hex of canonical payload
    canonical_payload TEXT NOT NULL,                -- the exact JSON we hashed (for verifier)
    public_proof_url  TEXT,                         -- Twitter / Discord / OTS proof URL
    ots_status        TEXT NOT NULL DEFAULT 'pending' CHECK (ots_status IN ('pending','submitted','confirmed','disabled'))
);

CREATE INDEX IF NOT EXISTS idx_paper_orders_portfolio_decided
    ON paper_orders(portfolio_id, decided_at DESC);

CREATE INDEX IF NOT EXISTS idx_paper_orders_hash
    ON paper_orders(hash_signature);

CREATE TABLE IF NOT EXISTS paper_holdings_snapshots (
    portfolio_id   UUID NOT NULL REFERENCES paper_portfolios(id) ON DELETE CASCADE,
    snapshot_date  DATE NOT NULL,
    holdings       JSONB NOT NULL,                 -- {symbol -> {shares, market_value, weight}}
    cash_balance   NUMERIC(20,4) NOT NULL DEFAULT 0,
    total_value    NUMERIC(20,4) NOT NULL,
    PRIMARY KEY (portfolio_id, snapshot_date)
);

CREATE TABLE IF NOT EXISTS paper_nav_history (
    portfolio_id   UUID NOT NULL REFERENCES paper_portfolios(id) ON DELETE CASCADE,
    snapshot_date  DATE NOT NULL,
    nav            NUMERIC(20,4) NOT NULL,
    daily_return   NUMERIC(10,6),                  -- fraction; 0.012 = +1.2%
    benchmark_nav  NUMERIC(20,4),                  -- optional, fed by ohlc.Fetcher
    PRIMARY KEY (portfolio_id, snapshot_date)
);
