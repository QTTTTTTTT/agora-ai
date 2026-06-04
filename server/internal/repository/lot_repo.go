// Package repository — lot ledger persistence (Phase 3A-1).
//
// LotRepo backs the two tables introduced in migration 038
// (position_lots + closed_lots) which together implement the
// FIFO buy/sell roundtrip ledger that the strategy attribution
// system needs. The repo is intentionally split from fund_repo.go
// because:
//
//  1. It has a self-contained transactional contract — opening a
//     lot, draining lots on a sell, and recording the closed_lots
//     row must all happen inside one *sql.Tx so the ledger never
//     drifts from trade_executions.
//  2. Its consumers (lotledger.Service, performance attribution
//     agent, sleeve scorecard) are higher-level than the rest of
//     fund_repo and won't share helpers with PlanRepo / TradeRepo.
//  3. Schema lifecycle: the lot ledger is the substrate for the
//     learning loop (Phase 3A-2/-3/-5). Keeping it in its own file
//     makes it obvious where to extend.
//
// All repo methods accept a `DBTX` (declared in uow.go) so the
// caller can run them inside an existing *sql.Tx (the trade-fill
// path) or against the bare *sql.DB (analytics queries).
package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrLotConflict is returned by PartialCloseTx when the targeted
// lot does not have enough quantity_remaining to satisfy the
// requested close (e.g. concurrent double-sell). Callers map it
// to api.ErrConflict at the boundary so the API contract stays
// independent of the repository's error vocabulary.
var ErrLotConflict = errors.New("lot_repo: insufficient lot quantity")

// ---------------------------------------------------------------------------
// Row types
// ---------------------------------------------------------------------------

// PositionLotRow mirrors a row in position_lots. It encodes one buy
// execution that is still (fully or partially) open. quantity_opened
// is the original lot size; quantity_remaining decreases as sells
// FIFO-consume it. When quantity_remaining reaches zero the row's
// status flips to "closed" and closed_at is stamped — the row is
// kept (not deleted) so audit and replay still work.
type PositionLotRow struct {
	ID                   string
	FundID               string
	InstrumentKey        string
	Symbol               string
	Market               sql.NullString
	AssetClass           sql.NullString
	OpeningTradeID       string
	OpeningPlanActionID  sql.NullString
	OpenedAt             time.Time
	EntryPrice           float64
	EntryFees            float64
	QuantityOpened       float64
	QuantityRemaining    float64
	Sleeve               sql.NullString
	RegimeAtEntry        sql.NullString
	SignalSource         sql.NullString
	ConfidenceAtEntry    sql.NullFloat64
	HighestPriceSeen     sql.NullFloat64
	LowestPriceSeen      sql.NullFloat64
	LastPrice            sql.NullFloat64
	LastPriceAt          sql.NullTime
	Status               string
	ClosedAt             sql.NullTime
	CreatedAt            time.Time
	UpdatedAt            time.Time
	// Side is "long" (default, pre-T8 baseline) or "short". A short
	// lot represents a sell-to-open position whose realized PnL on
	// close has the opposite sign from a long lot. Persisted on
	// position_lots.side (migration 090). Empty string is treated
	// as "long" by every reader so historical rows without a side
	// continue to flow through the long-lot helpers unchanged.
	Side string
}

// ClosedLotRow mirrors a row in closed_lots — one realised
// roundtrip (or partial close) ready for attribution. All numeric
// derivations (realised P&L, holding_days, MFE/MAE) are computed
// by the caller (the lotledger service) and persisted verbatim so
// downstream attribution queries are pure reads.
type ClosedLotRow struct {
	ID                     string
	FundID                 string
	PositionLotID          string
	InstrumentKey          string
	Symbol                 string
	Market                 sql.NullString
	AssetClass             sql.NullString
	ClosingTradeID         string
	ClosingPlanActionID    sql.NullString
	OpenedAt               time.Time
	ClosedAt               time.Time
	HoldingDays            float64
	QuantityClosed         float64
	EntryPrice             float64
	ExitPrice              float64
	EntryFees              float64
	ExitFees               float64
	RealizedPnL            float64
	RealizedPnLPct         float64
	MaxFavorableExcursion  sql.NullFloat64
	MaxAdverseExcursion    sql.NullFloat64
	Sleeve                 sql.NullString
	RegimeAtEntry          sql.NullString
	RegimeAtExit           sql.NullString
	SignalSource           sql.NullString
	ConfidenceAtEntry      sql.NullFloat64
	ExitReason             sql.NullString
	CreatedAt              time.Time
	// Side mirrors position_lots.side at the moment of close.
	// "long" (default) means the close walked long lots and the
	// realized_pnl is signed long-direction: positive when
	// exit > entry. "short" means the close walked short lots
	// and realized_pnl is signed short-direction: positive when
	// entry > exit (cover below the open). Migration 090.
	Side string
}

