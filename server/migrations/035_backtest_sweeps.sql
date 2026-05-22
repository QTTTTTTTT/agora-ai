-- 035_backtest_sweeps.sql
--
-- Phase 2H: parameter sweeps. A sweep is a 1-N "group" of related
-- backtest jobs that share a Base request and vary on a small set
-- of axes (slippage, commission, maxOrdersPerDay, initialCash,
-- engineKind). Sweeps cap at 25 cells × 2 axes to prevent runaway
-- fan-out; see backtest.MaxSweepCells / MaxSweepAxes.
--
-- Storage:
--   backtest_sweeps  — one row per sweep submission. Stores the
--                      Base request + Axes spec as JSONB, owner +
--                      fund metadata for ACL, total_cells for
--                      cheap "how many children?" rendering.
--   backtest_jobs    — gains sweep_id (nullable FK) + sweep_cell
--                      (JSONB axis → value) so each child knows
--                      which sweep it belongs to and which cell
--                      it represents.
--
-- The sweep is otherwise just metadata: each child remains a
-- normal backtest_job and shows up in ListBacktests as usual.
-- The web UI groups children by sweep_id when rendering.

CREATE TABLE IF NOT EXISTS backtest_sweeps (
    id              UUID PRIMARY KEY,
    fund_id         UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    user_id         UUID NOT NULL,
    name            TEXT NOT NULL DEFAULT '',
    -- base + axes capture the entire sweep spec so we can replay
    -- a sweep verbatim from history.
    base_request    JSONB NOT NULL,
    axes            JSONB NOT NULL,
    -- total_cells is the Cartesian product size at submit time.
    -- Denormalised so the UI can render "5/25 done" without
    -- counting child rows.
    total_cells     INTEGER NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_backtest_sweeps_fund_created
    ON backtest_sweeps (fund_id, created_at DESC);

-- Child link: every job that's part of a sweep points to its
-- parent and carries the axis → value map for cheap grid
-- rendering. Jobs not part of a sweep have sweep_id = NULL.
ALTER TABLE backtest_jobs
    ADD COLUMN IF NOT EXISTS sweep_id    UUID REFERENCES backtest_sweeps(id) ON DELETE SET NULL;

ALTER TABLE backtest_jobs
    ADD COLUMN IF NOT EXISTS sweep_cell  JSONB;

CREATE INDEX IF NOT EXISTS idx_backtest_jobs_sweep
    ON backtest_jobs (sweep_id)
    WHERE sweep_id IS NOT NULL;

COMMENT ON TABLE backtest_sweeps IS
    'Phase 2H: parameter sweep header. Each row is one user submission that fans out into N backtest_jobs (one per axis-value combination).';
COMMENT ON COLUMN backtest_sweeps.base_request IS
    'The template Request as JSONB. Each child job clones this and overrides one or more fields per its sweep_cell.';
COMMENT ON COLUMN backtest_sweeps.axes IS
    'Array of {name, values[]}. Order matters: axes[0] is the row dimension in the grid view, axes[1] the column.';
COMMENT ON COLUMN backtest_jobs.sweep_id IS
    'NULL for one-off backtests; non-NULL when the job was spawned by a sweep submission.';
COMMENT ON COLUMN backtest_jobs.sweep_cell IS
    'Axis name → value string map identifying which cell of the sweep this job covers. NULL when sweep_id is NULL.';
