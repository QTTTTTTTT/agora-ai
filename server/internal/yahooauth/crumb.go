// Package yahooauth provides a shared session-cookie + crumb cache
// for Yahoo Finance API callers (fundamental.YahooProvider,
// earnings.YahooProvider / YahooHistoryProvider). Sprint 1 / S5.
//
// Yahoo's keyless `query2.finance.yahoo.com/v10/finance/quoteSummary/...`
// and `v10/finance/calendar/...` endpoints started returning HTTP 401
// without a crumb param in 2024-Q4. The crumb is itself derived from a
// short-lived `A1` session cookie obtained by a single GET to
// `fc.yahoo.com`. Once both are cached, callers append `&crumb=...` and
// send the cookie back on the request — which restores 200 responses.
//
// The cache here is process-wide and refreshes on demand:
//   - First call seeds the cookie jar + crumb under a 10-minute TTL.
//   - Subsequent callers hit the cached values until expiry.
//   - A 401 response in the calling provider invalidates the cache via
//     Invalidate(); the next call re-seeds.
//
// The session is intentionally shared across providers so a single
// 401-recovery roundtrip benefits every Yahoo-backed component instead
// of each provider racing its own cookie/crumb pair.
package yahooauth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"time"
)

const (
	// defaultUserAgent matches the desktop Chrome UA we already use
	// in marketdata.YahooProvider. Yahoo 403s Go's default UA.
	defaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
	// cookieSeedURL is Yahoo's session-cookie origin. A simple GET
	// here sets the A1/A3 cookies the crumb endpoint requires.
	cookieSeedURL = "https://fc.yahoo.com/"
	// crumbURL returns a short opaque string we then echo back as
	// `&crumb=...` on every quoteSummary request.
	crumbURL = "https://query2.finance.yahoo.com/v1/test/getcrumb"
	// cacheTTL bounds how long a successful (cookie, crumb) pair
	// stays cached before we re-seed. Yahoo crumbs typically live
	// 1-2h but we err short so a stale crumb's blast radius is small.
	cacheTTL = 10 * time.Minute
	// httpTimeout for cookie/crumb roundtrips. Both endpoints are
	// fast; if either hangs we'd rather fall through and let the
	// caller retry.
	httpTimeout = 8 * time.Second
)

// Session bundles a process-wide cookie jar + the most recently
// fetched crumb value. Zero value is ready to use; the first call to
// Get triggers a lazy seed.
type Session struct {
	mu        sync.Mutex
	jar       http.CookieJar
	crumb     string
	fetchedAt time.Time
	// inflight de-duplicates concurrent first-time callers. Without
	// it, N goroutines racing through Get on a cold cache would each
	// fire their own cookie/crumb roundtrip.
	inflight chan struct{}
}

// Default is the process-wide instance every Yahoo provider shares.
// Callers should reuse it rather than allocating their own Session
// so a single 401-recovery roundtrip benefits the whole binary.
var Default = &Session{}

// Get returns the cached crumb + the cookie jar the caller must
// attach to its http.Client. A fresh seed happens on first call, on
// TTL expiry, or after an Invalidate. Returns ("", nil, err) on
// network failure — the caller should treat that as "no crumb
// available, fall back to legacy unauthenticated request".
func (s *Session) Get(ctx context.Context) (string, http.CookieJar, error) {
	s.mu.Lock()
	if s.crumb != "" && !s.fetchedAt.IsZero() && time.Since(s.fetchedAt) < cacheTTL && s.jar != nil {
		crumb, jar := s.crumb, s.jar
		s.mu.Unlock()
		return crumb, jar, nil
	}
	if s.inflight == nil {
		s.inflight = make(chan struct{})
		s.mu.Unlock()
		crumb, jar, err := s.seed(ctx)
		s.mu.Lock()
		if err == nil {
			s.crumb = crumb
			s.jar = jar
			s.fetchedAt = time.Now()
		}
		close(s.inflight)
		s.inflight = nil
		s.mu.Unlock()
		return crumb, jar, err
	}
	wait := s.inflight
	s.mu.Unlock()
	select {
	case <-wait:
	case <-ctx.Done():
		return "", nil, ctx.Err()
	}
	s.mu.Lock()
	crumb, jar := s.crumb, s.jar
	s.mu.Unlock()
	if crumb == "" || jar == nil {
		return "", nil, fmt.Errorf("yahooauth: seed failed (concurrent caller)")
	}
	return crumb, jar, nil
}

// Invalidate forces the next Get to re-seed. Call this when Yahoo
// returns 401/403 — the cached crumb is stale.
func (s *Session) Invalidate() {
	s.mu.Lock()
	s.crumb = ""
	s.jar = nil
	s.fetchedAt = time.Time{}
	s.mu.Unlock()
}

// seed performs the two-step handshake: GET fc.yahoo.com to seed
// cookies, then GET query2.finance.yahoo.com/v1/test/getcrumb with
// those cookies attached.
func (s *Session) seed(ctx context.Context) (string, http.CookieJar, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return "", nil, fmt.Errorf("yahooauth: cookiejar: %w", err)
	}
	client := &http.Client{Jar: jar, Timeout: httpTimeout}
	// Step 1: seed the A1/A3 session cookies.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cookieSeedURL, nil)
	if err != nil {
		return "", nil, fmt.Errorf("yahooauth: cookie req: %w", err)
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("yahooauth: cookie http: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	// Yahoo returns 200 OR 404 here; both leave the cookies set.
	// We don't strictly require a specific status — the cookies are
	// what matter.
	// Step 2: fetch crumb.
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, crumbURL, nil)
	if err != nil {
		return "", nil, fmt.Errorf("yahooauth: crumb req: %w", err)
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Accept", "text/plain")
	resp, err = client.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("yahooauth: crumb http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", nil, fmt.Errorf("yahooauth: crumb status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return "", nil, fmt.Errorf("yahooauth: crumb read: %w", err)
	}
	crumb := strings.TrimSpace(string(body))
	if crumb == "" {
		return "", nil, fmt.Errorf("yahooauth: empty crumb")
	}
	return crumb, jar, nil
}

// AttachToRequest sets the User-Agent + accept headers on the
// request and returns the cookie jar the caller should plug into
// their http.Client. The caller is responsible for appending
// `&crumb=<crumb>` to the URL.
func AttachToRequest(req *http.Request) {
	if req == nil {
		return
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", defaultUserAgent)
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
}
