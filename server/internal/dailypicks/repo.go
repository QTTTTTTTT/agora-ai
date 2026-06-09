// Package dailypicks is the persistence façade for the
// /daily-picks publisher surface (migration 106).
//
// Why it lives outside advisor/:
//
//	advisor.Repo deals with per-USER rows (advisor_consultations).
//	dailypicks.Repo deals with SHARED publisher rows
//	(daily_picks). Mixing them risks a refactor accidentally
//	letting a publisher row inherit a user_id column, which is
//	the kind of foot-gun the SEC Publishers' Exclusion makes
//	expensive — see the long comment in migration 106.
//
// Pattern mirrors advisor.Repo and analystreport.Repo:
//
//   - One *sql.DB at construction; nil-safe degradation.
//   - Strict input validation at the door.
//   - Idempotent UPSERT semantics (ON CONFLICT (symbol, market,
//     preset_key, pick_date)) so a re-run after a transient
//     failure overwrites instead of duplicates.
package dailypicks

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

// ErrNotFound is returned by Get when no row matches the lookup
// keys. Handlers map this to 404.
var ErrNotFound = errors.New("dailypicks: not found")

// Repo is the persistence façade. Construct with NewRepo(db).
// nil-safe: every read returns empty + nil error when r.db == nil
// so an under-wired binary (degraded boot, tests with no DB) doesn't
// crash.
type Repo struct {
	db *sql.DB
}

// NewRepo wires the repo. Passing nil yields a no-op repo (reads
// return empty, writes return "repo not initialised").
func NewRepo(db *sql.DB) *Repo { return &Repo{db: db} }

// --- Watchlist reads ---------------------------------------------------------

// Watchlist is one row from daily_pick_watchlists — an
// admin-curated pool of tickers to score nightly under a single
// preset.
type Watchlist struct {
	ID           string
	Name         string
	Market       string
	PresetKey    string
	Symbols      []string
	ScheduleCron string
	Active       bool
	Notes        string
}

// ListActiveWatchlists returns every active watchlist row. The
// daily picks loop iterates this list per wave. Order: name ASC for
// deterministic logs (so "us_largecap_disruptive_v1" always logs
// before "us_smallcap_value_v1" — easier to spot a degraded run).
func (r *Repo) ListActiveWatchlists(ctx context.Context) ([]Watchlist, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	const q = `
		SELECT id::TEXT, name, market, preset_key, symbols,
		       COALESCE(schedule_cron, '@daily_after_us_close'),
		       active, COALESCE(notes, '')
		  FROM daily_pick_watchlists
		 WHERE active = TRUE
		 ORDER BY name ASC`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("dailypicks: list watchlists: %w", err)
	}
	defer rows.Close()
	var out []Watchlist
	for rows.Next() {
		var w Watchlist
		var syms pq.StringArray
		if err := rows.Scan(&w.ID, &w.Name, &w.Market, &w.PresetKey,
			&syms, &w.ScheduleCron, &w.Active, &w.Notes); err != nil {
			return nil, fmt.Errorf("dailypicks: scan watchlist: %w", err)
		}
		w.Symbols = []string(syms)
		out = append(out, w)
	}
	return out, rows.Err()
}

// --- daily_picks writes ------------------------------------------------------

// SaveInput is the payload UpsertPick expects. All fields are
// validated at the door so we never write a row with an empty
// symbol or a 1900-01-01 pick_date by accident.
type SaveInput struct {
	Symbol           string
	SymbolName       string
	Market           string
	PresetKey        string
	PickDate         time.Time // truncated to date by the writer
	ResultJSON       []byte    // pre-marshalled ConsultResponse
	AggregateVerdict string
	AggregateScore   int
	Consensus        float64
	LLMCostUSD       float64
	ErrorReason      string // non-empty when the pick failed; result_json may still be valid (e.g. partial panel)
}

func (in *SaveInput) validate() error {
	if strings.TrimSpace(in.Symbol) == "" {
		return errors.New("dailypicks: Symbol required")
	}
	if strings.TrimSpace(in.Market) == "" {
		return errors.New("dailypicks: Market required")
	}
	if strings.TrimSpace(in.PresetKey) == "" {
		return errors.New("dailypicks: PresetKey required")
	}
	if in.PickDate.IsZero() {
		return errors.New("dailypicks: PickDate required")
	}
	if len(in.ResultJSON) == 0 {
		// We allow an explicit empty JSON object for an error row
		// (so a failed pick still occupies the (symbol, day) slot
		// and isn't retried on every wave), but the caller has to
		// pass {} explicitly — silent empty is a bug.
		return errors.New("dailypicks: ResultJSON required (use {} for error placeholders)")
	}
	return nil
}

