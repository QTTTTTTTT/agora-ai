-- Sprint 1 / S6: plan_actions.strategy column.
--
-- The TraderAgent module (server/internal/agent/trader.go) has
-- carried StrategyImmediate / StrategyTWAP / StrategyVWAP /
-- StrategyLimit constants since project inception, but the
-- runtimeTradingEngine.executePlanAction path always went through
-- the single-shot "immediate" path. This migration adds the column
-- so the LLM PM (and later, the deterministic strategy sleeves)
-- can request a smarter execution style on big orders and the
-- engine can dispatch accordingly.
--
-- Values produced by the wiring layer:
--   - "immediate" — default, single-shot fill at the live quote.
--   - "twap"      — slice into N=4..8 child fills with a simulated
--                    intra-day spread; per-slice price = day VWAP
--                    × (1 ± random slippage).
--   - "vwap"      — same shape as TWAP today; reserved for the
--                    minute-bar intraday OHLC fetcher (Sprint 5).
--   - "limit"     — reserved for the Sprint 5 limit-order book
--                    simulator (placeholder; treated as immediate
--                    by today's engine until the simulator lands).
--
-- Column is nullable so legacy rows and tests that pre-date this
-- column keep working; the engine treats NULL as "immediate".

ALTER TABLE plan_actions
    ADD COLUMN IF NOT EXISTS strategy TEXT;

COMMENT ON COLUMN plan_actions.strategy IS
  'Sprint 1/S6 execution strategy hint: immediate / twap / vwap / limit. NULL = immediate (legacy).';
