package marketdata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestNormalizeLanguageHint(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{in: "", want: ""},
		{in: "  ", want: ""},
		{in: "zh", want: LanguageHintZH},
		{in: "zh-CN", want: LanguageHintZH},
		{in: "zh-Hans", want: LanguageHintZH},
		{in: "ZH-HK", want: LanguageHintZH},
		{in: "en", want: LanguageHintEN},
		{in: "en-US", want: LanguageHintEN},
		{in: "EN_GB", want: LanguageHintEN},
		{in: "fr-FR", want: ""},
		{in: "ja-JP", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := normalizeLanguageHint(tc.in); got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

func TestWithLanguageAndLanguageHint(t *testing.T) {
	ctx := context.Background()
	if got := LanguageHint(ctx); got != "" {
		t.Fatalf("expected empty hint on bare ctx, got %q", got)
	}
	ctx = WithLanguage(ctx, "zh-CN")
	if got := LanguageHint(ctx); got != LanguageHintZH {
		t.Fatalf("expected zh-CN, got %q", got)
	}
	ctx = WithLanguage(ctx, "en-US")
	if got := LanguageHint(ctx); got != LanguageHintEN {
		t.Fatalf("expected en-US after overwrite, got %q", got)
	}
	// Unknown value should be ignored (preserve previous hint).
	ctx = WithLanguage(ctx, "fr-FR")
	if got := LanguageHint(ctx); got != LanguageHintEN {
		t.Fatalf("expected fr-FR to be ignored, got %q", got)
	}
}

func TestContainsCJK(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{in: "AAPL", want: false},
		{in: "600519 A-share stock market news", want: false},
		{in: "贵州茅台", want: true},
		{in: "Apple 财报", want: true},
		{in: "BTCUSDT", want: false},
		{in: "存储 行业 新闻", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := containsCJK(tc.in); got != tc.want {
				t.Fatalf("expected %t, got %t for %q", tc.want, got, tc.in)
			}
		})
	}
}

func TestWebSearchLocaleForCNStockUsesZH(t *testing.T) {
	loc := webSearchLocaleFor(context.Background(), "600519 A-share stock market news", InstrumentRef{
		Symbol: "600519",
		Market: "cnstock",
	})
	if loc.hl != "zh-CN" || loc.gl != "CN" || loc.ceid != "CN:zh-Hans" {
		t.Fatalf("expected zh-CN locale for cnstock, got %#v", loc)
	}
}

func TestWebSearchLocaleForHKStockUsesZH(t *testing.T) {
	loc := webSearchLocaleFor(context.Background(), "00700 market news", InstrumentRef{
		Symbol: "00700",
		Market: "hkstock",
	})
	if loc.hl != "zh-CN" {
		t.Fatalf("expected zh-CN for hkstock, got %#v", loc)
	}
}

func TestWebSearchLocaleForUSStockReturnsEmpty(t *testing.T) {
	loc := webSearchLocaleFor(context.Background(), "AAPL US stock market news", InstrumentRef{
		Symbol: "AAPL",
		Market: "us_equity",
	})
	if loc.hl != "" || loc.gl != "" || loc.ceid != "" {
		t.Fatalf("expected empty (MCP default) locale for US, got %#v", loc)
	}
}

func TestWebSearchLocaleForCJKQueryUsesZH(t *testing.T) {
	loc := webSearchLocaleFor(context.Background(), "存储 行业 新闻", InstrumentRef{
		Symbol: "semiconductor storage",
		Market: "us_equity",
	})
	if loc.hl != "zh-CN" {
		t.Fatalf("expected CJK content to force zh-CN, got %#v", loc)
	}
}

func TestWebSearchLocaleContextHintOverridesMarket(t *testing.T) {
	// Even for a cnstock instrument, an explicit en-US hint wins.
	ctx := WithLanguage(context.Background(), "en-US")
	loc := webSearchLocaleFor(ctx, "600519", InstrumentRef{
		Symbol: "600519",
		Market: "cnstock",
	})
	if loc.hl != "en-US" || loc.gl != "US" || loc.ceid != "US:en" {
		t.Fatalf("expected en-US to override cnstock default, got %#v", loc)
	}
}

func TestWebSearchLocaleContextHintForcesZHForUSInstrument(t *testing.T) {
	ctx := WithLanguage(context.Background(), "zh-CN")
	loc := webSearchLocaleFor(ctx, "AAPL US stock market news", InstrumentRef{
		Symbol: "AAPL",
		Market: "us_equity",
	})
	if loc.hl != "zh-CN" {
		t.Fatalf("expected zh-CN hint to win, got %#v", loc)
	}
}

func TestFetchSearchNewsAtSendsLocaleParamsForCNStock(t *testing.T) {
	var capturedQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.Query()
		_, _ = w.Write([]byte(`{"results":[{"title":"中文新闻","url":"https://example.com/a"}]}`))
	}))
	defer server.Close()

	service := NewService(Config{
		WebSearchURL:  server.URL,
		NewsProviders: []string{"web-search"},
	})

	// A-share instrument: should auto-pick zh-CN locale even without context hint.
	items, err := service.GetNews(context.Background(), InstrumentRef{
		Symbol: "600519",
		Market: "cnstock",
	}, 3)
	if err != nil {
		t.Fatalf("get news: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("expected items")
	}
	if got := capturedQuery.Get("hl"); got != "zh-CN" {
		t.Fatalf("expected hl=zh-CN, got %q", got)
	}
	if got := capturedQuery.Get("gl"); got != "CN" {
		t.Fatalf("expected gl=CN, got %q", got)
	}
	if got := capturedQuery.Get("ceid"); got != "CN:zh-Hans" {
		t.Fatalf("expected ceid=CN:zh-Hans, got %q", got)
	}
}

func TestFetchSearchNewsAtOmitsLocaleForUSStock(t *testing.T) {
	var capturedQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.Query()
		_, _ = w.Write([]byte(`{"results":[{"title":"english","url":"https://example.com/a"}]}`))
	}))
	defer server.Close()

	service := NewService(Config{
		WebSearchURL:  server.URL,
		NewsProviders: []string{"web-search"},
	})

	if _, err := service.GetNews(context.Background(), InstrumentRef{
		Symbol: "AAPL",
		Market: "us_equity",
	}, 3); err != nil {
		t.Fatalf("get news: %v", err)
	}
	if got := capturedQuery.Get("hl"); got != "" {
		t.Fatalf("expected no hl for US default, got %q", got)
	}
	if got := capturedQuery.Get("gl"); got != "" {
		t.Fatalf("expected no gl for US default, got %q", got)
	}
}

func TestFetchSearchNewsAtRespectsContextLanguageHint(t *testing.T) {
	var capturedQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.Query()
		_, _ = w.Write([]byte(`{"results":[{"title":"中文 by request hint","url":"https://example.com/a"}]}`))
	}))
	defer server.Close()

	service := NewService(Config{
		WebSearchURL:  server.URL,
		NewsProviders: []string{"web-search"},
	})

	ctx := WithLanguage(context.Background(), "zh-CN")
	if _, err := service.GetNews(ctx, InstrumentRef{
		Symbol: "AAPL",
		Market: "us_equity",
	}, 3); err != nil {
		t.Fatalf("get news: %v", err)
	}
	if got := capturedQuery.Get("hl"); got != "zh-CN" {
		t.Fatalf("expected hl=zh-CN from ctx hint, got %q", got)
	}
}
