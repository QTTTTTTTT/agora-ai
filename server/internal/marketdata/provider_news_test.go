package marketdata

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const eastmoneySampleResponse = `jQuery1({"result":{"cmsArticleWebOld":{"hitCount":2,"data":[` +
	`{"title":"贵州茅台一季度净利润超250亿","content":"业绩超预期","url":"https://finance.eastmoney.com/news/1.html","date":"2025-05-18 09:30:01"},` +
	`{"title":"白酒板块异动","content":"行业资讯","url":"https://finance.eastmoney.com/news/2.html","date":"2025-05-17 15:00:00"}` +
	`]}}});`

// eastmoneyLiveShapeResponse mirrors what search-api-web.eastmoney.com
// actually returns today: cmsArticleWebOld is a *flat array* (not an object
// with a `data` field), titles contain <em> highlight tags, and there is a
// `mediaName` source.
const eastmoneyLiveShapeResponse = `jQuery1({"bizCode":"","bizMsg":"","code":0,"extra":{},"hitsTotal":683,"msg":"OK","result":{"cmsArticleWebOld":[` +
	`{"date":"2026-05-18 16:55:00","image":"","code":"202605183740537634","title":"解密主力资金出逃股 连续<em>5</em>日净流出8<em>00</em>股","content":"<em>600519</em> 贵州茅台 业绩资讯","mediaName":"证券时报网","url":"http://finance.eastmoney.com/a/202605183740537634.html"},` +
	`{"date":"2026-05-18 16:55:00","image":"","code":"202605183740513726","title":"主力动向：5月18日特大单净流出","content":"<em>600519</em> 行业数据","mediaName":"证券时报网","url":"http://finance.eastmoney.com/a/202605183740513726.html"}` +
	`]},"searchId":"c7ca5255-43af-4bc5-9c0e-a34464ac9ca1"})`

func TestEastmoneyNewsProviderParsesJSONPResponse(t *testing.T) {
	var capturedQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/jsonp" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		capturedQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte(eastmoneySampleResponse))
	}))
	defer server.Close()

	service := NewService(Config{
		EastmoneyNewsBaseURL: server.URL,
		NewsProviders:        []string{"eastmoney"},
	})

	items, err := service.GetNews(context.Background(), InstrumentRef{
		Symbol: "600519",
		Market: "cnstock",
	}, 5)
	if err != nil {
		t.Fatalf("get news: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[0].Title != "贵州茅台一季度净利润超250亿" {
		t.Fatalf("expected first item title, got %q", items[0].Title)
	}
	if items[0].Source != "eastmoney" {
		t.Fatalf("expected source eastmoney, got %q", items[0].Source)
	}
	if items[0].URL != "https://finance.eastmoney.com/news/1.html" {
		t.Fatalf("expected url, got %q", items[0].URL)
	}
	if items[0].PublishedAt.IsZero() {
		t.Fatalf("expected non-zero publishedAt, got %v", items[0].PublishedAt)
	}
	if !strings.Contains(capturedQuery, "keyword") {
		t.Fatalf("expected keyword in query string, got %q", capturedQuery)
	}
	if !strings.Contains(capturedQuery, "600519") {
		t.Fatalf("expected stock code 600519 in query, got %q", capturedQuery)
	}
}

func TestEastmoneyNewsProviderParsesLiveFlatArrayShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte(eastmoneyLiveShapeResponse))
	}))
	defer server.Close()

	service := NewService(Config{
		EastmoneyNewsBaseURL: server.URL,
		NewsProviders:        []string{"eastmoney"},
	})

	items, err := service.GetNews(context.Background(), InstrumentRef{
		Symbol: "600519",
		Market: "cnstock",
	}, 5)
	if err != nil {
		t.Fatalf("get news against live-shape payload: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items from live-shape payload, got %d (%+v)", len(items), items)
	}
	wantTitle := "解密主力资金出逃股 连续5日净流出800股"
	if items[0].Title != wantTitle {
		t.Fatalf("expected <em> tags stripped from title: want %q got %q", wantTitle, items[0].Title)
	}
	if strings.Contains(items[0].Summary, "<em>") {
		t.Fatalf("expected <em> tags stripped from summary, got %q", items[0].Summary)
	}
	if items[0].URL != "http://finance.eastmoney.com/a/202605183740537634.html" {
		t.Fatalf("expected url populated, got %q", items[0].URL)
	}
}

