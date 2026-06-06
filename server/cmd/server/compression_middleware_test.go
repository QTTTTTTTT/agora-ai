package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
)

// TestCompressionMiddlewareNegotiatesBrotli asserts that a client
// advertising Brotli gets a Content-Encoding: br response and that
// decompressing the body yields the original bytes.
func TestCompressionMiddlewareNegotiatesBrotli(t *testing.T) {
	payload := []byte(strings.Repeat(`{"hello":"world"}`+"\n", 64))
	handler := compressionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Accept-Encoding", "br, gzip")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if got := rr.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("expected Content-Encoding=br, got %q", got)
	}
	if got := rr.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Errorf("expected Vary to include Accept-Encoding, got %q", got)
	}

	dec := brotli.NewReader(rr.Body)
	decoded, err := io.ReadAll(dec)
	if err != nil {
		t.Fatalf("brotli decompress: %v", err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Errorf("decompressed mismatch: got %d bytes, want %d", len(decoded), len(payload))
	}
	if rr.Body.Len() >= len(payload) {
		t.Errorf("expected compressed payload < %d bytes, got %d (no compression?)", len(payload), rr.Body.Len())
	}
}

// TestCompressionMiddlewarePrefersBrotliOverGzip asserts the codec
// negotiation: with both br + gzip on offer, Brotli wins.
func TestCompressionMiddlewarePrefersBrotliOverGzip(t *testing.T) {
	handler := compressionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"k":"v"}`))
	}))

	cases := []struct {
		name       string
		acceptEnc  string
		wantEnc    string
		wantBodySz int // 0 means "don't check"
	}{
		{name: "br only", acceptEnc: "br", wantEnc: "br"},
		{name: "br + gzip prefers br", acceptEnc: "br, gzip", wantEnc: "br"},
		{name: "gzip + br order doesn't matter", acceptEnc: "gzip, br", wantEnc: "br"},
		{name: "gzip only falls back to gzip", acceptEnc: "gzip", wantEnc: "gzip"},
		{name: "deflate only not supported", acceptEnc: "deflate", wantEnc: ""},
		{name: "empty accept-encoding pass through", acceptEnc: "", wantEnc: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
			if tc.acceptEnc != "" {
				req.Header.Set("Accept-Encoding", tc.acceptEnc)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			got := rr.Header().Get("Content-Encoding")
			if got != tc.wantEnc {
				t.Fatalf("Content-Encoding=%q, want %q", got, tc.wantEnc)
			}
		})
	}
}

// TestCompressionMiddlewareSkipsSSE asserts that text/event-stream
// responses still go out uncompressed even when the client offers
// Brotli — gzip / brotli buffering breaks the EventSource contract.
func TestCompressionMiddlewareSkipsSSE(t *testing.T) {
	handler := compressionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: ping\ndata: hi\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/stream", nil)
	req.Header.Set("Accept-Encoding", "br, gzip")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if got := rr.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("expected no Content-Encoding for SSE, got %q", got)
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte("event: ping")) {
		t.Errorf("expected raw SSE body in response, got %q", rr.Body.String())
	}
}

// TestNegotiateEncodingTokenForms exercises the parser against a
// few real-world Accept-Encoding shapes (q-values are stripped, case
// is folded, comma+space separators handled).
func TestNegotiateEncodingTokenForms(t *testing.T) {
	cases := map[string]string{
		"":                                "",
		"gzip":                            "gzip",
		"br":                              "br",
		"BR, GZIP":                        "br",
		"identity":                        "",
		"gzip;q=1.0, br;q=0.8":            "br", // we do not honour q-values; first match by code wins (br checked before gzip).
		"deflate, gzip":                   "gzip",
		"  br  ,  gzip  ":                 "br",
		"gzip;q=0":                        "gzip", // simplistic parser (we don't honour q=0); acceptable false-positive.
		"br;q=1.0, gzip;q=0.9, deflate":   "br",
	}
	for in, want := range cases {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		if in != "" {
			req.Header.Set("Accept-Encoding", in)
		}
		got := negotiateEncoding(req)
		if got != want {
			t.Errorf("negotiateEncoding(%q) = %q, want %q", in, got, want)
		}
	}
}
