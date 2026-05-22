-- 031_slippage_and_quote_refresh.sql
--
-- Track the execution-time slippage between the plan's reference price
-- and the actual fill price, plus the timestamp of the most recent
-- quote refresh applied to each plan action.
--
-- Context: Prior to this migration the trading engine executed every
-- approved plan at the price embedded in the PlanAction (i.e. the
-- quote captured at plan-generation time). For a user who approved a
-- plan minutes or hours after it was generated, the system would
-- silently fill at a stale reference price, producing simulation
-- results that didn't match real-market drift.
--
-- Two mechanisms ride on these columns:
--   1. SlippageGuard (risk policy) pulls a fresh quote at execution
--      and compares it to plan_actions.price; if the drift is within
--      a board-aware tolerance, the trade is rewritten to use the live
--      price and the realised slippage is recorded in
--      trade_executions.slippage_pct. Drift beyond the tolerance
--      bounces the plan back to pending_user instead of filling at
--      stale prices.
--   2. RefreshPlanQuote (POST /api/plans/{id}/refresh-quote) becomes
--      a full re-pricing operation that stamps quote_refreshed_at on
--      each affected action so the UI can show a "last refreshed N
--      min ago" badge before the user re-approves.

ALTER TABLE plan_actions
    ADD COLUMN IF NOT EXISTS quote_refreshed_at TIMESTAMPTZ;

COMMENT ON COLUMN plan_actions.quote_refreshed_at IS
    'Timestamp of the most recent quote refresh applied to this action via POST /api/plans/{id}/refresh-quote. NULL means the price still reflects the original plan-generation quote.';

ALTER TABLE trade_executions
    ADD COLUMN IF NOT EXISTS slippage_pct NUMERIC(10, 8);

COMMENT ON COLUMN trade_executions.slippage_pct IS
    'Signed fractional slippage between the plan reference price and the actual fill price: (filled_price - price) / price. NULL for executions predating the SlippageGuard rollout or for non-priced fills (e.g. sell-all of an odd lot). Used for analytics and SlippageGuard tuning.';

-- Analytics index: surface trades with material slippage in a window.
-- ABS-based expression so the same index serves positive/negative
-- drift queries. Partial index keeps the size proportional to actual
-- slippage-recorded rows.
CREATE INDEX IF NOT EXISTS idx_trade_executions_slippage_abs
    ON trade_executions (fund_id, ABS(slippage_pct))
    WHERE slippage_pct IS NOT NULL;
