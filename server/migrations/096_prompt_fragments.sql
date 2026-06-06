-- 096_prompt_fragments.sql — W3-22: closed-loop prompt evolution.
--
-- WHY THIS EXISTS
-- ---------------
-- Prompts in this codebase are currently hand-edited Go strings.
-- The closed-loop self-learning pipeline writes lessons (W1-5,
-- W2-9), maps them to outcomes (W1-5), and accumulates skills
-- (W2-8). What it CANNOT do is feed those wins back into the
-- system prompt itself. A reflection that "the bull agent
-- consistently misses dividend signals" stays a memory; it
-- cannot become a *prompt fragment* that the bull agent's
-- system prompt incorporates from then on.
--
-- W3-22 unblocks that loop. Each agent (or each prompt slot —
-- the system prompt, the rationale closer, the JSON output
-- contract) gets a *fragment* table where:
--
--   * variants are added (manually or by the reflection agent)
--   * each variant has a status (`draft` | `shadow` | `active`
--     | `archived`)
--   * use counts and outcome stats accumulate per variant
--   * an A/B-like router can pick `active` for production and
--     `shadow` for the parallel run that gathers reward signal
--
-- This migration is the schema; the internal/promptfragments
-- package owns the access pattern.
--
-- SHAPE
-- -----
-- prompt_fragments has a (slot_key, variant_id) primary key.
-- slot_key is the canonical name of the prompt slot
-- ("agent_bull/system", "pm/output_contract", etc).
-- variant_id is a stable nanoid generated when the variant is
-- inserted; the value is what the prompt-build path stamps
-- into provenance so post-hoc analysis can correlate.

CREATE TABLE IF NOT EXISTS prompt_fragments (
    slot_key       TEXT NOT NULL,
    variant_id     TEXT NOT NULL,
    body           TEXT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'draft'
        CHECK (status IN ('draft', 'shadow', 'active', 'archived')),
    weight         INTEGER NOT NULL DEFAULT 0,
    notes          TEXT,
    author_user_id UUID,
    parent_variant TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (slot_key, variant_id)
);

-- Per-variant rolling outcome stats. UPSERTed on every plan
-- whose provenance.prompt_variants includes this variant.
CREATE TABLE IF NOT EXISTS prompt_fragment_uses (
    slot_key      TEXT NOT NULL,
    variant_id    TEXT NOT NULL,
    uses          INTEGER NOT NULL DEFAULT 0,
    sum_alpha     DOUBLE PRECISION NOT NULL DEFAULT 0,
    hits          INTEGER NOT NULL DEFAULT 0,
    last_used_at  TIMESTAMPTZ,
    PRIMARY KEY (slot_key, variant_id),
    FOREIGN KEY (slot_key, variant_id) REFERENCES prompt_fragments(slot_key, variant_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_prompt_fragments_slot_status
    ON prompt_fragments (slot_key, status, weight DESC);

-- Constraint: at most one 'active' variant per slot. Shadow
-- variants are unlimited (multiple shadow variants are fine —
-- the router rotates between them). Enforced via partial unique
-- index because the standard UNIQUE constraint can't be
-- partial.
CREATE UNIQUE INDEX IF NOT EXISTS uq_prompt_fragments_active_per_slot
    ON prompt_fragments (slot_key)
    WHERE status = 'active';

COMMENT ON TABLE prompt_fragments IS
    'W3-22: prompt-fragment variants per slot. Closed-loop prompt evolution writes shadow variants here; admin promotes draft → shadow → active.';
COMMENT ON COLUMN prompt_fragments.status IS
    'Lifecycle. draft = author-only; shadow = served to a fraction of decisions for A/B; active = served by default; archived = retained for audit but never served.';
COMMENT ON COLUMN prompt_fragments.weight IS
    'Routing weight (used by the shadow router to bias one shadow variant over another). 0 means equal weighting.';
COMMENT ON COLUMN prompt_fragment_uses.sum_alpha IS
    'Cumulative realised alpha across plans that consumed this variant. Combined with uses to compute mean alpha.';
