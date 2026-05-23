// Package exitmanager owns the deterministic exit logic that closes
// positions when a hard risk-hygiene rule fires — independently of,
// and ahead of, whatever the LLM PM is about to propose.
//
// The exit manager is intentionally NOT a strategy. It does not
// look at alpha, sentiment, or sector rotation. It runs four
// classical rules against the open lot ledger:
//
//  1. stop_loss   — close when price drops X% below entry
//  2. take_profit — close when price rises Y% above entry
//  3. trailing    — close when price drops Z% below the
//                   highest price seen during the holding period
//  4. atr_stop    — close when price drops below
//                   anchor − k·ATR (Chandelier-style volatility
//                   trailing stop). Anchor is either the lot
//                   peak (anchorMode="peak", default) or the
//                   entry price (anchorMode="entry").
//  5. time_stop   — close after N calendar days
//
// Each rule is configured per-fund inside fund.config.exitPolicy
// and the service is OPT-IN — exitPolicy.enabled = false (the
// default) preserves the legacy "LLM decides everything" flow so
// the rollout is safe for existing funds. To activate, the
// operator flips `enabled` to true (and optionally tunes the
// per-rule thresholds).
//
// This file defines the configuration types and the
// PolicyFromFundConfig helper that decodes them out of the raw
// fund.config JSON.
package exitmanager

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
)

// ---------------------------------------------------------------------------
// Public configuration types
// ---------------------------------------------------------------------------

// Policy is the per-fund exit-manager configuration. Each *Rule
// pointer is nil when the rule isn't configured for the fund;
// EffectivePolicy() applies defaults + range clamps in one pass
// so the runtime code never sees malformed values.
//
// JSON shape under fund.config.exitPolicy:
//
//	{
//	  "enabled":   true,
//	  "stopLoss":   { "percent": 0.10 },
//	  "takeProfit": { "percent": 0.25 },
//	  "trailing":   { "percent": 0.12 },
//	  "atrStop":    { "multiplier": 3.0, "anchorMode": "peak" },
//	  "timeStop":   { "maxHoldingDays": 30 }
//	}
//
// The atrStop rule is the volatility-aware sibling of `trailing`:
// instead of "give back N% of the peak", it says "give back k
// ATRs of the peak (or entry)". Funds that hold both quiet and
// noisy names benefit because the stop self-scales — quiet
// names get a tight stop, noisy names get a wide one, so a 2%
// retracement in MSFT doesn't get stopped out by the same rule
// that's trying to hold TSLA through an 8% swing.
//
// Backward compatibility: when the column is absent or the
// JSON is empty, PolicyFromFundConfig returns a Policy whose
// Enabled is false — the trade flow stays identical to before
// Phase 3A-2.
type Policy struct {
	Enabled    bool             `json:"enabled"`
	StopLoss   *FixedPercent    `json:"stopLoss,omitempty"`
	TakeProfit *FixedPercent    `json:"takeProfit,omitempty"`
	Trailing   *TrailingPercent `json:"trailing,omitempty"`
	ATRStop    *ATRStopRule     `json:"atrStop,omitempty"`
	TimeStop   *TimeWindow      `json:"timeStop,omitempty"`
}

// FixedPercent expresses a percentage threshold relative to the
// lot's entry price. Percent is a fraction (0.10 = 10%), not a
// percentage (10.0). The rule normaliser maps both forms in case
// an operator types "10" instead of "0.10".
type FixedPercent struct {
	Percent float64 `json:"percent"`
}

// TrailingPercent expresses how much price can drop from the
// highest_price_seen during the lot's lifetime before the
// position is closed. Trailing is only meaningful once the lot
// has *risen* above its entry — until then it would degenerate
// into a wider stop_loss, so the rule no-ops in that case.
type TrailingPercent struct {
	Percent float64 `json:"percent"`
}

// TimeWindow expresses how many calendar days a lot can stay
// open before the position is force-closed. Useful for mean-
// reversion strategies where a position not paying off after
// N days is signal that the thesis broke.
type TimeWindow struct {
	MaxHoldingDays int `json:"maxHoldingDays"`
}

// ATRAnchorMode names where the ATR stop subtracts its volatility
// budget from. The two supported modes match the two classical
// shapes:
//
//   - "peak"  → Chandelier exit: anchor = highest_price_seen since
//     entry. Stop floats UP as the position breaks out and locks
//     in profit as the run extends. Strictly tighter than entry-
//     anchored once the lot has rallied.
//
//   - "entry" → Wilder-style fixed ATR stop: anchor = entry_price.
//     Stop never moves; the rule fires when the position bleeds
//     k·ATR from entry. Useful for swing entries where the manager
//     doesn't want the stop to creep up on noise.
type ATRAnchorMode string

const (
	// ATRAnchorPeak is the Chandelier-style anchor. Default.
	ATRAnchorPeak ATRAnchorMode = "peak"
	// ATRAnchorEntry pins the anchor to the lot's entry price.
	ATRAnchorEntry ATRAnchorMode = "entry"
)

