// Package instrument provides exchange-aware trading constraints such as
// A-share lot-size rules.
//
// A-share boards have non-uniform minimum-buy quantities and increment
// units:
//
//	Board                            MinLot  Step (above MinLot)
//	─────────────────────────────────────────────────────────────
//	SH/SZ Main  (600/601/603/605/000/001/002/003)   100    100
//	ChiNext     (300/301)                           100    100
//	STAR Market (688/689)                           200    1
//	BSE         (43/83/87/88/92)                    100    1
//
// Selling rule (all A-share boards): if the residual position after a
// partial sell would be a non-zero "odd lot" (< MinLot), the entire
// residual must be liquidated in a single order. This package's
// NormalizeSellQty enforces that by snapping such sells up to the full
// holding.
//
// For non-A-share symbols (US equities, HK stock, crypto, futures, etc.)
// Classify returns BoardUnknown and the normalizers pass quantities
// through unchanged. Callers that need other markets' lot rules should
// extend Classify and SpecFor rather than branching at the call site.
package instrument

import (
	"math"
	"strings"
	"unicode"
)

// Board identifies an A-share trading board with distinct lot-size rules.
type Board string

const (
	// BoardUnknown means the symbol is not an A-share, or could not be
	// classified. The normalizers treat this as "no lot constraint".
	BoardUnknown Board = ""
	// BoardSHMain covers Shanghai main-board equities (600/601/603/605).
	BoardSHMain Board = "sh_main"
	// BoardSZMain covers Shenzhen main-board equities (000/001/002/003).
	// SME board (002/003) was merged into main in 2021 and shares its
	// rules.
	BoardSZMain Board = "sz_main"
	// BoardChiNext covers ChiNext equities (300/301).
	BoardChiNext Board = "chinext"
	// BoardSTAR covers STAR Market equities (688/689).
	BoardSTAR Board = "star"
	// BoardBSE covers Beijing Stock Exchange equities (43/83/87/88/92).
	BoardBSE Board = "bse"
)

// Hint carries optional market metadata that helps disambiguate symbols
// when the prefix alone is insufficient (e.g. ADRs, GDRs, or 6-digit
// symbols on non-A-share venues). All fields are optional.
type Hint struct {
	Market     string // e.g. "a_share", "us_stock", "hk_stock", "crypto"
	Exchange   string // e.g. "SSE", "SZSE", "BSE", "NASDAQ"
	AssetClass string // e.g. "equity", "crypto", "futures"
}

// Spec is the lot-size constraint for one board.
type Spec struct {
	Board  Board
	MinLot int // minimum quantity for a buy or initial reduce; 0 means unconstrained
	Step   int // increment unit above MinLot; 1 means single-share increments
}

// IsAShare reports whether the spec describes an A-share board.
func (s Spec) IsAShare() bool { return s.MinLot > 0 }

// Classify maps a symbol (plus optional hint) to a Board. The hint is
// consulted first; when it unambiguously selects a non-A-share market the
// function returns BoardUnknown to short-circuit the prefix logic. For
// purely numeric A-share tickers the symbol prefix is authoritative.
func Classify(symbol string, hint Hint) Board {
	sym := normalizeSymbol(symbol)

	if !looksAShareByHint(hint) {
		// Hint says it's not A-share at all (e.g. NASDAQ AAPL with a
		// 4-letter symbol). Skip prefix matching.
		if hint.Market != "" || hint.Exchange != "" || hint.AssetClass != "" {
			if !hintAllowsAShare(hint) {
				return BoardUnknown
			}
		}
	}

	return classifyPrefix(sym)
}

// DefaultSlippageTolerance returns the default execution-time slippage
// tolerance for an A-share board, expressed as a positive fraction
// (e.g. 0.008 = 0.8%). BoardUnknown returns 0, meaning "no per-board
// default" — callers should fall back to their own market-level or
// global configuration.
//
// Values are tuned against each board's typical intraday volatility:
//
//	SH/SZ main 0.8%   (10% daily limit, low realised vol)
//	ChiNext    1.2%   (20% daily limit, slightly higher vol)
//	STAR/BSE   1.5%   (20% daily limit, thinner liquidity)
//
// These defaults are advisory; production configs can override per
// fund via risk.SlippageConfig.
func DefaultSlippageTolerance(b Board) float64 {
	switch b {
	case BoardSHMain, BoardSZMain:
		return 0.008
	case BoardChiNext:
		return 0.012
	case BoardSTAR, BoardBSE:
		return 0.015
	default:
		return 0
	}
}

// SpecFor returns the lot-size specification for a given board. For
// BoardUnknown it returns a zero Spec, which the normalizers treat as
// "no constraint".
func SpecFor(b Board) Spec {
	switch b {
	case BoardSHMain, BoardSZMain, BoardChiNext:
		return Spec{Board: b, MinLot: 100, Step: 100}
	case BoardSTAR:
		return Spec{Board: b, MinLot: 200, Step: 1}
	case BoardBSE:
		return Spec{Board: b, MinLot: 100, Step: 1}
	default:
		return Spec{Board: BoardUnknown}
	}
}

