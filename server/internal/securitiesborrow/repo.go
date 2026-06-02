// repo.go — three storage tables: rate calibrations, locate
// audit log, and the daily borrow-fee ledger.
//
// All write paths use returning columns so callers get the
// canonical row back without a follow-up read; this matters
// for the in-memory cache, which calls Upsert and then expects
// to be handed the up-to-date row to install.

package securitiesborrow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Repo bundles access to all three securitiesborrow tables.
type Repo struct {
	db *sql.DB
}

// NewRepo constructs the repo. nil db is rejected at first call.
func NewRepo(db *sql.DB) *Repo { return &Repo{db: db} }

// ==================== rate calibration ====================

// GetRateByKey returns the calibration row for one instrument.
// Returns (nil, nil) when missing — same convention as
// lockup.GetByID — so callers can branch on "no calibration".
func (r *Repo) GetRateByKey(ctx context.Context, instrumentKey string) (*BorrowRate, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("securitiesborrow: nil db")
	}
	if strings.TrimSpace(instrumentKey) == "" {
		return nil, errors.New("securitiesborrow: instrument_key required")
	}
	row := r.db.QueryRowContext(ctx, rateSelect+` WHERE instrument_key = $1`, instrumentKey)
	rate, err := scanRate(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return rate, err
}

// ListRateFilter narrows the admin list response.
type ListRateFilter struct {
	Market       string
	AssetClass   string
	Availability string
	Limit        int
	Offset       int
}

// ListRates returns matching rates ordered by symbol.
func (r *Repo) ListRates(ctx context.Context, filter ListRateFilter) ([]BorrowRate, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("securitiesborrow: nil db")
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
	if filter.Availability != "" {
		args = append(args, filter.Availability)
		clauses = append(clauses, fmt.Sprintf("availability = $%d", len(args)))
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
	q := rateSelect + where + fmt.Sprintf(`
		ORDER BY symbol ASC
		LIMIT $%d OFFSET $%d`, len(args)-1, len(args))
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]BorrowRate, 0, 64)
	for rows.Next() {
		rec, err := scanRate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	return out, rows.Err()
}

// ListAllRates streams every row. Used by the cache loader at
// boot. Not exposed via the admin API (no pagination).
func (r *Repo) ListAllRates(ctx context.Context) ([]BorrowRate, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("securitiesborrow: nil db")
	}
	rows, err := r.db.QueryContext(ctx, rateSelect+` ORDER BY instrument_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]BorrowRate, 0, 256)
	for rows.Next() {
		rec, err := scanRate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rec)
	}
	return out, rows.Err()
}

// UpsertRateParams is the upsert input. Pointer fields →
// nil = preserve existing column value (via COALESCE).
type UpsertRateParams struct {
	InstrumentKey       string
	Symbol              string
	Market              string
	AssetClass          string
	BorrowRateBpsAnnual *float64
	LocateFeeBps        *float64
	Availability        string
	AvailableShares     *int64
	MinLocateQty        *int64
	MaxLocateQty        *int64
	Source              string
	LastCalibratedAt    *time.Time
	Note                string
	UpdatedBy           string
}

// UpsertRate inserts or updates the rate row.
func (r *Repo) UpsertRate(ctx context.Context, p UpsertRateParams) (*BorrowRate, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("securitiesborrow: nil db")
	}
	if err := validateUpsertRate(p); err != nil {
		return nil, err
	}
	availability := strings.ToLower(strings.TrimSpace(p.Availability))
	if availability == "" {
		availability = string(AvailabilityEasy)
	}
	source := strings.ToLower(strings.TrimSpace(p.Source))
	if source == "" {
		source = string(SourceManual)
	}
	market := strings.TrimSpace(p.Market)
	if market == "" {
		market = "US"
	}
	assetClass := strings.TrimSpace(p.AssetClass)
	if assetClass == "" {
		assetClass = "equity"
	}
	calibratedAt := time.Now().UTC()
	if p.LastCalibratedAt != nil {
		calibratedAt = p.LastCalibratedAt.UTC()
	}
	var updatedBy any
	if strings.TrimSpace(p.UpdatedBy) != "" {
		updatedBy = p.UpdatedBy
	}
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO security_borrow_rates (
			instrument_key, symbol, market, asset_class,
			borrow_rate_bps_annual, locate_fee_bps,
			availability, available_shares, min_locate_qty, max_locate_qty,
			source, last_calibrated_at, note, updated_by
		) VALUES ($1, $2, $3, $4,
		          COALESCE($5, 0), COALESCE($6, 0),
		          $7, $8, $9, $10,
		          $11, $12, $13, $14)
		ON CONFLICT (instrument_key) DO UPDATE SET
			symbol                 = EXCLUDED.symbol,
			market                 = EXCLUDED.market,
			asset_class            = EXCLUDED.asset_class,
			borrow_rate_bps_annual = COALESCE(EXCLUDED.borrow_rate_bps_annual, security_borrow_rates.borrow_rate_bps_annual),
			locate_fee_bps         = COALESCE(EXCLUDED.locate_fee_bps,         security_borrow_rates.locate_fee_bps),
			availability           = EXCLUDED.availability,
			available_shares       = COALESCE(EXCLUDED.available_shares,       security_borrow_rates.available_shares),
			min_locate_qty         = COALESCE(EXCLUDED.min_locate_qty,         security_borrow_rates.min_locate_qty),
			max_locate_qty         = COALESCE(EXCLUDED.max_locate_qty,         security_borrow_rates.max_locate_qty),
			source                 = EXCLUDED.source,
			last_calibrated_at     = EXCLUDED.last_calibrated_at,
			note                   = EXCLUDED.note,
			updated_by             = EXCLUDED.updated_by,
			updated_at             = NOW()
		RETURNING ` + rateColumns,
		p.InstrumentKey, p.Symbol, market, assetClass,
		nullableFloat(p.BorrowRateBpsAnnual), nullableFloat(p.LocateFeeBps),
		availability, nullableInt(p.AvailableShares), nullableInt(p.MinLocateQty), nullableInt(p.MaxLocateQty),
		source, calibratedAt, p.Note, updatedBy,
	)
	return scanRate(row)
}

