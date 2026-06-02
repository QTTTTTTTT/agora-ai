// repo.go — DB-backed factor-loading + snapshot storage.
//
// Two surfaces:
//
//   * Loadings — the instrument_factor_loadings table, keyed by
//     (instrument_key, factor, asof). The hot path is "give me
//     the freshest loading for each (instrument, factor) the
//     portfolio needs"; one query per portfolio call, scanned
//     in-memory by the engine.
//
//   * Snapshots — the portfolio_factor_snapshots append-only
//     archive. Snapshots are written in a single transaction
//     (six rows per fund per snapshot, one per canonical
//     factor) so trend-line UIs never see a half-written
//     vintage.

package factorexposure

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

// Repo wraps the two tables.
type Repo struct {
	db *sql.DB
}

// NewRepo constructs a Repo. nil db is rejected at first call.
func NewRepo(db *sql.DB) *Repo { return &Repo{db: db} }

// LoadingsByInstruments returns the latest-as-of-T loading for
// each (instrument, factor) in instrumentKeys, ignoring rows
// whose asof is in the future. Missing rows are simply absent
// from the map; callers MUST treat absence as "no loading", not
// "zero loading".
//
// O(1) round trips: one query with WHERE instrument_key = ANY($1).
// The instrument_factor_loadings_latest_idx serves the
// (instrument, factor, asof DESC) ordering used by the DISTINCT
// ON projection.
func (r *Repo) LoadingsByInstruments(ctx context.Context, instrumentKeys []string, asOf time.Time) (map[LoadingKey]InstrumentLoading, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("factorexposure: nil db")
	}
	out := make(map[LoadingKey]InstrumentLoading, len(instrumentKeys)*len(AllFactors))
	if len(instrumentKeys) == 0 {
		return out, nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT DISTINCT ON (instrument_key, factor)
		       instrument_key, factor, asof, loading, source, note, updated_at
		  FROM instrument_factor_loadings
		 WHERE instrument_key = ANY($1)
		   AND asof <= $2
		 ORDER BY instrument_key, factor, asof DESC
	`, pq.Array(instrumentKeys), asOf)
	if err != nil {
		return nil, fmt.Errorf("factorexposure: query loadings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			rec  InstrumentLoading
			fact string
			src  string
		)
		if err := rows.Scan(&rec.InstrumentKey, &fact, &rec.AsOf, &rec.Loading, &src, &rec.Note, &rec.UpdatedAt); err != nil {
			return nil, fmt.Errorf("factorexposure: scan loading: %w", err)
		}
		rec.Factor = Factor(fact)
		rec.Source = LoadingSource(src)
		out[LoadingKey{InstrumentKey: rec.InstrumentKey, Factor: rec.Factor}] = rec
	}
	return out, rows.Err()
}

// ListLoadings is the admin browser query: returns rows matching
// the filter, optionally limited to one factor. Limit is clamped
// to [1, 1000] with a default of 200 to keep responses bounded.
type ListLoadingsFilter struct {
	Factor        Factor // empty = all factors
	InstrumentKey string // empty = all instruments
	Limit         int
	Offset        int
}

func (r *Repo) ListLoadings(ctx context.Context, f ListLoadingsFilter) ([]InstrumentLoading, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("factorexposure: nil db")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	args := []any{}
	conds := []string{}
	idx := 1
	if f.Factor != "" {
		if !f.Factor.IsValid() {
			return nil, fmt.Errorf("factorexposure: invalid factor %q", f.Factor)
		}
		conds = append(conds, fmt.Sprintf("factor = $%d", idx))
		args = append(args, string(f.Factor))
		idx++
	}
	if k := strings.TrimSpace(f.InstrumentKey); k != "" {
		conds = append(conds, fmt.Sprintf("instrument_key = $%d", idx))
		args = append(args, k)
		idx++
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	q := fmt.Sprintf(`
		SELECT instrument_key, factor, asof, loading, source, note, updated_at
		  FROM instrument_factor_loadings
		  %s
		 ORDER BY instrument_key, factor, asof DESC
		 LIMIT %d OFFSET %d
	`, where, limit, f.Offset)
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("factorexposure: list loadings: %w", err)
	}
	defer rows.Close()
	out := make([]InstrumentLoading, 0, limit)
	for rows.Next() {
		var (
			rec  InstrumentLoading
			fact string
			src  string
		)
		if err := rows.Scan(&rec.InstrumentKey, &fact, &rec.AsOf, &rec.Loading, &src, &rec.Note, &rec.UpdatedAt); err != nil {
			return nil, fmt.Errorf("factorexposure: scan loading: %w", err)
		}
		rec.Factor = Factor(fact)
		rec.Source = LoadingSource(src)
		out = append(out, rec)
	}
	return out, rows.Err()
}

// UpsertLoading writes one (instrument, factor, asof) row. The
// PK (instrument_key, factor, asof) collapses repeated writes for
// the same vintage; the engine reads "latest asof <= today" so
// historical vintages remain useful.
func (r *Repo) UpsertLoading(ctx context.Context, rec InstrumentLoading) error {
	if r == nil || r.db == nil {
		return errors.New("factorexposure: nil db")
	}
	if !rec.Factor.IsValid() {
		return fmt.Errorf("factorexposure: invalid factor %q", rec.Factor)
	}
	if !rec.Source.IsValid() {
		return fmt.Errorf("factorexposure: invalid source %q", rec.Source)
	}
	if strings.TrimSpace(rec.InstrumentKey) == "" {
		return errors.New("factorexposure: instrument_key required")
	}
	if rec.AsOf.IsZero() {
		return errors.New("factorexposure: asof required")
	}
	if rec.Loading < -10 || rec.Loading > 10 {
		return fmt.Errorf("factorexposure: loading %f outside allowed range [-10, 10]", rec.Loading)
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO instrument_factor_loadings
			(instrument_key, factor, asof, loading, source, note, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
		ON CONFLICT (instrument_key, factor, asof) DO UPDATE
			SET loading = EXCLUDED.loading,
			    source = EXCLUDED.source,
			    note = EXCLUDED.note,
			    updated_at = NOW()
	`, rec.InstrumentKey, string(rec.Factor), rec.AsOf, rec.Loading, string(rec.Source), rec.Note)
	return err
}

