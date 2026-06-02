package securitiesborrow

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestCache_LookupHitMiss(t *testing.T) {
	c := NewCache(CacheConfig{})
	c.SetRows([]BorrowRate{
		{InstrumentKey: "TSLA.US", BorrowRateBpsAnnual: 3000, Availability: AvailabilityHard},
	})
	if got := c.Lookup("TSLA.US"); got == nil || got.BorrowRateBpsAnnual != 3000 {
		t.Errorf("expected hit, got %+v", got)
	}
	if got := c.Lookup("missing"); got != nil {
		t.Errorf("expected miss")
	}
}

func TestCache_LookupCaseInsensitive(t *testing.T) {
	c := NewCache(CacheConfig{})
	c.SetRows([]BorrowRate{{InstrumentKey: "tsla.us", BorrowRateBpsAnnual: 50}})
	if c.Lookup("TSLA.US") == nil {
		t.Error("expected case-insensitive hit")
	}
}

func TestCache_LookupReturnsCopy(t *testing.T) {
	c := NewCache(CacheConfig{})
	c.SetRows([]BorrowRate{{InstrumentKey: "TSLA.US", BorrowRateBpsAnnual: 50}})
	got := c.Lookup("TSLA.US")
	got.BorrowRateBpsAnnual = 9999
	again := c.Lookup("TSLA.US")
	if again.BorrowRateBpsAnnual != 50 {
		t.Errorf("cache mutation leaked: %v", again.BorrowRateBpsAnnual)
	}
}

func TestCache_ApplyChange_Add(t *testing.T) {
	c := NewCache(CacheConfig{})
	c.ApplyChange("AMZN.US", &BorrowRate{InstrumentKey: "AMZN.US", BorrowRateBpsAnnual: 100})
	if c.Lookup("AMZN.US") == nil {
		t.Error("expected ApplyChange add")
	}
}

func TestCache_ApplyChange_Delete(t *testing.T) {
	c := NewCache(CacheConfig{})
	c.SetRows([]BorrowRate{{InstrumentKey: "TSLA.US"}})
	c.ApplyChange("TSLA.US", nil)
	if c.Lookup("TSLA.US") != nil {
		t.Error("expected delete")
	}
}

func TestCache_ConcurrentSafe(t *testing.T) {
	c := NewCache(CacheConfig{})
	c.SetRows([]BorrowRate{{InstrumentKey: "TSLA.US"}})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = c.Lookup("TSLA.US")
				c.ApplyChange("TSLA.US", &BorrowRate{InstrumentKey: "TSLA.US"})
			}
		}()
	}
	wg.Wait()
}

func TestCache_StartStop_Idempotent(t *testing.T) {
	c := NewCache(CacheConfig{RefreshInterval: 1 * time.Second})
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	c.Start(context.Background()) // idempotent
	c.Stop()
	c.Stop() // idempotent
}

func TestCache_LastRefresh_PopulatedAfterSet(t *testing.T) {
	c := NewCache(CacheConfig{})
	if !c.LastRefresh().IsZero() {
		t.Error("expected zero before any refresh")
	}
	c.SetRows([]BorrowRate{})
	if c.LastRefresh().IsZero() {
		t.Error("expected non-zero after SetRows")
	}
}

func TestCache_NilSafe(t *testing.T) {
	var c *Cache
	if c.Lookup("X") != nil {
		t.Error("nil cache must not return")
	}
	c.ApplyChange("X", &BorrowRate{}) // must not panic
	if c.Size() != 0 {
		t.Error("nil cache size must be 0")
	}
}
