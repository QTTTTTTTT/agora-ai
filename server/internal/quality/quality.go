// Package quality computes per-symbol cross-sectional quality
// factor z-scores from existing fundamental.Metrics inputs.
//
// Sprint E #3. The PM prompt already carries a free-form
// FundamentalSummary string built upstream — useful prose, but
// the LLM can't sort by it or compare two names directly. This
// package adds the structured channel: a per-symbol composite
// quality score plus the three sub-factor z-scores
// (Profitability / Growth / Safety) the LLM can read like a
// table and cite verbatim.
//
// The decomposition follows the Asness / Frazzini / Pedersen
// "Quality Minus Junk" (2013) recipe — the most widely-cited
// open-academia factor in long-only quant:
//
//   - Profitability: ROE + operating margin + profit margin
//     (z-scored across the universe, weighted blend).
//   - Growth:        revenue growth YoY + earnings growth YoY
//     (z-scored, weighted blend).
//   - Safety:        -1 × debt/equity z-score (lower = safer,
//     hence the negation).
//
// Composite Z = 0.4 Profitability + 0.3 Growth + 0.3 Safety,
// then quartile-bucketed (1 = top, 4 = bottom). Operators can
// retune the weights per fund via Options.
//
// Architecture mirrors ranking + correlation + earnings:
//   - SymbolRequest names a (symbol, market) pair.
//   - Options carries weights / floors / fetcher tier.
//   - Service holds the fundamental.Fetcher + opts.
//   - BuildScores returns one Score per symbol with enough data,
//     sorted descending by CompositeZ.
//
// Missing values: a symbol that has data for AT LEAST ONE
// sub-factor still produces a Score (the other sub-factors
// contribute zero to the composite). A symbol with zero usable
// fundamentals is dropped entirely. The z-score for any
// sub-factor with fewer than MinUniverse symbols reporting it
// is silently zero (no comparison group means no relative
// quality call), so the composite still works for partially-
// covered universes — exactly what every real production
// universe looks like.
package quality

import (
	"context"
	"math"
	"sort"
	"strings"

	"github.com/fundai/server/internal/fundamental"
)

// Score is one symbol's quality factor reading. All z-scores are
// cross-sectional within the calling universe — a +1 means "one
// standard deviation above the universe mean for this sub-factor".
type Score struct {
	// Symbol is the upper-cased ticker. Matches every other
	// prompt block's casing convention.
	Symbol string
	// ProfitabilityZ is the weighted z-score of ROE + operating
	// margin + profit margin. Higher = more profitable than peers.
	ProfitabilityZ float64
	// GrowthZ is the weighted z-score of revenue + earnings
	// growth. Higher = growing faster than peers.
	GrowthZ float64
	// SafetyZ is -1 × z(debt/equity). Higher = less leverage
	// than peers. The negation lives in the score (not the
	// composite weight) so the system prompt can read "safety
	// = high" without the operator remembering "negative debt".
	SafetyZ float64
	// CompositeZ is the weighted blend (default 0.4 / 0.3 / 0.3)
	// the PM should sort by. Higher = higher quality.
	CompositeZ float64
	// Quartile is the cross-sectional bucket 1..4 by CompositeZ.
	// 1 = top quartile (best quality), 4 = bottom. Zero when the
	// universe is too small for quartile bucketing (< 4 symbols).
	Quartile int
	// ComponentsAvailable counts how many of the three sub-factors
	// the symbol actually reported data for. The PM prompt uses
	// this to discount a symbol whose composite is only built on
	// (e.g.) safety alone — "quality" with no profit / growth
	// data isn't really a quality signal.
	ComponentsAvailable int
}

// SymbolRequest names a (symbol, market) pair to score. Same
// shape as ranking / quantsnapshot so the wiring layer can reuse
// one request list.
type SymbolRequest struct {
	Symbol string
	Market string
}

