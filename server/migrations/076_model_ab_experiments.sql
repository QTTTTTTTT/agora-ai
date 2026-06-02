-- 076_model_ab_experiments.sql
--
-- Sprint 10.1 — model-level A/B experiments.
--
-- This file introduces three tables that together let operators run
-- "same fund, same prompt, different LLM" experiments WITHOUT cloning
-- the underlying fund (the existing strategy A/B in 022_abtest_*
-- requires a full fund clone, which is too heavy for model-only
-- comparisons).
--
-- Tables
--   model_ab_experiments     : the experiment definition (scope, arms, split)
--   model_ab_assignments     : per-(run_id, step, agent) → arm sticky binding
--   model_ab_shadow_responses: B-arm and onwards responses (only A-arm
--                              actually steers the production decision)
--
-- The shadow_responses table is intentionally append-only and capped by
-- a TTL (defaults to 60 days via the operator-run cleanup job) because
-- it grows linearly with #steps × #arms × #experiments.

CREATE TABLE IF NOT EXISTS model_ab_experiments (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          TEXT NOT NULL,
    description   TEXT,

    -- Scope: which calls does this experiment apply to?
    --   'global'   : every fund, every agent
    --   'fund'     : only this fund (scope_target = fund_id)
    --   'agent_role': all agents of this role (scope_target = role name)
    --   'agent_id' : a specific agent (scope_target = agent_id)
    scope         TEXT NOT NULL CHECK (scope IN ('global','fund','agent_role','agent_id')),
    scope_target  TEXT,  -- nullable for 'global'

    -- Step filter: limit to specific workflow steps (pm_decision,
    -- debate, analyst_panel, ...). Empty array = match every step
    -- the request flows through.
    step_filter   TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[],

    -- Arms: ordered list, position 0 is the control arm. Each arm
    -- carries a model config JSON the router applies when the bucket
    -- hashes into that arm. Schema for each arm element:
    --   {
    --     "name": "control",
    --     "provider": "openai",
    --     "model_name": "gpt-4o",
    --     "base_url": "https://api.openai.com/v1",
    --     "model_tier": "critical",
    --     "temperature": 0.1
    --   }
    -- API key is NOT stored here; the router falls back to the
    -- experiment owner's user_provider_keys / system keys at
    -- resolution time, just like normal traffic.
    arms          JSONB NOT NULL,

    -- Traffic split: probabilities summing to 1.0 (or close to it).
    -- Position i is the weight for arms[i]. Length must equal arms.
    traffic_split DOUBLE PRECISION[] NOT NULL,

    -- Lifecycle
    status        TEXT NOT NULL DEFAULT 'draft'
                    CHECK (status IN ('draft','running','paused','completed','archived')),
    start_at      TIMESTAMP WITH TIME ZONE,
    end_at        TIMESTAMP WITH TIME ZONE,

    -- Cost guard: stop dispatching shadow arms once cumulative
    -- output tokens cross this number. NULL = no cap.
    max_total_tokens BIGINT,

    -- Telemetry: total tokens accumulated since start. Updated by
    -- the shadow dispatcher; the cap above is enforced lazily on
    -- every call (NOT atomically — over-spend by 1-2 calls is
    -- acceptable, we trade accuracy for hot-path latency).
    tokens_used   BIGINT NOT NULL DEFAULT 0,

    created_by    UUID,
    created_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_model_ab_experiments_status_scope
    ON model_ab_experiments (status, scope, scope_target);
CREATE INDEX IF NOT EXISTS idx_model_ab_experiments_created_at
    ON model_ab_experiments (created_at DESC);

-- model_ab_assignments records the arm decision for a given
-- (experiment, workflow_run, step, agent) tuple. The router is
-- expected to look this row up on every call and use the arm
-- it returns; if the row doesn't exist yet, the router computes
-- the bucket via the deterministic hash AND writes the row, so
-- subsequent calls inside the same workflow run see the same arm.
-- This is the sticky-arm guarantee that keeps a single
-- workflow_run from straddling multiple models mid-stream.
CREATE TABLE IF NOT EXISTS model_ab_assignments (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    experiment_id UUID NOT NULL REFERENCES model_ab_experiments(id) ON DELETE CASCADE,
    run_id        TEXT NOT NULL,
    step          TEXT NOT NULL,
    agent_id      TEXT,
    fund_id       TEXT,
    arm_index     INTEGER NOT NULL,
    arm_name      TEXT NOT NULL,
    assigned_at   TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),

    UNIQUE (experiment_id, run_id, step, agent_id)
);

CREATE INDEX IF NOT EXISTS idx_model_ab_assignments_experiment
    ON model_ab_assignments (experiment_id, assigned_at DESC);
CREATE INDEX IF NOT EXISTS idx_model_ab_assignments_run
    ON model_ab_assignments (run_id, step);

-- model_ab_shadow_responses captures the output of every NON-PRIMARY
-- arm (i.e. the B/C/D... arms whose decisions are NOT executed).
-- The primary arm's response is already persisted by the existing
-- decision audit trail; storing it again here would double the row
-- count for no analytical value.
CREATE TABLE IF NOT EXISTS model_ab_shadow_responses (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    experiment_id   UUID NOT NULL REFERENCES model_ab_experiments(id) ON DELETE CASCADE,
    assignment_id   UUID NOT NULL REFERENCES model_ab_assignments(id) ON DELETE CASCADE,
    run_id          TEXT NOT NULL,
    step            TEXT NOT NULL,
    agent_id        TEXT,
    fund_id         TEXT,

    arm_index       INTEGER NOT NULL,
    arm_name        TEXT NOT NULL,
    arm_model       TEXT NOT NULL,  -- "openai/gpt-4o" style label

    -- The raw LLM payload (system+user prompt INPUT) is intentionally
    -- not duplicated here — the primary arm's audit row already has
    -- it, and shadow arms see identical input by definition. We DO
    -- store the raw output so we can re-parse with newer parsers
    -- without re-querying the LLM.
    raw_output      TEXT,
    parsed_output   JSONB,           -- post-parse structured form, NULL on parse error
    parse_error     TEXT,

    input_tokens    INTEGER,
    output_tokens   INTEGER,
    latency_ms      INTEGER,
    cost_micro      BIGINT,          -- ¥ × 1e6 to avoid float drift

    error_text      TEXT,            -- non-null when the arm failed
    finished_at     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_model_ab_shadow_responses_experiment_finished
    ON model_ab_shadow_responses (experiment_id, finished_at DESC);
CREATE INDEX IF NOT EXISTS idx_model_ab_shadow_responses_run_step
    ON model_ab_shadow_responses (run_id, step);
