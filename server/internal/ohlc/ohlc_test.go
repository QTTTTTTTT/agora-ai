package ohlc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stubProvider lets us assert routing and fallback behaviour without
// real HTTP. Embed in registry tests to count calls and inject
// canned bars or errors per market.
type stubProvider struct {
	name    string
	markets []string
	bars    []Bar
	err     error
	calls   atomic.Int32
}

func (s *stubProvider) Name() string { return s.name }
func (s *stubProvider) Supports(market string) bool {
	for _, m := range s.markets {
		if strings.EqualFold(m, market) {
			return true
		}
	}
	return false
}
func (s *stubProvider) Fetch(_ context.Context, _ FetchRequest) ([]Bar, error) {
	s.calls.Add(1)
	if s.err != nil {
		return nil, s.err
	}
	out := make([]Bar, len(s.bars))
	copy(out, s.bars)
	return out, nil
}

// FetchRequest.Normalize fills in EndTime, LookbackN, Interval, and
// lower-cases Market. Idempotency check matters because the cache
// keys the bucket from the normalized request.
func TestFetchRequestNormalizeFillsDefaults(t *testing.T) {
	req := FetchRequest{Symbol: "  AAPL  ", Market: "US_Equity"}.Normalize()
	if req.Symbol != "AAPL" {
		t.Errorf("symbol = %q, want AAPL", req.Symbol)
	}
	if req.Market != "us_equity" {
		t.Errorf("market = %q, want us_equity lowercase", req.Market)
	}
	if req.Interval != IntervalDay {
		t.Errorf("interval default = %q, want 1d", req.Interval)
	}
	if req.LookbackN != 120 {
		t.Errorf("lookback default = %d, want 120", req.LookbackN)
	}
	if req.EndTime.IsZero() {
		t.Errorf("EndTime default should be now")
	}
	again := req.Normalize()
	if again != req {
		t.Errorf("Normalize should be idempotent")
	}
}

// Unknown intervals collapse to 1d so providers don't have to
// validate their own input.
func TestIntervalNormalizeCollapsesUnknownToDaily(t *testing.T) {
	for _, in := range []Interval{"", "weird", Interval("4h")} {
		if in.Normalize() != IntervalDay {
			t.Errorf("Normalize(%q) = %q, want 1d", in, in.Normalize())
		}
	}
	if Interval("60M").Normalize() != Interval1h {
		t.Errorf("60M should normalize to 1h")
	}
}

// Registry picks the first matching provider for the market and
// returns ErrNoProvider when none claim it.
func TestRegistryRoutesByMarket(t *testing.T) {
	usOnly := &stubProvider{name: "us", markets: []string{"us_equity"}, bars: []Bar{{Close: 1}}}
	cryptoOnly := &stubProvider{name: "binance", markets: []string{"crypto"}, bars: []Bar{{Close: 2}}}

	reg := NewRegistry()
	reg.Register(usOnly)
	reg.Register(cryptoOnly)

	bars, err := reg.Fetch(context.Background(), FetchRequest{Symbol: "AAPL", Market: "us_equity"})
	if err != nil {
		t.Fatalf("us_equity: %v", err)
	}
	if len(bars) != 1 || bars[0].Close != 1 {
		t.Errorf("us_equity bars = %+v", bars)
	}
	if usOnly.calls.Load() != 1 || cryptoOnly.calls.Load() != 0 {
		t.Errorf("call counts wrong: us=%d crypto=%d", usOnly.calls.Load(), cryptoOnly.calls.Load())
	}

	if _, err := reg.Fetch(context.Background(), FetchRequest{Symbol: "GOLD", Market: "futures"}); !errors.Is(err, ErrNoProvider) {
		t.Errorf("expected ErrNoProvider for unsupported market, got %v", err)
	}
}

// TestYahooSupportsDefault pins the YahooProvider's default Markets
// list. Adding/removing a market here is a CONTRACT change that
// affects whether the dashboard surfaces csi300 / chinext / star50
// or shows a "skipped" toast — pin tightly so the breaking change
// shows up in code review.
func TestYahooSupportsDefault(t *testing.T) {
	p := &YahooProvider{}
	want := map[string]bool{
		"us_equity": true,
		"hk_equity": true,
		"a_share":   true,
		"crypto":    false,
		"futures":   false,
		"":          false,
	}
	for m, expected := range want {
		got := p.Supports(m)
		if got != expected {
			t.Errorf("Supports(%q) = %v, want %v", m, got, expected)
		}
	}
}

