package instrument

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		symbol string
		hint   Hint
		want   Board
	}{
		{"sh-main-600", "600519", Hint{}, BoardSHMain},
		{"sh-main-601", "601318", Hint{}, BoardSHMain},
		{"sh-main-603", "603259", Hint{}, BoardSHMain},
		{"sh-main-605", "605499", Hint{}, BoardSHMain},
		{"star-688", "688205", Hint{}, BoardSTAR},
		{"star-689", "689009", Hint{}, BoardSTAR},
		{"sz-main-000", "000858", Hint{}, BoardSZMain},
		{"sz-main-001", "001979", Hint{}, BoardSZMain},
		{"sz-main-002", "002594", Hint{}, BoardSZMain},
		{"sz-main-003", "003816", Hint{}, BoardSZMain},
		{"chinext-300", "300750", Hint{}, BoardChiNext},
		{"chinext-301", "301236", Hint{}, BoardChiNext},
		{"bse-43", "430047", Hint{}, BoardBSE},
		{"bse-83", "830799", Hint{}, BoardBSE},
		{"bse-87", "870866", Hint{}, BoardBSE},
		{"bse-88", "889999", Hint{}, BoardBSE},
		{"bse-92", "920002", Hint{}, BoardBSE},

		{"prefix-sh-dot", "sh.600519", Hint{}, BoardSHMain},
		{"prefix-sh-tight", "sh688205", Hint{}, BoardSTAR},
		{"suffix-sh", "600519.SH", Hint{}, BoardSHMain},
		{"suffix-sz", "300750.SZ", Hint{}, BoardChiNext},
		{"suffix-bj", "830799.BJ", Hint{}, BoardBSE},

		{"hint-a-share-explicit", "688205", Hint{Market: "a_share", Exchange: "SSE", AssetClass: "equity"}, BoardSTAR},
		{"hint-us-rejects-numeric", "688205", Hint{Market: "us_stock"}, BoardUnknown},
		{"hint-nasdaq-non-numeric", "AAPL", Hint{Market: "us_stock", Exchange: "NASDAQ"}, BoardUnknown},
		{"hint-crypto-rejects-numeric", "688205", Hint{AssetClass: "crypto"}, BoardUnknown},
		{"hint-futures-rejects", "688205", Hint{AssetClass: "futures"}, BoardUnknown},

		{"non-numeric", "ABC123", Hint{}, BoardUnknown},
		{"too-short", "60051", Hint{}, BoardUnknown},
		{"too-long", "6005199", Hint{}, BoardUnknown},
		{"unknown-prefix", "500001", Hint{}, BoardUnknown},
		{"empty", "", Hint{}, BoardUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.symbol, tc.hint)
			if got != tc.want {
				t.Errorf("Classify(%q, %+v) = %q, want %q", tc.symbol, tc.hint, got, tc.want)
			}
		})
	}
}

func TestNormalizeBuyQty(t *testing.T) {
	cases := []struct {
		name   string
		symbol string
		raw    float64
		want   float64
	}{
		// SH/SZ main: floor to 100.
		{"sh-393-floor-300", "600519", 393, 300},
		{"sh-100-ok", "600519", 100, 100},
		{"sh-99-below-minlot", "600519", 99, 0},
		{"sh-700-ok", "600519", 700, 700},
		{"sz-393-floor-300", "000858", 393, 300},

		// ChiNext: same as main board.
		{"chinext-393-floor-300", "300750", 393, 300},
		{"chinext-1234-floor-1200", "300750", 1234, 1200},
		{"chinext-50-below-minlot", "300750", 50, 0},

		// STAR: 200 minimum, 1-share increments thereafter.
		{"star-393-keep", "688205", 393, 393},
		{"star-200-ok", "688205", 200, 200},
		{"star-199-below-minlot", "688205", 199, 0},
		{"star-1-below-minlot", "688205", 1, 0},
		{"star-201-keep", "688205", 201, 201},

		// BSE: 100 minimum, 1-share increments.
		{"bse-393-keep", "830799", 393, 393},
		{"bse-100-ok", "830799", 100, 100},
		{"bse-99-below-minlot", "830799", 99, 0},

		// Non-A-share: pass through.
		{"us-equity-passthrough", "AAPL", 7, 7},
		{"us-equity-zero", "AAPL", 0, 0},
		{"crypto-fractional", "BTCUSDT", 0.5, 0.5},

		// Edge: zero and negative.
		{"sh-zero", "600519", 0, 0},
		{"sh-negative", "600519", -10, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeBuyQty(tc.symbol, Hint{}, tc.raw)
			if got != tc.want {
				t.Errorf("NormalizeBuyQty(%q, %v) = %v, want %v", tc.symbol, tc.raw, got, tc.want)
			}
		})
	}
}