// Options tunes the sub-factor weights, the composite weights,
// and the per-universe floors. Zero-value Options yields the
// production defaults (0.4 profitability / 0.3 growth / 0.3
// safety; 3-symbol min for sub-factor z-scoring).
type Options struct {
	// Sub-factor weights inside Profitability. Default
	// 0.5 ROE / 0.3 OpMargin / 0.2 ProfitMargin — emphasises
	// ROE because it's the single best forward-return predictor
	// in the QMJ paper. Set any to negative to short the factor;
	// any unset (zero) entry uses the default.
	ROEWeight             float64
	OperatingMarginWeight float64
	ProfitMarginWeight    float64

	// Sub-factor weights inside Growth. Default 0.6 earnings,
	// 0.4 revenue — earnings growth is the harder, more
	// information-dense signal.
	EarningsGrowthWeight float64
	RevenueGrowthWeight  float64

	// Composite weights. Default 0.4 / 0.3 / 0.3. Operators
	// who want a pure "Buffet quality" (max profitability,
	// ignore growth) can push ProfitabilityWeight up here.
	ProfitabilityWeight float64
	GrowthWeight        float64
	SafetyWeight        float64

	// MinUniverse is the floor below which we skip the entire
	// scoring pass. Z-scoring a 2-symbol universe yields ±0.71
	// for every entry — useless. 3 is the theoretical minimum
	// for sample stdev != 0 in the degenerate case.
	MinUniverse int

	// PerFactorMin is the floor below which a specific
	// sub-factor (e.g. growth) is z-scored. When fewer than
	// PerFactorMin symbols report a sub-factor, every entry's
	// z-score for that sub-factor is 0 (no comparison group).
	// Defaults to MinUniverse.
	PerFactorMin int
}

func (o Options) withDefaults() Options {
	if o.ROEWeight == 0 && o.OperatingMarginWeight == 0 && o.ProfitMarginWeight == 0 {
		o.ROEWeight = 0.5
		o.OperatingMarginWeight = 0.3
		o.ProfitMarginWeight = 0.2
	}
	if o.EarningsGrowthWeight == 0 && o.RevenueGrowthWeight == 0 {
		o.EarningsGrowthWeight = 0.6
		o.RevenueGrowthWeight = 0.4
	}
	if o.ProfitabilityWeight == 0 && o.GrowthWeight == 0 && o.SafetyWeight == 0 {
		o.ProfitabilityWeight = 0.4
		o.GrowthWeight = 0.3
		o.SafetyWeight = 0.3
	}
	if o.MinUniverse <= 0 {
		o.MinUniverse = 3
	}
	if o.PerFactorMin <= 0 {
		o.PerFactorMin = o.MinUniverse
	}
	return o
}

// Service is the per-process orchestrator. Holds the fundamental
// fetcher (typically the cached registry the rest of the wiring
// uses) and the resolved options. Safe for concurrent use —
// state is immutable post-construction.
type Service struct {
	fetcher fundamental.Fetcher
	opts    Options
}

// NewService wires the Service. nil fetcher is tolerated;
// BuildScores returns nil in that case so the prompt skips the
// block. Matches the rest of the wiring layer's "feature off
// when the dependency is missing" contract.
func NewService(fetcher fundamental.Fetcher, opts Options) *Service {
	return &Service{
		fetcher: fetcher,
		opts:    opts.withDefaults(),
	}
}

// Options exposes the resolved tuning struct.
func (s *Service) Options() Options {
	if s == nil {
		return Options{}.withDefaults()
	}
	return s.opts
}

