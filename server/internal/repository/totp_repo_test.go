package repository

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

func newMockedTOTPRepo(t *testing.T) (*UserTOTPRepo, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return NewUserTOTPRepo(db), mock, func() { _ = db.Close() }
}

var totpRepoColumns = []string{
	"user_id", "secret_encrypted", "issuer", "account_label", "digits",
	"period_seconds", "algorithm", "recovery_codes_hashed", "enrolment_attempts",
	"enabled_at", "last_verified_at", "last_used_recovery_at",
	"created_at", "updated_at",
}

func TestUserTOTPRepo_GetByUserID_NoRow(t *testing.T) {
	repo, mock, cleanup := newMockedTOTPRepo(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta(`FROM user_totp_secrets`)).
		WithArgs("user-1").
		WillReturnError(sql.ErrNoRows)
	_, err := repo.GetByUserID(context.Background(), "user-1")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("err = %v, want sql.ErrNoRows", err)
	}
}

func TestUserTOTPRepo_GetByUserID_PendingEnrolment(t *testing.T) {
	repo, mock, cleanup := newMockedTOTPRepo(t)
	defer cleanup()
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`FROM user_totp_secrets`)).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows(totpRepoColumns).AddRow(
			"user-1", []byte("ct"), "FundAI", "u@example.com", 6,
			30, "SHA1", pq.Array([]string{"hashed-1", "hashed-2"}), 0,
			nil, nil, nil,
			now, now,
		))
	row, err := repo.GetByUserID(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if row.IsEnabled() {
		t.Errorf("IsEnabled = true, want false (enabled_at NULL)")
	}
	if len(row.RecoveryCodesHashed) != 2 {
		t.Errorf("RecoveryCodesHashed len = %d, want 2", len(row.RecoveryCodesHashed))
	}
}

func TestUserTOTPRepo_Enrol_RejectsAlreadyEnabled(t *testing.T) {
	repo, mock, cleanup := newMockedTOTPRepo(t)
	defer cleanup()
	now := time.Now()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT enabled_at FROM user_totp_secrets`)).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows([]string{"enabled_at"}).AddRow(now))
	mock.ExpectRollback()

	err := repo.Enrol(context.Background(), EnrolParams{
		UserID:              "user-1",
		SecretEncrypted:     []byte("ct"),
		Issuer:              "FundAI",
		AccountLabel:        "u",
		Digits:              6,
		PeriodSeconds:       30,
		Algorithm:           "SHA1",
		RecoveryCodesHashed: []string{"h1"},
	})
	if !errors.Is(err, ErrTOTPAlreadyEnabled) {
		t.Errorf("err = %v, want ErrTOTPAlreadyEnabled", err)
	}
}

func TestUserTOTPRepo_Enrol_HappyPath_NoExistingRow(t *testing.T) {
	repo, mock, cleanup := newMockedTOTPRepo(t)
	defer cleanup()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT enabled_at FROM user_totp_secrets`)).
		WithArgs("user-1").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO user_totp_secrets`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err := repo.Enrol(context.Background(), EnrolParams{
		UserID:              "user-1",
		SecretEncrypted:     []byte("ct"),
		Issuer:              "FundAI",
		AccountLabel:        "u",
		Digits:              6,
		PeriodSeconds:       30,
		Algorithm:           "SHA1",
		RecoveryCodesHashed: []string{"h1"},
	})
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}

func TestUserTOTPRepo_MarkEnabled(t *testing.T) {
	repo, mock, cleanup := newMockedTOTPRepo(t)
	defer cleanup()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE user_totp_secrets`)).
		WithArgs("user-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.MarkEnabled(context.Background(), "user-1"); err != nil {
		t.Errorf("err = %v", err)
	}
}

func TestUserTOTPRepo_MarkEnabled_ReturnsErrNoRowsWhenAbsent(t *testing.T) {
	repo, mock, cleanup := newMockedTOTPRepo(t)
	defer cleanup()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE user_totp_secrets`)).
		WithArgs("user-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := repo.MarkEnabled(context.Background(), "user-1"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("err = %v, want sql.ErrNoRows", err)
	}
}

func TestUserTOTPRepo_BumpEnrolmentAttempts(t *testing.T) {
	repo, mock, cleanup := newMockedTOTPRepo(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta(`UPDATE user_totp_secrets`)).
		WithArgs("user-1").
		WillReturnRows(sqlmock.NewRows([]string{"enrolment_attempts"}).AddRow(3))
	n, err := repo.BumpEnrolmentAttempts(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if n != 3 {
		t.Errorf("n = %d, want 3", n)
	}
}

func TestUserTOTPRepo_ConsumeRecoveryCode(t *testing.T) {
	repo, mock, cleanup := newMockedTOTPRepo(t)
	defer cleanup()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE user_totp_secrets`)).
		WithArgs("user-1", "hashed-code").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.ConsumeRecoveryCode(context.Background(), "user-1", "hashed-code"); err != nil {
		t.Errorf("err = %v", err)
	}
}

func TestUserTOTPRepo_Disable(t *testing.T) {
	repo, mock, cleanup := newMockedTOTPRepo(t)
	defer cleanup()
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM user_totp_secrets`)).
		WithArgs("user-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.Disable(context.Background(), "user-1"); err != nil {
		t.Errorf("err = %v", err)
	}
}

func TestUserTOTPRepo_Disable_NoRow(t *testing.T) {
	repo, mock, cleanup := newMockedTOTPRepo(t)
	defer cleanup()
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM user_totp_secrets`)).
		WithArgs("user-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := repo.Disable(context.Background(), "user-1"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("err = %v, want sql.ErrNoRows", err)
	}
}

func TestUserTOTPRepo_RejectsEmptyIDs(t *testing.T) {
	repo, _, cleanup := newMockedTOTPRepo(t)
	defer cleanup()
	if _, err := repo.GetByUserID(context.Background(), ""); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetByUserID empty id err = %v", err)
	}
	if err := repo.Enrol(context.Background(), EnrolParams{}); err == nil {
		t.Errorf("Enrol empty params accepted")
	}
}
