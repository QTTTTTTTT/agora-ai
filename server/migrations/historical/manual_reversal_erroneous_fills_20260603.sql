-- manual_reversal_erroneous_fills_20260603.sql
--
-- One-off trade-break reversal for two erroneous A-share fills on
-- fund 'aa11aa11-aa11-aa11-aa11-aa11aa11aa11' (存储基金):
--
--   1) 2026-06-02 06:34:17 UTC — buy 301308 @ 96,226.4188 CNY/share
--      Root cause: PM quote-unavailable fallback stamped the notional
--      buy budget into PlanAction.Price with Quantity=1. The broker
--      simulator honoured the resulting 96,226 CNY limit and filled.
--      True intraday mid for 301308 (江南奕帆, ChiNext) was ~500 CNY.
--      Code fix shipped 2026-06-03: wiring_adapters.go:14741, :16856
--      downgrade quote-unavailable to Action="watch"; new
--      broker-side price-collar gate (internal/pricecollar) rejects
--      future limits that deviate >21% from a recent reference quote
--      on ChiNext / STAR / BSE (>11% on A-share main board).
--
--   2) 2026-06-03 01:01:20 UTC = 09:01:20 CST — sell 301308 @ 519.50
--      Root cause: trading_calendar table was empty so the
--      marketstatus gate's evalCalendar branch returned no day and
--      the gate fell open (no halt → DecisionAllow). 09:01 CST is
--      outside A-share regular hours (09:30-11:30, 13:00-15:00).
--      Fixed 2026-06-03 by seeding trading_calendar with comprehensive
--      data for a_share / us_equity / crypto.
--
-- Method (per user choice "ledger_only"):
--   * trade_executions rows are kept at status='filled'; only
--     cancel_reason is populated so the audit chain on those rows
--     stays intact.
--   * The cash effect is reversed by inserting two cash_ledger
--     rows with entry_type='reversal' carrying both the original
--     trade_id and a structured metadata payload explaining the
--     root cause and the code fix.
--   * funds.current_capital and funds.total_assets are bumped by
--     the net cash effect (+96,322.6454 - 518.4608 = +95,804.1846
--     CNY) so the NAV reconciles with the ledger.
--   * One admin_change_log row captures the operator action with
--     full before/after snapshots.
--
-- Idempotency: the two cash_ledger inserts use deterministic
-- idempotency_keys so re-running this file is a no-op (ON CONFLICT
-- DO NOTHING). The funds UPDATE is guarded against double-apply by
-- checking that current_capital is still 842,493.4042 (the pre-
-- reversal value); a second run would skip the update and emit a
-- NOTICE.

BEGIN;

-- --------------------------------------------------------------
-- 1) Reversal cash_ledger entries
-- --------------------------------------------------------------
-- We use a WHERE NOT EXISTS guard rather than ON CONFLICT because
-- the unique index on (fund_id, idempotency_key) is partial
-- (WHERE idempotency_key IS NOT NULL), which Postgres can't drive
-- ON CONFLICT against without matching the index predicate.
INSERT INTO cash_ledger (
    fund_id, posted_at, trading_date, entry_type, amount, currency,
    trade_id, description, metadata, idempotency_key
)
SELECT
    'aa11aa11-aa11-aa11-aa11-aa11aa11aa11',
    NOW(), CURRENT_DATE, 'reversal',
    96322.6454, 'CNY',
    'b5e3405a-90fb-4bc8-a51a-9c737b364231',
    'Trade-break reversal: 301308 buy on 2026-06-02 at 96,226.4188 CNY/share. Root cause: PM quote-unavailable fallback stamped notional budget as per-share limit price. Code fix shipped 2026-06-03 (wiring_adapters.go → Action=watch + new broker price-collar gate).',
    jsonb_build_object(
      'reversal_kind',         'erroneous_fill',
      'reverses_trade_id',     'b5e3405a-90fb-4bc8-a51a-9c737b364231',
      'original_side',         'buy',
      'original_quantity',     1,
      'original_price_cny',    96226.4188,
      'true_mid_estimate_cny', 500,
      'original_fees_cny',     96.2266,
      'reason_code',           'quote_unavailable_budget_used_as_price',
      'incident_date',         '2026-06-02',
      'fix_commits',           jsonb_build_array(
        'wiring_adapters.go:14741,16856 - Action=watch on quote-unavailable',
        'internal/pricecollar - new engine with A-share 11/21% collars',
        'broker.WithPriceCollarGate + ErrPriceCollarRejected',
        'cmd/server/price_collar_gate.go - production wiring'
      )
    ),
    'erroneous_fill_reversal:b5e3405a-90fb-4bc8-a51a-9c737b364231'
