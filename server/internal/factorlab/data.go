// Package factorlab is the G1 #3 backtest harness MVP.
//
// Goal: produce per-factor IS Sharpe / drawdown / hit-rate
// numbers so we can answer "which of the 24 signal blocks
// actually carries alpha on its own, and which only matter as
// gating overlays?" without relying on LLM-in-loop replay
// (which the existing internal/backtest package supports but
// is too slow + costly for repeated per-factor sweeps).
//
// Scope vs the existing backtest package:
//
//   - internal/backtest = LLM-in-loop replay; runs the full
//     decision.DecisionEngine. Single-strategy at a time;
//     used to validate the production decision pipeline as a
//     whole.
//   - internal/factorlab = direct, pure-function per-factor
//     strategies. Multi-strategy in one run; used to MEASURE
//     each factor's marginal alpha in isolation. No LLM, no
//     fundamentals / earnings (yet — Sprint H candidate).
//
// MVP factor coverage:
//   - momentum_12_1m       (Jegadeesh-Titman 1993; the
//                           backbone of universeRanking + tsmom
//                           blocks)
//   - low_beta             (Frazzini-Pedersen 2014;
//                           lowBetaScores block)
//   - low_vol              (the "B" half of BAB — realized
//                           volatility, negated)
//   - equal_weight_long    (the buy-and-hold baseline every
//                           other factor must beat to claim
//                           any alpha)
//
// Each strategy is a pure function (Fixture, asOf) → target
// weights, so cross-factor comparison is a single-run sweep.
//
// Data layer (this file): fixture loader for daily OHLC CSVs.
// We deliberately use CSV (not parquet, not the live OHLC
// provider chain) so the backtest is REPRODUCIBLE across
// machines and CI runs — the fixture is committed and frozen.
package factorlab

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Bar is one symbol's daily OHLC observation. Volume / adjClose
// are optional; the strategies in the MVP only need Close (for
// returns) and OHLC (for ATR-style volatility metrics down the
// line).
type Bar struct {
	Date     time.Time `json:"date"`
	Open     float64   `json:"open,omitempty"`
	High     float64   `json:"high,omitempty"`
	Low      float64   `json:"low,omitempty"`
	Close    float64   `json:"close"`
	Volume   float64   `json:"volume,omitempty"`
	AdjClose float64   `json:"adjClose,omitempty"`
}

// SymbolHistory is the per-symbol time series sorted ascending
// by date. The slice MUST contain at least 2 bars for any
// strategy that depends on a return calculation.
type SymbolHistory struct {
	Symbol string
	Market string // optional, defaults to "us_equity" via Fixture metadata
	Bars   []Bar
}

// Fixture is the full backtest dataset: one history per symbol,
// plus an optional benchmark (typically SPY for US equity) used
// by beta-based strategies.
type Fixture struct {
	Histories []SymbolHistory
	Benchmark *SymbolHistory // optional; nil = no benchmark wired
	Start     time.Time
	End       time.Time
}

// Symbols returns the canonical (sorted, upper-cased) symbol
// list — handy for iteration and report ordering.
func (f *Fixture) Symbols() []string {
	if f == nil {
		return nil
	}
	out := make([]string, 0, len(f.Histories))
	for _, h := range f.Histories {
		out = append(out, strings.ToUpper(strings.TrimSpace(h.Symbol)))
	}
	sort.Strings(out)
	return out
}

