package matching

import "math"

// roundCurrency keeps four decimal places, matching the convention used by
// the runtime trading engine elsewhere in the codebase.
func roundCurrency(v float64) float64 {
	return math.Round(v*10000) / 10000
}

// FixedRateEquityFees mirrors the legacy hardcoded equity fee schedule:
//   - commission: 10 bps of notional
//   - stamp tax:  10 bps on sells only
//   - transfer:   ¥0.0002 per share
//
// This is the default used by NewDefaultEngine so wiring the engine in does
// not change observed fee numbers in tests or live trading.
type FixedRateEquityFees struct{}

// Fees implements FeeModel.
func (FixedRateEquityFees) Fees(order Order, fillPrice float64) (commission, stamp, transfer float64) {
	notional := order.Quantity * fillPrice
	commission = roundCurrency(notional * 0.001)
	transfer = roundCurrency(order.Quantity * 0.0002)
	if order.Side == SideSell {
		stamp = roundCurrency(notional * 0.001)
	}
	return commission, stamp, transfer
}

// FuturesFees applies a per-contract or notional-based commission with no
// stamp tax. Defaults are conservative (3 bps of notional, no per-contract
// floor) and intended to be configured at wiring time.
type FuturesFees struct {
	NotionalRateBps   float64 // 10000 bps == 100%
	PerContractFee    float64
	IncludeTransferFee bool
}

// Fees implements FeeModel.
func (f FuturesFees) Fees(order Order, fillPrice float64) (commission, stamp, transfer float64) {
	multiplier := order.ContractMultiplier
	if multiplier <= 0 {
		multiplier = 1
	}
	notional := order.Quantity * fillPrice * multiplier
	if f.NotionalRateBps > 0 {
		commission = roundCurrency(notional * f.NotionalRateBps / 10000)
	}
	if f.PerContractFee > 0 {
		commission = roundCurrency(commission + order.Quantity*f.PerContractFee)
	}
	return commission, 0, 0
}

// CryptoFees is a flat-rate maker/taker style schedule. We do not currently
// distinguish maker vs taker (the runtime treats every fill as a taker), but
// the field is exposed for future use.
type CryptoFees struct {
	TakerRateBps float64
}

// Fees implements FeeModel.
func (c CryptoFees) Fees(order Order, fillPrice float64) (commission, stamp, transfer float64) {
	notional := order.Quantity * fillPrice
	rate := c.TakerRateBps
	if rate <= 0 {
		rate = 10 // 10 bps default
	}
	commission = roundCurrency(notional * rate / 10000)
	return commission, 0, 0
}
