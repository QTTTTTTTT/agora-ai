package lotsizegate

import (
	"context"
	"strings"
	"testing"
)

// fakeSpecs hands back a fixed spec for every probe.
type fakeSpecs struct {
	spec InstrumentSpec
	err  error
}

func (f fakeSpecs) SpecFor(_ context.Context, _ Probe) (InstrumentSpec, error) {
	return f.spec, f.err
}

// fakePositions returns a fixed holding qty.
type fakePositions struct {
	qty float64
	err error
}

func (f fakePositions) HoldingQty(_ context.Context, _, _ string) (float64, error) {
	return f.qty, f.err
}

// --- Helper -----------------------------------------------------------------

func check(t *testing.T, probe Probe, specs SpecSource, positions PositionSource) Verdict {
	t.Helper()
	return NewEngine(specs, positions).Check(context.Background(), probe)
}

// --- BUY rejects ------------------------------------------------------------

func TestBuy_ChiNext_1Share_RegressionFor_301308(t *testing.T) {
	// 2026-06-02 incident: 301308 buy 1 share (ChiNext minimum 100).
	v := check(t,
		Probe{Symbol: "301308", Market: "a_share", Side: "buy", Quantity: 1, InstrumentKey: "SZSE:301308"},
		&DefaultSpecSource{},
		nil,
	)
	if !v.Rejected {
		t.Fatalf("buy 1 share of 301308 should be rejected, got %+v", v)
	}
	if v.AssetClass != AssetAShare {
		t.Errorf("AssetClass=%q, want %q", v.AssetClass, AssetAShare)
	}
	if !strings.Contains(v.RejectReason, "below") || !strings.Contains(v.RejectReason, "100") {
		t.Errorf("RejectReason=%q, want mention of min 100", v.RejectReason)
	}
}

func TestBuy_STAR_Below200_Rejects(t *testing.T) {
	// STAR (688/689) minimum 200, step 1.
	v := check(t,
		Probe{Symbol: "688195", Market: "a_share", Side: "buy", Quantity: 150, InstrumentKey: "SSE:688195"},
		&DefaultSpecSource{},
		nil,
	)
	if !v.Rejected {
		t.Fatalf("buy 150 of 688195 (STAR) should be rejected, got %+v", v)
	}
}

func TestBuy_STAR_211Shares_Allowed_StepIs1(t *testing.T) {
	// STAR step is 1, so 211 ≥ 200 is fine.
	v := check(t,
		Probe{Symbol: "688195", Market: "a_share", Side: "buy", Quantity: 211, InstrumentKey: "SSE:688195"},
		&DefaultSpecSource{},
		nil,
	)
	if v.Rejected {
		t.Fatalf("buy 211 of 688195 (STAR step=1) should be allowed, got %+v", v)
	}
}

func TestBuy_SHMain_Misaligned150_Rejects_StepIs100(t *testing.T) {
	// SH main (600/601/603/605) step 100.
	v := check(t,
		Probe{Symbol: "600519", Market: "a_share", Side: "buy", Quantity: 150, InstrumentKey: "SSE:600519"},
		&DefaultSpecSource{},
		nil,
	)
	if !v.Rejected {
		t.Fatalf("buy 150 of 600519 (SH main step=100) should be rejected")
	}
	if v.SuggestedQty != 100 {
		t.Errorf("SuggestedQty=%g, want 100", v.SuggestedQty)
	}
}

func TestBuy_US_FractionalRejected_DefaultCap(t *testing.T) {
	v := check(t,
		Probe{Symbol: "AAPL", Market: "us_equity", Side: "buy", Quantity: 0.5, InstrumentKey: "NASDAQ:AAPL"},
		&DefaultSpecSource{},
		nil,
	)
	if !v.Rejected {
		t.Fatalf("US fractional buy should be rejected by default")
	}
}

func TestBuy_US_FractionalAllowed_WhenSpecOverrides(t *testing.T) {
	v := check(t,
		Probe{Symbol: "AAPL", Market: "us_equity", Side: "buy", Quantity: 0.5},
		fakeSpecs{spec: InstrumentSpec{AssetClass: AssetUSEquity, Step: 1, SupportsFractional: true}},
		nil,
	)
	if v.Rejected {
		t.Fatalf("fractional US buy with SupportsFractional=true should be allowed, got %+v", v)
	}
}

func TestBuy_Crypto_BelowStep_Rejects(t *testing.T) {
	v := check(t,
		Probe{Symbol: "BTC-USDT", Market: "crypto", Side: "buy", Quantity: 0.0000001},
		fakeSpecs{spec: InstrumentSpec{AssetClass: AssetCrypto, Step: 1e-5, SupportsFractional: true}},
		nil,
	)
	if !v.Rejected {
		t.Fatalf("BTC qty below step should be rejected")
	}
}