// SleeveStat aggregates closed_lots for a single sleeve within
// the query window. Fed to the PM prompt + scorecard UI.
type SleeveStat struct {
	Sleeve         string
	TradeCount     int
	WinCount       int
	LossCount      int
	TotalPnL       float64
	AvgPnLPct      float64
	WinRate        float64
	MedianHoldDays float64
}

// RegimeStat is the same shape as SleeveStat but bucketed by
// regime_at_entry. Lets the attribution agent answer "which
// regimes did we make money in?".
type RegimeStat struct {
	Regime         string
	TradeCount     int
	WinCount       int
	LossCount      int
	TotalPnL       float64
	AvgPnLPct      float64
	WinRate        float64
}

// SleeveRegimeStat is the two-axis cross-tab the Phase 3A-5
// attribution agent leans on hardest. The "which sleeve makes
// money in which regime?" question is the single most-valuable
// chart we can build out of the lot ledger — the LLM PM can
// take a binary signal "trend sleeve + trend_up = winner,
// mean_reversion + chop = catastrophe" and rebalance, where the
// individual-axis stats are too coarse to drive the decision.
type SleeveRegimeStat struct {
	Sleeve         string
	Regime         string
	TradeCount     int
	WinCount       int
	LossCount      int
	TotalPnL       float64
	AvgPnLPct      float64
	WinRate        float64
	AvgHoldingDays float64
}

// ---------------------------------------------------------------------------
// Repo
// ---------------------------------------------------------------------------

// LotRepo persists the FIFO lot ledger.
type LotRepo struct {
	db *sql.DB
}

// NewLotRepo binds a repo to a database handle.
func NewLotRepo(db *sql.DB) *LotRepo { return &LotRepo{db: db} }

// DB exposes the handle for tests / fixture setup. Production
// code routes through the methods.
func (r *LotRepo) DB() *sql.DB { return r.db }

// ---------------------------------------------------------------------------
// Open + close
// ---------------------------------------------------------------------------

// OpenLot inserts a new position_lots row representing one filled
// buy execution. The runner uses the *sql.DB handle (no caller-
// supplied tx); the trade-fill path uses OpenLotTx so the open is
// in the same transaction as the trade_executions UPDATE.
//
// The DB allocates the UUID; the returned string is the new lot ID.
func (r *LotRepo) OpenLot(ctx context.Context, lot *PositionLotRow) (string, error) {
	return r.openLot(ctx, r.db, lot)
}

// OpenLotTx is the *sql.Tx variant of OpenLot.
func (r *LotRepo) OpenLotTx(ctx context.Context, tx DBTX, lot *PositionLotRow) (string, error) {
	if tx == nil {
		return "", ErrNoTx
	}
	return r.openLot(ctx, tx, lot)
}

