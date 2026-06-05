-- manual_s12_pmpath_lotsize_cleanup_20260604.sql
--
-- One-off cleanup of the OCS fund (b8434d1c-…) STAR-Market
-- positions left behind by the PM-direct-fill bypass of
-- broker.LotSizeGate. See cmd/server/wiring_adapters.go →
-- runtimeTradingEngine.pmPathLotSizeGuard for the new
-- in-engine guard that prevents this from happening again.
--
-- Trigger story:
--   * The S12.1 broker-side LotSizeGate (cmd/server/lot_size_gate.go)
--     only fires for orders routed through broker.Simulator.
--     SubmitOrder.
--   * runtimeTradingEngine.executePlanAction has a "direct fill"
--     fast path (tradeRepoCreateAndFill) that inserts into
--     trade_executions and updates holding_positions without
--     touching the simulator. All 11 of OCS's filled rows have
--     broker_order_id IS NULL — they bypassed the gate.
--   * Result: STAR-Market 688205 / 688195 accumulated odd-lot
--     residuals from partial sells whose post-sell holding was
--     below the STAR MinLot=200 ("卖出余额不足 200 必须一次性
--     申报卖出" was never enforced).
--
-- Concrete bad fills (688205):
--   * 5/29  buy  209 @ 239.35   (legal STAR open)
--   * 6/01  sell  62 @ 237.41   (legal qty step but residual 147 < 200)
--   * 6/03  sell 104 @ 247.60   (legal qty step but residual  43 < 200)
--
-- After those two non-compliant sells, lot_ledger reconciled to
-- quantity_remaining = 43 on the 5/29 lot. But the
-- holding_positions row had drifted further (a separate
-- bookkeeping bug in the direct-fill path) and now reports
-- quantity = 105 — a number that doesn't match either:
--   * lot ledger truth (43), or
--   * trade-history arithmetic (209-62-104 = 43).
--
-- Cleanup strategy:
--   1. Trust the user-visible "holding_positions.quantity = 105"
--      figure for the cash-equivalent settlement (because that is
--      what the platform shows the operator and what NAV has been
--      accruing against).
--   2. Liquidate the 105-share residual at the latest cached price
--      (233.63 CNY, recorded as the current_price on the same row).
--      A "full-position sell" is ALWAYS legal under
--      instrument.NormalizeSellQty regardless of board MinLot — the
--      odd-lot rule only rejects partial sells.
--   3. Credit fund.current_capital with the net cash inflow.
--   4. Zero the holding_positions row + close the still-open lot
--      (quantity_remaining 43 → 0). This makes lot ledger and
--      holding consistent (both reflect "position fully closed").
--   5. Tag the two STAR-Market 6/01 + 6/03 sells with a lot-size
--      violation marker for auditability.
--
-- Idempotency:
--   * cash_ledger row guarded by a unique idempotency_key
--     ('s12_pmpath_cleanup_2026_06_04:OCS:688205').
--   * funds.current_capital + holding_positions update guarded by
--     the precondition quantity ≈ 105 (skips on re-apply).
--   * trade_executions tag guarded by NOT LIKE '%lot_size%'.
--   * admin_change_log uses a deterministic action+target_id;
--     re-running silently appends an identical row only if the
--     operator forces it (default behaviour is one row per
--     invocation, identical to the s12 cleanup template).

-- Transactional: a single BEGIN/COMMIT pair means any error in the
-- body rolls the whole cleanup back. The `\set ON_ERROR_STOP on`
-- pragma is a psql-specific meta-command that the Go pq driver
-- can't parse — keep this file portable across `psql` and the
-- in-process migration runner by relying on TRANSACTION semantics
-- alone.

BEGIN;

-- ------------------------------------------------------------------
-- A) Cash credit for the 105-share virtual liquidation.
--    Amounts in CNY:
--      gross_sell_notional    = 105 * 233.63 = 24531.1500
--      commission       (0.1%) =                  24.5312
--      stamp_tax       (0.1%)  =                  24.5312
--      transfer_fee   (0.002%) =                   0.4906
--      net_cash_credit         =               24481.5970
-- ------------------------------------------------------------------

