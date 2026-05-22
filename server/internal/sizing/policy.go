package sizing

import (
	"encoding/json"
	"strings"
)

// ---------------------------------------------------------------------------
// fund.config.riskSizing shape
// ---------------------------------------------------------------------------

// Policy is the per-fund ATR-sizing configuration. Lives at
// fund.config.riskSizing and is OPT-IN — Policy.Enabled defaults
// to false so existing funds keep the legacy 25%-NAV behaviour.
//
// JSON shape:
//
//	{
//	  "riskSizing": {
//	    "enabled":              true,
//	    "perTradeRiskPct":      0.005,   // 0.5% of NAV per buy
//	    "atrLookback":          14,
//	    "atrStopMultiplier":    2.0,     // stop = entry - K * ATR
//	    "maxNotionalPctOfNAV":  0.10     // notional safety clip
//	  }
//	}
//
// All fields back-fill from Defaults via EffectivePolicy when
// the operator leaves them zero, so a minimal `{"enabled":true}`
// is a valid config that gets the textbook defaults.
type Policy struct {
	Enabled             bool    `json:"enabled"`
	PerTradeRiskPct     float64 `json:"perTradeRiskPct,omitempty"`
	ATRLookback         int     `json:"atrLookback,omitempty"`
	ATRStopMultiplier   float64 `json:"atrStopMultiplier,omitempty"`
	MaxNotionalPctOfNAV float64 `json:"maxNotionalPctOfNAV,omitempty"`
}

// Defaults are the textbook values used when the operator leaves
// a field zero. 0.5% per-trade risk on a 14-period ATR with a
// 2×ATR stop is the canonical "Turtle-style" sizing baseline.
const (
	DefaultPerTradeRiskPct     = 0.005
	DefaultATRLookback         = 14
	DefaultATRStopMultiplier   = 2.0
	DefaultMaxNotionalPctOfNAV = 0.10
)

// EffectivePolicy returns a normalised copy of the policy with
// zero / out-of-range values replaced by Defaults. The function
// also CLAMPS the user-supplied values into sane bands so an
// operator typo (perTradeRiskPct: 50 instead of 0.5) can't blow
// up the position book on the first run.
//
// Clamp rules:
//   - perTradeRiskPct      ∈ (0, 0.05]   (5% NAV per trade hard ceiling)
//   - atrLookback          ∈ [2, 200]
//   - atrStopMultiplier    ∈ (0, 10]
//   - maxNotionalPctOfNAV  ∈ (0, 1.0]
//
// Always returns a fresh struct; callers can mutate freely.
func (p Policy) EffectivePolicy() Policy {
	out := Policy{Enabled: p.Enabled}
	if !out.Enabled {
		return out
	}

	out.PerTradeRiskPct = p.PerTradeRiskPct
	if out.PerTradeRiskPct <= 0 {
		out.PerTradeRiskPct = DefaultPerTradeRiskPct
	}
	if out.PerTradeRiskPct > 0.05 {
		out.PerTradeRiskPct = 0.05
	}

	out.ATRLookback = p.ATRLookback
	if out.ATRLookback < 2 {
		out.ATRLookback = DefaultATRLookback
	}
	if out.ATRLookback > 200 {
		out.ATRLookback = 200
	}

	out.ATRStopMultiplier = p.ATRStopMultiplier
	if out.ATRStopMultiplier <= 0 {
		out.ATRStopMultiplier = DefaultATRStopMultiplier
	}
	if out.ATRStopMultiplier > 10 {
		out.ATRStopMultiplier = 10
	}

	out.MaxNotionalPctOfNAV = p.MaxNotionalPctOfNAV
	if out.MaxNotionalPctOfNAV <= 0 {
		out.MaxNotionalPctOfNAV = DefaultMaxNotionalPctOfNAV
	}
	if out.MaxNotionalPctOfNAV > 1.0 {
		out.MaxNotionalPctOfNAV = 1.0
	}

	return out
}

// ---------------------------------------------------------------------------
// Decoding from fund.config raw JSON
// ---------------------------------------------------------------------------

type fundConfigEnvelope struct {
	RiskSizing *Policy `json:"riskSizing,omitempty"`
}

// PolicyFromFundConfig extracts and normalises the sizing policy
// from a fund's persisted config blob. Same idiom as
// strategy.PolicyFromFundConfig:
//
//   - nil / empty / non-JSON   → Policy{Enabled: false}
//   - riskSizing missing       → Policy{Enabled: false}
//   - present                  → decoded + EffectivePolicy()
//
// Decode errors are NOT propagated. A typo in fund.config would
// otherwise block trading; better to fall back to the legacy
// sizing and let the operator notice via dashboard than crash
// the daily review.
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
	if env.RiskSizing == nil {
		return Policy{}
	}
	return env.RiskSizing.EffectivePolicy()
}
