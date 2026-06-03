-- P0-2: extend trade_executions schema for live-trading order types and lifecycle.
--
-- Why this migration exists
-- -------------------------
-- The legacy schema (001_init.sql) was tuned for the simulator era:
--   * order_type CHECK ('market', 'limit')  — only 2 of 8 types we need
--   * status     CHECK ('pending','filled','partial','cancelled','rejected')
--                                            — no 'working' (resting limit),
--                                              no 'triggered' (stop fired),
--                                              no 'expired' (TIF reached)
-- and it had no fields for stop trigger price, trailing-stop parameters,
-- iceberg display size, time-in-force, or bracket parent linkage. These
-- are all required for any live broker integration (and also for
-- accurate simulation in the new broker.Simulator path landed in P0-1).
--
-- What this migration does
-- ------------------------
--   1. Extends order_type CHECK to the 8-type vocabulary used by
--      broker.OrderType: market / limit / stop / stop_limit /
--      trailing_stop / moc / moo / iceberg.
--   2. Extends status CHECK with three lifecycle states the broker
--      interface returns: working / triggered / expired.
--   3. Adds 7 new nullable columns capturing the order parameters and
--      the bracket parent linkage. Every column is nullable + indexed
--      where the runtime needs to query the open subset, so legacy
--      rows (no stop, no TIF) keep working unchanged.
--
-- What this migration deliberately does NOT do
-- --------------------------------------------
--   * It does NOT touch plan_actions. The LLM-PM intent layer keeps
--     the existing stop_loss / take_profit fields; bracket order
--     generation lives engine-side and produces multiple
--     trade_executions rows linked via parent_trade_id.
--   * It does NOT make client_idempotency_key NOT NULL. That column
--     was added in 027 with a partial unique index (WHERE NOT NULL).
--     The runtime engine starts populating it for new rows in this
--     PR's Go-side change; backfilling legacy rows is unnecessary
--     because the partial uniqueness is already correct.
--
-- Backwards compatibility
-- -----------------------
-- Existing rows have order_type IN ('market','limit') which remain
-- valid under the new CHECK. Existing rows have status values that
-- remain valid under the new CHECK. New columns default to NULL so
-- the existing INSERT statements in fund_repo.go keep working
-- without code change until the engine starts populating them.

-- BEGIN;  -- stripped: outer migration runner already wraps each file in a transaction

-- ---------------------------------------------------------------------------
-- 1. Extend order_type vocabulary
-- ---------------------------------------------------------------------------

ALTER TABLE trade_executions
    DROP CONSTRAINT IF EXISTS trade_executions_order_type_check;

ALTER TABLE trade_executions
    ADD CONSTRAINT trade_executions_order_type_check
    CHECK (order_type IN (
        'market',
        'limit',
        'stop',
        'stop_limit',
        'trailing_stop',
        'moc',
        'moo',
        'iceberg'
    ));

-- The plan-side execution status enum also needs to permit the new
-- broker lifecycle values so partial-fill / triggered states can
-- propagate up. plan_actions.execution_status is unconstrained today
-- (no CHECK), so we only need to widen the executions side.

-- ---------------------------------------------------------------------------
-- 2. Extend status vocabulary
-- ---------------------------------------------------------------------------

ALTER TABLE trade_executions
    DROP CONSTRAINT IF EXISTS trade_executions_status_check;

ALTER TABLE trade_executions
    ADD CONSTRAINT trade_executions_status_check
    CHECK (status IN (
        'pending',
        'working',
        'triggered',
        'partial',
        'filled',
        'cancelled',
        'rejected',
        'expired'
    ));

-- ---------------------------------------------------------------------------
-- 3. New parameter columns
-- ---------------------------------------------------------------------------

ALTER TABLE trade_executions
    ADD COLUMN IF NOT EXISTS stop_price        NUMERIC(16, 4),
    ADD COLUMN IF NOT EXISTS trail_amount      NUMERIC(16, 4),
    ADD COLUMN IF NOT EXISTS trail_percent     NUMERIC(8, 6),
    ADD COLUMN IF NOT EXISTS display_qty       NUMERIC(16, 4),
    ADD COLUMN IF NOT EXISTS time_in_force     VARCHAR(8),
    ADD COLUMN IF NOT EXISTS good_till_date    TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS parent_trade_id   UUID
        REFERENCES trade_executions(id) ON DELETE SET NULL;

-- A small CHECK keeps the TIF column readable (NULL = engine default
-- of 'day' is applied at read time; explicit values must be from the
-- broker.TimeInForce vocabulary).
ALTER TABLE trade_executions
    DROP CONSTRAINT IF EXISTS trade_executions_tif_check;

ALTER TABLE trade_executions
    ADD CONSTRAINT trade_executions_tif_check
    CHECK (time_in_force IS NULL OR time_in_force IN (
        'day', 'gtc', 'ioc', 'fok', 'gtd', 'opg'
    ));

COMMENT ON COLUMN trade_executions.stop_price IS
    'Trigger price for stop / stop_limit orders. NULL for non-stop types.';
COMMENT ON COLUMN trade_executions.trail_amount IS
    'Absolute trailing distance (price units) for trailing_stop. Mutually exclusive with trail_percent.';
COMMENT ON COLUMN trade_executions.trail_percent IS
    'Fractional trailing distance (0.05 = 5%) for trailing_stop. Mutually exclusive with trail_amount.';
COMMENT ON COLUMN trade_executions.display_qty IS
    'Visible-qty for iceberg orders. NULL for non-iceberg types.';
COMMENT ON COLUMN trade_executions.time_in_force IS
    'broker.TimeInForce: day / gtc / ioc / fok / gtd / opg. NULL = engine applies day default.';
COMMENT ON COLUMN trade_executions.good_till_date IS
    'Expiry timestamp for time_in_force = gtd.';
COMMENT ON COLUMN trade_executions.parent_trade_id IS
    'Bracket parent — set on stop-loss / take-profit / OCO child orders so the engine can de-activate siblings when one fills.';

-- ---------------------------------------------------------------------------
-- 4. Indexes for the runtime hot paths
-- ---------------------------------------------------------------------------

-- Stop-trigger engine (P0-3) scans every quote tick against open
-- stop / trailing orders. Partial index on the active subset keeps
-- the scan cheap even on large historical tables.
CREATE INDEX IF NOT EXISTS idx_trade_executions_active_stop
    ON trade_executions (fund_id, order_type, stop_price)
    WHERE status IN ('pending', 'working') AND stop_price IS NOT NULL;

-- Cancel/Replace API (P0-5) and order-replay loop (P1-5) both need a
-- fast "all open orders for this fund" query.
CREATE INDEX IF NOT EXISTS idx_trade_executions_open_by_fund
    ON trade_executions (fund_id)
    WHERE status IN ('pending', 'working', 'triggered', 'partial');

-- Bracket sibling lookup: when one leg fills, find and cancel the others.
CREATE INDEX IF NOT EXISTS idx_trade_executions_parent_trade
    ON trade_executions (parent_trade_id)
    WHERE parent_trade_id IS NOT NULL;

-- TIF=GTD expiry sweeper.
CREATE INDEX IF NOT EXISTS idx_trade_executions_gtd_expiry
    ON trade_executions (good_till_date)
    WHERE good_till_date IS NOT NULL AND status IN ('pending', 'working');

-- COMMIT;  -- stripped: outer migration runner already wraps each file in a transaction
