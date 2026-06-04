-- 085_memory_template_payload.sql — server-side i18n for AI-generated
-- memory rows (attribution lessons, risk alerts, etc.).
--
-- Why: lessons today are stored as a single English string. The UI is
-- already i18n-capable (shared/api-client/src/i18n.ts covers zh-CN +
-- en-US), but a row's text is baked in at write time and the same row
-- is shared by users with different locale preferences. We can't store
-- one language and pretend it's the answer.
--
-- Architecture (see lesson.go + web/src/lib/lessonRenderer.ts):
--   1. Server writes a stable template_key + a structured payload.
--   2. Frontend looks up messages[locale][template_key], interpolates
--      the payload with Intl.NumberFormat(locale), and renders the
--      final text. Same row, two locales, zero LLM call at render time.
--
-- Why template_key is a column rather than a tag:
--   * Tags are an unordered set; we need an exact 1:1 lookup key.
--   * Tags drive the existing classifier (sleeve:trend, regime:chop);
--     mixing translation routing into them would be a layering smell.
--   * A dedicated column lets us index it for "find all rows of this
--     template type" without a GIN scan, and lets the API surface it
--     directly in the response DTO.
--
-- Why payload is jsonb (not normalised columns):
--   * Each template has its own field set (sleeve_regime_loser has
--     7 fields; insufficient_data has only 2). Normalising would
--     produce a row-per-template-type table → a join per memory.
--   * jsonb GIN can still index specific fields if a future analytics
--     query needs to filter by sleeve / regime without table joins.
--   * Frontend deserialises directly into the template's data slot.
--
-- Versioning convention:
--   template_key SHOULD include a version suffix (".v1", ".v2") when
--   the payload schema changes (renamed field, removed field, changed
--   type). The frontend dictionary keeps both versions until the older
--   memories age out. Same key without a suffix = "the v1, never had
--   a schema change". See docs/I18N_TEMPLATE_VERSIONING.md.
--
-- Backfill:
--   We DO NOT backfill historic rows. The lesson_replay window is
--   30 days, so existing English-only rows will naturally age out of
--   the system. The frontend falls back to memories.content when
--   template_key is NULL — so legacy rows still display, just in
--   English. New rows start writing template_key + payload from this
--   migration onward.

ALTER TABLE memories
    ADD COLUMN IF NOT EXISTS template_key TEXT,
    ADD COLUMN IF NOT EXISTS payload      JSONB;

-- Soft constraint: template_key must look like "namespace.subtype[.vN]".
-- We enforce structure (lowercase dotted segments + optional version
-- suffix) rather than a closed enum so adding a new template doesn't
-- require a migration. Allows NULL for the legacy / non-AI-generated
-- rows that bypass the i18n pipeline.
ALTER TABLE memories
    ADD CONSTRAINT memories_template_key_shape
    CHECK (
        template_key IS NULL
        OR template_key ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+(\.v[0-9]+)?$'
    );

-- Hot read path: GET /api/funds/{id}/memory returns memories joined
-- to the locale renderer. We add an index that's CHEAP (partial, only
-- on rows that actually have a template_key) and pinpoints the rare
-- "list all rows of template X across funds" admin queries we'll
-- eventually need.
CREATE INDEX IF NOT EXISTS idx_memories_template_key
    ON memories (template_key)
    WHERE template_key IS NOT NULL;

COMMENT ON COLUMN memories.template_key IS
    'i18n template identifier, dotted lowercase + optional .vN suffix. NULL = legacy row, render via memories.content.';
COMMENT ON COLUMN memories.payload IS
    'Structured data the template interpolates (numbers, identifiers). Always english-locale-agnostic; locale formatting happens at render time.';
