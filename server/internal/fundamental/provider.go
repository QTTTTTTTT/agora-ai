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
	"math"
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
	// Name is the issuer's short / human-readable name (e.g.
	// "德科立", "Apple Inc."). Optional — populated by providers
	// that can resolve it cheaply (akshare sidecar via sina, Yahoo
	// quoteSummary). Empty when the provider doesn't know.
	Name             string
	PE               float64
	ForwardPE        float64
	PB               float64
	DividendYield    float64
	ProfitMargin     float64
	OperatingMargin  float64
	ReturnOnEquity   float64
	// RevenueGrowth is the YoY top-line growth from the most
	// recent ANNUAL report (period ending 12-31). Stable for
	// long-horizon judgements (Buffett "is this a moat business",
	// Lynch "stalwart vs fast grower") but lags by up to 12mo on
	// the timing of turnarounds.
	RevenueGrowth    float64
	// EarningsGrowth — same convention as RevenueGrowth.
	EarningsGrowth   float64
	// RevenueGrowthLatest / EarningsGrowthLatest carry the YoY
	// growth from the most recent INTERIM report (e.g. Q1) when
	// the company has reported a period after the last annual.
	// Empty (0) when the latest period IS the annual (then the
	// fields above are already the latest signal).
	//
	// Rationale: for disruptive-innovation personas (Wood, Lynch
	// fast-grower bucket) and turnaround / momentum tactics, the
	// most timely YoY print is the decisive signal — a stock that
	// printed -28% earnings in the last annual but +35% in the
	// most recent quarter is mid-inflection, and AVOID-on-annual
	// is the wrong call. Both fields are exposed so the prompt
	// can put them side-by-side and the persona can reason about
	// the slope.
	RevenueGrowthLatest  float64
	EarningsGrowthLatest float64
	// LatestPeriod is the upstream report-date label for the
	// _Latest fields (e.g. "2026-03-31"). Empty when no interim
	// period is more recent than the annual.
	LatestPeriod  string
	// AnnualPeriod is the report-date label for the annual fields
	// (e.g. "2025-12-31"). Always set when ROE/margins are set.
	AnnualPeriod  string
	// ListingDate is the company's exchange listing date in
	// "YYYY-MM-DD" form (e.g. "2022-08-09" for 688205 德科立).
	// Used to stop master agents from mechanically flagging
	// "history.10yr data_unavailable" on a sub-10-year listing —
	// the LLM prompt re-frames the gap as "次新股 N年" instead.
	// Empty when the provider couldn't resolve it.
	ListingDate string
	// ListingYears is decimal years from ListingDate to Metrics.AsOf
	// (e.g. 3.83 for a stock listed 2022-08-09 read in 2026-06).
	// Zero means "the provider didn't supply a listing date" — to
	// distinguish from "literally just IPO'd today", callers should
	// gate on ListingDate != "" before believing a 0.00 here.
	ListingYears float64
	// LatestRevenue / LatestNetIncome are the absolute CNY amounts
	// from the most recent 业绩快报 (earnings flash) filing. Distinct
	// from the *Growth fields above: those are derived percentages,
	// these are the underlying numerators reviewers need to verify
	// the percentage. Without these, an "11% revenue growth" claim
	// is unfalsifiable — with them, a reviewer can divide by the
	// prior-period number and recompute.
	LatestRevenue   float64
	LatestNetIncome float64
	// LatestAnnounceDate is the YYYY-MM-DD on which the issuer
	// published the filing the _Latest fields above came from.
	// Master agents (rule 8 in master_agent.go) are instructed to
	// cite this date alongside any *_latest figure they quote, so
	// any external reviewer can pull the original announcement off
	// the exchange and double-check the numbers.
	LatestAnnounceDate string
	// LatestRevenueQoQ / LatestNetIncomeQoQ are the quarter-over-
	// quarter (not YoY) deltas from the same filing. Pure YoY can
	// hide a turning point: 688205 Q1 2026 was +27.97% YoY but
	// -9.63% QoQ on revenue, which signals the AI-network demand
	// pulse from Q4 2025 already cooled in Q1 2026 — exactly the
	// kind of momentum reversal a YoY-only view misses.
	LatestRevenueQoQ   float64
	LatestNetIncomeQoQ float64
	// GrossMarginLatest is the latest filing's 销售毛利率 as a
	// fraction (0.2573 for 25.73%). We already carry ProfitMargin
	// (net margin) from the annual print; gross margin is shipped
	// separately because (a) it lags differently than net margin
	// under price-war pressure and (b) the value-investing
	// personas (Munger, Lynch) read 毛利率 trend as the primary
	// "is the moat eroding" signal.
	GrossMarginLatest float64
	// LatestSource is the upstream tag for provenance (e.g.
	// "eastmoney_yjbb"). Surfaced verbatim in the LLM prompt
	// alongside latest_announce_date so a reviewer knows which
	// of the company's filings was indexed.
	LatestSource string
	DebtToEquity     float64
	MarketCap        float64
	Beta             float64
	Currency         string
	AsOf             time.Time
	Source           string

	// History carries the multi-year annual snapshots needed by
	// master agents whose criteria are stated as multi-year
	// averages (Buffett "ROE 10yr avg ≥ 15%", Graham "earnings
	// stable 10 years", Lynch "3yr EPS CAGR ≥ 25%").
	//
	// Ordered DESCENDING by Year — History[0] is the most recent.
	// nil / empty means the upstream provider didn't yield a
	// history series (single-period providers leave this empty).
	History []YearlyMetrics
}

