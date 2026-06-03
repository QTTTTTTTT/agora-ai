-- 083_platform_llm_provider_observability.sql — S14.A: provider
-- health history (per-ping) + daily cost rollups (per provider
-- × model × day).
--
-- Why two tables, not one:
--   * health_history rows are short-lived (30-day retention) and
--     written every 5 minutes per active provider. Volume is
--     bounded: N providers × 288 checks/day × 30 days ≈ low
--     thousands per provider.
--   * daily_rollups are permanent (year-over-year cost / token
--     trend lives here). Volume is far lower: providers × models
--     × days. Computed from usage_entries by a 1-hour rollup
--     loop, idempotent via PK (provider, model_name, day).
--
-- We deliberately do NOT extend usage_entries with latency_ms /
-- error_count. The hot path stays untouched; rollups source from
-- usage_entries for cost + tokens and from health_history for
-- latency + error count. Latency on rollup is "provider endpoint
-- reactivity from our datacentre", not "end-to-end Chat() latency
-- including user code" — that distinction is important to call out
-- when the dashboard goes live so operators don't misinterpret p95
-- spikes during a feature-flag toggle.

-- 1) Per-ping history. The probe loop (cmd/server/llm_provider_health_loop.go)
-- inserts one row per (provider_id × tick); the admin dashboard
-- reads the most recent 24h / 7d window per provider. 30-day
-- retention is enforced by a daily cleanup query the same loop
-- runs at startup.
CREATE TABLE IF NOT EXISTS platform_llm_provider_health_history (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_id        UUID NOT NULL REFERENCES platform_llm_providers(id) ON DELETE CASCADE,
    -- Denormalised provider taxonomy for fast filter without join.
    provider           VARCHAR(32) NOT NULL,
    label              VARCHAR(64) NOT NULL,
    -- 5-minute tick boundary the probe was scheduled for. Lets the
    -- dashboard align points across providers without explicit
    -- binning ("show the latency for the 10:05 tick").
    checked_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ok                 BOOLEAN NOT NULL,
    latency_ms         INTEGER NOT NULL DEFAULT 0,
    http_status        INTEGER NOT NULL DEFAULT 0,
    -- First 200 chars of error body / transport message. Plaintext
    -- never appears (the probe path masks keys before storing).
    message            TEXT,
    -- Optional: which model the probe used. Lets the dashboard
    -- distinguish "openai gpt-4o" vs "openai gpt-4o-mini" health
    -- when the same provider has multiple labels.
    model_name         VARCHAR(128),
    CHECK (latency_ms >= 0),
    CHECK (http_status >= 0)
);

-- Hot read path: dashboard fetches per-provider recent window.
CREATE INDEX IF NOT EXISTS idx_provider_health_history_provider_time
    ON platform_llm_provider_health_history (provider_id, checked_at DESC);

-- Cleanup path: drop rows older than 30 days.
CREATE INDEX IF NOT EXISTS idx_provider_health_history_checked_at
    ON platform_llm_provider_health_history (checked_at);

COMMENT ON TABLE platform_llm_provider_health_history IS
    '5-minute provider ping history, 30-day retention. Source for the admin observability dashboard. Written by llm_provider_health_loop.go.';

-- 2) Per-day cost / token rollup keyed by (provider, model_name, day).
-- The rollup loop reads usage_entries WHERE created_at >= now()-1h
-- and upserts the (provider, model_name, day) bucket. Re-runnable
-- because the upsert overwrites the bucket; missing a tick just
-- means the next tick catches up. Permanent retention — table
-- stays small (providers × models × days; ~thousands/year).
CREATE TABLE IF NOT EXISTS platform_llm_provider_daily_rollups (
    provider           VARCHAR(32) NOT NULL,
    model_name         VARCHAR(128) NOT NULL,
    day                DATE NOT NULL,
    calls              BIGINT NOT NULL DEFAULT 0,
    input_tokens       BIGINT NOT NULL DEFAULT 0,
    output_tokens      BIGINT NOT NULL DEFAULT 0,
    total_tokens       BIGINT NOT NULL DEFAULT 0,
    cost_cents         NUMERIC(14, 4) NOT NULL DEFAULT 0,
    custom_key_calls   BIGINT NOT NULL DEFAULT 0,
    -- Last time the rollup loop touched this bucket. Lets the
    -- admin dashboard show "is today's data up to date?".
    last_rolled_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (provider, model_name, day),
    CHECK (calls >= 0),
    CHECK (input_tokens >= 0),
    CHECK (output_tokens >= 0),
    CHECK (total_tokens >= 0),
    CHECK (cost_cents >= 0)
);

-- Hot read path: "last 7d cost per provider" — index by day DESC
-- with provider as a co-predicate.
CREATE INDEX IF NOT EXISTS idx_provider_daily_rollups_day
    ON platform_llm_provider_daily_rollups (day DESC);
CREATE INDEX IF NOT EXISTS idx_provider_daily_rollups_provider_day
    ON platform_llm_provider_daily_rollups (provider, day DESC);

COMMENT ON TABLE platform_llm_provider_daily_rollups IS
    'Per (provider, model_name, day) cost & token rollup. Computed hourly from usage_entries. Permanent retention. Source for the admin cost dashboard.';
