// rules.go — three baseline surveillance rules.
//
// Why these three
//
//   * wash_trade        — the simplest classic abuse pattern, and
//                         the one most likely to surface from
//                         honest-but-misconfigured agent loops
//                         (a poorly-tuned "exit on small drawdown"
//                         that thrashes in and out).
//   * marking_close     — single-trade pattern; needs a market
//                         reference, so it's the cleanest test
//                         of how the engine consumes context.
//   * self_trade_pair   — same fund crossed itself; trivially
//                         detectable and the rule that turns into
//                         a hard block once we get cross-trade
//                         prevention wired (future work).
//
// The remaining rule codes (rapid_fire_reversal, layering_suspect)
// are reserved in the schema for follow-on work; they're kept in
// the closed vocabulary so a future migration adding the rule
// doesn't need to widen the CHECK constraint.

package surveillance

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// detectorVersion is stamped onto every Event so a future model
// upgrade can re-run a window and the de-dup index doesn't
// silently swallow the new findings.
const detectorVersion = "v1"

// ----- WashTradeRule -----

// WashTradeOptions tunes the sliding window + tolerance.
type WashTradeOptions struct {
	// Window is the max time between the FIRST and LAST trade in a
	// candidate triplet. Tighter window → fewer false positives
	// but misses real wash sequences that span the lunch break.
	Window time.Duration
	// QuantityRelTol is the max relative quantity drift we accept
	// when calling two trades "the same size". 0.05 = 5%.
	QuantityRelTol float64
	// MinNotional ignores penny trades to keep the queue clean.
	MinNotional float64
}

// DefaultWashTradeOptions chosen so that:
//
//   - Window=10m: a real wash trade would close within minutes;
//     longer windows blow up against benign pair-trading.
//   - QtyRelTol=5%: a wash trade that's exactly the same shares
//     IS a wash trade; this tolerance accepts the rare case where
//     a partial fill makes the leg sizes off by 1-2 shares.
//   - MinNotional=$500: anything below that produces noise that
//     compliance can't actually act on.
var DefaultWashTradeOptions = WashTradeOptions{
	Window:         10 * time.Minute,
	QuantityRelTol: 0.05,
	MinNotional:    500,
}

// WashTradeRule emits one Event per (fund, symbol) trio buy→sell→buy
// (or sell→buy→sell) within Window with quantities aligned within
// QuantityRelTol and net qty change ≈ 0. Net qty alignment is
// what distinguishes a wash from a genuine round trip across two
// market sessions.
type WashTradeRule struct {
	opts WashTradeOptions
}

// NewWashTradeRule constructs a rule. Pass DefaultWashTradeOptions
// for the standard production tuning.
func NewWashTradeRule(opts WashTradeOptions) *WashTradeRule {
	if opts.Window <= 0 {
		opts.Window = DefaultWashTradeOptions.Window
	}
	if opts.QuantityRelTol <= 0 {
		opts.QuantityRelTol = DefaultWashTradeOptions.QuantityRelTol
	}
	if opts.MinNotional < 0 {
		opts.MinNotional = 0
	}
	return &WashTradeRule{opts: opts}
}

func (r *WashTradeRule) Code() RuleCode { return RuleWashTrade }

