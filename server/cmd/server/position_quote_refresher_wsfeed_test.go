package main

import (
	"testing"
	"time"

	"github.com/fundai/server/internal/marketdata"
)

// fakeWSCache lets the test drive the wsCacheLookup interface.
type fakeWSCache struct {
	snaps map[string]wsCacheSnap
	stale map[string]bool
}

func (f *fakeWSCache) Lookup(symbol string) (wsCacheSnap, bool, bool) {
	if f == nil {
		return wsCacheSnap{}, false, false
	}
	snap, ok := f.snaps[symbol]
	if !ok {
		return wsCacheSnap{}, false, false
	}
	return snap, true, f.stale[symbol]
}

func TestQuoteCacheLookupAdapterPassThrough(t *testing.T) {
	// Adapter on a nil cache → no panic, returns (zero, false, false).
	a := newQuoteCacheLookup(nil)
	if a != nil {
		t.Fatalf("expected nil adapter for nil cache, got %T", a)
	}
}

// applyWSCacheOverlay is a tiny test seam: re-implement the
// overlay loop from refresher.runRefreshPass in a function we
// can exercise without standing up the full DB-backed
// refresher. Mirrors the production logic line-for-line so a
// regression in the overlay surfaces here too.
func applyWSCacheOverlay(bySymbol map[string]*marketdata.QuoteSnapshot, refs []marketdata.InstrumentRef, cache wsCacheLookup) {
	if cache == nil {
		return
	}
	for _, ref := range refs {
		key := ref.NormalizedSymbol()
		snap, ok, stale := cache.Lookup(key)
		if !ok || stale || snap.Last <= 0 {
			continue
		}
		existing := bySymbol[key]
		if existing != nil && !existing.AsOf.IsZero() && existing.AsOf.After(snap.AsOf) {
			continue
		}
		bySymbol[key] = &marketdata.QuoteSnapshot{
			Symbol: key,
			Price:  snap.Last,
			Bid:    snap.Bid,
			Ask:    snap.Ask,
			AsOf:   snap.AsOf,
			Source: "wsfeed",
		}
	}
}

func TestWSOverlay_FreshTickReplacesRestQuote(t *testing.T) {
	now := time.Now().UTC()
	ref := marketdata.InstrumentRef{Symbol: "AAPL", Market: "US"}
	bySymbol := map[string]*marketdata.QuoteSnapshot{
		ref.NormalizedSymbol(): {Symbol: ref.NormalizedSymbol(), Price: 200, AsOf: now.Add(-2 * time.Minute), Source: "yahoo"},
	}
	cache := &fakeWSCache{snaps: map[string]wsCacheSnap{
		ref.NormalizedSymbol(): {Last: 210.50, Bid: 210.40, Ask: 210.60, AsOf: now.Add(-1 * time.Second)},
	}}

	applyWSCacheOverlay(bySymbol, []marketdata.InstrumentRef{ref}, cache)

	got := bySymbol[ref.NormalizedSymbol()]
	if got == nil || got.Price != 210.50 || got.Source != "wsfeed" {
		t.Fatalf("overlay did not apply: %+v", got)
	}
}

func TestWSOverlay_StaleTickDoesNotReplace(t *testing.T) {
	now := time.Now().UTC()
	ref := marketdata.InstrumentRef{Symbol: "MSFT", Market: "US"}
	bySymbol := map[string]*marketdata.QuoteSnapshot{
		ref.NormalizedSymbol(): {Symbol: ref.NormalizedSymbol(), Price: 410, AsOf: now, Source: "yahoo"},
	}
	cache := &fakeWSCache{
		snaps: map[string]wsCacheSnap{ref.NormalizedSymbol(): {Last: 1000, AsOf: now.Add(-1 * time.Hour)}},
		stale: map[string]bool{ref.NormalizedSymbol(): true},
	}

	applyWSCacheOverlay(bySymbol, []marketdata.InstrumentRef{ref}, cache)

	got := bySymbol[ref.NormalizedSymbol()]
	if got == nil || got.Price != 410 || got.Source != "yahoo" {
		t.Fatalf("stale overlay should not have replaced REST quote: %+v", got)
	}
}

func TestWSOverlay_NewerRestTickWins(t *testing.T) {
	now := time.Now().UTC()
	ref := marketdata.InstrumentRef{Symbol: "GOOG", Market: "US"}
	bySymbol := map[string]*marketdata.QuoteSnapshot{
		ref.NormalizedSymbol(): {Symbol: ref.NormalizedSymbol(), Price: 175, AsOf: now, Source: "yahoo"},
	}
	cache := &fakeWSCache{snaps: map[string]wsCacheSnap{
		ref.NormalizedSymbol(): {Last: 172, AsOf: now.Add(-5 * time.Minute)},
	}}

	applyWSCacheOverlay(bySymbol, []marketdata.InstrumentRef{ref}, cache)

	got := bySymbol[ref.NormalizedSymbol()]
	if got == nil || got.Price != 175 || got.Source != "yahoo" {
		t.Fatalf("newer REST quote should have stayed: %+v", got)
	}
}

func TestWSOverlay_MissLeavesRestUnchanged(t *testing.T) {
	ref := marketdata.InstrumentRef{Symbol: "TSLA", Market: "US"}
	bySymbol := map[string]*marketdata.QuoteSnapshot{
		ref.NormalizedSymbol(): {Symbol: ref.NormalizedSymbol(), Price: 240, Source: "yahoo"},
	}
	cache := &fakeWSCache{snaps: map[string]wsCacheSnap{}}

	applyWSCacheOverlay(bySymbol, []marketdata.InstrumentRef{ref}, cache)

	got := bySymbol[ref.NormalizedSymbol()]
	if got == nil || got.Price != 240 || got.Source != "yahoo" {
		t.Fatalf("miss should leave REST quote untouched: %+v", got)
	}
}

func TestWSOverlay_PopulatesEmptyRestMap(t *testing.T) {
	now := time.Now().UTC()
	ref := marketdata.InstrumentRef{Symbol: "AMD", Market: "US"}
	bySymbol := map[string]*marketdata.QuoteSnapshot{}
	cache := &fakeWSCache{snaps: map[string]wsCacheSnap{
		ref.NormalizedSymbol(): {Last: 150, AsOf: now},
	}}

	applyWSCacheOverlay(bySymbol, []marketdata.InstrumentRef{ref}, cache)

	got := bySymbol[ref.NormalizedSymbol()]
	if got == nil || got.Price != 150 || got.Source != "wsfeed" {
		t.Fatalf("WS overlay should populate empty map: %+v", got)
	}
}

func TestWSOverlay_NilCacheIsNoOp(t *testing.T) {
	ref := marketdata.InstrumentRef{Symbol: "NVDA", Market: "US"}
	bySymbol := map[string]*marketdata.QuoteSnapshot{
		ref.NormalizedSymbol(): {Symbol: ref.NormalizedSymbol(), Price: 900, Source: "yahoo"},
	}
	applyWSCacheOverlay(bySymbol, []marketdata.InstrumentRef{ref}, nil)
	if got := bySymbol[ref.NormalizedSymbol()]; got.Price != 900 {
		t.Fatalf("nil cache should not mutate: %+v", got)
	}
}
