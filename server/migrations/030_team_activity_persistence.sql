-- 030_team_activity_persistence.sql
--
-- Cross-restart persistence for the Team Live Activity panel.
--
-- Pre-Phase: workflow.ActivityBus held the latest ~200 events per fund
-- in an in-memory ring buffer. Every container restart (deploy, OOM,
-- crash) wiped the panel. Users with morning-only activity would open
-- the panel in the evening and see an empty timeline even though the
-- workflow had run; from their POV the team had stopped working.
--
-- This phase adds an async double-write: ActivityBus.Publish still hits
-- the ring synchronously (zero added latency for SSE subscribers) and
-- additionally queues each event for batched insertion into this table.
-- The REST /team/activity endpoint reads the ring for the hot path and
-- falls back to this table for "load more" / post-restart backfill.
--
-- Retention is per-fund via funds.config.activityRetentionDays (1..10
-- days, defaults to 7). A daily cleanup loop reads each fund's setting
-- and deletes rows older than now - retentionDays.

CREATE TABLE IF NOT EXISTS workflow_activity_events (
    id            BIGSERIAL PRIMARY KEY,
    fund_id       UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    -- Per-fund monotonic sequence. The bus seeds it from MAX(seq) on
    -- first publish after restart so SSE sinceSeq continues to work
    -- across deploys. UNIQUE(fund_id, seq) guards against double-write
    -- if the async writer's queue is somehow flushed twice.
    seq           BIGINT NOT NULL,
    type          TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'system',
    step          TEXT,
    run_id        TEXT,
    trading_date  TEXT,
    message       TEXT NOT NULL,
    error_message TEXT,
    event_at      TIMESTAMPTZ NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- (fund_id, event_at DESC) drives "/team/activity?before=<ts>" paging:
-- the panel passes the oldest visible event_at and asks for the next
-- page in newest-first order.
CREATE INDEX IF NOT EXISTS idx_waevents_fund_event_at
    ON workflow_activity_events (fund_id, event_at DESC, id DESC);

-- Per-fund seq uniqueness: dedupes a double-flushed async writer batch
-- and lets SSE clients use sinceSeq across restarts deterministically.
CREATE UNIQUE INDEX IF NOT EXISTS idx_waevents_fund_seq_uniq
    ON workflow_activity_events (fund_id, seq);

-- Retention cron deletes by event_at < cutoff. The cutoff is computed
-- per-fund from config, so the index needs to be (fund_id, event_at).
-- The newest-first listing index above already covers that lookup
-- direction, so we don't add a second index here.

COMMENT ON TABLE workflow_activity_events IS
    'Persistent backing store for workflow.ActivityBus. ActivityBus.Publish async-writes each event here so the Team Live Activity panel survives container restarts. Retention is per-fund via funds.config.activityRetentionDays (1..10 days, default 7), enforced by the activity-retention daily cron.';
