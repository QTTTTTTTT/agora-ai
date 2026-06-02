// Package fx implements P1-4 — FX rates + cross-currency NAV.
//
// What this package owns
//
//   - The Provider interface that any rate source (Yahoo, ECB,
//     manual upload, etc.) must satisfy.
//   - A YahooProvider implementation pulled from Yahoo Finance's
//     public quote endpoint (USDCNY=X, USDHKD=X, …).
//   - The Repo wrapping the fx_rates table with the read paths
//     the NAV aggregator needs (Latest, AsOf, Convert).
//   - A Convert helper that triangulates through USD when the
//     requested pair isn't stored directly. We deliberately do
//     NOT store every cross-rate (CNY/HKD etc.); instead we
//     read USD/CNY × HKD/USD and compute. This halves the write
//     volume and makes "the rate that mattered for snapshot T"
//     reproducible from one DB read pair.
//
// What this package does NOT own
//
//   - Scheduling. The daily-fetch loop lives in cmd/server/
//     fx_loop.go so it can be wired into the same leader-elected
//     scheduler the rest of the platform uses.
//   - Reporting / NAV math. The NAV aggregator imports this
//     package and asks Convert() — the math doesn't live here
//     because navcalc has its own opinions about totals.
//
// Numerical precision
//
// Rates are NUMERIC(20,8) in DB. We deserialize to float64 for
// the in-memory math because the worst-case rounding error from
// converting a USD position into CNY is on the order of 1e-7,
// which is below the 4 dp NAV displays anyway. If we later need
// higher precision (e.g. trillion-yen funds reporting in JPY),
// we'll switch to math/big.Rat — the Provider/Repo interfaces
// already accept and return float64 so the migration is local
// to the navcalc / cash_ledger summary callers.

package fx

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// AnchorCurrency is the pivot we triangulate through. Every
// stored row is either base=USD or quote=USD; cross-rates are
// computed.
const AnchorCurrency = "USD"

// SupportedCurrencies enumerates the closed vocabulary the
// platform accepts. Mirrors the DB CHECK on funds.base_currency
// + the allowlist the FX admin form uses.
//
// Order matters for UI rendering — we keep the most-used pairs
// first.
var SupportedCurrencies = []string{"USD", "CNY", "HKD", "EUR", "JPY", "GBP", "SGD"}

// IsSupported reports whether the platform should accept a
// currency. Callers should validate before writing into funds /
// fx_rates.
func IsSupported(c string) bool {
	c = strings.ToUpper(strings.TrimSpace(c))
	for _, s := range SupportedCurrencies {
		if c == s {
			return true
		}
	}
	return false
}

// Rate is one observation. Returned by both Provider and Repo
// so callers don't need to learn two shapes.
type Rate struct {
	Base   string
	Quote  string
	Rate   float64
	RateAt time.Time
	Source string
}

// Provider is the interface a FX source implements. Each call
// returns the latest rate the provider knows for the requested
// pair.
//
// Errors are returned for transport failures, unsupported pairs,
// and rate-limit responses. The scheduler interprets ErrRateUnavailable
// as "skip this pair this round, try again tomorrow" rather
// than failing the whole batch.
type Provider interface {
	Name() string
	Fetch(ctx context.Context, base, quote string) (*Rate, error)
}

// ErrRateUnavailable signals a transient miss the scheduler can
// safely retry. Concrete providers should wrap this with
// fmt.Errorf("...: %w", ErrRateUnavailable) so callers can use
// errors.Is.
var ErrRateUnavailable = errors.New("fx: rate unavailable")

// ErrUnsupportedPair signals "we will never have this rate" —
// the scheduler skips this pair for the rest of the run.
var ErrUnsupportedPair = errors.New("fx: unsupported currency pair")

// SameCurrency reports whether the conversion is a no-op. Used
// by Convert to short-circuit DB lookups when base==quote.
func SameCurrency(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// computeTriangulated returns the rate from `base` to `quote`
// given the two USD-anchored legs. Both legs are stored as
// "1 leg.Base = leg.Rate leg.Quote".
//
//   To go from base → quote where base != USD and quote != USD:
//     (quote per USD) / (base per USD)
//     = quote.Rate / base.Rate
//
//   When base == USD:  quote.Rate
//   When quote == USD: 1 / base.Rate
//
// Returns (0, false) if the rates are stale or inconsistent so
// the caller can fall back to "rate unavailable".
func computeTriangulated(usdToBase, usdToQuote *Rate) (float64, bool) {
	if usdToBase == nil || usdToQuote == nil {
		return 0, false
	}
	if usdToBase.Rate <= 0 || usdToQuote.Rate <= 0 {
		return 0, false
	}
	r := usdToQuote.Rate / usdToBase.Rate
	if math.IsNaN(r) || math.IsInf(r, 0) {
		return 0, false
	}
	return r, true
}

// roundFloat rounds to N decimal places — useful for stable
// equality in tests + display-side conversion. Not used inside
// the math itself.
func roundFloat(x float64, places int) float64 {
	if places < 0 {
		return x
	}
	shift := math.Pow(10, float64(places))
	return math.Round(x*shift) / shift
}

// canonicalCurrency normalises whitespace + case so the
// downstream lookups don't drift on mismatched casing.
func canonicalCurrency(c string) string {
	return strings.ToUpper(strings.TrimSpace(c))
}

// FormatPair returns "BASEQUOTE" suitable for logging /
// exception messages.
func FormatPair(base, quote string) string {
	return fmt.Sprintf("%s/%s", canonicalCurrency(base), canonicalCurrency(quote))
}
