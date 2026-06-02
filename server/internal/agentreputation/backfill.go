// backfill.go — S8.4 backfill driver. Reads recent
// AnalystPanelReports + DebateTranscripts + per-symbol
// realised returns and produces one Outcome row per
// (agent, symbol, asof, horizon).
//
// The driver is intentionally split from Repo so a deployment
// can swap the data sources (e.g. read from a backtest harness
// instead of live position changes) without touching the
// persistence layer.

package agentreputation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// RealisedReturnFn is the per-symbol return lookup the
// backfill driver depends on. asof is the date the agent made
// the call; horizonDays is the forward window (1, 5, or 21
// typically). The implementation should return the
// price-change fraction over the horizon and the matching
// benchmark return.
//
// Returning (0,0,nil) is valid — it just means "no realised
// data yet", and the outcome row is skipped.
type RealisedReturnFn func(ctx context.Context, fundID, symbol string, asof time.Time, horizonDays int) (realised, benchmark float64, ok bool, err error)

// PanelSource feeds the backfill with persisted panel reports.
type PanelSource interface {
	ListPanelsForBackfill(ctx context.Context, fundID string, since time.Time, limit int) ([]PanelRow, error)
}

// DebateSource feeds the backfill with persisted debate transcripts.
type DebateSource interface {
	ListDebatesForBackfill(ctx context.Context, fundID string, since time.Time, limit int) ([]DebateRow, error)
}

// PanelRow + PanelReportRow + DebateRow + DebateArgumentRow
// are the minimal projections the backfill driver needs from
// its source repos. Defined here (rather than imported) so
// agentreputation has no dependency on analystreport or
// debaterepo (avoids cycles with internal/agent).
type PanelRow struct {
	ID        string
	FundID    string
	Symbol    string
	AsOf      time.Time
	Reports   []PanelReportRow
}

type PanelReportRow struct {
	AgentID   string
	AgentName string
	Category  string
	Direction string
	Confidence int
}

type DebateRow struct {
	ID        string
	FundID    string
	Symbol    string
	AsOf      time.Time
	Arguments []DebateArgumentRow
}

type DebateArgumentRow struct {
	AgentID    string
	AgentName  string
	Stance     string
	Round      int
	Direction  string
	Confidence int
}

// BackfillConfig tunes the driver.
type BackfillConfig struct {
	// Horizons is the list of forward windows (in days) the
	// backfill produces outcomes for. Empty defaults to {1, 5}.
	Horizons []int

	// Since is the earliest asof the driver scans. Zero means
	// "all of it" — typical production value is 90 days.
	Since time.Time

	// Limit is the max number of panel / debate rows to scan
	// per pass. Zero means 500.
	Limit int
}

func (c BackfillConfig) horizons() []int {
	if len(c.Horizons) == 0 {
		return []int{1, 5}
	}
	return c.Horizons
}

func (c BackfillConfig) limit() int {
	if c.Limit <= 0 {
		return 500
	}
	return c.Limit
}

// Backfill is the orchestrator. Stateless after construction.
type Backfill struct {
	repo     *Repo
	panels   PanelSource
	debates  DebateSource
	returns  RealisedReturnFn
}

// NewBackfill wires the driver. repo + returns are required;
// either panels or debates may be nil, in which case that
// source is skipped.
func NewBackfill(repo *Repo, panels PanelSource, debates DebateSource, returns RealisedReturnFn) *Backfill {
	return &Backfill{repo: repo, panels: panels, debates: debates, returns: returns}
}

// Run produces outcomes for one fund and recomputes the
// rolling stats. Returns the number of outcome rows it wrote.
// It is safe to call repeatedly — the UpsertOutcomes path
// overwrites rows with the same key.
func (b *Backfill) Run(ctx context.Context, fundID string, cfg BackfillConfig) (int, error) {
	if b == nil {
		return 0, errors.New("agentreputation: nil backfill")
	}
	if b.repo == nil || b.returns == nil {
		return 0, errors.New("agentreputation: backfill missing repo or return fn")
	}
	if strings.TrimSpace(fundID) == "" {
		return 0, errors.New("agentreputation: backfill requires fundID")
	}
	var outs []Outcome

	if b.panels != nil {
		panels, err := b.panels.ListPanelsForBackfill(ctx, fundID, cfg.Since, cfg.limit())
		if err != nil {
			return 0, fmt.Errorf("agentreputation: panel source: %w", err)
		}
		for _, p := range panels {
			for _, r := range p.Reports {
				for _, h := range cfg.horizons() {
					realised, bench, ok, rerr := b.returns(ctx, fundID, p.Symbol, p.AsOf, h)
					if rerr != nil {
						return len(outs), fmt.Errorf("agentreputation: return fn: %w", rerr)
					}
					if !ok {
						continue
					}
					outs = append(outs, Outcome{
						FundID: fundID, AgentID: r.AgentID, AgentName: r.AgentName,
						AgentKind: KindAnalyst, Category: r.Category,
						Symbol: p.Symbol, AsOf: p.AsOf,
						Direction: Direction(r.Direction), Confidence: r.Confidence,
						RealisedReturn: realised, BenchmarkReturn: bench, Alpha: realised - bench,
						HorizonDays: h,
						SourcePanelID: sql.NullString{String: p.ID, Valid: p.ID != ""},
					})
				}
			}
		}
	}

	if b.debates != nil {
		debates, err := b.debates.ListDebatesForBackfill(ctx, fundID, cfg.Since, cfg.limit())
		if err != nil {
			return 0, fmt.Errorf("agentreputation: debate source: %w", err)
		}
		for _, d := range debates {
			for _, a := range d.Arguments {
				cat := a.Stance // "bull" or "bear"
				for _, h := range cfg.horizons() {
					realised, bench, ok, rerr := b.returns(ctx, fundID, d.Symbol, d.AsOf, h)
					if rerr != nil {
						return len(outs), fmt.Errorf("agentreputation: return fn: %w", rerr)
					}
					if !ok {
						continue
					}
					outs = append(outs, Outcome{
						FundID: fundID, AgentID: a.AgentID, AgentName: a.AgentName,
						AgentKind: KindAdvocate, Category: cat,
						Symbol: d.Symbol, AsOf: d.AsOf,
						Direction: Direction(a.Direction), Confidence: a.Confidence,
						RealisedReturn: realised, BenchmarkReturn: bench, Alpha: realised - bench,
						HorizonDays: h,
						SourceDebateID: sql.NullString{String: d.ID, Valid: d.ID != ""},
						Note:           fmt.Sprintf("round=%d", a.Round),
					})
				}
			}
		}
	}

	if len(outs) == 0 {
		return 0, nil
	}
	if err := b.repo.UpsertOutcomes(ctx, outs); err != nil {
		return 0, err
	}
	if err := b.repo.RecomputeStats(ctx, fundID); err != nil {
		return len(outs), fmt.Errorf("agentreputation: recompute: %w", err)
	}
	return len(outs), nil
}
