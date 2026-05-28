// Package lowbeta computes per-symbol Frazzini-Pedersen "Betting
// Against Beta" (BAB) overlay scores for the PM prompt.
//
// Sprint F #2. The classical BAB strategy (2014) shorts the
// highest-beta names and longs the lowest-beta names, levering
// the long side to beta-neutrality — Frazzini & Pedersen showed
// this strategy delivers risk-adjusted returns ~0.78 Sharpe per
// annum across US / EU / EM equity universes. The intuition:
// constrained investors (mutual funds with leverage caps,
// pension funds with risk targets) bid up high-beta names
// because they're the cheapest way to get exposure, leaving
// low-beta names systematically under-priced.
//
// For a LONG-ONLY LLM PM (no shorting infrastructure), we
// surface BAB as a DEFENSIVE TILT score rather than the full
// long-short construct:
//
//   - BetaZ:       z-scored realized beta vs market index
//                  (NEGATED so high BetaZ = low beta = preferred)
//   - VolatilityZ: z-scored realized daily-return stdev
//                  (NEGATED so high VolatilityZ = low vol = preferred)
//   - CompositeZ:  weighted blend (default 0.6 beta / 0.4 vol).
//                  High CompositeZ = defensive name; low = aggressive.
//   - Quartile:    1 = most defensive 25%, 4 = most aggressive.
//
// The system prompt teaches the PM to:
//
//   - Tilt toward Q1 BAB names when fund is in drawdown throttle
//     (Sprint B #2 risk-budget block reports drawdown_throttle).
//   - Tilt toward Q4 BAB names in confirmed trend_up regimes
//     where the higher-beta name will outrun the universe.
//   - Treat the Q1 ∩ Q1 (QualityScores ∩ BAB) intersection as
//     the canonical "low-risk anomaly" trade (Asness 2019
//     "Quality Investing").
//
// Beta math:
//
//     r_i(t)      = log(close_i(t) / close_i(t-1))   // per stock
//     r_m(t)      = log(close_m(t) / close_m(t-1))   // market index
//     beta_i      = cov(r_i, r_m) / var(r_m)
//     stdev_i     = sqrt(var(r_i))
//
// Lookback default = 60 daily bars (matches correlation /
// pairspread defaults so the OHLC cache is hot for all three
// services).
//
// Market index resolution (default):
//
//     us_equity  → SPY  (S&P 500 ETF)
//     a_share    → 510300.SS  (Huatai-PB CSI 300 ETF)
//     hk_equity  → 2800.HK  (Tracker Fund of Hong Kong)
//     crypto     → "" (skipped — beta vs market index undefined)
//
// Operators can override via Options.MarketIndexBySymbol — useful
// for sector-specialist funds that want to benchmark against a
// sector ETF (e.g. XLK for a tech-only US fund).
//
// Architecture mirrors quality / value:
//   - SymbolRequest names a (symbol, market) pair.
//   - Service holds the ohlc.Fetcher + opts.
//   - BuildScores returns sorted []Score (high CompositeZ first).
package lowbeta

import (
	"context"
	"math"
	"sort"
	"strings"

	"github.com/fundai/server/internal/ohlc"
)

// Score is one symbol's BAB reading. All z-scores are
// cross-sectional within the calling universe AND negated so
// that HIGH = defensive (low beta / low vol). Quartile 1 is the
// most defensive 25%; 4 is the most aggressive.
type Score struct {
	// Symbol is the upper-cased ticker.
	Symbol string
	// Beta is the raw realized beta vs the resolved market
	// index over the lookback window. Carried unscaled (NOT a
	// z-score) so the prompt can show "beta=1.42" alongside
	// the z-score; ergonomic for the PM's reasoning.
	Beta float64
	// Volatility is the raw realized daily-return stdev,
	// expressed in % per day (e.g. 0.018 = 1.8%/day). Same
	// rationale as Beta — carried raw for prompt readability.
	Volatility float64
	// BetaZ is the NEGATED cross-sectional z-score of Beta.
	// High BetaZ = low beta = preferred under BAB. The
	// negation is baked into the score (not the composite
	// weight) so the system prompt can read "betaZ=+1.2 →
	// defensive" without having to remember the sign.
	BetaZ float64
	// VolatilityZ is the NEGATED cross-sectional z-score of
	// Volatility. Same convention.
	VolatilityZ float64
	// CompositeZ is the weighted blend (default 0.6 beta /
	// 0.4 vol). HIGH = defensive.
	CompositeZ float64
	// Quartile is 1..4 by CompositeZ. 1 = most defensive,
	// 4 = most aggressive. Zero when universe < 4.
	Quartile int
	// ComponentsAvailable is 1 when only one of Beta / Vol
	// could be computed (e.g. market-index bars missing →
	// no beta, only volatility). 2 when both fired.
	ComponentsAvailable int
}

