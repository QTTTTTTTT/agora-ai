package marketdata

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

type Service struct {
	cfg            Config
	httpClient     *http.Client
	quoteCache     *ttlCache[*QuoteSnapshot]
	newsCache      *ttlCache[[]NewsItem]
	researchCache  *ttlCache[*ResearchContext]
	translator     NewsTranslator
	providerHealth *providerHealthTracker
	// quoteSF coalesces concurrent cache-miss GetQuote calls for the same
	// instrument key into a single upstream fetch. Without it, an SSE
	// pusher + a Dashboard render + a portfolio overlay arriving in the
	// same ~10s cache miss would each issue a separate Yahoo call. The
	// shared key is InstrumentRef.CacheKey().
	quoteSF singleflight.Group
	// rateLimiter paces per-provider call rates so a cold-cache stampede
	// can't trip an upstream IP block. Nil-safe (a nil receiver is a
	// no-op) so the legacy unit tests that build Service{} by hand still
	// work without code changes.
	rateLimiter *providerRateLimiter
	// cryptoWSCache backs the binance / coinbase quote providers when
	// CryptoWSEnabled is true. Lazily initialised so unit tests that do
	// not exercise the WS path don't pay for the allocation.
	cryptoWSCache *cryptoTickerCache
	// cryptoStreamsCancel stops the background WS goroutines started by
	// StartCryptoStreams. Nil when no streams are running.
	cryptoStreamsCancel context.CancelFunc
	cryptoStreamsDone   chan struct{}
	// testProviderOverrides is consulted by quoteProviderByName before any
	// real provider. Tests use it to inject deterministic fake upstreams;
	// production code never sets this field.
	testProviderOverrides map[string]quoteProviderFunc
}

func NewService(cfg Config) *Service {
	normalized := cfg.normalize()
	limits := normalized.ProviderRateLimits
	if limits == nil {
		limits = DefaultProviderRateLimits()
	}
	if spec := strings.TrimSpace(normalized.ProviderRateLimitsSpec); spec != "" {
		limits = ParseProviderRateLimits(spec, limits)
	}
	svc := &Service{
		cfg: normalized,
		httpClient: &http.Client{
			Timeout: normalized.ProviderTimeout,
		},
		quoteCache:    newTTLCache[*QuoteSnapshot](),
		newsCache:     newTTLCache[[]NewsItem](),
		researchCache: newTTLCache[*ResearchContext](),
		translator:    noopTranslator{},
		providerHealth: newProviderHealthTracker(
			normalized.QuoteCircuitFailureThreshold,
			normalized.QuoteCircuitCooldown,
			normalized.QuoteThrottleCooldown,
		),
		rateLimiter: newProviderRateLimiter(limits),
	}
	if normalized.CryptoWSEnabled {
		svc.cryptoWSCache = newCryptoTickerCache()
	}
	return svc
}

// ProviderHealth exposes the per-provider counters and circuit-breaker state
// for observability endpoints. The returned map is a copy and safe to mutate.
func (s *Service) ProviderHealth() map[string]ProviderHealthStats {
	if s == nil {
		return nil
	}
	return s.providerHealth.Snapshot()
}

// StartCryptoStreams launches the Binance and Coinbase websocket clients in
// background goroutines. Each owns its own retry loop and exits cleanly
// when the supplied ctx is cancelled or Close() is called on the service.
//
// Safe to call multiple times: subsequent calls after the first are no-ops.
// When CryptoWSEnabled is false the call also short-circuits silently so
// configuration toggles work without code changes at the call site.
//
// The Binance / Coinbase stream choice is intentional both-on by default:
// the two feeds cover overlapping coin universes but with different uptime
// profiles, and the cache de-duplicates by symbol so either stream can
// satisfy a Quote() call. To run only one, override
// MARKETDATA_BINANCE_WS_SYMBOLS or MARKETDATA_COINBASE_WS_PRODUCTS to an
// empty value.
func (s *Service) StartCryptoStreams(ctx context.Context) {
	if s == nil || !s.cfg.CryptoWSEnabled || s.cryptoWSCache == nil {
		return
	}
	if s.cryptoStreamsCancel != nil {
		return
	}
	streamCtx, cancel := context.WithCancel(ctx)
	s.cryptoStreamsCancel = cancel
	s.cryptoStreamsDone = make(chan struct{})

	binanceSymbols := s.cfg.BinanceWSSymbols
	if len(binanceSymbols) == 0 {
		binanceSymbols = defaultBinanceSymbols
	}
	coinbaseProducts := s.cfg.CoinbaseWSProducts
	if len(coinbaseProducts) == 0 {
		coinbaseProducts = defaultCoinbaseProducts
	}

	binance := newBinanceWSStream(s.cfg.BinanceWSURL, binanceSymbols, s.cryptoWSCache, s.providerHealth, slog.Default())
	coinbase := newCoinbaseWSStream(s.cfg.CoinbaseWSURL, coinbaseProducts, s.cryptoWSCache, s.providerHealth, slog.Default())

	var wg sync.WaitGroup
	if len(binance.symbols) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			binance.Run(streamCtx)
		}()
	}
	if len(coinbase.productIDs) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			coinbase.Run(streamCtx)
		}()
	}
	go func() {
		wg.Wait()
		close(s.cryptoStreamsDone)
	}()
}

// Close stops background goroutines (currently the crypto WS streams) and
// blocks until they have exited or the provided shutdownTimeout has
// elapsed. It is safe to call without a matching StartCryptoStreams.
func (s *Service) Close(shutdownTimeout time.Duration) error {
	if s == nil {
		return nil
	}
	if s.cryptoStreamsCancel == nil {
		return nil
	}
	s.cryptoStreamsCancel()
	if s.cryptoStreamsDone == nil {
		return nil
	}
	if shutdownTimeout <= 0 {
		<-s.cryptoStreamsDone
		s.cryptoStreamsCancel = nil
		return nil
	}
	select {
	case <-s.cryptoStreamsDone:
		s.cryptoStreamsCancel = nil
		return nil
	case <-time.After(shutdownTimeout):
		return errors.New("marketdata: crypto stream shutdown timeout")
	}
}

// CryptoWSSnapshot exposes the in-memory ticker cache for observability
// endpoints. Returns nil when CryptoWSEnabled is false.
func (s *Service) CryptoWSSnapshot() map[string]QuoteSnapshot {
	if s == nil || s.cryptoWSCache == nil {
		return nil
	}
	return s.cryptoWSCache.Snapshot()
}

