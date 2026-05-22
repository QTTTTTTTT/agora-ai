// Package navcalc computes a fund's Net Asset Value (NAV) using a clear,
// auditable formula:
//
//	NAV per unit = (cash + market_value - accrued_fees) / units_outstanding
//
// The legacy implementation in cmd/server/wiring_adapters.go used
// `total_assets / initial_capital`, which conflates "total return per yuan
// initially funded" with "per-unit NAV". That formula breaks under
// subscriptions/redemptions and cannot incorporate accrued management or
// performance fees. This package gives the rest of the system a single
// source of truth for NAV math.
//
// The package has no external dependencies and works on plain value structs
// so it can be unit-tested deterministically and reused by both the daily
// NAV snapshot job and the subscription/redemption pricing flow.
package navcalc

import (
	"errors"
	"math"
)

// Inputs holds everything required to compute a fund's NAV at a point in time.
type Inputs struct {
	// Cash is the fund's settled, available cash.
	Cash float64
	// MarketValue is the aggregate mark-to-market value of all open
	// positions, including unrealized P&L.
	MarketValue float64
	// AccruedManagementFee is the management fee that has been accrued but
	// not yet paid out. Reduces NAV until paid.
	AccruedManagementFee float64
	// AccruedPerformanceFee is the performance fee accrued but unpaid.
	AccruedPerformanceFee float64
	// OtherAccruedFees groups any other accruals (carry, audit, custodian).
	OtherAccruedFees float64
	// UnitsOutstanding is the number of fund units issued and not redeemed.
	// Must be > 0 for per-unit NAV to be defined.
	UnitsOutstanding float64
}

// Result is a snapshot of a NAV computation.
type Result struct {
	TotalAssets        float64 // cash + market value (gross)
	NetAssets          float64 // total assets - accrued fees
	UnitsOutstanding   float64
	NAVPerUnit         float64 // net assets / units
	AccruedFeesTotal   float64
}

// ErrUnitsOutstanding is returned when units_outstanding is non-positive.
var ErrUnitsOutstanding = errors.New("navcalc: units_outstanding must be > 0")

// Compute returns the NAV result for in. Negative cash or market value is
// allowed (a fund can be net short or carrying margin debt) but
// units_outstanding must be strictly positive.
func Compute(in Inputs) (Result, error) {
	if in.UnitsOutstanding <= 0 || math.IsNaN(in.UnitsOutstanding) {
		return Result{}, ErrUnitsOutstanding
	}
	gross := in.Cash + in.MarketValue
	accrued := in.AccruedManagementFee + in.AccruedPerformanceFee + in.OtherAccruedFees
	net := gross - accrued
	return Result{
		TotalAssets:      gross,
		NetAssets:        net,
		UnitsOutstanding: in.UnitsOutstanding,
		NAVPerUnit:       net / in.UnitsOutstanding,
		AccruedFeesTotal: accrued,
	}, nil
}

// DailyReturn computes the simple return between two NAV-per-unit snapshots.
// Returns 0 if previous is non-positive (e.g. a brand-new fund's first day).
func DailyReturn(previous, current float64) float64 {
	if previous <= 0 {
		return 0
	}
	return (current - previous) / previous
}

// CumulativeReturn computes the simple return relative to the inception NAV
// (typically 1.0). Returns 0 if inception is non-positive.
func CumulativeReturn(inception, current float64) float64 {
	if inception <= 0 {
		return 0
	}
	return (current - inception) / inception
}
