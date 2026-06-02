// Funding request repository (P1-2).
//
// Owns the CRUD + state-machine transitions for funding_requests:
// the queue of pending deposits / withdrawals waiting for 4-eye
// admin approval. Once approved, the handler tx-co-commits a
// cash_ledger row and a funds.current_capital UPDATE.
//
// Design notes
//
//   - The 4-eye check (approver != requester) is enforced both
//     by the handler (early 403 with explicit reason) and a
//     CHECK constraint on the table — the table CHECK is the
//     last line of defence against a code bug.
//
//   - Approval is implemented as a single atomic operation:
//     pending → approved + write cash_ledger + UPDATE funds in
//     one transaction. We expose ApproveTx to let the caller
//     own the tx and chain the cash_ledger insert, because the
//     cash_ledger column-set is too domain-specific to bury
//     inside this repo.

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
	// ErrFundingRequestNotFound is returned when the lookup id
	// matches no row.
	ErrFundingRequestNotFound = errors.New("funding_repo: not found")
	// ErrFundingRequestInvalid covers schema-level validation that
	// we want to catch before the round-trip (negative amount,
	// unknown direction, etc.).
	ErrFundingRequestInvalid = errors.New("funding_repo: invalid request")
	// ErrFundingRequestStateConflict is returned when the caller
	// asks for a transition the state machine doesn't allow
	// (e.g. approve a non-pending row).
	ErrFundingRequestStateConflict = errors.New("funding_repo: state conflict")
)

// Direction + status enums mirror the SQL CHECK; callers should
// use these constants instead of bare strings.
const (
	FundingDirectionDeposit    = "deposit"
	FundingDirectionWithdrawal = "withdrawal"
)

const (
	FundingStatusPending   = "pending"
	FundingStatusApproved  = "approved"
	FundingStatusRejected  = "rejected"
	FundingStatusCancelled = "cancelled"
	FundingStatusPosted    = "posted"
)

const (
	FundingMethodWire     = "wire"
	FundingMethodACH      = "ach"
	FundingMethodSEPA     = "sepa"
	FundingMethodCheck    = "check"
	FundingMethodInternal = "internal_transfer"
	FundingMethodManual   = "manual"
)

var validFundingMethods = map[string]bool{
	FundingMethodWire:     true,
	FundingMethodACH:      true,
	FundingMethodSEPA:     true,
	FundingMethodCheck:    true,
	FundingMethodInternal: true,
	FundingMethodManual:   true,
}

