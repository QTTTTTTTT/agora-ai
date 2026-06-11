-- 113_jwt_keyring.down.sql — drop the persistent keyring table.
--
-- Rolling back this migration loses every key that was rotated in
-- via the outbox flow after the deploy that added the table. The
-- env-supplied JWT_SECRETS_JSON / JWT_SECRET path still works, so
-- the platform stays bootable; operators just have to re-issue
-- whatever post-deploy rotations they wanted to keep.
--
-- The order matters: drop indexes first (they reference the table),
-- then the table. IF EXISTS on every line so a partial state from
-- a failed forward run doesn't block the rollback.

DROP INDEX IF EXISTS idx_jwt_keyring_rotated_out_at;
DROP INDEX IF EXISTS idx_jwt_keyring_kid;
DROP INDEX IF EXISTS uq_jwt_keyring_single_active;
DROP TABLE IF EXISTS jwt_keyring;
