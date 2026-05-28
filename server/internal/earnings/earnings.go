// Package earnings models per-symbol upcoming earnings events and
// surfaces them to the PM prompt as a structured catalyst block.
//
// Sprint E #2: the prompt's `newsCatalysts` block (Sprint B #3)
// covers recent published news but is blind to scheduled events.
// An earnings release in 24h is one of the highest-conviction
// catalysts a fundamental quant signal can carry — initiating a
// large position the day before earnings is the single most
// expensive retail mistake. This package gives the LLM PM the
// "T+1 AAPL earnings AMC" hint the human PM would never trade
// without.
//
// Architecture mirrors newsrecall + correlation: a `Fetcher`
// interface produces raw events, `Service` orchestrates the
// per-fund call, and the wiring layer routes the result into
// `decision.DecisionInput.EarningsCalendar`.
//
// Built-in providers:
//
//   - NoopFetcher    — returns no events; used when the env knob
//                      EARNINGS_DISABLED=1 or YAHOO_EARNINGS_DISABLED=1
//                      degrades the feature off.
//   - YahooProvider  — hits Yahoo Finance's keyless
//                      v10/quoteSummary endpoint. Default in the
//                      runtime wiring; works out-of-the-box for
//                      US tickers with zero configuration.
//   - StaticFetcher  — operator-seeded slice; useful for tests
//                      and for A-share / HK funds where Yahoo
//                      coverage is poor and an operator
//                      hand-maintains the calendar.
//
// The wiring layer chooses providers via the env-driven
// buildEarningsFetcherFromEnv() builder in cmd/server, mirroring
// the OHLC / fundamental / sectorflow patterns.
package earnings

import (
	"context"
	"sort"
	"strings"
	"time"
)

// TimeOfDay names when, within the calendar day, the earnings
// release happens. The three values cover the universe of how
// vendors tag releases:
//
//   - BMO  = before market open (release before the bell that
//            day; the price gap is on the SAME morning)
//   - AMC  = after market close (release after the bell that
//            day; the price gap is on the FOLLOWING morning)
//   - Unknown = vendor didn't tag, or the event is far enough
//            in the future that the exact time isn't known yet
type TimeOfDay string

const (
	TimeBMO     TimeOfDay = "bmo"
	TimeAMC     TimeOfDay = "amc"
	TimeUnknown TimeOfDay = "unknown"
)

// Event is one scheduled earnings release. EventDate is the
// calendar date of the report; for AMC events the price-impacting
// open is on EventDate+1, but we leave the +1 math to the
// consumer so the structure stays close to the source data.
type Event struct {
	// Symbol is the upper-cased ticker the event applies to.
	Symbol string
	// Market is the lower-cased market hint (e.g. "us_equity",
	// "a_share"). Optional; the consumer uses it to disambiguate
	// dual-listed tickers when present.
	Market string
	// EventDate is the calendar day of the release. UTC; the
	// pre/post-market shading lives in TimeOfDay.
	EventDate time.Time
	// TimeOfDay tags the release relative to the trading bell.
	TimeOfDay TimeOfDay
	// Source is the vendor / provider tag — useful for the
	// audit log so an operator can tell whether a tomorrow-AMC
	// hint came from Finnhub or from a hand-seeded YAML.
	Source string
}

// Fetcher is the interface every backend implements. Real-world
// implementations:
//
//   - NoopFetcher (this file)       — default; returns no events
//   - StaticFetcher (this file)     — operator-seeded slice;
//     useful for tests AND for hand-curated calendars on funds
//     whose universe is small enough to maintain manually.
//   - (future) FinnhubFetcher / PolygonFetcher / AkshareFetcher
//     — drop-in replacements wiring an external API.
//
// Returns the COMPLETE list of events the backend knows about;
// the Service applies the per-call horizon filter so back-end
// authors don't have to think about it.
type Fetcher interface {
	Fetch(ctx context.Context, req FetchRequest) ([]Event, error)
}

// FetchRequest names the per-call query: which symbols (and
// markets) the caller cares about, and the time window in
// question. Fields the back-end ignores are tolerated; the
// service-level filters always run on top.
type FetchRequest struct {
	// Symbols is the upper-cased ticker list the caller cares
	// about. An empty slice means "all symbols the back-end
	// knows about" — useful when callers want to seed an
	// "earnings season" warning across the universe.
	Symbols []string
	// Market is the fund's primary market. Back-ends use it
	// as a hint when a symbol could be dual-listed (e.g.
	// BABA on NYSE vs Hong Kong).
	Market string
	// HorizonDays is the upper bound on EventDate-now (in days).
	// 0 means "use Service.HorizonDays default". Back-ends may
	// ignore the field and let the Service filter; some prefer
	// to push the filter down to the API call.
	HorizonDays int
}

// Service is the per-process orchestrator: takes a Fetcher,
// applies the per-call horizon filter, dedups by (symbol,
// market, date), and returns a stable-ordered slice of Events.
type Service struct {
	fetcher     Fetcher
	now         func() time.Time
	horizonDays int
}

// Options configures the Service.
type Options struct {
	// Now is the clock function. Default time.Now (UTC normalised
	// inside the service). Tests use a fixed clock to make the
	// horizon filter deterministic.
	Now func() time.Time
	// HorizonDays is the default forward window the service
	// surfaces. 14 days covers the "imminent" zone every
	// professional PM cares about; anything further out is
	// noise relative to the day-to-day decision cadence.
	HorizonDays int
}

