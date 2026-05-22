// Package promotion implements the strategy promotion gate
// (Phase 2J), shadow-mode comparison (Phase 2K), and live-vs-
// backtest decay monitoring (Phase 2L).
//
// Lifecycle
//
//	pending_review → approved → shadow → active → superseded
//	                    ↓                    ↓        ↓
//	                rejected            rolled_back  decayed
//
// A Promotion bundles a (basis backtest, target engine, target
// params) tuple and walks through review → optional shadow run →
// activation. While active, a per-fund unique-index ensures only
// one Promotion drives the PMAgent. When a new Promotion is
// activated it supersedes the old one; when decay metrics breach
// the configured ratio the monitor flips it to "decayed" and
// rolls back to whatever was previously superseded (if any).
//
// Why the schema lives separately from the existing `funds.config`
// JSONB: the Promotion is a long-lived, audited object that
// outlives any single config change. We keep funds.config for
// ad-hoc admin overrides and store rich strategy lineage here.
package promotion

import (
	"errors"
	"fmt"
	"time"
)

// Status enumerates every valid promotion state. The string
// values mirror the CHECK constraint on strategy_promotions.status
// so the DB and the Go layer agree by construction.
type Status string

const (
	StatusPendingReview Status = "pending_review"
	StatusApproved      Status = "approved"
	StatusShadow        Status = "shadow"
	StatusActive        Status = "active"
	StatusSuperseded    Status = "superseded"
	StatusRejected      Status = "rejected"
	StatusRolledBack    Status = "rolled_back"
	StatusDecayed       Status = "decayed"
)

// IsTerminal reports whether the status is final — the promotion
// will never transition again. Used by the API layer to short-
// circuit further state changes and by the scheduler to skip
// decay scans.
func (s Status) IsTerminal() bool {
	switch s {
	case StatusSuperseded, StatusRejected, StatusRolledBack, StatusDecayed:
		return true
	default:
		return false
	}
}

// IsLive reports whether the promotion is currently steering
// production behaviour — either dry-running shadow comparisons or
// fully driving the PMAgent.
func (s Status) IsLive() bool {
	return s == StatusShadow || s == StatusActive
}

// EngineParams is a JSON-friendly bag of engine-specific knobs.
// Whatever Decision engine the promotion targets reads this back
// out and configures itself. We keep it opaque at the promotion
// layer so adding a new param doesn't require a migration.
type EngineParams map[string]any

// BaselineMetrics is the snapshot of the basis backtest's
// headline result, captured at proposal time. The decay monitor
// compares rolling live metrics against this; storing it on the
// Promotion (rather than re-reading the backtest) means the
// baseline is immune to later backtest re-runs.
type BaselineMetrics struct {
	CumulativeReturn float64 `json:"cumulativeReturn"`
	AnnualizedReturn float64 `json:"annualizedReturn"`
	SharpeRatio      float64 `json:"sharpeRatio"`
	Volatility       float64 `json:"volatility"`
	MaxDrawdown      float64 `json:"maxDrawdown"`
	WinRate          float64 `json:"winRate"`
	TradeCount       int     `json:"tradeCount"`
	// OOSReturn / OOSSharpe are populated only when the basis
	// backtest used walk-forward validation. They're a stricter
	// baseline because they reflect out-of-sample behaviour,
	// not the in-sample fit. The decay evaluator prefers OOS
	// metrics when available.
	OOSReturn *float64 `json:"oosReturn,omitempty"`
	OOSSharpe *float64 `json:"oosSharpe,omitempty"`
}

// EffectiveSharpe returns the OOS Sharpe when present, otherwise
// the in-sample Sharpe. Used as the denominator for the decay
// monitor's ratio.
func (b BaselineMetrics) EffectiveSharpe() float64 {
	if b.OOSSharpe != nil {
		return *b.OOSSharpe
	}
	return b.SharpeRatio
}

