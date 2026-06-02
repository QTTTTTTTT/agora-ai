-- Sprint 9.2 — workflow checkpoints
--
-- The daily orchestrator already records per-step results in
-- workflow_runs.step_results (a single JSONB blob per run). That
-- blob is convenient for the activity stream but useless for the
-- Sprint 9.2 use-case: when an operator wants to resume a paused
-- or failed run from an arbitrary node, they need (a) a stable
-- per-step row they can point a resume API at, and (b) a payload
-- snapshot capturing the small handful of identifiers / counts the
-- next step actually needs (plan_id, report counts, etc.). One
-- giant JSONB blob doesn't give you either — rows do.
--
-- The unique constraint on (run_id, step) means every wave of
-- check-pointing is idempotent: the orchestrator writes the same
-- step a second time when a retry succeeds and the existing row
-- is replaced.
CREATE TABLE workflow_checkpoints (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    run_id       UUID NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    fund_id      UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    trading_date DATE NOT NULL,
    step         VARCHAR(64) NOT NULL,
    status       VARCHAR(20) NOT NULL
                    CHECK (status IN ('success','failed','skipped','pending','paused')),
    attempts     INTEGER NOT NULL DEFAULT 1 CHECK (attempts >= 1),
    started_at   TIMESTAMPTZ NOT NULL,
    ended_at     TIMESTAMPTZ NOT NULL,
    duration_ms  BIGINT      NOT NULL DEFAULT 0,
    error_text   TEXT,
    payload      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (run_id, step)
);

COMMENT ON TABLE workflow_checkpoints IS
    'Per-step snapshots of the daily workflow. One row per (run_id, step); the orchestrator upserts after every runStep call so the row reflects the latest attempt.';

COMMENT ON COLUMN workflow_checkpoints.payload IS
    'Small structured payload capturing the identifiers / counts the next step needs to resume from this checkpoint (plan_id, report counts, etc.). Bounded — never store full LLM responses here.';

CREATE INDEX idx_workflow_checkpoints_run_id ON workflow_checkpoints (run_id);
CREATE INDEX idx_workflow_checkpoints_fund_date
    ON workflow_checkpoints (fund_id, trading_date DESC, step);
CREATE INDEX idx_workflow_checkpoints_status_failed
    ON workflow_checkpoints (status)
    WHERE status IN ('failed','paused');