// SeedQuotesForTesting installs a fixed quote into the in-memory cache
// for each (symbol → price) pair in `quotes`. Intended only for unit
// tests that need deterministic GetQuote responses without spinning up a
// real provider chain. Each entry is keyed by the upper-cased symbol and
// stored under a few common market combinations so callers don't need to
// reconstruct the CacheKey exactly.
//
// Calling on a nil service is a safe no-op.
func (s *Service) SeedQuotesForTesting(quotes map[string]float64) {
	if s == nil || s.quoteCache == nil {
		return
	}
	now := time.Now().UTC()
	for symbol, price := range quotes {
		ref := InstrumentRef{Symbol: symbol}
		snapshot := &QuoteSnapshot{
			Symbol: strings.ToUpper(strings.TrimSpace(symbol)),
			Price:  price,
			AsOf:   now,
			Source: "test-seed",
		}
		// Stamp under a few common market combinations so tests don't
		// need to reconstruct the exact CacheKey.
		variants := []InstrumentRef{
			ref,
			{Symbol: symbol, Market: "usstock", AssetClass: "equity", Exchange: "NASDAQ"},
			{Symbol: symbol, Market: "usstock", AssetClass: "equity"},
			{Symbol: symbol, Market: "usstock"},
		}
		for _, variant := range variants {
			s.quoteCache.Set(variant.CacheKey(), snapshot, s.cfg.QuoteTTL, now)
		}
	}
}

// WithTranslator swaps in a configured translator implementation. Pass
// noopTranslator{} or nil to disable translation entirely. Safe to call
// at startup before the service is shared with goroutines.
func (s *Service) WithTranslator(translator NewsTranslator) *Service {
	if s == nil {
		return nil
	}
	if translator == nil {
		s.translator = noopTranslator{}
	} else {
		s.translator = translator
	}
	return s
}

func (s *Service) Enabled() bool {
	return s != nil && s.cfg.Enabled()
}

func (s *Service) GetQuote(ctx context.Context, instrument InstrumentRef) (*QuoteSnapshot, error) {
	if s == nil || !s.cfg.Enabled() {
		return nil, ErrQuoteUnavailable
	}
	cacheKey := instrument.CacheKey()
	now := time.Now().UTC()
	if cached, ok := s.quoteCache.Get(cacheKey, now); ok && cached != nil {
		// IsStale is a function of (now, AsOf), so the cached value may be
		// out of date even when the cache entry itself is still warm.
		// Recompute on every read so consumers always get the current truth.
		fresh := *cached
		fresh.IsStale = isQuoteStale(fresh.AsOf, now, s.cfg.StaleQuoteAfter)
		return &fresh, nil
	}

	// Coalesce concurrent misses. Without singleflight, N parallel
	// GetQuote("MU") calls during the ~10s cache-miss window all hit
	// upstream; with it, only one does and the rest share its result.
	// We use the InstrumentRef.CacheKey() as the shared key because that's
	// what the cache itself indexes on, so an SSE pusher (key A) and a
	// CN-mirror request for the same upstream (key B) are still separate.
	value, err, _ := s.quoteSF.Do(cacheKey, func() (any, error) {
		// Re-check the cache inside the singleflight callback so the
		// follow-up callers (waiting on the leader) hit the freshly
		// populated entry instead of triggering another provider call.
		now := time.Now().UTC()
		if cached, ok := s.quoteCache.Get(cacheKey, now); ok && cached != nil {
			fresh := *cached
			fresh.IsStale = isQuoteStale(fresh.AsOf, now, s.cfg.StaleQuoteAfter)
			return &fresh, nil
		}
		return s.fetchQuoteFromProviders(ctx, instrument, now)
	})
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, ErrQuoteUnavailable
	}
	quote, ok := value.(*QuoteSnapshot)
	if !ok || quote == nil {
		return nil, ErrQuoteUnavailable
	}
	// Singleflight returns the same pointer to every shared caller, so
	// recompute IsStale here too — by the time a follower receives the
	// value the AsOf might already be older than StaleQuoteAfter.
	clone := *quote
	clone.IsStale = isQuoteStale(clone.AsOf, time.Now().UTC(), s.cfg.StaleQuoteAfter)
	return &clone, nil
}

// fetchQuoteFromProviders walks the configured provider chain for an
// instrument, paces each call through the per-provider rate limiter, and
// stamps the cache with the first non-empty result. Extracted so the
// singleflight callback in GetQuote stays small and so individual provider
// branches are unit-testable.
func (s *Service) fetchQuoteFromProviders(ctx context.Context, instrument InstrumentRef, now time.Time) (*QuoteSnapshot, error) {
	cacheKey := instrument.CacheKey()
	providers := s.namedQuoteProviders(instrument)
	if len(providers) == 0 {
		slog.Warn("marketdata: no quote provider available",
			"symbol", instrument.NormalizedSymbol(),
			"market", instrument.Market,
			"assetClass", instrument.AssetClass,
		)
		return nil, fmt.Errorf("%w: no provider configured for market %q", ErrQuoteUnavailable, instrument.Market)
	}

	var errs []string
	for _, np := range providers {
		if open, retryAt := s.providerHealth.shouldSkip(np.name, now); open {
			slog.Debug("marketdata: provider circuit open; skipping",
				"provider", np.name,
				"symbol", instrument.NormalizedSymbol(),
				"market", instrument.Market,
				"retryAt", retryAt,
			)
			errs = append(errs, np.name+": circuit open")
			continue
		}
		// Pace the call through the per-provider token bucket. We use
		// Wait (not Allow) so a tight cache-miss loop blocks the slow
		// caller instead of dropping the request — the caller already
		// has the singleflight follower semantics so blocking briefly is
		// preferable to skipping a provider in the chain.
		if err := s.rateLimiter.Wait(ctx, np.name); err != nil {
			errs = append(errs, np.name+": ratelimit: "+err.Error())
			continue
		}
		startedAt := time.Now()
		quote, err := np.fn(ctx, instrument)
		latency := time.Since(startedAt)
		if err != nil {
			s.providerHealth.recordFailure(np.name, err, now, latency)
			msg := np.name + ": " + err.Error()
			slog.Warn("marketdata: quote provider failed",
				"provider", np.name,
				"symbol", instrument.NormalizedSymbol(),
				"market", instrument.Market,
				"latency_ms", latency.Milliseconds(),
				"throttled", isThrottleError(err),
				"error", err,
			)
			errs = append(errs, msg)
			continue
		}
		if quote == nil || quote.Price <= 0 {
			s.providerHealth.recordFailure(np.name, fmt.Errorf("empty price"), now, latency)
			msg := np.name + ": empty price"
			slog.Warn("marketdata: quote provider returned empty price",
				"provider", np.name,
				"symbol", instrument.NormalizedSymbol(),
				"market", instrument.Market,
				"latency_ms", latency.Milliseconds(),
			)
			errs = append(errs, msg)
			continue
		}
		quote.Symbol = firstNonEmpty(quote.Symbol, instrument.NormalizedSymbol())
		quote.InstrumentKey = firstNonEmpty(quote.InstrumentKey, instrument.InstrumentKey)
		quote.Market = firstNonEmpty(quote.Market, strings.TrimSpace(instrument.Market))
		quote.Exchange = firstNonEmpty(quote.Exchange, strings.TrimSpace(instrument.Exchange))
		quote.AssetClass = firstNonEmpty(quote.AssetClass, strings.TrimSpace(instrument.AssetClass))
		quote.QuoteCurrency = firstNonEmpty(quote.QuoteCurrency, strings.TrimSpace(instrument.QuoteCurrency))
		if quote.AsOf.IsZero() {
			quote.AsOf = now
		}
		quote.IsStale = isQuoteStale(quote.AsOf, now, s.cfg.StaleQuoteAfter)
		s.providerHealth.recordSuccess(np.name, latency)
		slog.Debug("marketdata: quote provider hit",
			"provider", np.name,
			"symbol", instrument.NormalizedSymbol(),
			"market", instrument.Market,
			"latency_ms", latency.Milliseconds(),
			"isStale", quote.IsStale,
		)
		s.quoteCache.Set(cacheKey, quote, s.adaptiveQuoteTTL(instrument, now), now)
		return quote, nil
	}
	slog.Warn("marketdata: all quote providers failed",
		"symbol", instrument.NormalizedSymbol(),
		"market", instrument.Market,
		"providerCount", len(providers),
		"errors", strings.Join(errs, "; "),
	)
	if len(errs) == 0 {
		return nil, ErrQuoteUnavailable
	}
	return nil, fmt.Errorf("%w: %s", ErrQuoteUnavailable, strings.Join(errs, "; "))
}

