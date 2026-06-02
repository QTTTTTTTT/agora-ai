// Package alphalesson is the S9.1 alpha-aware memory layer.
//
// Two responsibilities:
//
//   1. WriteAlphaLessons — given a batch of newly-settled
//      agentreputation.Outcomes, write a long-term memory row
//      for each one whose |alpha| crosses a threshold. The
//      memory row carries the agent_id, the realised alpha and
//      a one-line lesson the PM can read on a future round.
//
//   2. BuildAlphaContext — given a fund_id, render a markdown
//      block the PM prompt can splice into the existing
//      MemoryContext seam. Two parts:
//        - "Agent Track Record" leaderboard (top + bottom
//          agents by avg α from agent_reputation_stats).
//        - "Alpha-tagged lessons" — the K most-recent
//          alpha-tagged memory rows for this fund.
//
// The package depends on agentreputation (read-only) and uses
// the existing memories table (extended in migration 074). It
// is deliberately decoupled from agent / pm so the PM team can
// integrate the context-builder by string-concat without
// pulling new behavioural deps.
package alphalesson

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/fundai/server/internal/agentreputation"
)

// ErrNotFound mirrors the rest of the package conventions.
var ErrNotFound = errors.New("alphalesson: not found")

// LessonRow is the projection of an alpha-tagged memory row.
type LessonRow struct {
	ID              string
	FundID          string
	AgentTag        string
	Content         string
	Title           sql.NullString
	AlphaVsBench    sql.NullFloat64
	SourceOutcomeID sql.NullString
	TradingDate     sql.NullTime
	CreatedAt       time.Time
}

// Repo persists and reads alpha-tagged memories.
type Repo struct {
	db *sql.DB
}

// NewRepo wires the repo.
func NewRepo(db *sql.DB) *Repo { return &Repo{db: db} }

// --- Writes ---------------------------------------------------------------

// WriteOptions tunes WriteAlphaLessons.
type WriteOptions struct {
	// AlphaThreshold is the minimum |alpha| required to mint a
	// memory row. Defaults to 0.01 (1 % realised alpha).
	AlphaThreshold float64
	// Layer is the memory layer to write into. Defaults to
	// "long_term".
	Layer string
	// Visibility is the memories.visibility value. Defaults to
	// "fund" (every agent in the fund team can read it).
	Visibility string
	// Sensitivity defaults to "internal".
	Sensitivity string
	// OriginKind defaults to "alpha_lesson".
	OriginKind string
}

func (o WriteOptions) normalize() WriteOptions {
	if o.AlphaThreshold <= 0 {
		o.AlphaThreshold = 0.01
	}
	if strings.TrimSpace(o.Layer) == "" {
		o.Layer = "long_term"
	}
	if strings.TrimSpace(o.Visibility) == "" {
		o.Visibility = "fund"
	}
	if strings.TrimSpace(o.Sensitivity) == "" {
		o.Sensitivity = "internal"
	}
	if strings.TrimSpace(o.OriginKind) == "" {
		o.OriginKind = "alpha_lesson"
	}
	return o
}

// WriteAlphaLessonsForOutcomes satisfies the
// agentreputation.LessonWriter contract — uses default
// WriteOptions so the backfill driver can call it without
// knowing about thresholds.
func (r *Repo) WriteAlphaLessonsForOutcomes(ctx context.Context, outcomes []agentreputation.Outcome) (int, error) {
	return r.WriteAlphaLessons(ctx, outcomes, WriteOptions{})
}

