package lotsizegate

import (
	"context"

	"github.com/fundai/server/internal/instrument"
)

// DefaultSpecSource is the production SpecSource. It resolves A-share
// boards via internal/instrument (deterministic, prefix-driven) and
// defers to a HKEXLotResolver / CryptoStepResolver for HK / crypto.
// US equity defaults to {MinLot: 0, Step: 1, SupportsFractional:
// false} until the venue's Broker.Capabilities advertises
// fractional support (S12.6).
//
// Operators can pass nil for the HK / crypto resolvers; the engine
// then falls back to safe defaults (HK lot=100, crypto step=1e-6)
// so a missing metadata table doesn't disable the gate entirely.
type DefaultSpecSource struct {
	HK        HKLotResolver
	Crypto    CryptoStepResolver
	Tick      TickResolver
	Overrides OverridesResolver
}

// HKLotResolver looks up the board-lot size for an HK-listed symbol.
// Implementations read from instrument_metadata (S12.3).
type HKLotResolver interface {
	LotFor(ctx context.Context, symbol string) (lot int, ok bool)
}

// CryptoStepResolver looks up the per-pair step size and min notional.
// Implementations read from instrument_metadata.
type CryptoStepResolver interface {
	StepFor(ctx context.Context, symbol string) (step, minNotional float64, ok bool)
}

// TickResolver looks up the per-instrument tick size + banded tick
// rules (S12.5). Implementations read from instrument_metadata.
// Returns (0, nil, false) when the instrument has no tick rules
// recorded — the engine then defaults to "no tick check".
type TickResolver interface {
	TickFor(ctx context.Context, instrumentKey, symbol string) (scalar float64, rules []TickRule, ok bool)
}

// OverridesResolver returns admin-side overrides for the asset's
// SupportsFractional / MinNotional / ContractMultiplier fields.
// Used by S12.6 so admins can flip US fractional or futures
// contract multipliers from instrument_metadata without code
// changes. Resolver miss → defaults stay.
type OverridesResolver interface {
	OverridesFor(ctx context.Context, instrumentKey, symbol string) (Overrides, bool)
}

// Overrides is the value bag from OverridesResolver. Fields the
// caller doesn't want to override should be set to the zero value
// (the resolver's "ok" boolean controls whether anything is
// applied at all).
type Overrides struct {
	SupportsFractional bool
	MinNotional        float64
	ContractMultiplier float64
}

// SpecFor implements SpecSource.
func (s *DefaultSpecSource) SpecFor(ctx context.Context, probe Probe) (InstrumentSpec, error) {
	asset := classifyAsset(probe)
	spec := InstrumentSpec{AssetClass: AssetUnknown}

	switch asset {
	case AssetAShare:
		boardSpec := instrument.SpecFor(instrument.Classify(probe.Symbol, instrument.Hint{
			Market:     probe.Market,
			Exchange:   probe.Exchange,
			AssetClass: probe.AssetClass,
		}))
		if !boardSpec.IsAShare() {
			return spec, nil
		}
		spec = InstrumentSpec{
			AssetClass: AssetAShare,
			MinLot:     float64(boardSpec.MinLot),
			Step:       float64(boardSpec.Step),
			TickSize:   0.01, // A-share fixed tick across boards
		}

	case AssetHKEquity:
		lot := 100
		if s != nil && s.HK != nil {
			if v, ok := s.HK.LotFor(ctx, probe.Symbol); ok && v > 0 {
				lot = v
			}
		}
		spec = InstrumentSpec{
			AssetClass: AssetHKEquity,
			MinLot:     float64(lot),
			Step:       float64(lot),
		}

	case AssetUSEquity:
		spec = InstrumentSpec{
			AssetClass:         AssetUSEquity,
			MinLot:             1,
			Step:               1,
			SupportsFractional: false,
			TickSize:           0.01,
		}

	case AssetFutures:
		spec = InstrumentSpec{
			AssetClass: AssetFutures,
			MinLot:     1,
			Step:       1,
		}

	case AssetCrypto:
		step := 1e-6
		var minNotional float64
		if s != nil && s.Crypto != nil {
			if st, mn, ok := s.Crypto.StepFor(ctx, probe.Symbol); ok && st > 0 {
				step = st
				minNotional = mn
			}
		}
		spec = InstrumentSpec{
			AssetClass:         AssetCrypto,
			MinLot:             0,
			Step:               step,
			SupportsFractional: true,
			MinNotional:        minNotional,
			TickSize:           step, // crypto tick == step by convention
		}

	default:
		return spec, nil
	}

	// S12.5 — overlay tick metadata from instrument_metadata when
	// the resolver is wired. Banded HK rules trump the scalar
	// tick set above. Resolver miss → keep the safe scalar
	// default.
	if s != nil && s.Tick != nil {
		if scalar, rules, ok := s.Tick.TickFor(ctx, probe.InstrumentKey, probe.Symbol); ok {
			if len(rules) > 0 {
				spec.TickRules = rules
			}
			if scalar > 0 {
				spec.TickSize = scalar
			}
		}
	}

	// S12.6 — admin-side overrides flip SupportsFractional /
	// MinNotional / ContractMultiplier on individual instruments.
	// The hot-path knob this powers is "make AAPL fractional on
	// this fund's broker without a code change": admin upserts
	// instrument_metadata with supports_fractional=true and the
	// gate honours it on the next order.
	if s != nil && s.Overrides != nil {
		if ov, ok := s.Overrides.OverridesFor(ctx, probe.InstrumentKey, probe.Symbol); ok {
			if ov.SupportsFractional {
				spec.SupportsFractional = true
			}
			if ov.MinNotional > 0 {
				spec.MinNotional = ov.MinNotional
			}
			// ContractMultiplier isn't consumed by the lot-size
			// engine today, but storing it on the spec lets
			// future passes (futures notional check, margin
			// recompute) read it via the same wiring.
		}
	}

	return spec, nil
}
