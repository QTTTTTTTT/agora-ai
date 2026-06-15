package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/mailer"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// ---------------------------------------------------------------------------
// Constants tuned to balance UX with abuse protection. Numbers come from
// Sprint 2A's plan: 6-digit numeric verification codes (short enough to
// type, 1M-space combined with the 10-minute window keeps brute force
// impractical), long URL tokens for password resets so they can be sent
// in a click-through email.
// ---------------------------------------------------------------------------

const (
	verificationCodeLength      = 6
	verificationCodeTTL         = 10 * time.Minute
	verificationCodeMaxAttempts = 5
	verificationSendCooldown    = 60 * time.Second
	verificationSendQuotaPerDay = 6

	passwordResetTokenBytes = 32
	passwordResetTTL        = 1 * time.Hour
	// Rate-limit windows for forgot-password. Per-IP keeps a single
	// attacker from scanning emails; per-email keeps a noisy form from
	// flooding a real user's inbox.
	forgotPasswordPerIPHour    = 3
	forgotPasswordPerEmailDay  = 5
	forgotPasswordRateInterval = 1 * time.Hour
	forgotPasswordRateDailyTTL = 24 * time.Hour

	loginLockThreshold = 5
	loginLockDuration  = 15 * time.Minute
)

// ---------------------------------------------------------------------------
// Rate limiter — in-memory sliding window per key. Adequate for a single
// app replica; distributed deployments should swap this for a Redis-backed
// limiter, but Sprint 2A's plan explicitly scopes to the current single-
// process server. Each window only stores timestamps inside the active TTL
// so memory stays bounded.
// ---------------------------------------------------------------------------

type rateBucket struct {
	hits []time.Time
}

type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*rateBucket
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{buckets: make(map[string]*rateBucket)}
}

func (l *rateLimiter) Allow(key string, limit int, window time.Duration) bool {
	if limit <= 0 || window <= 0 {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	bucket, ok := l.buckets[key]
	if !ok {
		bucket = &rateBucket{}
		l.buckets[key] = bucket
	}
	cutoff := now.Add(-window)
	filtered := bucket.hits[:0]
	for _, ts := range bucket.hits {
		if ts.After(cutoff) {
			filtered = append(filtered, ts)
		}
	}
	bucket.hits = filtered
	if len(bucket.hits) >= limit {
		return false
	}
	bucket.hits = append(bucket.hits, now)
	return true
}

// Package-level limiters shared across handlers. They live for the
// process lifetime and don't need explicit shutdown.
var (
	authRateLimiter = newRateLimiter()
)

// ---------------------------------------------------------------------------
// Hashing helpers — verification codes and reset tokens are stored only as
// SHA-256 hex digests so a database dump leak doesn't immediately let an
// attacker complete a reset. SHA-256 (not bcrypt) is appropriate here:
// tokens have full entropy, are short-lived, and are throttled.
// ---------------------------------------------------------------------------

func hashSecretToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func hashClientIPLine(remote string) string {
	if remote == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(remote)
	if err == nil && host != "" {
		remote = host
	}
	sum := sha256.Sum256([]byte(remote))
	return hex.EncodeToString(sum[:])
}

func generateVerificationCode() (string, error) {
	var b strings.Builder
	for i := 0; i < verificationCodeLength; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		b.WriteString(n.String())
	}
	return b.String(), nil
}

