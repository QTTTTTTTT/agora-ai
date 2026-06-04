package lotledger

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/fundai/server/internal/repository"
)

// TestService_ShortOpen_OpensSideShortLot pins the simplest short
// case: a sell with PositionSide=short routes to recordShortOpen
// and creates a position_lots row with side='short'.
func TestService_ShortOpen_OpensSideShortLot(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ev := FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "trade-short-open-1",
		InstrumentKey:    "NASDAQ:TSLA",
		Symbol:           "TSLA",
		Side:             "sell",
		PositionSide:     "short",
		Quantity:         100,
		FilledPrice:      200.0,
		TotalFees:        5.0,
		ExecutedAt:       time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
		Sleeve:           sql.NullString{String: "llm_pm", Valid: true},
	}

	result, err := svc.Record(context.Background(), &fakeTx{}, ev)
	if err != nil {
		t.Fatalf("Record short open: %v", err)
	}
	if result.OpenedLotID == "" {
		t.Fatalf("expected OpenedLotID, got empty")
	}
	if len(repo.openLots) != 1 {
		t.Fatalf("expected 1 open lot, got %d", len(repo.openLots))
	}
	lot := repo.openLots[0]
	if lot.Side != "short" {
		t.Errorf("expected side=short, got %q", lot.Side)
	}
	if lot.QuantityOpened != 100 || lot.QuantityRemaining != 100 {
		t.Errorf("expected qty 100/100, got %v/%v", lot.QuantityOpened, lot.QuantityRemaining)
	}
	if lot.EntryPrice != 200.0 {
		t.Errorf("expected entry_price 200, got %v", lot.EntryPrice)
	}
}

// TestService_ShortClose_ProfitableCoverComputesNegPnLSign pins the
// short-side PnL sign convention. Selling short at $200 and covering
// at $180 nets $20 * qty - fees profit; the realized_pnl row must
// be POSITIVE.
//
// This is the inverse of a long roundtrip which would net negative
// on the same price movement (bought at 200, sold at 180 = loss).
func TestService_ShortClose_ProfitableCoverComputesNegPnLSign(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	openTime := time.Date(2026, 5, 28, 10, 0, 0, 0, time.UTC)
	openEv := FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "trade-short-open",
		InstrumentKey:    "NASDAQ:TSLA",
		Symbol:           "TSLA",
		Side:             "sell",
		PositionSide:     "short",
		Quantity:         100,
		FilledPrice:      200.0,
		TotalFees:        5.0,
		ExecutedAt:       openTime,
		Sleeve:           sql.NullString{String: "llm_pm", Valid: true},
	}
	if _, err := svc.Record(ctx, &fakeTx{}, openEv); err != nil {
		t.Fatalf("open short: %v", err)
	}

	coverEv := FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "trade-short-close",
		InstrumentKey:    "NASDAQ:TSLA",
		Symbol:           "TSLA",
		Side:             "buy", // buy-to-cover
		PositionSide:     "short",
		Quantity:         100,
		FilledPrice:      180.0,
		TotalFees:        4.0,
		ExecutedAt:       openTime.Add(72 * time.Hour),
	}
	result, err := svc.Record(ctx, &fakeTx{}, coverEv)
	if err != nil {
		t.Fatalf("cover short: %v", err)
	}
	if result.QuantityClosed != 100 {
		t.Fatalf("expected closed qty=100, got %v", result.QuantityClosed)
	}

	if len(repo.closedLots) != 1 {
		t.Fatalf("expected 1 closed lot, got %d", len(repo.closedLots))
	}
	closed := repo.closedLots[0]
	if closed.Side != "short" {
		t.Errorf("expected closed_lots.side=short, got %q", closed.Side)
	}
	// Gross PnL = (200 - 180) * 100 = +2000
	// Fees: entry 5 (100% attributed) + exit 4 (100% attributed) = 9
	// Net PnL = 2000 - 9 = 1991 (rounded to currency).
	expectedNet := 2000.0 - 9.0
	if math.Abs(closed.RealizedPnL-expectedNet) > 0.01 {
		t.Errorf("expected net PnL ~%.2f, got %.2f", expectedNet, closed.RealizedPnL)
	}
	if closed.RealizedPnL <= 0 {
		t.Errorf("profitable short cover must produce POSITIVE realized_pnl, got %v", closed.RealizedPnL)
	}
	// Pct: net / (entry_price * qty) = 1991 / 20000 = 0.09955
	if math.Abs(closed.RealizedPnLPct-0.09955) > 0.001 {
		t.Errorf("expected PnL pct ~0.09955, got %v", closed.RealizedPnLPct)
	}
}

