package strategy

import (
	"encoding/json"
	"testing"
)

func TestPolicyFromFundConfigDecodesNestedShape(t *testing.T) {
	raw := json.RawMessage(`{
		"strategySleeves": {
			"enabled": true,
			"enabledSleeves": ["trend", "mean_reversion"],
			"minConfidence": 0.6,
			"trend":         {"donchianPeriod": 25, "stopLossPct": 0.06},
			"meanReversion": {"rsiPeriod": 9, "bbMultiplier": 2.5}
		},
		"market": "us_equity"
	}`)
	p := PolicyFromFundConfig(raw)
	if !p.Enabled {
		t.Fatalf("expected enabled, got %+v", p)
	}
	if len(p.EnabledSleeves) != 2 ||
		p.EnabledSleeves[0] != "trend" ||
		p.EnabledSleeves[1] != "mean_reversion" {
		t.Fatalf("enabledSleeves: got %+v", p.EnabledSleeves)
	}
	if p.MinConfidence != 0.6 {
		t.Fatalf("minConfidence: got %v, want 0.6", p.MinConfidence)
	}
	if p.Trend == nil || p.Trend.DonchianPeriod != 25 || p.Trend.StopLossPct != 0.06 {
		t.Fatalf("trend params: got %+v", p.Trend)
	}
	// Defaults filled in for fields the user didn't set.
	if p.Trend.FastMA != 50 || p.Trend.SlowMA != 200 {
		t.Fatalf("trend MAs should default: got fast=%d slow=%d", p.Trend.FastMA, p.Trend.SlowMA)
	}
	if p.MeanReversion == nil || p.MeanReversion.RSIPeriod != 9 || p.MeanReversion.BBMultiplier != 2.5 {
		t.Fatalf("mean_reversion params: got %+v", p.MeanReversion)
	}
}

func TestPolicyFromFundConfigDisabledWhenMissing(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"null", "null"},
		{"no key", `{"market":"us_equity"}`},
		{"malformed", `{bad`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := PolicyFromFundConfig(json.RawMessage(tc.raw))
			if p.Enabled {
				t.Fatalf("expected disabled, got %+v", p)
			}
			if p.HasAnySleeve() {
				t.Fatal("HasAnySleeve must be false on disabled policy")
			}
		})
	}
}

func TestEffectivePolicyNormalisesEnabledList(t *testing.T) {
	p := Policy{
		Enabled:        true,
		EnabledSleeves: []string{" Trend ", "MEAN_REVERSION", "trend", "  "},
	}
	eff := p.EffectivePolicy()
	if len(eff.EnabledSleeves) != 2 {
		t.Fatalf("expected 2 unique sleeves, got %+v", eff.EnabledSleeves)
	}
	if eff.EnabledSleeves[0] != "trend" || eff.EnabledSleeves[1] != "mean_reversion" {
		t.Fatalf("normalisation: got %+v", eff.EnabledSleeves)
	}
}

func TestEffectivePolicyClampsConfidence(t *testing.T) {
	p := Policy{Enabled: true, MinConfidence: 5.0}
	if eff := p.EffectivePolicy(); eff.MinConfidence != 0.95 {
		t.Fatalf("expected clamp to 0.95, got %v", eff.MinConfidence)
	}
	p = Policy{Enabled: true, MinConfidence: -1}
	if eff := p.EffectivePolicy(); eff.MinConfidence != 0 {
		t.Fatalf("expected negative to clamp to 0, got %v", eff.MinConfidence)
	}
}

