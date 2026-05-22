package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/promotion"
	"github.com/fundai/server/internal/repository"
)

// promotionServiceAdapter wires the Phase 2J/K/L domain layer
// behind the api.PromotionService façade.
//
// Responsibilities, mirroring backtestServiceAdapter:
//
//  1. Reuse authorizeFundAccess so every fund-scoped endpoint
//     walks the same RBAC path as the rest of the platform.
//  2. Translate api.Promotion ↔ promotion.Promotion so the api
//     package stays free of the promotion / repository imports.
//  3. Resolve the BacktestLookup dependency by reading backtest
//     job rows directly from BacktestRepo; the promotion package
//     remains storage-agnostic.
//  4. Optionally call back into the resolver to invalidate its
//     fund-scoped cache on every transition that could change
//     the active engine.
type promotionServiceAdapter struct {
	db            *sql.DB
	fundRepo      *repository.FundRepo
	companyRepo   *repository.FundCompanyRepo
	backtestRepo  *repository.BacktestRepo
	promotionRepo *repository.PromotionRepo
	svc           *promotion.Service
	shadow        *promotion.ShadowComparator
	decay         *promotion.DecayMonitor
	// resolver is the optional production engine resolver. When
	// set, transitions that change the active promotion call
	// resolver.Invalidate(fundID) so the next PMAgent run picks
	// up the new engine without waiting for the TTL.
	resolver *promotion.Resolver
}

// newPromotionServiceAdapter builds the adapter and the underlying
// promotion.Service / ShadowComparator / DecayMonitor. db is
// required — the promotion lifecycle is not viable without
// persistence — but we tolerate missing repositories at runtime by
// returning friendly errors from the service methods.
func newPromotionServiceAdapter(
	db *sql.DB,
	backtestRepo *repository.BacktestRepo,
	liveLookup promotion.LiveMetricsLookup,
) *promotionServiceAdapter {
	if db == nil {
		return nil
	}
	a := &promotionServiceAdapter{
		db:            db,
		fundRepo:      repository.NewFundRepo(db),
		companyRepo:   repository.NewFundCompanyRepo(db),
		backtestRepo:  backtestRepo,
		promotionRepo: repository.NewPromotionRepo(db),
	}
	now := func() time.Time { return time.Now().UTC() }
	newID := func() string { return uuid.NewString() }

	a.svc = &promotion.Service{
		Repo:              a.promotionRepo,
		LookupBacktest:    a.lookupBacktest,
		NewID:             newID,
		Now:               now,
		DefaultShadowDays: 7,
		DefaultDecayRatio: 0.5,
	}
	a.shadow = &promotion.ShadowComparator{
		Repo:  a.promotionRepo,
		NewID: newID,
		Now:   now,
	}
	a.decay = &promotion.DecayMonitor{
		Service:                     a.svc,
		Repo:                        a.promotionRepo,
		LiveLookup:                  liveLookup,
		NewID:                       newID,
		Now:                         now,
		WindowDays:                  30,
		MinSnapshotsBeforeDowngrade: 3,
	}
	return a
}

// WithResolver wires the production engine resolver so the
// adapter can invalidate its per-fund cache on transitions.
func (a *promotionServiceAdapter) WithResolver(r *promotion.Resolver) *promotionServiceAdapter {
	if a != nil {
		a.resolver = r
		a.decay.OnDowngrade = func(_ context.Context, fundID, _ string) {
			if a.resolver != nil {
				a.resolver.Invalidate(fundID)
			}
		}
	}
	return a
}

// Resolver returns the wired-in resolver (nil if not yet set).
func (a *promotionServiceAdapter) Resolver() *promotion.Resolver { return a.resolver }

// Shadow returns the wired comparator (used by the daily decision
// pipeline to record per-day diffs).
func (a *promotionServiceAdapter) Shadow() *promotion.ShadowComparator { return a.shadow }

// Decay returns the wired DecayMonitor so the scheduler can call
// SampleAll on its cadence.
func (a *promotionServiceAdapter) Decay() *promotion.DecayMonitor { return a.decay }

// Service returns the inner promotion service for adapter wiring
// that needs to bypass the api-facing translation (e.g. the
// scheduler issuing direct Deactivate calls).
func (a *promotionServiceAdapter) Service() *promotion.Service { return a.svc }

// --- api.PromotionService impl ---

