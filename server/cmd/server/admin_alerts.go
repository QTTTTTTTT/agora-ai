// admin_alerts.go — Sprint 12.2 alertmanager webhook + admin alert
// inspection endpoints.
//
//	POST  /api/admin/alerts/webhook            alertmanager webhook in
//	GET   /api/admin/alerts                    list recent alert events
//	PATCH /api/admin/alerts/{id}/ack           admin acknowledgement
//
// The webhook receiver is intentionally separate from requireAdmin —
// alertmanager doesn't carry a user-id, it carries a shared secret.
// Authentication is by Bearer token (FUNDAI_ALERT_WEBHOOK_SECRET).
//
// The list / ack routes go through requireAdmin like every other
// admin endpoint and emit audit-log entries for the ack action.

package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/repository"
)

func (h *adminHandler) registerAlertAdminRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil || h.alertEventRepo == nil {
		return
	}
	// The webhook is intentionally registered even when the repo is
	// unwired in non-prod builds — except… actually no, no repo means
	// nowhere to persist, so we keep the same nil-safe pattern as
	// the other admin sub-modules.
	mux.HandleFunc("POST /api/admin/alerts/webhook", h.handleAlertWebhook)
	mux.HandleFunc("GET /api/admin/alerts", h.handleListAlertEvents)
	mux.HandleFunc("PATCH /api/admin/alerts/{id}/ack", h.handleAcknowledgeAlert)
}

// --- alertmanager wire shape -------------------------------------------------

// alertmanagerPayload mirrors the alertmanager 0.27 webhook v4
// schema. We only decode the fields we actually persist.
type alertmanagerPayload struct {
	Version           string             `json:"version"`
	Status            string             `json:"status"`
	Receiver          string             `json:"receiver"`
	GroupKey          string             `json:"groupKey"`
	GroupLabels       map[string]string  `json:"groupLabels"`
	CommonLabels      map[string]string  `json:"commonLabels"`
	CommonAnnotations map[string]string  `json:"commonAnnotations"`
	ExternalURL       string             `json:"externalURL"`
	Alerts            []alertmanagerItem `json:"alerts"`
}

type alertmanagerItem struct {
	Fingerprint  string            `json:"fingerprint"`
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
}

// --- handlers ----------------------------------------------------------------

func (h *adminHandler) handleAlertWebhook(w http.ResponseWriter, r *http.Request) {
	if !h.authWebhook(w, r) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("read_failed", err.Error()))
		return
	}
	defer r.Body.Close()
	var payload alertmanagerPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("decode_failed", err.Error()))
		return
	}
	ingested := 0
	deduped := 0
	failed := 0
	for _, item := range payload.Alerts {
		ev := alertEventFromWire(item)
		_, err := h.alertEventRepo.Insert(r.Context(), &ev)
		if errors.Is(err, repository.ErrAlertEventDuplicate) {
			deduped++
			continue
		}
		if err != nil {
			failed++
			continue
		}
		ingested++
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ingested": ingested,
		"deduped":  deduped,
		"failed":   failed,
	})
}

func (h *adminHandler) handleListAlertEvents(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	status := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconvAtoiBounded(raw, 1, 500); err == nil {
			limit = n
		}
	}
	events, err := h.alertEventRepo.ListRecent(r.Context(), status, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("list_failed", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (h *adminHandler) handleAcknowledgeAlert(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("missing_id", "id is required"))
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, errorPayload("decode_failed", err.Error()))
			return
		}
	}
	userID, _ := api.AuthenticatedUserID(r)
	if err := h.alertEventRepo.Acknowledge(r.Context(), id, userID, body.Note); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, errorPayload("not_found", "alert event not found"))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorPayload("ack_failed", err.Error()))
		return
	}
	// Audit the ack. We use MutationEvent rather than DataAccess
	// because acking is an admin-state-changing action that the
	// compliance team queries for incident retrospectives.
	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "alert_acknowledge",
			TargetType:  "admin_alert_events",
			TargetID:    id,
			Metadata:    map[string]any{"note": body.Note},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- helpers -----------------------------------------------------------------

// authWebhook accepts the request when the Authorization header
// matches FUNDAI_ALERT_WEBHOOK_SECRET. Empty server-side secret
// rejects everything — we do NOT want a no-secret deploy to silently
// accept arbitrary alert payloads.
func (h *adminHandler) authWebhook(w http.ResponseWriter, r *http.Request) bool {
	want := strings.TrimSpace(os.Getenv("FUNDAI_ALERT_WEBHOOK_SECRET"))
	if want == "" {
		writeJSON(w, http.StatusServiceUnavailable, errorPayload("not_configured", "FUNDAI_ALERT_WEBHOOK_SECRET is not set"))
		return false
	}
	got := strings.TrimSpace(r.Header.Get("Authorization"))
	got = strings.TrimPrefix(got, "Bearer ")
	got = strings.TrimSpace(got)
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		writeJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "invalid webhook bearer"))
		return false
	}
	return true
}

func alertEventFromWire(item alertmanagerItem) repository.AlertEvent {
	severity := strings.ToLower(strings.TrimSpace(item.Labels["severity"]))
	if severity == "" {
		severity = "warning"
	}
	component := strings.TrimSpace(item.Labels["component"])
	summary := strings.TrimSpace(item.Annotations["summary"])
	description := strings.TrimSpace(item.Annotations["description"])
	labelsJSON, _ := json.Marshal(item.Labels)
	annotationsJSON, _ := json.Marshal(item.Annotations)
	ev := repository.AlertEvent{
		Fingerprint: strings.TrimSpace(item.Fingerprint),
		AlertName:   strings.TrimSpace(item.Labels["alertname"]),
		Severity:    severity,
		Component:   component,
		Status:      strings.ToLower(strings.TrimSpace(item.Status)),
		Summary:     summary,
		Description: description,
		Labels:      labelsJSON,
		Annotations: annotationsJSON,
		StartsAt:    item.StartsAt,
	}
	if !item.EndsAt.IsZero() && ev.Status == "resolved" {
		t := item.EndsAt
		ev.EndsAt = &t
	}
	return ev
}

// strconvAtoiBounded is a small helper to keep handler bodies tidy.
// Returns ErrBadValue when the parsed integer is outside [lo, hi].
func strconvAtoiBounded(raw string, lo, hi int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, errors.New("empty")
	}
	n := 0
	sign := 1
	for i, c := range raw {
		if i == 0 && (c == '-' || c == '+') {
			if c == '-' {
				sign = -1
			}
			continue
		}
		if c < '0' || c > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int(c-'0')
	}
	n *= sign
	if n < lo {
		return lo, nil
	}
	if n > hi {
		return hi, nil
	}
	return n, nil
}
