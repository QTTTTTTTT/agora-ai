-- Sprint 3 / L2: plan partial-fail isolation.
--
-- 现状：plan 在 trader execute 阶段 partial fill 时，目前两条路:
--   1) 所有 action 都 OK → status 'completed'
--   2) 任一 action fail → status 留在 'executing' 或 'rejected'，
--      下游 UI 一刀切显示"红色失败"，看不出"3 个中 2 个成功"。
-- 新增 'mixed' status 让 trader 在 partial fill 时显式标记，
-- 三端 UI 渲染 mixed badge 后用户能看清楚部分成交的情况。

ALTER TABLE investment_plans
    DROP CONSTRAINT IF EXISTS investment_plans_status_check;

ALTER TABLE investment_plans
    ADD CONSTRAINT investment_plans_status_check
    CHECK (status IN (
        'draft', 'risk_review', 'pending_user',
        'approved', 'rejected', 'executing', 'completed', 'mixed'
    ));
