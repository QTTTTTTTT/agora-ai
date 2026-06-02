// fx_nav.go — cross-currency NAV aggregator (P1-4).
//
// What lives here
//
// AggregateInBase takes a list of multi-currency value buckets
// (positions, cash, accruals) and returns the total in the
// fund's base_currency. Each bucket carries its native currency
// + an at-time so historical NAV reconstructs deterministically
// (using the rate that was current on that day, not today's
// rate).
//
// Why a tiny helper instead of an end-to-end NAV recompute
//
// The platform already has a NAV recompute path
// (cmd/server/wiring_adapters.go's UpdateCapital loop). That
// loop pulls positions + cash from a dozen sources and writes
// totals back into funds + nav_snapshots. Forking it for FX
// would be invasive and risky. Instead, we expose a small
// "given a list of (amount, currency) pairs and a base, give
// me the base-currency total" helper, and let each caller adopt
// it in one line where it currently sums `amounts` in USD.
//
// Stale rates
//
// The aggregator returns a "stale" flag so the caller can decide:
//   - report `?fx_stale=true` on the wire → UI shows "≈" badge,
//   - log a Prometheus stale_rate counter,
//   - fail the snapshot write if "no FX implies no NAV".
//
// We never silently drop the bucket — that would shrink reported
// AUM and look like a margin call from the LP's chair.

package fx

import (
	"context"
	"time"
)

// ValueBucket is one (amount, currency) pair the aggregator
// accepts. The caller decides what each bucket means (one
// position, the entire cash side, an accrual, etc.) — the math
// is currency-only.
type ValueBucket struct {
	Amount   float64
	Currency string
	// Label is purely informational; the aggregator echoes
	// missing-rate offenders back through Result.Stale so the
	// caller can render "FX rate missing for HKD position 0700.HK"
	// without re-walking the bucket list.
	Label string
}

// AggregateResult carries the converted total + audit info.
type AggregateResult struct {
	// Total is in BaseCurrency.
	Total float64
	// Stale is true if at least one bucket couldn't be converted
	// (rate missing). The aggregator includes the unconverted
	// amount in Total anyway so the LP doesn't see AUM drop to
	// zero on a transient outage; the UI is responsible for
	// rendering the warning.
	Stale bool
	// MissingPairs lists the unique "FROM/TO" pairs that lacked a
	// rate. Useful for ops alerts.
	MissingPairs []string
	// AsOf is the rate cutoff the caller asked for, echoed back.
	AsOf         time.Time
	BaseCurrency string
}

// Aggregator wraps Convert with the NAV-style API.
type Aggregator struct {
	repo *Repo
}

func NewAggregator(repo *Repo) *Aggregator { return &Aggregator{repo: repo} }

// Aggregate converts each bucket to baseCurrency at `asOf` and
// returns the sum. Empty `asOf` means "now".
//
// Callers that want strict mode (any missing rate fails the whole
// computation) can check Result.Stale and propagate as an error.
// We deliberately don't fail-fast inside the helper because the
// dashboard renders "≈" + a banner, not a 500.
func (a *Aggregator) Aggregate(
	ctx context.Context,
	buckets []ValueBucket,
	baseCurrency string,
	asOf time.Time,
) (AggregateResult, error) {
	res := AggregateResult{
		BaseCurrency: baseCurrency,
		AsOf:         asOf,
	}
	if asOf.IsZero() {
		res.AsOf = time.Now().UTC()
	}
	if a == nil || a.repo == nil {
		// No FX repo → identity. This is the safe default for
		// single-currency funds (legacy USD-only mode).
		for _, b := range buckets {
			if SameCurrency(b.Currency, baseCurrency) || b.Currency == "" {
				res.Total += b.Amount
				continue
			}
			// Without a repo we can't convert — flag stale and
			// add the raw number so AUM doesn't collapse.
			res.Stale = true
			res.MissingPairs = appendPair(res.MissingPairs, b.Currency, baseCurrency)
			res.Total += b.Amount
		}
		return res, nil
	}
	for _, b := range buckets {
		if b.Amount == 0 {
			continue
		}
		converted, _, err := a.repo.Convert(ctx, b.Amount, b.Currency, baseCurrency, res.AsOf)
		if err != nil {
			res.Stale = true
			res.MissingPairs = appendPair(res.MissingPairs, b.Currency, baseCurrency)
			// Best effort: include the unconverted amount so the
			// dashboard doesn't drop to zero on a missing rate.
			res.Total += b.Amount
			continue
		}
		res.Total += converted
	}
	return res, nil
}

func appendPair(s []string, from, to string) []string {
	pair := FormatPair(from, to)
	for _, existing := range s {
		if existing == pair {
			return s
		}
	}
	return append(s, pair)
}
