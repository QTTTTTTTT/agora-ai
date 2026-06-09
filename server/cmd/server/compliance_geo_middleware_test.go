package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestComplianceGeoMiddleware_OFACBlocks(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	mw := complianceGeoMiddleware()(next)

	req := httptest.NewRequest(http.MethodGet, "/api/funds", nil)
	req.Header.Set("CF-IPCountry", "IR")
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnavailableForLegalReasons {
		t.Errorf("OFAC country should yield 451, got %d", rec.Code)
	}
	if called {
		t.Errorf("downstream handler must not run for OFAC blocks")
	}
}

func TestComplianceGeoMiddleware_EUWarnsButServes(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	mw := complianceGeoMiddleware()(next)

	req := httptest.NewRequest(http.MethodGet, "/api/funds", nil)
	req.Header.Set("CF-IPCountry", "DE")
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("EU country should still serve, got %d", rec.Code)
	}
	if !called {
		t.Errorf("downstream handler must run for EU")
	}
	if rec.Header().Get("X-Compliance-Region") != "EU" {
		t.Errorf("EU should set X-Compliance-Region header, got %q", rec.Header().Get("X-Compliance-Region"))
	}
	if rec.Header().Get("X-Compliance-Country") != "DE" {
		t.Errorf("expected X-Compliance-Country: DE, got %q", rec.Header().Get("X-Compliance-Country"))
	}
}

func TestComplianceGeoMiddleware_NoHeaderFailsOpenInDev(t *testing.T) {
	// Explicit env reset: middleware captures APP_ENV at construction
	// time, so a parallel test that leaked APP_ENV=production into the
	// process would otherwise flip this test red. t.Setenv is unwound
	// by the framework when the test returns.
	t.Setenv("APP_ENV", "development")
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	mw := complianceGeoMiddleware()(next)
	req := httptest.NewRequest(http.MethodGet, "/api/funds", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if !called {
		t.Errorf("no country header should fail open in development")
	}
	if rec.Header().Get("X-Compliance-Country") != "" {
		t.Errorf("no country header should not set X-Compliance-Country")
	}
}

// TestComplianceGeoMiddleware_NoHeaderFailsCloseInProduction covers the
// OFAC bypass vector: in production, a request that reaches the origin
// without any of the CDN geo headers MUST be refused (we cannot prove
// the visitor is not in a sanctioned country). This is the single
// highest-leverage line in the file — without fail-close, sanctioned
// traffic that bypasses Cloudflare/Vercel is served untouched.
func TestComplianceGeoMiddleware_NoHeaderFailsCloseInProduction(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	mw := complianceGeoMiddleware()(next)
	req := httptest.NewRequest(http.MethodGet, "/api/funds", nil)
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if called {
		t.Errorf("downstream handler must NOT run in production when CDN header is missing")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 Service Unavailable, got %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("expected JSON body, got Content-Type %q", got)
	}
	if got := rec.Body.String(); !strings.Contains(got, "geo_resolution_unavailable") {
		t.Errorf("expected geo_resolution_unavailable error code in body, got %q", got)
	}
}

// TestComplianceGeoMiddleware_HealthAllowedInProductionWithoutHeader
// verifies the exempt-path allow-list trumps fail-close. Without this
// guarantee, the production fail-close would brick uptime monitors and
// k8s liveness probes (they almost never carry the CDN header) and the
// whole feature would get rolled back on day 1.
func TestComplianceGeoMiddleware_HealthAllowedInProductionWithoutHeader(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	for _, path := range []string{"/api/health", "/api/healthz", "/api/readiness", "/api/metrics", "/api/version"} {
		t.Run(path, func(t *testing.T) {
			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			})
			mw := complianceGeoMiddleware()(next)
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			mw.ServeHTTP(rec, req)

			if !called {
				t.Errorf("exempt path %s must be served even in production without CDN header", path)
			}
			if rec.Code != http.StatusOK {
				t.Errorf("expected 200, got %d", rec.Code)
			}
		})
	}
}

func TestComplianceGeoMiddleware_HealthAlwaysAllowed(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	mw := complianceGeoMiddleware()(next)

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Header.Set("CF-IPCountry", "IR") // OFAC, would normally block
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if !called {
		t.Errorf("/api/health must be served even from OFAC country")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 from health endpoint, got %d", rec.Code)
	}
}

func TestComplianceGeoMiddleware_USStateRequiresAck(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	mw := complianceGeoMiddleware()(next)

	req := httptest.NewRequest(http.MethodGet, "/api/funds", nil)
	req.Header.Set("CF-IPCountry", "US")
	req.Header.Set("CF-Region-Code", "CA")
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if !called {
		t.Errorf("CA must still serve (non-blocking ack required)")
	}
	if rec.Header().Get("X-Compliance-Requires-Ack") == "" {
		t.Errorf("CA should set X-Compliance-Requires-Ack header")
	}
}

func TestResolveCountryHeader_Priority(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Vercel-IP-Country", "FR")
	req.Header.Set("CF-IPCountry", "DE")
	got := resolveCountryHeader(req)
	if got != "DE" {
		t.Errorf("CF-IPCountry should win over Vercel; got %q", got)
	}
}
