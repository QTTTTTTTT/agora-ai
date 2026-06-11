// trending_handler.go — /api/trending/* surface.
//
// v1 endpoints:
//
//	GET /api/trending/most-active?market=us_equity  — ordered list
//	  of symbols sorted by 20-day relative volume (latest bar volume
//	  divided by 20-bar SMA volume). Pure market observation —
//	  zero LLM. No subjective text.
//
// Compliance contract:
//
//	The "Most Active by Volume" framing is the SAFEST publisher
//	posture available: every cell is a NUMBER pulled from public
//	OHLC data, and the ordering criterion is DISCLOSED IN THE
//	RESPONSE PAYLOAD (criteria_disclosed field). The page is
//	identical for every reader regardless of tier — same shape as
//	Yahoo Finance's "Most Active" / Stock Rover's "Volume Leaders".
//
//	The handler deliberately does NOT carry any "watch this" /
//	"strong volume signals X" language. Naming, ordering, and
//	rendering rules live in this file and the frontend's
//	/trending page — both keep the framing observational
//	("Today's Most Active by Volume") rather than recommendational
//	("Top Volume Picks").
//
// Performance:
//
//	The handler computes the list synchronously on first request
//	then memoises it in a process-level cache for 15 minutes. The
//	underlying ohlc.Fetcher is already cache-wrapped (15-min
//	bucket in production) so even a cold handler cache only fans
//	out into the 50 unique upstream calls once per quarter-hour
//	per market. Memory cost: O(symbols × bytes_per_row) ≈ <50KB
//	for the seed 50-ticker universe.

package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/fundai/server/internal/dailypicks"
	"github.com/fundai/server/internal/indicator"
	"github.com/fundai/server/internal/ohlc"
)

// trendingMostActiveTTL is how long the process-level memoised
// list stays warm. 15 min mirrors the underlying ohlc.Cache TTL
// so the handler cache never serves stale data the OHLC layer
// has already refreshed.
const trendingMostActiveTTL = 15 * time.Minute

// trendingWarmInterval is the background-refresh cadence. We pick
// it just under trendingMostActiveTTL so the cache never expires
// between two scheduled warms — every user request that arrives
// after the first warm hits a warm cache and returns in <5 ms,
// instead of paying the 15-30 s cold-path fan-out cost.
const trendingWarmInterval = 14 * time.Minute

// trendingMostActiveCap is the upper bound on the universe size
// the handler will fan out OHLC requests for. Protects the
// Yahoo upstream from a runaway watchlist that the ops team
// accidentally seeds with 5000 symbols.
const trendingMostActiveCap = 200

// trendingFetchConcurrency is the worker-pool size for the per-symbol
// OHLC fan-out. The previous handler walked the universe sequentially
// (loop body: fetchBars → 6 s timeout each), which made a cold cache
// take 15-30 s for a 50-symbol universe — long enough that the user's
// browser tab visibly hung on "Loading…". A worker pool of 10
// drops the wall-clock cost to roughly
//   ceil(universe / 10) × avg-symbol-latency
// without overwhelming the Yahoo upstream (Yahoo's v8 chart endpoint
// is documented to be soft-rate-limited at low double-digit RPS per
// origin, so 10 concurrent is conservative). The Cache layer
// (server/internal/ohlc/registry.go) is already concurrency-safe.
const trendingFetchConcurrency = 10

// trendingWarmTimeout caps a single warm-cycle compute. Big enough
// to absorb a slow upstream tail (Yahoo occasionally takes 4-6 s on
// a cold connection) without letting a wedged provider hold the
// warmer goroutine forever and starve later cycles.
const trendingWarmTimeout = 45 * time.Second

// trendingWatchlistLister is the subset of *dailypicks.Repo this
// handler needs. Extracted as an interface so tests can inject an
// in-memory stub without standing up a real PostgreSQL instance.
// *dailypicks.Repo satisfies it implicitly; production wiring in
// router.go is unchanged.
type trendingWatchlistLister interface {
	ListActiveWatchlists(ctx context.Context) ([]dailypicks.Watchlist, error)
}

