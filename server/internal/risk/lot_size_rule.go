// Lot-size compliance rule: rejects proposed trades whose quantity violates
// A-share board lot-size rules. The rule is symbol-driven (it does not need
// market hints from the PlanContext) because A-share tickers are always
// 6-digit numerics with deterministic prefixes — see instrument.Classify.
//
// Sells that liquidate the entire position are always permitted; the rule
// pulls the held quantity from PlanContext.Positions for that comparison.
package risk

import (
	"context"
	"fmt"

	"github.com/fundai/server/internal/instrument"
)

// LotSizeRule emits a fail-severity finding for any A-share buy or partial
// sell that violates the board's lot-size constraint. Non-A-share symbols
// and full-position liquidations are skipped silently.
type LotSizeRule struct{}

// Name implements Rule.
func (LotSizeRule) Name() string { return "lot_size" }

// Evaluate implements Rule.
func (LotSizeRule) Evaluate(_ context.Context, pc PlanContext) ([]Finding, error) {
	posQty := make(map[string]float64, len(pc.Positions))
	for _, p := range pc.Positions {
		posQty[p.Symbol] = p.Quantity
	}

	// LotSizeRule operates from the symbol prefix alone; ProposedTrade
	// does not currently carry market metadata. A future revision can
	// thread Hint through PlanContext if non-A-share lot rules are added.
	hint := instrument.Hint{}

	var out []Finding
	for _, t := range pc.Trades {
		board := instrument.Classify(t.Symbol, hint)
		spec := instrument.SpecFor(board)
		if !spec.IsAShare() {
			continue
		}

		// Full-position sell is always legal regardless of lot alignment
		// (handles odd-lot residuals from corporate actions / IPO).
		if t.Side.IsSell() && t.Quantity >= posQty[t.Symbol] && posQty[t.Symbol] > 0 {
			continue
		}

		if instrument.IsAligned(t.Symbol, hint, t.Quantity) {
			out = append(out, Finding{
				Rule:      "lot_size",
				Severity:  SeverityInfo,
				Symbol:    t.Symbol,
				Current:   t.Quantity,
				Threshold: float64(spec.MinLot),
				Message: fmt.Sprintf("%s qty=%.0f aligned to %s lot rules (min=%d, step=%d)",
					t.Symbol, t.Quantity, spec.Board, spec.MinLot, spec.Step),
			})
			continue
		}

		out = append(out, Finding{
			Rule:      "lot_size",
			Severity:  SeverityFail,
			Symbol:    t.Symbol,
			Current:   t.Quantity,
			Threshold: float64(spec.MinLot),
			Message: fmt.Sprintf("%s qty=%.0f violates %s lot rules (min=%d, step=%d)",
				t.Symbol, t.Quantity, spec.Board, spec.MinLot, spec.Step),
			Suggestion: lotSuggestion(spec, t.Quantity),
		})
	}
	return out, nil
}

func lotSuggestion(spec instrument.Spec, qty float64) string {
	q := int(qty)
	if q < spec.MinLot {
		return fmt.Sprintf("Increase qty to ≥%d (board minimum)", spec.MinLot)
	}
	if spec.Step > 1 {
		floored := (q / spec.Step) * spec.Step
		return fmt.Sprintf("Round qty to %d (nearest multiple of %d)", floored, spec.Step)
	}
	return fmt.Sprintf("Round qty to nearest multiple of %d", spec.Step)
}
