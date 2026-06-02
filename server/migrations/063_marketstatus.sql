-- Migration 063 — market-status gate (S6.1).
--
-- What this stores
--
-- Three small tables together model the "the market is or isn't
-- willing to take this trade right now" gate that the simulator
-- needs to be a credible production-rehearsal:
--
--   * `instrument_market_status` — per-instrument live state:
--     trading | halted | suspended, with halt reason / reopen
--     window, daily price-limit floor / ceiling, and a quote
--     freshness timestamp.
--   * `trading_calendar` — date-keyed open/close per market,
--     with a `half_day` flag for early close (HK / US holiday
--     eves, A-share post-typhoon, etc.).
--   * `marketstatus_events` — append-only audit of every order
--     decision the gate emitted (allow / reject / warn) so we
--     can reconstruct "why was this trade rejected at 14:32".
--
-- Why three tables instead of one
--
-- The three live in different cadences: instrument status moves
-- intra-day (operator manually halts, exchange announces a
-- circuit-breaker), the calendar is loaded once per quarter
-- from the exchange's public schedule, and the event log is the
-- effect rather than the cause. Mixing them as JSON inside one
-- "policy" table would conflate three different write rates.

CREATE TABLE IF NOT EXISTS instrument_market_status (
    instrument_key      VARCHAR(64) PRIMARY KEY,
    -- canonical symbol kept alongside instrument_key so the admin
    -- list view doesn't need to JOIN with quote_metadata for a
    -- human-readable label.
    symbol              VARCHAR(64) NOT NULL,
    market              VARCHAR(16) NOT NULL,
    -- Closed vocabulary: 'trading' | 'halted' | 'suspended'.
    -- 'suspended' = long-term (regulatory, delisting); 'halted'
    -- = short-term (news pending, volatility halt).
    status              VARCHAR(16) NOT NULL DEFAULT 'trading'
                          CHECK (status IN ('trading', 'halted', 'suspended')),
    halt_reason         TEXT,
    -- Reopen window. Empty = until further notice. The gate
    -- treats halt_until <= now() as effectively reopened so we
    -- don't have to remember to flip the row back.
    halt_started_at     TIMESTAMPTZ,
    halt_until          TIMESTAMPTZ,
    -- Daily price limits in absolute price (not percent) so
    -- A-share style (close-anchored) and US-style (handle vs
    -- LULD bands) can coexist. NULL = no limit. Operator updates
    -- ahead of session via the calendar uploader; the engine
    -- consults these alongside the live quote.
    lower_limit         NUMERIC(20, 6),
    upper_limit         NUMERIC(20, 6),
    -- last_quote_at is the freshness anchor. The engine compares
    -- against now() to compute "stale" and reject or warn. Kept
    -- here (denormalised from quote_metadata) so the gate can
    -- read in one row instead of joining.
    last_quote_at       TIMESTAMPTZ,
    last_quote_price    NUMERIC(20, 6),
    -- Optional asset-class hint so the gate's default freshness
    -- threshold (60s equity, 5s futures, 300s OTC) matches.
    asset_class         VARCHAR(24) NOT NULL DEFAULT 'equity',
    -- Operator-overridable freshness budget in seconds; NULL
    -- means use the asset-class default.
    staleness_budget_seconds INT
                          CHECK (staleness_budget_seconds IS NULL OR staleness_budget_seconds BETWEEN 1 AND 3600),
    -- Free-form note shown in admin UI.
    note                TEXT,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by          UUID
);

CREATE INDEX IF NOT EXISTS instrument_market_status_market_idx
    ON instrument_market_status (market, status);
CREATE INDEX IF NOT EXISTS instrument_market_status_symbol_idx
    ON instrument_market_status (symbol);

-- ----------------------------------------------------------------
-- trading_calendar
-- ----------------------------------------------------------------

CREATE TABLE IF NOT EXISTS trading_calendar (
    market              VARCHAR(16) NOT NULL,
    trading_date        DATE NOT NULL,
    -- false = closed (weekend / holiday). When false, the rest
    -- of the row is informational only.
    is_open             BOOLEAN NOT NULL DEFAULT TRUE,
    -- Local-timezone open/close as HH:MM:SS strings. The gate
    -- combines (date, time, market_tz) to compare against now().
    -- Strings rather than TIME-typed because we ALSO want to
    -- carry "23:59" (futures night session) without DST games.
    open_local          VARCHAR(8) NOT NULL DEFAULT '09:30:00',
    close_local         VARCHAR(8) NOT NULL DEFAULT '15:00:00',
    -- IANA timezone (e.g. 'Asia/Shanghai', 'America/New_York').
    -- We store per-row rather than per-market so the rare DST
    -- edge case (US uses different open in EST vs EDT) collapses
    -- to "the calendar already has the right offset for this
    -- date".
    market_tz           VARCHAR(64) NOT NULL DEFAULT 'Asia/Shanghai',
    -- half_day = early close (e.g. US Black Friday 13:00 ET,
    -- HK Christmas Eve 12:00 HKT). The gate enforces close_local
    -- regardless; this flag exists so the UI can highlight the
    -- shorter session and the PM agent can be told.
    half_day            BOOLEAN NOT NULL DEFAULT FALSE,
    note                TEXT,
    PRIMARY KEY (market, trading_date)
);

CREATE INDEX IF NOT EXISTS trading_calendar_open_idx
    ON trading_calendar (market, trading_date)
    WHERE is_open = TRUE;

-- ----------------------------------------------------------------
-- marketstatus_events
-- ----------------------------------------------------------------
--
-- Append-only audit. Every rejected order or stale-quote warning
-- emitted by the gate lands here. We do NOT log every "allow" —
-- that would balloon. Operators can replay rejections to verify
-- the gate is honouring policy.

CREATE TABLE IF NOT EXISTS marketstatus_events (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    fund_id         UUID,
    instrument_key  VARCHAR(64) NOT NULL,
    symbol          VARCHAR(64),
    -- 'reject' or 'warn'. Reject = order denied; warn = order
    -- accepted but flagged (today: only stale-quote warnings).
    decision        VARCHAR(16) NOT NULL
                      CHECK (decision IN ('reject', 'warn')),
    -- Closed vocabulary for the rule that fired. Mirrors
    -- marketstatus.RuleCode.
    rule_code       VARCHAR(40) NOT NULL,
    summary         TEXT,
    -- Compact diagnostic bundle: limit lower/upper, intended
    -- price, last_quote_at, halt_reason — whatever the rule had
    -- handy.
    metadata        JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- Reference back to the order request so an auditor can
    -- jump to the broker order. Nullable for stale-quote
    -- warnings emitted before an order existed.
    client_order_id VARCHAR(64),
    detected_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS marketstatus_events_instr_idx
    ON marketstatus_events (instrument_key, detected_at DESC);
CREATE INDEX IF NOT EXISTS marketstatus_events_fund_idx
    ON marketstatus_events (fund_id, detected_at DESC);
CREATE INDEX IF NOT EXISTS marketstatus_events_rule_idx
    ON marketstatus_events (rule_code, detected_at DESC);
