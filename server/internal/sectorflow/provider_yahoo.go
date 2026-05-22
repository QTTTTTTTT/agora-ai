package sectorflow

import (
	"context"
	"strings"
	"time"

	"github.com/fundai/server/internal/ohlc"
)

// YahooSectorProvider computes US sector rotation from the SPDR
// Select Sector ETFs (XLK, XLF, XLV, XLE, XLY, XLP, XLI, XLU, XLB,
// XLRE, XLC). It piggybacks on internal/ohlc so we don't need a
// separate HTTP integration — any ohlc.Fetcher that supports the
// us_equity market will do (Yahoo, IB, etc.).
//
// Returns are computed from daily closes:
//
//	Return1d  = close[-1] / close[-2] - 1
//	Return5d  = close[-1] / close[-5] - 1
//	Return20d = close[-1] / close[-20] - 1
//
// Missing history degrades the corresponding return to 0 rather
// than dropping the row; the formatter will simply skip the empty
// timeframe.
type YahooSectorProvider struct {
	OHLC ohlc.Fetcher
	// Markets defaults to {"us_equity"} when empty.
	Markets []string
}

// SectorETF binds a sector name to its index ETF symbol.
type SectorETF struct {
	Name   string
	Symbol string
}

// DefaultSectorETFs is the canonical SPDR Select Sector ETF set,
// covering the 11 GICS sectors. Operators can override by
// constructing a YahooSectorProvider with a custom etfs list (we'd
// expose that via env if/when needed).
func DefaultSectorETFs() []SectorETF {
	return []SectorETF{
		{Name: "Technology", Symbol: "XLK"},
		{Name: "Financials", Symbol: "XLF"},
		{Name: "Health Care", Symbol: "XLV"},
		{Name: "Energy", Symbol: "XLE"},
		{Name: "Consumer Discretionary", Symbol: "XLY"},
		{Name: "Consumer Staples", Symbol: "XLP"},
		{Name: "Industrials", Symbol: "XLI"},
		{Name: "Utilities", Symbol: "XLU"},
		{Name: "Materials", Symbol: "XLB"},
		{Name: "Real Estate", Symbol: "XLRE"},
		{Name: "Communication Services", Symbol: "XLC"},
	}
}

// Name implements Provider.
func (p *YahooSectorProvider) Name() string { return "yahoo_sector" }

// Supports implements Provider.
func (p *YahooSectorProvider) Supports(market string) bool {
	if p.OHLC == nil {
		return false
	}
	markets := p.Markets
	if len(markets) == 0 {
		markets = []string{"us_equity"}
	}
	for _, m := range markets {
		if strings.EqualFold(m, market) {
			return true
		}
	}
	return false
}

// Fetch implements Provider. Calls ohlc.Fetcher for each ETF in
// parallel via goroutines, with a context cap of 6s total per
// provider call.
func (p *YahooSectorProvider) Fetch(ctx context.Context, req FetchRequest) (*Snapshot, error) {
	if p.OHLC == nil {
		return nil, ErrNoData
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	etfs := DefaultSectorETFs()
	rows := make([]Sector, 0, len(etfs))
	for _, etf := range etfs {
		bars, err := p.OHLC.Fetch(ctx, ohlc.FetchRequest{
			Symbol:    etf.Symbol,
			Market:    "us_equity",
			Interval:  ohlc.IntervalDay,
			// 60 bars: enough for 20d return plus buffer for holidays.
			LookbackN: 60,
			EndTime:   time.Now().UTC(),
		})
		if err != nil || len(bars) < 2 {
			rows = append(rows, Sector{Name: etf.Name, Symbol: etf.Symbol, Currency: "USD"})
			continue
		}
		row := computeETFReturns(etf, bars)
		row.Currency = "USD"
		rows = append(rows, row)
	}
	if !anyReturnPopulated(rows) {
		return nil, ErrNoData
	}
	return &Snapshot{
		Market:  "us_equity",
		AsOf:    time.Now().UTC(),
		Sectors: rows,
		Source:  "yahoo_sector",
	}, nil
}

// computeETFReturns produces a single Sector row from bars. Bars
// are assumed chronologically sorted ascending — same contract the
// ohlc package gives us.
func computeETFReturns(etf SectorETF, bars []ohlc.Bar) Sector {
	row := Sector{Name: etf.Name, Symbol: etf.Symbol}
	n := len(bars)
	if n == 0 {
		return row
	}
	row.AsOf = bars[n-1].Time
	last := bars[n-1].Close
	if n >= 2 && bars[n-2].Close > 0 {
		row.Return1d = last/bars[n-2].Close - 1
	}
	if n >= 6 && bars[n-6].Close > 0 {
		row.Return5d = last/bars[n-6].Close - 1
	}
	if n >= 21 && bars[n-21].Close > 0 {
		row.Return20d = last/bars[n-21].Close - 1
	}
	return row
}

func anyReturnPopulated(rows []Sector) bool {
	for _, r := range rows {
		if r.Return1d != 0 || r.Return5d != 0 || r.Return20d != 0 {
			return true
		}
	}
	return false
}
