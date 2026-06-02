package modelab

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

// Repo wraps the three tables 076_model_ab_experiments creates.
// It uses *sql.DB so it composes with the rest of the platform's
// repos; no ORM is needed because the surface is small.
type Repo struct {
	db *sql.DB
}

// NewRepo constructs a Repo. A nil db is a wiring bug and the
// returned Repo will return errors on every call rather than
// panicking, so the hot path stays safe in degraded test
// configurations.
func NewRepo(db *sql.DB) *Repo { return &Repo{db: db} }

// ErrNotFound is the canonical "no such row" sentinel for the
// repo. Callers use errors.Is to discriminate.
var ErrNotFound = errors.New("modelab: not found")

// --- Experiment CRUD -------------------------------------------------------

// CreateExperiment validates and persists a draft experiment.
// Returns the newly assigned ID. Caller is responsible for
// setting status=running (via SetStatus) when ready to dispatch.
func (r *Repo) CreateExperiment(ctx context.Context, e *Experiment) (string, error) {
	if r == nil || r.db == nil {
		return "", errors.New("modelab: repo not initialised")
	}
	if err := e.Validate(); err != nil {
		return "", err
	}
	armsJSON, err := MarshalArms(e.Arms)
	if err != nil {
		return "", fmt.Errorf("modelab: marshal arms: %w", err)
	}
	stepFilter := e.StepFilter
	if stepFilter == nil {
		stepFilter = []string{}
	}
	var id string
	err = r.db.QueryRowContext(ctx, `
		INSERT INTO model_ab_experiments (
			name, description, scope, scope_target, step_filter,
			arms, traffic_split, status, start_at, end_at,
			max_total_tokens, created_by
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		RETURNING id::text
	`,
		e.Name,
		nullableString(e.Description),
		string(e.Scope),
		nullableString(e.ScopeTarget),
		pq.Array(stepFilter),
		armsJSON,
		pq.Array(e.TrafficSplit),
		string(coalesceStatus(e.Status)),
		nullableTime(e.StartAt),
		nullableTime(e.EndAt),
		nullableInt64(e.MaxTotalTokens),
		nullableString(e.CreatedBy),
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("modelab: insert experiment: %w", err)
	}
	return id, nil
}

// GetExperiment loads one row by ID. Returns ErrNotFound on miss.
func (r *Repo) GetExperiment(ctx context.Context, id string) (*Experiment, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("modelab: repo not initialised")
	}
	row := r.db.QueryRowContext(ctx, experimentSelectColumns+`
		FROM model_ab_experiments
		WHERE id = $1::uuid
	`, id)
	return scanExperiment(row)
}

