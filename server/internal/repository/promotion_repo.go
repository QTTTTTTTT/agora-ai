package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PromotionRepo owns persistence for the strategy promotion
// lifecycle (Phase 2J), shadow comparison rows (Phase 2K) and
// decay-monitor health snapshots (Phase 2L).
//
// We co-locate the four tables under one repo because they share
// the same parent ID — promotion_id — and the API endpoints
// (e.g. "fetch promotion with shadow + health for the detail
// page") naturally fan out across them. Splitting them would
// trade one round of joins for three round-trips with no gain.
type PromotionRepo struct {
	db *sql.DB
}

func NewPromotionRepo(db *sql.DB) *PromotionRepo { return &PromotionRepo{db: db} }

// DB exposes the handle for tests that need to set up fixtures
// alongside the repo. Production code goes through the methods.
func (r *PromotionRepo) DB() *sql.DB { return r.db }

// PromotionRow mirrors strategy_promotions. JSONB columns land in
// json.RawMessage so callers can unmarshal into typed structs in
// the promotion package without dragging that package into here.
type PromotionRow struct {
	ID                string
	FundID            string
	ProposedBy        string
	BasisJobID        string
	EngineKind        string
	EngineParams      json.RawMessage
	BaselineMetrics   json.RawMessage
	Status            string
	ShadowDays        int
	DecayRatio        float64
	ApprovedBy        sql.NullString
	ApprovedAt        sql.NullTime
	RejectedBy        sql.NullString
	RejectedAt        sql.NullTime
	RejectedReason    sql.NullString
	ShadowStartedAt   sql.NullTime
	ShadowCompletedAt sql.NullTime
	ActivatedAt       sql.NullTime
	DeactivatedAt     sql.NullTime
	DeactivatedReason sql.NullString
	Notes             sql.NullString
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// ShadowDiffRow mirrors promotion_shadow_diffs.
type ShadowDiffRow struct {
	ID             string
	PromotionID    string
	TradingDate    time.Time
	ShadowDecision json.RawMessage
	ActiveDecision json.RawMessage
	Agreement      bool
	CreatedAt      time.Time
}

// HealthSnapshotRow mirrors promotion_health_snapshots.
type HealthSnapshotRow struct {
	ID                string
	PromotionID       string
	SnapshotAt        time.Time
	WindowDays        int
	ActualSharpe      sql.NullFloat64
	ActualReturn      sql.NullFloat64
	ActualMaxDrawdown sql.NullFloat64
	ActualTradeCount  int
	SharpeDecayRatio  sql.NullFloat64
	DecayFlag         bool
	Notes             sql.NullString
}

// PromotionEventRow mirrors promotion_events.
type PromotionEventRow struct {
	ID          string
	PromotionID string
	EventType   string
	ActorUserID sql.NullString
	Payload     json.RawMessage
	CreatedAt   time.Time
}

// Insert persists a new promotion in pending_review status. The
// caller is expected to have validated the row at the domain
// layer; here we only protect against shape errors.
func (r *PromotionRepo) Insert(ctx context.Context, row *PromotionRow) error {
	if row == nil {
		return errors.New("promotion_repo: nil row")
	}
	if strings.TrimSpace(row.ID) == "" || strings.TrimSpace(row.FundID) == "" {
		return errors.New("promotion_repo: id and fund_id required")
	}
	params, err := normaliseJSON(row.EngineParams)
	if err != nil {
		return fmt.Errorf("promotion_repo: marshal params: %w", err)
	}
	baseline, err := normaliseJSON(row.BaselineMetrics)
	if err != nil {
		return fmt.Errorf("promotion_repo: marshal baseline: %w", err)
	}
	_, err = r.db.ExecContext(ctx, `
INSERT INTO strategy_promotions
    (id, fund_id, proposed_by, basis_job_id, engine_kind,
     engine_params, baseline_metrics, status, shadow_days, decay_ratio,
     notes, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		row.ID, row.FundID, row.ProposedBy, row.BasisJobID, row.EngineKind,
		params, baseline, row.Status, row.ShadowDays, row.DecayRatio,
		row.Notes, row.CreatedAt, row.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("promotion_repo: insert: %w", err)
	}
	return nil
}

// UpdateStatus writes the new status + the audit timestamps that
// belong to that transition. Callers pass nullable fields as
// sql.Null* so a no-op stays a no-op (no clobbering existing
// approved_at when, say, transitioning shadow → active).
//
// We use partial UPDATE (only set the columns that matter for
// the transition) so the caller doesn't have to round-trip the
// full row.
type StatusUpdate struct {
	Status            string
	ApprovedBy        sql.NullString
	ApprovedAt        sql.NullTime
	RejectedBy        sql.NullString
	RejectedAt        sql.NullTime
	RejectedReason    sql.NullString
	ShadowStartedAt   sql.NullTime
	ShadowCompletedAt sql.NullTime
	ActivatedAt       sql.NullTime
	DeactivatedAt     sql.NullTime
	DeactivatedReason sql.NullString
}

func (r *PromotionRepo) UpdateStatus(ctx context.Context, id string, upd StatusUpdate) error {
	if id == "" {
		return errors.New("promotion_repo: id required")
	}
	res, err := r.db.ExecContext(ctx, `
UPDATE strategy_promotions SET
    status              = $1,
    approved_by         = COALESCE($2, approved_by),
    approved_at         = COALESCE($3, approved_at),
    rejected_by         = COALESCE($4, rejected_by),
    rejected_at         = COALESCE($5, rejected_at),
    rejected_reason     = COALESCE($6, rejected_reason),
    shadow_started_at   = COALESCE($7, shadow_started_at),
    shadow_completed_at = COALESCE($8, shadow_completed_at),
    activated_at        = COALESCE($9, activated_at),
    deactivated_at      = COALESCE($10, deactivated_at),
    deactivated_reason  = COALESCE($11, deactivated_reason),
    updated_at          = NOW()
WHERE id = $12`,
		upd.Status,
		upd.ApprovedBy, upd.ApprovedAt,
		upd.RejectedBy, upd.RejectedAt, upd.RejectedReason,
		upd.ShadowStartedAt, upd.ShadowCompletedAt,
		upd.ActivatedAt, upd.DeactivatedAt, upd.DeactivatedReason,
		id,
	)
	if err != nil {
		return fmt.Errorf("promotion_repo: update status: %w", err)
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// Get fetches a single promotion. Returns ErrNotFound when the
// row is missing.
func (r *PromotionRepo) Get(ctx context.Context, id string) (*PromotionRow, error) {
	row := r.db.QueryRowContext(ctx, promotionSelect+` WHERE id = $1`, id)
	return scanPromotion(row)
}

// GetActiveByFund returns the (at-most-one) row currently in
// status='active' for the given fund. Used by the
// ProductionEngineResolver to pick the engine each PMAgent run.
//
// nil-with-nil-error means "no active promotion" — the resolver
// falls back to the fund's default engine in that case.
func (r *PromotionRepo) GetActiveByFund(ctx context.Context, fundID string) (*PromotionRow, error) {
	row := r.db.QueryRowContext(ctx, promotionSelect+` WHERE fund_id = $1 AND status = 'active'`, fundID)
	p, err := scanPromotion(row)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return p, err
}

// ListByFund returns recent promotions for a fund, newest first.
// limit defaults to 50 when ≤ 0.
func (r *PromotionRepo) ListByFund(ctx context.Context, fundID string, limit int) ([]*PromotionRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		promotionSelect+` WHERE fund_id = $1 ORDER BY created_at DESC LIMIT $2`,
		fundID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("promotion_repo: list: %w", err)
	}
	defer rows.Close()
	out := []*PromotionRow{}
	for rows.Next() {
		p, err := scanPromotion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListLive returns every promotion currently in shadow OR active
// status across all funds. The decay-monitor scheduler iterates
// this list to know which fund/promotion pairs to sample.
func (r *PromotionRepo) ListLive(ctx context.Context) ([]*PromotionRow, error) {
	rows, err := r.db.QueryContext(ctx,
		promotionSelect+` WHERE status IN ('shadow','active') ORDER BY activated_at DESC NULLS LAST, created_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("promotion_repo: list live: %w", err)
	}
	defer rows.Close()
	out := []*PromotionRow{}
	for rows.Next() {
		p, err := scanPromotion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// InsertEvent appends to the audit log.
func (r *PromotionRepo) InsertEvent(ctx context.Context, row *PromotionEventRow) error {
	if row == nil || row.ID == "" || row.PromotionID == "" {
		return errors.New("promotion_repo: event row missing required fields")
	}
	payload, err := normaliseJSON(row.Payload)
	if err != nil {
		return fmt.Errorf("promotion_repo: marshal event payload: %w", err)
	}
	_, err = r.db.ExecContext(ctx, `
INSERT INTO promotion_events (id, promotion_id, event_type, actor_user_id, payload, created_at)
VALUES ($1, $2, $3, $4, $5, $6)`,
		row.ID, row.PromotionID, row.EventType, row.ActorUserID, payload, row.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("promotion_repo: insert event: %w", err)
	}
	return nil
}

// ListEvents fetches the audit log for a promotion, oldest first
// (so the UI renders them as a chronological timeline).
func (r *PromotionRepo) ListEvents(ctx context.Context, promotionID string) ([]*PromotionEventRow, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT id, promotion_id, event_type, actor_user_id, payload, created_at
  FROM promotion_events
 WHERE promotion_id = $1
 ORDER BY created_at ASC`, promotionID)
	if err != nil {
		return nil, fmt.Errorf("promotion_repo: list events: %w", err)
	}
	defer rows.Close()
	out := []*PromotionEventRow{}
	for rows.Next() {
		var ev PromotionEventRow
		var payload []byte
		if err := rows.Scan(&ev.ID, &ev.PromotionID, &ev.EventType, &ev.ActorUserID, &payload, &ev.CreatedAt); err != nil {
			return nil, fmt.Errorf("promotion_repo: scan event: %w", err)
		}
		if len(payload) > 0 && string(payload) != "null" {
			ev.Payload = json.RawMessage(payload)
		}
		out = append(out, &ev)
	}
	return out, rows.Err()
}

// UpsertShadowDiff writes (or replaces) the per-day shadow vs
// active comparison. The (promotion_id, trading_date) unique
// index makes the upsert deterministic — re-running the
// comparator for the same day overwrites the previous row
// instead of duplicating it.
func (r *PromotionRepo) UpsertShadowDiff(ctx context.Context, row *ShadowDiffRow) error {
	if row == nil || row.ID == "" || row.PromotionID == "" {
		return errors.New("promotion_repo: shadow diff missing required fields")
	}
	shadow, err := normaliseJSON(row.ShadowDecision)
	if err != nil {
		return fmt.Errorf("promotion_repo: marshal shadow decision: %w", err)
	}
	active, err := normaliseJSON(row.ActiveDecision)
	if err != nil {
		return fmt.Errorf("promotion_repo: marshal active decision: %w", err)
	}
	_, err = r.db.ExecContext(ctx, `
INSERT INTO promotion_shadow_diffs
    (id, promotion_id, trading_date, shadow_decision, active_decision, agreement, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (promotion_id, trading_date) DO UPDATE SET
    shadow_decision = EXCLUDED.shadow_decision,
    active_decision = EXCLUDED.active_decision,
    agreement       = EXCLUDED.agreement,
    created_at      = EXCLUDED.created_at`,
		row.ID, row.PromotionID, row.TradingDate, shadow, active, row.Agreement, row.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("promotion_repo: upsert shadow diff: %w", err)
	}
	return nil
}

// ListShadowDiffs returns the trailing N rows for a promotion,
// newest first. limit ≤ 0 defaults to 50.
func (r *PromotionRepo) ListShadowDiffs(ctx context.Context, promotionID string, limit int) ([]*ShadowDiffRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT id, promotion_id, trading_date, shadow_decision, active_decision, agreement, created_at
  FROM promotion_shadow_diffs
 WHERE promotion_id = $1
 ORDER BY trading_date DESC
 LIMIT $2`, promotionID, limit)
	if err != nil {
		return nil, fmt.Errorf("promotion_repo: list shadow diffs: %w", err)
	}
	defer rows.Close()
	out := []*ShadowDiffRow{}
	for rows.Next() {
		var d ShadowDiffRow
		var sh, ac []byte
		if err := rows.Scan(&d.ID, &d.PromotionID, &d.TradingDate, &sh, &ac, &d.Agreement, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("promotion_repo: scan shadow diff: %w", err)
		}
		if len(sh) > 0 {
			d.ShadowDecision = json.RawMessage(sh)
		}
		if len(ac) > 0 {
			d.ActiveDecision = json.RawMessage(ac)
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}

// InsertHealthSnapshot appends one decay-monitor sample.
func (r *PromotionRepo) InsertHealthSnapshot(ctx context.Context, row *HealthSnapshotRow) error {
	if row == nil || row.ID == "" || row.PromotionID == "" {
		return errors.New("promotion_repo: health snapshot missing required fields")
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO promotion_health_snapshots
    (id, promotion_id, snapshot_at, window_days,
     actual_sharpe, actual_return, actual_max_drawdown,
     actual_trade_count, sharpe_decay_ratio, decay_flag, notes)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		row.ID, row.PromotionID, row.SnapshotAt, row.WindowDays,
		row.ActualSharpe, row.ActualReturn, row.ActualMaxDrawdown,
		row.ActualTradeCount, row.SharpeDecayRatio, row.DecayFlag, row.Notes,
	)
	if err != nil {
		return fmt.Errorf("promotion_repo: insert health: %w", err)
	}
	return nil
}

// ListHealthSnapshots returns the trailing N samples for a
// promotion, newest first.
func (r *PromotionRepo) ListHealthSnapshots(ctx context.Context, promotionID string, limit int) ([]*HealthSnapshotRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT id, promotion_id, snapshot_at, window_days,
       actual_sharpe, actual_return, actual_max_drawdown,
       actual_trade_count, sharpe_decay_ratio, decay_flag, notes
  FROM promotion_health_snapshots
 WHERE promotion_id = $1
 ORDER BY snapshot_at DESC
 LIMIT $2`, promotionID, limit)
	if err != nil {
		return nil, fmt.Errorf("promotion_repo: list health: %w", err)
	}
	defer rows.Close()
	out := []*HealthSnapshotRow{}
	for rows.Next() {
		var h HealthSnapshotRow
		if err := rows.Scan(
			&h.ID, &h.PromotionID, &h.SnapshotAt, &h.WindowDays,
			&h.ActualSharpe, &h.ActualReturn, &h.ActualMaxDrawdown,
			&h.ActualTradeCount, &h.SharpeDecayRatio, &h.DecayFlag, &h.Notes,
		); err != nil {
			return nil, fmt.Errorf("promotion_repo: scan health: %w", err)
		}
		out = append(out, &h)
	}
	return out, rows.Err()
}

// --- helpers ---

// promotionSelect is the column list shared by every promotion
// read so adding a column is one edit instead of N.
const promotionSelect = `
SELECT id, fund_id, proposed_by, basis_job_id, engine_kind,
       engine_params, baseline_metrics, status, shadow_days, decay_ratio,
       approved_by, approved_at,
       rejected_by, rejected_at, rejected_reason,
       shadow_started_at, shadow_completed_at,
       activated_at, deactivated_at, deactivated_reason,
       notes, created_at, updated_at
  FROM strategy_promotions`

func scanPromotion(s rowScanner) (*PromotionRow, error) {
	var p PromotionRow
	var params, baseline []byte
	err := s.Scan(
		&p.ID, &p.FundID, &p.ProposedBy, &p.BasisJobID, &p.EngineKind,
		&params, &baseline, &p.Status, &p.ShadowDays, &p.DecayRatio,
		&p.ApprovedBy, &p.ApprovedAt,
		&p.RejectedBy, &p.RejectedAt, &p.RejectedReason,
		&p.ShadowStartedAt, &p.ShadowCompletedAt,
		&p.ActivatedAt, &p.DeactivatedAt, &p.DeactivatedReason,
		&p.Notes, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("promotion_repo: scan: %w", err)
	}
	if len(params) > 0 {
		p.EngineParams = json.RawMessage(params)
	}
	if len(baseline) > 0 {
		p.BaselineMetrics = json.RawMessage(baseline)
	}
	return &p, nil
}
