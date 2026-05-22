// Package feeacct accrues fund-level management fees and performance fees in
// a way that is independent of database/persistence concerns. Callers feed
// daily NAV snapshots into the package and receive accrual amounts plus an
// updated high-water-mark; how those amounts get persisted (e.g. into a
// fee_accruals table) is a wiring concern.
//
// All rates are expressed as fractions: 0.02 = 2%/year. Days are calendar
// days; downstream code is free to use a 360- or 365-day basis by setting
// DaysInYear.
package feeacct

import "errors"

// ManagementFeeConfig describes a fund's management fee policy.
type ManagementFeeConfig struct {
	// AnnualRate is the annual management fee fraction, e.g. 0.02 = 2%.
	AnnualRate float64
	// DaysInYear is the day-count basis. Defaults to 365 when zero.
	DaysInYear int
}

// AccrueDailyManagementFee returns the management fee accrued for a single
// trading day, given the day's net asset value (after the previous day's
// fees were accrued but before today's). The amount is always non-negative.
//
// Formula: NAV * AnnualRate / DaysInYear.
func AccrueDailyManagementFee(cfg ManagementFeeConfig, nav float64) float64 {
	if cfg.AnnualRate <= 0 || nav <= 0 {
		return 0
	}
	days := cfg.DaysInYear
	if days <= 0 {
		days = 365
	}
	return nav * cfg.AnnualRate / float64(days)
}

// PerformanceFeeConfig describes a high-water-mark performance fee policy.
//
// The fee is computed at settlement time on the dollar gain above the
// high-water mark, after subtracting an optional hurdle return. If the
// portfolio is below HWM the fee is zero and the HWM stays put.
type PerformanceFeeConfig struct {
	// Rate is the performance fee fraction on excess gains, e.g. 0.20 = 20%.
	Rate float64
	// HurdleAnnualRate is an optional minimum annual return below which no
	// performance fee is charged, e.g. 0.05 = 5%/yr. Set 0 to disable.
	HurdleAnnualRate float64
	// DaysInYear is the day-count basis used to scale the hurdle to the
	// settlement period. Defaults to 365.
	DaysInYear int
}

// PerformanceFeeInputs describes the state needed to settle one period.
type PerformanceFeeInputs struct {
	// HighWaterMark is the previous peak NAV-per-unit. Use the inception
	// NAV (typically 1.0) at fund start.
	HighWaterMark float64
	// CurrentNAVPerUnit is the post-management-fee NAV-per-unit at
	// settlement.
	CurrentNAVPerUnit float64
	// UnitsOutstanding is the number of units at settlement.
	UnitsOutstanding float64
	// PeriodDays is the number of calendar days in the settlement period;
	// only used when a hurdle rate is configured.
	PeriodDays int
}

// PerformanceFeeResult is the output of SettlePerformanceFee.
type PerformanceFeeResult struct {
	// FeeAmount is the total fee owed for the period in fund currency.
	FeeAmount float64
	// NewHighWaterMark is the post-settlement HWM. It moves up if and only
	// if a fee was charged; otherwise it equals the input HWM.
	NewHighWaterMark float64
	// EligibleGainPerUnit is the per-unit gain above HWM (and hurdle).
	EligibleGainPerUnit float64
}

// ErrInvalidInputs is returned when units_outstanding or HWM is non-positive.
var ErrInvalidInputs = errors.New("feeacct: HWM and units_outstanding must be > 0")

// SettlePerformanceFee computes the high-water-mark performance fee for one
// settlement period. Returns ErrInvalidInputs if the inputs are unusable.
//
// When a hurdle rate is configured, the eligible gain is reduced by the
// hurdle accrued over the period: hurdle = HWM * HurdleAnnualRate * days/365.
// If the post-hurdle gain is non-positive, the fee is zero and HWM does not
// advance.
func SettlePerformanceFee(cfg PerformanceFeeConfig, in PerformanceFeeInputs) (PerformanceFeeResult, error) {
	if in.HighWaterMark <= 0 || in.UnitsOutstanding <= 0 {
		return PerformanceFeeResult{}, ErrInvalidInputs
	}
	if cfg.Rate <= 0 || in.CurrentNAVPerUnit <= in.HighWaterMark {
		return PerformanceFeeResult{NewHighWaterMark: in.HighWaterMark}, nil
	}

	gainPerUnit := in.CurrentNAVPerUnit - in.HighWaterMark

	if cfg.HurdleAnnualRate > 0 && in.PeriodDays > 0 {
		days := cfg.DaysInYear
		if days <= 0 {
			days = 365
		}
		hurdle := in.HighWaterMark * cfg.HurdleAnnualRate * float64(in.PeriodDays) / float64(days)
		gainPerUnit -= hurdle
	}
	if gainPerUnit <= 0 {
		return PerformanceFeeResult{NewHighWaterMark: in.HighWaterMark}, nil
	}

	fee := gainPerUnit * cfg.Rate * in.UnitsOutstanding
	return PerformanceFeeResult{
		FeeAmount:           fee,
		NewHighWaterMark:    in.CurrentNAVPerUnit,
		EligibleGainPerUnit: gainPerUnit,
	}, nil
}