func (a *promotionServiceAdapter) Propose(userID string, in api.ProposeInput) (*api.Promotion, error) {
	if err := a.authorize(userID, in.FundID); err != nil {
		return nil, err
	}
	out, err := a.svc.Propose(context.Background(), promotion.ProposeInput{
		FundID:       in.FundID,
		ProposedBy:   userID,
		BasisJobID:   in.BasisJobID,
		EngineParams: promotion.EngineParams(in.EngineParams),
		ShadowDays:   in.ShadowDays,
		DecayRatio:   in.DecayRatio,
		Notes:        in.Notes,
	})
	if err != nil {
		return nil, translatePromotionError(err)
	}
	a.invalidate(in.FundID)
	return promotionToAPI(out), nil
}

func (a *promotionServiceAdapter) Approve(userID, fundID, promotionID string) (*api.Promotion, error) {
	if err := a.authorize(userID, fundID); err != nil {
		return nil, err
	}
	if err := a.ensureBelongs(fundID, promotionID); err != nil {
		return nil, err
	}
	// Dual control: approver must differ from proposer. Most
	// real deployments enforce this; we surface it as a 403 via
	// the api.ErrPromotionDualControl sentinel.
	if err := a.ensureNotProposer(userID, promotionID); err != nil {
		return nil, err
	}
	out, err := a.svc.Approve(context.Background(), promotionID, userID)
	if err != nil {
		return nil, translatePromotionError(err)
	}
	a.invalidate(fundID)
	return promotionToAPI(out), nil
}

func (a *promotionServiceAdapter) Reject(userID, fundID, promotionID, reason string) (*api.Promotion, error) {
	if err := a.authorize(userID, fundID); err != nil {
		return nil, err
	}
	if err := a.ensureBelongs(fundID, promotionID); err != nil {
		return nil, err
	}
	out, err := a.svc.Reject(context.Background(), promotionID, userID, reason)
	if err != nil {
		return nil, translatePromotionError(err)
	}
	a.invalidate(fundID)
	return promotionToAPI(out), nil
}

func (a *promotionServiceAdapter) Activate(userID, fundID, promotionID string) (*api.Promotion, error) {
	if err := a.authorize(userID, fundID); err != nil {
		return nil, err
	}
	if err := a.ensureBelongs(fundID, promotionID); err != nil {
		return nil, err
	}
	if err := a.ensureNotProposer(userID, promotionID); err != nil {
		return nil, err
	}
	out, err := a.svc.Activate(context.Background(), promotionID, userID)
	if err != nil {
		return nil, translatePromotionError(err)
	}
	a.invalidate(fundID)
	return promotionToAPI(out), nil
}

func (a *promotionServiceAdapter) Rollback(userID, fundID, promotionID, reason string) (*api.Promotion, error) {
	if err := a.authorize(userID, fundID); err != nil {
		return nil, err
	}
	if err := a.ensureBelongs(fundID, promotionID); err != nil {
		return nil, err
	}
	out, err := a.svc.Deactivate(context.Background(), promotionID, promotion.StatusRolledBack, userID, reason)
	if err != nil {
		return nil, translatePromotionError(err)
	}
	a.invalidate(fundID)
	return promotionToAPI(out), nil
}

func (a *promotionServiceAdapter) List(userID, fundID string, limit int) ([]*api.Promotion, error) {
	if err := a.authorize(userID, fundID); err != nil {
		return nil, err
	}
	rows, err := a.svc.List(context.Background(), fundID, limit)
	if err != nil {
		return nil, translatePromotionError(err)
	}
	out := make([]*api.Promotion, 0, len(rows))
	for _, r := range rows {
		out = append(out, promotionToAPI(r))
	}
	return out, nil
}

func (a *promotionServiceAdapter) Get(userID, fundID, promotionID string) (*api.PromotionDetail, error) {
	if err := a.authorize(userID, fundID); err != nil {
		return nil, err
	}
	p, err := a.svc.Get(context.Background(), promotionID)
	if err != nil {
		return nil, translatePromotionError(err)
	}
	if p.FundID != fundID {
		return nil, api.ErrPromotionNotFound
	}
	events, err := a.svc.Events(context.Background(), promotionID)
	if err != nil {
		return nil, translatePromotionError(err)
	}
	diffs, err := a.shadow.ListRecent(context.Background(), promotionID, 30)
	if err != nil {
		return nil, translatePromotionError(err)
	}
	health, err := a.decay.ListHealth(context.Background(), promotionID, 60)
	if err != nil {
		return nil, translatePromotionError(err)
	}
	ratio, samples, _ := a.shadow.AgreementRatio(context.Background(), promotionID, 30)

	return &api.PromotionDetail{
		Promotion:        *promotionToAPI(p),
		Events:           eventsToAPI(events),
		ShadowDiffs:      diffsToAPI(diffs),
		Health:           healthToAPI(health),
		AgreementRatio:   ratio,
		AgreementSamples: samples,
	}, nil
}

