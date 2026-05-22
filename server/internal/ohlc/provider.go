// Package ohlc fetches historical OHLCV bars across markets so the
// indicator package and the Phase 2B debate's Quant role can reason
// over real chart data instead of qualitative narratives.
//
// The package is deliberately minimal:
//
//   - Bar is a single OHLCV row.
//   - FetchRequest names a symbol + market + interval + lookback.
//   - Provider is the per-source implementation (Yahoo / Binance /
//     Akshare-MCP / etc.); each provider declares which markets it
//     supports via Supports(market).
//   - Registry routes a FetchRequest to the first provider whose
//     Supports(market) returns true. Multiple providers per market
//     gives operators a fallback chain (e.g., Yahoo first, then a
//     local fallback if Yahoo is down).
//   - Cache wraps any Provider with a TTL-keyed in-memory cache so
//     repeated indicator calls within a workflow tick don't fan out
//     into duplicate HTTP calls.
//
// Network providers fail open: a degraded ("no data") response is
// preferable to crashing the workflow, because callers either fall
// back to qualitative quant signals or downgrade to "no quant input".
// Every public function is safe to call with an unconfigured service
// (the cache returns ErrNoData and the indicator layer skips that
// signal).
package ohlc

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ErrNoData signals that no provider in the registry produced bars
// for the request. Callers should treat this as a soft signal (skip
// the indicator) rather than an exceptional condition.
var ErrNoData = errors.New("ohlc: no data")

// ErrNoProvider signals that no provider in the registry claimed
// support for the requested market. Distinguished from ErrNoData so
// operators can detect misconfiguration (e.g., crypto fund running
// without Binance wired).
var ErrNoProvider = errors.New("ohlc: no provider supports market")

// Bar is a single OHLCV row. Time is the bar's open time in UTC; the
// provider is responsible for normalising vendor-specific timestamps.
type Bar struct {
	Time   time.Time
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume float64
}

// Interval names a bar resolution. We support the common subset that
// every upstream agrees on; finer-grained intraday intervals can be
// added per-provider later. Empty string and unknown intervals
// collapse to "1d" so callers don't have to validate.
type Interval string

const (
	Interval1m  Interval = "1m"
	Interval5m  Interval = "5m"
	Interval15m Interval = "15m"
	Interval1h  Interval = "1h"
	IntervalDay Interval = "1d"
	Interval1w  Interval = "1w"
)

// Normalize returns a canonical interval string, defaulting to 1d
// when the input is empty or unrecognized. Keeps providers from
// having to guard against ad-hoc spellings.
func (i Interval) Normalize() Interval {
	switch strings.ToLower(strings.TrimSpace(string(i))) {
	case "1m", "1min":
		return Interval1m
	case "5m":
		return Interval5m
	case "15m":
		return Interval15m
	case "1h", "60m":
		return Interval1h
	case "1w", "1week":
		return Interval1w
	case "1d", "day", "":
		return IntervalDay
	default:
		return IntervalDay
	}
}

// FetchRequest is the input to a provider call. EndTime defaults to
// time.Now().UTC() when zero; LookbackN defaults to 120 (covers most
// indicator lookback windows including MACD's 26+9 EMA span).
type FetchRequest struct {
	Symbol    string
	Market    string
	Interval  Interval
	LookbackN int
	EndTime   time.Time
}

// Normalize fills in the request defaults. Idempotent — calling
// twice on the same request is a no-op.
func (r FetchRequest) Normalize() FetchRequest {
	out := r
	out.Symbol = strings.TrimSpace(out.Symbol)
	out.Market = strings.ToLower(strings.TrimSpace(out.Market))
	out.Interval = out.Interval.Normalize()
	if out.LookbackN <= 0 {
		out.LookbackN = 120
	}
	if out.EndTime.IsZero() {
		out.EndTime = time.Now().UTC()
	}
	return out
}

// CacheKey is a canonical string used by the in-memory cache to key
// results. Two requests differing only in EndTime by less than the
// cache TTL share the same key because the cache truncates EndTime
// to the bucket boundary (see Cache.Get).
func (r FetchRequest) CacheKey(bucket time.Duration) string {
	n := r.Normalize()
	bucketed := n.EndTime.Truncate(bucket).UTC().Format(time.RFC3339)
	parts := []string{
		strings.ToUpper(n.Symbol),
		n.Market,
		string(n.Interval),
		bucketed,
	}
	return strings.Join(parts, "|")
}

// Provider is the per-source OHLC adapter contract.
type Provider interface {
	// Name is a short identifier for logs and error messages
	// ("yahoo", "binance", "akshare").
	Name() string
	// Supports returns true when the provider can handle this
	// market tag. Markets are the canonical lowercase tags used by
	// fund profiles: "us_equity", "a_share", "hk_equity", "crypto",
	// "futures". A provider may return true for multiple markets.
	Supports(market string) bool
	// Fetch returns up to req.LookbackN bars ending at req.EndTime.
	// Implementations MUST return bars sorted oldest-first so
	// indicator helpers can iterate in chronological order.
	// Returning (nil, ErrNoData) is acceptable when the upstream
	// has no coverage; the registry treats this as a fallthrough
	// signal and tries the next provider for the market.
	Fetch(ctx context.Context, req FetchRequest) ([]Bar, error)
}