// trendingHandler bundles the /api/trending endpoints.
//
// Both ohlc + picks dependencies are nil-safe:
//   - ohlc nil → handler returns empty `criteria_disclosed`
//     list and an empty results slice, never errors. (Lets a
//     degraded deploy with no OHLC keys keep the route mounted
//     so the frontend doesn't 404 on the link.)
//   - picks nil → universe falls back to an empty list, same
//     graceful degradation.
type trendingHandler struct {
	ohlc  ohlc.Fetcher
	picks trendingWatchlistLister
	clock func() time.Time

	mu    sync.Mutex
	cache map[string]trendingCacheEntry // key: market

	// sf coalesces concurrent cold-path compute requests for the
	// same market into a single fan-out. Without it, N parallel
	// users arriving at the same cache-miss instant would each
	// fire 50 OHLC requests — multiplying our upstream pressure
	// and our wall-clock latency by N. With singleflight the
	// extra arrivals just await the in-flight compute and share
	// its result.
	sf singleflight.Group
}

type trendingCacheEntry struct {
	at    time.Time
	value trendingMostActiveResponse
}

func newTrendingHandler(of ohlc.Fetcher, picks trendingWatchlistLister) *trendingHandler {
	return &trendingHandler{
		ohlc:  of,
		picks: picks,
		clock: time.Now,
		cache: make(map[string]trendingCacheEntry),
	}
}

// trendingMostActiveRow is one card on the page. Pure data:
// symbol, latest close, day change %, volume, vol/20d ratio.
// No subjective fields ("hot", "trending up", etc.).
type trendingMostActiveRow struct {
	Rank             int     `json:"rank"`
	Symbol           string  `json:"symbol"`
	SymbolName       string  `json:"symbol_name,omitempty"`
	LastClose        float64 `json:"last_close"`
	PctChange1D      float64 `json:"pct_change_1d"`
	Volume           float64 `json:"volume"`
	Vol20DRatio      float64 `json:"vol_20d_ratio"`
	AsOf             string  `json:"asof,omitempty"`
}

// trendingMostActiveResponse is the wire payload. Notice
// criteria_disclosed: that's the SEC-friendly contract that
// makes the ranking algorithmic-output-not-recommendation.
type trendingMostActiveResponse struct {
	ListName           string                  `json:"list_name"`     // "Most Active by Volume"
	CriteriaDisclosed  []string                `json:"criteria_disclosed"`
	Market             string                  `json:"market"`
	GeneratedAt        string                  `json:"generated_at"`  // RFC3339 UTC
	UniverseSize       int                     `json:"universe_size"` // number of symbols scanned
	Results            []trendingMostActiveRow `json:"results"`
	Disclaimer         string                  `json:"disclaimer"`
}

// criteriaDisclosed returns the public, verifiable rules the
// ranking applies. This is what makes the list "algorithmic
// output" instead of "we picked these" — the rules are
// declared up front and every reader can replicate them.
func criteriaDisclosedMostActive() []string {
	return []string{
		"Ranking metric: latest-bar volume / 20-bar SMA volume (descending).",
		"Universe: symbols from active daily-pick watchlists for the requested market.",
		"Data source: end-of-day OHLC bars from upstream provider (Yahoo Finance v8 chart endpoint).",
		"Refresh: list memoised for up to 15 minutes; OHLC bars themselves cached up to 15 minutes upstream.",
		"Exclusions: symbols with fewer than 20 bars of history (insufficient denominator), zero-volume symbols.",
	}
}

// trendingMostActiveDisclaimer is the per-response footer
// reminding readers this is observation, not advice.
const trendingMostActiveDisclaimer = "This list is algorithmic market data. It is NOT an investment recommendation. The ranking is identical for all readers and based on publicly disclosed objective criteria. Past performance is not indicative of future results. Consult a licensed financial advisor before making any investment decision."

// handleMostActive serves GET /api/trending/most-active.
//
// All heavy lifting (cache check, compute fan-out, store) lives in
// getOrCompute so the warmer goroutine and the request handler
// share the exact same caching + dedup semantics. This handler
// is now reduced to "parse, delegate, project, write" — no cache
// state machine to keep in sync between two code paths.
func (h *trendingHandler) handleMostActive(w http.ResponseWriter, r *http.Request) {
	market := strings.TrimSpace(r.URL.Query().Get("market"))
	if market == "" {
		market = "us_equity"
	}
	resp := h.getOrCompute(r.Context(), market)
	writeJSON(w, http.StatusOK, projectTrending(resp, r.URL.Query().Get("limit")))
}

