// Step-up authentication handler (P0-7).
//
// Per-action biometric "step-up" challenge for high-risk mutations
// — the security peer of P0-6's session 2FA. Where 2FA gates the
// session, step-up gates a single action: place / cancel / replace
// an order, request a withdrawal, etc. The intent is to keep a
// stolen session token from being weaponised into a string of
// trades while the legitimate user is offline.
//
// Wire model
//
//   1. The mobile client prompts for biometrics on every high-risk
//      tap (or every N seconds within an "active trading" window —
//      that's the client's call). On a successful biometric, it
//      calls POST /api/auth/step-up to mint a short-lived token.
//   2. The token is a JWT signed with the same keyring as the
//      session, but with audience="step_up" so it CANNOT be reused
//      as a session token (and a session token cannot be smuggled
//      in as a step-up — the audience is checked).
//   3. The client attaches the token via the X-Step-Up-Token header
//      when it submits the high-risk action.
//   4. The order handler (cancel / replace / place) calls
//      verifyStepUpToken to decide whether to record a "step_up"
//      flag in the audit metadata. Today this is OBSERVATIONAL —
//      we don't reject orders without a token because:
//        a) web sessions don't yet acquire one;
//        b) live-trading-mode hard-enforcement is P0-9's job.
//
// Why short-lived (default 90s)
//
//   - Long enough that a user can chain a few related actions
//     (open an order entry, hesitate, then submit) without the
//     biometric re-prompting on every keystroke.
//   - Short enough that a stolen token, replayed elsewhere, has a
//     tiny blast radius. RFC 8628 / TOTP-style 30s is too tight
//     for human reaction times; 5+ minutes lets a stolen token
//     ride for an entire trading session. 90s is the local maxima.
//
// What this handler is NOT
//
// It is not a "device assertion" — we don't verify Android
// SafetyNet / iOS DeviceCheck attestations here. The step-up token
// only proves the device's biometric prompt succeeded recently;
// the device's identity is established earlier by session login.
// A future P-tier hardening project can layer on Play Integrity
// API attestations, at which point the X-Step-Up-Token header
// gains a sibling X-Device-Attestation header.

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
)

const (
	// stepUpAudience is the JWT aud claim for step-up tokens. It
	// MUST differ from "2fa_challenge" and from session tokens
	// (which carry no aud) so a verifier can't be tricked into
	// honouring the wrong token type.
	stepUpAudience = "step_up"

	// stepUpTokenTTL is the lifetime of a step-up token. See file
	// docstring for why 90s is the chosen value.
	stepUpTokenTTL = 90 * time.Second

	// stepUpHeader is the canonical request header carrying the
	// token. Lowercased lookup is handled by net/http per RFC 7230
	// so client casing does not matter.
	stepUpHeader = "X-Step-Up-Token"
)

// stepUpHandler is the HTTP-side surface. It only owns the mint
// path; verification is exposed as a free function so order
// handlers can call it without depending on the handler struct.
type stepUpHandler struct {
	cfg *Config
}

// newStepUpHandler returns a handler bound to cfg. We keep the
// handler nilable so the route registration site can skip it if
// the platform was started without a JWT keyring (a degenerate
// dev-only path that already breaks login).
func newStepUpHandler(cfg *Config) *stepUpHandler {
	if cfg == nil {
		return nil
	}
	return &stepUpHandler{cfg: cfg}
}

func (h *stepUpHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("POST /api/auth/step-up", h.handleStepUp)
}

// stepUpResponse is what /step-up returns on success. We deliberately
// echo the TTL in seconds so the client can drive its in-memory
// cache without parsing the JWT (which would force the client to
// duplicate the JWT decode logic).
type stepUpResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
	TTLSec    int    `json:"ttl_seconds"`
}