// ATRStopRule expresses a volatility-scaled trailing stop:
//
//	threshold = anchor − Multiplier · ATR
//
// where ATR is the per-symbol 14-bar Wilder ATR sourced by the
// wiring layer (same series the prompt's quantSnapshots block
// uses). The rule no-ops when ATR is missing (legacy positions,
// freshly listed symbols whose history is shorter than ATRPeriod+1)
// so a fund can safely enable it ahead of full OHLC coverage —
// it'll just stay quiet until the data lands.
type ATRStopRule struct {
	// Multiplier is the ATR multiple subtracted from the anchor.
	// Common values: 3.0 (textbook Chandelier), 2.5 (tighter,
	// faster scalps), 4.0 (looser, for high-vol crypto-style
	// names). Clamped into [MinATRMultiplier, MaxATRMultiplier].
	Multiplier float64 `json:"multiplier"`
	// AnchorMode selects "peak" (default) or "entry"; unknown
	// values are coerced to "peak" by EffectivePolicy so the
	// rule degrades gracefully on hand-edited configs.
	AnchorMode ATRAnchorMode `json:"anchorMode,omitempty"`
}

// ---------------------------------------------------------------------------
// Bounds (kept generous on purpose; the goal is to reject obvious
// typos like 1000% stops, not to second-guess sensible operators).
// ---------------------------------------------------------------------------

const (
	// MinPercent is the smallest fractional stop we accept. 0.5%
	// is already tight for equities and pretty much never useful
	// at the fund timeframe we model.
	MinPercent = 0.005
	// MaxPercent is the widest fractional stop we accept. Above
	// 90% a "stop" is effectively a bankruptcy detector — not a
	// risk control. We clip at 0.9 to surface fat-finger configs.
	MaxPercent = 0.9
	// MinHoldingDays is the floor for time_stop. Anything ≤ 0
	// would fire instantly, defeating the purpose.
	MinHoldingDays = 1
	// MaxHoldingDays is the ceiling for time_stop. 365 calendar
	// days catches everything but multi-year value plays; the
	// latter should disable time_stop entirely instead of using
	// a hyper-large window.
	MaxHoldingDays = 365
	// MinATRMultiplier is the lower bound on the ATR stop
	// multiplier. Anything below 0.5 fires on intraday noise.
	MinATRMultiplier = 0.5
	// MaxATRMultiplier is the upper bound; values above 10 are
	// effectively no-stop and almost always a typo.
	MaxATRMultiplier = 10.0
	// DefaultATRMultiplier is the textbook Chandelier multiple
	// (Chuck LeBeau's original Le Long Chandelier exit).
	DefaultATRMultiplier = 3.0
)

// ---------------------------------------------------------------------------
// EffectivePolicy: validation + defaulting in one shot
// ---------------------------------------------------------------------------

// EffectivePolicy returns a normalised copy of the policy with:
//
//   - percent fields clamped into [MinPercent, MaxPercent]
//   - hold-day fields clamped into [MinHoldingDays, MaxHoldingDays]
//   - "raw 10 means 0.10" auto-detection (any percent > 1 is
//     interpreted as a percentage and divided by 100)
//   - rules with invalid / zero thresholds nilled out so the
//     runtime can fall back to "rule disabled"
//
// The returned Policy is safe to read from the rules layer
// without any further validation.
func (p Policy) EffectivePolicy() Policy {
	out := Policy{Enabled: p.Enabled}
	if !out.Enabled {
		return out
	}
	if p.StopLoss != nil {
		if pct := clampPercent(p.StopLoss.Percent); pct > 0 {
			out.StopLoss = &FixedPercent{Percent: pct}
		}
	}
	if p.TakeProfit != nil {
		if pct := clampPercent(p.TakeProfit.Percent); pct > 0 {
			out.TakeProfit = &FixedPercent{Percent: pct}
		}
	}
	if p.Trailing != nil {
		if pct := clampPercent(p.Trailing.Percent); pct > 0 {
			out.Trailing = &TrailingPercent{Percent: pct}
		}
	}
	if p.ATRStop != nil {
		if mult := clampATRMultiplier(p.ATRStop.Multiplier); mult > 0 {
			out.ATRStop = &ATRStopRule{
				Multiplier: mult,
				AnchorMode: normaliseAnchorMode(p.ATRStop.AnchorMode),
			}
		}
	}
	if p.TimeStop != nil {
		if days := clampDays(p.TimeStop.MaxHoldingDays); days > 0 {
			out.TimeStop = &TimeWindow{MaxHoldingDays: days}
		}
	}
	return out
}

// HasAnyRule reports whether the effective policy has at least
// one rule that will actually evaluate. Lets the wiring layer
// skip the whole exit-manager path on funds that are nominally
// "enabled" but configured with no rules.
func (p Policy) HasAnyRule() bool {
	if !p.Enabled {
		return false
	}
	return p.StopLoss != nil || p.TakeProfit != nil || p.Trailing != nil || p.ATRStop != nil || p.TimeStop != nil
}

