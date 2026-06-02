// Package marketstatus implements the S6.1 "is the market willing
// to take this trade right now?" gate.
//
// What this package owns
//
//   - Domain types: InstrumentStatus, CalendarDay, OrderProbe,
//     Decision, Event.
//   - Pure rules: HaltedRule, PriceLimitRule, StaleQuoteRule,
//     CalendarRule.
//   - Engine that orchestrates rules and emits a single Decision.
//   - Repo for the live state tables.
//
// What this package does NOT own
//
//   - Quote ingest. The broker / market-data layer keeps writing
//     `last_quote_at` to instrument_market_status; the gate only
//     reads.
//   - Order placement. The simulator (or a future live broker
//     adapter) calls `Engine.Check` synchronously before the
//     match attempt and acts on the verdict.
//
// Why a single Decision instead of independent boolean checks
//
// Each rule can produce a reject OR a warn (e.g. a halted symbol
// is reject, but a stale quote MIGHT only warn depending on
// asset class). The engine combines them: any reject → reject;
// otherwise warns from each rule are merged. This keeps the
// caller's job to "do I run the order?" rather than "which of
// six checks failed?".

package marketstatus

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// RuleCode is the closed vocabulary the gate emits. Keep in sync
// with marketstatus_events.rule_code (no DB CHECK there because
// the vocabulary may grow; the engine validates).
type RuleCode string

const (
	RuleHalted        RuleCode = "halted"
	RuleSuspended     RuleCode = "suspended"
	RulePriceLimit    RuleCode = "price_limit"
	RuleStaleQuote    RuleCode = "stale_quote"
	RuleMarketClosed  RuleCode = "market_closed"
	RuleHalfDayClosed RuleCode = "half_day_closed"
)

// Decision is one of: allow, warn (allow with concerns),
// reject. Encoded as a string so JSON shape and audit log
// agree.
type Decision string

const (
	DecisionAllow  Decision = "allow"
	DecisionWarn   Decision = "warn"
	DecisionReject Decision = "reject"
)

// detectorVersion gets stamped on every event so a rules
// upgrade can be replayed without dedup ambiguity.
const detectorVersion = "v1"

// Default freshness budgets — chosen empirically. Operators can
// override per instrument via instrument_market_status.staleness_budget_seconds.
var defaultStalenessBudget = map[string]time.Duration{
	"equity":  60 * time.Second,
	"etf":     60 * time.Second,
	"futures": 5 * time.Second,
	"crypto":  10 * time.Second,
	"option":  10 * time.Second,
	"otc":     300 * time.Second,
	"bond":    300 * time.Second,
}

// fallbackStalenessBudget is the safety net when an asset class
// is missing from the map (a new exotic class lands without an
// engine update). 60s mirrors equity — a deliberately moderate
// default that flags but doesn't choke unfamiliar instruments.
const fallbackStalenessBudget = 60 * time.Second

// InstrumentStatus is the per-instrument live row.
type InstrumentStatus struct {
	InstrumentKey         string
	Symbol                string
	Market                string
	Status                string // 'trading' | 'halted' | 'suspended'
	HaltReason            string
	HaltStartedAt         *time.Time
	HaltUntil             *time.Time
	LowerLimit            *float64
	UpperLimit            *float64
	LastQuoteAt           *time.Time
	LastQuotePrice        *float64
	AssetClass            string
	StalenessBudget       *time.Duration
	Note                  string
	UpdatedAt             time.Time
}

// CalendarDay is one row of trading_calendar.
type CalendarDay struct {
	Market       string
	TradingDate  time.Time
	IsOpen       bool
	OpenLocal    string
	CloseLocal   string
	MarketTZ     string
	HalfDay      bool
	Note         string
}

// OrderProbe is the engine input for a prospective order. We
// keep the field set lean so wire-up doesn't require dragging in
// the broker package.
type OrderProbe struct {
	FundID         string
	InstrumentKey  string
	Symbol         string
	Market         string
	AssetClass     string
	Side           string  // "buy" | "sell"
	Quantity       float64
	IntendedPrice  float64 // limit price if known; 0 → use last quote
	ClientOrderID  string
}

// Event is one rule firing. The engine returns a list of these
// alongside the Decision; callers persist them to
// marketstatus_events.
type Event struct {
	RuleCode    RuleCode
	Decision    Decision
	Summary     string
	Metadata    map[string]any
	DetectedAt  time.Time
	DetectorVersion string
}

// CheckResult is the engine's full verdict.
type CheckResult struct {
	Decision Decision
	Events   []Event
}

