package fundamental

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Registry routes FetchRequest by Market to the first Provider that
// claims support. Mirrors ohlc.Registry exactly — see that package
// for the design rationale.
type Registry struct {
	mu        sync.RWMutex
	providers []Provider
}

// NewRegistry constructs an empty registry.
func NewRegistry() *Registry { return &Registry{} }

// Register adds a provider, replacing any prior entry with the same
// Name() so config hot-reloads are well-defined.
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

// Fetch tries each registered provider for the request's market in
// order. ErrNoData from a provider triggers fallthrough; other
// errors are logged and remembered as lastErr so the caller has
// something to report when no provider succeeded.
func (r *Registry) Fetch(ctx context.Context, req FetchRequest) (*Metrics, error) {
	req = req.Normalize()
	r.mu.RLock()
	providers := append([]Provider(nil), r.providers...)
	r.mu.RUnlock()

	matched := false
	var lastErr error
	for _, p := range providers {
		if !p.Supports(req.Market) {
			continue
		}
		matched = true
		m, err := p.Fetch(ctx, req)
		if err == nil && m != nil {
			return m, nil
		}
		if err != nil && !errors.Is(err, ErrNoData) {
			slog.Warn("fundamental provider error",
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

// Cache wraps any Fetcher with TTL caching. Default suggestion: 24h
// (fundamentals refresh quarterly upstream, so even 24h keeps the
// data fresh enough for daily decisions while keeping outbound
// traffic minimal).
//
// Zero TTL disables caching; negative is clamped to zero.
type Cache struct {
	upstream Fetcher
	ttl      time.Duration

	mu      sync.RWMutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	value     *Metrics
	expiresAt time.Time
}

// NewCache constructs a TTL cache. Default TTL when ttl<=0 is 24h
// in production deployments — callers pass that explicitly.
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

// Fetch returns the cached entry when fresh, otherwise calls the
// upstream and stores the result. Upstream errors (including
// ErrNoData / ErrNoProvider) propagate unchanged — callers see
// identical degradation behaviour with or without caching.
func (c *Cache) Fetch(ctx context.Context, req FetchRequest) (*Metrics, error) {
	if c == nil || c.upstream == nil {
		return nil, ErrNoData
	}
	if c.ttl == 0 {
		return c.upstream.Fetch(ctx, req)
	}
	key := req.CacheKey()
	if v := c.lookup(key); v != nil {
		clone := *v
		return &clone, nil
	}
	m, err := c.upstream.Fetch(ctx, req)
	if err != nil {
		return nil, err
	}
	c.store(key, m)
	return m, nil
}

func (c *Cache) lookup(key string) *Metrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil
	}
	if time.Now().After(entry.expiresAt) {
		return nil
	}
	return entry.value
}

func (c *Cache) store(key string, m *Metrics) {
	if m == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	clone := *m
	c.entries[key] = cacheEntry{
		value:     &clone,
		expiresAt: time.Now().Add(c.ttl),
	}
}

// Purge clears every cached entry. Useful when an operator reloads
// provider config and wants to force a refresh.
func (c *Cache) Purge() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]cacheEntry)
}
