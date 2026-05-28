package factorlab

import (
	"math"
	"sort"
	"strings"
	"time"
)

// Strategy is the per-factor decision rule. Each strategy is a
// PURE function (Fixture, asOf) → target weights so the
// simulator can score N strategies side-by-side on the same
// fixture in one pass.
//
// Contract:
//
//   - asOf is the bar BOUNDARY: the strategy may inspect bars
//     with date <= asOf (no look-ahead). The simulator passes
//     asOf == the close of the previous trading day so positions
//     entered at "today's close" face the realised next-day
//     return cleanly.
//   - The returned map is symbol → target weight in [0, 1]. The
//     simulator will normalise so the total weight ≤ 1 (cash =
//     1 - sum_weights). Symbols absent from the map are flat.
//   - Returning nil is "stand pat" (no rebalance trigger). The
//     simulator carries forward the previous day's weights.
//   - Returning an empty (non-nil) map flattens the book to cash.
//
// Long-only by design: this is an MVP and the live system is
// long-only too. Short legs require borrow-cost modelling we
// don't carry yet.
type Strategy interface {
	Name() string
	Weights(f *Fixture, asOf time.Time) map[string]float64
}

// ---------------------------------------------------------------------------
// equalWeightLong — the buy-and-hold benchmark every factor
// strategy must beat to claim alpha. Holds the entire fixture
// universe in equal weight from day 1, never rebalances after
// the opening tick.
// ---------------------------------------------------------------------------

type EqualWeightLong struct{}

func (EqualWeightLong) Name() string { return "equal_weight_long" }

func (EqualWeightLong) Weights(f *Fixture, asOf time.Time) map[string]float64 {
	if f == nil || len(f.Histories) == 0 {
		return nil
	}
	// We could be clever and only emit the map on day 0; the
	// simulator handles repeated identical weights as a no-op so
	// emitting daily is fine and keeps the implementation
	// trivial.
	w := 1.0 / float64(len(f.Histories))
	out := make(map[string]float64, len(f.Histories))
	for _, h := range f.Histories {
		out[strings.ToUpper(strings.TrimSpace(h.Symbol))] = w
	}
	return out
}

// ---------------------------------------------------------------------------
// momentum_12_1m — the Jegadeesh-Titman 1993 classic. Ranks the
// universe by the trailing 12-month return EXCLUDING the most
// recent month (the "1m skip" is what makes it momentum and not
// reversal). Long the top quartile, equal-weighted; flat
// otherwise.
//
// This is the MVP equivalent of the universeRanking block's
// dominant signal and the tsmom sleeve (Sprint E #1). Used as
// the headline factor in the IS Sharpe attribution table.
// ---------------------------------------------------------------------------

type Momentum12_1M struct {
	// TopQuartileFraction overrides the default 0.25. Use a
	// smaller fraction (e.g. 0.20) to concentrate; larger to
	// diversify. Defaults to 0.25 (Asness-style quartile).
	TopQuartileFraction float64
	// LookbackDays is the 12-month window (≈ 252 trading days).
	// SkipDays is the recent-month exclusion (≈ 21).
	LookbackDays int
	SkipDays     int
}

func (m Momentum12_1M) Name() string { return "momentum_12_1m" }

func (m Momentum12_1M) Weights(f *Fixture, asOf time.Time) map[string]float64 {
	if f == nil || len(f.Histories) == 0 {
		return nil
	}
	lookback := m.LookbackDays
	if lookback <= 0 {
		lookback = 252
	}
	skip := m.SkipDays
	if skip <= 0 {
		skip = 21
	}
	frac := m.TopQuartileFraction
	if frac <= 0 || frac > 1 {
		frac = 0.25
	}
	type ranked struct {
		symbol string
		ret    float64
	}
	rows := make([]ranked, 0, len(f.Histories))
	for _, h := range f.Histories {
		// Window: [asOf - lookback days, asOf - skip days]
		end := asOf.AddDate(0, 0, -skip)
		start := asOf.AddDate(0, 0, -lookback)
		closes := f.CloseSeries(h.Symbol, start, end)
		if len(closes) < 30 {
			continue
		}
		ret := closes[len(closes)-1]/closes[0] - 1.0
		rows = append(rows, ranked{symbol: strings.ToUpper(strings.TrimSpace(h.Symbol)), ret: ret})
	}
	if len(rows) == 0 {
		return nil
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ret > rows[j].ret })
	cut := int(math.Ceil(float64(len(rows)) * frac))
	if cut < 1 {
		cut = 1
	}
	winners := rows[:cut]
	w := 1.0 / float64(len(winners))
	out := make(map[string]float64, len(winners))
	for _, r := range winners {
		out[r.symbol] = w
	}
	return out
}

// ---------------------------------------------------------------------------
// low_beta — Frazzini-Pedersen 2014 BAB long leg. Computes each
// symbol's realised beta against the fixture benchmark over a
// lookback window, ranks ascending, and equal-weights the bottom
// quartile (lowest beta). Mirrors the lowBetaScores block.
// ---------------------------------------------------------------------------

type LowBeta struct {
	// LookbackDays controls the beta estimation window.
	// Defaults to 60 (matches the production lowbeta.Service
	// default).
	LookbackDays int
	// BottomQuartileFraction controls how concentrated the
	// defensive bucket is. Defaults to 0.25.
	BottomQuartileFraction float64
}

func (l LowBeta) Name() string { return "low_beta" }

