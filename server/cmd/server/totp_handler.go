// 2FA / TOTP handler (P0-6).
//
// HTTP surface:
//
//	POST /api/auth/2fa/setup       — start enrolment, return QR + recovery codes
//	POST /api/auth/2fa/verify      — confirm first code, flip enabled_at
//	POST /api/auth/2fa/disable     — drop the row (requires password + valid code)
//	GET  /api/auth/2fa/status      — { enabled, lastVerifiedAt }
//	POST /api/auth/2fa/challenge   — login flow: exchange challenge token + code → session
//
// Login integration (handleLogin in main.go):
//
//   - When the password verifies AND the user has 2FA enabled, we
//     return { requires_2fa: true, challenge: "<short-lived
//     token>", expires_at } INSTEAD of the session token. The
//     challenge token is signed with the same JWT secret as the
//     session and embeds (user_id, "2fa_challenge", exp).
//   - The frontend posts that challenge + the user's 6-digit code
//     to /api/auth/2fa/challenge. On success we burn the challenge
//     and return the same shape handleLogin would have returned.
//
// Encryption key
//
// TOTP secrets are encrypted at rest under TOTP_ENCRYPTION_KEY (a
// 64-char hex string = 32 raw bytes for AES-256). Missing /
// malformed key disables every endpoint here with 503 — we will
// not silently store secrets unencrypted.

package main

import (
	"context"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/repository"
	"github.com/fundai/server/internal/totp"
	"golang.org/x/crypto/bcrypt"
)

const totpEncryptionKeyEnv = "TOTP_ENCRYPTION_KEY"

// totpFeatureWired reports whether this deployment has TOTP
// crypto configured. handleLogin uses it to decide whether to
// gate super_admin behind forced enrollment — without the key
// we can't validate codes and would lock the admin out of the
// platform, so we fall through to a plain session.
func totpFeatureWired() bool {
	return strings.TrimSpace(os.Getenv(totpEncryptionKeyEnv)) != ""
}

// isSuperAdminUser is the centralised role check used by the
// forced-enrollment gate. Lowercased comparison so deployments
// that seed the role from external systems (e.g. an SSO claim)
// don't trip on case mismatch.
func isSuperAdminUser(user *authenticatedUser) bool {
	if user == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(user.Role), "super_admin")
}

// totpRowFullyEnabled is the nil-safe "user has 2FA active"
// predicate. A row that exists but never had MarkEnabled called
// (e.g. the user started enrollment, scanned the QR, and
// abandoned) counts as NOT enabled — we want to push them back
// into enroll-complete on next login, not let them through with
// a half-finished setup.
func totpRowFullyEnabled(row *repository.UserTOTP) bool {
	return row != nil && row.IsEnabled()
}

// twoFAChallengeTTL is the lifetime of a login challenge token. We
// keep it short — long enough for the user to switch to their
// authenticator app and type the code, short enough that a stolen
// token expires before it's useful. 5 minutes balances both.
const twoFAChallengeTTL = 5 * time.Minute

// twoFAEnrollmentGrantTTL is the lifetime of an enrollment grant
// token issued at login when a super_admin has no 2FA enrolled yet.
// 10 minutes is generous: the user has to install an authenticator
// app, scan a QR, and type the first code. After expiry they have
// to log in again to restart the flow.
const twoFAEnrollmentGrantTTL = 10 * time.Minute

// twoFAChallengeAudience is the JWT audience claim for challenge
// tokens. Using a distinct audience keeps a stolen challenge from
// being passed off as a regular session token even if signed under
// the same secret.
const twoFAChallengeAudience = "2fa_challenge"

// twoFAEnrollmentGrantAudience is the JWT audience claim for the
// enrollment grant. Same rationale as the challenge audience: a
// stolen grant cannot be passed as a session because validateJWT
// audiences are not interchangeable, and the in-handler parsers
// below explicitly reject any token without the expected audience.
const twoFAEnrollmentGrantAudience = "2fa_enrollment_grant"

