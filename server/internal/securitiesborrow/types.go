// Package securitiesborrow models the S6.4 short-borrow stack:
// pre-trade locate decisions and end-of-day borrow-fee accrual.
//
// What this package owns
//
//   - Domain types: BorrowRate, Availability, LocateProbe,
//     LocateDecision, AccrualProbe, AccrualResult.
//   - Pure decision logic for locate (allow / reject) and for
//     daily fee calculation. No DB / I/O.
//   - DB-backed Repo for the three storage tables and an
//     in-memory Cache for the rate calibration table (so the
//     synchronous broker hot path doesn't touch SQL on every
//     short order).
//
// What it does NOT own
//
//   - Loading the position quantity or the closing price — the
//     cmd/server adapter does that and hands a populated probe
//     to the engine.
//   - Wiring into the broker simulator and into the daily loop.
//     Those live in cmd/server (broker_gate.go,
//     borrow_accrual_loop.go) so this package stays free of
//     broker / cash_ledger dependencies.
//
// Domain model
//
// In real markets a fund that wants to short stock X must:
//
//   1. Locate borrowable shares (Reg SHO). The simulator
//      reads the calibration table at order time, decides
//      whether the security is borrowable in the requested
//      size, and either accepts (charging an optional one-time
//      locate fee) or rejects with a structured reason.
//
//   2. Pay daily borrow fees while the position is open.
//      Fee = abs(short_qty) * close_price * rate / day_count.
//      Booked at EOD to the cash_ledger as a debit.
//
// Both calculations are pure functions; the DB is only used for
// the calibration data and the audit ledgers.
package securitiesborrow

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Availability is the closed enum from the schema. We refine
// the operator's intent ("hard but borrowable" vs "outright
// unavailable") so analytics can pivot on it.
type Availability string

const (
	AvailabilityEasy        Availability = "easy"
	AvailabilityHard        Availability = "hard"
	AvailabilityRestricted  Availability = "restricted"
	AvailabilityUnavailable Availability = "unavailable"
)

// AllAvailabilities is the canonical list. Validated by the
// admin handler.
var AllAvailabilities = []Availability{
	AvailabilityEasy, AvailabilityHard, AvailabilityRestricted, AvailabilityUnavailable,
}

// IsValidAvailability mirrors the SQL CHECK.
func IsValidAvailability(s string) bool {
	v := Availability(strings.ToLower(strings.TrimSpace(s)))
	for _, a := range AllAvailabilities {
		if a == v {
			return true
		}
	}
	return false
}

// CalibrationSource matches the source enum in the schema.
type CalibrationSource string

const (
	SourceManual                 CalibrationSource = "manual"
	SourceBrokerQuote            CalibrationSource = "broker_quote"
	SourceAgentLender            CalibrationSource = "agent_lender"
	SourceHistoricalCalibration  CalibrationSource = "historical_calibration"
	SourcePublicFeed             CalibrationSource = "public_feed"
)

// AllCalibrationSources is the closed list.
var AllCalibrationSources = []CalibrationSource{
	SourceManual, SourceBrokerQuote, SourceAgentLender,
	SourceHistoricalCalibration, SourcePublicFeed,
}

// IsValidCalibrationSource mirrors the SQL CHECK.
func IsValidCalibrationSource(s string) bool {
	v := CalibrationSource(strings.ToLower(strings.TrimSpace(s)))
	for _, src := range AllCalibrationSources {
		if src == v {
			return true
		}
	}
	return false
}

