package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Sentinel errors returned by DualControlService. Callers should check
// these with errors.Is to render correct HTTP status codes (e.g. 403
// for SelfApproval, 404 for NotFound, 409 for AlreadyFinalized).
var (
	ErrRequestNotFound        = errors.New("admin request not found")
	ErrRequestSelfApproval    = errors.New("admin request cannot be approved by its requester")
	ErrRequestAlreadyFinal    = errors.New("admin request already finalized")
	ErrRequestExpired         = errors.New("admin request expired")
	ErrUnknownAdminAction     = errors.New("unknown admin action")
	ErrRequesterNotSuperAdmin = errors.New("requester is not a super_admin")
	ErrApproverNotSuperAdmin  = errors.New("approver is not a super_admin")
)

// AdminRequest mirrors a row in admin_requests. Payload remains a raw
// JSON blob so callers can decode into their action-specific schema.
type AdminRequest struct {
	ID              string
	RequesterUserID string
	Action          string
	TargetType      string
	TargetID        string
	Payload         json.RawMessage
	Reason          string
	Status          string
	ApproverUserID  sql.NullString
	ApprovedAt      sql.NullTime
	ExecutedAt      sql.NullTime
	ExecutionError  sql.NullString
	ExpiresAt       time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ActionHandler executes a previously-approved admin request. Handlers
// are registered per-action with DualControlService.Register and are
// invoked from ApproveAndExecute under the same logical transaction as
// the status flip to "executed".
//
// The handler returns:
//   - resultDetails: optional payload echoed back to the approver
//     (e.g. the new entity id, the patch applied);
//   - err: any failure short-circuits the dual-control flip to
//     "failed" and the request is preserved for inspection.
type ActionHandler func(ctx context.Context, tx *sql.Tx, req AdminRequest) (resultDetails map[string]any, err error)

// SuperAdminChecker validates that a user has super_admin privileges.
// Kept as an interface so tests can stub without depending on the full
// auth/repo wiring.
type SuperAdminChecker interface {
	IsSuperAdmin(ctx context.Context, userID string) (bool, error)
}

// DualControlService manages two-person approval for sensitive admin
// operations. The flow is:
//
//  1. super_admin A calls Submit → creates pending row + audit access log
//  2. super_admin B (≠ A) calls Approve → flips to "approved", runs handler
//  3. on handler success → status "executed"; on failure → "failed"
//
// Self-approval is rejected at three layers: app code (Approve), CHECK
// constraint on admin_requests, and a unit test. Expired requests can
// never be approved — they must be re-submitted, which keeps the audit
// trail honest about "this old request was never executed".
type DualControlService struct {
	db                *sql.DB
	logger            Logger
	superAdminChecker SuperAdminChecker
	handlers          map[string]ActionHandler
	defaultTTL        time.Duration
	now               func() time.Time
}

// NewDualControlService constructs the service. defaultTTL is the time
// a pending request remains approvable; 24h is a reasonable balance
// between operational urgency and avoiding stale approvals against
// changed state (e.g. budget already raised by someone else).
func NewDualControlService(db *sql.DB, logger Logger, checker SuperAdminChecker, defaultTTL time.Duration) *DualControlService {
	if defaultTTL <= 0 {
		defaultTTL = 24 * time.Hour
	}
	if logger == nil {
		logger = NopLogger{}
	}
	return &DualControlService{
		db:                db,
		logger:            logger,
		superAdminChecker: checker,
		handlers:          map[string]ActionHandler{},
		defaultTTL:        defaultTTL,
		now:               time.Now,
	}
}

// Register installs an ActionHandler for a given action name. Registering
// the same action twice panics so duplicate registrations are caught at
// startup rather than at first request.
func (s *DualControlService) Register(action string, handler ActionHandler) {
	if _, exists := s.handlers[action]; exists {
		panic(fmt.Sprintf("audit: duplicate handler registered for action %q", action))
	}
	s.handlers[action] = handler
}

// KnownActions returns the set of registered actions. Used by handlers
// to validate request submissions before persisting.
func (s *DualControlService) KnownActions() []string {
	out := make([]string, 0, len(s.handlers))
	for k := range s.handlers {
		out = append(out, k)
	}
	return out
}

// SubmitInput is the request payload for Submit.
type SubmitInput struct {
	RequesterUserID string
	Action          string
	TargetType      string
	TargetID        string
	Payload         map[string]any
	Reason          string
	TTL             time.Duration // optional override of defaultTTL
}

// Submit creates a pending admin request. Requires the action to be
// registered (otherwise nothing could execute it) and the requester to
// be a super_admin. Returns the created row including its expiry so
// the UI can show "approvable until X".
func (s *DualControlService) Submit(ctx context.Context, input SubmitInput) (*AdminRequest, error) {
	if _, ok := s.handlers[input.Action]; !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownAdminAction, input.Action)
	}
	if s.superAdminChecker != nil {
		ok, err := s.superAdminChecker.IsSuperAdmin(ctx, input.RequesterUserID)
		if err != nil {
			return nil, fmt.Errorf("audit: check requester role: %w", err)
		}
		if !ok {
			return nil, ErrRequesterNotSuperAdmin
		}
	}
	ttl := input.TTL
	if ttl <= 0 {
		ttl = s.defaultTTL
	}
	expiresAt := s.now().Add(ttl)
	payloadJSON, err := json.Marshal(input.Payload)
	if err != nil {
		return nil, fmt.Errorf("audit: marshal payload: %w", err)
	}

	var id string
	var createdAt, updatedAt time.Time
	err = s.db.QueryRowContext(ctx,
		`INSERT INTO admin_requests (requester_user_id, action, target_type, target_id, payload, reason, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id, created_at, updated_at`,
		input.RequesterUserID, input.Action, input.TargetType, input.TargetID, payloadJSON, nullableString(input.Reason), expiresAt,
	).Scan(&id, &createdAt, &updatedAt)
	if err != nil {
		return nil, fmt.Errorf("audit: insert admin request: %w", err)
	}

	_ = s.logger.LogAccess(ctx, input.RequesterUserID, "submit_admin_request", input.TargetType, input.TargetID, map[string]any{
		"requestId": id,
		"action":    input.Action,
		"reason":    input.Reason,
	})

	return &AdminRequest{
		ID:              id,
		RequesterUserID: input.RequesterUserID,
		Action:          input.Action,
		TargetType:      input.TargetType,
		TargetID:        input.TargetID,
		Payload:         payloadJSON,
		Reason:          input.Reason,
		Status:          "pending",
		ExpiresAt:       expiresAt,
		CreatedAt:       createdAt,
		UpdatedAt:       updatedAt,
	}, nil
}

