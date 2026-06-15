// support_contact_handler.go — "Need help? Contact us" config.
//
// Two endpoints:
//
//   GET /api/support-contact         (public, any authenticated user)
//   PUT /api/admin/support-contact   (super_admin only)
//
// Single-row config in the support_contact table (migration 115).
// The /api/support-contact endpoint is the data source for the
// floating "Get help" button rendered globally on every SPA page.
// Disabled rows return enabled=false so the SPA hides the button.

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// supportContact is the on-the-wire JSON shape for both GET and PUT.
type supportContact struct {
	Enabled     bool      `json:"enabled"`
	DiscordURL  string    `json:"discordUrl"`
	QRImageURL  string    `json:"qrImageUrl"`
	Message     string    `json:"message"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// loadSupportContact returns the singleton row, falling back to a
// disabled empty config when the row is missing (defensive — the
// migration seeds one, but a fresh deploy may race).
func loadSupportContact(ctx context.Context, db *sql.DB) (*supportContact, error) {
	if db == nil {
		return &supportContact{}, nil
	}
	const q = `SELECT enabled, discord_url, qr_image_url, message, updated_at
	             FROM support_contact WHERE id = TRUE LIMIT 1`
	out := &supportContact{}
	err := db.QueryRowContext(ctx, q).Scan(&out.Enabled, &out.DiscordURL, &out.QRImageURL, &out.Message, &out.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return &supportContact{}, nil
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// handleGetSupportContact serves GET /api/support-contact. No
// requireSuperAdmin gate — the SPA needs this for every logged-in
// user (and the auth layer already gates /api/* via the standard
// middleware unless explicitly public). Sensitive data isn't here:
// the config is always-on platform metadata.
func (h *adminHandler) handleGetSupportContact(w http.ResponseWriter, r *http.Request) {
	cfg, err := loadSupportContact(r.Context(), h.db)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "load support contact failed", "detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// handleUpdateSupportContact serves PUT /api/admin/support-contact.
// super_admin gated.
func (h *adminHandler) handleUpdateSupportContact(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	if h.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "database unavailable"})
		return
	}
	defer r.Body.Close()

	var payload struct {
		Enabled    bool   `json:"enabled"`
		DiscordURL string `json:"discordUrl"`
		QRImageURL string `json:"qrImageUrl"`
		Message    string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body", "detail": err.Error()})
		return
	}

	discord := strings.TrimSpace(payload.DiscordURL)
	qr := strings.TrimSpace(payload.QRImageURL)
	message := strings.TrimSpace(payload.Message)

	// Light URL hygiene — accept empty (means "not configured") OR a
	// http(s) URL. The button's UX treats either DiscordURL or
	// QRImageURL as sufficient, so neither is required individually.
	if discord != "" && !strings.HasPrefix(discord, "http://") && !strings.HasPrefix(discord, "https://") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "discordUrl must be http(s) URL"})
		return
	}
	if qr != "" && !strings.HasPrefix(qr, "http://") && !strings.HasPrefix(qr, "https://") && !strings.HasPrefix(qr, "data:image/") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "qrImageUrl must be http(s) URL or data:image/..."})
		return
	}

	const q = `
		INSERT INTO support_contact (id, enabled, discord_url, qr_image_url, message, updated_at)
		VALUES (TRUE, $1, $2, $3, $4, NOW())
		ON CONFLICT (id) DO UPDATE
		SET enabled = EXCLUDED.enabled,
		    discord_url = EXCLUDED.discord_url,
		    qr_image_url = EXCLUDED.qr_image_url,
		    message = EXCLUDED.message,
		    updated_at = NOW()
		RETURNING enabled, discord_url, qr_image_url, message, updated_at
	`
	out := &supportContact{}
	err := h.db.QueryRowContext(r.Context(), q, payload.Enabled, discord, qr, message).
		Scan(&out.Enabled, &out.DiscordURL, &out.QRImageURL, &out.Message, &out.UpdatedAt)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "update support contact failed", "detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, out)
}
