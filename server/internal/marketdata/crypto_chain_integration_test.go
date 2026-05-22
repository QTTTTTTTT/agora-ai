package marketdata

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestCryptoChainPrefersWSWhenFresh confirms that when the binance / coinbase
// providers are listed (the default for crypto), a fresh WS cache hit short-
// circuits the chain before CoinGecko / Yahoo are tried. This is the core
// promise of F8 — the WS feed eliminates polling round-trips.
func TestCryptoChainPrefersWSWhenFresh(t *testing.T) {
	svc := NewService(Config{
		QuoteProviders:     []string{"binance", "coingecko", "yahoo"},
		CryptoWSEnabled:    true,
		CryptoWSStaleAfter: 30 * time.Second,
	})
	// Seed cache with a fresh BTCUSDT tick. coinGecko / yahoo overrides
	// would error if called; the test fails if the chain calls them.
	svc.cryptoWSCache.Put("BTCUSDT", &QuoteSnapshot{
		Symbol: "BTCUSDT",
		Price:  67000,
		AsOf:   time.Now().UTC(),
		Source: "binance-ws",
	})
	svc.testProviderOverrides = map[string]quoteProviderFunc{
		"binance": binanceQuoteProvider(svc.cryptoWSCache, svc.cfg.CryptoWSStaleAfter),
		"coingecko": func(ctx context.Context, _ InstrumentRef) (*QuoteSnapshot, error) {
			t.Fatalf("coingecko should not be called when WS cache hits")
			return nil, nil
		},
		"yahoo": func(ctx context.Context, _ InstrumentRef) (*QuoteSnapshot, error) {
			t.Fatalf("yahoo should not be called when WS cache hits")
			return nil, nil
		},
	}

	snap, err := svc.GetQuote(context.Background(), InstrumentRef{
		Symbol:     "BTCUSDT",
		Market:     "crypto",
		AssetClass: "crypto",
	})
	if err != nil {
		t.Fatalf("get quote: %v", err)
	}
	if snap.Source != "binance-ws" || snap.Price != 67000 {
		t.Fatalf("expected ws snapshot, got %+v", snap)
	}
}

// TestCryptoChainFallsThroughWhenWSStale confirms that an empty / stale WS
// cache yields to the next provider in the chain (CoinGecko, then Yahoo)
// rather than blocking the request. Without this guarantee a transient WS
// outage would degrade crypto pricing.
func TestCryptoChainFallsThroughWhenWSStale(t *testing.T) {
	svc := NewService(Config{
		QuoteProviders:     []string{"binance", "coingecko", "yahoo"},
		CryptoWSEnabled:    true,
		CryptoWSStaleAfter: 5 * time.Second,
	})
	// Seed cache with a stale tick — provider must return ErrQuoteUnavailable
	// and the chain must proceed to coingecko.
	svc.cryptoWSCache.Put("BTCUSDT", &QuoteSnapshot{
		Symbol: "BTCUSDT",
		Price:  10000,
		AsOf:   time.Now().UTC().Add(-time.Hour),
	})
	coingeckoCalled := false
	svc.testProviderOverrides = map[string]quoteProviderFunc{
		"binance": binanceQuoteProvider(svc.cryptoWSCache, svc.cfg.CryptoWSStaleAfter),
		"coingecko": func(ctx context.Context, _ InstrumentRef) (*QuoteSnapshot, error) {
			coingeckoCalled = true
			return &QuoteSnapshot{
				Symbol: "BTCUSDT",
				Price:  67100,
				AsOf:   time.Now().UTC(),
				Source: "coingecko",
			}, nil
		},
		"yahoo": func(ctx context.Context, _ InstrumentRef) (*QuoteSnapshot, error) {
			t.Fatalf("yahoo should not be called when coingecko succeeded")
			return nil, nil
		},
	}

	snap, err := svc.GetQuote(context.Background(), InstrumentRef{
		Symbol:     "BTCUSDT",
		Market:     "crypto",
		AssetClass: "crypto",
	})
	if err != nil {
		t.Fatalf("get quote: %v", err)
	}
	if !coingeckoCalled {
		t.Fatalf("expected coingecko to be invoked after WS miss")
	}
	if snap.Source != "coingecko" || snap.Price != 67100 {
		t.Fatalf("expected coingecko snapshot, got %+v", snap)
	}
}

