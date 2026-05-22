package marketplace

import (
	"errors"
	"math"
	"testing"
	"time"
)

func dt(year, month, day int) time.Time {
	return time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
}

func TestComputeTrackRecord_TooFewPoints(t *testing.T) {
	if _, err := ComputeTrackRecord(nil); !errors.Is(err, ErrInsufficientNAVHistory) {
		t.Errorf("nil: want ErrInsufficientNAVHistory, got %v", err)
	}
	if _, err := ComputeTrackRecord([]NAVObservation{{Date: dt(2026, 1, 1), NAV: 1}}); !errors.Is(err, ErrInsufficientNAVHistory) {
		t.Errorf("one point: want ErrInsufficientNAVHistory, got %v", err)
	}
	// Both points NAV<=0 → filtered to zero → still error.
	bad := []NAVObservation{
		{Date: dt(2026, 1, 1), NAV: 0},
		{Date: dt(2026, 1, 2), NAV: -1},
	}
	if _, err := ComputeTrackRecord(bad); !errors.Is(err, ErrInsufficientNAVHistory) {
		t.Errorf("filtered bad: want ErrInsufficientNAVHistory, got %v", err)
	}
}

func TestComputeTrackRecord_BasicMonotonicGrowth(t *testing.T) {
	// 11 daily NAVs growing 1% each day. Total return = 1.01^10 - 1 ≈ 10.46%.
	obs := make([]NAVObservation, 11)
	nav := 1.0
	for i := range obs {
		obs[i] = NAVObservation{Date: dt(2026, 1, i+1), NAV: nav}
		nav *= 1.01
	}
	tr, err := ComputeTrackRecord(obs)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if math.Abs(tr.TotalReturn-(math.Pow(1.01, 10)-1)) > 1e-9 {
		t.Errorf("TotalReturn off: %v", tr.TotalReturn)
	}
	if tr.MaxDrawdown != 0 {
		t.Errorf("monotonic growth should have 0 drawdown, got %v", tr.MaxDrawdown)
	}
	if tr.WinRate != 1.0 {
		t.Errorf("monotonic growth WinRate should be 1, got %v", tr.WinRate)
	}
	if tr.LiveDays != 10 {
		t.Errorf("LiveDays should be 10, got %d", tr.LiveDays)
	}
	if tr.DataPoints != 11 {
		t.Errorf("DataPoints should be 11, got %d", tr.DataPoints)
	}
}

func TestComputeTrackRecord_DrawdownDetected(t *testing.T) {
	// Up to 1.5, then crash to 1.05. MaxDD = (1.5-1.05)/1.5 = 0.30.
	obs := []NAVObservation{
		{Date: dt(2026, 1, 1), NAV: 1.00},
		{Date: dt(2026, 1, 2), NAV: 1.20},
		{Date: dt(2026, 1, 3), NAV: 1.50},
		{Date: dt(2026, 1, 4), NAV: 1.05},
		{Date: dt(2026, 1, 5), NAV: 1.10},
	}
	tr, err := ComputeTrackRecord(obs)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if math.Abs(tr.MaxDrawdown-0.30) > 1e-9 {
		t.Errorf("MaxDrawdown: want 0.30, got %v", tr.MaxDrawdown)
	}
}

func TestComputeTrackRecord_UnsortedInputAccepted(t *testing.T) {
	obs := []NAVObservation{
		{Date: dt(2026, 1, 5), NAV: 1.20},
		{Date: dt(2026, 1, 1), NAV: 1.00},
		{Date: dt(2026, 1, 3), NAV: 1.10},
	}
	tr, err := ComputeTrackRecord(obs)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !tr.StartDate.Equal(dt(2026, 1, 1)) {
		t.Errorf("StartDate not earliest: %v", tr.StartDate)
	}
	if !tr.EndDate.Equal(dt(2026, 1, 5)) {
		t.Errorf("EndDate not latest: %v", tr.EndDate)
	}
	if math.Abs(tr.TotalReturn-0.20) > 1e-9 {
		t.Errorf("TotalReturn: want 0.20, got %v", tr.TotalReturn)
	}
}

func TestComputeTrackRecord_FlatNAVZeroSharpe(t *testing.T) {
	obs := []NAVObservation{
		{Date: dt(2026, 1, 1), NAV: 1.00},
		{Date: dt(2026, 1, 2), NAV: 1.00},
		{Date: dt(2026, 1, 3), NAV: 1.00},
	}
	tr, err := ComputeTrackRecord(obs)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if tr.Sharpe != 0 {
		t.Errorf("flat NAV → Sharpe should be 0, got %v", tr.Sharpe)
	}
	if tr.WinRate != 0 {
		t.Errorf("no positive returns → WinRate should be 0, got %v", tr.WinRate)
	}
}

