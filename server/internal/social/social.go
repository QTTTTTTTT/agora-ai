// Package social pulls retail / crowd posts from public social
// platforms (Xueqiu, StockTwits, r/wallstreetbets) and emits them
// as sentiment.Item values that the existing scoring pipeline can
// consume.
//
// Why this exists. The SentimentAnalyst (S8.1) already has prompt
// hooks for crowd mood, but production wiring today only feeds it
// "news" items (Reuters / Bloomberg / akshare). Retail-driven
// flow on names like AMC / GME / Nvidia obviously isn't visible in
// the wire-news stream — Sprint 9.3 closes the gap by integrating
// the three platforms that dominate retail discourse in the
// markets we cover:
//
//   - Xueqiu (xueqiu.com): the de-facto retail social network for
//     A-shares + HK + US-listed China names.
//   - StockTwits: US equities + crypto.
//   - r/wallstreetbets: the noisiest, highest-frequency retail
//     forum for US single-name and meme momentum.
//
// I/O contract. Every provider satisfies the Provider interface
// (FetchPosts per symbol). The Registry runs all configured
// providers in parallel and concatenates the items. Errors on any
// single provider are downgraded to a slog warning — the rest of
// the request continues — because retail-mood is a SOFT signal
// and the downstream sentiment block must keep flowing when any
// one platform is down.
//
// Provider implementations live in subpackages (provider/xueqiu,
// provider/stocktwits, provider/reddit). They depend only on net/http
// and stdlib so the cycle-free dependency graph stays intact.
package social

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fundai/server/internal/sentiment"
)

// Platform is the canonical identifier for a social-feed source.
// Used as the sentiment.Item.Source value so the downstream
// SentimentAnalyst's SourceBreakdown column reads "xueqiu / stocktwits
// / reddit_wsb" instead of opaque integers.
type Platform string

const (
	PlatformXueqiu     Platform = "xueqiu"
	PlatformStockTwits Platform = "stocktwits"
	PlatformRedditWSB  Platform = "reddit_wsb"
)

// Request scopes one per-symbol fetch. The provider decides how to
// map Symbol + Market into its native query (e.g. Xueqiu uses
// "$AAPL$" cashtags, StockTwits uses bare symbols, Reddit needs a
// full-text search). Limit is a HINT, not a hard contract — the
// provider may return fewer (no posts on the day) or up to Limit*2
// (server-side pagination overshoot).
type Request struct {
	Symbol string
	// Market is the canonical market code (US, CN, HK, …). Empty =
	// provider falls back to its native default.
	Market string
	// Limit is the soft cap on posts returned. Providers should
	// honor it but the registry tolerates overshoot.
	Limit int
	// MaxAge filters out posts older than this. Zero = no filter.
	MaxAge time.Duration
}

// Provider is the per-platform fetcher. Implementations should
// return an empty slice rather than nil so callers can range over
// the result without a defensive guard. Errors should be RETURNED
// not panicked — the registry decides what to do with them.
type Provider interface {
	Platform() Platform
	FetchPosts(ctx context.Context, req Request) ([]sentiment.Item, error)
}

// Registry aggregates posts from multiple providers behind a single
// FetchPosts entry point. Used by the workflow wiring layer; tests
// can also build a Registry directly with stub providers.
type Registry struct {
	providers []Provider
	opts      RegistryOptions
	logger    *slog.Logger
}

// RegistryOptions tune the Registry's behaviour. All zero values
// produce a sensible default that mirrors how the existing news
// recall wiring is set up (small parallel pool, short per-call
// timeout, modest per-symbol cap).
type RegistryOptions struct {
	// PerProviderTimeout caps a single provider's FetchPosts call.
	// Defaults to 8s — long enough for a heavy-loaded provider to
	// reply, short enough that one stuck provider doesn't stall
	// the daily roundtable.
	PerProviderTimeout time.Duration
	// Concurrency caps simultaneous provider goroutines. Defaults
	// to len(providers) because we want every platform fetched in
	// parallel; raising it has no effect.
	Concurrency int
	// PerProviderLimit overrides Request.Limit for any provider
	// that didn't set its own. Defaults to 25 — Reddit's hot feed
	// in particular is noisy.
	PerProviderLimit int
	// MaxAge caps how old an item can be before it's dropped.
	// Defaults to 24h — retail mood beyond a day is rarely
	// actionable for tomorrow's PM.
	MaxAge time.Duration
}