func generatePasswordResetToken() (string, error) {
	buf := make([]byte, passwordResetTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// ---------------------------------------------------------------------------
// HTTP handler payloads
// ---------------------------------------------------------------------------

type sendVerificationRequest struct{}

type verifyEmailRequest struct {
	Code string `json:"code"`
}

type forgotPasswordRequest struct {
	Email string `json:"email"`
}

type resetPasswordRequest struct {
	Token       string `json:"token"`
	NewPassword string `json:"newPassword"`
}

type changePasswordRequest struct {
	OldPassword string `json:"oldPassword"`
	NewPassword string `json:"newPassword"`
}

// ---------------------------------------------------------------------------
// handleSendVerification — authenticated. Issues a fresh code, rate-limits
// per user so a misbehaving client can't spam an inbox.
// ---------------------------------------------------------------------------

func handleSendVerification(svc *Services, cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := ensureRequestID(w, r)
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed", "request_id": requestID})
			return
		}
		if svc == nil || svc.DB == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "service unavailable", "request_id": requestID})
			return
		}
		userID, ok := api.AuthenticatedUserID(r)
		if !ok || strings.TrimSpace(userID) == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing authenticated user", "request_id": requestID})
			return
		}
		user, err := loadActiveUserByID(r.Context(), svc.DB, userID)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "user unavailable", "request_id": requestID})
			return
		}
		if strings.TrimSpace(user.Email) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no email on file", "detail": "请先绑定邮箱。", "request_id": requestID})
			return
		}
		// Per-user per-hour cap + per-user 60s spacing.
		if !authRateLimiter.Allow("verify:hour:"+user.ID, verificationSendQuotaPerDay, forgotPasswordRateInterval) {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many requests", "detail": "验证码发送过于频繁，请稍后再试。", "request_id": requestID})
			return
		}
		if !authRateLimiter.Allow("verify:burst:"+user.ID, 1, verificationSendCooldown) {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many requests", "detail": "请等待 60 秒后再次请求。", "request_id": requestID})
			return
		}

		code, err := generateVerificationCode()
		if err != nil {
			slog.Error("verification code generation failed", "request_id", requestID, "user_id", user.ID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "code generation failed", "request_id": requestID})
			return
		}
		expiresAt := time.Now().UTC().Add(verificationCodeTTL)
		if err := insertEmailVerification(r.Context(), svc.DB, user.ID, user.Email, code, expiresAt); err != nil {
			slog.Error("insert email verification", "request_id", requestID, "user_id", user.ID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to issue code", "request_id": requestID})
			return
		}

		if svc.Mailer != nil {
			if err := mailer.SendEmailVerification(r.Context(), svc.Mailer, cfg.Mailer, mailer.EmailVerificationPayload{
				To:          user.Email,
				DisplayName: user.DisplayName,
				Code:        code,
				ExpiresAt:   expiresAt,
			}); err != nil {
				slog.Warn("mailer: verification dispatch failed", "request_id", requestID, "user_id", user.ID, "error", err)
			}
		}

		response := map[string]any{
			"status":     "code_sent",
			"expires_at": expiresAt.Format(time.RFC3339),
			"request_id": requestID,
		}
		// In dev mode without SMTP, surface the code in the JSON so a
		// developer can test the flow end-to-end without inspecting
		// logs. Production deployments always satisfy Enabled().
		if !cfg.Mailer.Enabled() {
			response["dev_code"] = code
		}
		writeJSON(w, http.StatusOK, response)
	}
}

// ---------------------------------------------------------------------------
// handleVerifyEmail — authenticated. Consumes the most recent unused code
// for the current user; bumps attempts on a wrong code so brute force is
// bounded by verificationCodeMaxAttempts.
// ---------------------------------------------------------------------------

func handleVerifyEmail(svc *Services, _ *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := ensureRequestID(w, r)
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed", "request_id": requestID})
			return
		}
		if svc == nil || svc.DB == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "service unavailable", "request_id": requestID})
			return
		}
		userID, ok := api.AuthenticatedUserID(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing authenticated user", "request_id": requestID})
			return
		}
		var input verifyEmailRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body", "request_id": requestID})
			return
		}
		code := strings.TrimSpace(input.Code)
		if len(code) != verificationCodeLength {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid code", "detail": "请输入 6 位验证码。", "request_id": requestID})
			return
		}

		verified, err := consumeEmailVerification(r.Context(), svc.DB, userID, code)
		if err != nil {
			if errors.Is(err, errVerificationNotFound) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "code expired", "detail": "验证码已过期或不存在，请重新发送。", "request_id": requestID})
				return
			}
			if errors.Is(err, errVerificationMismatch) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "code mismatch", "detail": "验证码错误，请检查后重试。", "request_id": requestID})
				return
			}
			if errors.Is(err, errVerificationLocked) {
				writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many attempts", "detail": "尝试次数过多，请重新申请验证码。", "request_id": requestID})
				return
			}
			slog.Error("verify email", "request_id", requestID, "user_id", userID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "verification failed", "request_id": requestID})
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"status":            "verified",
			"email_verified":    true,
			"email_verified_at": verified.Format(time.RFC3339),
			"request_id":        requestID,
		})
	}
}

// ---------------------------------------------------------------------------
// handleForgotPassword — anonymous. Always returns 200 so attackers cannot
// enumerate registered emails. Real work happens only when the email
// resolves; the empty path is the silent fallthrough.
// ---------------------------------------------------------------------------

