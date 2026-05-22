-- F27: super_admin dual-control + structured behaviour audit.
--
-- Two new tables:
--
--   1. admin_requests — pending high-risk operations awaiting a second
--      super_admin's approval. Submitting admin and approving admin
--      MUST differ (enforced at service layer + verified in tests).
--      Auto-expires after expires_at; expired requests cannot be
--      approved and must be re-submitted.
--
--   2. admin_change_log — append-only diff log capturing before / after
--      snapshots for every super_admin mutation. Distinct from
--      data_access_log (which is "actor X read/wrote Y"); this table is
--      "actor X changed Y from A to B".

CREATE TABLE IF NOT EXISTS admin_requests (
    id                UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    requester_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    action            TEXT NOT NULL,
    target_type       TEXT NOT NULL,
    target_id         TEXT NOT NULL,
    payload           JSONB NOT NULL DEFAULT '{}'::jsonb,
    reason            TEXT,
    status            TEXT NOT NULL DEFAULT 'pending'
                          CHECK (status IN ('pending', 'approved', 'rejected', 'expired', 'executed', 'failed')),
    approver_user_id  UUID REFERENCES users(id),
    approved_at       TIMESTAMPTZ,
    executed_at       TIMESTAMPTZ,
    execution_error   TEXT,
    expires_at        TIMESTAMPTZ NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT admin_requests_self_approval_block CHECK (
        approver_user_id IS NULL OR approver_user_id <> requester_user_id
    )
);

CREATE INDEX IF NOT EXISTS admin_requests_status_idx ON admin_requests (status, expires_at);
CREATE INDEX IF NOT EXISTS admin_requests_requester_idx ON admin_requests (requester_user_id, created_at DESC);

COMMENT ON TABLE admin_requests IS
    'Two-person approval queue. Sensitive super_admin mutations submit a row here; a different super_admin approves; the executing layer then performs the action and records the result.';
COMMENT ON COLUMN admin_requests.payload IS
    'Action-specific JSON payload. Schema is owned by the registered action handler (no DB-level validation beyond JSONB).';

CREATE TABLE IF NOT EXISTS admin_change_log (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    actor_user_id   UUID NOT NULL REFERENCES users(id) ON DELETE SET NULL,
    action          TEXT NOT NULL,
    target_type     TEXT NOT NULL,
    target_id       TEXT NOT NULL,
    request_id      UUID REFERENCES admin_requests(id) ON DELETE SET NULL,
    before_snapshot JSONB,
    after_snapshot  JSONB,
    metadata        JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS admin_change_log_actor_idx ON admin_change_log (actor_user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS admin_change_log_target_idx ON admin_change_log (target_type, target_id, created_at DESC);
CREATE INDEX IF NOT EXISTS admin_change_log_action_idx ON admin_change_log (action, created_at DESC);

COMMENT ON TABLE admin_change_log IS
    'Mutation diff log. One row per super_admin write capturing the before/after JSON snapshot so operators can answer "who changed X from A to B at time T".';
