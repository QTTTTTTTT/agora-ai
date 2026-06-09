package factorlab

import (
	"math"
	"sort"
	"strings"
	"time"
)

// Factor is the per-symbol scalar-score abstraction used by the
// IC/IR/分层 report path. It's a DIFFERENT contract than
// Strategy.Weights: a Strategy emits portfolio weights (long-only,
// sums to ≤ 1, hard cutoff at top quartile); a Factor emits a raw
// numeric score per symbol so the report engine can rank, slice
// into quintiles, and correlate vs forward returns without losing
// the cross-sectional information that the quartile cutoff
// destroys.
//
// The contract:
//
//   - Score(f, sym, asOf) returns (score, ok).
//   - asOf is the date through which bars are visible (no
//     look-ahead). The report passes asOf == close of the
//     observation day; forward returns are then close[asOf+h] /
//     close[asOf] − 1 for horizon h.
//   - ok=false means "no observation today" (insufficient history,
//     bad data, etc.) — the symbol is simply excluded from that
//     day's cross-section.
//   - Higher score = expected higher forward return (i.e. the
//     factor is signed so the long leg always goes long the top
//     quintile). For naturally-negative factors (like realised vol
//     where LOW vol predicts higher returns) the implementation
//     negates internally.
//
// The five factors below are intentionally PRICE-ONLY: the MVP
// doesn't depend on fundamentals (which we don't have for the
// 5000-stock backtest fixture). All five can be computed from
// OHLC alone — the same data the existing `Strategy` types use.
type Factor interface {
	Name() string
	Score(f *Fixture, sym string, asOf time.Time) (float64, bool)
}

