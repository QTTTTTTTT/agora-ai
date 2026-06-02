// repo.go — DB-backed lockup store.
//
// One table, one repo. The hot query is ListActiveFor —
// invoked by the gate adapter on every sell — which is
// covered by the (fund_id, instrument_key, locked_until)
// partial index defined in 065_position_lockups.sql.

package lockup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Repo wraps the position_lockups table.
type Repo struct {
	db *sql.DB
}

// NewRepo constructs a Repo. nil db is rejected at first call.
func NewRepo(db *sql.DB) *Repo { return &Repo{db: db} }

// ListActiveFor returns active records for (fund, instrument)
// at as_of. "Active" = released_at IS NULL AND locked_until > as_of.
//
// Used by the gate adapter; call sites are on the order hot
// path so the query is intentionally minimal.
func (r *Repo) ListActiveFor(ctx context.Context, fundID, instrumentKey string, asOf time.Time) ([]Record, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("lockup: nil db")
	}
	if strings.TrimSpace(fundID) == "" || strings.TrimSpace(instrumentKey) == "" {
		return nil, errors.New("lockup: fund_id + instrument_key required")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, fund_id, instrument_key, COALESCE(symbol, ''),
		       locked_qty, locked_until,
		       COALESCE(lockup_reason, 'ipo'),
		       source_lot_id, COALESCE(note, ''),
		       released_at, COALESCE(released_reason, ''), released_by,
		       created_by, created_at, updated_at
		  FROM position_lockups
		 WHERE fund_id = $1
		   AND instrument_key = $2
		   AND released_at IS NULL
		   AND locked_until > $3
		 ORDER BY locked_until ASC
	`, fundID, instrumentKey, asOf)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Record, 0, 8)
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	return out, rows.Err()
}

// ListFilter narrows the admin List response.
type ListFilter struct {
	FundID        string
	InstrumentKey string
	// Status filters the active/expired/released axis. Empty =
	// all rows.
	Status string // "" | "active" | "expired" | "released"
	AsOf   time.Time
	Limit  int
	Offset int
}

// List returns all records matching filter. Used by the admin
// UI table.
func (r *Repo) List(ctx context.Context, filter ListFilter) ([]Record, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("lockup: nil db")
	}
	asOf := filter.AsOf
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	var (
		clauses []string
		args    []any
	)
	if filter.FundID != "" {
		args = append(args, filter.FundID)
		clauses = append(clauses, fmt.Sprintf("fund_id = $%d", len(args)))
	}
	if filter.InstrumentKey != "" {
		args = append(args, filter.InstrumentKey)
		clauses = append(clauses, fmt.Sprintf("instrument_key = $%d", len(args)))
	}
	switch strings.ToLower(strings.TrimSpace(filter.Status)) {
	case "active":
		args = append(args, asOf)
		clauses = append(clauses, fmt.Sprintf("released_at IS NULL AND locked_until > $%d", len(args)))
	case "expired":
		args = append(args, asOf)
		clauses = append(clauses, fmt.Sprintf("released_at IS NULL AND locked_until <= $%d", len(args)))
	case "released":
		clauses = append(clauses, "released_at IS NOT NULL")
	}
	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + strings.Join(clauses, " AND ")
	}
	limit := filter.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}
	args = append(args, limit, offset)
	q := `SELECT id, fund_id, instrument_key, COALESCE(symbol, ''),
	             locked_qty, locked_until,
	             COALESCE(lockup_reason, 'ipo'),
	             source_lot_id, COALESCE(note, ''),
	             released_at, COALESCE(released_reason, ''), released_by,
	             created_by, created_at, updated_at
	        FROM position_lockups` + where + fmt.Sprintf(`
	    ORDER BY locked_until ASC
	       LIMIT $%d OFFSET $%d`, len(args)-1, len(args))
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Record, 0, 64)
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	return out, rows.Err()
}

// GetByID returns one record. Returns (nil, nil) when missing.
func (r *Repo) GetByID(ctx context.Context, id string) (*Record, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("lockup: nil db")
	}
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("lockup: id required")
	}
	row := r.db.QueryRowContext(ctx, `
		SELECT id, fund_id, instrument_key, COALESCE(symbol, ''),
		       locked_qty, locked_until,
		       COALESCE(lockup_reason, 'ipo'),
		       source_lot_id, COALESCE(note, ''),
		       released_at, COALESCE(released_reason, ''), released_by,
		       created_by, created_at, updated_at
		  FROM position_lockups
		 WHERE id = $1
	`, id)
	rec, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return rec, err
}

// CreateParams is the create-row input. Required fields are
// validated up front so the SQL constraint isn't the only line
// of defence.
type CreateParams struct {
	FundID        string
	InstrumentKey string
	Symbol        string
	LockedQty     float64
	LockedUntil   time.Time
	Reason        string
	SourceLotID   string
	Note          string
	CreatedBy     string
}

// Create inserts a new record.
func (r *Repo) Create(ctx context.Context, p CreateParams) (*Record, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("lockup: nil db")
	}
	if err := validateCreate(p); err != nil {
		return nil, err
	}
	reason := strings.ToLower(strings.TrimSpace(p.Reason))
	if reason == "" {
		reason = string(ReasonIPO)
	}
	var lotID, createdBy any
	if strings.TrimSpace(p.SourceLotID) != "" {
		lotID = p.SourceLotID
	}
	if strings.TrimSpace(p.CreatedBy) != "" {
		createdBy = p.CreatedBy
	}
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO position_lockups (
			fund_id, instrument_key, symbol,
			locked_qty, locked_until, lockup_reason,
			source_lot_id, note, created_by
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, fund_id, instrument_key, COALESCE(symbol, ''),
		          locked_qty, locked_until,
		          COALESCE(lockup_reason, 'ipo'),
		          source_lot_id, COALESCE(note, ''),
		          released_at, COALESCE(released_reason, ''), released_by,
		          created_by, created_at, updated_at
	`, p.FundID, p.InstrumentKey, p.Symbol,
		p.LockedQty, p.LockedUntil.UTC(), reason,
		lotID, p.Note, createdBy)
	return scanRecord(row)
}

