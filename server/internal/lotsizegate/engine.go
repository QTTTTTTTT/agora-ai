package lotsizegate

import (
	"context"
	"fmt"
	"math"
	"strings"
)

// Engine is the deterministic lot-size compliance engine. It is
// safe for concurrent use (no internal state).
type Engine struct {
	specs     SpecSource
	positions PositionSource
}

// NewEngine constructs an Engine. Either source may be nil — a nil
// SpecSource short-circuits Check to "allow" (the engine must have
// a spec to render a verdict); a nil PositionSource means partial
// sells will use a pessimistic 0 holding (any sell is rejected as
// "no position", which is the safe default).
func NewEngine(specs SpecSource, positions PositionSource) *Engine {
	return &Engine{specs: specs, positions: positions}
}

// Check is the pure verdict function. Returns a Verdict for the
// probe. The engine is conservative on errors (allow with a
// warning so a transient spec-source outage doesn't halt trading
// — the upstream PM-side NormalizeBuyQty / NormalizeSellQty
// already do the heavy lifting and the gate is the final safety
// net).
func (e *Engine) Check(ctx context.Context, probe Probe) Verdict {
	if e == nil || e.specs == nil {
		return Verdict{AssetClass: AssetUnknown}
	}
	asset := classifyAsset(probe)
	if asset == AssetUnknown {
		return Verdict{AssetClass: asset}
	}
	if probe.Quantity <= 0 {
		return Verdict{
			Rejected:     true,
			RejectReason: "lot-size: non-positive quantity",
			AssetClass:   asset,
		}
	}

	spec, err := e.specs.SpecFor(ctx, probe)
	if err != nil {
		return Verdict{
			Warnings:   []string{fmt.Sprintf("lot-size: spec lookup failed (%v) — allowing", err)},
			AssetClass: asset,
		}
	}
	if spec.AssetClass == AssetUnknown {
		spec.AssetClass = asset
	}

	side := strings.ToLower(strings.TrimSpace(probe.Side))
	var verdict Verdict
	switch side {
	case "buy":
		verdict = e.checkBuy(asset, spec, probe)
	case "sell":
		verdict = e.checkSell(ctx, asset, spec, probe)
	default:
		return Verdict{
			Rejected:     true,
			RejectReason: fmt.Sprintf("lot-size: unrecognised side %q", probe.Side),
			AssetClass:   asset,
		}
	}

	// S12.5 — tick check piggybacks on the lot-size gate. It only
	// fires when the qty check has already passed (no point in
	// flagging tick if we're already rejecting the order on lot)
	// AND when the order carries a limit price (market orders have
	// no price to align). Tick violations override an otherwise
	// allow verdict; if both rules disagree the qty check wins.
	if !verdict.Rejected && probe.LimitPrice > 0 {
		if tv := checkTick(spec, probe); tv.Rejected {
			tv.AssetClass = asset
			return tv
		}
	}

	return verdict
}

// checkBuy applies the buy-side rules. Integer-share venues reject
// fractional quantities outright; fractional-capable venues only
// check MinLot and step.
func (e *Engine) checkBuy(asset AssetClass, spec InstrumentSpec, probe Probe) Verdict {
	if !spec.SupportsFractional && probe.Quantity != math.Floor(probe.Quantity) {
		suggested := math.Floor(probe.Quantity)
		if spec.MinLot > 0 && suggested < spec.MinLot {
			suggested = 0
		} else if spec.Step > 1 {
			suggested = math.Floor(suggested/spec.Step) * spec.Step
		}
		return Verdict{
			Rejected: true,
			RejectReason: fmt.Sprintf("lot-size: %s qty=%g must be an integer (asset=%s, fractional not supported)",
				probe.Symbol, probe.Quantity, asset),
			AssetClass:   asset,
			SuggestedQty: suggested,
		}
	}

	if spec.MinLot > 0 && probe.Quantity < spec.MinLot {
		return Verdict{
			Rejected: true,
			RejectReason: fmt.Sprintf("lot-size: %s buy qty=%g below %s minimum %g",
				probe.Symbol, probe.Quantity, spec.AssetClass, spec.MinLot),
			AssetClass:   asset,
			SuggestedQty: 0, // sub-minimum buy can't be saved
		}
	}

	// Step check applies to integer-step venues (A-share boards
	// with step=100, etc.) and to crypto step_size. Fractional
	// venues without a meaningful step (US fractional, default
	// crypto when step=1e-6) pass through.
	if needsStepCheck(spec) && !alignedToStep(probe.Quantity, spec.Step) {
		suggested := floorToStep(probe.Quantity, spec.Step)
		if suggested < spec.MinLot {
			suggested = spec.MinLot
		}
		return Verdict{
			Rejected: true,
			RejectReason: fmt.Sprintf("lot-size: %s buy qty=%g not aligned to %s step %g",
				probe.Symbol, probe.Quantity, spec.AssetClass, spec.Step),
			AssetClass:   asset,
			SuggestedQty: suggested,
		}
	}

	// Notional floor (crypto-style "min order value"). We don't
	// know the price here, but if the spec carries a min-notional
	// the wiring layer should have multiplied through. The gate
	// stays silent if MinNotional is 0.

	return Verdict{AssetClass: asset}
}

