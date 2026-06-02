-- Down migration for 060_reconciliation.
DROP TABLE IF EXISTS reconciliation_breaks;
DROP TABLE IF EXISTS reconciliation_runs;
DROP TABLE IF EXISTS broker_statement_trades;
DROP TABLE IF EXISTS broker_statement_cash;
DROP TABLE IF EXISTS broker_statement_positions;
DROP TABLE IF EXISTS broker_statements;
