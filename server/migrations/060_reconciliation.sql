-- Migration 060 — reconciliation framework (P1-3).
--
-- Why this exists
--
-- Up to P1-2 the platform lived in a closed loop: simulator
-- + cash_ledger + holding_positions were both the "internal truth"
-- AND the "world truth", because the simulator IS the world.
--
-- The moment a real broker enters the picture (P0-9 broker_links
-- gate, future FIX/REST adapters), the world becomes external.
-- Two truths can drift:
--
--   - Holding mismatches: broker says 100 AAPL, we say 105 (we
--     forgot to record a partial sell, OR they double-counted a
--     fill).
--   - Cash mismatches: broker says $10,000.50 USD, we say
--     $10,012.40 (a fee posted from broker is not yet on our
--     ledger).
--   - Trade mismatches: broker reports a buy we don't have
--     (they sent it twice, OR our simulator missed an event from
--     the broker push).
--
-- The recon framework runs nightly (or on-demand), pulls a broker
-- statement, diffs it against internal state, and writes
-- `reconciliation_breaks` rows. An admin reviews + resolves, the
-- chain captures the resolution, ops sleeps better.
--
-- Three tables
--
--   broker_statements        — one row per (fund, statement_date,
--                              source). Holds the raw payload + a
--                              hash so we never re-ingest the same
--                              statement twice.
--   reconciliation_runs      — one row per executed diff. Carries
--                              the bucket counts + a status so the
--                              UI can render "all clear / 3 breaks
--                              pending review".
--   reconciliation_breaks    — one row per individual mismatch.
--                              Links to the run; carries
--                              symbol/currency/values + a resolution
--                              field that captures who said "this is
--                              fine, signed off".
--
-- Statement child tables
--
--   broker_statement_positions  — line-item per (statement, symbol)
--   broker_statement_cash       — line-item per (statement, currency)
--   broker_statement_trades     — line-item per (statement, broker
--                                 trade ref)
--
-- Why not store the diff in JSON instead of a separate table?
--
-- The breaks table is the work queue. We need indexed access by
-- status (pending vs resolved), severity (critical first), and
-- fund_id (filter to "my fund's open issues"). A normalized table
-- is cheaper than indexing into a JSON column for those access
-- patterns.

-- ----------------------------------------------------------------
-- broker_statements: raw EOD statement we ingested.
-- ----------------------------------------------------------------

CREATE TABLE IF NOT EXISTS broker_statements (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    fund_id          UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    broker_link_id   UUID, -- nullable: mock fixtures don't have a real link
    statement_date   DATE NOT NULL,
    source           VARCHAR(24) NOT NULL,
    payload_hash     VARCHAR(64) NOT NULL,
    raw_payload      JSONB NOT NULL DEFAULT '{}'::jsonb,
    ingested_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ingested_by      UUID,
    status           VARCHAR(24) NOT NULL DEFAULT 'pending'
                       CHECK (status IN ('pending', 'reconciled', 'failed')),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT broker_statements_source_chk
        CHECK (source IN ('mock', 'csv_upload', 'api', 'fix'))
);

-- One ingestion per (fund, date, source). The hash is what
-- guarantees the same content from the same source is dedup'd —
-- if a broker re-sends an identical CSV the second ingest is a
-- no-op.
CREATE UNIQUE INDEX IF NOT EXISTS broker_statements_fund_date_hash_uq
    ON broker_statements (fund_id, statement_date, source, payload_hash);

CREATE INDEX IF NOT EXISTS broker_statements_fund_date_idx
    ON broker_statements (fund_id, statement_date DESC);

-- ----------------------------------------------------------------
-- broker_statement_positions: positions reported by broker.
-- ----------------------------------------------------------------

CREATE TABLE IF NOT EXISTS broker_statement_positions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    statement_id    UUID NOT NULL REFERENCES broker_statements(id) ON DELETE CASCADE,
    symbol          VARCHAR(64) NOT NULL,
    quantity        NUMERIC(20, 6) NOT NULL,
    avg_cost        NUMERIC(20, 6) NOT NULL DEFAULT 0,
    market_value    NUMERIC(20, 6) NOT NULL DEFAULT 0,
    currency        VARCHAR(8) NOT NULL DEFAULT 'USD',
    metadata        JSONB DEFAULT '{}'::jsonb,
    UNIQUE (statement_id, symbol)
);

-- ----------------------------------------------------------------
-- broker_statement_cash: cash balances reported by broker.
-- ----------------------------------------------------------------

CREATE TABLE IF NOT EXISTS broker_statement_cash (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    statement_id    UUID NOT NULL REFERENCES broker_statements(id) ON DELETE CASCADE,
    currency        VARCHAR(8) NOT NULL,
    balance         NUMERIC(20, 6) NOT NULL,
    metadata        JSONB DEFAULT '{}'::jsonb,
    UNIQUE (statement_id, currency)
);