// TestService_ShortClose_SqueezeCoverComputesNegPnL pins the loss
// case: sold short at $200, forced to cover at $250 = $50 * qty
// loss. realized_pnl must be NEGATIVE.
func TestService_ShortClose_SqueezeCoverComputesNegPnL(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	openTime := time.Date(2026, 5, 28, 10, 0, 0, 0, time.UTC)
	_, err := svc.Record(ctx, &fakeTx{}, FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "open-1",
		InstrumentKey:    "X",
		Symbol:           "X",
		Side:             "sell",
		PositionSide:     "short",
		Quantity:         100,
		FilledPrice:      200.0,
		TotalFees:        5.0,
		ExecutedAt:       openTime,
	})
	if err != nil {
		t.Fatalf("open short: %v", err)
	}

	result, err := svc.Record(ctx, &fakeTx{}, FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "close-1",
		InstrumentKey:    "X",
		Symbol:           "X",
		Side:             "buy",
		PositionSide:     "short",
		Quantity:         100,
		FilledPrice:      250.0,
		TotalFees:        4.0,
		ExecutedAt:       openTime.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("squeeze cover: %v", err)
	}

	if result.RealizedPnL >= 0 {
		t.Fatalf("squeeze cover must produce NEGATIVE realized_pnl, got %v", result.RealizedPnL)
	}
	// Gross loss = (200 - 250) * 100 = -5000
	// Plus fees -9 → -5009.
	if math.Abs(result.RealizedPnL-(-5009.0)) > 0.01 {
		t.Errorf("expected ~-5009, got %v", result.RealizedPnL)
	}
}

// TestService_ShortClose_FIFOAcrossMultipleLots covers the
// multi-lot FIFO scenario. Two short opens at different prices,
// one cover that crosses both lots, expected to consume the
// oldest lot first and emit two closed_lots rows.
func TestService_ShortClose_FIFOAcrossMultipleLots(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	openTime := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

	// Lot 1: 60 shares short @ 200, oldest.
	if _, err := svc.Record(ctx, &fakeTx{}, FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "open-1",
		InstrumentKey:    "X", Symbol: "X",
		Side: "sell", PositionSide: "short",
		Quantity: 60, FilledPrice: 200.0,
		ExecutedAt: openTime,
	}); err != nil {
		t.Fatalf("open lot 1: %v", err)
	}
	// Lot 2: 40 shares short @ 220, newer.
	if _, err := svc.Record(ctx, &fakeTx{}, FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "open-2",
		InstrumentKey:    "X", Symbol: "X",
		Side: "sell", PositionSide: "short",
		Quantity: 40, FilledPrice: 220.0,
		ExecutedAt: openTime.Add(time.Hour),
	}); err != nil {
		t.Fatalf("open lot 2: %v", err)
	}

	// Cover 100 shares @ 180. FIFO must consume lot 1 (60 @ 200)
	// first, then lot 2 (40 @ 220).
	result, err := svc.Record(ctx, &fakeTx{}, FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "close-1",
		InstrumentKey:    "X", Symbol: "X",
		Side: "buy", PositionSide: "short",
		Quantity: 100, FilledPrice: 180.0,
		ExecutedAt: openTime.Add(48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("cover: %v", err)
	}
	if result.QuantityClosed != 100 {
		t.Fatalf("expected closed=100, got %v", result.QuantityClosed)
	}
	if len(result.ClosedLotIDs) != 2 {
		t.Fatalf("expected 2 closed lot ids (FIFO consumed both), got %d", len(result.ClosedLotIDs))
	}

	if len(repo.closedLots) != 2 {
		t.Fatalf("expected 2 closed_lots rows, got %d", len(repo.closedLots))
	}
	// Lot 1: 60 @ 200 -> 180 = (200-180)*60 = +1200
	// Lot 2: 40 @ 220 -> 180 = (220-180)*40 = +1600
	// Sum = +2800 (no fees this run since TotalFees=0).
	expectedTotal := 1200.0 + 1600.0
	if math.Abs(result.RealizedPnL-expectedTotal) > 0.01 {
		t.Errorf("expected total PnL %.2f, got %.2f", expectedTotal, result.RealizedPnL)
	}

	// FIFO order: oldest closed first.
	if repo.closedLots[0].PositionLotID == repo.closedLots[1].PositionLotID {
		t.Fatal("both closed_lots rows point at the same lot — FIFO didn't walk two distinct lots")
	}
	// The first closed row should be against the older (lot 1).
	for _, l := range repo.openLots {
		if l.OpeningTradeID == "open-1" {
			if repo.closedLots[0].PositionLotID != l.ID {
				t.Errorf("expected first closed_lots row to point at oldest lot (open-1, lot id %q), got %q",
					l.ID, repo.closedLots[0].PositionLotID)
			}
		}
	}
}

