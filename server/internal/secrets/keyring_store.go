// keyring_store.go — A4 persistence + rotation primitives for the
// JWT signing keyring.
//
// Three pieces live in this file:
//
//   KeyringStore   — the storage interface (read all, append, demote,
//                    prune). Two implementations follow: a NoopStore
//                    used when DB access is unavailable, and a
//                    PostgresKeyringStore used in production.
//
//   GenerateJWTKey — minted-key factory the rotation handler calls.
//                    Centralised so the kid format and the secret
//                    entropy come from one place (256 bits of
//                    crypto/rand for the secret, kid = "k" plus the
//                    big-endian unix-nanos at mint time for monotonic
//                    sort + uniqueness).
//
//   FingerprintKey — short SHA-256-truncated label safe to log so
//                    operators can confirm a rotation actually fired
//                    without ever seeing the secret material.
//
// The actual rotation orchestration (decide-when, swap-in-memory,
// emit-outbox-event) lives one level up in main.go and the outbox
// handler, not here. This file is intentionally I/O-thin so the
// secrets package stays import-cycle-free.

package secrets

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// StoredKey is the on-disk shape of one keyring row. SecretEncrypted
// is opaque ciphertext under the platform KEK; callers MUST run it
// through DecryptKeyringSecret before using the bytes as an HMAC key.
type StoredKey struct {
	Kid               string
	SecretEncrypted   []byte
	SecretFingerprint string
	IsActive          bool
	CreatedAt         time.Time
	RotatedOutAt      *time.Time
}

// KeyringStore is the persistence contract the rotation handler
// depends on. Keeping it as an interface means tests can plug in a
// pure-in-memory implementation and the secrets package never
// gains a hard dependency on lib/pq.
type KeyringStore interface {
	// ListAll returns every row, active and inactive. Caller is
	// expected to filter by RotatedOutAt + token TTL before
	// installing them in the live keyring.
	ListAll(ctx context.Context) ([]StoredKey, error)
	// AppendActive inserts a new active key AND demotes the
	// previous active row (rotated_out_at = now()) inside a
	// single transaction so the (is_active=TRUE) partial unique
	// index never sees two active keys.
	AppendActive(ctx context.Context, k StoredKey) error
	// PruneRotatedOutBefore deletes verification-only rows older
	// than cutoff. Returns the number of rows removed for the
	// rotation handler to surface in its slog line.
	PruneRotatedOutBefore(ctx context.Context, cutoff time.Time) (int, error)
	// ActiveAge returns how long the current active key has been
	// active. The trigger goroutine compares this against the
	// rotation interval. Returns (0, sql.ErrNoRows) when the
	// table is empty.
	ActiveAge(ctx context.Context) (time.Duration, error)
}

// NoopKeyringStore is the dev / unit-test fallback. Every method
// is a no-op that returns either an empty result or no error.
// Using it means the in-memory env-derived ring is the only source
// of truth — exactly the pre-A4 behaviour.
type NoopKeyringStore struct{}

func (NoopKeyringStore) ListAll(ctx context.Context) ([]StoredKey, error) {
	return nil, nil
}
func (NoopKeyringStore) AppendActive(ctx context.Context, k StoredKey) error { return nil }
func (NoopKeyringStore) PruneRotatedOutBefore(ctx context.Context, cutoff time.Time) (int, error) {
	return 0, nil
}
func (NoopKeyringStore) ActiveAge(ctx context.Context) (time.Duration, error) {
	return 0, sql.ErrNoRows
}

// PostgresKeyringStore is the production implementation. Backed by
// the jwt_keyring table from migration 113. Kept deliberately small
// — no caching, no retries — because the rotation cadence is so low
// (default monthly) that the round-trip cost is irrelevant.
type PostgresKeyringStore struct {
	db *sql.DB
}

func NewPostgresKeyringStore(db *sql.DB) *PostgresKeyringStore {
	if db == nil {
		return nil
	}
	return &PostgresKeyringStore{db: db}
}

func (s *PostgresKeyringStore) ListAll(ctx context.Context) ([]StoredKey, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("secrets.PostgresKeyringStore: nil db")
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT kid, secret_encrypted, secret_fingerprint, is_active, created_at, rotated_out_at
        FROM jwt_keyring
        ORDER BY is_active DESC, created_at DESC
    `)
	if err != nil {
		return nil, fmt.Errorf("jwt_keyring list: %w", err)
	}
	defer rows.Close()
	var out []StoredKey
	for rows.Next() {
		var k StoredKey
		var rotated sql.NullTime
		if err := rows.Scan(&k.Kid, &k.SecretEncrypted, &k.SecretFingerprint, &k.IsActive, &k.CreatedAt, &rotated); err != nil {
			return nil, err
		}
		if rotated.Valid {
			t := rotated.Time
			k.RotatedOutAt = &t
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *PostgresKeyringStore) AppendActive(ctx context.Context, k StoredKey) error {
	if s == nil || s.db == nil {
		return errors.New("secrets.PostgresKeyringStore: nil db")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Demote whoever is active right now. Setting rotated_out_at
	// here is what makes the janitor query work — the previous
	// active key becomes verification-only with a clear timestamp.
	if _, err := tx.ExecContext(ctx, `
        UPDATE jwt_keyring
           SET is_active = FALSE,
               rotated_out_at = COALESCE(rotated_out_at, now())
         WHERE is_active = TRUE
    `); err != nil {
		return fmt.Errorf("jwt_keyring demote prior active: %w", err)
	}
	// Insert the new active row. The UNIQUE(kid) constraint blocks
	// a replayed rotation from inserting a duplicate — we surface
	// the error so the outbox marks the event consumed (because
	// retrying would just hit the same constraint) rather than
	// looping forever.
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO jwt_keyring (kid, secret_encrypted, secret_fingerprint, is_active, created_at)
        VALUES ($1, $2, $3, TRUE, now())
    `, k.Kid, k.SecretEncrypted, k.SecretFingerprint); err != nil {
		return fmt.Errorf("jwt_keyring insert new active: %w", err)
	}
	return tx.Commit()
}

