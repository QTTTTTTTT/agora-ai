-- F28: per-fund resource quotas.
--
-- The dollar-based LLM budget (migration 026) is *per-user*. Quotas
-- here are *per-fund* — distinct in three ways:
--   1. enforced regardless of which user kicks off the workflow,
--   2. typed by resource (agents / concurrent_workflows / llm_tokens),
--   3. measured against operational counters (running rows / token
--      consumption rolled up by trading_date).
--
-- A nullable fund_id is allowed so platform-default quotas can live
-- in the same table (fund_id IS NULL → applies to any fund that has
-- no explicit override).

CREATE TABLE IF NOT EXISTS fund_quotas (
    id                          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    fund_id                     UUID REFERENCES funds(id) ON DELETE CASCADE,
    max_active_agents           INTEGER,
    max_concurrent_workflows    INTEGER,
    daily_llm_token_limit       BIGINT,
    monthly_llm_token_limit     BIGINT,
    notes                       TEXT,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fund_quotas_non_negative CHECK (
        COALESCE(max_active_agents, 0) >= 0 AND
        COALESCE(max_concurrent_workflows, 0) >= 0 AND
        COALESCE(daily_llm_token_limit, 0) >= 0 AND
        COALESCE(monthly_llm_token_limit, 0) >= 0
    )
);

-- Partial unique indexes mirror migration 026's two-tier pattern:
-- one row per fund, plus exactly one platform-default row.
CREATE UNIQUE INDEX IF NOT EXISTS fund_quotas_fund_uniq
    ON fund_quotas (fund_id)
    WHERE fund_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS fund_quotas_default_uniq
    ON fund_quotas ((TRUE))
    WHERE fund_id IS NULL;

COMMENT ON TABLE fund_quotas IS
    'Per-fund resource quotas (F28). fund_id NULL row is the platform default applied when no per-fund override exists.';

-- LLM token usage roll-up. Populated by the workflow / agent layer as
-- it consumes tokens. Keyed on (fund_id, trading_date) so we can
-- enforce both daily caps and (via SUM over the last 30 days) monthly
-- caps without a separate table.
CREATE TABLE IF NOT EXISTS fund_llm_token_usage (
    fund_id         UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    trading_date    DATE NOT NULL,
    prompt_tokens   BIGINT NOT NULL DEFAULT 0,
    completion_tokens BIGINT NOT NULL DEFAULT 0,
    total_tokens    BIGINT NOT NULL DEFAULT 0,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (fund_id, trading_date)
);

CREATE INDEX IF NOT EXISTS fund_llm_token_usage_date_idx
    ON fund_llm_token_usage (trading_date DESC);

COMMENT ON TABLE fund_llm_token_usage IS
    'Daily LLM token consumption per fund. UPSERTed by the LLM client after each successful call; the quota service reads this table to enforce daily_llm_token_limit and monthly_llm_token_limit.';