func (h *stepUpHandler) handleStepUp(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	// Acknowledge a small body for forward compatibility (e.g. in
	// future the client sends biometric_kind="fingerprint" so we
	// can record it in audit). Today we don't require it; an
	// empty / no-body POST mints a token.
	if r.ContentLength > 0 {
		var body struct {
			BiometricKind string `json:"biometricKind,omitempty"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		_ = dec.Decode(&body) // silently tolerate empty / malformed; not load-bearing
	}

	now := time.Now().UTC()
	expiresAt := now.Add(stepUpTokenTTL)

	activeSecret, activeKid := h.cfg.JWTSecret, ""
	if ring := h.cfg.effectiveJWTKeyring(); ring != nil {
		k := ring.Active()
		activeSecret, activeKid = k.Secret, k.Kid
	}
	tok, err := signJWTWithAudience(userID, stepUpAudience, activeSecret, activeKid, now, expiresAt)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	writeOrderActionJSON(w, http.StatusOK, stepUpResponse{
		Token:     tok,
		ExpiresAt: expiresAt.Format(time.RFC3339),
		TTLSec:    int(stepUpTokenTTL.Seconds()),
	})
}

// stepUpVerification captures what an order handler needs to know
// about a request's step-up state: whether a valid token was
// presented, whose biometric prompt produced it, and any issue
// that prevented validation.
//
// Today the order handlers consult Valid only to write the audit
// metadata; the live-trading hard gate (P0-9) ALSO consults Valid
// to refuse a state transition on a fund whose trading_mode='live'
// when the step-up token is missing or invalid.
type stepUpVerification struct {
	Valid       bool
	UserID      string
	IssuedAt    time.Time
	ExpiresAt   time.Time
	Audience    string
	Reason      string // populated when Valid==false; safe to log
}

// verifyStepUpToken pulls X-Step-Up-Token off the request and
// returns a verification report. A missing header is reported as
// {Valid:false, Reason:"missing"} — NOT an error — so callers
// can treat "no token" as "no proof" without having to special-case
// http.ErrNoCookie-style errors.
//
// We additionally enforce that the token's subject matches the
// authenticated user's ID. This guards against a user A presenting
// a token minted under user B's biometric (which they shouldn't
// have, but defense in depth).
func verifyStepUpToken(r *http.Request, cfg *Config) stepUpVerification {
	tok := strings.TrimSpace(r.Header.Get(stepUpHeader))
	if tok == "" {
		return stepUpVerification{Reason: "missing"}
	}
	if cfg == nil {
		return stepUpVerification{Reason: "config unavailable"}
	}
	ring := cfg.effectiveJWTKeyring()
	if ring == nil {
		return stepUpVerification{Reason: "jwt keyring not configured"}
	}
	claims, err := validateJWTWithKeyring(tok, ring)
	if err != nil {
		return stepUpVerification{Reason: err.Error()}
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return stepUpVerification{Reason: "invalid token format"}
	}
	payloadBytes, err := decodeJWTPart(parts[1])
	if err != nil {
		return stepUpVerification{Reason: err.Error()}
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return stepUpVerification{Reason: err.Error()}
	}
	aud, _ := payload["aud"].(string)
	if aud != stepUpAudience {
		return stepUpVerification{Reason: "audience mismatch"}
	}
	if strings.TrimSpace(claims.Subject) == "" {
		return stepUpVerification{Reason: "missing subject"}
	}

	// Enforce sub == authenticated user when one is known.
	if reqUser, ok := api.AuthenticatedUserID(r); ok && reqUser != "" && reqUser != claims.Subject {
		return stepUpVerification{Reason: "subject mismatch"}
	}

	v := stepUpVerification{
		Valid:    true,
		UserID:   claims.Subject,
		Audience: aud,
	}
	if iat, ok := payload["iat"].(float64); ok {
		v.IssuedAt = time.Unix(int64(iat), 0).UTC()
	}
	if exp, ok := payload["exp"].(float64); ok {
		v.ExpiresAt = time.Unix(int64(exp), 0).UTC()
	}
	return v
}

// stepUpAuditMetadata produces a small map suitable for splatting
// into an audit row's Metadata field. Lives here so the call site
// (order handlers, etc.) doesn't need to reach into the
// verification struct's internals.
func stepUpAuditMetadata(v stepUpVerification) map[string]any {
	out := map[string]any{
		"step_up": v.Valid,
	}
	if !v.Valid && v.Reason != "" {
		out["step_up_reason"] = v.Reason
	}
	if !v.IssuedAt.IsZero() {
		out["step_up_issued_at"] = v.IssuedAt.Format(time.RFC3339)
	}
	return out
}

// mergeAuditMetadata folds a list of metadata maps into one,
// later maps overriding earlier on key collision. Existing audit
// call sites build their primary metadata as a literal then layer
// the step-up flags on top — colliding keys are rare (we control
// the namespace) but the deterministic right-bias keeps the merge
// behaviour predictable when they do.
func mergeAuditMetadata(maps ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// errStepUpRequired is what a future hard-gate (P0-9) would
// return; exported as a sentinel today so call sites can already
// pattern against it without churning when enforcement lands.
var errStepUpRequired = errors.New("step-up authentication required for this action")