// ScoreCrossSection runs a factor across every symbol in the
// fixture and returns the sorted (symbol, score) cross-section
// for asOf. Symbols where Score returns ok=false are omitted.
// Sorted descending so winners come first — IC code consumes the
// raw slice (not the sort order); the layered code reads the sort
// order. Kept here so callers don't reimplement the loop.
func ScoreCrossSection(f *Fixture, factor Factor, asOf time.Time) []FactorScore {
	if f == nil || factor == nil {
		return nil
	}
	out := make([]FactorScore, 0, len(f.Histories))
	for _, h := range f.Histories {
		sym := strings.ToUpper(strings.TrimSpace(h.Symbol))
		score, ok := factor.Score(f, sym, asOf)
		if !ok || math.IsNaN(score) || math.IsInf(score, 0) {
			continue
		}
		out = append(out, FactorScore{Symbol: sym, Score: score})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// FactorScore is one (symbol, score) tuple in a cross-section.
type FactorScore struct {
	Symbol string  `json:"symbol"`
	Score  float64 `json:"score"`
}

// ---------------------------------------------------------------------------
// momentum_12_1m_factor — same definition as Momentum12_1M.Weights
// but emits the raw 12m-1m return per symbol instead of a binary
// "top quartile" weight.
// ---------------------------------------------------------------------------

type Momentum12_1MFactor struct {
	// LookbackDays / SkipDays default to (252, 21).
	LookbackDays int
	SkipDays     int
}

func (m Momentum12_1MFactor) Name() string { return "momentum_12_1m" }

func (m Momentum12_1MFactor) Score(f *Fixture, sym string, asOf time.Time) (float64, bool) {
	lookback := m.LookbackDays
	if lookback <= 0 {
		lookback = 252
	}
	skip := m.SkipDays
	if skip <= 0 {
		skip = 21
	}
	end := asOf.AddDate(0, 0, -skip)
	start := asOf.AddDate(0, 0, -lookback)
	closes := f.CloseSeries(sym, start, end)
	if len(closes) < 30 {
		return 0, false
	}
	first := closes[0]
	last := closes[len(closes)-1]
	if first <= 0 {
		return 0, false
	}
	return last/first - 1.0, true
}

// ---------------------------------------------------------------------------
// short_reversal_1m_factor — the classic short-term reversal: bad
// 1-month performers mean-revert. Sign-flipped so HIGH score =
// expected higher forward return.
// ---------------------------------------------------------------------------

type ShortReversal1MFactor struct {
	LookbackDays int // default 21
}

func (s ShortReversal1MFactor) Name() string { return "short_reversal_1m" }

func (s ShortReversal1MFactor) Score(f *Fixture, sym string, asOf time.Time) (float64, bool) {
	lookback := s.LookbackDays
	if lookback <= 0 {
		lookback = 21
	}
	start := asOf.AddDate(0, 0, -lookback)
	closes := f.CloseSeries(sym, start, asOf)
	if len(closes) < 5 {
		return 0, false
	}
	first := closes[0]
	last := closes[len(closes)-1]
	if first <= 0 {
		return 0, false
	}
	r1m := last/first - 1.0
	// Reversal: low last-month return → high score.
	return -r1m, true
}

// ---------------------------------------------------------------------------
// low_vol_60d_factor — realised vol over 60 sessions, sign-flipped.
// HIGH score = LOW volatility = expected higher risk-adjusted
// forward return. Matches the LowVol strategy's ranking logic but
// emits the raw inverted-vol per symbol.
// ---------------------------------------------------------------------------

type LowVol60DFactor struct {
	LookbackDays int // default 60
}

func (l LowVol60DFactor) Name() string { return "low_vol_60d" }

func (l LowVol60DFactor) Score(f *Fixture, sym string, asOf time.Time) (float64, bool) {
	lookback := l.LookbackDays
	if lookback <= 0 {
		lookback = 60
	}
	start := asOf.AddDate(0, 0, -2*lookback)
	closes := f.CloseSeries(sym, start, asOf)
	r := LogReturns(closes)
	if len(r) < lookback {
		return 0, false
	}
	r = r[len(r)-lookback:]
	vol := realizedStdev(r)
	if math.IsNaN(vol) || math.IsInf(vol, 0) || vol <= 0 {
		return 0, false
	}
	// Sign-flip: HIGH score = LOW vol.
	return -vol, true
}

// ---------------------------------------------------------------------------
// volume_breakout_20_factor — recent 5-day average volume divided
// by 20-day average. Above-1 means "volume is spiking now relative
// to the trailing month" — a classic accumulation tell. Requires
// Bar.Volume to be present in the fixture; symbols with no volume
// are excluded.
// ---------------------------------------------------------------------------

type VolumeBreakout20Factor struct {
	ShortDays int // default 5
	LongDays  int // default 20
}

func (v VolumeBreakout20Factor) Name() string { return "volume_breakout_20" }

func (v VolumeBreakout20Factor) Score(f *Fixture, sym string, asOf time.Time) (float64, bool) {
	short := v.ShortDays
	if short <= 0 {
		short = 5
	}
	long := v.LongDays
	if long <= 0 {
		long = 20
	}
	hist := f.History(sym)
	if hist == nil {
		return 0, false
	}
	target := normaliseDate(asOf)
	vols := make([]float64, 0, long)
	// Walk back from asOf collecting volumes for the trailing
	// `long` sessions where Volume > 0.
	for i := len(hist.Bars) - 1; i >= 0; i-- {
		b := hist.Bars[i]
		if normaliseDate(b.Date).After(target) {
			continue
		}
		if b.Volume <= 0 {
			continue
		}
		vols = append([]float64{b.Volume}, vols...) // prepend; oldest first
		if len(vols) >= long {
			break
		}
	}
	if len(vols) < long {
		return 0, false
	}
	// Short-window mean = last `short` of `vols`.
	var ss, ls float64
	for i, v := range vols {
		ls += v
		if i >= long-short {
			ss += v
		}
	}
	longMean := ls / float64(long)
	shortMean := ss / float64(short)
	if longMean <= 0 {
		return 0, false
	}
	return shortMean / longMean, true
}

// ---------------------------------------------------------------------------
// drawdown_recovery_factor — distance from the trailing 60-day
// peak, normalised. A symbol that's been holding near its peak
// scores high; one that's been crashing scores low. Signed so
// HIGH score = "in uptrend / shallow drawdown" = expected higher
// forward return.
//
// This is a deliberate complement to short-reversal: where SR
// chases names that crashed last month, DR favours names that
// HAVEN'T crashed. Diversifies the report's factor block.
// ---------------------------------------------------------------------------

type DrawdownRecoveryFactor struct {
	LookbackDays int // default 60
}

func (d DrawdownRecoveryFactor) Name() string { return "drawdown_recovery_60d" }

func (d DrawdownRecoveryFactor) Score(f *Fixture, sym string, asOf time.Time) (float64, bool) {
	lookback := d.LookbackDays
	if lookback <= 0 {
		lookback = 60
	}
	start := asOf.AddDate(0, 0, -lookback*2)
	closes := f.CloseSeries(sym, start, asOf)
	if len(closes) < lookback {
		return 0, false
	}
	closes = closes[len(closes)-lookback:]
	peak := closes[0]
	for _, c := range closes {
		if c > peak {
			peak = c
		}
	}
	if peak <= 0 {
		return 0, false
	}
	last := closes[len(closes)-1]
	// Score = last/peak − 1: zero when at peak, negative below.
	return last/peak - 1.0, true
}

// DefaultFactors returns the canonical Stage-2 factor list in the
// recommended display order. The order matters because IC reports
// are usually rendered side-by-side and we want the most
// fundamental ones (the cross-sectional momentum / mean-reversion
// pair) on the left.
func DefaultFactors() []Factor {
	return []Factor{
		Momentum12_1MFactor{},
		ShortReversal1MFactor{},
		LowVol60DFactor{},
		VolumeBreakout20Factor{},
		DrawdownRecoveryFactor{},
	}
}
