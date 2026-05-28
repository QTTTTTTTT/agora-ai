// Package value computes per-symbol cross-sectional value-factor
// z-scores from existing fundamental.Metrics inputs.
//
// Sprint F #1. Sister package to `quality`. Quality answers "is
// this a good company?"; value answers "is it cheap enough?". The
// classic AQR / Fama-French Value composite blends three
// price-relative ratios that all point in the same direction
// (high = cheap):
//
//   - Book-to-Price (B/P)        = 1 / PB
//   - Earnings-to-Price (E/P)    = 1 / PE     (loss-makers excluded)
//   - Dividend Yield (D/P)       = DividendYield
//
// Each ratio is z-scored cross-sectionally across the universe,
// then composited with weights (default 0.45 / 0.45 / 0.10 —
// dividend yield is the weakest forward-return predictor in the
// QMJ + Asness Devil-in-the-HML literature; we keep it for cash
// optionality but discount it).
//
// Composite = 0.45 B/P + 0.45 E/P + 0.10 D/P, then quartile-bucketed
// (1 = top, 4 = bottom). Operators can retune per fund via Options.
//
// Architecture mirrors quality exactly:
//   - SymbolRequest names a (symbol, market) pair.
//   - Options carries weights and floors.
//   - Service holds the fundamental.Fetcher + opts.
//   - BuildScores returns one Score per symbol with enough data,
//     sorted descending by CompositeZ.
//
// Why use B/P (inverse PB) and not PB directly: we want HIGH
// composite = CHEAP, so the sub-factors must all point the same
// way before z-scoring. Inverting moves "expensive PB=10" to
// B/P=0.1 and "cheap PB=0.5" to B/P=2.0, putting the cheap names
// in the right tail of the z-score distribution where the system
// prompt rules expect them.
//
// Loss-making companies (PE <= 0): E/P is meaningless (a negative
// E/P would imply "value" of a money-loser, which is the opposite
// of what we want). We treat E/P=NaN for those names; the
// composite still computes from B/P + D/P alone.
//
// Outlier clipping: raw 1/PE and 1/PB can blow up for tiny
// denominators (PE=0.01 → E/P=100). We winsorise at ±3 sigma
// AFTER z-scoring, which keeps the universe mean / stdev sane.
package value

import (
	"context"
	"math"
	"sort"
	"strings"

	"github.com/fundai/server/internal/fundamental"
)

// Score is one symbol's value factor reading. All z-scores are
// cross-sectional within the calling universe — a +1 means "one
// standard deviation cheaper than the universe mean" for that
// sub-factor.
type Score struct {
	// Symbol is the upper-cased ticker.
	Symbol string
	// BookToPriceZ is the z-score of 1/PB (book/price). Higher
	// = cheaper per dollar of book equity.
	BookToPriceZ float64
	// EarningsToPriceZ is the z-score of 1/PE (earnings/price).
	// Higher = cheaper per dollar of trailing earnings. NaN
	// for loss-makers (excluded from the comparison group).
	EarningsToPriceZ float64
	// DividendYieldZ is the z-score of trailing DividendYield.
	// Higher = more cash returned per dollar invested. Zero
	// for non-dividend payers (treated as "0 yield" rather
	// than NaN because not paying a dividend is a real
	// economic decision, not missing data).
	DividendYieldZ float64
	// CompositeZ is the weighted blend (default 0.45 / 0.45 /
	// 0.10) the PM should sort by. Higher = better-value.
	CompositeZ float64
	// Quartile is the bucket 1..4 by CompositeZ. 1 = top
	// quartile (cheapest), 4 = bottom (most expensive).
	// Zero when the universe is < 4 symbols.
	Quartile int
	// ComponentsAvailable counts how many sub-factors fired.
	// 1 or 2 components is a "value" call built on partial
	// data — the prompt teaches the PM to discount it.
	ComponentsAvailable int
}

// SymbolRequest names a (symbol, market) pair. Same shape as
// quality.SymbolRequest so the wiring layer can hand both
// services the same request slice.
type SymbolRequest struct {
	Symbol string
	Market string
}

// Options tunes the sub-factor and composite weights, plus the
// per-universe floors. Zero-value Options yields production
// defaults.
type Options struct {
	// Sub-factor / composite weights. Default 0.45 / 0.45 / 0.10.
	// Operators wanting "pure Fama-French HML" can push
	// BookToPriceWeight to 1.0 and zero the others.
	BookToPriceWeight     float64
	EarningsToPriceWeight float64
	DividendYieldWeight   float64

	// MinUniverse is the floor below which we skip scoring.
	// 3 is the theoretical minimum for a non-degenerate
	// sample stdev (matches quality.Options).
	MinUniverse int

	// PerFactorMin is the floor below which a specific
	// sub-factor (e.g. dividend yield) is z-scored. When fewer
	// than PerFactorMin symbols report it every entry's z-score
	// is 0. Defaults to MinUniverse.
	PerFactorMin int

	// WinsorSigma clips |z| at this multiple of stdev so a
	// single tiny-PE name doesn't dominate the composite. 0
	// disables clipping; default 3 (AQR-standard).
	WinsorSigma float64
}

func (o Options) withDefaults() Options {
	if o.BookToPriceWeight == 0 && o.EarningsToPriceWeight == 0 && o.DividendYieldWeight == 0 {
		o.BookToPriceWeight = 0.45
		o.EarningsToPriceWeight = 0.45
		o.DividendYieldWeight = 0.10
	}
	if o.MinUniverse <= 0 {
		o.MinUniverse = 3
	}
	if o.PerFactorMin <= 0 {
		o.PerFactorMin = o.MinUniverse
	}
	if o.WinsorSigma == 0 {
		o.WinsorSigma = 3
	}
	return o
}

