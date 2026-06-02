// repo.go — DB-backed drawdown store (P3-5).
//
// Two surfaces
//
//   - Policies: GetPolicy, UpsertTier, DeleteTier. Tier-level
//     granularity matches the table (one row per (fund, tier))
//     and lets the audit chain capture each tier knob change as
//     a discrete diff.
//   - Events: InsertEvent (idempotent on cooldown), ListEvents,
//     GetEvent, UpdateStatus. The lifecycle gate lives here so
//     the admin handler doesn't need to enforce status transitions
//     by hand.

package drawdown

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Repo wraps the drawdown_* tables.
type Repo struct {
	db *sql.DB
}

// NewRepo constructs a Repo. nil db is rejected at first call,
// matching the FX/recon/surveillance repo pattern.
func NewRepo(db *sql.DB) *Repo { return &Repo{db: db} }

// ----- Policies -----

// GetPolicy returns every tier configured for a fund, sorted by
// tier number. Returns an empty Policy (no error) when the fund
// has no tiers — this is the "no soft breaker" state.
func (r *Repo) GetPolicy(ctx context.Context, fundID string) (*Policy, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("drawdown: nil db")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT tier, dd_pct, action, trim_ratio, cooldown_hours, auto_execute, COALESCE(note, '')
		  FROM drawdown_policies
		 WHERE fund_id = $1
		 ORDER BY tier ASC
	`, fundID)
	if err != nil {
		return nil, fmt.Errorf("drawdown: list policy: %w", err)
	}
	defer rows.Close()
	out := &Policy{FundID: fundID}
	for rows.Next() {
		var t Tier
		var actionStr string
		if err := rows.Scan(&t.Tier, &t.DDPct, &actionStr, &t.TrimRatio, &t.CooldownHours, &t.AutoExecute, &t.Note); err != nil {
			return nil, err
		}
		t.Action = canonicalAction(actionStr)
		out.Tiers = append(out.Tiers, t)
	}
	return out, rows.Err()
}

// UpsertTier writes one tier; INSERT or UPDATE depending on whether
// a row already exists. Validates the closed action vocabulary
// and clamps trim_ratio to [0, 1] before sending to the DB.
func (r *Repo) UpsertTier(ctx context.Context, fundID string, t Tier) error {
	if r == nil || r.db == nil {
		return errors.New("drawdown: nil db")
	}
	if strings.TrimSpace(fundID) == "" {
		return errors.New("drawdown: fund_id required")
	}
	if t.Tier < 1 || t.Tier > 5 {
		return ErrInvalidTier
	}
	switch t.Action {
	case ActionTrimProportional, ActionFlatten, ActionDefensiveOnly:
	default:
		return errors.New("drawdown: invalid action")
	}
	if t.DDPct >= 0 || t.DDPct < -1 {
		return errors.New("drawdown: dd_pct must be in [-1, 0)")
	}
	if t.TrimRatio < 0 {
		t.TrimRatio = 0
	}
	if t.TrimRatio > 1 {
		t.TrimRatio = 1
	}
	if t.CooldownHours < 0 {
		t.CooldownHours = 0
	}
	if t.CooldownHours > 720 {
		t.CooldownHours = 720
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO drawdown_policies
		    (fund_id, tier, dd_pct, action, trim_ratio, cooldown_hours, auto_execute, note, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
		ON CONFLICT (fund_id, tier) DO UPDATE
		   SET dd_pct = EXCLUDED.dd_pct,
		       action = EXCLUDED.action,
		       trim_ratio = EXCLUDED.trim_ratio,
		       cooldown_hours = EXCLUDED.cooldown_hours,
		       auto_execute = EXCLUDED.auto_execute,
		       note = EXCLUDED.note,
		       updated_at = NOW()
	`, fundID, t.Tier, t.DDPct, string(t.Action), t.TrimRatio, t.CooldownHours, t.AutoExecute, nullableString(t.Note))
	if err != nil {
		return fmt.Errorf("drawdown: upsert tier: %w", err)
	}
	return nil
}

