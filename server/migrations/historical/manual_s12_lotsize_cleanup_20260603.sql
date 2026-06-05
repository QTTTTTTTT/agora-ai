-- manual_s12_lotsize_cleanup_20260603.sql
--
-- One-off clean-up of lot-size & corp-action residual dirty data
-- found by the 2026-06-03 S12 audit. Two unrelated incidents folded
-- into one transaction so the cash effect lands atomically.
--
--   A) Fund b8434d1c-f2d1-4463-aac6-4631ec0bdbb9 ("OCS 资本 精选基金")
--      holds 0.6000 shares of 688195 (SSE:688195) — a fractional
--      residual from the pre-S12.2 corp-action applier bug. The
--      applier scaled 289 × 1.4 = 404.6 straight into
--      holding_positions, then a sequence of integer sells left a
--      0.6-share residual that no A-share venue can route. We
--      cash-settle at the current_price 244.3600 (≈ 146.6160 CNY)
--      and zero the holding.
--
--   B) Four historical bad trades — pre-S12.1 lot-size violations
--      that the broker simulator accepted because no broker-side
--      gate existed:
--
--        301308 BUY  1 share  (ChiNext min 100)  — fund aa11aa11...
--        301308 SELL 1 share  (post-bad-buy)     — fund aa11aa11...
--        688195 SELL 85 shares (STAR; legal)     — already covered by
--                                                   the fund's 404.6
--                                                   fractional holding,
--                                                   no cash to reverse
--        688205 SELL 62 shares (STAR; legal)     — same
--
--      The two 301308 trades were already cash-reversed on
--      2026-06-03 via manual_reversal_erroneous_fills_20260603.sql
--      (cash_ledger reversal entries + cancel_reason annotations).
--      Here we add an additional lot-size-violation tag to all four
--      so the audit chain reflects BOTH the price-collar reason
--      AND the lot-size reason. trade_executions are NOT cash-
--      reversed in this script.
--
-- Method (per user choice "ledger_only" carried over from the
-- 2026-06-03 reversal):
--   * Holding is zeroed in-place; market_value / unrealized_pnl
--     follow. No trade_executions row is synthesised — the residual
--     is a corp-action artefact, not a trade.
--   * Cash residual is booked into cash_ledger with idempotency
--     key 's12_corp_residual:SSE:688195:b8434d1c-...' so a re-run
--     is a no-op.
--   * The four historical bad trades get their cancel_reason
--     APPENDED to (existing reason kept if any) so both root causes
--     are visible to operators.
--   * One admin_change_log row captures the operator action.
--
-- Idempotency: cash_ledger insert uses WHERE NOT EXISTS on the
-- idempotency_key; holding update is guarded against re-running by
-- the `quantity = 0.6` precondition (a second apply finds
-- quantity = 0 and skips); trade_executions UPDATE uses
-- WHERE cancel_reason NOT LIKE '%lot_size_violation_legacy%'.

BEGIN;

-- ---------------------------------------------------------------
-- A) 688195 fractional residual → cash + zero the holding
-- ---------------------------------------------------------------

INSERT INTO cash_ledger (
    fund_id, posted_at, trading_date, entry_type, amount, currency,
    description, metadata, idempotency_key
)
SELECT
    'b8434d1c-f2d1-4463-aac6-4631ec0bdbb9',
    NOW(), CURRENT_DATE, 'reversal',
    146.6160, 'CNY',
    'S12 cleanup: 688195 (SSE) 0.6000-share fractional residual from pre-S12.2 corp-action applier (289 × 1.4 = 404.6 written directly to holding_positions; cash-settled at current_price 244.3600). Holding zeroed.',
    jsonb_build_object(
      'cleanup_kind',           'corp_action_fractional_residual',
      'instrument_key',         'SSE:688195',
      'residual_shares',        0.6,
      'reference_price_cny',    244.36,
      'reason_code',            'whole_share_settlement_not_applied',
      'incident_date',          '2026-05-29',
      'fix_commits',            jsonb_build_array(
        'internal/corpaction/applier.go - settlement_mode_for + whole_shares branch',
        'internal/broker/simulator.go - WithLotSizeGate option (S12.1)',
        'internal/lotsizegate/* - cross-market lot-size engine (S12.1)',
        'cmd/server/lot_size_gate.go - production wiring with positions repo (S12.1)',
        'migrations/080_instrument_metadata.sql - HK + crypto + futures spec table (S12.3)'
      )
    ),
    's12_corp_residual:SSE:688195:b8434d1c-f2d1-4463-aac6-4631ec0bdbb9'
