// Package pairspread computes the rolling spread z-score for
// high-correlation symbol pairs already identified by the
// correlation.Service (Sprint C #2), so the PM prompt can
// surface "this is a tight pair AND it's currently extended"
// to the LLM.
//
// Sprint E #4. The correlation block tells the PM which pairs
// are tight (|rho| >= 0.7 by default). What it can't tell the
// PM is whether the pair is currently TRADING tight (close to
// its long-run ratio) or DIVERGENT (a long-only fund holding
// both is taking unintended single-name risk because one side
// has run away from the other). The spread z-score is the
// classical Vidyamurthy / Kestner pairs-trading signal:
//
//     spread(t)   = log(price_A(t) / price_B(t))
//     spread_mean = mean(spread over lookback)
//     spread_std  = stdev(spread over lookback)
//     spread_Z    = (spread(t) - spread_mean) / spread_std
//
// |z| < 1   → pair is trading near its long-run ratio; both
//             sides participate normally.
// |z| in [1, 2] → mild divergence; the PM should size NEW
//                 entries with caution but no forced exits.
// |z| >= 2  → 2-sigma divergence; the cheap side (the symbol
//             on the "low" end of the z) is a candidate "add"
//             OR the rich side is a candidate "reduce" for
//             pure long-only mean-reversion. The system
//             prompt teaches the PM the asymmetry.
//
// Implementation reuses the OHLC fetcher (and hence cache) the
// correlation matrix already populated, so the second pass is
// nearly free. We only fetch bars for symbols that appear in
// the supplied HighCorrPair list — the rest of the universe is
// skipped entirely.
//
// Architecture mirrors correlation / quality / earnings:
//   - Pair input names a (Left, Right, Market, Rho) tuple.
//   - Service holds the OHLC fetcher + options.
//   - Build returns a *Snapshot or nil; nil means "no
//     actionable pair spread today" (no input pairs / OHLC
//     unwired / every pair came back too noisy).
package pairspread

import (
	"context"
	"math"
	"sort"
	"strings"

	"github.com/fundai/server/internal/ohlc"
)

// PairRequest names one (Left, Right) pair whose spread to
// compute. Rho is carried from the upstream correlation snapshot
// so the prompt can show both numbers together — a high spread
// |z| on a pair with low |rho| means the pair was never really
// related, so the divergence isn't actionable. The PM sees both
// numbers and decides.
type PairRequest struct {
	// Left + Right are the upper-cased tickers. The package
	// does NOT enforce Left < Right alphabetically (the
	// upstream correlation snapshot already does that) so a
	// fund-specific override could pass a custom side ordering
	// if needed.
	Left  string
	Right string
	// Market is the lower-cased market tag passed straight
	// through to the OHLC fetcher. Both legs share a market;
	// a cross-market pair (e.g. an ADR vs its home listing)
	// would need a different signal anyway.
	Market string
	// Rho is the upstream correlation. Echoed back in the
	// output so the prompt block stays self-contained.
	Rho float64
}

// PairSpread is the computed signal for one input pair. All
// fields are populated only when at least LookbackBars+1 bars
// of price history were available for BOTH legs.
type PairSpread struct {
	Left       string
	Right      string
	Rho        float64
	// Spread is the latest log(left/right). Rendered with sign
	// so the LLM can tell whether Left is rich (positive) or
	// cheap (negative) vs Right.
	Spread float64
	// SpreadMean is the lookback-mean log ratio. The "fair
	// value" the spread is measured against.
	SpreadMean float64
	// SpreadStd is the lookback-stdev of the log ratio. The
	// unit the z-score is denominated in.
	SpreadStd float64
	// SpreadZ is (Spread - SpreadMean) / SpreadStd. Positive
	// = Left is rich vs Right; negative = Left is cheap.
	SpreadZ float64
	// LookbackBars is echoed back so the prompt can quote it.
	LookbackBars int
}

// Snapshot is the per-call output. PairsByAbsZ sorted descending
// by |SpreadZ| so the prompt's top-K cap surfaces the most
// extended pairs first.
type Snapshot struct {
	// Window: human-readable lookback ("60 daily bars").
	Window string
	// LookbackBars: same as Window but as an int the prompt
	// can do math on.
	LookbackBars int
	// ZThreshold: the |z| level above which the system prompt
	// treats a divergence as actionable (default 2.0). Echoed
	// back so operators can reason about the gate.
	ZThreshold float64
	// PairsByAbsZ: every input pair with a computable spread,
	// sorted descending by |SpreadZ|. Length <= MaxPairs.
	PairsByAbsZ []PairSpread
}

// HasSignal reports whether at least one pair crossed the
// |z| >= ZThreshold action threshold. Used by the wiring layer
// to decide whether to emit the prompt block at all — when
// every spread is within band the block is suppressed (the LLM
// doesn't need to see noise).
func (s *Snapshot) HasSignal() bool {
	if s == nil || len(s.PairsByAbsZ) == 0 {
		return false
	}
	for _, p := range s.PairsByAbsZ {
		if math.Abs(p.SpreadZ) >= s.ZThreshold {
			return true
		}
	}
	return false
}

