package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestLeaseManagerAcquireWinsWhenRowMissing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	mgr := NewLeaseManager(db, "replica-A", 30*time.Second, 10*time.Second)

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO scheduler_leases")).
		WithArgs("workflow-scheduler", "replica-A", "30").
		WillReturnRows(sqlmock.NewRows([]string{"is_leader"}).AddRow(true))

	got, err := mgr.acquire(context.Background(), "workflow-scheduler")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !got {
		t.Fatalf("expected to win lease, got false")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestLeaseManagerAcquireLosesWhenAnotherHolderActive(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	mgr := NewLeaseManager(db, "replica-B", 30*time.Second, 10*time.Second)

	// When another holder owns a non-expired lease, the WHERE clause
	// in the UPSERT excludes the row from the update, so the statement
	// returns zero rows.
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO scheduler_leases")).
		WithArgs("workflow-scheduler", "replica-B", "30").
		WillReturnError(sqlErrNoRows())

	got, err := mgr.acquire(context.Background(), "workflow-scheduler")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if got {
		t.Fatalf("expected to lose lease, got true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestLeaseManagerAcquirePropagatesError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	mgr := NewLeaseManager(db, "replica-C", 30*time.Second, 10*time.Second)

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO scheduler_leases")).
		WithArgs("workflow-scheduler", "replica-C", "30").
		WillReturnError(errors.New("boom"))

	_, err = mgr.acquire(context.Background(), "workflow-scheduler")
	if err == nil {
		t.Fatal("expected error from acquire")
	}
}

func TestLeaseManagerReleaseDeletesOwnedRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	mgr := NewLeaseManager(db, "replica-A", 30*time.Second, 10*time.Second)

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM scheduler_leases WHERE name = $1 AND holder = $2")).
		WithArgs("workflow-scheduler", "replica-A").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := mgr.release(context.Background(), "workflow-scheduler"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestLeaseManagerIsLeaderFalseWhenUnregistered(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	mgr := NewLeaseManager(db, "replica-A", 30*time.Second, 10*time.Second)
	if mgr.IsLeader("not-registered") {
		t.Fatal("unregistered lease should report not-leader")
	}
}

func TestDefaultHolderIDIsNonEmpty(t *testing.T) {
	id := defaultHolderID()
	if id == "" {
		t.Fatal("expected non-empty holder id")
	}
}

// sqlErrNoRows returns sql.ErrNoRows without importing "database/sql"
// at the top of the file; keeping imports minimal.
func sqlErrNoRows() error {
	return sql.ErrNoRows
}