WHERE NOT EXISTS (
    SELECT 1 FROM cash_ledger
    WHERE fund_id = 'b8434d1c-f2d1-4463-aac6-4631ec0bdbb9'
      AND idempotency_key = 's12_corp_residual:SSE:688195:b8434d1c-f2d1-4463-aac6-4631ec0bdbb9'
);

-- Funds.current_capital follows the cash credit. Guard against a
-- second apply by requiring holding_positions.quantity to be the
-- pre-cleanup 0.6.
DO $$
DECLARE
    pre_qty NUMERIC;
BEGIN
    SELECT quantity INTO pre_qty
    FROM holding_positions
    WHERE fund_id = 'b8434d1c-f2d1-4463-aac6-4631ec0bdbb9'
      AND instrument_key = 'SSE:688195';

    IF pre_qty IS NULL THEN
        RAISE NOTICE 'holding row gone; skipping update (already cleaned)';
    ELSIF abs(pre_qty - 0.6) < 0.001 THEN
        UPDATE funds
           SET current_capital = current_capital + 146.6160,
               total_assets    = total_assets    + 146.6160,
               updated_at      = NOW()
         WHERE id = 'b8434d1c-f2d1-4463-aac6-4631ec0bdbb9';
        UPDATE holding_positions
           SET quantity      = 0,
               available_qty = 0,
               market_value  = 0,
               unrealized_pnl= 0,
               updated_at    = NOW()
         WHERE fund_id = 'b8434d1c-f2d1-4463-aac6-4631ec0bdbb9'
           AND instrument_key = 'SSE:688195';
        RAISE NOTICE 'fund credited +146.6160 CNY, holding 688195 zeroed (was %)', pre_qty;
    ELSE
        RAISE NOTICE 'holding quantity = % (expected 0.6) — already cleaned or unexpected; skipping', pre_qty;
    END IF;
END$$;

-- ---------------------------------------------------------------
-- B) Annotate the four pre-S12.1 lot-size violation trades
--    (cancel_reason column is varchar(64), so we use short tags
--    and refer the operator to the admin_change_log row for
--    full narrative).
-- ---------------------------------------------------------------

-- 301308 buy 1 share (already has cancel_reason from
-- manual_reversal_erroneous_fills_20260603.sql); append the
-- lot-size tag if not already present.
UPDATE trade_executions
   SET cancel_reason = LEFT(
       COALESCE(NULLIF(cancel_reason, ''), '') || ' +lot_size_violation_legacy',
       64)
 WHERE id = 'b5e3405a-90fb-4bc8-a51a-9c737b364231'
   AND cancel_reason NOT LIKE '%lot_size%';

UPDATE trade_executions
   SET cancel_reason = LEFT(
       COALESCE(NULLIF(cancel_reason, ''), '') || ' +lot_size_violation_legacy',
       64)
 WHERE id = '55923b11-d6bd-4c7f-aa54-0629d2b363bc'
   AND cancel_reason NOT LIKE '%lot_size%';

-- 688195 sell 85, 688205 sell 62: these are STAR-board sells where
-- the post-sell residual was legal given the pre-S12.2 fractional
-- holding (404.6 / 209) but would not have been routed by any real
-- A-share venue. Pin the lot-size violation tag for auditability.
UPDATE trade_executions
   SET cancel_reason = LEFT(
       COALESCE(NULLIF(cancel_reason, ''), '') || 'lot_size_violation_legacy:STAR_partial_below_min_legal_due_to_corp_action_residual',
       64)
 WHERE id = '4ea322ee-6c4b-4f1a-b98c-3bcd8397b468'
   AND cancel_reason IS DISTINCT FROM 'lot_size_violation_legacy:STAR_partial_below_min_legal_due_to_corp_action_residual';

UPDATE trade_executions
   SET cancel_reason = LEFT(
       COALESCE(NULLIF(cancel_reason, ''), '') || 'lot_size_violation_legacy:STAR_partial_below_min_legal_due_to_corp_action_residual',
       64)
 WHERE id = 'f3c77d59-0eda-4e48-8cbc-a6405ce4115f'
   AND cancel_reason IS DISTINCT FROM 'lot_size_violation_legacy:STAR_partial_below_min_legal_due_to_corp_action_residual';

-- ---------------------------------------------------------------
-- C) Audit trail in admin_change_log (one row covering both A + B)
-- ---------------------------------------------------------------

