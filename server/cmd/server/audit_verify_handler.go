package main

import (
	"context"
	"net/http"
	"time"

	"github.com/fundai/server/internal/audit"
)

// auditVerifyHandler exposes admin-only endpoints to walk the audit
// hash chain (P0-8) and surface tamper. Mounted at:
//
//	GET /api/admin/audit/chain/verify        — both tables
//	GET /api/admin/audit/chain/verify/access — data_access_log only
//	GET /api/admin/audit/chain/verify/admin  — admin_change_log only
//
// All three endpoints are super-admin gated. They are read-only and
// safe to call repeatedly; verification cost is O(n) over chained rows
// and is not cached so operators always see the freshest state.
type auditVerifyHandler struct {
	verifier *audit.Verifier
}

func newAuditVerifyHandler(svc *Services) *auditVerifyHandler {
	if svc == nil || svc.DB == nil {
		return nil
	}
	return &auditVerifyHandler{verifier: audit.NewVerifier(svc.DB)}
}

// RegisterRoutes wires the verify endpoints on mux. Idempotent on a
// nil receiver so callers don't need a separate nil guard.
func (h *auditVerifyHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/audit/chain/verify", h.handleVerifyAll)
	mux.HandleFunc("GET /api/admin/audit/chain/verify/access", h.handleVerifyAccess)
	mux.HandleFunc("GET /api/admin/audit/chain/verify/admin", h.handleVerifyMutation)
}

// handleVerifyAll runs both verifiers and returns a combined report.
// Status is "ok" iff every chain is "ok" or "empty"; any "failed"
// surfaces in the response body and a 200 is still returned (the
// caller asked for a report, not for the chain to be repaired). HTTP
// errors (5xx) are reserved for actual infrastructure failures.
func (h *auditVerifyHandler) handleVerifyAll(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	access, err := h.verifier.VerifyAccessChain(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":  "verify_access_failed",
			"detail": err.Error(),
		})
		return
	}
	mutation, err := h.verifier.VerifyMutationChain(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":  "verify_mutation_failed",
			"detail": err.Error(),
		})
		return
	}

	overall := combinedStatus(access.Status, mutation.Status)
	writeJSON(w, http.StatusOK, map[string]any{
		"overall":  overall,
		"access":   access,
		"mutation": mutation,
	})
}

func (h *auditVerifyHandler) handleVerifyAccess(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	rep, err := h.verifier.VerifyAccessChain(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":  "verify_access_failed",
			"detail": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (h *auditVerifyHandler) handleVerifyMutation(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	rep, err := h.verifier.VerifyMutationChain(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":  "verify_mutation_failed",
			"detail": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// combinedStatus folds two per-chain statuses into a single
// human-readable summary. "failed" wins over "ok"; "ok" wins over
// "empty" so the caller knows at least one chain has data.
func combinedStatus(a, b audit.VerificationStatus) audit.VerificationStatus {
	if a == audit.VerificationFailed || b == audit.VerificationFailed {
		return audit.VerificationFailed
	}
	if a == audit.VerificationOK || b == audit.VerificationOK {
		return audit.VerificationOK
	}
	return audit.VerificationEmpty
}
