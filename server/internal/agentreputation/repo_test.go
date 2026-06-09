package agentreputation

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

func TestAgentKind_IsValid(t *testing.T) {
	for _, k := range []AgentKind{KindAnalyst, KindAdvocate, KindPM, KindResearcher} {
		if !k.IsValid() {
			t.Errorf("expected %q to be valid", k)
		}
	}
	if AgentKind("bogus").IsValid() {
		t.Error("expected bogus to be invalid")
	}
}

func TestDirection_IsValid(t *testing.T) {
	for _, d := range []Direction{DirBullish, DirBearish, DirNeutral} {
		if !d.IsValid() {
			t.Errorf("expected %q to be valid", d)
		}
	}
	if Direction("sideways").IsValid() {
		t.Error("expected sideways to be invalid")
	}
}

func TestOutcome_Validate(t *testing.T) {
	good := Outcome{
		FundID: "f1", AgentID: "a1", AgentKind: KindAnalyst,
		Direction: DirBullish, Symbol: "AAPL", Confidence: 50,
		HorizonDays: 5, AsOf: time.Now(),
	}
	if err := good.Validate(); err != nil {
		t.Errorf("expected ok, got %v", err)
	}

	bad := good
	bad.FundID = ""
	if err := bad.Validate(); err == nil {
		t.Error("expected FundID error")
	}

	bad = good
	bad.AgentID = ""
	if err := bad.Validate(); err == nil {
		t.Error("expected AgentID error")
	}

	bad = good
	bad.AgentKind = AgentKind("nope")
	if err := bad.Validate(); err == nil {
		t.Error("expected AgentKind error")
	}

	bad = good
	bad.Direction = Direction("sideways")
	if err := bad.Validate(); err == nil {
		t.Error("expected Direction error")
	}

	bad = good
	bad.Symbol = ""
	if err := bad.Validate(); err == nil {
		t.Error("expected Symbol error")
	}

	bad = good
	bad.Confidence = 150
	if err := bad.Validate(); err == nil {
		t.Error("expected Confidence error")
	}

	bad = good
	bad.AsOf = time.Time{}
	if err := bad.Validate(); err == nil {
		t.Error("expected AsOf error")
	}
}

func TestStats_HitRate(t *testing.T) {
	if (Stats{}).HitRate() != 0 {
		t.Error("zero")
	}
	s := Stats{DecisionsCount: 4, HitsCount: 3}
	if got := s.HitRate(); got != 0.75 {
		t.Errorf("want 0.75, got %v", got)
	}
}

func TestRepo_UpsertOutcomes_NilRepo(t *testing.T) {
	var r *Repo
	if err := r.UpsertOutcomes(context.Background(), nil); err == nil {
		t.Error("expected error from nil repo")
	}
}

func TestRepo_UpsertOutcomes_EmptyOK(t *testing.T) {
	r, _, done := newMockRepo(t)
	defer done()
	if err := r.UpsertOutcomes(context.Background(), nil); err != nil {
		t.Errorf("empty batch should be ok, got %v", err)
	}
}

func TestRepo_UpsertOutcomes_ValidationFails(t *testing.T) {
	r, _, done := newMockRepo(t)
	defer done()
	bad := []Outcome{{}}
	if err := r.UpsertOutcomes(context.Background(), bad); err == nil {
		t.Error("expected validation error")
	}
}

