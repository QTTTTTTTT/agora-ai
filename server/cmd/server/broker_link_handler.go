// Broker-link HTTP handler (P1-6).
//
// Exposes self-service endpoints that let a fund owner request a
// broker-link, list their own links, and revoke an active link.
// The 4-eye approval workflow lives in the admin handler
// (admin_broker_links.go) — keeping the user surface and the
// admin surface in separate files mirrors the cancel/replace
// vs. admin/audit split, and makes it easy to lock the admin
// file behind a stricter review process.
//
// Routes
//
//	POST /api/funds/{fundId}/broker-links
//	    Body: { broker_id, account_id, metadata? }
//	    Effect: inserts a row in status='pending'. The fund owner
//	            can have multiple pending requests across brokers
//	            but at most one ACTIVE link (DB partial UNIQUE
//	            broker_links_one_active_per_fund_idx).
//
//	GET  /api/funds/{fundId}/broker-links
//	    Lists all link rows for the fund (any status), newest
//	    first. Used by the AccountSecurity / FundSettings page to
//	    render the per-broker status badges.
//
//	POST /api/funds/{fundId}/broker-links/{linkId}/revoke
//	    Self-revoke. Useful when the operator rotates broker API
//	    keys: revoke the current link, create a fresh one, ask an
//	    admin to re-approve. The hard-gate (P0-9) starts blocking
//	    cancel/replace immediately because there's no active row.
//
// Why no /reject from the user
//
// A user can revoke their OWN active link, but cannot reject a
// pending request — that's an admin (4-eye) action. We surface
// "cancel my request" via the same revoke endpoint, which moves
// the row to terminal 'revoked' regardless of source state.
//
// Audit
//
// Every state-changing call writes a hash-chained audit row via
// audit.MutationEvent so a later forensic check can rebuild the
// who/when sequence. Read calls do NOT audit — link configuration
// is reasonably private but not high-sensitivity, and the
// account-security page polls this on every render.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/repository"
)

type brokerLinkHandler struct {
	brokerLinkRepo *repository.BrokerLinkRepo
	fundRepo       *repository.FundRepo
	companyRepo    *repository.FundCompanyRepo
	auditLogger    audit.Logger
	log            *slog.Logger
}

// newBrokerLinkHandler returns nil when the wiring is missing —
// matches the rest of the handler-registration pattern in main.go.
func newBrokerLinkHandler(svc *Services) *brokerLinkHandler {
	if svc == nil || svc.DB == nil {
		return nil
	}
	return &brokerLinkHandler{
		brokerLinkRepo: repository.NewBrokerLinkRepo(svc.DB),
		fundRepo:       repository.NewFundRepo(svc.DB),
		companyRepo:    repository.NewFundCompanyRepo(svc.DB),
		auditLogger:    audit.NewDBLogger(svc.DB),
		log:            slog.Default(),
	}
}

func (h *brokerLinkHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("POST /api/funds/{fundId}/broker-links", h.handleCreate)
	mux.HandleFunc("GET /api/funds/{fundId}/broker-links", h.handleList)
	mux.HandleFunc("POST /api/funds/{fundId}/broker-links/{linkId}/revoke", h.handleRevoke)
}

// allowedBrokerIDs is the closed vocabulary of broker identifiers
// we accept. We deliberately keep this short and additive — once
// the team commits to integrating an additional broker the
// migration adds the value to a future enum, NOT this set.
var allowedBrokerIDs = map[string]bool{
	"ibkr":    true,
	"futu":    true,
	"alpaca":  true,
	"binance": true,
	"mock":    true,
}

