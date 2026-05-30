package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubCorpActionService is the minimum CorpActionService impl for
// HTTP-layer tests. Each test rebinds applicationsForFundFn to
// inject the response or error it needs; the default returns the
// "unexpected" sentinel so a forgotten binding fails loudly.
type stubCorpActionService struct {
	applicationsForFundFn func(ctx context.Context, userID, fundID string, limit int) ([]CorpActionApplicationDTO, error)
}

func (s stubCorpActionService) ApplicationsForFund(ctx context.Context, userID, fundID string, limit int) ([]CorpActionApplicationDTO, error) {
	if s.applicationsForFundFn != nil {
		return s.applicationsForFundFn(ctx, userID, fundID, limit)
	}
	return nil, errors.New("unexpected ApplicationsForFund call")
}

// newCorpActionHandlerForTest stitches together a FundHandler with
// only the surface this test file exercises wired up. Nil services
// for the unused dependencies confirm the GetCorpActions path does
// not accidentally reach into them.
func newCorpActionHandlerForTest(t *testing.T, svc CorpActionService) (*FundHandler, *http.ServeMux) {
	t.Helper()
	h := NewFundHandler(
		stubFundService{},
		stubTeamService{},
		stubPlanService{},
		stubTradeService{},
		stubWorkflowService{},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	).WithCorpActionService(svc)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return h, mux
}

// TestGetCorpActions_RequiresAuth verifies the endpoint surfaces a
// 401 when no userID context is attached. Mirrors the rest of the
// fund-scope endpoints — the auth middleware is shared so this is
// the canonical "auth gate works" assertion for the whole route.
func TestGetCorpActions_RequiresAuth(t *testing.T) {
	_, mux := newCorpActionHandlerForTest(t, stubCorpActionService{})
	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/corp-actions", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}
}

// TestGetCorpActions_ServiceUnavailable pins the nil-safe branch:
// deployments without corp-action wiring (older binaries, partial
// stack) must respond 503 rather than panic on a nil-pointer call.
func TestGetCorpActions_ServiceUnavailable(t *testing.T) {
	h := NewFundHandler(
		stubFundService{}, stubTeamService{}, stubPlanService{}, stubTradeService{},
		stubWorkflowService{}, stubMemoryService{}, stubDecisionTraceService{},
		stubMarketService{}, stubABTestService{}, stubMarketplaceService{},
	)
	// Note: NOT wiring WithCorpActionService.
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/corp-actions", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503; body=%s", rr.Code, rr.Body.String())
	}
}

// TestGetCorpActions_HappyPath asserts the JSON shape of the
// response is what the Web/Android UI expect. Camel-case field
// names + `items[]` envelope + count are the contract; changes
// here are breaking changes.
func TestGetCorpActions_HappyPath(t *testing.T) {
	exDate := time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)
	appliedAt := time.Date(2026, 5, 29, 3, 0, 0, 0, time.UTC)

	stub := stubCorpActionService{
		applicationsForFundFn: func(ctx context.Context, userID, fundID string, limit int) ([]CorpActionApplicationDTO, error) {
			if userID != "user-1" || fundID != "fund-1" {
				t.Fatalf("auth args: user=%q fund=%q", userID, fundID)
			}
			if limit != 50 { // default
				t.Errorf("default limit = %d, want 50", limit)
			}
			return []CorpActionApplicationDTO{
				{
					InstrumentKey: "SSE:688195",
					ExDate:        exDate,
					ActionType:    "combined",
					SplitRatio:    1.4,
					CashDividend:  0.164,
					AppliedAt:     appliedAt,
					PreQuantity:   289.0,
					PostQuantity:  404.6,
					PreCostPrice:  335.20,
					PostCostPrice: 239.42857143,
					CashCredit:    47.396,
				},
			}, nil
		},
	}
	_, mux := newCorpActionHandlerForTest(t, stub)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/corp-actions", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Items []CorpActionApplicationDTO `json:"items"`
		Count int                        `json:"count"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 1 || len(resp.Items) != 1 {
		t.Fatalf("count/items = %d/%d, want 1/1", resp.Count, len(resp.Items))
	}
	got := resp.Items[0]
	if got.InstrumentKey != "SSE:688195" {
		t.Errorf("InstrumentKey = %q", got.InstrumentKey)
	}
	if got.SplitRatio != 1.4 {
		t.Errorf("SplitRatio = %v", got.SplitRatio)
	}
	if got.CashCredit != 47.396 {
		t.Errorf("CashCredit = %v", got.CashCredit)
	}
}

// TestGetCorpActions_LimitClampedAndForwarded covers the optional
// `limit` query param. Values inside [1,200] get forwarded; values
// outside (zero, negative, > 200, non-numeric) silently fall back
// to the default. The defensive parsing prevents a manipulated
// query string from over-loading the repository.
func TestGetCorpActions_LimitClampedAndForwarded(t *testing.T) {
	cases := []struct {
		raw       string
		wantLimit int
	}{
		{"", 50},          // default
		{"abc", 50},       // non-numeric
		{"0", 50},         // out of range below
		{"-1", 50},        // negative
		{"500", 50},       // over cap
		{"25", 25},        // valid
		{"200", 200},      // boundary valid
	}
	for _, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("limit=%q", tc.raw), func(t *testing.T) {
			var captured int
			stub := stubCorpActionService{
				applicationsForFundFn: func(_ context.Context, _, _ string, limit int) ([]CorpActionApplicationDTO, error) {
					captured = limit
					return nil, nil
				},
			}
			_, mux := newCorpActionHandlerForTest(t, stub)

			path := "/api/funds/fund-1/corp-actions"
			if tc.raw != "" {
				path += "?limit=" + tc.raw
			}
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d", rr.Code)
			}
			if captured != tc.wantLimit {
				t.Errorf("forwarded limit = %d, want %d", captured, tc.wantLimit)
			}
		})
	}
}

// TestGetCorpActions_ForbiddenFromService verifies that auth errors
// the service raises (e.g. fund-membership rejection inside the
// adapter) end up as a 403 via handleServiceError. We don't hard-
// code the body because the wrapper may evolve, but the status
// code is contractual.
func TestGetCorpActions_ForbiddenFromService(t *testing.T) {
	stub := stubCorpActionService{
		applicationsForFundFn: func(_ context.Context, _, _ string, _ int) ([]CorpActionApplicationDTO, error) {
			return nil, ErrForbidden
		},
	}
	_, mux := newCorpActionHandlerForTest(t, stub)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-99/corp-actions", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

// TestGetCorpActions_NilItemsBecomesEmptyArray pins the JSON
// contract that the response is always `items: []`, never `null`.
// Frontends rely on `for ... of items` not throwing.
func TestGetCorpActions_NilItemsBecomesEmptyArray(t *testing.T) {
	stub := stubCorpActionService{
		applicationsForFundFn: func(_ context.Context, _, _ string, _ int) ([]CorpActionApplicationDTO, error) {
			return nil, nil
		},
	}
	_, mux := newCorpActionHandlerForTest(t, stub)
	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/corp-actions", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	body := rr.Body.String()
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	// The literal "items":[] is what the frontend reduces over —
	// "items":null would crash on .map().
	if !strings.Contains(body, `"items":[]`) {
		t.Errorf("body missing items:[] envelope: %s", body)
	}
}
