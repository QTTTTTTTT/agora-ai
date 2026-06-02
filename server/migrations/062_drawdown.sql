-- Migration 062 — drawdown soft-circuit-breaker (P3-5).
--
-- What this stores
--
-- Two tables together model "automatic position trim when fund
-- drawdown exceeds policy thresholds":
--
--   * `drawdown_policies` — per-fund tier configuration. Multiple
--     tiers per fund (e.g. tier 1 = -5% DD → trim 25%, tier 2 =
--     -10% → trim 50%, tier 3 = -15% → flatten). The engine takes
--     the WORST tier currently breached.
--
--   * `drawdown_events` — one row per detected breach. Records the
--     DD level, the tier that fired, the trim plan the engine
--     produced, and the resolution (operator approved → orders
--     submitted, OR operator dismissed, OR auto-executed under
--     policy.auto_execute).
--
-- Why two tables instead of inlining the tiers
--
-- Per-tier rows let operators tune one tier at a time without
-- touching the others, and let the audit chain capture each tier
-- bump as a discrete diff. Inlining everything as JSON would force
-- the audit log to render a whole-policy diff on every minor
-- knob change.
--
-- Why we don't store peak_nav here
--
-- Peak NAV (high-water mark) is derived from the existing
-- nav_snapshots table — a daily roll-up that the platform already
-- maintains. Storing it again would create an unwanted source of
-- truth disagreement; the engine recomputes peak on demand from
-- nav_snapshots over the lookback window declared in the policy.

CREATE TABLE IF NOT EXISTS drawdown_policies (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    fund_id             UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    -- tier 1 = first / mildest; tier 3 = last / hardest. Constrained
    -- to [1, 5] so we can keep the metric label cardinality bounded.
    tier                SMALLINT NOT NULL CHECK (tier BETWEEN 1 AND 5),
    -- dd_pct is stored as a NEGATIVE fraction (e.g. -0.05 means
    -- -5% drawdown triggers this tier). Storing negative makes
    -- the comparison "current_dd <= dd_pct" read naturally.
    dd_pct              NUMERIC(6, 4) NOT NULL CHECK (dd_pct < 0 AND dd_pct >= -1),
    -- action vocabulary; matches drawdown.Action constants.
    action              VARCHAR(24) NOT NULL
                          CHECK (action IN ('trim_proportional', 'flatten', 'defensive_only')),
    -- trim_ratio in [0, 1]; only meaningful for trim_proportional.
    -- 0.25 = sell 25% of every long position pro-rata.
    trim_ratio          NUMERIC(4, 3) NOT NULL DEFAULT 0
                          CHECK (trim_ratio >= 0 AND trim_ratio <= 1),
    -- cooldown_hours rate-limits firing the SAME tier twice in a
    -- short window. A 24h cooldown means once we've fired tier 2
    -- today, we won't fire it again until tomorrow even if DD
    -- briefly recovers and re-breaches.
    cooldown_hours      INT NOT NULL DEFAULT 24
                          CHECK (cooldown_hours >= 0 AND cooldown_hours <= 720),
    -- auto_execute=true: the scheduler fires the trim plan
    -- automatically (still through the audit chain, still through
    -- the order pipeline's risk gates). Default false: the engine
    -- emits a `proposed` event for operator review. Real money
    -- funds typically opt-in only after a paper-trading dogfood.
    auto_execute        BOOLEAN NOT NULL DEFAULT FALSE,
    -- Optional explanatory note shown next to the tier in the
    -- admin UI (why this tier exists, what the operator should
    -- consider before approving). Free-form.
    note                TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- One tier per (fund, tier) — operator updates UPSERT in
    -- place. The DB unique guarantees no orphan tiers.
    UNIQUE (fund_id, tier)
);

CREATE INDEX IF NOT EXISTS drawdown_policies_fund_idx
    ON drawdown_policies (fund_id);

-- ----------------------------------------------------------------
-- drawdown_events — breach log
-- ----------------------------------------------------------------

CREATE TABLE IF NOT EXISTS drawdown_events (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    fund_id             UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    tier                SMALLINT NOT NULL CHECK (tier BETWEEN 1 AND 5),
    -- dd_pct at the moment of detection. Same sign convention as
    -- the policy column.
    current_dd_pct      NUMERIC(7, 4) NOT NULL,
    peak_nav            NUMERIC(20, 6) NOT NULL,
    current_nav         NUMERIC(20, 6) NOT NULL,
    -- The action the policy says to take (echoed for audit).
    action              VARCHAR(24) NOT NULL
                          CHECK (action IN ('trim_proportional', 'flatten', 'defensive_only')),
    -- Plan = {symbol, side='sell', qty}[]. JSONB instead of a
    -- secondary table because the plan is a snapshot artefact —
    -- once the operator approves, the plan items become orders;
    -- once it's dismissed, the plan stays as the audit record of
    -- "what the engine WOULD have done". No separate row-level
    -- queries hit this.
    trim_plan           JSONB NOT NULL DEFAULT '[]'::jsonb,
    -- Lifecycle:
    --   proposed   — engine detected breach; operator hasn't acted
    --   approved   — operator (or auto_execute) authorised; orders queued
    --   executed   — orders submitted; trade_ids back-fills
    --   dismissed  — operator waved it off (note required)
    --   superseded — a later, deeper-tier event preempted this one
    status              VARCHAR(16) NOT NULL DEFAULT 'proposed'
                          CHECK (status IN ('proposed', 'approved', 'executed', 'dismissed', 'superseded')),
    -- Trade IDs back-filled when status flips to 'executed'.
    -- Reads happen by event_id so we don't need an index here.
    trade_ids           JSONB NOT NULL DEFAULT '[]'::jsonb,
    review_note         TEXT,
    reviewed_by         UUID,
    reviewed_at         TIMESTAMPTZ,
    -- The snapshot ID we computed peak/current from. Pinned so a
    -- later auditor can reproduce the math.
    nav_snapshot_id     UUID,
    -- Detection bookkeeping.
    detected_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    detector_version    VARCHAR(32) NOT NULL DEFAULT 'v1',
    -- Cooldown gate: re-detecting the same tier inside the
    -- cooldown window is suppressed at the engine; this column
    -- exists so the engine can pull "last fired at" cheaply.
    -- Indexed in (fund_id, tier, detected_at DESC) below.
    metadata            JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS drawdown_events_fund_status_idx
    ON drawdown_events (fund_id, status, detected_at DESC);

CREATE INDEX IF NOT EXISTS drawdown_events_cooldown_idx
    ON drawdown_events (fund_id, tier, detected_at DESC);

CREATE INDEX IF NOT EXISTS drawdown_events_open_idx
    ON drawdown_events (detected_at DESC)
    WHERE status = 'proposed';
