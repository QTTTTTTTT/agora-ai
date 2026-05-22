package marketdata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetQuotePrefersConfiguredProviderOrder(t *testing.T) {
	chinaCalls := 0
	chinaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chinaCalls++
		_, _ = w.Write([]byte(`{"data":{"price":101.25,"symbol":"AAPL"}}`))
	}))
	defer chinaServer.Close()

	quantCalls := 0
	quantServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		quantCalls++
		_, _ = w.Write([]byte(`{"data":{"price":102.5,"symbol":"AAPL"}}`))
	}))
	defer quantServer.Close()

	service := NewService(Config{
		ChinaStockURL:  chinaServer.URL,
		QuantDingerURL: quantServer.URL,
		QuoteProviders: []string{"quantdinger", "china-stock"},
	})

	quote, err := service.GetQuote(context.Background(), InstrumentRef{Symbol: "AAPL", Market: "us"})
	if err != nil {
		t.Fatalf("get quote: %v", err)
	}
	if quote == nil || quote.Source != "quantdinger" {
		t.Fatalf("expected quantdinger quote, got %#v", quote)
	}
	if quantCalls != 1 {
		t.Fatalf("expected quantdinger to be called once, got %d", quantCalls)
	}
	if chinaCalls != 0 {
		t.Fatalf("expected china-stock not to be called, got %d", chinaCalls)
	}
}

func TestGetNewsPrefersConfiguredProviderOrder(t *testing.T) {
	webSearchCalls := 0
	webSearchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webSearchCalls++
		_, _ = w.Write([]byte(`{"results":[{"title":"web fallback","url":"https://example.com/web"}]}`))
	}))
	defer webSearchServer.Close()

	serpCalls := 0
	serpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serpCalls++
		_, _ = w.Write([]byte(`{"news_results":[{"title":"local first","link":"https://example.com/local","source":"serp"}]}`))
	}))
	defer serpServer.Close()

	service := NewService(Config{
		WebSearchURL:   webSearchServer.URL,
		SerpAPIKeys:    []string{"serp-key"},
		SerpAPIBaseURL: serpServer.URL,
		NewsProviders:  []string{"local-search", "web-search"},
	})

	items, err := service.GetNews(context.Background(), InstrumentRef{Symbol: "AAPL", Market: "us"}, 3)
	if err != nil {
		t.Fatalf("get news: %v", err)
	}
	if len(items) != 1 || items[0].Title != "local first" || items[0].Source != "serp" {
		t.Fatalf("expected local-search news result, got %#v", items)
	}
	if serpCalls != 1 {
		t.Fatalf("expected local serpapi to be called once, got %d", serpCalls)
	}
	if webSearchCalls != 0 {
		t.Fatalf("expected web-search not to be called, got %d", webSearchCalls)
	}
}

func TestGetNewsFallsBackWhenLocalSearchFails(t *testing.T) {
	webSearchCalls := 0
	webSearchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webSearchCalls++
		_, _ = w.Write([]byte(`{"results":[{"title":"web fallback","url":"https://example.com/web"}]}`))
	}))
	defer webSearchServer.Close()

	serpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer serpServer.Close()

	service := NewService(Config{
		WebSearchURL:   webSearchServer.URL,
		SerpAPIKeys:    []string{"serp-key"},
		SerpAPIBaseURL: serpServer.URL,
		NewsProviders:  []string{"local-search", "web-search"},
	})

	items, notes, err := service.GetNewsWithNotes(context.Background(), InstrumentRef{Symbol: "AAPL", Market: "us"}, 3)
	if err != nil {
		t.Fatalf("get news with notes: %v", err)
	}
	if len(items) != 1 || items[0].Title != "web fallback" || items[0].Source != "web-search" {
		t.Fatalf("expected web-search fallback result, got %#v", items)
	}
	if len(notes) == 0 || !strings.Contains(strings.Join(notes, " "), "news temporarily unavailable") {
		t.Fatalf("expected sanitized fallback note, got %#v", notes)
	}
	if webSearchCalls != 1 {
		t.Fatalf("expected web-search fallback to be called once, got %d", webSearchCalls)
	}
}

func TestGetNewsLocalSearchRotatesKeysAfterFailures(t *testing.T) {
	requests := make([]string, 0, 2)
	serpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Query().Get("api_key"))
		switch r.URL.Query().Get("api_key") {
		case "bad-key":
			http.Error(w, "bad key", http.StatusTooManyRequests)
		case "good-key":
			_, _ = w.Write([]byte(`{"news_results":[{"title":"rotated result","link":"https://example.com/rotated","source":"serp"}]}`))
		default:
			http.Error(w, "unexpected key", http.StatusBadRequest)
		}
	}))
	defer serpServer.Close()

	service := NewService(Config{
		SerpAPIKeys:    []string{"bad-key", "good-key"},
		SerpAPIBaseURL: serpServer.URL,
		NewsProviders:  []string{"local-search"},
	})

	items, err := service.GetNews(context.Background(), InstrumentRef{Symbol: "AAPL", Market: "us"}, 3)
	if err != nil {
		t.Fatalf("get news: %v", err)
	}
	if len(items) != 1 || items[0].Title != "rotated result" {
		t.Fatalf("expected rotated result, got %#v", items)
	}
	if strings.Join(requests, ",") != "bad-key,good-key" {
		t.Fatalf("expected key rotation order, got %#v", requests)
	}
}

