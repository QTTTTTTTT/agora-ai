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
	"regexp"
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
	// Visibility is the memories.visibility value. Pre-AP2 this
	// defaulted to "fund" globally. Post-AP2 the EMPTY default
	// triggers per-outcome resolution via
	// defaultVisibilityForKind(AgentKind) so researcher /
	// analyst lessons go straight to 'agent_portable' (portable
	// across funds, see migration 091). Setting Visibility
	// explicitly OVERRIDES the per-outcome resolver — useful for
	// tests / forced-fund-private backfills.
	Visibility string
	// Sensitivity defaults to "internal". A row written with
	// sensitivity='secret' is excluded from agent_portable
	// cross-fund propagation regardless of its visibility (AP7
	// reader gate enforces this — writer here just stamps the
	// requested sensitivity).
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
	// Visibility: deliberately NOT defaulted here. An empty
	// value signals "let the per-outcome resolver pick based on
	// AgentKind". See defaultVisibilityForKind below.
	if strings.TrimSpace(o.Sensitivity) == "" {
		o.Sensitivity = "internal"
	}
	if strings.TrimSpace(o.OriginKind) == "" {
		o.OriginKind = "alpha_lesson"
	}
	return o
}

// uuidPattern matches the canonical 8-4-4-4-12 hex shape. We
// use Go-side validation (rather than letting Postgres
// '$arg::uuid' implicit casts fail) so a single bad agent tag
// in a 100-row batch downgrades that ONE row to NULL rather
// than aborting the whole transaction.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// nullableUUID returns a sql.NullString that's Valid iff s
// parses as a canonical UUID. Used to populate memories.agent_id
// (UUID FK) from outcome.AgentID which is loosely-typed (some
// historical advocate / role-tag rows are non-UUID strings).
// A non-UUID input yields {Valid:false} — the DB will store
// NULL and the agent_portable read path will simply not see
// the row (correct: we can't safely propagate a row whose
// agent identity is opaque to the join key).
func nullableUUID(s string) sql.NullString {
	s = strings.TrimSpace(s)
	if s == "" {
		return sql.NullString{}
	}
	if !uuidPattern.MatchString(s) {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// defaultVisibilityForKind picks the memories.visibility value
// for a new alpha-tagged lesson when the caller did not pin one
// explicitly via WriteOptions.Visibility. The mapping:
//
//	AgentKind        Default visibility    Why
//	---------------  --------------------  ----------------------
//	researcher       agent_portable        instrument-level IP
//	                                       that follows the agent
//	analyst          agent_portable        category-level analysis
//	                                       (fundamentals, news,
//	                                       social) that's about
//	                                       the issuer, not the
//	                                       fund's portfolio
//	pm               fund                  portfolio construction
//	                                       lessons are about THIS
//	                                       fund's sleeves / risk
//	                                       limits — not portable
//	advocate         fund                  bull/bear stance is
//	                                       role-played for a fund
//	                                       context; portability
//	                                       can come later if the
//	                                       advocate track-record
//	                                       turns out to be agent
//	                                       IP in practice
//	(unknown)        fund                  conservative — when
//	                                       in doubt, keep
//	                                       lessons fund-private
//
// This split mirrors the user's stated intent: "the research
// agent's experience follows the agent across teams". PM lessons
// are intentionally NOT portable because they encode this fund's
// sleeve weights, risk limits, and team interaction patterns —
// none of which transfer when the agent joins a different fund
// with different sleeves and a different team.
//
// To change the default behaviour for an entire batch, set
// WriteOptions.Visibility explicitly. To override at the row
// level, the caller must split the batch — there is no
// per-outcome override field on Outcome itself today (would
// require a schema change to agentreputation.Outcome which is
// out of scope for AP2).
func defaultVisibilityForKind(kind agentreputation.AgentKind) string {
	switch kind {
	case agentreputation.KindResearcher, agentreputation.KindAnalyst:
		return "agent_portable"
	}
	return "fund"
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
	// AP2 widened the INSERT to also populate the agent_id FK
	// (was previously NULL on every alpha lesson row — the
	// agent's UUID went into agent_tag as text only). The new
	// agent_id column is what AP3's read path joins on for the
	// agent_portable cross-fund retrieval, so populating it
	// here is the prerequisite for the new visibility class.
	// agent_tag is preserved unchanged for back-compat with
	// existing consumers (the prompt builder still reads it).
	const insertSQL = `INSERT INTO memories
		(fund_id, agent_id, layer, visibility, sensitivity, origin_kind,
		 title, content, trading_date, tags,
		 agent_tag, alpha_vs_benchmark, source_outcome_id)
		VALUES ($1, $2, $3, $4, $5, $6,
		        $7, $8, $9, $10,
		        $11, $12, $13)`
	insertStmt, err := tx.PrepareContext(ctx, insertSQL)
	if err != nil {
		return 0, fmt.Errorf("alphalesson: prepare insert: %w", err)
	}
	defer insertStmt.Close()

	// optsVisibilityExplicit captures whether the caller pinned
	// a Visibility value before normalize() stripped its
	// default. We have to read it BEFORE the per-outcome loop
	// because normalize() ran above and we re-derive the
	// explicit-vs-resolver decision once per batch (the whole
	// batch shares one explicit override; per-row override is
	// out of scope, see defaultVisibilityForKind doc).
	optsVisibilityExplicit := strings.TrimSpace(opts.Visibility) != ""

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
		// Per-outcome visibility resolution. An explicit
		// opts.Visibility wins; otherwise the AgentKind picks
		// 'agent_portable' (researcher / analyst) or 'fund'
		// (pm / advocate / unknown). See
		// defaultVisibilityForKind comment for the full table.
		visibility := opts.Visibility
		if !optsVisibilityExplicit {
			visibility = defaultVisibilityForKind(o.AgentKind)
		}
		// agent_id is the UUID FK. We try to populate it from
		// o.AgentID — if it doesn't parse as a UUID (some
		// historical advocate rows use 'bull_researcher'-style
		// string tags) we leave the FK NULL and rely on
		// agent_tag for downstream attribution. The CHECK
		// constraint on memories.visibility allows
		// 'agent_portable' even when agent_id is NULL, so we
		// don't downgrade the visibility for tag-style rows —
		// but the AP3 read path will skip them on the
		// cross-fund branch because the join key is agent_id.
		// That's the right outcome: a tag-style row can't be
		// safely propagated cross-fund because we don't know
		// which actual agents.id it refers to.
		agentIDArg := nullableUUID(o.AgentID)
		if _, err := insertStmt.ExecContext(ctx,
			o.FundID, agentIDArg, opts.Layer, visibility, opts.Sensitivity, opts.OriginKind,
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

