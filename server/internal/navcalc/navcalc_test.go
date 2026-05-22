package navcalc

import (
	"errors"
	"math"
	"testing"
)

func TestCompute_BasicFormula(t *testing.T) {
	r, err := Compute(Inputs{
		Cash:                 1000,
		MarketValue:          4000,
		AccruedManagementFee: 50,
		UnitsOutstanding:     1000,
	})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if r.TotalAssets != 5000 {
		t.Errorf("total_assets: got %v want 5000", r.TotalAssets)
	}
	if r.NetAssets != 4950 {
		t.Errorf("net_assets: got %v want 4950", r.NetAssets)
	}
	if math.Abs(r.NAVPerUnit-4.95) > 1e-9 {
		t.Errorf("nav_per_unit: got %v want 4.95", r.NAVPerUnit)
	}
}

func TestCompute_AggregatesAllAccruals(t *testing.T) {
	r, err := Compute(Inputs{
		Cash:                  1000,
		MarketValue:           1000,
		AccruedManagementFee:  10,
		AccruedPerformanceFee: 20,
		OtherAccruedFees:      5,
		UnitsOutstanding:      100,
	})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if r.AccruedFeesTotal != 35 {
		t.Errorf("accrued: got %v want 35", r.AccruedFeesTotal)
	}
	want := (2000.0 - 35) / 100
	if math.Abs(r.NAVPerUnit-want) > 1e-9 {
		t.Errorf("nav: got %v want %v", r.NAVPerUnit, want)
	}
}

func TestCompute_RejectsZeroUnits(t *testing.T) {
	_, err := Compute(Inputs{Cash: 100, UnitsOutstanding: 0})
	if !errors.Is(err, ErrUnitsOutstanding) {
		t.Fatalf("expected ErrUnitsOutstanding, got %v", err)
	}
	_, err = Compute(Inputs{Cash: 100, UnitsOutstanding: -1})
	if !errors.Is(err, ErrUnitsOutstanding) {
		t.Fatalf("expected ErrUnitsOutstanding for negative, got %v", err)
	}
}

func TestCompute_AllowsNegativeCash(t *testing.T) {
	// margin-borrowed fund: cash is negative
	r, err := Compute(Inputs{
		Cash:             -500,
		MarketValue:      2000,
		UnitsOutstanding: 100,
	})
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if r.NAVPerUnit != 15.0 {
		t.Fatalf("nav: got %v want 15", r.NAVPerUnit)
	}
}

func TestDailyReturn(t *testing.T) {
	if got := DailyReturn(1.0, 1.05); math.Abs(got-0.05) > 1e-9 {
		t.Errorf("got %v", got)
	}
	if got := DailyReturn(0, 1.05); got != 0 {
		t.Errorf("expected 0 for zero prev, got %v", got)
	}
}

func TestCumulativeReturn(t *testing.T) {
	if got := CumulativeReturn(1.0, 1.20); math.Abs(got-0.20) > 1e-9 {
		t.Errorf("got %v", got)
	}
}
