-- 034_backtest_persistence.down.sql
--
-- Reverses 034_backtest_persistence.sql. Drops the per-day and
-- per-trade child tables first so the FK on the parent doesn't
-- prevent the parent drop.

DROP TABLE IF EXISTS backtest_trade_events;
DROP TABLE IF EXISTS backtest_nav_points;
DROP TABLE IF EXISTS backtest_jobs;
