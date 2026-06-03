// Package repository — platform_llm_providers access layer (S13).
//
// Backs the admin LLM-provider CRUD endpoints and the router
// hot-reload path. API keys are stored as AES-GCM ciphertext under
// MODEL_CONFIG_API_KEY_SECRET (the same secret already used by
// subscription/model_config.go for per-user configs). A short
// fingerprint (first 8 hex chars of SHA-256(plaintext)) is stored
// alongside the ciphertext so the UI can show "sk-…a3f2" without
// ever decrypting.
//
// Concurrency contract: SetDefault flips the platform default in
// a single transaction (unset previous + set new) so the partial
// UNIQUE index uq_platform_llm_providers_single_default cannot
// fire under normal traffic. Callers should NOT toggle
// is_platform_default through Upsert; use SetDefault instead.

package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/fundai/server/internal/subscription"
	"github.com/google/uuid"
)

// ErrPlatformLLMProviderNotFound is returned by Get / Delete when
// the row does not exist. Callers treat it as a 404.
var ErrPlatformLLMProviderNotFound = errors.New("platform_llm_provider: not found")

// PlatformLLMProviderRow mirrors one row in platform_llm_providers.
//
// APIKeyEncrypted holds the ciphertext exactly as stored; callers
// must call DecryptKey() to get the plaintext (which they should
// not log or persist). APIKeyFingerprint is safe to log and show.
type PlatformLLMProviderRow struct {
	ID                    uuid.UUID
	Provider              string
	Label                 string
	ModelTier             sql.NullString // critical/standard/simple/NULL
	ModelName             string
	BaseURL               string
	APIKeyEncrypted       string
	APIKeyFingerprint     string
	MaxTokens             int
	Temperature           float64
	InputPricePer1M       sql.NullFloat64
	OutputPricePer1M      sql.NullFloat64
	CostPer1M             sql.NullFloat64
	Status                string
	IsPlatformDefault     bool
	LastHealthCheckAt     sql.NullTime
	LastHealthCheckResult []byte // raw JSONB
	Source                string
	CreatedAt             time.Time
	UpdatedAt             time.Time
	CreatedBy             uuid.NullUUID
	UpdatedBy             uuid.NullUUID
}

// PlainAPIKey returns the decrypted API key. Reads
// MODEL_CONFIG_API_KEY_SECRET on every call so a runtime secret
// rotation is observed without a router restart. Caller must NOT
// log or persist the return value.
func (r *PlatformLLMProviderRow) PlainAPIKey() (string, error) {
	secret := strings.TrimSpace(os.Getenv("MODEL_CONFIG_API_KEY_SECRET"))
	if secret == "" {
		secret = strings.TrimSpace(os.Getenv("API_KEY_ENCRYPTION_SECRET"))
	}
	if secret == "" {
		return "", errors.New("platform_llm_provider: MODEL_CONFIG_API_KEY_SECRET not set")
	}
	pt, err := subscription.DecryptAPIKey(r.APIKeyEncrypted, secret)
	if err != nil {
		return "", fmt.Errorf("platform_llm_provider: decrypt %s: %w", r.Label, err)
	}
	return pt, nil
}

// PlatformLLMProviderRepo wraps a *sql.DB with the table-specific
// helpers consumed by the admin handler and the router-reload path.
type PlatformLLMProviderRepo struct {
	db *sql.DB
}

// NewPlatformLLMProviderRepo wires the repo to a *sql.DB.
// Returns nil on a nil db so the upstream wiring can degrade
// without panicking (the admin handler then returns 503).
func NewPlatformLLMProviderRepo(db *sql.DB) *PlatformLLMProviderRepo {
	if db == nil {
		return nil
	}
	return &PlatformLLMProviderRepo{db: db}
}

