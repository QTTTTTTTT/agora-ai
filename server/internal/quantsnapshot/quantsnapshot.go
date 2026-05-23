// Package quantsnapshot builds per-symbol regime + volatility +
// position-size-ceiling summaries that get pasted into the PM's LLM
// prompt. It pulls together three existing primitives:
//
//   - regime.Service classifies each symbol as trend_up / trend_down /
//     range / chop / unknown using its own internal MA cascade and ATR
//     threshold.
//   - ohlc.Fetcher supplies the daily bars used both by the regime
//     classifier (indirectly) and by this package for the explicit
//     ATR(14) computation that drives position sizing.
//   - indicator.ATR returns Wilder's ATR which is the de-facto
//     volatility unit the position-sizing literature uses (Kelly
//     bounded by stop distance, Van Tharp risk-parity, etc).
//
// The output is a deliberately small struct per symbol — Symbol,
// Regime, Close, ATR14, ATRPct, PositionSizeCeilingPct — that the
// LLM prompt can ingest and that the prompt rules can reference by
// name. The builder is INTENTIONALLY tolerant of partial failures:
// a missing bar feed, a too-short history, or a transient fetcher
// error returns a Snapshot with the populated fields it could compute
// (or an empty Snapshot, in which case the wiring layer drops it).
// Decisions never block on quant snapshots; they're a soft prior.
//
// Why a separate package rather than living inside decision/? The
// decision package is intentionally light — it doesn't import OHLC /
// regime / indicator today and we want to keep it that way so the
// fallback engine stays a pure CPU loop. The wiring layer pulls in
// the builder and converts the Snapshot results into the
// decision.SymbolQuantSnapshot prompt-facing type the decision
// package already declares (see decision/engine.go).
package quantsnapshot

import (
	"context"
	"math"
	"strings"

	"github.com/fundai/server/internal/indicator"
	"github.com/fundai/server/internal/ohlc"
	"github.com/fundai/server/internal/regime"
)

// Snapshot is the per-symbol quantitative summary the PM prompt
// consumes. Every field is best-effort — a Snapshot may carry only
// Symbol + Regime when ATR fails to seed (e.g. instrument has fewer
// than ATRPeriod + 1 daily bars). The wiring layer skips Snapshots
// whose only populated field is Symbol because that signals "no
// data, no opinion".
type Snapshot struct {
	// Symbol is the same uppercased ticker the prompt uses
	// everywhere else (positions, universe, debate verdicts).
	Symbol string
	// Regime is one of regime.{TrendUp,TrendDown,Range,Chop}'s
	// string form; empty when the classifier returns Unknown.
	Regime string
	// Close is the latest closing price the ATR was anchored
	// against. Surfaced so the PM can sanity-check that the
	// snapshot isn't stale relative to the position's CurrentPrice.
	Close float64
	// ATR14 is Wilder's 14-bar ATR in price units. Reported even
	// when the regime is Unknown so the prompt can still surface a
	// volatility hint when the MA cascade is unstable.
	ATR14 float64
	// ATRPct is ATR14 / Close expressed as a percentage. Easier
	// for the LLM to reason about cross-symbol ("AAPL trades at
	// 1.4% daily TR vs BTC's 4.8%") than the raw price unit.
	ATRPct float64
	// PositionSizeCeilingPct is the upper bound on qtyPct for a
	// buy/add action on this symbol, derived from
	//   ceiling = riskBudgetPct / (stopATRMultiple * ATRPct)
	// clamped to [MinPositionPct, MaxPositionPct]. The prompt's
	// rule 3 already forbids any single order > 10% of NAV; this
	// number tightens that cap on a per-symbol basis so a 5% ATR
	// crypto name can't get sized like a 1% ATR blue chip.
	PositionSizeCeilingPct float64
}

// HasSignal reports whether a Snapshot carries any usable field
// beyond its bare Symbol. Used by the wiring layer to drop
// completely-empty snapshots so the prompt doesn't pollute itself
// with rows the LLM has to ignore.
func (s Snapshot) HasSignal() bool {
	if strings.TrimSpace(s.Regime) != "" {
		return true
	}
	if s.ATR14 > 0 || s.ATRPct > 0 || s.PositionSizeCeilingPct > 0 {
		return true
	}
	return false
}

