-- 038_lot_ledger.sql
--
-- Phase 3A-1: introduce the lot ledger that powers strategy
-- attribution and the closed-loop learning system.
--
-- The legacy schema only tracked positions in aggregate via
-- holding_positions (one row per fund + instrument with the latest
-- average cost and remaining quantity). That representation is
-- sufficient for portfolio accounting but it loses every piece of
-- information the strategy review committee actually needs:
--
--   - "How long did we hold each buy lot before closing it?"
--   - "What was the max favourable / adverse excursion while the
--      position was alive?"
--   - "Which sleeve (LLM PM, Donchian breakout, mean-reversion, ...)
--      generated this trade idea, and what was the regime at entry?"
--   - "What was the realised P&L for this specific roundtrip after
--      fees, separate from any other lots of the same symbol?"
--
-- We answer all of the above by maintaining a FIFO ledger of open
-- buy lots (`position_lots`) and recording one row per close event
-- in `closed_lots`. The closing flow is FIFO by default; partial
-- sells split one lot into a `closed_lots` row plus a still-open
-- `position_lots` row with reduced quantity_remaining.
--
-- Three small additions to `plan_actions` thread the attribution
-- metadata from decision time → trade execution → lot ledger so
-- that the eventual performance attribution agent can group
-- realised P&L by (sleeve, regime, signal_source) without rebuilding
-- it from scratch.

-- ---------------------------------------------------------------------------
-- plan_actions: attribution metadata captured at decision time.
-- Phase 3A-3 will populate regime_tag from the indicator snapshot;
-- Phase 3A-4 will populate sleeve / signal_source when classical
-- strategy engines join the LLM PM at the decision table. The
-- columns are nullable so legacy rows + LLM-only plans simply leave
-- them empty without breaking the schema.
-- ---------------------------------------------------------------------------
ALTER TABLE plan_actions
    ADD COLUMN IF NOT EXISTS sleeve        VARCHAR(32),
    ADD COLUMN IF NOT EXISTS regime_tag    VARCHAR(16),
    ADD COLUMN IF NOT EXISTS signal_source VARCHAR(32),
    ADD COLUMN IF NOT EXISTS exit_reason   VARCHAR(32);

COMMENT ON COLUMN plan_actions.sleeve IS
    'Strategy sleeve that originated the action (e.g. llm_pm, trend, mean_reversion, momentum, manual).';
COMMENT ON COLUMN plan_actions.regime_tag IS
    'Market regime tag at decision time (trend_up | trend_down | range | chop).';
COMMENT ON COLUMN plan_actions.signal_source IS
    'Concrete signal source within the sleeve (e.g. donchian_20, dual_ma_50_200, llm_pm).';
COMMENT ON COLUMN plan_actions.exit_reason IS
    'For sell/reduce actions: why the exit was generated (manual, stop_loss, take_profit, trailing, time_stop, rebalance, llm_decision).';

-- ---------------------------------------------------------------------------
-- position_lots: one row per still-open buy lot. Quantity_remaining
-- decreases each time a sell consumes (FIFO) part of it; when it
-- reaches zero the row is marked status='closed' but kept for audit.
-- ---------------------------------------------------------------------------
CREATE TABLE position_lots (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    fund_id             UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    instrument_key      VARCHAR(128) NOT NULL,
    symbol              VARCHAR(32)  NOT NULL,
    market              VARCHAR(32),
    asset_class         VARCHAR(32),

    -- Originating trade. The execution that opened this lot must
    -- have side='buy' and status='filled'. plan_action_id is set
    -- nullable on delete so an audit purge of plan_actions doesn't
    -- cascade through the lot ledger.
    opening_trade_id    UUID NOT NULL REFERENCES trade_executions(id) ON DELETE RESTRICT,
    opening_plan_action_id UUID REFERENCES plan_actions(id) ON DELETE SET NULL,

    -- Lot opening snapshot. quantity_opened never changes;
    -- quantity_remaining is decremented as sells consume it.
    opened_at           TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    entry_price         NUMERIC(16, 4) NOT NULL,
    entry_fees          NUMERIC(16, 4) NOT NULL DEFAULT 0,
    quantity_opened     NUMERIC(16, 4) NOT NULL,
    quantity_remaining  NUMERIC(16, 4) NOT NULL,

    -- Attribution metadata copied from the originating plan_action
    -- so the lot is self-contained for attribution queries (avoids
    -- a join in hot paths).
    sleeve              VARCHAR(32),
    regime_at_entry     VARCHAR(16),
    signal_source       VARCHAR(32),
    confidence_at_entry NUMERIC(5, 4),

    -- Excursion tracking. Updated by the price refresher each time
    -- a fresh quote lands. last_price_at lets the refresher skip
    -- redundant updates when the quote hasn't actually moved.
    highest_price_seen  NUMERIC(16, 4),
    lowest_price_seen   NUMERIC(16, 4),
    last_price          NUMERIC(16, 4),
    last_price_at       TIMESTAMPTZ,

    status              VARCHAR(16) NOT NULL DEFAULT 'open'
                            CHECK (status IN ('open','partial','closed')),
    closed_at           TIMESTAMPTZ,

    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CHECK (quantity_opened > 0),
    CHECK (quantity_remaining >= 0),
    CHECK (quantity_remaining <= quantity_opened),
    CHECK ((status = 'closed') = (quantity_remaining = 0))
);

