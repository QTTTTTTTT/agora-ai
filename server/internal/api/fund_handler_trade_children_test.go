package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestListTradesExcludeChildSlicesForwardsTrue is the safety
// assertion the UI rollout depends on: when the frontend appends
// ?exclude_child_slices=true the handler MUST forward true to
// the service layer so the splitter's child rows get filtered
// out of the response. A regression that ignored the param would
// make the trade history page render 6 rows per TWAP parent again
// — the exact mess this rollout is meant to fix.
func TestListTradesExcludeChildSlicesForwardsTrue(t *testing.T) {
	gotFlag := false
	called := false
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{},
		stubPlanService{},
		stubTradeService{
			listTradesFn: func(_, _ string, _, _ *time.Time, _, _ int, exclude bool) ([]Trade, error) {
				called = true
				gotFlag = exclude
				return []Trade{}, nil
			},
		},
		stubWorkflowService{},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/trades?exclude_child_slices=true", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d body=%q", rr.Code, rr.Body.String())
	}
	if !called {
		t.Fatal("ListTrades was not called")
	}
	if !gotFlag {
		t.Fatal("excludeChildSlices=true was not forwarded to the service")
	}
}

// TestListTradesExcludeChildSlicesDefaultsFalse pins the
// backwards-compatibility contract: any client that doesn't
// pass the new param (analytics dumps, backups, the existing
// dashboard preview endpoint, etc.) gets the legacy "every row,
// children included" behaviour. A regression that flipped the
// default to true would silently hide rows from these clients.
func TestListTradesExcludeChildSlicesDefaultsFalse(t *testing.T) {
	var gotFlag bool
	called := false
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{},
		stubPlanService{},
		stubTradeService{
			listTradesFn: func(_, _ string, _, _ *time.Time, _, _ int, exclude bool) ([]Trade, error) {
				called = true
				gotFlag = exclude
				return []Trade{}, nil
			},
		},
		stubWorkflowService{},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/trades", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d body=%q", rr.Code, rr.Body.String())
	}
	if !called {
		t.Fatal("ListTrades was not called")
	}
	if gotFlag {
		t.Fatal("default behaviour regressed: excludeChildSlices was forwarded as true without query param")
	}
}

// TestListTradesExcludeChildSlicesGarbageValuesStayFalse: a
// malformed value (e.g. ?exclude_child_slices=banana) should
// neither error nor flip the flag — strconv.ParseBool returns
// an error so we keep the safe default. A regression that
// panicked or 4xx'd here would break clients passing legacy
// truthy strings ("yes" / "on") that bool parsers vary on.
func TestListTradesExcludeChildSlicesGarbageValuesStayFalse(t *testing.T) {
	var gotFlag bool
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{},
		stubPlanService{},
		stubTradeService{
			listTradesFn: func(_, _ string, _, _ *time.Time, _, _ int, exclude bool) ([]Trade, error) {
				gotFlag = exclude
				return []Trade{}, nil
			},
		},
		stubWorkflowService{},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/trades?exclude_child_slices=banana", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d body=%q", rr.Code, rr.Body.String())
	}
	if gotFlag {
		t.Fatal("garbage param value was treated as truthy; expected safe default false")
	}
}

// TestListTradeChildrenHappyPath pins the drilldown contract:
// the new endpoint hits ListTradeChildren with the right
// (userID, fundID, tradeID), serialises the result as a JSON
// array of Trade objects, and never wraps it in an extra
// envelope (the frontend expects a bare array).
func TestListTradeChildrenHappyPath(t *testing.T) {
	gotUserID := ""
	gotFundID := ""
	gotParentID := ""
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{},
		stubPlanService{},
		stubTradeService{
			listChildrenFn: func(userID, fundID, parentID string) ([]Trade, error) {
				gotUserID = userID
				gotFundID = fundID
				gotParentID = parentID
				return []Trade{
					{ID: "child-1", FundID: fundID, Symbol: "NVDA", Side: "buy", Quantity: 200, FilledQty: 200, StrategyParentTradeID: parentID, Strategy: "twap"},
					{ID: "child-2", FundID: fundID, Symbol: "NVDA", Side: "buy", Quantity: 200, FilledQty: 200, StrategyParentTradeID: parentID, Strategy: "twap"},
				}, nil
			},
		},
		stubWorkflowService{},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/trades/parent-1/children", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d body=%q", rr.Code, rr.Body.String())
	}
	if gotUserID != "user-1" || gotFundID != "fund-1" || gotParentID != "parent-1" {
		t.Fatalf("service got wrong args user=%q fund=%q parent=%q", gotUserID, gotFundID, gotParentID)
	}

	var rows []Trade
	if err := json.Unmarshal(rr.Body.Bytes(), &rows); err != nil {
		t.Fatalf("response is not a JSON array: %v body=%q", err, rr.Body.String())
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 children got %d", len(rows))
	}
	for i, row := range rows {
		if row.StrategyParentTradeID != "parent-1" {
			t.Errorf("child[%d] missing strategyParentTradeId=parent-1 (got %q)", i, row.StrategyParentTradeID)
		}
	}
}

// TestListTradeChildrenNilSliceReturnsJSONEmptyArray: when the
// service returns a Go nil slice (e.g. parent has no children
// because it pre-dates the splitter), the handler must serialise
// it as `[]` instead of `null` so the frontend's `.length` /
// `.map` calls don't blow up. This is the canonical "no leak of
// Go nil semantics" guard.
func TestListTradeChildrenNilSliceReturnsJSONEmptyArray(t *testing.T) {
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{},
		stubPlanService{},
		stubTradeService{
			listChildrenFn: func(_, _, _ string) ([]Trade, error) {
				return nil, nil
			},
		},
		stubWorkflowService{},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/trades/parent-1/children", nil)
	req = authRequest(req, "user-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d body=%q", rr.Code, rr.Body.String())
	}
	body := strings.TrimSpace(rr.Body.String())
	if body != "[]" {
		t.Fatalf("expected literal JSON empty array, got %q", body)
	}
}

// TestListTradeChildrenRejectsUnauthenticated verifies the
// drilldown endpoint refuses requests without an authenticated
// user. Mirrors the rule on every other /api/funds/* endpoint;
// regressing this would leak per-fund trade data to anon callers.
func TestListTradeChildrenRejectsUnauthenticated(t *testing.T) {
	called := false
	handler := NewFundHandler(
		stubFundService{},
		stubTeamService{},
		stubPlanService{},
		stubTradeService{
			listChildrenFn: func(_, _, _ string) ([]Trade, error) {
				called = true
				return nil, nil
			},
		},
		stubWorkflowService{},
		stubMemoryService{},
		stubDecisionTraceService{},
		stubMarketService{},
		stubABTestService{},
		stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/trades/parent-1/children", nil)
	// intentionally NO authRequest()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401 got %d body=%q", rr.Code, rr.Body.String())
	}
	if called {
		t.Fatal("ListTradeChildren must not be called for an unauthenticated request")
	}
}