// Reject reports whether the engine rejected. Convenience for
// callers that don't care about the per-event detail.
func (r CheckResult) Reject() bool { return r.Decision == DecisionReject }

// Warn reports whether the engine warned (and only warned).
func (r CheckResult) Warn() bool { return r.Decision == DecisionWarn }

// Errors that callers might want to type-assert.
var (
	ErrInstrumentNotFound = errors.New("marketstatus: instrument not found")
	ErrInvalidProbe       = errors.New("marketstatus: invalid probe")
)

// Engine orchestrates the rules. Stateless after construction;
// safe to share across goroutines.
type Engine struct {
	now func() time.Time
}

// NewEngine returns the production engine.
func NewEngine() *Engine {
	return &Engine{now: func() time.Time { return time.Now().UTC() }}
}

// withClock is a test seam.
func (e *Engine) withClock(now func() time.Time) *Engine {
	if now != nil {
		e.now = now
	}
	return e
}

// Check evaluates the order probe against the per-instrument
// status and the per-market calendar day. Either input can be
// nil — engine treats nil status as "not configured" (allow,
// since we have no reason to reject) and nil calendar as
// "open by default" (same).
//
// Returns the combined verdict + the per-rule events.
func (e *Engine) Check(probe OrderProbe, status *InstrumentStatus, day *CalendarDay) (*CheckResult, error) {
	if e == nil {
		return nil, errors.New("marketstatus: nil engine")
	}
	if strings.TrimSpace(probe.InstrumentKey) == "" {
		return nil, fmt.Errorf("%w: instrument_key required", ErrInvalidProbe)
	}
	now := e.now()
	res := &CheckResult{Decision: DecisionAllow}

	// Order: hardest rejects first (suspended > halted > calendar
	// > price-limit > stale). Doing the hard rejects first means
	// the metadata they emit is the most useful "primary cause"
	// when several rules would fire at once.
	if status != nil {
		if ev := evalSuspended(status, now); ev != nil {
			res.Decision = mergeDecision(res.Decision, ev.Decision)
			res.Events = append(res.Events, *ev)
			if ev.Decision == DecisionReject {
				return res, nil
			}
		}
		if ev := evalHalted(status, now); ev != nil {
			res.Decision = mergeDecision(res.Decision, ev.Decision)
			res.Events = append(res.Events, *ev)
			if ev.Decision == DecisionReject {
				return res, nil
			}
		}
	}
	if day != nil {
		if ev := evalCalendar(probe, day, now); ev != nil {
			res.Decision = mergeDecision(res.Decision, ev.Decision)
			res.Events = append(res.Events, *ev)
			if ev.Decision == DecisionReject {
				return res, nil
			}
		}
	}
	if status != nil {
		if ev := evalPriceLimit(probe, status, now); ev != nil {
			res.Decision = mergeDecision(res.Decision, ev.Decision)
			res.Events = append(res.Events, *ev)
			if ev.Decision == DecisionReject {
				return res, nil
			}
		}
		if ev := evalStaleQuote(probe, status, now); ev != nil {
			res.Decision = mergeDecision(res.Decision, ev.Decision)
			res.Events = append(res.Events, *ev)
		}
	}
	return res, nil
}

// mergeDecision combines two decisions: reject dominates warn,
// warn dominates allow.
func mergeDecision(a, b Decision) Decision {
	if a == DecisionReject || b == DecisionReject {
		return DecisionReject
	}
	if a == DecisionWarn || b == DecisionWarn {
		return DecisionWarn
	}
	return DecisionAllow
}

// ---- per-rule evaluators ----

func evalSuspended(s *InstrumentStatus, now time.Time) *Event {
	if !strings.EqualFold(s.Status, "suspended") {
		return nil
	}
	return &Event{
		RuleCode: RuleSuspended,
		Decision: DecisionReject,
		Summary:  fmt.Sprintf("instrument %s is suspended", s.Symbol),
		Metadata: map[string]any{
			"halt_reason": s.HaltReason,
			"asset_class": s.AssetClass,
		},
		DetectedAt:      now,
		DetectorVersion: detectorVersion,
	}
}

