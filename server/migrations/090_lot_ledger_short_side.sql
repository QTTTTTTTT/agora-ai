-- 090_lot_ledger_short_side.sql
-- T8 of the trader-agent step-2 integration.
--
-- Extends position_lots and closed_lots with a `side` column so
-- the lot ledger can represent short positions in addition to
-- the long positions modelled since 038. The short side adds
-- two new semantics:
--
--   recordShortOpen  (SELL with position_side='short')
--     -> opens a new short lot, entry_price = fill price, qty positive.
--   recordShortClose (BUY with position_side='short', cover to close)
--     -> FIFO-walks open short lots and emits closed_lots rows with
--        realized_pnl = (entry - exit) * qty * fees, opposite sign
--        of the long PnL formula.
--
-- Why a column instead of a sibling table:
--   - Most queries that want "all lots for an instrument" still want
--     both sides aggregated (e.g. exposure check, full position view).
--   - Indexes already include (fund_id, instrument_key, opened_at);
--     adding `side` to that hot-path index is a single ALTER vs the
--     dual-table approach which would need an UNION view everywhere.
--   - The existing CHECK on quantity_remaining/quantity_opened/status
--     applies identically — long and short lots both decrement qty
--     remaining as their respective close happens.
--
-- Default 'long' on the column + backfill of existing rows means
-- every historical lot becomes a long lot (which is what it was —
-- pre-T8 lotledger only ever wrote long lots). No data loss, no
-- recomputed PnL.
--
-- Indexes:
--   - The hot path is "list open lots for FIFO close" which now
--     needs a side filter. The existing idx_position_lots_open_fifo
--     is REPLACED with one that includes side as a leading equality
--     column so a side='long' lookup uses the index unchanged and
--     a side='short' lookup uses the same shape on the short side.
--   - closed_lots gets a side column but no new index — the closed
--     side lookup is by closing_trade_id / position_lot_id, which
--     already covers both lot sides equally.

BEGIN;

ALTER TABLE position_lots
    ADD COLUMN side VARCHAR(8) NOT NULL DEFAULT 'long';

ALTER TABLE position_lots
    ADD CONSTRAINT position_lots_side_chk
    CHECK (side IN ('long', 'short'));

ALTER TABLE closed_lots
    ADD COLUMN side VARCHAR(8) NOT NULL DEFAULT 'long';

ALTER TABLE closed_lots
    ADD CONSTRAINT closed_lots_side_chk
    CHECK (side IN ('long', 'short'));

-- Replace the FIFO index so side is part of the equality prefix.
-- IF EXISTS is defensive — 038's CREATE INDEX uses the same name.
DROP INDEX IF EXISTS idx_position_lots_open_fifo;
CREATE INDEX idx_position_lots_open_fifo
    ON position_lots(fund_id, instrument_key, side, opened_at)
    WHERE status <> 'closed';

COMMENT ON COLUMN position_lots.side IS
    'Lot direction: long (default) or short. Short lots track sell-to-open / buy-to-cover semantics, opposite sign on realized PnL.';
COMMENT ON COLUMN closed_lots.side IS
    'Mirrors position_lots.side at the moment of close. Always equal to the originating position_lots row.';

COMMIT;
