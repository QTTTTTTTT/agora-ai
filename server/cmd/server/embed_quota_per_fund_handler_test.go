// embed_quota_per_fund_handler_test.go — covers the W14-3
// /api/admin/embed-quota/per-fund handler. The /status sibling
// is already covered; these tests pin the per-fund route's
// behaviour around super-admin gating, the empty-recorder
// disabled path, and the JSON wire shape that the future Admin
// UI panel will consume.

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/embedquotaobs"
)

// helper: build the handler bound to a fresh recorder, register
// routes, and return an authenticated GET request prepared for
// the per-fund route. Test bodies focus on assertions, not
// boilerplate.
func newPerFundTestEnv(t *testing.T, recorder *embedquotaobs.Recorder) (*httptest.ResponseRecorder, *http.Request, *http.ServeMux, *embedQuotaHandler) {
	t.Helper()
	h := newEmbedQuotaHandler(&Services{EmbedQuotaRecorder: recorder})
	if h == nil {
		t.Fatal("expected non-nil handler when svc is populated")
	}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/embed-quota/per-fund", nil)
	ctx := api.WithAuthenticatedUserRole(req.Context(), userRoleSuperAdmin)
	ctx = api.WithAuthenticatedUserID(ctx, "admin-test-id")
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	return rr, req, mux, h
}

func TestEmbedQuotaPerFundHandler_DisabledWhenRecorderNil(t *testing.T) {
	rr, req, mux, _ := newPerFundTestEnv(t, nil)
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	var got embedQuotaPerFundResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Enabled {
		t.Errorf("expected enabled=false on nil recorder, got true")
	}
	// `Funds` MUST be a non-nil empty slice — JSON consumers
	// crash less obviously on `null` than they do on `[]`,
	// and this is a public wire shape so we lock that in.
	if got.Funds == nil {
		t.Errorf("expected funds field to be empty slice, got nil")
	}
	if len(got.Funds) != 0 {
		t.Errorf("expected zero funds when disabled, got %d", len(got.Funds))
	}
	if got.ObservedAt.IsZero() {
		t.Errorf("expected observedAt to be set even when disabled")
	}
}

func TestEmbedQuotaPerFundHandler_EnabledEmptyEmitsZeroFunds(t *testing.T) {
	rec := embedquotaobs.New(embedquotaobs.Config{MaxFunds: 8})
	defer rec.Close()
	rr, req, mux, _ := newPerFundTestEnv(t, rec)
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var got embedQuotaPerFundResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Enabled {
		t.Errorf("expected enabled=true when recorder wired, got false")
	}
	if len(got.Funds) != 0 {
		t.Errorf("expected zero funds when no observations recorded, got %d", len(got.Funds))
	}
}

// TestEmbedQuotaPerFundHandler_PopulatesEntriesFromRecorder is
// the happy-path integration: record a couple of calls + a
// throttle, hit the endpoint, assert the wire shape carries
// the live numbers in sorted-by-fundID order.
func TestEmbedQuotaPerFundHandler_PopulatesEntriesFromRecorder(t *testing.T) {
	rec := embedquotaobs.New(embedquotaobs.Config{MaxFunds: 8, RetainFor: time.Hour})
	defer rec.Close()
	rec.RecordCall("fund-z", 100, 10*time.Millisecond)
	rec.RecordCall("fund-a", 250, 0)
	rec.RecordThrottle("fund-a")

	rr, req, mux, _ := newPerFundTestEnv(t, rec)
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	var got embedQuotaPerFundResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Enabled || len(got.Funds) != 2 {
		t.Fatalf("expected 2 funds, got %d enabled=%v", len(got.Funds), got.Enabled)
	}
	// Snapshot returns sorted by fundID ascending → fund-a
	// before fund-z (no underscore prefix in either, plain
	// lex order applies).
	if got.Funds[0].FundID != "fund-a" {
		t.Errorf("expected fund-a first, got %q", got.Funds[0].FundID)
	}
	if got.Funds[1].FundID != "fund-z" {
		t.Errorf("expected fund-z second, got %q", got.Funds[1].FundID)
	}
	if got.Funds[0].ThrottledTotal != 1 {
		t.Errorf("expected fund-a throttled=1, got %d", got.Funds[0].ThrottledTotal)
	}
	if got.Funds[0].TokensTodayUsed != 250 {
		t.Errorf("expected fund-a tokensToday=250, got %d", got.Funds[0].TokensTodayUsed)
	}
	if got.Funds[1].TokensTodayUsed != 100 {
		t.Errorf("expected fund-z tokensToday=100, got %d", got.Funds[1].TokensTodayUsed)
	}
	// fund-a's call was 250 tokens which lands in the le=500
	// bucket; the P99 estimator snaps to that bucket boundary.
	if got.Funds[0].CallTokensP99 != 500 {
		t.Errorf("expected fund-a CallTokensP99=500 (le=500 bucket), got %v", got.Funds[0].CallTokensP99)
	}
	// LastSeenAt must be populated — empty would mean we lost
	// the time field on the JSON round-trip.
	if got.Funds[0].LastSeenAt.IsZero() {
		t.Errorf("expected LastSeenAt to be populated, got zero")
	}
}

// Non-super-admin requests must be rejected. Mirrors the gate
// already enforced on /status; the per-fund route ships fund-
// level data so it must follow the same role discipline.
func TestEmbedQuotaPerFundHandler_RejectsNonSuperAdmin(t *testing.T) {
	rec := embedquotaobs.New(embedquotaobs.Config{MaxFunds: 8})
	defer rec.Close()

	h := newEmbedQuotaHandler(&Services{EmbedQuotaRecorder: rec})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/embed-quota/per-fund", nil)
	// Authenticated regular user; no super-admin role.
	ctx := api.WithAuthenticatedUserRole(req.Context(), "user")
	ctx = api.WithAuthenticatedUserID(ctx, "regular-user-id")
	req = req.WithContext(ctx)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code == http.StatusOK {
		t.Errorf("expected non-200 for non-super-admin caller, got 200 with body=%s", rr.Body.String())
	}
}

// Belt-and-braces: an UNAUTHENTICATED request (no user in ctx)
// must also be denied. Different path through requireSuperAdmin
// from the regular-user case above.
func TestEmbedQuotaPerFundHandler_RejectsUnauthenticated(t *testing.T) {
	rec := embedquotaobs.New(embedquotaobs.Config{MaxFunds: 8})
	defer rec.Close()
	h := newEmbedQuotaHandler(&Services{EmbedQuotaRecorder: rec})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/embed-quota/per-fund", nil).WithContext(context.Background())
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code == http.StatusOK {
		t.Errorf("expected non-200 for unauthenticated caller, got 200 with body=%s", rr.Body.String())
	}
}
