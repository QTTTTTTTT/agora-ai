package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
)

const (
	usageEventPageView   = "page_view"
	usageEventFeatureUse = "feature_use"
)

type usageTelemetryHandler struct {
	db *sql.DB
}

type recordUsageRequest struct {
	EventName  string         `json:"event_name"`
	FeatureKey string         `json:"feature_key"`
	PagePath   string         `json:"page_path"`
	Count      int            `json:"count"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

type recordUsageResponse struct {
	Recorded bool   `json:"recorded"`
	Reason   string `json:"reason,omitempty"`
}

type adminUsageFeatureCount struct {
	FeatureKey string `json:"feature_key"`
	EventName  string `json:"event_name"`
	Count      int64  `json:"count"`
}

type adminUsageUserAggregate struct {
	UserID      string                   `json:"user_id"`
	Email       string                   `json:"email"`
	DisplayName string                   `json:"display_name"`
	Role        string                   `json:"role"`
	TotalEvents int64                    `json:"total_events"`
	PageViews   int64                    `json:"page_views"`
	FeatureUses int64                    `json:"feature_uses"`
	ActiveDays  int64                    `json:"active_days"`
	LastSeenAt  time.Time                `json:"last_seen_at"`
	TopFeatures []adminUsageFeatureCount `json:"top_features"`
}

type adminUsageAnalyticsResponse struct {
	Since string                    `json:"since"`
	Users []adminUsageUserAggregate `json:"users"`
}

func newUsageTelemetryHandler(svc *Services) *usageTelemetryHandler {
	if svc == nil || svc.DB == nil {
		return nil
	}
	return &usageTelemetryHandler{db: svc.DB}
}

func (h *usageTelemetryHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("POST /api/telemetry/usage", h.handleRecordUsage)
}

func (h *usageTelemetryHandler) handleRecordUsage(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized", "detail": "missing or invalid bearer token"})
		return
	}

	var currentRole string
	err := h.db.QueryRowContext(r.Context(), `SELECT COALESCE(role, 'user') FROM users WHERE id = $1 AND deleted_at IS NULL`, userID).Scan(&currentRole)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized", "detail": "user not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to resolve user role", "detail": err.Error()})
		return
	}
	role := strings.ToLower(strings.TrimSpace(currentRole))
	if role == "admin" || role == adminRoleSuperAdmin {
		writeJSON(w, http.StatusOK, recordUsageResponse{Recorded: false, Reason: "admin_user_ignored"})
		return
	}

	var req recordUsageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_json", "detail": err.Error()})
		return
	}
	eventName := normalizeUsageToken(req.EventName, 64)
	if eventName == "" {
		eventName = usageEventFeatureUse
	}
	if eventName != usageEventPageView && eventName != usageEventFeatureUse {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_event_name", "detail": "event_name must be page_view or feature_use"})
		return
	}
	featureKey := normalizeUsageToken(req.FeatureKey, 160)
	if featureKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_feature_key", "detail": "feature_key is required"})
		return
	}
	pagePath := normalizeUsagePath(req.PagePath, 256)
	count := req.Count
	if count <= 0 {
		count = 1
	}
	if count > 1000 {
		count = 1000
	}
	metadata := req.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadataBytes, err := json.Marshal(metadata)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_metadata", "detail": err.Error()})
		return
	}

	_, err = h.db.ExecContext(
		r.Context(),
		`INSERT INTO user_feature_usage_events (user_id, user_role, event_name, feature_key, page_path, event_count, metadata)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)`,
		userID, role, eventName, featureKey, pagePath, count, string(metadataBytes),
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to record usage", "detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, recordUsageResponse{Recorded: true})
}

func (h *adminHandler) handleAdminUsageAnalytics(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	since := time.Now().AddDate(0, 0, -30)
	if raw := strings.TrimSpace(r.URL.Query().Get("since")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_since", "detail": "since must be RFC3339"})
			return
		}
		since = parsed
	}
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > 200 {
		limit = 200
	}

	rows, err := h.db.QueryContext(r.Context(), `
		WITH filtered AS (
			SELECT e.*
			FROM user_feature_usage_events e
			WHERE e.occurred_at >= $1
			  AND LOWER(COALESCE(e.user_role, 'user')) NOT IN ('admin', 'super_admin')
		), ranked_features AS (
			SELECT user_id, feature_key, event_name, SUM(event_count)::BIGINT AS cnt,
			       ROW_NUMBER() OVER (PARTITION BY user_id ORDER BY SUM(event_count) DESC, feature_key ASC) AS rn
			FROM filtered
			GROUP BY user_id, feature_key, event_name
		), top_features AS (
			SELECT user_id,
			       COALESCE(jsonb_agg(jsonb_build_object('feature_key', feature_key, 'event_name', event_name, 'count', cnt) ORDER BY cnt DESC, feature_key ASC), '[]'::jsonb) AS features
			FROM ranked_features
			WHERE rn <= 5
			GROUP BY user_id
		), user_rollup AS (
			SELECT e.user_id,
			       SUM(e.event_count)::BIGINT AS total_events,
			       SUM(e.event_count) FILTER (WHERE e.event_name = 'page_view')::BIGINT AS page_views,
			       SUM(e.event_count) FILTER (WHERE e.event_name = 'feature_use')::BIGINT AS feature_uses,
			       COUNT(DISTINCT DATE_TRUNC('day', e.occurred_at))::BIGINT AS active_days,
			       MAX(e.occurred_at) AS last_seen_at
			FROM filtered e
			GROUP BY e.user_id
		)
		SELECT u.id::TEXT, COALESCE(u.email, ''), COALESCE(u.display_name, ''), COALESCE(u.role, 'user'),
		       COALESCE(r.total_events, 0), COALESCE(r.page_views, 0), COALESCE(r.feature_uses, 0),
		       COALESCE(r.active_days, 0), r.last_seen_at, COALESCE(t.features, '[]'::jsonb)::TEXT
		FROM user_rollup r
		JOIN users u ON u.id = r.user_id
		LEFT JOIN top_features t ON t.user_id = r.user_id
		WHERE LOWER(COALESCE(u.role, 'user')) NOT IN ('admin', 'super_admin')
		ORDER BY r.total_events DESC, r.last_seen_at DESC
		LIMIT $2`, since, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to load usage analytics", "detail": err.Error()})
		return
	}
	defer rows.Close()

	users := make([]adminUsageUserAggregate, 0)
	for rows.Next() {
		var row adminUsageUserAggregate
		var rawFeatures string
		if err := rows.Scan(&row.UserID, &row.Email, &row.DisplayName, &row.Role, &row.TotalEvents, &row.PageViews, &row.FeatureUses, &row.ActiveDays, &row.LastSeenAt, &rawFeatures); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to scan usage analytics", "detail": err.Error()})
			return
		}
		if err := json.Unmarshal([]byte(rawFeatures), &row.TopFeatures); err != nil {
			row.TopFeatures = []adminUsageFeatureCount{}
		}
		users = append(users, row)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to load usage analytics", "detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, adminUsageAnalyticsResponse{Since: since.Format(time.RFC3339), Users: users})
}

func normalizeUsageToken(value string, maxLen int) string {
	value = strings.TrimSpace(value)
	if maxLen > 0 && len(value) > maxLen {
		value = value[:maxLen]
	}
	return value
}

func normalizeUsagePath(value string, maxLen int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	if maxLen > 0 && len(value) > maxLen {
		value = value[:maxLen]
	}
	return value
}
