package repository

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestPlanRepo_SetDecisionSource_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlanRepo(db)

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE investment_plans
		    SET decision_source = $2,
		        fallback_reason = $3,
		        updated_at = NOW()
		  WHERE id = $1`)).
		WithArgs("plan-1", "llm_pm", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.SetDecisionSource(context.Background(), "plan-1", "llm_pm", nil); err != nil {
		t.Fatalf("SetDecisionSource: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPlanRepo_SetDecisionSource_WithReason(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlanRepo(db)

	reason := []byte(`{"category":"rate_limited","provider":"claude","model":"claude-opus-4"}`)
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE investment_plans
		    SET decision_source = $2,
		        fallback_reason = $3,
		        updated_at = NOW()
		  WHERE id = $1`)).
		WithArgs("plan-2", "fallback_after_llm_error", reason).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.SetDecisionSource(context.Background(), "plan-2", "fallback_after_llm_error", reason); err != nil {
		t.Fatalf("SetDecisionSource: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPlanRepo_SetDecisionSource_RejectsEmptyArgs(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlanRepo(db)

	if err := repo.SetDecisionSource(context.Background(), "", "llm_pm", nil); err == nil {
		t.Fatalf("expected error for empty plan id")
	}
	if err := repo.SetDecisionSource(context.Background(), "plan-x", "", nil); err == nil {
		t.Fatalf("expected error for empty source")
	}
}

func TestPlanRepo_GetDecisionSource_NullReason(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlanRepo(db)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COALESCE(decision_source, 'legacy'),
		        fallback_reason::text
		   FROM investment_plans
		  WHERE id = $1`)).
		WithArgs("plan-3").
		WillReturnRows(sqlmock.NewRows([]string{"decision_source", "fallback_reason"}).
			AddRow("llm_pm", nil))

	source, reason, err := repo.GetDecisionSource(context.Background(), "plan-3")
	if err != nil {
		t.Fatalf("GetDecisionSource: %v", err)
	}
	if source != "llm_pm" {
		t.Fatalf("expected source=llm_pm, got %q", source)
	}
	if reason != nil {
		t.Fatalf("expected nil reason for successful LLM row, got %s", string(reason))
	}
}

func TestPlanRepo_GetDecisionSource_WithReason(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlanRepo(db)

	reasonJSON := `{"category":"service_unavailable","provider":"openai"}`
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COALESCE(decision_source, 'legacy'),
		        fallback_reason::text
		   FROM investment_plans
		  WHERE id = $1`)).
		WithArgs("plan-4").
		WillReturnRows(sqlmock.NewRows([]string{"decision_source", "fallback_reason"}).
			AddRow("fallback_after_llm_error", reasonJSON))

	source, reason, err := repo.GetDecisionSource(context.Background(), "plan-4")
	if err != nil {
		t.Fatalf("GetDecisionSource: %v", err)
	}
	if source != "fallback_after_llm_error" {
		t.Fatalf("expected fallback_after_llm_error, got %q", source)
	}
	if string(reason) != reasonJSON {
		t.Fatalf("expected reason=%q, got %q", reasonJSON, string(reason))
	}
}

func TestPlanRepo_GetDecisionSource_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlanRepo(db)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COALESCE(decision_source, 'legacy'),
		        fallback_reason::text
		   FROM investment_plans
		  WHERE id = $1`)).
		WithArgs("plan-missing").
		WillReturnError(errNoRowsFixture())

	_, _, err = repo.GetDecisionSource(context.Background(), "plan-missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// errNoRowsFixture returns the canonical sql.ErrNoRows so sqlmock can
// surface it from a Query mock — sqlmock recognises it and turns the
// QueryRowContext.Scan into the no-rows error path.
func errNoRowsFixture() error { return sql.ErrNoRows }
