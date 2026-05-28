package earnings

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// G1 #1. Cache layer for the two earnings Fetcher interfaces
// (forward Fetcher + history HistoryFetcher). Both share the
// same Yahoo v10/quoteSummary endpoint, both are hit per PM
// tick per symbol, and Yahoo will rate-limit anyone hitting the
// keyless endpoint above a few hundred calls/hour. This cache
// brings outbound traffic down to roughly N_symbols × N_funds
// per TTL window, regardless of how many PM ticks fire inside
// that window.
//
// Two cache wrappers — one per fetcher interface — keep the
// public API of the package clean: the wiring layer can
// independently TTL-tune the forward and history channels
// (defaults: forward 6h because analysts publish revised dates
// intraday; history 24h because epsActual / epsEstimate is
// fixed once the print lands).
//
// Singleflight is layered ON TOP of the TTL cache so multiple
// concurrent PM ticks across funds DON'T all trigger the same
// upstream call after a TTL expiry — the second caller piggy-
// backs the first call's result. This matters because PM ticks
// across funds tend to fire in the same second (the workflow
// runner walks funds sequentially with no jitter), so an
// expired cache + 10 funds = 10 concurrent Yahoo calls without
// singleflight.

// CacheOptions tunes the cache behaviour. Zero-valued Options
// yields production defaults.
type CacheOptions struct {
	// TTL is the per-entry lifetime. Defaults vary per cache
	// type: NewCache (forward calendar) defaults to 6h;
	// NewHistoryCache (history) defaults to 24h. Pass an
	// explicit non-zero value to override; ttl <= 0 is
	// silently coerced to the type's default.
	TTL time.Duration
}

// ---------------------------------------------------------------------------
// Forward-calendar cache
// ---------------------------------------------------------------------------

// Cache wraps a Fetcher with per-(market, symbol-set) TTL
// caching + singleflight. The Yahoo provider returns multiple
// events per call (one per symbol), so we cache on the request
// signature (sorted symbols + market) rather than per symbol —
// this matches the actual call shape and keeps the hit rate
// high (the same fund hitting the same universe twice in 6h
// shares one entry).
type Cache struct {
	upstream Fetcher
	ttl      time.Duration

	mu      sync.RWMutex
	entries map[string]forwardCacheEntry

	sf singleflight.Group
}

type forwardCacheEntry struct {
	events    []Event
	expiresAt time.Time
}

// NewCache constructs the forward Fetcher cache. ttl <= 0 →
// 6h default. Passing a nil upstream returns a Cache whose
// Fetch always returns an empty slice (degrades the feature
// off the same way NoopFetcher does).
func NewCache(upstream Fetcher, opts CacheOptions) *Cache {
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = 6 * time.Hour
	}
	return &Cache{
		upstream: upstream,
		ttl:      ttl,
		entries:  make(map[string]forwardCacheEntry),
	}
}

// Fetch implements Fetcher. Cache hit returns a fresh copy of
// the cached slice so callers can mutate without poisoning the
// cache. Cache miss runs the upstream call under singleflight
// (so concurrent identical requests collapse to one).
func (c *Cache) Fetch(ctx context.Context, req FetchRequest) ([]Event, error) {
	if c == nil || c.upstream == nil {
		return nil, nil
	}
	key := forwardCacheKey(req)
	if events := c.lookupForward(key); events != nil {
		return cloneEvents(events), nil
	}
	v, err, _ := c.sf.Do(key, func() (any, error) {
		if cached := c.lookupForward(key); cached != nil {
			return cached, nil
		}
		events, err := c.upstream.Fetch(ctx, req)
		if err != nil {
			return nil, err
		}
		c.storeForward(key, events)
		return events, nil
	})
	if err != nil {
		return nil, err
	}
	events, _ := v.([]Event)
	return cloneEvents(events), nil
}

func (c *Cache) lookupForward(key string) []Event {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil
	}
	if time.Now().After(entry.expiresAt) {
		return nil
	}
	return entry.events
}

func (c *Cache) storeForward(key string, events []Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = forwardCacheEntry{
		events:    cloneEvents(events),
		expiresAt: time.Now().Add(c.ttl),
	}
}

