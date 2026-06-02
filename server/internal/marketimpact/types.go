// Package marketimpact implements the S6.2 size-aware slippage
// model.
//
// What this package owns
//
//   - Domain types: Liquidity, OrderProbe, Estimate, Model.
//   - Pure square-root impact engine that turns
//     (Liquidity, OrderProbe) into a basis-point Estimate.
//   - DB-backed Repo for the per-instrument calibration store.
//   - In-memory Cache so the matching engine can look up a
//     calibration without hitting the DB on every fill.
//
// What this package does NOT own
//
//   - The matching.SlippageModel adapter — that's a thin shim
//     in cmd/server which holds the cache and the engine. We
//     keep it out of this package so internal/marketimpact has
//     no compile-time dependency on internal/matching, and
//     vice versa.
//
// The model: bounded square-root law
//
// The widely-used academic parameterisation for temporary
// market impact is
//
//	adverse_bps = sigma * coef * (Q / V)^alpha * 10000
//
// where sigma is daily volatility, Q is the order size, V is
// the average daily volume (ADV), and (coef, alpha) are
// per-instrument calibration parameters. The square-root law
// (alpha = 0.5) is the most empirically supported default;
// alpha = 1 collapses to the linear model from the older
// matching.LinearImpactSlippage type.
//
// We then clamp the result into [min_slippage_bps,
// max_slippage_bps] for two reasons:
//
//   - For very small orders the formula produces near-zero
//     impact, but in reality the spread half is still paid;
//     the floor pins it above zero.
//   - For Q close to or larger than ADV the formula extrapolates
//     to cartoon numbers; the ceiling is a safety net so a
//     misconfigured ADV (e.g. 0) doesn't cause a 99% fill.
//
// Asset-class defaults
//
// When no calibration row exists, the engine substitutes
// asset-class defaults so the simulator never has zero impact
// for what should be a high-impact trade. The defaults are
// deliberately moderate:
//
//	equity:  sigma=0.02, coef=1.0, alpha=0.5, [1, 200] bps
//	futures: sigma=0.012, coef=0.8, alpha=0.5, [0.5, 100] bps
//	crypto:  sigma=0.04, coef=1.5, alpha=0.5, [2, 500] bps
//	bond:    sigma=0.005, coef=0.5, alpha=0.5, [1, 100] bps
//	otc:     sigma=0.005, coef=0.5, alpha=0.5, [1, 100] bps
//	option:  sigma=0.04, coef=1.5, alpha=0.5, [2, 500] bps
//
// These get returned by AssetClassDefault so the cmd/server
// adapter can mix them with the missing-row fallback.
package marketimpact

import (
	"errors"
	"math"
	"strings"
	"time"
)

// Liquidity is the per-instrument calibration row. ADV-related
// fields are pointers so a partially-calibrated row (e.g. only
// ADV, no volatility) survives without forcing zeroes that
// would silently disable the engine.
type Liquidity struct {
	InstrumentKey      string
	Symbol             string
	Market             string
	AssetClass         string
	ADVShares          *float64
	ADVNotional        *float64
	ADVWindowDays      int
	DailyVolatility    *float64
	ImpactCoefficient  float64
	ImpactExponent     float64
	MinSlippageBps     float64
	MaxSlippageBps     float64
	LastCalibratedAt   *time.Time
	CalibrationSource  string
	Note               string
	UpdatedAt          time.Time
}

// OrderProbe is the engine input for a prospective order.
// Pure: no DB handles, no broker types.
type OrderProbe struct {
	InstrumentKey string
	Symbol        string
	AssetClass    string
	Side          string  // "buy" | "sell"
	Quantity      float64
	ReferencePx   float64 // last/mid; used for notional + as the price the impact bps is anchored to
}

