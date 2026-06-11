-- 113_jwt_keyring.sql — A4: persistent storage for the JWT keyring
-- so rotation survives process restart.
--
-- Background
--
-- Until now JWT_SECRETS_JSON was the single source of truth for the
-- keyring. Rotation required an operator to (a) hand-edit the env
-- var with a new active key, (b) restart the deployment. Both were
-- manual, both required a human in the loop, and there was no
-- pipeline-driven cadence — so in practice keys rotated approximately
-- never. A4 introduces an outbox-driven rotation cron; for that to
-- be useful across restarts we have to persist what the cron emits.
--
-- Encryption-at-rest
--
-- secret_encrypted holds AES-GCM(plaintext_jwt_secret, KEK) where the
-- KEK is the same MODEL_CONFIG_API_KEY_SECRET we use for stored LLM
-- provider keys (see server/internal/secrets/secrets.go::EncryptionSecret).
-- Reusing that key reduces the KEK fan-out: rotating one secret out-
-- rotates every cyphertext that depends on it, instead of having
-- N independent KEKs that each need their own rotation policy.
--
-- Status semantics
--
-- One row at a time has is_active = TRUE; every other row is kept
-- for verification of in-flight tokens that were signed under it.
-- rotated_out_at is set when is_active flips from TRUE→FALSE, so a
-- janitor query "DELETE WHERE rotated_out_at < now() - jwt token TTL"
-- can clean stale verification-only keys safely.
--
-- The (is_active) partial unique index enforces the single-active-key
-- invariant at the DB level — so a buggy rotation that forgot to demote
-- the prior key fails on INSERT rather than producing two active keys
-- with undefined "which one signs new tokens" semantics.

CREATE TABLE IF NOT EXISTS jwt_keyring (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    kid                 TEXT NOT NULL UNIQUE,
    secret_encrypted    BYTEA NOT NULL,
    secret_fingerprint  TEXT NOT NULL,
    is_active           BOOLEAN NOT NULL DEFAULT FALSE,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    rotated_out_at      TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_jwt_keyring_single_active
    ON jwt_keyring (is_active)
    WHERE is_active = TRUE;

CREATE INDEX IF NOT EXISTS idx_jwt_keyring_kid
    ON jwt_keyring (kid);

CREATE INDEX IF NOT EXISTS idx_jwt_keyring_rotated_out_at
    ON jwt_keyring (rotated_out_at)
    WHERE rotated_out_at IS NOT NULL;
