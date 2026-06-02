package stress

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Repo persists stress scenarios and per-fund stress results.
//
// Scenarios are mutable (admin upsert + delete) while results are
// append-only — the latter mirror the design of
// portfolio_var_snapshots / portfolio_factor_snapshots so the
// trend chart and the audit packet can re-read history without
// worrying about post-hoc edits.
type Repo struct {
	db *sql.DB
}

func NewRepo(db *sql.DB) *Repo {
	return &Repo{db: db}
}

// GetScenario fetches one scenario by ID.
func (r *Repo) GetScenario(ctx context.Context, id string) (Scenario, error) {
	if strings.TrimSpace(id) == "" {
		return Scenario{}, errors.New("stress: scenario id required")
	}
	var s Scenario
	var createdBy sql.NullString
	var shocksJSON []byte
	var cat string
	err := r.db.QueryRowContext(ctx,
		`SELECT id, name, category, description, shocks, created_by, created_at, updated_at
		   FROM stress_scenarios
		  WHERE id = $1`, id,
	).Scan(&s.ID, &s.Name, &cat, &s.Description, &shocksJSON, &createdBy, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Scenario{}, ErrNotFound
	}
	if err != nil {
		return Scenario{}, fmt.Errorf("stress: get scenario: %w", err)
	}
	s.Category = Category(cat)
	if createdBy.Valid {
		s.CreatedBy = createdBy.String
	}
	if len(shocksJSON) > 0 {
		if err := json.Unmarshal(shocksJSON, &s.Shocks); err != nil {
			return Scenario{}, fmt.Errorf("stress: decode shocks: %w", err)
		}
	}
	return s, nil
}

// ErrNotFound is returned when GetScenario can't find a row.
var ErrNotFound = errors.New("stress: scenario not found")

// ListScenarios returns every scenario, ordered by category then
// name. The set is small (dozens, not thousands) so we don't
// paginate.
func (r *Repo) ListScenarios(ctx context.Context, category Category) ([]Scenario, error) {
	q := `SELECT id, name, category, description, shocks, created_by, created_at, updated_at
	        FROM stress_scenarios`
	args := []interface{}{}
	if category != "" {
		q += ` WHERE category = $1`
		args = append(args, string(category))
	}
	q += ` ORDER BY category, name`
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("stress: list scenarios: %w", err)
	}
	defer rows.Close()
	var out []Scenario
	for rows.Next() {
		var s Scenario
		var cat string
		var shocksJSON []byte
		var createdBy sql.NullString
		if err := rows.Scan(&s.ID, &s.Name, &cat, &s.Description, &shocksJSON, &createdBy, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, fmt.Errorf("stress: scan scenario: %w", err)
		}
		s.Category = Category(cat)
		if createdBy.Valid {
			s.CreatedBy = createdBy.String
		}
		if len(shocksJSON) > 0 {
			if err := json.Unmarshal(shocksJSON, &s.Shocks); err != nil {
				return nil, fmt.Errorf("stress: decode shocks: %w", err)
			}
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stress: iter scenarios: %w", err)
	}
	return out, nil
}

// UpsertScenario inserts a new scenario or updates an existing
// one. Matches on the unique `name` column. createdBy is
// optional; pass "" to leave it unset.
func (r *Repo) UpsertScenario(ctx context.Context, s Scenario, createdBy string) (Scenario, error) {
	if err := s.Validate(); err != nil {
		return Scenario{}, err
	}
	shocksJSON, err := json.Marshal(s.Shocks)
	if err != nil {
		return Scenario{}, fmt.Errorf("stress: encode shocks: %w", err)
	}
	var createdByArg interface{}
	if strings.TrimSpace(createdBy) != "" {
		createdByArg = createdBy
	}
	row := r.db.QueryRowContext(ctx,
		`INSERT INTO stress_scenarios (name, category, description, shocks, created_by)
		 VALUES ($1, $2, $3, $4::jsonb, $5)
		 ON CONFLICT (name) DO UPDATE
		   SET category    = EXCLUDED.category,
		       description = EXCLUDED.description,
		       shocks      = EXCLUDED.shocks,
		       updated_at  = NOW()
		 RETURNING id, name, category, description, shocks, created_by, created_at, updated_at`,
		s.Name, string(s.Category), s.Description, shocksJSON, createdByArg,
	)
	var out Scenario
	var cat string
	var shocksBytes []byte
	var cb sql.NullString
	if err := row.Scan(&out.ID, &out.Name, &cat, &out.Description, &shocksBytes, &cb, &out.CreatedAt, &out.UpdatedAt); err != nil {
		return Scenario{}, fmt.Errorf("stress: upsert scenario: %w", err)
	}
	out.Category = Category(cat)
	if cb.Valid {
		out.CreatedBy = cb.String
	}
	if len(shocksBytes) > 0 {
		_ = json.Unmarshal(shocksBytes, &out.Shocks)
	}
	return out, nil
}