// totpHandler bundles the deps needed by every 2FA endpoint. The
// encryption AEAD is constructed once at boot so we never re-derive
// it per request.
type totpHandler struct {
	repo        *repository.UserTOTPRepo
	cipher      cipher.AEAD
	auditLogger audit.Logger
	cfg         *Config
	db          *sql.DB
	log         *slog.Logger
}

// newTOTPHandler wires the handler. Returns nil when:
//
//   - svc has no DB (dev / unit-test boot)
//   - TOTP_ENCRYPTION_KEY is missing or malformed (the deploy
//     pipeline MUST set it; we'd rather refuse to register the
//     routes than silently store plaintext secrets).
//
// A nil handler skips RegisterRoutes, which leaves the 2FA URL
// space unrouted — clients see 404 and degrade gracefully.
func newTOTPHandler(svc *Services, cfg *Config) *totpHandler {
	if svc == nil || svc.DB == nil || cfg == nil {
		return nil
	}
	hexKey := strings.TrimSpace(os.Getenv(totpEncryptionKeyEnv))
	if hexKey == "" {
		slog.Warn("2FA: TOTP_ENCRYPTION_KEY not set — 2FA endpoints disabled")
		return nil
	}
	c, err := totp.NewCipherFromHex(hexKey)
	if err != nil {
		slog.Warn("2FA: TOTP_ENCRYPTION_KEY invalid — 2FA endpoints disabled", "err", err.Error())
		return nil
	}
	return &totpHandler{
		repo:        repository.NewUserTOTPRepo(svc.DB),
		cipher:      c,
		auditLogger: audit.NewDBLogger(svc.DB),
		cfg:         cfg,
		db:          svc.DB,
		log:         slog.Default(),
	}
}