// Service holds the fundamental.Fetcher + resolved options.
// Safe for concurrent use (state is immutable post-construction).
type Service struct {
	fetcher fundamental.Fetcher
	opts    Options
}

// NewService wires the Service. nil fetcher is tolerated (matches
// quality / earnings); BuildScores returns nil in that case.
func NewService(fetcher fundamental.Fetcher, opts Options) *Service {
	return &Service{
		fetcher: fetcher,
		opts:    opts.withDefaults(),
	}
}

// Options exposes the resolved tuning struct (tests + introspection).
func (s *Service) Options() Options {
	if s == nil {
		return Options{}.withDefaults()
	}
	return s.opts
}

// BuildScores returns the cross-sectional Score per symbol with
// at least one usable sub-factor, sorted descending by
// CompositeZ. nil when fewer than MinUniverse symbols produce
// any usable data.
func (s *Service) BuildScores(ctx context.Context, requests []SymbolRequest) []Score {
	if s == nil || s.fetcher == nil {
		return nil
	}
	raw := s.gatherMetrics(ctx, requests)
	if len(raw) < s.opts.MinUniverse {
		return nil
	}
	// Build the three price-relative ratios. All inverted so
	// HIGH = CHEAP. NaN for "not reported" or loss-making (E/P).
	rawBP := bookToPrice(raw)
	rawEP := earningsToPrice(raw)
	rawDY := dividendYield(raw)

	zBP := winsorisedZ(rawBP, s.opts.PerFactorMin, s.opts.WinsorSigma)
	zEP := winsorisedZ(rawEP, s.opts.PerFactorMin, s.opts.WinsorSigma)
	zDY := winsorisedZ(rawDY, s.opts.PerFactorMin, s.opts.WinsorSigma)

	scores := make([]Score, 0, len(raw))
	for i, m := range raw {
		availability := 0
		if !math.IsNaN(zBP[i]) {
			availability++
		}
		if !math.IsNaN(zEP[i]) {
			availability++
		}
		if !math.IsNaN(zDY[i]) {
			availability++
		}
		if availability == 0 {
			// Symbol has NO usable price-relative — drop. Keeps
			// the quartile distribution clean.
			continue
		}
		composite, _ := blend(
			[]float64{zBP[i], zEP[i], zDY[i]},
			[]float64{
				s.opts.BookToPriceWeight,
				s.opts.EarningsToPriceWeight,
				s.opts.DividendYieldWeight,
			},
		)
		scores = append(scores, Score{
			Symbol:              m.Symbol,
			BookToPriceZ:        nanToZero(zBP[i]),
			EarningsToPriceZ:    nanToZero(zEP[i]),
			DividendYieldZ:      nanToZero(zDY[i]),
			CompositeZ:          composite,
			ComponentsAvailable: availability,
		})
	}
	if len(scores) == 0 {
		return nil
	}
	sort.Slice(scores, func(i, j int) bool {
		if scores[i].CompositeZ != scores[j].CompositeZ {
			return scores[i].CompositeZ > scores[j].CompositeZ
		}
		return scores[i].Symbol < scores[j].Symbol
	})
	if len(scores) >= 4 {
		for i := range scores {
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
		m.Symbol = sym
		out = append(out, *m)
	}
	return out
}

// bookToPrice returns 1/PB per symbol. PB <= 0 (rare; usually
// only in distressed banks) treated as NaN.
func bookToPrice(rows []fundamental.Metrics) []float64 {
	out := make([]float64, len(rows))
	for i, m := range rows {
		if m.PB <= 0 || math.IsNaN(m.PB) || math.IsInf(m.PB, 0) {
			out[i] = math.NaN()
			continue
		}
		out[i] = 1 / m.PB
	}
	return out
}

// earningsToPrice returns 1/PE per symbol. Loss-makers (PE <= 0)
// excluded — a negative E/P would point in the wrong direction
// for the composite, and "money-loser" is a value flag the
// quality factor already handles.
//
// We prefer trailing PE; ForwardPE is more forward-looking but
// uses analyst estimates that drift. Operators wanting forward
// can read it from the prompt's fundamentalSummary block.
func earningsToPrice(rows []fundamental.Metrics) []float64 {
	out := make([]float64, len(rows))
	for i, m := range rows {
		if m.PE <= 0 || math.IsNaN(m.PE) || math.IsInf(m.PE, 0) {
			out[i] = math.NaN()
			continue
		}
		out[i] = 1 / m.PE
	}
	return out
}

// dividendYield returns the trailing yield per symbol. Zero
// (non-payer) is REAL DATA — kept as 0, not NaN — because the
// composite's "no yield is no problem" stance is exactly the
// signal: high-growth tech with 0% yield should still score on
// B/P + E/P.
func dividendYield(rows []fundamental.Metrics) []float64 {
	out := make([]float64, len(rows))
	for i, m := range rows {
		v := m.DividendYield
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			out[i] = math.NaN()
			continue
		}
		out[i] = v
	}
	return out
}

// winsorisedZ z-scores a slice across the non-NaN entries and
// clips |z| at ±sigma. Mirrors quality.crossSectionalZ but with
// the Winsor clip on top — value ratios have fatter tails than
// quality fundamentals (a single 0.01 PE blows up the universe
// stdev otherwise).
func winsorisedZ(values []float64, minSample int, winsor float64) []float64 {
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
			z := (v - mean) / stdev
			if winsor > 0 {
				if z > winsor {
					z = winsor
				} else if z < -winsor {
					z = -winsor
				}
			}
			out[i] = z
		}
	}
	return out
}

// blend mirrors quality.blend: redistribute weights across the
// non-NaN sub-factors so a symbol with partial data still gets
// a meaningful composite.
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

func nanToZero(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}
