package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Hold accounting model:
//
//   * HoldFunds locks the account row, verifies spendable balance, debits
//     the balance, writes a `wallet_hold` ledger entry and inserts a
//     `wallet_holds` row with status='active'. Balance and ledger move
//     together — exactly the same invariants as Transfer.
//
//   * ReleaseHold reverses the hold: balance += amount, ledger entry
//     `wallet_hold_release` (+), hold row flipped to status='released'.
//
//   * CaptureHold converts a held amount into a settlement transfer. It
//     first reverses the hold (release-style ledger entry, balance refund)
//     and then performs a standard buyer→seller Transfer — so the buyer's
//     ledger shows three rows (hold debit, hold release credit, settlement
//     debit) which net out to the original hold debit, and the seller sees
//     a single settlement credit. This keeps the ledger fully double-entry
//     and reuses the well-tested Transfer path for the actual settlement
//     side-effect.
//
// All three operations support an idempotency key on the wallet_holds row
// itself, so retrying the *same* logical operation always returns the
// existing hold instead of double-locking funds.

var (
	ErrHoldNotFound   = errors.New("repository: hold not found")
	ErrHoldNotActive  = errors.New("repository: hold is not active")
	ErrHoldAmountZero = errors.New("repository: hold amount must be positive")
)

type WalletHold struct {
	ID                 string          `json:"id"`
	AccountID          string          `json:"account_id"`
	UserID             string          `json:"user_id"`
	AmountMinor        int64           `json:"amount_minor"`
	Currency           string          `json:"currency"`
	Status             string          `json:"status"`
	ReferenceType      sql.NullString  `json:"reference_type"`
	ReferenceID        sql.NullString  `json:"reference_id"`
	CapturedToUserID   sql.NullString  `json:"captured_to_user_id"`
	CapturedAt         sql.NullTime    `json:"captured_at"`
	ReleasedAt         sql.NullTime    `json:"released_at"`
	IdempotencyKey     sql.NullString  `json:"idempotency_key"`
	Metadata           json.RawMessage `json:"metadata"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

type WalletHoldParams struct {
	UserID         string
	AmountMinor    int64
	Currency       string
	ReferenceType  string
	ReferenceID    string
	Metadata       json.RawMessage
	IdempotencyKey string
}

// HoldFunds is the convenience wrapper that opens its own transaction.
// Auction bid placement passes through the *_WithTx variant because it
// composes with the bid insert + listing UPDATE in a single Unit-of-Work.
func (r *WalletRepo) HoldFunds(ctx context.Context, params WalletHoldParams) (*WalletHold, *WalletLedgerEntry, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("wallet_repo: begin hold tx: %w", err)
	}
	defer tx.Rollback()
	hold, entry, err := r.HoldFundsWithTx(ctx, tx, params)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("wallet_repo: commit hold tx: %w", err)
	}
	return hold, entry, nil
}

func (r *WalletRepo) HoldFundsWithTx(ctx context.Context, tx *sql.Tx, params WalletHoldParams) (*WalletHold, *WalletLedgerEntry, error) {
	if tx == nil {
		return nil, nil, ErrNoTx
	}
	if params.AmountMinor <= 0 {
		return nil, nil, ErrHoldAmountZero
	}
	userID := strings.TrimSpace(params.UserID)
	if userID == "" {
		return nil, nil, fmt.Errorf("wallet_repo: hold requires user_id")
	}
	currency := normalizeWalletCurrency(params.Currency)
	idempotencyKey := strings.TrimSpace(params.IdempotencyKey)
	metadata, err := marshalWalletMetadata(params.Metadata)
	if err != nil {
		return nil, nil, fmt.Errorf("wallet_repo: marshal hold metadata: %w", err)
	}

	if idempotencyKey != "" {
		existing, err := loadHoldByIdempotencyKey(ctx, tx, idempotencyKey)
		if err != nil && !errors.Is(err, ErrHoldNotFound) {
			return nil, nil, err
		}
		if existing != nil {
			entry, err := loadHoldLedgerEntry(ctx, tx, existing.ID, "wallet_hold")
			if err != nil {
				return nil, nil, err
			}
			return existing, entry, nil
		}
	}

	account, err := ensureWalletAccountForUpdate(ctx, tx, userID)
	if err != nil {
		return nil, nil, err
	}
	if account.BalanceMinor < params.AmountMinor {
		return nil, nil, ErrInsufficientBalance
	}
	account.BalanceMinor -= params.AmountMinor
	if err := updateWalletAccountBalance(ctx, tx, account); err != nil {
		return nil, nil, err
	}

	hold := &WalletHold{}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO wallet_holds (
			account_id, user_id, amount_minor, currency,
			status, reference_type, reference_id,
			idempotency_key, metadata
		)
		VALUES ($1, $2, $3, $4, 'active', NULLIF($5, ''), NULLIF($6, ''), NULLIF($7, ''), $8)
		RETURNING id, account_id, user_id, amount_minor, currency, status,
		          reference_type, reference_id, captured_to_user_id, captured_at,
		          released_at, idempotency_key, metadata, created_at, updated_at
	`, account.ID, userID, params.AmountMinor, currency,
		strings.TrimSpace(params.ReferenceType), strings.TrimSpace(params.ReferenceID),
		idempotencyKey, metadata).Scan(
		&hold.ID, &hold.AccountID, &hold.UserID, &hold.AmountMinor, &hold.Currency, &hold.Status,
		&hold.ReferenceType, &hold.ReferenceID, &hold.CapturedToUserID, &hold.CapturedAt,
		&hold.ReleasedAt, &hold.IdempotencyKey, &hold.Metadata, &hold.CreatedAt, &hold.UpdatedAt,
	)
	if err != nil {
		if isUniqueViolation(err) {
			// Concurrent caller won the idempotency race; reload the row
			// they wrote and return it instead of double-holding.
			existing, loadErr := loadHoldByIdempotencyKey(ctx, tx, idempotencyKey)
			if loadErr == nil {
				entry, entErr := loadHoldLedgerEntry(ctx, tx, existing.ID, "wallet_hold")
				if entErr == nil {
					return existing, entry, nil
				}
			}
			return nil, nil, ErrIdempotencyConflict
		}
		return nil, nil, fmt.Errorf("wallet_repo: insert hold: %w", err)
	}

	ledgerIdem := ""
	if idempotencyKey != "" {
		ledgerIdem = idempotencyKey + ":hold"
	}
	entry, err := insertWalletLedgerEntry(ctx, tx, walletLedgerInsertParams{
		AccountID:         account.ID,
		EntryType:         "wallet_hold",
		AmountMinor:       -params.AmountMinor,
		BalanceAfterMinor: account.BalanceMinor,
		Currency:          currency,
		ReferenceType:     "wallet_hold",
		ReferenceID:       hold.ID,
		CreatedByUserID:   userID,
		Metadata:          metadata,
		IdempotencyKey:    ledgerIdem,
	})
	if err != nil {
		return nil, nil, err
	}
	return hold, entry, nil
}

