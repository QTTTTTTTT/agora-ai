// marketdata-probe is a lightweight CLI for sanity-checking the marketdata
// pipeline end-to-end against real upstreams. It bypasses the HTTP layer
// (auth, db, fund lookups) so you can verify Phase 1 + Phase 2 changes
// quickly without spinning up the full app stack.
//
// Examples:
//
//	# Quote + hybrid news for a CN A-share (uses Tencent quote + Eastmoney/Sina/web-search news)
//	go run ./cmd/marketdata-probe -symbol 600519 -market cnstock
//
//	# Quote + hybrid news for a US equity (uses Yahoo quote + web-search/serpapi)
//	go run ./cmd/marketdata-probe -symbol AAPL -market us_equity
//
//	# Force the legacy single-chain news fallback for comparison
//	go run ./cmd/marketdata-probe -symbol 600519 -market cnstock -hybrid=false
//
//	# Exercise the translator (requires MARKETDATA_TRANSLATOR_* env vars or flags)
//	go run ./cmd/marketdata-probe -symbol 600519 -market cnstock \
//	    -translator libretranslate -translator-base-url https://libretranslate.de
//
//	# Exercise the Binance + Coinbase websocket feed for crypto. Adds a ~3s
//	# warm-up window so the WS streams have time to receive a first ticker
//	# before the quote call runs. With -crypto-ws the chain default for
//	# crypto is binance → coinbase → coingecko → yahoo, so the snapshot
//	# Source should report binance-ws or coinbase-ws on a successful probe.
//	go run ./cmd/marketdata-probe -symbol BTCUSDT -market crypto -crypto-ws
//
// Exits non-zero if the quote or news fetch fails so the binary is CI-safe.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/fundai/server/internal/marketdata"
)