func (h *totpHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("POST /api/auth/2fa/setup", h.handleSetup)
	mux.HandleFunc("POST /api/auth/2fa/verify", h.handleVerify)
	mux.HandleFunc("POST /api/auth/2fa/disable", h.handleDisable)
	mux.HandleFunc("GET /api/auth/2fa/status", h.handleStatus)
	mux.HandleFunc("POST /api/auth/2fa/challenge", h.handleChallenge)
	// Enrollment-grant endpoints: used by super_admin first-login
	// flow when the role is held but TOTP has never been enrolled.
	// These bypass session auth because the user has no session
	// yet — the grant token issued at login is the only credential.
	mux.HandleFunc("POST /api/auth/2fa/enroll-start", h.handleEnrollStart)
	mux.HandleFunc("POST /api/auth/2fa/enroll-complete", h.handleEnrollComplete)
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

type totpStatusResponse struct {
	Enabled            bool   `json:"enabled"`
	EnrolmentPending   bool   `json:"enrolmentPending"`
	LastVerifiedAt     string `json:"lastVerifiedAt,omitempty"`
	LastUsedRecoveryAt string `json:"lastUsedRecoveryAt,omitempty"`
}

func (h *totpHandler) handleStatus(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	row, err := h.repo.GetByUserID(r.Context(), userID)
	if errors.Is(err, sql.ErrNoRows) {
		writeOrderActionJSON(w, http.StatusOK, totpStatusResponse{})
		return
	}
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	resp := totpStatusResponse{
		Enabled:          row.IsEnabled(),
		EnrolmentPending: !row.IsEnabled(),
	}
	if row.LastVerifiedAt.Valid {
		resp.LastVerifiedAt = row.LastVerifiedAt.Time.UTC().Format(time.RFC3339)
	}
	if row.LastUsedRecoveryAt.Valid {
		resp.LastUsedRecoveryAt = row.LastUsedRecoveryAt.Time.UTC().Format(time.RFC3339)
	}
	writeOrderActionJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Setup (enrolment start)
// ---------------------------------------------------------------------------

type totpSetupRequest struct {
	// Optional override; falls back to user's email otherwise.
	AccountLabel string `json:"accountLabel,omitempty"`
}

type totpSetupResponse struct {
	// Secret is the base32 form for "Can't scan the QR" UX. NEVER
	// echoed back after this single response — re-setup generates
	// a fresh secret.
	Secret string `json:"secret"`
	// ProvisioningURI is consumed by the frontend QR renderer.
	ProvisioningURI string `json:"provisioningUri"`
	// RecoveryCodes are 10 single-use plaintext codes the UI must
	// surface exactly once.
	RecoveryCodes []string `json:"recoveryCodes"`
	// Issuer / AccountLabel / Digits / Period / Algorithm round-
	// trip the persisted params for the verify call. The frontend
	// echoes them back verbatim — no client-side derivation.
	Issuer       string `json:"issuer"`
	AccountLabel string `json:"accountLabel"`
	Digits       int    `json:"digits"`
	Period       int    `json:"period"`
	Algorithm    string `json:"algorithm"`
}

func (h *totpHandler) handleSetup(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	var body totpSetupRequest
	if r.ContentLength > 0 {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
			return
		}
	}

	user, err := loadActiveUserByID(r.Context(), h.db, userID)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	label := strings.TrimSpace(body.AccountLabel)
	if label == "" {
		label = user.Email
	}
	if label == "" {
		label = user.ID
	}

	enr, err := totp.Enrol(h.cipher, totp.EnrolmentParams{
		Issuer:      "FundAI",
		AccountName: label,
	})
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	if err := h.repo.Enrol(r.Context(), repository.EnrolParams{
		UserID:              userID,
		SecretEncrypted:     enr.EncryptedSecret,
		Issuer:              enr.Issuer,
		AccountLabel:        enr.AccountName,
		Digits:              enr.Digits,
		PeriodSeconds:       enr.Period,
		Algorithm:           enr.Algorithm,
		RecoveryCodesHashed: enr.HashedRecoveryCodes,
	}); err != nil {
		if errors.Is(err, repository.ErrTOTPAlreadyEnabled) {
			writeOrderActionJSON(w, http.StatusConflict, errorPayload("already_enabled", "2FA is already enabled — disable first to re-enrol"))
			return
		}
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	h.logAudit(r.Context(), audit.MutationEvent{
		ActorUserID: userID,
		Action:      "2fa.enroll_start",
		TargetType:  "user",
		TargetID:    userID,
		Metadata: map[string]any{
			"client_addr": clientIP(r),
		},
	})

	writeOrderActionJSON(w, http.StatusOK, totpSetupResponse{
		Secret:          enr.PlainSecret,
		ProvisioningURI: enr.ProvisioningURI,
		RecoveryCodes:   enr.RecoveryCodes,
		Issuer:          enr.Issuer,
		AccountLabel:    enr.AccountName,
		Digits:          enr.Digits,
		Period:          enr.Period,
		Algorithm:       enr.Algorithm,
	})
}

// ---------------------------------------------------------------------------
// Verify (close enrolment)
// ---------------------------------------------------------------------------

type totpVerifyRequest struct {
	Code string `json:"code"`
}

type totpVerifyResponse struct {
	Enabled bool `json:"enabled"`
}

func (h *totpHandler) handleVerify(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	var body totpVerifyRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	if strings.TrimSpace(body.Code) == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", "code required"))
		return
	}
	row, err := h.repo.GetByUserID(r.Context(), userID)
	if errors.Is(err, sql.ErrNoRows) {
		writeOrderActionJSON(w, http.StatusNotFound, errorPayload("not_enrolled", "no pending 2FA enrolment — call /setup first"))
		return
	}
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if err := totp.Verify(h.cipher, totp.VerifyParams{
		EncryptedSecret: row.SecretEncrypted,
		Code:            body.Code,
		Digits:          row.Digits,
		Period:          row.PeriodSeconds,
		Algorithm:       row.Algorithm,
	}); err != nil {
		// Bump enrolment_attempts so we can refuse a brute force
		// burst even before login is involved.
		_, _ = h.repo.BumpEnrolmentAttempts(r.Context(), userID)
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("invalid_code", "code does not match"))
		return
	}
	if err := h.repo.MarkEnabled(r.Context(), userID); err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	h.logAudit(r.Context(), audit.MutationEvent{
		ActorUserID: userID,
		Action:      "2fa.enroll_complete",
		TargetType:  "user",
		TargetID:    userID,
		Metadata: map[string]any{
			"client_addr": clientIP(r),
		},
	})
	writeOrderActionJSON(w, http.StatusOK, totpVerifyResponse{Enabled: true})
}

