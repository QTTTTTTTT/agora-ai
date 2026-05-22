package audit

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// stubChecker is a SuperAdminChecker that returns canned answers
// keyed by user id. Anyone not in the map is non-admin.
type stubChecker map[string]bool

func (s stubChecker) IsSuperAdmin(_ context.Context, userID string) (bool, error) {
	return s[userID], nil
}

func newServiceFromMock(t *testing.T) (*DualControlService, sqlmock.Sqlmock, *sql.DB, time.Time) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	frozen := time.Date(2026, time.May, 18, 12, 0, 0, 0, time.UTC)
	svc := NewDualControlService(db, NopLogger{}, stubChecker{"alice": true, "bob": true}, time.Hour)
	svc.now = func() time.Time { return frozen }
	return svc, mock, db, frozen
}

// TestSubmitRejectsUnknownAction guards against handlers being
// registered after submissions (which would mean a request sits in the
// queue but nobody can ever execute it). We surface this early at
// submit time.
func TestSubmitRejectsUnknownAction(t *testing.T) {
	svc, _, db, _ := newServiceFromMock(t)
	defer db.Close()
	_, err := svc.Submit(context.Background(), SubmitInput{
		RequesterUserID: "alice",
		Action:          "unregistered_action",
	})
	if !errors.Is(err, ErrUnknownAdminAction) {
		t.Fatalf("expected ErrUnknownAdminAction, got %v", err)
	}
}

// TestSubmitRejectsNonSuperAdmin verifies the requester-role gate.
// Even if someone has admin privileges of other kinds, only super_admin
// can put a request into the dual-control queue.
func TestSubmitRejectsNonSuperAdmin(t *testing.T) {
	svc, _, db, _ := newServiceFromMock(t)
	defer db.Close()
	svc.Register("noop", func(ctx context.Context, tx *sql.Tx, req AdminRequest) (map[string]any, error) {
		return nil, nil
	})
	_, err := svc.Submit(context.Background(), SubmitInput{
		RequesterUserID: "charlie", // not in stub
		Action:          "noop",
	})
	if !errors.Is(err, ErrRequesterNotSuperAdmin) {
		t.Fatalf("expected ErrRequesterNotSuperAdmin, got %v", err)
	}
}

