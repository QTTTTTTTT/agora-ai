// Package fundamental fetches per-symbol fundamental metrics (PE,
// PB, margins, growth, market cap, dividend, debt) across markets.
//
// This is the Phase 2D companion to internal/ohlc: where ohlc gives
// the chart-driven Quant role its inputs, fundamental gives the
// Bull and Bear roles concrete valuation + earnings facts to argue
// over instead of generic narratives.
//
// The package mirrors ohlc's shape on purpose so the wiring layer
// has one mental model:
//
//   - Provider is the per-source adapter; declares Supports(market).
//   - Registry routes by market with fallback on ErrNoData.
//   - Cache wraps any Fetcher with TTL caching (24h is a reasonable
//     default — fundamentals change quarterly, not minute-by-minute).
//
// Like ohlc, every public function is safe to call against an
// unconfigured registry (returns ErrNoData / ErrNoProvider); the
// wiring layer's caller decides whether to silently skip or surface
// the gap.
package fundamental

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ErrNoData signals that no provider produced metrics for the
// request. Soft signal — callers should treat it as "skip this
// symbol", not "fail the workflow".
var ErrNoData = errors.New("fundamental: no data")

// ErrNoProvider signals that no provider in the registry claimed the
// requested market. Distinguished from ErrNoData so operators can
// detect a misconfigured deployment (e.g., a Chinese fund running
// without the Akshare URL wired).
var ErrNoProvider = errors.New("fundamental: no provider supports market")

// Metrics is a single symbol's fundamental snapshot. Zero-valued
// fields mean "the upstream didn't report this metric" — the
// formatter helpers in formatter.go skip them automatically so the
// prompt stays clean.
//
// Conventions:
//   - PE / ForwardPE: trailing / forward price-to-earnings ratio.
//   - PB: price-to-book.
//   - DividendYield: annualised yield, as a fraction (0.025 = 2.5%).
//   - ProfitMargin / OperatingMargin / ReturnOnEquity: fractions.
//   - RevenueGrowth / EarningsGrowth: YoY fractions (0.12 = +12%).
//   - DebtToEquity: ratio (1.5 = 150%).
//   - MarketCap: in BaseCurrency (the wiring layer is responsible
//     for currency normalisation if the fund mixes markets; we
//     surface the raw upstream number).
//   - Beta: 252-day equity beta vs the local benchmark.
//   - AsOf: when the upstream reported these values.
//   - Source: provider Name() that produced this snapshot.
type Metrics struct {
	Symbol           string
	PE               float64
	ForwardPE        float64
	PB               float64
	DividendYield    float64
	ProfitMargin     float64
	OperatingMargin  float64
	ReturnOnEquity   float64
	RevenueGrowth    float64
	EarningsGrowth   float64
	DebtToEquity     float64
	MarketCap        float64
	Beta             float64
	Currency         string
	AsOf             time.Time
	Source           string
}

// FetchRequest is the input to a provider call. Market is the
// canonical lowercase tag (us_equity, a_share, hk_equity).
type FetchRequest struct {
	Symbol string
	Market string
}

// Normalize trims and lower-cases the market tag, idempotent.
func (r FetchRequest) Normalize() FetchRequest {
	return FetchRequest{
		Symbol: strings.ToUpper(strings.TrimSpace(r.Symbol)),
		Market: strings.ToLower(strings.TrimSpace(r.Market)),
	}
}

// CacheKey produces a stable string for the cache layer. We do NOT
// bucket by time the way ohlc does because fundamentals are only
// refreshed quarterly upstream — the bucket is implicit in the
// cache TTL itself.
func (r FetchRequest) CacheKey() string {
	n := r.Normalize()
	return n.Market + "|" + n.Symbol
}

// Provider is the per-source adapter.
type Provider interface {
	Name() string
	Supports(market string) bool
	Fetch(ctx context.Context, req FetchRequest) (*Metrics, error)
}

// Fetcher is the small interface both Registry and Cache satisfy —
// useful so the wiring layer can swap in a stub for tests without
// caring which layer it's pointed at.
type Fetcher interface {
	Fetch(ctx context.Context, req FetchRequest) (*Metrics, error)
}