func (s *Service) GetQuotes(ctx context.Context, instruments []InstrumentRef) map[string]*QuoteSnapshot {
	results := make(map[string]*QuoteSnapshot, len(instruments))
	for _, instrument := range instruments {
		if strings.TrimSpace(instrument.Symbol) == "" {
			continue
		}
		quote, err := s.GetQuote(ctx, instrument)
		if err != nil || quote == nil {
			continue
		}
		results[strings.ToUpper(strings.TrimSpace(instrument.Symbol))] = quote
	}
	return results
}

func (s *Service) GetNews(ctx context.Context, instrument InstrumentRef, limit int) ([]NewsItem, error) {
	items, _, err := s.GetNewsWithNotes(ctx, instrument, limit)
	return items, err
}

func (s *Service) GetNewsWithNotes(ctx context.Context, instrument InstrumentRef, limit int) ([]NewsItem, []string, error) {
	if s == nil || !s.cfg.Enabled() {
		return nil, nil, ErrNewsUnavailable
	}
	if limit <= 0 {
		limit = 5
	}
	cacheKey := instrument.CacheKey() + fmt.Sprintf("|news|%d", limit)
	if s.cfg.NewsHybridEnabled {
		cacheKey += "|hybrid"
	}
	now := time.Now().UTC()
	if cached, ok := s.newsCache.Get(cacheKey, now); ok && len(cached) > 0 {
		return cached, nil, nil
	}
	if s.cfg.NewsHybridEnabled {
		items, notes, err := s.fetchHybridNews(ctx, instrument, limit)
		if err == nil && len(items) > 0 {
			translated := s.translateNewsItems(ctx, items)
			s.newsCache.Set(cacheKey, translated, s.adaptiveNewsTTL(now), now)
			return translated, notes, nil
		}
		if err != nil {
			return nil, notes, err
		}
		return nil, notes, ErrNewsUnavailable
	}
	providers := s.newsProviders(instrument)
	if len(providers) == 0 {
		return nil, nil, ErrNewsUnavailable
	}
	var errs []string
	for _, provider := range providers {
		providerCtx, cancel := s.providerContext(ctx)
		items, err := provider(providerCtx, instrument, limit)
		cancel()
		if err != nil {
			errs = append(errs, userVisibleResearchNote(err, "news temporarily unavailable"))
			continue
		}
		if len(items) == 0 {
			errs = append(errs, "news temporarily unavailable")
			continue
		}
		translated := s.translateNewsItems(ctx, items)
		s.newsCache.Set(cacheKey, translated, s.adaptiveNewsTTL(now), now)
		return translated, errs, nil
	}
	if len(errs) == 0 {
		return nil, nil, ErrNewsUnavailable
	}
	return nil, errs, fmt.Errorf("%w: %s", ErrNewsUnavailable, strings.Join(errs, "; "))
}

