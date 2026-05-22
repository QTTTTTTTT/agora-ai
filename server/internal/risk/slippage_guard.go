// SlippageGuard is a hard execution-gate rule that rejects risk-
// increasing trades whose live execution-time price has drifted
// further than the configured tolerance from the plan's reference
// price (the price the user saw at approval time).
//
// The rule relies on ProposedTrade.ExecutionPrice being populated by
// the trading engine before invoking the policy; when ExecutionPrice
// is zero the rule is silent (no drift signal). Sells and reduces
// are exempt because the system must remain able to de-risk even on
// a volatile tape.
//
// Tolerance lookup precedence (see SlippageConfig.ToleranceFor):
//  1. ToleranceByBoard[Classify(symbol)]
//  2. instrument.DefaultSlippageTolerance(board) (only when board is
//     a known A-share board)
//  3. ToleranceByMarket[lower(trade.Market)]
//  4. DefaultTolerance
//
// A SeverityFail finding from this rule is intentionally distinguished
// from other hard-risk failures by its rule name (Name()) so the
// trading engine can react with a "bounce back to pending_user" flow
// instead of the usual "reject action" flow.
package risk

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/fundai/server/internal/instrument"
)

// SlippageGuardRuleName is the stable rule identifier used in Finding.Rule.
// The trading engine matches on this name to decide whether to bounce a
// plan back to pending_user (slippage) vs reject the action outright
// (other hard-risk failures).
const SlippageGuardRuleName = "hard_slippage_guard"

// SlippageConfig holds the per-board / per-market / fallback tolerances
// used by SlippageGuard. All tolerances are positive fractions of the
// reference price (0.008 = 0.8%). Zero or negative values disable the
// check for that key.
type SlippageConfig struct {
	// DefaultTolerance is applied when no per-board / per-market value
	// matches. Set to 0 to disable the rule entirely for unknown
	// symbols.
	DefaultTolerance float64

	// ToleranceByBoard overrides DefaultTolerance for A-share boards.
	// Keys are the boards returned by instrument.Classify.
	ToleranceByBoard map[instrument.Board]float64

	// ToleranceByMarket overrides DefaultTolerance for non-A-share
	// venues. Keys are lower-cased market identifiers (e.g. "us_stock",
	// "hk_stock", "crypto").
	ToleranceByMarket map[string]float64
}

// ToleranceFor resolves the tolerance for a given symbol using the
// lookup precedence documented at the top of this file.
func (c SlippageConfig) ToleranceFor(symbol, market string) float64 {
	hint := instrument.Hint{Market: market}
	board := instrument.Classify(symbol, hint)
	if board != instrument.BoardUnknown {
		if v, ok := c.ToleranceByBoard[board]; ok && v > 0 {
			return v
		}
		if def := instrument.DefaultSlippageTolerance(board); def > 0 {
			return def
		}
	}
	if m := strings.ToLower(strings.TrimSpace(market)); m != "" {
		if v, ok := c.ToleranceByMarket[m]; ok && v > 0 {
			return v
		}
	}
	if c.DefaultTolerance > 0 {
		return c.DefaultTolerance
	}
	return 0
}

// DefaultSlippageConfig returns conservative production defaults. The
// per-board values match instrument.DefaultSlippageTolerance; the per-
// market values cover the non-A-share venues the platform supports.
func DefaultSlippageConfig() SlippageConfig {
	return SlippageConfig{
		DefaultTolerance: 0.01,
		ToleranceByBoard: map[instrument.Board]float64{
			instrument.BoardSHMain:  0.008,
			instrument.BoardSZMain:  0.008,
			instrument.BoardChiNext: 0.012,
			instrument.BoardSTAR:    0.015,
			instrument.BoardBSE:     0.015,
		},
		ToleranceByMarket: map[string]float64{
			"us_stock":  0.010,
			"us_equity": 0.010,
			"hk_stock":  0.015,
			"hk_equity": 0.015,
			"crypto":    0.025,
		},
	}
}

// SlippageGuard implements the Rule interface.
type SlippageGuard struct {
	Config SlippageConfig
}

// Name returns the stable rule identifier.
func (SlippageGuard) Name() string { return SlippageGuardRuleName }

// Evaluate compares each risk-increasing trade's ExecutionPrice against
// its Price (the plan reference). Drift beyond tolerance produces a
// SeverityFail finding; drift within tolerance produces a SeverityInfo
// finding so downstream consumers can audit realised slippage.
func (g SlippageGuard) Evaluate(_ context.Context, pc PlanContext) ([]Finding, error) {
	var out []Finding
	for _, t := range pc.Trades {
		if t.Side.IsSell() {
			continue
		}
		if t.Price <= 0 || t.ExecutionPrice <= 0 {
			continue
		}
		tolerance := g.Config.ToleranceFor(t.Symbol, t.Market)
		if tolerance <= 0 {
			continue
		}
		drift := (t.ExecutionPrice - t.Price) / t.Price
		absD := math.Abs(drift)
		f := Finding{
			Rule:      g.Name(),
			Symbol:    t.Symbol,
			Current:   drift,
			Threshold: tolerance,
			Message: fmt.Sprintf("%s slippage %+.3f%% (live=%.4f vs plan=%.4f; tolerance=%.3f%%)",
				t.Symbol, drift*100, t.ExecutionPrice, t.Price, tolerance*100),
		}
		if absD > tolerance {
			f.Severity = SeverityFail
			f.Suggestion = fmt.Sprintf(
				"Refresh quote for %s and re-approve plan; drift %+.2f%% exceeds %.2f%% tolerance",
				t.Symbol, drift*100, tolerance*100,
			)
		} else {
			f.Severity = SeverityInfo
		}
		out = append(out, f)
	}
	return out, nil
}