// WriteAlphaLessons mints a long-term memory row for every
// outcome whose |alpha| ≥ threshold. Returns the number of rows
// written. Idempotent on (source_outcome_id) — re-running the
// backfill won't duplicate lessons.
func (r *Repo) WriteAlphaLessons(ctx context.Context, outcomes []agentreputation.Outcome, opts WriteOptions) (int, error) {
	if r == nil || r.db == nil {
		return 0, errors.New("alphalesson: repo not initialised")
	}
	if len(outcomes) == 0 {
		return 0, nil
	}
	opts = opts.normalize()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("alphalesson: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Skip outcomes that already produced a lesson — keyed by
	// source_outcome_id. Doing this in a single round-trip would
	// require a temp table; the per-row check is cheap because
	// the backfill batch size is bounded.
	const checkSQL = `SELECT 1 FROM memories WHERE source_outcome_id = $1 LIMIT 1`
	const insertSQL = `INSERT INTO memories
		(fund_id, layer, visibility, sensitivity, origin_kind,
		 title, content, trading_date, tags,
		 agent_tag, alpha_vs_benchmark, source_outcome_id)
		VALUES ($1, $2, $3, $4, $5,
		        $6, $7, $8, $9,
		        $10, $11, $12)`
	insertStmt, err := tx.PrepareContext(ctx, insertSQL)
	if err != nil {
		return 0, fmt.Errorf("alphalesson: prepare insert: %w", err)
	}
	defer insertStmt.Close()

	count := 0
	for _, o := range outcomes {
		if math.Abs(o.Alpha) < opts.AlphaThreshold {
			continue
		}
		if strings.TrimSpace(o.ID) == "" {
			// Outcomes without an ID can't be deduped; treat
			// them like a one-shot write (still safe because
			// the backfill driver dedupes upstream).
		} else {
			var exists int
			err := tx.QueryRowContext(ctx, checkSQL, o.ID).Scan(&exists)
			if err == nil {
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return count, fmt.Errorf("alphalesson: dedupe lookup: %w", err)
			}
		}
		title := formatLessonTitle(o)
		content := formatLessonBody(o)
		tags := lessonTags(o)
		var tradingDate sql.NullTime
		if !o.AsOf.IsZero() {
			tradingDate = sql.NullTime{Time: o.AsOf, Valid: true}
		}
		var sourceID sql.NullString
		if strings.TrimSpace(o.ID) != "" {
			sourceID = sql.NullString{String: o.ID, Valid: true}
		}
		if _, err := insertStmt.ExecContext(ctx,
			o.FundID, opts.Layer, opts.Visibility, opts.Sensitivity, opts.OriginKind,
			sql.NullString{String: title, Valid: title != ""}, content,
			tradingDate, pq.Array(tags),
			sql.NullString{String: o.AgentID, Valid: o.AgentID != ""},
			sql.NullFloat64{Float64: o.Alpha, Valid: true},
			sourceID,
		); err != nil {
			return count, fmt.Errorf("alphalesson: insert lesson (agent=%s symbol=%s): %w", o.AgentID, o.Symbol, err)
		}
		count++
	}
	if err := tx.Commit(); err != nil {
		return count, fmt.Errorf("alphalesson: commit tx: %w", err)
	}
	return count, nil
}

// --- Reads ----------------------------------------------------------------

// ListLessonsParams filters the alpha-tagged memory listing.
type ListLessonsParams struct {
	FundID   string
	AgentTag string
	Limit    int
}

// ListLessons returns the most-recent alpha-tagged memory rows
// for a fund, optionally filtered by agent_tag.
func (r *Repo) ListLessons(ctx context.Context, p ListLessonsParams) ([]LessonRow, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("alphalesson: repo not initialised")
	}
	if strings.TrimSpace(p.FundID) == "" {
		return nil, errors.New("alphalesson: fundID required")
	}
	limit := p.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	conds := []string{"fund_id = $1", "agent_tag IS NOT NULL"}
	args := []interface{}{p.FundID}
	if strings.TrimSpace(p.AgentTag) != "" {
		args = append(args, p.AgentTag)
		conds = append(conds, fmt.Sprintf("agent_tag = $%d", len(args)))
	}
	args = append(args, limit)
	q := fmt.Sprintf(`SELECT id, fund_id, agent_tag, content, title,
	                         alpha_vs_benchmark, source_outcome_id, trading_date, created_at
	                    FROM memories
	                   WHERE %s
	                   ORDER BY created_at DESC
	                   LIMIT $%d`, strings.Join(conds, " AND "), len(args))
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("alphalesson: list lessons: %w", err)
	}
	defer rows.Close()
	var out []LessonRow
	for rows.Next() {
		var l LessonRow
		var agentTag sql.NullString
		if err := rows.Scan(
			&l.ID, &l.FundID, &agentTag, &l.Content, &l.Title,
			&l.AlphaVsBench, &l.SourceOutcomeID, &l.TradingDate, &l.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("alphalesson: scan lesson: %w", err)
		}
		if agentTag.Valid {
			l.AgentTag = agentTag.String
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("alphalesson: lesson rows: %w", err)
	}
	return out, nil
}

// --- Lesson formatters -----------------------------------------------------

func formatLessonTitle(o agentreputation.Outcome) string {
	dir := strings.ToUpper(string(o.Direction))
	verdict := "OK"
	if (o.Direction == agentreputation.DirBullish && o.RealisedReturn < 0) ||
		(o.Direction == agentreputation.DirBearish && o.RealisedReturn > 0) {
		verdict = "WRONG"
	} else if o.Alpha > 0 {
		verdict = "ALPHA"
	}
	return fmt.Sprintf("[%s][%s] %s on %s (%dd, α=%+.2f%%)",
		verdict, o.AgentName, dir, o.Symbol, o.HorizonDays, o.Alpha*100)
}

func formatLessonBody(o agentreputation.Outcome) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Agent %q (%s/%s) called %s on %s as of %s with confidence %d.\n",
		o.AgentName, o.AgentKind, o.Category, strings.ToUpper(string(o.Direction)), o.Symbol,
		o.AsOf.UTC().Format("2006-01-02"), o.Confidence)
	fmt.Fprintf(&sb, "Realised %dd return: %+.2f%% vs benchmark %+.2f%% → alpha %+.2f%%.\n",
		o.HorizonDays, o.RealisedReturn*100, o.BenchmarkReturn*100, o.Alpha*100)
	if (o.Direction == agentreputation.DirBullish && o.RealisedReturn < 0) ||
		(o.Direction == agentreputation.DirBearish && o.RealisedReturn > 0) {
		fmt.Fprintf(&sb, "Lesson: discount %s when this agent next calls %s on a similar setup.\n",
			o.AgentID, strings.ToUpper(string(o.Direction)))
	} else if o.Alpha > 0 {
		fmt.Fprintf(&sb, "Lesson: %s produced positive alpha — keep weight on this agent's %s calls.\n",
			o.AgentID, strings.ToUpper(string(o.Direction)))
	}
	if strings.TrimSpace(o.Note) != "" {
		fmt.Fprintf(&sb, "Note: %s\n", o.Note)
	}
	return sb.String()
}

func lessonTags(o agentreputation.Outcome) []string {
	tags := []string{"alpha_lesson", string(o.AgentKind), o.AgentID, strings.ToUpper(o.Symbol)}
	if o.Alpha > 0 {
		tags = append(tags, "positive_alpha")
	} else if o.Alpha < 0 {
		tags = append(tags, "negative_alpha")
	}
	if strings.TrimSpace(o.Category) != "" {
		tags = append(tags, o.Category)
	}
	return tags
}

