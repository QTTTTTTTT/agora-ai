package instrument

import "strings"

// SettlementCycle is a *market-level* attribute, not a per-symbol
// one: a venue's regulator decides how long a freshly bought share lot
// stays locked before it becomes eligible for resale, and the rule
// applies uniformly to every instrument traded on that venue. Today
// the platform models only the two cycles that actually constrain
// user behaviour:
//
//   - T+0: shares are sellable on the same trading day (default).
//     Covers HK/US/JP equities, crypto, and futures.
//   - T+1: shares bought today cannot be sold until the next trading
//     day. The China A-share market enforces this uniformly across
//     SH/SZ main, ChiNext, STAR, and BSE. T+1 is therefore a
//     property of "the A-share market" — *all* A-share boards, *all*
//     A-share symbols — not of any individual stock.
//
// Callers should think of this package as encoding venue rules.
// SettlementCycleFor / SellableQtyToday take a symbol only because the
// market hint may not be carried through to every call site; the
// classification logic itself routes via Classify -> board -> market.
//
// Cash settlement (broker funds withdrawal delay) is intentionally
// out of scope: this platform simulates trading P&L, not custodian
// operations, and the dominant retail experience is that proceeds
// from sells are immediately re-usable as buying power even when the
// official cash settlement is T+1 or T+2.
type SettlementCycle string

const (
	SettlementUnknown SettlementCycle = ""
	SettlementT0      SettlementCycle = "t+0"
	SettlementT1      SettlementCycle = "t+1"
)

// IsLocked reports whether buys on this cycle lock new lots until the
// next trading day. Equivalent to `c == SettlementT1` today, but the
// helper future-proofs callers against new cycles (e.g. T+2 if we
// later model some custodian-side delays).
func (c SettlementCycle) IsLocked() bool {
	return c == SettlementT1
}

// SettlementCycleFor resolves the settlement rule for a symbol+hint.
// A-share boards always return T+1 regardless of board (the 上交所
// /深交所 rule is uniform). All other identifiable markets, and any
// symbol we can't classify, default to T+0 — that matches both the
// regulatory reality of HK/US/JP equities and crypto, and the
// "fail-open" expectation when the hint is missing (don't block a
// legitimate sell just because we couldn't pattern-match the symbol).
func SettlementCycleFor(symbol string, hint Hint) SettlementCycle {
	board := Classify(symbol, hint)
	switch board {
	case BoardSHMain, BoardSZMain, BoardChiNext, BoardSTAR, BoardBSE:
		return SettlementT1
	}
	// Belt-and-suspenders: even if Classify returned BoardUnknown
	// (e.g. an ADR string that doesn't match A-share prefixes), an
	// explicit market hint of "a_share" still selects T+1. This guards
	// against future changes to Classify that might tighten the prefix
	// rules.
	if m := strings.ToLower(strings.TrimSpace(hint.Market)); m == "a_share" || m == "ashare" || m == "cn_equity" {
		return SettlementT1
	}
	return SettlementT0
}

// SellableQtyToday returns the number of shares of `totalQty` that can
// legally be sold during the current trading day, given the settlement
// cycle and how many shares were purchased today (`boughtToday`, sum of
// today's filled buys for this symbol). For T+0 markets the full
// position is sellable; for T+1 markets the freshly bought lot is
// excluded (`totalQty - boughtToday`, clamped to [0, totalQty]).
//
// Use this helper from the plan generator so PM never proposes a
// reduce/sell action whose qty exceeds what's legally tradable today.
// The trading engine still enforces the same rule at execution time
// (via SettlementCycleRule and the AvailableQty check); this helper is
// the upstream version that prevents the bad plan from ever being
// shown to the user.
//
// `boughtToday` should be the sum of *filled* buy quantities for the
// symbol within the fund's current trading session. Callers that don't
// have intraday trade data can pass 0 — the helper will then return
// `totalQty` on T+1 markets too, which fails open and defers the gate
// to the runtime layer (this matches the legacy behaviour for funds
// that haven't been migrated yet).
func SellableQtyToday(symbol string, hint Hint, totalQty, boughtToday float64) float64 {
	if totalQty <= 0 {
		return 0
	}
	cycle := SettlementCycleFor(symbol, hint)
	if !cycle.IsLocked() {
		return totalQty
	}
	if boughtToday <= 0 {
		// No intraday buy → nothing is locked today.
		return totalQty
	}
	sellable := totalQty - boughtToday
	if sellable <= 0 {
		return 0
	}
	if sellable > totalQty {
		return totalQty
	}
	return sellable
}
