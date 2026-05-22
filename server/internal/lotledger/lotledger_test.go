package lotledger

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/fundai/server/internal/repository"
)

// ---------------------------------------------------------------------------
// In-memory fake Repo so the service's arithmetic is unit-testable
// without a Postgres roundtrip. The fake mirrors the real repo's
// behaviour for the methods the service touches: OpenLotTx
// allocates a stable id, ListOpenByInstrumentTx returns lots in
// opened_at ASC order, PartialCloseTx decrements quantity_remaining
// + writes a closed_lots row.
// ---------------------------------------------------------------------------

type fakeRepo struct {
	nextLotID    int
	nextClosedID int
	openLots     []*repository.PositionLotRow
	closedLots   []*repository.ClosedLotRow
	openErr      error
	closeErr     error
	listErr      error
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{}
}

func (f *fakeRepo) OpenLotTx(_ context.Context, _ repository.DBTX, lot *repository.PositionLotRow) (string, error) {
	if f.openErr != nil {
		return "", f.openErr
	}
	f.nextLotID++
	cp := *lot
	cp.ID = formatID("lot", f.nextLotID)
	if cp.QuantityRemaining == 0 {
		cp.QuantityRemaining = cp.QuantityOpened
	}
	cp.Status = "open"
	f.openLots = append(f.openLots, &cp)
	return cp.ID, nil
}

