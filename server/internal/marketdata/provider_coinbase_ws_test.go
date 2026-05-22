package marketdata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestCoinbaseWSStreamHandlesTickerMessage(t *testing.T) {
	cache := newCryptoTickerCache()
	conn := newFakeWSConn()
	stream := newCoinbaseWSStream("ws://test", []string{"BTC-USD", "ETH-USD"}, cache, nil, nil)
	stream.readTimeout = 2 * time.Second
	stream.dial = func(_ context.Context, _ string) (wsConn, error) { return conn, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go stream.runOnce(ctx)

	select {
	case body := <-conn.writes:
		var msg map[string]any
		if err := json.Unmarshal(body, &msg); err != nil {
			t.Fatalf("subscribe payload not json: %v", err)
		}
		if msg["type"] != "subscribe" {
			t.Fatalf("expected type subscribe, got %v", msg["type"])
		}
		channels, _ := msg["channels"].([]any)
		if len(channels) != 1 {
			t.Fatalf("expected one channel, got %v", channels)
		}
		first, _ := channels[0].(map[string]any)
		if first["name"] != "ticker" {
			t.Fatalf("expected name ticker, got %v", first["name"])
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for subscribe")
	}

	// time must be recent so cache.Get freshness check passes — the real
	// Coinbase feed always sends current wall-clock time.
	tNow := time.Now().UTC().Format(time.RFC3339Nano)
	conn.pushTicker(fmt.Sprintf(`{"type":"ticker","product_id":"BTC-USD","price":"67000.10","best_bid":"67000","best_ask":"67000.20","volume_24h":"1200.34","time":"%s"}`, tNow))
	conn.pushTicker(fmt.Sprintf(`{"type":"ticker","product_id":"ETH-USD","price":"3500.50","best_bid":"3500.40","best_ask":"3500.60","volume_24h":"9876.5","time":"%s"}`, tNow))
	// Non-ticker messages (e.g. heartbeat/subscriptions ack) must be ignored
	// without writing to cache.
	conn.pushTicker(`{"type":"heartbeat","product_id":"BTC-USD","sequence":42}`)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := cache.Get("ETH-USD", time.Now().UTC(), time.Minute); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	btc, ok := cache.Get("BTC-USD", time.Now().UTC(), time.Minute)
	if !ok {
		t.Fatalf("BTC-USD not cached")
	}
	if btc.Price != 67000.10 || btc.Source != "coinbase-ws" || btc.QuoteCurrency != "USD" {
		t.Fatalf("BTC-USD wrong: %+v", btc)
	}
	expected, _ := time.Parse(time.RFC3339Nano, tNow)
	if !btc.AsOf.Equal(expected) {
		t.Fatalf("BTC-USD asOf wrong: got %s want %s", btc.AsOf, expected)
	}
}

func TestCoinbaseProviderReadsFromCache(t *testing.T) {
	cache := newCryptoTickerCache()
	cache.Put("BTC-USD", &QuoteSnapshot{Symbol: "BTC-USD", Price: 67000, AsOf: time.Now().UTC(), Source: "coinbase-ws"})
	provider := coinbaseQuoteProvider(cache, 30*time.Second)

	snap, err := provider(context.Background(), InstrumentRef{Symbol: "BTC-USD", Market: "crypto"})
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if snap.Price != 67000 || snap.Source != "coinbase-ws" {
		t.Fatalf("unexpected: %+v", snap)
	}
}

func TestCoinbaseProviderMissReturnsErr(t *testing.T) {
	cache := newCryptoTickerCache()
	provider := coinbaseQuoteProvider(cache, 30*time.Second)
	_, err := provider(context.Background(), InstrumentRef{Symbol: "BTC-USD"})
	if !errors.Is(err, ErrQuoteUnavailable) {
		t.Fatalf("expected ErrQuoteUnavailable, got %v", err)
	}
}

func TestNormalizeCoinbaseProductsInjectsDash(t *testing.T) {
	got := normalizeCoinbaseProducts([]string{"btcusd", "ETH-USD", "solusdt", " ", "bcheur", "btcusd"})
	want := []string{"BTC-USD", "ETH-USD", "SOL-USDT", "BCH-EUR"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("index %d: got %s want %s", i, got[i], w)
		}
	}
}

func TestInferQuoteFromCoinbaseProduct(t *testing.T) {
	cases := map[string]string{
		"BTC-USD":  "USD",
		"ETH-USDT": "USDT",
		"BTC":      "",
		"":         "",
	}
	for in, want := range cases {
		if got := inferQuoteFromCoinbaseProduct(in); got != want {
			t.Fatalf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestCryptoCacheQuoteOverridesInstrumentFields(t *testing.T) {
	cache := newCryptoTickerCache()
	cache.Put("BTCUSDT", &QuoteSnapshot{Symbol: "BTCUSDT", Price: 67000, AsOf: time.Now().UTC(), Source: "binance-ws"})

	snap, err := cryptoCacheQuote(cache, InstrumentRef{
		Symbol:        "btcusdt",
		Market:        "crypto",
		Exchange:      "binance",
		AssetClass:    "crypto",
		QuoteCurrency: "USDT",
		InstrumentKey: "k1",
	}, time.Minute, "binance-ws")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if snap.Market != "crypto" || snap.Exchange != "binance" || snap.InstrumentKey != "k1" {
		t.Fatalf("instrument fields not propagated: %+v", snap)
	}
	if snap.QuoteCurrency != "USDT" {
		t.Fatalf("quote currency: %q", snap.QuoteCurrency)
	}
}

// Smoke check for the Coinbase WS protocol contract: the SUBSCRIBE message
// must serialise channels as an array of objects (Coinbase rejects the
// shorthand array-of-strings variant). This guards against accidental
// regressions in the subscribe payload shape.
func TestCoinbaseSubscribeMessageShape(t *testing.T) {
	conn := newFakeWSConn()
	stream := newCoinbaseWSStream("ws://test", []string{"BTC-USD"}, newCryptoTickerCache(), nil, nil)
	stream.dial = func(_ context.Context, _ string) (wsConn, error) { return conn, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go stream.runOnce(ctx)

	select {
	case body := <-conn.writes:
		var raw map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("subscribe payload not json: %v", err)
		}
		channels, ok := raw["channels"].([]any)
		if !ok || len(channels) == 0 {
			t.Fatalf("channels must be a non-empty array, got %T %v", raw["channels"], raw["channels"])
		}
		channel, ok := channels[0].(map[string]any)
		if !ok {
			t.Fatalf("channel must be an object, got %T", channels[0])
		}
		if _, ok := channel["product_ids"].([]any); !ok {
			t.Fatalf("product_ids must be array, got %T %v", channel["product_ids"], channel["product_ids"])
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for subscribe")
	}
	conn.Close(websocket.StatusNormalClosure, "done")
}