func (s *PostgresKeyringStore) PruneRotatedOutBefore(ctx context.Context, cutoff time.Time) (int, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("secrets.PostgresKeyringStore: nil db")
	}
	res, err := s.db.ExecContext(ctx, `
        DELETE FROM jwt_keyring
         WHERE is_active = FALSE
           AND rotated_out_at IS NOT NULL
           AND rotated_out_at < $1
    `, cutoff)
	if err != nil {
		return 0, fmt.Errorf("jwt_keyring prune: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *PostgresKeyringStore) ActiveAge(ctx context.Context) (time.Duration, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("secrets.PostgresKeyringStore: nil db")
	}
	var createdAt time.Time
	err := s.db.QueryRowContext(ctx, `
        SELECT created_at FROM jwt_keyring WHERE is_active = TRUE LIMIT 1
    `).Scan(&createdAt)
	if err != nil {
		return 0, err
	}
	return time.Since(createdAt), nil
}

// GenerateJWTKey mints a fresh keyring entry: 256 bits of crypto/rand
// for the secret, kid = "k" plus the wall-clock nanos at mint time
// (so kids sort monotonically and never collide on a single host).
// Caller passes the encryptor so this function stays I/O-free and
// testable without the KEK in scope.
func GenerateJWTKey(encryptSecret func(plaintext string) ([]byte, error)) (plaintext string, stored StoredKey, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", StoredKey{}, fmt.Errorf("generate jwt key: %w", err)
	}
	plaintext = hex.EncodeToString(raw)
	cipherText, err := encryptSecret(plaintext)
	if err != nil {
		return "", StoredKey{}, fmt.Errorf("encrypt jwt key: %w", err)
	}
	kid := "k" + strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
	stored = StoredKey{
		Kid:               kid,
		SecretEncrypted:   cipherText,
		SecretFingerprint: FingerprintKey(plaintext),
		IsActive:          true,
		CreatedAt:         time.Now().UTC(),
	}
	return plaintext, stored, nil
}

// FingerprintKey returns the first 8 hex chars of SHA-256(plaintext).
// Safe to log + compare across processes; intentionally not
// reversible. Matches the same convention the LLM provider repo
// uses so admins reading audit logs see a consistent "key tag"
// format across systems.
func FingerprintKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])[:8]
}

// BuildKeyringFromStored is the canonical "stored rows → live
// JWTKeyring" assembly. Takes plaintexts (caller decrypts using
// the project's KEK because the secrets package intentionally
// stays unaware of subscription.DecryptAPIKey to avoid an import
// cycle) plus the per-row metadata and produces a ring NewJWTKeyring
// accepts. Rejects any row with empty plaintext so a decrypt
// failure surfaces as "ring construction error" rather than as
// a signing oracle with an empty secret.
func BuildKeyringFromStored(stored []StoredKey, plaintexts map[string]string) (*JWTKeyring, error) {
	if len(stored) == 0 {
		return nil, errors.New("BuildKeyringFromStored: empty input")
	}
	keys := make([]JWTKey, 0, len(stored))
	activeCount := 0
	for _, k := range stored {
		pt, ok := plaintexts[k.Kid]
		if !ok || pt == "" {
			return nil, fmt.Errorf("BuildKeyringFromStored: missing plaintext for kid %q (decrypt failure?)", k.Kid)
		}
		// Skip rotated-out rows: they were demoted explicitly and
		// we don't want to keep verifying tokens against them past
		// the janitor window. Caller (Manager.Reload) is responsible
		// for filtering by TTL before reaching this point — we
		// double-check just in case.
		if k.RotatedOutAt != nil && k.RotatedOutAt.Before(time.Now().Add(-72*time.Hour)) {
			continue
		}
		jk := JWTKey{Kid: k.Kid, Secret: pt, Active: k.IsActive}
		keys = append(keys, jk)
		if k.IsActive {
			activeCount++
		}
	}
	if activeCount == 0 {
		return nil, errors.New("BuildKeyringFromStored: no active key in stored set")
	}
	if activeCount > 1 {
		return nil, fmt.Errorf("BuildKeyringFromStored: %d active keys (expected exactly 1)", activeCount)
	}
	return NewJWTKeyring(keys)
}
