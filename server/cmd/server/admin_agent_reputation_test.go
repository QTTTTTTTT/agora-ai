// Admin agent reputation handler tests (S8.4).

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/agentreputation"
)

type stubRebuildSink struct {
	calls   int
	lastFid string
	n       int
	err     error
}

func (s *stubRebuildSink) RebuildForFund(_ context.Context, fundID string) (int, error) {
	s.calls++
	s.lastFid = fundID
	return s.n, s.err
}

func newAdminAgentReputationEnv(t *testing.T) (*adminHandler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	h := &adminHandler{
		db:                  db,
		metrics:             newServerMetrics(),
		agentReputationRepo: agentreputation.NewRepo(db),
	}
	return h, mock, func() { _ = db.Close() }
}

func reputationStatsRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"fund_id", "agent_id", "agent_name", "agent_kind", "category",
		"decisions_count", "hits_count", "misses_count",
		"avg_alpha", "sum_alpha", "avg_confidence",
		"last_decision_at", "updated_at",
	})
}

func TestAdminAgentRep_Stats_Unauthenticated(t *testing.T) {
	h, _, cleanup := newAdminAgentReputationEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/agent-reputation/stats", nil)
	rr := httptest.NewRecorder()
	h.handleAdminListAgentReputationStats(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAdminAgentRep_Stats_Forbidden_NonAdmin(t *testing.T) {
	h, mock, cleanup := newAdminAgentReputationEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "user")
	req := authReq(http.MethodGet, "/api/admin/agent-reputation/stats", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleAdminListAgentReputationStats(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAdminAgentRep_Stats_Happy(t *testing.T) {
	h, mock, cleanup := newAdminAgentReputationEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	now := time.Now()
	mock.ExpectQuery("FROM agent_reputation_stats").
		WillReturnRows(reputationStatsRows().AddRow(
			"f1", "fund_analyst", "Fundamentals", "analyst", "fundamentals",
			int64(8), int64(5), int64(3), 0.011, 0.088, 60.0, now, now,
		))
	req := authReq(http.MethodGet, "/api/admin/agent-reputation/stats?fund_id=f1", "", "u-1")
	rr := httptest.NewRecorder()
	h.handleAdminListAgentReputationStats(rr, req)
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

func TestAdminAgentRep_Rebuild_Forbidden(t *testing.T) {
	h, mock, cleanup := newAdminAgentReputationEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "user")
	req := authReq(http.MethodPost, "/api/admin/agent-reputation/rebuild", `{"fund_id":"f1"}`, "u-1")
	rr := httptest.NewRecorder()
	h.handleAdminRebuildAgentReputation(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAdminAgentRep_Rebuild_503_NoSink(t *testing.T) {
	h, mock, cleanup := newAdminAgentReputationEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	req := authReq(http.MethodPost, "/api/admin/agent-reputation/rebuild", `{"fund_id":"f1"}`, "u-1")
	rr := httptest.NewRecorder()
	h.handleAdminRebuildAgentReputation(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAdminAgentRep_Rebuild_Happy(t *testing.T) {
	h, mock, cleanup := newAdminAgentReputationEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	sink := &stubRebuildSink{n: 7}
	h.agentReputationRebuildSink = sink
	req := authReq(http.MethodPost, "/api/admin/agent-reputation/rebuild", `{"fund_id":"f1"}`, "u-1")
	rr := httptest.NewRecorder()
	h.handleAdminRebuildAgentReputation(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if sink.calls != 1 || sink.lastFid != "f1" {
		t.Errorf("sink not called as expected: %+v", sink)
	}
	var resp adminRebuildAgentReputationResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OutcomesWritten != 7 || resp.Status != "ok" {
		t.Errorf("got %+v", resp)
	}
}

func TestAdminAgentRep_Rebuild_SinkError(t *testing.T) {
	h, mock, cleanup := newAdminAgentReputationEnv(t)
	defer cleanup()
	expectAdminRoleLookup(mock, "u-1", "admin")
	h.agentReputationRebuildSink = &stubRebuildSink{err: errors.New("boom")}
	req := authReq(http.MethodPost, "/api/admin/agent-reputation/rebuild", `{}`, "u-1")
	rr := httptest.NewRecorder()
	h.handleAdminRebuildAgentReputation(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d", rr.Code)
	}
}