// BuildScores returns the cross-sectional Score per symbol with
// at least one usable sub-factor reading, sorted by descending
// CompositeZ. Returns nil when fewer than MinUniverse symbols
// produce any usable data, so the prompt block is omitted.
//
// One Fetch per dedup'd (symbol, market). The fundamental cache
// makes repeat calls free across the runtime.
func (s *Service) BuildScores(ctx context.Context, requests []SymbolRequest) []Score {
	if s == nil || s.fetcher == nil {
		return nil
	}
	raw := s.gatherMetrics(ctx, requests)
	if len(raw) < s.opts.MinUniverse {
		return nil
	}
	// Per-sub-factor z-score across the universe. NaN entries
	// (symbol didn't report that metric) are excluded from the
	// universe's mean / stdev — the symbol still appears in the
	// output Score with that sub-factor at z=0.
	zROE := crossSectionalZ(extract(raw, fROE), s.opts.PerFactorMin)
	zOpMargin := crossSectionalZ(extract(raw, fOpMargin), s.opts.PerFactorMin)
	zProfitMargin := crossSectionalZ(extract(raw, fProfitMargin), s.opts.PerFactorMin)
	zEarningsGrowth := crossSectionalZ(extract(raw, fEarningsGrowth), s.opts.PerFactorMin)
	zRevenueGrowth := crossSectionalZ(extract(raw, fRevenueGrowth), s.opts.PerFactorMin)
	zDebtEquity := crossSectionalZ(extract(raw, fDebtEquity), s.opts.PerFactorMin)

	scores := make([]Score, 0, len(raw))
	for i, m := range raw {
		prof, profOK := blend(
			[]float64{zROE[i], zOpMargin[i], zProfitMargin[i]},
			[]float64{s.opts.ROEWeight, s.opts.OperatingMarginWeight, s.opts.ProfitMarginWeight},
		)
		growth, growthOK := blend(
			[]float64{zEarningsGrowth[i], zRevenueGrowth[i]},
			[]float64{s.opts.EarningsGrowthWeight, s.opts.RevenueGrowthWeight},
		)
		// Safety is the NEGATION of debt-equity z so higher =
		// safer. We invert before composing.
		var safety float64
		safetyOK := !math.IsNaN(zDebtEquity[i])
		if safetyOK {
			safety = -zDebtEquity[i]
		}

		// Count how many sub-factors fired.
		availability := 0
		if profOK {
			availability++
		}
		if growthOK {
			availability++
		}
		if safetyOK {
			availability++
		}
		if availability == 0 {
			// Symbol had NO usable sub-factor — drop entirely.
			// Keeping it would float CompositeZ at 0 and pollute
			// the quartile distribution.
			continue
		}

		composite, _ := blend(
			[]float64{prof, growth, safety},
			[]float64{s.opts.ProfitabilityWeight, s.opts.GrowthWeight, s.opts.SafetyWeight},
		)
		scores = append(scores, Score{
			Symbol:              m.Symbol,
			ProfitabilityZ:      prof,
			GrowthZ:             growth,
			SafetyZ:             safety,
			CompositeZ:          composite,
			ComponentsAvailable: availability,
		})
	}

	if len(scores) == 0 {
		return nil
	}

	// Sort descending by CompositeZ for prompt readability.
	sort.Slice(scores, func(i, j int) bool {
		if scores[i].CompositeZ != scores[j].CompositeZ {
			return scores[i].CompositeZ > scores[j].CompositeZ
		}
		return scores[i].Symbol < scores[j].Symbol
	})

	// Quartile bucketing: 1 = top 25%, 4 = bottom. Only
	// meaningful when the universe is large enough that each
	// quartile gets at least one symbol — 4 is the floor.
	if len(scores) >= 4 {
		for i := range scores {
			// Sort is descending so the FIRST quartile (top) is
			// index 0..len/4-1. Using ceil divisor so symbols
			// distribute evenly even when len isn't divisible.
			q := (i*4)/len(scores) + 1
			if q > 4 {
				q = 4
			}
			scores[i].Quartile = q
		}
	}

	return scores
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// fieldTag identifies which Metrics field a generic extractor
// pulls out. The math layer below is field-agnostic; this enum
// keeps the call-sites readable.
type fieldTag int

const (
	fROE fieldTag = iota
	fOpMargin
	fProfitMargin
	fEarningsGrowth
	fRevenueGrowth
	fDebtEquity
)

// gatherMetrics dedups + normalises the symbol list, fires one
// Fetch per unique (symbol, market), and returns the surviving
// Metrics in stable request order. Errors are swallowed — a
// per-symbol Fetch failure simply drops that symbol from the
// scoring pass (same contract as ranking).
func (s *Service) gatherMetrics(ctx context.Context, requests []SymbolRequest) []fundamental.Metrics {
	seen := make(map[string]struct{}, len(requests))
	out := make([]fundamental.Metrics, 0, len(requests))
	for _, req := range requests {
		sym := strings.ToUpper(strings.TrimSpace(req.Symbol))
		mkt := strings.ToLower(strings.TrimSpace(req.Market))
		if sym == "" {
			continue
		}
		key := sym + "|" + mkt
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		m, err := s.fetcher.Fetch(ctx, fundamental.FetchRequest{
			Symbol: sym,
			Market: mkt,
		})
		if err != nil || m == nil {
			continue
		}
		// Force the canonical ticker; some providers echo back
		// lowercase / mixed-case.
		m.Symbol = sym
		out = append(out, *m)
	}
	return out
}

// extract pulls one fieldTag out of every Metrics in the slice.
// Returns a NaN slot for each symbol whose value is zero — the
// fundamental.Metrics convention treats zero as "not reported"
// for ratio fields (a real ROE of exactly 0.000000 is so
// improbable in practice that the false-negative cost is zero
// and the false-positive cost of treating every "not reported"
// as a real datum is very high — it would skew the universe's
// mean / stdev toward 0).
func extract(rows []fundamental.Metrics, tag fieldTag) []float64 {
	out := make([]float64, len(rows))
	for i, m := range rows {
		var v float64
		switch tag {
		case fROE:
			v = m.ReturnOnEquity
		case fOpMargin:
			v = m.OperatingMargin
		case fProfitMargin:
			v = m.ProfitMargin
		case fEarningsGrowth:
			v = m.EarningsGrowth
		case fRevenueGrowth:
			v = m.RevenueGrowth
		case fDebtEquity:
			v = m.DebtToEquity
		}
		if v == 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			out[i] = math.NaN()
		} else {
			out[i] = v
		}
	}
	return out
}