func (s *Service) GetResearchContext(ctx context.Context, instrument InstrumentRef, benchmark *InstrumentRef, limit int) (*ResearchContext, error) {
	if s == nil || !s.cfg.Enabled() {
		return nil, ErrQuoteUnavailable
	}
	cacheKey := instrument.CacheKey()
	if benchmark != nil {
		cacheKey += "|benchmark|" + benchmark.CacheKey()
	}
	now := time.Now().UTC()
	if cached, ok := s.researchCache.Get(cacheKey, now); ok && cached != nil {
		return cached, nil
	}

	var (
		quote          *QuoteSnapshot
		quoteErr       error
		benchmarkQuote *QuoteSnapshot
		benchmarkErr   error
		news           []NewsItem
		newsErr        error
	)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		quoteCtx, cancel := s.providerContext(ctx)
		defer cancel()
		quote, quoteErr = s.GetQuote(quoteCtx, instrument)
	}()
	go func() {
		defer wg.Done()
		newsCtx, cancel := s.providerContext(ctx)
		defer cancel()
		news, newsErr = s.GetNews(newsCtx, instrument, limit)
	}()
	if benchmark != nil && strings.TrimSpace(benchmark.Symbol) != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			benchmarkCtx, cancel := s.providerContext(ctx)
			defer cancel()
			benchmarkQuote, benchmarkErr = s.GetQuote(benchmarkCtx, *benchmark)
		}()
	}
	wg.Wait()

	contextValue := &ResearchContext{
		Instrument:     instrument,
		Quote:          quote,
		News:           news,
		BenchmarkQuote: benchmarkQuote,
		Signals:        s.technicalSignals(quote, benchmarkQuote),
		Summary:        buildResearchSummary(instrument, quote, benchmark, benchmarkQuote, news),
		GeneratedAt:    now,
	}
	if quoteErr != nil {
		contextValue.ProviderNotes = append(contextValue.ProviderNotes, userVisibleResearchNote(quoteErr, "quote unavailable"))
	}
	if quote != nil && quote.IsStale {
		contextValue.ProviderNotes = append(contextValue.ProviderNotes, staleQuoteNote(quote, now))
	}
	if newsErr != nil {
		contextValue.ProviderNotes = append(contextValue.ProviderNotes, userVisibleResearchNote(newsErr, "news temporarily unavailable"))
	}
	if benchmark != nil && strings.TrimSpace(benchmark.Symbol) != "" && benchmarkErr != nil {
		contextValue.ProviderNotes = append(contextValue.ProviderNotes, userVisibleResearchNote(benchmarkErr, "benchmark unavailable"))
	}
	if benchmarkQuote != nil && benchmarkQuote.IsStale {
		contextValue.ProviderNotes = append(contextValue.ProviderNotes, staleQuoteNote(benchmarkQuote, now))
	}
	if benchmark != nil && benchmarkQuote == nil {
		contextValue.ProviderNotes = append(contextValue.ProviderNotes, "benchmark unavailable")
	}
	if quote == nil && benchmarkQuote == nil && len(news) == 0 {
		if len(contextValue.ProviderNotes) == 0 {
			contextValue.ProviderNotes = append(contextValue.ProviderNotes, "quote unavailable")
		}
		contextValue.Summary = buildResearchSummary(instrument, nil, nil, nil, nil)
	}
	if len(contextValue.ProviderNotes) > 0 {
		contextValue.ProviderNotes = dedupeStrings(contextValue.ProviderNotes)
	}
	cacheTTL := s.cfg.NewsTTL
	if s.cfg.QuoteTTL < cacheTTL {
		cacheTTL = s.cfg.QuoteTTL
	}
	if cacheTTL <= 0 {
		cacheTTL = 10 * time.Second
	}
	s.researchCache.Set(cacheKey, contextValue, cacheTTL, now)
	return contextValue, nil
}

type quoteProviderFunc func(context.Context, InstrumentRef) (*QuoteSnapshot, error)
type newsProviderFunc func(context.Context, InstrumentRef, int) ([]NewsItem, error)

func (s *Service) quoteProviders(instrument InstrumentRef) []quoteProviderFunc {
	providerNames := s.providerNamesForMarket(instrument)
	providers := make([]quoteProviderFunc, 0, len(providerNames))
	for _, name := range providerNames {
		if provider := s.quoteProviderByName(name); provider != nil {
			providers = append(providers, provider)
		}
	}
	return providers
}

// providerNamesForMarket resolves the ordered list of provider names for the
// given instrument with the following precedence:
//
//  1. Per-market override (Config.QuoteProvidersByMarket[market]) if set, then
//     append any provider names from the global Config.QuoteProviders that
//     aren't already in the per-market list (so a global "always include this"
//     hint is still honoured).
//  2. Global Config.QuoteProviders if set.
//  3. defaultQuoteProviderOrder(instrument) — the built-in market-aware chain.
//
// The default chain is always appended at the tail (deduped) so that an
// incomplete operator config can never accidentally exclude the system's
// well-known fallbacks.
func (s *Service) providerNamesForMarket(instrument InstrumentRef) []string {
	defaultOrder := s.defaultQuoteProviderOrder(instrument)
	marketKey := normalizeMarket(instrument.Market, instrument.AssetClass)
	if perMarket, ok := s.cfg.QuoteProvidersByMarket[marketKey]; ok && len(perMarket) > 0 {
		merged := appendUniqueProviderNames(nil, perMarket...)
		merged = appendUniqueProviderNames(merged, s.cfg.QuoteProviders...)
		merged = appendUniqueProviderNames(merged, defaultOrder...)
		return merged
	}
	if len(s.cfg.QuoteProviders) > 0 {
		return appendUniqueProviderNames(s.cfg.QuoteProviders, defaultOrder...)
	}
	return defaultOrder
}

type namedQuoteProvider struct {
	name string
	fn   quoteProviderFunc
}

func (s *Service) namedQuoteProviders(instrument InstrumentRef) []namedQuoteProvider {
	// When test overrides are present we only use them: tests inject a fake
	// chain to assert resilience behaviour without invoking real upstreams.
	if len(s.testProviderOverrides) > 0 {
		providers := make([]namedQuoteProvider, 0, len(s.testProviderOverrides))
		for _, name := range s.cfg.QuoteProviders {
			if fn, ok := s.testProviderOverrides[name]; ok && fn != nil {
				providers = append(providers, namedQuoteProvider{name: name, fn: fn})
			}
		}
		return providers
	}
	providerNames := s.providerNamesForMarket(instrument)
	providers := make([]namedQuoteProvider, 0, len(providerNames))
	for _, name := range providerNames {
		if provider := s.quoteProviderByName(name); provider != nil {
			providers = append(providers, namedQuoteProvider{name: name, fn: provider})
		}
	}
	return providers
}

