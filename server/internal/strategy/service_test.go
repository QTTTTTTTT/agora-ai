package strategy

import (
	"context"
	"testing"

	"github.com/fundai/server/internal/regime"
)

func newConfiguredService(t *testing.T, sleeves ...string) *Service {
	t.Helper()
	p := Policy{
		Enabled:        true,
		EnabledSleeves: sleeves,
		Trend:          ptrTrend(defaultTrend()),
		MeanReversion:  ptrMR(defaultMeanReversion()),
	}.EffectivePolicy()
	svc := NewService(p)
	if svc == nil {
		t.Fatalf("NewService returned nil for policy %+v", p)
	}
	return svc
}

func ptrTrend(p TrendParams) *TrendParams                 { return &p }
func ptrMR(p MeanReversionParams) *MeanReversionParams    { return &p }

// ---------------------------------------------------------------------------
// NewService
// ---------------------------------------------------------------------------

func TestNewServiceReturnsNilWhenDisabled(t *testing.T) {
	for _, p := range []Policy{
		{},
		{Enabled: false, EnabledSleeves: []string{"trend"}},
		{Enabled: true, EnabledSleeves: nil},
	} {
		if svc := NewService(p); svc != nil {
			t.Fatalf("expected nil Service for %+v, got %+v", p, svc)
		}
	}
}

func TestNewServiceIgnoresUnknownSleeveNames(t *testing.T) {
	p := Policy{
		Enabled:        true,
		EnabledSleeves: []string{"trend", "unknown_sleeve"},
	}.EffectivePolicy()
	svc := NewService(p)
	if svc == nil {
		t.Fatal("service nil despite one valid sleeve")
	}
	if got := svc.EnabledSleeves(); len(got) != 1 || got[0] != "trend" {
		t.Fatalf("expected only trend, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Evaluate
// ---------------------------------------------------------------------------

func TestEvaluateRunsTrendOnUptrendBundle(t *testing.T) {
	svc := newConfiguredService(t, "trend")
	bundles := []Bundle{
		{
			Symbol:        "NVDA",
			InstrumentKey: "US:NVDA",
			Market:        "us_equity",
			Bars:          rampUpBars(),
			Regime:        regime.TrendUp,
		},
	}
	actions, err := svc.Evaluate(context.Background(), bundles)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(actions) != 1 || actions[0].Sleeve != "trend" {
		t.Fatalf("expected one trend action, got %+v", actions)
	}
	if actions[0].Proposal.Action != ActionBuy {
		t.Fatalf("expected buy proposal, got %+v", actions[0].Proposal)
	}
}

func TestEvaluateGatesMeanReversionOutOfTrendRegime(t *testing.T) {
	svc := newConfiguredService(t, "mean_reversion")
	// Oversold bars but regime is trend_down — sleeve should be
	// gated off, returning zero actions.
	bundles := []Bundle{
		{
			Symbol: "AAPL",
			Bars:   oversoldRangeBars(),
			Regime: regime.TrendDown,
		},
	}
	actions, err := svc.Evaluate(context.Background(), bundles)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("expected gate to block mean_reversion outside range, got %+v", actions)
	}
}

func TestEvaluateRunsBothSleevesAcrossInstruments(t *testing.T) {
	svc := newConfiguredService(t, "trend", "mean_reversion")
	bundles := []Bundle{
		{
			Symbol:        "NVDA",
			InstrumentKey: "US:NVDA",
			Market:        "us_equity",
			Bars:          rampUpBars(),
			Regime:        regime.TrendUp,
		},
		{
			Symbol:        "AAPL",
			InstrumentKey: "US:AAPL",
			Market:        "us_equity",
			Bars:          oversoldRangeBars(),
			Regime:        regime.Range,
		},
	}
	actions, err := svc.Evaluate(context.Background(), bundles)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("expected 2 actions, got %d: %+v", len(actions), actions)
	}
	// Sorted by instrument_key: AAPL before NVDA.
	if actions[0].InstrumentKey != "US:AAPL" || actions[0].Sleeve != "mean_reversion" {
		t.Fatalf("expected AAPL mean_reversion first, got %+v", actions[0])
	}
	if actions[1].InstrumentKey != "US:NVDA" || actions[1].Sleeve != "trend" {
		t.Fatalf("expected NVDA trend second, got %+v", actions[1])
	}
}

func TestEvaluateRespectsMinConfidence(t *testing.T) {
	p := Policy{
		Enabled:        true,
		EnabledSleeves: []string{"trend"},
		MinConfidence:  0.90, // very high floor
		Trend:          ptrTrend(defaultTrend()),
	}.EffectivePolicy()
	svc := NewService(p)
	bundles := []Bundle{
		{Symbol: "NVDA", Bars: rampUpBars(), Regime: regime.TrendUp},
	}
	actions, err := svc.Evaluate(context.Background(), bundles)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	// With the default rampUpBars the trend confidence sits in the
	// mid-band; minConfidence=0.90 should filter the action out
	// UNLESS the breakout is enormous. Allow either outcome but
	// assert the filter is enforced when it does drop.
	for _, a := range actions {
		if a.Proposal.Confidence < 0.90 {
			t.Fatalf("min_confidence filter leaked: got %v", a.Proposal.Confidence)
		}
	}
}

func TestEvaluateSkipsBundlesWithoutSymbol(t *testing.T) {
	svc := newConfiguredService(t, "trend")
	bundles := []Bundle{
		{Symbol: "", Bars: rampUpBars(), Regime: regime.TrendUp}, // skipped
	}
	actions, err := svc.Evaluate(context.Background(), bundles)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("expected zero actions, got %+v", actions)
	}
}

func TestEvaluateReturnsErrWhenNoSleeves(t *testing.T) {
	svc := &Service{policy: Policy{Enabled: true}}
	if _, err := svc.Evaluate(context.Background(), []Bundle{{Symbol: "X"}}); err == nil {
		t.Fatal("expected ErrNoSleeves, got nil")
	}
}

func TestEvaluateHonoursContextCancellation(t *testing.T) {
	svc := newConfiguredService(t, "trend")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := svc.Evaluate(ctx, []Bundle{
		{Symbol: "NVDA", Bars: rampUpBars(), Regime: regime.TrendUp},
	})
	if err == nil {
		t.Fatal("expected ctx.Err propagation, got nil")
	}
}

// ---------------------------------------------------------------------------
// AllPreferredRegimes
// ---------------------------------------------------------------------------

func TestAllPreferredRegimesUnionsSleeves(t *testing.T) {
	svc := newConfiguredService(t, "trend", "mean_reversion")
	got := svc.AllPreferredRegimes()
	// trend = {TrendUp, TrendDown}; mean_reversion = {Range}
	want := map[regime.Regime]struct{}{
		regime.TrendUp:   {},
		regime.TrendDown: {},
		regime.Range:     {},
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 regimes, got %d: %+v", len(got), got)
	}
	for _, r := range got {
		if _, ok := want[r]; !ok {
			t.Fatalf("unexpected regime %q in union", r)
		}
	}
}