func validateUpsertRate(p UpsertRateParams) error {
	if strings.TrimSpace(p.InstrumentKey) == "" {
		return errors.New("securitiesborrow: instrument_key required")
	}
	if strings.TrimSpace(p.Symbol) == "" {
		return errors.New("securitiesborrow: symbol required")
	}
	if p.BorrowRateBpsAnnual != nil && *p.BorrowRateBpsAnnual < 0 {
		return errors.New("securitiesborrow: borrow_rate_bps_annual must be >= 0")
	}
	if p.LocateFeeBps != nil && *p.LocateFeeBps < 0 {
		return errors.New("securitiesborrow: locate_fee_bps must be >= 0")
	}
	if p.Availability != "" && !IsValidAvailability(p.Availability) {
		return fmt.Errorf("securitiesborrow: invalid availability %q", p.Availability)
	}
	if p.Source != "" && !IsValidCalibrationSource(p.Source) {
		return fmt.Errorf("securitiesborrow: invalid source %q", p.Source)
	}
	return nil
}

// DeleteRate hard-removes a calibration row.
func (r *Repo) DeleteRate(ctx context.Context, instrumentKey string) error {
	if r == nil || r.db == nil {
		return errors.New("securitiesborrow: nil db")
	}
	if strings.TrimSpace(instrumentKey) == "" {
		return errors.New("securitiesborrow: instrument_key required")
	}
	res, err := r.db.ExecContext(ctx, `DELETE FROM security_borrow_rates WHERE instrument_key = $1`, instrumentKey)
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

// ==================== locate event audit log ====================

// LogLocateEventParams is the insert input for one audit row.
type LogLocateEventParams struct {
	FundID          string
	InstrumentKey   string
	Symbol          string
	RequestedQty    float64
	Decision        LocateDecisionKind
	RateBpsAnnual   *float64
	LocateFeeBps    *float64
	LocateFeeAmount *float64
	IntendedPrice   *float64
	Notional        *float64
	Reason          string
	ClientOrderID   string
}

// LogLocateEvent inserts one audit row. Returns the row id.
func (r *Repo) LogLocateEvent(ctx context.Context, p LogLocateEventParams) (string, error) {
	if r == nil || r.db == nil {
		return "", errors.New("securitiesborrow: nil db")
	}
	if strings.TrimSpace(p.FundID) == "" || strings.TrimSpace(p.InstrumentKey) == "" {
		return "", errors.New("securitiesborrow: fund_id + instrument_key required")
	}
	if p.Decision == "" {
		return "", errors.New("securitiesborrow: decision required")
	}
	var id string
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO security_locate_events (
			fund_id, instrument_key, symbol, requested_qty,
			decision, rate_bps_annual, locate_fee_bps, locate_fee_amount,
			intended_price, notional, reason, client_order_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING id
	`,
		p.FundID, p.InstrumentKey, p.Symbol, p.RequestedQty,
		string(p.Decision),
		nullableFloat(p.RateBpsAnnual), nullableFloat(p.LocateFeeBps), nullableFloat(p.LocateFeeAmount),
		nullableFloat(p.IntendedPrice), nullableFloat(p.Notional),
		p.Reason, p.ClientOrderID,
	).Scan(&id)
	return id, err
}

// ListLocateFilter narrows the audit list response.
type ListLocateFilter struct {
	FundID        string
	InstrumentKey string
	Decision      string
	Since         *time.Time
	Limit         int
	Offset        int
}

// ListLocateEvents reads the audit log.
func (r *Repo) ListLocateEvents(ctx context.Context, filter ListLocateFilter) ([]LocateEvent, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("securitiesborrow: nil db")
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
	if filter.Decision != "" {
		args = append(args, filter.Decision)
		clauses = append(clauses, fmt.Sprintf("decision = $%d", len(args)))
	}
	if filter.Since != nil {
		args = append(args, filter.Since.UTC())
		clauses = append(clauses, fmt.Sprintf("created_at >= $%d", len(args)))
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
	             requested_qty, decision,
	             rate_bps_annual, locate_fee_bps, locate_fee_amount,
	             intended_price, notional,
	             COALESCE(reason, ''), COALESCE(client_order_id, ''),
	             created_at
	        FROM security_locate_events` + where + fmt.Sprintf(`
	    ORDER BY created_at DESC
	       LIMIT $%d OFFSET $%d`, len(args)-1, len(args))
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]LocateEvent, 0, 64)
	for rows.Next() {
		ev, err := scanLocateEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ev)
	}
	return out, rows.Err()
}

