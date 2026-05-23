// Package ranking turns a fund's investable universe into a
// cross-sectional rank table the PM prompt can consume. Sprint A #2.
//
// The PM today evaluates each symbol in isolation: it gets a debate
// verdict, a regime tag, an ATR ceiling — but no way to ask "of the
// 12 names on my screen, which 3 look strongest relative to the rest
// today?" That single piece of relative information is the workhorse
// of every cross-sectional equity / crypto strategy at AQR, Two Sigma,
// Renaissance, etc. (Asness "Value and Momentum Everywhere"; Lopez de
// Prado "Advances in Financial Machine Learning" ch. 6 on
// cross-sectional features).
//
// Ranker turns raw bars into three primary features per symbol and
// then z-scores each feature across the universe so the LLM sees a
// fair scale-free comparison:
//
//   - MomentumZ   — 20-bar trailing total return, demeaned + scaled
//                   by the cross-sectional stdev. Positive Z means
//                   "outperformed peers over the last month".
//   - VolatilityZ — annualised stdev of daily log returns, z-scored.
//                   Positive Z means "more volatile than peers".
//   - LiquidityZ  — log of 10-bar mean dollar volume (close × volume),
//                   z-scored. Positive Z means "more liquid than
//                   peers" — relevant for crypto / small-cap names
//                   where order-impact dominates returns.
//
// CompositeZ is the weighted sum the PM can use as a single ranking
// number. Weights default to 0.5×momentum − 0.3×volatility + 0.2×
// liquidity — same shape AQR uses for the QMOM factor sleeve, just
// in fund-agnostic units.
//
// Quartile labels each surviving symbol 1..4 (1 = top quartile by
// CompositeZ) so the prompt can say "prefer Q1 names, avoid Q4
// names" without forcing the LLM to recompute the ranking.
//
// Symbols with insufficient bars are dropped — the universe sample
// has to be at least 3 entries for z-scoring to be meaningful;
// fewer-than-3 ranking tables just confuse the model.
//
// The same OHLC fetcher backs both quantsnapshot.Builder and Ranker;
// the cache layer in front of it makes back-to-back fetches free.
package ranking

import (
	"context"
	"math"
	"sort"
	"strings"

	"github.com/fundai/server/internal/ohlc"
)

// SymbolRanking is the per-symbol row the PM prompt consumes. Every
// numeric field is best-effort; symbols missing a feature get NaN
// which the prompt builder rounds to 0 and the LLM treats as
// "neutral".
type SymbolRanking struct {
	// Symbol is the same uppercased ticker shown everywhere else
	// in the prompt (positions, universe, quantSnapshots).
	Symbol string
	// MomentumZ is the trailing 20-bar return z-scored across the
	// universe. Positive = outperformed peers.
	MomentumZ float64
	// VolatilityZ is the 20-bar realised volatility z-scored.
	// Positive = noisier than peers.
	VolatilityZ float64
	// LiquidityZ is the log of mean dollar volume z-scored.
	// Positive = deeper book than peers.
	LiquidityZ float64
	// CompositeZ is a single ranking signal the PM can sort by.
	// Default weights (0.5 mom, -0.3 vol, 0.2 liq) tilt toward
	// momentum while penalising vol and rewarding liquidity.
	CompositeZ float64
	// Quartile is the cross-sectional bucket 1..4 where 1 is the
	// top quartile by CompositeZ. Zero for symbols with no
	// CompositeZ (insufficient bars across the whole universe).
	Quartile int
}

// SymbolRequest names a single (symbol, market) pair to rank. Same
// shape quantsnapshot uses so the wiring layer can reuse one
// request list.
type SymbolRequest struct {
	Symbol string
	Market string
}

