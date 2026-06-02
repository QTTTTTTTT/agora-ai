-- Down migration for 059_funds_base_currency.
ALTER TABLE funds DROP CONSTRAINT IF EXISTS funds_base_currency_chk;
ALTER TABLE funds DROP COLUMN IF EXISTS base_currency;
