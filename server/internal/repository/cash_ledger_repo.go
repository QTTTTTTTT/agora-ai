// Cash ledger repository (P1-1).
//
// Owns the append-only journal of every cash movement on a fund.
// The schema is documented in migrations/056_cash_ledger.sql; this
// file is the Go façade that the trading engine, corpaction
// applier, and HTTP read API talk to.
//
// Invariants the repo enforces
//
//   - Append-only: there is no Update / Delete on the public API.
//     Corrections post a NEW row of type 'reversal' or 'adjustment'.
//   - Idempotency: when the caller supplies an idempotency_key,
//     the partial UNIQUE index (fund_id, idempotency_key) collapses
//     duplicate writes into a single row. We treat the resulting
//     UNIQUE_VIOLATION as success — see Append below.
//   - Signed amount convention: positive credits, negative debits.
//     The runtime engine MUST submit signed values; we do NOT
//     auto-flip based on entry_type so a typo of "trade_buy_notional
//     amount=+100" is loud (it inflates SUM) instead of silently
//     "corrected".
//
// What we deliberately do NOT do here
//
//   - Reconciliation against funds.current_capital. That's a
//     separate batch job (BalanceByFund + read on funds row) so the
//     repo stays a write/read primitive.
//   - Currency conversion. Every row stores the original currency
//     (default fund's base). FX (P1-4) lives elsewhere.

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

// ErrCashLedgerEntryInvalid is returned when the entry would
// violate a CHECK constraint we can detect before the round-trip
// (zero amount, unknown type, missing fund_id, etc.).
var ErrCashLedgerEntryInvalid = errors.New("cash_ledger_repo: invalid entry")

// EntryType constants mirror migration 056's CHECK. Callers MUST
// use these instead of string literals so a vocabulary change
// surfaces at compile time.
const (
	CashEntryTradeBuyNotional   = "trade_buy_notional"
	CashEntryTradeBuyCommission = "trade_buy_commission"
	CashEntryTradeBuyTransfer   = "trade_buy_transfer_fee"
	CashEntryTradeBuyStampTax   = "trade_buy_stamp_tax"

	CashEntryTradeSellNotional   = "trade_sell_notional"
	CashEntryTradeSellCommission = "trade_sell_commission"
	CashEntryTradeSellTransfer   = "trade_sell_transfer_fee"
	CashEntryTradeSellStampTax   = "trade_sell_stamp_tax"

	CashEntryDividendCash    = "dividend_cash"
	CashEntryFeeManagement   = "fee_management"
	CashEntryFeePerformance  = "fee_performance"
	CashEntryFeePlatform     = "fee_platform"
	CashEntryFundingDeposit  = "funding_deposit"
	CashEntryFundingWithdraw = "funding_withdrawal"
	CashEntryAdjustment      = "adjustment"
	CashEntryReversal        = "reversal"
	// S6.4 short-borrow lifecycle:
	//   - locate_fee: one-time charge at order entry when the
	//     pre-trade locate gate accepts a short open.
	//   - borrow_fee: daily charge while the short position is
	//     held, posted by the borrow-accrual loop at EOD.
	// Both are debits (cash out). The borrow ledger
	// (short_position_borrow_ledger) is the forensic record;
	// cash_ledger_entries is the cash movement.
	CashEntryLocateFee = "locate_fee"
	CashEntryBorrowFee = "borrow_fee"
)

// validEntryTypes is the closed vocabulary the repo accepts. Kept
// in sync with migration 056 + the constants above. Adding a new
// type means: ALTER the CHECK, add a constant, append here.
var validEntryTypes = map[string]bool{
	CashEntryTradeBuyNotional:    true,
	CashEntryTradeBuyCommission:  true,
	CashEntryTradeBuyTransfer:    true,
	CashEntryTradeBuyStampTax:    true,
	CashEntryTradeSellNotional:   true,
	CashEntryTradeSellCommission: true,
	CashEntryTradeSellTransfer:   true,
	CashEntryTradeSellStampTax:   true,
	CashEntryDividendCash:        true,
	CashEntryFeeManagement:       true,
	CashEntryFeePerformance:      true,
	CashEntryFeePlatform:         true,
	CashEntryFundingDeposit:      true,
	CashEntryFundingWithdraw:     true,
	CashEntryAdjustment:          true,
	CashEntryReversal:            true,
	CashEntryLocateFee:           true,
	CashEntryBorrowFee:           true,
}