// ---------------------------------------------------------------------------
// Disable
// ---------------------------------------------------------------------------

type totpDisableRequest struct {
	// One of code or recoveryCode must be set; password is always
	// required so a stolen session can't unilaterally disable 2FA.
	Password     string `json:"password"`
	Code         string `json:"code,omitempty"`
	RecoveryCode string `json:"recoveryCode,omitempty"`
}

func (h *totpHandler) handleDisable(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	var body totpDisableRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	if strings.TrimSpace(body.Password) == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", "password required"))
		return
	}
	if strings.TrimSpace(body.Code) == "" && strings.TrimSpace(body.RecoveryCode) == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", "code or recoveryCode required"))
		return
	}

	user, err := loadActiveUserByID(r.Context(), h.db, userID)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if !verifyPasswordHash(user.PasswordHash, body.Password) {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("invalid_password", "password does not match"))
		return
	}

	row, err := h.repo.GetByUserID(r.Context(), userID)
	if errors.Is(err, sql.ErrNoRows) {
		writeOrderActionJSON(w, http.StatusNotFound, errorPayload("not_enrolled", "2FA is not enabled"))
		return
	}
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	if body.Code != "" {
		if err := totp.Verify(h.cipher, totp.VerifyParams{
			EncryptedSecret: row.SecretEncrypted,
			Code:            body.Code,
			Digits:          row.Digits,
			Period:          row.PeriodSeconds,
			Algorithm:       row.Algorithm,
		}); err != nil {
			writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("invalid_code", "code does not match"))
			return
		}
	} else {
		if totp.VerifyRecoveryCode(body.RecoveryCode, row.RecoveryCodesHashed) < 0 {
			writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("invalid_recovery", "recovery code does not match"))
			return
		}
	}

	if err := h.repo.Disable(r.Context(), userID); err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	h.logAudit(r.Context(), audit.MutationEvent{
		ActorUserID: userID,
		Action:      "2fa.disable",
		TargetType:  "user",
		TargetID:    userID,
		Metadata: map[string]any{
			"client_addr": clientIP(r),
		},
	})
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"disabled": true})
}

// ---------------------------------------------------------------------------
// Login challenge
// ---------------------------------------------------------------------------

type totpChallengeRequest struct {
	Challenge    string `json:"challenge"`
	Code         string `json:"code,omitempty"`
	RecoveryCode string `json:"recoveryCode,omitempty"`
}

func (h *totpHandler) handleChallenge(w http.ResponseWriter, r *http.Request) {
	requestID := ensureRequestID(w, r)
	var body totpChallengeRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	if strings.TrimSpace(body.Challenge) == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", "challenge required"))
		return
	}
	if strings.TrimSpace(body.Code) == "" && strings.TrimSpace(body.RecoveryCode) == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", "code or recoveryCode required"))
		return
	}

	userID, err := h.parseChallenge(body.Challenge)
	if err != nil {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("invalid_challenge", err.Error()))
		return
	}

	row, err := h.repo.GetByUserID(r.Context(), userID)
	if errors.Is(err, sql.ErrNoRows) || (row != nil && !row.IsEnabled()) {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("not_enrolled", "2FA is not enabled for this account"))
		return
	}
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	if body.Code != "" {
		if err := totp.Verify(h.cipher, totp.VerifyParams{
			EncryptedSecret: row.SecretEncrypted,
			Code:            body.Code,
			Digits:          row.Digits,
			Period:          row.PeriodSeconds,
			Algorithm:       row.Algorithm,
		}); err != nil {
			writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("invalid_code", "code does not match"))
			return
		}
	} else {
		idx := totp.VerifyRecoveryCode(body.RecoveryCode, row.RecoveryCodesHashed)
		if idx < 0 {
			writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("invalid_recovery", "recovery code does not match"))
			return
		}
		// Burn the code so it can't be replayed.
		_ = h.repo.ConsumeRecoveryCode(r.Context(), userID, row.RecoveryCodesHashed[idx])
	}
	_ = h.repo.MarkVerified(r.Context(), userID)

	user, err := loadActiveUserByID(r.Context(), h.db, userID)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	h.logAudit(r.Context(), audit.MutationEvent{
		ActorUserID: userID,
		Action:      "2fa.login_success",
		TargetType:  "user",
		TargetID:    userID,
		Metadata: map[string]any{
			"client_addr":   clientIP(r),
			"used_recovery": body.RecoveryCode != "",
		},
	})
	writeAuthSuccess(w, h.cfg, requestID, user)
}

