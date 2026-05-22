package promotion

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/fundai/server/internal/repository"
)

// BacktestLookup is the read-side dependency the service needs to
// validate "is this a real, completed backtest that can serve as
// a promotion baseline?". The repository's full repo type returns
// a wider record than we need, so we narrow it here.
//
// The lookup function returns a snapshot of just the fields that
// matter for promotion: the fund the job belonged to, its
// terminal status, and the headline metrics. nil-with-nil-error
// signals "no such job".
type BacktestLookup func(ctx context.Context, jobID string) (*BacktestBasis, error)

// BacktestBasis is the lookup's projection of the basis backtest.
// Smaller than repository.BacktestJobFull so unit tests can stub
// it without dragging the whole repo in.
type BacktestBasis struct {
	JobID            string
	FundID           string
	Status           string
	EngineKind       string
	CumulativeReturn float64
	AnnualizedReturn float64
	SharpeRatio      float64
	Volatility       float64
	MaxDrawdown      float64
	WinRate          float64
	TradeCount       int
	// WalkForward, when non-nil, marks the basis as an OOS-
	// validated run and supplies the stricter baseline used by
	// the decay monitor.
	OOSReturn *float64
	OOSSharpe *float64
	// HasWalkForward records whether the basis ran with the
	// walk-forward sub-spec. Used by RequireWalkForward gating.
	HasWalkForward bool
}

// IDGen returns a fresh promotion / event / snapshot ID. Tests
// inject a deterministic generator; production wires this to
// uuid.NewString().
type IDGen func() string

// Clock returns "now". Injectable so tests can pin time.
type Clock func() time.Time

// Service orchestrates the Promotion lifecycle. State changes
// always go through here so the audit log + status update + side-
// effects stay in lockstep.
type Service struct {
	Repo            *repository.PromotionRepo
	LookupBacktest  BacktestLookup
	NewID           IDGen
	Now             Clock
	// RequireWalkForward, when true, rejects basis jobs that ran
	// without the walkForward sub-spec — stricter gate that
	// forces OOS validation before any strategy promotion.
	RequireWalkForward bool
	// DefaultShadowDays / DefaultDecayRatio are the values
	// applied when the proposer leaves them blank.
	DefaultShadowDays int
	DefaultDecayRatio float64
}

// ProposeInput is what the API hands to Propose. We use a struct
// so adding optional knobs (notes, override engineParams, etc.)
// is non-breaking.
type ProposeInput struct {
	FundID       string
	ProposedBy   string
	BasisJobID   string
	EngineParams EngineParams
	ShadowDays   *int
	DecayRatio   *float64
	Notes        string
}