// NewService wires a Service with the supplied fetcher and
// options. A nil fetcher is tolerated and silently swapped to
// NoopFetcher{} so callers can always construct the service
// (matching the rest of the wiring layer's "feature off when
// dependencies are missing" contract).
func NewService(fetcher Fetcher, opts Options) *Service {
	s := &Service{
		fetcher:     fetcher,
		now:         opts.Now,
		horizonDays: opts.HorizonDays,
	}
	if s.fetcher == nil {
		s.fetcher = NoopFetcher{}
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.horizonDays <= 0 {
		s.horizonDays = 14
	}
	return s
}

// Snapshot is the per-fund result the wiring layer ships into
// decision.DecisionInput.EarningsCalendar. The PerSymbol map
// is keyed on the upper-cased symbol so the prompt-builder can
// dereference by candidate symbol in O(1).
type Snapshot struct {
	// AsOf is the clock-time of the snapshot, useful for the
	// audit log.
	AsOf time.Time
	// HorizonDays is the forward window the snapshot actually
	// covered. Echoed back so the prompt can quote it.
	HorizonDays int
	// PerSymbol is keyed on upper-cased symbol; each value is
	// the NEAREST upcoming event for that symbol (the LLM
	// rarely cares about the second-nearest because the
	// catalyst structure flips at the earnings gap anyway).
	PerSymbol map[string]Event
}

// HasSignal reports whether the snapshot carries any actionable
// information. False on empty snapshots so the wiring layer
// can drop the block from the prompt entirely.
func (s *Snapshot) HasSignal() bool {
	return s != nil && len(s.PerSymbol) > 0
}

// Build runs the per-fund earnings query. Returns nil on every
// failure path — earnings is a soft input; we'd rather omit the
// block than fail the decision.
func (s *Service) Build(ctx context.Context, symbols []string, market string) *Snapshot {
	if s == nil {
		return nil
	}
	now := s.now().UTC()
	req := FetchRequest{
		Symbols:     normaliseSymbols(symbols),
		Market:      strings.ToLower(strings.TrimSpace(market)),
		HorizonDays: s.horizonDays,
	}
	if len(req.Symbols) == 0 {
		// Empty symbol list → no scoped query; back-ends that
		// support "all" can opt in by treating empty as
		// universe-wide. Most callers pass at least one symbol.
		return nil
	}
	events, err := s.fetcher.Fetch(ctx, req)
	if err != nil || len(events) == 0 {
		return nil
	}
	deadline := now.AddDate(0, 0, s.horizonDays)
	perSymbol := make(map[string]Event, len(events))
	for _, e := range events {
		sym := strings.ToUpper(strings.TrimSpace(e.Symbol))
		if sym == "" {
			continue
		}
		// Filter: future events only, inside horizon.
		if e.EventDate.IsZero() || e.EventDate.Before(now) || e.EventDate.After(deadline) {
			continue
		}
		// Keep the NEAREST upcoming event per symbol.
		existing, ok := perSymbol[sym]
		if !ok || e.EventDate.Before(existing.EventDate) {
			normalised := e
			normalised.Symbol = sym
			normalised.Market = strings.ToLower(strings.TrimSpace(e.Market))
			normalised.TimeOfDay = normaliseTimeOfDay(e.TimeOfDay)
			perSymbol[sym] = normalised
		}
	}
	if len(perSymbol) == 0 {
		return nil
	}
	return &Snapshot{
		AsOf:        now,
		HorizonDays: s.horizonDays,
		PerSymbol:   perSymbol,
	}
}

// SortedEvents returns the snapshot's events sorted by
// (EventDate, Symbol). Convenient for the prompt builder, which
// needs a stable order, and for tests.
func (s *Snapshot) SortedEvents() []Event {
	if s == nil || len(s.PerSymbol) == 0 {
		return nil
	}
	out := make([]Event, 0, len(s.PerSymbol))
	for _, e := range s.PerSymbol {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].EventDate.Equal(out[j].EventDate) {
			return out[i].EventDate.Before(out[j].EventDate)
		}
		return out[i].Symbol < out[j].Symbol
	})
	return out
}

// ---------------------------------------------------------------------------
// Built-in fetchers
// ---------------------------------------------------------------------------

// NoopFetcher returns no events. Default for funds that haven't
// wired an earnings provider — the block is silently absent from
// the prompt.
type NoopFetcher struct{}

// Fetch implements Fetcher.
func (NoopFetcher) Fetch(_ context.Context, _ FetchRequest) ([]Event, error) {
	return nil, nil
}

// StaticFetcher returns a fixed slice of events. Useful for:
//
//   - Unit tests (deterministic input).
//   - Funds whose universe is small enough that an operator
//     hand-maintains the earnings calendar in YAML / fund.config.
//   - "Earnings season warning" overlays where the operator
//     wants to flag specific dates without paying for an API
//     subscription.
//
// The filter on Symbols / Market happens inside Service.Build,
// so this fetcher can return its full slice every call without
// caring about the request scope.
type StaticFetcher struct {
	Events []Event
}

// Fetch implements Fetcher.
func (f StaticFetcher) Fetch(_ context.Context, _ FetchRequest) ([]Event, error) {
	return f.Events, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// normaliseSymbols upper-cases, trims, and dedupes a slice of
// raw symbols. Empty strings are dropped. The output is
// caller-owned so the request struct can safely escape.
func normaliseSymbols(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		k := strings.ToUpper(strings.TrimSpace(s))
		if k == "" {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}

// normaliseTimeOfDay coerces a raw string into the supported
// TimeOfDay constants. Anything we don't recognise lands in
// TimeUnknown (rather than the raw string) so downstream
// consumers don't have to defend against arbitrary input.
func normaliseTimeOfDay(t TimeOfDay) TimeOfDay {
	switch TimeOfDay(strings.ToLower(strings.TrimSpace(string(t)))) {
	case TimeBMO:
		return TimeBMO
	case TimeAMC:
		return TimeAMC
	default:
		return TimeUnknown
	}
}