// TradingDays returns the union of every history's bar dates,
// sorted ascending and deduplicated. Strategies use this as the
// "for each day in the backtest window" iterator.
func (f *Fixture) TradingDays() []time.Time {
	if f == nil {
		return nil
	}
	seen := make(map[time.Time]struct{}, 512)
	for _, h := range f.Histories {
		for _, b := range h.Bars {
			d := normaliseDate(b.Date)
			seen[d] = struct{}{}
		}
	}
	out := make([]time.Time, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// History returns the history for the given symbol, or nil
// when the symbol isn't in the fixture. Case-insensitive.
func (f *Fixture) History(symbol string) *SymbolHistory {
	if f == nil {
		return nil
	}
	want := strings.ToUpper(strings.TrimSpace(symbol))
	for i := range f.Histories {
		if strings.ToUpper(strings.TrimSpace(f.Histories[i].Symbol)) == want {
			return &f.Histories[i]
		}
	}
	return nil
}

// CloseAt returns the close for the given (symbol, asOf) pair.
// asOf is matched as "the last bar at or before this date" to
// gracefully handle holidays where the symbol didn't trade. The
// ok flag is false when no such bar exists (asOf earlier than
// the history's first bar OR the symbol isn't in the fixture).
func (f *Fixture) CloseAt(symbol string, asOf time.Time) (float64, bool) {
	hist := f.History(symbol)
	if hist == nil || len(hist.Bars) == 0 {
		return 0, false
	}
	target := normaliseDate(asOf)
	// Binary search for the rightmost bar with bar.Date <= target.
	idx := sort.Search(len(hist.Bars), func(i int) bool {
		return normaliseDate(hist.Bars[i].Date).After(target)
	}) - 1
	if idx < 0 {
		return 0, false
	}
	return hist.Bars[idx].Close, true
}

// CloseSeries returns the close series for the symbol over the
// inclusive window [from, to]. Returns nil if the symbol is
// absent. Bars without a close in the window are simply
// omitted — strategies handle gaps gracefully.
func (f *Fixture) CloseSeries(symbol string, from, to time.Time) []float64 {
	hist := f.History(symbol)
	if hist == nil {
		return nil
	}
	fromN, toN := normaliseDate(from), normaliseDate(to)
	out := make([]float64, 0, len(hist.Bars))
	for _, b := range hist.Bars {
		d := normaliseDate(b.Date)
		if d.Before(fromN) || d.After(toN) {
			continue
		}
		if b.Close > 0 {
			out = append(out, b.Close)
		}
	}
	return out
}

// LogReturns converts a price series into log-returns.
// len(out) == len(in)-1. Returns nil when in has < 2 bars.
func LogReturns(closes []float64) []float64 {
	if len(closes) < 2 {
		return nil
	}
	out := make([]float64, 0, len(closes)-1)
	for i := 1; i < len(closes); i++ {
		if closes[i-1] <= 0 || closes[i] <= 0 {
			continue
		}
		out = append(out, math.Log(closes[i]/closes[i-1]))
	}
	return out
}

// ---------------------------------------------------------------------------
// CSV loader
// ---------------------------------------------------------------------------

// LoadFixture reads a directory of per-symbol CSV files and
// assembles them into a Fixture. The directory layout we expect:
//
//	rootDir/
//	  AAPL.csv     (header: date,open,high,low,close,volume[,adj_close])
//	  MSFT.csv
//	  ...
//	  _benchmark/SPY.csv   (optional)
//
// Per-symbol files are sorted by date asc on load. Missing
// columns are tolerated (we accept date,close-only files too).
// Date format is ISO-8601 (YYYY-MM-DD) — we reject any other
// format to keep the fixture format unambiguous.
//
// Returns an error when:
//   - rootDir doesn't exist
//   - any CSV is unparseable
//   - the resulting fixture is empty
//
// The returned Fixture's Start / End are the intersection of
// every symbol's date range (so strategies always have data for
// every symbol on every backtest day).
func LoadFixture(rootDir string) (*Fixture, error) {
	rootDir = strings.TrimRight(rootDir, "/")
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		return nil, fmt.Errorf("factorlab: read fixture dir %q: %w", rootDir, err)
	}
	fixture := &Fixture{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			if name == "_benchmark" {
				if bh, berr := loadBenchmarkDir(rootDir + "/_benchmark"); berr != nil {
					return nil, berr
				} else if bh != nil {
					fixture.Benchmark = bh
				}
			}
			continue
		}
		if !strings.HasSuffix(strings.ToLower(name), ".csv") {
			continue
		}
		symbol := strings.TrimSuffix(name, ".csv")
		symbol = strings.TrimSuffix(symbol, ".CSV")
		hist, err := loadSymbolCSV(rootDir+"/"+name, symbol)
		if err != nil {
			return nil, err
		}
		if len(hist.Bars) == 0 {
			continue
		}
		fixture.Histories = append(fixture.Histories, *hist)
	}
	if len(fixture.Histories) == 0 {
		return nil, fmt.Errorf("factorlab: no symbol files found in %q", rootDir)
	}
	fixture.Start, fixture.End = symbolIntersection(fixture.Histories)
	return fixture, nil
}

