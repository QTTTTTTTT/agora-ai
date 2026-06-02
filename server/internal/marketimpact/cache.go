// cache.go — in-memory snapshot of the instrument_liquidity
// table.
//
// Why a cache lives here
//
// matching.SlippageModel.FillPrice has no context.Context and
// is called on the hot order path. We can't synchronously hit
// the DB on every fill. The size of the calibration table is
// in the low thousands at most (one row per traded
// instrument), so a full snapshot is cheap to hold.
//
// Refresh strategy
//
//   - On startup: synchronous Refresh() so the engine doesn't
//     run with an empty cache.
//   - Periodically: a background goroutine calls Refresh()
//     every cfg.RefreshInterval (default 5m).
//   - On admin upsert/delete: callers invalidate via
//     ApplyChange so writes are visible immediately, without
//     waiting for the next refresh.
//
// Concurrency
//
// A copy-on-write map under sync.RWMutex. Lookups acquire
// RLock (cheap), refreshes swap the whole map under Lock.

package marketimpact

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// CacheConfig controls Cache lifecycle.
type CacheConfig struct {
	RefreshInterval time.Duration // default 5m; <=0 disables periodic refresh
	// Logger is called with a single error on refresh failures.
	// nil → silent.
	OnError func(error)
}

// Cache is an in-memory snapshot of all calibration rows,
// keyed by instrument_key (case-preserved).
type Cache struct {
	repo   *Repo
	cfg    CacheConfig
	mu     sync.RWMutex
	rows   map[string]Liquidity

	// stopCh is closed by Stop(); the background goroutine
	// exits when it observes the close.
	stopCh chan struct{}
	// running is bumped to 1 the first time Start runs and back
	// to 0 by Stop, so double-Start / double-Stop are no-ops.
	running int32
	// refreshedAt is updated after every successful Refresh.
	refreshedAt atomic.Pointer[time.Time]
}

// NewCache constructs a Cache. Repo may be nil for tests that
// drive the cache directly via SetRows.
func NewCache(repo *Repo, cfg CacheConfig) *Cache {
	if cfg.RefreshInterval == 0 {
		cfg.RefreshInterval = 5 * time.Minute
	}
	return &Cache{
		repo:   repo,
		cfg:    cfg,
		rows:   map[string]Liquidity{},
		stopCh: make(chan struct{}),
	}
}

// Start performs an initial Refresh and (if RefreshInterval >
// 0) launches the periodic refresher. Returns the initial
// Refresh's error, if any.
func (c *Cache) Start(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if !atomic.CompareAndSwapInt32(&c.running, 0, 1) {
		return nil
	}
	if err := c.Refresh(ctx); err != nil {
		// Refresh errors are returned to the caller but we still
		// start the loop — periodic refreshes may succeed once
		// the DB recovers.
		c.recordErr(err)
	}
	if c.cfg.RefreshInterval > 0 {
		go c.loop()
	}
	return nil
}

// Stop signals the periodic refresher to exit.
func (c *Cache) Stop() {
	if c == nil {
		return
	}
	if !atomic.CompareAndSwapInt32(&c.running, 1, 0) {
		return
	}
	close(c.stopCh)
}

// Refresh re-reads the whole table and atomically swaps the
// in-memory snapshot.
func (c *Cache) Refresh(ctx context.Context) error {
	if c == nil || c.repo == nil {
		return nil
	}
	rows, err := c.repo.ListAll(ctx)
	if err != nil {
		c.recordErr(err)
		return err
	}
	next := make(map[string]Liquidity, len(rows))
	for _, r := range rows {
		next[r.InstrumentKey] = r
	}
	c.mu.Lock()
	c.rows = next
	c.mu.Unlock()
	now := time.Now().UTC()
	c.refreshedAt.Store(&now)
	return nil
}

// Lookup returns the calibration row for an instrument (or nil
// if absent). Safe for concurrent use.
func (c *Cache) Lookup(key string) *Liquidity {
	if c == nil || key == "" {
		return nil
	}
	c.mu.RLock()
	row, ok := c.rows[key]
	c.mu.RUnlock()
	if !ok {
		return nil
	}
	out := row
	return &out
}

// ApplyChange merges a single row into the cache without
// requiring a full Refresh. Callers (e.g. the admin handler)
// call this after a successful Upsert/Delete so the engine
// sees the new value immediately.
//
// row==nil + key!="" → delete-from-cache.
func (c *Cache) ApplyChange(key string, row *Liquidity) {
	if c == nil || key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if row == nil {
		delete(c.rows, key)
		return
	}
	c.rows[key] = *row
}

// SetRows replaces the entire cache contents. Test-only.
func (c *Cache) SetRows(rows []Liquidity) {
	if c == nil {
		return
	}
	next := make(map[string]Liquidity, len(rows))
	for _, r := range rows {
		next[r.InstrumentKey] = r
	}
	c.mu.Lock()
	c.rows = next
	c.mu.Unlock()
	now := time.Now().UTC()
	c.refreshedAt.Store(&now)
}

// LastRefresh returns the time of the most recent successful
// Refresh. Zero time = never refreshed.
func (c *Cache) LastRefresh() time.Time {
	if c == nil {
		return time.Time{}
	}
	if v := c.refreshedAt.Load(); v != nil {
		return *v
	}
	return time.Time{}
}

// Size returns the number of cached rows. For metrics/admin.
func (c *Cache) Size() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.rows)
}

func (c *Cache) loop() {
	t := time.NewTicker(c.cfg.RefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_ = c.Refresh(ctx)
			cancel()
		}
	}
}

func (c *Cache) recordErr(err error) {
	if c == nil || c.cfg.OnError == nil || err == nil {
		return
	}
	c.cfg.OnError(err)
}
