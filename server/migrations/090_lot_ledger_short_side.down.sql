-- 090_lot_ledger_short_side.down.sql
--
-- Rollback for 090. SAFE only if no short lots have been written
-- (i.e. position_lots WHERE side='short' is empty AND closed_lots
-- WHERE side='short' is empty). The pre-T8 lotledger never wrote
-- short rows so this precondition holds for any environment that
-- hasn't merged the lotledger short-handling code.

BEGIN;

DROP INDEX IF EXISTS idx_position_lots_open_fifo;
-- Restore the 038 index shape (no side column).
CREATE INDEX idx_position_lots_open_fifo
    ON position_lots(fund_id, instrument_key, opened_at)
    WHERE status <> 'closed';

ALTER TABLE position_lots
    DROP CONSTRAINT IF EXISTS position_lots_side_chk;
ALTER TABLE position_lots
    DROP COLUMN IF EXISTS side;

ALTER TABLE closed_lots
    DROP CONSTRAINT IF EXISTS closed_lots_side_chk;
ALTER TABLE closed_lots
    DROP COLUMN IF EXISTS side;

COMMIT;
