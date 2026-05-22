package marketdata

import (
	"errors"
	"strings"
	"time"
)

var (
	ErrQuoteUnavailable = errors.New("marketdata: quote unavailable")
	ErrNewsUnavailable  = errors.New("marketdata: news unavailable")
	// ErrUpstreamThrottled wraps any provider error caused by HTTP 429 / 451
	// or an equivalent explicit throttle signal from upstream. The service
	// layer detects this sentinel via errors.Is and applies the longer
	// throttle-aware circuit cooldown (default 5min) instead of the regular
	// 30s break. Provider implementations should wrap with %w to keep the
	// classification working through call stacks.
	ErrUpstreamThrottled = errors.New("marketdata: upstream throttled")
)

type InstrumentRef struct {
	InstrumentKey      string
	Symbol             string
	Market             string
	Exchange           string
	AssetClass         string
	InstrumentType     string
	QuoteCurrency      string
	SettlementCurrency string
	ContractMultiplier float64
	ExpiryDate         string
}

func (r InstrumentRef) NormalizedSymbol() string {
	return strings.ToUpper(strings.TrimSpace(r.Symbol))
}

func (r InstrumentRef) CacheKey() string {
	parts := []string{
		strings.ToLower(strings.TrimSpace(r.Market)),
		strings.ToLower(strings.TrimSpace(r.Exchange)),
		strings.ToLower(strings.TrimSpace(r.AssetClass)),
		strings.ToUpper(strings.TrimSpace(r.Symbol)),
	}
	return strings.Join(parts, "|")
}

type QuoteSnapshot struct {
	Symbol        string    `json:"symbol"`
	InstrumentKey string    `json:"instrumentKey,omitempty"`
	Market        string    `json:"market,omitempty"`
	Exchange      string    `json:"exchange,omitempty"`
	AssetClass    string    `json:"assetClass,omitempty"`
	Price         float64   `json:"price"`
	Bid           float64   `json:"bid,omitempty"`
	Ask           float64   `json:"ask,omitempty"`
	Volume        int64     `json:"volume,omitempty"`
	QuoteCurrency string    `json:"quoteCurrency,omitempty"`
	AsOf          time.Time `json:"asOf"`
	Source        string    `json:"source"`
	IsStale       bool      `json:"isStale,omitempty"`
}

type NewsItem struct {
	Title       string    `json:"title"`
	TitleZh     string    `json:"titleZh,omitempty"`
	TitleEn     string    `json:"titleEn,omitempty"`
	Summary     string    `json:"summary,omitempty"`
	SummaryZh   string    `json:"summaryZh,omitempty"`
	SummaryEn   string    `json:"summaryEn,omitempty"`
	URL         string    `json:"url,omitempty"`
	Source      string    `json:"source,omitempty"`
	Language    string    `json:"language,omitempty"`
	PublishedAt time.Time `json:"publishedAt,omitempty"`
	Symbols     []string  `json:"symbols,omitempty"`
}

type ResearchContext struct {
	Instrument     InstrumentRef  `json:"instrument"`
	Quote          *QuoteSnapshot `json:"quote,omitempty"`
	News           []NewsItem     `json:"news,omitempty"`
	BenchmarkQuote *QuoteSnapshot `json:"benchmarkQuote,omitempty"`
	Signals        []string       `json:"signals,omitempty"`
	Summary        string         `json:"summary,omitempty"`
	ProviderNotes  []string       `json:"providerNotes,omitempty"`
	GeneratedAt    time.Time      `json:"generatedAt"`
}

