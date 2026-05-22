package exitmanager

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/fundai/server/internal/repository"
)

// ---------------------------------------------------------------------------
// Input / output types
// ---------------------------------------------------------------------------

// PositionView is the per-(fund, instrument) snapshot the
// service evaluates. The caller is responsible for:
//
//   - resolving the current price (typically from the
//     position-quote refresher's last update on
//     holding_positions.current_price)
//   - listing the open lots for the instrument (FIFO order)
//
// The service keeps the cost of this assembly outside the
// package so unit tests can inject lots directly without going
// through the repo.
type PositionView struct {
	InstrumentKey string
	Symbol        string
	Market        string
	AssetClass    string
	CurrentPrice  float64
	// QuoteAsOf is the timestamp of the current_price quote.
	// When zero, the staleness guard treats the quote as
	// indefinitely fresh — useful for tests; production wires
	// the refresher's last update time.
	QuoteAsOf time.Time
	OpenLots  []*repository.PositionLotRow
}

// ExitDecision is the service's output: close all open lots of
// one (fund, instrument) for the given reason.
//
// PR-3A-2 uses position-level closes (the whole instrument
// drains in FIFO order when ANY lot's rule fires). A future
// phase can refine this to per-lot targeting if attribution
// surfaces enough cases where multi-lot positions need
// different treatment.
type ExitDecision struct {
	InstrumentKey string
	Symbol        string
	Market        string
	AssetClass    string
	// Quantity is the sum of quantity_remaining across all open
	// lots of the instrument at evaluation time. The lotledger
	// service consumes them FIFO on the resulting fill.
	Quantity float64
	// TriggerPrice is the current_price at evaluation time.
	// Used as the order's reference price by the wiring layer.
	TriggerPrice float64
	// Reason / SignalSource are the attribution labels written to
	// plan_actions.exit_reason and plan_actions.signal_source.
	// Reason ∈ { "stop_loss", "take_profit", "trailing", "time_stop" }.
	// SignalSource mirrors Reason for now; later phases can
	// refine it to "atr_stop", "chandelier_22", etc.
	Reason       string
	SignalSource string
	// Reasoning is the human-readable explanation persisted to
	// plan_actions.reasoning. Built by the rule that fired.
	Reasoning string
	// LotID is the ID of the lot that *triggered* the rule. The
	// FIFO close may consume earlier lots first, so this is
	// informational only — handy for tests + audit / log lines.
	LotID string
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// Service evaluates exit policies against open lots. Construct
// one per process via NewService; call Evaluate inside the
// PMAgent's plan-generation flow.
type Service struct {
	now func() time.Time
}

// Option tunes the service for tests (clock injection) and
// future per-fund overrides. Kept as a functional option so the
// public constructor signature doesn't churn.
type Option func(*Service)

// WithClock injects a deterministic clock for tests. Default:
// time.Now().UTC().
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// NewService wires a Service. Defaults: time.Now().UTC() clock.
func NewService(opts ...Option) *Service {
	s := &Service{
		now: func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// ErrInvalidView is returned by Evaluate when a caller hands us
// a PositionView with no instrument key or symbol. The rest of
// the field validation (negative price, empty lots) is handled
// inline: those views just contribute no decision.
var ErrInvalidView = errors.New("exitmanager: invalid position view")

// Evaluate scans the positions and returns one ExitDecision per
// instrument that has at least one rule firing. Positions
// without any triggering rule produce no decision.
//
// Rule priority (when multiple fire on the same position): the
// one that signals the WORST imminent risk wins, in order:
//
//   stop_loss > trailing > take_profit > time_stop
//
// "stop_loss > trailing": both are downside protections; the
// fixed stop fires from entry and the trailing fires from peak.
// When both fire we prefer to attribute as stop_loss because
// that's the harder signal (price below entry, not just below
// peak).
//
// "take_profit > time_stop": when we're already in profit AND
// timed out, the proximate cause is the profit hit, not the
// staleness. The closed_lots row gets the more useful tag.
func (s *Service) Evaluate(policy Policy, positions []PositionView) []ExitDecision {
	if s == nil {
		return nil
	}
	eff := policy.EffectivePolicy()
	if !eff.HasAnyRule() {
		return nil
	}
	now := s.now()
	out := make([]ExitDecision, 0, len(positions))
	for _, view := range positions {
		if strings.TrimSpace(view.InstrumentKey) == "" || strings.TrimSpace(view.Symbol) == "" {
			continue
		}
		if len(view.OpenLots) == 0 {
			continue
		}
		if view.CurrentPrice <= 0 {
			// No usable quote → never trigger. Defensive: a stop
			// at "price = 0" would always fire on the wrong
			// side of the rule and chain-close every position.
			continue
		}
		dec := s.evaluateOne(eff, view, now)
		if dec == nil {
			continue
		}
		out = append(out, *dec)
	}
	// Stable ordering so attribution / debugging is reproducible.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].InstrumentKey < out[j].InstrumentKey
	})
	return out
}

// evaluateOne runs all configured rules against a single
// position view and picks the highest-priority firing rule.
// Returns nil when nothing fires.
func (s *Service) evaluateOne(p Policy, v PositionView, now time.Time) *ExitDecision {
	totalQty := totalOpenQuantity(v.OpenLots)
	if totalQty <= 0 {
		return nil
	}
	base := ExitDecision{
		InstrumentKey: v.InstrumentKey,
		Symbol:        v.Symbol,
		Market:        v.Market,
		AssetClass:    v.AssetClass,
		Quantity:      totalQty,
		TriggerPrice:  v.CurrentPrice,
	}

	// Priority: stop_loss → trailing → take_profit → time_stop.
	if p.StopLoss != nil {
		if hit, lot, reason := matchStopLoss(p.StopLoss.Percent, v); hit {
			fill(&base, lot, "stop_loss", reason)
			return &base
		}
	}
	if p.Trailing != nil {
		if hit, lot, reason := matchTrailing(p.Trailing.Percent, v); hit {
			fill(&base, lot, "trailing", reason)
			return &base
		}
	}
	if p.TakeProfit != nil {
		if hit, lot, reason := matchTakeProfit(p.TakeProfit.Percent, v); hit {
			fill(&base, lot, "take_profit", reason)
			return &base
		}
	}
	if p.TimeStop != nil {
		if hit, lot, reason := matchTimeStop(p.TimeStop.MaxHoldingDays, v, now); hit {
			fill(&base, lot, "time_stop", reason)
			return &base
		}
	}
	return nil
}

func fill(d *ExitDecision, lot *repository.PositionLotRow, reason, reasoning string) {
	d.Reason = reason
	d.SignalSource = reason
	d.Reasoning = reasoning
	if lot != nil {
		d.LotID = lot.ID
	}
}

// ---------------------------------------------------------------------------
// Rule implementations
// ---------------------------------------------------------------------------

// matchStopLoss fires when the current price is below
//
//	min(lot.EntryPrice) * (1 - percent)
//
// across all open lots. Picking the lot with the lowest threshold
// means we honour the LOWEST stop in the lot ledger — i.e. as
// soon as ANY lot is underwater past the threshold we close.
//
// We also report the *triggering* lot (the one with the
// highest entry, which is the most underwater) so the audit log
// can pinpoint the worst offender.
func matchStopLoss(percent float64, v PositionView) (bool, *repository.PositionLotRow, string) {
	if percent <= 0 {
		return false, nil, ""
	}
	var trigger *repository.PositionLotRow
	worstDrawdown := 0.0
	for _, lot := range v.OpenLots {
		if lot == nil || lot.EntryPrice <= 0 {
			continue
		}
		threshold := lot.EntryPrice * (1 - percent)
		if v.CurrentPrice < threshold {
			// The lot whose drawdown vs entry is the biggest is
			// the most underwater — surface that as the trigger.
			dd := (lot.EntryPrice - v.CurrentPrice) / lot.EntryPrice
			if trigger == nil || dd > worstDrawdown {
				trigger = lot
				worstDrawdown = dd
			}
		}
	}
	if trigger == nil {
		return false, nil, ""
	}
	return true, trigger, fmt.Sprintf("stop_loss: price %.4f fell below %.2f%% of entry %.4f (drawdown %.2f%%)",
		v.CurrentPrice, percent*100, trigger.EntryPrice, worstDrawdown*100)
}

// matchTakeProfit fires when the current price is above
//
//	lot.EntryPrice * (1 + percent)
//
// for ANY open lot. Picking the lot with the LOWEST entry
// (i.e. the most profitable one) gives the highest realised
// gain, which is the right number to surface in the audit log.
func matchTakeProfit(percent float64, v PositionView) (bool, *repository.PositionLotRow, string) {
	if percent <= 0 {
		return false, nil, ""
	}
	var trigger *repository.PositionLotRow
	bestGain := 0.0
	for _, lot := range v.OpenLots {
		if lot == nil || lot.EntryPrice <= 0 {
			continue
		}
		threshold := lot.EntryPrice * (1 + percent)
		if v.CurrentPrice > threshold {
			g := (v.CurrentPrice - lot.EntryPrice) / lot.EntryPrice
			if trigger == nil || g > bestGain {
				trigger = lot
				bestGain = g
			}
		}
	}
	if trigger == nil {
		return false, nil, ""
	}
	return true, trigger, fmt.Sprintf("take_profit: price %.4f exceeded %.2f%% above entry %.4f (gain %.2f%%)",
		v.CurrentPrice, percent*100, trigger.EntryPrice, bestGain*100)
}

// matchTrailing fires when the current price has retraced more
// than `percent` from the highest_price_seen for ANY open lot,
// PROVIDED that highest_price_seen actually rose above entry.
// The latter guard prevents trailing from degenerating into a
// noisy second stop_loss for lots that never broke even.
func matchTrailing(percent float64, v PositionView) (bool, *repository.PositionLotRow, string) {
	if percent <= 0 {
		return false, nil, ""
	}
	var trigger *repository.PositionLotRow
	worstRetracement := 0.0
	for _, lot := range v.OpenLots {
		if lot == nil || lot.EntryPrice <= 0 {
			continue
		}
		// highest_price_seen must be valid AND above entry to
		// give trailing any meaning. A lot whose peak was below
		// entry is still in the stop_loss domain.
		if !lot.HighestPriceSeen.Valid || lot.HighestPriceSeen.Float64 <= lot.EntryPrice {
			continue
		}
		threshold := lot.HighestPriceSeen.Float64 * (1 - percent)
		if v.CurrentPrice < threshold {
			retr := (lot.HighestPriceSeen.Float64 - v.CurrentPrice) / lot.HighestPriceSeen.Float64
			if trigger == nil || retr > worstRetracement {
				trigger = lot
				worstRetracement = retr
			}
		}
	}
	if trigger == nil {
		return false, nil, ""
	}
	return true, trigger, fmt.Sprintf("trailing: price %.4f retraced %.2f%% from peak %.4f (entry %.4f)",
		v.CurrentPrice, worstRetracement*100, trigger.HighestPriceSeen.Float64, trigger.EntryPrice)
}

// matchTimeStop fires when ANY open lot has been held for more
// than maxDays calendar days. The OLDEST lot is reported as the
// trigger so the audit log shows the most-stale lot.
func matchTimeStop(maxDays int, v PositionView, now time.Time) (bool, *repository.PositionLotRow, string) {
	if maxDays <= 0 {
		return false, nil, ""
	}
	var trigger *repository.PositionLotRow
	worstAge := 0.0
	for _, lot := range v.OpenLots {
		if lot == nil || lot.OpenedAt.IsZero() {
			continue
		}
		age := now.Sub(lot.OpenedAt).Hours() / 24.0
		if age > float64(maxDays) {
			if trigger == nil || age > worstAge {
				trigger = lot
				worstAge = age
			}
		}
	}
	if trigger == nil {
		return false, nil, ""
	}
	return true, trigger, fmt.Sprintf("time_stop: oldest lot held %.1f days (limit %d days)",
		worstAge, maxDays)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func totalOpenQuantity(lots []*repository.PositionLotRow) float64 {
	total := 0.0
	for _, lot := range lots {
		if lot == nil {
			continue
		}
		if lot.QuantityRemaining > 0 {
			total += lot.QuantityRemaining
		}
	}
	// Round to 4 decimals — matches NUMERIC(16,4) precision of
	// the underlying column so the final fill quantity is
	// representable losslessly.
	return math.Round(total*10000) / 10000
}
