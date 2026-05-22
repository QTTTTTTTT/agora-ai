package marketdata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDefaultQuoteProviderOrderRoutesFuturesToAkshareFirst guards the F1.3
// wiring: SHFE / DCE / CZCE / INE futures must hit the akshare MCP container
// before falling back to yahoo (which only covers global futures via "=F"
// suffix symbols).
func TestDefaultQuoteProviderOrderRoutesFuturesToAkshareFirst(t *testing.T) {
	svc := NewService(Config{
		AkshareURL: "http://akshare-mcp.local",
	})
	order := svc.defaultQuoteProviderOrder(InstrumentRef{
		Symbol:     "cu2503",
		Market:     "futures",
		Exchange:   "SHFE",
		AssetClass: "futures",
	})
	if len(order) == 0 || order[0] != "akshare" {
		t.Fatalf("expected akshare first in futures chain, got %v", order)
	}
	if !containsString(order, "yahoo") {
		t.Fatalf("expected yahoo in futures chain as fallback, got %v", order)
	}
}

// TestDefaultQuoteProviderOrderRoutesCryptoToCoingeckoFirst verifies F1.4's
// default chain — crypto instruments must hit CoinGecko before yahoo.
func TestDefaultQuoteProviderOrderRoutesCryptoToCoingeckoFirst(t *testing.T) {
	svc := NewService(Config{})
	order := svc.defaultQuoteProviderOrder(InstrumentRef{
		Symbol:     "BTCUSDT",
		Market:     "crypto",
		AssetClass: "crypto",
	})
	if len(order) == 0 || order[0] != "coingecko" {
		t.Fatalf("expected coingecko first in crypto chain, got %v", order)
	}
	if !containsString(order, "yahoo") {
		t.Fatalf("expected yahoo as fallback in crypto chain, got %v", order)
	}
}

// TestProviderNamesForMarketAppliesPerMarketOverride ensures the F1.5
// per-market chain config takes precedence over the global QuoteProviders
// list for the targeted market — and that other markets continue to honor
// the global+default combined order.
func TestProviderNamesForMarketAppliesPerMarketOverride(t *testing.T) {
	svc := NewService(Config{
		AkshareURL:     "http://akshare-mcp.local",
		QuoteProviders: []string{"quantdinger"}, // global hint
		QuoteProvidersByMarket: map[string][]string{
			"crypto": {"yahoo"}, // explicit override
		},
	})

	cryptoOrder := svc.providerNamesForMarket(InstrumentRef{
		Symbol:     "BTCUSDT",
		Market:     "crypto",
		AssetClass: "crypto",
	})
	if len(cryptoOrder) == 0 || cryptoOrder[0] != "yahoo" {
		t.Fatalf("expected yahoo first for crypto override, got %v", cryptoOrder)
	}
	// Global hint still appended (so an ops "always include this" intent
	// is preserved).
	if !containsString(cryptoOrder, "quantdinger") {
		t.Fatalf("expected global quantdinger to be appended after override, got %v", cryptoOrder)
	}
	// Default chain (coingecko) is still appended at the tail.
	if !containsString(cryptoOrder, "coingecko") {
		t.Fatalf("expected coingecko to be appended from default chain, got %v", cryptoOrder)
	}

	// Markets without an override use the global+default merge.
	futuresOrder := svc.providerNamesForMarket(InstrumentRef{
		Symbol:     "cu2503",
		Market:     "futures",
		AssetClass: "futures",
	})
	if len(futuresOrder) == 0 || futuresOrder[0] != "quantdinger" {
		t.Fatalf("expected global hint (quantdinger) first for futures, got %v", futuresOrder)
	}
	if !containsString(futuresOrder, "akshare") {
		t.Fatalf("expected akshare to be appended from default futures chain, got %v", futuresOrder)
	}
}

// TestQuoteProvidersForCryptoHitsCoinGeckoFirst is an end-to-end smoke test
// that exercises GetQuote against a fake CoinGecko endpoint: it confirms the
// Service.providerNamesForMarket → quoteProviderByName → coingecko provider
// wiring is reachable for crypto instruments without test overrides.
func TestQuoteProvidersForCryptoHitsCoinGeckoFirst(t *testing.T) {
	const sample = `{"bitcoin":{"usd":50000,"usd_24h_vol":1000000,"last_updated_at":1715000000}}`
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if !strings.Contains(r.URL.Path, "/simple/price") {
			t.Errorf("expected /simple/price, got %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(sample))
	}))
	defer srv.Close()

	svc := NewService(Config{
		CoinGeckoBaseURL: srv.URL,
	})
	quote, err := svc.GetQuote(context.Background(), InstrumentRef{
		Symbol:     "BTCUSDT",
		Market:     "crypto",
		AssetClass: "crypto",
	})
	if err != nil {
		t.Fatalf("GetQuote: %v", err)
	}
	if quote == nil || quote.Source != "coingecko" {
		t.Fatalf("expected coingecko source, got %#v", quote)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call to coingecko, got %d", calls)
	}
}

// TestNormalizeProvidersByMarketDropsEmptyEntries hardens the Config
// normaliser — empty market keys, blank provider names, and whitespace-only
// values must be dropped silently so loose env config doesn't leak into the
// runtime chain.
func TestNormalizeProvidersByMarketDropsEmptyEntries(t *testing.T) {
	in := map[string][]string{
		"":        {"yahoo"},
		"  ":      {"yahoo"},
		"CRYPTO":  {"  ", ""},
		"futures": {"akshare", " yahoo ", "akshare"}, // dedup + trim
	}
	out := normalizeProvidersByMarket(in)
	if len(out) != 1 {
		t.Fatalf("expected 1 normalised market, got %d (%v)", len(out), out)
	}
	got, ok := out["futures"]
	if !ok {
		t.Fatalf("expected futures key, got %v", out)
	}
	if len(got) != 2 || got[0] != "akshare" || got[1] != "yahoo" {
		t.Fatalf("expected deduped trimmed [akshare yahoo], got %v", got)
	}
}

// TestNormalizeProvidersByMarketReturnsNilForEmptyInput keeps the
// zero-allocation contract that lets callers skip the lookup entirely.
func TestNormalizeProvidersByMarketReturnsNilForEmptyInput(t *testing.T) {
	if out := normalizeProvidersByMarket(nil); out != nil {
		t.Fatalf("expected nil for nil input, got %v", out)
	}
	if out := normalizeProvidersByMarket(map[string][]string{}); out != nil {
		t.Fatalf("expected nil for empty map, got %v", out)
	}
	if out := normalizeProvidersByMarket(map[string][]string{"": {""}}); out != nil {
		t.Fatalf("expected nil when no usable entries survive normalisation, got %v", out)
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
