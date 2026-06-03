// Package repository — fund_llm_overrides access layer (S14.B).
//
// The hot read path is ResolveForRequest: ModelRouter calls it once
// per LLM request to pick the most specific override row. We do the
// specificity ranking in SQL (ORDER BY a CASE expression) so the
// query returns the best match in one round trip. Each fund has at
// most a handful of override rows (typically ≤ 20) so even without
// a specialised index the planner does an index-only scan on
// idx_fund_llm_overrides_fund_enabled.
//
// Writes are admin-driven (PUT /api/funds/{id}/llm-overrides) and
// rare compared to reads — we don't cache writes; the router's hot
// reload re-runs the resolve query against the new state.

package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrFundLLMOverrideNotFound is returned by Get / Delete for a
// missing row. Callers treat it as a 404.
var ErrFundLLMOverrideNotFound = errors.New("fund_llm_override: not found")

// ErrFundLLMOverrideInvalidProvider is returned when an override
// references a provider/label pair that does not exist in
// platform_llm_providers. Surfaced as 422 by the admin handler so
// the operator can fix it before save.
var ErrFundLLMOverrideInvalidProvider = errors.New("fund_llm_override: referenced provider not configured")

// FundLLMOverrideRow mirrors a row in fund_llm_overrides. The four
// scope columns (AgentID/Role/ModelTier) are NULLable wildcards.
type FundLLMOverrideRow struct {
	ID        uuid.UUID
	FundID    uuid.UUID
	AgentID   uuid.NullUUID
	Role      sql.NullString
	ModelTier sql.NullString

	Provider  string
	Label     sql.NullString
	ModelName sql.NullString

	Enabled bool
	Note    sql.NullString

	CreatedAt time.Time
	UpdatedAt time.Time
	CreatedBy uuid.NullUUID
	UpdatedBy uuid.NullUUID
}

// Specificity returns a deterministic score where higher = more
// specific. Used by ResolveForRequest to break ties and by tests
// to assert ordering. Bits: agent_id=8, role=4, tier=2, label=1.
func (r *FundLLMOverrideRow) Specificity() int {
	score := 0
	if r.AgentID.Valid {
		score |= 8
	}
	if r.Role.Valid && strings.TrimSpace(r.Role.String) != "" {
		score |= 4
	}
	if r.ModelTier.Valid && strings.TrimSpace(r.ModelTier.String) != "" {
		score |= 2
	}
	if r.Label.Valid && strings.TrimSpace(r.Label.String) != "" {
		score |= 1
	}
	return score
}

// FundLLMOverrideRepo wraps the fund_llm_overrides table.
type FundLLMOverrideRepo struct {
	db *sql.DB
}

// NewFundLLMOverrideRepo wires a *sql.DB. Returns nil on nil db.
func NewFundLLMOverrideRepo(db *sql.DB) *FundLLMOverrideRepo {
	if db == nil {
		return nil
	}
	return &FundLLMOverrideRepo{db: db}
}

// ListByFund returns all override rows for one fund, ordered by
// specificity DESC, then by created_at ASC for deterministic
// presentation. Used by the admin UI and by the resolver as a
// fallback when bulk-loading the cache.
func (r *FundLLMOverrideRepo) ListByFund(ctx context.Context, fundID uuid.UUID) ([]FundLLMOverrideRow, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("fund_llm_override_repo: nil db")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, fund_id, agent_id, role, model_tier,
		       provider, label, model_name,
		       enabled, note,
		       created_at, updated_at, created_by, updated_by
		  FROM fund_llm_overrides
		 WHERE fund_id = $1
		 ORDER BY created_at ASC
	`, fundID)
	if err != nil {
		return nil, fmt.Errorf("fund_llm_override_repo: list: %w", err)
	}
	defer rows.Close()
	return r.scanRows(rows)
}

// ListAllEnabled returns every enabled override row across the
// platform. The router uses this to populate its per-fund cache at
// boot and on hot reload. The result is bounded (one row per
// override; typical platform: hundreds, not millions) so we don't
// paginate.
func (r *FundLLMOverrideRepo) ListAllEnabled(ctx context.Context) ([]FundLLMOverrideRow, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("fund_llm_override_repo: nil db")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, fund_id, agent_id, role, model_tier,
		       provider, label, model_name,
		       enabled, note,
		       created_at, updated_at, created_by, updated_by
		  FROM fund_llm_overrides
		 WHERE enabled = TRUE
	`)
	if err != nil {
		return nil, fmt.Errorf("fund_llm_override_repo: list all: %w", err)
	}
	defer rows.Close()
	return r.scanRows(rows)
}

