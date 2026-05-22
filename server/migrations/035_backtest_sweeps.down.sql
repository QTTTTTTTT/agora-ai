-- Reverses 035_backtest_sweeps.sql. Drop child link first, then
-- the header table. ON DELETE CASCADE on funds ensures any
-- residual sweep rows for a removed fund are already gone.

DROP INDEX IF EXISTS idx_backtest_jobs_sweep;

ALTER TABLE backtest_jobs
    DROP COLUMN IF EXISTS sweep_cell;

ALTER TABLE backtest_jobs
    DROP COLUMN IF EXISTS sweep_id;

DROP INDEX IF EXISTS idx_backtest_sweeps_fund_created;

DROP TABLE IF EXISTS backtest_sweeps;