COMMENT ON TABLE position_lots IS
    'FIFO ledger of open buy lots. Each row is one buy execution; partial closes decrement quantity_remaining, full closes flip status to ''closed''.';

-- Hot path: ListOpenByInstrument(fund_id, instrument_key) ordered by
-- opened_at ASC for FIFO closing. Partial index excludes 'closed'
-- rows so the working set stays small as history accumulates.
CREATE INDEX idx_position_lots_open_fifo
    ON position_lots(fund_id, instrument_key, opened_at)
    WHERE status != 'closed';

-- Audit / replay path: scan all lots for a fund in any state.
CREATE INDEX idx_position_lots_fund
    ON position_lots(fund_id, opened_at DESC);

-- ---------------------------------------------------------------------------
-- closed_lots: one row per realised roundtrip (or partial close).
-- This is the table the performance attribution agent and sleeve
-- scorecard query against. Denormalised on purpose: every column
-- the attribution needs is present so a single index scan can drive
-- a sleeve / regime / symbol breakdown.
-- ---------------------------------------------------------------------------
CREATE TABLE closed_lots (
    id                      UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    fund_id                 UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    position_lot_id         UUID NOT NULL REFERENCES position_lots(id) ON DELETE CASCADE,
    instrument_key          VARCHAR(128) NOT NULL,
    symbol                  VARCHAR(32)  NOT NULL,
    market                  VARCHAR(32),
    asset_class             VARCHAR(32),

    -- Closing trade.
    closing_trade_id        UUID NOT NULL REFERENCES trade_executions(id) ON DELETE RESTRICT,
    closing_plan_action_id  UUID REFERENCES plan_actions(id) ON DELETE SET NULL,

    -- Time series. holding_days is denormalised so attribution can
    -- bucket by holding period without recomputing on every read.
    opened_at               TIMESTAMPTZ    NOT NULL,
    closed_at               TIMESTAMPTZ    NOT NULL,
    holding_days            NUMERIC(10, 4) NOT NULL,

    -- Roundtrip cashflows. quantity_closed * (exit_price - entry_price)
    -- minus the portion of the entry/exit fees that applies to this
    -- close; the service that emits this row does the arithmetic and
    -- writes the result here so attribution queries are pure reads.
    quantity_closed         NUMERIC(16, 4) NOT NULL,
    entry_price             NUMERIC(16, 4) NOT NULL,
    exit_price              NUMERIC(16, 4) NOT NULL,
    entry_fees              NUMERIC(16, 4) NOT NULL DEFAULT 0,
    exit_fees               NUMERIC(16, 4) NOT NULL DEFAULT 0,
    realized_pnl            NUMERIC(20, 4) NOT NULL,
    realized_pnl_pct        NUMERIC(12, 6) NOT NULL,

    -- Excursion: max favourable / adverse move while the lot was
    -- open, as a fraction of entry_price (positive = favourable for
    -- a long, negative = adverse). Captured from the
    -- highest_price_seen / lowest_price_seen on the lot at close.
    -- Stored fractional (0.0832 = 8.32%) so it's comparable across
    -- symbols and price scales.
    max_favorable_excursion NUMERIC(12, 6),
    max_adverse_excursion   NUMERIC(12, 6),

    -- Attribution metadata. Carried from position_lots for entry-side
    -- fields; closing-side fields (regime_at_exit, exit_reason) come
    -- from the closing plan_action / exit manager.
    sleeve                  VARCHAR(32),
    regime_at_entry         VARCHAR(16),
    regime_at_exit          VARCHAR(16),
    signal_source           VARCHAR(32),
    confidence_at_entry     NUMERIC(5, 4),
    exit_reason             VARCHAR(32),

    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CHECK (quantity_closed > 0),
    CHECK (closed_at >= opened_at)
);

COMMENT ON TABLE closed_lots IS
    'Realised roundtrips. One row per (partial or full) close event. The substrate for sleeve attribution, lesson generation, and decay monitoring.';

-- Sleeve scorecard: "rolling P&L by sleeve in the last N days".
CREATE INDEX idx_closed_lots_sleeve_window
    ON closed_lots(fund_id, sleeve, closed_at DESC);

-- Regime scorecard: "how well did each regime tag predict outcomes?".
CREATE INDEX idx_closed_lots_regime_window
    ON closed_lots(fund_id, regime_at_entry, closed_at DESC);

-- Per-symbol attribution + UI "trade history".
CREATE INDEX idx_closed_lots_symbol_window
    ON closed_lots(fund_id, instrument_key, closed_at DESC);

-- Generic fund-wide window for "last 30 days realised P&L" queries.
CREATE INDEX idx_closed_lots_fund_window
    ON closed_lots(fund_id, closed_at DESC);

-- Exit reason analytics: "how often does the trailing stop fire vs
-- the LLM-driven exit?".
CREATE INDEX idx_closed_lots_exit_reason
    ON closed_lots(fund_id, exit_reason, closed_at DESC);
