// announcements.go — admin-published in-app announcements (站内信).
//
//	GET    /api/announcements                         (any logged-in user)
//	POST   /api/announcements/{id}/read               (any logged-in user)
//	GET    /api/admin/announcements                   (admin)
//	POST   /api/admin/announcements                   (admin)
//	DELETE /api/admin/announcements/{id}              (admin, soft archive)
//
// We surface announcements as a small object list rather than a
// full inbox model because the v1 use case is a sticky banner at
// the top of the app with optional severity (info/warning/critical).
// A future iteration can layer on per-recipient targeting, action
// buttons, etc. — for now the schema is intentionally minimal.

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
)

type Announcement struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	Severity    string     `json:"severity"`
	PublishedAt time.Time  `json:"publishedAt"`
	PublishedBy *string    `json:"publishedBy,omitempty"`
	ArchivedAt  *time.Time `json:"archivedAt,omitempty"`
	// Read is set when the request is hydrated for a particular
	// user — the bare row out of the DB doesn't have it.
	Read bool `json:"read,omitempty"`
}

type announcementService struct {
	db *sql.DB
}

func newAnnouncementService(db *sql.DB) *announcementService {
	return &announcementService{db: db}
}

// listForUser returns active announcements for the given user, with
// the per-row Read flag set based on `announcement_reads`. Limit is
// applied at the SQL level (we don't expect more than ~20 active
// announcements on a healthy platform).
func (s *announcementService) listForUser(ctx context.Context, userID string, limit int) ([]Announcement, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("announcements: nil service")
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	const q = `
		SELECT a.id, a.title, a.body, a.severity, a.published_at,
		       u.display_name,
		       a.archived_at,
		       (r.user_id IS NOT NULL) AS read
		FROM announcements a
		LEFT JOIN users u ON u.id = a.published_by
		LEFT JOIN announcement_reads r ON r.announcement_id = a.id AND r.user_id = $1
		WHERE a.archived_at IS NULL
		ORDER BY a.published_at DESC
		LIMIT $2`
	rows, err := s.db.QueryContext(ctx, q, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Announcement
	for rows.Next() {
		var (
			a            Announcement
			publishedBy  sql.NullString
			archivedAt   sql.NullTime
		)
		if err := rows.Scan(&a.ID, &a.Title, &a.Body, &a.Severity, &a.PublishedAt, &publishedBy, &archivedAt, &a.Read); err != nil {
			return nil, err
		}
		if publishedBy.Valid && publishedBy.String != "" {
			s := publishedBy.String
			a.PublishedBy = &s
		}
		if archivedAt.Valid {
			t := archivedAt.Time
			a.ArchivedAt = &t
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// listAll returns the full set (including archived) for the admin
// console. Doesn't carry per-user read state because admins manage
// across users.
func (s *announcementService) listAll(ctx context.Context, includeArchived bool, limit int) ([]Announcement, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("announcements: nil service")
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	q := `
		SELECT a.id, a.title, a.body, a.severity, a.published_at,
		       u.display_name,
		       a.archived_at
		FROM announcements a
		LEFT JOIN users u ON u.id = a.published_by`
	if !includeArchived {
		q += ` WHERE a.archived_at IS NULL`
	}
	q += ` ORDER BY a.published_at DESC LIMIT $1`
	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Announcement
	for rows.Next() {
		var (
			a           Announcement
			publishedBy sql.NullString
			archivedAt  sql.NullTime
		)
		if err := rows.Scan(&a.ID, &a.Title, &a.Body, &a.Severity, &a.PublishedAt, &publishedBy, &archivedAt); err != nil {
			return nil, err
		}
		if publishedBy.Valid && publishedBy.String != "" {
			s := publishedBy.String
			a.PublishedBy = &s
		}
		if archivedAt.Valid {
			t := archivedAt.Time
			a.ArchivedAt = &t
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *announcementService) create(ctx context.Context, title, body, severity, publishedBy string) (*Announcement, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("announcements: nil service")
	}
	title = strings.TrimSpace(title)
	body = strings.TrimSpace(body)
	if title == "" || body == "" {
		return nil, errors.New("title and body are required")
	}
	if severity == "" {
		severity = "info"
	}
	if severity != "info" && severity != "warning" && severity != "critical" {
		return nil, errors.New("severity must be info|warning|critical")
	}
	const ins = `
		INSERT INTO announcements (title, body, severity, published_by)
		VALUES ($1, $2, $3, $4)
		RETURNING id, title, body, severity, published_at`
	var a Announcement
	if err := s.db.QueryRowContext(ctx, ins, title, body, severity, publishedBy).Scan(&a.ID, &a.Title, &a.Body, &a.Severity, &a.PublishedAt); err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *announcementService) archive(ctx context.Context, id, archivedBy string) error {
	if s == nil || s.db == nil {
		return errors.New("announcements: nil service")
	}
	const upd = `
		UPDATE announcements
		SET archived_at = NOW(), archived_by = $2
		WHERE id = $1 AND archived_at IS NULL`
	res, err := s.db.ExecContext(ctx, upd, id, archivedBy)
	if err != nil {
		return err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return errors.New("not found or already archived")
	}
	return nil
}

func (s *announcementService) markRead(ctx context.Context, userID, announcementID string) error {
	if s == nil || s.db == nil {
		return errors.New("announcements: nil service")
	}
	const ins = `
		INSERT INTO announcement_reads (user_id, announcement_id)
		VALUES ($1, $2)
		ON CONFLICT (user_id, announcement_id) DO NOTHING`
	_, err := s.db.ExecContext(ctx, ins, userID, announcementID)
	return err
}

// --- public routes -----------------------------------------------------------

func registerAnnouncementPublicRoutes(mux *http.ServeMux, svc *announcementService) {
	if mux == nil || svc == nil {
		return
	}
	mux.HandleFunc("GET /api/announcements", func(w http.ResponseWriter, r *http.Request) {
		userID, ok := api.AuthenticatedUserID(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing token"))
			return
		}
		out, err := svc.listForUser(r.Context(), userID, 50)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"announcements": out})
	})
	mux.HandleFunc("POST /api/announcements/{id}/read", func(w http.ResponseWriter, r *http.Request) {
		userID, ok := api.AuthenticatedUserID(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing token"))
			return
		}
		id := strings.TrimSpace(r.PathValue("id"))
		if id == "" {
			writeJSON(w, http.StatusBadRequest, errorPayload("invalid_id", "missing id"))
			return
		}
		if err := svc.markRead(r.Context(), userID, id); err != nil {
			writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
}

// --- admin routes ------------------------------------------------------------

func (h *adminHandler) registerAnnouncementAdminRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil || h.announcements == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/announcements", h.handleListAdminAnnouncements)
	mux.HandleFunc("POST /api/admin/announcements", h.handleCreateAnnouncement)
	mux.HandleFunc("DELETE /api/admin/announcements/{id}", h.handleArchiveAnnouncement)
}

func (h *adminHandler) handleListAdminAnnouncements(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	includeArchived := strings.EqualFold(r.URL.Query().Get("includeArchived"), "true")
	out, err := h.announcements.listAll(r.Context(), includeArchived, 100)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"announcements": out})
}

func (h *adminHandler) handleCreateAnnouncement(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	var body struct {
		Title    string `json:"title"`
		Body     string `json:"body"`
		Severity string `json:"severity"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("decode_failed", err.Error()))
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	created, err := h.announcements.create(r.Context(), body.Title, body.Body, body.Severity, userID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid", err.Error()))
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h *adminHandler) handleArchiveAnnouncement(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_id", "missing id"))
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	if err := h.announcements.archive(r.Context(), id, userID); err != nil {
		writeJSON(w, http.StatusNotFound, errorPayload("not_found", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
