// Package benchmark defines the canonical set of market indices /
// reference assets the platform benchmarks fund NAVs against, plus
// the rules for picking sensible defaults given a fund's universe.
//
// The package is deliberately data-only — Series/Catalog/Recommend are
// pure data + lookup helpers with no side effects. Actual price
// fetching is done by the api-layer adapter on top of ohlc.Registry,
// which keeps benchmark ↔ provider concerns decoupled (a new
// provider can be added without touching this catalog, and a new
// benchmark can be added without retesting providers).
//
// # Why not just store benchmarks in a DB table
//
// We considered a `benchmarks` table for late-binding, but:
//
//  1. The set is small (≈10 entries) and changes at the cadence of
//     "we want to support a new market" — a code-level change anyway.
//  2. Hardcoding lets the type system verify Series IDs at compile
//     time when wired into recommendations.
//  3. Operators don't author benchmarks; they pick from a curated
//     list. A DB table would invite "let me add my custom one"
//     requests and turn this into a CRUD surface, which is not the
//     business value here.
//
// If a customer ever needs custom benchmarks, the right move is a
// dedicated tenant-specific overlay; the curated catalog stays read-
// only.
package benchmark

import (
	"sort"
	"strings"
)

// Series describes a single benchmark identifier the UI can plot
// alongside a fund NAV. The (Symbol, Market) pair is what gets
// passed to ohlc.Registry to fetch bars.
//
// Currency is informative only — we render in normalized index
// units (start = 100), so the absolute price scale doesn't matter.
// We keep the field around because future "absolute return vs
// benchmark" panels need it.
type Series struct {
	ID       string
	Label    string
	Symbol   string
	Market   string
	Currency string
	// Tags categorize the series for the recommender. Common tags:
	//   - "broad_us"   (SPX, NDX, RUT)
	//   - "sector_us"  (SOXX, XLK, XLE...)
	//   - "broad_cn"   (CSI300, CSI500, CSI1000)
	//   - "broad_hk"   (HSI, HSCEI)
	//   - "crypto"     (BTC-USD, ETH-USD)
	Tags []string
}

// Catalog is the immutable list of supported benchmarks. Order is
// stable (UI lists this verbatim under "available benchmarks").
//
// IMPORTANT: when extending this list, update the Recommend rules
// below if the new series should appear by default for any market.
var Catalog = []Series{
	// US equity broad
	{
		ID: "spx", Label: "S&P 500", Symbol: "^GSPC",
		Market: "us_equity", Currency: "USD",
		Tags: []string{"broad_us"},
	},
	{
		ID: "ndx", Label: "Nasdaq 100", Symbol: "^NDX",
		Market: "us_equity", Currency: "USD",
		Tags: []string{"broad_us"},
	},
	{
		ID: "rut", Label: "Russell 2000", Symbol: "^RUT",
		Market: "us_equity", Currency: "USD",
		Tags: []string{"broad_us"},
	},

	// US sector / thematic
	{
		ID: "soxx", Label: "费城半导体 (SOXX)", Symbol: "SOXX",
		Market: "us_equity", Currency: "USD",
		Tags: []string{"sector_us", "semis"},
	},
	{
		ID: "xlk", Label: "美国科技 ETF (XLK)", Symbol: "XLK",
		Market: "us_equity", Currency: "USD",
		Tags: []string{"sector_us", "tech"},
	},

	// China A-share (Yahoo handles via .SS / .SZ suffix; the
	// us_equity provider chain happens to cover those too because
	// Yahoo's chart endpoint is symbol-suffix-aware).
	{
		ID: "csi300", Label: "沪深300", Symbol: "000300.SS",
		Market: "a_share", Currency: "CNY",
		Tags: []string{"broad_cn"},
	},
	{
		ID: "csi500", Label: "中证500", Symbol: "000905.SS",
		Market: "a_share", Currency: "CNY",
		Tags: []string{"broad_cn"},
	},
	{
		ID: "chinext", Label: "创业板指", Symbol: "399006.SZ",
		Market: "a_share", Currency: "CNY",
		Tags: []string{"broad_cn", "growth"},
	},
	{
		ID: "star50", Label: "科创50", Symbol: "000688.SS",
		Market: "a_share", Currency: "CNY",
		Tags: []string{"broad_cn", "tech"},
	},

	// Hong Kong
	{
		ID: "hsi", Label: "恒生指数", Symbol: "^HSI",
		Market: "hk_equity", Currency: "HKD",
		Tags: []string{"broad_hk"},
	},
	{
		ID: "hscei", Label: "恒生中国企业指数", Symbol: "^HSCE",
		Market: "hk_equity", Currency: "HKD",
		Tags: []string{"broad_hk"},
	},

	// Crypto
	{
		ID: "btc_usdt", Label: "BTC / USDT", Symbol: "BTCUSDT",
		Market: "crypto", Currency: "USD",
		Tags: []string{"crypto"},
	},
	{
		ID: "eth_usdt", Label: "ETH / USDT", Symbol: "ETHUSDT",
		Market: "crypto", Currency: "USD",
		Tags: []string{"crypto"},
	},
}

// ByID returns the Series with the given canonical ID and a found
// flag. ID lookup is case-insensitive on input but the canonical
// IDs are always lowercase.
func ByID(id string) (Series, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, s := range Catalog {
		if s.ID == id {
			return s, true
		}
	}
	return Series{}, false
}