// CashLedgerEntry is the persisted shape, returned by reads.
// All fields are always populated except the optional
// FK-style links and the free-form metadata blob.
type CashLedgerEntry struct {
	ID             string
	FundID         string
	PostedAt       time.Time
	TradingDate    sql.NullTime
	EntryType      string
	Amount         float64
	Currency       string
	TradeID        sql.NullString
	PlanID         sql.NullString
	PlanActionID   sql.NullString
	CorpActionID   sql.NullString
	BrokerLinkID   sql.NullString
	Description    string
	Metadata       json.RawMessage
	CreatedBy      sql.NullString
	IdempotencyKey sql.NullString
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// AppendParams carries the inputs for a single Append. Optional
// fields are pointers / sql.Null* so the caller can skip them
// without dragging zero values through the SQL.
type AppendParams struct {
	FundID         string
	PostedAt       time.Time // zero → NOW()
	TradingDate    *time.Time
	EntryType      string
	Amount         float64
	Currency       string // empty → "USD"
	TradeID        string
	PlanID         string
	PlanActionID   string
	CorpActionID   string
	BrokerLinkID   string
	Description    string
	Metadata       map[string]any
	CreatedBy      string
	IdempotencyKey string
}

// dbExecQuerier is the small subset of *sql.DB / *sql.Tx the
// cash ledger needs. Letting callers pass a Tx makes the journal
// safely co-commit with the trade row that triggered it — a
// trade insert and its 4 ledger rows MUST be one transaction.
type dbExecQuerier interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

// CashLedgerRepo is the read/write façade. The database handle
// is captured at construction; all mutating methods accept an
// optional Tx via AppendTx for the trade-engine's all-or-nothing
// flow.
type CashLedgerRepo struct {
	db *sql.DB
}

// NewCashLedgerRepo returns a repo bound to db. Nil-safe — a nil
// db produces a repo whose methods all return ErrNotConfigured.
func NewCashLedgerRepo(db *sql.DB) *CashLedgerRepo {
	return &CashLedgerRepo{db: db}
}

// Append inserts a single ledger entry. Returns the freshly
// allocated id. If an idempotency_key collides with an existing
// row for the same fund, the call is a no-op and the EXISTING
// id is returned — making retries safe.
func (r *CashLedgerRepo) Append(ctx context.Context, p AppendParams) (string, error) {
	if r == nil || r.db == nil {
		return "", fmt.Errorf("cash_ledger_repo: nil db")
	}
	return r.appendTx(ctx, r.db, p)
}

// AppendTx is the transactional variant. The caller is
// responsible for Commit / Rollback. Use this when the ledger
// write must atomically co-commit with another DB mutation
// (insert into trade_executions, update of corp_action_applications).
func (r *CashLedgerRepo) AppendTx(ctx context.Context, tx *sql.Tx, p AppendParams) (string, error) {
	if r == nil {
		return "", fmt.Errorf("cash_ledger_repo: nil repo")
	}
	if tx == nil {
		return "", fmt.Errorf("cash_ledger_repo: nil tx")
	}
	return r.appendTx(ctx, tx, p)
}

func (r *CashLedgerRepo) appendTx(ctx context.Context, dbq dbExecQuerier, p AppendParams) (string, error) {
	if err := validateAppend(&p); err != nil {
		return "", err
	}

	currency := strings.ToUpper(strings.TrimSpace(p.Currency))
	if currency == "" {
		currency = "USD"
	}

	var metadataBytes []byte
	if len(p.Metadata) > 0 {
		b, err := json.Marshal(p.Metadata)
		if err != nil {
			return "", fmt.Errorf("cash_ledger_repo: marshal metadata: %w", err)
		}
		metadataBytes = b
	}

	// We use INSERT ... ON CONFLICT DO NOTHING so duplicate
	// idempotency_keys silently coalesce. RETURNING id only
	// fires on a successful insert — when the conflict path
	// runs we follow up with a SELECT to recover the existing
	// row's id. Two-statement design keeps the happy path
	// single-round-trip.
	const insertQ = `
		INSERT INTO cash_ledger (
		    fund_id, posted_at, trading_date, entry_type,
		    amount, currency, trade_id, plan_id,
		    plan_action_id, corp_action_id, broker_link_id,
		    description, metadata, created_by, idempotency_key
		) VALUES (
		    $1,
		    COALESCE($2, NOW()),
		    $3, $4, $5, $6,
		    NULLIF($7, '')::uuid,
		    NULLIF($8, '')::uuid,
		    NULLIF($9, '')::uuid,
		    NULLIF($10, '')::uuid,
		    NULLIF($11, '')::uuid,
		    $12,
		    COALESCE($13::jsonb, '{}'::jsonb),
		    NULLIF($14, '')::uuid,
		    NULLIF($15, '')
		)
		ON CONFLICT (fund_id, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		RETURNING id`

	var postedAt any
	if !p.PostedAt.IsZero() {
		postedAt = p.PostedAt.UTC()
	}
	var tradingDate any
	if p.TradingDate != nil {
		tradingDate = p.TradingDate.UTC()
	}

	var id string
	row := dbq.QueryRowContext(ctx, insertQ,
		p.FundID, postedAt, tradingDate, p.EntryType,
		p.Amount, currency,
		p.TradeID, p.PlanID, p.PlanActionID,
		p.CorpActionID, p.BrokerLinkID,
		p.Description, metadataBytes, p.CreatedBy, p.IdempotencyKey,
	)
	if err := row.Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Conflict path — pull the existing row's id.
			if p.IdempotencyKey == "" {
				return "", fmt.Errorf("cash_ledger_repo: insert returned no row")
			}
			existing, lookupErr := r.findByIdempotency(ctx, dbq, p.FundID, p.IdempotencyKey)
			if lookupErr != nil {
				return "", lookupErr
			}
			return existing, nil
		}
		return "", fmt.Errorf("cash_ledger_repo: insert: %w", err)
	}
	return id, nil
}