// Options tunes the bar lookbacks and the composite weights. Zero-
// value Options yields the production defaults.
type Options struct {
	// MomentumBars is the trailing return window. 20 daily bars =
	// one trading month — long enough to filter out single-day
	// noise, short enough to capture the regime the PM is sizing
	// against today.
	MomentumBars int
	// VolBars is the realised-vol window. 20 = same month so
	// momentum and vol live on the same horizon.
	VolBars int
	// LiquidityBars is the dollar-volume averaging window. 10 =
	// two trading weeks; long enough to smooth single-day spikes
	// without lagging genuine liquidity shifts.
	LiquidityBars int
	// LookbackBars is the bar count we ask the fetcher for. Has
	// to clear max(MomentumBars, VolBars) + 5 so the first
	// computable return / vol value isn't at the edge.
	LookbackBars int
	// MomentumWeight, VolatilityWeight, LiquidityWeight feed the
	// CompositeZ. Volatility is negated inside the linear combo
	// so positive VolatilityWeight means "penalise vol".
	MomentumWeight   float64
	VolatilityWeight float64
	LiquidityWeight  float64
	// MinUniverse is the floor below which we don't produce a
	// ranking at all. Z-scoring a 2-symbol universe yields ±0.71
	// for every entry — useless for the prompt. 3 is the
	// theoretical minimum for sample stdev != 0 in the
	// degenerate case; we go with 3 as a hard floor.
	MinUniverse int
}

func (o Options) withDefaults() Options {
	if o.MomentumBars <= 0 {
		o.MomentumBars = 20
	}
	if o.VolBars <= 0 {
		o.VolBars = 20
	}
	if o.LiquidityBars <= 0 {
		o.LiquidityBars = 10
	}
	if o.LookbackBars <= 0 {
		o.LookbackBars = 60
	}
	if o.MomentumWeight == 0 && o.VolatilityWeight == 0 && o.LiquidityWeight == 0 {
		o.MomentumWeight = 0.5
		o.VolatilityWeight = 0.3
		o.LiquidityWeight = 0.2
	}
	if o.MinUniverse <= 0 {
		o.MinUniverse = 3
	}
	return o
}

// Ranker holds the wired OHLC fetcher and tuning knobs.
type Ranker struct {
	ohlc ohlc.Fetcher
	opts Options
}

// NewRanker wires a Ranker. nil fetcher is tolerated; BuildRanking
// returns nil in that case so the prompt skips the block.
func NewRanker(fetcher ohlc.Fetcher, opts Options) *Ranker {
	return &Ranker{ohlc: fetcher, opts: opts.withDefaults()}
}

// Options exposes the resolved tuning struct — useful for tests +
// observability.
func (r *Ranker) Options() Options {
	if r == nil {
		return Options{}.withDefaults()
	}
	return r.opts
}

// BuildRanking returns one SymbolRanking per symbol with enough
// data, sorted by descending CompositeZ. Symbols with too-short
// bar history are dropped. When fewer than MinUniverse symbols
// survive, BuildRanking returns nil so the prompt block is empty.
//
// One Fetch per unique (symbol, market). The OHLC cache makes
// repeat calls free across the runtime.
func (r *Ranker) BuildRanking(ctx context.Context, requests []SymbolRequest) []SymbolRanking {
	if r == nil || r.ohlc == nil {
		return nil
	}
	// Pull bars per dedup'd request.
	raw := make([]symbolFeatures, 0, len(requests))
	seen := make(map[string]struct{}, len(requests))
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
		bars, err := r.ohlc.Fetch(ctx, ohlc.FetchRequest{
			Symbol:    sym,
			Market:    mkt,
			Interval:  ohlc.IntervalDay,
			LookbackN: r.opts.LookbackBars,
		})
		if err != nil || len(bars) == 0 {
			continue
		}
		feat, ok := computeFeatures(sym, bars, r.opts)
		if !ok {
			continue
		}
		raw = append(raw, feat)
	}
	if len(raw) < r.opts.MinUniverse {
		return nil
	}
	zMomentum := zScore(extractField(raw, "momentum"))
	zVol := zScore(extractField(raw, "volatility"))
	zLiq := zScore(extractField(raw, "liquidity"))
	out := make([]SymbolRanking, len(raw))
	for i, feat := range raw {
		mz := zMomentum[i]
		vz := zVol[i]
		lz := zLiq[i]
		composite := r.opts.MomentumWeight*mz - r.opts.VolatilityWeight*vz + r.opts.LiquidityWeight*lz
		out[i] = SymbolRanking{
			Symbol:      feat.symbol,
			MomentumZ:   safeFloat(mz),
			VolatilityZ: safeFloat(vz),
			LiquidityZ:  safeFloat(lz),
			CompositeZ:  safeFloat(composite),
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CompositeZ > out[j].CompositeZ
	})
	assignQuartiles(out)
	return out
}

// symbolFeatures holds the three raw inputs before z-scoring.
type symbolFeatures struct {
	symbol     string
	momentum   float64
	volatility float64
	liquidity  float64
}

