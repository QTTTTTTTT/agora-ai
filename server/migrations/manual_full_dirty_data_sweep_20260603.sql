-- manual_full_dirty_data_sweep_20260603.sql
--
-- Phase 2 of the 2026-06-03 incident response — full-table sweep after
-- S12 lot-size gate + corpaction whole-share fix landed.
--
-- Phase 1 (manual_reversal_erroneous_fills_20260603.sql +
-- manual_s12_lotsize_cleanup_20260603.sql) covered the two
-- already-known fact patterns:
--   * 301308 buy 1 @ 96,226.4188 + 301308 sell 1 @ 519.50 fat-finger
--   * 4 lot-size-violation legacy trades + 688195 0.6-share residual
--
-- This script extends the sweep with the three additional dirty-data
-- vectors the post-Phase-1 audit revealed:
--
--   A) THREE further out-of-hours filled trades that slipped past
--      the EMPTY trading_calendar gate (the same fail-open root cause
--      that produced the 06-03 09:01:20 301308 sell). They are
--      cancel_reason-tagged only (ledger NOT touched) because:
--        - 300475 buy/sell round trip is already closed (5/27 buy +
--          5/29 sell, net flat) so reversing the buy would force a
--          retroactive reversal of the sell too;
--        - 688205 sell 207 on 05-27 09:19:40 is in a still-open
--          position chain (cur 105 shares) and reversing it would
--          require unwinding three downstream trades; the better
--          posture is to tag-and-document so the audit trail is
--          honest while the bookkeeping stays stable.
--      The same ledger_only philosophy the operator picked for
--      Phase 1's STAR partial-sell legacy trades applies here.
--
--   B) ONE duplicate corp-action import on SSE:688195. The
--      eastmoney + tushare adapters both wrote a 1.4x split +
--      0.164 cash dividend with the same ex_date (2026-05-29),
--      producing TWO distinct corporate_actions rows
--      (287124ed and c1048c06). corpaction.applier ran on both
--      (different corp_action_id => the ON CONFLICT idempotency
--      guard on corp_action_applications never fires), which:
--        - left holding_positions.quantity correct (the applier
--          re-runs are idempotent on quantity when the same
--          pre/post is computed twice — second run sees the same
--          post value), BUT
--        - credited funds.current_capital with cash_credit 47.396
--          TWICE.
--      The funds.current_capital double-credit IS real money; this
--      script reverses one of the two by:
--        1. Deleting the second corp_action_applications row
--           (the c1048c06 application, which is the duplicate);
--        2. Marking SSE:688195 corp event c1048c06 in
--           corporate_actions.notes as superseded (kept in-place
--           so the audit trail of the duplicate import is not
--           lost — the duplicate row stays in corporate_actions
--           with a "superseded by 287124ed" note);
--        3. Debiting funds.current_capital by 47.3960 CNY;
--        4. Writing a cash_ledger reversal row (entry_type
--           'reversal', amount 47.3960) with full forensics in
--           description/metadata.
--      The 287124ed application row stays — it is the canonical
--      first application of the legitimate corp event.
--
--   C) NEW invariant the application code MUST enforce going
--      forward (deferred to a separate code commit, called out in
--      admin_change_log so it lives in operational memory):
--        "corporate_actions UNIQUE on (instrument_key, ex_date,
--         action_type, split_ratio, cash_dividend) to prevent
--         multi-source duplicate inserts." A DB-level UNIQUE
--         index would have made Phase B impossible.
--
-- All four operations are wrapped in DO $$ idempotency blocks so
-- re-running this migration is a no-op once it has landed.

BEGIN;

-- =========================================================================
-- A) Three out-of-hours filled trades — cancel_reason tag only.
-- =========================================================================

-- A1: 300475 buy 400 @ 202.00 at 2026-05-27 09:19:48 CST.
--     09:19:48 is before the 09:25 集合竞价 freeze and well before
--     the 09:30 continuous-auction open — no real fill possible.
--     The matching 5/29 10:05 sell 400 has already closed the round
--     trip so we cannot retroactively reverse this without unbalancing
--     the sell side.
UPDATE trade_executions
   SET cancel_reason = COALESCE(NULLIF(cancel_reason, ''), '') ||
                       CASE WHEN cancel_reason IS NULL OR cancel_reason = '' THEN '' ELSE ' +' END ||
                       'out_of_hours_legacy:pre_calendar_seed_fail_open'
 WHERE id = '6acc1677-2fd0-4b74-9930-b314b4ed63d0'
   AND COALESCE(cancel_reason, '') NOT ILIKE '%out_of_hours_legacy%';

