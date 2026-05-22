-- Migration: 002_subscription_and_models
-- Description: 订阅体系、用量追踪、用户模型配置

-- 1. 用户表（如果不存在则创建）
CREATE TABLE IF NOT EXISTS users (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    username        VARCHAR(100) NOT NULL UNIQUE,
    display_name    VARCHAR(255),
    email           VARCHAR(255),
    phone           VARCHAR(20),
    avatar_url      TEXT,
    password_hash   TEXT,
    wechat_openid   VARCHAR(100) UNIQUE,
    status          VARCHAR(20) NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended', 'deleted')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 2. 订阅表
CREATE TABLE subscriptions (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    plan_tier       VARCHAR(20) NOT NULL CHECK (plan_tier IN ('free', 'pro', 'premium', 'enterprise')),
    status          VARCHAR(20) NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'expired', 'cancelled')),
    start_date      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    end_date        TIMESTAMPTZ NOT NULL,
    auto_renew      BOOLEAN NOT NULL DEFAULT true,
    payment_method  VARCHAR(20) DEFAULT 'manual' CHECK (payment_method IN ('wechat', 'alipay', 'manual', 'system')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_subscriptions_user_status ON subscriptions(user_id, status);
CREATE INDEX idx_subscriptions_end_date ON subscriptions(end_date);

-- 3. 用量记录表（高写入量，按月分区友好设计）
CREATE TABLE usage_entries (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    fund_id         UUID REFERENCES funds(id) ON DELETE SET NULL,
    step_name       VARCHAR(50) NOT NULL,
    model_provider  VARCHAR(30) NOT NULL,
    model_name      VARCHAR(100) NOT NULL,
    input_tokens    INTEGER NOT NULL DEFAULT 0,
    output_tokens   INTEGER NOT NULL DEFAULT 0,
    cost_cents      NUMERIC(12, 4) NOT NULL DEFAULT 0,
    price_cents     NUMERIC(12, 4) NOT NULL DEFAULT 0,
    is_custom_key   BOOLEAN NOT NULL DEFAULT false,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_usage_entries_user_date ON usage_entries(user_id, created_at);
CREATE INDEX idx_usage_entries_fund ON usage_entries(fund_id, created_at);
CREATE INDEX idx_usage_entries_model ON usage_entries(model_name, created_at);

-- 4. 日用量汇总表（定时任务聚合，加速查询）
CREATE TABLE usage_daily_summary (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    summary_date    DATE NOT NULL,
    total_calls     INTEGER NOT NULL DEFAULT 0,
    input_tokens    BIGINT NOT NULL DEFAULT 0,
    output_tokens   BIGINT NOT NULL DEFAULT 0,
    cost_cents      NUMERIC(14, 4) NOT NULL DEFAULT 0,
    price_cents     NUMERIC(14, 4) NOT NULL DEFAULT 0,
    custom_key_calls INTEGER NOT NULL DEFAULT 0,
    model_breakdown JSONB NOT NULL DEFAULT '{}',
    step_breakdown  JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX idx_usage_daily_user_date ON usage_daily_summary(user_id, summary_date);

-- 5. 用户模型配置表
CREATE TABLE user_model_configs (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    config_type     VARCHAR(30) NOT NULL CHECK (config_type IN ('tier_override', 'custom_endpoint')),
    tier            VARCHAR(20) CHECK (tier IN ('critical', 'standard', 'simple')),
    provider        VARCHAR(30) NOT NULL CHECK (provider IN ('openai', 'claude', 'deepseek', 'qwen', 'custom')),
    model_name      VARCHAR(100) NOT NULL,
    base_url        TEXT,
    api_key_encrypted TEXT,      -- AES-256-GCM 加密后的 API key
    is_active       BOOLEAN NOT NULL DEFAULT true,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_user_model_configs_user ON user_model_configs(user_id, is_active);
CREATE UNIQUE INDEX idx_user_model_configs_tier ON user_model_configs(user_id, config_type, tier) WHERE is_active = true AND config_type = 'tier_override';

-- 6. 月度账单表
CREATE TABLE monthly_bills (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    year_month      VARCHAR(7) NOT NULL,    -- "2026-04"
    plan_tier       VARCHAR(20) NOT NULL,
    subscription_fee INTEGER NOT NULL DEFAULT 0,
    model_usage_fee NUMERIC(14, 4) NOT NULL DEFAULT 0,
    custom_key_credit NUMERIC(14, 4) NOT NULL DEFAULT 0,
    total_fee       NUMERIC(14, 4) NOT NULL DEFAULT 0,
    final_amount    NUMERIC(14, 4) NOT NULL DEFAULT 0,
    status          VARCHAR(20) NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'paid', 'overdue')),
    details_json    JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX idx_monthly_bills_user_month ON monthly_bills(user_id, year_month);

-- 7. 给 fund_companies 加 owner_id
ALTER TABLE fund_companies ADD COLUMN IF NOT EXISTS owner_id UUID REFERENCES users(id);
CREATE INDEX IF NOT EXISTS idx_fund_companies_owner ON fund_companies(owner_id);

-- 8. 给 agents 加默认模型配置
ALTER TABLE agents ADD COLUMN IF NOT EXISTS model_provider VARCHAR(30) DEFAULT 'deepseek';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS model_name VARCHAR(100) DEFAULT 'deepseek-chat';
