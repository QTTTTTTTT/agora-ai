// repo.go — DB-backed marketimpact store.
//
// One table, one repo. Operations:
//
//   - GetByKey: fetch a single calibration row (engine input).
//   - List: admin view, optional filter by market/asset_class.
//   - Upsert: operator/auto write.
//   - Delete: rare; intended for retiring delisted symbols.

package marketimpact

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Repo wraps the instrument_liquidity table.
type Repo struct {
	db *sql.DB
}

// NewRepo constructs a Repo.
func NewRepo(db *sql.DB) *Repo { return &Repo{db: db} }

// GetByKey returns the calibration row for an instrument.
// Returns (nil, nil) when no row exists — callers treat this
// as "no calibration" and pass nil into the engine, triggering
// asset-class defaults.
func (r *Repo) GetByKey(ctx context.Context, key string) (*Liquidity, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("marketimpact: nil db")
	}
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("marketimpact: instrument_key required")
	}
	row := r.db.QueryRowContext(ctx, `
		SELECT instrument_key, COALESCE(symbol, ''), COALESCE(market, ''),
		       COALESCE(asset_class, 'equity'),
		       adv_shares, adv_notional, adv_window_days,
		       daily_volatility,
		       impact_coefficient, impact_exponent,
		       min_slippage_bps, max_slippage_bps,
		       last_calibrated_at,
		       COALESCE(calibration_source, 'manual'),
		       COALESCE(note, ''),
		       updated_at
		  FROM instrument_liquidity
		 WHERE instrument_key = $1
	`, key)
	out, err := scanLiquidity(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return out, err
}

// ListFilter narrows the List response. All fields optional.
type ListFilter struct {
	Market     string
	AssetClass string
	Limit      int
	Offset     int
}

// List returns all calibration rows matching the filter.
func (r *Repo) List(ctx context.Context, filter ListFilter) ([]Liquidity, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("marketimpact: nil db")
	}
	var (
		clauses []string
		args    []any
	)
	if filter.Market != "" {
		args = append(args, filter.Market)
		clauses = append(clauses, fmt.Sprintf("market = $%d", len(args)))
	}
	if filter.AssetClass != "" {
		args = append(args, filter.AssetClass)
		clauses = append(clauses, fmt.Sprintf("asset_class = $%d", len(args)))
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
	q := `SELECT instrument_key, COALESCE(symbol, ''), COALESCE(market, ''),
	             COALESCE(asset_class, 'equity'),
	             adv_shares, adv_notional, adv_window_days,
	             daily_volatility,
	             impact_coefficient, impact_exponent,
	             min_slippage_bps, max_slippage_bps,
	             last_calibrated_at,
	             COALESCE(calibration_source, 'manual'),
	             COALESCE(note, ''),
	             updated_at
	        FROM instrument_liquidity` + where + fmt.Sprintf(`
	    ORDER BY market, symbol
	       LIMIT $%d OFFSET $%d`, len(args)-1, len(args))
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Liquidity, 0, 64)
	for rows.Next() {
		l, err := scanLiquidity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *l)
	}
	return out, rows.Err()
}

// ListAll is a convenience used by the cache loader.
func (r *Repo) ListAll(ctx context.Context) ([]Liquidity, error) {
	return r.List(ctx, ListFilter{Limit: 10_000})
}

// UpsertParams is the operator-write input. Pointer fields
// → leave-alone semantics; non-pointer scalars are written.
type UpsertParams struct {
	InstrumentKey      string
	Symbol             string
	Market             string
	AssetClass         string
	ADVShares          *float64
	ADVNotional        *float64
	ADVWindowDays      *int
	DailyVolatility    *float64
	ImpactCoefficient  *float64
	ImpactExponent     *float64
	MinSlippageBps     *float64
	MaxSlippageBps     *float64
	LastCalibratedAt   *time.Time
	CalibrationSource  string
	Note               string
	UpdatedBy          string // UUID; empty = system
}

