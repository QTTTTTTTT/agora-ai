// Package lotledger turns raw trade fills into the FIFO lot ledger
// that powers strategy attribution and the closed-loop learning
// system (Phase 3A onward).
//
// The package sits between the trade-fill flow (wiring layer) and
// the lot persistence (repository.LotRepo). Callers hand it a
// FillEvent — a buy or sell that has just been recorded as
// status='filled' in trade_executions — and the service:
//
//   - For buys: opens a new position_lots row, carrying the
//     attribution metadata (sleeve, regime_tag, signal_source,
//     confidence_at_entry) that the PMAgent stamped onto the
//     originating plan_action.
//   - For sells: walks the still-open lots for that (fund,
//     instrument) in FIFO order, decrements each lot, and emits
//     one closed_lots row per lot consumed. The closed_lots row
//     carries the realised P&L, the % return, and the max
//     favourable / adverse excursion (computed from the
//     highest/lowest price the position saw while alive).
//
// All side effects happen inside the caller's *sql.Tx so the lot
// ledger stays consistent with trade_executions even when the
// outer transaction is rolled back for an unrelated reason.
//
// Why this is its own package (and not, say, more methods on
// LotRepo): the service owns the *arithmetic* (P&L formula, fee
// pro-rating, MFE/MAE derivation, holding-days computation) plus
// the *policy* (orphan sells degrade gracefully, partial fills
// split lots). Keeping that logic out of the repo means the SQL
// remains a thin data layer and the math is independently
// testable with an in-memory fake repo.
package lotledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/fundai/server/internal/repository"
)

// ---------------------------------------------------------------------------
// Public surface
// ---------------------------------------------------------------------------

// FillEvent is the input the wiring layer hands to the service
// right after a trade_executions row has been inserted as
// status='filled'. The service does not insert/update the trade
// itself — that responsibility stays with the trading engine.
//
// The struct is intentionally flat (no embedded TradeExecution
// pointer) so callers can build it from PlanAction + price + fees
// without dragging a full repository.TradeExecution around.
type FillEvent struct {
	FundID           string
	PlanActionID     sql.NullString
	TradeExecutionID string
	InstrumentKey    string
	Symbol           string
	Market           sql.NullString
	AssetClass       sql.NullString

	// Side must be "buy" or "sell"; futures-style "open_long" /
	// "close_short" semantics route through ClassifyFuturesSide
	// (helper below) to the same two-bucket model. Anything else
	// is a no-op the service quietly logs and ignores so it
	// doesn't disrupt the trade flow.
	Side string

	// Quantity is the *filled* quantity, not the originally
	// requested quantity. The service uses this to size the lot
	// (buys) or to determine how much to FIFO-consume (sells).
	Quantity float64

	// FilledPrice is the price the trade actually executed at
	// (trade_executions.filled_price). Used as entry_price for
	// new lots and exit_price for closed_lots rows.
	FilledPrice float64

	// TotalFees is commission + stamp_tax + transfer combined —
	// the full deduction from this single execution. The service
	// attributes it pro-rata when sells span multiple lots.
	TotalFees float64

	// ExecutedAt is the wall-clock fill timestamp. Defaults to
	// time.Now() when zero so callers without an explicit clock
	// stay correct.
	ExecutedAt time.Time

	// Entry-side attribution metadata. Populated for buys; for
	// sells the lot ledger reads these from the lot being closed.
	Sleeve            sql.NullString
	RegimeTag         sql.NullString
	SignalSource      sql.NullString
	ConfidenceAtEntry sql.NullFloat64

	// Exit-side attribution metadata. Populated for sells; the
	// service writes them into the closed_lots row.
	ExitReason  sql.NullString
	RegimeAtExit sql.NullString
}