func evalHalted(s *InstrumentStatus, now time.Time) *Event {
	if !strings.EqualFold(s.Status, "halted") {
		return nil
	}
	// halt_until in the past = effectively reopened. The DB row
	// can stay 'halted' until the scheduler flips it; the engine
	// short-circuits here so a reopen window doesn't block trades.
	if s.HaltUntil != nil && !s.HaltUntil.IsZero() && now.After(*s.HaltUntil) {
		return nil
	}
	meta := map[string]any{
		"halt_reason": s.HaltReason,
		"asset_class": s.AssetClass,
	}
	if s.HaltStartedAt != nil {
		meta["halt_started_at"] = s.HaltStartedAt.UTC().Format(time.RFC3339Nano)
	}
	if s.HaltUntil != nil {
		meta["halt_until"] = s.HaltUntil.UTC().Format(time.RFC3339Nano)
	}
	return &Event{
		RuleCode: RuleHalted,
		Decision: DecisionReject,
		Summary:  fmt.Sprintf("instrument %s is halted", s.Symbol),
		Metadata: meta,
		DetectedAt: now, DetectorVersion: detectorVersion,
	}
}

func evalCalendar(probe OrderProbe, day *CalendarDay, now time.Time) *Event {
	// is_open=false → market closed for the day.
	if !day.IsOpen {
		return &Event{
			RuleCode: RuleMarketClosed,
			Decision: DecisionReject,
			Summary:  fmt.Sprintf("market %s closed on %s", day.Market, day.TradingDate.Format("2006-01-02")),
			Metadata: map[string]any{
				"market":       day.Market,
				"trading_date": day.TradingDate.Format("2006-01-02"),
				"note":         day.Note,
			},
			DetectedAt: now, DetectorVersion: detectorVersion,
		}
	}
	// Open / half-day: if "now" is outside the local window, reject.
	tz, err := time.LoadLocation(strings.TrimSpace(day.MarketTZ))
	if err != nil || tz == nil {
		// Bad TZ → don't block; emit a warn so operators see the
		// config issue.
		return &Event{
			RuleCode: RuleMarketClosed,
			Decision: DecisionWarn,
			Summary:  fmt.Sprintf("market %s tz=%q invalid; calendar window unenforced", day.Market, day.MarketTZ),
			Metadata: map[string]any{
				"tz_error": fmt.Sprint(err),
			},
			DetectedAt: now, DetectorVersion: detectorVersion,
		}
	}
	open, openErr := parseClockOnDate(day.TradingDate, day.OpenLocal, tz)
	close, closeErr := parseClockOnDate(day.TradingDate, day.CloseLocal, tz)
	if openErr != nil || closeErr != nil {
		return &Event{
			RuleCode: RuleMarketClosed,
			Decision: DecisionWarn,
			Summary:  "calendar open/close malformed",
			Metadata: map[string]any{
				"open_local":  day.OpenLocal,
				"close_local": day.CloseLocal,
			},
			DetectedAt: now, DetectorVersion: detectorVersion,
		}
	}
	// "now" is UTC; we compare in absolute time, so the parse-in-tz
	// already handles DST and offset.
	if now.Before(open) || now.After(close) {
		rule := RuleMarketClosed
		summary := fmt.Sprintf("market %s outside session %s..%s %s",
			day.Market, day.OpenLocal, day.CloseLocal, day.MarketTZ)
		if day.HalfDay {
			rule = RuleHalfDayClosed
			summary = fmt.Sprintf("market %s half-day closed at %s %s", day.Market, day.CloseLocal, day.MarketTZ)
		}
		return &Event{
			RuleCode: rule,
			Decision: DecisionReject,
			Summary:  summary,
			Metadata: map[string]any{
				"market":       day.Market,
				"open_local":   day.OpenLocal,
				"close_local":  day.CloseLocal,
				"market_tz":    day.MarketTZ,
				"half_day":     day.HalfDay,
				"trading_date": day.TradingDate.Format("2006-01-02"),
			},
			DetectedAt: now, DetectorVersion: detectorVersion,
		}
	}
	// Inside window — half-day deserves a warn so the PM agent
	// can see the shorter session in attribution.
	if day.HalfDay {
		return &Event{
			RuleCode: RuleHalfDayClosed,
			Decision: DecisionWarn,
			Summary:  fmt.Sprintf("market %s on half-day session (closes %s %s)", day.Market, day.CloseLocal, day.MarketTZ),
			Metadata: map[string]any{
				"close_local": day.CloseLocal,
				"market_tz":   day.MarketTZ,
			},
			DetectedAt: now, DetectorVersion: detectorVersion,
		}
	}
	return nil
}

