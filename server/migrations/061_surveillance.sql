-- Migration 061 — trade surveillance (P1-7).
--
-- What this stores
--
-- The detection engine emits a `SurveillanceEvent` for every
-- pattern of trading activity that matches a configured rule
-- (wash trades, marking the close, self-trade pairs, etc.).
-- Those events land in `surveillance_events` so:
--
--   1. Compliance can review and triage from the admin UI.
--   2. Repeated detections roll up into a single durable artefact
--      (the row), not a stream of metric counters.
--   3. The audit hash chain captures who reviewed / cleared /
--      escalated each event.
--
-- One row = one pattern instance
--
-- The detector groups the contributing trades into a single event
-- — e.g. a 3-leg wash-trade triplet becomes ONE row with
-- `trade_ids` listing all three. This avoids fan-out to per-trade
-- alerts which would drown the queue.
--
-- Why a closed vocabulary on rule_code
--
-- The dashboard's filter list and the metric label series both
-- depend on a finite set of rule codes. New rules require BOTH a
-- code change AND a migration that updates this CHECK so we never
-- have a metric label we can't explain.

CREATE TABLE IF NOT EXISTS surveillance_events (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    fund_id            UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    rule_code          VARCHAR(48) NOT NULL,
    severity           VARCHAR(16) NOT NULL DEFAULT 'warning'
                         CHECK (severity IN ('info', 'warning', 'critical')),
    -- Symbol the pattern centres on. NULL only for cross-symbol
    -- rules (none today; future "pump-and-dump basket" rules
    -- might leave it null with the basket in metadata).
    symbol             VARCHAR(64),
    instrument_key     VARCHAR(96),
    -- Window the pattern spans. Both timestamps are stored UTC.
    window_start       TIMESTAMPTZ NOT NULL,
    window_end         TIMESTAMPTZ NOT NULL,
    -- The contributing trade_executions.id values, in chronological
    -- order. JSONB array of strings; the repo helper hides the
    -- shape from callers.
    trade_ids          JSONB NOT NULL DEFAULT '[]'::jsonb,
    -- Compact one-line description for table rendering.
    summary            TEXT NOT NULL DEFAULT '',
    -- Free-form extra; rule-specific. e.g. wash-trade puts
    -- the qty matrix here, marking-close puts the
    -- VWAP-deviation here.
    metadata           JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- Lifecycle.
    status             VARCHAR(16) NOT NULL DEFAULT 'open'
                         CHECK (status IN ('open', 'reviewing', 'cleared', 'escalated')),
    review_note        TEXT,
    reviewed_by        UUID,
    reviewed_at        TIMESTAMPTZ,
    -- Detection metadata.
    detected_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    detector_version   VARCHAR(32) NOT NULL DEFAULT 'v1',
    -- Idempotency: re-running the engine over the same window
    -- shouldn't double-insert. The detector computes a stable
    -- fingerprint over (fund, rule_code, sorted trade_ids) and
    -- the unique index dedupes.
    fingerprint        VARCHAR(64) NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Closed vocabulary keeps Prometheus label cardinality bounded
    -- and the UI filter list predictable.
    CONSTRAINT surveillance_events_rule_chk CHECK (rule_code IN (
        'wash_trade',
        'marking_close',
        'self_trade_pair',
        'rapid_fire_reversal',
        'layering_suspect'
    ))
);

CREATE UNIQUE INDEX IF NOT EXISTS surveillance_events_fingerprint_uq
    ON surveillance_events (fingerprint);

CREATE INDEX IF NOT EXISTS surveillance_events_fund_status_idx
    ON surveillance_events (fund_id, status, detected_at DESC);

CREATE INDEX IF NOT EXISTS surveillance_events_open_critical_idx
    ON surveillance_events (fund_id, severity, detected_at DESC)
    WHERE status = 'open';

CREATE INDEX IF NOT EXISTS surveillance_events_rule_idx
    ON surveillance_events (rule_code, detected_at DESC);

-- ----------------------------------------------------------------
-- surveillance_runs: optional run-level aggregation, mirrors
-- reconciliation_runs. Lets the operator see "scan ran at X over
-- N trades, produced M events" without grepping logs.
-- ----------------------------------------------------------------

CREATE TABLE IF NOT EXISTS surveillance_runs (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    fund_id             UUID REFERENCES funds(id) ON DELETE CASCADE,
    triggered_by        UUID,
    trigger_source      VARCHAR(24) NOT NULL DEFAULT 'manual'
                          CHECK (trigger_source IN ('manual', 'scheduled', 'replay')),
    window_start        TIMESTAMPTZ NOT NULL,
    window_end          TIMESTAMPTZ NOT NULL,
    trade_count         INT NOT NULL DEFAULT 0,
    event_count_total   INT NOT NULL DEFAULT 0,
    event_count_critical INT NOT NULL DEFAULT 0,
    event_count_warning INT NOT NULL DEFAULT 0,
    event_count_info    INT NOT NULL DEFAULT 0,
    duration_ms         INT NOT NULL DEFAULT 0,
    status              VARCHAR(16) NOT NULL DEFAULT 'completed'
                          CHECK (status IN ('pending', 'completed', 'failed')),
    error_message       TEXT,
    summary             JSONB NOT NULL DEFAULT '{}'::jsonb,
    started_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at        TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS surveillance_runs_started_idx
    ON surveillance_runs (started_at DESC);

CREATE INDEX IF NOT EXISTS surveillance_runs_fund_idx
    ON surveillance_runs (fund_id, started_at DESC);
