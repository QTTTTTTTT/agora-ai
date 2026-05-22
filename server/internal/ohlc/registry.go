package ohlc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Registry routes FetchRequest by Market to the first Provider that
// claims support. Providers are tried in registration order, so the
// wiring layer can put preferred sources first (e.g., Yahoo before
// a slower Akshare fallback for US equities).
//
// Registry methods are safe for concurrent use.
type Registry struct {
	mu        sync.RWMutex
	providers []Provider
}

// NewRegistry constructs an empty registry. Wire providers in via
// Register; the registry is intentionally separate from the cache
// so tests can run providers raw (no TTL surprises).
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds a provider to the chain. Idempotent on the Name():
// re-registering a provider with the same name replaces the prior
// instance so hot-reloading config is well-defined.
func (r *Registry) Register(p Provider) {
	if p == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, existing := range r.providers {
		if existing.Name() == p.Name() {
			r.providers[i] = p
			return
		}
	}
	r.providers = append(r.providers, p)
}

// Fetch tries each registered provider in order for the request's
// market. A provider returning ErrNoData (or any non-nil error) is
// logged and the next is tried. ErrNoProvider is returned when no
// provider claimed the market; ErrNoData is returned when every
// matching provider returned empty.
func (r *Registry) Fetch(ctx context.Context, req FetchRequest) ([]Bar, error) {
	req = req.Normalize()
	r.mu.RLock()
	providers := append([]Provider(nil), r.providers...)
	r.mu.RUnlock()

	var matched bool
	var lastErr error
	for _, p := range providers {
		if !p.Supports(req.Market) {
			continue
		}
		matched = true
		bars, err := p.Fetch(ctx, req)
		if err == nil && len(bars) > 0 {
			return bars, nil
		}
		if err != nil && !errors.Is(err, ErrNoData) {
			slog.Warn("ohlc provider error",
				"provider", p.Name(),
				"market", req.Market,
				"symbol", req.Symbol,
				"err", err,
			)
			lastErr = err
		}
	}
	if !matched {
		return nil, fmt.Errorf("%w: %s", ErrNoProvider, req.Market)
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, ErrNoData
}

// Cache is a TTL-keyed in-memory cache wrapping any Fetcher. It is
// safe for concurrent use. The TTL is applied at the bucket
// granularity used by FetchRequest.CacheKey: two requests within
// the same TTL window for the same (symbol, market, interval) share
// the same key and return the cached bars.
//
// A zero TTL disables caching (every call hits the underlying
// fetcher). A negative TTL is treated as zero.
type Cache struct {
	upstream Fetcher
	ttl      time.Duration

	mu      sync.RWMutex
	entries map[string]cacheEntry
}

// Fetcher is the minimal interface Cache wraps. Both Registry and
// Provider satisfy it directly, so the cache can sit in front of
// either a single source or the full chain.
type Fetcher interface {
	Fetch(ctx context.Context, req FetchRequest) ([]Bar, error)
}

type cacheEntry struct {
	bars      []Bar
	expiresAt time.Time
}

// NewCache wraps upstream with TTL caching. Recommended TTLs:
//
//   - 15 minutes for daily bars during the trading day.
//   - 1 hour for daily bars outside trading hours.
//   - 30 seconds for intraday bars when a workflow runs back-to-back.
//
// The cache exposes no eviction beyond TTL — callers expecting a
// long-lived process should keep universes small (debate runs over
// the configured fund universe, typically <= 30 symbols).
func NewCache(upstream Fetcher, ttl time.Duration) *Cache {
	if ttl < 0 {
		ttl = 0
	}
	return &Cache{
		upstream: upstream,
		ttl:      ttl,
		entries:  make(map[string]cacheEntry),
	}
}

// Fetch first consults the cache, then falls through to the upstream
// fetcher and stores a successful result. Cache misses propagate the
// upstream error unchanged (including ErrNoData / ErrNoProvider) so
// the caller's degradation path remains the same as without caching.
func (c *Cache) Fetch(ctx context.Context, req FetchRequest) ([]Bar, error) {
	if c == nil || c.upstream == nil {
		return nil, ErrNoData
	}
	if c.ttl == 0 {
		return c.upstream.Fetch(ctx, req)
	}
	key := req.CacheKey(c.ttl)
	if bars, ok := c.lookup(key); ok {
		return bars, nil
	}
	bars, err := c.upstream.Fetch(ctx, req)
	if err != nil {
		return nil, err
	}
	c.store(key, bars)
	return bars, nil
}

func (c *Cache) lookup(key string) ([]Bar, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expiresAt) {
		return nil, false
	}
	out := make([]Bar, len(entry.bars))
	copy(out, entry.bars)
	return out, true
}

func (c *Cache) store(key string, bars []Bar) {
	c.mu.Lock()
	defer c.mu.Unlock()
	stored := make([]Bar, len(bars))
	copy(stored, bars)
	c.entries[key] = cacheEntry{
		bars:      stored,
		expiresAt: time.Now().Add(c.ttl),
	}
}

// Purge wipes every cached entry. Useful when a config reload
// changes the provider chain and the operator wants to flush stale
// data; production deployments mostly rely on TTL expiry.
func (c *Cache) Purge() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]cacheEntry)
}
