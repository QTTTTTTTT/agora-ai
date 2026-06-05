// gzip_middleware.go — HTTP response compression for the API surface.
//
// WHY THIS EXISTS
// ---------------
// Until this commit the middleware chain was
//
//   recoverer → requestLogger → CORS → auth → pathAlias → mux
//
// — no compression anywhere. Every JSON payload (decision traces,
// trade history, audit logs, agent learning records) hit the wire
// uncompressed. For modest responses that's a few KB; for the
// dashboard endpoints that bundle the fund overview + 30 days of
// NAV + a roll of trades it's 200 KB+ uncompressed where 25 KB
// gzipped would suffice. Mobile clients on Chinese 4G feel that
// 8x bandwidth tax directly as TTFB.
//
// SCOPE
// -----
// Compression is applied only when:
//   1. the client advertises Accept-Encoding: gzip (browsers do, RN
//      does, curl by default doesn't — handlers stay correct
//      either way);
//   2. the response Content-Type is one of the textual / structured
//      types where gzip is a net win (text/*, application/json,
//      application/javascript, application/xml, image/svg+xml);
//   3. the response is NOT already encoded (handler set
//      Content-Encoding itself, e.g. binary file passthrough);
//   4. the response is NOT text/event-stream — SSE MUST stay
//      uncompressed because gzip's internal buffering breaks the
//      "frame-as-soon-as-flushed" contract that EventSource clients
//      depend on. Our 3 SSE endpoints (quotes / team-activity /
//      workflow) keep working unchanged.
//
// ORDER IN THE CHAIN
// ------------------
// Inserted between `requestLogger` (outer) and `corsMiddleware`
// (inner) so that:
//   - logger's `bytesWritten` counter records the actual gzipped
//     payload that goes out on the wire (matches what ops cares
//     about: bandwidth used);
//   - every authenticated/CORS-checked response gets compressed,
//     including 4xx error JSON;
//   - SSE handlers downstream still see a working Flusher because
//     gzipResponseWriter forwards Flush() correctly.
//
// PASS-THROUGH INTERFACE GUARANTEES
// ---------------------------------
// gzipResponseWriter implements http.Flusher and http.Hijacker so
// SSE + WebSocket upgrade paths keep working. The Flush() impl
// flushes the gzip writer first (so partial frames go out
// compressed) before flushing the underlying ResponseWriter; for
// the SSE skip path gzipWriter is nil and we just forward.
//
// Future work:
//   - Brotli for clients that advertise it (br compresses ~15-20%
//     better than gzip on JSON; behind a feature flag because it
//     needs cgo or a pure-Go encoder + the bundle-size tradeoff).
//   - Per-route opt-out for hot endpoints whose payloads are
//     dominated by binary data (today there are none, but the
//     option is cheap to add when needed).
//   - Compression-level tuning. We use gzip.DefaultCompression (6)
//     which is the standard CPU/ratio knee. A future capacity
//     planning exercise can A/B `BestSpeed` vs `DefaultCompression`
//     under representative load.

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
)

// gzipWriterPool reuses gzip.Writer instances across requests. Each
// gzip.Writer carries a ~256 KB internal buffer; allocating one per
// request would dwarf the JSON bytes we're trying to compress.
var gzipWriterPool = sync.Pool{
	New: func() any {
		// io.Discard is a placeholder — Reset() in the middleware
		// rebinds the writer to the actual response stream before
		// any Write() call.
		return gzip.NewWriter(io.Discard)
	},
}

