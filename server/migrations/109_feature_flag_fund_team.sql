-- Migration: 109_feature_flag_fund_team
-- Description:
--   Add the `fund_team` feature flag, seeded OFF.
--
--   Hides the per-fund "团队管理 / Team Management" surface from
--   the navigation rail and (defense-in-depth) the route + page
--   shell when the flag is OFF. The TeamManagement page used to
--   be unconditionally rendered on /funds/:id/team — this flag
--   pauses the feature while we redesign the AI-team mental
--   model. enforce_server_gate is intentionally FALSE so the
--   existing /api/funds/:id/team-members CRUD remains available
--   for admin tooling / one-off audits even while end users
--   can't reach the page.
--
--   See migration 097 for the feature_flags schema + the seed
--   pattern (same INSERT … ON CONFLICT shape).

INSERT INTO feature_flags (flag_key, label, description, enabled, affects_routes, enforce_server_gate)
VALUES
    (
        'fund_team',
        '基金团队管理',
        '基金内部 AI 团队成员管理（PM / Researcher / Trader / Risk 分工）。当前正在重构产品形态，关闭后基金侧边栏不再展示该入口，旧路由直接跳回基金总览。后端 API 仍可用，便于管理员审计。',
        FALSE,
        ARRAY['/funds/:fundId/team'],
        FALSE
    )
ON CONFLICT (flag_key) DO NOTHING;
