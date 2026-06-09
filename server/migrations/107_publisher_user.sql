-- Migration: 107_publisher_user
-- Description:
--   Creates the synthetic __publisher__ user identity that the
--   /daily-picks publisher loop runs under. Without this row, the
--   LLM router's tier gate (subscription.CheckModelAccess) and the
--   budget service both try to parse advisor.PublisherUserID as a
--   UUID and fail with PG 22P02 — which the agent layer surfaces
--   as "LLM failed, returning fallback", producing only the
--   "暂无足够数据形成强观点" placeholder card.
--
-- Identity choice:
--   * Fixed UUID 00000000-0000-0000-0000-000000000001 — visibly
--     synthetic so any debug query (e.g. "who ran this audit row?")
--     immediately surfaces "the publisher", not a real user.
--   * Email __publisher@internal.local — the local TLD makes it
--     impossible to accidentally match a real user; the underscore
--     prefix sorts it to the bottom of any user-list query.
--   * Role 'user' — the users_role_check CHECK constraint only
--     permits 'user' or 'super_admin'. We pick 'user' rather than
--     'super_admin' so the row does NOT leak into admin dashboards
--     (which key on role IN ('super_admin','admin')). Metrics that
--     want to exclude the publisher should filter on
--     username NOT LIKE '\_\_publisher%' or
--     id != '00000000-0000-0000-0000-000000000001'.
--   * No password_hash — login attempts must fail at the password
--     check, even if someone discovers the email.
--
-- Plan choice:
--   * 'enterprise' — gives the publisher pipeline access to every
--     model tier (the disruptive preset uses the standard tier).
--     This is conceptually correct: the publisher pays for its own
--     LLM cost out of a single pool (tracked via llm_cost_usd on
--     daily_picks rows), and that pool needs unrestricted access
--     to whichever model the preset requires.
--
-- Wiring requirement:
--   The Go constant advisor.PublisherUserID MUST match the UUID
--   below. If you change one without the other you get the same
--   22P02 regression all over again. Both are listed here for
--   easy grep:
--
--     advisor.PublisherUserID = "00000000-0000-0000-0000-000000000001"
--     users.id                = '00000000-0000-0000-0000-000000000001'

BEGIN;

-- Idempotent INSERTs so re-running the migration on a DB that
-- already has the row is a no-op rather than an error.

-- users.username is NOT NULL + UNIQUE; pick a deliberately
-- unwieldy value so a human can't realistically reuse it. The
-- email matches the same pattern for symmetry.

INSERT INTO users (id, username, email, role, status, created_at, updated_at)
VALUES (
    '00000000-0000-0000-0000-000000000001'::UUID,
    '__publisher',
    '__publisher@internal.local',
    'user',
    'active',
    now(),
    now()
)
ON CONFLICT (id) DO NOTHING;

-- Plain INSERT ... VALUES does not accept a WHERE clause; use
-- INSERT ... SELECT with NOT EXISTS so re-runs are idempotent
-- without needing a partial unique index on (user_id, status).

INSERT INTO subscriptions (
    id, user_id, plan_tier, status,
    start_date, end_date,
    auto_renew, payment_method,
    created_at, updated_at
)
SELECT
    gen_random_uuid(),
    '00000000-0000-0000-0000-000000000001'::UUID,
    'enterprise',
    'active',
    now(),
    -- 100 year window so the publisher never accidentally
    -- transitions to "expired" and stalls daily picks.
    now() + INTERVAL '100 years',
    FALSE,
    'system',
    now(),
    now()
WHERE NOT EXISTS (
    SELECT 1 FROM subscriptions
    WHERE user_id = '00000000-0000-0000-0000-000000000001'::UUID
      AND status = 'active'
);

COMMIT;
