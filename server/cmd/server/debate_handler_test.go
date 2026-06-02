// debate_handler_test.go — covers the S8.2 per-fund Bull/Bear
// debate REST endpoints. Reuses the stub panel from
// analyst_panel_handler_test.go so the handler tests stay
// focused on wiring (auth, ownership, persistence, decoding).

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/agent"
	"github.com/fundai/server/internal/analystreport"
	"github.com/fundai/server/internal/debaterepo"
)

// stubDebate returns a Debate orchestrator wired to the standard
// stub bull / bear with nil LLM and 1 round (2 arguments).
func stubDebate() *agent.Debate {
	bull := agent.NewBullResearcher("bull-x", "Bull-Stub", "fund-x", nil,
		agent.WithAdvocateClock(func() time.Time { return time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC) }))
	bear := agent.NewBearResearcher("bear-x", "Bear-Stub", "fund-x", nil,
		agent.WithAdvocateClock(func() time.Time { return time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC) }))
	return agent.NewDebate(bull, bear, agent.DebateConfig{MaxRounds: 1},
		agent.WithDebateClock(func() time.Time { return time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC) }))
}

// newDebateHandlerEnv constructs a debateHandler + sqlmock
// backed Services for the test cases below. Both providers
// return stubs so we never touch real LLM credentials.
func newDebateHandlerEnv(t *testing.T) (*debateHandler, sqlmock.Sqlmock, *sql.DB, AnalystPanelProvider, DebateProvider, func()) {
	t.Helper()
	db, mock := newMockDB(t)
	svc := &Services{
		DB:                db,
		AnalystReportRepo: analystreport.NewRepo(db),
		DebateRepo:        debaterepo.NewRepo(db),
	}
	panelProv := func(_ string) *agent.AnalystPanel { return stubPanel() }
	debateProv := func(_ string) *agent.Debate { return stubDebate() }
	h := newDebateHandler(svc)
	if h == nil {
		t.Fatal("newDebateHandler returned nil")
	}
	return h, mock, db, panelProv, debateProv, func() { _ = db.Close() }
}

