package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/fundai/server/internal/corpaction"
)

// CorpActionRow is the on-the-wire shape of a single corporate
// action event. Mirrors the corporate_actions schema column-for-
// column so callers (admin handler, daily ingest) don't need to
// know the SQL.
type CorpActionRow struct {
	ID            string
	InstrumentKey string
	ExDate        time.Time
	ActionType    string
	SplitRatio    float64
	CashDividend  float64
	Source        string
	Notes         sql.NullString
	AnnouncedAt   sql.NullTime
	RecordedAt    time.Time
}

// ToEvent narrows a CorpActionRow to the immutable Event shape the
// applier consumes. We keep the two structs separate so the applier
// package doesn't import database/sql.
func (r CorpActionRow) ToEvent() corpaction.Event {
	return corpaction.Event{
		ID:            r.ID,
		InstrumentKey: r.InstrumentKey,
		ExDate:        r.ExDate,
		ActionType:    r.ActionType,
		SplitRatio:    r.SplitRatio,
		CashDividend:  r.CashDividend,
		Source:        r.Source,
	}
}

// CorpActionRepo is the CRUD facade over corporate_actions and its
// child corp_action_applications. The methods here cover three
// callers:
//
//   - admin handler:  Upsert (record what the operator entered)
//                     and ListPending (audit dashboard).
//   - daily ingest:   Upsert in bulk after the Yahoo / Tushare
//                     poll, then iterate Pending in the sweeper.
//   - holding detail: ApplicationsForFund (timeline UI).
type CorpActionRepo struct {
	db *sql.DB
}

func NewCorpActionRepo(db *sql.DB) *CorpActionRepo {
	return &CorpActionRepo{db: db}
}

// Upsert writes a single event, deduping on the natural key
// (instrument_key, ex_date, action_type, source). When the row
// already exists we do not mutate anything — the operator/manual
// path that landed first is the source of truth and any divergent
// auto-ingested copy from Yahoo is dropped on the floor with a
// silent no-op rather than overriding human input.
//
// Returns the canonical id of the row in the table — either the
// freshly inserted one or the pre-existing one, so the caller can
// proceed to apply it to fund holdings.
func (r *CorpActionRepo) Upsert(ctx context.Context, in CorpActionRow) (string, error) {
	const q = `
        INSERT INTO corporate_actions
            (instrument_key, ex_date, action_type, split_ratio, cash_dividend,
             source, notes, announced_at)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
        ON CONFLICT (instrument_key, ex_date, action_type, source) DO UPDATE
            SET notes        = COALESCE(EXCLUDED.notes,        corporate_actions.notes),
                announced_at = COALESCE(EXCLUDED.announced_at, corporate_actions.announced_at)
        RETURNING id
    `
	var id string
	err := r.db.QueryRowContext(ctx, q,
		in.InstrumentKey, in.ExDate, in.ActionType, in.SplitRatio, in.CashDividend,
		in.Source, in.Notes, in.AnnouncedAt,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("corp_action_repo: upsert: %w", err)
	}
	return id, nil
}

// GetByID hydrates a single event by primary key. Used by the admin
// handler when the operator clicks "apply" on an event id from the
// dashboard.
func (r *CorpActionRepo) GetByID(ctx context.Context, id string) (CorpActionRow, error) {
	const q = `
        SELECT id, instrument_key, ex_date, action_type, split_ratio,
               cash_dividend, source, notes, announced_at, recorded_at
          FROM corporate_actions
         WHERE id = $1
    `
	var row CorpActionRow
	err := r.db.QueryRowContext(ctx, q, id).Scan(
		&row.ID, &row.InstrumentKey, &row.ExDate, &row.ActionType, &row.SplitRatio,
		&row.CashDividend, &row.Source, &row.Notes, &row.AnnouncedAt, &row.RecordedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return row, fmt.Errorf("corp_action_repo: id %s: %w", id, ErrCorpActionNotFound)
	}
	if err != nil {
		return row, fmt.Errorf("corp_action_repo: get: %w", err)
	}
	return row, nil
}

