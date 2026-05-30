package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// stubABOperationalAttributionService is the minimum impl for
// HTTP-layer tests. attributionFn is rebound per test; the
// default returns "unexpected" so missing bindings fail loudly.
type stubABOperationalAttributionService struct {
	attributionFn func(ctx context.Context, userID, testID string) (ABTestOperationalAttribution, error)
}

func (s stubABOperationalAttributionService) OperationalAttribution(ctx context.Context, userID, testID string) (ABTestOperationalAttribution, error) {
	if s.attributionFn != nil {
		return s.attributionFn(ctx, userID, testID)
	}
	return ABTestOperationalAttribution{}, errors.New("unexpected OperationalAttribution call")
}

func newABAttributionHandlerForTest(t *testing.T, svc ABOperationalAttributionService) *http.ServeMux {
	t.Helper()
	h := NewFundHandler(
		stubFundService{}, stubTeamService{}, stubPlanService{}, stubTradeService{},
		stubWorkflowService{}, stubMemoryService{}, stubDecisionTraceService{},
		stubMarketService{}, stubABTestService{}, stubMarketplaceService{},
	).WithABOperationalAttributionService(svc)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return mux
}

// TestGetABOperationalAttribution_RequiresAuth: no userID context → 401.
func TestGetABOperationalAttribution_RequiresAuth(t *testing.T) {
	mux := newABAttributionHandlerForTest(t, stubABOperationalAttributionService{})
	req := httptest.NewRequest(http.MethodGet, "/api/abtests/test-1/operational-attribution", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

// TestGetABOperationalAttribution_ServiceUnavailable: nil service → 503.
func TestGetABOperationalAttribution_ServiceUnavailable(t *testing.T) {
	h := NewFundHandler(
		stubFundService{}, stubTeamService{}, stubPlanService{}, stubTradeService{},
		stubWorkflowService{}, stubMemoryService{}, stubDecisionTraceService{},
		stubMarketService{}, stubABTestService{}, stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/abtests/test-1/operational-attribution", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

// TestGetABOperationalAttribution_ForbiddenMapping: ErrForbidden → 403.
func TestGetABOperationalAttribution_ForbiddenMapping(t *testing.T) {
	stub := stubABOperationalAttributionService{
		attributionFn: func(ctx context.Context, userID, testID string) (ABTestOperationalAttribution, error) {
			return ABTestOperationalAttribution{}, ErrForbidden
		},
	}
	mux := newABAttributionHandlerForTest(t, stub)
	req := httptest.NewRequest(http.MethodGet, "/api/abtests/test-1/operational-attribution", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "stranger"))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rr.Code)
	}
}

// TestGetABOperationalAttribution_HappyPath asserts the JSON
// wire shape: per-symbol rows, totals, and that nil BySymbol
// gets normalised to an empty slice.
func TestGetABOperationalAttribution_HappyPath(t *testing.T) {
	stub := stubABOperationalAttributionService{
		attributionFn: func(ctx context.Context, userID, testID string) (ABTestOperationalAttribution, error) {
			if userID != "user-1" || testID != "test-1" {
				t.Fatalf("auth args: user=%q test=%q", userID, testID)
			}
			return ABTestOperationalAttribution{
				TestID: testID,
				TotalA: ABAttributionTotals{
					TradeCount:   3,
					Turnover:     30000,
					RealizedPnL:  450,
					WinTradeRate: 0.66,
					AvgPnL:       150,
				},
				TotalB: ABAttributionTotals{
					TradeCount:   3,
					Turnover:     45000,
					RealizedPnL:  900,
					WinTradeRate: 0.66,
					AvgPnL:       300,
				},
				BySymbol: []ABAttributionSymbolRow{
					{
						Symbol:           "AAPL",
						TradeCountA:      1,
						TradeCountB:      1,
						RealizedPnLA:     100,
						RealizedPnLB:     200,
						TurnoverA:        10000,
						TurnoverB:        15000,
						PnLGap:           100,
						GapPctOfNotional: 0.66,
						Winner:           "B",
					},
				},
			}, nil
		},
	}
	mux := newABAttributionHandlerForTest(t, stub)
	req := httptest.NewRequest(http.MethodGet, "/api/abtests/test-1/operational-attribution", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp ABTestOperationalAttribution
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.TestID != "test-1" {
		t.Errorf("testId = %q, want test-1", resp.TestID)
	}
	if resp.TotalB.RealizedPnL != 900 {
		t.Errorf("totalB.realizedPnL = %v, want 900", resp.TotalB.RealizedPnL)
	}
	if len(resp.BySymbol) != 1 || resp.BySymbol[0].Winner != "B" {
		t.Errorf("bySymbol mismatch: %+v", resp.BySymbol)
	}
}

// TestGetABOperationalAttribution_NilBySymbol: handler must
// normalise nil BySymbol to an empty array so the client never
// sees JSON null.
func TestGetABOperationalAttribution_NilBySymbol(t *testing.T) {
	stub := stubABOperationalAttributionService{
		attributionFn: func(ctx context.Context, userID, testID string) (ABTestOperationalAttribution, error) {
			return ABTestOperationalAttribution{TestID: testID}, nil
		},
	}
	mux := newABAttributionHandlerForTest(t, stub)
	req := httptest.NewRequest(http.MethodGet, "/api/abtests/test-empty/operational-attribution", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v, ok := resp["bySymbol"]; !ok || v == nil {
		t.Errorf("bySymbol must be non-nil; got %#v", resp["bySymbol"])
	}
}