// YearlyMetrics is one annual snapshot inside Metrics.History.
//
// Convention: rates are stored as fractions where applicable
// (ROE=0.18 means 18%) — same as the snapshot fields above.
// Zero-valued fields mean "the upstream didn't report it for
// this year", not "the value is zero". HistoricalAggregate's
// average helpers skip zeros so a missing year doesn't pollute
// the 10yr mean.
type YearlyMetrics struct {
	Year             int
	ReturnOnEquity   float64 // 净资产收益率, fraction
	ReturnOnCapital  float64 // 投入资本回报率, fraction
	GrossMargin      float64 // 毛利率, fraction
	OperatingMargin  float64
	ProfitMargin     float64
	FreeCashFlow     float64 // raw amount in Currency
	EPS              float64 // basic earnings-per-share
	BookValuePerShare float64
	DividendPerShare float64
	CurrentRatio     float64 // 流动比率
	DebtToEquity     float64
	RevenueGrowthYoY float64 // 营业收入同比增速
	EarningsGrowthYoY float64
}

// HistoricalAggregate is a precomputed view of Metrics.History used
// by the master-agent rule layer. We compute the averages once on
// the data layer so every master agent gets the same numbers
// (Buffett's 10yr ROE average matches Graham's, etc.).
//
// All fields are 0 when no years contribute a non-zero value. Callers
// should check Years > 0 before treating the averages as meaningful.
type HistoricalAggregate struct {
	Years              int
	AvgROE             float64
	AvgROIC            float64
	AvgGrossMargin     float64
	AvgOperatingMargin float64
	AvgFCF             float64
	EPSCAGR            float64 // earnings CAGR over the lookback window
	BVPSCAGR           float64 // book value CAGR
	DividendCAGR       float64
	PositiveFCFYears   int   // count of years with FCF > 0
	PositiveEPSYears   int
	YearsObserved      []int // descending list of years that contributed
}

