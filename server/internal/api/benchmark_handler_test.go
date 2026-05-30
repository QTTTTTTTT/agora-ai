package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// stubBenchmarkService impls BenchmarkService for HTTP-layer tests.
// Each test rebinds historyFn to inject the response/error it needs.
type stubBenchmarkService struct {
	historyFn func(ctx context.Context, userID, fundID string, days int, ids []string) (BenchmarkHistoryResponse, error)
}

func (s stubBenchmarkService) History(ctx context.Context, userID, fundID string, days int, ids []string) (BenchmarkHistoryResponse, error) {
	if s.historyFn != nil {
		return s.historyFn(ctx, userID, fundID, days, ids)
	}
	return BenchmarkHistoryResponse{}, errors.New("unexpected History call")
}

func newBenchmarkHandlerForTest(t *testing.T, svc BenchmarkService) (*FundHandler, *http.ServeMux) {
	t.Helper()
	h := NewFundHandler(
		stubFundService{}, stubTeamService{}, stubPlanService{}, stubTradeService{},
		stubWorkflowService{}, stubMemoryService{}, stubDecisionTraceService{},
		stubMarketService{}, stubABTestService{}, stubMarketplaceService{},
	).WithBenchmarkService(svc)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return h, mux
}

func TestGetBenchmarkHistory_RequiresAuth(t *testing.T) {
	_, mux := newBenchmarkHandlerForTest(t, stubBenchmarkService{})
	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/benchmark-history", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestGetBenchmarkHistory_ServiceUnavailable(t *testing.T) {
	h := NewFundHandler(
		stubFundService{}, stubTeamService{}, stubPlanService{}, stubTradeService{},
		stubWorkflowService{}, stubMemoryService{}, stubDecisionTraceService{},
		stubMarketService{}, stubABTestService{}, stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/benchmark-history", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

// TestGetBenchmarkHistory_DaysClamping pins the soft-clamp
// behaviour. Out-of-range / non-numeric values must NOT 400 — the
// chart panel is a soft surface, a manipulated query string should
// degrade gracefully.
func TestGetBenchmarkHistory_DaysClamping(t *testing.T) {
	cases := []struct {
		raw      string
		wantDays int
	}{
		{"", 90},          // default
		{"abc", 90},       // non-numeric → default
		{"3", 7},          // below floor → clamp to 7
		{"-100", 7},       // negative → clamp to 7
		{"5000", 1825},    // above ceiling → clamp to 5y
		{"30", 30},        // valid
		{"1825", 1825},    // boundary valid
	}
	for _, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("days=%q", tc.raw), func(t *testing.T) {
			var captured int
			stub := stubBenchmarkService{
				historyFn: func(_ context.Context, _, _ string, days int, _ []string) (BenchmarkHistoryResponse, error) {
					captured = days
					return BenchmarkHistoryResponse{}, nil
				},
			}
			_, mux := newBenchmarkHandlerForTest(t, stub)
			path := "/api/funds/fund-1/benchmark-history"
			if tc.raw != "" {
				path += "?days=" + tc.raw
			}
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d", rr.Code)
			}
			if captured != tc.wantDays {
				t.Errorf("forwarded days = %d, want %d", captured, tc.wantDays)
			}
		})
	}
}

// TestGetBenchmarkHistory_SeriesParsing pins how the comma-separated
// series query is normalized: lowercase, trimmed, deduplicated,
// preserving the user's order. Empty value should forward nil so
// the service falls back to its own recommendation.
func TestGetBenchmarkHistory_SeriesParsing(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{"spx", []string{"spx"}},
		{"spx,csi300", []string{"spx", "csi300"}},
		{"  spx , CSI300  ", []string{"spx", "csi300"}},
		{"spx,spx,csi300", []string{"spx", "csi300"}},      // dedup
		{",,spx,,", []string{"spx"}},                       // empty parts dropped
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.raw, func(t *testing.T) {
			var captured []string
			stub := stubBenchmarkService{
				historyFn: func(_ context.Context, _, _ string, _ int, ids []string) (BenchmarkHistoryResponse, error) {
					captured = ids
					return BenchmarkHistoryResponse{}, nil
				},
			}
			_, mux := newBenchmarkHandlerForTest(t, stub)
			path := "/api/funds/fund-1/benchmark-history"
			if tc.raw != "" {
				path += "?series=" + url.QueryEscape(tc.raw)
			}
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d", rr.Code)
			}
			if !equalStringSlice(captured, tc.want) {
				t.Errorf("ids forwarded = %v, want %v", captured, tc.want)
			}
		})
	}
}

