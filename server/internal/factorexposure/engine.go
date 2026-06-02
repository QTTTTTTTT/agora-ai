// engine.go — pure portfolio-level factor exposure computation.
//
// Convention. Given a slice of Holdings (positions in base
// currency, signed) and a lookup of instrument → factor → loading,
// the engine returns one PortfolioExposure row per canonical
// factor.
//
// Weight definition. weight_i = MV_i / sum(|MV_j|). We use gross
// market value as the denominator because:
//
//   - For long-only books gross == net, so the result is identical
//     to the textbook "fraction of book".
//   - For long-short books gross != net; using net would make
//     weights blow up (or go negative for short-heavy books) and
//     produce nonsensical exposure numbers. Using gross gives the
//     correct "fraction of capital at risk on this name".
//
// Coverage. A loading that's missing for a holding contributes
// zero to net + gross, but the holding's |weight| is excluded from
// CapitalPct. That way the UI can tell "0.4 net momentum exposure,
// but only 75% of book had loadings" — much more honest than
// silently treating missing == zero loading.
//
// Determinism. The output is sorted by canonical AllFactors order;
// inputs in any order produce the same output. The engine does no
// I/O, no clock reads, no logging.

package factorexposure

import (
	"math"
	"time"
)

// LoadingKey is the lookup key the Engine wants. The Repo layer
// returns a `map[LoadingKey]InstrumentLoading` after fetching the
// latest-as-of-T row for every (instrument, factor) pair the
// portfolio uses.
type LoadingKey struct {
	InstrumentKey string
	Factor        Factor
}

// Engine is a function-pointer-free struct so callers can override
// individual hooks in tests. Today the only knob is the clock
// (used to stamp Snapshot.GeneratedAt); future knobs may add
// per-factor reweighting or asset-class default scrubbing.
type Engine struct {
	// Now returns the time stamped on Snapshot.GeneratedAt. nil →
	// time.Now (UTC).
	Now func() time.Time
}

// nowFn returns the configured clock or time.Now in UTC.
func (e *Engine) nowFn() time.Time {
	if e == nil || e.Now == nil {
		return time.Now().UTC()
	}
	return e.Now().UTC()
}

// Compute produces the Snapshot for the given fund. The caller is
// responsible for handing in:
//
//   - holdings: every non-zero position the fund holds, expressed
//     in base currency, signed.
//   - loadings: a map keyed by (instrument_key, factor) → the
//     latest-as-of-T loading row. Missing keys are treated as
//     "no loading for this factor on this instrument" (skipped,
//     not zero).
//
// fundID is echoed back on the result so callers can persist
// without re-threading; an empty fundID is accepted (the snapshot
// will have an empty FundID and is still usable for ad-hoc
// preview calls).
func (e *Engine) Compute(fundID string, holdings []Holding, loadings map[LoadingKey]InstrumentLoading) Snapshot {
	snap := Snapshot{
		FundID:      fundID,
		GeneratedAt: e.nowFn(),
		Exposures:   make([]PortfolioExposure, 0, len(AllFactors)),
	}

	if len(holdings) == 0 {
		// Still emit one row per factor so the UI iterators don't
		// have to special-case empty funds; mark them as 0-coverage.
		for _, f := range AllFactors {
			snap.Exposures = append(snap.Exposures, PortfolioExposure{Factor: f})
		}
		return snap
	}

	// Total gross MV — denominator for weights. Holdings with zero
	// market value contribute nothing and are skipped.
	var grossMV float64
	for _, h := range holdings {
		grossMV += math.Abs(h.MarketValue)
	}
	snap.NAV = grossMV
	snap.HoldingsTotal = len(holdings)

	if grossMV <= 0 {
		// All zero-MV holdings (defensive). Emit blank rows.
		for _, f := range AllFactors {
			snap.Exposures = append(snap.Exposures, PortfolioExposure{Factor: f})
		}
		return snap
	}

	coveredOverall := make(map[string]struct{}, len(holdings))
	oldestAsof := time.Time{}

	for _, f := range AllFactors {
		row := PortfolioExposure{Factor: f}
		var net, gross, covered float64
		var newestAsof time.Time
		holdingCount := 0

		for _, h := range holdings {
			if math.Abs(h.MarketValue) <= 0 {
				continue
			}
			loading, ok := loadings[LoadingKey{InstrumentKey: h.InstrumentKey, Factor: f}]
			if !ok {
				continue
			}
			// Use signed weight so shorts subtract from net but add
			// to gross. weight is a real fraction in [-1, 1].
			w := h.MarketValue / grossMV
			contrib := w * loading.Loading
			net += contrib
			gross += math.Abs(contrib)
			covered += math.Abs(w)
			holdingCount++
			coveredOverall[h.InstrumentKey] = struct{}{}
			if loading.AsOf.After(newestAsof) {
				newestAsof = loading.AsOf
			}
		}
		row.NetExposure = net
		row.GrossExposure = gross
		row.CapitalPct = covered
		row.HoldingCount = holdingCount
		row.LoadingsAsOf = newestAsof
		snap.Exposures = append(snap.Exposures, row)

		// Track the oldest "newest-as-of" across factors — that's
		// the bottleneck the UI surfaces as "loadings X days stale".
		if !newestAsof.IsZero() {
			if oldestAsof.IsZero() || newestAsof.Before(oldestAsof) {
				oldestAsof = newestAsof
			}
		}
	}
	snap.HoldingsCovered = len(coveredOverall)
	snap.OldestLoadingAsOf = oldestAsof
	return snap
}
