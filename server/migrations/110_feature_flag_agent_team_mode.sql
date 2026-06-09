-- Migration: 110_feature_flag_agent_team_mode
-- Description:
--   Add the `agent_team_mode` feature flag, seeded OFF.
--
--   This is the umbrella gate for the entire "AI team manages a
--   fund" product surface: /companies (the company + fund chooser)
--   and the whole /funds/:fundId/* subtree (Dashboard, Performance,
--   Team, Decisions, A/B compare, Forward gate, Backtests, Trades,
--   Cash ledger, Workflow, Models, etc.).
--
--   Until this flag flips ON the SPA hides the surface behind a
--   redirect to /masters (the new master-team Hub) for ordinary
--   users. super_admin sessions bypass the redirect so operators
--   can still reach the fund UI for audits, data cleanup, or to
--   onboard a beta cohort manually before enabling broadly.
--
--   enforce_server_gate is intentionally FALSE: the REST surface
--   (/api/companies, /api/funds/...) stays reachable so admin
--   tooling, the mobile app's existing wiring, and unit tests
--   continue to function while the SPA navigation hides the entry
--   point. Flip this to TRUE in a follow-up migration if the
--   product decision firms up into "fully retire the surface".
--
--   Note this is a SEPARATE flag from the narrower `fund_team`
--   added in migration 109. `fund_team` only hides the per-fund
--   "团队管理" subtab — it's a leaf inside the bigger surface this
--   flag controls. Keeping the two distinct lets operators turn
--   the broader product OFF without losing the ability to soft-
--   release just the team subtab back to a pilot group later.
--
--   See migration 097 for the feature_flags schema + the seed
--   pattern (same INSERT … ON CONFLICT shape).

INSERT INTO feature_flags (flag_key, label, description, enabled, affects_routes, enforce_server_gate)
VALUES
    (
        'agent_team_mode',
        'AI 团队炒股模式',
        'AI 团队管理基金的完整产品入口：公司/基金列表（/companies）以及所有基金子页面（/funds/:id/*，包含 Dashboard、Performance、Team、Decisions、A/B Compare、Forward Gate、Backtests、Trades、Cash Ledger、Workflow、Models 等）。关闭时普通用户在 SPA 内被重定向到 /masters 大师团队 Hub，super_admin 不受影响（可继续做数据维护与审计）。后端 REST 保持可用，只有前端入口被收敛。',
        FALSE,
        ARRAY['/companies', '/funds/:fundId', '/funds/:fundId/*'],
        FALSE
    )
ON CONFLICT (flag_key) DO NOTHING;
