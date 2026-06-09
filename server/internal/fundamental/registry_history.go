package fundamental

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// HistoricalRegistry routes FetchHistory by Market to the first
// HistoricalProvider that claims support. Mirrors Registry exactly —
// see registry.go for the design rationale.
type HistoricalRegistry struct {
	mu        sync.RWMutex
	providers []HistoricalProvider
}

// NewHistoricalRegistry constructs an empty registry.
func NewHistoricalRegistry() *HistoricalRegistry { return &HistoricalRegistry{} }

// Register adds a provider, replacing any prior entry with the same
// Name() so config hot-reloads are well-defined.
func (r *HistoricalRegistry) Register(p HistoricalProvider) {
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

// FetchHistory tries each registered provider for the request's
// market in order. Empty-slice + nil from a provider triggers
// fallthrough; other errors are logged and remembered.
func (r *HistoricalRegistry) FetchHistory(ctx context.Context, req FetchRequest, lookbackYears int) ([]YearlyMetrics, error) {
	req = req.Normalize()
	r.mu.RLock()
	providers := append([]HistoricalProvider(nil), r.providers...)
	r.mu.RUnlock()

	matched := false
	var lastErr error
	for _, p := range providers {
		if !p.Supports(req.Market) {
			continue
		}
		matched = true
		series, err := p.FetchHistory(ctx, req, lookbackYears)
		if err == nil && len(series) > 0 {
			return series, nil
		}
		if err != nil && !errors.Is(err, ErrNoData) {
			slog.Warn("fundamental historical provider error",
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

// HistoricalCache wraps any HistoricalFetcher with TTL caching.
// Recommended default: 24h. Annual financials only change once
// per quarter at most so 24h is a comfortable lower bound.
type HistoricalCache struct {
	upstream HistoricalFetcher
	ttl      time.Duration

	mu      sync.RWMutex
	entries map[string]histCacheEntry
}

type histCacheEntry struct {
	value     []YearlyMetrics
	expiresAt time.Time
}

// NewHistoricalCache constructs the cache wrapper.
func NewHistoricalCache(upstream HistoricalFetcher, ttl time.Duration) *HistoricalCache {
	if ttl < 0 {
		ttl = 0
	}
	return &HistoricalCache{
		upstream: upstream,
		ttl:      ttl,
		entries:  make(map[string]histCacheEntry),
	}
}

// FetchHistory returns the cached entry when fresh, otherwise
// calls the upstream and stores the result. Cache key includes
// lookbackYears so changing the window invalidates correctly.
func (c *HistoricalCache) FetchHistory(ctx context.Context, req FetchRequest, lookbackYears int) ([]YearlyMetrics, error) {
	if c == nil || c.upstream == nil {
		return nil, ErrNoData
	}
	if c.ttl == 0 {
		return c.upstream.FetchHistory(ctx, req, lookbackYears)
	}
	key := req.CacheKey() + "|h" + itoa(lookbackYears)
	if v := c.lookup(key); v != nil {
		clone := append([]YearlyMetrics(nil), v...)
		return clone, nil
	}
	series, err := c.upstream.FetchHistory(ctx, req, lookbackYears)
	if err != nil {
		return nil, err
	}
	if len(series) > 0 {
		c.store(key, series)
	}
	return series, nil
}

func (c *HistoricalCache) lookup(key string) []YearlyMetrics {
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

func (c *HistoricalCache) store(key string, series []YearlyMetrics) {
	c.mu.Lock()
	defer c.mu.Unlock()
	clone := append([]YearlyMetrics(nil), series...)
	c.entries[key] = histCacheEntry{
		value:     clone,
		expiresAt: time.Now().Add(c.ttl),
	}
}

// Purge clears every cached entry.
func (c *HistoricalCache) Purge() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]histCacheEntry)
}

// EnrichWithHistory is the small composing helper the wiring layer
// uses to attach a multi-year history series to a fresh single-
// period Metrics snapshot. Best-effort: history failures don't
// fail the snapshot fetch (callers still get the snapshot fields).
//
// lookbackYears caps both the upstream call and the returned
// History slice — pass 10 for Buffett-grade requirements.
func EnrichWithHistory(
	ctx context.Context,
	snapshot *Metrics,
	hist HistoricalFetcher,
	req FetchRequest,
	lookbackYears int,
) {
	if snapshot == nil || hist == nil || lookbackYears <= 0 {
		return
	}
	series, err := hist.FetchHistory(ctx, req, lookbackYears)
	if err != nil {
		// Soft failure — the caller already has the snapshot.
		return
	}
	snapshot.History = series
}

// itoa is a stdlib-free int-to-string helper used by the cache
// key. Inlined here so the registry file stays free of fmt for
// hot-path code.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := [12]byte{}
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