// checkSell applies the sell-side rules. The A-share odd-lot
// residual rule (residual < MinLot → expand to liquidate whole
// position) is the trickiest part and runs first.
func (e *Engine) checkSell(ctx context.Context, asset AssetClass, spec InstrumentSpec, probe Probe) Verdict {
	if !spec.SupportsFractional && probe.Quantity != math.Floor(probe.Quantity) {
		return Verdict{
			Rejected: true,
			RejectReason: fmt.Sprintf("lot-size: %s sell qty=%g must be an integer (asset=%s, fractional not supported)",
				probe.Symbol, probe.Quantity, asset),
			AssetClass:   asset,
			SuggestedQty: math.Floor(probe.Quantity),
		}
	}

	heldQty := 0.0
	if e.positions != nil {
		q, err := e.positions.HoldingQty(ctx, probe.FundID, probe.InstrumentKey)
		if err != nil {
			return Verdict{
				Warnings:   []string{fmt.Sprintf("lot-size: position lookup failed (%v) — allowing", err)},
				AssetClass: asset,
			}
		}
		heldQty = q
	}

	if heldQty <= 0 {
		return Verdict{
			Rejected: true,
			RejectReason: fmt.Sprintf("lot-size: %s sell rejected — no recorded position",
				probe.Symbol),
			AssetClass: asset,
		}
	}

	if probe.Quantity > heldQty {
		return Verdict{
			Rejected: true,
			RejectReason: fmt.Sprintf("lot-size: %s sell qty=%g exceeds holding %g",
				probe.Symbol, probe.Quantity, heldQty),
			AssetClass:   asset,
			SuggestedQty: heldQty,
		}
	}

	// Full-position sells are always legal (regardless of board
	// minimums — handles odd-lot residuals from corp actions).
	if floatApproxEq(probe.Quantity, heldQty) {
		return Verdict{AssetClass: asset}
	}

	// Asset classes without a board minimum (US equity integer,
	// futures hands, crypto) only need step alignment.
	if spec.MinLot == 0 {
		if needsStepCheck(spec) && !alignedToStep(probe.Quantity, spec.Step) {
			return Verdict{
				Rejected: true,
				RejectReason: fmt.Sprintf("lot-size: %s sell qty=%g not aligned to %s step %g",
					probe.Symbol, probe.Quantity, spec.AssetClass, spec.Step),
				AssetClass:   asset,
				SuggestedQty: floorToStep(probe.Quantity, spec.Step),
			}
		}
		return Verdict{AssetClass: asset}
	}

	// A-share boards (and any future board with a MinLot):
	//
	//   * A partial sell BELOW MinLot is legal as long as the
	//     residual after the sell is either 0 (full liquidation,
	//     handled above) or ≥ MinLot. The "卖出余额不足 MinLot 必须
	//     一次性卖出" rule fires only on the residual side.
	//   * The sell qty itself must be step-aligned (SH/SZ/ChiNext
	//     step=100; STAR/BSE step=1 so anything integer is OK).
	if needsStepCheck(spec) && !alignedToStep(probe.Quantity, spec.Step) {
		return Verdict{
			Rejected: true,
			RejectReason: fmt.Sprintf("lot-size: %s sell qty=%g not aligned to %s step %g",
				probe.Symbol, probe.Quantity, spec.AssetClass, spec.Step),
			AssetClass:   asset,
			SuggestedQty: floorToStep(probe.Quantity, spec.Step),
		}
	}

	residual := heldQty - probe.Quantity
	if residual > 0 && residual < spec.MinLot {
		return Verdict{
			Rejected: true,
			RejectReason: fmt.Sprintf("lot-size: %s partial sell qty=%g would leave odd-lot residual %g (< %s minimum %g); must liquidate full holding %g",
				probe.Symbol, probe.Quantity, residual, spec.AssetClass, spec.MinLot, heldQty),
			AssetClass:   asset,
			SuggestedQty: heldQty,
		}
	}

	return Verdict{AssetClass: asset}
}