// getOrCompute is the single source of truth for the cache-aside
// + singleflight read path. Both the user-facing HTTP handler and
// the background warmer call this; that way:
//
//  1. The warmer pre-fills the same cache the handler reads from,
//     so a user navigating to /trending/most-active after boot
//     never pays the cold-path latency (15-30 s with 50 symbols).
//  2. If N requests miss the cache at the same instant (e.g.
//     immediately post-warm-expiry), singleflight collapses them
//     into ONE compute and ONE upstream OHLC fan-out, instead of
//     N × 50 = 500+ upstream calls.
//  3. We don't include the ?limit param in the cache key. We
//     always cache the FULL list and trim per-request in
//     projectTrending. That keeps the cache key cardinality at
//     O(markets) instead of O(markets × distinct-limits).
func (h *trendingHandler) getOrCompute(ctx context.Context, market string) trendingMostActiveResponse {
	if cached, ok := h.cacheLookup(market); ok {
		return cached
	}

	// singleflight key is just the market — concurrent misses
	// for the same market share one fan-out; misses for
	// different markets proceed independently.
	v, _, _ := h.sf.Do(market, func() (any, error) {
		// Double-check the cache inside the singleflight callback:
		// while we were waiting for sf to grant us the slot,
		// another goroutine may have already populated it.
		if cached, ok := h.cacheLookup(market); ok {
			return cached, nil
		}
		resp := h.computeMostActive(ctx, market)
		h.cacheStore(market, resp)
		return resp, nil
	})
	if v == nil {
		// Defensive fallback — singleflight callback returned
		// nil interface (shouldn't happen with the explicit
		// return above, but keep the type assertion total).
		return trendingMostActiveResponse{
			ListName:          "Most Active by Volume",
			CriteriaDisclosed: criteriaDisclosedMostActive(),
			Market:            market,
			GeneratedAt:       h.clock().UTC().Format(time.RFC3339),
			Disclaimer:        trendingMostActiveDisclaimer,
		}
	}
	return v.(trendingMostActiveResponse)
}

func (h *trendingHandler) cacheLookup(market string) (trendingMostActiveResponse, bool) {
	now := h.clock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if entry, ok := h.cache[market]; ok && now.Sub(entry.at) < trendingMostActiveTTL {
		return entry.value, true
	}
	return trendingMostActiveResponse{}, false
}

func (h *trendingHandler) cacheStore(market string, value trendingMostActiveResponse) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cache[market] = trendingCacheEntry{at: h.clock(), value: value}
}

