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

var nowFixed = time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC)

// TestTransferWithTxRequiresActiveTx verifies that callers cannot bypass
// the WithinTx contract by passing a nil transaction.
func TestTransferWithTxRequiresActiveTx(t *testing.T) {
	repo := &WalletRepo{}
	_, _, _, _, err := repo.TransferWithTx(context.Background(), nil, WalletTransferParams{
		FromUserID:  "buyer",
		ToUserID:    "seller",
		AmountMinor: 100,
	})
	if !errors.Is(err, ErrNoTx) {
		t.Fatalf("expected ErrNoTx, got %v", err)
	}
}

// TestTransferWithTxIdempotencyConflict ensures that a unique-violation on
// idempotency_key surfaces as ErrIdempotencyConflict so the marketplace
// flow can map it to api.ErrConflict.
func TestTransferWithTxIdempotencyConflict(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	// ensure both wallet rows exist
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO wallet_accounts (user_id)`)).
		WithArgs("buyer").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO wallet_accounts (user_id)`)).
		WithArgs("seller").WillReturnResult(sqlmock.NewResult(0, 0))

	// lock both rows in deterministic order
	cols := []string{"id", "user_id", "base_currency", "balance_minor", "created_at", "updated_at"}
	mock.ExpectQuery(regexp.QuoteMeta(`FROM wallet_accounts`)).
		WithArgs("buyer").
		WillReturnRows(sqlmock.NewRows(cols).AddRow("acc-buyer", "buyer", "USD", int64(1_000_000), nowFixed, nowFixed))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM wallet_accounts`)).
		WithArgs("seller").
		WillReturnRows(sqlmock.NewRows(cols).AddRow("acc-seller", "seller", "USD", int64(0), nowFixed, nowFixed))

	// Update both balances.
	mock.ExpectQuery(regexp.QuoteMeta(`UPDATE wallet_accounts`)).
		WithArgs(int64(900_000), "acc-buyer").
		WillReturnRows(sqlmock.NewRows([]string{"updated_at"}).AddRow(nowFixed))
	mock.ExpectQuery(regexp.QuoteMeta(`UPDATE wallet_accounts`)).
		WithArgs(int64(100_000), "acc-seller").
		WillReturnRows(sqlmock.NewRows([]string{"updated_at"}).AddRow(nowFixed))

	// Debit ledger insert: simulate unique-violation on idempotency_key.
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO wallet_ledger_entries`)).
		WillReturnError(&pq.Error{Code: "23505"})

	mock.ExpectRollback()

	repo := NewWalletRepo(db)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	_, _, _, _, err = repo.TransferWithTx(context.Background(), tx, WalletTransferParams{
		FromUserID:        "buyer",
		ToUserID:          "seller",
		AmountMinor:       100_000,
		Currency:          "USD",
		ReferenceType:     "agent_market_order",
		ReferenceID:       "order-1",
		DebitIdempotency:  "k:debit",
		CreditIdempotency: "k:credit",
	})
	if !errors.Is(err, ErrIdempotencyConflict) {
		_ = tx.Rollback()
		t.Fatalf("expected ErrIdempotencyConflict, got %v", err)
	}
	_ = tx.Rollback()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestTransferWithTxInsufficientBalance ensures the typed error surfaces
// before any ledger row is written.
func TestTransferWithTxInsufficientBalance(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO wallet_accounts (user_id)`)).
		WithArgs("buyer").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO wallet_accounts (user_id)`)).
		WithArgs("seller").WillReturnResult(sqlmock.NewResult(0, 0))

	cols := []string{"id", "user_id", "base_currency", "balance_minor", "created_at", "updated_at"}
	mock.ExpectQuery(regexp.QuoteMeta(`FROM wallet_accounts`)).
		WithArgs("buyer").
		WillReturnRows(sqlmock.NewRows(cols).AddRow("acc-buyer", "buyer", "USD", int64(50), nowFixed, nowFixed))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM wallet_accounts`)).
		WithArgs("seller").
		WillReturnRows(sqlmock.NewRows(cols).AddRow("acc-seller", "seller", "USD", int64(0), nowFixed, nowFixed))

	mock.ExpectRollback()

	repo := NewWalletRepo(db)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	_, _, _, _, err = repo.TransferWithTx(context.Background(), tx, WalletTransferParams{
		FromUserID:  "buyer",
		ToUserID:    "seller",
		AmountMinor: 100_000,
		Currency:    "USD",
	})
	if !errors.Is(err, ErrInsufficientBalance) {
		_ = tx.Rollback()
		t.Fatalf("expected ErrInsufficientBalance, got %v", err)
	}
	_ = tx.Rollback()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestCreatePendingOrderWithTxIdempotentReplay ensures that retried calls
// land on the existing row instead of creating a duplicate.
func TestCreatePendingOrderWithTxIdempotentReplay(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	// Insert returns no rows because of ON CONFLICT DO NOTHING.
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO agent_market_orders`)).
		WillReturnError(sql.ErrNoRows)

	cols := []string{"id", "listing_id", "seller_user_id", "buyer_user_id", "buyer_fund_id", "source_agent_id", "delivered_agent_id", "amount_minor", "currency", "status", "created_at"}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, listing_id, seller_user_id`)).
		WithArgs("idem-1").
		WillReturnRows(sqlmock.NewRows(cols).AddRow("ord-1", "listing-1", "seller", "buyer", sql.NullString{String: "fund-1", Valid: true}, "agent-src", "agent-new", int64(1000), "USD", "completed", nowFixed))

	repo := NewMarketplaceRepo(db)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	order, created, err := repo.CreatePendingOrderWithTx(context.Background(), tx, CreateAgentMarketOrderParams{
		ListingID:      "listing-1",
		SellerUserID:   "seller",
		BuyerUserID:    "buyer",
		SourceAgentID:  "agent-src",
		AmountMinor:    1000,
		Currency:       "USD",
		IdempotencyKey: "idem-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if created {
		t.Fatalf("expected created=false on replay")
	}
	if order.Status != "completed" {
		t.Fatalf("expected replay to surface completed order, got %s", order.Status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestCreatePendingOrderWithTxRequiresIdempotencyKey ensures we never
// silently insert orders without a key — that would defeat the whole
// retry-safe contract.
func TestCreatePendingOrderWithTxRequiresIdempotencyKey(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()
	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	repo := NewMarketplaceRepo(db)
	_, _, err = repo.CreatePendingOrderWithTx(context.Background(), tx, CreateAgentMarketOrderParams{
		ListingID: "listing-1",
	})
	if err == nil {
		t.Fatal("expected error for missing idempotency key")
	}
}
