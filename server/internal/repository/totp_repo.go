// User TOTP secret repository (P0-6).
//
// Owns CRUD for the user_totp_secrets table — the single source of
// truth for "is 2FA enabled for user X" at login time.
//
// Why a separate file
//
// The fund_repo.go is already long (~3000 lines) and concerns
// fund/portfolio/trade/plan rows. 2FA state is an authentication
// concern with its own lifecycle (enrol → verify → enabled →
// disabled / re-enrol). Keeping it in its own file makes the auth
// surface easy to audit without paging through fund logic.
//
// Concurrency
//
// Each method opens its own transaction where mutation is required.
// Repeated reads inside a single auth handler can call GetByUserID
// directly — Postgres' MVCC gives us a consistent snapshot per call.
//
// Error semantics
//
// We return the canonical sql.ErrNoRows from GetByUserID so the
// auth layer can do `errors.Is(err, sql.ErrNoRows)` without
// importing this package's error vocabulary. Mutation methods
// return wrapped errors and let the handler bucket them as 5xx.

package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

// ErrTOTPNotEnabled is returned by methods that expect the row to
// have enabled_at set (e.g. ConsumeRecoveryCode for a login flow).
// The auth handler bucket-treats this as "no 2FA" and proceeds.
var ErrTOTPNotEnabled = errors.New("totp_repo: 2FA not enabled for user")

// ErrTOTPAlreadyEnabled is returned when Enrol is called on a row
// that is already past the verify step. The handler maps this to
// 409 so the UI can prompt the user to disable 2FA before re-enrol.
var ErrTOTPAlreadyEnabled = errors.New("totp_repo: 2FA already enabled, disable first to re-enrol")

// UserTOTP mirrors a row in user_totp_secrets. Recovery codes are
// stored as bcrypt hashes — the plaintext is shown to the user at
// enrol time and never persisted.
type UserTOTP struct {
	UserID               string
	SecretEncrypted      []byte
	Issuer               string
	AccountLabel         string
	Digits               int
	PeriodSeconds        int
	Algorithm            string
	RecoveryCodesHashed  []string
	EnrolmentAttempts    int
	EnabledAt            sql.NullTime
	LastVerifiedAt       sql.NullTime
	LastUsedRecoveryAt   sql.NullTime
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// IsEnabled reports whether the row represents an active 2FA
// binding (the user has verified at least once after enrolment).
func (u *UserTOTP) IsEnabled() bool {
	return u != nil && u.EnabledAt.Valid
}

// UserTOTPRepo is the persistence facade for user_totp_secrets.
type UserTOTPRepo struct {
	db *sql.DB
}

// NewUserTOTPRepo wires a repo bound to a sql.DB handle.
func NewUserTOTPRepo(db *sql.DB) *UserTOTPRepo {
	return &UserTOTPRepo{db: db}
}

// GetByUserID returns the row for userID. Returns sql.ErrNoRows
// when the user has never enrolled (the common case — most users
// don't have 2FA).
func (r *UserTOTPRepo) GetByUserID(ctx context.Context, userID string) (*UserTOTP, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, sql.ErrNoRows
	}
	const q = `
	SELECT user_id, secret_encrypted, issuer, account_label, digits,
	       period_seconds, algorithm, recovery_codes_hashed, enrolment_attempts,
	       enabled_at, last_verified_at, last_used_recovery_at,
	       created_at, updated_at
	  FROM user_totp_secrets
	 WHERE user_id = $1
	 LIMIT 1`
	row := r.db.QueryRowContext(ctx, q, userID)
	var t UserTOTP
	if err := row.Scan(
		&t.UserID, &t.SecretEncrypted, &t.Issuer, &t.AccountLabel, &t.Digits,
		&t.PeriodSeconds, &t.Algorithm, pq.Array(&t.RecoveryCodesHashed), &t.EnrolmentAttempts,
		&t.EnabledAt, &t.LastVerifiedAt, &t.LastUsedRecoveryAt,
		&t.CreatedAt, &t.UpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("totp_repo: get by user: %w", err)
	}
	return &t, nil
}