-- A2: 688205 sell 207 @ 240.00 at 2026-05-27 09:19:40 CST.
--     Same pre-09:30 violation as A1. The downstream chain
--     (5/27 14:12 buy 207 @ 241.47, 5/29 11:08 buy 209, 6/1 sell 62,
--     6/3 sell 104) makes reversal of just this one trade non-trivial,
--     so we tag-only.
UPDATE trade_executions
   SET cancel_reason = COALESCE(NULLIF(cancel_reason, ''), '') ||
                       CASE WHEN cancel_reason IS NULL OR cancel_reason = '' THEN '' ELSE ' +' END ||
                       'out_of_hours_legacy:pre_calendar_seed_fail_open'
 WHERE id = 'a150c277-8efd-43ba-8aae-2323423a2b0e'
   AND COALESCE(cancel_reason, '') NOT ILIKE '%out_of_hours_legacy%';

-- A3: 688205 buy 393 @ 253.92 at 2026-05-20 11:46:28 CST.
--     11:46 is in the 11:30-13:00 midday recess. The matching
--     5/22 sell 393 closed the round trip the same week.
UPDATE trade_executions
   SET cancel_reason = COALESCE(NULLIF(cancel_reason, ''), '') ||
                       CASE WHEN cancel_reason IS NULL OR cancel_reason = '' THEN '' ELSE ' +' END ||
                       'out_of_hours_legacy:midday_recess_pre_calendar_seed'
 WHERE id = 'a6a6431e-2315-4c75-88b0-f50ef92856b7'
   AND COALESCE(cancel_reason, '') NOT ILIKE '%out_of_hours_legacy%';

-- =========================================================================
-- B) Duplicate corp-action reversal on SSE:688195 / fund b8434d1c.
--    Both 287124ed and c1048c06 carry the same ex_date / split / cash.
--    Keep 287124ed as canonical; reverse the c1048c06 application.
-- =========================================================================

DO $$
DECLARE
    v_fund_id           UUID := 'b8434d1c-f2d1-4463-aac6-4631ec0bdbb9';
    v_dup_corp_id       UUID := 'c1048c06-b6ec-44ab-bb94-436df42d7021';
    v_canonical_corp_id UUID := '287124ed-6476-4a1c-8c89-e4cb8e7f7950';
    v_dup_cash_credit   NUMERIC(20,4) := 47.3960;
    v_dup_app_exists    BOOLEAN := FALSE;