func TestStripHTMLTags(t *testing.T) {
	cases := []struct{ in, want string }{
		{in: "解密主力资金出逃股 连续<em>5</em>日净流出8<em>00</em>股", want: "解密主力资金出逃股 连续5日净流出800股"},
		{in: "no tags", want: "no tags"},
		{in: "", want: ""},
		{in: "<p>hello <b>world</b></p>", want: "hello world"},
		{in: "  <em>spaced</em>  ", want: "spaced"},
	}
	for _, c := range cases {
		if got := stripHTMLTags(c.in); got != c.want {
			t.Fatalf("stripHTMLTags(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEastmoneyNewsProviderRejectsNonCNStockMarket(t *testing.T) {
	service := NewService(Config{
		EastmoneyNewsBaseURL: "http://127.0.0.1:1",
		NewsProviders:        []string{"eastmoney"},
	})

	_, _, err := service.GetNewsWithNotes(context.Background(), InstrumentRef{
		Symbol: "AAPL",
		Market: "us",
	}, 3)
	if err == nil {
		t.Fatal("expected error for US market when only eastmoney configured")
	}
}

func TestEastmoneyKeywordPrefers6DigitCode(t *testing.T) {
	cases := []struct {
		name       string
		instrument InstrumentRef
		want       string
	}{
		{name: "bare 6 digit", instrument: InstrumentRef{Symbol: "600519", Market: "cnstock"}, want: "600519"},
		{name: "with .SH suffix", instrument: InstrumentRef{Symbol: "600519.SH", Market: "cnstock"}, want: "600519"},
		{name: "with SH prefix", instrument: InstrumentRef{Symbol: "SH600519", Market: "cnstock"}, want: "600519"},
		{name: "company name", instrument: InstrumentRef{Symbol: "贵州茅台", Market: "cnstock"}, want: "贵州茅台"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := eastmoneyKeyword(tc.instrument); got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

func TestStripJSONPWrapper(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "standard", input: `jQuery1({"a":1})`, want: `{"a":1}`},
		{name: "trailing semicolon", input: `cb({"a":1});`, want: `{"a":1}`},
		{name: "whitespace", input: ` cb( {"a":1} ) `, want: `{"a":1}`},
		{name: "no wrapper", input: `not jsonp`, wantErr: true},
		{name: "empty body", input: `cb()`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := stripJSONPWrapper([]byte(tc.input))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}

const sinaSampleResponse = `{"result":{"status":{"code":0,"msg":""},"data":[` +
	`{"title":"A股早盘三大指数高开","intro":"沪指开盘上涨0.5%","url":"https://finance.sina.com.cn/1.shtml","wapurl":"https://finance.sina.cn/1.shtml","ctime":"1715920201","media_name":"新浪财经"},` +
	`{"title":"600519贵州茅台午后异动拉升","intro":"白酒板块走强","url":"https://finance.sina.com.cn/2.shtml","wapurl":"https://finance.sina.cn/2.shtml","ctime":"1715930000","media_name":"新浪财经"},` +
	`{"title":"白酒板块今日表现强势","intro":"消费股集体走高","url":"https://finance.sina.com.cn/3.shtml","wapurl":"https://finance.sina.cn/3.shtml","ctime":"1715940000","media_name":"新浪财经"}` +
	`]}}`

func TestSinaNewsProviderSoftFiltersBySymbol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/roll/get" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sinaSampleResponse))
	}))
	defer server.Close()

	service := NewService(Config{
		SinaNewsBaseURL: server.URL,
		NewsProviders:   []string{"sina"},
	})

	items, err := service.GetNews(context.Background(), InstrumentRef{
		Symbol: "600519",
		Market: "cnstock",
	}, 5)
	if err != nil {
		t.Fatalf("get news: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 filtered item (matching 600519), got %d items: %#v", len(items), items)
	}
	if !strings.Contains(items[0].Title, "600519") {
		t.Fatalf("expected matched title to contain 600519, got %q", items[0].Title)
	}
	if items[0].Source != "sina" {
		t.Fatalf("expected source sina, got %q", items[0].Source)
	}
	if items[0].PublishedAt.IsZero() {
		t.Fatalf("expected non-zero publishedAt from epoch ctime")
	}
}

func TestSinaNewsProviderFallsBackToTopItemsWhenNoMatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sinaSampleResponse))
	}))
	defer server.Close()

	service := NewService(Config{
		SinaNewsBaseURL: server.URL,
		NewsProviders:   []string{"sina"},
	})

	// 000999 doesn't appear in the sample feed; provider should fall back to
	// top-N items (general A-share market context) rather than return nothing.
	items, err := service.GetNews(context.Background(), InstrumentRef{
		Symbol: "000999",
		Market: "cnstock",
	}, 2)
	if err != nil {
		t.Fatalf("get news: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected fallback top-2 items, got %d", len(items))
	}
}

