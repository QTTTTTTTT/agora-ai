package stress

import (
	"math"
	"testing"
	"time"
)

func mkScenario(name string, shocks ...Shock) Scenario {
	return Scenario{
		ID:       "scen-" + name,
		Name:     name,
		Category: CategoryHistorical,
		Shocks:   shocks,
	}
}

func engineFor(t *testing.T) *Engine {
	t.Helper()
	return &Engine{Now: func() time.Time { return time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) }}
}

func TestEngine_Compute_NoHoldings(t *testing.T) {
	e := engineFor(t)
	r := e.Compute("f1", mkScenario("empty", Shock{TargetType: TargetWildcard, TargetKey: "*", Value: -0.20}), nil, nil)
	if r.NAVBefore != 0 || r.PnLTotal != 0 || r.PnLPct != 0 {
		t.Errorf("expected zero result for empty holdings, got %+v", r)
	}
	if len(r.Impacts) != 0 {
		t.Errorf("impacts should be empty, got %d", len(r.Impacts))
	}
}

func TestEngine_Compute_WildcardShock(t *testing.T) {
	e := engineFor(t)
	holdings := []Holding{
		{InstrumentKey: "US:AAPL", Symbol: "AAPL", MarketValue: 10000},
		{InstrumentKey: "CN:600519", Symbol: "MAOTAI", MarketValue: 5000},
	}
	scen := mkScenario("wild", Shock{TargetType: TargetWildcard, TargetKey: "*", Value: -0.10})
	r := e.Compute("f1", scen, holdings, nil)
	if r.NAVBefore != 15000 {
		t.Errorf("nav_before = %f", r.NAVBefore)
	}
	// Both legs shocked -10%.
	if math.Abs(r.PnLTotal-(-1500)) > 1e-9 {
		t.Errorf("pnl_total = %f, want -1500", r.PnLTotal)
	}
	if math.Abs(r.PnLPct-(-0.10)) > 1e-9 {
		t.Errorf("pnl_pct = %f, want -0.10", r.PnLPct)
	}
	if r.ShockedCount != 2 {
		t.Errorf("shocked_count = %d, want 2", r.ShockedCount)
	}
}

// instrument > market > asset_class > wildcard: more-specific
// match wins.
func TestEngine_Compute_PrioritiesMostSpecific(t *testing.T) {
	e := engineFor(t)
	holdings := []Holding{
		{InstrumentKey: "US:AAPL", Symbol: "AAPL", Market: "US", AssetClass: "equity", MarketValue: 10000},
		{InstrumentKey: "US:MSFT", Symbol: "MSFT", Market: "US", AssetClass: "equity", MarketValue: 10000},
	}
	scen := mkScenario("mixed",
		Shock{TargetType: TargetWildcard, TargetKey: "*", Value: -0.05},
		Shock{TargetType: TargetAssetClass, TargetKey: "equity", Value: -0.10},
		Shock{TargetType: TargetMarket, TargetKey: "US", Value: -0.15},
		Shock{TargetType: TargetInstrument, TargetKey: "US:AAPL", Value: -0.30},
	)
	r := e.Compute("f1", scen, holdings, nil)
	// AAPL took -30%; MSFT took the next-most-specific match, which
	// is the US market shock -15%.
	var aapl, msft HoldingImpact
	for _, im := range r.Impacts {
		if im.InstrumentKey == "US:AAPL" {
			aapl = im
		}
		if im.InstrumentKey == "US:MSFT" {
			msft = im
		}
	}
	if aapl.AppliedReturn != -0.30 || aapl.AppliedShockType != string(TargetInstrument) {
		t.Errorf("AAPL got %+v, want -0.30 instrument", aapl)
	}
	if msft.AppliedReturn != -0.15 || msft.AppliedShockType != string(TargetMarket) {
		t.Errorf("MSFT got %+v, want -0.15 market", msft)
	}
	wantPnL := 10000*-0.30 + 10000*-0.15
	if math.Abs(r.PnLTotal-wantPnL) > 1e-9 {
		t.Errorf("pnl_total = %f, want %f", r.PnLTotal, wantPnL)
	}
}