// ---------------------------------------------------------------------------
// Decoding from raw fund.config
// ---------------------------------------------------------------------------

// fundConfigEnvelope mirrors the relevant slice of fund.config we
// care about here: just the exitPolicy key. We use a typed
// envelope (not map[string]any) so additions to fund.config
// elsewhere don't quietly affect this decoder.
type fundConfigEnvelope struct {
	ExitPolicy *Policy `json:"exitPolicy,omitempty"`
}

// PolicyFromFundConfig extracts and normalises the exit policy
// from a fund's persisted config blob. Behaviour:
//
//   - nil / empty / non-JSON  → Policy{Enabled: false} (no-op)
//   - exitPolicy missing      → Policy{Enabled: false}
//   - exitPolicy present      → decoded + EffectivePolicy() applied
//
// Decode errors are NOT propagated — the exit manager would
// rather silently skip than block trading on a config that
// somehow drifted. Callers that care about config validity
// should validate at write time, not read time.
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
	if env.ExitPolicy == nil {
		return Policy{}
	}
	return env.ExitPolicy.EffectivePolicy()
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// clampPercent interprets a config-supplied percentage threshold.
// Inputs we accept:
//
//   - 0.10  → 10% (the canonical fraction form)
//   - 10    → 10% (auto-converted when > 1.0)
//   - 0.0   → 0   (caller treats as "rule disabled")
//   - <0    → 0   (defensive: never read a negative threshold)
//   - >100  → MaxPercent (clipped)
//
// Returns 0 to mean "no rule"; positive values are guaranteed to
// be inside [MinPercent, MaxPercent].
func clampPercent(v float64) float64 {
	if math.IsNaN(v) || v <= 0 {
		return 0
	}
	if v > 1 {
		// Auto-translate "10" → 0.10. We accept percentages-as-
		// integers because hand-edited JSON often forgets the
		// leading zero, and we'd rather DWIM than reject.
		v = v / 100.0
	}
	if v < MinPercent {
		return 0
	}
	if v > MaxPercent {
		return MaxPercent
	}
	return v
}

// clampATRMultiplier coerces a raw atrStop.multiplier into the
// supported [MinATRMultiplier, MaxATRMultiplier] band.
// Returns 0 to mean "rule disabled" (NaN, ≤0, or sub-min input).
// Anything above the ceiling is clipped rather than disabled —
// we'd rather honour a too-loose stop than silently drop the rule.
func clampATRMultiplier(v float64) float64 {
	if math.IsNaN(v) || v <= 0 {
		return 0
	}
	if v < MinATRMultiplier {
		return 0
	}
	if v > MaxATRMultiplier {
		return MaxATRMultiplier
	}
	return v
}

// normaliseAnchorMode forces an unknown / empty anchorMode to the
// default "peak". We don't error out on hand-edited typos like
// "PEAK" or "Entry" — case-fold + match is friendlier than 500ing
// the next decision cycle.
func normaliseAnchorMode(m ATRAnchorMode) ATRAnchorMode {
	switch ATRAnchorMode(strings.ToLower(strings.TrimSpace(string(m)))) {
	case ATRAnchorEntry:
		return ATRAnchorEntry
	default:
		return ATRAnchorPeak
	}
}

// clampDays clips an integer day count to the legal time_stop
// range. Returns 0 to mean "disabled".
func clampDays(v int) int {
	if v <= 0 {
		return 0
	}
	if v < MinHoldingDays {
		return 0
	}
	if v > MaxHoldingDays {
		return MaxHoldingDays
	}
	return v
}

// ErrInvalidPolicy is the error returned by ValidatePolicy when
// an operator hands us a config that won't pass clamping.
// Currently used only at the (future) write path; the read path
// silently coerces.
var ErrInvalidPolicy = errors.New("exitmanager: invalid policy")

// ValidatePolicy reports whether a config could survive the
// EffectivePolicy clamp without losing rules. Surface area for
// the (future) PATCH /funds endpoint validator.
func ValidatePolicy(p Policy) error {
	if !p.Enabled {
		return nil
	}
	if p.StopLoss != nil && clampPercent(p.StopLoss.Percent) == 0 {
		return ErrInvalidPolicy
	}
	if p.TakeProfit != nil && clampPercent(p.TakeProfit.Percent) == 0 {
		return ErrInvalidPolicy
	}
	if p.Trailing != nil && clampPercent(p.Trailing.Percent) == 0 {
		return ErrInvalidPolicy
	}
	if p.ATRStop != nil && clampATRMultiplier(p.ATRStop.Multiplier) == 0 {
		return ErrInvalidPolicy
	}
	if p.TimeStop != nil && clampDays(p.TimeStop.MaxHoldingDays) == 0 {
		return ErrInvalidPolicy
	}
	return nil
}
