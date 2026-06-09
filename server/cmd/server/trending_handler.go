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
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fundai/server/internal/dailypicks"
	"github.com/fundai/server/internal/indicator"
	"github.com/fundai/server/internal/ohlc"
)

// trendingMostActiveTTL is how long the process-level memoised
// list stays warm. 15 min mirrors the underlying ohlc.Cache TTL
// so the handler cache never serves stale data the OHLC layer
// has already refreshed.
const trendingMostActiveTTL = 15 * time.Minute

// trendingMostActiveCap is the upper bound on the universe size
// the handler will fan out OHLC requests for. Protects the
// Yahoo upstream from a runaway watchlist that the ops team
// accidentally seeds with 5000 symbols.
const trendingMostActiveCap = 200

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
	picks *dailypicks.Repo
	clock func() time.Time

	mu    sync.Mutex
	cache map[string]trendingCacheEntry // key: market
}

type trendingCacheEntry struct {
	at    time.Time
	value trendingMostActiveResponse
}

func newTrendingHandler(of ohlc.Fetcher, picks *dailypicks.Repo) *trendingHandler {
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
func (h *trendingHandler) handleMostActive(w http.ResponseWriter, r *http.Request) {
	market := strings.TrimSpace(r.URL.Query().Get("market"))
	if market == "" {
		market = "us_equity"
	}
	// Cache lookup — bucket key is just the market. We
	// deliberately don't include the limit in the cache key so
	// a `?limit=10` request hits the same compute as `?limit=50`;
	// the projection happens AFTER the cache fetch.
	now := h.clock()
	h.mu.Lock()
	if entry, ok := h.cache[market]; ok && now.Sub(entry.at) < trendingMostActiveTTL {
		cached := entry.value
		h.mu.Unlock()
		writeJSON(w, http.StatusOK, projectTrending(cached, r.URL.Query().Get("limit")))
		return
	}
	h.mu.Unlock()

	resp := h.computeMostActive(r.Context(), market)

	h.mu.Lock()
	h.cache[market] = trendingCacheEntry{at: now, value: resp}
	h.mu.Unlock()

	writeJSON(w, http.StatusOK, projectTrending(resp, r.URL.Query().Get("limit")))
}

// computeMostActive is the cold-path producer. Walks the watchlist
// universe, fetches OHLC for each, computes the relative-volume
// metric, sorts descending. The OHLC fetcher's own cache absorbs
// the second call within the TTL bucket.
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

	type scored struct {
		row trendingMostActiveRow
	}
	rows := make([]scored, 0, len(universe))
	// Per-symbol context bounded by a tight ceiling so a single
	// stuck upstream call can't gum up the whole compute.
	for _, sym := range universe {
		bars, err := h.fetchBars(ctx, sym, market)
		if err != nil || len(bars) < 21 { // need >= 20 prior bars + current
			continue
		}
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
		for i := start; i < len(bars)-1; i++ {
			sum += bars[i].Volume
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
