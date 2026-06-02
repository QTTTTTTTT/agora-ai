package brinson

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Repo persists benchmark compositions (admin-managed) and
// attribution snapshots (append-only archive).
type Repo struct {
	db *sql.DB
}

func NewRepo(db *sql.DB) *Repo { return &Repo{db: db} }

// ErrNotFound is returned when GetComposition / GetLatestComposition
// can't find a matching row.
var ErrNotFound = errors.New("brinson: not found")

// CompositionRow is the full row representation including admin
// metadata. The Composition struct alone doesn't carry the row
// id / timestamps.
type CompositionRow struct {
	ID          string
	BenchmarkID string
	Dimension   BucketDimension
	AsOf        time.Time
	Buckets     []Bucket
	Note        string
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// AsComposition projects the row into the engine input shape.
func (r CompositionRow) AsComposition() Composition {
	return Composition{
		BenchmarkID: r.BenchmarkID,
		Dimension:   r.Dimension,
		AsOf:        r.AsOf,
		Buckets:     r.Buckets,
		Note:        r.Note,
	}
}

// ListCompositionsParams filters the admin listing.
type ListCompositionsParams struct {
	BenchmarkID string
	Dimension   BucketDimension
	Limit       int
}

// ListCompositions returns rows ordered by asof DESC.
func (r *Repo) ListCompositions(ctx context.Context, p ListCompositionsParams) ([]CompositionRow, error) {
	conds := []string{}
	args := []interface{}{}
	if strings.TrimSpace(p.BenchmarkID) != "" {
		args = append(args, p.BenchmarkID)
		conds = append(conds, fmt.Sprintf("benchmark_id = $%d", len(args)))
	}
	if p.Dimension != "" {
		if !p.Dimension.IsValid() {
			return nil, fmt.Errorf("brinson: invalid dimension %q", p.Dimension)
		}
		args = append(args, string(p.Dimension))
		conds = append(conds, fmt.Sprintf("bucket_dimension = $%d", len(args)))
	}
	q := `SELECT id, benchmark_id, bucket_dimension, asof, buckets, note,
	             created_by, created_at, updated_at
	        FROM brinson_benchmark_compositions`
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY benchmark_id, bucket_dimension, asof DESC"
	limit := p.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	args = append(args, limit)
	q += fmt.Sprintf(" LIMIT $%d", len(args))

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("brinson: list compositions: %w", err)
	}
	defer rows.Close()
	var out []CompositionRow
	for rows.Next() {
		var row CompositionRow
		var dim string
		var bucketsJSON []byte
		var createdBy sql.NullString
		if err := rows.Scan(&row.ID, &row.BenchmarkID, &dim, &row.AsOf, &bucketsJSON, &row.Note,
			&createdBy, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, fmt.Errorf("brinson: scan composition: %w", err)
		}
		row.Dimension = BucketDimension(dim)
		if createdBy.Valid {
			row.CreatedBy = createdBy.String
		}
		if len(bucketsJSON) > 0 {
			if err := json.Unmarshal(bucketsJSON, &row.Buckets); err != nil {
				return nil, fmt.Errorf("brinson: decode buckets: %w", err)
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("brinson: iter compositions: %w", err)
	}
	return out, nil
}

// GetLatestComposition returns the most-recent composition row for
// (benchmark_id, dimension). Used by the per-fund handler when
// the operator doesn't pick a specific composition.
func (r *Repo) GetLatestComposition(ctx context.Context, benchmarkID string, dim BucketDimension) (CompositionRow, error) {
	if strings.TrimSpace(benchmarkID) == "" {
		return CompositionRow{}, errors.New("brinson: benchmark_id required")
	}
	if !dim.IsValid() {
		return CompositionRow{}, fmt.Errorf("brinson: invalid dimension %q", dim)
	}
	var row CompositionRow
	var dimStr string
	var bucketsJSON []byte
	var createdBy sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT id, benchmark_id, bucket_dimension, asof, buckets, note,
		        created_by, created_at, updated_at
		   FROM brinson_benchmark_compositions
		  WHERE benchmark_id = $1 AND bucket_dimension = $2
		  ORDER BY asof DESC
		  LIMIT 1`,
		benchmarkID, string(dim),
	).Scan(&row.ID, &row.BenchmarkID, &dimStr, &row.AsOf, &bucketsJSON, &row.Note,
		&createdBy, &row.CreatedAt, &row.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return CompositionRow{}, ErrNotFound
	}
	if err != nil {
		return CompositionRow{}, fmt.Errorf("brinson: get latest composition: %w", err)
	}
	row.Dimension = BucketDimension(dimStr)
	if createdBy.Valid {
		row.CreatedBy = createdBy.String
	}
	if len(bucketsJSON) > 0 {
		if err := json.Unmarshal(bucketsJSON, &row.Buckets); err != nil {
			return CompositionRow{}, fmt.Errorf("brinson: decode buckets: %w", err)
		}
	}
	return row, nil
}

// UpsertComposition inserts or updates by (benchmark_id, dimension,
// asof). createdBy is optional.
func (r *Repo) UpsertComposition(ctx context.Context, row CompositionRow, createdBy string) (CompositionRow, error) {
	if err := row.AsComposition().Validate(); err != nil {
		return CompositionRow{}, err
	}
	bucketsJSON, err := json.Marshal(row.Buckets)
	if err != nil {
		return CompositionRow{}, fmt.Errorf("brinson: encode buckets: %w", err)
	}
	var createdByArg interface{}
	if strings.TrimSpace(createdBy) != "" {
		createdByArg = createdBy
	}
	out := CompositionRow{}
	var dimStr string
	var bucketsBytes []byte
	var cb sql.NullString
	q := `INSERT INTO brinson_benchmark_compositions
	         (benchmark_id, bucket_dimension, asof, buckets, note, created_by)
	      VALUES ($1, $2, $3, $4::jsonb, $5, $6)
	      ON CONFLICT (benchmark_id, bucket_dimension, asof) DO UPDATE
	        SET buckets    = EXCLUDED.buckets,
	            note       = EXCLUDED.note,
	            updated_at = NOW()
	      RETURNING id, benchmark_id, bucket_dimension, asof, buckets, note,
	                created_by, created_at, updated_at`
	if err := r.db.QueryRowContext(ctx, q,
		row.BenchmarkID, string(row.Dimension), row.AsOf, bucketsJSON, row.Note, createdByArg,
	).Scan(&out.ID, &out.BenchmarkID, &dimStr, &out.AsOf, &bucketsBytes, &out.Note,
		&cb, &out.CreatedAt, &out.UpdatedAt); err != nil {
		return CompositionRow{}, fmt.Errorf("brinson: upsert composition: %w", err)
	}
	out.Dimension = BucketDimension(dimStr)
	if cb.Valid {
		out.CreatedBy = cb.String
	}
	if len(bucketsBytes) > 0 {
		_ = json.Unmarshal(bucketsBytes, &out.Buckets)
	}
	return out, nil
}

// DeleteComposition removes a composition by id.
func (r *Repo) DeleteComposition(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("brinson: composition id required")
	}
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM brinson_benchmark_compositions WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("brinson: delete composition: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// AppendSnapshot writes one Result + per-bucket details as a row.
// composition_id links back to the benchmark composition used.
func (r *Repo) AppendSnapshot(ctx context.Context, res Result, fundID, compositionID string) error {
	if strings.TrimSpace(res.BenchmarkID) == "" || strings.TrimSpace(fundID) == "" || strings.TrimSpace(compositionID) == "" {
		return errors.New("brinson: fund_id, benchmark_id, and composition_id required")
	}
	bucketsJSON, err := json.Marshal(res.Buckets)
	if err != nil {
		return fmt.Errorf("brinson: encode buckets: %w", err)
	}
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO brinson_attribution_snapshots (
			fund_id, benchmark_id, bucket_dimension, composition_id, calculated_at,
			allocation_total, selection_total, interaction_total,
			active_return, portfolio_return, benchmark_return,
			bucket_count, bucket_details
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8,
			$9, $10, $11,
			$12, $13::jsonb
		)`,
		fundID, res.BenchmarkID, string(res.Dimension), compositionID, res.GeneratedAt,
		res.AllocationTotal, res.SelectionTotal, res.InteractionTotal,
		res.ActiveReturn, res.PortfolioReturn, res.BenchmarkReturn,
		res.BucketCount, bucketsJSON,
	)
	if err != nil {
		return fmt.Errorf("brinson: insert snapshot: %w", err)
	}
	return nil
}

// ListSnapshotsParams scopes the trend query.
type ListSnapshotsParams struct {
	FundID      string
	BenchmarkID string
	Dimension   BucketDimension
	Limit       int
}

// ListSnapshots returns archive rows newest-first. Used by the
// fund-level trend chart.
type SnapshotRow struct {
	ID                int64
	FundID            string
	BenchmarkID       string
	Dimension         BucketDimension
	CompositionID     string
	CalculatedAt      time.Time
	AllocationTotal   float64
	SelectionTotal    float64
	InteractionTotal  float64
	ActiveReturn      float64
	PortfolioReturn   float64
	BenchmarkReturn   float64
	BucketCount       int
	Buckets           []BucketAttribution
}

func (r *Repo) ListSnapshots(ctx context.Context, p ListSnapshotsParams) ([]SnapshotRow, error) {
	if strings.TrimSpace(p.FundID) == "" {
		return nil, errors.New("brinson: fund_id required")
	}
	conds := []string{"fund_id = $1"}
	args := []interface{}{p.FundID}
	if strings.TrimSpace(p.BenchmarkID) != "" {
		args = append(args, p.BenchmarkID)
		conds = append(conds, fmt.Sprintf("benchmark_id = $%d", len(args)))
	}
	if p.Dimension != "" {
		args = append(args, string(p.Dimension))
		conds = append(conds, fmt.Sprintf("bucket_dimension = $%d", len(args)))
	}
	limit := p.Limit
	if limit <= 0 || limit > 365 {
		limit = 90
	}
	args = append(args, limit)
	q := fmt.Sprintf(`SELECT id, fund_id, benchmark_id, bucket_dimension, composition_id,
	                          calculated_at,
	                          allocation_total, selection_total, interaction_total,
	                          active_return, portfolio_return, benchmark_return,
	                          bucket_count, bucket_details
	                     FROM brinson_attribution_snapshots
	                    WHERE %s
	                    ORDER BY calculated_at DESC
	                    LIMIT $%d`, strings.Join(conds, " AND "), len(args))
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("brinson: list snapshots: %w", err)
	}
	defer rows.Close()
	var out []SnapshotRow
	for rows.Next() {
		var row SnapshotRow
		var dim string
		var bucketsJSON []byte
		if err := rows.Scan(&row.ID, &row.FundID, &row.BenchmarkID, &dim, &row.CompositionID,
			&row.CalculatedAt,
			&row.AllocationTotal, &row.SelectionTotal, &row.InteractionTotal,
			&row.ActiveReturn, &row.PortfolioReturn, &row.BenchmarkReturn,
			&row.BucketCount, &bucketsJSON); err != nil {
			return nil, fmt.Errorf("brinson: scan snapshot: %w", err)
		}
		row.Dimension = BucketDimension(dim)
		if len(bucketsJSON) > 0 {
			_ = json.Unmarshal(bucketsJSON, &row.Buckets)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("brinson: iter snapshots: %w", err)
	}
	return out, nil
}
