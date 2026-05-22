package marketdata

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCoinGeckoSimplePriceParsesUSDQuote verifies the happy-path of the
// CoinGecko provider — it decodes a /simple/price payload into a QuoteSnapshot
// with the right price, currency, and as-of timestamp.
func TestCoinGeckoSimplePriceParsesUSDQuote(t *testing.T) {
	const sample = `{"bitcoin":{"usd":45000.5,"usd_24h_vol":12000000000,"last_updated_at":1715000000}}`
	var capturedQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sample))
	}))
	defer srv.Close()

	svc := &Service{httpClient: srv.Client()}
	quote, err := svc.coingeckoSimplePriceAt(
		context.Background(),
		srv.URL,
		InstrumentRef{Symbol: "BTCUSDT", Market: "crypto", AssetClass: "crypto"},
	)
	if err != nil {
		t.Fatalf("coingeckoSimplePriceAt: %v", err)
	}
	if quote.Price != 45000.5 {
		t.Fatalf("expected price 45000.5, got %v", quote.Price)
	}
	// USDT pairs are reported as USD-equivalent: the platform treats
	// USDT/USDC/BUSD as pegged-to-USD and CoinGecko returned a USD figure,
	// so QuoteCurrency reflects what we actually have (USD), not the raw
	// trading-pair quote token.
	if quote.QuoteCurrency != "USD" {
		t.Fatalf("expected QuoteCurrency=USD (stablecoin collapse), got %q", quote.QuoteCurrency)
	}
	if quote.Volume != 12_000_000_000 {
		t.Fatalf("expected volume 1.2e10, got %d", quote.Volume)
	}
	if quote.Source != "coingecko" {
		t.Fatalf("expected source=coingecko, got %q", quote.Source)
	}
	if quote.AsOf.Unix() != 1_715_000_000 {
		t.Fatalf("expected AsOf to come from last_updated_at, got %v", quote.AsOf)
	}
	if !strings.Contains(capturedQuery, "ids=bitcoin") {
		t.Fatalf("expected ids=bitcoin in query, got %q", capturedQuery)
	}
	// USDT is collapsed to usd on the CoinGecko side (stablecoin parity).
	if !strings.Contains(capturedQuery, "vs_currencies=usd") {
		t.Fatalf("expected vs_currencies=usd (USDT→usd), got %q", capturedQuery)
	}
}

// TestCoinGeckoSimplePriceRejectsRateLimit verifies that HTTP 429 is surfaced
// with a clear message so the fallback chain (and the circuit breaker) react
// instead of treating it as a transient zero-price response.
func TestCoinGeckoSimplePriceRejectsRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"status":{"error_code":429}}`))
	}))
	defer srv.Close()

	svc := &Service{httpClient: srv.Client()}
	_, err := svc.coingeckoSimplePriceAt(context.Background(), srv.URL, InstrumentRef{Symbol: "BTCUSDT"})
	if err == nil {
		t.Fatalf("expected rate-limit error, got nil")
	}
	if !errors.Is(err, ErrUpstreamThrottled) {
		t.Fatalf("expected ErrUpstreamThrottled, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "http 429") {
		t.Fatalf("expected message to mention HTTP 429, got %q", err.Error())
	}
}

// TestCoinGeckoSimplePriceFallsBackWhenEmpty asserts that an empty result
// surfaces as an error rather than a zero-price quote.
func TestCoinGeckoSimplePriceFallsBackWhenEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body, _ := json.Marshal(map[string]any{})
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	svc := &Service{httpClient: srv.Client()}
	_, err := svc.coingeckoSimplePriceAt(context.Background(), srv.URL, InstrumentRef{Symbol: "BTCUSDT"})
	if err == nil || !strings.Contains(err.Error(), "empty result") {
		t.Fatalf("expected empty-result error, got %v", err)
	}
}

// TestCoinGeckoSimplePriceUsesExplicitQuoteCurrency confirms that an explicit
// QuoteCurrency on the instrument overrides the symbol-derived guess.
func TestCoinGeckoSimplePriceUsesExplicitQuoteCurrency(t *testing.T) {
	const sample = `{"ethereum":{"eur":3000,"eur_24h_vol":500000000,"last_updated_at":1715001000}}`
	var capturedQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(sample))
	}))
	defer srv.Close()

	svc := &Service{httpClient: srv.Client()}
	quote, err := svc.coingeckoSimplePriceAt(
		context.Background(),
		srv.URL,
		InstrumentRef{Symbol: "ETHEUR", Market: "crypto", AssetClass: "crypto", QuoteCurrency: "EUR"},
	)
	if err != nil {
		t.Fatalf("coingeckoSimplePriceAt: %v", err)
	}
	if quote.Price != 3000 {
		t.Fatalf("expected price 3000, got %v", quote.Price)
	}
	if quote.QuoteCurrency != "EUR" {
		t.Fatalf("expected QuoteCurrency=EUR, got %q", quote.QuoteCurrency)
	}
	if !strings.Contains(capturedQuery, "vs_currencies=eur") {
		t.Fatalf("expected vs_currencies=eur in query, got %q", capturedQuery)
	}
}

// TestCoinGeckoCoinIDMapsKnownSymbols spot-checks the symbol→coin-id mapping
// for a handful of high-volume pairs. The test keeps the rest of the map
// covered by guarding against accidental case-sensitivity regressions.
func TestCoinGeckoCoinIDMapsKnownSymbols(t *testing.T) {
	cases := []struct {
		symbol string
		want   string
	}{
		{"BTC", "bitcoin"},
		{"btcusdt", "bitcoin"},
		{"ETHUSDT", "ethereum"},
		{"SOL", "solana"},
		{"DOGE-USD", "doge-usd"}, // unknown after suffix strip → lowercase fallback
		{"XRPUSDC", "ripple"},
		{"matic", "matic-network"},
	}
	for _, tc := range cases {
		got := coinGeckoCoinID(InstrumentRef{Symbol: tc.symbol})
		if got != tc.want {
			t.Errorf("coinGeckoCoinID(%q) = %q, want %q", tc.symbol, got, tc.want)
		}
	}
}

// TestCoinGeckoCoinIDEmptySymbol guards against returning a usable coin id
// when the input is blank — that would let CoinGecko 404 with confusing logs.
func TestCoinGeckoCoinIDEmptySymbol(t *testing.T) {
	if got := coinGeckoCoinID(InstrumentRef{Symbol: "   "}); got != "" {
		t.Fatalf("expected empty coin id for blank symbol, got %q", got)
	}
}

// TestInferCryptoQuoteFromSymbol verifies the suffix detection used for
// vs_currency inference. It must not eat the entire symbol (e.g. "BTC"
// alone shouldn't decay to "btc" as a quote currency).
func TestInferCryptoQuoteFromSymbol(t *testing.T) {
	cases := map[string]string{
		"BTCUSDT": "usdt",
		"ETHUSDC": "usdc",
		"SOLUSD":  "usd",
		"BTCEUR":  "eur",
		"ETHBTC":  "btc",
		"BTC":     "", // suffix-only would consume the whole string → reject
		"":        "",
		"DOGE":    "",
	}
	for input, want := range cases {
		if got := inferCryptoQuoteFromSymbol(input); got != want {
			t.Errorf("inferCryptoQuoteFromSymbol(%q) = %q, want %q", input, got, want)
		}
	}
}
