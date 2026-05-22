package main

import (
	"database/sql"
	"testing"

	"github.com/fundai/server/internal/repository"
)

// Tests for T+1 settlement plumbing in mergeBoughtPosition +
// releaseLockedShares. The risk-rule side is covered separately under
// internal/risk; this file just verifies the trading-engine layer
// (the place that actually mutates AvailableQty).

func planActionFor(symbol, market string) repository.PlanAction {
	return repository.PlanAction{
		Symbol:     symbol,
		Market:     sql.NullString{String: market, Valid: market != ""},
		Exchange:   sql.NullString{String: "", Valid: false},
		AssetClass: sql.NullString{String: "", Valid: false},
	}
}

func TestMergeBoughtPosition_T1MarketLocksNewQuantity(t *testing.T) {
	// Fresh buy on A-share: Quantity goes up to 100, AvailableQty
	// stays 0 (the lot is locked until the next Settle).
	action := planActionFor("600519", "a_share")
	got := mergeBoughtPosition(repository.HoldingPosition{}, "fund-1", action, 100, 1700)
	if got.Quantity != 100 {
		t.Errorf("Quantity = %v, want 100", got.Quantity)
	}
	if got.AvailableQty != 0 {
		t.Errorf("AvailableQty = %v, want 0 (locked)", got.AvailableQty)
	}
}

func TestMergeBoughtPosition_T1OnExistingSettledPositionKeepsPriorAvailable(t *testing.T) {
	// 500 settled shares + buy 200 new on T+1 → Quantity 700,
	// AvailableQty still 500 (only the prior settled lot is sellable
	// today; the freshly bought 200 are locked).
	prev := repository.HoldingPosition{
		FundID:        "fund-1",
		InstrumentKey: "SH:600519",
		Symbol:        "600519",
		Market:        sql.NullString{String: "a_share", Valid: true},
		Quantity:      500,
		AvailableQty:  500,
		CostPrice:     1500,
	}
	action := planActionFor("600519", "a_share")
	got := mergeBoughtPosition(prev, "fund-1", action, 200, 1800)
	if got.Quantity != 700 {
		t.Errorf("Quantity = %v, want 700", got.Quantity)
	}
	if got.AvailableQty != 500 {
		t.Errorf("AvailableQty = %v, want 500 (only prior settled lot)", got.AvailableQty)
	}
}

func TestMergeBoughtPosition_T0MarketImmediatelyAvailable(t *testing.T) {
	// On T+0 markets the freshly bought lot is sellable on the same
	// day → AvailableQty must mirror Quantity.
	cases := []struct {
		name   string
		symbol string
		market string
	}{
		{"us-equity", "AAPL", "us_stock"},
		{"hk-equity", "0700", "hk_stock"},
		{"crypto", "BTCUSDT", "crypto"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action := planActionFor(tc.symbol, tc.market)
			got := mergeBoughtPosition(repository.HoldingPosition{}, "fund-1", action, 100, 200)
			if got.AvailableQty != got.Quantity {
				t.Errorf("%s: AvailableQty %v ≠ Quantity %v", tc.symbol, got.AvailableQty, got.Quantity)
			}
		})
	}
}

func TestMergeBoughtPosition_T1CapsPriorAvailableAtTotal(t *testing.T) {
	// Belt-and-suspenders: a corrupt position with AvailableQty >
	// Quantity must not propagate after the merge — clamp to total.
	prev := repository.HoldingPosition{
		FundID:        "fund-1",
		InstrumentKey: "SH:600519",
		Symbol:        "600519",
		Market:        sql.NullString{String: "a_share", Valid: true},
		Quantity:      100,
		AvailableQty:  9999,
	}
	action := planActionFor("600519", "a_share")
	got := mergeBoughtPosition(prev, "fund-1", action, 50, 1700)
	if got.Quantity != 150 {
		t.Errorf("Quantity = %v, want 150", got.Quantity)
	}
	if got.AvailableQty > got.Quantity {
		t.Errorf("AvailableQty %v must be ≤ Quantity %v", got.AvailableQty, got.Quantity)
	}
}

func TestReleaseLockedShares_UnlocksT1Positions(t *testing.T) {
	positions := map[string]repository.HoldingPosition{
		"SH:600519": {
			Symbol:       "600519",
			Market:       sql.NullString{String: "a_share", Valid: true},
			Quantity:     1000,
			AvailableQty: 400, // 600 still locked from today's buys
		},
		"NASDAQ:AAPL": {
			Symbol:       "AAPL",
			Market:       sql.NullString{String: "us_stock", Valid: true},
			Quantity:     50,
			AvailableQty: 50, // already in sync; T+0 anyway
		},
	}
	releaseLockedShares(positions)
	if got := positions["SH:600519"].AvailableQty; got != 1000 {
		t.Errorf("A-share AvailableQty = %v, want 1000 (fully released)", got)
	}
	if got := positions["NASDAQ:AAPL"].AvailableQty; got != 50 {
		t.Errorf("T+0 position must be untouched: AvailableQty = %v", got)
	}
}

func TestReleaseLockedShares_IsIdempotent(t *testing.T) {
	positions := map[string]repository.HoldingPosition{
		"SH:600519": {
			Symbol:       "600519",
			Market:       sql.NullString{String: "a_share", Valid: true},
			Quantity:     1000,
			AvailableQty: 400,
		},
	}
	releaseLockedShares(positions)
	releaseLockedShares(positions) // second call is a no-op
	if got := positions["SH:600519"].AvailableQty; got != 1000 {
		t.Errorf("AvailableQty = %v, want 1000 after double release", got)
	}
}

func TestReleaseLockedShares_SkipsAlreadyReleased(t *testing.T) {
	// AvailableQty already equals Quantity → no mutation (avoids
	// pointlessly re-writing rows during Settle).
	positions := map[string]repository.HoldingPosition{
		"SH:600519": {
			Symbol:       "600519",
			Market:       sql.NullString{String: "a_share", Valid: true},
			Quantity:     500,
			AvailableQty: 500,
		},
	}
	releaseLockedShares(positions)
	if got := positions["SH:600519"].AvailableQty; got != 500 {
		t.Errorf("AvailableQty = %v, want unchanged 500", got)
	}
}
