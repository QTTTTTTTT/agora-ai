package benchmark

import (
	"errors"
	"sort"
	"time"
)

// Point is a single (date, normalized value) pair. Date is a calendar
// date in UTC truncated to midnight, so two upstreams that disagree
// by hours converge to the same key.
type Point struct {
	Date  time.Time
	Value float64
}

// ErrEmptySeries is returned when Normalize gets nothing to work on.
// Callers usually swallow this and skip rendering rather than
// surfacing a 5xx.
var ErrEmptySeries = errors.New("benchmark: empty series")

// Normalize takes a sequence of (date, raw value) bars and returns a
// new series rebased so the FIRST point is exactly 100. Subsequent
// values are scaled by `value / first`. This is the canonical
// "normalize to 100" operation chart libraries expect when overlaying
// dissimilar instruments (a $4000 SPX index next to a 0.99-NAV fund
// would otherwise render as a flat line vs a peak).
//
// Rules:
//
//   - The slice is copied; the input is not mutated.
//   - The slice is sorted ascending by Date so callers can pass
//     unsorted data without surprises.
//   - Zero or negative first values cause ErrEmptySeries — neither
//     a flat line at 100 nor an inversion is meaningful in those
//     cases, and we'd rather force the caller to inspect.
//   - NaN / non-finite values inside the series are left as-is; the
//     UI's chart already gaps over them, which is the right behaviour
//     for a missing trading day on a holiday.
//
// Output dates are returned UTC-truncated so the Web layer can
// compare them with fund NAV dates string-equal.
func Normalize(rawDates []time.Time, rawValues []float64) ([]Point, error) {
	if len(rawDates) == 0 || len(rawDates) != len(rawValues) {
		return nil, ErrEmptySeries
	}
	pairs := make([]Point, 0, len(rawDates))
	for i := range rawDates {
		pairs = append(pairs, Point{
			Date:  rawDates[i].UTC().Truncate(24 * time.Hour),
			Value: rawValues[i],
		})
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		return pairs[i].Date.Before(pairs[j].Date)
	})
	first := pairs[0].Value
	if first <= 0 {
		return nil, ErrEmptySeries
	}
	for i := range pairs {
		pairs[i].Value = pairs[i].Value / first * 100.0
	}
	return pairs, nil
}

// AlphaSpread computes a third series representing fund-vs-benchmark
// outperformance: fund.Value - benchmark.Value at each shared date.
//
// We DO NOT forward-fill: a benchmark date with no corresponding fund
// point is dropped. That matches what an investor expects — the gap
// week of a Chinese New Year (where the fund traded but the benchmark
// didn't, or vice versa) should be a missing point, not an
// "alpha = whatever the prior fund return was, frozen" line.
//
// A point's date is the calendar day; both sides must already have
// been Normalize'd so their Y-axis units match (both rebased to 100).
//
// Returns ErrEmptySeries when no shared dates exist.
func AlphaSpread(fund, benchmark []Point) ([]Point, error) {
	if len(fund) == 0 || len(benchmark) == 0 {
		return nil, ErrEmptySeries
	}
	idx := make(map[time.Time]float64, len(benchmark))
	for _, p := range benchmark {
		idx[p.Date] = p.Value
	}
	out := make([]Point, 0, len(fund))
	for _, fp := range fund {
		if bv, ok := idx[fp.Date]; ok {
			out = append(out, Point{
				Date:  fp.Date,
				Value: fp.Value - bv,
			})
		}
	}
	if len(out) == 0 {
		return nil, ErrEmptySeries
	}
	return out, nil
}