// crossSectionalZ z-scores a slice of values across the entries
// that are not NaN. Returns a slice of the same length as the
// input; NaN entries map to NaN in the output too (the caller
// is expected to fall back to 0 when blending). If fewer than
// minSample non-NaN values exist, returns an all-NaN slice
// (no meaningful comparison group). When stdev is zero, returns
// zero for every non-NaN entry (no spread in the universe → no
// information).
func crossSectionalZ(values []float64, minSample int) []float64 {
	out := make([]float64, len(values))
	var sum float64
	var n int
	for _, v := range values {
		if math.IsNaN(v) {
			continue
		}
		sum += v
		n++
	}
	if n < minSample {
		for i := range out {
			out[i] = math.NaN()
		}
		return out
	}
	mean := sum / float64(n)
	var sumSq float64
	for _, v := range values {
		if math.IsNaN(v) {
			continue
		}
		d := v - mean
		sumSq += d * d
	}
	// Sample variance (n-1 denominator) when n >= 2.
	var stdev float64
	if n >= 2 {
		stdev = math.Sqrt(sumSq / float64(n-1))
	}
	for i, v := range values {
		switch {
		case math.IsNaN(v):
			out[i] = math.NaN()
		case stdev <= 0:
			out[i] = 0
		default:
			out[i] = (v - mean) / stdev
		}
	}
	return out
}

// blend computes a weighted average of zs[] with weights[]
// where any zs[i] == NaN is treated as "not contributing" and
// the weight is redistributed across the available entries.
// Returns (0, false) when zero weights survive — the caller
// uses the bool to count whether a sub-factor fired.
func blend(zs, weights []float64) (float64, bool) {
	if len(zs) != len(weights) {
		return 0, false
	}
	var num, denom float64
	for i, z := range zs {
		w := weights[i]
		if w <= 0 || math.IsNaN(z) {
			continue
		}
		num += w * z
		denom += w
	}
	if denom <= 0 {
		return 0, false
	}
	return num / denom, true
}