func TestBuy_Futures_FractionalHand_Rejects(t *testing.T) {
	v := check(t,
		Probe{Symbol: "IF2606", Market: "futures-cn", Side: "buy", Quantity: 1.5, AssetClass: "futures"},
		&DefaultSpecSource{},
		nil,
	)
	if !v.Rejected {
		t.Fatalf("futures fractional hand should be rejected")
	}
}

// --- SELL rejects -----------------------------------------------------------

func TestSell_NoPosition_Rejects(t *testing.T) {
	v := check(t,
		Probe{Symbol: "301308", Market: "a_share", Side: "sell", Quantity: 100, InstrumentKey: "SZSE:301308"},
		&DefaultSpecSource{},
		fakePositions{qty: 0},
	)
	if !v.Rejected {
		t.Fatalf("sell with no position should be rejected")
	}
}

func TestSell_ExceedsHolding_Rejects_AndSuggestsHolding(t *testing.T) {
	v := check(t,
		Probe{Symbol: "301308", Market: "a_share", Side: "sell", Quantity: 500, InstrumentKey: "SZSE:301308"},
		&DefaultSpecSource{},
		fakePositions{qty: 100},
	)
	if !v.Rejected {
		t.Fatal("oversell should be rejected")
	}
	if v.SuggestedQty != 100 {
		t.Errorf("SuggestedQty=%g, want 100", v.SuggestedQty)
	}
}

func TestSell_FullPosition_AlwaysAllowed_EvenOddLotResidual(t *testing.T) {
	// 0.6 share full liquidation (the 688195 corp-action residual).
	v := check(t,
		Probe{Symbol: "688195", Market: "a_share", Side: "sell", Quantity: 0.6, InstrumentKey: "SSE:688195"},
		&DefaultSpecSource{},
		fakePositions{qty: 0.6},
	)
	// 0.6 is non-integer → reject. The corp-action residual must be
	// liquidated via the cash-settle path (S12.2 + S12.4), not via
	// a fractional-share sell on A-share.
	if !v.Rejected {
		t.Fatalf("fractional sell on A-share must be rejected; corp-action residuals settle via cash, got %+v", v)
	}
}

func TestSell_FullPosition_IntegerOddLot_Allowed(t *testing.T) {
	// 88 shares on ChiNext (min 100) — held as residual after a
	// previous full-lot sell. Selling the entire 88-share residual
	// is always legal regardless of board minimum.
	v := check(t,
		Probe{Symbol: "300750", Market: "a_share", Side: "sell", Quantity: 88, InstrumentKey: "SZSE:300750"},
		&DefaultSpecSource{},
		fakePositions{qty: 88},
	)
	if v.Rejected {
		t.Fatalf("full-position sell of odd lot should be allowed, got %+v", v)
	}
}

func TestSell_STAR_85Shares_Regression_For_688195(t *testing.T) {
	// 2026-06-01: 688195 partial sell 85 with holding 404.6.
	// STAR step=1 so qty is aligned, but residual 319.6 is OK
	// (≥ 200) — so this should actually be ALLOWED. The 85-share
	// problem in the audit was that it was an *integer* sell out
	// of a *fractional* holding (404.6 → 319.6 residual). The
	// underlying bug is corp-action leaving a fractional holding;
	// this test pins the engine's correct behaviour for the
	// post-fix world where holdings are integers.
	v := check(t,
		Probe{Symbol: "688195", Market: "a_share", Side: "sell", Quantity: 85, InstrumentKey: "SSE:688195"},
		&DefaultSpecSource{},
		fakePositions{qty: 404},
	)
	if v.Rejected {
		t.Fatalf("STAR sell 85 of 404 should be allowed (residual 319 ≥ 200), got %+v", v)
	}
}

func TestSell_STAR_LeavingOddLotResidual_Rejects(t *testing.T) {
	// Holding 250, sell 100 → residual 150 < STAR min 200.
	// Engine must reject and suggest full liquidation.
	v := check(t,
		Probe{Symbol: "688195", Market: "a_share", Side: "sell", Quantity: 100, InstrumentKey: "SSE:688195"},
		&DefaultSpecSource{},
		fakePositions{qty: 250},
	)
	if !v.Rejected {
		t.Fatalf("partial sell leaving odd-lot residual should be rejected")
	}
	if v.SuggestedQty != 250 {
		t.Errorf("SuggestedQty=%g, want full holding 250", v.SuggestedQty)
	}
}

