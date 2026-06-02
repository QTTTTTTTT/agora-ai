package xueqiu

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
		"error_code": 0,
		"data": {
			"items": [
				{"id": 100, "user_id": 9, "text": "<p>看好<a href='x'>$茅台</a></p>", "created_at": 1700000000000, "target":"https://xueqiu.com/9/100"},
				{"id": 101, "user_id": 9, "text": "  ", "created_at": 1700000010000, "target": ""},
				{"id": 102, "user_id": 9, "text": "卖出", "created_at": 1700000020000, "target": ""}
			]
		}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("symbol"); got != "SH600519" {
			t.Errorf("expected symbol forwarded got %q", got)
		}
		if got := r.Header.Get("Cookie"); !strings.Contains(got, "xq_a_token=") {
			t.Errorf("expected guest cookie forwarded got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	p := New(Options{Endpoint: srv.URL, GuestCookie: "abc123"})
	items, err := p.FetchPosts(context.Background(), social.Request{Symbol: "sh600519", Limit: 5})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items (skip blank), got %d (%+v)", len(items), items)
	}
	first := items[0]
	if first.ID != "xueqiu:100" {
		t.Fatalf("ID mismatch %q", first.ID)
	}
	if !strings.Contains(first.Summary, "看好") || strings.Contains(first.Summary, "<a") {
		t.Fatalf("HTML not stripped: %q", first.Summary)
	}
	if first.Language != "zh" {
		t.Fatalf("expected zh got %q", first.Language)
	}
	if first.Symbols[0] != "SH600519" {
		t.Fatalf("symbol mismatch %v", first.Symbols)
	}
	if first.URL != "https://xueqiu.com/9/100" {
		t.Fatalf("URL mismatch %q", first.URL)
	}
}

func TestProvider_FetchPosts_APIError(t *testing.T) {
	body := `{"error_code": 400, "error_description": "no token"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	p := New(Options{Endpoint: srv.URL, GuestCookie: "x"})
	_, err := p.FetchPosts(context.Background(), social.Request{Symbol: "AAPL"})
	if err == nil || !strings.Contains(err.Error(), "no token") {
		t.Fatalf("expected api error, got %v", err)
	}
}

func TestProvider_FetchPosts_HTTPNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer srv.Close()
	p := New(Options{Endpoint: srv.URL})
	_, err := p.FetchPosts(context.Background(), social.Request{Symbol: "AAPL"})
	if err == nil || !strings.Contains(err.Error(), "status 403") {
		t.Fatalf("expected 403 got %v", err)
	}
}

func TestProvider_SymbolRequired(t *testing.T) {
	p := New(Options{})
	if _, err := p.FetchPosts(context.Background(), social.Request{}); err == nil {
		t.Fatalf("expected error")
	}
}

func TestProvider_Platform(t *testing.T) {
	if New(Options{}).Platform() != social.PlatformXueqiu {
		t.Fatalf("wrong platform tag")
	}
}

func TestStripHTML(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"plain", "plain"},
		{"<p>hi <a href='x'>link</a></p>", "hi link"},
		{"line1<br/>line2", "line1\nline2"},
		{"<div>boom</div>", "boom"},
	}
	for _, c := range cases {
		if got := stripHTML(c.in); got != c.want {
			t.Errorf("stripHTML(%q) = %q want %q", c.in, got, c.want)
		}
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("hello\nworld"); got != "hello" {
		t.Fatalf("got %q", got)
	}
}