// Propose creates a pending_review promotion using the basis
// backtest's engine kind + metrics as the seed. Defaults fill in
// missing knobs; explicit values override.
//
// Side effects: one row in strategy_promotions, one row in
// promotion_events (event_type='proposed').
func (s *Service) Propose(ctx context.Context, in ProposeInput) (*Promotion, error) {
	if err := s.ensureLookup(); err != nil {
		return nil, err
	}
	basis, err := s.LookupBacktest(ctx, in.BasisJobID)
	if err != nil {
		return nil, err
	}
	if basis == nil {
		return nil, fmt.Errorf("%w: backtest %q not found", ErrBasisNotEligible, in.BasisJobID)
	}
	if basis.FundID != in.FundID {
		return nil, fmt.Errorf("%w: backtest %q belongs to fund %q, not %q", ErrBasisNotEligible, in.BasisJobID, basis.FundID, in.FundID)
	}
	if basis.Status != "completed" {
		return nil, fmt.Errorf("%w: basis status %q (must be 'completed')", ErrBasisNotEligible, basis.Status)
	}
	if s.RequireWalkForward && !basis.HasWalkForward {
		return nil, fmt.Errorf("%w: basis must be a walk-forward run", ErrBasisNotEligible)
	}

	shadowDays := s.DefaultShadowDays
	if in.ShadowDays != nil {
		shadowDays = *in.ShadowDays
	}
	decay := s.DefaultDecayRatio
	if in.DecayRatio != nil {
		decay = *in.DecayRatio
	}
	if decay == 0 {
		decay = 0.5 // sane default
	}

	now := s.Now()
	p := Promotion{
		ID:           s.NewID(),
		FundID:       in.FundID,
		ProposedBy:   in.ProposedBy,
		BasisJobID:   in.BasisJobID,
		EngineKind:   basis.EngineKind,
		EngineParams: cloneParams(in.EngineParams),
		BaselineMetrics: BaselineMetrics{
			CumulativeReturn: basis.CumulativeReturn,
			AnnualizedReturn: basis.AnnualizedReturn,
			SharpeRatio:      basis.SharpeRatio,
			Volatility:       basis.Volatility,
			MaxDrawdown:      basis.MaxDrawdown,
			WinRate:          basis.WinRate,
			TradeCount:       basis.TradeCount,
			OOSReturn:        basis.OOSReturn,
			OOSSharpe:        basis.OOSSharpe,
		},
		Status:     StatusPendingReview,
		ShadowDays: shadowDays,
		DecayRatio: decay,
		Notes:      in.Notes,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	row, err := domainToRow(p)
	if err != nil {
		return nil, err
	}
	if err := s.Repo.Insert(ctx, row); err != nil {
		return nil, err
	}
	if err := s.audit(ctx, p.ID, in.ProposedBy, EventProposed, map[string]any{
		"basisJobId": in.BasisJobID,
		"engineKind": basis.EngineKind,
	}); err != nil {
		// Audit failure is logged-and-continue territory; the
		// promotion row already exists and is the source of truth.
		// We still surface the error so the caller knows the
		// timeline may be incomplete.
		return &p, fmt.Errorf("promotion proposed but audit log failed: %w", err)
	}
	return &p, nil
}

// Approve flips pending_review → approved (or directly to
// shadow / active depending on ShadowDays). Approver MUST differ
// from proposer when EnforceDualControl is wired — but we leave
// that check to the API/handler layer where the company-level
// dual-control flag lives.
func (s *Service) Approve(ctx context.Context, promotionID, approver string) (*Promotion, error) {
	if err := s.ensureWired(); err != nil {
		return nil, err
	}
	p, err := s.get(ctx, promotionID)
	if err != nil {
		return nil, err
	}
	if err := EnsureTransition(p.Status, StatusApproved); err != nil {
		return nil, err
	}
	now := s.Now()
	target := NextStatusAfterApproval(*p)

	upd := repository.StatusUpdate{
		Status:      string(target),
		ApprovedBy:  sql.NullString{String: approver, Valid: true},
		ApprovedAt:  sql.NullTime{Time: now, Valid: true},
	}
	if target == StatusShadow {
		upd.ShadowStartedAt = sql.NullTime{Time: now, Valid: true}
	} else if target == StatusActive {
		upd.ActivatedAt = sql.NullTime{Time: now, Valid: true}
		if err := s.supersedePriorActive(ctx, p.FundID, p.ID); err != nil {
			return nil, err
		}
	}
	if err := s.Repo.UpdateStatus(ctx, promotionID, upd); err != nil {
		return nil, err
	}
	if err := s.audit(ctx, promotionID, approver, EventApproved, map[string]any{"target": string(target)}); err != nil {
		return nil, err
	}
	if target == StatusShadow {
		_ = s.audit(ctx, promotionID, approver, EventShadowStarted, nil)
	} else if target == StatusActive {
		_ = s.audit(ctx, promotionID, approver, EventActivated, nil)
	}
	return s.get(ctx, promotionID)
}

// Reject is the terminal path from pending_review (or any non-
// terminal status, with a reason). The audit log records the why.
func (s *Service) Reject(ctx context.Context, promotionID, rejector, reason string) (*Promotion, error) {
	if err := s.ensureWired(); err != nil {
		return nil, err
	}
	p, err := s.get(ctx, promotionID)
	if err != nil {
		return nil, err
	}
	if err := EnsureTransition(p.Status, StatusRejected); err != nil {
		return nil, err
	}
	now := s.Now()
	if err := s.Repo.UpdateStatus(ctx, promotionID, repository.StatusUpdate{
		Status:         string(StatusRejected),
		RejectedBy:     sql.NullString{String: rejector, Valid: true},
		RejectedAt:     sql.NullTime{Time: now, Valid: true},
		RejectedReason: sql.NullString{String: reason, Valid: reason != ""},
	}); err != nil {
		return nil, err
	}
	_ = s.audit(ctx, promotionID, rejector, EventRejected, map[string]any{"reason": reason})
	return s.get(ctx, promotionID)
}

// Activate is the manual promote-from-shadow step. shadow →
// active. Bumps prior active out of the way first.
func (s *Service) Activate(ctx context.Context, promotionID, actor string) (*Promotion, error) {
	if err := s.ensureWired(); err != nil {
		return nil, err
	}
	p, err := s.get(ctx, promotionID)
	if err != nil {
		return nil, err
	}
	if err := EnsureTransition(p.Status, StatusActive); err != nil {
		return nil, err
	}
	now := s.Now()
	if err := s.supersedePriorActive(ctx, p.FundID, p.ID); err != nil {
		return nil, err
	}
	if err := s.Repo.UpdateStatus(ctx, promotionID, repository.StatusUpdate{
		Status:              string(StatusActive),
		ShadowCompletedAt:   sql.NullTime{Time: now, Valid: true},
		ActivatedAt:         sql.NullTime{Time: now, Valid: true},
	}); err != nil {
		return nil, err
	}
	_ = s.audit(ctx, promotionID, actor, EventShadowFinished, nil)
	_ = s.audit(ctx, promotionID, actor, EventActivated, nil)
	return s.get(ctx, promotionID)
}

// Deactivate handles both manual rollback and auto-decay. The
// caller picks the terminal status (rolled_back vs decayed) and
// the reason gets logged.
func (s *Service) Deactivate(ctx context.Context, promotionID string, target Status, actor, reason string) (*Promotion, error) {
	if err := s.ensureWired(); err != nil {
		return nil, err
	}
	if target != StatusRolledBack && target != StatusDecayed && target != StatusSuperseded {
		return nil, fmt.Errorf("%w: Deactivate target must be rolled_back/decayed/superseded, got %s", ErrIllegalTransition, target)
	}
	p, err := s.get(ctx, promotionID)
	if err != nil {
		return nil, err
	}
	if err := EnsureTransition(p.Status, target); err != nil {
		return nil, err
	}
	now := s.Now()
	if err := s.Repo.UpdateStatus(ctx, promotionID, repository.StatusUpdate{
		Status:             string(target),
		DeactivatedAt:      sql.NullTime{Time: now, Valid: true},
		DeactivatedReason:  sql.NullString{String: reason, Valid: reason != ""},
	}); err != nil {
		return nil, err
	}
	eventType := EventRolledBack
	switch target {
	case StatusDecayed:
		eventType = EventDecayDetected
	case StatusSuperseded:
		eventType = EventSuperseded
	}
	_ = s.audit(ctx, promotionID, actor, eventType, map[string]any{"reason": reason})
	return s.get(ctx, promotionID)
}

// ResolveActive returns the currently-active promotion for a
// fund, or nil-with-nil-error when none exists. The
// ProductionEngineResolver wraps this for the PMAgent.
func (s *Service) ResolveActive(ctx context.Context, fundID string) (*Promotion, error) {
	if err := s.ensureWired(); err != nil {
		return nil, err
	}
	row, err := s.Repo.GetActiveByFund(ctx, fundID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil
	}
	return rowToDomain(row)
}

// List returns recent promotions for a fund (newest first).
func (s *Service) List(ctx context.Context, fundID string, limit int) ([]*Promotion, error) {
	if err := s.ensureWired(); err != nil {
		return nil, err
	}
	rows, err := s.Repo.ListByFund(ctx, fundID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]*Promotion, 0, len(rows))
	for _, r := range rows {
		p, err := rowToDomain(r)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// Get returns one promotion by ID.
func (s *Service) Get(ctx context.Context, id string) (*Promotion, error) {
	if err := s.ensureWired(); err != nil {
		return nil, err
	}
	return s.get(ctx, id)
}

// Events returns the audit log for the detail page timeline.
func (s *Service) Events(ctx context.Context, promotionID string) ([]*Event, error) {
	if err := s.ensureWired(); err != nil {
		return nil, err
	}
	rows, err := s.Repo.ListEvents(ctx, promotionID)
	if err != nil {
		return nil, err
	}
	out := make([]*Event, 0, len(rows))
	for _, r := range rows {
		var payload map[string]any
		if len(r.Payload) > 0 {
			_ = json.Unmarshal(r.Payload, &payload)
		}
		out = append(out, &Event{
			ID:          r.ID,
			PromotionID: r.PromotionID,
			EventType:   EventType(r.EventType),
			ActorUserID: nullableString(r.ActorUserID),
			Payload:     payload,
			CreatedAt:   r.CreatedAt,
		})
	}
	return out, nil
}

// --- internals ---

func (s *Service) ensureWired() error {
	if s == nil || s.Repo == nil || s.NewID == nil || s.Now == nil {
		return errors.New("promotion: service not wired (Repo / NewID / Now required)")
	}
	return nil
}

// ensureLookup is the stricter wiring check used only by paths
// that look up backtest metadata (Propose). Split from ensureWired
// so callers exercising the lifecycle without backtest IO can
// still operate on existing rows.
func (s *Service) ensureLookup() error {
	if err := s.ensureWired(); err != nil {
		return err
	}
	if s.LookupBacktest == nil {
		return errors.New("promotion: LookupBacktest dependency not wired")
	}
	return nil
}

func (s *Service) get(ctx context.Context, id string) (*Promotion, error) {
	row, err := s.Repo.Get(ctx, id)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrPromotionNotFound
		}
		return nil, err
	}
	return rowToDomain(row)
}

