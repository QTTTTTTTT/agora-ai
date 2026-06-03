// Package lotsizegate is the broker-side lot-size compliance engine.
//
// # Purpose
//
// Every venue has rules about the smallest legal trade unit and the
// increment above it. Skipping the check causes "white-idiot" fills
// that brokers will route on a sim path but real venues will reject
// — turning a paper-OK plan into a live-trading P0 incident.
//
// The platform already has per-board normalisers in
// internal/instrument (NormalizeBuyQty / NormalizeSellQty /
// IsAligned) used by the PM agent and wiring layer; this package is
// the broker-side **safety net** that catches everything else:
//   - hand-edited orders bypassing the PM
//   - LLM-fallback paths that didn't quantize
//   - legacy code or test fixtures that emit raw quantities
//   - corp-action residuals (0.6 share holdings on A-share)
//   - misaligned partial sells on STAR / BSE / HK
//
// Triggering incident: 2026-06-03 audit revealed 4 historical bad
// fills on 301308 / 688195 / 688205 (ChiNext 1-share buy, STAR
// 85-share and 62-share partial sells) that all slipped past the
// upstream normalisers because the broker had no terminal gate.
//
// # Coverage matrix
//
//	Market        MinLot    Step    Residual rule           Status
//	─────────────────────────────────────────────────────────────────
//	A-share SH/SZ 100       100     Odd-lot must liquidate  ENFORCED
//	A-share CN    100       100     Odd-lot must liquidate  ENFORCED
//	A-share STAR  200       1       Odd-lot must liquidate  ENFORCED
//	A-share BSE   100       1       Odd-lot must liquidate  ENFORCED
//	HK equity     per-sym   per-sym Odd lots → board        ENFORCED*
//	US equity     1         1       Fractional via cap flag ENFORCED
//	Futures CN    1         1       Integer hands only      ENFORCED
//	Crypto        per-pair  per-pair Step + min-notional    ENFORCED*
//
//	* HK custom-lot table + crypto per-pair config loaded from the
//	  instrument_metadata table (S12.3). Engine ships with safe
//	  defaults (HK = 100, crypto = 1e-6 step) until the table is
//	  populated.
//
// # Design
//
// The engine is pure: given a Probe and an InstrumentSpec, it returns
// a Verdict. No I/O, no clocks, no DB. The production wiring in
// cmd/server pairs it with a positions/instrument-metadata source via
// the SpecSource and PositionSource interfaces so the engine itself
// stays deterministic and trivially testable.
//
// # Sell semantics
//
// Selling the entire holding is always legal (the gate consults
// PositionSource to know how much the fund actually holds). A partial
// sell whose residual would be < MinLot is rejected with a
// SuggestedQty that liquidates the whole position so the wiring layer
// can re-submit.
package lotsizegate

import (
	"context"

	"github.com/fundai/server/internal/instrument"
)

// AssetClass identifies the rule family. The classifier looks at the
// probe's Market + AssetClass + Symbol to pick the right one.
type AssetClass string

const (
	// AssetAShare covers SSE/SZSE/BSE equities. Lot rules are
	// board-specific (handled by instrument.SpecFor).
	AssetAShare AssetClass = "a_share"

	// AssetHKEquity covers HKEX-listed equities. Lot is per-symbol
	// (loaded from instrument_metadata; default 100).
	AssetHKEquity AssetClass = "hk_equity"

	// AssetUSEquity covers US equities. Integer shares unless the
	// venue advertises fractional capability.
	AssetUSEquity AssetClass = "us_equity"

	// AssetFutures covers futures (CN, US, HK). Integer hands only.
	AssetFutures AssetClass = "futures"

	// AssetCrypto covers crypto pairs. Step is per-pair (Binance:
	// BTC 1e-5, ETH 1e-4, ...). Defaults to 1e-6 step until the
	// metadata table is populated.
	AssetCrypto AssetClass = "crypto"

	// AssetUnknown short-circuits to allow (the engine should not
	// invent rules for asset classes it can't identify).
	AssetUnknown AssetClass = ""
)