// ReleaseHold credits the held amount back to the bidder and flips the
// hold row to status='released'. Safe to call after a no-op (already
// released or captured) — returns ErrHoldNotActive so callers can ignore
// the redundant attempt.
func (r *WalletRepo) ReleaseHold(ctx context.Context, holdID, reason string) (*WalletHold, *WalletLedgerEntry, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("wallet_repo: begin release tx: %w", err)
	}
	defer tx.Rollback()
	hold, entry, err := r.ReleaseHoldWithTx(ctx, tx, holdID, reason)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("wallet_repo: commit release tx: %w", err)
	}
	return hold, entry, nil
}

func (r *WalletRepo) ReleaseHoldWithTx(ctx context.Context, tx *sql.Tx, holdID, reason string) (*WalletHold, *WalletLedgerEntry, error) {
	if tx == nil {
		return nil, nil, ErrNoTx
	}
	holdID = strings.TrimSpace(holdID)
	if holdID == "" {
		return nil, nil, fmt.Errorf("wallet_repo: release requires hold_id")
	}
	hold, err := loadHoldForUpdate(ctx, tx, holdID)
	if err != nil {
		return nil, nil, err
	}
	if hold.Status != "active" {
		return nil, nil, ErrHoldNotActive
	}

	account, err := loadWalletAccountForUpdate(ctx, tx, hold.UserID)
	if err != nil {
		return nil, nil, err
	}
	account.BalanceMinor += hold.AmountMinor
	if err := updateWalletAccountBalance(ctx, tx, account); err != nil {
		return nil, nil, err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE wallet_holds
		   SET status = 'released',
		       released_at = NOW(),
		       updated_at = NOW()
		 WHERE id = $1
	`, hold.ID); err != nil {
		return nil, nil, fmt.Errorf("wallet_repo: mark hold released: %w", err)
	}
	hold.Status = "released"
	hold.ReleasedAt = sql.NullTime{Time: time.Now(), Valid: true}

	metadata := hold.Metadata
	if reason = strings.TrimSpace(reason); reason != "" {
		metadata = mergeWalletReasonMetadata(metadata, reason)
	}
	ledgerIdem := ""
	if hold.IdempotencyKey.Valid && strings.TrimSpace(hold.IdempotencyKey.String) != "" {
		ledgerIdem = strings.TrimSpace(hold.IdempotencyKey.String) + ":release"
	}
	entry, err := insertWalletLedgerEntry(ctx, tx, walletLedgerInsertParams{
		AccountID:         hold.AccountID,
		EntryType:         "wallet_hold_release",
		AmountMinor:       hold.AmountMinor,
		BalanceAfterMinor: account.BalanceMinor,
		Currency:          hold.Currency,
		ReferenceType:     "wallet_hold",
		ReferenceID:       hold.ID,
		CreatedByUserID:   hold.UserID,
		Metadata:          metadata,
		IdempotencyKey:    ledgerIdem,
	})
	if err != nil {
		return nil, nil, err
	}
	return hold, entry, nil
}

type WalletCaptureParams struct {
	HoldID             string
	ToUserID           string
	ReferenceType      string
	ReferenceID        string
	DebitEntryType     string
	CreditEntryType    string
	CreatedByUserID    string
	DebitMetadata      json.RawMessage
	CreditMetadata     json.RawMessage
	IdempotencyKeyBase string
}

// CaptureHold turns an active hold into a settlement transfer to a third
// party (the seller). It performs two ledger steps inside a single tx:
//
//  1. Reverse the hold (refund balance, write `wallet_hold_capture_release`).
//  2. Transfer the same amount from buyer → recipient.
//
// The two-step model keeps the buyer's ledger transparent (a "+X" reversal
// row sits next to the original "-X" hold, then a "-X" settlement row) and
// reuses TransferWithTx so we don't fork the settlement code path.
func (r *WalletRepo) CaptureHoldWithTx(ctx context.Context, tx *sql.Tx, params WalletCaptureParams) (*WalletHold, *WalletAccount, *WalletAccount, *WalletLedgerEntry, *WalletLedgerEntry, error) {
	if tx == nil {
		return nil, nil, nil, nil, nil, ErrNoTx
	}
	holdID := strings.TrimSpace(params.HoldID)
	if holdID == "" {
		return nil, nil, nil, nil, nil, fmt.Errorf("wallet_repo: capture requires hold_id")
	}
	toUserID := strings.TrimSpace(params.ToUserID)
	if toUserID == "" {
		return nil, nil, nil, nil, nil, fmt.Errorf("wallet_repo: capture requires to_user_id")
	}

	hold, err := loadHoldForUpdate(ctx, tx, holdID)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	if hold.Status != "active" {
		return nil, nil, nil, nil, nil, ErrHoldNotActive
	}
	if hold.UserID == toUserID {
		return nil, nil, nil, nil, nil, fmt.Errorf("wallet_repo: capture cannot transfer to self")
	}

	// Step 1: refund the hold so the buyer's spendable balance is restored
	// before we re-debit them in the Transfer below.
	account, err := loadWalletAccountForUpdate(ctx, tx, hold.UserID)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	account.BalanceMinor += hold.AmountMinor
	if err := updateWalletAccountBalance(ctx, tx, account); err != nil {
		return nil, nil, nil, nil, nil, err
	}

	releaseIdem := ""
	if base := strings.TrimSpace(params.IdempotencyKeyBase); base != "" {
		releaseIdem = base + ":capture_release"
	}
	if _, err := insertWalletLedgerEntry(ctx, tx, walletLedgerInsertParams{
		AccountID:         hold.AccountID,
		EntryType:         "wallet_hold_capture_release",
		AmountMinor:       hold.AmountMinor,
		BalanceAfterMinor: account.BalanceMinor,
		Currency:          hold.Currency,
		ReferenceType:     "wallet_hold",
		ReferenceID:       hold.ID,
		CreatedByUserID:   strings.TrimSpace(params.CreatedByUserID),
		Metadata:          hold.Metadata,
		IdempotencyKey:    releaseIdem,
	}); err != nil {
		return nil, nil, nil, nil, nil, err
	}

	debitIdem := ""
	creditIdem := ""
	if base := strings.TrimSpace(params.IdempotencyKeyBase); base != "" {
		debitIdem = base + ":capture_debit"
		creditIdem = base + ":capture_credit"
	}

	debitType := strings.TrimSpace(params.DebitEntryType)
	if debitType == "" {
		debitType = "marketplace_purchase"
	}
	creditType := strings.TrimSpace(params.CreditEntryType)
	if creditType == "" {
		creditType = "marketplace_sale"
	}

	fromAccount, toAccount, debitEntry, creditEntry, err := r.TransferWithTx(ctx, tx, WalletTransferParams{
		FromUserID:        hold.UserID,
		ToUserID:          toUserID,
		AmountMinor:       hold.AmountMinor,
		Currency:          hold.Currency,
		DebitEntryType:    debitType,
		CreditEntryType:   creditType,
		ReferenceType:     strings.TrimSpace(params.ReferenceType),
		ReferenceID:       strings.TrimSpace(params.ReferenceID),
		CreatedByUserID:   strings.TrimSpace(params.CreatedByUserID),
		DebitMetadata:     params.DebitMetadata,
		CreditMetadata:    params.CreditMetadata,
		DebitIdempotency:  debitIdem,
		CreditIdempotency: creditIdem,
	})
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE wallet_holds
		   SET status = 'captured',
		       captured_at = NOW(),
		       captured_to_user_id = $2,
		       updated_at = NOW()
		 WHERE id = $1
	`, hold.ID, toUserID); err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("wallet_repo: mark hold captured: %w", err)
	}
	hold.Status = "captured"
	hold.CapturedToUserID = sql.NullString{String: toUserID, Valid: true}
	hold.CapturedAt = sql.NullTime{Time: time.Now(), Valid: true}

	return hold, fromAccount, toAccount, debitEntry, creditEntry, nil
}

