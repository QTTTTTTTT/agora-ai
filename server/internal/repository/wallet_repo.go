package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

var (
	ErrInsufficientBalance  = errors.New("repository: insufficient balance")
	ErrIdempotencyConflict  = errors.New("repository: idempotency key conflict")
)

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505). Used to map duplicate idempotency-key inserts
// to the typed ErrIdempotencyConflict so business code can branch on it.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr.Code == "23505"
	}
	return false
}

type WalletAccount struct {
	ID           string    `json:"id"`
	UserID       string    `json:"user_id"`
	BaseCurrency string    `json:"base_currency"`
	BalanceMinor int64     `json:"balance_minor"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type WalletLedgerEntry struct {
	ID                string          `json:"id"`
	AccountID         string          `json:"account_id"`
	EntryType         string          `json:"entry_type"`
	AmountMinor       int64           `json:"amount_minor"`
	BalanceAfterMinor int64           `json:"balance_after_minor"`
	Currency          string          `json:"currency"`
	ReferenceType     sql.NullString  `json:"reference_type"`
	ReferenceID       sql.NullString  `json:"reference_id"`
	CreatedByUserID   sql.NullString  `json:"created_by_user_id"`
	Metadata          json.RawMessage `json:"metadata"`
	CreatedAt         time.Time       `json:"created_at"`
}

type WalletCreditParams struct {
	UserID          string
	AmountMinor     int64
	Currency        string
	ReferenceType   string
	ReferenceID     string
	CreatedByUserID string
	Metadata        json.RawMessage
	IdempotencyKey  string
}

type WalletTransferParams struct {
	FromUserID         string
	ToUserID           string
	AmountMinor        int64
	Currency           string
	DebitEntryType     string
	CreditEntryType    string
	ReferenceType      string
	ReferenceID        string
	CreatedByUserID    string
	DebitMetadata      json.RawMessage
	CreditMetadata     json.RawMessage
	DebitIdempotency   string
	CreditIdempotency  string
}

type WalletRepo struct {
	db *sql.DB
}

type walletQueryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func NewWalletRepo(db *sql.DB) *WalletRepo {
	return &WalletRepo{db: db}
}

func (r *WalletRepo) GetOrCreateByUserID(ctx context.Context, userID string) (*WalletAccount, error) {
	if err := ensureWalletAccount(ctx, r.db, userID); err != nil {
		return nil, err
	}
	return r.GetByUserID(ctx, userID)
}

func (r *WalletRepo) GetByUserID(ctx context.Context, userID string) (*WalletAccount, error) {
	account := &WalletAccount{}
	err := r.db.QueryRowContext(ctx, `
		SELECT id, user_id, base_currency, balance_minor, created_at, updated_at
		FROM wallet_accounts
		WHERE user_id = $1
	`, userID).Scan(&account.ID, &account.UserID, &account.BaseCurrency, &account.BalanceMinor, &account.CreatedAt, &account.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("wallet_repo: get by user id: %w", err)
	}
	return account, nil
}

func (r *WalletRepo) ListLedgerByUserID(ctx context.Context, userID string, offset, limit int) ([]WalletLedgerEntry, int, error) {
	var total int
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM wallet_ledger_entries e
		JOIN wallet_accounts a ON a.id = e.account_id
		WHERE a.user_id = $1
	`, userID).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("wallet_repo: count ledger by user id: %w", err)
	}

	rows, err := r.db.QueryContext(ctx, `
		SELECT e.id, e.account_id, e.entry_type, e.amount_minor, e.balance_after_minor, e.currency,
		       e.reference_type, e.reference_id, e.created_by_user_id, e.metadata, e.created_at
		FROM wallet_ledger_entries e
		JOIN wallet_accounts a ON a.id = e.account_id
		WHERE a.user_id = $1
		ORDER BY e.created_at DESC, e.id DESC
		OFFSET $2 LIMIT $3
	`, userID, offset, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("wallet_repo: list ledger by user id: %w", err)
	}
	defer rows.Close()

	entries := make([]WalletLedgerEntry, 0)
	for rows.Next() {
		var entry WalletLedgerEntry
		if err := rows.Scan(
			&entry.ID,
			&entry.AccountID,
			&entry.EntryType,
			&entry.AmountMinor,
			&entry.BalanceAfterMinor,
			&entry.Currency,
			&entry.ReferenceType,
			&entry.ReferenceID,
			&entry.CreatedByUserID,
			&entry.Metadata,
			&entry.CreatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("wallet_repo: scan ledger row: %w", err)
		}
		entries = append(entries, entry)
	}
	return entries, total, rows.Err()
}