func (f *fakeRepo) ListOpenByInstrumentTx(_ context.Context, _ repository.DBTX, fundID, instrumentKey string) ([]*repository.PositionLotRow, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := []*repository.PositionLotRow{}
	for _, l := range f.openLots {
		if l.FundID != fundID || l.InstrumentKey != instrumentKey {
			continue
		}
		if l.Status == "closed" {
			continue
		}
		out = append(out, l)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].OpenedAt.Equal(out[j].OpenedAt) {
			return out[i].OpenedAt.Before(out[j].OpenedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (f *fakeRepo) PartialCloseTx(_ context.Context, _ repository.DBTX, row *repository.ClosedLotRow) error {
	if f.closeErr != nil {
		return f.closeErr
	}
	for _, l := range f.openLots {
		if l.ID != row.PositionLotID {
			continue
		}
		if l.QuantityRemaining < row.QuantityClosed {
			return repository.ErrLotConflict
		}
		l.QuantityRemaining -= row.QuantityClosed
		if l.QuantityRemaining <= 0 {
			l.Status = "closed"
			l.ClosedAt = sql.NullTime{Time: row.ClosedAt, Valid: true}
		} else {
			l.Status = "partial"
		}
		f.nextClosedID++
		row.ID = formatID("closed", f.nextClosedID)
		cp := *row
		f.closedLots = append(f.closedLots, &cp)
		return nil
	}
	return repository.ErrNotFound
}

func formatID(prefix string, n int) string {
	return prefix + "-" + intToStr(n)
}

func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	negative := n < 0
	if negative {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func quietService(t *testing.T) (*Service, *fakeRepo) {
	t.Helper()
	repo := newFakeRepo()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewService(repo, logger), repo
}

func openedAt(daysAgo int) time.Time {
	return time.Date(2026, time.May, 14, 9, 30, 0, 0, time.UTC).Add(-time.Duration(daysAgo) * 24 * time.Hour)
}

func approxEqual(t *testing.T, got, want, tol float64, name string) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s: got %.6f, want %.6f (±%.6f)", name, got, want, tol)
	}
}

// fakeTx satisfies DBTX without doing anything; the in-memory
// repo ignores the value but the service still requires it
// non-nil.
type fakeTx struct{}

func (fakeTx) ExecContext(_ context.Context, _ string, _ ...any) (sql.Result, error) {
	return nil, nil
}
func (fakeTx) QueryContext(_ context.Context, _ string, _ ...any) (*sql.Rows, error) {
	return nil, nil
}
func (fakeTx) QueryRowContext(_ context.Context, _ string, _ ...any) *sql.Row {
	return nil
}

// ---------------------------------------------------------------------------
// Buy path
// ---------------------------------------------------------------------------

func TestRecordBuyOpensLotWithAttributionMetadata(t *testing.T) {
	svc, repo := quietService(t)
	ev := FillEvent{
		FundID:           "fund-1",
		PlanActionID:     sql.NullString{String: "action-1", Valid: true},
		TradeExecutionID: "trade-1",
		InstrumentKey:    "SSE:600000",
		Symbol:           "600000",
		Market:           sql.NullString{String: "a_share", Valid: true},
		AssetClass:       sql.NullString{String: "equity", Valid: true},
		Side:             "buy",
		Quantity:         500,
		FilledPrice:      10.50,
		TotalFees:        5.25,
		ExecutedAt:       openedAt(5),

		Sleeve:            sql.NullString{String: "llm_pm", Valid: true},
		RegimeTag:         sql.NullString{String: "trend_up", Valid: true},
		SignalSource:      sql.NullString{String: "llm_pm", Valid: true},
		ConfidenceAtEntry: sql.NullFloat64{Float64: 0.72, Valid: true},
	}
	res, err := svc.Record(context.Background(), fakeTx{}, ev)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if res.OpenedLotID == "" {
		t.Fatal("expected an opened lot ID")
	}
	if len(repo.openLots) != 1 {
		t.Fatalf("expected 1 open lot, got %d", len(repo.openLots))
	}
	lot := repo.openLots[0]
	if lot.EntryPrice != 10.50 || lot.QuantityOpened != 500 || lot.QuantityRemaining != 500 {
		t.Fatalf("lot fields mismatch: %+v", lot)
	}
	if lot.Sleeve.String != "llm_pm" || lot.RegimeAtEntry.String != "trend_up" {
		t.Fatalf("attribution not carried into lot: %+v", lot)
	}
	if !lot.HighestPriceSeen.Valid || lot.HighestPriceSeen.Float64 != 10.50 {
		t.Fatalf("expected highest_price_seen seeded from entry, got %+v", lot.HighestPriceSeen)
	}
	if !lot.ConfidenceAtEntry.Valid || lot.ConfidenceAtEntry.Float64 != 0.72 {
		t.Fatalf("confidence not carried: %+v", lot.ConfidenceAtEntry)
	}
}

// ---------------------------------------------------------------------------
// Sell path — FIFO correctness
// ---------------------------------------------------------------------------

func TestRecordSellFIFOClosesOldestLotFirst(t *testing.T) {
	svc, repo := quietService(t)
	ctx := context.Background()

	// Open three lots at increasing prices on different days.
	// FIFO order = lot1 (earliest) first.
	openEvent := func(price float64, qty float64, fees float64, days int, id string) FillEvent {
		return FillEvent{
			FundID:           "fund-1",
			TradeExecutionID: id,
			InstrumentKey:    "NASDAQ:AAPL",
			Symbol:           "AAPL",
			Side:             "buy",
			Quantity:         qty,
			FilledPrice:      price,
			TotalFees:        fees,
			ExecutedAt:       openedAt(days),
			Sleeve:           sql.NullString{String: "llm_pm", Valid: true},
		}
	}
	for _, ev := range []FillEvent{
		openEvent(100, 30, 0.30, 30, "trade-1"),
		openEvent(110, 30, 0.33, 20, "trade-2"),
		openEvent(120, 30, 0.36, 10, "trade-3"),
	} {
		if _, err := svc.Record(ctx, fakeTx{}, ev); err != nil {
			t.Fatalf("buy %+v: %v", ev, err)
		}
	}

	// Sell 40 @ 130 with $4 fees. FIFO consumes:
	//   lot1: 30 shares (full close)
	//   lot2: 10 shares (partial)
	sellEv := FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "trade-sell",
		InstrumentKey:    "NASDAQ:AAPL",
		Symbol:           "AAPL",
		Side:             "sell",
		Quantity:         40,
		FilledPrice:      130,
		TotalFees:        4,
		ExecutedAt:       openedAt(0),
		ExitReason:       sql.NullString{String: "llm_decision", Valid: true},
	}
	res, err := svc.Record(ctx, fakeTx{}, sellEv)
	if err != nil {
		t.Fatalf("sell: %v", err)
	}
	if res.QuantityClosed != 40 {
		t.Fatalf("expected to close 40, closed %f", res.QuantityClosed)
	}
	if res.QuantityOrphaned != 0 {
		t.Fatalf("expected 0 orphaned, got %f", res.QuantityOrphaned)
	}
	if len(res.ClosedLotIDs) != 2 {
		t.Fatalf("expected 2 closed_lots rows, got %d", len(res.ClosedLotIDs))
	}

	// First closed row: lot1, 30 @ 130, fees pro-rated.
	first := repo.closedLots[0]
	// entry_fees attribution = 0.30 * (30/30) = 0.30
	// exit_fees  attribution = 4    * (30/40) = 3.00
	// gross = (130-100) * 30 = 900; net = 900 - 0.30 - 3.00 = 896.70
	if first.QuantityClosed != 30 {
		t.Fatalf("first row qty: got %f, want 30", first.QuantityClosed)
	}
	approxEqual(t, first.EntryFees, 0.30, 0.001, "first.entry_fees")
	approxEqual(t, first.ExitFees, 3.00, 0.001, "first.exit_fees")
	approxEqual(t, first.RealizedPnL, 896.70, 0.001, "first.realized_pnl")
	approxEqual(t, first.RealizedPnLPct, 896.70/3000.0, 0.0001, "first.realized_pnl_pct")
	if first.ExitReason.String != "llm_decision" {
		t.Fatalf("first.exit_reason: got %q", first.ExitReason.String)
	}

	// Second closed row: lot2, 10 @ 130.
	// entry_fees attribution = 0.33 * (10/30) = 0.11
	// exit_fees  attribution = 4    * (10/40) = 1.00
	// gross = (130-110)*10 = 200; net = 200 - 0.11 - 1.00 = 198.89
	second := repo.closedLots[1]
	if second.QuantityClosed != 10 {
		t.Fatalf("second row qty: got %f, want 10", second.QuantityClosed)
	}
	approxEqual(t, second.EntryFees, 0.11, 0.001, "second.entry_fees")
	approxEqual(t, second.ExitFees, 1.00, 0.001, "second.exit_fees")
	approxEqual(t, second.RealizedPnL, 198.89, 0.001, "second.realized_pnl")

	// Lot ledger state: lot1 closed, lot2 partial 20 remaining, lot3 untouched.
	if repo.openLots[0].Status != "closed" || repo.openLots[0].QuantityRemaining != 0 {
		t.Fatalf("lot1 should be closed: %+v", repo.openLots[0])
	}
	if repo.openLots[1].Status != "partial" || repo.openLots[1].QuantityRemaining != 20 {
		t.Fatalf("lot2 should be partial=20: %+v", repo.openLots[1])
	}
	if repo.openLots[2].Status != "open" || repo.openLots[2].QuantityRemaining != 30 {
		t.Fatalf("lot3 should be untouched: %+v", repo.openLots[2])
	}
}

// ---------------------------------------------------------------------------
// Orphan sells: legacy positions without lot records
// ---------------------------------------------------------------------------

func TestRecordSellOrphanQuantityIsCountedNotErrored(t *testing.T) {
	svc, repo := quietService(t)
	ctx := context.Background()

	// Only one lot of 10 shares.
	if _, err := svc.Record(ctx, fakeTx{}, FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "trade-1",
		InstrumentKey:    "SSE:600000",
		Symbol:           "600000",
		Side:             "buy",
		Quantity:         10,
		FilledPrice:      20,
		TotalFees:        0,
		ExecutedAt:       openedAt(2),
	}); err != nil {
		t.Fatalf("buy: %v", err)
	}

	// Try to sell 25 (simulating an orphan position from legacy data).
	res, err := svc.Record(ctx, fakeTx{}, FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "trade-sell",
		InstrumentKey:    "SSE:600000",
		Symbol:           "600000",
		Side:             "sell",
		Quantity:         25,
		FilledPrice:      22,
		ExecutedAt:       openedAt(0),
	})
	if err != nil {
		t.Fatalf("sell should not error on orphan, got %v", err)
	}
	if res.QuantityClosed != 10 {
		t.Fatalf("expected to close 10, got %f", res.QuantityClosed)
	}
	if res.QuantityOrphaned != 15 {
		t.Fatalf("expected 15 orphaned, got %f", res.QuantityOrphaned)
	}
	if len(repo.closedLots) != 1 {
		t.Fatalf("expected 1 closed lot row, got %d", len(repo.closedLots))
	}
}

