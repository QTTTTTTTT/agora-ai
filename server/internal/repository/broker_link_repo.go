// Broker link repository (P0-9 / P1-6).
//
// Owns CRUD for the broker_links table — the routing record that
// tells the live-trading hard gate "this fund has an approved
// route to broker X account Y, dispatch is allowed".
//
// Why a separate file
//
// The hard gate (P0-9) is one read, one boolean, but the
// management surface (P1-6: 4-eye approval) is a small workflow.
// Putting both in fund_repo would bury the auth-relevant bits of
// the codebase. Keeping them here mirrors what we did for
// user_totp_secrets / totp_repo.go.
//
// Status semantics (mirrored from the migration comment so callers
// can read here without paging to migrations):
//
//	pending    — created, awaiting 4-eye approval
//	active     — approved, gate accepts trades on this fund
//	suspended  — temporarily disabled
//	revoked    — terminal; create a fresh row to resume live
//
// The hard gate ONLY accepts "active". Anything else produces a
// LiveReadiness reason="broker_link_not_active".

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

// ErrBrokerLinkNotFound is returned when no row matches the lookup.
// Callers that distinguish "not enrolled" from real errors should
// `errors.Is(err, ErrBrokerLinkNotFound)`.
var ErrBrokerLinkNotFound = errors.New("broker_link_repo: not found")

// BrokerLinkStatus enum mirroring the SQL CHECK. Repository
// callers should use these constants instead of string literals
// so a renamed status surfaces as a compile error.
const (
	BrokerLinkStatusPending   = "pending"
	BrokerLinkStatusActive    = "active"
	BrokerLinkStatusSuspended = "suspended"
	BrokerLinkStatusRevoked   = "revoked"
)

// BrokerLink mirrors a row in broker_links. Encrypted credential
// bytes are exposed raw — the gate doesn't decrypt them, only
// the (future) order-router that actually talks to the broker.
type BrokerLink struct {
	ID                   string
	FundID               string
	UserID               string
	BrokerID             string
	AccountID            string
	Status               string
	ApprovedBy           sql.NullString
	ApprovedAt           sql.NullTime
	CredentialsEncrypted []byte
	Metadata             json.RawMessage
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// IsActive returns true when the link is in the gate-passing
// status. Centralising this here means the hard gate doesn't
// depend on the literal "active" string.
func (b *BrokerLink) IsActive() bool {
	return b != nil && b.Status == BrokerLinkStatusActive
}

// BrokerLinkRepo is the persistence facade for broker_links.
type BrokerLinkRepo struct {
	db *sql.DB
}

// NewBrokerLinkRepo wires a repo bound to a sql.DB handle.
func NewBrokerLinkRepo(db *sql.DB) *BrokerLinkRepo {
	return &BrokerLinkRepo{db: db}
}

// GetActiveByFundID returns the (at most one) ACTIVE link for the
// fund. Hot path of the live-trading gate — runs on every cancel
// / replace / place-order on a fund whose trading_mode='live', so
// we keep it to a single indexed equality lookup. Returns
// ErrBrokerLinkNotFound when no active link exists; the gate maps
// this to reason="broker_link_not_active".
func (r *BrokerLinkRepo) GetActiveByFundID(ctx context.Context, fundID string) (*BrokerLink, error) {
	if strings.TrimSpace(fundID) == "" {
		return nil, ErrBrokerLinkNotFound
	}
	const q = `
	SELECT id, fund_id, user_id, broker_id, account_id, status,
	       approved_by, approved_at, credentials_encrypted, metadata,
	       created_at, updated_at
	  FROM broker_links
	 WHERE fund_id = $1
	   AND status = 'active'
	 LIMIT 1`
	row := r.db.QueryRowContext(ctx, q, fundID)
	link := &BrokerLink{}
	if err := scanBrokerLink(row, link); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrBrokerLinkNotFound
		}
		return nil, fmt.Errorf("broker_link_repo: get active by fund: %w", err)
	}
	return link, nil
}

// ListByFundID returns ALL links for a fund regardless of status,
// ordered newest-first. Used by the management UI; not on any
// hot path so we don't mind returning revoked rows.
func (r *BrokerLinkRepo) ListByFundID(ctx context.Context, fundID string) ([]BrokerLink, error) {
	if strings.TrimSpace(fundID) == "" {
		return nil, nil
	}
	const q = `
	SELECT id, fund_id, user_id, broker_id, account_id, status,
	       approved_by, approved_at, credentials_encrypted, metadata,
	       created_at, updated_at
	  FROM broker_links
	 WHERE fund_id = $1
	 ORDER BY created_at DESC`
	rows, err := r.db.QueryContext(ctx, q, fundID)
	if err != nil {
		return nil, fmt.Errorf("broker_link_repo: list by fund: %w", err)
	}
	defer rows.Close()
	var out []BrokerLink
	for rows.Next() {
		var link BrokerLink
		if err := scanBrokerLink(rows, &link); err != nil {
			return nil, fmt.Errorf("broker_link_repo: list by fund scan: %w", err)
		}
		out = append(out, link)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("broker_link_repo: list by fund rows: %w", err)
	}
	return out, nil
}

