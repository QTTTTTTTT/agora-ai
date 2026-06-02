// cache.go — in-memory snapshot of security_borrow_rates.
//
// Why a cache
//
// The broker pre-trade gate runs synchronously inside
// Simulator.PlaceOrder. We want a O(1) lookup for "what's the
// borrow rate / availability for AAPL.US right now?" without a
// Postgres round-trip on every short order. The cache is
// refreshed periodically by a background goroutine, and admin
// writes call ApplyChange to install the fresh row immediately
// (so the operator never sees stale data after editing the
// table).
//
// Concurrency: a sync.RWMutex guards the map. Reads return
// **copies** so callers can mutate the returned struct without
// data races; writes acquire the write lock briefly.

package securitiesborrow

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// CacheConfig is the cache constructor input.
type CacheConfig struct {
	Repo            *Repo
	RefreshInterval time.Duration
	OnError         func(error)  // optional
}

// Cache is a thread-safe snapshot of security_borrow_rates
// keyed on instrument_key (uppercased).
type Cache struct {
	repo            *Repo
	refreshInterval time.Duration
	onError         func(error)

	mu    sync.RWMutex
	rates map[string]BorrowRate

	lastRefresh atomic.Pointer[time.Time]
	startedOnce sync.Once
	stopOnce    sync.Once
	stopCh      chan struct{}
	doneCh      chan struct{}
}

// NewCache constructs a cache. The repo is required for refresh
// to do anything; nil repo is allowed for tests using SetRows.
func NewCache(cfg CacheConfig) *Cache {
	c := &Cache{
		repo:            cfg.Repo,
		refreshInterval: cfg.RefreshInterval,
		onError:         cfg.OnError,
		rates:           make(map[string]BorrowRate),
		stopCh:          make(chan struct{}),
		doneCh:          make(chan struct{}),
	}
	if c.refreshInterval <= 0 {
		c.refreshInterval = 5 * time.Minute
	}
	return c
}

// Start runs an initial refresh and kicks off the background
// loop. Idempotent: subsequent calls are no-ops.
func (c *Cache) Start(ctx context.Context) error {
	if c == nil {
		return errors.New("securitiesborrow.Cache: nil receiver")
	}
	var firstErr error
	c.startedOnce.Do(func() {
		// Initial refresh — non-fatal if it errors, the loop will
		// retry. We surface the error to OnError so observability
		// shows the boot-time failure.
		if err := c.Refresh(ctx); err != nil {
			firstErr = err
			if c.onError != nil {
				c.onError(err)
			}
		}
		go c.loop(ctx)
	})
	return firstErr
}

// Stop signals the loop to exit and waits for it. Idempotent.
func (c *Cache) Stop() {
	if c == nil {
		return
	}
	c.stopOnce.Do(func() {
		close(c.stopCh)
		<-c.doneCh
	})
}

// loop is the background refresh goroutine.
func (c *Cache) loop(ctx context.Context) {
	defer close(c.doneCh)
	t := time.NewTicker(c.refreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-t.C:
			if err := c.Refresh(ctx); err != nil && c.onError != nil {
				c.onError(err)
			}
		}
	}
}

// Refresh re-reads the table and installs the snapshot. Safe to
// call concurrently with Lookup; the new map is built first
// then swapped under the write lock.
func (c *Cache) Refresh(ctx context.Context) error {
	if c == nil {
		return errors.New("securitiesborrow.Cache: nil receiver")
	}
	if c.repo == nil {
		// No repo wired (test setup): no-op.
		now := time.Now().UTC()
		c.lastRefresh.Store(&now)
		return nil
	}
	rows, err := c.repo.ListAllRates(ctx)
	if err != nil {
		return err
	}
	next := make(map[string]BorrowRate, len(rows))
	for _, row := range rows {
		next[strings.ToUpper(strings.TrimSpace(row.InstrumentKey))] = row
	}
	c.mu.Lock()
	c.rates = next
	c.mu.Unlock()
	now := time.Now().UTC()
	c.lastRefresh.Store(&now)
	return nil
}

// Lookup returns the rate for an instrument. Returns nil when
// missing; the caller decides whether to treat that as
// "no calibration → fail-open" or "no calibration → reject".
func (c *Cache) Lookup(instrumentKey string) *BorrowRate {
	if c == nil {
		return nil
	}
	key := strings.ToUpper(strings.TrimSpace(instrumentKey))
	if key == "" {
		return nil
	}
	c.mu.RLock()
	row, ok := c.rates[key]
	c.mu.RUnlock()
	if !ok {
		return nil
	}
	// Return by value to prevent callers from mutating the cache
	// map's BorrowRate. Pointer fields inside (AvailableShares
	// etc) are copied as-is — they are read-only in the gate path.
	clone := row
	return &clone
}

// ApplyChange installs or removes one row immediately after an
// admin write. row=nil → delete by instrumentKey.
func (c *Cache) ApplyChange(instrumentKey string, row *BorrowRate) {
	if c == nil {
		return
	}
	key := strings.ToUpper(strings.TrimSpace(instrumentKey))
	if key == "" {
		return
	}
	c.mu.Lock()
	if row == nil {
		delete(c.rates, key)
	} else {
		c.rates[key] = *row
	}
	c.mu.Unlock()
}

// SetRows replaces the whole snapshot. Test helper.
func (c *Cache) SetRows(rows []BorrowRate) {
	if c == nil {
		return
	}
	next := make(map[string]BorrowRate, len(rows))
	for _, row := range rows {
		next[strings.ToUpper(strings.TrimSpace(row.InstrumentKey))] = row
	}
	c.mu.Lock()
	c.rates = next
	c.mu.Unlock()
	now := time.Now().UTC()
	c.lastRefresh.Store(&now)
}

// LastRefresh returns the last time Refresh / SetRows
// completed. Returns zero time when neither has run.
func (c *Cache) LastRefresh() time.Time {
	if c == nil {
		return time.Time{}
	}
	p := c.lastRefresh.Load()
	if p == nil {
		return time.Time{}
	}
	return *p
}

// Size returns the current number of cached rows.
func (c *Cache) Size() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.rates)
}
