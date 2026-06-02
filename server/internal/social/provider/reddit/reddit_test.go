package reddit

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
		"data": {
			"children": [
				{"data": {"id":"abc","title":"AAPL going to the moon","selftext":"big news","permalink":"/r/wallstreetbets/comments/abc/aapl/","score":420,"created_utc":1700000000}},
				{"data": {"id":"def","title":"","selftext":"empty title should be dropped","permalink":"/r/wallstreetbets/comments/def/x/","score":50,"created_utc":1700000100}},
				{"data": {"id":"ghi","title":"AAPL low score","selftext":"","permalink":"/r/wallstreetbets/comments/ghi/x/","score":1,"created_utc":1700000200}}
			]
		}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got == "" {
			t.Errorf("expected UA header to be set")
		}
		if got := r.URL.Query().Get("q"); !strings.Contains(got, "AAPL") {
			t.Errorf("expected symbol in q, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	p := New(Options{Endpoint: srv.URL, MinUpvotes: 10})
	items, err := p.FetchPosts(context.Background(), social.Request{Symbol: "AAPL", Limit: 10})
	if err != nil {
		t.Fatalf("FetchPosts err=%v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item after filters, got %d (%+v)", len(items), items)
	}
	got := items[0]
	if got.ID != "reddit:abc" {
		t.Fatalf("ID got=%q", got.ID)
	}
	if got.Source != string(social.PlatformRedditWSB) {
		t.Fatalf("source mismatch %q", got.Source)
	}
	if !strings.Contains(got.URL, "/r/wallstreetbets/comments/abc/aapl/") {
		t.Fatalf("URL mismatch %q", got.URL)
	}
	if got.Symbols[0] != "AAPL" {
		t.Fatalf("symbol mismatch %v", got.Symbols)
	}
	if got.Language != "en" {
		t.Fatalf("expected en language, got %q", got.Language)
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
		t.Fatalf("expected 429 error got %v", err)
	}
}

func TestProvider_FetchPosts_SymbolRequired(t *testing.T) {
	p := New(Options{})
	if _, err := p.FetchPosts(context.Background(), social.Request{}); err == nil {
		t.Fatalf("expected error on empty symbol")
	}
}

func TestProvider_Platform(t *testing.T) {
	if New(Options{}).Platform() != social.PlatformRedditWSB {
		t.Fatalf("wrong platform tag")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello world", 5); got != "hello…" {
		t.Fatalf("got %q", got)
	}
	if got := truncate("short", 100); got != "short" {
		t.Fatalf("got %q", got)
	}
	if got := truncate("   ", 5); got != "" {
		t.Fatalf("expected empty got %q", got)
	}
}
