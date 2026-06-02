-- P0-5 down migration. Drops the cancel / replace tracking columns.
-- Preserved data is lost; do NOT run this except in a development
-- reset.

BEGIN;

ALTER TABLE trade_executions
    DROP CONSTRAINT IF EXISTS trade_executions_cancel_reason_check;

ALTER TABLE trade_executions
    DROP COLUMN IF EXISTS cancelled_at,
    DROP COLUMN IF EXISTS cancel_reason,
    DROP COLUMN IF EXISTS replaced_at,
    DROP COLUMN IF EXISTS replace_count;

COMMIT;
