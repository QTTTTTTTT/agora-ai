-- 072_debates.sql — persistence for the S8.2 Bull/Bear debate
-- transcripts.
--
-- Two tables mirror the analyst panel layout (072 follows 071):
--   debate_transcripts: one row per debate run (per symbol per
--                       panel)
--   debate_arguments:    one row per (transcript, round, stance)
--
-- Each transcript is anchored to the AnalystPanel that triggered
-- it via panel_id (FK with CASCADE) so deleting a panel cleans
-- up the debate.

CREATE TABLE IF NOT EXISTS debate_transcripts (
    id              UUID NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    fund_id         UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    panel_id        UUID NOT NULL REFERENCES analyst_panel_reports(id) ON DELETE CASCADE,
    symbol          TEXT NOT NULL,
    asof            TIMESTAMPTZ NOT NULL,
    generated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    verdict_direction TEXT NOT NULL
        CHECK (verdict_direction IN ('bullish', 'bearish', 'neutral')),
    verdict_confidence INT NOT NULL
        CHECK (verdict_confidence BETWEEN 0 AND 100),
    verdict_winner TEXT NOT NULL DEFAULT ''
        CHECK (verdict_winner IN ('', 'bull', 'bear')),
    verdict_bull_confidence INT NOT NULL DEFAULT 0,
    verdict_bear_confidence INT NOT NULL DEFAULT 0,
    verdict_contested BOOLEAN NOT NULL DEFAULT FALSE,
    verdict_winning_summary TEXT NOT NULL DEFAULT '',
    verdict_losing_summary  TEXT NOT NULL DEFAULT '',

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_debate_transcripts_fund_asof
    ON debate_transcripts (fund_id, asof DESC);
CREATE INDEX IF NOT EXISTS idx_debate_transcripts_fund_symbol_asof
    ON debate_transcripts (fund_id, symbol, asof DESC);
CREATE INDEX IF NOT EXISTS idx_debate_transcripts_panel
    ON debate_transcripts (panel_id);

CREATE TABLE IF NOT EXISTS debate_arguments (
    id              UUID NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    transcript_id   UUID NOT NULL REFERENCES debate_transcripts(id) ON DELETE CASCADE,
    fund_id         UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    agent_id        TEXT NOT NULL,
    agent_name      TEXT NOT NULL,
    stance          TEXT NOT NULL
        CHECK (stance IN ('bull', 'bear')),
    symbol          TEXT NOT NULL,
    round_number    INT NOT NULL CHECK (round_number >= 1),
    asof            TIMESTAMPTZ NOT NULL,
    generated_at    TIMESTAMPTZ NOT NULL,

    direction       TEXT NOT NULL
        CHECK (direction IN ('bullish', 'bearish')),
    confidence      INT NOT NULL
        CHECK (confidence BETWEEN 0 AND 100),
    thesis          TEXT NOT NULL,
    support_points  JSONB NOT NULL DEFAULT '[]'::jsonb,
    rebuttals       JSONB NOT NULL DEFAULT '[]'::jsonb,
    cited_reports   JSONB NOT NULL DEFAULT '[]'::jsonb,
    llm_model       TEXT NOT NULL DEFAULT '',

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (transcript_id, round_number, stance)
);

CREATE INDEX IF NOT EXISTS idx_debate_arguments_transcript_round
    ON debate_arguments (transcript_id, round_number, stance);
CREATE INDEX IF NOT EXISTS idx_debate_arguments_fund_asof
    ON debate_arguments (fund_id, asof DESC);
CREATE INDEX IF NOT EXISTS idx_debate_arguments_agent
    ON debate_arguments (agent_id, asof DESC);