type Config struct {
	ChinaStockURL        string
	AkshareURL           string
	TALibURL             string
	WebSearchURL         string
	QuantDingerURL       string
	// CoinGeckoBaseURL is the CoinGecko v3 API endpoint. Empty falls back to
	// the public, key-less default (CoinGeckoBaseURLDefault). Operators with
	// a CoinGecko Pro key should point this at the pro endpoint and add an
	// Authorization header upstream via a reverse proxy.
	CoinGeckoBaseURL string
	// CryptoWSEnabled toggles the Binance + Coinbase websocket streamers
	// that maintain an in-memory ticker cache. When true (default), the
	// `binance` / `coinbase` quote providers serve from cache instead of
	// hitting CoinGecko on every request; cache misses fall through to the
	// configured polling chain. Set to false to disable WS entirely and
	// rely only on polling.
	CryptoWSEnabled bool
	// BinanceWSURL is the websocket endpoint. Empty uses BinanceWSURLDefault
	// (data-stream.binance.vision, key-less). Tests override this with an
	// httptest server.
	BinanceWSURL string
	// BinanceWSSymbols is the list of ticker streams to subscribe to. Empty
	// uses defaultBinanceSymbols (the top-volume USDT pairs).
	BinanceWSSymbols []string
	// CoinbaseWSURL mirrors BinanceWSURL for the Coinbase Exchange feed.
	CoinbaseWSURL string
	// CoinbaseWSProducts is the list of product ids ("BTC-USD") to subscribe
	// to. Empty uses defaultCoinbaseProducts.
	CoinbaseWSProducts []string
	// CryptoWSStaleAfter controls how old a cached ticker may be before the
	// WS providers return ErrQuoteUnavailable and the chain falls back to
	// CoinGecko / Yahoo. Default: 30s (well above Binance's 100ms publish
	// cadence so a brief disconnect doesn't immediately downgrade quotes).
	CryptoWSStaleAfter time.Duration
	QuoteProviders     []string
	// QuoteProvidersByMarket overrides QuoteProviders for the specified market
	// key (lowercase, e.g. "cnstock"/"usstock"/"futures"/"crypto"). When set,
	// the matching market uses this chain instead of the global QuoteProviders;
	// other markets continue to use the global default. See
	// defaultQuoteProviderOrder for the built-in chain when nothing is set.
	QuoteProvidersByMarket map[string][]string
	NewsProviders          []string
	SerpAPIKeys          []string
	TavilyAPIKeys        []string
	SerpAPIBaseURL       string
	TavilyBaseURL        string
	EastmoneyNewsBaseURL string
	SinaNewsBaseURL      string
	// NewsHybridEnabled toggles the hybrid news aggregator that runs the
	// Chinese-leaning and English-leaning provider chains in parallel and
	// merges/dedupes the result. Defaults to true; set to false to fall back
	// to the legacy first-non-empty-provider chain (useful for debugging or
	// when bandwidth/cost is a concern).
	NewsHybridEnabled bool
	QuoteTTL          time.Duration
	NewsTTL           time.Duration
	ProviderTimeout   time.Duration

	// StaleQuoteAfter is the maximum age (relative to QuoteSnapshot.AsOf at
	// the moment a request is served) before a quote is flagged with
	// IsStale=true. The risk + execution layers use this to surface
	// "outdated data" warnings in the Decision Center instead of silently
	// trading on a 2-hour-old price. Default: 15 minutes.
	StaleQuoteAfter time.Duration

	// QuoteCircuitFailureThreshold is the number of consecutive provider
	// errors that trigger a circuit break for that provider. Default: 3.
	QuoteCircuitFailureThreshold int
	// QuoteCircuitCooldown is how long a tripped provider is bypassed
	// before the next call is allowed to retry. Default: 30 seconds.
	QuoteCircuitCooldown time.Duration

	// AdaptiveTTLEnabled lets the cache shrink the news TTL outside of
	// trading hours (less volatile) and expand it during weekends/closed
	// markets. Default: true.
	AdaptiveTTLEnabled bool

	// AdaptiveQuoteTTLEnabled extends the AdaptiveTTL idea to quotes: while
	// equity sessions are open we use QuoteTTLInSession (default 5s) so
	// the SSE pusher and GET overlay can ride a high cache-hit rate; outside
	// session we expand to QuoteTTLOffSession (default 60s) because the
	// instrument simply isn't moving. Default: true.
	AdaptiveQuoteTTLEnabled bool
	// QuoteTTLInSession is the cache TTL applied to a quote when the
	// instrument's primary session is open. Default: 5s. Falls back to
	// QuoteTTL when zero.
	QuoteTTLInSession time.Duration
	// QuoteTTLOffSession is the cache TTL applied outside trading hours.
	// Default: 60s. Falls back to QuoteTTL when zero.
	QuoteTTLOffSession time.Duration

	// ProviderRateLimits overrides the default per-provider rate limits.
	// When nil DefaultProviderRateLimits() is used. The map keys match the
	// lowercase provider names (e.g. "yahoo", "eastmoney").
	ProviderRateLimits ProviderRateLimits
	// ProviderRateLimitsSpec is the raw env-style spec that gets merged on
	// top of DefaultProviderRateLimits when non-empty. See
	// ParseProviderRateLimits for the syntax. Mutually compatible with
	// ProviderRateLimits (the map takes precedence when both are set).
	ProviderRateLimitsSpec string

	// QuoteThrottleCooldown is the cooldown applied when a provider gets
	// explicitly throttled (HTTP 429/451 or ErrUpstreamThrottled). Default:
	// 5 minutes. Must be >= QuoteCircuitCooldown to be meaningful.
	QuoteThrottleCooldown time.Duration
}