// AllIDs lists every catalog ID in declaration order. Used by the
// admin / debug routes to surface what the platform supports.
func AllIDs() []string {
	out := make([]string, 0, len(Catalog))
	for _, s := range Catalog {
		out = append(out, s.ID)
	}
	return out
}

// FundProfile is the minimum subset of a fund the recommender
// needs. Kept narrow on purpose: the api package can construct
// one without dragging in the heavy fund DTO.
type FundProfile struct {
	// Market is the fund's primary market tag, mirroring
	// ohlc.Provider.Supports values: "us_equity", "a_share",
	// "hk_equity", "crypto", "futures", "mixed".
	Market string
	// Symbols the fund actually holds; lets the recommender
	// upweight matching sectors when the universe is narrow
	// (e.g. NVDA-heavy → suggest SOXX).
	Symbols []string
}

// Recommend picks a small ordered list of benchmark IDs that the
// UI should default-select for a fund profile. The first element is
// the primary benchmark; subsequent are alternates the user can
// toggle.
//
// Rules (deterministic — no randomness — so two consecutive calls
// with the same input return identical lists):
//
//   - us_equity   → spx (primary), then ndx if any holding's symbol
//                   resembles a tech / mega-cap; soxx if any holding
//                   is in a known semi list.
//   - a_share     → csi300 primary, csi500 alt; star50 if the fund
//                   holds STAR Market tickers (688xxx).
//   - hk_equity   → hsi primary, hscei alt.
//   - crypto      → btc_usdt primary, eth_usdt alt.
//   - futures     → falls through to btc_usdt because BTC is the
//                   "macro futures benchmark" we use; the right call
//                   long-term is exposing futures-specific indices.
//   - mixed / "" → spx + csi300 (a sensible cross-market pair).
//
// The recommender NEVER returns more than 4 IDs; UI clutter beats
// information density past that.
func Recommend(p FundProfile) []string {
	market := strings.ToLower(strings.TrimSpace(p.Market))
	symbols := normalizeSymbols(p.Symbols)
	switch market {
	case "us_equity":
		out := []string{"spx"}
		if hasAnyTechHeavy(symbols) {
			out = append(out, "ndx")
		}
		if hasAnySemi(symbols) {
			out = append(out, "soxx")
		}
		return capList(unique(out), 4)
	case "a_share":
		out := []string{"csi300"}
		if hasAnyStarMarket(symbols) {
			out = append(out, "star50")
		}
		out = append(out, "csi500")
		return capList(unique(out), 4)
	case "hk_equity":
		return []string{"hsi", "hscei"}
	case "crypto":
		return []string{"btc_usdt", "eth_usdt"}
	case "futures":
		// We don't have a futures-specific index yet. BTC is a
		// reasonable macro stand-in for crypto futures; for
		// commodity / index futures funds the operator should
		// explicitly pick from the catalog.
		return []string{"btc_usdt"}
	case "mixed", "":
		return []string{"spx", "csi300"}
	default:
		// Unknown market → default to broad US so the chart still
		// renders something sensible rather than 503.
		return []string{"spx"}
	}
}

// hasAnyTechHeavy returns true when the symbols contain a
// recognisably mega-cap-tech name. We deliberately keep the list
// short — a richer classifier would belong in the strategy layer.
func hasAnyTechHeavy(symbols []string) bool {
	for _, s := range symbols {
		switch s {
		case "AAPL", "MSFT", "GOOGL", "GOOG", "AMZN", "META", "NVDA", "TSLA", "AVGO", "ORCL":
			return true
		}
	}
	return false
}

// hasAnySemi mirrors hasAnyTechHeavy but for the semi index.
// Symbols that warrant SOXX as a benchmark.
func hasAnySemi(symbols []string) bool {
	for _, s := range symbols {
		switch s {
		case "NVDA", "AVGO", "AMD", "INTC", "TSM", "ASML", "MU", "QCOM", "TXN", "MRVL", "LRCX", "AMAT":
			return true
		}
	}
	return false
}

// hasAnyStarMarket returns true if any A-share symbol starts with
// "688" — STAR Market (科创板) tickers. Detecting these promotes
// star50 into the default set so growth-tilted A-share funds get
// a more representative benchmark than CSI300 alone.
func hasAnyStarMarket(symbols []string) bool {
	for _, s := range symbols {
		if strings.HasPrefix(s, "688") {
			return true
		}
	}
	return false
}

// Helpers — kept private; the package's external surface stays
// confined to Series / Catalog / ByID / AllIDs / Recommend.

func normalizeSymbols(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.ToUpper(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		// Strip any exchange suffix so the recommender's symbol
		// match works for both "NVDA" and "NASDAQ:NVDA".
		if i := strings.LastIndex(s, ":"); i >= 0 {
			s = s[i+1:]
		}
		// Also strip Yahoo-style suffix.
		s = strings.TrimSuffix(s, ".SS")
		s = strings.TrimSuffix(s, ".SZ")
		s = strings.TrimSuffix(s, ".HK")
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func unique(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func capList(in []string, n int) []string {
	if len(in) <= n {
		return in
	}
	return in[:n]
}