type createBrokerLinkRequest struct {
	BrokerID  string          `json:"brokerId"`
	AccountID string          `json:"accountId"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
}

type brokerLinkResponse struct {
	ID         string `json:"id"`
	FundID     string `json:"fundId"`
	UserID     string `json:"userId"`
	BrokerID   string `json:"brokerId"`
	AccountID  string `json:"accountId"`
	Status     string `json:"status"`
	ApprovedBy string `json:"approvedBy,omitempty"`
	ApprovedAt string `json:"approvedAt,omitempty"`
	CreatedAt  string `json:"createdAt"`
	UpdatedAt  string `json:"updatedAt"`
}

func (h *brokerLinkHandler) handleCreate(w http.ResponseWriter, r *http.Request) {
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
	var body createBrokerLinkRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	body.BrokerID = strings.ToLower(strings.TrimSpace(body.BrokerID))
	body.AccountID = strings.TrimSpace(body.AccountID)
	if body.BrokerID == "" || body.AccountID == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", "brokerId and accountId required"))
		return
	}
	if !allowedBrokerIDs[body.BrokerID] {
		// Closed vocabulary keeps the gate reasoning local — a
		// typo'd broker_id can never produce an "active" link.
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_broker", "unknown broker_id"))
		return
	}

	ctx := r.Context()
	if _, err := authorizeFundAccess(ctx, h.fundRepo, h.companyRepo, userID, fundID); err != nil {
		writeOrderActionFromAuthError(w, err)
		return
	}

	id, err := h.brokerLinkRepo.Create(ctx, repository.CreateParams{
		FundID:    fundID,
		UserID:    userID,
		BrokerID:  body.BrokerID,
		AccountID: body.AccountID,
		Metadata:  body.Metadata,
	})
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	// Audit: pending request creation. Records the broker_id +
	// (truncated) account_id so a later approver can decide
	// without re-querying the user.
	h.logAudit(ctx, audit.MutationEvent{
		ActorUserID: userID,
		Action:      "broker_link.request",
		TargetType:  "broker_link",
		TargetID:    id,
		After: map[string]any{
			"fund_id":    fundID,
			"broker_id":  body.BrokerID,
			"account_id": redactAccountID(body.AccountID),
			"status":     repository.BrokerLinkStatusPending,
		},
		Metadata: map[string]any{
			"client_addr": clientIP(r),
		},
	})

	writeOrderActionJSON(w, http.StatusCreated, map[string]any{
		"link_id": id,
		"status":  repository.BrokerLinkStatusPending,
	})
}

func (h *brokerLinkHandler) handleList(w http.ResponseWriter, r *http.Request) {
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
	links, err := h.brokerLinkRepo.ListByFundID(ctx, fundID)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	out := make([]brokerLinkResponse, 0, len(links))
	for i := range links {
		out = append(out, projectBrokerLink(&links[i]))
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"links": out})
}

func (h *brokerLinkHandler) handleRevoke(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	linkID := strings.TrimSpace(r.PathValue("linkId"))
	if fundID == "" || linkID == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "fundId and linkId required"))
		return
	}

	ctx := r.Context()
	if _, err := authorizeFundAccess(ctx, h.fundRepo, h.companyRepo, userID, fundID); err != nil {
		writeOrderActionFromAuthError(w, err)
		return
	}

	// Defense in depth: read the row first to verify it actually
	// belongs to this fund + user. A 404 on the wrong link beats
	// silently revoking the wrong row.
	links, err := h.brokerLinkRepo.ListByFundID(ctx, fundID)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	var target *repository.BrokerLink
	for i := range links {
		if links[i].ID == linkID {
			target = &links[i]
			break
		}
	}
	if target == nil {
		writeOrderActionJSON(w, http.StatusNotFound, errorPayload("not_found", "broker_link not found"))
		return
	}
	// User can only revoke their own row. An admin uses the
	// admin endpoint for that — different audit action label.
	if target.UserID != userID {
		writeOrderActionJSON(w, http.StatusForbidden, errorPayload("forbidden", "broker_link belongs to another user"))
		return
	}

	if err := h.brokerLinkRepo.Revoke(ctx, linkID); err != nil {
		if errors.Is(err, repository.ErrBrokerLinkNotFound) {
			writeOrderActionJSON(w, http.StatusConflict, errorPayload("already_revoked", "broker_link is already revoked"))
			return
		}
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	h.logAudit(ctx, audit.MutationEvent{
		ActorUserID: userID,
		Action:      "broker_link.revoke",
		TargetType:  "broker_link",
		TargetID:    linkID,
		Before: map[string]any{
			"status": target.Status,
		},
		After: map[string]any{
			"status": repository.BrokerLinkStatusRevoked,
		},
		Metadata: map[string]any{
			"fund_id":     fundID,
			"client_addr": clientIP(r),
			"actor":       "user_self",
		},
	})

	writeOrderActionJSON(w, http.StatusOK, map[string]any{
		"link_id": linkID,
		"status":  repository.BrokerLinkStatusRevoked,
	})
}

// projectBrokerLink converts a repository row to the wire shape.
// We deliberately drop credentials_encrypted (callers never need
// to read API keys) and the metadata blob (typically broker-
// specific routing hints we don't want to leak in plain text on
// every account-security page poll).
func projectBrokerLink(b *repository.BrokerLink) brokerLinkResponse {
	out := brokerLinkResponse{
		ID:        b.ID,
		FundID:    b.FundID,
		UserID:    b.UserID,
		BrokerID:  b.BrokerID,
		AccountID: redactAccountID(b.AccountID),
		Status:    b.Status,
		CreatedAt: b.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt: b.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
	if b.ApprovedBy.Valid {
		out.ApprovedBy = b.ApprovedBy.String
	}
	if b.ApprovedAt.Valid {
		out.ApprovedAt = b.ApprovedAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return out
}

// redactAccountID masks all but the last 4 characters of the
// broker account id. The full value is stored in the DB but
// never returned over the API — even fund owners see the
// redacted form, which matches what real broker statements show.
func redactAccountID(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 4 {
		return strings.Repeat("•", len(s))
	}
	return strings.Repeat("•", len(s)-4) + s[len(s)-4:]
}

func (h *brokerLinkHandler) logAudit(ctx context.Context, ev audit.MutationEvent) {
	if h == nil || h.auditLogger == nil {
		return
	}
	if err := h.auditLogger.LogMutation(ctx, ev); err != nil {
		h.log.Warn("broker_link audit write failed",
			"action", ev.Action, "target_id", ev.TargetID, "err", err.Error())
	}
}
