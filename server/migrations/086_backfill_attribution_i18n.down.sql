-- 086_backfill_attribution_i18n.down.sql — undo the i18n backfill.
--
-- This is a data-only down: 085 added the columns + index + check.
-- 086 only POPULATED template_key + payload for the five known
-- attribution lesson shapes. To revert, we re-NULL those rows so the
-- frontend falls back to the legacy English title/content path again.
--
-- We scope the revert by template_key (not just layer='attribution')
-- so that rows the BACKFILL did not touch — including any future
-- attribution rows that came in via the live i18n pipeline AFTER 086
-- ran but before the down migration was triggered — get the same
-- "back to legacy" treatment. If you want to keep live-pipeline rows
-- as-is, run a more selective revert by inspecting created_at against
-- the deploy timestamp of 086. We do not implement that here because
-- the down path is only used in test rollbacks and disaster recovery
-- where "force everything back to legacy" is what we want.

UPDATE memories
SET
    template_key = NULL,
    payload      = NULL
WHERE layer = 'attribution'
  AND template_key IN (
        'attribution.lesson.sleeve_regime_loser',
        'attribution.lesson.sleeve_regime_winner',
        'attribution.lesson.insufficient_data.watching',
        'attribution.lesson.insufficient_data.watching_no_date',
        'attribution.lesson.insufficient_data.empty'
  );
