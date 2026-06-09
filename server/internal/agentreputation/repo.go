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
	// KindMaster / KindTactic are the /advisor-mode kinds added in
	// migration 099 — outcomes for these kinds are stored with
	// fund_id IS NULL because the advisor surface is not scoped
	// to a fund.
	KindMaster AgentKind = "master"
	KindTactic AgentKind = "tactic"
)

// IsValid reports whether k is one of the known kinds.
func (k AgentKind) IsValid() bool {
	switch k {
	case KindAnalyst, KindAdvocate, KindPM, KindResearcher, KindMaster, KindTactic:
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
	// Advisor-mode mappings (migration 099): master verdicts
	// (BUY/HOLD/AVOID/...) and tactic verdicts (BUY_TAIL/SKIP/...)
	// collapse into these four buckets when written to the
	// reputation ledger so the rollup math stays uniform.
	DirBuy   Direction = "buy"
	DirAvoid Direction = "avoid"
	DirSkip  Direction = "skip"
	DirWait  Direction = "wait"
)

// IsValid reports whether d is one of the known directions.
func (d Direction) IsValid() bool {
	switch d {
	case DirBullish, DirBearish, DirNeutral, DirBuy, DirAvoid, DirSkip, DirWait:
		return true
	}
	return false
}

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

// IsAdvisor reports whether the outcome belongs to the
// /advisor surface (no fund scope, master/tactic kind).
// Advisor rows are persisted with fund_id IS NULL.
func (o Outcome) IsAdvisor() bool {
	return o.AgentKind == KindMaster || o.AgentKind == KindTactic
}

// Validate enforces the must-have fields.
func (o Outcome) Validate() error {
	if !o.IsAdvisor() && strings.TrimSpace(o.FundID) == "" {
		return errors.New("agentreputation: outcome.FundID required")
	}
	if o.IsAdvisor() && strings.TrimSpace(o.FundID) != "" {
		return errors.New("agentreputation: advisor outcomes must not carry FundID")
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

	const qFund = `INSERT INTO agent_reputation_outcomes
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

	// Advisor rows go through a separate ON CONFLICT path that
	// targets the partial unique index uq_agent_reputation_outcomes_advisor
	// (created in migration 099). Postgres requires the WHERE
	// predicate to match the partial index so the planner picks it.
	const qAdvisor = `INSERT INTO agent_reputation_outcomes
		(fund_id, agent_id, agent_name, agent_kind, category, symbol, asof,
		 direction, confidence, realised_return, benchmark_return, alpha,
		 horizon_days, source_panel_id, source_debate_id, note)
		VALUES (NULL, $1, $2, $3, $4, $5, $6,
		        $7, $8, $9, $10, $11,
		        $12, $13, $14, $15)
		ON CONFLICT (agent_id, symbol, asof, horizon_days)
		WHERE fund_id IS NULL
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

	// Prepare lazily so a batch containing only fund-scoped rows
	// (the dominant case for the legacy backfill) doesn't pay the
	// cost of preparing the advisor-path statement and existing
	// tests that ExpectPrepare("INSERT ...") exactly once still
	// pass without modification.
	var stmtFund, stmtAdvisor *sql.Stmt
	defer func() {
		if stmtFund != nil {
			_ = stmtFund.Close()
		}
		if stmtAdvisor != nil {
			_ = stmtAdvisor.Close()
		}
	}()

	for _, o := range outs {
		if o.IsAdvisor() {
			if stmtAdvisor == nil {
				prep, prepErr := tx.PrepareContext(ctx, qAdvisor)
				if prepErr != nil {
					return fmt.Errorf("agentreputation: prepare advisor upsert: %w", prepErr)
				}
				stmtAdvisor = prep
			}
			if _, err := stmtAdvisor.ExecContext(ctx,
				o.AgentID, o.AgentName, string(o.AgentKind), o.Category,
				strings.ToUpper(o.Symbol), o.AsOf,
				string(o.Direction), o.Confidence, o.RealisedReturn, o.BenchmarkReturn, o.Alpha,
				o.HorizonDays, o.SourcePanelID, o.SourceDebateID, o.Note,
			); err != nil {
				return fmt.Errorf("agentreputation: upsert advisor outcome (%s/%s): %w", o.AgentID, o.Symbol, err)
			}
			continue
		}
		if stmtFund == nil {
			prep, prepErr := tx.PrepareContext(ctx, qFund)
			if prepErr != nil {
				return fmt.Errorf("agentreputation: prepare fund upsert: %w", prepErr)
			}
			stmtFund = prep
		}
		if _, err := stmtFund.ExecContext(ctx,
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
//
// The hit / miss CASE expression covers six direction values:
//
//	bullish (legacy)  -> hit when realised_return > 0
//	bearish (legacy)  -> hit when realised_return < 0
//	buy     (advisor) -> hit when realised_return > 0
//	avoid   (advisor) -> hit when realised_return < 0
//	skip / wait / neutral -> never counted as hit or miss
func (r *Repo) RecomputeStats(ctx context.Context, fundID string) error {
	if r == nil || r.db == nil {
		return errors.New("agentreputation: repo not initialised")
	}
	const statsSelect = `SELECT fund_id, agent_id,
		       MAX(agent_name)             AS agent_name,
		       MAX(agent_kind)             AS agent_kind,
		       MAX(category)               AS category,
		       COUNT(*)                    AS decisions_count,
		       SUM(CASE WHEN (direction IN ('bullish','buy')   AND realised_return > 0)
		                  OR (direction IN ('bearish','avoid') AND realised_return < 0)
		                THEN 1 ELSE 0 END) AS hits_count,
		       SUM(CASE WHEN (direction IN ('bullish','buy')   AND realised_return < 0)
		                  OR (direction IN ('bearish','avoid') AND realised_return > 0)
		                THEN 1 ELSE 0 END) AS misses_count,
		       AVG(alpha)                  AS avg_alpha,
		       SUM(alpha)                  AS sum_alpha,
		       AVG(confidence)             AS avg_confidence,
		       MAX(asof)                   AS last_decision_at,
		       now()                       AS updated_at`

	// Fund-scoped recompute: ON CONFLICT targets uq_agent_reputation_stats_fund.
	const qFund = `INSERT INTO agent_reputation_stats AS s
		(fund_id, agent_id, agent_name, agent_kind, category,
		 decisions_count, hits_count, misses_count,
		 avg_alpha, sum_alpha, avg_confidence, last_decision_at, updated_at)
		` + statsSelect + `
		  FROM agent_reputation_outcomes
		 WHERE fund_id IS NOT NULL
		   AND ($1::uuid IS NULL OR fund_id = $1::uuid)
		 GROUP BY fund_id, agent_id
		ON CONFLICT (fund_id, agent_id) WHERE fund_id IS NOT NULL
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
	if _, err := r.db.ExecContext(ctx, qFund, arg); err != nil {
		return fmt.Errorf("agentreputation: recompute stats (fund): %w", err)
	}

	// Advisor recompute only runs when no fund filter was passed,
	// since advisor rows have fund_id IS NULL and a fund-id filter
	// would always exclude them.
	if strings.TrimSpace(fundID) != "" {
		return nil
	}
	const qAdvisor = `INSERT INTO agent_reputation_stats AS s
		(fund_id, agent_id, agent_name, agent_kind, category,
		 decisions_count, hits_count, misses_count,
		 avg_alpha, sum_alpha, avg_confidence, last_decision_at, updated_at)
		` + statsSelect + `
		  FROM agent_reputation_outcomes
		 WHERE fund_id IS NULL
		 GROUP BY fund_id, agent_id
		ON CONFLICT (agent_id) WHERE fund_id IS NULL
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
		              updated_at = now()`
	if _, err := r.db.ExecContext(ctx, qAdvisor); err != nil {
		return fmt.Errorf("agentreputation: recompute stats (advisor): %w", err)
	}
	return nil
}

// --- Reads ------------------------------------------------------------------

// ListStatsParams filters the stats listing.
type ListStatsParams struct {
	FundID string
	// AdvisorOnly forces fund_id IS NULL — used by the public
	// advisor track-record panel. Mutually exclusive with FundID;
	// when both are set FundID wins.
	AdvisorOnly bool
	AgentKind   AgentKind
	Limit       int
}

// ListStats returns rolling per-agent stats ordered by avg_alpha
// DESC (best first). Filterable by fund + kind.
func (r *Repo) ListStats(ctx context.Context, p ListStatsParams) ([]Stats, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("agentreputation: repo not initialised")
	}
	conds := []string{}
	args := []interface{}{}
	switch {
	case strings.TrimSpace(p.FundID) != "":
		args = append(args, p.FundID)
		conds = append(conds, fmt.Sprintf("fund_id = $%d", len(args)))
	case p.AdvisorOnly:
		conds = append(conds, "fund_id IS NULL")
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
	FundID string
	// AdvisorOnly forces fund_id IS NULL — used by the public
	// advisor surface. Mutually exclusive with FundID.
	AdvisorOnly bool
	AgentID     string
	Symbol      string
	Limit       int
}

// ListOutcomes returns recent outcomes (latest first).
func (r *Repo) ListOutcomes(ctx context.Context, p ListOutcomesParams) ([]Outcome, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("agentreputation: repo not initialised")
	}
	conds := []string{}
	args := []interface{}{}
	switch {
	case strings.TrimSpace(p.FundID) != "":
		args = append(args, p.FundID)
		conds = append(conds, fmt.Sprintf("fund_id = $%d", len(args)))
	case p.AdvisorOnly:
		conds = append(conds, "fund_id IS NULL")
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

// GetAdvisorStats fetches a single advisor-mode (fund_id IS NULL,
// agent_id) summary row. agentID should already carry the
// "master:" or "tactic:" prefix.
func (r *Repo) GetAdvisorStats(ctx context.Context, agentID string) (Stats, error) {
	if r == nil || r.db == nil {
		return Stats{}, errors.New("agentreputation: repo not initialised")
	}
	const q = `SELECT fund_id, agent_id, agent_name, agent_kind, category,
	                  decisions_count, hits_count, misses_count,
	                  avg_alpha, sum_alpha, avg_confidence,
	                  last_decision_at, updated_at
	             FROM agent_reputation_stats
	            WHERE fund_id IS NULL AND agent_id = $1`
	rows, err := r.db.QueryContext(ctx, q, agentID)
	if err != nil {
		return Stats{}, fmt.Errorf("agentreputation: get advisor stats: %w", err)
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
		var (
			kind   string
			fundID sql.NullString
		)
		if err := rows.Scan(
			&fundID, &s.AgentID, &s.AgentName, &kind, &s.Category,
			&s.DecisionsCount, &s.HitsCount, &s.MissesCount,
			&s.AvgAlpha, &s.SumAlpha, &s.AvgConfidence,
			&s.LastDecisionAt, &s.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("agentreputation: scan stats: %w", err)
		}
		s.FundID = fundID.String
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
		var (
			kind, dir string
			fundID    sql.NullString
		)
		if err := rows.Scan(
			&o.ID, &fundID, &o.AgentID, &o.AgentName, &kind, &o.Category, &o.Symbol, &o.AsOf,
			&dir, &o.Confidence, &o.RealisedReturn, &o.BenchmarkReturn, &o.Alpha,
			&o.HorizonDays, &o.SourcePanelID, &o.SourceDebateID, &o.Note, &o.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("agentreputation: scan outcome: %w", err)
		}
		o.FundID = fundID.String
		o.AgentKind = AgentKind(kind)
		o.Direction = Direction(dir)
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agentreputation: outcome rows: %w", err)
	}
	return out, nil
}
