// Package lotbackfill backfills legacy holdings into the
// position_lots FIFO ledger so attribution + the closed-loop
// learning system see them and future sells correctly produce
// closed_lots rows.
//
// Why this exists. Phase 3A-1 (migration 038_lot_ledger.sql)
// introduced position_lots / closed_lots; funds that held
// positions before that migration ran have rows in
// holding_positions but no corresponding open lot. Their next
// sell would then become an "orphan sell" inside lotledger and
// would never produce a closed_lots row — which means the
// realised P&L of those legacy positions would silently bypass
// the attribution + lesson-generation pipeline forever.
//
// What it does. One legacy lot per (fund, instrument) holding
// with quantity > 0 that has no still-open lot:
//
//   - entry_price = holding_positions.cost_price (the
//     position-weighted average, more accurate than picking one
//     buy fill at random).
//   - opening_trade_id = the most-recent filled buy execution
//     for the same (fund, instrument). position_lots requires
//     opening_trade_id NOT NULL, but the column has no FK in the
//     schema — we still match a real trade_executions.id for
//     audit traceability.
//   - sleeve = "legacy" so attribution can tell these
//     synthesised lots apart from sleeves the PMAgent actually
//     emitted. The lesson generator can choose to exclude
//     'legacy' when computing per-sleeve win rates if it ever
//     pollutes the report.
//
// The backfill is idempotent: the WHERE NOT EXISTS gate ensures
// re-running it after every server boot is a no-op once the
// legacy holdings have already been synthesised. Holdings that
// have no matching buy trade in trade_executions (e.g.
// broker-synced legacy positions) are reported in Stats.Skipped
// so the operator can investigate, but they don't fail the run.
package lotbackfill

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
)

// SleeveLabel is the sleeve tag stamped onto every synthesised
// legacy lot. Exported so the attribution layer (or its tests)
// can reference the same constant when filtering it out.
const SleeveLabel = "legacy"

// Stats summarises what a Run did. Inserted is the primary
// number; Skipped reports holdings that we couldn't backfill
// because no matching buy trade exists in trade_executions.
type Stats struct {
	Inserted int
	Skipped  int
}

// Service is the entry point. It carries the *sql.DB handle and
// a logger; all state lives in Postgres.
type Service struct {
	db     *sql.DB
	logger *slog.Logger
}

// New builds a Service. logger may be nil — slog.Default() is
// used so callers can construct without configuring logging.
func New(db *sql.DB, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{db: db, logger: logger}
}

// Run executes the idempotent backfill. Returns how many lots
// were inserted plus how many holdings had to be skipped (no
// matching buy trade). A nil-receiver returns an error so the
// caller's wiring code can tell apart "service not wired" from
// "service ran fine, nothing to do".
func (s *Service) Run(ctx context.Context) (Stats, error) {
	if s == nil || s.db == nil {
		return Stats{}, errors.New("lotbackfill: service nil")
	}

	// Single INSERT...SELECT. Idempotency comes from the NOT
	// EXISTS gate against position_lots so a re-run after a
	// successful pass touches nothing. The LATERAL join picks
	// one trade per holding (the most recent buy) so the join
	// stays 1:1 with holding_positions even if a fund has
	// several historical buys for the same instrument.
	const insertStmt = `
INSERT INTO position_lots
    (fund_id, instrument_key, symbol, market, asset_class,
     opening_trade_id, opened_at, entry_price, entry_fees,
     quantity_opened, quantity_remaining,
     sleeve, highest_price_seen, lowest_price_seen, last_price, last_price_at,
     status)
SELECT hp.fund_id, hp.instrument_key, hp.symbol, hp.market, hp.asset_class,
       te.id, te.executed_at, hp.cost_price, 0,
       hp.quantity, hp.quantity,
       $1, hp.cost_price, hp.cost_price, hp.cost_price, te.executed_at,
       'open'
  FROM holding_positions hp
  JOIN LATERAL (
    SELECT id, executed_at FROM trade_executions
     WHERE fund_id = hp.fund_id
       AND instrument_key = hp.instrument_key
       AND status = 'filled'
       AND side IN ('buy', 'open_long', 'close_short')
       AND executed_at IS NOT NULL
     ORDER BY executed_at DESC
     LIMIT 1
  ) te ON TRUE
 WHERE hp.quantity > 0
   AND hp.instrument_key <> ''
   AND hp.cost_price > 0
   AND (hp.position_side IS NULL OR hp.position_side = 'long')
   AND NOT EXISTS (
     SELECT 1 FROM position_lots pl
     WHERE pl.fund_id = hp.fund_id
       AND pl.instrument_key = hp.instrument_key
       AND pl.status <> 'closed'
   )`

	res, err := s.db.ExecContext(ctx, insertStmt, SleeveLabel)
	if err != nil {
		return Stats{}, fmt.Errorf("lotbackfill: exec insert: %w", err)
	}
	rows, _ := res.RowsAffected()
	stats := Stats{Inserted: int(rows)}

	// Separately count holdings we couldn't backfill so the
	// operator gets visibility into "we still don't see lot X"
	// without having to crack open SQL. We don't fail the run
	// for these — they're broker-synced legacy positions and
	// the rest of the platform still functions.
	const skippedStmt = `
SELECT COUNT(*)
  FROM holding_positions hp
 WHERE hp.quantity > 0
   AND hp.instrument_key <> ''
   AND hp.cost_price > 0
   AND (hp.position_side IS NULL OR hp.position_side = 'long')
   AND NOT EXISTS (
     SELECT 1 FROM position_lots pl
     WHERE pl.fund_id = hp.fund_id
       AND pl.instrument_key = hp.instrument_key
       AND pl.status <> 'closed'
   )
   AND NOT EXISTS (
     SELECT 1 FROM trade_executions te
     WHERE te.fund_id = hp.fund_id
       AND te.instrument_key = hp.instrument_key
       AND te.status = 'filled'
       AND te.side IN ('buy', 'open_long', 'close_short')
       AND te.executed_at IS NOT NULL
   )`
	if err := s.db.QueryRowContext(ctx, skippedStmt).Scan(&stats.Skipped); err != nil {
		// Skipped count is informational — fall through with
		// what we already inserted rather than failing the run.
		s.logger.WarnContext(ctx, "lotbackfill: skipped count query failed",
			slog.String("err", err.Error()),
		)
	}

	switch {
	case stats.Inserted > 0:
		s.logger.InfoContext(ctx, "lotbackfill: synthesised legacy lots from holdings",
			slog.Int("inserted", stats.Inserted),
			slog.Int("skipped_no_buy_trade", stats.Skipped),
		)
	case stats.Skipped > 0:
		s.logger.WarnContext(ctx, "lotbackfill: holdings without a matching buy trade remain",
			slog.Int("skipped_no_buy_trade", stats.Skipped),
		)
	default:
		s.logger.DebugContext(ctx, "lotbackfill: nothing to backfill")
	}
	return stats, nil
}
