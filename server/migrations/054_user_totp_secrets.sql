-- Migration 054: User TOTP Secrets (P0-6)
--
-- Stores per-user TOTP (RFC 6238) secrets and one-time recovery
-- codes used by the 2FA login challenge flow. The `secret_encrypted`
-- column holds the AES-GCM-encrypted base32 secret produced by the
-- internal/totp module; the encryption key is sourced from
-- TOTP_ENCRYPTION_KEY (32 bytes hex) in the environment so a DB
-- snapshot leak alone cannot bypass 2FA.
--
-- recovery_codes_hashed is a Postgres text array of bcrypt-hashed
-- single-use codes. The plaintext is shown to the user exactly once
-- at enrolment time. When the user authenticates with a recovery
-- code we MUST remove it from the array (see TOTPRepo.ConsumeRecoveryCode).
--
-- enabled_at is NULL until the user verifies their first TOTP code,
-- which closes the enrolment loop and "arms" 2FA on subsequent
-- logins. Until then the row exists but does not gate login.
--
-- Audit trail: every state transition (enroll / verify / disable /
-- recovery_used) emits a row to data_access_log via the audit
-- chain. We deliberately do NOT mirror those events in this table —
-- the audit chain is the source of truth, this table only holds
-- live state needed at login time.

CREATE TABLE IF NOT EXISTS user_totp_secrets (
    user_id                 UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    secret_encrypted        BYTEA NOT NULL,
    issuer                  TEXT NOT NULL DEFAULT 'FundAI',
    account_label           TEXT NOT NULL,
    digits                  INTEGER NOT NULL DEFAULT 6,
    period_seconds          INTEGER NOT NULL DEFAULT 30,
    algorithm               TEXT NOT NULL DEFAULT 'SHA1',
    recovery_codes_hashed   TEXT[] NOT NULL DEFAULT '{}',
    -- enrolment_attempts tracks failed first-verify attempts so a
    -- noisy enrolment can be auto-aborted after N tries. Re-enrolment
    -- resets this column.
    enrolment_attempts      INTEGER NOT NULL DEFAULT 0,
    enabled_at              TIMESTAMPTZ,
    last_verified_at        TIMESTAMPTZ,
    last_used_recovery_at   TIMESTAMPTZ,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT user_totp_digits_range CHECK (digits BETWEEN 6 AND 8),
    CONSTRAINT user_totp_period_range CHECK (period_seconds BETWEEN 30 AND 120),
    CONSTRAINT user_totp_algorithm_allowed CHECK (algorithm IN ('SHA1', 'SHA256', 'SHA512'))
);

-- Per-user audit-friendly trigger: bump updated_at on every UPDATE.
-- Defined as an independent function so other tables can reuse it
-- (we don't currently, but the platform uses this pattern in earlier
-- migrations so consistency wins).
CREATE OR REPLACE FUNCTION user_totp_touch_updated_at() RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS user_totp_touch_updated_at ON user_totp_secrets;
CREATE TRIGGER user_totp_touch_updated_at
    BEFORE UPDATE ON user_totp_secrets
    FOR EACH ROW
    EXECUTE FUNCTION user_totp_touch_updated_at();

-- Lookup index for the login challenge handler — given a user_id
-- we need to know "is 2FA enabled?" in O(1). The PRIMARY KEY on
-- user_id covers exact lookup, and the partial index below speeds
-- up "list all users with 2FA enabled" admin reports without
-- adding write overhead on the common (NOT enabled) path.
CREATE INDEX IF NOT EXISTS user_totp_enabled_idx
    ON user_totp_secrets (enabled_at)
    WHERE enabled_at IS NOT NULL;
