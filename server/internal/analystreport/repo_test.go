package analystreport

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/agent"
)

func newMockRepo(t *testing.T) (*Repo, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return NewRepo(db), mock, func() { _ = db.Close() }
}

// makeValidPanelReport returns a minimal-but-valid agent.PanelReport
// suitable for repo persistence tests.
func makeValidPanelReport(symbol, fundID string, asof time.Time) agent.PanelReport {
	makeRep := func(cat agent.AnalystCategory, dir agent.Direction, conf int) agent.AnalystReport {
		return agent.AnalystReport{
			AgentID:     string(cat) + "-1",
			AgentName:   string(cat) + "-bot",
			Category:    cat,
			Symbol:      symbol,
			AsOf:        asof,
			GeneratedAt: asof,
			Direction:   dir,
			Confidence:  conf,
			Thesis:      string(cat) + " thesis",
			KeyFindings: []string{"finding-1"},
			LLMModel:    "fallback",
		}
	}
	return agent.PanelReport{
		Symbol:      symbol,
		FundID:      fundID,
		AsOf:        asof,
		GeneratedAt: asof,
		Reports: map[agent.AnalystCategory]agent.AnalystReport{
			agent.CategoryFundamentals: makeRep(agent.CategoryFundamentals, agent.DirectionBullish, 70),
			agent.CategorySentiment:    makeRep(agent.CategorySentiment, agent.DirectionBullish, 60),
		},
		Aggregate: agent.AggregateView{
			Direction:        agent.DirectionBullish,
			Confidence:       65,
			CategoriesVoted:  2,
			PerCategoryVotes: map[agent.AnalystCategory]int{agent.CategoryFundamentals: 1, agent.CategorySentiment: 1},
		},
	}
}

func TestSavePanel_HappyPath(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	asof := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
	panel := makeValidPanelReport("AAPL", "f-1", asof)

	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO analyst_panel_reports").
		WithArgs("f-1", "AAPL", asof, asof, "bullish", 65, 2, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("panel-uuid-1"))
	mock.ExpectPrepare("INSERT INTO analyst_reports")
	// Two reports → two ExecContext calls. Order is map-iteration so
	// we use AnyArg for everything except panel_id + fund_id.
	mock.ExpectExec("INSERT INTO analyst_reports").
		WithArgs(
			"panel-uuid-1", "f-1",
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			"AAPL", asof, asof,
			"bullish", sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			0, 0, "fallback",
		).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO analyst_reports").
		WithArgs(
			"panel-uuid-1", "f-1",
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			"AAPL", asof, asof,
			"bullish", sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			0, 0, "fallback",
		).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	id, err := r.SavePanel(context.Background(), panel)
	if err != nil {
		t.Fatal(err)
	}
	if id != "panel-uuid-1" {
		t.Errorf("id = %q, want panel-uuid-1", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSavePanel_RollsBackOnError(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	asof := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
	panel := makeValidPanelReport("AAPL", "f-1", asof)

	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO analyst_panel_reports").
		WillReturnError(errors.New("DB on fire"))
	mock.ExpectRollback()

	if _, err := r.SavePanel(context.Background(), panel); err == nil {
		t.Error("expected error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSavePanel_RejectsInvalidPanel(t *testing.T) {
	r, _, done := newMockRepo(t)
	defer done()
	bad := agent.PanelReport{} // missing Symbol + Reports
	if _, err := r.SavePanel(context.Background(), bad); err == nil {
		t.Error("expected error for invalid panel")
	}
}

func TestListPanels_Filters(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	asof := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery("FROM analyst_panel_reports").
		WithArgs("f-1", "AAPL", 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "symbol", "asof", "generated_at",
			"aggregate_direction", "aggregate_confidence", "categories_voted",
			"per_category_votes", "created_at",
		}).AddRow("p-1", "f-1", "AAPL", asof, asof,
			"bullish", 65, 2,
			[]byte(`{"fundamentals":1,"sentiment":1}`), asof))

	got, err := r.ListPanels(context.Background(), ListPanelsParams{
		FundID: "f-1", Symbol: "aapl",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}
	if got[0].PerCategoryVote["fundamentals"] != 1 {
		t.Errorf("votes parsed wrong: %v", got[0].PerCategoryVote)
	}
}

func TestGetPanel_NotFound(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	mock.ExpectQuery("FROM analyst_panel_reports").
		WithArgs("p-nope").
		WillReturnError(errors.New("sql: no rows in result set"))
	if _, err := r.GetPanel(context.Background(), "p-nope"); err == nil {
		t.Error("expected error")
	}
}

func TestGetPanel_WithChildren(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	asof := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery("FROM analyst_panel_reports\\s*WHERE id").
		WithArgs("p-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "symbol", "asof", "generated_at",
			"aggregate_direction", "aggregate_confidence", "categories_voted",
			"per_category_votes", "created_at",
		}).AddRow("p-1", "f-1", "AAPL", asof, asof,
			"bullish", 65, 2,
			[]byte(`{}`), asof))
	mock.ExpectQuery("FROM analyst_reports\\s*WHERE panel_id IN").
		WithArgs("p-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "panel_id", "fund_id", "agent_id", "agent_name", "category",
			"symbol", "asof", "generated_at",
			"direction", "confidence", "thesis",
			"key_findings", "risks", "data_points", "sources",
			"prompt_tokens", "completion_tokens", "llm_model", "created_at",
		}).AddRow("r-1", "p-1", "f-1", "fund-a", "F-Bot", "fundamentals",
			"AAPL", asof, asof,
			"bullish", 70, "thesis-text",
			[]byte(`["a","b"]`), []byte(`[]`), []byte(`[]`), []byte(`[]`),
			0, 0, "fallback", asof))

	got, err := r.GetPanel(context.Background(), "p-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Reports) != 1 || got.Reports[0].Thesis != "thesis-text" {
		t.Errorf("children not parsed: %+v", got.Reports)
	}
	if len(got.Reports[0].KeyFindings) != 2 {
		t.Errorf("KeyFindings JSONB not parsed")
	}
}

func TestListReportsByAgent(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	asof := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery("FROM analyst_reports\\s*WHERE agent_id").
		WithArgs("fund-a", 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "panel_id", "fund_id", "agent_id", "agent_name", "category",
			"symbol", "asof", "generated_at",
			"direction", "confidence", "thesis",
			"key_findings", "risks", "data_points", "sources",
			"prompt_tokens", "completion_tokens", "llm_model", "created_at",
		}).AddRow("r-1", "p-1", "f-1", "fund-a", "F-Bot", "fundamentals",
			"AAPL", asof, asof,
			"bullish", 70, "thesis-text",
			[]byte(`[]`), []byte(`[]`), []byte(`[]`), []byte(`[]`),
			0, 0, "fallback", asof))

	got, err := r.ListReportsByAgent(context.Background(), "fund-a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].AgentID != "fund-a" {
		t.Errorf("unexpected: %+v", got)
	}
}

func TestListReportsByAgent_RejectsEmptyID(t *testing.T) {
	r, _, done := newMockRepo(t)
	defer done()
	if _, err := r.ListReportsByAgent(context.Background(), "  ", 100); err == nil {
		t.Error("expected error for empty agent id")
	}
}