func handleForgotPassword(svc *Services, cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := ensureRequestID(w, r)
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed", "request_id": requestID})
			return
		}
		var input forgotPasswordRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body", "request_id": requestID})
			return
		}
		email := normalizeEmail(input.Email)
		if email == "" {
			writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "request_id": requestID})
			return
		}

		ipHash := hashClientIPLine(r.RemoteAddr)
		ipKey := "forgot:ip:" + ipHash
		emailKey := "forgot:email:" + email
		if !authRateLimiter.Allow(ipKey, forgotPasswordPerIPHour, forgotPasswordRateInterval) {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many requests", "request_id": requestID})
			return
		}
		if !authRateLimiter.Allow(emailKey, forgotPasswordPerEmailDay, forgotPasswordRateDailyTTL) {
			// Silent ok — don't leak that the email exists.
			writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "request_id": requestID})
			return
		}

		response := map[string]any{"status": "ok", "request_id": requestID}

		if svc != nil && svc.DB != nil {
			user, err := loadUserByEmailIncludingInactive(r.Context(), svc.DB, email)
			if err == nil && strings.EqualFold(user.Status, userStatusActive) {
				token, err := generatePasswordResetToken()
				if err != nil {
					slog.Error("reset token generation", "request_id", requestID, "error", err)
				} else {
					expiresAt := time.Now().UTC().Add(passwordResetTTL)
					if err := insertPasswordReset(r.Context(), svc.DB, user.ID, user.Email, token, ipHash, r.UserAgent(), expiresAt); err != nil {
						slog.Error("insert password reset", "request_id", requestID, "user_id", user.ID, "error", err)
					} else {
						link := buildResetLink(cfg, token)
						if svc.Mailer != nil {
							if err := mailer.SendPasswordReset(r.Context(), svc.Mailer, cfg.Mailer, mailer.PasswordResetPayload{
								To:          user.Email,
								DisplayName: user.DisplayName,
								Link:        link,
								ExpiresAt:   expiresAt,
							}); err != nil {
								slog.Warn("mailer: reset dispatch failed", "request_id", requestID, "user_id", user.ID, "error", err)
							}
						}
						if !cfg.Mailer.Enabled() {
							// In dev surface the link so a developer can finish the loop.
							response["dev_reset_link"] = link
						}
					}
				}
			}
		}

		writeJSON(w, http.StatusOK, response)
	}
}

// ---------------------------------------------------------------------------
// handleResetPassword — anonymous. Consumes a token + rotates password +
// resets failed-login counters so the user can immediately sign back in.
// ---------------------------------------------------------------------------

func handleResetPassword(svc *Services, cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := ensureRequestID(w, r)
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed", "request_id": requestID})
			return
		}
		if svc == nil || svc.DB == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "service unavailable", "request_id": requestID})
			return
		}
		var input resetPasswordRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body", "request_id": requestID})
			return
		}
		token := strings.TrimSpace(input.Token)
		if token == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing token", "request_id": requestID})
			return
		}
		password, ok := validatePassword(w, input.NewPassword, requestID)
		if !ok {
			return
		}
		hashed, err := hashPassword(password)
		if err != nil {
			slog.Error("hash password", "request_id", requestID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to update password", "request_id": requestID})
			return
		}
		user, err := consumePasswordResetAndRotate(r.Context(), svc.DB, token, hashed)
		if err != nil {
			if errors.Is(err, errPasswordResetNotFound) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid token", "detail": "重置链接已失效，请重新申请。", "request_id": requestID})
				return
			}
			slog.Error("reset password", "request_id", requestID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to update password", "request_id": requestID})
			return
		}

		if svc.Mailer != nil {
			if err := mailer.SendPasswordChangedNotice(r.Context(), svc.Mailer, cfg.Mailer, mailer.PasswordChangedPayload{
				To:          user.Email,
				DisplayName: user.DisplayName,
				ChangedAt:   time.Now().UTC(),
			}); err != nil {
				slog.Warn("mailer: notice dispatch failed", "request_id", requestID, "user_id", user.ID, "error", err)
			}
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"status":     "password_reset",
			"request_id": requestID,
		})
	}
}

// ---------------------------------------------------------------------------
// handleChangePassword — authenticated. Requires the old password to be
// re-typed so a stolen session token cannot trivially change credentials.
// ---------------------------------------------------------------------------

