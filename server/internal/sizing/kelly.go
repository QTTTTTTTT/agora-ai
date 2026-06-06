// kelly.go — W2-11 ATR-Kelly post-processor.
//
// MOTIVATION
// ----------
// sizing.Size + sizing.Policy are the legacy ATR-anchored share-
// count translator: turn a sleeve's "BUY X" verdict into a
// concrete number of shares at a configurable per-trade risk.
// That works at the sleeve level (where the inputs are price,
// ATR, NAV) but does NOT cover the case the W2-11 task aims at:
// the LLM PM stage emits a *target weight* per ticker. There is
// no sleeve to anchor against, no ATR-driven share count — just
// "give name X 4% of NAV".
//
// W2-11 introduces a second, complementary post-processor that
// operates on ALREADY-COMPUTED weights. It deflates an LLM's
// target weight using:
//
//   1. an ATR-scaled volatility budget (cap = RiskPerTrade / ATR), and
//   2. a fractional Kelly damp (kelly = w_llm × confidence × KellyFraction).
//
// Final = sign(w_llm) × min(|w_llm|, vol_budget, |kelly|), then
// clipped to MaxAbsWeight. The post-processor is one-way: it
// can shrink the LLM's ask but never enlarge it.
//
// The naming difference (sizing.Size returns shares, sizing.Apply
// returns a Decision struct) is deliberate. They live in the
// same package because they share the same concepts (ATR,
// risk budget, NAV) but solve adjacent problems.
package sizing

import "math"

// KellyConfig holds the W2-11 risk-budget and Kelly-fraction
// parameters. Independent of the legacy sizing.Policy because
// the two post-processors operate on different inputs (shares
// vs. weights) and live at different stages of the pipeline.
type KellyConfig struct {
	// RiskPerTrade is the target dollar-risk per name as a
	// fraction of fund equity (e.g. 0.005 = 50 bps).
	RiskPerTrade float64
	// MaxRiskMultiplier scales RiskPerTrade for high-conviction
	// names. Default 1.5 — never more than 1.5x the baseline
	// per-name risk.
	MaxRiskMultiplier float64
	// KellyFraction is the fractional-Kelly damp factor.
	// Quarter-Kelly (0.25) is the production default — full
	// Kelly is too volatile for a managed portfolio.
	KellyFraction float64
	// MaxAbsWeight caps the absolute weight on any single name.
	// Defaults to 0.10 (10%).
	MaxAbsWeight float64
	// MinATR is the floor on ATR used for the volatility-budget
	// calculation. Without it, a thinly-traded name with stated
	// ATR=0 would produce an infinite position size.
	MinATR float64
}

// DefaultKellyConfig is the production-safe baseline.
func DefaultKellyConfig() KellyConfig {
	return KellyConfig{
		RiskPerTrade:      0.005,
		MaxRiskMultiplier: 1.5,
		KellyFraction:     0.25,
		MaxAbsWeight:      0.10,
		MinATR:            0.005,
	}
}

// KellyInputs is the per-name post-processor input.
type KellyInputs struct {
	Symbol         string
	NominalWeight  float64 // w_llm
	Confidence     float64 // c
	ATR            float64 // fraction of price
	Equity         float64
	HighConviction bool
}

// KellyDecision is the per-name post-processor output.
type KellyDecision struct {
	Symbol            string  `json:"symbol"`
	NominalWeight     float64 `json:"nominalWeight"`
	FinalWeight       float64 `json:"finalWeight"`
	VolBudgetWeight   float64 `json:"volBudgetWeight"`
	KellyWeight       float64 `json:"kellyWeight"`
	BindingConstraint string  `json:"bindingConstraint"`
	Confidence        float64 `json:"confidence"`
	ATR               float64 `json:"atr"`
}

// ApplyKelly runs the W2-11 post-processor. Pure: same inputs →
// same output, no time / RNG / I/O dependencies.
func ApplyKelly(in KellyInputs, cfg KellyConfig) KellyDecision {
	cfg = normaliseKelly(cfg)
	out := KellyDecision{
		Symbol:        in.Symbol,
		NominalWeight: in.NominalWeight,
		Confidence:    clamp01Kelly(in.Confidence),
		ATR:           in.ATR,
	}
	if in.NominalWeight == 0 || math.IsNaN(in.NominalWeight) {
		out.BindingConstraint = "llm_zero"
		return out
	}

	atr := in.ATR
	if atr < cfg.MinATR {
		atr = cfg.MinATR
	}

	risk := cfg.RiskPerTrade
	if in.HighConviction {
		risk = cfg.RiskPerTrade * cfg.MaxRiskMultiplier
	}
	out.VolBudgetWeight = risk / atr

	out.KellyWeight = math.Abs(in.NominalWeight) * out.Confidence * cfg.KellyFraction

	absLLM := math.Abs(in.NominalWeight)
	candidates := []struct {
		val  float64
		name string
	}{
		{absLLM, "llm_nominal"},
		{out.VolBudgetWeight, "vol_budget"},
		{out.KellyWeight, "kelly_fraction"},
	}
	binding := "llm_nominal"
	min := absLLM
	for _, c := range candidates {
		if c.val < min {
			min = c.val
			binding = c.name
		}
	}
	if min > cfg.MaxAbsWeight {
		min = cfg.MaxAbsWeight
		binding = "max_abs_cap"
	}
	out.FinalWeight = signKelly(in.NominalWeight) * min
	out.BindingConstraint = binding
	return out
}

// ApplyKellyBatch runs ApplyKelly over a slice. Pure / deterministic.
func ApplyKellyBatch(items []KellyInputs, cfg KellyConfig) []KellyDecision {
	out := make([]KellyDecision, len(items))
	for i, it := range items {
		out[i] = ApplyKelly(it, cfg)
	}
	return out
}

func normaliseKelly(cfg KellyConfig) KellyConfig {
	d := DefaultKellyConfig()
	if cfg.RiskPerTrade <= 0 {
		cfg.RiskPerTrade = d.RiskPerTrade
	}
	if cfg.MaxRiskMultiplier <= 0 {
		cfg.MaxRiskMultiplier = d.MaxRiskMultiplier
	}
	if cfg.KellyFraction <= 0 || cfg.KellyFraction > 1 {
		cfg.KellyFraction = d.KellyFraction
	}
	if cfg.MaxAbsWeight <= 0 || cfg.MaxAbsWeight > 1 {
		cfg.MaxAbsWeight = d.MaxAbsWeight
	}
	if cfg.MinATR <= 0 {
		cfg.MinATR = d.MinATR
	}
	return cfg
}

func clamp01Kelly(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func signKelly(v float64) float64 {
	if v > 0 {
		return 1
	}
	if v < 0 {
		return -1
	}
	return 0
}