// Purge clears every cached entry. Operator escape hatch when
// the upstream provider has been hot-swapped or an emergency
// refresh is needed (e.g. Yahoo started serving stale data).
func (c *Cache) Purge() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]forwardCacheEntry)
}

// ---------------------------------------------------------------------------
// History cache
// ---------------------------------------------------------------------------

// HistoryCache wraps a HistoryFetcher with per-(market,
// symbol-set, lookback) TTL caching + singleflight. Mirrors
// Cache exactly; only the entry payload type differs.
type HistoryCache struct {
	upstream HistoryFetcher
	ttl      time.Duration

	mu      sync.RWMutex
	entries map[string]historyCacheEntry

	sf singleflight.Group
}

type historyCacheEntry struct {
	events    []HistoricalEvent
	expiresAt time.Time
}

// NewHistoryCache constructs the history HistoryFetcher cache.
// ttl <= 0 → 24h default. nil upstream returns a cache whose
// FetchHistory always returns empty (degrades feature off the
// same way NoopHistoryFetcher does).
func NewHistoryCache(upstream HistoryFetcher, opts CacheOptions) *HistoryCache {
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &HistoryCache{
		upstream: upstream,
		ttl:      ttl,
		entries:  make(map[string]historyCacheEntry),
	}
}

// FetchHistory implements HistoryFetcher.
func (c *HistoryCache) FetchHistory(ctx context.Context, req HistoryRequest) ([]HistoricalEvent, error) {
	if c == nil || c.upstream == nil {
		return nil, nil
	}
	key := historyCacheKey(req)
	if events := c.lookupHistory(key); events != nil {
		return cloneHistoricalEvents(events), nil
	}
	v, err, _ := c.sf.Do(key, func() (any, error) {
		if cached := c.lookupHistory(key); cached != nil {
			return cached, nil
		}
		events, err := c.upstream.FetchHistory(ctx, req)
		if err != nil {
			return nil, err
		}
		c.storeHistory(key, events)
		return events, nil
	})
	if err != nil {
		return nil, err
	}
	events, _ := v.([]HistoricalEvent)
	return cloneHistoricalEvents(events), nil
}

func (c *HistoryCache) lookupHistory(key string) []HistoricalEvent {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil
	}
	if time.Now().After(entry.expiresAt) {
		return nil
	}
	return entry.events
}

func (c *HistoryCache) storeHistory(key string, events []HistoricalEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = historyCacheEntry{
		events:    cloneHistoricalEvents(events),
		expiresAt: time.Now().Add(c.ttl),
	}
}

// Purge mirrors Cache.Purge.
func (c *HistoryCache) Purge() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]historyCacheEntry)
}

// ---------------------------------------------------------------------------
// Cache-key construction
// ---------------------------------------------------------------------------

// forwardCacheKey is "fwd|market|sym1,sym2,sym3|horizon_days".
// Sorting the symbols means two calls with the same set in
// different order share an entry.
func forwardCacheKey(req FetchRequest) string {
	syms := normaliseSymbols(req.Symbols)
	sort.Strings(syms)
	return fmt.Sprintf("fwd|%s|%s|%d",
		strings.ToLower(strings.TrimSpace(req.Market)),
		strings.Join(syms, ","),
		req.HorizonDays,
	)
}

// historyCacheKey is "hist|market|sym1,sym2,sym3|lookback_days".
// Same shape and rationale as forwardCacheKey.
func historyCacheKey(req HistoryRequest) string {
	syms := normaliseSymbols(req.Symbols)
	sort.Strings(syms)
	return fmt.Sprintf("hist|%s|%s|%d",
		strings.ToLower(strings.TrimSpace(req.Market)),
		strings.Join(syms, ","),
		req.LookbackDays,
	)
}

// ---------------------------------------------------------------------------
// Clone helpers — the cache must never hand out a slice the
// caller can mutate.
// ---------------------------------------------------------------------------

func cloneEvents(in []Event) []Event {
	if len(in) == 0 {
		return nil
	}
	out := make([]Event, len(in))
	copy(out, in)
	return out
}

func cloneHistoricalEvents(in []HistoricalEvent) []HistoricalEvent {
	if len(in) == 0 {
		return nil
	}
	out := make([]HistoricalEvent, len(in))
	copy(out, in)
	return out
}