-- ----------------------------------------------------------------
-- broker_statement_trades: trades reported by broker (the day).
-- ----------------------------------------------------------------

CREATE TABLE IF NOT EXISTS broker_statement_trades (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    statement_id       UUID NOT NULL REFERENCES broker_statements(id) ON DELETE CASCADE,
    broker_trade_id    VARCHAR(128) NOT NULL,
    broker_order_id    VARCHAR(128),
    symbol             VARCHAR(64) NOT NULL,
    side               VARCHAR(8) NOT NULL CHECK (side IN ('buy', 'sell')),
    quantity           NUMERIC(20, 6) NOT NULL,
    price              NUMERIC(20, 6) NOT NULL,
    fee                NUMERIC(20, 6) NOT NULL DEFAULT 0,
    currency           VARCHAR(8) NOT NULL DEFAULT 'USD',
    executed_at        TIMESTAMPTZ NOT NULL,
    metadata           JSONB DEFAULT '{}'::jsonb,
    UNIQUE (statement_id, broker_trade_id)
);

CREATE INDEX IF NOT EXISTS broker_statement_trades_symbol_idx
    ON broker_statement_trades (statement_id, symbol);

-- ----------------------------------------------------------------
-- reconciliation_runs: one row per executed diff.
-- ----------------------------------------------------------------

CREATE TABLE IF NOT EXISTS reconciliation_runs (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    fund_id             UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    statement_id        UUID NOT NULL REFERENCES broker_statements(id) ON DELETE CASCADE,
    run_date            DATE NOT NULL,
    triggered_by        UUID,
    trigger_source      VARCHAR(24) NOT NULL DEFAULT 'manual'
                          CHECK (trigger_source IN ('manual', 'scheduled', 'replay')),
    status              VARCHAR(16) NOT NULL DEFAULT 'pending'
                          CHECK (status IN ('pending', 'completed', 'failed')),
    break_count_total   INT NOT NULL DEFAULT 0,
    break_count_critical INT NOT NULL DEFAULT 0,
    break_count_warning INT NOT NULL DEFAULT 0,
    break_count_info    INT NOT NULL DEFAULT 0,
    summary             JSONB DEFAULT '{}'::jsonb,
    started_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at        TIMESTAMPTZ,
    error_message       TEXT,
    UNIQUE (fund_id, statement_id, trigger_source)
);

CREATE INDEX IF NOT EXISTS reconciliation_runs_fund_date_idx
    ON reconciliation_runs (fund_id, run_date DESC);

CREATE INDEX IF NOT EXISTS reconciliation_runs_status_idx
    ON reconciliation_runs (status, run_date DESC);

-- ----------------------------------------------------------------
-- reconciliation_breaks: individual mismatches.
-- ----------------------------------------------------------------

CREATE TABLE IF NOT EXISTS reconciliation_breaks (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id              UUID NOT NULL REFERENCES reconciliation_runs(id) ON DELETE CASCADE,
    fund_id             UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    break_type          VARCHAR(48) NOT NULL,
    severity            VARCHAR(16) NOT NULL DEFAULT 'warning'
                          CHECK (severity IN ('info', 'warning', 'critical')),
    symbol              VARCHAR(64),
    currency            VARCHAR(8),
    internal_value      NUMERIC(20, 6),
    broker_value        NUMERIC(20, 6),
    diff_value          NUMERIC(20, 6),
    diff_percent        NUMERIC(12, 6),
    description         TEXT,
    metadata            JSONB DEFAULT '{}'::jsonb,
    status              VARCHAR(16) NOT NULL DEFAULT 'open'
                          CHECK (status IN ('open', 'acknowledged', 'resolved', 'ignored')),
    resolution_note     TEXT,
    resolved_by         UUID,
    resolved_at         TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- The closed vocabulary keeps the dashboard's filter list
    -- predictable. New break categories require a code change AND
    -- a migration that updates this CHECK.
    CONSTRAINT reconciliation_breaks_type_chk
        CHECK (break_type IN (
            'position_quantity_mismatch',
            'position_avg_cost_mismatch',
            'position_missing_internal',
            'position_missing_broker',
            'cash_balance_mismatch',
            'cash_currency_missing_internal',
            'cash_currency_missing_broker',
            'trade_missing_internal',
            'trade_missing_broker',
            'trade_quantity_mismatch',
            'trade_price_mismatch',
            'trade_side_mismatch'
        ))
);

CREATE INDEX IF NOT EXISTS reconciliation_breaks_run_idx
    ON reconciliation_breaks (run_id, severity);

CREATE INDEX IF NOT EXISTS reconciliation_breaks_fund_status_idx
    ON reconciliation_breaks (fund_id, status, created_at DESC);

CREATE INDEX IF NOT EXISTS reconciliation_breaks_open_idx
    ON reconciliation_breaks (fund_id, severity, created_at DESC)
    WHERE status = 'open';
