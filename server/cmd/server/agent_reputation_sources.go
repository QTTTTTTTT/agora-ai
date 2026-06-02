// agent_reputation_sources.go — adapters that translate the
// analystreport / debaterepo row shapes into the minimal
// projections the agentreputation backfill driver expects.
//
// Lives in cmd/server (not the agentreputation package itself)
// so the agentreputation package stays free of dependencies on
// analystreport / debaterepo — that keeps it cycle-safe and
// easy to swap out for a backtest-only data source in tests.

package main

import (
	"context"
	"strings"
	"time"

	"github.com/fundai/server/internal/agentreputation"
	"github.com/fundai/server/internal/analystreport"
	"github.com/fundai/server/internal/debaterepo"
)

// analystPanelSource wraps *analystreport.Repo and projects
// panels with their child reports into the minimal
// PanelRow / PanelReportRow shape the backfill driver consumes.
type analystPanelSource struct {
	repo *analystreport.Repo
}

func newAnalystPanelSource(repo *analystreport.Repo) *analystPanelSource {
	if repo == nil {
		return nil
	}
	return &analystPanelSource{repo: repo}
}

// ListPanelsForBackfill satisfies agentreputation.PanelSource.
// Always asks the underlying repo for child reports — the
// backfill driver needs every per-agent vote.
func (s *analystPanelSource) ListPanelsForBackfill(ctx context.Context, fundID string, since time.Time, limit int) ([]agentreputation.PanelRow, error) {
	if s == nil || s.repo == nil {
		return nil, nil
	}
	rows, err := s.repo.ListPanels(ctx, analystreport.ListPanelsParams{
		FundID:          strings.TrimSpace(fundID),
		AsOfFrom:        since,
		Limit:           limit,
		IncludeChildren: true,
	})
	if err != nil {
		return nil, err
	}
	out := make([]agentreputation.PanelRow, 0, len(rows))
	for _, p := range rows {
		pr := agentreputation.PanelRow{
			ID:     p.ID,
			FundID: p.FundID,
			Symbol: p.Symbol,
			AsOf:   p.AsOf,
		}
		for _, child := range p.Reports {
			pr.Reports = append(pr.Reports, agentreputation.PanelReportRow{
				AgentID:    child.AgentID,
				AgentName:  child.AgentName,
				Category:   child.Category,
				Direction:  child.Direction,
				Confidence: child.Confidence,
			})
		}
		out = append(out, pr)
	}
	return out, nil
}

// debateTranscriptSource wraps *debaterepo.Repo and projects
// transcripts + their child arguments into the minimal
// DebateRow / DebateArgumentRow shape the backfill driver
// consumes.
type debateTranscriptSource struct {
	repo *debaterepo.Repo
}

func newDebateTranscriptSource(repo *debaterepo.Repo) *debateTranscriptSource {
	if repo == nil {
		return nil
	}
	return &debateTranscriptSource{repo: repo}
}

// ListDebatesForBackfill satisfies agentreputation.DebateSource.
// Issues one ListTranscripts + per-transcript GetTranscript so
// children come along. The N+1 is acceptable because the loop
// is bounded by Limit and runs nightly.
func (s *debateTranscriptSource) ListDebatesForBackfill(ctx context.Context, fundID string, since time.Time, limit int) ([]agentreputation.DebateRow, error) {
	if s == nil || s.repo == nil {
		return nil, nil
	}
	rows, err := s.repo.ListTranscripts(ctx, debaterepo.ListTranscriptsParams{
		FundID:   strings.TrimSpace(fundID),
		AsOfFrom: since,
		Limit:    limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]agentreputation.DebateRow, 0, len(rows))
	for _, t := range rows {
		full, gerr := s.repo.GetTranscript(ctx, t.ID)
		if gerr != nil {
			continue
		}
		dr := agentreputation.DebateRow{
			ID:     full.ID,
			FundID: full.FundID,
			Symbol: full.Symbol,
			AsOf:   full.AsOf,
		}
		for _, a := range full.Arguments {
			dr.Arguments = append(dr.Arguments, agentreputation.DebateArgumentRow{
				AgentID:    a.AgentID,
				AgentName:  a.AgentName,
				Stance:     a.Stance,
				Round:      a.RoundNumber,
				Direction:  a.Direction,
				Confidence: a.Confidence,
			})
		}
		out = append(out, dr)
	}
	return out, nil
}
