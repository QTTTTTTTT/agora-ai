package marketdata

import (
	"sync"
	"testing"
	"time"
)

func TestCryptoTickerCacheRoundTrip(t *testing.T) {
	cache := newCryptoTickerCache()
	now := time.Now().UTC()
	cache.Put("btcusdt", &QuoteSnapshot{Symbol: "BTCUSDT", Price: 67000, AsOf: now, Source: "binance-ws"})

	got, ok := cache.Get("BTCUSDT", now, time.Minute)
	if !ok {
		t.Fatalf("expected cache hit")
	}
	if got.Price != 67000 || got.Source != "binance-ws" {
		t.Fatalf("unexpected snapshot: %+v", got)
	}
}

func TestCryptoTickerCacheNormalisesDashAndCase(t *testing.T) {
	cache := newCryptoTickerCache()
	now := time.Now().UTC()
	cache.Put("BTC-USD", &QuoteSnapshot{Symbol: "BTC-USD", Price: 67000, AsOf: now, Source: "coinbase-ws"})

	// Lookup with either dash form or join form should hit the same row;
	// case must not matter either.
	for _, lookup := range []string{"BTC-USD", "btc-usd", "BTCUSD", "btcusd"} {
		got, ok := cache.Get(lookup, now, time.Minute)
		if !ok {
			t.Fatalf("lookup %q: expected hit", lookup)
		}
		if got.Price != 67000 {
			t.Fatalf("lookup %q: expected price 67000, got %v", lookup, got.Price)
		}
	}
}

func TestCryptoTickerCacheStaleness(t *testing.T) {
	cache := newCryptoTickerCache()
	asOf := time.Now().UTC().Add(-2 * time.Minute)
	cache.Put("ETHUSDT", &QuoteSnapshot{Symbol: "ETHUSDT", Price: 3500, AsOf: asOf, Source: "binance-ws"})

	if _, ok := cache.Get("ETHUSDT", time.Now().UTC(), 30*time.Second); ok {
		t.Fatalf("expected stale miss")
	}
	// maxAge = 0 disables freshness check and should always hit.
	if _, ok := cache.Get("ETHUSDT", time.Now().UTC(), 0); !ok {
		t.Fatalf("expected hit when maxAge disabled")
	}
}

func TestCryptoTickerCacheMissReturnsFalse(t *testing.T) {
	cache := newCryptoTickerCache()
	if _, ok := cache.Get("NOPE", time.Now().UTC(), time.Minute); ok {
		t.Fatalf("expected miss for unseen symbol")
	}
	if _, ok := cache.Get("", time.Now().UTC(), time.Minute); ok {
		t.Fatalf("empty symbol should miss")
	}
}

func TestCryptoTickerCacheConcurrentAccess(t *testing.T) {
	cache := newCryptoTickerCache()
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			cache.Put("BTCUSDT", &QuoteSnapshot{Price: float64(i), AsOf: time.Now().UTC()})
		}(i)
		go func() {
			defer wg.Done()
			_, _ = cache.Get("BTCUSDT", time.Now().UTC(), time.Minute)
		}()
	}
	wg.Wait()
}

func TestCryptoTickerCacheReturnsCopy(t *testing.T) {
	cache := newCryptoTickerCache()
	now := time.Now().UTC()
	cache.Put("BTCUSDT", &QuoteSnapshot{Price: 100, AsOf: now})
	got, _ := cache.Get("BTCUSDT", now, time.Minute)
	got.Price = 999
	got2, _ := cache.Get("BTCUSDT", now, time.Minute)
	if got2.Price != 100 {
		t.Fatalf("cache returned aliased pointer; mutation leaked: %v", got2.Price)
	}
}