func handleChangePassword(svc *Services, cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := ensureRequestID(w, r)
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed", "request_id": requestID})
			return
		}
		if svc == nil || svc.DB == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "service unavailable", "request_id": requestID})
			return
		}
		userID, ok := api.AuthenticatedUserID(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing authenticated user", "request_id": requestID})
			return
		}
		var input changePasswordRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body", "request_id": requestID})
			return
		}
		if strings.TrimSpace(input.OldPassword) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing old password", "request_id": requestID})
			return
		}
		newPassword, ok := validatePassword(w, input.NewPassword, requestID)
		if !ok {
			return
		}
		user, err := loadActiveUserByID(r.Context(), svc.DB, userID)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "user unavailable", "request_id": requestID})
			return
		}
		if strings.TrimSpace(user.PasswordHash) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no password set", "detail": "当前账号不支持密码登录。", "request_id": requestID})
			return
		}
		if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(input.OldPassword)); err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid credentials", "detail": "原密码不正确。", "request_id": requestID})
			return
		}
		hashed, err := hashPassword(newPassword)
		if err != nil {
			slog.Error("hash password", "request_id", requestID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to update password", "request_id": requestID})
			return
		}
		if err := updateUserPassword(r.Context(), svc.DB, user.ID, hashed); err != nil {
			slog.Error("update password", "request_id", requestID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to update password", "request_id": requestID})
			return
		}
		if svc.Mailer != nil && strings.TrimSpace(user.Email) != "" {
			if err := mailer.SendPasswordChangedNotice(r.Context(), svc.Mailer, cfg.Mailer, mailer.PasswordChangedPayload{
				To:          user.Email,
				DisplayName: user.DisplayName,
				ChangedAt:   time.Now().UTC(),
			}); err != nil {
				slog.Warn("mailer: notice dispatch failed", "request_id", requestID, "user_id", user.ID, "error", err)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "password_changed", "request_id": requestID})
	}
}

// ---------------------------------------------------------------------------
// SQL helpers
// ---------------------------------------------------------------------------

var (
	errVerificationNotFound  = errors.New("verification: not found or expired")
	errVerificationMismatch  = errors.New("verification: code mismatch")
	errVerificationLocked    = errors.New("verification: max attempts reached")
	errPasswordResetNotFound = errors.New("password reset: not found or expired")
)

func insertEmailVerification(ctx context.Context, db *sql.DB, userID, email, code string, expiresAt time.Time) error {
	if db == nil {
		return errors.New("db unavailable")
	}
	id := uuid.NewString()
	codeHash := hashSecretToken(code)
	_, err := db.ExecContext(ctx, `
		INSERT INTO email_verifications (id, user_id, email, code_hash, expires_at)
		VALUES ($1, $2, $3, $4, $5)
	`, id, userID, email, codeHash, expiresAt)
	return err
}

// consumeEmailVerification picks the freshest open verification row for
// the user, increments attempts on a mismatch, marks it consumed on a
// match, and flips the user's email_verified flag in the same transaction
// so the two stay in sync even on partial failure.
func consumeEmailVerification(ctx context.Context, db *sql.DB, userID, code string) (time.Time, error) {
	if db == nil {
		return time.Time{}, errors.New("db unavailable")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback()

	var (
		id        string
		codeHash  string
		expiresAt time.Time
		attempts  int
	)
	err = tx.QueryRowContext(ctx, `
		SELECT id, code_hash, expires_at, attempts
		FROM email_verifications
		WHERE user_id = $1 AND consumed_at IS NULL
		ORDER BY created_at DESC
		LIMIT 1
	`, userID).Scan(&id, &codeHash, &expiresAt, &attempts)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, errVerificationNotFound
		}
		return time.Time{}, err
	}
	if time.Now().UTC().After(expiresAt) {
		return time.Time{}, errVerificationNotFound
	}
	if attempts >= verificationCodeMaxAttempts {
		return time.Time{}, errVerificationLocked
	}
	if hashSecretToken(code) != codeHash {
		if _, err := tx.ExecContext(ctx, `UPDATE email_verifications SET attempts = attempts + 1 WHERE id = $1`, id); err != nil {
			return time.Time{}, err
		}
		if err := tx.Commit(); err != nil {
			return time.Time{}, err
		}
		return time.Time{}, errVerificationMismatch
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE email_verifications SET consumed_at = $1 WHERE id = $2`, now, id); err != nil {
		return time.Time{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET email_verified = TRUE, email_verified_at = $1 WHERE id = $2`, now, userID); err != nil {
		return time.Time{}, err
	}
	if err := tx.Commit(); err != nil {
		return time.Time{}, err
	}
	return now, nil
}

