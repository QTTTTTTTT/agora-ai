-- 094_plan_outcome.sql — W1-5: per-plan outcome snapshot.
--
-- WHY THIS EXISTS
-- ---------------
-- Migration 093 captured WHAT shaped a plan (decision_provenance:
-- prompt blocks, lessons used, skills used). To close the
-- self-learning loop we need the matching OUTCOME column —
-- "after window W elapsed, this plan's realized PnL was X, alpha
-- was Y, win rate Z". Without this, the Wave-2 lesson-refute
-- path (#9), skill-effectiveness tracker (#8), and calibration
-- tracker (#7) can't compute "did the lesson actually help?" or
-- "is this agent's confidence calibrated?".
--
-- The outcome is computed asynchronously by a background
-- resolver: at the moment a plan is decided we don't know the
-- next 5 days of NAV. The resolver fires on a schedule (or on
-- demand from an admin endpoint) and fills the column once the
-- window has elapsed for the plan.
--
-- SHAPE
-- -----
-- One JSONB column matching internal/planoutcome.Outcome:
--
--   {
--     "windowKind":     "fixed_5d" | "fixed_10d" | "fixed_20d"
--                       | "next_earnings" | "next_news" | "manual",
--     "windowEndedAt":  RFC-3339 timestamp,
--     "realizedPnL":    float (in fund base currency),
--     "vsBenchmark":    float (excess return vs benchmark, fraction),
--     "alpha":          float (annualised alpha, fraction),
--     "winRate":        float in [0,1] across the actions in the plan,
--     "sampleCount":    int (number of resolved actions),
--     "computedAt":     RFC-3339 timestamp,
--     "computedBy":     "fixed_window_resolver" | "manual" | …,
--     "notes":          string (operator-set when manual)
--   }
--
-- The column is JSONB rather than a flat set of NUMERIC columns
-- because:
--   * we expect to add resolver kinds (event-driven windows for
--     thesis-aware outcome resolution — Wave 3 #16);
--   * the action-level breakdown is variable-length and a
--     side table for it would multiply the join count of the
--     calibration / refute queries.

ALTER TABLE investment_plans
    ADD COLUMN IF NOT EXISTS plan_outcome JSONB;

-- Partial index for the resolver worker: it scans the rows where
-- plan_outcome IS NULL AND created_at < now() - window so it knows
-- which plans need resolution. Using a partial index keeps the
-- index size tiny (only "pending" rows).
CREATE INDEX IF NOT EXISTS idx_investment_plans_pending_outcome
    ON investment_plans (created_at)
    WHERE plan_outcome IS NULL;

-- Read-side index for the Wave-2 trackers: they pivot on
-- plan_outcome->>'windowKind' to filter only plans whose outcome
-- was resolved by a comparable resolver kind. JSONB function index.
CREATE INDEX IF NOT EXISTS idx_investment_plans_outcome_window_kind
    ON investment_plans ((plan_outcome ->> 'windowKind'))
    WHERE plan_outcome IS NOT NULL;

COMMENT ON COLUMN investment_plans.plan_outcome IS
    'JSONB outcome snapshot. Shape mirrors internal/planoutcome.Outcome: windowKind, windowEndedAt, realizedPnL, vsBenchmark, alpha, winRate, sampleCount, computedAt, computedBy, notes. NULL means the resolver has not run yet for this plan.';