func expectFundOwnershipDebate(mock sqlmock.Sqlmock, fundID, companyID, userID string) {
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1`)).
		WithArgs(fundID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "company_id", "name", "description", "trading_mode",
			"initial_capital", "current_capital", "total_assets", "nav", "status",
			"config", "created_at", "updated_at",
		}).AddRow(fundID, companyID, "Fund", "", "simulation",
			100000.0, 100000.0, 100000.0, 1.0, "active",
			[]byte("{}"), now, now,
		))
	mock.ExpectQuery("FROM fund_companies").
		WithArgs(companyID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "owner_user_id", "name", "description", "created_at", "updated_at",
		}).AddRow(companyID, userID, "Co", "", now, now))
}

// expectPanelPersist mirrors the analyst-handler expectation
// helper; the debate handler always persists the panel so the
// child arguments can FK back to it.
func expectPanelPersist(mock sqlmock.Sqlmock) {
	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO analyst_panel_reports").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("panel-uuid"))
	mock.ExpectPrepare("INSERT INTO analyst_reports")
	for i := 0; i < 4; i++ {
		mock.ExpectExec("INSERT INTO analyst_reports").
			WillReturnResult(sqlmock.NewResult(1, 1))
	}
	mock.ExpectCommit()
}

func expectDebatePersist(mock sqlmock.Sqlmock, argRowCount int) {
	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO debate_transcripts").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("trans-uuid"))
	mock.ExpectPrepare("INSERT INTO debate_arguments")
	for i := 0; i < argRowCount; i++ {
		mock.ExpectExec("INSERT INTO debate_arguments").
			WillReturnResult(sqlmock.NewResult(1, 1))
	}
	mock.ExpectCommit()
}

// --- Run endpoint ---------------------------------------------------------

func TestDebate_Run_Unauthenticated(t *testing.T) {
	h, _, _, panelProv, debateProv, cleanup := newDebateHandlerEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodPost, "/api/funds/f1/debates/run", nil)
	req.SetPathValue("fundId", "f1")
	rr := httptest.NewRecorder()
	h.handleRun(rr, req, panelProv, debateProv)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestDebate_Run_MissingSymbol(t *testing.T) {
	h, mock, _, panelProv, debateProv, cleanup := newDebateHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipDebate(mock, fundID, companyID, userID)
	req := authReq(http.MethodPost, "/api/funds/"+fundID+"/debates/run", `{}`, userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleRun(rr, req, panelProv, debateProv)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestDebate_Run_NoProvider_ServiceUnavailable(t *testing.T) {
	h, mock, _, _, _, cleanup := newDebateHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipDebate(mock, fundID, companyID, userID)
	body := `{"symbol":"AAPL"}`
	req := authReq(http.MethodPost, "/api/funds/"+fundID+"/debates/run", body, userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleRun(rr, req, nil, nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

func TestDebate_Run_HappyPath(t *testing.T) {
	h, mock, _, panelProv, debateProv, cleanup := newDebateHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipDebate(mock, fundID, companyID, userID)
	expectPanelPersist(mock)
	// 1 round × 2 stances = 2 argument inserts.
	expectDebatePersist(mock, 2)

	body := `{"symbol":"aapl","price_last":100,"price_change":0.012}`
	req := authReq(http.MethodPost, "/api/funds/"+fundID+"/debates/run", body, userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleRun(rr, req, panelProv, debateProv)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Debate debateTranscriptWire `json:"debate"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Debate.Symbol != "AAPL" {
		t.Errorf("symbol = %q, want AAPL (uppercased)", got.Debate.Symbol)
	}
	if got.Debate.ID != "trans-uuid" || got.Debate.PanelID != "panel-uuid" {
		t.Errorf("ids wrong: %+v", got.Debate)
	}
	if len(got.Debate.Arguments) != 2 {
		t.Errorf("arguments = %d, want 2 (1 round × 2 stances)", len(got.Debate.Arguments))
	}
	stances := []string{got.Debate.Arguments[0].Stance, got.Debate.Arguments[1].Stance}
	if stances[0] != "bull" || stances[1] != "bear" {
		t.Errorf("stances = %v, want [bull bear]", stances)
	}
	if got.Debate.Verdict.Direction == "" {
		t.Errorf("verdict direction empty")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// --- List + Get endpoints --------------------------------------------------

func TestDebate_List_HappyPath(t *testing.T) {
	h, mock, _, _, _, cleanup := newDebateHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipDebate(mock, fundID, companyID, userID)
	now := time.Now()
	mock.ExpectQuery("FROM debate_transcripts").
		WithArgs(fundID, 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "panel_id", "symbol", "asof", "generated_at",
			"verdict_direction", "verdict_confidence", "verdict_winner",
			"verdict_bull_confidence", "verdict_bear_confidence",
			"verdict_contested", "verdict_winning_summary", "verdict_losing_summary",
			"created_at",
		}).AddRow("t-1", fundID, "p-1", "AAPL", now, now,
			"bullish", 65, "bull", 70, 60, false, "ws", "ls", now))

	req := authReq(http.MethodGet, "/api/funds/"+fundID+"/debates", "", userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleList(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Debates []debateTranscriptWire `json:"debates"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if len(got.Debates) != 1 || got.Debates[0].ID != "t-1" {
		t.Errorf("debates = %+v", got.Debates)
	}
}

func TestDebate_Get_HappyPath(t *testing.T) {
	h, mock, _, _, _, cleanup := newDebateHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipDebate(mock, fundID, companyID, userID)
	now := time.Now()
	mock.ExpectQuery("FROM debate_transcripts\\s*WHERE id").
		WithArgs("t-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "panel_id", "symbol", "asof", "generated_at",
			"verdict_direction", "verdict_confidence", "verdict_winner",
			"verdict_bull_confidence", "verdict_bear_confidence",
			"verdict_contested", "verdict_winning_summary", "verdict_losing_summary",
			"created_at",
		}).AddRow("t-1", fundID, "p-1", "AAPL", now, now,
			"bullish", 60, "bull", 70, 60, false, "ws", "ls", now))
	mock.ExpectQuery("FROM debate_arguments\\s*WHERE transcript_id IN").
		WithArgs("t-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "transcript_id", "fund_id", "agent_id", "agent_name", "stance",
			"symbol", "round_number", "asof", "generated_at",
			"direction", "confidence", "thesis",
			"support_points", "rebuttals", "cited_reports", "llm_model", "created_at",
		}).AddRow("a-1", "t-1", fundID, "bull-1", "Bull", "bull",
			"AAPL", 1, now, now, "bullish", 70, "thesis",
			[]byte(`["sp"]`), []byte(`[]`), []byte(`["fundamentals"]`), "fallback", now))

	req := authReq(http.MethodGet, "/api/funds/"+fundID+"/debates/t-1", "", userID)
	req.SetPathValue("fundId", fundID)
	req.SetPathValue("debateId", "t-1")
	rr := httptest.NewRecorder()
	h.handleGet(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Debate debateTranscriptWire `json:"debate"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Debate.ID != "t-1" || len(got.Debate.Arguments) != 1 {
		t.Errorf("debate = %+v", got.Debate)
	}
}

func TestDebate_Get_CrossFund404(t *testing.T) {
	h, mock, _, _, _, cleanup := newDebateHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipDebate(mock, fundID, companyID, userID)
	now := time.Now()
	// Transcript belongs to a DIFFERENT fund.
	mock.ExpectQuery("FROM debate_transcripts\\s*WHERE id").
		WithArgs("t-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "panel_id", "symbol", "asof", "generated_at",
			"verdict_direction", "verdict_confidence", "verdict_winner",
			"verdict_bull_confidence", "verdict_bear_confidence",
			"verdict_contested", "verdict_winning_summary", "verdict_losing_summary",
			"created_at",
		}).AddRow("t-1", "other-fund", "p-1", "AAPL", now, now,
			"bullish", 60, "bull", 70, 60, false, "ws", "ls", now))
	mock.ExpectQuery("FROM debate_arguments\\s*WHERE transcript_id IN").
		WithArgs("t-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "transcript_id", "fund_id", "agent_id", "agent_name", "stance",
			"symbol", "round_number", "asof", "generated_at",
			"direction", "confidence", "thesis",
			"support_points", "rebuttals", "cited_reports", "llm_model", "created_at",
		}))

	req := authReq(http.MethodGet, "/api/funds/"+fundID+"/debates/t-1", "", userID)
	req.SetPathValue("fundId", fundID)
	req.SetPathValue("debateId", "t-1")
	rr := httptest.NewRecorder()
	h.handleGet(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for cross-fund access", rr.Code)
	}
}

// Compile-time use of context so 'context' import is not unused.
var _ = context.Background
