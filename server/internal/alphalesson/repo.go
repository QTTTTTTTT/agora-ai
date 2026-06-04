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
//
// AP3 added Visibility, AgentID, and InheritedFromOtherFund.
// The first two are persisted columns from memories; the third
// is derived in the read path so the caller (and the UI) can
// surface a "learned at another fund" badge without re-doing
// the team-membership join.
type LessonRow struct {
	ID              string
	FundID          string // origin fund (always the writer)
	AgentTag        string
	Content         string
	Title           sql.NullString
	AlphaVsBench    sql.NullFloat64
	SourceOutcomeID sql.NullString
	TradingDate     sql.NullTime
	CreatedAt       time.Time
	// Visibility is the memories.visibility column. New
	// alpha-tagged rows (post-AP2) are either 'fund' or
	// 'agent_portable'. Older rows can be 'private' too.
	Visibility string
	// AgentID is the UUID FK to agents(id). NULL for legacy
	// rows whose agent identity was only ever stored in
	// agent_tag (tag-style strings — see AP2 nullableUUID).
	AgentID sql.NullString
	// InheritedFromOtherFund is true iff this row was
	// retrieved through the cross-fund agent_portable branch
	// AND the row's own fund_id does NOT match the querying
	// fund. Equivalently: the lesson originated at a different
	// fund and is visible here only because the agent that
	// emitted it is on this fund's team. The UI uses this to
	// label the lesson with "learned at fund X".
	InheritedFromOtherFund bool
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
//
// AP3 added TeamAgentIDs and AllowAgentPortableImports to
// support the cross-fund retrieval branch. Callers that haven't
// migrated yet leave both at their zero values, which collapses
// the query back to the pre-AP3 fund-scoped behaviour — no
// silent regressions.
type ListLessonsParams struct {
	// FundID is the QUERYING fund (the one whose PM prompt is
	// being built). Lessons whose own fund_id matches are
	// always returned regardless of visibility.
	FundID string
	// AgentTag optionally narrows results to a single
	// denormalized agent_tag value. Predates AP3.
	AgentTag string
	// Limit caps the row count. Defaults to 50, capped at 200.
	Limit int
	// TeamAgentIDs lists the UUIDs of agents currently on the
	// querying fund's team (typically the active rows in
	// fund_team_members for FundID). When non-empty AND
	// AllowAgentPortableImports is true (or unset), the read
	// path adds a second branch:
	//
	//   visibility = 'agent_portable'
	//   AND agent_id = ANY($TeamAgentIDs)
	//   AND sensitivity != 'secret'
	//
	// so lessons written at OTHER funds by agents who are now
	// on THIS fund's team become visible. When empty, the
	// cross-fund branch is skipped entirely (query collapses
	// to the legacy fund_id-only filter).
	TeamAgentIDs []string
	// AllowAgentPortableImports mirrors the per-fund opt-out
	// flag (fund.config.allow_agent_portable_imports). The
	// caller resolves the flag from fund.config and passes the
	// value here so the repo doesn't need to know about
	// fund.config layout. Treat ANY default value as opt-IN
	// because the caller MUST distinguish "flag absent" from
	// "flag = false"; passing a *bool would be cleaner but
	// every existing caller would have to be updated. We use
	// the simpler bool + companion ExplicitlyOptedOut field.
	AllowAgentPortableImports bool
	// ExplicitlyOptedOut, when true, hard-disables the
	// cross-fund branch even if TeamAgentIDs is populated.
	// This is how a regulated / multi-LP fund refuses to
	// receive lessons learned at any other fund — without
	// this flag the caller couldn't distinguish "team empty"
	// from "import disabled".
	ExplicitlyOptedOut bool
}

// ListLessons returns the most-recent alpha-tagged memory rows
// visible to the querying fund. The result is the UNION of:
//
//  1. Lessons whose memories.fund_id matches FundID (legacy
//     fund-scoped path; unchanged from pre-AP3).
//  2. Lessons with visibility='agent_portable' whose agent_id
//     is in TeamAgentIDs and sensitivity != 'secret' (AP3
//     cross-fund branch; opt-out via ExplicitlyOptedOut).
//
// Rows that match BOTH branches (e.g. an agent_portable lesson
// emitted at this same fund — common: the writer stamps both
// fields) appear ONCE because OR is set semantics in SQL.
//
// Each returned row carries Visibility, AgentID, and a derived
// InheritedFromOtherFund flag the UI can use to label the
// lesson with its origin fund (AP8 surfaces it as a badge).
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

	// Filter team agent IDs through nullableUUID so a single
	// bad string in the caller-supplied slice doesn't blow up
	// the whole query at Postgres cast time. Same defensive
	// posture as the write path.
	cleanTeam := make([]string, 0, len(p.TeamAgentIDs))
	for _, id := range p.TeamAgentIDs {
		if v := nullableUUID(id); v.Valid {
			cleanTeam = append(cleanTeam, v.String)
		}
	}
	crossFundBranchEnabled := !p.ExplicitlyOptedOut && len(cleanTeam) > 0

	args := []interface{}{p.FundID}
	// Build the WHERE clause incrementally so the query shape
	// is identical for unit-test sqlmock pinning whether the
	// cross-fund branch is on or off.
	conds := []string{"agent_tag IS NOT NULL"}
	if crossFundBranchEnabled {
		args = append(args, pq.Array(cleanTeam))
		// Branch 1 OR branch 2, parenthesised so the
		// agent_tag-IS-NOT-NULL filter applies to both.
		conds = append(conds, fmt.Sprintf(
			"(fund_id = $1 OR (visibility = 'agent_portable' AND agent_id = ANY($%d::uuid[]) AND sensitivity <> 'secret'))",
			len(args),
		))
	} else {
		conds = append(conds, "fund_id = $1")
	}
	if strings.TrimSpace(p.AgentTag) != "" {
		args = append(args, p.AgentTag)
		conds = append(conds, fmt.Sprintf("agent_tag = $%d", len(args)))
	}
	args = append(args, limit)
	q := fmt.Sprintf(`SELECT id, fund_id, agent_id, visibility, agent_tag, content, title,
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
		var visibility sql.NullString
		if err := rows.Scan(
			&l.ID, &l.FundID, &l.AgentID, &visibility, &agentTag, &l.Content, &l.Title,
			&l.AlphaVsBench, &l.SourceOutcomeID, &l.TradingDate, &l.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("alphalesson: scan lesson: %w", err)
		}
		if agentTag.Valid {
			l.AgentTag = agentTag.String
		}
		if visibility.Valid {
			l.Visibility = visibility.String
		}
		// Derived flag: a row is "inherited from another fund"
		// when its own fund_id is different from the querying
		// fund AND its visibility is agent_portable. The
		// fund_id-only branch can never produce inherited
		// rows; the cross-fund branch produces them whenever
		// the writer fund differs from the reader fund.
		if l.Visibility == "agent_portable" && l.FundID != p.FundID {
			l.InheritedFromOtherFund = true
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

