// Package userbyok persists user-supplied LLM API keys for the
// /advisor surface (Phase B-1). The store is intentionally
// separate from subscription.ModelConfigService because:
//
//   * subscription.ModelConfigService is fund-mode shaped — its
//     primary key is (user, model_tier, role) and its lifecycle
//     is tied to the fund workflow (create-fund → assign-keys).
//   * advisor BYOK is per-user, per-provider, with a monthly $
//     soft cap; tying it to the fund-mode rows would make every
//     /advisor consult page need to know whether the user has a
//     fund and which model_tier the panel is running on. They
//     don't and shouldn't.
//
// Encryption reuses subscription.EncryptAPIKey /
// subscription.DecryptAPIKey so there is exactly one secret
// (MODEL_CONFIG_API_KEY_SECRET) on the platform, sharing a
// rotation story with the existing fund-mode keys.
package userbyok

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fundai/server/internal/subscription"
)

// SupportedProviders enumerates the providers we accept on
// /api/advisor/byok/keys submissions. Kept in sync with the
// CHECK constraint in migration 101.
var SupportedProviders = []string{
	"openai", "anthropic", "deepseek", "kimi", "doubao", "qwen",
}

// IsSupportedProvider reports whether the provider name maps to
// a row in SupportedProviders.
func IsSupportedProvider(name string) bool {
	for _, p := range SupportedProviders {
		if p == name {
			return true
		}
	}
	return false
}