INSERT INTO cash_ledger (
    fund_id, posted_at, trading_date, entry_type, amount, currency,
    description, metadata, idempotency_key
)
SELECT
    'b8434d1c-f2d1-4463-aac6-4631ec0bdbb9',
    NOW(), CURRENT_DATE, 'adjustment',
    24481.5970, 'CNY',
    'S12 pmpath cleanup: virtual liquidation of 688205 (SSE) 105-share residual from PM-direct-fill bypass of broker.LotSizeGate. Cash settled at the latest cached price 233.6300 CNY. Underlying lots/holdings zeroed; lot ledger quantity_remaining 43 → 0 to match.',
    jsonb_build_object(
      'cleanup_kind',           'pm_path_lot_size_residual',
      'instrument_key',         'SSE:688205',
      'residual_shares',        105,
      'reference_price_cny',    233.6300,
      'gross_notional_cny',     24531.1500,
      'commission_cny',         24.5312,
      'stamp_tax_cny',          24.5312,
      'transfer_fee_cny',       0.4906,
      'net_credit_cny',         24481.5970,
      'reason_code',            'pm_direct_fill_bypassed_lot_size_gate',
      'incident_window',        '2026-06-01..2026-06-03',
      'related_trades', jsonb_build_array(
          jsonb_build_object('id', 'f3c77d59-0eda-4e48-8cbc-a6405ce4115f', 'side', 'sell', 'qty', 62,  'when', '2026-06-01', 'residual_after_sell', 147),
          jsonb_build_object('id', '0fa2ef6d-f65f-46ae-96da-34d98eb01199', 'side', 'sell', 'qty', 104, 'when', '2026-06-03', 'residual_after_sell', 43)
      ),
      'fix_commits',            jsonb_build_array(
        'cmd/server/wiring_adapters.go - runtimeTradingEngine.pmPathLotSizeGuard (PM-path lot-size pre-flight)',
        'cmd/server/pmpath_lotsize_guard_test.go - 10 regression cases pinning STAR / ChiNext / SH-main behaviour'
      )
    ),
    's12_pmpath_cleanup_2026_06_04:OCS:688205'
WHERE NOT EXISTS (
    SELECT 1 FROM cash_ledger
    WHERE fund_id = 'b8434d1c-f2d1-4463-aac6-4631ec0bdbb9'
      AND idempotency_key = 's12_pmpath_cleanup_2026_06_04:OCS:688205'
);

-- ------------------------------------------------------------------
-- B) Apply the cash + holding + lot consequences. Guarded by the
--    precondition holding_positions.quantity ≈ 105.
-- ------------------------------------------------------------------

DO $$
DECLARE
    pre_qty  NUMERIC;
    pre_lot  NUMERIC;
BEGIN
    SELECT quantity INTO pre_qty
    FROM holding_positions
    WHERE fund_id       = 'b8434d1c-f2d1-4463-aac6-4631ec0bdbb9'
      AND instrument_key = 'SSE:688205';

    SELECT quantity_remaining INTO pre_lot
    FROM position_lots
    WHERE fund_id       = 'b8434d1c-f2d1-4463-aac6-4631ec0bdbb9'
      AND instrument_key = 'SSE:688205'
      AND quantity_remaining > 0
    ORDER BY opened_at DESC
    LIMIT 1;

    IF pre_qty IS NULL THEN
        RAISE NOTICE 'holding row gone; skipping (already cleaned)';
    ELSIF abs(pre_qty - 105) < 0.01 THEN
        UPDATE funds
           SET current_capital = current_capital + 24481.5970,
               total_assets    = total_assets    + 24481.5970,
               updated_at      = NOW()
         WHERE id = 'b8434d1c-f2d1-4463-aac6-4631ec0bdbb9';

        UPDATE holding_positions
           SET quantity      = 0,
               available_qty = 0,
               market_value  = 0,
               unrealized_pnl= 0,
               updated_at    = NOW()
         WHERE fund_id       = 'b8434d1c-f2d1-4463-aac6-4631ec0bdbb9'
           AND instrument_key = 'SSE:688205';

        UPDATE position_lots
           SET quantity_remaining = 0,
               status             = 'closed',
               closed_at          = NOW()
         WHERE fund_id       = 'b8434d1c-f2d1-4463-aac6-4631ec0bdbb9'
           AND instrument_key = 'SSE:688205'
           AND quantity_remaining > 0;

        RAISE NOTICE
            'fund credited +24481.5970 CNY; 688205 holding 105 → 0; lot remaining % → 0',
            COALESCE(pre_lot, 0);
    ELSE
        RAISE NOTICE
            'holding quantity = % (expected ~105) — already cleaned or unexpected; skipping',
            pre_qty;
    END IF;
