// Package correlation computes pairwise return correlations
// between symbols in a fund's universe ∪ positions so the PM
// can recognise hidden cluster risk.
//
// Why this exists. Per-symbol sizing (Sprint A #1), cross-
// sectional ranking (Sprint A #2), and concentration caps
// (Sprint C #1) catch most overexposure, but they all assume
// symbols are mutually independent. In practice the worst
// drawdowns come from invisibly-correlated cluster bets:
//
//  - Five different semis names all trading off the same SOX
//    factor — the per-symbol R is fine, the sector cap may be
//    fine, but the realised portfolio swings as if the fund
//    held one giant SOX ETF.
//  - "Diversifying" into a Chinese ADR alongside US tech when
//    both ride the same USD-CNY funding factor.
//  - Adding a "value" name that turns out to be 0.85 correlated
//    with a held growth name when both react to rate cuts.
//
// Renaissance / Two Sigma / Citadel all run a real-time
// correlation overlay: a candidate buy whose rolling correlation
// to any held name exceeds a threshold (commonly 0.7) is either
// rejected or sized at half the per-symbol ceiling. Sprint C #2
// surfaces the same signal to the PM prompt.
//
// I/O contract. The Service depends on an OHLC fetcher (the same
// one wired into quantsnapshot and ranking) so cache hits make
// repeat fetches free. Per-symbol fetch failures degrade to "no
// data" for that symbol — the rest of the universe still
// surfaces correlation rows.
//
// Math notes:
//
//   - Daily simple returns: r_t = close_t / close_{t-1} - 1.
//   - Pearson correlation between two return series with mean-
//     subtraction and population stddev denominators (matches
//     numpy.corrcoef defaults).
//   - When the variance of either side is below epsilon (a
//     perfectly flat series, common on a freshly listed
//     symbol's seed bars) the pair is dropped from the result;
//     correlation is mathematically undefined and the prompt
//     should not see a fake zero.
//
// The package returns deterministic output: candidates are
// alphabetically sorted, pair rows are sorted DESC by |corr| so
// the loudest links surface first.
package correlation

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fundai/server/internal/ohlc"
)

// SymbolRequest is one per-symbol fetch description. Symbol is
// mandatory; the rest fill out the OHLC fetch when present.
// Held=true marks the symbol as currently held — the per-symbol
// summary is only computed for non-held candidates against the
// held set.
type SymbolRequest struct {
	Symbol string
	Market string
	Held   bool
}

// HighCorrPair is one (Left, Right) pair whose |Rho| exceeds the
// configured threshold. Left < Right alphabetically so the same
// pair is never reported twice.
type HighCorrPair struct {
	Left  string
	Right string
	Rho   float64
}

// SymbolCorrSummary is one candidate's worst correlation to any
// held position. MaxAbsRho is the |rho| value, MaxRho keeps the
// sign so the PM can tell a positive (additive risk) cluster
// from a negative (hedging) relationship.
type SymbolCorrSummary struct {
	Symbol       string
	MaxRho       float64
	MaxAbsRho    float64
	MaxAbsTarget string
}

// HeldClusterStats summarises the correlation tightness inside
// the held set itself. nil when fewer than 2 held positions have
// data — no cluster to summarise.
type HeldClusterStats struct {
	HeldCount   int
	AvgPairwise float64
	MaxPairwise float64
	MaxLeft     string
	MaxRight    string
}

// Snapshot is the prompt-facing correlation read.
type Snapshot struct {
	// Window: human-readable lookback (e.g. "60 daily bars").
	Window string

	// SampleSize: how many symbols had usable return series.
	// Always >= 2 when the snapshot is returned (the package
	// returns nil otherwise).
	SampleSize int

	// HighCorrThreshold echoes the |rho| floor used for
	// HighCorrPairs. Lets the PM see what "high" means.
	HighCorrThreshold float64

	// HighCorrPairs lists all (Left, Right) pairs whose |Rho|
	// exceeds the threshold. Sorted DESC by |Rho|; capped at
	// MaxPairs.
	HighCorrPairs []HighCorrPair

	// CandidateSummaries: one row per non-held symbol with its
	// worst correlation to any held name. Empty when there are
	// no held positions or no non-held candidates.
	CandidateSummaries []SymbolCorrSummary

	// HeldCluster summarises the held set itself. nil when the
	// held set is < 2 symbols with data.
	HeldCluster *HeldClusterStats
}

