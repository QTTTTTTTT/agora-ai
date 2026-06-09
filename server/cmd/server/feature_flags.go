// feature_flags.go — admin-controlled feature toggle store.
//
//	GET    /api/feature-flags                 (any authenticated user)
//	GET    /api/admin/feature-flags           (admin)
//	PUT    /api/admin/feature-flags/{key}     (admin)
//
// The flag set drives two effects:
//
//	1. Client soft-hide   — Web/Android/Miniapp consume the public
//	                         GET endpoint at boot and hide nav entries
//	                         / route shells when a flag is OFF. This
//	                         makes "we paused X feature" a one-click
//	                         operation without a code release.
//	2. Server hard-gate   — Flags carrying `enforce_server_gate=TRUE`
//	                         additionally cause hand-picked endpoints
//	                         (see featureFlagGateMiddleware) to return
//	                         503 with a friendly disabled payload.
//	                         Soft-hide alone is good enough for safe
//	                         rollouts, but for "this surface is broken,
//	                         shut it down" we want the API itself to
//	                         decline.
//
// Persistence lives in the `feature_flags` table provisioned by
// migration 097. We deliberately keep the repo layer in this file
// to minimise coupling — there are exactly two callers (this admin
// surface and the gate middleware) and both want raw SQL.

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/lib/pq"
)

// FeatureFlag is the wire-shape returned to admin and (in trimmed
// form) to non-admin clients. Mirrors the table schema, with
// `updated_by` collapsed to the actor's display name for legibility.
type FeatureFlag struct {
	Key               string    `json:"key"`
	Label             string    `json:"label"`
	Description       string    `json:"description"`
	Enabled           bool      `json:"enabled"`
	AffectsRoutes     []string  `json:"affectsRoutes"`
	EnforceServerGate bool      `json:"enforceServerGate"`
	UpdatedAt         time.Time `json:"updatedAt"`
	UpdatedBy         *string   `json:"updatedBy,omitempty"`
}

// publicFeatureFlag is the shape end-users see at /api/feature-flags.
// We intentionally drop the `updatedBy`, `enforceServerGate` and
// `description` fields so a normal user can't infer which features
// the platform is mid-rollout on.
type publicFeatureFlag struct {
	Key     string `json:"key"`
	Enabled bool   `json:"enabled"`
}

// featureFlagCache wraps the DB read with a short TTL so the
// gate middleware doesn't hammer Postgres on every request. The
// cache is invalidated whenever the admin updates a flag — that
// flow goes through this same struct so we know exactly when to
// refresh.
type featureFlagCache struct {
	db    *sql.DB
	ttl   time.Duration
	mu    sync.RWMutex
	flags map[string]FeatureFlag
	at    time.Time
}

func newFeatureFlagCache(db *sql.DB) *featureFlagCache {
	return &featureFlagCache{db: db, ttl: 5 * time.Second}
}

// load fetches the full flag set, caching the result for `ttl`.
// On DB error we return the previous cached snapshot if any —
// "fail open" matches the safer-default principle: a transient
// outage shouldn't break the whole UI.
func (c *featureFlagCache) load(ctx context.Context) (map[string]FeatureFlag, error) {
	if c == nil || c.db == nil {
		return map[string]FeatureFlag{}, nil
	}
	c.mu.RLock()
	if time.Since(c.at) < c.ttl && c.flags != nil {
		out := c.flags
		c.mu.RUnlock()
		return out, nil
	}
	c.mu.RUnlock()

	const q = `
		SELECT flag_key, label, COALESCE(description, ''),
		       enabled,
		       COALESCE(affects_routes, '{}'::TEXT[]),
		       enforce_server_gate,
		       updated_at,
		       updated_by
		FROM feature_flags
		ORDER BY flag_key`
	rows, err := c.db.QueryContext(ctx, q)
	if err != nil {
		c.mu.RLock()
		defer c.mu.RUnlock()
		if c.flags != nil {
			return c.flags, nil
		}
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]FeatureFlag)
	for rows.Next() {
		var (
			f         FeatureFlag
			updatedBy sql.NullString
			routes    pq.StringArray
		)
		if err := rows.Scan(&f.Key, &f.Label, &f.Description, &f.Enabled, &routes, &f.EnforceServerGate, &f.UpdatedAt, &updatedBy); err != nil {
			return nil, err
		}
		f.AffectsRoutes = []string(routes)
		if updatedBy.Valid && updatedBy.String != "" {
			s := updatedBy.String
			f.UpdatedBy = &s
		}
		out[f.Key] = f
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.flags = out
	c.at = time.Now()
	c.mu.Unlock()
	return out, nil
}