// CreateParams is the input for Create. Status is omitted on
// purpose — every fresh row starts as "pending" and only an
// approval call can promote it.
type CreateParams struct {
	FundID               string
	UserID               string
	BrokerID             string
	AccountID            string
	CredentialsEncrypted []byte
	Metadata             json.RawMessage
}

// Create inserts a new broker_link row in status='pending'.
// Returns the new row ID. Validates the required string fields
// up-front; SQL CHECKs handle the rest.
func (r *BrokerLinkRepo) Create(ctx context.Context, p CreateParams) (string, error) {
	if strings.TrimSpace(p.FundID) == "" {
		return "", errors.New("broker_link_repo: fund_id required")
	}
	if strings.TrimSpace(p.UserID) == "" {
		return "", errors.New("broker_link_repo: user_id required")
	}
	if strings.TrimSpace(p.BrokerID) == "" {
		return "", errors.New("broker_link_repo: broker_id required")
	}
	if strings.TrimSpace(p.AccountID) == "" {
		return "", errors.New("broker_link_repo: account_id required")
	}
	metadata := p.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	const q = `
	INSERT INTO broker_links
	    (fund_id, user_id, broker_id, account_id, status, credentials_encrypted, metadata)
	VALUES ($1, $2, $3, $4, 'pending', $5, $6)
	RETURNING id`
	var id string
	if err := r.db.QueryRowContext(ctx, q,
		p.FundID, p.UserID, p.BrokerID, p.AccountID, p.CredentialsEncrypted, []byte(metadata),
	).Scan(&id); err != nil {
		return "", fmt.Errorf("broker_link_repo: create: %w", err)
	}
	return id, nil
}

// Approve flips a pending link to active and records who did the
// approval. The 4-eye check (approver != requester) lives in the
// HTTP handler (P1-6) — the repo is policy-free so tests can
// drive any combination they need.
//
// Idempotent for the (already active, same approver) case so
// retries from P1-6's flaky network paths don't fail.
func (r *BrokerLinkRepo) Approve(ctx context.Context, linkID, approverUserID string) error {
	if strings.TrimSpace(linkID) == "" {
		return errors.New("broker_link_repo: link id required")
	}
	if strings.TrimSpace(approverUserID) == "" {
		return errors.New("broker_link_repo: approver required")
	}
	const q = `
	UPDATE broker_links
	   SET status = 'active',
	       approved_by = $2,
	       approved_at = NOW()
	 WHERE id = $1
	   AND status IN ('pending', 'suspended')`
	res, err := r.db.ExecContext(ctx, q, linkID, approverUserID)
	if err != nil {
		return fmt.Errorf("broker_link_repo: approve: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("broker_link_repo: approve rows affected: %w", err)
	}
	if rows == 0 {
		return ErrBrokerLinkNotFound
	}
	return nil
}

// Revoke moves a link to terminal state. Used by admin or by the
// user via the account-security page when a broker key is rotated
// out. Pending rows can also be revoked (effectively "cancel
// pending request"). Already-revoked rows return ErrBrokerLinkNotFound.
func (r *BrokerLinkRepo) Revoke(ctx context.Context, linkID string) error {
	if strings.TrimSpace(linkID) == "" {
		return errors.New("broker_link_repo: link id required")
	}
	const q = `
	UPDATE broker_links
	   SET status = 'revoked'
	 WHERE id = $1
	   AND status <> 'revoked'`
	res, err := r.db.ExecContext(ctx, q, linkID)
	if err != nil {
		return fmt.Errorf("broker_link_repo: revoke: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("broker_link_repo: revoke rows affected: %w", err)
	}
	if rows == 0 {
		return ErrBrokerLinkNotFound
	}
	return nil
}

// scanRow is a small interface that both sql.Row and sql.Rows
// satisfy via Scan; lets us share the column list between the
// single-row and many-row code paths.
type scanRow interface {
	Scan(...any) error
}

func scanBrokerLink(s scanRow, link *BrokerLink) error {
	var metadataBytes []byte
	if err := s.Scan(
		&link.ID, &link.FundID, &link.UserID, &link.BrokerID, &link.AccountID, &link.Status,
		&link.ApprovedBy, &link.ApprovedAt, &link.CredentialsEncrypted, &metadataBytes,
		&link.CreatedAt, &link.UpdatedAt,
	); err != nil {
		return err
	}
	if len(metadataBytes) == 0 {
		link.Metadata = json.RawMessage(`{}`)
	} else {
		link.Metadata = json.RawMessage(metadataBytes)
	}
	return nil
}
