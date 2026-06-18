-- ============================================================
-- 117. Subscription + LemonSqueezy 集成
--   - subscriptions 加 LS 关联字段（ls_subscription_id 等 9 列）
--   - plan_lemonsqueezy_variants  plan↔LS variant 映射
--   - checkout_intents            前端 success 回跳轮询用
--   - lemonsqueezy_webhook_events webhook 幂等去重
--
-- 不动 plan_tier 的 CHECK 约束（保留 free/pro/premium/enterprise
-- 4 个 tier，定价改为美元，价格点位由后端 Plans map 提供）。
-- 仅扩展 payment_method 加 'lemonsqueezy' 取值。
-- ============================================================
BEGIN;

-- 1. payment_method 扩展取值
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_payment_method_check;
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_payment_method_check
    CHECK (payment_method IN ('wechat','alipay','manual','system','lemonsqueezy'));

-- 2. subscriptions 新增 LS 关联字段
ALTER TABLE subscriptions
    ADD COLUMN IF NOT EXISTS ls_subscription_id  VARCHAR(64),
    ADD COLUMN IF NOT EXISTS ls_customer_id      VARCHAR(64),
    ADD COLUMN IF NOT EXISTS ls_variant_id       VARCHAR(64),
    ADD COLUMN IF NOT EXISTS billing_period      VARCHAR(16) DEFAULT 'monthly',
    ADD COLUMN IF NOT EXISTS locked_price_cents  INTEGER,
    ADD COLUMN IF NOT EXISTS current_period_start TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS current_period_end   TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS renews_at           TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS cancelled_at        TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS ends_at             TIMESTAMPTZ;

CREATE UNIQUE INDEX IF NOT EXISTS idx_subs_ls_subscription_id
    ON subscriptions(ls_subscription_id) WHERE ls_subscription_id IS NOT NULL;

-- 3. plan ↔ LS variant 绑定表（不在 schema 里硬填 variant id；
-- 由 admin 通过 SQL 或后续 admin endpoint 写入）
CREATE TABLE IF NOT EXISTS plan_lemonsqueezy_variants (
    plan_tier       VARCHAR(32) NOT NULL,
    billing_period  VARCHAR(16) NOT NULL CHECK (billing_period IN ('monthly','yearly')),
    ls_variant_id   VARCHAR(64) NOT NULL,
    ls_product_id   VARCHAR(64),
    price_cents_usd INTEGER NOT NULL,
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (plan_tier, billing_period)
);

-- 4. checkout 意图表（30 min TTL，前端 success 回跳后轮询查询）
CREATE TABLE IF NOT EXISTS checkout_intents (
    id              VARCHAR(32) PRIMARY KEY,        -- ULID
    user_id         UUID NOT NULL,
    plan_tier       VARCHAR(32) NOT NULL,
    billing_period  VARCHAR(16) NOT NULL,
    ls_variant_id   VARCHAR(64) NOT NULL,
    ls_checkout_id  VARCHAR(64),
    status          VARCHAR(16) NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending','completed','expired','cancelled')),
    expires_at      TIMESTAMPTZ NOT NULL,
    completed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_checkout_intents_user
    ON checkout_intents(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_checkout_intents_status
    ON checkout_intents(status, expires_at);

-- 5. webhook 幂等去重表
CREATE TABLE IF NOT EXISTS lemonsqueezy_webhook_events (
    event_id      VARCHAR(128) PRIMARY KEY,
    event_name    VARCHAR(64) NOT NULL,
    payload       JSONB NOT NULL,
    processed_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_ls_webhook_events_processed
    ON lemonsqueezy_webhook_events(processed_at DESC);

COMMIT;
