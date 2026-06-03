-- 084_fund_llm_overrides.sql — S14.B: fund-level / agent-level
-- LLM provider override.
--
-- Why this exists: marketplace funds run on a strategy owner's
-- platform credit. The owner needs final control over which provider
-- powers which agent — e.g. "send my pm_agent through Claude (best
-- reasoning), but my news_agent can stay on the cheaper deepseek".
-- Today the router only honours user-level overrides
-- (subscription/model_config). When a subscriber runs the fund the
-- choice is the subscriber's, not the owner's. That's wrong for
-- marketplace economics and reproducibility.
--
-- Specificity (resolved at request time, highest priority first):
--   1. (fund + agent + role + tier)   ← four-way match
--   2. (fund + agent + role)          ← any tier
--   3. (fund + agent + tier)          ← any role
--   4. (fund + agent)                 ← all roles, all tiers for this agent
--   5. (fund + role + tier)
--   6. (fund + role)
--   7. (fund + tier)
--   8. (fund)                         ← fund-wide override
--
-- We compute "which row wins" in code (single SQL with ORDER BY
-- specificity); storing all 8 specificity tiers as separate columns
-- would be unwieldy. The lookup table is small per fund (typically
-- < 20 rows) so this trades a tiny CPU cost for clarity.
--
-- Why FK on (provider, label) and not just (provider): a fund can
-- pin a specific labelled config (e.g. "openai-prod-with-rotation"
-- vs "openai-shared"). When label is NULL the override means "use
-- whichever active row for this provider is currently default" —
-- the FK is satisfied (MATCH SIMPLE) and the resolver picks the
-- platform default row at request time. CASCADE on delete keeps
-- overrides consistent when a provider is retired.

CREATE TABLE IF NOT EXISTS fund_llm_overrides (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    fund_id       UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,

    -- NULLable scope columns. NULL = "applies to all values in this
    -- dimension". We use NULLS NOT DISTINCT on the unique index so
    -- (fund, NULL, NULL, NULL) is a single distinct row per fund.
    -- See uniq_fund_llm_overrides_scope below.
    agent_id      UUID REFERENCES agents(id) ON DELETE CASCADE,
    role          VARCHAR(32),
    model_tier    VARCHAR(16),

    -- Provider taxonomy. The FK to platform_llm_providers is MATCH
    -- SIMPLE — when label is NULL the FK isn't enforced (Postgres
    -- default), giving us "any active row for this provider" semantics.
    provider      VARCHAR(32) NOT NULL,
    label         VARCHAR(64),
    model_name    VARCHAR(128),

    enabled       BOOLEAN NOT NULL DEFAULT TRUE,
    note          TEXT,

    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by    UUID,
    updated_by    UUID,

    -- Soft validation. The full set of valid values comes from
    -- platform_llm_providers (which is itself enum-constrained at
    -- the column level). Here we just sanity-check tier so a typo
    -- like 'criticla' is rejected at write time.
    CONSTRAINT chk_fund_llm_overrides_tier
        CHECK (model_tier IS NULL OR model_tier IN ('critical', 'standard', 'simple')),

    CONSTRAINT fk_fund_llm_overrides_provider_label
        FOREIGN KEY (provider, label)
        REFERENCES platform_llm_providers (provider, label)
        ON DELETE CASCADE
);

-- Unique per scope. NULLS NOT DISTINCT (pg15+) makes (fund, NULL,
-- NULL, NULL) collide with itself — so you can have at most one
-- fund-wide override per (fund, role, tier, agent) combination
-- including the all-wildcard row. This prevents the operator from
-- accidentally creating two competing fund-wide overrides.
--
-- IF NOT EXISTS so the migration is idempotent when the schema was
-- pre-created by an out-of-band tool but the schema_migrations row
-- was not inserted (e.g. dev environment that applied DDL by hand).
-- Re-running the same migration now becomes a clean no-op instead
-- of crashing on duplicate-object errors.
CREATE UNIQUE INDEX IF NOT EXISTS uniq_fund_llm_overrides_scope
    ON fund_llm_overrides (fund_id, agent_id, role, model_tier)
    NULLS NOT DISTINCT;

-- Hot read path: ModelRouter.ResolveModel issues one SELECT per
-- request scoped by fund_id. The composite index covers it
-- and also serves the admin "list overrides for fund X" page.
CREATE INDEX IF NOT EXISTS idx_fund_llm_overrides_fund_enabled
    ON fund_llm_overrides (fund_id) WHERE enabled = TRUE;

-- Reverse lookup: when a provider is retired the cascade deletion
-- needs an efficient seek over rows referencing it.
CREATE INDEX IF NOT EXISTS idx_fund_llm_overrides_provider
    ON fund_llm_overrides (provider, label);

-- Auto-update updated_at on row mutation. Reuses the same touch
-- trigger pattern as platform_llm_providers (S13).
CREATE OR REPLACE FUNCTION touch_fund_llm_overrides_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_fund_llm_overrides_updated_at ON fund_llm_overrides;
CREATE TRIGGER trg_fund_llm_overrides_updated_at
    BEFORE UPDATE ON fund_llm_overrides
    FOR EACH ROW EXECUTE FUNCTION touch_fund_llm_overrides_updated_at();

COMMENT ON TABLE fund_llm_overrides IS
    'Per-fund LLM provider preference (S14.B). Resolved at request time by ModelRouter; sits between A/B experiments and user-level overrides in the priority chain. NULL scope columns are wildcards.';