// Result summarises what the service did. Mostly informational —
// the wiring layer's main interest is whether something was
// recorded so it can attribute follow-up actions, but a structured
// return value makes it easy to log + emit a metric.
type Result struct {
	OpenedLotID         string   // set when Side == "buy"
	ClosedLotIDs        []string // set when Side == "sell", one per consumed lot
	QuantityClosed      float64  // total qty actually consumed (may be < FillEvent.Quantity if orphan)
	QuantityOrphaned    float64  // qty that had no matching open lot (legacy positions)
	RealizedPnL         float64  // sum of realized_pnl across the closed_lots rows
}

// Repo is the slice of *repository.LotRepo the service needs.
// Keeping it as an interface lets unit tests inject an in-memory
// fake without spinning up Postgres.
type Repo interface {
	OpenLotTx(ctx context.Context, tx repository.DBTX, lot *repository.PositionLotRow) (string, error)
	ListOpenByInstrumentTx(ctx context.Context, tx repository.DBTX, fundID, instrumentKey string) ([]*repository.PositionLotRow, error)
	PartialCloseTx(ctx context.Context, tx repository.DBTX, row *repository.ClosedLotRow) error
}

// Service is the lotledger entry point. Construct one per
// process with NewService and reuse it for every fill.
type Service struct {
	repo   Repo
	logger *slog.Logger
}

// NewService wires the service. logger may be nil — the service
// falls back to slog.Default() so production code doesn't need to
// configure it explicitly, and tests can swap in a discard logger
// to silence the "orphan sell" warning we emit on legacy
// positions.
func NewService(repo Repo, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{repo: repo, logger: logger}
}