// HasSignal reports whether the snapshot carries enough data to
// be worth rendering. Used by the prompt builder to omit the
// block entirely on degenerate inputs.
func (s Snapshot) HasSignal() bool {
	return s.SampleSize >= 2 && (len(s.HighCorrPairs) > 0 || len(s.CandidateSummaries) > 0 || s.HeldCluster != nil)
}

// Options configures the lookback, thresholds, and concurrency.
// Zero values fall back to withDefaults.
type Options struct {
	// LookbackBars is the number of OHLC bars (≈ trading
	// days) to use for the return series. Default 60; clamped
	// to [20, 252]. We compute (LookbackBars-1) return
	// observations per symbol.
	LookbackBars int

	// HighCorrThreshold is the |rho| floor for the
	// HighCorrPairs list. Default 0.7 — the conventional
	// "diversifying" cutoff. Clamped to [0.3, 0.99].
	HighCorrThreshold float64

	// MaxPairs caps how many HighCorrPairs make it into the
	// prompt so a degenerate universe (everything correlated
	// to everything) doesn't blow the prompt size. Default 10;
	// clamped to [1, 50].
	MaxPairs int

	// Concurrency is the worker pool size for the OHLC fetch
	// pass. Default 4; clamped to [1, 16]. Cached fetchers
	// barely move the needle here so we stay conservative.
	Concurrency int

	// PerCallTimeout bounds each OHLC fetch. Default 6s; same
	// convention as newsrecall.
	PerCallTimeout time.Duration
}

func (o Options) withDefaults() Options {
	if o.LookbackBars <= 0 {
		o.LookbackBars = 60
	}
	if o.LookbackBars < 20 {
		o.LookbackBars = 20
	}
	if o.LookbackBars > 252 {
		o.LookbackBars = 252
	}
	if o.HighCorrThreshold <= 0 {
		o.HighCorrThreshold = 0.7
	}
	if o.HighCorrThreshold < 0.3 {
		o.HighCorrThreshold = 0.3
	}
	if o.HighCorrThreshold > 0.99 {
		o.HighCorrThreshold = 0.99
	}
	if o.MaxPairs <= 0 {
		o.MaxPairs = 10
	}
	if o.MaxPairs > 50 {
		o.MaxPairs = 50
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 4
	}
	if o.Concurrency > 16 {
		o.Concurrency = 16
	}
	if o.PerCallTimeout <= 0 {
		o.PerCallTimeout = 6 * time.Second
	}
	return o
}

// Service is the only public type. Stateless apart from the
// configured OHLC fetcher + Options.
type Service struct {
	ohlc ohlc.Fetcher
	opts Options
}

// NewService is the only constructor. A nil fetcher produces a
// degenerate service whose Compute is a no-op — the wiring layer
// uses this when the platform's OHLC pipeline isn't enabled.
func NewService(fetcher ohlc.Fetcher, opts Options) *Service {
	return &Service{ohlc: fetcher, opts: opts.withDefaults()}
}

// Options exposes the effective options for diagnostics. Safe on
// nil receivers.
func (s *Service) Options() Options {
	if s == nil {
		return Options{}.withDefaults()
	}
	return s.opts
}