// Estimate is the engine's output. AdverseBps is what callers
// add to the fill price (positive number; the caller applies
// the sign based on side).
type Estimate struct {
	AdverseBps         float64
	TempImpactBps      float64
	PermImpactBps      float64
	UsedDefaults       bool   // true → engine fell back to asset-class defaults (no row)
	UsedADVFallback    bool   // true → ADV missing/<=0; engine returned just MinSlippageBps
	Reason             string // human-readable trace ("equity:default", "ADV missing → floor only", etc)
	AppliedAt          time.Time
	DetectorVersion    string
}

// detectorVersion stamps the estimate so a future model upgrade
// can distinguish records.
const detectorVersion = "v1"

// Model is the interface the cmd/server adapter calls. The
// production implementation is *Engine; tests can plug in a
// stub.
type Model interface {
	Estimate(probe OrderProbe, calib *Liquidity) Estimate
}

// Engine is the production implementation. Stateless; safe to
// share across goroutines.
type Engine struct {
	now func() time.Time
}

// NewEngine returns the engine.
func NewEngine() *Engine {
	return &Engine{now: func() time.Time { return time.Now().UTC() }}
}

// withClock is a test seam.
func (e *Engine) withClock(now func() time.Time) *Engine {
	if now != nil {
		e.now = now
	}
	return e
}

// Estimate evaluates the impact for an order. calib may be nil
// (no calibration row) → engine substitutes asset-class
// defaults.
//
// Behaviour:
//
//   - probe.Quantity <= 0 OR probe.ReferencePx <= 0 → returns
//     zero impact with reason "invalid probe".
//   - calib == nil → uses asset-class defaults; UsedDefaults=true.
//   - calib != nil but ADV missing/<=0 → returns just the row's
//     MinSlippageBps as a flat floor; UsedADVFallback=true.
//   - calib + ADV present → square-root law, clamped to [min,
//     max] bps.
func (e *Engine) Estimate(probe OrderProbe, calib *Liquidity) Estimate {
	now := e.now()
	if probe.Quantity <= 0 || probe.ReferencePx <= 0 {
		return Estimate{
			AppliedAt:       now,
			DetectorVersion: detectorVersion,
			Reason:          "invalid probe",
		}
	}

	// Pick the active parameter set: row's values fall back to
	// asset-class defaults field-by-field. This means a partially
	// calibrated row (operator filled in ADV but left coefs
	// blank) still works.
	usedDefaults := calib == nil
	defaults := AssetClassDefault(strings.ToLower(strings.TrimSpace(probe.AssetClass)))
	var (
		sigma     float64
		coef      float64
		alpha     float64
		minBps    float64
		maxBps    float64
		adv       float64
	)
	if calib == nil {
		sigma = defaults.DailyVolatility
		coef = defaults.ImpactCoefficient
		alpha = defaults.ImpactExponent
		minBps = defaults.MinSlippageBps
		maxBps = defaults.MaxSlippageBps
	} else {
		if calib.DailyVolatility != nil && *calib.DailyVolatility > 0 {
			sigma = *calib.DailyVolatility
		} else {
			sigma = defaults.DailyVolatility
		}
		coef = calib.ImpactCoefficient
		if coef <= 0 {
			coef = defaults.ImpactCoefficient
		}
		alpha = calib.ImpactExponent
		if alpha <= 0 {
			alpha = defaults.ImpactExponent
		}
		minBps = calib.MinSlippageBps
		if minBps < 0 {
			minBps = defaults.MinSlippageBps
		}
		maxBps = calib.MaxSlippageBps
		if maxBps <= 0 {
			maxBps = defaults.MaxSlippageBps
		}
		if calib.ADVShares != nil && *calib.ADVShares > 0 {
			adv = *calib.ADVShares
		}
	}

	// Sanity: clamp params before they enter the formula so a
	// crazy operator value can't generate Inf.
	if minBps > maxBps {
		minBps = maxBps
	}

	// ADV-missing path: the engine returns just minBps as a flat
	// floor (the spread half) so big orders don't silently fill
	// at exactly mid.
	if adv <= 0 {
		return Estimate{
			AdverseBps:      minBps,
			TempImpactBps:   minBps,
			UsedDefaults:    usedDefaults,
			UsedADVFallback: true,
			AppliedAt:       now,
			DetectorVersion: detectorVersion,
			Reason:          fallbackReason(probe.AssetClass, usedDefaults, true),
		}
	}

	// Main square-root formula.
	ratio := probe.Quantity / adv
	if ratio <= 0 {
		return Estimate{
			AdverseBps:      minBps,
			AppliedAt:       now,
			DetectorVersion: detectorVersion,
			Reason:          "ratio <= 0",
		}
	}
	bps := sigma * coef * math.Pow(ratio, alpha) * 10000
	if math.IsNaN(bps) || math.IsInf(bps, 0) {
		bps = maxBps
	}
	if bps < minBps {
		bps = minBps
	}
	if bps > maxBps {
		bps = maxBps
	}
	// Round to 2 decimal places — bps is a basis-point quantity
	// and over-precise output makes test goldens fragile.
	bps = math.Round(bps*100) / 100
	return Estimate{
		AdverseBps:      bps,
		TempImpactBps:   bps,
		UsedDefaults:    usedDefaults,
		AppliedAt:       now,
		DetectorVersion: detectorVersion,
		Reason:          fallbackReason(probe.AssetClass, usedDefaults, false),
	}
}

