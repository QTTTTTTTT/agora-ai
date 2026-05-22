// Package sectorflow reports cross-sector capital rotation: which
// industries are bid up and which are bled today and over the
// trailing N days. This lets the Bull/Bear/Quant debate ground its
// arguments in concrete rotation evidence ("Tech is leading
// today's tape, +1.8%, while Energy is bleeding -2.3%") instead of
// generic narratives ("the market is in risk-on mode").
//
// Two reference implementations ship with this package:
//
//   - YahooSectorProvider:   uses the SPDR Select Sector ETF set
//                            (XLK, XLF, XLV, XLE, XLY, XLP, XLI,
//                            XLU, XLB, XLRE, XLC) and computes
//                            1d / 5d / 20d returns via ohlc.
//   - AkshareSectorProvider: hits a self-hosted akshare-MCP route
//                            that returns A-share industry
//                            money-flow rows (today / 5d, net
//                            inflow + % change).
//
// Like the ohlc and fundamental packages, providers are routed by
// market through a Registry; the wiring layer composes a Cache
// around the registry with a short TTL (5 minutes is the suggested
// default since sector flow shifts intraday).
package sectorflow

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ErrNoData signals no provider produced rows. Soft.
var ErrNoData = errors.New("sectorflow: no data")

// ErrNoProvider signals no registered provider claimed the market.
// Surfaces config issues.
var ErrNoProvider = errors.New("sectorflow: no provider supports market")

// Sector is a single industry's snapshot. Returns are fractions:
//
//	0.018 = +1.8%   -0.023 = -2.3%
//
// NetInflow is currency-units inflow (positive = capital flowed in,
// negative = flowed out). Currency tags the units. AsOf is the
// upstream's stated freshness.
type Sector struct {
	Name          string
	Symbol        string
	Return1d      float64
	Return5d      float64
	Return20d     float64
	NetInflow     float64
	NetInflow5d   float64
	Currency      string
	AsOf          time.Time
}

// Snapshot is the per-market rotation snapshot. Sectors are sorted
// best→worst by Return1d (the formatter relies on this).
type Snapshot struct {
	Market  string
	AsOf    time.Time
	Sectors []Sector
	Source  string
}

// FetchRequest is the input to a provider call.
type FetchRequest struct {
	Market string
}

// Normalize idempotently lower-cases the market tag.
func (r FetchRequest) Normalize() FetchRequest {
	return FetchRequest{Market: strings.ToLower(strings.TrimSpace(r.Market))}
}

// CacheKey for memoisation. Market alone is enough because there's
// no symbol component to vary by.
func (r FetchRequest) CacheKey() string {
	return r.Normalize().Market
}

// Provider is the per-source adapter.
type Provider interface {
	Name() string
	Supports(market string) bool
	Fetch(ctx context.Context, req FetchRequest) (*Snapshot, error)
}

// Fetcher is the small interface Registry and Cache both satisfy.
type Fetcher interface {
	Fetch(ctx context.Context, req FetchRequest) (*Snapshot, error)
}