func insertPasswordReset(ctx context.Context, db *sql.DB, userID, email, token, ipHash, userAgent string, expiresAt time.Time) error {
	if db == nil {
		return errors.New("db unavailable")
	}
	id := uuid.NewString()
	tokenHash := hashSecretToken(token)
	_, err := db.ExecContext(ctx, `
		INSERT INTO password_resets (id, user_id, email, token_hash, expires_at, ip_hash, user_agent)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, id, userID, email, tokenHash, expiresAt, ipHash, userAgent)
	return err
}

func consumePasswordResetAndRotate(ctx context.Context, db *sql.DB, token, newPasswordHash string) (*authenticatedUser, error) {
	if db == nil {
		return nil, errors.New("db unavailable")
	}
	tokenHash := hashSecretToken(token)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var (
		resetID   string
		userID    string
		email     string
		expiresAt time.Time
	)
	err = tx.QueryRowContext(ctx, `
		SELECT id, user_id, email, expires_at
		FROM password_resets
		WHERE token_hash = $1 AND consumed_at IS NULL
		LIMIT 1
	`, tokenHash).Scan(&resetID, &userID, &email, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errPasswordResetNotFound
		}
		return nil, err
	}
	if time.Now().UTC().After(expiresAt) {
		return nil, errPasswordResetNotFound
	}

	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE password_resets SET consumed_at = $1 WHERE id = $2`, now, resetID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE users
		SET password_hash = $1,
		    failed_login_attempts = 0,
		    locked_until = NULL,
		    updated_at = NOW()
		WHERE id = $2
	`, newPasswordHash, userID); err != nil {
		return nil, err
	}

	// Burn any other outstanding tokens for the same user so a second
	// link from the same email cycle can't be reused after a successful
	// reset.
	if _, err := tx.ExecContext(ctx, `
		UPDATE password_resets
		SET consumed_at = $1
		WHERE user_id = $2 AND consumed_at IS NULL AND id <> $3
	`, now, userID, resetID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	user, err := loadActiveUserByID(ctx, db, userID)
	if err != nil {
		// Best effort: even if we can't reload (e.g. status flipped), the
		// password is rotated. Fall back to a minimal record so the
		// notification email can still address them by email.
		return &authenticatedUser{ID: userID, Email: email}, nil
	}
	return user, nil
}

func updateUserPassword(ctx context.Context, db *sql.DB, userID, newHash string) error {
	if db == nil {
		return errors.New("db unavailable")
	}
	_, err := db.ExecContext(ctx, `
		UPDATE users
		SET password_hash = $1,
		    failed_login_attempts = 0,
		    locked_until = NULL,
		    updated_at = NOW()
		WHERE id = $2
	`, newHash, userID)
	return err
}

// loadUserByEmailIncludingInactive is used by the password-reset path —
// we deliberately accept any status so a suspended user still receives a
// notice (suspending doesn't strip their right to know about a reset
// request) but the rotate itself still requires Active.
func loadUserByEmailIncludingInactive(ctx context.Context, db *sql.DB, email string) (*authenticatedUser, error) {
	if db == nil {
		return nil, errors.New("db unavailable")
	}
	var user authenticatedUser
	err := db.QueryRowContext(ctx, `
		SELECT id, COALESCE(email, ''), COALESCE(display_name, ''), COALESCE(role, 'user'), status, COALESCE(password_hash, ''), COALESCE(kyc_status, 'unverified'), COALESCE(kyc_level, 'tier1_basic'), COALESCE(preferred_language, 'zh-CN')
		FROM users
		WHERE LOWER(email) = LOWER($1)
		LIMIT 1
	`, email).Scan(&user.ID, &user.Email, &user.DisplayName, &user.Role, &user.Status, &user.PasswordHash, &user.KYCStatus, &user.KYCLevel, &user.PreferredLanguage)
	if err != nil {
		return nil, err
	}
	return &user, nil
}

func buildResetLink(cfg *Config, token string) string {
	base := strings.TrimRight(cfg.AppPublicURL, "/")
	if base == "" {
		base = "http://localhost:5173"
	}
	u := base + "/reset-password"
	q := url.Values{}
	q.Set("token", token)
	return fmt.Sprintf("%s?%s", u, q.Encode())
}
