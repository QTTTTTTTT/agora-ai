ALTER TABLE plan_actions
    ADD COLUMN IF NOT EXISTS instrument_key VARCHAR(128) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS market VARCHAR(32),
    ADD COLUMN IF NOT EXISTS exchange VARCHAR(32),
    ADD COLUMN IF NOT EXISTS asset_class VARCHAR(32),
    ADD COLUMN IF NOT EXISTS instrument_type VARCHAR(32),
    ADD COLUMN IF NOT EXISTS position_side VARCHAR(16),
    ADD COLUMN IF NOT EXISTS open_close VARCHAR(16),
    ADD COLUMN IF NOT EXISTS quote_currency VARCHAR(16),
    ADD COLUMN IF NOT EXISTS settlement_currency VARCHAR(16),
    ADD COLUMN IF NOT EXISTS margin_mode VARCHAR(16),
    ADD COLUMN IF NOT EXISTS leverage NUMERIC(12, 4),
    ADD COLUMN IF NOT EXISTS contract_multiplier NUMERIC(16, 4),
    ADD COLUMN IF NOT EXISTS expiry_date DATE,
    ADD COLUMN IF NOT EXISTS reduce_only BOOLEAN;

UPDATE plan_actions
SET instrument_key = symbol
WHERE instrument_key = '';

ALTER TABLE trade_executions
    ADD COLUMN IF NOT EXISTS instrument_key VARCHAR(128) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS market VARCHAR(32),
    ADD COLUMN IF NOT EXISTS exchange VARCHAR(32),
    ADD COLUMN IF NOT EXISTS asset_class VARCHAR(32),
    ADD COLUMN IF NOT EXISTS instrument_type VARCHAR(32),
    ADD COLUMN IF NOT EXISTS position_side VARCHAR(16),
    ADD COLUMN IF NOT EXISTS open_close VARCHAR(16),
    ADD COLUMN IF NOT EXISTS quote_currency VARCHAR(16),
    ADD COLUMN IF NOT EXISTS settlement_currency VARCHAR(16),
    ADD COLUMN IF NOT EXISTS margin_mode VARCHAR(16),
    ADD COLUMN IF NOT EXISTS leverage NUMERIC(12, 4),
    ADD COLUMN IF NOT EXISTS contract_multiplier NUMERIC(16, 4),
    ADD COLUMN IF NOT EXISTS expiry_date DATE,
    ADD COLUMN IF NOT EXISTS reduce_only BOOLEAN;

UPDATE trade_executions
SET instrument_key = symbol
WHERE instrument_key = '';

ALTER TABLE holding_positions
    ADD COLUMN IF NOT EXISTS instrument_key VARCHAR(128) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS market VARCHAR(32),
    ADD COLUMN IF NOT EXISTS exchange VARCHAR(32),
    ADD COLUMN IF NOT EXISTS asset_class VARCHAR(32),
    ADD COLUMN IF NOT EXISTS instrument_type VARCHAR(32),
    ADD COLUMN IF NOT EXISTS position_side VARCHAR(16),
    ADD COLUMN IF NOT EXISTS quote_currency VARCHAR(16),
    ADD COLUMN IF NOT EXISTS settlement_currency VARCHAR(16),
    ADD COLUMN IF NOT EXISTS margin_mode VARCHAR(16),
    ADD COLUMN IF NOT EXISTS leverage NUMERIC(12, 4),
    ADD COLUMN IF NOT EXISTS contract_multiplier NUMERIC(16, 4),
    ADD COLUMN IF NOT EXISTS expiry_date DATE,
    ADD COLUMN IF NOT EXISTS unrealized_pnl NUMERIC(20, 4),
    ADD COLUMN IF NOT EXISTS margin_used NUMERIC(20, 4);

UPDATE holding_positions
SET instrument_key = symbol
WHERE instrument_key = '';

ALTER TABLE holding_positions DROP CONSTRAINT IF EXISTS holding_positions_fund_id_symbol_key;
ALTER TABLE holding_positions ADD CONSTRAINT holding_positions_fund_id_instrument_key_key UNIQUE (fund_id, instrument_key);

CREATE INDEX IF NOT EXISTS idx_plan_actions_instrument_key ON plan_actions (instrument_key);
CREATE INDEX IF NOT EXISTS idx_trade_executions_instrument_key ON trade_executions (instrument_key);
CREATE INDEX IF NOT EXISTS idx_holding_positions_instrument_key ON holding_positions (instrument_key);