INSERT INTO admin_change_log (
    actor_user_id, action, target_type, target_id,
    before_snapshot, after_snapshot, metadata
) VALUES (
    '9c325e54-3b21-43b3-ab71-d26dcd343ea7',
    'lot_size_dirty_data_cleanup',
    'platform',
    's12_cleanup_20260603',
    jsonb_build_object(
        'fund_b8434d1c_688195_holding_qty', 0.6,
        'fund_b8434d1c_688195_market_value', 146.616,
        'historical_bad_trades', jsonb_build_array(
            jsonb_build_object('id', 'b5e3405a-90fb-4bc8-a51a-9c737b364231', 'symbol', '301308', 'side', 'buy',  'qty', 1,   'board', 'chinext', 'min_lot', 100),
            jsonb_build_object('id', '55923b11-d6bd-4c7f-aa54-0629d2b363bc', 'symbol', '301308', 'side', 'sell', 'qty', 1,   'board', 'chinext', 'min_lot', 100),
            jsonb_build_object('id', '4ea322ee-6c4b-4f1a-b98c-3bcd8397b468', 'symbol', '688195', 'side', 'sell', 'qty', 85,  'board', 'star',    'min_lot', 200),
            jsonb_build_object('id', 'f3c77d59-0eda-4e48-8cbc-a6405ce4115f', 'symbol', '688205', 'side', 'sell', 'qty', 62,  'board', 'star',    'min_lot', 200)
        )
    ),
    jsonb_build_object(
        'fund_b8434d1c_688195_holding_qty', 0,
        'fund_b8434d1c_688195_cash_credit_cny', 146.616,
        'cleanup_method', 'cash_ledger_residual+holding_zero+cancel_reason_tag',
        'reversal_entries', 1,
        'annotated_trades', 4
    ),
    jsonb_build_object(
        'incident_summary', 'S12 audit (2026-06-03) found 4 historical lot-size violation trades and 1 fractional-share corp-action residual. Pre-S12.1 the broker simulator had no LotSizeGate so quantities below board minimums (ChiNext 100, STAR 200) were filled. Pre-S12.2 the corpaction applier scaled 289 × 1.4 = 404.6 straight into holding_positions instead of distributing 115 whole bonus shares + 0.6-share cash residual.',
        'code_fixes_shipped', jsonb_build_array(
            'internal/broker/broker.go - ErrLotSizeRejected',
            'internal/broker/simulator.go - WithLotSizeGate + LotSizeProbe/Verdict (S12.1)',
            'internal/lotsizegate/* - new cross-market engine + DefaultSpecSource (S12.1)',
            'cmd/server/lot_size_gate.go - production wiring with dbHKLotResolver + dbCryptoStepResolver + dbHoldingQty (S12.1)',
            'cmd/server/main.go - lotSizeEvents metric + RecordLotSizeEvent + Prometheus export (S12.1)',
            'internal/corpaction/applier.go - settlementModeFor + whole_shares branch with residual cash_ledger leg (S12.2)',
            'migrations/080_instrument_metadata.sql - HKEX + crypto + futures spec table (S12.3)',
            'internal/repository/instrument_metadata_repo.go - Get / Upsert / ListByMarket (S12.3)'
        ),
        'tests_added', jsonb_build_array(
            'internal/lotsizegate/engine_test.go (25+ cases covering A-share boards, HK default, US fractional, crypto step, futures hands)',
            'internal/broker/simulator_lotsize_test.go (gate ordering, regression for 301308 1-share buy, suggestion propagation)',
            'internal/corpaction/applier_test.go - TestApplyEvent_HappyPath_TenSongSi_WholeSharesPlusResidual + TestApplyEvent_FractionalSettlement_NASDAQSplit'
        ),
        'cleanup_method',       'ledger_only_with_holding_zero_and_cancel_reason_tag',
        'requested_by',         'tong',
        'authorised_by_user',   '9c325e54-3b21-43b3-ab71-d26dcd343ea7'
    )
);

COMMIT;

-- Post-condition sanity checks (read-only):
-- SELECT instrument_key, quantity, market_value FROM holding_positions WHERE fund_id='b8434d1c-f2d1-4463-aac6-4631ec0bdbb9' AND instrument_key='SSE:688195';
-- SELECT id, current_capital FROM funds WHERE id='b8434d1c-f2d1-4463-aac6-4631ec0bdbb9';
-- SELECT idempotency_key, amount, description FROM cash_ledger WHERE fund_id='b8434d1c-f2d1-4463-aac6-4631ec0bdbb9' AND idempotency_key LIKE 's12_corp_residual%';
-- SELECT id, side, symbol, quantity, cancel_reason FROM trade_executions WHERE id IN ('b5e3405a-90fb-4bc8-a51a-9c737b364231','55923b11-d6bd-4c7f-aa54-0629d2b363bc','4ea322ee-6c4b-4f1a-b98c-3bcd8397b468','f3c77d59-0eda-4e48-8cbc-a6405ce4115f');
-- SELECT action, target_id, before_snapshot, after_snapshot FROM admin_change_log WHERE action='lot_size_dirty_data_cleanup' ORDER BY created_at DESC LIMIT 1;
