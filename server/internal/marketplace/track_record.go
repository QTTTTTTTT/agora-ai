package marketplace

// Forward-test track record & listing eligibility.
//
// This file owns two related concepts:
//
//  1. TrackRecord — objective, NAV-derived performance numbers (Sharpe,
//     max drawdown, total return, win rate, trading-day count) computed
//     from a forward-test NAV time series. No backtest data is mixed in.
//
//  2. EligibilityPolicy — the rule a fund must satisfy before it can be
//     listed on the marketplace. Today there is no such gate; this code
//     introduces the minimum forward-test window and a clear error type
//     so the wiring layer can refuse a CreateListing call with a
//     user-readable reason.
//
// The whole module is pure (no DB, no clocks captured at package level)
// so it can be exhaustively unit-tested.

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

// ---------------------------------------------------------------------------
// Track record computation
// ---------------------------------------------------------------------------

// NAVObservation is a single (date, NAV) point on the forward-test curve.
//
// We intentionally use time.Time rather than a string to make ordering and
// inception arithmetic unambiguous. Callers loading from `nav_snapshots`
// should parse `trading_date` once at the boundary.
type NAVObservation struct {
	Date time.Time
	NAV  float64
}

// TrackRecord summarises a forward-test NAV series.
//
// Conventions:
//   - TotalReturn is the cumulative return from the first to the last
//     observation, i.e. NAV[n-1]/NAV[0] - 1.
//   - AnnualReturn annualises TotalReturn over the actual elapsed wall
//     time (not the number of observations), using 365-day years.
//   - Sharpe uses daily simple returns, no risk-free rate adjustment, and
//     scales by sqrt(252). Returns 0 when there are <2 observations or
//     std-dev is zero.
//   - MaxDrawdown is the largest (peak - trough) / peak observed along
//     the way, expressed as a non-negative fraction.
//   - WinRate is the share of daily simple returns strictly greater than 0.
//   - LiveDays is the integer number of full days between first and last
//     observation, floor-divided.
//   - DataPoints is len(observations).
type TrackRecord struct {
	TotalReturn  float64
	AnnualReturn float64
	Sharpe       float64
	MaxDrawdown  float64
	Volatility   float64
	WinRate      float64
	LiveDays     int
	DataPoints   int
	StartDate    time.Time
	EndDate      time.Time
}

// ErrInsufficientNAVHistory is returned when the input has fewer than two
// observations — every metric collapses in that case so we refuse to
// fabricate one.
var ErrInsufficientNAVHistory = errors.New("marketplace: at least 2 NAV observations required")

// ComputeTrackRecord produces a TrackRecord from a forward-test NAV series.
//
// The input is sorted internally; callers don't have to pre-sort. NAVs ≤ 0
// are treated as data errors and skipped (a 0 NAV would produce -inf log
// returns). After filtering, at least 2 valid points must remain.
func ComputeTrackRecord(obs []NAVObservation) (TrackRecord, error) {
	cleaned := make([]NAVObservation, 0, len(obs))
	for _, o := range obs {
		if o.NAV <= 0 || o.Date.IsZero() {
			continue
		}
		cleaned = append(cleaned, o)
	}
	if len(cleaned) < 2 {
		return TrackRecord{}, ErrInsufficientNAVHistory
	}

	sort.SliceStable(cleaned, func(i, j int) bool {
		return cleaned[i].Date.Before(cleaned[j].Date)
	})

	first := cleaned[0]
	last := cleaned[len(cleaned)-1]

	// Daily simple returns.
	rets := make([]float64, 0, len(cleaned)-1)
	for i := 1; i < len(cleaned); i++ {
		prev := cleaned[i-1].NAV
		if prev <= 0 {
			continue
		}
		rets = append(rets, cleaned[i].NAV/prev-1)
	}

	totalRet := last.NAV/first.NAV - 1

	elapsedDays := int(last.Date.Sub(first.Date).Hours() / 24)
	years := float64(elapsedDays) / 365.0
	annual := 0.0
	if years > 0 {
		annual = math.Pow(1+totalRet, 1/years) - 1
	}

	mean, stdev := meanStdev(rets)
	sharpe := 0.0
	if stdev > 0 {
		sharpe = (mean / stdev) * math.Sqrt(252)
	}

	maxDD := 0.0
	peak := cleaned[0].NAV
	for _, o := range cleaned {
		if o.NAV > peak {
			peak = o.NAV
		}
		if peak > 0 {
			dd := (peak - o.NAV) / peak
			if dd > maxDD {
				maxDD = dd
			}
		}
	}

	wins := 0
	for _, r := range rets {
		if r > 0 {
			wins++
		}
	}
	winRate := 0.0
	if len(rets) > 0 {
		winRate = float64(wins) / float64(len(rets))
	}

	return TrackRecord{
		TotalReturn:  totalRet,
		AnnualReturn: annual,
		Sharpe:       sharpe,
		MaxDrawdown:  maxDD,
		Volatility:   stdev * math.Sqrt(252),
		WinRate:      winRate,
		LiveDays:     elapsedDays,
		DataPoints:   len(cleaned),
		StartDate:    first.Date,
		EndDate:      last.Date,
	}, nil
}