// TestYahooSupportsRespectsExplicitMarkets ensures operators can
// still narrow the provider (e.g., when wiring Akshare for A-share
// stocks AND wanting to keep Yahoo US-only for backtests).
func TestYahooSupportsRespectsExplicitMarkets(t *testing.T) {
	p := &YahooProvider{Markets: []string{"us_equity"}}
	if !p.Supports("us_equity") {
		t.Error("explicit us_equity should be supported")
	}
	if p.Supports("a_share") {
		t.Error("explicit Markets should NOT fall back to default")
	}
}

// When the first matching provider returns ErrNoData, the registry
// must try the next one before giving up.
func TestRegistryFallsThroughToNextProviderOnNoData(t *testing.T) {
	first := &stubProvider{name: "first", markets: []string{"us_equity"}, err: ErrNoData}
	second := &stubProvider{name: "second", markets: []string{"us_equity"}, bars: []Bar{{Close: 99}}}
	reg := NewRegistry()
	reg.Register(first)
	reg.Register(second)

	bars, err := reg.Fetch(context.Background(), FetchRequest{Symbol: "AAPL", Market: "us_equity"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(bars) != 1 || bars[0].Close != 99 {
		t.Errorf("bars = %+v", bars)
	}
	if first.calls.Load() != 1 || second.calls.Load() != 1 {
		t.Errorf("calls first=%d second=%d", first.calls.Load(), second.calls.Load())
	}
}

// Cache short-circuits the second call when within TTL. The third
// call (after TTL expires) re-hits the upstream.
func TestCacheTTLBehaviour(t *testing.T) {
	src := &stubProvider{name: "src", markets: []string{"crypto"}, bars: []Bar{{Close: 42}}}
	cache := NewCache(src, 50*time.Millisecond)

	req := FetchRequest{Symbol: "BTCUSDT", Market: "crypto"}
	for i := 0; i < 3; i++ {
		bars, err := cache.Fetch(context.Background(), req)
		if err != nil {
			t.Fatalf("Fetch %d: %v", i, err)
		}
		if len(bars) != 1 || bars[0].Close != 42 {
			t.Errorf("Fetch %d bars = %+v", i, bars)
		}
	}
	if src.calls.Load() != 1 {
		t.Errorf("expected 1 upstream call within TTL, got %d", src.calls.Load())
	}

	time.Sleep(60 * time.Millisecond)
	if _, err := cache.Fetch(context.Background(), req); err != nil {
		t.Fatalf("post-expiry Fetch: %v", err)
	}
	if src.calls.Load() != 2 {
		t.Errorf("expected 2 upstream calls after TTL expiry, got %d", src.calls.Load())
	}
}

// Zero TTL disables caching — every call hits the upstream.
func TestCacheZeroTTLDisables(t *testing.T) {
	src := &stubProvider{name: "src", markets: []string{"crypto"}, bars: []Bar{{Close: 1}}}
	cache := NewCache(src, 0)
	for i := 0; i < 3; i++ {
		if _, err := cache.Fetch(context.Background(), FetchRequest{Symbol: "BTCUSDT", Market: "crypto"}); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
	}
	if src.calls.Load() != 3 {
		t.Errorf("zero TTL should pass through every call, got %d", src.calls.Load())
	}
}

// Yahoo provider integration: full happy-path JSON, including null
// fields the indicator parsing must skip.
func TestYahooProviderParsesChartJSON(t *testing.T) {
	const sample = `{
		"chart":{
			"result":[{
				"timestamp":[1714435200,1714521600,1714608000],
				"indicators":{
					"quote":[{
						"open":[170.1,171.0,172.5],
						"high":[171.5,172.2,173.0],
						"low":[169.8,170.5,171.9],
						"close":[171.2,172.0,172.8],
						"volume":[10000000,11000000,12000000]
					}]
				}
			}],
			"error":null
		}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/v8/finance/chart/AAPL") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("interval") != "1d" {
			t.Errorf("interval = %q, want 1d", r.URL.Query().Get("interval"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sample))
	}))
	defer srv.Close()

	p := &YahooProvider{BaseURL: srv.URL}
	bars, err := p.Fetch(context.Background(), FetchRequest{Symbol: "AAPL", Market: "us_equity"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(bars) != 3 {
		t.Fatalf("bars len = %d, want 3", len(bars))
	}
	if bars[0].Close != 171.2 || bars[2].Close != 172.8 {
		t.Errorf("bars unexpected: %+v", bars)
	}
	if bars[0].Volume != 10000000 {
		t.Errorf("volume = %v, want 10000000", bars[0].Volume)
	}
}

// Yahoo: rows with null OHLC fields are dropped, not returned with
// NaN/zero values that would poison indicator math.
func TestYahooProviderSkipsNullRows(t *testing.T) {
	const sample = `{
		"chart":{"result":[{
			"timestamp":[1,2,3],
			"indicators":{"quote":[{
				"open":[1.0,null,3.0],
				"high":[1.5,null,3.5],
				"low":[0.9,null,2.9],
				"close":[1.2,null,3.2],
				"volume":[100,null,300]
			}]}
		}]}
	}`
	bars, err := parseYahooChart([]byte(sample), 10)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(bars) != 2 {
		t.Fatalf("expected 2 bars (nulls skipped), got %d: %+v", len(bars), bars)
	}
	for _, b := range bars {
		if b.Close <= 0 {
			t.Errorf("non-positive close survived parse: %+v", b)
		}
	}
}

// Yahoo returning 404 / no data → ErrNoData (registry then tries
// the next provider).
func TestYahooProviderReturnsErrNoDataOn404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()
	p := &YahooProvider{BaseURL: srv.URL}
	_, err := p.Fetch(context.Background(), FetchRequest{Symbol: "UNKNOWN", Market: "us_equity"})
	if !errors.Is(err, ErrNoData) {
		t.Errorf("404 should map to ErrNoData, got %v", err)
	}
}

// Yahoo Markets default to US + HK; explicitly setting Markets
// overrides this.
func TestYahooProviderMarketDefaults(t *testing.T) {
	p := &YahooProvider{}
	if !p.Supports("us_equity") || !p.Supports("hk_equity") {
		t.Errorf("default markets should cover US + HK")
	}
	if p.Supports("crypto") {
		t.Errorf("default markets should NOT include crypto")
	}
	custom := &YahooProvider{Markets: []string{"us_equity"}}
	if custom.Supports("hk_equity") {
		t.Errorf("custom Markets should be exact, no hk_equity")
	}
}

// Binance provider integration: JSON array of arrays, with both
// numeric and string fields (Binance returns prices as strings).
func TestBinanceProviderParsesKlines(t *testing.T) {
	const sample = `[
		[1714435200000,"60000.10","61000.20","59500.30","60500.40","1234.5",1714521599000,"some","extra","fields"],
		[1714521600000,"60500.40","62000.50","60000.10","61800.60","2345.6",1714607999000,"more","extra","fields"]
	]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("symbol") != "BTCUSDT" {
			t.Errorf("symbol = %q, want BTCUSDT", r.URL.Query().Get("symbol"))
		}
		if r.URL.Query().Get("interval") != "1d" {
			t.Errorf("interval = %q", r.URL.Query().Get("interval"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sample))
	}))
	defer srv.Close()

	p := &BinanceProvider{BaseURL: srv.URL}
	bars, err := p.Fetch(context.Background(), FetchRequest{Symbol: "btc-usdt", Market: "crypto"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(bars) != 2 {
		t.Fatalf("bars len = %d, want 2", len(bars))
	}
	if bars[0].Open != 60000.10 || bars[1].Close != 61800.60 {
		t.Errorf("bars unexpected: %+v", bars)
	}
	if bars[0].Time.IsZero() {
		t.Errorf("bar 0 time should be set from openTime")
	}
}

// Binance: separators stripped, casing normalized.
func TestNormalizeBinanceSymbol(t *testing.T) {
	cases := map[string]string{
		"btc-usdt":  "BTCUSDT",
		"BTC/USDT":  "BTCUSDT",
		" eth:usd ": "ETHUSD",
		"SOL-USDC":  "SOLUSDC",
	}
	for in, want := range cases {
		if got := normalizeBinanceSymbol(in); got != want {
			t.Errorf("normalizeBinanceSymbol(%q) = %q, want %q", in, got, want)
		}
	}
}

// Binance 400 (unknown symbol) maps to ErrNoData so callers can
// degrade gracefully.
func TestBinanceProviderUnknownSymbolReturnsErrNoData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"code":-1121,"msg":"Invalid symbol."}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	p := &BinanceProvider{BaseURL: srv.URL}
	_, err := p.Fetch(context.Background(), FetchRequest{Symbol: "FAKEUSDT", Market: "crypto"})
	if !errors.Is(err, ErrNoData) {
		t.Errorf("400 should map to ErrNoData, got %v", err)
	}
}

