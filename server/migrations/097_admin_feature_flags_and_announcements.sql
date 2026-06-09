-- Migration: 097_admin_feature_flags_and_announcements
-- Description:
--   * feature_flags        — admin-controlled toggle for product
--                            surfaces (e.g. AB compare). Server is
--                            the source of truth so all clients
--                            (web, miniapp, android) honour the flip.
--   * announcements        — admin-published in-app messages with
--                            severity for graceful "we paused X"
--                            broadcasts.
--   * announcement_reads   — per-user read tracking so the banner
--                            can dismiss itself.
--
--   We deliberately do NOT couple feature flags to a global config
--   blob: each row carries its own description / metadata so the
--   admin console can render a self-documenting toggle list. The
--   `affects_routes` text array is purely advisory metadata for
--   the UI ("when off, hide these routes"); enforcement is up to
--   the consumer.

CREATE TABLE IF NOT EXISTS feature_flags (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    flag_key        VARCHAR(64) NOT NULL UNIQUE,
    label           VARCHAR(255) NOT NULL,
    description     TEXT,
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    affects_routes  TEXT[] NOT NULL DEFAULT '{}',
    -- enforce_server_gate=TRUE means handlers wired to this flag
    -- (e.g. /api/funds/:id/abtests) MUST short-circuit with 503
    -- when enabled=FALSE. UI hide is always honoured; server
    -- enforcement is opt-in per flag because flipping it can
    -- break in-flight workflows for some surfaces.
    enforce_server_gate BOOLEAN NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by      UUID REFERENCES users(id)
);

CREATE INDEX IF NOT EXISTS idx_feature_flags_enabled ON feature_flags (enabled);

-- Pre-seed common flags so admin console has something to render
-- on day one. Keys match what the web client already references in
-- its router (see App.tsx routes for AB compare / agent marketplace).
INSERT INTO feature_flags (flag_key, label, description, enabled, affects_routes, enforce_server_gate)
VALUES
    (
        'ab_test_compare',
        'A/B 测试对比',
        '基金内策略 A/B 对比中心。关闭后基金侧边栏不再展示该入口，且后端会拒绝相关接口请求。',
        TRUE,
        ARRAY['/funds/:fundId/compare'],
        TRUE
    ),
    (
        'agent_marketplace',
        'Agent 市场',
        '社区共享 Agent 与策略市场。关闭后顶部导航不再展示。',
        TRUE,
        ARRAY['/marketplace'],
        FALSE
    ),
    (
        'cross_fund_skills',
        '跨基金技能',
        '团队复用其他基金已验证的技能模块。',
        TRUE,
        ARRAY[]::TEXT[],
        FALSE
    ),
    (
        'agent_lineage',
        'Agent 进化谱系',
        '展示每个 Agent 的成长史与技能传承。',
        TRUE,
        ARRAY['/agent-lineage'],
        FALSE
    )
ON CONFLICT (flag_key) DO NOTHING;

CREATE TABLE IF NOT EXISTS announcements (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    title           VARCHAR(200) NOT NULL,
    body            TEXT NOT NULL,
    -- severity is purely a UI hint: 'info' (blue), 'warning' (amber),
    -- 'critical' (red, sticky banner). Not enforced at the API
    -- layer because the surface treats it cosmetically.
    severity        VARCHAR(16) NOT NULL DEFAULT 'info'
                       CHECK (severity IN ('info', 'warning', 'critical')),
    published_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    published_by    UUID REFERENCES users(id),
    -- archived_at IS NULL → live announcement. Soft delete keeps
    -- the audit trail (operator can see "X published this on Y")
    -- without exposing an inactive announcement to end users.
    archived_at     TIMESTAMPTZ,
    archived_by     UUID REFERENCES users(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_announcements_active
    ON announcements (published_at DESC)
    WHERE archived_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_announcements_published_by
    ON announcements (published_by, published_at DESC);

CREATE TABLE IF NOT EXISTS announcement_reads (
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    announcement_id UUID NOT NULL REFERENCES announcements(id) ON DELETE CASCADE,
    read_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, announcement_id)
);

CREATE INDEX IF NOT EXISTS idx_announcement_reads_user
    ON announcement_reads (user_id, read_at DESC);
