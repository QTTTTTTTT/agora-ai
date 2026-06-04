// tick_test.go — regressions for the deterministic per-venue
// tick-size rules (instrument.TickSizeFor / IsTickAligned /
// FloorToTick).
//
// Trigger story: 2026-06-04 OCS-fund audit revealed the PM-direct-
// fill path bypassed broker.LotSizeGate, so an A-share order at
// 247.6234 CNY (sub-cent precision; venues round to 0.01) would
// have filled as-is. These tests pin the in-memory tick rules
// that pmPathLotSizeGuard now consults.

package instrument

import "testing"

func TestTickSizeFor_AShareAllBoards(t *testing.T) {
	hint := Hint{Market: "a_share", AssetClass: "equity"}
	// All A-share boards quote in 0.01 CNY increments.
	cases := []string{
		"600519", // SH main
		"601318", // SH main
		"002594", // SZ main / SME
		"300750", // ChiNext
		"301236", // ChiNext
		"688205", // STAR
		"689009", // STAR
		"830799", // BSE
		"920002", // BSE
	}
	for _, sym := range cases {
		if got := TickSizeFor(sym, hint, 0); got != 0.01 {
			t.Errorf("A-share %s: expected tick=0.01, got %v", sym, got)
		}
	}
}

func TestTickSizeFor_USEquityRegNMS(t *testing.T) {
	hint := Hint{Market: "us_stock", AssetClass: "equity"}
	if got := TickSizeFor("AAPL", hint, 0); got != 0.01 {
		t.Errorf("US equity at default price: expected 0.01, got %v", got)
	}
	if got := TickSizeFor("AAPL", hint, 150.25); got != 0.01 {
		t.Errorf("US equity ≥$1: expected 0.01, got %v", got)
	}
	if got := TickSizeFor("PENNY", hint, 0.45); got != 0.0001 {
		t.Errorf("US equity <$1: expected 0.0001, got %v", got)
	}
	if got := TickSizeFor("PENNY", hint, 1.00); got != 0.01 {
		t.Errorf("US equity at exactly $1: expected 0.01, got %v", got)
	}
}

func TestTickSizeFor_NonDeterministicVenuesReturnZero(t *testing.T) {
	// HK banded ticks and crypto step_size require an
	// instrument_metadata lookup; the in-memory function
	// returns 0 so the caller defers to a metadata gate.
	hk := Hint{Market: "hk_stock", AssetClass: "equity"}
	if got := TickSizeFor("00700", hk, 350); got != 0 {
		t.Errorf("HK equity: expected 0 (defer to metadata), got %v", got)
	}
	crypto := Hint{Market: "crypto", AssetClass: "crypto"}
	if got := TickSizeFor("BTC-USDT", crypto, 65000); got != 0 {
		t.Errorf("crypto: expected 0, got %v", got)
	}
	fut := Hint{AssetClass: "futures"}
	if got := TickSizeFor("CL", fut, 80); got != 0 {
		t.Errorf("futures: expected 0, got %v", got)
	}
}

func TestIsTickAligned_AShareAlignedAndMisaligned(t *testing.T) {
	hint := Hint{Market: "a_share", AssetClass: "equity"}
	aligned := []float64{0.01, 1.00, 239.35, 247.60, 10000.00}
	misaligned := []float64{0.001, 247.6234, 239.355, 1.005}
	for _, p := range aligned {
		if !IsTickAligned("688205", hint, p) {
			t.Errorf("A-share STAR price %v should align to 0.01", p)
		}
	}
	for _, p := range misaligned {
		if IsTickAligned("688205", hint, p) {
			t.Errorf("A-share STAR price %v should NOT align to 0.01", p)
		}
	}
}

func TestIsTickAligned_USSubDollarPrecision(t *testing.T) {
	hint := Hint{Market: "us_stock", AssetClass: "equity"}
	// Sub-dollar US equity: 0.0001 tick.
	if !IsTickAligned("PENNY", hint, 0.4567) {
		t.Errorf("US sub-dollar 0.4567 should align to 0.0001")
	}
	if IsTickAligned("PENNY", hint, 0.45671) {
		t.Errorf("US sub-dollar 0.45671 should NOT align to 0.0001")
	}
	// At-or-above-$1 US equity: 0.01 tick.
	if !IsTickAligned("AAPL", hint, 150.25) {
		t.Errorf("US ≥$1 150.25 should align to 0.01")
	}
	if IsTickAligned("AAPL", hint, 150.251) {
		t.Errorf("US ≥$1 150.251 should NOT align to 0.01")
	}
}

func TestIsTickAligned_ZeroPriceShortCircuits(t *testing.T) {
	hint := Hint{Market: "a_share", AssetClass: "equity"}
	// Market orders carry no limit price; the gate must allow.
	if !IsTickAligned("688205", hint, 0) {
		t.Errorf("price=0 (market order) must be treated as aligned")
	}
	if !IsTickAligned("688205", hint, -1) {
		t.Errorf("negative price must be treated as aligned (cancelled-action signalling)")
	}
}

func TestIsTickAligned_UnknownVenueDefersToTrue(t *testing.T) {
	// HK / crypto / futures return 0 from TickSizeFor; the
	// alignment function then returns true so the caller falls
	// back to the broker-side metadata gate without spuriously
	// rejecting.
	hk := Hint{Market: "hk_stock", AssetClass: "equity"}
	if !IsTickAligned("00700", hk, 350.123) {
		t.Errorf("HK unknown-tick should defer to true, got reject")
	}
}

func TestFloorToTick_AShareNudgesDownToCent(t *testing.T) {
	hint := Hint{Market: "a_share", AssetClass: "equity"}
	if got := FloorToTick("688205", hint, 247.6234); !floatEq(got, 247.62) {
		t.Errorf("floor 247.6234 → 247.62, got %v", got)
	}
	if got := FloorToTick("688205", hint, 239.359); !floatEq(got, 239.35) {
		t.Errorf("floor 239.359 → 239.35, got %v", got)
	}
	// Already-aligned input round-trips unchanged.
	if got := FloorToTick("688205", hint, 239.35); !floatEq(got, 239.35) {
		t.Errorf("aligned 239.35 must round-trip, got %v", got)
	}
}

func TestFloorToTick_USSubDollarPrecision(t *testing.T) {
	hint := Hint{Market: "us_stock", AssetClass: "equity"}
	if got := FloorToTick("PENNY", hint, 0.45678); !floatEq(got, 0.4567) {
		t.Errorf("US sub-dollar floor 0.45678 → 0.4567, got %v", got)
	}
	if got := FloorToTick("AAPL", hint, 150.259); !floatEq(got, 150.25) {
		t.Errorf("US ≥$1 floor 150.259 → 150.25, got %v", got)
	}
}

// floatEq compares two float64 values up to a 1e-6 tolerance.
// Local helper so the package-level alignedToStep / floatApproxEq
// stay private (their constants are tuned for share quantities,
// not prices).
func floatEq(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-6
}
