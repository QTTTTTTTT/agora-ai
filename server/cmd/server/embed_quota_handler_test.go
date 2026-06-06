package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/embedquota"
)

// TestEmbedQuotaHandlerStatusReturnsLiveStats exercises the
// happy path: a limiter that has handled a few Acquire +
// RecordUsage cycles must round-trip through the JSON shape with
// `enabled=true`, the live counters, and non-empty histogram
// summaries.
func TestEmbedQuotaHandlerStatusReturnsLiveStats(t *testing.T) {
	cfg := embedquota.DefaultConfig()
	cfg.MaxCallsPerMinute = 100
	cfg.TokenQuotaPerDay = 1_000_000
	limiter := embedquota.New(cfg)
	for i := 0; i < 3; i++ {
		if _, _, err := limiter.Acquire(50); err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
	}
	limiter.RecordUsage(45)
	limiter.RecordUsage(150)
	limiter.RecordUsage(2_500)

	h := newEmbedQuotaHandler(&Services{EmbedLimiter: limiter})
	if h == nil {
		t.Fatal("expected non-nil handler when svc is populated")
	}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/embed-quota/status", nil)
	ctx := api.WithAuthenticatedUserRole(req.Context(), userRoleSuperAdmin)
	ctx = api.WithAuthenticatedUserID(ctx, "admin-test-id")
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}

	var status embedQuotaStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, rr.Body.String())
	}
	if !status.Enabled {
		t.Error("expected Enabled=true when limiter is non-nil")
	}
	if status.Status == "" {
		t.Errorf("Status string should be populated, got empty")
	}
	if status.CallsLastMinute != 3 {
		t.Errorf("CallsLastMinute: got %d, want 3", status.CallsLastMinute)
	}
	if status.CallsPerMinuteMax != cfg.MaxCallsPerMinute {
		t.Errorf("CallsPerMinuteMax: got %d, want %d",
			status.CallsPerMinuteMax, cfg.MaxCallsPerMinute)
	}
	if status.AcquireWaitCount != 3 {
		t.Errorf("AcquireWaitCount: got %d, want 3", status.AcquireWaitCount)
	}
	if status.CallTokensCount != 3 {
		t.Errorf("CallTokensCount: got %d, want 3", status.CallTokensCount)
	}
	if status.CallTokensSum != uint64(45+150+2_500) {
		t.Errorf("CallTokensSum: got %d, want 2695", status.CallTokensSum)
	}
	if status.ObservedAt.IsZero() {
		t.Error("expected non-zero ObservedAt")
	}
	// W12-3: TokenHistory must be exactly 7 entries — the
	// Admin UI sparkline depends on a stable length.
	if len(status.TokenHistory) != tokenHistoryDays {
		t.Fatalf("TokenHistory length: got %d, want %d (Admin UI depends on this)",
			len(status.TokenHistory), tokenHistoryDays)
	}
	// Today's tokens must equal the sum of positive RecordUsage
	// calls we made above (45 + 150 + 2500 = 2695). Last element
	// is today by construction (RecentDays sorts ascending).
	today := status.TokenHistory[tokenHistoryDays-1]
	if today.Tokens != 45+150+2_500 {
		t.Errorf("today tokens in history: got %d, want 2695", today.Tokens)
	}
}

// TestEmbedQuotaHandlerStatusOmitsTokenHistoryWhenDisabled —
// when the limiter is nil the panel renders a "disabled" state.
// Sending a 7-element zero array would let a sparkline render
// flat zeros and look suspiciously like "we hit the budget so
// hard nothing went out today". `omitempty` keeps disabled and
// "really zero usage" visually distinguishable.
func TestEmbedQuotaHandlerStatusOmitsTokenHistoryWhenDisabled(t *testing.T) {
	h := newEmbedQuotaHandler(&Services{EmbedLimiter: nil})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/embed-quota/status", nil)
	ctx := api.WithAuthenticatedUserRole(req.Context(), userRoleSuperAdmin)
	ctx = api.WithAuthenticatedUserID(ctx, "admin-test-id")
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var raw map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := raw["tokenHistory"]; present {
		t.Errorf("tokenHistory should be absent when limiter is disabled, got %v",
			raw["tokenHistory"])
	}
}

