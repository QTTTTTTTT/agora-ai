package main

import (
	"net/http"
	"net/http/httptest"
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

func TestComplianceGeoMiddleware_NoHeaderFailsOpen(t *testing.T) {
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
		t.Errorf("no country header should fail open")
	}
	if rec.Header().Get("X-Compliance-Country") != "" {
		t.Errorf("no country header should not set X-Compliance-Country")
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
