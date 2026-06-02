package securitiesborrow

import (
	"math"
	"testing"
	"time"
)

func ptrInt64(v int64) *int64 { return &v }

func TestLocateEngine_NilRate_NoCalibration(t *testing.T) {
	d := NewLocateEngine().Evaluate(LocateProbe{
		FundID: "f1", InstrumentKey: "TSLA.US", RequestedQty: 100, IntendedPrice: 200,
	}, nil)
	if d.Kind != LocateNoCalibration || d.Allowed {
		t.Errorf("got %#v", d)
	}
}

func TestLocateEngine_Unavailable_Reject(t *testing.T) {
	r := &BorrowRate{Availability: AvailabilityUnavailable, BorrowRateBpsAnnual: 5000}
	d := NewLocateEngine().Evaluate(LocateProbe{RequestedQty: 100, IntendedPrice: 50}, r)
	if d.Kind != LocateRejectUnavail || d.Allowed {
		t.Errorf("got %#v", d)
	}
}

func TestLocateEngine_InsufficientShares(t *testing.T) {
	r := &BorrowRate{
		Availability:    AvailabilityHard,
		AvailableShares: ptrInt64(50),
	}
	d := NewLocateEngine().Evaluate(LocateProbe{RequestedQty: 100, IntendedPrice: 50}, r)
	if d.Kind != LocateRejectInsuff {
		t.Errorf("kind = %s", d.Kind)
	}
}

func TestLocateEngine_MinMaxBounds(t *testing.T) {
	r := &BorrowRate{
		Availability: AvailabilityEasy,
		MinLocateQty: ptrInt64(1000),
		MaxLocateQty: ptrInt64(100000),
	}
	below := NewLocateEngine().Evaluate(LocateProbe{RequestedQty: 500, IntendedPrice: 50}, r)
	if below.Kind != LocateRejectBelowMin {
		t.Errorf("expected below-min, got %s", below.Kind)
	}
	above := NewLocateEngine().Evaluate(LocateProbe{RequestedQty: 200000, IntendedPrice: 50}, r)
	if above.Kind != LocateRejectAboveMax {
		t.Errorf("expected above-max, got %s", above.Kind)
	}
}

func TestLocateEngine_ZeroQty_Reject(t *testing.T) {
	r := &BorrowRate{Availability: AvailabilityEasy}
	d := NewLocateEngine().Evaluate(LocateProbe{RequestedQty: 0, IntendedPrice: 50}, r)
	if d.Kind != LocateRejectInsuff {
		t.Errorf("expected reject_insufficient, got %s", d.Kind)
	}
}

func TestLocateEngine_AllowEasy_NoFee(t *testing.T) {
	r := &BorrowRate{Availability: AvailabilityEasy, BorrowRateBpsAnnual: 50}
	d := NewLocateEngine().Evaluate(LocateProbe{RequestedQty: 1000, IntendedPrice: 200}, r)
	if !d.Allowed || d.Kind != LocateAllow {
		t.Fatalf("got %#v", d)
	}
	if d.LocateFeeAmount != 0 {
		t.Errorf("expected 0 fee, got %v", d.LocateFeeAmount)
	}
	if d.Notional != 200000 {
		t.Errorf("notional = %v", d.Notional)
	}
}

func TestLocateEngine_AllowHard_FeeApplied(t *testing.T) {
	r := &BorrowRate{
		Availability:        AvailabilityHard,
		BorrowRateBpsAnnual: 3000,
		LocateFeeBps:        25, // 25 bps of notional
	}
	d := NewLocateEngine().Evaluate(LocateProbe{RequestedQty: 1000, IntendedPrice: 100}, r)
	if !d.Allowed {
		t.Fatalf("got %#v", d)
	}
	want := 100000.0 * 25.0 / 10000.0 // 250
	if math.Abs(d.LocateFeeAmount-want) > 1e-9 {
		t.Errorf("fee = %v, want %v", d.LocateFeeAmount, want)
	}
}

// ----- accrual engine -----