// Probe is what the gate sees on each PlaceOrder. Mirrors
// broker.LotSizeProbe but kept independent so the engine doesn't
// import the broker package (would create a cycle with the
// production wiring).
//
// S12.5 — LimitPrice is consulted to enforce per-venue tick size
// (A-share 0.01 CNY, US 0.01 USD, HK banded, crypto per pair). 0
// means "market order" and the tick check is skipped.
type Probe struct {
	FundID         string
	InstrumentKey  string
	Symbol         string
	Market         string
	Exchange       string
	AssetClass     string
	InstrumentType string
	Side           string // "buy" | "sell" (case-insensitive)
	Quantity       float64
	LimitPrice     float64
	ClientOrderID  string
}

// Verdict is the engine output.
type Verdict struct {
	Rejected     bool
	RejectReason string
	Warnings     []string
	// SuggestedQty is a legal quantity the wiring layer can re-submit
	// when the engine knows one (e.g. floor to step, expand to
	// liquidate odd-lot residual). 0 means "no suggestion".
	SuggestedQty float64
	// AssetClass is the classification the engine used; surfaced for
	// metrics labelling.
	AssetClass AssetClass
}

// InstrumentSpec captures the per-instrument lot rules the engine
// needs. Sourced from internal/instrument for A-share boards and
// from instrument_metadata for everything else.
//
// S12.5 — TickSize / TickRules describe price-alignment rules. The
// engine prefers TickRules (price-banded HK style) when populated,
// falls back to TickSize (scalar) otherwise. Both 0 → no check.
type InstrumentSpec struct {
	AssetClass AssetClass
	MinLot float64
	Step float64
	SupportsFractional bool
	MinNotional float64
	TickSize    float64
	TickRules   []TickRule
}

// TickRule expresses a price-banded tick: any limit price ≤
// MaxPrice uses Tick as the alignment increment.
type TickRule struct {
	MaxPrice float64
	Tick     float64
}

// SpecSource resolves a Probe to an InstrumentSpec. Production
// implementation reads from internal/instrument for A-share boards
// and from a SpecRepo for everything else. Tests pass fakes.
type SpecSource interface {
	SpecFor(ctx context.Context, probe Probe) (InstrumentSpec, error)
}

// PositionSource reports the fund's current holding for a given
// instrument. The engine uses this to apply the A-share odd-lot
// residual rule on partial sells (residual < MinLot → expand to
// liquidate whole position). A 0 result is treated as "no position",
// which makes any sell illegal regardless of qty.
type PositionSource interface {
	HoldingQty(ctx context.Context, fundID, instrumentKey string) (float64, error)
}

// classifyAsset picks an AssetClass from the probe. Market hint
// wins; falls back to the symbol prefix via internal/instrument
// (which handles A-share boards from numeric prefixes).
func classifyAsset(p Probe) AssetClass {
	switch normalize(p.AssetClass) {
	case "futures", "future":
		return AssetFutures
	case "crypto", "cryptocurrency":
		return AssetCrypto
	}
	switch normalize(p.Market) {
	case "a_share", "a-share", "cn_stock", "cn", "china":
		return AssetAShare
	case "hk", "hk_stock", "hkex":
		return AssetHKEquity
	case "us", "us_stock", "us_equity", "nyse", "nasdaq":
		return AssetUSEquity
	case "futures-cn", "cffex", "shfe", "dce", "czce", "ine":
		return AssetFutures
	case "crypto":
		return AssetCrypto
	}
	// Numeric A-share prefix → AssetAShare. Anything else with
	// an unknown market is left AssetUnknown so the engine
	// short-circuits.
	if instrument.SpecFor(instrument.Classify(p.Symbol, instrument.Hint{})).IsAShare() {
		return AssetAShare
	}
	return AssetUnknown
}

func normalize(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c == ' ' || c == '\t' {
			continue
		}
		out = append(out, c)
	}
	return string(out)
}
