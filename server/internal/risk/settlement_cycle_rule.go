// SettlementCycleRule enforces market-level settlement rules at the
// execution gate. Today only the A-share T+1 rule materially affects
// behaviour (China A-share regulation locks shares purchased during a
// trading session from being sold until the next trading day, applied
// uniformly across SH/SZ main, ChiNext, STAR, BSE — it's a market
// rule, not a per-symbol property). T+0 markets (US/HK/JP equities,
// crypto, futures) are silent.
//
// The downstream trading engine also checks AvailableQty < quantity
// and returns api.ErrConflict, but that path produces a generic error
// the user can't easily diagnose. This rule fires earlier in the
// hard-risk gate with a market-level message that explains *why* the
// sell is rejected so the UI can show something actionable.
//
// The rule is silent — not "info" — for fully available sells and
// for T+0 markets, to keep risk reports uncluttered. Buys are exempt
// entirely (settlement only constrains sales).
package risk

import (
	"context"
	"fmt"

	"github.com/fundai/server/internal/instrument"
)

// SettlementCycleRuleName is the stable Finding.Rule string used by
// the trading engine to identify T+1 violations (currently consumed
// by metrics; future code might surface a typed error here too).
const SettlementCycleRuleName = "hard_settlement_cycle"

// SettlementCycleRule implements the Rule interface.
type SettlementCycleRule struct{}

// Name implements Rule.
func (SettlementCycleRule) Name() string { return SettlementCycleRuleName }

// Evaluate inspects every sell trade and rejects those whose live
// AvailableQty is below the requested sell quantity on a T+1 market.
// "Available" here means "not still settling from a same-day buy" —
// the trading-engine's mergeBoughtPosition leaves AvailableQty
// untouched on T+1 buys, and the daily Settle step releases the lock.
func (SettlementCycleRule) Evaluate(_ context.Context, pc PlanContext) ([]Finding, error) {
	posByKey := make(map[string]Position, len(pc.Positions))
	for _, p := range pc.Positions {
		posByKey[p.Symbol] = p
	}
	var out []Finding
	for _, t := range pc.Trades {
		if !t.Side.IsSell() {
			continue
		}
		cycle := instrument.SettlementCycleFor(t.Symbol, instrument.Hint{
			Market:     t.Market,
			Exchange:   t.Exchange,
			AssetClass: t.AssetClass,
		})
		if !cycle.IsLocked() {
			continue
		}
		p, ok := posByKey[t.Symbol]
		if !ok {
			// Selling a symbol we don't hold is a different bug
			// (caught by the existing availableQty check); leave it
			// to the engine so this rule stays focused.
			continue
		}
		// Treat AvailableQty == 0 on a real position as "data
		// unavailable" rather than "everything locked": positions
		// persisted before T+1 tracking existed don't have the
		// field populated. The trading engine's downstream check
		// is the authoritative gate; this rule is purely a UX
		// upgrade.
		if p.AvailableQty <= 0 {
			continue
		}
		if p.AvailableQty+1e-9 >= t.Quantity {
			continue
		}
		locked := p.Quantity - p.AvailableQty
		if locked < 0 {
			locked = 0
		}
		out = append(out, Finding{
			Rule:      SettlementCycleRuleName,
			Severity:  SeverityFail,
			Symbol:    t.Symbol,
			Current:   p.AvailableQty,
			Threshold: t.Quantity,
			// Frame the message as a market rule ("A-share market is
			// T+1") rather than a per-symbol attribute; the symbol is
			// only mentioned as the affected holding.
			Message: fmt.Sprintf(
				"A-share market T+1 settlement: %s holds %.0f shares but only %.0f are sellable today (requested %.0f); the remaining %.0f were purchased today and settle on the next trading day",
				t.Symbol, p.Quantity, p.AvailableQty, t.Quantity, locked,
			),
			Suggestion: fmt.Sprintf(
				"Reduce the sell quantity to ≤ %.0f, or wait for next trading day when the %.0f T+1-locked shares settle",
				p.AvailableQty, locked,
			),
		})
	}
	return out, nil
}