func (s *Service) defaultQuoteProviderOrder(instrument InstrumentRef) []string {
	providers := make([]string, 0, 5)
	normalizedMarket := normalizeMarket(instrument.Market, instrument.AssetClass)
	switch normalizedMarket {
	case "cnstock":
		if s.cfg.quoteProviderEnabled("tencent") {
			providers = append(providers, "tencent")
		}
		if s.cfg.quoteProviderEnabled("china-stock") {
			providers = append(providers, "china-stock")
		}
		if s.cfg.quoteProviderEnabled("akshare") {
			providers = append(providers, "akshare")
		}
	case "hkstock":
		if s.cfg.quoteProviderEnabled("tencent") {
			providers = append(providers, "tencent")
		}
		if s.cfg.quoteProviderEnabled("yahoo") {
			providers = append(providers, "yahoo")
		}
	case "usstock":
		if s.cfg.quoteProviderEnabled("yahoo") {
			providers = append(providers, "yahoo")
		}
	case "futures":
		// Chinese futures (SHFE / DCE / CZCE / INE) are best served by
		// akshare's `futures_zh_spot` MCP; the global akshare-mcp container
		// exposes them through the standard /api/market/price path. Yahoo
		// covers CME / COMEX / NYMEX symbols (e.g. "GC=F", "CL=F"), so we
		// keep it as a secondary fallback for global futures.
		if s.cfg.quoteProviderEnabled("akshare") {
			providers = append(providers, "akshare")
		}
		if s.cfg.quoteProviderEnabled("china-stock") {
			providers = append(providers, "china-stock")
		}
		if s.cfg.quoteProviderEnabled("yahoo") {
			providers = append(providers, "yahoo")
		}
	case "crypto":
		// Tick-level real-time data comes from Binance and Coinbase
		// websocket feeds (free, keyless). They serve from an in-memory
		// cache populated by background goroutines so Quote() never
		// blocks on a network round-trip. CoinGecko handles the long-tail
		// altcoins via REST and is rate-limited (30 req/min on the free
		// plan); Yahoo keeps fallback duty for the BTC-USD / ETH-USD
		// majors when both WS feeds are unavailable.
		if s.cfg.quoteProviderEnabled("binance") {
			providers = append(providers, "binance")
		}
		if s.cfg.quoteProviderEnabled("coinbase") {
			providers = append(providers, "coinbase")
		}
		if s.cfg.quoteProviderEnabled("coingecko") {
			providers = append(providers, "coingecko")
		}
		if s.cfg.quoteProviderEnabled("yahoo") {
			providers = append(providers, "yahoo")
		}
	}
	if s.cfg.quoteProviderEnabled("quantdinger") {
		providers = append(providers, "quantdinger")
	}
	if normalizedMarket != "cnstock" && normalizedMarket != "futures" {
		if s.cfg.quoteProviderEnabled("china-stock") {
			providers = append(providers, "china-stock")
		}
		if s.cfg.quoteProviderEnabled("akshare") {
			providers = append(providers, "akshare")
		}
	}
	if normalizedMarket != "usstock" && normalizedMarket != "futures" && normalizedMarket != "hkstock" && normalizedMarket != "crypto" {
		if s.cfg.quoteProviderEnabled("yahoo") {
			providers = append(providers, "yahoo")
		}
	}
	return appendUniqueProviderNames(nil, providers...)
}

func (s *Service) quoteProviderByName(name string) quoteProviderFunc {
	if override, ok := s.testProviderOverrides[name]; ok && override != nil {
		return override
	}
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "china-stock":
		if s.cfg.quoteProviderEnabled("china-stock") {
			return s.mcpQuoteProvider("china-stock", s.cfg.ChinaStockURL)
		}
	case "akshare":
		if s.cfg.quoteProviderEnabled("akshare") {
			return s.mcpQuoteProvider("akshare", s.cfg.AkshareURL)
		}
	case "quantdinger":
		if s.cfg.quoteProviderEnabled("quantdinger") {
			return s.quantDingerQuoteProvider()
		}
	case "tencent":
		if s.cfg.quoteProviderEnabled("tencent") {
			return s.tencentQuoteProvider()
		}
	case "yahoo":
		if s.cfg.quoteProviderEnabled("yahoo") {
			return s.yahooQuoteProvider()
		}
	case "coingecko":
		if s.cfg.quoteProviderEnabled("coingecko") {
			return s.coingeckoQuoteProvider()
		}
	case "binance":
		if s.cfg.quoteProviderEnabled("binance") && s.cryptoWSCache != nil {
			return binanceQuoteProvider(s.cryptoWSCache, s.cfg.CryptoWSStaleAfter)
		}
	case "coinbase":
		if s.cfg.quoteProviderEnabled("coinbase") && s.cryptoWSCache != nil {
			return coinbaseQuoteProvider(s.cryptoWSCache, s.cfg.CryptoWSStaleAfter)
		}
	}
	return nil
}

func (s *Service) newsProviders(instrument InstrumentRef) []newsProviderFunc {
	providerNames := s.cfg.NewsProviders
	if len(providerNames) == 0 {
		providerNames = s.defaultNewsProviderOrder(instrument)
	} else {
		providerNames = appendUniqueProviderNames(providerNames, s.defaultNewsProviderOrder(instrument)...)
	}
	providers := make([]newsProviderFunc, 0, len(providerNames))
	for _, name := range providerNames {
		if provider := s.newsProviderByName(name); provider != nil {
			providers = append(providers, provider)
		}
	}
	return providers
}

func (s *Service) defaultNewsProviderOrder(instrument InstrumentRef) []string {
	providers := make([]string, 0, 6)
	if normalizeMarket(instrument.Market, instrument.AssetClass) == "cnstock" {
		if s.cfg.newsProviderEnabled("eastmoney") {
			providers = append(providers, "eastmoney")
		}
		if s.cfg.newsProviderEnabled("sina") {
			providers = append(providers, "sina")
		}
	}
	for _, name := range []string{"local-search", "web-search", "serpapi", "tavily"} {
		if s.cfg.newsProviderEnabled(name) {
			providers = append(providers, name)
		}
	}
	return providers
}

func (s *Service) newsProviderByName(name string) newsProviderFunc {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "local-search":
		if s.cfg.newsProviderEnabled("local-search") {
			return s.localSearchNewsProvider()
		}
	case "web-search":
		if s.cfg.newsProviderEnabled("web-search") {
			return func(ctx context.Context, instrument InstrumentRef, limit int) ([]NewsItem, error) {
				return s.fetchSearchNewsAt(ctx, s.cfg.WebSearchURL, "web-search", instrument, limit)
			}
		}
	case "serpapi":
		if s.cfg.newsProviderEnabled("serpapi") {
			return s.fetchSerpAPINews
		}
	case "tavily":
		if s.cfg.newsProviderEnabled("tavily") {
			return s.fetchTavilyNews
		}
	case "eastmoney":
		if s.cfg.newsProviderEnabled("eastmoney") {
			return s.eastmoneyNewsProvider()
		}
	case "sina":
		if s.cfg.newsProviderEnabled("sina") {
			return s.sinaNewsProvider()
		}
	}
	return nil
}

