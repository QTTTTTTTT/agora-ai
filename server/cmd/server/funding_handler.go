// Funding-request HTTP handler (P1-2).
//
// Self-service surface for fund owners:
//
//	POST   /api/funds/{fundId}/funding-requests
//	    Submit a deposit / withdrawal request. The row lands in
//	    'pending' state until a different admin approves it
//	    (4-eye via admin_funding.go).
//
//	GET    /api/funds/{fundId}/funding-requests
//	    Lists requests for the fund (any status), newest-first.
//	    Used by the wallet UI to render history + the pending
//	    queue at the top.
//
//	POST   /api/funds/{fundId}/funding-requests/{id}/cancel
//	    Self-cancel a pending request. Only the original
//	    requester can cancel; admins use the admin reject path.
//
// Why no /post or /confirm
//
// Approval is the operation that actually moves cash. The user
// only models intent ("I want to deposit $X"). Posting is
// approve-time and lives in admin_funding.go where the 4-eye
// check + cash_ledger insert happen atomically.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/repository"
)

type fundingHandler struct {
	fundingRepo *repository.FundingRepo
	fundRepo    *repository.FundRepo
	companyRepo *repository.FundCompanyRepo
	cashLedger  *repository.CashLedgerRepo
	auditLogger audit.Logger
	metrics     *serverMetrics
	log         *slog.Logger
}

func newFundingHandler(svc *Services) *fundingHandler {
	if svc == nil || svc.DB == nil {
		return nil
	}
	return &fundingHandler{
		fundingRepo: repository.NewFundingRepo(svc.DB),
		fundRepo:    repository.NewFundRepo(svc.DB),
		companyRepo: repository.NewFundCompanyRepo(svc.DB),
		cashLedger:  repository.NewCashLedgerRepo(svc.DB),
		auditLogger: audit.NewDBLogger(svc.DB),
		metrics:     svc.Metrics,
		log:         slog.Default(),
	}
}

func (h *fundingHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("POST /api/funds/{fundId}/funding-requests", h.handleCreate)
	mux.HandleFunc("GET /api/funds/{fundId}/funding-requests", h.handleList)
	mux.HandleFunc("POST /api/funds/{fundId}/funding-requests/{id}/cancel", h.handleCancel)
}

type createFundingRequest struct {
	Direction         string  `json:"direction"`
	Amount            float64 `json:"amount"`
	Currency          string  `json:"currency,omitempty"`
	Method            string  `json:"method"`
	ExternalReference string  `json:"externalReference,omitempty"`
	Notes             string  `json:"notes,omitempty"`
}

// fundingRequestResponse is the wire shape. We keep it stable
// across user + admin paths so the same TS interface in
// shared/api-client can render either.
type fundingRequestResponse struct {
	ID                 string  `json:"id"`
	FundID             string  `json:"fundId"`
	Direction          string  `json:"direction"`
	Amount             float64 `json:"amount"`
	Currency           string  `json:"currency"`
	Method             string  `json:"method"`
	ExternalReference  string  `json:"externalReference,omitempty"`
	Status             string  `json:"status"`
	RequestedBy        string  `json:"requestedBy"`
	ApprovedBy         string  `json:"approvedBy,omitempty"`
	ApprovedAt         string  `json:"approvedAt,omitempty"`
	RejectedBy         string  `json:"rejectedBy,omitempty"`
	RejectedAt         string  `json:"rejectedAt,omitempty"`
	RejectionReason    string  `json:"rejectionReason,omitempty"`
	CancelledAt        string  `json:"cancelledAt,omitempty"`
	CashLedgerEntryID  string  `json:"cashLedgerEntryId,omitempty"`
	Notes              string  `json:"notes,omitempty"`
	CreatedAt          string  `json:"createdAt"`
	UpdatedAt          string  `json:"updatedAt"`
}

func (h *fundingHandler) handleCreate(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	if fundID == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "fundId required"))
		return
	}

	var body createFundingRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	body.Direction = strings.ToLower(strings.TrimSpace(body.Direction))
	body.Method = strings.ToLower(strings.TrimSpace(body.Method))

	ctx := r.Context()
	if _, err := authorizeFundAccess(ctx, h.fundRepo, h.companyRepo, userID, fundID); err != nil {
		writeOrderActionFromAuthError(w, err)
		return
	}

	id, err := h.fundingRepo.Create(ctx, repository.CreateFundingRequestParams{
		FundID:            fundID,
		Direction:         body.Direction,
		Amount:            body.Amount,
		Currency:          body.Currency,
		Method:            body.Method,
		ExternalReference: body.ExternalReference,
		RequestedBy:       userID,
		Notes:             body.Notes,
		Metadata: map[string]any{
			"client_addr": clientIP(r),
		},
	})
	if err != nil {
		if errors.Is(err, repository.ErrFundingRequestInvalid) {
			writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
			return
		}
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	h.logAudit(ctx, audit.MutationEvent{
		ActorUserID: userID,
		Action:      "funding_request.create",
		TargetType:  "funding_request",
		TargetID:    id,
		After: map[string]any{
			"fund_id":   fundID,
			"direction": body.Direction,
			"amount":    body.Amount,
			"currency":  body.Currency,
			"method":    body.Method,
			"status":    repository.FundingStatusPending,
		},
	})
	if h.metrics != nil {
		h.metrics.RecordFundingRequestEvent("created")
	}

	writeOrderActionJSON(w, http.StatusCreated, map[string]any{
		"id":     id,
		"status": repository.FundingStatusPending,
	})
}