// ---------------------------------------------------------------------------
// Excursion: MFE / MAE picked from lot's tracked extremes
// ---------------------------------------------------------------------------

func TestRecordSellComputesMFEAndMAEFromTrackedExtremes(t *testing.T) {
	svc, repo := quietService(t)
	ctx := context.Background()

	// Open lot at 100, then simulate the price refresher bumping
	// highest_price_seen to 120 and lowest_price_seen to 80 over
	// the holding period. Sell at 110.
	if _, err := svc.Record(ctx, fakeTx{}, FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "trade-1",
		InstrumentKey:    "NASDAQ:NVDA",
		Symbol:           "NVDA",
		Side:             "buy",
		Quantity:         10,
		FilledPrice:      100,
		ExecutedAt:       openedAt(10),
	}); err != nil {
		t.Fatalf("buy: %v", err)
	}
	// Simulate refresher updates (the real refresher will call
	// LotRepo.UpdateExcursion; here we mutate the fake directly).
	repo.openLots[0].HighestPriceSeen = sql.NullFloat64{Float64: 120, Valid: true}
	repo.openLots[0].LowestPriceSeen = sql.NullFloat64{Float64: 80, Valid: true}

	if _, err := svc.Record(ctx, fakeTx{}, FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "trade-sell",
		InstrumentKey:    "NASDAQ:NVDA",
		Symbol:           "NVDA",
		Side:             "sell",
		Quantity:         10,
		FilledPrice:      110,
		ExecutedAt:       openedAt(0),
	}); err != nil {
		t.Fatalf("sell: %v", err)
	}
	row := repo.closedLots[0]
	// MFE = (max(120, 110) - 100) / 100 = 0.20
	// MAE = (min(80, 110)  - 100) / 100 = -0.20
	if !row.MaxFavorableExcursion.Valid || math.Abs(row.MaxFavorableExcursion.Float64-0.20) > 1e-9 {
		t.Fatalf("MFE: got %+v, want 0.20", row.MaxFavorableExcursion)
	}
	if !row.MaxAdverseExcursion.Valid || math.Abs(row.MaxAdverseExcursion.Float64-(-0.20)) > 1e-9 {
		t.Fatalf("MAE: got %+v, want -0.20", row.MaxAdverseExcursion)
	}
}