// needsStepCheck reports whether the spec has a meaningful step that
// the engine should enforce. step <= 0 means "no constraint" (the
// asset class doesn't model a step, e.g. unknown). step exactly 1
// only matters when fractional is NOT supported (the fractional
// check has already run; if we're here with integer-only and step=1
// the alignedToStep check is what reads "must be integer"). For
// fractional venues with step=1 we suppress the check entirely so
// the alignedToStep "must be whole number" test doesn't fire on
// fractional quantities that have already passed the cap test.
func needsStepCheck(spec InstrumentSpec) bool {
	if spec.Step <= 0 {
		return false
	}
	if spec.SupportsFractional && spec.Step == 1 {
		return false
	}
	return true
}

// alignedToStep reports whether qty is an exact multiple of step.
//   - step == 1 → qty must be a whole number.
//   - step > 1 → use math.Mod with fuzz tolerance.
//   - step < 1 (crypto step_size, e.g. 1e-5) → scale up to avoid
//     float fuzz in modular arithmetic. We round to the nearest
//     integer in scaled space and require the round-trip to recover
//     a value within 1e-7 of the input.
func alignedToStep(qty, step float64) bool {
	if step <= 0 {
		return true
	}
	if step == 1 {
		return qty == math.Floor(qty)
	}
	if step > 1 {
		rem := math.Mod(qty, step)
		return rem < 1e-9 || (step-rem) < 1e-9
	}
	// step < 1: scale and compare.
	scale := 1.0 / step
	scaled := qty * scale
	rounded := math.Round(scaled)
	return math.Abs(scaled-rounded) < 1e-6
}

// floorToStep returns the largest multiple of step ≤ qty.
//   - step <= 0 → return qty unchanged (no constraint).
//   - step == 1 → math.Floor(qty).
//   - step > 1 → multiply-divide.
//   - step < 1 → scale up to avoid float fuzz.
func floorToStep(qty, step float64) float64 {
	if step <= 0 {
		return qty
	}
	if step == 1 {
		return math.Floor(qty)
	}
	if step > 1 {
		return math.Floor(qty/step) * step
	}
	scale := 1.0 / step
	// Add a tiny epsilon before the floor so values that should
	// be exact multiples (e.g. 0.00037 / 1e-5 = 37) don't round
	// DOWN to 36 because of float fuzz on the multiply. The
	// epsilon must be small enough not to lift a sub-step
	// quantity into the next bucket; 1e-9 is safe relative to
	// the smallest steps we model (~1e-8).
	return math.Floor(qty*scale+1e-9) / scale
}

func floatApproxEq(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

// checkTick verifies that the limit price aligns with the
// instrument's tick rule. Returns Rejected with a SuggestedQty=0
// (no quantity adjustment can fix a bad price) and a
// human-readable reason naming the expected tick. Operators
// usually want to nudge the price up to the next legal tick;
// the wiring layer can do that when SuggestedQty == 0 and the
// RejectReason mentions a tick.
func checkTick(spec InstrumentSpec, probe Probe) Verdict {
	tick := tickFor(spec, probe.LimitPrice)
	if tick <= 0 {
		return Verdict{}
	}
	if alignedToStep(probe.LimitPrice, tick) {
		return Verdict{}
	}
	floor := floorToStep(probe.LimitPrice, tick)
	return Verdict{
		Rejected: true,
		RejectReason: fmt.Sprintf("tick-size: %s limit price %g not aligned to %s tick %g (suggested floor=%g, next=%g)",
			probe.Symbol, probe.LimitPrice, spec.AssetClass, tick, floor, floor+tick),
	}
}

// tickFor picks the right tick for the limit price. Banded rules
// win; the smallest MaxPrice ≥ limitPrice provides the tick.
// Falls back to the scalar TickSize when no band matches or
// TickRules is empty.
func tickFor(spec InstrumentSpec, limit float64) float64 {
	if len(spec.TickRules) > 0 {
		// We assume rules are sorted ascending by MaxPrice. The
		// wiring layer (admin / migration seed) is responsible
		// for that order; on the off-chance the rows arrived
		// unsorted we still pick the first matching band.
		var bestTick float64
		var bestMax float64
		for _, r := range spec.TickRules {
			if r.Tick <= 0 || r.MaxPrice <= 0 {
				continue
			}
			if limit <= r.MaxPrice && (bestMax == 0 || r.MaxPrice < bestMax) {
				bestTick = r.Tick
				bestMax = r.MaxPrice
			}
		}
		if bestTick > 0 {
			return bestTick
		}
	}
	return spec.TickSize
}
