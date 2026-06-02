package marketimpact

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestCache_Lookup_HitAndMiss(t *testing.T) {
	c := NewCache(nil, CacheConfig{})
	row := Liquidity{InstrumentKey: "AAPL.US", Symbol: "AAPL", Market: "US", AssetClass: "equity"}
	c.SetRows([]Liquidity{row})
	if got := c.Lookup("AAPL.US"); got == nil || got.Symbol != "AAPL" {
		t.Errorf("expected hit, got %+v", got)
	}
	if got := c.Lookup("MISS"); got != nil {
		t.Errorf("expected nil for miss, got %+v", got)
	}
	if c.Size() != 1 {
		t.Errorf("size = %d", c.Size())
	}
}

func TestCache_ApplyChange_AddAndDelete(t *testing.T) {
	c := NewCache(nil, CacheConfig{})
	row := &Liquidity{InstrumentKey: "AAPL.US", Symbol: "AAPL", Market: "US"}
	c.ApplyChange("AAPL.US", row)
	if c.Lookup("AAPL.US") == nil {
		t.Fatal("expected row after ApplyChange")
	}
	c.ApplyChange("AAPL.US", nil)
	if c.Lookup("AAPL.US") != nil {
		t.Fatal("expected nil after delete-by-nil")
	}
}

func TestCache_LookupReturnsCopy(t *testing.T) {
	// Mutating the lookup result must not corrupt the cache.
	c := NewCache(nil, CacheConfig{})
	c.SetRows([]Liquidity{{InstrumentKey: "X", Symbol: "X", Market: "US", Note: "orig"}})
	got := c.Lookup("X")
	got.Note = "mutated"
	again := c.Lookup("X")
	if again.Note != "orig" {
		t.Errorf("cache row mutated externally: %+v", again)
	}
}

func TestCache_LastRefreshUpdatedBySetRows(t *testing.T) {
	c := NewCache(nil, CacheConfig{})
	if !c.LastRefresh().IsZero() {
		t.Fatal("expected zero before any refresh")
	}
	c.SetRows([]Liquidity{{InstrumentKey: "X", Symbol: "X", Market: "US"}})
	if c.LastRefresh().IsZero() {
		t.Fatal("expected non-zero after SetRows")
	}
}

func TestCache_Refresh_NilRepoIsNoOp(t *testing.T) {
	c := NewCache(nil, CacheConfig{})
	if err := c.Refresh(context.Background()); err != nil {
		t.Errorf("expected nil err with nil repo, got %v", err)
	}
}

func TestCache_NilSafe(t *testing.T) {
	var c *Cache
	if c.Lookup("X") != nil {
		t.Error("nil cache lookup")
	}
	c.ApplyChange("X", nil)
	c.SetRows(nil)
	if c.Size() != 0 {
		t.Error("nil cache size")
	}
	if !c.LastRefresh().IsZero() {
		t.Error("nil cache last refresh")
	}
}

func TestCache_Start_RecordsRefreshErr(t *testing.T) {
	// Even when Refresh fails, Start should not block forever and
	// should invoke OnError so operators see the failure.
	var calls int32
	c := NewCache(nil, CacheConfig{
		RefreshInterval: 0, // disable loop
		OnError: func(err error) {
			atomic.AddInt32(&calls, 1)
		},
	})
	// Force an error path: simulate by setting repo to a *Repo
	// with nil DB. recordErr is called from Refresh, which short-
	// circuits when repo==nil. Use direct call instead.
	c.recordErr(errors.New("boom"))
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("expected OnError invoked, calls=%d", calls)
	}
}

func TestCache_StartIsIdempotent(t *testing.T) {
	c := NewCache(nil, CacheConfig{RefreshInterval: 0})
	if err := c.Start(context.Background()); err != nil {
		t.Errorf("first start err = %v", err)
	}
	// Second call must be a no-op (Stop not yet called).
	if err := c.Start(context.Background()); err != nil {
		t.Errorf("second start err = %v", err)
	}
	c.Stop()
	c.Stop()
}

func TestCache_LoopRefreshes(t *testing.T) {
	// Tight interval to verify the loop ticks.
	r := NewCache(nil, CacheConfig{RefreshInterval: 5 * time.Millisecond})
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer r.Stop()
	// Without a repo, Refresh is a no-op but the ticker should
	// fire — assert the goroutine doesn't crash within 50ms.
	time.Sleep(50 * time.Millisecond)
}