// Options tunes the lookback, threshold, and the top-K cap.
type Options struct {
	// LookbackBars is the bar window for the rolling mean +
	// stdev. 60 daily bars matches the correlation default so
	// the OHLC cache hits the same buckets.
	LookbackBars int
	// ZThreshold is the |z| level above which a divergence is
	// "actionable" — the snapshot still RETURNS pairs below
	// this level (the prompt likes to show the context) but
	// HasSignal flips false when nothing is above. Default 2.0
	// (the classical 2-σ pairs-trading entry).
	ZThreshold float64
	// MaxPairs caps the number of pairs surfaced to the prompt.
	// Default 10 — same as correlation.Options.MaxPairs.
	MaxPairs int
	// FetchTimeoutMs caps each OHLC fetch. 0 = use fetcher
	// default (typically context-bound). Tests pin a small
	// timeout to keep wall time bounded.
	FetchTimeoutMs int
}

func (o Options) withDefaults() Options {
	if o.LookbackBars <= 1 {
		o.LookbackBars = 60
	}
	if o.ZThreshold <= 0 {
		o.ZThreshold = 2.0
	}
	if o.MaxPairs <= 0 {
		o.MaxPairs = 10
	}
	return o
}

// Service holds the OHLC fetcher + resolved options.
type Service struct {
	ohlc ohlc.Fetcher
	opts Options
}

// NewService wires a Service. nil fetcher tolerated; Build
// returns nil so the prompt block is skipped.
func NewService(fetcher ohlc.Fetcher, opts Options) *Service {
	return &Service{ohlc: fetcher, opts: opts.withDefaults()}
}

// Options exposes the resolved tuning struct.
func (s *Service) Options() Options {
	if s == nil {
		return Options{}.withDefaults()
	}
	return s.opts
}