func TestRecordSellMFEFallsBackToExitPriceWhenUntracked(t *testing.T) {
	svc, repo := quietService(t)
	ctx := context.Background()

	if _, err := svc.Record(ctx, fakeTx{}, FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "trade-1",
		InstrumentKey:    "NASDAQ:MSFT",
		Symbol:           "MSFT",
		Side:             "buy",
		Quantity:         10,
		FilledPrice:      100,
		ExecutedAt:       openedAt(3),
	}); err != nil {
		t.Fatalf("buy: %v", err)
	}
	// Clear the seeded excursion (simulating "refresher never ran").
	repo.openLots[0].HighestPriceSeen = sql.NullFloat64{}
	repo.openLots[0].LowestPriceSeen = sql.NullFloat64{}

	if _, err := svc.Record(ctx, fakeTx{}, FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "trade-sell",
		InstrumentKey:    "NASDAQ:MSFT",
		Symbol:           "MSFT",
		Side:             "sell",
		Quantity:         10,
		FilledPrice:      105,
		ExecutedAt:       openedAt(0),
	}); err != nil {
		t.Fatalf("sell: %v", err)
	}
	row := repo.closedLots[0]
	// With no tracked extremes, MFE = max((105-100)/100, 0) = 0.05
	// and MAE = min((105-100)/100, 0) = 0 (clamped — the lot never
	// went negative as far as we know).
	if !row.MaxFavorableExcursion.Valid || math.Abs(row.MaxFavorableExcursion.Float64-0.05) > 1e-9 {
		t.Fatalf("MFE fallback: got %+v, want 0.05", row.MaxFavorableExcursion)
	}
	if !row.MaxAdverseExcursion.Valid || row.MaxAdverseExcursion.Float64 != 0 {
		t.Fatalf("MAE fallback: got %+v, want 0", row.MaxAdverseExcursion)
	}
}

// ---------------------------------------------------------------------------
// Holding days
// ---------------------------------------------------------------------------

func TestHoldingDaysIntraDayRoundsToOneHourMinimum(t *testing.T) {
	d := holdingDaysBetween(openedAt(0), openedAt(0))
	if d <= 0 {
		t.Fatalf("intraday holding_days must be > 0, got %f", d)
	}
	if d > 0.05 {
		t.Fatalf("intraday holding_days should be ~0.04, got %f", d)
	}
}

