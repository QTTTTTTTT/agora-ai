package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestStore() *rateLimiterStore {
	return newRateLimiterStore(rateLimitConfig{
		// Tight numbers for deterministic tests.
		authRPS:     2,
		authBurst:   2,
		mutateRPS:   3,
		mutateBurst: 3,
		readRPS:     5,
		readBurst:   5,
		bucketTTL:   time.Minute,
	})
}

func TestRateLimit_AuthBucketBlocksAfterBurst(t *testing.T) {
	store := newTestStore()
	mw := rateLimitMiddleware(store)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
		req.RemoteAddr = "1.2.3.4:5000"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("burst attempt %d: expected 200, got %d", i+1, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	req.RemoteAddr = "1.2.3.4:5000"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("post-burst: expected 429, got %d", rec.Code)
	}
	if rec.Header().Get("X-RateLimit-Class") != "auth" {
		t.Fatalf("expected X-RateLimit-Class=auth, got %q", rec.Header().Get("X-RateLimit-Class"))
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatalf("expected Retry-After header on 429")
	}
}

func TestRateLimit_DifferentIPsHaveSeparateBuckets(t *testing.T) {
	store := newTestStore()
	mw := rateLimitMiddleware(store)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
		req.RemoteAddr = "1.2.3.4:5000"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("ip-1 burst attempt %d: expected 200, got %d", i+1, rec.Code)
		}
	}

	// Second IP should still have full budget.
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	req.RemoteAddr = "5.6.7.8:5000"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ip-2 first req: expected 200, got %d", rec.Code)
	}
}

func TestRateLimit_ReadVsMutateBucketSeparate(t *testing.T) {
	store := newTestStore()
	mw := rateLimitMiddleware(store)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Burn the mutate bucket (burst=3).
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/funds", nil)
		req.RemoteAddr = "1.2.3.4:5000"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("mutate burst %d: expected 200, got %d", i+1, rec.Code)
		}
	}

	// One more mutate from same IP -> 429.
	req := httptest.NewRequest(http.MethodPost, "/api/funds", nil)
	req.RemoteAddr = "1.2.3.4:5000"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected mutate to 429 after burst, got %d", rec.Code)
	}

	// But a GET (read class) should still pass.
	req = httptest.NewRequest(http.MethodGet, "/api/funds", nil)
	req.RemoteAddr = "1.2.3.4:5000"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected read bucket to be untouched, got %d", rec.Code)
	}
}

func TestRateLimit_ExemptsHealthMetricsAndStream(t *testing.T) {
	store := newTestStore()
	mw := rateLimitMiddleware(store)
	called := 0
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	}))

	// Hit health 100 times — should never 429.
	for i := 0; i < 100; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
		req.RemoteAddr = "1.2.3.4:5000"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("health iter %d: expected 200, got %d", i, rec.Code)
		}
	}

	// And SSE stream paths exempt (suffix /stream).
	for i := 0; i < 50; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/funds/abc/workflow/stream", nil)
		req.RemoteAddr = "1.2.3.4:5000"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("stream iter %d: expected 200, got %d", i, rec.Code)
		}
	}

	if called != 150 {
		t.Fatalf("expected handler to be invoked 150 times, got %d", called)
	}
}

func TestRateLimit_PrefersXForwardedForFirstHop(t *testing.T) {
	store := newTestStore()
	mw := rateLimitMiddleware(store)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Same RemoteAddr (proxy) but different X-Forwarded-For = different buckets.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
		req.RemoteAddr = "10.0.0.1:5000" // proxy
		req.Header.Set("X-Forwarded-For", "203.0.113.1, 10.0.0.1")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("xff-1 burst %d: 200 expected, got %d", i+1, rec.Code)
		}
	}

	// Same proxy, different originating client -> own budget.
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	req.RemoteAddr = "10.0.0.1:5000"
	req.Header.Set("X-Forwarded-For", "203.0.113.99, 10.0.0.1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("xff-2: expected 200 (separate bucket), got %d", rec.Code)
	}
}

func TestClassifyRequest(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   string
	}{
		{http.MethodPost, "/api/auth/login", "auth"},
		{http.MethodPost, "/api/auth/forgot-password", "auth"},
		{http.MethodGet, "/api/auth/session", "auth"}, // auth still wins
		{http.MethodPost, "/api/funds", "mutate"},
		{http.MethodPut, "/api/funds/abc", "mutate"},
		{http.MethodPatch, "/api/funds/abc", "mutate"},
		{http.MethodDelete, "/api/funds/abc", "mutate"},
		{http.MethodGet, "/api/funds", "read"},
		{http.MethodGet, "/api/health", "read"}, // classify, but exempt is checked separately
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		got := classifyRequest(req)
		if got != tc.want {
			t.Errorf("classifyRequest(%s %s) = %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestRateLimit_ResponseBodyShape(t *testing.T) {
	store := newTestStore()
	mw := rateLimitMiddleware(store)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// Drain.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
		req.RemoteAddr = "9.9.9.9:1"
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	req.RemoteAddr = "9.9.9.9:1"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("expected JSON content-type, got %q", got)
	}
	body := rec.Body.String()
	if want := `"error":"rate_limited"`; !strings.Contains(body, want) {
		t.Errorf("expected body to contain %q, got %s", want, body)
	}
}
