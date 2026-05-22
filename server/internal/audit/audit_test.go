package audit

import (
	"context"
	"database/sql"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestNopLogger(t *testing.T) {
	logger := NopLogger{}
	err := logger.LogAccess(context.Background(), "user-1", "read", "memory", "mem-1", map[string]any{"reason": "test"})
	if err != nil {
		t.Errorf("NopLogger should not return error, got %v", err)
	}
}

func TestDBLoggerLogAccessInsertsDataAccessRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO data_access_log (actor_user_id, action, resource_type, resource_id, details)
		 VALUES ($1, $2, $3, $4, $5)`)).
		WithArgs("user-1", "read", "memory", "fund-1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	logger := NewDBLogger(db)
	if err := logger.LogAccess(context.Background(), "user-1", "read", "memory", "fund-1", map[string]any{"fundId": "fund-1"}); err != nil {
		t.Fatalf("LogAccess: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestDBLoggerLogAccessTxUsesTransaction(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO data_access_log (actor_user_id, action, resource_type, resource_id, details)
		 VALUES ($1, $2, $3, $4, $5)`)).
		WithArgs("user-1", "export", "audit_log", "fund-1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	logger := NewDBLogger(db)
	if err := logger.LogAccessTx(context.Background(), tx, "user-1", "export", "audit_log", "fund-1", map[string]any{"fundId": "fund-1"}); err != nil {
		t.Fatalf("LogAccessTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