func (r *LotRepo) openLot(ctx context.Context, q DBTX, lot *PositionLotRow) (string, error) {
	if lot == nil {
		return "", errors.New("lot_repo: nil lot")
	}
	if strings.TrimSpace(lot.FundID) == "" {
		return "", errors.New("lot_repo: open lot: fund_id required")
	}
	if strings.TrimSpace(lot.InstrumentKey) == "" || strings.TrimSpace(lot.Symbol) == "" {
		return "", errors.New("lot_repo: open lot: instrument_key + symbol required")
	}
	if strings.TrimSpace(lot.OpeningTradeID) == "" {
		return "", errors.New("lot_repo: open lot: opening_trade_id required")
	}
	if lot.QuantityOpened <= 0 {
		return "", errors.New("lot_repo: open lot: quantity_opened must be > 0")
	}
	if lot.EntryPrice < 0 {
		return "", errors.New("lot_repo: open lot: entry_price must be >= 0")
	}

	// quantity_remaining defaults to the opened quantity so the
	// caller doesn't have to set both fields. Useful when the open
	// is the first event for the lot.
	remaining := lot.QuantityRemaining
	if remaining == 0 {
		remaining = lot.QuantityOpened
	}
	openedAt := lot.OpenedAt
	if openedAt.IsZero() {
		openedAt = time.Now().UTC()
	}

	// Side defaults to "long" for back-compat with pre-T8 callers.
	// The CHECK constraint on position_lots.side rejects anything
	// other than "long" / "short", so callers passing garbage here
	// would surface as a write error rather than a silent default.
	side := strings.ToLower(strings.TrimSpace(lot.Side))
	if side == "" {
		side = "long"
	}

	var id string
	err := q.QueryRowContext(ctx, `
INSERT INTO position_lots
    (fund_id, instrument_key, symbol, market, asset_class,
     opening_trade_id, opening_plan_action_id,
     opened_at, entry_price, entry_fees,
     quantity_opened, quantity_remaining,
     sleeve, regime_at_entry, signal_source, confidence_at_entry,
     highest_price_seen, lowest_price_seen, last_price, last_price_at,
     status, side)
VALUES ($1, $2, $3, $4, $5,
        $6, $7,
        $8, $9, $10,
        $11, $12,
        $13, $14, $15, $16,
        $17, $18, $19, $20,
        'open', $21)
RETURNING id`,
		lot.FundID, lot.InstrumentKey, lot.Symbol, lot.Market, lot.AssetClass,
		lot.OpeningTradeID, lot.OpeningPlanActionID,
		openedAt, lot.EntryPrice, lot.EntryFees,
		lot.QuantityOpened, remaining,
		lot.Sleeve, lot.RegimeAtEntry, lot.SignalSource, lot.ConfidenceAtEntry,
		lot.HighestPriceSeen, lot.LowestPriceSeen, lot.LastPrice, lot.LastPriceAt,
		side,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("lot_repo: open lot: %w", err)
	}
	return id, nil
}