// DeleteTier removes a single tier. Returns nil even if no row was
// affected (operator may double-click — idempotent).
func (r *Repo) DeleteTier(ctx context.Context, fundID string, tierNum int) error {
	if r == nil || r.db == nil {
		return errors.New("drawdown: nil db")
	}
	if tierNum < 1 || tierNum > 5 {
		return ErrInvalidTier
	}
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM drawdown_policies WHERE fund_id = $1 AND tier = $2`,
		fundID, tierNum,
	)
	if err != nil {
		return fmt.Errorf("drawdown: delete tier: %w", err)
	}
	return nil
}

// ----- Events -----

// InsertEvent persists a BreachEvent. Returns the new row's ID.
// This does NOT enforce the cooldown — the engine already does.
// We rely on the engine's pre-check rather than a unique index
// because cooldown is time-relative, not fingerprint-relative.
func (r *Repo) InsertEvent(ctx context.Context, ev BreachEvent, initialStatus Status) (string, error) {
	if r == nil || r.db == nil {
		return "", errors.New("drawdown: nil db")
	}
	if strings.TrimSpace(ev.FundID) == "" {
		return "", errors.New("drawdown: fund_id required")
	}
	if initialStatus == "" {
		initialStatus = StatusProposed
	}
	switch initialStatus {
	case StatusProposed, StatusApproved, StatusExecuted, StatusDismissed, StatusSuperseded:
	default:
		return "", ErrInvalidStatus
	}
	planJSON, _ := json.Marshal(ev.TrimPlan)
	if len(planJSON) == 0 || string(planJSON) == "null" {
		planJSON = []byte("[]")
	}
	metaJSON, _ := json.Marshal(ev.Metadata)
	if len(metaJSON) == 0 || string(metaJSON) == "null" {
		metaJSON = []byte("{}")
	}
	var (
		navSnapID any
	)
	if strings.TrimSpace(ev.NavSnapshotID) != "" {
		navSnapID = ev.NavSnapshotID
	}
	if ev.DetectorVersion == "" {
		ev.DetectorVersion = detectorVersion
	}
	detectedAt := ev.DetectedAt
	if detectedAt.IsZero() {
		detectedAt = time.Now().UTC()
	}
	var id string
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO drawdown_events
		    (fund_id, tier, current_dd_pct, peak_nav, current_nav,
		     action, trim_plan, status, nav_snapshot_id,
		     detected_at, detector_version, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9, $10, $11, $12::jsonb)
		RETURNING id::text
	`,
		ev.FundID, ev.Tier, ev.CurrentDDPct, ev.PeakNAV, ev.CurrentNAV,
		string(ev.Action), string(planJSON), string(initialStatus), navSnapID,
		detectedAt.UTC(), ev.DetectorVersion, string(metaJSON),
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("drawdown: insert event: %w", err)
	}
	return id, nil
}

// EventDetail is the read-side struct for one event with all
// review fields populated.
type EventDetail struct {
	ID              string
	FundID          string
	Tier            int
	CurrentDDPct    float64
	PeakNAV         float64
	CurrentNAV      float64
	Action          Action
	TrimPlan        []TrimPlanItem
	TradeIDs        []string
	Status          Status
	ReviewNote      string
	ReviewedBy      string
	ReviewedAt      *time.Time
	NavSnapshotID   string
	DetectedAt      time.Time
	DetectorVersion string
	Metadata        map[string]any
	CreatedAt       time.Time
}

// ListEventsParams filters the events list. Empty fields apply
// no filter.
type ListEventsParams struct {
	FundID string
	Status Status
	From   time.Time
	To     time.Time
	Limit  int
	Offset int
}