// TestEmbedQuotaHandlerStatusReportsDisabledWhenLimiterNil pins
// the "feature off" contract: the route still registers and
// answers 200 with `enabled=false` and the textual
// `unavailable` status, so the Admin UI panel can render a
// disabled state without 404 special-casing.
func TestEmbedQuotaHandlerStatusReportsDisabledWhenLimiterNil(t *testing.T) {
	h := newEmbedQuotaHandler(&Services{EmbedLimiter: nil})
	if h == nil {
		t.Fatal("handler must register even with nil limiter")
	}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/embed-quota/status", nil)
	ctx := api.WithAuthenticatedUserRole(req.Context(), userRoleSuperAdmin)
	ctx = api.WithAuthenticatedUserID(ctx, "admin-test-id")
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	var status embedQuotaStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status.Enabled {
		t.Error("expected Enabled=false with nil limiter")
	}
	if status.Status != string(embedquota.StatusUnavailable) {
		t.Errorf("Status: got %q, want %q", status.Status, embedquota.StatusUnavailable)
	}
	// Counters MUST be zero when disabled — otherwise the panel
	// shows misleading data.
	if status.TokensTodayUsed != 0 || status.CallsLastMinute != 0 ||
		status.ThrottledTotal != 0 || status.AcquireWaitCount != 0 {
		t.Errorf("disabled limiter should zero every counter, got %+v", status)
	}
}

// TestEmbedQuotaHandlerStatusRejectsNonAdmin verifies the
// super-admin gate returns 401 / 403 for unauthenticated /
// regular-user callers. The limiter state could leak token
// budget and traffic patterns, both of which are operator-only.
func TestEmbedQuotaHandlerStatusRejectsNonAdmin(t *testing.T) {
	limiter := embedquota.New(embedquota.DefaultConfig())
	h := newEmbedQuotaHandler(&Services{EmbedLimiter: limiter})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	cases := []struct {
		name string
		ctx  context.Context
		want int
	}{
		{name: "unauthenticated", ctx: context.Background(), want: http.StatusUnauthorized},
		{
			name: "regular user",
			ctx: api.WithAuthenticatedUserRole(
				api.WithAuthenticatedUserID(context.Background(), "u1"),
				userRoleUser,
			),
			want: http.StatusForbidden,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/admin/embed-quota/status", nil).
				WithContext(tc.ctx)
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Fatalf("expected %d, got %d (body=%s)", tc.want, rr.Code, rr.Body.String())
			}
		})
	}
}

// TestEmbedQuotaHandlerNilSafe pins the codebase convention:
// constructor returns nil when Services is missing, router
// then skips route registration cleanly.
func TestEmbedQuotaHandlerNilSafe(t *testing.T) {
	if h := newEmbedQuotaHandler(nil); h != nil {
		t.Errorf("expected nil handler when svc is nil")
	}
}

// TestEmbedQuotaHandlerComputesP99FromBucketsCorrectly is the
// pure-math sibling of the round-trip test above. Pins the
// behaviour of histogramP99WaitSeconds / histogramP99Tokens
// without going through HTTP — fast feedback when the bucket
// math is the thing under suspicion.
func TestEmbedQuotaHandlerComputesP99FromBucketsCorrectly(t *testing.T) {
	t.Run("empty histogram returns 0", func(t *testing.T) {
		var l *embedquota.Limiter
		if got := histogramP99WaitSeconds(l.WaitHistogram()); got != 0 {
			t.Errorf("nil-limiter WaitHistogram p99: got %v, want 0", got)
		}
		if got := histogramP99Tokens(l.TokenHistogram()); got != 0 {
			t.Errorf("nil-limiter TokenHistogram p99: got %v, want 0", got)
		}
	})

	t.Run("p99 lands in the smallest bucket when all observations are 0-wait", func(t *testing.T) {
		cfg := embedquota.DefaultConfig()
		cfg.MaxCallsPerMinute = 100
		cfg.TokenQuotaPerDay = 1_000_000
		l := embedquota.New(cfg)
		for i := 0; i < 100; i++ {
			if _, _, err := l.Acquire(10); err != nil {
				t.Fatalf("Acquire %d: %v", i, err)
			}
		}
		got := histogramP99WaitSeconds(l.WaitHistogram())
		// Smallest bucket is le=0.001; with 100 observations
		// all in that bucket, p99 must hit it (100 ≥ 99% of 100).
		if got != 0.001 {
			t.Errorf("p99 should land in le=0.001 bucket, got %v", got)
		}
	})

	t.Run("p99 climbs to the bucket where 99% accumulates", func(t *testing.T) {
		l := embedquota.New(embedquota.DefaultConfig())
		// 99 small + 1 large: p99 must include the large one,
		// landing in the bucket that holds it.
		for i := 0; i < 99; i++ {
			l.RecordUsage(45) // → le=50
		}
		l.RecordUsage(60_000) // → le=100000

		got := histogramP99Tokens(l.TokenHistogram())
		// 99 obs at le=50 is 99% of 100 → cumulative count at
		// le=50 = 99, target = 100*0.99 = 99 → p99 lands at le=50.
		if got != 50 {
			t.Errorf("p99 should land at le=50 (cumulative count just hits 99%%), got %v", got)
		}
	})
}
