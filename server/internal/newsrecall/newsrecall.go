// Package newsrecall builds a per-symbol structured list of recent
// news catalysts the PM prompt consumes.
//
// Why this exists. The DecisionInput already carries a
// NewsSentiment string the existing analyst pipeline renders — but
// that string is a single market-wide blob. The PM has no easy
// way to ask "what catalysts are touching THIS symbol in the last
// 48h?" without re-reading the blob. Sprint B #3 surfaces the
// structured per-symbol view so the LLM can reason at the candidate
// level — the workflow Two Sigma / Citadel / Renaissance use when
// they pair a momentum signal with a same-week catalyst check
// before sizing.
//
// Sprint B #3 ships a simpler-than-RAG first cut: we fetch recent
// items via the existing marketdata.Service, drop everything older
// than MaxAge, sort by PublishedAt DESC, and keep the top-K per
// symbol. A future Sprint C can swap the recency-only ranker for a
// proper embedding-based recall (cosine similarity against the
// fund's investment thesis embedding) without changing the
// DecisionInput.NewsCatalysts surface — the package is already
// fronted by a NewsFetcher interface.
//
// I/O contract. The Service depends on a NewsFetcher (satisfied by
// *marketdata.Service). It runs all per-symbol fetches in parallel
// behind a small worker pool with a per-call deadline so a slow
// upstream provider cannot stall the PM loop. Network failures on
// individual symbols are downgraded to "no catalysts" for that
// symbol — the rest of the run continues.
package newsrecall

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fundai/server/internal/marketdata"
)

// NewsFetcher is the surface this package depends on. Satisfied by
// *marketdata.Service (which already handles caching, translation,
// provider rotation and rate limits). Mocked directly in tests so
// we don't drag marketdata's full surface into a unit test.
type NewsFetcher interface {
	GetNews(ctx context.Context, instrument marketdata.InstrumentRef, limit int) ([]marketdata.NewsItem, error)
}

// Request is one per-symbol lookup. Symbol is mandatory; the rest
// flesh out the InstrumentRef so the marketdata service can pick
// the right provider (Sina / Eastmoney for cnstock, SerpAPI /
// Tavily for global). The wiring layer builds these from the fund
// profile + Universe ∪ Positions.
type Request struct {
	Symbol         string
	Market         string
	Exchange       string
	AssetClass     string
	InstrumentType string
}

// Hit is one news item rendered for the prompt. Times come back as
// time.Time so the prompt layer can decide on the wire-format.
// Source / Language / URL surface so the LLM can dampen its weight
// on low-credibility sources or skip duplicates by URL.
type Hit struct {
	Title       string
	Summary     string
	Source      string
	Language    string
	URL         string
	PublishedAt time.Time
	// HoursOld is the convenience the prompt rounds and displays
	// inline — "AAPL: 4h ago, Reuters" reads better than two
	// RFC-3339 timestamps.
	HoursOld float64
}

// SymbolCatalysts is the per-symbol payload the wiring layer hands
// to decision.DecisionInput.NewsCatalysts. Hits are ordered most-
// recent first.
type SymbolCatalysts struct {
	Symbol string
	Hits   []Hit
}

// Options configures the lookback, per-symbol cap, fetch
// concurrency, and per-call deadline. Zero values fall back to
// withDefaults.
type Options struct {
	// MaxAge bounds how stale a news item can be before we drop
	// it. 7×24h is the conservative default — the PM cares most
	// about catalysts from the current week. Clamped to
	// [1h, 30 days].
	MaxAge time.Duration

	// MaxPerSymbol caps how many hits we surface per symbol so
	// the prompt JSON stays small on a 20-symbol universe.
	// Default 3, clamped to [1, 10].
	MaxPerSymbol int

	// FetchLimit is the per-symbol limit passed to the upstream
	// NewsFetcher. We over-fetch so the recency filter and the
	// MaxPerSymbol trim still surface fresh items. Default 8,
	// clamped to [MaxPerSymbol, 25].
	FetchLimit int

	// Concurrency is the size of the worker pool. Keeping it
	// small (default 4) limits the simultaneous upstream load
	// on Sina / Eastmoney / Tavily. Clamped to [1, 16].
	Concurrency int

	// PerCallTimeout bounds how long one upstream call can
	// take before we abandon it for that symbol. 6s matches the
	// existing GetNewsDigest convention. Clamped to [1s, 30s].
	PerCallTimeout time.Duration
}