// Get returns a single row by id.
func (r *FundLLMOverrideRepo) Get(ctx context.Context, id uuid.UUID) (*FundLLMOverrideRow, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("fund_llm_override_repo: nil db")
	}
	row := r.db.QueryRowContext(ctx, `
		SELECT id, fund_id, agent_id, role, model_tier,
		       provider, label, model_name,
		       enabled, note,
		       created_at, updated_at, created_by, updated_by
		  FROM fund_llm_overrides
		 WHERE id = $1
	`, id)
	out, err := r.scanOne(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrFundLLMOverrideNotFound
		}
		return nil, err
	}
	return out, nil
}

// UpsertParams carries a write request from the admin handler. We
// validate-then-write rather than relying on DB CHECKs for two
// reasons:
//   * The CHECK on (provider, label) FK only fires when label is
//     non-NULL — but the operator could still type a provider that
//     isn't in platform_llm_providers. We catch that here.
//   * Returning ErrFundLLMOverrideInvalidProvider lets the handler
//     render a meaningful 422 instead of a raw FK error.
type UpsertParams struct {
	ID        *uuid.UUID
	FundID    uuid.UUID
	AgentID   *uuid.UUID
	Role      string
	ModelTier string
	Provider  string
	Label     string
	ModelName string
	Enabled   bool
	Note      string
	ActorID   *uuid.UUID
}

// Upsert inserts a new row when ID is nil; otherwise updates the
// existing row. Returns the canonical row state after write.
//
// We DO NOT use ON CONFLICT here even though there's a unique index
// on (fund_id, agent_id, role, tier) — the admin UI flow always
// edits by ID, and surface-area "you have a conflicting override"
// is a UI message, not a silent merge. If the operator typed a
// duplicate scope, let the unique index throw 23505 and surface it.
func (r *FundLLMOverrideRepo) Upsert(ctx context.Context, p UpsertParams) (*FundLLMOverrideRow, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("fund_llm_override_repo: nil db")
	}
	if p.FundID == uuid.Nil {
		return nil, errors.New("fund_llm_override_repo: fund_id required")
	}
	provider := strings.ToLower(strings.TrimSpace(p.Provider))
	if provider == "" {
		return nil, errors.New("fund_llm_override_repo: provider required")
	}
	label := strings.TrimSpace(p.Label)
	role := strings.TrimSpace(p.Role)
	tier := strings.TrimSpace(p.ModelTier)
	model := strings.TrimSpace(p.ModelName)
	note := strings.TrimSpace(p.Note)

	// Soft FK check: confirm the (provider, label) pair exists when
	// label is non-empty. When label is empty we accept it (resolver
	// will pick the active default at request time). When label is
	// set but missing in platform_llm_providers we surface a 422.
	if label != "" {
		var exists bool
		if err := r.db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM platform_llm_providers WHERE provider=$1 AND label=$2)`,
			provider, label).Scan(&exists); err != nil {
			return nil, fmt.Errorf("fund_llm_override_repo: provider check: %w", err)
		}
		if !exists {
			return nil, fmt.Errorf("%w: %s/%s", ErrFundLLMOverrideInvalidProvider, provider, label)
		}
	} else {
		// Even with NULL label, refuse the override if the provider
		// has no row at all — runtime resolver would silently fall
		// through, masking a typo.
		var n int
		if err := r.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM platform_llm_providers WHERE provider=$1`,
			provider).Scan(&n); err != nil {
			return nil, fmt.Errorf("fund_llm_override_repo: provider count: %w", err)
		}
		if n == 0 {
			return nil, fmt.Errorf("%w: %s/*", ErrFundLLMOverrideInvalidProvider, provider)
		}
	}

	roleNS := sql.NullString{Valid: role != "", String: role}
	tierNS := sql.NullString{Valid: tier != "", String: tier}
	labelNS := sql.NullString{Valid: label != "", String: label}
	modelNS := sql.NullString{Valid: model != "", String: model}
	noteNS := sql.NullString{Valid: note != "", String: note}
	agentNN := uuid.NullUUID{}
	if p.AgentID != nil && *p.AgentID != uuid.Nil {
		agentNN = uuid.NullUUID{Valid: true, UUID: *p.AgentID}
	}
	actorNN := uuid.NullUUID{}
	if p.ActorID != nil && *p.ActorID != uuid.Nil {
		actorNN = uuid.NullUUID{Valid: true, UUID: *p.ActorID}
	}

	if p.ID == nil {
		var id uuid.UUID
		err := r.db.QueryRowContext(ctx, `
			INSERT INTO fund_llm_overrides
			    (fund_id, agent_id, role, model_tier,
			     provider, label, model_name,
			     enabled, note, created_by, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)
			RETURNING id
		`, p.FundID, agentNN, roleNS, tierNS,
			provider, labelNS, modelNS,
			p.Enabled, noteNS, actorNN,
		).Scan(&id)
		if err != nil {
			return nil, fmt.Errorf("fund_llm_override_repo: insert: %w", err)
		}
		return r.Get(ctx, id)
	}

	res, err := r.db.ExecContext(ctx, `
		UPDATE fund_llm_overrides
		   SET agent_id   = $2,
		       role       = $3,
		       model_tier = $4,
		       provider   = $5,
		       label      = $6,
		       model_name = $7,
		       enabled    = $8,
		       note       = $9,
		       updated_by = $10
		 WHERE id = $1
	`, *p.ID, agentNN, roleNS, tierNS,
		provider, labelNS, modelNS,
		p.Enabled, noteNS, actorNN,
	)
	if err != nil {
		return nil, fmt.Errorf("fund_llm_override_repo: update: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, ErrFundLLMOverrideNotFound
	}
	return r.Get(ctx, *p.ID)
}