func validateCreate(p CreateParams) error {
	if strings.TrimSpace(p.FundID) == "" {
		return fmt.Errorf("lockup: fund_id required")
	}
	if strings.TrimSpace(p.InstrumentKey) == "" {
		return fmt.Errorf("lockup: instrument_key required")
	}
	if strings.TrimSpace(p.Symbol) == "" {
		return fmt.Errorf("lockup: symbol required")
	}
	if p.LockedQty <= 0 {
		return fmt.Errorf("lockup: locked_qty must be > 0")
	}
	if p.LockedUntil.IsZero() {
		return fmt.Errorf("lockup: locked_until required")
	}
	if !p.LockedUntil.After(time.Now().UTC().Add(-1 * time.Minute)) {
		// One-minute slack so an admin entering "now" doesn't get
		// rejected by skew. Anything substantially in the past is
		// almost certainly a typo (operator picked the wrong year).
		return fmt.Errorf("lockup: locked_until must be in the future")
	}
	if p.Reason != "" && !IsValidReason(p.Reason) {
		return fmt.Errorf("lockup: invalid reason %q", p.Reason)
	}
	return nil
}

// UpdateParams is the patch input. Pointer fields → nil =
// leave alone.
type UpdateParams struct {
	ID          string
	LockedQty   *float64
	LockedUntil *time.Time
	Reason      *string
	Note        *string
	UpdatedBy   string
}

