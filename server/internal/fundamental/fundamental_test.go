package fundamental

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

// stubProvider is a Provider whose result is canned. Used for
// registry / cache routing tests.
type stubProvider struct {
	name    string
	markets []string
	metrics *Metrics
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
func (s *stubProvider) Fetch(_ context.Context, _ FetchRequest) (*Metrics, error) {
	s.calls.Add(1)
	if s.err != nil {
		return nil, s.err
	}
	if s.metrics == nil {
		return nil, nil
	}
	clone := *s.metrics
	return &clone, nil
}

// FetchRequest.Normalize is idempotent and lower-cases the market.
func TestFetchRequestNormalize(t *testing.T) {
	req := FetchRequest{Symbol: " AAPL ", Market: "US_Equity"}.Normalize()
	if req.Symbol != "AAPL" || req.Market != "us_equity" {
		t.Errorf("normalize wrong: %+v", req)
	}
	if req.CacheKey() != "us_equity|AAPL" {
		t.Errorf("CacheKey = %q", req.CacheKey())
	}
}

// Registry picks the first matching provider and falls through on
// ErrNoData.
func TestRegistryFallsThroughOnErrNoData(t *testing.T) {
	first := &stubProvider{name: "yahoo", markets: []string{"us_equity"}, err: ErrNoData}
	second := &stubProvider{name: "fallback", markets: []string{"us_equity"}, metrics: &Metrics{Symbol: "AAPL", PE: 28}}
	reg := NewRegistry()
	reg.Register(first)
	reg.Register(second)
	got, err := reg.Fetch(context.Background(), FetchRequest{Symbol: "AAPL", Market: "us_equity"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.PE != 28 {
		t.Errorf("got = %+v", got)
	}
	if first.calls.Load() != 1 || second.calls.Load() != 1 {
		t.Errorf("calls first=%d second=%d", first.calls.Load(), second.calls.Load())
	}
}

// Registry returns ErrNoProvider when no provider claims the market.
func TestRegistryNoProvider(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&stubProvider{name: "us", markets: []string{"us_equity"}})
	_, err := reg.Fetch(context.Background(), FetchRequest{Symbol: "600519", Market: "a_share"})
	if !errors.Is(err, ErrNoProvider) {
		t.Errorf("expected ErrNoProvider, got %v", err)
	}
}

// Register is idempotent on Name(): the second call replaces the
// first instance so config reload is well-defined.
func TestRegistryReplaceByName(t *testing.T) {
	reg := NewRegistry()
	old := &stubProvider{name: "yahoo", markets: []string{"us_equity"}, metrics: &Metrics{Symbol: "AAPL", PE: 1}}
	fresh := &stubProvider{name: "yahoo", markets: []string{"us_equity"}, metrics: &Metrics{Symbol: "AAPL", PE: 2}}
	reg.Register(old)
	reg.Register(fresh)
	got, _ := reg.Fetch(context.Background(), FetchRequest{Symbol: "AAPL", Market: "us_equity"})
	if got.PE != 2 {
		t.Errorf("expected replacement, got %+v", got)
	}
	if old.calls.Load() != 0 {
		t.Errorf("old should not be called after replacement")
	}
}

// Cache memoizes within TTL and re-fetches after expiry.
func TestCacheTTLBehaviour(t *testing.T) {
	src := &stubProvider{name: "src", markets: []string{"us_equity"}, metrics: &Metrics{Symbol: "AAPL", PE: 28}}
	cache := NewCache(src, 50*time.Millisecond)
	req := FetchRequest{Symbol: "AAPL", Market: "us_equity"}
	for i := 0; i < 3; i++ {
		got, err := cache.Fetch(context.Background(), req)
		if err != nil {
			t.Fatalf("Fetch %d: %v", i, err)
		}
		if got.PE != 28 {
			t.Errorf("Fetch %d: PE = %v, want 28", i, got.PE)
		}
	}
	if src.calls.Load() != 1 {
		t.Errorf("expected 1 upstream call within TTL, got %d", src.calls.Load())
	}
	time.Sleep(60 * time.Millisecond)
	if _, err := cache.Fetch(context.Background(), req); err != nil {
		t.Fatalf("post-expiry: %v", err)
	}
	if src.calls.Load() != 2 {
		t.Errorf("expected 2 calls after expiry, got %d", src.calls.Load())
	}
}

// Cache returns clones — mutating the result must not corrupt the
// cache entry.
func TestCacheReturnsCloneNotPointer(t *testing.T) {
	src := &stubProvider{name: "src", markets: []string{"us_equity"}, metrics: &Metrics{Symbol: "AAPL", PE: 28}}
	cache := NewCache(src, time.Minute)
	req := FetchRequest{Symbol: "AAPL", Market: "us_equity"}
	a, _ := cache.Fetch(context.Background(), req)
	a.PE = 99
	b, _ := cache.Fetch(context.Background(), req)
	if b.PE != 28 {
		t.Errorf("cache mutation leaked: got %v, want 28", b.PE)
	}
}

// Yahoo provider integration: a realistic quoteSummary response
// (raw/fmt wrapped numbers, all four modules present).
func TestYahooProviderParsesQuoteSummary(t *testing.T) {
	const body = `{
		"quoteSummary":{"result":[{
			"defaultKeyStatistics":{
				"trailingPE":{"raw":28.30,"fmt":"28.30"},
				"priceToBook":{"raw":47.20,"fmt":"47.20"},
				"beta":{"raw":1.21,"fmt":"1.21"},
				"marketCap":{"raw":2850000000000,"fmt":"2.85T"}
			},
			"financialData":{
				"profitMargins":{"raw":0.252},
				"operatingMargins":{"raw":0.309},
				"returnOnEquity":{"raw":1.567},
				"revenueGrowth":{"raw":0.082},
				"earningsGrowth":{"raw":0.124},
				"debtToEquity":{"raw":1.95},
				"financialCurrency":"USD"
			},
			"summaryDetail":{
				"forwardPE":{"raw":24.1},
				"dividendYield":{"raw":0.005}
			},
			"price":{
				"currency":"USD",
				"regularMarketTime":1714521600
			}
		}],"error":null}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/v10/finance/quoteSummary/AAPL") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	p := &YahooProvider{BaseURL: srv.URL}
	m, err := p.Fetch(context.Background(), FetchRequest{Symbol: "AAPL", Market: "us_equity"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if m.PE != 28.30 || m.ForwardPE != 24.1 || m.PB != 47.20 {
		t.Errorf("ratios wrong: %+v", m)
	}
	if m.ProfitMargin != 0.252 || m.OperatingMargin != 0.309 || m.ReturnOnEquity != 1.567 {
		t.Errorf("margins wrong: %+v", m)
	}
	if m.RevenueGrowth != 0.082 || m.EarningsGrowth != 0.124 {
		t.Errorf("growth wrong: %+v", m)
	}
	if m.MarketCap != 2.85e12 || m.Currency != "USD" {
		t.Errorf("market cap / currency wrong: %+v", m)
	}
	if m.AsOf.IsZero() {
		t.Errorf("AsOf should be parsed from regularMarketTime")
	}
}

// Yahoo: 404 → ErrNoData, fully-empty response → ErrNoData.
func TestYahooProvider404IsErrNoData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()
	p := &YahooProvider{BaseURL: srv.URL}
	_, err := p.Fetch(context.Background(), FetchRequest{Symbol: "FAKE", Market: "us_equity"})
	if !errors.Is(err, ErrNoData) {
		t.Errorf("404 should map to ErrNoData, got %v", err)
	}
}

func TestYahooProviderEmptyResultIsErrNoData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"quoteSummary":{"result":[],"error":null}}`))
	}))
	defer srv.Close()
	p := &YahooProvider{BaseURL: srv.URL}
	_, err := p.Fetch(context.Background(), FetchRequest{Symbol: "ANY", Market: "us_equity"})
	if !errors.Is(err, ErrNoData) {
		t.Errorf("empty result should be ErrNoData, got %v", err)
	}
}