func TestAccrualEngine_NoShort_ZeroFee(t *testing.T) {
	r := NewAccrualEngine().Evaluate(AccrualProbe{ShortQty: 0, MarketPrice: 100, RateBpsAnnual: 50})
	if r.FeeAmount != 0 {
		t.Errorf("expected 0, got %v", r.FeeAmount)
	}
	if r.Reason != "no short position" {
		t.Errorf("reason = %s", r.Reason)
	}
}

func TestAccrualEngine_NoPrice_ZeroFee(t *testing.T) {
	r := NewAccrualEngine().Evaluate(AccrualProbe{ShortQty: 1000, MarketPrice: 0, RateBpsAnnual: 50})
	if r.FeeAmount != 0 {
		t.Errorf("expected 0, got %v", r.FeeAmount)
	}
}

func TestAccrualEngine_ZeroRate_ZeroFee(t *testing.T) {
	r := NewAccrualEngine().Evaluate(AccrualProbe{ShortQty: 1000, MarketPrice: 100, RateBpsAnnual: 0})
	if r.FeeAmount != 0 {
		t.Errorf("expected 0, got %v", r.FeeAmount)
	}
}

func TestAccrualEngine_Computes_365(t *testing.T) {
	// 1000 shares * $100 = $100,000 notional at 5%/yr Act/365.
	// daily fee = 100000 * 500/365/10000 = 100000 * 0.01369863... ≈ 13.6986
	r := NewAccrualEngine().Evaluate(AccrualProbe{
		ShortQty: 1000, MarketPrice: 100, RateBpsAnnual: 500, DayCountBasis: 365,
	})
	want := 100000.0 * 500.0 / 365.0 / 10000.0
	if math.Abs(r.FeeAmount-want) > 1e-6 {
		t.Errorf("fee = %v, want %v", r.FeeAmount, want)
	}
	if r.DayCountBasis != 365 {
		t.Errorf("dcb = %d", r.DayCountBasis)
	}
}

func TestAccrualEngine_Computes_360(t *testing.T) {
	r := NewAccrualEngine().Evaluate(AccrualProbe{
		ShortQty: 1000, MarketPrice: 100, RateBpsAnnual: 500, DayCountBasis: 360,
	})
	want := 100000.0 * 500.0 / 360.0 / 10000.0
	if math.Abs(r.FeeAmount-want) > 1e-6 {
		t.Errorf("fee = %v, want %v", r.FeeAmount, want)
	}
}

func TestAccrualEngine_DefaultsTo365(t *testing.T) {
	r := NewAccrualEngine().Evaluate(AccrualProbe{
		ShortQty: 1000, MarketPrice: 100, RateBpsAnnual: 500, DayCountBasis: 0,
	})
	if r.DayCountBasis != 365 {
		t.Errorf("expected default 365, got %d", r.DayCountBasis)
	}
}

func TestIsValidAvailability(t *testing.T) {
	for _, a := range AllAvailabilities {
		if !IsValidAvailability(string(a)) {
			t.Errorf("expected %s valid", a)
		}
	}
	if IsValidAvailability("nonsense") {
		t.Error("expected nonsense invalid")
	}
}

func TestIsValidCalibrationSource(t *testing.T) {
	for _, s := range AllCalibrationSources {
		if !IsValidCalibrationSource(string(s)) {
			t.Errorf("expected %s valid", s)
		}
	}
	if IsValidCalibrationSource("xyz") {
		t.Error("expected xyz invalid")
	}
}

// Quick sanity for the AccrualResult time-zone neutrality —
// pure functions don't care about the TZ but we exercise the
// path so a future refactor can't accidentally introduce one.
func TestAccrualEngine_DateInProbeIgnored(t *testing.T) {
	tA := AccrualProbe{
		ShortQty: 1000, MarketPrice: 100, RateBpsAnnual: 500,
		AccrualDate: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	tB := tA
	tB.AccrualDate = time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("EST", -5*3600))
	rA := NewAccrualEngine().Evaluate(tA)
	rB := NewAccrualEngine().Evaluate(tB)
	if rA.FeeAmount != rB.FeeAmount {
		t.Errorf("date should not affect fee, got %v vs %v", rA.FeeAmount, rB.FeeAmount)
	}
}