// --- internals ---

func (a *promotionServiceAdapter) authorize(userID, fundID string) error {
	if strings.TrimSpace(userID) == "" {
		return errors.New("promotion: userID required")
	}
	if a.fundRepo == nil {
		return nil
	}
	_, err := authorizeFundAccess(context.Background(), a.fundRepo, a.companyRepo, userID, fundID)
	return err
}

// ensureBelongs guards against cross-fund ID guessing. The
// promotion ID lives in a single global namespace (UUIDs) so we
// need to verify the row belongs to the path's fundID.
func (a *promotionServiceAdapter) ensureBelongs(fundID, promotionID string) error {
	p, err := a.svc.Get(context.Background(), promotionID)
	if err != nil {
		return translatePromotionError(err)
	}
	if p.FundID != fundID {
		return api.ErrPromotionNotFound
	}
	return nil
}

// ensureNotProposer enforces dual-control on approval / activate.
// We keep it here (not in the service) because the service stays
// generic; dual-control is a deployment-level policy.
func (a *promotionServiceAdapter) ensureNotProposer(userID, promotionID string) error {
	p, err := a.svc.Get(context.Background(), promotionID)
	if err != nil {
		return translatePromotionError(err)
	}
	if strings.EqualFold(p.ProposedBy, userID) {
		return fmt.Errorf("%w: approver %s == proposer", api.ErrPromotionDualControl, userID)
	}
	return nil
}

func (a *promotionServiceAdapter) invalidate(fundID string) {
	if a.resolver != nil {
		a.resolver.Invalidate(fundID)
	}
}

