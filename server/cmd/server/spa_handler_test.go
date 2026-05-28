package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSPAHandlerReturnsJSONForUnknownAPIPath confirms F9.3: an unmatched
// /api/* route falls through to the SPA fallback but must respond with a
// JSON 404, not the index.html bundle. Without this guard, fetch callers
// see a 200 + HTML and silently fail to parse JSON.
func TestSPAHandlerReturnsJSONForUnknownAPIPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>app</html>"), 0o644); err != nil {
		t.Fatalf("write index.html: %v", err)
	}

	handler := spaHandler(dir)

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"unknown api get", http.MethodGet, "/api/funds/no-such-endpoint"},
		{"unknown api post", http.MethodPost, "/api/ab-tests"},
		{"api root no slash", http.MethodGet, "/api"},
		{"events prefix", http.MethodGet, "/events/something"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, nil)
			handler.ServeHTTP(rr, req)

			if rr.Code != http.StatusNotFound {
				t.Fatalf("status=%d want 404", rr.Code)
			}
			if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Fatalf("content-type=%q want application/json", ct)
			}
			if body := rr.Body.String(); !strings.Contains(body, `"error":"not_found"`) {
				t.Fatalf("body=%q does not contain not_found marker", body)
			}
			if strings.Contains(rr.Body.String(), "<html>") {
				t.Fatalf("body should not contain SPA HTML: %q", rr.Body.String())
			}
		})
	}
}

// TestSPAHandlerServesIndexForRoutes confirms the SPA still works: a non-API
// path with no matching file falls through to index.html as expected by the
// React router.
func TestSPAHandlerServesIndexForRoutes(t *testing.T) {
	dir := t.TempDir()
	indexBody := "<!doctype html><html>spa-index</html>"
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(indexBody), 0o644); err != nil {
		t.Fatalf("write index.html: %v", err)
	}

	handler := spaHandler(dir)

	cases := []string{"/", "/dashboard", "/funds/xyz/settings"}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, path, nil)
			handler.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("status=%d want 200", rr.Code)
			}
			if !strings.Contains(rr.Body.String(), "spa-index") {
				t.Fatalf("body should contain index.html, got %q", rr.Body.String())
			}
		})
	}
}

// TestSPAHandlerServesStaticAssets confirms real assets (JS, CSS) are still
// served verbatim instead of being intercepted by the JSON guard.
func TestSPAHandlerServesStaticAssets(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatalf("mkdir assets: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "app-HASH.js"), []byte("console.log('hi');"), 0o644); err != nil {
		t.Fatalf("write app-HASH.js: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html></html>"), 0o644); err != nil {
		t.Fatalf("write index.html: %v", err)
	}

	handler := spaHandler(dir)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/assets/app-HASH.js", nil)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, "console.log") {
		t.Fatalf("body=%q should serve app-HASH.js verbatim", body)
	}
	if cc := rr.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("hashed asset must be cached immutably; got Cache-Control=%q", cc)
	}
}

// TestSPAHandlerReturnsRealAssetMiss locks in the fix for the
// "Failed to fetch dynamically imported module" bug. When a user's stale
// browser tab references a chunk that no longer exists after a rebuild
// (different hash), the request to /assets/old-HASH.js must produce a real
// 404 — not a 200 + index.html, which the browser would then try to parse
// as a JS module and fail with that exact misleading error message.
func TestSPAHandlerReturnsRealAssetMiss(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>spa</html>"), 0o644); err != nil {
		t.Fatalf("write index.html: %v", err)
	}

	handler := spaHandler(dir)

	cases := []string{
		"/assets/ABTestCompare-OLDHASH.js",
		"/assets/index-OLDHASH.css",
		"/favicon.ico",
		"/vite.svg",
		"/static/something.woff2",
	}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, path, nil)
			handler.ServeHTTP(rr, req)

			if rr.Code != http.StatusNotFound {
				t.Fatalf("status=%d want 404 for missing asset", rr.Code)
			}
			if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Fatalf("content-type=%q want application/json (must NOT be html)", ct)
			}
			if strings.Contains(rr.Body.String(), "<html>") {
				t.Fatalf("body should not contain SPA HTML for missing asset, got %q", rr.Body.String())
			}
		})
	}
}

