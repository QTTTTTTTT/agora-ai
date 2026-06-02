-- P0-8 down: drop the audit hash chain columns and indexes.
-- Rollback removes tamper detection but leaves the underlying log
-- rows intact.

BEGIN;

DROP INDEX IF EXISTS idx_admin_change_log_chain;

ALTER TABLE admin_change_log
    DROP COLUMN IF EXISTS metadata_hash,
    DROP COLUMN IF EXISTS after_hash,
    DROP COLUMN IF EXISTS before_hash,
    DROP COLUMN IF EXISTS row_hash,
    DROP COLUMN IF EXISTS prev_hash;

DROP INDEX IF EXISTS idx_data_access_log_chain;

ALTER TABLE data_access_log
    DROP COLUMN IF EXISTS details_hash,
    DROP COLUMN IF EXISTS row_hash,
    DROP COLUMN IF EXISTS prev_hash;

COMMIT;
