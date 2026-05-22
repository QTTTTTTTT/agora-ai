package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CoinGeckoBaseURLDefault is the public, key-less CoinGecko API base. The free
// tier is rate-limited (~30 req/min, ~10k req/month) which is sufficient for
// fund-level periodic quoting but should not be used for tick-level trading.
const CoinGeckoBaseURLDefault = "https://api.coingecko.com/api/v3"

// coingeckoQuoteProvider returns a quote provider that resolves crypto
// instruments through CoinGecko's `/simple/price` endpoint.
//
// Symbol convention: the platform stores crypto instruments using exchange-style
// pair symbols (e.g. "BTCUSDT", "ETHUSDT", "SOLUSD"). CoinGecko, however, indexes
// coins by long-form ids ("bitcoin", "ethereum", "solana"). The provider applies
// `coinGeckoCoinID` to map between the two; unmapped symbols return an
// explanatory error so the fallback chain can move on cleanly.
//
// vs_currency is derived from `instrument.QuoteCurrency` when populated and
// falls back to "usd". CoinGecko supports a broad set (usd, usdt, eur, cny, ...);
// we lowercase the value before sending.
//
// Endpoint:
//
//	GET https://api.coingecko.com/api/v3/simple/price?ids=bitcoin&vs_currencies=usd
//	    &include_24hr_vol=true&include_last_updated_at=true
//
// Response shape:
//
//	{ "bitcoin": { "usd": 45000.5, "usd_24h_vol": 1.2e10, "last_updated_at": 1715000000 } }
func (s *Service) coingeckoQuoteProvider() quoteProviderFunc {
	baseURL := strings.TrimRight(s.cfg.CoinGeckoBaseURL, "/")
	if baseURL == "" {
		baseURL = CoinGeckoBaseURLDefault
	}
	return func(ctx context.Context, instrument InstrumentRef) (*QuoteSnapshot, error) {
		return s.coingeckoSimplePriceAt(ctx, baseURL, instrument)
	}
}

func (s *Service) coingeckoSimplePriceAt(ctx context.Context, baseURL string, instrument InstrumentRef) (*QuoteSnapshot, error) {
	coinID := coinGeckoCoinID(instrument)
	if coinID == "" {
		return nil, fmt.Errorf("coingecko: cannot derive coin id from %q", instrument.Symbol)
	}
	vsCurrency := coinGeckoVsCurrency(instrument)
	endpoint, err := url.Parse(baseURL + "/simple/price")
	if err != nil {
		return nil, fmt.Errorf("coingecko: parse url: %w", err)
	}
	q := endpoint.Query()
	q.Set("ids", coinID)
	q.Set("vs_currencies", vsCurrency)
	q.Set("include_24hr_vol", "true")
	q.Set("include_last_updated_at", "true")
	endpoint.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("coingecko: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "fundai-marketdata/1.0")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("coingecko: http: %w", err)
	}
	defer resp.Body.Close()
	if isThrottleStatus(resp.StatusCode) {
		return nil, fmt.Errorf("%w: coingecko: http %d (reduce request frequency or upgrade to Pro plan)", ErrUpstreamThrottled, resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("coingecko: http %d", resp.StatusCode)
	}

	var payload map[string]map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("coingecko: decode: %w", err)
	}
	row, ok := payload[coinID]
	if !ok || len(row) == 0 {
		return nil, fmt.Errorf("coingecko: empty result for %s (vs %s)", coinID, vsCurrency)
	}

	price := firstPositiveFloat(row, vsCurrency)
	if price <= 0 {
		return nil, fmt.Errorf("coingecko: non-positive price for %s/%s", coinID, vsCurrency)
	}
	volume := firstPositiveFloat(row, vsCurrency+"_24h_vol")
	asOf := time.Now().UTC()
	if ts := firstPositiveFloat(row, "last_updated_at"); ts > 0 {
		asOf = time.Unix(int64(ts), 0).UTC()
	}
	return &QuoteSnapshot{
		Symbol:        firstNonEmpty(instrument.NormalizedSymbol(), coinID),
		InstrumentKey: instrument.InstrumentKey,
		Market:        instrument.Market,
		Exchange:      instrument.Exchange,
		AssetClass:    instrument.AssetClass,
		Price:         price,
		Volume:        int64(volume),
		QuoteCurrency: firstNonEmpty(instrument.QuoteCurrency, strings.ToUpper(vsCurrency)),
		AsOf:          asOf,
		Source:        "coingecko",
	}, nil
}

