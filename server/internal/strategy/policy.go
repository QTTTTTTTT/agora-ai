package strategy

import (
	"encoding/json"
	"strings"
)

// ---------------------------------------------------------------------------
// fund.config.strategySleeves shape
// ---------------------------------------------------------------------------

// Policy is the per-fund strategy-sleeve configuration. Lives at
// fund.config.strategySleeves and is OPT-IN — Policy.Enabled
// defaults to false so existing funds see zero behaviour change.
//
// JSON shape:
//
//	{
//	  "strategySleeves": {
//	    "enabled": true,
//	    "enabledSleeves": ["trend", "mean_reversion"],
//	    "minConfidence": 0.55,
//	    "trend": {
//	      "donchianPeriod":  20,
//	      "stopLossPct":     0.05,
//	      "takeProfitPct":   0.20
//	    },
//	    "meanReversion": {
//	      "rsiPeriod":       14,
//	      "bbPeriod":        20,
//	      "bbMultiplier":    2.0,
//	      "rsiOversold":     30.0,
//	      "rsiOverbought":   70.0,
//	      "stopLossPct":     0.04
//	    },
//	    "dualMA": {
//	      "fastEMA":         12,
//	      "slowEMA":         26,
//	      "stopLossPct":     0.05
//	    },
//	    "xsMomentum": {
//	      "lookbackBars":    252,
//	      "skipBars":        21,
//	      "quintile":        0.20,
//	      "minUniverseSize": 5,
//	      "stopLossPct":     0.06
//	    }
//	  }
//	}
type Policy struct {
	Enabled        bool                    `json:"enabled"`
	EnabledSleeves []string                `json:"enabledSleeves,omitempty"`
	// MinConfidence is the lower bound on Proposal.Confidence
	// the Service will accept. Proposals below this floor are
	// silently dropped, even if every other gate passes.
	// 0 means "accept anything > 0".
	MinConfidence float64 `json:"minConfidence,omitempty"`

	Trend         *TrendParams                   `json:"trend,omitempty"`
	MeanReversion *MeanReversionParams           `json:"meanReversion,omitempty"`
	DualMA        *DualMAParams                  `json:"dualMA,omitempty"`
	XSMomentum    *CrossSectionalMomentumParams  `json:"xsMomentum,omitempty"`
}

// TrendParams tunes the trend-following sleeve. Zero values fall
// back to defaults via EffectivePolicy.
type TrendParams struct {
	// DonchianPeriod is the breakout window. 20 is the textbook
	// Turtle default; 55 is the slower long-term variant.
	DonchianPeriod int `json:"donchianPeriod,omitempty"`
	// FastMA / SlowMA are the trend-confirmation MAs. The sleeve
	// only fires LONG when fast > slow and slope is positive,
	// SHORT when fast < slow and slope is negative.
	FastMA int `json:"fastMA,omitempty"`
	SlowMA int `json:"slowMA,omitempty"`
	// StopLossPct / TakeProfitPct hint to the exit manager.
	// 0 leaves both fields unset on the resulting Proposal.
	StopLossPct   float64 `json:"stopLossPct,omitempty"`
	TakeProfitPct float64 `json:"takeProfitPct,omitempty"`
}

// DualMAParams tunes the dual-EMA crossover sleeve. The sleeve
// only fires on the day the cross actually happens, NOT on every
// subsequent bar where fast > slow, so it doesn't spam the trend
// sleeve. See dualma.go for the full signal logic.
type DualMAParams struct {
	// FastEMA / SlowEMA are the two crossover periods. Defaults
	// are the classic 12 / 26 (the MACD constituents); operators
	// can move them to 50/200 for a slower system. The sleeve
	// requires FastEMA < SlowEMA at construction time — calling
	// EffectivePolicy is enough to enforce that.
	FastEMA int `json:"fastEMA,omitempty"`
	SlowEMA int `json:"slowEMA,omitempty"`
	// StopLossPct hint to the exit manager. 0 leaves the field
	// off the resulting Proposal.
	StopLossPct float64 `json:"stopLossPct,omitempty"`
	// TakeProfitPct is optional and defaults to 0 (disabled).
	// Trend-following crossovers traditionally let the next
	// opposite cross flatten the position rather than a fixed
	// profit target; we expose the knob for operators who prefer
	// explicit targets on a per-fund basis.
	TakeProfitPct float64 `json:"takeProfitPct,omitempty"`
}

