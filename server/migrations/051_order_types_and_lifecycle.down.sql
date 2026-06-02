-- P0-2 down: restore the legacy 2-type / 5-state vocabulary on
-- trade_executions and drop the new columns.
--
-- Caveat: rows that have been written with order_type or status
-- values outside the legacy vocabulary will block the down-migration.
-- The DELETE before the CHECK re-add is intentional — without it the
-- ALTER would fail. Operators applying this rollback must accept that
-- non-legacy rows are removed (a forward-only migration is the
-- preferred pattern in production; this down-script is for dev/CI
-- only).

BEGIN;

-- 1. Drop indexes
DROP INDEX IF EXISTS idx_trade_executions_gtd_expiry;
DROP INDEX IF EXISTS idx_trade_executions_parent_trade;
DROP INDEX IF EXISTS idx_trade_executions_open_by_fund;
DROP INDEX IF EXISTS idx_trade_executions_active_stop;

-- 2. Drop the new CHECK and re-add the legacy ones.
ALTER TABLE trade_executions
    DROP CONSTRAINT IF EXISTS trade_executions_tif_check;

ALTER TABLE trade_executions
    DROP CONSTRAINT IF EXISTS trade_executions_status_check;
DELETE FROM trade_executions
    WHERE status IN ('working', 'triggered', 'expired');
ALTER TABLE trade_executions
    ADD CONSTRAINT trade_executions_status_check
    CHECK (status IN ('pending', 'filled', 'partial', 'cancelled', 'rejected'));

ALTER TABLE trade_executions
    DROP CONSTRAINT IF EXISTS trade_executions_order_type_check;
DELETE FROM trade_executions
    WHERE order_type NOT IN ('market', 'limit');
ALTER TABLE trade_executions
    ADD CONSTRAINT trade_executions_order_type_check
    CHECK (order_type IN ('market', 'limit'));

-- 3. Drop the new columns.
ALTER TABLE trade_executions
    DROP COLUMN IF EXISTS parent_trade_id,
    DROP COLUMN IF EXISTS good_till_date,
    DROP COLUMN IF EXISTS time_in_force,
    DROP COLUMN IF EXISTS display_qty,
    DROP COLUMN IF EXISTS trail_percent,
    DROP COLUMN IF EXISTS trail_amount,
    DROP COLUMN IF EXISTS stop_price;

COMMIT;