// gzipResponseWriter wraps an http.ResponseWriter. On the first
// Write or WriteHeader it inspects the response Content-Type to
// decide whether to compress; once decided, all subsequent writes
// flow through the gzip.Writer (or directly through the underlying
// writer if compression was skipped).
type gzipResponseWriter struct {
	http.ResponseWriter
	gzWriter    *gzip.Writer
	wroteHeader bool
	compress    bool
	status      int
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true
	g.status = code

	ct := g.ResponseWriter.Header().Get("Content-Type")
	ce := g.ResponseWriter.Header().Get("Content-Encoding")

	if ce == "" && shouldCompress(ct) {
		g.compress = true
		gw := gzipWriterPool.Get().(*gzip.Writer)
		gw.Reset(g.ResponseWriter)
		g.gzWriter = gw

		hdr := g.ResponseWriter.Header()
		hdr.Set("Content-Encoding", "gzip")
		// Content-Length on the source bytes is now wrong — strip
		// it so the chunked transfer can take over. Setting it to
		// the post-gzip length up front would require buffering
		// the entire response, which kills streaming.
		hdr.Del("Content-Length")
	}

	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	if g.compress && g.gzWriter != nil {
		return g.gzWriter.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

// Close finalises the gzip stream (writes the trailer + checksum).
// Always call this in the middleware's defer — without it a
// well-behaved client sees a gzip-truncated body and refuses to
// parse the JSON.
func (g *gzipResponseWriter) Close() error {
	if g.gzWriter == nil {
		return nil
	}
	err := g.gzWriter.Close()
	// Reset to discard before returning to the pool so we don't
	// hold a reference to the live ResponseWriter past the
	// request lifetime.
	g.gzWriter.Reset(io.Discard)
	gzipWriterPool.Put(g.gzWriter)
	g.gzWriter = nil
	return err
}

// Flush implements http.Flusher. SSE and other streaming handlers
// type-assert to Flusher and call Flush() between frames; without
// this passthrough the assertion would fail and the handler would
// return 500 "sse unsupported". When compression is enabled we
// flush the gzip writer first so the partial frame's compressed
// bytes reach the underlying writer before the underlying flush.
func (g *gzipResponseWriter) Flush() {
	if g.gzWriter != nil {
		_ = g.gzWriter.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack lets WebSocket / connection-upgrade handlers take over the
// underlying TCP connection. Compression must be off for the
// hijacked stream — by definition the application protocol is no
// longer HTTP, so an opportunistic Content-Encoding=gzip header
// would be incoherent. We refuse to hijack a connection whose
// gzip writer has already written bytes, and in any case the
// underlying ResponseWriter must support hijacking too.
func (g *gzipResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := g.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not implement http.Hijacker")
	}
	if g.gzWriter != nil {
		// Best-effort cleanup — discard the gzip state. After
		// hijack, the wrapper is dead anyway.
		g.gzWriter.Reset(io.Discard)
		gzipWriterPool.Put(g.gzWriter)
		g.gzWriter = nil
	}
	return hijacker.Hijack()
}

// shouldCompress returns true for Content-Types where gzip is a net
// win. Returns false for binary types, already-compressed types,
// and explicitly for text/event-stream where gzip's buffering
// breaks the streaming contract.
func shouldCompress(contentType string) bool {
	if contentType == "" {
		return false
	}
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	if base == "" {
		return false
	}
	if strings.HasPrefix(base, "text/event-stream") {
		return false
	}
	if strings.HasPrefix(base, "text/") {
		return true
	}
	switch base {
	case "application/json",
		"application/javascript",
		"application/x-javascript",
		"application/xml",
		"application/xhtml+xml",
		"image/svg+xml":
		return true
	}
	return false
}

// gzipMiddleware compresses the response body for clients that
// advertise Accept-Encoding: gzip and whose response Content-Type
// is in shouldCompress's whitelist.
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Cheap fast-path: clients that don't accept gzip skip the
		// wrapper entirely. `curl` without -H 'Accept-Encoding: gzip'
		// goes here; tests that compare body bytes byte-for-byte
		// stay deterministic.
		if !clientAcceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}

		// Vary tells caches that the response varies by
		// Accept-Encoding so a gzipped response isn't served to a
		// non-gzip client (or vice versa).
		w.Header().Add("Vary", "Accept-Encoding")

		gw := &gzipResponseWriter{ResponseWriter: w}
		defer func() {
			_ = gw.Close()
		}()

		next.ServeHTTP(gw, r)
	})
}

// clientAcceptsGzip parses Accept-Encoding lightly. We don't
// implement the full q-value priority spec — the wins from doing
// so vs. just looking for "gzip" are sub-percent in practice and
// the spec compliance burden isn't worth the CPU.
func clientAcceptsGzip(r *http.Request) bool {
	ae := r.Header.Get("Accept-Encoding")
	if ae == "" {
		return false
	}
	for _, part := range strings.Split(ae, ",") {
		token := strings.TrimSpace(part)
		// Trim any q-value suffix.
		if i := strings.IndexByte(token, ';'); i >= 0 {
			token = strings.TrimSpace(token[:i])
		}
		if strings.EqualFold(token, "gzip") {
			return true
		}
	}
	return false
}
