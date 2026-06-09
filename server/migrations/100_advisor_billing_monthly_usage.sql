-- Migration: 100_advisor_billing_monthly_usage
-- Description:
--   Phase A — advisor 模式按次服务费的月度用量聚合表。
--
-- 为什么单独一张表（而不是从 usage_entries 实时聚合）：
--   1. 配额闸门 (advisorbilling.Gate.Check) 每次 consult 之前都要查
--      "本月用了多少 unit"，扫整张 usage_entries 不可行。
--   2. usage_entries 记录的是 "model call" 维度，service unit 是
--      "advisor consult" 维度（一次 deep consult ≈ 14 个 model call）。
--      聚合粒度本来就不一样。
--   3. 月度跨表：1 个用户 1 个月 1 行，扫描成本 O(1)。
--
--   year_month 用字符串 ('2026-06') 而不是 DATE truncate，便于
--   admin 端按月报表直接 GROUP BY 该列，无需 date_trunc。

CREATE TABLE IF NOT EXISTS user_advisor_monthly_usage (
    user_id              UUID         NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    year_month           VARCHAR(7)   NOT NULL,
    deep_units_consumed  INTEGER      NOT NULL DEFAULT 0,
    quick_units_consumed INTEGER      NOT NULL DEFAULT 0,
    last_consumed_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, year_month),
    CHECK (year_month ~ '^[0-9]{4}-[0-9]{2}$'),
    CHECK (deep_units_consumed >= 0),
    CHECK (quick_units_consumed >= 0)
);

-- 给 admin 报表 + summary 读路径用的索引（按用户最新月份倒序）。
CREATE INDEX IF NOT EXISTS idx_advisor_monthly_usage_recent
    ON user_advisor_monthly_usage (user_id, year_month DESC);

COMMENT ON TABLE  user_advisor_monthly_usage IS
    'Phase A advisor 按次服务费的月度用量聚合。一次 consult 在 advisorbilling.Gate.Consume 时自增 1。';
COMMENT ON COLUMN user_advisor_monthly_usage.year_month IS
    '七字符 YYYY-MM，例如 2026-06。';