// Detect groups by (fund_id, symbol), sorts by ExecutedAt, and
// scans for triplets that satisfy the wash signature. We do NOT
// search across fund boundaries — each fund is its own actor.
func (r *WashTradeRule) Detect(snap []TradeSnapshot, _ *MarketContext) []Event {
	groups := groupByFundSymbol(snap)
	var out []Event
	seen := map[string]struct{}{} // dedupe within one Detect call
	for key, trades := range groups {
		fundID, symbol := splitFundSymbolKey(key)
		sort.Slice(trades, func(i, j int) bool {
			return trades[i].ExecutedAt.Before(trades[j].ExecutedAt)
		})
		// Walk: triplet [i, j, k] where i < j < k, all within Window,
		// sides alternate, qtys aligned.
		for i := 0; i < len(trades)-2; i++ {
			for j := i + 1; j < len(trades)-1; j++ {
				if trades[j].ExecutedAt.Sub(trades[i].ExecutedAt) > r.opts.Window {
					break
				}
				for k := j + 1; k < len(trades); k++ {
					if trades[k].ExecutedAt.Sub(trades[i].ExecutedAt) > r.opts.Window {
						break
					}
					if !sidesAlternate(trades[i], trades[j], trades[k]) {
						continue
					}
					if !qtysAligned(trades[i], trades[j], trades[k], r.opts.QuantityRelTol) {
						continue
					}
					if r.opts.MinNotional > 0 {
						q := pickQty(trades[i])
						if q*trades[i].Price < r.opts.MinNotional {
							continue
						}
					}
					ids := []string{trades[i].ID, trades[j].ID, trades[k].ID}
					fp := fingerprintFor(fundID, RuleWashTrade, ids)
					if _, ok := seen[fp]; ok {
						continue
					}
					seen[fp] = struct{}{}
					out = append(out, Event{
						FundID:          fundID,
						RuleCode:        RuleWashTrade,
						Severity:        SeverityWarning,
						Symbol:          symbol,
						InstrumentKey:   trades[i].InstrumentKey,
						WindowStart:     trades[i].ExecutedAt,
						WindowEnd:       trades[k].ExecutedAt,
						TradeIDs:        ids,
						Summary:         fmt.Sprintf("Wash-trade pattern: %s %s %s on %s within %s", trades[i].Side, trades[j].Side, trades[k].Side, symbol, trades[k].ExecutedAt.Sub(trades[i].ExecutedAt).Round(time.Second)),
						Metadata: map[string]any{
							"trade_qty":   []float64{trades[i].Quantity, trades[j].Quantity, trades[k].Quantity},
							"trade_price": []float64{trades[i].Price, trades[j].Price, trades[k].Price},
							"trade_side":  []string{trades[i].Side, trades[j].Side, trades[k].Side},
						},
						Fingerprint:     fp,
						DetectorVersion: detectorVersion,
					})
				}
			}
		}
	}
	return out
}

func sidesAlternate(a, b, c TradeSnapshot) bool {
	la, lb, lc := strings.ToLower(a.Side), strings.ToLower(b.Side), strings.ToLower(c.Side)
	return la != lb && lb != lc && la == lc
}

func qtysAligned(a, b, c TradeSnapshot, relTol float64) bool {
	qa, qb, qc := pickQty(a), pickQty(b), pickQty(c)
	if qa <= 0 || qb <= 0 || qc <= 0 {
		return false
	}
	// Each pair must be within relTol of each other.
	if math.Abs(qa-qb)/qb > relTol {
		return false
	}
	if math.Abs(qb-qc)/qb > relTol {
		return false
	}
	// Net qty across the triplet should round to zero given the
	// alternation: +q -q +q (or its inverse) leaves net = +q.
	// We don't require exact zero net; we do require the abs net
	// to be within relTol*qb. That's the wash signature: round
	// trip with no real position change.
	signed := signedQty(a) + signedQty(b) + signedQty(c)
	return math.Abs(signed-signedQty(a)) <= relTol*qb
}

func signedQty(t TradeSnapshot) float64 {
	q := pickQty(t)
	if strings.EqualFold(t.Side, "sell") {
		return -q
	}
	return q
}

// ----- MarkingCloseRule -----

// MarkingCloseOptions tunes the rule.
type MarkingCloseOptions struct {
	// CloseWindow is the duration measured from SessionClose
	// backward; trades inside this window are candidates.
	CloseWindow time.Duration
	// SizeRatioThreshold flags trades whose notional exceeds
	// (SizeRatioThreshold * AvgDailyNotional[symbol]). 0.05
	// means "5% of an average day's volume in one trade near
	// close" is suspicious.
	SizeRatioThreshold float64
	// VWAPDeviationThreshold flags trades whose price deviation
	// from RecentVWAP[symbol] exceeds this fraction. 0.005 = 50
	// bps deviation is the bar.
	VWAPDeviationThreshold float64
	// RequireBoth: when true, BOTH size AND vwap signals must
	// fire for an event. When false, either signal suffices.
	RequireBoth bool
}

// DefaultMarkingCloseOptions is conservative: we want either a
// big trade OR an aggressively-priced trade in the last 15
// minutes to surface. Compliance can later raise the bar by
// passing RequireBoth=true.
var DefaultMarkingCloseOptions = MarkingCloseOptions{
	CloseWindow:            15 * time.Minute,
	SizeRatioThreshold:     0.05,
	VWAPDeviationThreshold: 0.005,
	RequireBoth:            false,
}

// MarkingCloseRule emits one Event per qualifying near-close trade.
type MarkingCloseRule struct {
	opts MarkingCloseOptions
}

