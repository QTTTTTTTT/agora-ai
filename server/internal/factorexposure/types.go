// Package factorexposure computes portfolio-level factor tilts.
//
// What this package owns
//
//   - Domain types: Factor enum, InstrumentLoading, Holding,
//     PortfolioExposure, Snapshot.
//   - Pure engine: turn a slice of Holdings + lookup of
//     InstrumentLoadings into six PortfolioExposure rows (one per
//     canonical factor).
//   - Repository: read latest-as-of-T loadings for a set of
//     instruments; append portfolio snapshots; admin CRUD on
//     loadings.
//   - Cache: warm + refresh + admin write-through for the loadings
//     table, keyed by (instrument, factor) → latest loading.
//
// What this package deliberately does NOT own
//
//   - Computing loadings from scratch. That requires multi-year
//     returns regressions and is a Quant Lab batch job (planned
//     for S10). This package consumes whatever loadings the lab
//     (or a manual admin upsert) has written.
//   - Storing positions. The wiring layer reads from
//     `holding_positions` (the same table the rest of the
//     portfolio path uses) and hands a Holding slice in.
//
// Why six factors
//
// Fama-French three (market / size / value) + Carhart momentum +
// the modern multi-factor consensus (quality, low-vol) = six. This
// covers what every PM-targeted risk dashboard in the industry
// shows. Sector concentration is handled by internal/exposure
// (Sprint C #1) and explicitly excluded here.
//
// Why net + gross
//
//   - Net exposure = signed weighted average. Surfaces "tilt".
//   - Gross exposure = sum of |weight * loading|. Surfaces hidden
//     long-short factor bets that net-out at zero but still expose
//     the fund to factor volatility (pair trades, market-neutral
//     books, factor-hedged single-name positions).
//
// Showing both is the convention every prime broker risk report
// uses; reporting only net would mislead any fund running a
// long-short or pair-trade book.
package factorexposure

import (
	"strings"
	"time"
)

// Factor enumerates the canonical factor names this package
// understands. The string values match the CHECK constraint on
// instrument_factor_loadings.factor; changing them requires a
// follow-up migration.
type Factor string

const (
	FactorSize       Factor = "size"
	FactorValue      Factor = "value"
	FactorMomentum   Factor = "momentum"
	FactorQuality    Factor = "quality"
	FactorLowVol     Factor = "lowvol"
	FactorMarketBeta Factor = "market_beta"
)

// AllFactors is the canonical order the UI renders. Stable so
// snapshots render deterministically across builds.
var AllFactors = []Factor{
	FactorSize,
	FactorValue,
	FactorMomentum,
	FactorQuality,
	FactorLowVol,
	FactorMarketBeta,
}

// IsValid reports whether the given string is one of the six
// canonical factors. Empty strings return false. Comparison is
// case-sensitive: callers should pre-normalise.
func (f Factor) IsValid() bool {
	switch f {
	case FactorSize, FactorValue, FactorMomentum,
		FactorQuality, FactorLowVol, FactorMarketBeta:
		return true
	}
	return false
}

// ParseFactor accepts a case-insensitive name and returns the
// canonical Factor or "" + false when unknown.
func ParseFactor(s string) (Factor, bool) {
	candidate := Factor(strings.ToLower(strings.TrimSpace(s)))
	if !candidate.IsValid() {
		return "", false
	}
	return candidate, true
}

// LoadingSource enumerates the upstream that wrote a loading row.
// Used for both the source CHECK constraint and the audit log so
// operators can answer "where did this number come from?" without
// chasing through code.
type LoadingSource string

const (
	LoadingSourceManual    LoadingSource = "manual"
	LoadingSourceEastMoney LoadingSource = "eastmoney"
	LoadingSourceMSCI      LoadingSource = "msci"
	LoadingSourceComputed  LoadingSource = "computed"
	LoadingSourceOverride  LoadingSource = "override"
)

// IsValid for LoadingSource; mirrors the DB CHECK.
func (s LoadingSource) IsValid() bool {
	switch s {
	case LoadingSourceManual, LoadingSourceEastMoney,
		LoadingSourceMSCI, LoadingSourceComputed, LoadingSourceOverride:
		return true
	}
	return false
}

// InstrumentLoading is one row of the instrument_factor_loadings
// table. The engine consumes a slice of these together with a
// slice of Holdings; the repo writes them.
type InstrumentLoading struct {
	InstrumentKey string
	Factor        Factor
	AsOf          time.Time
	Loading       float64
	Source        LoadingSource
	Note          string
	UpdatedAt     time.Time
}

// Holding is the minimal view of one position the engine needs.
// Constructed by the wiring layer from holding_positions; the
// alias-free type lets this package stay dependency-free.
//
// MarketValue is in the fund's base currency, signed: long > 0,
// short < 0. The engine derives weight = MarketValue / Gross when
// Gross > 0; an all-cash fund gets zero exposure rows.
type Holding struct {
	InstrumentKey string
	Symbol        string
	MarketValue   float64
}

// PortfolioExposure is one row of the per-factor result. The
// engine produces exactly len(AllFactors) rows per call, in
// AllFactors order, so the UI can iterate without sorting and the
// snapshot writer can persist atomically.
//
//   - NetExposure = sum over holdings of (weight * loading), where
//     weight = MarketValue / TotalGrossMV. Signed.
//   - GrossExposure = sum of |weight * loading|. Non-negative.
//   - CapitalPct = sum of |weight| over holdings that contributed
//     a non-zero loading. In [0, 1]; "this number covers X% of
//     book". When < 1 it means some holdings had no loading row
//     for this factor — the UI shows a "coverage" pill.
//   - HoldingCount is the number of distinct holdings that
//     contributed (had a non-zero loading for this factor).
//   - LoadingsAsOf is the most recent asof date among the loadings
//     that contributed. Lets the UI show "loadings 9 days stale"
//     without re-querying.
type PortfolioExposure struct {
	Factor         Factor
	NetExposure    float64
	GrossExposure  float64
	CapitalPct     float64
	HoldingCount   int
	LoadingsAsOf   time.Time
}

// Snapshot is the full read returned by the live endpoint. It
// pairs the six PortfolioExposure rows with the input scalars
// the UI needs to render the dashboard (NAV breakdown, holding
// count, generation time).
type Snapshot struct {
	FundID             string
	GeneratedAt        time.Time
	NAV                float64 // gross MV (sum of |MarketValue|)
	HoldingsTotal      int     // total holdings in the input
	HoldingsCovered    int     // distinct holdings that contributed to at least one factor
	OldestLoadingAsOf  time.Time
	Exposures          []PortfolioExposure
}