// TestService_ShortClose_PartialCoverLeavesRemainder pins the
// partial-cover case: only part of an existing short lot is covered,
// leaving the lot with status='partial' and quantity_remaining set.
func TestService_ShortClose_PartialCoverLeavesRemainder(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	openTime := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	if _, err := svc.Record(ctx, &fakeTx{}, FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "open-1",
		InstrumentKey:    "X", Symbol: "X",
		Side: "sell", PositionSide: "short",
		Quantity: 100, FilledPrice: 200.0,
		ExecutedAt: openTime,
	}); err != nil {
		t.Fatalf("open: %v", err)
	}

	// Cover only 30 of the 100. Lot should drop to remaining=70,
	// status=partial.
	result, err := svc.Record(ctx, &fakeTx{}, FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "partial-cover",
		InstrumentKey:    "X", Symbol: "X",
		Side: "buy", PositionSide: "short",
		Quantity: 30, FilledPrice: 180.0,
		ExecutedAt: openTime.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("partial cover: %v", err)
	}
	if result.QuantityClosed != 30 {
		t.Errorf("expected closed=30, got %v", result.QuantityClosed)
	}

	if len(repo.openLots) != 1 {
		t.Fatalf("expected 1 lot still present, got %d", len(repo.openLots))
	}
	lot := repo.openLots[0]
	if lot.Status != "partial" {
		t.Errorf("expected status=partial, got %q", lot.Status)
	}
	if lot.QuantityRemaining != 70 {
		t.Errorf("expected qty_remaining=70, got %v", lot.QuantityRemaining)
	}
}

// TestService_ShortClose_OrphanCoverIsSoftError pins the
// graceful-degrade behaviour when there's no matching short lot.
// A buy-to-cover with no open short lot is an orphan: we log + count
// but don't fail the trade, same as the long-side orphan-sell path.
func TestService_ShortClose_OrphanCoverIsSoftError(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo, slog.New(slog.NewTextHandler(io.Discard, nil)))

	result, err := svc.Record(context.Background(), &fakeTx{}, FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "orphan-cover",
		InstrumentKey:    "X", Symbol: "X",
		Side: "buy", PositionSide: "short",
		Quantity: 50, FilledPrice: 180.0,
	})
	if err != nil {
		t.Fatalf("orphan cover must NOT error: %v", err)
	}
	if result.QuantityOrphaned != 50 {
		t.Errorf("expected orphan qty=50, got %v", result.QuantityOrphaned)
	}
	if result.QuantityClosed != 0 {
		t.Errorf("expected closed qty=0 (no matching short lot), got %v", result.QuantityClosed)
	}
	if len(repo.closedLots) != 0 {
		t.Errorf("expected zero closed_lots rows, got %d", len(repo.closedLots))
	}
}