// SymbolRequest names a (symbol, market) pair to score. Same
// shape as quality / value.
type SymbolRequest struct {
	Symbol string
	Market string
}

// Options tunes the composite weights, the lookback window, and
// the market-index resolver. Zero-value yields production
// defaults.
type Options struct {
	// LookbackBars is the daily-bar window over which beta +
	// volatility are computed. Default 60 (≈ 3 trading months
	// — matches the AQR / Robeco low-vol papers and the
	// existing correlation / pairspread defaults so the OHLC
	// cache is hot for the third service in the chain).
	LookbackBars int

	// BetaWeight + VolatilityWeight set the composite blend.
	// Default 0.6 / 0.4 — beta is the primary BAB lever, but
	// realized vol catches names with stale beta (just IPO'd,
	// just split). Set either to 0 to use the other alone.
	BetaWeight       float64
	VolatilityWeight float64

	// MinUniverse floors the universe size required to compute
	// the cross-sectional z-scores. Default 3.
	MinUniverse int

	// MarketIndexBySymbol overrides the per-market default
	// index symbol. Keys are lower-cased market tags
	// ("us_equity", "a_share", "hk_equity", ...). When a
	// fund's market isn't in the map and isn't a built-in
	// default, every symbol in that market gets BetaZ=NaN
	// (the composite falls back on VolatilityZ alone).
	MarketIndexBySymbol map[string]string
}

func (o Options) withDefaults() Options {
	if o.LookbackBars <= 0 {
		o.LookbackBars = 60
	}
	if o.BetaWeight == 0 && o.VolatilityWeight == 0 {
		o.BetaWeight = 0.6
		o.VolatilityWeight = 0.4
	}
	if o.MinUniverse <= 0 {
		o.MinUniverse = 3
	}
	return o
}

// MarketIndexFor resolves the market index symbol the BAB
// service uses to compute realized beta for `market`. Operator
// overrides win; built-in defaults cover US / A-share / HK.
// Returns "" when no index is known — the caller treats every
// per-symbol beta in that market as missing.
func (o Options) MarketIndexFor(market string) string {
	m := strings.ToLower(strings.TrimSpace(market))
	if m == "" {
		m = "us_equity"
	}
	if o.MarketIndexBySymbol != nil {
		if v, ok := o.MarketIndexBySymbol[m]; ok {
			return v
		}
	}
	switch m {
	case "us_equity", "us":
		return "SPY"
	case "a_share", "cn":
		// Huatai-PB CSI 300 ETF (510300.SS) — the most
		// liquid A-share market benchmark and our OHLC
		// providers all resolve it cleanly.
		return "510300.SS"
	case "hk_equity", "hk":
		// Tracker Fund of Hong Kong (2800.HK) tracks Hang Seng.
		return "2800.HK"
	default:
		// crypto / unknown — skip. Beta vs a market index
		// isn't well-defined here.
		return ""
	}
}

// Service holds the ohlc.Fetcher + resolved options. Safe for
// concurrent use; state is immutable post-construction.
type Service struct {
	ohlc ohlc.Fetcher
	opts Options
}

// NewService wires the Service. nil fetcher is tolerated;
// BuildScores returns nil in that case.
func NewService(fetcher ohlc.Fetcher, opts Options) *Service {
	return &Service{
		ohlc: fetcher,
		opts: opts.withDefaults(),
	}
}

// Options exposes the resolved tuning struct (tests + introspection).
func (s *Service) Options() Options {
	if s == nil {
		return Options{}.withDefaults()
	}
	return s.opts
}