func TestGetBenchmarkHistory_HappyPath(t *testing.T) {
	stub := stubBenchmarkService{
		historyFn: func(_ context.Context, userID, fundID string, days int, ids []string) (BenchmarkHistoryResponse, error) {
			if userID != "u1" || fundID != "f1" {
				t.Fatalf("auth args u=%q f=%q", userID, fundID)
			}
			if days != 90 {
				t.Errorf("days = %d, want 90", days)
			}
			return BenchmarkHistoryResponse{
				FundID: "f1",
				From:   "2026-02-28",
				To:     "2026-05-29",
				Fund: BenchmarkSeriesDTO{
					ID: "fund:f1", Label: "OCS", Symbol: "f1", Market: "us_equity",
					Points: []BenchmarkPointDTO{
						{Date: "2026-02-28", Value: 100.0},
						{Date: "2026-05-29", Value: 99.92},
					},
				},
				Benchmarks: []BenchmarkSeriesDTO{
					{
						ID: "spx", Label: "S&P 500", Symbol: "^GSPC", Market: "us_equity",
						Points: []BenchmarkPointDTO{
							{Date: "2026-02-28", Value: 100.0},
							{Date: "2026-05-29", Value: 102.5},
						},
					},
				},
				Recommended: []string{"spx"},
			}, nil
		},
	}
	_, mux := newBenchmarkHandlerForTest(t, stub)
	req := httptest.NewRequest(http.MethodGet, "/api/funds/f1/benchmark-history", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "u1"))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var resp BenchmarkHistoryResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.FundID != "f1" {
		t.Errorf("FundID = %q", resp.FundID)
	}
	if len(resp.Benchmarks) != 1 || resp.Benchmarks[0].ID != "spx" {
		t.Errorf("benchmarks = %+v", resp.Benchmarks)
	}
	if len(resp.Fund.Points) == 0 {
		t.Error("fund.points missing")
	}
}

func TestGetBenchmarkHistory_NilArraysBecomeEmpty(t *testing.T) {
	// The handler must defensively replace nil arrays with empty
	// arrays so the JSON envelope never carries `null`. Frontend
	// consumers reduce / map over these and would crash on null.
	stub := stubBenchmarkService{
		historyFn: func(_ context.Context, _, _ string, _ int, _ []string) (BenchmarkHistoryResponse, error) {
			return BenchmarkHistoryResponse{
				FundID: "f1",
				// leave Benchmarks / Recommended / Available as nil
			}, nil
		},
	}
	_, mux := newBenchmarkHandlerForTest(t, stub)
	req := httptest.NewRequest(http.MethodGet, "/api/funds/f1/benchmark-history", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "u1"))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, key := range []string{`"benchmarks":[]`, `"recommended":[]`, `"available":[]`} {
		if !strings.Contains(body, key) {
			t.Errorf("body missing %q; body=%s", key, body)
		}
	}
}

func TestGetBenchmarkHistory_ForbiddenFromService(t *testing.T) {
	stub := stubBenchmarkService{
		historyFn: func(_ context.Context, _, _ string, _ int, _ []string) (BenchmarkHistoryResponse, error) {
			return BenchmarkHistoryResponse{}, ErrForbidden
		},
	}
	_, mux := newBenchmarkHandlerForTest(t, stub)
	req := httptest.NewRequest(http.MethodGet, "/api/funds/f99/benchmark-history", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "u1"))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rr.Code)
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