// computeFeatures returns the three raw features for a symbol or
// (zero, false) when the bar history is too short for any of them
// to be meaningful.
func computeFeatures(symbol string, bars []ohlc.Bar, opts Options) (symbolFeatures, bool) {
	if len(bars) < opts.MomentumBars+1 || len(bars) < opts.VolBars+1 || len(bars) < opts.LiquidityBars {
		return symbolFeatures{}, false
	}
	closes := make([]float64, len(bars))
	volumes := make([]float64, len(bars))
	for i, b := range bars {
		closes[i] = b.Close
		volumes[i] = b.Volume
	}
	// Momentum: trailing return over MomentumBars.
	startIdx := len(closes) - 1 - opts.MomentumBars
	if startIdx < 0 || closes[startIdx] <= 0 {
		return symbolFeatures{}, false
	}
	momentum := closes[len(closes)-1]/closes[startIdx] - 1
	// Volatility: stdev of daily log returns over VolBars.
	volStart := len(closes) - 1 - opts.VolBars
	if volStart < 0 {
		return symbolFeatures{}, false
	}
	logRets := make([]float64, 0, opts.VolBars)
	for i := volStart + 1; i < len(closes); i++ {
		if closes[i-1] <= 0 || closes[i] <= 0 {
			return symbolFeatures{}, false
		}
		logRets = append(logRets, math.Log(closes[i]/closes[i-1]))
	}
	volatility := stdev(logRets)
	// Liquidity: log mean dollar volume over LiquidityBars.
	liqStart := len(closes) - opts.LiquidityBars
	if liqStart < 0 {
		return symbolFeatures{}, false
	}
	var liqSum float64
	var liqCount int
	for i := liqStart; i < len(closes); i++ {
		dv := closes[i] * volumes[i]
		if dv > 0 {
			liqSum += dv
			liqCount++
		}
	}
	if liqCount == 0 {
		return symbolFeatures{}, false
	}
	liquidity := math.Log(liqSum / float64(liqCount))
	return symbolFeatures{
		symbol:     symbol,
		momentum:   momentum,
		volatility: volatility,
		liquidity:  liquidity,
	}, true
}

// extractField pulls a single field out of every symbolFeatures so
// zScore can normalise the column across the universe.
func extractField(rows []symbolFeatures, field string) []float64 {
	out := make([]float64, len(rows))
	for i, r := range rows {
		switch field {
		case "momentum":
			out[i] = r.momentum
		case "volatility":
			out[i] = r.volatility
		case "liquidity":
			out[i] = r.liquidity
		}
	}
	return out
}

// zScore computes (x - mean) / stdev. Returns zeros when the input
// is empty or all values are identical (stdev == 0); that's the
// neutral "everyone is equally interesting" reading rather than
// poisoning the universe with NaNs.
func zScore(xs []float64) []float64 {
	out := make([]float64, len(xs))
	if len(xs) == 0 {
		return out
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	std := stdev(xs)
	if std == 0 {
		return out
	}
	for i, x := range xs {
		out[i] = (x - mean) / std
	}
	return out
}

// stdev returns the population stdev (divide by N) so two-symbol
// universes still produce a real number rather than NaN. Sample
// stdev (N-1) would force the MinUniverse floor much higher.
func stdev(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	var sumSq float64
	for _, x := range xs {
		d := x - mean
		sumSq += d * d
	}
	return math.Sqrt(sumSq / float64(len(xs)))
}

// safeFloat replaces NaN / ±Inf with 0 so the prompt JSON encodes
// cleanly. NaN can leak in when a single zScore call returns zero
// for its entire column (handled inside zScore) and downstream
// arithmetic ends up dividing.
func safeFloat(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// assignQuartiles splits the SORTED-by-CompositeZ slice into four
// roughly equal buckets and stamps Quartile (1 = top, 4 = bottom)
// on each row. With small N the buckets aren't exact; we err on
// the side of "tail symbols land in Q4" by computing the cut points
// against ceil(N/4) chunks.
func assignQuartiles(rows []SymbolRanking) {
	n := len(rows)
	if n == 0 {
		return
	}
	// Ceiling division so the last bucket is always the smallest
	// when N % 4 != 0; the prompt cares more about cleanly
	// labelling Q1 than Q4 boundary edge cases.
	chunk := (n + 3) / 4
	for i := range rows {
		q := i/chunk + 1
		if q > 4 {
			q = 4
		}
		rows[i].Quartile = q
	}
}