// TestSubmitInsertsPendingRow exercises the happy path. We assert the
// SQL params (requester, action, payload, expiry) match so a regression
// dropping a field would surface here.
func TestSubmitInsertsPendingRow(t *testing.T) {
	svc, mock, db, now := newServiceFromMock(t)
	defer db.Close()
	svc.Register("noop", func(ctx context.Context, tx *sql.Tx, req AdminRequest) (map[string]any, error) {
		return nil, nil
	})

	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO admin_requests`)).
		WithArgs("alice", "noop", "platform_settings", "_singleton_", sqlmock.AnyArg(), sql.NullString{String: "bumping budget", Valid: true}, now.Add(time.Hour)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at", "updated_at"}).AddRow("req-1", now, now))

	req, err := svc.Submit(context.Background(), SubmitInput{
		RequesterUserID: "alice",
		Action:          "noop",
		TargetType:      "platform_settings",
		TargetID:        "_singleton_",
		Payload:         map[string]any{"key": "val"},
		Reason:          "bumping budget",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if req.ID != "req-1" || req.Status != "pending" {
		t.Errorf("unexpected request: %+v", req)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestApproveRejectsSelfApproval is the core security invariant:
// even a super_admin cannot approve their own request. We additionally
// rely on a DB-level CHECK constraint as defense-in-depth, but this
// test asserts the application layer also enforces it so the error
// shows up as a 403 rather than a confusing DB constraint violation.
func TestApproveRejectsSelfApproval(t *testing.T) {
	svc, mock, db, now := newServiceFromMock(t)
	defer db.Close()
	svc.Register("noop", func(ctx context.Context, tx *sql.Tx, req AdminRequest) (map[string]any, error) {
		t.Fatal("handler must not be invoked for self-approval")
		return nil, nil
	})

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectAdminRequestSQL + ` WHERE id=$1 FOR UPDATE`)).
		WithArgs("req-1").
		WillReturnRows(adminRequestRow("req-1", "alice", "noop", "pending", "", now, now.Add(time.Hour)))
	mock.ExpectRollback()

	_, _, err := svc.ApproveAndExecute(context.Background(), "req-1", "alice")
	if !errors.Is(err, ErrRequestSelfApproval) {
		t.Fatalf("expected ErrRequestSelfApproval, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestApproveExpiredRequest verifies expired requests are auto-marked
// and rejected. This must commit the status change (so the queue stays
// clean) but still return ErrRequestExpired to the caller.
func TestApproveExpiredRequest(t *testing.T) {
	svc, mock, db, now := newServiceFromMock(t)
	defer db.Close()
	svc.Register("noop", func(ctx context.Context, tx *sql.Tx, req AdminRequest) (map[string]any, error) {
		t.Fatal("handler must not run on expired request")
		return nil, nil
	})

	expired := now.Add(-time.Minute)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectAdminRequestSQL + ` WHERE id=$1 FOR UPDATE`)).
		WithArgs("req-1").
		WillReturnRows(adminRequestRow("req-1", "alice", "noop", "pending", "", now.Add(-time.Hour), expired))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE admin_requests SET status='expired'`)).
		WithArgs("req-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	_, _, err := svc.ApproveAndExecute(context.Background(), "req-1", "bob")
	if !errors.Is(err, ErrRequestExpired) {
		t.Fatalf("expected ErrRequestExpired, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestApproveAndExecuteHappyPath verifies the entire flow: lock →
// approve → handler runs → executed. The handler receives a transaction
// so its writes commit atomically with the status flip.
func TestApproveAndExecuteHappyPath(t *testing.T) {
	svc, mock, db, now := newServiceFromMock(t)
	defer db.Close()
	handlerCalled := false
	svc.Register("noop", func(ctx context.Context, tx *sql.Tx, req AdminRequest) (map[string]any, error) {
		handlerCalled = true
		if tx == nil {
			t.Fatal("expected non-nil transaction in handler")
		}
		return map[string]any{"applied": true}, nil
	})

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectAdminRequestSQL + ` WHERE id=$1 FOR UPDATE`)).
		WithArgs("req-1").
		WillReturnRows(adminRequestRow("req-1", "alice", "noop", "pending", "", now, now.Add(time.Hour)))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE admin_requests SET status='approved'`)).
		WithArgs("req-1", "bob").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE admin_requests SET status='executed'`)).
		WithArgs("req-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	req, details, err := svc.ApproveAndExecute(context.Background(), "req-1", "bob")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if !handlerCalled {
		t.Error("handler was not called")
	}
	if details["applied"] != true {
		t.Errorf("details not propagated: %+v", details)
	}
	if req.Status != "executed" {
		t.Errorf("expected executed status, got %s", req.Status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestApproveHandlerFailureMarksFailed verifies that when the action
// handler returns an error, the transaction is rolled back AND the
// row is moved to "failed" (so it doesn't sit pending forever). The
// error from the handler is the one returned to the caller.
func TestApproveHandlerFailureMarksFailed(t *testing.T) {
	svc, mock, db, now := newServiceFromMock(t)
	defer db.Close()
	want := errors.New("kaboom")
	svc.Register("noop", func(ctx context.Context, tx *sql.Tx, req AdminRequest) (map[string]any, error) {
		return nil, want
	})

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectAdminRequestSQL + ` WHERE id=$1 FOR UPDATE`)).
		WithArgs("req-1").
		WillReturnRows(adminRequestRow("req-1", "alice", "noop", "pending", "", now, now.Add(time.Hour)))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE admin_requests SET status='approved'`)).
		WithArgs("req-1", "bob").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectRollback()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE admin_requests SET status='failed'`)).
		WithArgs("req-1", "bob", "kaboom").
		WillReturnResult(sqlmock.NewResult(0, 1))

	_, _, err := svc.ApproveAndExecute(context.Background(), "req-1", "bob")
	if !errors.Is(err, want) {
		t.Fatalf("expected wrapped handler error, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestApproveRejectsNonSuperAdminApprover verifies the approver role
// gate. Combined with TestSubmitRejectsNonSuperAdmin this proves the
// system enforces "super_admin × 2" not just "any admin × 2".
func TestApproveRejectsNonSuperAdminApprover(t *testing.T) {
	svc, _, db, _ := newServiceFromMock(t)
	defer db.Close()
	svc.Register("noop", func(ctx context.Context, tx *sql.Tx, req AdminRequest) (map[string]any, error) {
		return nil, nil
	})
	_, _, err := svc.ApproveAndExecute(context.Background(), "req-1", "charlie")
	if !errors.Is(err, ErrApproverNotSuperAdmin) {
		t.Fatalf("expected ErrApproverNotSuperAdmin, got %v", err)
	}
}

// TestRejectRequest confirms a different super_admin can reject a
// pending request, transitioning it cleanly out of the queue.
func TestRejectRequest(t *testing.T) {
	svc, mock, db, now := newServiceFromMock(t)
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(selectAdminRequestSQL + ` WHERE id=$1 FOR UPDATE`)).
		WithArgs("req-1").
		WillReturnRows(adminRequestRow("req-1", "alice", "noop", "pending", "", now, now.Add(time.Hour)))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE admin_requests SET status='rejected'`)).
		WithArgs("req-1", "bob", sql.NullString{String: "not aligned with policy", Valid: true}).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	req, err := svc.Reject(context.Background(), "req-1", "bob", "not aligned with policy")
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if req.Status != "rejected" {
		t.Errorf("expected rejected, got %s", req.Status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestDuplicateRegisterPanics is the operational safety net: two
// modules each thinking they own an action name would silently
// overwrite each other's handler. We panic loud at startup instead.
func TestDuplicateRegisterPanics(t *testing.T) {
	svc, _, db, _ := newServiceFromMock(t)
	defer db.Close()
	svc.Register("noop", func(ctx context.Context, tx *sql.Tx, req AdminRequest) (map[string]any, error) {
		return nil, nil
	})
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate register")
		}
	}()
	svc.Register("noop", func(ctx context.Context, tx *sql.Tx, req AdminRequest) (map[string]any, error) {
		return nil, nil
	})
}

// adminRequestRow constructs the row shape returned by selectAdminRequestSQL.
func adminRequestRow(id, requester, action, status, approver string, createdAt, expiresAt time.Time) *sqlmock.Rows {
	approverVal := sql.NullString{}
	if approver != "" {
		approverVal = sql.NullString{String: approver, Valid: true}
	}
	return sqlmock.NewRows([]string{
		"id", "requester_user_id", "action", "target_type", "target_id", "payload", "reason", "status",
		"approver_user_id", "approved_at", "executed_at", "execution_error", "expires_at", "created_at", "updated_at",
	}).AddRow(
		id, requester, action, "platform_settings", "_singleton_", []byte(`{}`), sql.NullString{}, status,
		approverVal, sql.NullTime{}, sql.NullTime{}, sql.NullString{}, expiresAt, createdAt, createdAt,
	)
}