// PartialCloseTx records a single close event against an open lot.
//
// It runs two SQL statements inside the caller's transaction:
//
//  1. INSERT INTO closed_lots — emits the realised-roundtrip row
//     the attribution layer queries.
//  2. UPDATE position_lots    — decrements quantity_remaining and,
//     when it hits zero, flips status='closed' and stamps closed_at.
//
// closeRow.QuantityClosed must be > 0 and ≤ the lot's current
// quantity_remaining. The UPDATE re-reads quantity_remaining inside
// the statement to be safe against concurrent partial closes (the
// trade-fill code path is single-flight per fund, so a clash is
// pathological — but the guard costs nothing).
//
// Returns ErrConflict if the lot has insufficient remaining qty.
func (r *LotRepo) PartialCloseTx(ctx context.Context, tx DBTX, closeRow *ClosedLotRow) error {
	if tx == nil {
		return ErrNoTx
	}
	if closeRow == nil {
		return errors.New("lot_repo: nil close row")
	}
	if strings.TrimSpace(closeRow.PositionLotID) == "" {
		return errors.New("lot_repo: close: position_lot_id required")
	}
	if strings.TrimSpace(closeRow.ClosingTradeID) == "" {
		return errors.New("lot_repo: close: closing_trade_id required")
	}
	if closeRow.QuantityClosed <= 0 {
		return errors.New("lot_repo: close: quantity_closed must be > 0")
	}

	// Side defaults to "long" for back-compat. Callers using the
	// pre-T8 buildClosedLot path will leave it empty; the DB's
	// CHECK constraint on closed_lots.side rejects anything other
	// than "long" / "short" so a malformed value surfaces as a
	// write error rather than a silent default.
	side := strings.ToLower(strings.TrimSpace(closeRow.Side))
	if side == "" {
		side = "long"
	}

	// 1. Insert the closed_lots row. The DB allocates id; we
	//    read it back so the caller can plumb it into logs / audit.
	err := tx.QueryRowContext(ctx, `
INSERT INTO closed_lots
    (fund_id, position_lot_id, instrument_key, symbol, market, asset_class,
     closing_trade_id, closing_plan_action_id,
     opened_at, closed_at, holding_days,
     quantity_closed, entry_price, exit_price, entry_fees, exit_fees,
     realized_pnl, realized_pnl_pct,
     max_favorable_excursion, max_adverse_excursion,
     sleeve, regime_at_entry, regime_at_exit, signal_source,
     confidence_at_entry, exit_reason, side)
VALUES ($1, $2, $3, $4, $5, $6,
        $7, $8,
        $9, $10, $11,
        $12, $13, $14, $15, $16,
        $17, $18,
        $19, $20,
        $21, $22, $23, $24,
        $25, $26, $27)
RETURNING id`,
		closeRow.FundID, closeRow.PositionLotID, closeRow.InstrumentKey, closeRow.Symbol, closeRow.Market, closeRow.AssetClass,
		closeRow.ClosingTradeID, closeRow.ClosingPlanActionID,
		closeRow.OpenedAt, closeRow.ClosedAt, closeRow.HoldingDays,
		closeRow.QuantityClosed, closeRow.EntryPrice, closeRow.ExitPrice, closeRow.EntryFees, closeRow.ExitFees,
		closeRow.RealizedPnL, closeRow.RealizedPnLPct,
		closeRow.MaxFavorableExcursion, closeRow.MaxAdverseExcursion,
		closeRow.Sleeve, closeRow.RegimeAtEntry, closeRow.RegimeAtExit, closeRow.SignalSource,
		closeRow.ConfidenceAtEntry, closeRow.ExitReason, side,
	).Scan(&closeRow.ID)
	if err != nil {
		return fmt.Errorf("lot_repo: insert closed lot: %w", err)
	}

	// 2. Decrement the originating lot. We do the math inside SQL
	//    and gate on quantity_remaining >= $closeQty so a concurrent
	//    over-sell becomes ErrConflict rather than a silent
	//    arithmetic underflow.
	res, err := tx.ExecContext(ctx, `
UPDATE position_lots
   SET quantity_remaining = quantity_remaining - $2,
       status             = CASE
                                WHEN quantity_remaining - $2 <= 0 THEN 'closed'
                                ELSE 'partial'
                            END,
       closed_at          = CASE
                                WHEN quantity_remaining - $2 <= 0 THEN $3
                                ELSE closed_at
                            END,
       updated_at         = NOW()
 WHERE id = $1
   AND status != 'closed'
   AND quantity_remaining >= $2`,
		closeRow.PositionLotID, closeRow.QuantityClosed, closeRow.ClosedAt,
	)
	if err != nil {
		return fmt.Errorf("lot_repo: drain lot: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("lot_repo: drain lot rows affected: %w", err)
	}
	if affected == 0 {
		return ErrLotConflict
	}
	return nil
}

// ---------------------------------------------------------------------------
// Queries
// ---------------------------------------------------------------------------

const positionLotSelect = `
SELECT id, fund_id, instrument_key, symbol, market, asset_class,
       opening_trade_id, opening_plan_action_id,
       opened_at, entry_price, entry_fees,
       quantity_opened, quantity_remaining,
       sleeve, regime_at_entry, signal_source, confidence_at_entry,
       highest_price_seen, lowest_price_seen, last_price, last_price_at,
       status, closed_at, created_at, updated_at, side
  FROM position_lots`

// ListOpenByInstrument returns every still-open lot for a (fund,
// instrument) pair, in FIFO order (oldest first). The lotledger
// service iterates this slice when a sell needs to consume lots.
//
// Status filter (status != 'closed') matches the partial index
// idx_position_lots_open_fifo so this is a fast index scan even
// when the historical closed-lot list is large.
func (r *LotRepo) ListOpenByInstrument(ctx context.Context, fundID, instrumentKey string) ([]*PositionLotRow, error) {
	return r.listOpenSide(ctx, r.db, fundID, instrumentKey, "long")
}

// ListOpenByInstrumentTx is the *sql.Tx variant used by the
// trade-fill flow so the read sees the same snapshot the
// subsequent close UPDATEs operate on.
//
// Filters to side='long' for back-compat with all pre-T8 callers.
// Use ListOpenByInstrumentSideTx if you need to read the short side.
func (r *LotRepo) ListOpenByInstrumentTx(ctx context.Context, tx DBTX, fundID, instrumentKey string) ([]*PositionLotRow, error) {
	if tx == nil {
		return nil, ErrNoTx
	}
	return r.listOpenSide(ctx, tx, fundID, instrumentKey, "long")
}

// ListOpenByInstrumentSideTx is the T8 short-side-aware variant.
// `side` MUST be one of "long" or "short". Mirrors the SQL plan
// of ListOpenByInstrumentTx (same partial index) and orders by
// opened_at ASC for FIFO close semantics within the chosen side.
func (r *LotRepo) ListOpenByInstrumentSideTx(ctx context.Context, tx DBTX, fundID, instrumentKey, side string) ([]*PositionLotRow, error) {
	if tx == nil {
		return nil, ErrNoTx
	}
	normSide := strings.ToLower(strings.TrimSpace(side))
	if normSide != "long" && normSide != "short" {
		return nil, fmt.Errorf("lot_repo: list open by side: invalid side %q (want long|short)", side)
	}
	return r.listOpenSide(ctx, tx, fundID, instrumentKey, normSide)
}

func (r *LotRepo) listOpenSide(ctx context.Context, q DBTX, fundID, instrumentKey, side string) ([]*PositionLotRow, error) {
	rows, err := q.QueryContext(ctx,
		positionLotSelect+` WHERE fund_id = $1 AND instrument_key = $2 AND status != 'closed' AND side = $3 ORDER BY opened_at ASC, id ASC`,
		fundID, instrumentKey, side,
	)
	if err != nil {
		return nil, fmt.Errorf("lot_repo: list open: %w", err)
	}
	defer rows.Close()
	out := []*PositionLotRow{}
	for rows.Next() {
		lot, err := scanPositionLot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, lot)
	}
	return out, rows.Err()
}