func (r *WalletRepo) Credit(ctx context.Context, params WalletCreditParams) (*WalletAccount, *WalletLedgerEntry, error) {
	if params.AmountMinor <= 0 {
		return nil, nil, fmt.Errorf("wallet_repo: credit amount must be positive")
	}
	currency := normalizeWalletCurrency(params.Currency)
	metadata, err := marshalWalletMetadata(params.Metadata)
	if err != nil {
		return nil, nil, fmt.Errorf("wallet_repo: marshal metadata: %w", err)
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("wallet_repo: begin credit tx: %w", err)
	}
	defer tx.Rollback()

	account, err := ensureWalletAccountForUpdate(ctx, tx, strings.TrimSpace(params.UserID))
	if err != nil {
		return nil, nil, err
	}
	account.BalanceMinor += params.AmountMinor
	if err := updateWalletAccountBalance(ctx, tx, account); err != nil {
		return nil, nil, err
	}
	entry, err := insertWalletLedgerEntry(ctx, tx, walletLedgerInsertParams{
		AccountID:         account.ID,
		EntryType:         "recharge",
		AmountMinor:       params.AmountMinor,
		BalanceAfterMinor: account.BalanceMinor,
		Currency:          currency,
		ReferenceType:     params.ReferenceType,
		ReferenceID:       params.ReferenceID,
		CreatedByUserID:   params.CreatedByUserID,
		Metadata:          metadata,
		IdempotencyKey:    strings.TrimSpace(params.IdempotencyKey),
	})
	if err != nil {
		return nil, nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("wallet_repo: commit credit tx: %w", err)
	}
	return account, entry, nil
}

func (r *WalletRepo) Transfer(ctx context.Context, params WalletTransferParams) (*WalletAccount, *WalletAccount, *WalletLedgerEntry, *WalletLedgerEntry, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("wallet_repo: begin transfer tx: %w", err)
	}
	defer tx.Rollback()

	from, to, debit, credit, err := r.TransferWithTx(ctx, tx, params)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("wallet_repo: commit transfer tx: %w", err)
	}
	return from, to, debit, credit, nil
}

