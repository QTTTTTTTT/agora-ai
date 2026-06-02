-- Down migration for 063_marketstatus.
DROP TABLE IF EXISTS marketstatus_events;
DROP TABLE IF EXISTS trading_calendar;
DROP TABLE IF EXISTS instrument_market_status;