func TestEligibility_NotLiveRefused(t *testing.T) {
	p := EligibilityPolicy{}
	_, err := p.CheckEligibility(EligibilityInputs{Now: dt(2026, 6, 1)})
	if err == nil {
		t.Fatal("expected error for not-live fund")
	}
	var ee *EligibilityError
	if !errors.As(err, &ee) || ee.Reason != "not_live" {
		t.Errorf("want Reason=not_live, got %+v", err)
	}
}

func TestEligibility_AllowSimulationOnly(t *testing.T) {
	p := EligibilityPolicy{AllowSimulationOnly: true}
	days, err := p.CheckEligibility(EligibilityInputs{Now: dt(2026, 6, 1)})
	if err != nil {
		t.Fatalf("AllowSimulationOnly should bypass not_live, got %v", err)
	}
	if days != 0 {
		t.Errorf("expected liveDays=0 for simulation, got %d", days)
	}
}

func TestEligibility_InsufficientDays(t *testing.T) {
	p := EligibilityPolicy{MinForwardTestDays: 30, MinDataPoints: 10}
	_, err := p.CheckEligibility(EligibilityInputs{
		LiveSince: dt(2026, 5, 15),
		Now:       dt(2026, 6, 1),
		NAVPoints: 50,
	})
	var ee *EligibilityError
	if !errors.As(err, &ee) || ee.Reason != "insufficient_days" {
		t.Fatalf("want Reason=insufficient_days, got %+v", err)
	}
	if ee.RequiredDays != 30 || ee.HaveDays != 17 {
		t.Errorf("error numbers off: %+v", ee)
	}
}

func TestEligibility_InsufficientData(t *testing.T) {
	p := EligibilityPolicy{MinForwardTestDays: 30, MinDataPoints: 25}
	_, err := p.CheckEligibility(EligibilityInputs{
		LiveSince: dt(2026, 1, 1),
		Now:       dt(2026, 6, 1),
		NAVPoints: 7,
	})
	var ee *EligibilityError
	if !errors.As(err, &ee) || ee.Reason != "insufficient_data" {
		t.Fatalf("want Reason=insufficient_data, got %+v", err)
	}
	if ee.RequiredData != 25 || ee.HaveData != 7 {
		t.Errorf("error numbers off: %+v", ee)
	}
}

func TestEligibility_PassesAndReportsDays(t *testing.T) {
	p := EligibilityPolicy{MinForwardTestDays: 30, MinDataPoints: 10}
	days, err := p.CheckEligibility(EligibilityInputs{
		LiveSince: dt(2026, 1, 1),
		Now:       dt(2026, 6, 1),
		NAVPoints: 100,
	})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if days < 150 || days > 152 {
		t.Errorf("expected ~151 days, got %d", days)
	}
}

func TestEligibility_DefaultsApply(t *testing.T) {
	p := EligibilityPolicy{} // zero values → use defaults
	_, err := p.CheckEligibility(EligibilityInputs{
		LiveSince: dt(2026, 5, 25),
		Now:       dt(2026, 6, 1),
		NAVPoints: 100,
	})
	var ee *EligibilityError
	if !errors.As(err, &ee) || ee.Reason != "insufficient_days" {
		t.Fatalf("default policy should refuse 7-day fund: %v", err)
	}
	if ee.RequiredDays != DefaultMinForwardTestDays {
		t.Errorf("required days should default to %d, got %d", DefaultMinForwardTestDays, ee.RequiredDays)
	}
}

func TestEligibility_ClockSkew(t *testing.T) {
	p := EligibilityPolicy{}
	_, err := p.CheckEligibility(EligibilityInputs{
		LiveSince: dt(2026, 6, 10),
		Now:       dt(2026, 6, 1), // earlier than live_since
		NAVPoints: 100,
	})
	var ee *EligibilityError
	if !errors.As(err, &ee) || ee.Reason != "insufficient_days" {
		t.Errorf("clock skew should report insufficient_days, got %v", err)
	}
	if ee.HaveDays != 0 {
		t.Errorf("HaveDays should be 0 for skew, got %d", ee.HaveDays)
	}
}
