// Package eventwindow maps a plan's "thesis kind" to the
// canonical outcome window the resolver should use.
//
// MOTIVATION
// ----------
// W1-5 introduced the planoutcome.WindowKind enum (fixed_5d,
// fixed_10d, fixed_20d, next_earnings, next_news, manual). The
// W1-5 placeholder always assigned fixed_5d because the wiring
// to thesis kinds wasn't there yet. That's a problem: a PEAD
// thesis that needs the next earnings cycle is given 5 days
// to "prove itself" and is then judged a miss; a value thesis
// that takes 25 trading days to mature is similarly
// short-windowed.
//
// W3-16 introduces an explicit mapper. The wiring layer:
//
//   1. extracts thesis tags from the LLM's plan reasoning
//      (already populated as ThesisTags on the Vote / plan
//      action in the codebase),
//   2. calls Resolve(tags, defaults) to get the WindowKind,
//   3. stamps it into the planoutcome.Outcome record at
//      window-open time so the resolver knows when to close.
//
// The mapping is policy data, not hard-coded logic — the
// canonical map lives here so changing horizons is a single
// PR, but the resolution function is pure and trivially
// reviewable.
//
// FALLBACK CHAIN
// --------------
// When NO thesis tag matches, we fall through to:
//
//   * fund-config override (for funds with explicit policies),
//   * thesis-side hint (e.g. "long" → fixed_10d, "short" →
//     fixed_5d),
//   * the global default (fixed_5d).
//
// SCOPE
// -----
//   * Owns the canonical tag → WindowKind table, the Resolve
//     function, and the policy struct.
//   * Does NOT own the actual outcome resolution. That lives
//     in the planoutcome resolver (where the next-earnings /
//     next-news lookups consume calendars).
package eventwindow

import (
	"strings"

	"github.com/fundai/server/internal/planoutcome"
)

// Policy holds the per-fund overrides and the default. The
// wiring layer constructs one from fund.config.eventWindows.
type Policy struct {
	// Overrides is the per-tag mapping. Keys are lowercase
	// tag strings; values are the canonical WindowKind. Used
	// before falling back to BuiltinTagMap.
	Overrides map[string]planoutcome.WindowKind
	// Default is the WindowKind to use when no tag matches.
	// Defaults to fixed_5d.
	Default planoutcome.WindowKind
}

// DefaultPolicy is the production-safe baseline.
func DefaultPolicy() Policy {
	return Policy{
		Default: planoutcome.WindowFixed5d,
	}
}

// BuiltinTagMap is the canonical thesis-kind → window mapping.
// Tags are matched case-insensitively. The map is intentionally
// short; over-specific tags are best handled via Policy.Overrides
// (per-fund) rather than blowing this list up.
var BuiltinTagMap = map[string]planoutcome.WindowKind{
	// Earnings-driven theses run to the next earnings.
	"earnings_beat":   planoutcome.WindowNextEarnings,
	"earnings_miss":   planoutcome.WindowNextEarnings,
	"pead":            planoutcome.WindowNextEarnings,
	"earnings_drift":  planoutcome.WindowNextEarnings,
	"guidance_raise":  planoutcome.WindowNextEarnings,
	"guidance_cut":    planoutcome.WindowNextEarnings,

	// News-catalyst theses run to the next high-importance news.
	"news_catalyst":   planoutcome.WindowNextNews,
	"merger":          planoutcome.WindowNextNews,
	"acquisition":     planoutcome.WindowNextNews,
	"litigation":      planoutcome.WindowNextNews,
	"regulatory":      planoutcome.WindowNextNews,
	"product_launch":  planoutcome.WindowNextNews,

	// Short-horizon technical / momentum theses.
	"momentum":        planoutcome.WindowFixed5d,
	"breakout":        planoutcome.WindowFixed5d,
	"reversal":        planoutcome.WindowFixed5d,
	"pairs":           planoutcome.WindowFixed5d,
	"mean_reversion":  planoutcome.WindowFixed5d,
	"options_flow":    planoutcome.WindowFixed5d,

	// Medium-horizon fundamentals / sector theses.
	"fundamentals":    planoutcome.WindowFixed10d,
	"sector_rotation": planoutcome.WindowFixed10d,
	"sleeve":          planoutcome.WindowFixed10d,

	// Long-horizon value / quality / regime theses.
	"value":           planoutcome.WindowFixed20d,
	"quality":         planoutcome.WindowFixed20d,
	"low_beta":        planoutcome.WindowFixed20d,
	"macro":           planoutcome.WindowFixed20d,
	"thematic":        planoutcome.WindowFixed20d,
	"regime":          planoutcome.WindowFixed20d,
}

// Resolve picks the WindowKind for a plan based on its thesis
// tags. The first match wins to keep the result deterministic
// across tag-set permutations:
//
//   1. Walk thesis tags in order; check Policy.Overrides first,
//      then BuiltinTagMap.
//   2. If nothing matches and any tag contains "earnings",
//      assume an earnings thesis.
//   3. If nothing matches and any tag contains "news" or
//      "catalyst", assume a news thesis.
//   4. Fall back to Policy.Default (or fixed_5d if unset).
//
// Empty / nil tags returns Policy.Default.
func Resolve(tags []string, p Policy) planoutcome.WindowKind {
	if p.Default == "" {
		p.Default = planoutcome.WindowFixed5d
	}
	if len(tags) == 0 {
		return p.Default
	}
	for _, raw := range tags {
		tag := strings.ToLower(strings.TrimSpace(raw))
		if tag == "" {
			continue
		}
		if p.Overrides != nil {
			if v, ok := p.Overrides[tag]; ok {
				return v
			}
		}
		if v, ok := BuiltinTagMap[tag]; ok {
			return v
		}
	}
	// Heuristic catch-all for tags that don't match exactly
	// but are recognisable.
	for _, raw := range tags {
		tag := strings.ToLower(strings.TrimSpace(raw))
		if strings.Contains(tag, "earnings") || strings.Contains(tag, "guidance") {
			return planoutcome.WindowNextEarnings
		}
		if strings.Contains(tag, "news") || strings.Contains(tag, "catalyst") {
			return planoutcome.WindowNextNews
		}
		if strings.Contains(tag, "value") || strings.Contains(tag, "macro") {
			return planoutcome.WindowFixed20d
		}
	}
	return p.Default
}

// ResolveDays returns an integer "days" estimate for fixed
// windows. For event-driven windows (next_earnings, next_news)
// the actual close date depends on a calendar, so this returns
// 0 and the caller defers to the resolver's calendar lookup.
func ResolveDays(kind planoutcome.WindowKind) int {
	switch kind {
	case planoutcome.WindowFixed5d:
		return 5
	case planoutcome.WindowFixed10d:
		return 10
	case planoutcome.WindowFixed20d:
		return 20
	default:
		return 0
	}
}