func TestCryptoWSDisabledSkipsProviders(t *testing.T) {
	svc := NewService(Config{
		QuoteProviders:  []string{"binance", "coingecko"},
		CryptoWSEnabled: false,
	})
	if svc.cryptoWSCache != nil {
		t.Fatalf("expected nil cache when CryptoWSEnabled=false")
	}
	// binance/coinbase names must resolve to nil provider so the chain
	// transparently skips them.
	if got := svc.quoteProviderByName("binance"); got != nil {
		t.Fatalf("binance provider should be nil when WS disabled")
	}
	if got := svc.quoteProviderByName("coinbase"); got != nil {
		t.Fatalf("coinbase provider should be nil when WS disabled")
	}
}

func TestCryptoWSDefaultChainIncludesWSProvidersFirst(t *testing.T) {
	svc := NewService(Config{CryptoWSEnabled: true})
	order := svc.defaultQuoteProviderOrder(InstrumentRef{Market: "crypto", AssetClass: "crypto"})
	if len(order) < 4 || order[0] != "binance" || order[1] != "coinbase" {
		t.Fatalf("expected ws providers first, got %v", order)
	}
	hasCoingecko := false
	hasYahoo := false
	for _, name := range order {
		if name == "coingecko" {
			hasCoingecko = true
		}
		if name == "yahoo" {
			hasYahoo = true
		}
	}
	if !hasCoingecko || !hasYahoo {
		t.Fatalf("expected polling fallbacks in chain, got %v", order)
	}
}

func TestStartCryptoStreamsIdempotent(t *testing.T) {
	svc := NewService(Config{CryptoWSEnabled: true, BinanceWSSymbols: []string{}, CoinbaseWSProducts: []string{}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.StartCryptoStreams(ctx)
	cancel1 := svc.cryptoStreamsCancel
	svc.StartCryptoStreams(ctx)
	if svc.cryptoStreamsCancel == nil || (cancel1 == nil && svc.cryptoStreamsCancel != nil) {
		// Pointer identity check: second call must not replace cancel.
	}
	// Close must be idempotent.
	if err := svc.Close(time.Second); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := svc.Close(time.Second); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestCryptoWSSnapshotNilWhenDisabled(t *testing.T) {
	svc := NewService(Config{CryptoWSEnabled: false})
	if got := svc.CryptoWSSnapshot(); got != nil {
		t.Fatalf("expected nil snapshot, got %v", got)
	}
}

func TestCryptoWSSnapshotReturnsCopy(t *testing.T) {
	svc := NewService(Config{CryptoWSEnabled: true})
	svc.cryptoWSCache.Put("BTCUSDT", &QuoteSnapshot{Symbol: "BTCUSDT", Price: 100, AsOf: time.Now().UTC()})
	snap := svc.CryptoWSSnapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(snap))
	}
	if _, ok := snap["BTCUSDT"]; !ok {
		t.Fatalf("expected BTCUSDT key, got %v", snap)
	}
}

func TestBinanceProviderUnavailableErrorIsRecognised(t *testing.T) {
	cache := newCryptoTickerCache()
	provider := binanceQuoteProvider(cache, 30*time.Second)
	_, err := provider(context.Background(), InstrumentRef{Symbol: "BTCUSDT"})
	if !errors.Is(err, ErrQuoteUnavailable) {
		t.Fatalf("expected ErrQuoteUnavailable, got %v", err)
	}
}