func TestGetResearchContextReturnsProviderNotesWhenProvidersUnavailable(t *testing.T) {
	service := NewService(Config{
		ChinaStockURL: "http://127.0.0.1:1",
		AkshareURL:    "http://127.0.0.1:1",
	})

	research, err := service.GetResearchContext(context.Background(), InstrumentRef{Symbol: "BTCUSDT", Market: "crypto"}, nil, 3)
	if err != nil {
		t.Fatalf("get research context: %v", err)
	}
	if research == nil {
		t.Fatal("expected research context")
	}
	if research.Summary == "" {
		t.Fatal("expected fallback summary")
	}
	if len(research.ProviderNotes) == 0 {
		t.Fatal("expected provider notes")
	}
	joinedNotes := strings.Join(research.ProviderNotes, " ")
	if !strings.Contains(joinedNotes, "quote unavailable") {
		t.Fatalf("expected quote unavailable note, got %#v", research.ProviderNotes)
	}
	if strings.Contains(joinedNotes, "dial tcp") || strings.Contains(joinedNotes, "127.0.0.1:1") {
		t.Fatalf("expected provider notes to hide raw transport error, got %#v", research.ProviderNotes)
	}
	if research.Quote != nil {
		t.Fatalf("expected no quote, got %#v", research.Quote)
	}
}

func TestBuildResearchSummaryIncludesBenchmarkSymbol(t *testing.T) {
	summary := buildResearchSummary(
		InstrumentRef{Symbol: "AAPL"},
		&QuoteSnapshot{Price: 123.45, Source: "quantdinger"},
		&InstrumentRef{Symbol: "SPY"},
		&QuoteSnapshot{Symbol: "SPY", Price: 512.34},
		[]NewsItem{{Title: "earnings"}},
	)
	for _, expected := range []string{"AAPL market context", "price 123.4500 (quantdinger)", "benchmark SPY 512.3400", "1 news items"} {
		if !strings.Contains(summary, expected) {
			t.Fatalf("expected %q in summary %q", expected, summary)
		}
	}
}

func TestConfigEnabledWithSearchAPIKeys(t *testing.T) {
	cfg := Config{SerpAPIKeys: []string{"serp-key"}}
	if !cfg.normalize().Enabled() {
		t.Fatal("expected config with serpapi key to be enabled")
	}
	if !cfg.normalize().newsProviderEnabled("local-search") {
		t.Fatal("expected local-search provider to be enabled when direct search keys exist")
	}
}

func TestNewsProvidersIgnoreQuantDingerForNewsWhenURLPresent(t *testing.T) {
	service := NewService(Config{
		QuantDingerURL: "http://example.com",
		SerpAPIKeys:    []string{"serp-key"},
	})

	// US instrument: eastmoney/sina must NOT appear (cnstock-only providers).
	providers := service.defaultNewsProviderOrder(InstrumentRef{Symbol: "AAPL", Market: "us"})
	if strings.Join(providers, ",") != "local-search,serpapi" {
		t.Fatalf("expected quantdinger to be excluded from default news order, got %#v", providers)
	}
	if provider := service.newsProviderByName("quantdinger"); provider != nil {
		t.Fatal("expected quantdinger news provider to be disabled")
	}
}

func TestIsTickerLikeSymbol(t *testing.T) {
	cases := []struct {
		name   string
		symbol string
		want   bool
	}{
		{name: "plain ticker", symbol: "NVDA", want: true},
		{name: "colon ticker", symbol: "NASDAQ:NVDA", want: true},
		{name: "free text", symbol: "semiconductor stock market news", want: false},
		{name: "contains chinese", symbol: "存储 行业 新闻", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTickerLikeSymbol(tc.symbol); got != tc.want {
				t.Fatalf("expected %t, got %t for %q", tc.want, got, tc.symbol)
			}
		})
	}
}

func TestSearchQueryForNewsPreservesFreeTextQueries(t *testing.T) {
	instrument := InstrumentRef{Symbol: "semiconductor stock market news", Market: "us_equity", AssetClass: "equity"}
	if got := searchQueryForNews(instrument); got != "semiconductor stock market news" {
		t.Fatalf("expected raw free-text query, got %q", got)
	}
}

func TestSearchQueryForNewsBuildsTickerQuery(t *testing.T) {
	instrument := InstrumentRef{Symbol: "NVDA", Exchange: "NASDAQ", Market: "us_equity", AssetClass: "equity"}
	got := searchQueryForNews(instrument)
	for _, want := range []string{"NVDA", "NASDAQ", "market news"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in query %q", want, got)
		}
	}
}