func main() {
	symbol := flag.String("symbol", "", "instrument symbol (e.g. 600519, AAPL, BTC-USD)")
	market := flag.String("market", "", "instrument market (cnstock, us_equity, hkstock, crypto, futures)")
	asset := flag.String("asset-class", "", "instrument asset class (equity, futures, crypto, etc.)")
	limit := flag.Int("limit", 5, "number of news items to fetch")
	hybrid := flag.Bool("hybrid", true, "enable hybrid news aggregation")
	skipQuote := flag.Bool("skip-quote", false, "skip quote fetch (news only)")
	skipNews := flag.Bool("skip-news", false, "skip news fetch (quote only)")
	timeout := flag.Duration("timeout", 12*time.Second, "per-provider HTTP timeout")
	webSearchURL := flag.String("web-search-url", "", "MCP web-search base URL (defaults to env MCP_WEB_SEARCH_URL)")
	serpKeys := flag.String("serpapi-keys", "", "comma-separated SerpAPI keys (defaults to env SERPAPI_KEYS)")
	tavilyKeys := flag.String("tavily-keys", "", "comma-separated Tavily keys (defaults to env TAVILY_API_KEYS)")
	translatorProvider := flag.String("translator", "", "translator provider: none|libretranslate|openai-compat (defaults to env MARKETDATA_TRANSLATOR_PROVIDER)")
	translatorBaseURL := flag.String("translator-base-url", "", "translator base URL (defaults to env MARKETDATA_TRANSLATOR_BASE_URL)")
	translatorAPIKey := flag.String("translator-api-key", "", "translator API key (defaults to env MARKETDATA_TRANSLATOR_API_KEY)")
	translatorModel := flag.String("translator-model", "", "translator model id (openai-compat only; defaults to env MARKETDATA_TRANSLATOR_MODEL)")
	akshareURL := flag.String("akshare-url", "", "akshare MCP base URL (defaults to env MCP_AKSHARE_URL)")
	chinaStockURL := flag.String("china-stock-url", "", "china-stock MCP base URL (defaults to env MCP_CHINA_STOCK_URL)")
	coingeckoURL := flag.String("coingecko-url", "", "CoinGecko base URL override (defaults to env MARKETDATA_COINGECKO_BASE_URL or the public v3 endpoint)")
	quoteProvidersCSV := flag.String("quote-providers", "", "comma-separated quote provider chain override (defaults to the market-aware built-in chain)")
	cryptoWS := flag.Bool("crypto-ws", false, "start the Binance + Coinbase websocket streamers and wait ~3s for first tickers before probing (crypto only)")
	cryptoWSWait := flag.Duration("crypto-ws-wait", 3*time.Second, "how long to wait after starting WS streams before probing the quote")
	flag.Parse()

	if strings.TrimSpace(*symbol) == "" {
		fmt.Fprintln(os.Stderr, "error: -symbol is required")
		flag.Usage()
		os.Exit(2)
	}

	quoteProviderChain := splitCSV(*quoteProvidersCSV)
	if len(quoteProviderChain) == 0 {
		// Match the server default so probe runs mirror production routing:
		// tencent (A/HK), yahoo (US/HK/futures fallback), akshare (A/futures),
		// coingecko (crypto), plus binance + coinbase WS when -crypto-ws is
		// set. Operators can override per-market via -quote-providers or
		// rely on the built-in market-aware chain.
		quoteProviderChain = []string{"tencent", "yahoo", "akshare", "china-stock", "coingecko"}
		if *cryptoWS {
			quoteProviderChain = append([]string{"binance", "coinbase"}, quoteProviderChain...)
		}
	}

	cfg := marketdata.Config{
		WebSearchURL:      firstNonEmpty(*webSearchURL, os.Getenv("MCP_WEB_SEARCH_URL")),
		AkshareURL:        firstNonEmpty(*akshareURL, os.Getenv("MCP_AKSHARE_URL")),
		ChinaStockURL:     firstNonEmpty(*chinaStockURL, os.Getenv("MCP_CHINA_STOCK_URL")),
		CoinGeckoBaseURL:  firstNonEmpty(*coingeckoURL, os.Getenv("MARKETDATA_COINGECKO_BASE_URL")),
		CryptoWSEnabled:   *cryptoWS,
		SerpAPIKeys:       splitCSV(firstNonEmpty(*serpKeys, os.Getenv("SERPAPI_KEYS"))),
		TavilyAPIKeys:     splitCSV(firstNonEmpty(*tavilyKeys, os.Getenv("TAVILY_API_KEYS"))),
		NewsProviders:     []string{"eastmoney", "sina", "web-search", "serpapi", "tavily"},
		QuoteProviders:    quoteProviderChain,
		NewsHybridEnabled: *hybrid,
		QuoteTTL:          5 * time.Second,
		NewsTTL:           30 * time.Second,
		ProviderTimeout:   *timeout,
	}

	translatorCfg := marketdata.TranslatorConfig{
		Provider: firstNonEmpty(*translatorProvider, os.Getenv("MARKETDATA_TRANSLATOR_PROVIDER")),
		BaseURL:  firstNonEmpty(*translatorBaseURL, os.Getenv("MARKETDATA_TRANSLATOR_BASE_URL")),
		APIKey:   firstNonEmpty(*translatorAPIKey, os.Getenv("MARKETDATA_TRANSLATOR_API_KEY")),
		Model:    firstNonEmpty(*translatorModel, os.Getenv("MARKETDATA_TRANSLATOR_MODEL")),
		Timeout:  *timeout,
	}
	translator := marketdata.NewTranslator(translatorCfg)
	svc := marketdata.NewService(cfg).WithTranslator(translator)

	if *cryptoWS {
		// Start the streams + sleep so the cache has a chance to be
		// populated before the quote probe runs. The seed-symbol list
		// is the built-in defaults (top-25 USDT / top-20 USD pairs).
		fmt.Printf("Starting Binance/Coinbase WS streams (warm-up %s)...\n", cryptoWSWait.String())
		svc.StartCryptoStreams(context.Background())
		defer func() { _ = svc.Close(2 * time.Second) }()
		time.Sleep(*cryptoWSWait)
		if cache := svc.CryptoWSSnapshot(); len(cache) > 0 {
			fmt.Printf("WS cache primed: %d symbols\n", len(cache))
		} else {
			fmt.Println("WS cache empty (network blocked or symbol not in seed list)")
		}
	}

	instrument := marketdata.InstrumentRef{
		Symbol:     *symbol,
		Market:     *market,
		AssetClass: *asset,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	exitCode := 0
	if !*skipQuote {
		fmt.Println("== Quote ==")
		quote, err := svc.GetQuote(ctx, instrument)
		if err != nil {
			fmt.Fprintf(os.Stderr, "quote error: %v\n", err)
			exitCode = 1
		} else {
			printJSON(quote)
		}
	}

	if !*skipNews {
		fmt.Println("\n== News ==")
		items, notes, err := svc.GetNewsWithNotes(ctx, instrument, *limit)
		if err != nil {
			fmt.Fprintf(os.Stderr, "news error: %v\n", err)
			exitCode = 1
		}
		if len(notes) > 0 {
			fmt.Printf("provider notes: %s\n", strings.Join(notes, "; "))
		}
		fmt.Printf("returned %d items (hybrid=%v)\n", len(items), cfg.NewsHybridEnabled)
		printNewsSummary(items)
	}

	if cfg.NewsHybridEnabled && !*skipNews {
		fmt.Println("\n== Language distribution ==")
		printLanguageBreakdown()
		// Re-fetch fresh to count by language; cache will satisfy this with
		// negligible cost.
		items, _, err := svc.GetNewsWithNotes(ctx, instrument, *limit)
		if err == nil {
			countByLanguage(items)
		}
	}

	fmt.Println("\n== Provider health ==")
	printProviderHealth(svc.ProviderHealth())

	os.Exit(exitCode)
}

func printProviderHealth(health map[string]marketdata.ProviderHealthStats) {
	if len(health) == 0 {
		fmt.Println("(no provider activity recorded)")
		return
	}
	for name, stats := range health {
		fmt.Printf("- %-14s calls=%d ok=%d fail=%d consecutive=%d ema=%dms last=%dms",
			name, stats.TotalCalls, stats.TotalSuccesses, stats.TotalFailures,
			stats.ConsecutiveFailures, stats.EMALatencyMs, stats.LastLatencyMs)
		if !stats.CircuitOpenUntil.IsZero() {
			fmt.Printf(" circuitOpenUntil=%s", stats.CircuitOpenUntil.Format(time.RFC3339))
		}
		if stats.LastError != "" {
			fmt.Printf(" lastErr=%q", stats.LastError)
		}
		fmt.Println()
	}
}

func printJSON(v any) {
	encoded, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "json encode: %v\n", err)
		return
	}
	fmt.Println(string(encoded))
}