func (s *Service) quantDingerQuoteProvider() quoteProviderFunc {
	return func(ctx context.Context, instrument InstrumentRef) (*QuoteSnapshot, error) {
		market := quantDingerMarket(instrument)
		if market == "" {
			return nil, fmt.Errorf("quantdinger: unsupported market %q", instrument.Market)
		}
		baseURL := strings.TrimRight(s.cfg.QuantDingerURL, "/")
		endpoint, err := url.Parse(baseURL + "/api/market/price")
		if err != nil {
			return nil, fmt.Errorf("quantdinger: parse url: %w", err)
		}
		query := endpoint.Query()
		query.Set("market", market)
		query.Set("symbol", quantDingerSymbol(instrument))
		endpoint.RawQuery = query.Encode()
		body, err := s.fetchJSON(ctx, endpoint.String())
		if err != nil {
			return nil, fmt.Errorf("quantdinger: %w", err)
		}
		data := unwrapData(body)
		price := firstPositiveFloat(data, "price", "lastPrice", "last", "close", "c")
		if price <= 0 {
			return nil, fmt.Errorf("quantdinger: empty price")
		}
		quote := &QuoteSnapshot{
			Symbol:        stringValue(data, "symbol"),
			InstrumentKey: instrument.InstrumentKey,
			Market:        instrument.Market,
			Exchange:      instrument.Exchange,
			AssetClass:    instrument.AssetClass,
			Price:         price,
			AsOf:          time.Now().UTC(),
			Source:        "quantdinger",
			QuoteCurrency: instrument.QuoteCurrency,
		}
		quote.Bid = firstPositiveFloat(data, "bid", "b")
		quote.Ask = firstPositiveFloat(data, "ask", "a")
		quote.Volume = int64(firstPositiveFloat(data, "volume", "v"))
		return quote, nil
	}
}

func (s *Service) mcpQuoteProvider(source, baseURL string) quoteProviderFunc {
	candidates := []struct {
		path   string
		params func(InstrumentRef) url.Values
	}{
		{path: "/api/market/price", params: func(instrument InstrumentRef) url.Values {
			q := url.Values{}
			q.Set("symbol", instrument.NormalizedSymbol())
			if market := quantDingerMarket(instrument); market != "" {
				q.Set("market", market)
			}
			return q
		}},
		{path: "/price", params: func(instrument InstrumentRef) url.Values {
			q := url.Values{}
			q.Set("symbol", instrument.NormalizedSymbol())
			return q
		}},
		{path: "/quote", params: func(instrument InstrumentRef) url.Values {
			q := url.Values{}
			q.Set("symbol", instrument.NormalizedSymbol())
			return q
		}},
		{path: "/quotes", params: func(instrument InstrumentRef) url.Values {
			q := url.Values{}
			q.Set("symbols", instrument.NormalizedSymbol())
			return q
		}},
		{path: "/ticker", params: func(instrument InstrumentRef) url.Values {
			q := url.Values{}
			q.Set("symbol", instrument.NormalizedSymbol())
			return q
		}},
	}
	return func(ctx context.Context, instrument InstrumentRef) (*QuoteSnapshot, error) {
		var errs []string
		for _, candidate := range candidates {
			endpoint, err := buildEndpoint(baseURL, candidate.path, candidate.params(instrument))
			if err != nil {
				errs = append(errs, err.Error())
				continue
			}
			body, err := s.fetchJSON(ctx, endpoint)
			if err != nil {
				errs = append(errs, err.Error())
				continue
			}
			data := unwrapData(body)
			price := firstPositiveFloat(data, "price", "lastPrice", "last", "close", "c", "current", "value")
			if price <= 0 {
				errs = append(errs, source+": empty price via "+candidate.path)
				continue
			}
			quote := &QuoteSnapshot{
				Symbol:        firstNonEmpty(stringValue(data, "symbol"), instrument.NormalizedSymbol()),
				InstrumentKey: instrument.InstrumentKey,
				Market:        instrument.Market,
				Exchange:      instrument.Exchange,
				AssetClass:    instrument.AssetClass,
				Price:         price,
				Bid:           firstPositiveFloat(data, "bid", "b"),
				Ask:           firstPositiveFloat(data, "ask", "a"),
				Volume:        int64(firstPositiveFloat(data, "volume", "v")),
				QuoteCurrency: firstNonEmpty(stringValue(data, "currency"), instrument.QuoteCurrency),
				AsOf:          firstTimeValue(data, "asOf", "timestamp", "time", "updatedAt"),
				Source:        source,
			}
			if quote.AsOf.IsZero() {
				quote.AsOf = time.Now().UTC()
			}
			return quote, nil
		}
		return nil, fmt.Errorf("%s: %s", source, strings.Join(errs, "; "))
	}
}

func (s *Service) fetchSearchNewsAt(ctx context.Context, baseURL, source string, instrument InstrumentRef, limit int) ([]NewsItem, error) {
	queryText := searchQueryForNews(instrument)
	locale := webSearchLocaleFor(ctx, queryText, instrument)
	return s.fetchSearchNewsWithLocale(ctx, baseURL, source, instrument, limit, locale)
}

// fetchSearchNewsWithLocale is the variant used by the hybrid aggregator to
// force a specific Google News locale (e.g. en-US even on an A-share instrument
// so we can surface English macro coverage as a complement to the native ZH
// coverage). It bypasses the context+instrument heuristics intentionally.
func (s *Service) fetchSearchNewsWithLocale(ctx context.Context, baseURL, source string, instrument InstrumentRef, limit int, locale webSearchLocale) ([]NewsItem, error) {
	queryText := searchQueryForNews(instrument)
	candidates := []string{
		"/search",
		"/api/search",
		"/news/search",
		"/api/news/search",
		"/api/web/search",
	}
	var errs []string
	for _, path := range candidates {
		params := url.Values{
			"q":     []string{queryText},
			"query": []string{queryText},
			"limit": []string{strconv.Itoa(limit)},
		}
		if locale.hl != "" {
			params.Set("hl", locale.hl)
		}
		if locale.gl != "" {
			params.Set("gl", locale.gl)
		}
		if locale.ceid != "" {
			params.Set("ceid", locale.ceid)
		}
		endpoint, err := buildEndpoint(baseURL, path, params)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		body, err := s.fetchJSON(ctx, endpoint)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		items := parseNewsItems(unwrapData(body), instrument, limit)
		if len(items) == 0 {
			errs = append(errs, source+": empty news via "+path)
			continue
		}
		return tagNewsItemsWithLanguage(withNewsSource(items, source), locale.newsLanguage()), nil
	}
	return nil, fmt.Errorf("%s: %w: %s", source, ErrNewsUnavailable, strings.Join(errs, "; "))
}

func (s *Service) fetchSerpAPINews(ctx context.Context, instrument InstrumentRef, limit int) ([]NewsItem, error) {
	return s.fetchLocalSerpAPINews(ctx, firstNonEmpty(s.cfg.SerpAPIBaseURL, "https://serpapi.com"), newRotatingKeySelector(s.cfg.SerpAPIKeys), instrument, limit)
}