// supersedePriorActive flips any existing active promotion for
// the same fund to 'superseded' before we install a new one. Done
// in the service (not the repo) so the audit log captures the
// hand-off. We don't make this transactional with the new row's
// activation update — the partial unique index makes the worst
// case "newer activate fails" rather than "two actives".
func (s *Service) supersedePriorActive(ctx context.Context, fundID, newID string) error {
	prior, err := s.Repo.GetActiveByFund(ctx, fundID)
	if err != nil {
		return err
	}
	if prior == nil || prior.ID == newID {
		return nil
	}
	now := s.Now()
	if err := s.Repo.UpdateStatus(ctx, prior.ID, repository.StatusUpdate{
		Status:             string(StatusSuperseded),
		DeactivatedAt:      sql.NullTime{Time: now, Valid: true},
		DeactivatedReason:  sql.NullString{String: "superseded by " + newID, Valid: true},
	}); err != nil {
		return fmt.Errorf("supersede prior active: %w", err)
	}
	_ = s.audit(ctx, prior.ID, "", EventSuperseded, map[string]any{"supersededBy": newID})
	return nil
}

func (s *Service) audit(ctx context.Context, promotionID, actor string, ev EventType, payload map[string]any) error {
	blob, _ := json.Marshal(payload)
	if blob == nil {
		blob = []byte("{}")
	}
	return s.Repo.InsertEvent(ctx, &repository.PromotionEventRow{
		ID:          s.NewID(),
		PromotionID: promotionID,
		EventType:   string(ev),
		ActorUserID: sql.NullString{String: actor, Valid: actor != ""},
		Payload:     blob,
		CreatedAt:   s.Now(),
	})
}

