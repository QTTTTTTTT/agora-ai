-- Migration: 108_daily_picks_more_presets
-- Description:
--   v1 of daily_picks shipped with exactly one publisher watchlist —
--   us_largecap_disruptive_v1 (Cathie Wood). Users immediately
--   flagged "策略视角就一个". This migration seeds three more
--   philosophically distinct strategy presets on the SAME 50-symbol
--   S&P large-cap universe so the /daily-picks page can offer a
--   value / growth / disruption / macro four-way comparison.
--
-- Cost framing:
--   Each watchlist costs ~$6/month per LLM call (50 symbols ×
--   ~30 trading days × ~$0.012/call). disruptive uses 1 master
--   (wood); the three additions use 2-3 masters each. Estimated
--   total publisher LLM spend after this migration: ~$72/month.
--
-- Selection rationale (why these four and not all six):
--   * disruptive  — wood                            (颠覆创新)
--   * conservative — buffett + munger + graham      (价值稳健)
--   * garp         — lynch + oneil                  (合理价成长)
--   * macro        — marks + dalio + druckenmiller  (宏观择时)
--   Skipped:
--   * deep_value (graham + greenblatt) — graham overlaps
--     conservative; for a v1 publisher we'd be paying twice for
--     similar signal.
--   * quant (greenblatt only) — single-master signal duplicates
--     deep_value's better half and offers no philosophical lens
--     end users associate with a named persona.
--
-- Idempotency:
--   ON CONFLICT (name, market, preset_key) DO NOTHING — re-running
--   this migration is a no-op.

BEGIN;

-- conservative: 价值稳健 (Buffett + Munger + Graham)
INSERT INTO daily_pick_watchlists (name, market, preset_key, symbols, schedule_cron, notes)
SELECT
    'us_largecap_conservative_v1',
    'us_equity',
    'conservative',
    symbols,
    schedule_cron,
    'Mirror of us_largecap_disruptive_v1 universe (50 S&P large caps) but scored by the conservative-value panel (buffett+munger+graham). Forms the value pole of the 4-preset publisher matrix. Migration 108.'
FROM daily_pick_watchlists
WHERE name = 'us_largecap_disruptive_v1'
ON CONFLICT (name, market, preset_key) DO NOTHING;

-- garp: GARP 成长 (Lynch + O'Neil)
INSERT INTO daily_pick_watchlists (name, market, preset_key, symbols, schedule_cron, notes)
SELECT
    'us_largecap_garp_v1',
    'us_equity',
    'garp',
    symbols,
    schedule_cron,
    'Mirror of us_largecap_disruptive_v1 universe scored by the GARP panel (lynch+oneil). Forms the growth-at-reasonable-price pole. Migration 108.'
FROM daily_pick_watchlists
WHERE name = 'us_largecap_disruptive_v1'
ON CONFLICT (name, market, preset_key) DO NOTHING;

-- macro: 宏观择时 (Marks + Dalio + Druckenmiller)
INSERT INTO daily_pick_watchlists (name, market, preset_key, symbols, schedule_cron, notes)
SELECT
    'us_largecap_macro_v1',
    'us_equity',
    'macro',
    symbols,
    schedule_cron,
    'Mirror of us_largecap_disruptive_v1 universe scored by the macro/top-down panel (marks+dalio+druckenmiller). Forms the macro-timing pole. Migration 108.'
FROM daily_pick_watchlists
WHERE name = 'us_largecap_disruptive_v1'
ON CONFLICT (name, market, preset_key) DO NOTHING;

COMMIT;