func (s *Service) fetchTavilyNews(ctx context.Context, instrument InstrumentRef, limit int) ([]NewsItem, error) {
	return s.fetchLocalTavilyNews(ctx, firstNonEmpty(s.cfg.TavilyBaseURL, "https://api.tavily.com"), newRotatingKeySelector(s.cfg.TavilyAPIKeys), instrument, limit)
}

func withNewsSource(items []NewsItem, source string) []NewsItem {
	for i := range items {
		if strings.TrimSpace(items[i].Source) == "" {
			items[i].Source = source
		}
	}
	return items
}

func (s *Service) providerContext(parent context.Context) (context.Context, context.CancelFunc) {
	if s == nil || s.cfg.ProviderTimeout <= 0 {
		return context.WithCancel(parent)
	}
	if deadline, ok := parent.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining > 0 && remaining <= s.cfg.ProviderTimeout {
			return context.WithCancel(parent)
		}
	}
	return context.WithTimeout(parent, s.cfg.ProviderTimeout)
}

func appendUniqueProviderNames(existing []string, values ...string) []string {
	result := make([]string, 0, len(existing)+len(values))
	seen := make(map[string]struct{}, len(existing)+len(values))
	for _, value := range existing {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		result = append(result, normalized)
	}
	for _, value := range values {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		result = append(result, normalized)
	}
	return result
}

func (s *Service) technicalSignals(quote *QuoteSnapshot, benchmark *QuoteSnapshot) []string {
	if quote == nil {
		return nil
	}
	signals := []string{fmt.Sprintf("latest price %.4f", quote.Price)}
	if quote.Bid > 0 && quote.Ask > 0 && quote.Ask >= quote.Bid {
		spread := quote.Ask - quote.Bid
		signals = append(signals, fmt.Sprintf("bid/ask %.4f/%.4f (spread %.4f)", quote.Bid, quote.Ask, spread))
	}
	if benchmark != nil && benchmark.Price > 0 {
		relative := ((quote.Price / benchmark.Price) - 1) * 100
		signals = append(signals, fmt.Sprintf("vs benchmark %.2f%%", relative))
	}
	return signals
}

func buildResearchSummary(instrument InstrumentRef, quote *QuoteSnapshot, benchmarkRef *InstrumentRef, benchmark *QuoteSnapshot, news []NewsItem) string {
	parts := []string{fmt.Sprintf("%s market context", firstNonEmpty(instrument.NormalizedSymbol(), instrument.Symbol))}
	if quote != nil {
		parts = append(parts, fmt.Sprintf("price %.4f (%s)", quote.Price, quote.Source))
	}
	benchmarkSymbol := ""
	if benchmarkRef != nil {
		benchmarkSymbol = benchmarkRef.NormalizedSymbol()
	}
	if benchmarkSymbol == "" && benchmark != nil {
		benchmarkSymbol = strings.ToUpper(strings.TrimSpace(benchmark.Symbol))
	}
	if benchmark != nil && benchmark.Price > 0 {
		if benchmarkSymbol != "" {
			parts = append(parts, fmt.Sprintf("benchmark %s %.4f", benchmarkSymbol, benchmark.Price))
		} else {
			parts = append(parts, fmt.Sprintf("benchmark %.4f", benchmark.Price))
		}
	}
	if len(news) > 0 {
		parts = append(parts, fmt.Sprintf("%d news items", len(news)))
	}
	return strings.Join(parts, " · ")
}

func userVisibleResearchNote(err error, fallback string) string {
	if err == nil {
		return ""
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	if strings.Contains(message, "news") {
		return "news temporarily unavailable"
	}
	if strings.Contains(message, "benchmark") {
		return "benchmark unavailable"
	}
	if strings.Contains(message, "quote") {
		return "quote unavailable"
	}
	return fallback
}

func IsTickerLikeSymbol(symbol string) bool {
	trimmed := strings.TrimSpace(symbol)
	if trimmed == "" || len(trimmed) > 16 || strings.ContainsAny(trimmed, " \t\n\r") {
		return false
	}
	hasAlphaNum := false
	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			hasAlphaNum = true
		case strings.ContainsRune("._:/-", r):
			continue
		default:
			return false
		}
	}
	return hasAlphaNum
}

func searchQueryForNews(instrument InstrumentRef) string {
	if raw := strings.TrimSpace(instrument.Symbol); raw != "" && !IsTickerLikeSymbol(raw) {
		return raw
	}
	return searchQuery(instrument)
}

// webSearchLocale holds the Google News locale triple (hl/gl/ceid) used to
// steer the web-search MCP. Empty fields fall back to the MCP's own defaults
// (en-US, US, US:en) so existing English-centric behaviour is preserved when
// no Chinese-language signal is present.
type webSearchLocale struct {
	hl   string
	gl   string
	ceid string
}

// newsLanguage maps the Google News locale triple to one of our canonical
// NewsLanguage constants ("zh" / "en"), or "" when the triple is the zero
// value (no signal — provider will auto-detect from content).
func (l webSearchLocale) newsLanguage() string {
	switch {
	case strings.HasPrefix(strings.ToLower(l.hl), "zh"):
		return NewsLanguageZH
	case strings.HasPrefix(strings.ToLower(l.hl), "en"):
		return NewsLanguageEN
	default:
		return ""
	}
}

// webSearchLocaleFor returns the locale triple appropriate for a given
// instrument and query. The precedence is:
//
//  1. Explicit LanguageHint(ctx) (set via WithLanguage by the request middleware).
//  2. CJK characters detected in the search query (covers free-text symbols
//     like "贵州茅台" / "A股 行业 新闻").
//  3. Instrument market: cnstock / hkstock implies Chinese locale because the
//     readership is overwhelmingly Mandarin-reading retail traders.
//
// Anything else returns the zero value, which signals the web-search MCP to
// fall back to its existing en-US defaults.
func webSearchLocaleFor(ctx context.Context, query string, instrument InstrumentRef) webSearchLocale {
	zh := webSearchLocale{hl: "zh-CN", gl: "CN", ceid: "CN:zh-Hans"}
	en := webSearchLocale{hl: "en-US", gl: "US", ceid: "US:en"}

	switch LanguageHint(ctx) {
	case LanguageHintZH:
		return zh
	case LanguageHintEN:
		return en
	}
	if containsCJK(query) {
		return zh
	}
	switch normalizeMarket(instrument.Market, instrument.AssetClass) {
	case "cnstock", "hkstock":
		return zh
	}
	return webSearchLocale{}
}

