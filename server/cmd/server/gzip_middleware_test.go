package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGzipMiddleware_CompressesJSONWhenClientAcceptsGzip(t *testing.T) {
	body := strings.Repeat(`{"key":"value"}`, 200) // ~3KB, big enough to compress meaningfully
	handler := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("expected Content-Encoding=gzip, got %q", rec.Header().Get("Content-Encoding"))
	}
	if !strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding") {
		t.Fatalf("expected Vary to include Accept-Encoding, got %q", rec.Header().Get("Vary"))
	}
	gz, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	got, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	if string(got) != body {
		t.Fatalf("decompressed body mismatch: got %d bytes want %d bytes", len(got), len(body))
	}
	if rec.Body.Len() >= len(body) {
		t.Fatalf("expected compressed body smaller than original, got %d >= %d", rec.Body.Len(), len(body))
	}
}

func TestGzipMiddleware_SkipsWhenClientDoesNotAcceptGzip(t *testing.T) {
	body := strings.Repeat(`{"key":"value"}`, 50)
	handler := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	// No Accept-Encoding header.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "" {
		t.Fatalf("expected no Content-Encoding when client doesn't accept gzip, got %q", rec.Header().Get("Content-Encoding"))
	}
	if rec.Body.String() != body {
		t.Fatalf("body mismatch: handler should pass through unchanged")
	}
}

func TestGzipMiddleware_SkipsSSEContentType(t *testing.T) {
	frames := "event: ping\ndata: 1\n\nevent: ping\ndata: 2\n\n"
	handler := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(frames))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/x/stream", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") == "gzip" {
		t.Fatalf("text/event-stream MUST NOT be gzipped (breaks Flush semantics)")
	}
	if rec.Body.String() != frames {
		t.Fatalf("SSE body mismatch — passthrough broken")
	}
}

func TestGzipMiddleware_SkipsBinaryContentType(t *testing.T) {
	binary := bytes.Repeat([]byte{0x00, 0x01, 0x02, 0x03}, 200)
	handler := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(binary)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/x.bin", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") == "gzip" {
		t.Fatalf("octet-stream should not be re-gzipped")
	}
	if !bytes.Equal(rec.Body.Bytes(), binary) {
		t.Fatalf("binary body mutated by middleware")
	}
}

func TestGzipMiddleware_SkipsAlreadyEncodedResponse(t *testing.T) {
	preEncoded := []byte("pretend this is already gzipped")
	handler := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "br") // handler already chose brotli
		_, _ = w.Write(preEncoded)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	req.Header.Set("Accept-Encoding", "gzip, br")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "br" {
		t.Fatalf("middleware overrode existing Content-Encoding (got %q)", rec.Header().Get("Content-Encoding"))
	}
	if !bytes.Equal(rec.Body.Bytes(), preEncoded) {
		t.Fatalf("middleware re-encoded an already-encoded body")
	}
}

func TestShouldCompress(t *testing.T) {
	cases := []struct {
		ct   string
		want bool
	}{
		{"application/json", true},
		{"application/json; charset=utf-8", true},
		{"text/html", true},
		{"text/plain; charset=utf-8", true},
		{"image/svg+xml", true},
		{"application/javascript", true},
		{"text/event-stream", false},
		{"text/event-stream; charset=utf-8", false},
		{"application/octet-stream", false},
		{"image/png", false},
		{"image/jpeg", false},
		{"video/mp4", false},
		{"", false},
	}
	for _, tc := range cases {
		got := shouldCompress(tc.ct)
		if got != tc.want {
			t.Errorf("shouldCompress(%q) = %v, want %v", tc.ct, got, tc.want)
		}
	}
}

func TestGzipMiddleware_FlushPassesThroughForSSE(t *testing.T) {
	flushed := false
	handler := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatalf("Flusher assertion failed — gzip middleware broke SSE")
		}
		_, _ = w.Write([]byte("event: ping\ndata: 1\n\n"))
		f.Flush()
		flushed = true
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/x/stream", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !flushed {
		t.Fatalf("handler did not reach the flush call")
	}
}

func TestClientAcceptsGzip(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{"gzip", true},
		{"gzip, deflate", true},
		{"deflate, gzip", true},
		{"gzip;q=1.0, deflate;q=0.5", true},
		{"deflate", false},
		{"br", false},
		{"", false},
		{"identity", false},
	}
	for _, tc := range cases {
		req, _ := http.NewRequest(http.MethodGet, "/", nil)
		if tc.header != "" {
			req.Header.Set("Accept-Encoding", tc.header)
		}
		got := clientAcceptsGzip(req)
		if got != tc.want {
			t.Errorf("clientAcceptsGzip(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}