// Promotion is the in-memory domain object. The Repo layer
// converts between this and the DB row representation.
type Promotion struct {
	ID                string
	FundID            string
	ProposedBy        string
	BasisJobID        string
	EngineKind        string
	EngineParams      EngineParams
	BaselineMetrics   BaselineMetrics
	Status            Status
	ShadowDays        int
	DecayRatio        float64
	ApprovedBy        string
	ApprovedAt        time.Time
	RejectedBy        string
	RejectedAt        time.Time
	RejectedReason    string
	ShadowStartedAt   time.Time
	ShadowCompletedAt time.Time
	ActivatedAt       time.Time
	DeactivatedAt     time.Time
	DeactivatedReason string
	Notes             string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Validate returns a sticky error when the proposal is
// structurally invalid. Used by the service before persistence so
// the caller gets a 400 instead of a row that can never legally
// transition.
func (p Promotion) Validate() error {
	if p.FundID == "" {
		return fmt.Errorf("%w: fundID required", ErrInvalidPromotion)
	}
	if p.BasisJobID == "" {
		return fmt.Errorf("%w: basisJobId required", ErrInvalidPromotion)
	}
	if p.EngineKind == "" {
		return fmt.Errorf("%w: engineKind required", ErrInvalidPromotion)
	}
	if p.ProposedBy == "" {
		return fmt.Errorf("%w: proposedBy required", ErrInvalidPromotion)
	}
	if p.ShadowDays < 0 || p.ShadowDays > 90 {
		return fmt.Errorf("%w: shadowDays must be in [0, 90]", ErrInvalidPromotion)
	}
	if p.DecayRatio <= 0 || p.DecayRatio >= 1 {
		return fmt.Errorf("%w: decayRatio must be in (0, 1)", ErrInvalidPromotion)
	}
	return nil
}

// EventType labels the audit-log payloads. Matches the strings
// the scheduler / service write to promotion_events.event_type.
type EventType string

const (
	EventProposed       EventType = "proposed"
	EventApproved       EventType = "approved"
	EventRejected       EventType = "rejected"
	EventShadowStarted  EventType = "shadow_started"
	EventShadowFinished EventType = "shadow_finished"
	EventActivated      EventType = "activated"
	EventSuperseded     EventType = "superseded"
	EventRolledBack     EventType = "rolled_back"
	EventDecayDetected  EventType = "decay_detected"
)

// Event is a single audit-log entry. Append-only; the UI renders
// these as a timeline on the promotion detail page.
type Event struct {
	ID           string
	PromotionID  string
	EventType    EventType
	ActorUserID  string
	Payload      map[string]any
	CreatedAt    time.Time
}

// ShadowDiff is one trading-day comparison row. It records what
// the candidate Promotion would have decided and what the
// currently-active engine actually decided. The operator uses
// the agreement ratio to judge promote-readiness.
type ShadowDiff struct {
	ID             string
	PromotionID    string
	TradingDate    time.Time
	ShadowDecision map[string]any
	ActiveDecision map[string]any
	Agreement      bool
	CreatedAt      time.Time
}

// HealthSnapshot is one decay-monitor sample. Snapshots are
// time-stamped so the UI can plot the sharpe-decay-ratio against
// time and the auto-downgrade scheduler can identify "fresh
// breach vs old noise".
type HealthSnapshot struct {
	ID                string
	PromotionID       string
	SnapshotAt        time.Time
	WindowDays        int
	ActualSharpe      *float64
	ActualReturn      *float64
	ActualMaxDrawdown *float64
	ActualTradeCount  int
	SharpeDecayRatio  *float64
	DecayFlag         bool
	Notes             string
}

// Sentinel errors. The service / handler layers wrap these with
// context-specific messages.
var (
	ErrInvalidPromotion    = errors.New("invalid promotion")
	ErrIllegalTransition   = errors.New("illegal promotion status transition")
	ErrPromotionNotFound   = errors.New("promotion not found")
	ErrBasisNotEligible    = errors.New("basis backtest is not eligible for promotion")
	ErrActiveAlreadyExists = errors.New("an active promotion already exists for this fund")
)
