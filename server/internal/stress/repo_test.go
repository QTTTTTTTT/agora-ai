package stress

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newMockRepo(t *testing.T) (*Repo, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return NewRepo(db), mock, func() { _ = db.Close() }
}

func TestRepo_GetScenario_HappyPath(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	now := time.Now()
	mock.ExpectQuery("FROM stress_scenarios").
		WithArgs("scen-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "name", "category", "description", "shocks", "created_by", "created_at", "updated_at",
		}).AddRow("scen-1", "2008 Lehman", "historical", "desc",
			[]byte(`[{"target_type":"asset_class","target_key":"equity","value":-0.4}]`),
			nil, now, now))
	s, err := r.GetScenario(context.Background(), "scen-1")
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "2008 Lehman" || s.Category != CategoryHistorical {
		t.Errorf("got %+v", s)
	}
	if len(s.Shocks) != 1 || s.Shocks[0].TargetType != TargetAssetClass {
		t.Errorf("shocks = %+v", s.Shocks)
	}
}

func TestRepo_GetScenario_NotFound(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	mock.ExpectQuery("FROM stress_scenarios").
		WillReturnError(errors.New("sql: no rows in result set"))
	_, err := r.GetScenario(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRepo_ListScenarios_AllAndFiltered(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	now := time.Now()
	// No category filter
	mock.ExpectQuery("FROM stress_scenarios").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "name", "category", "description", "shocks", "created_by", "created_at", "updated_at",
		}).
			AddRow("s1", "Lehman", "historical", "", []byte(`[]`), nil, now, now).
			AddRow("s2", "Hypothetical recession", "hypothetical", "", []byte(`[]`), nil, now, now))
	all, err := r.ListScenarios(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("len = %d", len(all))
	}
	// Filter by category
	mock.ExpectQuery("FROM stress_scenarios").
		WithArgs("historical").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "name", "category", "description", "shocks", "created_by", "created_at", "updated_at",
		}).AddRow("s1", "Lehman", "historical", "", []byte(`[]`), nil, now, now))
	filt, err := r.ListScenarios(context.Background(), CategoryHistorical)
	if err != nil {
		t.Fatal(err)
	}
	if len(filt) != 1 || filt[0].Category != CategoryHistorical {
		t.Errorf("filtered list wrong: %+v", filt)
	}
}

func TestRepo_UpsertScenario_RejectsBadShock(t *testing.T) {
	r, _, done := newMockRepo(t)
	defer done()
	_, err := r.UpsertScenario(context.Background(), Scenario{
		Name:     "bad",
		Category: CategoryHistorical,
		Shocks:   []Shock{{TargetType: "bogus", TargetKey: "k", Value: 0}},
	}, "")
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestRepo_UpsertScenario_HappyPath(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	now := time.Now()
	mock.ExpectQuery("INSERT INTO stress_scenarios").
		WithArgs("Lehman", "historical", "", sqlmock.AnyArg(), nil).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "name", "category", "description", "shocks", "created_by", "created_at", "updated_at",
		}).AddRow("s1", "Lehman", "historical", "",
			[]byte(`[{"target_type":"wildcard","target_key":"*","value":-0.4}]`),
			nil, now, now))
	out, err := r.UpsertScenario(context.Background(), Scenario{
		Name:     "Lehman",
		Category: CategoryHistorical,
		Shocks:   []Shock{{TargetType: TargetWildcard, TargetKey: "*", Value: -0.4}},
	}, "")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if out.ID != "s1" {
		t.Errorf("got %+v", out)
	}
}

func TestRepo_DeleteScenario_NotFound(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	mock.ExpectExec("DELETE FROM stress_scenarios").
		WillReturnResult(sqlmock.NewResult(0, 0))
	err := r.DeleteScenario(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestRepo_AppendResult_HappyPath(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	mock.ExpectExec("INSERT INTO portfolio_stress_results").
		WillReturnResult(sqlmock.NewResult(1, 1))
	err := r.AppendResult(context.Background(), Result{
		FundID:        "f1",
		ScenarioID:    "s1",
		GeneratedAt:   time.Now(),
		NAVBefore:     100000,
		NAVAfter:      80000,
		PnLTotal:      -20000,
		PnLPct:        -0.20,
		HoldingCount:  3,
		ShockedCount:  3,
		Impacts:       []HoldingImpact{{InstrumentKey: "x", PnL: -1000}},
	})
	if err != nil {
		t.Fatalf("AppendResult: %v", err)
	}
}

func TestRepo_AppendResult_RejectsMissingIds(t *testing.T) {
	r, _, done := newMockRepo(t)
	defer done()
	if err := r.AppendResult(context.Background(), Result{ScenarioID: "s1"}); err == nil {
		t.Error("expected error for missing fund_id")
	}
	if err := r.AppendResult(context.Background(), Result{FundID: "f1"}); err == nil {
		t.Error("expected error for missing scenario_id")
	}
}

func TestRepo_ListResults_HappyPath(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	now := time.Now()
	mock.ExpectQuery("FROM portfolio_stress_results").
		WithArgs("f1", 90).
		WillReturnRows(sqlmock.NewRows([]string{
			"fund_id", "scenario_id", "calculated_at",
			"nav_before", "nav_after", "pnl_total", "pnl_pct",
			"holding_count", "shocked_count", "impacts",
		}).AddRow("f1", "s1", now,
			100000.0, 80000.0, -20000.0, -0.20,
			3, 3, []byte(`[{"instrument_key":"x","pnl":-1000}]`)))
	out, err := r.ListResults(context.Background(), ListResultsParams{FundID: "f1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].PnLPct != -0.20 {
		t.Errorf("got %+v", out)
	}
}