func NewMarkingCloseRule(opts MarkingCloseOptions) *MarkingCloseRule {
	if opts.CloseWindow <= 0 {
		opts.CloseWindow = DefaultMarkingCloseOptions.CloseWindow
	}
	if opts.SizeRatioThreshold <= 0 {
		opts.SizeRatioThreshold = DefaultMarkingCloseOptions.SizeRatioThreshold
	}
	if opts.VWAPDeviationThreshold <= 0 {
		opts.VWAPDeviationThreshold = DefaultMarkingCloseOptions.VWAPDeviationThreshold
	}
	return &MarkingCloseRule{opts: opts}
}

func (r *MarkingCloseRule) Code() RuleCode { return RuleMarkingClose }

// Detect — needs MarketContext.SessionClose to compute the window.
// Without a session close we can't decide "near close" so we
// short-circuit and emit nothing.
func (r *MarkingCloseRule) Detect(snap []TradeSnapshot, ctx *MarketContext) []Event {
	if ctx == nil || ctx.SessionClose.IsZero() {
		return nil
	}
	cutoff := ctx.SessionClose.Add(-r.opts.CloseWindow)
	var out []Event
	for _, t := range snap {
		if t.ExecutedAt.Before(cutoff) || t.ExecutedAt.After(ctx.SessionClose) {
			continue
		}
		sym := canonicalSymbol(t.Symbol)
		var (
			sizeFlag bool
			vwapFlag bool
			sizeRatio float64
			vwapDev   float64
		)
		if avg, ok := ctx.AvgDailyNotional[sym]; ok && avg > 0 {
			notional := t.Notional
			if notional == 0 {
				notional = pickQty(t) * t.Price
			}
			sizeRatio = notional / avg
			if sizeRatio >= r.opts.SizeRatioThreshold {
				sizeFlag = true
			}
		}
		if vwap, ok := ctx.RecentVWAP[sym]; ok && vwap > 0 {
			vwapDev = (t.Price - vwap) / vwap
			// Direction matters: a sell ABOVE vwap or a buy BELOW
			// vwap close to session close are the patterns that
			// could mark NAV in a fund's favour.
			absDev := math.Abs(vwapDev)
			if absDev >= r.opts.VWAPDeviationThreshold {
				vwapFlag = true
			}
		}
		fire := false
		if r.opts.RequireBoth {
			fire = sizeFlag && vwapFlag
		} else {
			fire = sizeFlag || vwapFlag
		}
		if !fire {
			continue
		}
		severity := SeverityWarning
		if sizeFlag && vwapFlag {
			severity = SeverityCritical
		}
		ids := []string{t.ID}
		fp := fingerprintFor(t.FundID, RuleMarkingClose, ids)
		out = append(out, Event{
			FundID:          t.FundID,
			RuleCode:        RuleMarkingClose,
			Severity:        severity,
			Symbol:          sym,
			InstrumentKey:   t.InstrumentKey,
			WindowStart:     t.ExecutedAt,
			WindowEnd:       t.ExecutedAt,
			TradeIDs:        ids,
			Summary: fmt.Sprintf(
				"Near-close %s on %s @ %.4f (size %.1fx avg, vwap dev %.3f%%)",
				t.Side, sym, t.Price, sizeRatio, vwapDev*100,
			),
			Metadata: map[string]any{
				"size_ratio":            sizeRatio,
				"vwap_deviation":        vwapDev,
				"size_flag":             sizeFlag,
				"vwap_flag":             vwapFlag,
				"close_distance_seconds": int(ctx.SessionClose.Sub(t.ExecutedAt).Seconds()),
			},
			Fingerprint:     fp,
			DetectorVersion: detectorVersion,
		})
	}
	return out
}

// ----- SelfTradePairRule -----

// SelfTradePairOptions tunes the matching window.
type SelfTradePairOptions struct {
	// Window is the max time difference between the buy and sell
	// legs to treat as a single self-cross. 5s default — anything
	// longer is more likely two independent decisions.
	Window time.Duration
	// PriceTol is the max abs relative price drift between the
	// two legs. 0 = exact match required.
	PriceTol float64
	// QtyTol is the max abs relative qty drift; defaults to 0
	// because a true self-cross matches qty exactly.
	QtyTol float64
}

var DefaultSelfTradePairOptions = SelfTradePairOptions{
	Window:   5 * time.Second,
	PriceTol: 0,
	QtyTol:   0,
}