// Factor shocks compound additively.
func TestEngine_Compute_FactorShocksCompound(t *testing.T) {
	e := engineFor(t)
	holdings := []Holding{
		{InstrumentKey: "US:AAPL", Symbol: "AAPL", MarketValue: 10000},
	}
	loadings := FactorLoadings{
		"US:AAPL": {"momentum": 1.2, "size": -0.4},
	}
	scen := mkScenario("factor",
		Shock{TargetType: TargetFactor, TargetKey: "momentum", Value: -0.10},
		Shock{TargetType: TargetFactor, TargetKey: "size", Value: 0.05},
	)
	r := e.Compute("f1", scen, holdings, loadings)
	// applied = momentum_shock * momentum_loading + size_shock * size_loading
	//        = -0.10 * 1.2 + 0.05 * -0.4
	//        = -0.12 + -0.02
	//        = -0.14
	wantApplied := -0.14
	if math.Abs(r.Impacts[0].AppliedReturn-wantApplied) > 1e-9 {
		t.Errorf("applied_return = %f, want %f", r.Impacts[0].AppliedReturn, wantApplied)
	}
	wantPnL := 10000 * wantApplied
	if math.Abs(r.PnLTotal-wantPnL) > 1e-9 {
		t.Errorf("pnl_total = %f, want %f", r.PnLTotal, wantPnL)
	}
}

// Holdings with no matching shock get pnl=0 but still count in
// HoldingCount and NAVBefore.
func TestEngine_Compute_UnshockedHolding(t *testing.T) {
	e := engineFor(t)
	holdings := []Holding{
		{InstrumentKey: "US:AAPL", Symbol: "AAPL", Market: "US", MarketValue: 10000},
		{InstrumentKey: "CN:600519", Symbol: "MAOTAI", Market: "CN", MarketValue: 5000},
	}
	scen := mkScenario("us-only", Shock{TargetType: TargetMarket, TargetKey: "US", Value: -0.20})
	r := e.Compute("f1", scen, holdings, nil)
	if r.NAVBefore != 15000 || r.HoldingCount != 2 {
		t.Errorf("expected nav_before=15000, holding_count=2; got nav=%f holding_count=%d", r.NAVBefore, r.HoldingCount)
	}
	if r.ShockedCount != 1 {
		t.Errorf("shocked_count = %d, want 1", r.ShockedCount)
	}
	if math.Abs(r.PnLTotal-(-2000)) > 1e-9 {
		t.Errorf("pnl_total = %f, want -2000", r.PnLTotal)
	}
}

// Shorts (negative MV) lose money when shock is positive and gain
// when shock is negative.
func TestEngine_Compute_ShortLegFlipsSign(t *testing.T) {
	e := engineFor(t)
	holdings := []Holding{
		{InstrumentKey: "US:NFLX", Symbol: "NFLX", MarketValue: -10000},
	}
	scen := mkScenario("crash", Shock{TargetType: TargetWildcard, TargetKey: "*", Value: -0.20})
	r := e.Compute("f1", scen, holdings, nil)
	// MV_before = -10000; after = -10000 * (1 + -0.20) = -8000.
	// pnl = -8000 - -10000 = +2000 (short gains when market drops).
	if math.Abs(r.Impacts[0].PnL-2000) > 1e-9 {
		t.Errorf("short-leg pnl = %f, want +2000", r.Impacts[0].PnL)
	}
	// NAVBefore is gross = sum |MV| = 10000.
	if r.NAVBefore != 10000 {
		t.Errorf("nav_before = %f, want 10000", r.NAVBefore)
	}
}

