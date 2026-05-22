package regime

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/fundai/server/internal/ohlc"
)

// ---------------------------------------------------------------------------
// Service: batched + cached classifier
// ---------------------------------------------------------------------------

// Instrument identifies a single (symbol, market) pair to classify.
// Mirrors the minimum fields ohlc.FetchRequest needs — we keep our
// own struct so the wiring layer doesn't have to depend on the
// ohlc package directly.
type Instrument struct {
	Symbol string
	Market string
}

// normalisedKey returns a stable cache key. Two requests that
// differ only in trivial casing / whitespace share a cache slot.
func (i Instrument) normalisedKey() string {
	return strings.ToUpper(strings.TrimSpace(i.Symbol)) + "|" +
		strings.ToLower(strings.TrimSpace(i.Market))
}

// Service is the cached, fetcher-backed wrapper around the
// stateless Classify function. One Service per process; safe for
// concurrent use.
type Service struct {
	fetcher ohlc.Fetcher
	params  Params
	// cacheTTL governs how long a classified Regime stays valid.
	// Regimes shift on the daily timeframe, so the cache is sized
	// for an intraday loop (1h default): far below a regime
	// transition's typical duration, far above a single decision
	// pass's cost.
	cacheTTL time.Duration
	// lookback controls how many bars we ask the fetcher for.
	// Has to clear params.MinBars with a safety margin so a
	// half-filled day doesn't kick everything to Unknown.
	lookback int
	// interval is the bar resolution. Daily is the only sensible
	// choice for the MA50/MA200 cascade; intraday regimes need a
	// different param set we'll add in a follow-up.
	interval ohlc.Interval
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
	// inFlight dedupes concurrent fetches for the same instrument.
	// Without it, an intraday refresh storm could trigger N
	// upstream calls for a single symbol.
	inFlight map[string]chan struct{}
}

type cacheEntry struct {
	regime    Regime
	cachedAt  time.Time
	bars      int
	classifierVersion int
}

// Option tunes the Service constructor without forcing callers to
// fill in defaults they don't care about. Same functional-options
// pattern the rest of the codebase uses.
type Option func(*Service)

// WithParams overrides the default classifier knobs. Pass
// DefaultParams() and tweak the fields you care about.
func WithParams(p Params) Option { return func(s *Service) { s.params = p } }

// WithCacheTTL overrides the regime cache TTL. Pass <= 0 to
// disable caching entirely (every Classify hits the fetcher).
func WithCacheTTL(d time.Duration) Option { return func(s *Service) { s.cacheTTL = d } }

// WithLookback overrides the fetcher's LookbackN. Default is
// max(MinBars * 1.1, 250). Increase when the default tail looks
// thin on illiquid names.
func WithLookback(n int) Option { return func(s *Service) { s.lookback = n } }

// WithInterval overrides the bar interval. Default is daily.
func WithInterval(i ohlc.Interval) Option { return func(s *Service) { s.interval = i } }

