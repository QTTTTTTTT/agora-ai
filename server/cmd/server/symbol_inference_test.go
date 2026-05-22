package main

import "testing"

// TestFallbackWorkflowSymbolSkipsGenericWords confirms F11.2's primary
// fix: words like "SMOKE", "TEST", "ALPHA" that appear in fund names
// no longer leak through as fake tickers. This was the exact issue
// observed during the F8 smoke test where macro_brief was quoting
// symbol="SMOKE" for "Smoke Crypto Fund".
func TestFallbackWorkflowSymbolSkipsGenericWords(t *testing.T) {
	cases := []struct {
		name     string
		fundName string
		want     string
	}{
		{"smoke test fund", "Smoke Test Crypto Fund", ""},
		{"only fund word", "Fund", ""},
		{"alpha quant", "Alpha Quant Strategy", ""},
		{"global macro", "Global Macro Fund", ""},
		{"fundai partners core", "FundAI Partners Core Capital", ""},
		{"real ticker first", "NVDA Long Strategy", "NVDA"},
		{"real ticker after generic", "Crypto Fund BTC Strategy", "BTC"},
		{"empty", "", ""},
		{"valid AAPL", "AAPL Strategy", "AAPL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fallbackWorkflowSymbolFromFundName(tc.fundName)
			if got != tc.want {
				t.Fatalf("fallbackWorkflowSymbolFromFundName(%q)=%q want %q", tc.fundName, got, tc.want)
			}
		})
	}
}

// TestDefaultBenchmarkForMarketProfile locks in F11.2's market-aware
// defaults: when no benchmark is set, the symbol inference picks a
// reasonable per-market default before falling through to fund-name
// parsing.
func TestDefaultBenchmarkForMarketProfile(t *testing.T) {
	cases := []struct {
		name    string
		profile fundMarketProfile
		want    string
	}{
		{"crypto market", fundMarketProfile{Market: "crypto"}, "BTC-USD"},
		{"crypto assetclass", fundMarketProfile{AssetClass: "crypto"}, "BTC-USD"},
		{"us equity", fundMarketProfile{Market: "us_equity"}, "SPY"},
		{"a share", fundMarketProfile{Market: "a_share"}, "000300.SS"},
		{"futures market", fundMarketProfile{Market: "futures"}, "ES=F"},
		{"futures assetclass", fundMarketProfile{AssetClass: "futures"}, "ES=F"},
		{"unknown market", fundMarketProfile{Market: "exotic"}, ""},
		{"empty", fundMarketProfile{}, ""},
		{"case insensitive crypto", fundMarketProfile{Market: "CRYPTO"}, "BTC-USD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := defaultBenchmarkForMarketProfile(tc.profile)
			if got != tc.want {
				t.Fatalf("defaultBenchmarkForMarketProfile(%+v)=%q want %q", tc.profile, got, tc.want)
			}
		})
	}
}
