package repository

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

var fundingColumns = []string{
	"id", "fund_id", "direction", "amount", "currency", "method",
	"external_reference", "status", "requested_by", "approved_by",
	"approved_at", "rejected_by", "rejected_at", "rejection_reason",
	"cancelled_at", "cash_ledger_entry_id", "notes", "metadata",
	"created_at", "updated_at",
}

func newMockedFundingRepo(t *testing.T) (*FundingRepo, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return NewFundingRepo(db), mock, func() { _ = db.Close() }
}

func TestFundingRepo_Create_HappyPath(t *testing.T) {
	repo, mock, cleanup := newMockedFundingRepo(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO funding_requests")).
		WithArgs("fund-1", "deposit", 1000.0, "USD", "wire",
			"ref-1", "user-1", "first deposit", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("fr-1"))
	id, err := repo.Create(context.Background(), CreateFundingRequestParams{
		FundID:            "fund-1",
		Direction:         FundingDirectionDeposit,
		Amount:            1000.0,
		Currency:          "usd",
		Method:            FundingMethodWire,
		ExternalReference: "ref-1",
		RequestedBy:       "user-1",
		Notes:             "first deposit",
		Metadata:          map[string]any{"source": "test"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id != "fr-1" {
		t.Errorf("id = %q", id)
	}
}

func TestFundingRepo_Create_RejectsInvalid(t *testing.T) {
	repo, _, cleanup := newMockedFundingRepo(t)
	defer cleanup()
	cases := []struct {
		name string
		p    CreateFundingRequestParams
	}{
		{"empty fund", CreateFundingRequestParams{Direction: "deposit", Amount: 10, Method: "wire", RequestedBy: "u"}},
		{"empty user", CreateFundingRequestParams{FundID: "f", Direction: "deposit", Amount: 10, Method: "wire"}},
		{"bad direction", CreateFundingRequestParams{FundID: "f", Direction: "send", Amount: 10, Method: "wire", RequestedBy: "u"}},
		{"bad method", CreateFundingRequestParams{FundID: "f", Direction: "deposit", Amount: 10, Method: "carrier_pigeon", RequestedBy: "u"}},
		{"zero amount", CreateFundingRequestParams{FundID: "f", Direction: "deposit", Amount: 0, Method: "wire", RequestedBy: "u"}},
		{"negative amount", CreateFundingRequestParams{FundID: "f", Direction: "deposit", Amount: -1, Method: "wire", RequestedBy: "u"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := repo.Create(context.Background(), c.p); !errors.Is(err, ErrFundingRequestInvalid) {
				t.Errorf("err = %v, want ErrFundingRequestInvalid", err)
			}
		})
	}
}

func TestFundingRepo_GetByID_NotFound(t *testing.T) {
	repo, mock, cleanup := newMockedFundingRepo(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta("FROM funding_requests")).
		WithArgs("missing").
		WillReturnError(sql.ErrNoRows)
	if _, err := repo.GetByID(context.Background(), "missing"); !errors.Is(err, ErrFundingRequestNotFound) {
		t.Errorf("err = %v, want ErrFundingRequestNotFound", err)
	}
}

func TestFundingRepo_ListByFund_FilteredByStatus(t *testing.T) {
	repo, mock, cleanup := newMockedFundingRepo(t)
	defer cleanup()
	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta("FROM funding_requests WHERE fund_id = $1 AND status = ANY($2)")).
		WithArgs("fund-1", sqlmock.AnyArg(), 100).
		WillReturnRows(sqlmock.NewRows(fundingColumns).
			AddRow(
				"fr-1", "fund-1", "deposit", 1000.0, "USD", "wire",
				nil, "pending", "user-1", nil,
				nil, nil, nil, nil,
				nil, nil, nil, []byte(`{}`),
				now, now,
			))
	got, err := repo.ListByFund(context.Background(), "fund-1", ListFundingByFundParams{
		Statuses: []string{FundingStatusPending},
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 1 || got[0].Status != "pending" {
		t.Errorf("got = %+v", got)
	}
}

func TestFundingRepo_Cancel_OnlyOwnerPending(t *testing.T) {
	repo, mock, cleanup := newMockedFundingRepo(t)
	defer cleanup()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE funding_requests")).
		WithArgs("fr-1", "user-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := repo.Cancel(context.Background(), "fr-1", "user-1"); !errors.Is(err, ErrFundingRequestStateConflict) {
		t.Errorf("err = %v, want state conflict", err)
	}
}

func TestFundingRepo_Cancel_HappyPath(t *testing.T) {
	repo, mock, cleanup := newMockedFundingRepo(t)
	defer cleanup()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE funding_requests")).
		WithArgs("fr-1", "user-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repo.Cancel(context.Background(), "fr-1", "user-1"); err != nil {
		t.Errorf("err = %v", err)
	}
}

func TestFundingRepo_MarkApprovedTx_Success(t *testing.T) {
	repo, mock, cleanup := newMockedFundingRepo(t)
	defer cleanup()
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`SET status = 'approved'`)).
		WithArgs("fr-1", "approver-1", "ledger-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	tx, err := repo.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := repo.MarkApprovedTx(context.Background(), tx, "fr-1", "approver-1", "ledger-1"); err != nil {
		t.Fatalf("MarkApprovedTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestFundingRepo_MarkApprovedTx_StateConflict(t *testing.T) {
	repo, mock, cleanup := newMockedFundingRepo(t)
	defer cleanup()
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`SET status = 'approved'`)).
		WithArgs("fr-1", "approver-1", "ledger-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	tx, err := repo.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := repo.MarkApprovedTx(context.Background(), tx, "fr-1", "approver-1", "ledger-1"); !errors.Is(err, ErrFundingRequestStateConflict) {
		t.Errorf("err = %v, want state conflict", err)
	}
	_ = tx.Rollback()
}

func TestFundingRepo_MarkRejectedTx_RequiresPending(t *testing.T) {
	repo, mock, cleanup := newMockedFundingRepo(t)
	defer cleanup()
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`SET status = 'rejected'`)).
		WithArgs("fr-1", "rejector-1", "duplicate request").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	tx, err := repo.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := repo.MarkRejectedTx(context.Background(), tx, "fr-1", "rejector-1", "duplicate request"); !errors.Is(err, ErrFundingRequestStateConflict) {
		t.Errorf("err = %v, want state conflict", err)
	}
	_ = tx.Rollback()
}

func TestFundingRepo_ListPendingAdmin(t *testing.T) {
	repo, mock, cleanup := newMockedFundingRepo(t)
	defer cleanup()
	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta("WHERE status = 'pending'")).
		WithArgs(200).
		WillReturnRows(sqlmock.NewRows(fundingColumns).
			AddRow(
				"fr-1", "fund-1", "withdrawal", 500.0, "USD", "wire",
				nil, "pending", "user-1", nil,
				nil, nil, nil, nil,
				nil, nil, "urgent", []byte(`{}`),
				now, now,
			))
	got, err := repo.ListPendingAdmin(context.Background(), 0)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 1 || got[0].Direction != "withdrawal" {
		t.Errorf("got = %+v", got)
	}
}
