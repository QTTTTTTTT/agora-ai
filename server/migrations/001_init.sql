-- ============================================================================
-- Migration 001: Initial Schema
-- AI Fund Company Simulator — PostgreSQL
-- ============================================================================

-- UP Migration
-- ============================================================================

-- Extensions
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

-- ----------------------------------------------------------------------------
-- 1. Fund Company & Funds
-- ----------------------------------------------------------------------------

CREATE TABLE fund_companies (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name        VARCHAR(255) NOT NULL,
    description TEXT,
    owner_user_id UUID NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
COMMENT ON TABLE fund_companies IS 'Top-level entity representing an AI fund management company';

CREATE TABLE funds (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    company_id      UUID NOT NULL REFERENCES fund_companies(id) ON DELETE CASCADE,
    name            VARCHAR(255) NOT NULL,
    description     TEXT,
    trading_mode    VARCHAR(20) NOT NULL DEFAULT 'simulation'
                        CHECK (trading_mode IN ('simulation', 'live', 'paper')),
    initial_capital NUMERIC(20, 4) NOT NULL DEFAULT 0,
    current_capital NUMERIC(20, 4) NOT NULL DEFAULT 0,
    total_assets    NUMERIC(20, 4) NOT NULL DEFAULT 0,
    nav             NUMERIC(20, 6) NOT NULL DEFAULT 1.0,
    status          VARCHAR(20) NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active', 'paused', 'closed')),
    config          JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
COMMENT ON TABLE funds IS 'Individual investment fund managed by AI agents';

CREATE INDEX idx_funds_company_id ON funds (company_id);
CREATE INDEX idx_funds_status     ON funds (status);

-- ----------------------------------------------------------------------------
-- 2. Agents & Teams
-- ----------------------------------------------------------------------------

CREATE TABLE agents (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name             VARCHAR(255) NOT NULL,
    role             VARCHAR(20) NOT NULL
                        CHECK (role IN ('pm', 'researcher', 'trader', 'risk')),
    focus            VARCHAR(30)
                        CHECK (focus IS NULL OR focus IN ('stock', 'fundamental', 'macro')),
    llm_model        VARCHAR(128),
    system_prompt    TEXT,
    skill_config     JSONB NOT NULL DEFAULT '{}',
    domain_config    JSONB NOT NULL DEFAULT '{}',
    evolution_config JSONB NOT NULL DEFAULT '{}',
    status           VARCHAR(20) NOT NULL DEFAULT 'active',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
COMMENT ON TABLE agents IS 'AI agent definitions with LLM config and domain specialisation';

CREATE INDEX idx_agents_role   ON agents (role);
CREATE INDEX idx_agents_status ON agents (status);

CREATE TABLE fund_team_members (
    id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    fund_id    UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    agent_id   UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    role       VARCHAR(20) NOT NULL
                   CHECK (role IN ('pm', 'researcher', 'trader', 'risk')),
    focus      VARCHAR(30)
                   CHECK (focus IS NULL OR focus IN ('stock', 'fundamental', 'macro')),
    joined_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    status     VARCHAR(20) NOT NULL DEFAULT 'active'
                   CHECK (status IN ('active', 'inactive')),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (fund_id, agent_id)
);
COMMENT ON TABLE fund_team_members IS 'Many-to-many mapping of agents assigned to a fund';

CREATE INDEX idx_fund_team_members_fund_id  ON fund_team_members (fund_id);
CREATE INDEX idx_fund_team_members_agent_id ON fund_team_members (agent_id);

-- ----------------------------------------------------------------------------
-- 3. Investment Plans & Actions
-- ----------------------------------------------------------------------------

CREATE TABLE investment_plans (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    fund_id         UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    trading_date    DATE NOT NULL,
    status          VARCHAR(20) NOT NULL DEFAULT 'draft'
                        CHECK (status IN (
                            'draft', 'risk_review', 'pending_user',
                            'approved', 'rejected', 'executing', 'completed'
                        )),
    reasoning       TEXT,
    risk_score      NUMERIC(5, 2),
    expected_return NUMERIC(8, 4),
    roundtable_id   UUID,
    pm_agent_id     UUID,
    risk_review     JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
COMMENT ON TABLE investment_plans IS 'Investment plan produced by the PM agent after roundtable discussion';

CREATE INDEX idx_investment_plans_fund_id      ON investment_plans (fund_id);
CREATE INDEX idx_investment_plans_trading_date  ON investment_plans (trading_date);
CREATE INDEX idx_investment_plans_status        ON investment_plans (status);

CREATE TABLE plan_actions (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    plan_id          UUID NOT NULL REFERENCES investment_plans(id) ON DELETE CASCADE,
    symbol           VARCHAR(20) NOT NULL,
    action           VARCHAR(10) NOT NULL
                        CHECK (action IN ('buy', 'sell', 'hold', 'reduce', 'add')),
    quantity         NUMERIC(16, 4),
    price            NUMERIC(16, 4),
    amount           NUMERIC(20, 4),
    stop_loss        NUMERIC(16, 4),
    take_profit      NUMERIC(16, 4),
    reasoning        TEXT,
    confidence       NUMERIC(5, 4),
    supported_by     TEXT[],
    opposed_by       TEXT[],
    execution_status VARCHAR(20) NOT NULL DEFAULT 'pending',
    sort_order       INT NOT NULL DEFAULT 0
);
COMMENT ON TABLE plan_actions IS 'Individual trade actions within an investment plan';

CREATE INDEX idx_plan_actions_plan_id ON plan_actions (plan_id);
CREATE INDEX idx_plan_actions_symbol  ON plan_actions (symbol);

-- ----------------------------------------------------------------------------
-- 4. Trading
-- ----------------------------------------------------------------------------

CREATE TABLE trade_executions (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    fund_id         UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    plan_id         UUID REFERENCES investment_plans(id) ON DELETE SET NULL,
    plan_action_id  UUID REFERENCES plan_actions(id) ON DELETE SET NULL,
    symbol          VARCHAR(20) NOT NULL,
    side            VARCHAR(10) NOT NULL CHECK (side IN ('buy', 'sell')),
    order_type      VARCHAR(10) NOT NULL CHECK (order_type IN ('market', 'limit')),
    quantity        NUMERIC(16, 4) NOT NULL,
    price           NUMERIC(16, 4),
    amount          NUMERIC(20, 4),
    filled_qty      NUMERIC(16, 4) NOT NULL DEFAULT 0,
    filled_price    NUMERIC(16, 4),
    fee_commission  NUMERIC(12, 4) NOT NULL DEFAULT 0,
    fee_stamp_tax   NUMERIC(12, 4) NOT NULL DEFAULT 0,
    fee_transfer    NUMERIC(12, 4) NOT NULL DEFAULT 0,
    trading_mode    VARCHAR(20) NOT NULL DEFAULT 'simulation'
                        CHECK (trading_mode IN ('simulation', 'live', 'paper')),
    broker_order_id VARCHAR(128),
    mcp_server_id   UUID,
    status          VARCHAR(20) NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'filled', 'partial', 'cancelled', 'rejected')),
    executed_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
COMMENT ON TABLE trade_executions IS 'Actual trade execution records sent to brokers or simulated';

CREATE INDEX idx_trade_executions_fund_id ON trade_executions (fund_id);
CREATE INDEX idx_trade_executions_symbol  ON trade_executions (symbol);
CREATE INDEX idx_trade_executions_status  ON trade_executions (status);
CREATE INDEX idx_trade_executions_fund_created ON trade_executions (fund_id, created_at DESC);

CREATE TABLE holding_positions (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    fund_id       UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    symbol        VARCHAR(20) NOT NULL,
    name          VARCHAR(128),
    quantity      NUMERIC(16, 4) NOT NULL DEFAULT 0,
    available_qty NUMERIC(16, 4) NOT NULL DEFAULT 0,
    cost_price    NUMERIC(16, 4) NOT NULL DEFAULT 0,
    current_price NUMERIC(16, 4) NOT NULL DEFAULT 0,
    market_value  NUMERIC(20, 4) NOT NULL DEFAULT 0,
    weight        NUMERIC(8, 6) NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (fund_id, symbol)
);
COMMENT ON TABLE holding_positions IS 'Current portfolio positions per fund';

CREATE INDEX idx_holding_positions_fund_id ON holding_positions (fund_id);
CREATE INDEX idx_holding_positions_symbol  ON holding_positions (symbol);

CREATE TABLE nav_snapshots (
    id                 UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    fund_id            UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    trading_date       DATE NOT NULL,
    nav                NUMERIC(20, 6) NOT NULL,
    total_assets       NUMERIC(20, 4) NOT NULL DEFAULT 0,
    total_market_value NUMERIC(20, 4) NOT NULL DEFAULT 0,
    available_cash     NUMERIC(20, 4) NOT NULL DEFAULT 0,
    daily_return       NUMERIC(12, 6) NOT NULL DEFAULT 0,
    total_return       NUMERIC(12, 6) NOT NULL DEFAULT 0,
    positions_snapshot JSONB NOT NULL DEFAULT '[]',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (fund_id, trading_date)
);
COMMENT ON TABLE nav_snapshots IS 'Daily NAV and portfolio snapshot for performance tracking';

CREATE INDEX idx_nav_snapshots_fund_id      ON nav_snapshots (fund_id);
CREATE INDEX idx_nav_snapshots_trading_date ON nav_snapshots (trading_date);
CREATE INDEX idx_nav_snapshots_fund_trading_date ON nav_snapshots (fund_id, trading_date);

-- ----------------------------------------------------------------------------
-- 5. Roundtable
-- ----------------------------------------------------------------------------

CREATE TABLE roundtables (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    fund_id      UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    trading_date DATE NOT NULL,
    status       VARCHAR(20) NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active', 'completed', 'timeout')),
    consensus    JSONB,
    started_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ended_at     TIMESTAMPTZ
);
COMMENT ON TABLE roundtables IS 'Multi-agent roundtable discussion sessions';

CREATE INDEX idx_roundtables_fund_id      ON roundtables (fund_id);
CREATE INDEX idx_roundtables_trading_date ON roundtables (trading_date);
CREATE INDEX idx_roundtables_status       ON roundtables (status);

CREATE TABLE roundtable_rounds (
    id             UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    roundtable_id  UUID NOT NULL REFERENCES roundtables(id) ON DELETE CASCADE,
    round_number   INT NOT NULL,
    summary        TEXT,
    unresolved     TEXT[],
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (roundtable_id, round_number)
);
COMMENT ON TABLE roundtable_rounds IS 'Individual discussion rounds within a roundtable session';

CREATE INDEX idx_roundtable_rounds_roundtable_id ON roundtable_rounds (roundtable_id);

CREATE TABLE roundtable_opinions (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    round_id    UUID NOT NULL REFERENCES roundtable_rounds(id) ON DELETE CASCADE,
    agent_id    UUID NOT NULL,
    agent_name  VARCHAR(255),
    focus       VARCHAR(30),
    symbol      VARCHAR(20),
    direction   VARCHAR(10) NOT NULL
                    CHECK (direction IN ('bullish', 'bearish', 'neutral')),
    confidence  NUMERIC(5, 4),
    reasoning   TEXT,
    data_points TEXT[],
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
COMMENT ON TABLE roundtable_opinions IS 'Agent opinions expressed during a roundtable round';

CREATE INDEX idx_roundtable_opinions_round_id ON roundtable_opinions (round_id);
CREATE INDEX idx_roundtable_opinions_agent_id ON roundtable_opinions (agent_id);
CREATE INDEX idx_roundtable_opinions_symbol   ON roundtable_opinions (symbol);

-- ----------------------------------------------------------------------------
-- 6. A/B Tests
-- ----------------------------------------------------------------------------

CREATE TABLE ab_tests (
    id                UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name              VARCHAR(255) NOT NULL,
    control_fund_id   UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    treatment_fund_id UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    variable_type     VARCHAR(64) NOT NULL,
    variable_config   JSONB NOT NULL DEFAULT '{}',
    status            VARCHAR(20) NOT NULL DEFAULT 'draft'
                        CHECK (status IN ('draft', 'running', 'completed', 'analyzed')),
    start_date        DATE,
    end_date          DATE,
    results           JSONB,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
COMMENT ON TABLE ab_tests IS 'A/B test definitions comparing two fund configurations';

CREATE INDEX idx_ab_tests_status ON ab_tests (status);

-- ----------------------------------------------------------------------------
-- 7. Memory
-- ----------------------------------------------------------------------------

CREATE TABLE memories (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    fund_id      UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    agent_id     UUID REFERENCES agents(id) ON DELETE SET NULL,
    layer        VARCHAR(20) NOT NULL
                    CHECK (layer IN ('long_term', 'daily', 'dreams', 'agent')),
    title        VARCHAR(512),
    content      TEXT NOT NULL,
    trading_date DATE,
    tags         TEXT[],
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
COMMENT ON TABLE memories IS 'Agent memory store supporting long-term, daily, dream, and per-agent layers';

CREATE INDEX idx_memories_fund_id      ON memories (fund_id);
CREATE INDEX idx_memories_agent_id     ON memories (agent_id);
CREATE INDEX idx_memories_layer        ON memories (layer);
CREATE INDEX idx_memories_trading_date ON memories (trading_date);
CREATE INDEX idx_memories_tags         ON memories USING GIN (tags);

-- ----------------------------------------------------------------------------
-- 8. Risk Rules
-- ----------------------------------------------------------------------------

CREATE TABLE risk_rules (
    id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    fund_id    UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    name       VARCHAR(255) NOT NULL,
    rule_type  VARCHAR(64) NOT NULL,
    threshold  NUMERIC(16, 6),
    enabled    BOOLEAN NOT NULL DEFAULT TRUE,
    config     JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
COMMENT ON TABLE risk_rules IS 'Configurable risk management rules per fund';

CREATE INDEX idx_risk_rules_fund_id ON risk_rules (fund_id);

-- ----------------------------------------------------------------------------
-- 9. MCP Servers
-- ----------------------------------------------------------------------------

CREATE TABLE mcp_servers (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name             VARCHAR(255) NOT NULL,
    server_type      VARCHAR(10) NOT NULL
                        CHECK (server_type IN ('stdio', 'sse', 'http')),
    endpoint         VARCHAR(512),
    docker_image     VARCHAR(512),
    config           JSONB NOT NULL DEFAULT '{}',
    status           VARCHAR(20) NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active', 'inactive')),
    health_check_url VARCHAR(512),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
COMMENT ON TABLE mcp_servers IS 'Model Context Protocol server registry';

CREATE INDEX idx_mcp_servers_status ON mcp_servers (status);

-- ----------------------------------------------------------------------------
-- 10. Skills
-- ----------------------------------------------------------------------------

CREATE TABLE skills (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name            VARCHAR(255) NOT NULL,
    skill_type      VARCHAR(20) NOT NULL
                        CHECK (skill_type IN ('builtin', 'custom', 'mcp', 'generated')),
    description     TEXT,
    config          JSONB NOT NULL DEFAULT '{}',
    mcp_server_id   UUID REFERENCES mcp_servers(id) ON DELETE SET NULL,
    performance     JSONB NOT NULL DEFAULT '{}',
    approval_status VARCHAR(20) NOT NULL DEFAULT 'pending'
                        CHECK (approval_status IN ('pending', 'approved', 'rejected')),
    source          VARCHAR(20) NOT NULL DEFAULT 'manual'
                        CHECK (source IN ('manual', 'evolved', 'imported')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
COMMENT ON TABLE skills IS 'Skill catalogue including built-in, MCP-backed, and AI-evolved skills';

CREATE INDEX idx_skills_skill_type      ON skills (skill_type);
CREATE INDEX idx_skills_approval_status ON skills (approval_status);

CREATE TABLE agent_skills (
    agent_id UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    skill_id UUID NOT NULL REFERENCES skills(id) ON DELETE CASCADE,
    enabled  BOOLEAN NOT NULL DEFAULT TRUE,
    priority INT NOT NULL DEFAULT 0,
    PRIMARY KEY (agent_id, skill_id)
);
COMMENT ON TABLE agent_skills IS 'Agent-to-skill assignments with priority ordering';

CREATE INDEX idx_agent_skills_skill_id ON agent_skills (skill_id);

-- ----------------------------------------------------------------------------
-- 11. Workflow State
-- ----------------------------------------------------------------------------

CREATE TABLE workflow_runs (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    fund_id      UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    trading_date DATE NOT NULL,
    status       VARCHAR(20) NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'running', 'completed', 'failed', 'cancelled')),
    current_step VARCHAR(64),
    step_results JSONB NOT NULL DEFAULT '{}',
    started_at   TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
COMMENT ON TABLE workflow_runs IS 'Daily workflow execution state per fund';

CREATE INDEX idx_workflow_runs_fund_id      ON workflow_runs (fund_id);
CREATE INDEX idx_workflow_runs_trading_date ON workflow_runs (trading_date);
CREATE INDEX idx_workflow_runs_status       ON workflow_runs (status);

-- ============================================================================
-- Deferred foreign keys (cross-references between tables created earlier)
-- ============================================================================

ALTER TABLE investment_plans
    ADD CONSTRAINT fk_investment_plans_roundtable
        FOREIGN KEY (roundtable_id) REFERENCES roundtables(id) ON DELETE SET NULL;

ALTER TABLE investment_plans
    ADD CONSTRAINT fk_investment_plans_pm_agent
        FOREIGN KEY (pm_agent_id) REFERENCES agents(id) ON DELETE SET NULL;

ALTER TABLE trade_executions
    ADD CONSTRAINT fk_trade_executions_mcp_server
        FOREIGN KEY (mcp_server_id) REFERENCES mcp_servers(id) ON DELETE SET NULL;

-- ============================================================================
-- DOWN Migration
-- ============================================================================
-- To roll back, execute everything below this line.
-- DROP statements in reverse dependency order.
-- ============================================================================

-- DOWN:
-- ALTER TABLE trade_executions DROP CONSTRAINT IF EXISTS fk_trade_executions_mcp_server;
-- ALTER TABLE investment_plans DROP CONSTRAINT IF EXISTS fk_investment_plans_pm_agent;
-- ALTER TABLE investment_plans DROP CONSTRAINT IF EXISTS fk_investment_plans_roundtable;
-- DROP TABLE IF EXISTS workflow_runs;
-- DROP TABLE IF EXISTS agent_skills;
-- DROP TABLE IF EXISTS skills;
-- DROP TABLE IF EXISTS mcp_servers;
-- DROP TABLE IF EXISTS risk_rules;
-- DROP TABLE IF EXISTS memories;
-- DROP TABLE IF EXISTS ab_tests;
-- DROP TABLE IF EXISTS roundtable_opinions;
-- DROP TABLE IF EXISTS roundtable_rounds;
-- DROP TABLE IF EXISTS roundtables;
-- DROP TABLE IF EXISTS nav_snapshots;
-- DROP TABLE IF EXISTS holding_positions;
-- DROP TABLE IF EXISTS trade_executions;
-- DROP TABLE IF EXISTS plan_actions;
-- DROP TABLE IF EXISTS investment_plans;
-- DROP TABLE IF EXISTS fund_team_members;
-- DROP TABLE IF EXISTS agents;
-- DROP TABLE IF EXISTS funds;
-- DROP TABLE IF EXISTS fund_companies;
-- DROP EXTENSION IF EXISTS "uuid-ossp";