// Compute is the only public method. Fetches OHLC for each
// request in parallel (bounded by Options.Concurrency), computes
// daily returns, and rolls them up into the pair / summary /
// cluster views the prompt consumes.
//
// Returns nil when:
//   - the service / fetcher is nil
//   - requests is empty after dedup
//   - fewer than 2 symbols had usable data
//
// Per-symbol fetch errors do NOT abort the call — the symbol is
// dropped from the result, the rest continues.
func (s *Service) Compute(ctx context.Context, requests []SymbolRequest) *Snapshot {
	if s == nil || s.ohlc == nil {
		return nil
	}
	deduped := dedupeRequests(requests)
	if len(deduped) < 2 {
		return nil
	}

	// Parallel-fetch returns for each (symbol, market). slot[i]
	// stays nil on error so we can drop the symbol in the
	// post-pass without holding a lock for the whole loop.
	type slot struct {
		req     SymbolRequest
		returns []float64
	}
	results := make([]slot, len(deduped))
	limiter := make(chan struct{}, s.opts.Concurrency)
	var wg sync.WaitGroup
	for i, req := range deduped {
		wg.Add(1)
		limiter <- struct{}{}
		go func(i int, req SymbolRequest) {
			defer wg.Done()
			defer func() { <-limiter }()
			fetchCtx, cancel := context.WithTimeout(ctx, s.opts.PerCallTimeout)
			defer cancel()
			bars, err := s.ohlc.Fetch(fetchCtx, ohlc.FetchRequest{
				Symbol:    req.Symbol,
				Market:    req.Market,
				LookbackN: s.opts.LookbackBars,
			})
			if err != nil || len(bars) < 20 {
				return
			}
			results[i] = slot{req: req, returns: returnSeries(bars)}
		}(i, req)
	}
	wg.Wait()

	// Drop empty slots so the math passes only see usable rows.
	clean := make([]slot, 0, len(results))
	for _, r := range results {
		if len(r.returns) >= 19 { // need at least 20 bars → 19 returns
			clean = append(clean, r)
		}
	}
	if len(clean) < 2 {
		return nil
	}
	sort.SliceStable(clean, func(i, j int) bool { return clean[i].req.Symbol < clean[j].req.Symbol })

	// Truncate every return series to the shortest length so
	// the Pearson math compares the same number of
	// observations across pairs. min() over a small slice is
	// cheaper than aligning by date.
	minLen := len(clean[0].returns)
	for _, c := range clean {
		if len(c.returns) < minLen {
			minLen = len(c.returns)
		}
	}
	for i := range clean {
		clean[i].returns = clean[i].returns[len(clean[i].returns)-minLen:]
	}

	// Pearson over every (i, j), i < j.
	type pair struct {
		i, j int
		rho  float64
	}
	pairs := make([]pair, 0, len(clean)*(len(clean)-1)/2)
	for i := 0; i < len(clean); i++ {
		for j := i + 1; j < len(clean); j++ {
			rho, ok := pearson(clean[i].returns, clean[j].returns)
			if !ok {
				continue
			}
			pairs = append(pairs, pair{i: i, j: j, rho: rho})
		}
	}

	// HighCorrPairs.
	high := make([]HighCorrPair, 0)
	for _, p := range pairs {
		if math.Abs(p.rho) < s.opts.HighCorrThreshold {
			continue
		}
		left, right := clean[p.i].req.Symbol, clean[p.j].req.Symbol
		if left > right {
			left, right = right, left
		}
		high = append(high, HighCorrPair{Left: left, Right: right, Rho: round4(p.rho)})
	}
	sort.SliceStable(high, func(i, j int) bool {
		ai, aj := math.Abs(high[i].Rho), math.Abs(high[j].Rho)
		if ai == aj {
			if high[i].Left == high[j].Left {
				return high[i].Right < high[j].Right
			}
			return high[i].Left < high[j].Left
		}
		return ai > aj
	})
	if len(high) > s.opts.MaxPairs {
		high = high[:s.opts.MaxPairs]
	}

	// CandidateSummaries: for each non-held symbol, find the
	// pair with the held set that maximises |rho|.
	heldIdx := make([]int, 0)
	for i, c := range clean {
		if c.req.Held {
			heldIdx = append(heldIdx, i)
		}
	}

	candidates := make([]SymbolCorrSummary, 0)
	if len(heldIdx) > 0 {
		for _, c := range clean {
			if c.req.Held {
				continue
			}
			best := SymbolCorrSummary{Symbol: c.req.Symbol}
			seen := false
			for _, h := range heldIdx {
				rho, ok := pearson(c.returns, clean[h].returns)
				if !ok {
					continue
				}
				abs := math.Abs(rho)
				if !seen || abs > best.MaxAbsRho {
					best.MaxRho = rho
					best.MaxAbsRho = abs
					best.MaxAbsTarget = clean[h].req.Symbol
					seen = true
				}
			}
			if !seen {
				continue
			}
			best.MaxRho = round4(best.MaxRho)
			best.MaxAbsRho = round4(best.MaxAbsRho)
			candidates = append(candidates, best)
		}
		sort.SliceStable(candidates, func(i, j int) bool {
			if candidates[i].MaxAbsRho == candidates[j].MaxAbsRho {
				return candidates[i].Symbol < candidates[j].Symbol
			}
			return candidates[i].MaxAbsRho > candidates[j].MaxAbsRho
		})
	}

	// HeldCluster: avg + max pairwise inside the held set.
	var cluster *HeldClusterStats
	if len(heldIdx) >= 2 {
		var sum float64
		count := 0
		maxRho := -1.0
		var maxLeft, maxRight string
		for a := 0; a < len(heldIdx); a++ {
			for b := a + 1; b < len(heldIdx); b++ {
				rho, ok := pearson(clean[heldIdx[a]].returns, clean[heldIdx[b]].returns)
				if !ok {
					continue
				}
				abs := math.Abs(rho)
				sum += abs
				count++
				if abs > maxRho {
					maxRho = abs
					left, right := clean[heldIdx[a]].req.Symbol, clean[heldIdx[b]].req.Symbol
					if left > right {
						left, right = right, left
					}
					maxLeft, maxRight = left, right
				}
			}
		}
		if count > 0 {
			cluster = &HeldClusterStats{
				HeldCount:   len(heldIdx),
				AvgPairwise: round4(sum / float64(count)),
				MaxPairwise: round4(maxRho),
				MaxLeft:     maxLeft,
				MaxRight:    maxRight,
			}
		}
	}

	snap := &Snapshot{
		Window:             windowLabel(s.opts.LookbackBars),
		SampleSize:         len(clean),
		HighCorrThreshold:  s.opts.HighCorrThreshold,
		HighCorrPairs:      high,
		CandidateSummaries: candidates,
		HeldCluster:        cluster,
	}
	if !snap.HasSignal() {
		return nil
	}
	return snap
}