// Count returns the total row count. Used by the wiring layer to
// decide whether to env-seed on first startup.
func (r *PlatformLLMProviderRepo) Count(ctx context.Context) (int, error) {
	if r == nil || r.db == nil {
		return 0, errors.New("platform_llm_provider_repo: nil db")
	}
	var n int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM platform_llm_providers`).Scan(&n); err != nil {
		return 0, fmt.Errorf("platform_llm_provider_repo: count: %w", err)
	}
	return n, nil
}

// Get fetches a row by ID. Returns ErrPlatformLLMProviderNotFound
// when the row is missing.
func (r *PlatformLLMProviderRepo) Get(ctx context.Context, id uuid.UUID) (*PlatformLLMProviderRow, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("platform_llm_provider_repo: nil db")
	}
	row := &PlatformLLMProviderRow{}
	err := r.db.QueryRowContext(ctx, baseSelect+` WHERE id = $1`, id).Scan(scanArgs(row)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPlatformLLMProviderNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("platform_llm_provider_repo: get %s: %w", id, err)
	}
	return row, nil
}

// GetByProviderLabel returns the row matching (provider, label).
// label MUST be non-empty — for the "any label / default" case the
// fund override resolver uses GetPlatformDefault below. Surfaces
// ErrPlatformLLMProviderNotFound on miss so the resolver can fall
// through to the next priority layer (the override silently fails
// rather than crashing the LLM call).
func (r *PlatformLLMProviderRepo) GetByProviderLabel(ctx context.Context, provider, label string) (*PlatformLLMProviderRow, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("platform_llm_provider_repo: nil db")
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	label = strings.TrimSpace(label)
	if provider == "" || label == "" {
		return nil, errors.New("platform_llm_provider_repo: provider+label both required")
	}
	row := &PlatformLLMProviderRow{}
	err := r.db.QueryRowContext(ctx, baseSelect+` WHERE provider=$1 AND label=$2`, provider, label).Scan(scanArgs(row)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPlatformLLMProviderNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("platform_llm_provider_repo: get %s/%s: %w", provider, label, err)
	}
	return row, nil
}

// GetActiveDefaultForProvider returns the row the runtime should
// use when a fund override pins (provider, NULL-label). Preference:
//   1. is_platform_default = TRUE for this provider
//   2. status = 'active' && lowest created_at (deterministic tie-break)
// Returns ErrPlatformLLMProviderNotFound when no active row exists.
func (r *PlatformLLMProviderRepo) GetActiveDefaultForProvider(ctx context.Context, provider string) (*PlatformLLMProviderRow, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("platform_llm_provider_repo: nil db")
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return nil, errors.New("platform_llm_provider_repo: provider required")
	}
	row := &PlatformLLMProviderRow{}
	err := r.db.QueryRowContext(ctx, baseSelect+`
		WHERE provider = $1 AND status = 'active'
		ORDER BY is_platform_default DESC, created_at ASC
		LIMIT 1`, provider).Scan(scanArgs(row)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPlatformLLMProviderNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("platform_llm_provider_repo: default for %s: %w", provider, err)
	}
	return row, nil
}

// ListFilters narrows ListAll. All fields are optional; zero value
// means "no filter on this dimension". The repo always orders by
// (is_platform_default DESC, provider, label) so the UI gets a
// deterministic top row.
type ListFilters struct {
	Provider  string // "" = any
	Status    string // "" = any
	OnlyTier  string // "" = any; "any" means model_tier IS NULL
	IncludeID bool   // dummy: kept for caller readability
}

// ListAll returns all rows matching the filters. Used by the admin
// list endpoint AND by the wiring layer at startup. Never returns
// nil slice (returns empty slice instead) so the caller can range
// without a guard.
func (r *PlatformLLMProviderRepo) ListAll(ctx context.Context, f ListFilters) ([]PlatformLLMProviderRow, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("platform_llm_provider_repo: nil db")
	}
	args := []any{}
	clauses := []string{}
	if v := strings.TrimSpace(f.Provider); v != "" {
		args = append(args, v)
		clauses = append(clauses, fmt.Sprintf("provider = $%d", len(args)))
	}
	if v := strings.TrimSpace(f.Status); v != "" {
		args = append(args, v)
		clauses = append(clauses, fmt.Sprintf("status = $%d", len(args)))
	}
	if v := strings.TrimSpace(f.OnlyTier); v != "" {
		if v == "any" {
			clauses = append(clauses, `model_tier IS NULL`)
		} else {
			args = append(args, v)
			clauses = append(clauses, fmt.Sprintf("model_tier = $%d", len(args)))
		}
	}
	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + strings.Join(clauses, " AND ")
	}
	rows, err := r.db.QueryContext(ctx,
		baseSelect+where+` ORDER BY is_platform_default DESC, provider, label`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("platform_llm_provider_repo: list: %w", err)
	}
	defer rows.Close()
	out := []PlatformLLMProviderRow{}
	for rows.Next() {
		var row PlatformLLMProviderRow
		if err := rows.Scan(scanArgs(&row)...); err != nil {
			return nil, fmt.Errorf("platform_llm_provider_repo: list scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("platform_llm_provider_repo: list rows: %w", err)
	}
	return out, nil
}

// UpsertInput carries the admin-facing payload. APIKeyPlaintext is
// optional — empty string on Update keeps the existing key (the
// repo re-reads the current row and reuses ciphertext+fingerprint).
type UpsertInput struct {
	ID               uuid.UUID // zero -> insert
	Provider         string
	Label            string
	ModelTier        string // "" -> NULL
	ModelName        string
	BaseURL          string
	APIKeyPlaintext  string // "" + ID != zero -> keep existing
	MaxTokens        int
	Temperature      float64
	InputPricePer1M  sql.NullFloat64
	OutputPricePer1M sql.NullFloat64
	CostPer1M        sql.NullFloat64
	Status           string
	Source           string // "admin" / "env_seed" / "api"
	ActorUserID      uuid.NullUUID
}

// Upsert inserts a new row or updates an existing one. Does NOT
// touch is_platform_default — use SetDefault for that. Returns the
// fresh row.
func (r *PlatformLLMProviderRepo) Upsert(ctx context.Context, in UpsertInput) (*PlatformLLMProviderRow, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("platform_llm_provider_repo: nil db")
	}
	if err := validateUpsert(&in); err != nil {
		return nil, err
	}

	encrypted, fingerprint, err := r.resolveAPIKey(ctx, in)
	if err != nil {
		return nil, err
	}

	tier := sql.NullString{}
	if v := strings.TrimSpace(in.ModelTier); v != "" {
		tier.String = v
		tier.Valid = true
	}

	if in.ID == uuid.Nil {
		row := &PlatformLLMProviderRow{}
		err := r.db.QueryRowContext(ctx,
			`INSERT INTO platform_llm_providers
			    (provider, label, model_tier, model_name, base_url,
			     api_key_encrypted, api_key_fingerprint,
			     max_tokens, temperature,
			     input_price_per_1m, output_price_per_1m, cost_per_1m,
			     status, source, created_by, updated_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $15)
			 RETURNING `+returningCols,
			in.Provider, in.Label, tier, in.ModelName, in.BaseURL,
			encrypted, fingerprint,
			in.MaxTokens, in.Temperature,
			in.InputPricePer1M, in.OutputPricePer1M, in.CostPer1M,
			in.Status, in.Source, in.ActorUserID,
		).Scan(scanArgs(row)...)
		if err != nil {
			return nil, fmt.Errorf("platform_llm_provider_repo: insert: %w", err)
		}
		return row, nil
	}

	row := &PlatformLLMProviderRow{}
	err = r.db.QueryRowContext(ctx,
		`UPDATE platform_llm_providers SET
		    provider            = $2,
		    label               = $3,
		    model_tier          = $4,
		    model_name          = $5,
		    base_url            = $6,
		    api_key_encrypted   = $7,
		    api_key_fingerprint = $8,
		    max_tokens          = $9,
		    temperature         = $10,
		    input_price_per_1m  = $11,
		    output_price_per_1m = $12,
		    cost_per_1m         = $13,
		    status              = $14,
		    source              = $15,
		    updated_by          = $16
		  WHERE id = $1
		 RETURNING `+returningCols,
		in.ID, in.Provider, in.Label, tier, in.ModelName, in.BaseURL,
		encrypted, fingerprint,
		in.MaxTokens, in.Temperature,
		in.InputPricePer1M, in.OutputPricePer1M, in.CostPer1M,
		in.Status, in.Source, in.ActorUserID,
	).Scan(scanArgs(row)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPlatformLLMProviderNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("platform_llm_provider_repo: update %s: %w", in.ID, err)
	}
	return row, nil
}

// Delete removes a row by ID. Returns ErrPlatformLLMProviderNotFound
// when no row matched. The router-reload caller is responsible for
// flushing the in-process snapshot afterwards.
func (r *PlatformLLMProviderRepo) Delete(ctx context.Context, id uuid.UUID) error {
	if r == nil || r.db == nil {
		return errors.New("platform_llm_provider_repo: nil db")
	}
	res, err := r.db.ExecContext(ctx, `DELETE FROM platform_llm_providers WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("platform_llm_provider_repo: delete %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrPlatformLLMProviderNotFound
	}
	return nil
}