// invalidate forces the next load() to re-read from the DB. Called
// after a successful PUT so admins see their toggle take effect on
// the very next request.
func (c *featureFlagCache) invalidate() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.at = time.Time{}
	c.flags = nil
	c.mu.Unlock()
}

// IsEnabled is the gate-middleware entry point. Unknown flags
// default to TRUE (the feature is on) — this matters for new code
// paths that haven't been registered as flags yet, and it means
// `IsEnabled("nonexistent")` is safe.
func (c *featureFlagCache) IsEnabled(ctx context.Context, key string) bool {
	if c == nil {
		return true
	}
	flags, err := c.load(ctx)
	if err != nil {
		return true
	}
	flag, ok := flags[key]
	if !ok {
		return true
	}
	return flag.Enabled
}

// MustEnforceServerGate reports whether IsEnabled=FALSE on the named
// flag should result in a 503 from the gated endpoint. Returns false
// for unknown flags.
func (c *featureFlagCache) MustEnforceServerGate(ctx context.Context, key string) bool {
	if c == nil {
		return false
	}
	flags, err := c.load(ctx)
	if err != nil {
		return false
	}
	flag, ok := flags[key]
	if !ok {
		return false
	}
	return flag.EnforceServerGate
}

// gatedAPIPathPatterns maps a feature_flag key to the set of API
// path patterns whose handlers must short-circuit with 503 when the
// flag is OFF and `enforce_server_gate=TRUE`. Each pattern is a
// compiled regex anchored at the start of the path.
//
// We keep this as a single source of truth rather than scattering
// per-handler gate calls because:
//
//   1. Whether a flag enforces a gate or not is a column on the
//      flag row, not a code-time decision. Operators flip it
//      without redeploy.
//   2. Adding a new gated surface = registering one new entry here,
//      no edits inside internal/api/* (which is shared with the
//      mobile-only routes too).
//
// Maintained alongside the feature_flags table seed list — keep
// the keys in sync.
var gatedAPIPathPatterns = map[string][]*regexp.Regexp{
	"ab_test_compare": {
		// /api/funds/{fundId}/abtests (list)
		regexp.MustCompile(`^/api/funds/[^/]+/abtests(/.*)?$`),
		// /api/abtests, /api/abtests/{id}, /api/abtests/{id}/start, ...
		regexp.MustCompile(`^/api/abtests(/.*)?$`),
	},
	// agent_marketplace gates only marketplace + auction routes.
	// Agents themselves stay reachable so funds keep running.
	"agent_marketplace": {
		regexp.MustCompile(`^/api/marketplace(/.*)?$`),
	},
	// agent_lineage is a fund-detail tab; gating its data endpoint
	// means the page (which the SPA hides anyway when the flag is
	// off) doesn't keep retrying via a forgotten bookmark.
	"agent_lineage": {
		regexp.MustCompile(`^/api/funds/[^/]+/agents/lineage$`),
	},
	// advisor_mode gates the entire /advisor consultation surface.
	// Seeded in migration 098 with enforce_server_gate=TRUE so
	// flipping the flag in the admin console 503s every consult /
	// history / preset endpoint without a code release.
	//
	// Note: when advisor_mode is OFF, the BYOK + credits sub-routes
	// also 503 (the AND semantics of the middleware). That's by
	// design: those features only make sense when advisor mode
	// itself is on. The advisor_byok flag is an *additional* gate
	// on top, so admins can disable BYOK while keeping consults on.
	"advisor_mode": {
		regexp.MustCompile(`^/api/advisor(/.*)?$`),
	},
	// advisor_byok adds a second gate on top of advisor_mode for
	// the BYOK CRUD surface. Seeded in migration 101 with
	// enforce_server_gate=TRUE so a key-rotation emergency can
	// kill BYOK independently of consults.
	"advisor_byok": {
		regexp.MustCompile(`^/api/advisor/byok(/.*)?$`),
	},
}