// BuildScores fetches per-symbol bars + the market-index bars
// for the fund's market, computes (beta, volatility), then
// cross-sectionally z-scores both and produces the composite.
// Returns nil when:
//   - service is nil / no OHLC fetcher
//   - fewer than MinUniverse symbols produced usable bars
//   - every symbol's beta + volatility failed (e.g. market
//     index unresolvable AND every symbol had < LookbackBars+1
//     daily bars)
func (s *Service) BuildScores(ctx context.Context, requests []SymbolRequest) []Score {
	if s == nil || s.ohlc == nil {
		return nil
	}
	// Pass 1: dedup symbols + resolve the market-index per
	// distinct market. We resolve PER request market (a fund
	// could in principle hold cross-market positions) but
	// expect 99% of calls to converge on one market.
	bySymbol := s.fetchPerSymbolReturns(ctx, requests)
	if len(bySymbol) < s.opts.MinUniverse {
		return nil
	}
	// Market-index returns per market.
	byMarketReturns := s.fetchMarketReturns(ctx, requests)

	rawBetas := make([]float64, 0, len(bySymbol))
	rawVols := make([]float64, 0, len(bySymbol))
	symbols := make([]string, 0, len(bySymbol))
	markets := make([]string, 0, len(bySymbol))

	// Two-pass: first collect raw, then z-score.
	for _, entry := range bySymbol {
		idxReturns, ok := byMarketReturns[entry.market]
		var beta float64
		if ok && len(idxReturns) > 1 && len(entry.returns) > 1 {
			beta = realizedBeta(entry.returns, idxReturns)
		} else {
			beta = math.NaN()
		}
		var vol float64
		if len(entry.returns) > 1 {
			vol = realizedStdev(entry.returns)
		} else {
			vol = math.NaN()
		}
		rawBetas = append(rawBetas, beta)
		rawVols = append(rawVols, vol)
		symbols = append(symbols, entry.symbol)
		markets = append(markets, entry.market)
	}

	// Negated z-scores: HIGH = defensive.
	zBeta := negatedZ(rawBetas, s.opts.MinUniverse)
	zVol := negatedZ(rawVols, s.opts.MinUniverse)

	scores := make([]Score, 0, len(symbols))
	for i, sym := range symbols {
		availability := 0
		if !math.IsNaN(zBeta[i]) {
			availability++
		}
		if !math.IsNaN(zVol[i]) {
			availability++
		}
		if availability == 0 {
			continue
		}
		composite, _ := blend(
			[]float64{zBeta[i], zVol[i]},
			[]float64{s.opts.BetaWeight, s.opts.VolatilityWeight},
		)
		_ = markets // reserved for future per-market breakdown
		scores = append(scores, Score{
			Symbol:              sym,
			Beta:                nanToZero(rawBetas[i]),
			Volatility:          nanToZero(rawVols[i]),
			BetaZ:               nanToZero(zBeta[i]),
			VolatilityZ:         nanToZero(zVol[i]),
			CompositeZ:          composite,
			ComponentsAvailable: availability,
		})
	}
	if len(scores) == 0 {
		return nil
	}
	// Descending by CompositeZ → most defensive first.
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

type symbolReturnsEntry struct {
	symbol  string
	market  string
	returns []float64
}

// fetchPerSymbolReturns dedups the requests, fires one OHLC
// fetch per (symbol, market), and computes daily log-returns.
// Symbols that fail OR return fewer than 2 usable closes are
// dropped. Output is keyed by upper-cased symbol so the caller
// gets a stable iteration order via subsequent slice rebuild.
func (s *Service) fetchPerSymbolReturns(ctx context.Context, requests []SymbolRequest) map[string]symbolReturnsEntry {
	seen := make(map[string]struct{}, len(requests))
	out := make(map[string]symbolReturnsEntry, len(requests))
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
		closes := s.fetchCloses(ctx, sym, mkt)
		if len(closes) < 2 {
			continue
		}
		out[sym] = symbolReturnsEntry{
			symbol:  sym,
			market:  mkt,
			returns: logReturns(closes),
		}
	}
	return out
}

