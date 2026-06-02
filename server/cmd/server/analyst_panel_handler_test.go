// analyst_panel_handler_test.go — covers the S8.1 per-fund
// analyst panel REST endpoints. Repo / engine internals are
// exercised in their own packages; here we focus on the wiring
// (auth, fund ownership, request decoding, persist-or-not).

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
)

// --- Stub analyst for the wiring tests ------------------------------------

// stubPanelAnalyst is a deterministic analyst used by the
// handler tests to avoid pulling in the production scoring code.
type stubPanelAnalyst struct {
	cat        agent.AnalystCategory
	direction  agent.Direction
	confidence int
}

func (s *stubPanelAnalyst) ID() string                    { return string(s.cat) + "-stub" }
func (s *stubPanelAnalyst) Name() string                  { return string(s.cat) + "-bot" }
func (s *stubPanelAnalyst) Category() agent.AnalystCategory { return s.cat }
func (s *stubPanelAnalyst) Persona() string               { return "" }
func (s *stubPanelAnalyst) Analyze(_ context.Context, in agent.AnalystInput) (agent.AnalystReport, error) {
	return agent.AnalystReport{
		AgentID:     s.ID(),
		AgentName:   s.Name(),
		Category:    s.cat,
		Symbol:      in.Symbol,
		AsOf:        in.AsOf,
		GeneratedAt: in.AsOf,
		Direction:   s.direction,
		Confidence:  s.confidence,
		Thesis:      "stub thesis",
		KeyFindings: []string{"finding"},
		LLMModel:    "fallback",
	}, nil
}

func stubPanel() *agent.AnalystPanel {
	stubs := []agent.AnalystAgent{
		&stubPanelAnalyst{cat: agent.CategoryFundamentals, direction: agent.DirectionBullish, confidence: 70},
		&stubPanelAnalyst{cat: agent.CategorySentiment, direction: agent.DirectionBullish, confidence: 60},
		&stubPanelAnalyst{cat: agent.CategoryNews, direction: agent.DirectionBullish, confidence: 50},
		&stubPanelAnalyst{cat: agent.CategoryTechnical, direction: agent.DirectionBullish, confidence: 65},
	}
	return agent.NewAnalystPanel("fund-x", stubs, agent.WithPanelSerial())
}

// --- Env helpers ----------------------------------------------------------

func newAnalystHandlerEnv(t *testing.T) (*analystPanelHandler, sqlmock.Sqlmock, *sql.DB, AnalystPanelProvider, func()) {
	t.Helper()
	db, mock := newMockDB(t)
	svc := &Services{DB: db, AnalystReportRepo: analystreport.NewRepo(db)}
	provider := func(_ string) *agent.AnalystPanel { return stubPanel() }
	h := newAnalystPanelHandler(svc, provider)
	if h == nil {
		t.Fatal("newAnalystPanelHandler returned nil")
	}
	return h, mock, db, provider, func() { _ = db.Close() }
}

func expectFundOwnershipAnalyst(mock sqlmock.Sqlmock, fundID, companyID, userID string) {
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

// --- Run endpoint ---------------------------------------------------------

func TestAnalystPanel_Run_Unauthenticated(t *testing.T) {
	h, _, _, provider, cleanup := newAnalystHandlerEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodPost, "/api/funds/f1/analysts/run", nil)
	req.SetPathValue("fundId", "f1")
	rr := httptest.NewRecorder()
	h.handleRun(rr, req, provider)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d", rr.Code)
	}
}