func TestSinaNewsProviderRejectsNonCNStockMarket(t *testing.T) {
	service := NewService(Config{
		SinaNewsBaseURL: "http://127.0.0.1:1",
		NewsProviders:   []string{"sina"},
	})

	_, _, err := service.GetNewsWithNotes(context.Background(), InstrumentRef{
		Symbol: "AAPL",
		Market: "us",
	}, 3)
	if err == nil {
		t.Fatal("expected error when only sina configured for US market")
	}
}

func TestDefaultNewsProviderOrderRoutesByMarket(t *testing.T) {
	service := NewService(Config{
		WebSearchURL: "http://example.com",
		SerpAPIKeys:  []string{"k"},
	})

	cn := service.defaultNewsProviderOrder(InstrumentRef{Symbol: "600519", Market: "cnstock"})
	if got, want := strings.Join(cn, ","), "eastmoney,sina,local-search,web-search,serpapi"; got != want {
		t.Fatalf("cnstock order mismatch:\n got %q\nwant %q", got, want)
	}

	us := service.defaultNewsProviderOrder(InstrumentRef{Symbol: "AAPL", Market: "us"})
	if got, want := strings.Join(us, ","), "local-search,web-search,serpapi"; got != want {
		t.Fatalf("us order mismatch:\n got %q\nwant %q", got, want)
	}

	hk := service.defaultNewsProviderOrder(InstrumentRef{Symbol: "00700", Market: "hkstock"})
	if got, want := strings.Join(hk, ","), "local-search,web-search,serpapi"; got != want {
		t.Fatalf("hkstock order mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestGetNewsCNStockPrefersEastmoneyOverWebSearch(t *testing.T) {
	webCalls := 0
	webServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webCalls++
		_, _ = w.Write([]byte(`{"results":[{"title":"web fallback","url":"https://example.com"}]}`))
	}))
	defer webServer.Close()

	emCalls := 0
	emServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		emCalls++
		_, _ = w.Write([]byte(eastmoneySampleResponse))
	}))
	defer emServer.Close()

	service := NewService(Config{
		EastmoneyNewsBaseURL: emServer.URL,
		WebSearchURL:         webServer.URL,
	})

	items, err := service.GetNews(context.Background(), InstrumentRef{
		Symbol: "600519",
		Market: "cnstock",
	}, 3)
	if err != nil {
		t.Fatalf("get news: %v", err)
	}
	if len(items) == 0 || items[0].Source != "eastmoney" {
		t.Fatalf("expected eastmoney-sourced items, got %#v", items)
	}
	if emCalls != 1 {
		t.Fatalf("expected eastmoney to be hit once, got %d", emCalls)
	}
	if webCalls != 0 {
		t.Fatalf("expected web-search not to be hit for cnstock, got %d", webCalls)
	}
}

