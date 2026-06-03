-- 082_platform_llm_providers.sql — platform-level LLM provider
-- configuration (S13).
--
-- Replaces the 17-variable env soup
-- (LLM_PROVIDER / LLM_MODEL / LLM_BASE_URL / LLM_API_KEY
--  + LLM_<TIER>_* × 3 + <PROVIDER>_* × 6) with a single DB-backed
-- table managed via Admin UI. The env still loads but only when the
-- table is empty (first-time bootstrap / disaster recovery): the
-- wiring layer (cmd/server/wiring_adapters.go) seeds the table on
-- startup and then never reads env again until the next restart.
--
-- Why a single table (not three normalised ones for default /
-- tier-override / per-provider)?
--   * `is_platform_default` (partial UNIQUE) covers the LLM_* default.
--   * `model_tier` NULL/critical/standard/simple covers tier overrides.
--   * Multiple rows with the same `provider` but different `label`
--     cover the "openai-prod / openai-cheap / openai-staging" pattern
--     the env layout could never express.
-- The row's role is determined by its columns; the router resolves
-- `(provider, tier?)` against the active subset.
--
-- API key handling:
--   * Stored in `api_key_encrypted` as AES-GCM ciphertext under
--     MODEL_CONFIG_API_KEY_SECRET (same secret already used by
--     subscription/model_config.go for per-user configs). No new
--     key-management infra.
--   * `api_key_fingerprint` is the first 8 hex chars of SHA-256 over
--     the plaintext key. UI shows "sk-…a3f2" so operators can verify
--     "did the right key make it in?" without ever seeing plaintext.
--   * Plaintext key never crosses the API boundary on read paths.

CREATE TABLE IF NOT EXISTS platform_llm_providers (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Provider taxonomy mirrors internal/llm/provider.go.
    provider                 VARCHAR(32)  NOT NULL,
    -- Free-form label so the same provider can have multiple rows
    -- (e.g. "openai-prod-main" vs "openai-eu-relay"). Required so
    -- the (provider, label) UNIQUE has a non-empty discriminator.
    label                    VARCHAR(64)  NOT NULL,
    -- NULL = applies to any tier (used as the catch-all key by the
    -- router); critical/standard/simple = tier-specific override.
    model_tier               VARCHAR(16),
    model_name               VARCHAR(128) NOT NULL,
    base_url                 VARCHAR(512) NOT NULL,
    -- AES-GCM ciphertext, base64-encoded by EncryptAPIKey().
    api_key_encrypted        TEXT         NOT NULL,
    -- First 8 hex chars of SHA-256(plaintext_key). Lets the UI show
    -- "sk-…a3f2" without ever decrypting. Stable per key.
    api_key_fingerprint      VARCHAR(16)  NOT NULL,
    max_tokens               INT          NOT NULL DEFAULT 4096,
    temperature              NUMERIC(4,2) NOT NULL DEFAULT 0.70,
    input_price_per_1m       NUMERIC(12,4),
    output_price_per_1m      NUMERIC(12,4),
    cost_per_1m              NUMERIC(12,4),
    -- active / disabled / draft. Only active rows feed the router.
    status                   VARCHAR(16)  NOT NULL DEFAULT 'active',
    -- At most ONE row may be the platform default — enforced by
    -- the partial UNIQUE INDEX below. Default rows fill the catch-all
    -- slot in the router when no tier override matches.
    is_platform_default      BOOLEAN      NOT NULL DEFAULT FALSE,
    last_health_check_at     TIMESTAMPTZ,
    -- {"ok": true, "latency_ms": 412, "echoed_model": "gpt-4o", ...}
    last_health_check_result JSONB,
    -- Provenance: env_seed (first-time bootstrap) / admin (UI) /
    -- api (REST). Helps the audit trail distinguish "this came from
    -- LLM_API_KEY env at deploy time" from operator edits.
    source                   VARCHAR(16)  NOT NULL DEFAULT 'admin',
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by               UUID,
    updated_by               UUID,
    CHECK (provider IN ('openai','claude','deepseek','qwen','gemini','custom')),
    CHECK (model_tier IS NULL OR model_tier IN ('critical','standard','simple')),
    CHECK (status IN ('active','disabled','draft')),
    CHECK (source IN ('env_seed','admin','api')),
    CHECK (max_tokens > 0),
    CHECK (temperature >= 0 AND temperature <= 2),
    CHECK (length(api_key_fingerprint) BETWEEN 6 AND 16)
);

-- Same provider, different labels are allowed (multi-tenancy /
-- key rotation / staging endpoints). label='' is rejected by repo
-- but the column is plain NOT NULL here.
CREATE UNIQUE INDEX IF NOT EXISTS uq_platform_llm_providers_provider_label
    ON platform_llm_providers (provider, label);

-- Platform has AT MOST ONE default row — enforced via partial
-- UNIQUE on a constant expression. The repo writes
-- `is_platform_default = true` and the partial index rejects a
-- second concurrent attempt; the repo also unsets the previous
-- default in the same transaction so the toggle is atomic.
CREATE UNIQUE INDEX IF NOT EXISTS uq_platform_llm_providers_single_default
    ON platform_llm_providers ((1))
    WHERE is_platform_default = TRUE;

-- Hot path: router.LoadAll() filters on status='active'.
CREATE INDEX IF NOT EXISTS idx_platform_llm_providers_status_provider
    ON platform_llm_providers (status, provider)
    WHERE status = 'active';

-- Tier resolution: ResolveForTier(critical) wants rows where
-- model_tier = 'critical' AND status = 'active'.
CREATE INDEX IF NOT EXISTS idx_platform_llm_providers_tier_active
    ON platform_llm_providers (model_tier, status)
    WHERE status = 'active' AND model_tier IS NOT NULL;

-- Touch trigger so updated_at moves forward on every UPDATE.
CREATE OR REPLACE FUNCTION touch_platform_llm_providers_updated_at() RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_platform_llm_providers_updated_at ON platform_llm_providers;
CREATE TRIGGER trg_platform_llm_providers_updated_at
    BEFORE UPDATE ON platform_llm_providers
    FOR EACH ROW EXECUTE FUNCTION touch_platform_llm_providers_updated_at();

COMMENT ON TABLE platform_llm_providers IS
    'Platform-level LLM provider configurations (S13). Replaces the LLM_* / <PROVIDER>_API_KEY env layer for admin-managed CRUD with hot reload. API keys stored AES-GCM under MODEL_CONFIG_API_KEY_SECRET. At most one row may carry is_platform_default = true.';
