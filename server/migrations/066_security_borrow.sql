-- Migration 066 — Securities borrow / locate fee store (S6.4).
--
-- What this stores
--
--   1. security_borrow_rates  — calibration table. One row per
--      borrowable instrument keyed on instrument_key. Carries
--      the annual borrow rate (bps), an optional one-time
--      locate fee (bps of notional), the availability tier
--      (easy / hard / restricted / unavailable), and the
--      shares offered for borrow today.
--
--   2. security_locate_events — append-only audit of every
--      pre-trade locate decision. Read by the admin "Locate
--      audit" panel and by the integration tests asserting
--      the gate behaved as expected.
--
--   3. short_position_borrow_ledger — per-fund, per-instrument,
--      per-day record of the borrow fee that was charged.
--      Idempotent on (fund_id, instrument_key, accrual_date)
--      so the daily loop can safely retry without
--      double-billing. Cross-references the cash_ledger entry.
--
-- Why per-day ledger instead of "running total on the position"
--
--   - We need a forensic trail: "why did this fund's cash
--     decrease by $1240 on Tuesday → because it carried 50k
--     short shares of HARD_TO_BORROW_INC at 30%/yr".
--   - The fee computation depends on closing price, which
--     varies daily. A running total cannot reconstruct each
--     day's marker price.
--   - Idempotency on (fund, instrument, date) means the loop
--     can be re-run without producing duplicate charges.
--
-- Day count basis: 365 (Actual/365 Fixed). Equity borrow markets
-- are mixed (some 360, some 365); we pick 365 so a fund's NAV
-- numbers are honest about the financing drag in calendar terms.

