package feeacct

import (
	"errors"
	"math"
	"testing"
)

func TestAccrueDailyManagementFee(t *testing.T) {
	cfg := ManagementFeeConfig{AnnualRate: 0.02, DaysInYear: 365}
	got := AccrueDailyManagementFee(cfg, 1_000_000)
	want := 1_000_000.0 * 0.02 / 365
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestAccrueDailyManagementFee_DefaultsTo365(t *testing.T) {
	cfg := ManagementFeeConfig{AnnualRate: 0.02} // DaysInYear unset
	got := AccrueDailyManagementFee(cfg, 365_000)
	want := 365_000.0 * 0.02 / 365
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestAccrueDailyManagementFee_ZeroOnNonPositive(t *testing.T) {
	cfg := ManagementFeeConfig{AnnualRate: 0.02}
	if AccrueDailyManagementFee(cfg, 0) != 0 {
		t.Error("expected 0 for zero NAV")
	}
	if AccrueDailyManagementFee(cfg, -100) != 0 {
		t.Error("expected 0 for negative NAV")
	}
	if AccrueDailyManagementFee(ManagementFeeConfig{AnnualRate: 0}, 1000) != 0 {
		t.Error("expected 0 for zero rate")
	}
}

func TestSettlePerformanceFee_NoFeeBelowHWM(t *testing.T) {
	cfg := PerformanceFeeConfig{Rate: 0.20}
	r, err := SettlePerformanceFee(cfg, PerformanceFeeInputs{
		HighWaterMark:     1.10,
		CurrentNAVPerUnit: 1.05,
		UnitsOutstanding:  1000,
	})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if r.FeeAmount != 0 {
		t.Errorf("fee should be 0, got %v", r.FeeAmount)
	}
	if r.NewHighWaterMark != 1.10 {
		t.Errorf("HWM should not advance, got %v", r.NewHighWaterMark)
	}
}

func TestSettlePerformanceFee_AdvancesHWMOnGain(t *testing.T) {
	cfg := PerformanceFeeConfig{Rate: 0.20}
	r, err := SettlePerformanceFee(cfg, PerformanceFeeInputs{
		HighWaterMark:     1.00,
		CurrentNAVPerUnit: 1.20,
		UnitsOutstanding:  1000,
	})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	// gain = 0.20/unit, fee = 0.20 * 0.20 * 1000 = 40
	if math.Abs(r.FeeAmount-40) > 1e-9 {
		t.Errorf("fee: got %v want 40", r.FeeAmount)
	}
	if r.NewHighWaterMark != 1.20 {
		t.Errorf("HWM should advance to 1.20, got %v", r.NewHighWaterMark)
	}
	if math.Abs(r.EligibleGainPerUnit-0.20) > 1e-9 {
		t.Errorf("eligible gain: got %v want 0.20", r.EligibleGainPerUnit)
	}
}

func TestSettlePerformanceFee_HurdleReducesGain(t *testing.T) {
	cfg := PerformanceFeeConfig{Rate: 0.20, HurdleAnnualRate: 0.05, DaysInYear: 365}
	r, err := SettlePerformanceFee(cfg, PerformanceFeeInputs{
		HighWaterMark:     1.00,
		CurrentNAVPerUnit: 1.10,
		UnitsOutstanding:  1000,
		PeriodDays:        365, // full year, hurdle = 1.0 * 0.05 = 0.05
	})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	// raw gain 0.10, post-hurdle = 0.05, fee = 0.05 * 0.20 * 1000 = 10
	if math.Abs(r.FeeAmount-10) > 1e-9 {
		t.Errorf("fee: got %v want 10", r.FeeAmount)
	}
}

func TestSettlePerformanceFee_HurdleEatsAllGain(t *testing.T) {
	cfg := PerformanceFeeConfig{Rate: 0.20, HurdleAnnualRate: 0.10, DaysInYear: 365}
	r, err := SettlePerformanceFee(cfg, PerformanceFeeInputs{
		HighWaterMark:     1.00,
		CurrentNAVPerUnit: 1.05,
		UnitsOutstanding:  1000,
		PeriodDays:        365, // hurdle = 0.10 > gain 0.05
	})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if r.FeeAmount != 0 {
		t.Errorf("fee should be 0 when hurdle eats gain, got %v", r.FeeAmount)
	}
	if r.NewHighWaterMark != 1.00 {
		t.Errorf("HWM should not move, got %v", r.NewHighWaterMark)
	}
}

func TestSettlePerformanceFee_ValidatesInputs(t *testing.T) {
	cfg := PerformanceFeeConfig{Rate: 0.20}
	_, err := SettlePerformanceFee(cfg, PerformanceFeeInputs{HighWaterMark: 0, CurrentNAVPerUnit: 1, UnitsOutstanding: 1})
	if !errors.Is(err, ErrInvalidInputs) {
		t.Fatalf("expected ErrInvalidInputs for HWM=0, got %v", err)
	}
	_, err = SettlePerformanceFee(cfg, PerformanceFeeInputs{HighWaterMark: 1, CurrentNAVPerUnit: 1, UnitsOutstanding: 0})
	if !errors.Is(err, ErrInvalidInputs) {
		t.Fatalf("expected ErrInvalidInputs for units=0, got %v", err)
	}
}
