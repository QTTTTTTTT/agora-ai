-- 086_backfill_attribution_i18n.sql — one-time, idempotent backfill of
-- template_key + payload for attribution lessons that were written
-- BEFORE migration 085 (the i18n pipeline) shipped.
--
-- Why we changed direction since 085's "do not backfill" comment:
--   The 085 plan assumed legacy English rows would naturally age out
--   of the 30-day replay window. In production we observed that the
--   StrategyAttributionPanel surfaces ALL historic attribution rows
--   for a fund (not just the rolling window), so a zh-CN user who
--   onboarded before the i18n pipeline keeps seeing English forever.
--   That's a UX bug we own — the dictionary is locale-complete, the
--   data just needs to catch up.
--
-- Approach:
--   * UPDATE only rows with layer='attribution' AND template_key IS NULL.
--   * Use regexp_match() against the deterministic title/body strings
--     emitted by lesson.go's fmt.Sprintf calls. Five known shapes:
--       - sleeve_regime_loser
--       - sleeve_regime_winner
--       - insufficient_data.watching          (with earliest_opened_at)
--       - insufficient_data.watching_no_date  (open lots, no date)
--       - insufficient_data.empty             (no closed, no open)
--   * Reconstruct the exact field set lessonRenderer expects (see
--     attribution/lesson.go::sleeveRegimePayload + dictionary contracts
--     in shared/api-client/src/i18n.ts attribution.lesson.* keys).
--   * Lossy fields documented inline below — round-tripping a 33%
--     win-rate string back to a float gives us 0.33, not the original
--     0.3333. We accept this since the UI re-renders win-rate to the
--     same precision (no closed feedback loop on the lossy bit).
--
-- Idempotency:
--   * The WHERE template_key IS NULL guard means re-running this is a
--     no-op. New rows from 085-onward are written WITH a template_key
--     by the Go pipeline directly.
--   * If a row's title doesn't match any of the five shapes (e.g. a
--     human-edited row, or a future template we haven't taught this
--     migration about), the CASE falls through and template_key stays
--     NULL — the UI will keep using its existing legacy fallback path.

WITH parsed AS (
    SELECT
        id,
        regexp_match(
            title,
            '^Sleeve "(.+?)" is losing money in regime "(.+?)" \((\d+) trades, win-rate (\d+)%, PnL (-?\d+\.\d+)\)$'
        ) AS loser_t,
        regexp_match(
            content,
            'avg pnl pct: (-?\d+\.\d+), avg holding (\d+\.\d+) days'
        ) AS body_avg,
        regexp_match(
            title,
            '^Sleeve "(.+?)" is profitable in regime "(.+?)" \((\d+) trades, win-rate (\d+)%, PnL \+(-?\d+\.\d+)\)$'
        ) AS winner_t,
        regexp_match(
            title,
            '^Watching (\d+) open (?:lot|lots) since (\d{4}-\d{2}-\d{2}) — no closed roundtrip in the last (\d+) days yet$'
        ) AS watch_dt_t,
        regexp_match(
            title,
            '^Watching (\d+) open (?:lot|lots) — no closed roundtrip in the last (\d+) days yet$'
        ) AS watch_nd_t,
        regexp_match(
            title,
            '^No closed trades in the last (\d+) days$'
        ) AS empty_t
    FROM memories
    WHERE layer = 'attribution'
      AND template_key IS NULL
)
UPDATE memories m
SET
    template_key = CASE
        WHEN p.loser_t IS NOT NULL    THEN 'attribution.lesson.sleeve_regime_loser'
        WHEN p.winner_t IS NOT NULL   THEN 'attribution.lesson.sleeve_regime_winner'
        WHEN p.watch_dt_t IS NOT NULL THEN 'attribution.lesson.insufficient_data.watching'
        WHEN p.watch_nd_t IS NOT NULL THEN 'attribution.lesson.insufficient_data.watching_no_date'
        WHEN p.empty_t IS NOT NULL    THEN 'attribution.lesson.insufficient_data.empty'
        ELSE m.template_key
    END,
    payload = CASE
        WHEN p.loser_t IS NOT NULL THEN jsonb_build_object(
            'sleeve',           p.loser_t[1],
            'regime',           p.loser_t[2],
            'trade_count',      (p.loser_t[3])::int,
            'win_rate',         (p.loser_t[4])::numeric / 100.0,
            'total_pnl',        (p.loser_t[5])::numeric,
            'avg_pnl_pct',      COALESCE((p.body_avg[1])::numeric, 0),
            'avg_holding_days', COALESCE((p.body_avg[2])::numeric, 0)
        )
        WHEN p.winner_t IS NOT NULL THEN jsonb_build_object(
            'sleeve',           p.winner_t[1],
            'regime',           p.winner_t[2],
            'trade_count',      (p.winner_t[3])::int,
            'win_rate',         (p.winner_t[4])::numeric / 100.0,
            'total_pnl',        (p.winner_t[5])::numeric,
            'avg_pnl_pct',      COALESCE((p.body_avg[1])::numeric, 0),
            'avg_holding_days', COALESCE((p.body_avg[2])::numeric, 0)
        )
        WHEN p.watch_dt_t IS NOT NULL THEN jsonb_build_object(
            'open_lot_count',     (p.watch_dt_t[1])::int,
            'earliest_opened_at', p.watch_dt_t[2],
            'window_days',        (p.watch_dt_t[3])::int
        )
        WHEN p.watch_nd_t IS NOT NULL THEN jsonb_build_object(
            'open_lot_count', (p.watch_nd_t[1])::int,
            'window_days',    (p.watch_nd_t[2])::int
        )
        WHEN p.empty_t IS NOT NULL THEN jsonb_build_object(
            'window_days', (p.empty_t[1])::int
        )
        ELSE m.payload
    END
FROM parsed p
WHERE m.id = p.id
  AND (
        p.loser_t    IS NOT NULL
     OR p.winner_t   IS NOT NULL
     OR p.watch_dt_t IS NOT NULL
     OR p.watch_nd_t IS NOT NULL
     OR p.empty_t    IS NOT NULL
  );

-- Sanity check: this NOTICE prints the residual count of attribution
-- rows still without a template_key. Operators eyeballing the deploy
-- log get an immediate "did the regex catch everything?" signal. A
-- non-zero count after this migration means a new lesson template was
-- added to lesson.go without updating this backfill — go teach the
-- regex about it before the next release.
DO $$
DECLARE
    residual INT;
BEGIN
    SELECT COUNT(*) INTO residual
    FROM memories
    WHERE layer = 'attribution' AND template_key IS NULL;
    RAISE NOTICE '086_backfill_attribution_i18n: % attribution rows still without template_key', residual;
END $$;