// BorrowRate is one row of security_borrow_rates. Pointer
// fields distinguish "no value" from "explicit zero".
type BorrowRate struct {
	ID                  string
	InstrumentKey       string
	Symbol              string
	Market              string
	AssetClass          string
	BorrowRateBpsAnnual float64
	LocateFeeBps        float64
	Availability        Availability
	AvailableShares     *int64
	MinLocateQty        *int64
	MaxLocateQty        *int64
	Source              CalibrationSource
	LastCalibratedAt    time.Time
	Note                string
	UpdatedBy           string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// LocateProbe is the engine input for a pre-trade locate
// decision.
type LocateProbe struct {
	FundID         string
	InstrumentKey  string
	Symbol         string
	RequestedQty   float64  // shares to borrow (always positive)
	IntendedPrice  float64  // for notional + fee calc
	ClientOrderID  string
}

// LocateDecisionKind is the closed verdict vocabulary.
type LocateDecisionKind string

const (
	LocateAllow            LocateDecisionKind = "allow"
	LocateRejectUnavail    LocateDecisionKind = "reject_unavailable"
	LocateRejectInsuff     LocateDecisionKind = "reject_insufficient"
	LocateRejectBelowMin   LocateDecisionKind = "reject_below_min"
	LocateRejectAboveMax   LocateDecisionKind = "reject_above_max"
	LocateNoCalibration    LocateDecisionKind = "no_calibration"
	LocateFailOpen         LocateDecisionKind = "fail_open"
)

// LocateDecision is the engine output. Pointer fields render as
// omitempty in JSON; AvailableShares is only meaningful when the
// rate row had a value to begin with.
type LocateDecision struct {
	Kind             LocateDecisionKind
	Allowed          bool
	RequestedQty     float64
	AvailableShares  *int64
	BorrowRateBps    float64
	LocateFeeBps     float64
	LocateFeeAmount  float64  // requestedQty * price * fee_bps / 10000
	Notional         float64  // requestedQty * price
	IntendedPrice    float64
	Reason           string
	// Source carries the rate's provenance into the audit log.
	Source CalibrationSource
}

// LocateEngine is the pure decision logic. Stateless.
type LocateEngine struct{}

// NewLocateEngine returns the engine.
func NewLocateEngine() *LocateEngine { return &LocateEngine{} }

// Evaluate runs the locate rule. Pure: no DB, no I/O.
//
// The rate parameter is the cache lookup result. nil → no
// calibration on file: returns NoCalibration. The cmd/server
// adapter decides whether that's allow-with-warning (for
// optimistic ETB defaults) or hard-reject; the engine just
// reports the fact.
func (LocateEngine) Evaluate(probe LocateProbe, rate *BorrowRate) LocateDecision {
	d := LocateDecision{
		RequestedQty:  probe.RequestedQty,
		IntendedPrice: probe.IntendedPrice,
	}
	if probe.IntendedPrice > 0 {
		d.Notional = probe.RequestedQty * probe.IntendedPrice
	}
	if rate == nil {
		d.Kind = LocateNoCalibration
		d.Reason = "no calibration row for instrument"
		return d
	}
	d.BorrowRateBps = rate.BorrowRateBpsAnnual
	d.LocateFeeBps = rate.LocateFeeBps
	d.AvailableShares = rate.AvailableShares
	d.Source = rate.Source

	if rate.Availability == AvailabilityUnavailable {
		d.Kind = LocateRejectUnavail
		d.Reason = "borrow unavailable: " + string(rate.Availability)
		return d
	}
	if probe.RequestedQty <= 0 {
		// Defensive: caller should never send 0/negative qty.
		d.Kind = LocateRejectInsuff
		d.Reason = "requested qty must be positive"
		return d
	}
	if rate.MinLocateQty != nil && int64(probe.RequestedQty) < *rate.MinLocateQty {
		d.Kind = LocateRejectBelowMin
		d.Reason = fmt.Sprintf("locate qty %v below minimum %d", probe.RequestedQty, *rate.MinLocateQty)
		return d
	}
	if rate.MaxLocateQty != nil && int64(probe.RequestedQty) > *rate.MaxLocateQty {
		d.Kind = LocateRejectAboveMax
		d.Reason = fmt.Sprintf("locate qty %v above maximum %d", probe.RequestedQty, *rate.MaxLocateQty)
		return d
	}
	if rate.AvailableShares != nil && int64(probe.RequestedQty) > *rate.AvailableShares {
		d.Kind = LocateRejectInsuff
		d.Reason = fmt.Sprintf("only %d shares available, requested %v", *rate.AvailableShares, probe.RequestedQty)
		return d
	}

	// ALLOW path: compute the optional one-time locate fee.
	d.Kind = LocateAllow
	d.Allowed = true
	if rate.LocateFeeBps > 0 && d.Notional > 0 {
		d.LocateFeeAmount = d.Notional * rate.LocateFeeBps / 10000.0
	}
	switch rate.Availability {
	case AvailabilityHard:
		d.Reason = "allowed: hard-to-borrow"
	case AvailabilityRestricted:
		d.Reason = "allowed: restricted (admin override)"
	default:
		d.Reason = "allowed"
	}
	return d
}

// ----- Daily accrual -----

// AccrualProbe is the input for one fund × instrument × day.
type AccrualProbe struct {
	FundID           string
	InstrumentKey    string
	Symbol           string
	AccrualDate      time.Time  // truncated to date in adapter
	ShortQty         float64    // always positive (abs of holding qty)
	MarketPrice      float64    // closing price for the day
	RateBpsAnnual    float64    // resolved by adapter from cache
	DayCountBasis    int        // 360 or 365; defaults to 365 if 0
}

// AccrualResult is the engine output. fee_amount = notional *
// rate / day_count. Always non-negative.
type AccrualResult struct {
	Notional      float64
	DailyRate     float64  // bps_annual / day_count / 10000 (decimal fraction)
	FeeAmount     float64
	DayCountBasis int
	Reason        string
}

// AccrualEngine is the pure accrual logic.
type AccrualEngine struct{}

// NewAccrualEngine returns the engine.
func NewAccrualEngine() *AccrualEngine { return &AccrualEngine{} }

// Evaluate computes one day of borrow fee. Pure: no I/O.
//
// Skips:
//
//   - ShortQty <= 0    → 0 fee, "no short position"
//   - MarketPrice <= 0 → 0 fee, "missing closing price"
//   - RateBpsAnnual <= 0 → 0 fee, "no borrow cost"
//
// Each skip path is reported in Reason so the loop can decide
// whether to log a debug or a warn line.
func (AccrualEngine) Evaluate(probe AccrualProbe) AccrualResult {
	out := AccrualResult{DayCountBasis: probe.DayCountBasis}
	if out.DayCountBasis != 360 && out.DayCountBasis != 365 {
		out.DayCountBasis = 365
	}
	if probe.ShortQty <= 0 {
		out.Reason = "no short position"
		return out
	}
	if probe.MarketPrice <= 0 {
		out.Reason = "missing closing price"
		return out
	}
	if probe.RateBpsAnnual <= 0 {
		out.Reason = "zero borrow cost"
		return out
	}
	out.Notional = probe.ShortQty * probe.MarketPrice
	out.DailyRate = probe.RateBpsAnnual / float64(out.DayCountBasis) / 10000.0
	out.FeeAmount = out.Notional * out.DailyRate
	out.Reason = "accrued"
	return out
}

// ----- LocateEvent (audit log row shape) -----

// LocateEvent is one row of security_locate_events. Returned by
// the repo's list function for the admin "Locate audit" panel.
type LocateEvent struct {
	ID              string
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
	CreatedAt       time.Time
}

// ----- BorrowLedgerEntry (daily fee row shape) -----

// BorrowLedgerEntry is one row of short_position_borrow_ledger.
type BorrowLedgerEntry struct {
	ID                string
	FundID            string
	InstrumentKey     string
	Symbol            string
	AccrualDate       time.Time
	ShortQty          float64
	MarketPrice       float64
	Notional          float64
	RateBpsAnnual     float64
	DayCountBasis     int
	FeeAmount         float64
	CashLedgerEntryID string
	CreatedAt         time.Time
}

// ----- Errors -----

var ErrInvalidRow = errors.New("securitiesborrow: invalid row")