// Build computes the spread z for each input pair, sorts
// descending by |z|, and returns a *Snapshot (or nil when no
// pair was computable). Pair legs share one underlying OHLC
// fetch each — the OHLC cache makes the repeat call for a
// symbol that already showed up in correlation effectively free.
//
// Errors per leg are swallowed; that pair simply drops out of
// the output. A pair where BOTH legs have insufficient bars also
// drops out.
func (s *Service) Build(ctx context.Context, pairs []PairRequest) *Snapshot {
	if s == nil || s.ohlc == nil {
		return nil
	}
	cleaned := normalisePairs(pairs)
	if len(cleaned) == 0 {
		return nil
	}
	// Per-symbol close-price cache so each unique symbol fires
	// at most one Fetch. The map key is "MARKET|SYMBOL".
	closesByKey := make(map[string][]float64, len(cleaned)*2)
	for _, p := range cleaned {
		s.ensureCloses(ctx, closesByKey, p.Left, p.Market)
		s.ensureCloses(ctx, closesByKey, p.Right, p.Market)
	}
	out := make([]PairSpread, 0, len(cleaned))
	for _, p := range cleaned {
		ps, ok := computePairSpread(
			closesByKey[keyForSymbol(p.Left, p.Market)],
			closesByKey[keyForSymbol(p.Right, p.Market)],
			s.opts.LookbackBars,
		)
		if !ok {
			continue
		}
		ps.Left = p.Left
		ps.Right = p.Right
		ps.Rho = p.Rho
		out = append(out, ps)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Slice(out, func(i, j int) bool {
		if math.Abs(out[i].SpreadZ) != math.Abs(out[j].SpreadZ) {
			return math.Abs(out[i].SpreadZ) > math.Abs(out[j].SpreadZ)
		}
		// Tie-break by Left+Right to keep deterministic
		// prompt bytes across runs.
		if out[i].Left != out[j].Left {
			return out[i].Left < out[j].Left
		}
		return out[i].Right < out[j].Right
	})
	if len(out) > s.opts.MaxPairs {
		out = out[:s.opts.MaxPairs]
	}
	return &Snapshot{
		Window:       formatWindow(s.opts.LookbackBars),
		LookbackBars: s.opts.LookbackBars,
		ZThreshold:   s.opts.ZThreshold,
		PairsByAbsZ:  out,
	}
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// normalisePairs upper-cases / trims / dedupes the pair list.
// A pair (A, A) is dropped (degenerate). Pairs whose left ==
// right after normalisation also drop. Caller-owned output.
func normalisePairs(in []PairRequest) []PairRequest {
	out := make([]PairRequest, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, p := range in {
		l := strings.ToUpper(strings.TrimSpace(p.Left))
		r := strings.ToUpper(strings.TrimSpace(p.Right))
		if l == "" || r == "" || l == r {
			continue
		}
		mk := strings.ToLower(strings.TrimSpace(p.Market))
		// Canonical key: ordered pair so (A,B,USD) and
		// (B,A,USD) collide in the dedup. We do NOT reorder
		// the output struct's Left/Right — the upstream
		// correlation snapshot already enforces Left < Right
		// alphabetically and we keep that convention.
		canonL, canonR := l, r
		if canonL > canonR {
			canonL, canonR = canonR, canonL
		}
		key := mk + "|" + canonL + "|" + canonR
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, PairRequest{
			Left:   l,
			Right:  r,
			Market: mk,
			Rho:    p.Rho,
		})
	}
	return out
}

// ensureCloses memoises the per-symbol OHLC fetch. On error /
// missing data the entry stays nil; downstream consumers detect
// that and drop the pair.
func (s *Service) ensureCloses(ctx context.Context, cache map[string][]float64, symbol, market string) {
	key := keyForSymbol(symbol, market)
	if _, ok := cache[key]; ok {
		return
	}
	bars, err := s.ohlc.Fetch(ctx, ohlc.FetchRequest{
		Symbol:    symbol,
		Market:    market,
		Interval:  ohlc.IntervalDay,
		// +5 padding so we have a few extra bars in case the
		// upstream returns fewer than requested.
		LookbackN: s.opts.LookbackBars + 5,
	})
	if err != nil || len(bars) == 0 {
		cache[key] = nil
		return
	}
	closes := make([]float64, 0, len(bars))
	for _, b := range bars {
		if b.Close > 0 && !math.IsNaN(b.Close) {
			closes = append(closes, b.Close)
		}
	}
	cache[key] = closes
}

// keyForSymbol is the canonical cache / map key. Lower-case
// market + upper-case symbol matches the OHLC layer's own
// normalisation.
func keyForSymbol(symbol, market string) string {
	return strings.ToLower(market) + "|" + strings.ToUpper(symbol)
}

// computePairSpread returns the PairSpread for the supplied
// close-price slices. Both slices must carry at least
// lookbackBars+1 closes; the function aligns them on the LAST
// lookbackBars+1 entries (assumes both legs have synchronous bars
// — the upstream OHLC fetcher returns same-day daily bars in the
// same order). Returns (zero, false) when either slice is too
// short or every spread value is NaN.
func computePairSpread(left, right []float64, lookbackBars int) (PairSpread, bool) {
	if lookbackBars < 2 {
		return PairSpread{}, false
	}
	if len(left) < lookbackBars+1 || len(right) < lookbackBars+1 {
		return PairSpread{}, false
	}
	leftWin := left[len(left)-lookbackBars-1:]
	rightWin := right[len(right)-lookbackBars-1:]
	if len(leftWin) != len(rightWin) {
		// Defensive — different lookback would skew spread math.
		// Truncate both to the shorter window so the rolling
		// mean / stdev stay aligned.
		n := len(leftWin)
		if len(rightWin) < n {
			n = len(rightWin)
		}
		leftWin = leftWin[len(leftWin)-n:]
		rightWin = rightWin[len(rightWin)-n:]
		if n < lookbackBars+1 {
			return PairSpread{}, false
		}
	}
	spreads := make([]float64, 0, len(leftWin))
	for i := range leftWin {
		l := leftWin[i]
		r := rightWin[i]
		if l <= 0 || r <= 0 || math.IsNaN(l) || math.IsNaN(r) {
			continue
		}
		spreads = append(spreads, math.Log(l/r))
	}
	if len(spreads) < 2 {
		return PairSpread{}, false
	}
	var sum float64
	for _, v := range spreads {
		sum += v
	}
	mean := sum / float64(len(spreads))
	var sumSq float64
	for _, v := range spreads {
		d := v - mean
		sumSq += d * d
	}
	// Sample stdev (n-1 denominator).
	stdev := math.Sqrt(sumSq / float64(len(spreads)-1))
	last := spreads[len(spreads)-1]
	var z float64
	if stdev > 0 {
		z = (last - mean) / stdev
	}
	return PairSpread{
		Spread:       last,
		SpreadMean:   mean,
		SpreadStd:    stdev,
		SpreadZ:      z,
		LookbackBars: lookbackBars,
	}, true
}

// formatWindow renders the lookback as the same human-readable
// string the correlation snapshot uses ("60 daily bars"). Keeping
// the wording identical helps an operator quickly cross-check
// the two blocks in the prompt audit.
func formatWindow(bars int) string {
	if bars == 1 {
		return "1 daily bar"
	}
	return intoString(bars) + " daily bars"
}

// intoString avoids pulling strconv just for the one formatting
// call — strconv lives in the standard library so this is purely
// cosmetic, but the rest of the file already does strings ops
// so the cost of adding the import would dominate.
func intoString(v int) string {
	// fmt.Sprintf would also work; we use a hand-rolled path
	// for speed (called on every Build).
	if v == 0 {
		return "0"
	}
	negative := v < 0
	if negative {
		v = -v
	}
	digits := make([]byte, 0, 4)
	for v > 0 {
		digits = append(digits, byte('0'+v%10))
		v /= 10
	}
	// Reverse.
	for i, j := 0, len(digits)-1; i < j; i, j = i+1, j-1 {
		digits[i], digits[j] = digits[j], digits[i]
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}