// lookupBacktest implements promotion.BacktestLookup against
// repository.BacktestRepo. Returns nil-with-nil-error for
// missing jobs so the service can map to ErrBasisNotEligible.
func (a *promotionServiceAdapter) lookupBacktest(ctx context.Context, jobID string) (*promotion.BacktestBasis, error) {
	if a.backtestRepo == nil {
		return nil, errors.New("backtest repo unavailable")
	}
	row, err := a.backtestRepo.GetJob(ctx, jobID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	out := &promotion.BacktestBasis{
		JobID:       row.ID,
		FundID:      row.FundID,
		Status:      row.Status,
		EngineKind:  row.EngineKind,
		TradeCount:  row.TradeCount,
		HasWalkForward: len(row.WalkForward) > 0 && string(row.WalkForward) != "null",
	}
	if row.CumulativeReturn.Valid {
		out.CumulativeReturn = row.CumulativeReturn.Float64
	}
	if row.AnnualizedReturn.Valid {
		out.AnnualizedReturn = row.AnnualizedReturn.Float64
	}
	if row.SharpeRatio.Valid {
		out.SharpeRatio = row.SharpeRatio.Float64
	}
	if row.Volatility.Valid {
		out.Volatility = row.Volatility.Float64
	}
	if row.MaxDrawdown.Valid {
		out.MaxDrawdown = row.MaxDrawdown.Float64
	}
	if row.WinRate.Valid {
		out.WinRate = row.WinRate.Float64
	}
	// OOS metrics live inside the WalkForward blob — extract
	// when present so the decay monitor gets the stricter
	// baseline.
	if out.HasWalkForward {
		var wf struct {
			OverallReturn float64 `json:"overallReturn"`
			OverallSharpe float64 `json:"overallSharpe"`
		}
		if err := json.Unmarshal(row.WalkForward, &wf); err == nil {
			r, s := wf.OverallReturn, wf.OverallSharpe
			out.OOSReturn = &r
			out.OOSSharpe = &s
		} else {
			slog.Debug("promotion: walk-forward blob unparsable", "jobID", jobID, "err", err)
		}
	}
	return out, nil
}

// --- translators ---

// translatePromotionError maps the promotion package's sentinel
// errors onto the api package's sentinels. Anything else falls
// through unchanged.
func translatePromotionError(err error) error {
	switch {
	case errors.Is(err, promotion.ErrPromotionNotFound):
		return api.ErrPromotionNotFound
	case errors.Is(err, promotion.ErrInvalidPromotion):
		return fmt.Errorf("%w: %v", api.ErrPromotionInvalid, err)
	case errors.Is(err, promotion.ErrBasisNotEligible):
		return fmt.Errorf("%w: %v", api.ErrPromotionBasisIneligible, err)
	case errors.Is(err, promotion.ErrIllegalTransition):
		return fmt.Errorf("%w: %v", api.ErrPromotionIllegalTransition, err)
	default:
		return err
	}
}

func promotionToAPI(p *promotion.Promotion) *api.Promotion {
	if p == nil {
		return nil
	}
	out := &api.Promotion{
		ID:                p.ID,
		FundID:            p.FundID,
		ProposedBy:        p.ProposedBy,
		BasisJobID:        p.BasisJobID,
		EngineKind:        p.EngineKind,
		EngineParams:      map[string]any(p.EngineParams),
		BaselineMetrics:   api.PromotionBaseline(p.BaselineMetrics),
		Status:            string(p.Status),
		ShadowDays:        p.ShadowDays,
		DecayRatio:        p.DecayRatio,
		ApprovedBy:        p.ApprovedBy,
		RejectedBy:        p.RejectedBy,
		RejectedReason:    p.RejectedReason,
		DeactivatedReason: p.DeactivatedReason,
		Notes:             p.Notes,
		CreatedAt:         p.CreatedAt,
		UpdatedAt:         p.UpdatedAt,
	}
	if !p.ApprovedAt.IsZero() {
		t := p.ApprovedAt
		out.ApprovedAt = &t
	}
	if !p.RejectedAt.IsZero() {
		t := p.RejectedAt
		out.RejectedAt = &t
	}
	if !p.ShadowStartedAt.IsZero() {
		t := p.ShadowStartedAt
		out.ShadowStartedAt = &t
	}
	if !p.ShadowCompletedAt.IsZero() {
		t := p.ShadowCompletedAt
		out.ShadowCompletedAt = &t
	}
	if !p.ActivatedAt.IsZero() {
		t := p.ActivatedAt
		out.ActivatedAt = &t
	}
	if !p.DeactivatedAt.IsZero() {
		t := p.DeactivatedAt
		out.DeactivatedAt = &t
	}
	return out
}

func eventsToAPI(events []*promotion.Event) []*api.PromotionEvent {
	out := make([]*api.PromotionEvent, 0, len(events))
	for _, e := range events {
		out = append(out, &api.PromotionEvent{
			ID:          e.ID,
			EventType:   string(e.EventType),
			ActorUserID: e.ActorUserID,
			Payload:     e.Payload,
			CreatedAt:   e.CreatedAt,
		})
	}
	return out
}

func diffsToAPI(diffs []*promotion.ShadowDiff) []*api.PromotionShadowDiff {
	out := make([]*api.PromotionShadowDiff, 0, len(diffs))
	for _, d := range diffs {
		out = append(out, &api.PromotionShadowDiff{
			ID:             d.ID,
			TradingDate:    d.TradingDate,
			ShadowDecision: d.ShadowDecision,
			ActiveDecision: d.ActiveDecision,
			Agreement:      d.Agreement,
			CreatedAt:      d.CreatedAt,
		})
	}
	return out
}

func healthToAPI(snaps []*promotion.HealthSnapshot) []*api.PromotionHealth {
	out := make([]*api.PromotionHealth, 0, len(snaps))
	for _, h := range snaps {
		out = append(out, &api.PromotionHealth{
			ID:                h.ID,
			SnapshotAt:        h.SnapshotAt,
			WindowDays:        h.WindowDays,
			ActualSharpe:      h.ActualSharpe,
			ActualReturn:      h.ActualReturn,
			ActualMaxDrawdown: h.ActualMaxDrawdown,
			ActualTradeCount:  h.ActualTradeCount,
			SharpeDecayRatio:  h.SharpeDecayRatio,
			DecayFlag:         h.DecayFlag,
			Notes:             h.Notes,
		})
	}
	return out
}
