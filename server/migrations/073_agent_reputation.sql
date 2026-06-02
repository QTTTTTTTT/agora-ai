-- 073_agent_reputation.sql — S8.4 agent-level reputation ledger.
--
-- One row per (fund_id, agent_id) tracks each analyst /
-- advocate / PM agent's rolling performance: how many calls,
-- how often the direction matched a realised positive return,
-- and the latest realised alpha (vs benchmark) attributed to
-- that agent's contribution to a position.
--
-- The ledger lives separately from the analyst_panel_reports
-- and debate_arguments tables so re-running a panel doesn't
-- mutate reputation history. A nightly backfill job
-- (S8.4 — see internal/agentreputation/backfill.go) reads
-- recent Brinson runs + position changes and produces one
-- AgentDecisionOutcome row per (agent, symbol, asof).

CREATE TABLE IF NOT EXISTS agent_reputation_outcomes (
    id              UUID NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    fund_id         UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    agent_id        TEXT NOT NULL,
    agent_name      TEXT NOT NULL DEFAULT '',
    agent_kind      TEXT NOT NULL
        CHECK (agent_kind IN ('analyst', 'advocate', 'pm', 'researcher')),
    category        TEXT NOT NULL DEFAULT '', -- fundamentals/sentiment/news/technical/bull/bear/...
    symbol          TEXT NOT NULL,
    asof            TIMESTAMPTZ NOT NULL,
    direction       TEXT NOT NULL
        CHECK (direction IN ('bullish', 'bearish', 'neutral')),
    confidence      INT NOT NULL CHECK (confidence BETWEEN 0 AND 100),
    realised_return DOUBLE PRECISION NOT NULL DEFAULT 0,  -- 1d/5d/forward fraction
    benchmark_return DOUBLE PRECISION NOT NULL DEFAULT 0,
    alpha           DOUBLE PRECISION NOT NULL DEFAULT 0,  -- realised - benchmark
    horizon_days    INT NOT NULL DEFAULT 1 CHECK (horizon_days > 0),
    source_panel_id UUID REFERENCES analyst_panel_reports(id) ON DELETE SET NULL,
    source_debate_id UUID REFERENCES debate_transcripts(id) ON DELETE SET NULL,
    note            TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (fund_id, agent_id, symbol, asof, horizon_days)
);

CREATE INDEX IF NOT EXISTS idx_agent_rep_outcomes_fund_agent_asof
    ON agent_reputation_outcomes (fund_id, agent_id, asof DESC);
CREATE INDEX IF NOT EXISTS idx_agent_rep_outcomes_fund_kind_asof
    ON agent_reputation_outcomes (fund_id, agent_kind, asof DESC);
CREATE INDEX IF NOT EXISTS idx_agent_rep_outcomes_symbol_asof
    ON agent_reputation_outcomes (symbol, asof DESC);

-- Aggregate summary table — denormalised rolling stats kept
-- up to date by the backfill job. Reads are by (fund, agent).
CREATE TABLE IF NOT EXISTS agent_reputation_stats (
    fund_id              UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    agent_id             TEXT NOT NULL,
    agent_name           TEXT NOT NULL DEFAULT '',
    agent_kind           TEXT NOT NULL,
    category             TEXT NOT NULL DEFAULT '',
    decisions_count      BIGINT NOT NULL DEFAULT 0,
    hits_count           BIGINT NOT NULL DEFAULT 0,  -- direction matched sign of realised
    misses_count         BIGINT NOT NULL DEFAULT 0,
    avg_alpha            DOUBLE PRECISION NOT NULL DEFAULT 0,
    sum_alpha            DOUBLE PRECISION NOT NULL DEFAULT 0,
    avg_confidence       DOUBLE PRECISION NOT NULL DEFAULT 0,
    last_decision_at     TIMESTAMPTZ,
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (fund_id, agent_id)
);

CREATE INDEX IF NOT EXISTS idx_agent_rep_stats_fund_avgalpha
    ON agent_reputation_stats (fund_id, avg_alpha DESC);
