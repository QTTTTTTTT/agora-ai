// Package agentreputation persists per-agent realised
// performance ("did this agent's bullish call on AAPL on
// 2026-06-02 actually beat the benchmark over the next 5
// days?"). The PM reads the rolling stats to discount
// historically-bad agents and reward consistent winners.
//
// Tables: agent_reputation_outcomes (one row per decision +
// realised return horizon), agent_reputation_stats (rolling
// summary keyed by fund_id+agent_id, recomputed by the
// backfill job).
package agentreputation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound is returned by single-row reads when no row matches.
var ErrNotFound = errors.New("agentreputation: not found")

// AgentKind enumerates the agent personas the reputation ledger
// tracks. Mirrors the agent.AnalystCategory + advocate stance +
// PM/researcher buckets.
type AgentKind string

const (
	KindAnalyst    AgentKind = "analyst"
	KindAdvocate   AgentKind = "advocate"
	KindPM         AgentKind = "pm"
	KindResearcher AgentKind = "researcher"
)

// IsValid reports whether k is one of the known kinds.
func (k AgentKind) IsValid() bool {
	switch k {
	case KindAnalyst, KindAdvocate, KindPM, KindResearcher:
		return true
	}
	return false
}

// Direction matches the agent / debate direction taxonomy.
type Direction string

const (
	DirBullish Direction = "bullish"
	DirBearish Direction = "bearish"
	DirNeutral Direction = "neutral"
)

// IsValid reports whether d is one of the known directions.
func (d Direction) IsValid() bool { return d == DirBullish || d == DirBearish || d == DirNeutral }

// Outcome is one materialised decision row.
type Outcome struct {
	ID              string
	FundID          string
	AgentID         string
	AgentName       string
	AgentKind       AgentKind
	Category        string
	Symbol          string
	AsOf            time.Time
	Direction       Direction
	Confidence      int
	RealisedReturn  float64
	BenchmarkReturn float64
	Alpha           float64
	HorizonDays     int
	SourcePanelID   sql.NullString
	SourceDebateID  sql.NullString
	Note            string
	CreatedAt       time.Time
}

// Validate enforces the must-have fields.
func (o Outcome) Validate() error {
	if strings.TrimSpace(o.FundID) == "" {
		return errors.New("agentreputation: outcome.FundID required")
	}
	if strings.TrimSpace(o.AgentID) == "" {
		return errors.New("agentreputation: outcome.AgentID required")
	}
	if !o.AgentKind.IsValid() {
		return fmt.Errorf("agentreputation: outcome.AgentKind %q invalid", o.AgentKind)
	}
	if !o.Direction.IsValid() {
		return fmt.Errorf("agentreputation: outcome.Direction %q invalid", o.Direction)
	}
	if strings.TrimSpace(o.Symbol) == "" {
		return errors.New("agentreputation: outcome.Symbol required")
	}
	if o.Confidence < 0 || o.Confidence > 100 {
		return fmt.Errorf("agentreputation: outcome.Confidence %d out of [0,100]", o.Confidence)
	}
	if o.HorizonDays <= 0 {
		o.HorizonDays = 1
	}
	if o.AsOf.IsZero() {
		return errors.New("agentreputation: outcome.AsOf required")
	}
	return nil
}

// Stats is one row of the agent_reputation_stats summary.
type Stats struct {
	FundID         string
	AgentID        string
	AgentName      string
	AgentKind      AgentKind
	Category       string
	DecisionsCount int64
	HitsCount      int64
	MissesCount    int64
	AvgAlpha       float64
	SumAlpha       float64
	AvgConfidence  float64
	LastDecisionAt sql.NullTime
	UpdatedAt      time.Time
}

// HitRate is the convenience derived metric (0..1).
func (s Stats) HitRate() float64 {
	if s.DecisionsCount == 0 {
		return 0
	}
	return float64(s.HitsCount) / float64(s.DecisionsCount)
}

// Repo is the persistence façade.
type Repo struct {
	db *sql.DB
}

// NewRepo wires the repo to a *sql.DB.
func NewRepo(db *sql.DB) *Repo { return &Repo{db: db} }

// --- Writes -----------------------------------------------------------------