func TestSell_ChiNext_PartialAligned_LeavingFullLot_Allowed(t *testing.T) {
	// 300/301 step=100. Hold 500, sell 200 → residual 300 (OK).
	v := check(t,
		Probe{Symbol: "300750", Market: "a_share", Side: "sell", Quantity: 200, InstrumentKey: "SZSE:300750"},
		&DefaultSpecSource{},
		fakePositions{qty: 500},
	)
	if v.Rejected {
		t.Fatalf("aligned partial sell leaving full lot should be allowed, got %+v", v)
	}
}

func TestSell_ChiNext_MisalignedPartial_Rejects(t *testing.T) {
	// Hold 500, try to sell 150 — misaligned (step=100).
	v := check(t,
		Probe{Symbol: "300750", Market: "a_share", Side: "sell", Quantity: 150, InstrumentKey: "SZSE:300750"},
		&DefaultSpecSource{},
		fakePositions{qty: 500},
	)
	if !v.Rejected {
		t.Fatalf("misaligned partial sell on ChiNext step=100 should be rejected")
	}
}

func TestSell_US_FractionalRejected_ByDefault(t *testing.T) {
	v := check(t,
		Probe{Symbol: "AAPL", Market: "us_equity", Side: "sell", Quantity: 0.5},
		&DefaultSpecSource{},
		fakePositions{qty: 1},
	)
	if !v.Rejected {
		t.Fatalf("US fractional sell should be rejected by default")
	}
}

func TestSell_Crypto_StepAligned_Allowed(t *testing.T) {
	v := check(t,
		Probe{Symbol: "BTC-USDT", Market: "crypto", Side: "sell", Quantity: 0.001},
		fakeSpecs{spec: InstrumentSpec{AssetClass: AssetCrypto, Step: 1e-5, SupportsFractional: true}},
		fakePositions{qty: 0.05},
	)
	if v.Rejected {
		t.Fatalf("crypto step-aligned partial sell should be allowed, got %+v", v)
	}
}

// --- Misc -------------------------------------------------------------------

func TestBuy_UnknownAsset_ShortCircuits(t *testing.T) {
	// 4-letter US-ish symbol but with no market hint and not numeric.
	v := check(t,
		Probe{Symbol: "XYZQ", Side: "buy", Quantity: 0.5},
		&DefaultSpecSource{},
		nil,
	)
	if v.Rejected {
		t.Fatalf("unknown asset class should short-circuit to allow, got %+v", v)
	}
}

func TestZeroQty_Rejects(t *testing.T) {
	v := check(t,
		Probe{Symbol: "301308", Market: "a_share", Side: "buy", Quantity: 0},
		&DefaultSpecSource{},
		nil,
	)
	if !v.Rejected {
		t.Fatalf("zero qty should be rejected")
	}
}

func TestSpecSourceError_AllowsWithWarning(t *testing.T) {
	v := check(t,
		Probe{Symbol: "300750", Market: "a_share", Side: "buy", Quantity: 100},
		fakeSpecs{err: errBoom},
		nil,
	)
	if v.Rejected {
		t.Fatalf("spec-source error should allow with warning, got %+v", v)
	}
	if len(v.Warnings) == 0 {
		t.Errorf("expected a warning, got none")
	}
}

func TestPositionSourceError_AllowsWithWarning(t *testing.T) {
	v := check(t,
		Probe{Symbol: "300750", Market: "a_share", Side: "sell", Quantity: 100},
		&DefaultSpecSource{},
		fakePositions{err: errBoom},
	)
	if v.Rejected {
		t.Fatalf("position-source error should allow with warning, got %+v", v)
	}
}

var errBoom = errBoomError{}

type errBoomError struct{}

func (errBoomError) Error() string { return "boom" }

func TestHKEquity_DefaultLot_100(t *testing.T) {
	// No HK resolver wired → default lot = 100.
	v := check(t,
		Probe{Symbol: "00700", Market: "hk_stock", Side: "buy", Quantity: 50},
		&DefaultSpecSource{},
		nil,
	)
	if !v.Rejected {
		t.Fatalf("HK buy 50 should be rejected (default lot=100)")
	}
}

// --- Step alignment edge cases ---------------------------------------------

func TestAlignedToStep_FloatFuzz(t *testing.T) {
	// 0.0003 / 1e-5 = 30 exact, but float may give 29.9999...
	if !alignedToStep(0.0003, 1e-5) {
		t.Errorf("0.0003 should align to 1e-5 (float fuzz tolerated)")
	}
}

func TestFloorToStep_Basic(t *testing.T) {
	got := floorToStep(0.00037, 1e-5)
	want := 0.00037
	if delta := want - got; delta < -1e-9 || delta > 1e-9 {
		t.Errorf("floorToStep(0.00037, 1e-5) = %g, want ≈ 0.00037", got)
	}
}

// --- S12.5 tick-size checks ------------------------------------------------