func (h *fundingHandler) handleList(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	if fundID == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "fundId required"))
		return
	}
	ctx := r.Context()
	if _, err := authorizeFundAccess(ctx, h.fundRepo, h.companyRepo, userID, fundID); err != nil {
		writeOrderActionFromAuthError(w, err)
		return
	}

	q := r.URL.Query()
	limit := 100
	if raw := q.Get("limit"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			limit = v
		}
	}
	statuses := q["status"]

	rows, err := h.fundingRepo.ListByFund(ctx, fundID, repository.ListFundingByFundParams{
		Statuses: statuses,
		Limit:    limit,
	})
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	out := make([]fundingRequestResponse, 0, len(rows))
	for i := range rows {
		out = append(out, projectFundingRequest(&rows[i]))
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"requests": out})
}

func (h *fundingHandler) handleCancel(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	id := strings.TrimSpace(r.PathValue("id"))
	if fundID == "" || id == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "fundId and id required"))
		return
	}

	ctx := r.Context()
	if _, err := authorizeFundAccess(ctx, h.fundRepo, h.companyRepo, userID, fundID); err != nil {
		writeOrderActionFromAuthError(w, err)
		return
	}

	// Read first so we can return a useful error code (404 vs.
	// 409) and so the audit log captures the prior state.
	row, err := h.fundingRepo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrFundingRequestNotFound) {
			writeOrderActionJSON(w, http.StatusNotFound, errorPayload("not_found", "funding_request not found"))
			return
		}
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if row.FundID != fundID {
		writeOrderActionJSON(w, http.StatusNotFound, errorPayload("not_found", "funding_request not found"))
		return
	}
	if row.Status != repository.FundingStatusPending {
		writeOrderActionJSON(w, http.StatusConflict, errorPayload("already_processed", "request is no longer pending"))
		return
	}

	if err := h.fundingRepo.Cancel(ctx, id, userID); err != nil {
		if errors.Is(err, repository.ErrFundingRequestStateConflict) {
			writeOrderActionJSON(w, http.StatusForbidden, errorPayload("forbidden", "only the original requester can cancel"))
			return
		}
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	h.logAudit(ctx, audit.MutationEvent{
		ActorUserID: userID,
		Action:      "funding_request.cancel",
		TargetType:  "funding_request",
		TargetID:    id,
		Before:      map[string]any{"status": row.Status},
		After:       map[string]any{"status": repository.FundingStatusCancelled},
	})
	if h.metrics != nil {
		h.metrics.RecordFundingRequestEvent("cancelled")
	}

	writeOrderActionJSON(w, http.StatusOK, map[string]any{"status": repository.FundingStatusCancelled})
}

func projectFundingRequest(r *repository.FundingRequest) fundingRequestResponse {
	out := fundingRequestResponse{
		ID:          r.ID,
		FundID:      r.FundID,
		Direction:   r.Direction,
		Amount:      r.Amount,
		Currency:    r.Currency,
		Method:      r.Method,
		Status:      r.Status,
		RequestedBy: r.RequestedBy,
		CreatedAt:   r.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:   r.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if r.ExternalReference.Valid {
		out.ExternalReference = r.ExternalReference.String
	}
	if r.ApprovedBy.Valid {
		out.ApprovedBy = r.ApprovedBy.String
	}
	if r.ApprovedAt.Valid {
		out.ApprovedAt = r.ApprovedAt.Time.UTC().Format(time.RFC3339Nano)
	}
	if r.RejectedBy.Valid {
		out.RejectedBy = r.RejectedBy.String
	}
	if r.RejectedAt.Valid {
		out.RejectedAt = r.RejectedAt.Time.UTC().Format(time.RFC3339Nano)
	}
	if r.RejectionReason.Valid {
		out.RejectionReason = r.RejectionReason.String
	}
	if r.CancelledAt.Valid {
		out.CancelledAt = r.CancelledAt.Time.UTC().Format(time.RFC3339Nano)
	}
	if r.CashLedgerEntryID.Valid {
		out.CashLedgerEntryID = r.CashLedgerEntryID.String
	}
	if r.Notes.Valid {
		out.Notes = r.Notes.String
	}
	return out
}

func (h *fundingHandler) logAudit(ctx context.Context, evt audit.MutationEvent) {
	if h == nil || h.auditLogger == nil {
		return
	}
	if err := h.auditLogger.LogMutation(ctx, evt); err != nil {
		h.log.Warn("funding handler: audit log failed", "action", evt.Action, "err", err)
	}
}