// ==================== daily borrow-fee ledger ====================

// UpsertLedgerParams is the insert input. (fund, instrument,
// date) is unique; ON CONFLICT DO UPDATE so the daily loop is
// idempotent across retries.
type UpsertLedgerParams struct {
	FundID            string
	InstrumentKey     string
	Symbol            string
	AccrualDate       time.Time  // truncated to date in caller
	ShortQty          float64
	MarketPrice       float64
	Notional          float64
	RateBpsAnnual     float64
	DayCountBasis     int
	FeeAmount         float64
	CashLedgerEntryID string
}

// UpsertLedgerEntry inserts a fee row. Returns the canonical
// row.
func (r *Repo) UpsertLedgerEntry(ctx context.Context, p UpsertLedgerParams) (*BorrowLedgerEntry, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("securitiesborrow: nil db")
	}
	if strings.TrimSpace(p.FundID) == "" || strings.TrimSpace(p.InstrumentKey) == "" {
		return nil, errors.New("securitiesborrow: fund_id + instrument_key required")
	}
	if p.ShortQty <= 0 {
		return nil, errors.New("securitiesborrow: short_qty must be > 0")
	}
	if p.FeeAmount < 0 {
		return nil, errors.New("securitiesborrow: fee_amount must be >= 0")
	}
	dcb := p.DayCountBasis
	if dcb != 360 && dcb != 365 {
		dcb = 365
	}
	var entryID any
	if strings.TrimSpace(p.CashLedgerEntryID) != "" {
		entryID = p.CashLedgerEntryID
	}
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO short_position_borrow_ledger (
			fund_id, instrument_key, symbol, accrual_date,
			short_qty, market_price, notional,
			rate_bps_annual, day_count_basis, fee_amount,
			cash_ledger_entry_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (fund_id, instrument_key, accrual_date) DO UPDATE SET
			symbol               = EXCLUDED.symbol,
			short_qty            = EXCLUDED.short_qty,
			market_price         = EXCLUDED.market_price,
			notional             = EXCLUDED.notional,
			rate_bps_annual      = EXCLUDED.rate_bps_annual,
			day_count_basis      = EXCLUDED.day_count_basis,
			fee_amount           = EXCLUDED.fee_amount,
			cash_ledger_entry_id = COALESCE(EXCLUDED.cash_ledger_entry_id, short_position_borrow_ledger.cash_ledger_entry_id)
		RETURNING id, fund_id, instrument_key, COALESCE(symbol, ''),
		          accrual_date, short_qty, market_price, notional,
		          rate_bps_annual, day_count_basis, fee_amount,
		          cash_ledger_entry_id, created_at
	`,
		p.FundID, p.InstrumentKey, p.Symbol, p.AccrualDate.UTC(),
		p.ShortQty, p.MarketPrice, p.Notional,
		p.RateBpsAnnual, dcb, p.FeeAmount,
		entryID,
	)
	return scanLedger(row)
}

// ListLedgerFilter narrows the ledger list response.
type ListLedgerFilter struct {
	FundID        string
	InstrumentKey string
	Since         *time.Time
	Until         *time.Time
	Limit         int
	Offset        int
}

// ListLedger returns ledger rows ordered newest-first.
func (r *Repo) ListLedger(ctx context.Context, filter ListLedgerFilter) ([]BorrowLedgerEntry, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("securitiesborrow: nil db")
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
	if filter.Since != nil {
		args = append(args, filter.Since.UTC())
		clauses = append(clauses, fmt.Sprintf("accrual_date >= $%d", len(args)))
	}
	if filter.Until != nil {
		args = append(args, filter.Until.UTC())
		clauses = append(clauses, fmt.Sprintf("accrual_date <= $%d", len(args)))
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
	             accrual_date, short_qty, market_price, notional,
	             rate_bps_annual, day_count_basis, fee_amount,
	             cash_ledger_entry_id, created_at
	        FROM short_position_borrow_ledger` + where + fmt.Sprintf(`
	    ORDER BY accrual_date DESC, fund_id, instrument_key
	       LIMIT $%d OFFSET $%d`, len(args)-1, len(args))
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]BorrowLedgerEntry, 0, 64)
	for rows.Next() {
		entry, err := scanLedger(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *entry)
	}
	return out, rows.Err()
}