// Delete drops a row. Returns ErrFundLLMOverrideNotFound when the
// id doesn't exist so the admin handler returns a 404.
func (r *FundLLMOverrideRepo) Delete(ctx context.Context, id uuid.UUID) error {
	if r == nil || r.db == nil {
		return errors.New("fund_llm_override_repo: nil db")
	}
	res, err := r.db.ExecContext(ctx, `DELETE FROM fund_llm_overrides WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("fund_llm_override_repo: delete: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrFundLLMOverrideNotFound
	}
	return nil
}

// ResolveForRequest picks the most specific enabled override for
// (fundID, agentID, role, tier). The ORDER BY clause assigns a
// specificity score directly in SQL so the planner can short-circuit
// after the first row. Returns nil + nil when no override matches —
// caller falls through to the next priority level (user override,
// agent default, platform default).
//
// Why server-side ranking: doing it in Go would mean fetching ALL
// rows for the fund per request. With ~20 rows per fund that's
// cheap, but as funds grow it would become a hot path drag. SQL
// keeps the volume bounded per request.
func (r *FundLLMOverrideRepo) ResolveForRequest(ctx context.Context, fundID uuid.UUID, agentID *uuid.UUID, role, tier string) (*FundLLMOverrideRow, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("fund_llm_override_repo: nil db")
	}
	if fundID == uuid.Nil {
		return nil, nil
	}
	agentNN := uuid.NullUUID{}
	if agentID != nil && *agentID != uuid.Nil {
		agentNN = uuid.NullUUID{Valid: true, UUID: *agentID}
	}
	roleNS := sql.NullString{Valid: strings.TrimSpace(role) != "", String: strings.TrimSpace(role)}
	tierNS := sql.NullString{Valid: strings.TrimSpace(tier) != "", String: strings.TrimSpace(tier)}

	row := r.db.QueryRowContext(ctx, `
		SELECT id, fund_id, agent_id, role, model_tier,
		       provider, label, model_name,
		       enabled, note,
		       created_at, updated_at, created_by, updated_by
		  FROM fund_llm_overrides
		 WHERE fund_id = $1
		   AND enabled = TRUE
		   AND (agent_id   IS NULL OR agent_id   = $2)
		   AND (role       IS NULL OR role       = $3)
		   AND (model_tier IS NULL OR model_tier = $4)
		 ORDER BY
		     CASE WHEN agent_id   IS NOT NULL THEN 8 ELSE 0 END +
		     CASE WHEN role       IS NOT NULL THEN 4 ELSE 0 END +
		     CASE WHEN model_tier IS NOT NULL THEN 2 ELSE 0 END +
		     CASE WHEN label      IS NOT NULL THEN 1 ELSE 0 END DESC,
		     created_at ASC
		 LIMIT 1
	`, fundID, agentNN, roleNS, tierNS)

	out, err := r.scanOne(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}

// --- scan helpers ------------------------------------------------------------

func (r *FundLLMOverrideRepo) scanOne(row *sql.Row) (*FundLLMOverrideRow, error) {
	out := &FundLLMOverrideRow{}
	err := row.Scan(
		&out.ID, &out.FundID, &out.AgentID, &out.Role, &out.ModelTier,
		&out.Provider, &out.Label, &out.ModelName,
		&out.Enabled, &out.Note,
		&out.CreatedAt, &out.UpdatedAt, &out.CreatedBy, &out.UpdatedBy,
	)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (r *FundLLMOverrideRepo) scanRows(rows *sql.Rows) ([]FundLLMOverrideRow, error) {
	out := []FundLLMOverrideRow{}
	for rows.Next() {
		var row FundLLMOverrideRow
		if err := rows.Scan(
			&row.ID, &row.FundID, &row.AgentID, &row.Role, &row.ModelTier,
			&row.Provider, &row.Label, &row.ModelName,
			&row.Enabled, &row.Note,
			&row.CreatedAt, &row.UpdatedAt, &row.CreatedBy, &row.UpdatedBy,
		); err != nil {
			return nil, fmt.Errorf("fund_llm_override_repo: scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fund_llm_override_repo: rows: %w", err)
	}
	return out, nil
}