// UpsertOutcomes writes a batch of Outcome rows in a single
// transaction. The unique (fund_id, agent_id, symbol, asof,
// horizon_days) constraint means re-running the backfill is
// idempotent — existing rows are overwritten with the latest
// realised numbers.
func (r *Repo) UpsertOutcomes(ctx context.Context, outs []Outcome) error {
	if r == nil || r.db == nil {
		return errors.New("agentreputation: repo not initialised")
	}
	if len(outs) == 0 {
		return nil
	}
	for i := range outs {
		if outs[i].HorizonDays <= 0 {
			outs[i].HorizonDays = 1
		}
		if err := outs[i].Validate(); err != nil {
			return fmt.Errorf("outcome %d: %w", i, err)
		}
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("agentreputation: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const q = `INSERT INTO agent_reputation_outcomes
		(fund_id, agent_id, agent_name, agent_kind, category, symbol, asof,
		 direction, confidence, realised_return, benchmark_return, alpha,
		 horizon_days, source_panel_id, source_debate_id, note)
		VALUES ($1, $2, $3, $4, $5, $6, $7,
		        $8, $9, $10, $11, $12,
		        $13, $14, $15, $16)
		ON CONFLICT (fund_id, agent_id, symbol, asof, horizon_days)
		DO UPDATE SET agent_name = EXCLUDED.agent_name,
		              agent_kind = EXCLUDED.agent_kind,
		              category = EXCLUDED.category,
		              direction = EXCLUDED.direction,
		              confidence = EXCLUDED.confidence,
		              realised_return = EXCLUDED.realised_return,
		              benchmark_return = EXCLUDED.benchmark_return,
		              alpha = EXCLUDED.alpha,
		              source_panel_id = EXCLUDED.source_panel_id,
		              source_debate_id = EXCLUDED.source_debate_id,
		              note = EXCLUDED.note`
	stmt, err := tx.PrepareContext(ctx, q)
	if err != nil {
		return fmt.Errorf("agentreputation: prepare upsert: %w", err)
	}
	defer stmt.Close()
	for _, o := range outs {
		if _, err := stmt.ExecContext(ctx,
			o.FundID, o.AgentID, o.AgentName, string(o.AgentKind), o.Category,
			strings.ToUpper(o.Symbol), o.AsOf,
			string(o.Direction), o.Confidence, o.RealisedReturn, o.BenchmarkReturn, o.Alpha,
			o.HorizonDays, o.SourcePanelID, o.SourceDebateID, o.Note,
		); err != nil {
			return fmt.Errorf("agentreputation: upsert outcome (%s/%s/%s): %w", o.FundID, o.AgentID, o.Symbol, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("agentreputation: commit tx: %w", err)
	}
	return nil
}

// RecomputeStats rebuilds the agent_reputation_stats row(s) for
// the given fund (or all funds when fundID == "") from the
// outcomes table. Idempotent.
func (r *Repo) RecomputeStats(ctx context.Context, fundID string) error {
	if r == nil || r.db == nil {
		return errors.New("agentreputation: repo not initialised")
	}
	const q = `INSERT INTO agent_reputation_stats AS s
		(fund_id, agent_id, agent_name, agent_kind, category,
		 decisions_count, hits_count, misses_count,
		 avg_alpha, sum_alpha, avg_confidence, last_decision_at, updated_at)
		SELECT fund_id, agent_id,
		       MAX(agent_name)             AS agent_name,
		       MAX(agent_kind)             AS agent_kind,
		       MAX(category)               AS category,
		       COUNT(*)                    AS decisions_count,
		       SUM(CASE WHEN (direction='bullish' AND realised_return > 0)
		                  OR (direction='bearish' AND realised_return < 0)
		                THEN 1 ELSE 0 END) AS hits_count,
		       SUM(CASE WHEN (direction='bullish' AND realised_return < 0)
		                  OR (direction='bearish' AND realised_return > 0)
		                THEN 1 ELSE 0 END) AS misses_count,
		       AVG(alpha)                  AS avg_alpha,
		       SUM(alpha)                  AS sum_alpha,
		       AVG(confidence)             AS avg_confidence,
		       MAX(asof)                   AS last_decision_at,
		       now()                       AS updated_at
		  FROM agent_reputation_outcomes
		 WHERE ($1::uuid IS NULL OR fund_id = $1::uuid)
		 GROUP BY fund_id, agent_id
		ON CONFLICT (fund_id, agent_id)
		DO UPDATE SET agent_name = EXCLUDED.agent_name,
		              agent_kind = EXCLUDED.agent_kind,
		              category = EXCLUDED.category,
		              decisions_count = EXCLUDED.decisions_count,
		              hits_count = EXCLUDED.hits_count,
		              misses_count = EXCLUDED.misses_count,
		              avg_alpha = EXCLUDED.avg_alpha,
		              sum_alpha = EXCLUDED.sum_alpha,
		              avg_confidence = EXCLUDED.avg_confidence,
		              last_decision_at = EXCLUDED.last_decision_at,
		              updated_at = now()
		WHERE s.fund_id = EXCLUDED.fund_id`
	var arg interface{}
	if strings.TrimSpace(fundID) != "" {
		arg = fundID
	}
	if _, err := r.db.ExecContext(ctx, q, arg); err != nil {
		return fmt.Errorf("agentreputation: recompute stats: %w", err)
	}
	return nil
}

// --- Reads ------------------------------------------------------------------

// ListStatsParams filters the stats listing.
type ListStatsParams struct {
	FundID    string
	AgentKind AgentKind
	Limit     int
}

// ListStats returns rolling per-agent stats ordered by avg_alpha
// DESC (best first). Filterable by fund + kind.
func (r *Repo) ListStats(ctx context.Context, p ListStatsParams) ([]Stats, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("agentreputation: repo not initialised")
	}
	conds := []string{}
	args := []interface{}{}
	if strings.TrimSpace(p.FundID) != "" {
		args = append(args, p.FundID)
		conds = append(conds, fmt.Sprintf("fund_id = $%d", len(args)))
	}
	if p.AgentKind != "" {
		args = append(args, string(p.AgentKind))
		conds = append(conds, fmt.Sprintf("agent_kind = $%d", len(args)))
	}
	q := `SELECT fund_id, agent_id, agent_name, agent_kind, category,
	             decisions_count, hits_count, misses_count,
	             avg_alpha, sum_alpha, avg_confidence,
	             last_decision_at, updated_at
	        FROM agent_reputation_stats`
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY avg_alpha DESC, decisions_count DESC"
	limit := p.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args = append(args, limit)
	q += fmt.Sprintf(" LIMIT $%d", len(args))

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("agentreputation: list stats: %w", err)
	}
	defer rows.Close()
	return scanStatsRows(rows)
}

// ListOutcomesParams filters the outcomes listing.
type ListOutcomesParams struct {
	FundID  string
	AgentID string
	Symbol  string
	Limit   int
}

// ListOutcomes returns recent outcomes (latest first).
func (r *Repo) ListOutcomes(ctx context.Context, p ListOutcomesParams) ([]Outcome, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("agentreputation: repo not initialised")
	}
	conds := []string{}
	args := []interface{}{}
	if strings.TrimSpace(p.FundID) != "" {
		args = append(args, p.FundID)
		conds = append(conds, fmt.Sprintf("fund_id = $%d", len(args)))
	}
	if strings.TrimSpace(p.AgentID) != "" {
		args = append(args, p.AgentID)
		conds = append(conds, fmt.Sprintf("agent_id = $%d", len(args)))
	}
	if strings.TrimSpace(p.Symbol) != "" {
		args = append(args, strings.ToUpper(p.Symbol))
		conds = append(conds, fmt.Sprintf("symbol = $%d", len(args)))
	}
	q := `SELECT id, fund_id, agent_id, agent_name, agent_kind, category, symbol, asof,
	             direction, confidence, realised_return, benchmark_return, alpha,
	             horizon_days, source_panel_id, source_debate_id, note, created_at
	        FROM agent_reputation_outcomes`
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY asof DESC, created_at DESC"
	limit := p.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args = append(args, limit)
	q += fmt.Sprintf(" LIMIT $%d", len(args))

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("agentreputation: list outcomes: %w", err)
	}
	defer rows.Close()
	return scanOutcomeRows(rows)
}

// GetStats fetches a single (fund, agent) summary.
func (r *Repo) GetStats(ctx context.Context, fundID, agentID string) (Stats, error) {
	if r == nil || r.db == nil {
		return Stats{}, errors.New("agentreputation: repo not initialised")
	}
	const q = `SELECT fund_id, agent_id, agent_name, agent_kind, category,
	                  decisions_count, hits_count, misses_count,
	                  avg_alpha, sum_alpha, avg_confidence,
	                  last_decision_at, updated_at
	             FROM agent_reputation_stats
	            WHERE fund_id = $1 AND agent_id = $2`
	rows, err := r.db.QueryContext(ctx, q, fundID, agentID)
	if err != nil {
		return Stats{}, fmt.Errorf("agentreputation: get stats: %w", err)
	}
	defer rows.Close()
	out, err := scanStatsRows(rows)
	if err != nil {
		return Stats{}, err
	}
	if len(out) == 0 {
		return Stats{}, ErrNotFound
	}
	return out[0], nil
}

// --- helpers ----------------------------------------------------------------

func scanStatsRows(rows *sql.Rows) ([]Stats, error) {
	var out []Stats
	for rows.Next() {
		var s Stats
		var kind string
		if err := rows.Scan(
			&s.FundID, &s.AgentID, &s.AgentName, &kind, &s.Category,
			&s.DecisionsCount, &s.HitsCount, &s.MissesCount,
			&s.AvgAlpha, &s.SumAlpha, &s.AvgConfidence,
			&s.LastDecisionAt, &s.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("agentreputation: scan stats: %w", err)
		}
		s.AgentKind = AgentKind(kind)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agentreputation: stats rows: %w", err)
	}
	return out, nil
}

func scanOutcomeRows(rows *sql.Rows) ([]Outcome, error) {
	var out []Outcome
	for rows.Next() {
		var o Outcome
		var kind, dir string
		if err := rows.Scan(
			&o.ID, &o.FundID, &o.AgentID, &o.AgentName, &kind, &o.Category, &o.Symbol, &o.AsOf,
			&dir, &o.Confidence, &o.RealisedReturn, &o.BenchmarkReturn, &o.Alpha,
			&o.HorizonDays, &o.SourcePanelID, &o.SourceDebateID, &o.Note, &o.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("agentreputation: scan outcome: %w", err)
		}
		o.AgentKind = AgentKind(kind)
		o.Direction = Direction(dir)
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agentreputation: outcome rows: %w", err)
	}
	return out, nil
}
