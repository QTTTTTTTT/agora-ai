-- 036_backtest_walk_forward.sql
--
-- Phase 2I: persist the per-fold breakdown for walk-forward
-- backtest runs so the per-fold table + chart annotations survive
-- a server restart. The aggregate metrics already live in the
-- denormalised columns on backtest_jobs and don't need duplication
-- here.
--
-- We store the breakdown as a single JSONB blob rather than a
-- relational fold table because (a) it's strictly per-job, (b)
-- the UI consumes it as one structure, and (c) the spec lists at
-- most 12 folds — orders of magnitude smaller than the per-day
-- NAV rows that justify their own table.

ALTER TABLE backtest_jobs
    ADD COLUMN IF NOT EXISTS walk_forward JSONB;
