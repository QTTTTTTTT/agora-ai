// pmpath_lotsize_guard_test.go — regressions for the PM-direct-fill
// lot-size guard (executePlanAction → tradeRepoCreateAndFill bypass
// of broker.LotSizeGate).
//
// The trigger story is the 2026-06-03 OCS fund audit: STAR-board
// instruments 688205 / 688195 accumulated 105 / 283-share odd-lot
// residuals because partial sells (62, 85, 104 shares) left a
// residual below the STAR MinLot=200, which the broker-side gate
// would have rejected — but the PM direct-fill path bypassed that
// gate. These tests pin the new in-engine guard to the rules
// codified in instrument.SpecFor (STAR 200/1, ChiNext 100/100,
// SH-main 100/100, BSE 100/1).
//
// Naming convention: each test exercises one (board, side, qty,
// holding) combination and asserts the verdict shape (allow vs
// reject + the reason fragment). The guard is a pure function on
// the engine receiver — we only pass the `metrics` field so the
// recordPMPathLotSizeReject call doesn't NPE.

package main

import (
	"errors"
	"strings"
	"testing"

	"database/sql"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/repository"
)

func ashareAction(symbol string) repository.PlanAction {
	return repository.PlanAction{
		Symbol:     symbol,
		Market:     sql.NullString{String: "a_share", Valid: true},
		Exchange:   sql.NullString{String: "SSE", Valid: true},
		AssetClass: sql.NullString{String: "equity", Valid: true},
	}
}

func TestPMPathLotSizeGuard_AllowsValidSTARBuy(t *testing.T) {
	e := &runtimeTradingEngine{metrics: &serverMetrics{}}
	action := ashareAction("688205") // STAR: MinLot=200, Step=1
	if err := e.pmPathLotSizeGuard("buy", action, 393, 0, repository.HoldingPosition{}); err != nil {
		t.Fatalf("expected allow for STAR buy 393, got: %v", err)
	}
	if err := e.pmPathLotSizeGuard("buy", action, 200, 0, repository.HoldingPosition{}); err != nil {
		t.Fatalf("expected allow for STAR buy at MinLot 200, got: %v", err)
	}
}

func TestPMPathLotSizeGuard_RejectsSTARBuyBelowMinLot(t *testing.T) {
	e := &runtimeTradingEngine{metrics: &serverMetrics{}}
	action := ashareAction("688205")
	for _, qty := range []int{4, 100, 199} {
		err := e.pmPathLotSizeGuard("buy", action, qty, 0, repository.HoldingPosition{})
		if err == nil {
			t.Fatalf("expected reject for STAR buy qty=%d (below MinLot 200), got nil", qty)
		}
		if !errors.Is(err, api.ErrConflict) {
			t.Errorf("qty=%d: expected api.ErrConflict, got: %v", qty, err)
		}
		if !strings.Contains(err.Error(), "688205") || !strings.Contains(err.Error(), "lot-size") {
			t.Errorf("qty=%d: error should mention symbol+lot-size, got: %v", qty, err)
		}
	}
}

func TestPMPathLotSizeGuard_RejectsChinextBuyBelow100(t *testing.T) {
	e := &runtimeTradingEngine{metrics: &serverMetrics{}}
	action := ashareAction("301308") // ChiNext: MinLot=100, Step=100
	for _, qty := range []int{1, 50, 99, 150, 200} {
		// 1/50/99 are below min; 150/200 mod 100 only 200 passes
		err := e.pmPathLotSizeGuard("buy", action, qty, 0, repository.HoldingPosition{})
		shouldReject := qty < 100 || qty%100 != 0
		if shouldReject && err == nil {
			t.Errorf("ChiNext buy qty=%d: expected reject, got allow", qty)
		}
		if !shouldReject && err != nil {
			t.Errorf("ChiNext buy qty=%d: expected allow, got reject: %v", qty, err)
		}
	}
}

