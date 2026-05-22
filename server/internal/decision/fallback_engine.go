package decision

import "context"

// FallbackEngine is the deterministic, no-LLM decision engine used
// whenever the LLM call fails OR no LLM client is wired (legacy
// deployments, unit tests). Its only job is to mirror the *intent* of
// the pre-Phase-2A hardcoded heuristic so the workflow keeps
// producing plans even when the smart engine is unavailable:
//
//   - With held positions: propose a "reduce" of the first sellable-
//     today position, "hold" the rest.
//   - With no positions: propose a single "buy" of the first universe
//     symbol if any, else "watch".
//
// Confidence stays low (≤0.55) so the auto-execute gate (which
// requires confidence ≥ 0.6 by default) never lets a fallback plan
// auto-execute. Manual approval is still possible.
type FallbackEngine struct{}

func (FallbackEngine) Decide(_ context.Context, input DecisionInput) (*DecisionOutput, error) {
	if len(input.Positions) > 0 {
		actions := make([]DecisionAction, 0, len(input.Positions))
		reducedOnce := false
		for _, p := range input.Positions {
			if !reducedOnce && p.AvailableQty > 0 {
				actions = append(actions, DecisionAction{
					Symbol:     p.Symbol,
					Action:     "reduce",
					QtyPct:     1.0, // sell all sellable today
					Reasoning:  "fallback heuristic: rebalance first sellable holding",
					Confidence: 0.5,
				})
				reducedOnce = true
				continue
			}
			actions = append(actions, DecisionAction{
				Symbol:     p.Symbol,
				Action:     "hold",
				Reasoning:  "fallback heuristic: hold remaining positions",
				Confidence: 0.5,
			})
		}
		return &DecisionOutput{
			Actions:    actions,
			Confidence: 0.5,
			Stance:     "fallback: maintain existing exposures, lighten first holding",
		}, nil
	}

	if len(input.Universe) == 0 {
		return &DecisionOutput{
			Actions: []DecisionAction{{
				Action:     "watch",
				Reasoning:  "fallback heuristic: no universe configured; watch only",
				Confidence: 0.5,
			}},
			Confidence: 0.5,
			Stance:     "fallback: monitor for setup",
		}, nil
	}

	// Pick the budget fraction: prefer BuyBudget / TotalAssets if both
	// are known, otherwise default to 5% NAV.
	pct := 0.05
	if input.BuyBudget > 0 && input.TotalAssets > 0 {
		ratio := input.BuyBudget / input.TotalAssets
		if ratio > 0 && ratio <= 1 {
			pct = ratio
		}
	}
	return &DecisionOutput{
		Actions: []DecisionAction{{
			Symbol:     input.Universe[0],
			Action:     "buy",
			QtyPct:     pct,
			Reasoning:  "fallback heuristic: open small starter position in first universe symbol",
			Confidence: 0.55,
		}},
		Confidence: 0.55,
		Stance:     "fallback: cautiously initiate first universe symbol",
	}, nil
}
