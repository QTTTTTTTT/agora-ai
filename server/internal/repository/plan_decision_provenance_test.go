package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestPlanRepo_SetDecisionProvenance_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlanRepo(db)

	payload := []byte(`{"promptBlocks":["regime","exposure"],"signalCount":2}`)
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE investment_plans
		    SET decision_provenance = $2,
		        updated_at = NOW()
		  WHERE id = $1`)).
		WithArgs("plan-1", payload).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.SetDecisionProvenance(context.Background(), "plan-1", payload); err != nil {
		t.Fatalf("SetDecisionProvenance: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPlanRepo_SetDecisionProvenance_NoOpOnEmpty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlanRepo(db)

	// No mock UPDATE expected — empty payload is a no-op.
	if err := repo.SetDecisionProvenance(context.Background(), "plan-1", nil); err != nil {
		t.Fatalf("SetDecisionProvenance(nil): %v", err)
	}
	if err := repo.SetDecisionProvenance(context.Background(), "plan-1", []byte{}); err != nil {
		t.Fatalf("SetDecisionProvenance(empty): %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expected no DB calls, got %v", err)
	}
}

func TestPlanRepo_SetDecisionProvenance_RejectsEmptyPlanID(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlanRepo(db)

	payload := []byte(`{"signalCount":1}`)
	if err := repo.SetDecisionProvenance(context.Background(), "", payload); err == nil {
		t.Fatalf("expected error for empty plan id")
	}
}

func TestPlanRepo_GetDecisionProvenance_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlanRepo(db)

	want := `{"promptBlocks":["regime","exposure"],"signalCount":2}`
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT decision_provenance::text
		   FROM investment_plans
		  WHERE id = $1`)).
		WithArgs("plan-1").
		WillReturnRows(sqlmock.NewRows([]string{"decision_provenance"}).AddRow(want))

	got, err := repo.GetDecisionProvenance(context.Background(), "plan-1")
	if err != nil {
		t.Fatalf("GetDecisionProvenance: %v", err)
	}
	if string(got) != want {
		t.Fatalf("payload mismatch: got %q, want %q", string(got), want)
	}
}

func TestPlanRepo_GetDecisionProvenance_NullColumn(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlanRepo(db)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT decision_provenance::text
		   FROM investment_plans
		  WHERE id = $1`)).
		WithArgs("plan-1").
		WillReturnRows(sqlmock.NewRows([]string{"decision_provenance"}).AddRow(nil))

	got, err := repo.GetDecisionProvenance(context.Background(), "plan-1")
	if err != nil {
		t.Fatalf("GetDecisionProvenance: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil bytes for NULL column, got %q", string(got))
	}
}

func TestPlanRepo_GetDecisionProvenance_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlanRepo(db)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT decision_provenance::text
		   FROM investment_plans
		  WHERE id = $1`)).
		WithArgs("plan-missing").
		WillReturnError(errNoRowsFixture())

	_, err = repo.GetDecisionProvenance(context.Background(), "plan-missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
