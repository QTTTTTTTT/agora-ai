-- 016_scheduler_leases.sql
--
-- PR-01: Distributed leader election for background schedulers.
--
-- The workflow scheduler and marketplace reconciler run on every server
-- replica today. They are individually idempotent (workflow runs are
-- claimed atomically via fund_id+trading_date UNIQUE; marketplace orders
-- are reconciled with idempotency keys) but a thundering herd of N replicas
-- all hitting the database every minute is wasteful and noisy.
--
-- Instead we elect a single leader per scheduler "name" using a tiny lease
-- table. Acquisition is a single SQL statement:
--
--   INSERT INTO scheduler_leases (name, holder, acquired_at, heartbeat_at, expires_at)
--   VALUES ($1, $2, NOW(), NOW(), NOW() + interval '30 seconds')
--   ON CONFLICT (name) DO UPDATE
--   SET holder       = EXCLUDED.holder,
--       acquired_at  = EXCLUDED.acquired_at,
--       heartbeat_at = EXCLUDED.heartbeat_at,
--       expires_at   = EXCLUDED.expires_at
--   WHERE scheduler_leases.holder = EXCLUDED.holder
--      OR scheduler_leases.expires_at < NOW()
--   RETURNING holder = $2 AS is_leader;
--
-- The same statement is used both for initial acquisition and for renewal:
-- the current holder always wins; otherwise an expired lease can be stolen.

CREATE TABLE IF NOT EXISTS scheduler_leases (
    name         TEXT PRIMARY KEY,
    holder       TEXT NOT NULL,
    acquired_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    heartbeat_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at   TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_scheduler_leases_expires_at
    ON scheduler_leases (expires_at);