func (l LowBeta) Weights(f *Fixture, asOf time.Time) map[string]float64 {
	if f == nil || f.Benchmark == nil || len(f.Histories) == 0 {
		return nil
	}
	lookback := l.LookbackDays
	if lookback <= 0 {
		lookback = 60
	}
	frac := l.BottomQuartileFraction
	if frac <= 0 || frac > 1 {
		frac = 0.25
	}
	mktCloses := f.Benchmark
	if mktCloses == nil {
		return nil
	}
	start := asOf.AddDate(0, 0, -2*lookback)
	mktSeries := closeSeriesFromHistory(mktCloses, start, asOf)
	mktR := LogReturns(mktSeries)
	if len(mktR) < lookback {
		return nil
	}
	type ranked struct {
		symbol string
		beta   float64
	}
	rows := make([]ranked, 0, len(f.Histories))
	for _, h := range f.Histories {
		sym := strings.ToUpper(strings.TrimSpace(h.Symbol))
		symCloses := f.CloseSeries(h.Symbol, start, asOf)
		symR := LogReturns(symCloses)
		if len(symR) < lookback {
			continue
		}
		beta := realizedBeta(symR, mktR)
		if math.IsNaN(beta) || math.IsInf(beta, 0) {
			continue
		}
		rows = append(rows, ranked{symbol: sym, beta: beta})
	}
	if len(rows) == 0 {
		return nil
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].beta < rows[j].beta })
	cut := int(math.Ceil(float64(len(rows)) * frac))
	if cut < 1 {
		cut = 1
	}
	winners := rows[:cut]
	w := 1.0 / float64(len(winners))
	out := make(map[string]float64, len(winners))
	for _, r := range winners {
		out[r.symbol] = w
	}
	return out
}

// ---------------------------------------------------------------------------
// low_vol — the "B" half of BAB (volatility-only). Ranks the
// universe by realised vol over the lookback window and equal-
// weights the bottom quartile. Useful as a sanity check on
// LowBeta: when the two strategies pick the same names, the
// signal is robust; when they diverge, the benchmark choice
// (or beta estimation noise) matters.
// ---------------------------------------------------------------------------

type LowVol struct {
	LookbackDays           int
	BottomQuartileFraction float64
}

func (l LowVol) Name() string { return "low_vol" }

func (l LowVol) Weights(f *Fixture, asOf time.Time) map[string]float64 {
	if f == nil || len(f.Histories) == 0 {
		return nil
	}
	lookback := l.LookbackDays
	if lookback <= 0 {
		lookback = 60
	}
	frac := l.BottomQuartileFraction
	if frac <= 0 || frac > 1 {
		frac = 0.25
	}
	type ranked struct {
		symbol string
		vol    float64
	}
	rows := make([]ranked, 0, len(f.Histories))
	start := asOf.AddDate(0, 0, -2*lookback)
	for _, h := range f.Histories {
		sym := strings.ToUpper(strings.TrimSpace(h.Symbol))
		closes := f.CloseSeries(h.Symbol, start, asOf)
		r := LogReturns(closes)
		if len(r) < lookback {
			continue
		}
		vol := realizedStdev(r)
		if math.IsNaN(vol) || math.IsInf(vol, 0) {
			continue
		}
		rows = append(rows, ranked{symbol: sym, vol: vol})
	}
	if len(rows) == 0 {
		return nil
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].vol < rows[j].vol })
	cut := int(math.Ceil(float64(len(rows)) * frac))
	if cut < 1 {
		cut = 1
	}
	winners := rows[:cut]
	w := 1.0 / float64(len(winners))
	out := make(map[string]float64, len(winners))
	for _, r := range winners {
		out[r.symbol] = w
	}
	return out
}

// ---------------------------------------------------------------------------
// Internal math helpers (mirror lowbeta.realizedBeta /
// realizedStdev so the backtest scoring agrees with the
// production scoring).
// ---------------------------------------------------------------------------

func closeSeriesFromHistory(h *SymbolHistory, from, to time.Time) []float64 {
	if h == nil {
		return nil
	}
	out := make([]float64, 0, len(h.Bars))
	fromN, toN := normaliseDate(from), normaliseDate(to)
	for _, b := range h.Bars {
		d := normaliseDate(b.Date)
		if d.Before(fromN) || d.After(toN) {
			continue
		}
		if b.Close > 0 {
			out = append(out, b.Close)
		}
	}
	return out
}

// realizedBeta uses the textbook formula
// cov(sym, mkt) / var(mkt). The two series must align in
// length; we truncate to the shorter so callers don't have to
// pre-align.
func realizedBeta(symR, mktR []float64) float64 {
	n := len(symR)
	if len(mktR) < n {
		n = len(mktR)
	}
	if n < 5 {
		return math.NaN()
	}
	symR = symR[len(symR)-n:]
	mktR = mktR[len(mktR)-n:]
	var sumSym, sumMkt float64
	for i := 0; i < n; i++ {
		sumSym += symR[i]
		sumMkt += mktR[i]
	}
	meanSym, meanMkt := sumSym/float64(n), sumMkt/float64(n)
	var cov, varM float64
	for i := 0; i < n; i++ {
		ds := symR[i] - meanSym
		dm := mktR[i] - meanMkt
		cov += ds * dm
		varM += dm * dm
	}
	if varM == 0 {
		return math.NaN()
	}
	return cov / varM
}

func realizedStdev(r []float64) float64 {
	if len(r) < 2 {
		return math.NaN()
	}
	var sum float64
	for _, v := range r {
		sum += v
	}
	mean := sum / float64(len(r))
	var ss float64
	for _, v := range r {
		d := v - mean
		ss += d * d
	}
	return math.Sqrt(ss / float64(len(r)-1))
}