// TestPolicyFromFundConfigDecodesDualMAAndXSMomentum is PR-3A8's
// regression test for the two new sleeves: both decode cleanly,
// merge with their defaults, and the EffectivePolicy normaliser
// honours the operator's overrides while back-filling unspecified
// fields. Pinned because attribution rows depend on the sleeve
// name + params being recorded consistently across releases.
func TestPolicyFromFundConfigDecodesDualMAAndXSMomentum(t *testing.T) {
	raw := json.RawMessage(`{
		"strategySleeves": {
			"enabled": true,
			"enabledSleeves": ["dual_ma", "xs_momentum"],
			"dualMA":      {"fastEMA": 9, "stopLossPct": 0.07},
			"xsMomentum":  {"quintile": 0.10, "skipBars": 5, "minUniverseSize": 8}
		}
	}`)
	p := PolicyFromFundConfig(raw)
	if !p.Enabled {
		t.Fatalf("expected enabled, got %+v", p)
	}
	if len(p.EnabledSleeves) != 2 ||
		p.EnabledSleeves[0] != "dual_ma" ||
		p.EnabledSleeves[1] != "xs_momentum" {
		t.Fatalf("enabledSleeves: got %+v", p.EnabledSleeves)
	}
	if p.DualMA == nil {
		t.Fatal("expected DualMA params decoded")
	}
	if p.DualMA.FastEMA != 9 || p.DualMA.StopLossPct != 0.07 {
		t.Fatalf("dual_ma override missing: got %+v", *p.DualMA)
	}
	// Defaults filled in for fields the user didn't set.
	if p.DualMA.SlowEMA != 26 {
		t.Fatalf("dual_ma SlowEMA default: got %d, want 26", p.DualMA.SlowEMA)
	}
	if p.XSMomentum == nil {
		t.Fatal("expected XSMomentum params decoded")
	}
	if p.XSMomentum.Quintile != 0.10 || p.XSMomentum.SkipBars != 5 || p.XSMomentum.MinUniverseSize != 8 {
		t.Fatalf("xs_momentum overrides missing: got %+v", *p.XSMomentum)
	}
	if p.XSMomentum.LookbackBars != 240 {
		t.Fatalf("xs_momentum LookbackBars default: got %d, want 240", p.XSMomentum.LookbackBars)
	}
}

// TestEffectivePolicyClampsXSMomentumQuintile guards the (0, 0.5]
// range we promise: anything > 0.5 would let the BUY and SELL
// buckets overlap and produce contradictory proposals for the
// middle names.
func TestEffectivePolicyClampsXSMomentumQuintile(t *testing.T) {
	p := Policy{
		Enabled:        true,
		EnabledSleeves: []string{"xs_momentum"},
		XSMomentum:     &CrossSectionalMomentumParams{Quintile: 0.9},
	}.EffectivePolicy()
	if p.XSMomentum == nil {
		t.Fatal("expected XSMomentum to survive normalisation")
	}
	if p.XSMomentum.Quintile != 0.5 {
		t.Fatalf("expected quintile clamp to 0.5, got %v", p.XSMomentum.Quintile)
	}
}

// TestEffectivePolicyClampsXSMomentumSkip ensures SkipBars cannot
// exceed LookbackBars (which would collapse the window to <= 0
// bars and produce no signal).
func TestEffectivePolicyClampsXSMomentumSkip(t *testing.T) {
	p := Policy{
		Enabled:        true,
		EnabledSleeves: []string{"xs_momentum"},
		XSMomentum:     &CrossSectionalMomentumParams{SkipBars: 999},
	}.EffectivePolicy()
	if p.XSMomentum == nil {
		t.Fatal("expected XSMomentum to survive normalisation")
	}
	if p.XSMomentum.SkipBars >= p.XSMomentum.LookbackBars {
		t.Fatalf("expected skip clamped below lookback, got skip=%d lookback=%d", p.XSMomentum.SkipBars, p.XSMomentum.LookbackBars)
	}
}

func TestIsSleeveEnabled(t *testing.T) {
	p := Policy{
		Enabled:        true,
		EnabledSleeves: []string{"trend"},
	}
	if !p.IsSleeveEnabled("trend") {
		t.Fatal("trend should be enabled")
	}
	if !p.IsSleeveEnabled("TREND") {
		t.Fatal("case-insensitive match")
	}
	if p.IsSleeveEnabled("mean_reversion") {
		t.Fatal("mean_reversion should be disabled")
	}
	disabled := Policy{Enabled: false, EnabledSleeves: []string{"trend"}}
	if disabled.IsSleeveEnabled("trend") {
		t.Fatal("disabled policy must report all sleeves disabled")
	}
}
