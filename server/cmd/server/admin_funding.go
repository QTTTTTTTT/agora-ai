// Admin funding-request endpoints (P1-2 / 4-eye approval).
//
// Companion to funding_handler.go. Adds the privileged side of
// the deposit/withdrawal workflow:
//
//	GET  /api/admin/funding-requests?status=...
//	POST /api/admin/funding-requests/{id}/approve
//	POST /api/admin/funding-requests/{id}/reject
//
// 4-eye constraint
//
// The user who submitted the request (funding_requests.requested_by)
// MUST NOT be the user who approves it. The CHECK on the table is
// the last line of defence; the handler returns a clean 403
// four_eye_violation so the UI can show "ask a colleague".
//
// What approval actually does
//
// The approve handler runs a single DB transaction:
//
//   1. SELECT ... FOR UPDATE the funding_requests row (so two
//      admins can't race the same approve).
//   2. Validate state machine (status must still be 'pending').
//   3. Validate withdrawal cash sufficiency (amount <=
//      funds.current_capital). For deposits, no balance check.
//   4. INSERT into cash_ledger with the matching entry_type
//      (funding_deposit / funding_withdrawal). Amount is signed:
//      deposit credits (+), withdrawal debits (-).
//   5. UPDATE funds.current_capital atomically with the same
//      delta the cash_ledger row records.
//   6. UPDATE funding_requests → status='approved', cash_ledger_entry_id=…
//   7. COMMIT.
//
// If any step fails, ROLLBACK leaves the request as pending and
// surfaces a useful error code. The cash_ledger insert uses an
// idempotency_key derived from the funding_request id so a retry
// path can't double-post.

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/repository"
)

func (h *adminHandler) registerFundingAdminRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/funding-requests", h.handleListFundingAdmin)
	mux.HandleFunc("POST /api/admin/funding-requests/{id}/approve", h.handleApproveFunding)
	mux.HandleFunc("POST /api/admin/funding-requests/{id}/reject", h.handleRejectFunding)
}

type adminFundingRow struct {
	ID                string  `json:"id"`
	FundID            string  `json:"fundId"`
	Direction         string  `json:"direction"`
	Amount            float64 `json:"amount"`
	Currency          string  `json:"currency"`
	Method            string  `json:"method"`
	ExternalReference string  `json:"externalReference,omitempty"`
	Status            string  `json:"status"`
	RequestedBy       string  `json:"requestedBy"`
	ApprovedBy        string  `json:"approvedBy,omitempty"`
	ApprovedAt        string  `json:"approvedAt,omitempty"`
	RejectedBy        string  `json:"rejectedBy,omitempty"`
	RejectedAt        string  `json:"rejectedAt,omitempty"`
	RejectionReason   string  `json:"rejectionReason,omitempty"`
	Notes             string  `json:"notes,omitempty"`
	CreatedAt         string  `json:"createdAt"`
}

func (h *adminHandler) handleListFundingAdmin(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	status := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	if status == "" {
		status = repository.FundingStatusPending
	}
	if status != "all" && !isValidFundingStatus(status) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":  "invalid_status",
			"detail": "status must be one of: pending, approved, rejected, cancelled, posted, all",
		})
		return
	}

	repo := repository.NewFundingRepo(h.db)
	var (
		rows []repository.FundingRequest
		err  error
	)
	ctx := r.Context()
	if status == "pending" {
		rows, err = repo.ListPendingAdmin(ctx, 200)
	} else if status == "all" {
		rows, err = h.listAllFundingAdmin(ctx, 200)
	} else {
		rows, err = h.listFundingByStatusAdmin(ctx, status, 200)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "list_failed", "detail": err.Error()})
		return
	}

	out := make([]adminFundingRow, 0, len(rows))
	for i := range rows {
		out = append(out, projectAdminFundingRow(&rows[i]))
	}

	approverID, _ := api.AuthenticatedUserID(r)
	safeAuditLogAccess(ctx, h.auditLogger, approverID, "read", "funding_request", status, map[string]any{
		"row_count": len(out),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"requests":  out,
		"status":    status,
		"row_count": len(out),
	})
}