// Key is the read-shape returned to the handler. Never carries
// the plaintext API key — even RotateActiveKey returns the
// pre-rotation row with the encrypted blob.
type Key struct {
	ID                    string
	UserID                string
	Provider              string
	Label                 string
	APIKeyFingerprint     string
	APIKeyPreview         string
	BaseURL               string
	ModelName             string
	MonthlyBudgetCentsUSD int
	IsActive              bool
	LastUsedAt            sql.NullTime
	LastVerifiedAt        sql.NullTime
	RevokedAt             sql.NullTime
	RevokedReason         string
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// CreateRequest is the input to Repo.Create.
type CreateRequest struct {
	UserID                string
	Provider              string
	Label                 string
	PlaintextAPIKey       string
	BaseURL               string
	ModelName             string
	MonthlyBudgetCentsUSD int
}

// Repo wraps the user_llm_keys table.
//
// Methods follow the same pattern as subscription.Repo: each
// returns its full read-shape so the handler can render without
// extra round-trips.
type Repo struct {
	db *sql.DB
}

// NewRepo wires the repo with the supplied DB handle.
func NewRepo(db *sql.DB) *Repo {
	return &Repo{db: db}
}

// ErrUnsupportedProvider is returned by Create when the provider
// is not in SupportedProviders.
var ErrUnsupportedProvider = errors.New("userbyok: unsupported provider")

// ErrEmptyAPIKey is returned by Create when the plaintext key is
// blank after trim.
var ErrEmptyAPIKey = errors.New("userbyok: empty api key")

// ErrEncryptionUnconfigured surfaces when MODEL_CONFIG_API_KEY_SECRET
// (or API_KEY_ENCRYPTION_SECRET) is unset. The handler should
// 503 in this case so the SPA renders a clear "BYOK requires
// platform crypto config" message instead of a silent crash.
var ErrEncryptionUnconfigured = errors.New("userbyok: encryption not configured")

// ErrNotFound is returned when a key id / (user,provider) lookup
// misses every row.
var ErrNotFound = errors.New("userbyok: not found")

// ErrAlreadyActive is returned by Create when there's already an
// active row for the same (user, provider). The caller should
// revoke the existing one first or call UpsertActive (TODO).
var ErrAlreadyActive = errors.New("userbyok: an active key already exists for this provider")

// Create persists a new key. Encrypts via subscription.EncryptAPIKey
// — that function returns an error when the secret is unconfigured.
//
// We do NOT verify the key against the upstream provider here;
// that's the handler's job (a separate "verify" endpoint can
// hit /v1/models on OpenAI, the equivalent on Anthropic, etc.).
// Keeping verification out of Create lets the user save a key
// that's only valid after their billing portal finishes a slow
// approval.
func (r *Repo) Create(ctx context.Context, req CreateRequest) (*Key, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("userbyok: repo not configured")
	}
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	if !IsSupportedProvider(provider) {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedProvider, req.Provider)
	}
	plaintext := strings.TrimSpace(req.PlaintextAPIKey)
	if plaintext == "" {
		return nil, ErrEmptyAPIKey
	}
	secret, err := getEncryptionSecret()
	if err != nil {
		return nil, ErrEncryptionUnconfigured
	}
	encrypted, err := subscription.EncryptAPIKey(plaintext, secret)
	if err != nil {
		return nil, fmt.Errorf("userbyok: encrypt: %w", err)
	}
	fingerprint := Fingerprint(plaintext)
	preview := Preview(plaintext)

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("userbyok: begin tx: %w", err)
	}
	defer tx.Rollback()

	var existsActive bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM user_llm_keys
			WHERE user_id = $1 AND provider = $2 AND revoked_at IS NULL
		)
	`, req.UserID, provider).Scan(&existsActive); err != nil {
		return nil, fmt.Errorf("userbyok: precheck active: %w", err)
	}
	if existsActive {
		return nil, ErrAlreadyActive
	}

	id := ""
	now := time.Now().UTC()
	row := tx.QueryRowContext(ctx, `
		INSERT INTO user_llm_keys
		    (user_id, provider, label, api_key_encrypted, api_key_fingerprint,
		     base_url, model_name, monthly_budget_cents_usd, is_active,
		     created_at, updated_at)
		VALUES
		    ($1, $2, $3, $4, $5, $6, $7, $8, TRUE, $9, $9)
		RETURNING id
	`,
		req.UserID, provider, strings.TrimSpace(req.Label), encrypted, fingerprint,
		strings.TrimSpace(req.BaseURL), strings.TrimSpace(req.ModelName),
		clampBudget(req.MonthlyBudgetCentsUSD), now,
	)
	if err := row.Scan(&id); err != nil {
		return nil, fmt.Errorf("userbyok: insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("userbyok: commit: %w", err)
	}

	return &Key{
		ID:                    id,
		UserID:                req.UserID,
		Provider:              provider,
		Label:                 strings.TrimSpace(req.Label),
		APIKeyFingerprint:     fingerprint,
		APIKeyPreview:         preview,
		BaseURL:               strings.TrimSpace(req.BaseURL),
		ModelName:             strings.TrimSpace(req.ModelName),
		MonthlyBudgetCentsUSD: clampBudget(req.MonthlyBudgetCentsUSD),
		IsActive:              true,
		CreatedAt:             now,
		UpdatedAt:             now,
	}, nil
}

// List returns every row for the user, active or revoked, newest
// first. The SPA renders revoked rows greyed out so the user can
// see the full audit trail (and so support can answer "I'm sure
// I deleted that key").
func (r *Repo) List(ctx context.Context, userID string) ([]*Key, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("userbyok: repo not configured")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, user_id, provider, label, api_key_encrypted, api_key_fingerprint,
		       base_url, model_name, monthly_budget_cents_usd, is_active,
		       last_used_at, last_verified_at, revoked_at, revoked_reason,
		       created_at, updated_at
		FROM user_llm_keys
		WHERE user_id = $1
		ORDER BY revoked_at IS NULL DESC, created_at DESC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("userbyok: list: %w", err)
	}
	defer rows.Close()

	out := make([]*Key, 0, 4)
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// GetActiveForRouting is the hot-path read invoked by
// llm.UserOverrideHook on every model-call. Returns ErrNotFound
// when the user has no active key for that provider — the hook
// then falls back to the next layer in the routing chain.
//
// Returns the decrypted plaintext API key alongside the masked
// metadata so the LLM client can perform the actual HTTP call.
// We accept the trade-off of decrypting per request rather than
// caching: keys may be revoked at any time and the request rate
// to advisor consultations is low (one consult = one decrypt).
type RoutingKey struct {
	ID                    string
	Provider              string
	BaseURL               string
	ModelName             string
	PlaintextAPIKey       string
	MonthlyBudgetCentsUSD int
}

// GetActiveForRouting fetches and decrypts the user's currently
// active key for the given provider. The hot path on every
// LLM call when BYOK is on. Returns ErrNotFound when the user
// hasn't set up that provider — the hook should then fall
// through to the platform's default routing.
func (r *Repo) GetActiveForRouting(ctx context.Context, userID, provider string) (*RoutingKey, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("userbyok: repo not configured")
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if !IsSupportedProvider(provider) {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedProvider, provider)
	}
	row := r.db.QueryRowContext(ctx, `
		SELECT id, api_key_encrypted, base_url, model_name, monthly_budget_cents_usd
		FROM user_llm_keys
		WHERE user_id = $1 AND provider = $2
		  AND is_active = TRUE AND revoked_at IS NULL
		LIMIT 1
	`, userID, provider)

	var (
		id        string
		encrypted string
		baseURL   string
		modelName string
		budget    int
	)
	if err := row.Scan(&id, &encrypted, &baseURL, &modelName, &budget); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("userbyok: read active: %w", err)
	}
	secret, err := getEncryptionSecret()
	if err != nil {
		return nil, ErrEncryptionUnconfigured
	}
	plaintext, err := subscription.DecryptAPIKey(encrypted, secret)
	if err != nil {
		return nil, fmt.Errorf("userbyok: decrypt: %w", err)
	}
	return &RoutingKey{
		ID:                    id,
		Provider:              provider,
		BaseURL:               baseURL,
		ModelName:             modelName,
		PlaintextAPIKey:       plaintext,
		MonthlyBudgetCentsUSD: budget,
	}, nil
}

// Delete soft-revokes the key. We never DELETE the row — the
// audit trail is the whole point of the table. Reuses the
// (user, provider) unique-when-active index so the user can
// create a fresh row for the same provider right after.
func (r *Repo) Delete(ctx context.Context, userID, keyID, reason string) error {
	if r == nil || r.db == nil {
		return errors.New("userbyok: repo not configured")
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE user_llm_keys
		SET revoked_at = NOW(), revoked_reason = $3, is_active = FALSE, updated_at = NOW()
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL
	`, keyID, userID, strings.TrimSpace(reason))
	if err != nil {
		return fmt.Errorf("userbyok: revoke: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateBudget edits the monthly $ cap on an existing key.
func (r *Repo) UpdateBudget(ctx context.Context, userID, keyID string, cents int) error {
	if r == nil || r.db == nil {
		return errors.New("userbyok: repo not configured")
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE user_llm_keys
		SET monthly_budget_cents_usd = $3, updated_at = NOW()
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL
	`, keyID, userID, clampBudget(cents))
	if err != nil {
		return fmt.Errorf("userbyok: update budget: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetActive toggles is_active without revoking; the user can use
// this to pause a key without losing it.
func (r *Repo) SetActive(ctx context.Context, userID, keyID string, active bool) error {
	if r == nil || r.db == nil {
		return errors.New("userbyok: repo not configured")
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE user_llm_keys
		SET is_active = $3, updated_at = NOW()
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL
	`, keyID, userID, active)
	if err != nil {
		return fmt.Errorf("userbyok: set active: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordUsage stamps last_used_at on the key row. Called by the
// UserOverrideHook AFTER a successful LLM call (best-effort,
// fire-and-forget — losing the timestamp doesn't break routing).
func (r *Repo) RecordUsage(ctx context.Context, keyID string) error {
	if r == nil || r.db == nil {
		return errors.New("userbyok: repo not configured")
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE user_llm_keys
		SET last_used_at = NOW(), updated_at = NOW()
		WHERE id = $1
	`, keyID)
	return err
}

// MarkVerified updates last_verified_at after a successful
// /v1/models (or equivalent) probe.
func (r *Repo) MarkVerified(ctx context.Context, userID, keyID string) error {
	if r == nil || r.db == nil {
		return errors.New("userbyok: repo not configured")
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE user_llm_keys
		SET last_verified_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND user_id = $2
	`, keyID, userID)
	if err != nil {
		return fmt.Errorf("userbyok: mark verified: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- internals -------------------------------------------------------------

type rowScanner interface {
	Scan(dest ...any) error
}

func scanKey(rs rowScanner) (*Key, error) {
	var (
		k         Key
		encrypted string
	)
	if err := rs.Scan(
		&k.ID, &k.UserID, &k.Provider, &k.Label, &encrypted, &k.APIKeyFingerprint,
		&k.BaseURL, &k.ModelName, &k.MonthlyBudgetCentsUSD, &k.IsActive,
		&k.LastUsedAt, &k.LastVerifiedAt, &k.RevokedAt, &k.RevokedReason,
		&k.CreatedAt, &k.UpdatedAt,
	); err != nil {
		return nil, fmt.Errorf("userbyok: scan: %w", err)
	}
	// Decrypt just to render the preview. If decryption fails
	// (secret rotated), we still surface the row but show a
	// "needs re-entry" preview so the user understands the key
	// can't be used until they enter it again.
	if secret, err := getEncryptionSecret(); err == nil {
		if plain, derr := subscription.DecryptAPIKey(encrypted, secret); derr == nil {
			k.APIKeyPreview = Preview(plain)
		} else {
			k.APIKeyPreview = "decrypt-failed"
		}
	} else {
		k.APIKeyPreview = "encryption-disabled"
	}
	return &k, nil
}

// Fingerprint computes SHA-256(plaintext) as hex. Used as a
// dedup key when the user re-submits the same OpenAI key from a
// different device.
func Fingerprint(plaintext string) string {
	if plaintext == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// Preview returns the user-facing "sk-...K8s2" style mask. We
// always show 4 chars at the head and 4 at the tail; for keys
// shorter than 10 chars we redact entirely.
func Preview(plaintext string) string {
	plaintext = strings.TrimSpace(plaintext)
	if len(plaintext) < 10 {
		return strings.Repeat("*", len(plaintext))
	}
	return plaintext[:4] + "..." + plaintext[len(plaintext)-4:]
}

func clampBudget(cents int) int {
	if cents < 0 {
		return 0
	}
	// Refuse anything larger than $10,000 / month — likely a typo.
	if cents > 1_000_000 {
		return 1_000_000
	}
	return cents
}

// getEncryptionSecret defers to the same env var subscription
// uses, so we share one rotation story. Wrapped in a function
// rather than read once at package init so tests can override
// via os.Setenv between subtests.
func getEncryptionSecret() (string, error) {
	for _, env := range []string{"MODEL_CONFIG_API_KEY_SECRET", "API_KEY_ENCRYPTION_SECRET"} {
		if v := strings.TrimSpace(getEnv(env)); v != "" {
			return v, nil
		}
	}
	return "", ErrEncryptionUnconfigured
}

// getEnv is a tiny indirection so tests can swap in a stub
// without polluting the real environment.
var getEnv = func(key string) string {
	return getOSEnv(key)
}
