// Admin broker-link endpoints (P1-6 / 4-eye approval).
//
// Companion to broker_link_handler.go. Adds the privileged side
// of the workflow:
//
//	GET  /api/admin/broker-links?status=...
//	POST /api/admin/broker-links/{id}/approve
//	POST /api/admin/broker-links/{id}/reject
//
// 4-eye constraint
//
// The user who SUBMITTED a broker_link request (broker_links.user_id)
// MUST NOT be the same person who APPROVES it. We enforce this at
// the handler level rather than in the repo so:
//   - the repo stays policy-free and trivially testable;
//   - the constraint can vary by deployment (e.g. relax to "any
//     super_admin can self-approve in dev") without churning the
//     SQL surface.
//
// Today we hard-code "approver != requester". A future variant
// can introduce a per-broker policy ("require 2 super_admins for
// IBKR live", etc.) by extending this handler.
//
// Rejection
//
// Rejection moves the row to status='revoked' (the same terminal
// state as user-self-revoke) so a future query "list approved
// broker_links" doesn't have to special-case rejected vs.
// revoked. A free-text `reason` becomes part of the audit
// metadata; we deliberately don't store it on the row itself
// because revoked rows are kept only for forensic purposes.

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/repository"
)

// registerBrokerLinkAdminRoutes wires the admin endpoints onto
// the existing adminHandler. Called from RegisterAdminRoutes —
// keeps the route map central and discoverable.
func (h *adminHandler) registerBrokerLinkAdminRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/broker-links", h.handleListBrokerLinksAdmin)
	mux.HandleFunc("POST /api/admin/broker-links/{id}/approve", h.handleApproveBrokerLink)
	mux.HandleFunc("POST /api/admin/broker-links/{id}/reject", h.handleRejectBrokerLink)
}

type adminBrokerLinkRow struct {
	ID         string `json:"id"`
	FundID     string `json:"fundId"`
	UserID     string `json:"userId"`
	BrokerID   string `json:"brokerId"`
	AccountID  string `json:"accountId"` // redacted
	Status     string `json:"status"`
	ApprovedBy string `json:"approvedBy,omitempty"`
	ApprovedAt string `json:"approvedAt,omitempty"`
	CreatedAt  string `json:"createdAt"`
	UpdatedAt  string `json:"updatedAt"`
}

// handleListBrokerLinksAdmin lists broker_link requests filtered
// by status. Default filter is "pending" — the list the admin
// most often wants to act on. Operators can pass `status=all`
// to see history.
//
// We expose it through a single SELECT (no offset/limit) for
// MVP — the realistic volume is single-digit pending rows. When
// usage grows we add cursor pagination.
func (h *adminHandler) handleListBrokerLinksAdmin(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	status := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	if status == "" {
		status = repository.BrokerLinkStatusPending
	}
	if status != "all" && !isValidBrokerLinkStatus(status) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_status", "detail": "status must be one of: pending, active, suspended, revoked, all"})
		return
	}

	const baseQuery = `
		SELECT id, fund_id, user_id, broker_id, account_id, status,
		       approved_by, approved_at, credentials_encrypted, metadata,
		       created_at, updated_at
		  FROM broker_links`

	var (
		rows *sql.Rows
		err  error
	)
	if status == "all" {
		rows, err = h.db.QueryContext(r.Context(), baseQuery+` ORDER BY created_at DESC LIMIT 200`)
	} else {
		rows, err = h.db.QueryContext(r.Context(), baseQuery+` WHERE status = $1 ORDER BY created_at DESC LIMIT 200`, status)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "list_failed", "detail": err.Error()})
		return
	}
	defer rows.Close()

	out := make([]adminBrokerLinkRow, 0)
	for rows.Next() {
		var b repository.BrokerLink
		var metadataBytes []byte
		if err := rows.Scan(
			&b.ID, &b.FundID, &b.UserID, &b.BrokerID, &b.AccountID, &b.Status,
			&b.ApprovedBy, &b.ApprovedAt, &b.CredentialsEncrypted, &metadataBytes,
			&b.CreatedAt, &b.UpdatedAt,
		); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "scan_failed", "detail": err.Error()})
			return
		}
		out = append(out, adminBrokerLinkRow{
			ID:        b.ID,
			FundID:    b.FundID,
			UserID:    b.UserID,
			BrokerID:  b.BrokerID,
			AccountID: redactAccountID(b.AccountID),
			Status:    b.Status,
			CreatedAt: b.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			UpdatedAt: b.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			ApprovedBy: func() string {
				if b.ApprovedBy.Valid {
					return b.ApprovedBy.String
				}
				return ""
			}(),
			ApprovedAt: func() string {
				if b.ApprovedAt.Valid {
					return b.ApprovedAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
				}
				return ""
			}(),
		})
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "rows_err", "detail": err.Error()})
		return
	}

	auditCtx := r.Context()
	adminID, _ := api.AuthenticatedUserID(r)
	safeAuditLogAccess(auditCtx, h.auditLogger, adminID, "read", "broker_link", status, map[string]any{
		"row_count": len(out),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"links":      out,
		"status":     status,
		"row_count":  len(out),
	})
}

type approveBrokerLinkPayload struct {
	// Note is captured into the audit metadata so the reviewer can
	// record context (ticket id, broker rep name, etc.). No
	// validation beyond length — it's never shown back to users.
	Note string `json:"note,omitempty"`
}

type rejectBrokerLinkPayload struct {
	// Reason MUST be non-empty for a reject — keeps the audit
	// trail searchable ("how many were rejected for missing
	// statements?").
	Reason string `json:"reason"`
}

