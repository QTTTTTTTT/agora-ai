-- Migration: 108_daily_picks_more_presets (DOWN)
-- Removes the three additional watchlist rows. Leaves
-- us_largecap_disruptive_v1 untouched.

BEGIN;

DELETE FROM daily_pick_watchlists
 WHERE name IN (
   'us_largecap_conservative_v1',
   'us_largecap_garp_v1',
   'us_largecap_macro_v1'
 );

-- Also clear any daily_picks already produced under these preset
-- keys, otherwise the orphaned rows would render in /daily-picks
-- with no watchlist to refresh them.
DELETE FROM daily_picks
 WHERE market = 'us_equity'
   AND preset_key IN ('conservative', 'garp', 'macro');

COMMIT;
