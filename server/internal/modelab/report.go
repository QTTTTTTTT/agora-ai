package modelab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Report is the per-experiment, per-arm metrics roll-up the
// admin UI consumes. Built by Reporter.Compute from the rows
// in model_ab_assignments + model_ab_shadow_responses (plus an
// optional injected primary-output supplier for the agreement
// metric).
//
// Layout:
//
//	{
//	  "experiment": {<short summary>},
//	  "arms": [
//	    {
//	      "arm_index": 0, "arm_name": "control", "arm_label": "openai/gpt-4o",
//	      "primary_count": 142, "shadow_count": 0,
//	      "errors": 0, "avg_latency_ms": 1320, "total_tokens": 18900,
//	      "total_cost_micro": 12345, "agreement_with_primary_pct": null
//	    },
//	    {
//	      "arm_index": 1, "arm_name": "treat", "arm_label": "claude/claude-opus",
//	      "primary_count": 0, "shadow_count": 142,
//	      "errors": 3, "avg_latency_ms": 980, "total_tokens": 16210,
//	      "total_cost_micro": 21030, "agreement_with_primary_pct": 0.74
//	    }
//	  ]
//	}
//
// The agreement metric compares the "verdict" / "stance" field
// across primary and shadow JSON outputs for the same
// assignment. Different arms with the same answer = agreement.
type Report struct {
	Experiment ReportExperiment  `json:"experiment"`
	Arms       []ReportArmMetric `json:"arms"`
	Window     ReportWindow      `json:"window"`
}

