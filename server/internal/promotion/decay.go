package promotion

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/fundai/server/internal/repository"
)

// sqlFloat wraps a float into sql.NullFloat64. Kept inline so
// the snapshot translator stays compact.
func sqlFloat(v float64) sql.NullFloat64 {
	return sql.NullFloat64{Float64: v, Valid: true}
}

// LiveMetricsLookup returns rolling-window actual metrics for a
// fund. The caller picks the window (typically 30 days).
//
// The wiring layer plugs this into the fund's NAV history +
// trade ledger; the monitor stays storage-agnostic so it can be
// unit-tested with a deterministic stub.
//
// Returning a nil pointer for any metric means "not enough data
// to compute" — the monitor then records a snapshot with NULL
// values rather than emitting a false decay signal.
type LiveMetricsLookup func(ctx context.Context, fundID string, windowDays int) (*LiveMetrics, error)

// LiveMetrics is the lookup's projection — same shape as the
// backtest Metrics but reflecting actual fund behaviour.
type LiveMetrics struct {
	Sharpe       *float64
	Return       *float64
	MaxDrawdown  *float64
	TradeCount   int
	WindowFrom   time.Time
	WindowTo     time.Time
	DataComplete bool // false when window had fewer than 2 NAV points
}

// DecayMonitor is the Phase 2L scheduler-friendly worker. It
// samples live metrics, compares them to the promotion's baseline,
// and flips the promotion to "decayed" + auto-rolls-back when the
// observed Sharpe ratio falls below baseline * DecayRatio.
//
// Run frequency is the caller's choice — typically every trading
// day at market close — so we keep this stateless across calls.
type DecayMonitor struct {
	Service           *Service
	Repo              *repository.PromotionRepo
	LiveLookup        LiveMetricsLookup
	NewID             IDGen
	Now               Clock
	// WindowDays is the rolling window used for live metrics.
	// Default 30.
	WindowDays int
	// MinSnapshotsBeforeDowngrade prevents one bad window from
	// killing a promotion: the monitor requires at least N
	// consecutive snapshots flagged as decayed before triggering
	// auto-rollback. Default 3.
	MinSnapshotsBeforeDowngrade int
	// OnDowngrade is invoked when the monitor auto-deactivates
	// a promotion. Typically wired to the resolver so it
	// invalidates its cache; left nil for tests.
	OnDowngrade func(ctx context.Context, fundID, promotionID string)
}

// Sample takes one snapshot for a single live promotion. Public
// so a test can drive a single fund deterministically; the
// scheduler iterates all live promotions and calls this for each.
func (m *DecayMonitor) Sample(ctx context.Context, p *Promotion) (*HealthSnapshot, error) {
	if m == nil || m.Service == nil || m.Repo == nil || m.LiveLookup == nil || m.NewID == nil || m.Now == nil {
		return nil, errors.New("decay monitor: not wired")
	}
	if p == nil {
		return nil, errors.New("decay monitor: nil promotion")
	}
	if !p.Status.IsLive() {
		return nil, fmt.Errorf("decay monitor: promotion %s is %s (not live)", p.ID, p.Status)
	}
	window := m.WindowDays
	if window <= 0 {
		window = 30
	}
	live, err := m.LiveLookup(ctx, p.FundID, window)
	if err != nil {
		return nil, fmt.Errorf("live metrics lookup: %w", err)
	}
	snap := buildSnapshot(p, live, window, m.NewID(), m.Now())
	if err := m.Repo.InsertHealthSnapshot(ctx, snapshotToRow(snap)); err != nil {
		return nil, err
	}
	if snap.DecayFlag {
		if err := m.maybeAutoDowngrade(ctx, p); err != nil {
			// Auto-rollback failed; log via returning the error
			// so the caller can surface it. The snapshot is
			// already persisted, so we don't lose the signal.
			return snap, fmt.Errorf("auto-downgrade: %w", err)
		}
	}
	return snap, nil
}

// SampleAll iterates every live promotion and samples each. Returns
// the slice of snapshots taken plus the first non-fatal error
// encountered (subsequent promotions still get sampled). Designed
// for a scheduler loop: log the error, continue tomorrow.
func (m *DecayMonitor) SampleAll(ctx context.Context) ([]*HealthSnapshot, error) {
	if m == nil || m.Service == nil {
		return nil, errors.New("decay monitor: not wired")
	}
	rows, err := m.Repo.ListLive(ctx)
	if err != nil {
		return nil, err
	}
	out := []*HealthSnapshot{}
	var firstErr error
	for _, r := range rows {
		p, derr := rowToDomain(r)
		if derr != nil {
			if firstErr == nil {
				firstErr = derr
			}
			continue
		}
		snap, serr := m.Sample(ctx, p)
		if serr != nil {
			if firstErr == nil {
				firstErr = serr
			}
			continue
		}
		out = append(out, snap)
	}
	return out, firstErr
}