// ListExperiments returns experiments filtered by status (empty
// → all). Ordered by created_at DESC so the admin UI shows the
// newest experiment first by default.
func (r *Repo) ListExperiments(ctx context.Context, statuses []ExperimentStatus, limit int) ([]*Experiment, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("modelab: repo not initialised")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args := []any{}
	where := ""
	if len(statuses) > 0 {
		strs := make([]string, 0, len(statuses))
		for _, s := range statuses {
			strs = append(strs, string(s))
		}
		args = append(args, pq.Array(strs))
		where = "WHERE status = ANY($1::text[])"
	}
	args = append(args, limit)
	q := fmt.Sprintf(`%s
		FROM model_ab_experiments
		%s
		ORDER BY created_at DESC
		LIMIT $%d
	`, experimentSelectColumns, where, len(args))
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("modelab: list experiments: %w", err)
	}
	defer rows.Close()
	var out []*Experiment
	for rows.Next() {
		e, err := scanExperiment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListRunningMatching returns the (typically small) subset of
// running experiments whose scope filter matches the supplied
// (fund, agent, role) tuple. The router calls this once per
// request — it MUST be fast. We index on (status, scope,
// scope_target) so the hot path is a small index scan.
//
// We deliberately don't filter by step here — step_filter is
// matched in-memory via Experiment.Match. The DB index is
// already narrow enough.
func (r *Repo) ListRunningMatching(ctx context.Context, fundID, agentID, agentRole string) ([]*Experiment, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	q := experimentSelectColumns + `
		FROM model_ab_experiments
		WHERE status = 'running'
		  AND (
			scope = 'global'
			OR (scope = 'fund'        AND scope_target = $1)
			OR (scope = 'agent_id'    AND scope_target = $2)
			OR (scope = 'agent_role'  AND scope_target = $3)
		  )
		ORDER BY created_at DESC
	`
	rows, err := r.db.QueryContext(ctx, q, fundID, agentID, agentRole)
	if err != nil {
		return nil, fmt.Errorf("modelab: list matching: %w", err)
	}
	defer rows.Close()
	var out []*Experiment
	for rows.Next() {
		e, err := scanExperiment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SetStatus updates the lifecycle column. Returns ErrNotFound if
// no row was affected, so callers know whether the experiment
// even exists.
func (r *Repo) SetStatus(ctx context.Context, id string, status ExperimentStatus) error {
	if r == nil || r.db == nil {
		return errors.New("modelab: repo not initialised")
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE model_ab_experiments
		SET status = $1, updated_at = NOW(),
		    start_at = COALESCE(start_at, CASE WHEN $1='running' THEN NOW() ELSE NULL END),
		    end_at   = COALESCE(end_at, CASE WHEN $1 IN ('completed','archived') THEN NOW() ELSE NULL END)
		WHERE id = $2::uuid
	`, string(status), id)
	if err != nil {
		return fmt.Errorf("modelab: set status: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// AddTokens atomically increments tokens_used on the experiment
// row. Used by the shadow dispatcher after each call so the
// max_total_tokens guard can fire. Best-effort: a failure here
// is logged but never aborts the workflow.
func (r *Repo) AddTokens(ctx context.Context, id string, n int64) error {
	if r == nil || r.db == nil || n <= 0 {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE model_ab_experiments
		SET tokens_used = tokens_used + $1, updated_at = NOW()
		WHERE id = $2::uuid
	`, n, id)
	return err
}

// --- Assignment sticky-arm -------------------------------------------------

// UpsertAssignment writes (or returns existing) the sticky-arm
// row for (experiment, run, step, agent). The unique index on
// the same 4-tuple guarantees that two concurrent calls for the
// same tuple converge on a single row.
func (r *Repo) UpsertAssignment(ctx context.Context, a *Assignment) (*Assignment, error) {
	if r == nil || r.db == nil || a == nil {
		return nil, errors.New("modelab: repo or assignment nil")
	}
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO model_ab_assignments (
			experiment_id, run_id, step, agent_id, fund_id, arm_index, arm_name
		) VALUES ($1::uuid, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (experiment_id, run_id, step, agent_id) DO UPDATE
		   SET assigned_at = model_ab_assignments.assigned_at
		RETURNING id::text, arm_index, arm_name, assigned_at
	`, a.ExperimentID, a.RunID, a.Step, a.AgentID, nullableString(a.FundID), a.ArmIndex, a.ArmName)
	out := &Assignment{
		ExperimentID: a.ExperimentID,
		RunID:        a.RunID,
		Step:         a.Step,
		AgentID:      a.AgentID,
		FundID:       a.FundID,
	}
	if err := row.Scan(&out.ID, &out.ArmIndex, &out.ArmName, &out.AssignedAt); err != nil {
		return nil, fmt.Errorf("modelab: upsert assignment: %w", err)
	}
	return out, nil
}

// ListAssignments returns the assignment rows for an experiment
// in (assigned_at DESC) order. Used by the report endpoint to
// project bucket fairness.
func (r *Repo) ListAssignments(ctx context.Context, experimentID string, limit int) ([]*Assignment, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id::text, experiment_id::text, run_id, step,
		       COALESCE(agent_id, ''), COALESCE(fund_id, ''),
		       arm_index, arm_name, assigned_at
		FROM model_ab_assignments
		WHERE experiment_id = $1::uuid
		ORDER BY assigned_at DESC
		LIMIT $2
	`, experimentID, limit)
	if err != nil {
		return nil, fmt.Errorf("modelab: list assignments: %w", err)
	}
	defer rows.Close()
	var out []*Assignment
	for rows.Next() {
		a := &Assignment{}
		if err := rows.Scan(&a.ID, &a.ExperimentID, &a.RunID, &a.Step,
			&a.AgentID, &a.FundID, &a.ArmIndex, &a.ArmName, &a.AssignedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// --- Shadow responses ------------------------------------------------------

// InsertShadowResponse persists the output of a non-primary arm.
// Errors are surfaced so the dispatcher can log them; failure to
// persist a shadow row does NOT fail the user-facing call.
func (r *Repo) InsertShadowResponse(ctx context.Context, s *ShadowResponse) error {
	if r == nil || r.db == nil || s == nil {
		return errors.New("modelab: repo or response nil")
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO model_ab_shadow_responses (
			experiment_id, assignment_id, run_id, step, agent_id, fund_id,
			arm_index, arm_name, arm_model,
			raw_output, parsed_output, parse_error,
			input_tokens, output_tokens, latency_ms, cost_micro,
			error_text
		) VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6,
		          $7, $8, $9,
		          $10, $11, $12,
		          $13, $14, $15, $16,
		          $17)
	`,
		s.ExperimentID, s.AssignmentID, s.RunID, s.Step,
		nullableString(s.AgentID), nullableString(s.FundID),
		s.ArmIndex, s.ArmName, s.ArmModel,
		nullableString(s.RawOutput), nullableJSON(s.ParsedOutput), nullableString(s.ParseError),
		nullableInt(s.InputTokens), nullableInt(s.OutputTokens), nullableInt(s.LatencyMs), nullableInt64(s.CostMicro),
		nullableString(s.ErrorText),
	)
	if err != nil {
		return fmt.Errorf("modelab: insert shadow response: %w", err)
	}
	return nil
}

// ListShadowResponses returns recent shadow rows for an
// experiment, used by the report endpoint.
func (r *Repo) ListShadowResponses(ctx context.Context, experimentID string, limit int) ([]*ShadowResponse, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id::text, experiment_id::text, assignment_id::text, run_id, step,
		       COALESCE(agent_id,''), COALESCE(fund_id,''),
		       arm_index, arm_name, arm_model,
		       COALESCE(raw_output,''), parsed_output, COALESCE(parse_error,''),
		       COALESCE(input_tokens,0), COALESCE(output_tokens,0),
		       COALESCE(latency_ms,0), COALESCE(cost_micro,0),
		       COALESCE(error_text,''), finished_at
		FROM model_ab_shadow_responses
		WHERE experiment_id = $1::uuid
		ORDER BY finished_at DESC
		LIMIT $2
	`, experimentID, limit)
	if err != nil {
		return nil, fmt.Errorf("modelab: list shadow responses: %w", err)
	}
	defer rows.Close()
	var out []*ShadowResponse
	for rows.Next() {
		s := &ShadowResponse{}
		var parsed []byte
		if err := rows.Scan(&s.ID, &s.ExperimentID, &s.AssignmentID, &s.RunID, &s.Step,
			&s.AgentID, &s.FundID,
			&s.ArmIndex, &s.ArmName, &s.ArmModel,
			&s.RawOutput, &parsed, &s.ParseError,
			&s.InputTokens, &s.OutputTokens,
			&s.LatencyMs, &s.CostMicro,
			&s.ErrorText, &s.FinishedAt); err != nil {
			return nil, err
		}
		if len(parsed) > 0 {
			s.ParsedOutput = json.RawMessage(parsed)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// --- internals -------------------------------------------------------------

const experimentSelectColumns = `
	SELECT id::text, name, COALESCE(description,''), scope, COALESCE(scope_target,''),
	       step_filter, arms, traffic_split,
	       status, COALESCE(start_at, '0001-01-01'::timestamptz),
	       COALESCE(end_at,   '0001-01-01'::timestamptz),
	       COALESCE(max_total_tokens, 0), COALESCE(tokens_used, 0),
	       COALESCE(created_by::text, ''),
	       created_at, updated_at
`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanExperiment(row rowScanner) (*Experiment, error) {
	e := &Experiment{}
	var stepFilter pq.StringArray
	var traffic pq.Float64Array
	var armsJSON []byte
	err := row.Scan(
		&e.ID, &e.Name, &e.Description, &e.Scope, &e.ScopeTarget,
		&stepFilter, &armsJSON, &traffic,
		&e.Status, &e.StartAt, &e.EndAt,
		&e.MaxTotalTokens, &e.TokensUsed,
		&e.CreatedBy,
		&e.CreatedAt, &e.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("modelab: scan experiment: %w", err)
	}
	e.StepFilter = []string(stepFilter)
	e.TrafficSplit = []float64(traffic)
	arms, err := UnmarshalArms(armsJSON)
	if err != nil {
		return nil, fmt.Errorf("modelab: parse arms: %w", err)
	}
	e.Arms = arms
	return e, nil
}

func coalesceStatus(s ExperimentStatus) ExperimentStatus {
	if strings.TrimSpace(string(s)) == "" {
		return StatusDraft
	}
	return s
}

func nullableString(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func nullableInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func nullableInt64(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func nullableJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}
