package attribution

import (
	"sort"
	"strings"

	"github.com/fundai/server/internal/repository"
)

// ---------------------------------------------------------------------------
// Pure aggregation helpers
// ---------------------------------------------------------------------------

// SleeveRegimeKey indexes a single cell of the cross-tab. The
// lesson generator and any future "merge stats from multiple
// funds" path use this to dedupe cells.
type SleeveRegimeKey struct {
	Sleeve string
	Regime string
}

// NormalizeKey lower-cases / trims a key, mapping NULLs (empty
// strings from the SQL fold) to the literal "(unspecified)" so
// dashboards and lesson text can show something readable instead
// of disappearing rows.
func NormalizeKey(k SleeveRegimeKey) SleeveRegimeKey {
	out := SleeveRegimeKey{
		Sleeve: strings.ToLower(strings.TrimSpace(k.Sleeve)),
		Regime: strings.ToLower(strings.TrimSpace(k.Regime)),
	}
	if out.Sleeve == "" {
		out.Sleeve = "(unspecified)"
	}
	if out.Regime == "" {
		out.Regime = "(unspecified)"
	}
	return out
}

// IndexSleeveRegime turns a flat slice into a map keyed by the
// normalised (sleeve, regime) pair. Repeated keys are merged
// rather than overwriting — the rare cousin of "same row twice"
// is real when the SQL layer returns separate cells for
// upper/lower-case dimension labels.
func IndexSleeveRegime(stats []repository.SleeveRegimeStat) map[SleeveRegimeKey]repository.SleeveRegimeStat {
	out := make(map[SleeveRegimeKey]repository.SleeveRegimeStat, len(stats))
	for _, s := range stats {
		k := NormalizeKey(SleeveRegimeKey{Sleeve: s.Sleeve, Regime: s.Regime})
		if existing, ok := out[k]; ok {
			out[k] = mergeSleeveRegime(existing, s, k)
			continue
		}
		// Stamp the normalised labels onto the kept row so the
		// caller sees "(unspecified)" rather than "" when the
		// source had a NULL.
		s.Sleeve = k.Sleeve
		s.Regime = k.Regime
		out[k] = s
	}
	return out
}

func mergeSleeveRegime(a, b repository.SleeveRegimeStat, k SleeveRegimeKey) repository.SleeveRegimeStat {
	merged := repository.SleeveRegimeStat{
		Sleeve:     k.Sleeve,
		Regime:     k.Regime,
		TradeCount: a.TradeCount + b.TradeCount,
		WinCount:   a.WinCount + b.WinCount,
		LossCount:  a.LossCount + b.LossCount,
		TotalPnL:   a.TotalPnL + b.TotalPnL,
	}
	// Weighted averages by trade count. Falls back to a simple
	// mean when both sides have zero trades (defensive — the
	// caller shouldn't hand us empty rows, but the upstream is
	// SQL, not Go, and we want one fewer panic surface).
	totalTrades := merged.TradeCount
	if totalTrades > 0 {
		merged.AvgPnLPct = (a.AvgPnLPct*float64(a.TradeCount) + b.AvgPnLPct*float64(b.TradeCount)) / float64(totalTrades)
		merged.AvgHoldingDays = (a.AvgHoldingDays*float64(a.TradeCount) + b.AvgHoldingDays*float64(b.TradeCount)) / float64(totalTrades)
		merged.WinRate = float64(merged.WinCount) / float64(totalTrades)
	}
	return merged
}

// SortedSleeveRegime returns the flattened map back as a slice
// sorted by (sleeve, regime) ASC. Deterministic order is the
// only thing that lets the lesson generator dedupe across runs.
func SortedSleeveRegime(stats map[SleeveRegimeKey]repository.SleeveRegimeStat) []repository.SleeveRegimeStat {
	out := make([]repository.SleeveRegimeStat, 0, len(stats))
	for _, s := range stats {
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Sleeve != out[j].Sleeve {
			return out[i].Sleeve < out[j].Sleeve
		}
		return out[i].Regime < out[j].Regime
	})
	return out
}
