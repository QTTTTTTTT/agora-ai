-- Migration 059 — funds.base_currency (P1-4).
--
-- Adds a per-fund reporting currency. The NAV aggregator and
-- cash_ledger summary will translate every position / cash row
-- into this currency before producing the value the LP sees.
--
-- Default 'USD' keeps the migration backward-compatible: every
-- existing fund continues to behave exactly as today (1:1 USD)
-- until an operator changes it.
--
-- We store a CHAR-bounded VARCHAR(8) instead of a domain to keep
-- the schema dialect-portable and let the application layer own
-- the closed vocabulary check (USD / CNY / HKD / EUR / JPY / GBP
-- / SGD as of P1-4). Adding a new currency means: insert a few
-- fx_rates rows + UPDATE the allowlist in the Go code; no DDL.

ALTER TABLE funds
    ADD COLUMN IF NOT EXISTS base_currency VARCHAR(8) NOT NULL DEFAULT 'USD';

-- Defensive guard: the application keeps a closed vocabulary,
-- but the DB CHECK is the last line of defence against a
-- programmer typo in raw SQL migrations.
ALTER TABLE funds
    ADD CONSTRAINT funds_base_currency_chk
    CHECK (base_currency IN ('USD', 'CNY', 'HKD', 'EUR', 'JPY', 'GBP', 'SGD'));
