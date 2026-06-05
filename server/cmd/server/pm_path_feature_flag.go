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
	// AllowAgentPortableImports (AP10) mirrors the per-fund
	// opt-OUT lever for cross-fund agent-portable lessons
	// (docs/AGENT_PORTABLE_LEARNING.md).
	//
	// Polarity is INVERTED from the other flags here: this one
	// defaults to TRUE (the feature is on; the flag is the
	// off-switch), whereas every other flag in this envelope
	// defaults to FALSE (opt-in features). The default is
	// modelled by *bool — pre-AP10 funds have no key set, the
	// pointer is nil, the resolver treats that as "opt-in"
	// (allow imports). An explicit `false` is the only way to
	// hard-disable. See agentPortableImportsOptOut().
	//
	// Why a pointer rather than the omitempty + bool trick:
	// JSON's omitempty makes the absent and "false" cases
	// indistinguishable, which would force us to choose ONE
	// default at the type level. *bool keeps the absent case
	// observable so the resolver can default to "import on"
	// while still letting an operator explicitly write false.
	AllowAgentPortableImports *bool `json:"allow_agent_portable_imports,omitempty"`
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

// agentPortableImportsOptOut reports whether the fund has
// EXPLICITLY opted out of receiving cross-fund agent-portable
// lessons (AP10 wiring of docs/AGENT_PORTABLE_LEARNING.md).
//
// Polarity is the inverse of the splitter / v2 resolvers above:
// the agent-portable feature DEFAULTS TO ON, so this resolver
// returns true only when the operator has explicitly written
// `"allow_agent_portable_imports": false` into fund.config.
//
// Truth table:
//
//	raw bytes shape                          → optedOut
//	-------------------------------------     --------
//	nil / empty                               false (= imports allowed)
//	malformed JSON                            true  (= imports BLOCKED)
//	{} / key absent                           false (= imports allowed)
//	{ allow_agent_portable_imports: true  }   false (= imports allowed)
//	{ allow_agent_portable_imports: false }   true  (= imports blocked)
//
// The malformed-JSON case is deliberately FAIL-SAFE here:
// every other resolver in this file falls back to "feature
// off" when the bytes don't parse, but the symmetric
// fail-safe for an opt-out feature is "block the feature"
// because that's the conservative answer for a regulated /
// multi-LP fund whose config might be in flux. A noisy
// false-positive on the import block is recoverable; a silent
// import leak through a corrupted config blob is not.
//
// Caller is the AP10 TeamProvider in agentPortableTeamProvider
// (wiring_adapters.go); the value is passed straight into
// alphalesson.ListLessonsParams.ExplicitlyOptedOut.
func agentPortableImportsOptOut(raw json.RawMessage) bool {
	if len(raw) == 0 {
		// Pre-AP10 funds have no key. The new feature
		// applies — they're opt-IN by default.
		return false
	}
	var env pmPathFeatureFlagEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// Fail-safe in the BLOCK direction. See doc above.
		return true
	}
	if env.AllowAgentPortableImports == nil {
		// Key absent inside an otherwise-valid blob → keep
		// default (allow imports).
		return false
	}
	// Key present + explicit boolean → opt-out iff false.
	return !*env.AllowAgentPortableImports
}
