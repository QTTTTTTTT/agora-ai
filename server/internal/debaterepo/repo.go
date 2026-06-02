// Package debaterepo persists S8.2 Bull/Bear debate transcripts
// + per-round arguments to Postgres. Symmetric to analystreport
// — internal/agent stays pure-Go domain logic; this package
// owns the SQL.
package debaterepo

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
var ErrNotFound = errors.New("debaterepo: not found")

// Repo is the persistence façade.
type Repo struct {
	db *sql.DB
}

// NewRepo wires the repo to a *sql.DB.
func NewRepo(db *sql.DB) *Repo { return &Repo{db: db} }

// TranscriptRow is the read shape — transcript row + its child
// argument rows when fetched via GetTranscript.
type TranscriptRow struct {
	ID                    string
	FundID                string
	PanelID               string
	Symbol                string
	AsOf                  time.Time
	GeneratedAt           time.Time
	VerdictDirection      string
	VerdictConfidence     int
	VerdictWinner         string
	VerdictBullConfidence int
	VerdictBearConfidence int
	VerdictContested      bool
	VerdictWinningSummary string
	VerdictLosingSummary  string
	CreatedAt             time.Time
	Arguments             []ArgumentRow
}

// ArgumentRow is one debate_arguments row.
type ArgumentRow struct {
	ID            string
	TranscriptID  string
	FundID        string
	AgentID       string
	AgentName     string
	Stance        string
	Symbol        string
	RoundNumber   int
	AsOf          time.Time
	GeneratedAt   time.Time
	Direction     string
	Confidence    int
	Thesis        string
	SupportPoints []string
	Rebuttals     []string
	CitedReports  []string
	LLMModel      string
	CreatedAt     time.Time
}

// --- Writes -----------------------------------------------------------------