// Aggregate returns the rule-anchored view of `m.History` over at
// most lookbackYears (e.g. lookbackYears=10 for Buffett). When
// History has fewer entries than lookbackYears we aggregate
// whatever is present and set Years accordingly so callers can
// detect "not enough data".
func (m Metrics) Aggregate(lookbackYears int) HistoricalAggregate {
	out := HistoricalAggregate{}
	if len(m.History) == 0 || lookbackYears <= 0 {
		return out
	}
	n := lookbackYears
	if n > len(m.History) {
		n = len(m.History)
	}
	out.Years = n
	slice := m.History[:n]

	sumROE, cntROE := 0.0, 0
	sumROIC, cntROIC := 0.0, 0
	sumGM, cntGM := 0.0, 0
	sumOM, cntOM := 0.0, 0
	sumFCF, cntFCF := 0.0, 0
	for _, y := range slice {
		out.YearsObserved = append(out.YearsObserved, y.Year)
		if y.ReturnOnEquity != 0 {
			sumROE += y.ReturnOnEquity
			cntROE++
		}
		if y.ReturnOnCapital != 0 {
			sumROIC += y.ReturnOnCapital
			cntROIC++
		}
		if y.GrossMargin != 0 {
			sumGM += y.GrossMargin
			cntGM++
		}
		if y.OperatingMargin != 0 {
			sumOM += y.OperatingMargin
			cntOM++
		}
		if y.FreeCashFlow != 0 {
			sumFCF += y.FreeCashFlow
			cntFCF++
			if y.FreeCashFlow > 0 {
				out.PositiveFCFYears++
			}
		}
		if y.EPS > 0 {
			out.PositiveEPSYears++
		}
	}
	if cntROE > 0 {
		out.AvgROE = sumROE / float64(cntROE)
	}
	if cntROIC > 0 {
		out.AvgROIC = sumROIC / float64(cntROIC)
	}
	if cntGM > 0 {
		out.AvgGrossMargin = sumGM / float64(cntGM)
	}
	if cntOM > 0 {
		out.AvgOperatingMargin = sumOM / float64(cntOM)
	}
	if cntFCF > 0 {
		out.AvgFCF = sumFCF / float64(cntFCF)
	}
	// CAGR uses oldest → newest endpoints when both are positive.
	out.EPSCAGR = cagr(slice[len(slice)-1].EPS, slice[0].EPS, len(slice))
	out.BVPSCAGR = cagr(slice[len(slice)-1].BookValuePerShare, slice[0].BookValuePerShare, len(slice))
	out.DividendCAGR = cagr(slice[len(slice)-1].DividendPerShare, slice[0].DividendPerShare, len(slice))
	return out
}

// cagr computes the compound annual growth rate from begin → end
// across n entries (n-1 periods). Returns 0 when inputs are
// missing or the ratio is invalid (would otherwise return NaN/Inf).
func cagr(begin, end float64, n int) float64 {
	if begin <= 0 || end <= 0 || n <= 1 {
		return 0
	}
	periods := float64(n - 1)
	ratio := end / begin
	if ratio <= 0 {
		return 0
	}
	growth := math.Pow(ratio, 1.0/periods) - 1
	if math.IsNaN(growth) || math.IsInf(growth, 0) {
		return 0
	}
	return growth
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

// HistoricalProvider is the equivalent of Provider for multi-year
// snapshots. Kept separate from Provider so an upstream that only
// offers a single-period snapshot can satisfy Provider without
// having to fake a history series.
//
// Implementations live in provider_yahoo_history.go and
// provider_akshare_history.go. Phase 2 wires them through a
// HistoricalRegistry + HistoricalCache mirroring the snapshot
// counterparts; the wiring layer can attach the history series to
// Metrics.History on every Fetch by passing both registries
// through a small composing helper (see internal/fundamental
// EnrichWithHistory).
type HistoricalProvider interface {
	Name() string
	Supports(market string) bool
	FetchHistory(ctx context.Context, req FetchRequest, lookbackYears int) ([]YearlyMetrics, error)
}

// HistoricalFetcher is the small interface the cache satisfies.
type HistoricalFetcher interface {
	FetchHistory(ctx context.Context, req FetchRequest, lookbackYears int) ([]YearlyMetrics, error)
}
