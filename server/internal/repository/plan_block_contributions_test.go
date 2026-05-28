package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// G1 #2b: SetBlockContributions issues a single UPDATE that
// stamps the JSONB payload onto the existing plan row. The
// query is intentionally separate from the INSERT so the
// existing plan-create path (and its tests) is unchanged.

func TestSetBlockContributionsExecutesUpdate(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	repo := NewPlanRepo(db)
	payload := []byte(`{"present":["quantSnapshots"],"cited":["qualityScores"]}`)
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE investment_plans
		    SET block_contributions = $2,
		        updated_at = NOW()
		  WHERE id = $1`)).
		WithArgs("plan-1", payload).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.SetBlockContributions(context.Background(), "plan-1", payload); err != nil {
		t.Fatalf("SetBlockContributions: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestSetBlockContributionsEmptyPayloadIsNoOp(t *testing.T) {
	// Empty / nil payload must NOT issue a DB call — the caller
	// often produces an empty payload when the trace was absent
	// (legacy path took over).
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	// No ExpectExec — any DB call will fail expectations.
	repo := NewPlanRepo(db)
	if err := repo.SetBlockContributions(context.Background(), "plan-1", nil); err != nil {
		t.Errorf("nil payload should not error, got %v", err)
	}
	if err := repo.SetBlockContributions(context.Background(), "plan-1", []byte{}); err != nil {
		t.Errorf("empty payload should not error, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("no DB call expected, got: %v", err)
	}
}

func TestSetBlockContributionsRejectsEmptyPlanID(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlanRepo(db)
	err = repo.SetBlockContributions(context.Background(), "", []byte("{}"))
	if err == nil {
		t.Error("expected error on empty plan id")
	}
}

func TestSetBlockContributionsPropagatesDBError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlanRepo(db)
	wantErr := errors.New("connection refused")
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE investment_plans`)).
		WithArgs("plan-1", []byte("{}")).
		WillReturnError(wantErr)
	err = repo.SetBlockContributions(context.Background(), "plan-1", []byte("{}"))
	if err == nil {
		t.Error("expected error to propagate")
	}
}
