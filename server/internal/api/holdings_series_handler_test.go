package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type stubHoldingsSeriesService struct {
	historyFn func(ctx context.Context, userID, fundID string, days int) (HoldingsSeriesResponse, error)
}

func (s stubHoldingsSeriesService) HoldingsSeries(ctx context.Context, userID, fundID string, days int) (HoldingsSeriesResponse, error) {
	if s.historyFn != nil {
		return s.historyFn(ctx, userID, fundID, days)
	}
	return HoldingsSeriesResponse{}, nil
}

func newHoldingsSeriesHandlerForTest(t *testing.T, svc HoldingsSeriesService) *http.ServeMux {
	t.Helper()
	h := NewFundHandler(
		stubFundService{}, stubTeamService{}, stubPlanService{}, stubTradeService{},
		stubWorkflowService{}, stubMemoryService{}, stubDecisionTraceService{},
		stubMarketService{}, stubABTestService{}, stubMarketplaceService{},
	).WithHoldingsSeriesService(svc)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return mux
}

func TestGetHoldingsSeries_RequiresAuth(t *testing.T) {
	mux := newHoldingsSeriesHandlerForTest(t, stubHoldingsSeriesService{})
	req := httptest.NewRequest(http.MethodGet, "/api/funds/f1/holdings/series", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestGetHoldingsSeries_ServiceUnavailable(t *testing.T) {
	h := NewFundHandler(
		stubFundService{}, stubTeamService{}, stubPlanService{}, stubTradeService{},
		stubWorkflowService{}, stubMemoryService{}, stubDecisionTraceService{},
		stubMarketService{}, stubABTestService{}, stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/f1/holdings/series", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "u1"))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

func TestGetHoldingsSeries_DaysClamping(t *testing.T) {
	cases := []struct {
		raw      string
		wantDays int
	}{
		{"", 90},
		{"abc", 90},
		{"3", 7},
		{"5000", 1825},
		{"30", 30},
	}
	for _, tc := range cases {
		tc := tc
		t.Run("days="+tc.raw, func(t *testing.T) {
			var captured int
			stub := stubHoldingsSeriesService{
				historyFn: func(_ context.Context, _, _ string, days int) (HoldingsSeriesResponse, error) {
					captured = days
					return HoldingsSeriesResponse{}, nil
				},
			}
			mux := newHoldingsSeriesHandlerForTest(t, stub)
			path := "/api/funds/f1/holdings/series"
			if tc.raw != "" {
				path += "?days=" + tc.raw
			}
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req = req.WithContext(WithAuthenticatedUserID(req.Context(), "u1"))
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
			}
			if captured != tc.wantDays {
				t.Errorf("days forwarded = %d, want %d", captured, tc.wantDays)
			}
		})
	}
}

func TestGetHoldingsSeries_HappyPath(t *testing.T) {
	stub := stubHoldingsSeriesService{
		historyFn: func(_ context.Context, userID, fundID string, days int) (HoldingsSeriesResponse, error) {
			if userID != "u1" || fundID != "f1" {
				t.Fatalf("auth args u=%q f=%q", userID, fundID)
			}
			return HoldingsSeriesResponse{
				FundID: "f1",
				From:   "2026-02-28",
				To:     "2026-05-29",
				Items: []HoldingSeriesDTO{
					{
						InstrumentKey: "INSTR:NVDA",
						Symbol:        "NVDA",
						Market:        "us_equity",
						EntryPrice:    480.0,
						Points: []BenchmarkPointDTO{
							{Date: "2026-02-28", Value: 100.0},
							{Date: "2026-05-29", Value: 110.0},
						},
					},
				},
			}, nil
		},
	}
	mux := newHoldingsSeriesHandlerForTest(t, stub)
	req := httptest.NewRequest(http.MethodGet, "/api/funds/f1/holdings/series", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "u1"))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var resp HoldingsSeriesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 || resp.Items[0].Symbol != "NVDA" {
		t.Errorf("items = %+v", resp.Items)
	}
}

func TestGetHoldingsSeries_NilItemsBecomesEmpty(t *testing.T) {
	stub := stubHoldingsSeriesService{
		historyFn: func(_ context.Context, _, _ string, _ int) (HoldingsSeriesResponse, error) {
			return HoldingsSeriesResponse{FundID: "f1"}, nil
		},
	}
	mux := newHoldingsSeriesHandlerForTest(t, stub)
	req := httptest.NewRequest(http.MethodGet, "/api/funds/f1/holdings/series", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "u1"))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), `"items":[]`) {
		t.Errorf("expected items:[]; body=%s", rr.Body.String())
	}
}