func TestAnalystPanel_Run_MissingSymbol(t *testing.T) {
	h, mock, _, provider, cleanup := newAnalystHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipAnalyst(mock, fundID, companyID, userID)
	req := authReq(http.MethodPost, "/api/funds/"+fundID+"/analysts/run", `{}`, userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleRun(rr, req, provider)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestAnalystPanel_Run_NoProvider_ServiceUnavailable(t *testing.T) {
	h, mock, _, _, cleanup := newAnalystHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipAnalyst(mock, fundID, companyID, userID)
	body := `{"symbol":"AAPL","price_last":100,"price_change":0.01}`
	req := authReq(http.MethodPost, "/api/funds/"+fundID+"/analysts/run", body, userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleRun(rr, req, nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}

func TestAnalystPanel_Run_HappyPath_NoPersist(t *testing.T) {
	h, mock, _, provider, cleanup := newAnalystHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipAnalyst(mock, fundID, companyID, userID)
	body := `{"symbol":"aapl","asset_class":"equity","market":"us","price_last":100,"price_change":0.012,"persist":false}`
	req := authReq(http.MethodPost, "/api/funds/"+fundID+"/analysts/run", body, userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleRun(rr, req, provider)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Panel analystPanelWire `json:"panel"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Panel.Symbol != "AAPL" {
		t.Errorf("symbol = %q, want AAPL (uppercased)", got.Panel.Symbol)
	}
	if got.Panel.AggregateDirection != "bullish" {
		t.Errorf("aggregate = %q", got.Panel.AggregateDirection)
	}
	if len(got.Panel.Reports) != 4 {
		t.Errorf("reports = %d, want 4", len(got.Panel.Reports))
	}
	if got.Panel.ID != "" {
		t.Errorf("expected no ID when persist=false, got %q", got.Panel.ID)
	}
}

func TestAnalystPanel_Run_HappyPath_WithPersist(t *testing.T) {
	h, mock, _, provider, cleanup := newAnalystHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipAnalyst(mock, fundID, companyID, userID)

	// SavePanel transaction.
	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO analyst_panel_reports").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("panel-uuid"))
	mock.ExpectPrepare("INSERT INTO analyst_reports")
	for i := 0; i < 4; i++ {
		mock.ExpectExec("INSERT INTO analyst_reports").
			WillReturnResult(sqlmock.NewResult(1, 1))
	}
	mock.ExpectCommit()

	body := `{"symbol":"AAPL","price_last":100,"price_change":0.01,"persist":true}`
	req := authReq(http.MethodPost, "/api/funds/"+fundID+"/analysts/run", body, userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleRun(rr, req, provider)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Panel analystPanelWire `json:"panel"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Panel.ID != "panel-uuid" {
		t.Errorf("panel.id = %q, want panel-uuid", got.Panel.ID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// --- List endpoint --------------------------------------------------------

func TestAnalystPanel_ListPanels_HappyPath(t *testing.T) {
	h, mock, _, _, cleanup := newAnalystHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipAnalyst(mock, fundID, companyID, userID)
	now := time.Now()
	mock.ExpectQuery("FROM analyst_panel_reports").
		WithArgs(fundID, 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "symbol", "asof", "generated_at",
			"aggregate_direction", "aggregate_confidence", "categories_voted",
			"per_category_votes", "created_at",
		}).AddRow("p-1", fundID, "AAPL", now, now,
			"bullish", 65, 3, []byte(`{}`), now))

	req := authReq(http.MethodGet, "/api/funds/"+fundID+"/analysts/panels", "", userID)
	req.SetPathValue("fundId", fundID)
	rr := httptest.NewRecorder()
	h.handleListPanels(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Panels []analystPanelWire `json:"panels"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if len(got.Panels) != 1 || got.Panels[0].ID != "p-1" {
		t.Errorf("panels = %+v", got.Panels)
	}
}

func TestAnalystPanel_ListPanels_Unauthenticated(t *testing.T) {
	h, _, _, _, cleanup := newAnalystHandlerEnv(t)
	defer cleanup()
	req := httptest.NewRequest(http.MethodGet, "/api/funds/f1/analysts/panels", nil)
	req.SetPathValue("fundId", "f1")
	rr := httptest.NewRecorder()
	h.handleListPanels(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d", rr.Code)
	}
}

// --- Get endpoint ---------------------------------------------------------

func TestAnalystPanel_GetPanel_HappyPath(t *testing.T) {
	h, mock, _, _, cleanup := newAnalystHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipAnalyst(mock, fundID, companyID, userID)
	now := time.Now()
	mock.ExpectQuery("FROM analyst_panel_reports\\s*WHERE id").
		WithArgs("p-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "symbol", "asof", "generated_at",
			"aggregate_direction", "aggregate_confidence", "categories_voted",
			"per_category_votes", "created_at",
		}).AddRow("p-1", fundID, "AAPL", now, now,
			"bullish", 65, 3, []byte(`{}`), now))
	mock.ExpectQuery("FROM analyst_reports\\s*WHERE panel_id IN").
		WithArgs("p-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "panel_id", "fund_id", "agent_id", "agent_name", "category",
			"symbol", "asof", "generated_at",
			"direction", "confidence", "thesis",
			"key_findings", "risks", "data_points", "sources",
			"prompt_tokens", "completion_tokens", "llm_model", "created_at",
		}).AddRow("r-1", "p-1", fundID, "a1", "Bot", "fundamentals",
			"AAPL", now, now,
			"bullish", 70, "thesis text",
			[]byte(`["finding"]`), []byte(`[]`), []byte(`[]`), []byte(`[]`),
			0, 0, "fallback", now))

	req := authReq(http.MethodGet, "/api/funds/"+fundID+"/analysts/panels/p-1", "", userID)
	req.SetPathValue("fundId", fundID)
	req.SetPathValue("panelId", "p-1")
	rr := httptest.NewRecorder()
	h.handleGetPanel(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Panel analystPanelWire `json:"panel"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Panel.ID != "p-1" || len(got.Panel.Reports) != 1 {
		t.Errorf("got panel %+v", got.Panel)
	}
}

func TestAnalystPanel_GetPanel_NotFound(t *testing.T) {
	h, mock, _, _, cleanup := newAnalystHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundID = "22222222-2222-2222-2222-222222222222"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipAnalyst(mock, fundID, companyID, userID)
	mock.ExpectQuery("FROM analyst_panel_reports\\s*WHERE id").
		WithArgs("p-nope").
		WillReturnError(sql.ErrNoRows)

	req := authReq(http.MethodGet, "/api/funds/"+fundID+"/analysts/panels/p-nope", "", userID)
	req.SetPathValue("fundId", fundID)
	req.SetPathValue("panelId", "p-nope")
	rr := httptest.NewRecorder()
	h.handleGetPanel(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestAnalystPanel_GetPanel_CrossFundForbidden(t *testing.T) {
	// A panel from fund A cannot be fetched via fund B's URL,
	// even when the URL fund-id passes ownership for B.
	h, mock, _, _, cleanup := newAnalystHandlerEnv(t)
	defer cleanup()
	const userID = "11111111-1111-1111-1111-111111111111"
	const fundB = "22222222-2222-2222-2222-222222222222"
	const fundA = "44444444-4444-4444-4444-444444444444"
	const companyID = "33333333-3333-3333-3333-333333333333"
	expectFundOwnershipAnalyst(mock, fundB, companyID, userID)
	now := time.Now()
	mock.ExpectQuery("FROM analyst_panel_reports\\s*WHERE id").
		WithArgs("p-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "symbol", "asof", "generated_at",
			"aggregate_direction", "aggregate_confidence", "categories_voted",
			"per_category_votes", "created_at",
		}).AddRow("p-1", fundA, "AAPL", now, now, "bullish", 65, 3, []byte(`{}`), now))
	mock.ExpectQuery("FROM analyst_reports\\s*WHERE panel_id IN").
		WithArgs("p-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "panel_id", "fund_id", "agent_id", "agent_name", "category",
			"symbol", "asof", "generated_at",
			"direction", "confidence", "thesis",
			"key_findings", "risks", "data_points", "sources",
			"prompt_tokens", "completion_tokens", "llm_model", "created_at",
		}))

	req := authReq(http.MethodGet, "/api/funds/"+fundB+"/analysts/panels/p-1", "", userID)
	req.SetPathValue("fundId", fundB)
	req.SetPathValue("panelId", "p-1")
	rr := httptest.NewRecorder()
	h.handleGetPanel(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (cross-fund leak)", rr.Code)
	}
}
