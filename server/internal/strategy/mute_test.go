package strategy

import (
	"context"
	"testing"

	"github.com/fundai/server/internal/ohlc"
	"github.com/fundai/server/internal/regime"
)

// muteFakeSleeve is a deterministic Sleeve that ALWAYS returns a
// buy proposal at confidence 0.9 on the regimes it claims to
// like. Used to drive the mute / unmute paths without spinning
// up the real Trend / MeanReversion sleeves (those have their
// own indicator-driven gating which is orthogonal to mute).
type muteFakeSleeve struct {
	name    string
	regimes []regime.Regime
}

func (s *muteFakeSleeve) Name() string                       { return s.name }
func (s *muteFakeSleeve) PreferredRegimes() []regime.Regime { return s.regimes }
func (s *muteFakeSleeve) Evaluate(_ Bundle) *Proposal {
	return &Proposal{Action: ActionBuy, Confidence: 0.9, Reasoning: "fake"}
}

func newMuteServiceWithFakeSleeves(t *testing.T, sleeves ...Sleeve) *Service {
	t.Helper()
	return &Service{
		policy:  Policy{Enabled: true, MinConfidence: 0.5},
		sleeves: sleeves,
	}
}

func goldenBundle() Bundle {
	bars := make([]ohlc.Bar, 200)
	for i := range bars {
		bars[i] = ohlc.Bar{Open: 100, High: 102, Low: 99, Close: 101, Volume: 1000}
	}
	return Bundle{
		InstrumentKey: "us_equity:NVDA",
		Symbol:        "NVDA",
		Market:        "us_equity",
		AssetClass:    "equity",
		Bars:          bars,
		Regime:        regime.TrendUp,
	}
}

func TestServiceEvaluateMutesByExactSleeveRegimePair(t *testing.T) {
	trend := &muteFakeSleeve{name: "trend", regimes: []regime.Regime{regime.TrendUp, regime.TrendDown}}
	svc := newMuteServiceWithFakeSleeves(t, trend)
	svc = svc.WithMutedSleeveRegimes([]SleeveRegimeMute{
		{Sleeve: "trend", Regime: string(regime.TrendUp)},
	})

	got, err := svc.Evaluate(context.Background(), []Bundle{goldenBundle()})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 actions (trend/trend_up muted), got %d: %+v", len(got), got)
	}
}

func TestServiceEvaluateDoesNotMuteUnrelatedRegimes(t *testing.T) {
	trend := &muteFakeSleeve{name: "trend", regimes: []regime.Regime{regime.TrendUp, regime.TrendDown}}
	svc := newMuteServiceWithFakeSleeves(t, trend)
	// Mute trend in chop only; trend_up is left active.
	svc = svc.WithMutedSleeveRegimes([]SleeveRegimeMute{
		{Sleeve: "trend", Regime: "chop"},
	})

	got, err := svc.Evaluate(context.Background(), []Bundle{goldenBundle()})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 action (trend/trend_up not muted), got %d", len(got))
	}
}

func TestServiceEvaluateMuteIsCaseInsensitive(t *testing.T) {
	trend := &muteFakeSleeve{name: "trend", regimes: []regime.Regime{regime.TrendUp}}
	svc := newMuteServiceWithFakeSleeves(t, trend)
	svc = svc.WithMutedSleeveRegimes([]SleeveRegimeMute{
		{Sleeve: "  TREND  ", Regime: "Trend_Up"},
	})
	got, _ := svc.Evaluate(context.Background(), []Bundle{goldenBundle()})
	if len(got) != 0 {
		t.Fatalf("case/whitespace should not bypass mute, got %d", len(got))
	}
}

func TestServiceMutedSleeveRegimesRoundTrip(t *testing.T) {
	svc := newMuteServiceWithFakeSleeves(t)
	svc = svc.WithMutedSleeveRegimes([]SleeveRegimeMute{
		{Sleeve: "trend", Regime: "chop"},
		{Sleeve: "mean_reversion", Regime: "trend_down"},
	})
	out := svc.MutedSleeveRegimes()
	if len(out) != 2 {
		t.Fatalf("expected 2 muted cells, got %d: %+v", len(out), out)
	}
	// Sorted alphabetically by sleeve then regime.
	want := []SleeveRegimeMute{
		{Sleeve: "mean_reversion", Regime: "trend_down"},
		{Sleeve: "trend", Regime: "chop"},
	}
	for i, w := range want {
		if out[i] != w {
			t.Fatalf("position %d: got %+v, want %+v", i, out[i], w)
		}
	}
}

func TestServiceMutedSleeveRegimesNilClearsMutes(t *testing.T) {
	trend := &muteFakeSleeve{name: "trend", regimes: []regime.Regime{regime.TrendUp}}
	svc := newMuteServiceWithFakeSleeves(t, trend)
	svc.WithMutedSleeveRegimes([]SleeveRegimeMute{{Sleeve: "trend", Regime: "trend_up"}})
	svc.WithMutedSleeveRegimes(nil)
	if got := svc.MutedSleeveRegimes(); got != nil {
		t.Fatalf("nil mute should clear, got %+v", got)
	}
	got, _ := svc.Evaluate(context.Background(), []Bundle{goldenBundle()})
	if len(got) != 1 {
		t.Fatalf("after clear, expected 1 action, got %d", len(got))
	}
}
