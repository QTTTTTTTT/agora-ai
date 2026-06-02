// repo.go — DB-backed surveillance store (P1-7).
//
// Two surfaces
//
//   - Events: Insert (with idempotency), List, Get, UpdateStatus.
//     The unique index on `fingerprint` handles dedup at the DB
//     level; we surface a non-error "no-op" result when a re-run
//     hits the same fingerprint.
//   - Runs: CreateRun, ListRuns. The run row is the audit record
//     that "we scanned X trades and produced N events", separate
//     from the events themselves.

package surveillance

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Repo wraps the surveillance tables.
type Repo struct {
	db *sql.DB
}

// NewRepo constructs a Repo. nil db is rejected at first call,
// matching the FX/recon repo pattern.
func NewRepo(db *sql.DB) *Repo {
	return &Repo{db: db}
}

// ----- Events -----

// InsertEventResult tells the caller whether the row landed or was
// deduped. Inserted = true when this row is new to the table.
type InsertEventResult struct {
	ID       string
	Inserted bool
}

// InsertEvent persists one Event. Re-inserting the same fingerprint
// returns the existing row's ID with Inserted = false.
func (r *Repo) InsertEvent(ctx context.Context, ev Event) (*InsertEventResult, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("surveillance: nil db")
	}
	if strings.TrimSpace(ev.FundID) == "" {
		return nil, errors.New("surveillance: fund_id required")
	}
	if string(ev.RuleCode) == "" {
		return nil, errors.New("surveillance: rule_code required")
	}
	if ev.Severity == "" {
		ev.Severity = SeverityWarning
	}
	if ev.Status == "" {
		ev.Status = StatusOpen
	}
	if ev.DetectorVersion == "" {
		ev.DetectorVersion = detectorVersion
	}
	if ev.Fingerprint == "" {
		ev.Fingerprint = fingerprintFor(ev.FundID, ev.RuleCode, ev.TradeIDs)
	}
	tradeIDsJSON, _ := json.Marshal(append([]string(nil), ev.TradeIDs...))
	metadataJSON, _ := json.Marshal(ev.Metadata)
	if len(metadataJSON) == 0 || string(metadataJSON) == "null" {
		metadataJSON = []byte("{}")
	}

	// Try insert; if the unique index fires, look up the existing
	// row and return that. ON CONFLICT lets us do this in one
	// round-trip.
	var id string
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO surveillance_events
		    (fund_id, rule_code, severity, symbol, instrument_key,
		     window_start, window_end, trade_ids, summary, metadata,
		     status, detector_version, fingerprint)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10::jsonb, $11, $12, $13)
		ON CONFLICT (fingerprint) DO NOTHING
		RETURNING id::text
	`,
		ev.FundID, string(ev.RuleCode), string(ev.Severity),
		nullableString(ev.Symbol), nullableString(ev.InstrumentKey),
		ev.WindowStart.UTC(), ev.WindowEnd.UTC(),
		string(tradeIDsJSON), ev.Summary, string(metadataJSON),
		string(ev.Status), ev.DetectorVersion, ev.Fingerprint,
	).Scan(&id)
	switch {
	case err == nil:
		return &InsertEventResult{ID: id, Inserted: true}, nil
	case errors.Is(err, sql.ErrNoRows):
		// Conflict path — find the existing row.
		var existing string
		lookupErr := r.db.QueryRowContext(ctx,
			`SELECT id::text FROM surveillance_events WHERE fingerprint = $1`,
			ev.Fingerprint,
		).Scan(&existing)
		if lookupErr != nil {
			return nil, fmt.Errorf("surveillance: lookup after conflict: %w", lookupErr)
		}
		return &InsertEventResult{ID: existing, Inserted: false}, nil
	default:
		return nil, fmt.Errorf("surveillance: insert event: %w", err)
	}
}

// ListEventsParams filters the events list. Empty fields apply
// no filter.
type ListEventsParams struct {
	FundID   string
	RuleCode RuleCode
	Status   EventStatus
	Severity Severity
	From     time.Time // detected_at >=
	To       time.Time // detected_at <=
	Limit    int
	Offset   int
}

// ListEvents returns events ordered by (severity DESC, detected_at DESC).
func (r *Repo) ListEvents(ctx context.Context, p ListEventsParams) ([]Event, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("surveillance: nil db")
	}
	if p.Limit <= 0 || p.Limit > 500 {
		p.Limit = 100
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	conds := []string{}
	args := []any{p.Limit, p.Offset}
	if strings.TrimSpace(p.FundID) != "" {
		args = append(args, p.FundID)
		conds = append(conds, fmt.Sprintf("fund_id = $%d", len(args)))
	}
	if string(p.RuleCode) != "" {
		args = append(args, string(p.RuleCode))
		conds = append(conds, fmt.Sprintf("rule_code = $%d", len(args)))
	}
	if string(p.Status) != "" {
		args = append(args, string(p.Status))
		conds = append(conds, fmt.Sprintf("status = $%d", len(args)))
	}
	if string(p.Severity) != "" {
		args = append(args, string(p.Severity))
		conds = append(conds, fmt.Sprintf("severity = $%d", len(args)))
	}
	if !p.From.IsZero() {
		args = append(args, p.From.UTC())
		conds = append(conds, fmt.Sprintf("detected_at >= $%d", len(args)))
	}
	if !p.To.IsZero() {
		args = append(args, p.To.UTC())
		conds = append(conds, fmt.Sprintf("detected_at <= $%d", len(args)))
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	q := fmt.Sprintf(`
		SELECT id::text, fund_id::text, rule_code, severity,
		       COALESCE(symbol, ''), COALESCE(instrument_key, ''),
		       window_start, window_end,
		       COALESCE(trade_ids::text, '[]'),
		       COALESCE(summary, ''),
		       COALESCE(metadata::text, '{}'),
		       status, COALESCE(review_note, ''),
		       COALESCE(reviewed_by::text, ''), reviewed_at,
		       detected_at, COALESCE(detector_version, ''), fingerprint
		  FROM surveillance_events
		  %s
		 ORDER BY (CASE severity
		             WHEN 'critical' THEN 0
		             WHEN 'warning' THEN 1
		             ELSE 2 END),
		          detected_at DESC
		 LIMIT $1 OFFSET $2
	`, where)
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("surveillance: list events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		ev := Event{}
		var (
			tradeIDsRaw string
			metadataRaw string
			reviewedAt  sql.NullTime
		)
		if err := rows.Scan(&ev.ID, &ev.FundID, (*string)(&ev.RuleCode), (*string)(&ev.Severity),
			&ev.Symbol, &ev.InstrumentKey,
			&ev.WindowStart, &ev.WindowEnd,
			&tradeIDsRaw, &ev.Summary, &metadataRaw,
			(*string)(&ev.Status), new(string), // review_note unused on list
			new(string), &reviewedAt,
			&ev.DetectedAt, &ev.DetectorVersion, &ev.Fingerprint); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(tradeIDsRaw), &ev.TradeIDs)
		_ = json.Unmarshal([]byte(metadataRaw), &ev.Metadata)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// GetEvent returns a single event with all fields populated
// (including review_note and reviewed_by).
type EventDetail struct {
	Event
	ReviewNote string
	ReviewedBy string
	ReviewedAt *time.Time
}

func (r *Repo) GetEvent(ctx context.Context, id string) (*EventDetail, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("surveillance: nil db")
	}
	row := r.db.QueryRowContext(ctx, `
		SELECT id::text, fund_id::text, rule_code, severity,
		       COALESCE(symbol, ''), COALESCE(instrument_key, ''),
		       window_start, window_end,
		       COALESCE(trade_ids::text, '[]'),
		       COALESCE(summary, ''),
		       COALESCE(metadata::text, '{}'),
		       status, COALESCE(review_note, ''),
		       COALESCE(reviewed_by::text, ''), reviewed_at,
		       detected_at, COALESCE(detector_version, ''), fingerprint
		  FROM surveillance_events
		 WHERE id = $1
	`, id)
	d := &EventDetail{}
	var (
		tradeIDsRaw string
		metadataRaw string
		reviewedAt  sql.NullTime
	)
	if err := row.Scan(&d.ID, &d.FundID, (*string)(&d.RuleCode), (*string)(&d.Severity),
		&d.Symbol, &d.InstrumentKey,
		&d.WindowStart, &d.WindowEnd,
		&tradeIDsRaw, &d.Summary, &metadataRaw,
		(*string)(&d.Status), &d.ReviewNote,
		&d.ReviewedBy, &reviewedAt,
		&d.DetectedAt, &d.DetectorVersion, &d.Fingerprint); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrEventNotFound
		}
		return nil, fmt.Errorf("surveillance: get event: %w", err)
	}
	_ = json.Unmarshal([]byte(tradeIDsRaw), &d.TradeIDs)
	_ = json.Unmarshal([]byte(metadataRaw), &d.Metadata)
	if reviewedAt.Valid {
		t := reviewedAt.Time
		d.ReviewedAt = &t
	}
	return d, nil
}

// UpdateStatusParams flips an event between lifecycle states.
type UpdateStatusParams struct {
	ID         string
	NewStatus  EventStatus
	Note       string
	ReviewedBy string
}

// UpdateStatus enforces the closed status vocabulary and writes
// reviewer + note + reviewed_at when transitioning to a terminal
// state. Re-opening (status='open') clears the reviewer fields so
// the row visibly returns to the queue.
func (r *Repo) UpdateStatus(ctx context.Context, p UpdateStatusParams) error {
	if r == nil || r.db == nil {
		return errors.New("surveillance: nil db")
	}
	switch p.NewStatus {
	case StatusOpen, StatusReviewing, StatusCleared, StatusEscalated:
	default:
		return ErrInvalidStatus
	}
	now := time.Now().UTC()
	var (
		reviewedAtArg any
		reviewerArg   any
	)
	if p.NewStatus == StatusCleared || p.NewStatus == StatusEscalated || p.NewStatus == StatusReviewing {
		reviewedAtArg = now
		if strings.TrimSpace(p.ReviewedBy) != "" {
			reviewerArg = p.ReviewedBy
		}
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE surveillance_events
		   SET status = $2,
		       review_note = $3,
		       reviewed_by = $4,
		       reviewed_at = $5
		 WHERE id = $1
	`, p.ID, string(p.NewStatus), p.Note, reviewerArg, reviewedAtArg)
	if err != nil {
		return fmt.Errorf("surveillance: update status: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrEventNotFound
	}
	return nil
}