func TestTick_AShare_LimitPriceMisaligned_Rejects(t *testing.T) {
	// A-share tick is 0.01 CNY. 500.123 is not a multiple of 0.01.
	v := check(t,
		Probe{Symbol: "600519", Market: "a_share", Side: "buy", Quantity: 100,
			InstrumentKey: "SSE:600519", LimitPrice: 500.123},
		&DefaultSpecSource{},
		nil,
	)
	if !v.Rejected {
		t.Fatalf("misaligned A-share tick should be rejected, got %+v", v)
	}
	if v.RejectReason == "" || !strings.Contains(v.RejectReason, "tick-size") {
		t.Errorf("RejectReason=%q, want mention of tick-size", v.RejectReason)
	}
}

func TestTick_AShare_LimitPriceAligned_Allowed(t *testing.T) {
	v := check(t,
		Probe{Symbol: "600519", Market: "a_share", Side: "buy", Quantity: 100,
			InstrumentKey: "SSE:600519", LimitPrice: 500.12},
		&DefaultSpecSource{},
		nil,
	)
	if v.Rejected {
		t.Fatalf("aligned A-share tick should be allowed, got %+v", v)
	}
}

func TestTick_MarketOrder_NoLimit_NoCheck(t *testing.T) {
	// LimitPrice=0 means market order — tick check must be skipped.
	v := check(t,
		Probe{Symbol: "600519", Market: "a_share", Side: "buy", Quantity: 100, InstrumentKey: "SSE:600519"},
		&DefaultSpecSource{},
		nil,
	)
	if v.Rejected {
		t.Fatalf("market order should not be tick-checked, got %+v", v)
	}
}

func TestTick_HK_BandedRules_PicksRightTick(t *testing.T) {
	// HK banded rules: ≤ 10 → 0.01; > 10 ≤ 20 → 0.02; > 20 ≤ 100 → 0.05.
	rules := []TickRule{
		{MaxPrice: 10, Tick: 0.01},
		{MaxPrice: 20, Tick: 0.02},
		{MaxPrice: 100, Tick: 0.05},
	}
	source := fakeSpecs{spec: InstrumentSpec{
		AssetClass: AssetHKEquity, MinLot: 100, Step: 100,
		TickRules: rules,
	}}
	// 5.005 is misaligned to the ≤10 band (tick=0.01).
	v1 := check(t, Probe{Symbol: "00700", Market: "hk_stock", Side: "buy", Quantity: 100, LimitPrice: 5.005},
		source, nil)
	if !v1.Rejected {
		t.Errorf("HK 5.005 should fail ≤10 band tick=0.01")
	}
	// 15.02 is aligned to the 10<p≤20 band (tick=0.02).
	v2 := check(t, Probe{Symbol: "00700", Market: "hk_stock", Side: "buy", Quantity: 100, LimitPrice: 15.02},
		source, nil)
	if v2.Rejected {
		t.Errorf("HK 15.02 should pass 10<p≤20 band tick=0.02, got %+v", v2)
	}
	// 50.03 is misaligned to the 20<p≤100 band (tick=0.05).
	v3 := check(t, Probe{Symbol: "00700", Market: "hk_stock", Side: "buy", Quantity: 100, LimitPrice: 50.03},
		source, nil)
	if !v3.Rejected {
		t.Errorf("HK 50.03 should fail 20<p≤100 band tick=0.05")
	}
}

func TestTick_NoTickConfigured_AllowsAnyPrice(t *testing.T) {
	source := fakeSpecs{spec: InstrumentSpec{AssetClass: AssetFutures, MinLot: 1, Step: 1}}
	v := check(t,
		Probe{Symbol: "IF2606", Market: "futures-cn", Side: "buy", Quantity: 1, AssetClass: "futures", LimitPrice: 4567.123456},
		source, nil,
	)
	if v.Rejected {
		t.Fatalf("no tick configured → no tick check; got %+v", v)
	}
}

func TestTick_QtyViolation_TakesPrecedenceOverTick(t *testing.T) {
	// 50-share buy on 600519 (SH main, min 100) is a qty violation;
	// tick is also wrong. The qty reject must win — both for clarity
	// in the operator UI and so the wiring layer's SuggestedQty
	// remediation has a single target.
	v := check(t,
		Probe{Symbol: "600519", Market: "a_share", Side: "buy", Quantity: 50,
			InstrumentKey: "SSE:600519", LimitPrice: 500.123},
		&DefaultSpecSource{},
		nil,
	)
	if !v.Rejected {
		t.Fatal("qty 50 on SH main should be rejected")
	}
	if !strings.Contains(v.RejectReason, "below") {
		t.Errorf("RejectReason=%q, want qty 'below' (qty check wins over tick)", v.RejectReason)
	}
}