// featureGateMiddleware is the request-time enforcement of
// `enforce_server_gate`. It walks gatedAPIPathPatterns once per
// request and checks the cache for any flag whose pattern matches
// the path; if the flag is OFF + enforces, we 503 with a clear,
// localisable payload. The cache TTL keeps this cheap (~1 read
// per ~5s under load).
func featureGateMiddleware(cache *featureFlagCache) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if cache != nil {
				path := r.URL.Path
				for key, patterns := range gatedAPIPathPatterns {
					matched := false
					for _, p := range patterns {
						if p.MatchString(path) {
							matched = true
							break
						}
					}
					if !matched {
						continue
					}
					if cache.IsEnabled(r.Context(), key) {
						continue
					}
					if !cache.MustEnforceServerGate(r.Context(), key) {
						continue
					}
					writeJSON(w, http.StatusServiceUnavailable, map[string]any{
						"error":    "feature_disabled",
						"flag":     key,
						"detail":   "该功能已被管理员暂停。",
						"detailEn": "This feature has been paused by the platform admin.",
					})
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// registerFeatureFlagPublicRoutes hangs the user-facing endpoint off
// the main mux. Auth: any logged-in user (we still want to know who
// is asking, both for audit and so anonymous browsers don't get a
// 200 they can cache).
func registerFeatureFlagPublicRoutes(mux *http.ServeMux, cache *featureFlagCache) {
	mux.HandleFunc("GET /api/feature-flags", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := api.AuthenticatedUserID(r); !ok {
			writeJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing token"))
			return
		}
		flags, err := cache.load(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
			return
		}
		out := make([]publicFeatureFlag, 0, len(flags))
		for _, f := range flags {
			out = append(out, publicFeatureFlag{Key: f.Key, Enabled: f.Enabled})
		}
		writeJSON(w, http.StatusOK, map[string]any{"flags": out})
	})
}

// --- admin handlers ----------------------------------------------------------

func (h *adminHandler) registerFeatureFlagAdminRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil || h.featureFlags == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/feature-flags", h.handleListFeatureFlags)
	mux.HandleFunc("PUT /api/admin/feature-flags/{key}", h.handleUpdateFeatureFlag)
}

func (h *adminHandler) handleListFeatureFlags(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	flags, err := h.featureFlags.load(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	out := make([]FeatureFlag, 0, len(flags))
	for _, f := range flags {
		out = append(out, f)
	}
	writeJSON(w, http.StatusOK, map[string]any{"flags": out})
}

func (h *adminHandler) handleUpdateFeatureFlag(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	key := strings.TrimSpace(r.PathValue("key"))
	if key == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_key", "missing flag key"))
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("decode_failed", err.Error()))
		return
	}
	if body.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("missing_field", "`enabled` is required"))
		return
	}
	userID, _ := api.AuthenticatedUserID(r)

	const upd = `
		UPDATE feature_flags
		SET enabled = $2,
		    updated_at = NOW(),
		    updated_by = $3
		WHERE flag_key = $1`
	res, err := h.db.ExecContext(r.Context(), upd, key, *body.Enabled, userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		writeJSON(w, http.StatusNotFound, errorPayload("not_found", "unknown flag"))
		return
	}
	h.featureFlags.invalidate()

	// Return the freshly-updated row so the admin UI can patch its
	// local list without an extra GET.
	flags, err := h.featureFlags.load(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"key": key, "enabled": *body.Enabled})
		return
	}
	if flag, ok := flags[key]; ok {
		writeJSON(w, http.StatusOK, flag)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "enabled": *body.Enabled})
}

// errFeatureFlagNotFound is exported in case future callers need to
// distinguish "no such flag" from a network error. We don't return
// it from the public surface today; left as a typed sentinel so a
// follow-up admin-CLI tool can hook on it.
var errFeatureFlagNotFound = errors.New("feature flag not found")