// ----- Runs -----

// Run mirrors surveillance_runs.
type Run struct {
	ID                  string
	FundID              string
	TriggeredBy         string
	TriggerSource       string
	WindowStart         time.Time
	WindowEnd           time.Time
	TradeCount          int
	EventCountTotal     int
	EventCountCritical  int
	EventCountWarning   int
	EventCountInfo      int
	DurationMS          int
	Status              string
	ErrorMessage        string
	Summary             map[string]any
	StartedAt           time.Time
	CompletedAt         *time.Time
}

// CreateRunParams is the input for CreateRun.
type CreateRunParams struct {
	FundID         string
	TriggeredBy    string
	TriggerSource  string
	WindowStart    time.Time
	WindowEnd      time.Time
	TradeCount     int
	Result         RunResult
	DurationMS     int
	Status         string
	ErrorMessage   string
	Summary        map[string]any
}

// CreateRun writes one row into surveillance_runs. The events are
// expected to already be persisted via InsertEvent — the run row
// is bookkeeping.
func (r *Repo) CreateRun(ctx context.Context, p CreateRunParams) (*Run, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("surveillance: nil db")
	}
	if p.TriggerSource == "" {
		p.TriggerSource = "manual"
	}
	if p.Status == "" {
		p.Status = "completed"
	}
	now := time.Now().UTC()
	summaryJSON, _ := json.Marshal(p.Summary)
	if len(summaryJSON) == 0 || string(summaryJSON) == "null" {
		summaryJSON = []byte("{}")
	}
	cnt := p.Result.CountsBySeverity
	totalEvents := cnt[SeverityCritical] + cnt[SeverityWarning] + cnt[SeverityInfo]
	var (
		fundIDArg     any
		triggeredArg  any
	)
	if strings.TrimSpace(p.FundID) != "" {
		fundIDArg = p.FundID
	}
	if strings.TrimSpace(p.TriggeredBy) != "" {
		triggeredArg = p.TriggeredBy
	}

	var id string
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO surveillance_runs
		    (fund_id, triggered_by, trigger_source, window_start, window_end,
		     trade_count, event_count_total, event_count_critical,
		     event_count_warning, event_count_info, duration_ms,
		     status, error_message, summary, started_at, completed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14::jsonb, $15, $16)
		RETURNING id::text
	`,
		fundIDArg, triggeredArg, p.TriggerSource,
		p.WindowStart.UTC(), p.WindowEnd.UTC(),
		p.TradeCount, totalEvents,
		cnt[SeverityCritical], cnt[SeverityWarning], cnt[SeverityInfo],
		p.DurationMS, p.Status, p.ErrorMessage, string(summaryJSON),
		now, now,
	).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("surveillance: insert run: %w", err)
	}
	completed := now
	return &Run{
		ID:                 id,
		FundID:             p.FundID,
		TriggeredBy:        p.TriggeredBy,
		TriggerSource:      p.TriggerSource,
		WindowStart:        p.WindowStart.UTC(),
		WindowEnd:          p.WindowEnd.UTC(),
		TradeCount:         p.TradeCount,
		EventCountTotal:    totalEvents,
		EventCountCritical: cnt[SeverityCritical],
		EventCountWarning:  cnt[SeverityWarning],
		EventCountInfo:     cnt[SeverityInfo],
		DurationMS:         p.DurationMS,
		Status:             p.Status,
		ErrorMessage:       p.ErrorMessage,
		Summary:            p.Summary,
		StartedAt:          now,
		CompletedAt:        &completed,
	}, nil
}

// ListRunsParams filters the run list.
type ListRunsParams struct {
	FundID string
	Limit  int
	Offset int
}

// ListRuns returns recent runs.
func (r *Repo) ListRuns(ctx context.Context, p ListRunsParams) ([]Run, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("surveillance: nil db")
	}
	if p.Limit <= 0 || p.Limit > 200 {
		p.Limit = 50
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	args := []any{p.Limit, p.Offset}
	where := ""
	if strings.TrimSpace(p.FundID) != "" {
		where = "WHERE fund_id = $3"
		args = append(args, p.FundID)
	}
	q := fmt.Sprintf(`
		SELECT id::text, COALESCE(fund_id::text, ''),
		       COALESCE(triggered_by::text, ''), trigger_source,
		       window_start, window_end, trade_count,
		       event_count_total, event_count_critical, event_count_warning, event_count_info,
		       duration_ms, status, COALESCE(error_message, ''),
		       COALESCE(summary::text, '{}'),
		       started_at, completed_at
		  FROM surveillance_runs
		  %s
		 ORDER BY started_at DESC
		 LIMIT $1 OFFSET $2
	`, where)
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("surveillance: list runs: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var (
			run         Run
			summaryRaw  string
			completedAt sql.NullTime
		)
		if err := rows.Scan(&run.ID, &run.FundID, &run.TriggeredBy, &run.TriggerSource,
			&run.WindowStart, &run.WindowEnd, &run.TradeCount,
			&run.EventCountTotal, &run.EventCountCritical, &run.EventCountWarning, &run.EventCountInfo,
			&run.DurationMS, &run.Status, &run.ErrorMessage,
			&summaryRaw, &run.StartedAt, &completedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(summaryRaw), &run.Summary)
		if completedAt.Valid {
			t := completedAt.Time
			run.CompletedAt = &t
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

// ----- helpers -----

func nullableString(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// SortedTradeIDs returns a stable copy of the trade ID slice;
// callers serialising the event use this so deserialised JSON
// is reproducible.
func SortedTradeIDs(ids []string) []string {
	cp := append([]string(nil), ids...)
	sort.Strings(cp)
	return cp
}
