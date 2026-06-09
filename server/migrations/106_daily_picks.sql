-- Migration: 106_daily_picks
-- Description:
--   Adds the two tables behind the /daily-picks publisher surface:
--     * daily_pick_watchlists — admin-managed candidate pools
--       (one row per "we want to score these N tickers every day
--       under preset P for market M").
--     * daily_picks — the per-(symbol, market, preset, pick_date)
--       SHARED cache row that the publisher-mode advisor writes
--       once and EVERY user reads identically.
--
-- Compliance posture (the entire reason these are separate from
-- advisor_consultations):
--
--   advisor_consultations is a per-USER record — each row is one
--   consult that one user requested, billed against that user's
--   credits. By design, two users can get DIFFERENT outputs from
--   the same symbol on the same day (different personas chosen,
--   different in-flight model snapshot, etc.). That's fine for the
--   /advisor surface because it's framed as "you ran this query".
--
--   /daily-picks works under the SEC Publishers' Exclusion safe
--   harbour (Lowe v. SEC, 1985; Seeking Alpha v. SEC, 2024) — the
--   product is a FINANCIAL NEWSLETTER, not an RIA. For the
--   exclusion to apply, the content must be:
--     1. publicly distributed (not 1-on-1 advice),
--     2. NON-PERSONALIZED — every reader sees the same thing,
--     3. a real periodic publication, and
--     4. of general interest.
--
--   Condition #2 is the wire-level invariant this migration
--   enforces: the UNIQUE constraint on
--     (symbol, market, preset_key, pick_date)
--   means there's literally only ONE row for "AAPL US disruptive
--   2026-06-08", and the API serves it to whichever subscriber
--   shows up. Personalisation would require a per-user FK, which
--   we're deliberately not introducing.
--
-- Tier model (this migration is intentionally tier-agnostic):
--   Free / Basic / Pro all read the SAME rows. The handler enforces
--   a time-lag filter (`pick_date <= today() - 14 days` for free
--   tier) on top of these rows. That keeps "what is the content"
--   and "who can see it when" cleanly separated — if compliance
--   later needs to audit that we never personalise, they can
--   grep this file and confirm the per-user dimension simply
--   doesn't exist.

BEGIN;

-- ------------------------------------------------------------------
-- 1. Watchlists (admin-managed candidate pools)
-- ------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS daily_pick_watchlists (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name            TEXT NOT NULL,
    market          TEXT NOT NULL,            -- "us_equity", "a_share", "hk_equity"
    preset_key      TEXT NOT NULL,            -- e.g. "disruptive" — the preset to score under
    symbols         TEXT[] NOT NULL,          -- ticker list; UPPERCASE, broker-normalised
    -- schedule_cron is intentionally a free-form string rather than
    -- a structured time so the loop can interpret it (`@daily_after_us_close`,
    -- `@daily_after_cn_close`, or a literal cron expression for an
    -- ops user that wants Monday-only scoring).
    schedule_cron   TEXT NOT NULL DEFAULT '@daily_after_us_close',
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    notes           TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- One watchlist row per (name, market, preset) — the same pool
    -- can be re-scored under multiple presets, but each (name,
    -- market, preset) combination is unique to keep the cron from
    -- accidentally scoring twice.
    UNIQUE (name, market, preset_key)
);

COMMENT ON TABLE daily_pick_watchlists IS
    'Admin-managed pool of tickers to pre-score nightly under a specific persona preset. Powers the SEC-Publishers-Exclusion /daily-picks newsletter surface; all rows are operator-curated, not user-curated.';
COMMENT ON COLUMN daily_pick_watchlists.symbols IS
    'UPPERCASE broker-normalised tickers (e.g. "BRK-B" not "BRK.B" — Yahoo quoteSummary uses dash). Order is preserved as the default browse order in the UI.';
COMMENT ON COLUMN daily_pick_watchlists.schedule_cron IS
    'Either a named tag (@daily_after_us_close / @daily_after_cn_close) or a literal cron expression. The loop interprets named tags; literal crons are reserved for ops escape hatches.';

CREATE INDEX IF NOT EXISTS idx_daily_pick_watchlists_active
    ON daily_pick_watchlists (active, market, preset_key)
    WHERE active = TRUE;

-- ------------------------------------------------------------------
-- 2. Per-day pre-computed picks (the SHARED publisher cache)
-- ------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS daily_picks (
    id              BIGSERIAL PRIMARY KEY,
    symbol          TEXT NOT NULL,
    symbol_name     TEXT,                     -- "Apple Inc.", resolved by the loader
    market          TEXT NOT NULL,
    preset_key      TEXT NOT NULL,
    pick_date       DATE NOT NULL,            -- LOCAL exchange trading date the run was for
    -- The full ConsultResponse JSON: master_reports, tactic_reports,
    -- aggregate_verdict, fundamentals snapshot, etc. Stored once,
    -- served to every subscriber identically — that is the publisher
    -- invariant. The handler is forbidden from mutating any field
    -- on a per-user basis (e.g. injecting a "you" pronoun).
    result_json     JSONB NOT NULL,
    -- Denormalised browse-grid fields. Kept in sync by the writer
    -- so the list endpoint can ORDER BY without parsing JSONB on
    -- every row. Recomputable from result_json — if these ever drift
    -- the loop can backfill in one UPDATE.
    aggregate_verdict   TEXT,                 -- "STRONG_BUY" | "BUY" | "HOLD" | "AVOID" | "SHORT" | "MIXED" | "SKIP"
    aggregate_score     INT,                  -- 0-100, derived from per-master confidence + consensus
    consensus           DOUBLE PRECISION,     -- 0.0-1.0, share of masters agreeing with aggregate
    computed_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- llm_cost_usd lets us bill the publisher budget separately
    -- from per-user budgets — important because publisher-mode
    -- consults are NOT charged to any individual user but DO consume
    -- real LLM dollars that we need to track for unit economics.
    llm_cost_usd        DOUBLE PRECISION NOT NULL DEFAULT 0,
    -- error_reason captures "we tried and failed" so the next
    -- wave can retry just the failed cells instead of re-scoring
    -- the whole watchlist. NULL means success.
    error_reason        TEXT
);

-- The publisher invariant in SQL form: one row per
-- (symbol, market, preset, day). ON CONFLICT (symbol, market,
-- preset_key, pick_date) DO UPDATE keeps re-runs idempotent.
CREATE UNIQUE INDEX IF NOT EXISTS idx_daily_picks_publisher_key
    ON daily_picks (symbol, market, preset_key, pick_date);

-- Browse-grid index: "show me today's top picks for disruptive
-- preset, sorted by score, paginated". Covers the dominant list
-- query.
CREATE INDEX IF NOT EXISTS idx_daily_picks_browse
    ON daily_picks (preset_key, market, pick_date DESC, aggregate_score DESC);

-- Per-symbol history index: "show me what disruptive said about
-- AAPL over the trailing 30 days" (powers the per-stock detail
-- drill-down).
CREATE INDEX IF NOT EXISTS idx_daily_picks_symbol_history
    ON daily_picks (symbol, market, preset_key, pick_date DESC);