// GetHold returns the current state of a hold (no locking). Used by
// settlement workers to read hold state outside the auction tx.
func (r *WalletRepo) GetHold(ctx context.Context, holdID string) (*WalletHold, error) {
	hold := &WalletHold{}
	err := r.db.QueryRowContext(ctx, holdSelectColumns+` FROM wallet_holds WHERE id = $1`, strings.TrimSpace(holdID)).Scan(
		&hold.ID, &hold.AccountID, &hold.UserID, &hold.AmountMinor, &hold.Currency, &hold.Status,
		&hold.ReferenceType, &hold.ReferenceID, &hold.CapturedToUserID, &hold.CapturedAt,
		&hold.ReleasedAt, &hold.IdempotencyKey, &hold.Metadata, &hold.CreatedAt, &hold.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrHoldNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("wallet_repo: get hold: %w", err)
	}
	return hold, nil
}

const holdSelectColumns = `SELECT id, account_id, user_id, amount_minor, currency, status,
		       reference_type, reference_id, captured_to_user_id, captured_at,
		       released_at, idempotency_key, metadata, created_at, updated_at`

func loadHoldForUpdate(ctx context.Context, tx *sql.Tx, holdID string) (*WalletHold, error) {
	hold := &WalletHold{}
	err := tx.QueryRowContext(ctx, holdSelectColumns+` FROM wallet_holds WHERE id = $1 FOR UPDATE`, strings.TrimSpace(holdID)).Scan(
		&hold.ID, &hold.AccountID, &hold.UserID, &hold.AmountMinor, &hold.Currency, &hold.Status,
		&hold.ReferenceType, &hold.ReferenceID, &hold.CapturedToUserID, &hold.CapturedAt,
		&hold.ReleasedAt, &hold.IdempotencyKey, &hold.Metadata, &hold.CreatedAt, &hold.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrHoldNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("wallet_repo: load hold for update: %w", err)
	}
	return hold, nil
}