func TestGetNewsCNStockFallsBackToSinaWhenEastmoneyFails(t *testing.T) {
	emServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer emServer.Close()

	sinaCalls := 0
	sinaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sinaCalls++
		_, _ = w.Write([]byte(sinaSampleResponse))
	}))
	defer sinaServer.Close()

	service := NewService(Config{
		EastmoneyNewsBaseURL: emServer.URL,
		SinaNewsBaseURL:      sinaServer.URL,
	})

	items, notes, err := service.GetNewsWithNotes(context.Background(), InstrumentRef{
		Symbol: "600519",
		Market: "cnstock",
	}, 5)
	if err != nil {
		t.Fatalf("get news with notes: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("expected sina fallback items, got none")
	}
	if items[0].Source != "sina" {
		t.Fatalf("expected source sina, got %q", items[0].Source)
	}
	if sinaCalls != 1 {
		t.Fatalf("expected sina to be hit once, got %d", sinaCalls)
	}
	if len(notes) == 0 {
		t.Fatal("expected provider notes from failed eastmoney call")
	}
}

func TestGetNewsUSStockSkipsEastmoneyAndSina(t *testing.T) {
	emCalls := 0
	emServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		emCalls++
		_, _ = w.Write([]byte(eastmoneySampleResponse))
	}))
	defer emServer.Close()

	webServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"title":"us news","url":"https://example.com"}]}`))
	}))
	defer webServer.Close()

	service := NewService(Config{
		EastmoneyNewsBaseURL: emServer.URL,
		WebSearchURL:         webServer.URL,
	})

	items, err := service.GetNews(context.Background(), InstrumentRef{
		Symbol: "AAPL",
		Market: "us",
	}, 3)
	if err != nil {
		t.Fatalf("get news: %v", err)
	}
	if len(items) == 0 || items[0].Source != "web-search" {
		t.Fatalf("expected web-search items for US, got %#v", items)
	}
	if emCalls != 0 {
		t.Fatalf("expected eastmoney not to be hit for US, got %d", emCalls)
	}
}

func TestSinaNewsProviderToleratesAPIErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":{"status":{"code":1,"msg":"throttled"},"data":[]}}`))
	}))
	defer server.Close()

	// Important: defaultNewsProviderOrder auto-prepends eastmoney for cnstock
	// instruments. To isolate the sina behaviour under test we point
	// eastmoney at a dead endpoint as well, otherwise the legacy chain would
	// happily fall through to the live Eastmoney upstream and the test would
	// only assert "did the chain pick anything up at all".
	service := NewService(Config{
		SinaNewsBaseURL:      server.URL,
		EastmoneyNewsBaseURL: "http://127.0.0.1:1",
		NewsProviders:        []string{"sina"},
	})

	_, _, err := service.GetNewsWithNotes(context.Background(), InstrumentRef{
		Symbol: "600519",
		Market: "cnstock",
	}, 3)
	if err == nil {
		t.Fatal("expected error when sina returns error status")
	}
	if !strings.Contains(err.Error(), "throttled") && !strings.Contains(err.Error(), "news temporarily unavailable") {
		t.Fatalf("expected sina error in message, got %v", err)
	}
}

func TestEastmoneyNewsProviderRespectsContextDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		_, _ = w.Write([]byte(eastmoneySampleResponse))
	}))
	defer server.Close()

	service := NewService(Config{
		EastmoneyNewsBaseURL: server.URL,
		NewsProviders:        []string{"eastmoney"},
		ProviderTimeout:      30 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, err := service.GetNewsWithNotes(ctx, InstrumentRef{
		Symbol: "600519",
		Market: "cnstock",
	}, 3)
	if err == nil {
		t.Fatal("expected timeout/cancellation error")
	}
}

// Sanity: ensure the eastmoney provider hits the configured base URL exactly
// once per call (no implicit retries).
func TestEastmoneyNewsProviderSingleRequestPerCall(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(eastmoneySampleResponse))
	}))
	defer server.Close()

	service := NewService(Config{
		EastmoneyNewsBaseURL: server.URL,
		NewsProviders:        []string{"eastmoney"},
		NewsTTL:              0,
	})

	for i := 0; i < 3; i++ {
		instrument := InstrumentRef{
			Symbol: fmt.Sprintf("60051%d", i),
			Market: "cnstock",
		}
		if _, err := service.GetNews(context.Background(), instrument, 3); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
	if calls != 3 {
		t.Fatalf("expected exactly 3 calls, got %d", calls)
	}
}
