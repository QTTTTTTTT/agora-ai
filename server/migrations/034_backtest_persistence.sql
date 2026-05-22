-- 034_backtest_persistence.sql
--
-- Phase 2F of the auto-execute + decision refactor: persist backtest
-- runs across process restarts so operators don't lose multi-hour
-- replays when the server is redeployed. The in-memory backtest.JobStore
-- stays as the runtime ledger for ACTIVE jobs (queued / running);
-- these tables hold the long-term history and any job in a terminal
-- state.
--
-- Three tables:
--
--   backtest_jobs        — one row per submitted run, denormalised
--                          metrics + summary fields for cheap list
--                          queries. Status mirrors the in-memory
--                          progress.Status enum.
--   backtest_nav_points  — per-day NAV snapshots; ~250 rows per yearly
--                          run. Has the held-positions map as JSONB
--                          so we don't need a child table per symbol.
--   backtest_trade_events — per-execution + per-skip event log. Same
--                          life-cycle as nav_points; child of jobs.
--
-- Recovery: a startup sweep (handled in the adapter, not in SQL)
-- marks any row with status in ('queued','running') as 'failed' with
-- error = 'server restart before completion'. We don't auto-resume —
-- backtest state (Portfolio, lots, etc.) lives entirely in memory
-- and isn't reconstructable from the journaled NAV/Trades streams
-- without significant extra plumbing.
--
-- Why JSONB for request + positions instead of full normalisation:
--   - request is opaque to the DB (no queries against fields)
--   - positions per NAV row averages ≤ 10 keys; a child table would
--     triple the write volume for marginal query benefit

CREATE TABLE IF NOT EXISTS backtest_jobs (
    id              UUID PRIMARY KEY,
    fund_id         UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    user_id         UUID NOT NULL,
    name            TEXT NOT NULL DEFAULT '',
    engine_kind     TEXT NOT NULL DEFAULT 'fallback',
    status          TEXT NOT NULL DEFAULT 'queued',
    request         JSONB NOT NULL,
    error           TEXT,
    -- Window mirror — duplicates fields inside request but lets us
    -- run "show recent runs over the last 30 days" queries without
    -- a JSONB lookup.
    window_start    TIMESTAMPTZ NOT NULL,
    window_end      TIMESTAMPTZ NOT NULL,
    -- Denormalised metrics for cheap list rendering. NULL when the
    -- job hasn't completed yet (or completed with no NAV curve).
    initial_cash    NUMERIC(20, 6),
    final_nav       NUMERIC(20, 6),
    cumulative_return NUMERIC(10, 6),
    annualized_return NUMERIC(10, 6),
    volatility      NUMERIC(10, 6),
    sharpe_ratio    NUMERIC(10, 6),
    max_drawdown    NUMERIC(10, 6),
    win_rate        NUMERIC(10, 6),
    trade_count     INTEGER NOT NULL DEFAULT 0,
    winning_trade_count INTEGER NOT NULL DEFAULT 0,
    losing_trade_count  INTEGER NOT NULL DEFAULT 0,
    -- Progress mirror — written only at terminal transitions plus
    -- the initial submit. Live progress for queued/running jobs
    -- still lives in memory (backtest.Progress).
    total_days      INTEGER NOT NULL DEFAULT 0,
    done_days       INTEGER NOT NULL DEFAULT 0,
    submitted_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    CONSTRAINT backtest_jobs_status_check CHECK (
        status IN ('queued','running','completed','failed','cancelled')
    )
);

CREATE INDEX IF NOT EXISTS idx_backtest_jobs_fund_submitted
    ON backtest_jobs (fund_id, submitted_at DESC);

-- Partial index for the startup sweep: small + hot.
CREATE INDEX IF NOT EXISTS idx_backtest_jobs_active
    ON backtest_jobs (status)
    WHERE status IN ('queued','running');

CREATE TABLE IF NOT EXISTS backtest_nav_points (
    job_id          UUID NOT NULL REFERENCES backtest_jobs(id) ON DELETE CASCADE,
    seq             INTEGER NOT NULL,
    -- Date is the trading-day key from the runner (truncated to
    -- midnight UTC). seq + date together uniquely identify a point
    -- within a job.
    date            TIMESTAMPTZ NOT NULL,
    nav             NUMERIC(20, 6) NOT NULL,
    cash            NUMERIC(20, 6) NOT NULL,
    position_value  NUMERIC(20, 6) NOT NULL DEFAULT 0,
    drawdown_pct    NUMERIC(10, 6) NOT NULL DEFAULT 0,
    -- positions is the symbol → quantity map for that day.
    positions       JSONB NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (job_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_backtest_nav_points_job_date
    ON backtest_nav_points (job_id, date);

CREATE TABLE IF NOT EXISTS backtest_trade_events (
    job_id          UUID NOT NULL REFERENCES backtest_jobs(id) ON DELETE CASCADE,
    seq             INTEGER NOT NULL,
    date            TIMESTAMPTZ NOT NULL,
    symbol          TEXT NOT NULL DEFAULT '',
    action          TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT '',
    quantity        NUMERIC(20, 6) NOT NULL DEFAULT 0,
    fill_price      NUMERIC(20, 6) NOT NULL DEFAULT 0,
    notional        NUMERIC(20, 6) NOT NULL DEFAULT 0,
    reason          TEXT,
    confidence      NUMERIC(20, 6),
    PRIMARY KEY (job_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_backtest_trade_events_job_date
    ON backtest_trade_events (job_id, date);

CREATE INDEX IF NOT EXISTS idx_backtest_trade_events_job_status
    ON backtest_trade_events (job_id, status);

COMMENT ON TABLE backtest_jobs IS
    'Phase 2F: persistent backtest history. The in-memory backtest.JobStore is still the runtime ledger for active jobs; this table holds terminal state plus the initial-submit row for the audit trail.';
COMMENT ON COLUMN backtest_jobs.status IS
    'queued / running / completed / failed / cancelled. queued+running rows from a crashed process are swept to failed on startup.';
COMMENT ON COLUMN backtest_jobs.request IS
    'Original SubmitBacktestInput as JSONB — opaque to the DB. Operators replay a run by re-POSTing this payload.';
COMMENT ON TABLE backtest_nav_points IS
    'Per-day NAV snapshot for one backtest run. Written in bulk when the run reaches a terminal state.';
COMMENT ON TABLE backtest_trade_events IS
    'Per-execution + per-skip event log. Written in bulk when the run reaches a terminal state. Confidence carries the realized P&L delta on sell/reduce events (see runner.recordSellEvent).';