// NormalizeBuyQty returns the largest legal buy quantity not exceeding
// rawQty for the given symbol. For non-A-share symbols rawQty is returned
// unchanged. Returns 0 when rawQty is below MinLot (caller should treat
// that as "buy budget too small for this board").
//
// Examples:
//   - 393 on 600519 (SH main)  → 300
//   - 393 on 300750 (ChiNext)  → 300
//   - 393 on 688205 (STAR)     → 393  (200 + 1-share increments)
//   - 150 on 688205 (STAR)     → 0    (below 200-share threshold)
//   - 393 on 830799 (BSE)      → 393  (100 + 1-share increments)
func NormalizeBuyQty(symbol string, hint Hint, rawQty float64) float64 {
	spec := SpecFor(Classify(symbol, hint))
	if !spec.IsAShare() {
		return rawQty
	}
	if rawQty <= 0 {
		return 0
	}
	q := int(rawQty)
	if q < spec.MinLot {
		return 0
	}
	if spec.Step <= 1 {
		return float64(q)
	}
	return float64((q / spec.Step) * spec.Step)
}

// NormalizeSellQty returns a legal sell/reduce quantity. The semantics
// differ from buys because the residual must not be an "odd lot":
//
//   - Selling the entire holding is always legal.
//   - A partial sell is floored to a Step multiple.
//   - If the residual after the floored sell would be > 0 but < MinLot,
//     the sell is grown to liquidate the whole position (A-share rule:
//     "余额不足 [MinLot] 股应一次性申报卖出").
//   - If the floored sell ends up smaller than MinLot but the holding has
//     full lots available, the sell is rounded up to MinLot.
//
// For non-A-share symbols rawQty is returned unchanged (subject to the
// holding cap).
func NormalizeSellQty(symbol string, hint Hint, rawQty, holdingQty float64) float64 {
	if rawQty <= 0 || holdingQty <= 0 {
		return 0
	}
	if rawQty >= holdingQty {
		return holdingQty
	}

	spec := SpecFor(Classify(symbol, hint))
	if !spec.IsAShare() {
		return rawQty
	}

	hold := int(holdingQty)
	sell := int(rawQty)

	if spec.Step > 1 {
		sell = (sell / spec.Step) * spec.Step
	}

	// If the floored sell came in below MinLot but the holding has full
	// lots available, snap up to MinLot first. This avoids producing a
	// no-op trade just because the requested fraction was small.
	if sell < spec.MinLot {
		sell = spec.MinLot
		if sell > hold {
			return float64(hold)
		}
	}

	// Final residual check: if what's left after the sell would be a
	// non-zero odd lot (< MinLot), the residual cannot be left behind —
	// A-share rules require odd-lot residuals to be liquidated in a
	// single order. Expand the sell to cover the full holding.
	residual := hold - sell
	if residual > 0 && residual < spec.MinLot {
		return float64(hold)
	}

	return float64(sell)
}

// TickSizeFor returns the minimum legal price increment ("tick")
// for symbol+hint at the given price, in the symbol's quote
// currency. Returns 0 when the package has no deterministic
// per-symbol-prefix rule and the caller should defer to a
// metadata-backed resolver (HK banded ticks, crypto step_size).
//
// Deterministic rules covered here:
//
//   A-share (all boards)     0.01 CNY              (沪深主板 / 创业板 / 科创板 / 北交所 全部 0.01)
//   US equity, price ≥ $1.00 0.01 USD              (Reg NMS Rule 612 ≥$1 tier)
//   US equity, price <  $1.00 0.0001 USD           (sub-dollar tier)
//
// Hints with explicit non-equity asset classes (crypto / futures)
// always return 0 — the lot-size engine's step_size / contract
// multiplier already constrains those venues, so an additional
// tick check would double-count.
//
// price is consulted only for the US sub-dollar rule; pass 0 to
// get the ≥$1 tick (the common case for plan-time pre-checks
// where the live price isn't loaded yet).
func TickSizeFor(symbol string, hint Hint, price float64) float64 {
	ac := strings.ToLower(strings.TrimSpace(hint.AssetClass))
	switch ac {
	case "crypto", "futures", "future", "option", "options":
		return 0
	}
	if SpecFor(Classify(symbol, hint)).IsAShare() {
		return 0.01
	}
	market := strings.ToLower(strings.TrimSpace(hint.Market))
	switch market {
	case "us_stock", "us_equity", "us", "usequity":
		if price > 0 && price < 1.0 {
			return 0.0001
		}
		return 0.01
	}
	return 0
}

