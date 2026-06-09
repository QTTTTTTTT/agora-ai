// admin_user_roles.go — admin-only role promotion / demotion.
//
//	GET  /api/admin/user-roles                  list users + role + email
//	PUT  /api/admin/user-roles/{userId}         change role
//
// Authorisation rules:
//
//   * `user → admin`   admin or super_admin can flip
//   * `admin → user`   ONLY super_admin can demote (so a regular
//                      admin can't lock out other admins)
//   * `super_admin`    only super_admin can grant or revoke; the
//                      DB also retains the "must keep at least one
//                      super_admin alive" invariant (mirrors the
//                      bootstrap path in main.go and auth_wechat.go).
//   * self-demotion    blocked outright — preventing accidental
//                      lock-outs is more valuable than a clean API.
//
// The endpoint is intentionally simple: we don't bundle promotions
// with audit metadata because the standard audit logger is wired
// through `requireAdmin` already.

package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/fundai/server/internal/api"
)

type adminUserRow struct {
	ID          string  `json:"id"`
	Email       string  `json:"email"`
	DisplayName string  `json:"displayName"`
	Role        string  `json:"role"`
	CreatedAt   string  `json:"createdAt"`
	LastLoginAt *string `json:"lastLoginAt,omitempty"`
}

// adminAllowedRoles are the only role values the admin UI is allowed
// to set. Everything else (e.g. legacy 'pm' / 'trader') is left to
// SQL migrations or super_admin direct DB edits.
var adminAllowedRoles = map[string]struct{}{
	"user":        {},
	"admin":       {},
	"super_admin": {},
}

func (h *adminHandler) registerUserRoleAdminRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/user-roles", h.handleListUserRoles)
	mux.HandleFunc("PUT /api/admin/user-roles/{userId}", h.handleUpdateUserRole)
}

func (h *adminHandler) handleListUserRoles(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	const baseQ = `
		SELECT id, COALESCE(email, ''), COALESCE(display_name, ''),
		       COALESCE(role, 'user'),
		       to_char(created_at, 'YYYY-MM-DD"T"HH24:MI:SSZ'),
		       last_login_at
		FROM users
		WHERE deleted_at IS NULL`
	const filter = ` AND (LOWER(email) LIKE $1 OR LOWER(display_name) LIKE $1)`
	const order = ` ORDER BY CASE role
				WHEN 'super_admin' THEN 0
				WHEN 'admin' THEN 1
				ELSE 2
			END, created_at DESC LIMIT 200`

	var (
		rows *sql.Rows
		err  error
	)
	if q != "" {
		// Postgres wildcard with simple lower() lookup. We don't
		// build trigram indexes here because the user table is
		// small and the admin search is interactive only.
		needle := "%" + strings.ToLower(q) + "%"
		rows, err = h.db.QueryContext(r.Context(), baseQ+filter+order, needle)
	} else {
		rows, err = h.db.QueryContext(r.Context(), baseQ+order)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	defer rows.Close()

	out := make([]adminUserRow, 0, 50)
	for rows.Next() {
		var u adminUserRow
		var last sql.NullTime
		if err := rows.Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &u.CreatedAt, &last); err != nil {
			writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
			return
		}
		if last.Valid {
			s := last.Time.UTC().Format("2006-01-02T15:04:05Z")
			u.LastLoginAt = &s
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

func (h *adminHandler) handleUpdateUserRole(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	targetID := strings.TrimSpace(r.PathValue("userId"))
	if targetID == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_id", "missing userId"))
		return
	}
	actorID, _ := api.AuthenticatedUserID(r)
	if actorID == targetID {
		writeJSON(w, http.StatusBadRequest, errorPayload("self_update", "操作者不能修改自己的角色，请让另一位管理员协助。"))
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("decode_failed", err.Error()))
		return
	}
	target := strings.ToLower(strings.TrimSpace(body.Role))
	if _, ok := adminAllowedRoles[target]; !ok {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_role", "role must be one of user|admin|super_admin"))
		return
	}

	// Look up actor + target current role in one round trip so we
	// can enforce the privilege rules below atomically vs the
	// rest of the request.
	var actorRole, targetCurrent string
	err := h.db.QueryRowContext(r.Context(),
		`SELECT
			COALESCE((SELECT role FROM users WHERE id = $1 AND deleted_at IS NULL), '') AS actor_role,
			COALESCE((SELECT role FROM users WHERE id = $2 AND deleted_at IS NULL), '') AS target_role`,
		actorID, targetID).Scan(&actorRole, &targetCurrent)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if targetCurrent == "" {
		writeJSON(w, http.StatusNotFound, errorPayload("not_found", "目标用户不存在或已停用。"))
		return
	}

	// Privilege gate. requireAdmin already established that actor
	// is at least admin; here we further constrain by what they're
	// allowed to *do*.
	if err := authorizeRoleChange(actorRole, targetCurrent, target); err != nil {
		writeJSON(w, http.StatusForbidden, errorPayload("forbidden", err.Error()))
		return
	}

	// "Must keep at least one super_admin alive" invariant. We only
	// need to check it when demoting one — promotions can never
	// reduce the count.
	if targetCurrent == adminRoleSuperAdmin && target != adminRoleSuperAdmin {
		var remaining int
		if err := h.db.QueryRowContext(r.Context(),
			`SELECT COUNT(*) FROM users WHERE role = $1 AND deleted_at IS NULL AND id <> $2`,
			adminRoleSuperAdmin, targetID).Scan(&remaining); err != nil {
			writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
			return
		}
		if remaining == 0 {
			writeJSON(w, http.StatusBadRequest, errorPayload("invariant_violation",
				"系统至少需要保留一位超级管理员，请先指定其他人为超级管理员后再降级。"))
			return
		}
	}

	if _, err := h.db.ExecContext(r.Context(),
		`UPDATE users SET role = $2, updated_at = NOW() WHERE id = $1`,
		targetID, target); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	if h.auditLogger != nil {
		_ = h.auditLogger.LogAccess(r.Context(), actorID, "admin_user_role_change", "user", targetID, map[string]any{
			"from": targetCurrent,
			"to":   target,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":   targetID,
		"role": target,
	})
}

// authorizeRoleChange enforces the matrix described in the file
// header. Returns nil when the change is allowed, or a typed error
// with a user-readable message for forbidden transitions.
func authorizeRoleChange(actorRole, currentRole, targetRole string) error {
	if actorRole == "" {
		return errors.New("actor role unresolved")
	}
	// super_admin can do anything (modulo the caller-side checks
	// for self-modification and the keep-one-super-admin invariant).
	if actorRole == adminRoleSuperAdmin {
		return nil
	}
	// Everything below is a non-super admin.
	if targetRole == adminRoleSuperAdmin {
		return errors.New("仅超级管理员可授予超级管理员权限。")
	}
	if currentRole == adminRoleSuperAdmin {
		return errors.New("仅超级管理员可降级超级管理员账号。")
	}
	if currentRole == "admin" && targetRole != "admin" {
		// 普通 admin 不能踢掉另一个 admin —— 仅超级管理员可以。
		return errors.New("仅超级管理员可降级管理员账号。")
	}
	return nil
}