func (r *CashLedgerRepo) findByIdempotency(ctx context.Context, dbq dbExecQuerier, fundID, key string) (string, error) {
	var id string
	err := dbq.QueryRowContext(ctx,
		`SELECT id FROM cash_ledger WHERE fund_id = $1 AND idempotency_key = $2 LIMIT 1`,
		fundID, key,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("cash_ledger_repo: lookup by idempotency_key: %w", err)
	}
	return id, nil
}

func validateAppend(p *AppendParams) error {
	if p == nil {
		return fmt.Errorf("%w: nil params", ErrCashLedgerEntryInvalid)
	}
	if strings.TrimSpace(p.FundID) == "" {
		return fmt.Errorf("%w: empty fund_id", ErrCashLedgerEntryInvalid)
	}
	if !validEntryTypes[p.EntryType] {
		return fmt.Errorf("%w: unknown entry_type %q", ErrCashLedgerEntryInvalid, p.EntryType)
	}
	if p.Amount == 0 {
		return fmt.Errorf("%w: zero amount", ErrCashLedgerEntryInvalid)
	}
	return nil
}

// BalanceByFundParams scopes the SUM. From / To are optional
// (zero values mean unbounded). A nil pointer would have served
// equally well; we use zero-time-as-unbounded to keep the call
// site terser.
type BalanceByFundParams struct {
	From time.Time
	To   time.Time
}