// SymbolRequest names a single (symbol, market) pair to classify.
// Market is the same lowercase tag the rest of the codebase uses
// (a_share / us_equity / crypto / futures / hk_stock). Both fields
// are trimmed + normalised before the underlying fetch.
type SymbolRequest struct {
	Symbol string
	Market string
}

// Options tunes the volatility math without forcing callers to set
// every knob. Zero-value Options yields the production defaults the
// builder uses out of the box.
type Options struct {
	// LookbackBars is how many daily bars to ask the fetcher for.
	// ATR(14) needs at least 15 bars to produce its first valid
	// reading; 60 leaves headroom for IPO names + thinly traded
	// instruments + Wilder's initial smoothing.
	LookbackBars int
	// ATRPeriod is forwarded to indicator.ATR. 14 = the textbook
	// Wilder default and what every chart package uses.
	ATRPeriod int
	// RiskBudgetPct is the fraction of NAV the PM is willing to
	// risk on a single trade if the stop fills. 0.005 = 50 bps,
	// matching the prompt's "be quantitative" stance and the
	// existing strategy.Service sleeve budgets.
	RiskBudgetPct float64
	// StopATRMultiple is how many ATR units below entry the
	// implicit stop sits. 2× ATR is the trend-following standard
	// (PVB / Faber); mean-reversion sleeves can override down to
	// 1.5×.
	StopATRMultiple float64
	// MinPositionPct clamps the ceiling from below so an ultra-
	// volatile symbol doesn't produce a 0.001-NAV trade the lot-
	// rounder will floor to zero. 0.005 = 50 bps.
	MinPositionPct float64
	// MaxPositionPct clamps the ceiling from above. Defaults to
	// 0.10 to mirror the prompt's hard rule that no single order
	// exceed 10% of TotalAssets.
	MaxPositionPct float64
}

// withDefaults returns a fully-populated Options struct. Used by
// NewBuilder so callers can supply Options{LookbackBars: 90} and
// trust the rest will pick sensible values.
func (o Options) withDefaults() Options {
	if o.LookbackBars <= 0 {
		o.LookbackBars = 60
	}
	if o.ATRPeriod <= 0 {
		o.ATRPeriod = 14
	}
	if o.RiskBudgetPct <= 0 {
		o.RiskBudgetPct = 0.005
	}
	if o.StopATRMultiple <= 0 {
		o.StopATRMultiple = 2.0
	}
	if o.MinPositionPct <= 0 {
		o.MinPositionPct = 0.005
	}
	if o.MaxPositionPct <= 0 {
		o.MaxPositionPct = 0.10
	}
	if o.MaxPositionPct < o.MinPositionPct {
		o.MaxPositionPct = o.MinPositionPct
	}
	return o
}

// Builder is the stateful façade that holds the wired regime
// classifier + OHLC fetcher + tuning knobs. One Builder per
// runtime; safe for concurrent BuildBatch calls because the
// underlying services are themselves thread-safe.
type Builder struct {
	regimeSvc *regime.Service
	ohlc      ohlc.Fetcher
	opts      Options
	now       func() ohlc.Bar // unused today; reserved for backdated snapshot tests
}

// NewBuilder wires a builder. nil regimeSvc / nil ohlc fetcher are
// tolerated — BuildBatch will return an empty slice rather than
// panic, matching the rest of the wiring layer's "feature off when
// dependencies are missing" contract.
func NewBuilder(regimeSvc *regime.Service, fetcher ohlc.Fetcher, opts Options) *Builder {
	return &Builder{
		regimeSvc: regimeSvc,
		ohlc:      fetcher,
		opts:      opts.withDefaults(),
	}
}

// Options returns the resolved (defaults-applied) options the
// builder is using. Exposed so tests and observability can read
// back the actual numbers driving each snapshot.
func (b *Builder) Options() Options {
	if b == nil {
		return Options{}.withDefaults()
	}
	return b.opts
}

