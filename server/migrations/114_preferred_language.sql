-- 114_preferred_language.sql — full-stack English mode rollout.
--
-- Adds per-user and per-fund language preferences so:
--
--   1. The HTTP middleware no longer needs to guess from Accept-Language
--      after the first request — once the user picks "en-US" in the
--      preference dock, every subsequent request reads the persisted
--      value (and we still honour the X-User-Language header for the
--      same-tab override).
--
--   2. Background loops (A/B shadow learning, daily self-learning,
--      promotion scan, drawdown alarm, ...) have NO HTTP request and
--      therefore no header to read. They look up funds.preferred_language
--      (falling back to the owner's users.preferred_language) so the
--      lessons / reasoning_log entries they write match the language of
--      the people who will actually read them in the UI.
--
-- Schema notes
--
--   - users.preferred_language is NOT NULL with a 'zh-CN' default so we
--     don't break existing callers. The CHECK constraint pins the
--     enum at the DB level — that way a stray UPDATE with 'zh' or
--     'english' bounces back instead of silently producing rows the
--     i18n bundle can't translate.
--
--   - funds.preferred_language is nullable on purpose: NULL means
--     "inherit from fund.owner_id" so a freshly-created fund picks up
--     the owner's preference automatically, and a fund-specific
--     override (e.g. an English-language fund owned by a bilingual
--     operator) is opt-in rather than required at creation time.
--
--   - The partial index on funds is for the loop-scheduler queries that
--     pull only the en-US funds for the english-aware code paths once
--     we ship the feature flag. Until then it's idle but cheap.
--
-- Lock budget
--
-- ALTER TABLE ... ADD COLUMN with a non-volatile default is metadata-
-- only on PostgreSQL 11+, so this migration does not rewrite either
-- table. If you're on a fork/Aurora variant where the default-fill is
-- not metadata-only, run the up migration in three steps as documented
-- inline below.

-- Step 1 — add the columns. Metadata-only on PG 11+, so no full table
-- rewrite. The CHECK constraint is added in the same statement so we
-- never see a window where invalid values could be inserted.
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS preferred_language TEXT NOT NULL DEFAULT 'zh-CN'
        CHECK (preferred_language IN ('zh-CN','en-US'));

ALTER TABLE funds
    ADD COLUMN IF NOT EXISTS preferred_language TEXT NULL
        CHECK (preferred_language IS NULL OR preferred_language IN ('zh-CN','en-US'));

-- Step 2 — partial index for loop scans that filter by language.
-- Funds with NULL preferred_language fall through to owner lookup, so
-- excluding them from the index keeps it small.
CREATE INDEX IF NOT EXISTS idx_funds_preferred_language
    ON funds (preferred_language)
    WHERE preferred_language IS NOT NULL;

-- Step 3 — comment for forensics. Future operators reading \d users in
-- psql get a quick pointer to the i18nmsg package that consumes this.
COMMENT ON COLUMN users.preferred_language IS
    'BCP-47 locale honoured by the i18nmsg bundle and the X-User-Language fallback chain.';
COMMENT ON COLUMN funds.preferred_language IS
    'Per-fund override; NULL inherits funds.owner_id -> users.preferred_language.';
