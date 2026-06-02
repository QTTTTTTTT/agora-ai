// Package quotecache is the last-tick-per-symbol cache that
// sits between the wsfeed manager and the hot-path readers
// (broker simulator, position refresher, stop-trigger poller).
//
// Why a separate package
//
//   - The wsfeed.Manager fans out raw Tick events; it knows
//     nothing about "what's the latest price for AAPL". The
//     cache is the projection from event stream → key/value
//     snapshot.
//   - The broker reads quotes synchronously on every fill;
//     it can't await an async Tick. The cache makes the
//     read O(1) without a DB roundtrip.
//   - Readers want a stable shape (Last/Bid/Ask) regardless
//     of whether the latest tick was a trade or a quote.
//     The cache merges adjacent ticks into a single snapshot.
//
// What it does NOT do
//
//   - It doesn't fall back to REST. That's the wiring layer's
//     job — broker calls cache.Lookup first, then the existing
//     marketdata.Service if Stale or Miss.
//   - It doesn't persist. Today there's no replay; a process
//     restart starts cold.

package quotecache

import (
	"strings"
	"sync"
	"time"
)

// Snapshot is what readers get back. All fields may be zero
// when no tick has arrived yet.
type Snapshot struct {
	Symbol        string
	DisplaySymbol string
	Market        string
	Provider      string
	Last          float64
	Bid           float64
	Ask           float64
	Volume        float64
	// AsOf is the tick's upstream Timestamp (preferred) or
	// ReceivedAt if Timestamp was zero. Used by callers to
	// decide if the snapshot is fresh enough for their use.
	AsOf       time.Time
	ReceivedAt time.Time
	// LastUpdateKind reports which event populated this
	// snapshot most recently. Useful for debugging "why is
	// my bid stale".
	LastUpdateKind string // "trade" | "quote" | "snapshot"
}

// IsZero reports whether the snapshot carries no usable
// pricing data.
func (s Snapshot) IsZero() bool {
	return s.Last == 0 && s.Bid == 0 && s.Ask == 0
}

// Config is the constructor input.
type Config struct {
	// StaleAfter — how long since AsOf before a Snapshot is
	// considered stale. Readers may still use the snapshot;
	// the cache just reports IsStale=true so they can choose
	// to fall back to a fresher source.
	StaleAfter time.Duration
	// MaxEntries — soft cap on the number of cached symbols.
	// When exceeded the cache evicts the least-recently-
	// touched entry. 0 = unbounded.
	MaxEntries int
	// Now is the time source. nil → time.Now.
	Now func() time.Time
}

// Cache is the symbol → Snapshot store.
type Cache struct {
	cfg Config

	mu      sync.RWMutex
	entries map[string]*entry

	hits    uint64
	misses  uint64
	stales  uint64
	evicts  uint64
}

type entry struct {
	snap     Snapshot
	touched  time.Time
}