func loadHoldByIdempotencyKey(ctx context.Context, tx *sql.Tx, key string) (*WalletHold, error) {
	hold := &WalletHold{}
	err := tx.QueryRowContext(ctx, holdSelectColumns+` FROM wallet_holds WHERE idempotency_key = $1`, key).Scan(
		&hold.ID, &hold.AccountID, &hold.UserID, &hold.AmountMinor, &hold.Currency, &hold.Status,
		&hold.ReferenceType, &hold.ReferenceID, &hold.CapturedToUserID, &hold.CapturedAt,
		&hold.ReleasedAt, &hold.IdempotencyKey, &hold.Metadata, &hold.CreatedAt, &hold.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrHoldNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("wallet_repo: load hold by idempotency: %w", err)
	}
	return hold, nil
}

func loadHoldLedgerEntry(ctx context.Context, tx *sql.Tx, holdID, entryType string) (*WalletLedgerEntry, error) {
	entry := &WalletLedgerEntry{}
	err := tx.QueryRowContext(ctx, `
		SELECT id, account_id, entry_type, amount_minor, balance_after_minor, currency,
		       reference_type, reference_id, created_by_user_id, metadata, created_at
		  FROM wallet_ledger_entries
		 WHERE reference_type = 'wallet_hold'
		   AND reference_id = $1
		   AND entry_type = $2
		 ORDER BY created_at ASC
		 LIMIT 1
	`, holdID, entryType).Scan(
		&entry.ID, &entry.AccountID, &entry.EntryType, &entry.AmountMinor, &entry.BalanceAfterMinor,
		&entry.Currency, &entry.ReferenceType, &entry.ReferenceID, &entry.CreatedByUserID, &entry.Metadata, &entry.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		// Some idempotent flows may legitimately have no matching ledger
		// row (e.g. a hold that was created in a torn write and then
		// replayed). Callers should treat nil as "no original entry".
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("wallet_repo: load hold ledger entry: %w", err)
	}
	return entry, nil
}

// mergeWalletReasonMetadata returns metadata with `reason` injected at the
// top level. Used by ReleaseHold so the audit row records *why* the hold
// was released (e.g. "outbid", "auction_cancelled") without forcing callers
// to construct the full JSON object themselves.
func mergeWalletReasonMetadata(raw json.RawMessage, reason string) json.RawMessage {
	if reason == "" {
		return raw
	}
	out := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &out); err != nil {
			// Replace unparseable metadata wholesale rather than risk
			// embedding garbage in the audit trail.
			out = map[string]any{}
		}
	}
	out["release_reason"] = reason
	marshaled, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	return marshaled
}