// returnSeries converts OHLC bars to a simple-return series.
// Always returns len(bars) - 1 observations or 0 on degenerate
// input.
func returnSeries(bars []ohlc.Bar) []float64 {
	if len(bars) < 2 {
		return nil
	}
	out := make([]float64, 0, len(bars)-1)
	for i := 1; i < len(bars); i++ {
		prev := bars[i-1].Close
		curr := bars[i].Close
		if prev <= 0 || curr <= 0 {
			continue
		}
		r := curr/prev - 1
		if math.IsNaN(r) || math.IsInf(r, 0) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// pearson computes the Pearson correlation between two equal-
// length series. Returns (rho, false) when either side has near-
// zero variance (correlation undefined).
func pearson(xs, ys []float64) (float64, bool) {
	n := len(xs)
	if n != len(ys) || n < 3 {
		return 0, false
	}
	var sx, sy float64
	for i := 0; i < n; i++ {
		sx += xs[i]
		sy += ys[i]
	}
	mx := sx / float64(n)
	my := sy / float64(n)
	var cov, vx, vy float64
	for i := 0; i < n; i++ {
		dx := xs[i] - mx
		dy := ys[i] - my
		cov += dx * dy
		vx += dx * dx
		vy += dy * dy
	}
	const eps = 1e-12
	if vx < eps || vy < eps {
		return 0, false
	}
	return cov / math.Sqrt(vx*vy), true
}

// dedupeRequests upper-cases symbols + lower-cases markets,
// dedupes on (symbol, market), and propagates the Held flag with
// OR semantics — if a symbol shows up in both the universe and
// the positions list, the merged request is marked held.
func dedupeRequests(in []SymbolRequest) []SymbolRequest {
	if len(in) == 0 {
		return nil
	}
	type key struct{ s, m string }
	idx := make(map[key]int, len(in))
	out := make([]SymbolRequest, 0, len(in))
	for _, r := range in {
		sym := strings.ToUpper(strings.TrimSpace(r.Symbol))
		if sym == "" {
			continue
		}
		mkt := strings.ToLower(strings.TrimSpace(r.Market))
		k := key{s: sym, m: mkt}
		if i, ok := idx[k]; ok {
			if r.Held {
				out[i].Held = true
			}
			continue
		}
		r.Symbol = sym
		r.Market = mkt
		idx[k] = len(out)
		out = append(out, r)
	}
	return out
}

// windowLabel renders the prompt window string. Kept simple so
// the assertion in the prompt integration tests can pin a known
// string.
func windowLabel(bars int) string {
	if bars <= 1 {
		bars = 1
	}
	// Single concatenation; no fmt to keep allocation low.
	return intStr(bars) + " daily bars"
}

func intStr(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var tmp [20]byte
	i := len(tmp)
	for v > 0 {
		i--
		tmp[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		tmp[i] = '-'
	}
	return string(tmp[i:])
}

// round4 trims to 4 dp signed (correlations can be negative).
func round4(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	const scale = 1e4
	if v < 0 {
		return -float64(int64(-v*scale+0.5)) / scale
	}
	return float64(int64(v*scale+0.5)) / scale
}
