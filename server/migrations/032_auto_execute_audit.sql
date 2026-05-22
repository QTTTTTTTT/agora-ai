-- 032_auto_execute_audit.sql
--
-- Audit trail for fund-level auto-execute. When the per-fund auto-
-- execute toggle is on and the autoExecuteGateCheck guardrails pass,
-- the runtimeApprovalGateway skips the human-in-the-loop and stamps
-- plan_actions.auto_executed_at on every action of that plan as it
-- moves to "approved". Manual approvals leave this column NULL.
--
-- Two consumers rely on this column:
--   1. The daily-cumulative guardrail (autoExecuteGateCheck): before
--      approving the *next* plan automatically the gateway sums
--      amount over plan_actions.auto_executed_at >= today_utc_midnight
--      and refuses to bypass approval once the fund-configured daily
--      cap (default 20% of NAV) is reached. Without this column we
--      can't tell apart "user clicked approve 5 times today" from
--      "the system auto-approved 5 plans today".
--   2. Compliance / replay tooling: the audit log needs to distinguish
--      operator-mediated executions from autonomous ones, regardless
--      of plan.status.
--
-- NULL by default; back-filling is intentionally omitted — historical
-- plans were all human-approved at the time they were written.

ALTER TABLE plan_actions
    ADD COLUMN IF NOT EXISTS auto_executed_at TIMESTAMPTZ;

COMMENT ON COLUMN plan_actions.auto_executed_at IS
    'Timestamp at which the runtimeApprovalGateway auto-approved this action under the fund-level auto-execute toggle. NULL means the action was human-approved (or has not yet been approved). Used by the daily cumulative guardrail and by audit/replay tooling.';

-- Daily-cumulative gate index: scope is "per fund, per UTC day, only
-- auto-executed rows". We index plan_id because the gate computes the
-- sum by joining plan_actions back to investment_plans (fund_id lives
-- there). Partial WHERE keeps the index tiny — most plan_actions stay
-- NULL on this column.
CREATE INDEX IF NOT EXISTS idx_plan_actions_auto_executed_at
    ON plan_actions (auto_executed_at, plan_id)
    WHERE auto_executed_at IS NOT NULL;
