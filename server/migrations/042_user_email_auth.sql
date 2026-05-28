-- Migration: 042_user_email_auth
-- Description: Sprint 2A — email verification + password reset + login throttling.
--   * Adds email verification/login tracking columns to users.
--   * email_verifications stores short 6-digit codes (user types the code).
--   * password_resets stores long single-use tokens (URL link, never reused).
--   * Tokens hashed at rest so a stolen row dump cannot trivially log users in.
--   * IP fingerprint hashed (sha256 hex) so we keep abuse signals without
--     storing raw IPs longer than necessary.

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS email_verified BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS email_verified_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS last_login_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS failed_login_attempts INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS locked_until TIMESTAMPTZ;

CREATE TABLE IF NOT EXISTS email_verifications (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    email           VARCHAR(255) NOT NULL,
    code_hash       TEXT NOT NULL,
    expires_at      TIMESTAMPTZ NOT NULL,
    consumed_at     TIMESTAMPTZ,
    attempts        INT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_email_verifications_user_open
    ON email_verifications (user_id, created_at DESC)
    WHERE consumed_at IS NULL;

CREATE TABLE IF NOT EXISTS password_resets (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    email           VARCHAR(255) NOT NULL,
    token_hash      TEXT NOT NULL,
    expires_at      TIMESTAMPTZ NOT NULL,
    consumed_at     TIMESTAMPTZ,
    ip_hash         TEXT,
    user_agent      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_password_resets_token_hash
    ON password_resets (token_hash);

CREATE INDEX IF NOT EXISTS idx_password_resets_user_recent
    ON password_resets (user_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_password_resets_email_recent
    ON password_resets (LOWER(email), created_at DESC);