// fetchMarketReturns resolves the market-index symbol per
// distinct market in the request list and pulls its bars
// exactly once. Returns a map keyed by lower-cased market tag.
// A market with no resolvable index symbol is skipped (no
// entry in the map → caller falls back to vol-only composite).
func (s *Service) fetchMarketReturns(ctx context.Context, requests []SymbolRequest) map[string][]float64 {
	markets := make(map[string]struct{})
	for _, req := range requests {
		markets[strings.ToLower(strings.TrimSpace(req.Market))] = struct{}{}
	}
	out := make(map[string][]float64, len(markets))
	for mkt := range markets {
		idxSym := s.opts.MarketIndexFor(mkt)
		if idxSym == "" {
			continue
		}
		closes := s.fetchCloses(ctx, idxSym, mkt)
		if len(closes) < 2 {
			continue
		}
		out[mkt] = logReturns(closes)
	}
	return out
}

// fetchCloses pulls the trailing LookbackBars+5 daily closes for
// (symbol, market). The +5 padding mirrors pairspread / correlation
// — providers sometimes return fewer bars than asked, the
// padding keeps the lookback rolling on partial outages.
func (s *Service) fetchCloses(ctx context.Context, symbol, market string) []float64 {
	bars, err := s.ohlc.Fetch(ctx, ohlc.FetchRequest{
		Symbol:    symbol,
		Market:    market,
		Interval:  ohlc.IntervalDay,
		LookbackN: s.opts.LookbackBars + 5,
	})
	if err != nil || len(bars) == 0 {
		return nil
	}
	out := make([]float64, 0, len(bars))
	for _, b := range bars {
		if b.Close > 0 && !math.IsNaN(b.Close) && !math.IsInf(b.Close, 0) {
			out = append(out, b.Close)
		}
	}
	return out
}

// logReturns computes per-bar log(close_t / close_{t-1}).
// Output length is len(closes) - 1.
func logReturns(closes []float64) []float64 {
	if len(closes) < 2 {
		return nil
	}
	out := make([]float64, len(closes)-1)
	for i := 1; i < len(closes); i++ {
		if closes[i-1] <= 0 || closes[i] <= 0 {
			out[i-1] = 0
			continue
		}
		out[i-1] = math.Log(closes[i] / closes[i-1])
	}
	return out
}

// realizedBeta computes cov(stock, market) / var(market). Uses
// the OVERLAPPING tail of the two series — if one is shorter
// we truncate from the FRONT so the most recent observations
// stay aligned. Returns NaN when either series is < 2 elements
// or the market variance is zero.
func realizedBeta(stock, market []float64) float64 {
	n := len(stock)
	if len(market) < n {
		n = len(market)
	}
	if n < 2 {
		return math.NaN()
	}
	s := stock[len(stock)-n:]
	m := market[len(market)-n:]
	var sumS, sumM float64
	for i := 0; i < n; i++ {
		sumS += s[i]
		sumM += m[i]
	}
	meanS := sumS / float64(n)
	meanM := sumM / float64(n)
	var cov, varM float64
	for i := 0; i < n; i++ {
		ds := s[i] - meanS
		dm := m[i] - meanM
		cov += ds * dm
		varM += dm * dm
	}
	if varM <= 0 {
		return math.NaN()
	}
	return cov / varM
}

// realizedStdev computes the sample stdev of the returns. NaN
// when fewer than 2 entries.
func realizedStdev(returns []float64) float64 {
	if len(returns) < 2 {
		return math.NaN()
	}
	var sum float64
	for _, r := range returns {
		sum += r
	}
	mean := sum / float64(len(returns))
	var sumSq float64
	for _, r := range returns {
		d := r - mean
		sumSq += d * d
	}
	return math.Sqrt(sumSq / float64(len(returns)-1))
}

// negatedZ z-scores the input slice across non-NaN entries,
// then NEGATES every result so HIGH = LOW raw value (= defensive
// for both beta and volatility). Returns all-NaN when fewer
// than minSample valid entries.
func negatedZ(values []float64, minSample int) []float64 {
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
			// Negate so high z = low raw value.
			out[i] = -(v - mean) / stdev
		}
	}
	return out
}

// blend mirrors quality/value blend: redistribute weight across
// non-NaN sub-factors. (0,false) when no usable input.
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