func TestPMPathLotSizeGuard_AllowsFullPositionSellEvenIfOddLot(t *testing.T) {
	// A 43-share STAR holding (from a residual the engine should
	// never have produced, but we still need to be able to clear)
	// MUST be sellable in one shot: full-position sells are always
	// legal regardless of MinLot.
	e := &runtimeTradingEngine{metrics: &serverMetrics{}}
	action := ashareAction("688205")
	pos := repository.HoldingPosition{Quantity: 43, AvailableQty: 43}
	if err := e.pmPathLotSizeGuard("sell", action, 43, 0, pos); err != nil {
		t.Fatalf("full-position odd-lot sell must always be allowed, got: %v", err)
	}
}

func TestPMPathLotSizeGuard_RejectsSTARSellLeavingOddLotResidual(t *testing.T) {
	// This is the OCS-fund regression: holding 209 STAR shares,
	// sell 62 → residual 147 < MinLot 200 → must liquidate full
	// 209 instead. The guard must reject the 62-share sell.
	e := &runtimeTradingEngine{metrics: &serverMetrics{}}
	action := ashareAction("688205")
	pos := repository.HoldingPosition{Quantity: 209, AvailableQty: 209}
	err := e.pmPathLotSizeGuard("sell", action, 62, 0, pos)
	if err == nil {
		t.Fatalf("expected reject for STAR partial sell leaving residual 147 < 200, got nil")
	}
	if !errors.Is(err, api.ErrConflict) {
		t.Errorf("expected api.ErrConflict, got: %v", err)
	}
	if !strings.Contains(err.Error(), "residual") && !strings.Contains(err.Error(), "liquidate") {
		t.Errorf("error should explain odd-lot residual, got: %v", err)
	}
	// And the second OCS regression: sell 104 against holding 147.
	pos2 := repository.HoldingPosition{Quantity: 147, AvailableQty: 147}
	if err := e.pmPathLotSizeGuard("sell", action, 104, 0, pos2); err == nil {
		t.Fatalf("expected reject for STAR sell 104 on holding 147 (residual 43 < 200), got nil")
	}
}

func TestPMPathLotSizeGuard_AllowsSTARSellLeavingFullLotResidual(t *testing.T) {
	// Holding 500, sell 200 → residual 300 ≥ MinLot 200 → legal.
	e := &runtimeTradingEngine{metrics: &serverMetrics{}}
	action := ashareAction("688205")
	pos := repository.HoldingPosition{Quantity: 500, AvailableQty: 500}
	if err := e.pmPathLotSizeGuard("sell", action, 200, 0, pos); err != nil {
		t.Fatalf("STAR sell 200 on holding 500 (residual 300) must be allowed, got: %v", err)
	}
}

func TestPMPathLotSizeGuard_RejectsSellWhenNoPosition(t *testing.T) {
	e := &runtimeTradingEngine{metrics: &serverMetrics{}}
	action := ashareAction("688205")
	err := e.pmPathLotSizeGuard("sell", action, 100, 0, repository.HoldingPosition{})
	if err == nil {
		t.Fatalf("expected reject for sell with no holding, got nil")
	}
	if !errors.Is(err, api.ErrConflict) {
		t.Errorf("expected api.ErrConflict, got: %v", err)
	}
}

func TestPMPathLotSizeGuard_PassesThroughNonAShare(t *testing.T) {
	e := &runtimeTradingEngine{metrics: &serverMetrics{}}
	// US equity (AAPL): non-A-share, guard short-circuits to allow.
	action := repository.PlanAction{
		Symbol:     "AAPL",
		Market:     sql.NullString{String: "us_stock", Valid: true},
		Exchange:   sql.NullString{String: "NASDAQ", Valid: true},
		AssetClass: sql.NullString{String: "equity", Valid: true},
	}
	if err := e.pmPathLotSizeGuard("buy", action, 1, 0, repository.HoldingPosition{}); err != nil {
		t.Fatalf("non-A-share must short-circuit allow, got: %v", err)
	}
	pos := repository.HoldingPosition{Quantity: 10, AvailableQty: 10}
	if err := e.pmPathLotSizeGuard("sell", action, 3, 0, pos); err != nil {
		t.Fatalf("non-A-share partial sell must short-circuit allow, got: %v", err)
	}
}

