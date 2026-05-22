package marketdata

import (
	"strings"
	"sync"
	"time"
)

// cryptoTickerCache is a small in-memory cache of the latest QuoteSnapshot per
// crypto pair symbol, populated asynchronously by a websocket stream. It is
// the shared core behind the Binance / Coinbase WS providers: a Quote() call
// just reads from the cache instead of issuing a REST request, which keeps
// decision latency in the millisecond range and avoids burning the
// CoinGecko free-tier rate budget.
//
// Symbol normalisation: keys are uppercase, whitespace-trimmed. Callers pass
// raw user/market symbols (e.g. "btcusdt" or "BTC-USDT") and the cache
// normalises both writes and reads.
//
// Concurrency: a sync.RWMutex guards the map. Writers (the WS goroutine) are
// strictly serialised; readers (Quote() handlers, observability snapshots)
// share the read lock. Returned snapshots are pointers to immutable copies
// so callers can safely mutate the returned struct without racing with the
// writer.
type cryptoTickerCache struct {
	mu    sync.RWMutex
	items map[string]*QuoteSnapshot
}

func newCryptoTickerCache() *cryptoTickerCache {
	return &cryptoTickerCache{items: make(map[string]*QuoteSnapshot, 32)}
}

// Put stores snap under the normalised symbol key. snap must be non-nil.
// The cache stores a copy so external mutation of the input is safe.
func (c *cryptoTickerCache) Put(symbol string, snap *QuoteSnapshot) {
	key := normalizeCryptoSymbol(symbol)
	if key == "" || snap == nil {
		return
	}
	clone := *snap
	c.mu.Lock()
	c.items[key] = &clone
	c.mu.Unlock()
}

// Get returns the latest snapshot for symbol when it is no older than
// maxAge. Returns (nil, false) when missing or stale so the quote chain can
// fall back to the next provider (CoinGecko, Yahoo) cleanly.
//
// maxAge <= 0 disables the freshness check and returns whatever is cached.
func (c *cryptoTickerCache) Get(symbol string, now time.Time, maxAge time.Duration) (*QuoteSnapshot, bool) {
	key := normalizeCryptoSymbol(symbol)
	if key == "" {
		return nil, false
	}
	c.mu.RLock()
	snap, ok := c.items[key]
	c.mu.RUnlock()
	if !ok || snap == nil {
		return nil, false
	}
	if maxAge > 0 && now.Sub(snap.AsOf) > maxAge {
		return nil, false
	}
	out := *snap
	return &out, true
}

// Snapshot returns a copy of the current cache, for observability /
// diagnostics. Keys are normalised symbols (uppercase).
func (c *cryptoTickerCache) Snapshot() map[string]QuoteSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]QuoteSnapshot, len(c.items))
	for k, v := range c.items {
		if v == nil {
			continue
		}
		out[k] = *v
	}
	return out
}

// Len returns the number of symbols currently cached. Cheap, intended for
// metrics / health endpoints.
func (c *cryptoTickerCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

// normalizeCryptoSymbol canonicalises a crypto pair symbol for cache lookup.
// It uppercases, trims whitespace, and strips a single optional dash
// separator ("BTC-USD" → "BTCUSD"). The dash form is what Coinbase uses
// natively; we normalise to the join form so a single cache can serve
// both Binance ("BTCUSDT") and Coinbase ("BTC-USD") writers and have
// callers look up either spelling.
func normalizeCryptoSymbol(symbol string) string {
	s := strings.ToUpper(strings.TrimSpace(symbol))
	if s == "" {
		return ""
	}
	return strings.ReplaceAll(s, "-", "")
}