// UpsertPick writes one daily_picks row. Idempotent — re-running
// the loop for the same (symbol, market, preset, pick_date) will
// REPLACE the row, not insert a duplicate. This is the publisher
// invariant in code form: there's literally only one row, served
// to every reader.
//
// Returns the row id (a BIGSERIAL — handy for "show me the last 100
// publisher rows in id order" debug queries).
func (r *Repo) UpsertPick(ctx context.Context, in SaveInput) (int64, error) {
	if r == nil || r.db == nil {
		return 0, errors.New("dailypicks: repo not initialised")
	}
	if err := in.validate(); err != nil {
		return 0, err
	}
	const q = `
		INSERT INTO daily_picks (
			symbol, symbol_name, market, preset_key, pick_date,
			result_json, aggregate_verdict, aggregate_score,
			consensus, llm_cost_usd, error_reason, computed_at
		) VALUES (
			$1, NULLIF($2, ''), $3, $4, $5::DATE,
			$6::JSONB, NULLIF($7, ''), $8,
			$9, $10, NULLIF($11, ''), now()
		)
		ON CONFLICT (symbol, market, preset_key, pick_date)
		DO UPDATE SET
			symbol_name       = EXCLUDED.symbol_name,
			result_json       = EXCLUDED.result_json,
			aggregate_verdict = EXCLUDED.aggregate_verdict,
			aggregate_score   = EXCLUDED.aggregate_score,
			consensus         = EXCLUDED.consensus,
			llm_cost_usd      = EXCLUDED.llm_cost_usd,
			error_reason      = EXCLUDED.error_reason,
			computed_at       = now()
		RETURNING id`
	var id int64
	err := r.db.QueryRowContext(ctx, q,
		strings.ToUpper(strings.TrimSpace(in.Symbol)),
		strings.TrimSpace(in.SymbolName),
		strings.ToLower(strings.TrimSpace(in.Market)),
		strings.ToLower(strings.TrimSpace(in.PresetKey)),
		in.PickDate.UTC(),
		in.ResultJSON,
		strings.ToUpper(strings.TrimSpace(in.AggregateVerdict)),
		in.AggregateScore,
		in.Consensus,
		in.LLMCostUSD,
		strings.TrimSpace(in.ErrorReason),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("dailypicks: upsert: %w", err)
	}
	return id, nil
}

// --- daily_picks reads -------------------------------------------------------

// ListFilter is the browse-grid query shape. Empty fields mean
// "any". The handler is responsible for tier-based time-lag
// enforcement BEFORE calling List — this repo does NOT know about
// user tier, by design (keeps the publisher / personalisation
// boundary clean).
type ListFilter struct {
	Market    string
	PresetKey string
	// PickDate (optional): when set, only rows for this exact day.
	// When zero, the repo returns the most-recent ``Limit`` rows
	// across all days for the (market, preset) pair, useful for
	// the trailing-N-days history view.
	PickDate time.Time
	// MaxPickDate (optional): "show me rows with pick_date <=
	// this date". This is the slot the handler uses to inject
	// free-tier time-lag — e.g. MaxPickDate = today - 14d for a
	// free reader. Zero means no upper bound.
	MaxPickDate time.Time
	// MinAggregateScore filters out the "no view formed"
	// (insufficient_data) rows so the browse-grid doesn't lead
	// with blanks. Zero disables filtering. Note: HOLD rows
	// legitimately have score 0 because the aggregator doesn't
	// score HOLD — handlers that want to include them should
	// leave this at zero and filter on verdict instead.
	MinAggregateScore int
	Limit             int
	Offset            int
}

// PickRow is one row from daily_picks for the browse grid. The
// ResultJSON is included so the handler can decide whether to ship
// the full report or just the header — for a 50-row browse grid
// the per-row payload is small enough that shipping all of it is
// cheaper than a second round-trip.
type PickRow struct {
	ID               int64
	Symbol           string
	SymbolName       string
	Market           string
	PresetKey        string
	PickDate         time.Time
	ResultJSON       []byte
	AggregateVerdict string
	AggregateScore   int
	Consensus        float64
	LLMCostUSD       float64
	ErrorReason      string
	ComputedAt       time.Time
}