// ListOpenByFund returns every still-open lot for a fund (across
// every instrument), in opened_at ASC order. Used by the exit
// manager (PR-3A2) to scan all positions at the top of a slot.
func (r *LotRepo) ListOpenByFund(ctx context.Context, fundID string) ([]*PositionLotRow, error) {
	rows, err := r.db.QueryContext(ctx,
		positionLotSelect+` WHERE fund_id = $1 AND status != 'closed' ORDER BY instrument_key ASC, opened_at ASC, id ASC`,
		fundID,
	)
	if err != nil {
		return nil, fmt.Errorf("lot_repo: list open by fund: %w", err)
	}
	defer rows.Close()
	out := []*PositionLotRow{}
	for rows.Next() {
		lot, err := scanPositionLot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, lot)
	}
	return out, rows.Err()
}

// GetLot returns a single lot by ID (open or closed). Used by
// tests and audit.
func (r *LotRepo) GetLot(ctx context.Context, id string) (*PositionLotRow, error) {
	row := r.db.QueryRowContext(ctx, positionLotSelect+` WHERE id = $1`, id)
	lot, err := scanPositionLot(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return lot, err
}

// UpdateExcursion bulk-updates highest_price_seen / lowest_price_seen
// / last_price for every OPEN lot of the given (fund, instrument).
//
// The math runs entirely inside SQL via GREATEST/LEAST so the round-
// trip cost is one statement per quote update, regardless of how
// many open lots there are for the instrument.
//
// This is intentionally a fire-and-forget update; the caller need
// not transact it with a fill because excursion drift between two
// polls is bounded by the polling interval.
func (r *LotRepo) UpdateExcursion(ctx context.Context, fundID, instrumentKey string, price float64, at time.Time) error {
	if strings.TrimSpace(fundID) == "" || strings.TrimSpace(instrumentKey) == "" {
		return errors.New("lot_repo: update excursion: fund_id + instrument_key required")
	}
	if price <= 0 {
		// A zero/negative quote is almost always a feed glitch.
		// Silently no-op so the refresher doesn't poison the
		// excursion track with a bad print.
		return nil
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
UPDATE position_lots
   SET highest_price_seen = CASE
                                 WHEN highest_price_seen IS NULL OR $3 > highest_price_seen THEN $3
                                 ELSE highest_price_seen
                            END,
       lowest_price_seen  = CASE
                                 WHEN lowest_price_seen IS NULL OR $3 < lowest_price_seen THEN $3
                                 ELSE lowest_price_seen
                            END,
       last_price         = $3,
       last_price_at      = $4,
       updated_at         = NOW()
 WHERE fund_id = $1
   AND instrument_key = $2
   AND status != 'closed'`,
		fundID, instrumentKey, price, at,
	)
	if err != nil {
		return fmt.Errorf("lot_repo: update excursion: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// closed_lots queries
// ---------------------------------------------------------------------------

const closedLotSelect = `
SELECT id, fund_id, position_lot_id, instrument_key, symbol, market, asset_class,
       closing_trade_id, closing_plan_action_id,
       opened_at, closed_at, holding_days,
       quantity_closed, entry_price, exit_price, entry_fees, exit_fees,
       realized_pnl, realized_pnl_pct,
       max_favorable_excursion, max_adverse_excursion,
       sleeve, regime_at_entry, regime_at_exit, signal_source,
       confidence_at_entry, exit_reason, created_at
  FROM closed_lots`

// ListClosedSince returns closed_lots for a fund whose closed_at
// is in (since, +∞), newest first. The default limit (50) keeps
// API responses bounded; pass a higher limit when feeding the
// performance attribution agent.
func (r *LotRepo) ListClosedSince(ctx context.Context, fundID string, since time.Time, limit int) ([]*ClosedLotRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		closedLotSelect+` WHERE fund_id = $1 AND closed_at >= $2 ORDER BY closed_at DESC, id DESC LIMIT $3`,
		fundID, since, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("lot_repo: list closed: %w", err)
	}
	defer rows.Close()
	out := []*ClosedLotRow{}
	for rows.Next() {
		row, err := scanClosedLot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListClosedBetween returns every closed lot whose ClosedAt falls
// inside [from, to). Used by P&L attribution so the realised number
// reflects only the trades in the user-chosen window (Last 7 days,
// custom range, etc.). Unlike ListClosedSince, this query does NOT
// apply a default limit — attribution sums over all rows in the
// window, so silently truncating at 50 would produce an
// under-counted P&L. Callers that care about pagination should slice
// the returned slice themselves.
func (r *LotRepo) ListClosedBetween(ctx context.Context, fundID string, from, to time.Time) ([]*ClosedLotRow, error) {
	rows, err := r.db.QueryContext(ctx,
		closedLotSelect+` WHERE fund_id = $1 AND closed_at >= $2 AND closed_at < $3 ORDER BY closed_at ASC, id ASC`,
		fundID, from, to,
	)
	if err != nil {
		return nil, fmt.Errorf("lot_repo: list closed between: %w", err)
	}
	defer rows.Close()
	out := []*ClosedLotRow{}
	for rows.Next() {
		row, err := scanClosedLot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListClosedByInstrument returns the trade-history for a single
// (fund, instrument). Used by the fund detail page.
func (r *LotRepo) ListClosedByInstrument(ctx context.Context, fundID, instrumentKey string, limit int) ([]*ClosedLotRow, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx,
		closedLotSelect+` WHERE fund_id = $1 AND instrument_key = $2 ORDER BY closed_at DESC, id DESC LIMIT $3`,
		fundID, instrumentKey, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("lot_repo: list closed by instrument: %w", err)
	}
	defer rows.Close()
	out := []*ClosedLotRow{}
	for rows.Next() {
		row, err := scanClosedLot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// StatsBySleeve aggregates closed_lots over the window for each
// sleeve. Rows where sleeve is NULL roll up under the empty string
// so legacy LLM-only trades still get counted (typically as
// "llm_pm" once Phase 3A-4 plumbs the tag through).
//
// Numerical contract:
//   - win  = realized_pnl  > 0
//   - loss = realized_pnl  < 0
//   - flat = realized_pnl == 0 (counts as neither)
func (r *LotRepo) StatsBySleeve(ctx context.Context, fundID string, since time.Time) ([]SleeveStat, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT COALESCE(sleeve, '')                                    AS sleeve,
       COUNT(*)                                                AS trade_count,
       COUNT(*) FILTER (WHERE realized_pnl > 0)                AS win_count,
       COUNT(*) FILTER (WHERE realized_pnl < 0)                AS loss_count,
       COALESCE(SUM(realized_pnl), 0)                          AS total_pnl,
       COALESCE(AVG(realized_pnl_pct), 0)                      AS avg_pnl_pct,
       COALESCE(percentile_cont(0.5) WITHIN GROUP
                (ORDER BY holding_days), 0)                    AS median_hold_days
  FROM closed_lots
 WHERE fund_id = $1
   AND closed_at >= $2
 GROUP BY COALESCE(sleeve, '')
 ORDER BY total_pnl DESC, sleeve ASC`,
		fundID, since,
	)
	if err != nil {
		return nil, fmt.Errorf("lot_repo: stats by sleeve: %w", err)
	}
	defer rows.Close()
	out := []SleeveStat{}
	for rows.Next() {
		var s SleeveStat
		if err := rows.Scan(&s.Sleeve, &s.TradeCount, &s.WinCount, &s.LossCount, &s.TotalPnL, &s.AvgPnLPct, &s.MedianHoldDays); err != nil {
			return nil, fmt.Errorf("lot_repo: scan sleeve stat: %w", err)
		}
		if s.TradeCount > 0 {
			s.WinRate = float64(s.WinCount) / float64(s.TradeCount)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// StatsByRegime is the regime_at_entry-bucketed twin of StatsBySleeve.
// Same NULL → empty-string folding so unlabelled rows still appear.
func (r *LotRepo) StatsByRegime(ctx context.Context, fundID string, since time.Time) ([]RegimeStat, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT COALESCE(regime_at_entry, '')                           AS regime,
       COUNT(*)                                                AS trade_count,
       COUNT(*) FILTER (WHERE realized_pnl > 0)                AS win_count,
       COUNT(*) FILTER (WHERE realized_pnl < 0)                AS loss_count,
       COALESCE(SUM(realized_pnl), 0)                          AS total_pnl,
       COALESCE(AVG(realized_pnl_pct), 0)                      AS avg_pnl_pct
  FROM closed_lots
 WHERE fund_id = $1
   AND closed_at >= $2
 GROUP BY COALESCE(regime_at_entry, '')
 ORDER BY total_pnl DESC, regime ASC`,
		fundID, since,
	)
	if err != nil {
		return nil, fmt.Errorf("lot_repo: stats by regime: %w", err)
	}
	defer rows.Close()
	out := []RegimeStat{}
	for rows.Next() {
		var s RegimeStat
		if err := rows.Scan(&s.Regime, &s.TradeCount, &s.WinCount, &s.LossCount, &s.TotalPnL, &s.AvgPnLPct); err != nil {
			return nil, fmt.Errorf("lot_repo: scan regime stat: %w", err)
		}
		if s.TradeCount > 0 {
			s.WinRate = float64(s.WinCount) / float64(s.TradeCount)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// StatsBySleeveRegime aggregates closed_lots along the
// (sleeve, regime_at_entry) cross-tab. Both dimensions are NULL-
// folded to '' so unlabelled rows stay visible — the Phase 3A-5
// lesson generator uses those empty buckets as evidence of
// missing instrumentation rather than letting them disappear.
//
// Sort order: total_pnl descending so the most-impactful cells
// land at the top of paginated dashboards; ties broken by
// (sleeve, regime) ASC for stable display.
// OpenLotInventory returns the number of still-open lots for a
// fund plus the earliest opened_at across them. Used by the
// Phase 3A-5 attribution lesson generator to fill the
// "insufficient_data" lesson with concrete numbers — so the
// dashboard can read "the agent is watching 7 positions opened
// since 2026-05-12, waiting for the first closed roundtrip"
// instead of the generic "no closed trades" placeholder.
//
// Returns count=0 and a !Valid earliest when the fund has no
// open lots — the caller is expected to treat that as "agent
// hasn't bought anything yet" rather than an error.
func (r *LotRepo) OpenLotInventory(ctx context.Context, fundID string) (int, sql.NullTime, error) {
	var (
		count    int
		earliest sql.NullTime
	)
	err := r.db.QueryRowContext(ctx, `
SELECT COUNT(*),
       MIN(opened_at)
  FROM position_lots
 WHERE fund_id = $1
   AND status <> 'closed'`,
		fundID,
	).Scan(&count, &earliest)
	if err != nil {
		return 0, sql.NullTime{}, fmt.Errorf("lot_repo: open lot inventory: %w", err)
	}
	return count, earliest, nil
}

func (r *LotRepo) StatsBySleeveRegime(ctx context.Context, fundID string, since time.Time) ([]SleeveRegimeStat, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT COALESCE(sleeve, '')                                    AS sleeve,
       COALESCE(regime_at_entry, '')                           AS regime,
       COUNT(*)                                                AS trade_count,
       COUNT(*) FILTER (WHERE realized_pnl > 0)                AS win_count,
       COUNT(*) FILTER (WHERE realized_pnl < 0)                AS loss_count,
       COALESCE(SUM(realized_pnl), 0)                          AS total_pnl,
       COALESCE(AVG(realized_pnl_pct), 0)                      AS avg_pnl_pct,
       COALESCE(AVG(holding_days), 0)                          AS avg_holding_days
  FROM closed_lots
 WHERE fund_id = $1
   AND closed_at >= $2
 GROUP BY COALESCE(sleeve, ''), COALESCE(regime_at_entry, '')
 ORDER BY total_pnl DESC, sleeve ASC, regime ASC`,
		fundID, since,
	)
	if err != nil {
		return nil, fmt.Errorf("lot_repo: stats by sleeve+regime: %w", err)
	}
	defer rows.Close()
	out := []SleeveRegimeStat{}
	for rows.Next() {
		var s SleeveRegimeStat
		if err := rows.Scan(&s.Sleeve, &s.Regime, &s.TradeCount, &s.WinCount, &s.LossCount, &s.TotalPnL, &s.AvgPnLPct, &s.AvgHoldingDays); err != nil {
			return nil, fmt.Errorf("lot_repo: scan sleeve+regime stat: %w", err)
		}
		if s.TradeCount > 0 {
			s.WinRate = float64(s.WinCount) / float64(s.TradeCount)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Scan helpers
// ---------------------------------------------------------------------------

// scannable is the minimal Scan interface used by both *sql.Row
// and *sql.Rows so the same scan helper covers Get and List paths.
type scannable interface {
	Scan(dest ...any) error
}

func scanPositionLot(s scannable) (*PositionLotRow, error) {
	var lot PositionLotRow
	if err := s.Scan(
		&lot.ID, &lot.FundID, &lot.InstrumentKey, &lot.Symbol, &lot.Market, &lot.AssetClass,
		&lot.OpeningTradeID, &lot.OpeningPlanActionID,
		&lot.OpenedAt, &lot.EntryPrice, &lot.EntryFees,
		&lot.QuantityOpened, &lot.QuantityRemaining,
		&lot.Sleeve, &lot.RegimeAtEntry, &lot.SignalSource, &lot.ConfidenceAtEntry,
		&lot.HighestPriceSeen, &lot.LowestPriceSeen, &lot.LastPrice, &lot.LastPriceAt,
		&lot.Status, &lot.ClosedAt, &lot.CreatedAt, &lot.UpdatedAt, &lot.Side,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("lot_repo: scan position lot: %w", err)
	}
	return &lot, nil
}

func scanClosedLot(s scannable) (*ClosedLotRow, error) {
	var row ClosedLotRow
	if err := s.Scan(
		&row.ID, &row.FundID, &row.PositionLotID, &row.InstrumentKey, &row.Symbol, &row.Market, &row.AssetClass,
		&row.ClosingTradeID, &row.ClosingPlanActionID,
		&row.OpenedAt, &row.ClosedAt, &row.HoldingDays,
		&row.QuantityClosed, &row.EntryPrice, &row.ExitPrice, &row.EntryFees, &row.ExitFees,
		&row.RealizedPnL, &row.RealizedPnLPct,
		&row.MaxFavorableExcursion, &row.MaxAdverseExcursion,
		&row.Sleeve, &row.RegimeAtEntry, &row.RegimeAtExit, &row.SignalSource,
		&row.ConfidenceAtEntry, &row.ExitReason, &row.CreatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("lot_repo: scan closed lot: %w", err)
	}
	return &row, nil
}
