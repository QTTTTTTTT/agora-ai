// Package riskbudget computes a per-trade risk multiplier the PM
// applies on top of its baseline R-per-trade.
//
// Why this exists. The static per-trade R% (default 50 bps) the
// sizing package uses is right on average but wrong in two
// directions:
//
//  1. When realised portfolio vol is well below the fund's vol
//     target the fund is under-deploying its risk budget. Bridgewater
//     risk-parity, AHL DTP, Two Sigma's vol-target overlays all dial
//     R UP in this regime — the simplest version doubles R until
//     realised matches target.
//
//  2. When the fund is in a drawdown the same R% accelerates the
//     hole. AQR, Renaissance, every textbook on portfolio risk
//     management throttles R DOWN with deepening drawdown. The
//     throttle is asymmetric — it kicks in fast at the first 5%, the
//     second 15% halves R, and around 25% it caps at a 0.4× floor
//     so the fund can still trade out of the hole.
//
// Sprint B #2 surfaces both signals into the PM prompt as a single
// effectivePerTradeRiskPct so the LLM can size with the full picture
// instead of treating its baseline R as gospel. The sizing package
// stays the single execution authority — riskbudget is advisory and
// the PM is expected to use the throttle to scale qtyPct, not to
// fight it.
//
// I/O contract. The Service depends on a thin *sql.DB read against
// nav_snapshots and is side-effect free. Empty fundID, nil DB,
// fewer-than-floor history rows all degrade gracefully to (nil, nil)
// so the wiring layer can call it unconditionally.
//
// Math notes:
//
//   - Daily log returns from NAV: ln(nav_t / nav_{t-1}).
//   - Realised vol = stdev(returns) * sqrt(252) (annualised).
//   - Drawdown = (peak - current) / peak over the same window;
//     peak is the running max NAV across the lookback.
//   - VolScalar = clamp(VolTarget / RealisedVol, 0.5, 2.0).
//   - DDScalar  = clamp(1 - DD / DDCeiling, 0.4, 1.0).
//   - EffectiveR = BaseR * VolScalar * DDScalar.
//
// The clamps are deliberately conservative. We don't want a 2-week
// quiet period to push R to 5× and then a single bad day to
// vaporise the recovered ground.
package riskbudget

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"
)

// Snapshot is the prompt-facing render of the fund's current vol /
// drawdown / effective R picture. All floats are scrubbed for NaN /
// Inf before they leave the Service, so the prompt JSON is always
// valid.
type Snapshot struct {
	// Window: human-readable description of the lookback used
	// for both vol and DD computations. Comes from
	// Options.LookbackDays.
	Window string

	// SampleSize is the number of valid daily NAV rows
	// the computation used. < MinSamples → no Snapshot is
	// returned at all (BuildSnapshot returns nil).
	SampleSize int

	// BasePerTradeRiskPct is the fund's static R% baseline as
	// configured (defaults to 0.005 = 50 bps). Echoed back to
	// the prompt so the model can show its working.
	BasePerTradeRiskPct float64

	// RealisedVolAnnualized is the annualised stdev of the
	// daily NAV log returns. Always non-negative.
	RealisedVolAnnualized float64

	// VolTargetAnnualized is the configured vol target (default
	// 0.15 = 15% annualised, matching AHL DTP / typical vol-
	// target overlays). Echoed for transparency.
	VolTargetAnnualized float64

	// VolScalar is clamp(VolTarget / RealisedVol, 0.5, 2.0).
	// >1 means we're under-deploying R; <1 means we're over.
	VolScalar float64

	// PeakNAV is the running max NAV over the window; the
	// reference point against which DrawdownPct is measured.
	PeakNAV float64

	// CurrentNAV is the last NAV row in the window. PeakNAV
	// ≥ CurrentNAV always (otherwise CurrentNAV would BE the
	// peak).
	CurrentNAV float64

	// DrawdownPct is (PeakNAV - CurrentNAV) / PeakNAV, clamped
	// to [0, 1]. Zero when the fund is at all-time highs over
	// the lookback.
	DrawdownPct float64

	// DDCeilingPct is the drawdown threshold at which the
	// throttle hits its 0.4× floor. Default 0.25 = a 25% DD
	// halves-or-more R.
	DDCeilingPct float64

	// DDScalar is clamp(1 - DD/DDCeiling, 0.4, 1.0). 1.0 when
	// the fund is at the peak; floor at 0.4 once DD reaches
	// DDCeiling.
	DDScalar float64

	// EffectivePerTradeRiskPct = Base * VolScalar * DDScalar.
	// The number the PM is told to size with.
	EffectivePerTradeRiskPct float64
}

