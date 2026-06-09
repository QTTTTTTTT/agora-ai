// cache.go — TTL caches for each Provider surface. Different TTLs
// per surface because the underlying data refreshes at different
// cadences:
//
//   * Intraday snapshot — 60s default. Limit-up seal amount changes
//     fast during the bid; faster cadence than that is hammering.
//   * Dragon-Tiger list — 10m. Settles overnight; only the
//     "today vs yesterday" rotation matters intraday.
//   * Market regime — 60s. Used to gate every consult.
//   * Sector strength — 5m. Sector rankings shift slowly intraday.
//
// Operators can override every TTL via env knobs. The cache is
// per-provider-chain (one cache wraps the entire Registry).

package cnmarketstructure

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// CacheOptions tunes the TTL for each Provider surface. Zero means
// "use the package default".
type CacheOptions struct {
	IntradayTTL       time.Duration
	DragonTigerTTL    time.Duration
	MarketRegimeTTL   time.Duration
	SectorStrengthTTL time.Duration
}

func (o CacheOptions) intraday() time.Duration {
	if o.IntradayTTL > 0 {
		return o.IntradayTTL
	}
	return 60 * time.Second
}

func (o CacheOptions) dragonTiger() time.Duration {
	if o.DragonTigerTTL > 0 {
		return o.DragonTigerTTL
	}
	return 10 * time.Minute
}

func (o CacheOptions) marketRegime() time.Duration {
	if o.MarketRegimeTTL > 0 {
		return o.MarketRegimeTTL
	}
	return 60 * time.Second
}

func (o CacheOptions) sectorStrength() time.Duration {
	if o.SectorStrengthTTL > 0 {
		return o.SectorStrengthTTL
	}
	return 5 * time.Minute
}

// Cache wraps any Provider with per-surface TTL caching. Implements
// Provider too so callers can swap it in transparently.
type Cache struct {
	upstream Provider
	opts     CacheOptions

	mu              sync.RWMutex
	intradayEntries map[string]intradayEntry
	dragonEntries   map[string]dragonEntry
	regime          *regimeEntry
	sectors         map[int]sectorsEntry
}

type intradayEntry struct {
	value   IntradaySnapshot
	expires time.Time
}

type dragonEntry struct {
	value   []DragonTigerEntry
	expires time.Time
}

type regimeEntry struct {
	value   MarketRegime
	expires time.Time
}

type sectorsEntry struct {
	value   []SectorStrength
	expires time.Time
}

// NewCache wraps upstream with per-surface TTLs. nil upstream is
// permitted and turns every Fetch into ErrNotConfigured.
func NewCache(upstream Provider, opts CacheOptions) *Cache {
	return &Cache{
		upstream:        upstream,
		opts:            opts,
		intradayEntries: make(map[string]intradayEntry),
		dragonEntries:   make(map[string]dragonEntry),
		sectors:         make(map[int]sectorsEntry),
	}
}

// Name implements Provider.
func (c *Cache) Name() string {
	if c.upstream == nil {
		return "cnmarketstructure_cache_empty"
	}
	return c.upstream.Name() + "_cache"
}

// FetchIntraday returns the cached snapshot or refreshes it.
func (c *Cache) FetchIntraday(ctx context.Context, symbol string) (*IntradaySnapshot, error) {
	if c.upstream == nil {
		return nil, ErrNotConfigured
	}
	key := symbol
	if v := c.lookupIntraday(key); v != nil {
		clone := *v
		return &clone, nil
	}
	snap, err := c.upstream.FetchIntraday(ctx, symbol)
	if err != nil {
		return nil, err
	}
	if snap == nil {
		return nil, ErrNoData
	}
	c.storeIntraday(key, *snap)
	clone := *snap
	return &clone, nil
}

// FetchDragonTiger returns the cached entries or refreshes them.
func (c *Cache) FetchDragonTiger(ctx context.Context, symbol string, lookbackDays int) ([]DragonTigerEntry, error) {
	if c.upstream == nil {
		return nil, ErrNotConfigured
	}
	key := fmt.Sprintf("%s|%d", symbol, lookbackDays)
	if v := c.lookupDragon(key); v != nil {
		return append([]DragonTigerEntry(nil), v...), nil
	}
	entries, err := c.upstream.FetchDragonTiger(ctx, symbol, lookbackDays)
	if err != nil {
		return nil, err
	}
	c.storeDragon(key, entries)
	return append([]DragonTigerEntry(nil), entries...), nil
}

// FetchMarketRegime returns the cached snapshot or refreshes it.
func (c *Cache) FetchMarketRegime(ctx context.Context) (*MarketRegime, error) {
	if c.upstream == nil {
		return nil, ErrNotConfigured
	}
	if v := c.lookupRegime(); v != nil {
		clone := *v
		return &clone, nil
	}
	regime, err := c.upstream.FetchMarketRegime(ctx)
	if err != nil {
		return nil, err
	}
	if regime == nil {
		return nil, ErrNoData
	}
	c.storeRegime(*regime)
	clone := *regime
	return &clone, nil
}

// FetchSectorStrength returns the cached entries or refreshes them.
func (c *Cache) FetchSectorStrength(ctx context.Context, topN int) ([]SectorStrength, error) {
	if c.upstream == nil {
		return nil, ErrNotConfigured
	}
	if v := c.lookupSectors(topN); v != nil {
		return append([]SectorStrength(nil), v...), nil
	}
	sectors, err := c.upstream.FetchSectorStrength(ctx, topN)
	if err != nil {
		return nil, err
	}
	c.storeSectors(topN, sectors)
	return append([]SectorStrength(nil), sectors...), nil
}

// Purge invalidates every cached entry.
func (c *Cache) Purge() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.intradayEntries = make(map[string]intradayEntry)
	c.dragonEntries = make(map[string]dragonEntry)
	c.sectors = make(map[int]sectorsEntry)
	c.regime = nil
}

func (c *Cache) lookupIntraday(key string) *IntradaySnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.intradayEntries[key]
	if !ok || time.Now().After(entry.expires) {
		return nil
	}
	v := entry.value
	return &v
}

func (c *Cache) storeIntraday(key string, snap IntradaySnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.intradayEntries[key] = intradayEntry{value: snap, expires: time.Now().Add(c.opts.intraday())}
}

func (c *Cache) lookupDragon(key string) []DragonTigerEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.dragonEntries[key]
	if !ok || time.Now().After(entry.expires) {
		return nil
	}
	return entry.value
}

func (c *Cache) storeDragon(key string, entries []DragonTigerEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dragonEntries[key] = dragonEntry{value: append([]DragonTigerEntry(nil), entries...), expires: time.Now().Add(c.opts.dragonTiger())}
}

func (c *Cache) lookupRegime() *MarketRegime {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.regime == nil || time.Now().After(c.regime.expires) {
		return nil
	}
	v := c.regime.value
	return &v
}

func (c *Cache) storeRegime(regime MarketRegime) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.regime = &regimeEntry{value: regime, expires: time.Now().Add(c.opts.marketRegime())}
}

func (c *Cache) lookupSectors(topN int) []SectorStrength {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.sectors[topN]
	if !ok || time.Now().After(entry.expires) {
		return nil
	}
	return entry.value
}

func (c *Cache) storeSectors(topN int, sectors []SectorStrength) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sectors[topN] = sectorsEntry{value: append([]SectorStrength(nil), sectors...), expires: time.Now().Add(c.opts.sectorStrength())}
}