// BalanceByFund returns SUM(amount) for the fund — i.e. the
// cash position implied by the journal. Used by reconciliation
// to compare against funds.current_capital.
func (r *CashLedgerRepo) BalanceByFund(ctx context.Context, fundID string, p BalanceByFundParams) (float64, error) {
	if r == nil || r.db == nil {
		return 0, fmt.Errorf("cash_ledger_repo: nil db")
	}
	if strings.TrimSpace(fundID) == "" {
		return 0, fmt.Errorf("cash_ledger_repo: empty fund_id")
	}
	q := `SELECT COALESCE(SUM(amount), 0) FROM cash_ledger WHERE fund_id = $1`
	args := []any{fundID}
	if !p.From.IsZero() {
		q += fmt.Sprintf(" AND posted_at >= $%d", len(args)+1)
		args = append(args, p.From.UTC())
	}
	if !p.To.IsZero() {
		q += fmt.Sprintf(" AND posted_at < $%d", len(args)+1)
		args = append(args, p.To.UTC())
	}
	var balance float64
	if err := r.db.QueryRowContext(ctx, q, args...).Scan(&balance); err != nil {
		return 0, fmt.Errorf("cash_ledger_repo: balance: %w", err)
	}
	return balance, nil
}

// ListByFundParams is the page-shape for ListByFund.
//
// Cursor pagination is keyed off (posted_at DESC, id DESC) — the
// repo accepts an opaque last-row marker (PostedAtBefore + IDBefore)
// rather than offset/limit so two consecutive pages are stable
// even as new entries land at the head.
type ListByFundParams struct {
	From            time.Time
	To              time.Time
	EntryTypes      []string
	Limit           int
	PostedAtBefore  time.Time
	IDBefore        string
}

// ListByFund returns up to `Limit` entries for the fund newest-
// first, optionally filtered by [from, to) and entry_types.
func (r *CashLedgerRepo) ListByFund(ctx context.Context, fundID string, p ListByFundParams) ([]CashLedgerEntry, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("cash_ledger_repo: nil db")
	}
	if strings.TrimSpace(fundID) == "" {
		return nil, fmt.Errorf("cash_ledger_repo: empty fund_id")
	}
	limit := p.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	q := `SELECT ` + cashLedgerSelectColumns + ` FROM cash_ledger WHERE fund_id = $1`
	args := []any{fundID}
	if !p.From.IsZero() {
		q += fmt.Sprintf(" AND posted_at >= $%d", len(args)+1)
		args = append(args, p.From.UTC())
	}
	if !p.To.IsZero() {
		q += fmt.Sprintf(" AND posted_at < $%d", len(args)+1)
		args = append(args, p.To.UTC())
	}
	if len(p.EntryTypes) > 0 {
		q += fmt.Sprintf(" AND entry_type = ANY($%d)", len(args)+1)
		args = append(args, pq.Array(p.EntryTypes))
	}
	if !p.PostedAtBefore.IsZero() && p.IDBefore != "" {
		// Compound cursor: rows older than (PostedAtBefore, IDBefore).
		// Compare on the tuple to keep ties consistent.
		q += fmt.Sprintf(" AND (posted_at, id) < ($%d, $%d::uuid)",
			len(args)+1, len(args)+2)
		args = append(args, p.PostedAtBefore.UTC(), p.IDBefore)
	}
	q += " ORDER BY posted_at DESC, id DESC LIMIT $" + fmt.Sprintf("%d", len(args)+1)
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("cash_ledger_repo: list: %w", err)
	}
	defer rows.Close()

	out := make([]CashLedgerEntry, 0, limit)
	for rows.Next() {
		entry, err := scanCashLedgerEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cash_ledger_repo: rows: %w", err)
	}
	return out, nil
}

