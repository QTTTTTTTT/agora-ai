-- 116_users_deleted_at.sql — soft-delete column expected by admin handlers.
--
-- Several admin endpoints (admin_user_roles.go, admin_users_handler.go)
-- already filter `WHERE deleted_at IS NULL`, but the column was never
-- introduced in any prior migration, so the query crashes with
-- "column deleted_at does not exist" and the /admin page shows a
-- 500 banner. Adding the column nullable + default NULL is the
-- minimal fix — no row needs backfilling because every existing
-- user is implicitly "not deleted".

ALTER TABLE users ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_users_deleted_at ON users (deleted_at)
    WHERE deleted_at IS NOT NULL;