// New returns a fresh Cache.
func New(cfg Config) *Cache {
	if cfg.StaleAfter < 0 {
		cfg.StaleAfter = 0
	}
	if cfg.StaleAfter == 0 {
		cfg.StaleAfter = 10 * time.Second
	}
	if cfg.MaxEntries < 0 {
		cfg.MaxEntries = 0
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Cache{
		cfg:     cfg,
		entries: make(map[string]*entry),
	}
}

// Tick is the input contract for Apply. Mirrors
// wsfeed.Tick by field but is duplicated here so this
// package has no compile dependency on wsfeed (avoids
// circular imports with consumers).
type Tick struct {
	Symbol        string
	DisplaySymbol string
	Market        string
	Provider      string
	EventKind     string // "trade" | "quote" | "snapshot" | "status"
	Last          float64
	Bid           float64
	Ask           float64
	Volume        float64
	Timestamp     time.Time
	ReceivedAt    time.Time
}

// Apply merges one Tick into the cache. Safe for concurrent
// callers but designed to be invoked from the wsfeed
// dispatcher's single goroutine, so contention is minimal.
//
// Merge semantics:
//
//   - "trade" updates Last + Volume; preserves Bid/Ask if
//     the new tick doesn't carry them.
//   - "quote" updates Bid/Ask; preserves Last if the new
//     tick is purely a quote.
//   - "snapshot" overwrites every populated field.
//   - "status" is a no-op for the cache (the wsfeed manager
//     handles trading halts separately).
//
// AsOf takes the new Timestamp if non-zero, else ReceivedAt.
// If both are zero we use the cache's Now() to avoid a
// "from 1970" snapshot.
func (c *Cache) Apply(t Tick) {
	if c == nil {
		return
	}
	sym := strings.ToUpper(strings.TrimSpace(t.Symbol))
	if sym == "" {
		return
	}
	if t.EventKind == "status" {
		return
	}
	now := c.cfg.Now()
	asOf := t.Timestamp
	if asOf.IsZero() {
		asOf = t.ReceivedAt
	}
	if asOf.IsZero() {
		asOf = now
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[sym]
	if !ok {
		e = &entry{
			snap: Snapshot{
				Symbol:        sym,
				DisplaySymbol: pickStr(t.DisplaySymbol, sym),
				Market:        t.Market,
				Provider:      t.Provider,
			},
			// Seed touched so the LRU sweep below doesn't
			// pick the brand-new entry as its own victim.
			touched: now,
		}
		c.entries[sym] = e
		c.evictIfNeeded(sym)
	}
	// Always keep the latest display/provider/market labels
	// so the snapshot reflects whichever provider is
	// currently authoritative.
	if t.DisplaySymbol != "" {
		e.snap.DisplaySymbol = t.DisplaySymbol
	}
	if t.Market != "" {
		e.snap.Market = t.Market
	}
	if t.Provider != "" {
		e.snap.Provider = t.Provider
	}
	switch t.EventKind {
	case "trade":
		if t.Last > 0 {
			e.snap.Last = t.Last
		}
		if t.Volume > 0 {
			e.snap.Volume = t.Volume
		}
		if t.Bid > 0 {
			e.snap.Bid = t.Bid
		}
		if t.Ask > 0 {
			e.snap.Ask = t.Ask
		}
	case "quote":
		if t.Bid > 0 {
			e.snap.Bid = t.Bid
		}
		if t.Ask > 0 {
			e.snap.Ask = t.Ask
		}
		if t.Last > 0 {
			e.snap.Last = t.Last
		}
	case "snapshot":
		if t.Last > 0 {
			e.snap.Last = t.Last
		}
		if t.Bid > 0 {
			e.snap.Bid = t.Bid
		}
		if t.Ask > 0 {
			e.snap.Ask = t.Ask
		}
		if t.Volume > 0 {
			e.snap.Volume = t.Volume
		}
	default:
		// Unknown kind: treat as a trade for safety so the
		// cache still advances rather than silently dropping
		// data.
		if t.Last > 0 {
			e.snap.Last = t.Last
		}
	}
	e.snap.AsOf = asOf
	e.snap.ReceivedAt = pickTime(t.ReceivedAt, now)
	e.snap.LastUpdateKind = t.EventKind
	e.touched = now
}

// Lookup returns the cached snapshot. The bool indicates
// presence (true if the symbol has ever received a tick).
// IsStale is true when AsOf is older than cfg.StaleAfter at
// the time of the call.
func (c *Cache) Lookup(symbol string) (Snapshot, bool, bool) {
	if c == nil {
		return Snapshot{}, false, false
	}
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	if sym == "" {
		return Snapshot{}, false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[sym]
	if !ok {
		c.misses++
		return Snapshot{}, false, false
	}
	stale := false
	if c.cfg.StaleAfter > 0 && !e.snap.AsOf.IsZero() {
		stale = c.cfg.Now().Sub(e.snap.AsOf) > c.cfg.StaleAfter
	}
	if stale {
		c.stales++
	} else {
		c.hits++
	}
	e.touched = c.cfg.Now()
	out := e.snap
	return out, true, stale
}

// Delete drops the cache entry for a symbol. Used by the
// wiring layer when a symbol unsubscribes and we don't want
// to serve a stale "last known good" forever.
func (c *Cache) Delete(symbol string) {
	if c == nil {
		return
	}
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	if sym == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, sym)
}

// Stats is the observability snapshot.
type Stats struct {
	Symbols int
	Hits    uint64
	Misses  uint64
	Stales  uint64
	Evicts  uint64
}

// Stats returns counters since process start.
func (c *Cache) Stats() Stats {
	if c == nil {
		return Stats{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return Stats{
		Symbols: len(c.entries),
		Hits:    c.hits,
		Misses:  c.misses,
		Stales:  c.stales,
		Evicts:  c.evicts,
	}
}

// SnapshotAll returns a copy of every cached entry. Used by
// the admin endpoint and tests.
func (c *Cache) SnapshotAll() []Snapshot {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Snapshot, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e.snap)
	}
	return out
}

// evictIfNeeded enforces MaxEntries by dropping the LRU
// entry. Caller must hold c.mu (write). The `protect`
// argument names the symbol that must never be chosen as
// the victim (the one we just inserted).
func (c *Cache) evictIfNeeded(protect string) {
	if c.cfg.MaxEntries <= 0 || len(c.entries) <= c.cfg.MaxEntries {
		return
	}
	var (
		victimSym string
		victimAt  time.Time
		first     = true
	)
	for sym, e := range c.entries {
		if sym == protect {
			continue
		}
		if first || e.touched.Before(victimAt) {
			victimSym = sym
			victimAt = e.touched
			first = false
		}
	}
	if victimSym != "" {
		delete(c.entries, victimSym)
		c.evicts++
	}
}

func pickStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func pickTime(a, b time.Time) time.Time {
	if !a.IsZero() {
		return a
	}
	return b
}