func fallbackReason(asset string, usedDefaults, advFallback bool) string {
	asset = strings.ToLower(strings.TrimSpace(asset))
	if asset == "" {
		asset = "equity"
	}
	switch {
	case usedDefaults && advFallback:
		return asset + ":default+adv_missing"
	case usedDefaults:
		return asset + ":default"
	case advFallback:
		return asset + ":calibrated+adv_missing"
	default:
		return asset + ":calibrated"
	}
}

// Defaults is one row of the asset-class default table.
type Defaults struct {
	DailyVolatility   float64
	ImpactCoefficient float64
	ImpactExponent    float64
	MinSlippageBps    float64
	MaxSlippageBps    float64
}

// AssetClassDefault returns the moderate defaults sized to land
// near academic findings. Unknown classes collapse to equity.
func AssetClassDefault(class string) Defaults {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case "futures":
		return Defaults{0.012, 0.8, 0.5, 0.5, 100}
	case "crypto":
		return Defaults{0.04, 1.5, 0.5, 2, 500}
	case "option":
		return Defaults{0.04, 1.5, 0.5, 2, 500}
	case "bond":
		return Defaults{0.005, 0.5, 0.5, 1, 100}
	case "otc":
		return Defaults{0.005, 0.5, 0.5, 1, 100}
	case "etf":
		return Defaults{0.015, 0.8, 0.5, 1, 150}
	case "equity", "":
		return Defaults{0.02, 1.0, 0.5, 1, 200}
	default:
		return Defaults{0.02, 1.0, 0.5, 1, 200}
	}
}

// ApplyAdverse adjusts a base price by the adverse-bps amount,
// signed by side ("buy" pays more, "sell" gets less). Convenience
// for the matching adapter.
func ApplyAdverse(basePrice float64, side string, adverseBps float64) float64 {
	if basePrice <= 0 || adverseBps <= 0 {
		return basePrice
	}
	adj := basePrice * adverseBps / 10000
	switch strings.ToLower(strings.TrimSpace(side)) {
	case "buy":
		return basePrice + adj
	case "sell":
		out := basePrice - adj
		if out <= 0 {
			// Pathological case: bps so high we'd cross zero. Pin
			// at a tiny fraction of base so the matcher doesn't
			// see a zero / negative price.
			return basePrice * 0.0001
		}
		return out
	default:
		return basePrice
	}
}

// ----- Errors -----

var (
	ErrCalibrationNotFound = errors.New("marketimpact: calibration row not found")
)
