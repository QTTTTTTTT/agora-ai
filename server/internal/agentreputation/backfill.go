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

// AlphaForDirection converts (realisedReturn, benchmarkReturn,
// direction) into the agent's "skill alpha" — the excess return
// the fund would have earned by acting on the agent's call versus
// the benchmark hold.
//
//   - bullish: long the symbol vs long the benchmark.
//     alpha = realised - benchmark.
//   - bearish: short the symbol vs long the benchmark. A 5%
//     drop in the symbol with a flat benchmark is +5% credit
//     to the bearish caller, so alpha = -realised - benchmark
//     (equivalently, benchmarkLong + symbolShort excess return).
//   - neutral: the agent declined to commit; no position would
//     have been opened, so alpha is zero. The hits/misses
//     aggregation already excludes neutrals from both buckets;
//     this keeps avg_alpha consistent with that semantic.
//
// W16-2 audit: previously the backfill emitted alpha = realised -
// benchmark for every direction including bearish, which made a
// correct bearish call (e.g. symbol -5%, benchmark 0%) emit
// alpha = -5%. The leaderboard ORDER BY avg_alpha DESC then
// ranked accurate bearish researchers at the BOTTOM and the
// alpha-tagged lesson formatter ("lesson: positive alpha →
// keep weight on this agent's calls") never fired for them. The
// direction-aware mapping below restores the intended skill
// reading without changing realised_return / benchmark_return on
// the row (those stay as raw observed numbers for the audit log).
func AlphaForDirection(direction Direction, realised, benchmark float64) float64 {
	switch direction {
	case DirBullish:
		return realised - benchmark
	case DirBearish:
		return -realised - benchmark
	}
	return 0
}

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

// LessonWriter is the S9.1 hook the backfill driver calls after
// it has persisted outcomes. The alphalesson package satisfies
// it via Repo.WriteAlphaLessons. Optional — when nil, no
// lessons are produced.
type LessonWriter interface {
	WriteAlphaLessonsForOutcomes(ctx context.Context, outcomes []Outcome) (int, error)
}

// Backfill is the orchestrator. Stateless after construction.
type Backfill struct {
	repo    *Repo
	panels  PanelSource
	debates DebateSource
	returns RealisedReturnFn
	lessons LessonWriter
}

// NewBackfill wires the driver. repo + returns are required;
// either panels or debates may be nil, in which case that
// source is skipped.
func NewBackfill(repo *Repo, panels PanelSource, debates DebateSource, returns RealisedReturnFn) *Backfill {
	return &Backfill{repo: repo, panels: panels, debates: debates, returns: returns}
}

// WithLessonWriter installs the S9.1 alpha-lesson sink. Returns
// the backfill so it can be chain-constructed.
func (b *Backfill) WithLessonWriter(w LessonWriter) *Backfill {
	if b != nil {
		b.lessons = w
	}
	return b
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
					dir := Direction(r.Direction)
					outs = append(outs, Outcome{
						FundID: fundID, AgentID: r.AgentID, AgentName: r.AgentName,
						AgentKind: KindAnalyst, Category: r.Category,
						Symbol: p.Symbol, AsOf: p.AsOf,
						Direction: dir, Confidence: r.Confidence,
						RealisedReturn: realised, BenchmarkReturn: bench,
						Alpha:       AlphaForDirection(dir, realised, bench),
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
					dir := Direction(a.Direction)
					outs = append(outs, Outcome{
						FundID: fundID, AgentID: a.AgentID, AgentName: a.AgentName,
						AgentKind: KindAdvocate, Category: cat,
						Symbol: d.Symbol, AsOf: d.AsOf,
						Direction: dir, Confidence: a.Confidence,
						RealisedReturn: realised, BenchmarkReturn: bench,
						Alpha:       AlphaForDirection(dir, realised, bench),
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
	// S9.1 — best-effort alpha-tagged memory write. A lesson
	// writer failure must not roll back the upsert (the
	// reputation table is the source of truth; lessons are a
	// derived prompt aid). Log + continue.
	if b.lessons != nil {
		if _, err := b.lessons.WriteAlphaLessonsForOutcomes(ctx, outs); err != nil {
			return len(outs), fmt.Errorf("agentreputation: lesson writer: %w", err)
		}
	}
	return len(outs), nil
}