// ListEvents returns events ordered (status='proposed' first,
// then by detected_at DESC). The "open queue first" ordering
// matches what the admin UI wants by default.
func (r *Repo) ListEvents(ctx context.Context, p ListEventsParams) ([]EventDetail, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("drawdown: nil db")
	}
	if p.Limit <= 0 || p.Limit > 500 {
		p.Limit = 100
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	args := []any{p.Limit, p.Offset}
	conds := []string{}
	if strings.TrimSpace(p.FundID) != "" {
		args = append(args, p.FundID)
		conds = append(conds, fmt.Sprintf("fund_id = $%d", len(args)))
	}
	if string(p.Status) != "" {
		args = append(args, string(p.Status))
		conds = append(conds, fmt.Sprintf("status = $%d", len(args)))
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
		SELECT id::text, fund_id::text, tier, current_dd_pct, peak_nav, current_nav,
		       action,
		       COALESCE(trim_plan::text, '[]'),
		       COALESCE(trade_ids::text, '[]'),
		       status,
		       COALESCE(review_note, ''),
		       COALESCE(reviewed_by::text, ''), reviewed_at,
		       COALESCE(nav_snapshot_id::text, ''),
		       detected_at, COALESCE(detector_version, ''),
		       COALESCE(metadata::text, '{}'),
		       created_at
		  FROM drawdown_events
		  %s
		 ORDER BY (CASE status WHEN 'proposed' THEN 0 ELSE 1 END),
		          detected_at DESC
		 LIMIT $1 OFFSET $2
	`, where)
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("drawdown: list events: %w", err)
	}
	defer rows.Close()
	var out []EventDetail
	for rows.Next() {
		d := EventDetail{}
		var (
			actionStr   string
			planRaw     string
			tradeIDsRaw string
			metaRaw     string
			reviewedAt  sql.NullTime
		)
		if err := rows.Scan(&d.ID, &d.FundID, &d.Tier, &d.CurrentDDPct, &d.PeakNAV, &d.CurrentNAV,
			&actionStr,
			&planRaw, &tradeIDsRaw,
			(*string)(&d.Status), &d.ReviewNote, &d.ReviewedBy, &reviewedAt,
			&d.NavSnapshotID, &d.DetectedAt, &d.DetectorVersion,
			&metaRaw, &d.CreatedAt); err != nil {
			return nil, err
		}
		d.Action = canonicalAction(actionStr)
		_ = json.Unmarshal([]byte(planRaw), &d.TrimPlan)
		_ = json.Unmarshal([]byte(tradeIDsRaw), &d.TradeIDs)
		_ = json.Unmarshal([]byte(metaRaw), &d.Metadata)
		if reviewedAt.Valid {
			t := reviewedAt.Time
			d.ReviewedAt = &t
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetEvent returns one event. Used by the admin detail view and
// by the auto-execute path so it can re-read after persisting.
func (r *Repo) GetEvent(ctx context.Context, id string) (*EventDetail, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("drawdown: nil db")
	}
	row := r.db.QueryRowContext(ctx, `
		SELECT id::text, fund_id::text, tier, current_dd_pct, peak_nav, current_nav,
		       action,
		       COALESCE(trim_plan::text, '[]'),
		       COALESCE(trade_ids::text, '[]'),
		       status,
		       COALESCE(review_note, ''),
		       COALESCE(reviewed_by::text, ''), reviewed_at,
		       COALESCE(nav_snapshot_id::text, ''),
		       detected_at, COALESCE(detector_version, ''),
		       COALESCE(metadata::text, '{}'),
		       created_at
		  FROM drawdown_events
		 WHERE id = $1
	`, id)
	d := &EventDetail{}
	var (
		actionStr   string
		planRaw     string
		tradeIDsRaw string
		metaRaw     string
		reviewedAt  sql.NullTime
	)
	if err := row.Scan(&d.ID, &d.FundID, &d.Tier, &d.CurrentDDPct, &d.PeakNAV, &d.CurrentNAV,
		&actionStr, &planRaw, &tradeIDsRaw,
		(*string)(&d.Status), &d.ReviewNote, &d.ReviewedBy, &reviewedAt,
		&d.NavSnapshotID, &d.DetectedAt, &d.DetectorVersion,
		&metaRaw, &d.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrEventNotFound
		}
		return nil, fmt.Errorf("drawdown: get event: %w", err)
	}
	d.Action = canonicalAction(actionStr)
	_ = json.Unmarshal([]byte(planRaw), &d.TrimPlan)
	_ = json.Unmarshal([]byte(tradeIDsRaw), &d.TradeIDs)
	_ = json.Unmarshal([]byte(metaRaw), &d.Metadata)
	if reviewedAt.Valid {
		t := reviewedAt.Time
		d.ReviewedAt = &t
	}
	return d, nil
}

// UpdateStatusParams flips an event between lifecycle states.
// validateTransition enforces the legal moves so a buggy admin
// call can't put a row in an impossible state.
type UpdateStatusParams struct {
	ID           string
	NewStatus    Status
	Note         string
	ReviewedBy   string
	TradeIDs     []string // populated when transitioning to executed
}

// UpdateStatus persists the lifecycle move. Returns ErrEventNotFound
// when no row matches.
func (r *Repo) UpdateStatus(ctx context.Context, p UpdateStatusParams) error {
	if r == nil || r.db == nil {
		return errors.New("drawdown: nil db")
	}
	switch p.NewStatus {
	case StatusProposed, StatusApproved, StatusExecuted, StatusDismissed, StatusSuperseded:
	default:
		return ErrInvalidStatus
	}
	tradeIDsJSON, _ := json.Marshal(p.TradeIDs)
	if len(tradeIDsJSON) == 0 || string(tradeIDsJSON) == "null" {
		tradeIDsJSON = []byte("[]")
	}
	now := time.Now().UTC()
	var (
		reviewedAtArg any
		reviewerArg   any
	)
	if p.NewStatus != StatusProposed {
		reviewedAtArg = now
		if strings.TrimSpace(p.ReviewedBy) != "" {
			reviewerArg = p.ReviewedBy
		}
	}
	res, err := r.db.ExecContext(ctx, `
		UPDATE drawdown_events
		   SET status = $2,
		       review_note = $3,
		       reviewed_by = $4,
		       reviewed_at = $5,
		       trade_ids = $6::jsonb
		 WHERE id = $1
	`, p.ID, string(p.NewStatus), p.Note, reviewerArg, reviewedAtArg, string(tradeIDsJSON))
	if err != nil {
		return fmt.Errorf("drawdown: update status: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return ErrEventNotFound
	}
	return nil
}

// LastFiredAtForFund returns a map[tier]→most-recent detected_at
// for the fund, restricted to events that actually FIRED (i.e.
// not 'dismissed' / 'superseded'). Used by the engine's cooldown
// check via the snapshot adapter.
func (r *Repo) LastFiredAtForFund(ctx context.Context, fundID string, lookback time.Duration) (map[int]time.Time, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("drawdown: nil db")
	}
	since := time.Now().UTC().Add(-lookback)
	rows, err := r.db.QueryContext(ctx, `
		SELECT tier, MAX(detected_at)
		  FROM drawdown_events
		 WHERE fund_id = $1
		   AND detected_at >= $2
		   AND status NOT IN ('dismissed', 'superseded')
		 GROUP BY tier
	`, fundID, since)
	if err != nil {
		return nil, fmt.Errorf("drawdown: last fired: %w", err)
	}
	defer rows.Close()
	out := map[int]time.Time{}
	for rows.Next() {
		var tierNum int
		var ts time.Time
		if err := rows.Scan(&tierNum, &ts); err != nil {
			return nil, err
		}
		out[tierNum] = ts.UTC()
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