// All-zero metrics map to ErrNoData so the registry tries the next
// provider instead of returning a useless snapshot.
func TestYahooProviderAllZeroIsErrNoData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"quoteSummary":{"result":[{"defaultKeyStatistics":{}}]}}`))
	}))
	defer srv.Close()
	p := &YahooProvider{BaseURL: srv.URL}
	_, err := p.Fetch(context.Background(), FetchRequest{Symbol: "X", Market: "us_equity"})
	if !errors.Is(err, ErrNoData) {
		t.Errorf("all-zero should be ErrNoData, got %v", err)
	}
}

// Akshare provider: parses both the wrapped {data: {...}} shape and
// the bare-object shape that different MCP forks use.
func TestAkshareProviderParsesWrappedShape(t *testing.T) {
	const body = `{"code":0,"data":{"pe":28.1,"pb":8.2,"roe":0.32,"net_profit_margin":0.51,"revenue_growth":0.12,"market_cap":2.1e12,"currency":"CNY"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	p := &AkshareProvider{BaseURL: srv.URL}
	m, err := p.Fetch(context.Background(), FetchRequest{Symbol: "600519", Market: "a_share"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if m.PE != 28.1 || m.PB != 8.2 || m.ReturnOnEquity != 0.32 {
		t.Errorf("metrics wrong: %+v", m)
	}
	if m.MarketCap != 2.1e12 || m.Currency != "CNY" {
		t.Errorf("mkt cap / currency wrong: %+v", m)
	}
}

// Akshare bare-object shape (no "data" wrapper).
func TestAkshareProviderParsesBareObject(t *testing.T) {
	const body = `{"PE":15.6,"PB":1.2,"ROE":0.18,"市盈率":15.6}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	p := &AkshareProvider{BaseURL: srv.URL}
	m, err := p.Fetch(context.Background(), FetchRequest{Symbol: "000001", Market: "a_share"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if m.PE != 15.6 || m.PB != 1.2 || m.ReturnOnEquity != 0.18 {
		t.Errorf("PE/PB/ROE wrong: %+v", m)
	}
}

// Akshare Supports() requires BaseURL to claim a market.
func TestAkshareProviderSupportsRequiresBaseURL(t *testing.T) {
	p := &AkshareProvider{}
	if p.Supports("a_share") {
		t.Errorf("Supports should be false without BaseURL")
	}
	with := &AkshareProvider{BaseURL: "http://akshare.local"}
	if !with.Supports("a_share") {
		t.Errorf("default markets should cover a_share")
	}
}

// FormatForPrompt produces a human-readable line.
func TestFormatForPromptProducesReadableLine(t *testing.T) {
	m := &Metrics{
		Symbol:          "AAPL",
		PE:              28.30,
		ForwardPE:       24.10,
		PB:              47.20,
		ProfitMargin:    0.252,
		OperatingMargin: 0.309,
		ReturnOnEquity:  1.567,
		RevenueGrowth:   0.082,
		EarningsGrowth:  0.124,
		MarketCap:       2.85e12,
		Currency:        "USD",
	}
	line := m.FormatForPrompt()
	if !strings.Contains(line, "AAPL:") {
		t.Errorf("missing symbol: %q", line)
	}
	for _, want := range []string{"PE 28.3", "fwd 24.1", "PB 47.2", "ROE 156.7%", "net margin 25.2%", "op margin 30.9%", "rev growth +8.2%", "eps growth +12.4%", "mkt cap 2.85T USD"} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in line: %s", want, line)
		}
	}
}

// FormatForPrompt returns "" when no metric is set so callers skip
// empty bullets.
func TestFormatForPromptEmpty(t *testing.T) {
	m := &Metrics{Symbol: "AAPL"}
	if line := m.FormatForPrompt(); line != "" {
		t.Errorf("expected empty, got %q", line)
	}
}

// formatMarketCap units sanity check.
func TestFormatMarketCapUnits(t *testing.T) {
	cases := map[string]string{
		"K": formatMarketCap(2.5e3, ""),
		"M": formatMarketCap(2.5e6, ""),
		"B": formatMarketCap(2.5e9, "USD"),
		"T": formatMarketCap(2.85e12, "USD"),
	}
	for unit, got := range cases {
		if !strings.Contains(got, unit) {
			t.Errorf("expected unit %s in %q", unit, got)
		}
	}
	if !strings.Contains(formatMarketCap(2.85e12, "USD"), "USD") {
		t.Errorf("currency should be appended")
	}
}
