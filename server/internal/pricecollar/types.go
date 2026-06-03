// Package pricecollar implements the "is this limit price within a
// believable distance of the reference price?" gate. It is the
// second layer of defense after marketstatus: marketstatus catches
// halts / calendar / stale-quote / exchange price-limit; pricecollar
// catches the operator-side fat-finger / PM-bug / LLM-hallucination
// case where the order itself is the source of the bad price.
//
// Trigger story: on 2026-06-02 a PM fallback path stamped the
// notional buy budget (96,226 CNY) into PlanAction.Price with
// quantity=1 because the quote service couldn't reach the symbol.
// The broker simulator faithfully honoured the 96,226 CNY/share
// "limit" and produced a fill. True mid was ~500 CNY. Even after
// fixing the PM fallback (Action=watch) we need a broker-side
// safety net so future fat-fingers / LLM hallucinations / bad
// pasted prices can't make the next 96,226 happen.
//
// Why a separate gate rather than baking the check into the
// simulator:
//
//   - Reuse: the same engine should run for any future broker
//     implementation (live IBKR / Schwab / CTP adapter), not just
//     the simulator. Keeping it package-local makes that a single
//     import.
//   - Testability: pure rule + a tiny ReferenceQuote source means
//     the engine has no DB / network deps. The wiring layer plugs
//     in marketdata + a small adapter; the engine is exercised
//     with table tests.
//   - Decoupling: the broker package stays out of marketdata,
//     marketstatus and pricecollar dependency graphs.
//
// Thresholds. We default to per-asset-class collars chosen to sit
// just above the exchange-enforced daily price band. They are
// deliberately loose enough that a normal limit price almost never
// trips them, and tight enough that the 96,226 vs ~500 case (about
// 19,000% off) is a 200x rejection. Operators override per market
// via DefaultThresholdBps / SetMarketThreshold.

package pricecollar

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Decision mirrors marketstatus.Decision so admin dashboards can
// fold both gates into the same UI / metric labels.
type Decision string

const (
	DecisionAllow  Decision = "allow"
	DecisionWarn   Decision = "warn"
	DecisionReject Decision = "reject"
)

// RuleCode is the closed vocabulary the gate emits.
type RuleCode string

const (
	// RulePriceCollar fires when |intended - reference| / reference
	// exceeds the configured tolerance.
	RulePriceCollar RuleCode = "price_collar"

	// RuleNoReference fires when the engine can't obtain a usable
	// reference quote (no quote at all, or quote is too stale to
	// be trusted as a comparison baseline). Configurable: the
	// default mode is warn (the order proceeds, marketstatus's
	// stale_quote rule will already have made noise upstream) but
	// strict operators can flip it to reject.
	RuleNoReference RuleCode = "price_collar_no_reference"
)

// detectorVersion gets stamped on every event so a rules upgrade
// can be replayed without dedup ambiguity. Match the convention
// from marketstatus.
const detectorVersion = "v1"

// Default tolerances by asset class, expressed in basis points
// (10,000 = 100%). Chosen to sit comfortably above the daily
// exchange price band so a normal limit price almost never trips
// the gate.
//
//   - A-share main board (sh / sz 600/000/001/002): 10% daily band
//     → 1,100 bps cap (10% + 1% buffer for after-hours / pre-open
//     limit moves that aren't yet reflected in the reference quote).
//   - A-share ChiNext / STAR / BSE (300/301/688/688/8): 20% daily
//     band → 2,100 bps cap.
//   - US equities (NYSE / NASDAQ): no statutory daily band, but
//     LULD Tier-1 is 5%, Tier-2 10%; we use 1,500 bps as a
//     fat-finger collar matching the common broker default.
//   - HK equities: no daily band; 3,000 bps matches HKEX's "extreme
//     deviation" guard for ambush trades.
//   - Crypto: 24/7 high volatility, 3,000 bps.
//   - Futures (CN / US): ~10% typical daily band; 2,000 bps.
//   - Bond / OTC: hand-picked illiquids where quote refresh can be
//     hours stale; allow 3,000 bps so legitimate quote drift
//     doesn't false-positive.
var defaultThresholdBps = map[string]int{
	"equity":  1500,
	"etf":     1500,
	"futures": 2000,
	"crypto":  3000,
	"option":  3000,
	"bond":    3000,
	"otc":     3000,
}

const fallbackThresholdBps = 1500

// A-share specialised thresholds: the asset class is "equity" but
// the exchange daily band depends on the board. Lookup is done on
// (market, exchange, ChiNext/STAR/BSE-prefix). When market=a_share
// the EngineCheck overrides defaultThresholdBps["equity"] with one
// of these.
const (
	aShareMainBoardThresholdBps  = 1100
	aShareWideBoardThresholdBps  = 2100
)

// ChiNext (300/301), STAR (688/689), BSE (8) all have a 20% daily
// limit; the rest of the A-share universe is 10%. We use prefix
// matching against the symbol because the calendar / status table
// doesn't carry the board attribution yet.
func isAShareWideBoard(symbol string) bool {
	s := strings.TrimSpace(symbol)
	if len(s) == 0 {
		return false
	}
	// ChiNext: 300, 301
	if len(s) >= 3 && (strings.HasPrefix(s, "300") || strings.HasPrefix(s, "301")) {
		return true
	}
	// STAR: 688, 689
	if len(s) >= 3 && (strings.HasPrefix(s, "688") || strings.HasPrefix(s, "689")) {
		return true
	}
	// BSE: 8XXXXX
	if len(s) >= 1 && strings.HasPrefix(s, "8") {
		return true
	}
	return false
}

