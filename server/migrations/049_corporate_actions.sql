-- 049_corporate_actions.sql
--
-- Sprint 4 / corp-action: stock split + cash dividend ingestion and
-- application registry.
--
-- Background: A-share funds (e.g. 688195 腾景科技 2026-05-29 10送4 +
-- 0.164/股 派现) and US large-caps (e.g. NVDA 2024-06 10:1) routinely
-- emit corporate actions whose ex-date causes the upstream quote to
-- drop while shares-outstanding multiplies. Without an explicit
-- adjustment, holding_positions.cost_price stays at the pre-split
-- value while current_price comes back post-split, producing a
-- phantom unrealized_pnl loss of (1 - 1/ratio) * notional that has
-- nothing to do with trading.
--
-- The schema models two layers:
--
--   1. corporate_actions   — one row per (instrument, ex_date, type).
--                            Source-of-truth event ledger; same row
--                            is shared across every fund that holds
--                            the instrument.
--
--   2. corp_action_applications — one row per (action, fund) recording
--                            the actual numeric mutation that landed
--                            on holding_positions / position_lots.
--                            The compound PK enforces idempotency:
--                            re-running the applier with the same
--                            action_id / fund_id is a no-op.
--
-- Splits are stored as a single decimal `split_ratio` =
-- new_shares / old_shares (1.4 for 10送4, 2.0 for 1拆2, 0.1 for a
-- 10:1 reverse split). Cash dividends are gross per old share,
-- irrespective of withholding.

CREATE TABLE IF NOT EXISTS corporate_actions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),

    -- "EXCHANGE:SYMBOL" — must match holding_positions.instrument_key
    -- exactly. The applier joins on this column literally.
    instrument_key  VARCHAR(64) NOT NULL,

    -- Effective date of the action. For A-share rights issues this is
    -- 除权除息日; for US splits it is the trading-effective date.
    ex_date         DATE NOT NULL,

    action_type     VARCHAR(32) NOT NULL
        CHECK (action_type IN ('split', 'cash_dividend', 'stock_dividend', 'combined')),

    -- new_shares / old_shares. Always > 0. 1.0 is "no share change",
    -- meaningful for a pure cash_dividend row whose split_ratio == 1.
    split_ratio     NUMERIC(20, 8) NOT NULL DEFAULT 1.0
        CHECK (split_ratio > 0),

    -- Gross dividend per OLD share, before any withholding tax. 0.0
    -- when the action carries no cash payout.
    cash_dividend   NUMERIC(20, 8) NOT NULL DEFAULT 0.0
        CHECK (cash_dividend >= 0),

    -- Provenance label so we can re-ingest from the same source
    -- idempotently and audit which path produced which row.
    source          VARCHAR(32) NOT NULL
        CHECK (source IN ('manual', 'yahoo', 'tushare', 'sina', 'tencent')),

    notes           TEXT,
    announced_at    TIMESTAMPTZ,
    recorded_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Re-ingesting from the same source for the same event is a no-op:
-- the (instrument_key, ex_date, action_type, source) tuple is unique.
-- Two different sources may report the same event — that's allowed
-- and the applier picks the highest-confidence one.
CREATE UNIQUE INDEX IF NOT EXISTS corporate_actions_dedup
    ON corporate_actions (instrument_key, ex_date, action_type, source);

-- Hot query: "give me everything that happened to instruments my
-- funds hold, since timestamp X" — used by the daily applier sweep.
CREATE INDEX IF NOT EXISTS corporate_actions_instrument_date_idx
    ON corporate_actions (instrument_key, ex_date DESC);


CREATE TABLE IF NOT EXISTS corp_action_applications (
    -- Compound PK guarantees a single application per (event, fund).
    -- Rerunning the applier with the same args is a fast PK-violation
    -- ON CONFLICT DO NOTHING and the math doesn't double-fire.
    corp_action_id  UUID NOT NULL REFERENCES corporate_actions(id) ON DELETE CASCADE,
    fund_id         UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,

    applied_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Pre/post snapshot of holding_positions for the affected
    -- (fund, instrument). Stored verbatim for audit + rollback —
    -- not derived elsewhere because position_quote_refresher will
    -- have moved current_price by the time anyone audits this row.
    pre_quantity    NUMERIC(20, 8) NOT NULL,
    post_quantity   NUMERIC(20, 8) NOT NULL,
    pre_cost_price  NUMERIC(20, 8) NOT NULL,
    post_cost_price NUMERIC(20, 8) NOT NULL,

    -- Total cash credited to the fund as a result of this action.
    -- Currently informational (we don't post a cash leg yet — the
    -- fund has no formal cash_balances model), but recording it
    -- avoids losing the audit trail when that model lands.
    cash_credit     NUMERIC(20, 8) NOT NULL DEFAULT 0.0,

    PRIMARY KEY (corp_action_id, fund_id)
);

-- "What did fund X get applied to it?" — used by the read endpoint
-- that powers the holding detail page's corp-action timeline.
CREATE INDEX IF NOT EXISTS corp_action_applications_fund_idx
    ON corp_action_applications (fund_id, applied_at DESC);