-- ----- 1. Calibration table -----
CREATE TABLE IF NOT EXISTS security_borrow_rates (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    instrument_key         VARCHAR(64) NOT NULL UNIQUE,
    symbol                 VARCHAR(64) NOT NULL,
    market                 VARCHAR(16) NOT NULL DEFAULT 'US',
    asset_class            VARCHAR(16) NOT NULL DEFAULT 'equity',
    -- Annualised borrow rate in basis points (10000 bps = 100%).
    -- 50 = 0.50%/yr (ETB); 3000 = 30%/yr (HTB); 0 allowed for
    -- e.g. self-borrowable inventory.
    borrow_rate_bps_annual NUMERIC(10, 2) NOT NULL DEFAULT 0
                             CHECK (borrow_rate_bps_annual >= 0),
    -- Optional one-time locate fee charged at order entry, in
    -- bps of notional. Most US prime-broker quotes use 0 for ETB
    -- and 5-50 bps for HTB.
    locate_fee_bps         NUMERIC(10, 2) NOT NULL DEFAULT 0
                             CHECK (locate_fee_bps >= 0),
    -- Closed enum for analytics.
    availability           VARCHAR(16) NOT NULL DEFAULT 'easy'
                             CHECK (availability IN (
                                 'easy', 'hard', 'restricted', 'unavailable'
                             )),
    -- How many shares are borrowable today. NULL = unbounded
    -- (only allowed when availability='easy'); 0 = effectively
    -- unavailable regardless of the availability tier label.
    available_shares       BIGINT,
    -- Optional caps on the locate request size. Most prime
    -- brokers reject very small (< 100) or very large (> 1M)
    -- locates depending on liquidity.
    min_locate_qty         BIGINT,
    max_locate_qty         BIGINT,
    -- Calibration provenance.
    source                 VARCHAR(32) NOT NULL DEFAULT 'manual'
                             CHECK (source IN (
                                 'manual', 'broker_quote', 'agent_lender',
                                 'historical_calibration', 'public_feed'
                             )),
    last_calibrated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    note                   TEXT,
    updated_by             UUID,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS security_borrow_rates_avail_idx
    ON security_borrow_rates (availability)
    WHERE availability != 'easy';

-- ----- 2. Locate audit log -----
CREATE TABLE IF NOT EXISTS security_locate_events (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    fund_id             UUID NOT NULL,
    instrument_key      VARCHAR(64) NOT NULL,
    symbol              VARCHAR(64) NOT NULL,
    requested_qty       NUMERIC(20, 4) NOT NULL,
    -- Closed enum: how the locate engine resolved the request.
    decision            VARCHAR(24) NOT NULL
                          CHECK (decision IN (
                              'allow',
                              'reject_unavailable',
                              'reject_insufficient',
                              'reject_below_min',
                              'reject_above_max',
                              'no_calibration',
                              'fail_open'
                          )),
    rate_bps_annual     NUMERIC(10, 2),
    locate_fee_bps      NUMERIC(10, 2),
    locate_fee_amount   NUMERIC(20, 4),
    intended_price      NUMERIC(20, 4),
    notional            NUMERIC(20, 4),
    reason              TEXT,
    client_order_id     VARCHAR(128),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS security_locate_events_fund_idx
    ON security_locate_events (fund_id, created_at DESC);

CREATE INDEX IF NOT EXISTS security_locate_events_instrument_idx
    ON security_locate_events (instrument_key, created_at DESC);

-- ----- 3. Daily borrow-fee ledger -----
CREATE TABLE IF NOT EXISTS short_position_borrow_ledger (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    fund_id              UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    instrument_key       VARCHAR(64) NOT NULL,
    symbol               VARCHAR(64) NOT NULL,
    accrual_date         DATE NOT NULL,
    short_qty            NUMERIC(20, 4) NOT NULL CHECK (short_qty > 0),
    market_price         NUMERIC(20, 4) NOT NULL,
    notional             NUMERIC(20, 4) NOT NULL,
    rate_bps_annual      NUMERIC(10, 2) NOT NULL,
    day_count_basis      INT NOT NULL DEFAULT 365 CHECK (day_count_basis IN (360, 365)),
    fee_amount           NUMERIC(20, 4) NOT NULL CHECK (fee_amount >= 0),
    -- Reference to the cash_ledger row that booked the debit.
    -- Nullable because the loop can run before cash_ledger is
    -- wired (test environment) or in dry-run mode.
    cash_ledger_entry_id UUID,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (fund_id, instrument_key, accrual_date)
);

CREATE INDEX IF NOT EXISTS short_position_borrow_ledger_fund_idx
    ON short_position_borrow_ledger (fund_id, accrual_date DESC);

-- ----- 4. Extend cash_ledger entry_type enum -----
--
-- Two new cash_ledger entry types so borrow fees and locate
-- fees are first-class line items (not "adjustment" rows). The
-- daily accrual loop books `borrow_fee`; the pre-trade locate
-- adapter books `locate_fee` when the gate's verdict carries
-- a positive LocateFee. Both are negative amounts (cash out).
-- The cash ledger table is named `cash_ledger` (singular), see
-- migration 056. Earlier drafts of this migration referenced
-- cash_ledger_entries which is wrong; the original deploy never
-- landed because of this typo, so the bare singular name is
-- safe and matches every code path in
-- server/internal/repository/cash_ledger_repo.go.
ALTER TABLE cash_ledger DROP CONSTRAINT IF EXISTS cash_ledger_entry_type_chk;
ALTER TABLE cash_ledger ADD CONSTRAINT cash_ledger_entry_type_chk CHECK (
    entry_type IN (
        'trade_buy_notional',
        'trade_buy_commission',
        'trade_buy_transfer_fee',
        'trade_buy_stamp_tax',
        'trade_sell_notional',
        'trade_sell_commission',
        'trade_sell_transfer_fee',
        'trade_sell_stamp_tax',
        'dividend_cash',
        'fee_management',
        'fee_performance',
        'fee_platform',
        'funding_deposit',
        'funding_withdrawal',
        'adjustment',
        'reversal',
        -- S6.4: short-borrow line items.
        'borrow_fee',
        'locate_fee'
    )
);
