package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// stubABShadowAgentService is the minimum impl for HTTP-layer
// tests. shadowAgentsFn is rebound per test; the default
// returns "unexpected" so missing bindings fail loudly.
type stubABShadowAgentService struct {
	shadowAgentsFn func(ctx context.Context, userID, testID string) (ABTestShadowAgentResponse, error)
}

func (s stubABShadowAgentService) ShadowAgents(ctx context.Context, userID, testID string) (ABTestShadowAgentResponse, error) {
	if s.shadowAgentsFn != nil {
		return s.shadowAgentsFn(ctx, userID, testID)
	}
	return ABTestShadowAgentResponse{}, errors.New("unexpected ShadowAgents call")
}

// newABShadowAgentHandlerForTest stitches a FundHandler with the
// shadow-agent surface wired up. All other services are nil-stubbed
// to assert the GetABShadowAgents path doesn't reach into them.
func newABShadowAgentHandlerForTest(t *testing.T, svc ABShadowAgentService) *http.ServeMux {
	t.Helper()
	h := NewFundHandler(
		stubFundService{}, stubTeamService{}, stubPlanService{}, stubTradeService{},
		stubWorkflowService{}, stubMemoryService{}, stubDecisionTraceService{},
		stubMarketService{}, stubABTestService{}, stubMarketplaceService{},
	).WithABShadowAgentService(svc)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return mux
}

// TestGetABShadowAgents_RequiresAuth: no userID context → 401.
func TestGetABShadowAgents_RequiresAuth(t *testing.T) {
	mux := newABShadowAgentHandlerForTest(t, stubABShadowAgentService{})
	req := httptest.NewRequest(http.MethodGet, "/api/abtests/test-1/shadow-agents", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body=%s", rr.Code, rr.Body.String())
	}
}

// TestGetABShadowAgents_ServiceUnavailable: nil service → 503,
// not panic. Pinning the nil-safe branch.
func TestGetABShadowAgents_ServiceUnavailable(t *testing.T) {
	h := NewFundHandler(
		stubFundService{}, stubTeamService{}, stubPlanService{}, stubTradeService{},
		stubWorkflowService{}, stubMemoryService{}, stubDecisionTraceService{},
		stubMarketService{}, stubABTestService{}, stubMarketplaceService{},
	)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/abtests/test-1/shadow-agents", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503; body=%s", rr.Code, rr.Body.String())
	}
}

// TestGetABShadowAgents_NotFoundMapping: ErrNotFound from the
// service must surface as 404 via handleServiceError.
func TestGetABShadowAgents_NotFoundMapping(t *testing.T) {
	stub := stubABShadowAgentService{
		shadowAgentsFn: func(ctx context.Context, userID, testID string) (ABTestShadowAgentResponse, error) {
			return ABTestShadowAgentResponse{}, ErrNotFound
		},
	}
	mux := newABShadowAgentHandlerForTest(t, stub)
	req := httptest.NewRequest(http.MethodGet, "/api/abtests/missing/shadow-agents", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

// TestGetABShadowAgents_ForbiddenMapping: ErrForbidden → 403.
// Verifies the dual-fund auth path (control + treatment) lifts
// to the canonical forbidden response.
func TestGetABShadowAgents_ForbiddenMapping(t *testing.T) {
	stub := stubABShadowAgentService{
		shadowAgentsFn: func(ctx context.Context, userID, testID string) (ABTestShadowAgentResponse, error) {
			return ABTestShadowAgentResponse{}, ErrForbidden
		},
	}
	mux := newABShadowAgentHandlerForTest(t, stub)
	req := httptest.NewRequest(http.MethodGet, "/api/abtests/test-1/shadow-agents", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "stranger"))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rr.Code)
	}
}

// TestGetABShadowAgents_HappyPath asserts the response wire shape
// the Web UI relies on. Variants always 2 elements; nil Agents
// gets normalised to an empty slice so client renders "no agents
// learned anything yet" without nil-checks.
func TestGetABShadowAgents_HappyPath(t *testing.T) {
	stub := stubABShadowAgentService{
		shadowAgentsFn: func(ctx context.Context, userID, testID string) (ABTestShadowAgentResponse, error) {
			if userID != "user-1" || testID != "test-1" {
				t.Fatalf("auth args: user=%q test=%q", userID, testID)
			}
			return ABTestShadowAgentResponse{
				TestID: testID,
				Variants: []ABTestShadowAgentVariant{
					{
						VariantKey:  "A",
						VariantName: "current strategy",
						Agents: []ABTestShadowAgent{
							{
								AgentID:           "agent-1",
								AgentName:         "Risk PM",
								Role:              "pm",
								EventCount:        3,
								LatestTradingDate: "2026-05-26",
								Lessons:           []string{"avoid over-trading"},
								Adjustments:       []string{"reduce single position to 18%"},
							},
						},
					},
					{
						VariantKey:  "B",
						VariantName: "aggressive strategy",
						// Agents nil intentionally — handler must normalise to []
					},
				},
			}, nil
		},
	}
	mux := newABShadowAgentHandlerForTest(t, stub)
	req := httptest.NewRequest(http.MethodGet, "/api/abtests/test-1/shadow-agents", nil)
	req = req.WithContext(WithAuthenticatedUserID(req.Context(), "user-1"))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp ABTestShadowAgentResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.TestID != "test-1" {
		t.Errorf("testId = %q, want test-1", resp.TestID)
	}
	if len(resp.Variants) != 2 {
		t.Fatalf("variants = %d, want 2", len(resp.Variants))
	}
	if resp.Variants[0].Agents == nil || resp.Variants[1].Agents == nil {
		t.Errorf("Agents nil; handler must normalise")
	}
	if len(resp.Variants[1].Agents) != 0 {
		t.Errorf("variant B agents = %d, want empty (nil → [])", len(resp.Variants[1].Agents))
	}
	if resp.Variants[0].Agents[0].AgentID != "agent-1" {
		t.Errorf("first agent id mismatch: %q", resp.Variants[0].Agents[0].AgentID)
	}
}

// TestGetABShadowAgents_EmptyVariants: even when the service
// returns nil Variants, the handler must default to an empty
// slice so the client never sees a JSON null.
func TestGetABShadowAgents_EmptyVariants(t *testing.T) {
	stub := stubABShadowAgentService{
		shadowAgentsFn: func(ctx context.Context, userID, testID string) (ABTestShadowAgentResponse, error) {
			return ABTestShadowAgentResponse{TestID: testID}, nil
		},
	}
	mux := newABShadowAgentHandlerForTest(t, stub)
	req := httptest.NewRequest(http.MethodGet, "/api/abtests/test-empty/shadow-agents", nil)
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
	if v, ok := resp["variants"]; !ok || v == nil {
		t.Errorf("variants must be non-nil; got %#v", resp["variants"])
	}
}
