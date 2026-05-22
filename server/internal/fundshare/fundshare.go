// Package fundshare implements the per-unit accounting math for fund
// subscriptions and redemptions.
//
// All operations are pure functions on value structs so they can be wired
// into both an HTTP path (apply a single order live) and a batch path
// (process pending orders against a cut-off NAV).
//
// The package intentionally has no notion of a database, a wallet, or a
// transaction. Callers are responsible for translating UnitsAfter / CashAfter
// into ledger entries.
package fundshare

import "errors"

// State captures the fund's pre-trade share & cash state.
type State struct {
	// UnitsOutstanding is the number of units issued and not redeemed.
	UnitsOutstanding float64
	// Cash is the fund's available cash before applying the order.
	Cash float64
}

// Subscription describes a buy order at the cut-off NAV.
type Subscription struct {
	// Amount is the cash the investor wants to invest.
	Amount float64
	// EntryFeeRate is the front-end load expressed as a fraction
	// (0.005 = 0.5%). The fee is deducted from Amount before unit issuance.
	EntryFeeRate float64
}

// Redemption describes a sell order at the cut-off NAV.
type Redemption struct {
	// Units is the number of units the investor wants to redeem.
	Units float64
	// ExitFeeRate is the back-end load expressed as a fraction. The fee is
	// deducted from the gross proceeds before payout.
	ExitFeeRate float64
}

// Result is the post-trade outcome of a single order.
type Result struct {
	// UnitsDelta is positive on subscription, negative on redemption.
	UnitsDelta float64
	// CashDelta is positive on subscription (cash flowing into the fund),
	// negative on redemption.
	CashDelta float64
	// PayoutToInvestor is the amount the redeeming investor receives net
	// of exit fees. Zero for subscriptions.
	PayoutToInvestor float64
	// FeeAmount is the entry/exit fee retained by the fund manager.
	FeeAmount float64
	// UnitsAfter is the post-trade units_outstanding.
	UnitsAfter float64
	// CashAfter is the post-trade cash balance.
	CashAfter float64
}

// Errors that callers must distinguish.
var (
	ErrNonPositiveNAV   = errors.New("fundshare: cut-off NAV must be > 0")
	ErrNonPositiveOrder = errors.New("fundshare: order amount/units must be > 0")
	ErrInsufficientUnits = errors.New("fundshare: redemption exceeds units_outstanding")
	ErrInsufficientCash  = errors.New("fundshare: redemption exceeds available cash")
)

// ApplySubscription issues units at navPerUnit. The investor pays
// sub.Amount; sub.EntryFeeRate * Amount is retained as a fee, and the
// remainder buys units at the cut-off NAV.
func ApplySubscription(state State, navPerUnit float64, sub Subscription) (Result, error) {
	if navPerUnit <= 0 {
		return Result{}, ErrNonPositiveNAV
	}
	if sub.Amount <= 0 {
		return Result{}, ErrNonPositiveOrder
	}
	fee := sub.Amount * clampRate(sub.EntryFeeRate)
	investable := sub.Amount - fee
	units := investable / navPerUnit

	return Result{
		UnitsDelta:  units,
		CashDelta:   investable, // fee does not enter fund cash
		FeeAmount:   fee,
		UnitsAfter:  state.UnitsOutstanding + units,
		CashAfter:   state.Cash + investable,
	}, nil
}

// ApplyRedemption cancels units at navPerUnit. Gross proceeds are
// units * NAV; ExitFeeRate of that is retained, the remainder is paid to
// the investor.
//
// Returns ErrInsufficientUnits if the order exceeds outstanding units, and
// ErrInsufficientCash if the gross proceeds exceed the fund's cash (i.e.
// the fund would have to liquidate positions to honour the redemption — the
// caller can either reject the order or trigger a sell-down).
func ApplyRedemption(state State, navPerUnit float64, red Redemption) (Result, error) {
	if navPerUnit <= 0 {
		return Result{}, ErrNonPositiveNAV
	}
	if red.Units <= 0 {
		return Result{}, ErrNonPositiveOrder
	}
	if red.Units > state.UnitsOutstanding+1e-9 {
		return Result{}, ErrInsufficientUnits
	}
	gross := red.Units * navPerUnit
	if gross > state.Cash+1e-9 {
		return Result{}, ErrInsufficientCash
	}
	fee := gross * clampRate(red.ExitFeeRate)
	payout := gross - fee

	return Result{
		UnitsDelta:       -red.Units,
		CashDelta:        -gross,
		PayoutToInvestor: payout,
		FeeAmount:        fee,
		UnitsAfter:       state.UnitsOutstanding - red.Units,
		CashAfter:        state.Cash - gross,
	}, nil
}

// clampRate clips a fee rate into [0, 1) to defend against bad config.
func clampRate(r float64) float64 {
	if r < 0 {
		return 0
	}
	if r >= 1 {
		return 0.999
	}
	return r
}
