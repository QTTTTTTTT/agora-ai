package quotecache_test

import (
	"sync"
	"testing"
	"time"

	"github.com/fundai/server/internal/quotecache"
)

func TestApplyTradeUpdatesLastAndVolume(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	c := quotecache.New(quotecache.Config{Now: func() time.Time { return now }})
	c.Apply(quotecache.Tick{
		Symbol:    "AAPL",
		Provider:  "mock",
		EventKind: "trade",
		Last:      210.50,
		Volume:    1_000_000,
		Timestamp: now.Add(-time.Second),
	})
	snap, ok, stale := c.Lookup("aapl")
	if !ok {
		t.Fatalf("expected hit")
	}
	if stale {
		t.Fatalf("expected fresh, got stale")
	}
	if snap.Last != 210.50 || snap.Volume != 1_000_000 {
		t.Fatalf("unexpected snap: %+v", snap)
	}
	if snap.LastUpdateKind != "trade" {
		t.Fatalf("LastUpdateKind=%q", snap.LastUpdateKind)
	}
}

func TestApplyQuotePreservesLast(t *testing.T) {
	now := time.Now().UTC()
	c := quotecache.New(quotecache.Config{Now: func() time.Time { return now }})
	c.Apply(quotecache.Tick{Symbol: "AAPL", EventKind: "trade", Last: 210, Timestamp: now})
	c.Apply(quotecache.Tick{Symbol: "AAPL", EventKind: "quote", Bid: 209.95, Ask: 210.05, Timestamp: now})

	snap, ok, _ := c.Lookup("AAPL")
	if !ok {
		t.Fatalf("miss")
	}
	if snap.Last != 210 {
		t.Fatalf("Last should survive a quote-only tick, got %v", snap.Last)
	}
	if snap.Bid != 209.95 || snap.Ask != 210.05 {
		t.Fatalf("unexpected snap: %+v", snap)
	}
	if snap.LastUpdateKind != "quote" {
		t.Fatalf("LastUpdateKind should reflect latest update")
	}
}

func TestApplySnapshotOverwrites(t *testing.T) {
	now := time.Now().UTC()
	c := quotecache.New(quotecache.Config{Now: func() time.Time { return now }})
	c.Apply(quotecache.Tick{Symbol: "AAPL", EventKind: "trade", Last: 210, Timestamp: now})
	c.Apply(quotecache.Tick{Symbol: "AAPL", EventKind: "snapshot", Last: 215, Bid: 214.95, Ask: 215.05, Timestamp: now})
	snap, _, _ := c.Lookup("AAPL")
	if snap.Last != 215 || snap.Bid != 214.95 || snap.Ask != 215.05 {
		t.Fatalf("snapshot did not overwrite: %+v", snap)
	}
}

func TestLookupMissOnUnknownSymbol(t *testing.T) {
	c := quotecache.New(quotecache.Config{})
	if _, ok, _ := c.Lookup("MSFT"); ok {
		t.Fatalf("expected miss")
	}
	if got := c.Stats().Misses; got != 1 {
		t.Fatalf("Misses=%d, want 1", got)
	}
}

func TestStalenessByAsOf(t *testing.T) {
	now := time.Now().UTC()
	tick := time.Now()
	c := quotecache.New(quotecache.Config{
		StaleAfter: 5 * time.Second,
		Now:        func() time.Time { return tick },
	})
	tick = now
	c.Apply(quotecache.Tick{Symbol: "AAPL", EventKind: "trade", Last: 1, Timestamp: now.Add(-3 * time.Second)})
	_, _, stale := c.Lookup("AAPL")
	if stale {
		t.Fatalf("3s ago should be fresh under 5s window")
	}
	tick = now.Add(10 * time.Second)
	_, _, stale = c.Lookup("AAPL")
	if !stale {
		t.Fatalf("13s after AsOf should be stale under 5s window")
	}
}

func TestDeleteEvictsEntry(t *testing.T) {
	c := quotecache.New(quotecache.Config{})
	c.Apply(quotecache.Tick{Symbol: "AAPL", EventKind: "trade", Last: 1, Timestamp: time.Now()})
	c.Delete("AAPL")
	if _, ok, _ := c.Lookup("AAPL"); ok {
		t.Fatalf("expected miss after delete")
	}
}

func TestMaxEntriesLRUEviction(t *testing.T) {
	tick := time.Now()
	c := quotecache.New(quotecache.Config{
		MaxEntries: 2,
		Now:        func() time.Time { return tick },
	})
	tick = time.Unix(100, 0)
	c.Apply(quotecache.Tick{Symbol: "A", EventKind: "trade", Last: 1, Timestamp: tick})
	tick = time.Unix(200, 0)
	c.Apply(quotecache.Tick{Symbol: "B", EventKind: "trade", Last: 1, Timestamp: tick})
	tick = time.Unix(300, 0)
	c.Apply(quotecache.Tick{Symbol: "C", EventKind: "trade", Last: 1, Timestamp: tick})

	if _, ok, _ := c.Lookup("A"); ok {
		t.Fatalf("LRU entry A should have been evicted")
	}
	if _, ok, _ := c.Lookup("B"); !ok {
		t.Fatalf("B should still be in cache")
	}
	if _, ok, _ := c.Lookup("C"); !ok {
		t.Fatalf("C should still be in cache")
	}
	if got := c.Stats().Evicts; got == 0 {
		t.Fatalf("Evicts should be > 0")
	}
}

func TestConcurrentApplyAndLookup(t *testing.T) {
	c := quotecache.New(quotecache.Config{})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			c.Apply(quotecache.Tick{
				Symbol:    "AAPL",
				EventKind: "trade",
				Last:      210 + float64(i)*0.01,
				Timestamp: time.Now(),
			})
		}(i)
		go func() {
			defer wg.Done()
			_, _, _ = c.Lookup("AAPL")
		}()
	}
	wg.Wait()
}

func TestApplyEmptySymbolIgnored(t *testing.T) {
	c := quotecache.New(quotecache.Config{})
	c.Apply(quotecache.Tick{Symbol: "", EventKind: "trade", Last: 1, Timestamp: time.Now()})
	if got := c.Stats().Symbols; got != 0 {
		t.Fatalf("Symbols=%d, want 0", got)
	}
}

func TestApplyStatusIsNoOp(t *testing.T) {
	c := quotecache.New(quotecache.Config{})
	c.Apply(quotecache.Tick{Symbol: "AAPL", EventKind: "status", Last: 0, Timestamp: time.Now()})
	if got := c.Stats().Symbols; got != 0 {
		t.Fatalf("status should not create cache entries")
	}
}
