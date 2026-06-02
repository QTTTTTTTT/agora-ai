// Package surveillance implements P1-7 — trade surveillance / market
// abuse detection.
//
// What this package owns
//
//   - Domain types: Event, EventInput (a trade snapshot), Rule
//     interface, RunResult.
//   - Three concrete rules:
//       * WashTradeRule        — round-trip with no economic intent
//       * MarkingCloseRule     — outsized order within last N min
//                                of the trading day
//       * SelfTradePairRule    — buy + sell from the same fund on
//                                the same instrument within an
//                                ultra-tight window (matched cross)
//   - The Engine that runs all configured rules and dedupes by
//     fingerprint.
//   - The DB-backed Repo for persisting Events + Runs and managing
//     the review workflow.
//
// What this package does NOT own
//
//   - Reading trades from the DB. The cmd/server adapter
//     (`surveillance_snapshot.go`) builds an []EventInput from
//     trade_executions and hands it off; the engine is pure.
//   - Scheduling. The intraday scan loop lives in
//     cmd/server/surveillance_loop.go alongside the FX and recon
//     loops.
//
// Why "best-effort" rather than authoritative
//
// True market-abuse detection at exchange level uses level-2 order
// book data, intent classification, and human compliance review.
// Here we operate purely on our own trade_executions stream, so the
// rules are deliberately conservative: every detected event lands
// as `open` for a human reviewer to clear or escalate, never auto
// blocked. False-positive rate is acceptable; false-negative rate
// (missing a real abuse) is what we tune for.

package surveillance

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"time"
)

// RuleCode is the closed vocabulary the engine emits. Matches the
// CHECK constraint on `surveillance_events.rule_code`.
type RuleCode string

const (
	RuleWashTrade          RuleCode = "wash_trade"
	RuleMarkingClose       RuleCode = "marking_close"
	RuleSelfTradePair      RuleCode = "self_trade_pair"
	RuleRapidFireReversal  RuleCode = "rapid_fire_reversal"
	RuleLayeringSuspect    RuleCode = "layering_suspect"
)

// Severity mirrors surveillance_events.severity.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// EventStatus mirrors surveillance_events.status.
type EventStatus string

const (
	StatusOpen       EventStatus = "open"
	StatusReviewing  EventStatus = "reviewing"
	StatusCleared    EventStatus = "cleared"
	StatusEscalated  EventStatus = "escalated"
)

// TradeSnapshot is the engine's input shape per trade. Adapters
// in cmd/server populate this from trade_executions.
type TradeSnapshot struct {
	ID            string
	FundID        string
	Symbol        string
	InstrumentKey string
	Side          string // "buy" | "sell"
	Quantity      float64
	Price         float64
	Notional      float64 // pre-computed |Quantity| * Price for sizing rules
	ExecutedAt    time.Time
	Status        string  // "filled" | "partial" — engine ignores cancels
}

// MarketContext is the optional reference the engine consults when
// the rule needs price-level / volume-level perspective.
type MarketContext struct {
	// SessionClose is the official close time for the trading day
	// the snapshot lives in. MarkingCloseRule uses
	// (close - trade.ExecutedAt) <= window to decide "near close".
	// Stored UTC. Zero value disables the rule.
	SessionClose time.Time
	// AvgDailyNotional gives the rule a reference for "outsized";
	// 0 disables the size threshold half of the rule.
	AvgDailyNotional map[string]float64 // symbol → avg notional
	// RecentVWAP gives the rule a reference price; 0 disables the
	// price-deviation half of the rule.
	RecentVWAP map[string]float64 // symbol → vwap
}

// Event is the engine's output shape for one detection.
type Event struct {
	ID              string // populated by the Repo
	FundID          string
	RuleCode        RuleCode
	Severity        Severity
	Symbol          string
	InstrumentKey   string
	WindowStart     time.Time
	WindowEnd       time.Time
	TradeIDs        []string
	Summary         string
	Metadata        map[string]any
	Status          EventStatus
	DetectorVersion string
	Fingerprint     string
	DetectedAt      time.Time
}

// Rule is the abstraction the Engine drives. A rule is stateless
// across calls to Detect; any sliding-window state lives on the
// stack inside Detect.
type Rule interface {
	Code() RuleCode
	Detect(snap []TradeSnapshot, ctx *MarketContext) []Event
}

// RunResult is the engine's per-call output.
type RunResult struct {
	Events []Event
	// CountsBySeverity is derived from Events; the Repo
	// uses it to populate surveillance_runs.event_count_*.
	CountsBySeverity map[Severity]int
	// CountsByRule is exported so the metrics layer can bump
	// per-rule counters in O(1).
	CountsByRule map[RuleCode]int
}

// ----- Errors -----

var (
	// ErrEventNotFound signals surveillance_events.id missing.
	ErrEventNotFound = errors.New("surveillance: event not found")

	// ErrInvalidStatus signals an unknown review status. The
	// Repo's UpdateStatus rejects with this; the handler maps to
	// 400.
	ErrInvalidStatus = errors.New("surveillance: invalid status")
)

// ----- Helpers -----

// canonicalSymbol normalises a ticker for matching.
func canonicalSymbol(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}

// fingerprintFor computes the dedup fingerprint for an event.
// Stable: same (fund, rule, sorted trade IDs) → same fingerprint.
// We sort the trade IDs first so a re-run that visits the same
// pattern from a different anchor still produces the same key.
func fingerprintFor(fundID string, rule RuleCode, tradeIDs []string) string {
	ids := append([]string(nil), tradeIDs...)
	sort.Strings(ids)
	h := sha256.New()
	h.Write([]byte(fundID))
	h.Write([]byte{0})
	h.Write([]byte(rule))
	h.Write([]byte{0})
	for _, id := range ids {
		h.Write([]byte(id))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:48]
}

// isFilledLike returns true for executions the rules should
// consider — pending / cancelled / rejected don't represent
// market activity for surveillance purposes.
func isFilledLike(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "filled", "partial":
		return true
	}
	return false
}

// pickQty returns the qty the rule should treat as "the trade's
// size". We prefer Quantity but fall back to absolute notional /
// price if quantity is missing (legacy data path before P1-1).
func pickQty(t TradeSnapshot) float64 {
	if t.Quantity > 0 {
		return t.Quantity
	}
	if t.Price > 0 && t.Notional > 0 {
		return t.Notional / t.Price
	}
	return 0
}