func TestPMPathLotSizeGuard_AllowsSHMainBuyAtMinLot(t *testing.T) {
	// SH main: MinLot=100, Step=100. 600519 (Moutai), 393 → reject
	// (not a 100 multiple); 400 → allow.
	e := &runtimeTradingEngine{metrics: &serverMetrics{}}
	action := repository.PlanAction{
		Symbol:     "600519",
		Market:     sql.NullString{String: "a_share", Valid: true},
		Exchange:   sql.NullString{String: "SSE", Valid: true},
		AssetClass: sql.NullString{String: "equity", Valid: true},
	}
	if err := e.pmPathLotSizeGuard("buy", action, 393, 0, repository.HoldingPosition{}); err == nil {
		t.Fatalf("SH-main buy 393 (not 100-multiple) must reject, got allow")
	}
	if err := e.pmPathLotSizeGuard("buy", action, 400, 0, repository.HoldingPosition{}); err != nil {
		t.Fatalf("SH-main buy 400 must allow, got: %v", err)
	}
}

func TestPMPathLotSizeGuard_ZeroQuantityIsNoOp(t *testing.T) {
	e := &runtimeTradingEngine{metrics: &serverMetrics{}}
	action := ashareAction("688205")
	if err := e.pmPathLotSizeGuard("buy", action, 0, 0, repository.HoldingPosition{}); err != nil {
		t.Fatalf("qty=0 should be a no-op (the upstream `quantity <= 0 → cancelled` branch catches it), got: %v", err)
	}
	if err := e.pmPathLotSizeGuard("buy", action, -5, 0, repository.HoldingPosition{}); err != nil {
		t.Fatalf("qty<0 should be a no-op, got: %v", err)
	}
}

func TestPMPathLotSizeGuard_RejectsAShareSubCentPrice(t *testing.T) {
	// A-share tick = 0.01 CNY. A LLM PM occasionally emits a
	// limit price with sub-cent precision (e.g. 247.6234 from
	// VWAP slicing) — the broker would round it down, but the
	// audit trail must record the actual fill price. We reject
	// pre-trade so the PM re-prices.
	e := &runtimeTradingEngine{metrics: &serverMetrics{}}
	action := ashareAction("688205")
	err := e.pmPathLotSizeGuard("buy", action, 200, 247.6234, repository.HoldingPosition{})
	if err == nil {
		t.Fatalf("expected reject for A-share sub-cent price 247.6234, got nil")
	}
	if !errors.Is(err, api.ErrConflict) {
		t.Errorf("expected api.ErrConflict, got: %v", err)
	}
	if !strings.Contains(err.Error(), "tick-size") {
		t.Errorf("error should mention tick-size, got: %v", err)
	}
	// And a sell-side regression: sub-cent on a partial sell
	// must also reject (here we make residual full-lot-aligned
	// so the lot-size check passes, isolating the tick failure).
	pos := repository.HoldingPosition{Quantity: 500, AvailableQty: 500}
	if err := e.pmPathLotSizeGuard("sell", action, 200, 247.6234, pos); err == nil {
		t.Errorf("A-share sell at sub-cent must reject, got allow")
	}
}

func TestPMPathLotSizeGuard_AllowsAShareCentAlignedPrice(t *testing.T) {
	// Round-trip a clean 0.01-aligned price (the common case).
	e := &runtimeTradingEngine{metrics: &serverMetrics{}}
	action := ashareAction("688205")
	if err := e.pmPathLotSizeGuard("buy", action, 200, 247.60, repository.HoldingPosition{}); err != nil {
		t.Fatalf("aligned A-share buy must allow, got: %v", err)
	}
	pos := repository.HoldingPosition{Quantity: 500, AvailableQty: 500}
	if err := e.pmPathLotSizeGuard("sell", action, 200, 247.60, pos); err != nil {
		t.Fatalf("aligned A-share sell must allow, got: %v", err)
	}
}

func TestPMPathLotSizeGuard_AllowsMarketOrderAtAnyPrice(t *testing.T) {
	// orderPrice = 0 signifies a market order in this code
	// path; tick alignment is the broker's responsibility once
	// the matcher picks a price, so the guard short-circuits.
	e := &runtimeTradingEngine{metrics: &serverMetrics{}}
	action := ashareAction("688205")
	if err := e.pmPathLotSizeGuard("buy", action, 200, 0, repository.HoldingPosition{}); err != nil {
		t.Fatalf("market-order buy at orderPrice=0 must allow, got: %v", err)
	}
}