// BuildBatch produces one Snapshot per unique input request. Dupes
// are dropped (the same symbol/market pair only fires once). The
// returned slice is in input order (after dedup); requests whose
// fetch / regime / ATR all fail are still emitted so the wiring
// layer can decide whether to drop them via Snapshot.HasSignal.
//
// BuildBatch is intentionally NOT batched at the upstream level —
// the OHLC fetcher in production is the *Cache from internal/ohlc,
// so back-to-back fetches for the same symbol within the cache TTL
// are effectively free. Keeping the loop sequential also keeps the
// error budget predictable for the PM step: at worst we spend
// (N requests × fetcher RTT), which is what the runtime budget for
// the prompt-building phase is already sized for.
func (b *Builder) BuildBatch(ctx context.Context, requests []SymbolRequest) []Snapshot {
	if b == nil || b.ohlc == nil {
		return nil
	}
	out := make([]Snapshot, 0, len(requests))
	seen := make(map[string]struct{}, len(requests))
	for _, req := range requests {
		key := normalizeSymbol(req.Symbol) + "|" + strings.ToLower(strings.TrimSpace(req.Market))
		if key == "|" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		snap := b.buildOne(ctx, req)
		out = append(out, snap)
	}
	return out
}

// buildOne is the per-symbol pipeline. Fetches bars, classifies the
// regime, computes ATR. Each stage is independent so a partial
// success (regime OK but bars too short for ATR) still yields a
// useful Snapshot.
func (b *Builder) buildOne(ctx context.Context, req SymbolRequest) Snapshot {
	symbol := normalizeSymbol(req.Symbol)
	if symbol == "" {
		return Snapshot{}
	}
	snap := Snapshot{Symbol: symbol}

	// 1. Regime first — the regime service maintains its own
	// cache so this is usually a hash-map lookup. Errors are
	// silenced; Unknown is the natural "no opinion" return.
	if b.regimeSvc != nil {
		if r, err := b.regimeSvc.Classify(ctx, regime.Instrument{
			Symbol: symbol,
			Market: strings.ToLower(strings.TrimSpace(req.Market)),
		}); err == nil && r.IsKnown() {
			snap.Regime = r.String()
		}
	}

	// 2. Bars for ATR. ohlc.Cache will satisfy this from memory
	// 99% of the time because the regime service just fetched the
	// same instrument with a comparable lookback. The cache is
	// keyed by (symbol, market, interval, lookback) so we ask
	// for the same lookback the regime service uses by default
	// (max(MinBars + 30, 250)) to maximise cache reuse.
	lookback := b.opts.LookbackBars
	if lookback < b.opts.ATRPeriod+5 {
		lookback = b.opts.ATRPeriod + 5
	}
	bars, err := b.ohlc.Fetch(ctx, ohlc.FetchRequest{
		Symbol:    symbol,
		Market:    strings.ToLower(strings.TrimSpace(req.Market)),
		Interval:  ohlc.IntervalDay,
		LookbackN: lookback,
		// EndTime intentionally unset → the fetcher uses its
		// own "now" so backtests / live diverge predictably.
	})
	if err != nil || len(bars) <= b.opts.ATRPeriod {
		return snap
	}

	highs := make([]float64, len(bars))
	lows := make([]float64, len(bars))
	closes := make([]float64, len(bars))
	for i, bar := range bars {
		highs[i] = bar.High
		lows[i] = bar.Low
		closes[i] = bar.Close
	}
	atrSeries := indicator.ATR(highs, lows, closes, b.opts.ATRPeriod)
	if len(atrSeries) == 0 {
		return snap
	}
	atr := atrSeries[len(atrSeries)-1]
	if atr <= 0 || math.IsNaN(atr) || math.IsInf(atr, 0) {
		return snap
	}
	close := closes[len(closes)-1]
	if close <= 0 {
		return snap
	}
	snap.Close = close
	snap.ATR14 = atr
	snap.ATRPct = (atr / close) * 100.0

	// 3. Position-size ceiling. Formula: risk budget / (stop ×
	// ATR%) — clamp to [Min, Max]. ATRPct is in percent here, so
	// divide by 100 to get the fraction the formula expects.
	atrFraction := atr / close
	denom := b.opts.StopATRMultiple * atrFraction
	if denom <= 0 || math.IsNaN(denom) || math.IsInf(denom, 0) {
		return snap
	}
	ceiling := b.opts.RiskBudgetPct / denom
	if math.IsNaN(ceiling) || math.IsInf(ceiling, 0) {
		return snap
	}
	if ceiling < b.opts.MinPositionPct {
		ceiling = b.opts.MinPositionPct
	}
	if ceiling > b.opts.MaxPositionPct {
		ceiling = b.opts.MaxPositionPct
	}
	snap.PositionSizeCeilingPct = ceiling
	return snap
}

// normalizeSymbol uppercases + trims a ticker. Centralised so the
// dedup key and the Snapshot.Symbol field stay byte-identical.
func normalizeSymbol(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}
