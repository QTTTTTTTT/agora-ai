// context.go — render the PM-facing alpha context block.
//
// The output is a tightly-budgeted markdown string the PM
// prompt builder splices into PlanInput.MemoryContext (or
// any equivalent seam). It carries two things:
//
//   1. Agent Track Record — the top-K and bottom-K agents by
//      avg α for the fund (from agent_reputation_stats). This
//      tells the LLM "this analyst has been right 70% of the
//      time on tech; that one is a coin flip".
//
//   2. Alpha-tagged lessons — the K most-recent alpha-tagged
//      memory rows (from this package's repo). This tells the
//      LLM "the last 3 BUY AAPL calls by fundamentals_analyst
//      lost an aggregate -2.6% vs SPX".
//
// The builder takes a *agentreputation.Repo (for the
// leaderboard) and a *alphalesson.Repo (for the memory rows)
// so callers don't have to wire two readers themselves.

package alphalesson

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/fundai/server/internal/agentreputation"
)

// ContextOptions tunes BuildContext.
type ContextOptions struct {
	// TopAgents is how many leaderboard rows to render at the
	// top end (best α). Defaults to 5.
	TopAgents int
	// BottomAgents is how many bottom-end rows to render
	// (worst α). Defaults to 3.
	BottomAgents int
	// MaxLessons is how many alpha-tagged memory rows to
	// render. Defaults to 8.
	MaxLessons int
	// MinDecisions is the minimum decisions_count an agent
	// must have to qualify for the leaderboard. Defaults to 5.
	MinDecisions int64
	// SectionHeading is the top-level markdown heading.
	// Defaults to "## Agent Track Record & Alpha Lessons".
	SectionHeading string

	// TeamProvider, when non-nil, supplies the cross-fund
	// retrieval params at render time. The callback is invoked
	// exactly once with the querying fundID and must return:
	//   - The UUIDs of agents currently on the fund's team
	//     (typically the active rows of fund_team_members).
	//     An empty slice disables the cross-fund branch.
	//   - The CurrentRegime label (e.g. "trend_up") so the AP5
	//     regime gate can suppress regime-mismatched lessons.
	//     Empty string disables the gate.
	//   - The OptedOut bool that mirrors
	//     fund.config.allow_agent_portable_imports=false. When
	//     true the cross-fund branch is hard-disabled even
	//     with a populated team list.
	//
	// When nil, BuildContext skips all three lookups and
	// ListLessons is called with the fund-only legacy
	// parameter set — no behavioural change for pre-AP8
	// callers.
	//
	// The two-stage shape (ContextOptions field, not
	// BuildContext parameter) keeps the public BuildContext
	// signature stable while letting each caller wire its own
	// data source (FundRepo, regime classifier, fund config
	// reader) without dragging those types into this package.
	TeamProvider func(ctx context.Context, fundID string) (teamAgentIDs []string, currentRegime string, optedOut bool)

	// InheritedLabel customises the marker rendered next to
	// each lesson that came in via the cross-fund branch
	// (LessonRow.InheritedFromOtherFund=true). Defaults to
	// " [inherited]". Localised callers (CN PMs) might pass
	// " [继承自其他基金]".
	InheritedLabel string
}

func (o ContextOptions) normalize() ContextOptions {
	if o.TopAgents <= 0 {
		o.TopAgents = 5
	}
	if o.BottomAgents < 0 {
		o.BottomAgents = 0
	}
	if o.BottomAgents == 0 {
		o.BottomAgents = 3
	}
	if o.MaxLessons <= 0 {
		o.MaxLessons = 8
	}
	if o.MinDecisions <= 0 {
		o.MinDecisions = 5
	}
	if strings.TrimSpace(o.SectionHeading) == "" {
		o.SectionHeading = "## Agent Track Record & Alpha Lessons"
	}
	if strings.TrimSpace(o.InheritedLabel) == "" {
		o.InheritedLabel = " [inherited]"
	}
	return o
}