// IsTickAligned reports whether price aligns to the per-venue
// tick size for symbol+hint. Returns true when no deterministic
// tick is known (the caller should fall back to a metadata-backed
// gate for those venues — for HK banded ticks and crypto step we
// rely on broker-side LotSizeGate which queries
// instrument_metadata).
//
// price ≤ 0 is treated as aligned (market orders carry no price,
// the broker price will be re-checked downstream).
func IsTickAligned(symbol string, hint Hint, price float64) bool {
	if price <= 0 {
		return true
	}
	tick := TickSizeFor(symbol, hint, price)
	if tick <= 0 {
		return true
	}
	// Scale to integer space to avoid float fuzz: e.g. price=0.30
	// / tick=0.01 = 30.0 in scaled space; round and require the
	// round-trip to recover within 1e-6 of the input.
	scale := 1.0 / tick
	scaled := price * scale
	rounded := math.Round(scaled)
	return math.Abs(scaled-rounded) < 1e-6
}

// FloorToTick returns the largest legal price ≤ price for the
// given symbol+hint. Useful when a fat-finger limit needs to be
// nudged down to the nearest tick before re-submission. Returns
// price unchanged when no deterministic tick applies.
func FloorToTick(symbol string, hint Hint, price float64) float64 {
	if price <= 0 {
		return price
	}
	tick := TickSizeFor(symbol, hint, price)
	if tick <= 0 {
		return price
	}
	scale := 1.0 / tick
	// Add a tiny epsilon so prices that are exact tick multiples
	// (e.g. 239.35 / 0.01 = 23935) don't get floored to 23934
	// because of double-precision drift on the multiply.
	return math.Floor(price*scale+1e-9) / scale
}

// IsAligned reports whether qty satisfies the lot-size constraint for
// symbol+hint. Non-A-share symbols are always considered aligned.
// qty == 0 is considered aligned (no trade). Used by risk policy.
func IsAligned(symbol string, hint Hint, qty float64) bool {
	if qty <= 0 {
		return true
	}
	spec := SpecFor(Classify(symbol, hint))
	if !spec.IsAShare() {
		return true
	}
	q := int(qty)
	if q < spec.MinLot {
		return false
	}
	if spec.Step <= 1 {
		return true
	}
	return q%spec.Step == 0
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

func normalizeSymbol(s string) string {
	s = strings.TrimSpace(s)
	// Strip common venue prefixes like "sh.", "sz.", "bj." used by
	// upstream feeds (Tencent, Tushare, etc.) — only the numeric tail
	// drives A-share classification.
	for _, p := range []string{"sh.", "sz.", "bj.", "SH.", "SZ.", "BJ.", "sh", "sz", "bj"} {
		if strings.HasPrefix(s, p) {
			s = strings.TrimPrefix(s, p)
			break
		}
	}
	// Strip suffixes like ".SH", ".SZ", ".BJ" used by Wind/Bloomberg.
	for _, sfx := range []string{".SH", ".SZ", ".BJ", ".sh", ".sz", ".bj"} {
		if strings.HasSuffix(s, sfx) {
			s = strings.TrimSuffix(s, sfx)
			break
		}
	}
	return strings.TrimSpace(s)
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func classifyPrefix(sym string) Board {
	if !isNumeric(sym) || len(sym) != 6 {
		return BoardUnknown
	}
	switch sym[:3] {
	case "600", "601", "603", "605":
		return BoardSHMain
	case "688", "689":
		return BoardSTAR
	case "000", "001", "002", "003":
		return BoardSZMain
	case "300", "301":
		return BoardChiNext
	}
	// BSE codes: 43xxxx, 83xxxx, 87xxxx, 88xxxx, 92xxxx.
	switch sym[:2] {
	case "43", "83", "87", "88", "92":
		return BoardBSE
	}
	return BoardUnknown
}

// hintAllowsAShare returns true when the hint is either silent or
// explicitly indicates an A-share market. Hints pointing at other markets
// (us_stock, hk_stock, crypto, futures, …) suppress prefix matching so
// that a 6-digit non-A-share identifier — e.g. a future contract code —
// isn't accidentally treated as an SH main-board ticker.
func hintAllowsAShare(h Hint) bool {
	m := strings.ToLower(strings.TrimSpace(h.Market))
	ex := strings.ToUpper(strings.TrimSpace(h.Exchange))
	ac := strings.ToLower(strings.TrimSpace(h.AssetClass))

	switch m {
	case "", "a_share", "a-share", "ashare", "cn", "china", "cn_stock":
		// fall through to other checks
	default:
		return false
	}
	switch ex {
	case "", "SSE", "XSHG", "SHA", "SZSE", "XSHE", "SHE", "BSE", "BJSE", "XBSE":
	default:
		return false
	}
	switch ac {
	case "", "equity", "stock", "shares":
	default:
		return false
	}
	return true
}

func looksAShareByHint(h Hint) bool {
	m := strings.ToLower(strings.TrimSpace(h.Market))
	return m == "a_share" || m == "a-share" || m == "ashare" || m == "cn_stock"
}