// maybeAutoDowngrade flips the promotion to decayed status when
// MinSnapshotsBeforeDowngrade most-recent snapshots are flagged.
// Built in so a single noisy day doesn't yank a promotion that
// has a long healthy history.
func (m *DecayMonitor) maybeAutoDowngrade(ctx context.Context, p *Promotion) error {
	threshold := m.MinSnapshotsBeforeDowngrade
	if threshold <= 0 {
		threshold = 3
	}
	rows, err := m.Repo.ListHealthSnapshots(ctx, p.ID, threshold)
	if err != nil {
		return err
	}
	if len(rows) < threshold {
		return nil // not enough samples yet
	}
	for _, r := range rows {
		if !r.DecayFlag {
			return nil // streak broken — don't downgrade
		}
	}
	target := StatusDecayed
	if _, err := m.Service.Deactivate(ctx, p.ID, target,
		"decay-monitor",
		fmt.Sprintf("decay detected: %d consecutive samples below baseline*%.2f", threshold, p.DecayRatio),
	); err != nil {
		return err
	}
	if m.OnDowngrade != nil {
		m.OnDowngrade(ctx, p.FundID, p.ID)
	}
	return nil
}

// buildSnapshot composes a HealthSnapshot from a baseline + live
// metrics pair. Pulled out as a pure function so the decay logic
// (when to flag, how to compute the ratio) is unit-testable
// independent of DB IO.
func buildSnapshot(p *Promotion, live *LiveMetrics, window int, id string, now time.Time) *HealthSnapshot {
	snap := &HealthSnapshot{
		ID:          id,
		PromotionID: p.ID,
		SnapshotAt:  now,
		WindowDays:  window,
	}
	if live == nil || !live.DataComplete {
		snap.Notes = "insufficient live data"
		return snap
	}
	if live.Sharpe != nil {
		v := *live.Sharpe
		snap.ActualSharpe = &v
	}
	if live.Return != nil {
		v := *live.Return
		snap.ActualReturn = &v
	}
	if live.MaxDrawdown != nil {
		v := *live.MaxDrawdown
		snap.ActualMaxDrawdown = &v
	}
	snap.ActualTradeCount = live.TradeCount

	baselineSharpe := p.BaselineMetrics.EffectiveSharpe()
	// We only compute the ratio when both sides are meaningfully
	// positive. A negative baseline Sharpe means the basis
	// backtest itself was unprofitable — the decay concept
	// doesn't apply and we don't trigger downgrade signals.
	if baselineSharpe > 0 && snap.ActualSharpe != nil {
		ratio := *snap.ActualSharpe / baselineSharpe
		if !math.IsNaN(ratio) && !math.IsInf(ratio, 0) {
			snap.SharpeDecayRatio = &ratio
			if ratio < p.DecayRatio {
				snap.DecayFlag = true
			}
		}
	}
	return snap
}

func snapshotToRow(s *HealthSnapshot) *repository.HealthSnapshotRow {
	r := &repository.HealthSnapshotRow{
		ID: s.ID, PromotionID: s.PromotionID, SnapshotAt: s.SnapshotAt,
		WindowDays: s.WindowDays, ActualTradeCount: s.ActualTradeCount,
		DecayFlag: s.DecayFlag,
	}
	if s.ActualSharpe != nil {
		r.ActualSharpe = sqlFloat(*s.ActualSharpe)
	}
	if s.ActualReturn != nil {
		r.ActualReturn = sqlFloat(*s.ActualReturn)
	}
	if s.ActualMaxDrawdown != nil {
		r.ActualMaxDrawdown = sqlFloat(*s.ActualMaxDrawdown)
	}
	if s.SharpeDecayRatio != nil {
		r.SharpeDecayRatio = sqlFloat(*s.SharpeDecayRatio)
	}
	if s.Notes != "" {
		r.Notes = nullableStringSrc(s.Notes)
	}
	return r
}

// ListHealth returns the trailing N snapshots for a promotion,
// newest first. Used by the API for the detail page chart.
func (m *DecayMonitor) ListHealth(ctx context.Context, promotionID string, limit int) ([]*HealthSnapshot, error) {
	if m == nil || m.Repo == nil {
		return nil, errors.New("decay monitor: not wired")
	}
	rows, err := m.Repo.ListHealthSnapshots(ctx, promotionID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]*HealthSnapshot, 0, len(rows))
	for _, r := range rows {
		out = append(out, rowToHealthSnapshot(r))
	}
	return out, nil
}

func rowToHealthSnapshot(r *repository.HealthSnapshotRow) *HealthSnapshot {
	if r == nil {
		return nil
	}
	out := &HealthSnapshot{
		ID:               r.ID,
		PromotionID:      r.PromotionID,
		SnapshotAt:       r.SnapshotAt,
		WindowDays:       r.WindowDays,
		ActualTradeCount: r.ActualTradeCount,
		DecayFlag:        r.DecayFlag,
		Notes:            nullableString(r.Notes),
	}
	if r.ActualSharpe.Valid {
		v := r.ActualSharpe.Float64
		out.ActualSharpe = &v
	}
	if r.ActualReturn.Valid {
		v := r.ActualReturn.Float64
		out.ActualReturn = &v
	}
	if r.ActualMaxDrawdown.Valid {
		v := r.ActualMaxDrawdown.Float64
		out.ActualMaxDrawdown = &v
	}
	if r.SharpeDecayRatio.Valid {
		v := r.SharpeDecayRatio.Float64
		out.SharpeDecayRatio = &v
	}
	return out
}