func (o Options) withDefaults() Options {
	if o.MaxAge <= 0 {
		o.MaxAge = 7 * 24 * time.Hour
	}
	if o.MaxAge < time.Hour {
		o.MaxAge = time.Hour
	}
	if o.MaxAge > 30*24*time.Hour {
		o.MaxAge = 30 * 24 * time.Hour
	}
	if o.MaxPerSymbol <= 0 {
		o.MaxPerSymbol = 3
	}
	if o.MaxPerSymbol > 10 {
		o.MaxPerSymbol = 10
	}
	if o.FetchLimit <= 0 {
		o.FetchLimit = 8
	}
	if o.FetchLimit < o.MaxPerSymbol {
		o.FetchLimit = o.MaxPerSymbol
	}
	if o.FetchLimit > 25 {
		o.FetchLimit = 25
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
	if o.PerCallTimeout < time.Second {
		o.PerCallTimeout = time.Second
	}
	if o.PerCallTimeout > 30*time.Second {
		o.PerCallTimeout = 30 * time.Second
	}
	return o
}

// Service is the only public type. Stateless apart from the
// configured NewsFetcher + Options.
type Service struct {
	fetcher NewsFetcher
	opts    Options
}

// NewService is the only constructor. A nil fetcher produces a
// degenerate service whose BuildCatalysts is a no-op — the wiring
// layer relies on this when the marketdata service isn't enabled.
func NewService(fetcher NewsFetcher, opts Options) *Service {
	return &Service{fetcher: fetcher, opts: opts.withDefaults()}
}

// Options exposes the effective options for diagnostics. Safe on
// nil receivers — returns defaults.
func (s *Service) Options() Options {
	if s == nil {
		return Options{}.withDefaults()
	}
	return s.opts
}

// BuildCatalysts fetches recent news for each Request in parallel
// (bounded by Options.Concurrency) and emits one SymbolCatalysts
// per symbol that has at least one in-window hit.
//
// The result is ordered by the first Request's Symbol position so
// the prompt is deterministic across runs.
//
// Returns nil (no error) when:
//   - the service / fetcher is nil
//   - requests is empty after normalisation
//   - every fetch errored or returned no in-window hits
//
// Never returns an error: per-symbol fetch failures are recorded
// into the result as "no catalysts" for that symbol, on the
// principle that an outage on one provider should not stall a PM
// run.
func (s *Service) BuildCatalysts(ctx context.Context, requests []Request, now time.Time) []SymbolCatalysts {
	if s == nil || s.fetcher == nil {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	cutoff := now.Add(-s.opts.MaxAge)
	deduped := dedupeRequests(requests)
	if len(deduped) == 0 {
		return nil
	}

	type slot struct {
		idx     int
		symbol  string
		hits    []Hit
	}
	results := make([]slot, len(deduped))
	limiter := make(chan struct{}, s.opts.Concurrency)
	var wg sync.WaitGroup
	for i, req := range deduped {
		wg.Add(1)
		limiter <- struct{}{}
		go func(i int, req Request) {
			defer wg.Done()
			defer func() { <-limiter }()
			fetchCtx, cancel := context.WithTimeout(ctx, s.opts.PerCallTimeout)
			defer cancel()
			items, err := s.fetcher.GetNews(fetchCtx, requestToInstrument(req), s.opts.FetchLimit)
			if err != nil {
				results[i] = slot{idx: i, symbol: req.Symbol, hits: nil}
				return
			}
			hits := filterAndRank(items, cutoff, now, s.opts.MaxPerSymbol)
			results[i] = slot{idx: i, symbol: req.Symbol, hits: hits}
		}(i, req)
	}
	wg.Wait()

	out := make([]SymbolCatalysts, 0, len(results))
	for _, r := range results {
		if len(r.hits) == 0 {
			continue
		}
		out = append(out, SymbolCatalysts{Symbol: r.symbol, Hits: r.hits})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// dedupeRequests normalises symbols to upper case + trim, drops
// blanks, and keeps the first occurrence per (Symbol, Market) pair.
// Mirror the cooldown / ranking dedup convention so the wiring
// layer doesn't have to remember per-package idioms.
func dedupeRequests(in []Request) []Request {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]Request, 0, len(in))
	for _, r := range in {
		sym := strings.ToUpper(strings.TrimSpace(r.Symbol))
		if sym == "" {
			continue
		}
		mkt := strings.ToLower(strings.TrimSpace(r.Market))
		key := sym + "|" + mkt
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		r.Symbol = sym
		r.Market = mkt
		out = append(out, r)
	}
	return out
}

// requestToInstrument builds the marketdata.InstrumentRef the
// fetcher consumes. Currency / multiplier / expiry are intentionally
// omitted — news lookups don't need them.
func requestToInstrument(req Request) marketdata.InstrumentRef {
	return marketdata.InstrumentRef{
		Symbol:         req.Symbol,
		Market:         req.Market,
		Exchange:       req.Exchange,
		AssetClass:     req.AssetClass,
		InstrumentType: req.InstrumentType,
	}
}

// filterAndRank keeps only items inside the cutoff window, sorts
// them most-recent-first, and trims to cap. Items missing a
// PublishedAt are dropped — they can't be ranked without a time
// stamp and would push real catalysts off the cap.
func filterAndRank(items []marketdata.NewsItem, cutoff, now time.Time, cap int) []Hit {
	if len(items) == 0 {
		return nil
	}
	kept := make([]Hit, 0, len(items))
	for _, it := range items {
		if it.PublishedAt.IsZero() {
			continue
		}
		if it.PublishedAt.Before(cutoff) {
			continue
		}
		title := strings.TrimSpace(firstNonEmpty(it.TitleEn, it.Title, it.TitleZh))
		if title == "" {
			continue
		}
		summary := strings.TrimSpace(firstNonEmpty(it.SummaryEn, it.Summary, it.SummaryZh))
		hoursOld := now.Sub(it.PublishedAt).Hours()
		if hoursOld < 0 {
			hoursOld = 0
		}
		kept = append(kept, Hit{
			Title:       title,
			Summary:     summary,
			Source:      strings.TrimSpace(it.Source),
			Language:    strings.TrimSpace(it.Language),
			URL:         strings.TrimSpace(it.URL),
			PublishedAt: it.PublishedAt.UTC(),
			HoursOld:    hoursOld,
		})
	}
	if len(kept) == 0 {
		return nil
	}
	sort.SliceStable(kept, func(i, j int) bool {
		return kept[i].PublishedAt.After(kept[j].PublishedAt)
	})
	if cap > 0 && len(kept) > cap {
		kept = kept[:cap]
	}
	return kept
}

// firstNonEmpty is a small helper — the marketdata package's
// version is private, and importing it would couple the packages
// for no payoff. Trim-empty-only because the caller already
// validates the field's intent.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
