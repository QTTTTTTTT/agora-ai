-- A/B strategy comparison shadow data foundation.
-- These tables keep variant-level simulated NAV/trade output separate from
-- real fund portfolio state so future strategy A/B runs do not pollute live
-- fund trades, positions, or NAV snapshots.

CREATE TABLE IF NOT EXISTS ab_test_variants (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    test_id             UUID NOT NULL REFERENCES ab_tests(id) ON DELETE CASCADE,
    variant_key         VARCHAR(8) NOT NULL CHECK (variant_key IN ('A', 'B')),
    name                VARCHAR(255) NOT NULL,
    strategy_config     JSONB NOT NULL DEFAULT '{}',
    team_snapshot       JSONB NOT NULL DEFAULT '{}',
    initial_cash        NUMERIC(20, 4) NOT NULL DEFAULT 0,
    initial_positions   JSONB NOT NULL DEFAULT '[]',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (test_id, variant_key)
);

CREATE TABLE IF NOT EXISTS ab_test_variant_nav (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    test_id             UUID NOT NULL REFERENCES ab_tests(id) ON DELETE CASCADE,
    variant_id          UUID NOT NULL REFERENCES ab_test_variants(id) ON DELETE CASCADE,
    trading_date        DATE NOT NULL,
    nav                 NUMERIC(20, 8) NOT NULL,
    total_assets        NUMERIC(20, 4) NOT NULL DEFAULT 0,
    cash                NUMERIC(20, 4) NOT NULL DEFAULT 0,
    daily_return        NUMERIC(12, 8) NOT NULL DEFAULT 0,
    cumulative_return   NUMERIC(12, 8) NOT NULL DEFAULT 0,
    drawdown            NUMERIC(12, 8) NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (variant_id, trading_date)
);

CREATE TABLE IF NOT EXISTS ab_test_variant_trades (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    test_id             UUID NOT NULL REFERENCES ab_tests(id) ON DELETE CASCADE,
    variant_id          UUID NOT NULL REFERENCES ab_test_variants(id) ON DELETE CASCADE,
    trading_date        DATE NOT NULL,
    symbol              VARCHAR(32) NOT NULL,
    side                VARCHAR(16) NOT NULL,
    quantity            NUMERIC(20, 8) NOT NULL DEFAULT 0,
    price               NUMERIC(20, 8) NOT NULL DEFAULT 0,
    notional            NUMERIC(20, 4) NOT NULL DEFAULT 0,
    realized_pnl        NUMERIC(20, 4) NOT NULL DEFAULT 0,
    reasoning           TEXT,
    source_plan_id      UUID,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS ab_test_decision_diffs (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    test_id             UUID NOT NULL REFERENCES ab_tests(id) ON DELETE CASCADE,
    trading_date        DATE NOT NULL,
    symbol              VARCHAR(32) NOT NULL,
    variant_a_action    TEXT,
    variant_b_action    TEXT,
    return_impact       NUMERIC(12, 8) NOT NULL DEFAULT 0,
    explanation         TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS ab_test_variant_memory (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    test_id             UUID NOT NULL REFERENCES ab_tests(id) ON DELETE CASCADE,
    variant_id          UUID NOT NULL REFERENCES ab_test_variants(id) ON DELETE CASCADE,
    agent_id            UUID,
    memory_key          VARCHAR(255) NOT NULL,
    layer               VARCHAR(64) NOT NULL DEFAULT 'shadow',
    content             JSONB NOT NULL DEFAULT '{}',
    trading_date        DATE,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (variant_id, agent_id, memory_key)
);

CREATE TABLE IF NOT EXISTS ab_test_agent_learning_events (
    id                          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    test_id                     UUID NOT NULL REFERENCES ab_tests(id) ON DELETE CASCADE,
    variant_id                  UUID NOT NULL REFERENCES ab_test_variants(id) ON DELETE CASCADE,
    agent_id                    UUID NOT NULL,
    trading_date                DATE NOT NULL,
    summary                     TEXT,
    lessons                     JSONB NOT NULL DEFAULT '[]',
    adjustments                 JSONB NOT NULL DEFAULT '[]',
    specialization_learning     JSONB NOT NULL DEFAULT '{}',
    proposed_evolution_config   JSONB NOT NULL DEFAULT '{}',
    source_memory_id            UUID REFERENCES ab_test_variant_memory(id) ON DELETE SET NULL,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (variant_id, agent_id, trading_date)
);

CREATE TABLE IF NOT EXISTS ab_test_learning_promotions (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    test_id             UUID NOT NULL REFERENCES ab_tests(id) ON DELETE CASCADE,
    variant_id          UUID NOT NULL REFERENCES ab_test_variants(id) ON DELETE CASCADE,
    agent_id            UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    mode                VARCHAR(32) NOT NULL CHECK (mode IN ('merge', 'overwrite')),
    previous_config     JSONB NOT NULL DEFAULT '{}',
    promoted_config     JSONB NOT NULL DEFAULT '{}',
    promoted_by         UUID,
    promoted_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_ab_test_variants_test_id ON ab_test_variants(test_id);
CREATE INDEX IF NOT EXISTS idx_ab_test_variant_nav_test_date ON ab_test_variant_nav(test_id, trading_date);
CREATE INDEX IF NOT EXISTS idx_ab_test_variant_trades_test_date ON ab_test_variant_trades(test_id, trading_date);
CREATE INDEX IF NOT EXISTS idx_ab_test_decision_diffs_test_date ON ab_test_decision_diffs(test_id, trading_date);
CREATE INDEX IF NOT EXISTS idx_ab_test_variant_memory_agent ON ab_test_variant_memory(test_id, variant_id, agent_id);
CREATE INDEX IF NOT EXISTS idx_ab_test_agent_learning_events_agent ON ab_test_agent_learning_events(test_id, variant_id, agent_id, trading_date);
CREATE INDEX IF NOT EXISTS idx_ab_test_learning_promotions_test_agent ON ab_test_learning_promotions(test_id, agent_id, promoted_at);