func evalPriceLimit(probe OrderProbe, s *InstrumentStatus, now time.Time) *Event {
	// Price-limit only enforced when an intended price is known.
	// Market orders fall through; the matching layer's quote
	// will be inside the limits anyway because the same exchange
	// enforces them at the matcher.
	if probe.IntendedPrice <= 0 {
		return nil
	}
	if s.LowerLimit != nil && probe.IntendedPrice < *s.LowerLimit {
		return &Event{
			RuleCode: RulePriceLimit,
			Decision: DecisionReject,
			Summary:  fmt.Sprintf("price %.4f below lower limit %.4f", probe.IntendedPrice, *s.LowerLimit),
			Metadata: map[string]any{
				"intended_price": probe.IntendedPrice,
				"lower_limit":    *s.LowerLimit,
				"upper_limit":    nullableFloat(s.UpperLimit),
				"side":           probe.Side,
			},
			DetectedAt: now, DetectorVersion: detectorVersion,
		}
	}
	if s.UpperLimit != nil && probe.IntendedPrice > *s.UpperLimit {
		return &Event{
			RuleCode: RulePriceLimit,
			Decision: DecisionReject,
			Summary:  fmt.Sprintf("price %.4f above upper limit %.4f", probe.IntendedPrice, *s.UpperLimit),
			Metadata: map[string]any{
				"intended_price": probe.IntendedPrice,
				"lower_limit":    nullableFloat(s.LowerLimit),
				"upper_limit":    *s.UpperLimit,
				"side":           probe.Side,
			},
			DetectedAt: now, DetectorVersion: detectorVersion,
		}
	}
	return nil
}

func evalStaleQuote(probe OrderProbe, s *InstrumentStatus, now time.Time) *Event {
	if s.LastQuoteAt == nil || s.LastQuoteAt.IsZero() {
		// No quote anchor at all = warn, not reject. Some
		// admin-managed bonds and OTC names sit without quote
		// updates between session opens; rejecting would block
		// legitimate trades.
		return &Event{
			RuleCode: RuleStaleQuote,
			Decision: DecisionWarn,
			Summary:  fmt.Sprintf("no quote timestamp for %s", s.Symbol),
			Metadata: map[string]any{
				"asset_class": s.AssetClass,
			},
			DetectedAt: now, DetectorVersion: detectorVersion,
		}
	}
	budget := EffectiveStalenessBudget(s)
	age := now.Sub(*s.LastQuoteAt)
	if age <= budget {
		return nil
	}
	return &Event{
		RuleCode: RuleStaleQuote,
		Decision: DecisionWarn,
		Summary:  fmt.Sprintf("quote age %s exceeds %s budget for %s", age.Round(time.Second), budget, s.Symbol),
		Metadata: map[string]any{
			"asset_class":          s.AssetClass,
			"quote_age_seconds":    int64(age.Seconds()),
			"budget_seconds":       int64(budget.Seconds()),
			"last_quote_at":        s.LastQuoteAt.UTC().Format(time.RFC3339Nano),
			"intended_price":       probe.IntendedPrice,
			"last_quote_price":     nullableFloat(s.LastQuotePrice),
		},
		DetectedAt: now, DetectorVersion: detectorVersion,
	}
}

// EffectiveStalenessBudget returns the freshness threshold in
// effect for a status row: explicit override → asset-class
// default → fallback.
func EffectiveStalenessBudget(s *InstrumentStatus) time.Duration {
	if s == nil {
		return fallbackStalenessBudget
	}
	if s.StalenessBudget != nil && *s.StalenessBudget > 0 {
		return *s.StalenessBudget
	}
	if s.AssetClass != "" {
		if d, ok := defaultStalenessBudget[strings.ToLower(s.AssetClass)]; ok {
			return d
		}
	}
	return fallbackStalenessBudget
}

// ---- helpers ----

// parseClockOnDate combines a YYYY-MM-DD date with an HH:MM:SS
// time string in the given timezone. Half-day rows like
// "12:00:00" go through unchanged. We accept HH:MM as well as
// HH:MM:SS so the calendar uploader doesn't have to pad.
func parseClockOnDate(date time.Time, clock string, loc *time.Location) (time.Time, error) {
	clock = strings.TrimSpace(clock)
	if clock == "" {
		return time.Time{}, errors.New("empty clock")
	}
	formats := []string{"15:04:05", "15:04"}
	for _, f := range formats {
		t, err := time.Parse(f, clock)
		if err == nil {
			return time.Date(date.Year(), date.Month(), date.Day(),
				t.Hour(), t.Minute(), t.Second(), 0, loc), nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable clock %q", clock)
}

func nullableFloat(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}