// listAllFundingAdmin / listFundingByStatusAdmin are admin-only
// helpers. We keep them off the public repo to avoid leaking the
// "list everyone's funding" capability to user-side callers.
func (h *adminHandler) listAllFundingAdmin(ctx context.Context, limit int) ([]repository.FundingRequest, error) {
	rows, err := h.db.QueryContext(ctx,
		`SELECT id, fund_id, direction, amount, currency, method,
		        external_reference, status, requested_by, approved_by,
		        approved_at, rejected_by, rejected_at, rejection_reason,
		        cancelled_at, cash_ledger_entry_id, notes, metadata,
		        created_at, updated_at
		   FROM funding_requests
		   ORDER BY created_at DESC
		   LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFundingRows(rows)
}

func (h *adminHandler) listFundingByStatusAdmin(ctx context.Context, status string, limit int) ([]repository.FundingRequest, error) {
	rows, err := h.db.QueryContext(ctx,
		`SELECT id, fund_id, direction, amount, currency, method,
		        external_reference, status, requested_by, approved_by,
		        approved_at, rejected_by, rejected_at, rejection_reason,
		        cancelled_at, cash_ledger_entry_id, notes, metadata,
		        created_at, updated_at
		   FROM funding_requests
		   WHERE status = $1
		   ORDER BY created_at DESC
		   LIMIT $2`,
		status, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFundingRows(rows)
}

func scanFundingRows(rows *sql.Rows) ([]repository.FundingRequest, error) {
	out := make([]repository.FundingRequest, 0)
	for rows.Next() {
		var (
			fr            repository.FundingRequest
			metadataBytes []byte
		)
		if err := rows.Scan(
			&fr.ID, &fr.FundID, &fr.Direction, &fr.Amount, &fr.Currency, &fr.Method,
			&fr.ExternalReference, &fr.Status, &fr.RequestedBy, &fr.ApprovedBy,
			&fr.ApprovedAt, &fr.RejectedBy, &fr.RejectedAt, &fr.RejectionReason,
			&fr.CancelledAt, &fr.CashLedgerEntryID, &fr.Notes, &metadataBytes,
			&fr.CreatedAt, &fr.UpdatedAt,
		); err != nil {
			return nil, err
		}
		if len(metadataBytes) == 0 {
			fr.Metadata = json.RawMessage(`{}`)
		} else {
			fr.Metadata = json.RawMessage(metadataBytes)
		}
		out = append(out, fr)
	}
	return out, rows.Err()
}

type approveFundingPayload struct {
	Note string `json:"note,omitempty"`
}

type rejectFundingPayload struct {
	Reason string `json:"reason"`
}

// handleApproveFunding runs the full approve-and-post flow inside
// one DB transaction. Errors at any point roll back so the request
// stays pending and the cash_ledger doesn't get a half-row.
func (h *adminHandler) handleApproveFunding(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id_required"})
		return
	}
	approverID, _ := api.AuthenticatedUserID(r)
	if approverID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthenticated"})
		return
	}

	var body approveFundingPayload
	if r.ContentLength > 0 {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_body", "detail": err.Error()})
			return
		}
	}

	ctx := r.Context()
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "tx_begin_failed", "detail": err.Error()})
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	repo := repository.NewFundingRepo(h.db)
	row, err := repo.LookupForApprovalTx(ctx, tx, id)
	if err != nil {
		if errors.Is(err, repository.ErrFundingRequestNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "not_found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "lookup_failed", "detail": err.Error()})
		return
	}
	if row.Status != repository.FundingStatusPending {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":         "not_pending",
			"detail":        "request is no longer pending",
			"current_state": row.Status,
		})
		return
	}
	if row.RequestedBy == approverID {
		safeAuditLogAccess(ctx, h.auditLogger, approverID,
			"funding_request.approve_blocked_4eye", "funding_request", id,
			map[string]any{
				"requester_user_id": row.RequestedBy,
				"reason":            "approver_equals_requester",
			})
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":  "four_eye_violation",
			"detail": "approver must differ from the funding requester",
		})
		return
	}

	// Withdrawal sufficiency check: refuse if the fund's current
	// cash position is below the requested amount. We pull
	// funds.current_capital INSIDE the same tx so a concurrent
	// trade can't shrink the buffer between our read and write.
	if row.Direction == repository.FundingDirectionWithdrawal {
		var availableCash float64
		err := tx.QueryRowContext(ctx,
			`SELECT current_capital FROM funds WHERE id = $1 FOR UPDATE`,
			row.FundID,
		).Scan(&availableCash)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error":  "fund_lookup_failed",
				"detail": err.Error(),
			})
			return
		}
		if availableCash < row.Amount-0.0001 {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":          "insufficient_cash",
				"detail":         "withdrawal exceeds current_capital",
				"requested":      row.Amount,
				"current_capital": availableCash,
			})
			return
		}
	}

	// Compute signed delta + the cash_ledger entry_type that
	// matches the request direction.
	var (
		signedAmount float64
		entryType    string
	)
	if row.Direction == repository.FundingDirectionDeposit {
		signedAmount = row.Amount
		entryType = repository.CashEntryFundingDeposit
	} else {
		signedAmount = -row.Amount
		entryType = repository.CashEntryFundingWithdraw
	}

	// 1) cash_ledger row first (so its id can be linked back).
	cashLedger := repository.NewCashLedgerRepo(h.db)
	ledgerID, err := cashLedger.AppendTx(ctx, tx, repository.AppendParams{
		FundID:      row.FundID,
		PostedAt:    time.Now().UTC(),
		EntryType:   entryType,
		Amount:      signedAmount,
		Currency:    row.Currency,
		Description: fmt.Sprintf("%s via %s (request %s)", row.Direction, row.Method, id),
		Metadata: map[string]any{
			"funding_request_id": id,
			"method":             row.Method,
			"requested_by":       row.RequestedBy,
			"approved_by":        approverID,
		},
		CreatedBy:      approverID,
		IdempotencyKey: fmt.Sprintf("funding:%s", id),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "ledger_append_failed", "detail": err.Error()})
		return
	}

	// 2) funds.current_capital atomic update.
	if _, err := tx.ExecContext(ctx,
		`UPDATE funds
		    SET current_capital = current_capital + $2,
		        updated_at = NOW()
		  WHERE id = $1`,
		row.FundID, signedAmount,
	); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "fund_update_failed", "detail": err.Error()})
		return
	}

	// 3) flip the request state.
	if err := repo.MarkApprovedTx(ctx, tx, id, approverID, ledgerID); err != nil {
		if errors.Is(err, repository.ErrFundingRequestStateConflict) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "state_conflict"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "mark_approved_failed", "detail": err.Error()})
		return
	}

	if err := tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "tx_commit_failed", "detail": err.Error()})
		return
	}
	committed = true

	if h.metrics != nil {
		h.metrics.RecordFundingRequestEvent("approved")
	}

	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(ctx, audit.MutationEvent{
			ActorUserID: approverID,
			Action:      "funding_request.approve",
			TargetType:  "funding_request",
			TargetID:    id,
			Before:      map[string]any{"status": repository.FundingStatusPending},
			After: map[string]any{
				"status":               repository.FundingStatusApproved,
				"approved_by":          approverID,
				"cash_ledger_entry_id": ledgerID,
			},
			Metadata: map[string]any{
				"requester_user_id": row.RequestedBy,
				"fund_id":           row.FundID,
				"direction":         row.Direction,
				"amount":            row.Amount,
				"currency":          row.Currency,
				"method":            row.Method,
				"note":              strings.TrimSpace(body.Note),
				"client_addr":       clientIP(r),
			},
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":                id,
		"status":            repository.FundingStatusApproved,
		"cashLedgerEntryId": ledgerID,
	})
}

// handleRejectFunding flips a pending row to rejected with a
// reason. No cash movement, but we still 4-eye-check + audit.
func (h *adminHandler) handleRejectFunding(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id_required"})
		return
	}
	approverID, _ := api.AuthenticatedUserID(r)
	if approverID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthenticated"})
		return
	}
	var body rejectFundingPayload
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_body", "detail": err.Error()})
		return
	}
	if strings.TrimSpace(body.Reason) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "reason_required"})
		return
	}

	ctx := r.Context()
	repo := repository.NewFundingRepo(h.db)

	row, err := repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrFundingRequestNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "not_found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "lookup_failed", "detail": err.Error()})
		return
	}
	if row.Status != repository.FundingStatusPending {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":         "not_pending",
			"detail":        "request is no longer pending",
			"current_state": row.Status,
		})
		return
	}
	if row.RequestedBy == approverID {
		safeAuditLogAccess(ctx, h.auditLogger, approverID,
			"funding_request.reject_blocked_4eye", "funding_request", id,
			map[string]any{"reason": "approver_equals_requester"})
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":  "four_eye_violation",
			"detail": "approver must differ from the funding requester",
		})
		return
	}

	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "tx_begin_failed", "detail": err.Error()})
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := repo.MarkRejectedTx(ctx, tx, id, approverID, strings.TrimSpace(body.Reason)); err != nil {
		if errors.Is(err, repository.ErrFundingRequestStateConflict) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "state_conflict"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "mark_rejected_failed", "detail": err.Error()})
		return
	}
	if err := tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "tx_commit_failed", "detail": err.Error()})
		return
	}
	committed = true

	if h.metrics != nil {
		h.metrics.RecordFundingRequestEvent("rejected")
	}

	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(ctx, audit.MutationEvent{
			ActorUserID: approverID,
			Action:      "funding_request.reject",
			TargetType:  "funding_request",
			TargetID:    id,
			Before:      map[string]any{"status": repository.FundingStatusPending},
			After: map[string]any{
				"status":      repository.FundingStatusRejected,
				"rejected_by": approverID,
			},
			Metadata: map[string]any{
				"requester_user_id": row.RequestedBy,
				"fund_id":           row.FundID,
				"reason":            strings.TrimSpace(body.Reason),
				"client_addr":       clientIP(r),
			},
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":     id,
		"status": repository.FundingStatusRejected,
	})
}

func projectAdminFundingRow(r *repository.FundingRequest) adminFundingRow {
	out := adminFundingRow{
		ID:          r.ID,
		FundID:      r.FundID,
		Direction:   r.Direction,
		Amount:      r.Amount,
		Currency:    r.Currency,
		Method:      r.Method,
		Status:      r.Status,
		RequestedBy: r.RequestedBy,
		CreatedAt:   r.CreatedAt.UTC().Format(time.RFC3339),
	}
	if r.ExternalReference.Valid {
		out.ExternalReference = r.ExternalReference.String
	}
	if r.ApprovedBy.Valid {
		out.ApprovedBy = r.ApprovedBy.String
	}
	if r.ApprovedAt.Valid {
		out.ApprovedAt = r.ApprovedAt.Time.UTC().Format(time.RFC3339)
	}
	if r.RejectedBy.Valid {
		out.RejectedBy = r.RejectedBy.String
	}
	if r.RejectedAt.Valid {
		out.RejectedAt = r.RejectedAt.Time.UTC().Format(time.RFC3339)
	}
	if r.RejectionReason.Valid {
		out.RejectionReason = r.RejectionReason.String
	}
	if r.Notes.Valid {
		out.Notes = r.Notes.String
	}
	return out
}

func isValidFundingStatus(s string) bool {
	switch s {
	case repository.FundingStatusPending,
		repository.FundingStatusApproved,
		repository.FundingStatusRejected,
		repository.FundingStatusCancelled,
		repository.FundingStatusPosted:
		return true
	}
	return false
}
