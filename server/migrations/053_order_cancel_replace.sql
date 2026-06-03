-- P0-5: cancel / replace order tracking on trade_executions.
--
-- The Cancel API sets status='cancelled' AND records the actor +
-- reason for compliance. The Replace API can update qty / limit /
-- stop / trail / display fields on an open order; we track the
-- replace count and last-replace timestamp so the runtime can
-- enforce a per-order modification budget and so order-replay
-- (P1-5) can re-derive the live state from the row alone.
--
-- The hash-chained audit (data_access_log, P0-8) records each
-- cancel/replace event independently with a richer payload; these
-- columns only carry the canonical state observable on the order
-- itself.

-- BEGIN;  -- stripped: outer migration runner already wraps each file in a transaction

ALTER TABLE trade_executions
    ADD COLUMN IF NOT EXISTS cancelled_at  TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS cancel_reason VARCHAR(64),
    ADD COLUMN IF NOT EXISTS replaced_at   TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS replace_count SMALLINT NOT NULL DEFAULT 0;

COMMENT ON COLUMN trade_executions.cancelled_at IS
    'When status transitioned to cancelled. NULL on never-cancelled rows.';
COMMENT ON COLUMN trade_executions.cancel_reason IS
    'Short cancel reason: user_requested / superseded_by_replace / ttl / risk_breach / system. Free-form within 64 chars.';
COMMENT ON COLUMN trade_executions.replaced_at IS
    'Timestamp of the most recent successful Replace. Updated atomically with the field changes.';
COMMENT ON COLUMN trade_executions.replace_count IS
    'Number of successful replace calls applied. Caps at 32 in the runtime to bound retries; column is SMALLINT for headroom.';

-- Cancel reason length is bounded by the column type but we also
-- want to forbid empty strings so a legacy NULL stays distinguishable
-- from "cancelled with empty reason".
ALTER TABLE trade_executions
    DROP CONSTRAINT IF EXISTS trade_executions_cancel_reason_check;

ALTER TABLE trade_executions
    ADD CONSTRAINT trade_executions_cancel_reason_check
    CHECK (cancel_reason IS NULL OR length(cancel_reason) > 0);

-- COMMIT;  -- stripped: outer migration runner already wraps each file in a transaction
