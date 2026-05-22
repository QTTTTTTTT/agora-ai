package marketdata

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestGetNewsWithNotesHybridMergesZHAndENChains verifies that when hybrid is
// enabled the service hits both the Eastmoney (ZH) chain and the web-search
// MCP (EN locale) chain and surfaces a merged, deduped list. The Eastmoney
// payload covers the A-share native source while the web-search payload
// supplies an English macro headline.
func TestGetNewsWithNotesHybridMergesZHAndENChains(t *testing.T) {
	eastmoneyHits, websearchHits := 0, 0
	eastmoney := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		eastmoneyHits++
		w.Header().Set("Content-Type", "application/json")
		payload := `jQuery1({"result":{"cmsArticleWebOld":{"data":[` +
			`{"title":"贵州茅台一季度净利增长","content":"公司公告称…","url":"https://eastmoney.example/zh-1","date":"2026-05-18 09:30:01"}` +
			`]}}});`
		_, _ = w.Write([]byte(payload))
	}))
	defer eastmoney.Close()

	websearch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		websearchHits++
		// Capture the locale params the hybrid aggregator sends, to assert
		// EN locale is forced even though the instrument is a CN stock.
		if got := r.URL.Query().Get("hl"); !strings.HasPrefix(got, "en") {
			t.Errorf("expected hl=en-* on EN chain call, got %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"title": "Kweichow Moutai Q1 net profit jumps", "url": "https://en.example/articles/123", "summary": "Reuters"},
			},
		})
	}))
	defer websearch.Close()

	svc := NewService(Config{
		EastmoneyNewsBaseURL: eastmoney.URL,
		WebSearchURL:         websearch.URL,
		NewsProviders:        []string{"eastmoney", "web-search"},
		NewsHybridEnabled:    true,
		QuoteTTL:             time.Second,
		NewsTTL:              time.Second,
	})

	items, _, err := svc.GetNewsWithNotes(context.Background(), InstrumentRef{Symbol: "600519", Market: "cnstock"}, 5)
	if err != nil {
		t.Fatalf("GetNewsWithNotes: %v", err)
	}
	if eastmoneyHits == 0 {
		t.Fatalf("expected eastmoney to be called")
	}
	if websearchHits == 0 {
		t.Fatalf("expected web-search to be called for the EN chain")
	}
	if len(items) < 2 {
		t.Fatalf("expected at least 2 merged items, got %d", len(items))
	}
	titlesByLang := map[string]string{}
	for _, item := range items {
		titlesByLang[item.Language] = item.Title
	}
	if titlesByLang[NewsLanguageZH] == "" {
		t.Fatalf("expected a Chinese item in merged result, got %v", titlesByLang)
	}
	if titlesByLang[NewsLanguageEN] == "" {
		t.Fatalf("expected an English item in merged result, got %v", titlesByLang)
	}
}

func TestGetNewsWithNotesHybridFallsBackToSingleChainWhenDisabled(t *testing.T) {
	eastmoney := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		payload := `jQuery1({"result":{"cmsArticleWebOld":{"data":[` +
			`{"title":"贵州茅台一季度净利增长","url":"https://eastmoney.example/1","date":"2026-05-18 09:30:01"}` +
			`]}}});`
		_, _ = w.Write([]byte(payload))
	}))
	defer eastmoney.Close()
	websearchCalled := false
	websearch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		websearchCalled = true
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{"title": "Should not be hit"}},
		})
	}))
	defer websearch.Close()

	svc := NewService(Config{
		EastmoneyNewsBaseURL: eastmoney.URL,
		WebSearchURL:         websearch.URL,
		NewsProviders:        []string{"eastmoney", "web-search"},
		NewsHybridEnabled:    false,
		QuoteTTL:             time.Second,
		NewsTTL:              time.Second,
	})

	items, _, err := svc.GetNewsWithNotes(context.Background(), InstrumentRef{Symbol: "600519", Market: "cnstock"}, 5)
	if err != nil {
		t.Fatalf("GetNewsWithNotes: %v", err)
	}
	if websearchCalled {
		t.Fatalf("web-search should not be called when hybrid is disabled and eastmoney already returned items")
	}
	if len(items) == 0 || items[0].Language != NewsLanguageZH {
		t.Fatalf("expected single-chain ZH items, got %d items first.lang=%q", len(items), firstLang(items))
	}
}

func firstLang(items []NewsItem) string {
	if len(items) == 0 {
		return ""
	}
	return items[0].Language
}