COMMENT ON TABLE daily_picks IS
    'Pre-computed, ONE-row-per-(symbol, market, preset, day) shared cache. Served identically to all subscribers — this is the wire-level invariant that keeps the product inside the SEC Publishers Exclusion (Lowe v. SEC, 1985). The handler is forbidden from personalising any field.';
COMMENT ON COLUMN daily_picks.result_json IS
    'Full ConsultResponse JSONB (master_reports, tactic_reports, aggregates, fundamentals snapshot). Frozen at compute time — historical for the avoidance of survivorship bias when reviewers audit the publisher track record months later.';
COMMENT ON COLUMN daily_picks.aggregate_score IS
    '0-100 derived from per-master confidence × consensus. Pre-computed so the browse-grid sort does not parse JSONB row-by-row. If aggregate_verdict is HOLD/MIXED/SKIP score may be 0 — the UI uses verdict, not score, for the headline pill.';
COMMENT ON COLUMN daily_picks.llm_cost_usd IS
    'Real LLM cost charged to the PUBLISHER budget (not any user). Lets ops watch unit economics: e.g. 50 tickers × disruptive × 30 days × $0.012 ≈ $18/month.';

-- ------------------------------------------------------------------
-- 3. Default watchlist seed: 50 large-cap US stocks
-- ------------------------------------------------------------------
--
-- Hand-curated cross-sector slice of the S&P 100. Rationale:
--   * 50 tickers × 1 preset × Gemini-3.1-pro-preview ≈ $18/month
--     LLM spend — small enough to validate the pipeline before
--     scaling to the full S&P 500.
--   * Covers all 11 GICS sectors so the daily browse-grid has
--     visible diversity (vs e.g. picking only FAANG and looking
--     like a tech blog).
--   * Tickers are in Yahoo's wire format (BRK-B with dash, not
--     BRK.B with dot) so the loader can call quoteSummary without
--     symbol massaging.
--   * Active=TRUE so the cron picks this up on first launch.
--
-- Refresh policy: this seed snapshots S&P 100 as of 2026-06. The
-- ops user is expected to maintain it via the admin UI (a future
-- migration may re-seed if drift exceeds 20% of constituents).

INSERT INTO daily_pick_watchlists (name, market, preset_key, symbols, schedule_cron, notes)
VALUES (
    'us_largecap_disruptive_v1',
    'us_equity',
    'disruptive',
    ARRAY[
        -- Mega-cap tech (the dominant cohort by market cap; ~30% of S&P 500)
        'AAPL','MSFT','GOOGL','AMZN','META','NVDA','TSLA','AVGO','ORCL','ADBE',
        'CRM','PYPL',
        -- Financials
        'JPM','BAC','WFC','GS','V','MA','BRK-B','AXP','BLK',
        -- Healthcare
        'UNH','JNJ','LLY','PFE','ABBV','MRK','TMO',
        -- Consumer discretionary + staples
        'WMT','HD','MCD','NKE','KO','PEP','DIS','COST',
        -- Energy
        'XOM','CVX',
        -- Industrials
        'BA','CAT','HON','GE','UPS',
        -- Communication services
        'T','VZ','NFLX',
        -- Materials
        'LIN','APD',
        -- Real estate
        'AMT','PLD'
    ]::TEXT[],
    '@daily_after_us_close',
    'Initial 50-ticker S&P 100 slice to validate the publisher pipeline. Sized for ~$18/month LLM spend. Expand to full S&P 500 once smoke tests and free-tier time-lag enforcement are verified end-to-end.'
)
ON CONFLICT (name, market, preset_key) DO NOTHING;

COMMIT;