// TransferWithTx is the in-transaction variant. Callers (notably the
// marketplace purchase flow) MUST pass an active *sql.Tx so that the wallet
// movement and the surrounding business effects (agent clone, listing
// transition, order finalisation) commit or roll back together.
//
// On ErrInsufficientBalance the caller is expected to roll back the outer
// transaction; the function does not commit anything itself.
func (r *WalletRepo) TransferWithTx(ctx context.Context, tx *sql.Tx, params WalletTransferParams) (*WalletAccount, *WalletAccount, *WalletLedgerEntry, *WalletLedgerEntry, error) {
	if tx == nil {
		return nil, nil, nil, nil, ErrNoTx
	}
	if params.AmountMinor <= 0 {
		return nil, nil, nil, nil, fmt.Errorf("wallet_repo: transfer amount must be positive")
	}
	fromUserID := strings.TrimSpace(params.FromUserID)
	toUserID := strings.TrimSpace(params.ToUserID)
	if fromUserID == "" || toUserID == "" || fromUserID == toUserID {
		return nil, nil, nil, nil, fmt.Errorf("wallet_repo: transfer users must be different and non-empty")
	}
	currency := normalizeWalletCurrency(params.Currency)
	debitEntryType := strings.TrimSpace(params.DebitEntryType)
	if debitEntryType == "" {
		debitEntryType = "marketplace_purchase"
	}
	creditEntryType := strings.TrimSpace(params.CreditEntryType)
	if creditEntryType == "" {
		creditEntryType = "marketplace_sale"
	}
	debitMetadata, err := marshalWalletMetadata(params.DebitMetadata)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("wallet_repo: marshal debit metadata: %w", err)
	}
	creditMetadata, err := marshalWalletMetadata(params.CreditMetadata)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("wallet_repo: marshal credit metadata: %w", err)
	}

	if err := ensureWalletAccount(ctx, tx, fromUserID); err != nil {
		return nil, nil, nil, nil, err
	}
	if err := ensureWalletAccount(ctx, tx, toUserID); err != nil {
		return nil, nil, nil, nil, err
	}

	// Lock both rows in a deterministic order to avoid deadlocks when two
	// transfers target the same pair of accounts in opposite directions.
	firstUserID := fromUserID
	secondUserID := toUserID
	if secondUserID < firstUserID {
		firstUserID, secondUserID = secondUserID, firstUserID
	}
	firstAccount, err := loadWalletAccountForUpdate(ctx, tx, firstUserID)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	secondAccount, err := loadWalletAccountForUpdate(ctx, tx, secondUserID)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	accounts := map[string]*WalletAccount{
		firstUserID:  firstAccount,
		secondUserID: secondAccount,
	}
	fromAccount := accounts[fromUserID]
	toAccount := accounts[toUserID]
	if fromAccount.BalanceMinor < params.AmountMinor {
		return nil, nil, nil, nil, ErrInsufficientBalance
	}

	fromAccount.BalanceMinor -= params.AmountMinor
	toAccount.BalanceMinor += params.AmountMinor
	if err := updateWalletAccountBalance(ctx, tx, fromAccount); err != nil {
		return nil, nil, nil, nil, err
	}
	if err := updateWalletAccountBalance(ctx, tx, toAccount); err != nil {
		return nil, nil, nil, nil, err
	}

	debitEntry, err := insertWalletLedgerEntry(ctx, tx, walletLedgerInsertParams{
		AccountID:         fromAccount.ID,
		EntryType:         debitEntryType,
		AmountMinor:       -params.AmountMinor,
		BalanceAfterMinor: fromAccount.BalanceMinor,
		Currency:          currency,
		ReferenceType:     params.ReferenceType,
		ReferenceID:       params.ReferenceID,
		CreatedByUserID:   params.CreatedByUserID,
		Metadata:          debitMetadata,
		IdempotencyKey:    strings.TrimSpace(params.DebitIdempotency),
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	creditEntry, err := insertWalletLedgerEntry(ctx, tx, walletLedgerInsertParams{
		AccountID:         toAccount.ID,
		EntryType:         creditEntryType,
		AmountMinor:       params.AmountMinor,
		BalanceAfterMinor: toAccount.BalanceMinor,
		Currency:          currency,
		ReferenceType:     params.ReferenceType,
		ReferenceID:       params.ReferenceID,
		CreatedByUserID:   params.CreatedByUserID,
		Metadata:          creditMetadata,
		IdempotencyKey:    strings.TrimSpace(params.CreditIdempotency),
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return fromAccount, toAccount, debitEntry, creditEntry, nil
}

type walletLedgerInsertParams struct {
	AccountID         string
	EntryType         string
	AmountMinor       int64
	BalanceAfterMinor int64
	Currency          string
	ReferenceType     string
	ReferenceID       string
	CreatedByUserID   string
	Metadata          []byte
	IdempotencyKey    string
}

func ensureWalletAccount(ctx context.Context, q walletQueryer, userID string) error {
	trimmed := strings.TrimSpace(userID)
	if trimmed == "" {
		return fmt.Errorf("wallet_repo: user id is required")
	}
	if _, err := q.ExecContext(ctx, `
		INSERT INTO wallet_accounts (user_id)
		VALUES ($1)
		ON CONFLICT (user_id) DO NOTHING
	`, trimmed); err != nil {
		return fmt.Errorf("wallet_repo: ensure account: %w", err)
	}
	return nil
}

func ensureWalletAccountForUpdate(ctx context.Context, tx *sql.Tx, userID string) (*WalletAccount, error) {
	if err := ensureWalletAccount(ctx, tx, userID); err != nil {
		return nil, err
	}
	return loadWalletAccountForUpdate(ctx, tx, userID)
}

func loadWalletAccountForUpdate(ctx context.Context, tx *sql.Tx, userID string) (*WalletAccount, error) {
	account := &WalletAccount{}
	err := tx.QueryRowContext(ctx, `
		SELECT id, user_id, base_currency, balance_minor, created_at, updated_at
		FROM wallet_accounts
		WHERE user_id = $1
		FOR UPDATE
	`, strings.TrimSpace(userID)).Scan(&account.ID, &account.UserID, &account.BaseCurrency, &account.BalanceMinor, &account.CreatedAt, &account.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("wallet_repo: load account for update: %w", err)
	}
	return account, nil
}

func updateWalletAccountBalance(ctx context.Context, tx *sql.Tx, account *WalletAccount) error {
	if account == nil {
		return fmt.Errorf("wallet_repo: account is required")
	}
	if err := tx.QueryRowContext(ctx, `
		UPDATE wallet_accounts
		SET balance_minor = $1, updated_at = NOW()
		WHERE id = $2
		RETURNING updated_at
	`, account.BalanceMinor, account.ID).Scan(&account.UpdatedAt); err != nil {
		return fmt.Errorf("wallet_repo: update account balance: %w", err)
	}
	return nil
}

func insertWalletLedgerEntry(ctx context.Context, tx *sql.Tx, params walletLedgerInsertParams) (*WalletLedgerEntry, error) {
	entry := &WalletLedgerEntry{}
	err := tx.QueryRowContext(ctx, `
		INSERT INTO wallet_ledger_entries (
			account_id,
			entry_type,
			amount_minor,
			balance_after_minor,
			currency,
			reference_type,
			reference_id,
			created_by_user_id,
			metadata,
			idempotency_key
		)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, ''), NULLIF($8, '')::uuid, $9, NULLIF($10, ''))
		RETURNING id, account_id, entry_type, amount_minor, balance_after_minor, currency, reference_type, reference_id, created_by_user_id, metadata, created_at
	`, params.AccountID, strings.TrimSpace(params.EntryType), params.AmountMinor, params.BalanceAfterMinor, normalizeWalletCurrency(params.Currency), strings.TrimSpace(params.ReferenceType), strings.TrimSpace(params.ReferenceID), strings.TrimSpace(params.CreatedByUserID), params.Metadata, strings.TrimSpace(params.IdempotencyKey)).Scan(
		&entry.ID,
		&entry.AccountID,
		&entry.EntryType,
		&entry.AmountMinor,
		&entry.BalanceAfterMinor,
		&entry.Currency,
		&entry.ReferenceType,
		&entry.ReferenceID,
		&entry.CreatedByUserID,
		&entry.Metadata,
		&entry.CreatedAt,
	)
	if err != nil {
		// Map the unique-violation on idempotency_key into a typed error so
		// callers (PR-02 marketplace flow) can short-circuit safely instead
		// of treating it as a generic write failure.
		if isUniqueViolation(err) {
			return nil, ErrIdempotencyConflict
		}
		return nil, fmt.Errorf("wallet_repo: insert ledger entry: %w", err)
	}
	return entry, nil
}

func normalizeWalletCurrency(value string) string {
	trimmed := strings.ToUpper(strings.TrimSpace(value))
	if trimmed == "" {
		return "USD"
	}
	return trimmed
}

func marshalWalletMetadata(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return []byte(`{}`), nil
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("invalid JSON payload")
	}
	if string(raw) == "null" {
		return []byte(`{}`), nil
	}
	return raw, nil
}