// SelfTradePairRule flags any (buy, sell) leg pair within the same
// fund / symbol that fires within Window with matching qty and
// price. Critical severity by default — a self-cross is more
// damning than a wash because the round-trip is instant.
type SelfTradePairRule struct {
	opts SelfTradePairOptions
}

func NewSelfTradePairRule(opts SelfTradePairOptions) *SelfTradePairRule {
	if opts.Window <= 0 {
		opts.Window = DefaultSelfTradePairOptions.Window
	}
	if opts.PriceTol < 0 {
		opts.PriceTol = 0
	}
	if opts.QtyTol < 0 {
		opts.QtyTol = 0
	}
	return &SelfTradePairRule{opts: opts}
}

func (r *SelfTradePairRule) Code() RuleCode { return RuleSelfTradePair }

func (r *SelfTradePairRule) Detect(snap []TradeSnapshot, _ *MarketContext) []Event {
	groups := groupByFundSymbol(snap)
	var out []Event
	seen := map[string]struct{}{}
	for key, trades := range groups {
		fundID, symbol := splitFundSymbolKey(key)
		sort.Slice(trades, func(i, j int) bool {
			return trades[i].ExecutedAt.Before(trades[j].ExecutedAt)
		})
		for i := 0; i < len(trades); i++ {
			for j := i + 1; j < len(trades); j++ {
				if trades[j].ExecutedAt.Sub(trades[i].ExecutedAt) > r.opts.Window {
					break
				}
				if !oppositeSide(trades[i], trades[j]) {
					continue
				}
				if !approxEqual(pickQty(trades[i]), pickQty(trades[j]), r.opts.QtyTol) {
					continue
				}
				if !approxEqual(trades[i].Price, trades[j].Price, r.opts.PriceTol) {
					continue
				}
				ids := []string{trades[i].ID, trades[j].ID}
				fp := fingerprintFor(fundID, RuleSelfTradePair, ids)
				if _, ok := seen[fp]; ok {
					continue
				}
				seen[fp] = struct{}{}
				out = append(out, Event{
					FundID:        fundID,
					RuleCode:      RuleSelfTradePair,
					Severity:      SeverityCritical,
					Symbol:        symbol,
					InstrumentKey: trades[i].InstrumentKey,
					WindowStart:   trades[i].ExecutedAt,
					WindowEnd:     trades[j].ExecutedAt,
					TradeIDs:      ids,
					Summary: fmt.Sprintf(
						"Self-cross on %s: %s @ %.4f and %s @ %.4f within %s",
						symbol, trades[i].Side, trades[i].Price,
						trades[j].Side, trades[j].Price,
						trades[j].ExecutedAt.Sub(trades[i].ExecutedAt).Round(time.Millisecond),
					),
					Metadata: map[string]any{
						"qty":   []float64{trades[i].Quantity, trades[j].Quantity},
						"price": []float64{trades[i].Price, trades[j].Price},
						"side":  []string{trades[i].Side, trades[j].Side},
					},
					Fingerprint:     fp,
					DetectorVersion: detectorVersion,
				})
			}
		}
	}
	return out
}

func oppositeSide(a, b TradeSnapshot) bool {
	la, lb := strings.ToLower(a.Side), strings.ToLower(b.Side)
	return (la == "buy" && lb == "sell") || (la == "sell" && lb == "buy")
}

func approxEqual(a, b, relTol float64) bool {
	if a == 0 && b == 0 {
		return true
	}
	if relTol == 0 {
		return a == b
	}
	denom := math.Max(math.Abs(a), math.Abs(b))
	if denom == 0 {
		return true
	}
	return math.Abs(a-b)/denom <= relTol
}

// ----- shared grouping helpers -----

func groupByFundSymbol(snap []TradeSnapshot) map[string][]TradeSnapshot {
	out := map[string][]TradeSnapshot{}
	for _, t := range snap {
		if !isFilledLike(t.Status) {
			continue
		}
		sym := canonicalSymbol(t.Symbol)
		if sym == "" {
			continue
		}
		key := t.FundID + "|" + sym
		out[key] = append(out[key], t)
	}
	return out
}

func splitFundSymbolKey(key string) (string, string) {
	idx := strings.Index(key, "|")
	if idx < 0 {
		return key, ""
	}
	return key[:idx], key[idx+1:]
}