// SaveTranscript persists a transcript + every argument inside
// it as one transaction. panelID is the analyst_panel_reports.id
// that fed the debate.
func (r *Repo) SaveTranscript(ctx context.Context, panelID string, t agent.DebateTranscript) (string, error) {
	if r == nil || r.db == nil {
		return "", errors.New("debaterepo: repo not initialised")
	}
	if strings.TrimSpace(panelID) == "" {
		return "", errors.New("debaterepo: panelID required")
	}
	if err := t.Validate(); err != nil {
		return "", err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("debaterepo: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	transcriptID := ""
	const tsSQL = `INSERT INTO debate_transcripts
		(fund_id, panel_id, symbol, asof, generated_at,
		 verdict_direction, verdict_confidence, verdict_winner,
		 verdict_bull_confidence, verdict_bear_confidence,
		 verdict_contested, verdict_winning_summary, verdict_losing_summary)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id`
	if err := tx.QueryRowContext(ctx, tsSQL,
		t.FundID, panelID, t.Symbol, t.AsOf, t.GeneratedAt,
		string(t.Verdict.Direction), t.Verdict.Confidence, string(t.Verdict.WinnerStance),
		t.Verdict.BullConfidence, t.Verdict.BearConfidence,
		t.Verdict.Contested, t.Verdict.WinningSummary, t.Verdict.LosingSummary,
	).Scan(&transcriptID); err != nil {
		return "", fmt.Errorf("debaterepo: insert transcript: %w", err)
	}

	const argSQL = `INSERT INTO debate_arguments
		(transcript_id, fund_id, agent_id, agent_name, stance, symbol,
		 round_number, asof, generated_at, direction, confidence, thesis,
		 support_points, rebuttals, cited_reports, llm_model)
		VALUES ($1, $2, $3, $4, $5, $6,
		        $7, $8, $9, $10, $11, $12,
		        $13::jsonb, $14::jsonb, $15::jsonb, $16)`
	stmt, err := tx.PrepareContext(ctx, argSQL)
	if err != nil {
		return "", fmt.Errorf("debaterepo: prepare argument insert: %w", err)
	}
	defer stmt.Close()

	for _, a := range t.Arguments {
		spJSON, _ := json.Marshal(a.SupportPoints)
		rbJSON, _ := json.Marshal(a.Rebuttals)
		cited := make([]string, len(a.CitedReports))
		for i, c := range a.CitedReports {
			cited[i] = string(c)
		}
		ctJSON, _ := json.Marshal(cited)
		if _, err := stmt.ExecContext(ctx,
			transcriptID, t.FundID, a.AgentID, a.AgentName, string(a.Stance),
			a.Symbol, a.Round, a.AsOf, a.GeneratedAt,
			string(a.Direction), a.Confidence, a.Thesis,
			string(spJSON), string(rbJSON), string(ctJSON), a.LLMModel,
		); err != nil {
			return "", fmt.Errorf("debaterepo: insert argument (round %d, %s): %w", a.Round, a.Stance, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("debaterepo: commit tx: %w", err)
	}
	return transcriptID, nil
}

// --- Reads ------------------------------------------------------------------

// ListTranscriptsParams filters the transcript listing.
type ListTranscriptsParams struct {
	FundID   string
	Symbol   string
	AsOfFrom time.Time
	AsOfTo   time.Time
	Limit    int
}

// ListTranscripts returns transcript summaries (no children)
// ordered by asof DESC.
func (r *Repo) ListTranscripts(ctx context.Context, p ListTranscriptsParams) ([]TranscriptRow, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("debaterepo: repo not initialised")
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
	q := `SELECT id, fund_id, panel_id, symbol, asof, generated_at,
	             verdict_direction, verdict_confidence, verdict_winner,
	             verdict_bull_confidence, verdict_bear_confidence,
	             verdict_contested, verdict_winning_summary, verdict_losing_summary,
	             created_at
	        FROM debate_transcripts`
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
		return nil, fmt.Errorf("debaterepo: list transcripts: %w", err)
	}
	defer rows.Close()
	return scanTranscriptRows(rows)
}

// GetTranscript fetches one transcript + its child arguments.
func (r *Repo) GetTranscript(ctx context.Context, id string) (TranscriptRow, error) {
	if r == nil || r.db == nil {
		return TranscriptRow{}, errors.New("debaterepo: repo not initialised")
	}
	const q = `SELECT id, fund_id, panel_id, symbol, asof, generated_at,
	                  verdict_direction, verdict_confidence, verdict_winner,
	                  verdict_bull_confidence, verdict_bear_confidence,
	                  verdict_contested, verdict_winning_summary, verdict_losing_summary,
	                  created_at
	             FROM debate_transcripts
	            WHERE id = $1`
	rows, err := r.db.QueryContext(ctx, q, id)
	if err != nil {
		return TranscriptRow{}, fmt.Errorf("debaterepo: get transcript: %w", err)
	}
	defer rows.Close()
	parents, err := scanTranscriptRows(rows)
	if err != nil {
		return TranscriptRow{}, err
	}
	if len(parents) == 0 {
		return TranscriptRow{}, ErrNotFound
	}
	t := parents[0]
	children, err := r.fetchArgumentsForTranscripts(ctx, []string{t.ID})
	if err != nil {
		return TranscriptRow{}, err
	}
	t.Arguments = children[t.ID]
	return t, nil
}

// GetTranscriptByPanel returns the latest transcript anchored to
// a given panel id. Useful for the panel-detail UI.
func (r *Repo) GetTranscriptByPanel(ctx context.Context, panelID string) (TranscriptRow, error) {
	if r == nil || r.db == nil {
		return TranscriptRow{}, errors.New("debaterepo: repo not initialised")
	}
	if strings.TrimSpace(panelID) == "" {
		return TranscriptRow{}, errors.New("debaterepo: panelID required")
	}
	const q = `SELECT id, fund_id, panel_id, symbol, asof, generated_at,
	                  verdict_direction, verdict_confidence, verdict_winner,
	                  verdict_bull_confidence, verdict_bear_confidence,
	                  verdict_contested, verdict_winning_summary, verdict_losing_summary,
	                  created_at
	             FROM debate_transcripts
	            WHERE panel_id = $1
	            ORDER BY generated_at DESC
	            LIMIT 1`
	rows, err := r.db.QueryContext(ctx, q, panelID)
	if err != nil {
		return TranscriptRow{}, fmt.Errorf("debaterepo: get by panel: %w", err)
	}
	defer rows.Close()
	parents, err := scanTranscriptRows(rows)
	if err != nil {
		return TranscriptRow{}, err
	}
	if len(parents) == 0 {
		return TranscriptRow{}, ErrNotFound
	}
	t := parents[0]
	children, err := r.fetchArgumentsForTranscripts(ctx, []string{t.ID})
	if err != nil {
		return TranscriptRow{}, err
	}
	t.Arguments = children[t.ID]
	return t, nil
}

// --- helpers ----------------------------------------------------------------

func (r *Repo) fetchArgumentsForTranscripts(ctx context.Context, ids []string) (map[string][]ArgumentRow, error) {
	if len(ids) == 0 {
		return map[string][]ArgumentRow{}, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	q := fmt.Sprintf(`SELECT id, transcript_id, fund_id, agent_id, agent_name, stance,
	                          symbol, round_number, asof, generated_at,
	                          direction, confidence, thesis,
	                          support_points, rebuttals, cited_reports, llm_model, created_at
	                     FROM debate_arguments
	                    WHERE transcript_id IN (%s)
	                    ORDER BY transcript_id, round_number, stance`, strings.Join(placeholders, ","))
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("debaterepo: fetch arguments: %w", err)
	}
	defer rows.Close()
	all, err := scanArgumentRows(rows)
	if err != nil {
		return nil, err
	}
	out := map[string][]ArgumentRow{}
	for _, a := range all {
		out[a.TranscriptID] = append(out[a.TranscriptID], a)
	}
	return out, nil
}

func scanTranscriptRows(rows *sql.Rows) ([]TranscriptRow, error) {
	var out []TranscriptRow
	for rows.Next() {
		var r TranscriptRow
		if err := rows.Scan(
			&r.ID, &r.FundID, &r.PanelID, &r.Symbol, &r.AsOf, &r.GeneratedAt,
			&r.VerdictDirection, &r.VerdictConfidence, &r.VerdictWinner,
			&r.VerdictBullConfidence, &r.VerdictBearConfidence,
			&r.VerdictContested, &r.VerdictWinningSummary, &r.VerdictLosingSummary,
			&r.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("debaterepo: scan transcript row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("debaterepo: scan transcripts: %w", err)
	}
	return out, nil
}

func scanArgumentRows(rows *sql.Rows) ([]ArgumentRow, error) {
	var out []ArgumentRow
	for rows.Next() {
		var a ArgumentRow
		var spRaw, rbRaw, ctRaw []byte
		if err := rows.Scan(
			&a.ID, &a.TranscriptID, &a.FundID, &a.AgentID, &a.AgentName, &a.Stance,
			&a.Symbol, &a.RoundNumber, &a.AsOf, &a.GeneratedAt,
			&a.Direction, &a.Confidence, &a.Thesis,
			&spRaw, &rbRaw, &ctRaw, &a.LLMModel, &a.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("debaterepo: scan argument row: %w", err)
		}
		_ = json.Unmarshal(spRaw, &a.SupportPoints)
		_ = json.Unmarshal(rbRaw, &a.Rebuttals)
		_ = json.Unmarshal(ctRaw, &a.CitedReports)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("debaterepo: scan arguments: %w", err)
	}
	return out, nil
}
