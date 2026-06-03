-- 080_instrument_metadata.sql — per-instrument market-microstructure
-- metadata for the S12.1 lot-size gate.
--
-- Trigger story: 2026-06-03 audit found broker-side lot-size
-- violations on A-share (301308 buy 1 share, 688195 sell 85, 688205
-- sell 62) and a 0.6-share residual from a corp-action applier bug
-- that S12.2 fixed. The lot-size gate ships with hard-coded A-share
-- board rules (via internal/instrument), but HK / US / crypto rules
-- are venue- and per-symbol-specific so they live in this table.
--
-- Coverage:
--   * HK equities: board_lot per symbol (00700=100, 00939=1000, …)
--   * US equities: fractional support flag (gated on venue)
--   * Crypto: step_size + min_notional per trading pair
--   * Futures: contract_multiplier per contract
--
-- The lot-size gate falls back to safe defaults when a row is
-- missing (HK lot=100, crypto step=1e-6, US integer-only) so this
-- table can be populated incrementally.

CREATE TABLE IF NOT EXISTS instrument_metadata (
    instrument_key      TEXT PRIMARY KEY,
    market              TEXT NOT NULL,
    asset_class         TEXT NOT NULL,
    -- HK / A-share / futures: integer board-lot or contract size.
    -- 0 means "no minimum" (engine falls back to default).
    board_lot           NUMERIC(20, 8) NOT NULL DEFAULT 0,
    -- Step size above board_lot. 0 → 1-unit increments.
    -- Crypto step_size lives here too (e.g. 0.00001 for BTC).
    step_size           NUMERIC(20, 8) NOT NULL DEFAULT 0,
    -- True when the venue allows non-integer quantities. Default
    -- false; flipped true for US fractional-capable brokers and
    -- crypto.
    supports_fractional BOOLEAN NOT NULL DEFAULT false,
    -- Minimum notional value (BTC: 10 USDT on Binance, etc.). 0
    -- means unenforced.
    min_notional        NUMERIC(20, 8) NOT NULL DEFAULT 0,
    -- Contract multiplier for futures (CFFEX IF: 300 CNY/point).
    -- 1 for equity / crypto / spot FX.
    contract_multiplier NUMERIC(20, 8) NOT NULL DEFAULT 1,
    -- Free-form provenance (HKEX securities list date, exchange
    -- API timestamp, etc.). Source tracking helps the daily
    -- refresh job know whether a row is stale.
    source              TEXT NOT NULL DEFAULT 'manual',
    source_as_of        TIMESTAMPTZ,
    notes               TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (board_lot >= 0),
    CHECK (step_size >= 0),
    CHECK (min_notional >= 0),
    CHECK (contract_multiplier > 0)
);

-- Cover the lookup paths the lot-size gate uses.
CREATE INDEX IF NOT EXISTS idx_instrument_metadata_market_asset
    ON instrument_metadata (market, asset_class);
CREATE INDEX IF NOT EXISTS idx_instrument_metadata_supports_fractional
    ON instrument_metadata (supports_fractional)
    WHERE supports_fractional = true;

-- Maintenance trigger so updated_at is always fresh on row mutation.
CREATE OR REPLACE FUNCTION touch_instrument_metadata_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_instrument_metadata_updated_at ON instrument_metadata;
CREATE TRIGGER trg_instrument_metadata_updated_at
    BEFORE UPDATE ON instrument_metadata
    FOR EACH ROW EXECUTE FUNCTION touch_instrument_metadata_updated_at();

-- Seed: the HK heavies the platform's universe currently touches.
-- Lot sizes per HKEX 2025 securities list. Source can be refreshed
-- via the admin sync handler (S12.6 will add a fetcher).
INSERT INTO instrument_metadata
    (instrument_key, market, asset_class, board_lot, step_size, source, source_as_of, notes)