// computeMostActive is the cold-path producer. Walks the watchlist
// universe, fans out OHLC fetches in PARALLEL (bounded by
// trendingFetchConcurrency), computes the relative-volume metric,
// sorts descending.
//
// Parallel fan-out is the bug fix for the "first hit shows Loading
// forever; refresh shows data" UX issue. Sequential fetch with a
// 6 s per-symbol timeout took 15-30 s on a cold cache for a
// 50-symbol universe. Parallel fetch with 10 workers compresses
// the same work into roughly ceil(50/10) × avg-symbol-latency =
// 5 × ~1 s ≈ 5 s, which keeps the user inside a tolerable
// "loading" budget even when they manage to slip in before the
// warmer's first run.
//
// Errors per symbol are SWALLOWED (logged at debug) — one
// delisted ticker or Yahoo 404 must not poison the entire list.
func (h *trendingHandler) computeMostActive(ctx context.Context, market string) trendingMostActiveResponse {
	resp := trendingMostActiveResponse{
		ListName:          "Most Active by Volume",
		CriteriaDisclosed: criteriaDisclosedMostActive(),
		Market:            market,
		GeneratedAt:       h.clock().UTC().Format(time.RFC3339),
		Disclaimer:        trendingMostActiveDisclaimer,
	}
	universe, names := h.universe(ctx, market)
	resp.UniverseSize = len(universe)
	if h.ohlc == nil || len(universe) == 0 {
		return resp
	}

	// Parallel fan-out — preserves universe order in `fetched`
	// so the eventual ranking is deterministic for symbols that
	// tie on Vol20DRatio (Yahoo / Stock Rover order-stability
	// matters for the screenshot-diff regression tests).
	type fetched struct {
		ok   bool
		bars []ohlc.Bar
	}
	results := make([]fetched, len(universe))
	sem := make(chan struct{}, trendingFetchConcurrency)
	var wg sync.WaitGroup
	for i, sym := range universe {
		wg.Add(1)
		go func(i int, sym string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			bars, err := h.fetchBars(ctx, sym, market)
			if err != nil || len(bars) < 21 { // need >= 20 prior bars + current
				return
			}
			results[i] = fetched{ok: true, bars: bars}
		}(i, sym)
	}
	wg.Wait()

	type scored struct {
		row trendingMostActiveRow
	}
	rows := make([]scored, 0, len(universe))
	for i, sym := range universe {
		r := results[i]
		if !r.ok {
			continue
		}
		bars := r.bars
		latest := bars[len(bars)-1]
		if latest.Volume <= 0 {
			continue
		}
		// 20-bar SMA volume EXCLUDING the latest bar — same
		// denominator Yahoo / Stock Rover / Finviz use for
		// the "Relative Volume" column.
		var sum float64
		start := len(bars) - 1 - 20
		if start < 0 {
			start = 0
		}
		count := 0
		for j := start; j < len(bars)-1; j++ {
			sum += bars[j].Volume
			count++
		}
		if count == 0 || sum == 0 {
			continue
		}
		avg20 := sum / float64(count)
		ratio := latest.Volume / avg20

		dod := 0.0
		if prev := bars[len(bars)-2].Close; prev != 0 {
			dod = latest.Close/prev - 1
		}

		row := trendingMostActiveRow{
			Symbol:      sym,
			SymbolName:  names[sym],
			LastClose:   latest.Close,
			PctChange1D: dod,
			Volume:      latest.Volume,
			Vol20DRatio: ratio,
			AsOf:        latest.Time.UTC().Format(time.RFC3339),
		}
		rows = append(rows, scored{row: row})
	}
	// Sort by Vol20DRatio descending. Stable sort so ties
	// (two symbols at 1.0x exactly) stay in deterministic
	// universe order — easier to spot test fixture issues.
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].row.Vol20DRatio > rows[j].row.Vol20DRatio
	})

	out := make([]trendingMostActiveRow, 0, len(rows))
	for i, s := range rows {
		r := s.row
		r.Rank = i + 1
		out = append(out, r)
	}
	resp.Results = out
	// Indicator helpers stay imported for future "Momentum
	// Screen" / "Disruptive Screen" siblings that share this
	// handler. The compiler optimises the unused-import-of-an-
	// alias away — but the explicit reference keeps a casual
	// grep from removing the package on the assumption it's
	// dead.
	_ = indicator.MinBarsForFullSnapshot
	return resp
}

// trendingWarmMarkets is the static list of markets the warmer
// pre-populates. The frontend's MARKETS selector currently only
// exposes "us_equity"; adding a new market here costs nothing
// for the warmer (one extra ~5 s compute per cycle) and instantly
// makes the new tab feel instant for users instead of
// "page-loads-for-20-seconds-then-works".
func trendingWarmMarkets() []string {
	return []string{"us_equity"}
}

// RunWarmer is the long-running background goroutine that keeps
// the most-active cache warm. It does an immediate first warm
// (so the cache is hot within seconds of boot, not 14 min later)
// then refreshes on every tick.
//
// The function returns when ctx is cancelled. Caller is
// responsible for `go trendingH.RunWarmer(appCtx)` somewhere in
// the boot path; we don't self-spawn because we want the caller
// to own goroutine lifecycle.
func (h *trendingHandler) RunWarmer(ctx context.Context) {
	if h == nil || h.ohlc == nil {
		// Degraded deploy — no OHLC fetcher means there's
		// nothing to warm. handleMostActive will still return
		// an empty results list with the criteria disclosure.
		slog.Info("trending warmer skipped (no OHLC fetcher wired)")
		return
	}
	markets := trendingWarmMarkets()
	h.warmAll(ctx, markets)

	ticker := time.NewTicker(trendingWarmInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.warmAll(ctx, markets)
		}
	}
}