WHERE NOT EXISTS (
    SELECT 1 FROM cash_ledger
    WHERE fund_id = 'aa11aa11-aa11-aa11-aa11-aa11aa11aa11'
      AND idempotency_key = 'erroneous_fill_reversal:b5e3405a-90fb-4bc8-a51a-9c737b364231'
);

INSERT INTO cash_ledger (
    fund_id, posted_at, trading_date, entry_type, amount, currency,
    trade_id, description, metadata, idempotency_key
)
SELECT
    'aa11aa11-aa11-aa11-aa11-aa11aa11aa11',
    NOW(), CURRENT_DATE, 'reversal',
    -518.4608, 'CNY',
    '55923b11-d6bd-4c7f-aa54-0629d2b363bc',
    'Trade-break reversal: 301308 sell on 2026-06-03 09:01:20 CST (outside A-share market hours 09:30-11:30, 13:00-15:00). Root cause: trading_calendar empty so marketstatus gate evalCalendar fell open. Fixed 2026-06-03 by seeding trading_calendar.',
    jsonb_build_object(
      'reversal_kind',        'out_of_hours_fill',
      'reverses_trade_id',    '55923b11-d6bd-4c7f-aa54-0629d2b363bc',
      'original_side',        'sell',
      'original_quantity',    1,
      'original_price_cny',   519.5,
      'execution_time_utc',   '2026-06-03T01:01:20Z',
      'execution_time_cst',   '2026-06-03T09:01:20+08:00',
      'market_open_cst',      '09:30',
      'reason_code',          'gate_bypassed_empty_calendar',
      'incident_date',        '2026-06-03',
      'fix_commits',          jsonb_build_array(
        'server/migrations/seed_trading_calendar.sql - seeded a_share/us_equity/crypto trading hours and holidays'
      )
    ),
    'erroneous_fill_reversal:55923b11-d6bd-4c7f-aa54-0629d2b363bc'
WHERE NOT EXISTS (
    SELECT 1 FROM cash_ledger
    WHERE fund_id = 'aa11aa11-aa11-aa11-aa11-aa11aa11aa11'
      AND idempotency_key = 'erroneous_fill_reversal:55923b11-d6bd-4c7f-aa54-0629d2b363bc'
);

-- --------------------------------------------------------------
-- 2) Restore funds.current_capital + total_assets
-- --------------------------------------------------------------
-- The pre-reversal current_capital is 842,493.4042. We guard against
-- a second apply by checking that exact value; if someone has already
-- run the reversal the funds.current_capital will be 938,297.5888 and
-- the update will skip with a NOTICE.
DO $$
DECLARE
    pre_cap NUMERIC;
BEGIN
    SELECT current_capital INTO pre_cap
    FROM funds WHERE id = 'aa11aa11-aa11-aa11-aa11-aa11aa11aa11';

    IF pre_cap IS NULL THEN
        RAISE EXCEPTION 'fund aa11aa11-aa11-aa11-aa11-aa11aa11aa11 not found';
    END IF;

    IF abs(pre_cap - 842493.4042) < 0.005 THEN
        UPDATE funds
        SET current_capital = current_capital + 95804.1846,
            total_assets    = total_assets    + 95804.1846,
            updated_at      = NOW()
        WHERE id = 'aa11aa11-aa11-aa11-aa11-aa11aa11aa11';
        RAISE NOTICE 'funds.current_capital bumped by +95804.1846 (was %)', pre_cap;
    ELSE
        RAISE NOTICE 'funds.current_capital is % — already reversed or unexpected value, skipping bump', pre_cap;
    END IF;
END$$;

-- --------------------------------------------------------------
-- 3) Annotate the original trade rows (cancel_reason only;
--    status preserved per ledger_only choice)
-- --------------------------------------------------------------
-- cancel_reason column is varchar(64) — keep the on-row tag short.
-- The full narrative lives in cash_ledger.description /
-- admin_change_log.metadata for this trade.
UPDATE trade_executions
SET cancel_reason = 'erroneous_fill:budget_as_price (see cash_ledger reversal)'
WHERE id = 'b5e3405a-90fb-4bc8-a51a-9c737b364231'
  AND (cancel_reason IS NULL OR cancel_reason = '');

UPDATE trade_executions
SET cancel_reason = 'erroneous_fill:out_of_hours (see cash_ledger reversal)'
WHERE id = '55923b11-d6bd-4c7f-aa54-0629d2b363bc'
  AND (cancel_reason IS NULL OR cancel_reason = '');

