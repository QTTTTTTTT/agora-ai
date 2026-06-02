// drawdown_snapshot.go — adapter that turns the platform's NAV
// history + open positions into a drawdown.Snapshot.
//
// What it does
//
//   - Pulls the peak NAV over a configurable lookback window from
//     `nav_snapshots` (the existing daily roll-up).
//   - Reads `funds.nav` (or, optionally, the most recent
//     nav_snapshots row) for current NAV.
//   - Reads `holding_positions` for the trim engine.
//   - Joins `drawdown_events` to fill in `LastFiredAt[tier]` so
//     the engine can enforce cooldown without reaching back into
//     the DB on its own.
//
// Why "peak over lookback" vs all-time peak
//
// All-time peak is fine for a fund that's been running 6 months
// but punitive for a long-running fund that hit an absolute peak
// 3 years ago and has been recovering since — that fund would
// always be "in drawdown". Operators set `lookback_days` (default
// 90) and the engine reads peak inside that window only.

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fundai/server/internal/drawdown"
	"github.com/fundai/server/internal/repository"
)

// DefaultDrawdownLookbackDays controls how far back we look for
// the peak NAV. 90d is roughly one quarter — long enough that a
// short-term dip recovery doesn't churn breaches, short enough
// that the policy stays responsive.
const DefaultDrawdownLookbackDays = 90

// DefaultDrawdownCooldownLookback controls how far back we look
// for "last fired" rows. 7d covers any cooldown ≤ 168h (a week);
// longer cooldowns are rare and not worth the index scan cost.
const DefaultDrawdownCooldownLookback = 7 * 24 * time.Hour

// drawdownSnapshotBuilder loads a per-fund Snapshot for the engine.
// Stateless after wiring, so a single instance can be shared
// across the loop and the admin API.
type drawdownSnapshotBuilder struct {
	db           *sql.DB
	positionRepo *repository.PositionRepo
	repo         *drawdown.Repo
	lookbackDays int
}

// newDrawdownSnapshotBuilder is the standard constructor. nil DB
// is rejected at first call rather than at construction so the
// wiring path can defer DB attachment.
func newDrawdownSnapshotBuilder(db *sql.DB, repo *drawdown.Repo) *drawdownSnapshotBuilder {
	if db == nil {
		return nil
	}
	return &drawdownSnapshotBuilder{
		db:           db,
		positionRepo: repository.NewPositionRepo(db),
		repo:         repo,
		lookbackDays: DefaultDrawdownLookbackDays,
	}
}

// Build returns a freshly read snapshot for (fund, asOf). asOf
// is treated as the "now" for cooldown comparisons; pass the
// trigger time when called from the loop or the admin click.
func (b *drawdownSnapshotBuilder) Build(ctx context.Context, fundID string, asOf time.Time) (*drawdown.Snapshot, error) {
	if b == nil || b.db == nil {
		return nil, errors.New("drawdown_snapshot: nil builder")
	}
	if strings.TrimSpace(fundID) == "" {
		return nil, errors.New("drawdown_snapshot: fund_id required")
	}
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}

	// 1) Current NAV from funds.nav (the running mark, kept fresh
	// by the navcalc loop). If the fund row is missing we bail —
	// the engine has nothing to evaluate against.
	var currentNAV float64
	err := b.db.QueryRowContext(ctx,
		`SELECT COALESCE(nav, 1.0) FROM funds WHERE id = $1`, fundID,
	).Scan(&currentNAV)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("drawdown_snapshot: fund not found: %s", fundID)
		}
		return nil, fmt.Errorf("drawdown_snapshot: read fund nav: %w", err)
	}

	// 2) Peak NAV inside lookback. nav_snapshots is the daily
	// truth; we MAX over the window and fall back to the current
	// NAV when the fund is too young to have a history.
	lookback := b.lookbackDays
	if lookback <= 0 {
		lookback = DefaultDrawdownLookbackDays
	}
	since := asOf.AddDate(0, 0, -lookback)
	var (
		peakNAV       sql.NullFloat64
		navSnapshotID sql.NullString
	)
	err = b.db.QueryRowContext(ctx, `
		SELECT nav, id::text
		  FROM nav_snapshots
		 WHERE fund_id = $1 AND trading_date >= $2::date
		 ORDER BY nav DESC, trading_date ASC
		 LIMIT 1
	`, fundID, since.Format("2006-01-02")).Scan(&peakNAV, &navSnapshotID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("drawdown_snapshot: peak nav: %w", err)
	}
	peak := currentNAV
	if peakNAV.Valid && peakNAV.Float64 > peak {
		peak = peakNAV.Float64
	}

	// 3) Positions (long only).
	holdings, err := b.positionRepo.ListByFund(ctx, fundID)
	if err != nil {
		return nil, fmt.Errorf("drawdown_snapshot: list positions: %w", err)
	}
	positions := make([]drawdown.Position, 0, len(holdings))
	for _, h := range holdings {
		// Engine ignores ≤ 0; we still want zero-qty rows out so
		// the trim plan ratio math works on a clean slice.
		if h.Quantity <= 0 {
			continue
		}
		positions = append(positions, drawdown.Position{
			Symbol:        h.Symbol,
			InstrumentKey: h.InstrumentKey,
			Quantity:      h.Quantity,
			AvgCost:       h.CostPrice,
			MarketValue:   h.MarketValue,
		})
	}

	// 4) LastFiredAt for cooldown. Empty when repo is missing
	// (test wiring) or the fund has no prior breaches.
	lastFired := map[int]time.Time{}
	if b.repo != nil {
		got, err := b.repo.LastFiredAtForFund(ctx, fundID, DefaultDrawdownCooldownLookback)
		if err != nil {
			return nil, fmt.Errorf("drawdown_snapshot: last fired: %w", err)
		}
		lastFired = got
	}

	snap := &drawdown.Snapshot{
		FundID:       fundID,
		PeakNAV:      peak,
		CurrentNAV:   currentNAV,
		AsOf:         asOf.UTC(),
		Positions:    positions,
		LastFiredAt:  lastFired,
	}
	if navSnapshotID.Valid {
		snap.NavSnapshotID = navSnapshotID.String
	}
	return snap, nil
}
