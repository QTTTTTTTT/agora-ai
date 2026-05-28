-- Migration: 042_user_email_auth (down)

DROP INDEX IF EXISTS idx_password_resets_email_recent;
DROP INDEX IF EXISTS idx_password_resets_user_recent;
DROP INDEX IF EXISTS idx_password_resets_token_hash;
DROP TABLE IF EXISTS password_resets;

DROP INDEX IF EXISTS idx_email_verifications_user_open;
DROP TABLE IF EXISTS email_verifications;

ALTER TABLE users
    DROP COLUMN IF EXISTS locked_until,
    DROP COLUMN IF EXISTS failed_login_attempts,
    DROP COLUMN IF EXISTS last_login_at,
    DROP COLUMN IF EXISTS email_verified_at,
    DROP COLUMN IF EXISTS email_verified;
