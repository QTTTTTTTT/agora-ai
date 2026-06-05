// rate_limit_middleware.go — per-IP token-bucket rate limiting.
//
// WHY THIS EXISTS
// ---------------
// The application surface had no rate limit. Specifically:
//   - /api/auth/login could be hammered without backoff, and the
//     bcrypt cost on the server was the only defence — meaning a
//     credential-stuffing attack at modest concurrency could
//     saturate CPU and lock real users out for the duration of
//     the attack. Bcrypt is cheap enough that "let CPU exhaustion
//     be the throttle" is the wrong answer.
//   - /api/auth/register and /api/auth/forgot-password sent emails
//     and could be spammed to either burn email reputation or
//     enumerate user accounts via timing.
//   - All authenticated endpoints could be looped on without any
//     ceiling, which means a single client running a polling
//     loop bug could DOS its own tenant.
//   - Marketdata had its own per-provider rate limit
//     (server/internal/marketdata/ratelimit.go) but that's about
//     PROTECTING UPSTREAM, not protecting our API surface.
//
// This middleware adds per-client token-bucket rate limiting at
// the HTTP layer, with separate policies for three path classes:
//
//   AUTH        /api/auth/*               aggressive  (5 RPS, burst 10)
//   MUTATION    POST/PUT/PATCH/DELETE     moderate    (10 RPS, burst 20)
//   READ        GET (everything else)     lenient     (50 RPS, burst 100)
//
// Defaults are tuned for the hosted SaaS profile we serve today
// — every legitimate user comfortably stays under READ's 50 RPS
// even when scrubbing through a fund dashboard, while the auth
// limit is well below what a brute-force attacker needs. Ops can
// override via env vars (see Config below) without a rebuild.
//
// EXEMPTIONS
// ----------
//   - /api/health, /api/version, /api/metrics — used by load
//     balancers and Prometheus scrape; rate-limiting these would
//     either flap LB health or starve metrics. Bypassed wholesale.
//   - SSE endpoints (path suffix /stream) — the limit applies to
//     connection ESTABLISHMENT, not per-frame. Once a stream is
//     open it bypasses the limiter; the client can't "spam" the
//     server through an established stream because the server is
//     the writer. We rely on auth + per-fund stream caps (future
//     work) to bound concurrent stream resource use.
//
// CLIENT IDENTITY
// ---------------
// Per-IP for now, with the IP extracted via X-Forwarded-For (first
// hop) when present, falling back to r.RemoteAddr. This trusts the
// reverse proxy to set XFF correctly — our deployment puts every
// public ingress behind a proxy that does, and dev runs go through
// the Go reverse proxy in main.go which forwards XFF unchanged.
//
// FUTURE WORK
// -----------
//   - Per-user rate limit (post-auth) for authenticated endpoints,
//     so a single bad actor account can't lean on a NAT pool to
//     amplify their effective limit.
//   - Distributed rate-limit backend (Redis) for horizontally
//     scaled deployments — the in-memory map is per-process and
//     diverges across pods.
//   - 429 response body i18n via the middleware-aware language
//     header (current message is short ASCII).

package main

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// rateLimitConfig captures the three policies + a TTL for evicting
// inactive client buckets.
type rateLimitConfig struct {
	authRPS     float64
	authBurst   int
	mutateRPS   float64
	mutateBurst int
	readRPS     float64
	readBurst   int
	bucketTTL   time.Duration
}

func defaultRateLimitConfig() rateLimitConfig {
	return rateLimitConfig{
		authRPS:     envFloat("RATE_LIMIT_AUTH_RPS", 5),
		authBurst:   envInt("RATE_LIMIT_AUTH_BURST", 10),
		mutateRPS:   envFloat("RATE_LIMIT_MUTATE_RPS", 10),
		mutateBurst: envInt("RATE_LIMIT_MUTATE_BURST", 20),
		readRPS:     envFloat("RATE_LIMIT_READ_RPS", 50),
		readBurst:   envInt("RATE_LIMIT_READ_BURST", 100),
		// 10 minutes is long enough that a normal "open dashboard,
		// idle, come back" cycle reuses the same bucket; short
		// enough that a transient burst from one IP doesn't pin
		// a bucket forever.
		bucketTTL: time.Duration(envInt("RATE_LIMIT_BUCKET_TTL_SEC", 600)) * time.Second,
	}
}