func printNewsSummary(items []marketdata.NewsItem) {
	for i, item := range items {
		fmt.Printf("\n[%d] lang=%s source=%s\n", i+1, fallback(item.Language, "?"), fallback(item.Source, "?"))
		if item.Title != "" {
			fmt.Printf("    title:     %s\n", item.Title)
		}
		if item.TitleZh != "" && item.TitleZh != item.Title {
			fmt.Printf("    titleZh:   %s\n", item.TitleZh)
		}
		if item.TitleEn != "" && item.TitleEn != item.Title {
			fmt.Printf("    titleEn:   %s\n", item.TitleEn)
		}
		if !item.PublishedAt.IsZero() {
			fmt.Printf("    publishedAt: %s\n", item.PublishedAt.Format(time.RFC3339))
		}
		if item.URL != "" {
			fmt.Printf("    url:       %s\n", item.URL)
		}
	}
}

func printLanguageBreakdown() {
	// Header line; counts printed by countByLanguage.
}

func countByLanguage(items []marketdata.NewsItem) {
	counts := map[string]int{}
	bilingual := 0
	for _, item := range items {
		counts[fallback(item.Language, "?")]++
		hasZh := strings.TrimSpace(item.TitleZh) != ""
		hasEn := strings.TrimSpace(item.TitleEn) != ""
		if hasZh && hasEn {
			bilingual++
		}
	}
	for lang, count := range counts {
		fmt.Printf("  %s: %d items\n", lang, count)
	}
	fmt.Printf("  bilingual (both titleZh and titleEn populated): %d items\n", bilingual)
}

func fallback(value, def string) string {
	if strings.TrimSpace(value) == "" {
		return def
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