// handleApproveBrokerLink approves a pending broker_link with
// 4-eye enforcement: the approver MUST be a super_admin AND MUST
// NOT be the user who submitted the request.
func (h *adminHandler) handleApproveBrokerLink(w http.ResponseWriter, r *http.Request) {
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
		// Defense in depth — requireSuperAdmin should already
		// have ensured this, but a stale auth context shouldn't
		// land us in a "approved by ''" state.
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthenticated"})
		return
	}

	var body approveBrokerLinkPayload
	if r.ContentLength > 0 {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_body", "detail": err.Error()})
			return
		}
	}

	repo := repository.NewBrokerLinkRepo(h.db)

	// 4-eye check — load the row first to compare user_id.
	links, err := h.lookupBrokerLink(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "lookup_failed", "detail": err.Error()})
		return
	}
	if links == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not_found"})
		return
	}
	if links.UserID == approverID {
		// 4-eye violation — log it and reject. We surface it as
		// 403 with a specific error code so the UI can render
		// "ask a colleague to approve" rather than a generic
		// auth failure.
		safeAuditLogAccess(r.Context(), h.auditLogger, approverID,
			"broker_link.approve_blocked_4eye", "broker_link", id,
			map[string]any{
				"requester_user_id": links.UserID,
				"reason":            "approver_equals_requester",
			})
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":  "four_eye_violation",
			"detail": "approver must differ from the broker_link requester",
		})
		return
	}

	if err := repo.Approve(r.Context(), id, approverID); err != nil {
		if errors.Is(err, repository.ErrBrokerLinkNotFound) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":  "not_approvable",
				"detail": "broker_link is in a terminal state or not found",
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "approve_failed", "detail": err.Error()})
		return
	}

	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: approverID,
			Action:      "broker_link.approve",
			TargetType:  "broker_link",
			TargetID:    id,
			Before: map[string]any{
				"status": links.Status,
			},
			After: map[string]any{
				"status":      repository.BrokerLinkStatusActive,
				"approved_by": approverID,
			},
			Metadata: map[string]any{
				"requester_user_id": links.UserID,
				"fund_id":           links.FundID,
				"broker_id":         links.BrokerID,
				"note":              strings.TrimSpace(body.Note),
				"client_addr":       clientIP(r),
			},
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":     id,
		"status": repository.BrokerLinkStatusActive,
	})
}

// handleRejectBrokerLink moves a pending row to revoked. Same
// 4-eye check as approval — even rejecting your own request must
// go through a different super_admin.
func (h *adminHandler) handleRejectBrokerLink(w http.ResponseWriter, r *http.Request) {
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

	var body rejectBrokerLinkPayload
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

	repo := repository.NewBrokerLinkRepo(h.db)
	links, err := h.lookupBrokerLink(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "lookup_failed", "detail": err.Error()})
		return
	}
	if links == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not_found"})
		return
	}
	if links.UserID == approverID {
		safeAuditLogAccess(r.Context(), h.auditLogger, approverID,
			"broker_link.reject_blocked_4eye", "broker_link", id,
			map[string]any{
				"requester_user_id": links.UserID,
				"reason":            "approver_equals_requester",
			})
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":  "four_eye_violation",
			"detail": "approver must differ from the broker_link requester",
		})
		return
	}

	if err := repo.Revoke(r.Context(), id); err != nil {
		if errors.Is(err, repository.ErrBrokerLinkNotFound) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":  "not_rejectable",
				"detail": "broker_link is already revoked",
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "reject_failed", "detail": err.Error()})
		return
	}

	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: approverID,
			Action:      "broker_link.reject",
			TargetType:  "broker_link",
			TargetID:    id,
			Before: map[string]any{
				"status": links.Status,
			},
			After: map[string]any{
				"status": repository.BrokerLinkStatusRevoked,
			},
			Metadata: map[string]any{
				"requester_user_id": links.UserID,
				"fund_id":           links.FundID,
				"broker_id":         links.BrokerID,
				"reason":            strings.TrimSpace(body.Reason),
				"client_addr":       clientIP(r),
			},
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":     id,
		"status": repository.BrokerLinkStatusRevoked,
	})
}

// isValidBrokerLinkStatus enumerates the closed status vocabulary
// (mirrored from the SQL CHECK on broker_links.status). Kept
// here rather than in the repo because the repo intentionally
// stays policy-free.
func isValidBrokerLinkStatus(s string) bool {
	switch s {
	case repository.BrokerLinkStatusPending,
		repository.BrokerLinkStatusActive,
		repository.BrokerLinkStatusSuspended,
		repository.BrokerLinkStatusRevoked:
		return true
	}
	return false
}

// lookupBrokerLink loads a row by id regardless of status. Used
// by approve/reject so they can inspect the requester and current
// status before acting. Returns (nil, nil) when no row matches —
// distinguishes from a real DB error.
func (h *adminHandler) lookupBrokerLink(ctx context.Context, id string) (*repository.BrokerLink, error) {
	const q = `
		SELECT id, fund_id, user_id, broker_id, account_id, status,
		       approved_by, approved_at, credentials_encrypted, metadata,
		       created_at, updated_at
		  FROM broker_links
		 WHERE id = $1
		 LIMIT 1`
	var b repository.BrokerLink
	var metadataBytes []byte
	err := h.db.QueryRowContext(ctx, q, id).Scan(
		&b.ID, &b.FundID, &b.UserID, &b.BrokerID, &b.AccountID, &b.Status,
		&b.ApprovedBy, &b.ApprovedAt, &b.CredentialsEncrypted, &metadataBytes,
		&b.CreatedAt, &b.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(metadataBytes) == 0 {
		b.Metadata = json.RawMessage(`{}`)
	} else {
		b.Metadata = json.RawMessage(metadataBytes)
	}
	return &b, nil
}
