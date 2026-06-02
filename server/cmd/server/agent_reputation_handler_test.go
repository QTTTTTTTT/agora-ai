// agent_reputation_handler_test.go — S8.4 per-fund read API tests.
// Repo internals are covered in agentreputation/repo_test.go; here
// we focus on the wiring (auth, fund ownership, projection).

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/agentreputation"
)

func newAgentReputationHandlerEnv(t *testing.T) (*agentReputationHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock := newMockDB(t)
	svc := &Services{DB: db, AgentReputationRepo: agentreputation.NewRepo(db)}
	h := newAgentReputationHandler(svc)
	if h == nil {
		t.Fatal("newAgentReputationHandler returned nil")
	}
	return h, mock, func() { _ = db.Close() }
}

func TestAgentReputationFundStats_Unauthenticated(t *testing.T) {
	h, _, cleanup := newAgentReputationHandlerEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/api/funds/f1/agent-reputation/stats", nil)
	req.SetPathValue("fundId", "f1")
	rr := httptest.NewRecorder()
	h.handleListFundStats(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAgentReputationFundStats_MissingFundID(t *testing.T) {
	h, _, cleanup := newAgentReputationHandlerEnv(t)
	defer cleanup()
	req := authReq(http.MethodGet, "/api/funds//agent-reputation/stats", "", "user-1")
	req.SetPathValue("fundId", "")
	rr := httptest.NewRecorder()
	h.handleListFundStats(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAgentReputationFundStats_Happy(t *testing.T) {
	h, mock, cleanup := newAgentReputationHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipAnalyst(mock, fundID, companyID, userID)
	now := time.Now()
	mock.ExpectQuery("FROM agent_reputation_stats").
		WillReturnRows(sqlmock.NewRows([]string{
			"fund_id", "agent_id", "agent_name", "agent_kind", "category",
			"decisions_count", "hits_count", "misses_count",
			"avg_alpha", "sum_alpha", "avg_confidence",
			"last_decision_at", "updated_at",
		}).AddRow(fundID, "fund_analyst", "F", "analyst", "fundamentals",
			int64(4), int64(3), int64(1), 0.02, 0.08, 60.0, now, now))
	req := authReq(http.MethodGet, "/api/funds/"+fundID+"/agent-reputation/stats", "", userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleListFundStats(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Stats []agentReputationStatsWire `json:"stats"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Stats) != 1 || body.Stats[0].AgentID != "fund_analyst" {
		t.Errorf("got %+v", body.Stats)
	}
	if body.Stats[0].HitRate <= 0 {
		t.Errorf("hit_rate = %v", body.Stats[0].HitRate)
	}
}

func TestAgentReputationFundOutcomes_Happy(t *testing.T) {
	h, mock, cleanup := newAgentReputationHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipAnalyst(mock, fundID, companyID, userID)
	now := time.Now()
	mock.ExpectQuery("FROM agent_reputation_outcomes").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "agent_id", "agent_name", "agent_kind", "category", "symbol", "asof",
			"direction", "confidence", "realised_return", "benchmark_return", "alpha",
			"horizon_days", "source_panel_id", "source_debate_id", "note", "created_at",
		}).AddRow("o1", fundID, "fund_analyst", "F", "analyst", "fundamentals", "AAPL", now,
			"bullish", 65, 0.04, 0.01, 0.03, 5, nil, nil, "", now))
	req := authReq(http.MethodGet, "/api/funds/"+fundID+"/agent-reputation/outcomes?symbol=aapl", "", userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleListFundOutcomes(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Outcomes []agentReputationOutcomeWire `json:"outcomes"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Outcomes) != 1 || body.Outcomes[0].Alpha != 0.03 {
		t.Errorf("got %+v", body.Outcomes)
	}
}