func (c Config) normalize() Config {
	if c.QuoteTTL <= 0 {
		c.QuoteTTL = 10 * time.Second
	}
	if c.NewsTTL <= 0 {
		c.NewsTTL = 2 * time.Minute
	}
	if c.ProviderTimeout <= 0 {
		c.ProviderTimeout = 5 * time.Second
	}
	if c.StaleQuoteAfter <= 0 {
		c.StaleQuoteAfter = 15 * time.Minute
	}
	if c.QuoteCircuitFailureThreshold <= 0 {
		c.QuoteCircuitFailureThreshold = 3
	}
	if c.QuoteCircuitCooldown <= 0 {
		c.QuoteCircuitCooldown = 30 * time.Second
	}
	if c.QuoteThrottleCooldown <= 0 {
		c.QuoteThrottleCooldown = 5 * time.Minute
	}
	if c.QuoteThrottleCooldown < c.QuoteCircuitCooldown {
		c.QuoteThrottleCooldown = c.QuoteCircuitCooldown
	}
	if c.QuoteTTLInSession <= 0 {
		c.QuoteTTLInSession = 5 * time.Second
	}
	if c.QuoteTTLOffSession <= 0 {
		c.QuoteTTLOffSession = 60 * time.Second
	}
	// Adaptive quote TTL defaults to enabled because it strictly reduces
	// upstream load (off-hours TTL is longer than QuoteTTL). Operators who
	// want the old uniform 10s behaviour can set the env flag to false.
	// `c.AdaptiveQuoteTTLEnabled` is a bool, so the legacy normalize() call
	// keeps it false; instead the caller (config loader) is expected to
	// initialize this explicitly. We do NOT auto-toggle it in normalize()
	// to keep the zero-value Config usable by tests that don't want the
	// behaviour change.
	if c.CryptoWSStaleAfter <= 0 {
		c.CryptoWSStaleAfter = 30 * time.Second
	}
	c.ChinaStockURL = strings.TrimSpace(c.ChinaStockURL)
	c.AkshareURL = strings.TrimSpace(c.AkshareURL)
	c.TALibURL = strings.TrimSpace(c.TALibURL)
	c.WebSearchURL = strings.TrimSpace(c.WebSearchURL)
	c.QuantDingerURL = strings.TrimSpace(c.QuantDingerURL)
	c.CoinGeckoBaseURL = strings.TrimSpace(c.CoinGeckoBaseURL)
	c.BinanceWSURL = strings.TrimSpace(c.BinanceWSURL)
	c.CoinbaseWSURL = strings.TrimSpace(c.CoinbaseWSURL)
	c.QuoteProviders = normalizeProviderNames(c.QuoteProviders)
	c.QuoteProvidersByMarket = normalizeProvidersByMarket(c.QuoteProvidersByMarket)
	c.NewsProviders = normalizeProviderNames(c.NewsProviders)
	c.SerpAPIKeys = normalizeProviderSecrets(c.SerpAPIKeys)
	c.TavilyAPIKeys = normalizeProviderSecrets(c.TavilyAPIKeys)
	c.SerpAPIBaseURL = strings.TrimSpace(c.SerpAPIBaseURL)
	c.TavilyBaseURL = strings.TrimSpace(c.TavilyBaseURL)
	c.EastmoneyNewsBaseURL = strings.TrimSpace(c.EastmoneyNewsBaseURL)
	c.SinaNewsBaseURL = strings.TrimSpace(c.SinaNewsBaseURL)
	return c
}