func TestHoldingDaysMultiDay(t *testing.T) {
	d := holdingDaysBetween(openedAt(7), openedAt(0))
	if math.Abs(d-7) > 1e-3 {
		t.Fatalf("expected ~7 days, got %f", d)
	}
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func TestRecordRejectsMissingRequiredFields(t *testing.T) {
	svc, _ := quietService(t)
	cases := []struct {
		name string
		ev   FillEvent
	}{
		{"no fund_id", FillEvent{InstrumentKey: "X", Symbol: "X", TradeExecutionID: "t", Side: "buy", Quantity: 1, FilledPrice: 1}},
		{"no instrument_key", FillEvent{FundID: "f", Symbol: "X", TradeExecutionID: "t", Side: "buy", Quantity: 1, FilledPrice: 1}},
		{"no trade_id", FillEvent{FundID: "f", InstrumentKey: "X", Symbol: "X", Side: "buy", Quantity: 1, FilledPrice: 1}},
		{"zero qty", FillEvent{FundID: "f", InstrumentKey: "X", Symbol: "X", TradeExecutionID: "t", Side: "buy", Quantity: 0, FilledPrice: 1}},
		{"negative price", FillEvent{FundID: "f", InstrumentKey: "X", Symbol: "X", TradeExecutionID: "t", Side: "buy", Quantity: 1, FilledPrice: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Record(context.Background(), fakeTx{}, tc.ev)
			if err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestRecordIgnoresUnknownSide(t *testing.T) {
	svc, repo := quietService(t)
	res, err := svc.Record(context.Background(), fakeTx{}, FillEvent{
		FundID:           "fund-1",
		InstrumentKey:    "X",
		Symbol:           "X",
		TradeExecutionID: "trade-1",
		Side:             "swap", // not in the buy/sell vocabulary
		Quantity:         1,
		FilledPrice:      1,
	})
	if err != nil {
		t.Fatalf("unknown side should soft-fail, got %v", err)
	}
	if res.OpenedLotID != "" || len(res.ClosedLotIDs) != 0 {
		t.Fatalf("unknown side should be a no-op, got %+v", res)
	}
	if len(repo.openLots) != 0 || len(repo.closedLots) != 0 {
		t.Fatalf("unknown side should not touch repo")
	}
}

// ---------------------------------------------------------------------------
// Buy attribution after the fact: the closed_lots row should
// carry the SLEEVE the entry was opened with, even when the
// closing FillEvent has a different sleeve (defensive: ensures
// reporting always groups by entry-side attribution).
// ---------------------------------------------------------------------------

func TestClosedLotInheritsEntrySleeveNotExitSleeve(t *testing.T) {
	svc, repo := quietService(t)
	ctx := context.Background()
	_, err := svc.Record(ctx, fakeTx{}, FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "trade-1",
		InstrumentKey:    "X",
		Symbol:           "X",
		Side:             "buy",
		Quantity:         5,
		FilledPrice:      10,
		ExecutedAt:       openedAt(2),
		Sleeve:           sql.NullString{String: "donchian", Valid: true},
		SignalSource:     sql.NullString{String: "donchian_20", Valid: true},
		RegimeTag:        sql.NullString{String: "trend_up", Valid: true},
	})
	if err != nil {
		t.Fatalf("buy: %v", err)
	}
	_, err = svc.Record(ctx, fakeTx{}, FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "trade-sell",
		InstrumentKey:    "X",
		Symbol:           "X",
		Side:             "sell",
		Quantity:         5,
		FilledPrice:      15,
		ExecutedAt:       openedAt(0),
		// Different sleeve on exit — should NOT leak into closed_lots row.
		Sleeve:       sql.NullString{String: "exit_manager", Valid: true},
		ExitReason:   sql.NullString{String: "trailing", Valid: true},
		RegimeAtExit: sql.NullString{String: "range", Valid: true},
	})
	if err != nil {
		t.Fatalf("sell: %v", err)
	}
	row := repo.closedLots[0]
	if row.Sleeve.String != "donchian" {
		t.Fatalf("entry sleeve should be carried: got %q", row.Sleeve.String)
	}
	if row.SignalSource.String != "donchian_20" {
		t.Fatalf("entry signal source should be carried: got %q", row.SignalSource.String)
	}
	if row.RegimeAtEntry.String != "trend_up" {
		t.Fatalf("entry regime should be carried: got %q", row.RegimeAtEntry.String)
	}
	if row.RegimeAtExit.String != "range" {
		t.Fatalf("exit regime should come from sell event: got %q", row.RegimeAtExit.String)
	}
	if row.ExitReason.String != "trailing" {
		t.Fatalf("exit reason should come from sell event: got %q", row.ExitReason.String)
	}
}

// ---------------------------------------------------------------------------
// Multi-close from the same lot accumulates fees correctly
// ---------------------------------------------------------------------------

func TestRepeatedPartialClosesAccumulateFeesProportionally(t *testing.T) {
	svc, repo := quietService(t)
	ctx := context.Background()

	if _, err := svc.Record(ctx, fakeTx{}, FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "trade-1",
		InstrumentKey:    "X",
		Symbol:           "X",
		Side:             "buy",
		Quantity:         100,
		FilledPrice:      50,
		TotalFees:        10, // big round number so the math is easy
		ExecutedAt:       openedAt(5),
	}); err != nil {
		t.Fatalf("buy: %v", err)
	}
	// First partial close: 25 of 100.
	if _, err := svc.Record(ctx, fakeTx{}, FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "trade-sell-a",
		InstrumentKey:    "X",
		Symbol:           "X",
		Side:             "sell",
		Quantity:         25,
		FilledPrice:      60,
		TotalFees:        0,
		ExecutedAt:       openedAt(2),
	}); err != nil {
		t.Fatalf("sell 25: %v", err)
	}
	// Second partial close: 30 of remaining 75.
	if _, err := svc.Record(ctx, fakeTx{}, FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "trade-sell-b",
		InstrumentKey:    "X",
		Symbol:           "X",
		Side:             "sell",
		Quantity:         30,
		FilledPrice:      62,
		TotalFees:        0,
		ExecutedAt:       openedAt(1),
	}); err != nil {
		t.Fatalf("sell 30: %v", err)
	}

	if len(repo.closedLots) != 2 {
		t.Fatalf("expected 2 closed rows, got %d", len(repo.closedLots))
	}
	// Entry fees: 25 of 100 = 2.5, 30 of 100 = 3.0. Sum = 5.5.
	// The original lot still has 100-25-30 = 45 quantity remaining
	// and 10-5.5 = 4.5 of entry fees still "owed" to the eventual
	// final close. We don't track that explicitly, but verify the
	// proportions emitted so far:
	approxEqual(t, repo.closedLots[0].EntryFees, 2.5, 1e-9, "first.entry_fees")
	approxEqual(t, repo.closedLots[1].EntryFees, 3.0, 1e-9, "second.entry_fees")
	if repo.openLots[0].QuantityRemaining != 45 {
		t.Fatalf("expected remaining=45, got %f", repo.openLots[0].QuantityRemaining)
	}
}