// Akshare provider: parses both the array shape and the wrapped
// {data: [...]} shape used by some MCP forks.
func TestAkshareProviderParsesArrayShape(t *testing.T) {
	const sample = `[
		{"date":"2026-05-19","open":12.3,"high":12.5,"low":12.1,"close":12.4,"volume":42000},
		{"date":"2026-05-20","open":12.4,"high":12.6,"low":12.2,"close":12.55,"volume":51000}
	]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("symbol") != "600519" {
			t.Errorf("symbol = %q", r.URL.Query().Get("symbol"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sample))
	}))
	defer srv.Close()

	p := &AkshareProvider{BaseURL: srv.URL}
	bars, err := p.Fetch(context.Background(), FetchRequest{Symbol: "600519", Market: "a_share"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(bars) != 2 {
		t.Fatalf("len = %d, want 2", len(bars))
	}
	if bars[1].Close != 12.55 {
		t.Errorf("bar 1 close = %v", bars[1].Close)
	}
	if bars[1].Time.IsZero() {
		t.Errorf("bar time should be parsed from 'date'")
	}
}

// Akshare wrapped {"data": [...]} shape used by some forks.
func TestAkshareProviderParsesWrappedShape(t *testing.T) {
	const sample = `{"code":0,"data":[{"date":"2026-05-19","open":1,"high":2,"low":0.5,"close":1.5,"volume":100}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sample))
	}))
	defer srv.Close()
	p := &AkshareProvider{BaseURL: srv.URL}
	bars, err := p.Fetch(context.Background(), FetchRequest{Symbol: "600519", Market: "a_share"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(bars) != 1 || bars[0].Close != 1.5 {
		t.Errorf("bars = %+v", bars)
	}
}

// Akshare: when BaseURL is empty, Supports must return false so the
// provider doesn't claim a market it can't service.
func TestAkshareProviderSupportsRequiresBaseURL(t *testing.T) {
	p := &AkshareProvider{}
	if p.Supports("a_share") {
		t.Errorf("Supports must require BaseURL")
	}
	with := &AkshareProvider{BaseURL: "http://akshare.local"}
	if !with.Supports("a_share") {
		t.Errorf("default markets should cover a_share")
	}
}

// CacheKey buckets EndTime so two requests within the same TTL
// window collide on the same key (the cache hit path). The bucket
// is computed via time.Truncate, so requests must fall inside the
// same [floor(t/bucket), floor(t/bucket)+bucket) interval to share
// a key — small offsets that cross a boundary are expected to miss.
func TestCacheKeyBucketsEndTime(t *testing.T) {
	// Two times comfortably inside the same minute bucket.
	base := time.Date(2026, 5, 20, 12, 34, 10, 0, time.UTC)
	a := FetchRequest{Symbol: "AAPL", Market: "us_equity", EndTime: base}
	b := FetchRequest{Symbol: "AAPL", Market: "us_equity", EndTime: base.Add(20 * time.Second)}
	if a.CacheKey(time.Minute) != b.CacheKey(time.Minute) {
		t.Errorf("times in the same minute bucket should share a key: %s vs %s", a.CacheKey(time.Minute), b.CacheKey(time.Minute))
	}
	// Times straddling the bucket boundary must NOT collide.
	c := FetchRequest{Symbol: "AAPL", Market: "us_equity", EndTime: base.Add(2 * time.Minute)}
	if a.CacheKey(time.Minute) == c.CacheKey(time.Minute) {
		t.Errorf("times across the bucket boundary should NOT share a key")
	}
	// Different symbols must never collide even within the same bucket.
	d := FetchRequest{Symbol: "MSFT", Market: "us_equity", EndTime: base}
	if a.CacheKey(time.Minute) == d.CacheKey(time.Minute) {
		t.Errorf("different symbols must produce different keys")
	}
}
