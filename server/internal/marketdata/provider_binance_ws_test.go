package marketdata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeWSConn is a minimal in-memory implementation of wsConn that lets a
// test push read frames at the stream loop and observe the SUBSCRIBE
// payload the loop wrote. closeErr is returned from subsequent Read calls
// after the read queue is drained so the runOnce loop can be made to exit
// deterministically.
type fakeWSConn struct {
	reads     chan fakeRead
	writes    chan []byte
	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error
}

type fakeRead struct {
	typ  websocket.MessageType
	body []byte
	err  error
}

func newFakeWSConn() *fakeWSConn {
	return &fakeWSConn{
		reads:    make(chan fakeRead, 16),
		writes:   make(chan []byte, 4),
		closed:   make(chan struct{}),
		closeErr: io.EOF,
	}
}

func (f *fakeWSConn) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	case <-f.closed:
		return 0, nil, f.closeErr
	case r := <-f.reads:
		return r.typ, r.body, r.err
	}
}

func (f *fakeWSConn) Write(ctx context.Context, _ websocket.MessageType, p []byte) error {
	clone := make([]byte, len(p))
	copy(clone, p)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case f.writes <- clone:
		return nil
	}
}

func (f *fakeWSConn) Close(code websocket.StatusCode, reason string) error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeWSConn) pushTicker(payload string) {
	f.reads <- fakeRead{typ: websocket.MessageText, body: []byte(payload)}
}

func (f *fakeWSConn) pushError(err error) {
	f.reads <- fakeRead{err: err}
}