// Upsert writes a calibration row. Returns the post-write row.
//
// The trick: pointer-nil fields preserve prior values via
// COALESCE. So `ImpactCoefficient = nil` keeps the existing
// value (or the table default for INSERT).
func (r *Repo) Upsert(ctx context.Context, p UpsertParams) (*Liquidity, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("marketimpact: nil db")
	}
	if strings.TrimSpace(p.InstrumentKey) == "" {
		return nil, errors.New("marketimpact: instrument_key required")
	}
	if strings.TrimSpace(p.Symbol) == "" {
		return nil, errors.New("marketimpact: symbol required")
	}
	if strings.TrimSpace(p.Market) == "" {
		return nil, errors.New("marketimpact: market required")
	}
	if p.AssetClass == "" {
		p.AssetClass = "equity"
	}
	if p.CalibrationSource == "" {
		p.CalibrationSource = "manual"
	}
	switch p.CalibrationSource {
	case "manual", "historical", "broker_reported":
		// ok
	default:
		return nil, fmt.Errorf("marketimpact: invalid calibration_source %q", p.CalibrationSource)
	}
	now := time.Now().UTC()

	var updatedBy any
	if strings.TrimSpace(p.UpdatedBy) != "" {
		updatedBy = p.UpdatedBy
	}

	row := r.db.QueryRowContext(ctx, `
		INSERT INTO instrument_liquidity (
			instrument_key, symbol, market, asset_class,
			adv_shares, adv_notional, adv_window_days,
			daily_volatility, impact_coefficient, impact_exponent,
			min_slippage_bps, max_slippage_bps,
			last_calibrated_at, calibration_source, note,
			updated_at, updated_by
		) VALUES (
			$1, $2, $3, $4,
			$5, $6, COALESCE($7, 20),
			$8, COALESCE($9, 1.0), COALESCE($10, 0.5),
			COALESCE($11, 1), COALESCE($12, 500),
			$13, $14, $15, $16, $17
		)
		ON CONFLICT (instrument_key) DO UPDATE SET
			symbol             = EXCLUDED.symbol,
			market             = EXCLUDED.market,
			asset_class        = EXCLUDED.asset_class,
			adv_shares         = COALESCE(EXCLUDED.adv_shares, instrument_liquidity.adv_shares),
			adv_notional       = COALESCE(EXCLUDED.adv_notional, instrument_liquidity.adv_notional),
			adv_window_days    = COALESCE($7, instrument_liquidity.adv_window_days),
			daily_volatility   = COALESCE(EXCLUDED.daily_volatility, instrument_liquidity.daily_volatility),
			impact_coefficient = COALESCE($9, instrument_liquidity.impact_coefficient),
			impact_exponent    = COALESCE($10, instrument_liquidity.impact_exponent),
			min_slippage_bps   = COALESCE($11, instrument_liquidity.min_slippage_bps),
			max_slippage_bps   = COALESCE($12, instrument_liquidity.max_slippage_bps),
			last_calibrated_at = COALESCE(EXCLUDED.last_calibrated_at, instrument_liquidity.last_calibrated_at),
			calibration_source = EXCLUDED.calibration_source,
			note               = CASE WHEN EXCLUDED.note = '' THEN instrument_liquidity.note ELSE EXCLUDED.note END,
			updated_at         = EXCLUDED.updated_at,
			updated_by         = COALESCE(EXCLUDED.updated_by, instrument_liquidity.updated_by)
		RETURNING instrument_key, COALESCE(symbol, ''), COALESCE(market, ''),
		          COALESCE(asset_class, 'equity'),
		          adv_shares, adv_notional, adv_window_days,
		          daily_volatility,
		          impact_coefficient, impact_exponent,
		          min_slippage_bps, max_slippage_bps,
		          last_calibrated_at,
		          COALESCE(calibration_source, 'manual'),
		          COALESCE(note, ''),
		          updated_at
	`,
		p.InstrumentKey, p.Symbol, p.Market, p.AssetClass,
		nullableFloat(p.ADVShares), nullableFloat(p.ADVNotional), nullableInt(p.ADVWindowDays),
		nullableFloat(p.DailyVolatility), nullableFloat(p.ImpactCoefficient), nullableFloat(p.ImpactExponent),
		nullableFloat(p.MinSlippageBps), nullableFloat(p.MaxSlippageBps),
		nullableTime(p.LastCalibratedAt), p.CalibrationSource, p.Note,
		now, updatedBy,
	)
	return scanLiquidity(row)
}

// Delete removes a calibration row. Used for retiring delisted
// symbols. Returns sql.ErrNoRows if no row existed.
func (r *Repo) Delete(ctx context.Context, key string) error {
	if r == nil || r.db == nil {
		return errors.New("marketimpact: nil db")
	}
	if strings.TrimSpace(key) == "" {
		return errors.New("marketimpact: instrument_key required")
	}
	res, err := r.db.ExecContext(ctx, `DELETE FROM instrument_liquidity WHERE instrument_key = $1`, key)
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

func scanLiquidity(s rowScanner) (*Liquidity, error) {
	var (
		out                Liquidity
		adv, advN, sigma   sql.NullFloat64
		lastCal            sql.NullTime
		coef, exp          sql.NullFloat64
		minBps, maxBps     sql.NullFloat64
	)
	err := s.Scan(
		&out.InstrumentKey, &out.Symbol, &out.Market, &out.AssetClass,
		&adv, &advN, &out.ADVWindowDays,
		&sigma, &coef, &exp,
		&minBps, &maxBps,
		&lastCal, &out.CalibrationSource, &out.Note,
		&out.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if adv.Valid {
		v := adv.Float64
		out.ADVShares = &v
	}
	if advN.Valid {
		v := advN.Float64
		out.ADVNotional = &v
	}
	if sigma.Valid {
		v := sigma.Float64
		out.DailyVolatility = &v
	}
	if coef.Valid {
		out.ImpactCoefficient = coef.Float64
	}
	if exp.Valid {
		out.ImpactExponent = exp.Float64
	}
	if minBps.Valid {
		out.MinSlippageBps = minBps.Float64
	}
	if maxBps.Valid {
		out.MaxSlippageBps = maxBps.Float64
	}
	if lastCal.Valid {
		t := lastCal.Time
		out.LastCalibratedAt = &t
	}
	return &out, nil
}

func nullableFloat(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullableInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullableTime(p *time.Time) any {
	if p == nil {
		return nil
	}
	return *p
}
