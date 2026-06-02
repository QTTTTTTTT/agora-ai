// Package analystreport persists S8.1 PanelReports (and the
// per-category analyst reports inside them) to Postgres.
//
// Layered so internal/agent stays pure-Go domain logic. The
// repo lives here, accepts already-validated agent.PanelReport
// values, and exposes read methods scoped by fund/symbol/agent.
package analystreport

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fundai/server/internal/agent"
)

// ErrNotFound is returned when a single-row read finds no rows.
var ErrNotFound = errors.New("analystreport: not found")

// Repo is the persistence façade. Construct with NewRepo(db).
type Repo struct {
	db *sql.DB
}

// NewRepo wires the repo to a *sql.DB.
func NewRepo(db *sql.DB) *Repo { return &Repo{db: db} }

// PanelRow is the read shape returned to handlers / UI. It is a
// view of analyst_panel_reports joined with all its child
// analyst_reports rows so a single call to GetPanel returns the
// complete picture.
type PanelRow struct {
	ID              string
	FundID          string
	Symbol          string
	AsOf            time.Time
	GeneratedAt     time.Time
	AggDirection    string
	AggConfidence   int
	CategoriesVoted int
	PerCategoryVote map[string]int
	Reports         []ReportRow
	CreatedAt       time.Time
}

// ReportRow is one row in analyst_reports — the per-category
// analyst output, including the JSONB-serialised lists.
type ReportRow struct {
	ID               string
	PanelID          string
	FundID           string
	AgentID          string
	AgentName        string
	Category         string
	Symbol           string
	AsOf             time.Time
	GeneratedAt      time.Time
	Direction        string
	Confidence       int
	Thesis           string
	KeyFindings      []string
	Risks            []string
	DataPoints       []DataPointWire
	Sources          []string
	PromptTokens     int
	CompletionTokens int
	LLMModel         string
	CreatedAt        time.Time
}

// DataPointWire mirrors agent.DataPoint as a JSON-safe shape.
type DataPointWire struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Source string `json:"source,omitempty"`
}

// --- Writes -----------------------------------------------------------------

