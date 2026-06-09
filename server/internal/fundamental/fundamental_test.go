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

// TestAkshareProviderConsumesSidecarPayload pins the wire-format
// contract between the Go provider and the in-repo Python sidecar at
// services/akshare-fundamental. The body is a verbatim sample
// captured from a live `/api/fundamental?symbol=688205&market=a_share`
// call against the sidecar — if the sidecar ever drops or renames a
// field the Go parser depends on, this test fails before the change
// hits production.
//
// The sidecar deliberately ships only the statement-derived metrics
// (ROE / margins / growth) because eastmoney's live-quote endpoint
// (`push2.eastmoney.com`) is not reliably reachable from outside CN
// and so PE / PB / market_cap aren't included. If a future deployment
// adds those, extend this fixture rather than relaxing the parser.
func TestAkshareProviderConsumesSidecarPayload(t *testing.T) {
	// Captured from a live `/api/fundamental?symbol=688205` call
	// after the sidecar started shipping annual + latest-period
	// growth side-by-side, plus the listing-tenure block. The
	// 2025 annual prints earnings down -28.77% but the 2026-Q1
	// print already shows +35.08% — surfacing both is what stops
	// Wood's persona from giving stale AVOID verdicts on names
	// that just inflected. listing_date / listing_years are
	// surfaced so 10y-horizon personas can stop flagging
	// "history.10yr data_unavailable" on a 2022-IPO 次新股.
	// gross_margin_latest / latest_revenue / latest_net_income /
	// latest_*_qoq / latest_announce_date / latest_source — added
	// for rule-8 citation support. Without these the LLM can quote
	// a percent like '+27.97%' but never an absolute number or an
	// announce date, and a reviewer can't trace the figure back to
	// the source filing.
	const body = `{"data":{"annual_period":"2025-12-31","currency":"CNY",` +
		`"earnings_growth":-0.287683,"earnings_growth_latest":0.350823,` +
		`"gross_margin_latest":0.257339594,` +
		`"latest_announce_date":"2026-04-28",` +
		`"latest_net_income":19639010,"latest_net_income_qoq":-0.375526,` +
		`"latest_period":"2026-03-31",` +
		`"latest_revenue":254444250,"latest_revenue_qoq":-0.096262,` +
		`"latest_source":"eastmoney_yjbb",` +
		`"listing_date":"2022-08-09","listing_years":3.83,` +
		`"name":"德科立",` +
		`"operating_margin":0.07761,"profit_margin":0.076629,` +
		`"revenue_growth":0.109933,"revenue_growth_latest":0.279698,` +
		`"roe":0.0309,"symbol":"688205"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	p := &AkshareProvider{BaseURL: srv.URL}

	m, err := p.Fetch(context.Background(), FetchRequest{Symbol: "688205", Market: "a_share"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if m.Name != "德科立" {
		t.Errorf("name got %q want %q", m.Name, "德科立")
	}
	if m.AnnualPeriod != "2025-12-31" {
		t.Errorf("AnnualPeriod got %q want 2025-12-31", m.AnnualPeriod)
	}
	if m.LatestPeriod != "2026-03-31" {
		t.Errorf("LatestPeriod got %q want 2026-03-31", m.LatestPeriod)
	}
	if m.RevenueGrowthLatest != 0.279698 {
		t.Errorf("RevenueGrowthLatest got %v want 0.279698", m.RevenueGrowthLatest)
	}
	if m.EarningsGrowthLatest != 0.350823 {
		t.Errorf("EarningsGrowthLatest got %v want 0.350823", m.EarningsGrowthLatest)
	}
	if m.ReturnOnEquity != 0.0309 {
		t.Errorf("roe got %v want 0.0309", m.ReturnOnEquity)
	}
	if m.ProfitMargin != 0.076629 {
		t.Errorf("profit_margin got %v want 0.076629", m.ProfitMargin)
	}
	if m.OperatingMargin != 0.07761 {
		t.Errorf("operating_margin got %v want 0.07761", m.OperatingMargin)
	}
	if m.RevenueGrowth != 0.109933 {
		t.Errorf("revenue_growth got %v want 0.109933", m.RevenueGrowth)
	}
	if m.EarningsGrowth != -0.287683 {
		t.Errorf("earnings_growth got %v want -0.287683", m.EarningsGrowth)
	}
	if m.ListingDate != "2022-08-09" {
		t.Errorf("ListingDate got %q want 2022-08-09", m.ListingDate)
	}
	if m.ListingYears != 3.83 {
		t.Errorf("ListingYears got %v want 3.83", m.ListingYears)
	}
	// Citation block: announce_date / absolute revenue + net
	// income / QoQ deltas / latest gross margin / provenance tag.
	// Each one is the input rule-8 in master_agent.go uses to
	// force the LLM to write '2026Q1（公告日 2026-04-28）营收
	// 2.54 亿元，同比 +27.97%' style citations.
	if m.LatestAnnounceDate != "2026-04-28" {
		t.Errorf("LatestAnnounceDate got %q want 2026-04-28", m.LatestAnnounceDate)
	}
	if m.LatestRevenue != 254444250 {
		t.Errorf("LatestRevenue got %v want 254444250", m.LatestRevenue)
	}
	if m.LatestNetIncome != 19639010 {
		t.Errorf("LatestNetIncome got %v want 19639010", m.LatestNetIncome)
	}
	if m.LatestRevenueQoQ != -0.096262 {
		t.Errorf("LatestRevenueQoQ got %v want -0.096262", m.LatestRevenueQoQ)
	}
	if m.LatestNetIncomeQoQ != -0.375526 {
		t.Errorf("LatestNetIncomeQoQ got %v want -0.375526", m.LatestNetIncomeQoQ)
	}
	if m.GrossMarginLatest != 0.257339594 {
		t.Errorf("GrossMarginLatest got %v want 0.257339594", m.GrossMarginLatest)
	}
	if m.LatestSource != "eastmoney_yjbb" {
		t.Errorf("LatestSource got %q want eastmoney_yjbb", m.LatestSource)
	}
	if m.Currency != "CNY" {
		t.Errorf("currency got %q want CNY", m.Currency)
	}
	// FormatForPrompt is the actual sink into the LLM prompt — pin
	// that the percent-formatting and ordering survive the
	// round-trip, since this is the string the master agents see.
	got := m.FormatForPrompt()
	for _, want := range []string{
		"688205:", "ROE 3.1%", "net margin 7.7%", "op margin 7.8%",
		"rev growth +11.0%", "eps growth -28.8%",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("FormatForPrompt missing %q in %q", want, got)
		}
	}
}

// TestAkshareNameAliases pins the alternate Chinese-language keys
// the sidecar (or a future provider) might use to ship the issuer
// name. parseAkshareMetrics has to recognise all of them so a
// schema rename on the upstream side doesn't silently drop the name.
//
// We exercise parseAkshareMetrics through its real signature
// (raw JSON body + symbol) — that's also what the HTTP path uses,
// so the test covers JSON decoding alias-resolution end-to-end.
func TestAkshareNameAliases(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"canonical_name", `{"name":"德科立","roe":0.05}`, "德科立"},
		{"stock_name", `{"stock_name":"贵州茅台","roe":0.4}`, "贵州茅台"},
		{"company_name", `{"company_name":"宁德时代","roe":0.2}`, "宁德时代"},
		{"zh_证券简称", `{"证券简称":"比亚迪","roe":0.18}`, "比亚迪"},
		{"zh_股票简称", `{"股票简称":"平安银行","roe":0.1}`, "平安银行"},
		{"zh_证券名称", `{"证券名称":"中国平安","roe":0.12}`, "中国平安"},
		{"missing_name", `{"roe":0.1}`, ""},
		{"wrapped_data", `{"data":{"name":"德科立","roe":0.05}}`, "德科立"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := parseAkshareMetrics([]byte(tc.body), "688205")
			if err != nil {
				t.Fatalf("parseAkshareMetrics: %v", err)
			}
			if m.Name != tc.want {
				t.Errorf("Name got %q want %q", m.Name, tc.want)
			}
		})
	}
}

// TestAkshareListingTenureAliases pins the field-name aliases the
// parser accepts for ListingDate / ListingYears. The sidecar ships
// canonical English names today, but providers wired in from other
// channels (e.g. a future eastmoney route, or a Chinese-language
// dump) may use 上市日期 / 上市年限 instead. Locking the aliases
// down here means a schema rename on the upstream side surfaces as
// a test failure rather than a silent prompt regression.
func TestAkshareListingTenureAliases(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantDate  string
		wantYears float64
	}{
		{"canonical_english", `{"listing_date":"2022-08-09","listing_years":3.83,"roe":0.03}`, "2022-08-09", 3.83},
		{"zh_chinese_keys", `{"上市日期":"2022-08-09","上市年限":3.83,"roe":0.03}`, "2022-08-09", 3.83},
		{"alt_ipo_date", `{"ipo_date":"2022-08-09","listing_age_years":3.83,"roe":0.03}`, "2022-08-09", 3.83},
		{"date_only_no_tenure", `{"listing_date":"2022-08-09","roe":0.03}`, "2022-08-09", 0},
		{"absent_both", `{"roe":0.03}`, "", 0},
		{"wrapped_data_envelope", `{"data":{"listing_date":"2022-08-09","listing_years":3.83,"roe":0.03}}`, "2022-08-09", 3.83},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := parseAkshareMetrics([]byte(tc.body), "688205")
			if err != nil {
				t.Fatalf("parseAkshareMetrics: %v", err)
			}
			if m.ListingDate != tc.wantDate {
				t.Errorf("ListingDate got %q want %q", m.ListingDate, tc.wantDate)
			}
			if m.ListingYears != tc.wantYears {
				t.Errorf("ListingYears got %v want %v", m.ListingYears, tc.wantYears)
			}
		})
	}
}

// TestAkshareCitationMetadataAliases pins the alternate field names
// parseAkshareMetrics accepts for the rule-8 citation block (absolute
// revenue / net income, announce date, QoQ deltas, gross margin,
// source tag). The canonical sidecar (services/akshare-fundamental)
// emits English snake_case keys, but a future provider could ship
// the same payload using the Chinese column names from the upstream
// 业绩快报 endpoint or with slight naming variations. Locking the
// aliases here means a schema rename surfaces as a test failure
// rather than a silent prompt regression where the LLM stops citing
// announce dates.
func TestAkshareCitationMetadataAliases(t *testing.T) {
	cases := []struct {
		name string
		body string
		// Pointer-style assertion so an absent field stays at the
		// zero value rather than getting compared as 0 == ""; we
		// only set the field we're exercising in each case.
		check func(*testing.T, *Metrics)
	}{
		{
			"canonical_english",
			`{"latest_revenue":254444250,"latest_net_income":19639010,` +
				`"latest_announce_date":"2026-04-28","latest_revenue_qoq":-0.0963,` +
				`"latest_net_income_qoq":-0.3755,"gross_margin_latest":0.2573,` +
				`"latest_source":"eastmoney_yjbb","roe":0.03}`,
			func(t *testing.T, m *Metrics) {
				if m.LatestRevenue != 254444250 {
					t.Errorf("LatestRevenue got %v", m.LatestRevenue)
				}
				if m.LatestAnnounceDate != "2026-04-28" {
					t.Errorf("LatestAnnounceDate got %q", m.LatestAnnounceDate)
				}
				if m.GrossMarginLatest != 0.2573 {
					t.Errorf("GrossMarginLatest got %v", m.GrossMarginLatest)
				}
				if m.LatestSource != "eastmoney_yjbb" {
					t.Errorf("LatestSource got %q", m.LatestSource)
				}
			},
		},
		{
			"alt_period_prefix",
			`{"latest_period_revenue":254444250,"latest_period_net_income":19639010,` +
				`"announce_date":"2026-04-28","revenue_growth_qoq":-0.0963,` +
				`"earnings_growth_qoq":-0.3755,"gross_margin_q":0.2573,` +
				`"数据源":"sina","roe":0.03}`,
			func(t *testing.T, m *Metrics) {
				if m.LatestRevenue != 254444250 {
					t.Errorf("LatestRevenue got %v (alt key)", m.LatestRevenue)
				}
				if m.LatestNetIncome != 19639010 {
					t.Errorf("LatestNetIncome got %v (alt key)", m.LatestNetIncome)
				}
				if m.LatestAnnounceDate != "2026-04-28" {
					t.Errorf("LatestAnnounceDate got %q (alt key)", m.LatestAnnounceDate)
				}
				if m.LatestRevenueQoQ != -0.0963 {
					t.Errorf("LatestRevenueQoQ got %v (alt key)", m.LatestRevenueQoQ)
				}
				if m.LatestNetIncomeQoQ != -0.3755 {
					t.Errorf("LatestNetIncomeQoQ got %v (alt key)", m.LatestNetIncomeQoQ)
				}
				if m.GrossMarginLatest != 0.2573 {
					t.Errorf("GrossMarginLatest got %v (alt key)", m.GrossMarginLatest)
				}
				if m.LatestSource != "sina" {
					t.Errorf("LatestSource got %q (zh alias)", m.LatestSource)
				}
			},
		},
		{
			"zh_chinese_keys",
			`{"营业总收入_最新":254444250,"净利润_最新":19639010,` +
				`"最新公告日期":"2026-04-28","营收环比":-0.0963,` +
				`"净利润环比":-0.3755,"销售毛利率":0.2573,"roe":0.03}`,
			func(t *testing.T, m *Metrics) {
				if m.LatestRevenue != 254444250 {
					t.Errorf("LatestRevenue got %v (zh key)", m.LatestRevenue)
				}
				if m.LatestAnnounceDate != "2026-04-28" {
					t.Errorf("LatestAnnounceDate got %q (zh key)", m.LatestAnnounceDate)
				}
				if m.LatestRevenueQoQ != -0.0963 {
					t.Errorf("LatestRevenueQoQ got %v (zh key)", m.LatestRevenueQoQ)
				}
				if m.GrossMarginLatest != 0.2573 {
					t.Errorf("GrossMarginLatest got %v (zh key)", m.GrossMarginLatest)
				}
			},
		},
		{
			// Defensive: sidecar drops the entire citation block
			// (older sidecar version, or eastmoney_yjbb upstream
			// failure). Parser should leave the fields at zero
			// rather than panicking.
			"absent_block",
			`{"roe":0.03}`,
			func(t *testing.T, m *Metrics) {
				if m.LatestRevenue != 0 || m.LatestAnnounceDate != "" || m.LatestSource != "" {
					t.Errorf("expected zero-valued citation block, got %+v", m)
				}
			},
		},
		{
			"wrapped_data_envelope",
			`{"data":{"latest_revenue":254444250,"latest_announce_date":"2026-04-28","roe":0.03}}`,
			func(t *testing.T, m *Metrics) {
				if m.LatestRevenue != 254444250 {
					t.Errorf("LatestRevenue got %v (envelope)", m.LatestRevenue)
				}
				if m.LatestAnnounceDate != "2026-04-28" {
					t.Errorf("LatestAnnounceDate got %q (envelope)", m.LatestAnnounceDate)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := parseAkshareMetrics([]byte(tc.body), "688205")
			if err != nil {
				t.Fatalf("parseAkshareMetrics: %v", err)
			}
			tc.check(t, m)
		})
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