VALUES
    -- Mega-cap tech.
    ('HKEX:00700', 'hk', 'equity',  100,  100, 'hkex_seed_2026_06', NOW(), 'Tencent Holdings'),
    ('HKEX:09988', 'hk', 'equity',  100,  100, 'hkex_seed_2026_06', NOW(), 'Alibaba Group'),
    ('HKEX:03690', 'hk', 'equity',  100,  100, 'hkex_seed_2026_06', NOW(), 'Meituan'),
    ('HKEX:09618', 'hk', 'equity',  100,  100, 'hkex_seed_2026_06', NOW(), 'JD.com'),
    ('HKEX:01024', 'hk', 'equity',  100,  100, 'hkex_seed_2026_06', NOW(), 'Kuaishou Technology'),
    ('HKEX:09999', 'hk', 'equity',  100,  100, 'hkex_seed_2026_06', NOW(), 'NetEase'),
    -- Financial heavyweights with non-100 lots.
    ('HKEX:00939', 'hk', 'equity', 1000, 1000, 'hkex_seed_2026_06', NOW(), 'China Construction Bank'),
    ('HKEX:01398', 'hk', 'equity', 1000, 1000, 'hkex_seed_2026_06', NOW(), 'ICBC'),
    ('HKEX:03988', 'hk', 'equity', 1000, 1000, 'hkex_seed_2026_06', NOW(), 'Bank of China'),
    ('HKEX:00005', 'hk', 'equity',  400,  400, 'hkex_seed_2026_06', NOW(), 'HSBC Holdings'),
    ('HKEX:00388', 'hk', 'equity',  100,  100, 'hkex_seed_2026_06', NOW(), 'HKEX itself'),
    ('HKEX:01299', 'hk', 'equity',  200,  200, 'hkex_seed_2026_06', NOW(), 'AIA Group'),
    -- Energy / industrial.
    ('HKEX:00857', 'hk', 'equity', 2000, 2000, 'hkex_seed_2026_06', NOW(), 'PetroChina'),
    ('HKEX:00386', 'hk', 'equity', 2000, 2000, 'hkex_seed_2026_06', NOW(), 'Sinopec'),
    ('HKEX:00883', 'hk', 'equity', 1000, 1000, 'hkex_seed_2026_06', NOW(), 'CNOOC')
ON CONFLICT (instrument_key) DO NOTHING;

-- Seed: common crypto pairs (Binance spot defaults). step_size and
-- min_notional are exchange-specific; these match Binance as of
-- 2026-06. S12.6 will add an exchange-specific resolver so we can
-- vary per venue.
INSERT INTO instrument_metadata
    (instrument_key, market, asset_class, board_lot, step_size,
     supports_fractional, min_notional, source, source_as_of, notes)
VALUES
    ('BINANCE:BTC-USDT', 'crypto', 'crypto', 0, 0.00001, true,  5,  'binance_seed_2026_06', NOW(), 'BTC spot'),
    ('BINANCE:ETH-USDT', 'crypto', 'crypto', 0, 0.0001,  true,  5,  'binance_seed_2026_06', NOW(), 'ETH spot'),
    ('BINANCE:SOL-USDT', 'crypto', 'crypto', 0, 0.001,   true,  5,  'binance_seed_2026_06', NOW(), 'SOL spot'),
    ('BINANCE:BNB-USDT', 'crypto', 'crypto', 0, 0.001,   true,  5,  'binance_seed_2026_06', NOW(), 'BNB spot'),
    ('BINANCE:XRP-USDT', 'crypto', 'crypto', 0, 0.1,     true,  5,  'binance_seed_2026_06', NOW(), 'XRP spot'),
    ('BINANCE:DOGE-USDT','crypto', 'crypto', 0, 1,       true,  5,  'binance_seed_2026_06', NOW(), 'DOGE spot')
ON CONFLICT (instrument_key) DO NOTHING;

-- Seed: a CFFEX futures contract example with its multiplier so the
-- lot-size gate (S12.1) sees the integer-hand rule.
INSERT INTO instrument_metadata
    (instrument_key, market, asset_class, board_lot, step_size,
     supports_fractional, contract_multiplier, source, source_as_of, notes)
VALUES
    ('CFFEX:IF2606',  'futures-cn', 'futures', 1, 1, false, 300, 'cffex_seed_2026_06', NOW(), 'CSI 300 index future, 300 CNY/point'),
    ('CFFEX:IC2606',  'futures-cn', 'futures', 1, 1, false, 200, 'cffex_seed_2026_06', NOW(), 'CSI 500 index future, 200 CNY/point'),
    ('CFFEX:IH2606',  'futures-cn', 'futures', 1, 1, false, 300, 'cffex_seed_2026_06', NOW(), 'SSE 50 index future, 300 CNY/point')
ON CONFLICT (instrument_key) DO NOTHING;