// CrossSectionalMomentumParams tunes the cross-sectional momentum
// sleeve. UNLIKE the per-instrument sleeves above, this one needs
// the FULL batch of bundles to make a ranking decision; the
// strategy.Service routes it through BatchSleeve.EvaluateBatch.
//
// The classic "12-1 momentum" anomaly: rank instruments by
// (price[t-skip] - price[t-lookback]) / price[t-lookback], buy
// the top quintile, sell the bottom quintile. The "skip" window
// excludes the most recent month to dodge short-term reversal —
// it's the part of the signal that academic studies consistently
// find to be the real edge. PR-3A8 ships sensible defaults but
// the knobs are operator-tunable per fund.
type CrossSectionalMomentumParams struct {
	// LookbackBars is the upper end of the momentum window
	// counted backward from the most recent bar. 252 daily bars
	// is ~12 months for an equity calendar. Crypto / 24h
	// markets should adjust this to whatever 12 months means in
	// their bar cadence.
	LookbackBars int `json:"lookbackBars,omitempty"`
	// SkipBars excludes the most recent N bars from the
	// momentum calculation. The default 21 daily bars (≈1
	// month) implements the well-known "skip the latest month
	// to dodge short-term reversal" trick. Set to 0 to disable.
	SkipBars int `json:"skipBars,omitempty"`
	// Quintile is the fraction of the universe that lands in
	// the BUY / SELL bucket. 0.20 = top 20% buy, bottom 20%
	// sell. Clamped to (0, 0.5] at normalisation time.
	Quintile float64 `json:"quintile,omitempty"`
	// MinUniverseSize is the minimum number of valid bundles
	// required before the sleeve will fire. Below this the
	// "rank" stops being meaningful and the sleeve returns
	// nothing for every bundle. Defaults to 5.
	MinUniverseSize int `json:"minUniverseSize,omitempty"`
	// StopLossPct hint to the exit manager. 0 disables.
	StopLossPct float64 `json:"stopLossPct,omitempty"`
}

// MeanReversionParams tunes the RSI+BB sleeve.
type MeanReversionParams struct {
	// RSIPeriod is Wilder's RSI window. 14 is textbook.
	RSIPeriod int `json:"rsiPeriod,omitempty"`
	// BBPeriod / BBMultiplier are the Bollinger band knobs.
	BBPeriod     int     `json:"bbPeriod,omitempty"`
	BBMultiplier float64 `json:"bbMultiplier,omitempty"`
	// RSIOversold / RSIOverbought are the entry thresholds.
	// Defaults: 30 / 70.
	RSIOversold   float64 `json:"rsiOversold,omitempty"`
	RSIOverbought float64 `json:"rsiOverbought,omitempty"`
	// StopLossPct hint for the exit manager. Mean reversion
	// typically uses a TIGHT stop because the thesis breaks the
	// moment price extends instead of reverting.
	StopLossPct float64 `json:"stopLossPct,omitempty"`
}

// ---------------------------------------------------------------------------
// Defaults
// ---------------------------------------------------------------------------

func defaultTrend() TrendParams {
	return TrendParams{
		DonchianPeriod: 20,
		FastMA:         50,
		SlowMA:         200,
		StopLossPct:    0.05,
		TakeProfitPct:  0.0, // disabled by default — let trailing/exit_policy handle profits
	}
}

func defaultMeanReversion() MeanReversionParams {
	return MeanReversionParams{
		RSIPeriod:     14,
		BBPeriod:      20,
		BBMultiplier:  2.0,
		RSIOversold:   30.0,
		RSIOverbought: 70.0,
		StopLossPct:   0.04,
	}
}

