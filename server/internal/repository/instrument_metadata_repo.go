// Package repository — instrument_metadata access layer (S12.3).
//
// Backs the lot-size gate's HKLotResolver and CryptoStepResolver
// interfaces against the instrument_metadata table introduced in
// migration 080. Reads are intentionally simple (PK lookup, no
// joins, no batching) because the broker hot path calls these
// per order and a single round-trip is acceptable; if profiling
// shows otherwise we'll add an in-process cache here without
// changing the consumer-facing API.
//
// All writes go through the admin path (S12.6 will add the REST
// handler); this file is read-mostly.

package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// InstrumentMetadataRow mirrors a row in instrument_metadata.
//
// S12.5 — TickSize + TickRulesJSON describe price-alignment rules
// stored alongside the lot rules. TickRulesJSON is a raw JSONB
// blob (the lot-size gate parses it); callers that just need the
// scalar tick can ignore it.
type InstrumentMetadataRow struct {
	InstrumentKey      string
	Market             string
	AssetClass         string
	BoardLot           float64
	StepSize           float64
	SupportsFractional bool
	MinNotional        float64
	ContractMultiplier float64
	TickSize           float64
	TickRulesJSON      []byte
	Source             string
	SourceAsOf         sql.NullTime
	Notes              sql.NullString
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// InstrumentMetadataRepo provides Get / Upsert / List against
// instrument_metadata.
type InstrumentMetadataRepo struct {
	db *sql.DB
}

// NewInstrumentMetadataRepo wires the repo to a *sql.DB.
func NewInstrumentMetadataRepo(db *sql.DB) *InstrumentMetadataRepo {
	if db == nil {
		return nil
	}
	return &InstrumentMetadataRepo{db: db}
}

// Get fetches a row by instrument_key. Returns nil, nil when the
// row is missing — the lot-size gate's resolvers treat that as
// "use default" rather than an error so a missing row never halts
// trading.
func (r *InstrumentMetadataRepo) Get(ctx context.Context, instrumentKey string) (*InstrumentMetadataRow, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	key := strings.TrimSpace(instrumentKey)
	if key == "" {
		return nil, nil
	}
	row := &InstrumentMetadataRow{}
	err := r.db.QueryRowContext(ctx,
		`SELECT instrument_key, market, asset_class,
		        board_lot, step_size, supports_fractional,
		        min_notional, contract_multiplier,
		        COALESCE(tick_size, 0), COALESCE(tick_rules, '[]'::jsonb),
		        source, source_as_of, notes,
		        created_at, updated_at
		   FROM instrument_metadata
		  WHERE instrument_key = $1`, key,
	).Scan(
		&row.InstrumentKey, &row.Market, &row.AssetClass,
		&row.BoardLot, &row.StepSize, &row.SupportsFractional,
		&row.MinNotional, &row.ContractMultiplier,
		&row.TickSize, &row.TickRulesJSON,
		&row.Source, &row.SourceAsOf, &row.Notes,
		&row.CreatedAt, &row.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("instrument_metadata_repo: get %s: %w", key, err)
	}
	return row, nil
}

// Upsert inserts or replaces a row keyed by instrument_key. The
// admin-side write path uses this; we don't expose individual
// column updates because the lot-size gate's correctness depends
// on the full set being consistent.
func (r *InstrumentMetadataRepo) Upsert(ctx context.Context, row InstrumentMetadataRow) error {
	if r == nil || r.db == nil {
		return errors.New("instrument_metadata_repo: nil db")
	}
	if row.InstrumentKey == "" || row.Market == "" || row.AssetClass == "" {
		return fmt.Errorf("instrument_metadata_repo: instrument_key/market/asset_class required")
	}
	if row.ContractMultiplier == 0 {
		row.ContractMultiplier = 1
	}
	if row.Source == "" {
		row.Source = "manual"
	}
	rulesJSON := row.TickRulesJSON
	if len(rulesJSON) == 0 {
		rulesJSON = []byte("[]")
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO instrument_metadata
		    (instrument_key, market, asset_class,
		     board_lot, step_size, supports_fractional,
		     min_notional, contract_multiplier,
		     tick_size, tick_rules,
		     source, source_as_of, notes)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, $12, $13)
		 ON CONFLICT (instrument_key) DO UPDATE SET
		     market              = EXCLUDED.market,
		     asset_class         = EXCLUDED.asset_class,
		     board_lot           = EXCLUDED.board_lot,
		     step_size           = EXCLUDED.step_size,
		     supports_fractional = EXCLUDED.supports_fractional,
		     min_notional        = EXCLUDED.min_notional,
		     contract_multiplier = EXCLUDED.contract_multiplier,
		     tick_size           = EXCLUDED.tick_size,
		     tick_rules          = EXCLUDED.tick_rules,
		     source              = EXCLUDED.source,
		     source_as_of        = EXCLUDED.source_as_of,
		     notes               = EXCLUDED.notes,
		     updated_at          = NOW()`,
		row.InstrumentKey, row.Market, row.AssetClass,
		row.BoardLot, row.StepSize, row.SupportsFractional,
		row.MinNotional, row.ContractMultiplier,
		row.TickSize, string(rulesJSON),
		row.Source, row.SourceAsOf, row.Notes,
	)
	if err != nil {
		return fmt.Errorf("instrument_metadata_repo: upsert %s: %w", row.InstrumentKey, err)
	}
	return nil
}

// ListByMarket returns all rows for the given market (paged by
// limit/offset). Used by the admin UI; the lot-size gate hot path
// only calls Get.
func (r *InstrumentMetadataRepo) ListByMarket(ctx context.Context, market string, limit, offset int) ([]InstrumentMetadataRow, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT instrument_key, market, asset_class,
		        board_lot, step_size, supports_fractional,
		        min_notional, contract_multiplier,
		        COALESCE(tick_size, 0), COALESCE(tick_rules, '[]'::jsonb),
		        source, source_as_of, notes,
		        created_at, updated_at
		   FROM instrument_metadata
		  WHERE ($1 = '' OR market = $1)
		  ORDER BY instrument_key
		  LIMIT $2 OFFSET $3`, market, limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("instrument_metadata_repo: list: %w", err)
	}
	defer rows.Close()

	var out []InstrumentMetadataRow
	for rows.Next() {
		var row InstrumentMetadataRow
		if err := rows.Scan(
			&row.InstrumentKey, &row.Market, &row.AssetClass,
			&row.BoardLot, &row.StepSize, &row.SupportsFractional,
			&row.MinNotional, &row.ContractMultiplier,
			&row.TickSize, &row.TickRulesJSON,
			&row.Source, &row.SourceAsOf, &row.Notes,
			&row.CreatedAt, &row.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("instrument_metadata_repo: scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("instrument_metadata_repo: rows: %w", err)
	}
	return out, nil
}
