package fundshare

import (
	"errors"
	"math"
	"testing"
)

func TestApplySubscription_NoFee(t *testing.T) {
	state := State{UnitsOutstanding: 1000, Cash: 1000}
	r, err := ApplySubscription(state, 1.10, Subscription{Amount: 110})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if math.Abs(r.UnitsDelta-100) > 1e-9 {
		t.Errorf("units: got %v want 100", r.UnitsDelta)
	}
	if r.CashDelta != 110 {
		t.Errorf("cash delta: got %v want 110", r.CashDelta)
	}
	if r.UnitsAfter != 1100 {
		t.Errorf("units after: got %v want 1100", r.UnitsAfter)
	}
	if r.CashAfter != 1110 {
		t.Errorf("cash after: got %v want 1110", r.CashAfter)
	}
	if r.FeeAmount != 0 {
		t.Errorf("fee: got %v want 0", r.FeeAmount)
	}
}

func TestApplySubscription_WithEntryFee(t *testing.T) {
	state := State{UnitsOutstanding: 1000, Cash: 1000}
	r, err := ApplySubscription(state, 1.0, Subscription{Amount: 1000, EntryFeeRate: 0.01})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// fee=10, investable=990, units=990
	if r.FeeAmount != 10 {
		t.Errorf("fee: got %v want 10", r.FeeAmount)
	}
	if r.UnitsDelta != 990 {
		t.Errorf("units: got %v want 990", r.UnitsDelta)
	}
	if r.CashDelta != 990 {
		t.Errorf("cash delta excludes fee: got %v want 990", r.CashDelta)
	}
}

func TestApplyRedemption_NoFee(t *testing.T) {
	state := State{UnitsOutstanding: 1000, Cash: 2000}
	r, err := ApplyRedemption(state, 1.50, Redemption{Units: 100})
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if r.UnitsDelta != -100 {
		t.Errorf("units delta: got %v", r.UnitsDelta)
	}
	if r.CashDelta != -150 {
		t.Errorf("cash delta: got %v", r.CashDelta)
	}
	if r.PayoutToInvestor != 150 {
		t.Errorf("payout: got %v want 150", r.PayoutToInvestor)
	}
	if r.UnitsAfter != 900 {
		t.Errorf("units after: got %v", r.UnitsAfter)
	}
	if r.CashAfter != 1850 {
		t.Errorf("cash after: got %v", r.CashAfter)
	}
}

func TestApplyRedemption_WithExitFee(t *testing.T) {
	state := State{UnitsOutstanding: 1000, Cash: 1000}
	r, err := ApplyRedemption(state, 1.0, Redemption{Units: 100, ExitFeeRate: 0.005})
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	// gross = 100, fee = 0.5, payout = 99.5
	if r.FeeAmount != 0.5 {
		t.Errorf("fee: got %v", r.FeeAmount)
	}
	if r.PayoutToInvestor != 99.5 {
		t.Errorf("payout: got %v", r.PayoutToInvestor)
	}
}

func TestApplyRedemption_InsufficientUnits(t *testing.T) {
	state := State{UnitsOutstanding: 50, Cash: 1000}
	_, err := ApplyRedemption(state, 1.0, Redemption{Units: 100})
	if !errors.Is(err, ErrInsufficientUnits) {
		t.Fatalf("expected ErrInsufficientUnits, got %v", err)
	}
}

func TestApplyRedemption_InsufficientCash(t *testing.T) {
	state := State{UnitsOutstanding: 1000, Cash: 50}
	_, err := ApplyRedemption(state, 1.0, Redemption{Units: 100})
	if !errors.Is(err, ErrInsufficientCash) {
		t.Fatalf("expected ErrInsufficientCash, got %v", err)
	}
}

func TestRejectsBadInputs(t *testing.T) {
	state := State{UnitsOutstanding: 100, Cash: 100}
	if _, err := ApplySubscription(state, 0, Subscription{Amount: 100}); !errors.Is(err, ErrNonPositiveNAV) {
		t.Errorf("expected ErrNonPositiveNAV, got %v", err)
	}
	if _, err := ApplySubscription(state, 1.0, Subscription{Amount: 0}); !errors.Is(err, ErrNonPositiveOrder) {
		t.Errorf("expected ErrNonPositiveOrder, got %v", err)
	}
	if _, err := ApplyRedemption(state, -1, Redemption{Units: 1}); !errors.Is(err, ErrNonPositiveNAV) {
		t.Errorf("expected ErrNonPositiveNAV, got %v", err)
	}
	if _, err := ApplyRedemption(state, 1.0, Redemption{Units: 0}); !errors.Is(err, ErrNonPositiveOrder) {
		t.Errorf("expected ErrNonPositiveOrder, got %v", err)
	}
}