// WithClock injects a deterministic clock for tests.
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// NewService wires a Service around the supplied fetcher. A nil
// fetcher is allowed — the service simply returns Unknown for
// every call, which is what the wiring layer wants in builds
// where the OHLC service is disabled.
func NewService(fetcher ohlc.Fetcher, opts ...Option) *Service {
	s := &Service{
		fetcher:  fetcher,
		params:   DefaultParams(),
		cacheTTL: 1 * time.Hour,
		interval: ohlc.IntervalDay,
		now:      func() time.Time { return time.Now().UTC() },
		cache:    make(map[string]cacheEntry),
		inFlight: make(map[string]chan struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.lookback <= 0 {
		s.lookback = s.params.MinBars + 30
		if s.lookback < 250 {
			s.lookback = 250
		}
	}
	return s
}

// ErrFetcherUnavailable is returned by Classify when the underlying
// fetcher returned an error. Surfaced so callers can distinguish
// "no data" from "classifier doesn't think there's a regime".
var ErrFetcherUnavailable = errors.New("regime: fetcher unavailable")

// Classify returns the Regime for a single instrument. Cached
// hits return immediately; misses fetch bars and run the
// classifier. Returns Unknown on any error path; the optional
// error explains why for callers that want to log / metric it.
//
// The error is nil for legitimate Unknown returns (e.g. insufficient
// bars on a newly listed stock) — only fetcher / fatal failures
// produce a non-nil error. This convention lets the wiring layer
// treat (Unknown, nil) as "skip the tag" and (_, err) as "log
// + skip the tag".
func (s *Service) Classify(ctx context.Context, inst Instrument) (Regime, error) {
	if s == nil || s.fetcher == nil {
		return Unknown, nil
	}
	if strings.TrimSpace(inst.Symbol) == "" {
		return Unknown, nil
	}
	key := inst.normalisedKey()
	now := s.now()

	// 1. Cache hit fast path.
	s.mu.Lock()
	if entry, ok := s.cache[key]; ok && s.cacheTTL > 0 && now.Sub(entry.cachedAt) < s.cacheTTL {
		s.mu.Unlock()
		return entry.regime, nil
	}
	// 2. Coalesce in-flight fetches. If somebody else is already
	// fetching this instrument, wait for them and then re-check
	// the cache. Stops the intraday-loop thundering herd.
	if ch, busy := s.inFlight[key]; busy {
		s.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return Unknown, ctx.Err()
		}
		s.mu.Lock()
		if entry, ok := s.cache[key]; ok && s.cacheTTL > 0 && now.Sub(entry.cachedAt) < s.cacheTTL {
			s.mu.Unlock()
			return entry.regime, nil
		}
		// Fell through: the previous holder errored out; try
		// again under our own in-flight slot.
	}
	ch := make(chan struct{})
	s.inFlight[key] = ch
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.inFlight, key)
		s.mu.Unlock()
		close(ch)
	}()

	// 3. Fetch + classify.
	req := ohlc.FetchRequest{
		Symbol:    strings.TrimSpace(inst.Symbol),
		Market:    strings.ToLower(strings.TrimSpace(inst.Market)),
		Interval:  s.interval,
		LookbackN: s.lookback,
		EndTime:   now,
	}
	bars, err := s.fetcher.Fetch(ctx, req)
	if err != nil {
		// Cache nothing on errors — next call gets a retry, but
		// we don't hammer the upstream because the in-flight
		// dedupe is already in place above.
		return Unknown, errFetcherWrap(err)
	}
	r := Classify(bars, s.params)

	s.mu.Lock()
	s.cache[key] = cacheEntry{
		regime:   r,
		cachedAt: now,
		bars:     len(bars),
	}
	s.mu.Unlock()
	return r, nil
}

// ClassifyBatch is the bulk variant. It dedupes the input list
// (so duplicate instruments hit the cache once) and returns the
// result indexed by Instrument.normalisedKey().
//
// On per-instrument errors the entry is omitted from the map
// (rather than recorded as Unknown). This lets the caller
// distinguish "we tried and got nothing" from "we never tried".
// A combined error wraps the first encountered error so callers
// can log a single representative failure; the rest are visible
// only at debug level.
func (s *Service) ClassifyBatch(ctx context.Context, instruments []Instrument) (map[string]Regime, error) {
	out := make(map[string]Regime, len(instruments))
	if s == nil || s.fetcher == nil {
		return out, nil
	}
	seen := make(map[string]struct{}, len(instruments))
	var firstErr error
	for _, inst := range instruments {
		key := inst.normalisedKey()
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		r, err := s.Classify(ctx, inst)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			slog.Debug("regime: classify failed",
				"symbol", inst.Symbol,
				"market", inst.Market,
				"error", err,
			)
			continue
		}
		// Only record known regimes — Unknown means "not enough
		// data", which the caller should treat the same as
		// "not in the map".
		if r.IsKnown() {
			out[key] = r
		}
	}
	return out, firstErr
}

// Lookup is the cheap read-only accessor for already-classified
// instruments. Returns (regime, true) on a fresh cache hit, or
// (Unknown, false) otherwise. Lets the wiring layer pull regime
// tags without triggering a fetch (used by exit-manager wiring
// where the decision-time classification is already cached).
func (s *Service) Lookup(inst Instrument) (Regime, bool) {
	if s == nil {
		return Unknown, false
	}
	key := inst.normalisedKey()
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.cache[key]
	if !ok {
		return Unknown, false
	}
	if s.cacheTTL > 0 && s.now().Sub(entry.cachedAt) >= s.cacheTTL {
		return Unknown, false
	}
	return entry.regime, true
}

// InvalidateAll wipes the cache. Useful for tests + the rare
// operator-triggered re-classification after a calibration change.
func (s *Service) InvalidateAll() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.cache = make(map[string]cacheEntry)
	s.mu.Unlock()
}

func errFetcherWrap(err error) error {
	if err == nil {
		return nil
	}
	// Don't wrap ErrNoData — that's a legitimate "no coverage"
	// signal and the caller should just stamp Unknown.
	if errors.Is(err, ohlc.ErrNoData) || errors.Is(err, ohlc.ErrNoProvider) {
		return nil
	}
	return errors.Join(ErrFetcherUnavailable, err)
}
