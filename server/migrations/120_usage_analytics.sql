-- Migration 120: User feature usage telemetry

CREATE TABLE IF NOT EXISTS user_feature_usage_events (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    user_role   VARCHAR(32) NOT NULL DEFAULT 'user',
    event_name  VARCHAR(64) NOT NULL,
    feature_key VARCHAR(160) NOT NULL,
    page_path   VARCHAR(256) NOT NULL DEFAULT '',
    event_count INTEGER NOT NULL DEFAULT 1 CHECK (event_count > 0 AND event_count <= 1000),
    metadata    JSONB NOT NULL DEFAULT '{}',
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_user_feature_usage_events_user_time
    ON user_feature_usage_events (user_id, occurred_at DESC);

CREATE INDEX IF NOT EXISTS idx_user_feature_usage_events_event_time
    ON user_feature_usage_events (event_name, occurred_at DESC);

CREATE INDEX IF NOT EXISTS idx_user_feature_usage_events_feature_time
    ON user_feature_usage_events (feature_key, occurred_at DESC);

