-- ============================================================
-- 118. Pricing rev — add `team` tier + seat-based subscription
--   - 加 plan_tier='team' 取值（保留 free/pro/premium/enterprise 历史值）
--   - subscriptions 加 seat_count（per-seat 团队订阅数量；个人订阅恒为 1）
-- ============================================================
BEGIN;

-- 扩 plan_tier CHECK
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_plan_tier_check;
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_plan_tier_check
    CHECK (plan_tier IN ('free','pro','premium','enterprise','team'));

-- seat_count: 个人订阅恒为 1；team 档 min 3
ALTER TABLE subscriptions
    ADD COLUMN IF NOT EXISTS seat_count INTEGER NOT NULL DEFAULT 1
        CHECK (seat_count >= 1);

-- plan_lemonsqueezy_variants 同步加 team 行（可选，由 admin 后续 INSERT）。
-- 这里不直接插行——variant_id 必须由 LS dashboard 配置后由 admin 写入。

COMMIT;