// warmAll fires getOrCompute for every market in `markets`,
// bounded by trendingWarmTimeout per market. Serial across
// markets so a slow upstream for market A doesn't multiply our
// upstream pressure when market B's worker pool also kicks in
// — daily-picks loops also share the same OHLC fetcher and we
// want to leave them headroom.
func (h *trendingHandler) warmAll(ctx context.Context, markets []string) {
	for _, m := range markets {
		select {
		case <-ctx.Done():
			return
		default:
		}
		wctx, cancel := context.WithTimeout(ctx, trendingWarmTimeout)
		start := h.clock()
		resp := h.getOrCompute(wctx, m)
		cancel()
		slog.Info("trending warmer cycle",
			"market", m,
			"universe", resp.UniverseSize,
			"rows", len(resp.Results),
			"elapsed_ms", h.clock().Sub(start).Milliseconds(),
		)
	}
}

// fetchBars is a thin wrapper over h.ohlc.Fetch with the
// canonical 30-bar lookback. 30 bars is enough for the 20-bar
// SMA denominator + a few days of buffer if one upstream bar is
// missing.
func (h *trendingHandler) fetchBars(parent context.Context, symbol, market string) ([]ohlc.Bar, error) {
	ctx, cancel := context.WithTimeout(parent, 6*time.Second)
	defer cancel()
	bars, err := h.ohlc.Fetch(ctx, ohlc.FetchRequest{
		Symbol:    symbol,
		Market:    market,
		Interval:  ohlc.IntervalDay,
		LookbackN: 30,
	})
	if err != nil {
		// ErrNoData / ErrNoProvider are soft conditions — the
		// caller just skips the symbol.
		if errors.Is(err, ohlc.ErrNoData) || errors.Is(err, ohlc.ErrNoProvider) {
			return nil, err
		}
		return nil, err
	}
	return bars, nil
}

// universe returns the (deduped, capped) symbol list to scan
// for the requested market, plus a name lookup populated from
// the watchlist rows. Pulled from active daily_pick_watchlists
// so adding a new watchlist via SQL automatically extends the
// Most-Active page.
//
// We honour the per-watchlist Market column so a USA-only
// request doesn't pull HK tickers in.
func (h *trendingHandler) universe(ctx context.Context, market string) ([]string, map[string]string) {
	names := map[string]string{}
	if h.picks == nil {
		return nil, names
	}
	wls, err := h.picks.ListActiveWatchlists(ctx)
	if err != nil {
		return nil, names
	}
	seen := map[string]bool{}
	out := make([]string, 0, 50)
	for _, wl := range wls {
		if !strings.EqualFold(wl.Market, market) {
			continue
		}
		for _, raw := range wl.Symbols {
			sym := strings.ToUpper(strings.TrimSpace(raw))
			if sym == "" || seen[sym] {
				continue
			}
			seen[sym] = true
			out = append(out, sym)
			if len(out) >= trendingMostActiveCap {
				return out, names
			}
		}
	}
	return out, names
}

// projectTrending honours the ?limit= query param without
// invalidating the cache (we always cache the full list).
// Limit defaults to 50, capped at 200.
func projectTrending(in trendingMostActiveResponse, limitStr string) trendingMostActiveResponse {
	limit := 50
	if limitStr != "" {
		// strconv.Atoi failure → keep default.
		if n, err := parsePositiveInt(limitStr); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 200 {
		limit = 200
	}
	out := in
	if len(in.Results) > limit {
		out.Results = in.Results[:limit]
	}
	return out
}

// parsePositiveInt is the local helper to keep the handler
// dependency surface lean (we don't import strconv just for
// one Atoi at the handler boundary; the standard wisdom is
// "the boundary parsers ARE the validation", so keep them
// terse and explicit).
func parsePositiveInt(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errors.New("not a positive integer")
		}
		n = n*10 + int(r-'0')
		if n > 1_000_000 { // overflow guard
			return 0, errors.New("too large")
		}
	}
	if n == 0 {
		return 0, errors.New("zero")
	}
	return n, nil
}