// coinGeckoVsCurrency derives the CoinGecko `vs_currencies` query value from
// the instrument's quote currency. Defaults to USD when no hint is available.
func coinGeckoVsCurrency(instrument InstrumentRef) string {
	quote := strings.ToLower(strings.TrimSpace(instrument.QuoteCurrency))
	if quote == "" {
		// Try to infer from the symbol's trailing quote token (BTCUSDT → usdt).
		quote = inferCryptoQuoteFromSymbol(instrument.NormalizedSymbol())
	}
	if quote == "" {
		return "usd"
	}
	// USDT/USDC pairs route to USD on CoinGecko's simple endpoint — the
	// platform treats stablecoin prices as USD-equivalent for quote purposes.
	switch quote {
	case "usdt", "usdc", "busd", "dai", "tusd":
		return "usd"
	}
	return quote
}

// inferCryptoQuoteFromSymbol pulls the trailing quote-currency segment off a
// canonical crypto pair symbol like "BTCUSDT". Returns lowercase or empty
// when the symbol has no recognisable suffix.
func inferCryptoQuoteFromSymbol(symbol string) string {
	upper := strings.ToUpper(symbol)
	suffixes := []string{"USDT", "USDC", "BUSD", "TUSD", "DAI", "USD", "EUR", "BTC", "ETH"}
	for _, suffix := range suffixes {
		if strings.HasSuffix(upper, suffix) && len(upper) > len(suffix) {
			return strings.ToLower(suffix)
		}
	}
	return ""
}

// coinGeckoSymbolToID maps the leading base-asset segment of a canonical
// crypto pair symbol to the CoinGecko coin id. This is intentionally a
// closed list of the most commonly traded coins on the platform — adding more
// is cheap and unit-tested. Unknown symbols return "" so callers (the
// provider) can emit a clear error and the fallback chain proceeds.
var coinGeckoSymbolToID = map[string]string{
	"BTC":   "bitcoin",
	"XBT":   "bitcoin",
	"ETH":   "ethereum",
	"BNB":   "binancecoin",
	"SOL":   "solana",
	"XRP":   "ripple",
	"ADA":   "cardano",
	"DOGE":  "dogecoin",
	"AVAX":  "avalanche-2",
	"DOT":   "polkadot",
	"MATIC": "matic-network",
	"POL":   "matic-network",
	"LTC":   "litecoin",
	"LINK":  "chainlink",
	"UNI":   "uniswap",
	"ATOM":  "cosmos",
	"NEAR":  "near",
	"ARB":   "arbitrum",
	"OP":    "optimism",
	"TRX":   "tron",
	"BCH":   "bitcoin-cash",
	"FIL":   "filecoin",
	"APT":   "aptos",
	"SUI":   "sui",
	"INJ":   "injective-protocol",
	"PEPE":  "pepe",
	"SHIB":  "shiba-inu",
	"TON":   "the-open-network",
}

// coinGeckoCoinID derives a CoinGecko coin id from an InstrumentRef.
// Heuristics, in order:
//
//  1. `instrument.Symbol` matches a known coin id directly (case-insensitive,
//     allows callers to bypass the mapping by storing the canonical id).
//  2. Strip a known quote-currency suffix (USDT, USD, BTC, ...) and look the
//     base up in coinGeckoSymbolToID.
//  3. As a last resort, lowercase the full symbol and treat it as a coin id
//     (works for one-off niche coins; CoinGecko returns empty if it doesn't
//     recognise it, which we surface as a clean error).
func coinGeckoCoinID(instrument InstrumentRef) string {
	symbol := strings.TrimSpace(instrument.Symbol)
	if symbol == "" {
		return ""
	}
	upper := strings.ToUpper(symbol)
	if id, ok := coinGeckoSymbolToID[upper]; ok {
		return id
	}
	// Try to strip a quote suffix and re-look-up the base.
	if quoteSuffix := inferCryptoQuoteFromSymbol(upper); quoteSuffix != "" {
		base := strings.TrimSuffix(upper, strings.ToUpper(quoteSuffix))
		if id, ok := coinGeckoSymbolToID[base]; ok {
			return id
		}
	}
	// Fall back to lowercase-as-id. CoinGecko will 404 if it isn't valid,
	// which the caller surfaces verbatim.
	return strings.ToLower(symbol)
}