func defaultDualMA() DualMAParams {
	return DualMAParams{
		FastEMA:     12,
		SlowEMA:     26,
		StopLossPct: 0.05,
	}
}

func defaultXSMomentum() CrossSectionalMomentumParams {
	// LookbackBars defaults to 240 (not 252) so the sleeve fits
	// comfortably inside the wiring layer's 250-bar OHLC fetch
	// budget. Academic studies of 12-1 momentum use 12 calendar
	// months, but the 12-bar window has always been a calendar
	// approximation anyway — 240 daily bars ≈ 11.5 months which
	// is close enough for the well-known anomaly to remain
	// detectable. Operators who want a strict 252-bar window
	// can override the default via fund.config.strategySleeves.
	return CrossSectionalMomentumParams{
		LookbackBars:    240,
		SkipBars:        21,
		Quintile:        0.20,
		MinUniverseSize: 5,
		StopLossPct:     0.06,
	}
}

// ---------------------------------------------------------------------------
// EffectivePolicy: normalisation + defaulting
// ---------------------------------------------------------------------------

// EffectivePolicy returns a normalised copy of the policy with:
//
//   - EnabledSleeves entries lowercased + trimmed
//   - duplicate names collapsed
//   - per-sleeve params back-filled with defaults where zero
//   - MinConfidence clamped to [0, 0.95] (0.95 = "almost any
//     real signal would fail" — defensive against fat-finger
//     configs like minConfidence=99)
//
// Always returns a fresh struct; callers can mutate without
// affecting the source.
func (p Policy) EffectivePolicy() Policy {
	out := Policy{Enabled: p.Enabled, MinConfidence: clampConfidence(p.MinConfidence)}
	if !out.Enabled {
		return out
	}
	seen := make(map[string]struct{}, len(p.EnabledSleeves))
	for _, s := range p.EnabledSleeves {
		k := strings.ToLower(strings.TrimSpace(s))
		if k == "" {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out.EnabledSleeves = append(out.EnabledSleeves, k)
	}
	if p.Trend != nil {
		merged := defaultTrend()
		if p.Trend.DonchianPeriod > 0 {
			merged.DonchianPeriod = p.Trend.DonchianPeriod
		}
		if p.Trend.FastMA > 0 {
			merged.FastMA = p.Trend.FastMA
		}
		if p.Trend.SlowMA > 0 {
			merged.SlowMA = p.Trend.SlowMA
		}
		if p.Trend.StopLossPct > 0 {
			merged.StopLossPct = p.Trend.StopLossPct
		}
		if p.Trend.TakeProfitPct > 0 {
			merged.TakeProfitPct = p.Trend.TakeProfitPct
		}
		out.Trend = &merged
	}
	if p.MeanReversion != nil {
		merged := defaultMeanReversion()
		if p.MeanReversion.RSIPeriod > 0 {
			merged.RSIPeriod = p.MeanReversion.RSIPeriod
		}
		if p.MeanReversion.BBPeriod > 0 {
			merged.BBPeriod = p.MeanReversion.BBPeriod
		}
		if p.MeanReversion.BBMultiplier > 0 {
			merged.BBMultiplier = p.MeanReversion.BBMultiplier
		}
		if p.MeanReversion.RSIOversold > 0 {
			merged.RSIOversold = p.MeanReversion.RSIOversold
		}
		if p.MeanReversion.RSIOverbought > 0 {
			merged.RSIOverbought = p.MeanReversion.RSIOverbought
		}
		if p.MeanReversion.StopLossPct > 0 {
			merged.StopLossPct = p.MeanReversion.StopLossPct
		}
		out.MeanReversion = &merged
	}
	if p.DualMA != nil {
		merged := defaultDualMA()
		if p.DualMA.FastEMA > 0 {
			merged.FastEMA = p.DualMA.FastEMA
		}
		if p.DualMA.SlowEMA > 0 {
			merged.SlowEMA = p.DualMA.SlowEMA
		}
		// Operator might (accidentally) configure FastEMA >=
		// SlowEMA. That would invert every signal — silently
		// swap so the sleeve still produces something sane.
		// We do NOT log here because EffectivePolicy is called
		// on every plan run; a config-time validator should
		// catch this on the way in instead.
		if merged.FastEMA >= merged.SlowEMA {
			merged.FastEMA, merged.SlowEMA = merged.SlowEMA, merged.FastEMA
			if merged.FastEMA == merged.SlowEMA {
				// degenerate — fall back to defaults entirely.
				merged = defaultDualMA()
			}
		}
		if p.DualMA.StopLossPct > 0 {
			merged.StopLossPct = p.DualMA.StopLossPct
		}
		if p.DualMA.TakeProfitPct > 0 {
			merged.TakeProfitPct = p.DualMA.TakeProfitPct
		}
		out.DualMA = &merged
	}
	if p.XSMomentum != nil {
		merged := defaultXSMomentum()
		if p.XSMomentum.LookbackBars > 0 {
			merged.LookbackBars = p.XSMomentum.LookbackBars
		}
		if p.XSMomentum.SkipBars >= 0 {
			// 0 is a legitimate value here (caller wants no
			// skip). Use a sentinel of <0 to mean "use default".
			merged.SkipBars = p.XSMomentum.SkipBars
		}
		// SkipBars must be strictly less than the lookback or
		// the window collapses to zero bars. Clamp defensively.
		if merged.SkipBars >= merged.LookbackBars {
			merged.SkipBars = merged.LookbackBars / 12
		}
		if p.XSMomentum.Quintile > 0 {
			merged.Quintile = p.XSMomentum.Quintile
		}
		// (0, 0.5] guard: above 0.5 the BUY and SELL buckets
		// would overlap.
		if merged.Quintile > 0.5 {
			merged.Quintile = 0.5
		}
		if p.XSMomentum.MinUniverseSize > 0 {
			merged.MinUniverseSize = p.XSMomentum.MinUniverseSize
		}
		if p.XSMomentum.StopLossPct > 0 {
			merged.StopLossPct = p.XSMomentum.StopLossPct
		}
		out.XSMomentum = &merged
	}
	return out
}

// IsSleeveEnabled reports whether the named sleeve appears in the
// (normalised) EnabledSleeves list. Returns false on a disabled
// Policy regardless of the list contents.
func (p Policy) IsSleeveEnabled(name string) bool {
	if !p.Enabled {
		return false
	}
	target := strings.ToLower(strings.TrimSpace(name))
	for _, s := range p.EnabledSleeves {
		if s == target {
			return true
		}
	}
	return false
}

// HasAnySleeve reports whether at least one sleeve is enabled.
// The Service short-circuits when this is false, skipping the
// per-instrument OHLC fetch / regime classify cost.
func (p Policy) HasAnySleeve() bool {
	return p.Enabled && len(p.EnabledSleeves) > 0
}

// ---------------------------------------------------------------------------
// Decoding from fund.config raw JSON
// ---------------------------------------------------------------------------

// fundConfigEnvelope mirrors only the strategySleeves slice of
// fund.config we care about here.
type fundConfigEnvelope struct {
	StrategySleeves *Policy `json:"strategySleeves,omitempty"`
}

// PolicyFromFundConfig extracts and normalises the strategy
// policy from a fund's persisted config blob. Behaviour:
//
//   - nil / empty / non-JSON       → Policy{Enabled: false}
//   - strategySleeves missing      → Policy{Enabled: false}
//   - present                      → decoded + EffectivePolicy()
//
// Decode errors are NOT propagated. The classical strategy path
// would rather skip than block trading on a typo. Callers that
// care about config validity should validate at write time, not
// read time.
func PolicyFromFundConfig(raw json.RawMessage) Policy {
	if len(raw) == 0 {
		return Policy{}
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return Policy{}
	}
	var env fundConfigEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return Policy{}
	}
	if env.StrategySleeves == nil {
		return Policy{}
	}
	return env.StrategySleeves.EffectivePolicy()
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func clampConfidence(v float64) float64 {
	if v <= 0 {
		return 0
	}
	if v > 0.95 {
		return 0.95
	}
	return v
}
