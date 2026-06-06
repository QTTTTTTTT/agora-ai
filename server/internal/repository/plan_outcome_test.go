package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestPlanRepo_SetPlanOutcome_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlanRepo(db)

	payload := []byte(`{"windowKind":"fixed_5d","realizedPnL":100}`)
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE investment_plans
		    SET plan_outcome = $2,
		        updated_at = NOW()
		  WHERE id = $1`)).
		WithArgs("plan-1", payload).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.SetPlanOutcome(context.Background(), "plan-1", payload); err != nil {
		t.Fatalf("SetPlanOutcome: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPlanRepo_SetPlanOutcome_NoOpOnEmpty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlanRepo(db)

	if err := repo.SetPlanOutcome(context.Background(), "plan-1", nil); err != nil {
		t.Fatalf("SetPlanOutcome(nil): %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expected no DB calls for nil payload, got %v", err)
	}
}

func TestPlanRepo_GetPlanOutcome_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlanRepo(db)

	want := `{"windowKind":"fixed_5d","realizedPnL":100}`
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT plan_outcome::text
		   FROM investment_plans
		  WHERE id = $1`)).
		WithArgs("plan-1").
		WillReturnRows(sqlmock.NewRows([]string{"plan_outcome"}).AddRow(want))

	got, err := repo.GetPlanOutcome(context.Background(), "plan-1")
	if err != nil {
		t.Fatalf("GetPlanOutcome: %v", err)
	}
	if string(got) != want {
		t.Fatalf("payload mismatch: got %q, want %q", string(got), want)
	}
}

func TestPlanRepo_GetPlanOutcome_NullColumn(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlanRepo(db)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT plan_outcome::text
		   FROM investment_plans
		  WHERE id = $1`)).
		WithArgs("plan-1").
		WillReturnRows(sqlmock.NewRows([]string{"plan_outcome"}).AddRow(nil))

	got, err := repo.GetPlanOutcome(context.Background(), "plan-1")
	if err != nil {
		t.Fatalf("GetPlanOutcome: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for NULL column, got %q", string(got))
	}
}

func TestPlanRepo_GetPlanOutcome_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlanRepo(db)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT plan_outcome::text
		   FROM investment_plans
		  WHERE id = $1`)).
		WithArgs("plan-missing").
		WillReturnError(errNoRowsFixture())

	_, err = repo.GetPlanOutcome(context.Background(), "plan-missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestPlanRepo_ListPendingOutcomePlans_WithLimit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlanRepo(db)

	cutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id
			   FROM investment_plans
			  WHERE plan_outcome IS NULL
			    AND created_at < $1
			  ORDER BY created_at ASC
			  LIMIT $2`)).
		WithArgs(cutoff, 50).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow("p1").AddRow("p2").AddRow("p3"))

	ids, err := repo.ListPendingOutcomePlans(context.Background(), cutoff, 50)
	if err != nil {
		t.Fatalf("ListPendingOutcomePlans: %v", err)
	}
	if len(ids) != 3 {
		t.Errorf("expected 3 ids, got %d (%v)", len(ids), ids)
	}
}

func TestPlanRepo_ListPendingOutcomePlans_Unbounded(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlanRepo(db)

	cutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id
		   FROM investment_plans
		  WHERE plan_outcome IS NULL
		    AND created_at < $1
		  ORDER BY created_at ASC`)).
		WithArgs(cutoff).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("p1"))

	ids, err := repo.ListPendingOutcomePlans(context.Background(), cutoff, 0)
	if err != nil {
		t.Fatalf("ListPendingOutcomePlans: %v", err)
	}
	if len(ids) != 1 || ids[0] != "p1" {
		t.Errorf("expected [p1], got %v", ids)
	}
}