// EnrolParams is the input for Enrol. All fields are required;
// the handler is responsible for filling them in from the totp
// package's Enrolment shape.
type EnrolParams struct {
	UserID              string
	SecretEncrypted     []byte
	Issuer              string
	AccountLabel        string
	Digits              int
	PeriodSeconds       int
	Algorithm           string
	RecoveryCodesHashed []string
}

// Enrol upserts a fresh enrolment row for userID. If a row already
// exists AND is enabled, the call is rejected with
// ErrTOTPAlreadyEnabled — the user must disable 2FA before
// re-enrolling. Otherwise (no row, or row exists but never reached
// enabled_at) we overwrite the secret and reset enrolment_attempts.
//
// Why an UPSERT instead of INSERT-only
//
// A user who started enrolment, scanned the QR, but never verified
// the first code can simply restart the flow. Forcing them to
// manually delete the half-created row would be hostile UX. The
// trade-off is that an attacker who briefly grabs an authenticated
// session can clobber an existing pending enrolment — but they
// would already have full session access, which is a strictly
// larger compromise.
func (r *UserTOTPRepo) Enrol(ctx context.Context, p EnrolParams) error {
	if strings.TrimSpace(p.UserID) == "" {
		return errors.New("totp_repo: user_id required")
	}
	if len(p.SecretEncrypted) == 0 {
		return errors.New("totp_repo: secret required")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("totp_repo: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	// Reject re-enrol-while-enabled to keep the audit trail clean.
	var enabledAt sql.NullTime
	if err := tx.QueryRowContext(ctx,
		`SELECT enabled_at FROM user_totp_secrets WHERE user_id = $1 FOR UPDATE`,
		p.UserID,
	).Scan(&enabledAt); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("totp_repo: lock existing: %w", err)
	}
	if enabledAt.Valid {
		return ErrTOTPAlreadyEnabled
	}

	const upsert = `
	INSERT INTO user_totp_secrets (
		user_id, secret_encrypted, issuer, account_label, digits,
		period_seconds, algorithm, recovery_codes_hashed,
		enrolment_attempts, enabled_at, last_verified_at,
		last_used_recovery_at
	) VALUES (
		$1, $2, $3, $4, $5,
		$6, $7, $8,
		0, NULL, NULL,
		NULL
	)
	ON CONFLICT (user_id) DO UPDATE SET
		secret_encrypted = EXCLUDED.secret_encrypted,
		issuer = EXCLUDED.issuer,
		account_label = EXCLUDED.account_label,
		digits = EXCLUDED.digits,
		period_seconds = EXCLUDED.period_seconds,
		algorithm = EXCLUDED.algorithm,
		recovery_codes_hashed = EXCLUDED.recovery_codes_hashed,
		enrolment_attempts = 0,
		enabled_at = NULL,
		last_verified_at = NULL,
		last_used_recovery_at = NULL`
	if _, err := tx.ExecContext(ctx, upsert,
		p.UserID, p.SecretEncrypted, p.Issuer, p.AccountLabel, p.Digits,
		p.PeriodSeconds, p.Algorithm, pq.Array(p.RecoveryCodesHashed),
	); err != nil {
		return fmt.Errorf("totp_repo: upsert: %w", err)
	}
	return tx.Commit()
}

// MarkEnabled flips enabled_at + last_verified_at to NOW(). Called
// the moment the user verifies their first code, closing the
// enrolment loop. Idempotent: calling on an already-enabled row is
// a no-op (just bumps last_verified_at).
func (r *UserTOTPRepo) MarkEnabled(ctx context.Context, userID string) error {
	if strings.TrimSpace(userID) == "" {
		return errors.New("totp_repo: user_id required")
	}
	const q = `
	UPDATE user_totp_secrets
	   SET enabled_at = COALESCE(enabled_at, NOW()),
	       last_verified_at = NOW(),
	       enrolment_attempts = 0
	 WHERE user_id = $1`
	res, err := r.db.ExecContext(ctx, q, userID)
	if err != nil {
		return fmt.Errorf("totp_repo: mark enabled: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("totp_repo: mark enabled rows: %w", err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// BumpEnrolmentAttempts is called by the verify handler when the
// user supplies a wrong code DURING enrolment. After enabled, the
// platform uses last_verified_at + a separate rate-limiter to
// throttle login attempts; this counter is for the pre-enrol grace
// window only.
func (r *UserTOTPRepo) BumpEnrolmentAttempts(ctx context.Context, userID string) (int, error) {
	if strings.TrimSpace(userID) == "" {
		return 0, errors.New("totp_repo: user_id required")
	}
	var n int
	if err := r.db.QueryRowContext(ctx,
		`UPDATE user_totp_secrets
		    SET enrolment_attempts = enrolment_attempts + 1
		  WHERE user_id = $1
	  RETURNING enrolment_attempts`,
		userID,
	).Scan(&n); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, sql.ErrNoRows
		}
		return 0, fmt.Errorf("totp_repo: bump attempts: %w", err)
	}
	return n, nil
}

// MarkVerified bumps last_verified_at to NOW() — used after a
// successful login challenge so admins can spot stale 2FA bindings.
func (r *UserTOTPRepo) MarkVerified(ctx context.Context, userID string) error {
	if strings.TrimSpace(userID) == "" {
		return errors.New("totp_repo: user_id required")
	}
	if _, err := r.db.ExecContext(ctx,
		`UPDATE user_totp_secrets SET last_verified_at = NOW() WHERE user_id = $1`,
		userID,
	); err != nil {
		return fmt.Errorf("totp_repo: mark verified: %w", err)
	}
	return nil
}

// ConsumeRecoveryCode removes the recovery code at idx from the
// stored array and bumps last_used_recovery_at. Caller is
// responsible for verifying the bcrypt match BEFORE calling — this
// method only mutates state.
//
// We use array_remove rather than slice arithmetic to keep the
// mutation server-side and atomic. RowsAffected = 0 means the row
// disappeared between the verify and the consume — treat as a
// no-op in the handler (the user logs in anyway since the code
// did match).
func (r *UserTOTPRepo) ConsumeRecoveryCode(ctx context.Context, userID, hashedCode string) error {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(hashedCode) == "" {
		return errors.New("totp_repo: user_id and code hash required")
	}
	const q = `
	UPDATE user_totp_secrets
	   SET recovery_codes_hashed = array_remove(recovery_codes_hashed, $2),
	       last_used_recovery_at = NOW()
	 WHERE user_id = $1`
	if _, err := r.db.ExecContext(ctx, q, userID, hashedCode); err != nil {
		return fmt.Errorf("totp_repo: consume recovery: %w", err)
	}
	return nil
}

// Disable removes the row entirely. We deliberately DROP the row
// rather than NULL-out enabled_at because:
//
//   - The next enrolment generates a fresh secret + recovery codes
//     anyway — keeping the old encrypted secret on disk only
//     widens the attack surface.
//   - Audit history lives in data_access_log, not here. The repo
//     is for live state.
//
// Returns sql.ErrNoRows when the user wasn't enrolled, so the
// handler can surface "you don't have 2FA" cleanly.
func (r *UserTOTPRepo) Disable(ctx context.Context, userID string) error {
	if strings.TrimSpace(userID) == "" {
		return errors.New("totp_repo: user_id required")
	}
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM user_totp_secrets WHERE user_id = $1`,
		userID,
	)
	if err != nil {
		return fmt.Errorf("totp_repo: disable: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("totp_repo: disable rows: %w", err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
