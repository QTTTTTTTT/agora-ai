package stocktwits

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fundai/server/internal/social"
)

func TestProvider_FetchPosts_DecodesAndMaps(t *testing.T) {
	body := `{
		"messages": [
			{"id": 1, "body": "TSLA is going parabolic\n2nd line", "created_at": "2025-01-02T15:04:05Z"},
			{"id": 2, "body": "", "created_at": "2025-01-02T15:05:05Z"},
			{"id": 3, "body": "bearish on TSLA", "created_at": "bad-timestamp"}
		]
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/TSLA.json") {
			t.Errorf("expected /TSLA.json path got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	p := New(Options{Endpoint: srv.URL})
	items, err := p.FetchPosts(context.Background(), social.Request{Symbol: "tsla", Limit: 5})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items (skip empty body), got %d (%+v)", len(items), items)
	}
	first := items[0]
	if first.ID != "stocktwits:1" {
		t.Fatalf("ID mismatch %q", first.ID)
	}
	if first.Title == "" || strings.Contains(first.Title, "\n") {
		t.Fatalf("title should be first line, got %q", first.Title)
	}
	if first.Symbols[0] != "TSLA" {
		t.Fatalf("symbol should be uppercased got %v", first.Symbols)
	}
	if first.Language != "en" {
		t.Fatalf("expected en got %q", first.Language)
	}
}

func TestProvider_FetchPosts_LimitTrim(t *testing.T) {
	body := `{"messages":[
		{"id":1,"body":"a","created_at":"2025-01-02T15:04:05Z"},
		{"id":2,"body":"b","created_at":"2025-01-02T15:04:05Z"},
		{"id":3,"body":"c","created_at":"2025-01-02T15:04:05Z"}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	p := New(Options{Endpoint: srv.URL})
	got, err := p.FetchPosts(context.Background(), social.Request{Symbol: "AAA", Limit: 2})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 (limit), got %d", len(got))
	}
}

func TestProvider_FetchPosts_AccessToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("access_token"); got != "tok-123" {
			t.Errorf("expected access_token forwarded, got %q", got)
		}
		_, _ = w.Write([]byte(`{"messages":[]}`))
	}))
	defer srv.Close()
	p := New(Options{Endpoint: srv.URL, AccessToken: "tok-123"})
	if _, err := p.FetchPosts(context.Background(), social.Request{Symbol: "AAPL"}); err != nil {
		t.Fatalf("err=%v", err)
	}
}

func TestProvider_FetchPosts_HTTPNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "ratelimited", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	p := New(Options{Endpoint: srv.URL})
	_, err := p.FetchPosts(context.Background(), social.Request{Symbol: "AAPL"})
	if err == nil || !strings.Contains(err.Error(), "status 429") {
		t.Fatalf("expected 429 got %v", err)
	}
}

func TestProvider_SymbolRequired(t *testing.T) {
	p := New(Options{})
	if _, err := p.FetchPosts(context.Background(), social.Request{}); err == nil {
		t.Fatalf("expected error")
	}
}

func TestProvider_Platform(t *testing.T) {
	if New(Options{}).Platform() != social.PlatformStockTwits {
		t.Fatalf("wrong platform tag")
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("hello\nworld"); got != "hello" {
		t.Fatalf("got %q", got)
	}
	if got := firstLine("   "); got != "" {
		t.Fatalf("got %q", got)
	}
}
