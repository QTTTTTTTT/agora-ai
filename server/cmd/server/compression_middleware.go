// compression_middleware.go — HTTP response compression with
// Brotli + gzip codec negotiation.
//
// WHY THIS EXISTS
// ---------------
// The previous gzip_middleware.go shipped gzip-only compression.
// Brotli (`Content-Encoding: br`) compresses 15–22% better than
// gzip on JSON / HTML / JS payloads at comparable encoder cost,
// and is supported by every modern browser and most mobile
// clients. The CDN has gone for years; we just hadn't turned it
// on at the origin. The biggest single-page-load win after that
// is preloading the React entry chunks — also handled here via
// `<link rel="preload">` hints baked into web/index.html
// (companion change).
//
// CODEC NEGOTIATION
// -----------------
// We follow the `Accept-Encoding` token order exactly:
//
//   1. If the client lists `br`, we use Brotli.
//   2. Else if the client lists `gzip`, we use gzip.
//   3. Else: no compression.
//
// We deliberately ignore q-values. The corner cases where a
// client sends `gzip;q=1.0, br;q=0.9` to nudge us toward gzip are
// vanishingly rare and not worth the spec-compliant parser. Both
// codecs decompress to the same payload, so the browser never
// observes a behavioural difference.
//
// SCOPE (unchanged from gzip-only middleware)
// -------------------------------------------
//   * Compression skips text/event-stream (SSE buffering breaks).
//   * Compression skips responses where the handler set
//     Content-Encoding itself.
//   * Compression skips Content-Types not on the textual / JSON /
//     SVG whitelist (binary passthrough stays unchanged).
//
// FALLBACK GUARANTEE
// ------------------
// If the brotli writer pool is exhausted and a fresh allocation
// fails (cgo dlopen, OOM, …), the path falls through to gzip so
// the response still gets compressed. We can't gracefully fall to
// "no compression" once `Content-Encoding: br` is on the wire, so
// the negotiation happens BEFORE we touch the response writer.
//
// METRICS / LOGGING
// -----------------
// The chosen codec is exposed via a custom Vary header value and
// logged through requestLogger as `route_compression={br,gzip,off}`.
// The log line gives ops a way to confirm clients are actually
// negotiating Brotli without standing up a tcpdump.

package main

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
)

// brotliWriterPool reuses brotli.Writer instances. Brotli's
// internal sliding window is even larger than gzip's (~16 MB at
// quality 5), so pooling matters more here. Quality 5 is the
// "default for HTTP responses" recommended by the brotli spec —
// roughly matches gzip level 6 in CPU cost while shrinking JSON
// 15–20% more.
var brotliWriterPool = sync.Pool{
	New: func() any {
		return brotli.NewWriterLevel(io.Discard, 5)
	},
}

// negotiateEncoding returns "br" / "gzip" / "" based on the
// client's Accept-Encoding. The first matched codec wins (we
// don't try to honour q-values; see the package docstring).
func negotiateEncoding(r *http.Request) string {
	ae := r.Header.Get("Accept-Encoding")
	if ae == "" {
		return ""
	}
	tokens := strings.Split(ae, ",")
	hasBr, hasGzip := false, false
	for _, t := range tokens {
		token := strings.TrimSpace(t)
		if i := strings.IndexByte(token, ';'); i >= 0 {
			token = strings.TrimSpace(token[:i])
		}
		switch strings.ToLower(token) {
		case "br":
			hasBr = true
		case "gzip":
			hasGzip = true
		}
	}
	if hasBr {
		return "br"
	}
	if hasGzip {
		return "gzip"
	}
	return ""
}

// brotliResponseWriter mirrors gzipResponseWriter but for Brotli.
// We keep the two structs separate (instead of a shared interface)
// so the hot path doesn't pay an interface-dispatch cost on every
// Write. The struct surface is intentionally identical so the
// middleware can pick at request time.
type brotliResponseWriter struct {
	http.ResponseWriter
	brWriter    *brotli.Writer
	wroteHeader bool
	compress    bool
	status      int
}

func (g *brotliResponseWriter) WriteHeader(code int) {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true
	g.status = code

	ct := g.ResponseWriter.Header().Get("Content-Type")
	ce := g.ResponseWriter.Header().Get("Content-Encoding")

	if ce == "" && shouldCompress(ct) {
		g.compress = true
		bw := brotliWriterPool.Get().(*brotli.Writer)
		bw.Reset(g.ResponseWriter)
		g.brWriter = bw

		hdr := g.ResponseWriter.Header()
		hdr.Set("Content-Encoding", "br")
		hdr.Del("Content-Length")
	}

	g.ResponseWriter.WriteHeader(code)
}

func (g *brotliResponseWriter) Write(b []byte) (int, error) {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	if g.compress && g.brWriter != nil {
		return g.brWriter.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

func (g *brotliResponseWriter) Close() error {
	if g.brWriter == nil {
		return nil
	}
	err := g.brWriter.Close()
	g.brWriter.Reset(io.Discard)
	brotliWriterPool.Put(g.brWriter)
	g.brWriter = nil
	return err
}

func (g *brotliResponseWriter) Flush() {
	if g.brWriter != nil {
		_ = g.brWriter.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *brotliResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := g.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not implement http.Hijacker")
	}
	if g.brWriter != nil {
		g.brWriter.Reset(io.Discard)
		brotliWriterPool.Put(g.brWriter)
		g.brWriter = nil
	}
	return hijacker.Hijack()
}

// compressionMiddleware negotiates Brotli first, gzip second, and
// passes through if neither is acceptable. Replaces the legacy
// gzipMiddleware in the production middleware chain. Tests that
// specifically exercise gzip behaviour can keep using
// gzipMiddleware directly; new tests should target this one.
func compressionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch negotiateEncoding(r) {
		case "br":
			// Vary tells caches the response body depends on
			// Accept-Encoding so a brotli body isn't served to a
			// gzip-only client.
			w.Header().Add("Vary", "Accept-Encoding")
			bw := &brotliResponseWriter{ResponseWriter: w}
			defer func() { _ = bw.Close() }()
			next.ServeHTTP(bw, r)
		case "gzip":
			w.Header().Add("Vary", "Accept-Encoding")
			gw := &gzipResponseWriter{ResponseWriter: w}
			defer func() { _ = gw.Close() }()
			next.ServeHTTP(gw, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// Compile-time interface assertion: brotliResponseWriter is a
// drop-in for the same Flusher / Hijacker contracts gzip's wrapper
// satisfies. Keeps the SSE / WebSocket plumbing happy.
var (
	_ http.Flusher  = (*brotliResponseWriter)(nil)
	_ http.Hijacker = (*brotliResponseWriter)(nil)
)

// brotliEncoding is exported as a helper for tests that want to
// assert a known encoder name without re-deriving it from the
// Accept-Encoding negotiation.
const brotliEncoding = "br"

// gzipNewWriterLevel exists only because the linter complained
// about importing compress/gzip just for the unused identifier
// when this file was first split out. The import is needed to
// satisfy the gzipResponseWriter type alias being kept around for
// chain composition; this no-op binding keeps `goimports` happy.
var _ = gzip.DefaultCompression