func meanStdev(xs []float64) (mean, stdev float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	sum := 0.0
	for _, x := range xs {
		sum += x
	}
	mean = sum / float64(len(xs))
	if len(xs) < 2 {
		return mean, 0
	}
	sq := 0.0
	for _, x := range xs {
		d := x - mean
		sq += d * d
	}
	stdev = math.Sqrt(sq / float64(len(xs)-1))
	return mean, stdev
}

// ---------------------------------------------------------------------------
// Eligibility policy
// ---------------------------------------------------------------------------

// DefaultMinForwardTestDays is the platform-wide minimum forward-test
// window before a fund may be listed. 30 days is short enough to keep
// the marketplace populated but long enough that "I made one good trade"
// listings cannot pass.
const DefaultMinForwardTestDays = 30

// EligibilityPolicy bundles the listing gate parameters. Callers can
// override per-platform; zero values fall back to defaults.
type EligibilityPolicy struct {
	// MinForwardTestDays is the floor on how many days must have passed
	// since the fund went live. 0 disables the gate (legacy behaviour).
	MinForwardTestDays int

	// MinDataPoints is the floor on how many NAV observations the fund
	// must have. Independent from the day count: a fund could be 90 days
	// old but only have 5 NAV snapshots if the workflow has been broken.
	MinDataPoints int

	// AllowSimulationOnly, when true, lets funds that never went live
	// (no live_since) be listed. Defaults to false: simulation funds
	// cannot be sold — only forward-tested ones.
	AllowSimulationOnly bool
}

func (p EligibilityPolicy) minDays() int {
	if p.MinForwardTestDays > 0 {
		return p.MinForwardTestDays
	}
	return DefaultMinForwardTestDays
}

func (p EligibilityPolicy) minPoints() int {
	if p.MinDataPoints > 0 {
		return p.MinDataPoints
	}
	// At minimum we need 2 NAVs to compute a return; default to a more
	// meaningful 10 to filter out funds with broken workflows.
	return 10
}

// EligibilityInputs is everything the policy needs to make a yes/no call.
type EligibilityInputs struct {
	// LiveSince is the timestamp the fund was flipped to live trading,
	// or the zero value if the fund never went live.
	LiveSince time.Time

	// Now is "now" — passed in rather than read from the clock so tests
	// can stay deterministic.
	Now time.Time

	// NAVPoints is the count of forward-test NAV observations available
	// for the fund. Cheap to compute (`SELECT COUNT(*)`).
	NAVPoints int
}

// EligibilityError describes exactly why a listing was refused. It carries
// a structured `Reason` so the API layer can render i18n messages and the
// raw numbers so the seller can see how close they are to passing.
type EligibilityError struct {
	Reason       string // "not_live" | "insufficient_days" | "insufficient_data"
	RequiredDays int
	HaveDays     int
	RequiredData int
	HaveData     int
}

func (e *EligibilityError) Error() string {
	switch e.Reason {
	case "not_live":
		return "marketplace: fund must be in live forward-test mode before listing"
	case "insufficient_days":
		return fmt.Sprintf(
			"marketplace: fund needs %d forward-test days, has %d",
			e.RequiredDays, e.HaveDays,
		)
	case "insufficient_data":
		return fmt.Sprintf(
			"marketplace: fund needs %d NAV observations, has %d",
			e.RequiredData, e.HaveData,
		)
	}
	return "marketplace: listing not eligible"
}

// CheckEligibility returns nil if the inputs satisfy the policy, otherwise
// an *EligibilityError describing the failure mode. Successful return also
// reports `liveDays` so the caller can persist it on the listing row.
func (p EligibilityPolicy) CheckEligibility(in EligibilityInputs) (liveDays int, err error) {
	if in.LiveSince.IsZero() {
		if p.AllowSimulationOnly {
			return 0, nil
		}
		return 0, &EligibilityError{Reason: "not_live"}
	}

	if in.Now.Before(in.LiveSince) {
		// Clock skew or bad data — treat as not yet live.
		return 0, &EligibilityError{
			Reason:       "insufficient_days",
			RequiredDays: p.minDays(),
			HaveDays:     0,
		}
	}

	liveDays = int(in.Now.Sub(in.LiveSince).Hours() / 24)
	if liveDays < p.minDays() {
		return liveDays, &EligibilityError{
			Reason:       "insufficient_days",
			RequiredDays: p.minDays(),
			HaveDays:     liveDays,
		}
	}

	if in.NAVPoints < p.minPoints() {
		return liveDays, &EligibilityError{
			Reason:       "insufficient_data",
			RequiredData: p.minPoints(),
			HaveData:     in.NAVPoints,
		}
	}

	return liveDays, nil
}
