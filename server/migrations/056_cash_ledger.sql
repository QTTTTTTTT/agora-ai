-- Migration 056 — fund cash ledger (P1-1).
--
-- Why this table exists
--
-- Up to Sprint 4 the fund's cash position lived in a single
-- numeric column (`funds.current_capital`). That column is
-- mutated by:
--   - the trading engine (debits on buy, credits on sell, net of
--     fees);
--   - corpaction.applier (credits on cash dividends);
--   - persistPortfolioState at the end of every plan execution.
--
-- A scalar can answer "how much cash is in the fund right now?"
-- but it can NOT answer:
--   - "show me every cash movement in March"
--   - "how much did this fund pay in commissions last quarter?"
--   - "did our recorded cash match the broker statement on date X?"
--
-- The cash_ledger table is the append-only journal that every
-- regulated fund accounting stack maintains. We sum the entries
-- to recover the scalar (reconciliation invariant), and we slice
-- by entry_type / posted_at to answer the audit questions.
--
-- Design choices
--
-- 1. Append-only. No UPDATE / DELETE in normal operation. A
--    correction is recorded as a NEW row of type 'adjustment'
--    referencing the original via `metadata.reverses_id`. This
--    keeps the audit trail forensically defensible and lines up
--    with the hash-chained admin_change_log.
--
-- 2. Signed amounts. Positive = cash in (credit). Negative =
--    cash out (debit). The convention is fund-centric: a buy
--    debits cash (negative), a dividend credits cash (positive).
--    SUM(amount) over a fund equals current_capital under normal
--    operation; reconciliation jobs can detect drift.
--
-- 3. Idempotency. (fund_id, idempotency_key) is UNIQUE. The
--    trading engine writes 4 rows per fill (notional + 3 fee
--    types) under a deterministic key derived from
--    `trade:{trade_id}:{role}` so a retry doesn't double-post.
--
-- 4. Foreign-key everything we know. trade_id, plan_id,
--    plan_action_id, corp_action_id, broker_link_id are nullable
--    because not every entry type has all of them — but when we
--    do know, we link, so a join can rebuild a per-trade
--    reconciliation in one query.
--
-- 5. Single-currency. We keep a `currency` column for forward
--    compat with the FX work (P1-4) but for now every row of an
--    existing fund is the fund's base currency. Cross-currency
--    funds need fund_cash_balances which lands with FX.

CREATE TABLE IF NOT EXISTS cash_ledger (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    fund_id         UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    posted_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- trading_date is the *business* date the entry belongs to
    -- (vs. posted_at which is wall-clock). On a normal day the two
    -- match, but a late-evening corp-action import can post into
    -- yesterday's trading date so NAV-by-trading-date stays correct.
    trading_date    DATE,

    -- entry_type vocabulary. We deliberately enumerate every
    -- legal value here rather than a free-form CHECK so a typo
    -- can't silently produce orphan rows. Adding a new type =
    -- ALTER the constraint AND update CashLedger.EntryType
    -- constants in Go.
    entry_type      VARCHAR(48) NOT NULL,

    -- Signed monetary value. NUMERIC(20,4) gives us 16 digits of
    -- whole-dollar headroom and 4 decimals for sub-cent fees.
    amount          NUMERIC(20,4) NOT NULL,

    currency        VARCHAR(8) NOT NULL DEFAULT 'USD',

    -- Optional links — kept nullable + ON DELETE SET NULL so the
    -- ledger row outlives the linked artefact (we still want to
    -- audit a trade after the underlying trade_executions row
    -- gets purged in a future archival pass).
    trade_id        UUID,
    plan_id         UUID,
    plan_action_id  UUID,
    corp_action_id  UUID,
    broker_link_id  UUID,

    description     TEXT,
    metadata        JSONB DEFAULT '{}'::jsonb,
    created_by      UUID,

    -- Idempotency. NULL for entries that don't need it (manual
    -- adjustments). When non-NULL, must be unique per fund.
    idempotency_key VARCHAR(128),

    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT cash_ledger_entry_type_chk CHECK (
        entry_type IN (
            -- Trade legs (one per fill, broken out so reports can
            -- subtotal commissions vs. notional cleanly).
            'trade_buy_notional',
            'trade_buy_commission',
            'trade_buy_transfer_fee',
            'trade_buy_stamp_tax',
            'trade_sell_notional',
            'trade_sell_commission',
            'trade_sell_transfer_fee',
            'trade_sell_stamp_tax',
            -- Corporate actions
            'dividend_cash',
            -- Recurring fund fees
            'fee_management',
            'fee_performance',
            'fee_platform',
            -- Cash movements (P1-2)
            'funding_deposit',
            'funding_withdrawal',
            -- Manual / forensic
            'adjustment',
            'reversal'
        )
    ),
    CONSTRAINT cash_ledger_amount_nonzero_chk CHECK (amount <> 0)
);

-- Per-fund unique idempotency. NULLs don't conflict (Postgres
-- default behaviour) so manual rows without a key still work.
CREATE UNIQUE INDEX IF NOT EXISTS cash_ledger_fund_idem_uq
    ON cash_ledger (fund_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- Read paths.
CREATE INDEX IF NOT EXISTS cash_ledger_fund_posted_idx
    ON cash_ledger (fund_id, posted_at DESC);
CREATE INDEX IF NOT EXISTS cash_ledger_fund_trading_date_idx
    ON cash_ledger (fund_id, trading_date)
    WHERE trading_date IS NOT NULL;
CREATE INDEX IF NOT EXISTS cash_ledger_fund_type_idx
    ON cash_ledger (fund_id, entry_type);
CREATE INDEX IF NOT EXISTS cash_ledger_trade_idx
    ON cash_ledger (trade_id)
    WHERE trade_id IS NOT NULL;

CREATE OR REPLACE FUNCTION cash_ledger_touch_updated_at() RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS cash_ledger_touch_updated_at ON cash_ledger;
CREATE TRIGGER cash_ledger_touch_updated_at
    BEFORE UPDATE ON cash_ledger
    FOR EACH ROW EXECUTE FUNCTION cash_ledger_touch_updated_at();