// SavePanel persists a PanelReport + every report it contains as
// one transaction. Returns the newly-allocated panel ID. Callers
// should already have called PanelReport.Validate().
func (r *Repo) SavePanel(ctx context.Context, p agent.PanelReport) (string, error) {
	if r == nil || r.db == nil {
		return "", errors.New("analystreport: repo not initialised")
	}
	if err := p.Validate(); err != nil {
		return "", err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("analystreport: begin tx: %w", err)
	}
	defer func() {
		// Best-effort rollback if we returned early.
		_ = tx.Rollback()
	}()

	votesJSON, err := json.Marshal(p.Aggregate.PerCategoryVotes)
	if err != nil {
		return "", fmt.Errorf("analystreport: marshal votes: %w", err)
	}

	panelID := ""
	const panelSQL = `INSERT INTO analyst_panel_reports
		(fund_id, symbol, asof, generated_at,
		 aggregate_direction, aggregate_confidence, categories_voted, per_category_votes)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb)
		RETURNING id`
	if err := tx.QueryRowContext(ctx, panelSQL,
		p.FundID, p.Symbol, p.AsOf, p.GeneratedAt,
		string(p.Aggregate.Direction), p.Aggregate.Confidence, p.Aggregate.CategoriesVoted, string(votesJSON),
	).Scan(&panelID); err != nil {
		return "", fmt.Errorf("analystreport: insert panel: %w", err)
	}

	const reportSQL = `INSERT INTO analyst_reports
		(panel_id, fund_id, agent_id, agent_name, category, symbol, asof, generated_at,
		 direction, confidence, thesis,
		 key_findings, risks, data_points, sources,
		 prompt_tokens, completion_tokens, llm_model)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
		        $9, $10, $11,
		        $12::jsonb, $13::jsonb, $14::jsonb, $15::jsonb,
		        $16, $17, $18)`

	stmt, err := tx.PrepareContext(ctx, reportSQL)
	if err != nil {
		return "", fmt.Errorf("analystreport: prepare report insert: %w", err)
	}
	defer stmt.Close()

	for cat, rep := range p.Reports {
		findingsJSON, _ := json.Marshal(rep.KeyFindings)
		risksJSON, _ := json.Marshal(rep.Risks)
		dpWire := make([]DataPointWire, len(rep.DataPoints))
		for i, dp := range rep.DataPoints {
			dpWire[i] = DataPointWire{Name: dp.Name, Value: dp.Value, Source: dp.Source}
		}
		dpJSON, _ := json.Marshal(dpWire)
		srcJSON, _ := json.Marshal(rep.Sources)
		if _, err := stmt.ExecContext(ctx,
			panelID, p.FundID, rep.AgentID, rep.AgentName, string(cat),
			rep.Symbol, rep.AsOf, rep.GeneratedAt,
			string(rep.Direction), rep.Confidence, rep.Thesis,
			string(findingsJSON), string(risksJSON), string(dpJSON), string(srcJSON),
			rep.PromptTokens, rep.CompletionTokens, rep.LLMModel,
		); err != nil {
			return "", fmt.Errorf("analystreport: insert report %q: %w", cat, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("analystreport: commit tx: %w", err)
	}
	return panelID, nil
}

// --- Reads ------------------------------------------------------------------

// ListPanelsParams filters the panel listing. Both AsOf bounds
// are inclusive; zero-value times mean "no bound".
type ListPanelsParams struct {
	FundID     string
	Symbol     string
	AsOfFrom   time.Time
	AsOfTo     time.Time
	Limit      int
	IncludeChildren bool
}

// ListPanels returns panel summaries ordered by asof DESC. When
// IncludeChildren is true each row also carries its
// analyst_reports rows in PanelRow.Reports.
func (r *Repo) ListPanels(ctx context.Context, p ListPanelsParams) ([]PanelRow, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("analystreport: repo not initialised")
	}
	conds := []string{}
	args := []interface{}{}
	if strings.TrimSpace(p.FundID) != "" {
		args = append(args, p.FundID)
		conds = append(conds, fmt.Sprintf("fund_id = $%d", len(args)))
	}
	if strings.TrimSpace(p.Symbol) != "" {
		args = append(args, strings.ToUpper(p.Symbol))
		conds = append(conds, fmt.Sprintf("symbol = $%d", len(args)))
	}
	if !p.AsOfFrom.IsZero() {
		args = append(args, p.AsOfFrom)
		conds = append(conds, fmt.Sprintf("asof >= $%d", len(args)))
	}
	if !p.AsOfTo.IsZero() {
		args = append(args, p.AsOfTo)
		conds = append(conds, fmt.Sprintf("asof <= $%d", len(args)))
	}

	q := `SELECT id, fund_id, symbol, asof, generated_at,
	             aggregate_direction, aggregate_confidence, categories_voted, per_category_votes, created_at
	        FROM analyst_panel_reports`
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY asof DESC, generated_at DESC"
	limit := p.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args = append(args, limit)
	q += fmt.Sprintf(" LIMIT $%d", len(args))

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("analystreport: list panels: %w", err)
	}
	defer rows.Close()

	var out []PanelRow
	for rows.Next() {
		var row PanelRow
		var votesRaw []byte
		if err := rows.Scan(
			&row.ID, &row.FundID, &row.Symbol, &row.AsOf, &row.GeneratedAt,
			&row.AggDirection, &row.AggConfidence, &row.CategoriesVoted, &votesRaw, &row.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("analystreport: scan panel row: %w", err)
		}
		row.PerCategoryVote = map[string]int{}
		if len(votesRaw) > 0 {
			_ = json.Unmarshal(votesRaw, &row.PerCategoryVote)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("analystreport: list panels rows: %w", err)
	}
	if p.IncludeChildren && len(out) > 0 {
		ids := make([]string, len(out))
		for i := range out {
			ids[i] = out[i].ID
		}
		children, err := r.fetchReportsForPanels(ctx, ids)
		if err != nil {
			return nil, err
		}
		for i := range out {
			out[i].Reports = children[out[i].ID]
		}
	}
	return out, nil
}

// GetPanel returns one panel + its children by id. Returns
// ErrNotFound when no such row exists.
func (r *Repo) GetPanel(ctx context.Context, id string) (PanelRow, error) {
	if r == nil || r.db == nil {
		return PanelRow{}, errors.New("analystreport: repo not initialised")
	}
	const q = `SELECT id, fund_id, symbol, asof, generated_at,
	                  aggregate_direction, aggregate_confidence, categories_voted, per_category_votes, created_at
	             FROM analyst_panel_reports
	            WHERE id = $1`
	var row PanelRow
	var votesRaw []byte
	err := r.db.QueryRowContext(ctx, q, id).Scan(
		&row.ID, &row.FundID, &row.Symbol, &row.AsOf, &row.GeneratedAt,
		&row.AggDirection, &row.AggConfidence, &row.CategoriesVoted, &votesRaw, &row.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return PanelRow{}, ErrNotFound
	}
	if err != nil {
		return PanelRow{}, fmt.Errorf("analystreport: get panel: %w", err)
	}
	row.PerCategoryVote = map[string]int{}
	if len(votesRaw) > 0 {
		_ = json.Unmarshal(votesRaw, &row.PerCategoryVote)
	}
	children, err := r.fetchReportsForPanels(ctx, []string{row.ID})
	if err != nil {
		return PanelRow{}, err
	}
	row.Reports = children[row.ID]
	return row, nil
}

// ListReportsByAgent returns the most-recent N reports authored
// by a specific agent_id. Used by S8.4 reputation calibration.
func (r *Repo) ListReportsByAgent(ctx context.Context, agentID string, limit int) ([]ReportRow, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("analystreport: repo not initialised")
	}
	if strings.TrimSpace(agentID) == "" {
		return nil, errors.New("analystreport: agentID required")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	const q = `SELECT id, panel_id, fund_id, agent_id, agent_name, category, symbol, asof, generated_at,
	                  direction, confidence, thesis, key_findings, risks, data_points, sources,
	                  prompt_tokens, completion_tokens, llm_model, created_at
	             FROM analyst_reports
	            WHERE agent_id = $1
	            ORDER BY asof DESC, generated_at DESC
	            LIMIT $2`
	rows, err := r.db.QueryContext(ctx, q, agentID, limit)
	if err != nil {
		return nil, fmt.Errorf("analystreport: list reports by agent: %w", err)
	}
	defer rows.Close()
	return scanReportRows(rows)
}

// --- private helpers --------------------------------------------------------

func (r *Repo) fetchReportsForPanels(ctx context.Context, panelIDs []string) (map[string][]ReportRow, error) {
	if len(panelIDs) == 0 {
		return map[string][]ReportRow{}, nil
	}
	placeholders := make([]string, len(panelIDs))
	args := make([]interface{}, len(panelIDs))
	for i, id := range panelIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	q := fmt.Sprintf(`SELECT id, panel_id, fund_id, agent_id, agent_name, category, symbol, asof, generated_at,
	                          direction, confidence, thesis, key_findings, risks, data_points, sources,
	                          prompt_tokens, completion_tokens, llm_model, created_at
	                     FROM analyst_reports
	                    WHERE panel_id IN (%s)
	                    ORDER BY panel_id, category`, strings.Join(placeholders, ","))
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("analystreport: fetch children: %w", err)
	}
	defer rows.Close()
	all, err := scanReportRows(rows)
	if err != nil {
		return nil, err
	}
	out := map[string][]ReportRow{}
	for _, r := range all {
		out[r.PanelID] = append(out[r.PanelID], r)
	}
	return out, nil
}

func scanReportRows(rows *sql.Rows) ([]ReportRow, error) {
	var out []ReportRow
	for rows.Next() {
		var rr ReportRow
		var findingsRaw, risksRaw, dpRaw, srcRaw []byte
		if err := rows.Scan(
			&rr.ID, &rr.PanelID, &rr.FundID, &rr.AgentID, &rr.AgentName, &rr.Category,
			&rr.Symbol, &rr.AsOf, &rr.GeneratedAt,
			&rr.Direction, &rr.Confidence, &rr.Thesis,
			&findingsRaw, &risksRaw, &dpRaw, &srcRaw,
			&rr.PromptTokens, &rr.CompletionTokens, &rr.LLMModel, &rr.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("analystreport: scan report: %w", err)
		}
		_ = json.Unmarshal(findingsRaw, &rr.KeyFindings)
		_ = json.Unmarshal(risksRaw, &rr.Risks)
		_ = json.Unmarshal(dpRaw, &rr.DataPoints)
		_ = json.Unmarshal(srcRaw, &rr.Sources)
		out = append(out, rr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("analystreport: scan rows: %w", err)
	}
	return out, nil
}