func TestBinanceWSStreamHandlesTickerMessage(t *testing.T) {
	cache := newCryptoTickerCache()
	conn := newFakeWSConn()
	stream := newBinanceWSStream("ws://test", []string{"BTCUSDT", "ETHUSDT"}, cache, nil, nil)
	stream.readTimeout = 2 * time.Second
	stream.dial = func(_ context.Context, url string) (wsConn, error) {
		return conn, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- stream.runOnce(ctx)
	}()

	// Wait for subscribe write so we can assert payload before pushing data.
	select {
	case body := <-conn.writes:
		var msg map[string]any
		if err := json.Unmarshal(body, &msg); err != nil {
			t.Fatalf("subscribe payload not json: %v / %s", err, body)
		}
		if msg["method"] != "SUBSCRIBE" {
			t.Fatalf("expected method SUBSCRIBE, got %v", msg["method"])
		}
		params, _ := msg["params"].([]any)
		if len(params) != 2 || params[0] != "btcusdt@ticker" || params[1] != "ethusdt@ticker" {
			t.Fatalf("unexpected params %#v", params)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for subscribe write")
	}

	// EventTime must be recent so the freshness check at cache.Get time
	// passes — Binance's real feed always carries the current ms timestamp.
	nowMs := time.Now().UnixMilli()
	conn.pushTicker(fmt.Sprintf(`{"e":"24hrTicker","E":%d,"s":"BTCUSDT","c":"67000.10","b":"67000.00","a":"67000.20","v":"1234.567"}`, nowMs))
	conn.pushTicker(fmt.Sprintf(`{"e":"24hrTicker","E":%d,"s":"ETHUSDT","c":"3500.50","b":"3500.40","a":"3500.60","v":"9876.5"}`, nowMs+1000))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := cache.Get("ETHUSDT", time.Now().UTC(), time.Minute); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	btc, ok := cache.Get("BTCUSDT", time.Now().UTC(), time.Minute)
	if !ok || btc.Price != 67000.10 || btc.Source != "binance-ws" {
		t.Fatalf("BTCUSDT not cached correctly: %+v ok=%v", btc, ok)
	}
	if btc.Bid != 67000.00 || btc.Ask != 67000.20 || btc.Volume != 1234 {
		t.Fatalf("BTCUSDT bid/ask/volume wrong: %+v", btc)
	}
	if btc.QuoteCurrency != "USDT" {
		t.Fatalf("BTCUSDT quote currency wrong: %q", btc.QuoteCurrency)
	}
	eth, ok := cache.Get("ETHUSDT", time.Now().UTC(), time.Minute)
	if !ok || eth.Price != 3500.50 {
		t.Fatalf("ETHUSDT not cached correctly: %+v ok=%v", eth, ok)
	}

	// Close the conn to drain runOnce; assert it returns the EOF error so
	// the outer Run() loop would back off + reconnect.
	conn.Close(websocket.StatusNormalClosure, "test done")
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "eof") {
			t.Fatalf("expected EOF from runOnce, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("runOnce did not return after close")
	}
}

func TestBinanceWSStreamIgnoresSubscribeAck(t *testing.T) {
	cache := newCryptoTickerCache()
	conn := newFakeWSConn()
	stream := newBinanceWSStream("ws://test", []string{"BTCUSDT"}, cache, nil, nil)
	stream.readTimeout = 2 * time.Second
	stream.dial = func(_ context.Context, _ string) (wsConn, error) { return conn, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go stream.runOnce(ctx)

	<-conn.writes
	// Binance acks subscribe with {"result":null,"id":1}; should not crash
	// and should leave the cache empty.
	conn.pushTicker(`{"result":null,"id":1}`)
	nowMs := time.Now().UnixMilli()
	conn.pushTicker(fmt.Sprintf(`{"e":"24hrTicker","E":%d,"s":"BTCUSDT","c":"67000.10","b":"0","a":"0","v":"0"}`, nowMs))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := cache.Get("BTCUSDT", time.Now().UTC(), time.Minute); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := cache.Get("BTCUSDT", time.Now().UTC(), time.Minute); !ok {
		t.Fatalf("BTCUSDT should be cached after ticker following ack")
	}
}

func TestBinanceWSStreamReconnects(t *testing.T) {
	cache := newCryptoTickerCache()
	var dials atomic.Int32
	makeConn := func() *fakeWSConn { return newFakeWSConn() }
	current := makeConn()
	stream := newBinanceWSStream("ws://test", []string{"BTCUSDT"}, cache, nil, nil)
	stream.readTimeout = 200 * time.Millisecond
	stream.backoffInitial = 5 * time.Millisecond
	stream.backoffMax = 5 * time.Millisecond
	stream.dial = func(_ context.Context, _ string) (wsConn, error) {
		dials.Add(1)
		// First connection drops immediately; subsequent connection lives.
		if dials.Load() == 1 {
			return current, nil
		}
		next := makeConn()
		next.pushTicker(`{"e":"24hrTicker","s":"BTCUSDT","c":"68000","b":"0","a":"0","v":"0"}`)
		current = next
		return next, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go stream.Run(ctx)

	// Drain subscribe + drop first connection.
	<-current.writes
	current.Close(websocket.StatusNormalClosure, "drop")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := cache.Get("BTCUSDT", time.Now().UTC(), time.Minute); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := cache.Get("BTCUSDT", time.Now().UTC(), time.Minute); !ok {
		t.Fatalf("ticker after reconnect not cached; dials=%d", dials.Load())
	}
	if dials.Load() < 2 {
		t.Fatalf("expected ≥2 dial attempts, got %d", dials.Load())
	}
}

func TestBinanceProviderReadsFromCache(t *testing.T) {
	cache := newCryptoTickerCache()
	now := time.Now().UTC()
	cache.Put("BTCUSDT", &QuoteSnapshot{Symbol: "BTCUSDT", Price: 67000, AsOf: now, Source: "binance-ws"})
	provider := binanceQuoteProvider(cache, 30*time.Second)

	snap, err := provider(context.Background(), InstrumentRef{
		Symbol:        "BTCUSDT",
		Market:        "crypto",
		AssetClass:    "crypto",
		QuoteCurrency: "USDT",
		InstrumentKey: "binance:BTCUSDT",
	})
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if snap.Price != 67000 {
		t.Fatalf("unexpected price %v", snap.Price)
	}
	if snap.InstrumentKey != "binance:BTCUSDT" {
		t.Fatalf("instrument key not propagated: %q", snap.InstrumentKey)
	}
	if snap.QuoteCurrency != "USDT" {
		t.Fatalf("quote currency override lost: %q", snap.QuoteCurrency)
	}
}

func TestBinanceProviderFallsThroughOnStale(t *testing.T) {
	cache := newCryptoTickerCache()
	cache.Put("BTCUSDT", &QuoteSnapshot{Symbol: "BTCUSDT", Price: 67000, AsOf: time.Now().UTC().Add(-time.Hour)})
	provider := binanceQuoteProvider(cache, 30*time.Second)

	_, err := provider(context.Background(), InstrumentRef{Symbol: "BTCUSDT"})
	if !errors.Is(err, ErrQuoteUnavailable) {
		t.Fatalf("expected ErrQuoteUnavailable on stale cache, got %v", err)
	}
}

func TestBinanceProviderEmptySymbol(t *testing.T) {
	cache := newCryptoTickerCache()
	provider := binanceQuoteProvider(cache, 30*time.Second)
	_, err := provider(context.Background(), InstrumentRef{Symbol: " "})
	if err == nil {
		t.Fatalf("expected error for empty symbol")
	}
}

func TestNormalizeBinanceSymbols(t *testing.T) {
	got := normalizeBinanceSymbols([]string{"btcusdt", " eth-usdt ", "", "BTCUSDT"})
	want := []string{"BTCUSDT", "ETHUSDT"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("index %d: got %s want %s", i, got[i], w)
		}
	}
}

func TestInferQuoteFromBinanceSymbol(t *testing.T) {
	cases := map[string]string{
		"BTCUSDT": "USDT",
		"ETHUSD":  "USD",
		"BTCBTC":  "BTC", // edge: base == quote? still returns suffix
		"":        "",
		"FOO":     "",
	}
	for in, want := range cases {
		if got := inferQuoteFromBinanceSymbol(in); got != want {
			t.Fatalf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestNextBackoff(t *testing.T) {
	if got := nextBackoff(0, 30*time.Second); got != time.Second {
		t.Fatalf("zero current should reset to 1s, got %v", got)
	}
	if got := nextBackoff(2*time.Second, 30*time.Second); got != 4*time.Second {
		t.Fatalf("2s × 2 should be 4s, got %v", got)
	}
	if got := nextBackoff(20*time.Second, 30*time.Second); got != 30*time.Second {
		t.Fatalf("expected cap at 30s, got %v", got)
	}
}
