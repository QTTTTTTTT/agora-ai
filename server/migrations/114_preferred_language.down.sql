-- 114_preferred_language.down.sql — drop user/fund language preferences.
--
-- Rolling this back loses every user/fund language override that was
-- persisted while the column existed. The X-User-Language header path
-- still works for in-flight requests, but background loops will revert
-- to the historical zh-CN-only behaviour.
--
-- Order matters: drop the partial index before the column it references,
-- and drop the funds column before the users column so anything that
-- joined the two during the rollback window degrades cleanly.

DROP INDEX IF EXISTS idx_funds_preferred_language;
ALTER TABLE funds DROP COLUMN IF EXISTS preferred_language;
ALTER TABLE users DROP COLUMN IF EXISTS preferred_language;
