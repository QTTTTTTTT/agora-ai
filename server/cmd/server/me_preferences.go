package main

// me_preferences.go — minimal "user preferences" surface so the frontend
// can persist the language choice that drives both client-side i18next
// resource selection and the server-side i18nmsg bundle (via
// users.preferred_language).
//
// Why a brand-new endpoint instead of folding the field into the existing
// auth flow?
//
//  1. The auth flow (register / login / wechat-login) is already
//     write-once for the user row; preferences are *update-many*. Mixing
//     the two would force every preference change through a privileged
//     code path.
//  2. /api/auth/session is a cheap GET; PATCHing it would muddle the
//     "reflect current session token" contract that the frontend's
//     SessionExpiryWatcher depends on.
//  3. Keeping preferences on a dedicated path (PATCH /api/me/preferences)
//     makes it easy to expand later (theme, density, timezone overrides)
//     without re-touching auth.
//
// Validation is deliberately strict: the only currently-supported
// languages are zh-CN and en-US (mirroring the bundle's Locale enum and
// the migration's CHECK constraint). Anything else returns 400 so an
// out-of-set value can't slip past us and surface as "!!MISSING:" in
// production.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/i18nmsg"
)

type mePreferencesRequest struct {
	// Language is the BCP-47 locale tag. Only "zh-CN" and "en-US" are
	// accepted today; anything else is rejected at the validation
	// layer. The pointer lets a caller send `{}` to fetch-only the
	// current preferences without writing.
	Language *string `json:"language,omitempty"`
}

type mePreferencesResponse struct {
	PreferredLanguage string `json:"preferred_language"`
}

// handlePatchMePreferences implements PATCH /api/me/preferences. It updates
// users.preferred_language for the authenticated user and returns the
// resulting state so the frontend can confirm the write.
func handlePatchMePreferences(svc *Services) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := ensureRequestID(w, r)
		if r.Method != http.MethodPatch {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
				"error":      "method not allowed",
				"request_id": requestID,
			})
			return
		}
		userID, ok := api.AuthenticatedUserID(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error":      "unauthenticated",
				"request_id": requestID,
			})
			return
		}
		if svc == nil || svc.DB == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error":      "service unavailable",
				"request_id": requestID,
			})
			return
		}

		var req mePreferencesRequest
		if r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error":      "invalid body",
					"request_id": requestID,
				})
				return
			}
		}

		if req.Language != nil {
			normalised := i18nmsg.Normalize(*req.Language)
			rawTrimmed := strings.TrimSpace(*req.Language)
			// Reject anything that doesn't normalise back to the input
			// (e.g. "klingon" -> LocaleZH would let garbage through).
			// The Normalize contract collapses unknown values to ZH, so
			// we re-validate here against the canonical set.
			if !(normalised == i18nmsg.LocaleZH || normalised == i18nmsg.LocaleEN) || rawTrimmed == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error":      "invalid language",
					"detail":     "language must be zh-CN or en-US",
					"request_id": requestID,
				})
				return
			}
			if err := updateUserPreferredLanguage(r.Context(), svc.DB, userID, string(normalised)); err != nil {
				slog.Error("failed to persist preferred_language",
					"request_id", requestID, "user_id", userID, "error", err)
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error":      "failed to update preferences",
					"request_id": requestID,
				})
				return
			}
		}

		current, err := loadActiveUserByID(r.Context(), svc.DB, userID)
		if err != nil {
			if errors.Is(err, errUserNotFoundOrInactive) {
				writeJSON(w, http.StatusUnauthorized, map[string]any{
					"error":      "user not found",
					"request_id": requestID,
				})
				return
			}
			slog.Error("failed to reload user after preferences update",
				"request_id", requestID, "user_id", userID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error":      "failed to load user",
				"request_id": requestID,
			})
			return
		}

		writeJSON(w, http.StatusOK, mePreferencesResponse{
			PreferredLanguage: current.PreferredLanguage,
		})
	}
}

// updateUserPreferredLanguage writes preferred_language for the given
// user. The CHECK constraint on the column doubles as a server-side
// firewall against bad values that slipped past the handler.
func updateUserPreferredLanguage(ctx context.Context, db *sql.DB, userID, language string) error {
	if db == nil {
		return errors.New("database unavailable")
	}
	res, err := db.ExecContext(ctx, `
		UPDATE users
		SET preferred_language = $2, updated_at = NOW()
		WHERE id = $1
	`, userID, language)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err == nil && rows == 0 {
		return errUserNotFoundOrInactive
	}
	return nil
}