func (o RegistryOptions) normalize(numProviders int) RegistryOptions {
	out := o
	if out.PerProviderTimeout <= 0 {
		out.PerProviderTimeout = 8 * time.Second
	}
	if out.Concurrency <= 0 {
		out.Concurrency = numProviders
		if out.Concurrency == 0 {
			out.Concurrency = 1
		}
	}
	if out.PerProviderLimit <= 0 {
		out.PerProviderLimit = 25
	}
	if out.MaxAge <= 0 {
		out.MaxAge = 24 * time.Hour
	}
	return out
}

// NewRegistry constructs a Registry. Nil providers are silently
// dropped so the caller can pass the result of conditional
// constructors (e.g. only Xueqiu when ENABLE_XUEQIU=1).
func NewRegistry(providers []Provider, opts RegistryOptions, logger *slog.Logger) *Registry {
	clean := make([]Provider, 0, len(providers))
	for _, p := range providers {
		if p == nil {
			continue
		}
		clean = append(clean, p)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Registry{
		providers: clean,
		opts:      opts.normalize(len(clean)),
		logger:    logger,
	}
}

// HasProviders reports whether the registry has at least one
// active provider. Callers can use this to skip the round-trip
// when the feature is disabled entirely.
func (r *Registry) HasProviders() bool {
	return r != nil && len(r.providers) > 0
}

// FetchPosts runs every configured provider in parallel, merges
// the results, filters by MaxAge, and returns them sorted by
// PublishedAt DESC. Errors on individual providers are logged and
// skipped — the caller never sees a failed provider unless EVERY
// provider failed, in which case ErrAllProvidersFailed is returned.
func (r *Registry) FetchPosts(ctx context.Context, req Request) ([]sentiment.Item, error) {
	if r == nil || len(r.providers) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(req.Symbol) == "" {
		return nil, errors.New("social: Request.Symbol required")
	}
	if req.Limit <= 0 {
		req.Limit = r.opts.PerProviderLimit
	}
	if req.MaxAge <= 0 {
		req.MaxAge = r.opts.MaxAge
	}

	type providerResult struct {
		items []sentiment.Item
		err   error
		name  Platform
	}
	results := make(chan providerResult, len(r.providers))

	sem := make(chan struct{}, r.opts.Concurrency)
	var wg sync.WaitGroup
	for _, p := range r.providers {
		wg.Add(1)
		sem <- struct{}{}
		go func(p Provider) {
			defer wg.Done()
			defer func() { <-sem }()
			callCtx, cancel := context.WithTimeout(ctx, r.opts.PerProviderTimeout)
			defer cancel()
			items, err := p.FetchPosts(callCtx, req)
			results <- providerResult{items: items, err: err, name: p.Platform()}
		}(p)
	}
	wg.Wait()
	close(results)

	merged := make([]sentiment.Item, 0, len(r.providers)*req.Limit)
	successCount := 0
	for res := range results {
		if res.err != nil {
			r.logger.Warn("social.provider.fetch_failed",
				"platform", string(res.name),
				"symbol", req.Symbol,
				"err", res.err,
			)
			continue
		}
		successCount++
		merged = append(merged, res.items...)
	}
	if successCount == 0 && len(r.providers) > 0 {
		return nil, ErrAllProvidersFailed
	}

	cutoff := time.Time{}
	if req.MaxAge > 0 {
		cutoff = time.Now().Add(-req.MaxAge)
	}
	filtered := merged[:0]
	for _, it := range merged {
		if !cutoff.IsZero() && it.PublishedAt.Before(cutoff) {
			continue
		}
		filtered = append(filtered, it)
	}

	sort.SliceStable(filtered, func(i, j int) bool {
		return filtered[i].PublishedAt.After(filtered[j].PublishedAt)
	})
	if len(filtered) > req.Limit*len(r.providers) {
		filtered = filtered[:req.Limit*len(r.providers)]
	}
	return filtered, nil
}

// ErrAllProvidersFailed is returned by Registry.FetchPosts when
// EVERY configured provider returned an error. Callers can
// distinguish this from a successful-but-empty result, which is
// indistinguishable from "no posts in the lookback window".
var ErrAllProvidersFailed = errors.New("social: all providers failed")