// Options configures the lookback, vol target, drawdown ceiling,
// base R, and minimum sample count. The zero value is fine;
// withDefaults installs production tunings.
type Options struct {
	// LookbackDays is the rolling window for both vol and DD
	// computations. 60 trading days ≈ 3 months, the convention
	// for vol-target overlays. Clamped to [10, 252].
	LookbackDays int

	// MinSamples is the minimum valid NAV row count needed
	// before BuildSnapshot returns a result. Below this, the
	// vol estimate is too noisy to act on and we degrade to
	// nil. Clamped to [5, LookbackDays].
	MinSamples int

	// BasePerTradeRiskPct is the static R% the wiring layer
	// otherwise tells sizing.Policy to use. We echo it into the
	// Snapshot so the PM prompt sees what the multiplier is
	// adjusting from. Clamped to [0.001, 0.05].
	BasePerTradeRiskPct float64

	// VolTargetAnnualized is the annualised vol the throttle
	// aims for. 15% is the AHL DTP convention; we use the same.
	// Clamped to [0.05, 0.40].
	VolTargetAnnualized float64

	// VolFloor / VolCeil clamp the VolScalar so a quiet (or
	// noisy) period can't blow R up (or down) catastrophically.
	// Default [0.5, 2.0].
	VolScalarFloor float64
	VolScalarCeil  float64

	// DDCeilingPct is the drawdown level at which the throttle
	// hits its floor. Default 0.25 (25%); clamped to [0.05,
	// 0.60].
	DDCeilingPct float64

	// DDScalarFloor is the lowest the drawdown throttle can
	// push R. Default 0.4 — even in a 30% DD the fund can still
	// trade at 40% of base R, otherwise it can't recover.
	DDScalarFloor float64
}

func (o Options) withDefaults() Options {
	if o.LookbackDays <= 0 {
		o.LookbackDays = 60
	}
	if o.LookbackDays < 10 {
		o.LookbackDays = 10
	}
	if o.LookbackDays > 252 {
		o.LookbackDays = 252
	}
	if o.MinSamples <= 0 {
		o.MinSamples = 20
	}
	if o.MinSamples < 5 {
		o.MinSamples = 5
	}
	if o.MinSamples > o.LookbackDays {
		o.MinSamples = o.LookbackDays
	}
	if o.BasePerTradeRiskPct <= 0 {
		o.BasePerTradeRiskPct = 0.005
	}
	if o.BasePerTradeRiskPct < 0.001 {
		o.BasePerTradeRiskPct = 0.001
	}
	if o.BasePerTradeRiskPct > 0.05 {
		o.BasePerTradeRiskPct = 0.05
	}
	if o.VolTargetAnnualized <= 0 {
		o.VolTargetAnnualized = 0.15
	}
	if o.VolTargetAnnualized < 0.05 {
		o.VolTargetAnnualized = 0.05
	}
	if o.VolTargetAnnualized > 0.40 {
		o.VolTargetAnnualized = 0.40
	}
	if o.VolScalarFloor <= 0 {
		o.VolScalarFloor = 0.5
	}
	if o.VolScalarCeil <= 0 {
		o.VolScalarCeil = 2.0
	}
	if o.VolScalarCeil < o.VolScalarFloor {
		o.VolScalarCeil = o.VolScalarFloor
	}
	if o.DDCeilingPct <= 0 {
		o.DDCeilingPct = 0.25
	}
	if o.DDCeilingPct < 0.05 {
		o.DDCeilingPct = 0.05
	}
	if o.DDCeilingPct > 0.60 {
		o.DDCeilingPct = 0.60
	}
	if o.DDScalarFloor <= 0 {
		o.DDScalarFloor = 0.4
	}
	if o.DDScalarFloor < 0.1 {
		o.DDScalarFloor = 0.1
	}
	if o.DDScalarFloor > 1.0 {
		o.DDScalarFloor = 1.0
	}
	return o
}

// Service is the only public type. Stateless apart from the
// configured Options.
type Service struct {
	db   *sql.DB
	opts Options
}

// NewService is the only constructor. Passing a nil db produces a
// degenerate service whose BuildSnapshot is a no-op.
func NewService(db *sql.DB, opts Options) *Service {
	return &Service{db: db, opts: opts.withDefaults()}
}

// Options exposes the effective configuration for diagnostics.
// Safe on nil receivers — returns the default options.
func (s *Service) Options() Options {
	if s == nil {
		return Options{}.withDefaults()
	}
	return s.opts
}

