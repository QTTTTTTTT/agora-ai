package sectorflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Registry routes a sectorflow FetchRequest by market.
type Registry struct {
	mu        sync.RWMutex
	providers []Provider
}

func NewRegistry() *Registry { return &Registry{} }

// Register adds / replaces a provider by Name().
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

// Fetch tries each provider in registration order, falling through
// on ErrNoData. Result is sorted best→worst by Return1d so the
// formatter can take TopN / BottomN slices cheaply.
func (r *Registry) Fetch(ctx context.Context, req FetchRequest) (*Snapshot, error) {
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
		snap, err := p.Fetch(ctx, req)
		if err == nil && snap != nil && len(snap.Sectors) > 0 {
			sortSectorsByReturn(snap.Sectors)
			return snap, nil
		}
		if err != nil && !errors.Is(err, ErrNoData) {
			slog.Warn("sectorflow provider error",
				"provider", p.Name(),
				"market", req.Market,
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

// sortSectorsByReturn sorts in-place by Return1d desc, treating
// missing values as neutral so they trail the populated ones.
func sortSectorsByReturn(sectors []Sector) {
	sort.SliceStable(sectors, func(i, j int) bool {
		return sectors[i].Return1d > sectors[j].Return1d
	})
}

// Cache wraps any Fetcher with TTL caching. Recommend 5 minutes
// (sector rotation drifts intraday; longer staleness defeats the
// signal).
type Cache struct {
	upstream Fetcher
	ttl      time.Duration

	mu      sync.RWMutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	value     *Snapshot
	expiresAt time.Time
}

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

func (c *Cache) Fetch(ctx context.Context, req FetchRequest) (*Snapshot, error) {
	if c == nil || c.upstream == nil {
		return nil, ErrNoData
	}
	if c.ttl == 0 {
		return c.upstream.Fetch(ctx, req)
	}
	key := req.CacheKey()
	if v := c.lookup(key); v != nil {
		clone := cloneSnapshot(v)
		return clone, nil
	}
	snap, err := c.upstream.Fetch(ctx, req)
	if err != nil {
		return nil, err
	}
	c.store(key, snap)
	return snap, nil
}

func (c *Cache) lookup(key string) *Snapshot {
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

func (c *Cache) store(key string, s *Snapshot) {
	if s == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = cacheEntry{
		value:     cloneSnapshot(s),
		expiresAt: time.Now().Add(c.ttl),
	}
}

// Purge clears all cached entries. Useful when an operator reloads
// provider config and wants fresh data.
func (c *Cache) Purge() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]cacheEntry)
}

func cloneSnapshot(s *Snapshot) *Snapshot {
	if s == nil {
		return nil
	}
	clone := *s
	if s.Sectors != nil {
		clone.Sectors = append([]Sector(nil), s.Sectors...)
	}
	return &clone
}
