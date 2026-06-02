-- Migration 058 — fx_rates (P1-4).
--
-- Why this table exists
--
-- Until P1-3 the platform implicitly treated everything as USD.
-- That works for a US-only fund but breaks the moment a fund:
--   - holds an HKEX or ASHARE position (price in HKD or CNY)
--   - takes a deposit denominated in EUR
--   - reports NAV to a CNY-resident LP
--
-- This table is the SOURCE OF TRUTH for the FX rates the NAV
-- aggregator uses. Every row is one observed rate at a moment
-- in time:
--
--   base_currency = "USD" rate to quote_currency = "CNY" of 7.18
--   means: 1 USD = 7.18 CNY.
--
-- Cardinality is bounded — we only support a small set of
-- currencies (USD/CNY/HKD/EUR/JPY/GBP/SGD) and one observation
-- per pair per day. Intra-day variants are written as additional
-- rows with finer-grained `rate_at`.
--
-- Layout decisions
--
--   - We DON'T store cross-rates (CNY/HKD). Compute on read by
--     pivoting through USD (always the anchor). This halves the
--     write volume and avoids triangular-arbitrage drift between
--     stored cells.
--
--   - source captures provider attribution: 'yahoo', 'manual',
--     'eod' (a future scheduled End-of-Day batch). 'manual' rows
--     win over auto-fetched rows when both exist for the same
--     pair-date — operators may correct a bad fetch.
--
--   - "rate_at" is when the rate WAS OBSERVED, not when we wrote
--     the row. NAV computations look up "the latest rate where
--     rate_at <= snapshot_at" so back-filling missed days works.
--
--   - The unique index is partial on (base, quote, rate_at,
--     source). Without source the table couldn't carry both
--     yahoo + manual snapshots for the same day; with it, the
--     read path picks `manual` first, then `yahoo` as fallback.

CREATE TABLE IF NOT EXISTS fx_rates (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    base_currency   VARCHAR(8) NOT NULL,
    quote_currency  VARCHAR(8) NOT NULL,
    rate            NUMERIC(20,8) NOT NULL,
    rate_at         TIMESTAMPTZ NOT NULL,
    source          VARCHAR(24) NOT NULL DEFAULT 'manual',
    metadata        JSONB DEFAULT '{}'::jsonb,
    created_by      UUID,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT fx_rates_rate_pos_chk CHECK (rate > 0),
    CONSTRAINT fx_rates_pair_distinct_chk
        CHECK (base_currency <> quote_currency),
    CONSTRAINT fx_rates_source_chk
        CHECK (source IN ('yahoo', 'manual', 'eod', 'override'))
);

-- One observation per pair / instant / source.
CREATE UNIQUE INDEX IF NOT EXISTS fx_rates_pair_at_source_uq
    ON fx_rates (base_currency, quote_currency, rate_at, source);

-- Latest-rate lookup: "give me the last manual or yahoo rate for
-- USD/CNY at or before T".
CREATE INDEX IF NOT EXISTS fx_rates_pair_rate_at_idx
    ON fx_rates (base_currency, quote_currency, rate_at DESC);

-- Range scans for the FX admin page.
CREATE INDEX IF NOT EXISTS fx_rates_rate_at_idx
    ON fx_rates (rate_at DESC);