// ReferenceQuote is what the engine compares against. We keep it
// independent of the marketdata types so this package has no
// downstream import — the wiring layer translates the broker's
// chosen quote source into this shape.
type ReferenceQuote struct {
	InstrumentKey string
	Symbol        string
	Market        string
	AssetClass    string
	Price         float64
	// AsOf is when the price was observed. Engine combines this
	// with MaxAge to decide whether the reference is fresh enough
	// to compare against.
	AsOf          time.Time
}

// ReferenceSource resolves a probe to a reference quote. Implementations
// typically hit marketdata.GetQuote first and fall back to the
// last persisted instrument_market_status quote when live providers
// are down. Returning (nil, nil) means "no usable reference" — the
// engine then routes through the RuleNoReference path.
type ReferenceSource interface {
	GetReferenceQuote(ctx context.Context, probe Probe) (*ReferenceQuote, error)
}

// Probe is the engine input for a prospective order.
type Probe struct {
	FundID        string
	InstrumentKey string
	Symbol        string
	Market        string
	AssetClass    string
	Side          string  // "buy" | "sell"
	Quantity      float64
	IntendedPrice float64 // 0 → market order; engine SHORT-CIRCUITS to allow
	ClientOrderID string
}

// Event is one rule firing.
type Event struct {
	RuleCode        RuleCode
	Decision        Decision
	Summary         string
	Metadata        map[string]any
	DetectedAt      time.Time
	DetectorVersion string
}

// CheckResult is the engine's full verdict.
type CheckResult struct {
	Decision Decision
	Events   []Event
	// Reference is the resolved reference quote (if any). Returned
	// even on Allow so the wiring layer can record it.
	Reference *ReferenceQuote
	// AppliedThresholdBps is the threshold (in basis points) the
	// engine used for this probe. Returned for audit so operators
	// can see exactly which asset-class default / per-market
	// override was in force.
	AppliedThresholdBps int
}

// Reject reports whether the engine rejected. Convenience for
// callers that don't care about the per-event detail.
func (r CheckResult) Reject() bool { return r.Decision == DecisionReject }

// Warn reports whether the engine warned (and only warned).
func (r CheckResult) Warn() bool { return r.Decision == DecisionWarn }

// Errors callers might type-assert.
var (
	ErrInvalidProbe = errors.New("pricecollar: invalid probe")
)

// EngineOptions controls non-default behaviour. Zero value is
// sane for production: warn on missing reference, default per-
// asset-class thresholds, max reference age 10 minutes.
type EngineOptions struct {
	// MaxReferenceAge is how stale a reference quote is allowed
	// to be before the engine treats it as no-reference. Default
	// 10 minutes. Set to 0 to disable the freshness check (NOT
	// recommended in production — a 3-day-old quote is no
	// reference at all).
	MaxReferenceAge time.Duration

	// NoReferenceDecision is the verdict when the reference source
	// returns (nil, nil) or the reference is too stale. Default
	// DecisionWarn — the order proceeds, marketstatus's stale_quote
	// rule will already have surfaced an upstream warning. Set to
	// DecisionReject for strict deployments.
	NoReferenceDecision Decision

	// OverrideThresholdBpsByMarket lets operators tighten or loosen
	// the asset-class default for specific markets without code
	// changes. Key = canonical market string ("a_share", "us_equity",
	// "crypto", …). Value <=0 means use the asset-class default.
	OverrideThresholdBpsByMarket map[string]int

	// Now is the clock seam for tests. Defaults to time.Now.
	Now func() time.Time
}

// applyDefaults fills zero-valued options with safe production defaults.
func (o EngineOptions) applyDefaults() EngineOptions {
	out := o
	if out.MaxReferenceAge <= 0 {
		out.MaxReferenceAge = 10 * time.Minute
	}
	if out.NoReferenceDecision == "" {
		out.NoReferenceDecision = DecisionWarn
	}
	if out.Now == nil {
		out.Now = func() time.Time { return time.Now().UTC() }
	}
	return out
}

// ResolveThresholdBps returns the threshold in basis points the
// engine would apply to a probe. Exported so the wiring layer can
// surface it in metrics labels without having to call Check.
//
// Precedence (highest first):
//
//  1. options.OverrideThresholdBpsByMarket[market]
//  2. A-share board-specific specialisation (wide board 21%,
//     main board 11%)
//  3. defaultThresholdBps[assetClass]
//  4. fallbackThresholdBps (1,500 bps)
func ResolveThresholdBps(opts EngineOptions, probe Probe) int {
	market := strings.ToLower(strings.TrimSpace(probe.Market))
	assetClass := strings.ToLower(strings.TrimSpace(probe.AssetClass))

	if opts.OverrideThresholdBpsByMarket != nil {
		if v, ok := opts.OverrideThresholdBpsByMarket[market]; ok && v > 0 {
			return v
		}
	}
	if market == "a_share" || market == "cn_a_share" {
		if isAShareWideBoard(probe.Symbol) {
			return aShareWideBoardThresholdBps
		}
		return aShareMainBoardThresholdBps
	}
	if v, ok := defaultThresholdBps[assetClass]; ok && v > 0 {
		return v
	}
	return fallbackThresholdBps
}

// formatPct turns basis-points into a human-readable percentage
// like "12.5%". Used in summary strings.
func formatBpsPct(bps int) string {
	pct := float64(bps) / 100.0
	return fmt.Sprintf("%g%%", pct)
}