// Shock value beyond ±100% is clamped so notional impact can't
// exceed 100% of the position.
func TestEngine_Compute_ClampsBeyond100Pct(t *testing.T) {
	e := engineFor(t)
	holdings := []Holding{
		{InstrumentKey: "US:GME", Symbol: "GME", MarketValue: 10000},
	}
	scen := mkScenario("meme",
		Shock{TargetType: TargetInstrument, TargetKey: "US:GME", Value: -2.5}, // -250%
	)
	r := e.Compute("f1", scen, holdings, nil)
	if r.Impacts[0].AppliedReturn != -1.0 {
		t.Errorf("clamp failed: applied_return = %f, want -1.0", r.Impacts[0].AppliedReturn)
	}
	if math.Abs(r.PnLTotal-(-10000)) > 1e-9 {
		t.Errorf("pnl_total = %f, want -10000 (full wipe-out, not -25000)", r.PnLTotal)
	}
}

// Result.Impacts is sorted by |PnL| descending so the UI gets the
// biggest contributors first.
func TestEngine_Compute_ImpactsSortedByMagnitude(t *testing.T) {
	e := engineFor(t)
	holdings := []Holding{
		{InstrumentKey: "small", Symbol: "small", MarketValue: 1000, AssetClass: "equity"},
		{InstrumentKey: "huge", Symbol: "huge", MarketValue: 100000, AssetClass: "equity"},
		{InstrumentKey: "medium", Symbol: "medium", MarketValue: 10000, AssetClass: "equity"},
	}
	scen := mkScenario("equity-shock", Shock{TargetType: TargetAssetClass, TargetKey: "equity", Value: -0.10})
	r := e.Compute("f1", scen, holdings, nil)
	if r.Impacts[0].InstrumentKey != "huge" {
		t.Errorf("expected biggest impact first; got %s", r.Impacts[0].InstrumentKey)
	}
	if r.Impacts[len(r.Impacts)-1].InstrumentKey != "small" {
		t.Errorf("expected smallest impact last; got %s", r.Impacts[len(r.Impacts)-1].InstrumentKey)
	}
}

// Asset-class match is case-insensitive (we store "equity" but
// the holding's repo column may be "Equity").
func TestEngine_Compute_AssetClassCaseInsensitive(t *testing.T) {
	e := engineFor(t)
	holdings := []Holding{
		{InstrumentKey: "x", AssetClass: "Equity", MarketValue: 1000},
	}
	scen := mkScenario("ac", Shock{TargetType: TargetAssetClass, TargetKey: "equity", Value: -0.10})
	r := e.Compute("f1", scen, holdings, nil)
	if r.ShockedCount != 1 {
		t.Errorf("case-insensitive AC match failed: shocked_count=%d", r.ShockedCount)
	}
}

func TestShock_Validate(t *testing.T) {
	ok := []Shock{
		{TargetType: TargetInstrument, TargetKey: "x", Value: -0.20},
		{TargetType: TargetWildcard, TargetKey: "", Value: -0.10},
		{TargetType: TargetFactor, TargetKey: "momentum", Value: 1.5},
	}
	for _, s := range ok {
		if err := s.Validate(); err != nil {
			t.Errorf("ok shock failed: %+v: %v", s, err)
		}
	}
	bad := []Shock{
		{TargetType: "bogus", TargetKey: "x", Value: -0.1},
		{TargetType: TargetInstrument, TargetKey: "", Value: -0.1},
		{TargetType: TargetInstrument, TargetKey: "x", Value: math.NaN()},
		{TargetType: TargetInstrument, TargetKey: "x", Value: 11},
	}
	for _, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("bad shock accepted: %+v", s)
		}
	}
}

func TestScenario_Validate(t *testing.T) {
	if err := (Scenario{Name: "", Category: CategoryHistorical}).Validate(); err == nil {
		t.Error("empty name should fail")
	}
	if err := (Scenario{Name: "x", Category: "bogus"}).Validate(); err == nil {
		t.Error("bad category should fail")
	}
	if err := (Scenario{
		Name: "x", Category: CategoryHistorical,
		Shocks: []Shock{{TargetType: "bogus", TargetKey: "k", Value: 0}},
	}).Validate(); err == nil {
		t.Error("bad shock inside should fail")
	}
}
