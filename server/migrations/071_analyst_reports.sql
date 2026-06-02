-- 071_analyst_reports.sql — persistence layer for the S8.1
-- specialised analyst quartet (fundamentals / sentiment / news /
-- technical) and the panel-level aggregate they roll up into.
--
-- Two tables:
--   analyst_reports         one row per (panel run, analyst category)
--   analyst_panel_reports   one row per panel run
--
-- panel_reports is the parent; analyst_reports rows reference it
-- via panel_id. Both carry fund_id + symbol redundantly so a fund-
-- scope query doesn't need a join.

CREATE TABLE IF NOT EXISTS analyst_panel_reports (
    id              UUID NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    fund_id         UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    symbol          TEXT NOT NULL,
    asof            TIMESTAMPTZ NOT NULL,
    generated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    aggregate_direction TEXT NOT NULL
        CHECK (aggregate_direction IN ('bullish', 'bearish', 'neutral')),
    aggregate_confidence INT NOT NULL
        CHECK (aggregate_confidence BETWEEN 0 AND 100),
    categories_voted INT NOT NULL DEFAULT 0
        CHECK (categories_voted >= 0),
    per_category_votes JSONB NOT NULL DEFAULT '{}'::jsonb,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_panel_reports_fund_asof
    ON analyst_panel_reports (fund_id, asof DESC);
CREATE INDEX IF NOT EXISTS idx_panel_reports_fund_symbol_asof
    ON analyst_panel_reports (fund_id, symbol, asof DESC);

CREATE TABLE IF NOT EXISTS analyst_reports (
    id              UUID NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    panel_id        UUID NOT NULL REFERENCES analyst_panel_reports(id) ON DELETE CASCADE,
    fund_id         UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    agent_id        TEXT NOT NULL,
    agent_name      TEXT NOT NULL,
    category        TEXT NOT NULL
        CHECK (category IN ('fundamentals', 'sentiment', 'news', 'technical')),
    symbol          TEXT NOT NULL,
    asof            TIMESTAMPTZ NOT NULL,
    generated_at    TIMESTAMPTZ NOT NULL,

    direction       TEXT NOT NULL
        CHECK (direction IN ('bullish', 'bearish', 'neutral')),
    confidence      INT NOT NULL
        CHECK (confidence BETWEEN 0 AND 100),
    thesis          TEXT NOT NULL,
    key_findings    JSONB NOT NULL DEFAULT '[]'::jsonb,
    risks           JSONB NOT NULL DEFAULT '[]'::jsonb,
    data_points     JSONB NOT NULL DEFAULT '[]'::jsonb,
    sources         JSONB NOT NULL DEFAULT '[]'::jsonb,
    prompt_tokens   INT NOT NULL DEFAULT 0,
    completion_tokens INT NOT NULL DEFAULT 0,
    llm_model       TEXT NOT NULL DEFAULT '',

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- One report per (panel, category). A category can't be
    -- duplicated inside a single panel run.
    UNIQUE (panel_id, category)
);

CREATE INDEX IF NOT EXISTS idx_analyst_reports_fund_asof
    ON analyst_reports (fund_id, asof DESC);
CREATE INDEX IF NOT EXISTS idx_analyst_reports_fund_symbol_category
    ON analyst_reports (fund_id, symbol, category, asof DESC);
CREATE INDEX IF NOT EXISTS idx_analyst_reports_agent
    ON analyst_reports (agent_id, asof DESC);
