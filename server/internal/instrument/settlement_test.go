package instrument

import "testing"

func TestSettlementCycleForAShareBoardsAreT1(t *testing.T) {
	cases := []struct {
		name   string
		symbol string
	}{
		{"sh-main-600", "600519"},
		{"sh-main-601", "601318"},
		{"sh-main-603", "603501"},
		{"sh-main-605", "605499"},
		{"sz-main-000", "000858"},
		{"sz-main-001", "001288"},
		{"sz-sme-002", "002594"},
		{"sz-sme-003", "003816"},
		{"chinext-300", "300750"},
		{"chinext-301", "301236"},
		{"star-688", "688205"},
		{"star-689", "689009"},
		{"bse-43", "430510"},
		{"bse-83", "832735"},
		{"bse-87", "870866"},
		{"bse-88", "880001"},
		{"bse-92", "920002"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SettlementCycleFor(tc.symbol, Hint{})
			if got != SettlementT1 {
				t.Errorf("SettlementCycleFor(%q) = %q, want %q", tc.symbol, got, SettlementT1)
			}
			if !got.IsLocked() {
				t.Errorf("IsLocked() should be true for %q", tc.symbol)
			}
		})
	}
}

func TestSettlementCycleForNonAShareAreT0(t *testing.T) {
	cases := []struct {
		name   string
		symbol string
		hint   Hint
	}{
		{"us-equity", "AAPL", Hint{Market: "us_stock"}},
		{"hk-equity", "0700", Hint{Market: "hk_stock"}},
		{"jp-equity", "7203", Hint{Market: "jp_stock"}},
		{"crypto-btc", "BTCUSDT", Hint{Market: "crypto", AssetClass: "crypto"}},
		{"futures", "IF2406", Hint{Market: "cn_futures", AssetClass: "futures"}},
		{"unknown", "ZZZZ", Hint{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SettlementCycleFor(tc.symbol, tc.hint)
			if got != SettlementT0 {
				t.Errorf("SettlementCycleFor(%q, %+v) = %q, want %q", tc.symbol, tc.hint, got, SettlementT0)
			}
			if got.IsLocked() {
				t.Errorf("IsLocked() should be false for %q", tc.symbol)
			}
		})
	}
}

func TestSettlementCycleForRespectsMarketHintForAShareAliases(t *testing.T) {
	// Symbols that *look* like A-share by prefix get classified
	// correctly via Classify; this test pins down the fallback path
	// where Classify says BoardUnknown but the hint explicitly tags
	// the symbol as A-share. Useful for unusual symbol formats
	// (custom indices, custom ETF tickers) that callers want to flag
	// as A-share even when the prefix rule doesn't fire.
	for _, market := range []string{"a_share", "A_Share", "ashare", "cn_equity"} {
		got := SettlementCycleFor("WEIRD", Hint{Market: market})
		if got != SettlementT1 {
			t.Errorf("hint market %q should force T+1, got %q", market, got)
		}
	}
}

func TestSellableQtyToday_T1Markets(t *testing.T) {
	cases := []struct {
		name        string
		symbol      string
		hint        Hint
		totalQty    float64
		boughtToday float64
		want        float64
	}{
		// 1000 held, 0 bought today → all sellable
		{"a-share-no-intraday-buy", "600519", Hint{Market: "a_share"}, 1000, 0, 1000},
		// 1000 held, 400 bought today → 600 sellable, 400 locked
		{"a-share-partial-lock", "600519", Hint{Market: "a_share"}, 1000, 400, 600},
		// 100 held, 100 bought today → 0 sellable
		{"a-share-full-lock", "300750", Hint{Market: "a_share"}, 100, 100, 0},
		// boughtToday > totalQty (shouldn't happen but defend) → 0
		{"a-share-overlock", "688205", Hint{Market: "a_share"}, 100, 200, 0},
		// Empty position
		{"empty-position", "600519", Hint{Market: "a_share"}, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SellableQtyToday(tc.symbol, tc.hint, tc.totalQty, tc.boughtToday)
			if got != tc.want {
				t.Errorf("SellableQtyToday = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSellableQtyToday_T0MarketsAlwaysFullySellable(t *testing.T) {
	// On T+0 markets the boughtToday signal is irrelevant — the
	// whole position is sellable regardless of intraday buy activity.
	cases := []struct {
		name        string
		symbol      string
		hint        Hint
		totalQty    float64
		boughtToday float64
	}{
		{"us-equity-with-intraday-buy", "AAPL", Hint{Market: "us_stock"}, 100, 50},
		{"crypto-with-intraday-buy", "BTCUSDT", Hint{Market: "crypto"}, 10, 10},
		{"hk-equity-with-intraday-buy", "0700", Hint{Market: "hk_stock"}, 200, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SellableQtyToday(tc.symbol, tc.hint, tc.totalQty, tc.boughtToday)
			if got != tc.totalQty {
				t.Errorf("T+0 market: got %v, want totalQty %v", got, tc.totalQty)
			}
		})
	}
}

func TestSettlementCycleZeroValueIsUnknown(t *testing.T) {
	// The zero value of SettlementCycle should not accidentally claim
	// to be locked; otherwise callers that forget to set it would
	// silently start enforcing T+1 on every position.
	var zero SettlementCycle
	if zero != SettlementUnknown {
		t.Errorf("zero value = %q, want %q", zero, SettlementUnknown)
	}
	if zero.IsLocked() {
		t.Errorf("zero value must not be locked")
	}
}