// BuildSnapshot is the only public method. Reads the most recent
// `opts.LookbackDays` nav_snapshots rows for `fundID`, computes the
// vol / DD / scalar trio, and returns the prompt-facing Snapshot.
//
// Returns nil (no error) when:
//   - the service / db is nil
//   - fundID is blank
//   - fewer than MinSamples NAV rows exist in the window
//
// Returns (nil, error) only on a SQL failure. The wiring layer is
// expected to log + ignore — risk budget is advisory, not load-
// bearing.
func (s *Service) BuildSnapshot(ctx context.Context, fundID string, now time.Time) (*Snapshot, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	fundID = strings.TrimSpace(fundID)
	if fundID == "" {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	// Calendar lookback in days, not strictly trading days — DB
	// query then filters by trading_date. The +5 buffer absorbs
	// holiday gaps without losing the floor of valid rows.
	from := now.AddDate(0, 0, -(s.opts.LookbackDays + 5))

	const q = `
		SELECT trading_date, nav
		  FROM nav_snapshots
		 WHERE fund_id = $1
		   AND trading_date >= $2
		 ORDER BY trading_date ASC
	`
	rows, err := s.db.QueryContext(ctx, q, fundID, from)
	if err != nil {
		return nil, fmt.Errorf("riskbudget: query nav_snapshots: %w", err)
	}
	defer rows.Close()

	type point struct {
		date time.Time
		nav  float64
	}
	pts := make([]point, 0, s.opts.LookbackDays)
	for rows.Next() {
		var (
			d   time.Time
			nav float64
		)
		if err := rows.Scan(&d, &nav); err != nil {
			return nil, fmt.Errorf("riskbudget: scan: %w", err)
		}
		if nav <= 0 {
			continue // skip rows that would NaN our log return
		}
		pts = append(pts, point{date: d.UTC(), nav: nav})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("riskbudget: rows: %w", err)
	}
	// Keep at most LookbackDays trading rows (tail) so the calc
	// matches the documented window even when the DB happens to
	// have a longer history.
	if len(pts) > s.opts.LookbackDays {
		pts = pts[len(pts)-s.opts.LookbackDays:]
	}
	if len(pts) < s.opts.MinSamples {
		return nil, nil
	}

	// Daily log returns. len = len(pts) - 1.
	returns := make([]float64, 0, len(pts)-1)
	for i := 1; i < len(pts); i++ {
		r := math.Log(pts[i].nav / pts[i-1].nav)
		if math.IsNaN(r) || math.IsInf(r, 0) {
			continue
		}
		returns = append(returns, r)
	}
	if len(returns) < s.opts.MinSamples-1 { // we need at least MinSamples nav rows ⇒ MinSamples-1 returns
		return nil, nil
	}
	realisedVol := stdev(returns) * math.Sqrt(252)

	// Drawdown over the same window.
	peak := pts[0].nav
	for _, p := range pts {
		if p.nav > peak {
			peak = p.nav
		}
	}
	current := pts[len(pts)-1].nav
	ddPct := 0.0
	if peak > 0 {
		ddPct = (peak - current) / peak
	}
	if ddPct < 0 {
		ddPct = 0
	}
	if ddPct > 1 {
		ddPct = 1
	}

	// Scalars. A zero realised vol (a perfectly flat NAV
	// window — rare in production but common in fresh /
	// simulation funds) means the fund is using *none* of its
	// vol budget; per the AHL DTP convention we snap straight
	// to the configured ceiling rather than defaulting to 1.0
	// (which would silently under-deploy when the fund is in
	// the deepest possible "should size up" state).
	volScalar := s.opts.VolScalarCeil
	if realisedVol > 0 {
		volScalar = s.opts.VolTargetAnnualized / realisedVol
	}
	volScalar = clamp(volScalar, s.opts.VolScalarFloor, s.opts.VolScalarCeil)

	ddScalar := 1.0
	if s.opts.DDCeilingPct > 0 {
		ddScalar = 1.0 - ddPct/s.opts.DDCeilingPct
	}
	ddScalar = clamp(ddScalar, s.opts.DDScalarFloor, 1.0)

	effective := s.opts.BasePerTradeRiskPct * volScalar * ddScalar

	return &Snapshot{
		Window:                   fmt.Sprintf("%d trading days", s.opts.LookbackDays),
		SampleSize:               len(pts),
		BasePerTradeRiskPct:      safe(s.opts.BasePerTradeRiskPct),
		RealisedVolAnnualized:    safe(realisedVol),
		VolTargetAnnualized:      safe(s.opts.VolTargetAnnualized),
		VolScalar:                safe(volScalar),
		PeakNAV:                  safe(peak),
		CurrentNAV:               safe(current),
		DrawdownPct:              safe(ddPct),
		DDCeilingPct:             safe(s.opts.DDCeilingPct),
		DDScalar:                 safe(ddScalar),
		EffectivePerTradeRiskPct: safe(effective),
	}, nil
}

// stdev returns the sample standard deviation. Empty slice / single
// value → 0 (no spread). Matches numpy.std(ddof=1) on the same input
// modulo floating point.
func stdev(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	var sse float64
	for _, x := range xs {
		d := x - mean
		sse += d * d
	}
	return math.Sqrt(sse / float64(len(xs)-1))
}

// clamp restricts x to [lo, hi]. lo / hi are validated by
// withDefaults; this helper assumes lo <= hi.
func clamp(x, lo, hi float64) float64 {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

// safe scrubs NaN / ±Inf so a degenerate computation can never
// poison the prompt JSON.
func safe(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}
