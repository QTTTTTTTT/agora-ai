-- 037_strategy_promotions.sql
--
-- Phase 2J/K/L: strategy promotion lifecycle, shadow comparison,
-- and live-vs-backtest decay monitoring.
--
-- A "promotion" wraps a (basis backtest → production engine
-- configuration) handover. State machine:
--
--   pending_review → approved → shadow → active → superseded
--                      ↓                    ↓        ↓
--                  rejected            rolled_back  decayed
--
-- The basis_job_id MUST reference a completed backtest job (the
-- service layer enforces walk-forward presence for stricter
-- promotion gates; the schema only enforces the FK).

CREATE TABLE strategy_promotions (
    id                  TEXT PRIMARY KEY,
    fund_id             TEXT NOT NULL,
    proposed_by         TEXT NOT NULL,
    basis_job_id        TEXT NOT NULL,
    engine_kind         TEXT NOT NULL,
    -- Free-form engine params snapshot — the resolver hands this
    -- back to the PMAgent at decision time. JSONB so we can add
    -- fields without a migration.
    engine_params       JSONB NOT NULL DEFAULT '{}',
    -- Snapshot of the basis backtest's headline metrics. Used by
    -- the decay monitor as the comparison baseline.
    baseline_metrics    JSONB NOT NULL DEFAULT '{}',
    status              TEXT NOT NULL
                            CHECK (status IN (
                                'pending_review','approved','shadow',
                                'active','superseded','rejected',
                                'rolled_back','decayed'
                            )),
    -- How long shadow mode runs before auto-activation eligibility.
    shadow_days         INT NOT NULL DEFAULT 7,
    -- decay_ratio: when actual_sharpe / baseline_sharpe falls below
    -- this, the monitor fires a downgrade. 0.5 = "half the
    -- backtest Sharpe survives, otherwise pull the plug".
    decay_ratio         DOUBLE PRECISION NOT NULL DEFAULT 0.5,
    -- Approval audit. Dual-control gate enforced at service layer
    -- when the company has it enabled.
    approved_by         TEXT,
    approved_at         TIMESTAMPTZ,
    rejected_by         TEXT,
    rejected_at         TIMESTAMPTZ,
    rejected_reason     TEXT,
    -- Shadow + active lifecycle timestamps.
    shadow_started_at   TIMESTAMPTZ,
    shadow_completed_at TIMESTAMPTZ,
    activated_at        TIMESTAMPTZ,
    deactivated_at      TIMESTAMPTZ,
    deactivated_reason  TEXT,
    notes               TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_strategy_promotions_fund_status
    ON strategy_promotions(fund_id, status, created_at DESC);

-- Only ONE active promotion per fund at any time. Enforced via a
-- partial unique index so superseded/rejected rows don't conflict.
CREATE UNIQUE INDEX idx_strategy_promotions_one_active_per_fund
    ON strategy_promotions(fund_id) WHERE status = 'active';

-- ShadowDiff: each row is a per-trading-day comparison of what the
-- shadow promotion would have decided vs what the live engine
-- actually decided. Used by the operator to validate the shadow
-- before flipping to active.
CREATE TABLE promotion_shadow_diffs (
    id              TEXT PRIMARY KEY,
    promotion_id    TEXT NOT NULL REFERENCES strategy_promotions(id) ON DELETE CASCADE,
    trading_date    DATE NOT NULL,
    -- Each side's compact decision summary. Captured as JSONB so
    -- we can grow the shape (e.g. add confidence bands) without
    -- a schema migration.
    shadow_decision JSONB NOT NULL DEFAULT '{}',
    active_decision JSONB NOT NULL DEFAULT '{}',
    -- agreement: derived flag, true when shadow & active produced
    -- the same (action, symbol) tuple. Distance / amount deltas
    -- live inside the decision blobs.
    agreement       BOOLEAN NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(promotion_id, trading_date)
);

CREATE INDEX idx_promotion_shadow_diffs_lookup
    ON promotion_shadow_diffs(promotion_id, trading_date DESC);

-- HealthSnapshot: rolling-window actual metrics produced by the
-- decay monitor. Each row records (window, observed sharpe,
-- observed return, max DD, sharpe_decay_ratio = actual / baseline).
CREATE TABLE promotion_health_snapshots (
    id                   TEXT PRIMARY KEY,
    promotion_id         TEXT NOT NULL REFERENCES strategy_promotions(id) ON DELETE CASCADE,
    snapshot_at          TIMESTAMPTZ NOT NULL,
    window_days          INT NOT NULL,
    actual_sharpe        DOUBLE PRECISION,
    actual_return        DOUBLE PRECISION,
    actual_max_drawdown  DOUBLE PRECISION,
    actual_trade_count   INT NOT NULL DEFAULT 0,
    sharpe_decay_ratio   DOUBLE PRECISION,
    decay_flag           BOOLEAN NOT NULL DEFAULT FALSE,
    notes                TEXT
);

CREATE INDEX idx_promotion_health_lookup
    ON promotion_health_snapshots(promotion_id, snapshot_at DESC);

-- Audit log: every state transition + decay alert lands here so
-- the UI can show a timeline.
CREATE TABLE promotion_events (
    id              TEXT PRIMARY KEY,
    promotion_id    TEXT NOT NULL REFERENCES strategy_promotions(id) ON DELETE CASCADE,
    event_type      TEXT NOT NULL,
    actor_user_id   TEXT,
    payload         JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_promotion_events_lookup
    ON promotion_events(promotion_id, created_at);