// TestService_LongAndShortLotsCoexistOnSameInstrument is the
// isolation invariant: opening a long lot AND a short lot for the
// same (fund, instrument) must NOT cross-contaminate. A long sell
// must consume long lots only, and a short cover must consume short
// lots only. This is what protects the long path from regressing
// after T8 wires in the short side.
func TestService_LongAndShortLotsCoexistOnSameInstrument(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	openTime := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

	// Long lot: 60 shares @ 100 (PositionSide unset → long).
	if _, err := svc.Record(ctx, &fakeTx{}, FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "long-open",
		InstrumentKey:    "X", Symbol: "X",
		Side:     "buy",
		Quantity: 60, FilledPrice: 100.0,
		ExecutedAt: openTime,
	}); err != nil {
		t.Fatalf("long open: %v", err)
	}
	// Short lot: 40 shares @ 120 (PositionSide=short).
	if _, err := svc.Record(ctx, &fakeTx{}, FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "short-open",
		InstrumentKey:    "X", Symbol: "X",
		Side: "sell", PositionSide: "short",
		Quantity: 40, FilledPrice: 120.0,
		ExecutedAt: openTime.Add(time.Hour),
	}); err != nil {
		t.Fatalf("short open: %v", err)
	}

	if len(repo.openLots) != 2 {
		t.Fatalf("expected 2 open lots, got %d", len(repo.openLots))
	}

	// Sell 30 on the long side. Must consume long lot only.
	if _, err := svc.Record(ctx, &fakeTx{}, FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "long-sell",
		InstrumentKey:    "X", Symbol: "X",
		Side:     "sell",
		Quantity: 30, FilledPrice: 110.0,
		ExecutedAt: openTime.Add(2 * time.Hour),
	}); err != nil {
		t.Fatalf("long sell: %v", err)
	}
	// Cover 20 on the short side. Must consume short lot only.
	if _, err := svc.Record(ctx, &fakeTx{}, FillEvent{
		FundID:           "fund-1",
		TradeExecutionID: "short-cover",
		InstrumentKey:    "X", Symbol: "X",
		Side: "buy", PositionSide: "short",
		Quantity: 20, FilledPrice: 100.0,
		ExecutedAt: openTime.Add(3 * time.Hour),
	}); err != nil {
		t.Fatalf("short cover: %v", err)
	}

	// Long lot now has 30 remaining; short lot has 20 remaining.
	var longLot, shortLot *repository.PositionLotRow
	for _, l := range repo.openLots {
		switch l.Side {
		case "", "long":
			longLot = l
		case "short":
			shortLot = l
		}
	}
	if longLot == nil {
		t.Fatal("long lot missing")
	}
	if shortLot == nil {
		t.Fatal("short lot missing")
	}
	if longLot.QuantityRemaining != 30 {
		t.Errorf("long lot remaining: want 30, got %v", longLot.QuantityRemaining)
	}
	if shortLot.QuantityRemaining != 20 {
		t.Errorf("short lot remaining: want 20, got %v", shortLot.QuantityRemaining)
	}

	// Two closed_lots rows: one side=long, one side=short.
	if len(repo.closedLots) != 2 {
		t.Fatalf("expected 2 closed_lots, got %d", len(repo.closedLots))
	}
	sideCount := map[string]int{}
	for _, c := range repo.closedLots {
		sideCount[c.Side]++
	}
	if sideCount["long"] != 1 || sideCount["short"] != 1 {
		t.Errorf("expected 1 long-side close + 1 short-side close, got %v", sideCount)
	}
}