// ==================== helpers ====================

const rateColumns = `id, instrument_key, symbol, market, asset_class,
		borrow_rate_bps_annual, locate_fee_bps,
		availability, available_shares, min_locate_qty, max_locate_qty,
		source, last_calibrated_at, COALESCE(note, ''), updated_by,
		created_at, updated_at`

const rateSelect = `SELECT ` + rateColumns + ` FROM security_borrow_rates`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRate(s rowScanner) (*BorrowRate, error) {
	var (
		out          BorrowRate
		availStr     string
		sourceStr    string
		availShares  sql.NullInt64
		minLocate    sql.NullInt64
		maxLocate    sql.NullInt64
		updatedBy    sql.NullString
	)
	err := s.Scan(
		&out.ID, &out.InstrumentKey, &out.Symbol, &out.Market, &out.AssetClass,
		&out.BorrowRateBpsAnnual, &out.LocateFeeBps,
		&availStr, &availShares, &minLocate, &maxLocate,
		&sourceStr, &out.LastCalibratedAt, &out.Note, &updatedBy,
		&out.CreatedAt, &out.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	out.Availability = Availability(availStr)
	out.Source = CalibrationSource(sourceStr)
	if availShares.Valid {
		v := availShares.Int64
		out.AvailableShares = &v
	}
	if minLocate.Valid {
		v := minLocate.Int64
		out.MinLocateQty = &v
	}
	if maxLocate.Valid {
		v := maxLocate.Int64
		out.MaxLocateQty = &v
	}
	if updatedBy.Valid {
		out.UpdatedBy = updatedBy.String
	}
	return &out, nil
}

func scanLocateEvent(s rowScanner) (*LocateEvent, error) {
	var (
		out         LocateEvent
		decStr      string
		rate        sql.NullFloat64
		feeBps      sql.NullFloat64
		feeAmt      sql.NullFloat64
		intended    sql.NullFloat64
		notional    sql.NullFloat64
	)
	err := s.Scan(
		&out.ID, &out.FundID, &out.InstrumentKey, &out.Symbol,
		&out.RequestedQty, &decStr,
		&rate, &feeBps, &feeAmt,
		&intended, &notional,
		&out.Reason, &out.ClientOrderID,
		&out.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	out.Decision = LocateDecisionKind(decStr)
	if rate.Valid {
		v := rate.Float64
		out.RateBpsAnnual = &v
	}
	if feeBps.Valid {
		v := feeBps.Float64
		out.LocateFeeBps = &v
	}
	if feeAmt.Valid {
		v := feeAmt.Float64
		out.LocateFeeAmount = &v
	}
	if intended.Valid {
		v := intended.Float64
		out.IntendedPrice = &v
	}
	if notional.Valid {
		v := notional.Float64
		out.Notional = &v
	}
	return &out, nil
}

func scanLedger(s rowScanner) (*BorrowLedgerEntry, error) {
	var (
		out     BorrowLedgerEntry
		entryID sql.NullString
	)
	err := s.Scan(
		&out.ID, &out.FundID, &out.InstrumentKey, &out.Symbol,
		&out.AccrualDate, &out.ShortQty, &out.MarketPrice, &out.Notional,
		&out.RateBpsAnnual, &out.DayCountBasis, &out.FeeAmount,
		&entryID, &out.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	if entryID.Valid {
		out.CashLedgerEntryID = entryID.String
	}
	return &out, nil
}

func nullableFloat(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullableInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}