// SetDefault atomically clears the previous default and sets the
// new one. ID == uuid.Nil clears the default without setting a new
// one. Returns the affected rows so the caller can audit-log.
func (r *PlatformLLMProviderRepo) SetDefault(ctx context.Context, id uuid.UUID, actor uuid.NullUUID) error {
	if r == nil || r.db == nil {
		return errors.New("platform_llm_provider_repo: nil db")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("platform_llm_provider_repo: setdefault tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`UPDATE platform_llm_providers
		    SET is_platform_default = FALSE,
		        updated_by = COALESCE($1, updated_by)
		  WHERE is_platform_default = TRUE`,
		actor,
	); err != nil {
		return fmt.Errorf("platform_llm_provider_repo: setdefault clear: %w", err)
	}
	if id != uuid.Nil {
		res, err := tx.ExecContext(ctx,
			`UPDATE platform_llm_providers
			    SET is_platform_default = TRUE,
			        status              = 'active',
			        updated_by          = COALESCE($2, updated_by)
			  WHERE id = $1`,
			id, actor,
		)
		if err != nil {
			return fmt.Errorf("platform_llm_provider_repo: setdefault apply: %w", err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrPlatformLLMProviderNotFound
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("platform_llm_provider_repo: setdefault commit: %w", err)
	}
	return nil
}

// TouchHealth records the latest test-connection result. result is
// any JSON-serialisable struct (latency, ok, error, echoed model).
// Failures from this method are logged but never block the test
// itself — the caller already has its answer.
func (r *PlatformLLMProviderRepo) TouchHealth(ctx context.Context, id uuid.UUID, result any) error {
	if r == nil || r.db == nil {
		return nil
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("platform_llm_provider_repo: touchhealth marshal: %w", err)
	}
	_, err = r.db.ExecContext(ctx,
		`UPDATE platform_llm_providers
		    SET last_health_check_at     = NOW(),
		        last_health_check_result = $2::jsonb
		  WHERE id = $1`,
		id, string(raw),
	)
	if err != nil {
		return fmt.Errorf("platform_llm_provider_repo: touchhealth %s: %w", id, err)
	}
	return nil
}

// Fingerprint returns the first 8 hex chars of SHA-256(plaintext).
// Exported so the admin handler can compute it for the test path
// without going through a full Upsert.
func Fingerprint(plaintext string) string {
	if plaintext == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:4]) // 4 bytes -> 8 hex chars
}

// EncryptKey encrypts plaintext under MODEL_CONFIG_API_KEY_SECRET.
// Exported so the env-seed path in wiring_adapters can pre-encrypt
// values before bulk-inserting.
func EncryptKey(plaintext string) (string, error) {
	if plaintext == "" {
		return "", errors.New("platform_llm_provider_repo: empty plaintext")
	}
	secret := strings.TrimSpace(os.Getenv("MODEL_CONFIG_API_KEY_SECRET"))
	if secret == "" {
		secret = strings.TrimSpace(os.Getenv("API_KEY_ENCRYPTION_SECRET"))
	}
	if secret == "" {
		return "", errors.New("platform_llm_provider_repo: MODEL_CONFIG_API_KEY_SECRET not set")
	}
	return subscription.EncryptAPIKey(plaintext, secret)
}

// resolveAPIKey returns (encrypted, fingerprint) for the given
// upsert input. If ID is non-zero AND plaintext is empty, the
// existing row's encrypted+fingerprint pair is reused.
func (r *PlatformLLMProviderRepo) resolveAPIKey(ctx context.Context, in UpsertInput) (string, string, error) {
	plaintext := strings.TrimSpace(in.APIKeyPlaintext)
	if plaintext == "" {
		if in.ID == uuid.Nil {
			return "", "", errors.New("platform_llm_provider_repo: api_key_plaintext required on create")
		}
		existing, err := r.Get(ctx, in.ID)
		if err != nil {
			return "", "", err
		}
		return existing.APIKeyEncrypted, existing.APIKeyFingerprint, nil
	}
	enc, err := EncryptKey(plaintext)
	if err != nil {
		return "", "", err
	}
	return enc, Fingerprint(plaintext), nil
}

func validateUpsert(in *UpsertInput) error {
	in.Provider = strings.TrimSpace(strings.ToLower(in.Provider))
	in.Label = strings.TrimSpace(in.Label)
	in.ModelName = strings.TrimSpace(in.ModelName)
	in.BaseURL = strings.TrimSpace(in.BaseURL)
	in.Status = strings.TrimSpace(in.Status)
	in.Source = strings.TrimSpace(in.Source)
	if in.Status == "" {
		in.Status = "active"
	}
	if in.Source == "" {
		in.Source = "admin"
	}
	if in.MaxTokens <= 0 {
		in.MaxTokens = 4096
	}
	if in.Temperature < 0 {
		in.Temperature = 0
	}
	if in.Temperature > 2 {
		in.Temperature = 2
	}
	if !validProvider(in.Provider) {
		return fmt.Errorf("platform_llm_provider_repo: invalid provider %q", in.Provider)
	}
	if in.Label == "" {
		return errors.New("platform_llm_provider_repo: label required")
	}
	if in.ModelName == "" {
		return errors.New("platform_llm_provider_repo: model_name required")
	}
	if in.BaseURL == "" {
		return errors.New("platform_llm_provider_repo: base_url required")
	}
	if in.ModelTier != "" && !validTier(in.ModelTier) {
		return fmt.Errorf("platform_llm_provider_repo: invalid model_tier %q", in.ModelTier)
	}
	if !validStatus(in.Status) {
		return fmt.Errorf("platform_llm_provider_repo: invalid status %q", in.Status)
	}
	if !validSource(in.Source) {
		return fmt.Errorf("platform_llm_provider_repo: invalid source %q", in.Source)
	}
	return nil
}

func validProvider(p string) bool {
	switch p {
	case "openai", "claude", "deepseek", "qwen", "gemini", "custom":
		return true
	}
	return false
}

func validTier(t string) bool {
	switch t {
	case "critical", "standard", "simple":
		return true
	}
	return false
}

func validStatus(s string) bool {
	switch s {
	case "active", "disabled", "draft":
		return true
	}
	return false
}

func validSource(s string) bool {
	switch s {
	case "env_seed", "admin", "api":
		return true
	}
	return false
}

const baseSelect = `SELECT
    id, provider, label, model_tier, model_name, base_url,
    api_key_encrypted, api_key_fingerprint,
    max_tokens, temperature,
    input_price_per_1m, output_price_per_1m, cost_per_1m,
    status, is_platform_default,
    last_health_check_at, last_health_check_result,
    source, created_at, updated_at, created_by, updated_by
  FROM platform_llm_providers`

const returningCols = `
    id, provider, label, model_tier, model_name, base_url,
    api_key_encrypted, api_key_fingerprint,
    max_tokens, temperature,
    input_price_per_1m, output_price_per_1m, cost_per_1m,
    status, is_platform_default,
    last_health_check_at, last_health_check_result,
    source, created_at, updated_at, created_by, updated_by`

func scanArgs(r *PlatformLLMProviderRow) []any {
	return []any{
		&r.ID, &r.Provider, &r.Label, &r.ModelTier, &r.ModelName, &r.BaseURL,
		&r.APIKeyEncrypted, &r.APIKeyFingerprint,
		&r.MaxTokens, &r.Temperature,
		&r.InputPricePer1M, &r.OutputPricePer1M, &r.CostPer1M,
		&r.Status, &r.IsPlatformDefault,
		&r.LastHealthCheckAt, &r.LastHealthCheckResult,
		&r.Source, &r.CreatedAt, &r.UpdatedAt, &r.CreatedBy, &r.UpdatedBy,
	}
}