// PendingForFund returns every event whose ex_date is on or before
// `asOf` AND that has NOT yet been applied to the given fund. Used
// by the daily sweeper after each ingest pass to fan out
// applications to every affected fund.
//
// Note we filter by an outer LEFT JOIN against corp_action_applications
// rather than maintaining a denormalised "pending" flag — that flag
// would be wrong as soon as a NEW fund starts holding the
// instrument, since the same event_id has different application
// state per fund.
func (r *CorpActionRepo) PendingForFund(ctx context.Context, fundID string, asOf time.Time) ([]CorpActionRow, error) {
	const q = `
        SELECT ca.id, ca.instrument_key, ca.ex_date, ca.action_type,
               ca.split_ratio, ca.cash_dividend, ca.source, ca.notes,
               ca.announced_at, ca.recorded_at
          FROM corporate_actions ca
          JOIN holding_positions hp ON hp.instrument_key = ca.instrument_key
          LEFT JOIN corp_action_applications app
                 ON app.corp_action_id = ca.id AND app.fund_id = hp.fund_id
         WHERE hp.fund_id = $1
           AND ca.ex_date <= $2
           AND app.corp_action_id IS NULL
         ORDER BY ca.ex_date ASC
    `
	rows, err := r.db.QueryContext(ctx, q, fundID, asOf)
	if err != nil {
		return nil, fmt.Errorf("corp_action_repo: pending: %w", err)
	}
	defer rows.Close()

	var out []CorpActionRow
	for rows.Next() {
		var row CorpActionRow
		if err := rows.Scan(
			&row.ID, &row.InstrumentKey, &row.ExDate, &row.ActionType,
			&row.SplitRatio, &row.CashDividend, &row.Source, &row.Notes,
			&row.AnnouncedAt, &row.RecordedAt,
		); err != nil {
			return nil, fmt.Errorf("corp_action_repo: scan: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ApplicationRow is the audit receipt persisted by the applier.
// Surfaced to the holding detail page as a "corp action timeline".
type ApplicationRow struct {
	CorpActionID  string
	FundID        string
	InstrumentKey string
	ExDate        time.Time
	ActionType    string
	SplitRatio    float64
	CashDividend  float64
	AppliedAt     time.Time
	PreQuantity   float64
	PostQuantity  float64
	PreCost       float64
	PostCost      float64
	CashCredit    float64
}

// ApplicationsForFund powers the timeline UI on the fund overview /
// holding detail page. Returns events newest-first.
func (r *CorpActionRepo) ApplicationsForFund(ctx context.Context, fundID string, limit int) ([]ApplicationRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	const q = `
        SELECT app.corp_action_id, app.fund_id,
               ca.instrument_key, ca.ex_date, ca.action_type,
               ca.split_ratio, ca.cash_dividend,
               app.applied_at,
               app.pre_quantity, app.post_quantity,
               app.pre_cost_price, app.post_cost_price,
               app.cash_credit
          FROM corp_action_applications app
          JOIN corporate_actions ca ON ca.id = app.corp_action_id
         WHERE app.fund_id = $1
         ORDER BY app.applied_at DESC
         LIMIT $2
    `
	rows, err := r.db.QueryContext(ctx, q, fundID, limit)
	if err != nil {
		return nil, fmt.Errorf("corp_action_repo: applications: %w", err)
	}
	defer rows.Close()
	var out []ApplicationRow
	for rows.Next() {
		var row ApplicationRow
		if err := rows.Scan(
			&row.CorpActionID, &row.FundID,
			&row.InstrumentKey, &row.ExDate, &row.ActionType,
			&row.SplitRatio, &row.CashDividend,
			&row.AppliedAt,
			&row.PreQuantity, &row.PostQuantity,
			&row.PreCost, &row.PostCost,
			&row.CashCredit,
		); err != nil {
			return nil, fmt.Errorf("corp_action_repo: scan application: %w", err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ErrCorpActionNotFound is returned by GetByID when the event id is
// unknown. The admin handler maps this to 404.
var ErrCorpActionNotFound = errors.New("corp_action_repo: event not found")
