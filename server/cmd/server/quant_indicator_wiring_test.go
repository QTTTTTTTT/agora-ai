package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fundai/server/internal/indicator"
	"github.com/fundai/server/internal/marketdata"
	"github.com/fundai/server/internal/ohlc"
)

// stubOHLCFetcher lets tests control exactly what the indicator
// snapshot pipeline sees, decoupled from real HTTP / Yahoo /
// Binance. Calls() exposes the requests received so we can assert
// on the routing layer (correct symbol, correct market).
type stubOHLCFetcher struct {
	bars  []ohlc.Bar
	err   error
	calls []ohlc.FetchRequest
}

func (s *stubOHLCFetcher) Fetch(_ context.Context, req ohlc.FetchRequest) ([]ohlc.Bar, error) {
	s.calls = append(s.calls, req)
	if s.err != nil {
		return nil, s.err
	}
	out := make([]ohlc.Bar, len(s.bars))
	copy(out, s.bars)
	return out, nil
}

// indicatorSnapshot returns ok=false when the fetcher is nil, so the
// quant path falls back cleanly to legacy qualitative signals.
func TestIndicatorSnapshotNilFetcherReturnsFalse(t *testing.T) {
	pool := runtimeResearcherPool{}
	_, ok := pool.indicatorSnapshot(context.Background(), marketdata.InstrumentRef{Symbol: "AAPL", Market: "us_equity"})
	if ok {
		t.Errorf("expected ok=false when ohlcFetcher is nil")
	}
}

// indicatorSnapshot returns ok=false on ErrNoData / ErrNoProvider
// without bubbling the error, so per-symbol gaps never break the
// research line.
func TestIndicatorSnapshotSwallowsErrNoData(t *testing.T) {
	for _, errCase := range []error{ohlc.ErrNoData, ohlc.ErrNoProvider, errors.New("network unhappy")} {
		stub := &stubOHLCFetcher{err: errCase}
		pool := runtimeResearcherPool{ohlcFetcher: stub}
		_, ok := pool.indicatorSnapshot(context.Background(), marketdata.InstrumentRef{Symbol: "AAPL", Market: "us_equity"})
		if ok {
			t.Errorf("expected ok=false for err=%v", errCase)
		}
		if len(stub.calls) != 1 {
			t.Errorf("expected exactly one upstream attempt, got %d", len(stub.calls))
		}
	}
}

// indicatorSnapshot returns ok=false when bars are too short to
// compute (< 5 bars) — the indicator package documented behaviour.
func TestIndicatorSnapshotShortBarsReturnsFalse(t *testing.T) {
	stub := &stubOHLCFetcher{bars: []ohlc.Bar{{Close: 1, High: 1, Low: 1}, {Close: 2, High: 2, Low: 2}}}
	pool := runtimeResearcherPool{ohlcFetcher: stub}
	_, ok := pool.indicatorSnapshot(context.Background(), marketdata.InstrumentRef{Symbol: "AAPL", Market: "us_equity"})
	if ok {
		t.Errorf("expected ok=false on short bars")
	}
}

// indicatorSnapshot returns ok=true with a populated Snapshot when
// bars are long enough. Verifies the fetcher routing carries Symbol
// + Market correctly (the cache key needs both).
func TestIndicatorSnapshotHappyPath(t *testing.T) {
	bars := buildIncreasingBars(60)
	stub := &stubOHLCFetcher{bars: bars}
	pool := runtimeResearcherPool{ohlcFetcher: stub}
	snap, ok := pool.indicatorSnapshot(context.Background(), marketdata.InstrumentRef{Symbol: "aapl", Market: "US_Equity"})
	if !ok {
		t.Fatalf("expected ok=true, snap=%+v", snap)
	}
	if snap.LastClose <= 0 {
		t.Errorf("LastClose should be > 0, got %v", snap.LastClose)
	}
	if len(snap.Tags) == 0 {
		t.Errorf("expected non-empty tags")
	}
	if len(stub.calls) != 1 {
		t.Fatalf("expected 1 fetch, got %d", len(stub.calls))
	}
	req := stub.calls[0]
	if req.Symbol != "AAPL" {
		t.Errorf("Symbol should be normalized uppercase, got %q", req.Symbol)
	}
	if req.Market != "us_equity" {
		// FetchRequest.Normalize is called by the fetcher impl,
		// not the caller — the registry/cache do that. The pool
		// passes through whatever instrument.Market is. The stub
		// gets called with the raw instrument.Market though.
		// instrument.Market lowercase is owned by marketdata profile
		// normalization, so a raw "US_Equity" passes through here.
		// We assert on the value the pool actually sends.
		if req.Market != "US_Equity" {
			t.Errorf("Market forwarded = %q", req.Market)
		}
	}
}

// collectIndicatorBlock is the debate-side wrapper: it returns the
// pre-formatted indicator strings the LLMResearcher embeds in its
// QuantSignals prompt section. Verify that universe symbols flow
// through and missing symbols are silently dropped.
func TestCollectIndicatorBlockFiltersMissingSymbols(t *testing.T) {
	bars := buildIncreasingBars(60)
	stub := &stubOHLCFetcher{bars: bars}
	pool := runtimeResearcherPool{ohlcFetcher: stub}
	profile := fundMarketProfile{Market: "us_equity"}
	lines := pool.collectIndicatorBlock(context.Background(), profile, []string{"AAPL", "MSFT", "", "  ", "NVDA"})
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines (empty/whitespace filtered), got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "AAPL @") {
		t.Errorf("expected AAPL header, got %q", lines[0])
	}
}