// BuildContext returns the PM-ready markdown block. Returns an
// empty string when there's nothing useful to say (no stats and
// no lessons) so the PM prompt stays clean.
func BuildContext(
	ctx context.Context,
	statsRepo *agentreputation.Repo,
	lessonRepo *Repo,
	fundID string,
	opts ContextOptions,
) (string, error) {
	if strings.TrimSpace(fundID) == "" {
		return "", fmt.Errorf("alphalesson: fundID required")
	}
	opts = opts.normalize()

	var sb strings.Builder

	var stats []agentreputation.Stats
	if statsRepo != nil {
		s, err := statsRepo.ListStats(ctx, agentreputation.ListStatsParams{
			FundID: fundID,
			Limit:  200,
		})
		if err == nil {
			stats = s
		}
	}
	top, bottom := splitTopBottom(stats, opts.TopAgents, opts.BottomAgents, opts.MinDecisions)
	if len(top) > 0 || len(bottom) > 0 {
		sb.WriteString(opts.SectionHeading)
		sb.WriteString("\n")
		if len(top) > 0 {
			sb.WriteString("### Top by avg α\n")
			for _, s := range top {
				fmt.Fprintf(&sb, "- %s (%s/%s): %d calls · hit_rate=%.0f%% · avg α=%+.2f%% · last %s\n",
					displayAgentLabel(s), s.AgentKind, s.Category,
					s.DecisionsCount, s.HitRate()*100, s.AvgAlpha*100,
					formatLastDecision(s))
			}
		}
		if len(bottom) > 0 {
			sb.WriteString("### Bottom by avg α (discount these)\n")
			for _, s := range bottom {
				fmt.Fprintf(&sb, "- %s (%s/%s): %d calls · hit_rate=%.0f%% · avg α=%+.2f%% · last %s\n",
					displayAgentLabel(s), s.AgentKind, s.Category,
					s.DecisionsCount, s.HitRate()*100, s.AvgAlpha*100,
					formatLastDecision(s))
			}
		}
	}

	var lessons []LessonRow
	if lessonRepo != nil {
		params := ListLessonsParams{
			FundID: fundID,
			Limit:  opts.MaxLessons,
		}
		// AP8 wiring: when the caller supplied a
		// TeamProvider, resolve cross-fund params and pass
		// them down to ListLessons. Errors / nils degrade
		// gracefully — a nil provider OR an empty team list
		// silently collapses to the legacy fund-only path.
		if opts.TeamProvider != nil {
			team, regime, optedOut := opts.TeamProvider(ctx, fundID)
			params.TeamAgentIDs = team
			params.CurrentRegime = regime
			params.ExplicitlyOptedOut = optedOut
		}
		l, err := lessonRepo.ListLessons(ctx, params)
		if err == nil {
			lessons = l
		}
	}
	if len(lessons) > 0 {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("### Recent alpha-tagged lessons\n")
		for _, l := range lessons {
			alpha := 0.0
			if l.AlphaVsBench.Valid {
				alpha = l.AlphaVsBench.Float64
			}
			title := l.Title.String
			if !l.Title.Valid || strings.TrimSpace(title) == "" {
				title = l.Content
			}
			// AP8 surface: mark inherited lessons so the
			// LLM (and any human reading the decision log)
			// can see at a glance which lessons came in
			// from another fund's history rather than being
			// learned at this fund. Default suffix is
			// " [inherited]" — operators can localise via
			// ContextOptions.InheritedLabel.
			suffix := ""
			if l.InheritedFromOtherFund {
				suffix = opts.InheritedLabel
			}
			fmt.Fprintf(&sb, "- %s [α=%+.2f%%]%s\n", oneLine(title), alpha*100, suffix)
		}
	}

	return sb.String(), nil
}

// --- helpers --------------------------------------------------------------

// splitTopBottom picks the topN and bottomN stats rows by
// avg_alpha, filtering out anyone below minDecisions. The
// returned slices never overlap (avoids the degenerate
// "leaderboard has 2 rows, show top 5 + bottom 3 = 8 copies of
// the same row").
func splitTopBottom(stats []agentreputation.Stats, topN, bottomN int, minDecisions int64) ([]agentreputation.Stats, []agentreputation.Stats) {
	if len(stats) == 0 {
		return nil, nil
	}
	filtered := make([]agentreputation.Stats, 0, len(stats))
	for _, s := range stats {
		if s.DecisionsCount >= minDecisions {
			filtered = append(filtered, s)
		}
	}
	if len(filtered) == 0 {
		return nil, nil
	}
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].AvgAlpha > filtered[j].AvgAlpha
	})
	if topN > len(filtered) {
		topN = len(filtered)
	}
	top := filtered[:topN]
	rest := filtered[topN:]
	if bottomN > len(rest) {
		bottomN = len(rest)
	}
	if bottomN == 0 {
		return top, nil
	}
	bottom := rest[len(rest)-bottomN:]
	// Bottom should be ordered worst-first for the operator.
	sort.Slice(bottom, func(i, j int) bool {
		return bottom[i].AvgAlpha < bottom[j].AvgAlpha
	})
	return top, bottom
}

func displayAgentLabel(s agentreputation.Stats) string {
	if strings.TrimSpace(s.AgentName) != "" {
		return s.AgentName
	}
	return s.AgentID
}

func formatLastDecision(s agentreputation.Stats) string {
	if !s.LastDecisionAt.Valid {
		return "—"
	}
	return s.LastDecisionAt.Time.UTC().Format("2006-01-02")
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	if idx := strings.IndexAny(s, "\n\r"); idx >= 0 {
		s = s[:idx]
	}
	const maxRunes = 160
	runes := []rune(s)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "…"
	}
	return s
}