// logAudit forwards a 2FA mutation event to the audit logger,
// swallowing errors. Same pattern as orderActionsHandler.logAudit:
// a failed audit write is preferable to refusing the operation.
func (h *totpHandler) logAudit(ctx context.Context, ev audit.MutationEvent) {
	if h == nil || h.auditLogger == nil {
		return
	}
	if err := h.auditLogger.LogMutation(ctx, ev); err != nil {
		h.log.Warn("2fa audit write failed",
			"action", ev.Action, "target_id", ev.TargetID, "err", err.Error())
	}
}

// verifyPasswordHash bcrypt-checks the supplied plaintext against
// the persisted hash. Returns false on empty input or mismatch
// (constant-time inside bcrypt).
func verifyPasswordHash(hash, plain string) bool {
	if strings.TrimSpace(hash) == "" || plain == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// issueTwoFAChallenge mints a short-lived JWT used as the handoff
// between handleLogin (1st factor accepted) and /2fa/challenge (2nd
// factor). The token's audience claim is "2fa_challenge" so a
// stolen challenge cannot be presented as a regular session token.
//
// Why a JWT instead of a server-side nonce
//
//   - We already sign session tokens with the same secret + key
//     ring, so reusing the primitive halves the auth-state surface.
//   - JWT exp gives us automatic expiry without a janitor goroutine.
//   - Stateless: a horizontally-scaled deployment doesn't need a
//     shared cache for in-flight challenges.
//
// The trade-off is that we can't revoke a leaked challenge ahead
// of its TTL — but with a 5-minute window that's an acceptable
// blast radius.
func issueTwoFAChallenge(userID string, cfg *Config) (string, time.Time, error) {
	activeSecret, activeKid := cfg.JWTSecret, ""
	if ring := cfg.effectiveJWTKeyring(); ring != nil {
		k := ring.Active()
		activeSecret, activeKid = k.Secret, k.Kid
	}
	now := time.Now().UTC()
	expiresAt := now.Add(twoFAChallengeTTL)
	tok, err := signJWTWithAudience(userID, twoFAChallengeAudience, activeSecret, activeKid, now, expiresAt)
	return tok, expiresAt, err
}

// parseChallenge verifies a 2FA challenge token, returning the
// embedded user_id when valid. Rejects:
//
//   - signatures that don't match any key in the ring;
//   - audience != "2fa_challenge" (a regular session token slipped
//     in as a challenge);
//   - tokens past their exp claim.
func (h *totpHandler) parseChallenge(token string) (string, error) {
	ring := h.cfg.effectiveJWTKeyring()
	if ring == nil {
		return "", errors.New("jwt keyring not configured")
	}
	// Reuse the platform's keyring-aware verifier for signature +
	// expiry. It populates Subject from sub/user_id/uid; the
	// audience claim is NOT in the returned struct so we re-decode
	// the payload below to extract it.
	claims, err := validateJWTWithKeyring(token, ring)
	if err != nil {
		return "", err
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("invalid token format")
	}
	payloadBytes, err := decodeJWTPart(parts[1])
	if err != nil {
		return "", err
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return "", err
	}
	aud, _ := payload["aud"].(string)
	if aud != twoFAChallengeAudience {
		return "", errors.New("token is not a 2fa challenge")
	}
	if strings.TrimSpace(claims.Subject) == "" {
		return "", errors.New("token missing subject")
	}
	return claims.Subject, nil
}

// ---------------------------------------------------------------------------
// Enrollment grant (super_admin first-login forced-2FA flow)
// ---------------------------------------------------------------------------

// issueTwoFAEnrollmentGrant mints the bearer token a freshly-logged-
// in super_admin uses to call /2fa/enroll-start + /enroll-complete.
// Same wire format as issueTwoFAChallenge — only the audience and
// TTL differ — so frontend code can share the JWT-decoding helper.
func issueTwoFAEnrollmentGrant(userID string, cfg *Config) (string, time.Time, error) {
	activeSecret, activeKid := cfg.JWTSecret, ""
	if ring := cfg.effectiveJWTKeyring(); ring != nil {
		k := ring.Active()
		activeSecret, activeKid = k.Secret, k.Kid
	}
	now := time.Now().UTC()
	expiresAt := now.Add(twoFAEnrollmentGrantTTL)
	tok, err := signJWTWithAudience(userID, twoFAEnrollmentGrantAudience, activeSecret, activeKid, now, expiresAt)
	return tok, expiresAt, err
}

// parseEnrollmentGrant mirrors parseChallenge but requires the
// enrollment-grant audience. Rejecting any other audience prevents
// a stolen regular session or a stolen 2FA challenge from being
// promoted into an enrollment grant.
func (h *totpHandler) parseEnrollmentGrant(token string) (string, error) {
	ring := h.cfg.effectiveJWTKeyring()
	if ring == nil {
		return "", errors.New("jwt keyring not configured")
	}
	claims, err := validateJWTWithKeyring(token, ring)
	if err != nil {
		return "", err
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("invalid token format")
	}
	payloadBytes, err := decodeJWTPart(parts[1])
	if err != nil {
		return "", err
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return "", err
	}
	aud, _ := payload["aud"].(string)
	if aud != twoFAEnrollmentGrantAudience {
		return "", errors.New("token is not a 2fa enrollment grant")
	}
	if strings.TrimSpace(claims.Subject) == "" {
		return "", errors.New("token missing subject")
	}
	return claims.Subject, nil
}

// enrollGrantRequest is the shared body shape for both
// /enroll-start and /enroll-complete.
type enrollGrantRequest struct {
	Grant string `json:"grant"`
	Code  string `json:"code,omitempty"`
}

// handleEnrollStart accepts a freshly-issued enrollment grant and
// kicks off the QR + recovery-codes phase. Mirrors handleSetup but
// the userID comes from the grant claims rather than from the
// session middleware (which has not run because the user has no
// session yet).
//
// We deliberately allow re-calling this endpoint with a fresh grant
// when an earlier enrollment was started but never verified: the
// underlying repo.Enrol upserts the pending row, replacing the old
// secret. If the user already has TOTP fully enabled, we surface
// the conflict so the frontend can redirect them to the regular
// login challenge flow instead.
func (h *totpHandler) handleEnrollStart(w http.ResponseWriter, r *http.Request) {
	var body enrollGrantRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	if strings.TrimSpace(body.Grant) == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", "grant required"))
		return
	}
	userID, err := h.parseEnrollmentGrant(body.Grant)
	if err != nil {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("invalid_grant", err.Error()))
		return
	}

	user, err := loadActiveUserByID(r.Context(), h.db, userID)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	label := user.Email
	if label == "" {
		label = user.ID
	}

	enr, err := totp.Enrol(h.cipher, totp.EnrolmentParams{
		Issuer:      "FundAI",
		AccountName: label,
	})
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	if err := h.repo.Enrol(r.Context(), repository.EnrolParams{
		UserID:              userID,
		SecretEncrypted:     enr.EncryptedSecret,
		Issuer:              enr.Issuer,
		AccountLabel:        enr.AccountName,
		Digits:              enr.Digits,
		PeriodSeconds:       enr.Period,
		Algorithm:           enr.Algorithm,
		RecoveryCodesHashed: enr.HashedRecoveryCodes,
	}); err != nil {
		if errors.Is(err, repository.ErrTOTPAlreadyEnabled) {
			writeOrderActionJSON(w, http.StatusConflict, errorPayload("already_enabled", "2FA is already enabled — log in normally and complete the challenge"))
			return
		}
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	h.logAudit(r.Context(), audit.MutationEvent{
		ActorUserID: userID,
		Action:      "2fa.enroll_grant_start",
		TargetType:  "user",
		TargetID:    userID,
		Metadata: map[string]any{
			"client_addr": clientIP(r),
			"role":        user.Role,
		},
	})

	writeOrderActionJSON(w, http.StatusOK, totpSetupResponse{
		Secret:          enr.PlainSecret,
		ProvisioningURI: enr.ProvisioningURI,
		RecoveryCodes:   enr.RecoveryCodes,
		Issuer:          enr.Issuer,
		AccountLabel:    enr.AccountName,
		Digits:          enr.Digits,
		Period:          enr.Period,
		Algorithm:       enr.Algorithm,
	})
}

