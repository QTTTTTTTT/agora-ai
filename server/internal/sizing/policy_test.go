package sizing

import (
	"encoding/json"
	"testing"
)

func TestEffectivePolicyZeroFieldsFallBackToDefaults(t *testing.T) {
	out := Policy{Enabled: true}.EffectivePolicy()
	if out.PerTradeRiskPct != DefaultPerTradeRiskPct {
		t.Fatalf("perTradeRiskPct fallback wrong: %v", out.PerTradeRiskPct)
	}
	if out.ATRLookback != DefaultATRLookback {
		t.Fatalf("atrLookback fallback wrong: %d", out.ATRLookback)
	}
	if out.ATRStopMultiplier != DefaultATRStopMultiplier {
		t.Fatalf("atrStopMultiplier fallback wrong: %v", out.ATRStopMultiplier)
	}
	if out.MaxNotionalPctOfNAV != DefaultMaxNotionalPctOfNAV {
		t.Fatalf("maxNotionalPctOfNAV fallback wrong: %v", out.MaxNotionalPctOfNAV)
	}
}

func TestEffectivePolicyClampsTooBigRiskPct(t *testing.T) {
	// A typo of 50 instead of 0.5 must NOT translate to 50%
	// of NAV per trade — that would obliterate the fund in
	// two losers. We clamp at the 5% NAV ceiling.
	out := Policy{Enabled: true, PerTradeRiskPct: 50}.EffectivePolicy()
	if out.PerTradeRiskPct != 0.05 {
		t.Fatalf("expected clamp to 0.05, got %v", out.PerTradeRiskPct)
	}
}

func TestEffectivePolicyClampsTooBigStopMultiplier(t *testing.T) {
	out := Policy{Enabled: true, ATRStopMultiplier: 100}.EffectivePolicy()
	if out.ATRStopMultiplier != 10 {
		t.Fatalf("expected clamp to 10, got %v", out.ATRStopMultiplier)
	}
}

func TestEffectivePolicyClampsATRLookback(t *testing.T) {
	out := Policy{Enabled: true, ATRLookback: 9999}.EffectivePolicy()
	if out.ATRLookback != 200 {
		t.Fatalf("expected clamp to 200, got %d", out.ATRLookback)
	}
	out = Policy{Enabled: true, ATRLookback: 1}.EffectivePolicy()
	if out.ATRLookback != DefaultATRLookback {
		t.Fatalf("expected fallback to default for <2, got %d", out.ATRLookback)
	}
}

func TestEffectivePolicyClampsMaxNotionalPct(t *testing.T) {
	out := Policy{Enabled: true, MaxNotionalPctOfNAV: 9.0}.EffectivePolicy()
	if out.MaxNotionalPctOfNAV != 1.0 {
		t.Fatalf("expected clamp to 1.0, got %v", out.MaxNotionalPctOfNAV)
	}
}

func TestEffectivePolicyDisabledReturnsDisabled(t *testing.T) {
	out := Policy{Enabled: false, PerTradeRiskPct: 0.005}.EffectivePolicy()
	if out.Enabled {
		t.Fatal("disabled policy should stay disabled after normalisation")
	}
	// Disabled means no defaults are filled — wiring layer
	// will short-circuit before reading them anyway.
	if out.PerTradeRiskPct != 0 {
		t.Fatalf("disabled policy should not back-fill defaults, got %v", out.PerTradeRiskPct)
	}
}

func TestPolicyFromFundConfigParsesEnabled(t *testing.T) {
	raw := json.RawMessage(`{"riskSizing":{"enabled":true,"perTradeRiskPct":0.01}}`)
	p := PolicyFromFundConfig(raw)
	if !p.Enabled {
		t.Fatal("expected enabled=true")
	}
	if p.PerTradeRiskPct != 0.01 {
		t.Fatalf("expected perTradeRiskPct=0.01, got %v", p.PerTradeRiskPct)
	}
}

func TestPolicyFromFundConfigDisabledByDefault(t *testing.T) {
	cases := []json.RawMessage{
		nil,
		json.RawMessage(``),
		json.RawMessage(`null`),
		json.RawMessage(`{}`),
		json.RawMessage(`{"otherKey":1}`),
		json.RawMessage(`not-json`),
	}
	for i, raw := range cases {
		if PolicyFromFundConfig(raw).Enabled {
			t.Fatalf("case %d should return disabled policy, raw=%q", i, string(raw))
		}
	}
}

func TestPolicyFromFundConfigSurvivesBadlyTypedField(t *testing.T) {
	// A typo'd type ("perTradeRiskPct" as string) should
	// degrade to disabled, NOT propagate an error.
	raw := json.RawMessage(`{"riskSizing":{"enabled":true,"perTradeRiskPct":"oops"}}`)
	p := PolicyFromFundConfig(raw)
	if p.Enabled {
		t.Fatal("malformed field should degrade to disabled")
	}
}
