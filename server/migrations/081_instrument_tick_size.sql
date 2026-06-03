-- 081_instrument_tick_size.sql — extend instrument_metadata with
-- per-instrument tick_size + tick_rules JSONB so the S12 lot-size
-- gate can also enforce price alignment.
--
-- # Why a separate migration
--
-- Migration 080 introduced instrument_metadata for quantity rules
-- (lot, step, fractional). Tick size lives in the same row but
-- needs its own column + a JSONB for the multi-tier rules (HK
-- price bands, US sub-dollar penny rules). Splitting the migration
-- keeps each upgrade atomic and reviewable.
--
-- # tick_size scalar
--
-- The simple case — a single increment for any limit price.
--   A-share equities: 0.01 CNY
--   US equities ≥ $1: 0.01 USD
--   Crypto pairs:    varies (BTC-USDT 0.01, ETH-USDT 0.01, …)
--
-- # tick_rules JSONB
--
-- For venues with price-banded tick rules (HK is the canonical
-- example):
--   [{"max_price": 0.25, "tick": 0.001},
--    {"max_price": 0.5,  "tick": 0.005},
--    {"max_price": 10,   "tick": 0.01},
--    {"max_price": 20,   "tick": 0.02},
--    {"max_price": 100,  "tick": 0.05},
--    ...]
-- The engine picks the smallest "max_price" that is >= the limit
-- price and uses its "tick".
--
-- When tick_rules is populated it takes precedence over tick_size.

ALTER TABLE instrument_metadata
    ADD COLUMN IF NOT EXISTS tick_size NUMERIC(20, 8) NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS tick_rules JSONB NOT NULL DEFAULT '[]'::jsonb;

-- A scalar tick for A-share (0.01 CNY uniformly across boards).
UPDATE instrument_metadata
   SET tick_size = 0.01
 WHERE market = 'a_share' AND tick_size = 0;

-- HK uses a banded tick. Single row per HK instrument carries the
-- same tick_rules (the rule is per-venue, but storing per-row makes
-- the lot-size gate's lookup trivially symmetrical with the lot
-- column). The rules below match HKEX 2025.
UPDATE instrument_metadata
   SET tick_rules = '[
        {"max_price":     0.25, "tick": 0.001},
        {"max_price":     0.5,  "tick": 0.005},
        {"max_price":    10,    "tick": 0.01},
        {"max_price":    20,    "tick": 0.02},
        {"max_price":   100,    "tick": 0.05},
        {"max_price":   200,    "tick": 0.1},
        {"max_price":   500,    "tick": 0.2},
        {"max_price":  1000,    "tick": 0.5},
        {"max_price":  2000,    "tick": 1},
        {"max_price":  5000,    "tick": 2},
        {"max_price": 9995,     "tick": 5}
   ]'::jsonb
 WHERE market = 'hk' AND tick_rules = '[]'::jsonb;

-- US equities default tick: 0.01 USD (NMS Rule 612 for stocks ≥ $1).
-- Sub-dollar penny stocks use 0.0001, but the platform's universe
-- today is all > $1; flagging that as a deferred follow-up.
UPDATE instrument_metadata
   SET tick_size = 0.01
 WHERE market IN ('us', 'us_stock', 'us_equity') AND tick_size = 0;

-- Crypto: same scalar as the step_size (most pairs have tick == step).
-- Operators can override via the admin endpoint when needed.
UPDATE instrument_metadata
   SET tick_size = step_size
 WHERE market = 'crypto' AND tick_size = 0;

-- CN futures: tick is the contract's tick (CFFEX index: 0.2 points,
-- 0.2 × multiplier = 60 CNY for IF). Index-future seeds:
UPDATE instrument_metadata
   SET tick_size = 0.2
 WHERE instrument_key IN ('CFFEX:IF2606', 'CFFEX:IH2606', 'CFFEX:IC2606')
   AND tick_size = 0;