// ---------------------------------------------------------------------------
// Error propagation
// ---------------------------------------------------------------------------

func TestRecordPropagatesRepoErrorFromOpen(t *testing.T) {
	svc, repo := quietService(t)
	repo.openErr = errors.New("db down")
	_, err := svc.Record(context.Background(), fakeTx{}, FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "trade-1",
		InstrumentKey:    "X",
		Symbol:           "X",
		Side:             "buy",
		Quantity:         1,
		FilledPrice:      1,
	})
	if err == nil {
		t.Fatal("expected open error")
	}
}

func TestRecordPropagatesRepoErrorFromClose(t *testing.T) {
	svc, repo := quietService(t)
	ctx := context.Background()
	_, _ = svc.Record(ctx, fakeTx{}, FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "trade-1",
		InstrumentKey:    "X",
		Symbol:           "X",
		Side:             "buy",
		Quantity:         5,
		FilledPrice:      10,
	})
	repo.closeErr = errors.New("partial close failed")
	_, err := svc.Record(ctx, fakeTx{}, FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "trade-sell",
		InstrumentKey:    "X",
		Symbol:           "X",
		Side:             "sell",
		Quantity:         3,
		FilledPrice:      11,
	})
	if err == nil {
		t.Fatal("expected close error")
	}
}

func TestClassifyFuturesSideMaps(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"buy", "buy"},
		{"BUY", "buy"},
		{"open_long", "buy"},
		{"close_short", "buy"},
		{"sell", "sell"},
		{"close_long", "sell"},
		{"open_short", ""},
		{"swap", ""},
	}
	for _, tc := range cases {
		got := ClassifyFuturesSide(tc.in)
		if got != tc.want {
			t.Errorf("ClassifyFuturesSide(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