// SubtotalByEntryType returns SUM(amount) grouped by entry_type
// for the fund inside [from, to). Used by the UI to render the
// commission/fee breakdown card on the fund-detail page.
func (r *CashLedgerRepo) SubtotalByEntryType(ctx context.Context, fundID string, from, to time.Time) (map[string]float64, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("cash_ledger_repo: nil db")
	}
	if strings.TrimSpace(fundID) == "" {
		return nil, fmt.Errorf("cash_ledger_repo: empty fund_id")
	}
	q := `SELECT entry_type, SUM(amount) FROM cash_ledger WHERE fund_id = $1`
	args := []any{fundID}
	if !from.IsZero() {
		q += fmt.Sprintf(" AND posted_at >= $%d", len(args)+1)
		args = append(args, from.UTC())
	}
	if !to.IsZero() {
		q += fmt.Sprintf(" AND posted_at < $%d", len(args)+1)
		args = append(args, to.UTC())
	}
	q += " GROUP BY entry_type"

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("cash_ledger_repo: subtotal: %w", err)
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var et string
		var amt float64
		if err := rows.Scan(&et, &amt); err != nil {
			return nil, fmt.Errorf("cash_ledger_repo: scan subtotal: %w", err)
		}
		out[et] = amt
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cash_ledger_repo: subtotal rows: %w", err)
	}
	return out, nil
}

// SubtotalByCurrency returns SUM(amount) grouped by currency for
// the period. Used by the FX-aware cash_ledger summary handler
// (P1-4) to compute a base-currency-denominated balance from a
// multi-currency journal: convert each currency bucket once,
// sum, done. The grouping is on the cash_ledger.currency column
// (set per insert) so the math doesn't depend on whether
// individual entry_types happen to be USD-only.
func (r *CashLedgerRepo) SubtotalByCurrency(ctx context.Context, fundID string, from, to time.Time) (map[string]float64, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("cash_ledger_repo: nil db")
	}
	if strings.TrimSpace(fundID) == "" {
		return nil, fmt.Errorf("cash_ledger_repo: empty fund_id")
	}
	q := `SELECT currency, SUM(amount) FROM cash_ledger WHERE fund_id = $1`
	args := []any{fundID}
	if !from.IsZero() {
		q += fmt.Sprintf(" AND posted_at >= $%d", len(args)+1)
		args = append(args, from.UTC())
	}
	if !to.IsZero() {
		q += fmt.Sprintf(" AND posted_at < $%d", len(args)+1)
		args = append(args, to.UTC())
	}
	q += " GROUP BY currency"

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("cash_ledger_repo: subtotal_by_currency: %w", err)
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var cur string
		var amt float64
		if err := rows.Scan(&cur, &amt); err != nil {
			return nil, fmt.Errorf("cash_ledger_repo: scan currency: %w", err)
		}
		if strings.TrimSpace(cur) == "" {
			cur = "USD"
		}
		out[strings.ToUpper(cur)] = amt
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cash_ledger_repo: subtotal_by_currency rows: %w", err)
	}
	return out, nil
}

const cashLedgerSelectColumns = `
    id, fund_id, posted_at, trading_date, entry_type,
    amount, currency, trade_id, plan_id, plan_action_id,
    corp_action_id, broker_link_id, description, metadata,
    created_by, idempotency_key, created_at, updated_at`

// scanCashLedgerEntry deserialises one row. Works for both
// *sql.Row and *sql.Rows because we use the package-local
// rowScanner type defined in backtest_repo.go.
func scanCashLedgerEntry(r rowScanner) (CashLedgerEntry, error) {
	var (
		e             CashLedgerEntry
		metadataBytes []byte
	)
	err := r.Scan(
		&e.ID, &e.FundID, &e.PostedAt, &e.TradingDate, &e.EntryType,
		&e.Amount, &e.Currency, &e.TradeID, &e.PlanID, &e.PlanActionID,
		&e.CorpActionID, &e.BrokerLinkID, &e.Description, &metadataBytes,
		&e.CreatedBy, &e.IdempotencyKey, &e.CreatedAt, &e.UpdatedAt,
	)
	if err != nil {
		return CashLedgerEntry{}, fmt.Errorf("cash_ledger_repo: scan: %w", err)
	}
	if len(metadataBytes) == 0 {
		e.Metadata = json.RawMessage(`{}`)
	} else {
		e.Metadata = json.RawMessage(metadataBytes)
	}
	return e, nil
}