// Update mutates an existing record. Released records cannot
// be re-edited (would corrupt the audit trail); to change a
// released row, create a new one and the old one stays as
// historical evidence.
func (r *Repo) Update(ctx context.Context, p UpdateParams) (*Record, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("lockup: nil db")
	}
	if strings.TrimSpace(p.ID) == "" {
		return nil, errors.New("lockup: id required")
	}
	if p.LockedQty != nil && *p.LockedQty <= 0 {
		return nil, errors.New("lockup: locked_qty must be > 0")
	}
	if p.Reason != nil && *p.Reason != "" && !IsValidReason(*p.Reason) {
		return nil, fmt.Errorf("lockup: invalid reason %q", *p.Reason)
	}
	row := r.db.QueryRowContext(ctx, `
		UPDATE position_lockups
		   SET locked_qty   = COALESCE($2, locked_qty),
		       locked_until = COALESCE($3, locked_until),
		       lockup_reason = COALESCE($4, lockup_reason),
		       note         = COALESCE($5, note),
		       updated_at   = NOW()
		 WHERE id = $1
		   AND released_at IS NULL
		RETURNING id, fund_id, instrument_key, COALESCE(symbol, ''),
		          locked_qty, locked_until,
		          COALESCE(lockup_reason, 'ipo'),
		          source_lot_id, COALESCE(note, ''),
		          released_at, COALESCE(released_reason, ''), released_by,
		          created_by, created_at, updated_at
	`,
		p.ID,
		nullableFloat(p.LockedQty),
		nullableTime(p.LockedUntil),
		nullableString(p.Reason),
		nullableString(p.Note),
	)
	return scanRecord(row)
}

// Release marks the record as early-released. released_at + reason
// are required together.
//
// Returns sql.ErrNoRows if the record doesn't exist or was
// already released — either way the caller should treat it as
// "no-op, already not in the active set".
func (r *Repo) Release(ctx context.Context, id, reason, releasedBy string) (*Record, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("lockup: nil db")
	}
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("lockup: id required")
	}
	if strings.TrimSpace(reason) == "" {
		return nil, errors.New("lockup: release reason required")
	}
	var by any
	if strings.TrimSpace(releasedBy) != "" {
		by = releasedBy
	}
	row := r.db.QueryRowContext(ctx, `
		UPDATE position_lockups
		   SET released_at     = NOW(),
		       released_reason = $2,
		       released_by     = $3,
		       updated_at      = NOW()
		 WHERE id = $1
		   AND released_at IS NULL
		RETURNING id, fund_id, instrument_key, COALESCE(symbol, ''),
		          locked_qty, locked_until,
		          COALESCE(lockup_reason, 'ipo'),
		          source_lot_id, COALESCE(note, ''),
		          released_at, COALESCE(released_reason, ''), released_by,
		          created_by, created_at, updated_at
	`, id, reason, by)
	rec, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	return rec, err
}

// Delete hard-removes a record. Reserved for typo-fix
// scenarios; for compliance-aware removal use Release.
func (r *Repo) Delete(ctx context.Context, id string) error {
	if r == nil || r.db == nil {
		return errors.New("lockup: nil db")
	}
	if strings.TrimSpace(id) == "" {
		return errors.New("lockup: id required")
	}
	res, err := r.db.ExecContext(ctx, `DELETE FROM position_lockups WHERE id = $1`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ----- helpers -----

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRecord(s rowScanner) (*Record, error) {
	var (
		out         Record
		lotID       sql.NullString
		releasedAt  sql.NullTime
		releasedBy  sql.NullString
		createdBy   sql.NullString
		reason      string
	)
	err := s.Scan(
		&out.ID, &out.FundID, &out.InstrumentKey, &out.Symbol,
		&out.LockedQty, &out.LockedUntil,
		&reason,
		&lotID, &out.Note,
		&releasedAt, &out.ReleasedReason, &releasedBy,
		&createdBy, &out.CreatedAt, &out.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	out.Reason = LockupReason(reason)
	if lotID.Valid {
		v := lotID.String
		out.SourceLotID = &v
	}
	if releasedAt.Valid {
		t := releasedAt.Time
		out.ReleasedAt = &t
	}
	if releasedBy.Valid {
		out.ReleasedBy = releasedBy.String
	}
	if createdBy.Valid {
		out.CreatedBy = createdBy.String
	}
	return &out, nil
}

func nullableFloat(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullableTime(p *time.Time) any {
	if p == nil {
		return nil
	}
	return p.UTC()
}

func nullableString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}