// TestSPAHandlerIndexHTMLIsNotCached locks in that the entry HTML is served
// with no-cache. If a CDN or the browser cached index.html across a deploy,
// the user would keep loading the old entry chunk names and hit the same
// dynamic-import errors all over again. `no-cache` does NOT mean "don't
// cache" — it means "always revalidate" — which is the right semantics
// here (the body is small and the ETag round-trip is cheap).
func TestSPAHandlerIndexHTMLIsNotCached(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>spa-index</html>"), 0o644); err != nil {
		t.Fatalf("write index.html: %v", err)
	}

	handler := spaHandler(dir)

	for _, path := range []string{"/", "/dashboard"} {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, path, nil)
			handler.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("status=%d want 200", rr.Code)
			}
			cc := rr.Header().Get("Cache-Control")
			if !strings.Contains(cc, "no-cache") {
				t.Fatalf("index.html must use no-cache so users pick up new entry after rebuild; got %q", cc)
			}
		})
	}
}

// TestIsStaticAssetPath: the heuristic that distinguishes "asset request"
// (must 404 when missing) from "SPA route" (falls back to index.html).
// React-router routes are always extension-less, so checking for a "." in
// the last segment is both sufficient and zero-config — we don't have to
// maintain a list of known extensions.
func TestIsStaticAssetPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/", false},
		{"/dashboard", false},
		{"/funds/abc/settings", false},
		{"/funds/abc.def", true},
		{"/assets/foo-HASH.js", true},
		{"/assets/foo-HASH.css", true},
		{"/favicon.ico", true},
		{"/vite.svg", true},
		{"/index.html", true},
		{"", false},
	}
	for _, tc := range cases {
		got := isStaticAssetPath(tc.path)
		if got != tc.want {
			t.Errorf("isStaticAssetPath(%q)=%v want %v", tc.path, got, tc.want)
		}
	}
}

// TestPathAliasMiddlewareRewritesKebabCase locks in F11.3's alias: callers
// using /api/ab-tests (kebab) reach the canonical /api/abtests handler
// without 404s. Avoids the smoke-test debugging trap where typos silently
// went nowhere.
func TestPathAliasMiddlewareRewritesKebabCase(t *testing.T) {
	captured := ""
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	handler := pathAliasMiddleware(inner)

	cases := []struct {
		incoming string
		expected string
	}{
		{"/api/ab-tests", "/api/abtests"},
		{"/api/ab-tests/abc-123", "/api/abtests/abc-123"},
		{"/api/ab-tests/abc-123/start", "/api/abtests/abc-123/start"},
		{"/api/abtests", "/api/abtests"},
		{"/api/abtests/abc-123", "/api/abtests/abc-123"},
		{"/api/funds/fund-1", "/api/funds/fund-1"},
	}

	for _, tc := range cases {
		t.Run(tc.incoming, func(t *testing.T) {
			captured = ""
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.incoming, nil)
			handler.ServeHTTP(rr, req)
			if captured != tc.expected {
				t.Fatalf("rewrote %s -> %s, want %s", tc.incoming, captured, tc.expected)
			}
		})
	}
}

func TestIsAPILikePath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/api/foo", true},
		{"/api/foo/bar", true},
		{"/api", true},
		{"/events", true},
		{"/events/stream", true},
		{"/", false},
		{"/dashboard", false},
		{"/apidocs", false},
		{"/eventsource", false},
	}
	for _, tc := range cases {
		got := isAPILikePath(tc.path)
		if got != tc.want {
			t.Errorf("isAPILikePath(%q)=%v want %v", tc.path, got, tc.want)
		}
	}
}