// RecordWithUoW is the convenience helper most callers want: it
// opens a fresh tx via the UnitOfWork, hands it to Record, and
// commits on success / rolls back on error. Use it from the
// trade-fill flow where the lot ledger is an independent
// shadow-ledger update separate from the trade_executions writes.
//
// Errors from the inner Record are returned to the caller so the
// wiring layer can log them; the trade itself is unaffected (the
// caller treats the lot ledger as best-effort).
func (s *Service) RecordWithUoW(ctx context.Context, uow repository.UnitOfWork, ev FillEvent) (*Result, error) {
	if s == nil {
		return nil, errors.New("lotledger: nil service")
	}
	if uow == nil {
		return nil, errors.New("lotledger: nil unit of work")
	}
	var result *Result
	err := uow.WithinTx(ctx, func(tx *sql.Tx) error {
		var inner error
		result, inner = s.Record(ctx, tx, ev)
		return inner
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Record dispatches the fill to the buy or sell handler. tx is
// the caller's outer transaction — the lot ledger updates land in
// the same tx as the trade_executions UPDATE so they commit or
// roll back atomically with the fill itself.
//
// Unknown sides are a soft error: the service logs and returns
// nil so an unfamiliar (e.g. futures "close_short") fill doesn't
// break the trade flow. The wiring layer should classify futures
// sides into "buy" / "sell" before calling Record.
func (s *Service) Record(ctx context.Context, tx repository.DBTX, ev FillEvent) (*Result, error) {
	if s == nil {
		return nil, errors.New("lotledger: nil service")
	}
	if tx == nil {
		return nil, repository.ErrNoTx
	}
	if err := s.validateFill(ev); err != nil {
		return nil, err
	}

	switch strings.ToLower(strings.TrimSpace(ev.Side)) {
	case "buy":
		return s.recordBuy(ctx, tx, ev)
	case "sell":
		return s.recordSell(ctx, tx, ev)
	default:
		s.logger.DebugContext(ctx, "lotledger: ignoring non-buy/sell fill",
			slog.String("side", ev.Side), slog.String("symbol", ev.Symbol))
		return &Result{}, nil
	}
}

// ---------------------------------------------------------------------------
// Buy path
// ---------------------------------------------------------------------------

func (s *Service) recordBuy(ctx context.Context, tx repository.DBTX, ev FillEvent) (*Result, error) {
	openedAt := ev.ExecutedAt
	if openedAt.IsZero() {
		openedAt = time.Now().UTC()
	}
	// Seed excursion tracking with the entry price so we never
	// see "highest_price_seen < entry_price" on a quote-less
	// position (e.g. the refresher hasn't run yet).
	seedPrice := sql.NullFloat64{Float64: ev.FilledPrice, Valid: ev.FilledPrice > 0}
	lot := &repository.PositionLotRow{
		FundID:              ev.FundID,
		InstrumentKey:       ev.InstrumentKey,
		Symbol:              ev.Symbol,
		Market:              ev.Market,
		AssetClass:          ev.AssetClass,
		OpeningTradeID:      ev.TradeExecutionID,
		OpeningPlanActionID: ev.PlanActionID,
		OpenedAt:            openedAt,
		EntryPrice:          ev.FilledPrice,
		EntryFees:           ev.TotalFees,
		QuantityOpened:      ev.Quantity,
		QuantityRemaining:   ev.Quantity,
		Sleeve:              ev.Sleeve,
		RegimeAtEntry:       ev.RegimeTag,
		SignalSource:        ev.SignalSource,
		ConfidenceAtEntry:   ev.ConfidenceAtEntry,
		HighestPriceSeen:    seedPrice,
		LowestPriceSeen:     seedPrice,
		LastPrice:           seedPrice,
		LastPriceAt:         sql.NullTime{Time: openedAt, Valid: ev.FilledPrice > 0},
	}
	id, err := s.repo.OpenLotTx(ctx, tx, lot)
	if err != nil {
		return nil, fmt.Errorf("lotledger: open lot for %s: %w", ev.Symbol, err)
	}
	s.logger.DebugContext(ctx, "lotledger: opened lot",
		slog.String("fund_id", ev.FundID),
		slog.String("symbol", ev.Symbol),
		slog.Float64("quantity", ev.Quantity),
		slog.Float64("entry_price", ev.FilledPrice),
		slog.String("sleeve", ev.Sleeve.String),
		slog.String("lot_id", id),
	)
	return &Result{OpenedLotID: id}, nil
}

// ---------------------------------------------------------------------------
// Sell path
// ---------------------------------------------------------------------------

func (s *Service) recordSell(ctx context.Context, tx repository.DBTX, ev FillEvent) (*Result, error) {
	openLots, err := s.repo.ListOpenByInstrumentTx(ctx, tx, ev.FundID, ev.InstrumentKey)
	if err != nil {
		return nil, fmt.Errorf("lotledger: list open lots for %s: %w", ev.Symbol, err)
	}
	closedAt := ev.ExecutedAt
	if closedAt.IsZero() {
		closedAt = time.Now().UTC()
	}

	result := &Result{}
	remaining := ev.Quantity
	for _, lot := range openLots {
		if remaining <= 0 {
			break
		}
		// FIFO: take min(lot.QuantityRemaining, remaining).
		take := lot.QuantityRemaining
		if take > remaining {
			take = remaining
		}
		if take <= 0 {
			continue
		}
		closedRow, err := s.buildClosedLot(lot, ev, take, closedAt)
		if err != nil {
			return nil, err
		}
		if err := s.repo.PartialCloseTx(ctx, tx, closedRow); err != nil {
			return nil, fmt.Errorf("lotledger: partial close lot %s: %w", lot.ID, err)
		}
		result.ClosedLotIDs = append(result.ClosedLotIDs, closedRow.ID)
		result.QuantityClosed += take
		result.RealizedPnL += closedRow.RealizedPnL
		remaining -= take
	}

	if remaining > 0 {
		// Orphan sell — the lot ledger doesn't know about this
		// quantity. This is expected for positions that pre-date
		// the lot ledger rollout (Phase 3A-1 only knows about
		// trades from this point forward). We log + count, but
		// don't fail: the trade itself is real and the holding
		// table accounting still works.
		result.QuantityOrphaned = remaining
		s.logger.WarnContext(ctx, "lotledger: orphan sell (legacy position?)",
			slog.String("fund_id", ev.FundID),
			slog.String("symbol", ev.Symbol),
			slog.Float64("orphan_qty", remaining),
			slog.Float64("sell_qty", ev.Quantity),
		)
	}
	return result, nil
}

// buildClosedLot computes the closed_lots row for one lot
// consumption. Centralising the arithmetic here keeps the SQL
// helpers thin and makes the math unit-testable without a DB.
//
// Fee attribution:
//   - entry_fees are pro-rated by qty consumed / qty originally
//     opened (NOT qty originally - already-closed): the same
//     denominator across partial closes so the sum of attributed
//     entry_fees over a lot's life equals lot.EntryFees.
//   - exit_fees are pro-rated by qty consumed / total sell qty so
//     fee allocation across lots in a multi-lot sell sums to the
//     fill's TotalFees.
//
// MFE / MAE come from the lot's highest_price_seen /
// lowest_price_seen tracked over its lifetime. If they're nil
// (excursion refresher didn't run before close — likely an
// intraday fill on a freshly opened lot), we fall back to the
// exit price so the row still has a defensible value rather than
// NULL.
func (s *Service) buildClosedLot(lot *repository.PositionLotRow, ev FillEvent, qty float64, closedAt time.Time) (*repository.ClosedLotRow, error) {
	if lot.QuantityOpened <= 0 {
		return nil, fmt.Errorf("lotledger: lot %s has zero quantity_opened", lot.ID)
	}
	entryFeesAttr := 0.0
	if lot.EntryFees > 0 {
		entryFeesAttr = lot.EntryFees * (qty / lot.QuantityOpened)
	}
	exitFeesAttr := 0.0
	if ev.TotalFees > 0 && ev.Quantity > 0 {
		exitFeesAttr = ev.TotalFees * (qty / ev.Quantity)
	}
	gross := (ev.FilledPrice - lot.EntryPrice) * qty
	net := gross - entryFeesAttr - exitFeesAttr
	pctDenom := lot.EntryPrice * qty
	pct := 0.0
	if pctDenom > 0 {
		pct = net / pctDenom
	}

	mfe := nullExcursion(lot.HighestPriceSeen, lot.EntryPrice, ev.FilledPrice, true)
	mae := nullExcursion(lot.LowestPriceSeen, lot.EntryPrice, ev.FilledPrice, false)

	holdingDays := holdingDaysBetween(lot.OpenedAt, closedAt)

	return &repository.ClosedLotRow{
		FundID:                ev.FundID,
		PositionLotID:         lot.ID,
		InstrumentKey:         ev.InstrumentKey,
		Symbol:                ev.Symbol,
		Market:                lot.Market,
		AssetClass:            lot.AssetClass,
		ClosingTradeID:        ev.TradeExecutionID,
		ClosingPlanActionID:   ev.PlanActionID,
		OpenedAt:              lot.OpenedAt,
		ClosedAt:              closedAt,
		HoldingDays:           holdingDays,
		QuantityClosed:        qty,
		EntryPrice:            lot.EntryPrice,
		ExitPrice:             ev.FilledPrice,
		EntryFees:             roundCurrency(entryFeesAttr),
		ExitFees:              roundCurrency(exitFeesAttr),
		RealizedPnL:           roundCurrency(net),
		RealizedPnLPct:        pct,
		MaxFavorableExcursion: mfe,
		MaxAdverseExcursion:   mae,
		Sleeve:                lot.Sleeve,
		RegimeAtEntry:         lot.RegimeAtEntry,
		RegimeAtExit:          ev.RegimeAtExit,
		SignalSource:          lot.SignalSource,
		ConfidenceAtEntry:     lot.ConfidenceAtEntry,
		ExitReason:            ev.ExitReason,
	}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *Service) validateFill(ev FillEvent) error {
	if strings.TrimSpace(ev.FundID) == "" {
		return errors.New("lotledger: fund_id required")
	}
	if strings.TrimSpace(ev.InstrumentKey) == "" || strings.TrimSpace(ev.Symbol) == "" {
		return errors.New("lotledger: instrument_key + symbol required")
	}
	if strings.TrimSpace(ev.TradeExecutionID) == "" {
		return errors.New("lotledger: trade_execution_id required")
	}
	if ev.Quantity <= 0 {
		return errors.New("lotledger: quantity must be > 0")
	}
	if ev.FilledPrice < 0 {
		return errors.New("lotledger: filled_price must be >= 0")
	}
	return nil
}

// nullExcursion picks the better of the lot-tracked price and the
// fallback (typically the exit price) for the favourable side, or
// the worse for the adverse side. Returns sql.Null{} when both
// inputs are unusable so the column stays NULL rather than 0
// (which would otherwise pollute the attribution percentiles).
//
// favourable = true → returns the MORE-favourable-to-a-long ratio
// favourable = false → returns the MORE-adverse-to-a-long ratio
func nullExcursion(tracked sql.NullFloat64, entry, fallback float64, favourable bool) sql.NullFloat64 {
	if entry <= 0 {
		return sql.NullFloat64{}
	}
	pickPrice := fallback
	if tracked.Valid {
		if favourable {
			if tracked.Float64 > pickPrice {
				pickPrice = tracked.Float64
			}
		} else {
			if tracked.Float64 < pickPrice {
				pickPrice = tracked.Float64
			}
		}
	}
	if pickPrice <= 0 {
		return sql.NullFloat64{}
	}
	ratio := (pickPrice - entry) / entry
	// For favourable excursion, clamp at >= 0 (a long whose
	// highest was BELOW entry has zero favourable excursion, not
	// negative). For adverse excursion, clamp at <= 0 by the
	// same logic on the downside.
	if favourable && ratio < 0 {
		ratio = 0
	}
	if !favourable && ratio > 0 {
		ratio = 0
	}
	return sql.NullFloat64{Float64: ratio, Valid: true}
}

func holdingDaysBetween(opened, closed time.Time) float64 {
	if opened.IsZero() || closed.IsZero() || !closed.After(opened) {
		// Intraday roundtrip — round up to a fraction of a day
		// instead of 0 so percentile_cont aggregations remain
		// well-defined and downstream "median holding days"
		// queries don't collapse a same-day flip to 0.
		return math.Round(1.0/24.0*10000) / 10000 // ~0.0417 day (~1h)
	}
	d := closed.Sub(opened).Hours() / 24.0
	// Two decimal places is plenty for human-readable reports
	// while keeping NUMERIC(10,4) lossless on the round-trip.
	return math.Round(d*10000) / 10000
}

// roundCurrency clips a monetary value to 4 decimal places to
// match the NUMERIC(20,4) precision of the underlying columns.
// Float64 arithmetic accumulates rounding error that, untreated,
// would round-trip differently in different libraries.
func roundCurrency(v float64) float64 {
	return math.Round(v*10000) / 10000
}

// ClassifyFuturesSide collapses futures-specific side strings
// (open_long / close_long / open_short / close_short) into the
// simple buy/sell vocabulary the lot ledger speaks. The wiring
// layer calls this before Record so the package stays
// asset-class-agnostic.
//
// For now we treat:
//   - open_long  → buy   (opens an upside lot)
//   - close_short → buy  (closes a short by buying-to-cover; lots are positive-side only in this phase)
//   - close_long → sell  (closes an upside lot)
//   - open_short → sell  (opens a downside lot; in this phase
//     short lots are not modelled so the wiring layer should
//     decide whether to call Record at all)
//
// The function returns "" for sides we don't yet model so the
// caller can skip the call without losing the warn.
func ClassifyFuturesSide(side string) string {
	switch strings.ToLower(strings.TrimSpace(side)) {
	case "buy", "open_long", "close_short":
		return "buy"
	case "sell", "close_long":
		return "sell"
	default:
		return ""
	}
}
