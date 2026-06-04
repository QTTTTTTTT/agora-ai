-- Migration 088: trade_executions.strategy + strategy_parent_trade_id
--
-- Step 2 of the Trader Agent integration plan
-- (docs/TRADER_AGENT_INTEGRATION.md). Step 1 was strategy
-- SELECTION + slog logging only — the chosen execution strategy
-- ('immediate' / 'limit' / 'twap' / 'vwap') was written to the
-- runtime log but not the database, so post-hoc analytics had to
-- grep logs to answer "what did TWAP intent fill at?".
--
-- Step 2 adds two columns:
--
--   * strategy                  — the execution strategy the
--                                 PM-direct-fill path selected for
--                                 this row. Identical across all
--                                 children of one parent: a 5-slice
--                                 TWAP parent + four children all
--                                 carry strategy='twap'. NULL = row
--                                 predates the column; new INSERTs
--                                 default to 'immediate' so the
--                                 column is always populated going
--                                 forward (TradeRepo.Create
--                                 normalizes).
--
--   * strategy_parent_trade_id  — pointer back to the parent trade
--                                 row when this row is a child
--                                 slice of a sliced execution
--                                 strategy (TWAP / VWAP / iceberg /
--                                 POV). NULL means "this IS the
--                                 parent" (or the trade predates
--                                 child-order splitting).
--
-- Disambiguation from the existing `parent_trade_id` column
-- (migration 051): that one points at the BRACKET parent — i.e. a
-- stop-loss / take-profit / OCO child whose lifecycle is bound to
-- the entry leg. The two relationships are orthogonal: a TWAP
-- entry CAN have OCO protective children that themselves point
-- back at the entry via parent_trade_id, while the TWAP slice
-- children point at the TWAP parent via
-- strategy_parent_trade_id. Keeping them separate avoids forcing
-- one ID to overload two semantics, which would silently break
-- bracket sibling cancellation when a TWAP fills.
--
-- The parent row carries the OVERALL fill summary (aggregated qty,
-- weighted-average fill price, summed cash), and each child row
-- carries a per-slice fill. cash_ledger / position_lots is written
-- PER CHILD so weighted-average price is preserved end-to-end and
-- FIFO cost basis remains accurate. The parent row deliberately
-- does NOT write cash_ledger / position_lots — that's the
-- invariant the splitting path enforces to avoid double-debiting.
--
-- Indexing:
--   * idx_trade_executions_strategy_parent on
--     (strategy_parent_trade_id) so "list all children of TWAP
--     parent X" (used by attribution + the trade-detail UI
--     drilldown) doesn't seq-scan. Partial index
--     (WHERE strategy_parent_trade_id IS NOT NULL) because the
--     bulk of rows will be parents while the feature flag is
--     gated.

ALTER TABLE trade_executions
    ADD COLUMN IF NOT EXISTS strategy                  VARCHAR(16),
    ADD COLUMN IF NOT EXISTS strategy_parent_trade_id  UUID;

ALTER TABLE trade_executions
    DROP CONSTRAINT IF EXISTS trade_executions_strategy_check;

ALTER TABLE trade_executions
    ADD CONSTRAINT trade_executions_strategy_check
    CHECK (strategy IS NULL OR strategy IN (
        'immediate',
        'limit',
        'twap',
        'vwap',
        'iceberg',
        'pov'
    ));

-- Self-referencing FK is intentionally NOT added at this step.
-- A parent row is INSERTed in the same transaction as its
-- children, but the parent gets its UUID at RETURNING time and
-- the children's strategy_parent_trade_id is populated in a
-- second INSERT inside the same tx. Adding a FK now would force
-- us to defer the constraint (more lock surface) or commit the
-- parent before the children (loses atomicity if any child
-- INSERT fails). We'll add the FK in a later migration once the
-- splitting path is fully exercised and we know it never
-- produces orphans.

CREATE INDEX IF NOT EXISTS idx_trade_executions_strategy_parent
    ON trade_executions (strategy_parent_trade_id)
    WHERE strategy_parent_trade_id IS NOT NULL;

COMMENT ON COLUMN trade_executions.strategy IS
    'Execution strategy selected by the PM direct-fill path '
    '(immediate / limit / twap / vwap / iceberg / pov). NULL '
    'rows predate the column. Children inherit the parent value.';

COMMENT ON COLUMN trade_executions.strategy_parent_trade_id IS
    'NULL = this row is a TWAP / VWAP parent (or a non-sliced '
    'fill, or pre-088 legacy row). Set = this row is one slice '
    'of a multi-child sliced execution rooted at '
    'strategy_parent_trade_id. cash_ledger / position_lots is '
    'written per child; the parent carries only the aggregated '
    'summary. Distinct from parent_trade_id (migration 051), '
    'which points at the BRACKET parent (stop-loss / OCO).';
