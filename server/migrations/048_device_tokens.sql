-- 048_device_tokens.sql
--
-- Sprint 4 / android-core: FCM (and APNs via FCM bridge) device-token registry.
-- The server fan-outs push notifications for 4 trigger classes:
--
--   * plan_ready       — a new investment_plan landed for a fund the user owns
--   * plan_failed      — workflow ended with status failed/timeout
--   * plan_mixed       — Sprint 3 / L1 partial-fill heads-up
--   * reflection_ready — long-term reflection landed
--
-- Tokens are per (user_id, token); a single user can have N devices, and
-- a single device can re-register after token refresh (we upsert on the
-- token column). Soft-deletes are implemented via revoked_at so the
-- push fan-out can still attribute past sends; physical purge of stale
-- rows is the operator's job (cron not in scope for this migration).

CREATE TABLE IF NOT EXISTS device_tokens (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token        TEXT NOT NULL,
    platform     TEXT NOT NULL CHECK (platform IN ('android', 'ios', 'web')),
    app_version  TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at   TIMESTAMPTZ
);

-- Same token may show up for a different user (device re-login as
-- another account). We dedup per (user_id, token) — same user
-- re-registering merely bumps last_seen_at via upsert.
CREATE UNIQUE INDEX IF NOT EXISTS device_tokens_user_token_uniq
    ON device_tokens (user_id, token);

-- Fan-out path: "give me all active tokens for users in fund X" is the
-- hot query. We don't have a fund linkage on device_tokens itself —
-- the join goes device_tokens -> users -> fund_company_members -> funds.
-- A plain user_id index covers it.
CREATE INDEX IF NOT EXISTS device_tokens_user_idx
    ON device_tokens (user_id) WHERE revoked_at IS NULL;
