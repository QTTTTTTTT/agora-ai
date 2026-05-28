// Package intraday builds per-symbol intraday OHLC signal snapshots
// the PM decision prompt can fold into the universe view.
//
// Sprint 3 / L1 motivation:
//
//   - Daily bars miss intraday-only regimes — gap up + reversal, opening
//     range break, late-day fade. These show up only at 5/15/60-min
//     resolution and historically caused entry timing errors that
//     daily bars could not surface.
//   - We don't want the PM to start chasing intraday noise; the snapshot
//     is a SOFT signal blended into the existing daily prompt.
//
// What's in the snapshot:
//
//   - Last 24 bars on the requested interval (default 5m), aligned to
//     market hours by the upstream provider.
//   - Three derived stats:
//   * trendDir ∈ {up, down, range}, based on (last_close vs sma8) and
//     (last_close vs first_close in window).
//   * volZScore: today's realised range vs a 20-day median range,
//     reported as a z-score capped to ±3.
//   * volRatio: last bar's volume vs the median bar volume in the
//     last 100 5m bars.
//
// We deliberately don't predict direction; the prompt reasons over the
// stats. Empty / partial data results in nil snapshot (caller renders
// nothing).
package intraday

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/fundai/server/internal/ohlc"
)

// IntradayInterval is the resolution caller requests; 5m default.
type IntradayInterval string

const (
	Interval5m  IntradayInterval = "5m"
	Interval15m IntradayInterval = "15m"
	Interval60m IntradayInterval = "60m"
)

// Snapshot is the per-symbol output the prompt consumes.
type Snapshot struct {
	Symbol         string    `json:"symbol"`
	Interval       string    `json:"interval"`
	Bars           int       `json:"bars"`
	TrendDirection string    `json:"trendDirection"` // up / down / range
	LastClose      float64   `json:"lastClose"`
	OpenClose      float64   `json:"openClose"` // first_close in window
	SMA8           float64   `json:"sma8"`
	VolZScore      float64   `json:"volZScore"`
	VolRatio       float64   `json:"volRatio"`
	AsOf           time.Time `json:"asOf"`
}

// Builder bundles the OHLC fetcher + interval choice. Sequential
// per-symbol fetch — the caller (PMAgent) already bounds candidate
// universes to <= 30, so fanning out parallel is unnecessary noise
// for upstream rate-limit budgets.
type Builder struct {
	Fetcher  ohlc.Fetcher
	Interval IntradayInterval
}

// NewBuilder returns a configured Builder. nil fetcher → nil Builder
// (caller's nil-safe Build returns empty).
func NewBuilder(fetcher ohlc.Fetcher, interval IntradayInterval) *Builder {
	if strings.TrimSpace(string(interval)) == "" {
		interval = Interval5m
	}
	return &Builder{Fetcher: fetcher, Interval: interval}
}

// Build runs one Fetch per symbol and returns the snapshot map. Missing
// data is silently skipped so the prompt block stays bounded; the map
// only carries successful entries.
func (b *Builder) Build(ctx context.Context, symbols []string, market string, asOf time.Time) []Snapshot {
	if b == nil || b.Fetcher == nil || len(symbols) == 0 {
		return nil
	}
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	out := make([]Snapshot, 0, len(symbols))
	for _, raw := range symbols {
		sym := strings.TrimSpace(raw)
		if sym == "" {
			continue
		}
		bars, err := b.Fetcher.Fetch(ctx, ohlc.FetchRequest{
			Symbol:    sym,
			Market:    market,
			Interval:  ohlc.Interval(b.Interval),
			LookbackN: 100,
			EndTime:   asOf,
		})
		if err != nil || len(bars) < 8 {
			continue
		}
		snap := buildSnapshot(sym, string(b.Interval), bars, asOf)
		if snap != nil {
			out = append(out, *snap)
		}
	}
	// Stable order: alphabetical by symbol. Avoids prompt churn between
	// runs even when fetcher response order varies.
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}

func buildSnapshot(symbol, interval string, bars []ohlc.Bar, asOf time.Time) *Snapshot {
	n := len(bars)
	if n < 8 {
		return nil
	}
	first := bars[0].Close
	last := bars[n-1].Close
	sma8 := mean(closes(bars[n-8 : n]))
	ranges := make([]float64, 0, n)
	vols := make([]float64, 0, n)
	for _, b := range bars {
		ranges = append(ranges, math.Max(0, b.High-b.Low))
		vols = append(vols, b.Volume)
	}
	rangeMed := median(ranges)
	rangeStd := stdev(ranges)
	todayRange := ranges[n-1]
	var z float64
	if rangeStd > 0 {
		z = (todayRange - rangeMed) / rangeStd
	}
	if z > 3 {
		z = 3
	}
	if z < -3 {
		z = -3
	}
	volMed := median(vols)
	var volRatio float64
	if volMed > 0 {
		volRatio = vols[n-1] / volMed
	}
	dir := classifyDirection(last, first, sma8)
	return &Snapshot{
		Symbol:         symbol,
		Interval:       interval,
		Bars:           n,
		TrendDirection: dir,
		LastClose:      last,
		OpenClose:      first,
		SMA8:           sma8,
		VolZScore:      z,
		VolRatio:       volRatio,
		AsOf:           asOf,
	}
}

func classifyDirection(last, first, sma8 float64) string {
	// 0.2% buffer to avoid flapping on flat tape.
	const eps = 0.002
	upFromOpen := (last-first)/first >= eps
	downFromOpen := (last-first)/first <= -eps
	aboveSMA := last >= sma8*(1+eps)
	belowSMA := last <= sma8*(1-eps)
	switch {
	case upFromOpen && aboveSMA:
		return "up"
	case downFromOpen && belowSMA:
		return "down"
	default:
		return "range"
	}
}

func closes(bars []ohlc.Bar) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		out[i] = b.Close
	}
	return out
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := make([]float64, len(xs))
	copy(cp, xs)
	sort.Float64s(cp)
	mid := len(cp) / 2
	if len(cp)%2 == 0 {
		return (cp[mid-1] + cp[mid]) / 2
	}
	return cp[mid]
}

func stdev(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	m := mean(xs)
	var ss float64
	for _, x := range xs {
		d := x - m
		ss += d * d
	}
	return math.Sqrt(ss / float64(len(xs)-1))
}

// ErrEmpty is returned when the builder is called without any usable
// symbol; the value is mostly used by tests to assert behavior.
var ErrEmpty = errors.New("intraday: no symbols")