// FundingRequest is the persisted shape returned by lookups.
type FundingRequest struct {
	ID                 string
	FundID             string
	Direction          string
	Amount             float64
	Currency           string
	Method             string
	ExternalReference  sql.NullString
	Status             string
	RequestedBy        string
	ApprovedBy         sql.NullString
	ApprovedAt         sql.NullTime
	RejectedBy         sql.NullString
	RejectedAt         sql.NullTime
	RejectionReason    sql.NullString
	CancelledAt        sql.NullTime
	CashLedgerEntryID  sql.NullString
	Notes              sql.NullString
	Metadata           json.RawMessage
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// CreateParams is the inbound payload for a new pending request.
type CreateFundingRequestParams struct {
	FundID            string
	Direction         string
	Amount            float64
	Currency          string
	Method            string
	ExternalReference string
	RequestedBy       string
	Notes             string
	Metadata          map[string]any
}

// FundingRepo is the database façade. Both *sql.DB and *sql.Tx
// satisfy the same QueryContext / ExecContext / QueryRowContext
// signatures, so we expose Tx-aware methods (ApproveTx,
// RejectTx) the handler can use to compose with cash_ledger
// inserts atomically.
type FundingRepo struct {
	db *sql.DB
}

func NewFundingRepo(db *sql.DB) *FundingRepo {
	return &FundingRepo{db: db}
}

// Create inserts a new pending request and returns the row id.
func (r *FundingRepo) Create(ctx context.Context, p CreateFundingRequestParams) (string, error) {
	if r == nil || r.db == nil {
		return "", fmt.Errorf("funding_repo: nil db")
	}
	if err := validateCreate(&p); err != nil {
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
			return "", fmt.Errorf("funding_repo: marshal metadata: %w", err)
		}
		metadataBytes = b
	}
	var id string
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO funding_requests
		    (fund_id, direction, amount, currency, method,
		     external_reference, requested_by, notes, metadata)
		 VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, NULLIF($8, ''),
		         COALESCE($9::jsonb, '{}'::jsonb))
		 RETURNING id`,
		p.FundID, p.Direction, p.Amount, currency, p.Method,
		p.ExternalReference, p.RequestedBy, p.Notes, metadataBytes,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("funding_repo: create: %w", err)
	}
	return id, nil
}

// GetByID returns the row or ErrFundingRequestNotFound.
func (r *FundingRepo) GetByID(ctx context.Context, id string) (*FundingRequest, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("funding_repo: nil db")
	}
	q := `SELECT ` + fundingSelectColumns + ` FROM funding_requests WHERE id = $1`
	row := r.db.QueryRowContext(ctx, q, id)
	return scanFundingRequest(row)
}

// ListByFundParams scopes ListByFund. From / To unbounded when
// zero. EntryStatuses lets callers narrow to pending or any
// other set; empty = all.
type ListFundingByFundParams struct {
	Statuses []string
	From     time.Time
	To       time.Time
	Limit    int
}

// ListByFund returns up to Limit rows for the fund, newest-first.
func (r *FundingRepo) ListByFund(ctx context.Context, fundID string, p ListFundingByFundParams) ([]FundingRequest, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("funding_repo: nil db")
	}
	if strings.TrimSpace(fundID) == "" {
		return nil, fmt.Errorf("funding_repo: empty fund_id")
	}
	limit := p.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT ` + fundingSelectColumns + ` FROM funding_requests WHERE fund_id = $1`
	args := []any{fundID}
	if len(p.Statuses) > 0 {
		q += fmt.Sprintf(" AND status = ANY($%d)", len(args)+1)
		args = append(args, pq.Array(p.Statuses))
	}
	if !p.From.IsZero() {
		q += fmt.Sprintf(" AND created_at >= $%d", len(args)+1)
		args = append(args, p.From.UTC())
	}
	if !p.To.IsZero() {
		q += fmt.Sprintf(" AND created_at < $%d", len(args)+1)
		args = append(args, p.To.UTC())
	}
	q += fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d", len(args)+1)
	args = append(args, limit)
	return r.queryList(ctx, q, args...)
}

// ListPendingAdmin returns the platform-wide queue an admin
// reviews. Cap defaults to 200 to keep the page responsive.
func (r *FundingRepo) ListPendingAdmin(ctx context.Context, limit int) ([]FundingRequest, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("funding_repo: nil db")
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT ` + fundingSelectColumns + `
		FROM funding_requests
		WHERE status = 'pending'
		ORDER BY created_at ASC
		LIMIT $1`
	return r.queryList(ctx, q, limit)
}

func (r *FundingRepo) queryList(ctx context.Context, q string, args ...any) ([]FundingRequest, error) {
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("funding_repo: list: %w", err)
	}
	defer rows.Close()
	out := make([]FundingRequest, 0)
	for rows.Next() {
		fr, err := scanFundingRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *fr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("funding_repo: rows: %w", err)
	}
	return out, nil
}

// Cancel transitions a row from pending → cancelled, but ONLY if
// the caller is the original requester. Returns ErrFundingRequestStateConflict
// if the row isn't pending or the user isn't the requester.
func (r *FundingRepo) Cancel(ctx context.Context, id, userID string) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("funding_repo: nil db")
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE funding_requests
		    SET status = 'cancelled',
		        cancelled_at = NOW()
		  WHERE id = $1
		    AND status = 'pending'
		    AND requested_by = $2`,
		id, userID,
	)
	if err != nil {
		return fmt.Errorf("funding_repo: cancel: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("funding_repo: cancel rows: %w", err)
	}
	if n == 0 {
		return ErrFundingRequestStateConflict
	}
	return nil
}

// LookupForApprovalTx grabs the row inside the caller's tx with
// FOR UPDATE so two admins can't race the same approve.
func (r *FundingRepo) LookupForApprovalTx(ctx context.Context, tx *sql.Tx, id string) (*FundingRequest, error) {
	if tx == nil {
		return nil, fmt.Errorf("funding_repo: nil tx")
	}
	q := `SELECT ` + fundingSelectColumns + `
		FROM funding_requests WHERE id = $1 FOR UPDATE`
	row := tx.QueryRowContext(ctx, q, id)
	return scanFundingRequest(row)
}