-- --------------------------------------------------------------
-- 4) Audit trail in admin_change_log (uses super_admin user
--    1160133516@qq.com / 9c325e54-3b21-43b3-ab71-d26dcd343ea7
--    as the operator, since this reversal was authorised by tong)
-- --------------------------------------------------------------
INSERT INTO admin_change_log (
    actor_user_id, action, target_type, target_id,
    before_snapshot, after_snapshot, metadata
) VALUES (
    '9c325e54-3b21-43b3-ab71-d26dcd343ea7',
    'trade_break_reversal',
    'fund',
    'aa11aa11-aa11-aa11-aa11-aa11aa11aa11',
    jsonb_build_object(
        'current_capital_before', 842493.4042,
        'erroneous_trades', jsonb_build_array(
            jsonb_build_object(
                'id',     'b5e3405a-90fb-4bc8-a51a-9c737b364231',
                'symbol', '301308',
                'side',   'buy',
                'qty',    1,
                'price',  96226.4188,
                'amount', 96226.4188,
                'fees',   96.2266
            ),
            jsonb_build_object(
                'id',     '55923b11-d6bd-4c7f-aa54-0629d2b363bc',
                'symbol', '301308',
                'side',   'sell',
                'qty',    1,
                'price',  519.5,
                'amount', 519.5,
                'fees',   1.0392
            )
        )
    ),
    jsonb_build_object(
        'current_capital_after',    938297.5888,
        'reversal_ledger_entries',  2,
        'net_cash_credit_cny',      95804.1846,
        'trade_break_method',       'cash_ledger_reversal+cancel_reason_annotation+admin_change_log'
    ),
    jsonb_build_object(
        'incident_summary', 'Two erroneous A-share fills on 2026-06-02/03 reversed: (a) 96,226.4188 CNY/share 301308 buy on 2026-06-02 (PM quote-unavailable fallback stamped budget as price), (b) 519.50 CNY 09:01:20 CST sell on 2026-06-03 (marketstatus gate bypassed due to empty trading_calendar).',
        'code_fixes_shipped', jsonb_build_array(
            'wiring_adapters.go:14741 - downgrade legacy/PM path quote-unavailable to Action=watch',
            'wiring_adapters.go:16856 - downgrade LLM decision-engine path quote-unavailable to Action=watch',
            'internal/pricecollar - new engine: per-asset-class collars (A-share 11/21%, US equity 15%, crypto 30%) with stale/missing reference handling',
            'broker.WithPriceCollarGate + ErrPriceCollarRejected - broker-side fat-finger safety net',
            'cmd/server/price_collar_gate.go - production wiring with marketdata.Service reference source',
            'cmd/server/main.go - simulator now constructed with WithPriceCollarGate',
            'fundai_pricecollar_events_total Prometheus counter',
            'server/migrations/seed_trading_calendar.sql - seeded trading hours/holidays for a_share/us_equity/crypto',
            'docs/PRICE_COLLAR_GATE.md'
        ),
        'tests_added', jsonb_build_array(
            'internal/pricecollar/engine_test.go (17 cases incl. the 96,226-CNY regression)',
            'internal/broker/simulator_pricecollar_test.go (7 cases)',
            'cmd/server/translate_buy_quote_unavailable_test.go (pins Action=watch contract)'
        ),
        'reversal_method',     'ledger_only',
        'requested_by',        'tong',
        'authorised_by_user',  '9c325e54-3b21-43b3-ab71-d26dcd343ea7'
    )
);

COMMIT;

-- --------------------------------------------------------------
-- Post-condition sanity checks (read-only; run independently)
-- --------------------------------------------------------------
-- SELECT id, current_capital, total_assets FROM funds WHERE id='aa11aa11-aa11-aa11-aa11-aa11aa11aa11';
-- SELECT id, entry_type, amount, currency, trade_id, idempotency_key, description FROM cash_ledger WHERE fund_id='aa11aa11-aa11-aa11-aa11-aa11aa11aa11' ORDER BY posted_at DESC LIMIT 5;
-- SELECT id, side, symbol, price, amount, status, cancel_reason FROM trade_executions WHERE id IN ('b5e3405a-90fb-4bc8-a51a-9c737b364231','55923b11-d6bd-4c7f-aa54-0629d2b363bc');
-- SELECT action, target_type, target_id, before_snapshot->>'current_capital_before' AS before_cap, after_snapshot->>'current_capital_after' AS after_cap, created_at FROM admin_change_log WHERE action='trade_break_reversal' ORDER BY created_at DESC LIMIT 1;