BEGIN
    -- Idempotency guard: only do the work if the duplicate
    -- application row is still present.
    SELECT EXISTS (
        SELECT 1 FROM corp_action_applications
         WHERE corp_action_id = v_dup_corp_id AND fund_id = v_fund_id
    ) INTO v_dup_app_exists;

    IF NOT v_dup_app_exists THEN
        RAISE NOTICE '688195 corp dup already cleaned, skipping B';
        RETURN;
    END IF;

    -- B1) Delete the duplicate application row.
    DELETE FROM corp_action_applications
     WHERE corp_action_id = v_dup_corp_id AND fund_id = v_fund_id;

    -- B2) Mark the duplicate corporate_actions row as superseded.
    --     We keep the row (no DELETE) so the audit trail of the
    --     dupe import remains queryable.
    UPDATE corporate_actions
       SET notes = COALESCE(notes, '') || E'\n[SUPERSEDED 2026-06-03] duplicate import of 287124ed (same ex_date / split_ratio / cash_dividend). Application reversed in manual_full_dirty_data_sweep_20260603.sql.',
           recorded_at = recorded_at
     WHERE id = v_dup_corp_id
       AND COALESCE(notes, '') NOT ILIKE '%SUPERSEDED%';

    -- B3) Debit funds.current_capital to undo the duplicate
    --     cash_credit. Sign: applier added cash_credit, we subtract.
    UPDATE funds
       SET current_capital = current_capital - v_dup_cash_credit,
           updated_at      = NOW()
     WHERE id = v_fund_id;

    -- B4) Reflect the debit in cash_ledger so the journal stays
    --     in lock-step with funds.current_capital. amount is
    --     negative (cash out) per cash_ledger convention.
    --     Manual idempotency check (the unique index on
    --     (fund_id, idempotency_key) is partial WHERE
    --     idempotency_key IS NOT NULL, so it cannot serve as an
    --     ON CONFLICT target — we guard with NOT EXISTS instead).
    INSERT INTO cash_ledger (
        fund_id, posted_at, trading_date, entry_type, amount, currency,
        corp_action_id, description, metadata, idempotency_key
    )
    SELECT
        v_fund_id, NOW(), CURRENT_DATE, 'reversal', -v_dup_cash_credit, 'CNY',
        v_canonical_corp_id,
        'Sweep 2026-06-03: reverse duplicate corp_action_applications row for SSE:688195 ex 2026-05-29 (dup id ' || v_dup_corp_id::text || ', kept canonical ' || v_canonical_corp_id::text || ').',
        jsonb_build_object(
            'sweep',          'manual_full_dirty_data_sweep_20260603',
            'phase',          'B',
            'instrument_key', 'SSE:688195',
            'symbol',         '688195',
            'ex_date',        '2026-05-29',
            'dup_corp_id',    v_dup_corp_id,
            'canonical_corp_id', v_canonical_corp_id,
            'cash_credit',    v_dup_cash_credit,
            'reason',         'duplicate_corp_action_import_eastmoney_and_tushare_both_wrote',
            'invariant_gap',  'corporate_actions lacks UNIQUE (instrument_key, ex_date, action_type, split_ratio, cash_dividend)'
        ),
        'sweep:20260603:corp_dup:' || v_dup_corp_id::text || ':' || v_fund_id::text
    WHERE NOT EXISTS (
        SELECT 1 FROM cash_ledger
         WHERE fund_id = v_fund_id
           AND idempotency_key = 'sweep:20260603:corp_dup:' || v_dup_corp_id::text || ':' || v_fund_id::text
    );

    RAISE NOTICE 'B done: dup corp_action_applications deleted, funds debited %, cash_ledger reversal written',
        v_dup_cash_credit;
END $$;

-- =========================================================================
-- C) Audit-log everything so the operator sees one row per sweep.
-- =========================================================================