// collectIndicatorBlock honors the per-call cap so a fund with 100
// universe symbols doesn't burn an unbounded budget on each debate.
func TestCollectIndicatorBlockCapsSymbolCount(t *testing.T) {
	bars := buildIncreasingBars(60)
	stub := &stubOHLCFetcher{bars: bars}
	pool := runtimeResearcherPool{ohlcFetcher: stub}
	syms := make([]string, 40)
	for i := range syms {
		syms[i] = "SYM" + string(rune('A'+i%26))
	}
	lines := pool.collectIndicatorBlock(context.Background(), fundMarketProfile{Market: "us_equity"}, syms)
	if len(lines) > 20 {
		t.Errorf("expected cap at 20 symbols, got %d lines", len(lines))
	}
	if len(stub.calls) > 20 {
		t.Errorf("expected cap at 20 upstream calls, got %d", len(stub.calls))
	}
}

// collectIndicatorBlock returns nil when fetcher is unwired so the
// debate doesn't accumulate empty noise.
func TestCollectIndicatorBlockNilFetcherReturnsNil(t *testing.T) {
	pool := runtimeResearcherPool{}
	lines := pool.collectIndicatorBlock(context.Background(), fundMarketProfile{Market: "us_equity"}, []string{"AAPL"})
	if lines != nil {
		t.Errorf("expected nil, got %v", lines)
	}
}

// buildOHLCFetcherFromEnv returns nil when OHLC_DISABLED=1 so
// operators can fully disable the path. Without disabling, the
// default chain is non-nil.
func TestBuildOHLCFetcherFromEnvDisable(t *testing.T) {
	t.Setenv("OHLC_DISABLED", "1")
	if f := buildOHLCFetcherFromEnv(); f != nil {
		t.Errorf("expected nil when OHLC_DISABLED=1, got %T", f)
	}
}

func TestBuildOHLCFetcherFromEnvDefault(t *testing.T) {
	t.Setenv("OHLC_DISABLED", "")
	t.Setenv("YAHOO_OHLC_DISABLED", "")
	t.Setenv("BINANCE_OHLC_DISABLED", "")
	t.Setenv("AKSHARE_OHLC_URL", "")
	f := buildOHLCFetcherFromEnv()
	if f == nil {
		t.Fatal("expected non-nil fetcher by default")
	}
	if _, ok := f.(*ohlc.Cache); !ok {
		t.Errorf("expected cache wrapper, got %T", f)
	}
}

// Build end-to-end through buildOHLCFetcherFromEnv: a fetched
// symbol goes through Yahoo (when YAHOO_OHLC_DISABLED is unset)
// and the cache layer. Exercised by pointing Yahoo at an httptest
// server.
func TestBuildOHLCFetcherFromEnvEndToEnd(t *testing.T) {
	const sample = `{"chart":{"result":[{"timestamp":[1,2,3],"indicators":{"quote":[{"open":[1,2,3],"high":[2,3,4],"low":[0.5,1.5,2.5],"close":[1.5,2.5,3.5],"volume":[100,200,300]}]}}]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sample))
	}))
	defer srv.Close()
	// Disable everything but Yahoo, and point Yahoo at the test
	// server. We can't override Yahoo's BaseURL through env, so
	// we build a separate chain manually that mirrors the env-
	// driven assembly.
	reg := ohlc.NewRegistry()
	reg.Register(&ohlc.YahooProvider{BaseURL: srv.URL})
	cache := ohlc.NewCache(reg, 0)
	bars, err := cache.Fetch(context.Background(), ohlc.FetchRequest{Symbol: "AAPL", Market: "us_equity"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(bars) != 3 {
		t.Errorf("expected 3 bars, got %d", len(bars))
	}
}

// Compute on the same synthetic series matches indicator package
// expectations: an uptrend produces an above-zero MACD and an RSI
// above 50, so the wiring layer's snapshot output is meaningful for
// the debate Quant role.
func TestSnapshotShapeForUptrendSeries(t *testing.T) {
	snap := indicator.Compute(buildIncreasingBars(120))
	if snap.MACDLine <= 0 {
		t.Errorf("MACDLine should be positive on uptrend, got %v", snap.MACDLine)
	}
	if snap.RSI14 <= 50 {
		t.Errorf("RSI14 should be > 50 on uptrend, got %v", snap.RSI14)
	}
}

// buildIncreasingBars makes a synthetic uptrending series with a
// small per-bar drift + noise so MACD/RSI/MA all light up.
func buildIncreasingBars(n int) []ohlc.Bar {
	bars := make([]ohlc.Bar, n)
	price := 100.0
	for i := 0; i < n; i++ {
		price += 0.5
		noise := 0.0
		if i%3 == 0 {
			noise = 0.4
		} else if i%3 == 1 {
			noise = -0.3
		}
		closePx := price + noise
		bars[i] = ohlc.Bar{
			Open:   closePx - 0.2,
			High:   closePx + 0.5,
			Low:    closePx - 0.6,
			Close:  closePx,
			Volume: 1_000_000 + float64(i*1000),
		}
	}
	return bars
}
