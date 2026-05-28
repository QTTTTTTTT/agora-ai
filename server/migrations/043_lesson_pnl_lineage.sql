-- Sprint 3 / M1: lesson PnL lineage table.
--
-- Tracks the realized outcome of a learning lesson. Each lesson the
-- distiller writes (memories.layer in ('agent','daily','long_term'))
-- may carry a falsifiable hypothesis like "如果 next week 减 X，则 P&L
-- should improve". The scoring cron evaluates that hypothesis once the
-- prediction window closes and stores the verdict here so the prompt
-- layer can surface "该 lesson 历史命中率" alongside the lesson body.
--
-- Design notes:
--  * lesson_memory_id is NOT a foreign key — memories may be archived
--    by the nightly archive job (M4) and the lineage row must survive
--    so we never lose a historical hit/miss. Tying to a soft id keeps
--    the schema decoupled.
--  * symbol is nullable: portfolio-level lessons ("减仓位") have no
--    single ticker; we still want to score them via fund-level PnL.
--  * predicted_direction uses smallint instead of an enum so we can add
--    "flat" / "volatility" verdicts later without a schema change.
--  * window_close_at and observed_at are separate columns: the cron
--    waits until window_close_at to score, then stamps observed_at when
--    the score actually lands. A NULL observed_at means "due but not
--    yet processed" — convenient for the worker query.

CREATE TABLE IF NOT EXISTS lesson_pnl_lineage (
    id BIGSERIAL PRIMARY KEY,
    lesson_memory_id UUID NOT NULL,
    fund_id UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    agent_id UUID NULL REFERENCES agents(id) ON DELETE SET NULL,
    symbol TEXT NULL,
    hypothesis TEXT NOT NULL,
    predicted_direction SMALLINT NOT NULL DEFAULT 0,
    hypothesis_window_days INTEGER NOT NULL DEFAULT 7
        CHECK (hypothesis_window_days > 0 AND hypothesis_window_days <= 90),
    window_close_at TIMESTAMPTZ NOT NULL,
    observed_at TIMESTAMPTZ NULL,
    observed_pnl NUMERIC(18,6) NULL,
    observed_return_pct NUMERIC(10,4) NULL,
    score NUMERIC(5,3) NULL,
    verdict TEXT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_lesson_pnl_lineage_memory
    ON lesson_pnl_lineage(lesson_memory_id);
CREATE INDEX IF NOT EXISTS idx_lesson_pnl_lineage_fund
    ON lesson_pnl_lineage(fund_id);
CREATE INDEX IF NOT EXISTS idx_lesson_pnl_lineage_due
    ON lesson_pnl_lineage(window_close_at)
    WHERE observed_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_lesson_pnl_lineage_agent
    ON lesson_pnl_lineage(agent_id)
    WHERE agent_id IS NOT NULL;