func TestRepo_UpsertOutcomes_HappyPath(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	asof := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectPrepare("INSERT INTO agent_reputation_outcomes")
	mock.ExpectExec("INSERT INTO agent_reputation_outcomes").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	err := r.UpsertOutcomes(context.Background(), []Outcome{{
		FundID: "f1", AgentID: "fund_analyst", AgentName: "F",
		AgentKind: KindAnalyst, Category: "fundamentals",
		Symbol: "aapl", AsOf: asof, Direction: DirBullish, Confidence: 60,
		RealisedReturn: 0.03, BenchmarkReturn: 0.01, Alpha: 0.02,
		HorizonDays: 5,
	}})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestRepo_UpsertOutcomes_AdvisorRow(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	asof := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectPrepare("INSERT INTO agent_reputation_outcomes")
	mock.ExpectExec("INSERT INTO agent_reputation_outcomes").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	err := r.UpsertOutcomes(context.Background(), []Outcome{{
		AgentID: "master:buffett", AgentName: "Buffett",
		AgentKind: KindMaster, Category: "master",
		Symbol: "AAPL", AsOf: asof, Direction: DirBuy, Confidence: 70,
		RealisedReturn: 0.05, BenchmarkReturn: 0.01, Alpha: 0.04,
		HorizonDays: 5,
	}})
	if err != nil {
		t.Fatalf("upsert advisor: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestRepo_UpsertOutcomes_RejectsAdvisorRowWithFundID(t *testing.T) {
	r, _, done := newMockRepo(t)
	defer done()
	asof := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	bad := []Outcome{{
		FundID: "f1", AgentID: "master:buffett",
		AgentKind: KindMaster, Symbol: "AAPL", AsOf: asof,
		Direction: DirBuy, Confidence: 70, HorizonDays: 5,
	}}
	if err := r.UpsertOutcomes(context.Background(), bad); err == nil {
		t.Error("expected advisor rows with FundID to fail validation")
	}
}

func TestRepo_RecomputeStats_HappyPath(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	// Single fund-scoped recompute when a fundID is passed —
	// the advisor recompute is skipped.
	mock.ExpectExec("INSERT INTO agent_reputation_stats").
		WithArgs("f1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.RecomputeStats(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
}

func TestRepo_RecomputeStats_AllFunds(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	// Two execs when fundID == "": one for fund rows, then one
	// for advisor rows.
	mock.ExpectExec("INSERT INTO agent_reputation_stats").
		WithArgs(nil).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO agent_reputation_stats").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := r.RecomputeStats(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
}

func TestRepo_ListStats_HappyPath(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	now := time.Now()
	mock.ExpectQuery("FROM agent_reputation_stats").
		WithArgs("f1", "analyst", 25).
		WillReturnRows(sqlmock.NewRows([]string{
			"fund_id", "agent_id", "agent_name", "agent_kind", "category",
			"decisions_count", "hits_count", "misses_count",
			"avg_alpha", "sum_alpha", "avg_confidence",
			"last_decision_at", "updated_at",
		}).AddRow("f1", "fund_analyst", "Fundamentals", "analyst", "fundamentals",
			int64(10), int64(7), int64(3), 0.012, 0.12, 60.0, now, now))
	got, err := r.ListStats(context.Background(), ListStatsParams{
		FundID: "f1", AgentKind: KindAnalyst, Limit: 25,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].AgentID != "fund_analyst" || got[0].HitsCount != 7 {
		t.Errorf("unexpected: %+v", got)
	}
	if got[0].HitRate() < 0.69 || got[0].HitRate() > 0.71 {
		t.Errorf("hit rate %v", got[0].HitRate())
	}
}

func TestRepo_ListOutcomes_HappyPath(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	now := time.Now()
	mock.ExpectQuery("FROM agent_reputation_outcomes").
		WithArgs("f1", "fund_analyst", "AAPL", 50).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "agent_id", "agent_name", "agent_kind", "category", "symbol", "asof",
			"direction", "confidence", "realised_return", "benchmark_return", "alpha",
			"horizon_days", "source_panel_id", "source_debate_id", "note", "created_at",
		}).AddRow("o1", "f1", "fund_analyst", "Fundamentals", "analyst", "fundamentals", "AAPL", now,
			"bullish", 65, 0.04, 0.01, 0.03, 5, nil, nil, "", now))
	got, err := r.ListOutcomes(context.Background(), ListOutcomesParams{
		FundID: "f1", AgentID: "fund_analyst", Symbol: "aapl", Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "o1" || got[0].Alpha != 0.03 {
		t.Errorf("unexpected: %+v", got)
	}
}

func TestRepo_GetStats_NotFound(t *testing.T) {
	r, mock, done := newMockRepo(t)
	defer done()
	mock.ExpectQuery("FROM agent_reputation_stats").
		WithArgs("f1", "missing").
		WillReturnRows(sqlmock.NewRows([]string{
			"fund_id", "agent_id", "agent_name", "agent_kind", "category",
			"decisions_count", "hits_count", "misses_count",
			"avg_alpha", "sum_alpha", "avg_confidence",
			"last_decision_at", "updated_at",
		}))
	if _, err := r.GetStats(context.Background(), "f1", "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}
