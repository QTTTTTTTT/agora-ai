-- 079: Sprint 13 — model-A/B auto-promotion drafts.
--
-- The promotion scanner reads the rolled-up Reporter output every
-- night, and when a treatment arm has beaten the primary on a
-- pre-agreed set of metrics for the configured streak length it
-- writes ONE draft row here. The admin board surfaces drafts in
-- "pending" state; a human applies or rejects them. We never
-- auto-flip production traffic — the draft is a recommendation, an
-- "apply" action records the decision and closes the experiment.

CREATE TABLE IF NOT EXISTS model_ab_promotion_drafts (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    experiment_id           UUID NOT NULL REFERENCES model_ab_experiments(id) ON DELETE CASCADE,
    -- Recommendation pointers. recommended_arm_index is the winner
    -- of the comparison; primary_arm_index records what the
    -- experiment's current primary arm was at draft time so the
    -- admin UI can show "X → Y" cleanly.
    recommended_arm_index   INT  NOT NULL,
    recommended_arm_label   TEXT NOT NULL,
    primary_arm_index       INT  NOT NULL,
    primary_arm_label       TEXT NOT NULL,
    -- Streak metadata.
    streak_days             INT  NOT NULL DEFAULT 1,
    evaluated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    window_from             TIMESTAMPTZ,
    window_to               TIMESTAMPTZ,
    -- Audit payload — the full criteria the scanner used + the
    -- report snapshot that backs the recommendation. Stored as
    -- JSONB so the admin UI can render a "why did this fire?"
    -- panel without re-running the scanner.
    criteria_payload        JSONB NOT NULL DEFAULT '{}'::jsonb,
    report_snapshot         JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- Lifecycle. pending → applied / rejected. We don't expose
    -- "applied_with_followup" yet — the UI offers a checkbox
    -- "open a follow-up experiment" that fans into the
    -- model_ab_experiments table via a second insert; the draft
    -- itself just records the human decision.
    status                  TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending','applied','rejected','superseded')),
    applied_by              TEXT,
    applied_at              TIMESTAMPTZ,
    rejection_reason        TEXT,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Idempotency — at most one PENDING draft per experiment so the
-- nightly scanner can re-run safely without piling up duplicates.
CREATE UNIQUE INDEX IF NOT EXISTS model_ab_promotion_drafts_one_pending_per_exp
  ON model_ab_promotion_drafts (experiment_id)
  WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS model_ab_promotion_drafts_exp_idx
  ON model_ab_promotion_drafts (experiment_id, evaluated_at DESC);
CREATE INDEX IF NOT EXISTS model_ab_promotion_drafts_status_idx
  ON model_ab_promotion_drafts (status, evaluated_at DESC);

COMMENT ON TABLE model_ab_promotion_drafts IS
  'Sprint 13 promotion-draft store. One pending row per experiment; admins apply / reject.';