END$$;

-- ------------------------------------------------------------------
-- C) Tag the two non-compliant STAR sells so a future audit can
--    find them. The 6/01 sell already carried a "legacy" marker
--    from the s12 cleanup migration; we append the new tag if not
--    yet present. The 6/03 sell is new and gets the full reason.
-- ------------------------------------------------------------------

UPDATE trade_executions
   SET cancel_reason = LEFT(
       COALESCE(NULLIF(cancel_reason, ''), '') || ' +pmpath_lot_size_bypass',
       64)
 WHERE id IN (
       'f3c77d59-0eda-4e48-8cbc-a6405ce4115f',   -- 6/01 sell 62
       '0fa2ef6d-f65f-46ae-96da-34d98eb01199'    -- 6/03 sell 104
   )
   AND (cancel_reason IS NULL OR cancel_reason NOT LIKE '%pmpath_lot_size%');

-- ------------------------------------------------------------------
-- D) Audit trail
-- ------------------------------------------------------------------

INSERT INTO admin_change_log (
    actor_user_id, action, target_type, target_id,
    before_snapshot, after_snapshot, metadata
) VALUES (
    '9c325e54-3b21-43b3-ab71-d26dcd343ea7',
    'pmpath_lotsize_residual_cleanup',
    'fund',
    'b8434d1c-f2d1-4463-aac6-4631ec0bdbb9',
    jsonb_build_object(
        'fund_id',                       'b8434d1c-f2d1-4463-aac6-4631ec0bdbb9',
        'fund_name',                     'OCS 主题精选 1 号',
        'instrument',                    'SSE:688205',
        'pre_holding_quantity',          105,
        'pre_holding_market_value_cny',  24531.15,
        'pre_lot_quantity_remaining',    43,
        'non_compliant_trades', jsonb_build_array(
            jsonb_build_object('id', 'f3c77d59-0eda-4e48-8cbc-a6405ce4115f', 'symbol', '688205', 'side', 'sell', 'qty', 62,  'residual', 147, 'board', 'star', 'min_lot', 200),
            jsonb_build_object('id', '0fa2ef6d-f65f-46ae-96da-34d98eb01199', 'symbol', '688205', 'side', 'sell', 'qty', 104, 'residual', 43,  'board', 'star', 'min_lot', 200)
        )
    ),
    jsonb_build_object(
        'fund_id',                       'b8434d1c-f2d1-4463-aac6-4631ec0bdbb9',
        'instrument',                    'SSE:688205',
        'post_holding_quantity',         0,
        'post_lot_quantity_remaining',   0,
        'cash_credit_cny',               24481.5970,
        'liquidation_price_cny',         233.6300
    ),
    jsonb_build_object(
        'incident_summary',
        'OCS fund 688205 (STAR) accumulated a 105-share residual via the PM-direct-fill path that bypassed broker.LotSizeGate. Two partial sells (62, 104) would have been rejected by an A-share venue under "卖出余额不足 200 必须一次性申报卖出" because they left residuals (147, 43) below STAR''s 200-share minimum. The new pmPathLotSizeGuard in runtimeTradingEngine.executePlanAction closes the gap. This row records the one-time cash-settled liquidation that zeroed the residual.',
        'fix_commits', jsonb_build_array(
            'cmd/server/wiring_adapters.go - runtimeTradingEngine.pmPathLotSizeGuard',
            'cmd/server/pmpath_lotsize_guard_test.go - regression suite (STAR / ChiNext / SH-main / non-A-share / no-position)'
        ),
        'related_runbook',
        'docs/PRICE_COLLAR_GATE.md / docs/MODEL_AB_AUTO_PROMOTION.md (sibling fast-path safety nets)'
    )
);

COMMIT;

-- Sanity check after re-running:
--   SELECT quantity, available_qty FROM holding_positions
--    WHERE fund_id='b8434d1c-f2d1-4463-aac6-4631ec0bdbb9'
--      AND instrument_key='SSE:688205';
--   SELECT quantity_remaining FROM position_lots
--    WHERE fund_id='b8434d1c-f2d1-4463-aac6-4631ec0bdbb9'
--      AND instrument_key='SSE:688205'
--      AND quantity_remaining > 0;       -- expect 0 rows