func loadBenchmarkDir(dir string) (*SymbolHistory, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Missing benchmark dir is fine; we just won't carry one.
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("factorlab: read benchmark dir %q: %w", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(name), ".csv") {
			continue
		}
		symbol := strings.TrimSuffix(name, ".csv")
		hist, err := loadSymbolCSV(dir+"/"+name, symbol)
		if err != nil {
			return nil, err
		}
		if len(hist.Bars) == 0 {
			continue
		}
		return hist, nil
	}
	return nil, nil
}

func loadSymbolCSV(path, symbol string) (*SymbolHistory, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("factorlab: open %q: %w", path, err)
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("factorlab: read header %q: %w", path, err)
	}
	cols := indexColumns(header)
	if _, ok := cols["date"]; !ok {
		return nil, fmt.Errorf("factorlab: %q missing required 'date' column", path)
	}
	if _, ok := cols["close"]; !ok {
		return nil, fmt.Errorf("factorlab: %q missing required 'close' column", path)
	}
	hist := &SymbolHistory{Symbol: strings.ToUpper(strings.TrimSpace(symbol))}
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("factorlab: read row %q: %w", path, err)
		}
		bar, err := parseBar(row, cols)
		if err != nil {
			return nil, fmt.Errorf("factorlab: parse row in %q: %w", path, err)
		}
		hist.Bars = append(hist.Bars, bar)
	}
	sort.Slice(hist.Bars, func(i, j int) bool {
		return hist.Bars[i].Date.Before(hist.Bars[j].Date)
	})
	return hist, nil
}

func indexColumns(header []string) map[string]int {
	out := make(map[string]int, len(header))
	for i, h := range header {
		k := strings.ToLower(strings.TrimSpace(h))
		// Accept both "adj_close" and "adjclose" / "adjusted_close".
		switch k {
		case "adj_close", "adjclose", "adjusted_close":
			k = "adj_close"
		}
		out[k] = i
	}
	return out
}

func parseBar(row []string, cols map[string]int) (Bar, error) {
	get := func(name string) string {
		idx, ok := cols[name]
		if !ok || idx >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[idx])
	}
	dateStr := get("date")
	parsedDate, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		return Bar{}, fmt.Errorf("bad date %q (expected YYYY-MM-DD)", dateStr)
	}
	closeF, err := strconv.ParseFloat(get("close"), 64)
	if err != nil {
		return Bar{}, fmt.Errorf("bad close %q", get("close"))
	}
	bar := Bar{Date: normaliseDate(parsedDate), Close: closeF}
	if v := get("open"); v != "" {
		bar.Open, _ = strconv.ParseFloat(v, 64)
	}
	if v := get("high"); v != "" {
		bar.High, _ = strconv.ParseFloat(v, 64)
	}
	if v := get("low"); v != "" {
		bar.Low, _ = strconv.ParseFloat(v, 64)
	}
	if v := get("volume"); v != "" {
		bar.Volume, _ = strconv.ParseFloat(v, 64)
	}
	if v := get("adj_close"); v != "" {
		bar.AdjClose, _ = strconv.ParseFloat(v, 64)
	}
	return bar, nil
}

// symbolIntersection returns the latest first-bar and earliest
// last-bar across all histories so the backtest window is the
// region where every symbol has data.
func symbolIntersection(hist []SymbolHistory) (time.Time, time.Time) {
	var start, end time.Time
	for i, h := range hist {
		if len(h.Bars) == 0 {
			continue
		}
		first := normaliseDate(h.Bars[0].Date)
		last := normaliseDate(h.Bars[len(h.Bars)-1].Date)
		if i == 0 {
			start, end = first, last
			continue
		}
		if first.After(start) {
			start = first
		}
		if last.Before(end) {
			end = last
		}
	}
	return start, end
}

// normaliseDate strips the time component so map keys / sorts
// are stable.
func normaliseDate(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
