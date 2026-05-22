package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubReflectionService is a tiny test double for the ReflectionService
// contract. Tests configure the returned list or error; the handler does
// not look inside, so a flat function field is enough.
type stubReflectionService struct {
	listFn func(userID, fundID string, limit int) (*ReflectionList, error)
}

func (s stubReflectionService) ListReflections(userID, fundID string, limit int) (*ReflectionList, error) {
	if s.listFn != nil {
		return s.listFn(userID, fundID, limit)
	}
	return nil, errors.New("unexpected ListReflections call")
}

func newReflectionTestHandler(svc ReflectionService) *FundHandler {
	return NewFundHandler(
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
	).WithReflectionService(svc)
}

func TestListReflectionsReturnsServiceItems(t *testing.T) {
	t.Parallel()
	stub := stubReflectionService{
		listFn: func(userID, fundID string, limit int) (*ReflectionList, error) {
			if userID != "u-1" || fundID != "fund-1" {
				t.Fatalf("unexpected args: user=%s fund=%s", userID, fundID)
			}
			if limit != 50 {
				t.Fatalf("expected default limit=50, got %d", limit)
			}
			return &ReflectionList{
				FundID:      fundID,
				GeneratedAt: time.Unix(1715000000, 0).UTC(),
				Items: []ReflectionItem{{
					ID: "ref-1", FundID: fundID, Theme: "chip",
					Title: "reflection:chip:abc", Content: "lesson body",
					Tags: []string{"chip"}, CreatedAt: time.Unix(1715000100, 0).UTC(),
				}},
			}, nil
		},
	}
	mux := http.NewServeMux()
	newReflectionTestHandler(stub).RegisterRoutes(mux)
	req := authRequest(httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/reflections", nil), "u-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp ReflectionList
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.FundID != "fund-1" || len(resp.Items) != 1 || resp.Items[0].Theme != "chip" {
		t.Fatalf("unexpected payload: %+v", resp)
	}
}

func TestListReflectionsPropagatesLimitQuery(t *testing.T) {
	t.Parallel()
	gotLimit := 0
	stub := stubReflectionService{
		listFn: func(_, _ string, limit int) (*ReflectionList, error) {
			gotLimit = limit
			return &ReflectionList{FundID: "fund-1", Items: []ReflectionItem{}}, nil
		},
	}
	mux := http.NewServeMux()
	newReflectionTestHandler(stub).RegisterRoutes(mux)
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"?limit=10", 10},
		{"?limit=999", 200},  // clamped to 200 max
		{"?limit=-3", 50},    // ignored → fallback
		{"?limit=notnum", 50}, // ignored → fallback
		{"", 50},
	} {
		req := authRequest(httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/reflections"+tc.query, nil), "u-1")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		if gotLimit != tc.want {
			t.Fatalf("query=%q: expected limit=%d, got %d", tc.query, tc.want, gotLimit)
		}
	}
}

func TestListReflectionsRequiresAuth(t *testing.T) {
	t.Parallel()
	stub := stubReflectionService{
		listFn: func(_, _ string, _ int) (*ReflectionList, error) {
			t.Fatal("service must not be called when unauthenticated")
			return nil, nil
		},
	}
	mux := http.NewServeMux()
	newReflectionTestHandler(stub).RegisterRoutes(mux)
	// No auth context
	req := httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/reflections", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestListReflectionsServiceUnavailableWhenUnwired(t *testing.T) {
	t.Parallel()
	// Construct a handler with NO reflection service to verify the 503
	// path. This is the "server not configured" hand-back to the UI.
	handler := NewFundHandler(
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
	)
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	req := authRequest(httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/reflections", nil), "u-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "reflection_unavailable") {
		t.Fatalf("expected error code reflection_unavailable, got body=%s", rr.Body.String())
	}
}

func TestListReflectionsNullableSafetyDefaultsItemsToEmptyArray(t *testing.T) {
	t.Parallel()
	stub := stubReflectionService{
		listFn: func(_, _ string, _ int) (*ReflectionList, error) {
			// Service returns nil pointer; handler must default to an
			// empty list rather than emit `null` (frontend treats null
			// as a different state than empty).
			return nil, nil
		},
	}
	mux := http.NewServeMux()
	newReflectionTestHandler(stub).RegisterRoutes(mux)
	req := authRequest(httptest.NewRequest(http.MethodGet, "/api/funds/fund-1/reflections", nil), "u-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"items":[]`) {
		t.Fatalf("expected items default to empty array, got body=%s", body)
	}
}