// DeleteScenario removes a scenario by id. Cascading FK in the
// migration removes any portfolio_stress_results rows that
// referenced it.
func (r *Repo) DeleteScenario(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("stress: scenario id required")
	}
	res, err := r.db.ExecContext(ctx, `DELETE FROM stress_scenarios WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("stress: delete scenario: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// AppendResult writes the full Result + per-holding impacts as
// one row. Impacts go into a JSONB column so the UI drill-down
// stays atomic with the parent stress run.
func (r *Repo) AppendResult(ctx context.Context, res Result) error {
	if strings.TrimSpace(res.FundID) == "" || strings.TrimSpace(res.ScenarioID) == "" {
		return errors.New("stress: fund_id and scenario_id required")
	}
	impactsJSON, err := json.Marshal(res.Impacts)
	if err != nil {
		return fmt.Errorf("stress: encode impacts: %w", err)
	}
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO portfolio_stress_results (
			fund_id, scenario_id, calculated_at,
			nav_before, nav_after, pnl_total, pnl_pct,
			holding_count, shocked_count, impacts
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb)`,
		res.FundID, res.ScenarioID, res.GeneratedAt,
		res.NAVBefore, res.NAVAfter, res.PnLTotal, res.PnLPct,
		res.HoldingCount, res.ShockedCount, impactsJSON,
	)
	if err != nil {
		return fmt.Errorf("stress: insert result: %w", err)
	}
	return nil
}

// ListResultsParams scopes the trend / history query.
type ListResultsParams struct {
	FundID     string
	ScenarioID string
	Limit      int
}

// ListResults returns the last N stress runs for a fund, newest
// first. When ScenarioID is non-empty the query filters to one
// scenario.
func (r *Repo) ListResults(ctx context.Context, p ListResultsParams) ([]Result, error) {
	if strings.TrimSpace(p.FundID) == "" {
		return nil, errors.New("stress: fund_id required")
	}
	limit := p.Limit
	if limit <= 0 || limit > 365 {
		limit = 90
	}
	q := `SELECT fund_id, scenario_id, calculated_at,
	             nav_before, nav_after, pnl_total, pnl_pct,
	             holding_count, shocked_count, impacts
	        FROM portfolio_stress_results
	       WHERE fund_id = $1`
	args := []interface{}{p.FundID}
	if strings.TrimSpace(p.ScenarioID) != "" {
		q += ` AND scenario_id = $2 ORDER BY calculated_at DESC LIMIT $3`
		args = append(args, p.ScenarioID, limit)
	} else {
		q += ` ORDER BY calculated_at DESC LIMIT $2`
		args = append(args, limit)
	}
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("stress: list results: %w", err)
	}
	defer rows.Close()
	var out []Result
	for rows.Next() {
		var res Result
		var impactsJSON []byte
		if err := rows.Scan(&res.FundID, &res.ScenarioID, &res.GeneratedAt,
			&res.NAVBefore, &res.NAVAfter, &res.PnLTotal, &res.PnLPct,
			&res.HoldingCount, &res.ShockedCount, &impactsJSON); err != nil {
			return nil, fmt.Errorf("stress: scan result: %w", err)
		}
		if len(impactsJSON) > 0 {
			_ = json.Unmarshal(impactsJSON, &res.Impacts)
		}
		out = append(out, res)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stress: iter results: %w", err)
	}
	return out, nil
}

// Silence "imported and not used" warnings from generated SDK
// trees by referencing the time package.
var _ = time.Now
