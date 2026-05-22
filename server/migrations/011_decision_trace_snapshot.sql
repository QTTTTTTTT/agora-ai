ALTER TABLE investment_plans
    ADD COLUMN IF NOT EXISTS discussion_snapshot JSONB NOT NULL DEFAULT '{}';

CREATE INDEX IF NOT EXISTS idx_investment_plans_fund_date_created
    ON investment_plans (fund_id, trading_date DESC, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_trade_executions_plan_id_created
    ON trade_executions (plan_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_trade_executions_plan_action_id_created
    ON trade_executions (plan_action_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_memories_fund_trading_date_created
    ON memories (fund_id, trading_date DESC, created_at DESC);
