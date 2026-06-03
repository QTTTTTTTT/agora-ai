package modelab

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/llm"
)

func TestRepoUpdateDraft_RejectsNonDraft(t *testing.T) {
	db, mock := openMock(t)
	defer db.Close()

	mock.ExpectExec(`UPDATE model_ab_experiments\s+SET name`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	// UpdateDraft probes GetExperiment when 0 rows affected.
	armsJSON, _ := MarshalArms(stubArms())
	mock.ExpectQuery(`FROM model_ab_experiments\s+WHERE id`).
		WillReturnRows(experimentRows().AddRow(
			"00000000-0000-0000-0000-000000000010",
			"running exp", "",
			string(ScopeGlobal), "",
			stringArray(),
			armsJSON,
			float64Array(0.5, 0.5),
			string(StatusRunning),
			time.Time{}, time.Time{},
			int64(0), int64(0), "",
			time.Now(), time.Now(),
		))

	repo := NewRepo(db)
	exp := &Experiment{
		Name:         "patched",
		Scope:        ScopeGlobal,
		Arms:         stubArms(),
		TrafficSplit: []float64{0.5, 0.5},
	}
	err := repo.UpdateDraft(context.Background(), "00000000-0000-0000-0000-000000000010", exp)
	if !errors.Is(err, ErrNotEditable) {
		t.Fatalf("expected ErrNotEditable, got %v", err)
	}
}

func TestRepoUpdateDraft_HappyPath(t *testing.T) {
	db, mock := openMock(t)
	defer db.Close()
	mock.ExpectExec(`UPDATE model_ab_experiments\s+SET name`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	repo := NewRepo(db)
	exp := &Experiment{
		Name:         "patched",
		Scope:        ScopeGlobal,
		Arms:         stubArms(),
		TrafficSplit: []float64{0.5, 0.5},
	}
	if err := repo.UpdateDraft(context.Background(), "00000000-0000-0000-0000-000000000020", exp); err != nil {
		t.Fatalf("UpdateDraft: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
}

func TestRepoUpdateDraft_RejectsInvalidShape(t *testing.T) {
	db, _ := openMock(t)
	defer db.Close()
	repo := NewRepo(db)
	bad := &Experiment{Name: "", Scope: ScopeGlobal,
		Arms:         []ArmConfig{{Provider: llm.ProviderOpenAI, ModelName: "x"}},
		TrafficSplit: []float64{1.0},
	}
	if err := repo.UpdateDraft(context.Background(), "00000000-0000-0000-0000-000000000030", bad); err == nil {
		t.Fatalf("expected validation error for empty name + single-arm")
	}
}

func TestRepoClone_DuplicatesAsDraft(t *testing.T) {
	db, mock := openMock(t)
	defer db.Close()

	armsJSON, _ := MarshalArms(stubArms())
	// GetExperiment of source
	mock.ExpectQuery(`FROM model_ab_experiments\s+WHERE id`).
		WillReturnRows(experimentRows().AddRow(
			"00000000-0000-0000-0000-000000000040",
			"source", "",
			string(ScopeAgentRole), "pm",
			stringArray("pm_decision"),
			armsJSON,
			float64Array(0.5, 0.5),
			string(StatusRunning),
			time.Time{}, time.Time{},
			int64(0), int64(0), "",
			time.Now(), time.Now(),
		))
	// CreateExperiment of clone
	mock.ExpectQuery(`INSERT INTO model_ab_experiments`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("00000000-0000-0000-0000-000000000041"))

	repo := NewRepo(db)
	newID, err := repo.Clone(context.Background(), "00000000-0000-0000-0000-000000000040", "", "actor")
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if newID == "" {
		t.Fatalf("expected non-empty new ID")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
}

func TestRepoBulkSetStatus(t *testing.T) {
	db, mock := openMock(t)
	defer db.Close()

	mock.ExpectExec(`UPDATE model_ab_experiments\s+SET status`).
		WithArgs("archived", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 3))

	repo := NewRepo(db)
	n, err := repo.BulkSetStatus(context.Background(),
		[]string{"00000000-0000-0000-0000-000000000050", "00000000-0000-0000-0000-000000000051", "00000000-0000-0000-0000-000000000052"},
		StatusArchived)
	if err != nil {
		t.Fatalf("BulkSetStatus: %v", err)
	}
	if n != 3 {
		t.Fatalf("expected 3 updated rows, got %d", n)
	}
}

func TestRepoBulkSetStatus_EmptyIsNoop(t *testing.T) {
	db, _ := openMock(t)
	defer db.Close()
	repo := NewRepo(db)
	n, err := repo.BulkSetStatus(context.Background(), nil, StatusArchived)
	if err != nil {
		t.Fatalf("empty bulk should not error: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 updates for empty list, got %d", n)
	}
}