// ApproveAndExecute validates the approver, locks the pending row,
// executes the registered handler inside a single transaction, then
// flips the row to "executed" / "failed" depending on the outcome.
// All status updates are inside the same TX so a server crash mid-flight
// leaves the row in either "pending" (untouched) or one of the final
// states — never split-brain.
func (s *DualControlService) ApproveAndExecute(ctx context.Context, requestID, approverUserID string) (*AdminRequest, map[string]any, error) {
	if s.superAdminChecker != nil {
		ok, err := s.superAdminChecker.IsSuperAdmin(ctx, approverUserID)
		if err != nil {
			return nil, nil, fmt.Errorf("audit: check approver role: %w", err)
		}
		if !ok {
			return nil, nil, ErrApproverNotSuperAdmin
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("audit: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	req, err := s.lockRequest(ctx, tx, requestID)
	if err != nil {
		return nil, nil, err
	}

	if req.Status != "pending" {
		return req, nil, fmt.Errorf("%w: status=%s", ErrRequestAlreadyFinal, req.Status)
	}
	if !req.ExpiresAt.After(s.now()) {
		_, _ = tx.ExecContext(ctx,
			`UPDATE admin_requests SET status='expired', updated_at=NOW() WHERE id=$1`,
			requestID,
		)
		if err := tx.Commit(); err == nil {
			committed = true
		}
		return req, nil, ErrRequestExpired
	}
	if req.RequesterUserID == approverUserID {
		return req, nil, ErrRequestSelfApproval
	}

	handler, ok := s.handlers[req.Action]
	if !ok {
		return req, nil, fmt.Errorf("%w: %s", ErrUnknownAdminAction, req.Action)
	}

	_, err = tx.ExecContext(ctx,
		`UPDATE admin_requests SET status='approved', approver_user_id=$2, approved_at=NOW(), updated_at=NOW() WHERE id=$1`,
		requestID, approverUserID,
	)
	if err != nil {
		return req, nil, fmt.Errorf("audit: mark approved: %w", err)
	}
	req.Status = "approved"
	req.ApproverUserID = sql.NullString{String: approverUserID, Valid: true}

	details, handlerErr := handler(ctx, tx, *req)
	if handlerErr != nil {
		// The handler ran inside this same TX so any partial writes will
		// be rolled back together with the approved → failed flip.
		_ = tx.Rollback()
		// Open a fresh TX just to record the failure outcome. We tolerate
		// secondary failures here — losing the failure tag is preferable
		// to bubbling up a confusing dual-error.
		_, _ = s.db.ExecContext(ctx,
			`UPDATE admin_requests SET status='failed', approver_user_id=$2, approved_at=COALESCE(approved_at, NOW()), execution_error=$3, updated_at=NOW() WHERE id=$1`,
			requestID, approverUserID, handlerErr.Error(),
		)
		req.Status = "failed"
		req.ExecutionError = sql.NullString{String: handlerErr.Error(), Valid: true}
		return req, nil, handlerErr
	}

	_, err = tx.ExecContext(ctx,
		`UPDATE admin_requests SET status='executed', executed_at=NOW(), updated_at=NOW() WHERE id=$1`,
		requestID,
	)
	if err != nil {
		return req, nil, fmt.Errorf("audit: mark executed: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return req, nil, fmt.Errorf("audit: commit: %w", err)
	}
	committed = true
	req.Status = "executed"

	_ = s.logger.LogAccess(ctx, approverUserID, "approve_admin_request", req.TargetType, req.TargetID, map[string]any{
		"requestId": req.ID,
		"action":    req.Action,
	})

	return req, details, nil
}

// Reject closes a pending request without executing it. Rejection still
// requires a different super_admin than the requester so an actor cannot
// silently kill their own pending request without anyone noticing.
func (s *DualControlService) Reject(ctx context.Context, requestID, approverUserID, reason string) (*AdminRequest, error) {
	if s.superAdminChecker != nil {
		ok, err := s.superAdminChecker.IsSuperAdmin(ctx, approverUserID)
		if err != nil {
			return nil, fmt.Errorf("audit: check approver role: %w", err)
		}
		if !ok {
			return nil, ErrApproverNotSuperAdmin
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("audit: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	req, err := s.lockRequest(ctx, tx, requestID)
	if err != nil {
		return nil, err
	}
	if req.Status != "pending" {
		return req, fmt.Errorf("%w: status=%s", ErrRequestAlreadyFinal, req.Status)
	}
	if req.RequesterUserID == approverUserID {
		return req, ErrRequestSelfApproval
	}

	_, err = tx.ExecContext(ctx,
		`UPDATE admin_requests SET status='rejected', approver_user_id=$2, approved_at=NOW(), execution_error=$3, updated_at=NOW() WHERE id=$1`,
		requestID, approverUserID, nullableString(reason),
	)
	if err != nil {
		return req, fmt.Errorf("audit: mark rejected: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return req, fmt.Errorf("audit: commit: %w", err)
	}
	committed = true
	req.Status = "rejected"

	_ = s.logger.LogAccess(ctx, approverUserID, "reject_admin_request", req.TargetType, req.TargetID, map[string]any{
		"requestId": req.ID,
		"action":    req.Action,
		"reason":    reason,
	})
	return req, nil
}

// Get returns a single admin request or ErrRequestNotFound.
func (s *DualControlService) Get(ctx context.Context, requestID string) (*AdminRequest, error) {
	row := s.db.QueryRowContext(ctx, selectAdminRequestSQL+` WHERE id=$1`, requestID)
	return scanAdminRequest(row)
}

// List returns recent admin requests, newest first. When statusFilter is
// empty all statuses are returned. limit ≤ 0 defaults to 100.
func (s *DualControlService) List(ctx context.Context, statusFilter string, limit int) ([]*AdminRequest, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var (
		rows *sql.Rows
		err  error
	)
	if statusFilter == "" {
		rows, err = s.db.QueryContext(ctx, selectAdminRequestSQL+` ORDER BY created_at DESC LIMIT $1`, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, selectAdminRequestSQL+` WHERE status=$1 ORDER BY created_at DESC LIMIT $2`, statusFilter, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("audit: list admin requests: %w", err)
	}
	defer rows.Close()
	out := make([]*AdminRequest, 0)
	for rows.Next() {
		req, err := scanAdminRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, req)
	}
	return out, rows.Err()
}

func (s *DualControlService) lockRequest(ctx context.Context, tx *sql.Tx, requestID string) (*AdminRequest, error) {
	row := tx.QueryRowContext(ctx, selectAdminRequestSQL+` WHERE id=$1 FOR UPDATE`, requestID)
	req, err := scanAdminRequest(row)
	if err != nil {
		return nil, err
	}
	return req, nil
}

// rowScanner abstracts *sql.Row and *sql.Rows for scanAdminRequest.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanAdminRequest(scanner rowScanner) (*AdminRequest, error) {
	req := &AdminRequest{}
	var reason sql.NullString
	err := scanner.Scan(
		&req.ID,
		&req.RequesterUserID,
		&req.Action,
		&req.TargetType,
		&req.TargetID,
		&req.Payload,
		&reason,
		&req.Status,
		&req.ApproverUserID,
		&req.ApprovedAt,
		&req.ExecutedAt,
		&req.ExecutionError,
		&req.ExpiresAt,
		&req.CreatedAt,
		&req.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRequestNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("audit: scan admin request: %w", err)
	}
	if reason.Valid {
		req.Reason = reason.String
	}
	return req, nil
}

const selectAdminRequestSQL = `SELECT id, requester_user_id, action, target_type, target_id, payload, reason, status,
		approver_user_id, approved_at, executed_at, execution_error, expires_at, created_at, updated_at
		FROM admin_requests`

func nullableString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