-- Idempotency: admin_change_log has no natural key on (action, target_id),
-- so we guard with NOT EXISTS to avoid duplicate rows on re-run.
INSERT INTO admin_change_log (
    actor_user_id, action, target_type, target_id, metadata
)
SELECT
    '9c325e54-3b21-43b3-ab71-d26dcd343ea7',  -- same system-operator UUID used by Phase 1
    'manual_full_dirty_data_sweep',
    'system',
    'manual_full_dirty_data_sweep_20260603',
    jsonb_build_object(
        'sweep_script',     'server/migrations/manual_full_dirty_data_sweep_20260603.sql',
        'incident_date',    '2026-06-03',
        'phase',            '2_of_2',
        'phase_1_scripts',  jsonb_build_array(
            'server/migrations/manual_reversal_erroneous_fills_20260603.sql',
            'server/migrations/manual_s12_lotsize_cleanup_20260603.sql'
        ),
        'A_out_of_hours_tagged', jsonb_build_array(
            jsonb_build_object(
                'trade_id', '6acc1677-2fd0-4b74-9930-b314b4ed63d0',
                'symbol',   '300475',
                'side',     'buy',
                'qty',      400,
                'price',    202.00,
                'cst',      '2026-05-27 09:19:48',
                'reason',   'pre_calendar_seed_fail_open_before_0930',
                'round_trip_closed_by', '747e0dc6-6b03-471c-9491-55f2561941f8'
            ),
            jsonb_build_object(
                'trade_id', 'a150c277-8efd-43ba-8aae-2323423a2b0e',
                'symbol',   '688205',
                'side',     'sell',
                'qty',      207,
                'price',    240.00,
                'cst',      '2026-05-27 09:19:40',
                'reason',   'pre_calendar_seed_fail_open_before_0930'
            ),
            jsonb_build_object(
                'trade_id', 'a6a6431e-2315-4c75-88b0-f50ef92856b7',
                'symbol',   '688205',
                'side',     'buy',
                'qty',      393,
                'price',    253.92,
                'cst',      '2026-05-20 11:46:28',
                'reason',   'midday_recess_pre_calendar_seed'
            )
        ),
        'B_corp_dup_reversed', jsonb_build_object(
            'instrument_key',    'SSE:688195',
            'symbol',            '688195',
            'fund_id',           'b8434d1c-f2d1-4463-aac6-4631ec0bdbb9',
            'ex_date',           '2026-05-29',
            'canonical_corp_id', '287124ed-6476-4a1c-8c89-e4cb8e7f7950',
            'dup_corp_id',       'c1048c06-b6ec-44ab-bb94-436df42d7021',
            'reversed_cash_cny', 47.3960,
            'mechanism',         'DELETE corp_action_applications + UPDATE corporate_actions notes superseded + UPDATE funds.current_capital -47.3960 + INSERT cash_ledger reversal'
        ),
        'follow_up_invariants', jsonb_build_array(
            'ADD UNIQUE INDEX ON corporate_actions (instrument_key, ex_date, action_type, split_ratio, cash_dividend) — schema-level guard against multi-source dupe imports.',
            'corpaction.applier semantic-dedup: when ApplyEvent sees an event whose (ex_date, instrument_key, action_type, split_ratio, cash_dividend) matches a previously-applied event for the same fund, treat as no-op even if corp_action_id differs.',
            'trading_calendar gate must FAIL CLOSED when the calendar row count for the symbol''s market is zero (Phase 1 fixed by seeding through 2027; the code-level fix-closed is the durable belt-and-braces).',
            'monthly dirty-data scan cron — replicate this script''s D1-D4 queries as a Prometheus alert (fundai_dirty_data_count{kind="fractional_holding|lot_size_violation|out_of_hours|price_outlier"}).'
        )
    )
WHERE NOT EXISTS (
    SELECT 1 FROM admin_change_log
     WHERE action    = 'manual_full_dirty_data_sweep'
       AND target_id = 'manual_full_dirty_data_sweep_20260603'
);

-- Cleanup any duplicate admin_change_log rows that may have been
-- written by an earlier non-idempotent revision of this script —
-- keep the OLDEST row (canonical first run), delete the rest.
DELETE FROM admin_change_log a
 USING (
    SELECT id FROM admin_change_log
     WHERE action    = 'manual_full_dirty_data_sweep'
       AND target_id = 'manual_full_dirty_data_sweep_20260603'
     ORDER BY created_at ASC
     OFFSET 1
 ) dup
 WHERE a.id = dup.id;

COMMIT;

-- Post-sweep sanity:
--   1. SELECT * FROM holding_positions WHERE quantity != FLOOR(quantity)
--        AND instrument_key ~ '^(SSE|SZSE|BSE|HKEX):' — expected zero rows.
--   2. SELECT id FROM trade_executions
--        WHERE filled_qty > 0
--          AND (instrument_key LIKE 'SSE:%' OR instrument_key LIKE 'SZSE:%' OR instrument_key LIKE 'BSE:%')
--          AND (
--            EXTRACT(HOUR FROM executed_at AT TIME ZONE 'Asia/Shanghai') * 100
--            + EXTRACT(MINUTE FROM executed_at AT TIME ZONE 'Asia/Shanghai') < 930
--          )
--          AND cancel_reason NOT ILIKE '%out_of_hours%' — expected zero rows.
--   3. SELECT corp_action_id, fund_id, count(*) FROM corp_action_applications
--        GROUP BY 1, 2 HAVING count(*) > 1 — expected zero rows (PK guarantees,
--        but the *cross-corp_action_id* dupe pattern needs the future UNIQUE
--        index on corporate_actions).
--   4. SELECT current_capital FROM funds WHERE id = 'b8434d1c-f2d1-4463-aac6-4631ec0bdbb9'
--        — should be 47.3960 CNY less than pre-sweep snapshot.