func searchQuery(instrument InstrumentRef) string {
	parts := []string{instrument.NormalizedSymbol()}
	if exchange := strings.TrimSpace(instrument.Exchange); exchange != "" {
		parts = append(parts, exchange)
	}
	switch normalizeMarket(instrument.Market, instrument.AssetClass) {
	case "crypto":
		parts = append(parts, "crypto market news")
	case "futures":
		parts = append(parts, "futures market news")
	case "cnstock":
		parts = append(parts, "A-share stock market news")
	case "usstock":
		parts = append(parts, "US stock market news")
	default:
		parts = append(parts, "market news")
	}
	return strings.Join(parts, " ")
}

func quantDingerMarket(instrument InstrumentRef) string {
	switch normalizeMarket(instrument.Market, instrument.AssetClass) {
	case "cnstock":
		return "CNStock"
	case "usstock":
		return "USStock"
	case "hkstock":
		return "HKStock"
	case "crypto":
		return "Crypto"
	case "futures":
		return "Futures"
	default:
		return ""
	}
}

func quantDingerSymbol(instrument InstrumentRef) string {
	symbol := instrument.NormalizedSymbol()
	if normalizeMarket(instrument.Market, instrument.AssetClass) == "crypto" {
		symbol = strings.ReplaceAll(symbol, " ", "")
	}
	return symbol
}

func normalizeMarket(market, assetClass string) string {
	combined := strings.ToLower(strings.TrimSpace(market))
	asset := strings.ToLower(strings.TrimSpace(assetClass))
	switch {
	case strings.Contains(combined, "cn"), strings.Contains(combined, "china"), strings.Contains(combined, "a_share"), strings.Contains(combined, "a-share"):
		return "cnstock"
	case strings.Contains(combined, "us"), strings.Contains(combined, "nasdaq"), strings.Contains(combined, "nyse"):
		return "usstock"
	case strings.Contains(combined, "hk"):
		return "hkstock"
	case strings.Contains(combined, "crypto") || strings.Contains(asset, "crypto"):
		return "crypto"
	case strings.Contains(combined, "futures") || strings.Contains(asset, "futures"):
		return "futures"
	case strings.Contains(asset, "equity"):
		return "usstock"
	default:
		return combined
	}
}

func buildEndpoint(baseURL, path string, params url.Values) (string, error) {
	endpoint, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/") + path)
	if err != nil {
		return "", err
	}
	query := endpoint.Query()
	for key, values := range params {
		if len(values) == 0 {
			continue
		}
		query.Set(key, values[len(values)-1])
	}
	endpoint.RawQuery = query.Encode()
	return endpoint.String(), nil
}

func (s *Service) fetchJSON(ctx context.Context, endpoint string) (map[string]any, error) {
	return s.fetchJSONWithHeaders(ctx, endpoint, nil)
}

func (s *Service) fetchJSONWithHeaders(ctx context.Context, endpoint string, headers map[string]string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	for key, value := range headers {
		if strings.TrimSpace(value) != "" {
			req.Header.Set(key, value)
		}
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func (s *Service) postJSON(ctx context.Context, endpoint string, payload any, headers map[string]string) (map[string]any, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		if strings.TrimSpace(value) != "" {
			req.Header.Set(key, value)
		}
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}

func unwrapData(payload map[string]any) map[string]any {
	if payload == nil {
		return map[string]any{}
	}
	if data, ok := payload["data"].(map[string]any); ok {
		return data
	}
	return payload
}

func parseNewsItems(data map[string]any, instrument InstrumentRef, limit int) []NewsItem {
	arrays := [][]any{}
	for _, key := range []string{"results", "items", "news", "articles", "data", "news_results"} {
		if list, ok := data[key].([]any); ok {
			arrays = append(arrays, list)
		}
	}
	for _, list := range arrays {
		items := make([]NewsItem, 0, len(list))
		for _, raw := range list {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			title := firstNonEmpty(stringValue(item, "title"), stringValue(item, "headline"), stringValue(item, "name"))
			if title == "" {
				continue
			}
			entry := NewsItem{
				Title:       title,
				Summary:     firstNonEmpty(stringValue(item, "summary"), stringValue(item, "snippet"), stringValue(item, "description"), stringValue(item, "content")),
				URL:         firstNonEmpty(stringValue(item, "url"), stringValue(item, "link")),
				Source:      firstNonEmpty(stringValue(item, "source"), stringValue(item, "site"), stringValue(item, "provider"), nestedStringValue(item, "source", "name")),
				PublishedAt: firstTimeValue(item, "publishedAt", "published_at", "published_date", "time", "date"),
				Symbols:     []string{instrument.NormalizedSymbol()},
			}
			items = append(items, entry)
			if len(items) >= limit {
				return items
			}
		}
		if len(items) > 0 {
			return items
		}
	}
	return nil
}

func firstPositiveFloat(values map[string]any, keys ...string) float64 {
	for _, key := range keys {
		if v, ok := values[key]; ok {
			if parsed := floatValue(v); parsed > 0 {
				return parsed
			}
		}
	}
	return 0
}

func stringValue(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	return stringFromAny(values[key])
}

func nestedStringValue(values map[string]any, key string, nestedKeys ...string) string {
	if values == nil {
		return ""
	}
	nested, ok := values[key].(map[string]any)
	if !ok {
		return ""
	}
	for _, nestedKey := range nestedKeys {
		if value := stringFromAny(nested[nestedKey]); value != "" {
			return value
		}
	}
	return ""
}

func firstTimeValue(values map[string]any, keys ...string) time.Time {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			if parsed, ok := timeFromAny(value); ok {
				return parsed
			}
		}
	}
	return time.Time{}
}

func floatValue(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		f, _ := v.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f
	default:
		return 0
	}
}

func stringFromAny(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case fmt.Stringer:
		return strings.TrimSpace(v.String())
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	default:
		return ""
	}
}

func timeFromAny(value any) (time.Time, bool) {
	switch v := value.(type) {
	case string:
		trimmed := strings.TrimSpace(v)
		for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
			if parsed, err := time.Parse(layout, trimmed); err == nil {
				return parsed.UTC(), true
			}
		}
	case float64:
		return unixTime(v)
	case int64:
		return unixTime(float64(v))
	case int:
		return unixTime(float64(v))
	}
	return time.Time{}, false
}

func unixTime(value float64) (time.Time, bool) {
	if value <= 0 {
		return time.Time{}, false
	}
	if value > 1e12 {
		millis := int64(math.Round(value))
		return time.UnixMilli(millis).UTC(), true
	}
	seconds := int64(math.Round(value))
	return time.Unix(seconds, 0).UTC(), true
}

func dedupeStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		key := strings.ToLower(trimmed)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