func TestNormalizeSellQty(t *testing.T) {
	cases := []struct {
		name        string
		symbol      string
		raw         float64
		holdingQty  float64
		want        float64
		description string
	}{
		// Sell-all is always legal.
		{"sh-sell-all-exact", "600519", 500, 500, 500, "sell entire position"},
		{"sh-sell-more-than-held", "600519", 700, 500, 500, "cap at holding"},
		{"star-sell-all-odd", "688205", 393, 393, 393, "sell-all of odd-lot is legal"},

		// Main board partial sell aligned.
		{"sh-partial-aligned", "600519", 300, 1000, 300, "300/700 split, both legal"},
		{"sh-partial-floor", "600519", 393, 1000, 300, "floor 393 → 300"},

		// Main board: odd-lot residual triggers full liquidation.
		{"sh-residual-odd-forces-sellall", "600519", 100, 150, 150, "remainder 50 < 100 → sell all 150"},
		{"sh-residual-zero-keep", "600519", 100, 100, 100, "remainder 0 is fine"},

		// Main board: raw below MinLot but holding has full lots → round up.
		{"sh-raw-below-lot-roundup", "600519", 50, 1000, 100, "50 < 100, round up to 100"},
		{"sh-raw-below-lot-cap-holding", "600519", 50, 80, 80, "raw < lot, holding < lot → sell all 80"},

		// STAR: 200 min, 1-share increment.
		{"star-partial-aligned-201", "688205", 201, 1000, 201, "201 is legal on STAR"},
		{"star-raw-100-below-min", "688205", 100, 1000, 200, "raw < 200, round up to 200"},
		{"star-residual-odd-forces-sellall", "688205", 600, 700, 700, "remainder 100 < 200 → sell all"},
		{"star-residual-200-ok", "688205", 500, 700, 500, "remainder 200 == MinLot, fine"},

		// BSE: 100 min, 1-share increment.
		{"bse-partial-201-ok", "830799", 201, 1000, 201, "201 legal on BSE"},
		{"bse-residual-odd-forces-sellall", "830799", 350, 400, 400, "remainder 50 < 100 → sell all"},

		// Non-A-share pass-through.
		{"us-equity-partial", "AAPL", 3, 10, 3, "no constraint"},

		// Degenerate.
		{"zero-raw", "600519", 0, 1000, 0, "no sell"},
		{"zero-holding", "600519", 100, 0, 0, "nothing to sell"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeSellQty(tc.symbol, Hint{}, tc.raw, tc.holdingQty)
			if got != tc.want {
				t.Errorf("NormalizeSellQty(%q, raw=%v, hold=%v) = %v, want %v (%s)",
					tc.symbol, tc.raw, tc.holdingQty, got, tc.want, tc.description)
			}
		})
	}
}

func TestIsAligned(t *testing.T) {
	cases := []struct {
		name   string
		symbol string
		qty    float64
		want   bool
	}{
		{"sh-300-aligned", "600519", 300, true},
		{"sh-393-not-aligned", "600519", 393, false},
		{"sh-50-below-minlot", "600519", 50, false},
		{"sh-100-aligned", "600519", 100, true},
		{"star-200-aligned", "688205", 200, true},
		{"star-393-aligned", "688205", 393, true},
		{"star-150-below-minlot", "688205", 150, false},
		{"chinext-300-aligned", "300750", 300, true},
		{"chinext-393-not-aligned", "300750", 393, false},
		{"bse-101-aligned", "830799", 101, true},
		{"bse-50-below-minlot", "830799", 50, false},
		{"us-equity-any", "AAPL", 7, true},
		{"zero-qty", "600519", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsAligned(tc.symbol, Hint{}, tc.qty)
			if got != tc.want {
				t.Errorf("IsAligned(%q, %v) = %v, want %v", tc.symbol, tc.qty, got, tc.want)
			}
		})
	}
}