func (c Config) Enabled() bool {
	return c.quoteProviderEnabled("china-stock") ||
		c.quoteProviderEnabled("akshare") ||
		c.quoteProviderEnabled("quantdinger") ||
		c.quoteProviderEnabled("tencent") ||
		c.quoteProviderEnabled("yahoo") ||
		c.quoteProviderEnabled("coingecko") ||
		c.quoteProviderEnabled("binance") ||
		c.quoteProviderEnabled("coinbase") ||
		c.newsProviderEnabled("web-search") ||
		c.newsProviderEnabled("local-search") ||
		c.newsProviderEnabled("serpapi") ||
		c.newsProviderEnabled("tavily") ||
		c.newsProviderEnabled("eastmoney") ||
		c.newsProviderEnabled("sina")
}

func (c Config) quoteProviderEnabled(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "china-stock":
		return c.ChinaStockURL != ""
	case "akshare":
		return c.AkshareURL != ""
	case "quantdinger":
		return c.QuantDingerURL != ""
	case "tencent":
		return true
	case "yahoo":
		return true
	case "coingecko":
		// CoinGecko's free public API is always reachable; operators can
		// disable it explicitly by omitting it from QuoteProviders or
		// QuoteProvidersByMarket. Setting a custom CoinGeckoBaseURL is
		// optional (Pro endpoint or local reverse proxy).
		return true
	case "binance", "coinbase":
		// WS providers are gated by CryptoWSEnabled. When disabled the
		// provider name is silently dropped from the chain so the rest
		// of the crypto fallback (CoinGecko, Yahoo) still functions.
		return c.CryptoWSEnabled
	default:
		return false
	}
}

func (c Config) newsProviderEnabled(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "web-search":
		return c.WebSearchURL != ""
	case "local-search":
		return len(c.SerpAPIKeys) > 0 || len(c.TavilyAPIKeys) > 0
	case "quantdinger":
		return c.QuantDingerURL != ""
	case "serpapi":
		return len(c.SerpAPIKeys) > 0
	case "tavily":
		return len(c.TavilyAPIKeys) > 0
	case "eastmoney", "sina":
		return true
	default:
		return false
	}
}

func normalizeProviderNames(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		trimmed := strings.ToLower(strings.TrimSpace(value))
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}

// normalizeProvidersByMarket lowercases market keys, trims/dedupes each
// provider list, and drops empty entries. A nil/empty input returns nil so
// callers can treat "no per-market override" as a single zero-value check.
func normalizeProvidersByMarket(in map[string][]string) map[string][]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]string, len(in))
	for market, names := range in {
		key := strings.ToLower(strings.TrimSpace(market))
		if key == "" {
			continue
		}
		normalized := normalizeProviderNames(names)
		if len(normalized) == 0 {
			continue
		}
		out[key] = normalized
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeProviderSecrets(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	return result
}