// List returns rows matching the filter, ordered by pick_date DESC
// then aggregate_score DESC. The default ordering matches what a
// reader actually wants: "most recent day first, top scores at the
// top". When PickDate is pinned, the ordering degenerates to score
// DESC (date is already constant within the result set).
func (r *Repo) List(ctx context.Context, f ListFilter) ([]PickRow, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > 500 {
		f.Limit = 500
	}
	if f.Offset < 0 {
		f.Offset = 0
	}

	// Build the WHERE clause dynamically so missing filters don't
	// burn a parameter slot (and so EXPLAIN can use the indexes
	// cleanly — adding $N IS NULL OR symbol = $N forces a seq scan).
	var (
		conds = []string{"1=1"}
		args  []interface{}
	)
	add := func(cond string, val interface{}) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(cond, len(args)))
	}
	if f.Market != "" {
		add("market = $%d", strings.ToLower(strings.TrimSpace(f.Market)))
	}
	if f.PresetKey != "" {
		add("preset_key = $%d", strings.ToLower(strings.TrimSpace(f.PresetKey)))
	}
	if !f.PickDate.IsZero() {
		add("pick_date = $%d::DATE", f.PickDate.UTC())
	}
	if !f.MaxPickDate.IsZero() {
		add("pick_date <= $%d::DATE", f.MaxPickDate.UTC())
	}
	if f.MinAggregateScore > 0 {
		add("aggregate_score >= $%d", f.MinAggregateScore)
	}
	args = append(args, f.Limit, f.Offset)
	limitOffsetSQL := fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)-1, len(args))

	q := `
		SELECT id, symbol, COALESCE(symbol_name, ''), market, preset_key,
		       pick_date, result_json,
		       COALESCE(aggregate_verdict, ''), COALESCE(aggregate_score, 0),
		       COALESCE(consensus, 0), COALESCE(llm_cost_usd, 0),
		       COALESCE(error_reason, ''), computed_at
		  FROM daily_picks
		 WHERE ` + strings.Join(conds, " AND ") + `
		 ORDER BY pick_date DESC, aggregate_score DESC, symbol ASC` +
		limitOffsetSQL

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("dailypicks: list: %w", err)
	}
	defer rows.Close()
	var out []PickRow
	for rows.Next() {
		var r PickRow
		if err := rows.Scan(&r.ID, &r.Symbol, &r.SymbolName, &r.Market, &r.PresetKey,
			&r.PickDate, &r.ResultJSON,
			&r.AggregateVerdict, &r.AggregateScore,
			&r.Consensus, &r.LLMCostUSD, &r.ErrorReason, &r.ComputedAt); err != nil {
			return nil, fmt.Errorf("dailypicks: scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get fetches a single row by the publisher key. Used by the
// detail endpoint and the cache short-circuit in PublishConsult.
// Returns ErrNotFound when no row exists.
func (r *Repo) Get(ctx context.Context, symbol, market, presetKey string, pickDate time.Time) (*PickRow, error) {
	if r == nil || r.db == nil {
		return nil, ErrNotFound
	}
	const q = `
		SELECT id, symbol, COALESCE(symbol_name, ''), market, preset_key,
		       pick_date, result_json,
		       COALESCE(aggregate_verdict, ''), COALESCE(aggregate_score, 0),
		       COALESCE(consensus, 0), COALESCE(llm_cost_usd, 0),
		       COALESCE(error_reason, ''), computed_at
		  FROM daily_picks
		 WHERE symbol = $1 AND market = $2 AND preset_key = $3 AND pick_date = $4::DATE
		 LIMIT 1`
	var p PickRow
	err := r.db.QueryRowContext(ctx, q,
		strings.ToUpper(strings.TrimSpace(symbol)),
		strings.ToLower(strings.TrimSpace(market)),
		strings.ToLower(strings.TrimSpace(presetKey)),
		pickDate.UTC(),
	).Scan(&p.ID, &p.Symbol, &p.SymbolName, &p.Market, &p.PresetKey,
		&p.PickDate, &p.ResultJSON,
		&p.AggregateVerdict, &p.AggregateScore,
		&p.Consensus, &p.LLMCostUSD, &p.ErrorReason, &p.ComputedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("dailypicks: get: %w", err)
	}
	return &p, nil
}

// CountForDay returns how many picks were written for a given
// (market, preset, day). Used by the loop to short-circuit a wave
// when today's set is already complete (e.g. ops re-ran by
// accident) and by the API for the "fully-computed?" health bar.
func (r *Repo) CountForDay(ctx context.Context, market, presetKey string, pickDate time.Time) (int, error) {
	if r == nil || r.db == nil {
		return 0, nil
	}
	const q = `
		SELECT COUNT(*) FROM daily_picks
		 WHERE market = $1 AND preset_key = $2 AND pick_date = $3::DATE`
	var n int
	err := r.db.QueryRowContext(ctx, q,
		strings.ToLower(strings.TrimSpace(market)),
		strings.ToLower(strings.TrimSpace(presetKey)),
		pickDate.UTC(),
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("dailypicks: count: %w", err)
	}
	return n, nil
}

// --- JSON helpers ------------------------------------------------------------

// MarshalResult is a convenience the loop uses to serialise the
// service.ConsultResponse-shaped struct to JSON before calling
// UpsertPick. Kept here so the canonical "what does result_json
// look like on disk" lives next to the schema.
//
// We deliberately use the default json.Marshal (no MarshalIndent) so
// the on-disk form is compact — these rows are read by other rows
// of the pipeline and humans only via psql.
func MarshalResult(v interface{}) ([]byte, error) {
	if v == nil {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("dailypicks: marshal result: %w", err)
	}
	return b, nil
}