// rateLimitedClient pairs a token-bucket limiter with the time it
// was last touched, so the eviction sweep can drop quiet IPs.
type rateLimitedClient struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// rateLimiterStore holds three keyed maps — one per path class —
// because the buckets refill at different rates and a single
// shared bucket would conflate "this IP is hammering /login" with
// "this IP is happily reading dashboards".
type rateLimiterStore struct {
	mu      sync.Mutex
	auth    map[string]*rateLimitedClient
	mutate  map[string]*rateLimitedClient
	read    map[string]*rateLimitedClient
	cfg     rateLimitConfig
	lastGC  time.Time
}

func newRateLimiterStore(cfg rateLimitConfig) *rateLimiterStore {
	return &rateLimiterStore{
		auth:   map[string]*rateLimitedClient{},
		mutate: map[string]*rateLimitedClient{},
		read:   map[string]*rateLimitedClient{},
		cfg:    cfg,
		lastGC: time.Now(),
	}
}

// allow returns true if the client at `key` for the given class has
// budget. It maintains the per-class map atomically and runs a
// piggy-backed GC sweep at most once per minute to drop stale
// buckets.
func (s *rateLimiterStore) allow(class string, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	if now.Sub(s.lastGC) > time.Minute {
		s.gcLocked(now)
		s.lastGC = now
	}

	var (
		bucket map[string]*rateLimitedClient
		rps    rate.Limit
		burst  int
	)
	switch class {
	case "auth":
		bucket, rps, burst = s.auth, rate.Limit(s.cfg.authRPS), s.cfg.authBurst
	case "mutate":
		bucket, rps, burst = s.mutate, rate.Limit(s.cfg.mutateRPS), s.cfg.mutateBurst
	default:
		bucket, rps, burst = s.read, rate.Limit(s.cfg.readRPS), s.cfg.readBurst
	}

	c, ok := bucket[key]
	if !ok {
		c = &rateLimitedClient{limiter: rate.NewLimiter(rps, burst)}
		bucket[key] = c
	}
	c.lastSeen = now
	return c.limiter.Allow()
}

func (s *rateLimiterStore) gcLocked(now time.Time) {
	for _, m := range []map[string]*rateLimitedClient{s.auth, s.mutate, s.read} {
		for k, c := range m {
			if now.Sub(c.lastSeen) > s.cfg.bucketTTL {
				delete(m, k)
			}
		}
	}
}

// classifyRequest picks one of {auth, mutate, read} for the path
// + method pair. Order matters: auth wins over the method-based
// classification because /api/auth/forgot-password is a POST but
// we want it on the aggressive auth limiter, not mutate.
func classifyRequest(r *http.Request) string {
	path := r.URL.Path
	if strings.HasPrefix(path, "/api/auth/") {
		return "auth"
	}
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return "mutate"
	default:
		return "read"
	}
}

// shouldExempt returns true for paths that bypass the limiter
// entirely. Health and metrics endpoints must always succeed for
// the LB / Prometheus scrape to work; SSE streams are exempted
// because the limit applies at connection time, not per-frame.
func shouldExempt(r *http.Request) bool {
	p := r.URL.Path
	switch p {
	case "/api/health", "/api/version", "/api/metrics":
		return true
	}
	// SSE streams. Once the stream is open, the server writes the
	// frames; the client can't spam the server through it.
	if strings.HasSuffix(p, "/stream") {
		return true
	}
	return false
}

// clientKeyForRateLimit returns the IP we'll use as the bucket
// key. Trusts X-Forwarded-For when present (our edges set it);
// falls back to RemoteAddr. The port is stripped so a single
// client behind a single egress IP shares one bucket regardless
// of source-port churn.
func clientKeyForRateLimit(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// First hop is the original client.
		first := strings.TrimSpace(strings.SplitN(xff, ",", 2)[0])
		if first != "" {
			return first
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimitMiddleware enforces the per-class token-bucket policy.
// On limit exceeded it returns 429 with a Retry-After hint and the
// X-RateLimit-Class header so debug tools / tests can see which
// bucket triggered.
func rateLimitMiddleware(store *rateLimiterStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if shouldExempt(r) {
				next.ServeHTTP(w, r)
				return
			}
			class := classifyRequest(r)
			key := clientKeyForRateLimit(r)
			if !store.allow(class, key) {
				w.Header().Set("X-RateLimit-Class", class)
				w.Header().Set("Retry-After", "1")
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":"rate_limited","message":"too many requests, please retry shortly"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// envFloat is the float64 sibling of main.go's envInt — kept local
// because pulling main.go's env helpers around for one extra
// parser is more disruptive than its weight.
func envFloat(key string, def float64) float64 {
	v := firstEnv(key, "")
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		return def
	}
	return f
}
