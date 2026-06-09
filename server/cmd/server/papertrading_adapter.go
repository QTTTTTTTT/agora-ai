package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/papertrading"
	"github.com/fundai/server/internal/repository"
)

// paperTradingAdapter is the wiring shim between the HTTP layer
// and the internal/papertrading service.
//
// Auth note: the Stage-4 MVP scope says "operator-side; one
// account manages many public portfolios". userID is currently
// only stamped at the API layer for logging — the service itself
// doesn't gate by user. When per-user portfolios land, gate
// inside the service.
type paperTradingAdapter struct {
	svc *papertrading.Service
}

func newPaperTradingAdapter(db *sql.DB) *paperTradingAdapter {
	if db == nil {
		return nil
	}
	repo := repository.NewPaperTradingRepo(db)
	svc := papertrading.New(repo, papertrading.StubOTSClient{}, nil)
	return &paperTradingAdapter{svc: svc}
}

func (a *paperTradingAdapter) CreatePortfolio(_ string, input api.PaperPortfolioInput) (*api.PaperPortfolioView, error) {
	if a == nil || a.svc == nil {
		return nil, api.ErrPaperTradingUnconfigured
	}
	p, err := a.svc.CreatePortfolio(context.Background(), papertrading.CreatePortfolioInput{
		Name:            input.Name,
		Strategy:        input.Strategy,
		Market:          input.Market,
		BenchmarkSymbol: input.BenchmarkSymbol,
		InitialCapital:  input.InitialCapital,
	})
	if err != nil {
		return nil, err
	}
	return projectPaperPortfolio(p), nil
}

func (a *paperTradingAdapter) ListPortfolios(_ string) ([]*api.PaperPortfolioView, error) {
	if a == nil || a.svc == nil {
		return nil, api.ErrPaperTradingUnconfigured
	}
	ps, err := a.svc.ListPortfolios(context.Background())
	if err != nil {
		return nil, err
	}
	out := make([]*api.PaperPortfolioView, 0, len(ps))
	for _, p := range ps {
		out = append(out, projectPaperPortfolio(p))
	}
	return out, nil
}

func (a *paperTradingAdapter) GetPortfolio(_ string, portfolioID string) (*api.PaperPortfolioView, error) {
	if a == nil || a.svc == nil {
		return nil, api.ErrPaperTradingUnconfigured
	}
	p, err := a.svc.GetPortfolio(context.Background(), portfolioID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, nil
	}
	return projectPaperPortfolio(p), nil
}

func (a *paperTradingAdapter) ProposeOrder(_ string, input api.ProposeOrderAPIInput) (*api.PaperOrderView, error) {
	if a == nil || a.svc == nil {
		return nil, api.ErrPaperTradingUnconfigured
	}
	o, err := a.svc.ProposeOrder(context.Background(), papertrading.ProposeOrderInput{
		PortfolioID:  input.PortfolioID,
		Symbol:       input.Symbol,
		Action:       input.Action,
		TargetWeight: input.TargetWeight,
		SharesChange: input.SharesChange,
		DecidedPrice: input.DecidedPrice,
		AIReasoning:  input.AIReasoning,
	})
	if err != nil {
		return nil, err
	}
	return projectPaperOrder(o), nil
}

func (a *paperTradingAdapter) ListOrders(_ string, portfolioID string, limit int) ([]*api.PaperOrderView, error) {
	if a == nil || a.svc == nil {
		return nil, api.ErrPaperTradingUnconfigured
	}
	os, err := a.svc.ListOrders(context.Background(), portfolioID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]*api.PaperOrderView, 0, len(os))
	for _, o := range os {
		out = append(out, projectPaperOrder(o))
	}
	return out, nil
}

func (a *paperTradingAdapter) NavHistory(_ string, portfolioID string) ([]api.PaperNavPointView, error) {
	if a == nil || a.svc == nil {
		return nil, api.ErrPaperTradingUnconfigured
	}
	rows, err := a.svc.NavHistory(context.Background(), portfolioID)
	if err != nil {
		return nil, err
	}
	out := make([]api.PaperNavPointView, 0, len(rows))
	for _, r := range rows {
		out = append(out, api.PaperNavPointView{
			Date:         r.Date,
			Nav:          r.Nav,
			DailyReturn:  r.DailyReturn,
			BenchmarkNav: r.BenchmarkNav,
		})
	}
	return out, nil
}

func (a *paperTradingAdapter) SnapshotNAV(_ string, input api.SnapshotNAVAPIInput) error {
	if a == nil || a.svc == nil {
		return api.ErrPaperTradingUnconfigured
	}
	date, err := parsePaperDate(input.SnapshotDate)
	if err != nil {
		return err
	}
	holdings := make(map[string]papertrading.HoldingPosition, len(input.Holdings))
	for sym, h := range input.Holdings {
		holdings[sym] = papertrading.HoldingPosition{
			Shares:      h.Shares,
			MarketValue: h.MarketValue,
			Weight:      h.Weight,
		}
	}
	return a.svc.SnapshotNAV(context.Background(), papertrading.SnapshotNAVInput{
		PortfolioID:  input.PortfolioID,
		SnapshotDate: date,
		Nav:          input.Nav,
		DailyReturn:  input.DailyReturn,
		BenchmarkNav: input.BenchmarkNav,
		CashBalance:  input.CashBalance,
		Holdings:     holdings,
	})
}

// helpers -------------------------------------------------------------------

func parsePaperDate(s string) (time.Time, error) {
	if s == "" {
		return time.Now().UTC(), nil
	}
	// Accept YYYY-MM-DD or RFC3339.
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid snapshotDate %q (want YYYY-MM-DD or RFC3339)", s)
}

func projectPaperPortfolio(p *papertrading.Portfolio) *api.PaperPortfolioView {
	if p == nil {
		return nil
	}
	return &api.PaperPortfolioView{
		ID:              p.ID,
		Name:            p.Name,
		Strategy:        p.Strategy,
		Market:          p.Market,
		BenchmarkSymbol: p.BenchmarkSymbol,
		InitialCapital:  p.InitialCapital,
		CurrentNav:      p.CurrentNav,
		CashBalance:     p.CashBalance,
		CreatedAt:       p.CreatedAt,
		LastRebalanceAt: p.LastRebalanceAt,
	}
}

func projectPaperOrder(o *papertrading.Order) *api.PaperOrderView {
	if o == nil {
		return nil
	}
	return &api.PaperOrderView{
		ID:               o.ID,
		PortfolioID:      o.PortfolioID,
		Symbol:           o.Symbol,
		Action:           o.Action,
		TargetWeight:     o.TargetWeight,
		SharesChange:     o.SharesChange,
		DecidedAt:        o.DecidedAt,
		DecidedPrice:     o.DecidedPrice,
		ExecutedAt:       o.ExecutedAt,
		ExecutedPrice:    o.ExecutedPrice,
		AIReasoning:      o.AIReasoning,
		HashSignature:    o.HashSignature,
		CanonicalPayload: o.CanonicalPayload,
		PublicProofURL:   o.PublicProofURL,
		OTSStatus:        o.OTSStatus,
	}
}