// MarkApprovedTx flips a pending row to approved. Caller is
// responsible for verifying 4-eye + posting the cash_ledger row.
// We pass cashLedgerEntryID so the back-link lands in the same
// UPDATE.
func (r *FundingRepo) MarkApprovedTx(ctx context.Context, tx *sql.Tx, id, approverID, cashLedgerEntryID string) error {
	if tx == nil {
		return fmt.Errorf("funding_repo: nil tx")
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE funding_requests
		    SET status = 'approved',
		        approved_by = $2,
		        approved_at = NOW(),
		        cash_ledger_entry_id = NULLIF($3, '')::uuid
		  WHERE id = $1
		    AND status = 'pending'`,
		id, approverID, cashLedgerEntryID,
	)
	if err != nil {
		return fmt.Errorf("funding_repo: mark approved: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("funding_repo: mark approved rows: %w", err)
	}
	if n == 0 {
		return ErrFundingRequestStateConflict
	}
	return nil
}

// MarkRejectedTx flips a pending row to rejected, capturing the
// rejecting admin + the human-readable reason for the audit log.
func (r *FundingRepo) MarkRejectedTx(ctx context.Context, tx *sql.Tx, id, rejectorID, reason string) error {
	if tx == nil {
		return fmt.Errorf("funding_repo: nil tx")
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE funding_requests
		    SET status = 'rejected',
		        rejected_by = $2,
		        rejected_at = NOW(),
		        rejection_reason = $3
		  WHERE id = $1
		    AND status = 'pending'`,
		id, rejectorID, reason,
	)
	if err != nil {
		return fmt.Errorf("funding_repo: mark rejected: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("funding_repo: mark rejected rows: %w", err)
	}
	if n == 0 {
		return ErrFundingRequestStateConflict
	}
	return nil
}

func validateCreate(p *CreateFundingRequestParams) error {
	if p == nil {
		return fmt.Errorf("%w: nil params", ErrFundingRequestInvalid)
	}
	if strings.TrimSpace(p.FundID) == "" {
		return fmt.Errorf("%w: empty fund_id", ErrFundingRequestInvalid)
	}
	if strings.TrimSpace(p.RequestedBy) == "" {
		return fmt.Errorf("%w: empty requested_by", ErrFundingRequestInvalid)
	}
	if p.Direction != FundingDirectionDeposit && p.Direction != FundingDirectionWithdrawal {
		return fmt.Errorf("%w: unknown direction %q", ErrFundingRequestInvalid, p.Direction)
	}
	if !validFundingMethods[p.Method] {
		return fmt.Errorf("%w: unknown method %q", ErrFundingRequestInvalid, p.Method)
	}
	if p.Amount <= 0 {
		return fmt.Errorf("%w: amount must be > 0", ErrFundingRequestInvalid)
	}
	return nil
}

const fundingSelectColumns = `
    id, fund_id, direction, amount, currency, method,
    external_reference, status, requested_by, approved_by,
    approved_at, rejected_by, rejected_at, rejection_reason,
    cancelled_at, cash_ledger_entry_id, notes, metadata,
    created_at, updated_at`

func scanFundingRequest(r rowScanner) (*FundingRequest, error) {
	var (
		fr            FundingRequest
		metadataBytes []byte
	)
	err := r.Scan(
		&fr.ID, &fr.FundID, &fr.Direction, &fr.Amount, &fr.Currency, &fr.Method,
		&fr.ExternalReference, &fr.Status, &fr.RequestedBy, &fr.ApprovedBy,
		&fr.ApprovedAt, &fr.RejectedBy, &fr.RejectedAt, &fr.RejectionReason,
		&fr.CancelledAt, &fr.CashLedgerEntryID, &fr.Notes, &metadataBytes,
		&fr.CreatedAt, &fr.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrFundingRequestNotFound
		}
		return nil, fmt.Errorf("funding_repo: scan: %w", err)
	}
	if len(metadataBytes) == 0 {
		fr.Metadata = json.RawMessage(`{}`)
	} else {
		fr.Metadata = json.RawMessage(metadataBytes)
	}
	return &fr, nil
}
