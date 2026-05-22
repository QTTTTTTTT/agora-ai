package repository

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

// TestHoldFundsWithTxRequiresActiveTx mirrors the TransferWithTx guard —
// callers must always pass an active *sql.Tx so the hold + ledger writes
// commit/rollback together.
func TestHoldFundsWithTxRequiresActiveTx(t *testing.T) {
	repo := &WalletRepo{}
	_, _, err := repo.HoldFundsWithTx(context.Background(), nil, WalletHoldParams{
		UserID:      "buyer",
		AmountMinor: 100,
	})
	if !errors.Is(err, ErrNoTx) {
		t.Fatalf("expected ErrNoTx, got %v", err)
	}
}

func TestHoldFundsRejectsZeroAmount(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()
	// No SQL should run at all.
	mock.ExpectBegin()
	mock.ExpectRollback()

	repo := NewWalletRepo(db)
	tx, _ := db.BeginTx(context.Background(), nil)
	_, _, err = repo.HoldFundsWithTx(context.Background(), tx, WalletHoldParams{
		UserID:      "buyer",
		AmountMinor: 0,
	})
	if !errors.Is(err, ErrHoldAmountZero) {
		t.Fatalf("expected ErrHoldAmountZero, got %v", err)
	}
}

// TestHoldFundsInsufficientBalance verifies the balance gate. When the
// buyer's balance is below the requested hold, no row should be written
// and the typed sentinel is returned.
func TestHoldFundsInsufficientBalance(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO wallet_accounts (user_id)`)).
		WithArgs("buyer").WillReturnResult(sqlmock.NewResult(0, 0))
	cols := []string{"id", "user_id", "base_currency", "balance_minor", "created_at", "updated_at"}
	mock.ExpectQuery(regexp.QuoteMeta(`FROM wallet_accounts`)).
		WithArgs("buyer").
		WillReturnRows(sqlmock.NewRows(cols).AddRow("acc-buyer", "buyer", "USD", int64(50), nowFixed, nowFixed))
	mock.ExpectRollback()

	repo := NewWalletRepo(db)
	tx, _ := db.BeginTx(context.Background(), nil)
	_, _, err = repo.HoldFundsWithTx(context.Background(), tx, WalletHoldParams{
		UserID:      "buyer",
		AmountMinor: 1_000, // more than balance (50)
		Currency:    "USD",
	})
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("expected ErrInsufficientBalance, got %v", err)
	}
	_ = tx.Rollback()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// TestHoldFundsIdempotentReplay verifies that a re-issued hold with the
// same idempotency key returns the existing row without double-deducting
// the balance.
func TestHoldFundsIdempotentReplay(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	// Idempotency lookup: an existing hold is returned.
	holdCols := []string{
		"id", "account_id", "user_id", "amount_minor", "currency", "status",
		"reference_type", "reference_id", "captured_to_user_id", "captured_at",
		"released_at", "idempotency_key", "metadata", "created_at", "updated_at",
	}
	mock.ExpectQuery(regexp.QuoteMeta(`FROM wallet_holds WHERE idempotency_key = $1`)).
		WithArgs("idem-1").
		WillReturnRows(sqlmock.NewRows(holdCols).AddRow(
			"hold-1", "acc-1", "buyer", int64(500), "USD", "active",
			nil, nil, nil, nil, nil, "idem-1", []byte(`{}`), nowFixed, nowFixed,
		))
	// Then load the matching ledger entry.
	ledgerCols := []string{
		"id", "account_id", "entry_type", "amount_minor", "balance_after_minor",
		"currency", "reference_type", "reference_id", "created_by_user_id", "metadata", "created_at",
	}
	mock.ExpectQuery(regexp.QuoteMeta(`FROM wallet_ledger_entries`)).
		WithArgs("hold-1", "wallet_hold").
		WillReturnRows(sqlmock.NewRows(ledgerCols).AddRow(
			"ent-1", "acc-1", "wallet_hold", int64(-500), int64(0), "USD",
			"wallet_hold", "hold-1", nil, []byte(`{}`), nowFixed,
		))
	mock.ExpectCommit()

	repo := NewWalletRepo(db)
	tx, _ := db.BeginTx(context.Background(), nil)
	hold, entry, err := repo.HoldFundsWithTx(context.Background(), tx, WalletHoldParams{
		UserID:         "buyer",
		AmountMinor:    500,
		Currency:       "USD",
		IdempotencyKey: "idem-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hold.ID != "hold-1" {
		t.Fatalf("expected existing hold-1, got %s", hold.ID)
	}
	if entry == nil || entry.ID != "ent-1" {
		t.Fatalf("expected existing ledger entry ent-1")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// TestHoldFundsConcurrentRaceMapsToIdempotencyConflict verifies that when
// the INSERT loses an idempotency race (unique-violation on the partial
// index) the helper retries the lookup and returns the row written by
// the winning caller, not a generic write error.
func TestHoldFundsConcurrentRaceMapsToIdempotencyConflict(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	// First idempotency lookup: nothing — proceed to insert.
	holdCols := []string{
		"id", "account_id", "user_id", "amount_minor", "currency", "status",
		"reference_type", "reference_id", "captured_to_user_id", "captured_at",
		"released_at", "idempotency_key", "metadata", "created_at", "updated_at",
	}
	mock.ExpectQuery(regexp.QuoteMeta(`FROM wallet_holds WHERE idempotency_key = $1`)).
		WithArgs("idem-race").
		WillReturnError(sql.ErrNoRows)

	// Lock buyer account (insert-or-select).
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO wallet_accounts (user_id)`)).
		WithArgs("buyer").WillReturnResult(sqlmock.NewResult(0, 0))
	accCols := []string{"id", "user_id", "base_currency", "balance_minor", "created_at", "updated_at"}
	mock.ExpectQuery(regexp.QuoteMeta(`FROM wallet_accounts`)).
		WithArgs("buyer").
		WillReturnRows(sqlmock.NewRows(accCols).AddRow("acc-1", "buyer", "USD", int64(1_000), nowFixed, nowFixed))
	mock.ExpectQuery(regexp.QuoteMeta(`UPDATE wallet_accounts`)).
		WithArgs(int64(500), "acc-1").
		WillReturnRows(sqlmock.NewRows([]string{"updated_at"}).AddRow(nowFixed))
	// INSERT loses the race: unique-violation.
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO wallet_holds`)).
		WillReturnError(&pq.Error{Code: "23505"})
	// Retry lookup returns the row the winner wrote.
	mock.ExpectQuery(regexp.QuoteMeta(`FROM wallet_holds WHERE idempotency_key = $1`)).
		WithArgs("idem-race").
		WillReturnRows(sqlmock.NewRows(holdCols).AddRow(
			"hold-winner", "acc-1", "buyer", int64(500), "USD", "active",
			nil, nil, nil, nil, nil, "idem-race", []byte(`{}`), nowFixed, nowFixed,
		))
	ledgerCols := []string{
		"id", "account_id", "entry_type", "amount_minor", "balance_after_minor",
		"currency", "reference_type", "reference_id", "created_by_user_id", "metadata", "created_at",
	}
	mock.ExpectQuery(regexp.QuoteMeta(`FROM wallet_ledger_entries`)).
		WithArgs("hold-winner", "wallet_hold").
		WillReturnRows(sqlmock.NewRows(ledgerCols).AddRow(
			"ent-winner", "acc-1", "wallet_hold", int64(-500), int64(500), "USD",
			"wallet_hold", "hold-winner", nil, []byte(`{}`), nowFixed,
		))
	mock.ExpectCommit()

	repo := NewWalletRepo(db)
	tx, _ := db.BeginTx(context.Background(), nil)
	hold, _, err := repo.HoldFundsWithTx(context.Background(), tx, WalletHoldParams{
		UserID:         "buyer",
		AmountMinor:    500,
		Currency:       "USD",
		IdempotencyKey: "idem-race",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hold.ID != "hold-winner" {
		t.Fatalf("expected hold-winner, got %s", hold.ID)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// TestReleaseHoldRejectsNonActiveStatus verifies the helper refuses to
// double-release an already-released or already-captured hold. The
// invariant lets callers (settlement worker) safely retry without
// double-crediting the bidder.
func TestReleaseHoldRejectsNonActiveStatus(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	holdCols := []string{
		"id", "account_id", "user_id", "amount_minor", "currency", "status",
		"reference_type", "reference_id", "captured_to_user_id", "captured_at",
		"released_at", "idempotency_key", "metadata", "created_at", "updated_at",
	}
	mock.ExpectQuery(regexp.QuoteMeta(`FROM wallet_holds WHERE id = $1 FOR UPDATE`)).
		WithArgs("hold-1").
		WillReturnRows(sqlmock.NewRows(holdCols).AddRow(
			"hold-1", "acc-1", "buyer", int64(500), "USD", "released",
			nil, nil, nil, nowFixed, nowFixed, nil, []byte(`{}`), nowFixed, nowFixed,
		))
	mock.ExpectRollback()

	repo := NewWalletRepo(db)
	tx, _ := db.BeginTx(context.Background(), nil)
	_, _, err = repo.ReleaseHoldWithTx(context.Background(), tx, "hold-1", "test")
	if !errors.Is(err, ErrHoldNotActive) {
		t.Fatalf("expected ErrHoldNotActive, got %v", err)
	}
	_ = tx.Rollback()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// TestCaptureHoldRejectsSelfTransfer verifies the safety guard: a hold
// cannot be captured back to its owner (would be a no-op + accounting
// noise). The settlement path should never trip this in production, but
// the validation prevents footguns in admin tooling.
func TestCaptureHoldRejectsSelfTransfer(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	holdCols := []string{
		"id", "account_id", "user_id", "amount_minor", "currency", "status",
		"reference_type", "reference_id", "captured_to_user_id", "captured_at",
		"released_at", "idempotency_key", "metadata", "created_at", "updated_at",
	}
	mock.ExpectQuery(regexp.QuoteMeta(`FROM wallet_holds WHERE id = $1 FOR UPDATE`)).
		WithArgs("hold-1").
		WillReturnRows(sqlmock.NewRows(holdCols).AddRow(
			"hold-1", "acc-1", "buyer", int64(500), "USD", "active",
			nil, nil, nil, nil, nil, nil, []byte(`{}`), nowFixed, nowFixed,
		))
	mock.ExpectRollback()

	repo := NewWalletRepo(db)
	tx, _ := db.BeginTx(context.Background(), nil)
	_, _, _, _, _, err = repo.CaptureHoldWithTx(context.Background(), tx, WalletCaptureParams{
		HoldID:   "hold-1",
		ToUserID: "buyer", // same as hold.UserID
	})
	if err == nil {
		t.Fatalf("expected self-transfer error, got nil")
	}
	_ = tx.Rollback()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

// TestMergeWalletReasonMetadataPreservesExistingKeys ensures the helper
// only injects the release_reason key without clobbering caller-supplied
// metadata (the hold's original metadata is reused on release).
func TestMergeWalletReasonMetadataPreservesExistingKeys(t *testing.T) {
	raw := []byte(`{"flow":"auction_bid_hold","bidId":"bid-1"}`)
	out := mergeWalletReasonMetadata(raw, "outbid")
	// Re-parse & inspect — order is not guaranteed.
	if !regexpMatchAll(out, `"flow":"auction_bid_hold"`, `"bidId":"bid-1"`, `"release_reason":"outbid"`) {
		t.Fatalf("merged metadata missing keys: %s", out)
	}
}

func regexpMatchAll(payload []byte, needles ...string) bool {
	for _, n := range needles {
		if !regexp.MustCompile(regexp.QuoteMeta(n)).Match(payload) {
			return false
		}
	}
	return true
}