// handleEnrollComplete closes the enrollment loop AND mints the
// real session in one step. The user goes from "just logged in
// with password" → "2FA enabled + session issued" without ever
// receiving a 2FA-less session token, which is the whole point of
// the forced-enrollment flow.
//
// On success this writes the same response shape handleLogin would
// have written for a normal user, so the frontend treats the two
// paths identically after this point.
func (h *totpHandler) handleEnrollComplete(w http.ResponseWriter, r *http.Request) {
	requestID := ensureRequestID(w, r)
	var body enrollGrantRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	if strings.TrimSpace(body.Grant) == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", "grant required"))
		return
	}
	if strings.TrimSpace(body.Code) == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", "code required"))
		return
	}
	userID, err := h.parseEnrollmentGrant(body.Grant)
	if err != nil {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("invalid_grant", err.Error()))
		return
	}

	row, err := h.repo.GetByUserID(r.Context(), userID)
	if errors.Is(err, sql.ErrNoRows) {
		writeOrderActionJSON(w, http.StatusNotFound, errorPayload("not_enrolled", "no pending 2FA enrolment — call /enroll-start first"))
		return
	}
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if err := totp.Verify(h.cipher, totp.VerifyParams{
		EncryptedSecret: row.SecretEncrypted,
		Code:            body.Code,
		Digits:          row.Digits,
		Period:          row.PeriodSeconds,
		Algorithm:       row.Algorithm,
	}); err != nil {
		_, _ = h.repo.BumpEnrolmentAttempts(r.Context(), userID)
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("invalid_code", "code does not match"))
		return
	}
	if err := h.repo.MarkEnabled(r.Context(), userID); err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	user, err := loadActiveUserByID(r.Context(), h.db, userID)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	h.logAudit(r.Context(), audit.MutationEvent{
		ActorUserID: userID,
		Action:      "2fa.enroll_grant_complete",
		TargetType:  "user",
		TargetID:    userID,
		Metadata: map[string]any{
			"client_addr": clientIP(r),
			"role":        user.Role,
		},
	})
	writeAuthSuccess(w, h.cfg, requestID, user)
}

// signJWTWithAudience mints an HS256 JWT carrying sub + aud + exp
// + iat + nbf. Mirrors issueSessionTokenWithKid's wire format so a
// future migration to a single token-mint helper is a rename.
func signJWTWithAudience(userID, audience, secret, kid string, issuedAt, expiresAt time.Time) (string, error) {
	headerMap := map[string]any{"alg": "HS256", "typ": "JWT"}
	if strings.TrimSpace(kid) != "" {
		headerMap["kid"] = kid
	}
	headerJSON, err := json.Marshal(headerMap)
	if err != nil {
		return "", err
	}
	payloadJSON, err := json.Marshal(map[string]any{
		"sub": userID,
		"aud": audience,
		"iat": issuedAt.Unix(),
		"nbf": issuedAt.Unix(),
		"exp": expiresAt.Unix(),
	})
	if err != nil {
		return "", err
	}
	header := base64.RawURLEncoding.EncodeToString(headerJSON)
	payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	signed := header + "." + payload
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(signed))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signed + "." + signature, nil
}