// ReportExperiment is a slimmed projection of the model_ab_experiments
// row — the report doesn't need the full Arms JSONB inline.
type ReportExperiment struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Scope       string    `json:"scope"`
	ScopeTarget string    `json:"scope_target,omitempty"`
	Status      string    `json:"status"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	EndedAt     time.Time `json:"ended_at,omitempty"`
}

// ReportWindow bounds the rows considered for this report.
type ReportWindow struct {
	From time.Time `json:"from,omitempty"`
	To   time.Time `json:"to,omitempty"`
}

// ReportArmMetric is the per-arm aggregate. Numbers are
// computed across whatever window the caller passed in.
type ReportArmMetric struct {
	ArmIndex      int    `json:"arm_index"`
	ArmName       string `json:"arm_name"`
	ArmLabel      string `json:"arm_label"`
	PrimaryCount  int    `json:"primary_count"`
	ShadowCount   int    `json:"shadow_count"`
	ErrorCount    int    `json:"error_count"`
	AvgLatencyMs  int    `json:"avg_latency_ms"`
	TotalInputTok int64  `json:"total_input_tokens"`
	TotalOutTok   int64  `json:"total_output_tokens"`
	TotalCostMicr int64  `json:"total_cost_micro"`
	// AgreementWithPrimary expresses how often this arm
	// produced the same primary-key field as the primary arm.
	// For PM decisions we compare "stance" / "verdict"; the
	// chosen field is whichever Reporter.AgreementField points
	// at. Negative = no comparable pairs were found.
	AgreementWithPrimary float64 `json:"agreement_with_primary_pct"`
}

// Reporter computes Reports from the modelab repo. The struct
// is intentionally tiny — most of the heavy lifting is SQL
// aggregates inside the repo helpers.
type Reporter struct {
	Repo *Repo

	// AgreementField is the JSON key (case-sensitive) Reporter
	// extracts from parsed_output to compute the agreement
	// metric. Defaults to "stance" — PM decisions surface that
	// field — but operators experimenting on other steps can
	// override.
	AgreementField string
}

// NewReporter constructs a Reporter with the default agreement
// field.
func NewReporter(repo *Repo) *Reporter {
	return &Reporter{Repo: repo, AgreementField: "stance"}
}

// Compute builds the full Report for a single experiment over
// an optional window. Pass zero values for from/to to scan the
// entire history.
func (r *Reporter) Compute(ctx context.Context, experimentID string, from, to time.Time) (*Report, error) {
	if r == nil || r.Repo == nil {
		return nil, errors.New("modelab: reporter or repo nil")
	}
	exp, err := r.Repo.GetExperiment(ctx, experimentID)
	if err != nil {
		return nil, err
	}

	rep := &Report{
		Experiment: ReportExperiment{
			ID:          exp.ID,
			Name:        exp.Name,
			Scope:       string(exp.Scope),
			ScopeTarget: exp.ScopeTarget,
			Status:      string(exp.Status),
			StartedAt:   exp.StartAt,
			EndedAt:     exp.EndAt,
		},
		Window: ReportWindow{From: from, To: to},
	}

	// Initialise per-arm metric slots from the experiment's arms
	// so arms with zero traffic still appear in the report.
	armMetrics := make([]*ReportArmMetric, len(exp.Arms))
	for i, arm := range exp.Arms {
		armMetrics[i] = &ReportArmMetric{
			ArmIndex:             i,
			ArmName:              arm.Name,
			ArmLabel:             arm.Label(),
			AgreementWithPrimary: -1,
		}
	}

	// Primary-arm rows live in model_ab_assignments. They tell
	// us how many production calls landed on each arm. The
	// shadow rows tell us latency / tokens / cost / errors for
	// the non-primary arms.
	assignments, err := r.Repo.listAssignmentsInWindow(ctx, experimentID, from, to)
	if err != nil {
		return nil, err
	}
	for _, a := range assignments {
		if a.ArmIndex < 0 || a.ArmIndex >= len(armMetrics) {
			continue
		}
		armMetrics[a.ArmIndex].PrimaryCount++
	}

	shadows, err := r.Repo.listShadowsInWindow(ctx, experimentID, from, to)
	if err != nil {
		return nil, err
	}
	// Build a quick (run, step, agent) → arm0 primary-output map
	// for the agreement metric. We never know the primary's
	// output from shadow_responses alone — the dispatcher only
	// persists non-primary arms there. So we read the primary's
	// answer from the assignments table's "step" + "run_id" and
	// look it up via the optional primaryOutputLookup if
	// supplied. Without that, agreement defaults to -1 (n/a).
	// (Full primary-output capture is deferred to S10.3.B; we
	// already have the building blocks.)
	latencySum := make([]int64, len(armMetrics))
	latencyCount := make([]int, len(armMetrics))
	for _, s := range shadows {
		if s.ArmIndex < 0 || s.ArmIndex >= len(armMetrics) {
			continue
		}
		m := armMetrics[s.ArmIndex]
		m.ShadowCount++
		if s.ErrorText != "" {
			m.ErrorCount++
		}
		m.TotalInputTok += int64(s.InputTokens)
		m.TotalOutTok += int64(s.OutputTokens)
		m.TotalCostMicr += s.CostMicro
		if s.LatencyMs > 0 {
			latencySum[s.ArmIndex] += int64(s.LatencyMs)
			latencyCount[s.ArmIndex]++
		}
	}
	for i, m := range armMetrics {
		if latencyCount[i] > 0 {
			m.AvgLatencyMs = int(latencySum[i] / int64(latencyCount[i]))
		}
	}

	rep.Arms = make([]ReportArmMetric, len(armMetrics))
	for i, m := range armMetrics {
		rep.Arms[i] = *m
	}
	return rep, nil
}

// ExtractField returns the string value at the JSON key
// AgreementField from a raw output blob. Returns "" if the
// blob isn't JSON or the field is missing / empty / not a
// string. Exposed as a pure helper so tests can validate it
// directly.
func (r *Reporter) ExtractField(raw json.RawMessage) string {
	field := r.AgreementField
	if strings.TrimSpace(field) == "" {
		return ""
	}
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	v, ok := m[field]
	if !ok {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// --- Repo helpers for windowed scans ---------------------------------------

// listAssignmentsInWindow returns model_ab_assignments rows for
// the experiment, optionally bounded by an inclusive window. The
// repo's normal List is unbounded — this one trims.
func (r *Repo) listAssignmentsInWindow(ctx context.Context, experimentID string, from, to time.Time) ([]*Assignment, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	q := `
		SELECT id::text, experiment_id::text, run_id, step,
		       COALESCE(agent_id,''), COALESCE(fund_id,''),
		       arm_index, arm_name, assigned_at
		FROM model_ab_assignments
		WHERE experiment_id = $1::uuid`
	args := []any{experimentID}
	if !from.IsZero() {
		args = append(args, from)
		q += fmt.Sprintf(" AND assigned_at >= $%d", len(args))
	}
	if !to.IsZero() {
		args = append(args, to)
		q += fmt.Sprintf(" AND assigned_at <= $%d", len(args))
	}
	q += " ORDER BY assigned_at ASC"

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("modelab: list assignments in window: %w", err)
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

// listShadowsInWindow returns model_ab_shadow_responses rows for
// the experiment, optionally bounded by an inclusive window.
func (r *Repo) listShadowsInWindow(ctx context.Context, experimentID string, from, to time.Time) ([]*ShadowResponse, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	q := `
		SELECT id::text, experiment_id::text, assignment_id::text, run_id, step,
		       COALESCE(agent_id,''), COALESCE(fund_id,''),
		       arm_index, arm_name, arm_model,
		       COALESCE(raw_output,''), parsed_output, COALESCE(parse_error,''),
		       COALESCE(input_tokens,0), COALESCE(output_tokens,0),
		       COALESCE(latency_ms,0), COALESCE(cost_micro,0),
		       COALESCE(error_text,''), finished_at
		FROM model_ab_shadow_responses
		WHERE experiment_id = $1::uuid`
	args := []any{experimentID}
	if !from.IsZero() {
		args = append(args, from)
		q += fmt.Sprintf(" AND finished_at >= $%d", len(args))
	}
	if !to.IsZero() {
		args = append(args, to)
		q += fmt.Sprintf(" AND finished_at <= $%d", len(args))
	}
	q += " ORDER BY finished_at ASC"

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("modelab: list shadows in window: %w", err)
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
