// slippage.go — matching.SlippageModel adapter.
//
// This file is the bridge between the pure marketimpact engine
// and the broker's matching pipeline. It implements
// matching.SlippageModel by:
//
//  1. Reading the calibration row out of the in-memory Cache
//     (no DB hit on the hot path).
//  2. Asking the Engine for the adverse-bps Estimate.
//  3. Using SpreadCrossSlippage as the base-price source so a
//     buy fills at the ask (or last when bid/ask are missing),
//     a sell fills at the bid, and the impact bps is applied
//     on top.
//
// Why a Cache + Engine + inner-SlippageModel triplet
//
// The matching engine needs a synchronous, no-context, pure
// FillPrice call. We can't reach into Postgres on every order.
// The Cache resolves that. The Engine is pure. The inner
// SlippageModel decides the base price (ask/bid/last); the
// engine's bps is applied on top. Splitting the two lets us
// reuse SpreadCrossSlippage's well-tested behaviour without
// re-implementing it.

package marketimpact

import (
	"strings"

	"github.com/fundai/server/internal/matching"
)

// Lookup is the cache contract the adapter needs. *Cache
// satisfies it; tests can pass a stub.
type Lookup interface {
	Lookup(instrumentKey string) *Liquidity
}

// EstimateRecorder is an optional sink for telemetry — the
// adapter calls it on every FillPrice call so cmd/server can
// emit Prometheus metrics ("how often did we use defaults?
// how often did we hit the ADV-fallback?"). nil = no telemetry.
type EstimateRecorder interface {
	RecordEstimate(probe OrderProbe, est Estimate)
}

// SlippageAdapter is the concrete matching.SlippageModel.
type SlippageAdapter struct {
	Cache    Lookup
	Engine   Model
	Inner    matching.SlippageModel    // base-price source; default = SpreadCrossSlippage{}
	Recorder EstimateRecorder
}

// NewSlippageAdapter constructs an adapter with sane defaults.
// cache + engine are required; passing nil for either falls
// back to a no-op (used only by tests / panic-prevention in
// boot).
func NewSlippageAdapter(cache Lookup, engine Model, opts ...AdapterOption) *SlippageAdapter {
	a := &SlippageAdapter{
		Cache:  cache,
		Engine: engine,
		Inner:  matching.SpreadCrossSlippage{},
	}
	for _, opt := range opts {
		opt(a)
	}
	if a.Inner == nil {
		a.Inner = matching.SpreadCrossSlippage{}
	}
	return a
}

// AdapterOption configures a SlippageAdapter.
type AdapterOption func(*SlippageAdapter)

// WithInnerModel overrides the base-price source.
func WithInnerModel(m matching.SlippageModel) AdapterOption {
	return func(a *SlippageAdapter) { a.Inner = m }
}

// WithRecorder wires in a telemetry sink.
func WithRecorder(r EstimateRecorder) AdapterOption {
	return func(a *SlippageAdapter) { a.Recorder = r }
}

// FillPrice implements matching.SlippageModel.
func (a *SlippageAdapter) FillPrice(order matching.Order, quote matching.Quote) float64 {
	if a == nil {
		return matching.ZeroSlippage{}.FillPrice(order, quote)
	}
	inner := a.Inner
	if inner == nil {
		inner = matching.SpreadCrossSlippage{}
	}
	base := inner.FillPrice(order, quote)
	// The matching engine treats negative prices as
	// invalid; if the inner model returned <= 0 the order will
	// be rejected anyway. Skip impact in that case.
	if base <= 0 {
		return base
	}
	if a.Engine == nil {
		return base
	}

	probe := OrderProbe{
		InstrumentKey: order.InstrumentKey,
		Symbol:        order.InstrumentKey, // best-effort; cmd/server passes a single key
		AssetClass:    strings.ToLower(strings.TrimSpace(order.AssetClass)),
		Side:          string(order.Side),
		Quantity:      order.Quantity,
		ReferencePx:   base,
	}
	var calib *Liquidity
	if a.Cache != nil {
		calib = a.Cache.Lookup(order.InstrumentKey)
	}
	est := a.Engine.Estimate(probe, calib)
	if a.Recorder != nil {
		a.Recorder.RecordEstimate(probe, est)
	}
	if est.AdverseBps <= 0 {
		return base
	}
	return ApplyAdverse(base, string(order.Side), est.AdverseBps)
}

// EstimateForProbe is exposed so the admin "preview" endpoint
// can run the same engine-pipeline that the simulator uses.
//
// Called outside the hot path (one HTTP call per click), so it's
// fine that this re-traverses the cache.
func (a *SlippageAdapter) EstimateForProbe(probe OrderProbe) Estimate {
	if a == nil || a.Engine == nil {
		return Estimate{Reason: "no-adapter"}
	}
	var calib *Liquidity
	if a.Cache != nil {
		calib = a.Cache.Lookup(probe.InstrumentKey)
	}
	return a.Engine.Estimate(probe, calib)
}
