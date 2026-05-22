package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type stubAgentSkillService struct {
	listFn    func(userID, agentID string) (*AgentSkillList, error)
	approveFn func(userID, agentID, skillKey string) (*AgentSkillEntry, error)
	rejectFn  func(userID, agentID, skillKey string) error
}

func (s stubAgentSkillService) ListSkills(userID, agentID string) (*AgentSkillList, error) {
	if s.listFn != nil {
		return s.listFn(userID, agentID)
	}
	return nil, errors.New("unexpected ListSkills call")
}

func (s stubAgentSkillService) ApproveSkill(userID, agentID, skillKey string) (*AgentSkillEntry, error) {
	if s.approveFn != nil {
		return s.approveFn(userID, agentID, skillKey)
	}
	return nil, errors.New("unexpected ApproveSkill call")
}

func (s stubAgentSkillService) RejectSkill(userID, agentID, skillKey string) error {
	if s.rejectFn != nil {
		return s.rejectFn(userID, agentID, skillKey)
	}
	return errors.New("unexpected RejectSkill call")
}

func newSkillTestHandler(svc AgentSkillService) *FundHandler {
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
	).WithAgentSkillService(svc)
}

func TestListAgentSkillsReturnsList(t *testing.T) {
	t.Parallel()
	stub := stubAgentSkillService{
		listFn: func(userID, agentID string) (*AgentSkillList, error) {
			if userID != "u-1" || agentID != "ag-1" {
				t.Fatalf("unexpected args: user=%s agent=%s", userID, agentID)
			}
			return &AgentSkillList{
				AgentID: agentID,
				Skills: []AgentSkillEntry{
					{Key: "reflection:r-1", Name: "Reflection: chip", Status: "proposed", Enabled: false},
					{Key: "manual-1", Name: "Manual skill", Status: "approved", Enabled: true},
				},
			}, nil
		},
	}
	mux := http.NewServeMux()
	newSkillTestHandler(stub).RegisterRoutes(mux)
	req := authRequest(httptest.NewRequest(http.MethodGet, "/api/agents/ag-1/skills", nil), "u-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp AgentSkillList
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.AgentID != "ag-1" || len(resp.Skills) != 2 {
		t.Fatalf("unexpected payload: %+v", resp)
	}
	if resp.Skills[0].Status != "proposed" || resp.Skills[1].Status != "approved" {
		t.Fatalf("expected one proposed + one approved, got %+v", resp.Skills)
	}
}

func TestApproveAgentSkillRoutesArgsAndReturnsEntry(t *testing.T) {
	t.Parallel()
	called := false
	stub := stubAgentSkillService{
		approveFn: func(userID, agentID, skillKey string) (*AgentSkillEntry, error) {
			called = true
			if userID != "u-1" || agentID != "ag-1" || skillKey != "reflection:r-1" {
				t.Fatalf("unexpected args: user=%s agent=%s key=%s", userID, agentID, skillKey)
			}
			return &AgentSkillEntry{Key: skillKey, Status: "approved", Enabled: true, ApprovedAt: "2026-05-19T12:00:00Z"}, nil
		},
	}
	mux := http.NewServeMux()
	newSkillTestHandler(stub).RegisterRoutes(mux)
	req := authRequest(httptest.NewRequest(http.MethodPost, "/api/agents/ag-1/skills/reflection:r-1/approve", nil), "u-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if !called {
		t.Fatalf("expected ApproveSkill to be invoked")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var entry AgentSkillEntry
	if err := json.Unmarshal(rr.Body.Bytes(), &entry); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if entry.Status != "approved" || !entry.Enabled {
		t.Fatalf("unexpected entry: %+v", entry)
	}
}

func TestRejectAgentSkillRespondsNoContent(t *testing.T) {
	t.Parallel()
	stub := stubAgentSkillService{
		rejectFn: func(userID, agentID, skillKey string) error {
			if userID != "u-1" || agentID != "ag-1" || skillKey != "manual-2" {
				t.Fatalf("unexpected args: user=%s agent=%s key=%s", userID, agentID, skillKey)
			}
			return nil
		},
	}
	mux := http.NewServeMux()
	newSkillTestHandler(stub).RegisterRoutes(mux)
	req := authRequest(httptest.NewRequest(http.MethodDelete, "/api/agents/ag-1/skills/manual-2", nil), "u-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d body=%s", rr.Code, rr.Body.String())
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("expected empty body, got %q", rr.Body.String())
	}
}

func TestAgentSkillsRequireAuth(t *testing.T) {
	t.Parallel()
	stub := stubAgentSkillService{
		listFn: func(_, _ string) (*AgentSkillList, error) {
			t.Fatal("must not call service when unauthenticated")
			return nil, nil
		},
	}
	mux := http.NewServeMux()
	newSkillTestHandler(stub).RegisterRoutes(mux)
	for _, path := range []string{
		"/api/agents/ag-1/skills",
		"/api/agents/ag-1/skills/k/approve",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if path == "/api/agents/ag-1/skills/k/approve" {
			// POST route, but we used GET — server returns 405 Method Not
			// Allowed before auth check fires. Switch to POST.
			req = httptest.NewRequest(http.MethodPost, path, nil)
			rr = httptest.NewRecorder()
			mux.ServeHTTP(rr, req)
		}
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("path=%s expected 401, got %d", path, rr.Code)
		}
	}
}

func TestAgentSkillsServiceUnavailableWhenUnwired(t *testing.T) {
	t.Parallel()
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
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/agents/ag-1/skills"},
		{http.MethodPost, "/api/agents/ag-1/skills/k/approve"},
		{http.MethodDelete, "/api/agents/ag-1/skills/k"},
	} {
		req := authRequest(httptest.NewRequest(tc.method, tc.path, nil), "u-1")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s: expected 503, got %d body=%s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "agent_skills_unavailable") {
			t.Fatalf("%s %s: expected error code agent_skills_unavailable, got body=%s", tc.method, tc.path, rr.Body.String())
		}
	}
}

func TestRejectAgentSkillNotFoundPropagates(t *testing.T) {
	t.Parallel()
	stub := stubAgentSkillService{
		rejectFn: func(_, _, _ string) error { return ErrNotFound },
	}
	mux := http.NewServeMux()
	newSkillTestHandler(stub).RegisterRoutes(mux)
	req := authRequest(httptest.NewRequest(http.MethodDelete, "/api/agents/ag-1/skills/missing", nil), "u-1")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing skill, got %d", rr.Code)
	}
}
