package main

// Feature-flag resolver for the PM-direct-fill child-order splitter
// (step 2 of the Trader Agent integration plan,
// docs/TRADER_AGENT_INTEGRATION.md).
//
// Step 2 introduces parent + N child trade_executions rows for
// strategies that warrant splitting (TWAP today; VWAP / iceberg /
// POV in later iterations). Because the splitting path changes how
// cash_ledger and position_lots get written, we don't want it on by
// default — funds opt in via a per-fund flag inside the JSON config
// blob:
//
//	fund.config = {
//	  ...
//	  "pm_path_child_splitting": true
//	}
//
// Missing key / nil config / non-JSON config / any unmarshal error
// → false. This matches the convention strategy.PolicyFromFundConfig
// uses (errors collapse to disabled) so a partially-corrupted config
// can never accidentally turn child splitting ON for a fund whose
// operator never asked for it.
//
// Resolver is intentionally pure (no DB, no clock). The wiring layer
// fetches Fund.Config from the repo once per execution context and
// passes the raw bytes here.

import (
	"encoding/json"
)

// pmPathFeatureFlagEnvelope mirrors only the pm_path_child_splitting
// field of the fund.config blob. Keeping a dedicated envelope per
// resolver (rather than one giant FundConfig struct) is the same
// pattern strategy.fundConfigEnvelope uses — it lets each subsystem
// add fields without coupling their unmarshallers together.
type pmPathFeatureFlagEnvelope struct {
	PMPathChildSplitting bool `json:"pm_path_child_splitting"`
	// FuturesCashLedgerV2 (T7) flips the futures cash flow
	// from the legacy trade_buy_notional / trade_sell_notional
	// pair to the v2 quartet:
	//   futures_margin_post    at open (debit = -initial_margin)
	//   futures_margin_release at close (credit = +initial_margin)
	//   futures_realized_pnl   at close (signed)
	//   trade_{buy,sell}_commission etc. unchanged
	// Per-fund opt-in because flipping it changes
	// funds.current_capital math on every futures fill — funds
	// already in production with positions open at the moment
	// of the flip need a separate reconciliation pass, not a
	// silent flag flip mid-day.
	FuturesCashLedgerV2 bool `json:"futures_cash_ledger_v2"`
}

// pmPathChildSplittingEnabled returns true iff the fund's persisted
// config blob has `"pm_path_child_splitting": true`. False on:
//
//   - nil / empty raw                  (legacy fund, no key set)
//   - malformed JSON                   (config got truncated /
//                                       corrupted — fail safe to
//                                       legacy single-row path)
//   - key absent                       (most funds today)
//   - key present but value is false   (explicit opt-out)
//
// This is the ONE chokepoint the splitter path checks. Adding new
// per-fund splitter knobs (TWAP slice count, slice interval, ...)
// goes here so the call site stays a single bool check.
func pmPathChildSplittingEnabled(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var env pmPathFeatureFlagEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// Corrupted config — fall back to legacy non-splitting
		// path so we never split silently when the operator has
		// no idea the flag is set.
		return false
	}
	return env.PMPathChildSplitting
}

// futuresCashLedgerV2Enabled reports whether the fund opted in to
// the T7 futures cash flow model. See FuturesCashLedgerV2 above for
// behaviour. Same fail-safe-to-false discipline as the splitter
// resolver — malformed / missing config keeps legacy notional
// math active so the flip is always an explicit per-fund decision.
func futuresCashLedgerV2Enabled(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var env pmPathFeatureFlagEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return false
	}
	return env.FuturesCashLedgerV2
}