// --- domain ↔ row translation ---

func domainToRow(p Promotion) (*repository.PromotionRow, error) {
	params, err := json.Marshal(p.EngineParams)
	if err != nil {
		return nil, fmt.Errorf("marshal engine params: %w", err)
	}
	baseline, err := json.Marshal(p.BaselineMetrics)
	if err != nil {
		return nil, fmt.Errorf("marshal baseline: %w", err)
	}
	return &repository.PromotionRow{
		ID:              p.ID,
		FundID:          p.FundID,
		ProposedBy:      p.ProposedBy,
		BasisJobID:      p.BasisJobID,
		EngineKind:      p.EngineKind,
		EngineParams:    params,
		BaselineMetrics: baseline,
		Status:          string(p.Status),
		ShadowDays:      p.ShadowDays,
		DecayRatio:      p.DecayRatio,
		Notes:           nullableStringSrc(p.Notes),
		CreatedAt:       p.CreatedAt,
		UpdatedAt:       p.UpdatedAt,
	}, nil
}

func rowToDomain(r *repository.PromotionRow) (*Promotion, error) {
	if r == nil {
		return nil, nil
	}
	var params EngineParams
	if len(r.EngineParams) > 0 {
		_ = json.Unmarshal(r.EngineParams, &params)
	}
	var baseline BaselineMetrics
	if len(r.BaselineMetrics) > 0 {
		_ = json.Unmarshal(r.BaselineMetrics, &baseline)
	}
	p := &Promotion{
		ID:                r.ID,
		FundID:            r.FundID,
		ProposedBy:        r.ProposedBy,
		BasisJobID:        r.BasisJobID,
		EngineKind:        r.EngineKind,
		EngineParams:      params,
		BaselineMetrics:   baseline,
		Status:            Status(r.Status),
		ShadowDays:        r.ShadowDays,
		DecayRatio:        r.DecayRatio,
		ApprovedBy:        nullableString(r.ApprovedBy),
		RejectedBy:        nullableString(r.RejectedBy),
		RejectedReason:    nullableString(r.RejectedReason),
		DeactivatedReason: nullableString(r.DeactivatedReason),
		Notes:             nullableString(r.Notes),
		CreatedAt:         r.CreatedAt,
		UpdatedAt:         r.UpdatedAt,
	}
	if r.ApprovedAt.Valid {
		p.ApprovedAt = r.ApprovedAt.Time
	}
	if r.RejectedAt.Valid {
		p.RejectedAt = r.RejectedAt.Time
	}
	if r.ShadowStartedAt.Valid {
		p.ShadowStartedAt = r.ShadowStartedAt.Time
	}
	if r.ShadowCompletedAt.Valid {
		p.ShadowCompletedAt = r.ShadowCompletedAt.Time
	}
	if r.ActivatedAt.Valid {
		p.ActivatedAt = r.ActivatedAt.Time
	}
	if r.DeactivatedAt.Valid {
		p.DeactivatedAt = r.DeactivatedAt.Time
	}
	return p, nil
}

func cloneParams(p EngineParams) EngineParams {
	if len(p) == 0 {
		return EngineParams{}
	}
	out := make(EngineParams, len(p))
	for k, v := range p {
		out[k] = v
	}
	return out
}

func nullableString(s sql.NullString) string {
	if s.Valid {
		return s.String
	}
	return ""
}

func nullableStringSrc(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
