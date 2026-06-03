-- 078: Sprint 12 — alertmanager webhook ingest.
--
-- One row per alert *transition*. A single alert that flaps fires
-- multiple rows (firing → resolved → firing); the admin UI groups by
-- fingerprint and renders the timeline.
--
-- Fingerprint is what alertmanager already computes — a sha256 over
-- (alertname + label_set). Using it as the dedup key lets a flaky
-- alertmanager retry the same webhook N times without spamming the
-- table, while still recording genuine state changes.

CREATE TABLE IF NOT EXISTS admin_alert_events (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    fingerprint  TEXT NOT NULL,
    alertname    TEXT NOT NULL,
    severity     TEXT NOT NULL DEFAULT 'warning',
    component    TEXT,
    status       TEXT NOT NULL,          -- 'firing' / 'resolved'
    summary      TEXT,
    description  TEXT,
    labels       JSONB NOT NULL DEFAULT '{}'::jsonb,
    annotations  JSONB NOT NULL DEFAULT '{}'::jsonb,
    starts_at    TIMESTAMPTZ NOT NULL,
    ends_at      TIMESTAMPTZ,
    received_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Acknowledgement bookkeeping. NULL until an admin acks the
    -- alert via PATCH /api/admin/alerts/{id}/ack. acknowledged_by
    -- is the admin user id.
    acknowledged_by TEXT,
    acknowledged_at TIMESTAMPTZ,
    acknowledgement_note TEXT
);

-- Idempotency dedup: alertmanager retries the same webhook with the
-- same (fingerprint, status, starts_at). The partial unique index
-- skips the rows where starts_at is NULL (never happens in
-- production, but defensive against poorly-formed test fixtures).
CREATE UNIQUE INDEX IF NOT EXISTS admin_alert_events_dedup_idx
  ON admin_alert_events (fingerprint, status, starts_at)
  WHERE starts_at IS NOT NULL;

-- Read access patterns:
--   * timeline by fingerprint (admin UI drill-down)
--   * recent firing alerts (admin UI top panel)
CREATE INDEX IF NOT EXISTS admin_alert_events_fp_idx
  ON admin_alert_events (fingerprint, starts_at DESC);
CREATE INDEX IF NOT EXISTS admin_alert_events_recent_idx
  ON admin_alert_events (received_at DESC)
  WHERE status = 'firing';

COMMENT ON TABLE admin_alert_events IS
  'Sprint 12 alertmanager webhook ingest. One row per state transition; fingerprint+status+starts_at is the dedup key.';