// DeleteLoading drops one (instrument, factor, asof) row. Used by
// the admin "this calibration was wrong" workflow; the audit log
// captures who removed which row.
func (r *Repo) DeleteLoading(ctx context.Context, instrumentKey string, factor Factor, asof time.Time) error {
	if r == nil || r.db == nil {
		return errors.New("factorexposure: nil db")
	}
	if !factor.IsValid() {
		return fmt.Errorf("factorexposure: invalid factor %q", factor)
	}
	_, err := r.db.ExecContext(ctx, `
		DELETE FROM instrument_factor_loadings
		 WHERE instrument_key = $1 AND factor = $2 AND asof = $3
	`, instrumentKey, string(factor), asof)
	return err
}

// AppendSnapshot persists one Snapshot as six rows. Wrapped in a
// transaction so trend-line readers never see a half-written
// vintage. fundID is taken from the snapshot to avoid
// inconsistency between the Snapshot.FundID and the caller's
// intent.
func (r *Repo) AppendSnapshot(ctx context.Context, snap Snapshot) error {
	if r == nil || r.db == nil {
		return errors.New("factorexposure: nil db")
	}
	if strings.TrimSpace(snap.FundID) == "" {
		return errors.New("factorexposure: snapshot.FundID required")
	}
	if len(snap.Exposures) == 0 {
		return errors.New("factorexposure: snapshot.Exposures empty")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, row := range snap.Exposures {
		if !row.Factor.IsValid() {
			return fmt.Errorf("factorexposure: snapshot has invalid factor %q", row.Factor)
		}
		asof := row.LoadingsAsOf
		if asof.IsZero() {
			// No coverage for this factor; persist with today so
			// the CHECK constraints don't reject a zero date.
			asof = snap.GeneratedAt
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO portfolio_factor_snapshots
				(fund_id, calculated_at, factor, net_exposure, gross_exposure,
				 capital_pct, holding_count, loadings_asof)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`,
			snap.FundID, snap.GeneratedAt, string(row.Factor),
			row.NetExposure, row.GrossExposure,
			row.CapitalPct, row.HoldingCount, asof,
		)
		if err != nil {
			return fmt.Errorf("factorexposure: insert snapshot row %s: %w", row.Factor, err)
		}
	}
	return tx.Commit()
}

// SnapshotPoint is one row from the trend-line API. Used to plot
// "how has our momentum exposure evolved over the last 30 days?"
type SnapshotPoint struct {
	CalculatedAt  time.Time
	Factor        Factor
	NetExposure   float64
	GrossExposure float64
	CapitalPct    float64
	HoldingCount  int
	LoadingsAsOf  time.Time
}

// ListSnapshots returns recent snapshot rows for a fund, optionally
// filtered to one factor. Sorted DESC by calculated_at so the
// chart can render newest-first without re-sorting.
func (r *Repo) ListSnapshots(ctx context.Context, fundID string, factor Factor, limit int) ([]SnapshotPoint, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("factorexposure: nil db")
	}
	if strings.TrimSpace(fundID) == "" {
		return nil, errors.New("factorexposure: fund_id required")
	}
	if limit <= 0 {
		limit = 60
	}
	if limit > 1000 {
		limit = 1000
	}
	args := []any{fundID}
	cond := ""
	if factor != "" {
		if !factor.IsValid() {
			return nil, fmt.Errorf("factorexposure: invalid factor %q", factor)
		}
		cond = "AND factor = $2"
		args = append(args, string(factor))
	}
	q := fmt.Sprintf(`
		SELECT calculated_at, factor, net_exposure, gross_exposure,
		       capital_pct, holding_count, loadings_asof
		  FROM portfolio_factor_snapshots
		 WHERE fund_id = $1 %s
		 ORDER BY calculated_at DESC, factor
		 LIMIT %d
	`, cond, limit)
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("factorexposure: query snapshots: %w", err)
	}
	defer rows.Close()
	out := make([]SnapshotPoint, 0, limit)
	for rows.Next() {
		var (
			rec  SnapshotPoint
			fact string
		)
		if err := rows.Scan(&rec.CalculatedAt, &fact, &rec.NetExposure, &rec.GrossExposure,
			&rec.CapitalPct, &rec.HoldingCount, &rec.LoadingsAsOf); err != nil {
			return nil, fmt.Errorf("factorexposure: scan snapshot: %w", err)
		}
		rec.Factor = Factor(fact)
		out = append(out, rec)
	}
	return out, rows.Err()
}
