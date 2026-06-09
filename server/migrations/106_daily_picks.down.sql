-- Migration: 106_daily_picks (DOWN)
-- Drops the publisher-mode tables in reverse dependency order.
-- The seed row in daily_pick_watchlists falls with its table —
-- explicit DELETE is unnecessary and would race the DROP anyway.

BEGIN;

DROP INDEX IF EXISTS idx_daily_picks_symbol_history;
DROP INDEX IF EXISTS idx_daily_picks_browse;
DROP INDEX IF EXISTS idx_daily_picks_publisher_key;
DROP TABLE IF EXISTS daily_picks;

DROP INDEX IF EXISTS idx_daily_pick_watchlists_active;
DROP TABLE IF EXISTS daily_pick_watchlists;

COMMIT;
